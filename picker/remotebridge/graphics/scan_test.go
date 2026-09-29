package graphics

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
)

const (
	bareSeq    = "\x1b_Gi=31,a=T,U=1,f=100,t=f;L3RtcC94LnBuZw==\x1b\\"
	wrappedSeq = "\x1bPtmux;\x1b\x1b_Gi=31,a=T,U=1,f=100,t=f;L3RtcC94LnBuZw==\x1b\x1b\\\x1b\\"
)

func chunkKinds(cs []Chunk) string {
	var b bytes.Buffer
	for _, c := range cs {
		switch {
		case c.Seq != nil:
			b.WriteByte('S')
		case c.Raster != nil:
			b.WriteByte('R')
		default:
			b.WriteByte('L')
		}
	}
	return b.String()
}

func TestScanBareSequence(t *testing.T) {
	cs := NewScanner().Feed([]byte("before" + bareSeq + "after"))
	if got := chunkKinds(cs); got != "LSL" {
		t.Fatalf("kinds = %q, want LSL", got)
	}
	if got := string(cs[1].Seq.Keys); got != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("keys = %q", got)
	}
	if got := string(cs[1].Seq.Payload); got != "L3RtcC94LnBuZw==" {
		t.Fatalf("payload = %q", got)
	}
	if cs[1].Seq.Wrapped {
		t.Fatal("bare sequence reported as wrapped")
	}
}

func TestScanWrappedSequenceUndoublesEscapes(t *testing.T) {
	cs := NewScanner().Feed([]byte(wrappedSeq))
	if got := chunkKinds(cs); got != "S" {
		t.Fatalf("kinds = %q, want S", got)
	}
	if !cs[0].Seq.Wrapped {
		t.Fatal("wrapped sequence not flagged")
	}
	if got := string(cs[0].Seq.Keys); got != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("keys = %q", got)
	}
}

func TestScanSequenceSplitAcrossFeeds(t *testing.T) {
	s := NewScanner()
	cut := len(bareSeq) / 2
	if cs := s.Feed([]byte(bareSeq[:cut])); chunkKinds(cs) != "" {
		t.Fatalf("partial sequence emitted early: %q", chunkKinds(cs))
	}
	cs := s.Feed([]byte(bareSeq[cut:]))
	if chunkKinds(cs) != "S" {
		t.Fatalf("kinds = %q, want S", chunkKinds(cs))
	}
}

// A passthrough carrying something other than a graphics APC is complete, not
// partial: it must be forwarded at once, or every later byte of the pane queues
// behind it until the partial cap.
func TestScanCompleteNonGraphicsPassthroughForwardsVerbatim(t *testing.T) {
	const in = "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\tail"
	s := NewScanner()
	cs := s.Feed([]byte(in))
	var got []byte
	for _, c := range cs {
		if c.Seq != nil {
			t.Fatal("OSC 52 passthrough decoded as a graphics sequence")
		}
		got = append(got, c.Literal...)
	}
	if string(got) != in {
		t.Fatalf("forwarded %q, want byte-identical %q", got, in)
	}
	if len(s.held) != 0 {
		t.Fatalf("held %q, want nothing held", s.held)
	}
}

// A wrapper holding a kitty sequence plus trailing bytes is DROPPED, not
// forwarded whole.
//
// This assertion is the inverse of what it used to be. Forwarding preserved
// every byte the sender asked us to relay, which reads like the conservative
// choice — but among those bytes is a t=f payload that never passed the
// localiser, so the terminal is handed a path chosen by the far end. D7 already
// settles the direction for a store we cannot localise: drop it, because a
// missing image renders blank and self-heals where a wrong one renders wrong.
//
// The cost is that a legitimate multi-sequence wrapper loses its trailer too.
// Nothing produces one — tmuxPassthrough wraps exactly one sequence — and
// paying that uniformly beats teaching the scanner which payloads are dangerous.
func TestScanWrapperWithTrailingBytesIsDropped(t *testing.T) {
	const in = "\x1bPtmux;\x1b\x1b_Gi=1,a=T;abc\x1b\x1b\\extra\x1b\\"
	s := NewScanner()
	cs := s.Feed([]byte(in))
	if len(cs) != 0 {
		t.Fatalf("emitted %d chunk(s), want the wrapper dropped: %+v", len(cs), cs)
	}
	if s.Malformed != 1 {
		t.Fatalf("Malformed = %d, want 1 — a drop nobody can count is a silent drop", s.Malformed)
	}
	if len(s.held) != 0 {
		t.Fatalf("held %q, want nothing held", s.held)
	}
}

