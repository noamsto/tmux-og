package daemon

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowScriptConn replays scriptConn's wire script one control-mode block at a
// time, pausing delay between blocks. It models a healthy but slow link: every
// reply does arrive, just late.
type slowScriptConn struct {
	mu     sync.Mutex
	blocks []string
	delay  time.Duration
	closed chan struct{}
	once   sync.Once
}

// newSlowScriptConn splits script at each %begin and releases those blocks in
// order, delay apart.
func newSlowScriptConn(script string, delay time.Duration) *slowScriptConn {
	parts := strings.Split(script, "%begin")
	blocks := make([]string, 0, len(parts)-1)
	for _, p := range parts[1:] {
		blocks = append(blocks, "%begin"+p)
	}
	return &slowScriptConn{blocks: blocks, delay: delay, closed: make(chan struct{})}
}

func (c *slowScriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.blocks) == 0 {
		c.mu.Unlock()
		<-c.closed
		return 0, io.EOF
	}
	blk := c.blocks[0]
	c.blocks = c.blocks[1:]
	c.mu.Unlock()

	select {
	case <-time.After(c.delay):
	case <-c.closed:
		return 0, io.EOF
	}
	n := copy(p, blk)
	if n < len(blk) {
		c.mu.Lock()
		c.blocks = append([]string{blk[n:]}, c.blocks...)
		c.mu.Unlock()
	}
	return n, nil
}

