package daemon

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"

	"github.com/noamsto/tmux-og/picker/remotebridge/render"
)

// newTermEmu is a vt emulator whose reply pipe is drained, so queries in the
// stream never block Write. It is never Close()d: see predictModel.close.
func newTermEmu(t *testing.T, w, h int) *vt.Emulator {
	t.Helper()
	e := vt.NewEmulator(w, h)
	e.SetScrollbackSize(0)
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := e.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if c, ok := e.InputPipe().(io.Closer); ok {
			_ = c.Close()
		}
	})
	return e
}

func screenEq(t *testing.T, local, ref *vt.Emulator) {
	t.Helper()
	for y := range ref.Height() {
		for x := range ref.Width() {
			l, r := local.CellAt(x, y), ref.CellAt(x, y)
			if l == nil || r == nil {
				t.Fatalf("cell(%d,%d): local %v ref %v", x, y, l, r)
			}
			if l.Content != r.Content || l.Width != r.Width || !l.Style.Equal(&r.Style) {
				t.Errorf("cell(%d,%d): local %q w%d %+v, ref %q w%d %+v", x, y, l.Content, l.Width, l.Style, r.Content, r.Width, r.Style)
			}
		}
	}
	if lp, rp := local.CursorPosition(), ref.CursorPosition(); lp != rp {
		t.Errorf("cursor: local %v, ref %v", lp, rp)
	}
}

func noFaint(t *testing.T, e *vt.Emulator) {
	t.Helper()
	for y := range e.Height() {
		for x := range e.Width() {
			if c := e.CellAt(x, y); c != nil && c.Style.Attrs&uv.AttrFaint != 0 {
				t.Errorf("cell(%d,%d) %q is faint", x, y, c.Content)
			}
		}
	}
}

// predHarness drives a predictor the way the pump does: local is the mirror
// pane, fed every byte written to the renderer; ref is fed only the remote's
// stream.
type predHarness struct {
	t          *testing.T
	p          *predictor
	local, ref *vt.Emulator
	clock      time.Time
	requests   int
	paused     bool
	lost       bool
}

type harnessOpts struct {
	w, h   int
	auto   bool // PredictAuto instead of PredictOn
	alt    bool
	hidden bool
}

func newHarness(t *testing.T, captured string, cx, cy int, o harnessOpts) *predHarness {
	t.Helper()
	if o.w == 0 {
		o.w, o.h = 80, 24
	}
	mode := PredictOn
	if o.auto {
		mode = PredictAuto
	}
	h := &predHarness{
		t:     t,
		local: newTermEmu(t, o.w, o.h),
		ref:   newTermEmu(t, o.w, o.h),
		clock: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
	}
	h.p = newPredictor(newPredictShared(mode),
		func() { h.requests++ },
		func() bool { return h.paused },
		func() bool {
			l := h.lost
			h.lost = false
			return l
		})
	h.p.now = func() time.Time { return h.clock }
	t.Cleanup(func() { h.p.shutdown() })

	seed := render.Seed([]byte(captured), cx, cy, o.alt, false, nil, nil, nil)
	h.writeLocal(h.p.onResize(o.w, o.h))
	h.p.observe(seed)
	h.writeLocal(seed)
	h.writeRef(seed)
	m := plainModes(o.h)
	m.cursorVisible = !o.hidden
	m.cx, m.cy = cx, cy
	h.p.onModelSeed(seed, m)
	if h.p.model == nil {
		t.Fatal("onModelSeed built no model")
	}
	// A model is only ever requested by a key; stand in for it so output
	// before the test's first key is not an idle pane's.
	h.p.lastKey = h.clock
	return h
}

func (h *predHarness) writeLocal(b []byte) {
	h.t.Helper()
	if _, err := h.local.Write(b); err != nil {
		h.t.Fatal(err)
	}
}

func (h *predHarness) writeRef(b []byte) {
	h.t.Helper()
	if _, err := h.ref.Write(b); err != nil {
		h.t.Fatal(err)
	}
}

func (h *predHarness) keys(s string) []byte {
	h.t.Helper()
	out := h.p.onKeys([]byte(s))
	h.writeLocal(out)
	return out
}