func TestScanRecoversAfterNonGraphicsPassthrough(t *testing.T) {
	cs := NewScanner().Feed([]byte("\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\" + bareSeq))
	if got := chunkKinds(cs); got != "LS" {
		t.Fatalf("kinds = %q, want LS", got)
	}
	if got := string(cs[1].Seq.Keys); got != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("keys = %q", got)
	}
}

// The mirror of the case above: an unterminated passthrough is genuinely
// partial and must still be held, not forwarded.
func TestScanIncompletePassthroughIsHeld(t *testing.T) {
	s := NewScanner()
	if cs := s.Feed([]byte("\x1bPtmux;\x1b\x1b_Gi=1,a=T;abc")); chunkKinds(cs) != "" {
		t.Fatalf("incomplete passthrough emitted early: %q", chunkKinds(cs))
	}
	cs := s.Feed([]byte("\x1b\x1b\\\x1b\\"))
	if chunkKinds(cs) != "S" {
		t.Fatalf("kinds = %q, want S", chunkKinds(cs))
	}
}

func TestScanLiteralBeforePartialIsEmittedImmediately(t *testing.T) {
	cs := NewScanner().Feed([]byte("visible\x1b_Gi=1,a=T;abc"))
	if chunkKinds(cs) != "L" || string(cs[0].Literal) != "visible" {
		t.Fatalf("kinds = %q, first = %q", chunkKinds(cs), cs[0].Literal)
	}
}

// Flush is the whole mechanism behind "held bytes are never silently
// swallowed", so both directions are pinned: a held partial comes back out as a
// literal, and a Scanner holding nothing stays quiet. The second call also
// proves Flush clears what it emitted — re-emitting would duplicate those bytes
// into the pane.
func TestFlushEmitsHeldPartialThenNothing(t *testing.T) {
	const partial = "\x1b_Gi=1,a=T;abc"
	s := NewScanner()
	if cs := s.Feed([]byte(partial)); chunkKinds(cs) != "" {
		t.Fatalf("partial emitted early: %q", chunkKinds(cs))
	}
	cs := s.Flush()
	if len(cs) == 0 || chunkKinds(cs) != "L" || string(cs[0].Literal) != partial {
		t.Fatalf("kinds = %q literal = %q, want L / %q", chunkKinds(cs), cs[0].Literal, partial)
	}
	if got := s.Flush(); got != nil {
		t.Fatalf("second Flush = %v, want nil", got)
	}
	if got := NewScanner().Flush(); got != nil {
		t.Fatalf("fresh Flush = %v, want nil", got)
	}
}

func TestScanOversizedPartialFlushesAsLiteral(t *testing.T) {
	s := NewScanner()
	s.Feed([]byte("\x1b_Gi=1,a=T;"))
	cs := s.Feed(bytes.Repeat([]byte("A"), maxPartial+1))
	if chunkKinds(cs) != "L" {
		t.Fatalf("kinds = %q, want L (give up, forward verbatim)", chunkKinds(cs))
	}
	// Giving up must not wedge the scanner: nothing stays held going forward,
	// and the very next Feed decodes an ordinary sequence like nothing happened.
	if len(s.held) != 0 {
		t.Fatalf("held %q after overflow, want nothing held", s.held)
	}
	if cs := s.Feed([]byte(bareSeq)); chunkKinds(cs) != "S" {
		t.Fatalf("kinds = %q, want S — scanner did not recover after the overflow", chunkKinds(cs))
	}
}

// TestScanPartialIntroducerAcrossFeeds proves indexSeqStart holds a buffer
// that ends partway through the apcStart or passStart introducer, the way its
// sibling indexSixelStart already holds a partial sixel prefix. A cut that
// lands inside the introducer (with no preceding literal, to isolate this
// from the ordinary split-sequence case already covered above) must emit
// nothing on the first Feed and reassemble into exactly one Seq chunk once
// the rest arrives — matching an unsplit baseline Feed of the same bytes.
func TestScanPartialIntroducerAcrossFeeds(t *testing.T) {
	tests := []struct {
		name    string
		full    string
		cut     int
		wrapped bool
	}{
		{name: "apc cut=1", full: bareSeq, cut: 1},
		{name: "apc cut=2", full: bareSeq, cut: 2},
		{name: "passthrough cut=1", full: tmuxPassthrough(bareSeq), cut: 1, wrapped: true},
		{name: "passthrough cut=2", full: tmuxPassthrough(bareSeq), cut: 2, wrapped: true},
		{name: "passthrough cut=3", full: tmuxPassthrough(bareSeq), cut: 3, wrapped: true},
		{name: "passthrough cut=4", full: tmuxPassthrough(bareSeq), cut: 4, wrapped: true},
		{name: "passthrough cut=5", full: tmuxPassthrough(bareSeq), cut: 5, wrapped: true},
		{name: "passthrough cut=6", full: tmuxPassthrough(bareSeq), cut: 6, wrapped: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := NewScanner().Feed([]byte(tt.full))
			if chunkKinds(baseline) != "S" {
				t.Fatalf("baseline kinds = %q, want S", chunkKinds(baseline))
			}

			s := NewScanner()
			buf := []byte(tt.full)
			if cs := s.Feed(buf[:tt.cut]); chunkKinds(cs) != "" {
				t.Fatalf("partial introducer emitted early: %q", chunkKinds(cs))
			}
			cs := s.Feed(buf[tt.cut:])
			if chunkKinds(cs) != "S" {
				t.Fatalf("kinds = %q, want S (introducer split at %d not reassembled)", chunkKinds(cs), tt.cut)
			}
			if cs[0].Seq.Wrapped != tt.wrapped {
				t.Fatalf("Wrapped = %v, want %v", cs[0].Seq.Wrapped, tt.wrapped)
			}
			if string(cs[0].Seq.Keys) != string(baseline[0].Seq.Keys) {
				t.Fatalf("Keys = %q, want %q", cs[0].Seq.Keys, baseline[0].Seq.Keys)
			}
			if string(cs[0].Seq.Payload) != string(baseline[0].Seq.Payload) {
				t.Fatalf("Payload = %q, want %q", cs[0].Seq.Payload, baseline[0].Seq.Payload)
			}
		})
	}
}

