package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestSplitPasteDrops(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKept  string
		wantDrops int
	}{
		{"no ctrl-v", "ls\r", "ls\r", 0},
		{"bare ctrl-v", "\x16", "", 1},
		{"mid-chunk", "a\x16b", "ab", 1},
		{"two in one payload", "\x16\x16", "", 2},
		// Inside a bracketed paste the byte is content, not the gesture.
		{"inside bracketed paste", "\x1b[200~a\x16b\x1b[201~", "\x1b[200~a\x16b\x1b[201~", 0},
		{"before and after bracket", "\x16\x1b[200~\x16\x1b[201~\x16", "\x1b[200~\x16\x1b[201~", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &pasteHandler{}
			kept, drops := h.splitPasteDrops([]byte(c.in))
			if string(kept) != c.wantKept || drops != c.wantDrops {
				t.Errorf("splitPasteDrops(%q) = %q, %d — want %q, %d",
					c.in, kept, drops, c.wantKept, c.wantDrops)
			}
		})
	}
}

// TestSplitPasteDropsCrossesFrames pins that the bracketed-paste scan state
// must survive across separate handle calls (pty reads), since a paste
// longer than one 4096-byte frame arrives as several. A 0x16 in a later
// frame, still inside the bracket opened by an earlier one, must be kept.
func TestSplitPasteDropsCrossesFrames(t *testing.T) {
	h := &pasteHandler{}
	kept1, drops1 := h.splitPasteDrops([]byte("\x1b[200~start"))
	if drops1 != 0 || string(kept1) != "\x1b[200~start" {
		t.Fatalf("frame 1: kept=%q drops=%d", kept1, drops1)
	}
	if !h.inPaste {
		t.Fatal("frame 1 did not leave inPaste set")
	}
	kept2, drops2 := h.splitPasteDrops([]byte("mid\x16dle\x1b[201~"))
	if drops2 != 0 {
		t.Fatalf("frame 2: a 0x16 mid-bracket was dropped: kept=%q drops=%d", kept2, drops2)
	}
	if string(kept2) != "mid\x16dle\x1b[201~" {
		t.Errorf("frame 2: kept=%q, want the 0x16 preserved as content", kept2)
	}
	if h.inPaste {
		t.Error("frame 2 did not close the bracket")
	}
}

func TestImageTarget(t *testing.T) {
	cases := []struct {
		name       string
		targets    []string
		wantTarget string
		wantExt    string
	}{
		{"no image", []string{"text/plain", "UTF8_STRING"}, "", ""},
		{"png", []string{"text/plain", "image/png"}, "image/png", "png"},
		{"jpeg maps to jpg", []string{"image/jpeg"}, "image/jpeg", "jpg"},
		{"png preferred over bmp", []string{"image/bmp", "image/png"}, "image/png", "png"},
		{"bmp only", []string{"image/bmp"}, "image/bmp", "bmp"},
		{"whitespace tolerated", []string{"image/png\r"}, "image/png", "png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, ext := imageTarget(c.targets)
			if target != c.wantTarget || ext != c.wantExt {
				t.Errorf("imageTarget(%v) = %q, %q — want %q, %q",
					c.targets, target, ext, c.wantTarget, c.wantExt)
			}
		})
	}
}

// pasteFixture wires a handler with fakes; every observable the async paste
// goroutine produces arrives on a channel, so tests synchronize with it
// instead of racing it. The test goroutine plays the pump: it alone calls
// handle and settle, so the fields below that settle writes need no lock.
type pasteFixture struct {
	h         *pasteHandler
	uploadErr error
	path      string // what newPath picks
	sent      chan string
	notified  chan string

	mu       sync.Mutex
	sentAll  []string
	refuseAt int // sendCtl returns false starting at this 0-based call index; -1 = never

	frames     [][]byte // every frame deliver was given
	heldFrames [][]byte // the frames the final settle released
	held       []byte   // heldFrames joined, for replay assertions
}

func newPasteFixture() *pasteFixture {
	f := &pasteFixture{
		path:     f0path,
		sent:     make(chan string, 32),
		notified: make(chan string, 8),
		refuseAt: -1,
	}
	f.h = &pasteHandler{
		done:    make(chan pasteResult, 2),
		newPath: func(string) string { return f.path },
		upload: func(context.Context, string, []byte) error {
			return f.uploadErr
		},
		probeClipboard: func() (clipboardProbe, bool, error) {
			return clipboardProbe{
				ext:     "png",
				extract: func() ([]byte, error) { return []byte("png-bytes"), nil },
			}, true, nil
		},
		procFor: func(string) (string, error) { return "claude", nil },
		notify:  func(msg string) { f.notified <- msg },
		sendCtl: f.sendCtl,
	}
	return f
}

// Variadic to match Config.SendCtl, though the paste path only ever sends one
// command per call — the batching form exists for the relay publish (D7).
func (f *pasteFixture) sendCtl(cmds ...string) bool {
	for _, s := range cmds {
		f.mu.Lock()
		idx := len(f.sentAll)
		f.sentAll = append(f.sentAll, s)
		refuse := f.refuseAt >= 0 && idx >= f.refuseAt
		f.mu.Unlock()
		if refuse {
			return false
		}
		f.sent <- s
	}
	return true
}

