package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// replyRT answers each command in a batch with one canned line, recording what
// was issued. Enough for the shippers, whose reads are single commands.
func replyRT(issued *[]string, reply controlmode.Line) roundTrip {
	return func(cmds ...string) replies {
		i := 0
		return func() (controlmode.Line, bool) {
			if i >= len(cmds) {
				return controlmode.Line{}, false
			}
			*issued = append(*issued, cmds[i])
			i++
			return reply, true
		}
	}
}

func body(s string) controlmode.Line {
	return controlmode.Line{Kind: controlmode.End, Data: []byte(s)}
}

func TestSubscribeCmdIsOneQuotedToken(t *testing.T) {
	got := subscribeCmd(labelSubName, "@*", windowLabelFormat)
	want := `refresh-client -B 'og_labels:@*:` + windowLabelFormat + `'`
	if got != want {
		t.Errorf("cmd = %q, want %q", got, want)
	}
	// The formats are interpolated into a single-quoted argv token, so a quote
	// inside one would need escaping the subscribe path does not do — and the
	// control-mode parser rejects an unquoted '#{...}' outright.
	for _, f := range []string{windowLabelFormat, agentStatusFormat, sessionResFormat} {
		if strings.ContainsAny(f, "'\"") {
			t.Errorf("format must carry no quotes of its own: %q", f)
		}
	}
}

func TestSubscribeFormatsFallsBackToPollingOnError(t *testing.T) {
	var issued []string
	rt := replyRT(&issued, controlmode.Line{Kind: controlmode.Error})
	if labels, agents, _ := subscribeFormats(rt); labels || agents {
		t.Errorf("an %%error must leave both polling shippers polling, got labels=%v agents=%v", labels, agents)
	}

	issued = nil
	rt = replyRT(&issued, body(""))
	labels, agents, res := subscribeFormats(rt)
	if !labels || !agents || !res {
		t.Fatalf("accepted subscriptions must all be marked subscribed, got labels=%v agents=%v res=%v", labels, agents, res)
	}
	// The third carries an EMPTY what, which is the session-scoped spelling —
	// and the one field a unit test cannot prove tmux accepts, since a bad spec
	// is dropped with no %error (see TestSessionResSubscriptionIsSessionScoped).
	if len(issued) != 3 || !strings.Contains(issued[0], "@*") || !strings.Contains(issued[1], "%*") ||
		!strings.Contains(issued[2], resSubName+"::") {
		t.Errorf("issued = %v, want a window, a pane, then a session subscription", issued)
	}
}

func TestSubscriptionValueRoutesByName(t *testing.T) {
	l := controlmode.ParseLine("%subscription-changed " + labelSubName + " $0 @3 2 - : @3|nova")
	if v, ok := subscriptionValue(l, labelSubName); !ok || v != "@3|nova" {
		t.Errorf("own subscription = %q, %v", v, ok)
	}
	// A control client is shared: another subscription's value must not be fed
	// to a shipper as if it were its format.
	if _, ok := subscriptionValue(controlmode.ParseLine("%subscription-changed other $0 @3 2 - : x"), labelSubName); ok {
		t.Error("a foreign subscription must not match")
	}
}

// A snapshot moves every window at once (that is what re-subscribing yields),
// and each reflow is a forced one — so the pass must cost exactly one.
func TestLabelShipperFlushCoalescesTheReflow(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	reflows := 0
	var calls [][]string
	cfg := Config{
		Reflow:    func() { reflows++ },
		LocalTmux: func(args ...string) error { calls = append(calls, args); return nil },
	}
	s := newLabelShipper()
	s.subscribed = true
	s.lastPoll, s.lastGen = time.Now(), reg.gen()

	s.queue("@1|nova|#89b4fa|||||||")
	s.queue("@2|orbit|#89b4fa|||||||")

	var issued []string
	s.flush(cfg, reg, replyRT(&issued, body("")), reg.gen(), true)

	if reflows != 1 {
		t.Errorf("reflows = %d, want 1 for a two-window snapshot", reflows)
	}
	if len(calls) != 2 {
		t.Errorf("stamped %d windows, want 2: %v", len(calls), calls)
	}
	// Subscribed, current generation, backstop not due: nothing may be asked of
	// the remote at all.
	if len(issued) != 0 {
		t.Errorf("issued %v, want no round-trip on the notification path", issued)
	}
}

