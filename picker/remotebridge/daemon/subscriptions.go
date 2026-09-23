package daemon

import (
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// Every shipper here carries state the remote holds in OPTIONS (window, pane,
// session or global), whose change emits no control-stream traffic of its own —
// no %output, no %layout-change, nothing. A control client can subscribe to a
// format instead and be told when its value moves (tmux 3.2):
//
//	refresh-client -B "<name>:<what>:<format>"
//	  -> %subscription-changed <name> $sess @win idx <pane|-> : <value>
//
// One notification per changed object, carrying that object's own value.
// Objects created after the subscription are covered; re-subscribing under the
// same name re-reports every object, which is the cheap way back to ground
// truth. A subscription also reports once on subscribe, including an empty
// value for an unset option — which is what lets "this remote stamps nothing"
// be recognised rather than merely looking silent.
//
// An EMPTY what is the session-scoped spelling: the format is evaluated against
// the control client's own attached session, and reported with no object to
// name — "%subscription-changed <name> $N - - - : <value>".
//
// Each subscription carries the shipper's own -F format verbatim, so the
// notification and the backstop read cannot describe different rows. The window
// and pane formats begin with their object's id, which is why nothing here
// parses the ids out of the notification.
const (
	labelSubName = "og_labels"
	agentSubName = "og_agents"
	resSubName   = "og_res"
	usageSubName = "og_usage"
)

// subscribeCmd builds the subscribe command for one format. Quoted as a single
// argv token: the format holds '#{...}', and control mode's own parser rejects
// it unquoted ("parse error: syntax error") rather than passing it through.
func subscribeCmd(name, what, format string) string {
	return "refresh-client -B " + tmuxQuote(name+":"+what+":"+format)
}

// subscribeFormats installs the subscriptions on the current control client and
// reports, per shipper, whether it may now rely on notifications. Per client,
// not per session, so a reconnect must re-run it — see repair.
//
// A round-trip rather than a bare send, because the answer decides whether the
// 1s poll can stand down: an %error means the remote's tmux predates
// subscriptions, and that shipper keeps polling for the life of the connection.
// The res and usage results are recorded and drive nothing: those shippers
// have no poll mode to fall back to, by design.
func subscribeFormats(rt roundTrip) (labels, agents, res, usage bool) {
	return sendSubscription(rt, labelSubName, "@*", windowLabelFormat),
		sendSubscription(rt, agentSubName, "%*", agentStatusFormat),
		sendSubscription(rt, resSubName, "", sessionResFormat),
		sendSubscription(rt, usageSubName, "", agentUsageFormat)
}

func sendSubscription(rt roundTrip, name, what, format string) bool {
	l, ok := one(rt, subscribeCmd(name, what, format))
	return ok && l.Kind != controlmode.Error
}

// subscriptionValue picks the value out of a %subscription-changed line for
// name, and rejects any other subscription — a control client is shared, and
// another tool's format is not ours to parse.
func subscriptionValue(l controlmode.Line, name string) (string, bool) {
	if len(l.Args) == 0 || l.Args[0] != name {
		return "", false
	}
	return string(l.Data), true
}

// pollFloor is how long a shipper waits between reads of the remote. Subscribed,
// the read is no longer the mechanism but a backstop for a dropped line, so it
// stands down to the longer floor; what a notification genuinely cannot report
// is covered by the generation check in due.
func pollFloor(subscribed bool, live, backstop time.Duration) time.Duration {
	if subscribed {
		return backstop
	}
	return live
}

// queuedApplyMaxHold bounds how long queued rows may wait for the burst to
// drain, so a stream that never goes quiet cannot hold them indefinitely. Half
// the old poll floor, so the subscribed path is never slower than polling was.
const queuedApplyMaxHold = 500 * time.Millisecond

// queuedApplyDue reports whether the rows notifications queued should be applied
// on this pass, where drained is "nothing else is already waiting on the pump".
//
// A snapshot arrives as one notification per object and the main loop runs a
// pass per line, so applying eagerly costs a local fork per shipper — and, for
// labels, a forced reflow — per window and per pane of a burst that is one
// pass's worth of work. Waiting for the pump to go quiet collapses it without a
// timer; maxHold keeps that from becoming a wait for silence that never comes.
func queuedApplyDue(pending int, drained bool, lastApply time.Time) bool {
	if pending == 0 {
		return false
	}
	return drained || time.Since(lastApply) >= queuedApplyMaxHold
}

// due reports whether a read should run now: either the floor has elapsed, or
// the mirror set has moved since the last one.
//
// The generation half is what a notification cannot replace. A window
// registered after its value was last reported, and a retireMirror rebuild
// re-adding a remote id against a fresh local window, both leave a mirror whose
// stamp is missing while the remote value is unchanged — so nothing fires, and
// a bare mirror would keep its stale label until the remote's own next edit.
func due(last time.Time, floor time.Duration, gen, lastGen uint64) bool {
	return gen != lastGen || time.Since(last) >= floor
}
