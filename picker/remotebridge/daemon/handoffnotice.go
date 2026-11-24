package daemon

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// handoffWindow bounds how long after a hand-off press a failure to land is
// still blamed on it; a later remote navigation is ordinary and stays quiet.
const handoffWindow = 60 * time.Second

// The notice texts carry nothing the remote said: they are rendered by the
// local tmux as plain message text.
const (
	noticeWindowNotMirrored  = "tmux-og: hand-off window is not mirrored here"
	noticeHandOffFailed      = "tmux-og: could not open the remote session as a mirror"
	noticeSessionNotFollowed = "tmux-og: switching remote session is not followed in this mirror"
)

// handoffWatch records that a hand-off press was just sent, so the main loop
// can tell a failure to land from ordinary remote navigation. The ctl
// goroutine arms and the main loop takes, so it has its own mutex, held only
// for the field access and never across a send. The zero value is usable.
type handoffWatch struct {
	mu    sync.Mutex
	now   func() time.Time
	armed time.Time
}

func (w *handoffWatch) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *handoffWatch) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = w.clock()
}

// take reports whether a press armed the watch within handoffWindow, and
// clears it: one press raises at most one notice.
func (w *handoffWatch) take() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	live := !w.armed.IsZero() && w.clock().Sub(w.armed) <= handoffWindow
	w.armed = time.Time{}
	return live
}

func (w *handoffWatch) disarm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = time.Time{}
}

// notifyClients shows msg for five seconds on every non-control client of
// target. A placed mirror shares its host session with local windows, so own
// narrows the clients to those looking at one of this daemon's windows; nil
// (a dedicated session) keeps them all.
func notifyClients(out func(...string) (string, error), run func(...string) error, target, msg string, own func(win string) bool) {
	rows, err := out("list-clients", "-t", target, "-F", "#{client_control_mode}|#{client_name}|#{window_id}")
	if err != nil {
		return
	}
	for row := range strings.SplitSeq(rows, "\n") {
		f := strings.SplitN(row, "|", 3)
		if len(f) != 3 || f[0] != "0" || f[1] == "" {
			continue
		}
		if own != nil && !own(f[2]) {
			continue
		}
		_ = run("display-message", "-d", "5000", "-c", f[1], msg)
	}
}

// notifyHost is notifyClients against cfg's host session.
func notifyHost(cfg Config, msg string) {
	if cfg.LocalTmuxOut == nil || cfg.LocalTmux == nil {
		return
	}
	var own func(string) bool
	if cfg.Placed {
		own = func(win string) bool {
			ids, ok := ownWindows(cfg)
			if !ok {
				return false
			}
			return slices.Contains(ids, win)
		}
	}
	notifyClients(cfg.LocalTmuxOut, cfg.LocalTmux, cfg.target(), msg, own)
}
