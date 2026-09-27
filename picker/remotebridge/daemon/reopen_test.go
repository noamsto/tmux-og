package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// testPin is the pin the recording fakes below answer to as still standing;
// ownedAnswer is display-message's reply that says so.
var testPin = localPin{serverPID: "100", id: "$1"}

const ownedAnswer = "100|$1\n"

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
		local:     testPin,
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "display-message":
				return ownedAnswer, nil
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
		"-a", "-t", "$1:{end}", "--", "sleep", "2147483647",
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
		local:     testPin,
		LocalTmuxOut: func(args ...string) (string, error) {
			switch args[0] {
			case "display-message":
				return ownedAnswer, nil
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
		local:         testPin,
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "display-message":
				return ownedAnswer, nil
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

	tombstoneMirror(cfg, tombstoneText(cfg.RemoteHost, cfg.RemoteSession))

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
		argvKey([]string{"set-option", "-u", "-t", "$1", "@bridge_sock"}),
		argvKey([]string{"set-option", "-u", "-t", "$1", "@bridge_state"}),
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

var errRebuild = errors.New("daemon: identity read for sess timed out")

// runLoopConfig is a Config for driving runLoop: recording local tmux fakes
// that answer to testPin as still standing, and a pidfile naming this process,
// as the replaced run's teardown leaves it.
func runLoopConfig(t *testing.T, log *callLog, shutdown chan struct{}) (Config, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sock")
	pidFile := sock + ".pid"
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	return Config{
		LocalSess:     "m",
		RemoteHost:    "host",
		RemoteSession: "sess",
		SockPath:      sock,
		Shutdown:      shutdown,
		local:         testPin,
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "display-message":
				return ownedAnswer, nil
			case "list-windows":
				return "@1\n", nil
			case "new-window":
				return "@9\n", nil
			}
			return "", nil
		},
		LocalTmux: func(args ...string) error {
			log.add(args)
			return nil
		},
	}, pidFile
}

// scriptedRun answers runLoop's calls from results in order, repeating the
// last one past the end, and records the cfg.reopened each call saw.
func scriptedRun(results ...error) (func(Config) error, *[]bool) {
	var seen []bool
	return func(cfg Config) error {
		seen = append(seen, cfg.reopened)
		return results[min(len(seen), len(results))-1]
	}, &seen
}

func findCall(calls [][]string, verb string) []string {
	for _, argv := range calls {
		if argv[0] == verb {
			return argv
		}
	}
	return nil
}

func pidFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestRunLoopRebuildsOnAReplacedServer checks a replaced server re-runs the
// mirror as a re-open: only the rebuild carries reopened, which is what shows
// the fresh-server notice.
func TestRunLoopRebuildsOnAReplacedServer(t *testing.T) {
	var log callLog
	cfg, _ := runLoopConfig(t, &log, make(chan struct{}))
	run, seen := scriptedRun(errServerReplaced, nil)

	if err := runLoop(cfg, run); err != nil {
		t.Fatalf("runLoop(...) = %v, want nil", err)
	}
	if want := []bool{false, true}; !slices.Equal(*seen, want) {
		t.Errorf("reopened per run = %v, want %v", *seen, want)
	}
}

// TestRunLoopStopBetweenRunsKillsTheSession checks a stop landing as the old
// mirror is torn down never rebuilds: the placeholder session is killed, as
// og-remote-detach expects, and the pidfile the replaced run kept goes too.
func TestRunLoopStopBetweenRunsKillsTheSession(t *testing.T) {
	var log callLog
	shutdown := make(chan struct{})
	cfg, pidFile := runLoopConfig(t, &log, shutdown)
	calls := 0
	run := func(Config) error {
		calls++
		close(shutdown)
		return errServerReplaced
	}

	if err := runLoop(cfg, run); err != nil {
		t.Fatalf("runLoop(...) = %v, want nil on a stop", err)
	}
	if calls != 1 {
		t.Errorf("run called %d times, want 1 — a stop must not rebuild", calls)
	}
	if kill := findCall(log.snapshot(), "kill-session"); argvKey(kill) != argvKey([]string{"kill-session", "-t", "$1"}) {
		t.Errorf("kill-session call = %v, want kill-session -t $1 among %v", kill, log.snapshot())
	}
	if pidFileExists(t, pidFile) {
		t.Error("pidfile still present after a stop between runs")
	}
}

