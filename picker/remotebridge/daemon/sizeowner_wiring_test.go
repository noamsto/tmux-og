package daemon

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

// testViewing is a view with a sixel relay and a known cell size, so a claim or
// release has both env values to carry.
func testViewing() *Viewing {
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(true, ""))}
	view.SetCell("15x28")
	return view
}

// releaseWant is the release a survivor holding view sends for token|client.
func releaseWant(sessID string, view *Viewing, token, client string) string {
	return ownerReleaseCmd(sessID, token, client,
		RelayEnvCmd(sessID, view.Relay.Load().String()), CellEnvSyncCmd(sessID, view.Cell()))
}

// ownerReadReply is the two-reply batch ownerCtl.read and claim ask for: the
// owner read, then the session's clients.
func ownerReadReply(self, record, clients string) string {
	return "%begin 1 1 1\n" + self + "|" + record + "\n%end 1 1 1\n" +
		"%begin 1 2 1\n" + clients + "\n%end 1 2 1\n"
}

// testOwnerCtl is an ownerCtl over a scripted round-trip, recording its sends.
func testOwnerCtl(script string, own *sizeOwner, cv *converger) (ownerCtl, *[]string) {
	rt, _ := scriptedRT(script)
	sent := &[]string{}
	return ownerCtl{own: own, sessID: "$1", rt: rt, cv: cv, view: testViewing(), send: func(cmds ...string) bool {
		*sent = append(*sent, cmds...)
		return true
	}}, sent
}

// recordedCV is a converger that has asserted a client size, so a flag change
// carries a recalculation trigger.
func recordedCV() *converger {
	cv := newConverger()
	cv.need(clientSizeKey, 100, 40)
	return cv
}

const sessionClientRows = "client-me|1|attached,control-mode\n" +
	"client-b|1|attached,control-mode\n" +
	"/dev/pts/3|0|attached,focused\n" +
	"client-c|1|attached,control-mode,ignore-size"

// TestPrimeClientOwnership: the replacement's old client is closed before
// repair runs, so the prime batch must already carry what the new client needs
// — the claim naming the NEW client when the record is ours, the self-flag when
// another daemon owns — ahead of its size asserts. Reply counting must still
// match: a round-trip after priming gets its own reply.
func TestPrimeClientOwnership(t *testing.T) {
	claim := ownerClaimCmd("$1", tokA, "client-new")
	cases := []struct {
		name, sess, read string
		blocks           int
		first            string
		absent           []string
	}{
		{"ours", "$1", "client-new|" + tokA + "|client-old", 1 + primingCmds, claim, []string{"-f ignore-size", "client-old"}},
		{"foreign", "$1", "client-new|" + tokB + "|client-b", 1 + primingCmds, ignoreSizeCmd("", true), []string{"set-option"}},
		{"unset", "$1", "client-new|", primingCmds, ClientSizeCmd(100, 40), []string{"set-option", "-f ignore-size"}},
		{"off", "", "", primingCmds, ClientSizeCmd(100, 40), []string{bridgeOwnerOption, "-f ignore-size"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			script := ""
			if tc.read != "" {
				script = "%begin 1 1 1\n" + tc.read + "\n%end 1 1 1\n"
			}
			script += replyBlocks(tc.blocks) + "%begin 1 9 1\nfollowing\n%end 1 9 1\n"
			c := newCtlConn(newRecordConn(log, "new", script))
			t.Cleanup(c.close)
			reg := newRegistry()
			reg.add("@1", "@10")
			cfg := Config{LocalArea: func() (int, int) { return 100, 40 }}

			if !primeClient(cfg, c, reg, tc.sess, tokA) {
				t.Fatalf("primeClient failed: %s", log)
			}
			if l, ok := one(c.rt, "list-windows"); !ok || !strings.Contains(string(l.Data), "following") {
				t.Fatalf("the round-trip after priming read %q (ok=%v), want its own reply", l.Data, ok)
			}
			// The batch is one write; everything ahead of the size assert in it
			// is the ownership prefix.
			i := log.find("refresh-client -C 100x40")
			if i < 0 {
				t.Fatalf("the size assert was never written: %s", log)
			}
			batch := log.ev[i]
			if !strings.HasPrefix(batch, "write:new:"+tc.first+"\n") {
				t.Errorf("prime batch = %q, want it to lead with %q", batch, tc.first)
			}
			for _, a := range tc.absent {
				if strings.Contains(log.String(), a) {
					t.Errorf("priming wrote %q: %s", a, log)
				}
			}
		})
	}
}

