package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// signalSink is a router sink that says when it has been written to, so a test
// can order an event *after* the routing rather than after a sleep.
type signalSink struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	got  chan struct{}
}

func newSignalSink() *signalSink { return &signalSink{got: make(chan struct{})} }

func (s *signalSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf.Write(p)
	s.mu.Unlock()
	s.once.Do(func() { close(s.got) })
	return len(p), nil
}

func (s *signalSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestApplyPaneOpsRoutesSiblingOutputDuringHelloWait is #434's acceptance
// criterion, driven through the reconcile path rather than through waitHellos
// alone: applyPaneOps runs on the goroutine that owns the control stream, so
// the hello wait it stops on has to keep draining or the remote's output backs
// up behind it for the whole renderer handshake (and tmux %pauses every busy
// pane behind that).
//
// The ordering is the proof and it is enforced, not hoped for: the hello is
// handed over only once the sibling pane's sink has actually recorded the
// bytes. On a non-draining waiter the sink never fires, so the hello is never
// delivered and applyPaneOps dies on the wait's deadline.
func TestApplyPaneOpsRoutesSiblingOutputDuringHelloWait(t *testing.T) {
	// The split lands the local pane %9local; applyPaneOps reads the window back
	// to learn its id, since a split reports nothing through LocalTmux. The
	// listing grows only once the split has run — applyPaneOps also re-reads on
	// entry, and a window that already held the new pane would read as desynced.
	var split bool
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			if argv[0] == "split-window" {
				split = true
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) {
			if !split {
				return "%1 0\n", nil
			}
			return "%1 0\n%9local 0\n", nil
		},
	}

	// A pipe nothing else writes: the only line the pump ever carries is the
	// sibling's %output below, so the sink firing cannot be some other traffic.
	pr, pw := io.Pipe()
	defer pw.Close()
	pump := startCtlPump(controlmode.NewReader(pr))

	// One Router for both the waiter and applyPaneOps — two would leave the
	// sibling assertion unfirable for a reason that has nothing to do with
	// draining.
	router := NewRouter()
	sink := newSignalSink()
	router.Register("%1", sink)

	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	connCh := make(chan helloConn)
	handoff := make(chan error, 1)
	go func() {
		if _, err := pw.Write([]byte("%output %1 live\n")); err != nil {
			handoff <- fmt.Errorf("write %%output: %w", err)
			return
		}
		select {
		case <-sink.got:
		case <-time.After(5 * time.Second):
			handoff <- errors.New("sibling sink never saw the routed output")
			return
		}
		select {
		case connCh <- helloConn{paneID: "%9", conn: srv}:
			handoff <- nil
		case <-time.After(5 * time.Second):
			handoff <- errors.New("nobody collected the hello")
		}
	}()

	// The real waiter, wired exactly as Run wires it.
	waiter := func(want []string) (map[string]net.Conn, error) {
		return waitHellos(pump.lines, router, &asyncQueue{}, testStream(), connCh, want, 2*time.Second, nil)
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
		[]string{"%1"}, []string{"%1", "%9"}, func(string) {}, router, waiter, rt); err != nil {
		t.Fatalf("applyPaneOps: %v (the wait did not drain the control stream)", err)
	}

	select {
	case err := <-handoff:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hello-delivering goroutine never finished")
	}
	if got := sink.String(); got != "live" {
		t.Errorf("sibling pane %%1 recorded %q, want %q", got, "live")
	}
}

