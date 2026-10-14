package daemon

import (
	"fmt"
	"sync"
)

// ConvergeCmd returns the control-mode command that caps one remote window at
// WxH. The per-window form of refresh-client -C, not the whole-client one:
// tmux applies it as a clamp *after* window-size's own calculation (resize.c
// clients_calculate_size), whereas the whole-client form loses that
// calculation outright — under window-size latest every client but w->latest
// is skipped.
//
// The clamp only reaches a window that participates in sizing at all, so the
// per-window form holds only alongside AggressiveResizeOffCmd below (#478).
func ConvergeCmd(remoteID string, w, h int) string {
	return fmt.Sprintf("refresh-client -C %s:%dx%d", remoteID, w, h)
}

// AggressiveResizeOffCmd returns the control-mode command that opts one remote
// window out of aggressive-resize, which every remote window inherits from
// tmux-og's own global config.
//
// With that option on, tmux sizes a window only from clients whose session
// currently has it selected (resize.c, clients_calculate_size_skip_client's
// `current` branch). The bridge holds one control client on the mirrored
// session, so it is "on" one window at a time: for every other mirrored window
// no client contributes a size, ConvergeCmd's cap is accepted and then
// discarded during recalculation, and the window keeps whatever size it last
// held — or default-size if it never held one (#478).
//
// A relaxation, not a pin: window-size stays latest, so a human client
// attached to the remote still clamps the window down.
func AggressiveResizeOffCmd(remoteID string) string {
	return fmt.Sprintf("set-option -w -t %s aggressive-resize off", remoteID)
}

// clientSizeKey is the converger slot for the whole-client size below. Remote
// window ids are all @N, so this cannot collide with one.
const clientSizeKey = "client"

// ClientSizeCmd returns the control-mode command that gives the bridge's own
// control client a size.
//
// Without it the client has none: tmux defaults a control client to 80 columns
// and no rows, and under `window-size latest` a window CREATED on the remote is
// born at that size (measured on a live bridge: 80x23, converging to 250x63
// only 230ms later, once ConvergeCmd's per-window cap lands). Anything the
// remote starts in that gap reads the wrong terminal size.
//
// This does not replace the per-window caps — it cannot, for the reason
// ConvergeCmd documents. It only gives a window a sane size to be BORN at; the
// cap still owns the size it settles to.
func ClientSizeCmd(w, h int) string {
	return fmt.Sprintf("refresh-client -C %dx%d", w, h)
}

// converger records the size last asserted for each remote window so the
// resize poll only re-sends on change. Written from the window setup path and
// from the resize watcher goroutine, hence the mutex.
type converger struct {
	mu   sync.Mutex
	last map[string][2]int
}

func newConverger() *converger { return &converger{last: map[string][2]int{}} }

// need reports whether remoteID still has to be told about WxH, recording the
// size as asserted when it does. clientSizeKey tracks the whole-client size in
// the same map, so it is re-sent on a change and skipped otherwise.
func (c *converger) need(remoteID string, w, h int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last[remoteID] == [2]int{w, h} {
		return false
	}
	c.last[remoteID] = [2]int{w, h}
	return true
}

// unrecord drops the size recorded for remoteID when the write meant to carry
// it never happened, so the next tick retries instead of treating the remote as
// already told. Only if the slot still holds that size: the other writer may
// have asserted a different one in between, and that assertion is the current
// truth.
//
// What this cannot close: a written command is still not an applied one. A cap
// that draws %error because the window vanished between reg.remoteIDs() and the
// send latches as asserted — acceptable only because that path is followed by
// closeWindow's cv.forget.
func (c *converger) unrecord(remoteID string, w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last[remoteID] == [2]int{w, h} {
		delete(c.last, remoteID)
	}
}

// reset drops everything this converger believed the remote had been told,
// because a fresh control client has been told nothing: not the client size,
// not one per-window cap. Called on re-attach (#482).
//
// It mutates in place rather than being replaced: watchLocalClient is started once,
// off the main-loop goroutine, and holds the *converger — a fresh one would
// leave the watcher writing to an object nothing reads, silently.
func (c *converger) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = map[string][2]int{}
}

func (c *converger) forget(remoteID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.last, remoteID)
}

// asserted returns the size last recorded for key.
func (c *converger) asserted(key string) (w, h int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sz, ok := c.last[key]
	return sz[0], sz[1], ok
}

// recalcCmd returns the whole-client size assert that makes tmux recalculate
// every window right now, or false when nothing has been recorded yet. A
// refresh-client -C is the only trigger: server_client_set_flags (an
// ignore-size flip) does not recalculate, and a lost client recalculates only
// the current window of an attached session (resize.c recalculate_size, now=0).
func recalcCmd(cv *converger) (string, bool) {
	w, h, ok := cv.asserted(clientSizeKey)
	if !ok {
		return "", false
	}
	return ClientSizeCmd(w, h), true
}
