package daemon

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestSinkReshapedIsDueOnlyOnALaterPass is the whole point of the mark: the
// pass that reshapes a pane captures the remote before its app has repainted
// for the new size, so the confirmation capture has to wait for a later wake —
// which the app's own repaint output provides.
func TestSinkReshapedIsDueOnlyOnALaterPass(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.markReshaped()

	if s.takeReshaped() {
		t.Fatal("takeReshaped = true on the pass that marked it; the capture would be as early as the one it repairs")
	}
	if !s.takeReshaped() {
		t.Fatal("takeReshaped = false on the next pass; want the confirmation re-seed to come due")
	}
	if s.takeReshaped() {
		t.Error("takeReshaped must clear the mark once it has come due")
	}
}

// TestSinkReshapedWaitsForAPausedPane keeps the two recovery paths from firing
// at once, exactly as takeDirty does: a paused pane is already owed a seed by
// its %continue. The mark survives, so the pane is still repainted later.
func TestSinkReshapedWaitsForAPausedPane(t *testing.T) {
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.markReshaped()
	s.takeReshaped() // due on the next pass
	s.pause()

	if s.takeReshaped() {
		t.Fatal("a paused sink must not ask for a re-seed")
	}
	s.resume()
	if !s.takeReshaped() {
		t.Error("the mark must survive the pause, not be spent by it")
	}
}

// TestSinkReshapedWaitsForACongestedPane mirrors takeDirty's other gate: a
// re-seed is a whole extra screen, and a pane that has not drained is already
// behind.
func TestSinkReshapedWaitsForACongestedPane(t *testing.T) {
	// No pump: the channel is the only consumer, so it fills deterministically.
	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.markReshaped()
	s.takeReshaped() // due on the next pass
	s.enqueue(wire.FrameOutput, []byte("queued"))

	if s.takeReshaped() {
		t.Fatal("takeReshaped = true while frames are still queued")
	}
	<-s.ch
	if !s.takeReshaped() {
		t.Error("takeReshaped = false once drained; want the deferred re-seed")
	}
}

// TestReshapedPanesSkipsNonSinks guards the router's mixed map: tests register
// plain io.Writer fakes, which have nothing to re-seed.
func TestReshapedPanesSkipsNonSinks(t *testing.T) {
	r := NewRouter()
	r.Register("%9", &fakeSink{})

	s := &outputSink{ch: make(chan sinkFrame, 1)}
	s.markReshaped()
	r.Register("%1", s)

	if got := r.reshapedPanes(); len(got) != 0 {
		t.Fatalf("reshapedPanes = %v on the marking pass, want none", got)
	}
	if got := r.reshapedPanes(); len(got) != 1 || got[0] != "%1" {
		t.Fatalf("reshapedPanes = %v, want [%%1]", got)
	}
}

// TestReseedReshapedRepaintsFromCapture is the fix end to end: the second
// capture is what the app's repaint made of the pane, and it replaces the
// rewrapped screen the reshape pass had to ship.
func TestReseedReshapedRepaintsFromCapture(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	s := newOutputSink(local, nil)
	s.markReshaped()
	router := NewRouter()
	router.Register("%1", s)

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", "REPAINTED", "%end 1 2 1",
	}, "\n") + "\n")

	reseedReshaped(router, rt) // marking pass: nothing due, no round-trip spent
	go reseedReshaped(router, rt)

	peer.SetDeadline(time.Now().Add(5 * time.Second))
	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Type != wire.FrameSeed || !strings.Contains(string(f.Payload), "REPAINTED") {
		t.Fatalf("frame = %v %q, want a seed carrying REPAINTED", f.Type, f.Payload)
	}
}

// TestReshapeReconcileMarksTheReseededPane wires the mark to the path that
// needs it: every pane the layout re-seed repaints was captured before the
// remote app could redraw at its new size, so every one of them is owed a
// confirmation capture.
func TestReshapeReconcileMarksTheReseededPane(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	router := NewRouter()
	sink := newOutputSink(local, nil)
	router.Register("%3", sink)

	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}
	const onePane = "bd67,190x45,0,0,3"
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", onePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1",
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

	// Drain the reshape's own frames so the sink is idle; the mark is what is
	// under test, not the frames.
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 2; i++ {
		if _, err := wire.ReadFrame(peer); err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
	}

	// Two calls at the earliest: the first promotes the mark, the second hands
	// the pane over. Polled because reconcileLayout runs on its own goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := router.reshapedPanes(); len(got) == 1 && got[0] == "%3" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("reconcile never marked %3 for a confirmation re-seed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
