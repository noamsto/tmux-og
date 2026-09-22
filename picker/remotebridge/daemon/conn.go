package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// ctlConn is everything scoped to one control connection: the transport itself
// and the pump, stream, async queue and round-tripper built over it. A new
// stream restarts its ordinals at 0 and tmux restarts the command/reply
// correspondence at 1 on a fresh attach, so these are only ever replaced as
// one unit — a half-swapped set desyncs every round-trip (#482).
type ctlConn struct {
	rwc   io.ReadWriteCloser
	pump  *ctlPump
	st    *stream
	async *asyncQueue
	rt    roundTrip
}

// newCtlConn builds a connection whose output goes nowhere. readReplyRouting
// routes %output into registered sinks as it walks past reply blocks, so a far
// end that has not yet proved it is the server whose pane ids the registry
// holds must round-trip against a Router with nothing in it: Route finds no
// sink and drops. Remote pane ids are small and sequential, so an unverified
// far end could otherwise paint into panes the user believes are their shells.
// bind opens the connection onto the real router once, after the identity read.
func newCtlConn(rwc io.ReadWriteCloser) *ctlConn {
	c := &ctlConn{
		rwc:   rwc,
		pump:  startCtlPump(controlmode.NewReader(rwc)),
		st:    newStream(rwc),
		async: &asyncQueue{},
	}
	c.rt = newRoundTrip(c.pump, NewRouter(), c.async, c.st)
	return c
}

// bind re-points this connection's round-tripper at the mirror's real router,
// over the SAME pump, stream and async queue — a second ctlConn would restart
// the stream's ordinals and tmux's command/reply correspondence mid-connection.
// Output dropped before this needs no repair of its own: every caller reseeds
// each pane straight afterwards. Notifications the verification round-trip
// queued are on this connection's own async queue, so they reach the main loop
// on the bind path and are discarded with the connection when there is none.
//
// Called on the main-loop goroutine before the connection is published, so rt —
// read by every later round-trip through connHolder — is never written while
// another goroutine can reach it.
func (c *ctlConn) bind(router *Router) {
	c.rt = newRoundTrip(c.pump, router, c.async, c.st)
}

// close ends this connection. The stream goes first, so every later send fails
// closed deterministically rather than waiting for some write to hit EPIPE and
// latch closed inside stampAll's flush; then the transport, whose Close must
// unpark a reader blocked on it — see cmd/daemon's child.Close, since closing
// only the write half leaves a silent far end holding the pump forever.
func (c *ctlConn) close() {
	c.st.close()
	c.rwc.Close()
}

// connHolder is the one indirection between the daemon's long-lived goroutines
// and the connection of the moment. Each renderer's input pump, the resize
// watcher and the ctl accept loop are started once and outlive the connection
// they were started on, so they reach it through here rather than capturing it.
//
// An empty slot is a normal state, not a bug: it is what the holder is between
// a drop and the next successful dial, including the whole of a parked wait.
type connHolder struct {
	mu sync.Mutex
	c  *ctlConn
}

func (h *connHolder) set(c *ctlConn) {
	h.mu.Lock()
	h.c = c
	h.mu.Unlock()
}

func (h *connHolder) get() *ctlConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.c
}

// close ends the current connection and empties the slot. Idempotent, because
// both the drop path and teardown call it, and because a teardown after a
// failed reattach finds nothing left in the slot to close.
func (h *connHolder) close() {
	h.mu.Lock()
	c := h.c
	h.c = nil
	h.mu.Unlock()
	if c != nil {
		c.close()
	}
}

// send writes cmd on whichever connection is current and reports whether it was
// written. With no connection it fails closed — the same answer stampAll gives
// on a closed stream, which every caller already handles (a ctl request is
// nacked, a keystroke is dropped). It must never block: a renderer's input pump
// waiting out a reconnect would wedge the pane it serves.
func (h *connHolder) send(cmds ...string) bool {
	c := h.get()
	if c == nil {
		return false
	}
	return c.st.send(cmds...)
}

