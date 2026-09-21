package daemon

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// testRoundTrip wires PaneSeed the way Run does — one stream numbering the
// commands, one reader answering them — with the wire discarded and the command
// lines recorded for assertions.
func testRoundTrip(stream string, sent *[]string) roundTrip {
	// withBarriers: the wire carries a barrier block per command (#723).
	rt := newRoundTrip(controlmode.NewReader(strings.NewReader(withBarriers(stream))),
		NewRouter(), &asyncQueue{}, newStream(io.Discard))
	return func(cmds ...string) replies {
		*sent = append(*sent, cmds...)
		return rt(cmds...)
	}
}

func TestPaneSeed(t *testing.T) {
	// Scripted server replies: display-message (cursor/mode) then capture-pane.
	// Each command emits a %begin/…/%end block in issue order.
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // display-message: cx cy alt appck
		"%begin 2 2 1", "line-one", "line-two", "%end 2 2 1", // capture-pane
	}, "\n") + "\n"

	var sent []string
	got, err := PaneSeed(testRoundTrip(stream, &sent), "%3")
	if err != nil {
		t.Fatalf("PaneSeed: %v", err)
	}
	// Commands must target %3.
	if len(sent) != 2 || !strings.Contains(sent[0], "-t %3") || !strings.Contains(sent[1], "-t %3") {
		t.Fatalf("sent = %v, want display-message + capture-pane targeting %%3", sent)
	}
	// Seed must contain the captured content and a cursor CUP for (5,2) => \x1b[3;6H.
	if !bytes.Contains(got, []byte("line-one")) || !bytes.Contains(got, []byte("\x1b[3;6H")) {
		t.Errorf("seed missing content or cursor CUP: %q", got)
	}
}

func TestPaneSeedErrorReply(t *testing.T) {
	// capture-pane's reply is a %error block (e.g. the pane closed between
	// list-panes and this capture-pane) — PaneSeed must reject it.
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // display-message: cx cy alt appck
		"%begin 2 2 1", "%error 2 2 1", // capture-pane: error, no body
	}, "\n") + "\n"

	var sent []string
	got, err := PaneSeed(testRoundTrip(stream, &sent), "%3")
	if err == nil {
		t.Fatalf("PaneSeed: want error on %%error reply, got seed %q", got)
	}
}

func TestPaneSeedEmptyCaptureIsValid(t *testing.T) {
	// capture-pane's reply is a normal %end block with an EMPTY body — a
	// genuinely blank pane, which must NOT be treated as an error (fatal
	// for a sole pane if it were).
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // display-message: cx cy alt appck
		"%begin 2 2 1", "%end 2 2 1", // capture-pane: success, empty body
	}, "\n") + "\n"

	var sent []string
	got, err := PaneSeed(testRoundTrip(stream, &sent), "%3")
	if err != nil {
		t.Fatalf("PaneSeed: want nil error for a blank pane, got %v", err)
	}
	if got == nil {
		t.Errorf("PaneSeed: want a non-nil seed for a blank pane, got nil")
	}
}

// TestPaneSeedSurvivesHookBlocks: a remote hook's own %begin..%end (flagged 0)
// landing between our two commands must not be mistaken for either reply.
func TestPaneSeedSurvivesHookBlocks(t *testing.T) {
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // display-message
		"%begin 1 2 0", "%end 1 2 0", // a hook's command, not ours
		"%begin 2 3 1", "line-one", "%end 2 3 1", // capture-pane
	}, "\n") + "\n"

	var sent []string
	got, err := PaneSeed(testRoundTrip(stream, &sent), "%3")
	if err != nil {
		t.Fatalf("PaneSeed: %v", err)
	}
	if !bytes.Contains(got, []byte("line-one")) {
		t.Errorf("seed %q lost the capture to a hook block", got)
	}
}

// gatingReader serves the scripted reply stream only once both command lines
// have been observed on the recording writer sharing the same *stream, and
// returns EOF otherwise. EOF, not a block: PaneSeed's write and this Read run
// on the same goroutine, so withholding bytes would hang the test rather than
// fail it. On a regression to send-wait-send-wait, the first reply read lands
// before the second command is written, this gate is still closed, Read
// returns EOF, bufio.Scanner latches that permanently, and every later read —
// including the capture-pane reply that same command is waiting on — fails
// too, which is what makes PaneSeed return an error instead of merely running
// slower.
type gatingReader struct {
	sent   *bytes.Buffer
	need   []string
	script *strings.Reader
}

func (g *gatingReader) Read(p []byte) (int, error) {
	s := g.sent.String()
	for _, n := range g.need {
		if !strings.Contains(s, n) {
			return 0, io.EOF
		}
	}
	return g.script.Read(p)
}

