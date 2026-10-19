package daemon

import (
	"bytes"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/render"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

const holdLeaveAlt = "\x1b[?1049l"

// holdPipe is a plain sink whose renderer end the test reads. Cleanup waits for
// the pump, which reads holdFreezeMax, so a test's restore of it cannot race.
func holdPipe(t *testing.T) (*outputSink, net.Conn) {
	t.Helper()
	return holdPipeGfx(t, nil)
}

func holdPipeGfx(t *testing.T, gfx *graphics.Proxy) (*outputSink, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	s := newOutputSink(local, gfx)
	t.Cleanup(func() {
		s.Close()
		_ = local.Close()
		_ = peer.Close()
		s.Wait()
	})
	return s, peer
}

// shrinkHoldFreeze sets holdFreezeMax for one test. Call it before holdPipe, so
// the restore runs after the pump has exited.
func shrinkHoldFreeze(t *testing.T, d time.Duration) {
	t.Helper()
	saved := holdFreezeMax
	holdFreezeMax = d
	t.Cleanup(func() { holdFreezeMax = saved })
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

// TestSinkHoldReleaseWhileHeldOwesReseed: ?1049l would expose the main screen
// tmux reflowed on the way out, so a release of a pane still held writes
// nothing and asks for a reseed, whose seed releases.
func TestSinkHoldReleaseWhileHeldOwesReseed(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	s.release()

	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	if n, ok := settleDirty(t, s, 5*time.Second); !ok || n != 1 {
		t.Errorf("takeDirty = %d, %v; want 1, true", n, ok)
	}
	requireSilence(t, peer)
	if !s.isHeld() {
		t.Error("isHeld = false after a release with no seed")
	}

	const seed = holdLeaveAlt + "SEED"
	s.enqueue(wire.FrameSeed, []byte(seed))
	requireFrame(t, peer, wire.FrameSeed, seed)
	if s.isHeld() {
		t.Error("isHeld = true after the seed")
	}
}

// TestSinkHoldDiscardsOutputUntilSeed: output reaching a held pane would move
// the parked cursor and write the remote's stream over the copy; the seed's
// capture carries its screen effects instead.
func TestSinkHoldDiscardsOutputUntilSeed(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	if !s.isHeld() {
		t.Error("isHeld = false after the hold was written")
	}
	_, _ = s.Write([]byte("X"))
	const seed = holdLeaveAlt + "SEED"
	s.enqueue(wire.FrameSeed, []byte(seed))

	requireFrame(t, peer, wire.FrameSeed, seed)
	requireSilence(t, peer)
	if s.isHeld() {
		t.Error("isHeld = true after the seed")
	}

	_, _ = s.Write([]byte("Y"))
	requireFrame(t, peer, wire.FrameOutput, "Y")
}

// TestSinkHoldKeepsKittyStoreFromDiscardedOutput: gfx still filters output
// while held, so a store in discarded output is replayed ahead of the seed.
func TestSinkHoldKeepsKittyStoreFromDiscardedOutput(t *testing.T) {
	s, peer := holdPipeGfx(t, graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil))
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	_, _ = s.Write(testKittyStore("9"))
	s.enqueue(wire.FrameSeed, []byte("SEED"))

	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	replay := readHoldFrame(t, peer)
	if replay.Type != wire.FrameOutput || !bytes.Contains(replay.Payload, []byte(kittyLocalisedMarker)) {
		t.Fatalf("frame after the hold = %v %q, want the retained store's replay", replay.Type, replay.Payload)
	}
	requireFrame(t, peer, wire.FrameSeed, "SEED")
	requireSilence(t, peer)
}

// TestSinkHoldDiscardedOutputLeavesMouseTracker: the tracker mirrors what the
// renderer was written, so a DECSET in discarded output must not reach it; the
// seed's clear-then-set brings it up to date.
func TestSinkHoldDiscardedOutputLeavesMouseTracker(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	_, _ = s.Write([]byte("\x1b[?1005h"))
	s.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))

	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	if f := readHoldFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("frame after the hold = %v %q, want the resize", f.Type, f.Payload)
	}
	if _, known := s.mouse.MouseUTF8(); known {
		t.Fatal("mouse mode known after discarded output; the tracker followed bytes never written")
	}

	const seed = holdLeaveAlt + "\x1b[?1005h"
	s.enqueue(wire.FrameSeed, []byte(seed))
	requireFrame(t, peer, wire.FrameSeed, seed)
	if utf8, known := s.mouse.MouseUTF8(); !utf8 || !known {
		t.Errorf("MouseUTF8 = %v, %v after the seed; want true, true", utf8, known)
	}
}

// TestSinkHoldWhileHeldIsDropped: a second hold would copy the held copy, maybe
// already cut by a shrink, and restart the deadline.
func TestSinkHoldWhileHeldIsDropped(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD1")) || !s.hold([]byte("HOLD2")) {
		t.Fatal("hold = false on a live sink")
	}
	s.enqueue(wire.FrameSeed, []byte("SEED"))

	requireFrame(t, peer, wire.FrameOutput, "HOLD1")
	requireFrame(t, peer, wire.FrameSeed, "SEED")
	requireSilence(t, peer)
}

