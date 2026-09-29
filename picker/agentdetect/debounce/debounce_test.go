package debounce

import (
	"slices"
	"testing"
	"time"
)

func TestDueOnlyAfterQuietWindow(t *testing.T) {
	base := time.Unix(1000, 0)
	d := New(80*time.Millisecond, time.Hour) // ceiling out of reach
	d.Mark(base)
	if d.Due(base.Add(50 * time.Millisecond)) {
		t.Fatal("should not be due at 50ms")
	}
	if !d.Due(base.Add(80 * time.Millisecond)) {
		t.Fatal("should be due at 80ms")
	}
	if d.Due(base.Add(120 * time.Millisecond)) {
		t.Fatal("should not re-fire without a new Mark")
	}
	d.Mark(base.Add(200 * time.Millisecond))
	if !d.Due(base.Add(300 * time.Millisecond)) {
		t.Fatal("should be due again after a new Mark + window")
	}
}

// A TUI painting faster than the quiet window (codex: ~30 frames/sec) never
// stops marking, so the ceiling is the only thing that ever samples it (#238).
func TestCeilingFiresWhenQuietWindowNeverSettles(t *testing.T) {
	base := time.Unix(1000, 0)
	d := New(80*time.Millisecond, 500*time.Millisecond)

	var fireAt []int
	for ms := 0; ms <= 1200; ms += 10 {
		now := base.Add(time.Duration(ms) * time.Millisecond)
		d.Mark(now) // a frame lands every step, so the quiet window never closes
		if d.Due(now) {
			fireAt = append(fireAt, ms)
		}
	}
	// Paced by the ceiling alone: first at 500, then 500 past the mark that
	// followed it. Without the ceiling this list is empty.
	want := []int{500, 1010}
	if !slices.Equal(fireAt, want) {
		t.Fatalf("ceiling samples: got %v, want %v", fireAt, want)
	}
}

// The ceiling paces samples from the first mark after the last one, so a pane
// that idles for hours and then paints once is not instantly due.
func TestCeilingMeasuredFromFirstMarkSinceLastFire(t *testing.T) {
	base := time.Unix(1000, 0)
	d := New(80*time.Millisecond, 500*time.Millisecond)

	d.Mark(base)
	if !d.Due(base.Add(80 * time.Millisecond)) {
		t.Fatal("quiet window should fire")
	}
	d.Mark(base.Add(time.Hour))
	if d.Due(base.Add(time.Hour + 10*time.Millisecond)) {
		t.Fatal("ceiling should be measured from the new mark, not the epoch")
	}
}

// The watcher stops its sample ticker whenever nothing is pending, so Pending
// must stay true from a mark until the sample that consumes it.
func TestPendingFromMarkUntilSampled(t *testing.T) {
	base := time.Unix(1000, 0)
	d := New(80*time.Millisecond, time.Hour)
	if d.Pending() {
		t.Fatal("fresh debouncer should have nothing pending")
	}
	d.Mark(base)
	if d.Due(base.Add(40*time.Millisecond)) || !d.Pending() {
		t.Fatal("mark should stay pending until the quiet window closes")
	}
	if !d.Due(base.Add(80*time.Millisecond)) || d.Pending() {
		t.Fatal("sampling should clear pending")
	}
}
