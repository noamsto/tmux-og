package graphics

import (
	"bytes"
	"slices"
	"testing"
)

// --- byte-script decoding into fuzz input streams ------------------------
//
// buildStream/buildStreamSafeOnly decode a fuzzer-controlled `script []byte`
// into a stream of Chunk-scanner input, made of typed segments (see the
// design plan). Every segment that is drop-designated (built to overflow a
// hold budget, or a confirmed-but-truncated sequence meant for Flush) has a
// canary spliced into its payload; the fuzz property is that the canary must
// never surface in the scanner's Literal output (#319).

const (
	maxSegments   = 32
	maxLiteralLen = 256
)

// encodeCanary builds the 12-byte, 0xFF-delimited canary. 0xFF cannot occur
// in any control byte the scanner recognizes (discard-mode exit is keyed on
// 0x1b; OSC exit scanning recognizes only 0x07/0x18/0x1a/0x1b) or in a valid
// sixel param byte (isSixelParam), so it cannot appear in correctly scanned
// output — only in bytes this test deliberately drops.
//
// The 8 payload bytes are masked to printable ASCII, same as maskPrintable:
// a raw fuzzed uint64 could otherwise spell 0x1b 0x5c (a real ST) or a bare
// 0x07/0x18/0x1a inside a drop-designated body, letting the canary itself
// prematurely terminate the sequence it's meant to be trapped inside.
func encodeCanary(v uint64) []byte {
	b := make([]byte, 0, 12)
	b = append(b, 0xff)
	for i := 7; i >= 0; i-- {
		b = append(b, 0x20+byte(v>>(8*i))%(0x7f-0x20))
	}
	return append(b, 0xff, 0xff, 0xff)
}

// maskPrintable maps every byte in b onto printable ASCII (0x20-0x7e), so a
// "safe" (non-drop-designated) segment can never contain 0xFF (which would
// make the canary check fire on correct code) or 0x1b (which would make it
// accidentally look like a sequence introducer).
func maskPrintable(b []byte) {
	for i, c := range b {
		b[i] = 0x20 + c%(0x7f-0x20)
	}
}

// sixelParamByte maps an arbitrary byte onto a valid sixel param byte
// ('0'-'9' or ';', per isSixelParam in scan.go) — used to fill a bare-sixel
// body so it cannot terminate early via some path other than the ST this
// test deliberately withholds.
func sixelParamByte(c byte) byte {
	const alphabet = "0123456789;"
	return alphabet[int(c)%len(alphabet)]
}

// nonExitByte maps an arbitrary byte onto printable ASCII, which already
// excludes the OSC exit bytes (0x07 bel, 0x18 can, 0x1a sub) and ESC
// (0x1b) — used to fill an OSC 1337 File= body that must stay unresolved.
func nonExitByte(c byte) byte {
	return 0x20 + c%(0x7f-0x20)
}

// spliceMiddle inserts canary into the middle of body, padding body first if
// it is shorter than canary. Padding is 0x20 (space), not 0x00 — kept
// printable like every other filler byte in this file, so a short body never
// introduces a byte the canary-encoding rationale hasn't already accounted for.
func spliceMiddle(body, canary []byte) []byte {
	if len(body) < len(canary) {
		pad := make([]byte, len(canary)-len(body))
		for i := range pad {
			pad[i] = 0x20
		}
		body = append(body, pad...)
	}
	if body == nil {
		body = []byte{}
	}
	mid := len(body) / 2
	out := make([]byte, 0, len(body)+len(canary))
	out = append(out, body[:mid]...)
	out = append(out, canary...)
	return append(out, body[mid:]...)
}

// doublePassthroughNoTerm doubles inner's ESCs the way EncodeWrapped/tmux's
// passthrough does, but — unlike tmuxPassthrough in scan_test.go — never
// appends the closing ST. Used to build a wrapped-sixel segment that must
// stay genuinely incomplete until the hold budget trips its drop, rather
// than resolving as a complete (and therefore droppable-but-not-held) sixel.
func doublePassthroughNoTerm(inner []byte) []byte {
	out := make([]byte, 0, len(passStart)+2*len(inner))
	out = append(out, passStart...)
	for _, c := range inner {
		if c == 0x1b {
			out = append(out, 0x1b)
		}
		out = append(out, c)
	}
	return out
}

// buildStream decodes script into a stream exercising all eight segment
// templates, splicing canary into every drop-designated segment.
func buildStream(script []byte, canary uint64) (stream, canaryBytes []byte, hasDrop bool) {
	return decodeScript(script, encodeCanary(canary), true)
}

// buildStreamSafeOnly decodes script using only the three safe templates
// (literal, kittySafe, oscOtherSafe) — leg B's split-invariance stream,
// which must never come near a hold budget.
func buildStreamSafeOnly(script []byte) (stream, canaryBytes []byte, hasDrop bool) {
	return decodeScript(script, nil, false)
}

