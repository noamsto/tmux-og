// Package keyneg strips terminal QUERY sequences from a mirrored pane's output
// stream before they reach the local pty (#338, #544).
//
// A remote pane's occupant asks its terminal questions: "CSI 6 n" for the
// cursor position, "CSI > 4 ; 2 m" to negotiate extended key reporting,
// "OSC 11 ; ?" for the background colour. The remote tmux answers each one into
// the remote pane's input, exactly as it does when the session is detached. But
// the query bytes are also pane OUTPUT, so they cross the bridge and render.Run
// writes them straight to the local mirror pane's pty, where the local tmux
// parses them as a question from the renderer and answers a SECOND time — a
// reply that travels back renderer stdin -> daemon -> send-keys and lands on the
// remote occupant as unsolicited input. Every reply therefore arrives doubled,
// which desynchronizes the occupant's escape parser: fzf loses its first
// keystrokes, and a cell-size probe times out and leaks its reply into the pane
// as literal text.
//
// The mirror pane's occupant is the renderer, which asks nothing for itself, so
// every query byte reaching it came from the remote and has already been
// answered there. Dropping it restores the detached behaviour, and paints
// nothing either way.
//
// Only QUERIES are stripped, never replies. Three families share a final byte
// between the two directions ("CSI Ps n", "CSI ? Ps u", "CSI Ps t"), so the
// discriminator is a parameter allowlist rather than the final byte:
// over-stripping a query is harmless — it paints nothing and the remote has
// already answered it — while over-stripping a reply destroys bytes a program is
// legitimately painting.
//
// A query inside a "DCS tmux;" passthrough wrapper is forwarded: the remote tmux
// does not answer that one, so the local answer is the only one it gets. Every
// DCS/APC/OSC region is walked PAST, never into — and a region ends exactly
// where the local tmux's parser ends it, since anything after that point is a
// live sequence again (see regionKind).
//
// Two deliberate non-goals. 8-bit C1 introducers (0x9B CSI, 0x9D OSC, 0x90 DCS,
// 0x9F APC) are not recognized — outside the measured scope. And "CSI > flags u",
// the kitty-keyboard push and the protocol twin of #338's "CSI > 4 ; 2 m", is
// NOT stripped here: this package does not own that whole class, only the
// modifyOtherKeys pair.
package keyneg

import "bytes"

// maxPending bounds bytes held waiting for a sequence's final byte. A real
// query is a handful of bytes ("\x1b[>4;2m" is 7); this much unterminated means
// it was never one, and the hold is released as literal.
const maxPending = 32

// maxRegion bounds a DCS/APC/OSC span, and the hold of an OSC colour query
// whose terminator hasn't arrived. An output frame dropped under backpressure
// can discard the terminator outright, so the span must not be unbounded —
// graphics.maxPartial exists for the same reason. Ordinary OSCs are long (a
// window title, a hyperlink, an OSC 52 clipboard body), which is why this bound
// and not maxPending governs them.
const maxRegion = 64 << 10

const (
	esc = 0x1b
	bel = 0x07
	can = 0x18
	sub = 0x1a
)

// regionKind names a span whose bytes are forwarded verbatim and never matched
// against the strip table — the passthrough carve-out, plus every other DCS,
// APC and OSC, none of which can be parsed as a bare CSI.
//
// Where a span ENDS is not a detail: tmux resumes parsing at that byte, so a
// walker that runs past it forwards a live query unfiltered. tmux's OSC and APC
// string tables both begin with INPUT_STATE_ANYWHERE (input.c), which maps CAN
// (0x18) and SUB (0x1a) to ground and ESC to esc_enter — so all three leave the
// string, and the ESC-doubling convention does not apply. The DCS handler table
// deliberately omits that macro ("/* No INPUT_STATE_ANYWHERE */") and reads
// 0x00-0x1a as payload, with ESC going to dcs_escape where anything but '\'
// returns to the handler: that, and only that, is where "ESC ESC" is a consumed
// pair and where a "\ePtmux;" body survives intact.
type regionKind uint8

const (
	regionNone regionKind = iota
	regionDCS             // ST ends it; ESC is doubled, never an abort
	regionAPC             // ST, CAN, SUB or a bare ESC
	regionOSC             // as APC, plus BEL
)

// isCancel reports whether c is one of the two bytes INPUT_STATE_ANYWHERE
// dispatches straight back to ground. 0x19 is listed in the string tables
// themselves as an ignored byte, so it is NOT one of them.
func isCancel(c byte) bool { return c == can || c == sub }

