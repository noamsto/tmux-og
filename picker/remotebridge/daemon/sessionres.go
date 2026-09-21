package daemon

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sessionResFormat reads whatever the remote's own resource poller stamped on
// the mirrored session: "<cpu> <mem> <cores> <tick> <agents>", where cpu is a
// raw per-core ps %CPU sum, mem is MB and cores is the REMOTE host's core count
// — carried because the local reader tints against the owning machine's count,
// not its own. agents is the agent commands found in the session's process
// tree, comma-joined, or "-" for none. The tick is the remote's epoch and is
// read by nothing: it exists so the value moves on every poller pass, which is
// the only way a subscription can witness that the poller is still alive — a
// tmux option outlives the process that wrote it, so re-reading one proves
// nothing.
// Unquoted: it is a subscription format, and the call site owns the quoting.
const sessionResFormat = "#{@og_session_res}"

// sessionResTickMaxAge is how old the remote's own tick may be, on our clock,
// before a report is dropped as a dead poller's leftover. The subscription
// reports the option's current value on every subscribe, and an option outlives
// the poller that wrote it — so a remote that stopped running one (downgraded,
// or a server that lost the hook) would otherwise be stamped fresh on every
// attach and every reconnect. A live poller's value is at most one 5s pass old
// when it arrives.
const sessionResTickMaxAge = 30 * time.Second

// sessionResRefresh is the floor at which an UNCHANGED row is re-stamped. The
// epoch field the daemon writes is a local receive time the picker ages
// against, so a mirror whose figures genuinely hold still would otherwise age
// out of that window and read as dead. Comfortably above mainLoopTickInterval,
// so the re-stamp is a floor and not a race with the loop's own wake-up.
const sessionResRefresh = 30 * time.Second

// sessionResMaxLen caps the value before the regex runs, the length cap every
// carried @bridge_* value has: the agents list is otherwise unbounded, and the
// picker merges it into a proc list per session on every 1s rebuild. Four
// manifest names and the numbers fit in well under half of it.
const sessionResMaxLen = 128

// sessionResRe matches the whole value or nothing, the identity-field policy
// every non-display @bridge_* value here follows: a truncated number is a
// different number, and there is no partial reading of a CPU figure worth
// stamping. '|' is excluded by construction rather than by a strip pass —
// the picker reads session-scoped @bridge_* out of a '|'-delimited
// list-panes -a row whose parse fails CLOSED on a wrong field count, so a pipe
// here does not garble a column, it drops the whole session from the picker.
var sessionResRe = regexp.MustCompile(`^[0-9]+(\.[0-9])? [0-9]+(\.[0-9])? [0-9]+(\.[0-9])? [0-9]+ (-|[a-z0-9][a-z0-9._-]*(,[a-z0-9][a-z0-9._-]*)*)$`)

// resShipper stamps the remote session's own CPU/mem figures onto the local
// mirror session as @bridge_res, so the picker's columns measure the remote
// rather than the renderers its local panes actually run.
//
// Unlike its two siblings this shipper has no poll mode and no backstop read:
// the value is one session option, the subscription reports it on subscribe
// (including an empty one when the remote carries no poller), and repair's
// re-subscribe is what recovers it after an outage.
type resShipper struct {
	// session is the pinned remote session's id. The subscription is
	// session-scoped against the control client's CURRENT session, so during a
	// session-pin excursion (#396) it reports another session's value; a report
	// naming any other id is that, and is ignored. Empty accepts every report,
	// the posture sessionPin itself takes when it could not read the id.
	session string
	// skew is localNow - remoteNow, for aging the remote's tick on our clock.
	skew int64

	// pending is the raw value the last notification carried, applied by flush
	// rather than by the dispatch that queued it.
	pending     string
	havePending bool

	figures   string    // every field but the tick, as last written, for the unchanged-row check
	lastWrite time.Time // when they were written, for the refresh floor

	// subscribed is recorded per connection by Run and drives nothing: there is
	// no poll to fall back to. It could not be trusted for that anyway —
	// cmd_refresh_client_update_subscription silently DROPS a subscription it
	// cannot parse and returns no %error, so an accepted-looking reply is not
	// evidence the spec parsed.
	subscribed bool
}

