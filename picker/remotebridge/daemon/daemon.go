// Package daemon owns the M2.2 mirror: one control-mode connection to a
// remote tmux, converged to a local window's size, with one native local
// window per remote window (one local pane per remote pane) and one renderer
// process per pane feeding/draining a unix socket. Run wires all of it
// together; see the orchestration sequence in
// docs/superpowers/plans/2026-07-20-remote-bridge-m2.2.md (Task 3).
package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
	"github.com/noamsto/tmux-og/picker/remotebridge/keyneg"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// Config is the injectable seam for Run: everything that talks to a real
// ssh/tmux/socket in production is a field here, so the bats test can point it
// at a second local tmux instead.
type Config struct {
	Ctl            io.ReadWriteCloser         // one already-opened tmux -C control-mode stream (stdin+stdout duplex); superseded by Dial
	SockPath       string                     // unix socket renderers dial
	LocalSess      string                     // "<host>-<sess>"
	RemoteHost     string                     // ssh host being mirrored (picker's Host column)
	RemoteSession  string                     // remote session name (may contain spaces)
	RemoteWindow   string                     // initially-selected remote window INDEX (not a mirror filter)
	PauseAfterSecs int                        // refresh-client -f pause-after=N (0 disables); backpressure insurance answered by a %continue re-seed
	RendererBin    string                     // absolute store path to cmd/renderer
	LocalTmux      func(args ...string) error // runs local tmux (injected; prod = exec)
	// LocalTmuxOut runs local tmux and captures stdout (injected; prod = exec).
	LocalTmuxOut func(args ...string) (string, error)
	LocalArea    func() (int, int)        // content area the local mirror session's clients can show (injected)
	Reflow       func()                   // forces a status-bar reflow of the mirror session (injected; nil = off)
	LocalPanes   func() map[string]string // remote pane id -> local pane id, read back from @bridge_pane (injected)
	// NewGraphics builds the per-pane kitty-graphics proxy that localises image
	// payloads crossing the bridge. nil disables proxying entirely (tests, and
	// any transport where there is no remote filesystem to fetch from).
	NewGraphics func(paneID string) *graphics.Proxy
	// PasteUpload ships one clipboard image to the remote over the bridge's
	// ssh ControlMaster and returns the remote path it landed at (#361). nil
	// disables ctrl+v image-paste interception (tests, --test-local).
	PasteUpload func(ctx context.Context, ext string, data []byte) (string, error)
	// RendererDied is death.wake, stamped onto cfg once per Run so pumpInput
	// can wake the main loop without threading a parameter through the whole
	// reconcile call chain. nil is legal — every test that builds a bare
	// Config{} directly, and any call path that predates this field.
	RendererDied func()
	// SendCtl is Run's sendCtl, the bool-reporting form of send, stamped onto
	// cfg once per Run so paster() can hand it to pasteHandler without
	// threading a parameter through the whole reconcile call chain. Unset in
	// every test Config that builds a pasteHandler directly rather than
	// through Run.
	SendCtl func(cmds ...string) bool
	// HandOff opens a remote session this bridge was switched to as a mirror of
	// its own (injected; prod = og-remote-open, nil = off). See sessionPin.
	HandOff func(remoteSession string)
	// Dial opens a fresh control-mode connection. Called for the first attach
	// and again after every drop; nil is single-shot over Ctl — a drop is
	// terminal, exactly the behaviour before #482.
	Dial func() (io.ReadWriteCloser, error)
	// Shutdown is closed when the user asks the daemon to stop (SIGTERM/SIGINT).
	// A pending reconnect selects on it, because SIGTERM works by dropping the
	// transport and that is indistinguishable from a link failure — and during
	// a backoff sleep there is no transport for the signal to reach at all.
	// og-remote-detach falls back to kill-session after 2s, so a daemon
	// that waits out its backoff is a daemon stranded. nil never cancels.
	Shutdown <-chan struct{}
	// Retry bounds the reconnect schedule; nil takes DefaultBackoff. A pointer
	// so "unset" stays distinguishable from a deliberately tiny schedule — the
	// Go tests shrink it rather than paying the real backoff.
	Retry *Backoff
	// IdentityTimeout bounds one attach's identity read; 0 takes
	// defaultIdentityTimeout. See armIdentityDeadline.
	IdentityTimeout time.Duration
	// View is the daemon's live view-identity cell — see Viewing's doc in
	// viewident.go for the full State model (Desired/Advertised/Relay).
	// Desired is what every dial's argv reads; Relay is the single capability
	// published to the remote via relayenv.go and read by the graphics
	// proxy's drop policy (R4/R6). Seeded at startup by cmd/daemon/main.go,
	// re-published on change by watchLocalClient below, and re-asserted on
	// every reconnect by repair().
	View *Viewing
}

// defaultIdentityTimeout bounds the identity read that leads every re-attach.
//
// The case it exists for is a wedged remote tmux behind a healthy sshd: the
// ServerAlive probes are answered by sshd, not by tmux, so the keepalives never
// fire, the connection stays up, and one(rt, …) blocks forever on a reply that
// is never coming. Backoff's attempt and elapsed caps are consulted only
// BETWEEN attempts, so in that state the bounded-retry guarantee is not
// enforced at all and only a manual SIGTERM recovers.
//
// 30s because Dial returns as soon as the transport process is started, so this
// covers the ssh handshake, authentication and the remote tmux attach as well
// as the display-message round-trip — seconds, legitimately, on a slow link or
// a cold ControlMaster. Far above that, and far below the reconnect budget
// (DefaultBackoff's 10 minutes), so a wedged remote burns retry attempts and
// then tears down like any other unreachable one.
const defaultIdentityTimeout = 30 * time.Second

// identityTimeout is the identity-read deadline this Config asks for.
func (c Config) identityTimeout() time.Duration {
	if c.IdentityTimeout > 0 {
		return c.IdentityTimeout
	}
	return defaultIdentityTimeout
}

// retrySchedule is the reconnect schedule this Config asks for.
func (c Config) retrySchedule() Backoff {
	if c.Retry != nil {
		return *c.Retry
	}
	return DefaultBackoff(time.Now)
}

func (c Config) graphicsFor(paneID string) *graphics.Proxy {
	if c.NewGraphics == nil {
		return nil
	}
	return c.NewGraphics(paneID)
}

// reflow re-derives the mirror session's window labels. The after-new-window
// hook's own reflow races the @bridge_win / @window_bridge_name stamps that
// follow the create, so it can label a mirror window from the launcher's cwd
// (#196); and a later rename changes no window count, so reflow's
// count:width cache would skip it. Every path that stamps a name ends here.
func (c Config) reflow() {
	if c.Reflow != nil {
		c.Reflow()
	}
}

// createMirrorWindow appends a window to the mirror session and returns its
// tmux window ID.
//
// The ID, not the index, is what every later command targets. renumber-windows
// is on, so closing one mirror window renumbers the rest — an index captured at
// creation would silently start addressing its neighbour (#411). Appending at
// {end} rather than at an index of our own choosing is what leaves the
// re-indexing to tmux, so a mirror never grows the gaps a local session can't.
func createMirrorWindow(cfg Config) (string, error) {
	out, err := cfg.LocalTmuxOut("new-window", "-d", "-P", "-F", "#{window_id}",
		"-a", "-t", cfg.LocalSess+":{end}")
	if err != nil {
		return "", fmt.Errorf("daemon: new-window in %s: %w", cfg.LocalSess, err)
	}
	return parseWindowID(out)
}

// firstMirrorWindow is the ID of the window the launcher created the mirror
// session with, which the first remote window reuses rather than adding a
// second one beside it.
func firstMirrorWindow(cfg Config) (string, error) {
	out, err := cfg.LocalTmuxOut("list-windows", "-t", cfg.LocalSess, "-F", "#{window_id}")
	if err != nil {
		return "", fmt.Errorf("daemon: list-windows %s: %w", cfg.LocalSess, err)
	}
	first, _, _ := strings.Cut(out, "\n")
	return parseWindowID(first)
}

// parseWindowID validates a window id read back from tmux. A reply that isn't
// one is an error rather than something to interpolate into a target: tmux
// would read a bare "7" as window INDEX 7, which is exactly the addressing this
// change exists to remove.
func parseWindowID(s string) (string, error) {
	id := strings.TrimSpace(s)
	if !strings.HasPrefix(id, "@") || len(id) == 1 {
		return "", fmt.Errorf("daemon: %q is not a tmux window id", id)
	}
	return id, nil
}

// stampMirrorWindow marks localWin as this daemon's mirror of a remote window
// and gives it that window's name.
//
// automatic-rename goes off because the daemon owns the name: tmux only
// re-derives one when the active pane produces output, and an idle renderer
// never does — so a name derived once, during setup, would freeze on the
// launcher's cwd for the life of the window.
//
// Panes are addressed 0-based (spawnRenderer/reconcileLayout use index starting
// at 0); the pane-base-index override keeps that true regardless of the host's
// global (real hosts set 1).
//
// remain-on-exit goes on because a renderer's exit must not be structural.
// argv covers a user Respawn: spawnRenderer passes sock and remote pane id
// as renderer arguments, so a bare respawn-pane reconnects. A genuine crash
// still exits, and with the host's global remain-on-exit off the pane then
// closes, taking the window and, for a single-pane mirror, the whole mirror
// session with it (#547). A dead pane instead of a lost session is the
// difference; healDeadRenderers repairs it from there.
func stampMirrorWindow(cfg Config, localWin, remoteName string) {
	cfg.LocalTmux("set-option", "-w", "-t", localWin, "@bridge_win", "1")
	cfg.LocalTmux("set-option", "-w", "-t", localWin, "pane-base-index", "0")
	cfg.LocalTmux("set-option", "-w", "-t", localWin, "automatic-rename", "off")
	cfg.LocalTmux("set-option", "-w", "-t", localWin, "remain-on-exit", "on")
	applyMirrorName(cfg, localWin, remoteName)
}

// applyMirrorName writes the remote window's name to both places a mirror
// window carries it: @window_bridge_name (what reflow labels from) and the
// window name itself. Both, every time — with automatic-rename off nothing else
// re-derives the name, so a path that wrote only the option would leave the
// window name frozen at whatever the previous write left behind.
func applyMirrorName(cfg Config, localWin, remoteName string) {
	name := sanitizeWindowName(remoteName)
	if name == "" {
		return
	}
	cfg.LocalTmux("set-option", "-w", "-t", localWin, "@window_bridge_name", name)
	cfg.LocalTmux("rename-window", "-t", localWin, name)
}

// outputSinkBuf is the per-renderer output buffer depth. Overflow drops the
// frame rather than blocking the control-stream loop; the pane self-heals on
// its next %output, or on the fresh FrameSeed any %continue sends.
const outputSinkBuf = 4096

// helloTimeout bounds how long waitHellos blocks for renderers to dial
// back. A spawned renderer that never connects (bad RendererBin, exec
// failure, crash before it dials) doesn't surface as a LocalTmux error —
// respawn-pane itself succeeds — so without a deadline the wait blocks its
// caller forever: startup never proceeds, and reconcile never returns the main
// loop to dispatching.
const helloTimeout = 10 * time.Second

// helloConn pairs an accepted renderer connection with the remote pane id it
// announced via FrameHello.
type helloConn struct {
	paneID string
	conn   net.Conn
}

// helloWaiter collects a renderer connection for each remote pane id the
// caller just spawned. Every mirror path takes one of these rather than the
// connection channel itself: the wait has to keep the control stream moving
// (see waitHellos), and the pump it drains to do that is Run's alone.
type helloWaiter func(want []string) (map[string]net.Conn, error)

// resizePollInterval is how often the resize watcher re-checks the nudge
// file's mtime (an os.Stat, not a fork). It only forks LocalArea's
// display-message/list-clients calls when that mtime has advanced (#433).
const resizePollInterval = time.Second

// resizeNudgeSuffix names the per-bridge file a session-scoped client-resized
// hook touches (see registerResizeHook). Its mtime is the event watchLocalClient
// polls for instead of forking a query every tick.
const resizeNudgeSuffix = ".resize"

// resizeFallbackInterval bounds staleness on top of the mtime nudge: a
// filesystem with coarse mtime resolution can make a real touch
// indistinguishable from one already observed (two resizes landing in the
// same rounded second read back as the same mtime), which would otherwise
// leave the mirror capped at a stale size forever if no further resize ever
// lands in a distinguishable bucket. Every tick where the interval has
// elapsed since the last check forces one regardless of the nudge, same as
// the unconditional poll this replaces — just far less often.
const resizeFallbackInterval = 30 * time.Second