// verdict is what classify decided about the escape sequence at the head of a
// buffer.
type verdict uint8

const (
	vStrip   verdict = iota // a query: consume it, emit nothing
	vForward                // complete and not ours: emit it verbatim
	vRegion                 // a span to walk past: emit the introducer, then walk
	vHold                   // incomplete, bounded by maxPending
	vHoldOSC                // incomplete OSC colour query, bounded by maxRegion
	vAbort                  // not a sequence we can parse; release the ESC alone
)

// Filter strips query sequences from a pane's output stream, holding an
// incomplete trailing sequence until the next Feed. Not goroutine-safe — one
// Filter per pane's output pump, like graphics.Scanner.
type Filter struct {
	held []byte
	// A region routinely straddles two %output frames, so its state lives here
	// rather than in Feed: region names the span, regionLen bounds it, and
	// escPending carries an ESC whose next byte hasn't arrived yet.
	region     regionKind
	regionLen  int
	escPending bool
	// oscScan is how far into held a colour query's terminator scan has already
	// looked. Without it every Feed rescans the whole hold, which a remote
	// dribbling bytes turns into ~2e9 comparisons per maxRegion cycle — memory
	// is bounded, CPU would not be.
	oscScan int
}

func NewFilter() *Filter { return &Filter{} }

// Feed returns p with any bare terminal query removed.
func (f *Filter) Feed(p []byte) []byte {
	if len(f.held) == 0 && f.region == regionNone && bytes.IndexByte(p, esc) < 0 {
		return p
	}
	buf := p
	if len(f.held) > 0 {
		buf = append(append([]byte(nil), f.held...), p...)
		f.held = nil
	}
	var out []byte
	for i := 0; i < len(buf); {
		if f.region != regionNone {
			n := f.walkRegion(buf[i:])
			out = append(out, buf[i:i+n]...)
			i += n
			continue
		}
		j := bytes.IndexByte(buf[i:], esc)
		if j < 0 {
			out = append(out, buf[i:]...)
			break
		}
		out = append(out, buf[i:i+j]...)
		i += j
		rest := buf[i:]
		n, v := f.classify(rest)
		switch v {
		case vStrip:
			i += n
		case vForward, vRegion:
			out = append(out, rest[:n]...)
			i += n
		case vHold, vHoldOSC:
			limit := maxPending
			if v == vHoldOSC {
				limit = maxRegion
			}
			if len(rest) <= limit {
				f.held = append([]byte(nil), rest...)
				return out
			}
			// Overflowed: release the introducer ESC ALONE and rescan from the
			// next byte, so the rest of the run falls out as ordinary literal
			// and the net output is the run untouched. Emitting the whole run
			// here and resuming before it would duplicate it; returning the
			// remainder untouched would stop filtering the frames drained
			// behind it. The abandoned hold takes its scan offset with it.
			f.oscScan = 0
			out = append(out, esc)
			i++
		case vAbort:
			out = append(out, esc)
			i++
		}
	}
	return out
}

// Flush emits any held partial sequence as a literal — called when a pane's
// sink closes, so held bytes are never silently swallowed.
func (f *Filter) Flush() []byte {
	held := f.held
	f.held = nil
	f.oscScan = 0
	return held
}

// classify decides what to do with the escape sequence at the head of b, which
// begins with ESC, and enters a region when the sequence introduces one.
func (f *Filter) classify(b []byte) (int, verdict) {
	if len(b) < 2 {
		return 0, vHold
	}
	switch b[1] {
	case '[':
		return classifyCSI(b)
	case ']':
		n, v := f.classifyOSC(b)
		if v == vRegion {
			f.enterRegion(regionOSC)
		}
		return n, v
	case 'P':
		n, v := classifyDCS(b)
		if v == vRegion {
			f.enterRegion(regionDCS)
		}
		return n, v
	case '_':
		f.enterRegion(regionAPC)
		return 2, vRegion
	}
	// A two-byte or charset escape ("ESC (B", "ESC ="): nothing we match, and
	// its body is plain bytes, so releasing the ESC and rescanning forwards it
	// unchanged.
	return 0, vAbort
}

func (f *Filter) enterRegion(k regionKind) {
	f.region = k
	f.regionLen = 0
	f.escPending = false
}

