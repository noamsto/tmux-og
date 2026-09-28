package daemon

import (
	"errors"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"
)

// scriptConn is one control-mode connection for reattach: it replays script on
// read, discards writes, and ends the stream on Close. A script that runs out
// leaves the read BLOCKED rather than at EOF, which is what makes "the endpoint
// accepted the connection and then never said anything" expressible — the case
// the identity deadline exists for.
type scriptConn struct {
	mu     sync.Mutex
	rest   []byte
	done   bool
	closed chan struct{}
}

func newScriptConn(script string) *scriptConn {
	// withBarriers: the wire carries a barrier block per command (#723).
	return &scriptConn{rest: []byte(withBarriers(script)), closed: make(chan struct{})}
}

func (c *scriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.rest) > 0 {
		n := copy(p, c.rest)
		c.rest = c.rest[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	<-c.closed
	return 0, io.EOF
}

func (c *scriptConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *scriptConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = true
		close(c.closed)
	}
	return nil
}

func (c *scriptConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// eofScriptConn is like scriptConn, except a spent script ends the read at
// once with io.EOF instead of blocking. It models a far end that said its
// piece and hung up — a refused attach's %exit, or plain silence — as opposed
// to scriptConn's "accepted the connection and then never spoke again",
// which is what the identity deadline exists to catch.
type eofScriptConn struct {
	mu     sync.Mutex
	rest   []byte
	done   bool
	closed chan struct{}
}

// newEOFScriptConn takes script RAW, with no withBarriers wrapping: a refused
// attach's leading block is unflagged (flags 0, the transport's own
// attach-session), and withBarriers only ever inserts a barrier behind a
// flagged block of ours, so it has nothing to do here and would only obscure
// what is actually on the wire.
func newEOFScriptConn(script string) *eofScriptConn {
	return &eofScriptConn{rest: []byte(script), closed: make(chan struct{})}
}

func (c *eofScriptConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func (c *eofScriptConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *eofScriptConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = true
		close(c.closed)
	}
	return nil
}

func (c *eofScriptConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// epipeConn models a far end that has already hung up its write side by the
// time we try to send to it: every Write fails with EPIPE, while Read still
// replays whatever the far end said before that, then io.EOF. This is the
// shape a refusal takes when the remote closes so fast our own send loses
// the race against its reply.
type epipeConn struct {
	mu     sync.Mutex
	rest   []byte
	done   bool
	closed chan struct{}
}

// newEPipeConn takes script RAW, for the same reason as newEOFScriptConn.
func newEPipeConn(script string) *epipeConn {
	return &epipeConn{rest: []byte(script), closed: make(chan struct{})}
}

func (c *epipeConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func (c *epipeConn) Write(p []byte) (int, error) { return 0, syscall.EPIPE }

func (c *epipeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = true
		close(c.closed)
	}
	return nil
}

func (c *epipeConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// refusedAttach is a fresh connection's very first reply block: an unflagged
// %error (flags 0 — the reply to the attach-session the transport ran, not
// to one of our commands) reporting the pinned session is gone, followed by
// %exit. Pass it RAW to eofScriptConn/epipeConn, never through withBarriers —
// it carries no flagged block for withBarriers to find a barrier behind.
const refusedAttach = "%begin 1 1 0\ncan't find session: A\n%error 1 1 0\n%exit\n"

// localTmuxRecorder records every LocalTmux argv, for asserting which
// @bridge_state stamps a reattach cycle does or does not make. argvKey
// (park_test.go) turns a recorded argv into a comparable key.
type localTmuxRecorder struct {
	mu   sync.Mutex
	argv [][]string
}

func (r *localTmuxRecorder) record(args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argv = append(r.argv, append([]string(nil), args...))
	return nil
}

func (r *localTmuxRecorder) calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.argv...)
}

// reattachCfg is a Config with just the fields reattach reads. LocalSess stays
// empty so the @bridge_state stamps are no-ops, and the schedule never sleeps.
func reattachCfg(dial func() (io.ReadWriteCloser, error), attempts int) Config {
	return Config{
		RemoteHost:    "h",
		RemoteSession: "A",
		Dial:          dial,
		// Every dial's argv reads View.Desired and every publish writes
		// View.Advertised, so reattach needs the cell even where a test does
		// not look at it.
		View: &Viewing{},
		Retry: &Backoff{
			MaxAttempts: attempts,
			Now:         time.Now,
			Jitter:      func() float64 { return 0 },
		},
	}
}

// identityMatch is the reply to a whole readIdentity batch: the new-layouts
// flag ack (newLayoutsFlagAck, see sessionpin_test.go) followed by a matching
// identity reply.
const identityMatch = newLayoutsFlagAck + "%begin 1 1 1\n2151|1788283304|$1\n%end 1 1 1\n"

// TestReattachDropsOutputFromAnUnverifiedConnection is the trust boundary: the
// identity round-trip runs readReplyRouting, which routes %output into
// registered sinks as it walks past reply blocks. Remote pane ids are small,
// sequential and entirely predictable, so a far end that is NOT the server the
// registry's ids belong to would otherwise paint arbitrary bytes into panes the
// user believes are their shells — before anything has checked who it is.
func TestReattachDropsOutputFromAnUnverifiedConnection(t *testing.T) {
	router := NewRouter()
	sink := &capBuf{}
	router.Register("%1", sink)

	conn := newScriptConn("%output %1 INTRUDER\n" + newLayoutsFlagAck +
		"%begin 1 1 1\n9999|1788283304|$1\n%end 1 1 1\n")
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) { return conn, nil }, 2)

	c, end := reattach(cfg, router, &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c != nil {
		t.Fatal("reattach onto a different tmux server should tear down")
	}
	if end != endReplaced {
		t.Errorf("end = %v, want endReplaced", end)
	}
	if sink.Len() != 0 {
		t.Errorf("an unverified connection painted %q into a live pane's sink", sink.String())
	}
}