// TestRunLoopTombstonesARebuildThatNeverStands checks a rebuild that fails
// before its mirror stood is not retried — its dial would run with no listener
// up — and ends in a tombstone that says the re-open failed, not that the
// session is gone, which the new server confirmed moments before.
func TestRunLoopTombstonesARebuildThatNeverStands(t *testing.T) {
	var log callLog
	cfg, pidFile := runLoopConfig(t, &log, make(chan struct{}))
	run, seen := scriptedRun(errServerReplaced, errRebuild)

	if err := runLoop(cfg, run); !errors.Is(err, errRebuild) {
		t.Fatalf("runLoop(...) = %v, want %v", err, errRebuild)
	}
	if len(*seen) != 2 {
		t.Errorf("run called %d times, want 2 — the replaced run and one rebuild", len(*seen))
	}
	nw := findCall(log.snapshot(), "new-window")
	if nw == nil {
		t.Fatalf("no tombstone new-window among %v", log.snapshot())
	}
	if last, want := nw[len(nw)-1], reopenFailedText("host", "sess"); last != want {
		t.Errorf("tombstone text = %q, want %q", last, want)
	}
	if pidFileExists(t, pidFile) {
		t.Error("pidfile still present after the rebuild was abandoned")
	}
}

// TestRunLoopStopDuringAFailedRebuildKillsTheSession checks a stop that
// arrives while a rebuild is failing ends like any other stop: kill-session,
// no tombstone.
func TestRunLoopStopDuringAFailedRebuildKillsTheSession(t *testing.T) {
	var log callLog
	shutdown := make(chan struct{})
	cfg, pidFile := runLoopConfig(t, &log, shutdown)
	calls := 0
	run := func(cfg Config) error {
		calls++
		if !cfg.reopened {
			return errServerReplaced
		}
		close(shutdown)
		return errRebuild
	}

	if err := runLoop(cfg, run); err != nil {
		t.Fatalf("runLoop(...) = %v, want nil on a stop", err)
	}
	if calls != 2 {
		t.Errorf("run called %d times, want 2", calls)
	}
	if nw := findCall(log.snapshot(), "new-window"); nw != nil {
		t.Errorf("a stopped rebuild was tombstoned: %v", nw)
	}
	if findCall(log.snapshot(), "kill-session") == nil {
		t.Errorf("no kill-session among %v", log.snapshot())
	}
	if pidFileExists(t, pidFile) {
		t.Error("pidfile still present after a stop")
	}
}

// TestRunLoopLeavesATornDownRebuildAlone checks an error runMirror returns
// after its own teardown is passed straight through: that teardown already
// killed the session and removed the pidfile, and a session og-remote-open
// recreated under the same name since must never be touched (#680).
func TestRunLoopLeavesATornDownRebuildAlone(t *testing.T) {
	var log callLog
	cfg, pidFile := runLoopConfig(t, &log, make(chan struct{}))
	run, seen := scriptedRun(errServerReplaced, tornDown{errRebuild})

	err := runLoop(cfg, run)
	if !errors.As(err, new(tornDown)) || !errors.Is(err, errRebuild) {
		t.Fatalf("runLoop(...) = %v, want tornDown{%v}", err, errRebuild)
	}
	if len(*seen) != 2 {
		t.Errorf("run called %d times, want 2 — a torn-down rebuild is not retried", len(*seen))
	}
	if calls := log.snapshot(); len(calls) != 0 {
		t.Errorf("runLoop touched the local server after runMirror's teardown: %v", calls)
	}
	if !pidFileExists(t, pidFile) {
		t.Error("runLoop removed the pidfile runMirror's teardown owns")
	}
}