// decodeScript is the shared segment decoder. allowDrop selects the tag
// range (0-7 vs 0-2) and whether drop-designated segments are ever built; at
// most one of the three over-budget templates (tags 3-5) is built at full
// (~64KB) size per script, so a script that repeatedly selects them stays
// fast — later selections are demoted to a no-op (their length-selector byte
// is still consumed, for determinism, but nothing is emitted).
//
// Every drop-designated template (tags 3-7) is only actually built when it
// is the LAST segment in the script: a segment appended afterward could
// supply bytes that inadvertently complete an "over budget"/"truncated"
// body (e.g. an ST that lets a still-growing sixel resolve as an ordinary
// complete Raster instead of overflowing), which would both mislabel
// hasDrop and drop that instance out of the property-1 canary check
// entirely. A non-last selection of tags 3-5 is a no-op (nothing emitted,
// like a repeated over-budget selection); tags 6-7 fall back to emitting
// their raw bytes as an ordinary (uncanaried) literal — see takeTruncatedBody.
func decodeScript(script []byte, canaryBytes []byte, allowDrop bool) (stream, cb []byte, hasDrop bool) {
	var buf bytes.Buffer
	seg := 0
	usedOverBudget := false
	for len(script) > 0 && seg < maxSegments {
		tagByte := script[0]
		script = script[1:]
		var tag int
		if allowDrop {
			tag = int(tagByte % 8)
		} else {
			tag = int(tagByte % 3)
		}
		seg++
		switch tag {
		case 0: // literal
			n := 0
			if len(script) > 0 {
				n = int(script[0])
				script = script[1:]
			}
			if n > maxLiteralLen {
				n = maxLiteralLen
			}
			if n > len(script) {
				n = len(script)
			}
			data := append([]byte(nil), script[:n]...)
			script = script[n:]
			maskPrintable(data)
			buf.Write(data)

		case 1: // kittySafe
			wrapped := false
			if len(script) > 0 {
				wrapped = script[0]&1 == 1
				script = script[1:]
			}
			if wrapped {
				buf.WriteString(wrappedSeq)
			} else {
				buf.WriteString(bareSeq)
			}

		case 2: // oscOtherSafe
			can := false
			if len(script) > 0 {
				can = script[0]&1 == 1
				script = script[1:]
			}
			if can {
				// keyneg's CAN-abort shape (picker/remotebridge/keyneg,
				// strip_test.go): "\x1b]0;\x18\x1b[>4;2m\x07" — no
				// "1337;File=" prefix, so it never enters the OSC 1337
				// path at all and flows through the literal scan.
				buf.WriteString("\x1b]0;\x18\x1b[>4;2m\x07")
			} else {
				buf.WriteString("\x1b]1337;CurrentDir=/tmp\x07")
			}

		case 3: // sixelOverBudget: bare, unterminated, over maxPartial. Only when last (see decodeScript's doc comment).
			jitter := 0
			if len(script) > 0 {
				jitter = int(script[0]) % 64
				script = script[1:]
			}
			isLast := len(script) == 0 || seg == maxSegments
			if !allowDrop || !isLast || usedOverBudget {
				break
			}
			usedOverBudget = true
			body := makeSixelBody(maxPartial+1+jitter, canaryBytes)
			buf.WriteString(dcsStart + "q")
			buf.Write(body)
			hasDrop = true

		case 4: // sixelWrappedOverBudget: same, inside a tmux passthrough. Only when last (see decodeScript's doc comment).
			jitter := 0
			if len(script) > 0 {
				jitter = int(script[0]) % 64
				script = script[1:]
			}
			isLast := len(script) == 0 || seg == maxSegments
			if !allowDrop || !isLast || usedOverBudget {
				break
			}
			usedOverBudget = true
			body := makeSixelBody(maxPartial+1+jitter, canaryBytes)
			inner := append([]byte(dcsStart+"q"), body...)
			buf.Write(doublePassthroughNoTerm(inner))
			hasDrop = true

		case 5: // osc1337FileOverBudget: confirmed File=, unterminated, over maxPartial. Only when last (see decodeScript's doc comment).
			jitter := 0
			if len(script) > 0 {
				jitter = int(script[0]) % 64
				script = script[1:]
			}
			isLast := len(script) == 0 || seg == maxSegments
			if !allowDrop || !isLast || usedOverBudget {
				break
			}
			usedOverBudget = true
			body := makeOSCBody(maxPartial+1+jitter, canaryBytes)
			buf.WriteString(osc1337FilePrefix)
			buf.Write(body)
			hasDrop = true

		case 6: // osc1337FileTruncatedAtFlush: only when this is the last segment.
			raw, isLast := takeTruncatedBody(&script, seg)
			for i := range raw {
				raw[i] = nonExitByte(raw[i])
			}
			if !allowDrop || !isLast {
				buf.Write(raw) // demoted: an ordinary literal, no canary.
				break
			}
			buf.WriteString(osc1337FilePrefix)
			buf.Write(spliceMiddle(raw, canaryBytes))
			hasDrop = true

		case 7: // sixelBareTruncatedAtFlush: only when this is the last segment.
			raw, isLast := takeTruncatedBody(&script, seg)
			for i := range raw {
				raw[i] = sixelParamByte(raw[i])
			}
			if !allowDrop || !isLast {
				buf.Write(raw) // demoted: an ordinary literal, no canary.
				break
			}
			buf.WriteString(dcsStart + "q")
			buf.Write(spliceMiddle(raw, canaryBytes))
			hasDrop = true
		}
	}
	return buf.Bytes(), canaryBytes, hasDrop
}

