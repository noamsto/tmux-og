package daemon

import (
	"reflect"
	"strings"
	"testing"
)

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
	return ownerCtl{own: own, sessID: "$1", rt: rt, cv: cv, send: func(cmds ...string) bool {
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
	want := []string{
		ownerClaimCmd("$1", tokA, "client-me"),
		ignoreSizeCmd("", false),
		ignoreSizeCmd("client-b", true),
		ClientSizeCmd(100, 40),
	}
	if !reflect.DeepEqual(*sent, want) {
		t.Errorf("claim sent %q, want %q", *sent, want)
	}
	if token, client := own.recordedOwner(); token != tokA || client != "client-me" {
		t.Errorf("recorded owner = %q|%q, want ours as client-me", token, client)
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

func TestOwnerCtlDetachedReleasesAForeignOwner(t *testing.T) {
	own := newSizeOwner(tokA)
	own.queueReport("client-me|attached,control-mode,ignore-size|" + tokB + "|client-b")
	o, sent := testOwnerCtl("", own, recordedCV())
	o.detached("client-x")
	if len(*sent) != 0 {
		t.Fatalf("a non-owner client detaching sent %q", *sent)
	}
	o.detached("client-b")
	if want := []string{ownerReleaseCmd("$1", tokB, "client-b")}; !reflect.DeepEqual(*sent, want) {
		t.Errorf("owner detaching sent %q, want %q", *sent, want)
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
