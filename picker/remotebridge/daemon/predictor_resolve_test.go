package daemon

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// echoStep is keys typed, or a remote payload, and the number of predictions
// outstanding after it (-1: any).
type echoStep struct {
	keys, out string
	pending   int
}

// runEchoSteps fails on any epoch bump: every script echoes what was typed.
func runEchoSteps(t *testing.T, h *predHarness, steps []echoStep) {
	t.Helper()
	e0 := h.p.epoch
	for i, s := range steps {
		if s.keys != "" {
			h.keys(s.keys)
		} else {
			h.output(s.out)
		}
		if h.p.epoch != e0 {
			t.Fatalf("step %d (%q%q): epoch %d -> %d, preds %+v", i, s.keys, s.out, e0, h.p.epoch, h.p.preds)
		}
		if s.pending >= 0 && len(h.p.preds) != s.pending {
			t.Fatalf("step %d (%q%q): %d preds outstanding, want %d: %+v", i, s.keys, s.out, len(h.p.preds), s.pending, h.p.preds)
		}
	}
}

// confirmedHarness is "$ g" with the cursor after g and its epoch confirmed.
func confirmedHarness(t *testing.T) *predHarness {
	t.Helper()
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("g")
	return h
}

func TestPredictorEchoResolution(t *testing.T) {
	cases := []struct {
		name  string
		steps []echoStep
	}{
		{"a BS b one per frame", []echoStep{
			{keys: "a\x7fb", pending: 3},
			{out: "a", pending: 2},
			{out: "\b \b", pending: 1},
			{out: "b", pending: 0},
		}},
		{"a then BS b coalesced", []echoStep{
			{keys: "a\x7fb", pending: 3},
			{out: "a", pending: 2},
			{out: "\b \bb", pending: 0},
		}},
		{"a BS b fully coalesced", []echoStep{
			{keys: "a\x7fb", pending: 3},
			{out: "a\b \bb", pending: 0},
		}},
		{"ab BS one per frame", []echoStep{
			{keys: "ab\x7f", pending: 3},
			{out: "a", pending: 2},
			{out: "b", pending: 1},
			{out: "\b \b", pending: 0},
			{keys: "c", pending: 1},
			{out: "c", pending: 0},
		}},
		{"ab BS, c typed before b echo", []echoStep{
			{keys: "ab\x7f", pending: 3},
			{out: "a", pending: 2},
			{keys: "c", pending: 3},
			{out: "b", pending: 2},
			{out: "\b \b", pending: 1},
			{out: "c", pending: 0},
		}},
		// b BS echoed in one payload looks exactly like neither echoed yet.
		{"ab BS fully coalesced then c", []echoStep{
			{keys: "ab\x7f", pending: 3},
			{out: "ab\b \b", pending: -1},
			{keys: "c", pending: -1},
			{out: "c", pending: 0},
		}},
		{"ab BS BS a one per frame", []echoStep{
			{keys: "ab\x7f\x7fa", pending: 5},
			{out: "a", pending: 4},
			{out: "b", pending: 3},
			{out: "\b \b", pending: 2},
			{out: "\b \b", pending: 1},
			{out: "a", pending: 0},
		}},
		{"char then erase coalesced, then d", []echoStep{
			{keys: "c\x7f", pending: 2},
			{out: "c\b \b", pending: -1},
			{keys: "d", pending: -1},
			{out: "d", pending: 0},
		}},
		{"erase then char coalesced", []echoStep{
			{keys: "\x7fx", pending: 2},
			{out: "\b \bx", pending: 0},
		}},
		{"fish: erase and char across a column-0 redraw", []echoStep{
			{keys: "\x7fx", pending: 2},
			{out: "\r\x1b[K", pending: 2},
			{out: "$ x\x1b[90mtra\x1b[m\x1b[1;4H", pending: 0},
		}},
		{"zsh autosuggestion refills the erased cell", []echoStep{
			{keys: "\x7f", pending: 1},
			{out: "\b\x1b[90mg\x1b[m\b", pending: 0},
		}},
		{"zsh autosuggestion right of the cursor", []echoStep{
			{keys: "it", pending: 2},
			{out: "i\x1b[90mt status\x1b[m\x1b[1;5H", pending: 1},
			{out: "t\x1b[90m status\x1b[m\x1b[1;6H", pending: 0},
		}},
		{"autosuggestion after a b BS, then c", []echoStep{
			{keys: "ab\x7f", pending: 3},
			{out: "ab\b \b\x1b[90mb\x1b[m\b", pending: -1},
			{keys: "c", pending: -1},
			{out: "c\x1b[K", pending: 0},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := confirmedHarness(t)
			runEchoSteps(t, h, c.steps)
			h.settled()
			if out := h.keys("z"); !bytes.Contains(out, []byte("\x1b[0;2mz")) {
				t.Errorf("next key = %q, want a faint z: epoch unconfirmed", out)
			}
		})
	}
}