func makeSixelBody(size int, canary []byte) []byte {
	body := make([]byte, size)
	for i := range body {
		body[i] = sixelParamByte(byte(i))
	}
	return spliceMiddle(body, canary)
}

func makeOSCBody(size int, canary []byte) []byte {
	body := make([]byte, size)
	for i := range body {
		body[i] = nonExitByte(byte(i))
	}
	return spliceMiddle(body, canary)
}

// takeTruncatedBody consumes a short (8-31 byte) body for a tag-6/7 segment
// from *script, and reports whether this was the last segment a forward,
// single-pass decode could see — i.e. whether fewer than the 1-byte minimum
// header remains afterward, or the segment cap was reached by this segment.
func takeTruncatedBody(script *[]byte, seg int) (raw []byte, isLast bool) {
	n := 20
	s := *script
	if len(s) > 0 {
		n = 8 + int(s[0])%24
		s = s[1:]
	}
	if n > len(s) {
		n = len(s)
	}
	raw = append([]byte(nil), s[:n]...)
	s = s[n:]
	*script = s
	isLast = len(s) == 0 || seg == maxSegments
	return raw, isLast
}

// --- chunk-slice helpers ---------------------------------------------------

// concatEmitted concatenates every chunk's emitted bytes in order: Literal
// for a literal chunk, Raw (never Seq.Encode/EncodeWrapped — see Chunk's doc
// comment) for a sequence chunk, Raster for a raster chunk.
func concatEmitted(cs []Chunk) []byte {
	var b bytes.Buffer
	for _, c := range cs {
		switch {
		case c.Seq != nil:
			b.Write(c.Raw)
		case c.Raster != nil:
			b.Write(c.Raster)
		default:
			b.Write(c.Literal)
		}
	}
	return b.Bytes()
}

// assertSubsequence fails t unless subset occurs in superset, in order, as a
// (not necessarily contiguous) subsequence — the no-fabrication property.
func assertSubsequence(t *testing.T, superset, subset []byte) {
	t.Helper()
	i := 0
	for _, c := range subset {
		for i < len(superset) && superset[i] != c {
			i++
		}
		if i >= len(superset) {
			t.Fatalf("emitted bytes are not a subsequence of the input (fabrication): superset=%q subset=%q", superset, subset)
		}
		i++
	}
}

func assertConcatEqual(t *testing.T, whole, split []Chunk) {
	t.Helper()
	w, s := concatEmitted(whole), concatEmitted(split)
	if !bytes.Equal(w, s) {
		t.Fatalf("split-vs-whole byte mismatch:\n whole=%q\n split=%q", w, s)
	}
}

// collapseRuns collapses consecutive 'L' runs in a chunkKinds string to a
// single 'L' — a split Feed may legally emit N adjacent Literal chunks where
// a whole Feed emits one, purely from Feed-boundary framing.
func collapseRuns(s string) string {
	var b bytes.Buffer
	var prev byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 'L' && prev == 'L' {
			continue
		}
		b.WriteByte(c)
		prev = c
	}
	return b.String()
}

func assertCoalescedKindsEqual(t *testing.T, whole, split []Chunk) {
	t.Helper()
	w, s := collapseRuns(chunkKinds(whole)), collapseRuns(chunkKinds(split))
	if w != s {
		t.Fatalf("chunk kind sequence mismatch (after coalescing L runs): whole=%q split=%q", w, s)
	}
}

