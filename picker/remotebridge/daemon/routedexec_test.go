package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestReadReplyRoutingReturnsParkedReply pins the slot's read side: a reply
// routeWhile already parked answers readReplyRouting(want) directly, with no
// read off the stream at all — the reader here holds only an %output line
// that must stay untouched.
func TestReadReplyRoutingReturnsParkedReply(t *testing.T) {
	st := testStream()
	seqs, ok := st.stampAll("a")
	if !ok {
		t.Fatal("stampAll failed")
	}
	l := controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte("parked")}
	seq := claimSeq(l, st)
	if seq != seqs[0] {
		t.Fatalf("claimSeq = %d, want %d", seq, seqs[0])
	}
	st.park(seq, l)

	router := NewRouter()
	var sink capBuf
	router.Register("%1", &sink)

	got, ok := readReplyRouting(rawTestReader("%output %1 after\n"), router, &asyncQueue{}, st, seqs[0])
	if !ok || string(got.Data) != "parked" {
		t.Fatalf("readReplyRouting = %+v, ok=%v; want the parked reply", got, ok)
	}
	if sink.String() != "" {
		t.Errorf("sink = %q, want empty: nothing past the parked reply was read", sink.String())
	}
}

// TestReadReplyRoutingDropsStaleParkedReply pins the slot's stale-drop: a
// parked reply that isn't the one asked for is dropped, exactly as the old
// walk would have dropped it in passing, and reading resumes on the stream.
func TestReadReplyRoutingDropsStaleParkedReply(t *testing.T) {
	st := testStream()
	seqs, ok := st.stampAll("a", "b")
	if !ok {
		t.Fatal("stampAll failed")
	}
	l := controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte("a-reply")}
	seq := claimSeq(l, st)
	if seq != seqs[0] {
		t.Fatalf("claimSeq = %d, want %d", seq, seqs[0])
	}
	st.park(seq, l)

	// A raw reader: barrier 2's own reply (og-fanout-1), then b's reply.
	s := strings.Join([]string{
		"%begin 1 2 1",
		"og-fanout-1",
		"%end 1 2 1",
		"%begin 1 3 1",
		"B",
		"%end 1 3 1",
	}, "\n") + "\n"

	got, ok := readReplyRouting(rawTestReader(s), NewRouter(), &asyncQueue{}, st, seqs[1])
	if !ok || string(got.Data) != "B" {
		t.Fatalf("readReplyRouting = %+v, ok=%v; want b's reply %q", got, ok, "B")
	}
	if p := st.parkedSeq(); p != 0 {
		t.Errorf("parkedSeq = %d, want 0: the stale slot was taken and dropped", p)
	}
}

// pumpReaderOver reads lines the way the main loop's round-trips do, so a
// batch and routeWhile can share one channel as they share the pump.
func pumpReaderOver(lines chan controlmode.Line) lineReader { return &ctlPump{lines: lines} }

func replyLine(body string) controlmode.Line {
	return controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte(body)}
}

func outputLine(pane, data string) controlmode.Line {
	return controlmode.Line{Kind: controlmode.Output, Pane: pane, Data: []byte(data)}
}

// within runs f and fails the test if it hasn't returned after d, so a
// routeWhile that stops making progress fails instead of hanging the run.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
	}
}