// TestScanLiteralNotHeldOnDivergence guards against the introducer-hold fix
// over-holding: bytes speculatively held as a possible apcStart/passStart
// introducer, but which turn out not to continue into one, must eventually
// surface as Literal rather than being silently dropped or held forever.
func TestScanLiteralNotHeldOnDivergence(t *testing.T) {
	const in = "\x1b[31mX"
	s := NewScanner()
	// "\x1b[" matches no prefix of apcStart ("\x1b_G") or passStart
	// ("\x1bPtmux;") past the first byte, so nothing should need holding here
	// even before the fix — this is the control case for the property.
	first := s.Feed([]byte(in[:2]))
	rest := s.Feed([]byte(in[2:]))
	got := concatLiterals(first) + concatLiterals(rest)
	if got != in {
		t.Fatalf("literals = %q, want byte-identical %q (held bytes lost or stuck)", got, in)
	}
	for _, c := range append(first, rest...) {
		if c.Seq != nil {
			t.Fatalf("unrelated CSI decoded as a graphics Seq: %+v", c)
		}
	}
}

// TestScanPartialIntroducerFlush pins Flush() behavior for every partial
// introducer sub-case in TestScanPartialIntroducerAcrossFeeds, held via Feed
// but never completed. In every case but one, Flush must return the held
// bytes byte-identical as a single Literal chunk — the same "never silently
// swallowed" contract TestFlushEmitsHeldPartialThenNothing pins for an
// ordinary partial APC. The one exception is passthrough cut=2, whose held
// bytes are exactly "\x1bP": isPartialSixel treats that as a possible partial
// sixel DCS (indistinguishable at 2 bytes from the start of a bare sixel
// introducer), and Flush's existing, deliberate sixel policy drops it. That
// is not something this fix changes or should change — it is pinned here
// explicitly so a future change to indexFixedStart doesn't accidentally
// alter it.
func TestScanPartialIntroducerFlush(t *testing.T) {
	tests := []struct {
		name string
		full string
		cut  int
	}{
		{name: "apc cut=1", full: bareSeq, cut: 1},
		{name: "apc cut=2", full: bareSeq, cut: 2},
		{name: "passthrough cut=1", full: tmuxPassthrough(bareSeq), cut: 1},
		{name: "passthrough cut=2", full: tmuxPassthrough(bareSeq), cut: 2},
		{name: "passthrough cut=3", full: tmuxPassthrough(bareSeq), cut: 3},
		{name: "passthrough cut=4", full: tmuxPassthrough(bareSeq), cut: 4},
		{name: "passthrough cut=5", full: tmuxPassthrough(bareSeq), cut: 5},
		{name: "passthrough cut=6", full: tmuxPassthrough(bareSeq), cut: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScanner()
			held := []byte(tt.full)[:tt.cut]
			if cs := s.Feed(append([]byte(nil), held...)); chunkKinds(cs) != "" {
				t.Fatalf("partial introducer emitted early: %q", chunkKinds(cs))
			}

			cs := s.Flush()
			if tt.name == "passthrough cut=2" {
				// held == "\x1bP" exactly: isPartialSixel's sixel-ambiguity
				// policy drops it, same as an ordinary partial sixel.
				if cs != nil {
					t.Fatalf("Flush = %+v, want nil (sixel-ambiguous \\x1bP is dropped)", cs)
				}
				return
			}
			if chunkKinds(cs) != "L" {
				t.Fatalf("kinds = %q, want L", chunkKinds(cs))
			}
			if string(cs[0].Literal) != string(held) {
				t.Fatalf("literal = %q, want byte-identical %q", cs[0].Literal, held)
			}
		})
	}
}

// tmuxPassthrough wraps inner the way EncodeWrapped / tmux's passthrough does:
// every ESC doubled, then a final ST.
func tmuxPassthrough(inner string) string {
	var b bytes.Buffer
	b.WriteString(passStart)
	for i := 0; i < len(inner); i++ {
		if inner[i] == 0x1b {
			b.WriteByte(0x1b)
		}
		b.WriteByte(inner[i])
	}
	b.WriteString(st)
	return b.String()
}

func concatLiterals(cs []Chunk) string {
	var b bytes.Buffer
	for _, c := range cs {
		if c.Seq != nil {
			continue
		}
		b.Write(c.Literal)
	}
	return b.String()
}

