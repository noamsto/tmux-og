package daemon

import (
	"fmt"
	"os"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// reseedOwed repaints every tiled pane the remote window shows, for a reshape
// the mirror never saw end to end. It runs where a pass found nothing to apply:
// the panes were resized and repainted at the intermediate shape, so their
// screens are stale even though the layout reads as already applied.
//
// A zoomed window's hidden panes keep their cells and show nothing, so only
// remoteActive is seeded then — the same rule as the shaped branch's zoomHides.
func reseedOwed(w *mirrorWindow, router *Router, rt roundTrip, remoteActive string, zoomed bool) {
	zoomHides := zoomed && indexOf(w.remotePanes, remoteActive) >= 0
	ids := make([]string, 0, len(w.remotePanes))
	sinks := make([]*outputSink, 0, len(w.remotePanes))
	for _, id := range w.remotePanes {
		if zoomHides && id != remoteActive {
			continue
		}
		if s := router.sink(id); s != nil {
			ids = append(ids, id)
			sinks = append(sinks, s)
		}
	}
	// Marked before the seed goes out for the same reason as the shaped branch:
	// the app's repaint for the restored size may still be to come.
	PaneSeeds(rt, ids, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: transient reseed for %s: %v\n", ids[i], err)
			return
		}
		sinks[i].markReshaped()
		enqueueSeedWithReplay(sinks[i], seed)
	})
}

// layoutBurstKey is what a %layout-change line tells the mirror's tiled panes:
// the tiled-only layout string and the zoom flag, or the raw Args when the line
// does not parse. A float's geometry is left out — it moves no tiled pane.
func layoutBurstKey(l controlmode.Line) layoutNotice {
	n, ok := parseLayoutNotice(l)
	if !ok {
		return layoutNotice{layout: fmt.Sprint(l.Args)}
	}
	if L, err := controlmode.ParseLayout(n.layout); err == nil {
		n.layout = L.Raw
	}
	return n
}

// coalesceLayoutBursts collapses a burst of %layout-change notifications for
// the same window into just the last one. reconcileLayoutFrom reads the
// surviving line's own Args, so only the last notification for a given window
// in an already-buffered batch can still matter — a resize drag can otherwise
// queue many of these while a single reconcile call's own round-trips are in
// flight, and dispatching each one individually would pay for N-1 redundant
// readLayout round-trips before reconcileLayoutFrom's own gates, or
// reconcileLayout's dedup behind them, even get a chance to discard them.
// Every other notification kind, and the relative order of what survives, is
// left untouched.
//
// It also reports which survivors dropped a notice describing a different
// tiled shape or zoom state. burst is keyed by index into out. The dropped
// shape may have been an out-and-back whose survivor equals the applied state,
// so the survivor must not be treated as a duplicate.
func coalesceLayoutBursts(lines []controlmode.Line) (out []controlmode.Line, burst map[int]bool) {
	last := map[string]int{}
	for i, l := range lines {
		if l.Kind == controlmode.LayoutChange && len(l.Args) > 0 {
			last[l.Args[0]] = i
		}
	}
	out = make([]controlmode.Line, 0, len(lines))
	burst = map[int]bool{}
	differs := map[string]bool{}
	for i, l := range lines {
		if l.Kind == controlmode.LayoutChange && len(l.Args) > 0 {
			id := l.Args[0]
			if last[id] != i {
				if layoutBurstKey(l) != layoutBurstKey(lines[last[id]]) {
					differs[id] = true
				}
				continue
			}
			if differs[id] {
				burst[len(out)] = true
			}
		}
		out = append(out, l)
	}
	return out, burst
}

// dispatchLayoutChange reconciles window mw against a %layout-change line. A
// burst line is a survivor whose dropped predecessors described a different
// shape, so the mirror owes a reseed whatever the survivor reads as.
func dispatchLayoutChange(cfg Config, mw *mirrorWindow, l controlmode.Line, burst bool, send func(string), router *Router,
	waitHellos helloWaiter, cst *ctlState, cv *converger, rt roundTrip) (retire bool) {
	if burst {
		mw.reshapeOwed = true
	}
	return reconcileLayoutFrom(cfg, mw, l, send, router, waitHellos, cst, cv, rt)
}
