package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// remoteProbeTimeout bounds each per-host ssh list-sessions probe so a down
// host cannot stall the session picker's async remote section.
const remoteProbeTimeout = 3 * time.Second

// remoteIdentityPreamble prints machine-id (or uuid/hostname fallback) and the
// remote username on separate lines. Folded into remoteListSessionsCmd so
// identity rides the existing ssh probe with no extra round trip.
const remoteIdentityPreamble = `cat /etc/machine-id 2>/dev/null || sysctl -n kern.uuid 2>/dev/null || hostname; id -un`

// remoteListSessionsBody lists the remote's tmux sessions. Stdout of the full
// command begins with the identity preamble (machine-id line, username line),
// then session names.
var remoteListSessionsBody = remoteTmuxCmd(`list-sessions -F '#{session_name}'`)

// remoteTmuxBin resolves the remote's tmux without a shell assignment: PATH
// first, then the nix per-user profile a non-interactive ssh does not see.
const remoteTmuxBin = `$(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux)`

// remoteTmuxCmd runs one tmux argument string under the same TMUX_TMPDIR /
// binary resolution as og-remote-open. Both legs name the PARENT of the
// socket dir — tmux appends tmux-<uid> to $TMUX_TMPDIR itself — so
// /run/user/<uid> on Linux and /tmp on macOS, which has no $XDG_RUNTIME_DIR
// and keeps its server at tmux's own /tmp/tmux-<uid> default. It tries Linux
// first and a wrong guess costs a stat.
// Both legs are silenced and OR'd, so a missing server yields empty stdout
// rather than an error a caller would read as unreachable. Everything it emits
// must stay fish-safe: no `var=value` assignments — fish login shells reject
// them, and the picker would mark a reachable host unreachable.
func remoteTmuxCmd(args string) string {
	leg := func(tmpdir string) string {
		return `env TMUX_TMPDIR=` + tmpdir + ` ` + remoteTmuxBin + ` ` + args + ` 2>/dev/null`
	}
	return leg(`/run/user/$(id -u)`) + ` || ` + leg(`/tmp`)
}

var remoteListSessionsCmd = remoteIdentityPreamble + `; ` + remoteListSessionsBody

// remoteKillSessionBody builds the remote-side tmux command that kills one
// session. The `=` prefix makes the target an exact name match (a numeric
// session name would otherwise also resolve as a window/pane index), and the
// name is single-quoted by shellQuote so a remote-controlled session name can
// never inject a second command through the remote login shell.
func remoteKillSessionBody(sess string) string {
	return remoteTmuxCmd(`kill-session -t ` + shellQuote("="+sess))
}

// remoteSelfCacheDir holds alias→self verdicts so pendingRemoteItems can omit
// known-self hosts on the first paint without another ssh probe.
var remoteSelfCacheDir = "/tmp/og-remote-self"

// remoteRestorableCmd emits the remote host's own hostname (line 1, used to
// verify a fetched snapshot really belongs to this host) followed by
// tmux-remux's event log as newline-delimited JSON. No tmux server is
// required — tmux-remux reads state.db directly — so this only runs once a
// host has already probed as remoteProbeNoServer. Must stay fish-safe like
// remoteListSessionsCmd: no `var=value` shell assignments.
const remoteRestorableCmd = `hostname; $(command -v tmux-remux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux-remux) list --json 2>/dev/null`

const sshConnectFailureExit = 255

var (
	errRemoteUnreachable    = errors.New("remote unreachable")
	errRemoteNoServer       = errors.New("remote tmux server not running")
	errRemoteNeedsAuth      = errors.New("remote needs interactive authentication")
	errRemoteHostKeyChanged = errors.New("remote host key changed")
	errRemoteTailscaleCheck = errors.New("remote requires a Tailscale SSH check")
	errRemoteSessionGone    = errors.New("remote session already gone")
	// errRemoteKillUnrunnable is a reachable host whose tmux the ssh command
	// could not execute (exit 126/127, or any other non-1 tmux failure). The
	// session may still exist, so the row is kept rather than forgotten.
	errRemoteKillUnrunnable = errors.New("remote could not run tmux")
)

type remoteProbeState int

const (
	remoteProbeOK remoteProbeState = iota
	remoteProbeNoServer
	remoteProbeUnreachable
	remoteProbeNeedsAuth
	remoteProbeHostKeyChanged
	remoteProbeTailscaleCheck
)

// Tree prefixes for a host's session rows; markRemoteTreeEnds decides which
// prefix each row ends up with.
const (
	remoteTreeMid = "├─"
	remoteTreeEnd = "╰─"
)