// waitFor reports whether ch closed before d elapsed.
func waitFor(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// waitParked blocks until st's slot holds seq, bounded, then gives routeWhile
// a further beat in which reading on past the parked reply would show.
func waitParked(st *stream, seq uint64) {
	deadline := time.Now().Add(2 * time.Second)
	for st.parkedSeq() != seq && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestRouteWhileRoutesOutputDuringExec is #808's core: the exec only returns
// once %output that arrived during it has been routed, so an exec that blocks
// the stream (the pre-change model) never sees it.
func TestRouteWhileRoutesOutputDuringExec(t *testing.T) {
	st := testStream()
	router := NewRouter()
	sink := newSignalSink()
	router.Register("%0", sink)
	lines := make(chan controlmode.Line, 4)
	lines <- outputLine("%0", "echo")

	var routed bool
	within(t, 5*time.Second, func() {
		routeWhile(lines, router, &asyncQueue{}, st, func() { routed = waitFor(sink.got, 2*time.Second) })
	})
	if !routed {
		t.Fatalf("exec never saw its %%output routed: routeWhile did not read the stream while it ran")
	}
	if got := sink.String(); got != "echo" {
		t.Errorf("sink = %q, want %q", got, "echo")
	}
}

// TestRouteWhileQueuesNotifications pins waitHellos' disposal of a non-output
// notification: queued for settle, never dispatched or dropped.
func TestRouteWhileQueuesNotifications(t *testing.T) {
	st := testStream()
	router := NewRouter()
	sink := newSignalSink()
	router.Register("%0", sink)
	async := &asyncQueue{}
	lines := make(chan controlmode.Line, 4)
	lines <- controlmode.Line{Kind: controlmode.WindowAdd, Args: []string{"@9"}}
	lines <- outputLine("%0", "x")

	var routed bool
	within(t, 5*time.Second, func() {
		routeWhile(lines, router, async, st, func() { routed = waitFor(sink.got, 2*time.Second) })
	})
	if !routed {
		t.Fatalf("%%output behind the notification was never routed")
	}
	q := async.take()
	if len(q) != 1 || q[0].Kind != controlmode.WindowAdd || q[0].Args[0] != "@9" {
		t.Fatalf("async queue = %+v, want exactly the %%window-add @9", q)
	}
}

// TestRouteWhileDropsUnawaitedReplies pins the claim-and-drop path for a
// fire-and-forget reply and its barrier: consumed, counted, and the next
// command's ordinal still lines up.
func TestRouteWhileDropsUnawaitedReplies(t *testing.T) {
	st := testStream()
	if !st.send("send-keys -t %0 x") {
		t.Fatal("send failed")
	}
	router := NewRouter()
	sink := newSignalSink()
	router.Register("%0", sink)
	lines := make(chan controlmode.Line, 4)
	lines <- replyLine("")
	lines <- replyLine("og-fanout-1")
	lines <- outputLine("%0", "x")

	var routed bool
	within(t, 5*time.Second, func() {
		routeWhile(lines, router, &asyncQueue{}, st, func() { routed = waitFor(sink.got, 2*time.Second) })
	})
	if !routed {
		t.Fatalf("%%output behind the dropped replies was never routed")
	}
	if p := st.parkedSeq(); p != 0 {
		t.Errorf("parkedSeq = %d, want 0: no round-trip awaited those replies", p)
	}
	seqs, ok := st.stampAll("next")
	if !ok {
		t.Fatal("stampAll failed")
	}
	if got := st.claim(nil); got != seqs[0] {
		t.Errorf("next claim = %d, want %d: ordinals drifted", got, seqs[0])
	}
}

// TestRouteWhileParksAwaitedReplyAndStops pins seed-before-output: a reply a
// lazy batch still awaits is parked, and the %output behind it stays unread
// until that batch's next() has taken the reply.
func TestRouteWhileParksAwaitedReplyAndStops(t *testing.T) {
	st := testStream()
	router := NewRouter()
	async := &asyncQueue{}
	var sink capBuf
	router.Register("%1", &sink)
	lines := make(chan controlmode.Line, 4)
	rt := newRoundTrip(pumpReaderOver(lines), router, async, st)
	next := rt("capture-pane -p -t %1")
	lines <- replyLine("seed")
	lines <- outputLine("%1", "after")

	within(t, 5*time.Second, func() {
		routeWhile(lines, router, async, st, func() { waitParked(st, 1) })
	})
	if sink.String() != "" {
		t.Fatalf("sink = %q before the seed was read, want empty", sink.String())
	}
	if p := st.parkedSeq(); p != 1 {
		t.Fatalf("parkedSeq = %d, want 1", p)
	}
	l, ok := next()
	if !ok || string(l.Data) != "seed" {
		t.Fatalf("next() = %+v, ok=%v; want the parked seed", l, ok)
	}
	if sink.String() != "" {
		t.Fatalf("sink = %q after next(), want empty: the parked reply was served without reading", sink.String())
	}
	l, _, ok = nextLine(pumpReaderOver(lines), st)
	if !ok {
		t.Fatalf("the %%output behind the parked reply is gone")
	}
	handleAsideLine(l, router, async)
	if sink.String() != "after" {
		t.Errorf("sink = %q, want %q", sink.String(), "after")
	}
}

// TestRouteWhileSkipsReadingWhileParked pins the occupied-slot rule: with a
// reply already parked, routeWhile reads nothing, since anything it routed
// would land ahead of the parked reply's reader.
func TestRouteWhileSkipsReadingWhileParked(t *testing.T) {
	st := testStream()
	router := NewRouter()
	async := &asyncQueue{}
	var sink capBuf
	router.Register("%0", &sink)
	lines := make(chan controlmode.Line, 4)
	next := newRoundTrip(pumpReaderOver(lines), router, async, st)("a")
	l := replyLine("A")
	st.park(claimSeq(l, st), l)
	lines <- outputLine("%0", "x")

	within(t, 5*time.Second, func() {
		routeWhile(lines, router, async, st, func() { time.Sleep(50 * time.Millisecond) })
	})
	if sink.String() != "" {
		t.Errorf("sink = %q, want empty: nothing is read while the slot is occupied", sink.String())
	}
	if n := len(lines); n != 1 {
		t.Errorf("len(lines) = %d, want 1", n)
	}
	if got, ok := next(); !ok || string(got.Data) != "A" {
		t.Errorf("next() = %+v, ok=%v; want the parked reply", got, ok)
	}
}

// TestRouteWhileParksAtBarrierMidBatch pins the park floor mid-batch: after
// the first next(), the barrier behind it is itself at or below awaitHigh, so
// routeWhile parks there and the batch's second next() still finds its reply.
func TestRouteWhileParksAtBarrierMidBatch(t *testing.T) {
	st := testStream()
	router := NewRouter()
	async := &asyncQueue{}
	lines := make(chan controlmode.Line, 4)
	next := newRoundTrip(pumpReaderOver(lines), router, async, st)("a", "b")
	lines <- replyLine("A")
	if l, ok := next(); !ok || string(l.Data) != "A" {
		t.Fatalf("first next() = %+v, ok=%v; want %q", l, ok, "A")
	}
	lines <- replyLine("og-fanout-1")
	lines <- replyLine("B")
	lines <- replyLine("og-fanout-3")

	within(t, 5*time.Second, func() {
		routeWhile(lines, router, async, st, func() { waitParked(st, 2) })
	})
	if p := st.parkedSeq(); p != 2 {
		t.Fatalf("parkedSeq = %d, want 2 (barrier behind a)", p)
	}
	if n := len(lines); n != 2 {
		t.Errorf("len(lines) = %d, want 2: reading stopped at the barrier", n)
	}
	if l, ok := next(); !ok || string(l.Data) != "B" {
		t.Errorf("second next() = %+v, ok=%v; want %q", l, ok, "B")
	}
}

// TestRouteWhileEOFMidExec pins stream end during an exec: routeWhile still
// returns once the exec does, and the next reader sees the same EOF.
func TestRouteWhileEOFMidExec(t *testing.T) {
	st := testStream()
	router := NewRouter()
	async := &asyncQueue{}
	lines := make(chan controlmode.Line)
	close(lines)

	within(t, 5*time.Second, func() {
		routeWhile(lines, router, async, st, func() { time.Sleep(20 * time.Millisecond) })
	})
	if _, ok := readReplyRouting(pumpReaderOver(lines), router, async, st, 1); ok {
		t.Error("readReplyRouting after EOF returned ok, want the EOF path")
	}
}

// TestRoutingWrapsEveryExecHook pins Config.routing's contract: each
// exec-backed hook's own call runs inside run, in the returned copy only,
// with results passed through unchanged and a nil hook left nil.
func TestRoutingWrapsEveryExecHook(t *testing.T) {
	var localTmux, localTmuxOut, localArea, reflow, localPanes int
	runCalls := 0
	run := func(fn func()) { runCalls++; fn() }

	cfg := Config{
		LocalTmux: func(args ...string) error {
			localTmux++
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			localTmuxOut++
			return "out", nil
		},
		LocalArea: func() (int, int) {
			localArea++
			return 80, 24
		},
		Reflow: func() {
			reflow++
		},
		LocalPanes: func() map[string]string {
			localPanes++
			return map[string]string{"%1": "%2"}
		},
	}

	routed := cfg.routing(run)

	if err := routed.LocalTmux("x"); err != nil {
		t.Errorf("LocalTmux returned %v, want nil", err)
	}
	if out, err := routed.LocalTmuxOut("x"); out != "out" || err != nil {
		t.Errorf("LocalTmuxOut = %q, %v; want %q, nil", out, err, "out")
	}
	if w, h := routed.LocalArea(); w != 80 || h != 24 {
		t.Errorf("LocalArea = %d,%d; want 80,24", w, h)
	}
	routed.Reflow()
	if panes := routed.LocalPanes(); len(panes) != 1 || panes["%1"] != "%2" {
		t.Errorf("LocalPanes = %+v, want map with %%1 -> %%2", panes)
	}

	if runCalls != 5 {
		t.Errorf("run calls = %d, want 5", runCalls)
	}
	if localTmux != 1 || localTmuxOut != 1 || localArea != 1 || reflow != 1 || localPanes != 1 {
		t.Errorf("hook counters = %d,%d,%d,%d,%d, want all 1", localTmux, localTmuxOut, localArea, reflow, localPanes)
	}

	if routedNil := (Config{}).routing(run); routedNil.Reflow != nil {
		t.Error("routing a nil Reflow hook produced a non-nil one")
	}
}

// TestPasterUsesPlainHooks pins goroutine confinement (#808): paster()
// restores the plain hooks from a routing Config before building its
// closures, since routeWhile's run is only correct on the main-loop
// goroutine and pumpInput's paste handler runs on its own.
func TestPasterUsesPlainHooks(t *testing.T) {
	var plainCalls int
	plain := Config{
		LocalSess: "host-sess",
		LocalTmuxOut: func(args ...string) (string, error) {
			plainCalls++
			return "%1|claude\n", nil
		},
		PasteUpload: func(ctx context.Context, ext string, data []byte) (string, error) {
			return "", nil
		},
	}
	routed := plain.routing(func(fn func()) {
		t.Error("run was called: paster() did not restore the plain hooks")
		fn()
	})

	h := routed.paster()
	if h == nil {
		t.Fatal("paster() returned nil, want a handler (PasteUpload set)")
	}
	if got := h.procFor("%1"); got != "claude" {
		t.Errorf("procFor = %q, want %q", got, "claude")
	}
	if plainCalls != 1 {
		t.Errorf("plain LocalTmuxOut calls = %d, want 1", plainCalls)
	}
}
