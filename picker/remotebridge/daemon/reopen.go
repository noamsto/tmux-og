package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
)

// errServerReplaced is runMirror's return when its reattach ended on a
// different tmux server and teardown left the local session reset for a
// rebuild.
var errServerReplaced = errors.New("daemon: remote tmux server replaced")

// Run mirrors every window of the bridged remote session, each into its own
// local window, over a -CC connection, until %exit, an emptied mirror, the
// local mirror session going away, or a stop — re-opening the mirror when a
// reconnect lands on a different tmux server. A drop the reconnect budget
// cannot outlast parks the mirror instead of ending it; see reattach.
//
// Two lifetimes live in runMirror and they only look like one (#482). The
// session lifetime — listener, pidfile, registry, renderer panes and their
// sinks, the resize watcher, the agent shipper — is created once and destroyed
// only by teardown, which runs exactly once per return path. The connection
// lifetime — transport, pump, stream, round-tripper, and every client-scoped
// value the remote holds for this control client — is rebuilt on every attach.
//
// A reattach that meets a different tmux server tears the old mirror down —
// never splicing: the old windows are gone before the new connection exists —
// and Run builds a fresh mirror onto the same-named session through the very
// path a first open takes, in the same local session so a viewing client
// stays put. The second dial is deliberate: reusing the connection that
// answered the mismatch would hand a verified-foreign stream to a half-built
// mirror.
func Run(cfg Config) error {
	for {
		err := runMirror(cfg)
		if errors.Is(err, errServerReplaced) {
			if stopped(cfg.Shutdown) {
				endReopen(cfg, false)
				return nil
			}
			fmt.Fprintf(os.Stderr, "daemon: %s: re-opening %s on the new tmux server\n", cfg.RemoteHost, cfg.RemoteSession)
			cfg.reopened = true
			continue
		}
		if cfg.reopened && err != nil {
			// A rebuild that never stood: runMirror's early returns (dial,
			// identity timeout, list-windows, no windows) precede its
			// teardown, so only the placeholder is left.
			endReopen(cfg, !stopped(cfg.Shutdown))
		}
		return err
	}
}

// endReopen ends a rebuild that will not stand: the session is killed on a
// stop, which is what og-remote-detach expects, and tombstoned otherwise. The
// pidfile goes too, since the replaced run's teardown kept it for the rebuild.
func endReopen(cfg Config, tombstone bool) {
	os.Remove(cfg.SockPath + ".pid")
	if cfg.LocalSess == "" {
		return
	}
	if tombstone {
		tombstoneMirror(cfg)
		return
	}
	cfg.LocalTmux("kill-session", "-t", cfg.LocalSess)
}

// resetMirrorSession leaves cfg.LocalSess holding one fresh window running
// argv, killing every other window, and returns the new window's id. The new
// window comes first so the session never empties (and a viewing client lands
// on it); the old ones go by id, never index, since renumber-windows is on.
//
// argv rather than a shell: `sleep 2147483647` as the rebuild's placeholder
// is no shell and nothing on screen, and setupWindow respawns it into the
// first renderer via firstMirrorWindow, as it does the launcher's loading
// pane. (BSD sleep accepts the large integer; `infinity` it rejects.)
func resetMirrorSession(cfg Config, argv ...string) (string, bool) {
	old, err := cfg.LocalTmuxOut("list-windows", "-t", cfg.LocalSess, "-F", "#{window_id}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: list-windows %s: %v\n", cfg.LocalSess, err)
		return "", false
	}
	out, err := cfg.LocalTmuxOut(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}",
		"-a", "-t", cfg.LocalSess + ":{end}", "--"}, argv...)...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: new-window in %s: %v\n", cfg.LocalSess, err)
		return "", false
	}
	id, err := parseWindowID(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return "", false
	}
	for _, w := range strings.Split(old, "\n") {
		if w = strings.TrimSpace(w); w != "" && w != id {
			cfg.LocalTmux("kill-window", "-t", w)
		}
	}
	return id, true
}

// tombstoneScript prints its first argument and waits for Enter. The text is
// an argv element, never interpolated into the script.
const tombstoneScript = `printf '%s\n\n%s\n' "$1" 'press Enter to close'; read -r _`

// tombstoneText is what a gone mirror tells whoever finds it. host and session
// reach a local pty, so control runes are dropped; tmux's refusal text is
// never shown there — it is only logged.
func tombstoneText(host, session string) string {
	return fmt.Sprintf("%s: session %s no longer exists (the tmux server was restarted or the session was closed) — this mirror is closed",
		printable(host), printable(session))
}

// printable drops every control rune from s.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// tombstoneMirror resets the mirror session to one window explaining that the
// remote session is gone. It is persistent by construction: the ending
// usually happens with nobody looking — a probe or wake hours later — so a
// transient message would be lost, while the tombstone waits in the session
// for whoever comes back to it.
func tombstoneMirror(cfg Config) {
	text := tombstoneText(cfg.RemoteHost, cfg.RemoteSession)
	id, ok := resetMirrorSession(cfg, "sh", "-c", tombstoneScript, "sh", text)
	if !ok {
		return
	}
	// Enter closes the window, and the session with it.
	cfg.LocalTmux("set-option", "-w", "-t", id, "remain-on-exit", "off")
	// Nothing answers the socket and nothing is dialling. @bridge_host and
	// @bridge_session stay, so og-remote-open's pair lookup recognises this as
	// the pair's mirror and replaces it on the next open instead of walking to
	// a -remote suffix.
	cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, "@bridge_sock")
	cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, "@bridge_state")
	fmt.Fprintf(os.Stderr, "daemon: %s\n", text)
}

// reopenNoticeRecheck is the backstop interval for showReopenNotice.
const reopenNoticeRecheck = 15 * time.Second

// showReopenNotice tells the first viewer of a re-opened mirror that it now
// mirrors a fresh server. The re-open usually happens unobserved (a probe, a
// wake cycle's restore), so the notice waits for a viewer rather than firing
// into an empty session: the park's focus edge catches a client arriving, and
// the recheck is the backstop for an attach that touches no resize-nudge hook.
// stop bounds it by the mirror's life.
func showReopenNotice(cfg Config, nudged func() (time.Time, bool), stop <-chan struct{}) {
	msg := fmt.Sprintf("%s: tmux server restarted — now mirroring a fresh %s", printable(cfg.RemoteHost), printable(cfg.RemoteSession))
	fmt.Fprintf(os.Stderr, "daemon: %s\n", msg)
	if localViewing(cfg) && notifyLocal(cfg, msg) {
		return
	}
	focus := focusEdge{nudged: nudged, viewing: func() bool { return localViewing(cfg) }}
	focus.reset()
	ft := time.NewTicker(parkFocusInterval)
	defer ft.Stop()
	recheck := time.NewTicker(reopenNoticeRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ft.C:
			if focus.poll() && notifyLocal(cfg, msg) {
				return
			}
		case <-recheck.C:
			if localViewing(cfg) && notifyLocal(cfg, msg) {
				return
			}
		}
	}
}
