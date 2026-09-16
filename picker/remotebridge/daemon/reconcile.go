package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// maxReconcilePasses bounds reconcileLayout's trailing-reread loop (below).
const maxReconcilePasses = 5

// errLocalPanesDesynced reports that the mirror window no longer holds one
// local pane per remote pane, so applyPaneOps' positional mapping — local pane
// i renders remote pane i — cannot be trusted. Recovered from by rebuilding the
// window, never by acting on the broken mapping.
var errLocalPanesDesynced = errors.New("local panes desynced from the remote order")

// reconcileLayout reads ground truth for window w's remote layout and zoom
// state, then delegates to reconcileSnapshot to apply it.
func reconcileLayout(cfg Config, w *mirrorWindow, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, cv *converger, rt roundTrip) (retire bool) {
	target := remoteWinTarget(cfg, w.remoteID)

	L, remoteActive, zoomed, err := readLayout(rt, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: layout-change: %v\n", err)
		return false
	}

	return reconcileSnapshot(cfg, w, L, remoteActive, zoomed, send, router, waitHellos, cst, cv, rt)
}

// reconcileLayoutFrom is reconcileLayout for a %layout-change notification,
// which already carries the unzoomed layout string and the zoom flag. A line
// that reports nothing the mirror does not already show returns without a
// round-trip, and a geometry-only reshape enters the pass loop from the line's
// own layout. Everything else still reads first: a pane-set or float change
// needs the active pane id for focus-follow, and a zoom transition needs the
// post-command snapshot — window_push_zoom/window_pop_zoom bracket structural
// commands, so on a zoomed window tmux emits transient unzoomed lines
// describing a state it leaves before the command returns.
func reconcileLayoutFrom(cfg Config, w *mirrorWindow, l controlmode.Line, send func(string), router *Router,
	waitHellos helloWaiter, cst *ctlState, cv *converger, rt roundTrip) (retire bool) {
	n, ok := parseLayoutNotice(l)
	if !ok {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	L, err := controlmode.ParseLayout(n.layout)
	if err != nil {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	// A stale pane-set line applied here would split or kill a local pane for
	// a remote pane the command has already moved past.
	if !slices.Equal(RemotePaneOrder(L), w.remotePanes) {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	if !noFloatWork(w, L) {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	// Only the zoom flag can still disagree. A mismatch is a real zoom or
	// unzoom, or the unzoomed line that opens a push/pop-zoom bracket;
	// only the read's post-command snapshot tells them apart.
	if n.zoomed != w.appliedZoom {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	if L.Raw == w.layout {
		return false
	}
	// A zoomed reshape needs the active pane for the -Z toggle and the zoomed
	// pane's dims.
	if n.zoomed {
		return reconcileLayout(cfg, w, send, router, waitHellos, cst, cv, rt)
	}
	// Geometry only: same panes, same floats, no zoom reported — whether or
	// not the window holds a float, since the tiled-only select-layout leaves
	// every float where it is. No active pane id is passed because nothing on
	// this path consumes one — focus-follow is gated on a structural diff, the
	// -Z toggle and zoomed-pane dims on a zoom, the post-loop float focus on an
	// added float. A line the remote has already moved past costs one pass: the
	// trailing re-read below runs the next one on its own ground-truth triple.
	return reconcileSnapshot(cfg, w, L, "", false, send, router, waitHellos, cst, cv, rt)
}

// reconcileSnapshot applies the general pane diff (planPaneOps) for window w
// against the already-read remote layout L, so the local mirror renders the
// remote's panes in the remote's order, then re-fits geometry.
//
// The remote window is targeted by its id (@N) directly, never by a bare index.
//
// Loops on a trailing re-read after applying: a second remote layout change
// landing back-to-back with the first is queued for the main loop rather than
// acted on here, so this pass would otherwise finish against a layout that is
// already stale. Re-reading once more right after applying catches it: the
// round-trips above give the remote plenty of time to settle, so a still-
// different layout means something changed underneath us and needs its own pass.
//
// Reports whether the mirror should be retired: its local window is gone, so
// nothing this pass aims at it can land. The caller owns that recovery — it
// holds the registry, and the rebuild goes back through reconcileWindows so the
// replacement is built by the one path that stamps and names a mirror window.
func reconcileSnapshot(cfg Config, w *mirrorWindow, L controlmode.Layout, remoteActive string, zoomed bool, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, cv *converger, rt roundTrip) (retire bool) {
	target := remoteWinTarget(cfg, w.remoteID)

	// Same tiled shape, same zoom state: no tiled pane moved, so the pass loop
	// below has nothing to do for one. A resize burst or a spurious/duplicate
	// %layout-change reaching this entry — the read-first one directly, or
	// reconcileLayoutFrom after falling through its own gates 1/2 — then pays
	// only this one cheap readLayout round-trip instead of the FitWindowCmd +
	// per-pane resize/reseed; a notification that matched reconcileLayoutFrom's
	// own gate 3 no-op returned before paying even that. L.Raw alone isn't
	// enough for that: it is deliberately the unzoomed geometry (see
	// readLayout's doc comment), so the zoom flag has to agree too.
	if L.Raw == w.layout {
		if w.appliedZoom == zoomed {
			// ParseLayout prunes floats out of Raw, so a float opening, moving
			// or closing leaves it byte-identical — every float bind and every
			// carousel toggle lands here. The floats are settled from here, not
			// through the loop, because applyLayout would then return ok=true
			// trivially (the shape it is asked for is the one already applied)
			// and gate a FrameResize plus a capture-pane reseed into every
			// tiled pane in the window for a change that moved none of them.
			if !noFloatWork(w, L) {
				added := reconcileFloats(cfg, w, L, send, router, waitHellos, rt)
				cst.setWindowPanes(w.remoteID, w.allRemotePanes())
				if indexOf(added, remoteActive) >= 0 {
					focusLocalPane(cfg, cst, w, RemotePaneOrder(L), remoteActive)
				}
			}
			return false
		}
	}

	// Only a rebuild inside this call may raise it, so a value left over from an
	// earlier call must not read as floats this one owes a re-add.
	w.floatsDropped = false
	remote := w.remotePanes
	converged := false
passes:
	for pass := 0; pass < maxReconcilePasses; pass++ {
		newRemote := RemotePaneOrder(L)
		ops := planPaneOps(remote, newRemote)
		structural := len(ops.Remove) > 0 || len(ops.Append) > 0 || len(ops.Swaps) > 0

		switch {
		case ops.Reset:
			if err := resetWindow(cfg, w, send, router, waitHellos, cst, cv, rt); err != nil {
				fmt.Fprintf(os.Stderr, "daemon: layout-change reset %s: %v\n", w.remoteID, err)
				w.remotePanes = remote
				return retireOrRestoreFloats(cfg, w, L, send, router, waitHellos, cst, rt)
			}
			// setupWindow re-read the layout and re-shaped the window itself.
			return false
		case structural:
			err := applyPaneOps(cfg, w, ops, L, remote, newRemote, send, router, waitHellos, rt)
			if errors.Is(err, errLocalPanesDesynced) {
				fmt.Fprintf(os.Stderr, "daemon: layout-change %s: %v; rebuilding\n", w.remoteID, err)
				if err := resetWindow(cfg, w, send, router, waitHellos, cst, cv, rt); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: layout-change reset %s: %v\n", w.remoteID, err)
					w.remotePanes = remote
					return retireOrRestoreFloats(cfg, w, L, send, router, waitHellos, cst, rt)
				}
				// setupWindow re-read the layout and re-shaped the window itself.
				return false
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "daemon: layout-change: %v\n", err)
				w.remotePanes = remote
				return retireOrRestoreFloats(cfg, w, L, send, router, waitHellos, cst, rt)
			}
		}

		// Both halves of the broadcast below speak the REMOTE's geometry, so
		// both are worthless — actively harmful — when the shape did not land:
		// they would paint the remote's screen at the remote's dims into panes
		// still carrying tmux's own auto-geometry, which is the blank-mirror
		// failure. On a miss the mirror keeps its last-good screen instead.
		// applyPaneOps' own inner seed is deliberately NOT gated this way: a
		// pane it just appended has no last-good screen to keep.
		if applyLayout(cfg, w, L) {
			// Bounded on both sides: after applyLayout, because select-layout unzooms
			// the window it shapes (measured), and before the dims and seeds below,
			// which describe and paint the pane at whatever geometry it holds now.
			//
			// tmux exposes zoom only as a toggle; if -F asserts the flag in one
			// idempotent command rather than reading local state and toggling on
			// mismatch. applyLayout may skip select-layout on a zoom-only reconcile,
			// so a bare toggle would fire on every pass and an even count inverts.
			// A zoomed pane is usually the active one, but a float can be active
			// while the window stays zoomed — see the -Z targeting note below.
			//
			// localIsZoomed carries what the assert landed, not what the remote
			// reported — what the dims below are entitled to claim. A zoom that
			// never landed must not stop the pass: the reseed still owes every
			// pane a fresh screen, and skipping it reopens #233/#417.
			localIsZoomed := false
			if assertMirrorZoom(cfg, w, zoomed, remoteActive, newRemote) {
				localIsZoomed = zoomed
				w.appliedZoom = zoomed
			}
			// select-layout reshapes every surviving pane, so push each its new
			// dims (layout is daemon-authoritative — renderers only record them).
			//
			// A zoomed pane's cell IS the layout root: #{window_layout} keeps
			// reporting the saved, unzoomed tree, so L.Panes[i] names a cell this
			// pane has left. Reading #{window_visible_layout} instead is not the
			// fix — it reports the window as single-pane and reconcile would kill
			// every hidden pane's renderer (#413). The others keep their cell
			// because tmux's zoom drops them from the layout without resizing
			// them. When remoteActive is a float, no tiled id matches — leave
			// every tiled pane at its cell size rather than guessing which one
			// holds the zoom (same skip as the -Z toggle above).
			for i, id := range newRemote {
				if s := router.sink(id); s != nil {
					pw, ph := L.Panes[i].W, L.Panes[i].H
					if localIsZoomed && id == remoteActive {
						pw, ph = L.W, L.H
					}
					s.enqueue(wire.FrameResize, wire.EncodeResize(pw, ph))
				}
			}
			// Every pane gets a fresh screen once the reshape has landed. The
			// painters hold no back-buffer to reflow, so a pane whose dims moved has
			// to be repainted from the remote — and that is as true of a pane that
			// survived a close or a split as of one whose window merely resized
			// (#417). A newly appended pane is re-seeded too: applyPaneOps enqueued
			// its first seed before the reshape, into a pane that was still the old
			// size.
			//
			// After the reshape, never before (#233): a seed sized for the new
			// geometry painted into a pane still at the old size leaves the mirror
			// blank.
			//
			// The one exemption is a zoomed window's hidden panes (#557): tmux
			// keeps their cells, so their dims never moved and nothing they show
			// is visible — seeding them is pure transfer on a slow link. The
			// unzoom reconcile (the zoom flag flips, so localIsZoomed reads false
			// here) reseeds them before they show. No skip when remoteActive is a
			// float: which tiled pane holds the zoom is then unknowable, the same
			// guess-forbid as the -Z toggle above.
			zoomHides := localIsZoomed && indexOf(newRemote, remoteActive) >= 0
			reseedIDs := make([]string, 0, len(newRemote))
			sinks := make([]*outputSink, 0, len(newRemote))
			for _, id := range newRemote {
				if zoomHides && id != remoteActive {
					continue
				}
				if s := router.sink(id); s != nil {
					reseedIDs = append(reseedIDs, id)
					sinks = append(sinks, s)
				}
			}
			// Marked before the seed goes out, not after: this capture was taken
			// the instant the geometry moved, so the remote app's repaint for the
			// new size is still to come and the screen being shipped is tmux's
			// rewrap of the old one. markReshaped owes the pane a second capture
			// a main-loop pass later, by which time that repaint has landed.
			PaneSeeds(rt, reseedIDs, func(i int, seed []byte, err error) {
				if err != nil {
					fmt.Fprintf(os.Stderr, "daemon: layout-change reseed for %s: %v\n", reseedIDs[i], err)
					return
				}
				sinks[i].markReshaped()
				enqueueSeedWithReplay(sinks[i], seed)
			})
		}

		// Follow the remote's active pane, but only when the pane set or order
		// moved: a pure resize must not yank local focus. remoteActive comes
		// from the same round-trip as the layout, so it is ground truth rather
		// than a tracked belief.
		if structural {
			focusLocalPane(cfg, cst, w, newRemote, remoteActive)
		}
		remote = newRemote
		w.remotePanes = remote
		// Float-inclusive even mid-loop: parseCtl refuses a pane it cannot map to
		// a window, so a gesture inside a mirrored float while further passes are
		// still in flight would come back as a --display-error banner.
		cst.setWindowPanes(w.remoteID, w.allRemotePanes())

		// The float set rides alongside Raw and the zoom flag, both of which are
		// float-blind: without it a float that appeared or moved mid-pass would
		// read as converged and the reconcileFloats below would apply the stale
		// L.Floats. Compared against L.Floats — the previous REMOTE read — never
		// against w.floatGeom, which only the post-loop reconcileFloats updates
		// and which would then never converge.
		fresh, freshActive, freshZoom, err := readLayout(rt, target)
		if err != nil || (fresh.Raw == L.Raw && freshZoom == zoomed && floatCellsEqual(fresh.Floats, L.Floats)) {
			converged = true
			break passes
		}
		L, remoteActive, zoomed = fresh, freshActive, freshZoom
	}
	if !converged {
		fmt.Fprintf(os.Stderr, "daemon: layout-change: didn't converge after %d passes, stopping at %v\n", maxReconcilePasses, remote)
		w.remotePanes = remote
	}
	// After the loop, so one pass's worth of float surgery runs however many
	// times the tiled shape had to be re-applied. The exits that return instead
	// of breaking skip this: a rebuild that succeeded has already settled the
	// window's floats, and one that failed went through retireOrRestoreFloats.
	added := reconcileFloats(cfg, w, L, send, router, waitHellos, rt)
	// The in-loop setWindowPanes asserted the TILED set, and setWindowPanes
	// clears every pane mapped to the window before re-setting, so the
	// float-inclusive set has to be re-asserted once the floats exist. Without
	// it parseCtl refuses the first keybind pressed inside a freshly mirrored
	// float with a visible --display-error banner.
	cst.setWindowPanes(w.remoteID, w.allRemotePanes())
	// A float appearing moves no tiled pane, so `structural` above is false for
	// it and the focus-follow there never fires — but a verb that opens a float
	// (prefix + g) makes it the remote's active pane, and without this the user
	// types into a tiled renderer. Newly added only: re-asserting focus on a
	// float already mirrored would fight the user's own local focus on every
	// unrelated reconcile.
	if indexOf(added, remoteActive) >= 0 {
		focusLocalPane(cfg, cst, w, remote, remoteActive)
	}
	return false
}

// noFloatWork reports that w's mirrored floats already match L's — the float
// half of reconcileSnapshot's "nothing to do" test and gate 2 of
// reconcileLayoutFrom, expressed as the same pure diff the reconcile itself
// drives so the two can never disagree.
func noFloatWork(w *mirrorWindow, L controlmode.Layout) bool {
	ops := planFloatOps(w.floatGeom, mirrorableFloats(L))
	return len(ops.Remove) == 0 && len(ops.Add) == 0 && len(ops.Move) == 0
}

// maxMirroredFloats caps how many of a remote window's floats get a local
// mirror. The tiled panes need no such bound — each one takes a cell out of the
// window's area, so the area itself limits them — but floats overlap freely and
// a layout string can name arbitrarily many, while each one mirrored costs a
// local pane, a renderer process, a socket and a goroutine. Set far above what
// the float binds can produce by hand.
const maxMirroredFloats = 32

// mirrorableFloats returns the floats of L this daemon will mirror.
//
// The WANTED set is what the cap truncates, never the add loop: an add loop cut
// short would leave the excess permanently absent from localFloats, so every
// later pass would re-plan them as missing and churn a new-pane burst per
// %layout-change. Both the diff that decides there is work (noFloatWork) and the
// one that applies it (reconcileFloats) read this, or the guard would believe
// work is outstanding that the reconcile never intends to do.
//
// The cut is positional, and tmux promises no stable float-section order
// between reads, so a window above the cap can churn the floats either side of
// the cutoff.
func mirrorableFloats(L controlmode.Layout) []controlmode.PaneCell {
	if len(L.Floats) <= maxMirroredFloats {
		return L.Floats
	}
	fmt.Fprintf(os.Stderr, "daemon: %d floats reported, mirroring the first %d\n", len(L.Floats), maxMirroredFloats)
	return L.Floats[:maxMirroredFloats]
}

// resetLostWindow asks, of a pass that already failed, whether the reason was
// that the mirror's local window went away — the one failure a retry can never
// clear, and the one a rebuild fixes. Consulted only on an error path, so the
// extra tmux read costs nothing on the pass that succeeds.
//
// Every other failure returns false and leaves the mirror alone: they are
// transient, and retiring on one would tear down a live window to rebuild it.
func resetLostWindow(cfg Config, w *mirrorWindow) bool {
	if !localWindowGone(cfg, w.localWin) {
		return false
	}
	fmt.Fprintf(os.Stderr, "daemon: layout-change %s: local window %s is gone; retiring\n", w.remoteID, w.localWin)
	return true
}

// retireOrRestoreFloats settles a reconcileLayout pass that failed and returns
// rather than breaking, so it never reaches the post-loop reconcileFloats.
//
// A failed rebuild can have lost the mirrored floats already: dropMirroredPanes
// discards them, and setupWindow can fail before its own trailing
// reconcileFloats puts them back. The drop raises floatsDropped, so this re-add
// runs only when there is something to restore. It is orthogonal to whatever
// else the failure left broken — reconcileFloats touches no tiled pane mapping,
// and L.Floats is still the wanted set.
//
// resetLostWindow first, never after: a window that is genuinely gone must not
// be handed a burst of doomed new-panes.
func retireOrRestoreFloats(cfg Config, w *mirrorWindow, L controlmode.Layout, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, rt roundTrip) bool {
	retire := resetLostWindow(cfg, w)
	if !retire && w.floatsDropped {
		reconcileFloats(cfg, w, L, send, router, waitHellos, rt)
		cst.setWindowPanes(w.remoteID, w.allRemotePanes())
	}
	return retire
}

// applyLayout fits the local mirror window to the remote's geometry and then
// shapes it into L, reporting whether the window ends up carrying L's shape.
// The fit comes first: an unfitted window would make select-layout rescale the
// remote's layout to the local client's size instead of taking the remote's.
//
// The shape is skipped when the window already carries L, and applyPaneOps
// clears w.layout, so surgery that reshapes the window always shapes it back.
//
// L.Raw is the tiled-only v1 string, which select-layout applies while leaving
// every float in place, the user's own included (#535). A resident local server
// older than the pinned tmux refuses it while any float is open, so the mirror
// keeps its last-good screen until a remote change lands with none open.
//
// ok is false only when select-layout itself failed. The caller gates the
// remote's dims and screen on it: painting them into panes that never took the
// shape is the blank-mirror failure.
func applyLayout(cfg Config, w *mirrorWindow, L controlmode.Layout) (ok bool) {
	if err := cfg.LocalTmux(FitWindowCmd(w.localWin, L)...); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: layout-change resize-window: %v\n", err)
	}
	if L.Raw == w.layout {
		return true
	}
	if err := cfg.LocalTmux("select-layout", "-t", w.localWin, L.Raw); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: layout-change select-layout: %v\n", err)
		return false
	}
	w.appliedZoom = false // select-layout unzooms; caller's assertMirrorZoom may set it again
	w.layout = L.Raw
	return true
}

// sortedFloatIDs returns localFloats' keys in a fixed order, so a rebuild kills
// the window's floats in the same sequence every time rather than in map order.
func sortedFloatIDs(localFloats map[string]string) []string {
	ids := make([]string, 0, len(localFloats))
	for id := range localFloats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// floatCellsEqual reports whether two remote float sets are the same floats at
// the same geometry.
//
// By id, not by position: nothing promises tmux emits a window's float section
// in the same order between two reads, and a positional compare would then read
// an unchanged window as changed, burn a whole extra reconcile pass and end in a
// spurious "didn't converge". Pane ids are unique within a window, so equal
// lengths plus every id of a matched in b is set equality. The scan is nested
// rather than mapped: a window holds a handful of floats, and this runs once per
// pass.
func floatCellsEqual(a, b []controlmode.PaneCell) bool {
	if len(a) != len(b) {
		return false
	}
	for _, ca := range a {
		found := false
		for _, cb := range b {
			if cb.ID != ca.ID {
				continue
			}
			if cb != ca {
				return false
			}
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// applyPaneOps performs the local pane surgery ops describes: kill the panes
// whose remote pane is gone, split new ones off the tail and wire a renderer to
// each, then swap the local panes into the remote's order.
func applyPaneOps(cfg Config, w *mirrorWindow, ops paneOps, L controlmode.Layout, remote, newRemote []string, send func(string), router *Router, waitHellos helloWaiter, rt roundTrip) error {
	// tmux is the only authority on what the window holds (see
	// refreshLocalPanes), and a pane can leave by a route this daemon never
	// saw — a renderer that died, a local kill, the window picker's ^x. Re-read
	// before trusting the positional mapping every op below depends on.
	//
	// A count that no longer matches means the mapping is broken outright, not
	// merely stale: local pane i stops rendering remote[i], so a kill or a swap
	// lands on a live neighbour, and the panes that should have gone survive
	// into a select-layout that then refuses them ("have 2 panes but need 1").
	// Rebuild instead — the caller's resetWindow is the existing escape hatch
	// for exactly this, and a desync is rare enough to afford it.
	if err := refreshLocalPanes(cfg, w); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	if len(w.localPanes) != len(remote) {
		return fmt.Errorf("%w: %d local panes for %d remote", errLocalPanesDesynced, len(w.localPanes), len(remote))
	}
	// Every op below reshapes the window, so what it carries is no longer the
	// layout last applied to it.
	w.layout = ""
	// Each kill renumbers the window's remaining panes, so the targets are pane
	// ids: they survive the kills ahead of them, an ordinal does not.
	for _, i := range ops.Remove {
		removed := remote[i]
		router.Unregister(removed)
		if c := w.conns[removed]; c != nil {
			c.Close()
			delete(w.conns, removed)
		}
		local, ok := localPaneAt(w, i)
		if !ok {
			return fmt.Errorf("reconcile: no local pane for %s", removed)
		}
		// Killing by id, so a failure here means the pane was already gone —
		// which the entry re-read above has already accounted for.
		if err := cfg.LocalTmux("kill-pane", "-t", local); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile kill-pane: %v\n", err)
		}
	}
	if len(ops.Remove) > 0 {
		if err := refreshLocalPanes(cfg, w); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
	}

	// Splits target the current last pane explicitly, which lands the new pane
	// at lastIdx+1 (verified) — the old code split the window and relied on the
	// new pane implicitly taking the loop index. The re-read after each split
	// is what names the pane just created.
	//
	// The pane is created on the axis the remote used and already running the
	// renderer, rather than -h with a shell that respawn-pane then replaces
	// (#447). Both were visible: a stacked remote split showed side-by-side
	// until select-layout landed, and the shell painted a prompt into the pane
	// first. srcID is the pane being split from — the last surviving remote
	// pane for the first append, then each previous append in turn.
	srcID := ""
	for i := len(remote) - 1; i >= 0; i-- {
		if indexOf(newRemote, remote[i]) >= 0 {
			srcID = remote[i]
			break
		}
	}
	for _, id := range ops.Append {
		last, ok := localPaneAt(w, len(w.localPanes)-1)
		if !ok {
			return fmt.Errorf("reconcile: window %s has no pane to split", w.localWin)
		}
		axis := SplitAxis(L, newRemote, srcID, id)
		split := append([]string{"split-window", axis, "-t", last, "--"}, rendererSpawnArgs(cfg, id)...)
		if err := cfg.LocalTmux(split...); err != nil {
			return fmt.Errorf("reconcile split-window: %w", err)
		}
		if err := refreshLocalPanes(cfg, w); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		added, ok := localPaneAt(w, len(w.localPanes)-1)
		if !ok {
			return fmt.Errorf("reconcile: split of %s produced no pane", w.localWin)
		}
		markRendererPane(cfg, added, id)
		srcID = id
	}

	if len(ops.Append) > 0 {
		// Before the hello wait below, not after: the splits above are always -h,
		// and the wait costs a renderer spawn, so leaving the geometry until the
		// caller's trailing pass puts the wrong shape on screen for the whole
		// handshake rather than for a frame (#408). The swaps below exchange
		// panes between cells without changing cell geometry, so this stays
		// correct and the caller's pass becomes idempotent.
		applyLayout(cfg, w, L)

		// Seeding is sequential over the single control stream, so every new
		// renderer must be connected first (mirrors setupWindow).
		added, err := waitHellos(ops.Append)
		if err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		for _, id := range ops.Append {
			c := added[id]
			if c == nil {
				continue
			}
			w.conns[id] = c
			// Dims come from the pane's cell in the REMOTE order, not from the
			// temporary local index it was appended at — the swaps below have
			// not run yet, so the two differ.
			seedRenderer(rt, router, c, id, L.Panes[indexOf(newRemote, id)], cfg.graphicsFor(id))
			go pumpInput(c, id, send, cfg.paster())
		}
	}

	// -d keeps the same pane active rather than the same index (verified: the
	// flag's effect on the active pane id is opposite between swap-pane's
	// one-target and two-target forms), so local focus rides with its pane.
	for _, s := range ops.Swaps {
		src, srcOK := localPaneAt(w, s[0])
		dst, dstOK := localPaneAt(w, s[1])
		if !srcOK || !dstOK {
			return fmt.Errorf("reconcile: swap %d<->%d out of range for %s", s[0], s[1], w.localWin)
		}
		if err := cfg.LocalTmux("swap-pane", "-d", "-s", src, "-t", dst); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile swap-pane: %v\n", err)
		}
		w.localPanes[s[0]], w.localPanes[s[1]] = w.localPanes[s[1]], w.localPanes[s[0]]
	}
	return nil
}

// resetWindow rebuilds w from scratch, for the case where no current pane
// survives (planPaneOps.Reset). Killing every pane instead would make tmux
// destroy the mirror window and leave a registry entry pointing at nothing.
// Closing a pane's conn kills that pane just as surely as kill-pane does, which
// is why the kept pane's conn must outlive setupWindow until a replacement
// renderer is known.
//
// The SHARED converger, like every other setupWindow caller: this reads
// cfg.LocalArea() independently of watchLocalClient's own read and writes to the
// same stream, so a throwaway map lets the two disagree — a stale size written
// last while the shared record holds the new one, which watchLocalClient then never
// re-sends. setupWindow's own cv.forget is what makes the reset re-cap.
func resetWindow(cfg Config, w *mirrorWindow, send func(string), router *Router, waitHellos helloWaiter, cst *ctlState, cv *converger, rt roundTrip) error {
	// Captured before dropMirroredPanes empties the map, so the merge below can
	// tell a float's conn from a tiled pane's.
	floats := sortedFloatIDs(w.localFloats)
	for _, id := range w.allRemotePanes() {
		router.Unregister(id)
	}
	oldConns := w.conns
	w.conns = map[string]net.Conn{}
	dropMirroredPanes(cfg, w)
	err := setupWindow(cfg, send, router, waitHellos, cst, w, cv, rt)

	closed := map[string]bool{}
	for remoteID, oldConn := range oldConns {
		if newConn, ok := w.conns[remoteID]; ok && newConn != oldConn {
			oldConn.Close()
			closed[remoteID] = true
		}
	}
	// A float's old conn is never merged back, on any path: its pane is dead
	// (dropMirroredPanes killed it), and setupWindow's reconcileFloats rebuilds
	// the window's floats from the remote's layout from scratch, so there is
	// nothing left for it to render. Reviving one would put a sink on a pane
	// that no longer exists.
	for _, id := range floats {
		if closed[id] {
			continue
		}
		if c := oldConns[id]; c != nil {
			c.Close()
		}
		closed[id] = true
	}
	if err == nil || w.spawned {
		for remoteID, oldConn := range oldConns {
			if closed[remoteID] {
				continue
			}
			oldConn.Close()
		}
		if err == nil {
			return nil
		}
		return err
	}
	for remoteID, oldConn := range oldConns {
		if closed[remoteID] {
			continue
		}
		if _, ok := w.conns[remoteID]; !ok {
			w.conns[remoteID] = oldConn
			// Output while unrouted is lost; the pane stays live but drifts
			// until the next successful reset/reseed repairs the screen.
			router.Register(remoteID, newOutputSink(oldConn, cfg.graphicsFor(remoteID)))
		}
	}
	return err
}

// dropMirroredPanes kills every mirrored pane but the first, which resetWindow
// leaves for setupWindow to re-shape and respawn, and clears w's belief about
// what the window holds. Only the panes this daemon created: a float this
// daemon did not create — the user's own, from the unguarded prefix + b/k/i
// binds — is not its to reap.
//
// The mirrored floats go with them, so the rebuild starts from scratch and
// setupWindow's reconcileFloats re-creates them from the remote's own layout.
// That teardown is also what repairs a float whose renderer died: a rebuild
// that kept live floats would need to tell a dead one apart. A rebuild that
// fails before reaching reconcileFloats leaves them gone, which is why the drop
// raises floatsDropped.
func dropMirroredPanes(cfg Config, w *mirrorWindow) {
	for i := len(w.localPanes) - 1; i > 0; i-- {
		if err := cfg.LocalTmux("kill-pane", "-t", w.localPanes[i]); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reset kill-pane: %v\n", err)
		}
	}
	floats := sortedFloatIDs(w.localFloats)
	if len(floats) > 0 {
		w.floatsDropped = true
	}
	for _, id := range floats {
		if err := cfg.LocalTmux("kill-pane", "-t", w.localFloats[id]); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reset float kill-pane: %v\n", err)
		}
		delete(w.localFloats, id)
		delete(w.floatGeom, id)
	}
	w.remotePanes = nil
	w.localPanes = nil
	w.layout = ""
	w.appliedZoom = false // select-layout in setupWindow unzooms; stale true would dedup forever
}

// localPaneFor resolves the local pane rendering remoteID: a mirrored float by
// id, otherwise the tiled pane holding remoteID's ordinal in order. Floats are
// looked up first because they hold no slot in that order at all, so the
// ordinal path could only ever miss one.
func localPaneFor(w *mirrorWindow, order []string, remoteID string) (string, bool) {
	if local, ok := w.localFloats[remoteID]; ok {
		return local, true
	}
	return localPaneAt(w, indexOf(order, remoteID))
}

// focusLocalPane points local focus at whichever local pane renders the
// remote's active pane, mirrored float included — a verb that moves the remote's
// focus onto one (prefix + g) would otherwise leave the user typing into a tiled
// renderer. A remote pane the mirror does not render (it closed between the
// layout read and now) leaves local focus alone.
func focusLocalPane(cfg Config, cst *ctlState, w *mirrorWindow, order []string, remoteActive string) {
	local, ok := localPaneFor(w, order, remoteActive)
	if !ok {
		return
	}
	// Record before issuing: the local select-pane fires after-select-pane, whose
	// ctl report must be recognised as this move's echo rather than a new gesture.
	cst.noteLocalFocus(w.remoteID, remoteActive)
	if err := cfg.LocalTmux("select-pane", "-t", local); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: reconcile select-pane: %v\n", err)
	}
}