// TestReplaceConnClaimsForTheNewClientBeforeClosingTheOld: with the record
// still naming the old client when it detaches, every survivor would release
// it and the size would snap to the minimum on each viewer switch.
func TestReplaceConnClaimsForTheNewClientBeforeClosingTheOld(t *testing.T) {
	f := newReplaceFixture(t)
	next := newRecordConn(f.log, "new", identityMatch+
		"%begin 1 1 1\nclient-new|"+tokA+"|client-old\n%end 1 1 1\n"+replyBlocks(1+primingCmds))
	cfg := f.cfg(f.dialing(next))
	cfg.own = newSizeOwner(tokA)

	if _, outcome := replaceConn(cfg, f.router, f.hold, f.want, f.reg, func() bool { return true }); outcome != replaced {
		t.Fatalf("outcome = %v, want replaced", outcome)
	}
	claimed := f.log.find("write:new:" + ownerClaimCmd("$1", tokA, "client-new"))
	closed := f.log.find("close:old")
	if claimed < 0 || closed < 0 || claimed > closed {
		t.Errorf("the claim for the new client must land before the old closes: %s", f.log)
	}
}

func TestOwnerCtlClaimOrder(t *testing.T) {
	own := newSizeOwner(tokA)
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokB+"|client-b", sessionClientRows), own, recordedCV())
	o.claim()
	want := append([]string{ownerClaimCmd("$1", tokA, "client-me")}, o.relayEnv().publish(o.view)...)
	want = append(want,
		ignoreSizeCmd("", false),
		ignoreSizeCmd("client-b", true),
		ClientSizeCmd(100, 40),
	)
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("claim sent %q, want %q", *sent, want)
	}
	if token, client := own.recordedOwner(); token != tokA || client != "client-me" {
		t.Errorf("recorded owner = %q|%q, want ours as client-me", token, client)
	}
}

func TestOwnerCtlClaimIfStale(t *testing.T) {
	const flaggedSelf = "client-me|1|attached,control-mode,ignore-size\n" +
		"client-b|1|attached,control-mode"
	cases := []struct {
		name    string
		record  string
		clients string
		claims  bool
	}{
		{"ours and unflagged sends nothing", tokA + "|client-me", sessionClientRows, false},
		{"ours but self flagged claims", tokA + "|client-me", flaggedSelf, true},
		{"foreign owner claims", tokB + "|client-b", sessionClientRows, true},
		{"unset claims", "", sessionClientRows, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, sent := testOwnerCtl(ownerReadReply("client-me", c.record, c.clients), newSizeOwner(tokA), recordedCV())
			var want []string
			if c.claims {
				want = append([]string{ownerClaimCmd("$1", tokA, "client-me")}, o.relayEnv().publish(o.view)...)
				want = append(want, ignoreSizeCmd("", false), ignoreSizeCmd("client-b", true), ClientSizeCmd(100, 40))
			}
			o.claimIfStale()
			if !reflect.DeepEqual(*sent, want) && (len(*sent) != 0 || len(want) != 0) {
				t.Errorf("sent %q, want %q", *sent, want)
			}
		})
	}
}

