package daemon

import (
	"sync"
	"time"
)

// deathSweepDelay is the wait between a renderer connection closing and the
// forced sweep that looks for its corpse, sized (like carouselProbeInterval)
// to give tmux's own pane_dead update time to land before the daemon's own
// re-list, since pumpInput's socket EOF has no ordering guarantee against it.
const deathSweepDelay = 250 * time.Millisecond

// deathNudge carries no payload because mirrorPaneRows re-derives ground
// truth on every sweep — the seam only has to wake runConn's select sooner
// than mainLoopTickInterval, never say which window died. Unlike
// viewReplacer, it needs no cancel(): a stale pending fire surviving a
// reconnect costs one extra forced sweep, not a wrong repair.
//
// One race wake/fired has that carouselProbe.arm/rearm avoids: a wake()
// landing after the timer has fired but before runConn's case reaches
// fired() sees armed == true and does not re-arm, so that death rides the
// already-firing pass with less than deathSweepDelay of grace rather than
// getting its own — worst case it falls back to the 5s backstop like any
// other missed race, which the design already accepts in general.
type deathNudge struct {
	mu    sync.Mutex
	armed bool
	timer *time.Timer
}

// newDeathNudge builds the seam with its timer stopped. Session-lifetime,
// like carouselProbe: the timer is created once per Run and only ever Reset,
// since its channel is what runConn selects on and a per-death channel would
// be a handle the loop cannot see.
func newDeathNudge() *deathNudge {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &deathNudge{timer: t}
}

// C is the arm runConn selects on.
func (d *deathNudge) C() <-chan time.Time { return d.timer.C }

// wake is called from pumpInput's goroutine on a renderer connection closing
// — crash, clean exit, or the daemon's own deliberate close during reconcile,
// all harmless to wake on since the sweep re-checks rather than trusting the
// call. Arms the timer only if it isn't already armed, so a steady trickle of
// deaths cannot keep pushing the fire time out — the same "don't let a second
// press push the first one's read further out" reasoning carouselProbe.arm
// documents for itself.
func (d *deathNudge) wake() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		d.armed = true
		d.timer.Reset(deathSweepDelay)
	}
}

// fired clears the armed flag, called by runConn's case body once the
// timer's case has actually been taken (mirrors carouselProbe.due/rearm
// clearing pending state before the next arm is honoured).
func (d *deathNudge) fired() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = false
}
