package daemon

import (
	"net"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestOutputSinkResizeUnsplit pins the corruption guard (C5's trap): a
// FrameResize must reach the wire as the one frame it was enqueued as, never
// routed through wire.WriteStream, which would reject it since it isn't a
// byte-stream type.
func TestOutputSinkResizeUnsplit(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()

	s := &outputSink{ch: make(chan sinkFrame, outputSinkBuf)}
	s.enqueue(wire.FrameResize, wire.EncodeResize(120, 40))
	s.start(remote)

	f, err := wire.ReadFrame(local)
	if err != nil {
		t.Fatalf("read resize: %v", err)
	}
	if f.Type != wire.FrameResize {
		t.Fatalf("frame type = %v, want FrameResize", f.Type)
	}
	w, h, err := wire.DecodeResize(f.Payload)
	if err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if w != 120 || h != 40 {
		t.Fatalf("resize = %dx%d, want 120x40", w, h)
	}

	s.Close()
	s.Wait()

	if err := local.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadFrame(local); err == nil {
		t.Fatal("resize arrived as more than one frame")
	}
}

// TestOutputSinkMultiFrameOutputByteIdentical: several FrameOutput frames
// queued before the pump starts (so drainOutput sees the whole burst, per
// newOutputSink's doc) must arrive at the wire byte-identical to their
// concatenation, having gone through wire.WriteStream rather than the
// unsplit wire.WriteFrame.
func TestOutputSinkMultiFrameOutputByteIdentical(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()

	chunks := [][]byte{
		[]byte("first chunk of pane output\n"),
		[]byte("second chunk right behind it\n"),
		[]byte("third and final chunk\n"),
	}
	var want []byte
	for _, c := range chunks {
		want = append(want, c...)
	}

	s := &outputSink{ch: make(chan sinkFrame, outputSinkBuf)}
	for _, c := range chunks {
		s.ch <- sinkFrame{typ: wire.FrameOutput, payload: c}
	}
	s.start(remote)

	got := readAllFrames(t, local, 200*time.Millisecond)
	if got != string(want) {
		t.Fatalf("sink output = %q, want %q", got, want)
	}

	s.Close()
	s.Wait()
}

// TestOutputSinkSeedStillWorks: a FrameSeed enqueued on the sink still
// arrives whole and untouched now that the pump branches its write path on
// frame type.
func TestOutputSinkSeedStillWorks(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()

	s := &outputSink{ch: make(chan sinkFrame, outputSinkBuf)}
	s.enqueue(wire.FrameSeed, []byte("initial screen capture"))
	s.start(remote)

	f, err := wire.ReadFrame(local)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if f.Type != wire.FrameSeed {
		t.Fatalf("frame type = %v, want FrameSeed", f.Type)
	}
	if string(f.Payload) != "initial screen capture" {
		t.Fatalf("seed payload = %q", f.Payload)
	}

	s.Close()
	s.Wait()
}
