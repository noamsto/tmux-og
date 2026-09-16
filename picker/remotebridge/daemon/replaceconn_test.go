package daemon

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// eventLog orders the observable moments of a replacement — the priming batch's
// write, each transport's close, the first byte routed out of the new
// connection, repair. R7 is almost entirely an ordering contract, and nothing
// else in this package records order.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.ev = append(l.ev, e)
	l.mu.Unlock()
}

// find is the index of the FIRST event containing sub, or -1. First, because a
// transport can be closed more than once (the holder's close, then a test's
// cleanup) and only the first close is the one under test.
func (l *eventLog) find(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.ev {
		if strings.Contains(e, sub) {
			return i
		}
	}
	return -1
}

func (l *eventLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.ev, " | ")
}

// recordConn is scriptConn with its writes and its close logged. The original's
// Write is `return len(p), nil` and records nothing at all, so the write-order
// assertions need this sibling rather than the original.
type recordConn struct {
	*scriptConn
	log  *eventLog
	name string
}

func newRecordConn(log *eventLog, name, script string) *recordConn {
	return &recordConn{scriptConn: newScriptConn(script), log: log, name: name}
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.log.add("write:" + c.name + ":" + string(p))
	return c.scriptConn.Write(p)
}

func (c *recordConn) Close() error {
	c.log.add("close:" + c.name)
	return c.scriptConn.Close()
}

// logSink records the moment a byte routes out of a connection, which is the
// only externally visible consequence of bind.
type logSink struct{ log *eventLog }

func (s *logSink) Write(p []byte) (int, error) {
	s.log.add("sink:" + string(p))
	return len(p), nil
}

// replyBlocks is n client-flagged reply blocks. The reply reader pairs commands
// to blocks by counting the flagged ones, so the numbers inside the %begin
// lines are decoration.
func replyBlocks(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%%begin 1 %d 1\nok\n%%end 1 %d 1\n", i+2, i+2)
	}
	return b.String()
}

// primingCmds is what the priming batch sends: the client-size command plus one
// cap per mirrored window — replaceFixture mirrors one window, so two.
const primingCmds = 2

// replaceFixture is the state a carousel press with a new viewer finds: one
// bound, published connection over a transport that says nothing more, one
// mirrored window, and a view cell advertising xterm-kitty while wanting foot.
type replaceFixture struct {
	log     *eventLog
	router  *Router
	hold    *connHolder
	reg     *registry
	view    *Viewing
	old     *ctlConn
	oldConn *recordConn
	want    remoteIdentity
}

func newReplaceFixture(t *testing.T) *replaceFixture {
	t.Helper()
	log := &eventLog{}
	router := NewRouter()
	router.Register("%1", &logSink{log: log})
	oldConn := newRecordConn(log, "old", "")
	old := newCtlConn(oldConn)
	old.bind(router)
	hold := &connHolder{}
	hold.set(old)
	// Whatever is published when the test ends, so no pump outlives it.
	t.Cleanup(hold.close)

	reg := newRegistry()
	reg.add("@1", "@10")

	view := &Viewing{}
	view.Seed("xterm-kitty")
	view.SetDesired("foot")

	return &replaceFixture{
		log:     log,
		router:  router,
		hold:    hold,
		reg:     reg,
		view:    view,
		old:     old,
		oldConn: oldConn,
		want:    mustIdentity(t, "A", "2151|1788283304|$1"),
	}
}

// cfg is a Config with just the fields replaceConn reads: the dial, the view
// cell whose Desired the dial argv reads, and the content area the priming
// batch asserts.
func (f *replaceFixture) cfg(dial func() (io.ReadWriteCloser, error)) Config {
	return Config{
		RemoteHost:    "h",
		RemoteSession: "A",
		Dial:          dial,
		View:          f.view,
		LocalArea:     func() (int, int) { return 100, 40 },
	}
}

func (f *replaceFixture) dialing(c io.ReadWriteCloser) func() (io.ReadWriteCloser, error) {
	return func() (io.ReadWriteCloser, error) { return c, nil }
}