// TestPaneSeedWritesBothCommandsBeforeReadingEitherReply is AC1: PaneSeed must
// write display-message AND capture-pane before reading either reply. See
// gatingReader for how the regression it catches actually fails.
func TestPaneSeedWritesBothCommandsBeforeReadingEitherReply(t *testing.T) {
	replyStream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // display-message
		"%begin 2 2 1", "line-one", "%end 2 2 1", // capture-pane
	}, "\n") + "\n"

	sent := &bytes.Buffer{}
	g := &gatingReader{sent: sent, need: []string{"display-message", "capture-pane"}, script: strings.NewReader(withBarriers(replyStream))}
	rt := newRoundTrip(controlmode.NewReader(g), NewRouter(), &asyncQueue{}, newStream(sent))

	got, err := PaneSeed(rt, "%3")
	if err != nil {
		t.Fatalf("PaneSeed: %v", err)
	}
	if !bytes.Contains(got, []byte("line-one")) {
		t.Errorf("seed %q missing captured content", got)
	}
}

// TestPaneSeedsIndexAlignmentAndPerPaneError covers AC2: results land at the
// right index, and a %error capture for one pane doesn't cost its siblings
// their seeds.
func TestPaneSeedsIndexAlignmentAndPerPaneError(t *testing.T) {
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // pane A: cursor
		"%begin 2 2 1", "line-A", "%end 2 2 1", // pane A: capture
		"%begin 3 3 1", "1 1 0 0", "%end 3 3 1", // pane B: cursor
		"%begin 4 4 1", "%error 4 4 1", // pane B: capture, errors
		"%begin 5 5 1", "0 0 0 0", "%end 5 5 1", // pane C: cursor
		"%begin 6 6 1", "line-C", "%end 6 6 1", // pane C: capture
	}, "\n") + "\n"

	var sent []string
	rt := testRoundTrip(stream, &sent)

	type result struct {
		seed []byte
		err  error
	}
	got := make([]result, 3)
	calls := 0
	PaneSeeds(rt, []string{"%1", "%2", "%3"}, func(i int, seed []byte, err error) {
		calls++
		got[i] = result{seed, err}
	})

	if calls != 3 {
		t.Fatalf("onSeed called %d times, want 3", calls)
	}
	if got[0].err != nil || !bytes.Contains(got[0].seed, []byte("line-A")) {
		t.Errorf("pane 0 (index-aligned to %%1) = %+v, want a seed containing line-A", got[0])
	}
	if got[1].err == nil {
		t.Errorf("pane 1 (index-aligned to %%2) = %+v, want an error from its %%error capture reply", got[1])
	}
	if got[2].err != nil || !bytes.Contains(got[2].seed, []byte("line-C")) {
		t.Errorf("pane 2 (index-aligned to %%3) = %+v, want a seed containing line-C", got[2])
	}
}

// TestPaneSeedsStreamLossErrorsRemainingPanes: once the stream runs out
// mid-batch, every pane still owed a reply gets onSeed called with an error —
// a caller's bookkeeping (e.g. which conns to close) is never left half done.
func TestPaneSeedsStreamLossErrorsRemainingPanes(t *testing.T) {
	// Only pane A's two replies are on the wire; the stream ends there, so
	// pane B's commands (issued the same as A's) are never answered.
	stream := strings.Join([]string{
		"%begin 1 1 1", "5 2 0 0", "%end 1 1 1", // pane A: cursor
		"%begin 2 2 1", "line-A", "%end 2 2 1", // pane A: capture
	}, "\n") + "\n"

	var sent []string
	rt := testRoundTrip(stream, &sent)

	type result struct {
		seed []byte
		err  error
	}
	got := make([]result, 2)
	calls := 0
	PaneSeeds(rt, []string{"%1", "%2"}, func(i int, seed []byte, err error) {
		calls++
		got[i] = result{seed, err}
	})

	if calls != 2 {
		t.Fatalf("onSeed called %d times, want 2 (including the pane starved by stream loss)", calls)
	}
	if got[0].err != nil {
		t.Errorf("pane 0: got err %v, want nil", got[0].err)
	}
	if got[1].err == nil {
		t.Errorf("pane 1: want a stream-loss error, got seed %q", got[1].seed)
	}
}

// TestPaneSeedsNilPaneIDsIsANoop: no commands issued, onSeed never called.
func TestPaneSeedsNilPaneIDsIsANoop(t *testing.T) {
	var sent []string
	called := false
	PaneSeeds(testRoundTrip("", &sent), nil, func(int, []byte, error) { called = true })
	if called {
		t.Error("onSeed called for nil paneIDs")
	}
	if len(sent) != 0 {
		t.Errorf("sent %v, want no commands issued", sent)
	}
}