// parseRemoteHosts splits a whitespace-separated @remote_bridge_hosts value
// into ssh Host aliases. Empty tokens are dropped.
func parseRemoteHosts(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, h := range fields {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// configuredHosts is the @remote_bridge_hosts list with cached self-aliases
// dropped — the host list Tab's cycle and scopeAll's membership check share.
func configuredHosts(tmuxOpts map[string]string) []string {
	return dropCachedSelfAliases(parseRemoteHosts(envOrMap("REMOTE_BRIDGE_HOSTS", tmuxOpts, "@remote_bridge_hosts", "")))
}

// remoteIdentity is a host's machine-id (or uuid/hostname fallback) plus the
// username the ssh probe ran as.
type remoteIdentity struct {
	MachineID string
	User      string
}

// remoteProbeResult is stdout from remoteListSessionsCmd: identity on the first
// two lines, session names after.
type remoteProbeResult struct {
	Identity remoteIdentity
	Sessions []string
}

// readLocalRemoteIdentity returns this machine's identity using the same
// resolution order as remoteIdentityPreamble.
var readLocalRemoteIdentity = func() remoteIdentity {
	return remoteIdentity{
		MachineID: readLocalMachineID(),
		User:      localUsername(),
	}
}

func readLocalMachineID() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	if out, err := exec.Command("sysctl", "-n", "kern.uuid").Output(); err == nil {
		if id := strings.TrimSpace(string(out)); id != "" {
			return id
		}
	}
	hostname, _ := os.Hostname()
	return strings.TrimSpace(hostname)
}

func localUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// parseRemoteProbeOutput splits probe stdout into identity and session names.
func parseRemoteProbeOutput(stdout string) remoteProbeResult {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	var res remoteProbeResult
	if len(lines) >= 1 {
		res.Identity.MachineID = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		res.Identity.User = strings.TrimSpace(lines[1])
	}
	// A failed probe still lands here with whatever stdout it managed, which
	// for an unreachable host is nothing at all.
	if len(lines) > 2 {
		for _, line := range lines[2:] {
			line = strings.TrimSpace(line)
			if line != "" {
				res.Sessions = append(res.Sessions, line)
			}
		}
	}
	return res
}

// isRemoteSelf reports whether remote resolved to this machine as the same user.
func isRemoteSelf(local, remote remoteIdentity) bool {
	if local.MachineID == "" || local.User == "" || remote.MachineID == "" || remote.User == "" {
		return false
	}
	// Cloned VMs can share /etc/machine-id; not a concern for physical fleets.
	return local.MachineID == remote.MachineID && local.User == remote.User
}

// hostFileName maps an ssh host alias onto a single safe path segment.
// Distinct hosts can collide, so a reader that cares must verify the host.
func hostFileName(host string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, host)
}

func remoteSelfCachePath(host string) string {
	return filepath.Join(remoteSelfCacheDir, hostFileName(host))
}

func markCachedRemoteSelfAlias(host string) {
	_ = os.MkdirAll(remoteSelfCacheDir, 0o700)
	_ = os.WriteFile(remoteSelfCachePath(host), []byte("1\n"), 0o600)
}

func clearCachedRemoteSelfAlias(host string) {
	_ = os.Remove(remoteSelfCachePath(host))
}

func isCachedRemoteSelfAlias(host string) bool {
	if !remoteSelfCacheDirTrusted() {
		return false
	}
	path := remoteSelfCachePath(host)
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return false
	}
	return true
}

func remoteSelfCacheDirTrusted() bool {
	info, err := os.Stat(remoteSelfCacheDir)
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return false
	}
	if info.Mode().Perm()&0o002 != 0 {
		return false
	}
	return true
}

func dropCachedSelfAliases(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !isCachedRemoteSelfAlias(h) {
			out = append(out, h)
		}
	}
	return out
}

// remoteCacheStaleAfter is how old a cached session list may get before its
// rows render dimmed with their age.
const remoteCacheStaleAfter = 5 * time.Minute

// remoteSessionCache is one host's last answered probe. Sessions is the raw
// list, before bridge suppression, so a mirror detached since still shows.
type remoteSessionCache struct {
	Host     string   `json:"host"`
	SavedAt  int64    `json:"saved_at"` // unix millis, formatSnapshotAge's unit
	Sessions []string `json:"sessions"`
}

// remoteSessionCacheDir is $XDG_CACHE_HOME/tmux-og/remote, or "" when no
// absolute base resolves.
func remoteSessionCacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "tmux-og", "remote")
}

// ownerOnlyDir rejects a cache dir another user could have planted or can
// write into: the rows it holds become Enter targets.
func ownerOnlyDir(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return false
	}
	return info.Mode().Perm()&0o077 == 0
}

func writeRemoteSessionCache(host string, sessions []string, now time.Time) {
	dir := remoteSessionCacheDir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil || !ownerOnlyDir(dir) {
		return
	}
	data, err := json.Marshal(remoteSessionCache{Host: host, SavedAt: now.UnixMilli(), Sessions: sessions})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil {
		_ = os.Remove(f.Name())
		return
	}
	// Rename, so a picker reading concurrently never sees a torn file.
	if os.Rename(f.Name(), filepath.Join(dir, hostFileName(host)+".json")) != nil {
		_ = os.Remove(f.Name())
	}
}

// forgetRemoteSessionCache drops sess from host's cached listing, preserving
// the snapshot's SavedAt so the host's remaining rows keep their (cached …)
// age instead of jumping to fresh. A missing or untrusted cache is a no-op.
func forgetRemoteSessionCache(host, sess string) {
	c, ok := readRemoteSessionCache(host)
	if !ok {
		return
	}
	kept := make([]string, 0, len(c.Sessions))
	for _, s := range c.Sessions {
		if s != sess {
			kept = append(kept, s)
		}
	}
	writeRemoteSessionCache(host, kept, time.UnixMilli(c.SavedAt))
}

