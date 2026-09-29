package daemon

import "slices"

// Echo suppression for pane focus.
//
// Local focus and remote focus each drive the other, so both directions need a
// brake or they ping-pong:
//
//   - local focus moves -> after-select-pane hook -> ctl focus -> the daemon
//     tells the remote to select that pane -> the remote reports
//     %window-pane-changed, which must not be mistaken for an external change
//     and re-driven back into local focus;
//   - the remote's focus moves for an external reason -> %window-pane-changed ->
//     the daemon moves local focus to match -> the local hook fires -> ctl focus,
//     which must not be sent back to the remote.
//
// Two beliefs, one per direction:
//
//   - remoteActivePane guards the local->remote direction. A focus request for a
//     pane the remote is already on sends nothing.
//   - localActive guards the remote->local direction. A report naming the pane
//     local focus already renders applies nothing.
//
// plus a FIFO of panes we have commanded the remote to focus but not yet seen
// reported back, which absorbs the lag when focus moves faster than the round
// trip (a quick A->B->C would otherwise see the stale A and C reports as
// external changes and walk local focus back through them).
//
// The FIFO is deliberately self-healing rather than exact. tmux does not report a
// select-pane that was already a no-op, so a commanded entry can never be
// reported at all; popping THROUGH a match flushes those, an unmatched report
// clears the FIFO outright (the remote has moved on, so outstanding intents are
// meaningless), and a hard cap bounds it regardless. The alternative — an exact
// counter — leaks one swallowed external report per unreported command, which is
// a lasting focus divergence rather than a transient one.

// maxCommandedFocus caps the outstanding-focus FIFO. Focus changes faster than
// this many round trips are a key-repeat storm, not a state worth tracking
// exactly; the cap keeps a lost report from growing the slice without bound.
const maxCommandedFocus = 8

// focusState is one mirror window's focus beliefs. All access is under
// ctlState.mu; the methods below assume it is held.
type focusState struct {
	localActive      string   // remote pane id the local active pane renders
	remoteActivePane string   // belief of the remote window's active pane ("" = unknown)
	commanded        []string // commanded remote panes awaiting their reports
	seq              int64    // highest focus request sequence accepted
}

// planFocusLocked decides what a local focus report should do. It returns the
// remote command to send, or ok=false to send nothing.
//
// seq orders requests: the local hook runs backgrounded (a synchronous
// run-shell would put a socket round-trip on tmux's command queue for every
// focus change), so requests can arrive out of order. A request older than one
// already accepted is dropped rather than allowed to install a stale belief.
func (c *ctlState) planFocusLocked(remoteWin, pane string, seq int64) (string, bool) {
	f := c.focus[remoteWin]
	if f == nil {
		return "", false
	}
	if seq <= f.seq {
		return "", false
	}
	f.seq = seq
	f.localActive = pane
	if f.remoteActivePane == pane {
		return "", false // the remote is already there
	}
	f.remoteActivePane = pane
	if len(f.commanded) >= maxCommandedFocus {
		f.commanded = f.commanded[1:]
	}
	f.commanded = append(f.commanded, pane)
	return "select-pane -t " + pane, true
}

// applyRemoteFocus decides what a %window-pane-changed report should do. It
// returns the remote pane local focus must move to, or ok=false for nothing.
//
// A report is authoritative about the remote even when no local action follows,
// so remoteActivePane is updated on every path.
func (c *ctlState) applyRemoteFocus(remoteWin, pane string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	f := c.focus[remoteWin]
	if f == nil {
		return "", false // window outside the registry: never act on it
	}
	f.remoteActivePane = pane

	if i := indexOf(f.commanded, pane); i >= 0 {
		// Our own echo. Pop through it, discarding earlier entries that will
		// never be reported (a commanded no-op emits nothing).
		f.commanded = slices.Delete(f.commanded, 0, i+1)
		return "", false
	}
	// An unmatched report means the remote moved for its own reasons, so any
	// outstanding intents are stale.
	f.commanded = nil
	if pane == f.localActive {
		return "", false // local focus is already there
	}
	f.localActive = pane
	return pane, true
}

// noteLocalFocus records that local focus now renders pane, without sending
// anything. The reconcile uses it after moving local focus itself, so the
// hook's resulting report is recognised as an echo.
func (c *ctlState) noteLocalFocus(remoteWin, pane string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f := c.focus[remoteWin]; f != nil {
		f.localActive = pane
		f.remoteActivePane = pane
	}
}