// TestReattachBindsTheRouterOnlyAfterIdentityMatches is the other half: the
// quarantine must be lifted, and lifted over the SAME stream — output arriving
// after the identity matched has to reach the sink, on the connection whose
// ordinals the identity command already advanced.
func TestReattachBindsTheRouterOnlyAfterIdentityMatches(t *testing.T) {
	router := NewRouter()
	sink := &capBuf{}
	router.Register("%1", sink)

	conn := newScriptConn("%output %1 EARLY\n" + identityMatch +
		"%output %1 LATE\n" +
		"%begin 1 2 1\nok\n%end 1 2 1\n")
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) { return conn, nil }, 2)
	cfg.View.SetDesired("foot")

	c, _ := reattach(cfg, router, &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c == nil {
		t.Fatal("reattach onto the same server should return a connection")
	}
	if sink.Len() != 0 {
		t.Fatalf("output from before the identity check leaked: %q", sink.String())
	}
	// Ordinal 3 on the same stream (identityMatch's flag ack and identity reply
	// claim 1 and 2): a round-trip that answers proves the rebind reused this
	// connection's counters rather than restarting them.
	if _, ok := one(c.rt, "list-windows"); !ok {
		t.Fatal("round-trip on the rebound connection found no reply")
	}
	if got := sink.String(); got != "LATE" {
		t.Errorf("sink got %q, want %q", got, "LATE")
	}
	// Publishing the connection publishes the term it was dialled with:
	// Advertised left naming the old one has the next carousel press dial and
	// repair a client that already carries what it wants (#574).
	if got := cfg.View.Advertised(); got != "foot" {
		t.Errorf("Advertised = %q after a reattach dialled with foot, want %q", got, "foot")
	}
}

// TestReattachStopRaisedDuringTheDialTearsDown: SIGTERM arriving between the
// dial and the caller seeing its connection must not be overtaken by it — the
// loop's own stop check predates the dial. See reattach's post-dial check.
func TestReattachStopRaisedDuringTheDialTearsDown(t *testing.T) {
	stop := make(chan struct{})
	conn := newScriptConn(identityMatch)
	dials := 0
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		close(stop)
		return conn, nil
	}, 2)
	cfg.Shutdown = stop

	repaired := false
	hold := &connHolder{}
	c, _ := reattach(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool {
		repaired = true
		return true
	}, nil)
	if c != nil {
		t.Fatal("a stop raised during the dial should tear down, not reconnect")
	}
	if repaired {
		t.Error("repair ran after the user asked to detach")
	}
	if dials != 1 {
		t.Errorf("dials = %d, want 1 — a stop must not schedule another attempt", dials)
	}
	if hold.get() != nil {
		t.Error("a connection the stop discarded was published anyway")
	}
	if !conn.isClosed() {
		t.Error("reattach left the transport it dialled open")
	}
}

