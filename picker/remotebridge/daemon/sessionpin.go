package daemon

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// sessionIDRe matches a tmux session id. The id is interpolated into a command
// sent over the control stream, so a value that is not one is not pinned with.
var sessionIDRe = regexp.MustCompile(`^\$[0-9]+$`)

// remoteIdentity names the tmux server process hosting the mirrored session —
// read in the same round-trip sessionPin already made for the session id, so
// there is one identity read and one authority for "which session are we on"
// (#482). A rebooted remote gets a different pid even when it hosts a
// same-named session with the same $N, since a fresh server restarts session
// ids at $0.
type remoteIdentity struct {
	pid       int
	sessionID string
	// startTime is optional belt-and-braces against a recycled pid. tmux
	// renders an unknown format as an empty field, so a remote predating
	// #{start_time} must not fail the read over it.
	startTime    int
	hasStartTime bool
}

// matches reports whether two identity reads name the same server. pid and
// sessionID compare always; startTime compares only when both sides carry
// one, so a remote that never offered it is never read as a mismatch.
func (a remoteIdentity) matches(b remoteIdentity) bool {
	if a.pid != b.pid || a.sessionID != b.sessionID {
		return false
	}
	return !a.hasStartTime || !b.hasStartTime || a.startTime == b.startTime
}

// identityReadErr is what readIdentity returns on failure. Retry distinguishes
// the two shapes a caller must never conflate: one(rt, …) returning ok==false
// means the connection EOF'd before the reply arrived — another drop, and the
// caller retries; a reply that arrived and is Kind==Error or does not parse is
// a genuine identity failure — a different server — and the caller tears down.
type identityReadErr struct {
	err   error
	retry bool
}

func (e *identityReadErr) Error() string { return e.err.Error() }

// Retry reports whether this failure is the EOF-mid-read shape (retry) as
// opposed to a reply that arrived and was wrong or malformed (teardown).
func (e *identityReadErr) Retry() bool { return e.retry }

// readIdentity fetches #{pid}|#{start_time}|#{session_id} for session in one
// round-trip and parses it strictly: pid and session_id are required, the
// same posture sessionIDRe and parseWindowID already take for anything
// interpolated into a later command — malformed is rejected, never coerced.
//
// It first opts the control client into v2 layouts: flags are per control
// client, every attach leads with this read, and without the flag tmux
// leaves floats out of a control client's layout dumps. A remote that
// refuses the flag still mirrors its tiled panes, so that is logged, not
// fatal.
func readIdentity(rt roundTrip, session string) (remoteIdentity, error) {
	next := rt(
		"refresh-client -f new-layouts",
		fmt.Sprintf("display-message -p -t %s -F '#{pid}|#{start_time}|#{session_id}'", tmuxQuote(session)),
	)
	flagReply, ok := next()
	if !ok {
		return remoteIdentity{}, &identityReadErr{
			err:   fmt.Errorf("daemon: identity read for %s: connection closed before reply", session),
			retry: true,
		}
	}
	if flagReply.Kind == controlmode.Error {
		fmt.Fprintf(os.Stderr, "daemon: remote refused refresh-client -f new-layouts (%s); floats will not be mirrored\n", strings.TrimSpace(string(flagReply.Data)))
	}
	l, ok := next()
	if !ok {
		return remoteIdentity{}, &identityReadErr{
			err:   fmt.Errorf("daemon: identity read for %s: connection closed before reply", session),
			retry: true,
		}
	}
	if l.Kind == controlmode.Error {
		return remoteIdentity{}, &identityReadErr{err: fmt.Errorf("daemon: identity read for %s: %s", session, strings.TrimSpace(string(l.Data)))}
	}
	return parseIdentity(session, string(l.Data))
}

// parseIdentity parses one pid|start_time|session_id reply body.
func parseIdentity(session, body string) (remoteIdentity, error) {
	fields := strings.SplitN(strings.TrimSpace(body), "|", 3)
	if len(fields) != 3 {
		return remoteIdentity{}, &identityReadErr{err: fmt.Errorf("daemon: identity reply for %s (%q) is not pid|start_time|session_id", session, body)}
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return remoteIdentity{}, &identityReadErr{err: fmt.Errorf("daemon: identity reply for %s (%q) has a non-numeric pid", session, body)}
	}
	sessionID := fields[2]
	if !sessionIDRe.MatchString(sessionID) {
		return remoteIdentity{}, &identityReadErr{err: fmt.Errorf("daemon: identity reply for %s (%q) is not a session id (%q)", session, body, sessionID)}
	}
	id := remoteIdentity{pid: pid, sessionID: sessionID}
	// Absent or unparsable start_time just means the field goes unrecorded —
	// it's belt-and-braces, not required, unlike pid and session_id above.
	if st := strings.TrimSpace(fields[1]); st != "" {
		if v, err := strconv.Atoi(st); err == nil {
			id.startTime, id.hasStartTime = v, true
		}
	}
	return id, nil
}