func TestScanDropsSixel(t *testing.T) {
	const (
		bareSixel    = "\x1bPq#0;2;100;0;0@@@@@@\x1b\\"
		chafaSixel   = "\x1bP0;1;0q\"1;1;40;40#0;2;100;0;0@@@@@@\x1b\\"
		decrqss      = "\x1bP$q\"q\x1b\\"
		osc52pass    = "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\"
		sixelPayload = "SIXELPAYLOAD"
	)

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			// A COMPLETE bare sixel is no longer the scanner's to drop: it is
			// reported verbatim and the layer above decides. What stays pinned
			// here is that not one of its bytes reaches a Literal.
			name: "bare sixel mid-literal is reported, not literal",
			run: func(t *testing.T) {
				cs := NewScanner().Feed([]byte("before" + bareSixel + "after"))
				if got := concatLiterals(cs); got != "beforeafter" {
					t.Fatalf("literals = %q, want beforeafter", got)
				}
				if chunkKinds(cs) != "LRL" {
					t.Fatalf("kinds = %q, want LRL", chunkKinds(cs))
				}
				if got := string(cs[1].Raster); got != bareSixel {
					t.Fatalf("raster = %q, want the sixel verbatim %q", got, bareSixel)
				}
			},
		},
		{
			name: "parametrized bare sixel (chafa)",
			run: func(t *testing.T) {
				cs := NewScanner().Feed([]byte("x" + chafaSixel + "y"))
				if got := concatLiterals(cs); got != "xy" {
					t.Fatalf("literals = %q, want xy", got)
				}
				if chunkKinds(cs) != "LRL" || string(cs[1].Raster) != chafaSixel {
					t.Fatalf("kinds = %q raster = %q", chunkKinds(cs), cs[1].Raster)
				}
			},
		},
		{
			// A wrapper routes the local tmux to tty_cmd_rawstring, which
			// neither positions nor clips, so a wrapped sixel keeps its drop
			// and is never reported as raster.
			name: "passthrough-wrapped sixel stays dropped",
			run: func(t *testing.T) {
				in := "pre" + tmuxPassthrough(bareSixel) + "post"
				cs := NewScanner().Feed([]byte(in))
				if got := concatLiterals(cs); got != "prepost" {
					t.Fatalf("literals = %q, want prepost", got)
				}
				if chunkKinds(cs) != "LL" {
					t.Fatalf("kinds = %q, want LL (wrapped sixel consumed)", chunkKinds(cs))
				}
			},
		},
		{
			name: "sixel split across feeds",
			run: func(t *testing.T) {
				s := NewScanner()
				// Mid-header: `\eP0;1` held, then `0q…ST` completes and drops.
				if cs := s.Feed([]byte("a\x1bP0;1")); concatLiterals(cs) != "a" || len(s.held) == 0 {
					t.Fatalf("mid-header: literals=%q held=%q", concatLiterals(cs), s.held)
				}
				if cs := s.Feed([]byte("0q" + sixelPayload + st)); concatLiterals(cs) != "" || len(s.held) != 0 {
					t.Fatalf("after header complete: literals=%q held=%q, want drop", concatLiterals(cs), s.held)
				}
				// Mid-payload: header+partial body held, rest completes and drops.
				partial := "\x1bPq" + sixelPayload[:4]
				if cs := s.Feed([]byte("b" + partial)); concatLiterals(cs) != "b" || len(s.held) == 0 {
					t.Fatalf("mid-payload: literals=%q held=%q", concatLiterals(cs), s.held)
				}
				if cs := s.Feed([]byte(sixelPayload[4:] + st + "c")); concatLiterals(cs) != "c" {
					t.Fatalf("after payload complete: literals=%q, want c", concatLiterals(cs))
				}
			},
		},
		{
			name: "truncated sixel over maxPartial drops payload",
			run: func(t *testing.T) {
				s := NewScanner()
				s.Feed([]byte("\x1bPq"))
				cs := s.Feed(bytes.Repeat([]byte("Z"), maxPartial+1))
				got := concatLiterals(cs)
				if bytes.Contains([]byte(got), []byte("Z")) {
					t.Fatalf("overflow forwarded sixel payload: %q", got)
				}
				if len(s.held) != 0 {
					t.Fatalf("held %q after sixel overflow, want nothing", s.held)
				}
				// The scanner is now discarding the corrupt sequence's tail
				// until the next ESC, so the "ok" ahead of it goes too — the
				// accepted, bounded cost of not painting the body as text. The
				// sequence itself still decodes, which is what proves the
				// discard does not latch.
				if cs := s.Feed([]byte("ok" + bareSeq)); concatLiterals(cs) != "" || chunkKinds(cs) != "S" {
					t.Fatalf("kinds=%q literals=%q, want S — scanner did not recover", chunkKinds(cs), concatLiterals(cs))
				}
			},
		},
		{
			name: "Flush drops held partial sixel",
			run: func(t *testing.T) {
				s := NewScanner()
				if cs := s.Feed([]byte("\x1bP0;1;0q" + sixelPayload)); chunkKinds(cs) != "" {
					t.Fatalf("partial sixel emitted early: %q", chunkKinds(cs))
				}
				if cs := s.Flush(); cs != nil {
					t.Fatalf("Flush = %v, want nil (drop partial sixel)", cs)
				}
				if got := s.Flush(); got != nil {
					t.Fatalf("second Flush = %v, want nil", got)
				}
			},
		},
		{
			name: "non-sixel DCS (DECRQSS) forwarded verbatim",
			run: func(t *testing.T) {
				in := "a" + decrqss + "b"
				cs := NewScanner().Feed([]byte(in))
				if got := concatLiterals(cs); got != in {
					t.Fatalf("forwarded %q, want byte-identical %q", got, in)
				}
			},
		},
		{
			name: "non-sixel passthrough (OSC 52) forwarded verbatim",
			run: func(t *testing.T) {
				cs := NewScanner().Feed([]byte(osc52pass + "tail"))
				if got := concatLiterals(cs); got != osc52pass+"tail" {
					t.Fatalf("forwarded %q, want OSC 52 + tail", got)
				}
				for _, c := range cs {
					if c.Seq != nil {
						t.Fatal("OSC 52 decoded as graphics Seq")
					}
				}
			},
		},
		{
			name: "kitty APC bare and wrapped still decoded",
			run: func(t *testing.T) {
				cs := NewScanner().Feed([]byte(bareSeq + wrappedSeq))
				if chunkKinds(cs) != "SS" {
					t.Fatalf("kinds = %q, want SS", chunkKinds(cs))
				}
				if cs[0].Seq.Wrapped || !cs[1].Seq.Wrapped {
					t.Fatalf("wrapped flags = %v/%v, want false/true", cs[0].Seq.Wrapped, cs[1].Seq.Wrapped)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// R10 — the overflow tail leak. The overflow arm dropped the partial sixel's
// held prefix and left nothing behind, so the sequence's continuation — which
// carries no ESC — was emitted as Literal on the next Feed and painted on the
// mirror pane as text. That is #319's exact symptom, at a size (>64 KiB) no
// earlier test reached: the smallest real chafa sixel measured is 478 KB, so it
// fires on every real image.
func TestScanOverflowedSixelTailNeverLeaks(t *testing.T) {
	body := bytes.Repeat([]byte("~"), maxPartial+1)
	tail := bytes.Repeat([]byte("~"), 4096)

	tests := []struct {
		name  string
		head  string
		close string
	}{
		{name: "bare", head: "\x1bPq", close: st},
		// The wrapped form leaks identically: its outer terminator is a lone
		// \e\\ while the payload's own ESCs are doubled.
		{name: "wrapped", head: passStart + "\x1b\x1bPq", close: "\x1b\x1b\\" + st},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScanner()
			if got := concatLiterals(s.Feed([]byte(tt.head))); got != "" {
				t.Fatalf("head leaked %q", got)
			}
			if got := concatLiterals(s.Feed(body)); got != "" {
				t.Fatalf("overflow leaked %q", got)
			}
			cs := s.Feed(append(append([]byte(nil), tail...), tt.close...))
			if got := concatLiterals(cs); got != "" {
				t.Fatalf("post-overflow tail leaked %d byte(s) as literal: %.40q…", len(got), got)
			}
			// The discard must not latch: the very next sequence still decodes.
			if cs := s.Feed([]byte("ok" + bareSeq)); chunkKinds(cs) != "LS" || concatLiterals(cs) != "ok" {
				t.Fatalf("kinds=%q literals=%q, want LS / ok — discard latched", chunkKinds(cs), concatLiterals(cs))
			}
		})
	}
}

// The discard state must not swallow the sequence that ends it. A bare ESC
// introduces whatever follows, so it is left for the scanner; only the corrupt
// sequence's own ST is consumed as a terminator.
func TestScanDiscardExitsAtNextEscWithoutConsumingIt(t *testing.T) {
	body := bytes.Repeat([]byte("~"), maxPartial+1)

	// Exit on a foreign introducer: the ST never arrives at all, which is the
	// case a dropped sink frame produces.
	s := NewScanner()
	s.Feed([]byte("\x1bPq"))
	s.Feed(body)
	cs := s.Feed([]byte("~~~" + bareSeq + "tail"))
	if chunkKinds(cs) != "SL" || concatLiterals(cs) != "tail" {
		t.Fatalf("kinds=%q literals=%q, want SL / tail", chunkKinds(cs), concatLiterals(cs))
	}

	// Exit on the sequence's own ST: it is the terminator and is consumed, so
	// the text after it survives whole.
	s = NewScanner()
	s.Feed([]byte("\x1bPq"))
	s.Feed(body)
	cs = s.Feed([]byte("~~~" + st + "after"))
	if chunkKinds(cs) != "L" || concatLiterals(cs) != "after" {
		t.Fatalf("kinds=%q literals=%q, want L / after", chunkKinds(cs), concatLiterals(cs))
	}
}

// An ESC landing on a Feed boundary cannot be classified yet. Re-arming on it
// would hand the next Feed a headless sequence; emitting it would paint a
// stray ESC. It is held, and the discard stays armed.
func TestScanDiscardHoldsTrailingEscAcrossFeeds(t *testing.T) {
	s := NewScanner()
	s.Feed([]byte("\x1bPq"))
	s.Feed(bytes.Repeat([]byte("~"), maxPartial+1))
	if cs := s.Feed([]byte("~~\x1b")); len(cs) != 0 {
		t.Fatalf("emitted %+v across the boundary, want nothing", cs)
	}
	if string(s.held) != "\x1b" || s.discard == discardOff {
		t.Fatalf("held=%q discard=%v, want the ESC held and the discard armed", s.held, s.discard)
	}
	if cs := s.Feed([]byte("\\ok")); chunkKinds(cs) != "L" || concatLiterals(cs) != "ok" {
		t.Fatalf("kinds=%q literals=%q, want L / ok", chunkKinds(cs), concatLiterals(cs))
	}
}

// Flush mid-discard must drop, not emit: whatever is held there is the tail of
// a corrupt raster sequence, never text.
func TestFlushMidDiscardDropsAndDisarms(t *testing.T) {
	s := NewScanner()
	s.Feed([]byte("\x1bPq"))
	s.Feed(bytes.Repeat([]byte("~"), maxPartial+1))
	s.Feed([]byte("~~\x1b"))
	if cs := s.Flush(); cs != nil {
		t.Fatalf("Flush = %+v, want nil", cs)
	}
	if s.discard != discardOff {
		t.Fatal("Flush left the discard armed")
	}
	if cs := s.Feed([]byte("ok" + bareSeq)); chunkKinds(cs) != "LS" || concatLiterals(cs) != "ok" {
		t.Fatalf("kinds=%q literals=%q, want LS / ok", chunkKinds(cs), concatLiterals(cs))
	}
}

// The budget decides WHEN a partial raster is given up on, never what happens
// to it: the same input drops below the budget and completes above it, and the
// drop is a discard either way.
func TestScanRasterHoldBudget(t *testing.T) {
	// Three times the default budget, so the overflow arm is genuinely
	// reached: a body that fits in the last piece alongside its ST arrives as
	// one complete sequence and never overflows anything.
	sixel := "\x1bPq" + string(bytes.Repeat([]byte("~"), 3*maxPartial)) + st

	t.Run("above the budget the partial is discarded", func(t *testing.T) {
		s := NewScanner()
		var got []Chunk
		for i := 0; i < len(sixel); i += 4096 {
			got = append(got, s.Feed([]byte(sixel[i:min(i+4096, len(sixel))]))...)
		}
		if len(got) != 0 {
			t.Fatalf("emitted %q, want the oversized sixel discarded", chunkKinds(got))
		}
	})

	t.Run("within a raised budget the sixel completes", func(t *testing.T) {
		s := NewScanner()
		s.SetRasterHold(1 << 20)
		var got []Chunk
		for i := 0; i < len(sixel); i += 4096 {
			got = append(got, s.Feed([]byte(sixel[i:min(i+4096, len(sixel))]))...)
		}
		if chunkKinds(got) != "R" {
			t.Fatalf("kinds = %q, want R", chunkKinds(got))
		}
		if string(got[0].Raster) != sixel {
			t.Fatalf("raster is not byte-identical (%d bytes, want %d)", len(got[0].Raster), len(sixel))
		}
	})

	// The raster budget is the bare sixel's alone. A kitty APC keeps
	// maxPartial, whose second job is self-healing a stuck ordinary escape by
	// forwarding it verbatim.
	t.Run("a raised budget does not widen a kitty hold", func(t *testing.T) {
		s := NewScanner()
		s.SetRasterHold(1 << 20)
		s.Feed([]byte("\x1b_Gi=1,a=T;"))
		if cs := s.Feed(bytes.Repeat([]byte("A"), maxPartial+1)); chunkKinds(cs) != "L" {
			t.Fatalf("kinds = %q, want L (forwarded verbatim at maxPartial)", chunkKinds(cs))
		}
	})
}

// Feed's contract is that it does not retain p. The hold is amortised by
// retaining the concatenation buffer rather than re-copying it, and that
// buffer is the scanner's only when it was built from held bytes — so the
// no-hold case must still copy. Mutating p after every Feed is what pins it.
func TestFeedDoesNotRetainCallerBuffer(t *testing.T) {
	s := NewScanner()
	first := []byte("\x1b_Gi=31,a=T,U=1,f=100,t=f;L3Rt")
	s.Feed(first)
	for i := range first {
		first[i] = 'X'
	}
	second := []byte("cC94LnBuZw==")
	s.Feed(second)
	for i := range second {
		second[i] = 'Y'
	}
	cs := s.Feed([]byte(st))
	if chunkKinds(cs) != "S" {
		t.Fatalf("kinds = %q, want S", chunkKinds(cs))
	}
	if got := string(cs[0].Seq.Keys); got != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("keys = %q — Feed retained the caller's buffer", got)
	}
	if got := string(cs[0].Seq.Payload); got != "L3RtcC94LnBuZw==" {
		t.Fatalf("payload = %q — Feed retained the caller's buffer", got)
	}
}

