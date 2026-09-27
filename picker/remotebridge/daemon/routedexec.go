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
//
// A %layout-change is a stream position too: the %output behind it was drawn
// after the remote reshape, so it must wait for settle's local select-layout.
// Reading stops once one is queued, and doesn't start while async holds one.
// Round-trips inside the same operation still read past it — the read-first
// transient this doesn't widen.
func routeWhile(lines <-chan controlmode.Line, router *Router, async *asyncQueue, st *stream, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	if st.parkedSeq() != 0 || async.holdsLayoutChange() {
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
			if l.Kind == controlmode.LayoutChange {
				lines = nil
			}
		}
	}
}

// routing returns a copy of c whose exec-backed hooks (LocalTmux,
// LocalTmuxOut, LocalArea, Reflow, LocalPanes) each run their call inside
// run, so Run's flowCfg can route %output around a window-set operation's
// execs via routeWhile. A nil hook stays nil. The copy is for main-loop
// operations on whole mirror windows only — never pane-shaping ones, whose
// execs must keep output held until the reshape lands; paster() restores the original hooks before it hands any of this to a goroutine.
func (c Config) routing(run func(func())) Config {
	r := c
	r.plain = &c
	if c.LocalTmux != nil {
		r.LocalTmux = func(args ...string) (err error) {
			run(func() { err = c.LocalTmux(args...) })
			return err
		}
	}
	if c.LocalTmuxOut != nil {
		r.LocalTmuxOut = func(args ...string) (out string, err error) {
			run(func() { out, err = c.LocalTmuxOut(args...) })
			return out, err
		}
	}
	if c.LocalArea != nil {
		r.LocalArea = func() (w, h int) {
			run(func() { w, h = c.LocalArea() })
			return w, h
		}
	}
	if c.Reflow != nil {
		r.Reflow = func() {
			run(func() { c.Reflow() })
		}
	}
	if c.LocalPanes != nil {
		r.LocalPanes = func() (m map[string]string) {
			run(func() { m = c.LocalPanes() })
			return m
		}
	}
	return r
}
