package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// errServerReplaced is runMirror's return when its reattach ended on a
// different tmux server and teardown left the local session reset for a
// rebuild.
var errServerReplaced = errors.New("daemon: remote tmux server replaced")

// tornDown marks a runMirror error returned after its own teardown ran: the
// local session is already killed and the pidfile gone, so the loop must not
// tombstone or kill — a session og-remote-open recreated under the same
// name in that gap is never this daemon's to touch (#680).
type tornDown struct{ error }

func (e tornDown) Unwrap() error { return e.error }

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
//
// The local session is pinned first, before anything can rename or recreate
// it: the launcher has just created that exact name and spawned this daemon,
// so the name is the hand-off, and from here on only the pin is trusted.
func Run(cfg Config) error {
	if p, ok := pinLocalSession(cfg); ok {
		cfg.local = p
	}
	return runLoop(cfg, runMirror)
}

// runLoop is Run over an injectable run. A rebuild that fails before its
// mirror stood is not retried: the restore window already ran inside the
// replaced run, in reattach, while it still held its listener, and a retry
// here would dial again with no listener up — the gap in which og-remote-open
// reads this daemon as dead and recreates the session under its own.
func runLoop(cfg Config, run func(Config) error) error {
	for {
		err := run(cfg)
		if errors.Is(err, errNotOurs) {
			return nil
		}
		if errors.Is(err, errServerReplaced) {
			if stopped(cfg.Shutdown) {
				endReopen(cfg, "")
				return nil
			}
			fmt.Fprintf(os.Stderr, "daemon: %s: re-opening %s on the new tmux server\n", cfg.RemoteHost, cfg.RemoteSession)
			cfg.reopened = true
			continue
		}
		if !cfg.reopened || err == nil || errors.As(err, new(tornDown)) {
			return err
		}
		// Every other return precedes runMirror's teardown, so only the
		// placeholder the replaced run left is standing.
		if stopped(cfg.Shutdown) {
			endReopen(cfg, "")
			return nil
		}
		endReopen(cfg, reopenFailedText(cfg.RemoteHost, cfg.RemoteSession))
		return err
	}
}

// errNotOurs is a rebuild's return when the local session it would rebuild
// into is no longer the one this daemon was launched into; see runMirror.
var errNotOurs = errors.New("daemon: local session is no longer this mirror's")

// ownsLocalSession reports whether cfg.local still stands, logging when it
// does not. Every path that destroys or rebuilds the session after it could
// have gone asks this first, and then targets it by id alone.
func ownsLocalSession(cfg Config) bool {
	if cfg.local.check(cfg) == owned {
		return true
	}
	fmt.Fprintf(os.Stderr, "daemon: %s: local session is no longer this mirror's; leaving it alone\n", cfg.LocalSess)
	return false
}

// endReopen ends a rebuild that will not stand: with no tombstone text it is a
// stop and kills the session, which is what og-remote-detach expects;
// otherwise the session is tombstoned with that text. The pidfile goes either
// way, since the replaced run's teardown kept it for the rebuild — unless it
// no longer names this process, i.e. a new daemon has taken the pair over.
func endReopen(cfg Config, tombstone string) {
	pidFile := cfg.SockPath + ".pid"
	if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		os.Remove(pidFile)
	}
	if cfg.LocalSess == "" {
		return
	}
	if tombstone != "" {
		tombstoneMirror(cfg, tombstone)
		return
	}
	if ownsLocalSession(cfg) {
		cfg.LocalTmux("kill-session", "-t", cfg.local.id)
	}
}

// resetMirrorSession leaves the pinned mirror session holding one fresh window
// running argv, killing every other window, and returns the new window's id.
// The new window comes first so the session never empties (and a viewing
// client lands on it); the old ones go by id, never index, since
// renumber-windows is on.
//
// argv rather than a shell: `sleep 2147483647` as the rebuild's placeholder
// is no shell and nothing on screen, and setupWindow respawns it into the
// first renderer via firstMirrorWindow, as it does the launcher's loading
// pane. (BSD sleep accepts the large integer; `infinity` it rejects.)
func resetMirrorSession(cfg Config, argv ...string) (string, bool) {
	if !ownsLocalSession(cfg) {
		return "", false
	}
	old, err := cfg.LocalTmuxOut("list-windows", "-t", cfg.local.id, "-F", "#{window_id}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: list-windows %s: %v\n", cfg.LocalSess, err)
		return "", false
	}
	out, err := cfg.LocalTmuxOut(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}",
		"-a", "-t", cfg.local.id + ":{end}", "--"}, argv...)...)
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

// reopenFailedText is what a mirror whose rebuild never stood tells whoever
// finds it; sanitized like tombstoneText.
func reopenFailedText(host, session string) string {
	return fmt.Sprintf("%s: could not re-open %s on the restarted tmux server — this mirror is closed",
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

// tombstoneMirror resets the mirror session to one window showing text — why
// the mirror ended. It is persistent by construction: the ending usually
// happens with nobody looking — a probe or wake hours later — so a transient
// message would be lost, while the tombstone waits in the session for whoever
// comes back to it.
func tombstoneMirror(cfg Config, text string) {
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
	cfg.LocalTmux("set-option", "-u", "-t", cfg.local.id, "@bridge_sock")
	cfg.LocalTmux("set-option", "-u", "-t", cfg.local.id, "@bridge_state")
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