func (h *predHarness) output(s string) []byte {
	h.t.Helper()
	h.writeRef([]byte(s))
	out := h.p.onOutput([]byte(s))
	h.writeLocal(out)
	return out
}

func (h *predHarness) expire() []byte {
	h.t.Helper()
	out := h.p.onExpire()
	h.writeLocal(out)
	return out
}

// confirm types and echoes one key, confirming the current epoch.
func (h *predHarness) confirm(s string) {
	h.t.Helper()
	if out := h.keys(s); out != nil {
		h.t.Fatalf("tentative key %q painted %q", s, out)
	}
	h.output(s)
}

func (h *predHarness) model() *predictModel {
	h.t.Helper()
	m := h.p.model
	if m == nil {
		h.t.Fatal("no model")
	}
	return m
}

func (h *predHarness) cell(x, y int) *uv.Cell {
	h.t.Helper()
	c := h.local.CellAt(x, y)
	if c == nil {
		h.t.Fatalf("local cell(%d,%d) out of bounds", x, y)
	}
	return c
}

func (h *predHarness) requireFaint(x, y int, want string) {
	h.t.Helper()
	if c := h.cell(x, y); c.Content != want || c.Style.Attrs&uv.AttrFaint == 0 {
		h.t.Errorf("local cell(%d,%d) = %q %+v, want faint %q", x, y, c.Content, c.Style, want)
	}
}

func (h *predHarness) settled() {
	h.t.Helper()
	screenEq(h.t, h.local, h.ref)
	noFaint(h.t, h.local)
}

func TestPredictorAC1(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("l")
	h.keys("s")
	h.requireFaint(3, 0, "s")
	if p := h.local.CursorPosition(); p.X != 4 || p.Y != 0 {
		t.Errorf("local cursor = %v, want (4,0)", p)
	}
	h.output("s")
	h.settled()
	if c := h.cell(3, 0); c.Content != "s" {
		t.Errorf("cell(3,0) = %q, want s", c.Content)
	}
}

func TestPredictorTentative(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	if out := h.keys("l"); out != nil {
		t.Fatalf("first key painted %q", out)
	}
	if out := h.output("l"); !bytes.Equal(out, []byte("l")) {
		t.Errorf("echo returned %q, want the payload alone", out)
	}
	if out := h.keys("s"); !bytes.Contains(out, []byte("\x1b7")) {
		t.Errorf("key after a confirmed echo returned %q, want a paint", out)
	}
}

func TestPredictorAC2(t *testing.T) {
	for name, o := range map[string]harnessOpts{
		"alt screen":    {alt: true},
		"hidden cursor": {hidden: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "$ ", 2, 0, o)
			for _, k := range []string{"l", "s", "x"} {
				if out := h.keys(k); out != nil {
					t.Errorf("keys(%q) = %q, want nil", k, out)
				}
				if out := h.output(k); !bytes.Equal(out, []byte(k)) {
					t.Errorf("output(%q) = %q, want the payload alone", k, out)
				}
			}
			h.settled()
		})
	}
}

func TestPredictorMispredictions(t *testing.T) {
	cases := []struct {
		name string
		run  func(h *predHarness)
	}{
		{"different char", func(h *predHarness) {
			h.keys("b")
			h.requireFaint(3, 0, "b")
			h.output("c")
		}},
		{"no echo then expiry", func(h *predHarness) {
			h.keys("b")
			h.clock = h.clock.Add(predictExpiryMin - time.Millisecond)
			if out := h.expire(); out != nil {
				h.t.Errorf("early expire returned %q", out)
			}
			h.requireFaint(3, 0, "b")
			h.clock = h.clock.Add(time.Millisecond)
			if out := h.expire(); !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
				h.t.Errorf("expire returned %q, want an unpaint", out)
			}
			if len(h.p.preds) != 0 {
				h.t.Errorf("preds = %v after expiry", h.p.preds)
			}
		}},
		{"redraw elsewhere", func(h *predHarness) {
			h.keys("bc")
			h.output("\r\x1b[K$ other")
		}},
		{"backspace refused", func(h *predHarness) {
			h.keys("\x7f")
			h.requireFaint(2, 0, " ")
			h.output("\x07")
			h.requireFaint(2, 0, " ")
			h.clock = h.clock.Add(predictExpiryMin)
			h.expire()
		}},
		{"fish autosuggestion redraw", func(h *predHarness) {
			h.output("\x1b[90mit status\x1b[m\x1b[1;4H")
			h.keys("i")
			h.requireFaint(3, 0, "i")
			// Mid-redraw the cursor sits at column 0: pending, repainted.
			if out := h.output("\r\x1b[K"); !bytes.Contains(out, []byte("\x1b7")) {
				h.t.Errorf("pending prediction not repainted: %q", out)
			}
			h.output("$ gi\x1b[90mt status\x1b[m\x1b[1;5H")
			if len(h.p.preds) != 0 {
				h.t.Errorf("preds = %v, want the redraw to confirm", h.p.preds)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "$ ", 2, 0, harnessOpts{})
			h.confirm("g")
			c.run(h)
			h.settled()
		})
	}
}

