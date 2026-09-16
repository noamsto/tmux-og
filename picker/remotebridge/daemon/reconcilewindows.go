package daemon

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// reconcileWindows re-reads the bridged session's whole window set and makes the
// mirror match: mirror any remote window that has appeared, tear down any that
// has gone, and re-assert the remote name on the ones that stayed.
//
// A local structural gesture does get its remote notification (tmux emits it
// inside the causing command's %begin/%end block, which the reader surfaces),
// but that notification arrives with the mirror mid-gesture. Re-reading ground
// truth covers add, close and rename with one round-trip, and self-heals a
// notification lost for any other reason.
func reconcileWindows(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip) {
	// Every return path reflows: the round-trip below can bail, and Run's
	// startup batch has no other forced reflow — skipping it leaves those
	// windows on the label the after-new-window hook raced in (#196).
	defer cfg.reflow()

	lw, ok := one(rt, fmt.Sprintf("list-windows -t %s -F %s", tmuxQuote(cfg.RemoteSession), windowListFormat))
	if !ok || lw.Kind == controlmode.Error {
		fmt.Fprintf(os.Stderr, "daemon: reconcile-windows: list-windows failed\n")
		return
	}
	remoteWins := parseWindowList(string(lw.Data))
	if len(remoteWins) == 0 {
		// An empty reply is more likely a lost round-trip than a session with no
		// windows (the remote emits %exit for that), and acting on it would kill
		// every mirror window. Leave the mirror alone.
		return
	}

	live := make(map[string]bool, len(remoteWins))
	added := false
	activeRemote := ""
	for _, rw := range remoteWins {
		live[rw.id] = true
		if rw.active {
			activeRemote = rw.id
		}
		mw, known := reg.byRemoteID(rw.id)
		if !known {
			if mirrorNewWindow(cfg, send, router, waitHellos, cst, reg, cv, rt, rw) {
				added = true
			}
			continue
		}
		// Cheap and idempotent: re-assert the name rather than tracking whether it
		// changed, since this is also the rename path.
		applyMirrorName(cfg, mw.localWin, rw.name)
	}

	for _, remoteID := range reg.remoteIDs() {
		if !live[remoteID] {
			closeWindow(cfg, router, cst, reg, cv, remoteID)
		}
	}

	// Follow the remote's selection only when a window appeared. A local
	// `prefix c` makes the new remote window active, and without this the human
	// would stay on the old window while the remote moved; gating on "added"
	// keeps it from fighting ordinary local window navigation.
	if added && activeRemote != "" {
		if mw, ok := reg.byRemoteID(activeRemote); ok {
			cfg.LocalTmux("select-window", "-t", mw.localWin)
		}
	}
}

// mirrorNewWindow creates and wires the local mirror for one remote window,
// reporting whether it succeeded. Shared by reconcileWindows and addWindow.
func mirrorNewWindow(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip, rw remoteWindow) bool {
	localWin, err := createMirrorWindow(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: mirror %s: %v\n", rw.id, err)
		return false
	}
	stampMirrorWindow(cfg, localWin, rw.name)
	mw := reg.add(rw.id, localWin)
	if err := setupWindow(cfg, send, router, waitHellos, cst, mw, cv, rt); err != nil {
		// Drop the half-created entry + local window so a later retry for this id
		// is not blocked by the already-registered guard.
		fmt.Fprintf(os.Stderr, "daemon: mirror %s: %v\n", rw.id, err)
		reg.remove(rw.id)
		cv.forget(rw.id)
		cst.forgetWindow(rw.id)
		cfg.LocalTmux("kill-window", "-t", localWin)
		return false
	}
	return true
}

// healLostWindows retires every mirror whose local window has gone, rebuilding
// it from the remote's own window list.
//
// The reattach sweep asks the same question, but once: localWindowGone answers
// on positive evidence alone, so a read it cannot make leaves the dead entry in
// place, and no other pass re-reads the local window set. Riding the sweep
// bounds a missed retire at one sweep interval rather than the session (#514).
//
// live comes from the caller because the sweep's other pass needs the same
// snapshot (see mirrorPaneRows) and the sweep is budgeted at one local fork.
func healLostWindows(cfg Config, live map[string]bool, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip) {
	// Verdicts first, retires after: retireMirror rebuilds the mirror under a
	// fresh local window, which the listing predates and would read as gone.
	var gone []string
	for _, remoteID := range reg.remoteIDs() {
		if mw, ok := reg.byRemoteID(remoteID); ok && !live[mw.localWin] {
			gone = append(gone, remoteID)
		}
	}
	// By id, re-read each time: retireMirror reconciles the whole registry, so
	// an entry taken before it ran may no longer be the one for that window.
	for _, remoteID := range gone {
		if mw, ok := reg.byRemoteID(remoteID); ok && !live[mw.localWin] {
			retireMirror(cfg, send, router, waitHellos, cst, reg, cv, rt, remoteID)
		}
	}
}

