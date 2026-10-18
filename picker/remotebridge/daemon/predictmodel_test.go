package daemon

import (
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// plainModes is a default pane of height h: cursor shown, wrapping, no scroll region.
func plainModes(h int) predictModes {
	return predictModes{cursorVisible: true, wrap: true, bottom: h - 1}
}

func newTestModel(t *testing.T, w, h int, seed string, m predictModes) *predictModel {
	t.Helper()
	pm := newPredictModel(w, h, []byte(seed), m)
	if pm == nil {
		t.Fatal("newPredictModel returned nil")
	}
	t.Cleanup(pm.close)
	return pm
}

func mustCell(t *testing.T, pm *predictModel, x, y int) *uv.Cell {
	t.Helper()
	c := pm.cell(x, y)
	if c == nil {
		t.Fatalf("cell(%d,%d) is out of bounds", x, y)
	}
	return c
}

func requireDrained(t *testing.T, pm *predictModel) {
	t.Helper()
	select {
	case <-pm.drained:
	case <-time.After(time.Second):
		t.Fatal("drain goroutine did not exit within 1s of close()")
	}
}

func TestPredictModelSeedCursorAndCells(t *testing.T) {
	m := plainModes(5)
	pm := newTestModel(t, 20, 5, "$ ls\x1b[3;4H", m)

	if x, y := pm.cursor(); x != 3 || y != 2 {
		t.Errorf("cursor = (%d,%d), want (3,2)", x, y)
	}
	for i, want := range []string{"$", " ", "l", "s"} {
		if c := mustCell(t, pm, i, 0); c.Content != want {
			t.Errorf("cell(%d,0) = %+v, want %q", i, c, want)
		}
	}
	if pm.hidden || pm.insert || pm.origin || pm.slotUsed || pm.alt() {
		t.Errorf("flags set on a plain seed: %+v", pm)
	}
}

func TestPredictModelModeFlags(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", predictModes{insert: true, origin: true, wrap: true, bottom: 4})
	if !pm.hidden || !pm.insert || !pm.origin {
		t.Errorf("hidden=%v insert=%v origin=%v, want all true", pm.hidden, pm.insert, pm.origin)
	}

	pm = newTestModel(t, 20, 5, "", plainModes(5))
	if pm.hidden || pm.insert || pm.origin {
		t.Errorf("hidden=%v insert=%v origin=%v, want all false", pm.hidden, pm.insert, pm.origin)
	}
}

func TestPredictModelWrapOff(t *testing.T) {
	pm := newTestModel(t, 5, 2, "", predictModes{cursorVisible: true, bottom: 1})
	if !pm.feed([]byte("abcdefgh")) {
		t.Fatal("feed failed")
	}
	if x, y := pm.cursor(); y != 0 || x != 4 {
		t.Errorf("cursor = (%d,%d), want (4,0): no wrap", x, y)
	}
}

func TestPredictModelScrollRegionRestoresCursor(t *testing.T) {
	m := plainModes(6)
	m.top, m.bottom, m.cx, m.cy = 1, 3, 4, 2
	pm := newTestModel(t, 20, 6, "", m)

	if x, y := pm.cursor(); x != 4 || y != 2 {
		t.Errorf("cursor = (%d,%d), want (4,2)", x, y)
	}
	// A linefeed at the region's bottom scrolls the region, not the screen.
	if !pm.feed([]byte("\x1b[1;1Htop\x1b[4;1Hbot\n")) {
		t.Fatal("feed failed")
	}
	if c := mustCell(t, pm, 0, 0); c.Content != "t" {
		t.Errorf("row 0 = %q, want untouched \"t\"", c.Content)
	}
	if c := mustCell(t, pm, 0, 2); c.Content != "b" {
		t.Errorf("row 2 = %q, want \"b\" scrolled up inside the region", c.Content)
	}
}

func TestPredictModelOriginAfterScrollRegion(t *testing.T) {
	m := plainModes(6)
	m.top, m.bottom, m.cx, m.cy, m.origin = 2, 4, 3, 3, true
	pm := newTestModel(t, 20, 6, "", m)

	if x, y := pm.cursor(); x != 3 || y != 3 {
		t.Errorf("cursor = (%d,%d), want (3,3)", x, y)
	}
	if !pm.origin {
		t.Error("origin not tracked")
	}
}

func TestPredictModelSlotUsed(t *testing.T) {
	for name, save := range map[string]string{"ESC 7": "\x1b7", "CSI s": "\x1b[s"} {
		t.Run(name, func(t *testing.T) {
			pm := newTestModel(t, 20, 5, "", plainModes(5))
			if pm.slotUsed {
				t.Fatal("slotUsed set before any save")
			}
			if !pm.feed([]byte("\x1b[2;5H" + save + "\x1b[1;1H")) {
				t.Fatal("feed failed")
			}
			if !pm.slotUsed {
				t.Error("slotUsed not set")
			}
			if !pm.feed([]byte("\x1b8")) {
				t.Fatal("feed failed")
			}
			if x, y := pm.cursor(); x != 4 || y != 1 {
				t.Errorf("after restore cursor = (%d,%d), want (4,1): vt's default save must still run", x, y)
			}
		})
	}
}

func TestPredictModelCursorVisibilityCallback(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", predictModes{wrap: true, bottom: 4})
	if !pm.hidden {
		t.Fatal("seed with cursorVisible=false did not set hidden")
	}
	pm.feed([]byte("\x1b[?25h"))
	if pm.hidden {
		t.Error("?25h did not clear hidden")
	}
	pm.feed([]byte("\x1b[?25l"))
	if !pm.hidden {
		t.Error("?25l did not set hidden")
	}
}

func TestPredictModelModesTrackedAfterSeed(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", plainModes(5))
	pm.feed([]byte("\x1b[4h\x1b[?6h"))
	if !pm.insert || !pm.origin {
		t.Errorf("insert=%v origin=%v, want both true", pm.insert, pm.origin)
	}
	pm.feed([]byte("\x1b[4l\x1b[?6l"))
	if pm.insert || pm.origin {
		t.Errorf("insert=%v origin=%v, want both false", pm.insert, pm.origin)
	}
}

func TestPredictModelAltScreen(t *testing.T) {
	pm := newTestModel(t, 20, 5, "\x1b[?1049h", plainModes(5))
	if !pm.alt() {
		t.Error("alt() = false after ?1049h")
	}
}

func TestPredictModelCloseParkedReader(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", plainModes(5))
	pm.close()
	requireDrained(t, pm)
	pm.close()
}

func TestPredictModelCloseAfterDSRReply(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", plainModes(5))
	if !pm.feed([]byte("\x1b[6n")) {
		t.Fatal("feed failed")
	}
	pm.close()
	requireDrained(t, pm)
	// Replies after close fail inside vt instead of blocking.
	done := make(chan struct{})
	go func() {
		pm.feed([]byte("\x1b[6n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("feed of a query blocked after close()")
	}
}

func TestPredictModelNilCloseIsNoop(t *testing.T) {
	var pm *predictModel
	pm.close()
}

func TestPredictModelDSRDoesNotBlock(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", plainModes(5))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			pm.feed([]byte("\x1b[6n\x1b[c"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("feeding queries blocked: reply pipe is not drained")
	}
}

// Step 3 refuses to predict onto any cell whose Width is not 1.
func TestPredictModelWideCellRepresentation(t *testing.T) {
	pm := newTestModel(t, 20, 2, "漢a", plainModes(2))

	lead, cont, next := mustCell(t, pm, 0, 0), mustCell(t, pm, 1, 0), mustCell(t, pm, 2, 0)
	if lead.Content != "漢" || lead.Width != 2 {
		t.Errorf("lead cell = %q width %d, want \"漢\" width 2", lead.Content, lead.Width)
	}
	if cont.Content != "" || cont.Width != 0 {
		t.Errorf("continuation cell = %q width %d, want empty content and width 0", cont.Content, cont.Width)
	}
	if next.Content != "a" || next.Width != 1 {
		t.Errorf("cell after the wide rune = %q width %d, want \"a\" width 1", next.Content, next.Width)
	}
}