// roundTrip is the stable roundTrip the mirror paths hold. With no connection
// it yields a drained batch, which is exactly what a caller sees when the
// stream dies mid-batch.
func (h *connHolder) roundTrip(cmds ...string) replies {
	c := h.get()
	if c == nil {
		return func() (controlmode.Line, bool) { return controlmode.Line{}, false }
	}
	return c.rt(cmds...)
}

// dialConn opens the next control connection, unverified. Dial is the
// re-dialable form and, when set, the only source of connections; Ctl is the
// single already-opened one a caller that cannot make another supplies (M1, the
// scripted Go tests), and Run consumes it exactly once.
func dialConn(cfg Config) (*ctlConn, error) {
	if cfg.Dial == nil {
		if cfg.Ctl == nil {
			return nil, fmt.Errorf("daemon: Config needs one of Ctl or Dial")
		}
		return newCtlConn(cfg.Ctl), nil
	}
	rwc, err := cfg.Dial()
	if err != nil {
		return nil, err
	}
	return newCtlConn(rwc), nil
}

// armIdentityDeadline closes c unless the returned disarm is called within d.
//
// one(rt, …) has no deadline of its own, so an endpoint that accepts the
// connection and then never answers parks the identity read forever: the retry
// budget is only consulted between attempts, so it never advances, and a detach
// never lands either. See defaultIdentityTimeout for the case ssh's own
// keepalives cannot catch. Closing the connection is the whole mechanism — the
// pump then hits EOF and readIdentity returns its existing "connection closed
// before reply" retry shape — so this adds no second reply reader, which would
// take an ordinal of its own and desync the stream.
//
// disarm reports whether THIS watchdog has closed the connection — not whether
// it is live, which nothing here tracks. It is false once the deadline has
// closed it, including when the reply landed in the same instant, so a caller
// can never publish a connection the watchdog has shut. Both sides meet under
// one mutex, so the close happens at most once and never after disarm returned
// true. A connection closed by something else in the meantime — a SIGTERM
// reaching the transport during the first identity read — still disarms true;
// the round-trip that follows then fails on its own, which is the answer the
// caller acts on either way.
func armIdentityDeadline(c *ctlConn, d time.Duration) (disarm func() (live bool)) {
	var (
		mu     sync.Mutex
		fired  bool
		beaten bool
	)
	t := time.AfterFunc(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if beaten {
			return
		}
		fired = true
		c.close()
	})
	return func() bool {
		t.Stop()
		mu.Lock()
		defer mu.Unlock()
		beaten = true
		return !fired
	}
}

// reattach re-dials after a drop and returns the connection the mirror is live
// on again, bound to router and published in hold, or nil once the daemon
// should tear down. want is the identity recorded at the first attach; repair
// brings the mirror back to remote ground truth and reports whether it still
// stands.
//
// An exhausted schedule is not an ending in itself: park, when non-nil, holds
// the mirror until the user comes back to it and reports whether to try again,
// which buys one short wakeSchedule cycle rather than the full retry budget. A
// nil park, or one that answers false (a stop, the local session gone), tears
// down.
//
// Package-level rather than a closure over Run's locals so the endings it has
// to tell apart — a drop that retries, a different server that tears down, a
// detach raised mid-dial — are reachable from a test without a live mirror.
func reattach(cfg Config, router *Router, hold *connHolder, want remoteIdentity, repair func() bool, park func() bool) *ctlConn {
	// Every send fails closed from this instant, rather than from whenever a
	// write happens to hit EPIPE.
	hold.close()
	// Stamped before the first dial, so the badge appears within one status
	// tick of the drop rather than after the backoff.
	setBridgeState(cfg, bridgeStateDisconnected)
	// Figures measured over a link that is gone describe nothing live. Dropped
	// at the source, so the picker's freshness rule gets its "not disconnected"
	// condition from the absent stamp rather than from reading a second option.
	clearBridgeRes(cfg)
	bo := cfg.retrySchedule()
	for {
		conn, result := attemptCycle(cfg, router, hold, want, repair, bo)
		switch result {
		case cycleConnected:
			return conn
		case cycleTerminal:
			return nil
		}
		if park == nil || !park() {
			return nil
		}
		// park stamped parked; the wake cycle is a re-dial pending like any
		// other, so the badge goes back to the one that says so.
		setBridgeState(cfg, bridgeStateDisconnected)
		bo = cfg.wakeSchedule()
	}
}

