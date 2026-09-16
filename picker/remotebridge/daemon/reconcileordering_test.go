package daemon

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// scriptedRTRouter is scriptedRT (sessionpin_test.go), except router is the
// caller's own rather than a fresh one built inside. scriptedRT can't be reused
// as-is here: a script that injects live %output needs it to route into the
// same router the code under test registered its sinks on, not into a router
// the test never observes.
func scriptedRTRouter(script string, router *Router) roundTrip {
	return newRoundTrip(newTestReader(script), router, &asyncQueue{}, testStream())
}

// scriptedRTRouterW is scriptedRTRouter, except the control stream's writes go
// to w rather than io.Discard — for a test that needs to see the commands the
// stream itself wrote, not just its scripted replies.
func scriptedRTRouterW(script string, router *Router, w io.Writer) roundTrip {
	return newRoundTrip(newTestReader(script), router, &asyncQueue{}, newStream(w))
}

// orderedLog collects LocalTmux calls and control-stream writes in the order
// reconcileLayout actually issues them. Both seams run on reconcileLayout's own
// goroutine — Config.LocalTmux is called directly, and stream.stampAll flushes
// its bufio.Writer before returning — so a plain unlocked append preserves call
// order with no need for a mutex.
type orderedLog struct {
	entries []string
}

func (l *orderedLog) append(s string) { l.entries = append(l.entries, s) }

// Write lets orderedLog stand in for the control stream's underlying writer:
// newStream wraps it in a bufio.Writer, and each stampAll call's Flush turns
// its whole batch of commands into one Write here.
func (l *orderedLog) Write(p []byte) (int, error) {
	l.append(string(p))
	return len(p), nil
}

// indexContainingAll returns the index of the first entry containing every one
// of substrs, or -1 if none does.
func (l *orderedLog) indexContainingAll(substrs ...string) int {
	for i, e := range l.entries {
		all := true
		for _, s := range substrs {
			if !strings.Contains(e, s) {
				all = false
				break
			}
		}
		if all {
			return i
		}
	}
	return -1
}

// TestReconcileZoomsBeforeReseeding is #511: reconcileLayout used to fire the
// local zoom toggle last, after the reseed loop had already issued its
// capture-pane commands — so a capture taken before the toggle could paint a
// seed sized for the wrong (pre-toggle) geometry into a pane whose dims the
// toggle was about to change, the #233/#417 defect class one level up. The
// toggle now runs immediately after applyLayout, before FrameResize is pushed
// and before PaneSeeds issues any capture-pane, and this pins that against the
// commands reconcileLayout actually sent.
//
// Both directions are covered: the zoom case (remote zoomed, local not) and
// the unzoom case (remote unzoomed, local zoomed) each fall through the #431
// dedup on a mismatched zoom flag and fire the toggle — see
// TestReconcileLayoutFallsThroughOnZoomChange for the same fall-through on the
// single-direction case.
func TestReconcileZoomsBeforeReseeding(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"

	cases := []struct {
		name        string
		remoteZoom  string // #{window_zoomed_flag} readLayout reports for the remote
		appliedZoom bool   // mirrorWindow.appliedZoom before reconcile
	}{
		{name: "zoom", remoteZoom: "1", appliedZoom: false},
		{name: "unzoom", remoteZoom: "0", appliedZoom: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local, peer := net.Pipe()
			defer local.Close()
			defer peer.Close()
			// Nothing here is asserted from the wire; drain it so the sink's
			// pump goroutine never blocks writing to a pipe nobody reads.
			go io.Copy(io.Discard, peer)

			router := NewRouter()
			router.Register("%3", newOutputSink(local, nil))

			w := &mirrorWindow{
				remoteID: "@1", localWin: "@101",
				remotePanes: []string{"%3"}, localPanes: []string{"%l3"},
				layout: layout, appliedZoom: tc.appliedZoom,
			}

			// A zoom-only pass: same shape as
			// TestReconcileLayoutFallsThroughOnZoomChange (reconcilededup_test.go)
			// but with one registered sink, so it also runs the PaneSeeds
			// round-trip: readLayout, PaneSeed(%3) cursor, PaneSeed(%3) capture,
			// trailing readLayout.
			script := strings.Join([]string{
				"%begin 1 1 1", layout + " %3 " + tc.remoteZoom, "%end 1 1 1", // readLayout
				"%begin 1 2 1", "0 0 0 0", "%end 1 2 1", // PaneSeed(%3): cursor
				"%begin 1 3 1", "SEED", "%end 1 3 1", // PaneSeed(%3): capture
				"%begin 1 4 1", layout + " %3 " + tc.remoteZoom, "%end 1 4 1", // trailing re-read: unchanged, stop
			}, "\n") + "\n"

			log := &orderedLog{}
			rt := scriptedRTRouterW(script, router, log)

			cfg := Config{
				LocalTmux: func(args ...string) error {
					log.append(strings.Join(args, " "))
					return nil
				},
			}

			// Synchronous, not backgrounded: the only thing under test is the
			// order of two seams' writes, both of which happen on this call's
			// own goroutine. This cannot deadlock — enqueue is a non-blocking
			// select over a 4096-deep channel, and the scripted reader EOFs
			// rather than hanging once the script runs out.
			reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

			zoomIdx := log.indexContainingAll("if", "-F")
			if zoomIdx == -1 {
				zoomIdx = log.indexContainingAll("resize-pane", "-Z")
			}
			captureIdx := log.indexContainingAll("capture-pane")

			// Fail explicitly rather than let a missing entry sit at -1 and
			// pass a `zoomIdx < captureIdx` comparison vacuously.
			if zoomIdx == -1 {
				t.Fatalf("no if -F zoom assert entry in log: %v", log.entries)
			}
			if captureIdx == -1 {
				t.Fatalf("no capture-pane entry in log: %v", log.entries)
			}

			// This over-constrains on purpose: the invariant that actually
			// matters is zoom-before-FrameSeed, and zoom-before-capture-pane
			// implies it (a seed can't be enqueued before the capture-pane
			// reply it carries). The stricter form is what's assertable here
			// without racing the sink's pump goroutine for FrameSeed's arrival.
			if zoomIdx >= captureIdx {
				t.Fatalf("zoom toggle at log[%d], capture-pane at log[%d]; want the toggle strictly before the capture that feeds the reseed\nlog: %v", zoomIdx, captureIdx, log.entries)
			}
		})
	}
}

