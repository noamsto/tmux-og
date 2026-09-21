package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// capBuf is a tiny io.Writer that captures what it's given, for asserting
// routed output.
type capBuf struct{ bytes.Buffer }

func newTestReader(s string) *controlmode.Reader {
	return controlmode.NewReader(strings.NewReader(withBarriers(s)))
}

// rawTestReader is newTestReader without the barrier injection, for a fixture
// that scripts the BLOCK stream directly rather than one block per command —
// where an injected barrier would shift the very ordinals the test is pinning.
func rawTestReader(s string) *controlmode.Reader {
	return controlmode.NewReader(strings.NewReader(s))
}

// withBarriers is what lets a fixture keep scripting one reply block per
// command. stampAll writes a barrier behind every command (#723), so the real
// wire carries two blocks per command and a fixture listing only its own would
// desynchronise from the second command on — failing closed, since claim's
// swallow window stays open and the round-trip returns not-ok.
//
// So the barrier is injected here rather than written out nineteen times: after
// each client-flagged block, carrying the tag of the command that block
// answered. Only flagged blocks are counted, because those are the only ones
// claim gives an ordinal to — a hook's block (flags 0) is not one of ours and
// takes no barrier.
//
// A fixture that scripts a fan-out — more blocks than commands — cannot use
// this and builds its reader directly; fanout_test.go is the one that does.
func withBarriers(script string) string {
	var out []string
	cmds := 0
	for _, line := range strings.Split(script, "\n") {
		out = append(out, line)
		if !isFlaggedBlockEnd(line) {
			continue
		}
		cmds++
		// The command's own ordinal is 2n-1: each one before it spent two.
		out = append(out,
			"%begin 1 1 1",
			fmt.Sprintf("og-fanout-%d", 2*cmds-1),
			"%end 1 1 1")
	}
	return strings.Join(out, "\n")
}

// isFlaggedBlockEnd reports whether line closes a block one of our own commands
// produced — "%end <t> <n> 1" or "%error <t> <n> 1".
func isFlaggedBlockEnd(line string) bool {
	f := strings.Fields(line)
	if len(f) != 4 || (f[0] != "%end" && f[0] != "%error") {
		return false
	}
	return f[3] == strconv.Itoa(controlmode.ClientCommandFlag)
}

// testStream is a stream whose command side goes nowhere, for driving a reply
// reader against a scripted response stream.
func testStream() *stream { return newStream(io.Discard) }

// noHellos is the waiter for the mirror paths that append no pane, and so never
// reach a hello wait. Calling it at all is the bug it would expose.
func noHellos([]string) (map[string]net.Conn, error) { return nil, nil }

// errWriter stands in for a half-closed ssh stdin — io.Discard never errors, so
// the flush guard has nothing to trip on without it.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestStampAllFailsTheBatchOnFlushError pins the guard between a dead command
// side and a frozen main loop: ordinals handed out for commands tmux never
// received leave every next() waiting on a reply block that cannot arrive.
func TestStampAllFailsTheBatchOnFlushError(t *testing.T) {
	st := newStream(errWriter{})
	if seqs, ok := st.stampAll("a", "b"); ok {
		t.Fatalf("stampAll = %v, %v; want the whole batch failed", seqs, ok)
	}

	// A reader that never yields, so a batch believed to be in flight hangs
	// here exactly as it would against a live remote.
	pr, pw := io.Pipe()
	defer pw.Close()
	rt := newRoundTrip(controlmode.NewReader(pr), NewRouter(), &asyncQueue{}, st)

	yielded := make(chan bool, 1)
	go func() { _, ok := rt("a", "b")(); yielded <- ok }()
	select {
	case ok := <-yielded:
		if ok {
			t.Error("iterator yielded a reply for a batch tmux never received")
		}
	case <-time.After(time.Second):
		t.Fatal("iterator blocked on a lost batch; the main loop would freeze")
	}

	// The consequence one level up: every pane still owed a reply reports the
	// loss rather than hanging.
	seeded := make(chan error, 2)
	go PaneSeeds(rt, []string{"%1", "%2"}, func(_ int, _ []byte, err error) { seeded <- err })
	for i := 0; i < 2; i++ {
		select {
		case err := <-seeded:
			if err == nil {
				t.Errorf("pane %d seeded over a lost batch", i)
			}
		case <-time.After(time.Second):
			t.Fatal("PaneSeeds blocked on a lost batch")
		}
	}
}

// TestLoopRoutesAndExits, TestLoopStopsOnWindowClose, and
// TestLoopReturnsFalseOnEOF (M2.1) drove the extracted runLoop/handleLine
// helpers, which are gone: Task 4 deletes them as dead code (Run's real main
// loop already had its own inline switch, never called them) and flips the
// stop-semantics they encoded — %window-close no longer ends the daemon, only
// %exit/EOF/an emptied registry do (see TestTranslateWindowNotification for the
// B2-filtered translation, and tests/remote-m2-integration.bats for live
// add/close/rename coverage).

