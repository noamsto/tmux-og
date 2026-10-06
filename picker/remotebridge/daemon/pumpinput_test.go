package daemon

import (
	"bytes"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestPumpInputCallsDiedOnceOnReadError pins the seam that wires into
// death.wake(): a renderer connection closing (crash, clean exit, or the
// daemon's own deliberate Close during reconcile) must fire died() exactly
// once — zero times would mean the corpse is only ever found by the 5s
// backstop, more than once would be pumpInput's own bug rather than the
// no-op-while-armed behavior deathNudge.wake() already owns.
func TestPumpInputCallsDiedOnceOnReadError(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()

	var diedCount atomic.Int32
	done := make(chan struct{})
	go func() {
		pumpInput(conn, "%7", func(string) {}, nil, func() { diedCount.Add(1) }, nil, nil)
		close(done)
	}()

	_ = peer.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpInput did not return after its connection closed")
	}

	if n := diedCount.Load(); n != 1 {
		t.Errorf("died called %d times, want exactly 1", n)
	}
}

// A clean FrameInput read must not fire died — only a read error (the
// connection actually closing) may, or ordinary keystrokes would keep waking
// the sweep.
func TestPumpInputDoesNotCallDiedOnACleanFrameRead(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	var diedCount atomic.Int32
	sendCh := make(chan string, 1)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, func() { diedCount.Add(1) }, nil, nil)

	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	select {
	case <-sendCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no send-keys reached the sink: the frame was never forwarded")
	}
	if n := diedCount.Load(); n != 0 {
		t.Errorf("died called %d times after a clean frame read, want 0 — the connection is still open", n)
	}
}

// died's own doc comment promises nil is legal — every call site that
// predates this field, and any bare Config{} a test builds directly, must
// not panic or hang.
func TestPumpInputToleratesANilDiedCallback(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()

	done := make(chan struct{})
	go func() {
		pumpInput(conn, "%7", func(string) {}, nil, nil, nil, nil)
		close(done)
	}()

	_ = peer.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpInput did not return after its connection closed, with a nil died callback")
	}
}

// seen is what wakes a parked mirror, so it must fire even when the keystroke
// goes nowhere — and it does, before the send that fails closed while parked.
func TestPumpInputCallsSeenBeforeForwarding(t *testing.T) {
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	var seen atomic.Int32
	sendCh := make(chan int32, 1)
	go pumpInput(conn, "%7", func(string) { sendCh <- seen.Load() }, nil, nil, func() { seen.Add(1) }, nil)

	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	select {
	case n := <-sendCh:
		if n != 1 {
			t.Errorf("seen called %d times before the send, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no send-keys reached the sink: the frame was never forwarded")
	}
}

// splitMouseReports are complete mouse reports used to check that a report
// split across two FrameInput reads is reassembled before classification. The
// ambiguous legacy report is the col 162+/row 96+ case whose raw bytes also
// read as valid UTF-8.
var splitMouseReports = map[string]string{
	"sgr":              "\x1b[<0;5;5M",
	"utf8-1005":        "\x1b[M\xc3\xa9!!",
	"legacy-x10":       "\x1b[M !!",
	"legacy-ambiguous": "\x1b[M \xc3\xa9",
}

// splitMouseSentinel is a non-dismiss mouse report that terminates the read.
// Because it is a mouse report itself, any deadKeyCmd seen while reading the
// split report is the bug under test, never the sentinel.
const splitMouseSentinel = "\x1b[<9;9;9M"

// sendKeysBytes decodes one send-keys -H command's hex payload, reporting
// whether cmd was one.
func sendKeysBytes(t *testing.T, cmd string) ([]byte, bool) {
	t.Helper()
	if !strings.HasPrefix(cmd, "send-keys -H -t %7 ") {
		return nil, false
	}
	fields := strings.Fields(cmd)
	var out []byte
	for _, h := range fields[4:] {
		v, err := strconv.ParseUint(h, 16, 8)
		if err != nil {
			t.Fatalf("bad hex token %q in %q", h, cmd)
		}
		out = append(out, byte(v))
	}
	return out, true
}

// A mouse report split across two FrameInput reads must be reassembled, so no
// dead-key guard fires for any byte offset and the forwarded bytes stay in
// order. On the pre-fix classifier the tail of the report starts the next frame
// and counts as a key, dismissing a dead `remain-on-exit key` pane on a click.
func TestPumpInputCarriesSplitMouseReport(t *testing.T) {
	for name, report := range splitMouseReports {
		reportB := []byte(report)
		sent := []byte(splitMouseSentinel)
		want := append(append([]byte{}, reportB...), sent...)
		for i := 0; i <= len(reportB); i++ {
			t.Run(fmt.Sprintf("%s/offset-%d", name, i), func(t *testing.T) {
				conn, peer := net.Pipe()
				defer func() { _ = conn.Close() }()
				defer func() { _ = peer.Close() }()
				sendCh := make(chan string, 16)
				go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, nil)

				for _, p := range [][]byte{reportB[:i], reportB[i:], sent} {
					if err := wire.WriteFrame(peer, wire.FrameInput, p); err != nil {
						t.Fatalf("write frame: %v", err)
					}
				}

				var got []byte
				deadKeys := 0
				deadline := time.After(2 * time.Second)
				for len(got) < len(want) {
					select {
					case cmd := <-sendCh:
						if cmd == deadKeyCmd("%7") {
							deadKeys++
							continue
						}
						if b, ok := sendKeysBytes(t, cmd); ok {
							got = append(got, b...)
						}
					case <-deadline:
						t.Fatalf("timed out: got %q (%d bytes), want %q", got, len(got), want)
					}
				}
				if deadKeys != 0 {
					t.Errorf("deadKeyCmd sent %d time(s) for a mouse report split at offset %d", deadKeys, i)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("forwarded %q, want %q", got, want)
				}
			})
		}
	}
}

