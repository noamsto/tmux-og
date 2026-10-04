package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// themeProbeRetries and themeProbeInterval bound themeToggleAvailable's wait
// for its own #() job to populate. tmux caches a job by its exact command
// string and runs it asynchronously (measured: the first display-message read
// of a fresh job routinely comes back empty; a second read ~300ms later has
// it), so one empty read is expected steady-state behaviour, not evidence the
// probe failed.
const (
	themeProbeRetries  = 6
	themeProbeInterval = 300 * time.Millisecond
)

// themeToggleAvailable checks, once per bridge connect, whether the remote can
// act on a theme fan-out at all (CLAUDE.md, "Theme fan-out (any light/dark
// toggle)" — theme-toggle ships from the desktop profile, so any headless
// remote lacks it). This is decoupled from the "theme" ctl verb (ctl.go) on
// purpose: that verb is fire-and-forget — the daemon acks its local caller as
// soon as it has queued the remote command, never waiting for the remote's
// exit status — so there is no synchronous channel to learn "applied" vs
// "missing" per toggle. Run() calls this exactly once, right after the
// initial list-windows round-trip: unlike that per-toggle verb, Run() itself
// only executes once per bridge (a reconnect goes through repair, never Run),
// which is what makes the result "once per bridge connect" rather than a
// poll.
//
// It carries the bound connection's reply deadline, like the list-windows
// round-trip immediately before it (bind runs before both, past the identity
// read's armIdentityDeadline): a remote whose default shell wedged fails this
// probe — and startup — at that deadline instead of parking it. The retry
// budget above covers the probe's own transient failures.
func themeToggleAvailable(rt roundTrip, sess string) (available, checked bool) {
	return themeToggleAvailableRetry(rt, sess, themeProbeRetries, themeProbeInterval, time.Sleep)
}

// themeToggleAvailableRetry is themeToggleAvailable's loop, pulled out with
// sleep injected so a test can drive it without a real wait (the same shape
// as retryNotifyLocal below). Each retry re-sends themeProbeCmd rather than
// re-reading a held reply: the round-trip is the only channel back to the
// remote, and re-issuing the identical command string is what lets tmux serve
// the now-populated job cache instead of kicking off a second job.
func themeToggleAvailableRetry(rt roundTrip, sess string, retries int, interval time.Duration, sleep func(time.Duration)) (available, checked bool) {
	cmd := themeProbeCmd(sess)
	for i := range retries {
		l, ok := one(rt, cmd)
		if !ok || l.Kind == controlmode.Error {
			return false, false
		}
		switch strings.TrimSpace(string(l.Data)) {
		case "yes":
			return true, true
		case "no":
			return false, true
		}
		if i < retries-1 {
			sleep(interval)
		}
	}
	return false, false
}

// themeMissingRetries and themeMissingRetryInterval bound notifyThemeMissing's
// wait for a client to attach. scripts/og-remote-open.sh backgrounds the
// daemon and only then runs switch-client, so the very first thing Run() might
// report can race a mirror session that has no client yet; by the time the
// probe's round-trip returns that's usually already resolved, but this is the
// backstop, not the mechanism.
const (
	themeMissingRetries       = 6
	themeMissingRetryInterval = 500 * time.Millisecond
)

// notifyThemeMissing tells the client viewing the mirror, once, that this host
// has no theme-toggle — best-effort and asynchronous like every other
// notifyLocal caller, so it must not block Run()'s setup.
func notifyThemeMissing(cfg Config) {
	msg := fmt.Sprintf(
		"no theme-toggle on %s — mirrored panes will not follow a light/dark toggle (see CLAUDE.md)",
		cfg.RemoteHost)
	go retryNotifyLocal(cfg, msg, themeMissingRetries, themeMissingRetryInterval, time.Sleep)
}

// retryNotifyLocal is notifyThemeMissing's loop, pulled out with sleep
// injected so a test can drive it without a real wait (Backoff, in this same
// package, takes the same approach for the reconnect schedule).
func retryNotifyLocal(cfg Config, msg string, retries int, interval time.Duration, sleep func(time.Duration)) {
	for i := range retries {
		if notifyLocal(cfg, msg) {
			return
		}
		if i < retries-1 {
			sleep(interval)
		}
	}
}