// watchLocalClient re-asserts every mirrored window's cap, and re-resolves the
// viewing identity, whenever something about the local client changes — two
// consequences of one nudge, which is why one watcher owns both (renamed from
// watchResize when the second joined). A local terminal/client resize emits no
// control-stream event, so the daemon polls — but cheaply: nudged reports the
// resize-hook file's mtime via a plain os.Stat, and area's fork-per-call query
// only runs once that mtime has advanced past the last one observed,
// collapsing the steady-state cost from one fork/sec to one stat/sec (plus one
// fork every resizeFallbackInterval as a safety net — see its doc). A tick
// whose stat misses a touch is not lost: mtime persists on disk, so the next
// tick's stat still sees it and converges — one poll cycle later than the hook
// itself.
//
// On a size change it re-pushes ConvergeCmd per mirrored window, which resizes
// the remote and makes it emit %layout-change per window, driving the existing
// reconcile + re-seed (and the re-fit of the local window to the remote's new
// size). send is the same mutex-guarded, no-op-when-closed sender the main
// loop uses; this only injects fire-and-forget commands (their %begin/%end
// acks are consumed harmlessly by the main loop's own nextLine read) — but it
// reports whether the line was written, so a send onto a dead stream undoes
// the converger's record rather than latching a size the remote never got.
//
// On a viewing-identity change (R10): resolveView is a function parameter,
// like area and nudged, so this stays unit-testable with no tmux. A non-empty
// resolution always moves Desired — every dial reads it fresh, so asserting it
// costs nothing — and always refreshes the stored Relay (its raw diagnostic
// included), but a RelayEnvCmd publish fires only when the resolved
// CAPABILITY itself changed: a termname-only change, or no attached client at
// all (R3 — the resolution is empty), publishes nothing. A failed send is
// undone the same way the converger's is above: a write that never reached
// the remote must not be recorded as current, or the next still-different
// resolve would read as already-published and never retry. This watcher must
// NEVER write Advertised — only a publish site does that (see Viewing's State
// model).
func watchLocalClient(area func() (int, int), nudged func() (time.Time, bool), activeWin func() string, resolveView func() (ViewIdentity, bool), view *Viewing, remoteSession string, reg *registry, cv *converger, send func(...string) bool, stop <-chan struct{}, tick <-chan time.Time) {
	var lastNudge time.Time
	lastCheck := time.Now()
	for {
		select {
		case <-stop:
			return
		case now := <-tick:
			due := false
			if mtime, ok := nudged(); ok && mtime.After(lastNudge) {
				lastNudge = mtime
				due = true
			}
			if !due && now.Sub(lastCheck) >= resizeFallbackInterval {
				due = true
			}
			if !due {
				continue
			}
			lastCheck = now
			w, h := area()
			// The client size first: it governs what a window created after this
			// point is born at, while the per-window caps below govern the ones
			// that already exist (#449).
			if w > 0 && h > 0 && cv.need(clientSizeKey, w, h) && !send(ClientSizeCmd(w, h)) {
				cv.unrecord(clientSizeKey, w, h)
			}
			// Active window first: each converge resizes the remote window,
			// whose %layout-change queues a reconcile — so this order is the
			// order the main loop works through them in (#557).
			for _, remoteID := range activeFirst(reg, activeWin(), reg.remoteIDs()) {
				if cv.need(remoteID, w, h) && !send(ConvergeCmd(remoteID, w, h)) {
					cv.unrecord(remoteID, w, h)
				}
			}
			if id, ok := resolveView(); ok {
				view.SetDesired(id.Term)
				prev := view.Relay.Load()
				view.Relay.Store(id.Relay)
				if id.Relay.Sixel() != prev.Sixel() && !send(RelayEnvCmd(remoteSession, id.Relay.String())) {
					view.Relay.Store(prev)
				}
			}
		}
	}
}

// resizeHookEvents are the events that can grow the mirror session's window,
// or move the viewing identity a control client should advertise:
// client-resized fires for an attached client's terminal resize,
// window-resized for any window resize including a programmatic one against a
// detached session (window-size is "latest", so the mirror stays detached
// between launcher switches — #433's own reproduction resizes it that way),
// client-session-changed for a client switching onto or off this session
// (measured redundant with client-attached on a fresh attach too, so that one
// is left out), and client-detached for the last client leaving. Every one of
// these now also runs watchLocalClient's area() fork and a re-resolve of the
// viewing identity (R10) — cv.need and the Relay comparison dedupe the actual
// sends, so a session switch or detach is cheap but no longer free.
var resizeHookEvents = [...]string{"client-resized", "window-resized", "client-session-changed", "client-detached"}

// localActiveWindow reports the mirror session's current window — the one the
// local client is looking at — or "" when it can't be learned (detached
// session, query failure). One local tmux fork, never an ssh round-trip.
func localActiveWindow(cfg Config) string {
	if cfg.LocalSess == "" {
		return ""
	}
	out, err := cfg.LocalTmuxOut("display-message", "-p", "-t", cfg.LocalSess, "#{window_id}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// activeFirst returns ids with the window whose local id is activeLocalWin
// moved to the front. Every multi-window pass is serialized round-trips on
// the main loop, so on a slow link the ordering IS the perceived re-attach
// latency: the window under the user's eyes reconciles and reseeds first, the
// rest follow while already-correct content is on screen (#557). "" or an
// unknown window leaves the order untouched.
func activeFirst(reg *registry, activeLocalWin string, ids []string) []string {
	if activeLocalWin == "" {
		return ids
	}
	for i, id := range ids {
		if w, ok := reg.byRemoteID(id); ok && w.localWin == activeLocalWin {
			if i == 0 {
				return ids
			}
			out := make([]string, 0, len(ids))
			out = append(out, id)
			out = append(out, ids[:i]...)
			out = append(out, ids[i+1:]...)
			return out
		}
	}
	return ids
}

// registerResizeHook wires session-scoped hooks that touch nudgePath — no
// fork on the daemon's side, just a stat once a tick sees the touch.
// Session-scoped (not the global config's hooks) so the lifecycle stays owned
// by the bridge: registered here, removed in unregisterResizeHook.
func registerResizeHook(cfg Config, nudgePath string) {
	if cfg.LocalSess == "" {
		return
	}
	touch := "touch -- " + tmuxQuote(nudgePath)
	hook := fmt.Sprintf("run-shell -b %s", tmuxQuote(touch))
	for _, event := range resizeHookEvents {
		cfg.LocalTmux("set-hook", "-t", cfg.LocalSess, event, hook)
	}
}

// unregisterResizeHook removes the hooks registerResizeHook set, so a dead
// bridge's session (or one reused for a later daemon) carries none of its
// hooks forward.
func unregisterResizeHook(cfg Config) {
	if cfg.LocalSess == "" {
		return
	}
	for _, event := range resizeHookEvents {
		cfg.LocalTmux("set-hook", "-u", "-t", cfg.LocalSess, event)
	}
}

// stream owns the command side of the control connection. It serializes writes
// — the setup path, every renderer's input pump and every ctl connection share
// one wire — and numbers the commands, which is what lets a round-trip find its
// own reply.
//
// tmux guards each command a control client sends with a %begin..%end carrying
// ClientCommandFlag, in the order the commands were run, so the Nth such block
// answers the Nth command. Counting is the only way to line them up: most
// commands here are fire-and-forget (a keystroke's send-keys, a ctl gesture, a
// converge) and leave a reply block behind that nobody waits for, and a hook on
// the remote adds blocks flagged 0 on top of those (#276).
type stream struct {
	mu     sync.Mutex
	w      *bufio.Writer
	closed bool
	sent   uint64 // commands written
	seen   uint64 // client-flagged reply blocks consumed
	// fans is the FIFO of if-shell commands whose branch replies are still
	// being swallowed; see fanout.
	fans []fanout
}

// fanout marks one command written with its own barrier behind it. tmux runs
// an if-shell's branch as further commands of the SAME client, and each of
// them guards a client-flagged reply block of its own, so one command written
// produces 1+N blocks, N being however many branch commands actually ran — a
// failing branch command aborts the rest of its list, so N is not even a
// constant. Counting blocks cannot tell them from the next command's reply, so
// the count would run ahead of the commands for the rest of the connection.
//
// The branch runs immediately after the if-shell's own block and before
// anything written behind it, so the barrier's reply (recognised by its body,
// which no branch command can produce) is the first block after them. Every
// block between the command's own reply and that one takes no ordinal.
//
// EVERY command takes one, not just the verbs known to fan out (#723). The
// swallow window in claim is not an N-block assumption — it consumes whatever
// arrives until the barrier's own reply — so arming it unconditionally leaves
// no verb list to keep in sync with tmux. Measured on tmux next-3.9, one
// control client, counting client-flagged blocks per command written:
// `display-message -p` 1, `run-shell -C` 2, `if-shell` with a two-command
// branch 3. Nothing compares s.seen against s.sent, so an unarmed fan-out
// desyncs the stream for the rest of the connection.
//
// `run-shell -b` needs no barrier: it defers its branch past one, and those
// blocks come back flagged 0, which claim never sees.
type fanout struct {
	after uint64 // ordinal of the command the barrier follows
	tag   string // body of the barrier's reply
}

func newStream(w io.Writer) *stream { return &stream{w: bufio.NewWriter(w)} }

// stampAll writes every command in cmds and returns their ordinals. ok is false
// once the daemon is tearing down: a ctl request that loses that race must not
// be acked as accepted, or the keybind reports success for a gesture that never
// happened.
//
// One lock for the whole batch, so no foreign command from pumpInput, a ctl
// request or watchLocalClient lands between ours — correctness doesn't need it (the
// ordinals are assigned under the lock either way), but a contiguous batch keeps
// a wire trace legible. The lock is never held across a read: this returns
// before any reply is read, which is what keeps those three deadlock-free while
// a batch is in flight.
func (s *stream) stampAll(cmds ...string) (seqs []uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	for _, cmd := range cmds {
		fmt.Fprintf(s.w, "%s\n", cmd)
		s.sent++
		seqs = append(seqs, s.sent)
		// The tag is a format to display-message: keep it a literal, never
		// remote-derived text. No -t either — a target that vanished would
		// answer with an error block and leave the swallow window open.
		f := fanout{after: s.sent, tag: fmt.Sprintf("og-fanout-%d", s.sent)}
		fmt.Fprintf(s.w, "display-message -p %s\n", f.tag)
		s.sent++
		s.fans = append(s.fans, f)
	}
	// bufio.Writer latches its first write error and no-ops every later write,
	// so a half-closed ssh stdin mid-batch has to fail the whole batch: s.sent
	// would otherwise keep advancing for commands tmux never received, and the
	// matching next() would block in readReplyRouting awaiting a reply block
	// that can never arrive, freezing the main loop. Closing bars every later
	// ordinal, so the gap is inert by construction. The prefix that did reach
	// tmux leaves reply blocks nobody reads, which desyncs nothing:
	// readReplyRouting walks past any ordinal it isn't waiting for, and s.seen
	// advances in nextLine regardless of who reads.
	if err := s.w.Flush(); err != nil {
		s.closed = true
		return nil, false
	}
	return seqs, true
}

// send writes cmds for callers that don't read the reply. Variadic so a
// caller needing two commands to land together gets one stampAll batch, which
// fails as a unit.
func (s *stream) send(cmds ...string) bool {
	_, ok := s.stampAll(cmds...)
	return ok
}

// claim consumes one client-flagged reply block, whose body is body, and
// returns the ordinal of the command it answers — 0 for a block that answers
// none of ours, which is what the branch of an if-shell does (see fanout).
func (s *stream) claim(body []byte) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.fans) > 0 && s.seen == s.fans[0].after {
		if string(body) != s.fans[0].tag {
			return 0
		}
		s.fans = s.fans[1:]
	}
	s.seen++
	return s.seen
}