// deliver stands in for pumpInput's: it records the frame, runs it through
// handle and sends what handle forwards through the same sendCtl the path and
// the erase use, as pumpInput's send shares one writer with them.
func (f *pasteFixture) deliver(b []byte) {
	f.frames = append(f.frames, b)
	for _, args := range controlmode.SendKeysArgs("%1", f.h.handle("%1", b), controlmode.InputChunkBytes) {
		f.sendCtl(strings.Join(args, " "))
	}
}

// settle plays the pump for one paste result, keeping what a final released.
func (f *pasteFixture) settle(r pasteResult) {
	start := len(f.frames)
	f.h.settle("%1", r, f.deliver)
	if !r.ready {
		f.heldFrames = f.frames[start:]
		f.held = bytes.Join(f.heldFrames, nil)
	}
}

// awaitOutcome plays the pump (settling the ready, then the final) until the
// async paste produces its first observable — a send-keys command or a notify
// — and returns it.
func (f *pasteFixture) awaitOutcome(t *testing.T) (sent, notified string) {
	t.Helper()
	for {
		select {
		case s := <-f.sent:
			return s, ""
		case msg := <-f.notified:
			return "", msg
		case r := <-f.h.results():
			f.settle(r)
		case <-time.After(5 * time.Second):
			t.Fatal("paste produced no outcome")
			return "", ""
		}
	}
}

// awaitReady settles the in-flight paste's ready result, which sends the path
// while the upload runs behind it.
func (f *pasteFixture) awaitReady(t *testing.T) {
	t.Helper()
	select {
	case r := <-f.h.results():
		if !r.ready {
			t.Fatalf("paste ended before its path was ready: %+v", r)
		}
		f.settle(r)
	case <-time.After(5 * time.Second):
		t.Fatal("paste never posted its ready path")
	}
}

// finishPaste plays the pump for the in-flight paste until its final result
// is settled, keeping the released input in held. A final already settled by
// awaitOutcome leaves nothing in flight, so it returns at once.
func (f *pasteFixture) finishPaste(t *testing.T) {
	t.Helper()
	for f.h.pumping() {
		select {
		case r := <-f.h.results():
			f.settle(r)
			if !r.ready {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("paste goroutine never reported its outcome")
		}
	}
}

// sentLog is every command sendCtl was given so far, in order.
func (f *pasteFixture) sentLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sentAll...)
}

// sendKeysCmd is the command the pump sends for b on pane %1.
func sendKeysCmd(b string) string {
	args := controlmode.SendKeysArgs("%1", []byte(b), controlmode.InputChunkBytes)
	if len(args) != 1 {
		panic("sendKeysCmd: payload spans more than one chunk")
	}
	return strings.Join(args[0], " ")
}

// assertNoNotify pins that a silent forward stays silent: notifies are
// dispatched async, so it waits briefly for a late one.
func (f *pasteFixture) assertNoNotify(t *testing.T) {
	t.Helper()
	select {
	case msg := <-f.notified:
		t.Errorf("unexpected notify: %q", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// awaitNotify returns the next notify, failing fast instead of hanging.
func (f *pasteFixture) awaitNotify(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-f.notified:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("no notify")
		return ""
	}
}

func TestHandleForwardsWithoutCtrlV(t *testing.T) {
	f := newPasteFixture()
	in := []byte("ls -la\r")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Errorf("handle rewrote %q to %q", in, got)
	}
}

func TestHandleForwardsOnNonAgentPane(t *testing.T) {
	f := newPasteFixture()
	f.h.procFor = func(string) (string, error) { return "fish", nil }
	in := []byte("\x16")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Errorf("shell pane: handle dropped the byte: %q", got)
	}
	f.assertNoNotify(t)
}

// pi reads the system clipboard on ctrl+v exactly as claude does, so a pi
// pane takes the path mechanism too: the mirrored pane's clipboard is
// headless, and the byte reaching it would otherwise no-op.
func TestHandleSwallowsOnPiPane(t *testing.T) {
	f := newPasteFixture()
	f.h.procFor = func(string) (string, error) { return "pi", nil }
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("pi pane: handle forwarded the byte instead of pasting: %q", got)
	}
	if _, notified := f.awaitOutcome(t); notified != "" {
		t.Fatalf("unexpected notify: %q", notified)
	}
}

func TestHandleForwardsWhenNoImage(t *testing.T) {
	f := newPasteFixture()
	f.h.probeClipboard = func() (clipboardProbe, bool, error) { return clipboardProbe{}, false, nil }
	in := []byte("\x16")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Errorf("empty clipboard: handle dropped the byte: %q", got)
	}
	f.assertNoNotify(t)
}

// TestHandleSwallowsAndInjectsOnImage pins the ordering: the byte is
// swallowed, the text typed before it is returned for pumpInput to send, and
// settle (also on the pump goroutine) then injects the path after it.
func TestHandleSwallowsAndInjectsOnImage(t *testing.T) {
	f := newPasteFixture()
	got := f.h.handle("%1", []byte("x\x16"))
	if string(got) != "x" {
		t.Fatalf("handle returned %q, want the typed prefix x", got)
	}
	cmd, notified := f.awaitOutcome(t)
	if notified != "" {
		t.Fatalf("unexpected notify: %q", notified)
	}
	// The injection is hex send-keys of the path plus a trailing space
	// (0x20), so what the user types next cannot merge into the path token.
	if !strings.HasPrefix(cmd, "send-keys -H -t %1 ") {
		t.Fatalf("injection %q is not a hex send-keys to the pane", cmd)
	}
	if !strings.HasSuffix(cmd, " 20") {
		t.Errorf("injection %q does not end with the trailing space byte", cmd)
	}
	select {
	case extra := <-f.sent:
		t.Errorf("more than one command sent; extra: %q", extra)
	default:
	}
}