func TestOwnerCtlNewConnectionRows(t *testing.T) {
	claim := ownerClaimCmd("$1", tokA, "client-me")
	cases := []struct {
		name, record string
		ownedBefore  bool
		reopened     bool
		clients      string
		first        string
	}{
		{"ours", tokA + "|client-old", false, false, sessionClientRows, claim},
		{"unset, owned before the drop", "", true, false, sessionClientRows, claim},
		{"unset, not owned before", "", false, false, sessionClientRows, ""},
		{"foreign", tokB + "|client-b", true, false, sessionClientRows, ignoreSizeCmd("", true)},
		{"re-open, unset and alone", "", false, true, "client-me|1|attached,control-mode\n/dev/pts/3|0|attached", claim},
		{"re-open, unset with another bridge", "", false, true, sessionClientRows, ""},
		{"re-open, foreign", tokB + "|client-b", false, true, sessionClientRows, ignoreSizeCmd("", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			own := newSizeOwner(tokA)
			if tc.ownedBefore {
				own.noteClaim("client-old")
			}
			// Nothing recorded, as at first attach and right after cv.reset: the
			// size asserts that follow recalculate.
			o, sent := testOwnerCtl(ownerReadReply("client-me", tc.record, tc.clients), own, newConverger())
			if tc.reopened {
				o.reopened()
			} else {
				o.reconnected()
			}
			got := ""
			if len(*sent) > 0 {
				got = (*sent)[0]
			}
			if got != tc.first {
				t.Errorf("sent %q, want it to lead with %q", *sent, tc.first)
			}
			if tc.first == ignoreSizeCmd("", true) && len(*sent) != 1 {
				t.Errorf("self-flag sent %q, want the flag alone with nothing recorded", *sent)
			}
		})
	}
}

// detachedOwner is a survivor whose last report names client-b as the owner.
func detachedOwner() *sizeOwner {
	own := newSizeOwner(tokA)
	own.queueReport("client-me|attached,control-mode,ignore-size|" + tokB + "|client-b")
	return own
}

const flaggedSurvivorRows = "client-me|1|attached,control-mode,ignore-size\n" +
	"client-c|1|attached,control-mode,ignore-size\n" +
	"/dev/pts/3|0|attached,ignore-size"

// TestOwnerCtlReleaseWaitsOutTheGrace: a blip on the active controller must not
// unflag every survivor at once; the release goes out only once due.
func TestOwnerCtlReleaseWaitsOutTheGrace(t *testing.T) {
	own := detachedOwner()
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokB+"|client-b", flaggedSurvivorRows), own, recordedCV())
	o.detached("client-x")
	o.releaseDue(time.Now().Add(2 * ownerReleaseGrace))
	if len(*sent) != 0 {
		t.Fatalf("a non-owner client detaching sent %q", *sent)
	}
	o.detached("client-b")
	o.releaseDue(time.Now())
	if len(*sent) != 0 {
		t.Fatalf("release went out before its grace: %q", *sent)
	}
	o.releaseDue(time.Now().Add(ownerReleaseGrace + time.Second))
	if want := []string{releaseWant("$1", o.view, tokB, "client-b")}; !reflect.DeepEqual(*sent, want) {
		t.Errorf("due release sent %q, want %q", *sent, want)
	}
	*sent = nil
	o.releaseDue(time.Now().Add(2 * ownerReleaseGrace))
	if len(*sent) != 0 {
		t.Errorf("a release went out twice: %q", *sent)
	}
}

// TestOwnerCtlReleaseDroppedWhenTheRecordChanged: the owner reclaiming under a
// new client name, seen in a report or in the due read, cancels the release.
func TestOwnerCtlReleaseDroppedWhenTheRecordChanged(t *testing.T) {
	due := time.Now().Add(ownerReleaseGrace + time.Second)
	t.Run("report", func(t *testing.T) {
		own := detachedOwner()
		o, sent := testOwnerCtl("", own, recordedCV())
		o.detached("client-b")
		own.queueReport("client-me|attached,control-mode,ignore-size|" + tokB + "|client-b2")
		o.releaseDue(due)
		if len(*sent) != 0 {
			t.Errorf("sent %q after the owner reclaimed", *sent)
		}
	})
	t.Run("due read", func(t *testing.T) {
		own := detachedOwner()
		o, sent := testOwnerCtl(ownerReadReply("client-me", tokB+"|client-b2", flaggedSurvivorRows), own, recordedCV())
		o.detached("client-b")
		o.releaseDue(due)
		if len(*sent) != 0 {
			t.Errorf("sent %q after the owner reclaimed", *sent)
		}
	})
}

