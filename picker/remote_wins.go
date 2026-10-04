package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// remoteWindow is one window of a remote tmux session. Session is the raw name
// the launcher's argv needs; ID and SessionID are tmux's @N and $N, which the
// kill targets instead of any name.
type remoteWindow struct {
	Session   string `json:"session"`
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	Index     int    `json:"index"`
	Name      string `json:"name"`
}

// remoteListWindowsBody lists sessions and every window in one chained tmux
// command. Each free-text field sits last on its own line kind (S or W), so a
// name may hold `|`; the window line joins its session by session_id. `\;` is
// a literal `;` argument under both fish and bash.
var remoteListWindowsBody = remoteTmuxCmd(`list-sessions -F 'S|#{session_id}|#{session_name}' \; list-windows -a -F 'W|#{session_id}|#{window_id}|#{window_index}|#{window_name}'`)

var remoteListWindowsCmd = remoteIdentityPreamble + `; ` + remoteListWindowsBody

// remoteDisplayNameCells caps a remote session or window name on a row.
const remoteDisplayNameCells = 40

// remoteDisplayName is the one place a remote session or window name is made
// render-safe; the raw name stays for actions.
func remoteDisplayName(s string) string {
	return truncateCells(sanitizeStatusText(s), remoteDisplayNameCells)
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isTmuxID reports whether s is prefix followed by digits ($N session id,
// @N window id).
func isTmuxID(s string, prefix byte) bool {
	return s != "" && s[0] == prefix && isDigits(s[1:])
}

// parseRemoteWindowsOutput splits window-probe stdout into identity, session
// names and windows. Only well-formed S/W lines are kept (a login greeting
// can't pass as one) and a W line whose session id has no S line is dropped.
// Names are kept raw apart from the ssh pipe's trailing \r.
func parseRemoteWindowsOutput(stdout string) remoteProbeResult {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	var res remoteProbeResult
	if len(lines) >= 1 {
		res.Identity.MachineID = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		res.Identity.User = strings.TrimSpace(lines[1])
	}
	if len(lines) <= 2 {
		return res
	}
	names := make(map[string]string)
	for _, line := range lines[2:] {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "S|"):
			f := strings.SplitN(line, "|", 3)
			if len(f) != 3 || !isTmuxID(f[1], '$') || f[2] == "" {
				continue
			}
			names[f[1]] = f[2]
			res.Sessions = append(res.Sessions, f[2])
			if res.SessionIDs == nil {
				res.SessionIDs = make(map[string]string)
			}
			res.SessionIDs[f[2]] = f[1]
		case strings.HasPrefix(line, "W|"):
			f := strings.SplitN(line, "|", 5)
			if len(f) != 5 || !isTmuxID(f[1], '$') || !isTmuxID(f[2], '@') || !isDigits(f[3]) {
				continue
			}
			sess, ok := names[f[1]]
			if !ok {
				continue
			}
			idx, err := strconv.Atoi(f[3])
			if err != nil {
				continue
			}
			res.Windows = append(res.Windows, remoteWindow{Session: sess, SessionID: f[1], ID: f[2], Index: idx, Name: f[4]})
		}
	}
	return res
}

// sshListRemoteWindows is sshListRemoteSessions for the window probe.
func sshListRemoteWindows(host string) (remoteProbeResult, error) {
	return sshProbe(host, remoteListWindowsCmd, parseRemoteWindowsOutput)
}

// remoteWindowCache is one host's last answered window probe, before bridge
// suppression.
type remoteWindowCache struct {
	Host    string         `json:"host"`
	SavedAt int64          `json:"saved_at"` // unix millis, formatSnapshotAge's unit
	Windows []remoteWindow `json:"windows"`
}

// remoteWindowCacheDir is the remote-windows sibling of remoteSessionCacheDir,
// or "" when no absolute base resolves.
func remoteWindowCacheDir() string {
	dir := remoteSessionCacheDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), "remote-windows")
}

func writeRemoteWindowCache(host string, windows []remoteWindow, now time.Time) {
	writeOwnerOnlyJSON(remoteWindowCacheDir(), hostFileName(host)+".json",
		remoteWindowCache{Host: host, SavedAt: now.UnixMilli(), Windows: windows})
}

func readRemoteWindowCache(host string) (remoteWindowCache, bool) {
	var c remoteWindowCache
	if !readOwnerOnlyJSON(remoteWindowCacheDir(), hostFileName(host)+".json", &c) || c.Host != host {
		return remoteWindowCache{}, false
	}
	return c, true
}

