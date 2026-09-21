package daemon

import (
	"io"
	"sync"
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
	// withBarriers for the same reason newTestReader applies it: the wire now
	// carries a barrier block per command (#723), and a fixture scripting only
	// its own replies would leave every round-trip after the first waiting on an
	// ordinal that never arrives.
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

	if c := reattach(cfg, router, &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }); c != nil {
		t.Fatal("reattach onto a different tmux server should tear down")
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

	c := reattach(cfg, router, &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true })
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
	if c := reattach(cfg, NewRouter(), hold, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool {
		repaired = true
		return true
	}); c != nil {
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

	if c := reattach(cfg, NewRouter(), &connHolder{}, mustIdentity(t, "A", "2151|1788283304|$1"), func() bool { return true }); c != nil {
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
