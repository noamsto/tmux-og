package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// remoteWindow is one window of a remote tmux session. Session is the raw
// name (the exact =sess target for launcher argv and kill); ID and SessionID
// are tmux's stable @N and $N.
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
//
//nolint:unused // called by the window picker wiring (#902)
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
