package daemon

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// scriptedRT drives a roundTrip against a canned reply stream, capturing every
// command the pin sends. Blocks in script must be numbered from seq 1.
func scriptedRT(script string) (rt roundTrip, sent *bytes.Buffer) {
	sent = &bytes.Buffer{}
	return newRoundTrip(newTestReader(script), NewRouter(), &asyncQueue{}, newStream(sent)), sent
}

func TestParseSessionChanged(t *testing.T) {
	l := controlmode.ParseLine("%session-changed $5 my proj")
	if l.Kind != controlmode.SessionChanged {
		t.Fatalf("kind = %v, want SessionChanged", l.Kind)
	}
	if len(l.Args) != 1 || l.Args[0] != "$5" {
		t.Errorf("args = %v, want [$5]", l.Args)
	}
	// A session name may hold spaces, so the name is the whole rest, not Fields.
	if string(l.Data) != "my proj" {
		t.Errorf("name = %q, want %q", l.Data, "my proj")
	}
}

// An id equal to ours is either the attach-time notification or our own switch
// back landing — neither is an excursion, and reacting to the latter would
// switch in a loop.
func TestSessionPinIgnoresOwnSession(t *testing.T) {
	rt, sent := scriptedRT("%exit\n")
	handedOff := false
	p := &sessionPin{id: "$0", handOff: func(string) { handedOff = true }}

	p.apply(controlmode.ParseLine("%session-changed $0 A"), newRegistry(), NewRouter(), rt)

	if sent.Len() != 0 {
		t.Errorf("sent %q, want nothing", sent.String())
	}
	if handedOff {
		t.Error("handed off on our own session")
	}
}

// A pin that could not resolve its session id must stay out of the way rather
// than switch the client to an empty target.
func TestSessionPinDisabledWithoutID(t *testing.T) {
	rt, sent := scriptedRT("%exit\n")
	p := &sessionPin{}

	p.apply(controlmode.ParseLine("%session-changed $5 other"), newRegistry(), NewRouter(), rt)

	if sent.Len() != 0 {
		t.Errorf("sent %q, want nothing", sent.String())
	}
}

// The whole fix in one pass: a foreign %session-changed switches the client
// back, repaints every mirrored pane (output during the excursion is dropped by
// the server, not buffered), and hands the session we were switched to off to a
// mirror of its own.
func TestSessionPinSwitchesBackReseedsAndHandsOff(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	router := NewRouter()
	router.Register("%1", newOutputSink(local, nil))
	reg := newRegistry()
	mw := reg.add("@1", "@101")
	mw.remotePanes = []string{"%1"}

	// Three replies: switch-client, then PaneSeed's cursor + capture.
	script := strings.Join([]string{
		"%begin 1 1 1",
		"%end 1 1 1",
		"%begin 1 2 1",
		"0 0 0 0",
		"%end 1 2 1",
		"%begin 1 3 1",
		"FRESH-CAPTURE",
		"%end 1 3 1",
	}, "\n") + "\n"
	rt, sent := scriptedRT(script)

	gotHandOff := make(chan string, 1)
	p := &sessionPin{id: "$0", handOff: func(s string) { gotHandOff <- s }}

	go p.apply(controlmode.ParseLine("%session-changed $5 other proj"), reg, router, rt)

	f, err := wire.ReadFrame(peer)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Type != wire.FrameSeed {
		t.Fatalf("frame type = %v, want FrameSeed", f.Type)
	}
	if !bytes.Contains(f.Payload, []byte("FRESH-CAPTURE")) {
		t.Errorf("seed payload = %q, want the fresh capture", f.Payload)
	}
	if s := <-gotHandOff; s != "other proj" {
		t.Errorf("handed off %q, want %q", s, "other proj")
	}

	// No -c: over this stream, "current client" is the control client itself.
	if first := strings.SplitN(sent.String(), "\n", 2)[0]; first != "switch-client -t '$0'" {
		t.Errorf("first command = %q, want the switch back", first)
	}
}

// newLayoutsFlagAck models the reply to readIdentity's leading
// `refresh-client -f new-layouts`. It is claimed and, on success, ignored, so
// its content never matters — only that a reply block sits there for the
// flag command's ordinal to land on, ahead of the identity reply that
// follows.
const newLayoutsFlagAck = "%begin 1 0 1\n%end 1 0 1\n"

func TestNewSessionPinRejectsNonID(t *testing.T) {
	// A reply that is not a session id (an error text, a truncated read) must
	// not be interpolated into switch-client.
	rt, _ := scriptedRT(newLayoutsFlagAck + "%begin 1 1 1\nnot-an-id\n%end 1 1 1\n")
	if p := newSessionPin(Config{RemoteSession: "A"}, rt); p.id != "" {
		t.Errorf("id = %q, want pinning disabled", p.id)
	}
}

