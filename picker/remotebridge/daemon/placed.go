package daemon

import "strings"

// placedOption marks a mirror window that lives in a host session it shares
// with local windows (--into). A session-level reader skips a window carrying
// it, since its per-mirror values do not describe the session.
const placedOption = "@bridge_placed"

// hostWindowsFormat lists a host session's windows with the identity this
// daemon stamps. The sock goes last because a socket path may hold '|'.
const hostWindowsFormat = "#{window_id}|#{@bridge_placed}|#{@bridge_sock}"

// mirrorIdentity is the window-scoped identity every mirror window carries, in
// stamp order: the socket a keybind dials, the raw host and remote session a
// window-level reader shows, and the placed marker in placed mode. A window is
// a mirror iff its window-scoped @bridge_host is set.
func mirrorIdentity(cfg Config) [][2]string {
	id := [][2]string{
		{"@bridge_sock", cfg.SockPath},
		{"@bridge_host", cfg.RemoteHost},
		{"@bridge_session", cfg.RemoteSession},
	}
	if cfg.Placed {
		id = append(id, [2]string{placedOption, "1"})
	}
	return id
}

type hostWindow struct {
	id, sock string
	placed   bool
}

// parseHostWindows reads a hostWindowsFormat listing; only the first two '|'
// split a row.
func parseHostWindows(out string) []hostWindow {
	var wins []hostWindow
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 || f[0] == "" {
			continue
		}
		wins = append(wins, hostWindow{id: f[0], placed: f[1] != "", sock: f[2]})
	}
	return wins
}

// hostWindows lists every window of the host session. ok is false when the
// listing could not be made.
func hostWindows(cfg Config) ([]hostWindow, bool) {
	if cfg.LocalTmuxOut == nil {
		return nil, false
	}
	out, err := cfg.LocalTmuxOut("list-windows", "-t", cfg.target(), "-F", hostWindowsFormat)
	if err != nil {
		return nil, false
	}
	return parseHostWindows(out), true
}

// ownWindows is the ids of the host session's windows stamped with this
// daemon's socket — the only windows it may write to or kill in a session it
// shares with local windows and other daemons.
func ownWindows(cfg Config) ([]string, bool) {
	wins, ok := hostWindows(cfg)
	if !ok {
		return nil, false
	}
	var own []string
	for _, w := range wins {
		if w.sock == cfg.SockPath {
			own = append(own, w.id)
		}
	}
	return own, true
}

// hostCurrentIsOwn reports whether following the remote's window selection
// locally is safe: always in a dedicated session, and in a placed host session
// only while its current window is one of ours, so a select-window never pulls
// the user off a local window or another daemon's mirror. A failed read
// answers false, which only skips a follow.
func hostCurrentIsOwn(cfg Config) bool {
	if !cfg.Placed {
		return true
	}
	if cfg.LocalTmuxOut == nil {
		return false
	}
	out, err := cfg.LocalTmuxOut("display-message", "-p", "-t", cfg.target(), "#{@bridge_sock}")
	if err != nil {
		return false
	}
	return strings.TrimSuffix(out, "\n") == cfg.SockPath
}