// forgetRemoteWindowsWhere drops host's cached windows matching drop,
// preserving SavedAt so the remaining rows keep their (cached …) age. A
// missing or untrusted cache is a no-op.
func forgetRemoteWindowsWhere(host string, drop func(remoteWindow) bool) {
	c, ok := readRemoteWindowCache(host)
	if !ok {
		return
	}
	kept := make([]remoteWindow, 0, len(c.Windows))
	for _, w := range c.Windows {
		if !drop(w) {
			kept = append(kept, w)
		}
	}
	writeRemoteWindowCache(host, kept, time.UnixMilli(c.SavedAt))
}

// forgetRemoteWindowCache drops the window with tmux id from host's cache.
func forgetRemoteWindowCache(host, id string) {
	forgetRemoteWindowsWhere(host, func(w remoteWindow) bool { return w.ID == id })
}

// forgetRemoteSessionWindowsCache drops every cached window of sess on host.
func forgetRemoteSessionWindowsCache(host, sess string) {
	forgetRemoteWindowsWhere(host, func(w remoteWindow) bool { return w.Session == sess })
}

// remoteKillWindowBody builds the remote-side command that kills one window,
// only on the server the probe would list. Both ids are probe-validated ($N,
// @N), so no remote name reaches the login shell; `$sid:@id` resolves only when
// that window is linked into that session, so a moved or unknown window fails
// (exit 1, the gone class) instead of killing another one.
func remoteKillWindowBody(sessionID, id string) string {
	return remoteKillBody("kill-window", sessionID+":"+id)
}

// sshKillRemoteWindowCtx kills window id of session sessionID on host over ssh.
func sshKillRemoteWindowCtx(ctx context.Context, host, sessionID, id string) error {
	if !isTmuxID(sessionID, '$') || !isTmuxID(id, '@') {
		return errRemoteKillNoID
	}
	return sshKillCtx(ctx, host, remoteKillWindowBody(sessionID, id))
}

// remoteWindowRowItem renders one remote window as a tree child of its host:
// the dimmed session name, then "<index>: <name>". note is a dim suffix; dim
// greys the whole label, for a row no probe just confirmed. Names are made
// render-safe here; remoteSess and target keep the raw session for actions.
func remoteWindowRowItem(host string, w remoteWindow, note, cHost, cDim string, dim bool) listItem {
	reset := "\033[0m"
	sess := remoteDisplayName(w.Session)
	name := remoteDisplayName(w.Name)
	fullSess, fullName := sanitizeStatusText(w.Session), sanitizeStatusText(w.Name)
	idx := strconv.Itoa(w.Index)
	body := sess + " " + idx + ": " + name
	plain := body
	if note != "" {
		plain += "  " + note
	}
	label := cDim + sess + reset + " " + idx + ": " + name
	switch {
	case dim:
		label = cDim + plain + reset
	case note != "":
		label += cDim + "  " + note + reset
	}
	return listItem{
		isRemoteRow:       true,
		target:            "remote:" + host + ":" + w.Session + ":" + w.ID,
		remoteHost:        host,
		remoteSess:        w.Session,
		remoteWindowID:    w.ID,
		remoteWindowIndex: w.Index,
		remoteSessionID:   w.SessionID,
		remoteWindowName:  name,
		display:           cHost + remoteTreeMid + reset + " " + label,
		displayEnd:        cHost + remoteTreeEnd + reset + " " + label,
		plain:             remoteTreeMid + " " + plain,
		plainEnd:          remoteTreeEnd + " " + plain,
		searchText:        host + "/" + fullSess + ":" + idx + " " + host + " " + fullSess + " " + fullName,
	}
}

// cachedRemoteWindowRows is cachedRemoteSessionRows for the window cache: the
// unbridged windows, dimmed with the cache's age when stale. A cached row is
// never live.
func cachedRemoteWindowRows(c remoteWindowCache, bridges map[string]bool, stale, unreachable bool, now time.Time, cHost, cDim string) []listItem {
	note := ""
	if stale {
		note = "(cached " + formatSnapshotAge(c.SavedAt, now) + ")"
	}
	var rows []listItem
	for _, w := range c.Windows {
		if w.Session == "" || bridgeSessionPresent(bridges, c.Host, w.Session) {
			continue
		}
		row := remoteWindowRowItem(c.Host, w, note, cHost, cDim, stale)
		row.remoteUnreachable = unreachable
		rows = append(rows, row)
	}
	return rows
}