// TestReadReplyRoutingRoutesSiblingOutput: while awaiting one command's
// %begin..%end reply, %output for another pane must be routed, not dropped —
// whether it arrives between blocks or inside the guard.
func TestReadReplyRoutingRoutesSiblingOutput(t *testing.T) {
	stream := strings.Join([]string{
		"%output %2 live-B",
		"%begin 1 1 1",
		"cursor-and-capture-reply",
		"%end 1 1 1",
	}, "\n") + "\n"
	reader := newTestReader(stream)
	router := NewRouter()
	var sink capBuf
	router.Register("%2", &sink)

	l, ok := readReplyRouting(reader, router, &asyncQueue{}, testStream(), 1)
	if !ok || l.Kind != controlmode.End {
		t.Fatalf("readReplyRouting returned %+v ok=%v, want End", l, ok)
	}
	if sink.String() != "live-B" {
		t.Errorf("sibling pane-B output %q was dropped, want %q", sink.String(), "live-B")
	}
}

// TestReadReplyRoutingMatchesItsOwnCommand is #276, in the exact shape `prefix
// c` produces: our list-windows is command 2, but three blocks reach it first —
// the reply to the fire-and-forget `new-window` a ctl gesture sent (command 1),
// and one flagged 0 per after-new-window hook the remote ran in our queue.
// Returning any of them hands back an empty body instead of the window list, and
// leaves every later round-trip a command behind.
func TestReadReplyRoutingMatchesItsOwnCommand(t *testing.T) {
	s := strings.Join([]string{
		"%begin 1 1 1", // the ctl gesture's own new-window reply
		"%end 1 1 1",
		"%begin 1 2 0", // after-new-window[0] on the remote
		"%end 1 2 0",
		"%begin 1 3 0", // after-new-window[10]
		"%end 1 3 0",
		"%begin 1 4 1", // ours
		"@5",
		"@6",
		"%end 1 4 1",
	}, "\n") + "\n"

	// A raw reader: this pins readReplyRouting's ordinal matching against a
	// hand-built block stream, so the fixture owns every block in it.
	l, ok := readReplyRouting(rawTestReader(s), NewRouter(), &asyncQueue{}, testStream(), 2)
	if !ok || l.Kind != controlmode.End {
		t.Fatalf("readReplyRouting returned %+v ok=%v, want End", l, ok)
	}
	if got := string(l.Data); got != "@5\n@6" {
		t.Errorf("reply body = %q, want the list-windows output %q", got, "@5\n@6")
	}
}

// TestReadReplyRoutingQueuesNotifications: a notification met while awaiting a
// reply must be handed to the main loop, not dropped. A dropped %pause left its
// pane paused on the remote with no %continue ever sent.
func TestReadReplyRoutingQueuesNotifications(t *testing.T) {
	s := strings.Join([]string{
		"%pause %1",
		"%window-add @7",
		"%begin 1 1 1",
		"body",
		"%end 1 1 1",
	}, "\n") + "\n"

	async := &asyncQueue{}
	if _, ok := readReplyRouting(newTestReader(s), NewRouter(), async, testStream(), 1); !ok {
		t.Fatal("readReplyRouting: want the reply block")
	}
	queued := async.take()
	if len(queued) != 2 || queued[0].Kind != controlmode.Pause || queued[1].Kind != controlmode.WindowAdd {
		t.Fatalf("queued = %+v, want %%pause then %%window-add", queued)
	}
	if rest := async.take(); len(rest) != 0 {
		t.Errorf("take must empty the queue, still holds %+v", rest)
	}
}

