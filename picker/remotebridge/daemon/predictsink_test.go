package daemon

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/noamsto/tmux-og/picker/remotebridge/render"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// newPipeSink is a predicting sink (shared On) whose renderer end the test
// reads. Cleanup closes the peer before waiting, so a pump blocked writing a
// frame nobody reads still exits.
func newPipeSink(t *testing.T) (*outputSink, net.Conn) {
	t.Helper()
	conn, peer := net.Pipe()
	s := newPredictingSink(conn, nil, newPredictShared(PredictOn))
	t.Cleanup(func() {
		s.Close()
		_ = peer.Close()
		s.Wait()
		_ = conn.Close()
	})
	return s, peer
}

func readSinkFrame(t *testing.T, peer net.Conn) wire.Frame {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// expectNoFrame asserts the pump wrote nothing within a short window.
func expectNoFrame(t *testing.T, peer net.Conn) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if f, err := wire.ReadFrame(peer); err == nil {
		t.Fatalf("unexpected frame %v %q", f.Type, f.Payload)
	}
}

func readOutput(t *testing.T, peer net.Conn) []byte {
	t.Helper()
	f := readSinkFrame(t, peer)
	if f.Type != wire.FrameOutput {
		t.Fatalf("frame type = %v %q, want FrameOutput", f.Type, f.Payload)
	}
	return f.Payload
}

func writeEmu(t *testing.T, e interface{ Write([]byte) (int, error) }, b []byte) {
	t.Helper()
	if _, err := e.Write(b); err != nil {
		t.Fatal(err)
	}
}

// waitModelWanted waits for the pump to have handled a key that asked for a
// model seed.
func waitModelWanted(t *testing.T, s *outputSink) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !s.modelWanted.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the key never requested a model seed")
		}
		time.Sleep(time.Millisecond)
	}
}

// primeAC1 drives a predicting sink to a painted, faint "s" after a "$ "
// prompt: the "l" before it is echoed, which confirms the epoch.
func primeAC1(t *testing.T, s *outputSink, peer net.Conn, local, ref interface{ Write([]byte) (int, error) }) {
	t.Helper()
	seed := render.Seed([]byte("$ "), 2, 0, false, false, nil, nil)
	s.enqueue(wire.FrameSeed, seed)
	f := readSinkFrame(t, peer)
	if f.Type != wire.FrameSeed {
		t.Fatalf("first frame = %v, want FrameSeed", f.Type)
	}
	writeEmu(t, local, f.Payload)
	writeEmu(t, ref, f.Payload)

	s.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	if f := readSinkFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("second frame = %v, want FrameResize", f.Type)
	}

	m := plainModes(24)
	m.cx, m.cy = 2, 0
	s.predictSeed(seed, m)
	s.keys([]byte("l"))
	_, _ = s.Write([]byte("l"))
	out := readOutput(t, peer)
	if string(out) != "l" {
		t.Fatalf("echo of the tentative key = %q, want the bare echo", out)
	}
	writeEmu(t, local, out)
	writeEmu(t, ref, []byte("l"))

	s.keys([]byte("s"))
	writeEmu(t, local, readOutput(t, peer))
}

// TestPredictSinkPaintsThroughPump is AC1 through the real pump: the second
// key of a run is painted dimmed before its echo, and the echo leaves the
// local pane equal to the remote's.
func TestPredictSinkPaintsThroughPump(t *testing.T) {
	s, peer := newPipeSink(t)
	local, ref := newTermEmu(t, 80, 24), newTermEmu(t, 80, 24)
	primeAC1(t, s, peer, local, ref)

	c := local.CellAt(3, 0)
	if c == nil || c.Content != "s" || c.Style.Attrs&uv.AttrFaint == 0 {
		t.Fatalf("cell(3,0) = %+v, want a faint s", c)
	}
	if p := local.CursorPosition(); p.X != 4 || p.Y != 0 {
		t.Fatalf("local cursor = %v, want 4,0", p)
	}

	_, _ = s.Write([]byte("s"))
	writeEmu(t, local, readOutput(t, peer))
	writeEmu(t, ref, []byte("s"))
	screenEq(t, local, ref)
	noFaint(t, local)
}

// TestPredictSinkModelSeedWritesNothing: a model seed never reaches the
// renderer, so the local pane's screen and history are untouched. The paint
// that follows proves the model was built.
func TestPredictSinkModelSeedWritesNothing(t *testing.T) {
	s, peer := newPipeSink(t)
	s.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	readSinkFrame(t, peer)

	m := plainModes(24)
	m.cx = 2
	s.predictSeed(render.Seed([]byte("$ "), 2, 0, false, false, nil, nil), m)
	expectNoFrame(t, peer)

	s.keys([]byte("a"))
	_, _ = s.Write([]byte("a"))
	readOutput(t, peer)
	s.keys([]byte("b"))
	if out := readOutput(t, peer); !bytes.Contains(out, []byte("\x1b[0;2mb")) {
		t.Fatalf("frame after the model seed = %q, want a paint of b", out)
	}
}