func TestPredictorMidLineInsertEcho(t *testing.T) {
	h := newHarness(t, "$ echo world", 7, 0, harnessOpts{})
	h.keys("x")
	h.output("\x1b[1@x")
	h.keys("yz")
	h.requireFaint(8, 0, "y")
	h.requireFaint(9, 0, "z")
	h.output("\x1b[1@y")
	h.requireFaint(9, 0, "z")
	h.output("\x1b[1@z")
	h.settled()
	if got := h.local.Render(); !strings.HasPrefix(got, "$ echo xyzworld") {
		t.Errorf("local = %q", got)
	}
}

func TestPredictorSeedDrops(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	seed := render.Seed([]byte("$ ab"), 4, 0, false, false, nil, nil, nil)
	out := h.p.onSeed(seed)
	if !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
		t.Errorf("onSeed = %q, want an unpaint", out)
	}
	h.writeLocal(out)
	h.p.observe(seed)
	h.writeLocal(seed)
	h.writeRef(seed)
	if len(h.p.preds) != 0 {
		t.Errorf("preds = %v after a seed", h.p.preds)
	}
	if h.p.model == nil {
		t.Fatal("a real seed dropped the model")
	}
	h.settled()
	if out := h.keys("c"); out != nil {
		t.Errorf("first key after a seed painted %q", out)
	}
}

func TestPredictorResizeDrops(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	model := h.model()
	if out := h.p.onResize(100, 30); string(out) != "\x1b8" {
		t.Errorf("onResize = %q, want ESC 8", out)
	}
	if h.p.model != nil || len(h.p.preds) != 0 || h.p.paintedAny {
		t.Errorf("model %v preds %v paintedAny %v after a resize", h.p.model, h.p.preds, h.p.paintedAny)
	}
	requireDrained(t, model)
	if out := h.p.onResize(100, 30); out != nil {
		t.Errorf("same-dims onResize = %q", out)
	}
	h.keys("c")
	if h.requests != 1 {
		t.Errorf("requests = %d, want 1", h.requests)
	}
}

func TestPredictorStickySlot(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	h.requireFaint(3, 0, "b")
	payload := "\x1b7\x1b[5;1Hstatus\x1b8"
	out := h.output(payload)
	if !bytes.HasSuffix(out, []byte(payload)) {
		t.Errorf("output after ESC 7 = %q, want no paint after the payload", out)
	}
	if out := h.keys("c"); out != nil {
		t.Errorf("keys after the app used the slot = %q", out)
	}
	h.settled()
}

func TestPredictorSplitSequenceNoPaint(t *testing.T) {
	cases := []struct{ name, first, second, last string }{
		{"OSC 8", "\x1b]8;;http://x", "\x1b\\", "\x1b]8;;\x1b\\b"},
		{"UTF-8", "\x1b[2;1H\xc3", "\xa9", "\x1b[1;4Hb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "$ ", 2, 0, harnessOpts{})
			h.confirm("a")
			h.keys("b")
			if out := h.output(c.first); !bytes.HasSuffix(out, []byte(c.first)) {
				t.Errorf("paint after a split sequence: %q", out)
			}
			if out := h.output(c.second); !bytes.HasPrefix(out, []byte(c.second+"\x1b7")) {
				t.Errorf("no paint once the sequence ended: %q", out)
			}
			h.output(c.last)
			h.settled()
		})
	}
}

