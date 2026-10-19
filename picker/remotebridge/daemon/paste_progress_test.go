package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

type statusCall struct {
	client, msg string
	ttl         time.Duration
}

// statusLog records every status call and wakes waiters.
type statusLog struct {
	mu    sync.Mutex
	calls []statusCall
	ch    chan statusCall
	err   error // returned by record, e.g. a detached client
}

func (l *statusLog) record(client, msg string, ttl time.Duration) error {
	l.mu.Lock()
	l.calls = append(l.calls, statusCall{client, msg, ttl})
	l.mu.Unlock()
	l.ch <- statusCall{client, msg, ttl}
	return l.err
}

func (l *statusLog) all() []statusCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]statusCall(nil), l.calls...)
}

// progressFixture is a paste fixture whose upload blocks until released and
// whose pasting client is "cli-a".
func progressFixture(upload func(ctx context.Context, release <-chan struct{}) (string, error)) (*pasteFixture, *statusLog, chan struct{}) {
	f := newPasteFixture()
	log := &statusLog{ch: make(chan statusCall, 256)}
	release := make(chan struct{})
	f.h.upload = func(ctx context.Context, _ string, _ []byte) (string, error) {
		return upload(ctx, release)
	}
	f.h.clientFor = func() string { return "cli-a" }
	f.h.status = log.record
	f.h.tick = 5 * time.Millisecond
	return f, log, release
}

func awaitStatus(t *testing.T, l *statusLog, match func(statusCall) bool) statusCall {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-l.ch:
			if match(c) {
				return c
			}
		case <-deadline:
			t.Fatalf("no matching status; saw %+v", l.all())
		}
	}
}

func isFrame(c statusCall) bool { return strings.Contains(c.msg, "·") }

// assertSettled waits past several ticks and checks that the last status call
// is the terminal one: nothing paints over it.
func assertSettled(t *testing.T, l *statusLog, wantSubstr string) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	calls := l.all()
	last := calls[len(calls)-1]
	if !strings.Contains(last.msg, wantSubstr) || last.client != "cli-a" {
		t.Fatalf("last status = %+v, want %q on cli-a (all: %+v)", last, wantSubstr, calls)
	}
	for _, c := range calls[:len(calls)-1] {
		if !isFrame(c) {
			t.Errorf("non-frame status before the terminal one: %+v", c)
		}
	}
}

func TestPasteProgressStartsBeforeUploadFinishesAndClearsOnSuccess(t *testing.T) {
	f, log, release := progressFixture(func(_ context.Context, release <-chan struct{}) (string, error) {
		<-release
		return f0path, nil
	})
	f.h.handle("%1", []byte("\x16"))
	first := awaitStatus(t, log, isFrame)
	if first.client != "cli-a" || first.ttl <= 0 {
		t.Fatalf("first frame = %+v", first)
	}
	// An update, while the upload is still blocked.
	awaitStatus(t, log, func(c statusCall) bool { return isFrame(c) && c.msg != first.msg })
	awaitStatus(t, log, func(c statusCall) bool { return strings.Contains(c.msg, "uploading image") })
	select {
	case s := <-f.sent:
		t.Fatalf("path injected before the upload finished: %q", s)
	default:
	}
	close(release)
	if s, n := f.awaitOutcome(t); n != "" || !strings.HasPrefix(s, "send-keys -H -t %1") {
		t.Fatalf("outcome = %q / %q, want the path send", s, n)
	}
	assertSettled(t, log, pasteDoneMsg)
	f.assertNoNotify(t)
}

func TestPasteProgressReplacedOnFailureCancelAndRefusedSend(t *testing.T) {
	cases := []struct {
		name   string
		upload func(context.Context, <-chan struct{}) (string, error)
		refuse bool
		want   string
	}{
		{"upload error", func(_ context.Context, r <-chan struct{}) (string, error) {
			<-r
			return "", errors.New("exit status 1")
		}, false, "upload to remote failed"},
		{"cancelled", func(ctx context.Context, r <-chan struct{}) (string, error) {
			<-r
			return "", context.Canceled
		}, false, "context canceled"},
		{"refused inject", func(_ context.Context, r <-chan struct{}) (string, error) {
			<-r
			return f0path, nil
		}, true, "dropped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, log, release := progressFixture(c.upload)
			if c.refuse {
				f.refuseAt = 0
			}
			f.h.handle("%1", []byte("\x16"))
			awaitStatus(t, log, isFrame)
			close(release)
			awaitStatus(t, log, func(sc statusCall) bool { return strings.Contains(sc.msg, c.want) })
			assertSettled(t, log, c.want)
			f.assertNoNotify(t)
		})
	}
}

