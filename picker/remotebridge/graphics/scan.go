// Package graphics localises kitty graphics sequences crossing the remote
// bridge. A store sent by a program on the remote host references a file by
// path (t=f), which the LOCAL terminal cannot read; this package rewrites that
// payload to a local copy. The placement half needs nothing: kitty unicode
// placeholders are ordinary grid text and already cross the bridge.
package graphics

import "bytes"

// maxPartial bounds the bytes held waiting for a sequence terminator. A frame
// dropped by the sink's bounded buffer can truncate a sequence mid-flight, and
// without a cap the scanner would swallow the pane's stream forever waiting for
// an ST that was already discarded. On overflow the held bytes are forwarded
// verbatim: a garbled escape beats a dead pane — except a partial sixel, whose
// printable payload is the damaging part, so that is discarded instead.
//
// It stays the bound for every hold but a bare partial sixel, which is the one
// hold that may end in a sequence worth relaying (see Scanner.holdLimit).
const maxPartial = 64 << 10

const (
	apcStart  = "\x1b_G"
	passStart = "\x1bPtmux;"
	dcsStart  = "\x1bP"
	oscIntro  = "\x1b]"
	st        = "\x1b\\"
)

const (
	bel = 0x07
	can = 0x18
	sub = 0x1a
)

// osc1337FilePrefix is the OSC 1337 inline-image verb (R3) — the only OSC 1337
// verb this scanner drops. Everything else in the namespace (CurrentDir=,
// SetUserVar=, RemoteHost=, …) is routine shell-integration output and keeps
// forwarding verbatim. 12 bytes, verified by len() — not 13.
const osc1337FilePrefix = oscIntro + "1337;File="

// Chunk is one piece of a pane's byte stream: a literal run to forward
// untouched, one decoded graphics sequence, or one complete raster image.
// Exactly one of Literal, Seq and Raster is non-nil.
//
// Raw carries the sequence's verbatim input bytes and is set with Seq. A
// consumer that forwards a sequence unchanged must use it rather than
// re-encoding: Encode/EncodeWrapped render the canonical form, which for a
// bare input would gain a wrapper it never had.
type Chunk struct {
	Literal []byte
	Seq     *Seq
	Raw     []byte
	Raster  []byte
}

// discardMode says how the scanner is consuming the tail of a raster sequence
// it has already given up on. Off is the normal scanning state.
type discardMode int

const (
	discardOff discardMode = iota
	discardBare
	discardWrapped
	discardOSC
)

// Scanner splits a pane's byte stream into Chunks across successive Feed calls,
// holding an incomplete trailing sequence until the rest arrives.
//
// Malformed counts kitty sequences dropped because they could not be decoded
// whole (dropMalformed). InlineImage counts OSC 1337 File= sequences dropped
// (R3), whole or partial. Both exported so the proxy can report them: a drop
// emits no chunk at all, and a security-relevant one nobody can observe is how
// a bridge failure turns into an afternoon of guessing.
type Scanner struct {
	held    []byte
	discard discardMode
	// rasterHold caps a bare partial sixel; <= 0 means maxPartial.
	rasterHold int
	// rasterScanned is how far into held the ST search already reached, so a
	// growing raster hold is searched once per byte rather than once per Feed
	// (rasterResumeFor). Set only for a bare partial sixel; 0 otherwise.
	rasterScanned int
	Malformed     int
	InlineImage   int
}

func NewScanner() *Scanner { return &Scanner{} }

// SetRasterHold sets the byte budget for holding a bare partial sixel until its
// terminator arrives. n <= 0 restores the default, maxPartial.
//
// This is the scanner's only configurable input, and it is deliberately just a
// number: overflow of the budget, Flush, and the remainder of the sequence
// after an overflow all end in a drop at EVERY value of n. The budget decides
// when a partial raster is given up on, never what happens to it.
func (s *Scanner) SetRasterHold(n int) { s.rasterHold = n }

