package daemon

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
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

// tmux takes ESC k as a window-rename string ended by any ESC; vt would print
// it. oh-my-zsh emits one at every prompt under TERM=tmux*/screen*.
func TestPredictModelScreenTitle(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		wantX  int
		text   string
	}{
		{"ST", []string{"\x1bkcd foo\x1b\\"}, 2, "$ "},
		{"split in title", []string{"\x1bkcd", " foo\x1b\\"}, 2, "$ "},
		{"split after ending ESC", []string{"\x1bkcd foo\x1b", "\\"}, 2, "$ "},
		{"split after opening ESC", []string{"\x1b", "kcd foo\x1b\\"}, 2, "$ "},
		{"between text", []string{"ab\x1bkt\x1b\\c"}, 5, "$ abc"},
		{"ended by a new sequence", []string{"\x1bkt\x1b[1mx"}, 3, "$ x"},
		{"ended by CAN", []string{"\x1bkt\x18x"}, 3, "$ x"},
		{"after ESC ESC", []string{"\x1b\x1bkt\x1b\\x"}, 3, "$ x"},
		{"C0 between ESC and k", []string{"\x1b\bkt\x1b\\x"}, 2, "$x"},
		{"OSC ST kept", []string{"\x1b]0;t\x1b", "\\x"}, 3, "$ x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := plainModes(5)
			m.cx = 2
			pm := newTestModel(t, 20, 5, "$ ", m)
			for _, ch := range c.chunks {
				if !pm.feed([]byte(ch)) {
					t.Fatalf("feed(%q) failed", ch)
				}
			}
			if x, y := pm.cursor(); x != c.wantX || y != 0 {
				t.Errorf("cursor = (%d,%d), want (%d,0)", x, y, c.wantX)
			}
			for i, want := range c.text {
				if got := mustCell(t, pm, i, 0).Content; got != string(want) {
					t.Errorf("cell(%d,0) = %q, want %q", i, got, want)
				}
			}
		})
	}
	t.Run("pen after title", func(t *testing.T) {
		pm := newTestModel(t, 20, 5, "", plainModes(5))
		pm.feed([]byte("\x1bkt\x1b[1mx"))
		if c := mustCell(t, pm, 0, 0); c.Style.Attrs&uv.AttrBold == 0 {
			t.Errorf("cell(0,0) %+v, want bold: the ESC ending the title starts a CSI", c.Style)
		}
	})
}

// REP repeats the last char n times, n up to 65535, and tmux's REP depends on
// state the model does not track: any REP costs the model, even one in width.
func TestPredictModelREP(t *testing.T) {
	pm := newTestModel(t, 20, 5, "", plainModes(5))
	if !pm.feed([]byte("a")) {
		t.Fatal("feed of a plain char failed")
	}
	if pm.feed([]byte("\x1b[5b")) {
		t.Error("feed of an in-width REP succeeded")
	}
	if pm.feed([]byte("b")) {
		t.Error("feed succeeded after a poisoning REP")
	}
}

// Rows tmux next-3.9 (-f /dev/null, 20x6 pane) draws for each stream: a model
// that survives a REP must draw the same.
func TestPredictModelREPMatchesTmux(t *testing.T) {
	cases := []struct {
		name, in string
		tmux     []string
	}{
		{"in width", "a\x1b[5b", []string{"aaaaaa"}},
		{"capped at line end", "ABCDEFGHIJKLMNO\x1b[10b|", []string{"ABCDEFGHIJKLMNOOOOOO", "|"}},
		{"exact fit", "ABCDEFGHIJKLMNOP\x1b[3b|", []string{"ABCDEFGHIJKLMNOPPPP|"}},
		{"pending wrap prints none", "ABCDEFGHIJKLMNOPQRST\x1b[1b|", []string{"ABCDEFGHIJKLMNOPQRST", "|"}},
		{"last column prints one", "ABCDEFGHIJKLMNOPQRS\x1b[1b|", []string{"ABCDEFGHIJKLMNOPQRSS", "|"}},
		{"after C0 prints none", "xa\r\x1b[3b|", []string{"|a"}},
		{"after CSI prints none", "xa\x1b[1m\x1b[3b|", []string{"xa|"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pm := newTestModel(t, 20, 6, "", plainModes(6))
			if !pm.feed([]byte(c.in)) {
				return
			}
			for y, want := range c.tmux {
				var b strings.Builder
				for x := range 20 {
					b.WriteString(cellText(pm.cell(x, y)))
				}
				if got := strings.TrimRight(b.String(), " "); got != want {
					t.Errorf("row %d: model %q, tmux %q", y, got, want)
				}
			}
		})
	}
}