// blockedPaste starts a paste whose upload waits on release.
func blockedPaste(t *testing.T) (*pasteFixture, chan struct{}) {
	t.Helper()
	f := newPasteFixture()
	release := make(chan struct{})
	f.h.upload = func(context.Context, string, []byte) error {
		<-release
		return f.uploadErr
	}
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("handle returned bytes: %q", got)
	}
	return f, release
}

// TestReadyPathSendPrecedesUploadReturn pins the latency fix: the path the
// daemon picked is in the prompt while the upload still runs, and the upload
// stores the clipboard bytes at that same path.
func TestReadyPathSendPrecedesUploadReturn(t *testing.T) {
	f := newPasteFixture()
	release := make(chan struct{})
	type call struct {
		path string
		data []byte
	}
	calls := make(chan call, 1)
	f.h.upload = func(_ context.Context, path string, data []byte) error {
		calls <- call{path, data}
		<-release
		return nil
	}
	f.h.handle("%1", []byte("\x16"))
	f.awaitReady(t)
	if got, want := f.sentLog(), []string{sendKeysCmd(f0path + " ")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sent while the upload runs = %q, want the path and its trailing space %q", got, want)
	}
	select {
	case c := <-calls:
		if c.path != f0path || string(c.data) != "png-bytes" {
			t.Errorf("upload(%q, %q), want the sent path and the clipboard bytes", c.path, c.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload never started")
	}
	close(release)
	f.finishPaste(t)
	if got := f.sentLog(); len(got) != 1 {
		t.Errorf("a successful paste sent more than the path: %q", got)
	}
}

// TestTypingDuringUploadIsNotHeld pins the responsiveness rule: plain text
// typed while the bytes are read, and again once the path is in the prompt,
// is returned at once for pumpInput to send.
func TestTypingDuringUploadIsNotHeld(t *testing.T) {
	f, release := blockedPaste(t)
	if got := f.h.handle("%1", []byte("hello")); string(got) != "hello" {
		t.Fatalf("plain text typed while reading was held back: got %q", got)
	}
	f.awaitReady(t)
	if got := f.h.handle("%1", []byte(" wörld")); string(got) != " wörld" {
		t.Fatalf("plain text typed while uploading was held back: got %q", got)
	}
	close(release)
	f.finishPaste(t)
	if len(f.held) != 0 {
		t.Errorf("nothing was held, the final released %q", f.held)
	}
}

// TestSubmissionAndCursorKeysAreHeldBehindThePath pins the uploading-phase
// rule: once the path is in the prompt, cursor and edit keys pass (they act
// on a buffer that already holds it), while anything that could submit — and
// everything after it — waits for the upload and is released, in order, by
// the final. Before the path is in, a cursor key waits for it.
func TestSubmissionAndCursorKeysAreHeldBehindThePath(t *testing.T) {
	cases := []struct{ name, in, wantFree, wantHeld string }{
		{"enter", "ab\rcd", "ab", "\rcd"},
		{"lone enter", "\r", "", "\r"},
		{"left arrow", "ab\x1b[Dcd", "ab\x1b[Dcd", ""},
		{"ctrl+a", "ab\x01c", "ab\x01c", ""},
		{"backspace", "a\x7fb", "a\x7fb", ""},
		{"tab", "a\tb", "a\tb", ""},
		{"ctrl+c", "a\x03b", "a", "\x03b"},
		{"sgr click", "a\x1b[<0;5;5M", "a", "\x1b[<0;5;5M"},
		{"bracketed paste", "a\x1b[200~zz\x1b[201~", "a", "\x1b[200~zz\x1b[201~"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, release := blockedPaste(t)
			f.awaitReady(t)
			if got := f.h.handle("%1", []byte(c.in)); string(got) != c.wantFree {
				t.Errorf("free = %q, want %q", got, c.wantFree)
			}
			wantHeld := c.wantHeld
			got := f.h.handle("%1", []byte("late"))
			switch {
			case wantHeld == "" && string(got) != "late":
				t.Errorf("text with nothing held was held back: got %q", got)
			case wantHeld != "":
				// Everything after the first held byte is held too, even text.
				if len(got) != 0 {
					t.Errorf("text after a held byte was forwarded: %q", got)
				}
				wantHeld += "late"
			}
			close(release)
			f.finishPaste(t)
			if string(f.held) != wantHeld {
				t.Errorf("released %q, want %q", f.held, wantHeld)
			}
		})
	}

	t.Run("cursor key while reading waits for the path", func(t *testing.T) {
		f, release := blockedPaste(t)
		for _, in := range []string{"\x1b[D", "z"} {
			if got := f.h.handle("%1", []byte(in)); len(got) != 0 {
				t.Fatalf("%q forwarded behind a cursor key before the path was in the prompt: %q", in, got)
			}
		}
		f.awaitReady(t)
		want := []string{sendKeysCmd(f0path + " "), sendKeysCmd("\x1b[D"), sendKeysCmd("z")}
		if got := f.sentLog(); !reflect.DeepEqual(got, want) {
			t.Errorf("sent = %q, want the path, then every held frame the uploading rule passes %q", got, want)
		}
		close(release)
		f.finishPaste(t)
		if len(f.held) != 0 {
			t.Errorf("the final released %q, want nothing left held", f.held)
		}
	})
}

// TestHeldInputIsReleasedOnFailure pins that a failed upload still releases
// the held keys, with the failure shown, rather than leaving the prompt stuck.
func TestHeldInputIsReleasedOnFailure(t *testing.T) {
	f, release := blockedPaste(t)
	f.uploadErr = errors.New("exit status 1")
	f.h.handle("%1", []byte("\r"))
	close(release)
	f.finishPaste(t)
	if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") {
		t.Fatalf("notify = %q", msg)
	}
	if string(f.held) != "\r" {
		t.Errorf("released %q, want the held Enter", f.held)
	}
	if string(f.h.handle("%1", []byte("x"))) != "x" || f.h.pumping() {
		t.Error("handler still holds input after the paste ended")
	}
}

