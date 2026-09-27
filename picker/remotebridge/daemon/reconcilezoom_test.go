package daemon

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// parseZoomAssertTarget extracts the resize-pane -Z -t target from an if -F
// zoom assert argv (the -Z lives inside a then/else branch string, not args[0]).
func parseZoomAssertTarget(args []string) (target string, ok bool) {
	if len(args) < 7 || args[0] != "if" || args[1] != "-F" {
		return "", false
	}
	const prefix = "resize-pane -Z -t "
	for _, branch := range []string{args[5], args[6]} {
		if strings.HasPrefix(branch, prefix) {
			return strings.TrimPrefix(branch, prefix), true
		}
	}
	return "", false
}

// TestReconcileGivesZoomedPaneTheWindowDims is #511's other half: a zoomed pane
// is told the layout root rather than the cell #{window_layout} still reports
// for it (see reconcile.go's comment there). Asserted on the FrameResize a
// renderer actually receives, so the fixture's root and cell must disagree.
//
// The hidden pane gets its FrameResize (its cell is unchanged, the frame is a
// local no-op reassert) but NO seed: invisible while zoomed, and the unzoom
// reconcile repaints it before it shows (#557).
func TestReconcileGivesZoomedPaneTheWindowDims(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	// The layout root is 190x45 while pane 0's own cell is 95x45 — a fixture
	// where the two disagree, so a resize frame that carried the pane's own
	// cell instead of the root couldn't pass unnoticed as coincidence.
	const layout = "4ed4,190x45,0,0{95x45,0,0,0,94x45,96,0,1}"
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
		layout: layout,
	}
	script := strings.Join([]string{
		"%begin 1 1 1", layout + " %0 1", "%end 1 1 1", // readLayout: remote zoomed, pane 0 active
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-0", "%end 1 3 1", // PaneSeed(%0): capture
		// No PaneSeed(%1): the zoom hides it (#557).
		"%begin 1 4 1", layout + " %0 1", "%end 1 4 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	var zoomCmd []string
	cfg := Config{
		LocalTmux: func(args ...string) error {
			if len(args) > 0 && args[0] == "if" {
				zoomCmd = append([]string(nil), args...)
			}
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if target, ok := parseZoomAssertTarget(zoomCmd); !ok || target != "%l0" {
		t.Errorf("zoom assert = %v, want if -F zoom-on targeting %%l0", zoomCmd)
	}
	if !w.appliedZoom {
		t.Error("appliedZoom = false after successful zoom assert, want true")
	}

	peerA.SetDeadline(time.Now().Add(5 * time.Second))
	peerB.SetDeadline(time.Now().Add(5 * time.Second))

	fA, err := wire.ReadFrame(peerA)
	if err != nil {
		t.Fatalf("read pane 0's first frame: %v", err)
	}
	if fA.Type != wire.FrameResize {
		t.Fatalf("pane 0's first frame = %v, want a resize", fA.Type)
	}
	wA, hA, err := wire.DecodeResize(fA.Payload)
	if err != nil {
		t.Fatalf("decode pane 0's resize payload: %v", err)
	}
	if wA != 190 || hA != 45 {
		t.Errorf("pane 0 (zoomed, active) dims = %dx%d, want 190x45 (the layout root)", wA, hA)
	}

	fB, err := wire.ReadFrame(peerB)
	if err != nil {
		t.Fatalf("read pane 1's first frame: %v", err)
	}
	if fB.Type != wire.FrameResize {
		t.Fatalf("pane 1's first frame = %v, want a resize", fB.Type)
	}
	wB, hB, err := wire.DecodeResize(fB.Payload)
	if err != nil {
		t.Fatalf("decode pane 1's resize payload: %v", err)
	}
	if wB != 94 || hB != 45 {
		t.Errorf("pane 1 (unzoomed) dims = %dx%d, want 94x45 (its own cell)", wB, hB)
	}

	peerB.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if f, err := wire.ReadFrame(peerB); err == nil {
		t.Errorf("pane 1 (zoom-hidden) got an unexpected second frame: %v", f.Type)
	}
}

// TestReconcileUnzoomReseedsEveryPane is the #557 skip's other half: the
// unzoom reconcile (remote flag 0, appliedZoom true) must reseed the panes the
// zoom hid, or they would show their pre-zoom screens.
func TestReconcileUnzoomReseedsEveryPane(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	const layout = "4ed4,190x45,0,0{95x45,0,0,0,94x45,96,0,1}"
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
		layout: layout, appliedZoom: true, // was zoomed; remote now unzoomed -> fall through dedup
	}

	script := strings.Join([]string{
		"%begin 1 1 1", layout + " %0 0", "%end 1 1 1", // readLayout: remote NOT zoomed, pane 0 active
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-0", "%end 1 3 1", // PaneSeed(%0): capture
		"%begin 1 4 1", "0 0 0 0 0 0 0 0 0", "%end 1 4 1", // PaneSeed(%1): cursor
		"%begin 1 5 1", "SEED-1", "%end 1 5 1", // PaneSeed(%1): capture
		"%begin 1 6 1", layout + " %0 0", "%end 1 6 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	cfg := Config{
		LocalTmux: func(...string) error { return nil },
	}

	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if w.appliedZoom {
		t.Error("appliedZoom still true after unzoom assert, want false")
	}

	peerA.SetDeadline(time.Now().Add(5 * time.Second))
	peerB.SetDeadline(time.Now().Add(5 * time.Second))
	for name, peer := range map[string]net.Conn{"%0": peerA, "%1": peerB} {
		if _, err := wire.ReadFrame(peer); err != nil {
			t.Fatalf("read %s resize: %v", name, err)
		}
		f, err := wire.ReadFrame(peer)
		if err != nil {
			t.Fatalf("%s got no seed on unzoom: %v", name, err)
		}
		if f.Type != wire.FrameSeed {
			t.Fatalf("%s second frame = %v, want a seed", name, f.Type)
		}
	}
}

// TestReconcileKeepsPaneCellDimsOnZoomAssertFailure: when the if -F assert
// errors, localIsZoomed stays false — the daemon never imposed the zoom — so
// the active pane gets its own cell, not the root, and every pane is reseeded.
func TestReconcileKeepsPaneCellDimsOnZoomAssertFailure(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	const layout = "4ed4,190x45,0,0{95x45,0,0,0,94x45,96,0,1}"
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
		layout: layout,
	}

	script := strings.Join([]string{
		"%begin 1 1 1", layout + " %0 1", "%end 1 1 1", // readLayout: remote zoomed, pane 0 active
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-0", "%end 1 3 1", // PaneSeed(%0): capture
		"%begin 1 4 1", "0 0 0 0 0 0 0 0 0", "%end 1 4 1", // PaneSeed(%1): cursor
		"%begin 1 5 1", "SEED-1", "%end 1 5 1", // PaneSeed(%1): capture
		"%begin 1 6 1", layout + " %0 1", "%end 1 6 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	cfg := Config{
		LocalTmux: func(args ...string) error {
			if len(args) > 0 && args[0] == "if" {
				return errors.New("if: command failed")
			}
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if w.appliedZoom {
		t.Error("appliedZoom set despite assert failure, want false")
	}

	peerA.SetDeadline(time.Now().Add(5 * time.Second))
	peerB.SetDeadline(time.Now().Add(5 * time.Second))

	fA, err := wire.ReadFrame(peerA)
	if err != nil {
		t.Fatalf("read pane 0's first frame: %v", err)
	}
	if fA.Type != wire.FrameResize {
		t.Fatalf("pane 0's first frame = %v, want a resize", fA.Type)
	}
	wA, hA, err := wire.DecodeResize(fA.Payload)
	if err != nil {
		t.Fatalf("decode pane 0's resize payload: %v", err)
	}
	if wA != 95 || hA != 45 {
		t.Errorf("pane 0 (zoom assert failed, active) dims = %dx%d, want 95x45 (its own cell, not the root)", wA, hA)
	}

	fB, err := wire.ReadFrame(peerB)
	if err != nil {
		t.Fatalf("read pane 1's first frame: %v", err)
	}
	wB, hB, err := wire.DecodeResize(fB.Payload)
	if err != nil {
		t.Fatalf("decode pane 1's resize payload: %v", err)
	}
	if wB != 94 || hB != 45 {
		t.Errorf("pane 1 dims = %dx%d, want 94x45 (its own cell)", wB, hB)
	}
}

// TestReconcileZoomAssertNeverTargetsAFloat pins the #409/#517 merge bug: a
// float can be remoteActive while the window is zoomed. When remoteActive is a
// float, skip zoom-on entirely rather than guessing a tiled pane.
func TestReconcileZoomAssertNeverTargetsAFloat(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
		localFloats: map[string]string{"%9": "%l9"},
		floatGeom:   map[string]controlmode.PaneCell{"%9": float9},
		layout:      tiledLayout,
	}

	script := strings.Join([]string{
		"%begin 1 1 1", tiledFloatLayout + " %9 1", "%end 1 1 1", // readLayout: zoomed, float active
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-0", "%end 1 3 1", // PaneSeed(%0): capture
		"%begin 1 4 1", "0 0 0 0 0 0 0 0 0", "%end 1 4 1", // PaneSeed(%1): cursor
		"%begin 1 5 1", "SEED-1", "%end 1 5 1", // PaneSeed(%1): capture
		"%begin 1 6 1", tiledFloatLayout + " %9 1", "%end 1 6 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	var zoomTargets []string
	cfg := Config{
		LocalTmux: func(args ...string) error {
			if target, ok := parseZoomAssertTarget(args); ok {
				zoomTargets = append(zoomTargets, target)
			}
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if len(zoomTargets) != 0 {
		t.Errorf("zoom if -F issued targets %v; want none when remoteActive is a float", zoomTargets)
	}
	for _, target := range zoomTargets {
		if target == "%l9" {
			t.Errorf("zoom assert targeted float local id %q", target)
		}
	}

	peerA.SetDeadline(time.Now().Add(5 * time.Second))
	peerB.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := wire.ReadFrame(peerA); err != nil {
		t.Fatalf("read pane 0 frame: %v", err)
	}
	if _, err := wire.ReadFrame(peerB); err != nil {
		t.Fatalf("read pane 1 frame: %v", err)
	}
}

// TestReconcileZoomAssertTargetsTiledPaneBesideFloat is the happy-path twin:
// remoteActive is a tiled pane while a float exists — the assert must still
// land on that tiled local, not skip or hit the float.
func TestReconcileZoomAssertTargetsTiledPaneBesideFloat(t *testing.T) {
	localA, peerA := net.Pipe()
	defer localA.Close()
	defer peerA.Close()
	localB, peerB := net.Pipe()
	defer localB.Close()
	defer peerB.Close()

	router := NewRouter()
	router.Register("%0", newOutputSink(localA, nil))
	router.Register("%1", newOutputSink(localB, nil))

	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0", "%1"}, localPanes: []string{"%l0", "%l1"},
		localFloats: map[string]string{"%9": "%l9"},
		floatGeom:   map[string]controlmode.PaneCell{"%9": float9},
		layout:      tiledLayout,
	}

	script := strings.Join([]string{
		"%begin 1 1 1", tiledFloatLayout + " %0 1", "%end 1 1 1", // readLayout: zoomed, tiled %0 active
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1", // PaneSeed(%0): cursor
		"%begin 1 3 1", "SEED-0", "%end 1 3 1", // PaneSeed(%0): capture
		"%begin 1 4 1", tiledFloatLayout + " %0 1", "%end 1 4 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n"

	rt := scriptedRTRouter(script, router)

	var zoomTargets []string
	cfg := Config{
		LocalTmux: func(args ...string) error {
			if target, ok := parseZoomAssertTarget(args); ok {
				zoomTargets = append(zoomTargets, target)
			}
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if len(zoomTargets) != 1 || zoomTargets[0] != "%l0" {
		t.Errorf("zoom assert targets = %v, want [%%l0]", zoomTargets)
	}
	for _, target := range zoomTargets {
		if target == "%l9" {
			t.Errorf("zoom assert targeted float local id %q", target)
		}
	}

	peerA.SetDeadline(time.Now().Add(5 * time.Second))
	peerB.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := wire.ReadFrame(peerA); err != nil {
		t.Fatalf("read pane 0 frame: %v", err)
	}
}

// TestReconcileSecondZoomedReconcileDoesNotInvert: after a successful zoom
// assert, a second reconcile with the same layout and remote still zoomed must
// dedup and issue no LocalTmux (parity-safe because if -F is idempotent anyway).
func TestReconcileSecondZoomedReconcileDoesNotInvert(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      layout,
		appliedZoom: true,
	}

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", layout + " %3 1", "%end 1 1 1", // readLayout
	}, "\n") + "\n")

	cfg := Config{
		LocalTmux: func(...string) error {
			t.Fatal("unexpected LocalTmux on second still-zoomed dedup")
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)
}