// Rows and cursor tmux next-3.9 (-f /dev/null, 20x6 pane) ends with for each
// byte stream carrying bytes that are not valid UTF-8 or are C1 controls
// (tmux treats none of them as controls). A keep stream must stay modelled and
// draw them; any other must either cost the model or draw them.
func TestPredictModelNonUTF8MatchesTmux(t *testing.T) {
	const fffd = "\uFFFD"
	cases := []struct {
		name, in string
		keep     bool
		rows     []string
		cursor   [2]int
	}{
		{"ground 0x84", "a\x84b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"ground 0x8d", "a\x8db", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"ground 0x9c", "a\x9cb", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"ground 0x90", "a\x90bc\x9cd", false, []string{"a" + fffd + "bc" + fffd + "d"}, [2]int{6, 0}},
		{"ground 0x9d", "a\x9d0;title\x9cb", false, []string{"a" + fffd + "0;title" + fffd + "b"}, [2]int{11, 0}},
		{"ground 0x9b", "a\x9bX1mb", false, []string{"a" + fffd + "X1mb"}, [2]int{6, 0}},
		{"stray high bytes", "a\xa0b\xbfc\xc0d\xc1e\xf5f\xffg", false,
			[]string{"a" + fffd + "b" + fffd + "c" + fffd + "d" + fffd + "e" + fffd + "f" + fffd + "g"}, [2]int{13, 0}},
		{"truncated two-byte", "a\xc3b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"truncated three-byte", "a\xe2\x82b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"overlong", "a\xe0\x80\x80b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"surrogate", "a\xed\xa0\x80b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"above U+10FFFF", "a\xf4\x90\x80\x80b", false, []string{"a" + fffd + "b"}, [2]int{3, 0}},
		{"ESC state 0x84", "a\x1b\x84b", false, []string{"a"}, [2]int{1, 0}},
		{"CSI state 0x84", "a\x1b[1\x84mb", false, []string{"ab"}, [2]int{2, 0}},
		{"CSI state 0x9c", "a\x1b[\x9cmb", false, []string{"ab"}, [2]int{2, 0}},
		{"CSI state UTF-8", "a\x1b[\xc3\xa9m|", false, []string{"a|"}, [2]int{2, 0}},
		{"OSC body 0x9c", "a\x1b]2;ti\x9ctle\x07b", true, []string{"ab"}, [2]int{2, 0}},
		{"OSC body 0x84", "a\x1b]2;ti\x84tle\x07b", true, []string{"ab"}, [2]int{2, 0}},
		{"OSC body continuation 0x9c", "a\x1b]0;\xc4\x9c\x07b", true, []string{"ab"}, [2]int{2, 0}},
		{"DCS body 0x9c", "a\x1bPxy\x9cz\x1b\\b", true, []string{"ab"}, [2]int{2, 0}},
		{"DCS body 0x84", "a\x1bPxy\x84z\x1b\\b", true, []string{"ab"}, [2]int{2, 0}},
		{"APC body 0x9c", "a\x1b_xy\x9cz\x1b\\b", false, []string{"ab"}, [2]int{2, 0}},
		{"APC body 0x84", "a\x1b_xy\x84z\x1b\\b", false, []string{"ab"}, [2]int{2, 0}},
		{"SOS body 0x9c", "a\x1bXxy\x9cz\x1b\\b", false, []string{"ab"}, [2]int{2, 0}},
		{"PM body 0x9c", "a\x1b^xy\x9cz\x1b\\b", false, []string{"ab"}, [2]int{2, 0}},
		{"valid mixed UTF-8", "A\xc3\xa9BC", true, []string{"A\u00e9BC"}, [2]int{4, 0}},
		{"valid then stray", "A\xc3\xa9B\x84C", false, []string{"A\u00e9B" + fffd + "C"}, [2]int{5, 0}},
		{"U+00A0", "a\xc2\xa0|", true, []string{"a\u00a0|"}, [2]int{3, 0}},
		{"U+0084 is width 0", "a\xc2\x84b|", false, []string{"a\u0084b|"}, [2]int{3, 0}},
		{"U+009F is width 0", "a\xc2\x9f|", false, []string{"a\u009f|"}, [2]int{2, 0}},
		{"U+0084 under the cursor", "AB\r\xc2\x84", false, []string{"AB"}, [2]int{0, 0}},
		{"stray after CR", "ab\x84\r\x84|c", false, []string{fffd + "|c"}, [2]int{3, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pm := newTestModel(t, 20, 6, "", plainModes(6))
			if !pm.feed([]byte(c.in)) {
				if c.keep {
					t.Fatalf("feed refused %q: this stream must stay modelled", c.in)
				}
				return
			}
			for y, want := range c.rows {
				var b strings.Builder
				for x := range 20 {
					b.WriteString(cellText(pm.cell(x, y)))
				}
				if got := strings.TrimRight(b.String(), " "); got != want {
					t.Errorf("row %d: model %q, tmux %q", y, got, want)
				}
			}
			if x, y := pm.cursor(); x != c.cursor[0] || y != c.cursor[1] {
				t.Errorf("cursor: model (%d,%d), tmux (%d,%d)", x, y, c.cursor[0], c.cursor[1])
			}
		})
	}
}

// guardOracle reports whether tmux and vt part ways on in, from the tmux table
// and a parser of its own rather than from vtGuard's tracking.
func guardOracle(p *ansi.Parser, in []byte) bool {
	p.Reset()
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c < 0x80 {
			p.Advance(c)
			continue
		}
		switch p.State() {
		case parser.GroundState:
			rest := in[i:]
			if !utf8.FullRune(rest) {
				return false
			}
			r, size := utf8.DecodeRune(rest)
			if r == utf8.RuneError && size == 1 || r >= 0x80 && r <= 0x9f {
				return true
			}
			for _, b := range rest[:size] {
				p.Advance(b)
			}
			i += size - 1
		case parser.OscStringState, parser.DcsStringState:
			if c != 0x9c {
				p.Advance(c)
			}
		default:
			return true
		}
	}
	return false
}

