package daemon

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const (
	tokA = "0123456789abcdef"
	tokB = "fedcba9876543210"
)

func TestSizeOwnerToken(t *testing.T) {
	a, b := newOwnerToken(), newOwnerToken()
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) {
		t.Errorf("token %q is not 16 lower-case hex chars", a)
	}
	if a == b {
		t.Errorf("two tokens collide: %q", a)
	}
}

func TestParseOwnerValue(t *testing.T) {
	cases := []struct {
		name, in, token, client string
	}{
		{"good", tokA + "|client-123", tokA, "client-123"},
		{"slash and dot", tokA + "|/dev/pts/3.x", tokA, "/dev/pts/3.x"},
		{"surrounding space", "  " + tokA + "|c1\n", tokA, "c1"},
		{"short token", "0123456789abcde|c1", "", ""},
		{"long token", tokA + "0|c1", "", ""},
		{"uppercase hex", "0123456789ABCDEF|c1", "", ""},
		{"hash in client", tokA + "|c#1", "", ""},
		{"comma in client", tokA + "|c,1", "", ""},
		{"space in client", tokA + "|c 1", "", ""},
		{"brace in client", tokA + "|c}1", "", ""},
		{"quote in client", tokA + "|c'1", "", ""},
		{"empty client", tokA + "|", "", ""},
		{"client too long", tokA + "|" + strings.Repeat("a", 65), "", ""},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token, client := parseOwnerValue(c.in)
			if token != c.token || client != c.client {
				t.Errorf("parseOwnerValue(%q) = %q, %q; want %q, %q", c.in, token, client, c.token, c.client)
			}
		})
	}
}

func TestParseSelfReport(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want selfReport
		ok   bool
	}{
		{"unflagged owner ours", "c1|attached,focused|" + tokA + "|c1",
			selfReport{client: "c1", ownerToken: tokA, ownerClient: "c1"}, true},
		{"flagged", "c1|attached,ignore-size|" + tokB + "|c2",
			selfReport{client: "c1", flagged: true, ownerToken: tokB, ownerClient: "c2"}, true},
		{"flag prefix does not count", "c1|ignore-size-x,attached|",
			selfReport{client: "c1"}, true},
		{"flag suffix does not count", "c1|x-ignore-size|",
			selfReport{client: "c1"}, true},
		{"owner field missing", "c1|ignore-size", selfReport{client: "c1", flagged: true}, true},
		{"owner field empty", "c1|attached|", selfReport{client: "c1"}, true},
		{"owner garbage", "c1|attached|nonsense", selfReport{client: "c1"}, true},
		{"owner with # rejected", "c1|attached|" + tokA + "|c#1", selfReport{client: "c1"}, true},
		{"one field", "c1", selfReport{}, false},
		{"empty client", "|attached|" + tokA + "|c1", selfReport{}, false},
		{"empty", "", selfReport{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseSelfReport(c.in)
			if ok != c.ok || got != c.want {
				t.Errorf("parseSelfReport(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSizeOwnerCmdBuilders(t *testing.T) {
	cases := []struct {
		name, got, want string
	}{
		{"ignore self on", ignoreSizeCmd("", true), "refresh-client -f ignore-size"},
		{"ignore self off", ignoreSizeCmd("", false), "refresh-client -f !ignore-size"},
		{"ignore other on", ignoreSizeCmd("client-12", true), "refresh-client -t 'client-12' -f ignore-size"},
		{"ignore other off", ignoreSizeCmd("/dev/pts/3", false), "refresh-client -t '/dev/pts/3' -f !ignore-size"},
		{"claim", ownerClaimCmd("$4", tokA, "/dev/pts/3"),
			"set-option -t '$4' @og_bridge_owner '" + tokA + "|/dev/pts/3'"},
		{"release", ownerReleaseCmd("$4", tokB, "/dev/pts/9"),
			"if-shell -F -t '$4' '#{==:#{@og_bridge_owner}," + tokB + "|/dev/pts/9}' 'set-option -u -t '\\''$4'\\'' @og_bridge_owner'"},
		{"read", ownerReadCmd("$4"),
			"display-message -p -t '$4' '" + ctlSafe("#{client_name}|#{@og_bridge_owner}") + "'"},
		{"session clients", sessionClientsCmd("$4"),
			"list-clients -t '$4' -F '" + ctlSafe("#{client_name}|#{client_control_mode}|#{client_flags}") + "'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got  %s\nwant %s", c.got, c.want)
			}
		})
	}
}

func TestSizeOwnerParseSessionClients(t *testing.T) {
	body := "c1|1|attached,ignore-size\n" +
		"\n" +
		"/dev/ttys001|0|attached,focused\n" +
		"malformed\n" +
		"|1|attached\n" +
		"c2|1|attached"
	want := []sessionClient{
		{name: "c1", control: true, flagged: true},
		{name: "/dev/ttys001"},
		{name: "c2", control: true},
	}
	if got := parseSessionClients(body); !reflect.DeepEqual(got, want) {
		t.Errorf("parseSessionClients = %+v, want %+v", got, want)
	}
}

func TestEnforceTargets(t *testing.T) {
	clients := []sessionClient{
		{name: "me", control: true},
		{name: "tty", control: false},
		{name: "done", control: true, flagged: true},
		{name: "b1", control: true},
		{name: "b2", control: true},
	}
	cases := []struct {
		name    string
		clients []sessionClient
		self    string
		want    []string
	}{
		{"skips self, tty and flagged, keeps order", clients, "me", []string{"b1", "b2"}},
		{"self elsewhere is a target", clients, "other", []string{"me", "b1", "b2"}},
		{"none", nil, "me", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := enforceTargets(c.clients, c.self); !reflect.DeepEqual(got, c.want) {
				t.Errorf("enforceTargets = %v, want %v", got, c.want)
			}
		})
	}
}