// deriveSplits derives up to 8 sorted, unique, deterministic split offsets in
// [1, len(streamB)-1] from splitSeed, excluding any offset landing right
// after a bare trailing ESC — a lone ESC at the end of a Feed piece is
// emitted as Literal rather than held (the scanner needs a lookahead byte to
// recognize any sequence start), so a split there legitimately diverges from
// the whole-feed result without indicating a bug.
func deriveSplits(seed uint64, s []byte) []int {
	n := len(s)
	if n < 2 {
		return nil
	}
	seen := map[int]bool{}
	for range 8 {
		seed = seed*6364136223846793005 + 1442695040888963407
		off := int(seed%uint64(n-1)) + 1
		if s[off-1] == 0x1b {
			continue
		}
		seen[off] = true
	}
	out := make([]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func selectRasterHold(sel uint8) int {
	table := []int{0, -1, 1, 1 << 6, 1 << 16, 1 << 24}
	return table[int(sel)%len(table)]
}

// --- seed corpus -----------------------------------------------------------

type fuzzSeed struct {
	script        []byte
	rasterHoldSel uint8
	splitSeed     uint64
	canary        uint64
}

func fuzzSeeds() []fuzzSeed {
	filler := bytes.Repeat([]byte{0x41}, 40)
	seeds := []fuzzSeed{
		{append([]byte{0, 5}, []byte("hello")...), 0, 1, 0xdeadbeef},                // tag 0 literal
		{[]byte{1, 0}, 0, 2, 0xdeadbeef},                                            // tag 1 kittySafe bare
		{[]byte{1, 1}, 0, 3, 0xdeadbeef},                                            // tag 1 kittySafe wrapped
		{[]byte{2, 0}, 0, 4, 0xdeadbeef},                                            // tag 2 oscOtherSafe
		{[]byte{3, 10}, 0, 5, 0xdeadbeef},                                           // tag 3 sixelOverBudget
		{[]byte{4, 10}, 0, 6, 0xdeadbeef},                                           // tag 4 sixelWrappedOverBudget
		{[]byte{5, 10}, 0, 7, 0xdeadbeef},                                           // tag 5 osc1337FileOverBudget
		{append([]byte{6, 20}, filler...), 0, 8, 0xdeadbeef},                        // tag 6 osc1337FileTruncatedAtFlush
		{append([]byte{7, 20}, filler...), 0, 9, 0xdeadbeef},                        // tag 7 sixelBareTruncatedAtFlush
		{append(append([]byte{0, 4}, []byte("abcd")...), 1, 0, 2, 1), 0, 10, 0xabc}, // all-safe mixed
		{nil, 0, 0, 0}, // empty script
	}
	for sel := range uint8(6) {
		seeds = append(seeds, fuzzSeed{[]byte{3, 5}, sel, 11 + uint64(sel), 0x1234})
	}
	return seeds
}

// --- FuzzScan ---------------------------------------------------------------

func FuzzScan(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s.script, s.rasterHoldSel, s.splitSeed, s.canary)
	}
	f.Fuzz(func(t *testing.T, script []byte, rasterHoldSel uint8, splitSeed uint64, canary uint64) {
		rasterHold := selectRasterHold(rasterHoldSel)
		stream, canaryBytes, hasDrop := buildStream(script, canary)
		if len(stream) == 0 {
			return
		}

		// Leg A: single Feed, fuzzed rasterHold — properties 1 (canary),
		// 3 (no-fabrication), 4 (boundedness).
		s := NewScanner()
		s.SetRasterHold(rasterHold)
		chunks := s.Feed(stream)
		if len(s.held) > s.holdLimit(s.held) {
			t.Fatalf("held budget violated: len=%d limit=%d", len(s.held), s.holdLimit(s.held))
		}
		chunks = append(chunks, s.Flush()...)

		allEmitted := concatEmitted(chunks)
		assertSubsequence(t, stream, allEmitted)
		// A drop-designated segment is always the script's last (decodeScript),
		// so it can never complete — the canary must never surface in any
		// chunk kind, not just Literal (the #319 property).
		if hasDrop && bytes.Contains(allEmitted, canaryBytes) {
			t.Fatalf("#319: canary leaked into scanner output; stream=%q", stream)
		}

		// Leg B: split invariance, an independent safe-only stream, rasterHold
		// pinned to 0 (property 2).
		streamB, _, _ := buildStreamSafeOnly(script)
		if len(streamB) > 0 {
			if len(streamB) >= maxPartial {
				t.Fatalf("leg B precondition violated: streamB too large (%d)", len(streamB))
			}
			splits := deriveSplits(splitSeed, streamB)

			sw := NewScanner()
			whole := sw.Feed(streamB)
			whole = append(whole, sw.Flush()...)

			sp := NewScanner()
			var split []Chunk
			prev := 0
			for _, k := range splits {
				split = append(split, sp.Feed(streamB[prev:k])...)
				prev = k
			}
			split = append(split, sp.Feed(streamB[prev:])...)
			split = append(split, sp.Flush()...)

			assertConcatEqual(t, whole, split)
			assertCoalescedKindsEqual(t, whole, split)
		}

		// Property 5: Flush on a fresh, untouched scanner is nil.
		if got := NewScanner().Flush(); got != nil {
			t.Fatalf("Flush on untouched scanner = %v, want nil", got)
		}
	})
}
