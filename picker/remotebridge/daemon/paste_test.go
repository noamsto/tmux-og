package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
// instead of racing it.
type pasteFixture struct {
	h         *pasteHandler
	uploadErr error
	uploadOut string
	sent      chan string
	notified  chan string

	mu       sync.Mutex
	sentAll  []string
	held     []byte // input finish released, for replay assertions
	refuseAt int    // sendCtl returns false starting at this 0-based call index; -1 = never
}

func newPasteFixture() *pasteFixture {
	f := &pasteFixture{
		uploadOut: "/tmp/og-paste-abc123/img.png",
		sent:      make(chan string, 8),
		notified:  make(chan string, 8),
		refuseAt:  -1,
	}
	f.h = &pasteHandler{
		done: make(chan pasteResult, 1),
		upload: func(_ context.Context, _ string, _ []byte) (string, error) {
			return f.uploadOut, f.uploadErr
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

// awaitOutcome blocks until the async paste produces its first observable
// (a send-keys command or a notify) and returns it with the channel it came
// from drained into the fixture for assertion.
func (f *pasteFixture) awaitOutcome(t *testing.T) (sent, notified string) {
	t.Helper()
	for {
		select {
		case s := <-f.sent:
			return s, ""
		case msg := <-f.notified:
			return "", msg
		case r := <-f.h.results():
			f.held = f.h.finish("%1", r)
		case <-time.After(5 * time.Second):
			t.Fatal("paste produced no outcome")
			return "", ""
		}
	}
}

// finishPaste plays the pump goroutine for the in-flight paste: it waits for
// the goroutine's outcome and runs finish, keeping the released input in held.
func (f *pasteFixture) finishPaste(t *testing.T) {
	t.Helper()
	select {
	case r := <-f.h.results():
		f.held = f.h.finish("%1", r)
	case <-time.After(5 * time.Second):
		t.Fatal("paste goroutine never reported its outcome")
	}
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
// finish (also on the pump goroutine) then injects the path after it.
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
	f.h.upload = func(context.Context, string, []byte) (string, error) {
		<-release
		return f.uploadOut, f.uploadErr
	}
	if got := f.h.handle("%1", []byte("\x16")); len(got) != 0 {
		t.Fatalf("handle returned bytes: %q", got)
	}
	return f, release
}

// TestTypingDuringUploadIsNotHeld pins the responsiveness rule: plain text
// typed while the upload runs is returned at once for pumpInput to send, and
// the path lands after it.
func TestTypingDuringUploadIsNotHeld(t *testing.T) {
	f, release := blockedPaste(t)
	for _, in := range []string{"hello", " wörld"} {
		if got := f.h.handle("%1", []byte(in)); string(got) != in {
			t.Fatalf("plain text %q was held back: got %q", in, got)
		}
	}
	close(release)
	cmd, _ := f.awaitOutcome(t)
	if !strings.HasPrefix(cmd, "send-keys -H -t %1 ") || !strings.HasSuffix(cmd, " 20") {
		t.Fatalf("path injection = %q", cmd)
	}
	if len(f.held) != 0 {
		t.Errorf("nothing was held, finish released %q", f.held)
	}
}

// TestSubmissionAndCursorKeysAreHeldBehindThePath pins the serialization
// rule, now scoped to submission: Enter, anything after it, and the first key
// that could move the cursor are held and handed back, in order, by finish.
func TestSubmissionAndCursorKeysAreHeldBehindThePath(t *testing.T) {
	cases := []struct{ name, in, wantFree, wantHeld string }{
		{"enter", "ab\rcd", "ab", "\rcd"},
		{"lone enter", "\r", "", "\r"},
		{"left arrow", "ab\x1b[Dcd", "ab", "\x1b[Dcd"},
		{"ctrl+a", "ab\x01c", "ab", "\x01c"},
		{"backspace", "a\x7fb", "a", "\x7fb"},
		{"tab", "a\tb", "a", "\tb"},
		{"bracketed paste", "a\x1b[200~zz\x1b[201~", "a", "\x1b[200~zz\x1b[201~"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, release := blockedPaste(t)
			if got := f.h.handle("%1", []byte(c.in)); string(got) != c.wantFree {
				t.Errorf("free = %q, want %q", got, c.wantFree)
			}
			// Everything after the first held byte is held too, even text.
			if got := f.h.handle("%1", []byte("late")); c.wantHeld != "" && len(got) != 0 {
				t.Errorf("text after a held byte was forwarded: %q", got)
			}
			close(release)
			cmd, _ := f.awaitOutcome(t)
			if !strings.HasPrefix(cmd, "send-keys -H -t %1 ") {
				t.Fatalf("path injection = %q", cmd)
			}
			if want := c.wantHeld + "late"; string(f.held) != want {
				t.Errorf("released %q, want %q", f.held, want)
			}
		})
	}
}

// TestHeldInputIsReleasedOnFailure pins that a failed upload still releases
// the held keys, with the failure shown, rather than leaving the prompt stuck.
func TestHeldInputIsReleasedOnFailure(t *testing.T) {
	f, release := blockedPaste(t)
	f.uploadErr = errors.New("exit status 1")
	f.h.handle("%1", []byte("\r"))
	close(release)
	_, notified := f.awaitOutcome(t)
	if !strings.Contains(notified, "upload to remote failed") {
		t.Fatalf("notify = %q", notified)
	}
	f.finishPaste(t)
	if string(f.held) != "\r" {
		t.Errorf("released %q, want the held Enter", f.held)
	}
	if string(f.h.handle("%1", []byte("x"))) != "x" || f.h.pumping {
		t.Error("handler still holds input after the paste ended")
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
	f.awaitOutcome(t)
	if string(f.held) != "\x16" {
		t.Fatalf("held = %q, want the second ctrl+v", f.held)
	}
	if got := f.h.handle("%1", f.held); len(got) != 0 {
		t.Errorf("replayed ctrl+v was forwarded: %q", got)
	}
}

// TestPartialMarkerAfterCtrlVHoldsEverything pins that a triggering frame
// ending inside a bracketed paste holds even plain text, since its body would
// otherwise reach the remote ahead of the path.
func TestPartialMarkerAfterCtrlVHoldsEverything(t *testing.T) {
	f := newPasteFixture()
	f.h.upload = func(context.Context, string, []byte) (string, error) { return f.uploadOut, nil }
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
		{"upload error", func(f *pasteFixture) {
			f.uploadErr = errors.New("exit status 1")
		}, "upload to remote failed"},
		{"bad reply path", func(f *pasteFixture) {
			f.uploadOut = "/etc/passwd"
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
		})
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
	if _, msg := f.awaitOutcome(t); !strings.Contains(msg, "dropped") {
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