func TestReconcileCmd(t *testing.T) {
	cases := []struct {
		name string
		r    selfReport
		want string
	}{
		{"ours and flagged unflags", selfReport{flagged: true, ownerToken: tokA}, "refresh-client -f !ignore-size"},
		{"ours and unflagged is quiet", selfReport{ownerToken: tokA}, ""},
		{"other and unflagged flags", selfReport{ownerToken: tokB}, "refresh-client -f ignore-size"},
		{"other and flagged is quiet", selfReport{flagged: true, ownerToken: tokB}, ""},
		{"unset and flagged unflags", selfReport{flagged: true}, "refresh-client -f !ignore-size"},
		{"unset and unflagged is quiet", selfReport{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := reconcileCmd(c.r, tokA)
			if got != c.want || ok != (c.want != "") {
				t.Errorf("reconcileCmd(%+v) = %q, %v; want %q", c.r, got, ok, c.want)
			}
		})
	}
}

func TestSizeOwnerForeign(t *testing.T) {
	cases := []struct {
		owner string
		want  bool
	}{{"", false}, {tokA, false}, {tokB, true}}
	for _, c := range cases {
		if got := foreignOwner(c.owner, tokA); got != c.want {
			t.Errorf("foreignOwner(%q) = %v, want %v", c.owner, got, c.want)
		}
	}
}

func TestActivityMark(t *testing.T) {
	const now = 1000
	type step struct {
		rows []activityRow
		now  int64
		want bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"first call only seeds", []step{
			{[]activityRow{{true, now, ""}}, now, false},
		}},
		{"focused fresh stamp advances", []step{
			{[]activityRow{{true, now - 50, ""}}, now - 1, false},
			{[]activityRow{{true, now, ""}}, now, true},
		}},
		{"same stamp twice counts once", []step{
			{nil, now - 1, false},
			{[]activityRow{{true, now, ""}}, now, true},
			{[]activityRow{{true, now, ""}}, now + 1, false},
		}},
		{"equal to high does not advance", []step{
			{[]activityRow{{true, now - 1, ""}}, now - 1, false},
			{[]activityRow{{true, now - 1, ""}}, now, false},
		}},
		{"focus-out stamp is raised but not counted", []step{
			{[]activityRow{{true, now - 50, ""}}, now - 1, false},
			{[]activityRow{{false, now, ""}}, now, false},
			{[]activityRow{{true, now, ""}}, now + 1, false},
		}},
		{"stale stamp on a moved-in client", []step{
			{nil, now - 1, false},
			{[]activityRow{{true, now - 10, ""}}, now, false},
		}},
		{"recency edge", []step{
			{nil, now - 1, false},
			{[]activityRow{{true, now - 3, ""}}, now, true},
		}},
		{"any counted row wins", []step{
			{nil, now - 1, false},
			{[]activityRow{{false, now, ""}, {true, now - 30, ""}, {true, now, ""}}, now, true},
		}},
		{"no rows", []step{
			{nil, now, false},
			{nil, now + 1, false},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m activityMark
			for i, s := range c.steps {
				if got := m.advance(s.rows, s.now); got != s.want {
					t.Errorf("step %d: advance(%+v, %d) = %v, want %v", i, s.rows, s.now, got, s.want)
				}
			}
		})
	}
}