// The other half of the invariant: a real key split across two frames still
// dismisses.
func TestPumpInputRealKeySplitStillDismisses(t *testing.T) {
	for _, key := range []string{"\x1b[A", "x"} {
		keyB := []byte(key)
		for i := 0; i <= len(keyB); i++ {
			t.Run(fmt.Sprintf("%q/offset-%d", key, i), func(t *testing.T) {
				conn, peer := net.Pipe()
				defer func() { _ = conn.Close() }()
				defer func() { _ = peer.Close() }()
				sendCh := make(chan string, 16)
				go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, nil)

				for _, p := range [][]byte{keyB[:i], keyB[i:]} {
					if err := wire.WriteFrame(peer, wire.FrameInput, p); err != nil {
						t.Fatalf("write frame: %v", err)
					}
				}
				// A sentinel key after the split; the dead-key guard must land by
				// the time its own send-keys arrives.
				if err := wire.WriteFrame(peer, wire.FrameInput, []byte("z")); err != nil {
					t.Fatalf("write sentinel: %v", err)
				}

				deadKeys := 0
				deadline := time.After(2 * time.Second)
				for {
					select {
					case cmd := <-sendCh:
						if cmd == deadKeyCmd("%7") {
							deadKeys++
						}
						if cmd == "send-keys -H -t %7 7a" {
							if deadKeys == 0 {
								t.Errorf("split real key %q at offset %d did not dismiss", key, i)
							}
							return
						}
					case <-deadline:
						t.Fatalf("sentinel never arrived for key %q offset %d", key, i)
					}
				}
			})
		}
	}
}

func TestIncompleteUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"ascii", "A", false},
		{"continuation only", "\x80", false},
		{"two-byte lead alone", "\xc3", true},
		{"two-byte complete", "\xc3\xa9", false},
		{"two-byte lead then invalid", "\xc3q", false},
		{"three-byte lead alone", "\xe0", true},
		{"three-byte partial", "\xe0\xa0", true},
		{"three-byte complete", "\xe0\xa0\x80", false},
		{"four-byte partial", "\xf0\x9f\x98", true},
		{"four-byte complete", "\xf0\x9f\x98\x80", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := incompleteUTF8([]byte(tt.in)); got != tt.want {
				t.Errorf("incompleteUTF8(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// splitIncompleteEscape's complete/tail split. The ambiguous legacy report is
// deliberately held: the same six bytes are a valid prefix of a 7-byte 1005
// report, so only the next frame or the grace flush can resolve them.
func TestSplitIncompleteEscape(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantComplete string
		wantTail     string
	}{
		{"plain keys", "abc", "abc", ""},
		{"up arrow", "\x1b[A", "\x1b[A", ""},
		{"alt key", "\x1bz", "\x1bz", ""},
		{"lone esc", "\x1b", "", "\x1b"},
		{"esc esc", "\x1b\x1b", "\x1b\x1b", ""},
		{"open csi", "\x1b[", "", "\x1b["},
		{"open ss3", "\x1bO", "", "\x1bO"},
		{"ss3 final", "\x1bOA", "\x1bOA", ""},
		{"sgr mouse", "\x1b[<0;5;5M", "\x1b[<0;5;5M", ""},
		{"sgr mouse open", "\x1b[<0;5;", "", "\x1b[<0;5;"},
		{"sgr release", "\x1b[<0;5;5m", "\x1b[<0;5;5m", ""},
		{"legacy x10", "\x1b[M !!", "\x1b[M !!", ""},
		{"x10 one param", "\x1b[M x", "", "\x1b[M x"},
		{"1005 two-byte param", "\x1b[M\xc3\xa9!!", "\x1b[M\xc3\xa9!!", ""},
		{"1005 truncated rune", "\x1b[M A\xc3", "", "\x1b[M A\xc3"},
		{"ambiguous legacy held", "\x1b[M \xc3\xa9", "", "\x1b[M \xc3\xa9"},
		{"bracketed paste", "\x1b[200~hi\x1b[201~", "\x1b[200~hi\x1b[201~", ""},
		{"open bracketed paste", "\x1b[200~hi", "\x1b[200~hi", ""},
		{"complete then open", "ab\x1b[<0;5;", "ab", "\x1b[<0;5;"},
		{"key then open", "a\x1b[M", "a", "\x1b[M"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			complete, tail := splitIncompleteEscape([]byte(tt.in))
			if string(complete) != tt.wantComplete || string(tail) != tt.wantTail {
				t.Errorf("splitIncompleteEscape(%q) = (%q, %q), want (%q, %q)",
					tt.in, complete, tail, tt.wantComplete, tt.wantTail)
			}
		})
	}
}

// skipX10Mouse's return pins the pre-refactor length rule, including the
// truncated-rune and fewer-than-three-param cases.
func TestSkipX10Mouse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"legacy three bytes", "\x1b[M !!", ""},
		{"one param", "\x1b[M x", ""},
		{"two params", "\x1b[M xy", ""},
		{"truncated two-byte rune", "\x1b[M\xc3", ""},
		{"two truncated leads", "\x1b[M\xc3\xc3", ""},
		{"three params then leftover", "\x1b[M !!\xc3", "\xc3"},
		{"1005 two-byte param", "\x1b[M\xc3\xa9!!", ""},
		{"ambiguous legacy", "\x1b[M \xc3\xa9", ""},
		{"ambiguous legacy then here", "\x1b[M A\xc3\xa9", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(skipX10Mouse([]byte(tt.in), false, false)); got != tt.want {
				t.Errorf("skipX10Mouse(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A carried lone Escape must still be delivered once the grace expires, with no
// further input.
func TestPumpInputLoneEscDeliveredAfterGrace(t *testing.T) {
	oldGrace := escCarryGrace
	escCarryGrace = 20 * time.Millisecond
	t.Cleanup(func() { escCarryGrace = oldGrace })

	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()
	sendCh := make(chan string, 8)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, nil)

	if err := wire.WriteFrame(peer, wire.FrameInput, []byte{0x1b}); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := []string{deadKeyCmd("%7"), "send-keys -H -t %7 1b", modalClearCmd("%7")}
	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < len(want) {
		select {
		case s := <-sendCh:
			got = append(got, s)
		case <-deadline:
			t.Fatalf("lone ESC not delivered after the grace; got %q", got)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("sends = %q, want %q", got, want)
	}
}

func TestModalClearCmd(t *testing.T) {
	got := modalClearCmd("%7")
	want := `if -F -t %7 '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %7'`
	if got != want {
		t.Errorf("modalClearCmd(%%7) = %q, want %q", got, want)
	}
}

func TestDeadKeyCmd(t *testing.T) {
	got := deadKeyCmd("%7")
	want := `if -F -t %7 '#{&&:#{pane_dead},#{||:#{==:#{remain-on-exit},key},#{==:#{remain-on-exit},failed-key}}}' 'kill-pane -t %7'`
	if got != want {
		t.Errorf("deadKeyCmd(%%7) = %q, want %q", got, want)
	}
}

func TestIsDismissKey(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"a", []byte("a"), true},
		{"carriage return", []byte("\r"), true},
		{"lone escape", []byte{0x1b}, true},
		{"lone ctrl-c", []byte{0x03}, true},
		{"up arrow", []byte("\x1b[A"), true},
		{"meta-x", []byte("\x1bx"), true},
		{"utf-8", []byte("é"), true},
		{"hello", []byte("hello"), true},
		{"sgr mouse click", []byte("\x1b[<0;5;5M"), false},
		{"sgr mouse release", []byte("\x1b[<0;5;5m"), false},
		{"x10 mouse", []byte("\x1b[M !!"), false},
		{"focus in", []byte("\x1b[I"), false},
		{"focus out", []byte("\x1b[O"), false},
		{"bracketed paste", []byte("\x1b[200~hi\x1b[201~"), true},
		{"trailing bracketed paste marker", []byte("x\x1b[200~y"), true},
		{"empty bracketed paste", []byte("\x1b[200~\x1b[201~"), false},
		{"focus-in then sgr mouse in one flush", []byte("\x1b[I\x1b[<0;5;5M"), false},
		{"focus-out then focus-in", []byte("\x1b[O\x1b[I"), false},
		{"sgr mouse then a real key", []byte("\x1b[<0;5;5Mx"), true},
		{"x10 mouse then a real key", []byte("\x1b[M !!q"), true},
		{"x10/utf-8 mouse two-byte param", []byte("\x1b[M\xc3\xa9!!"), false},
		{"x10/utf-8 mouse two-byte y after a one-byte x", []byte("\x1b[M A\xc3\xa9"), false},
		// Legacy raw X10 reports whose coordinate bytes are also valid UTF-8
		// (x in 0xc2-0xdf, y in 0x80-0xbf: column 162+, row 96+) — the
		// ambiguity skipX10Mouse resolves.
		{"two raw x10 clicks whose coordinates form UTF-8", []byte("\x1b[M \xc3\xa9\x1b[M#\xc3\xa9"), false},
		{"raw x10 click whose coordinates form UTF-8 then a real key", []byte("\x1b[M \xc3\xa9q"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDismissKey(tt.frame, nil); got != tt.want {
				t.Errorf("isDismissKey(%q) = %v, want %v", tt.frame, got, tt.want)
			}
		})
	}
}

// TestIsDismissKeyMouseMode pins the disambiguation #814 adds: the same bytes
// are a mouse report under 1005 and a legacy report plus a key without it.
func TestIsDismissKeyMouseMode(t *testing.T) {
	// legacy click col 195/row 169 + "q", or 1005 button 0/x 200/y 80
	const ambiguous = "\x1b[M \xc3\xa9q"
	tracker := func(set bool) *mouseModeTracker {
		m := &mouseModeTracker{}
		if set {
			m.Feed([]byte("\x1b[?1005h"))
		} else {
			m.Feed([]byte("\x1b[?1005l"))
		}
		return m
	}

	tests := []struct {
		name  string
		frame []byte
		mode  *mouseModeTracker
		want  bool
	}{
		{"ambiguous frame under 1005 is a mouse report", []byte(ambiguous), tracker(true), false},
		{"ambiguous frame without 1005 is legacy + key", []byte(ambiguous), tracker(false), true},
		{"unambiguous 1005 two-byte x under 1005", []byte("\x1b[M\xc3\xa9!!"), tracker(true), false},
		{"ambiguous legacy without 1005", []byte("\x1b[M \xc3\xa9"), tracker(false), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDismissKey(tt.frame, tt.mode); got != tt.want {
				t.Errorf("isDismissKey(%q) = %v, want %v", tt.frame, got, tt.want)
			}
		})
	}
}