func (s *stream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// newRoundTrip builds the roundTrip seam over one control connection: the whole
// batch is written first, then each next() reads the reply block of the next
// command in issue order.
func newRoundTrip(reader lineReader, router *Router, async *asyncQueue, st *stream) roundTrip {
	return func(cmds ...string) replies {
		seqs, ok := st.stampAll(cmds...)
		if !ok {
			return func() (controlmode.Line, bool) { return controlmode.Line{}, false }
		}
		i := 0
		return func() (controlmode.Line, bool) {
			// An index-out-of-range panic in a long-running daemon is worse than
			// one dead-batch report.
			if i >= len(seqs) {
				return controlmode.Line{}, false
			}
			seq := seqs[i]
			i++
			return readReplyRouting(reader, router, async, st, seq)
		}
	}
}

// Run mirrors every window of the bridged remote session, each into its own
// local window, over a -CC connection, until %exit, an emptied mirror, the
// local mirror session going away, or a drop the reconnect budget cannot
// outlast.
//
// Two lifetimes live here and they only look like one (#482). The session
// lifetime — listener, pidfile, registry, renderer panes and their sinks, the
// resize watcher, the agent shipper — is created once and destroyed only by
// teardown, which runs exactly once per return path. The connection lifetime —
// transport, pump, stream, round-tripper, and every client-scoped value the
// remote holds for this control client — is rebuilt on every attach.
func Run(cfg Config) error {
	router := NewRouter()
	// Declared here rather than beside the registry below: the client-size send
	// that follows the first dial is the converger's first user.
	cv := newConverger()
	// hold separates the daemon's session lifetime from its connection lifetime
	// (#482). send, sendCtl and rt are stable closures over it, so the
	// goroutines that capture them — every renderer's input pump, the resize
	// watcher, the ctl accept loop — keep working across a re-dial rather than
	// having to be restarted onto the new connection.
	hold := &connHolder{}
	// send is the fire-and-forget form the mirror paths use; the ctl and resize
	// paths take sendCtl, since they act on whether the line was written.
	send := func(s string) { hold.send(s) }
	sendCtl := hold.send
	rt := hold.roundTrip
	cfg.SendCtl = sendCtl

	c, err := dialConn(cfg)
	if err != nil {
		return err
	}

	// The identity read leads every attach, this one included, and runs on the
	// unverified connection — before bind, so nothing the far end says can reach
	// a sink. Here it only records: there is nothing yet to compare against, so
	// newSessionPin never tears down.
	//
	// The deadline it runs under matters even so: attach 1 has no retry budget
	// behind it — that is reattach's — so a far end that accepts the connection
	// and then never answers would park daemon startup here indefinitely. A
	// fired deadline has already closed the connection, so there is nothing
	// left to carry on over.
	disarm := armIdentityDeadline(c, cfg.identityTimeout())
	pin := newSessionPin(cfg, c.rt)
	if !disarm() {
		c.close()
		return fmt.Errorf("daemon: identity read for %s timed out", cfg.RemoteSession)
	}
	c.bind(router)
	hold.set(c)
	setPhase(cfg, "attached to %s", cfg.RemoteHost)

	// The first thing the remote might act on: give this control client a size,
	// so a window created on the remote is born at the local client's size
	// instead of tmux's 80-column control-client default (#449). It follows the
	// identity read rather than leading it — nothing between the two creates a
	// window. watchLocalClient re-sends this slot only on a CHANGE, so a lost write
	// here has nothing to correct it — every window created afterwards is born
	// at the default — which is why the record is undone when the write did not
	// happen (#481).
	if w, h := cfg.LocalArea(); w > 0 && h > 0 && cv.need(clientSizeKey, w, h) && !sendCtl(ClientSizeCmd(w, h)) {
		cv.unrecord(clientSizeKey, w, h)
	}

	// Reconnect needs both a way to open another connection and a recorded
	// identity to check it against; without either the daemon stays single-shot,
	// exactly the behaviour before #482.
	reconnect := cfg.Dial != nil && pin.identityKnown

	// The implicit attach reply needs no draining: it is flagged 0, so the reply
	// reader skips it like any other block we did not ask for.
	//
	// Enumerate every window of the bridged remote session. Read BOTH index
	// and id: --window is an *index*, the registry is keyed by *id* (@N).
	//
	// These returns, and the identity timeout above, are the only ones that
	// precede teardown: nothing has been built yet to tear down, but Run dialled
	// this connection itself and so owes it a close.
	lw, ok := one(rt, fmt.Sprintf("list-windows -t %s -F %s", tmuxQuote(cfg.RemoteSession), windowListFormat))
	if !ok || lw.Kind == controlmode.Error {
		hold.close()
		return fmt.Errorf("daemon: list-windows for %s failed", cfg.RemoteSession)
	}
	remoteWins := parseWindowList(string(lw.Data))
	if len(remoteWins) == 0 {
		hold.close()
		return fmt.Errorf("daemon: remote session %s has no windows", cfg.RemoteSession)
	}

	// One-shot, here rather than in repair: Run() runs exactly once per bridge,
	// so this is what makes the report "once per bridge connect" (#545) with no
	// state of its own to track.
	if available, checked := themeToggleAvailable(rt, cfg.RemoteSession); checked && !available {
		notifyThemeMissing(cfg)
	}
	// Once per bridge, like the probe above: tmux sets session_path at creation
	// and nothing a mirror follows changes it afterwards.
	if p := readSessionPath(rt, cfg.RemoteSession); p != "" {
		cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_session_path", p)
	}

	// Published here for the first attach; repair() (below) re-sends the same
	// read on every later one, since a capability change during an outage has
	// nobody else to tell the remote (R5). Sent unconditionally, empty value
	// included — a prior bridge from a sixel-capable terminal can have left
	// "sixel" in this same session's table, and skipping the write when this
	// one has nothing to say would leave that stale value standing and make
	// the remote emit graphics this proxy only drops.
	sendCtl(RelayEnvCmd(cfg.RemoteSession, cfg.View.Relay.Load().String()))

	os.Remove(cfg.SockPath)
	listener, err := net.Listen("unix", cfg.SockPath)
	if err != nil {
		hold.close()
		return fmt.Errorf("daemon: listen %s: %w", cfg.SockPath, err)
	}
	// The socket forwards keystrokes to the remote pane and streams its output,
	// so restrict it to the owning user.
	if err := os.Chmod(cfg.SockPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: chmod %s: %v\n", cfg.SockPath, err)
	}
	// Pidfile beside the socket: the launcher reads it to detect an already-live
	// bridge for this host:session (reuse instead of stacking a rival daemon)
	// and to tell a stale socket from one a running daemon still owns. Removed in
	// teardown so a clean exit leaves neither file behind.
	pidFile := cfg.SockPath + ".pid"
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: write pidfile %s: %v\n", pidFile, err)
	}
	connCh := make(chan helloConn, 64)
	cst := newCtlState()
	// One query construction for the viewing identity, shared by the ctl
	// handler's raise below, watchLocalClient's re-resolve (R10) and the
	// startup seed in cmd/daemon/main.go, rather than building the
	// list-clients argv once per reader.
	resolveView := func() (ViewIdentity, bool) { return ResolveLocalViewIdentity(cfg.LocalTmuxOut, cfg.LocalSess) }
	// Session-lifetime, like loopTick below and for the same reason: runConn
	// selects on its channel, so a seam built per attach would leak one
	// channel per reconnect and the loop could only watch the handle it can
	// see.
	replacer := newViewReplacer(cfg.View, resolveView, reconnect)
	// Session lifetime, like replacer and loopTick: runConn selects on its
	// timer, and one built per attach would leak a timer per reconnect.
	carousel := newCarouselProbe()
	// Session lifetime, like carousel: runConn selects on its timer too, and
	// it is set on cfg here, before any call that might invoke pumpInput (the
	// earliest is inside the mirror-window setup loop, well after this point
	// in Run's body) — cfg is a value parameter, so every later call site that
	// receives a copy of it carries the field once it is set here.
	death := newDeathNudge()
	cfg.RendererDied = death.wake
	// The listener outlives a drop, so a keybind pressed mid-outage reaches
	// here and gets nacked by the closed stream rather than hanging. The nack
	// must carry a non-empty error or the keybind claims a gesture landed that
	// never did; `ping` is exempt by construction, since parseCtl returns an
	// empty request for it and submit therefore sends nothing — which is what
	// keeps og-remote-open reusing this bridge instead of stacking a second
	// daemon on the same socket.
	go acceptConns(listener, connCh, func(argv []string) error {
		return handleCtl(cst, replacer, carousel, argv, cfg.RemoteSession, sendCtl)
	})

	// @bridge_sock is the carrier a keybind reads to reach this daemon. Stamped
	// on the session before any mirror window exists, so no gate can fire against
	// a bridge window whose session has no socket yet — and stamped by the daemon
	// rather than the launcher so the offline --test-local harness gets it too.
	if cfg.LocalSess != "" {
		cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_sock", cfg.SockPath)
		// @bridge_host is what the session picker's Host column reads; the local
		// session name can't be split back into host+session (either may hold a "-").
		cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_host", cfg.RemoteHost)
		cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_session", cfg.RemoteSession)
	}
	// The launcher reuses a mirror session (#474), so a prior daemon killed
	// mid-outage — teardown would have taken the session with it — can leave
	// its disconnected badge behind on a session this one is now attached to.
	clearBridgeState(cfg)

	reg := newRegistry()
	// The one waiter every mirror path gets. connCh goes no further than this
	// closure: draining the stream while waiting needs the pump, and only Run
	// has it. unexpected hellos are reconnects that arrived mid-wait (a
	// respawn whose pane is not in this spawn set); collect them here and
	// rebind after waitHellos returns. rebindRenderer seeds via a round-trip,
	// and waitHellos is already the goroutine reading that stream.
	waitHellosFn := func(want []string) (map[string]net.Conn, error) {
		c := hold.get()
		if c == nil {
			return nil, fmt.Errorf("daemon: hello wait with no control connection")
		}
		var extras []helloConn
		out, err := waitHellos(c.pump.lines, router, c.async, c.st, connCh, want, helloTimeout, func(hc helloConn) {
			extras = append(extras, hc)
		})
		// Adopt extras even when the wait failed: closeConns already dropped
		// the want-set, and these panes reconnect independently of it.
		for _, hc := range extras {
			rebindRenderer(cfg, hc, send, router, reg, rt)
		}
		return out, err
	}
	// nudgePath is the file registerResizeHook's client-resized hook touches;
	// removed here so a stale touch from a prior daemon on this same socket
	// path can't be mistaken for a resize before the hook ever fires again.
	nudgePath := cfg.SockPath + resizeNudgeSuffix
	os.Remove(nudgePath)
	// stopWatch stops the resize watcher (started just before the main loop).
	// Declared here so teardown can close it; teardown runs exactly once per
	// Run return path, so a plain close is safe.
	stopWatch := make(chan struct{})
	// reveals collects the local mirror windows a local attach, switch-client
	// or window switch just started displaying, for reseedRevealed to re-seed
	// on the main loop. Declared here, beside stopWatch, for the same reason:
	// the watcher goroutine that fills it starts below, before the main loop.
	reveals := &revealQueue{}
	// sessionGone counts consecutive definite negatives from the local-session
	// probe the coarse tick runs. Session-lifetime, like the registry: a
	// reconnect does not bring a gone session back, so the count survives one.
	var sessionGone sessionGoneTracker
	// localSessionVanished records that this run is ending because the local
	// mirror session is gone. teardown then leaves the session name alone — it
	// belongs to nobody now, and a reopen that recreated it in the gap must not
	// have the fresh session killed out from under it.
	var localSessionVanished bool
	// Assigned once the mirror is up; teardown must drop the status files it
	// wrote, so it is declared ahead of the closure that captures it.
	var agents *agentShipper
	// Same reason for labels; and the coarse tick is created at the main loop,
	// which the three early returns below never reach, so teardown needs a
	// nil-guarded handle to stop it.
	var (
		labels   *labelShipper
		res      *resShipper
		loopTick *time.Ticker
	)
	teardown := func() {
		close(stopWatch)
		clearPhase(cfg)
		unregisterResizeHook(cfg)
		os.Remove(nudgePath)
		if agents != nil {
			agents.clear()
		}
		// Before the kill-session below, which is what makes the -u land: the
		// reg.all() loop between only unregisters sinks and closes conns.
		if labels != nil {
			labels.clear(cfg, reg)
		}
		if res != nil {
			res.clear(cfg)
		}
		if loopTick != nil {
			loopTick.Stop()
		}
		listener.Close()
		os.Remove(cfg.SockPath)
		os.Remove(pidFile)
		for _, mw := range reg.all() {
			// Unregister closes each pane's output sink, stopping its pump
			// goroutine (mirrors closeWindow); then drop the renderer conns.
			// Floats included, or their sinks outlive the daemon they belong to.
			for _, id := range mw.allRemotePanes() {
				router.Unregister(id)
			}
			for _, c := range mw.conns {
				c.Close()
			}
		}
		// Unset before hold.close(): the variable describes the *local*
		// terminal of a bridge that, after this closure returns, no longer
		// exists. A stale "sixel" read by someone who later attaches to this
		// remote session directly, from a terminal with no sixel, reproduces
		// the #319 garbage-on-screen symptom on a screen the bridge was never
		// part of. This is best-effort, not a guarantee: the dominant teardown
		// path is SIGTERM, whose signal handler kills the transport before Run
		// ever reaches this closure, so send here already fails closed and the
		// unset does not land — it only lands on Run's own early-return
		// teardowns, where the connection is still alive. A residue left by a
		// SIGKILL or a lost race is corrected by the next bridge's
		// unconditional RelayEnvCmd write above; a direct attach in that gap
		// can still read the stale value.
		sendCtl(RelayEnvUnsetCmd(cfg.RemoteSession))
		// Whichever connection is current, which after a reconnect is no longer
		// the one cfg.Ctl named.
		hold.close()
		if cfg.LocalSess != "" && !localSessionVanished {
			cfg.LocalTmux("kill-session", "-t", cfg.LocalSess)
		}
	}

	// Before the mirror windows exist, not after they are all set up: the hook
	// only touches nudgePath, so arming it early costs one stat a tick, while
	// arming it late drops every resize landing during setup — and setup is the
	// slow part, spanning a spawn/hello/seed round-trip per window plus the
	// reconcile below. A dropped nudge is not lost work but a 30s wait for
	// resizeFallbackInterval, which is the delay the hook exists to avoid.
	registerResizeHook(cfg, nudgePath)

	// Mirror each remote window into its own local window. The first reuses the
	// launcher's initial window; the rest are appended.
	//
	// Only the first caption is ever seen: setupWindow's respawn-pane replaces
	// the loading pane with that window's renderer. The rest are written anyway
	// so a bridge whose first window stalls says which one.
	for i, rw := range remoteWins {
		setPhase(cfg, "mirroring window %d/%d", i+1, len(remoteWins))
		var (
			localWin string
			err      error
		)
		if i == 0 {
			localWin, err = firstMirrorWindow(cfg)
		} else {
			localWin, err = createMirrorWindow(cfg)
		}
		if err != nil {
			teardown()
			return err
		}
		stampMirrorWindow(cfg, localWin, rw.name)
		mw := reg.add(rw.id, localWin)
		if err := setupWindow(cfg, send, router, waitHellosFn, cst, mw, cv, rt); err != nil {
			teardown()
			return err
		}
	}

	// Select the initially-requested window. RemoteWindow is a window INDEX
	// (not an id), so resolve index -> id -> local window via the enumerated
	// list; never treat it as id "@<idx>".
	if initWin, ok := localWinForRemoteIndex(remoteWins, reg, cfg.RemoteWindow); ok {
		cfg.LocalTmux("select-window", "-t", initWin)
	}

	// Re-read the remote once setup is done. Names were captured by the single
	// enumeration above, but setup spans a spawn/hello/seed round-trip per
	// window and reads its replies with the plain skip reader — so every
	// %window-renamed the remote emits in that interval is discarded (B3). A
	// remote whose windows rename as their shells settle (or, on a real
	// tmux-og host, on every automatic-rename tick) would otherwise keep the
	// name it happened to have at attach for the life of the mirror. Reconcile
	// re-asserts each name from ground truth, and ends in a reflow.
	reconcileWindows(cfg, send, router, waitHellosFn, cst, reg, cv, rt)
	if reg.empty() {
		teardown()
		return nil
	}

	// Re-converge the remote whenever the local client resizes. A local resize
	// emits no control-stream event, so poll (cheaply — see watchLocalClient);
	// teardown closes stopWatch and removes the hook registered above.
	nudged := func() (time.Time, bool) {
		fi, err := os.Stat(nudgePath)
		if err != nil {
			return time.Time{}, false
		}
		return fi.ModTime(), true
	}
	// The same tick drives the viewing-identity re-resolve (R10), through the
	// resolveView built above.
	ticker := time.NewTicker(resizePollInterval)
	go func() {
		defer ticker.Stop()
		watchLocalClient(cfg.LocalArea, nudged, func() string { return localActiveWindow(cfg) }, resolveView, cfg.View, cfg.RemoteSession, reg, cv, sendCtl, stopWatch, ticker.C)
	}()

	// The reveal watcher polls this session's own clients for a window a
	// local attach, switch-client or window switch just started displaying,
	// so reseedRevealed (main loop) can re-seed a mirror pane whose retained
	// kitty store never survives tmux's own reveal repaint (#731) — see
	// reveal.go. Skipped when there is no local session to poll, same as
	// registerResizeHook above.
	if cfg.LocalSess != "" {
		isMirror := func(localWin string) bool {
			for _, mw := range reg.all() {
				if mw.localWin == localWin {
					return true
				}
			}
			return false
		}
		revealTicker := time.NewTicker(resizePollInterval)
		go func() {
			defer revealTicker.Stop()
			watchReveal(func() (string, error) { return cfg.LocalTmuxOut(clientViewsArgs(cfg.LocalSess)...) }, isMirror, reveals, func() bool { return sendCtl(wakeCmd(cfg.RemoteSession)) }, stopWatch, revealTicker.C)
		}()
	}

	// Ship the remote's agent state into the local claude-status tree, its
	// window labels onto the mirror windows as @bridge_* options, and the remote
	// session's own CPU/mem figures onto the mirror session.
	skew := remoteClockSkew(rt)
	agents = newAgentShipper(cfg.LocalSess, skew)
	labels = newLabelShipper()
	res = newResShipper(pin.id, skew)
	// Subscriptions are per control client, so this runs once per attach — here
	// for the first one, and at the end of repair for every reconnect. The two
	// shippers with a poll mode keep polling if the remote refuses; res has none,
	// and could not trust the answer anyway — a spec tmux cannot parse is dropped
	// with no %error.
	subscribe := func() { labels.subscribed, agents.subscribed, _ = subscribeFormats(rt) }
	subscribe()
	// Session-lifetime like the tick: a sweeper built per attach would restart
	// its floor on every reconnect.
	sweeper := &windowSweeper{}

	// dispatch handles one notification, whether it came straight off the stream
	// or a reply reader queued it while awaiting a reply. It reports whether the
	// bridge is finished. Only the main loop calls it: several branches run their
	// own round-trips, which must not nest inside another.
	dispatch := func(l controlmode.Line) (done bool) {
		switch l.Kind {
		case controlmode.Output:
			router.Route(l.Pane, l.Data)
		case controlmode.LayoutChange:
			if len(l.Args) > 0 {
				if mw, ok := reg.byRemoteID(l.Args[0]); ok {
					if reconcileLayoutFrom(cfg, mw, l, send, router, waitHellosFn, cst, cv, rt) {
						retireMirror(cfg, send, router, waitHellosFn, cst, reg, cv, rt, l.Args[0])
					}
				}
			}
		case controlmode.WindowRenamed:
			if len(l.Args) > 0 {
				if mw, ok := reg.byRemoteID(l.Args[0]); ok {
					applyMirrorName(cfg, mw.localWin, string(l.Data))
					cfg.reflow()
				}
			}
		case controlmode.SessionChanged:
			pin.apply(l, reg, router, rt)
		case controlmode.SessionWindowChanged:
			if argv, ok := translateWindowNotification(l, reg); ok {
				cfg.LocalTmux(argv...)
			}
		case controlmode.WindowAdd:
			if len(l.Args) > 0 {
				addWindow(cfg, send, router, waitHellosFn, cst, reg, cv, rt, l.Args[0])
			}
		case controlmode.WindowClose:
			if len(l.Args) > 0 {
				closeWindow(cfg, router, cst, reg, cv, l.Args[0])
				return reg.empty()
			}
		case controlmode.WindowPaneChanged:
			// The remote's active pane moved. The echo guards decide whether this
			// is our own select-pane coming back or a genuine external change that
			// local focus must follow (focus.go).
			if len(l.Args) > 1 {
				if mw, ok := reg.byRemoteID(l.Args[0]); ok {
					if pane, follow := cst.applyRemoteFocus(mw.remoteID, l.Args[1]); follow {
						focusLocalPane(cfg, cst, mw, mw.remotePanes, pane)
					}
				}
			}
		case controlmode.SubscriptionChanged:
			// Queued, not applied: the loop coalesces a burst — see
			// queuedApplyDue.
			if v, ok := subscriptionValue(l, labelSubName); ok {
				labels.queue(v)
			}
			if v, ok := subscriptionValue(l, agentSubName); ok {
				agents.queue(v)
			}
			if v, ok := subscriptionValue(l, resSubName); ok && len(l.Args) > 1 {
				res.queue(l.Args[1], v)
			}
		case controlmode.Pause:
			if len(l.Args) > 0 {
				handlePause(router, send, l.Args[0])
			}
		case controlmode.Continue:
			if len(l.Args) > 0 {
				handleContinue(router, rt, l.Args[0])
			}
		case controlmode.Exit:
			return true
		}
		return false
	}

	// settle runs the queued notifications and the reconcile intents a ctl
	// request registered, until neither has anything left: each dispatch and each
	// reconcile does round-trips of its own, which can queue more of both. It
	// takes the connection rather than reaching through hold, because the queue
	// it drains is the one this connection's reply readers fill.
	settle := func(c *ctlConn) (done bool) {
		for {
			queued := c.async.take()
			wantWindows, layouts := cst.takeIntents()
			if len(queued) == 0 && !wantWindows && len(layouts) == 0 {
				return false
			}
			for _, q := range coalesceLayoutChanges(queued) {
				if dispatch(q) {
					return true
				}
			}
			if wantWindows {
				reconcileWindows(cfg, send, router, waitHellosFn, cst, reg, cv, rt)
				if reg.empty() {
					return true
				}
			}
			for _, remoteID := range layouts {
				// A layout intent for a window reconcileWindows just closed has
				// nothing to reconcile.
				if mw, ok := reg.byRemoteID(remoteID); ok {
					if reconcileLayout(cfg, mw, send, router, waitHellosFn, cst, cv, rt) {
						retireMirror(cfg, send, router, waitHellosFn, cst, reg, cv, rt, remoteID)
					}
				}
			}
		}
	}

	// runConn is the main loop for one control connection, from a live attach to
	// whichever of the endings finishes it.
	runConn := func(c *ctlConn) connVerdict {
		// pause-after is per control client, so a fresh connection has never
		// been told; re-armed here rather than in repair for the reason the
		// send site below documents.
		pauseAfterSet := false
		for {
			// Settling before the blocking read is what makes a ctl gesture land
			// without a timer: nextLine wakes on any line, and by the time it
			// returns the intent is already registered, so the next pass through here
			// drains it. It also picks up whatever window setup queued.
			if settle(c) {
				return connEnd
			}
			// Same wake-up for an agent, which redraws its pane before it changes
			// state; a window option carries no such traffic, which is what the
			// tick below is for.
			gen := reg.gen()
			// A subscription snapshot arrives as one notification per object and
			// this loop runs a pass per line, so the shippers hold their queued
			// rows while more lines are already buffered — see queuedApplyDue.
			drained := len(c.pump.lines) == 0
			agents.flush(cfg, rt, gen, drained)
			labels.flush(cfg, reg, rt, gen, drained)
			res.flush(cfg)
			sweeper.sweep(cfg, send, router, waitHellosFn, cst, reg, cv, rt)
			reseedDropped(router, rt)
			reseedReshaped(router, rt)
			reseedRevealed(reg, router, rt, reveals)
			// Enable pause-after only now that every window is set up. Setup does
			// drain the stream (its round-trips route, and so does the hello wait),
			// but only dispatch runs handlePause — so a %pause arriving mid-setup is
			// merely queued, and its pane would sit paused with no %continue re-seed
			// until setup finished (a deadlock offline bats can't catch). A
			// reattach is a setup pass too, hence the send site being here and not
			// in repair.
			if !pauseAfterSet {
				pauseAfterSet = true
				if cfg.PauseAfterSecs > 0 {
					send(fmt.Sprintf("refresh-client -f pause-after=%d", cfg.PauseAfterSecs))
				}
			}
			select {
			case l, ok := <-c.pump.lines:
				if !ok {
					return connDrop // control-stream EOF, with no %exit before it
				}
				// Every line taken off this channel must claim its ordinal or the
				// count falls behind sent and no later round-trip recognises its own
				// reply — the guarantee nextLine gives the other readers, and what
				// waitHellos does explicitly for the same reason.
				claimSeq(l, c.st)
				if dispatch(l) {
					return connEnd
				}
			case hc := <-connCh:
				// waitHellos is not running here; a hello is a renderer that
				// redialed (bare respawn-pane keeps argv and reconnects).
				rebindRenderer(cfg, hc, send, router, reg, rt)
			case <-carousel.C():
				// A carousel press that found no images changes nothing the
				// mirror can see, so its own reply block is the last thing
				// that would wake this loop — hence a timer of its own rather
				// than waiting out mainLoopTickInterval to tell the user.
				carousel.poll(cfg, rt, sendCtl)
			case <-loopTick.C:
				// A remote window-option change produces no stream traffic at all,
				// so falling through to the top is the only thing that polls it.
				//
				// The same tick asks the one liveness question none of the other
				// endings covers: is the LOCAL mirror session still there? The
				// registry's window ids are remote and all still present, and the
				// control connection is healthy, so a session that has permanently
				// gone reads identically to a transient blip and the daemon runs
				// (keeping its control client, and so the remote's per-window size
				// clamp, in force) forever (#680). Two consecutive definite
				// negatives, so a single spurious one cannot take a healthy mirror
				// down; localSessionGone already refuses a question that could not
				// be asked.
				if sessionGone.observe(localSessionGone(cfg)) {
					localSessionVanished = true
					return connEnd
				}
			case <-replacer.C():
				// The gesture that raised this deliberately sends no command of
				// its own (R6), so nothing else would bring the loop back here
				// before mainLoopTickInterval — every other ctl request rides
				// its own reply block back. The replacement itself runs in the
				// attach loop, the only place a round-trip may run.
				return connReplace
			case <-death.C():
				// deathSweepDelay has now elapsed since the first connection close in
				// this batch, giving tmux time to settle pane_dead. force() ensures
				// the sweep below actually runs this pass instead of being floored by
				// windowSweepInterval — a bare wake-up with no force can be silently
				// swallowed by that floor, which would leave this no faster than the
				// mainLoopTickInterval backstop it exists to shortcut.
				death.fired()
				sweeper.force()
			}
		}
	}

	// repair brings the mirror back to remote ground truth after a re-attach,
	// reporting whether it still stands. Every step runs on the main-loop
	// goroutine, the only place a round-trip may run, and the order is
	// load-bearing throughout — see the design spec.
	//
	// Load-bearing, but not exclusive: reattach publishes the connection before
	// calling this, so a watchLocalClient tick can land in the same converger slots
	// mid-pass. Tolerated — cv.reset can only discard a fact this pass re-asserts
	// anyway — and nothing drains the pump until the first round-trip below,
	// which follows the resume loop.
	repair := func() bool {
		// The converger caches what THIS control client told the remote, and the
		// fresh one has told it nothing. Reset wholesale rather than invalidating
		// a key: only setupWindow and watchLocalClient write it, and neither runs for
		// a window that survived the outage, so nothing else would re-assert
		// those per-window caps. watchLocalClient also records before it sends, so a
		// local resize during the outage left the converger believing a size the
		// remote was never told — carried across, it is not merely stale but
		// actively wrong, and every symptom is a silently 80-column mirror.
		cv.reset()
		// Every step below is serialized ssh round-trips on this goroutine, so
		// the window the user is looking at goes first in each of them (#557).
		activeWin := localActiveWindow(cfg)
		w, h := cfg.LocalArea()
		if w > 0 && h > 0 && cv.need(clientSizeKey, w, h) && !sendCtl(ClientSizeCmd(w, h)) {
			cv.unrecord(clientSizeKey, w, h)
		}
		// These draw %error blocks for windows that died during the outage, since
		// reconcileWindows has not pruned them yet. Inert, and deliberately so:
		// they are fire-and-forget sends, and claimSeq claims End *or* Error
		// carrying ClientCommandFlag, so the ordinals stay exact. Do not route
		// them through a round-trip that treats Kind == Error as fatal.
		for _, remoteID := range activeFirst(reg, activeWin, reg.remoteIDs()) {
			if cv.need(remoteID, w, h) && !sendCtl(ConvergeCmd(remoteID, w, h)) {
				cv.unrecord(remoteID, w, h)
			}
		}
		// %pause is per-control-client state: the new client will never send the
		// paired %continue, so a sink left paused drops every frame forever —
		// and takeDirty skips a paused sink, putting it beyond reseedDropped's
		// reach too. Resume before the reseed below is enqueued into it.
		for _, mw := range reg.all() {
			for _, id := range mw.remotePanes {
				if s := router.sink(id); s != nil {
					s.resume()
				}
			}
		}
		reconcileWindows(cfg, send, router, waitHellosFn, cst, reg, cv, rt)
		if reg.empty() {
			return false
		}
		// Asked outright here, where the live path asks only of a pass that
		// already failed (#487): an outage is the one stretch in which a local
		// window can die with no %layout-change to discover it on, since the
		// stream those arrive on is down and the remote need never touch that
		// window again. Short-circuited before reconcileLayout so a doomed pass
		// does not spray commands at a window that is already gone.
		//
		// Iterated by id rather than over reg.all(): retireMirror reconciles the
		// whole registry, so a *mirrorWindow taken before it ran may no longer
		// be the entry for that remote window.
		for _, remoteID := range activeFirst(reg, activeWin, reg.remoteIDs()) {
			mw, ok := reg.byRemoteID(remoteID)
			if !ok {
				continue
			}
			if localWindowGone(cfg, mw.localWin) ||
				reconcileLayout(cfg, mw, send, router, waitHellosFn, cst, cv, rt) {
				retireMirror(cfg, send, router, waitHellosFn, cst, reg, cv, rt, remoteID)
			}
		}
		if reg.empty() {
			return false
		}
		// reconcileLayout early-returns on an unchanged layout, so it cannot be
		// relied on for the repaint; and output produced while disconnected was
		// dropped by the remote, not buffered.
		reseedPanes(reg, router, rt, activeWin, "after reattach")
		skew := remoteClockSkew(rt)
		agents.reskew(skew)
		res.reskew(skew)
		// Before the re-subscribe below, whose re-report is the only thing that
		// puts the figures back: reattach dropped the stamp, and a shipper that
		// still remembered writing it would suppress the write as unchanged.
		res.reset()
		// Last, and after the registry has settled: the fresh client carries no
		// subscriptions, and re-subscribing re-reports every window and pane —
		// so this doubles as the label/agent-state half of the repair.
		subscribe()
		// R5's second half: the remote session's environment table survives
		// the outage (same server — see newSessionPin above), so the only gap
		// this closes is a capability change that happened WHILE disconnected
		// — watchLocalClient's own immediate publish had no live connection to
		// send it on. Re-sent unconditionally, same as the one-shot at Run()'s
		// own startup and for the same reason: a stale value must not stand.
		sendCtl(RelayEnvCmd(cfg.RemoteSession, cfg.View.Relay.Load().String()))
		return true
	}

	// Session-lifetime, not per attach: runConn selects on it, but a ticker built
	// per attach would leak one per reconnect, and teardown can only stop the
	// handle it can see.
	loopTick = time.NewTicker(mainLoopTickInterval)

attach:
	for {
		switch runConn(c) {
		case connReplace:
			next, outcome := replaceConn(cfg, router, hold, pin.identity, reg, repair)
			// After replaceConn returns, on every outcome: the Advertised
			// write happens at its publish point, so clearing the flag any
			// earlier would let a press in that window read the stale value
			// and raise a second, redundant dial and repair().
			replacer.done()
			switch outcome {
			case replaced:
				c = next
			case notReplaced:
				// Nothing was closed — c is still the mirror's connection, so
				// re-enter the loop on it and the gesture merely cost a dial.
			case mirrorGone:
				break attach
			}
		case connDrop:
			// A raise that lost runConn's select to this drop is moot, and
			// leaving it queued costs a redundant dial and reseed right after
			// the outage's own — see viewReplacer.cancel.
			replacer.cancel()
			if !reconnect {
				break attach
			}
			if c = reattach(cfg, router, hold, pin.identity, repair); c == nil {
				break attach
			}
		default:
			// connEnd: the remote ended this control client, the local mirror
			// session is gone, or the mirror was left with no windows — either way
			// there is nothing to re-dial into.
			break attach
		}
	}
	teardown()
	return nil
}