// The hold must be amortised O(1) per byte. It used to re-copy everything held
// on every call, which at a multi-megabyte raster delivered in kilobyte %output
// lines is O(N x size) of memcpy on the pane's own pump goroutine — the very
// goroutine that must also drain that pane's frames.
func TestFeedHoldIsAmortised(t *testing.T) {
	const (
		size  = 2 << 20
		piece = 4096
	)
	sixel := append([]byte("\x1bPq"), bytes.Repeat([]byte("~"), size)...)
	sixel = append(sixel, st...)

	s := NewScanner()
	s.SetRasterHold(8 << 20)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var got []Chunk
	for i := 0; i < len(sixel); i += piece {
		got = append(got, s.Feed(sixel[i:min(i+piece, len(sixel))])...)
	}
	runtime.ReadMemStats(&after)

	if len(got) == 0 || chunkKinds(got) != "R" || len(got[0].Raster) != len(sixel) {
		t.Fatalf("kinds = %q, want one complete R", chunkKinds(got))
	}
	// Quadratic re-copy of a 2 MiB hold in 4 KiB pieces allocates ~1 GB; the
	// amortised hold allocates a small multiple of the payload.
	const budget = 64 << 20
	if n := after.TotalAlloc - before.TotalAlloc; n > budget {
		t.Fatalf("allocated %d bytes holding %d, want under %d — the hold is re-copying", n, len(sixel), budget)
	}
}

