package daemon

import (
	"math/rand"
	"time"
)

// Backoff is a pure, injectable retry schedule for the reconnect loop:
// exponential delay from Base to Ceiling, full-jittered, bounded by both a
// max attempt count and a max total elapsed time. Both bounds apply — a fixed
// attempt count times a growing delay is an unintuitive wall-clock bound on
// its own, and an elapsed-only bound has no backstop if Now ever misbehaves
// (a clock step, a stub clock in a test). Now and Jitter are injected so
// Next never reads real time or randomness itself, which is what keeps the
// reconnect loop's tests from ever sleeping for real.
type Backoff struct {
	Base        time.Duration
	Ceiling     time.Duration
	MaxAttempts int
	MaxElapsed  time.Duration
	Now         func() time.Time
	Jitter      func() float64 // [0, 1); rand.Float64 in production
}

// DefaultBackoff sizes the schedule to outlast an ordinary laptop lid-close,
// wifi/LTE hop, or VPN reconnect — all comfortably past the 60s the existing
// ssh keepalives (ServerAliveInterval x ServerAliveCountMax) take to even
// notice the drop — while still stopping: unbounded retrying would leave a
// daemon dialing a dead host long after the user has moved on, so exhaustion
// parks the mirror and waits for the user instead. At this Ceiling
// MaxElapsed is the bound that governs; MaxAttempts only ever fires as the
// backstop the type documents.
func DefaultBackoff(now func() time.Time) Backoff {
	return Backoff{
		Base:        500 * time.Millisecond,
		Ceiling:     30 * time.Second,
		MaxAttempts: 40,
		MaxElapsed:  10 * time.Minute,
		Now:         now,
		Jitter:      rand.Float64,
	}
}

// WakeBackoff sizes the one short cycle a wake from parked buys. A user who
// pressed a key or focused the mirror is watching, so this schedule spends at
// most 30s of dialing before re-parking rather than the full DefaultBackoff
// budget — the outage that exhausted the retry schedule in the first place
// hasn't necessarily cleared just because the user came back.
func WakeBackoff(now func() time.Time) Backoff {
	return Backoff{
		Base:        500 * time.Millisecond,
		Ceiling:     5 * time.Second,
		MaxAttempts: 10,
		MaxElapsed:  30 * time.Second,
		Now:         now,
		Jitter:      rand.Float64,
	}
}

// Next returns the delay before retry attempt (1-indexed: the delay before
// the first retry is Next(1, start)) and whether the caller should even try.
// Once either bound is exhausted it returns false and the delay is
// meaningless — the caller's schedule is over, not merely long.
func (b Backoff) Next(attempt int, start time.Time) (time.Duration, bool) {
	if b.MaxAttempts > 0 && attempt > b.MaxAttempts {
		return 0, false
	}
	if b.MaxElapsed > 0 && b.Now().Sub(start) >= b.MaxElapsed {
		return 0, false
	}
	return b.delay(attempt), true
}

// delay computes the jittered exponential wait for attempt, doubling from
// Base and clamping at Ceiling. The loop form (rather than Base<<attempt)
// sidesteps overflow for a misconfigured schedule with a large MaxAttempts —
// it stops doubling the instant it would exceed Ceiling anyway. Full jitter
// (uniform on [0, d), not d plus jitter) so a fleet of daemons dropped by the
// same VPN blip doesn't retry in lockstep.
func (b Backoff) delay(attempt int) time.Duration {
	d := b.Base
	for i := 1; i < attempt && d < b.Ceiling; i++ {
		d *= 2
	}
	if d > b.Ceiling {
		d = b.Ceiling
	}
	return time.Duration(b.Jitter() * float64(d))
}

// Wait sleeps for d or until stop is closed, whichever comes first, and
// reports whether it was cancelled. A nil stop channel is never ready to
// receive, so it never cancels — the caller with no shutdown channel (a
// single-shot Config with no Dial) just sleeps the full delay.
func Wait(d time.Duration, stop <-chan struct{}) (cancelled bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-stop:
		return true
	}
}