func TestPredictorUnpaintResetsPen(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.output("\x1b[90mfoo\x1b[m \x1b[1;31m\x1b[1;3H")
	h.keys("f")
	h.output("f")
	h.keys("xyz")
	h.requireFaint(5, 0, "z")
	h.output("q")
	h.settled()
	h.output("r")
	h.settled()
	if c := h.cell(4, 0); c.Style.Attrs&uv.AttrBold == 0 {
		t.Errorf("cell(4,0) %+v lost the remote's bold red pen", c.Style)
	}
}

func TestPredictorErase(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("\x7f")
	h.requireFaint(2, 0, " ")
	h.output("\b \b")
	h.settled()

	h.keys("\r")
	h.output("\r\n$ ")
	if out := h.keys("\x7f"); out != nil || !h.p.blocked {
		t.Errorf("backspace opening an epoch = %q blocked=%v, want refused", out, h.p.blocked)
	}
}

// An erase and a char echoed in one payload leave the cursor past both, which
// the head-by-head settle reads as the erase still pending.
func TestPredictorCoalescedEcho(t *testing.T) {
	t.Run("erase then char", func(t *testing.T) {
		h := newHarness(t, "$ ", 2, 0, harnessOpts{})
		h.confirm("a")
		h.keys("b")
		h.output("b")
		h.keys("\x7f")
		h.keys("c")
		h.output("\b \bc")
		if len(h.p.preds) != 0 {
			t.Errorf("preds = %+v after the coalesced echo", h.p.preds)
		}
		h.settled()
	})
	t.Run("char then erase", func(t *testing.T) {
		h := newHarness(t, "$ ", 2, 0, harnessOpts{})
		h.confirm("a")
		h.keys("b")
		h.output("b")
		h.keys("c")
		h.keys("\x7f")
		h.output("c\b \b")
		h.keys("d")
		h.output("d")
		if len(h.p.preds) != 0 {
			t.Errorf("preds = %+v after d echoed", h.p.preds)
		}
		h.settled()
		if out := h.keys("e"); !bytes.Contains(out, []byte("\x1b[1;6H\x1b[0;2me")) {
			t.Errorf("key after the settle = %q, want a paint of e at column 5", out)
		}
	})
	// A char erased again nets a blank the cursor already sits on: no echo
	// evidence, so a silent prompt must not confirm the epoch.
	t.Run("net blank does not confirm", func(t *testing.T) {
		h := newHarness(t, "Password: ", 10, 0, harnessOpts{})
		h.keys("c")
		h.keys("\x7f")
		h.output("\x1b[m")
		if h.p.confirmed != -1 {
			t.Errorf("confirmed = %d with nothing echoed", h.p.confirmed)
		}
		if out := h.keys("x"); out != nil {
			t.Errorf("key at a silent prompt painted %q", out)
		}
	})
}

// A screen title shifts vt's cursor unless the model drops it; the paint must
// land where the remote's cursor is.
func TestPredictorScreenTitle(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.p.onOutput([]byte("\x1bkzsh\x1b\\"))
	h.keys("l")
	h.p.onOutput([]byte("l"))
	if out := h.p.onKeys([]byte("s")); !bytes.Contains(out, []byte("\x1b[1;4H\x1b[0;2ms")) {
		t.Errorf("paint = %q, want a faint s at column 3", out)
	}
}

func TestPredictorREPFloodDropsModel(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	model := h.model()
	start := time.Now()
	h.p.onOutput([]byte(strings.Repeat("\x1b[65535b", 200)))
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("REP flood took %v", d)
	}
	if h.p.model != nil {
		t.Fatal("model survived a REP over the pane width")
	}
	requireDrained(t, model)
}