func readRemoteSessionCache(host string) (remoteSessionCache, bool) {
	dir := remoteSessionCacheDir()
	if dir == "" || !ownerOnlyDir(dir) {
		return remoteSessionCache{}, false
	}
	path := filepath.Join(dir, hostFileName(host)+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return remoteSessionCache{}, false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return remoteSessionCache{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return remoteSessionCache{}, false
	}
	var c remoteSessionCache
	if json.Unmarshal(data, &c) != nil || c.Host != host {
		return remoteSessionCache{}, false
	}
	return c, true
}

func remoteCacheStale(c remoteSessionCache, now time.Time) bool {
	return now.Sub(time.UnixMilli(c.SavedAt)) > remoteCacheStaleAfter
}

// firstPaintBridges indexes the mirror sessions already in the list under the
// legacy <host>-<sess> key: enough to keep a cached row from duplicating an
// open mirror without forking tmux for @bridge_session before first paint.
// The probe's result, built from the authoritative pair key, replaces it.
func firstPaintBridges(items []listItem) map[string]bool {
	bridges := make(map[string]bool)
	for _, it := range items {
		if it.bridgeHost != "" {
			bridges[bridgeSessionLegacyKey(it.session)] = true
		}
	}
	return bridges
}

// localBridgeSession is the local mirror name for a remote host+session pair.
func localBridgeSession(host, sess string) string {
	return host + "-" + sess
}

// bridgeMirror is one local session already mirroring a remote host+session.
type bridgeMirror struct {
	host, sess, target string // target = the local mirror's session name
}

func bridgeSessionKey(host, sess string) string {
	return "pair\x00" + host + "\x00" + sess
}

func bridgeSessionLegacyKey(name string) string {
	return "legacy\x00" + name
}

// parseBridgeSessions reads the session options that identify local mirror
// sessions. Ordinary local sessions are deliberately ignored, even when their
// names happen to equal the old <host>-<session> convention.
func parseBridgeSessions(raw string) map[string]bool {
	bridges := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		if len(parts) == 3 && parts[2] != "" {
			bridges[bridgeSessionKey(parts[1], parts[2])] = true
			continue
		}
		bridges[bridgeSessionLegacyKey(parts[0])] = true
	}
	return bridges
}

func collectBridgeSessions() map[string]bool {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}|#{@bridge_host}|#{@bridge_session}").Output()
	if err != nil {
		return nil
	}
	return parseBridgeSessions(string(out))
}

func bridgeSessionPresent(bridges map[string]bool, host, sess string) bool {
	return bridges[bridgeSessionKey(host, sess)] || bridges[bridgeSessionLegacyKey(localBridgeSession(host, sess))]
}

// parseBridgeMirrors is the pure parse of `tmux list-sessions -F
// "#{session_name}|#{@bridge_host}|#{@bridge_session}"` output into the
// mirrors it describes. A row with no @bridge_host isn't a mirror. A row
// with @bridge_session empty (legacy bridge, no option set) derives sess by
// stripping the "<host>-" prefix off the session name — same convention
// localBridgeSession assumes; a name that doesn't carry the prefix is
// skipped (nothing to resolve).
func parseBridgeMirrors(raw string) []bridgeMirror {
	var mirrors []bridgeMirror
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		name, host := parts[0], parts[1]
		if len(parts) == 3 && parts[2] != "" {
			mirrors = append(mirrors, bridgeMirror{host: host, sess: parts[2], target: name})
			continue
		}
		prefix := host + "-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		mirrors = append(mirrors, bridgeMirror{host: host, sess: strings.TrimPrefix(name, prefix), target: name})
	}
	return mirrors
}

// collectBridgeMirrors runs the tmux call. Off-thread only — see tui.go's
// refreshDataCmd, the same rule as every other tmux/ps fork here.
func collectBridgeMirrors() []bridgeMirror {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}|#{@bridge_host}|#{@bridge_session}").Output()
	if err != nil {
		return nil
	}
	return parseBridgeMirrors(string(out))
}

// remoteSessionsForHost probes one host for live tmux session names. On
// remoteProbeOK the returned list is the remote sessions not already bridged
// locally (may be empty when every session is already open).
// An empty list with no error means the remote has no server to list.
func remoteSessionsForHost(host string, bridges map[string]bool, probe func(string) (remoteProbeResult, error)) ([]string, remoteProbeState) {
	result, err := probe(host)
	if err != nil {
		switch {
		case errors.Is(err, errRemoteNoServer):
			return nil, remoteProbeNoServer
		case errors.Is(err, errRemoteNeedsAuth):
			return nil, remoteProbeNeedsAuth
		case errors.Is(err, errRemoteHostKeyChanged):
			return nil, remoteProbeHostKeyChanged
		case errors.Is(err, errRemoteTailscaleCheck):
			return nil, remoteProbeTailscaleCheck
		}
		return nil, remoteProbeUnreachable
	}
	if len(result.Sessions) == 0 {
		return nil, remoteProbeNoServer
	}
	out := make([]string, 0, len(result.Sessions))
	for _, sess := range result.Sessions {
		if sess == "" {
			continue
		}
		if bridgeSessionPresent(bridges, host, sess) {
			continue
		}
		out = append(out, sess)
	}
	return out, remoteProbeOK
}

// authFailurePatterns are the ssh stderr signatures meaning a human at a real
// terminal could fix this by answering a prompt. Only consulted on exit 255,
// where the failure is ssh's own.
var authFailurePatterns = []string{
	"Host key verification failed",
	// The "(" anchors on ssh's own "Permission denied (publickey,password)."
	// rather than the bare phrase, which also appears in a local firewall's
	// "ssh: connect to host X port 22: Permission denied" (EACCES) — a
	// genuinely down host, not one a prompt could fix.
	"Permission denied (",
	"Too many authentication failures",
	"keyboard-interactive",
}

// hostKeyChangedPattern is ssh's warning that the host key no longer matches
// known_hosts. Kept out of authFailurePatterns deliberately: this is the
// signature of a MITM as much as of a reinstalled host, so it must never reach
// a flow that invites the user to connect. ssh prints it alongside "Host key
// verification failed", so it is matched first.
const hostKeyChangedPattern = "REMOTE HOST IDENTIFICATION HAS CHANGED"

// revokedHostKeyPattern is ssh's refusal banner for a key listed in a
// RevokedHostKeys file. It prints no hostKeyChangedPattern alongside it, only a
// bare "Host key verification failed.", which would otherwise fall into
// authFailurePatterns and offer to connect to a host ssh has already refused.
const revokedHostKeyPattern = "REVOKED HOST KEY DETECTED"