// cycleResult is how one attemptCycle ended.
type cycleResult int

const (
	// cycleConnected — a dial matched identity and repair() kept the mirror.
	cycleConnected cycleResult = iota
	// cycleTerminal — the daemon tears down: a stop, a different or malformed
	// identity, or a repair that emptied the registry.
	cycleTerminal
	// cycleExhausted — bo ran out with the remote still unreachable. The only
	// result a further cycle can change.
	cycleExhausted
)

// attemptCycle runs reattach's dial/verify/repair attempts on one schedule.
// start is taken per cycle, so a wake cycle's MaxElapsed is measured from the
// wake rather than from the original drop.
func attemptCycle(cfg Config, router *Router, hold *connHolder, want remoteIdentity, repair func() bool, bo Backoff) (*ctlConn, cycleResult) {
	start := bo.Now()
	for attempt := 1; ; attempt++ {
		// SIGTERM works by dropping the transport, so only the stop signal
		// tells a detach from a link failure (see Config.Shutdown). Consulted
		// before any retry is scheduled.
		if stopped(cfg.Shutdown) {
			return nil, cycleTerminal
		}
		d, ok := bo.Next(attempt, start)
		if !ok {
			fmt.Fprintf(os.Stderr, "daemon: %s still unreachable after %d reconnect attempt(s)\n", cfg.RemoteHost, attempt-1)
			return nil, cycleExhausted
		}
		if Wait(d, cfg.Shutdown) {
			return nil, cycleTerminal
		}
		// Snapshotted per attempt, immediately before the dial whose argv reads
		// it, so a retry that dials later records what IT dialled. Published
		// with the connection below, or this path leaves Advertised naming a
		// term the live client does not carry: a reconnect following a terminal
		// switch dials the new one, and the next carousel press would then pay a
		// whole redundant dial and repair() to change nothing.
		term := cfg.View.Desired()
		next, err := dialConn(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: re-dial %s: %v\n", cfg.RemoteHost, err)
			continue
		}
		// A detach raised while the dial was in flight must not be overtaken by
		// it: the check at the top of the iteration predates the dial, and
		// without this one Run re-enters runConn on a connection the user has
		// already asked to go away. og-remote-detach then waits out its 2s,
		// kills the mirror session itself, and leaves this daemon holding a live
		// transport child.
		if stopped(cfg.Shutdown) {
			next.close()
			return nil, cycleTerminal
		}
		// The identity read comes before anything touches the mirror: this
		// connection is still unbound (see newCtlConn), so a far end that is not
		// the server whose pane ids the registry holds cannot paint into panes
		// the user believes are their shells, and an unpublished connection
		// cannot carry a renderer's keystroke there either.
		disarm := armIdentityDeadline(next, cfg.identityTimeout())
		id, err := readIdentity(next.rt, cfg.RemoteSession)
		live := disarm()
		if err != nil {
			if live {
				next.close()
			}
			var ire *identityReadErr
			if errors.As(err, &ire) && ire.Retry() {
				// The new connection died before answering — another drop,
				// not a verdict on the remote. A deadline that expired lands
				// here too, by closing the connection out from under the read.
				fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "daemon: %v; tearing the mirror down\n", err)
			return nil, cycleTerminal
		}
		if !live {
			// The reply parsed, but only after the deadline had already closed
			// the connection under it. Same shape as any other drop.
			fmt.Fprintf(os.Stderr, "daemon: identity read for %s answered after its deadline\n", cfg.RemoteSession)
			continue
		}
		if !want.matches(id) {
			fmt.Fprintf(os.Stderr, "daemon: %s now hosts %s on a different tmux server (was pid %d %s, now pid %d %s); tearing the mirror down\n",
				cfg.RemoteHost, cfg.RemoteSession, want.pid, want.sessionID, id.pid, id.sessionID)
			next.close()
			return nil, cycleTerminal
		}
		next.bind(router)
		hold.set(next)
		cfg.View.setAdvertised(term)
		if !repair() {
			return nil, cycleTerminal
		}
		// Cleared once the panes show live content again, not on the bare
		// re-attach: a stale screen the user knows is stale is a paused
		// mirror, one they don't is a lie.
		clearBridgeState(cfg)
		return next, cycleConnected
	}
}