// splitRandom cuts b at random points, empty chunks included.
func splitRandom(rng *rand.Rand, b []byte) [][]byte {
	var chunks [][]byte
	for len(b) > 0 {
		n := rng.IntN(len(b) + 1)
		chunks = append(chunks, b[:n])
		b = b[n:]
	}
	return chunks
}

// runGuard filters chunks through a fresh guard, stopping at the first poison.
func runGuard(chunks [][]byte) (out []byte, bad bool) {
	var g vtGuard
	for _, c := range chunks {
		if out, bad = g.filter(out, c); bad {
			return nil, true
		}
	}
	return out, false
}

// vtGuard must poison exactly where tmux and vt disagree and hand vt a stream
// its parser reads the way tmux does, whatever the chunking.
func TestVTGuardAgreesWithParser(t *testing.T) {
	ascii := []byte("aZ m0123456789;~\\\x1b[]P_X^k\x07\x18\x1a\n")
	structural := []byte("a\x1b[]P_X^k\x07\\")
	hot := []byte{0x84, 0x9c, 0xa9, 0xbd, 0xbf, 0xc2, 0xdf, 0xef}
	var high []byte
	for c := 0x80; c <= 0x9f; c++ {
		high = append(high, byte(c))
	}
	high = append(high, 0xa0, 0xa9, 0xbd, 0xbe, 0xbf, 0xc0, 0xc1, 0xc2, 0xdf, 0xe0, 0xe1, 0xed, 0xef,
		0xf0, 0xf1, 0xf3, 0xf4, 0xf5, 0xff)
	// Most high-byte soup poisons, so each stream draws its own density to
	// keep a share of streams that reach the parser comparison.
	densities := [...]int{2, 5, 10, 20, 30, 60}

	rng := rand.New(rand.NewPCG(1096, 1))
	oracle, check := ansi.NewParser(), ansi.NewParser()
	var executed []byte
	var printed []rune
	check.SetHandler(ansi.Handler{
		Print:   func(r rune) { printed = append(printed, r) },
		Execute: func(b byte) { executed = append(executed, b) },
	})
	var one [1]byte
	var step []byte
	const streams, modelEvery = 20000, 20000 / 300

	for n := range streams {
		raw := make([]byte, 1+rng.IntN(24))
		density := densities[rng.IntN(len(densities))]
		pool, hi := ascii, high
		if rng.IntN(2) == 0 {
			pool = structural
		}
		if rng.IntN(2) == 0 {
			hi = hot
		}
		for i := range raw {
			if rng.IntN(100) < density {
				raw[i] = hi[rng.IntN(len(hi))]
			} else {
				raw[i] = pool[rng.IntN(len(pool))]
			}
		}
		var title titleFilter
		in := title.filter(nil, raw)
		chunks := splitRandom(rng, in)

		whole, wholeBad := runGuard([][]byte{in})
		if want := guardOracle(oracle, in); wholeBad != want {
			t.Fatalf("stream %d %q: guard poison = %v, oracle = %v", n, in, wholeBad, want)
		}
		out, bad := runGuard(chunks)
		if bad != wholeBad || !bytes.Equal(out, whole) {
			t.Fatalf("stream %d %q split %q: chunked guard (%q, %v), whole (%q, %v)",
				n, in, chunks, out, bad, whole, wholeBad)
		}
		if wholeBad {
			continue
		}

		var g vtGuard
		check.Reset()
		executed, printed = executed[:0], printed[:0]
		for i, c := range in {
			one[0] = c
			var bad bool
			step, bad = g.filter(step[:0], one[:])
			if bad {
				t.Fatalf("stream %d %q: byte-wise guard poisons at %d", n, in, i)
			}
			for _, b := range step {
				check.Advance(b)
			}
			if g.st != parser.Utf8State && g.st != check.State() {
				t.Fatalf("stream %d %q after byte %d: guard state %v, parser %v",
					n, in, i, g.st, check.State())
			}
		}
		if i := slices.IndexFunc(executed, func(b byte) bool { return b >= 0x80 }); i >= 0 {
			t.Fatalf("stream %d %q: parser executed %#x", n, in, executed[i])
		}
		if !bytes.Contains(in, []byte("�")) && slices.Contains(printed, utf8.RuneError) {
			t.Fatalf("stream %d %q: parser printed U+FFFD", n, in)
		}

		if n%modelEvery == 0 {
			compareGuardedModels(t, rng, raw)
		}
	}
}

