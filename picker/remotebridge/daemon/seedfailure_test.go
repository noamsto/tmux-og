package daemon

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func twoPaneAppendHarness(t *testing.T, send func(string)) (cfg Config, w *mirrorWindow, router *Router, newConn, newPeer net.Conn, rt roundTrip, localTrace *[]string) {
	t.Helper()
	var mu sync.Mutex
	var split bool
	trace := []string{}
	localTrace = &trace

	cfg = Config{
		SockPath:    "/run/sock",
		RendererBin: "/nix/store/x-renderer/bin/renderer",
		LocalTmux: func(argv ...string) error {
			mu.Lock()
			trace = append(trace, strings.Join(argv, " "))
			mu.Unlock()
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if !split {
				split = true
				return "%l1 0\n", nil
			}
			return "%l1 0\n%l2 0\n", nil
		},
	}

	L := controlmode.Layout{W: 80, H: 40, Panes: []controlmode.PaneCell{
		{ID: "%1", W: 80, H: 20, X: 0, Y: 0},
		{ID: "%2", W: 80, H: 19, X: 0, Y: 21},
	}}
	w = &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%1"}, localPanes: []string{"%l1"},
		conns: map[string]net.Conn{},
	}
	router = NewRouter()

	newConn, newPeer = net.Pipe()
	waiter := func([]string) (map[string]net.Conn, error) {
		return map[string]net.Conn{"%2": newConn}, nil
	}

	rt, _ = scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", "%error 1 2 1",
	}, "\n") + "\n")

	if err := applyPaneOps(cfg, w, paneOps{Append: []string{"%2"}}, L,
		[]string{"%1"}, []string{"%1", "%2"}, send, router, waiter, rt); err != nil {
		t.Fatalf("applyPaneOps: %v", err)
	}
	w.remotePanes = []string{"%1", "%2"}
	return cfg, w, router, newConn, newPeer, rt, localTrace
}

// TestSeedFailureApplyPaneOpsKeepsPaneWired is acceptance 1: a failed capture on
// an incrementally added pane leaves the renderer conn open, the sink registered,
// and local/remote pane counts aligned — no kill-pane.
func TestSeedFailureApplyPaneOpsKeepsPaneWired(t *testing.T) {
	_, w, router, _, newPeer, _, trace := twoPaneAppendHarness(t, func(string) {})
	defer newPeer.Close()

	if _, err := newPeer.Write([]byte("x")); err != nil {
		t.Errorf("appended renderer conn not writable: %v", err)
	}
	if router.sink("%2") == nil {
		t.Error("router.sink(%2) = nil, want a registered sink")
	}
	if w.conns["%2"] == nil {
		t.Error("w.conns[%2] = nil, want the conn kept")
	}
	if len(w.localPanes) != 2 {
		t.Errorf("localPanes = %v, want 2", w.localPanes)
	}
	if len(w.remotePanes) != len(w.localPanes) {
		t.Errorf("remotePanes = %d, localPanes = %d, want equal", len(w.remotePanes), len(w.localPanes))
	}
	for _, cmd := range *trace {
		if strings.HasPrefix(cmd, "kill-pane") {
			t.Errorf("kill-pane in LocalTmux trace: %q", cmd)
		}
	}
}

// TestSeedFailureInputStillFlows is acceptance 2: pumpInput runs on an unseeded
// pane and forwards FrameInput to send-keys for the remote id.
func TestSeedFailureInputStillFlows(t *testing.T) {
	var mu sync.Mutex
	var got []string
	send := func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}
	_, _, _, _, newPeer, _, _ := twoPaneAppendHarness(t, send)
	defer newPeer.Close()

	if err := wire.WriteFrame(newPeer, wire.FrameInput, []byte("x")); err != nil {
		t.Fatalf("write FrameInput: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, s := range got {
			if strings.Contains(s, "send-keys") && strings.Contains(s, "%2") {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("send-keys for %%2 not seen in %v", got)
}

// TestSeedFailurePaneInTrailingReseedSet is acceptance 3: a pane wired unseeded
// is included in reconcileLayout's post-reshape reseed because its sink is
// registered. Driving the reseed block directly — same inputs as reconcileLayout
// uses over newRemote after a reshape.
func TestSeedFailurePaneInTrailingReseedSet(t *testing.T) {
	_, w, router, _, newPeer, _, _ := twoPaneAppendHarness(t, func(string) {})
	defer newPeer.Close()

	w.remotePanes = []string{"%1", "%2"}

	peer := newPeer
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read resize after unseeded wire: %v", err)
	}
	if f.Type != wire.FrameResize {
		t.Fatalf("first frame = %v, want resize from unseeded wire", f.Type)
	}

	reseedRT, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", "RESEED-OK", "%end 1 2 1",
	}, "\n") + "\n")

	reseedIDs := []string{"%2"}
	sinks := []*outputSink{router.sink("%2")}
	PaneSeeds(reseedRT, reseedIDs, func(i int, seed []byte, err error) {
		if err != nil {
			t.Fatalf("reseed PaneSeeds: %v", err)
		}
		sinks[i].enqueue(wire.FrameSeed, seed)
	})

	f, err = wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read reseed frame: %v", err)
	}
	if f.Type != wire.FrameSeed || !strings.Contains(string(f.Payload), "RESEED-OK") {
		t.Fatalf("frame after reseed = %v %q, want FrameSeed carrying remote screen", f.Type, f.Payload)
	}
}

type trackCloseConn struct {
	net.Conn
	closed bool
}