// TestCoalesceLayoutChangesKeepsLastPerWindow is #431: a resize burst can
// queue many %layout-change notifications for the same window while a single
// reconcile call's own round-trips are in flight. reconcileLayoutFrom reads
// the surviving line's own Args, so only the last queued notification per
// window can still matter — coalesceLayoutChanges must drop the rest, keep
// the survivor's Args intact, and leave every other notification kind, and
// the relative order of what survives, untouched.
func TestCoalesceLayoutChangesKeepsLastPerWindow(t *testing.T) {
	lines := []controlmode.Line{
		{Kind: controlmode.LayoutChange, Args: []string{"@1", "first-layout", "first-visible", "*"}, Data: []byte("first")},
		{Kind: controlmode.WindowRenamed, Args: []string{"@3"}, Data: []byte("renamed")},
		{Kind: controlmode.LayoutChange, Args: []string{"@2"}, Data: []byte("only")},
		{Kind: controlmode.LayoutChange, Args: []string{"@1", "last-layout", "last-visible", "*Z"}, Data: []byte("last")},
	}

	got := coalesceLayoutChanges(lines)

	want := []controlmode.Line{
		{Kind: controlmode.WindowRenamed, Args: []string{"@3"}, Data: []byte("renamed")},
		{Kind: controlmode.LayoutChange, Args: []string{"@2"}, Data: []byte("only")},
		{Kind: controlmode.LayoutChange, Args: []string{"@1", "last-layout", "last-visible", "*Z"}, Data: []byte("last")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coalesceLayoutChanges = %+v, want %+v", got, want)
	}
}

// fakeSink is a Close()-tracking sink, for asserting closeWindow unregisters
// (and thereby closes) every pane it tears down.
type fakeSink struct{ closed bool }

func (s *fakeSink) Write(p []byte) (int, error) { return len(p), nil }
func (s *fakeSink) Close()                      { s.closed = true }

// TestCloseWindowTearsDownOnlyItsWindow pins the stop-semantics flip: closing
// one remote window must remove it from the registry, unregister/close every
// one of its panes' sinks, close its renderer conns, and kill-window the local
// mirror — without touching any other registered window.
func TestCloseWindowTearsDownOnlyItsWindow(t *testing.T) {
	reg := newRegistry()
	mw := reg.add("@1", "@101")
	mw.remotePanes = []string{"%1", "%2"}
	other := reg.add("@2", "@102")
	router := NewRouter()
	sink1, sink2 := &fakeSink{}, &fakeSink{}
	router.Register("%1", sink1)
	router.Register("%2", sink2)
	c1, c1peer := net.Pipe()
	c2, c2peer := net.Pipe()
	defer c1peer.Close()
	defer c2peer.Close()
	mw.conns["%1"] = c1
	mw.conns["%2"] = c2

	var gotArgs []string
	cfg := Config{LocalTmux: func(args ...string) error { gotArgs = args; return nil }}

	cv := newConverger()
	cv.need("@1", 100, 30)
	closeWindow(cfg, router, newCtlState(), reg, cv, "@1")

	if !cv.need("@1", 100, 30) {
		t.Fatal("closeWindow must forget the closed window's asserted size")
	}

	if !sink1.closed || !sink2.closed {
		t.Fatal("closeWindow must unregister (and close) every pane's sink")
	}
	if _, ok := reg.byRemoteID("@1"); ok {
		t.Fatal("closeWindow must remove the closed window from the registry")
	}
	if _, ok := reg.byRemoteID("@2"); !ok || other.localWin != "@102" {
		t.Fatal("closeWindow must not touch an unrelated registered window")
	}
	want := []string{"kill-window", "-t", "@101"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("LocalTmux called with %v, want %v", gotArgs, want)
	}
	if _, err := c1.Write([]byte("x")); err == nil {
		t.Fatal("closeWindow must close each pane's conn")
	}
}

// TestCloseWindowOutOfRegistryIsNoop is the B2 filter: a %window-close for a
// window this daemon doesn't own must never touch tmux or the registry.
func TestCloseWindowOutOfRegistryIsNoop(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	router := NewRouter()
	called := false
	cfg := Config{LocalTmux: func(args ...string) error { called = true; return nil }}

	closeWindow(cfg, router, newCtlState(), reg, newConverger(), "@9")

	if called {
		t.Fatal("closeWindow must no-op for an out-of-registry window (B2)")
	}
	if _, ok := reg.byRemoteID("@1"); !ok {
		t.Fatal("closeWindow must not remove an unrelated registry entry")
	}
}

// TestPauseContinueReseedsBeforeResumingOutput pins I1: a %pause %N marks the
// sink paused (output dropped), and a %continue %N captures a fresh screen —
// routing sibling %output while that round-trip is in flight (B3) — and writes
// it as a FrameSeed BEFORE any resumed output reaches the pane's conn.
func TestPauseContinueReseedsBeforeResumingOutput(t *testing.T) {
	// %1 -> a real outputSink over one end of a pipe (so router.sink() finds
	// it and the test can read its frames off the peer); %2 -> a capBuf, to
	// assert sibling output isn't dropped during the re-seed round-trip.
	oneLocal, onePeer := net.Pipe()
	defer oneLocal.Close()
	defer onePeer.Close()

	router := NewRouter()
	router.Register("%1", newOutputSink(oneLocal, nil))
	var two capBuf
	router.Register("%2", &two)

	// PaneSeed issues two commands (cursor display-message + capture-pane), so
	// the %continue round-trip carries two reply blocks; the sibling %output
	// lands mid-round-trip, exercising the routing-aware reply reader.
	s := strings.Join([]string{
		"%pause %1",
		"%output %1 dropped-while-paused", // paused: must never reach %1's conn
		"%continue %1",
		"%output %2 sibling", // routed by readReplyRouting during the round-trip
		"%begin 1 1 1",
		"0 0 0 0",
		"%end 1 1 1",
		"%begin 1 2 1",
		"FRESH-CAPTURE",
		"%end 1 2 1",
		"%output %1 after-continue", // must arrive AFTER the seed frame
		"%exit",
	}, "\n") + "\n"

	reader := newTestReader(s)
	st := newStream(io.Discard)
	async := &asyncQueue{}
	rt := newRoundTrip(reader, router, async, st)
	send := func(string) {}
	for {
		l, ok := reader.Next()
		if !ok {
			break
		}
		switch l.Kind {
		case controlmode.Output:
			router.Route(l.Pane, l.Data)
		case controlmode.Pause:
			handlePause(router, send, l.Args[0])
		case controlmode.Continue:
			handleContinue(router, rt, l.Args[0])
		case controlmode.Exit:
			// stop below
		}
		if l.Kind == controlmode.Exit {
			break
		}
	}

	// %1's conn must see FrameSeed(FRESH-CAPTURE) first, then FrameOutput.
	first, err := wire.ReadFrame(onePeer)
	if err != nil {
		t.Fatalf("read seed frame: %v", err)
	}
	if first.Type != wire.FrameSeed {
		t.Fatalf("first frame type = %d, want FrameSeed(%d)", first.Type, wire.FrameSeed)
	}
	if !bytes.Contains(first.Payload, []byte("FRESH-CAPTURE")) {
		t.Errorf("seed frame %q does not contain the fresh capture", first.Payload)
	}
	second, err := wire.ReadFrame(onePeer)
	if err != nil {
		t.Fatalf("read output frame: %v", err)
	}
	if second.Type != wire.FrameOutput {
		t.Fatalf("second frame type = %d, want FrameOutput(%d)", second.Type, wire.FrameOutput)
	}
	if string(second.Payload) != "after-continue" {
		t.Errorf("output frame = %q, want %q", second.Payload, "after-continue")
	}
	if two.String() != "sibling" {
		t.Errorf("sibling pane %%2 recorded %q, want %q (dropped during re-seed)", two.String(), "sibling")
	}
}

// TestPauseContinueReplaysRetainedKittyStoreAfterSeed pins #465: a re-seed
// restores placeholders but not kitty stores, so the retained localised store
// must land as FrameOutput immediately after the FrameSeed.
func TestPauseContinueReplaysRetainedKittyStoreAfterSeed(t *testing.T) {
	oneLocal, onePeer := net.Pipe()
	defer oneLocal.Close()
	defer onePeer.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	router := NewRouter()
	router.Register("%1", newOutputSink(oneLocal, p))

	s := strings.Join([]string{
		"%output %1 " + string(testKittyStore("9")),
		"%pause %1",
		"%continue %1",
		"%begin 1 1 1",
		"0 0 0 0",
		"%end 1 1 1",
		"%begin 1 2 1",
		"FRESH-CAPTURE",
		"%end 1 2 1",
		"%exit",
	}, "\n") + "\n"

	reader := newTestReader(s)
	st := newStream(io.Discard)
	rt := newRoundTrip(reader, router, &asyncQueue{}, st)
	for {
		l, ok := reader.Next()
		if !ok {
			break
		}
		switch l.Kind {
		case controlmode.Output:
			router.Route(l.Pane, l.Data)
		case controlmode.Pause:
			handlePause(router, func(string) {}, l.Args[0])
		case controlmode.Continue:
			handleContinue(router, rt, l.Args[0])
		}
		if l.Kind == controlmode.Exit {
			break
		}
	}

	store, err := wire.ReadFrame(onePeer)
	if err != nil {
		t.Fatalf("read initial store: %v", err)
	}
	if store.Type != wire.FrameOutput || !bytes.Contains(store.Payload, []byte(kittyLocalisedMarker)) {
		t.Fatalf("initial store = %v %q", store.Type, store.Payload)
	}

	seed, replay := seedThenReplayFrames(t, onePeer)
	if seed.Type != wire.FrameSeed {
		t.Fatalf("first frame = %v, want FrameSeed", seed.Type)
	}
	if !bytes.Contains(seed.Payload, []byte("FRESH-CAPTURE")) {
		t.Fatalf("seed = %q, want FRESH-CAPTURE", seed.Payload)
	}
	if replay.Type != wire.FrameOutput {
		t.Fatalf("second frame = %v, want FrameOutput replay", replay.Type)
	}
	if !bytes.Contains(replay.Payload, []byte(kittyLocalisedMarker)) {
		t.Fatalf("replay = %q, want retained localised store", replay.Payload)
	}
}

// TestOutputSinkCloseDiscardsReplayState: retained stores live on the sink's
// proxy; teardown drops the sink and a fresh proxy must not inherit replay.
func TestOutputSinkCloseDiscardsReplayState(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	s := newOutputSink(remote, p)
	s.Write(testKittyStore("1"))
	if got := readAllFrames(t, local, 200*time.Millisecond); !strings.Contains(got, kittyLocalisedMarker) {
		t.Fatalf("store not forwarded: %q", got)
	}
	s.Close()
	s.Wait()
	if len(p.Replay()) != 0 {
		t.Fatal("closed sink's proxy still retained replay state")
	}

	p2 := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	if len(p2.Replay()) != 0 {
		t.Fatal("fresh proxy inherited replay state")
	}

	local2, remote2 := net.Pipe()
	defer local2.Close()
	defer remote2.Close()
	s2 := newOutputSink(remote2, p2)
	enqueueSeedWithReplay(s2, []byte("only-seed"))
	seed, err := wire.ReadFrame(local2)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if seed.Type != wire.FrameSeed || string(seed.Payload) != "only-seed" {
		t.Fatalf("seed = %v %q", seed.Type, seed.Payload)
	}
	if err := local2.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.ReadFrame(local2); err == nil {
		t.Fatal("fresh sink replayed kitty stores without a new store passing through")
	}
}

// TestWaitHellosTimesOutWhenRenderersDontConnect uses a real listener
// that nobody dials: a spawned renderer that never connects back (bad
// RendererBin, exec failure, crash) must not wedge the wait forever.
//
// The pump reads a pipe nothing is written to, so the deadline is the only arm
// that can fire — a reader over an already-ended stream would end the wait on
// the pump-close arm instead and prove nothing about the timeout.
func TestWaitHellosTimesOutWhenRenderersDontConnect(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	connCh := make(chan helloConn, 16)
	go acceptConns(l, connCh, func([]string) error { return nil })

	pr, pw := io.Pipe()
	defer pw.Close()
	pump := startCtlPump(controlmode.NewReader(pr))

	start := time.Now()
	_, err = waitHellos(pump.lines, NewRouter(), &asyncQueue{}, testStream(), connCh, []string{"%1"}, 100*time.Millisecond, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitHellos: want an error when no renderer connects, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("waitHellos blocked for %s, want it to return near the 100ms deadline", elapsed)
	}
}

// nudgeResult is TestWatchLocalClientReconvergesOnChange's fake for the
// resize hook's nudge file: ok mirrors os.Stat failing (no touch yet), t its
// mtime.
type nudgeResult struct {
	t  time.Time
	ok bool
}

// noResolveView is the fake resolveView for tests that exercise the size half
// of watchLocalClient only: an always-empty resolution never stores a
// capability and never sends (R3), which keeps `sent` free of RelayEnvCmd
// noise so the size assertions below stay exact.
func noResolveView() (ViewIdentity, bool) { return ViewIdentity{}, false }

// TestWatchLocalClientReconvergesOnChange drives watchLocalClient
// deterministically (no time.Sleep): nudged and area read from channels so the
// test controls exactly what the watcher observes each tick, and each tick
// sent on `tick` blocks until the watcher is back at its select — so sending
// the next tick is a barrier that proves the previous iteration (including any
// nudge/area read and send) has fully completed.
func TestWatchLocalClientReconvergesOnChange(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudgeCh := make(chan nudgeResult)
	sizeCh := make(chan [2]int)
	nudged := func() (time.Time, bool) { n := <-nudgeCh; return n.t, n.ok }
	area := func() (int, int) { s := <-sizeCh; return s[0], s[1] }

	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "host-sess:2")
	cv := newConverger()
	// The setup path already asserted the startup size — the client's own size
	// as well as both windows' caps.
	cv.need(clientSizeKey, 100, 30)
	cv.need("@1", 100, 30)
	cv.need("@2", 100, 30)
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.Relay{})}

	var sent []string
	send := func(s ...string) bool { sent = append(sent, s...); return true }

	done := make(chan struct{})
	go func() {
		watchLocalClient(area, nudged, func() string { return "" }, noResolveView, view, "sess", reg, cv, send, stop, tick)
		close(done)
	}()

	t1 := time.Now()
	t2 := t1.Add(time.Second)

	// Tick 1 — nudge file not yet created (no hook has fired): no stat, no
	// send.
	tick <- time.Now()
	nudgeCh <- nudgeResult{ok: false}

	// Tick 2 — first-ever touch, but the resize left the size unchanged
	// (100x30): area is polled (nudged, so no fork skipped) but must NOT
	// resend.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t1, ok: true}
	sizeCh <- [2]int{100, 30}

	// Tick 3 — mtime advanced and the size changed (120x40): the client size
	// once, then one send per mirrored window. This tick's sends block the
	// goroutine from re-selecting, so the next tick can't unblock until they
	// land.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t2, ok: true}
	sizeCh <- [2]int{120, 40}

	// Tick 4 — same mtime as tick 3 (no new touch): area must NOT be polled at
	// all, so no send even though a stale sizeCh value would mismatch.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: t2, ok: true}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLocalClient did not return after stop was closed")
	}

	// remoteIDs snapshots a map, so the per-window sends land in either order.
	sort.Strings(sent)
	want := []string{ClientSizeCmd(120, 40), ConvergeCmd("@1", 120, 40), ConvergeCmd("@2", 120, 40)}
	sort.Strings(want)
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
}