// setupWindow runs the per-window plan/spawn/hello/seed pipeline for mw: it
// reads the remote window's layout, shapes mw.localWin to match, spawns one
// renderer per pane, waits for their Hellos, then seeds each and wires it into
// the router. It records the remote pane ids and their conns on mw.
//
// For a 1-pane remote window this is exactly M1's behavior — no split, one
// renderer, matching dims — since PlanWindow emits zero splits for a 1-pane
// layout.
func setupWindow(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, mw *mirrorWindow, cv *converger, rt roundTrip) error {
	mw.spawned = false
	// Cap the remote window at what the local clients can show before reading
	// its layout, so the layout that gets mirrored is the converged one. The
	// opt-out first, and unconditionally: it is a property of the window's whole
	// life, not of a size, so a re-setup of a window whose size the converger
	// already records must still assert it.
	send(AggressiveResizeOffCmd(mw.remoteID))
	// Same shape and the same reason to be unconditional: a property of the
	// window's whole life, and the only client the mirrored session has is a
	// control client, which no visibility test passes (#529).
	send(PassthroughAllCmd(mw.remoteID))
	// reg.add publishes the window before this runs, so watchLocalClient can already
	// have capped it — a cap tmux discarded, since the opt-out above had not
	// landed yet, and which cv.need would then read as asserted. Changing a
	// window's sizing eligibility invalidates any record made against it by
	// definition: whatever was recorded was recorded against a window that could
	// not accept it.
	cv.forget(mw.remoteID)
	if w, h := cfg.LocalArea(); cv.need(mw.remoteID, w, h) {
		if _, ok := one(rt, ConvergeCmd(mw.remoteID, w, h)); !ok {
			cv.unrecord(mw.remoteID, w, h)
		}
	}

	L, remoteActive, zoomed, err := readLayout(rt, remoteWinTarget(cfg, mw.remoteID))
	if err != nil {
		return err
	}

	// Apply the mirror shape to the local window.
	for _, c := range PlanWindow(mw.localWin, L) {
		if err := cfg.LocalTmux(c...); err != nil {
			return fmt.Errorf("daemon: apply mirror for %s: %w", mw.remoteID, err)
		}
	}

	mw.remotePanes = RemotePaneOrder(L)
	mw.layout = L.Raw // PlanWindow's last command is this select-layout
	cst.setWindowPanes(mw.remoteID, mw.remotePanes)

	// PlanWindow's splits create panes in RemotePaneOrder position (see
	// mirror.go), so the tiled list lines up index-for-index with remotePanes.
	if err := refreshLocalPanes(cfg, mw); err != nil {
		return fmt.Errorf("daemon: mirror panes for %s: %w", mw.remoteID, err)
	}
	if assertMirrorZoom(cfg, mw, zoomed, remoteActive, mw.remotePanes) {
		mw.appliedZoom = zoomed
	}
	if len(mw.localPanes) != len(mw.remotePanes) {
		return fmt.Errorf("daemon: mirror for %s: %d local panes for %d remote",
			mw.remoteID, len(mw.localPanes), len(mw.remotePanes))
	}
	// Past here respawn-pane -k has killed any kept pane's old renderer; a
	// mid-spawn failure must not merge that dead conn back.
	mw.spawned = true
	for i, remotePane := range mw.remotePanes {
		if err := spawnRenderer(cfg, mw.localPanes[i], remotePane); err != nil {
			return fmt.Errorf("daemon: spawn renderer for %s: %w", remotePane, err)
		}
	}

	// Collect a hello for every remote pane (any order) before seeding —
	// seeding is sequential over the single control stream, so all renderers
	// must be connected (and hence writable) first.
	byRemote, err := waitHellos(mw.remotePanes)
	if err != nil {
		return err
	}
	for id, c := range byRemote {
		mw.conns[id] = c
	}

	// Seed every connected pane in one batch. Panes that never hello'd are
	// filtered out rather than skipped in the loop, so no command is issued for
	// a pane nobody will wire; idxs carries each batch entry back to its
	// position in remotePanes, which is the index space L.Panes uses.
	paneIDs := make([]string, 0, len(mw.remotePanes))
	idxs := make([]int, 0, len(mw.remotePanes))
	for i, remotePane := range mw.remotePanes {
		if mw.conns[remotePane] == nil {
			continue // didn't connect; the hello wait reported the shortfall
		}
		paneIDs = append(paneIDs, remotePane)
		idxs = append(idxs, i)
	}
	wired := make([]bool, len(paneIDs))
	PaneSeeds(rt, paneIDs, func(i int, seed []byte, err error) {
		idx := idxs[i]
		remotePane := paneIDs[i]
		wired[i] = wireRenderer(router, mw.conns[remotePane], remotePane, seed, err,
			L.Panes[idx], cfg.graphicsFor(remotePane))
	})

	for i, remotePane := range paneIDs {
		if wired[i] {
			go pumpInput(mw.conns[remotePane], remotePane, send, cfg.paster(), cfg.RendererDied)
			continue
		}
		// A sole pane's failure is fatal: this error is what makes addWindow /
		// mirrorNewWindow tear the half-created mirror window down instead of
		// leaving a blank one behind a live registry entry.
		if len(mw.remotePanes) == 1 {
			router.Unregister(remotePane)
			mw.conns[remotePane].Close()
			delete(mw.conns, remotePane)
			return fmt.Errorf("daemon: seed failed for sole pane %s", remotePane)
		}
		go pumpInput(mw.conns[remotePane], remotePane, send, cfg.paster(), cfg.RendererDied)
	}

	// A window that already holds a float when the bridge opens mirrors it now
	// rather than waiting for an unrelated %layout-change.
	reconcileFloats(cfg, mw, L, send, router, waitHellos, rt)
	// The setWindowPanes above asserted the TILED set, before any float
	// existed, and setWindowPanes clears every pane mapped to the window before
	// re-setting — so the float-inclusive set has to be asserted after the
	// floats are created. Without it parseCtl cannot map a float's pane to a
	// window and refuses the first keybind pressed inside one, including the
	// focus ctl after-select-pane fires on a mere click, with a visible
	// --display-error banner.
	cst.setWindowPanes(mw.remoteID, mw.allRemotePanes())
	return nil
}