// compareGuardedModels feeds raw to predictModel.feed and its guard-filtered
// bytes to a second model in the same chunks (vt flushes graphemes per Write),
// and requires the same cells.
func compareGuardedModels(t *testing.T, rng *rand.Rand, raw []byte) {
	t.Helper()
	const w, h = 8, 3
	fed := newPredictModel(w, h, nil, plainModes(h))
	direct := newPredictModel(w, h, nil, plainModes(h))
	defer fed.close()
	defer direct.close()
	if fed == nil || direct == nil {
		t.Fatal("newPredictModel returned nil")
	}
	var title titleFilter
	var g vtGuard
	for _, c := range splitRandom(rng, raw) {
		fb, bad := g.filter(nil, title.filter(nil, c))
		if bad {
			t.Fatalf("stream %q: guard poisons a stream it passed whole", raw)
		}
		if !fed.feed(c) {
			t.Fatalf("stream %q: feed refused a stream the guard passes", raw)
		}
		if _, err := direct.e.Write(fb); err != nil {
			t.Fatal(err)
		}
	}
	for y := range h {
		for x := range w {
			a, b := fed.cell(x, y), direct.cell(x, y)
			if cellText(a) != cellText(b) || a.Width != b.Width {
				t.Fatalf("stream %q: cell (%d,%d) fed %+v, direct %+v", raw, x, y, a, b)
			}
		}
	}
	fx, fy := fed.cursor()
	dx, dy := direct.cursor()
	if fx != dx || fy != dy {
		t.Fatalf("stream %q: cursor fed (%d,%d), direct (%d,%d)", raw, fx, fy, dx, dy)
	}
}

// onKeys refuses to predict onto any cell whose Width is not 1.
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
