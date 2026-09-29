package daemon

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

// TestKeyNegStrippedEndToEndThroughASink: a modifyOtherKeys negotiation
// sequence must never reach a mirror pane's pty, or local tmux re-encodes
// future keystrokes for it — including Ctrl+R — as if the renderer itself
// had requested it (#338).
func TestKeyNegStrippedEndToEndThroughASink(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, nil) // nil gfx: the strip must not depend on graphics being wired in
	defer s.Close()

	_, _ = s.Write([]byte("hello \x1b[>4;2mworld"))

	got := readAllFrames(t, local, 500*time.Millisecond)
	if strings.Contains(got, "\x1b[>4;2m") {
		t.Fatalf("negotiation sequence leaked through: %q", got)
	}
	if want := "hello world"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestKeyNegSplitAcrossWritesEndToEnd exercises the case a live network read
// is most likely to hit: the negotiation sequence straddling two separate
// pane-output frames.
func TestKeyNegSplitAcrossWritesEndToEnd(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, nil)
	defer s.Close()

	_, _ = s.Write([]byte("hello \x1b[>4;"))
	_, _ = s.Write([]byte("2mworld"))

	got := readAllFrames(t, local, 500*time.Millisecond)
	if want := "hello world"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestQueriesStrippedEndToEndThroughASink: a terminal query must never reach a
// mirror pane's pty, or the local tmux answers it a second time and that reply
// lands on the remote occupant as unsolicited input, doubling every answer
// (#544).
func TestQueriesStrippedEndToEndThroughASink(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, nil)
	defer s.Close()

	_, _ = s.Write([]byte("a\x1b[6nb\x1b[?2004$pc\x1b[16td"))

	got := readAllFrames(t, local, 500*time.Millisecond)
	if want := "abcd"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPassthroughWrappedQuerySurvivesTheSink: the remote tmux does not answer a
// query wrapped for passthrough, so the local answer is the only one it gets —
// stripping it would be a regression, not a fix.
func TestPassthroughWrappedQuerySurvivesTheSink(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, nil)
	defer s.Close()

	in := "\x1bPtmux;\x1b\x1b[6n\x1b\\"
	_, _ = s.Write([]byte(in))

	got := readAllFrames(t, local, 500*time.Millisecond)
	if got != in {
		t.Fatalf("got %q, want unchanged %q", got, in)
	}
}

// TestKeyNegAndGraphicsFlushOrderOnClose: kn only ever holds back the
// newest unprocessed tail of the stream, so on close its leftover must be
// threaded through gfx before gfx's own held bytes are flushed, preserving
// the original byte order of the stream's tail.
func TestKeyNegAndGraphicsFlushOrderOnClose(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, graphics.New(nil, nil))

	// An incomplete kitty APC (held by gfx), then — in a separate write, so it
	// lands in a later pump iteration — more bytes behind it. kn forwards the
	// trailing "\x1b[>4;" rather than holding it: the unterminated APC is an
	// open region, and a region's bytes go through verbatim. gfx is what holds
	// the whole tail here, so this pins the close path's ordering.
	_, _ = s.Write([]byte("AAA\x1b_Ga=t,f=100;"))
	time.Sleep(20 * time.Millisecond)
	_, _ = s.Write([]byte("BBB\x1b[>4;"))
	time.Sleep(20 * time.Millisecond)

	s.Close()

	got := readAllFrames(t, local, 500*time.Millisecond)
	if want := "AAA\x1b_Ga=t,f=100;BBB\x1b[>4;"; got != want {
		t.Fatalf("got %q, want %q (original byte order preserved)", got, want)
	}
}
