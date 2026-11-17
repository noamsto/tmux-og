# Window naming: follow `cd` and show a manual rename (#1157)

## Findings (root cause, measured on a scratch server running the built wrapper, main `be0b43a9`)

- `prefix + ,` *does* stick in `#{window_name}`: `rename-window` flips `automatic-rename` off and
  `@window_manual_name 1` makes the `tmux-update-icons` sweep leave it off (verified with a real attached
  client, real keys). What the user sees is the **status-bar tab**, and tabs render reflow's
  `@window_label_*` options, never `#{window_name}`. `tmux-reflow-windows` derives the label from
  issue/branch/task/dir and has no notion of a manual rename → the tab ignores the typed name
  (scratch client showed `1: a │ 2: b` for windows named `mynewname` / `renamed1`).
- `@window_label_short` (what `automatic-rename-format` reads first) and every tab option are only
  rewritten by reflow. Reflow's triggers (window add/remove/resize, attach, `--force` from sweeps) include
  branch / task / ai-name / crew / occupancy changes and a **worktree** move (`tmux-reconcile-window
  --cwd-move`, which exits without a reflow for a non-git cwd), but nothing fires on a plain `cd` between
  non-repo dirs or within one worktree. A plain shell window therefore keeps the label (and name) of the
  directory it was at the last reflow. Live server: plain fish windows show `@window_label_short` = stale
  basename.
- No sweep writer clears `@window_manual_name` or re-enables `automatic-rename` for a manual window
  (`tmux-update-icons.sh` ~963 is correctly gated). Deployed rev `6cf8b10b` has the same bind, gate and
  reflow label code (diff of the naming files since then touches only icons/PR/float code) → both symptoms
  are present there and on main.

## Design