// TestReplaceConnSwapsAndPublishesTheDialledTerm is the whole happy path: the
// new connection becomes the mirror's, repair runs on it, the old transport is
// gone, and Advertised names what this dial actually carried — which is what
// makes the next press with the same viewer a no-op instead of a second dial.
func TestReplaceConnSwapsAndPublishesTheDialledTerm(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", identityMatch+replyBlocks(primingCmds))
	cfg := f.cfg(f.dialing(next))

	repairs := 0
	c, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool {
		repairs++
		f.log.add("repair")
		return true
	})
	if outcome != replaced {
		t.Fatalf("outcome = %v, want replaced (%v)", outcome, replaced)
	}
	if c == nil || c.rwc != io.ReadWriteCloser(next) {
		t.Fatalf("replaceConn returned %v, want the connection it dialled", c)
	}
	if f.hold.get() != c {
		t.Error("the returned connection is not the published one")
	}
	if repairs != 1 {
		t.Errorf("repair ran %d times, want 1", repairs)
	}
	if got := f.view.Advertised(); got != "foot" {
		t.Errorf("Advertised = %q, want %q", got, "foot")
	}
	if !f.oldConn.isClosed() {
		t.Error("the old transport outlived the swap")
	}
}

// TestReplaceConnPrimesTheNewClientBeforeClosingTheOld: the client size and
// each window's cap are per-client state the new client has never been told, so
// a window the remote creates between the close and repair()'s own sends would
// be born at tmux's 80x23 control-client default (#449). Asserted as ordering,
// not as a resize — an existing window does not shrink when a client goes away.
func TestReplaceConnPrimesTheNewClientBeforeClosingTheOld(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", identityMatch+replyBlocks(primingCmds))
	cfg := f.cfg(f.dialing(next))

	if _, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool { return true }); outcome != replaced {
		t.Fatalf("outcome = %v, want replaced", outcome)
	}
	size := f.log.find("write:new:refresh-client -C 100x40")
	capped := f.log.find("refresh-client -C @1:100x40")
	closed := f.log.find("close:old")
	if size < 0 || capped < 0 {
		t.Fatalf("the priming batch never reached the new connection: %s", f.log)
	}
	if closed < 0 {
		t.Fatalf("the old connection was never closed: %s", f.log)
	}
	if size > closed || capped > closed {
		t.Errorf("priming landed after the old connection closed: %s", f.log)
	}
}

// TestReplaceConnClosesTheOldBeforeTheNewRoutes is the other half of the
// ordering: both connections stream the same remote panes, so a window in which
// both are bound routes duplicate %output into one sink. The first byte to
// reach a sink from the new connection therefore has to follow the old
// connection's close.
func TestReplaceConnClosesTheOldBeforeTheNewRoutes(t *testing.T) {
	f := newReplaceFixture(t)
	// UNBOUND is walked past by the priming round-trip, which runs before the
	// old connection is closed — the empty router of an unbound connection must
	// drop it. BOUND sits behind the priming replies, so nothing routes it until
	// repair's own round-trip walks past it, on the published connection.
	next := newRecordConn(f.log, "new", identityMatch+
		"%begin 1 2 1\nok\n%end 1 2 1\n"+
		"%output %1 UNBOUND\n"+
		"%begin 1 3 1\nok\n%end 1 3 1\n"+
		"%output %1 BOUND\n"+
		"%begin 1 9 1\nok\n%end 1 9 1\n")
	cfg := f.cfg(f.dialing(next))

	if _, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool {
		if _, ok := one(f.hold.roundTrip, "list-windows"); !ok {
			t.Error("round-trip on the published connection found no reply")
		}
		return true
	}); outcome != replaced {
		t.Fatalf("outcome = %v, want replaced", outcome)
	}
	if f.log.find("sink:UNBOUND") >= 0 {
		t.Errorf("the new connection painted into a live sink while still unbound: %s", f.log)
	}
	routed := f.log.find("sink:BOUND")
	if routed < 0 {
		t.Fatalf("the new connection never routed into the sink: %s", f.log)
	}
	if closed := f.log.find("close:old"); closed > routed {
		t.Errorf("the new connection routed while the old was still bound: %s", f.log)
	}
}