// TestOwnerCtlReleaseRetriedAfterAFailure: a read or send lost at the due
// moment keeps the release pending, so it goes out once the retry delay passes.
func TestOwnerCtlReleaseRetriedAfterAFailure(t *testing.T) {
	due := time.Now().Add(ownerReleaseGrace + time.Second)
	want := []string{releaseWant("$1", testViewing(), tokB, "client-b")}
	okRead := ownerReadReply("client-me", tokB+"|client-b", flaggedSurvivorRows)
	cases := []struct {
		name        string
		failingRead string
		failSend    bool
	}{
		{"read", "", false},
		{"send", okRead, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			own := detachedOwner()
			failing, sent := testOwnerCtl(tc.failingRead, own, recordedCV())
			if tc.failSend {
				failing.send = func(...string) bool { return false }
			}
			failing.detached("client-b")
			failing.releaseDue(due)
			if len(*sent) != 0 {
				t.Fatalf("a failed attempt sent %q", *sent)
			}
			failing.releaseDue(due)
			if len(*sent) != 0 {
				t.Fatalf("the retry came before its delay: %q", *sent)
			}
			retry, sent := testOwnerCtl(okRead, own, recordedCV())
			retry.releaseDue(due.Add(ownerReleaseRetry))
			if !reflect.DeepEqual(*sent, want) {
				t.Errorf("retry sent %q, want %q", *sent, want)
			}
		})
	}
}

// TestSizeOwnerReholdKeepsANewerRelease: a detach for another owner landing
// between the take and the put-back must not be replaced by the stale one.
func TestSizeOwnerReholdKeepsANewerRelease(t *testing.T) {
	own := detachedOwner()
	now := time.Now()
	own.holdRelease(tokB, "client-b", now)
	r, ok := own.takeDueRelease(now)
	if !ok {
		t.Fatal("release not due")
	}
	own.holdRelease(tokB, "client-b2", now.Add(ownerReleaseGrace))
	own.rehold(r, now.Add(ownerReleaseRetry))
	if got, _ := own.takeDueRelease(now.Add(ownerReleaseGrace)); got.client != "client-b2" {
		t.Errorf("pending release = %q, want client-b2", got.client)
	}
}

// TestOwnerCtlEnforceUnderStaleBelief: the isOwner belief that gates enforce
// can lag a foreign claim; the fresh read naming another token wins.
func TestOwnerCtlEnforceUnderStaleBelief(t *testing.T) {
	own := newSizeOwner(tokA)
	own.noteClaim("client-me")
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokB+"|client-b", sessionClientRows), own, recordedCV())
	o.enforce()
	if len(*sent) != 0 {
		t.Errorf("enforce under a foreign record sent %q", *sent)
	}
}

func TestOwnerCtlMembershipRecalculates(t *testing.T) {
	own := newSizeOwner(tokA)
	own.noteClaim("client-me")
	own.setSelf("client-me")
	own.setMembers([]string{"/dev/pts/3", "client-b"})
	o, sent := testOwnerCtl("", own, recordedCV())

	o.detached("/dev/pts/3")
	o.detached("/dev/pts/3")
	o.sessionChanged("client-b", "$2")
	o.sessionChanged("client-b", "$2")
	want := []string{ClientSizeCmd(100, 40), ClientSizeCmd(100, 40)}
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("sent %q, want one recalculation per member leaving", *sent)
	}
}

