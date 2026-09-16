package daemon

import (
	"sync"
	"testing"
	"time"
)

// countingCfg answers every listing with rows — mirrorPaneListFormat lines,
// which is what the sweep reads — and counts the local tmux forks.
func countingCfg(rows string, n *int) Config {
	var mu sync.Mutex
	return Config{
		LocalSess: "host-sess",
		LocalTmux: func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			*n++
			return rows, nil
		},
	}
}

// The sweep rides the per-line maintenance block, so without the floor it forks
// a local tmux client for every line a redrawing pane emits.
func TestWindowSweeperFloorsRepeatedPasses(t *testing.T) {
	forks := 0
	cfg := countingCfg("@0|%0|0|%0\n@143|%1|0|%1\n", &forks)
	reg := newRegistry()
	reg.add("@1", "@143")

	var s windowSweeper
	for i := 0; i < 20; i++ {
		s.sweep(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())
	}
	if forks != 1 {
		t.Errorf("local tmux forks = %d over 20 back-to-back sweeps, want 1 (the floor admits one pass)", forks)
	}
	s.lastPass = time.Now().Add(-2 * windowSweepInterval)
	s.sweep(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())
	if forks != 2 {
		t.Errorf("local tmux forks = %d after the floor elapsed, want 2 (the sweep still runs)", forks)
	}
}

// force() is the death-nudge case's way of getting the very next sweep() to
// actually run: a bare wake with no force is silently swallowed by the
// windowSweepInterval floor, which would leave the event path no faster than
// the mainLoopTickInterval backstop it exists to shortcut. Calling sweep() a
// second time with no elapsed gap is floored (TestWindowSweeperFloorsRepeatedPasses
// pins that); force()ing first must make that same call run anyway.
func TestWindowSweeperForceBypassesTheFloor(t *testing.T) {
	forks := 0
	cfg := countingCfg("@0|%0|0|%0\n@143|%1|0|%1\n", &forks)
	reg := newRegistry()
	reg.add("@1", "@143")

	var s windowSweeper
	s.sweep(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())
	if forks != 1 {
		t.Fatalf("local tmux forks = %d after the first sweep, want 1", forks)
	}

	s.force()
	s.sweep(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())
	if forks != 2 {
		t.Errorf("local tmux forks = %d after force()+sweep() called well inside windowSweepInterval, want 2 — force() must bypass the floor", forks)
	}
}

// One listing answers for the whole registry AND for both passes: a fork per
// entry made the sweep scale with window count, and a fork per pass doubled it
// (#547). mirrorPaneRows is why both are one read.
func TestSweepListsOnceForEveryMirrorAndBothPasses(t *testing.T) {
	forks := 0
	cfg := countingCfg("@1|%0|0|%0\n@2|%1|0|%1\n@3|%2|0|%2\n", &forks)
	reg := newRegistry()
	reg.add("@10", "@1")
	reg.add("@11", "@2")
	reg.add("@12", "@3")

	var s windowSweeper
	s.sweep(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())

	if forks != 1 {
		t.Errorf("local tmux forks = %d for 3 mirror windows over both heal passes, want 1", forks)
	}
}