// addWindow B2-confirms a %window-add notification with a list-windows re-read.
// If remoteID is now in the bridged session and not already mirrored, it runs
// the same plan/spawn/hello/seed pipeline as startup. Otherwise (the window
// belongs elsewhere, or a duplicate notification for an already-registered
// window) it's a no-op.
func addWindow(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip, remoteID string) {
	if _, already := reg.byRemoteID(remoteID); already {
		return
	}

	lw, ok := one(rt, fmt.Sprintf("list-windows -t %s -F %s", tmuxQuote(cfg.RemoteSession), windowListFormat))
	if !ok || lw.Kind == controlmode.Error {
		fmt.Fprintf(os.Stderr, "daemon: window-add %s: list-windows failed\n", remoteID)
		return
	}
	inSession := false
	var addedName string
	for _, rw := range parseWindowList(string(lw.Data)) {
		if rw.id == remoteID {
			inSession = true
			addedName = rw.name
			break
		}
	}
	if !inSession {
		return
	}

	localWin, err := createMirrorWindow(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: window-add %s: %v\n", remoteID, err)
		return
	}
	stampMirrorWindow(cfg, localWin, addedName)
	mw := reg.add(remoteID, localWin)
	if err := setupWindow(cfg, send, router, waitHellos, cst, mw, cv, rt); err != nil {
		// Drop the half-created entry + local window so the already-registered
		// guard doesn't block a later %window-add retry for this id.
		fmt.Fprintf(os.Stderr, "daemon: window-add %s: %v\n", remoteID, err)
		reg.remove(remoteID)
		cv.forget(remoteID)
		cfg.LocalTmux("kill-window", "-t", localWin)
		return
	}
	cfg.reflow()
}

