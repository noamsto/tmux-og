package daemon

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestStructuralReconcileReseedsTheSurvivor is #417: closing one pane of a
// split resizes the pane that stays, and the renderer holds no back-buffer to
// reflow — so the survivor has to be repainted from the remote. The re-seed
// used to be gated on the change being geometry-only, which a close is not, and
// the survivor kept the screen it was painted with at the old size.
func TestStructuralReconcileReseedsTheSurvivor(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	router := NewRouter()
	router.Register("%3", newOutputSink(local, nil))

	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}

	// A one-pane layout: %4 is gone, %3 survives and fills the window.
	const onePane = "bd67,190x45,0,0,3"
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", onePane + " %3 0", "%end 1 1 1", // readLayout
		"%begin 1 2 1", "0 0 0 0", "%end 1 2 1", // PaneSeed: cursor
		"%begin 1 3 1", "SURVIVOR-REPAINT", "%end 1 3 1", // PaneSeed: capture
		"%begin 1 4 1", onePane + " %3 0", "%end 1 4 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n")

	// %l4 is killed with its remote pane, leaving the survivor alone. The
	// listing shrinks only once that kill has run: applyPaneOps re-reads on
	// entry, where the window still holds both panes.
	var killed bool
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			if argv[0] == "kill-pane" {
				killed = true
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) {
			if !killed {
				return "%l3 0\n%l4 0\n", nil
			}
			return "%l3 0\n", nil
		},
	}
	go reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	// The resize lands first (dims are pushed right after select-layout), then
	// the repaint. Both matter, and the order is the point: a seed sized for the
	// new geometry must not arrive before the pane has it (#233).
	deadline := time.Now().Add(5 * time.Second)
	peer.SetDeadline(deadline)

	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if f.Type != wire.FrameResize {
		t.Fatalf("first frame = %v, want a resize", f.Type)
	}
	f, err = wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read second frame: %v", err)
	}
	if f.Type != wire.FrameSeed || !strings.Contains(string(f.Payload), "SURVIVOR-REPAINT") {
		t.Fatalf("second frame = %v %q, want a seed carrying the remote's screen", f.Type, f.Payload)
	}
}

// TestStructuralReconcileReplaysRetainedKittyStoreBeforeSeed pins #465/#731
// on the layout-reshape re-seed path.
func TestStructuralReconcileReplaysRetainedKittyStoreBeforeSeed(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	sink := newOutputSink(local, p)
	sink.Write(testKittyStore("3"))
	storeFrame, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if storeFrame.Type != wire.FrameOutput || !strings.Contains(string(storeFrame.Payload), kittyLocalisedMarker) {
		t.Fatalf("store frame = %v %q", storeFrame.Type, storeFrame.Payload)
	}

	router := NewRouter()
	router.Register("%3", sink)

	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}

	const onePane = "bd67,190x45,0,0,3"
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", onePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "0 0 0 0", "%end 1 2 1",
		"%begin 1 3 1", "SURVIVOR-REPAINT", "%end 1 3 1",
		"%begin 1 4 1", onePane + " %3 0", "%end 1 4 1",
	}, "\n") + "\n")

	var killed bool
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			if argv[0] == "kill-pane" {
				killed = true
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) {
			if !killed {
				return "%l3 0\n%l4 0\n", nil
			}
			return "%l3 0\n", nil
		},
	}
	go reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	peer.SetDeadline(time.Now().Add(5 * time.Second))

	resize, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read resize: %v", err)
	}
	if resize.Type != wire.FrameResize {
		t.Fatalf("first frame = %v, want FrameResize", resize.Type)
	}
	replay, seed := replayThenSeedFrames(t, peer)
	if replay.Type != wire.FrameOutput || !strings.Contains(string(replay.Payload), kittyLocalisedMarker) {
		t.Fatalf("replay = %v %q, want localised store before seed", replay.Type, replay.Payload)
	}
	if seed.Type != wire.FrameSeed || !strings.Contains(string(seed.Payload), "SURVIVOR-REPAINT") {
		t.Fatalf("seed = %v %q", seed.Type, seed.Payload)
	}
}
