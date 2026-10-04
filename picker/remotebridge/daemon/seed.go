package daemon

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/render"
)

// replies yields a batch's reply blocks in issue order, one per call. ok is
// false once the batch is drained or the control stream is gone, and stays
// false thereafter.
type replies = func() (controlmode.Line, bool)

// roundTrip writes every command in cmds BEFORE any reply is read, then hands
// back an iterator over their reply blocks — always a closure over
// stream.stampAll + readReplyRouting, so it neither drops another pane's live
// %output while it waits nor mistakes some other command's reply for its own.
// It is the only way a mirror path asks the remote a question; everything else
// is fire-and-forget.
//
// An iterator rather than a slice of replies, because readReplyRouting delivers
// %output into registered sinks as it walks past reply blocks: a result derived
// from reply i but acted on only after reply i+1 was taken off the stream would
// be applied over output the renderer has already seen. Yielding incrementally
// makes "act on reply i before reading i+1" the shape of the code.
//
// The contract:
//   - next() returns reply i for command i. Call it in issue order, at most
//     len(cmds) times.
//   - Nothing done between two next() calls may read the control stream. A
//     nested round-trip stamps a later ordinal, and readReplyRouting would
//     consume and discard the in-flight batch's remaining blocks hunting for
//     it — the next next() would then wait for an ordinal already gone past,
//     hanging the main loop until EOF. Local tmux work, sink.enqueue and
//     stderr between calls, nothing else.
type roundTrip = func(cmds ...string) replies

// one is the degenerate single-command round-trip.
func one(rt roundTrip, cmd string) (controlmode.Line, bool) { return rt(cmd)() }

func cursorCmd(paneID string) string {
	return fmt.Sprintf("display-message -p -t %s -F '#{cursor_x} #{cursor_y} #{alternate_on} #{keypad_cursor_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_all_flag} #{mouse_sgr_flag} #{mouse_utf8_flag} #{pane_private_modes}'", paneID)
}

func captureCmd(paneID string) string {
	return fmt.Sprintf("capture-pane -e -p -t %s", paneID)
}

// PaneSeed issues display-message/capture-pane for paneID over an established
// control stream and returns the render.Seed bytes. It is the N=1 case of
// PaneSeeds, so the reply-pairing logic below exists once.
func PaneSeed(rt roundTrip, paneID string) ([]byte, error) {
	var seed []byte
	var seedErr error
	PaneSeeds(rt, []string{paneID}, func(_ int, s []byte, err error) {
		seed, seedErr = s, err
	})
	return seed, seedErr
}

// PaneSeeds issues display-message+capture-pane for every id in paneIDs — all
// 2N commands written before any reply is read — then calls onSeed once per
// pane, in order, as soon as that pane's two replies are parsed and BEFORE any
// later pane's reply is taken off the stream.
//
// A caller that enqueues onSeed's result as a FrameSeed depends on that
// ordering: it is what guarantees nothing was routed into the pane's sink
// between its capture reply and its seed (see roundTrip above). onSeed must
// not read the control stream.
//
// Commands are interleaved per pane — dm(A), cap(A), dm(B), cap(B), … — never
// grouped by kind: a cursor read 2N commands ahead of the capture it decorates
// would describe a screen the capture no longer shows, placing the cursor
// where the pane used to have it.
//
// A per-pane error never costs its siblings their seeds; a stream lost
// mid-batch still reports one error per pane still owed a reply.
func PaneSeeds(rt roundTrip, paneIDs []string, onSeed func(i int, seed []byte, err error)) {
	if len(paneIDs) == 0 {
		return
	}
	cmds := make([]string, 0, 2*len(paneIDs))
	for _, id := range paneIDs {
		cmds = append(cmds, cursorCmd(id), captureCmd(id))
	}
	next := rt(cmds...)
	for i, id := range paneIDs {
		curLine, curOK := next()
		capLine, capOK := next()
		cx, cy, alt, appck, mouse, modes := parseCursor(curLine, curOK)
		captured, isErr := parseCapture(capLine, capOK)
		// isErr, not len(captured)==0: a genuinely blank pane is a valid
		// successful capture with empty Data, so an emptiness check alone
		// would reject a legitimate blank seed. A pane that closed between
		// list-panes and this capture-pane instead gets a %error reply
		// (non-empty body: the error text), and a stream gone mid-batch never
		// yields a reply at all — isErr covers both, keyed off Kind and ok,
		// never body length.
		if isErr {
			if capLine.Err != nil {
				onSeed(i, nil, fmt.Errorf("capture-pane failed for %s: %w", id, capLine.Err))
				continue
			}
			onSeed(i, nil, fmt.Errorf("capture-pane failed for %s", id))
			continue
		}
		onSeed(i, render.Seed(replaceLF(captured), cx, cy, alt, appck, mouse, modes), nil)
	}
}

func replaceLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

// parseCursor parses an already-read display-message reply into
// "cursor_x cursor_y alternate_on keypad_cursor_flag mouse_standard_flag
// mouse_button_flag mouse_all_flag mouse_sgr_flag mouse_utf8_flag
// pane_private_modes". An error reply or a dead stream degrades to
// (0,0,false,false,nil,nil) rather than rejecting the seed — only the
// capture-pane reply can do that (see parseCapture). mouse is nil (unknown)
// unless the reply carries the five mouse fields, and modes is nil (unknown)
// unless it carries the private-mode list: a remote tmux missing that format
// expands it empty and drops the field.
func parseCursor(l controlmode.Line, ok bool) (cx, cy int, alt, appCursorKeys bool, mouse *render.MouseMode, modes *render.TerminalModes) {
	if !ok || l.Kind == controlmode.Error {
		return 0, 0, false, false, nil, nil
	}
	fields := strings.Fields(string(l.Data))
	if len(fields) < 4 {
		return 0, 0, false, false, nil, nil
	}
	cx, _ = strconv.Atoi(fields[0])
	cy, _ = strconv.Atoi(fields[1])
	if len(fields) >= 9 {
		mouse = &render.MouseMode{
			Standard: fields[4] == "1",
			Button:   fields[5] == "1",
			All:      fields[6] == "1",
			SGR:      fields[7] == "1",
			UTF8:     fields[8] == "1",
		}
	}
	// #{pane_private_modes} is a comma-separated list of the pane's active
	// DECSET modes. strings.Fields drops it when the list is empty, so a reply
	// with fewer than ten fields (an older remote tmux) leaves both modes
	// unknown rather than reading an absent list as "off".
	if len(fields) >= 10 {
		modes = &render.TerminalModes{
			BracketedPaste: privateModeSet(fields[9], 2004),
			FocusReporting: privateModeSet(fields[9], 1004),
		}
	}
	return cx, cy, fields[2] == "1", fields[3] == "1", mouse, modes
}

// privateModeSet reports whether a #{pane_private_modes} list contains mode.
func privateModeSet(list string, mode int) bool {
	for m := range strings.SplitSeq(list, ",") {
		if n, err := strconv.Atoi(m); err == nil && n == mode {
			return true
		}
	}
	return false
}

// parseCapture parses an already-read capture-pane reply and reports whether
// it is a rejection — either an %error block (e.g. the pane closed between
// list-panes and this capture-pane) or the stream having gone away before any
// reply arrived (ok == false). isErr is the only signal PaneSeeds uses to
// reject a seed: a successful reply with an empty body is a valid blank pane,
// not an error.
func parseCapture(l controlmode.Line, ok bool) (data []byte, isErr bool) {
	if !ok {
		return nil, true
	}
	return l.Data, l.Kind == controlmode.Error
}