// TestSinkHoldAfterSeedIsWritten: a seed between two holds released the first,
// so the second is a fresh hold, written in stream order.
func TestSinkHoldAfterSeedIsWritten(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD1")) {
		t.Fatal("hold = false on a live sink")
	}
	s.enqueue(wire.FrameSeed, []byte("SEED"))
	if !s.hold([]byte("HOLD2")) {
		t.Fatal("hold = false on a live sink")
	}

	requireFrame(t, peer, wire.FrameOutput, "HOLD1")
	requireFrame(t, peer, wire.FrameSeed, "SEED")
	requireFrame(t, peer, wire.FrameOutput, "HOLD2")
	if !s.isHeld() {
		t.Error("isHeld = false after the second hold")
	}
}

// TestSinkHoldDeadlineReleases: a seed that never comes leaves the pane held
// no longer than holdFreezeMax; output flows again after it, and the discarded
// output is owed a reseed.
func TestSinkHoldDeadlineReleases(t *testing.T) {
	shrinkHoldFreeze(t, 50*time.Millisecond)
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	_, _ = s.Write([]byte("X"))

	requireFrame(t, peer, wire.FrameOutput, holdLeaveAlt)
	if s.isHeld() {
		t.Error("isHeld = true after the deadline")
	}
	_, _ = s.Write([]byte("Y"))
	requireFrame(t, peer, wire.FrameOutput, "Y")
	if n, ok := settleDirty(t, s, 5*time.Second); !ok || n != 1 {
		t.Errorf("takeDirty = %d, %v; want 1, true", n, ok)
	}
}

// TestSinkHoldDeadlineIsHoldFreezeMax pins the real constant: the deadline
// release comes no earlier than about a second after the hold.
func TestSinkHoldDeadlineIsHoldFreezeMax(t *testing.T) {
	s, peer := holdPipe(t)
	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	requireFrame(t, peer, wire.FrameOutput, "HOLD")
	start := time.Now()
	requireFrame(t, peer, wire.FrameOutput, holdLeaveAlt)
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Errorf("deadline release after %v, want no earlier than 0.9s", d)
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

// TestPredictSinkHoldTurnsPredictionOff: while held, keys paint nothing, a
// model seed builds no model and output is neither written nor observed; after
// the seed releases, a model seed and keys predict again.
func TestPredictSinkHoldTurnsPredictionOff(t *testing.T) {
	s, peer := newPipeSink(t)
	s.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	readSinkFrame(t, peer)
	m := plainModes(24)
	m.cx = 2
	s.predictSeed(render.Seed([]byte("$ "), 2, 0, false, false, nil, nil), m)
	s.keys([]byte("a"))
	_, _ = s.Write([]byte("a"))
	readOutput(t, peer)
	s.keys([]byte("b"))
	if out := readOutput(t, peer); !bytes.Contains(out, []byte("\x1b[0;2mb")) {
		t.Fatalf("frame before the hold = %q, want a paint of b", out)
	}

	if !s.hold([]byte("HOLD")) {
		t.Fatal("hold = false on a live sink")
	}
	if out := readOutput(t, peer); !bytes.HasPrefix(out, []byte("\x1b8\x1b7")) {
		t.Fatalf("frame ahead of the hold = %q, want the unpaint", out)
	}
	requireFrame(t, peer, wire.FrameOutput, "HOLD")

	m.cx = 3
	s.predictSeed(render.Seed([]byte("$ a"), 3, 0, false, false, nil, nil), m)
	s.keys([]byte("c"))
	// A lone UTF-8 lead byte leaves the stream mid-character if observed.
	_, _ = s.Write([]byte("\xe2"))
	s.keys([]byte("d"))
	s.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	if f := readSinkFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("frame while held = %v %q, want only the resize", f.Type, f.Payload)
	}
	// The pump wrote the resize after handling everything queued ahead of it,
	// and touches neither field again until the next frame.
	if s.pred.model != nil {
		t.Error("a model seed while held built a model")
	}
	if !s.pred.ground.atGround() {
		t.Error("output while held was observed")
	}

	seed := render.Seed([]byte("$ a"), 3, 0, false, false, nil, nil)
	s.enqueue(wire.FrameSeed, seed)
	if f := readSinkFrame(t, peer); f.Type != wire.FrameSeed {
		t.Fatalf("frame = %v %q, want the seed", f.Type, f.Payload)
	}
	s.predictSeed(seed, m)
	s.keys([]byte("e"))
	_, _ = s.Write([]byte("e"))
	if out := readOutput(t, peer); string(out) != "e" {
		t.Fatalf("echo after the seed = %q, want the bare echo", out)
	}
	s.keys([]byte("f"))
	if out := readOutput(t, peer); !bytes.Contains(out, []byte("\x1b[0;2mf")) {
		t.Fatalf("frame after the seed = %q, want a paint of f", out)
	}
}
