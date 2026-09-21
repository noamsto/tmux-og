package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/agentdetect/manifest"
	"github.com/noamsto/tmux-og/picker/agentdetect/screen"
	"github.com/noamsto/tmux-og/picker/agentdetect/statefile"
)

// newTestWriter is statefile.New with a no-op TmuxRunner, so these tests
// never fork a real tmux.
func newTestWriter(dir, paneID string) *statefile.Writer {
	return statefile.NewWithTmuxRunner(dir, paneID, func(...string) error { return nil })
}

func TestPaneInfoReportsOK(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		wantCols int
		wantRows int
		wantCmd  string
		wantOK   bool
	}{
		{"valid", "100 30 codex", 100, 30, "codex", true},
		{"empty", "", 0, 0, "", false},
		{"width only", "100", 0, 0, "", false},
		{"non-numeric", "abc def", 0, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cols, rows, cmd, ok := parsePaneInfo(c.out)
			if cols != c.wantCols || rows != c.wantRows || cmd != c.wantCmd || ok != c.wantOK {
				t.Errorf("parsePaneInfo(%q) = (%d,%d,%q,%v), want (%d,%d,%q,%v)",
					c.out, cols, rows, cmd, ok, c.wantCols, c.wantRows, c.wantCmd, c.wantOK)
			}
		})
	}
}

func TestSeedBytesConvertsLFToCRLF(t *testing.T) {
	got := seedBytes([]byte("line1\nline2\n"))
	want := []byte("line1\r\nline2\r\n")
	if string(got) != string(want) {
		t.Fatalf("seedBytes = %q, want %q", got, want)
	}
}

type fakeScreen struct {
	panicOnFeed bool
}

func (f *fakeScreen) Feed([]byte) {
	if f.panicOnFeed {
		panic("feed boom")
	}
}
func (f *fakeScreen) Text() string    { return "" }
func (f *fakeScreen) Title() string   { return "" }
func (f *fakeScreen) AltScreen() bool { return false }
func (f *fakeScreen) Close()          {}

func TestFeedSafeRecoversFromPanic(t *testing.T) {
	if feedSafe(&fakeScreen{panicOnFeed: true}, []byte("x")) {
		t.Fatal("feedSafe should return false when Feed panics")
	}
	if !feedSafe(&fakeScreen{}, []byte("x")) {
		t.Fatal("feedSafe should return true when Feed succeeds")
	}
}

// Real capture: ESC[1;40r + 38 reverse-indices from a live codex pane after resize.
func TestFeedSafeRecoversFromRealResizeBurst(t *testing.T) {
	data, err := os.ReadFile("testdata/codex_resize_scroll.txt")
	if err != nil {
		t.Fatal(err)
	}
	if feedSafe(screen.New(100, 30), data) {
		t.Fatal("feedSafe on 100x30 should return false for the resize-scroll burst")
	}
	if !feedSafe(screen.New(100, 40), data) {
		t.Fatal("feedSafe on 100x40 should return true for the same resize-scroll burst")
	}
}

func TestSeedMatchesIdleCodex(t *testing.T) {
	content, err := os.ReadFile("testdata/codex_idle_seed.txt")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := manifest.Load()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := manifest.ForCommand(ms, "codex")
	if !ok {
		t.Fatal("no manifest for codex")
	}

	scr := screen.New(100, 30)
	scr.Feed(seedBytes(content))
	got, _, ok := manifest.Match(m, scr.Text(), scr.Title(), scr.AltScreen())
	if !ok || got != "idle" {
		t.Fatalf("seedBytes feed Match = (%q,%v), want (idle,true)", got, ok)
	}

	raw := screen.New(100, 30)
	raw.Feed(content)
	got, _, ok = manifest.Match(m, raw.Text(), raw.Title(), raw.AltScreen())
	if ok && got == "idle" {
		t.Fatal("raw unconverted feed unexpectedly matched idle — seedBytes should be load-bearing")
	}
}

func TestAliveFromProbe(t *testing.T) {
	if !aliveFromProbe(nil) {
		t.Error("nil probe error should mean alive")
	}
	if aliveFromProbe(errors.New("can't find pane")) {
		t.Error("any probe error should mean gone, including an unreachable tmux server")
	}
}

// TestPaneAliveRealTmux exercises paneAlive's actual shell-out, not just the
// pure aliveFromProbe decision it wraps: a tmux subcommand whose target
// resolution silently tolerates a missing target would pass aliveFromProbe
// fine while never actually detecting a dead pane, so only invoking the real
// command catches that class of bug.
func TestPaneAliveRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	// Isolates this test's tmux server on its own default socket (no -L
	// needed on either side: tmux resolves the default socket under
	// TMUX_TMPDIR) so it can never reach the developer's real interactive
	// server or its panes, and paneAlive — which always shells out to plain
	// "tmux" — talks to the same isolated server without needing its own
	// socket flag.
	t.Setenv("TMUX_TMPDIR", t.TempDir())

	session := "agentdetect-test-" + strconv.Itoa(os.Getpid())
	if err := exec.Command("tmux", "new-session", "-d", "-s", session, "-x", "80", "-y", "24").Run(); err != nil {
		t.Skipf("could not start a scratch tmux session: %v", err)
	}
	defer exec.Command("tmux", "kill-session", "-t", session).Run()

	out, err := exec.Command("tmux", "list-panes", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	paneID := strings.TrimPrefix(strings.TrimSpace(string(out)), "%")

	if !paneAlive(paneID) {
		t.Fatalf("paneAlive(%q) = false for a live scratch pane", paneID)
	}

	if err := exec.Command("tmux", "kill-session", "-t", session).Run(); err != nil {
		t.Fatalf("kill-session: %v", err)
	}

	if paneAlive(paneID) {
		t.Fatalf("paneAlive(%q) = true for a pane whose session was just killed", paneID)
	}
}