// mirrorPaneListFormat asks tmux for every pane in the mirror session, tagged
// with the window it sits in, whether its command has exited, and the remote
// pane the daemon wired it to render. Pipe-delimited rather than the space
// localPaneListFormat can afford: @bridge_pane is unset on a pane the daemon
// never created, and the empty trailing field has to survive the split.
const mirrorPaneListFormat = "#{window_id}|#{pane_id}|#{pane_dead}|#{@bridge_pane}"

// mirrorPaneRows reads the whole mirror session's panes in one fork, which is
// what both sweep passes run off: the window set healLostWindows needs and the
// dead-renderer set healDeadRenderers needs are two views of one listing, and
// the sweep is budgeted at one local fork per pass (see windowSweepInterval).
// A window always holds at least one pane, so the window ids in this reply are
// the same set list-windows would give.
//
// Positive evidence only, the localWindowGone rule: a listing that cannot be
// made returns ok=false and the sweep does nothing, since retiring or
// rebuilding on a transient read tears down healthy mirrors.
func mirrorPaneRows(cfg Config) (live, deadRenderer map[string]bool, ok bool) {
	if cfg.LocalTmuxOut == nil {
		return nil, nil, false
	}
	out, err := cfg.LocalTmuxOut("list-panes", "-s", "-t", cfg.LocalSess, "-F", mirrorPaneListFormat)
	if err != nil {
		return nil, nil, false
	}
	live, deadRenderer = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 4 || !strings.HasPrefix(f[0], "@") {
			continue
		}
		live[f[0]] = true
		// Keyed on @bridge_pane being set, not on the pane merely being dead. A
		// mirror window can hold a float this daemon never created (prefix +
		// b/k/I, and ^o's remote picker), remain-on-exit keeps that float's
		// corpse too, and it is not ours to reap — rebuilding the window on its
		// death drops the user's float. @bridge_pane is the daemon's own stamp,
		// written only on panes it spawned as renderers, and it survives
		// respawn-pane -k.
		if f[2] == "1" && f[3] != "" {
			deadRenderer[f[0]] = true
		}
	}
	return live, deadRenderer, true
}

// deadRendererStrikes caps how many times one mirror is rebuilt for a dead
// renderer before the sweep gives up on it. A renderer that dies at spawn every
// time — its binary gone from the store under a live session, say — would
// otherwise be rebuilt once per sweep for the life of the daemon. A window left
// bearing a corpse is a worse mirror; a window rebuilt every second is a worse
// session.
const deadRendererStrikes = 3

// deadRendererRecovery bounds how soon a healthy pass after a window's own
// rebuild counts as evidence the renderer is staying up. A pass inside this
// window of the rebuild is that rebuild's own echo — resetWindow closing the
// superseded conns arms the same died() the event path wakes on, not a
// second data point — and mainLoopTickInterval is the sweep's own former
// cadence, so using it as the threshold means the event path can never return
// the budget faster than the sweep it replaces did.
const deadRendererRecovery = mainLoopTickInterval

