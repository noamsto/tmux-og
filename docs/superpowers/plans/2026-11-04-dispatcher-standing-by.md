# Dispatcher standing-by icon and status-line count (#1083)

## Design decisions

- **Display-only overlay.** "Standing by" never enters `claude_priority_state` / `agentPriority` /
  `counts.priorityState`. The window's real state is still idle/done; only the glyph and hue
  rendered for it change. Why: the tables must stay byte-identical across shell, Go and picker;
  an overlay needs no change to any of them and cannot leak into the session tint or pickers.
- **No new forks / queries.** `tmux-update-icons` already walks every window each tick from one
  batched `list-panes -a`. It gains `#{@crew_id}` in that format (a `|`-squashed field placed
  before the free-form `@window_task`), derives per-window standing-by, and publishes a
  server-wide option `@og_dispatchers` (space-joined per-dispatcher display states in a fixed
  order, e.g. `waiting standby idle`), change-gated through `tmux_cmds` (the current value is read
  back through the same batched format, `#{@og_dispatchers}`).
- **Go segment reads that option** via the existing `fetchVolatile` display-message
  (`volatileFields` gains `#{@og_dispatchers}` as field 23; the format is still one roundtrip).
  Go owns the count + urgency ranking (needs-human > working > standby > idle/done), so the
  priority logic is unit-tested in `picker/statusline`.
- **Glyphs** are named: shell `CLAUDE_ICON_STANDBY` (lib-claude.sh, with `# nerd:` comment),
  hue `H_SB` in both theme branches of `setup_claude_colors`; Go consts `iconStandby`,
  `iconDispatcher` and a `standby` hue in `statePalette`, mirroring how `stateIcon` already mirrors
  the `CLAUDE_ICON_*` set (no new generator/`@icon_*` plumbing needed).

## Files

- `scripts/lib-claude.sh` — `CLAUDE_ICON_STANDBY`, `H_SB`/`C_SB`.
- `scripts/tmux-update-icons.sh` — read `@crew_id` + `@og_dispatchers`; standing-by derivation; icon overlay; publish option.
- `picker/statusline/claude.go` — standby hue/icon consts, `dispatcherSegment(states, theme)`.
- `picker/statusline/main.go` — field 23, `args.dispatchers`, append segment to line 0.
- `picker/statusline/main_test.go`, `claude_test.go` — field-count test (24), segment tests.
- `tests/update-icons-dispatcher.bats` (new) — bats cases.
- `flake.nix` — register `update-icons-dispatcher-tests` runCommand check (copy of `update-icons-all-windows-tests`, same nativeBuildInputs); `nix flake check` output must list it.
- `tests/lib-claude-theme.bats` — `H_SB` assertions for both themes.
- `docs/agents/agent-state.md`, `docs/agents/status-bar.md`, `docs/agents/scripts.md` (tmux-update-icons row) — docs.

## Steps

- [ ] **Step 1: Go tests first** (`picker/statusline/claude_test.go`): `dispatcherSegment` returns "" for no states; count shown; tint ranking for `["idle","standby","processing","waiting"]` -> waiting hue, `["idle","standby","compacting"]` -> working hue, `["idle","standby"]` -> standby hue, `["done","idle"]` -> idle hue; denied/error rank as needs-human. Update `volatileFields` length test to 24 with new last index. Run `cd picker && go test ./statusline/...` -> FAIL (undefined).
- [ ] **Step 2: Implement Go segment** in `claude.go` + wire `fetchVolatile`/`renderLine` in `main.go`. Run the same command -> PASS.
- [ ] **Step 3: bats tests first** (`tests/update-icons-dispatcher.bats`, modelled on `update-icons-all-windows.bats`; scratch tmux server): (a) dispatcher window (`@crew_name dispatcher`, `@window_has_agent 1`, idle pane state file) + same-session worker window (`@crew_id x`, `@crew_name amber`, `@window_has_agent 1`) -> `@window_icon_display` contains the standby glyph, not the idle glyph; (b) no worker -> idle glyph; (c) worker with `@window_has_agent` unset -> idle glyph; (d) dispatcher with `@crew_id` joins a worker in another session by id, and a same-session worker with a different id does not; (e) `@bridge_win 1` worker/dispatcher ignored; (f) processing dispatcher keeps processing glyph; (g) `@og_dispatchers` value for mixed windows. Run `nix develop -c bats tests/update-icons-dispatcher.bats` -> FAIL.
- [ ] **Step 4: Implement shell** in lib-claude.sh and tmux-update-icons.sh per Design decisions. Run bats -> PASS; also `bats tests/icons.bats tests/update-icons-all-windows.bats tests/lib-claude-theme.bats` (consumers of the changed read loop / colors) -> PASS.
- [ ] **Step 5: Docs** in the three docs files.
- [ ] **Step 6: Gates**: `nix build .#lint --accept-flake-config`, `nix flake check --no-build`, `nix flake check`, `nix build .#default`.
- [ ] **Step 7: Manual scratch-server capture** per task acceptance; paste into PR.