func TestOwnerMatches(t *testing.T) {
	cases := []struct {
		name       string
		registered string
		pid        int
		want       bool
	}{
		{"exact match", "1234", 1234, true},
		{"trailing newline", "1234\n", 1234, true},
		{"different pid", "5678", 1234, false},
		{"empty registry", "", 1234, false},
		{"two-line file", "1234\nserver=99\n", 1234, true},
		{"two-line file, different pid", "5678\nserver=1234\n", 1234, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ownerMatches(c.registered, c.pid); got != c.want {
				t.Errorf("ownerMatches(%q, %d) = %v, want %v", c.registered, c.pid, got, c.want)
			}
		})
	}
}

func TestRegisterWatcherContent(t *testing.T) {
	cases := []struct {
		name, server, want string
	}{
		{"with server", "777", "100\nserver=777\n"},
		{"without server", "", "100"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if !registerWatcher(dir, "42", 100, c.server) {
				t.Fatal("registerWatcher failed")
			}
			got, err := os.ReadFile(filepath.Join(dir, "42"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("content = %q, want %q", got, c.want)
			}
			if !stillOwner(dir, "42", 100) {
				t.Error("registered pid should still be the owner")
			}
		})
	}
}

func TestRegisterWatcherAndStillOwner(t *testing.T) {
	dir := t.TempDir()

	if !registerWatcher(dir, "42", 100, "") {
		t.Fatal("registerWatcher should succeed against a writable temp dir")
	}
	if !stillOwner(dir, "42", 100) {
		t.Error("the pid that just registered should still be the owner")
	}
	if stillOwner(dir, "42", 999) {
		t.Error("a different pid must not read as the owner")
	}

	// A later registration (simulating a re-arm) supersedes the first —
	// this is the exact scenario that leaked in #239: old watcher still
	// running, new watcher starts for the same pane.
	if !registerWatcher(dir, "42", 200, "") {
		t.Fatal("re-registration should succeed")
	}
	if stillOwner(dir, "42", 100) {
		t.Error("the superseded pid must no longer be the owner")
	}
	if !stillOwner(dir, "42", 200) {
		t.Error("the new pid should be the owner after re-registration")
	}
}

func TestStillOwnerMissingRegistry(t *testing.T) {
	dir := t.TempDir()
	if stillOwner(dir, "no-such-pane", 100) {
		t.Error("a pane with no registry entry must not read as owned")
	}
}

// TestEmitIfOwnerSkipsWhenSuperseded reproduces the #326 interaction hazard:
// a superseded watcher reaching an exit path (EOF or the liveness backstop)
// must not clobber the state the new watcher already wrote for the same
// pane. Without the emitIfOwner guard, this fails because emit() overwrites
// the file unconditionally with the superseded watcher's stale screen
// content.
func TestEmitIfOwnerSkipsWhenSuperseded(t *testing.T) {
	regDir := t.TempDir()
	stateDir := t.TempDir()
	paneID := "42"

	m := manifest.Manifest{Rules: []manifest.Rule{
		{State: "busy", Priority: 1, Contains: []string{"OLD-SCREEN"}},
		{State: "done", Priority: 1, Contains: []string{"NEW-SCREEN"}},
	}}

	if !registerWatcher(regDir, paneID, 100, "") {
		t.Fatal("registerWatcher should succeed for the old watcher")
	}

	// Re-arm supersedes the old watcher before it exits — the exact race
	// #239/#324 target: old pid 100 is still running when pid 200 takes over.
	if !registerWatcher(regDir, paneID, 200, "") {
		t.Fatal("registerWatcher should succeed for the new watcher")
	}

	// The new watcher reports its own current state, as it does at startup.
	newScr := screen.New(80, 24)
	newScr.Feed([]byte("NEW-SCREEN"))
	emit(newScr, m, newTestWriter(stateDir, paneID))

	before, err := os.ReadFile(stateDir + "/" + paneID)
	if err != nil {
		t.Fatalf("reading state file written by the new watcher: %v", err)
	}

	// The superseded old watcher (pid 100) now reaches an exit path with a
	// different screen. It must not write, since it is no longer the owner.
	oldScr := screen.New(80, 24)
	oldScr.Feed([]byte("OLD-SCREEN"))
	emitIfOwner(regDir, paneID, 100, oldScr, m, newTestWriter(stateDir, paneID))

	after, err := os.ReadFile(stateDir + "/" + paneID)
	if err != nil {
		t.Fatalf("reading state file after superseded watcher's exit: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("superseded watcher clobbered the new watcher's state: before=%q after=%q", before, after)
	}
}

// TestEmitIfOwnerEmitsWhenStillOwner is the mirror case: an ordinary
// (non-superseded) exit must still emit its final snapshot.
func TestEmitIfOwnerEmitsWhenStillOwner(t *testing.T) {
	regDir := t.TempDir()
	stateDir := t.TempDir()
	paneID := "42"

	m := manifest.Manifest{Rules: []manifest.Rule{
		{State: "busy", Priority: 1, Contains: []string{"SCREEN"}},
	}}

	if !registerWatcher(regDir, paneID, 100, "") {
		t.Fatal("registerWatcher should succeed")
	}

	scr := screen.New(80, 24)
	scr.Feed([]byte("SCREEN"))
	emitIfOwner(regDir, paneID, 100, scr, m, newTestWriter(stateDir, paneID))

	content, err := os.ReadFile(stateDir + "/" + paneID)
	if err != nil {
		t.Fatalf("still-owner exit should have written a final snapshot: %v", err)
	}
	if !strings.Contains(string(content), "state=busy") {
		t.Errorf("state file = %q, want it to contain state=busy", content)
	}
}