// TestFailedUploadErasesThePathBeforeTheHeldEnter pins the failure cleanup
// when nothing passed since the path went in: the path and its space are
// backspaced away on the pump, ahead of the held Enter, so the agent is never
// asked to submit a path to a file that does not exist.
func TestFailedUploadErasesThePathBeforeTheHeldEnter(t *testing.T) {
	f, release := blockedPaste(t)
	f.uploadErr = errors.New("exit status 1")
	f.awaitReady(t)
	if got := f.h.handle("%1", []byte("\r")); len(got) != 0 {
		t.Fatalf("Enter forwarded during the upload: %q", got)
	}
	close(release)
	f.finishPaste(t)
	want := []string{
		sendKeysCmd(f0path + " "),
		sendKeysCmd(strings.Repeat("\x7f", len(f0path)+1)),
		sendKeysCmd("\r"),
	}
	if got := f.sentLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %q, want the path, its erase, then the held Enter %q", got, want)
	}
	if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") || strings.Contains(msg, "path left") {
		t.Errorf("notify = %q, want the upload failure without a path-left note", msg)
	}
}

// TestFailureAfterPassThroughKeepsThePath pins the other failure branch: once
// a key passed after the path, backspacing a fixed count could eat the user's
// own edit, so the path stays and the message says so.
func TestFailureAfterPassThroughKeepsThePath(t *testing.T) {
	f, release := blockedPaste(t)
	f.uploadErr = errors.New("exit status 1")
	f.awaitReady(t)
	f.deliver([]byte("\x1b[D"))
	f.deliver([]byte("\r"))
	close(release)
	f.finishPaste(t)
	want := []string{sendKeysCmd(f0path + " "), sendKeysCmd("\x1b[D"), sendKeysCmd("\r")}
	if got := f.sentLog(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %q, want no erase: %q", got, want)
	}
	if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") || !strings.Contains(msg, "(path left in the prompt)") {
		t.Errorf("notify = %q, want the failure and that the path was left", msg)
	}
}

// TestHeldCtrlVIsReinterceptedOnRelease pins that held input replays through
// handle, so a second ctrl+v typed during the first upload is not forwarded raw.
func TestHeldCtrlVIsReinterceptedOnRelease(t *testing.T) {
	f, release := blockedPaste(t)
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("second ctrl+v during an upload was forwarded: %q", got)
	}
	close(release)
	f.finishPaste(t)
	if string(f.held) != "\x16" {
		t.Fatalf("held = %q, want the second ctrl+v", f.held)
	}
	if !f.h.pumping() {
		t.Error("the replayed ctrl+v did not start a second paste")
	}
	if slices.Contains(f.sentLog(), sendKeysCmd("\x16")) {
		t.Errorf("replayed ctrl+v was forwarded: %q", f.sentLog())
	}
}

// TestHeldPasteAndCtrlVReplayAtFinal pins the uploading phase's handling of
// the rest of the stream: an arrow passes, a bracketed paste waits (its
// markers are not classified as safe) and replays at the final, and a ctrl+v
// typed after it is still intercepted rather than reaching the remote.
func TestHeldPasteAndCtrlVReplayAtFinal(t *testing.T) {
	f, release := blockedPaste(t)
	f.awaitReady(t)
	const pasted = "\x1b[200~text\x1b[201~"
	for _, in := range []string{"\x1b[C", pasted, "\x16"} {
		f.deliver([]byte(in))
	}
	if got, want := f.sentLog(), []string{sendKeysCmd(f0path + " "), sendKeysCmd("\x1b[C")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sent during the upload = %q, want the path and the arrow %q", got, want)
	}
	close(release)
	f.finishPaste(t)
	if got, want := f.heldFrames, [][]byte{[]byte(pasted), []byte("\x16")}; !reflect.DeepEqual(got, want) {
		t.Errorf("the final released %q, want %q", got, want)
	}
	if got, want := f.sentLog(), []string{sendKeysCmd(f0path + " "), sendKeysCmd("\x1b[C"), sendKeysCmd(pasted)}; !reflect.DeepEqual(got, want) {
		t.Errorf("sent = %q, want %q", got, want)
	}
	if !f.h.pumping() {
		t.Error("the ctrl+v replayed after the bracketed paste did not start a paste")
	}
}

