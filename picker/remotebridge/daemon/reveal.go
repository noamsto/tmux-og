package daemon

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// revealQueue is a mutex-guarded set of local window ids a local client has
// started displaying (an attach, switch-client, or window switch inside the
// mirror session). watchReveal adds from its own goroutine; the main loop
// takes, off the goroutine that owns reg and router, in reseedRevealed.
type revealQueue struct {
	mu   sync.Mutex
	wins map[string]struct{}
}

// add queues win as revealed. Safe to call from the watcher goroutine.
func (q *revealQueue) add(win string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.wins == nil {
		q.wins = map[string]struct{}{}
	}
	q.wins[win] = struct{}{}
}

// take returns the queued window ids, sorted, and clears the queue.
func (q *revealQueue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.wins))
	for win := range q.wins {
		out = append(out, win)
	}
	q.wins = nil
	sort.Strings(out)
	return out
}

// clientViewsFormat is the list-clients query watchReveal polls: one row per
// attached client, control-mode leading like viewClientFormat, so the
// "skip this row" test reads the same way. client_created is what tells two
// attaches of the same client NAME apart — a detach/reattach gets a fresh
// created stamp, and that is what a re-attach needs to look like a reveal
// again rather than an unchanged view. client_flags and client_activity trail
// for activityRows; the width/height/status tail feeds clientGeoms, watchReveal's
// geometry signal to the resize watcher (#1266). clientViews reads only the
// first four fields.
const clientViewsFormat = "#{client_control_mode}|#{client_name}|#{client_created}|#{window_id}|#{client_flags}|#{client_activity}|#{client_width}|#{client_height}|#{status}"

// clientViewsArgs returns the list-clients argv for sess.
func clientViewsArgs(sess string) []string {
	return []string{"list-clients", "-t", sess, "-F", clientViewsFormat}
}

// clientViews parses list-clients output lines
// "<control_mode>|<name>|<created>|<window_id>[|...]", skipping control-mode
// (1), blank and malformed lines. The returned map is keyed by "<name>|<created>"
// — a view's identity — mapped to the window id that view is currently
// looking at, which is what revealedWindows diffs against a prior poll.
func clientViews(out string) map[string]string {
	views := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 6)
		if len(fields) < 4 {
			continue
		}
		control, name, created, winID := fields[0], fields[1], fields[2], fields[3]
		if control == "1" {
			continue
		}
		views[name+"|"+created] = winID
	}
	return views
}

// activityRows parses the same list-clients output as clientViews into one
// activityRow per non-control client, skipping blank and malformed lines and
// rows whose activity stamp is missing or unparsable. clientViewsFormat now
// carries a width/height/status tail, so a well-formed row has all nine fields;
// a six-field row predates the tail and is malformed (#1266).
func activityRows(out string) []activityRow {
	var rows []activityRow
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 9)
		if len(fields) != 9 || fields[0] == "1" {
			continue
		}
		stamp, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			continue
		}
		rows = append(rows, activityRow{
			focused: slices.Contains(strings.Split(fields[4], ","), "focused"),
			stamp:   stamp,
			window:  fields[3],
		})
	}
	return rows
}

// geomSignal is the one-shot flag watchReveal sets when the client geometry
// changed and watchLocalClient consumes (#1266). A nil *geomSignal is a no-op.
type geomSignal struct {
	marked atomic.Bool
}

// mark records that the client geometry changed. Safe on a nil receiver.
func (g *geomSignal) mark() {
	if g == nil {
		return
	}
	g.marked.Store(true)
}

// take reports whether a mark was pending and clears it.
func (g *geomSignal) take() bool {
	if g == nil {
		return false
	}
	return g.marked.Swap(false)
}

// clientGeoms maps each non-control view's "<name>|<created>" identity to its
// "<width>|<height>|<status>" geometry, skipping control-mode and malformed
// rows. watchReveal diffs successive polls with maps.Equal: a resize, a
// status-line count change and an attach or detach all move the map (#1266).
func clientGeoms(out string) map[string]string {
	geoms := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 9)
		if len(fields) != 9 || fields[0] == "1" {
			continue
		}
		geoms[fields[1]+"|"+fields[2]] = fields[6] + "|" + fields[7] + "|" + fields[8]
	}
	return geoms
}

// revealedWindows returns, sorted and deduped, the window ids of views in cur
// that were absent from prev or showed a different window there. A view that
// left reveals nothing.
func revealedWindows(prev, cur map[string]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for key, win := range cur {
		if prevWin, ok := prev[key]; ok && prevWin == win {
			continue
		}
		if _, dup := seen[win]; dup {
			continue
		}
		seen[win] = struct{}{}
		out = append(out, win)
	}
	sort.Strings(out)
	return out
}