// TestRunLoopFirstRunErrorIsNotARebuild checks a first open's failure is the
// daemon's error, unretried: there is no replaced server behind it, and the
// launcher owns what a failed first open leaves.
func TestRunLoopFirstRunErrorIsNotARebuild(t *testing.T) {
	var log callLog
	cfg, pidFile := runLoopConfig(t, &log, make(chan struct{}))
	run, seen := scriptedRun(errRebuild)

	if err := runLoop(cfg, run); err != errRebuild {
		t.Fatalf("runLoop(...) = %v, want %v as-is", err, errRebuild)
	}
	if len(*seen) != 1 {
		t.Errorf("run called %d times, want 1", len(*seen))
	}
	if calls := log.snapshot(); len(calls) != 0 {
		t.Errorf("runLoop touched the local server after a first-run error: %v", calls)
	}
	if !pidFileExists(t, pidFile) {
		t.Error("runLoop removed the pidfile after a first-run error")
	}
}

// TestRunLoopLeavesASessionThatIsNotOursAlone checks a rebuild that found the
// local session no longer its own ends quietly: runMirror already logged it,
// and whatever holds that name now — a sibling, a namesake og-remote-open
// recreated with its own daemon — is not this daemon's to touch.
func TestRunLoopLeavesASessionThatIsNotOursAlone(t *testing.T) {
	var log callLog
	cfg, pidFile := runLoopConfig(t, &log, make(chan struct{}))
	run, seen := scriptedRun(errServerReplaced, errNotOurs)

	if err := runLoop(cfg, run); err != nil {
		t.Fatalf("runLoop(...) = %v, want nil", err)
	}
	if len(*seen) != 2 {
		t.Errorf("run called %d times, want 2", len(*seen))
	}
	if calls := log.snapshot(); len(calls) != 0 {
		t.Errorf("runLoop touched the local server for a session that is not its own: %v", calls)
	}
	if !pidFileExists(t, pidFile) {
		t.Error("runLoop removed the pidfile on a session that is not its own")
	}
}

// TestEndReopenKeepsAnotherDaemonsPidFile checks endReopen removes the pidfile
// only while it still names this process: a daemon og-remote-open started for
// the pair meanwhile has written its own there.
func TestEndReopenKeepsAnotherDaemonsPidFile(t *testing.T) {
	var log callLog
	cfg, pidFile := runLoopConfig(t, &log, make(chan struct{}))
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid()+1)), 0o600); err != nil {
		t.Fatal(err)
	}

	endReopen(cfg, "")

	if !pidFileExists(t, pidFile) {
		t.Error("endReopen removed a pidfile naming another process")
	}
}

// fakeServer is a local tmux that resolves targets the way tmux does, down to
// the traps the pin exists for: a bare name falls back to a unique prefix once
// the exact session is gone, and display-message on a missing $N exits 0 with
// an empty id.
type fakeServer struct {
	t        *testing.T
	pid      string
	sessions []*fakeSession
	nextWin  int
	gone     error // tmux's exit 1, "can't find ..."
	mutated  []string
}

type fakeSession struct {
	id, name string
	windows  []string
}

func (f *fakeServer) resolve(target string) *fakeSession {
	target, _, _ = strings.Cut(target, ":")
	match := func(ok func(*fakeSession) bool) *fakeSession {
		for _, s := range f.sessions {
			if ok(s) {
				return s
			}
		}
		return nil
	}
	switch {
	case strings.HasPrefix(target, "$"):
		return match(func(s *fakeSession) bool { return s.id == target })
	case strings.HasPrefix(target, "@"):
		return match(func(s *fakeSession) bool { return slices.Contains(s.windows, target) })
	case strings.HasPrefix(target, "="):
		return match(func(s *fakeSession) bool { return s.name == target[1:] })
	}
	if s := match(func(s *fakeSession) bool { return s.name == target }); s != nil {
		return s
	}
	var prefixed []*fakeSession
	for _, s := range f.sessions {
		if strings.HasPrefix(s.name, target) {
			prefixed = append(prefixed, s)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0]
	}
	return nil
}

