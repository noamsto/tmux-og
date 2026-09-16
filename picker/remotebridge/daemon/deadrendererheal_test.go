package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// paneRowsCfg answers the sweep's listing with mirrorPaneListFormat rows.
func paneRowsCfg(rows ...string) Config {
	return Config{
		LocalSess:    "host-sess",
		LocalArea:    func() (int, int) { return 190, 45 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return strings.Join(rows, "\n") + "\n", nil },
	}
}

// healRig counts the rebuilds healDeadRenderers drives. setupWindow's first act
// is the aggressive-resize opt-out, unconditionally and before any round-trip,
// so a send of it is the earliest observable that resetWindow was entered —
// which matters because emptyRemote makes the rebuild fail at readLayout, well
// before it reaches a pane.
type healRig struct {
	mu       sync.Mutex
	rebuilds int
}

func (r *healRig) send(cmd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(cmd, "aggressive-resize off") {
		r.rebuilds++
	}
}

func (r *healRig) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rebuilds
}

func (r *healRig) heal(cfg Config, dead map[string]bool, reg *registry, s *windowSweeper) {
	s.healDeadRenderers(cfg, dead, r.send, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())
}

// #547: a mirror window can hold a float this daemon never created (prefix +
// b/k/I, and ^o's remote picker), remain-on-exit now keeps that float's corpse
// too, and it is not ours to reap — a rebuild drops the user's float. Hence
// the @bridge_pane gate rather than pane_dead alone. A corpse's
// pane_current_command still reads "renderer", so a command-name probe would
// be blind to the real case in the other direction.
func TestMirrorPaneRowsGatesOnTheDaemonsOwnStamp(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|0|%0", "@143|%9|1|", "@200|%1|1|%1")

	live, dead, ok := mirrorPaneRows(cfg)
	if !ok {
		t.Fatal("mirrorPaneRows reported the listing unreadable")
	}
	// A window always holds a pane, so the pane listing carries the window set.
	if !live["@143"] || !live["@200"] || len(live) != 2 {
		t.Errorf("live = %v, want both windows", live)
	}
	if dead["@143"] {
		t.Error("a dead pane the daemon never wired marked its window for rebuild; that drops the user's float")
	}
	if !dead["@200"] {
		t.Error("a dead renderer pane did not mark its window for rebuild")
	}
}

// Positive evidence only, the localWindowGone rule: a listing that cannot be
// made must leave every mirror alone rather than retire or rebuild on a
// transient read.
func TestMirrorPaneRowsReportsAnUnreadableListing(t *testing.T) {
	cfg := Config{
		LocalSess:    "host-sess",
		LocalTmuxOut: func(...string) (string, error) { return "", errors.New("no server running") },
	}
	if _, _, ok := mirrorPaneRows(cfg); ok {
		t.Error("mirrorPaneRows reported ok on a listing it could not read")
	}
}

// With remain-on-exit on the mirror window, a renderer crash/exit leaves a dead
// pane instead of closing the window (and, for a single-pane mirror, the
// session). A user Respawn reconnects via argv and does not produce this
// corpse. Nothing else can find a crash corpse — a dead pane is still a pane to
// list-panes, so the pane diff reads the local set as matching the remote's —
// so this pass is what notices.
func TestHealDeadRenderersRebuildsAMirrorHoldingACorpse(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|1|%0")
	reg := newRegistry()
	reg.add("@1", "@143")
	rig := &healRig{}

	rig.heal(cfg, map[string]bool{"@143": true}, reg, &windowSweeper{})

	if rig.count() != 1 {
		t.Errorf("rebuilds = %d, want 1 — the corpse otherwise stays for the life of the session", rig.count())
	}
	// resetWindow, not retireMirror: the window is live, and closeWindow's
	// kill-window on a single-window mirror session takes the session — the
	// very failure this repairs.
	if _, ok := reg.byRemoteID("@1"); !ok {
		t.Error("the registry entry went away; the mirror window was retired rather than reset")
	}
}

// The heal runs every sweep, so a false positive rebuilds a healthy mirror on
// a loop.
func TestHealDeadRenderersLeavesLiveMirrorsAlone(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|0|%0", "@143|%1|0|%1")
	reg := newRegistry()
	reg.add("@1", "@143")
	rig := &healRig{}

	rig.heal(cfg, map[string]bool{}, reg, &windowSweeper{})

	if rig.count() != 0 {
		t.Errorf("rebuilds = %d, want 0 — every renderer is alive", rig.count())
	}
}