// Each case ends with a sentinel "z" frame, so a missing or extra send shows
// up as the wrong next send rather than as a timeout. The sentinel is itself
// a dismiss key, so its own guard lands in got right before the send-keys
// that ends the case — every want ends with deadKeyCmd("%7").
func TestPumpInputSendOrder(t *testing.T) {
	guard := deadKeyCmd("%7")
	modalClear := modalClearCmd("%7")
	// A lone Escape is now carried until the grace expires or a next frame
	// completes it, so this test drives a short grace and waits it out for the
	// lone-escape case (which must then land alone, before the sentinel).
	oldGrace := escCarryGrace
	escCarryGrace = 20 * time.Millisecond
	t.Cleanup(func() { escCarryGrace = oldGrace })
	tests := []struct {
		name      string
		frame     []byte
		want      []string
		graceWait bool
	}{
		{
			name:      "lone escape",
			frame:     []byte{0x1b},
			want:      []string{guard, "send-keys -H -t %7 1b", modalClear, guard},
			graceWait: true,
		},
		{
			name:  "lone ctrl-c",
			frame: []byte{0x03},
			want:  []string{guard, "send-keys -H -t %7 03", modalClear, guard},
		},
		{
			name:  "escape sequence (up arrow)",
			frame: []byte{0x1b, 0x5b, 0x41},
			want:  []string{guard, "send-keys -H -t %7 1b 5b 41", guard},
		},
		{
			name:  "ordinary keystroke",
			frame: []byte("a"),
			want:  []string{guard, "send-keys -H -t %7 61", guard},
		},
		{
			name:  "sgr mouse click",
			frame: []byte("\x1b[<0;5;5M"),
			want:  []string{"send-keys -H -t %7 1b 5b 3c 30 3b 35 3b 35 4d", guard},
		},
		{
			name:  "focus-in",
			frame: []byte("\x1b[I"),
			want:  []string{"send-keys -H -t %7 1b 5b 49", guard},
		},
		{
			name:  "focus then a mouse click in one flush",
			frame: []byte("\x1b[I\x1b[<0;5;5M"),
			want:  []string{"send-keys -H -t %7 1b 5b 49 1b 5b 3c 30 3b 35 3b 35 4d", guard},
		},
		{
			name:  "bracketed paste",
			frame: []byte("\x1b[200~hi\x1b[201~"),
			want:  []string{guard, "send-keys -H -t %7 1b 5b 32 30 30 7e 68 69 1b 5b 32 30 31 7e", guard},
		},
	}

	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	sendCh := make(chan string, 8)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
			if err := wire.WriteFrame(peer, wire.FrameInput, tt.frame); err != nil {
				t.Fatalf("write input: %v", err)
			}
			if tt.graceWait {
				time.Sleep(100 * time.Millisecond)
			}
			if err := wire.WriteFrame(peer, wire.FrameInput, []byte("z")); err != nil {
				t.Fatalf("write sentinel: %v", err)
			}

			var got []string
			for {
				select {
				case s := <-sendCh:
					if s == "send-keys -H -t %7 7a" {
						if !slices.Equal(got, tt.want) {
							t.Errorf("send calls = %q, want %q", got, tt.want)
						}
						return
					}
					got = append(got, s)
				case <-time.After(5 * time.Second):
					t.Fatalf("sentinel send-keys never arrived; got %q so far", got)
				}
			}
		})
	}
}