func TestPasteProgressStopsOnExtractFailure(t *testing.T) {
	f, log, _ := progressFixture(nil)
	f.h.probeClipboard = func() (clipboardProbe, bool, error) {
		return clipboardProbe{ext: "png", extract: func() ([]byte, error) { return nil, errors.New("xclip died") }}, true, nil
	}
	f.h.handle("%1", []byte("\x16"))
	awaitStatus(t, log, func(c statusCall) bool { return strings.Contains(c.msg, "read failed") })
	assertSettled(t, log, "read failed")
}

func TestPasteWithoutClientFallsBackToNotify(t *testing.T) {
	f, log, _ := progressFixture(func(context.Context, <-chan struct{}) (string, error) {
		return "", errors.New("boom")
	})
	f.h.clientFor = func() string { return "" }
	f.h.handle("%1", []byte("\x16"))
	if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") {
		t.Fatalf("notify = %q", msg)
	}
	if calls := log.all(); len(calls) != 0 {
		t.Fatalf("status called with no client: %+v", calls)
	}
}

func TestPasteFailureFallsBackToNotifyWhenClientDetached(t *testing.T) {
	f, log, release := progressFixture(func(_ context.Context, r <-chan struct{}) (string, error) {
		<-r
		return "", errors.New("boom")
	})
	log.err = errors.New("can't find client")
	f.h.handle("%1", []byte("\x16"))
	close(release)
	if msg := f.awaitNotify(t); !strings.Contains(msg, "upload to remote failed") {
		t.Fatalf("notify = %q", msg)
	}
}

func TestRefusedKeptPrefixAbortsBeforeUpload(t *testing.T) {
	f, log, _ := progressFixture(func(context.Context, <-chan struct{}) (string, error) {
		t.Error("upload ran after the kept prefix was refused")
		return "", errors.New("unreachable")
	})
	f.refuseAt = 0
	f.h.handle("%1", []byte("ab\x16"))
	awaitStatus(t, log, func(c statusCall) bool { return strings.Contains(c.msg, "dropped") })
	assertSettled(t, log, "dropped")
	for _, c := range log.all() {
		if isFrame(c) {
			t.Errorf("progress frame after a refused kept prefix: %+v", c)
		}
	}
}