func fakeTarget(args []string) string {
	if i := slices.Index(args, "-t"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *fakeServer) mutate(s *fakeSession, args []string) {
	f.mutated = append(f.mutated, s.id+" "+strings.Join(args, " "))
}

func (f *fakeServer) out(args ...string) (string, error) {
	s := f.resolve(fakeTarget(args))
	switch args[0] {
	case "list-sessions":
		var b strings.Builder
		for _, s := range f.sessions {
			b.WriteString(f.pid + "|" + s.id + "|" + s.name + "\n")
		}
		return b.String(), nil
	case "display-message":
		if s == nil {
			return f.pid + "|\n", nil
		}
		return f.pid + "|" + s.id + "\n", nil
	case "list-windows":
		if s == nil {
			return "", f.gone
		}
		return strings.Join(s.windows, "\n") + "\n", nil
	case "new-window":
		if s == nil {
			return "", f.gone
		}
		f.nextWin++
		id := "@" + strconv.Itoa(f.nextWin)
		s.windows = append(s.windows, id)
		f.mutate(s, args)
		return id + "\n", nil
	}
	f.t.Fatalf("fakeServer: unexpected read %v", args)
	return "", nil
}

func (f *fakeServer) run(args ...string) error {
	target := fakeTarget(args)
	s := f.resolve(target)
	if s == nil {
		return f.gone
	}
	f.mutate(s, args)
	switch args[0] {
	case "kill-window":
		s.windows = slices.DeleteFunc(s.windows, func(w string) bool { return w == target })
	case "kill-session":
		f.sessions = slices.DeleteFunc(f.sessions, func(x *fakeSession) bool { return x == s })
	case "set-option":
	default:
		f.t.Fatalf("fakeServer: unexpected command %v", args)
	}
	return nil
}

// TestNoDestructiveCallReachesAPrefixSibling pins "the daemon never touches a
// local tmux session it does not own" for every path that resets, tombstones
// or kills the mirror session. Once that session is gone its bare name reaches
// a prefix sibling, and after a server restart its $N may name someone else;
// either way the sibling must come through untouched.
func TestNoDestructiveCallReachesAPrefixSibling(t *testing.T) {
	gone := exitError(t, 1)
	for _, tc := range []struct {
		name  string
		after func(*fakeServer)
	}{
		{"mirror session gone, prefix sibling alive", func(f *fakeServer) {
			f.run("kill-session", "-t", "$1")
		}},
		{"server restarted, $1 reused by the sibling", func(f *fakeServer) {
			f.pid = "200"
			f.sessions = []*fakeSession{{id: "$1", name: "h-api-v2", windows: []string{"@5"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeServer{t: t, pid: "100", gone: gone, nextWin: 9, sessions: []*fakeSession{
				{id: "$1", name: "h-api", windows: []string{"@1"}},
				{id: "$2", name: "h-api-v2", windows: []string{"@5"}},
			}}
			cfg := Config{
				LocalSess:     "h-api",
				RemoteHost:    "host",
				RemoteSession: "sess",
				SockPath:      filepath.Join(t.TempDir(), "sock"),
				LocalTmuxOut:  f.out,
				LocalTmux:     f.run,
			}
			pin, ok := pinLocalSession(cfg)
			if !ok || pin != testPin {
				t.Fatalf("pinLocalSession(...) = (%+v, %v), want (%+v, true)", pin, ok, testPin)
			}
			cfg.local = pin
			tc.after(f)
			f.mutated = nil

			if _, ok := resetMirrorSession(cfg, "sleep", "2147483647"); ok {
				t.Error("resetMirrorSession reported a reset of a session that is not its own")
			}
			tombstoneMirror(cfg, tombstoneText(cfg.RemoteHost, cfg.RemoteSession))
			endReopen(cfg, reopenFailedText(cfg.RemoteHost, cfg.RemoteSession))
			endReopen(cfg, "")

			if len(f.mutated) != 0 {
				t.Errorf("mutations reached a session that is not the mirror's: %v", f.mutated)
			}
			sib := f.resolve("=h-api-v2")
			if sib == nil || !slices.Equal(sib.windows, []string{"@5"}) {
				t.Errorf("sibling h-api-v2 = %+v, want it standing with only @5", sib)
			}
		})
	}
}