// TestWatchLocalClientConvergesActiveWindowFirst pins the #557 ordering: the
// window the local client is looking at is converged ahead of the background
// ones, so its %layout-change (and the reconcile it queues) lands first on a
// slow link.
func TestWatchLocalClientConvergesActiveWindowFirst(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudgeCh := make(chan nudgeResult)
	sizeCh := make(chan [2]int)
	nudged := func() (time.Time, bool) { n := <-nudgeCh; return n.t, n.ok }
	area := func() (int, int) { s := <-sizeCh; return s[0], s[1] }

	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	cv := newConverger()
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.Relay{})}

	var sent []string
	send := func(s ...string) bool { sent = append(sent, s...); return true }

	done := make(chan struct{})
	go func() {
		watchLocalClient(area, nudged, func() string { return "@102" }, noResolveView, view, "sess", reg, cv, send, stop, tick)
		close(done)
	}()

	tick <- time.Now()
	nudgeCh <- nudgeResult{t: time.Now(), ok: true}
	sizeCh <- [2]int{120, 40}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLocalClient did not return after stop was closed")
	}

	want := []string{ClientSizeCmd(120, 40), ConvergeCmd("@2", 120, 40), ConvergeCmd("@1", 120, 40)}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v, want %v (active window @2 first)", sent, want)
	}
}