// The gap a notification cannot close: retireMirror re-adds the same remote id
// against a fresh local window, and addWindow registers a mirror for a window
// whose value was reported before it existed. Neither changes a remote value,
// so only a re-read restores the stamp.
func TestLabelShipperFlushReReadsWhenTheMirrorSetMoves(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	var calls [][]string
	cfg := Config{
		Reflow:    func() {},
		LocalTmux: func(args ...string) error { calls = append(calls, args); return nil },
	}
	s := newLabelShipper()
	s.subscribed = true
	s.lastPoll, s.lastGen = time.Now(), reg.gen()

	var issued []string
	s.flush(cfg, reg, replyRT(&issued, body("")), reg.gen(), true)
	if len(issued) != 0 {
		t.Fatalf("a settled mirror set must not be re-read: %v", issued)
	}

	// The rebuild: same remote id, new local window.
	reg.add("@1", "@202")
	issued = nil
	s.flush(cfg, reg, replyRT(&issued, body("@1|nova|#89b4fa|||||||")), reg.gen(), true)
	if len(issued) != 1 || !strings.Contains(issued[0], "list-windows") {
		t.Fatalf("issued = %v, want one list-windows re-read", issued)
	}
	if len(calls) == 0 {
		t.Error("the re-read must re-stamp the rebuilt mirror")
	}
	for _, c := range calls {
		if got := c[3]; got != "@202" {
			t.Errorf("stamped %q, want the rebuilt local window @202", got)
		}
	}
}

func TestQueuedApplyDueHoldsForABurstButNotForever(t *testing.T) {
	now := time.Now()
	if queuedApplyDue(0, true, time.Time{}) {
		t.Error("nothing queued is nothing to do")
	}
	// Lines still waiting on the pump: the rest of the snapshot is on its way,
	// so this pass holds and the next one applies the whole burst at once.
	if queuedApplyDue(3, false, now) {
		t.Error("queued rows must wait while the pump still holds lines")
	}
	if !queuedApplyDue(3, true, now) {
		t.Error("a drained pump must apply immediately — that is the latency win")
	}
	// A stream that never goes quiet must not hold the rows indefinitely.
	if !queuedApplyDue(3, false, now.Add(-2*queuedApplyMaxHold)) {
		t.Error("maxHold must break a wait for silence that never comes")
	}
}

// The burst path end to end: two notifications arriving while the pump still
// holds lines cost nothing, and the pass that finds it drained applies both
// with a single reflow.
func TestLabelShipperHoldsQueuedRowsUntilThePumpDrains(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	reflows := 0
	var calls [][]string
	cfg := Config{
		Reflow:    func() { reflows++ },
		LocalTmux: func(args ...string) error { calls = append(calls, args); return nil },
	}
	s := newLabelShipper()
	s.subscribed = true
	s.lastPoll, s.lastGen, s.lastApply = time.Now(), reg.gen(), time.Now()

	var issued []string
	rt := replyRT(&issued, body(""))

	s.queue("@1|nova|#89b4fa|||||||")
	s.flush(cfg, reg, rt, reg.gen(), false)
	s.queue("@2|orbit|#89b4fa|||||||")
	s.flush(cfg, reg, rt, reg.gen(), false)
	if reflows != 0 || len(calls) != 0 {
		t.Fatalf("held pass did work: %d reflows, calls %v", reflows, calls)
	}

	s.flush(cfg, reg, rt, reg.gen(), true)
	if reflows != 1 {
		t.Errorf("reflows = %d, want 1 for the drained burst", reflows)
	}
	if len(calls) != 2 {
		t.Errorf("stamped %d windows, want both: %v", len(calls), calls)
	}
}
