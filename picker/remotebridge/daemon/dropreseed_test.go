package daemon

import (
	"net"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestSinkDirtyOnlyOnceDrained pins the gate: a drop is remembered, but not
// acted on until the pane has caught up — re-seeding a congested pane just
// feeds the queue that is already behind.
func TestSinkDirtyOnlyOnceDrained(t *testing.T) {
	// No pump: the channel is the only consumer, so it fills deterministically.
	s := &outputSink{ch: make(chan sinkFrame, 1)}

	s.Write([]byte("kept"))
	s.Write([]byte("dropped"))
	if n, dirty := s.takeDirty(); dirty {
		t.Fatalf("takeDirty = %d, %v while the queue is still full; want not dirty", n, dirty)
	}

	<-s.ch
	n, dirty := s.takeDirty()
	if !dirty || n != 1 {
		t.Fatalf("takeDirty = %d, %v after draining; want 1, true", n, dirty)
	}
	if _, dirty := s.takeDirty(); dirty {
		t.Error("takeDirty must clear the count")
	}
}

// TestSinkPausedIsNotDirty keeps the two recovery paths from firing at once:
// a paused pane is already owed a seed by its %continue.
func TestSinkPausedIsNotDirty(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.Write([]byte("kept"))
	s.Write([]byte("dropped"))
	<-s.ch
	s.pause()
	if _, dirty := s.takeDirty(); dirty {
		t.Error("a paused sink must not ask for a re-seed")
	}
}

// TestDirtyPanesSkipsNonSinks guards the router's mixed map: tests register
// plain io.Writer fakes, which have nothing to re-seed.
func TestDirtyPanesSkipsNonSinks(t *testing.T) {
	r := NewRouter()
	r.Register("%9", &fakeSink{})

	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.Write([]byte("kept"))
	s.Write([]byte("dropped"))
	<-s.ch
	r.Register("%1", s)

	if got := r.dirtyPanes(); len(got) != 1 || got[0] != "%1" {
		t.Fatalf("dirtyPanes = %v, want [%%1]", got)
	}
}

// TestReseedDroppedRepaintsFromCapture is the fix end to end: a pane that lost
// a frame gets capture-pane's ground truth pushed to its renderer.
func TestReseedDroppedRepaintsFromCapture(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	s := newOutputSink(local, nil)
	s.mu.Lock()
	s.dropped = 3
	s.mu.Unlock()
	router := NewRouter()
	router.Register("%1", s)

	// PaneSeed's two round-trips: cursor, then capture.
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1",
		"0 0 0 0",
		"%end 1 1 1",
		"%begin 1 2 1",
		"FRESH-CAPTURE",
		"%end 1 2 1",
	}, "\n") + "\n")

	go reseedDropped(router, rt)

	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Type != wire.FrameSeed || !strings.Contains(string(f.Payload), "FRESH-CAPTURE") {
		t.Fatalf("frame = %v %q, want a seed carrying FRESH-CAPTURE", f.Type, f.Payload)
	}
}

// TestReseedDroppedReplaysRetainedKittyStoreBeforeSeed pins #465/#731 on the
// drop-recovery path: the last localised store for each id lands first, then
// the seed that repaints the placeholders referencing it.
func TestReseedDroppedReplaysRetainedKittyStoreBeforeSeed(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	s := newOutputSink(local, p)
	s.Write(testKittyStore("5"))
	storeFrame, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read store frame: %v", err)
	}
	if storeFrame.Type != wire.FrameOutput || !strings.Contains(string(storeFrame.Payload), kittyLocalisedMarker) {
		t.Fatalf("store frame = %v %q", storeFrame.Type, storeFrame.Payload)
	}

	s.mu.Lock()
	s.dropped = 1
	s.mu.Unlock()

	router := NewRouter()
	router.Register("%1", s)

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1",
		"0 0 0 0",
		"%end 1 1 1",
		"%begin 1 2 1",
		"FRESH-CAPTURE",
		"%end 1 2 1",
	}, "\n") + "\n")

	go reseedDropped(router, rt)

	replay, seed := replayThenSeedFrames(t, peer)
	if replay.Type != wire.FrameOutput || !strings.Contains(string(replay.Payload), kittyLocalisedMarker) {
		t.Fatalf("replay = %v %q, want localised store before seed", replay.Type, replay.Payload)
	}
	if seed.Type != wire.FrameSeed || !strings.Contains(string(seed.Payload), "FRESH-CAPTURE") {
		t.Fatalf("seed = %v %q", seed.Type, seed.Payload)
	}
}
