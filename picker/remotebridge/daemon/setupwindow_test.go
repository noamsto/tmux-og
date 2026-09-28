package daemon

import (
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// setupWindowRT drives a roundTrip against a canned reply stream, discarding
// the commands. Blocks in script must be numbered from seq 1.
func setupWindowRT(script string) roundTrip {
	return newRoundTrip(newTestReader(script), NewRouter(), &asyncQueue{}, testStream())
}

// recordingRT wraps setupWindowRT to capture what was issued, for the
// assertions that care about a command reaching the round-trip rather than
// about its reply.
func recordingRT(script string, issued *[]string) roundTrip {
	rt := setupWindowRT(script)
	return func(cmds ...string) replies {
		*issued = append(*issued, cmds...)
		return rt(cmds...)
	}
}

// readSeedThenResize takes a wired pane's first two frames — wireRenderer
// enqueues the seed, then the resize — and returns the resize's cell dims.
func readSeedThenResize(t *testing.T, peer net.Conn, pane string) (w, h int) {
	t.Helper()
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("%s: read seed frame: %v", pane, err)
	}
	if f.Type != wire.FrameSeed {
		t.Fatalf("%s: first frame = %v, want a seed", pane, f.Type)
	}
	f, err = wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("%s: read resize frame: %v", pane, err)
	}
	if f.Type != wire.FrameResize {
		t.Fatalf("%s: second frame = %v, want a resize", pane, f.Type)
	}
	w, h, err = wire.DecodeResize(f.Payload)
	if err != nil {
		t.Fatalf("%s: decode resize: %v", pane, err)
	}
	return w, h
}

// TestSetupWindowResizesEachPaneFromItsOwnLayoutCell pins the index mapping the
// batched seed rests on: setupWindow filters unconnected panes out of the batch,
// so a batch entry's position is NOT its position in the layout, and `idxs`
// carries it back. Every wired pane must get the dims of ITS OWN L.Panes cell.
//
// Lose that — read L.Panes by batch position instead — and a pane after a hole
// takes its neighbour's cell: the renderer records dims the remote pane does not
// have and paints at the wrong size, silently, with no error anywhere (the
// #233/#417 defect class). A hole only opens when a pane fails to connect, which
// normal use never does, so nothing else would catch it.
func TestSetupWindowResizesEachPaneFromItsOwnLayoutCell(t *testing.T) {
	// Three panes, all three cells distinct, so a neighbour's dims can't pass
	// for the right ones: %0 90x45, %1 99x20, %2 99x24.
	const layout = "a1b2,190x45,0,0{90x45,0,0,0,99x45,91,0[99x20,91,0,1,99x24,91,21,2]}"

	conn0, peer0 := net.Pipe()
	defer conn0.Close()
	defer peer0.Close()
	conn2, peer2 := net.Pipe()
	defer conn2.Close()
	defer peer2.Close()
	stray, strayPeer := net.Pipe()
	defer stray.Close()
	defer strayPeer.Close()

	// collectHellos counts connections, not panes, and keys them by the id each
	// announced — so a renderer left over from a pane this window no longer has
	// satisfies the count while the middle pane %1 stays unwired. However the
	// hole arises, this is the batch it produces: paneIDs [%0 %2], idxs [0 2].
	connCh := make(chan helloConn, 3)
	connCh <- helloConn{paneID: "%0", conn: conn0}
	connCh <- helloConn{paneID: "%2", conn: conn2}
	connCh <- helloConn{paneID: "%9", conn: stray}
	waiter := func(want []string) (map[string]net.Conn, error) {
		out := map[string]net.Conn{}
		for range want {
			hc := <-connCh
			out[hc.paneID] = hc.conn
		}
		return out, nil
	}

	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "@1 " + layout + " %0 0", "%end 1 2 1", // readLayout
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeeds(%0): cursor
		"%begin 1 4 1", "SEED-0", "%end 1 4 1", // PaneSeeds(%0): capture
		"%begin 1 5 1", "0 0 0 0 0 0 0 0 0", "%end 1 5 1", // PaneSeeds(%2): cursor
		"%begin 1 6 1", "SEED-2", "%end 1 6 1", // PaneSeeds(%2): capture
	}, "\n") + "\n"

	cfg := Config{
		LocalArea:    func() (int, int) { return 190, 45 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l0 0\n%l1 0\n%l2 0\n", nil },
	}
	mw := newRegistry().add("@1", "@101")

	errCh := make(chan error, 1)
	go func() {
		errCh <- setupWindow(cfg, func(string) {}, NewRouter(), waiter, newCtlState(), mw, newConverger(), setupWindowRT(script))
	}()

	if w, h := readSeedThenResize(t, peer0, "%0"); w != 90 || h != 45 {
		t.Errorf("resize for %%0 = %dx%d, want 90x45 (its own layout cell)", w, h)
	}
	// The one that matters: %2 is batch entry 1 but layout pane 2. 99x20 here
	// is %1's cell — the mapping collapsed onto the batch index.
	if w, h := readSeedThenResize(t, peer2, "%2"); w != 99 || h != 24 {
		t.Errorf("resize for %%2 = %dx%d, want 99x24 (its own layout cell, not %%1's 99x20)", w, h)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("setupWindow: %v", err)
	}
}

