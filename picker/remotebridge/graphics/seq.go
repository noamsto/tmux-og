package graphics

import "bytes"

// Seq is one decoded kitty graphics APC: the control keys before the ';' and
// the payload after it. Wrapped records whether it arrived inside a tmux
// passthrough, for diagnostics only — output is always wrapped exactly once,
// because the renderer pane always sits inside the local tmux.
type Seq struct {
	Keys    []byte
	Payload []byte
	HasBody bool // a ';' was present (an empty payload is distinct from none)
	Wrapped bool
}

// dropReason says how decodeSeq disposed of the bytes it consumed without
// yielding a Seq. Two very different things used to share one bool, which is
// the conflation that let a partly-decoded store be forwarded as though it were
// none of ours.
type dropReason int

const (
	dropNone        dropReason = iota // not a drop
	dropSixel                         // a passthrough-wrapped sixel: not relayable
	dropMalformed                     // ours, but we could not decode it whole
	keepRaster                        // a complete BARE sixel: report b[:n] verbatim
	dropInlineImage                   // an OSC 1337 File= sequence (R3): never relayable
)

// decodeSeq decodes the sequence at the head of b:
//
//	seq != nil, n > 0, dropNone      — a graphics sequence; consume n
//	seq == nil, n > 0, dropNone      — a COMPLETE sequence that isn't ours (a
//	                                   passthrough carrying something else, e.g.
//	                                   OSC 52); forward b[:n] verbatim
//	seq == nil, n > 0, dropSixel     — a complete passthrough-WRAPPED sixel;
//	                                   consume n and emit nothing
//	seq == nil, n > 0, keepRaster    — a complete BARE sixel; consume n and
//	                                   report b[:n] verbatim as Chunk.Raster
//	seq == nil, n > 0, dropMalformed — a kitty APC we could not decode whole;
//	                                   consume n and emit nothing (see the
//	                                   wrapped branch for why forwarding is
//	                                   not an option)
//	seq == nil, n == 0               — incomplete; hold for more bytes
//
// The forward-verbatim case is why this returns a length rather than an ok
// bool. "Not complete yet" and "complete, but not mine" both mean "no sequence
// here", but conflating them stalls the pane: a clipboard escape would hold
// every later byte behind it until the partial cap or Flush.
//
// resume is the ST-search offset carried over from a previous Feed of the same
// held sequence (rasterResumeFor). Only the bare-sixel branch honours it, and
// only that branch ever sets it; 0 means search from the head.
func decodeSeq(b []byte, resume int) (*Seq, int, dropReason) {
	if bytes.HasPrefix(b, []byte(oscIntro)) {
		return decodeOSC1337(b)
	}
	if bytes.HasPrefix(b, []byte(passStart)) {
		inner, n, ok := unwrapPassthrough(b)
		if !ok {
			return nil, 0, dropNone
		}
		// Sixel through the bridge is an explicit non-goal: drop a
		// passthrough whose undoubled payload is a bare sixel DCS rather
		// than forwarding it (which would paint a SIXEL IMAGE placeholder
		// or garble mid-sequence text into the mirrored pane).
		if isConfirmedSixelHead(inner) {
			return nil, n, dropSixel
		}
		q, m, ok := decodeBare(inner)
		// Forwarding a wrapper verbatim is only safe when the inner escape is
		// none of ours. If it IS a kitty APC and we could not decode it whole,
		// forwarding hands the terminal a sequence whose t=f payload never
		// passed the localiser — a path on the REMOTE filesystem, or one the
		// sender chose to name on this one. D7 governs: an unlocalisable store
		// is dropped, never forwarded, because a missing image renders blank and
		// self-heals where a wrong one renders wrong. So the two failures split.
		switch {
		case !ok && !bytes.HasPrefix(inner, []byte(apcStart)):
			// Genuinely not ours (OSC 52, a title, …): forward the wrapper
			// whole, which is what its sender meant and what the renderer's
			// local tmux needs to reach the outer terminal.
			return nil, n, dropNone
		case !ok:
			// Ours, but unterminated inside the wrapper. Forwarding it would
			// leave the terminal holding an open APC that a LATER wrapper could
			// terminate, assembling a store out of pieces neither of which we
			// ever localised.
			return nil, n, dropMalformed
		case m != len(inner):
			// Ours, plus trailing bytes. Splitting the wrapper would mean
			// re-wrapping the remainder to keep its passthrough semantics; not
			// worth it for a shape no legitimate sender emits (tmuxPassthrough
			// wraps exactly one sequence), and dropping is the safe direction.
			return nil, n, dropMalformed
		case hasDuplicateKey(q.Keys):
			return nil, n, dropMalformed
		}
		q.Wrapped = true
		return q, n, dropNone
	}
	if isSixelPrefix(b) {
		n, complete := consumeBareSixel(b, resume)
		if !complete {
			return nil, 0, dropNone
		}
		return nil, n, keepRaster
	}
	// Feed only calls this at an indexSeqStart hit, so a head that isn't a
	// passthrough or sixel is an apcStart. decodeBare can fail for want of
	// the ST (a complete apcStart with no terminator yet), or — since
	// indexSeqStart now holds a buffer ending partway through apcStart
	// itself — for want of the introducer's own remaining bytes. Both are
	// "incomplete, hold for more" (n == 0), so the branch below is unchanged.
	q, n, ok := decodeBare(b)
	if !ok {
		return nil, 0, dropNone
	}
	if hasDuplicateKey(q.Keys) {
		return nil, n, dropMalformed
	}
	return q, n, dropNone
}