// TestPumpInput1005ClickDoesNotDismiss is the #814 regression through the
// production entry point: an ambiguous ESC[M frame is a mouse report while the
// pane's 1005 flag is set, so a click at column 96+ on a dead pane must not
// emit the dead-key guard; with 1005 clear the same bytes are legacy + a key
// and the guard is owed.
func TestPumpInput1005ClickDoesNotDismiss(t *testing.T) {
	const frame = "\x1b[M \xc3\xa9q"
	const framingSend = "send-keys -H -t %7 1b 5b 4d 20 c3 a9 71"
	tests := []struct {
		name string
		utf8 bool
		want bool // dead-key guard expected
	}{
		{"1005 mode: mouse report, no guard", true, false},
		{"legacy: report + key, guard", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer func() { _ = conn.Close() }()
			defer func() { _ = peer.Close() }()

			mode := &mouseModeTracker{}
			if tt.utf8 {
				mode.Feed([]byte("\x1b[?1005h"))
			} else {
				mode.Feed([]byte("\x1b[?1005l"))
			}

			sendCh := make(chan string, 8)
			go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, func() *mouseModeTracker { return mode })

			_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
			if err := wire.WriteFrame(peer, wire.FrameInput, []byte(frame)); err != nil {
				t.Fatalf("write input: %v", err)
			}

			guard := deadKeyCmd("%7")
			var got []string
			deadline := time.After(5 * time.Second)
			for {
				select {
				case s := <-sendCh:
					if s == framingSend {
						if tt.want && !slices.Contains(got, guard) {
							t.Fatalf("no dead-key guard sent for the legacy reading; got %q", got)
						}
						if !tt.want && slices.Contains(got, guard) {
							t.Fatalf("dead-key guard sent for a 1005 mouse report; got %q", got)
						}
						return
					}
					got = append(got, s)
				case <-deadline:
					t.Fatalf("the frame's send-keys never arrived; got %q", got)
				}
			}
		})
	}
}