// TestWatchLocalClientDoesNotRecordAWriteThatDidNotHappen pins the converger
// invariant: its recorded size is never ahead of what the remote was actually
// told. need() records at the moment it returns true, before the write — so a
// send onto a dead stream would otherwise latch that size and the window would
// never be re-sent it.
func TestWatchLocalClientDoesNotRecordAWriteThatDidNotHappen(t *testing.T) {
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudgeCh := make(chan nudgeResult)
	sizeCh := make(chan [2]int)
	nudged := func() (time.Time, bool) { n := <-nudgeCh; return n.t, n.ok }
	area := func() (int, int) { s := <-sizeCh; return s[0], s[1] }

	reg := newRegistry()
	reg.add("@1", "@101")
	cv := newConverger()
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.Relay{})}

	var sent []string
	send := func(s ...string) bool { sent = append(sent, s...); return false }

	done := make(chan struct{})
	go func() {
		watchLocalClient(area, nudged, func() string { return "" }, noResolveView, view, "sess", reg, cv, send, stop, tick)
		close(done)
	}()

	// One tick with a fresh touch and a new size: both the client size and the
	// window's cap are attempted, and both writes report failure.
	tick <- time.Now()
	nudgeCh <- nudgeResult{t: time.Now(), ok: true}
	sizeCh <- [2]int{120, 40}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLocalClient did not return after stop was closed")
	}

	want := []string{ClientSizeCmd(120, 40), ConvergeCmd("@1", 120, 40)}
	sort.Strings(sent)
	sort.Strings(want)
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
	if !cv.need("@1", 120, 40) {
		t.Error("@1's cap was recorded despite a failed write, so the next tick would skip it")
	}
	if !cv.need(clientSizeKey, 120, 40) {
		t.Error("the client size was recorded despite a failed write")
	}
}