func TestPasteProgressHoldsLaterInputBehindThePath(t *testing.T) {
	f, log, release := progressFixture(func(_ context.Context, r <-chan struct{}) (string, error) {
		<-r
		return f0path, nil
	})
	f.h.handle("%1", []byte("\x16"))
	awaitStatus(t, log, isFrame)
	got := make(chan []byte, 1)
	go func() { got <- f.h.handle("%1", []byte("x\r")) }()
	select {
	case <-got:
		t.Fatal("later input was not held behind the paste")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	f.awaitOutcome(t) // the path send
	select {
	case b := <-got:
		if string(b) != "x\r" {
			t.Errorf("held input = %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("held input never released")
	}
}

func TestActiveClientPicksMostRecentlyActive(t *testing.T) {
	cases := []struct{ name, out, want string }{
		{"most recent wins", "100|cli-a\n300|cli-b\n200|cli-c\n", "cli-b"},
		{"first wins a tie", "300|cli-a\n300|cli-b\n", "cli-a"},
		{"no clients", "", ""},
		{"garbage skipped", "x|cli-a\n50|cli-b\n", "cli-b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{LocalTmuxOut: func(...string) (string, error) { return c.out, nil }}
			if got := activeClient(cfg); got != c.want {
				t.Errorf("activeClient = %q, want %q", got, c.want)
			}
		})
	}
	if got := activeClient(Config{LocalTmuxOut: func(...string) (string, error) { return "", errors.New("x") }}); got != "" {
		t.Errorf("error lookup = %q, want empty", got)
	}
}

const f0path = "/tmp/og-paste-abc123/img.png"

// TestPasteProgressShowsOnThePastingClientOnly runs the real wiring against a
// scratch tmux with two attached clients. Each client is a pane of an outer
// scratch server, so capture-pane reads exactly what that client draws; no
// pty library is needed. The progress and the failure must appear on the most
// recently active client only, and be gone once the ttl passes.
func TestPasteProgressShowsOnThePastingClientOnly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH")
		}
		t.Skip("tmux is not available")
	}
	inner := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))
	sock, err := inner("display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("inner socket: %v", err)
	}
	attach := "env -u TMUX tmux -S " + strings.TrimSpace(string(sock)) + " attach -t w"

	outerDir, err := os.MkdirTemp("", "lo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outerDir) })
	outer := func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", append([]string{"-L", "o"}, args...)...)
		cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+outerDir)
		return cmd
	}
	t.Cleanup(func() { _ = outer("kill-server").Run() })
	for _, name := range []string{"a", "b"} {
		if out, err := outer("new-session", "-d", "-s", name, "-x", "80", "-y", "24", attach).CombinedOutput(); err != nil {
			t.Fatalf("outer %s: %v\n%s", name, err, out)
		}
	}
	awaitScreen(t, "two inner clients", func() bool {
		out, _ := inner("list-clients", "-t", "w", "-F", "#{client_name}").Output()
		return len(strings.Fields(string(out))) == 2
	})
	// client_activity has one-second resolution; type into one client so it is
	// the most recent. Which outer session that is is read back below from the
	// client's tty, since a terminal's own query replies also count as activity.
	time.Sleep(1100 * time.Millisecond)
	if out, err := outer("send-keys", "-t", "b", "x").CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v\n%s", err, out)
	}
	time.Sleep(1100 * time.Millisecond)

	cfg := Config{
		LocalSess: "w",
		LocalTmuxOut: func(args ...string) (string, error) {
			out, err := inner(args...).Output()
			return string(out), err
		},
		LocalTmux: func(args ...string) error { return inner(args...).Run() },
	}
	screen := func(name string) string {
		out, _ := outer("capture-pane", "-p", "-t", name).Output()
		return string(out)
	}

	// The outer session whose pane tty is the chosen client is the pasting one.
	active := activeClient(cfg)
	ttys, _ := outer("list-panes", "-a", "-F", "#{pane_tty}|#{session_name}").Output()
	pasting, other := "", ""
	for line := range strings.SplitSeq(strings.TrimSpace(string(ttys)), "\n") {
		tty, name, _ := strings.Cut(line, "|")
		if tty == active {
			pasting = name
		} else {
			other = name
		}
	}
	if pasting == "" || other == "" {
		t.Fatalf("active client %q matches no outer pane (%q)", active, ttys)
	}

	f := newPasteFixture()
	release := make(chan struct{})
	f.h.upload = func(context.Context, string, []byte) (string, error) {
		<-release
		return "", errors.New("exit status 1")
	}
	f.h.clientFor = func() string { return activeClient(cfg) }
	f.h.status = func(client, msg string, ttl time.Duration) error { return showOnClient(cfg, client, msg, ttl) }
	f.h.tick = 100 * time.Millisecond
	f.h.handle("%1", []byte("\x16"))

	awaitScreen(t, "spinner on the active client", func() bool {
		return strings.Contains(screen(pasting), "uploading image")
	})
	if got := screen(other); strings.Contains(got, "uploading image") || strings.Contains(got, "tmux-og:") {
		t.Errorf("progress flashed on the other client:\n%s", got)
	}
	close(release)
	awaitScreen(t, "failure on the pasting client", func() bool {
		return strings.Contains(screen(pasting), "upload to remote failed")
	})
	if got := screen(other); strings.Contains(got, "upload to remote failed") {
		t.Errorf("failure shown on the other client:\n%s", got)
	}
	// The ttl-bound frames are gone, replaced by the failure, which then expires.
	awaitScreen(t, "message to clear", func() bool {
		return !strings.Contains(screen(pasting), "tmux-og:")
	})
}

func awaitScreen(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