// wakeCmd is a command whose only purpose is to make the main loop's blocking
// read return: has-session against remoteSession writes nothing, changes
// nothing, and always succeeds while the bridge is attached — output-less and
// side-effect-free, so its %begin/%end only wakes the loop.
func wakeCmd(remoteSession string) string {
	return "has-session -t " + tmuxQuote(remoteSession)
}

// watchReveal polls the mirror session's own clients every tick and queues,
// onto q, the local mirror windows a reveal exposed.
//
// Unlike watchLocalClient it is not gated on the resize nudge. A window switch
// inside the mirror session raises only session-window-changed, and hooking
// that per session would shadow every global session-window-changed hook
// (reflow, mark-seen, the carousel's own reconcile) inside every mirror
// session — the same trap pane-died hit (#647). One local list-clients fork a
// tick is the price of seeing every attach, switch-client and window switch
// without a hook of its own.
//
// query is cfg.LocalTmuxOut bound to clientViewsArgs in production; a query
// error is skipped (prev is kept, not reset), so one failed poll does not
// manufacture a reveal out of an empty read on the next successful one.
// isMirror reports whether a local window id belongs to a mirror — see its
// production doc in Run — filtering out reveals of windows this bridge does
// not own. Every window isMirror accepts is queued, then wake is called once
// per tick that queued at least one, so the main loop's next pass picks the
// batch up together rather than being woken once per window.
//
// placed narrows activity to rows showing a window isMirror accepts: a placed
// host session also holds local windows and other daemons' mirrors, and input
// there is not input on this controller.
//
// geom, when non-nil, is marked on the first successful poll and whenever the
// non-control clients' geometry (name|created -> width|height|status) differs
// from the previous successful poll: the resize watcher's second, hook-
// independent trigger (#1266). A failed query leaves the baseline untouched and
// marks nothing.
func watchReveal(query func() (string, error), isMirror func(localWin string) bool, placed bool,
	q *revealQueue, wake func() bool, activity func(rows []activityRow, now int64),
	geom *geomSignal, stop <-chan struct{}, tick <-chan time.Time,
) {
	var prev map[string]string
	var prevGeom map[string]string
	first := true
	for {
		select {
		case <-stop:
			return
		case <-tick:
			// Taken before the fork: client_activity prints truncated to the
			// second and is compared against now-3, so a now read after the
			// fork would eat into that window.
			now := time.Now().Unix()
			out, err := query()
			if err != nil {
				continue
			}
			cur := clientViews(out)
			curGeom := clientGeoms(out)
			if first || !maps.Equal(prevGeom, curGeom) {
				if sizeDebug.Load() {
					sizeDebugf("clients changed: %s -> %s", sizeGeomSig(prevGeom), sizeGeomSig(curGeom))
				}
				geom.mark()
			}
			prevGeom = curGeom
			first = false
			queued := false
			for _, win := range revealedWindows(prev, cur) {
				if isMirror(win) {
					q.add(win)
					queued = true
				}
			}
			if queued {
				wake()
			}
			prev = cur
			if activity != nil {
				rows := activityRows(out)
				if placed {
					rows = slices.DeleteFunc(rows, func(r activityRow) bool { return !isMirror(r.window) })
				}
				activity(rows, now)
			}
		}
	}
}

// revealable reports whether s is open and unpaused. Unlike takeDirty and
// takeReshaped it does not wait for the sink to drain: the reveal mark isn't
// carried across passes the way theirs is, so a seed dropped onto a full
// queue here just falls through to reseedDropped's own mark on a later pass,
// and a paused pane's %continue already replays a fresh seed on its own.
func (s *outputSink) revealable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && !s.paused
}

// reseedRevealed re-seeds every pane holding a retained kitty store in the
// windows the watcher queued. It runs on the main loop, beside reseedReshaped,
// because that is the only place a round-trip may run and the only goroutine
// that may walk a mirror window's pane lists.
func reseedRevealed(reg *registry, router *Router, rt roundTrip, q *revealQueue) {
	wins := q.take()
	if len(wins) == 0 {
		return
	}
	revealed := map[string]struct{}{}
	for _, win := range wins {
		revealed[win] = struct{}{}
	}
	var ids []string
	var sinks []*outputSink
	for _, mw := range reg.all() {
		if _, ok := revealed[mw.localWin]; !ok {
			continue
		}
		for _, paneID := range mw.allRemotePanes() {
			s := router.sink(paneID)
			if s == nil || !s.revealable() || !s.hasImages.Load() {
				continue
			}
			ids = append(ids, paneID)
			sinks = append(sinks, s)
		}
	}
	PaneSeeds(rt, ids, func(i int, seed []byte, err error) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: re-seed after reveal for %s: %v\n", ids[i], err)
			return
		}
		enqueueSeedWithReplay(sinks[i], seed)
	})
}
