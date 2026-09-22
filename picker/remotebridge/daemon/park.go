package daemon

import (
	"strings"
	"sync/atomic"
	"time"
)

// bridgeStateParked is the @bridge_state value picker/statusline renders the
// "offline — press a key" badge for: the reconnect budget is exhausted and
// the mirror is waiting on the user rather than dialing. bridgeStateDisconnected
// (conn.go) still covers a retry cycle in progress, initial or waked.
const bridgeStateParked = "parked"

// parkFocusInterval is how often the parked wait re-checks the resize-nudge
// mtime for a focus edge. A stat, not a fork, so this can be cheap enough to
// run for as long as the mirror stays parked.
const parkFocusInterval = time.Second

// parkWaker carries a keypress from pumpInput's goroutine to the parked
// wait's select. It is armed only while parked, so poke costs one atomic load
// on the live hot path and nothing more.
type parkWaker struct {
	armed atomic.Bool
	ch    chan struct{}
}

// newParkWaker builds the seam with its channel unarmed. 1-buffered so one
// poke while parked is never lost to the select not yet being ready for it,
// and further pokes before that one is read coalesce into it rather than
// blocking pumpInput's goroutine.
func newParkWaker() *parkWaker {
	return &parkWaker{ch: make(chan struct{}, 1)}
}

// C is the arm the parked wait selects on.
func (w *parkWaker) C() <-chan struct{} { return w.ch }

// poke wakes a parked wait. Called for every FrameInput frame, live or not,
// so the armed check has to come first: a no-op send would still cost the
// channel op on every keypress of every ordinary session.
func (w *parkWaker) poke() {
	if !w.armed.Load() {
		return
	}
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// arm is called on park entry. Drains any poke a prior park left queued
// before arming — a keypress that landed while disarmed answered nothing and
// must not be replayed as this park's wake.
func (w *parkWaker) arm() {
	select {
	case <-w.ch:
	default:
	}
	w.armed.Store(true)
}

// disarm is called on park exit; a poke that slipped in before it is drained
// by the next arm.
func (w *parkWaker) disarm() { w.armed.Store(false) }

// focusEdge detects the moment a client starts looking at the mirror while
// parked. It fires only on the false->true transition, not on "still
// viewing": a user already looking at the mirror when it parks is not woken
// again by every reflow-driven touch of the resize-nudge file — they press a
// key instead.
type focusEdge struct {
	// nudged reads the resize-nudge mtime; the session hooks behind it already
	// fire on client-session-changed, which is the focus change.
	nudged  func() (time.Time, bool)
	viewing func() bool
	last    time.Time
	was     bool
}

// reset records the mtime and viewing state at park entry, so poll's first
// call measures against ground truth rather than a zero value.
func (f *focusEdge) reset() {
	if mtime, ok := f.nudged(); ok {
		f.last = mtime
	}
	f.was = f.viewing()
}

// poll reports whether this call caught a wake-worthy focus edge. False on
// every call where the nudge mtime hasn't advanced past last — viewing is
// re-read only once there is a touch to explain, since that read forks a
// local tmux.
func (f *focusEdge) poll() bool {
	mtime, ok := f.nudged()
	if !ok || !mtime.After(f.last) {
		return false
	}
	f.last = mtime
	viewing := f.viewing()
	wake := viewing && !f.was
	f.was = viewing
	return wake
}

// localViewing reports whether any local client is attached to cfg.LocalSess.
// A failed read answers false, which can only miss a focus wake, never cause
// one.
func localViewing(cfg Config) bool {
	if cfg.LocalTmuxOut == nil || cfg.LocalSess == "" {
		return false
	}
	out, err := cfg.LocalTmuxOut("list-clients", "-F", "#{client_session}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == cfg.LocalSess {
			return true
		}
	}
	return false
}

// parkDimStyle paints a parked mirror window's panes as tmux-og already
// paints an inactive one: default-foreground text in the theme's overlay
// colour, on the mantle background.
const parkDimStyle = "fg=#{@thm_overlay_0},bg=#{@thm_mantle}"

// parkDimOptions are stamped together because a per-window value replaces
// the inherited global one rather than merging with it — dimming only
// window-style would leave the active pane at full brightness.
var parkDimOptions = [...]string{"window-style", "window-active-style"}

// dimMirror stamps parkDimStyle on both style options of every window in the
// registry. No-op on a Config with no local session, matching setBridgeState.
func dimMirror(cfg Config, reg *registry) {
	if cfg.LocalSess == "" {
		return
	}
	for _, mw := range reg.all() {
		for _, opt := range parkDimOptions {
			cfg.LocalTmux("set-option", "-w", "-t", mw.localWin, opt, parkDimStyle)
		}
	}
}

// undimMirror unsets both style options on every window in the registry, so
// they inherit the global theme value again. A window the repair created was
// never dimmed; unsetting it is harmless.
func undimMirror(cfg Config, reg *registry) {
	if cfg.LocalSess == "" {
		return
	}
	for _, mw := range reg.all() {
		for _, opt := range parkDimOptions {
			cfg.LocalTmux("set-option", "-w", "-u", "-t", mw.localWin, opt)
		}
	}
}