// walkRegion forwards region bytes verbatim up to and including the region's
// terminator, and returns the bytes consumed, all of which the caller forwards.
// A bare ESC ends an OSC or APC without being consumed: it introduces whatever
// comes next, which the top-level scan must classify. Only in a DCS is ESC
// doubled — as graphics.unwrapPassthrough documents, tmux doubles every ESC in
// a passthrough payload, so "\e\e\\" contains "\e\\" at its second byte and a
// scan for the first ST would cut at the INNER terminator.
func (f *Filter) walkRegion(b []byte) int {
	i := 0
	if f.escPending {
		f.escPending = false
		switch b[0] {
		case esc:
			i = 1 // the second half of a doubled ESC
		case '\\':
			f.region = regionNone
			return 1
		}
	}
	for i < len(b) {
		if f.regionLen > maxRegion {
			// The terminator may have gone with a dropped frame. Leave the
			// region so the filter re-arms — latching it on would disable the
			// strip for this pane's life with nothing to observe it.
			f.region = regionNone
			return i
		}
		if b[i] == esc {
			if f.region != regionDCS {
				f.region = regionNone
				return i
			}
			if i+1 >= len(b) {
				f.escPending = true
				f.regionLen++
				return i + 1
			}
			if b[i+1] == esc {
				i += 2
				f.regionLen += 2
				continue
			}
			if b[i+1] == '\\' {
				f.region = regionNone
				return i + 2
			}
		}
		if f.region == regionOSC && b[i] == bel {
			f.region = regionNone
			return i + 1
		}
		if f.region != regionDCS && isCancel(b[i]) {
			f.region = regionNone
			return i + 1
		}
		i++
		f.regionLen++
	}
	return i
}

// csi is a decomposed CSI sequence: an optional private prefix (0x3C-0x3F), the
// parameter bytes after it, the intermediate bytes, and the final byte.
type csi struct {
	private byte
	params  []byte
	inter   []byte
	final   byte
}

// classifyCSI parses the CSI at the head of b (which begins with "ESC ["). A
// byte belonging to none of the three classes aborts the sequence rather than
// being taken as its final byte.
func classifyCSI(b []byte) (int, verdict) {
	var c csi
	i := 2
	if i < len(b) && b[i] >= 0x3c && b[i] <= 0x3f {
		c.private = b[i]
		i++
	}
	start := i
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	c.params = b[start:i]
	start = i
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	c.inter = b[start:i]
	if i >= len(b) {
		return 0, vHold
	}
	if b[i] < 0x40 || b[i] > 0x7e {
		return 0, vAbort
	}
	c.final = b[i]
	if stripCSI(c) {
		return i + 1, vStrip
	}
	return i + 1, vForward
}

// stripCSI implements the CSI half of the strip table: the query forms only,
// with a parameter allowlist wherever the reply shares the final byte.
func stripCSI(c csi) bool {
	if len(c.inter) == 1 && c.inter[0] == '$' && c.final == 'p' {
		return c.private == 0 || c.private == '?' // DECRQM; its reply ends "$y"
	}
	if len(c.inter) > 0 {
		// DECSCUSR is "CSI Ps SP q" — an intermediate is what tells it apart
		// from XTVERSION's "CSI > q".
		return false
	}
	p0 := param0(c.params)
	switch c.final {
	case 'c': // DA1 / DA2 / DA3; a reply carries the '?' prefix or a non-zero param
		switch c.private {
		case 0, '>', '=':
			return len(c.params) == 0 || string(c.params) == "0"
		}
	case 'q': // XTVERSION. Bare "CSI Ps q" is DECLL, which tmux ignores.
		return c.private == '>' && (len(c.params) == 0 || string(c.params) == "0")
	case 'm': // modifyOtherKeys set (#338)
		return c.private == '>'
	case 'n':
		switch c.private {
		case '>': // modifyOtherKeys reset (#338)
			return true
		case 0: // DSR; "0n" and "3n" are its replies
			return p0 == "5" || p0 == "6"
		case '?': // DSR-DEC; every other value is reply space
			switch p0 {
			case "6", "15", "25", "26", "996":
				return true
			}
		}
	// XTWINOPS. Parameter 0 alone decides: 1-10, 22 and 23 are window ACTIONS,
	// and every reply's first parameter is 4, 5, 6, 8 or 9.
	case 't':
		if c.private != 0 {
			return false
		}
		switch p0 {
		case "11", "13", "14", "15", "16", "18", "19", "20", "21":
			return true
		}
	// kitty keyboard query. "CSI ? Ps u" is its reply; the other private
	// prefixes push, set and pop flags.
	case 'u':
		return c.private == '?' && len(c.params) == 0
	}
	return false
}