func TestOwnerCtlJoinEnforces(t *testing.T) {
	own := newSizeOwner(tokA)
	own.noteClaim("client-me")
	own.setSelf("client-me")
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokA+"|client-me", sessionClientRows), own, recordedCV())

	o.sessionChanged("client-me", "$1")
	if len(*sent) != 0 {
		t.Fatalf("our own session change sent %q", *sent)
	}
	o.sessionChanged("client-b", "$1")
	want := []string{ignoreSizeCmd("client-b", true), ClientSizeCmd(100, 40)}
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("join sent %q, want %q", *sent, want)
	}
}

// TestOwnerCtlEnforceRefreshesMembership: a client that attached while setup
// skipped notifications is known to the owner after enforce, so its later
// detach recalculates.
func TestOwnerCtlEnforceRefreshesMembership(t *testing.T) {
	own := newSizeOwner(tokA)
	own.noteClaim("client-me")
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokA+"|client-me", sessionClientRows), own, recordedCV())

	o.enforce()
	*sent = nil
	o.detached("/dev/pts/3")
	if want := []string{ClientSizeCmd(100, 40)}; !reflect.DeepEqual(*sent, want) {
		t.Errorf("detach of a client enforce discovered sent %q, want %q", *sent, want)
	}
}

func TestOwnerCtlApplyReportReconciles(t *testing.T) {
	own := newSizeOwner(tokA)
	o, sent := testOwnerCtl("", own, recordedCV())
	// Only the newest report counts: the stale one would flag us.
	own.queueReport("client-me|attached,control-mode|" + tokB + "|client-b")
	own.queueReport("client-me|attached,control-mode,ignore-size|" + tokA + "|client-me")
	o.applyReport()
	want := []string{ignoreSizeCmd("", false), ClientSizeCmd(100, 40)}
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("sent %q, want %q", *sent, want)
	}
	if own.selfName() != "client-me" {
		t.Errorf("self = %q, want the report's client", own.selfName())
	}
}

// TestOwnerCtlClaimPublishesRightAfterTheRecord: the env slot moves with
// ownership, guarded, in the same batch and ahead of the flag commands.
func TestOwnerCtlClaimPublishesRightAfterTheRecord(t *testing.T) {
	o, sent := testOwnerCtl(ownerReadReply("client-me", "", sessionClientRows), newSizeOwner(tokA), recordedCV())
	o.claim()
	publish := o.relayEnv().publish(o.view)
	if len(*sent) < 1+len(publish) || (*sent)[0] != ownerClaimCmd("$1", tokA, "client-me") ||
		!reflect.DeepEqual((*sent)[1:1+len(publish)], publish) {
		t.Fatalf("claim sent %q, want the record write then %q", *sent, publish)
	}
	for _, cmd := range publish {
		if !strings.HasPrefix(cmd, "if-shell -F -t '$1' ") || !strings.Contains(cmd, relayOwnedCond(tokA)) {
			t.Errorf("publish %q is not guarded by the owner record", cmd)
		}
	}
}

// TestOwnerCtlReleaseCarriesTheReleasersValues: the release writes the
// releaser's own view, and an unknown cell unsets.
func TestOwnerCtlReleaseCarriesTheReleasersValues(t *testing.T) {
	o, sent := testOwnerCtl(ownerReadReply("client-me", tokB+"|client-b", flaggedSurvivorRows), detachedOwner(), recordedCV())
	o.view = &Viewing{Relay: graphics.NewRelaySource(graphics.NewRelayFromClient(false, ""))}
	o.detached("client-b")
	o.releaseDue(time.Now().Add(ownerReleaseGrace + time.Second))
	want := ownerReleaseCmd("$1", tokB, "client-b", RelayEnvCmd("$1", ""), CellEnvUnsetCmd("$1"))
	if !reflect.DeepEqual(*sent, []string{want}) {
		t.Errorf("release sent %q, want %q", *sent, want)
	}
}

