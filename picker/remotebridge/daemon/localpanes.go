package daemon

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// localPaneListFormat asks tmux for each pane's identity and whether it floats.
// Space-delimited: a pane id and a flag are both bare ASCII, so neither field
// can carry the separator.
const localPaneListFormat = "#{pane_id} #{?pane_floating_flag,1,0}"

// parseLocalPaneList splits a localPaneListFormat listing into the window's
// tiled pane ids, in tmux's own order, and its floating ones. A float occupies
// an ordinal slot like any other pane, so only the tiled list is positionally
// comparable to the remote's pane order.
func parseLocalPaneList(out string) (tiled, floats []string) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		id, flag, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.HasPrefix(id, "%") {
			continue
		}
		if flag == "1" {
			floats = append(floats, id)
			continue
		}
		tiled = append(tiled, id)
	}
	return tiled, floats
}

// refreshLocalPanes re-reads w's tiled pane ids into w.localPanes. This is the
// mirror's only source of local pane identity: the panes are created by
// split-window, which reports nothing back through LocalTmux.
//
// Re-read after each structural change rather than tracked incrementally: a
// mirror window can acquire a pane this daemon never created (a local float),
// so tmux is the only authority on what it holds.
func refreshLocalPanes(cfg Config, w *mirrorWindow) error {
	out, err := cfg.LocalTmuxOut("list-panes", "-t", w.localWin, "-F", localPaneListFormat)
	if err != nil {
		return fmt.Errorf("list-panes %s: %w", w.localWin, err)
	}
	w.localPanes, _ = parseLocalPaneList(out)
	return nil
}

// localPaneAt returns the local pane rendering the i'th remote pane, reporting
// a miss rather than guessing one.
func localPaneAt(w *mirrorWindow, i int) (string, bool) {
	if i < 0 || i >= len(w.localPanes) {
		return "", false
	}
	return w.localPanes[i], true
}

// localWindowGone reports that the mirror's local window is affirmatively no
// longer there. A window leaves by routes this daemon never sees — its last
// renderer pane exiting, a local kill — and a mirrorWindow holds the dead id
// indefinitely, so every command a later pass aims at it can only fail (#487).
//
// Positive evidence only: an unreadable answer returns false and leaves the
// mirror alone, since retiring on a transient read would rebuild a healthy
// window. Hence the whole session's listing rather than a lookup aimed at the
// window itself: `display-message -p -t @<dead>` exits 0 with empty output
// (#152/#169), so a lookup that fails for any other reason cannot be told from
// a live window. A listing fails as a whole or answers in full, and absence
// from a complete reply is evidence the target could never give.
func localWindowGone(cfg Config, localWin string) bool {
	live, ok := localWindowSet(cfg)
	if !ok {
		return false
	}
	return !live[localWin]
}

// localWindowSet is that listing, for a caller asking about every mirror at
// once: one fork answers for the whole registry. ok is false when the listing
// could not be made — same positive-evidence rule as above.
func localWindowSet(cfg Config) (map[string]bool, bool) {
	if cfg.LocalTmuxOut == nil {
		return nil, false
	}
	out, err := cfg.LocalTmuxOut("list-windows", "-t", cfg.LocalSess, "-F", "#{window_id}")
	if err != nil {
		return nil, false
	}
	live := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		live[strings.TrimSpace(line)] = true
	}
	return live, true
}

// localSessionGone reports that cfg.LocalSess is affirmatively gone.
//
// The local session vanishing is none of Run's other terminal endings: the
// registry holds *remote* window ids and they are all still there, the control
// connection stays healthy, and every local command failing is individually
// survivable — localWindowSet is positive-evidence-only precisely so a transient
// read cannot tear down a healthy mirror. A session that is permanently gone
// therefore reads like a blip forever (#680).
//
// has-session answers what list-windows cannot. A listing error is
// indistinguishable from a transient one; has-session's own exit status is
// tmux's answer — status 1 is its "can't find session" (or "no server running",
// which means the same thing for a session that lived on that server). Only
// that definite negative is evidence: tmux failing to start at all, or exiting
// with any other status, is a question that could not be asked, and nothing may
// be torn down on it. Positive evidence only, the same rule as localWindowGone.
//
// -t "=<name>" is the exact match og-remote-open.sh's own liveness check uses,
// so a sibling session whose name has ours as a prefix cannot read here as
// alive.
func localSessionGone(cfg Config) bool {
	if cfg.LocalTmuxOut == nil || cfg.LocalSess == "" {
		return false
	}
	_, err := cfg.LocalTmuxOut("has-session", "-t", "="+cfg.LocalSess)
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

// sessionGoneStrikes is how many consecutive definite negatives localSessionGone
// must return before the daemon treats the mirror session as gone. The probe
// already filters a question that could not be asked, so this is the second
// line of defence: one spurious negative must not tear a healthy mirror down,
// and a single affirmative answer clears the count.
const sessionGoneStrikes = 2

// sessionGoneTracker carries that count across the coarse tick's passes. It is
// session-lifetime state, like the registry: a reconnect does not make a gone
// session come back, so a strike survives one.
type sessionGoneTracker struct{ strikes int }

// observe records one probe result and reports whether the session is to be
// treated as gone.
func (t *sessionGoneTracker) observe(gone bool) bool {
	if !gone {
		t.strikes = 0
		return false
	}
	t.strikes++
	return t.strikes >= sessionGoneStrikes
}