// The held-raster path must be amortised in SCAN work, not only in memcpy
// (TestFeedHoldIsAmortised). Feed used to re-search the whole hold from byte 0
// on every call — two whole-buffer bytes.Index passes hunting the sequence
// start, then another hunting the terminator — which at a 16 MiB budget
// delivered in kilobyte %output lines is over a second of CPU on the pane's own
// pump goroutine, the one that must also drain that pane's frames.
//
// The assertion is the SCALING, not a wall clock, which would be flaky on a
// loaded box: 4x the input costs ~4x linear and ~16x quadratic, so 8x separates
// them with room to spare. Each figure is the best of three runs, so a
// scheduling spike lengthens a run instead of failing the test.

func TestFeedHoldScanIsLinear(t *testing.T) {
	const piece = 4096
	body := bytes.Repeat([]byte("~"), 4<<20)
	sixel := append([]byte("\x1bPq"), body...)
	sixel = append(sixel, st...)

	s := NewScanner()
	s.SetRasterHold(32 << 20)
	var got []Chunk
	searched := 0
	for i := 0; i < len(sixel); i += piece {
		resume := s.rasterScanned
		got = append(got, s.Feed(sixel[i:min(i+piece, len(sixel))])...)
		if len(s.held) == 0 {
			continue // the raster completed on this Feed; nothing is carried
		}
		// The ST search runs from the carried resume to the end of the hold, so
		// this is exactly the bytes this Feed examined.
		searched += len(s.held) - resume
	}
	if len(got) == 0 || chunkKinds(got) != "R" || len(got[0].Raster) != len(sixel) {
		t.Fatalf("kinds = %q, want one complete R", chunkKinds(got))
	}
	// Linear: every byte is searched once, plus the len(st)-1 overlap each Feed
	// re-examines so a terminator split across the boundary is still found. A
	// hold re-scanned from the start instead would search ~n^2/piece — for this
	// input over 500x the bound.
	if limit := 2 * len(sixel); searched > limit {
		t.Fatalf("searched %d bytes for a %d byte raster (limit %d) — the hold is re-scanning",
			searched, len(sixel), limit)
	}
}