// TestPredictSinkOutputAfterCaptureReachesModel pins the spec's ordering: the
// model seed is enqueued as soon as its capture reply is parsed, so %output
// routed before the next pane's replies is fed to the model. Enqueued after
// the loop, the model would miss "ab" and paint d at column 3.
func TestPredictSinkOutputAfterCaptureReachesModel(t *testing.T) {
	sa, peerA := newPipeSink(t)
	sb, peerB := newPipeSink(t)
	go func() { _, _ = io.Copy(io.Discard, peerB) }()

	router := NewRouter()
	router.Register("%1", sa)
	router.Register("%2", sb)

	sa.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	readSinkFrame(t, peerA)
	sb.enqueue(wire.FrameResize, wire.EncodeResize(80, 24))
	// A key with no model asks for one; it stands in for the user typing.
	sa.keys([]byte("x"))
	sb.keys([]byte("x"))
	waitModelWanted(t, sa)
	waitModelWanted(t, sb)

	script := strings.Join([]string{
		"%begin 1 1 1", "2 0 0 0 0 0 0 0 0", "%end 1 1 1", // %1 cursor
		"%begin 1 2 1", "1|0|0|1|0|23", "%end 1 2 1", // %1 modes
		"%begin 1 3 1", "$ ", "%end 1 3 1", // %1 capture
		"%output %1 ab",
		"%begin 1 4 1", "0 0 0 0 0 0 0 0 0", "%end 1 4 1", // %2 cursor
		"%begin 1 5 1", "1|0|0|1|0|23", "%end 1 5 1", // %2 modes
		"%begin 1 6 1", "B", "%end 1 6 1", // %2 capture
	}, "\n") + "\n"
	seedPredictModels(router, scriptedRTRouter(script, router))

	if out := readOutput(t, peerA); string(out) != "ab" {
		t.Fatalf("routed output = %q, want ab", out)
	}
	sa.keys([]byte("c"))
	_, _ = sa.Write([]byte("c"))
	readOutput(t, peerA)
	sa.keys([]byte("d"))
	if out := readOutput(t, peerA); !bytes.Contains(out, []byte("\x1b[1;6H\x1b[0;2md")) {
		t.Fatalf("paint = %q, want a faint d at column 5", out)
	}
}

// TestPredictSinkCloseWhilePaintedUnpaints: closing a painted sink restores
// the pane before the pump exits, and closes the model.
func TestPredictSinkCloseWhilePaintedUnpaints(t *testing.T) {
	s, peer := newPipeSink(t)
	local, ref := newTermEmu(t, 80, 24), newTermEmu(t, 80, 24)
	primeAC1(t, s, peer, local, ref)
	// The pump is parked in its select after the paint it just wrote; it only
	// writes s.pred.model again in shutdown, after Close.
	m := s.pred.model
	if m == nil {
		t.Fatal("no model after a paint")
	}

	s.Close()
	var last []byte
	for {
		if err := peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		f, err := wire.ReadFrame(peer)
		if err != nil {
			break
		}
		last = f.Payload
		writeEmu(t, local, f.Payload)
	}
	s.Wait()

	if !bytes.HasPrefix(last, []byte("\x1b8\x1b7")) {
		t.Fatalf("last frame = %q, want an unpaint", last)
	}
	screenEq(t, local, ref)
	noFaint(t, local)
	if s.pred.model != nil {
		t.Error("model survived shutdown")
	}
	select {
	case <-m.drained:
	case <-time.After(time.Second):
		t.Fatal("the model's drain goroutine outlived the sink")
	}
}

// TestPredictSinkWriteErrorClosesModel: a pump that exits on a renderer write
// error closes the model too.
func TestPredictSinkWriteErrorClosesModel(t *testing.T) {
	s, peer := newPipeSink(t)
	local, ref := newTermEmu(t, 80, 24), newTermEmu(t, 80, 24)
	primeAC1(t, s, peer, local, ref)
	m := s.pred.model
	if m == nil {
		t.Fatal("no model after a paint")
	}

	_ = peer.Close()
	_, _ = s.Write([]byte("s"))
	s.Wait()
	requireDrained(t, m)
}

func bareSink(buf int) *outputSink {
	s := &outputSink{ch: make(chan sinkFrame, buf)}
	s.pred = newPredictor(newPredictShared(PredictOn), func() {}, s.isPaused, s.keysLost.Load)
	return s
}