// removeFloat tears down the local mirror of one remote float: its router
// sink, its renderer conn, the local floating pane, and both float map
// entries.
//
// Unregister and close come before the kill, as in applyPaneOps' Remove loop:
// Unregister closes the pane's output sink and stops its pump, so nothing is
// still writing frames at a pane tmux is about to destroy.
func removeFloat(cfg Config, w *mirrorWindow, router *Router, remoteID string) {
	router.Unregister(remoteID)
	if c := w.conns[remoteID]; c != nil {
		c.Close()
		delete(w.conns, remoteID)
	}
	if local, ok := w.localFloats[remoteID]; ok {
		// Killing by id, so a failure here means the pane was already gone.
		if err := cfg.LocalTmux("kill-pane", "-t", local); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float kill-pane %s: %v\n", local, err)
		}
	}
	delete(w.localFloats, remoteID)
	delete(w.floatGeom, remoteID)
}

// stampFloatGeom records the geometry a local float was just created or moved
// with in its @float_geom pane option.
//
// Nothing forces this for a daemon-made float — the repo's
// float-conf-assertions check only greps `bind` lines out of the generated
// tmux.conf, and these are not binds — but tmux-float-refit reasserts
// @float_geom on every window-resized, so an unstamped float drifts off the
// remote's geometry the first time the client resizes. The value is the outer
// box, the space refit's resize-pane -x/-y and move-pane -X/-Y speak: stamping
// the cell instead would shrink the float by two cells per axis per resize.
func stampFloatGeom(cfg Config, local string, c controlmode.PaneCell, L controlmode.Layout) {
	if err := cfg.LocalTmux("set-option", "-p", "-t", local, "@float_geom", floatGeomStamp(c, L.W, L.H)); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: reconcile float @float_geom %s: %v\n", local, err)
	}
}