// Output elsewhere on the screen between echoes leaves the cursor off the
// prediction's row; that is a pending echo, not a miss.
func TestPredictorMultiLinePrompt(t *testing.T) {
	h := newHarness(t, "top\r\n$ ", 2, 1, harnessOpts{})
	h.confirm("g")
	runEchoSteps(t, h, []echoStep{
		{keys: "x\x7fy", pending: 3},
		{out: "\x1b[1;1H\x1b[Ktop2", pending: 3},
		{out: "\x1b[2;4Hx", pending: 2},
		{out: "\x1b[1;1H\x1b[Ktop3", pending: 2},
		{out: "\x1b[2;4H\x1b[K", pending: 1},
		{out: "y", pending: 0},
	})
	h.settled()
}

func TestPredictorPasswordNeverConfirms(t *testing.T) {
	for _, keys := range []string{"c\x7f", "cd\x7f\x7f", "abc", "c\x7fd\x7f"} {
		t.Run(keys, func(t *testing.T) {
			h := newHarness(t, "Password: ", 10, 0, harnessOpts{})
			h.keys(keys)
			h.output("\x1b[m")
			h.output("")
			if h.p.confirmed != -1 {
				t.Errorf("confirmed = %d with nothing echoed", h.p.confirmed)
			}
			if out := h.keys("x"); out != nil {
				t.Errorf("key at a silent prompt painted %q", out)
			}
		})
	}
}

func TestPredictorRealMissStillMisses(t *testing.T) {
	for name, s := range map[string][2]string{
		"different char":        {"b", "c"},
		"erase then wrong char": {"\x7fb", "\b \bc"},
		"char erase char wrong": {"a\x7fb", "a\b \bc"},
	} {
		t.Run(name, func(t *testing.T) {
			h := confirmedHarness(t)
			e0 := h.p.epoch
			h.keys(s[0])
			h.output(s[1])
			if h.p.epoch == e0 || len(h.p.preds) != 0 {
				t.Errorf("epoch %d -> %d, preds %+v, want a miss", e0, h.p.epoch, h.p.preds)
			}
			h.settled()
		})
	}
}

func TestPredictorQuietExpiry(t *testing.T) {
	t.Run("net-zero tail echoed in one payload", func(t *testing.T) {
		h := confirmedHarness(t)
		h.keys("ab\x7f")
		h.output("ab\b \b")
		e0 := h.p.epoch
		h.clock = h.clock.Add(predictExpiryMax)
		h.expire()
		h.settled()
		if len(h.p.preds) != 0 || h.p.epoch != e0 {
			t.Fatalf("after expiry: epoch %d -> %d, preds %+v", e0, h.p.epoch, h.p.preds)
		}
		if out := h.keys("c"); !bytes.Contains(out, []byte("\x1b[0;2mc")) {
			t.Errorf("next key = %q, want a faint c", out)
		}
	})
	t.Run("silent prompt stays tentative", func(t *testing.T) {
		h := newHarness(t, "Password: ", 10, 0, harnessOpts{})
		h.keys("c\x7f")
		h.output("\x1b[m")
		h.clock = h.clock.Add(predictExpiryMax)
		h.expire()
		if out := h.keys("x"); out != nil || h.p.confirmed != -1 {
			t.Errorf("silent prompt: paint %q, confirmed %d", out, h.p.confirmed)
		}
	})
}

// A key and Enter echoed in one payload move the cursor off the key's row:
// the barrier's epoch bump drops the key rather than leave it pending there.
func TestPredictorBarrierCoalesced(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("l")
	h.keys("s")
	h.keys("\r")
	h.output("s\r\nfile1\r\n$ ")
	if len(h.p.preds) != 0 {
		t.Errorf("preds %+v after the barrier's echo", h.p.preds)
	}
	h.settled()
	h.output("\x1b[1;1H\x1b[2J$ ")
	h.settled()
}