// TestSetupWindowOptsTheWindowOutOfAggressiveResize pins the emission every
// mirrored window depends on: without it the cap issued right after is accepted
// and discarded for every window but the remote's current one (#478).
// setupWindow is the single choke point every mirrored window runs through, so
// asserting it here covers them all.
//
// The script fails readLayout, which is the first thing after the cap: the
// opt-out precedes anything that can go wrong, and nothing else is sent.
func TestSetupWindowOptsTheWindowOutOfAggressiveResizeAndIntoPassthrough(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "%error 1 2 1", // readLayout, window gone
	}, "\n") + "\n"

	cfg := Config{
		LocalArea: func() (int, int) { return 190, 45 },
		LocalTmux: func(...string) error { return nil },
	}
	mw := newRegistry().add("@1", "@101")

	var sent []string
	send := func(s string) { sent = append(sent, s) }

	if err := setupWindow(cfg, send, NewRouter(), noHellos, newCtlState(), mw, newConverger(), setupWindowRT(script)); err == nil {
		t.Fatal("setupWindow err = nil, want the readLayout failure the script scripts")
	}
	want := []string{AggressiveResizeOffCmd("@1"), PassthroughAllCmd("@1")}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("sent = %v, want %v", sent, want)
	}
}

// TestSetupWindowDoesNotRecordACapItCouldNotIssue covers the second place the
// converger could record a size the remote was never told — the call site the
// issue's own suggested fix would have missed. An empty script leaves the
// round-trip no reply to read, so the cap reports a failed write.
func TestSetupWindowDoesNotRecordACapItCouldNotIssue(t *testing.T) {
	cfg := Config{
		LocalArea: func() (int, int) { return 190, 45 },
		LocalTmux: func(...string) error { return nil },
	}
	mw := newRegistry().add("@1", "@101")
	cv := newConverger()

	if err := setupWindow(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), mw, cv, setupWindowRT("")); err == nil {
		t.Fatal("setupWindow err = nil, want a failure on a stream with no replies")
	}
	if !cv.need("@1", 190, 45) {
		t.Error("the cap was recorded although its round-trip never read a reply")
	}
}

// TestSetupWindowCapsAWindowWatchLocalClientAlreadyRecorded closes the ordering race
// between reg.add and setupWindow: reg.add publishes the window first, so
// watchLocalClient's goroutine can cap it before the opt-out lands. tmux discards
// that cap, but cv.need has recorded the size — and without the forget, setup's
// own cap is then skipped and the window is left opted out but never capped.
func TestSetupWindowCapsAWindowWatchLocalClientAlreadyRecorded(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "%error 1 2 1", // readLayout, window gone
	}, "\n") + "\n"

	cfg := Config{
		LocalArea: func() (int, int) { return 190, 45 },
		LocalTmux: func(...string) error { return nil },
	}
	mw := newRegistry().add("@1", "@101")

	// What watchLocalClient did between reg.add and here: a cap the window could not
	// yet accept, recorded as asserted.
	cv := newConverger()
	cv.need("@1", 190, 45)

	var issued []string
	if err := setupWindow(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), mw, cv, recordingRT(script, &issued)); err == nil {
		t.Fatal("setupWindow err = nil, want the readLayout failure the script encodes")
	}
	want := ConvergeCmd("@1", 190, 45)
	if len(issued) == 0 || issued[0] != want {
		t.Errorf("first command issued = %q, want %q", issued, want)
	}
}