// reconcileFloats makes the mirror window hold one local floating pane per
// remote float in L, each wired through the same renderer sequence a tiled
// mirror pane gets.
//
// Best-effort throughout, and no failure is reported back: every other step of
// its setupWindow caller is fatal — addWindow kills the local window on a setup
// error and Run aborts the bridge — and a failed new-pane, or a hello timeout,
// for one decorative float must not cost the user the mirror, let alone the
// whole bridge, because the remote session merely had lazygit floating. Each
// float that fails is logged and skipped; the others still land.
//
// Returns the remote ids it newly mirrored — created AND wired to a renderer,
// so a float that failed either half is absent. That is what lets the caller
// follow the remote's focus onto a float without re-asserting focus on one it
// was already rendering.
func reconcileFloats(cfg Config, w *mirrorWindow, L controlmode.Layout, send func(string), router *Router, waitHellos helloWaiter, rt roundTrip) []string {
	ops := planFloatOps(w.floatGeom, mirrorableFloats(L))

	for _, id := range ops.Remove {
		removeFloat(cfg, w, router, id)
	}

	added := make([]controlmode.PaneCell, 0, len(ops.Add))
	for _, cell := range ops.Add {
		out, err := cfg.LocalTmuxOut(floatCreateArgv(w.localWin, cell, L.W, L.H)...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float new-pane for %s: %v\n", cell.ID, err)
			continue
		}
		local := strings.TrimSpace(out)
		if !strings.HasPrefix(local, "%") {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float new-pane for %s: no pane id in %q\n", cell.ID, out)
			continue
		}
		stampFloatGeom(cfg, local, cell, L)
		w.localFloats[cell.ID] = local
		w.floatGeom[cell.ID] = cell
		// Created bare and respawned rather than created running the renderer:
		// respawn-pane -k preserves a float's floatness, its geometry and the
		// pane options markRendererPane writes, so a float takes the tiled
		// path's spawn unchanged.
		if err := spawnRenderer(cfg, local, cell.ID); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float renderer for %s: %v\n", cell.ID, err)
			removeFloat(cfg, w, router, cell.ID)
			continue
		}
		added = append(added, cell)
	}

	wired := make([]string, 0, len(added))
	if len(added) > 0 {
		// One batch for every add, after the whole create/spawn loop (mirrors
		// applyPaneOps): seeding is sequential over the single control stream,
		// so every renderer has to be connected — and hence writable — first.
		ids := make([]string, len(added))
		for i, cell := range added {
			ids[i] = cell.ID
		}
		conns, err := waitHellos(ids)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile floats: %v\n", err)
		}
		for _, cell := range added {
			c := conns[cell.ID]
			if c == nil {
				// waitHellos closes whatever it had collected before reporting,
				// so this pane's renderer has no owner. Drop it rather than keep
				// a map entry a later pass reads as already mirrored and never
				// retries.
				removeFloat(cfg, w, router, cell.ID)
				continue
			}
			w.conns[cell.ID] = c
			// The cell goes in unconverted: a float's layout cell IS its usable
			// pane size, and only tmux's create/resize/move flags take the
			// border inset.
			//
			// A false return does not mean the pane is stranded — wireRenderer
			// registers the sink and wires the pane unseeded either way, and a
			// later reseed repairs the screen — so the pump starts regardless.
			seedRenderer(rt, router, c, cell.ID, cell, cfg.graphicsFor(cell.ID))
			// Its own paste handler, like every other pumpInput: the handler
			// serializes one pane's input against its own pending upload, so
			// sharing one between panes would make a float's paste block a
			// tiled pane's keystrokes.
			go pumpInput(c, cell.ID, send, cfg.paster())
			wired = append(wired, cell.ID)
		}
	}

	moved := make([]string, 0, len(ops.Move))
	sinks := make([]*outputSink, 0, len(ops.Move))
	for _, cell := range ops.Move {
		local, ok := w.localFloats[cell.ID]
		if !ok {
			continue
		}
		// Size before position, both in the outer box tmux's flags speak.
		if err := cfg.LocalTmux(floatResizeArgv(local, cell, L.W, L.H)...); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float resize-pane %s: %v\n", local, err)
			continue
		}
		if err := cfg.LocalTmux(floatMoveArgv(local, cell, L.W, L.H)...); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float move-pane %s: %v\n", local, err)
			continue
		}
		stampFloatGeom(cfg, local, cell, L)
		w.floatGeom[cell.ID] = cell
		s := router.sink(cell.ID)
		if s == nil {
			continue
		}
		s.enqueue(wire.FrameResize, wire.EncodeResize(cell.W, cell.H))
		moved = append(moved, cell.ID)
		sinks = append(sinks, s)
	}
	// A renderer holds no back-buffer, so a float whose dims moved has to be
	// repainted from the remote or it keeps the old screen at the wrong size.
	// After the geometry has landed, never before: a seed sized for the new
	// geometry painted into a pane still at the old size leaves it blank.
	PaneSeeds(rt, moved, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: reconcile float reseed for %s: %v\n", moved[i], err)
			return
		}
		enqueueSeedWithReplay(sinks[i], seed)
	})
	return wired
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

// assertMirrorZoom idempotently matches the mirror window's zoom flag to the
// remote's. Zoom-on targets the tiled local pane rendering remoteActive; unzoom
// targets the window. Zoom-on is skipped when remoteActive is a float (#517).
func assertMirrorZoom(cfg Config, w *mirrorWindow, zoomed bool, remoteActive string, newRemote []string) bool {
	if zoomed {
		target, ok := localPaneAt(w, indexOf(newRemote, remoteActive))
		if !ok {
			return false
		}
		if err := cfg.LocalTmux("if", "-F", "-t", w.localWin, "#{window_zoomed_flag}", "", "resize-pane -Z -t "+target); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: layout-change zoom: %v\n", err)
			return false
		}
		return true
	}
	if err := cfg.LocalTmux("if", "-F", "-t", w.localWin, "#{window_zoomed_flag}", "resize-pane -Z -t "+w.localWin, ""); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: layout-change zoom: %v\n", err)
		return false
	}
	return true
}