// tailscaleCheckPattern is Tailscale SSH's own banner when the peer's ACL
// rule requires an interactive re-check (`action: "check"`). It arrives on
// STDOUT over the already-established ssh session, not through ssh's own
// auth flow, and blocks until the check clears — which is why the probe
// always exhausts remoteProbeTimeout instead of exiting 255 (#486).
const tailscaleCheckPattern = "Tailscale SSH requires an additional check"

// tailscaleCheckURLPattern extracts the per-attempt login URL tailscaled
// prints on the line after tailscaleCheckPattern (e.g. "# To authenticate,
// visit: https://login.tailscale.com/a/xyz"). Restricted to a URL-safe
// character class and capped in length deliberately: this is
// remote-controlled stdout, and an unrestricted \S capture matches ESC
// (0x1B) — the remote-row preview is not run through stripStringEscapes,
// so an uncapped capture could carry ANSI into a row fitVisibleWidth
// accounts for in visible cells.
var tailscaleCheckURLPattern = regexp.MustCompile(`To authenticate, visit:\s*([A-Za-z0-9:/_.\-]{1,200})`)

// detectTailscaleCheck reports whether stdout carries tailscaled's "check"
// banner and, if so, the login URL it printed (empty if the banner's shape
// ever changes upstream and the URL line isn't found).
func detectTailscaleCheck(stdout string) (url string, ok bool) {
	if !strings.Contains(stdout, tailscaleCheckPattern) {
		return "", false
	}
	if m := tailscaleCheckURLPattern.FindStringSubmatch(stdout); len(m) == 2 {
		return m[1], true
	}
	return "", true
}