// TestReattachIdentityDeadlineRetriesRatherThanTearingDown: an endpoint that
// accepts the connection and never answers must consume retry attempts and then
// give up, not park reattach forever — the retry budget is the bound, and a read
// with no deadline of its own defeats it. Reaching the "gave up" return at all
// is the assertion; this test hangs against an unbounded read.
func TestReattachIdentityDeadlineRetriesRatherThanTearingDown(t *testing.T) {
	var conns []*scriptConn
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		c := newScriptConn("")
		conns = append(conns, c)
		return c, nil
	}, 2)
	cfg.IdentityTimeout = 20 * time.Millisecond

	c, _ := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c != nil {
		t.Fatal("a silent endpoint should never be reattached to")
	}
	if len(conns) != 2 {
		t.Fatalf("dials = %d, want 2 — a deadline is another drop, so it retries", len(conns))
	}
	for i, c := range conns {
		if !c.isClosed() {
			t.Errorf("dial %d: the deadline left its transport open", i+1)
		}
	}
}

// TestArmIdentityDeadlineDisarmReportsTheRace pins the guard the publish path
// depends on: once the deadline has closed the connection, disarm reports it
// dead even if the reply landed in the same instant, so reattach can never
// publish a connection the watchdog shut. And a disarm that wins bars the close
// outright, so a published connection is never closed behind reattach's back.
func TestArmIdentityDeadlineDisarmReportsTheRace(t *testing.T) {
	fired := newScriptConn("")
	disarm := armIdentityDeadline(newCtlConn(fired), time.Millisecond)
	<-fired.closed
	if disarm() {
		t.Error("disarm reported live after the deadline closed the connection")
	}

	beat := newScriptConn("")
	c := newCtlConn(beat)
	defer c.close()
	if !armIdentityDeadline(c, time.Hour)() {
		t.Error("disarm reported dead before the deadline could fire")
	}
	// The deadline is disarmed, not merely early: nothing closes it later.
	time.Sleep(10 * time.Millisecond)
	if beat.isClosed() {
		t.Error("a disarmed deadline closed the connection anyway")
	}
}

// parkCfg is reattachCfg with a wake schedule of its own and a dial that fails
// until conn (if any) is handed out on dial number connAt. dials counts every
// dial, failed or not.
func parkCfg(retry, wake int, dials *int, connAt int, conn func() io.ReadWriteCloser) Config {
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		*dials++
		if conn != nil && *dials == connAt {
			return conn(), nil
		}
		return nil, errors.New("unreachable")
	}, retry)
	cfg.WakeRetry = &Backoff{
		MaxAttempts: wake,
		Now:         time.Now,
		Jitter:      func() float64 { return 0 },
	}
	return cfg
}

func TestReattachExhaustionWithoutParkTearsDown(t *testing.T) {
	dials := 0
	cfg := parkCfg(3, 2, &dials, 0, nil)
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c != nil {
		t.Fatal("an exhausted schedule with no park should tear down")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
	if dials != 3 {
		t.Errorf("dials = %d, want 3", dials)
	}
}

// TestReattachExhaustionParksOnceAndStopsWhenParkDeclines: park answering
// parkStop is a stop or a gone session while parked — the teardown exhaustion
// used to be, and no further dial.
func TestReattachExhaustionParksOnceAndStopsWhenParkDeclines(t *testing.T) {
	dials, parks := 0, 0
	cfg := parkCfg(2, 3, &dials, 0, nil)
	park := func() parkVerdict {
		parks++
		return parkStop
	}
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("a declined park should tear down")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
	if parks != 1 {
		t.Errorf("parks = %d, want 1", parks)
	}
	if dials != 2 {
		t.Errorf("dials = %d, want 2 — a declined park must not dial again", dials)
	}
}

// TestReattachWakeRunsOneWakeRetryCycle: a wake buys exactly one cycle on
// WakeRetry, not a fresh Retry budget, and exhausting it parks again.
func TestReattachWakeRunsOneWakeRetryCycle(t *testing.T) {
	dials := 0
	var dialsAtPark []int
	cfg := parkCfg(2, 3, &dials, 0, nil)
	park := func() parkVerdict {
		dialsAtPark = append(dialsAtPark, dials)
		if len(dialsAtPark) < 2 {
			return parkWoken
		}
		return parkStop
	}
	c, _ := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("an unreachable remote should never be reattached to")
	}
	if len(dialsAtPark) != 2 || dialsAtPark[0] != 2 || dialsAtPark[1] != 5 {
		t.Errorf("dials at each park = %v, want [2 5] — Retry's 2, then WakeRetry's 3", dialsAtPark)
	}
}