// runWatchLocalClientView drives one due tick of watchLocalClient with a
// fixed resolve result — id/ok are returned on every call, not just the
// first — and area/reg/cv arranged so only the view-identity half of the
// watcher can produce a send: area returns 0,0 (the client-size gate is
// `w > 0 && h > 0`) and reg is empty (no per-window ConvergeCmd). Returns the
// commands sent and the view cell the watcher wrote into.
func runWatchLocalClientView(t *testing.T, seedRelay graphics.Relay, seedTerm string, id ViewIdentity, ok bool) ([]string, *Viewing) {
	t.Helper()
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudgeCh := make(chan nudgeResult)
	nudged := func() (time.Time, bool) { n := <-nudgeCh; return n.t, n.ok }
	area := func() (int, int) { return 0, 0 }
	resolveView := func() (ViewIdentity, bool) { return id, ok }

	reg := newRegistry()
	cv := newConverger()
	view := &Viewing{Relay: graphics.NewRelaySource(seedRelay)}
	view.SetDesired(seedTerm)

	var sent []string
	send := func(s ...string) bool { sent = append(sent, s...); return true }

	done := make(chan struct{})
	go func() {
		watchLocalClient(area, nudged, func() string { return "" }, resolveView, view, "sess", reg, cv, send, stop, tick)
		close(done)
	}()

	tick <- time.Now()
	nudgeCh <- nudgeResult{t: time.Now(), ok: true}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLocalClient did not return after stop was closed")
	}
	return sent, view
}

// TestWatchLocalClientPublishesOnCapabilityChange is R5: a resolve whose
// capability differs from what is currently stored publishes exactly one
// RelayEnvCmd carrying the new value, and the new capability is stored.
func TestWatchLocalClientPublishesOnCapabilityChange(t *testing.T) {
	sent, view := runWatchLocalClientView(t, graphics.Relay{}, "foot",
		ViewIdentity{Term: "foot", Relay: graphics.NewRelayFromClient(true, "sixel")}, true)

	want := []string{RelayEnvCmd("sess", "sixel")}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
	if !view.Relay.Load().Sixel() {
		t.Error("the resolved capability was not stored")
	}
}

// TestWatchLocalClientSkipsUnchangedCapability: the same capability resolving
// again must not re-publish (R5 fires on a CHANGE, not on every tick).
func TestWatchLocalClientSkipsUnchangedCapability(t *testing.T) {
	sent, _ := runWatchLocalClientView(t, graphics.NewRelayFromClient(true, "sixel"), "xterm-kitty",
		ViewIdentity{Term: "xterm-kitty", Relay: graphics.NewRelayFromClient(true, "sixel")}, true)

	if len(sent) != 0 {
		t.Fatalf("sent = %v, want none — the capability did not change", sent)
	}
}