// classifyProbeErr decides which failure a non-zero probe was. ssh exits 255
// when it could not reach the host; any other status is the remote command's
// own, so the host answered and only its tmux server is missing (#266). Within
// 255, stderr distinguishes a host that merely wants an interactive answer from
// one that is genuinely down (#357) — an unrecognised 255 stays unreachable, so
// being wrong costs a stale label rather than a pointless password prompt. The
// timeout branch also inspects stdout: a Tailscale SSH "check" host is killed
// by the probe's own deadline rather than exiting 255, but tailscaled's check
// banner still arrives on stdout before the process is killed, so a killed
// probe's stdout is still usable evidence (#486).
// classifyKillErr maps a failed kill-session ssh round trip to the same
// host-level states classifyProbeErr maps a probe to, with one difference: a
// non-255 exit is the remote tmux command's own failure. Exit 1 is tmux
// reporting the session already absent (or no server), so it maps to
// errRemoteSessionGone; a command that could not be executed at all (126/127,
// e.g. tmux missing from both PATH and the per-user profile) or any other
// uncertain non-zero code maps to errRemoteKillUnrunnable, which keeps the row
// rather than forgetting a session never proven gone.
func classifyKillErr(err error, stdout, stderr string, timedOut bool) error {
	if timedOut {
		if url, ok := detectTailscaleCheck(stdout); ok {
			return &tailscaleCheckErr{url: url}
		}
		return fmt.Errorf("%w: kill timed out", errRemoteUnreachable)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != sshConnectFailureExit {
		if exitErr.ExitCode() == 1 {
			return fmt.Errorf("%w: %w", errRemoteSessionGone, err)
		}
		return fmt.Errorf("%w: %w", errRemoteKillUnrunnable, err)
	}
	if strings.Contains(stderr, hostKeyChangedPattern) || strings.Contains(stderr, revokedHostKeyPattern) {
		return fmt.Errorf("%w: %w", errRemoteHostKeyChanged, err)
	}
	for _, p := range authFailurePatterns {
		if strings.Contains(stderr, p) {
			return fmt.Errorf("%w: %w", errRemoteNeedsAuth, err)
		}
	}
	return fmt.Errorf("%w: %w", errRemoteUnreachable, err)
}

func classifyProbeErr(err error, stdout, stderr string, timedOut bool) error {
	if timedOut {
		if url, ok := detectTailscaleCheck(stdout); ok {
			return &tailscaleCheckErr{url: url}
		}
		return fmt.Errorf("%w: probe timed out", errRemoteUnreachable)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != sshConnectFailureExit {
		return fmt.Errorf("%w: %w", errRemoteNoServer, err)
	}
	if strings.Contains(stderr, hostKeyChangedPattern) || strings.Contains(stderr, revokedHostKeyPattern) {
		return fmt.Errorf("%w: %w", errRemoteHostKeyChanged, err)
	}
	for _, p := range authFailurePatterns {
		if strings.Contains(stderr, p) {
			return fmt.Errorf("%w: %w", errRemoteNeedsAuth, err)
		}
	}
	return fmt.Errorf("%w: %w", errRemoteUnreachable, err)
}

// tailscaleCheckErr carries the login URL classifyProbeErr captured for a
// Tailscale SSH "check" host; tailscaleCheckURL recovers it via errors.As.
type tailscaleCheckErr struct {
	url string
}

func (e *tailscaleCheckErr) Error() string {
	if e.url == "" {
		return errRemoteTailscaleCheck.Error()
	}
	return errRemoteTailscaleCheck.Error() + ": " + e.url
}

func (e *tailscaleCheckErr) Unwrap() error { return errRemoteTailscaleCheck }

// tailscaleCheckURL recovers the login URL classifyProbeErr captured for a
// Tailscale SSH "check" host, or "" if err isn't that state or carried none.
func tailscaleCheckURL(err error) string {
	var e *tailscaleCheckErr
	if errors.As(err, &e) {
		return e.url
	}
	return ""
}

// remoteAuthStartFailure classifies the tea.ExecProcess callback error for the
// auth handshake popup. *exec.ExitError means og-remote-auth ran and
// already explained itself — it pauses on any failure it prints before
// returning the pty — so only a *exec.Error (the process never started at
// all, most often a stale PATH after a tmux-og bump until the server
// restarts) has nothing on screen to explain, and that is the only case worth
// surfacing into the status line.
func remoteAuthStartFailure(err error) (string, bool) {
	var startErr *exec.Error
	if errors.As(err, &startErr) {
		return err.Error(), true
	}
	return "", false
}

// sshKillRemoteSession kills sess on host over ssh. It returns nil on success
// and a classified error otherwise: errRemoteSessionGone when the remote tmux
// ran and reported the session already absent, errRemoteUnreachable (or the
// auth/host-key/tailscale states) when ssh itself could not complete.
func sshKillRemoteSession(host, sess string) error {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-T",
		host,
		"--",
		remoteKillSessionBody(sess),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	return classifyKillErr(err, stdout.String(), stderr.String(), ctx.Err() != nil)
}

// sshListRemoteSessions runs the same path/tmpdir resolution as
// og-remote-open so a remote without tmux on the non-interactive PATH still
// lists. Returns session names, or an error wrapping errRemoteUnreachable /
// errRemoteNoServer.
func sshListRemoteSessions(host string) (remoteProbeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-T",
		host,
		"--",
		remoteListSessionsCmd,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	parsed := parseRemoteProbeOutput(stdout.String())
	if err != nil {
		return parsed, classifyProbeErr(err, stdout.String(), stderr.String(), ctx.Err() != nil)
	}
	return parsed, nil
}

// remuxManifestSession is the subset of a tmux-remux snapshot session the
// picker needs to render a restorable row.
type remuxManifestSession struct {
	Name         string `json:"name"`
	LastAttached int64  `json:"last_attached"`
}

// remuxManifest mirrors tmux-remux's snapshot.Manifest (internal/snapshot/manifest.go
// in noamsto/tmux-remux) — only the fields the picker reads.
type remuxManifest struct {
	Host     string                 `json:"host"`
	SavedAt  int64                  `json:"saved_at"`
	Sessions []remuxManifestSession `json:"sessions"`
}

// remuxEvent mirrors the fields of tmux-remux's store.Event this package
// needs, as emitted (one JSON object per line) by `tmux-remux list --json`.
// The upstream type carries no json tags, so field names must match Go's
// default (capitalized) encoding exactly.
type remuxEvent struct {
	Ts           int64
	Kind         string
	ManifestJSON string
}

// newestSnapshotManifest scans newline-delimited tmux-remux events for the
// snapshot with the highest timestamp and decodes its manifest. A snapshot is
// a point-in-time dump of every session on the server at save time, not an
// append-only session list, so only the single newest one is ever consulted —
// unioning across snapshots would resurrect sessions closed since. Malformed
// lines are skipped: this reads over an ssh pipe, where truncated output is a
// real failure mode, not a programming error.
func newestSnapshotManifest(ndjson string) (remuxManifest, bool) {
	var best remuxEvent
	found := false
	for _, line := range strings.Split(ndjson, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev remuxEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Kind != "snapshot" {
			continue
		}
		if !found || ev.Ts > best.Ts {
			best, found = ev, true
		}
	}
	if !found {
		return remuxManifest{}, false
	}
	var m remuxManifest
	if err := json.Unmarshal([]byte(best.ManifestJSON), &m); err != nil {
		return remuxManifest{}, false
	}
	return m, true
}

// filterThrowawaySessions drops sessions tmux reports as never attached
// (session_last_attached == 0) — the exact signature of an ephemeral session
// like probe-verify (seeded and killed by TestSSHListRemoteSessionsLive)
// rather than one a person was actually using. This is a principled signal,
// not a name denylist: any detached-and-abandoned session is caught the same
// way.
func filterThrowawaySessions(sessions []remuxManifestSession) []remuxManifestSession {
	out := make([]remuxManifestSession, 0, len(sessions))
	for _, s := range sessions {
		if s.LastAttached == 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// formatSnapshotAge renders how long ago a snapshot was saved, coarsening
// units the further back it is (mirrors usageResetSuffix's granularity in
// picker/statusline/usage.go). A stale manifest is worse than no row, so its
// age has to be legible at a glance, not buried in a tooltip.
func formatSnapshotAge(savedAtMillis int64, now time.Time) string {
	age := now.Sub(time.UnixMilli(savedAtMillis))
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
	}
}

// restorableFromProbeOutput parses remoteRestorableCmd's stdout (the remote's
// own hostname, then tmux-remux's ndjson event log), verifies the snapshot's
// Host field against the hostname that actually answered ssh, and drops
// throwaway sessions. Split out from sshListRestorableSessions so the host
// check — the one security-relevant rule here, guarding against a stale or
// wrong-host state.db producing rows — is testable without a real ssh.
func restorableFromProbeOutput(stdout string) (remuxManifest, error) {
	remoteHostname, ndjson, ok := strings.Cut(stdout, "\n")
	if !ok {
		return remuxManifest{}, errors.New("no manifest data")
	}
	m, found := newestSnapshotManifest(ndjson)
	if !found {
		return remuxManifest{}, errors.New("no snapshot to restore")
	}
	if m.Host != strings.TrimSpace(remoteHostname) {
		return remuxManifest{}, fmt.Errorf("snapshot host %q does not match %q", m.Host, remoteHostname)
	}
	m.Sessions = filterThrowawaySessions(m.Sessions)
	return m, nil
}

// sshListRestorableSessions fetches the newest tmux-remux snapshot from a
// serverless host and returns its sessions, verified and filtered by
// restorableFromProbeOutput. Only meaningful once a host has already probed
// as remoteProbeNoServer — a live server's sessions come from
// sshListRemoteSessions instead.
func sshListRestorableSessions(host string) (remuxManifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=2",
		"-T",
		host,
		"--",
		remoteRestorableCmd,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return remuxManifest{}, classifyProbeErr(err, stdout.String(), stderr.String(), ctx.Err() != nil)
	}
	return restorableFromProbeOutput(stdout.String())
}

// bridgeCtlKillWindow sends the ctl kill-window verb — the one prefix+& sends
// from inside a mirror. The local mirror window goes when the daemon's
// reconcile sees the remote lose it, never by a local kill.
func bridgeCtlKillWindow(tmuxOpts map[string]string, sock, pane string) error {
	// An option, not a PATH lookup — see @bridge_ctl_bin in the config.
	bin := envOrMap("BRIDGE_CTL_BIN", tmuxOpts, "@bridge_ctl_bin", "og-remote-bridge-ctl")
	cmd := exec.Command(bin, "--sock="+sock, "kill-window", pane)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := lastNonEmptyLine(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// openRemoteBridge launches og-remote-open for host[/sess] and returns any
// start error (the script switches the client itself on success). restore
// signals a row built from a tmux-remux snapshot rather than a live probe
// (#268): the session doesn't exist on the remote yet, so the launcher must
// restore it before there's anything to bridge into.
func openRemoteBridge(tmuxOpts map[string]string, host, sess string, restore bool) error {
	args := []string{host}
	if sess != "" {
		args = append(args, sess)
	}
	// An option, not a PATH lookup — see @remote_open_bin in the config.
	bin := envOrMap("REMOTE_OPEN_BIN", tmuxOpts, "@remote_open_bin", "og-remote-open")
	cmd := exec.Command(bin, args...)
	if restore {
		cmd.Env = append(os.Environ(), "OG_REMOTE_RESTORE=1")
	}
	cmd.Stdout = os.Stderr
	// Captured, not inherited: the picker owns the screen, so a failure has to
	// come back as a string for the hint line rather than paint over the popup.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := lastNonEmptyLine(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// launchRemoteBridgeDetached fires the same launcher openRemoteBridge runs,
// without waiting for it. Setsid so tmux tearing down this popup's pane
// doesn't take the launcher (and the daemon it backgrounds) with it — the
// same reason og-remote-open.sh setsids the daemon it starts.
func launchRemoteBridgeDetached(tmuxOpts map[string]string, host, sess string, restore bool) {
	args := []string{host}
	if sess != "" {
		args = append(args, sess)
	}
	bin := envOrMap("REMOTE_OPEN_BIN", tmuxOpts, "@remote_open_bin", "og-remote-open")
	cmd := exec.Command(bin, args...)
	if restore {
		cmd.Env = append(os.Environ(), "OG_REMOTE_RESTORE=1")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Nothing left to report to once the popup has quit, but a spawn failure
	// (bad @remote_open_bin, PATH) must not vanish silently — it would
	// otherwise look identical to "still dialing" for this session.
	if err := cmd.Start(); err != nil {
		logEvent("picker", "event", "open_marked_launch_failed", "host", host, "sess", sess, "error", err.Error())
	}
}

// lastNonEmptyLine picks the launcher's most specific complaint: ssh and
// systemctl noise comes first, the script's own message last.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// remoteHeaderItem is the "── Remote ──" divider row. Shared by the
// synchronous pending render (Task 2) and the probed result so both agree on
// layout — a pending header must look identical to a resolved one.
func remoteHeaderItem(tmuxOpts map[string]string) listItem {
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	reset := "\033[0m"
	rule := "── Remote " + strings.Repeat("─", 220)
	return listItem{
		display:        cDim + rule + reset,
		plain:          rule,
		isHeader:       true,
		isRemoteHeader: true,
		headerLabel:    "Remote",
		headerIcon:     envOrMap("PICKER_ICON_REMOTE", tmuxOpts, "@icon_remote", iconRemote),
	}
}

// hostPalette is the set of @thm_* options a remote host may be tinted with,
// each paired with its Mocha fallback. Excludes mauve, red and blue — local
// sessions, error states and the path icon already mean those.
var hostPalette = [...][2]string{
	{"THM_PEACH", "#fab387"},
	{"THM_TEAL", "#94e2d5"},
	{"THM_PINK", "#f5c2e7"},
	{"THM_YELLOW", "#f9e2af"},
	{"THM_SKY", "#89dceb"},
}

// hostColorFunc returns the per-host tint drawn everywhere a host or one of
// its sessions appears — Host column, mirror session name, Remote row and its
// children — so one host is one colour across the popup. Configured hosts
// (@remote_bridge_hosts) are assigned in sorted order: each host's hash picks
// a preferred palette slot, then linear-probes to the next free one, so the
// whole set gets distinct colours when possible with minimal churn when the
// set changes. More than len(hostPalette) hosts wrap and reuse slots. nil or
// empty opts, and hosts outside the configured list, fall back to plain hash
// (collisions possible).
func hostColorFunc(tmuxOpts map[string]string) func(string) string {
	colors := make([]string, len(hostPalette))
	for i, p := range hostPalette {
		colors[i] = ansiFg(envOrMap(p[0], tmuxOpts, "@"+strings.ToLower(p[0]), p[1]))
	}
	n := len(colors)
	preferredSlot := func(host string) int {
		h := fnv.New32a()
		_, _ = h.Write([]byte(host))
		return int(h.Sum32() % uint32(n))
	}
	hashColor := func(host string) string {
		return colors[preferredSlot(host)]
	}
	hosts := parseRemoteHosts(envOrMap("REMOTE_BRIDGE_HOSTS", tmuxOpts, "@remote_bridge_hosts", ""))
	if len(hosts) == 0 {
		return func(host string) string {
			if host == "" {
				return ""
			}
			return hashColor(host)
		}
	}
	sorted := append([]string(nil), hosts...)
	sort.Strings(sorted)
	assigned := make(map[string]int, len(sorted))
	used := make([]bool, n)
	for _, host := range sorted {
		preferred := preferredSlot(host)
		slot := preferred
		for i := 0; i < n; i++ {
			candidate := (preferred + i) % n
			if !used[candidate] {
				slot = candidate
				used[candidate] = true
				break
			}
			slot = candidate
		}
		assigned[host] = slot
	}
	return func(host string) string {
		if host == "" {
			return ""
		}
		if slot, ok := assigned[host]; ok {
			return colors[slot]
		}
		return hashColor(host)
	}
}

// sessionDisplayName trims the "${host}-" prefix og-remote-open bakes into
// a mirror session's name. Session-picker rows only, where the Host column
// carries the host: window mode and everything outside the picker have no host
// of their own to read, which is why the session itself is never renamed and
// the untrimmed name stays the tmux target.
func sessionDisplayName(name, bridgeHost string) string {
	if bridgeHost == "" {
		return name
	}
	trimmed := strings.TrimPrefix(name, bridgeHost+"-")
	if trimmed == "" {
		return name
	}
	return trimmed
}

// remoteHostRowItem renders one host's row with the given trailing note —
// either a resolved annotation ("(no server — Enter starts one)", …) or
// remotePendingNote before the probe has run. The row opens the remote's
// most-recent session and keeps the section alive once every session is
// bridged — but it's only selectable while it matched the filter query on
// its own text; a copy pulled in purely as tree context for a matching child
// session (marked remoteContextOnly by withFilter in tui.go) is not.
func remoteHostRowItem(tmuxOpts map[string]string, host, note string) listItem {
	cHost := hostColorFunc(tmuxOpts)(host)
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	iSess := envOrMap("PICKER_ICON_SESSION", tmuxOpts, "@icon_session", iconSession)
	reset := "\033[0m"

	display := fmt.Sprintf("%s %s", cHost+iSess+reset, cHost+host+reset)
	plain := fmt.Sprintf("%s %s", iSess, host)
	if note != "" {
		display += "  " + cDim + note + reset
		plain += "  " + note
	}
	return listItem{
		isRemoteRow: true,
		target:      "remote:" + host,
		remoteHost:  host,
		display:     display,
		plain:       plain,
		searchText:  host,
	}
}

// remotePendingNote is the placeholder annotation on a host row before its
// ssh probe returns — a dim ellipsis, since no state (unreachable / no
// server / session list) is known yet (#312).
const remotePendingNote = "…"

// pendingRemoteItems builds the Remote section synchronously from
// @remote_bridge_hosts alone — no ssh, so it belongs on the first-paint
// path. Each host gets exactly the row collectRemoteItems would give it,
// with remotePendingNote standing in for whatever annotation the probe will
// resolve, and the host's cached sessions (#631) under it so a query can
// match them before any probe answers. remoteMsg (collectRemoteItems's
// result) replaces this slice wholesale once every host's probe returns.
func pendingRemoteItems(tmuxOpts map[string]string, bridges map[string]bool) []listItem {
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
		if c, ok := readRemoteSessionCache(h); ok {
			items = append(items, cachedRemoteSessionRows(c, bridges, remoteCacheStale(c, now), false, now, hostColor(h), cDim)...)
		}
	}
	return items
}

// remoteSessionRowItem renders one session row under host. note is a dim
// suffix; dim greys the session name too, for a row no probe just confirmed.
func remoteSessionRowItem(host, sess, note, cHost, cDim string, dim bool) listItem {
	reset := "\033[0m"
	plain := sess
	if note != "" {
		plain += "  " + note
	}
	label := sess
	switch {
	case dim:
		label = cDim + plain + reset
	case note != "":
		label = sess + cDim + "  " + note + reset
	}
	return listItem{
		isRemoteRow: true,
		target:      "remote:" + host + ":" + sess,
		remoteHost:  host,
		remoteSess:  sess,
		display:     cHost + remoteTreeMid + reset + " " + label,
		displayEnd:  cHost + remoteTreeEnd + reset + " " + label,
		plain:       remoteTreeMid + " " + plain,
		plainEnd:    remoteTreeEnd + " " + plain,
		searchText:  host + "/" + sess + " " + host + " " + sess,
	}
}

// cachedRemoteSessionRows renders a cache's unbridged sessions; stale rows are
// dimmed and carry the cache's age. unreachable marks every row inert for
// picker/tui.go's markable — set only once a probe has actually confirmed the
// host down, never for the pre-probe first-paint call, where reachability is
// still unknown and the row may resolve live a moment later.
func cachedRemoteSessionRows(c remoteSessionCache, bridges map[string]bool, stale, unreachable bool, now time.Time, cHost, cDim string) []listItem {
	note := ""
	if stale {
		note = "(cached " + formatSnapshotAge(c.SavedAt, now) + ")"
	}
	var rows []listItem
	for _, sess := range c.Sessions {
		if sess == "" || bridgeSessionPresent(bridges, c.Host, sess) {
			continue
		}
		row := remoteSessionRowItem(c.Host, sess, note, cHost, cDim, stale)
		row.remoteUnreachable = unreachable
		rows = append(rows, row)
	}
	return rows
}

// collectRemoteItems builds the "Remote" suggestion rows (header + hosts /
// sessions) by probing every configured host over ssh. Runs off the
// first-paint path; the result merges via remoteMsg, replacing
// pendingRemoteItems's synchronous placeholder (#312).
func collectRemoteItems(tmuxOpts map[string]string, bridges map[string]bool, probe func(string) (remoteProbeResult, error), restoreProbe func(string) (remuxManifest, error)) []listItem {
	hosts := parseRemoteHosts(envOrMap("REMOTE_BRIDGE_HOSTS", tmuxOpts, "@remote_bridge_hosts", ""))
	if len(hosts) == 0 {
		return nil
	}
	if probe == nil {
		probe = sshListRemoteSessions
	}
	if restoreProbe == nil {
		restoreProbe = sshListRestorableSessions
	}
	localID := readLocalRemoteIdentity()

	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	hostColor := hostColorFunc(tmuxOpts)

	type hostResult struct {
		host            string
		sess            []string
		state           remoteProbeState
		restorable      []remuxManifestSession
		manifestSavedAt int64
		drop            bool
		tailscaleURL    string
		cached          []listItem
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
			sess, state := remoteSessionsForHost(h, bridges, func(string) (remoteProbeResult, error) {
				if err != nil {
					return result, err
				}
				return result, nil
			})
			res := hostResult{host: h, sess: sess, state: state}
			switch state {
			case remoteProbeOK:
				writeRemoteSessionCache(h, result.Sessions, now)
			case remoteProbeNoServer:
				writeRemoteSessionCache(h, nil, now)
			case remoteProbeUnreachable:
				// A host that can't answer keeps its last answer, always stale.
				// The auth/host-key/tailscale states get none: those rows must
				// stay the only thing Enter can reach for that host.
				if c, ok := readRemoteSessionCache(h); ok {
					res.cached = cachedRemoteSessionRows(c, bridges, true, true, now, hostColor(h), cDim)
				}
			case remoteProbeTailscaleCheck:
				res.tailscaleURL = tailscaleCheckURL(err)
			}
			if state == remoteProbeNoServer {
				if m, err := restoreProbe(h); err == nil && len(m.Sessions) > 0 {
					res.restorable = m.Sessions
					res.manifestSavedAt = m.SavedAt
				}
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
		note := ""
		switch r.state {
		case remoteProbeUnreachable:
			// The host may be back up by the time it is picked.
			note = "(unreachable — open default)"
		case remoteProbeNeedsAuth:
			// ssh wants an answer a batch-mode probe can never give (#357).
			note = "(auth needed — Enter to connect)"
		case remoteProbeHostKeyChanged:
			note = "(host key changed — verify manually)"
		case remoteProbeTailscaleCheck:
			// Not ssh auth — tailscaled intercepts and blocks on the remote's ACL
			// check, which og-remote-auth's ssh-copy-id/ControlMaster flow
			// can't clear regardless of keys or multiplexing (#486). The one
			// remedy that reliably works is running ssh interactively yourself.
			note = "(tailscale check — run: ssh " + r.host + ")"
		case remoteProbeNoServer:
			// The launcher cold-starts the host's own startup session (#287).
			// The host row itself never restores — it carries no
			// remoteSess/remoteRestore either way — so its note doesn't
			// change when restorable child rows exist below it; those rows
			// carry their own "(restore — saved …)" suffix instead.
			note = "(no server — Enter starts one)"
		default:
			if len(r.sess) == 0 {
				note = "(all open)"
			}
		}
		hostRow := remoteHostRowItem(tmuxOpts, r.host, note)
		switch r.state {
		case remoteProbeNeedsAuth:
			hostRow.remoteNeedsAuth = true
		case remoteProbeHostKeyChanged:
			hostRow.remoteInert = true
		case remoteProbeTailscaleCheck:
			hostRow.remoteTailscaleCheck = true
			hostRow.remoteTailscaleURL = r.tailscaleURL
		}
		items = append(items, hostRow)
		cH := hostColor(r.host)
		for _, sess := range r.sess {
			items = append(items, remoteSessionRowItem(r.host, sess, "", cH, cDim, false))
		}
		for _, s := range r.restorable {
			row := remoteSessionRowItem(r.host, s.Name, "(restore — saved "+formatSnapshotAge(r.manifestSavedAt, now)+")", cH, cDim, false)
			row.remoteRestore = true
			items = append(items, row)
		}
		items = append(items, r.cached...)
	}
	if !hasHosts {
		return nil
	}
	return items
}

// bridgePIDFromFile parses a bridge daemon pidfile's raw contents into a
// validated PID. Pure logic split out for unit testing; malformed or empty
// content returns ok=false rather than erroring, matching how
// scripts/lib-remote.sh's remote_daemon_alive treats an unreadable pidfile.
func bridgePIDFromFile(raw string) (pid int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// stopBridgeDaemon best-effort SIGTERMs the remote-bridge daemon mirroring
// sess, found via the @bridge_sock session option the daemon stamps on
// itself. All failures are swallowed: this is opportunistic cleanup, not a
// required step — og-remote-open.sh's stale-daemon check self-heals on
// the next reopen regardless of whether this signal lands. The daemon's own
// SIGTERM handler already tears itself down (removes its socket/pidfile,
// exits), so this only needs to deliver the signal.
func stopBridgeDaemon(sess string) {
	logEvent("picker", "event", "stop_bridge_daemon", "target", sess)
	out, err := exec.Command("tmux", "display-message", "-p", "-t", sess, "#{@bridge_sock}").Output()
	if err != nil {
		return
	}
	sock := strings.TrimSpace(string(out))
	if sock == "" {
		return
	}
	raw, err := os.ReadFile(sock + ".pid")
	if err != nil {
		return
	}
	pid, ok := bridgePIDFromFile(string(raw))
	if !ok {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	proc.Signal(syscall.SIGTERM) //nolint:errcheck
}

// stopBridgeDaemonFn seals stopBridgeDaemon for tests: the remote-session kill
// path signals the mirror daemon through it, and a unit test cannot read a
// live server's @bridge_sock. Production uses the real function.
var stopBridgeDaemonFn = stopBridgeDaemon