func TestReattachWakeCycleConnects(t *testing.T) {
	dials, parks := 0, 0
	cfg := parkCfg(2, 3, &dials, 3, func() io.ReadWriteCloser { return newScriptConn(identityMatch) })
	park := func() parkVerdict {
		parks++
		return parkWoken
	}
	hold := &connHolder{}
	c, _ := reattach(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c == nil {
		t.Fatal("a wake cycle that reaches the same server should return its connection")
	}
	defer c.close()
	if hold.get() != c {
		t.Error("the woken connection was not published")
	}
	if parks != 1 || dials != 3 {
		t.Errorf("parks = %d, dials = %d, want 1 and 3", parks, dials)
	}
}

// TestReattachWakeCycleIdentityMismatchReplaces: a wake does not soften the
// correctness cliff — a different server on the far end ends the mirror as a
// replacement rather than parking again.
func TestReattachWakeCycleIdentityMismatchReplaces(t *testing.T) {
	dials, parks := 0, 0
	cfg := parkCfg(2, 3, &dials, 3, func() io.ReadWriteCloser {
		return newScriptConn(newLayoutsFlagAck + "%begin 1 1 1\n9999|1788283304|$1\n%end 1 1 1\n")
	})
	park := func() parkVerdict {
		parks++
		return parkWoken
	}
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("a mismatched identity on a wake cycle should tear down")
	}
	if end != endReplaced {
		t.Errorf("end = %v, want endReplaced", end)
	}
	if parks != 1 {
		t.Errorf("parks = %d, want 1 — a mismatch is terminal, not another park", parks)
	}
	if dials != 3 {
		t.Errorf("dials = %d, want 3", dials)
	}
}

// TestAttemptCycleRefusedAttachIsAVerdict pins the refusal shape itself: a
// fresh connection whose first reply block is an unflagged %error is the
// remote saying "no such session" to the attach the transport ran, not a drop
// mid-read — one dial is enough to know that, so a non-restoring cycle must
// not spend its retry budget rediscovering it.
func TestAttemptCycleRefusedAttachIsAVerdict(t *testing.T) {
	dials := 0
	var conn *eofScriptConn
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		conn = newEOFScriptConn(refusedAttach)
		return conn, nil
	}, 3)

	c, result := attemptCycle(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, cfg.retrySchedule(), false)
	if c != nil {
		t.Fatal("a refused attach should never yield a connection")
	}
	if result != cycleRefused {
		t.Errorf("result = %v, want cycleRefused", result)
	}
	if dials != 1 {
		t.Errorf("dials = %d, want 1 — a refusal is a verdict, not a retry", dials)
	}
	if !conn.isClosed() {
		t.Error("attemptCycle left the refused connection open")
	}
}

// TestAttemptCycleRefusalSeenAfterAFailedWrite: the remote can close its write
// side before our own send lands on it, so the refusal must be read off
// whatever Read already has buffered even though the write that would carry
// our commands fails outright.
func TestAttemptCycleRefusalSeenAfterAFailedWrite(t *testing.T) {
	dials := 0
	var conn *epipeConn
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		conn = newEPipeConn(refusedAttach)
		return conn, nil
	}, 3)

	c, result := attemptCycle(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, cfg.retrySchedule(), false)
	if c != nil {
		t.Fatal("a refused attach should never yield a connection")
	}
	if result != cycleRefused {
		t.Errorf("result = %v, want cycleRefused", result)
	}
	if dials != 1 {
		t.Errorf("dials = %d, want 1", dials)
	}
	if !conn.isClosed() {
		t.Error("attemptCycle left the refused connection open")
	}
}