// sessionPin keeps this control client on the session it mirrors.
//
// A control-mode client receives %output only for the session it is currently
// attached to. A mirror pane's keystrokes reach the *remote* shell, where $TMUX
// is set, so `sesh connect` — or any switch-client from a bridged shell — runs
// switch-client with no -c, and tmux resolves "current client" to this daemon:
// the only client the bridged session has. Every %output for our panes stops
// from that instant, while input still lands and the remote command completes,
// so the mirror reads as frozen rather than dead (#396).
type sessionPin struct {
	// id is the mirrored session's id ($N), read from the remote rather than
	// learned from the first %session-changed — stream order is not evidence of
	// which session we are supposed to be on. Empty disables pinning.
	id string
	// identity is the server-identity tuple read alongside id, at the same
	// attach. identityKnown is false when the first attach's read failed, and
	// Run then stays single-shot rather than comparing a later attach against
	// a tuple that was never recorded (#482).
	identity      remoteIdentity
	identityKnown bool
	// handOff opens the session we were switched to as a mirror of its own, so
	// the gesture that switched us does what the user meant instead of nothing.
	// nil disables it.
	handOff func(session string)
	// activeWin reports the mirror session's locally-current window, for
	// active-first reseed ordering (#557).
	activeWin func() string
}

func newSessionPin(cfg Config, rt roundTrip) *sessionPin {
	p := &sessionPin{
		handOff:   cfg.HandOff,
		activeWin: func() string { return localActiveWindow(cfg) },
	}
	id, err := readIdentity(rt, cfg.RemoteSession)
	if err != nil {
		// The first attach records; it never tears down — there is nothing yet
		// to compare a later attach against. So an unusable read here (EOF or
		// malformed alike) leaves session pinning off and identityKnown false,
		// which keeps this daemon single-shot instead of failing startup.
		fmt.Fprintf(os.Stderr, "daemon: cannot read identity for %s; session pinning is off (%v)\n", cfg.RemoteSession, err)
		return p
	}
	p.id = id.sessionID
	p.identity = id
	p.identityKnown = true
	return p
}

// apply reacts to one %session-changed. Our own id is the attach-time
// notification, or our switch back landing; any other is an excursion.
func (p *sessionPin) apply(l controlmode.Line, reg *registry, router *Router, rt roundTrip) {
	if p.id == "" || len(l.Args) == 0 || l.Args[0] == p.id {
		return
	}
	away := string(l.Data)
	// No -c: a command sent over this stream resolves "current client" to this
	// control client, which is exactly the one that was switched away.
	if r, ok := one(rt, fmt.Sprintf("switch-client -t '%s'", p.id)); !ok || r.Kind == controlmode.Error {
		fmt.Fprintf(os.Stderr, "daemon: switch back to %s failed; mirror stays frozen\n", p.id)
		return
	}
	p.reseed(reg, router, rt)
	if p.handOff != nil && away != "" {
		go p.handOff(away)
	}
}

// reseed repaints every mirrored pane after the switch back.
func (p *sessionPin) reseed(reg *registry, router *Router, rt roundTrip) {
	active := ""
	if p.activeWin != nil {
		active = p.activeWin()
	}
	reseedPanes(reg, router, rt, active, "after session change")
}

// reseedPanes repaints every mirrored pane from the remote's own screens.
// Output produced while this control client was elsewhere — on another session,
// or on no connection at all — is dropped by the server, not buffered, so the
// panes are showing screens the remote has already moved past: an excursion
// swallows the very command that caused it and the prompt that followed, and an
// outage swallows everything that happened during it.
//
// One reseed path for both callers, deliberately: reusing PaneSeeds is what
// keeps the seed-before-output ordering (#233/#412/#417/#430) true.
//
// activeWin orders the batch so the locally-current window's seeds are written
// (and their replies read) first (#557) — one PaneSeeds call is a single
// round-trip, but the replies stream back in issue order, so the visible
// window's repaint lands ahead of the background ones on a slow link.
func reseedPanes(reg *registry, router *Router, rt roundTrip, activeWin, reason string) {
	wins := reg.all()
	// Order windows active-first before flattening their panes.
	for i, mw := range wins {
		if mw.localWin == activeWin && i > 0 {
			wins = append(append([]*mirrorWindow{mw}, wins[:i]...), wins[i+1:]...)
			break
		}
	}
	n := 0
	for _, mw := range wins {
		n += len(mw.remotePanes) + len(mw.localFloats)
	}
	ids := make([]string, 0, n)
	sinks := make([]*outputSink, 0, n)
	for _, mw := range wins {
		// Floats too: a mirrored float's renderer holds no back-buffer either,
		// so an excursion or an outage leaves it on a screen the remote has
		// already moved past.
		for _, id := range mw.allRemotePanes() {
			if s := router.sink(id); s != nil {
				ids = append(ids, id)
				sinks = append(sinks, s)
			}
		}
	}
	PaneSeeds(rt, ids, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reseed %s %s: %v\n", ids[i], reason, err)
			return
		}
		enqueueSeedWithReplay(sinks[i], seed)
	})
}