func (c *trackCloseConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

// TestResetWindowKeepsKeptPaneConnOnSetupFailure is acceptance 4a: when
// setupWindow fails before replacing renderers, the kept pane's conn stays open.
func TestResetWindowKeepsKeptPaneConnOnSetupFailure(t *testing.T) {
	raw, keptPeer := net.Pipe()
	defer keptPeer.Close()
	go io.Copy(io.Discard, keptPeer)

	keptConn := &trackCloseConn{Conn: raw}

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l1 0\n", nil },
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%1"}, localPanes: []string{"%l1"},
		conns: map[string]net.Conn{"%1": keptConn},
	}

	router := NewRouter()
	err := resetWindow(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), setupWindowRT(strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "%error 1 2 1", // readLayout fails
	}, "\n")+"\n"))
	if err == nil {
		t.Fatal("resetWindow err = nil, want setupWindow failure")
	}
	if keptConn.closed {
		t.Error("kept pane conn was closed after failed reset")
	}
	if w.conns["%1"] != keptConn {
		t.Errorf("w.conns[%%1] = %v, want the original kept conn merged back", w.conns["%1"])
	}
	if router.sink("%1") == nil {
		t.Error("router.sink(%1) = nil, want sink re-registered after merge-back")
	}
}

// TestResetWindowClosesKeptPaneConnAfterSuccessfulReshape is acceptance 4b: on
// success the old renderer conn is closed only after setupWindow's reshape ran.
func TestResetWindowClosesKeptPaneConnAfterSuccessfulReshape(t *testing.T) {
	oldRaw, oldPeer := net.Pipe()
	defer oldPeer.Close()
	go io.Copy(io.Discard, oldPeer)
	oldConn := &trackCloseConn{Conn: oldRaw}

	newConn, newPeer := net.Pipe()
	defer newConn.Close()
	defer newPeer.Close()
	go io.Copy(io.Discard, newPeer)

	connCh := make(chan helloConn, 1)
	connCh <- helloConn{paneID: "%0", conn: newConn}
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
		"%begin 1 2 1", "b2c3,80x24,0,0,0 %0 0", "%end 1 2 1", // readLayout
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeeds(%0): cursor
		"%begin 1 4 1", "FRESH-SEED", "%end 1 4 1", // PaneSeeds(%0): capture
	}, "\n") + "\n"

	var reshaped bool
	cfg := Config{
		LocalArea: func() (int, int) { return 80, 24 },
		LocalTmux: func(argv ...string) error {
			if argv[0] == "select-layout" || argv[0] == "split-window" {
				reshaped = true
				if oldConn.closed {
					t.Error("old conn already closed during reshape, want it kept until superseded")
				}
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) { return "%l0 0\n", nil },
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0"}, localPanes: []string{"%l0"},
		conns: map[string]net.Conn{"%0": oldConn},
	}

	if err := resetWindow(cfg, w, func(string) {}, NewRouter(), waiter, newCtlState(), newConverger(), setupWindowRT(script)); err != nil {
		t.Fatalf("resetWindow: %v", err)
	}
	if !reshaped {
		t.Fatal("reshape commands never ran")
	}
	if !oldConn.closed {
		t.Error("old conn still open after successful reset, want it closed once superseded")
	}
}

// TestSetupWindowSolePaneSeedFailureCleansUp is acceptance 5: sole-pane seed
// failure still errors, and disposes of the sink and conn wireRenderer registered.
func TestSetupWindowSolePaneSeedFailureCleansUp(t *testing.T) {
	conn, peer := net.Pipe()
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
		"%begin 1 2 1", "b2c3,80x24,0,0,0 %0 0", "%end 1 2 1", // readLayout
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeeds(%0): cursor
		"%begin 1 4 1", "%error 1 4 1", // PaneSeeds(%0): capture
	}, "\n") + "\n"

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l0 0\n", nil },
	}
	router := NewRouter()
	mw := newRegistry().add("@1", "@101")

	err := setupWindow(cfg, func(string) {}, router, waiter, newCtlState(), mw, newConverger(), setupWindowRT(script))
	if err == nil || !strings.Contains(err.Error(), "sole pane") {
		t.Fatalf("setupWindow err = %v, want sole-pane seed failure", err)
	}
	if router.sink("%0") != nil {
		t.Error("sink still registered for sole-pane failure")
	}
	if mw.conns["%0"] != nil {
		t.Error("conn still in mw.conns after sole-pane failure cleanup")
	}
	if _, err := peer.Write([]byte("x")); err == nil {
		t.Error("sole-pane conn still open after cleanup")
	}
}

// TestResetWindowClosesKeptPaneConnOnSpawnedSetupFailure is acceptance 4c:
// when setupWindow fails after respawn-pane -k, the kept pane's old conn is
// closed and not merged back — the old renderer is dead.
func TestResetWindowClosesKeptPaneConnOnSpawnedSetupFailure(t *testing.T) {
	raw, keptPeer := net.Pipe()
	defer keptPeer.Close()
	go io.Copy(io.Discard, keptPeer)

	keptConn := &trackCloseConn{Conn: raw}

	script := strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1", // ConvergeCmd
		"%begin 1 2 1", "b2c3,80x24,0,0,0 %0 0", "%end 1 2 1", // readLayout
	}, "\n") + "\n"

	waiter := func([]string) (map[string]net.Conn, error) {
		return nil, io.EOF
	}

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l0 0\n", nil },
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%0"}, localPanes: []string{"%l0"},
		conns: map[string]net.Conn{"%0": keptConn},
	}

	err := resetWindow(cfg, w, func(string) {}, NewRouter(), waiter, newCtlState(), newConverger(), setupWindowRT(script))
	if err == nil {
		t.Fatal("resetWindow err = nil, want setupWindow failure after spawn")
	}
	if !keptConn.closed {
		t.Error("kept pane conn still open after spawned setup failure, want it closed")
	}
	if w.conns["%0"] == keptConn {
		t.Error("w.conns[%0] still maps to the dead kept conn, want it not merged back")
	}
}
