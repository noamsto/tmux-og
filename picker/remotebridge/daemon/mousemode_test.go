package daemon

import (
	"net"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func TestMouseModeTracker(t *testing.T) {
	tests := []struct {
		name      string
		feeds     [][]byte
		wantUTF8  bool
		wantKnown bool
	}{
		{"set", [][]byte{[]byte("\x1b[?1005h")}, true, true},
		{"reset", [][]byte{[]byte("\x1b[?1005l")}, false, true},
		{"combined set", [][]byte{[]byte("\x1b[?1000;1005h")}, true, true},
		{"combined reset", [][]byte{[]byte("\x1b[?1005;1000l")}, false, true},
		{"combined other params stay unknown", [][]byte{[]byte("\x1b[?1000;1006h")}, false, false},
		{"ris clears a set flag", [][]byte{[]byte("\x1b[?1005h\x1bc")}, false, true},
		{"split across feeds", [][]byte{[]byte("\x1b[?10"), []byte("05h")}, true, true},
		{"esc alone at the end", [][]byte{[]byte("\x1b"), []byte("[?1005h")}, true, true},
		{"unrelated modes stay unknown", [][]byte{[]byte("\x1b[?1006h\x1b[?25l")}, false, false},
		{"text stays unknown", [][]byte{[]byte("hello \x1b[?1002h")}, false, false},
		{"set then reset", [][]byte{[]byte("\x1b[?1005h"), []byte("\x1b[?1005l")}, false, true},
		{"reset then set", [][]byte{[]byte("\x1b[?1005l\x1b[?1005h")}, true, true},
		{"malformed long tail dropped", [][]byte{[]byte("\x1b[?999999999999999999999999999999999999")}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m mouseModeTracker
			for _, f := range tt.feeds {
				m.Feed(f)
			}
			utf8, known := m.MouseUTF8()
			if utf8 != tt.wantUTF8 || known != tt.wantKnown {
				t.Errorf("MouseUTF8() = (%v, %v), want (%v, %v)", utf8, known, tt.wantUTF8, tt.wantKnown)
			}
		})
	}
}

func TestMouseModeTrackerNilIsUnknown(t *testing.T) {
	var m *mouseModeTracker
	if utf8, known := m.MouseUTF8(); utf8 || known {
		t.Errorf("nil tracker MouseUTF8() = (%v, %v), want (false, false)", utf8, known)
	}
}

// TestOutputSinkTracksMouseMode pins the pump path the classifier depends on:
// the sink feeds the tracker from FrameSeed and FrameOutput, not just Feed in
// isolation.
func TestOutputSinkTracksMouseMode(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()

	// Drain the renderer side continuously: net.Pipe is synchronous, so a pump
	// write blocks until read, exactly as a live renderer never lets it.
	go func() {
		for {
			if _, err := wire.ReadFrame(peer); err != nil {
				return
			}
		}
	}()

	s := newOutputSink(conn, nil)

	s.enqueue(wire.FrameSeed, []byte("\x1b[?1005h"))
	waitMouseMode(t, s, true, true)

	s.writeOwned([]byte("\x1b[?1005l"))
	waitMouseMode(t, s, false, true)
}

// waitMouseMode polls the tracker until it reports (wantUTF8, wantKnown) or the
// deadline passes. The sink pump runs on its own goroutine, so there is no
// happens-before edge to wait on but the atomic state.
func waitMouseMode(t *testing.T, s *outputSink, wantUTF8, wantKnown bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if utf8, known := s.mouse.MouseUTF8(); utf8 == wantUTF8 && known == wantKnown {
			return
		}
		time.Sleep(time.Millisecond)
	}
	utf8, known := s.mouse.MouseUTF8()
	t.Fatalf("MouseUTF8() = (%v, %v), want (%v, %v)", utf8, known, wantUTF8, wantKnown)
}