// TestHoldAllHoldsWhileUploading pins that a paste triggered from a frame
// that ended inside a bracketed paste keeps holding everything once the path
// is in, keys the uploading rule would otherwise pass included: the paste
// body must not trail the path while it is still open.
func TestHoldAllHoldsWhileUploading(t *testing.T) {
	f := newPasteFixture()
	release := make(chan struct{})
	f.h.upload = func(context.Context, string, []byte) error { <-release; return nil }
	if got := f.h.handle("%1", []byte("\x16\x1b[200~body")); len(got) != 0 {
		t.Fatalf("handle returned %q", got)
	}
	f.awaitReady(t)
	for _, in := range []string{"\x1b[D", "more\x1b[201~"} {
		if got := f.h.handle("%1", []byte(in)); len(got) != 0 {
			t.Errorf("%q forwarded while holding everything: %q", in, got)
		}
	}
	if got, want := f.sentLog(), []string{sendKeysCmd(f0path + " ")}; !reflect.DeepEqual(got, want) {
		t.Errorf("sent during the upload = %q, want only the path", got)
	}
	close(release)
	f.finishPaste(t)
	if want := "\x1b[200~body\x1b[Dmore\x1b[201~"; string(f.held) != want {
		t.Errorf("the final released %q, want %q", f.held, want)
	}
}

// TestPassedInputLeavesPasteScanAtRest pins why frames passed during an
// upload may skip splitPasteDrops: whatever submitHold lets through, scanned
// from rest, ends back at rest — outside a paste, with no partial marker — so
// the scan state the final replay resumes from is the one it would have had.
func TestPassedInputLeavesPasteScanAtRest(t *testing.T) {
	samples := []string{
		"plain wörld", "\x01\x02\x05\x06\x08\t\x0b\x15\x17\x7f",
		"\x1b[A\x1b[B\x1b[C\x1b[D\x1b[F\x1b[H\x1b[Z", "\x1b[1;5C", "\x1b[3~", "\x1b[20~", "\x1b[2001~",
		"\x1b[I\x1b[O", "\x1bOA\x1bOH", "\x1bb\x1bf\x1bd\x1b\x7f",
	}
	rng := rand.New(rand.NewPCG(1158, 0))
	alphabet := []byte("\x1b[O0129;~<ACDHMIbfu\r\x16\x7f\x01xé")
	for range 5000 {
		b := make([]byte, rng.IntN(12))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		samples = append(samples, string(b))
	}
	for _, s := range samples {
		passed := []byte(s)[:submitHold([]byte(s))]
		h := &pasteHandler{}
		_, drops := h.splitPasteDrops(passed)
		if drops != 0 || h.inPaste || h.matched != 0 {
			t.Errorf("passed %q left drops=%d inPaste=%v matched=%d", passed, drops, h.inPaste, h.matched)
		}
	}
}

// TestPartialMarkerAfterCtrlVHoldsEverything pins that a triggering frame
// ending inside a bracketed paste holds even plain text, since its body would
// otherwise reach the remote ahead of the path.
func TestPartialMarkerAfterCtrlVHoldsEverything(t *testing.T) {
	f := newPasteFixture()
	if got := f.h.handle("%1", []byte("\x16\x1b[200~body")); len(got) != 0 {
		t.Fatalf("handle returned %q", got)
	}
	if got := f.h.handle("%1", []byte("more")); len(got) != 0 {
		t.Errorf("paste body forwarded ahead of the path: %q", got)
	}
}

func TestPasteFailuresNotifyAndSendNothing(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*pasteFixture)
		wantInMsg string
	}{
		{"bmp unsupported", func(f *pasteFixture) {
			f.h.probeClipboard = func() (clipboardProbe, bool, error) {
				return clipboardProbe{ext: "bmp", extract: func() ([]byte, error) { return []byte("b"), nil }}, true, nil
			}
		}, "not pasteable"},
		{"extract error", func(f *pasteFixture) {
			f.h.probeClipboard = func() (clipboardProbe, bool, error) {
				return clipboardProbe{ext: "png", extract: func() ([]byte, error) { return nil, errors.New("xclip died") }}, true, nil
			}
		}, "clipboard image read failed"},
		{"oversize", func(f *pasteFixture) {
			f.h.probeClipboard = func() (clipboardProbe, bool, error) {
				return clipboardProbe{ext: "png", extract: func() ([]byte, error) { return make([]byte, pasteMaxBytes+1), nil }}, true, nil
			}
		}, "too large"},
		{"generated path rejected", func(f *pasteFixture) {
			f.path = "/etc/passwd"
		}, "unexpected paste path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPasteFixture()
			c.mutate(f)
			if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
				t.Fatalf("byte not swallowed: %q", got)
			}
			sent, notified := f.awaitOutcome(t)
			if sent != "" {
				t.Fatalf("send-keys ran on failure: %q", sent)
			}
			if !strings.Contains(notified, c.wantInMsg) {
				t.Errorf("notify %q lacks %q", notified, c.wantInMsg)
			}
			// The final carries no message of its own: the goroutine said it.
			f.finishPaste(t)
			if got := f.sentLog(); len(got) != 0 {
				t.Errorf("sent = %q, want nothing", got)
			}
			f.assertNoNotify(t)
		})
	}

	t.Run("upload error", func(t *testing.T) {
		f := newPasteFixture()
		f.uploadErr = errors.New("exit status 1")
		if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
			t.Fatalf("byte not swallowed: %q", got)
		}
		f.finishPaste(t)
		want := []string{sendKeysCmd(f0path + " "), sendKeysCmd(strings.Repeat("\x7f", len(f0path)+1))}
		if got := f.sentLog(); !reflect.DeepEqual(got, want) {
			t.Errorf("sent = %q, want the path then its erase %q", got, want)
		}
		if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") {
			t.Errorf("notify = %q", msg)
		}
	})
}