// TestResetWindowRecordsItsCapInTheSharedConverger pins which converger
// resetWindow threads through. A throwaway map lets its cap and watchLocalClient's
// disagree: both read cfg.LocalArea() independently and both write to the same
// stream, so a size that changed in the gap can be written stale-last while the
// shared record holds the new one — and watchLocalClient then never re-sends it.
func TestResetWindowRecordsItsCapInTheSharedConverger(t *testing.T) {
	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "%error 1 2 1", // readLayout, window gone
	}, "\n") + "\n"

	cfg := Config{
		LocalArea: func() (int, int) { return 190, 45 },
		LocalTmux: func(...string) error { return nil },
	}
	mw := newRegistry().add("@1", "@101")
	cv := newConverger()

	if err := resetWindow(cfg, mw, func(string) {}, NewRouter(), noHellos, newCtlState(), cv, setupWindowRT(script)); err == nil {
		t.Fatal("resetWindow err = nil, want the readLayout failure the script encodes")
	}
	if cv.need("@1", 190, 45) {
		t.Error("the reset's cap never reached the shared converger, which is then free to disagree with what the remote was told")
	}
}

// TestSetupWindowFailsWhenSolePaneSeedFails pins the other half of the loop
// after the batch: a failed seed leaves the window blank, and for a one-pane
// window there is nothing left to mirror — so it must surface as an error the
// caller can act on, which is what makes addWindow/mirrorNewWindow tear the
// half-created mirror down instead of leaving a blank window behind a live
// registry entry.
func TestSetupWindowFailsWhenSolePaneSeedFails(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()

	connCh := make(chan helloConn, 1)
	connCh <- helloConn{paneID: "%0", conn: conn}
	waiter := func(want []string) (map[string]net.Conn, error) {
		out := map[string]net.Conn{}
		for range want {
			hc := <-connCh
			out[hc.paneID] = hc.conn
		}
		return out, nil
	}

	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "@1 b2c3,80x24,0,0,0 %0 0", "%end 1 2 1", // readLayout
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeeds(%0): cursor
		"%begin 1 4 1", "%error 1 4 1", // PaneSeeds(%0): capture, pane gone
	}, "\n") + "\n"

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l0 0\n", nil },
	}
	mw := newRegistry().add("@1", "@101")

	err := setupWindow(cfg, func(string) {}, NewRouter(), waiter, newCtlState(), mw, newConverger(), setupWindowRT(script))
	if err == nil || !strings.Contains(err.Error(), "sole pane") {
		t.Fatalf("setupWindow err = %v, want a sole-pane seed failure", err)
	}
}

// PlanWindow ends in select-layout, which unzooms; setupWindow must assert the
// remote zoom flag after refreshLocalPanes so appliedZoom matches reality.
func TestSetupWindowAssertsRemoteZoom(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"

	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	go func() { io.Copy(io.Discard, peer) }()

	connCh := make(chan helloConn, 1)
	connCh <- helloConn{paneID: "%3", conn: conn}
	waiter := func(want []string) (map[string]net.Conn, error) {
		out := map[string]net.Conn{}
		for range want {
			hc := <-connCh
			out[hc.paneID] = hc.conn
		}
		return out, nil
	}

	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "@1 " + layout + " %3 1", "%end 1 2 1", // readLayout: zoomed, pane %3 active
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeeds(%3): cursor
		"%begin 1 4 1", "SEED-3", "%end 1 4 1", // PaneSeeds(%3): capture
	}, "\n") + "\n"

	var zoomCmd []string
	cfg := Config{
		LocalArea: func() (int, int) { return 190, 45 },
		LocalTmux: func(args ...string) error {
			if len(args) > 0 && args[0] == "if" {
				zoomCmd = append([]string(nil), args...)
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) { return "%l3 0\n", nil },
	}
	mw := newRegistry().add("@1", "@101")
	mw.appliedZoom = true

	if err := setupWindow(cfg, func(string) {}, NewRouter(), waiter, newCtlState(), mw, newConverger(), setupWindowRT(script)); err != nil {
		t.Fatalf("setupWindow: %v", err)
	}
	if target, ok := parseZoomAssertTarget(zoomCmd); !ok || target != "%l3" {
		t.Errorf("zoom assert = %v, want if -F zoom-on targeting %%l3", zoomCmd)
	}
	if !mw.appliedZoom {
		t.Error("appliedZoom = false after remote zoom assert, want true")
	}
}