// Feed consumes p and returns every chunk that completed. Bytes belonging to a
// sequence whose terminator hasn't arrived are retained for the next call.
//
// Feed does not retain p; the caller may reuse the buffer once it returns. A
// Scanner is single-goroutine — one per pane.
func (s *Scanner) Feed(p []byte) []Chunk {
	buf := p
	// owned says whether buf's backing array is the scanner's. Concatenating
	// onto held bytes copies p into an array we own, so only the no-hold case
	// aliases the caller — and that is the only case that must copy before
	// retaining. Re-copying the whole hold on every call is quadratic memcpy
	// at a multi-megabyte raster hold delivered in kilobyte %output lines,
	// on the very goroutine that must also drain that pane's frames.
	owned := false
	// scanned carries the previous Feed's ST-search progress into the hold it
	// belongs to. It describes buf's head, so only the loop's first pass may
	// use it; taking it into a per-pass resume zeroes it for every later one,
	// each of which has consumed bytes and moved that head.
	scanned := s.rasterScanned
	s.rasterScanned = 0
	if len(s.held) > 0 {
		buf = append(s.held, p...)
		s.held = nil
		owned = true
	}
	out := []Chunk{}
	for len(buf) > 0 {
		resume := scanned
		scanned = 0
		if s.discard != discardOff {
			n, exited := s.consumeDiscard(buf)
			buf = buf[n:]
			if !exited {
				s.hold(buf, owned)
				return out
			}
			continue
		}
		if resume == 0 {
			i := indexSeqStart(buf)
			if i < 0 {
				out = appendLiteral(out, buf)
				return out
			}
			if i > 0 {
				out = appendLiteral(out, buf[:i])
				buf = buf[i:]
			}
		}
		// A non-zero resume is only ever set for a confirmed bare sixel head,
		// which sits at buf[0] — so indexSeqStart would answer 0, and skipping
		// it is what keeps the two whole-buffer bytes.Index passes it runs off
		// the growing hold.
		seq, n, drop := decodeSeq(buf, resume)
		switch {
		case n == 0:
			// Incomplete: hold it, unless it has outgrown its budget.
			if len(buf) > s.holdLimit(buf) {
				if mode := partialSixelDiscard(buf); mode != discardOff {
					// A partial sixel's printable payload is what garbles the
					// mirrored pane, so it is dropped rather than forwarded as
					// a truncated escape. Dropping the prefix alone is not
					// enough: a sixel body carries no ESC, so every byte after
					// the drop point would be scanned as literal and painted
					// as text (#319). Consume the introducer's ESC so the
					// discard scan cannot match it, then run to the next ESC.
					s.discard = mode
					buf = buf[1:]
					continue
				}
				if oscFileViable(buf) {
					// A confirmed (or still-resolving, which cannot survive
					// this long past a 12-byte discriminator) OSC 1337 File=
					// whose terminator never arrived. Same reasoning as the
					// sixel case above: drop it rather than leak the payload
					// as text, consuming the introducer's ESC first so the
					// discard scan cannot match it. Counted here, at the point
					// the drop is committed to, so an overflow-discarded
					// sequence is not missed by the proxy's log.
					s.discard = discardOSC
					s.InlineImage++
					buf = buf[1:]
					continue
				}
				return appendLiteral(out, buf)
			}
			s.hold(buf, owned)
			s.rasterScanned = rasterResumeFor(buf)
			return out
		case drop == keepRaster:
			// A complete bare sixel. The scanner reports it verbatim and does
			// not decide its fate; the layer above owns that policy.
			out = append(out, Chunk{Raster: append([]byte(nil), buf[:n]...)})
		case drop == dropSixel:
			// A complete passthrough-wrapped sixel: consume, emit nothing. The
			// wrapper routes the local tmux to tty_cmd_rawstring, which neither
			// positions nor clips, so it is not relayable at all.
		case drop == dropMalformed:
			// Ours, but not decodable whole — consume and emit nothing rather
			// than forward a store the localiser never saw (see decodeSeq).
			s.Malformed++
		case drop == dropInlineImage:
			// An OSC 1337 File= sequence (R3): consume and emit nothing. It is
			// never relayable — tmux has no inline-image handling, so a
			// relayed one would land unclipped wherever the pane's cursor
			// last was and be destroyed by the next redraw.
			s.InlineImage++
		case seq == nil:
			// A complete passthrough carrying something else (OSC 52, …).
			// Forward it verbatim: it is already wrapped for one tmux layer,
			// which is exactly what the renderer's local tmux needs, so the
			// escape reaches the outer terminal and does what its sender meant.
			out = appendLiteral(out, buf[:n])
		default:
			out = append(out, Chunk{Seq: seq, Raw: append([]byte(nil), buf[:n]...)})
		}
		buf = buf[n:]
	}
	return out
}