// TestNewPastePath pins the daemon-chosen remote path: it passes the local
// validation for every whitelisted extension and its directory is 32 hex
// characters of fresh randomness, so nothing can be pre-created ahead of it.
func TestNewPastePath(t *testing.T) {
	for _, ext := range []string{"png", "jpg", "jpeg", "gif", "webp"} {
		p := newPastePath(ext)
		if !pastePathRe.MatchString(p) {
			t.Errorf("newPastePath(%q) = %q fails pastePathRe", ext, p)
		}
		dir, ok := strings.CutSuffix(strings.TrimPrefix(p, "/tmp/og-paste-"), "/img."+ext)
		if _, err := hex.DecodeString(dir); !ok || len(dir) != 32 || err != nil {
			t.Errorf("newPastePath(%q) = %q, want /tmp/og-paste-<32 hex>/img.%s", ext, p, ext)
		}
	}
	if a, b := newPastePath("png"), newPastePath("png"); a == b {
		t.Errorf("two pastes picked the same path %q", a)
	}
}

func TestHandleClipboardProbeErrorNotifies(t *testing.T) {
	f := newPasteFixture()
	f.h.probeClipboard = func() (clipboardProbe, bool, error) {
		return clipboardProbe{}, false, errors.New("xclip died")
	}
	in := []byte("\x16")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Fatalf("byte not forwarded unchanged: %q", got)
	}
	msg := f.awaitNotify(t)
	if !strings.Contains(msg, "ctrl+v forwarded") || !strings.Contains(msg, "xclip died") {
		t.Errorf("notify %q lacks the forwarded notice or the probe error", msg)
	}
}

func TestHandleGateLookupErrorNotifies(t *testing.T) {
	f := newPasteFixture()
	f.h.procFor = func(string) (string, error) { return "", errors.New("mirror pane %1 not found") }
	in := []byte("\x16")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Fatalf("byte not forwarded unchanged: %q", got)
	}
	msg := f.awaitNotify(t)
	if !strings.Contains(msg, "ctrl+v forwarded") || !strings.Contains(msg, "not found") {
		t.Errorf("notify %q lacks the forwarded notice or the gate error", msg)
	}
}

// TestHandleBurstedDropsWithProbeErrorNotifiesOnce pins that a forwarded
// two-gesture frame explains itself once, not once per byte.
func TestHandleBurstedDropsWithProbeErrorNotifiesOnce(t *testing.T) {
	f := newPasteFixture()
	f.h.probeClipboard = func() (clipboardProbe, bool, error) {
		return clipboardProbe{}, false, errors.New("xclip died")
	}
	in := []byte("\x16\x16")
	if got := f.h.handle("%1", in); string(got) != string(in) {
		t.Fatalf("payload not forwarded unchanged: %q", got)
	}
	f.awaitNotify(t)
	f.assertNoNotify(t)
}

// TestHandleNotifiesOnRefusedSend pins that a dropped send-keys (e.g. the
// bridge mid-reconnect) surfaces a notify rather than vanishing.
func TestHandleNotifiesOnRefusedSend(t *testing.T) {
	f := newPasteFixture()
	f.refuseAt = 0
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("byte not swallowed: %q", got)
	}
	f.finishPaste(t)
	if msg := f.awaitNotify(t); !strings.Contains(msg, "dropped") {
		t.Errorf("notify %q does not describe the dropped send", msg)
	}
	select {
	case s := <-f.sent:
		t.Errorf("a refused send should not also land as sent: %q", s)
	default:
	}
}

// TestHandleNotifiesOnExtraBurstedDrop pins that two 0x16 in one frame
// trigger exactly one paste, and the extra gesture is notified rather than
// silently discarded.
func TestHandleNotifiesOnExtraBurstedDrop(t *testing.T) {
	f := newPasteFixture()
	got := f.h.handle("%1", []byte("\x16\x16"))
	if len(got) != 0 {
		t.Fatalf("byte(s) not swallowed: %q", got)
	}
	var sawExtraNotice, sawInjection bool
	for range 2 {
		sent, msg := f.awaitOutcome(t)
		sawExtraNotice = sawExtraNotice || strings.Contains(msg, "extra")
		sawInjection = sawInjection || sent != ""
	}
	if !sawExtraNotice {
		t.Error("no notify about the extra swallowed ctrl+v")
	}
	if !sawInjection {
		t.Error("the single paste never injected a path")
	}
}

func TestPasterNilWithoutUpload(t *testing.T) {
	var cfg Config
	if cfg.paster() != nil {
		t.Error("paster() non-nil without PasteUpload")
	}
}

