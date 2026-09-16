package daemon

import (
	"testing"
	"time"
)

// isArmed is a test-only accessor for the state wake()/fired() manage —
// carouselProbe.pendingCount()'s precedent (carouselprobe_test.go): nothing
// in production asks whether the nudge is armed.
func (d *deathNudge) isArmed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.armed
}

// A single wake() arms the nudge.
func TestDeathNudgeWakeArms(t *testing.T) {
	d := newDeathNudge()
	d.wake()
	if !d.isArmed() {
		t.Error("armed = false after a single wake(), want true")
	}
}

// Several deaths in one beat (e.g. every pane in a killed window) must
// collapse to one timer fire, not keep pushing the deadline out — wake()'s
// own "if !armed" guard is the whole mechanism, so this pins that a second
// (and third, ...) call while still armed changes nothing observable, with
// no fake-timer seam needed.
func TestDeathNudgeRepeatedWakeIsANoOp(t *testing.T) {
	d := newDeathNudge()
	for i := 0; i < 5; i++ {
		d.wake()
		if !d.isArmed() {
			t.Fatalf("armed = false after wake() call #%d, want true", i+1)
		}
	}
}

// fired() clears armed so a later death gets its own timer instead of
// silently riding a nudge that already fired.
func TestDeathNudgeFiredClearsArmedAndAllowsRearm(t *testing.T) {
	d := newDeathNudge()
	d.wake()
	d.fired()
	if d.isArmed() {
		t.Error("armed = true after fired(), want false")
	}
	d.wake()
	if !d.isArmed() {
		t.Error("armed = false after wake() following fired(), want true — a later death must re-arm")
	}
}

// The one integration point between the state-machine tests above and the
// actual time.Timer, generously margined so scheduler jitter can't flake it —
// still catches a Reset that fires immediately (wrong duration) or never
// fires at all (Reset not called, or called on the wrong timer).
func TestDeathNudgeTimerFiresAfterDeathSweepDelay(t *testing.T) {
	d := newDeathNudge()
	d.wake()

	select {
	case <-d.C():
		t.Fatal("C() fired before deathSweepDelay/2 had elapsed")
	case <-time.After(deathSweepDelay / 2):
	}

	select {
	case <-d.C():
	case <-time.After(deathSweepDelay * 3):
		t.Fatal("C() did not fire within deathSweepDelay*3 of wake()")
	}
}
