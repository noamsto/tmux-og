package daemon

import (
	"strconv"
	"sync/atomic"
)

// mouseModeCarryMax bounds the trailing partial escape sequence Feed holds for
// the next frame. A 1005 DECSET is at most a handful of bytes, so a longer tail
// is malformed and dropped rather than buffered.
const mouseModeCarryMax = 32

// mouseMode state values. A single word, so a reader can never observe a
// half-updated pair: known-but-legacy and known-but-utf8 are one Store apart.
const (
	mouseModeUnknown int32 = iota
	mouseModeLegacy
	mouseModeUTF8
)

// mouseModeTracker reconstructs a pane's mouse encoding from the byte stream the
// daemon writes toward that pane's renderer. The renderer writes those bytes
// verbatim into the mirror pane's pty (render.Run), so the stream is exactly the
// sequence local tmux parses to decide how to encode an outgoing click — the
// seed's own #{mouse_utf8_flag} (render.Seed's clear-then-set ?1005l/?1005h) is
// its first bytes, and a live DECSET in %output keeps it current after the seed.
//
// Feed is called only from a sink's pump goroutine; MouseUTF8 is read from the
// pane's pumpInput goroutine, which is why the state is atomic and carry is not.
type mouseModeTracker struct {
	state atomic.Int32
	carry []byte
}

// MouseUTF8 reports whether DECSET 1005 (UTF-8) mouse reporting is set on the
// pane, and whether that is known at all. A nil tracker is unknown, which keeps
// the mode-blind heuristic in skipX10Mouse.
func (t *mouseModeTracker) MouseUTF8() (utf8, known bool) {
	if t == nil {
		return false, false
	}
	s := t.state.Load()
	return s == mouseModeUTF8, s != mouseModeUnknown
}

// Feed consumes p, updating the tracked mode from any CSI private-mode
// set/reset it contains. It scans for ESC [ ? <digits> h|l and records mode
// 1005; every other sequence is skipped. A partial sequence at the end of p is
// carried into the next Feed so a DECSET split across two frames still lands.
func (t *mouseModeTracker) Feed(p []byte) {
	b := p
	if len(t.carry) > 0 {
		b = append(append([]byte(nil), t.carry...), p...)
		t.carry = nil
	}
	for i := 0; i < len(b); {
		if b[i] != 0x1b {
			i++
			continue
		}
		if i+1 >= len(b) {
			t.carryTail(b, i)
			return
		}
		if b[i+1] != '[' {
			if b[i+1] == 'c' {
				// RIS resets the pane's screen, mouse modes included, so the
				// 1005 flag goes clear. This is output, where ESC c is RIS (not
				// Alt-c as on the input side).
				t.state.Store(mouseModeLegacy)
				i += 2
				continue
			}
			i++ // ESC + one byte (Alt) or ESC ESC
			continue
		}
		if i+2 >= len(b) {
			t.carryTail(b, i)
			return
		}
		if b[i+2] != '?' {
			i++ // some other CSI; the byte scan moves past its introducer
			continue
		}
		// A private set/reset is a ';'-separated param list ended by h or l.
		// tmux applies every param, so a combined ESC[?1000;1005h must be seen
		// as setting 1005 — not only the single-param form.
		j := i + 3
		set1005 := false
		for {
			start := j
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			if j >= len(b) {
				t.carryTail(b, i)
				return
			}
			if j > start {
				if n, err := strconv.Atoi(string(b[start:j])); err == nil && n == 1005 {
					set1005 = true
				}
			}
			if b[j] != ';' {
				break
			}
			j++
		}
		final := b[j]
		if final != 'h' && final != 'l' {
			i++ // not a set/reset we track, e.g. a query
			continue
		}
		if set1005 {
			if final == 'h' {
				t.state.Store(mouseModeUTF8)
			} else {
				t.state.Store(mouseModeLegacy)
			}
		}
		i = j + 1
	}
}

// carryTail holds b[i:] for the next Feed. i names an ESC; a longer tail than
// the cap is malformed and dropped.
func (t *mouseModeTracker) carryTail(b []byte, i int) {
	tail := append([]byte(nil), b[i:]...)
	if len(tail) > mouseModeCarryMax {
		t.carry = nil
		return
	}
	t.carry = tail
}
