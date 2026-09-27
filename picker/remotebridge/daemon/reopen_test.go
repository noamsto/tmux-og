package daemon

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// callLog records argv calls from both LocalTmux and LocalTmuxOut in the
// order they happened. resetMirrorSession's ordering guarantee (new window
// before any kill) only means anything if both fakes feed one shared,
// ordered, concurrency-safe log.
type callLog struct {
	mu    sync.Mutex
	calls [][]string
}

func (l *callLog) add(argv []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, append([]string(nil), argv...))
}

func (l *callLog) snapshot() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]string(nil), l.calls...)
}

// TestTombstoneTextStripsControlBytes checks the message a gone mirror shows
// is safe for a local pty: host/session come from the remote and may carry
// escape sequences (an OSC title, a C1 control), which must not survive into
// terminal output. A lone C1 byte is invalid UTF-8 and comes out as U+FFFD,
// which is just as inert on the pty as dropping it.
func TestTombstoneTextStripsControlBytes(t *testing.T) {
	text := tombstoneText("h\x1b]0;x\x07\x9b", "s\u009bq\n")

	if !utf8.ValidString(text) {
		t.Errorf("tombstoneText(...) = %q is not valid UTF-8", text)
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			t.Errorf("tombstoneText(...) = %q contains control rune %U", text, r)
		}
	}
	if !strings.Contains(text, "h]0;x") {
		t.Errorf("tombstoneText(...) = %q, want it to contain the sanitized host %q", text, "h]0;x")
	}
	if !strings.Contains(text, "sq") {
		t.Errorf("tombstoneText(...) = %q, want it to contain the sanitized session %q", text, "sq")
	}
	if !strings.Contains(text, "no longer exists") {
		t.Errorf("tombstoneText(...) = %q, want it to contain %q", text, "no longer exists")
	}
}

// TestResetMirrorSessionKillsEveryOldWindow pins the rebuild-without-emptying
// order the doc comment promises: the fresh window goes up before any old
// one comes down, and only the old windows die — never the one just created.
func TestResetMirrorSessionKillsEveryOldWindow(t *testing.T) {
	var log callLog
	cfg := Config{
		LocalSess: "m",
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "list-windows":
				return "@1\n@4\n", nil
			case "new-window":
				return "@9\n", nil
			}
			return "", nil
		},
		LocalTmux: func(args ...string) error {
			log.add(args)
			return nil
		},
	}

	id, ok := resetMirrorSession(cfg, "sleep", "2147483647")
	if !ok || id != "@9" {
		t.Fatalf("resetMirrorSession(...) = (%q, %v), want (\"@9\", true)", id, ok)
	}

	calls := log.snapshot()
	wantNewWindow := argvKey([]string{
		"new-window", "-d", "-P", "-F", "#{window_id}",
		"-a", "-t", "m:{end}", "--", "sleep", "2147483647",
	})
	newWindowIdx := -1
	killed := map[string]bool{}
	for i, argv := range calls {
		if argvKey(argv) == wantNewWindow {
			newWindowIdx = i
		}
		if argv[0] == "kill-window" {
			if newWindowIdx == -1 {
				t.Errorf("kill-window ran before the new window went up: %v", calls)
			}
			target := argv[len(argv)-1]
			if target == "@9" {
				t.Errorf("kill-window targeted the new window @9: %v", argv)
			}
			killed[target] = true
		}
	}
	if newWindowIdx == -1 {
		t.Fatalf("new-window call %v not found among %v", wantNewWindow, calls)
	}
	want := map[string]bool{"@1": true, "@4": true}
	if len(killed) != len(want) {
		t.Fatalf("killed windows = %v, want exactly %v", killed, want)
	}
}