// classifyOSC parses the OSC at the head of b (which begins with "ESC ]"). Only
// the colour queries are held for their terminator; every other OSC becomes a
// region, since a title or a clipboard body is far too long to hold. A resumed
// hold picks the scan up at f.oscScan: held bytes always begin at the
// introducer ESC, so the offset is stable across Feeds.
func (f *Filter) classifyOSC(b []byte) (int, verdict) {
	scanned := f.oscScan
	f.oscScan = 0
	i := 2
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	if i >= len(b) {
		return 0, vHold
	}
	num := string(b[2:i])
	if b[i] != ';' || !isColourOSC(num) {
		return 2, vRegion
	}
	from := max(i, scanned)
	body, term, res := regionEnd(b[from:], regionOSC)
	switch res {
	case stopPartial:
		f.oscScan = from + body
		return 0, vHoldOSC
	case stopAbort:
		// tmux abandons the OSC here and never answers it, so this is no longer
		// a query to strip — hand it to the region walker, which owns the one
		// implementation of where a region ends.
		return 2, vRegion
	case stopTerm:
	}
	end := from + body
	if oscQuery(num, b[i+1:end]) {
		return end + term, vStrip
	}
	return end + term, vForward
}

func isColourOSC(num string) bool {
	switch num {
	case "4", "10", "11", "12":
		return true
	}
	return false
}

// oscQuery reports whether an OSC colour sequence's payload asks rather than
// sets: exactly "?" for the single-colour 10/11/12, any "?" element for the
// indexed list of OSC 4.
func oscQuery(num string, payload []byte) bool {
	if num != "4" {
		return string(payload) == "?"
	}
	for field := range bytes.SplitSeq(payload, []byte{';'}) {
		if string(field) == "?" {
			return true
		}
	}
	return false
}

// classifyDCS parses the DCS at the head of b (which begins with "ESC P").
// DECRQSS ("DCS $ q ... ST") is the one query in this class; everything else —
// a sixel, a tmux passthrough wrapper — is a region.
func classifyDCS(b []byte) (int, verdict) {
	if len(b) < 3 {
		return 0, vHold
	}
	if b[2] != '$' {
		return 2, vRegion
	}
	if len(b) < 4 {
		return 0, vHold
	}
	if b[3] != 'q' {
		return 2, vRegion
	}
	body, term, res := regionEnd(b[4:], regionDCS)
	if res != stopTerm {
		return 0, vHold
	}
	return 4 + body + term, vStrip
}

// regionStop is how far regionEnd got: to the region's terminator, to a byte
// that ends it without terminating it, or to the end of what it was given.
type regionStop uint8

const (
	stopPartial regionStop = iota
	stopTerm
	stopAbort
)

// regionEnd walks b to its terminator under kind's rules (see regionKind) and
// returns the body length and the terminator's length. On stopPartial, body is
// instead how far the scan got — the index of a trailing ESC whose next byte
// has not arrived, else len(b) — so a resumed scan re-reads that ESC rather
// than splitting it from what decides its meaning.
func regionEnd(b []byte, kind regionKind) (body, term int, res regionStop) {
	for i := 0; i < len(b); {
		if b[i] == esc {
			if i+1 >= len(b) {
				return i, 0, stopPartial
			}
			// ST is checked before the abort, for every kind. In an OSC tmux
			// dispatches at the ESC and reads the '\\' as a bare ST, so taking
			// the pair as the terminator consumes the same bytes and lets a
			// colour query be stripped whole rather than forwarded.
			if b[i+1] == '\\' {
				return i, 2, stopTerm
			}
			if kind != regionDCS {
				return i, 0, stopAbort
			}
			if b[i+1] == esc {
				i += 2
				continue
			}
		}
		if kind == regionOSC && b[i] == bel {
			return i, 1, stopTerm
		}
		if kind != regionDCS && isCancel(b[i]) {
			return i, 0, stopAbort
		}
		i++
	}
	return len(b), 0, stopPartial
}

// param0 returns the first ';'-separated parameter, the one every allowlist in
// stripCSI is keyed on.
func param0(params []byte) string {
	first, _, _ := bytes.Cut(params, []byte{';'})
	return string(first)
}