## Revision 1 (plan-critic findings)

- **Overlay mechanics:** add a `standby` case to `claude_state_icon` and `claude_faded_hex` (lib-claude.sh) and call `claude_colored_icon standby 0 1`-style from tmux-update-icons only when the overlay applies; `claude_priority_state` never sees `standby`.
- **Read loop:** add `#{@crew_id}` (`s/[|]/ /`-wrapped) and `#{@og_dispatchers}` to the format and matching `read` variables, both before `cur_task`; copy `@og_dispatchers` into a scalar inside the loop (the EOF read blanks loop vars), like `sess_cur_active_icon`.
- **Dispatcher test:** `win_cur_crew` is `@crew_name` plus an optional ` <host>` suffix; compare `${win_cur_crew%% *}` to `dispatcher`; workers are `!= dispatcher` with non-empty `@crew_id`.
- **Edge decisions:** `interrupted` ranks with idle/done in Go (halted, not urgent); a dispatcher window with `@window_has_agent 1` but no claude state emits `idle`.
- **Placement:** segment goes in `renderLine` right after `claudeSegment` output, before `#[align=right]`; empty segment leaves existing output byte-identical. Add `{23, "#{@og_dispatchers}"}` to the volatileFields index test.
- **PR note:** glyphs are named consts/vars rather than `@icon_*` options; say so in the PR body.

## Revision 2 (second plan-critic round)

- **Bats assertions are ASCII-only.** The flake sandbox's querying client is non-UTF-8 (glyphs read back as `_`). Step 3 therefore `sed`s `CLAUDE_ICON_STANDBY` / `CLAUDE_ICON_IDLE` / `CLAUDE_ICON_DONE` to distinct ASCII tokens (`SB`, `ID`, `DN`) in the test's copy of lib-claude.sh, exactly as `update-icons-all-windows.bats` seds ICON_MAP, and asserts `@window_icon_display` contains `SB` or `ID`. Setup seams copied from that suite: `AGENT_COMMANDS=claude`, pinned `CLAUDE_NOW` (multiple of 5), bash-copied-to-`claude` default shell; a fresh `unseen=1` state file so hue is never faded.
- **Overlay passes real fade/unseen:** `claude_colored_icon standby "${win_claude_fade[$wkey]:-0}" "${win_claude_unseen[$wkey]:-0}"` — the overlay replaces only the state name, keeping fade semantics identical to idle/done.
- **Palette parity:** Go `claudePalette` gains `standby` with the same literal hexes as `H_SB` in both themes; a Go test pins the literals and `tests/lib-claude-theme.bats` pins `H_SB`.
- **Option scope:** `@og_dispatchers` is a global session option (`set -g`), written and read the same way; docs say so.
- **Docs:** note that the standby glyph also reaches `@window_icon_display` consumers (status-format[1], FOLD_AGENT in reflow, automatic-rename-format), not only the tab bar; `# nerd:` names recorded for both new glyphs.

## Consumer map (shared contract: the batched list-panes row)

The row is read only by the `while IFS='|' read` loop in `tmux-update-icons.sh`; adding a field before `@window_task` and matching `read` variable keeps later fields aligned. `@og_dispatchers` consumers: only the Go statusline. `volatileFields` consumers: `fetchVolatile` and `main_test.go` count test. Priority tables untouched.

## Acceptance

Maps 1:1 to the task's checklist: bats (Step 3/4), Go tests (Step 1/2), manual capture (Step 7), the four nix commands (Step 6).