// TestHandleMarkerSplitAcrossFrames pins that a bracketed-paste marker split
// across two handle calls must not change the scan state: a split end marker
// still closes the bracket (a later ctrl+v is the gesture), a split begin
// marker still opens it (a 0x16 in the body is content).
func TestHandleMarkerSplitAcrossFrames(t *testing.T) {
	begin, end := string(bracketedPasteBegin), string(bracketedPasteEnd)
	await := func(t *testing.T, f *pasteFixture) {
		t.Helper()
		s, msg := f.awaitOutcome(t)
		if msg != "" {
			t.Fatalf("unexpected notify: %q", msg)
		}
		if !strings.HasPrefix(s, "send-keys -H -t %1 ") {
			t.Errorf("send %q is not a hex send-keys to the pane", s)
		}
	}
	assertSwallowedAndPasted := func(t *testing.T, f *pasteFixture) {
		t.Helper()
		if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
			t.Fatalf("ctrl+v after the paste closed was forwarded as %q, want swallowed", got)
		}
		await(t, f)
	}

	for k := 1; k <= 5; k++ {
		t.Run(fmt.Sprintf("end marker split after %d bytes", k), func(t *testing.T) {
			f := newPasteFixture()
			f.h.handle("%1", []byte(begin+"text"+end[:k]))
			f.h.handle("%1", []byte(end[k:]))
			assertSwallowedAndPasted(t, f)
		})
		t.Run(fmt.Sprintf("begin marker split after %d bytes", k), func(t *testing.T) {
			f := newPasteFixture()
			f.h.handle("%1", []byte("x"+begin[:k]))
			in := begin[k:] + "a\x16b"
			if got := f.h.handle("%1", []byte(in)); string(got) != in {
				t.Fatalf("0x16 inside the bracket was not kept as content: got %q, want %q", got, in)
			}
			select {
			case s := <-f.sent:
				t.Fatalf("paste triggered from inside the bracket: %q", s)
			case msg := <-f.notified:
				t.Fatalf("paste triggered from inside the bracket, notify: %q", msg)
			case <-time.After(50 * time.Millisecond):
			}
			if got := f.h.handle("%1", []byte(end)); string(got) != end {
				t.Fatalf("end marker was not forwarded unchanged: %q", got)
			}
			assertSwallowedAndPasted(t, f)
		})
	}

	t.Run("carried partial that mismatches, then full end marker", func(t *testing.T) {
		f := newPasteFixture()
		f.h.handle("%1", []byte(begin+"t\x1b["))
		f.h.handle("%1", []byte(end))
		assertSwallowedAndPasted(t, f)
	})
}

