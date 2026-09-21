package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTmux returns an injectable TmuxRunner that records every call's args
// (joined by spaces) instead of forking a real tmux, so these tests never
// touch a live server.
func fakeTmux() (TmuxRunner, *[]string) {
	var calls []string
	return func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}, &calls
}

func newTestWriter(dir, paneID string) (*Writer, *[]string) {
	w := New(dir, paneID)
	tmux, calls := fakeTmux()
	w.tmux = tmux
	return w, calls
}

func TestWritesOnChangeOnly(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWriter(dir, "3")
	now := time.Unix(1000, 0)

	changed, err := w.Update("processing", nil, now)
	if err != nil || !changed {
		t.Fatalf("first update: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "3"))
	if string(b) != "state=processing\ntimestamp=1000\n" {
		t.Fatalf("file = %q", b)
	}

	changed, _ = w.Update("processing", nil, now.Add(time.Second))
	if changed {
		t.Fatal("same state should not rewrite")
	}

	changed, _ = w.Update("idle", nil, now.Add(2*time.Second))
	if !changed {
		t.Fatal("state change should rewrite")
	}
}

func TestEmptyStateClearsFile(t *testing.T) {
	dir := t.TempDir()
	w, calls := newTestWriter(dir, "3")
	now := time.Unix(1000, 0)

	if changed, err := w.Update("processing", nil, now); err != nil || !changed {
		t.Fatalf("seed write: changed=%v err=%v", changed, err)
	}

	changed, err := w.Update("", nil, now.Add(time.Second))
	if err != nil {
		t.Fatalf("empty-state update: %v", err)
	}
	if !changed {
		t.Fatal("agent going away should be reported as a change")
	}
	if _, err := os.Stat(filepath.Join(dir, "3")); !os.IsNotExist(err) {
		t.Fatal("state file should be removed once the agent is gone")
	}
	if got := (*calls)[len(*calls)-1]; got != "set-option -p -q -u -t %3 @agent_screen" {
		t.Fatalf("last tmux call = %q, want the option unset", got)
	}
}

func TestUpdateAfterClearRewritesSameState(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWriter(dir, "3")
	now := time.Unix(1000, 0)

	if _, err := w.Update("processing", nil, now); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if _, err := w.Update("", nil, now.Add(time.Second)); err != nil {
		t.Fatalf("clear via empty state: %v", err)
	}

	// Same state as before the clear must still write — if Clear left
	// w.last set, this would look like a no-op change and the file would
	// stay missing.
	changed, err := w.Update("processing", nil, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("post-clear update: %v", err)
	}
	if !changed {
		t.Fatal("a real state after a clear must write, even if it repeats the pre-clear state")
	}
	b, err := os.ReadFile(filepath.Join(dir, "3"))
	if err != nil {
		t.Fatalf("state file should exist after post-clear update: %v", err)
	}
	if string(b) != "state=processing\ntimestamp=1002\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestClearWithNoFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	w, calls := newTestWriter(dir, "3")

	changed, err := w.Clear()
	if err != nil {
		t.Fatalf("clearing a writer that never wrote should not error: %v", err)
	}
	if changed {
		t.Fatal("clearing a writer that never wrote should report no change")
	}
	if _, err := os.Stat(filepath.Join(dir, "3")); !os.IsNotExist(err) {
		t.Fatal("no file should exist")
	}

	// Clearing again, and clearing via an empty Update, must also stay quiet.
	if changed, err := w.Clear(); err != nil || changed {
		t.Fatalf("second clear: changed=%v err=%v", changed, err)
	}
	if changed, err := w.Update("", nil, time.Unix(1, 0)); err != nil || changed {
		t.Fatalf("empty update on a never-written writer: changed=%v err=%v", changed, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("a no-op clear must not touch tmux, got %v", *calls)
	}
}

func TestClearRemovesFileWrittenByAnotherWriter(t *testing.T) {
	dir := t.TempDir()

	// Simulate a file left behind by a different Writer instance — e.g. a
	// prior watcher process for the same pane — that this Writer's own
	// w.last knows nothing about.
	path := filepath.Join(dir, "3")
	if err := os.WriteFile(path, []byte("state=processing\ntimestamp=1000\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	w, calls := newTestWriter(dir, "3")
	changed, err := w.Clear()
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !changed {
		t.Fatal("clearing a file this Writer didn't itself write should still report a change")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file left by another Writer instance should be removed")
	}
	if len(*calls) != 1 || (*calls)[0] != "set-option -p -q -u -t %3 @agent_screen" {
		t.Fatalf("calls = %v, want a single option unset", *calls)
	}
}

// A background shell starting or finishing moves no state, so deduping on the
// state alone would pin the first count forever.
func TestUpdateWritesOnFlagChangeAlone(t *testing.T) {
	dir := t.TempDir()
	w, calls := newTestWriter(dir, "%1")
	now := time.Unix(1000, 0)

	if changed, err := w.Update("idle", map[string]int{"bg": 1}, now); err != nil || !changed {
		t.Fatalf("first Update = (%v,%v), want (true,nil)", changed, err)
	}
	if changed, err := w.Update("idle", map[string]int{"bg": 2}, now.Add(time.Second)); err != nil || !changed {
		t.Fatalf("count change should rewrite, got (%v,%v)", changed, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "%1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "bg=2\n") {
		t.Fatalf("file = %q, want bg=2", data)
	}
	if got := (*calls)[len(*calls)-1]; !strings.HasSuffix(got, "@agent_screen idle 1001 bg=2") {
		t.Fatalf("last tmux call = %q, want the option carrying bg=2", got)
	}
	if changed, _ := w.Update("idle", map[string]int{"bg": 2}, now.Add(2*time.Second)); changed {
		t.Fatal("identical state+flags should not rewrite")
	}
	if changed, _ := w.Update("idle", nil, now.Add(3*time.Second)); !changed {
		t.Fatal("dropping the last flag should rewrite")
	}
}

// TestUpdateStampsPaneOption verifies a real state change stamps
// @agent_screen on the watched pane, distinct from @claude_status, so the
// remote-bridge daemon can carry it across without confusing it for
// hook-driven state.
func TestUpdateStampsPaneOption(t *testing.T) {
	dir := t.TempDir()
	w, calls := newTestWriter(dir, "7")
	now := time.Unix(2000, 0)

	if _, err := w.Update("processing", nil, now); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v, want exactly one stamp", *calls)
	}
	if got, want := (*calls)[0], "set-option -p -q -t %7 @agent_screen processing 2000"; got != want {
		t.Fatalf("stamp call = %q, want %q", got, want)
	}
}

func TestWithServerStampsFileBetweenTimestampAndFlags(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWriter(dir, "3")
	w.WithServer("4242")

	if _, err := w.Update("idle", map[string]int{"bg": 1}, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "3"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "state=idle\ntimestamp=1000\nserver=4242\nbg=1\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

func TestUnstampedWriterOutputUnchanged(t *testing.T) {
	dir := t.TempDir()
	w, _ := newTestWriter(dir, "3")

	if _, err := w.Update("idle", map[string]int{"bg": 1}, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "3"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "state=idle\ntimestamp=1000\nbg=1\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}