// TestAttemptCycleLaterUnflaggedErrorIsNotARefusal: an unflagged %error that
// shows up after the attach itself already succeeded is an ordinary hook
// failure notification, not the attach's own refusal — only the FIRST reply
// block gets the special reading, so this still runs out its retry budget
// like any other unreachable remote.
func TestAttemptCycleLaterUnflaggedErrorIsNotARefusal(t *testing.T) {
	dials := 0
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		return newEOFScriptConn("%begin 1 1 0\n%end 1 1 0\n%begin 1 2 0\nhook failed\n%error 1 2 0\n"), nil
	}, 2)

	c, result := attemptCycle(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, cfg.retrySchedule(), false)
	if c != nil {
		t.Fatal("no attempt here ever answers our identity read")
	}
	if result != cycleExhausted {
		t.Errorf("result = %v, want cycleExhausted", result)
	}
	if dials != 2 {
		t.Errorf("dials = %d, want 2 — a later unflagged error must not short-circuit the retry budget", dials)
	}
}

// TestAttemptCycleNoControlOutputIsUnreachable: a connection that answers with
// nothing at all before EOF is the ordinary "unreachable" shape, not a
// refusal — there is no first reply block to read a verdict from.
func TestAttemptCycleNoControlOutputIsUnreachable(t *testing.T) {
	dials := 0
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		return newEOFScriptConn(""), nil
	}, 2)

	c, result := attemptCycle(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, cfg.retrySchedule(), false)
	if c != nil {
		t.Fatal("empty control output should never yield a connection")
	}
	if result != cycleExhausted {
		t.Errorf("result = %v, want cycleExhausted", result)
	}
	if dials != 2 {
		t.Errorf("dials = %d, want 2", dials)
	}
}

// restoreCfg is reattachCfg plus a zero-jitter RestoreRetry, for the tests
// that drive reattach across a refused attach's restore window.
func restoreCfg(dial func() (io.ReadWriteCloser, error), retry, restore int) Config {
	cfg := reattachCfg(dial, retry)
	cfg.RestoreRetry = &Backoff{
		MaxAttempts: restore,
		Now:         time.Now,
		Jitter:      func() float64 { return 0 },
	}
	return cfg
}

// TestReattachRestoreWindowEndsGoneWhenStillRefused: a session that never
// comes back exhausts the restore window on refusals alone — the mirror ends
// outright rather than dialling forever on a session that is not returning,
// and never parks over it (parking is for an unreachable HOST, not a
// confirmed-gone session).
func TestReattachRestoreWindowEndsGoneWhenStillRefused(t *testing.T) {
	dials := 0
	cfg := restoreCfg(func() (io.ReadWriteCloser, error) {
		dials++
		return newEOFScriptConn(refusedAttach), nil
	}, 3, 3)

	parks := 0
	park := func() parkVerdict {
		parks++
		return parkStop
	}
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("a session that never returns should never yield a connection")
	}
	if end != endGone {
		t.Errorf("end = %v, want endGone", end)
	}
	if dials != 1+3 {
		t.Errorf("dials = %d, want 4 (1 + RestoreRetry's 3)", dials)
	}
	if parks != 0 {
		t.Error("park ran during the restore window — a still-refused session ends outright")
	}
}

// TestReattachRestoreWindowReopensOnANewServer: a restart during the window
// can come back as a DIFFERENT server than the one that dropped — same
// session name, different pid — and that is a replacement, not a restore.
func TestReattachRestoreWindowReopensOnANewServer(t *testing.T) {
	dials := 0
	cfg := restoreCfg(func() (io.ReadWriteCloser, error) {
		dials++
		if dials == 1 {
			return newEOFScriptConn(refusedAttach), nil
		}
		return newScriptConn(newLayoutsFlagAck + "%begin 1 1 1\n9999|1788283304|$1\n%end 1 1 1\n"), nil
	}, 3, 3)

	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c != nil {
		t.Fatal("a mismatched identity should never yield a connection")
	}
	if end != endReplaced {
		t.Errorf("end = %v, want endReplaced", end)
	}
}