func (c *slowScriptConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *slowScriptConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// blockingWriteConn is a transport that accepts the connection but never
// drains it: Write blocks until Close.
type blockingWriteConn struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingWriteConn() *blockingWriteConn {
	return &blockingWriteConn{closed: make(chan struct{})}
}

func (c *blockingWriteConn) Read(p []byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *blockingWriteConn) Write(p []byte) (int, error) {
	<-c.closed
	return 0, io.ErrClosedPipe
}

func (c *blockingWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// boundConn builds a bound control connection over conn with the given
// deadlines, the way dialConn + bind do in production.
func boundConn(t *testing.T, conn io.ReadWriteCloser, reply, seed time.Duration) *ctlConn {
	t.Helper()
	c, err := dialConn(Config{Ctl: conn, ReplyTimeout: reply, SeedTimeout: seed})
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	c.bind(NewRouter())
	return c
}

// A far end that accepts the connection and never answers must not park the
// caller: the reply deadline closes the connection, so the round trip fails and
// every later send fails closed. Nothing is ever read from the poisoned
// connection afterwards, which is what keeps a late reply from being matched to
// the wrong request (#900).
func TestReplyDeadlineAgainstAStalledConnection(t *testing.T) {
	conn := newScriptConn("") // spent script leaves Read blocked, not at EOF
	c := boundConn(t, conn, 50*time.Millisecond, time.Second)
	defer c.close()

	start := time.Now()
	if _, ok := one(c.rt, "display-message -p x"); ok {
		t.Fatal("round trip on a stalled connection returned a reply")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("round trip took %v to fail, want the 50ms deadline", elapsed)
	}
	if c.live() || !c.st.isClosed() || !conn.isClosed() {
		t.Error("a fired reply deadline must close the connection and its transport")
	}
	second := time.Now()
	if _, ok := one(c.rt, "display-message -p y"); ok {
		t.Error("a round trip on a poisoned connection returned a reply")
	}
	if elapsed := time.Since(second); elapsed > 100*time.Millisecond {
		t.Errorf("poisoned round trip took %v, want an immediate fail-closed", elapsed)
	}
}

// The class split is deterministic on the command, and the two classes are
// really armed: the same late reply fails the ordinary deadline and passes the
// capture-pane one.
func TestReplyDeadlineClassSplit(t *testing.T) {
	st := newStream(io.Discard)
	st.replyTimeout = 30 * time.Second
	st.seedTimeout = 2 * time.Minute
	if got := st.replyDeadline("display-message -p x"); got != 30*time.Second {
		t.Errorf("display-message deadline = %v, want the reply timeout", got)
	}
	if got := st.replyDeadline("capture-pane -e -p -t %1"); got != 2*time.Minute {
		t.Errorf("capture-pane deadline = %v, want the seed timeout", got)
	}

	const reply = "%begin 1 1 1\nx\n%end 1 1 1\n"

	// A display-message reply that arrives after the ordinary deadline fails.
	late := newSlowScriptConn(withBarriers(reply), 3*time.Second)
	c := boundConn(t, late, 200*time.Millisecond, 10*time.Second)
	start := time.Now()
	if _, ok := one(c.rt, "display-message -p x"); ok {
		t.Error("a display-message reply past its deadline was accepted")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ordinary deadline took %v to fire, want the 200ms clause", elapsed)
	}
	c.close()

	// The same delay on a capture-pane reply passes the seed deadline.
	slow := newSlowScriptConn(withBarriers(reply), 3*time.Second)
	c = boundConn(t, slow, 200*time.Millisecond, 10*time.Second)
	defer c.close()
	if _, ok := one(c.rt, "capture-pane -e -p -t %1"); !ok {
		t.Error("a capture-pane reply inside its seed deadline was rejected")
	}
}

// A fired deadline must never deliver the terminal End that controlmode
// synthesizes for a block left open at EOF: it is not the pending command's
// reply, and handing it to the waiter would apply a foreign body to the very
// request the deadline exists to abandon.
func TestReplyDeadlineDiscardsAnEOFBlock(t *testing.T) {
	// A top-level %begin opens a block; the spent script then blocks, and the
	// deadline's close makes the reader resolve the open block at EOF.
	conn := newScriptConn("%begin 1 2 1\n")
	c := boundConn(t, conn, 50*time.Millisecond, time.Second)
	defer c.close()

	if _, ok := one(c.rt, "display-message -p x"); ok {
		t.Fatal("a deadline-closed block was delivered as the round trip's reply")
	}
	if !c.st.stalledOut() || !c.st.isClosed() {
		t.Error("the connection must be marked stalled and closed")
	}
}

// A flush that cannot complete is the same freeze as a reply that never
// arrives, so the write carries the ordinary deadline too.
func TestWriteDeadlineClosesAStalledTransport(t *testing.T) {
	conn := newBlockingWriteConn()
	c := boundConn(t, conn, 50*time.Millisecond, time.Second)
	defer c.close()

	start := time.Now()
	if c.st.send("display-message -p x") {
		t.Error("send to a stalled transport reported success")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("stalled write took %v to fail, want the 50ms deadline", elapsed)
	}
	if c.live() {
		t.Error("a fired write deadline must close the connection")
	}
}

// repair() returning true on a connection that died under it must not read as
// connected: reattach builds a fresh schedule per call, so returning here would
// reset the retry budget on every drop and a remote that wedges each newly
// attached client would dial forever instead of walking to exhaustion and park.
func TestRepairPoisonDoesNotReportConnected(t *testing.T) {
	var dials int
	dial := func() (io.ReadWriteCloser, error) {
		dials++
		// Identity answers, then the connection never says another word.
		return newScriptConn(identityMatch), nil
	}
	cfg := reattachCfg(dial, 3)
	cfg.ReplyTimeout = 50 * time.Millisecond
	hold := &connHolder{}
	parks := 0
	repair := func() bool {
		// The real repair logs a failed round trip and carries on; mimic that
		// and report the mirror standing.
		_, _ = one(hold.roundTrip, "display-message -p x")
		return true
	}
	park := func() parkVerdict { parks++; return parkStop }

	c, end := reattach(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), repair, park)
	if c != nil {
		t.Fatal("reattach returned a poisoned connection")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
	if dials != 3 {
		t.Errorf("dialed %d times, want the schedule's 3 attempts", dials)
	}
	if parks != 1 {
		t.Errorf("parked %d times, want exactly once after exhaustion", parks)
	}
}

// A replacement whose priming stalls must keep the current backend: every other
// replacement failure keeps it, and this one must not cost a mirror.
func TestReplacePrimeTimeoutKeepsTheOldConnection(t *testing.T) {
	old := newCtlConn(newScriptConn(""))
	hold := &connHolder{}
	hold.set(old)

	var replacement *scriptConn
	dial := func() (io.ReadWriteCloser, error) {
		replacement = newScriptConn(identityMatch) // identity answers, nothing else
		return replacement, nil
	}
	repairCalled := false
	repair := func() bool { repairCalled = true; return true }
	cfg := Config{
		RemoteHost:    "h",
		RemoteSession: "A",
		Dial:          dial,
		View:          &Viewing{},
		LocalArea:     func() (int, int) { return 80, 24 },
		ReplyTimeout:  50 * time.Millisecond,
	}

	next, outcome := replaceConn(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), newRegistry(), repair)
	if next != nil || outcome != notReplaced {
		t.Fatalf("replaceConn = (%v, %v), want (nil, notReplaced)", next, outcome)
	}
	if repairCalled {
		t.Error("repair ran on a replacement that never finished priming")
	}
	if replacement == nil || !replacement.isClosed() {
		t.Error("the failed replacement was not closed")
	}
	if hold.get() != old || old.dead.Load() {
		t.Error("the current connection was dropped by a failed priming")
	}
}

// An unset LocalArea is not a priming failure: the old code no-opped past it,
// and the termname replacement must still land (#574).
func TestReplaceZeroLocalAreaStillReplaces(t *testing.T) {
	old := newCtlConn(newScriptConn(""))
	hold := &connHolder{}
	hold.set(old)

	dial := func() (io.ReadWriteCloser, error) { return newScriptConn(identityMatch), nil }
	repairCalled := false
	repair := func() bool { repairCalled = true; return true }
	cfg := Config{
		RemoteHost:    "h",
		RemoteSession: "A",
		Dial:          dial,
		View:          &Viewing{},
		LocalArea:     func() (int, int) { return 0, 0 },
		ReplyTimeout:  50 * time.Millisecond,
	}

	next, outcome := replaceConn(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), newRegistry(), repair)
	if outcome != replaced || next == nil {
		t.Fatalf("replaceConn = (%v, %v), want a replaced connection", next, outcome)
	}
	if !repairCalled {
		t.Error("repair did not run for a zero-area replacement")
	}
	if !next.live() {
		t.Error("the replacement connection is not live")
	}
}
