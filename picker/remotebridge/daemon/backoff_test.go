package daemon

import (
	"testing"
	"time"
)

// noJitter drives Backoff.delay deterministically: with Jitter returning 1,
// the jittered delay equals the unjittered one, so the doubling/clamping math
// is asserted exactly rather than as a range.
func noJitter() float64 { return 1 }

func TestBackoffDelayGrowsAndClampsAtCeiling(t *testing.T) {
	b := Backoff{
		Base:    100 * time.Millisecond,
		Ceiling: 500 * time.Millisecond,
		Now:     time.Now,
		Jitter:  noJitter,
	}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond}, // Base, undoubled
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 500 * time.Millisecond}, // would double to 800ms; clamps at Ceiling
		{5, 500 * time.Millisecond}, // stays clamped, doesn't keep growing
		{50, 500 * time.Millisecond},
	}
	for _, c := range cases {
		if got := b.delay(c.attempt); got != c.want {
			t.Errorf("delay(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

func TestBackoffElapsedBoundEndsScheduleWithAttemptsRemaining(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	b := Backoff{
		Base:        10 * time.Millisecond,
		Ceiling:     time.Second,
		MaxAttempts: 1000, // generous — the elapsed bound must end this first
		MaxElapsed:  time.Minute,
		Now:         func() time.Time { return now },
		Jitter:      noJitter,
	}

	if _, ok := b.Next(1, start); !ok {
		t.Fatal("Next(1) at zero elapsed should still be allowed")
	}

	now = start.Add(time.Minute) // exactly at MaxElapsed
	if _, ok := b.Next(2, start); ok {
		t.Error("Next should end the schedule once elapsed reaches MaxElapsed, even with attempts left")
	}
}

// A late attempt's delay is clamped to the budget left, so the schedule ends
// at MaxElapsed plus one attempt instead of one oversized sleep past it.
func TestBackoffNextNeverWaitsPastMaxElapsed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	b := Backoff{
		Base:        500 * time.Millisecond,
		Ceiling:     30 * time.Second,
		MaxAttempts: 40,
		MaxElapsed:  2 * time.Second,
		Now:         func() time.Time { return now },
		Jitter:      noJitter,
	}

	if d, ok := b.Next(1, start); !ok || d != 500*time.Millisecond {
		t.Fatalf("Next(1, start) = %s, %v, want 500ms, true", d, ok)
	}

	now = start.Add(1900 * time.Millisecond)
	d, ok := b.Next(6, start) // unclamped delay(6) is 16s
	if !ok {
		t.Fatal("Next(6) with 100ms of budget left should still be allowed")
	}
	if d != 100*time.Millisecond {
		t.Errorf("Next(6) = %s, want 100ms (the budget remaining before MaxElapsed)", d)
	}
}

func TestBackoffAttemptBoundEndsScheduleWithElapsedRemaining(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := Backoff{
		Base:        10 * time.Millisecond,
		Ceiling:     time.Second,
		MaxAttempts: 3,
		MaxElapsed:  time.Hour, // generous — the attempt bound must end this first
		Now:         func() time.Time { return start },
		Jitter:      noJitter,
	}

	if _, ok := b.Next(3, start); !ok {
		t.Fatal("Next(3) should still be allowed at MaxAttempts")
	}
	if _, ok := b.Next(4, start); ok {
		t.Error("Next should end the schedule once attempt exceeds MaxAttempts, even with elapsed time left")
	}
}

// TestWaitClosedStopChannelReturnsImmediately proves cancellation wins the
// select rather than the caller waiting out the delay — an already-closed
// channel is ready instantly, so a correct Wait returns long before the
// (deliberately large) delay would have elapsed.
func TestWaitClosedStopChannelReturnsImmediately(t *testing.T) {
	stop := make(chan struct{})
	close(stop)

	start := time.Now()
	cancelled := Wait(time.Hour, stop)
	elapsed := time.Since(start)

	if !cancelled {
		t.Error("Wait against a closed stop channel should report cancelled")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Wait blocked for %s against an already-closed stop channel", elapsed)
	}
}

// TestWaitNilStopChannelDoesNotCancel exercises a nil Config.Shutdown (the
// single-shot / no-cancellation case): a nil channel is never ready, so Wait
// must run out the real delay rather than returning early.
func TestWaitNilStopChannelDoesNotCancel(t *testing.T) {
	if cancelled := Wait(10*time.Millisecond, nil); cancelled {
		t.Error("Wait against a nil stop channel should never report cancelled")
	}
}