// TestPumpInputTracksSinkReplacementWhileRunning pins that the tracker is
// resolved off the router per frame: a sink registered over a live pumpInput —
// resetWindow's revival on the surviving conn — is picked up rather than the
// tracker the pump resolved first.
func TestPumpInputTracksSinkReplacementWhileRunning(t *testing.T) {
	router := NewRouter()
	first := &outputSink{}
	first.mouse.Feed([]byte("\x1b[?1005l"))
	router.Register("%7", first)

	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()

	sendCh := make(chan string, 8)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil, sinkMouseResolver(router, "%7"))

	// Known legacy: the ambiguous frame is a report plus a key -> guard.
	assertAmbiguousFrame(t, peer, sendCh, true)

	// Replace the sink under the live pump; now 1005 is set.
	second := &outputSink{}
	second.mouse.Feed([]byte("\x1b[?1005h"))
	router.Register("%7", second)

	// The same bytes are now a mouse report -> no guard.
	assertAmbiguousFrame(t, peer, sendCh, false)
}

// assertAmbiguousFrame writes the #814 ambiguous frame and reads sends until its
// send-keys lands, asserting whether the dead-key guard preceded it.
func assertAmbiguousFrame(t *testing.T, peer net.Conn, sendCh <-chan string, wantGuard bool) {
	t.Helper()
	const framingSend = "send-keys -H -t %7 1b 5b 4d 20 c3 a9 71"
	if err := wire.WriteFrame(peer, wire.FrameInput, []byte("\x1b[M \xc3\xa9q")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	guard := deadKeyCmd("%7")
	var got []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-sendCh:
			if s == framingSend {
				if wantGuard && !slices.Contains(got, guard) {
					t.Fatalf("no dead-key guard sent; got %q", got)
				}
				if !wantGuard && slices.Contains(got, guard) {
					t.Fatalf("dead-key guard sent for a 1005 mouse report; got %q", got)
				}
				return
			}
			got = append(got, s)
		case <-deadline:
			t.Fatalf("the frame's send-keys never arrived; got %q", got)
		}
	}
}

// TestPumpInputPasteMarkerSplitByCarryFlush pins the production chain behind
// a split paste marker: pumpInput carries an incomplete trailing escape, but
// flushes it alone once the grace passes, so a late next frame splits the end
// marker across two handle calls. The ctrl+v after the paste must still be
// swallowed into an image paste, never forwarded as a raw 0x16.
func TestPumpInputPasteMarkerSplitByCarryFlush(t *testing.T) {
	oldGrace := escCarryGrace
	escCarryGrace = 20 * time.Millisecond
	t.Cleanup(func() { escCarryGrace = oldGrace })

	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()
	f := newPasteFixture()
	sends := make(chan string, 32)
	go pumpInput(conn, "%1", func(s string) { sends <- s }, f.h, nil, nil, nil)

	write := func(s string) {
		t.Helper()
		if err := wire.WriteFrame(peer, wire.FrameInput, []byte(s)); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}
	write("\x1b[200~x\x1b[20")
	time.Sleep(100 * time.Millisecond)
	write("1~")
	write("\x16")

	select {
	case s := <-f.sent:
		if !strings.HasPrefix(s, "send-keys -H -t %1 ") {
			t.Errorf("send %q is not a hex send-keys to the pane", s)
		}
	case msg := <-f.notified:
		t.Fatalf("unexpected notify: %q", msg)
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl+v after the split end marker produced no paste")
	}

	for {
		select {
		case s := <-sends:
			if s == "send-keys -H -t %1 16" {
				t.Fatalf("ctrl+v was forwarded to the pane: %q", s)
			}
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}