// TestReplaceConnDialFailureLeavesTheMirrorAlone: the gesture is a nicety, so a
// dial that fails must cost nothing. The old connection was never closed, is
// still published, and Advertised still names its term — so the next press
// raises again rather than comparing equal to a client that never existed.
func TestReplaceConnDialFailureLeavesTheMirrorAlone(t *testing.T) {
	f := newReplaceFixture(t)
	cfg := f.cfg(func() (io.ReadWriteCloser, error) { return nil, errors.New("ssh: no route to host") })

	repaired := false
	c, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool {
		repaired = true
		return true
	})
	if outcome != notReplaced {
		t.Fatalf("outcome = %v, want notReplaced", outcome)
	}
	if c != nil {
		t.Error("an abandoned replacement handed back a connection")
	}
	if f.hold.get() != f.old {
		t.Error("the original connection is no longer published")
	}
	if f.oldConn.isClosed() {
		t.Error("a failed dial closed the working connection")
	}
	if got := f.view.Advertised(); got != "xterm-kitty" {
		t.Errorf("Advertised = %q after an abandoned replacement, want it untouched (%q)", got, "xterm-kitty")
	}
	if repaired {
		t.Error("repair ran for a replacement that never happened")
	}
}

// TestReplaceConnIdentityMismatchDoesNotTearTheMirrorDown is where this routine
// parts company with reattach: a fresh dial reaching a different tmux server is
// fatal to the ATTEMPT only. The verified connection still in hold is the better
// evidence about which server the mirror is on, and a remote that really has
// been replaced drops that stream too — where reattach makes the call.
func TestReplaceConnIdentityMismatchDoesNotTearTheMirrorDown(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", newLayoutsFlagAck+"%begin 1 1 1\n9999|1788283304|$1\n%end 1 1 1\n")
	cfg := f.cfg(f.dialing(next))

	c, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool { return true })
	if outcome != notReplaced {
		t.Fatalf("outcome = %v, want notReplaced — a mismatch must never reach teardown", outcome)
	}
	if c != nil {
		t.Error("a mismatched replacement handed back a connection")
	}
	if f.hold.get() != f.old {
		t.Error("the original connection is no longer published")
	}
	if f.oldConn.isClosed() {
		t.Error("a mismatched dial closed the working connection")
	}
	if !next.isClosed() {
		t.Error("the rejected connection was left open")
	}
	if got := f.view.Advertised(); got != "xterm-kitty" {
		t.Errorf("Advertised = %q after a rejected replacement, want it untouched (%q)", got, "xterm-kitty")
	}
}

// TestReplaceConnEmptyRegistryReportsMirrorGone: the swap landed, so the caller
// must NOT carry on over the connection this routine already closed. Collapsing
// this into notReplaced sends the loop down the involuntary drop path, whose
// first act is hold.close() — killing the working new connection.
func TestReplaceConnEmptyRegistryReportsMirrorGone(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", identityMatch+replyBlocks(primingCmds))
	cfg := f.cfg(f.dialing(next))

	c, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool { return false })
	if outcome != mirrorGone {
		t.Fatalf("outcome = %v, want mirrorGone", outcome)
	}
	if c != nil {
		t.Error("a mirror that is gone handed a connection back to the loop")
	}
	if !f.oldConn.isClosed() {
		t.Error("the old transport outlived the swap")
	}
	// Published, so the caller's teardown closes the connection it swapped in
	// rather than leaving the transport behind.
	if h := f.hold.get(); h == nil || h.rwc != io.ReadWriteCloser(next) {
		t.Error("the swapped-in connection is not the published one")
	}
}

// TestReplaceConnAdvertisesThePreDialSnapshot: cfg.Dial builds the ssh argv
// outside this package, so what it read is unknowable afterwards — Advertised
// must record the value taken BEFORE the dial. A viewer switch landing inside
// the dial window is off by one and corrected by the next change; recorded at
// the publish site instead it becomes a lie the raise guard reads as equal
// forever, and the user's next press paints tofu with no recovery.
func TestReplaceConnAdvertisesThePreDialSnapshot(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", identityMatch+replyBlocks(primingCmds))
	cfg := f.cfg(func() (io.ReadWriteCloser, error) {
		// A third terminal takes over while ssh is still connecting.
		f.view.SetDesired("xterm-256color")
		return next, nil
	})

	if _, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool { return true }); outcome != replaced {
		t.Fatalf("outcome = %v, want replaced", outcome)
	}
	if got := f.view.Advertised(); got != "foot" {
		t.Errorf("Advertised = %q, want the pre-dial %q — this client carries foot, not the switch that raced it", got, "foot")
	}
	if got := f.view.Desired(); got != "xterm-256color" {
		t.Errorf("Desired = %q, want %q — the publish must not move it", got, "xterm-256color")
	}
}