// writeClipStub installs a fake clipboard tool whose listing prints body, or
// fails with stderr when failErr is non-empty.
func writeClipStub(t *testing.T, dir, name, body, failErr string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s' '" + body + "'\n"
	if failErr != "" {
		script = "#!/bin/sh\necho '" + failErr + "' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
}

func TestProbeClipboardImage(t *testing.T) {
	type stub struct{ name, body, failErr string }
	cases := []struct {
		name    string
		stubs   []stub
		wantOK  bool
		wantExt string
		wantErr string // substring; "" means err must be nil
	}{
		{name: "no tools", wantErr: "no clipboard tool"},
		{
			name:    "wl-paste offers png",
			stubs:   []stub{{name: "wl-paste", body: "text/plain\nimage/png\n"}},
			wantOK:  true,
			wantExt: "png",
		},
		{
			name:  "text only",
			stubs: []stub{{name: "wl-paste", body: "text/plain\nUTF8_STRING\n"}},
		},
		{
			name:    "unsupported image type",
			stubs:   []stub{{name: "wl-paste", body: "image/tiff\n"}},
			wantErr: "image/tiff",
		},
		{
			name:    "listing fails with stderr",
			stubs:   []stub{{name: "wl-paste", failErr: "Nothing is copied"}},
			wantErr: "wl-paste: Nothing is copied",
		},
		{
			name: "xclip fails, wl-paste lists text",
			stubs: []stub{
				{name: "xclip", failErr: "Error: target TARGETS not available"},
				{name: "wl-paste", body: "text/plain\n"},
			},
		},
		{
			name:    "osascript offers png",
			stubs:   []stub{{name: "osascript", body: "«class PNGf», 12345, TIFF picture, 67890"}},
			wantOK:  true,
			wantExt: "png",
		},
		{
			name:    "osascript offers only tiff",
			stubs:   []stub{{name: "osascript", body: "TIFF picture, 67890"}},
			wantOK:  true,
			wantExt: "png",
		},
		{
			name:    "osascript offers only jpeg",
			stubs:   []stub{{name: "osascript", body: "JPEG picture, 30456"}},
			wantOK:  true,
			wantExt: "png",
		},
		{
			name:  "osascript text only",
			stubs: []stub{{name: "osascript", body: "«class utf8», 5, string, 5"}},
		},
		{
			name:    "osascript fails with stderr",
			stubs:   []stub{{name: "osascript", failErr: "execution error: not authorized"}},
			wantErr: "osascript: execution error: not authorized",
		},
		{
			name: "xclip text, wl-paste png",
			stubs: []stub{
				{name: "xclip", body: "text/plain\n"},
				{name: "wl-paste", body: "image/png\n"},
			},
			wantOK:  true,
			wantExt: "png",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, s := range c.stubs {
				writeClipStub(t, dir, s.name, s.body, s.failErr)
			}
			t.Setenv("PATH", dir)
			probe, ok, err := probeClipboardImage()
			if ok != c.wantOK || probe.ext != c.wantExt {
				t.Errorf("ok=%v ext=%q, want ok=%v ext=%q", ok, probe.ext, c.wantOK, c.wantExt)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestExtractOsascriptPNG(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	write := func(t *testing.T, data string) {
		t.Helper()
		dir := t.TempDir()
		script := "#!/bin/sh\ncase \"$2\" in\n'clipboard info') printf '%s' '«class PNGf», 11';;\n*) printf '%s' '" + data + "';;\nesac\n"
		if err := os.WriteFile(filepath.Join(dir, "osascript"), []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
	}
	probe := func(t *testing.T) clipboardProbe {
		t.Helper()
		p, ok, err := probeClipboardImage()
		if !ok || err != nil {
			t.Fatalf("probe ok=%v err=%v", ok, err)
		}
		return p
	}

	t.Run("round trip", func(t *testing.T) {
		write(t, "«data PNGf"+hex.EncodeToString(png)+"»")
		got, err := probe(t).extract()
		if err != nil || !bytes.Equal(got, png) {
			t.Fatalf("extract = %x, %v; want %x", got, err, png)
		}
	})
	t.Run("oversize rejected", func(t *testing.T) {
		if _, err := decodeOsascriptPNG(make([]byte, osascriptReadLimit)); err == nil {
			t.Fatal("oversize payload was accepted")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		for _, in := range []string{"", "garbage", "«data PNGfzz»", "«data PNGf8950"} {
			if _, err := decodeOsascriptPNG([]byte(in)); err == nil {
				t.Errorf("decodeOsascriptPNG(%q) succeeded", in)
			}
		}
	})
}

func TestBridgeProc(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		outErr   error
		pane     string
		wantProc string
		wantErr  string
	}{
		{name: "lookup error", outErr: errors.New("no server"), pane: "%1", wantErr: "lookup failed"},
		{name: "found", out: "%1|claude|/s|1\n%2|fish|/s|1\n", pane: "%2", wantProc: "fish"},
		{name: "pane missing", out: "%1|claude|/s|1\n", pane: "%9", wantErr: "not found"},
		{name: "empty proc", out: "%1||/s|1\n", pane: "%1", wantErr: "no @bridge_proc yet"},
		// Remote pane ids collide across daemons sharing a host session.
		{name: "another daemon's colliding pane is ignored", out: "%3|claude|/other\n%3|fish|/s|1\n", pane: "%3", wantProc: "fish"},
		{name: "only another daemon's pane", out: "%3|claude|/other\n", pane: "%3", wantErr: "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var argv []string
			cfg := Config{
				LocalSess: "m",
				SockPath:  "/s|1",
				LocalTmuxOut: func(args ...string) (string, error) {
					argv = args
					return c.out, c.outErr
				},
			}
			got, err := bridgeProc(cfg, c.pane)
			if want := []string{"list-panes", "-s", "-t", "m", "-F", "#{@bridge_pane}|#{@bridge_proc}|#{@bridge_sock}"}; !reflect.DeepEqual(argv, want) {
				t.Errorf("asked %q, want %q", argv, want)
			}
			if got != c.wantProc {
				t.Errorf("proc = %q, want %q", got, c.wantProc)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

// #956: a wrapper script keeps bash as the pane's foreground command while the
// agent runs as its child, so the paste gate reads @bridge_proc == bash and
// forwards 0x16. The shipper resolves the remote poller's per-pane tree agent
// into @bridge_proc; this test drives that stamp through the real bridgeProc
// lookup and the paste gate, so a wrong option name, a shifted format field or
// a wrong pane mapping cannot pass unnoticed.
func TestWrapperAgentPasteEndToEnd(t *testing.T) {
	dir := privateDir(t)
	a := &agentShipper{dir: dir, sess: "lab-mono", written: map[string]paneStatus{}}
	stamped := ""
	cfg := Config{
		LocalPanes: func() map[string]string { return map[string]string{"%1": "%7"} },
		LocalTmux: func(args ...string) error {
			if len(args) == 6 && args[0] == "set-option" && args[4] == "@bridge_proc" {
				stamped = args[5]
			}
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) { return "%1|" + stamped + "|\n", nil },
	}
	a.apply(cfg, []paneStatus{{pane: "%1", proc: "bash", treeAgent: "claude"}})
	if stamped != "claude" {
		t.Fatalf("shipper resolved @bridge_proc = %q, want claude", stamped)
	}

	f := newPasteFixture()
	f.h.procFor = func(remotePane string) (string, error) { return bridgeProc(cfg, remotePane) }
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("wrapper pane forwarded the byte instead of pasting: %q", got)
	}
	sent, notified := f.awaitOutcome(t)
	if notified != "" {
		t.Fatalf("unexpected notify: %q", notified)
	}
	if sent == "" {
		t.Fatal("wrapper paste injected no path")
	}
}

// TestBytesTypedBeforeCtrlVAreNeverHeld pins that control bytes ahead of the
// gesture in the same frame go out before the path, as they were typed.
func TestBytesTypedBeforeCtrlVAreNeverHeld(t *testing.T) {
	f := newPasteFixture()
	if got := f.h.handle("%1", []byte("hi\r\x16ab\rcd")); string(got) != "hi\rab" {
		t.Fatalf("free = %q, want the pre-gesture bytes and plain text up to the first held byte", got)
	}
	f.finishPaste(t)
	if string(f.held) != "\rcd" {
		t.Errorf("released %q, want only the suffix typed after the gesture", f.held)
	}
}

// TestHeldFramesKeepTheirBoundaries pins that a held lone Escape is replayed
// as its own frame, not fused with the text typed after it.
func TestHeldFramesKeepTheirBoundaries(t *testing.T) {
	f, release := blockedPaste(t)
	f.h.handle("%1", []byte("\x1b"))
	f.h.handle("%1", []byte("hello"))
	close(release)
	f.finishPaste(t)
	if got, want := f.heldFrames, [][]byte{[]byte("\x1b"), []byte("hello")}; !reflect.DeepEqual(got, want) {
		t.Errorf("frames = %q, want [ESC hello]", got)
	}
}
