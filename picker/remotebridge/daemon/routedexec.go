package daemon

import "github.com/noamsto/tmux-og/picker/remotebridge/controlmode"

// routeWhile runs fn on a helper goroutine and keeps consuming the control
// stream until it returns, so a slow local exec no longer holds the remote's
// %output in the pump (#808). Only the exec moves: every stream, router and
// async-queue access stays on the caller, which must be the main-loop
// goroutine — a second concurrent reader would reorder lines.
//
// Lines are disposed of as waitHellos does: each is claimed so ordinals stay
// exact, %output is routed, other notifications are queued for settle, and a
// reply nobody awaits is dropped.
//
// A reply at or below awaitHigh may belong to a batch whose lazy next() hasn't
// run yet (PaneSeeds). It is parked and reading stops for the rest of the
// exec: routing the output behind it would paint a pane before that batch
// has seeded it. For the same reason nothing is read while the slot is already
// occupied. A closed stream stops the reads and stays closed, so the next
// reader takes the usual EOF path.
func routeWhile(lines <-chan controlmode.Line, router *Router, async *asyncQueue, st *stream, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	if st.parkedSeq() != 0 {
		lines = nil
	}
	for {
		select {
		case <-done:
			return
		case l, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			seq := claimSeq(l, st)
			if st.awaited(seq) {
				st.park(seq, l)
				lines = nil
				continue
			}
			handleAsideLine(l, router, async)
		}
	}
}