func TestNewSessionPinReadsID(t *testing.T) {
	rt, sent := scriptedRT(newLayoutsFlagAck + "%begin 1 1 1\n2151|1788283304|$3\n%end 1 1 1\n")
	p := newSessionPin(Config{RemoteSession: "my proj"}, rt)
	if p.id != "$3" {
		t.Errorf("id = %q, want $3", p.id)
	}
	if !p.identityKnown {
		t.Error("identityKnown = false, want true on a well-formed reply")
	}
	if p.identity != (remoteIdentity{pid: 2151, sessionID: "$3", startTime: 1788283304, hasStartTime: true}) {
		t.Errorf("identity = %+v, want the parsed tuple", p.identity)
	}
	if !strings.Contains(sent.String(), "-t 'my proj'") {
		t.Errorf("sent %q, want the session name quoted as one token", sent.String())
	}
	// The flag must lead the batch: a reattach or replacement that sent it
	// after the identity read would still open the connection unbound, but a
	// remote that answered the identity command before seeing the flag could
	// still race a %layout-change against it.
	if flagAt, idAt := strings.Index(sent.String(), "refresh-client -f new-layouts"), strings.Index(sent.String(), "#{pid}"); flagAt < 0 || idAt < 0 || flagAt > idAt {
		t.Errorf("sent %q, want the new-layouts flag before the identity read", sent.String())
	}
}

// TestNewSessionPinToleratesAnOldRemoteRejectingTheFlag: an %error on the
// flag's own reply means an older remote that cannot take it — still reports
// v1, not a reason to fail the identity read that follows.
func TestNewSessionPinToleratesAnOldRemoteRejectingTheFlag(t *testing.T) {
	// Real %error framing: the error text is the block's body, and %error
	// itself is the terminator — there is no trailing %end (readBlock returns
	// as soon as it sees %error).
	script := "%begin 1 0 1\nunknown flag: new-layouts\n%error 1 0 1\n" +
		"%begin 1 1 1\n2151|1788283304|$3\n%end 1 1 1\n"
	var p *sessionPin
	logs := captureRouterStderr(t, func() {
		rt, _ := scriptedRT(script)
		p = newSessionPin(Config{RemoteSession: "A"}, rt)
	})
	if p.id != "$3" || !p.identityKnown {
		t.Errorf("id = %q identityKnown = %v, want $3 / true — an %%error on the flag reply must not fail the identity read", p.id, p.identityKnown)
	}
	if !strings.Contains(logs, "unknown flag: new-layouts") || !strings.Contains(logs, "floats will not be mirrored") {
		t.Errorf("logs = %q, want the flag's error text and the floats warning", logs)
	}
}

// TestNewSessionPinAttach1NeverTearsDown: an unusable identity at the first
// attach disables the feature, it never fails startup — there is nothing yet
// to compare a later attach against.
func TestNewSessionPinAttach1NeverTearsDown(t *testing.T) {
	for name, script := range map[string]string{
		"malformed":  newLayoutsFlagAck + "%begin 1 1 1\nnot-an-id\n%end 1 1 1\n",
		"empty body": newLayoutsFlagAck + "%begin 1 1 1\n\n%end 1 1 1\n",
		"error":      newLayoutsFlagAck + "%begin 1 1 1\n%error 1 1 1\nboom\n%end 1 1 1\n",
		"eof":        "%exit\n",
	} {
		t.Run(name, func(t *testing.T) {
			rt, _ := scriptedRT(script)
			p := newSessionPin(Config{RemoteSession: "A"}, rt)
			if p.identityKnown {
				t.Error("identityKnown = true, want false")
			}
			if p.id != "" {
				t.Errorf("id = %q, want pinning disabled", p.id)
			}
		})
	}
}

func mustIdentity(t *testing.T, session, body string) remoteIdentity {
	t.Helper()
	id, err := parseIdentity(session, body)
	if err != nil {
		t.Fatalf("parseIdentity(%q): %v", body, err)
	}
	return id
}

