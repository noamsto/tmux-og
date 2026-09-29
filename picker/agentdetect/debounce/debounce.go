package debounce

import "time"

// Debouncer decides when the emulated screen is worth sampling. The quiet
// window is the fast path: an agent that repaints only on change settles within
// a frame or two of going still. The ceiling is the backstop — a continuously
// animating TUI (codex paints ~30 frames/sec) never goes quiet for a whole
// window, so the quiet path alone starves and the pane is never sampled at all
// (#238).
type Debouncer struct {
	window   time.Duration
	ceiling  time.Duration
	lastMark time.Time
	dirtyAt  time.Time
	fired    bool
	dirty    bool
}

func New(window, ceiling time.Duration) *Debouncer {
	return &Debouncer{window: window, ceiling: ceiling}
}

func (d *Debouncer) Mark(t time.Time) {
	if !d.dirty {
		d.dirtyAt = t
	}
	d.lastMark = t
	d.dirty = true
	d.fired = false
}

func (d *Debouncer) Pending() bool { return d.dirty }

func (d *Debouncer) Due(now time.Time) bool {
	if !d.dirty || d.fired {
		return false
	}
	// dirtyAt is the first mark since the last sample, so the ceiling paces
	// samples of an animating pane rather than measuring from its first frame
	// ever. Such a sample can land mid-repaint; the next one corrects it.
	if now.Sub(d.lastMark) >= d.window || now.Sub(d.dirtyAt) >= d.ceiling {
		d.fired = true
		d.dirty = false
		return true
	}
	return false
}