// hasDuplicateKey reports whether keys names the same key twice.
//
// Get returns the FIRST match, so a sequence carrying t=d,t=f reads to us as
// inline data while a terminal resolving last-wins reads it as a file
// transmission — and the payload, a path we never localised, reaches it
// verbatim. Rather than guess which way any given terminal resolves it, reject
// the sequence: no legitimate sender emits a duplicate control key, and this
// removes the disagreement instead of trying to match it.
func hasDuplicateKey(keys []byte) bool {
	seen := make(map[string]bool)
	for kv := range bytes.SplitSeq(keys, []byte{','}) {
		before, _, ok := bytes.Cut(kv, []byte{'='})
		if !ok {
			continue
		}
		k := string(before)
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}

func decodeBare(b []byte) (*Seq, int, bool) {
	rest, ok := bytes.CutPrefix(b, []byte(apcStart))
	if !ok {
		return nil, 0, false
	}
	body, _, ok := bytes.Cut(rest, []byte(st))
	if !ok {
		return nil, 0, false
	}
	end := len(body)
	q := &Seq{}
	if before, after, ok := bytes.Cut(body, []byte{';'}); ok {
		q.Keys = append([]byte(nil), before...)
		q.Payload = append([]byte(nil), after...)
		q.HasBody = true
	} else {
		q.Keys = append([]byte(nil), body...)
	}
	return q, len(apcStart) + end + len(st), true
}

// unwrapPassthrough un-doubles the ESCs of a \ePtmux;… wrapper and returns the
// inner sequence plus the bytes consumed. Scanning for the first ST would cut at
// the INNER terminator (\e\e\\ contains \e\\ at its second byte), so the ESCs are
// un-doubled as we walk instead.
func unwrapPassthrough(b []byte) ([]byte, int, bool) {
	i := len(passStart)
	var inner []byte
	for i < len(b) {
		if b[i] == 0x1b {
			if i+1 >= len(b) {
				return nil, 0, false
			}
			if b[i+1] == 0x1b {
				inner = append(inner, 0x1b)
				i += 2
				continue
			}
			if b[i+1] == '\\' {
				return inner, i + 2, true
			}
		}
		inner = append(inner, b[i])
		i++
	}
	return nil, 0, false
}

// isConfirmedSixelHead reports whether b begins with a sixel DCS introducer
// `\eP[0-9;]*q`. Used after unwrap to decide drop-vs-forward on a complete
// passthrough, and does not treat an incomplete `\eP[0-9;]*` prefix as sixel.
func isConfirmedSixelHead(b []byte) bool {
	if !bytes.HasPrefix(b, []byte(dcsStart)) {
		return false
	}
	j := len(dcsStart)
	for j < len(b) && isSixelParam(b[j]) {
		j++
	}
	return j < len(b) && b[j] == 'q'
}

// isSixelPrefix reports whether b begins with a confirmed sixel introducer or
// an incomplete `\eP[0-9;]*` that has not yet been ruled out — the scanner must
// hold the latter rather than emit it as literal.
func isSixelPrefix(b []byte) bool {
	if !bytes.HasPrefix(b, []byte(dcsStart)) {
		return false
	}
	// `\ePtmux;` shares the `\eP` prefix; passthrough is handled first in
	// decodeSeq, but keep the prefix helper honest for held-byte checks.
	if bytes.HasPrefix(b, []byte(passStart)) {
		return false
	}
	j := len(dcsStart)
	for j < len(b) && isSixelParam(b[j]) {
		j++
	}
	if j >= len(b) {
		return true
	}
	return b[j] == 'q'
}

// consumeBareSixel returns the length of a complete bare sixel DCS at the head
// of b. complete is false when the ST has not arrived yet (caller holds).
//
// resume skips the body a previous Feed of this same hold already searched (see
// rasterResumeFor); it is a floor on the search start, never a substitute for
// the header walk, since a resume smaller than the header must not read the
// introducer's own bytes as body.
func consumeBareSixel(b []byte, resume int) (n int, complete bool) {
	if !isConfirmedSixelHead(b) {
		// Incomplete header (`\eP` / `\eP0;1`) — still a sixel prefix, hold.
		return 0, false
	}
	j := len(dcsStart)
	for j < len(b) && isSixelParam(b[j]) {
		j++
	}
	// j points at 'q'.
	from := max(resume, j+1)
	end := bytes.Index(b[from:], []byte(st))
	if end < 0 {
		return 0, false
	}
	return from + end + len(st), true
}

// rasterResumeFor is how far into a held bare partial sixel the ST search has
// already reached: everything but the trailing len(st)-1 bytes, which a
// terminator straddling the Feed boundary needs re-examined.
//
// Only this hold gets a resume. It is the only one whose budget is measured in
// megabytes (Scanner.holdLimit), and a %output line arriving every few
// kilobytes made re-scanning it from byte 0 quadratic in the raster's size — on
// the pane's own pump goroutine, so the pane froze and the sink dropped the
// frames carrying the rest of the very image being assembled. Every other hold
// is bounded by maxPartial and is re-scanned whole.
func rasterResumeFor(b []byte) int {
	if !isConfirmedSixelHead(b) {
		return 0
	}
	if n := len(b) - (len(st) - 1); n > 0 {
		return n
	}
	return 0
}

// isPartialSixel reports whether held/overflow bytes are a sixel in progress
// (bare, or a passthrough whose undoubled payload so far is sixel-headed).
// Those must be dropped on Flush / hold-budget overflow rather than forwarded.
func isPartialSixel(b []byte) bool { return partialSixelDiscard(b) != discardOff }

// partialSixelDiscard classifies a partial sixel by the discard mode its tail
// needs. The two forms differ only in their terminator: a bare sixel ends at a
// lone ST, while a wrapped one doubles its payload's ESCs and ends at the
// wrapper's own lone ST.
func partialSixelDiscard(b []byte) discardMode {
	if bytes.HasPrefix(b, []byte(passStart)) {
		if isSixelPrefix(peekPassthroughInner(b)) {
			return discardWrapped
		}
		return discardOff
	}
	if isSixelPrefix(b) {
		return discardBare
	}
	return discardOff
}

// peekPassthroughInner undoubles ESC pairs inside a `\ePtmux;…` wrapper, like
// unwrapPassthrough, but returns whatever has been seen so far when the outer
// ST is missing — enough to classify a partial wrapped sixel for drop-on-cap.
func peekPassthroughInner(b []byte) []byte {
	if !bytes.HasPrefix(b, []byte(passStart)) {
		return nil
	}
	i := len(passStart)
	var inner []byte
	for i < len(b) {
		if b[i] == 0x1b {
			if i+1 >= len(b) {
				return inner
			}
			if b[i+1] == 0x1b {
				inner = append(inner, 0x1b)
				i += 2
				continue
			}
			if b[i+1] == '\\' {
				return inner
			}
		}
		inner = append(inner, b[i])
		i++
	}
	return inner
}

// Get returns the value of a comma-separated control key ("t", "i", "a", …),
// or "" when absent.
func (q *Seq) Get(key string) string {
	for kv := range bytes.SplitSeq(q.Keys, []byte{','}) {
		before, after, ok := bytes.Cut(kv, []byte{'='})
		if !ok {
			continue
		}
		if string(before) == key {
			return string(after)
		}
	}
	return ""
}

// Encode renders the canonical bare form.
func (q *Seq) Encode() []byte {
	out := append([]byte(apcStart), q.Keys...)
	if q.HasBody {
		out = append(out, ';')
		out = append(out, q.Payload...)
	}
	return append(out, st...)
}

// EncodeWrapped renders the bare form inside exactly one tmux passthrough,
// whatever the input form was: the renderer pane always sits inside the local
// tmux, which needs one wrapper to unwrap to the outer terminal.
func (q *Seq) EncodeWrapped() []byte {
	inner := q.Encode()
	out := make([]byte, 0, len(passStart)+2*len(inner)+len(st))
	out = append(out, passStart...)
	for _, c := range inner {
		if c == 0x1b {
			out = append(out, 0x1b)
		}
		out = append(out, c)
	}
	return append(out, st...)
}

// isStore reports whether q transmits image data under an id — the sequences
// coalescing may supersede.
func isStore(q *Seq) bool {
	switch q.Get("a") {
	case "T", "t":
		return q.Get("i") != ""
	}
	return false
}

// isDelete reports whether q deletes an image by id.
func isDelete(q *Seq) bool { return q.Get("a") == "d" && q.Get("i") != "" }

// decodeOSC1337 decodes the OSC introducer at the head of b (\x1b]), which
// indexOSC1337Start has already confirmed is either a still-resolving or a
// confirmed \x1b]1337;File= (R3) — every other OSC 1337 verb, and every other
// OSC, is ruled out before this is ever called and flows through the ordinary
// literal scan instead.
//
// Bare form only. Unlike sixel, a \ePtmux;-wrapped File= needs nothing: a
// truncated payload inside a passthrough is swallowed whole by tmux's DCS
// parser rather than painted, so there is no text-leak path to close here.
func decodeOSC1337(b []byte) (*Seq, int, dropReason) {
	if !oscFileConfirmed(b) {
		// Still short of the 12-byte discriminator; indexOSC1337Start only
		// stops here when every byte seen so far still matches.
		return nil, 0, dropNone
	}
	body := b[len(osc1337FilePrefix):]
	n, kind := scanOSCExit(body)
	if kind == oscNotFound || kind == oscPending {
		return nil, 0, dropNone
	}
	return nil, len(osc1337FilePrefix) + n, dropInlineImage
}

// oscFileViable reports whether b, read from an "\x1b]" start, could still
// become — or already is — \x1b]1337;File=: every byte available so far
// matches the discriminator. Used both to decide whether indexOSC1337Start
// should stop at a candidate, and whether a held or overflowed span is this
// sequence rather than ordinary OSC text.
func oscFileViable(b []byte) bool {
	n := min(len(b), len(osc1337FilePrefix))
	return bytes.Equal(b[:n], []byte(osc1337FilePrefix)[:n])
}

// oscFileConfirmed reports whether b begins with the full 12-byte
// \x1b]1337;File= discriminator.
func oscFileConfirmed(b []byte) bool {
	return len(b) >= len(osc1337FilePrefix) && bytes.HasPrefix(b, []byte(osc1337FilePrefix))
}

// oscExit classifies how scanOSCExit ended.
type oscExit int

const (
	oscNotFound   oscExit = iota // no exit byte found in the scanned span
	oscPending                   // a trailing ESC with nothing after it yet
	oscTerminator                // ST or BEL: consumed with the sequence
	oscAbort                     // CAN/SUB: consumed with the sequence
	oscIntroducer                // a bare ESC: not consumed, introduces what follows
)

// scanOSCExit finds the first OSC exit in b, per keyneg's regionOSC
// classification (strip.go): ST (\x1b\\), BEL, CAN, or SUB terminate or abort
// the region and are consumed; any other ESC introduces whatever follows and
// is left for the caller, never consumed. n is the number of bytes the exit
// accounts for — through the exit byte(s) when consumed, or up to (not
// including) a bare ESC.
func scanOSCExit(b []byte) (n int, kind oscExit) {
	i := bytes.IndexAny(b, "\x07\x18\x1a\x1b")
	if i < 0 {
		return len(b), oscNotFound
	}
	switch b[i] {
	case bel:
		return i + 1, oscTerminator
	case can, sub:
		return i + 1, oscAbort
	default: // esc
		if i+1 >= len(b) {
			return i, oscPending
		}
		if b[i+1] == '\\' {
			return i + 2, oscTerminator
		}
		return i, oscIntroducer
	}
}