// TestPredictSinkKeysFullChannelSetsKeysLost: a key frame lost to a full
// buffer costs the predictor a reset, never the pane a re-seed.
func TestPredictSinkKeysFullChannelSetsKeysLost(t *testing.T) {
	s := bareSink(1)
	s.ch <- sinkFrame{typ: wire.FrameOutput, payload: []byte("x")}

	s.keys([]byte("a"))
	if !s.keysLost.Load() {
		t.Error("keysLost not set")
	}
	s.predictSeed([]byte("seed"), plainModes(24))
	if s.dropped != 0 {
		t.Errorf("dropped = %d, want 0", s.dropped)
	}
	if len(s.ch) != 1 {
		t.Errorf("queued = %d, want only the original frame", len(s.ch))
	}

	var plain outputSink
	plain.ch = make(chan sinkFrame, 1)
	plain.keys([]byte("a"))
	plain.predictSeed([]byte("seed"), plainModes(24))
	if len(plain.ch) != 0 {
		t.Error("a sink without a predictor queued a predict frame")
	}
	var none *outputSink
	none.keys([]byte("a"))
}

// TestPredictSinkTakeModelWantedGates: the request waits out a paused,
// undrained or closed sink, and is spent once taken.
func TestPredictSinkTakeModelWantedGates(t *testing.T) {
	s := bareSink(2)
	if s.takeModelWanted() {
		t.Fatal("taken without a request")
	}
	s.modelWanted.Store(true)

	s.pause()
	if s.takeModelWanted() {
		t.Error("taken while paused")
	}
	s.resume()

	s.ch <- sinkFrame{typ: wire.FrameOutput}
	if s.takeModelWanted() {
		t.Error("taken while undrained")
	}
	<-s.ch

	if !s.takeModelWanted() {
		t.Fatal("not taken once drained")
	}
	if s.takeModelWanted() {
		t.Error("taken twice")
	}

	s.modelWanted.Store(true)
	s.Close()
	if s.takeModelWanted() {
		t.Error("taken after close")
	}
}

// TestSeedPredictModelsCommandOrder: cursor, modes, capture per pane, in one
// batch, and the seed carries the cursor in its modes.
func TestSeedPredictModelsCommandOrder(t *testing.T) {
	router := NewRouter()
	s := bareSink(outputSinkBuf)
	s.modelWanted.Store(true)
	router.Register("%1", s)

	script := strings.Join([]string{
		"%begin 1 1 1", "4 1 0 0 0 0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", "1|0|0|1|0|23", "%end 1 2 1",
		"%begin 1 3 1", "PROMPT", "%end 1 3 1",
	}, "\n") + "\n"
	log := &orderedLog{}
	seedPredictModels(router, scriptedRTRouterW(script, router, log))

	sent := strings.Join(log.entries, "")
	cur := strings.Index(sent, cursorCmd("%1"))
	modes := strings.Index(sent, predictModesCmd("%1"))
	capt := strings.Index(sent, captureCmd("%1"))
	if cur < 0 || modes <= cur || capt <= modes {
		t.Fatalf("commands at cursor %d, modes %d, capture %d in %q; want that order", cur, modes, capt, sent)
	}

	if len(s.ch) != 1 {
		t.Fatalf("queued %d frames, want one model seed", len(s.ch))
	}
	f := <-s.ch
	if f.typ != framePredictSeed || f.modes == nil {
		t.Fatalf("frame = %v, want framePredictSeed with modes", f.typ)
	}
	if f.modes.cx != 4 || f.modes.cy != 1 || !f.modes.cursorVisible || f.modes.bottom != 23 {
		t.Errorf("modes = %+v", *f.modes)
	}
	if !bytes.Contains(f.payload, []byte("PROMPT")) {
		t.Errorf("seed = %q, want the capture", f.payload)
	}
}

// TestSeedPredictModelsSkipsWithoutModesOrCapture: a remote that cannot
// report the modes, or a failed capture, builds no model.
func TestSeedPredictModelsSkipsWithoutModesOrCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modes   []string
		capture []string
	}{
		{"modes unreported", []string{"%begin 1 2 1", "||||", "%end 1 2 1"}, []string{"%begin 1 3 1", "PROMPT", "%end 1 3 1"}},
		{"capture error", []string{"%begin 1 2 1", "1|0|0|1|0|23", "%end 1 2 1"}, []string{"%begin 1 3 1", "%error 1 3 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter()
			s := bareSink(outputSinkBuf)
			s.modelWanted.Store(true)
			router.Register("%1", s)

			lines := append([]string{"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1"}, tc.modes...)
			lines = append(lines, tc.capture...)
			seedPredictModels(router, scriptedRTRouter(strings.Join(lines, "\n")+"\n", router))

			if len(s.ch) != 0 {
				t.Errorf("queued %d frames, want none", len(s.ch))
			}
		})
	}
}