// closeWindow tears down remoteID's local mirror: unregisters (and thereby
// closes) each pane's output sink, closes each renderer conn, and kills the
// local window. A notification for a window outside the registry is a no-op
// (B2) — kill-window must never run against a window this daemon doesn't own.
func closeWindow(cfg Config, router *Router, cst *ctlState, reg *registry, cv *converger, remoteID string) {
	mw, ok := reg.remove(remoteID)
	if !ok {
		return
	}
	cv.forget(remoteID)
	cst.forgetWindow(remoteID)
	for _, id := range mw.allRemotePanes() {
		router.Unregister(id)
	}
	for _, c := range mw.conns {
		c.Close()
	}
	cfg.LocalTmux("kill-window", "-t", mw.localWin)
}

// retireMirror drops the mirror for a remote window whose LOCAL window is gone,
// then rebuilds it from the remote's own window list. The remote window is
// still there — only this side's rendering of it died — so leaving the entry
// retired would silently lose a window the remote still has.
//
// Teardown reuses closeWindow: its trailing kill-window against an
// already-dead window is a no-op, and every other step (registry, converger,
// ctl state, sinks, conns) has to happen either way. The rebuild goes through
// reconcileWindows rather than a create here, so the replacement is made by
// mirrorNewWindow — the one path that stamps @bridge_win, names the window from
// the remote's own name, and rolls back a half-built mirror.
func retireMirror(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip, remoteID string) {
	closeWindow(cfg, router, cst, reg, cv, remoteID)
	reconcileWindows(cfg, send, router, waitHellos, cst, reg, cv, rt)
}

// asyncQueue holds the notifications a reply reader met while awaiting a reply.
// Only the main loop dispatches them: %window-add and friends run their own
// round-trips, which must not execute reentrantly from inside one. It needs no
// mutex — the main loop's goroutine is the only one that touches it, directly or
// through the reply readers it calls.
type asyncQueue struct{ lines []controlmode.Line }

func (q *asyncQueue) push(l controlmode.Line) { q.lines = append(q.lines, l) }

func (q *asyncQueue) take() []controlmode.Line {
	lines := q.lines
	q.lines = nil
	return lines
}

// coalesceLayoutChanges collapses a burst of %layout-change notifications for
// the same window into just the last one. reconcileLayoutFrom reads the
// surviving line's own Args, so only the last notification for a given window
// in an already-buffered batch can still matter — a resize drag can otherwise
// queue many of these while a single reconcile call's own round-trips are in
// flight, and dispatching each one individually would pay for N-1 redundant
// readLayout round-trips before reconcileLayoutFrom's own gates, or
// reconcileLayout's dedup behind them, even get a chance to discard them.
// Every other notification kind, and the relative order of what survives, is
// left untouched.
func coalesceLayoutChanges(lines []controlmode.Line) []controlmode.Line {
	last := map[string]int{}
	for i, l := range lines {
		if l.Kind == controlmode.LayoutChange && len(l.Args) > 0 {
			last[l.Args[0]] = i
		}
	}
	out := make([]controlmode.Line, 0, len(lines))
	for i, l := range lines {
		if l.Kind == controlmode.LayoutChange && len(l.Args) > 0 && last[l.Args[0]] != i {
			continue
		}
		out = append(out, l)
	}
	return out
}

// lineReader is the control stream as its consumers see it: one blocking read
// that ends with the stream. Both *controlmode.Reader and *ctlPump satisfy it.
type lineReader interface {
	Next() (controlmode.Line, bool)
}

// ctlPumpBuf is the depth of the pump's line channel. It is slack for every
// stretch where the consuming goroutine is busy rather than reading — LocalTmux
// execs, window shaping, per-pane seeding, the hello wait — which is what keeps
// the remote's output moving out of the socket and so keeps a pane below tmux's
// pause-after age. Deliberately looser than the one-line-at-a-time backpressure
// a synchronous reader gave: the slack IS the fix. Once it is full the pump
// blocks on the send and the remote feels the stall as it always did.
//
// The bound is a line count, so what it costs is 256 × one control-mode line —
// an %output chunk, or a reply block's joined body. Both are small in practice;
// the scanner's 4MB ceiling sizes a pathological reply body, not a routine one.
const ctlPumpBuf = 256

// ctlPump is the daemon's one caller of controlmode.Reader.Next() for the life
// of one control connection; a reconnect builds a new reader and a new pump
// beside it (#482). Reading on a goroutine of its own is what lets a consumer
// select over the stream alongside other events (see waitHellos) — Next()
// blocks, so a select cannot include it directly.
//
// The goroutine ends when the stream does, and otherwise dies with the process:
// closing the connection closes only the ssh stdin, so a pump parked on a send
// to a channel nobody is draining is not woken by it. Harmless — the reader of
// a connection the daemon has moved on from has nothing left to deliver.
type ctlPump struct {
	lines chan controlmode.Line
}

func startCtlPump(rd *controlmode.Reader) *ctlPump {
	p := &ctlPump{lines: make(chan controlmode.Line, ctlPumpBuf)}
	go func() {
		defer close(p.lines)
		for {
			l, ok := rd.Next()
			if !ok {
				return
			}
			p.lines <- l
		}
	}()
	return p
}

func (p *ctlPump) Next() (controlmode.Line, bool) {
	l, ok := <-p.lines
	return l, ok
}

// claimSeq gives a reply block the ordinal of the command it answers, and 0 to
// everything else. Every line a consumer takes off the stream must pass through
// here so the count stays exact: most of our commands are fire-and-forget and
// their reply blocks are consumed by the main loop, so counting only inside
// round-trips would let seen fall behind sent and no round-trip would ever
// recognise its reply again.
func claimSeq(l controlmode.Line, st *stream) uint64 {
	// A block flagged 0 answers a command we never sent, so it takes no ordinal.
	if (l.Kind == controlmode.End || l.Kind == controlmode.Error) && l.Flags == controlmode.ClientCommandFlag {
		return st.claim(l.Data)
	}
	return 0
}

// handleAsideLine disposes of a line nobody is waiting for. %output is routed as
// it goes past, so a wait for one pane never drops live output for another (B3);
// every other notification is queued for the main loop rather than dropped,
// since a swallowed %pause leaves its pane paused on the remote with no
// %continue ever answered. A reply block is dropped whatever its ordinal: it
// answers a command whose caller has already moved on.
func handleAsideLine(l controlmode.Line, router *Router, async *asyncQueue) {
	switch l.Kind {
	case controlmode.End, controlmode.Error:
	case controlmode.Output:
		router.Route(l.Pane, l.Data)
	case controlmode.Other:
	default:
		async.push(l)
	}
}

// nextLine reads one line and claims its ordinal. For a reply block one of our
// own commands produced it returns that command's ordinal; seq is 0 for
// everything else.
func nextLine(reader lineReader, st *stream) (l controlmode.Line, seq uint64, ok bool) {
	l, ok = reader.Next()
	if !ok {
		return controlmode.Line{}, 0, false
	}
	return l, claimSeq(l, st), true
}

// readReplyRouting returns the reply block to command number want, passing every
// other line to handleAsideLine.
func readReplyRouting(reader lineReader, router *Router, async *asyncQueue, st *stream, want uint64) (controlmode.Line, bool) {
	for {
		l, seq, ok := nextLine(reader, st)
		if !ok {
			return controlmode.Line{}, false
		}
		if (l.Kind == controlmode.End || l.Kind == controlmode.Error) && seq == want {
			return l, true
		}
		handleAsideLine(l, router, async)
	}
}

// handlePause answers a %pause %N: mark the pane's sink paused (Write drops
// output while paused) and ask tmux to unblock it with a paired %continue,
// which the main loop turns into a full-repaint re-seed.
func handlePause(router *Router, send func(string), paneID string) {
	if s := router.sink(paneID); s != nil {
		s.pause()
		send(fmt.Sprintf("refresh-client -A '%s:continue'", paneID))
	}
}

// handleContinue answers a %continue %N: capture a fresh screen (routing-aware,
// so sibling panes keep streaming during the round-trip — B3) and enqueue it as
// a FrameSeed BEFORE resuming, so the full repaint lands ahead of any resumed
// output and closes the %pause gap.
func handleContinue(router *Router, rt roundTrip, paneID string) {
	s := router.sink(paneID)
	if s == nil {
		return
	}
	if seed, err := PaneSeed(rt, paneID); err == nil {
		enqueueSeedWithReplay(s, seed)
	} else {
		fmt.Fprintf(os.Stderr, "daemon: %%continue reseed for %s: %v\n", paneID, err)
	}
	s.resume()
}

