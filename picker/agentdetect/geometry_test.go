package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundStep is one scripted result of a fake geometryRound. When printed is
// set the round reports g before returning, as runGeometryRound does for a
// first line that parses as a geometry.
type roundStep struct {
	g       geom
	printed bool
	err     error
}

var errRejected = errors.New("exit status 1")

// scriptedRound replays steps in order. Once the script is exhausted it
// cancels the watcher's context and returns a neutral result, so a test
// observes "no fallback" by watchGeometry returning false on that cancel.
type scriptedRound struct {
	mu     sync.Mutex
	steps  []roundStep
	calls  int
	cancel context.CancelFunc
}

func (s *scriptedRound) round(_ context.Context, report func(geom)) (bool, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()

	if i >= len(s.steps) {
		s.cancel()
		return false, nil
	}
	step := s.steps[i]
	if step.printed {
		report(step.g)
	}
	return step.printed, step.err
}

func (s *scriptedRound) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// runScripted runs watchGeometry over steps with a buffered out, so the
// sends never block, and returns what it returned, what it sent and how many
// rounds ran.
func runScripted(t *testing.T, steps []roundStep) (rejected bool, got []geom, calls int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &scriptedRound{steps: steps, cancel: cancel}
	out := make(chan geom, 16)

	done := make(chan bool, 1)
	go func() { done <- watchGeometry(ctx, fake.round, out, time.Millisecond) }()
	select {
	case rejected = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchGeometry did not return")
	}
	for len(out) > 0 {
		got = append(got, <-out)
	}
	return rejected, got, fake.callCount()
}

func TestWatchGeometryRejectsAfterLimit(t *testing.T) {
	rejected, got, calls := runScripted(t, []roundStep{
		{err: errRejected}, {err: errRejected}, {err: errRejected},
		{err: errRejected}, // never reached
	})
	if !rejected {
		t.Error("rejected = false after geometryRejectLimit consecutive failed rounds")
	}
	if calls != geometryRejectLimit {
		t.Errorf("rounds = %d, want %d", calls, geometryRejectLimit)
	}
	if len(got) != 0 {
		t.Errorf("sent %v, want nothing", got)
	}
}