// Flush emits any held partial sequence as a literal. Called when a pane's sink
// closes, so held bytes are never silently swallowed — except a partial sixel
// (and the tail of one already given up on), which is dropped for the same
// reason as a hold-budget overflow of one.
func (s *Scanner) Flush() []Chunk {
	held := s.held
	s.held = nil
	s.rasterScanned = 0
	if s.discard != discardOff {
		// Mid-discard: whatever is held is the tail of a corrupt raster
		// sequence, never text.
		s.discard = discardOff
		return nil
	}
	if len(held) == 0 {
		return nil
	}
	if isPartialSixel(held) {
		return nil
	}
	if oscFileConfirmed(held) {
		// A confirmed File= cut short: the same #319-shaped hazard as a
		// partial sixel (R3), so it is dropped rather than forwarded. A
		// still-ambiguous "\x1b]" prefix that never resolved falls through
		// below and is forwarded verbatim — harmless, since the local tmux
		// discards an unknown or incomplete OSC.
		return nil
	}
	return []Chunk{{Literal: held}}
}

// hold retains b for the next Feed. b is copied only when it still aliases the
// caller's buffer, which is what keeps the hold amortised O(1) per byte.
func (s *Scanner) hold(b []byte, owned bool) {
	if len(b) == 0 {
		s.held = nil
		return
	}
	if !owned {
		b = append([]byte(nil), b...)
	}
	s.held = b
}

// holdLimit is the cap on bytes held for the sequence at the head of b. Only a
// BARE partial sixel gets the raster budget: it is the one hold that can end in
// a sequence worth relaying. Every other hold keeps maxPartial, which has a
// second, unrelated job — it is when a stuck ordinary escape self-heals by
// being forwarded verbatim, and raising it would multiply that stall.
func (s *Scanner) holdLimit(b []byte) int {
	if s.rasterHold > 0 && partialSixelDiscard(b) == discardBare {
		return s.rasterHold
	}
	return maxPartial
}

// consumeDiscard eats the tail of a raster sequence the scanner has given up
// on, returning the bytes consumed and whether normal scanning is re-armed.
//
// The bound is the next ESC, never a byte count and never the terminator. The
// event that overflows the hold budget is a dropped sink frame — the same event
// that can carry the terminator away — so latching until an ST would swallow
// the pane's whole output until some unrelated \e\\ wandered past. A sixel body
// provably contains no ESC (its bytes are `?`–`~`, `#`, `!`, `-`, `$`, digits
// and `;`), so this runs to exactly the end of the corrupt image. A byte-budget
// re-arm is not an option either: re-arming into forwarding drops straight back
// into the leak, making it bounded rather than fixed.
//
// The deliberate cost: any plain text between the corrupt image and the next
// ESC is discarded with it. That is bounded by the next escape sequence, and is
// strictly better than painting megabytes of sixel body into the pane as text.
func (s *Scanner) consumeDiscard(b []byte) (n int, exited bool) {
	if s.discard == discardOSC {
		// An OSC has four exits, not sixel's one — see consumeOSCDiscard.
		return s.consumeOSCDiscard(b)
	}
	for {
		i := bytes.IndexByte(b[n:], 0x1b)
		if i < 0 {
			return len(b), false
		}
		n += i
		if n+1 >= len(b) {
			// A lone trailing ESC cannot be classified yet; hold it and stay
			// in discard rather than guess.
			return n, false
		}
		// Inside a passthrough the payload's own ESCs are doubled, while the
		// wrapper's terminator is a lone \e\\ — so a doubled pair is body, not
		// an exit.
		if s.discard == discardWrapped && b[n+1] == 0x1b {
			n += 2
			continue
		}
		s.discard = discardOff
		if b[n+1] == '\\' {
			// The sequence's own ST: consume it as the terminator.
			return n + 2, true
		}
		// Anything else introduces whatever follows — leave it for the scanner.
		return n, true
	}
}