func TestPredictorAltScreenMidStream(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	h.requireFaint(3, 0, "b")
	h.output("\x1b[?1049h\x1b[H\x1b[2J")
	if len(h.p.preds) != 0 {
		t.Errorf("preds = %+v on the alt screen", h.p.preds)
	}
	for _, k := range []string{"j", "k"} {
		if out := h.keys(k); out != nil {
			t.Errorf("keys(%q) on the alt screen = %q", k, out)
		}
	}
	h.output("jk")
	h.settled()
}

func TestPredictorShutdown(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	model := h.model()
	out := h.p.shutdown()
	if !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
		t.Errorf("shutdown = %q, want an unpaint", out)
	}
	h.writeLocal(out)
	requireDrained(t, model)
	h.settled()
}

func TestPredictorRightEdge(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{w: 10, h: 3})
	h.confirm("a")
	h.keys("bcdefghij")
	if !h.p.blocked {
		t.Error("not blocked at the right edge")
	}
	if n := len(h.p.preds); n != 5 {
		t.Errorf("%d predictions, want 5 (cols 3-7)", n)
	}
	h.requireFaint(7, 0, "f")
	for x := 8; x < 10; x++ {
		if c := h.cell(x, 0); c.Content != " " {
			t.Errorf("cell(%d,0) = %q, painted past w-2", x, c.Content)
		}
	}
	if p := h.local.CursorPosition(); p.X != 8 {
		t.Errorf("local cursor = %v, want x 8", p)
	}
	h.output("bcdef")
	h.settled()
}

// A real cursor in the last column may be pending a wrap, which DECRC does
// not restore: no paint then, or the remote's next char lands unwrapped.
func TestPredictorPendingWrapNoPaint(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{w: 10, h: 4})
	h.confirm("a")
	h.keys("b")
	if out := h.output("\x1b[2;1H0123456789"); !bytes.HasSuffix(out, []byte("0123456789")) {
		t.Errorf("paint with the cursor in the last column: %q", out)
	}
	h.output("X")
	h.output("\x1b[1;4Hb")
	h.settled()
}

func TestPredictorBudgetAndIdle(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		h := newHarness(t, "$ ", 2, 0, harnessOpts{})
		h.confirm("a")
		h.keys("b")
		model := h.model()
		h.output(strings.Repeat("\x1b[H", predictOutputBudget/3+1))
		if h.p.model != nil {
			t.Fatal("model survived the output budget")
		}
		requireDrained(t, model)
		h.settled()
		h.keys("c")
		h.keys("d")
		if h.requests != 1 {
			t.Errorf("requests = %d, want 1 (rate-limited)", h.requests)
		}
		h.clock = h.clock.Add(predictModelRetry)
		h.keys("e")
		if h.requests != 2 {
			t.Errorf("requests = %d, want 2", h.requests)
		}
	})
	t.Run("idle", func(t *testing.T) {
		h := newHarness(t, "$ ", 2, 0, harnessOpts{})
		h.confirm("a")
		h.clock = h.clock.Add(predictIdle + time.Second)
		h.output("x")
		if h.p.model != nil {
			t.Fatal("model survived the idle timeout")
		}
		h.settled()
		h.keys("c")
		if h.requests != 1 {
			t.Errorf("requests = %d, want 1", h.requests)
		}
	})
}

func TestPredictorKeysLostAndPaused(t *testing.T) {
	for name, set := range map[string]func(*predHarness){
		"keysLost": func(h *predHarness) { h.lost = true },
		"paused":   func(h *predHarness) { h.paused = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "$ ", 2, 0, harnessOpts{})
			h.confirm("a")
			h.keys("b")
			set(h)
			if out := h.keys("c"); !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
				t.Errorf("keys = %q, want an unpaint", out)
			}
			if len(h.p.preds) != 0 {
				t.Errorf("preds = %v", h.p.preds)
			}
			h.paused = false
			h.output("bc")
			h.settled()
		})
	}
}

func TestPredictorAutoSamples(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{auto: true})
	h.keys("a")
	if h.p.model != nil {
		t.Fatal("model kept while prediction is not wanted")
	}
	if h.requests != 0 {
		t.Fatalf("requests = %d before any latency sample", h.requests)
	}
	h.clock = h.clock.Add(50 * time.Millisecond)
	h.output("a")
	h.keys("b")
	if h.requests != 1 {
		t.Errorf("requests = %d, want 1 once echo latency is high", h.requests)
	}
	if got := h.p.expiry(); got != predictExpiryMin {
		t.Errorf("expiry = %v, want the %v floor for a 50ms estimate", got, predictExpiryMin)
	}
}