func TestSizeOwnerQueueKeepsNewestReport(t *testing.T) {
	o := newSizeOwner(tokA)
	if _, ok := o.takeReport(); ok {
		t.Fatal("fresh sizeOwner has a report")
	}
	o.queueReport("c1|attached|")
	o.queueReport("garbage")
	o.queueReport("c1|ignore-size|" + tokB + "|c2")
	r, ok := o.takeReport()
	if !ok || !r.flagged || r.ownerToken != tokB {
		t.Fatalf("takeReport = %+v, %v; want the newest valid report", r, ok)
	}
	if _, ok := o.takeReport(); ok {
		t.Error("takeReport did not consume the report")
	}
}

func TestSizeOwnerOwnership(t *testing.T) {
	o := newSizeOwner(tokA)
	if o.isOwner() {
		t.Fatal("fresh sizeOwner owns")
	}
	o.noteClaim("c1")
	if !o.isOwner() || !o.ownedBefore {
		t.Fatalf("after noteClaim: isOwner=%v ownedBefore=%v", o.isOwner(), o.ownedBefore)
	}
	o.queueReport("c1|attached|" + tokA + "|c1")
	if !o.isOwner() || !o.ownedBefore {
		t.Error("a report naming us changed ownership")
	}
	o.queueReport("c1|ignore-size|" + tokB + "|c2")
	if o.isOwner() || o.ownedBefore {
		t.Errorf("after a foreign report: isOwner=%v ownedBefore=%v", o.isOwner(), o.ownedBefore)
	}
	o.noteClaim("c1")
	o.queueReport("c1|attached|")
	if o.isOwner() || !o.ownedBefore {
		t.Errorf("after an unset report: isOwner=%v ownedBefore=%v; an unset record keeps ownedBefore", o.isOwner(), o.ownedBefore)
	}
}

func TestSizeOwnerNoteActivity(t *testing.T) {
	const now = 1000
	fresh := []activityRow{{true, now, ""}}
	cases := []struct {
		name      string
		owner     bool
		want      bool
		wantWoken bool
	}{
		{"not owner wants a claim", false, true, true},
		{"owner wants one too: the belief can lag", true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newSizeOwner(tokA)
			o.noteActivity(nil, now-1)
			if c.owner {
				o.noteClaim("c1")
			}
			o.noteActivity(fresh, now)
			if got := o.takeClaim(); got != c.want {
				t.Errorf("takeClaim = %v, want %v", got, c.want)
			}
			if o.takeClaim() {
				t.Error("takeClaim did not clear the claim")
			}
			select {
			case <-o.wake:
				if !c.wantWoken {
					t.Error("woke the loop")
				}
			default:
				if c.wantWoken {
					t.Error("did not wake the loop")
				}
			}
		})
	}
}

func TestSizeOwnerWakeDoesNotBlock(t *testing.T) {
	o := newSizeOwner(tokA)
	o.noteActivity(nil, 99)
	o.noteActivity([]activityRow{{true, 100, ""}}, 100)
	o.noteActivity([]activityRow{{true, 101, ""}}, 101)
	if cap(o.wake) != 1 || len(o.wake) != 1 {
		t.Errorf("wake len/cap = %d/%d, want 1/1", len(o.wake), cap(o.wake))
	}
}

func TestSizeOwnerSetSelf(t *testing.T) {
	o := newSizeOwner(tokA)
	o.setSelf("c1")
	if o.self != "c1" {
		t.Errorf("self = %q, want c1", o.self)
	}
}

func TestSizeOwnerMembers(t *testing.T) {
	o := newSizeOwner(tokA)
	if o.dropMember("a") {
		t.Error("dropMember on an empty set reported present")
	}
	o.setMembers([]string{"a", "b"})
	o.addMember("c")
	if !o.dropMember("a") || o.dropMember("a") {
		t.Error("dropMember must report presence once")
	}
	o.setMembers([]string{"z"})
	if o.dropMember("b") || o.dropMember("c") {
		t.Error("setMembers must replace the set")
	}
	if !o.dropMember("z") {
		t.Error("setMembers lost a member")
	}
}

func TestRecalcCmd(t *testing.T) {
	cv := newConverger()
	if cmd, ok := recalcCmd(cv); ok {
		t.Fatalf("recalcCmd on an empty converger = %q, true", cmd)
	}
	cv.need("@3", 100, 30)
	if _, ok := recalcCmd(cv); ok {
		t.Error("a per-window cap must not stand in for the client size")
	}
	cv.need(clientSizeKey, 210, 52)
	if cmd, ok := recalcCmd(cv); !ok || cmd != "refresh-client -C 210x52" {
		t.Errorf("recalcCmd = %q, %v", cmd, ok)
	}
	cv.reset()
	if _, ok := recalcCmd(cv); ok {
		t.Error("recalcCmd after reset still reports a size")
	}
}