// reseedDropped repaints every pane that lost frames to a full buffer.
//
// The drop itself is deliberate: blocking the control-stream loop on one
// stalled renderer would stall every other pane with it. But terminal output is
// positional, so a frame lost mid-repaint leaves those cells wrong until
// something happens to overwrite them — for an agent pane that has just
// finished a turn, that can be a very long time, and what the human sees is
// debris that never clears (#412). capture-pane is ground truth, so the re-seed
// takes the debris with it.
//
// Called from the main loop, which is the only place a round-trip may run, and
// reached without a timer for the same reason everything else here is: the
// output that caused the drop has already woken the loop.
func reseedDropped(router *Router, rt roundTrip) {
	dirty := router.dirtyPanes()
	ids := make([]string, 0, len(dirty))
	sinks := make([]*outputSink, 0, len(dirty))
	for _, paneID := range dirty {
		if s := router.sink(paneID); s != nil {
			ids = append(ids, paneID)
			sinks = append(sinks, s)
		}
	}
	PaneSeeds(rt, ids, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: re-seed after drop for %s: %v\n", ids[i], err)
			return
		}
		enqueueSeedWithReplay(sinks[i], seed)
	})
}

// reseedReshaped repaints every pane whose confirmation re-seed has come due —
// the second half of markReshaped's story, and the half that actually shows the
// user the application's repaint rather than tmux's rewrap of it.
//
// Called from the main loop beside reseedDropped, which is the only place a
// round-trip may run, and reached without a timer for the same reason: the
// repaint that makes the second capture worth taking is itself output, and
// output wakes the loop.
func reseedReshaped(router *Router, rt roundTrip) {
	due := router.reshapedPanes()
	ids := make([]string, 0, len(due))
	sinks := make([]*outputSink, 0, len(due))
	for _, paneID := range due {
		if s := router.sink(paneID); s != nil {
			ids = append(ids, paneID)
			sinks = append(sinks, s)
		}
	}
	PaneSeeds(rt, ids, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: re-seed after reshape for %s: %v\n", ids[i], err)
			return
		}
		enqueueSeedWithReplay(sinks[i], seed)
	})
}

// remoteWinTarget builds the tmux target for a remote window by its id (@N),
// quoting the session name so a name with spaces (e.g. "my proj") stays one
// token. The id is used verbatim — never TrimPrefix'd to a bare N, which tmux
// would read as window INDEX N (a different window).
func remoteWinTarget(cfg Config, remoteID string) string {
	return fmt.Sprintf("%s:%s", tmuxQuote(cfg.RemoteSession), remoteID)
}

// tmuxQuote single-quotes s for a tmux control-mode command line, escaping
// any embedded single quote the tmux-safe way.
func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readLayout reads target's layout string and, in the same round-trip, the
// remote window's active pane id — #{pane_id} in window scope — and whether it
// is zoomed. Layout strings contain no spaces, so one space-separated reply
// carries all three, and every reconcile gets the remote's focus and zoom from
// ground truth instead of a belief.
//
// #{window_layout} is deliberately the UNZOOMED geometry. #{window_visible_layout}
// would report a zoomed window as single-pane, and reconcile would read the
// hidden panes as closed and kill their renderers on every zoom toggle; the
// flag rides alongside instead, and zoom is applied locally as zoom (#413).
func readLayout(rt roundTrip, target string) (l0 controlmode.Layout, active string, zoomed bool, err error) {
	l, ok := one(rt, fmt.Sprintf("display-message -p -t %s -F '#{window_layout} #{pane_id} #{window_zoomed_flag}'", target))
	if !ok {
		return controlmode.Layout{}, "", false, fmt.Errorf("daemon: control connection closed reading layout for %s", target)
	}
	if l.Kind == controlmode.Error {
		return controlmode.Layout{}, "", false, fmt.Errorf("daemon: display-message window_layout -t %s: %s", target, l.Data)
	}
	fields := strings.Fields(string(l.Data))
	if len(fields) == 0 {
		return controlmode.Layout{}, "", false, fmt.Errorf("daemon: empty layout reply for %s", target)
	}
	if len(fields) > 1 {
		active = fields[1]
	}
	zoomed = len(fields) > 2 && fields[2] == "1"
	L, err := controlmode.ParseLayout(fields[0])
	return L, active, zoomed, err
}

// spawnRenderer respawns the local pane target (a %N pane id) with the
// renderer binary, wired to dial back with remotePane's id.
//
// It also stamps the pane's remote id into the @bridge_pane pane option: that
// is the carrier a local keybind reads to tell the daemon which remote pane a
// structural gesture applies to. Pane options survive respawn-pane -k and ride
// with the pane through select-layout and swap-pane (verified), so this is the
// only place it needs writing.
func spawnRenderer(cfg Config, target, remotePane string) error {
	if err := cfg.LocalTmux(append([]string{"respawn-pane", "-k", "-t", target, "--"},
		rendererSpawnArgs(cfg, remotePane)...)...); err != nil {
		return err
	}
	markRendererPane(cfg, target, remotePane)
	return nil
}

// rendererSpawnArgs is the command a renderer pane runs, shared by
// respawn-pane (an existing pane) and split-window (a pane created to run it
// directly, which is how a mirrored split avoids painting a shell first).
// Sock paths are daemon-pid-derived absolute paths and remote pane ids are
// %N, so neither is a tmux flag today; callers still pass -- before this
// slice so a future path cannot become one.
func rendererSpawnArgs(cfg Config, remotePane string) []string {
	return []string{cfg.RendererBin, cfg.SockPath, remotePane}
}

// markRendererPane stamps the reverse mapping a keybind reads to reach the
// remote pane this local one renders, and opts the pane into unrestricted
// passthrough.
//
// allow-passthrough is per pane, and the global stays `on`: with `on`, tmux
// hands a passthrough sequence to a client only while the pane's window is that
// client's current one, and a kitty image store dropped that way is gone —
// tmux stores nothing and never retransmits, while the placeholders naming the
// image are grid text and redraw without it, so the pane paints chrome around
// an empty picture. A mirror carousel would trip that on nearly every open,
// since the split is created by reconcile rather than by the keypress.
//
// `all` here rather than globally because a mirror pane replays a remote host's
// bytes verbatim: the widened reach is confined to the panes that already carry
// remote output by design (#464). It does not help a client attached to a
// *different* session — `all` still requires session_has — which is why #465
// and #468 exist.
//
// Both creation paths reach this function (respawn-pane and the mirrored
// split), and pane options survive respawn-pane -k, select-layout and
// swap-pane, so this is the only place either option needs writing.
func markRendererPane(cfg Config, target, remotePane string) {
	cfg.LocalTmux("set-option", "-p", "-t", target, "@bridge_pane", remotePane)
	cfg.LocalTmux("set-option", "-p", "-t", target, "allow-passthrough", "all")
}

// acceptConns accepts connections on l until it's closed and dispatches each on
// its FIRST frame: a FrameHello is a renderer, delivered to out and kept open for
// the life of its pane; a FrameCtl is a one-shot structural request from a local
// keybind, answered with a FrameCtlAck and closed. Anything else is dropped.
//
// onCtl reports an error to send back in the ack; an empty ack means accepted.
func acceptConns(l net.Listener, out chan<- helloConn, onCtl func(argv []string) error) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			f, err := wire.ReadFrame(conn)
			if err != nil {
				conn.Close()
				return
			}
			switch f.Type {
			case wire.FrameHello:
				out <- helloConn{paneID: string(f.Payload), conn: conn}
			case wire.FrameCtl:
				defer conn.Close()
				msg := ""
				if err := onCtl(wire.DecodeArgv(f.Payload)); err != nil {
					msg = err.Error()
				}
				wire.WriteFrame(conn, wire.FrameCtlAck, []byte(msg))
			default:
				conn.Close()
			}
		}()
	}
}

// waitHellos reads renderer connections off connCh until every id in want has
// announced, keyed by the remote pane id each hello carries, while keeping the
// control stream draining: every mirror path that waits here runs on the
// goroutine that owns the stream, so a wait that only watched connCh would let
// the remote's output back up for its whole duration and tmux would %pause
// every busy pane behind it (#434).
//
// Bounded by timeout so a renderer that never dials back can't wedge the caller
// forever (see helloTimeout); on timeout, or once the stream ends, any
// connections already collected are closed here (nothing else owns them yet)
// and an error is returned.
//
// A second hello for an id already in out replaces the previous conn (closed)
// and does not fill an extra slot. A hello whose pane is not in want is handed
// to unexpected — a reconnect mid-wait — never counted; nil unexpected closes
// it.
func waitHellos(lines <-chan controlmode.Line, router *Router, async *asyncQueue, st *stream, connCh <-chan helloConn, want []string, timeout time.Duration, unexpected func(helloConn)) (map[string]net.Conn, error) {
	wanted := make(map[string]struct{}, len(want))
	for _, id := range want {
		wanted[id] = struct{}{}
	}
	out := map[string]net.Conn{}
	deadline := time.After(timeout)
	for len(out) < len(wanted) {
		select {
		case hc, ok := <-connCh:
			if !ok {
				closeConns(out)
				return nil, fmt.Errorf("daemon: renderer socket closed after %d/%d connections", len(out), len(wanted))
			}
			if _, ok := wanted[hc.paneID]; !ok {
				if unexpected != nil {
					unexpected(hc)
				} else {
					hc.conn.Close()
				}
				continue
			}
			if prev := out[hc.paneID]; prev != nil {
				prev.Close()
			}
			out[hc.paneID] = hc.conn
		case l, ok := <-lines:
			if !ok {
				closeConns(out)
				return nil, fmt.Errorf("daemon: control stream ended after %d/%d connections", len(out), len(wanted))
			}
			claimSeq(l, st)
			handleAsideLine(l, router, async)
		case <-deadline:
			closeConns(out)
			return nil, fmt.Errorf("daemon: timed out after %s waiting for renderers (%d/%d connected)", timeout, len(out), len(wanted))
		}
	}
	return out, nil
}

// rebindRenderer adopts a reconnect hello: a bare respawn-pane re-execs the
// renderer argv and redials, and nothing else would replace the dead sink
// (the pane is live, so heal does not fire). Called from the main loop when
// waitHellos is not running, and from waitHellosFn after the wait returns
// for extras collected mid-wait — never from inside waitHellos, which would
// nest a seed round-trip on the stream reader.
func rebindRenderer(cfg Config, hc helloConn, send func(string), router *Router, reg *registry, rt roundTrip) {
	var mw *mirrorWindow
	for _, w := range reg.all() {
		for _, id := range w.allRemotePanes() {
			if id == hc.paneID {
				mw = w
				break
			}
		}
		if mw != nil {
			break
		}
	}
	if mw == nil {
		hc.conn.Close()
		return
	}
	if old := mw.conns[hc.paneID]; old != nil {
		old.Close()
	}
	mw.conns[hc.paneID] = hc.conn
	router.Unregister(hc.paneID)
	seedRenderer(rt, router, hc.conn, hc.paneID, rendererDims(mw, hc.paneID), cfg.graphicsFor(hc.paneID))
	go pumpInput(hc.conn, hc.paneID, send, cfg.paster(), cfg.RendererDied)
}

func rendererDims(mw *mirrorWindow, paneID string) controlmode.PaneCell {
	if g, ok := mw.floatGeom[paneID]; ok {
		return g
	}
	if mw.layout == "" {
		return controlmode.PaneCell{}
	}
	L, err := controlmode.ParseLayout(mw.layout)
	if err != nil {
		return controlmode.PaneCell{}
	}
	for _, c := range L.Panes {
		if c.ID == paneID {
			return c
		}
	}
	return controlmode.PaneCell{}
}

func closeConns(conns map[string]net.Conn) {
	for _, c := range conns {
		c.Close()
	}
}

// seedRenderer produces the initial screen for remotePane and wires it in.
// The halves are separable so a batched caller can wire each pane from inside
// onSeed instead.
func seedRenderer(rt roundTrip, router *Router, conn net.Conn, remotePane string, dims controlmode.PaneCell, gfx *graphics.Proxy) bool {
	seed, err := PaneSeed(rt, remotePane)
	return wireRenderer(router, conn, remotePane, seed, err, dims, gfx)
}

