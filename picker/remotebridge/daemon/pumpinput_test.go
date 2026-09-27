package daemon

import (
	"net"
	"slices"
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
	defer conn.Close()

	var diedCount int32
	done := make(chan struct{})
	go func() {
		pumpInput(conn, "%7", func(string) {}, nil, func() { atomic.AddInt32(&diedCount, 1) }, nil)
		close(done)
	}()

	peer.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpInput did not return after its connection closed")
	}

	if n := atomic.LoadInt32(&diedCount); n != 1 {
		t.Errorf("died called %d times, want exactly 1", n)
	}
}

// A clean FrameInput read must not fire died — only a read error (the
// connection actually closing) may, or ordinary keystrokes would keep waking
// the sweep.
func TestPumpInputDoesNotCallDiedOnACleanFrameRead(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()

	var diedCount int32
	sendCh := make(chan string, 1)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, func() { atomic.AddInt32(&diedCount, 1) }, nil)

	peer.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}

	select {
	case <-sendCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no send-keys reached the sink: the frame was never forwarded")
	}
	if n := atomic.LoadInt32(&diedCount); n != 0 {
		t.Errorf("died called %d times after a clean frame read, want 0 — the connection is still open", n)
	}
}

// died's own doc comment promises nil is legal — every call site that
// predates this field, and any bare Config{} a test builds directly, must
// not panic or hang.
func TestPumpInputToleratesANilDiedCallback(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		pumpInput(conn, "%7", func(string) {}, nil, nil, nil)
		close(done)
	}()

	peer.Close()

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
	defer conn.Close()
	defer peer.Close()

	var seen int32
	sendCh := make(chan int32, 1)
	go pumpInput(conn, "%7", func(string) { sendCh <- atomic.LoadInt32(&seen) }, nil, nil, func() { atomic.AddInt32(&seen, 1) })

	peer.SetDeadline(time.Now().Add(5 * time.Second))
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
			if got := isDismissKey(tt.frame); got != tt.want {
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
	tests := []struct {
		name  string
		frame []byte
		want  []string
	}{
		{
			name:  "lone escape",
			frame: []byte{0x1b},
			want:  []string{guard, "send-keys -H -t %7 1b", modalClear, guard},
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
	defer conn.Close()
	defer peer.Close()

	sendCh := make(chan string, 8)
	go pumpInput(conn, "%7", func(s string) { sendCh <- s }, nil, nil, nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer.SetDeadline(time.Now().Add(5 * time.Second))
			if err := wire.WriteFrame(peer, wire.FrameInput, tt.frame); err != nil {
				t.Fatalf("write input: %v", err)
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