// TestResetMirrorSessionKillsNothingWhenTheNewWindowFails guards the
// session-never-empties promise from the other side: if the replacement
// window never lands, nothing may be killed — there would be nothing left to
// hold the session open.
func TestResetMirrorSessionKillsNothingWhenTheNewWindowFails(t *testing.T) {
	var killed [][]string
	cfg := Config{
		LocalSess: "m",
		LocalTmuxOut: func(args ...string) (string, error) {
			switch args[0] {
			case "list-windows":
				return "@1\n@4\n", nil
			case "new-window":
				return "", errors.New("boom")
			}
			return "", nil
		},
		LocalTmux: func(args ...string) error {
			killed = append(killed, args)
			return nil
		},
	}

	id, ok := resetMirrorSession(cfg, "sleep", "2147483647")
	if ok || id != "" {
		t.Fatalf("resetMirrorSession(...) = (%q, %v), want (\"\", false) when new-window fails", id, ok)
	}
	if len(killed) != 0 {
		t.Errorf("kill-window ran %v after a failed new-window — the session must never be emptied", killed)
	}
}

// TestTombstoneMirrorUnsetsTheDaemonOptions checks the tombstone retires
// @bridge_sock/@bridge_state (nothing is dialling or listening any more)
// while leaving @bridge_host/@bridge_session standing, so og-remote-open's
// pair lookup still finds this mirror on the next open.
func TestTombstoneMirrorUnsetsTheDaemonOptions(t *testing.T) {
	var log callLog
	cfg := Config{
		LocalSess:     "m",
		RemoteHost:    "host",
		RemoteSession: "sess",
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "list-windows":
				return "@1\n@4\n", nil
			case "new-window":
				return "@9\n", nil
			}
			return "", nil
		},
		LocalTmux: func(args ...string) error {
			log.add(args)
			return nil
		},
	}

	tombstoneMirror(cfg)

	calls := log.snapshot()
	got := map[string]bool{}
	var newWindowArgv []string
	for _, argv := range calls {
		got[argvKey(argv)] = true
		if argv[0] == "new-window" {
			newWindowArgv = argv
		}
		if argv[0] == "set-option" {
			for _, a := range argv {
				if a == "@bridge_host" || a == "@bridge_session" {
					t.Errorf("tombstoneMirror touched %s, want it left standing: %v", a, argv)
				}
			}
		}
	}

	want := []string{
		argvKey([]string{"set-option", "-w", "-t", "@9", "remain-on-exit", "off"}),
		argvKey([]string{"set-option", "-u", "-t", "m", "@bridge_sock"}),
		argvKey([]string{"set-option", "-u", "-t", "m", "@bridge_state"}),
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing call %q among %v", w, calls)
		}
	}

	if newWindowArgv == nil {
		t.Fatal("no new-window call recorded")
	}
	dashDash := -1
	for i, a := range newWindowArgv {
		if a == "--" {
			dashDash = i
			break
		}
	}
	if dashDash == -1 || dashDash+2 >= len(newWindowArgv) ||
		newWindowArgv[dashDash+1] != "sh" || newWindowArgv[dashDash+2] != "-c" {
		t.Fatalf("new-window argv %v does not start with -- sh -c", newWindowArgv)
	}
	last := newWindowArgv[len(newWindowArgv)-1]
	if want := tombstoneText(cfg.RemoteHost, cfg.RemoteSession); last != want {
		t.Errorf("new-window's last arg = %q, want tombstoneText(...) = %q", last, want)
	}
}

// reopenFake fakes the two LocalTmuxOut queries showReopenNotice's call
// chain depends on — localViewing's session probe and notifyLocal's client
// lookup — plus LocalTmux's display-message calls. Guarded by a mutex since
// showReopenNotice runs on its own goroutine in these tests.
type reopenFake struct {
	mu       sync.Mutex
	viewing  bool
	displays [][]string
}

func (f *reopenFake) setViewing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viewing = v
}

func (f *reopenFake) localTmuxOut(args ...string) (string, error) {
	f.mu.Lock()
	viewing := f.viewing
	f.mu.Unlock()
	if len(args) > 1 && args[0] == "list-clients" && args[1] == "-F" {
		// localViewing's session probe: list-clients -F #{client_session}.
		if viewing {
			return "m\n", nil
		}
		return "", nil
	}
	if len(args) > 1 && args[0] == "list-clients" && args[1] == "-t" {
		// notifyLocal's client lookup: list-clients -t <sess> -F #{client_name}.
		return "/dev/pts/9\n", nil
	}
	return "", nil
}

