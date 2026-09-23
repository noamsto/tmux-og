# Statusline session segment includes screen-scraped agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `tmux-statusline`'s session agent segment count screen-scraped panes (`screen/<id>`) the same way `collectAgentPanesFrom` does, so pi/codex/cursor (and bridge-shipped mirror screens) show up on line 0.

**Architecture:** Keep aggregation in `picker/statusline/claude.go`. `aggregateSession` unions `panes/` and `screen/`, applies hook-first merge with the same stale-active screen override as `read_pane_state` / `collectAgentPanesFrom`, and attributes screen-only (and screen-override) panes via an injected live pane-id set from `tmux list-panes -s`. Hook-only panes still key off `session=`. Tests inject the map so they never talk to a live tmux.

**Tech Stack:** Go (`picker/statusline`), existing `os.ReadDir`/`ReadFile` parse, `tmux list-panes -s` with a 2s `CommandContext` timeout matching `fetchVolatile`.

## Global Constraints

- Do not change `counts.priorityState()` order (must stay byte-identical to `claude_priority_state` / `agentPriority`).
- Do not touch `picker/statusline/usage.go` or anything under `picker/remotebridge/**`.
- Do not add a shared package or import `picker`'s `collectAgentPanesFrom` (statusline is a separate `package main`).
- Tests must not call the live tmux server: inject `liveIDs`; production `main` is the only caller of `listSessionPaneIDs`.
- `|`-delimited tmux formats stay `|`-delimited; this change uses a single `#{pane_id}` field.

## Evidence / consumer map

Violated invariant: a pane whose only state is `screen/<id>` must contribute to the session segment for the session that currently owns that pane.