// replaceOutcome is how a voluntary replacement ended, and a caller has to act
// on all three (#574). notReplaced is the zero value on purpose: it is the one
// outcome under which nothing was closed and the old connection is still the
// mirror's, so the verdict a caller reads when it reads none is the harmless
// one.
type replaceOutcome int

const (
	// notReplaced — abandoned before anything was closed: a failed dial, a
	// failed or timed-out identity read, or an identity that does not match.
	// The old connection is still live and still published, and Advertised is
	// untouched, so the next raise correctly tries again.
	notReplaced replaceOutcome = iota
	// replaced — the swap landed, and the returned connection is the mirror's
	// now. The caller must adopt it exactly as it adopts reattach's, or runConn
	// goes on reading the old, closed pump.
	replaced
	// mirrorGone — the swap landed but repair() found the registry empty, so
	// the daemon tears down. Never collapsed into either of the others:
	// notReplaced would have the caller carry on over a connection this routine
	// has already closed, read a closed pump, and take the involuntary drop
	// path — whose first act is hold.close(), killing the working NEW
	// connection and dialling a third.
	mirrorGone
)

// replaceConn swaps the mirror's control client for a freshly dialled one, so
// that the termname cfg.View now wants is the one the remote sees and the
// remote's own graphics backend follows the client the user is looking through
// (#574). It returns the connection the mirror ended up on, if any, and the
// outcome.
//
// reattach's voluntary twin, differing in exactly one structural way: reattach
// closes the live connection BEFORE it dials, because a drop has already taken
// it, while this dials, verifies and primes first and abandons the attempt with
// the old connection untouched if any of that fails. Nothing here may cost a
// mirror — the gesture is a nicety, so a dial that fails, times out or answers
// as a different tmux server degrades this one press to the old backend rather
// than tearing anything down. That is also why a mismatched identity is not the
// teardown it is in reattach: the verified connection still in hold is the
// better evidence about which server the mirror is on, and a remote that really
// has been replaced drops that stream too, where reattach makes the call with
// the whole retry budget behind it.
//
// The old connection's buffered notifications are discarded wholesale with it —
// including the %client-session-changed the new attach provokes, measured
// arriving on the OLD stream during the overlap while the main loop is in here
// — so no ordinal can desync and no parser case is needed for them.
func replaceConn(cfg Config, router *Router, hold *connHolder, want remoteIdentity, reg *registry, repair func() bool) (*ctlConn, replaceOutcome) {
	// A detach raised before this gesture reached the main loop must not be
	// answered with a fresh transport for a daemon that is already shutting
	// down — reattach consults Shutdown twice for the same reason, and this
	// path had no check at all. Nothing has been closed yet, so notReplaced
	// leaves the mirror exactly as teardown expects to find it.
	if stopped(cfg.Shutdown) {
		return nil, notReplaced
	}
	// Snapshotted before the dial and published verbatim below. cfg.Dial builds
	// the ssh argv outside this package, so the daemon cannot ask what it
	// actually read; re-reading Desired() at the publish site instead would
	// widen the window a viewer switch can land in from one gesture to the whole
	// dial-plus-identity-read-plus-priming span, and a switch inside that window
	// makes Advertised a lie the raise guard then reads as equal forever.
	term := cfg.View.Desired()
	next, err := dialConn(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: replacement dial for %s: %v; keeping the current connection\n", cfg.RemoteHost, err)
		return nil, notReplaced
	}
	// Unbound (see newCtlConn) for the whole verification and priming span: an
	// unverified far end must not paint into the panes the registry holds, and
	// since both connections stream the same remote panes, a verified one must
	// not route alongside the live one either.
	disarm := armIdentityDeadline(next, cfg.identityTimeout())
	id, err := readIdentity(next.rt, cfg.RemoteSession)
	live := disarm()
	if err != nil {
		if live {
			next.close()
		}
		// reattach's retry/teardown split has no analogue here: neither shape of
		// this failure may touch the mirror, so both end the one attempt.
		fmt.Fprintf(os.Stderr, "daemon: %v; keeping the current connection\n", err)
		return nil, notReplaced
	}
	if !live {
		// The deadline closed it out from under the read, which on this path is
		// simply the end of the attempt.
		fmt.Fprintf(os.Stderr, "daemon: replacement identity read for %s answered after its deadline; keeping the current connection\n", cfg.RemoteSession)
		return nil, notReplaced
	}
	if !want.matches(id) {
		next.close()
		fmt.Fprintf(os.Stderr, "daemon: replacement dial for %s reached pid %d %s, not pid %d %s; keeping the current connection\n",
			cfg.RemoteSession, id.pid, id.sessionID, want.pid, want.sessionID)
		return nil, notReplaced
	}
	primeClient(cfg, next, reg)
	// The first and only close, reached only with a verified replacement in
	// hand. It leads the bind because a window in which both connections are
	// bound routes duplicate %output into one sink; the gap is the two
	// statements below, on this one goroutine, and whatever the remote drops
	// inside it is exactly what repair()'s reseed exists to restore.
	hold.close()
	next.bind(router)
	hold.set(next)
	cfg.View.setAdvertised(term)
	if !repair() {
		return nil, mirrorGone
	}
	return next, replaced
}

