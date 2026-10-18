package daemon

import (
	"bytes"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

const holdLeaveAlt = "\x1b[?1049l"

func holdPipe(t *testing.T) (*outputSink, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = peer.Close() })
	s := newOutputSink(local, nil)
	t.Cleanup(s.Close)
	return s, peer
}

func readHoldFrame(t *testing.T, peer net.Conn) wire.Frame {
	t.Helper()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

func requireFrame(t *testing.T, peer net.Conn, typ wire.FrameType, payload string) {
	t.Helper()
	f := readHoldFrame(t, peer)
	if f.Type != typ || string(f.Payload) != payload {
		t.Fatalf("frame = %v %q, want %v %q", f.Type, f.Payload, typ, payload)
	}
}

func requireSilence(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetDeadline(time.Now().Add(200 * time.Millisecond))
	f, err := wire.ReadFrame(peer)
	if err == nil {
		t.Fatalf("unexpected frame %v %q", f.Type, f.Payload)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error = %v, want a deadline timeout", err)
	}
}

// settleDirty polls takeDirty until the pump has drained or the deadline
// passes; takeDirty reports only once the channel is empty.
func settleDirty(t *testing.T, s *outputSink, wait time.Duration) (int, bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if n, ok := s.takeDirty(); ok {
			return n, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSinkHoldThenSeedThenReleaseWritesNoRelease(t *testing.T) {
	s, peer := holdPipe(t)
	const seed = holdLeaveAlt + "SEED"
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	s.enqueue(wire.FrameSeed, []byte(seed))
	s.release()

	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	requireFrame(t, peer, wire.FrameSeed, seed)
	requireSilence(t, peer)
	if n, ok := settleDirty(t, s, 200*time.Millisecond); ok {
		t.Errorf("takeDirty = %d, true; a seed that released the hold owes no reseed", n)
	}
}

func TestSinkHoldThenReleaseLeavesAltScreenAndOwesReseed(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	s.release()

	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	requireFrame(t, peer, wire.FrameOutput, holdLeaveAlt)
	if n, ok := settleDirty(t, s, 5*time.Second); !ok || n != 1 {
		t.Errorf("takeDirty = %d, %v; want 1, true", n, ok)
	}
}

func TestSinkHoldReleaseWithoutHoldWritesNothing(t *testing.T) {
	s, peer := holdPipe(t)
	s.release()

	requireSilence(t, peer)
	if n, ok := settleDirty(t, s, 200*time.Millisecond); ok {
		t.Errorf("takeDirty = %d, true; a release with no hold owes no reseed", n)
	}
}

func TestSinkHoldFullChannelDoesNotCountAsDropped(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.ch <- sinkFrame{typ: wire.FrameOutput}

	if s.hold([]byte("HOLD")) {
		t.Fatal("hold = true on a full channel")
	}
	if s.dropped != 0 {
		t.Errorf("dropped = %d, want 0: nothing from the remote stream was lost", s.dropped)
	}
}

func TestSinkHoldRefusedWhenPausedOrClosed(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.pause()
	if s.hold([]byte("HOLD")) {
		t.Error("hold = true on a paused sink")
	}
	s.resume()
	s.closed = true
	if s.hold([]byte("HOLD")) {
		t.Error("hold = true on a closed sink")
	}
	if len(s.ch) != 0 {
		t.Errorf("%d frames queued by refused holds", len(s.ch))
	}
}

func TestSinkHoldNilSinkIsSafe(t *testing.T) {
	var s *outputSink
	if s.hold([]byte("HOLD")) {
		t.Error("hold = true on a nil sink")
	}
	s.release()
}

func TestSinkHoldReleaseFullChannelCountsAsDropped(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.ch <- sinkFrame{typ: wire.FrameOutput}

	s.release()
	if s.dropped != 1 {
		t.Errorf("dropped = %d, want 1: the reseed that follows writes a seed, which releases", s.dropped)
	}
}

func TestSinkHoldReleaseOnClosedSinkIsNoop(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1), closed: true}
	s.release()
	if len(s.ch) != 0 || s.dropped != 0 {
		t.Errorf("queued %d, dropped %d on a closed sink", len(s.ch), s.dropped)
	}
}

func TestPredictorHoldUnpaintsAndDropsModel(t *testing.T) {
	h := newHarness(t, "$ ", 2, 0, harnessOpts{})
	h.confirm("a")
	h.keys("b")
	h.requireFaint(3, 0, "b")
	model := h.model()
	epoch := h.p.epoch

	out := h.p.onHold()
	if !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
		t.Errorf("onHold = %q, want an unpaint", out)
	}
	h.writeLocal(out)
	h.settled()
	if h.p.model != nil {
		t.Error("onHold kept the model; it tracked the main screen")
	}
	requireDrained(t, model)
	if len(h.p.preds) != 0 || h.p.epoch == epoch {
		t.Errorf("preds %v epoch %d -> %d; onHold must reset", h.p.preds, epoch, h.p.epoch)
	}
	if out := h.p.onHold(); out != nil {
		t.Errorf("onHold with nothing painted = %q, want nil", out)
	}
}