// TestWatchLocalClientTermOnlyChangeSendsNothing: Desired moves (every dial
// reads it fresh, so asserting it is free) but nothing is published from this
// path — RelayEnvCmd carries only the capability, never the termname.
func TestWatchLocalClientTermOnlyChangeSendsNothing(t *testing.T) {
	sent, view := runWatchLocalClientView(t, graphics.NewRelayFromClient(true, "sixel"), "xterm-kitty",
		ViewIdentity{Term: "foot", Relay: graphics.NewRelayFromClient(true, "sixel")}, true)

	if len(sent) != 0 {
		t.Fatalf("sent = %v, want none (termname-only change)", sent)
	}
	if got := view.Desired(); got != "foot" {
		t.Errorf("Desired() = %q, want foot", got)
	}
}

// TestWatchLocalClientEmptyResolutionPreservesState is R3: nobody attached to
// the mirror session is not evidence, so an empty resolution must store
// nothing, send nothing, and leave Desired untouched — this path now fires on
// every client-session-changed/client-detached tick, so a mirror nobody is
// looking at must not be degraded by it.
func TestWatchLocalClientEmptyResolutionPreservesState(t *testing.T) {
	sent, view := runWatchLocalClientView(t, graphics.NewRelayFromClient(true, "sixel"), "xterm-kitty",
		ViewIdentity{}, false)

	if len(sent) != 0 {
		t.Fatalf("sent = %v, want none", sent)
	}
	if got := view.Desired(); got != "xterm-kitty" {
		t.Errorf("Desired() = %q, want unchanged xterm-kitty", got)
	}
	if !view.Relay.Load().Sixel() {
		t.Error("the previously stored capability must survive an empty resolution")
	}
}

func TestOutputSinkFiltersAndCoalescesThroughTheProxy(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)

	// Construct the sink without starting its pump, write both stores onto
	// s.ch directly, then start the pump: its first receive is guaranteed to
	// see both frames already queued, so drainOutput's batch is deterministic
	// instead of racing the goroutine's startup against these writes.
	s := &outputSink{ch: make(chan sinkFrame, outputSinkBuf), gfx: p}
	seq := func(payload string) []byte {
		return []byte("\x1b_Gi=7,a=T,U=1,f=100,t=f;" + payload + "\x1b\\")
	}
	s.Write(seq("L3RtcC9hLnBuZw==")) // /tmp/a.png
	s.Write(seq("L3RtcC9iLnBuZw==")) // /tmp/b.png
	s.start(remote)
	defer s.Close()

	got := readAllFrames(t, local, 500*time.Millisecond)
	if n := strings.Count(got, "\x1bPtmux;"); n != 1 {
		t.Fatalf("forwarded %d stores, want 1 (the newest); got %q", n, got)
	}
	if !strings.Contains(got, "L2xvY2FsL2EuYmlu") {
		t.Fatalf("payload not localised: %q", got)
	}
}

// TestDrainOutputCoalescesConsecutiveStopsAtBoundary pins drainOutput's two
// obligations in isolation, without the pump goroutine or a net.Pipe: it must
// concatenate consecutive queued FrameOutput payloads in order, and it must
// stop at the first non-output frame and hand that frame back as "pending"
// rather than merging it in — the frozen wire invariant that a seed or resize
// never gets reordered past output (sinkFrame's doc comment).
func TestDrainOutputCoalescesConsecutiveStopsAtBoundary(t *testing.T) {
	boundary := func(t *testing.T, typ wire.FrameType, payload []byte) {
		t.Helper()
		ch := make(chan sinkFrame, 8)
		ch <- sinkFrame{typ: wire.FrameOutput, payload: []byte("AB")}
		ch <- sinkFrame{typ: wire.FrameOutput, payload: []byte("CD")}
		ch <- sinkFrame{typ: typ, payload: payload}
		ch <- sinkFrame{typ: wire.FrameOutput, payload: []byte("EF")}

		// start() seeds buf with the first frame's payload before calling
		// drainOutput; mirror that call shape here.
		first := <-ch
		buf := append([]byte(nil), first.payload...)
		buf, pending := drainOutput(ch, buf)

		if got, want := string(buf), "AB"+"CD"; got != want {
			t.Fatalf("drained buf = %q, want %q", got, want)
		}
		if pending == nil {
			t.Fatal("pending = nil, want the boundary frame returned")
		}
		if pending.typ != typ {
			t.Fatalf("pending.typ = %d, want %d", pending.typ, typ)
		}
		if !bytes.Equal(pending.payload, payload) {
			t.Fatalf("pending.payload = %q, want %q", pending.payload, payload)
		}

		// The trailing FrameOutput must still be sitting unread on ch: drainOutput
		// must not have looked past the boundary frame.
		select {
		case v := <-ch:
			if v.typ != wire.FrameOutput || string(v.payload) != "EF" {
				t.Fatalf("trailing frame = %+v, want FrameOutput %q", v, "EF")
			}
		default:
			t.Fatal("trailing FrameOutput frame was consumed by drainOutput, want it left on ch")
		}
	}

	t.Run("FrameResize boundary", func(t *testing.T) {
		boundary(t, wire.FrameResize, wire.EncodeResize(80, 24))
	})
	t.Run("FrameSeed boundary", func(t *testing.T) {
		boundary(t, wire.FrameSeed, []byte("seed-payload"))
	})
}