func newResShipper(session string, skew int64) *resShipper {
	return &resShipper{session: session, skew: skew}
}

// reskew re-points the shipper at a freshly measured clock offset.
func (r *resShipper) reskew(skew int64) { r.skew = skew }

// queue records the value a %subscription-changed line carried, from the
// session id that line names. Pure: it is called from dispatch, which may
// itself be running inside a reply reader's drain, so it must not read the
// remote or fork tmux.
func (r *resShipper) queue(session, v string) {
	if r.session != "" && session != r.session {
		return
	}
	r.pending, r.havePending = v, true
}

// flush stamps whatever the last notification queued. Main loop only, but it
// issues no round-trip: everything it needs already arrived on the stream.
//
// The nothing-pending return is first and unconditional. Without it the loop's
// own 5s tick re-enters holding a stale pending, and the refresh floor below
// stops being a floor: it becomes an unconditional re-stamp of whatever arrived
// last, including re-stamping a value the empty branch had just unset.
func (r *resShipper) flush(cfg Config) {
	if !r.havePending {
		return
	}
	v := r.pending
	r.pending, r.havePending = "", false
	if cfg.LocalSess == "" {
		return
	}
	if v == "" {
		// The remote runs no resource poller, or its own teardown cleared the
		// option. Either way there is nothing to measure and the picker must
		// render '-' rather than the figures of whenever it last did.
		r.figures = ""
		cfg.LocalTmux("set-option", "-u", "-t", cfg.LocalSess, "@bridge_res")
		return
	}
	if len(v) > sessionResMaxLen || !sessionResRe.MatchString(v) {
		// Dropped whole, leaving the previous stamp standing: a malformed value
		// is evidence about the remote's poller, not about the figures, and
		// stale-but-real beats plausibly-wrong.
		return
	}
	f := strings.Fields(v)
	if tick, _ := strconv.ParseInt(f[3], 10, 64); time.Now().Unix()-(tick+r.skew) > int64(sessionResTickMaxAge/time.Second) {
		// No poller moved this value recently: it is a leftover, and stamping it
		// with our receive time would present it as a live reading. Dropped,
		// not unset — a report this stale arrives only on subscribe, after
		// reattach has already cleared the stamp.
		return
	}
	figures := strings.Join([]string{f[0], f[1], f[2], f[4]}, " ")
	if figures == r.figures && time.Since(r.lastWrite) < sessionResRefresh {
		// This is what makes the remote's per-pass tick free locally: it moves
		// every pass by design, and it is the only field that moved.
		return
	}
	r.figures, r.lastWrite = figures, time.Now()
	// The epoch is the DAEMON'S OWN receive time, never the remote's tick: the
	// reader compares it against its own clock, so there is no skew to correct
	// for — and it is what lets an orphaned mirror, whose daemon was killed
	// without teardown, age out with no cooperation from the corpse.
	cfg.LocalTmux("set-option", "-t", cfg.LocalSess, "@bridge_res",
		fmt.Sprintf("%s %s %s %d %s", f[0], f[1], f[2], time.Now().Unix(), f[4]))
}

// reset forgets what was written, so the next report stamps even if it is
// identical — repair calls it before re-subscribing, and the re-report is then
// what puts back the stamp the outage dropped. pending goes with it: a value
// queued just before the drop describes the mirror as it was, and applying it
// afterwards would date it to the receive time of a reading taken before the
// outage.
func (r *resShipper) reset() {
	r.figures, r.lastWrite = "", time.Time{}
	r.pending, r.havePending = "", false
}

// clear drops the stamp while the mirror session still exists. Near-vacuous in
// production for the same reason labelShipper.clear is — teardown ends in
// kill-session and session options die with it — but it is the path that runs
// when that kill fails.
func (r *resShipper) clear(cfg Config) { clearBridgeRes(cfg) }