// The resume a growing raster hold carries must overlap the previous Feed by
// len(st)-1 bytes. Without the overlap a terminator split across the boundary
// is searched past, the raster never completes, and at a raised budget it dies
// in the overflow discard — the image silently vanishing, which is exactly the
// failure the resume was added to prevent.
func TestScanRasterResumeStraddlesTerminator(t *testing.T) {
	body := bytes.Repeat([]byte("~"), 12<<10)
	whole := append([]byte("\x1bPq"), body...)
	whole = append(whole, st...)

	// tailIn is how many of the ST's bytes fall in the FINAL Feed: 1 puts the
	// split exactly on the resume boundary, the case the overlap exists for.
	for _, tailIn := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("st %d/%d across the boundary", len(st)-tailIn, tailIn), func(t *testing.T) {
			cut := len(whole) - tailIn
			s := NewScanner()
			s.SetRasterHold(1 << 20)
			var got []Chunk
			for i := 0; i < cut; i += 4096 {
				got = append(got, s.Feed(whole[i:min(i+4096, cut)])...)
			}
			got = append(got, s.Feed(append(append([]byte(nil), whole[cut:]...), "tail"...))...)
			if chunkKinds(got) != "RL" {
				t.Fatalf("kinds = %q, want RL", chunkKinds(got))
			}
			if !bytes.Equal(got[0].Raster, whole) {
				t.Fatalf("raster is %d bytes, want %d byte-identical", len(got[0].Raster), len(whole))
			}
			if lit := concatLiterals(got); lit != "tail" {
				t.Fatalf("literals = %q, want tail — the resume over- or under-consumed", lit)
			}
		})
	}
}

// The mirror of the terminator case: an introducer split across Feeds. A
// `\eP[0-9;]*` whose `q` has not arrived is not a confirmed head, so no resume
// may be carried for it — one carried too early would skip the header walk's
// own bytes and read the introducer as body.
func TestScanRasterResumeStraddlesIntroducer(t *testing.T) {
	const intro = "\x1bP0;1q"
	body := string(bytes.Repeat([]byte("~"), 8<<10))
	whole := "text" + intro + body + st

	// From just past the introducer's ESC (a lone trailing ESC is forwarded as
	// literal by indexSixelStart, which predates the resume) to just past `q`.
	for cut := len("text") + 2; cut <= len("text")+len(intro); cut++ {
		t.Run(fmt.Sprintf("cut at %d", cut), func(t *testing.T) {
			s := NewScanner()
			s.SetRasterHold(1 << 20)
			got := s.Feed([]byte(whole[:cut]))
			for i := cut; i < len(whole); i += 4096 {
				got = append(got, s.Feed([]byte(whole[i:min(i+4096, len(whole))]))...)
			}
			if chunkKinds(got) != "LR" {
				t.Fatalf("kinds = %q, want LR", chunkKinds(got))
			}
			if lit := concatLiterals(got); lit != "text" {
				t.Fatalf("literals = %q, want text", lit)
			}
			if string(got[1].Raster) != intro+body+st {
				t.Fatalf("raster is %d bytes, want %d byte-identical", len(got[1].Raster), len(intro+body+st))
			}
		})
	}
}

// R3 — a complete OSC 1337 File= is dropped whole, and counted.
func TestScanDropsOSC1337File(t *testing.T) {
	const seq = "\x1b]1337;File=name=YQ==;size=1:aGVsbG8=\x07"
	s := NewScanner()
	cs := s.Feed([]byte("before" + seq + "after"))
	if got := concatLiterals(cs); got != "beforeafter" {
		t.Fatalf("literals = %q, want beforeafter", got)
	}
	for _, c := range cs {
		if c.Seq != nil || c.Raster != nil {
			t.Fatalf("File= decoded as something other than a drop: %+v", cs)
		}
	}
	if s.InlineImage != 1 {
		t.Fatalf("InlineImage = %d, want 1", s.InlineImage)
	}
}

// R3's four exits: ST and BEL terminate and are consumed, CAN and SUB abort
// and are consumed — keyneg's regionOSC classification (strip.go) is the
// in-repo statement of this set.
func TestScanOSC1337FileConsumingExits(t *testing.T) {
	const body = "\x1b]1337;File=abc"
	tests := []struct {
		name string
		exit string
	}{
		{"ST", st},
		{"BEL", "\x07"},
		{"CAN", "\x18"},
		{"SUB", "\x1a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScanner()
			cs := s.Feed([]byte(body + tt.exit + "after"))
			if got := concatLiterals(cs); got != "after" {
				t.Fatalf("literals = %q, want after — the exit must be consumed with the sequence", got)
			}
			if s.InlineImage != 1 {
				t.Fatalf("InlineImage = %d, want 1", s.InlineImage)
			}
		})
	}
}

// R3's fifth exit, a bare ESC, is different in kind from the other four: it is
// NOT consumed, because it introduces whatever comes next. Swallowing it would
// destroy the following sequence.
func TestScanOSC1337FileBareESCNotConsumed(t *testing.T) {
	s := NewScanner()
	cs := s.Feed([]byte("\x1b]1337;File=abc" + bareSeq))
	if chunkKinds(cs) != "S" {
		t.Fatalf("kinds = %q, want S — the ESC introducing the next sequence must survive", chunkKinds(cs))
	}
	if got := string(cs[0].Seq.Keys); got != "i=31,a=T,U=1,f=100,t=f" {
		t.Fatalf("keys = %q — the following sequence was corrupted", got)
	}
	if s.InlineImage != 1 {
		t.Fatalf("InlineImage = %d, want 1", s.InlineImage)
	}
}