// TestWaitHellosKeepsReplyOrdinalsInIssueOrder: the ordinal count is what lets
// a round-trip find its own reply among the fire-and-forget blocks around it,
// so a client-flagged %end the wait consumes must advance stream.seen by
// exactly one. Miss it and every later round-trip is a command behind; claim it
// twice and every later one is a command ahead.
//
// Ordering the %end ahead of the %output makes the sink the barrier: the channel
// is FIFO and waitHellos consumes it serially, so a fired sink proves the %end
// was already claimed by the time the hello is delivered.
func TestWaitHellosKeepsReplyOrdinalsInIssueOrder(t *testing.T) {
	st := testStream()
	seqs, _ := st.stampAll("list-panes -t @1", "capture-pane -p -t %1")
	seq1, seq2 := seqs[0], seqs[1]

	lines := make(chan controlmode.Line, 2)
	lines <- controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte("reply to " + fmt.Sprint(seq1))}
	lines <- controlmode.Line{Kind: controlmode.Output, Pane: "%1", Data: []byte("live")}

	router := NewRouter()
	sink := newSignalSink()
	router.Register("%1", sink)

	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	connCh := make(chan helloConn)
	go func() {
		select {
		case <-sink.got:
		case <-time.After(5 * time.Second):
			return
		}
		select {
		case connCh <- helloConn{paneID: "%9", conn: srv}:
		case <-time.After(5 * time.Second):
		}
	}()

	added, err := waitHellos(lines, router, &asyncQueue{}, st, connCh, []string{"%9"}, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("waitHellos: %v", err)
	}
	if added["%9"] == nil {
		t.Fatalf("waitHellos returned %v, want the %%9 renderer", added)
	}
	// The next block off the stream must answer command 2 — the one whose reply
	// has not been consumed yet.
	if got := st.claim(nil); got != seq2 {
		t.Errorf("next claim = %d, want %d: the wait must advance seen by exactly one per client-flagged block", got, seq2)
	}
}

// TestWaitHellosUnexpectedHelloIsNotCounted: a reconnect hello for a pane we
// are not waiting on must not fill the want set. Counting it would return with
// the wrong pane wired and the spawned one never collected.
func TestWaitHellosUnexpectedHelloIsNotCounted(t *testing.T) {
	lines := make(chan controlmode.Line)
	connCh := make(chan helloConn, 2)

	one, onePeer := net.Pipe()
	defer one.Close()
	defer onePeer.Close()
	two, twoPeer := net.Pipe()
	defer two.Close()
	defer twoPeer.Close()

	connCh <- helloConn{paneID: "%1", conn: one}
	connCh <- helloConn{paneID: "%2", conn: two}

	var unexpectedIDs []string
	out, err := waitHellos(lines, NewRouter(), &asyncQueue{}, testStream(), connCh, []string{"%2"}, 2*time.Second, func(hc helloConn) {
		unexpectedIDs = append(unexpectedIDs, hc.paneID)
	})
	if err != nil {
		t.Fatalf("waitHellos: %v", err)
	}
	if len(unexpectedIDs) != 1 || unexpectedIDs[0] != "%1" {
		t.Errorf("unexpected = %v, want [%%1]", unexpectedIDs)
	}
	if len(out) != 1 || out["%2"] != two {
		t.Errorf("out = %v, want only %%2", out)
	}
}

// TestWaitHellosDuplicateDoesNotCompleteWait: two hellos for the same wanted
// pane replace rather than count as two slots, so waiting for %2 and %3 does
// not return on a pair of %2s.
func TestWaitHellosDuplicateDoesNotCompleteWait(t *testing.T) {
	lines := make(chan controlmode.Line)
	connCh := make(chan helloConn)

	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	second, secondPeer := net.Pipe()
	defer second.Close()
	defer secondPeer.Close()
	third, thirdPeer := net.Pipe()
	defer third.Close()
	defer thirdPeer.Close()

	firstClosed := make(chan struct{})
	go func() {
		io.Copy(io.Discard, firstPeer)
		close(firstClosed)
	}()

	done := make(chan struct{})
	var out map[string]net.Conn
	var err error
	go func() {
		defer close(done)
		out, err = waitHellos(lines, NewRouter(), &asyncQueue{}, testStream(), connCh, []string{"%2", "%3"}, 2*time.Second, nil)
	}()

	connCh <- helloConn{paneID: "%2", conn: first}
	connCh <- helloConn{paneID: "%2", conn: second}

	select {
	case <-firstClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("overwritten %%2 conn was not closed")
	}
	select {
	case <-done:
		t.Fatal("waitHellos returned before %%3 arrived")
	default:
	}

	connCh <- helloConn{paneID: "%3", conn: third}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitHellos did not return after %%3")
	}
	if err != nil {
		t.Fatalf("waitHellos: %v", err)
	}
	if out["%2"] != second || out["%3"] != third {
		t.Errorf("out = %v, want %%2=replacement and %%3", out)
	}
}