func TestStreamGround(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"empty", nil, true},
		{"text", []string{"abc"}, true},
		{"lone ESC", []string{"\x1b"}, false},
		{"ESC final", []string{"\x1b7"}, true},
		{"ESC intermediate", []string{"\x1b("}, false},
		{"ESC charset", []string{"\x1b(0"}, true},
		{"CSI open", []string{"\x1b[1;3"}, false},
		{"CSI split", []string{"\x1b[1;", "31m"}, true},
		{"ESC restarts CSI", []string{"\x1b[1\x1b"}, false},
		{"ESC in CSI then final", []string{"\x1b[1\x1b[m"}, true},
		{"OSC open", []string{"\x1b]8;;http://x"}, false},
		{"OSC BEL", []string{"\x1b]0;title\x07"}, true},
		{"OSC ST", []string{"\x1b]8;;http://x", "\x1b\\"}, true},
		{"OSC ESC pending", []string{"\x1b]0;t\x1b"}, false},
		{"APC BEL does not end", []string{"\x1b_Gq=1\x07"}, false},
		{"APC ST", []string{"\x1b_Gq=1\x1b\\"}, true},
		{"DCS BEL does not end", []string{"\x1bPtmux;\x07"}, false},
		{"DCS passthrough wrapped OSC", []string{"\x1bPtmux;\x1b\x1b]0;x\x07"}, false},
		{"DCS passthrough inner ST", []string{"\x1bPtmux;\x1b\x1b_Gx\x1b\x1b\\"}, false},
		{"DCS passthrough outer ST", []string{"\x1bPtmux;\x1b\x1b_Gx\x1b\x1b\\\x1b\\"}, true},
		{"DCS ESC non-backslash is data", []string{"\x1bPq\x1b[m"}, false},
		{"DCS ST split", []string{"\x1bPq", "\x1b", "\\"}, true},
		{"CAN in DCS data", []string{"\x1bPq#0\x18abc"}, false},
		{"CAN after DCS ESC", []string{"\x1bPq\x1b\x18"}, false},
		{"CAN before DCS final", []string{"\x1bP1\x18abc"}, true},
		{"ESC k open", []string{"\x1bkti"}, false},
		{"ESC k BEL does not end", []string{"\x1bkt\x07"}, false},
		{"ESC k ST", []string{"\x1bkt", "\x1b\\"}, true},
		{"ESC k ended by CSI", []string{"\x1bkt\x1b[1m"}, true},
		{"string ESC non-backslash starts CSI", []string{"\x1b]0;t\x1b["}, false},
		{"string ESC non-backslash final", []string{"\x1b]0;t\x1b[m"}, true},
		{"string ESC then ESC final", []string{"\x1b_x\x1b7"}, true},
		{"CAN in CSI", []string{"\x1b[1;3\x18"}, true},
		{"SUB in OSC", []string{"\x1b]0;t\x1a"}, true},
		{"CAN in UTF-8", []string{"\xe2\x18"}, true},
		{"UTF-8 2-byte split", []string{"\xc3"}, false},
		{"UTF-8 2-byte whole", []string{"\xc3", "\xa9"}, true},
		{"UTF-8 3-byte partial", []string{"\xe2\x94"}, false},
		{"UTF-8 4-byte whole", []string{"\xf0\x9f\x98", "\x80"}, true},
		{"UTF-8 broken by ASCII", []string{"\xe2a"}, true},
		{"UTF-8 broken by ESC", []string{"\xe2\x1b"}, false},
		{"C0 inside ESC stays", []string{"\x1b\n"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g streamGround
			for _, ch := range c.chunks {
				g.observe([]byte(ch))
			}
			if got := g.atGround(); got != c.want {
				t.Errorf("atGround = %v, want %v (state %+v)", got, c.want, g)
			}
		})
	}
}