// The regression this scoping exists to prevent: every other OSC 1337 verb —
// routine iTerm2/WezTerm shell-integration output a remote shell emits
// constantly — must keep forwarding verbatim, byte-identical, uncounted.
func TestScanOSC1337OtherVerbsForwardVerbatim(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"CurrentDir", "\x1b]1337;CurrentDir=/tmp\x07"},
		{"SetUserVar", "\x1b]1337;SetUserVar=foo=YmFy\x07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScanner()
			cs := s.Feed([]byte(tt.in + "tail"))
			if got := concatLiterals(cs); got != tt.in+"tail" {
				t.Fatalf("forwarded %q, want byte-identical %q", got, tt.in+"tail")
			}
			for _, c := range cs {
				if c.Seq != nil || c.Raster != nil {
					t.Fatalf("non-File verb decoded as something other than literal: %+v", cs)
				}
			}
			if s.InlineImage != 0 {
				t.Fatalf("InlineImage = %d, want 0 — not a File= verb", s.InlineImage)
			}
		})
	}
}

// The undecided-prefix rule (R3): an "\x1b]" prefix at a Feed boundary is held
// until the discriminator resolves or is ruled out, and the resolution — drop
// or forward — happens on the Feed call that supplies enough bytes to decide,
// not deferred to Flush.
func TestScanOSC1337PrefixHeldAcrossFeedBoundaryResolves(t *testing.T) {
	t.Run("resolves into a drop", func(t *testing.T) {
		s := NewScanner()
		if cs := s.Feed([]byte("before\x1b]1337;Fil")); concatLiterals(cs) != "before" {
			t.Fatalf("literals = %q, want before (prefix held)", concatLiterals(cs))
		}
		if len(s.held) == 0 {
			t.Fatal("undecided prefix not held")
		}
		cs := s.Feed([]byte("e=abc" + st + "after"))
		if got := concatLiterals(cs); got != "after" {
			t.Fatalf("literals = %q, want after (File= dropped)", got)
		}
		if s.InlineImage != 1 {
			t.Fatalf("InlineImage = %d, want 1", s.InlineImage)
		}
	})

	t.Run("resolves into a forward once ruled out", func(t *testing.T) {
		s := NewScanner()
		if cs := s.Feed([]byte("before\x1b]1337;Fil")); concatLiterals(cs) != "before" {
			t.Fatalf("literals = %q, want before (prefix held)", concatLiterals(cs))
		}
		const rest = "eNotIt=xyz\x07after"
		cs := s.Feed([]byte(rest))
		want := "\x1b]1337;Fil" + rest
		if got := concatLiterals(cs); got != want {
			t.Fatalf("literals = %q, want byte-identical %q", got, want)
		}
		if s.InlineImage != 0 {
			t.Fatalf("InlineImage = %d, want 0 — ruled out, not File=", s.InlineImage)
		}
	})

	t.Run("still ambiguous at Flush is forwarded verbatim", func(t *testing.T) {
		s := NewScanner()
		const held = "\x1b]1337;Fil"
		if cs := s.Feed([]byte(held)); len(cs) != 0 {
			t.Fatalf("emitted %+v early, want the prefix held", cs)
		}
		cs := s.Flush()
		if chunkKinds(cs) != "L" || string(cs[0].Literal) != held {
			t.Fatalf("Flush = %+v, want the ambiguous prefix forwarded verbatim (%q)", cs, held)
		}
	})
}

// A confirmed File= cut short — even well under the overflow budget — is
// dropped, not leaked: "forward at Flush" is only for a prefix that never
// resolved, never for one the scanner already knows is File=.
func TestFlushDropsConfirmedOSC1337FileWithNoTerminator(t *testing.T) {
	s := NewScanner()
	const held = "\x1b]1337;File=abc"
	if cs := s.Feed([]byte(held)); len(cs) != 0 {
		t.Fatalf("emitted %+v early", cs)
	}
	if cs := s.Flush(); cs != nil {
		t.Fatalf("Flush = %+v, want nil (drop the confirmed File= body)", cs)
	}
}

// R3's own overflow bound, the OSC1337 analogue of R10: a confirmed File=
// whose terminator never arrives before the non-relay budget must not leak
// its body as text.
func TestScanOverflowedOSC1337FileTailNeverLeaks(t *testing.T) {
	body := bytes.Repeat([]byte("A"), maxPartial+1)
	tail := bytes.Repeat([]byte("A"), 4096)

	s := NewScanner()
	if got := concatLiterals(s.Feed([]byte("\x1b]1337;File="))); got != "" {
		t.Fatalf("head leaked %q", got)
	}
	if got := concatLiterals(s.Feed(body)); got != "" {
		t.Fatalf("overflow leaked %q", got)
	}
	cs := s.Feed(append(append([]byte(nil), tail...), st...))
	if got := concatLiterals(cs); got != "" {
		t.Fatalf("post-overflow tail leaked %d byte(s) as literal: %.40q…", len(got), got)
	}
	// The discard must not latch: the very next sequence still decodes.
	if cs := s.Feed([]byte("ok" + bareSeq)); chunkKinds(cs) != "LS" || concatLiterals(cs) != "ok" {
		t.Fatalf("kinds=%q literals=%q, want LS / ok — discard latched", chunkKinds(cs), concatLiterals(cs))
	}
	if s.InlineImage != 1 {
		t.Fatalf("InlineImage = %d, want 1", s.InlineImage)
	}
}