// primeClient asserts on c the size state tmux holds per control client, which
// a brand new one has therefore never been told: this client's own size, and
// each mirrored window's cap. Sent while c is still unbound and ahead of the
// old connection's close, because a window the remote CREATES in the gap before
// repair()'s own sends would be born at tmux's 80x23 control-client default
// (#449). Not protection against an existing window shrinking as a client goes
// away — that was measured, and tmux does not do it.
//
// One round-trip rather than the fire-and-forget sends repair() uses: nothing
// drains an unbound connection's pump, and every command written must have its
// reply claimed, or the claim count falls behind sent and no later round-trip
// recognises its own reply. A cap for a window that died on the remote answers
// with %error, which is claimed like any other block and discarded here — the
// same posture repair() takes for the same sends.
//
// Deliberately not recorded in the converger: repair() resets it wholesale and
// re-sends both anyway, so that stays the single authoritative record and these
// are idempotent asserts of a size already in force.
func primeClient(cfg Config, c *ctlConn, reg *registry) {
	w, h := cfg.LocalArea()
	if w <= 0 || h <= 0 {
		return
	}
	cmds := []string{ClientSizeCmd(w, h)}
	for _, remoteID := range reg.remoteIDs() {
		cmds = append(cmds, ConvergeCmd(remoteID, w, h))
	}
	reply := c.rt(cmds...)
	for range cmds {
		reply()
	}
}

// connVerdict is how one connection's main loop ended. Only connDrop is a
// transport failure worth another dial; connEnd covers the remote deliberately
// ending this control client (%exit) and a mirror left with no windows, either
// of which would reconnect into a session there is nothing left to mirror in.
type connVerdict int

const (
	connEnd connVerdict = iota
	connDrop
	// connReplace is not an ending: the loop is handing its connection back so
	// a voluntary replacement can run on the main-loop goroutine, the only
	// place a round-trip may run (#574). The mirror stands throughout, and
	// which connection it stands on afterwards is replaceOutcome's answer.
	connReplace
)

// bridgeStateDisconnected is the @bridge_state value picker/statusline renders
// a red marker for on a mirror window; absent means connected. It names the
// state of the user's mirror, not the daemon's retry activity (#482).
const bridgeStateDisconnected = "disconnected"

func setBridgeState(cfg Config, v string) {
	if cfg.LocalSess == "" {
		return
	}
	cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_state", v)
}

func clearBridgeState(cfg Config) {
	if cfg.LocalSess == "" {
		return
	}
	cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, "@bridge_state")
}

func clearBridgeRes(cfg Config) {
	if cfg.LocalSess == "" {
		return
	}
	cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, "@bridge_res")
}

// stopped reports whether the user has asked the daemon to shut down. A nil
// channel is never ready, so a Config without one never reads as stopped.
func stopped(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