// A renderer that dies at spawn every time — its binary gone from the store
// under a live session — would otherwise be rebuilt once per sweep for the
// life of the daemon.
func TestHealDeadRenderersStopsRebuildingAWindowThatKeepsDying(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|1|%0")
	reg := newRegistry()
	reg.add("@1", "@143")
	rig := &healRig{}
	s := &windowSweeper{}

	for i := 0; i < deadRendererStrikes+3; i++ {
		rig.heal(cfg, map[string]bool{"@143": true}, reg, s)
	}

	if rig.count() != deadRendererStrikes {
		t.Errorf("rebuilds = %d, want %d — the cap has to stop a repeating failure rebuilding the window every sweep",
			rig.count(), deadRendererStrikes)
	}
}

// The cap rations a repeating failure, not repairs over a session in which
// renderers die once and come back: a healthy pass returns the budget.
func TestHealDeadRenderersReturnsTheBudgetAfterAHealthyPass(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|1|%0")
	reg := newRegistry()
	reg.add("@1", "@143")
	rig := &healRig{}
	s := &windowSweeper{}

	for i := 0; i < deadRendererStrikes; i++ {
		rig.heal(cfg, map[string]bool{"@143": true}, reg, s)
	}
	// The healthy pass below must land outside deadRendererRecovery of the
	// last rebuild, or it reads as that rebuild's own died() echo rather than
	// the unrelated later recovery this test means to exercise.
	s.lastRebuild["@1"] = time.Now().Add(-deadRendererRecovery - time.Second)
	rig.heal(cfg, map[string]bool{}, reg, s)             // healthy: the strike is forgotten
	rig.heal(cfg, map[string]bool{"@143": true}, reg, s) // a fresh death is repaired, not refused

	if rig.count() != deadRendererStrikes+1 {
		t.Errorf("rebuilds = %d, want %d — a healthy pass has to return the budget",
			rig.count(), deadRendererStrikes+1)
	}
}

// TestHealDeadRenderersStaysCappedUnderEventDrivenCrashLoop pins the strike
// cap under the event path: a rebuild's own resetWindow closes the
// superseded conns, which fires died() — so the very next pass after a
// rebuild looks exactly like a healthy pass, with no wall-clock gap to tell
// "the rebuild's own echo" apart from "an unrelated later recovery". Unlike
// TestHealDeadRenderersReturnsTheBudgetAfterAHealthyPass, this never advances
// s.lastRebuild past deadRendererRecovery — every healthy pass here lands
// immediately after its preceding rebuild, the way the echo actually would.
//
// Without deadRendererRecovery gating the healthy branch, each of these
// echoes would delete the strike outright and a crash-looping renderer would
// be rebuilt forever instead of capping at deadRendererStrikes.
func TestHealDeadRenderersStaysCappedUnderEventDrivenCrashLoop(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|1|%0")
	reg := newRegistry()
	reg.add("@1", "@143")
	rig := &healRig{}
	s := &windowSweeper{}

	for i := 0; i < deadRendererStrikes+3; i++ {
		rig.heal(cfg, map[string]bool{"@143": true}, reg, s) // dead pass: may rebuild
		rig.heal(cfg, map[string]bool{}, reg, s)             // its own died() echo, no time gap
	}

	if rig.count() != deadRendererStrikes {
		t.Errorf("rebuilds = %d, want %d — an interleaved healthy pass inside deadRendererRecovery must not return the strike budget, or a crash-looping renderer rebuilds forever under the event path",
			rig.count(), deadRendererStrikes)
	}
}

// TestHealDeadRenderersStampsLastRebuildAfterResetWindowFinishes pins the
// stamp ordering: lastRebuild must be set AFTER resetWindow returns, not
// before, or a rebuild slower than deadRendererRecovery blows through the
// recovery window on its own duration alone — no echo needed — since the
// clock would have already started before the slow work even began. Drives a
// resetWindow slow enough (via a send hook that sleeps on setupWindow's
// first, unconditional call) to prove the stamp waits for it to finish rather
// than racing ahead of it.
func TestHealDeadRenderersStampsLastRebuildAfterResetWindowFinishes(t *testing.T) {
	cfg := paneRowsCfg("@143|%0|1|%0")
	reg := newRegistry()
	reg.add("@1", "@143")

	const delay = 50 * time.Millisecond
	var once sync.Once
	send := func(cmd string) {
		if strings.Contains(cmd, "aggressive-resize off") {
			once.Do(func() { time.Sleep(delay) })
		}
	}

	s := &windowSweeper{}
	before := time.Now()
	s.healDeadRenderers(cfg, map[string]bool{"@143": true}, send, NewRouter(), noHellos, newCtlState(), reg, newConverger(), emptyRemote())

	stamp := s.lastRebuild["@1"]
	if stamp.IsZero() {
		t.Fatal("lastRebuild was never stamped")
	}
	if stamp.Before(before.Add(delay)) {
		t.Errorf("lastRebuild stamped at %v, want at or after %v (resetWindow's own delay) — "+
			"the stamp must follow resetWindow's completion, not precede it, or a slow rebuild's own "+
			"duration alone can blow through deadRendererRecovery before any echo is even involved",
			stamp, before.Add(delay))
	}
}