// TestReattachRestoreWindowReconnectsToTheSameServer: the case the window
// exists for — tmux-remux restores the same session moments after a restart,
// and the mirror should pick right back up on it.
func TestReattachRestoreWindowReconnectsToTheSameServer(t *testing.T) {
	dials := 0
	cfg := restoreCfg(func() (io.ReadWriteCloser, error) {
		dials++
		if dials == 1 {
			return newEOFScriptConn(refusedAttach), nil
		}
		return newScriptConn(identityMatch), nil
	}, 3, 3)

	repairs := 0
	hold := &connHolder{}
	c, _ := reattach(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { repairs++; return true }, nil)
	if c == nil {
		t.Fatal("a restore onto the same server should reconnect")
	}
	defer c.close()
	if hold.get() != c {
		t.Error("the restored connection was not published")
	}
	if repairs != 1 {
		t.Errorf("repairs = %d, want 1", repairs)
	}
}

// TestReattachRestoreWindowParksWhenTheHostVanishes: a refusal proves the
// remote answered once, but if it goes fully unreachable partway through the
// window that is the ordinary exhausted shape, not another refusal —
// restoring drops back to false and the mirror parks like any other
// exhausted schedule.
func TestReattachRestoreWindowParksWhenTheHostVanishes(t *testing.T) {
	dials := 0
	cfg := restoreCfg(func() (io.ReadWriteCloser, error) {
		dials++
		if dials == 1 {
			return newEOFScriptConn(refusedAttach), nil
		}
		return nil, errors.New("unreachable")
	}, 3, 3)

	parks := 0
	park := func() parkVerdict {
		parks++
		return parkStop
	}
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("a vanished host should never yield a connection")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
	if parks != 1 {
		t.Errorf("parks = %d, want 1", parks)
	}
}

// TestReattachProbeRunsOneAttemptAndKeepsTheBadge: parkProbe buys one silent
// dial with no @bridge_state churn — a probe that fails should leave the
// parked badge exactly as it was, not flicker it to "disconnected" and back.
func TestReattachProbeRunsOneAttemptAndKeepsTheBadge(t *testing.T) {
	dials := 0
	rec := &localTmuxRecorder{}
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		dials++
		return nil, errors.New("unreachable")
	}, 2)
	cfg.LocalSess = "m"
	cfg.LocalTmux = rec.record

	parks := 0
	// markerAt splits the recorded log at the first park call, so the
	// assertion below only looks at what happened during the probe cycle
	// that call bought, not the initial drop's own disconnected stamp.
	markerAt := -1
	park := func() parkVerdict {
		parks++
		if parks == 1 {
			markerAt = len(rec.calls())
			return parkProbe
		}
		return parkStop
	}
	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, park)
	if c != nil {
		t.Fatal("an unreachable remote should never yield a connection")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
	if dials != 2+1 {
		t.Errorf("dials = %d, want 3 (Retry's 2, then the probe's 1)", dials)
	}
	if parks != 2 {
		t.Errorf("parks = %d, want 2", parks)
	}
	disconnected := argvKey([]string{"set-option", "-t", "m", "@bridge_state", bridgeStateDisconnected})
	for _, argv := range rec.calls()[markerAt:] {
		if argvKey(argv) == disconnected {
			t.Errorf("a probe cycle stamped disconnected: %v", argv)
		}
	}
}

// TestReattachMalformedIdentityIsAPlainTeardown: a reply that arrives but
// fails to parse is neither a refusal nor a mismatch — the far end answered
// something readIdentity can't make sense of, and that stays the ordinary
// teardown it always was.
func TestReattachMalformedIdentityIsAPlainTeardown(t *testing.T) {
	cfg := reattachCfg(func() (io.ReadWriteCloser, error) {
		return newScriptConn(newLayoutsFlagAck + "%begin 1 1 1\ngarbage\n%end 1 1 1\n"), nil
	}, 2)

	c, end := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }, nil)
	if c != nil {
		t.Fatal("a malformed identity reply should never yield a connection")
	}
	if end != endTeardown {
		t.Errorf("end = %v, want endTeardown", end)
	}
}
