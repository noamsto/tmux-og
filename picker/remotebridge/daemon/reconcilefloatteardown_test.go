package daemon

import (
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
)

// A rebuild starts from scratch, mirrored floats included — setupWindow's
// reconcileFloats re-creates them, which is also what repairs a float whose
// renderer died. The user's own float carries no localFloats entry and stays,
// as does pane 0, which resetWindow keeps for the respawn.
func TestDropMirroredPanesReapsMirroredFloats(t *testing.T) {
	var got [][]string
	cfg := recordingCfg("%l0 0\n%l1 0\n%l9 1\n%lforeign 1\n", &got)
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1"}
	w.localPanes = []string{"%l0", "%l1"}
	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9

	dropMirroredPanes(cfg, w)

	var killed []string
	for _, c := range tmuxCalls(got, "kill-pane") {
		killed = append(killed, c[len(c)-1])
	}
	if want := []string{"%l1", "%l9"}; !reflect.DeepEqual(killed, want) {
		t.Errorf("killed %v, want %v — never pane 0, never the foreign %%lforeign", killed, want)
	}
	if len(w.localFloats) != 0 || len(w.floatGeom) != 0 {
		t.Errorf("localFloats=%v floatGeom=%v, want both cleared so reconcileFloats rebuilds them from the remote",
			w.localFloats, w.floatGeom)
	}
}

// resetWindow's failure path merges a surviving pane's conn back so its sink
// keeps rendering. A float's must never be merged: dropMirroredPanes killed the
// pane it drove, so the merge would put a live sink on a pane that is gone.
func TestResetWindowNeverMergesBackAFloatConn(t *testing.T) {
	tiledRaw, tiledPeer := net.Pipe()
	defer tiledPeer.Close()
	go io.Copy(io.Discard, tiledPeer)
	tiledConn := &trackCloseConn{Conn: tiledRaw}

	floatRaw, floatPeer := net.Pipe()
	defer floatPeer.Close()
	go io.Copy(io.Discard, floatPeer)
	floatConn := &trackCloseConn{Conn: floatRaw}

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l1 0\n", nil },
	}
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%1"}
	w.localPanes = []string{"%l1"}
	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9
	w.conns["%1"] = tiledConn
	w.conns["%9"] = floatConn

	router := NewRouter()
	err := resetWindow(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), setupWindowRT(strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "%error 1 2 1", // readLayout fails, before any respawn
	}, "\n")+"\n"))
	if err == nil {
		t.Fatal("resetWindow err = nil, want the setupWindow failure the script encodes")
	}
	if _, ok := w.conns["%9"]; ok {
		t.Error("the float's conn was merged back onto a pane the drop had killed")
	}
	if router.sink("%9") != nil {
		t.Error("router.sink(%9) revived a sink on a dead pane")
	}
	if !floatConn.closed {
		t.Error("the float's conn was neither merged nor closed; it leaks for the daemon's life")
	}
	// The tiled merge-back is a separate fix and has to keep working.
	if w.conns["%1"] != tiledConn || router.sink("%1") == nil {
		t.Error("the kept tiled pane lost its merged-back conn or sink")
	}
	if tiledConn.closed {
		t.Error("the kept tiled pane's conn was closed after a failed reset")
	}
}

// A float's output sink is registered like any other pane's, so a teardown
// walking only remotePanes leaves its pump running for the daemon's whole life.
func TestCloseWindowUnregistersFloatSinks(t *testing.T) {
	tiled, tiledPeer := net.Pipe()
	defer tiledPeer.Close()
	flt, floatPeer := net.Pipe()
	defer floatPeer.Close()

	reg := newRegistry()
	w := reg.add("@1", "@101")
	w.remotePanes = []string{"%1"}
	w.localFloats["%9"] = "%l9"
	w.conns["%1"] = tiled
	w.conns["%9"] = flt

	router := NewRouter()
	router.Register("%1", newOutputSink(tiled, nil))
	router.Register("%9", newOutputSink(flt, nil))

	cfg := Config{LocalTmux: func(...string) error { return nil }}
	closeWindow(cfg, router, newCtlState(), reg, newConverger(), "@1")

	if router.sink("%9") != nil {
		t.Error("the float's sink outlived the window it belonged to")
	}
	if router.sink("%1") != nil {
		t.Error("the tiled pane's sink outlived the window it belonged to")
	}
}

// The reseed after a %session-changed excursion or a reconnect repaints from
// the remote's own screens. A mirrored float's renderer holds no back-buffer
// either, so leaving it out strands it on a screen the remote has moved past.
func TestReseedPanesRepaintsMirroredFloats(t *testing.T) {
	tiled, tiledPeer := net.Pipe()
	defer tiledPeer.Close()
	go io.Copy(io.Discard, tiledPeer)
	flt, floatPeer := net.Pipe()
	defer floatPeer.Close()
	go io.Copy(io.Discard, floatPeer)

	reg := newRegistry()
	w := reg.add("@1", "@101")
	w.remotePanes = []string{"%1"}
	w.localFloats["%9"] = "%l9"

	router := NewRouter()
	router.Register("%1", newOutputSink(tiled, nil))
	router.Register("%9", newOutputSink(flt, nil))

	var issued []string
	rt := recordingRT(strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0", "%end 1 1 1", // %1 cursor
		"%begin 1 2 1", "TILED", "%end 1 2 1", // %1 capture
		"%begin 1 3 1", "0 0 0 0", "%end 1 3 1", // %9 cursor
		"%begin 1 4 1", "FLOAT", "%end 1 4 1", // %9 capture
	}, "\n")+"\n", &issued)

	reseedPanes(reg, router, rt, "", "after session change")

	var captured []string
	for _, cmd := range issued {
		if strings.HasPrefix(cmd, "capture-pane") {
			captured = append(captured, cmd)
		}
	}
	if len(captured) != 2 {
		t.Fatalf("capture-pane commands = %v, want one per mirrored pane including the float", captured)
	}
	if !strings.Contains(captured[1], "%9") {
		t.Errorf("second capture-pane = %q, want it to name the float %%9", captured[1])
	}
}

// The drop's signal is what a failing reconcile exit reads to know it owes the
// floats a re-add, so it must be raised on a real discard only: raised on a
// floatless window, a failed rebuild would run a float reconcile for floats
// that were never lost.
func TestDropMirroredPanesRaisesFloatsDroppedOnlyForAFloat(t *testing.T) {
	var got [][]string
	cfg := recordingCfg("%l0 0\n%l1 0\n", &got)
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1"}
	w.localPanes = []string{"%l0", "%l1"}

	dropMirroredPanes(cfg, w)
	if w.floatsDropped {
		t.Error("floatsDropped = true for a window that held no float")
	}

	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9
	dropMirroredPanes(cfg, w)
	if !w.floatsDropped {
		t.Error("floatsDropped = false after the drop killed %l9; the failing exits then never re-add")
	}
}
