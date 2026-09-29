package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePaneStateFile writes a panes/ or screen/ style state file. extra is
// appended verbatim (e.g. "session=s\nunseen=1\n" for a hook file).
func writePaneStateFile(t *testing.T, dir, id, state string, timestamp int64, extra string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("state=%s\ntimestamp=%d\n%s", state, timestamp, extra)
	if err := os.WriteFile(filepath.Join(dir, id), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCollectAgentPanesMergesHookAndScreen covers the three cases #342 asks
// for directly: a hook-written (Claude) pane, a scraper-only (codex/cursor)
// pane, and a pane with neither file (no agent at all).
func TestCollectAgentPanesMergesHookAndScreen(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "panes")
	screenDir := filepath.Join(root, "screen")
	issuesDir := filepath.Join(root, "issues")

	// "1": hook-written Claude pane.
	writePaneStateFile(t, hookDir, "1", "processing", 1000, "session=claude-sess\n")
	// "2": scraper-only pane (codex/cursor) — screen/ has no session field,
	// so it can only be placed via the live pane map.
	writePaneStateFile(t, screenDir, "2", "processing", 1000, "")
	// "3": a live pane running neither an agent nor a scraper — no file in
	// either directory, so it must never appear in the result.

	paneMap := map[string]paneMapping{
		"1": {session: "claude-sess", winIdx: 0},
		"2": {session: "codex-sess", winIdx: 1},
		"3": {session: "shell-sess", winIdx: 2},
	}

	panes := collectAgentPanesFrom(hookDir, screenDir, issuesDir, paneMap, 1000)

	byPaneSession := map[string]agentPaneInfo{}
	for _, p := range panes {
		byPaneSession[p.session] = p
	}

	if len(panes) != 2 {
		t.Fatalf("collectAgentPanesFrom() returned %d panes, want 2 (got %+v)", len(panes), panes)
	}
	hookPane, ok := byPaneSession["claude-sess"]
	if !ok || hookPane.state != "processing" {
		t.Errorf("hook-written pane missing or wrong state: %+v", byPaneSession)
	}
	screenPane, ok := byPaneSession["codex-sess"]
	if !ok || screenPane.state != "processing" || screenPane.winIdx != 1 {
		t.Errorf("scraper-only pane missing, wrong state, or wrong window: %+v", byPaneSession)
	}
	if _, ok := byPaneSession["shell-sess"]; ok {
		t.Errorf("non-agent pane must not appear in result: %+v", byPaneSession)
	}
}

// TestCollectAgentPanesHookFirstPrecedence pins the hook-first precedence
// read_pane_state implements in scripts/lib-claude.sh: a waiting/error/denied
// hook state is never overridden by a fresher screen reading, no matter its
// age — those states look identical to idle on screen and would be wrongly
// downgraded. A "most recent timestamp wins" or "screen always wins when
// present" implementation would flip this to "idle" and fail here.
func TestCollectAgentPanesHookFirstPrecedence(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "panes")
	screenDir := filepath.Join(root, "screen")
	issuesDir := filepath.Join(root, "issues")

	writePaneStateFile(t, hookDir, "1", "waiting", 1000, "session=s\n")
	writePaneStateFile(t, screenDir, "1", "idle", 999999, "")

	paneMap := map[string]paneMapping{"1": {session: "s", winIdx: 0}}

	// now is far past every staleness threshold; waiting still must win.
	panes := collectAgentPanesFrom(hookDir, screenDir, issuesDir, paneMap, 999999)

	if len(panes) != 1 || panes[0].state != "waiting" {
		t.Fatalf("collectAgentPanesFrom() = %+v, want one pane with state=waiting (hook-first, protected state)", panes)
	}
}

// TestCollectAgentPanesScreenOverridesStaleHook covers the other half of the
// same precedence rule: compacting/processing/done CAN be corrected by a live
// screen reading once the hook state is stale past its own threshold — the
// case where a completion hook was missed.
func TestCollectAgentPanesScreenOverridesStaleHook(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "panes")
	screenDir := filepath.Join(root, "screen")
	issuesDir := filepath.Join(root, "issues")

	// processing stale threshold is 300s; hook written at t=0, screen fresher.
	writePaneStateFile(t, hookDir, "1", "processing", 0, "session=s\nunseen=1\n")
	writePaneStateFile(t, screenDir, "1", "idle", 400, "")

	paneMap := map[string]paneMapping{"1": {session: "s", winIdx: 0}}

	panes := collectAgentPanesFrom(hookDir, screenDir, issuesDir, paneMap, 400)

	if len(panes) != 1 {
		t.Fatalf("collectAgentPanesFrom() returned %d panes, want 1", len(panes))
	}
	if panes[0].state != "idle" {
		t.Errorf("stale processing hook with a live screen reading = %q, want idle (screen overrides)", panes[0].state)
	}
	if panes[0].unseen {
		t.Error("unseen must clear when the screen reading overrides a stale hook state")
	}
}