| Stage | Symbol | Disposition |
| --- | --- | --- |
| Producer | `agent-detect` `statefile.Writer` → `screen/<id>` (no `session=`) | compatible |
| Producer | `claude-status-update` → `panes/<id>` (`session=` present) | compatible |
| Producer | bridge daemon ships `screen/<local_pane_id>` (#741/#744) | compatible: local id is in the mirror session's `list-panes -s` |
| Transform | `aggregateSession` | **changed** — union + merge + live-id filter |
| Consumer | `claudeSegment` → `renderLine` | **changed** — pass live ids |
| Consumer | `scripts/lib-claude.sh` `read_pane_state`, `picker/main.go` `collectAgentPanesFrom` | compatible, out of scope (already merge) |
| Consumer | `picker/statusline/usage.go` `openAgents` | compatible, must not touch |

Proof: Go tests on `aggregateSession` (production helper that owns the invariant). Red against current HEAD for screen-only; green after the merge. Fade uses the same `fadePct` already applied to hook files (same timestamp field).

---

### Task 1: Red tests for screen merge + wire live IDs

**Files:**
- Modify: `picker/statusline/claude.go` (`aggregateSession` signature + `claudeSegment` + new `listSessionPaneIDs`)
- Modify: `picker/statusline/claude_test.go` (existing callers + new tests)
- Modify: `picker/statusline/main.go` (`renderLine`, `main`)
- Modify: `picker/statusline/main_test.go` (`renderLine` call sites)
- Modify: `docs/agents/agent-state.md`, `docs/agents/status-bar.md`
- Test: `picker/statusline/claude_test.go`

**Interfaces:**
- Consumes: existing `sessionAgg`, `fadePct`, `counts.tally`, `readIssueFile`
- Produces: `aggregateSession(dir, session string, now int64, liveIDs map[string]bool) sessionAgg`; `listSessionPaneIDs(session string) map[string]bool`; `claudeSegment(dir, session, theme string, now int64, liveIDs map[string]bool) string`; `renderLine(..., liveIDs map[string]bool)`

- [ ] **Step 1: Write the failing tests** (do not implement merge yet — only the signature plumbing needed to compile)

Add a liveIDs parameter defaulting through existing tests as `nil`. After plumbing, these new tests must fail on current merge logic (screen files ignored).

In `picker/statusline/claude_test.go`, update existing calls:

```go
agg := aggregateSession(dir, "work", now, nil)
got := claudeSegment(dir, "s", "dark", now, nil)
```

Add:

```go
func TestAggregateSessionScreenOnlyCountsViaLiveIDs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/screen/9", []byte("state=waiting\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"9": true})
	if agg.counts.total != 1 {
		t.Fatalf("screen-only total = %d, want 1", agg.counts.total)
	}
	if agg.counts.priorityState() != "waiting" {
		t.Fatalf("state = %q, want waiting", agg.counts.priorityState())
	}
}

func TestAggregateSessionHookWinsOverScreen(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/panes/1", []byte("state=waiting\ntimestamp=2000\nsession=work\n"), 0o644)
	os.WriteFile(dir+"/screen/1", []byte("state=idle\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"1": true})
	if agg.counts.total != 1 {
		t.Fatalf("total = %d, want 1", agg.counts.total)
	}
	if agg.counts.priorityState() != "waiting" {
		t.Fatalf("state = %q, want waiting (hook-first)", agg.counts.priorityState())
	}
}

func TestAggregateSessionScreenOtherSessionExcluded(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	os.WriteFile(dir+"/screen/9", []byte("state=processing\ntimestamp=2000\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"8": true}) // 9 not in this session
	if agg.counts.total != 0 {
		t.Fatalf("foreign screen-only total = %d, want 0", agg.counts.total)
	}
}

func TestAggregateSessionScreenStalenessMatchesPanes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(2000)
	// waiting fade starts at 30s; fully faded at 30+45
	os.WriteFile(dir+"/screen/9", []byte("state=waiting\ntimestamp=1925\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"9": true})
	if agg.minFade != 100 {
		t.Fatalf("screen-only minFade = %d, want 100 (same fadePct as panes/)", agg.minFade)
	}
}

func TestAggregateSessionStaleProcessingOverriddenByScreen(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/screen", 0o755)
	now := int64(400)
	os.WriteFile(dir+"/panes/1", []byte("state=processing\ntimestamp=0\nsession=work\nunseen=1\n"), 0o644)
	os.WriteFile(dir+"/screen/1", []byte("state=idle\ntimestamp=400\n"), 0o644)

	agg := aggregateSession(dir, "work", now, map[string]bool{"1": true})
	if agg.counts.priorityState() != "idle" {
		t.Fatalf("state = %q, want idle (stale processing + live screen)", agg.counts.priorityState())
	}
	if agg.unseen {
		t.Fatal("unseen must clear on screen override")
	}
}
```

In `picker/statusline/main.go` and `main_test.go`, add `liveIDs map[string]bool` as the last arg of `renderLine` and pass `nil` from tests; in `main`, call `listSessionPaneIDs(a.session)` and pass the result. Stub `listSessionPaneIDs` as:

```go
func listSessionPaneIDs(session string) map[string]bool {
	ids := map[string]bool{}
	if session == "" {
		return ids
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-s", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		return ids
	}
	for line := range strings.Lines(string(out)) {
		id := strings.TrimPrefix(strings.TrimSpace(line), "%")
		if id != "" {
			ids[id] = true
		}
	}
	return ids
}
```

Place it in `claude.go` (needs `context`, `os/exec`, `time` already used in main — add those imports to `claude.go`). Fail closed (empty map) so a tmux error does not invent pane membership.

Update `claudeSegment` to take `liveIDs` and pass it through. Update the comment on `aggregateSession` (still only `panes/` until Step 3).

In `docs/agents/agent-state.md` dead-agent bullet, the phrase `Go consumers (picker/statusline, the pickers) that read panes/ directly` is still true for hooks; add one sentence near the screen-scraped files bullet:

`picker/statusline` `aggregateSession` unions `panes/` and `screen/` for the line-0 session segment: hook-first with the same stale-active screen override as `read_pane_state` / `collectAgentPanesFrom`; a screen-only pane is attributed via `tmux list-panes -s` (live pane id set), not a file `session=` field. Mirror `screen/<local_pane_id>` files (#741/#744) land in the mirror session through that same map.

In `docs/agents/status-bar.md` Line 0 bullet, after "claude status (left)", add: session agent icon is `claudeSegment` over hook+screen files as above.

- [ ] **Step 2: Run tests — screen-only must be red**

Run: `nix develop -c bash -lc 'cd picker && go test ./statusline/ -count=1 -run TestAggregateSessionScreen'`

Expected: `TestAggregateSessionScreenOnlyCountsViaLiveIDs` FAIL `screen-only total = 0, want 1`. Other existing tests still pass after signature plumbing. `TestAggregateSessionHookWinsOverScreen` may already pass (hook-only path). `TestAggregateSessionScreenOtherSessionExcluded` may already pass (screen ignored). `TestAggregateSessionScreenStalenessMatchesPanes` FAIL. `TestAggregateSessionStaleProcessingOverriddenByScreen` FAIL (still waiting-or-processing from hook).

Also confirm current HEAD without the new merge is the bug: if Step 1's plumbing accidentally already counts screen files, stop — that is not today's code.

- [ ] **Step 3: Implement merge in `aggregateSession`**

Replace the body of `aggregateSession` so it:

1. Reads both `dir/panes` and `dir/screen` (missing dir is empty, not fatal).
2. Unions basenames (skip directories).
3. Parses each file with the existing `key=val` loop into `state`, `session`, `timestamp`, `unseen`.
4. For each id:
   - If hook present: start from hook fields. If `screenOverrideMaxAge(hook.state) > 0 && now-hook.ts > maxAge && screen.state != ""`: take screen `state`/`timestamp`, `unseen=false`, and session membership from `liveIDs[id]` (this session iff true). Else keep hook `session=` filter (`sess == session`).
   - If no hook: skip unless `liveIDs[id]` (screen-only cannot join without the live map; skip if screen state empty).
5. Tally, `fadePct`, `lastTs`, `unseen`, `readIssueFile(dir/issues/<id>)` exactly as today.
6. Duplicate `screenOverrideMaxAge` locally in `claude.go` (same cases and constants as picker: compacting 60, processing 300, done 60 — already implied by `fadePct`'s start table). Do not import picker.

`screenOverrideMaxAge` + constants:

```go
func screenOverrideMaxAge(state string) int64 {
	switch state {
	case "compacting":
		return 60
	case "processing":
		return 300
	case "done":
		return 60
	}
	return 0
}
```

These numbers already appear in `fadePct`'s start map; keep them equal (do not invent a second table of different values).

- [ ] **Step 4: Re-run statusline tests**

Run: `nix develop -c bash -lc 'cd picker && go test ./statusline/ -count=1'`

Expected: PASS, including all new `TestAggregateSession*` cases and existing `TestClaudeSegment*` / `renderLine` goldens.

- [ ] **Step 5: Commit**

```bash
nix develop -c git add picker/statusline/claude.go picker/statusline/claude_test.go picker/statusline/main.go picker/statusline/main_test.go docs/agents/agent-state.md docs/agents/status-bar.md docs/superpowers/plans/2026-09-23-statusline-screen-scraped-session-segment.md
nix develop -c git commit -m "$(cat <<'EOF'
fix(statusline): count screen-scraped agents in the session segment

Line 0 only scanned panes/, so pi/codex/cursor (and bridge-shipped
screen files) never contributed. Merge screen/ with hook-first
precedence and attribute screen-only panes via list-panes.

Closes #745
EOF
)"
```
