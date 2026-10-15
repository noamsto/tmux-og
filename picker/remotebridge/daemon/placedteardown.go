package daemon

import (
	"fmt"
	"os"
	"slices"
)

// serverReplaced is a placed runMirror's errServerReplaced: it carries the
// placeholder window the rebuild left in the host session, which the next run
// claims for its first remote window.
type serverReplaced struct{ placeholder string }

func (serverReplaced) Error() string { return errServerReplaced.Error() }

func (serverReplaced) Unwrap() error { return errServerReplaced }

// killOwnWindows closes every window of the host session stamped with this
// daemon's socket — its mirrors, a placeholder, a half-built window — and
// nothing else. Gated on the pin: a restarted local server reuses window ids,
// so an unguarded kill-window could land on an unrelated window. One call per
// window, since a failed command in a `;` chain skips the rest.
func killOwnWindows(cfg Config) {
	if !ownsLocalSession(cfg) {
		return
	}
	own, ok := ownWindows(cfg)
	if !ok {
		return
	}
	for _, w := range own {
		_ = cfg.LocalTmux("kill-window", "-t", w)
	}
}

// closeOwnWindows ends a placed mirror that will not come back: why, when set,
// reaches a client while one is still on the host session — no tombstone
// window is left there — and then every own window goes.
func closeOwnWindows(cfg Config, why string) {
	if why != "" {
		notifyLocal(cfg, why)
		fmt.Fprintf(os.Stderr, "daemon: %s\n", why)
	}
	killOwnWindows(cfg)
}

// rebuildPlaceholder is a placed rebuild's resetMirrorSession: it adds one
// `sleep 2147483647` window to the host session, stamped with this daemon's
// identity but no @bridge_win, then kills every other own window, and returns
// the new window's id for the next run to claim. The window comes first so a
// host session made only of our windows never empties mid-rebuild.
func rebuildPlaceholder(cfg Config) (string, bool) {
	if !ownsLocalSession(cfg) {
		return "", false
	}
	end := cfg.target() + ":{end}"
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", end, "--", "sleep", "2147483647"}
	for _, o := range mirrorIdentity(cfg) {
		args = append(args, ";", "set-option", "-w", "-t", end, o[0], o[1])
	}
	out, err := cfg.LocalTmuxOut(args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: new-window in %s: %v\n", cfg.target(), err)
		return "", false
	}
	id, err := parseWindowID(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return "", false
	}
	own, _ := ownWindows(cfg)
	for _, w := range slices.DeleteFunc(own, func(w string) bool { return w == id }) {
		_ = cfg.LocalTmux("kill-window", "-t", w)
	}
	return id, true
}
