package daemon

import (
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// Pins the ordering #408 fixed: the appended pane's geometry is applied before
// applyPaneOps waits on the renderer hello. The splits are always -h and the
// hello wait costs an ssh round-trip, so shaping afterwards leaves the wrong
// layout on screen for the whole handshake.
func TestApplyPaneOpsShapesBeforeHelloWait(t *testing.T) {
	var mu sync.Mutex
	var trace []string
	rec := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		trace = append(trace, s)
	}
	seen := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(trace, s)
	}

	// The split lands the local pane %9local; applyPaneOps reads the window back
	// to learn its id, since a split reports nothing through LocalTmux.
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			rec(argv[0])
			return nil
		},
		// Grows only once the split has run: applyPaneOps re-reads on entry too,
		// and a window that already held the new pane would read as desynced.
		LocalTmuxOut: func(...string) (string, error) {
			if !seen("split-window") {
				return "%1 0\n", nil
			}
			return "%1 0\n%9local 0\n", nil
		},
	}

	cli, srv := net.Pipe()
	defer func() { _ = cli.Close() }()
	defer func() { _ = srv.Close() }()

	// Hand the hello over as soon as the shape lands, so a correct run is fast;
	// the deadline makes a regressed run fail rather than hang for helloTimeout.
	connCh := make(chan helloConn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for deadline := time.Now().Add(2 * time.Second); !seen("select-layout") && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		rec("hello")
		connCh <- helloConn{paneID: "%9", conn: srv}
	}()
	// Blocks on connCh exactly as the real waiter does — the ordering this test
	// pins only exists because the wait is what applyPaneOps stops on.
	waiter := func(want []string) (map[string]net.Conn, error) {
		out := map[string]net.Conn{}
		for range want {
			hc := <-connCh
			out[hc.paneID] = hc.conn
		}
		return out, nil
	}

	w := &mirrorWindow{
		remoteID: "@1", localWin: "$0:@1",
		remotePanes: []string{"%1"}, localPanes: []string{"%1"},
		conns: map[string]net.Conn{},
	}
	L := controlmode.Layout{W: 80, H: 24, Raw: "abcd,80x24,0,0", Panes: []controlmode.PaneCell{{W: 80, H: 12}, {W: 80, H: 11}}}
	// A failing round-trip makes the seed error out, so the renderer wiring ends
	// right after the hello instead of pumping a pipe nobody reads.
	rt := func(...string) replies {
		return func() (controlmode.Line, bool) { return controlmode.Line{}, false }
	}

	if err := applyPaneOps(cfg, w, paneOps{Append: []string{"%9"}}, L,
		[]string{"%1"}, []string{"%1", "%9"}, func(string) {}, NewRouter(), waiter, rt); err != nil {
		t.Fatalf("applyPaneOps: %v", err)
	}
	<-done // wait for the sender's rec("hello") to land before asserting on trace

	mu.Lock()
	defer mu.Unlock()
	idx := map[string]int{}
	for i, e := range trace {
		if _, ok := idx[e]; !ok {
			idx[e] = i
		}
	}
	for _, want := range []string{"split-window", "select-layout", "hello"} {
		if _, ok := idx[want]; !ok {
			t.Fatalf("trace %v missing %q", trace, want)
		}
	}
	if idx["split-window"] >= idx["select-layout"] || idx["select-layout"] >= idx["hello"] {
		t.Fatalf("want split-window < select-layout < hello, got %v", trace)
	}
}