One shadow, written by the per-tick `main()` pass of `tmux-update-icons` (the per-window loop that runs for a session name — NOT the `OG_TICK_SWEEP=1` sweep, which returns before that loop), `@window_label_seen` (window option, written only by `tmux-update-icons`, same shape
as `@window_cwd_seen`/`@crew_seen`): the sweep's memo of the label input no tmux hook reports —
`p:<cwd>` for a normal window, `n:<window_name>` for a manual window (`@window_manual_name 1` and
`automatic-rename` off). On mismatch the sweep writes the memo (direct argv) and sets `sess_need_reflow`,
which already ends in `reflow --force`. Reflow gains a manual arm: for such a window the label is the typed
name (id empty, rest short = rest long = the raw name, PR segment kept; local `@window_label_*` options store raw `#` everywhere — `build_window_label`, the picker, the bridge daemon's `cleanLabelValue` — so the name is stored raw too), so tabs,
`@window_label_short`, and the picker's `@window_label_id/@window_label_rest_long` all show it. No conf
change (bind untouched). Mirror windows (`@bridge_win`) stay daemon-owned and are skipped.

Alternative considered: `after-rename-window` hook / bind tail running reflow — rejected: two-file conf edit
(template + frozen reference), covers only the bind, not cd.

## Files

- `scripts/tmux-update-icons.sh` — read `@window_label_seen` and `window_name` in the batched row; compare,
  write, request reflow.
- `scripts/tmux-reflow-windows.sh` — read `@window_manual_name` + `automatic-rename`; manual label arm.
- `tests/reflow-fanout.bats` — real-tmux suite (check `reflow-fanout-tests`, has tmux, already has a
  fake-reflow log harness and real-reflow label assertions): cd and rename trigger a reflow; the manual label
  arm. (`tests/reflow.bats` is a lib-reflow unit suite without tmux — not usable.)
- `docs/agents/agent-state.md`, `docs/agents/scripts.md`, `docs/agents/status-bar.md` — document ownership.

## Steps

- [ ] **Step 1: red tests, update-icons.** In `tests/reflow-fanout.bats` (fake reflow + `run_update_icons`
  which waits for the backgrounded reflow log; windows run a plain shell, no agent) add (a) "a plain
  window's cd requests a reflow": fake reflow appends its argv to a log; run `run_update_icons` (stamps memo),
  clear log, `respawn-pane -k -t <win> -c <dir2>` (as `tests/update-icons-cwd-move.bats` does; no polling, shell-independent), `run_update_icons` → log has the session;
  `@window_label_seen` = `p:<dir2>`; a further tick with no change leaves the log empty. (b) "a manual
  rename requests a reflow": `set -w @window_manual_name 1; rename-window typed` (auto off), tick, clear,
  `rename-window typed2`, tick → reflow logged, memo `n:typed2`. (c) a non-manual window's rename does not
  key on the name. Clear the log only after the previous tick's reflow has logged, and wait a bounded time before any
  absence check. Run: `nix build .#checks.x86_64-linux.reflow-fanout-tests` — expect (a),(b) FAIL on current code (no reflow call, memo unset).
- [ ] **Step 2: red tests, reflow.** In `tests/reflow-fanout.bats` (real `tmux-reflow-windows`, attached-width
  arg as the existing label tests do) add a window with `@window_manual_name 1`,
  `automatic-rename off`, name `typed`, in a dir `d`: after reflow `@window_label_short`,
  `@window_label_rest_long` = `typed`, `@window_label_id` empty; a `#` in the name is stored unchanged (`a#b`) and a `|` is stored as a space (`api | web` → `api   web`, the sanitizer that keeps later fields from shifting); the same window with `automatic-rename on` keeps the derived label (`d`). Expect FAIL (label is `d`).
- [ ] **Step 3: implement sweep trigger.** `tmux-update-icons.sh`: append `#{@window_label_seen}` (beside
  `@window_cwd_seen`, ahead of the `pane_active` canary) and `#{s/[|]/ /:window_name}` (beside the other
  sanitized free-form fields) to the `list-panes` format and the `read` list; record per window
  (first pane wins) in `win_label_seen`, `win_wname`. After the cwd-move block: skip bridge/poisoned
  windows; compute key (`n:`+name when `win_cur_manual==1 && win_cur_rename!=1`, else `p:`+cwd); on
  mismatch `tmux set-option -t <cwd-pane|target> -w @window_label_seen "$key"` (direct argv, a path may
  contain `'`) and `sess_need_reflow[$s]=1`. Update the field-order comment block and the comment at the cwd-move block that says a reflow from
  there would race ahead of the stamps (the reconciler's own forced reflow follows, so the end state is
  correct; the cd reflow is a deliberate extra).
- [ ] **Step 4: implement reflow arm.** `tmux-reflow-windows.sh`: add `#{@window_manual_name}|
  #{automatic-rename}` after `@window_has_agent` in `FMT` and the `read`; sanitize the existing
  `#{window_name}` field as `#{s/[|]/ /:window_name}` (it becomes the label, so a `|` must not shift the
  trailing `@window_bridge_name|@window_task` fields); in the local arm after both `build_window_label`
  calls, when `manual==1 && autorename==0 && -n wname`: `win_id=""`, `win_rest_short=win_rest_long=$wname`.
  Update the FMT comment that says window_name is unused on the local path.
- [ ] **Step 5: green + neighbours.** Run the affected checks (`update-icons-all-windows-tests`, `reflow-tests`, `reflow-fanout-tests`,
  `pane-shell-prompt`, `naming-seed`, `rename-bind-integration`, `tmux-next38-readiness`) — all pass.
- [ ] **Step 6: counterfactual proof.** In a scratch copy restore the old scripts, run the new tests → red
  assertion output recorded for the PR body.
- [ ] **Step 7: live-style repro after fix** on the scratch server + attached client: cd shows in the tab and
  `window_name`; `prefix + ,` rename shows in the tab and survives sweeps.
- [ ] **Step 8: docs.** Update the docs (plus `performance.md`: one reflow per `cd`, one-time memo write per
  window after upgrade; and note the memo keys on the first non-floating pane, like the cwd-move gate) (new `@window_label_seen` shadow, manual label arm, the
  "tabs render `@window_label_*`, not `window_name`" fact).
- [ ] **Step 9: gates.** `nix build .#lint --accept-flake-config`, `nix flake check --no-build`,
  `nix flake check`, `nix build .#default`.

## Consumer map (shared contract: `@window_label_*` / label inputs)

| edge | disposition |
| --- | --- |
| producers of `@window_label_*`: reflow only | changed (manual arm), tested in reflow-fanout.bats |
| sweep → reflow trigger (`sess_need_reflow`) | changed (new cause), tested in reflow-fanout.bats |
| status-format[0..3] tabs | compatible, read the same options |
| picker window rows (`picker/main.go` labelID/labelRest) | compatible; raw name, shows manual name |
| bridge daemon `windowlabels.go` (ships `@window_label_*` into `@bridge_label_*` for a controller mirror) | compatible; a remote's manual name now reaches the mirror, raw like every other local label |
| pane-border title `TITLE_RAW` (`tmux-apply-theme-colors.sh:83`), fold `FOLD_LABEL` (`tmux-reflow-windows.sh:817`) | compatible; read the same options, show the manual name |
| `automatic-rename-format` (`@window_label_short`) | compatible; unused while auto-rename off |
| agent reset (`claude_clear_window_*`, `MANUAL_NAME`) | compatible, untouched |
| mirror windows (`@bridge_win`) | skipped in sweep memo; reflow bridge arm unchanged |
| remux restore | compatible: restore-induced auto-off without manual stays re-enabled by the sweep |
| memo empty/first tick | one extra forced reflow per session; accepted |

## Acceptance

- PR body: repro revs (6cf8b10b and main both show it), writers, trace evidence, fix → write from Findings.
- New bats red before / green after → Steps 1, 2, 6.
- `nix build .#lint --accept-flake-config`, `nix flake check --no-build`, `nix flake check`,
  `nix build .#default` → Step 9.
- CI build job x86_64-linux + aarch64-darwin → cite run URL after push.

## Notes for docs

- The memo key uses the pipe-sanitized name, so `a|b` and `a b` share a key (harmless).
- The memo keys on the first non-floating pane's path, like the cwd-move gate: a `cd` in a different active
  split pane does not refresh the label.
