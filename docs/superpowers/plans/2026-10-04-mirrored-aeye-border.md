# Plan: detect a mirrored aeye carousel pane on the local border (#866)

## Design

The remote reduces `@claude_img_src` to a presence bit **inside the format it evaluates**:
`#{?@claude_img_src,1,}`. The raw value (`"<server pid>-<pane>"`, aeye's own key) never
crosses the wire, so there is nothing free-form to sanitize; the daemon additionally accepts
only the exact string `"1"` (anything else reads as absent), so a hostile/odd remote can at
worst toggle the bit. The daemon stamps it onto the mirror pane as `@bridge_img_src` = `1`
(or `-u` when it goes away) through the existing `stampCrew` argv sequence — daemon-owned,
never the real `@claude_img_src` name (a local `@claude_img_src` on a mirror pane would make
local `tmux-update-icons` treat the mirror pane as a local carousel and stamp
`@remux_relaunch` on it).

Alternative considered: ship the raw key and validate it with a regex daemon-side. Rejected:
only presence matters locally, and a value that never crosses cannot be sanitized wrongly.

## File list

- `picker/remotebridge/daemon/agentstatus.go` — add the presence field to `agentStatusFormat`
  (before the free-form task, which must stay last), bump `agentStatusFields` 9→10, add
  `paneStatus.imgSrc bool`, parse it, add `{"@bridge_img_src", ...}` to the stamped option
  table; rename `bridgeCrewOptions`/`stampCrew` to `bridgePaneOptions`/`stampPaneOptions`
  since the table is no longer crew-only, and update their comments.
- `picker/remotebridge/daemon/agentstatus_test.go` — fixture rows shifted for the new field;
  new parse case and new shipper stamp/clear test.
- `scripts/tmux-apply-theme-colors.sh` — `AEYE` also true for `@bridge_img_src`.
- `tests/pane-border-format.bats` — new case: a mirror pane carrying `@bridge_img_src`
  renders `━━ aeye ━━` and loses the bridged role colour.
- `docs/agents/bridge-shipped-state.md` — new bullet under "Remote Agent Status".
- `docs/agents/status-bar.md` — line 53: replace the "A mirrored aeye is not detected"
  sentence with the `@bridge_img_src` detection.

## Steps

- [ ] **Step 1: failing Go tests** (`agentstatus_test.go`).
  - `TestParseAgentStatus`: shift every fixture row's task to field index 9 (insert one empty
    field before the task in rows `%3` and `%5`; other rows have empty/absent task). Add row
    `"%9|fish|||||||1|"` asserting `imgSrc == true`, and
    row `"%10|fish|||||||yes|"` asserting `imgSrc == false` (only exact `1` counts). Update
    the row-count assertion.
  - New `TestAgentShipperStampsCarouselMarker`: `apply` a row `{pane:"%1", proc:"fish",
    imgSrc:true}` → expect a call `set-option -p -t %7 @bridge_img_src 1`; re-apply unchanged
    → no new call; set `imgSrc=false` → expect `set-option -p -t %7 -u @bridge_img_src`.
    Also assert a never-marked pane (first seen, imgSrc false) issues no `@bridge_img_src` call.
  - Run `cd picker && go test ./remotebridge/daemon/ -run 'TestParseAgentStatus|TestAgentShipperStampsCarouselMarker'` → expect FAIL (compile error on `imgSrc`).
- [ ] **Step 2: implement** (`agentstatus.go`): format becomes
  `...|#{s/[|]/ /:@crew_role_color}|#{?@claude_img_src,1,}|#{@claude_task}`;
  `agentStatusFields = 10`; `imgSrc: at(8) == "1"`, `task: at(9)`; table entry
  `{"@bridge_img_src", func(r paneStatus) string { if r.imgSrc { return "1" }; return "" }}`;
  rename table/function, update the format doc comment to mention the marker.
  Run `cd picker && go test -race ./remotebridge/daemon/` → PASS.
- [ ] **Step 3: failing bats case** (`tests/pane-border-format.bats`): `@test "mirror aeye pane
  detected by @bridge_img_src"` — `@bridge_win 1` on `$WIN`, split, set
  `@bridge_img_src 1` and `@bridge_crew_role_color colour114` on the split pane; assert
  `render` = `━━ aeye ━━` and inactive style contains `fg=#7f849c` and not `colour114`.
  Run `bats tests/pane-border-format.bats` (inside devshell, pinned tmux) → new case FAILS.
- [ ] **Step 4: implement** (`tmux-apply-theme-colors.sh`):
  `AEYE='#{||:#{||:#{@claude_img_src},#{@bridge_img_src}},#{m:*/bin/aeye *,#{pane_start_command}}}'`
  and its comment. Re-run bats → PASS; `shellcheck scripts/tmux-apply-theme-colors.sh` clean.
- [ ] **Step 5: docs** — bridge-shipped-state.md bullet (what crosses: a presence bit evaluated
  remote-side, stamped as `@bridge_img_src`, why not the raw key, why not the real name);
  status-bar.md line 53 updated.
- [ ] **Step 6: gate** — `nix flake check` and `nix build .#lint` (whole: flake check is the
  repo's canonical gate and golangci needs cross-package info).

## Acceptance

- `nix flake check` + `nix build .#lint` pass → Step 6 output.
- Daemon ships marker when remote has `@claude_img_src`, clears when gone →
  `TestAgentShipperStampsCarouselMarker` + `TestParseAgentStatus` (Steps 1–2).
- Border format picks aeye label for a mirror pane carrying `@bridge_img_src` → new bats case (Steps 3–4).