// echoLine is a shell line editor after "$ ": it turns each key into the
// bytes its echo writes, in one of four styles.
type echoLine struct {
	style string
	buf   []byte
}

func (r *echoLine) key(c byte) string {
	if c == 0x7f {
		r.buf = r.buf[:len(r.buf)-1]
	} else {
		r.buf = append(r.buf, c)
	}
	switch r.style {
	case "tty":
		if c == 0x7f {
			return "\b \b"
		}
		return string(c)
	case "zle":
		if c == 0x7f {
			return "\b\x1b[K"
		}
		return string(c)
	case "autosuggest":
		sug := "\x1b[90mzzz\x1b[m\x1b[3D"
		if c == 0x7f {
			return "\b\x1b[K" + sug
		}
		return string(c) + "\x1b[K" + sug
	case "fish":
		return fmt.Sprintf("\r\x1b[K$ %s\x1b[90mqq\x1b[m\x1b[1;%dH", r.buf, 3+len(r.buf))
	}
	panic(r.style)
}

// TestPredictorFuzzEchoStyles types random char and backspace runs (never past
// the run start) in random chunks while the remote echoes them in random
// frames. Split at key boundaries, no echo is a miss and no tail left pending
// costs the epoch at expiry; split at any byte, a frame can end mid-echo and
// misses are legal, but the repair must still leave the remote's screen.
func TestPredictorFuzzEchoStyles(t *testing.T) {
	for _, style := range []string{"tty", "zle", "autosuggest", "fish"} {
		for _, byteSplit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/byteSplit=%v", style, byteSplit), func(t *testing.T) {
				for seed := range uint64(150) {
					trace := fuzzEcho(t, rand.New(rand.NewPCG(seed, 7)), style, byteSplit)
					if t.Failed() {
						t.Fatalf("seed %d:\n%s", seed, trace)
					}
				}
			})
		}
	}
}

func fuzzEcho(t *testing.T, rng *rand.Rand, style string, byteSplit bool) string {
	t.Helper()
	h := confirmedHarness(t)
	r := &echoLine{style: style, buf: []byte("g")}
	e0 := h.p.epoch
	var tr strings.Builder

	// The remote has processed done of the typed keys into stream, of which
	// sent bytes reached the predictor; bounds is the stream offset after
	// each processed key.
	var typed, stream []byte
	var bounds []int
	done, sent := 0, 0
	lineLen := 1
	nKeys := 2 + rng.IntN(10)

	for done < nKeys || sent < len(stream) {
		switch {
		case len(typed) < nKeys && rng.IntN(2) == 0:
			var chunk []byte
			for range 1 + rng.IntN(3) {
				if len(typed)+len(chunk) >= nKeys {
					break
				}
				if lineLen > 1 && rng.IntN(3) == 0 {
					chunk = append(chunk, 0x7f)
					lineLen--
				} else {
					chunk = append(chunk, "abc"[rng.IntN(3)])
					lineLen++
				}
			}
			if len(chunk) == 0 {
				continue
			}
			typed = append(typed, chunk...)
			fmt.Fprintf(&tr, "keys %q\n", chunk)
			h.keys(string(chunk))
		case done < len(typed) && rng.IntN(2) == 0:
			for range 1 + rng.IntN(len(typed)-done) {
				stream = append(stream, r.key(typed[done])...)
				done++
				bounds = append(bounds, len(stream))
			}
		case sent < len(stream):
			var end int
			if byteSplit {
				end = sent + 1 + rng.IntN(len(stream)-sent)
			} else {
				for _, b := range bounds {
					if b > sent {
						end = b
						if rng.IntN(2) == 0 {
							break
						}
					}
				}
			}
			frame := stream[sent:end]
			sent = end
			h.output(string(frame))
			fmt.Fprintf(&tr, "out  %q -> preds %d epoch %d\n", frame, len(h.p.preds), h.p.epoch)
		case len(typed) >= nKeys && done < len(typed):
			stream = append(stream, r.key(typed[done])...)
			done++
			bounds = append(bounds, len(stream))
		}
		if !byteSplit && h.p.epoch != e0 {
			t.Errorf("false miss: epoch %d -> %d", e0, h.p.epoch)
			return tr.String()
		}
	}
	h.clock = h.clock.Add(predictExpiryMax)
	h.expire()
	if !byteSplit && h.p.epoch != e0 {
		t.Errorf("expiry reset the epoch with every key echoed: %d -> %d", e0, h.p.epoch)
	}
	h.settled()
	return tr.String()
}