// pendingRemoteWindowItems is pendingRemoteItems for window mode: the first
// paint's Remote section from @remote_bridge_hosts and the window cache alone.
func pendingRemoteWindowItems(tmuxOpts map[string]string, bridges map[string]bool) []listItem {
	hosts := configuredHosts(tmuxOpts)
	if len(hosts) == 0 {
		return nil
	}
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	hostColor := hostColorFunc(tmuxOpts)
	now := time.Now()
	items := make([]listItem, 0, len(hosts)+1)
	items = append(items, remoteHeaderItem(tmuxOpts))
	for _, h := range hosts {
		items = append(items, remoteHostRowItem(tmuxOpts, h, remotePendingNote))
		if c, ok := readRemoteWindowCache(h); ok {
			items = append(items, cachedRemoteWindowRows(c, bridges, savedAtStale(c.SavedAt, now), false, now, hostColor(h), cDim)...)
		}
	}
	return items
}

// remoteWindowsForHost is remoteSessionsForHost for a window probe result:
// the windows of sessions not already bridged locally, and the probe state.
func remoteWindowsForHost(host string, bridges map[string]bool, result remoteProbeResult, err error) ([]remoteWindow, remoteProbeState) {
	if err != nil {
		return nil, probeStateOf(err)
	}
	if len(result.Sessions) == 0 {
		return nil, remoteProbeNoServer
	}
	out := make([]remoteWindow, 0, len(result.Windows))
	for _, w := range result.Windows {
		if bridgeSessionPresent(bridges, host, w.Session) {
			continue
		}
		out = append(out, w)
	}
	return out, remoteProbeOK
}

// collectRemoteWindowItems is collectRemoteItems for window mode: one probe per
// host, then the host row and one row per unbridged window. The cache policy
// is the session picker's; there is no restore probe.
func collectRemoteWindowItems(tmuxOpts map[string]string, bridges map[string]bool, probe func(string) (remoteProbeResult, error)) []listItem {
	hosts := parseRemoteHosts(envOrMap("REMOTE_BRIDGE_HOSTS", tmuxOpts, "@remote_bridge_hosts", ""))
	if len(hosts) == 0 {
		return nil
	}
	if probe == nil {
		probe = sshListRemoteWindows
	}
	localID := readLocalRemoteIdentity()
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	hostColor := hostColorFunc(tmuxOpts)

	type hostResult struct {
		host         string
		windows      []remoteWindow
		state        remoteProbeState
		drop         bool
		tailscaleURL string
		cached       []listItem
	}
	now := time.Now()
	results := make([]hostResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			result, err := probe(h)
			if isRemoteSelf(localID, result.Identity) {
				markCachedRemoteSelfAlias(h)
				results[i] = hostResult{host: h, drop: true}
				return
			}
			if isCachedRemoteSelfAlias(h) && result.Identity.MachineID != "" && result.Identity.User != "" {
				clearCachedRemoteSelfAlias(h)
			}
			windows, state := remoteWindowsForHost(h, bridges, result, err)
			res := hostResult{host: h, windows: windows, state: state}
			switch state {
			case remoteProbeOK:
				writeRemoteWindowCache(h, result.Windows, now)
			case remoteProbeNoServer:
				writeRemoteWindowCache(h, nil, now)
			case remoteProbeUnreachable:
				if c, ok := readRemoteWindowCache(h); ok {
					res.cached = cachedRemoteWindowRows(c, bridges, true, true, now, hostColor(h), cDim)
				}
			case remoteProbeTailscaleCheck:
				res.tailscaleURL = tailscaleCheckURL(err)
			case remoteProbeNeedsAuth, remoteProbeHostKeyChanged:
			}
			results[i] = res
		}(i, h)
	}
	wg.Wait()

	items := make([]listItem, 0, len(hosts)+1)
	hasHosts := false
	for _, r := range results {
		if r.drop {
			continue
		}
		if !hasHosts {
			items = append(items, remoteHeaderItem(tmuxOpts))
			hasHosts = true
		}
		items = append(items, remoteHostRowForState(tmuxOpts, r.host, r.state, r.tailscaleURL, len(r.windows) > 0))
		cH := hostColor(r.host)
		for _, w := range r.windows {
			row := remoteWindowRowItem(r.host, w, "", cH, cDim, false)
			row.remoteLive = true
			items = append(items, row)
		}
		items = append(items, r.cached...)
	}
	if !hasHosts {
		return nil
	}
	return items
}