// wireRenderer registers conn's output sink with router, then enqueues frames
// through that sink. On a successful seed it enqueues FrameSeed followed by
// FrameResize (dims from the pane's layout cell). Register-then-enqueue keeps
// the seed the sink's first frame (FIFO), so it precedes any routed output —
// no frame bypasses the sink (frozen wire invariant).
//
// On seed failure the pane is wired unseeded: only FrameResize is enqueued, the
// conn stays open, and false is returned. For such a pane there is no seed, so
// its sink's first frame may legitimately be a FrameOutput or FrameResize; the
// seed-before-output guarantee binds a seed and the output of the same pane, it
// does not require a seed to exist. The pane starts blank rather than stale.
func wireRenderer(router *Router, conn net.Conn, remotePane string, seed []byte, err error, dims controlmode.PaneCell, gfx *graphics.Proxy) bool {
	sink := newOutputSink(conn, gfx)
	router.Register(remotePane, sink)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: seed %s: %v (wired unseeded; trailing reseed will repair)\n", remotePane, err)
		sink.enqueue(wire.FrameResize, wire.EncodeResize(dims.W, dims.H))
		return false
	}
	sink.enqueue(wire.FrameSeed, seed)
	sink.enqueue(wire.FrameResize, wire.EncodeResize(dims.W, dims.H))
	return true
}

// sinkFrame is a typed daemon->renderer frame queued on an outputSink. Once
// pause-after flow control lets a mid-stream re-seed happen, the seed is a
// second writer of the conn alongside the output pump, so every frame type
// (seed, output, resize) serializes through the one pump goroutine (frozen
// wire invariant: no frame bypasses the sink).
type sinkFrame struct {
	typ     wire.FrameType
	payload []byte
}

// outputSink serializes all daemon->renderer frames for one pane through a
// single pump goroutine so a slow reader can't block Router.Route (which runs
// on the single main control-stream loop) and the seed/resize/output writers
// never race. A full buffer or a paused pane drops the frame; a paused pane's
// state is recovered by the mandatory fresh FrameSeed that every %continue
// enqueues, an overflow's by reseedDropped (dropped, below).
type outputSink struct {
	mu     sync.Mutex
	ch     chan sinkFrame
	gfx    *graphics.Proxy
	closed bool
	paused bool
	// dropped counts frames lost to a full buffer since the last re-seed.
	// Nonzero means this pane's screen no longer follows from what it was
	// sent, and only a re-seed can put that right — terminal output is
	// positional, so the bytes that would have repaired those cells are the
	// ones that went missing.
	dropped int
	// reshaped tracks the confirmation re-seed a pane is owed after its
	// geometry moved; see markReshaped.
	reshaped reshapeState
	// hasImages mirrors gfx.Retained(), written only by the pump after every
	// gfx.Filter call; the main loop's reveal pass reads it to decide whether
	// a revealed pane is worth re-seeding.
	hasImages atomic.Bool
	// done closes when the pump goroutine returns. Close only signals the
	// pump to stop; the pump may still be mid-flush (draining kn/gfx state on
	// teardown) after Close returns. Wait is how a caller that needs to
	// inspect gfx directly — outside the pump's own goroutine confinement —
	// gets a happens-before edge instead of racing that flush.
	done chan struct{}
}

// newOutputSink constructs the sink and starts its pump immediately; see
// start's doc for what the pump does and why it's a separate method.
func newOutputSink(conn net.Conn, gfx *graphics.Proxy) *outputSink {
	s := &outputSink{ch: make(chan sinkFrame, outputSinkBuf), gfx: gfx}
	s.start(conn)
	return s
}

// Wait blocks until the pump goroutine has exited. See done's doc: this is
// the synchronization a caller needs before touching gfx after Close.
func (s *outputSink) Wait() { <-s.done }

// start launches the pump goroutine, which batch-drains queued FrameOutput
// frames before handing them to gfx: coalescing can only drop a store a later
// one supersedes if it can see that later store, and with the proxy on this
// goroutine, fetches are serial per pane, so there's never a second store
// arriving mid-fetch — the only way it sees a burst is by draining what's
// already queued behind the frame it woke on. gfx == nil skips filtering
// entirely (tests, and any transport with no remote filesystem to localise
// from).
//
// start is split from newOutputSink so a test can construct the sink,
// enqueue frames directly onto s.ch, and only then call start —
// guaranteeing the pump's first receive sees the whole burst instead of
// racing its startup against the writer.
func (s *outputSink) start(conn net.Conn) {
	if s.done == nil {
		s.done = make(chan struct{})
	}
	go func() {
		defer close(s.done)
		gfx := s.gfx
		// kn strips the terminal queries a remote pane's occupant asked ITS
		// terminal — key negotiation (#338), cursor position, colours, window
		// size (#544) — before they reach the local mirror pane's pty, where
		// local tmux would otherwise answer them a second time as if the
		// renderer had asked. Unconditional: unlike gfx, this runs regardless
		// of whether graphics localisation is wired in. FrameSeed bypasses it
		// entirely (see the FrameOutput guard below), which is correct: a seed
		// is capture-pane's rendered cells, never a query.
		kn := keyneg.NewFilter()
		var pending *sinkFrame
		for {
			var f sinkFrame
			if pending != nil {
				f, pending = *pending, nil
			} else {
				v, ok := <-s.ch
				if !ok {
					// The pane is gone: flush whatever the proxies were
					// still holding (a partial sequence cut mid-stream)
					// rather than silently dropping it. kn only ever holds
					// the newest unprocessed tail, so its leftover is
					// chronologically after anything gfx is already
					// holding — route it through gfx.Filter first, same as
					// the steady-state order, so the tail comes out in the
					// original byte order.
					tail := kn.Flush()
					if gfx != nil {
						tail = append(gfx.Filter(tail), gfx.Close()...)
					}
					if len(tail) > 0 {
						wire.WriteStream(conn, wire.FrameOutput, tail)
					}
					return
				}
				f = v
			}
			if f.typ == wire.FrameOutput {
				// drainOutput can append more queued FrameOutput frames'
				// raw payload onto buf, and any of those can carry a
				// negotiation sequence too, so kn.Feed runs on the fully
				// drained batch.
				buf := append([]byte(nil), f.payload...)
				buf, pending = drainOutput(s.ch, buf)
				buf = kn.Feed(buf)
				if gfx != nil {
					buf = gfx.Filter(buf)
					s.hasImages.Store(gfx.Retained())
				}
				f.payload = buf
				if len(f.payload) == 0 {
					continue
				}
			}
			if f.typ == wire.FrameSeed && gfx != nil {
				if replay := gfx.Replay(); len(replay) > 0 {
					if err := wire.WriteStream(conn, wire.FrameOutput, replay); err != nil {
						return
					}
				}
			}
			write := wire.WriteFrame
			if f.typ == wire.FrameOutput || f.typ == wire.FrameSeed {
				write = wire.WriteStream
			}
			if err := write(conn, f.typ, f.payload); err != nil {
				return
			}
		}
	}()
}

// drainOutput appends every FrameOutput already queued on ch to buf, so the
// proxy sees a whole burst at once and can drop stores a later frame
// supersedes. It stops at the first non-output frame and hands it back to be
// written next: reordering a seed or resize past output would break the
// frozen wire invariant (sinkFrame's doc above).
func drainOutput(ch chan sinkFrame, buf []byte) ([]byte, *sinkFrame) {
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return buf, nil
			}
			if v.typ != wire.FrameOutput {
				return buf, &v
			}
			buf = append(buf, v.payload...)
		default:
			return buf, nil
		}
	}
}

// writeOwned enqueues p as a FrameOutput without copying. Callers must
// guarantee p is freshly allocated and never retained or mutated after this
// call returns. Non-blocking, like Write: a full buffer or a paused/closed
// sink drops the frame.
func (s *outputSink) writeOwned(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.paused {
		return
	}
	select {
	case s.ch <- sinkFrame{typ: wire.FrameOutput, payload: p}:
	default:
		s.dropped++
	}
}

// Write is the router-facing io.Writer path: it enqueues a FrameOutput. While
// paused, output is dropped (tmux is discarding it remote-side anyway) and
// recovered by the fresh FrameSeed on the paired %continue. A full buffer drops
// the frame too; the pane self-heals on its next %output or the next re-seed.
func (s *outputSink) Write(p []byte) (int, error) {
	s.writeOwned(append([]byte(nil), p...))
	return len(p), nil
}

// enqueue serializes a non-output frame (seed, resize) through the same pump so
// it never races the output writer. It must NOT block: a stalled (not dead)
// renderer with a full buffer would otherwise wedge the control-stream loop, so
// it uses the same bounded non-blocking select + drop as Write.
func (s *outputSink) enqueue(typ wire.FrameType, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- sinkFrame{typ: typ, payload: append([]byte(nil), payload...)}:
	default:
		s.dropped++
	}
}

// enqueueSeedWithReplay enqueues a FrameSeed; the sink pump writes any
// retained kitty stores immediately before the seed (same goroutine as
// gfx.Filter, so Replay stays race-free).
func enqueueSeedWithReplay(s *outputSink, seed []byte) {
	if s == nil {
		return
	}
	s.enqueue(wire.FrameSeed, seed)
}

// reshapeState is the two-step life of a pane's confirmation re-seed: marked on
// the pass that reshaped it, due on the next one. The step is what makes the
// second capture later than the first — see markReshaped.
type reshapeState uint8

const (
	reshapeNone reshapeState = iota
	reshapeMarked
	reshapeDue
)

// markReshaped records that this pane's geometry just moved and its screen was
// repainted from a capture taken at that instant.
//
// That capture is too early to be the last word. tmux rewraps a pane's grid the
// moment it resizes, while the application's own repaint waits on SIGWINCH and
// lands whenever it lands — measured on a live remote, capture-pane immediately
// after an unzoom returns a screen the app replaces ~150ms later. The mirror
// paints what it is given, so the user watches the rewrap until something
// overwrites it.
//
// The repair is one more capture, taken late enough to catch the repaint. Which
// is what the step exists for: takeReshaped only comes due on a LATER main-loop
// pass, and the app's own repaint output is what wakes that pass — so the timing
// needs no timer, exactly as reseedDropped needs none.
func (s *outputSink) markReshaped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.reshaped = reshapeMarked
}

// takeReshaped reports whether this pane is due its confirmation re-seed,
// advancing the mark one step when it is not.
//
// The gates are takeDirty's, for takeDirty's reasons: a paused pane is already
// owed a seed by its %continue, and re-seeding a pane that has not drained is a
// whole extra screen on a queue already behind. Neither spends the mark — it
// waits and comes due on a later pass.
func (s *outputSink) takeReshaped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reshaped == reshapeNone || s.closed || s.paused || len(s.ch) > 0 {
		return false
	}
	if s.reshaped == reshapeMarked {
		s.reshaped = reshapeDue
		return false
	}
	s.reshaped = reshapeNone
	return true
}

// takeDirty reports how many frames this sink dropped, and clears the count, but
// only once the sink has drained: while a pane is still congested a re-seed
// would be dropped in its turn, and it is a whole extra screen on a queue that
// is already behind. A paused pane is left alone too — its %continue owes it a
// seed already.
func (s *outputSink) takeDirty() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped == 0 || s.closed || s.paused || len(s.ch) > 0 {
		return 0, false
	}
	n := s.dropped
	s.dropped = 0
	return n, true
}

func (s *outputSink) pause()  { s.mu.Lock(); s.paused = true; s.mu.Unlock() }
func (s *outputSink) resume() { s.mu.Lock(); s.paused = false; s.mu.Unlock() }

// Close stops the sink's pump goroutine so it doesn't leak once its pane is
// torn down (reconcile-removal, teardown); the channel is otherwise never
// closed and an idle sink would linger until process exit. Safe to call more
// than once, and safe to race with a concurrent Write. Close only asks the
// pump to stop — it does not wait for it; a caller that needs to know gfx has
// gone quiet (there is no other reason to) must Wait too.
func (s *outputSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// pumpInput forwards conn's FrameInput frames to the remote pane as
// send-keys commands, until conn closes. A non-nil paste handler intercepts
// ctrl+v image pastes first (see paste.go); nil forwards input verbatim.
//
// died fires for every connection close, not just crashes — the caller
// (sweeper.sweep, once the debounced timer forces it) re-derives which
// window is actually dead, so a spurious wake costs one no-op forced sweep
// pass, never a wrong repair.
func pumpInput(conn net.Conn, remotePane string, send func(string), paste *pasteHandler, died func()) {
	for {
		f, err := wire.ReadFrame(conn)
		if err != nil {
			if died != nil {
				died()
			}
			return
		}
		if f.Type != wire.FrameInput {
			continue
		}
		payload := f.Payload
		if paste != nil {
			payload = paste.handle(remotePane, payload)
		}
		for _, args := range controlmode.SendKeysArgs(remotePane, payload, controlmode.InputChunkBytes) {
			send(strings.Join(args, " "))
		}
	}
}
