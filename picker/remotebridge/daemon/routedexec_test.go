package daemon

import (
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestReadReplyRoutingReturnsParkedReply pins the slot's read side: a reply
// routeWhile already parked answers readReplyRouting(want) directly, with no
// read off the stream at all — the reader here holds only an %output line
// that must stay untouched.
func TestReadReplyRoutingReturnsParkedReply(t *testing.T) {
	st := testStream()
	seqs, ok := st.stampAll("a")
	if !ok {
		t.Fatal("stampAll failed")
	}
	l := controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte("parked")}
	seq := claimSeq(l, st)
	if seq != seqs[0] {
		t.Fatalf("claimSeq = %d, want %d", seq, seqs[0])
	}
	st.park(seq, l)

	router := NewRouter()
	var sink capBuf
	router.Register("%1", &sink)

	got, ok := readReplyRouting(rawTestReader("%output %1 after\n"), router, &asyncQueue{}, st, seqs[0])
	if !ok || string(got.Data) != "parked" {
		t.Fatalf("readReplyRouting = %+v, ok=%v; want the parked reply", got, ok)
	}
	if sink.String() != "" {
		t.Errorf("sink = %q, want empty: nothing past the parked reply was read", sink.String())
	}
}

// TestReadReplyRoutingDropsStaleParkedReply pins the slot's stale-drop: a
// parked reply that isn't the one asked for is dropped, exactly as the old
// walk would have dropped it in passing, and reading resumes on the stream.
func TestReadReplyRoutingDropsStaleParkedReply(t *testing.T) {
	st := testStream()
	seqs, ok := st.stampAll("a", "b")
	if !ok {
		t.Fatal("stampAll failed")
	}
	l := controlmode.Line{Kind: controlmode.End, Flags: controlmode.ClientCommandFlag, Data: []byte("a-reply")}
	seq := claimSeq(l, st)
	if seq != seqs[0] {
		t.Fatalf("claimSeq = %d, want %d", seq, seqs[0])
	}
	st.park(seq, l)

	// A raw reader: barrier 2's own reply (og-fanout-1), then b's reply.
	s := strings.Join([]string{
		"%begin 1 2 1",
		"og-fanout-1",
		"%end 1 2 1",
		"%begin 1 3 1",
		"B",
		"%end 1 3 1",
	}, "\n") + "\n"

	got, ok := readReplyRouting(rawTestReader(s), NewRouter(), &asyncQueue{}, st, seqs[1])
	if !ok || string(got.Data) != "B" {
		t.Fatalf("readReplyRouting = %+v, ok=%v; want b's reply %q", got, ok, "B")
	}
	if p := st.parkedSeq(); p != 0 {
		t.Errorf("parkedSeq = %d, want 0: the stale slot was taken and dropped", p)
	}
}