func TestWatchGeometryPrintedRoundResetsRejectCount(t *testing.T) {
	g1, g2 := geom{100, 30}, geom{90, 20}
	rejected, got, _ := runScripted(t, []roundStep{
		{err: errRejected}, {err: errRejected},
		{g: g1, printed: true},
		{err: errRejected}, {err: errRejected},
		{g: g2, printed: true},
	})
	if rejected {
		t.Error("rejected = true although a printing round came between the failures")
	}
	if want := []geom{g1, g2}; !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestWatchGeometryPrintedRoundWithErrorResetsCount(t *testing.T) {
	// A pane that vanished mid-wait prints a geometry and then exits non-zero.
	rejected, got, calls := runScripted(t, []roundStep{
		{err: errRejected}, {err: errRejected},
		{g: geom{100, 30}, printed: true, err: errRejected},
		{err: errRejected}, {err: errRejected},
		{g: geom{90, 20}, printed: true, err: errRejected},
	})
	if rejected {
		t.Error("rejected = true although each failure pair was followed by a printing round")
	}
	if len(got) != 2 {
		t.Errorf("sent %v, want two geometries", got)
	}
	if calls <= 6 {
		t.Errorf("rounds = %d, want the loop to continue past the scripted rounds", calls)
	}
}

func TestWatchGeometryNonGeometryWakeNeitherCountsNorResets(t *testing.T) {
	// err == nil without a printed geometry: the wait fired but the display
	// line was not a geometry. It must not mask a rejecting server.
	rejected, _, calls := runScripted(t, []roundStep{
		{err: errRejected}, {err: errRejected},
		{}, // neither counts nor resets
		{err: errRejected},
	})
	if !rejected {
		t.Error("rejected = false: a non-geometry wake reset the count")
	}
	if calls != 4 {
		t.Errorf("rounds = %d, want 4: a non-geometry wake counted as a rejection", calls)
	}
}

func TestWatchGeometryDeliversReportsInOrder(t *testing.T) {
	want := []geom{{100, 30}, {80, 24}, {80, 24}, {120, 40}}
	steps := make([]roundStep, len(want))
	for i, g := range want {
		steps[i] = roundStep{g: g, printed: true}
	}
	rejected, got, _ := runScripted(t, steps)
	if rejected {
		t.Error("rejected = true for rounds that all printed")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestWatchGeometryCancelWhileBlockedOnOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	round := func(_ context.Context, report func(geom)) (bool, error) {
		close(entered)
		report(geom{100, 30}) // nobody reads out, so this blocks until cancel
		return true, nil
	}
	out := make(chan geom)

	done := make(chan bool, 1)
	go func() { done <- watchGeometry(ctx, round, out, time.Millisecond) }()
	<-entered
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case rejected := <-done:
		if rejected {
			t.Error("rejected = true on cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("watchGeometry still blocked on out after cancel")
	}
}

func TestWatchGeometryCancelInsideRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	round := func(ctx context.Context, _ func(geom)) (bool, error) {
		close(entered)
		<-ctx.Done()
		return false, ctx.Err()
	}

	done := make(chan bool, 1)
	go func() { done <- watchGeometry(ctx, round, make(chan geom), time.Millisecond) }()
	<-entered
	cancel()
	select {
	case rejected := <-done:
		if rejected {
			t.Error("rejected = true on cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("watchGeometry still inside a round after cancel")
	}
}

func TestGeometryWaitArgs(t *testing.T) {
	cases := []struct {
		name   string
		paneID string
		want   []string
	}{
		{
			"single digit",
			"3",
			[]string{"display", "-p", "-t", "%3", "#{pane_width} #{pane_height}", ";", "wait-for", "-E", "-F", "#{==:#{pane},%3}", "pane-resized"},
		},
		{
			"two digits",
			"12",
			[]string{"display", "-p", "-t", "%12", "#{pane_width} #{pane_height}", ";", "wait-for", "-E", "-F", "#{==:#{pane},%12}", "pane-resized"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := geometryWaitArgs(tc.paneID); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("geometryWaitArgs(%q) =\n %q\nwant\n %q", tc.paneID, got, tc.want)
			}
		})
	}
}

// waiterLines lists the clients blocked in `wait-for -E pane-resized`.
func waiterLines(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("tmux", "wait-for", "-l", "-E", "pane-resized").Output()
	if err != nil {
		t.Fatalf("wait-for -l -E pane-resized: %v", err)
	}
	var lines []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestGeometryWaitFiresWithoutClients is the client-less proof: pane-resized
// fires on resize-window with no client ever attached, and the waiter is a
// command client, not an attached one, that goes away with its context.
func TestGeometryWaitFiresWithoutClients(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux not on PATH")
	}
	// Short path, not t.TempDir(): <dir>/tmux-<uid>/default must fit the
	// ~104-byte AF_UNIX limit on darwin.
	tmpdir, err := os.MkdirTemp("/tmp", "og")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) }) //nolint:errcheck // best-effort scratch cleanup
	t.Setenv("TMUX_TMPDIR", tmpdir)
	t.Setenv("CLAUDE_STATUS_DIR", t.TempDir())
	// Inside a pane TMUX would point every bare tmux call at the live server.
	t.Setenv("TMUX", "")
	os.Unsetenv("TMUX") //nolint:errcheck // t.Setenv restores it on cleanup

	if out, err := exec.Command("tmux", "-f", "/dev/null", "new-session", "-d", "-x", "80", "-y", "24", "sleep 600").CombinedOutput(); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatalf("could not start a scratch tmux server (OG_REQUIRE_TMUX set): %v\n%s", err, out)
		}
		t.Skipf("could not start a scratch tmux server: %v", err)
	}
	// Safe: TMUX_TMPDIR is the private dir above and TMUX is unset.
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() }) //nolint:errcheck // best-effort scratch-server cleanup

	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id} #{window_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("list-panes = %q, want one pane", out)
	}
	paneID, winID := strings.TrimPrefix(fields[0], "%"), fields[1]

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	geomC := make(chan geom)
	go watchGeometry(ctx, runGeometryRound(paneID), geomC, 50*time.Millisecond)

	expect := func(want geom, within time.Duration) {
		t.Helper()
		select {
		case got := <-geomC:
			if got != want {
				t.Fatalf("geometry = %v, want %v", got, want)
			}
		case <-time.After(within):
			t.Fatalf("no geometry %v within %v", want, within)
		}
	}
	noClients := func() {
		t.Helper()
		out, err := exec.Command("tmux", "list-clients").Output()
		if err != nil {
			t.Fatalf("list-clients: %v", err)
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			t.Fatalf("list-clients = %q, want none: the waiter must not be an attached client", s)
		}
	}

	// The pane's own size, not the window's: a tmux on PATH that is the
	// tmux-og wrapper loads its config despite -f /dev/null, and its pane
	// border line takes a row.
	paneSize := func() geom {
		t.Helper()
		c, r, _, ok := paneInfo(paneID)
		if !ok {
			t.Fatal("paneInfo: no geometry")
		}
		return geom{c, r}
	}

	before := paneSize()
	expect(before, 5*time.Second)

	if out, err := exec.Command("tmux", "resize-window", "-t", winID, "-x", "100", "-y", "30").CombinedOutput(); err != nil {
		t.Fatalf("resize-window: %v\n%s", err, out)
	}
	after := paneSize()
	if after == before {
		t.Fatalf("resize-window left the pane at %v", after)
	}
	expect(after, 2*time.Second)

	// The round that read 100x30 re-registers after its settle delay.
	var lines []string
	if !eventually(2*time.Second, func() bool { lines = waiterLines(t); return len(lines) == 1 }) {
		t.Fatalf("pane-resized waiters = %q, want exactly one", lines)
	}
	noClients()

	cancel()
	if !eventually(2*time.Second, func() bool { lines = waiterLines(t); return len(lines) == 0 }) {
		t.Fatalf("pane-resized waiters after cancel = %q, want none", lines)
	}
	noClients()
}