// teardownReply is envTeardown's two-reply batch: "<own client>|<attached>",
// then the session's clients.
func teardownReply(self string, attached int, clients string) string {
	return ownerReadReply(self, strconv.Itoa(attached), clients)
}

func TestOwnerCtlEnvTeardown(t *testing.T) {
	const errorReply = "%begin 1 1 1\nboom\n%error 1 1 1\n%begin 1 2 1\nx\n%end 1 2 1\n"
	cases := []struct {
		name   string
		script string
		want   []string
	}{
		{"another control client", teardownReply("client-me", 3, sessionClientRows),
			[]string{relayEnvRelinquishCmd("$1", tokA)}},
		{"only tty clients", teardownReply("client-me", 2, "client-me|1|attached,control-mode\n/dev/pts/3|0|attached"),
			[]string{relayEnvTeardownCmd("$1", 2)}},
		{"no others", teardownReply("client-me", 1, "client-me|1|attached,control-mode"),
			[]string{relayEnvTeardownCmd("$1", 1)}},
		{"count is session_attached, not rows", teardownReply("client-me", 2,
			"client-me|1|attached,control-mode\n/dev/pts/3|0|attached\n/dev/pts/4|0|attached"),
			[]string{relayEnvTeardownCmd("$1", 2)}},
		{"error reply", errorReply, nil},
		{"lost reply", "", nil},
		{"malformed head", "%begin 1 1 1\nnonsense\n%end 1 1 1\n%begin 1 2 1\nclient-me|1|attached\n%end 1 2 1\n", nil},
		{"empty name", teardownReply("", 1, "client-me|1|attached"), nil},
		{"non-integer count", "%begin 1 1 1\nclient-me|x\n%end 1 1 1\n%begin 1 2 1\nclient-me|1|attached\n%end 1 2 1\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, sent := testOwnerCtl(tc.script, newSizeOwner(tokA), recordedCV())
			o.envTeardown()
			if !reflect.DeepEqual(*sent, tc.want) && (len(*sent) != 0 || len(tc.want) != 0) {
				t.Errorf("sent %q, want %q", *sent, tc.want)
			}
		})
	}
}

// TestOwnerCtlCatchesUpWhenTheViewMoves: the change watcher's guarded write is
// skipped while the record is foreign, so a claim or release that sent the old
// snapshot republishes the new view once it lands, and sends nothing extra when
// the view held still.
func TestOwnerCtlCatchesUpWhenTheViewMoves(t *testing.T) {
	cases := []struct {
		name   string
		run    func(o ownerCtl)
		script string
		own    func() *sizeOwner
	}{
		{"claim", func(o ownerCtl) { o.claim() },
			ownerReadReply("client-me", "", sessionClientRows), func() *sizeOwner { return newSizeOwner(tokA) }},
		{"release", func(o ownerCtl) {
			o.detached("client-b")
			o.releaseDue(time.Now().Add(ownerReleaseGrace + time.Second))
		}, ownerReadReply("client-me", tokB+"|client-b", flaggedSurvivorRows), detachedOwner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, moves := range []bool{false, true} {
				o, sent := testOwnerCtl(tc.script, tc.own(), recordedCV())
				batches := 0
				o.send = func(cmds ...string) bool {
					batches++
					*sent = append(*sent, cmds...)
					if batches == 1 && moves {
						o.view.SetCell("20x40")
					}
					return true
				}
				tc.run(o)
				wantBatches := 1
				if moves {
					wantBatches = 2
				}
				if batches != wantBatches {
					t.Fatalf("moves=%v: %d batches sent, want %d: %q", moves, batches, wantBatches, *sent)
				}
				if !moves {
					continue
				}
				want := o.relayEnv().publishValues("sixel", "20x40")
				if got := (*sent)[len(*sent)-len(want):]; !reflect.DeepEqual(got, want) {
					t.Errorf("catch-up sent %q, want %q", got, want)
				}
			}
		})
	}
}