// TestReconcileSeedsPaneBeforeItsLaterOutputArrives pins the load-bearing
// invariant the whole batched-roundTrip design rests on (the design doc's
// "ordering hazard" section): a pane's FrameSeed must reach its sink before any
// %output that arrived on the wire after that pane's capture-pane reply.
//
// PaneSeeds calls onSeed(i, ...) as soon as pane i's two replies are parsed,
// strictly before reading pane i+1's replies off the stream. A batch that
// instead read every pane's replies first and wired sinks only afterward would
// let pane B's read route pane A's post-capture %output into A's sink ahead of
// A's own seed — repainting A with a screen that predates output the renderer
// already painted (the #233/#412/#417 defect class). Two panes make the
// distinction observable: with only one pane there is no "later pane's read"
// to smuggle the live output in behind.
func TestReconcileSeedsPaneBeforeItsLaterOutputArrives(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()
	// Pane B's frames are never asserted on; drain them so its sink's pump
	// goroutine never blocks writing to a pipe nobody reads.
	go io.Copy(io.Discard, peerB)

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	// A two-pane layout, unchanged across the reconcile pass: the point is the
	// re-seed loop's ordering, not any pane add/remove/swap. w.layout stays empty
	// so the dedup early-out does not skip the pass.
	const layout = "4ed4,190x45,0,0{95x45,0,0,0,94x45,96,0,1}"
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
	}

	script := strings.Join([]string{
		"%begin 1 1 1", layout + " %0 0", "%end 1 1 1", // readLayout
		"%begin 1 2 1", "0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-A", "%end 1 3 1", // PaneSeed(%0): capture
		"%output %0 LIVE-AFTER-A-SEED",          // must land after FrameSeed(%0), never before
		"%begin 1 4 1", "0 0 0 0", "%end 1 4 1", // PaneSeed(%1): cursor
		"%begin 1 5 1", "SEED-B", "%end 1 5 1", // PaneSeed(%1): capture
		"%begin 1 6 1", layout + " %0 0", "%end 1 6 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	cfg := Config{
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "", nil },
	}
	go reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	peerA.SetDeadline(time.Now().Add(5 * time.Second))

	// The resize lands first (pushed right after select-layout, ahead of any
	// re-seed) — same shape as TestStructuralReconcileReseedsTheSurvivor.
	f, err := wire.ReadFrame(peerA)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if f.Type != wire.FrameResize {
		t.Fatalf("first frame = %v, want a resize", f.Type)
	}

	f, err = wire.ReadFrame(peerA)
	if err != nil {
		t.Fatalf("read second frame: %v", err)
	}
	if f.Type != wire.FrameSeed || !strings.Contains(string(f.Payload), "SEED-A") {
		t.Fatalf("second frame = %v %q, want the FrameSeed carrying SEED-A", f.Type, f.Payload)
	}

	f, err = wire.ReadFrame(peerA)
	if err != nil {
		t.Fatalf("read third frame: %v", err)
	}
	if f.Type != wire.FrameOutput || !strings.Contains(string(f.Payload), "LIVE-AFTER-A-SEED") {
		t.Fatalf("third frame = %v %q, want the live output routed after the seed", f.Type, f.Payload)
	}
}