// TestCollectAgentPanesOverrideRefreshesSessionFromPaneMap: when a stale hook
// is overridden by a live screen reading, the pane's session is re-resolved
// from the live pane map rather than trusted from the (possibly outdated)
// hook write — so the pane still surfaces under its real, current session
// instead of being silently dropped or misplaced.
func TestCollectAgentPanesOverrideRefreshesSessionFromPaneMap(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "panes")
	screenDir := filepath.Join(root, "screen")
	issuesDir := filepath.Join(root, "issues")

	writePaneStateFile(t, hookDir, "1", "processing", 0, "session=stale-sess\n")
	writePaneStateFile(t, screenDir, "1", "idle", 400, "")

	paneMap := map[string]paneMapping{"1": {session: "live-sess", winIdx: 2}}

	panes := collectAgentPanesFrom(hookDir, screenDir, issuesDir, paneMap, 400)

	if len(panes) != 1 {
		t.Fatalf("collectAgentPanesFrom() returned %d panes, want 1", len(panes))
	}
	if panes[0].session != "live-sess" || panes[0].winIdx != 2 {
		t.Errorf("collectAgentPanesFrom() session/winIdx = %q/%d, want live-sess/2 (from the pane map, not the stale hook)",
			panes[0].session, panes[0].winIdx)
	}
}

// A fresh hook state wins the state but carries no background-shell count —
// only the scraper sees those — so the badge must come off the screen file
// even when the hook governs, and must survive the pane falling back to idle.
func TestCollectAgentPanesTakesBGFromScreenUnderFreshHook(t *testing.T) {
	root := t.TempDir()
	hookDir := filepath.Join(root, "panes")
	screenDir := filepath.Join(root, "screen")
	issuesDir := filepath.Join(root, "issues")

	writePaneStateFile(t, hookDir, "1", "waiting", 1000, "session=claude-sess\n")
	writePaneStateFile(t, screenDir, "1", "idle", 1000, "bg=3\n")

	paneMap := map[string]paneMapping{"1": {session: "claude-sess", winIdx: 0}}
	panes := collectAgentPanesFrom(hookDir, screenDir, issuesDir, paneMap, 1000)

	if len(panes) != 1 {
		t.Fatalf("got %d panes, want 1: %+v", len(panes), panes)
	}
	if panes[0].state != "waiting" {
		t.Errorf("state = %q, want waiting (fresh hook governs)", panes[0].state)
	}
	if panes[0].bg != 3 {
		t.Errorf("bg = %d, want 3 (from the screen file)", panes[0].bg)
	}
}

// TestCollectAgentPanesUntrustedDirReturnsNil covers the #850 gate at the
// collectAgentPanes level: a loose CLAUDE_STATUS_DIR must yield no panes at
// all, even when it holds a state file that would otherwise match live panes.
func TestCollectAgentPanesUntrustedDirReturnsNil(t *testing.T) {
	root := t.TempDir() // t.TempDir() is 0755, i.e. not owner-only.
	writePaneStateFile(t, filepath.Join(root, "panes"), "1", "processing", 1000, "session=s\n")
	t.Setenv("CLAUDE_STATUS_DIR", root)

	snap := panesSnapshot{"%1|s|0||||||||"}
	if panes := collectAgentPanes(snap); panes != nil {
		t.Fatalf("collectAgentPanes() on an untrusted root = %+v, want nil", panes)
	}
}

// TestCollectAgentPanesTrustedDirReturnsPanes is the positive counterpart: a
// 0700 CLAUDE_STATUS_DIR is read normally.
func TestCollectAgentPanesTrustedDirReturnsPanes(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writePaneStateFile(t, filepath.Join(root, "panes"), "1", "processing", 1000, "session=s\n")
	t.Setenv("CLAUDE_STATUS_DIR", root)

	snap := panesSnapshot{"%1|s|0||||||||"}
	panes := collectAgentPanes(snap)
	if len(panes) != 1 || panes[0].state != "processing" {
		t.Fatalf("collectAgentPanes() on a trusted root = %+v, want one processing pane", panes)
	}
}

func TestAppendAgentIconBadgeIsAdditive(t *testing.T) {
	plain, plainDW := appendAgentIcon("", 0, agentCounts{idle: 1}, "dark", "", "")
	withBG, bgDW := appendAgentIcon("", 0, agentCounts{idle: 1, bg: 2}, "dark", "", "")

	if !strings.HasPrefix(withBG, plain) {
		t.Fatalf("badge should append to the state icon: %q vs %q", withBG, plain)
	}
	if !strings.Contains(withBG, claudeIconBG+"2") {
		t.Errorf("badge missing its count: %q", withBG)
	}
	if bgDW != plainDW+3 {
		t.Errorf("display width = %d, want %d (icon + 1 digit + space)", bgDW, plainDW+3)
	}
	// The state itself must be untouched — priority never sees bg.
	if got := agentPriority(agentCounts{idle: 1, bg: 2}); got != "idle" {
		t.Errorf("agentPriority = %q, want idle", got)
	}
}