// graphics.Proxy.Close() flushes a partial sequence the Scanner was still
// holding — but it's only reachable if newOutputSink's pump actually calls it
// on teardown. gfx is a value captured by the pump closure, not a field
// Close() can reach from another goroutine, so this has to be wired inside
// the pump's own !ok exit path.
func TestOutputSinkFlushesTheProxyHeldPartialOnClose(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	p := graphics.New(&stubLocalizer{local: "/local/a.bin"}, nil)
	s := newOutputSink(remote, p)

	const partial = "\x1b_Gi=1,a=T;abc" // no ST: the scanner holds it, incomplete
	s.Write([]byte(partial))
	s.Close()

	got := readAllFrames(t, local, 500*time.Millisecond)
	if got != partial {
		t.Fatalf("got = %q, want the held partial %q flushed on close", got, partial)
	}
}

type stubLocalizer struct{ local string }

func (s *stubLocalizer) Localize(context.Context, string) (string, error) { return s.local, nil }

const kittyLocalisedMarker = "L2xvY2FsL2EuYmlu"

func testKittyStore(id string) []byte {
	return []byte("\x1b_Gi=" + id + ",a=T,t=f;L3RtcC94LnBuZw==\x1b\\")
}

func seedThenReplayFrames(t *testing.T, peer net.Conn) (seed, replay wire.Frame) {
	t.Helper()
	var err error
	seed, err = wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	replay, err = wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	return seed, replay
}

// readAllFrames reads frames off conn until it goes quiet for the deadline and
// returns their concatenated payloads.
func readAllFrames(t *testing.T, conn net.Conn, quiet time.Duration) string {
	t.Helper()
	var out []byte
	for {
		if err := conn.SetReadDeadline(time.Now().Add(quiet)); err != nil {
			t.Fatal(err)
		}
		f, err := wire.ReadFrame(conn)
		if err != nil {
			return string(out)
		}
		out = append(out, f.Payload...)
	}
}

// TestCreateMirrorWindowReturnsAnID pins the two halves of #411's fix: the
// window is appended at {end} (so tmux's own renumbering keeps the mirror
// contiguous) and what the registry stores is the id tmux printed, not an
// index this daemon guessed.
func TestCreateMirrorWindowReturnsAnID(t *testing.T) {
	var gotArgs []string
	cfg := Config{
		LocalSess: "h-s",
		LocalTmuxOut: func(args ...string) (string, error) {
			gotArgs = args
			return "@42\n", nil
		},
	}
	got, err := createMirrorWindow(cfg)
	if err != nil || got != "@42" {
		t.Fatalf("createMirrorWindow = %q, %v; want @42", got, err)
	}
	want := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", "h-s:{end}"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("LocalTmuxOut called with %v, want %v", gotArgs, want)
	}
}

// TestFirstMirrorWindowTakesTheFirstLine covers the window the launcher left
// behind, which the first remote window reuses.
func TestFirstMirrorWindowTakesTheFirstLine(t *testing.T) {
	cfg := Config{
		LocalSess:    "h-s",
		LocalTmuxOut: func(...string) (string, error) { return "@7\n@8\n", nil },
	}
	got, err := firstMirrorWindow(cfg)
	if err != nil || got != "@7" {
		t.Fatalf("firstMirrorWindow = %q, %v; want @7", got, err)
	}
}

// TestParseWindowIDRejectsAnIndex is the guard that matters: tmux reads a bare
// "7" in a target-window slot as INDEX 7, so accepting one would reintroduce
// exactly the addressing #411 removed.
func TestParseWindowIDRejectsAnIndex(t *testing.T) {
	for _, in := range []string{"7", "", "@", "h-s:1"} {
		if got, err := parseWindowID(in); err == nil {
			t.Errorf("parseWindowID(%q) = %q, want an error", in, got)
		}
	}
}

// A mirror pane must be opted into unrestricted passthrough at creation. With
// the global left at `on`, tmux drops a kitty image store aimed at a pane whose
// window is not the client's current one, and never retransmits it — so a
// carousel opened by reconcile rather than by the keypress paints chrome around
// an empty picture (#464). Both creation paths stamp through this function, so
// asserting it here covers respawn-pane and the mirrored split alike.
func TestMarkRendererPaneStampsBridgePaneAndPassthrough(t *testing.T) {
	var calls [][]string
	cfg := Config{LocalTmux: func(args ...string) error {
		calls = append(calls, args)
		return nil
	}}

	markRendererPane(cfg, "%7", "%42")

	want := [][]string{
		{"set-option", "-p", "-t", "%7", "@bridge_pane", "%42"},
		{"set-option", "-p", "-t", "%7", "allow-passthrough", "all"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("markRendererPane calls =\n%q\nwant\n%q", calls, want)
	}
}
