package daemon

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
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
// again rather than an unchanged view.
const clientViewsFormat = "#{client_control_mode}|#{client_name}|#{client_created}|#{window_id}"

// clientViewsArgs returns the list-clients argv for sess.
func clientViewsArgs(sess string) []string {
	return []string{"list-clients", "-t", sess, "-F", clientViewsFormat}
}

// clientViews parses list-clients output lines
// "<control_mode>|<name>|<created>|<window_id>", skipping control-mode (1),
// blank and malformed lines. The returned map is keyed by "<name>|<created>"
// — a view's identity — mapped to the window id that view is currently
// looking at, which is what revealedWindows diffs against a prior poll.
func clientViews(out string) map[string]string {
	views := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 4)
		if len(fields) != 4 {
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

// revealedWindows returns, sorted and deduped, the window ids of views in cur
// whose window differs from what that same view (by "<name>|<created>" key)
// showed in prev — a fresh view (key absent from prev), a re-attach (same
// name, new created), or a window switch (same view, new window_id). A view
// present only in prev (one that left) contributes nothing: leaving reveals
// nothing.
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
func watchReveal(query func() (string, error), isMirror func(localWin string) bool,
	q *revealQueue, wake func() bool, stop <-chan struct{}, tick <-chan time.Time,
) {
	var prev map[string]string
	for {
		select {
		case <-stop:
			return
		case <-tick:
			out, err := query()
			if err != nil {
				continue
			}
			cur := clientViews(out)
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

// reseedRevealed repaints every retained-store pane in a window a local
// client just started displaying. Called from the main loop beside
// reseedReshaped, which is the only place a round-trip may run: q.take()
// drains the windows the watcher goroutine queued (main-loop owned reg and
// router are never touched off this goroutine — mw.allRemotePanes() walks the
// registry's pane lists here, not in the watcher), and only a pane whose sink
// is revealable and still holds a retained kitty store is worth the
// round-trip.
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