// consumeOSCDiscard eats the tail of an OSC 1337 File= sequence the scanner
// has given up on (R3's overflow bound), by the same next-exit rule as
// consumeDiscard's sixel case, extended to OSC's other exits. keyneg's
// regionOSC classification (strip.go) is the in-repo statement of the set: ST
// and BEL terminate and are consumed, CAN/SUB abort and are consumed, and a
// bare ESC is not consumed — it introduces whatever follows, which tmux maps
// out of an OSC but deliberately not out of a DCS, so sixel still ends on ST
// alone.
func (s *Scanner) consumeOSCDiscard(b []byte) (n int, exited bool) {
	n, kind := scanOSCExit(b)
	if kind == oscNotFound || kind == oscPending {
		return n, false
	}
	s.discard = discardOff
	return n, true
}

func appendLiteral(out []Chunk, b []byte) []Chunk {
	if len(b) == 0 {
		return out
	}
	return append(out, Chunk{Literal: append([]byte(nil), b...)})
}

// indexSeqStart returns the offset of the next sequence start (kitty APC bare
// or passthrough-wrapped, or a sixel DCS), or -1. Passthrough (`\ePtmux;`) is
// matched as its own pattern so it wins over the bare-sixel `\eP` prefix —
// `t` is not a sixel param byte, so the sixel scan also skips it, but keeping
// the dedicated match makes the priority explicit.
func indexSeqStart(b []byte) int {
	best := -1
	for _, pat := range []string{apcStart, passStart} {
		if i := indexFixedStart(b, pat); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if i := indexSixelStart(b); i >= 0 && (best < 0 || i < best) {
		best = i
	}
	if i := indexOSC1337Start(b); i >= 0 && (best < 0 || i < best) {
		best = i
	}
	return best
}

// indexFixedStart returns the offset of pat's next occurrence in b: a
// complete match, or — mirroring indexSixelStart's tolerance for the sixel
// introducer — a trailing run at the end of b that is still a viable prefix
// of pat and must be held rather than mistaken for literal text, since a Feed
// boundary can split pat anywhere. Only the trailing suffix of b needs
// checking for the partial case: a partial match anywhere else in b is
// already resolved by the following byte, either completing into a full
// match (already caught by bytes.Index) or diverging (not a match at all).
func indexFixedStart(b []byte, pat string) int {
	if i := bytes.Index(b, []byte(pat)); i >= 0 {
		return i
	}
	max := min(len(pat)-1, len(b))
	for l := max; l > 0; l-- {
		if bytes.Equal(b[len(b)-l:], []byte(pat[:l])) {
			return len(b) - l
		}
	}
	return -1
}

// indexOSC1337Start finds the next \x1b]1337;File= introducer (R3), or an
// \x1b] prefix still consistent with becoming one — which must be held rather
// than mistaken for plain text, since a Feed boundary can split the
// discriminator anywhere. Every other OSC 1337 verb (CurrentDir=, SetUserVar=,
// RemoteHost=, …) and every other OSC entirely is ruled out here and left to
// the ordinary literal scan — that is R3's scope.
func indexOSC1337Start(b []byte) int {
	for from := 0; ; {
		j := bytes.Index(b[from:], []byte(oscIntro))
		if j < 0 {
			return -1
		}
		pos := from + j
		if oscFileViable(b[pos:]) {
			return pos
		}
		from = pos + 1
	}
}

// indexSixelStart finds a bare sixel DCS introducer `\eP[0-9;]*q`, or an
// incomplete `\eP[0-9;]*` still open at the end of b (must hold — emitting it
// as literal would miss the `q` on the next Feed). A `\eP` followed by any
// other byte (e.g. `$` of DECRQSS, `t` of tmux passthrough) is not sixel.
func indexSixelStart(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != 'P' {
			continue
		}
		j := i + 2
		for j < len(b) && isSixelParam(b[j]) {
			j++
		}
		if j >= len(b) || b[j] == 'q' {
			return i
		}
	}
	// A trailing ESC with no following byte can't be `\eP` yet. indexFixedStart
	// (used for apcStart/passStart above) makes the opposite choice: a lone
	// trailing ESC there IS held, since it's a genuine 1-byte prefix of both.
	return -1
}

func isSixelParam(c byte) bool {
	return c >= '0' && c <= '9' || c == ';'
}