func (f *reopenFake) localTmux(args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.displays = append(f.displays, append([]string(nil), args...))
	return nil
}

func (f *reopenFake) displayCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.displays)
}

// advancingNudge answers a fresh, always-later mtime on every call, so
// focusEdge.poll always has a touch to explain and re-reads viewing.
func advancingNudge() func() (time.Time, bool) {
	var n int64
	return func() (time.Time, bool) {
		return time.Unix(0, atomic.AddInt64(&n, 1)), true
	}
}

// TestShowReopenNoticeWaitsForAViewer models the common case: the re-open
// happens with the mirror unobserved (a probe, a wake restore), and the
// notice must hold until the park's focus edge catches a client arriving —
// then fire exactly once and let the goroutine return.
func TestShowReopenNoticeWaitsForAViewer(t *testing.T) {
	f := &reopenFake{} // no viewer at start
	cfg := Config{
		LocalSess:     "m",
		RemoteHost:    "host",
		RemoteSession: "sess",
		LocalTmuxOut:  f.localTmuxOut,
		LocalTmux:     f.localTmux,
	}
	stop := make(chan struct{})
	defer close(stop)

	done := make(chan struct{})
	go func() {
		showReopenNotice(cfg, advancingNudge(), stop)
		close(done)
	}()

	// The viewer arrives well after the early no-viewer checks but before
	// the first 1s focus tick, so the notice must come from the focus edge,
	// not the 15s recheck backstop.
	time.AfterFunc(200*time.Millisecond, func() { f.setViewing(true) })

	deadline := time.After(5 * time.Second)
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
waitForNotice:
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the reopen notice")
		case <-poll.C:
			if f.displayCount() >= 1 {
				break waitForNotice
			}
		}
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("showReopenNotice did not return after sending the notice")
	}

	if n := f.displayCount(); n != 1 {
		t.Fatalf("display-message sent %d times, want exactly 1", n)
	}

	// Give an errant second notice the time it would need to land, then
	// confirm none did — the goroutine already returned.
	time.Sleep(150 * time.Millisecond)
	if n := f.displayCount(); n != 1 {
		t.Fatalf("a second notice landed after showReopenNotice returned: %d display-message calls", n)
	}
}

// TestShowReopenNoticeStopsWithoutAViewer checks stop bounds the wait by the
// mirror's life: with nobody ever looking, closing stop must return the
// goroutine without ever sending a notice.
func TestShowReopenNoticeStopsWithoutAViewer(t *testing.T) {
	f := &reopenFake{} // no viewer, ever
	cfg := Config{
		LocalSess:     "m",
		RemoteHost:    "host",
		RemoteSession: "sess",
		LocalTmuxOut:  f.localTmuxOut,
		LocalTmux:     f.localTmux,
	}
	stop := make(chan struct{})

	done := make(chan struct{})
	go func() {
		showReopenNotice(cfg, advancingNudge(), stop)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let it settle into the wait loop
	close(stop)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("showReopenNotice did not return after stop closed")
	}
	if n := f.displayCount(); n != 0 {
		t.Errorf("display-message sent %d times with no viewer ever present, want 0", n)
	}
}

// TestShowReopenNoticeShowsAtOnceToACurrentViewer checks the fast path: a
// viewer already looking when the re-open happens gets the notice
// immediately, without waiting on the focus ticker at all.
func TestShowReopenNoticeShowsAtOnceToACurrentViewer(t *testing.T) {
	f := &reopenFake{viewing: true}
	cfg := Config{
		LocalSess:     "m",
		RemoteHost:    "host",
		RemoteSession: "sess",
		LocalTmuxOut:  f.localTmuxOut,
		LocalTmux:     f.localTmux,
	}
	stop := make(chan struct{})
	defer close(stop)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		showReopenNotice(cfg, advancingNudge(), stop)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("showReopenNotice did not return immediately for an already-viewing client")
	}
	if elapsed := time.Since(start); elapsed >= 1*time.Second {
		t.Errorf("took %v to notify an already-present viewer, want well under 1s", elapsed)
	}
	if n := f.displayCount(); n != 1 {
		t.Errorf("display-message sent %d times, want exactly 1", n)
	}
}