// TestRemoteIdentityMatches covers the comparison rule: pid and sessionID
// always compare, startTime only when both sides carry one.
func TestRemoteIdentityMatches(t *testing.T) {
	tests := []struct {
		name string
		a, b remoteIdentity
		want bool
	}{
		{"identical", mustIdentity(t, "A", "2151|1788283304|$1"), mustIdentity(t, "A", "2151|1788283304|$1"), true},
		{"pid mismatch", mustIdentity(t, "A", "2151|1788283304|$1"), mustIdentity(t, "A", "9999|1788283304|$1"), false},
		{"session_id mismatch", mustIdentity(t, "A", "2151|1788283304|$1"), mustIdentity(t, "A", "2151|1788283304|$2"), false},
		{"start_time mismatch, both present", mustIdentity(t, "A", "2151|1788283304|$1"), mustIdentity(t, "A", "2151|1|$1"), false},
		{"start_time absent on one side", mustIdentity(t, "A", "2151|1788283304|$1"), mustIdentity(t, "A", "2151||$1"), true},
		{"start_time absent on both sides", mustIdentity(t, "A", "2151||$1"), mustIdentity(t, "A", "2151||$1"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.matches(tt.b); got != tt.want {
				t.Errorf("matches = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadIdentityDistinguishesRetryFromTeardown is the load-bearing test:
// EOF mid-read (one()'s ok==false) must report Retry()==true, while a reply
// that arrived and is wrong must report Retry()==false, so a caller cannot
// accidentally route one into the other's handling.
func TestReadIdentityDistinguishesRetryFromTeardown(t *testing.T) {
	tests := []struct {
		name      string
		script    string
		wantRetry bool
	}{
		{"eof mid-read", "%exit\n", true},
		{"eof after the flag reply", newLayoutsFlagAck, true},
		{"error reply", newLayoutsFlagAck + "%begin 1 1 1\n%error 1 1 1\nboom\n%end 1 1 1\n", false},
		{"malformed body", newLayoutsFlagAck + "%begin 1 1 1\nnot-an-id\n%end 1 1 1\n", false},
		{"empty body", newLayoutsFlagAck + "%begin 1 1 1\n\n%end 1 1 1\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _ := scriptedRT(tt.script)
			_, err := readIdentity(rt, "A")
			if err == nil {
				t.Fatal("err = nil, want a failure")
			}
			ie, ok := err.(*identityReadErr)
			if !ok {
				t.Fatalf("err = %T, want *identityReadErr", err)
			}
			if ie.Retry() != tt.wantRetry {
				t.Errorf("Retry() = %v, want %v", ie.Retry(), tt.wantRetry)
			}
		})
	}
}

func TestReadIdentityWellFormed(t *testing.T) {
	rt, sent := scriptedRT(newLayoutsFlagAck + "%begin 1 1 1\n2151|1788283304|$1\n%end 1 1 1\n")
	id, err := readIdentity(rt, "my proj")
	if err != nil {
		t.Fatalf("readIdentity: %v", err)
	}
	want := remoteIdentity{pid: 2151, sessionID: "$1", startTime: 1788283304, hasStartTime: true}
	if id != want {
		t.Errorf("id = %+v, want %+v", id, want)
	}
	if !strings.Contains(sent.String(), "-t 'my proj'") {
		t.Errorf("sent %q, want the session name quoted as one token", sent.String())
	}
	if !strings.Contains(sent.String(), "#{pid}|#{start_time}|#{session_id}") {
		t.Errorf("sent %q, want the pipe-delimited identity format", sent.String())
	}
	// The flag leads every attach: a reattach or replacement that sent it
	// after the identity read would still race a %layout-change over the old
	// flags.
	flagAt, idAt := strings.Index(sent.String(), "refresh-client -f new-layouts"), strings.Index(sent.String(), "display-message")
	if flagAt < 0 || idAt < 0 || flagAt > idAt {
		t.Errorf("sent %q, want the new-layouts flag before the identity read", sent.String())
	}
}

// TestSessionPinReseedRoutesEachPaneItsOwnCapture pins the parallel ids/sinks
// mapping of the cross-window reseed batch. A seed handed to the wrong sink
// paints one window's screen into another window's renderer and nothing errors
// — both are valid seeds — so only distinct captures catch it.
func TestSessionPinReseedRoutesEachPaneItsOwnCapture(t *testing.T) {
	peers := map[string]net.Conn{}
	router := NewRouter()
	reg := newRegistry()
	for _, w := range []struct{ remoteWin, localWin, pane string }{
		{"@1", "@101", "%1"},
		{"@2", "@102", "%2"},
	} {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()
		peers[w.pane] = peer
		router.Register(w.pane, newOutputSink(local, nil))
		reg.add(w.remoteWin, w.localWin).remotePanes = []string{w.pane}
	}

	// Two cursor+capture pairs, in issue order.
	rt, sent := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0", "%end 1 1 1",
		"%begin 1 2 1", "FRESH-A", "%end 1 2 1",
		"%begin 1 3 1", "0 0 0 0", "%end 1 3 1",
		"%begin 1 4 1", "FRESH-B", "%end 1 4 1",
	}, "\n") + "\n")

	(&sessionPin{id: "$0"}).reseed(reg, router, rt)

	// reg.all() walks a map, so the issue order is whatever it gave us; the
	// commands on the wire are the record of it.
	order := capturedPanes(sent.String())
	want := map[string]string{order[0]: "FRESH-A", order[1]: "FRESH-B"}
	for pane, peer := range peers {
		f, err := wire.ReadFrame(peer)
		if err != nil {
			t.Fatalf("read frame for %s: %v", pane, err)
		}
		if !bytes.Contains(f.Payload, []byte(want[pane])) {
			t.Errorf("%s got seed %q, want the one carrying %s", pane, f.Payload, want[pane])
		}
	}
}

// capturedPanes lists the pane ids of the capture-pane commands on the wire, in
// the order they were issued.
func capturedPanes(sent string) []string {
	var ids []string
	for _, line := range strings.Split(sent, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "capture-pane" {
			ids = append(ids, fields[len(fields)-1])
		}
	}
	return ids
}
