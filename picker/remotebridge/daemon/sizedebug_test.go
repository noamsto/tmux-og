package daemon

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// armSizeDebug swaps the trace sink for buf and arms the switch, restoring both
// in t.Cleanup. These tests mutate package globals, so they never run in
// parallel.
func armSizeDebug(t *testing.T, on bool) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	restoreW := setSizeDebugWriter(buf)
	prev := sizeDebug.Load()
	sizeDebug.Store(on)
	t.Cleanup(func() {
		sizeDebug.Store(prev)
		restoreW()
	})
	return buf
}

// clientReply builds a client-flagged reply block line the way claimSeq sees it.
func clientReply(kind controlmode.Kind, body string) controlmode.Line {
	return controlmode.Line{Kind: kind, Flags: controlmode.ClientCommandFlag, Data: []byte(body)}
}

// Off: stampAll leaves the traced map nil and nothing reaches the sink.
func TestSizeDebugOffRecordsAndLogsNothing(t *testing.T) {
	buf := armSizeDebug(t, false)
	st := newStream(io.Discard)

	if _, ok := st.stampAll("refresh-client -C 150x40"); !ok {
		t.Fatal("stampAll failed")
	}
	if st.traced != nil {
		t.Fatalf("traced = %v, want nil when off", st.traced)
	}
	claimSeq(clientReply(controlmode.End, "ok"), st)
	if buf.Len() != 0 {
		t.Fatalf("trace output = %q, want none when off", buf.String())
	}
}

// On: a traced send and its End reply log both lines and empty the map.
func TestSizeDebugTracesSendAndEndReply(t *testing.T) {
	buf := armSizeDebug(t, true)
	st := newStream(io.Discard)

	seqs, ok := st.stampAll("refresh-client -C 150x40")
	if !ok || len(seqs) != 1 || seqs[0] != 1 {
		t.Fatalf("stampAll seqs = %v, %v; want [1]", seqs, ok)
	}
	if got := st.traced[1]; got != "refresh-client -C 150x40" {
		t.Fatalf("traced[1] = %q, want the command", got)
	}
	claimSeq(clientReply(controlmode.End, "  done \n"), st)
	if len(st.traced) != 0 {
		t.Fatalf("traced = %v, want empty after the reply", st.traced)
	}
	out := buf.String()
	if !strings.Contains(out, "daemon: size: sent #1 refresh-client -C 150x40\n") {
		t.Fatalf("missing sent line in %q", out)
	}
	if !strings.Contains(out, "daemon: size: reply #1 to refresh-client -C 150x40: %end done\n") {
		t.Fatalf("missing trimmed End reply in %q", out)
	}
}

// On: an Error reply logs %error.
func TestSizeDebugTracesErrorReply(t *testing.T) {
	buf := armSizeDebug(t, true)
	st := newStream(io.Discard)

	if _, ok := st.stampAll("refresh-client -C 150x40"); !ok {
		t.Fatal("stampAll failed")
	}
	claimSeq(clientReply(controlmode.Error, "no such client"), st)
	if got := buf.String(); !strings.Contains(got, "reply #1 to refresh-client -C 150x40: %error no such client") {
		t.Fatalf("missing %%error reply in %q", got)
	}
}

// On: an untraced command is never recorded and logs nothing.
func TestSizeDebugIgnoresUntracedCommands(t *testing.T) {
	buf := armSizeDebug(t, true)
	st := newStream(io.Discard)

	if _, ok := st.stampAll("display-message -p x"); !ok {
		t.Fatal("stampAll failed")
	}
	if len(st.traced) != 0 {
		t.Fatalf("traced = %v, want empty for an untraced command", st.traced)
	}
	if buf.Len() != 0 {
		t.Fatalf("trace output = %q, want none for an untraced command", buf.String())
	}
}

// On: the fanout barrier's ordinal (the command's own s.sent + 1) is never
// recorded, and a later command keeps its real ordinal.
func TestSizeDebugFanoutBarrierDoesNotShiftOrdinal(t *testing.T) {
	armSizeDebug(t, true)
	st := newStream(io.Discard)

	first, ok := st.stampAll("refresh-client -C 10x10")
	if !ok || len(first) != 1 || first[0] != 1 {
		t.Fatalf("first stampAll seqs = %v, %v; want [1]", first, ok)
	}
	second, ok := st.stampAll("refresh-client -C 20x20")
	if !ok || len(second) != 1 || second[0] != 3 {
		t.Fatalf("second stampAll seqs = %v, %v; want [3] (barrier took 2)", second, ok)
	}
	if _, barrier := st.traced[2]; barrier {
		t.Fatalf("traced recorded the barrier ordinal 2: %v", st.traced)
	}
	if st.traced[1] != "refresh-client -C 10x10" || st.traced[3] != "refresh-client -C 20x20" {
		t.Fatalf("traced = %v, want ordinals 1 and 3", st.traced)
	}
	if len(st.traced) != 2 {
		t.Fatalf("traced = %v, want exactly two entries", st.traced)
	}
}

// On: a reply body carrying interior newlines, CRs and an ESC sequence is
// stripped to printable text, so it logs as exactly one line with no control
// bytes instead of forging a log line or emitting a terminal sequence.
func TestSizeDebugSanitizesReplyBody(t *testing.T) {
	buf := armSizeDebug(t, true)
	st := newStream(io.Discard)

	if _, ok := st.stampAll("refresh-client -C 150x40"); !ok {
		t.Fatal("stampAll failed")
	}
	claimSeq(clientReply(controlmode.End, "a\nb\rc\x1b[31md"), st)

	out := buf.String()
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("trace lines = %d, want 2 (sent + reply): %q", n, out)
	}
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("trace output has control bytes: %q", out)
	}
	if !strings.Contains(out, "reply #1 to refresh-client -C 150x40: %end abc[31md\n") {
		t.Fatalf("missing sanitized reply in %q", out)
	}
}

// On: a reply body over 200 bytes is capped at 200 bytes in the trace.
func TestSizeDebugCapsOverlongReplyBody(t *testing.T) {
	buf := armSizeDebug(t, true)
	st := newStream(io.Discard)

	if _, ok := st.stampAll("refresh-client -C 150x40"); !ok {
		t.Fatal("stampAll failed")
	}
	claimSeq(clientReply(controlmode.End, strings.Repeat("x", 300)), st)

	out := buf.String()
	if !strings.Contains(out, strings.Repeat("x", 200)+"\n") {
		t.Fatalf("capped body missing in %q", out)
	}
	if strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("body not capped at 200 bytes: %q", out)
	}
}