// healDeadRenderers rebuilds every mirror holding a dead renderer pane.
//
// A renderer's exit is not structural any more — stampMirrorWindow sets
// remain-on-exit on the mirror window, so the pane goes dead instead of
// closing and taking the window (and, for a single-pane mirror, the session)
// with it (#547). That leaves a corpse nothing else will ever notice: a dead
// pane is still a pane to list-panes, so the pane diff reads the local set as
// matching the remote's and reconcile correctly does nothing. This is the pass
// that notices.
//
// resetWindow rather than retireMirror, and rather than respawning the one
// pane. Retiring is what healLostWindows does to a window that is already
// gone, and closeWindow's kill-window would here destroy a live window — which
// for a single-window mirror session takes the session, the very failure this
// is repairing. resetWindow keeps the window and re-runs the whole
// plan/spawn/hello/seed pipeline through setupWindow, which is where the seed
// ordering and the geometry each pane is painted at already live (#233, #412
// and #417 are all bugs in exactly that arithmetic); a per-pane respawn would
// re-derive it out of band for no gain, since the corpse holds no content
// worth preserving.
func (s *windowSweeper) healDeadRenderers(cfg Config, dead map[string]bool, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip) {
	if s.deadStrikes == nil {
		s.deadStrikes = map[string]int{}
	}
	if s.lastRebuild == nil {
		s.lastRebuild = map[string]time.Time{}
	}
	for _, remoteID := range reg.remoteIDs() {
		mw, ok := reg.byRemoteID(remoteID)
		if !ok {
			continue
		}
		if !dead[mw.localWin] {
			// A pass that finds this mirror healthy returns its budget: the cap
			// exists to stop a repeating failure, not to ration repairs over a
			// session in which renderers die once and come back. But a pass
			// inside deadRendererRecovery of the window's own last rebuild is
			// that rebuild's own died() echo, not a second data point — see
			// deadRendererRecovery's doc comment.
			if last := s.lastRebuild[remoteID]; last.IsZero() || time.Since(last) >= deadRendererRecovery {
				delete(s.deadStrikes, remoteID)
			}
			continue
		}
		if s.deadStrikes[remoteID] >= deadRendererStrikes {
			continue
		}
		s.deadStrikes[remoteID]++
		if err := resetWindow(cfg, mw, send, router, waitHellos, cst, cv, rt); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: dead renderer in %s: %v\n", remoteID, err)
		}
		// Stamped after resetWindow returns, not before: deadRendererRecovery
		// measures "has the renderer stayed up since the rebuild FINISHED",
		// and resetWindow's own spawn/hello/seed round trip can itself run
		// past deadRendererRecovery on a loaded box — stamping first would let
		// the rebuild's own duration, not just its echo, blow through the
		// recovery window it exists to close.
		s.lastRebuild[remoteID] = time.Now()
		if s.deadStrikes[remoteID] == deadRendererStrikes {
			fmt.Fprintf(os.Stderr, "daemon: %s: renderer keeps dying after %d rebuilds; leaving it\n",
				remoteID, deadRendererStrikes)
		}
	}
}

// windowSweepInterval is the floor between two maintenance sweeps, the agent
// and label shippers'. Both passes fork a local tmux client (~30ms measured),
// and the main loop's maintenance block runs once per control-stream LINE, not
// once per coarse tick: unfloored, a pane redrawing itself charges every mirror
// window a fork per line and the loop stops keeping up with the stream.
const windowSweepInterval = time.Second

// windowSweeper carries that floor across the main loop's iterations, and
// across a reconnect: no pass holds connection-scoped state. deadStrikes and
// lastRebuild are the exception to "no state" — they have to outlive the
// mirrorWindow they count against, which a rebuild replaces.
type windowSweeper struct {
	lastPass    time.Time
	deadStrikes map[string]int
	// lastRebuild is when healDeadRenderers last rebuilt each remote id's
	// window — see deadRendererRecovery.
	lastRebuild map[string]time.Time
}

// force resets the floor so the very next sweep call actually runs. Used only
// from the death-nudge case in runConn: windowSweepInterval exists to stop a
// per-stream-line fork storm, not to defer a corpse a connection close just
// reported.
//
// The real cost: a successful heal's own resetWindow closes the superseded
// conns, which arms a follow-up force too (that is what deadRendererRecovery
// exists to keep from being read as a second data point) — so a single death
// episode costs on the order of two forced sweeps, not one, and under
// sustained reconcile churn (a remote repeatedly splitting/closing panes) the
// forced cadence tops out at one sweep per deathSweepDelay (4/s) — still far
// below the per-stream-line storm the floor exists to prevent. A forced sweep
// whose own mirrorPaneRows listing fails still spends lastPass (sweep's
// s.lastPass = time.Now() precedes the listing read) — the one way a force is
// consumed without doing anything, and harmless since the backstop still
// applies.
func (s *windowSweeper) force() {
	s.lastPass = time.Time{}
}

// sweep runs the registry-wide repair passes, at most once per interval.
func (s *windowSweeper) sweep(cfg Config, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, reg *registry, cv *converger, rt roundTrip) {
	if time.Since(s.lastPass) < windowSweepInterval {
		return
	}
	s.lastPass = time.Now()
	live, dead, ok := mirrorPaneRows(cfg)
	if !ok {
		return
	}
	// Lost windows first: a retire rebuilds under a fresh local window, which
	// the snapshot predates — so it cannot appear in dead, and the pass below
	// leaves the replacement alone rather than rebuilding it again.
	healLostWindows(cfg, live, send, router, waitHellos, cst, reg, cv, rt)
	s.healDeadRenderers(cfg, dead, send, router, waitHellos, cst, reg, cv, rt)
}
