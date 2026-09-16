# PLAN — #647: drive pane-death cleanup from pane-died/pane-exited hooks

Spec of record: `SPEC.md` (accepted by spec-critic, 5 low notes folded in). Scope: shell/hook half
only — the daemon half (`healDeadRenderers`) is split to a follow-up issue per dispatcher ruling
(see step 10).

## Steps

- [ ] **Step 1: `claude_reap_pane` + sweep widening — `scripts/lib-claude.sh`**
  - New function `claude_reap_pane PANE_ID`:
    - Normalize/validate: accept `%N` or bare `N`, then require `^%[0-9]+$` after re-adding the
      `%`; anything else returns 0 (fail closed, same posture as the sweep's row check, #373).
    - Ownership guard: if `$CLAUDE_PANES_DIR/<id>` exists, read its first `session=` line with a
      pure-bash `while read` loop (no sed fork). If non-empty, require
      `tmux has-session -t "=$sess"` (exact-match `=`, per the session-targeting gotcha) to
      succeed; failure returns 0 without deleting. `tmux` absent/failing also fails closed —
      under bats the raw lib has no server, and skipping is the safe default.
    - `claude_progress_emit "$id" clear` (corpse keeps its tty + OSC 9;4 bar; best-effort helper
      already never aborts).
    - `rm -f` the six explicit paths: `$CLAUDE_PANES_DIR/$id`, `$CLAUDE_SCREEN_DIR/$id`,
      `$CLAUDE_INTERRUPT_DIR/$id`, `$CLAUDE_TASKS_DIR/$id`, `$CLAUDE_ISSUES_DIR/$id`,
      `$CLAUDE_WATCHERS_DIR/$id`. No globbing, no directory iteration.
    - Header comment: why single-id-only (shared CLAUDE_STATUS_DIR, per-server `%N` counters), the
      guard's two residuals (same-named session on two servers; rename-session fail-closed), and
      why `names/`/`live/` are excluded (ids monotonic within a server run; prune owns them).
  - `claude_reap_dead_panes`: dir list gains `$CLAUDE_TASKS_DIR` and `$CLAUDE_ISSUES_DIR` (six
    total, matching the hook — tasks/issues are documented to die with the pane). Update the
    function's header comment to say it is now the *backstop* for the death paths no hook fires
    on (kill-pane/kill-window/kill-session/respawn-pane -k/crash — measured matrix in SPEC.md).

- [ ] **Step 2: `scripts/tmux-reap-pane.sh`** (new)
  - `#!/usr/bin/env bash`, `set -euo pipefail`, shfmt tabs.
  - Guard-source `@lib_claude@` exactly like `claude-status-update.sh` guard-sources `@lib_log@`
    (`[[ -f "@lib_claude@" ]]` — raw script stays runnable under bats), then
    `claude_reap_pane "${1:-}"`.
  - No `-F` formats anywhere (tmux-format-delimiter-assertions scans `scripts/`).

- [ ] **Step 3: Nix packaging — `config/tmux.conf.nix`, `generator/paths/paths.go`**
  - Add `"tmux-reap-pane"` to `scriptNames` (:276) and route it through `mkScriptWithLibs` beside
    the `tmux-kill-pane-guard` route (:544) (needs `@lib_claude@` substitution, nothing else).
  - Add `"tmux-reap-pane"` to `ogInternal` (:683, sorted: between `"tmux-kill-pane-guard"` and
    `"tmux-reconcile-window"`) — the `ogPartitionOk` assert (:927) fails evaluation for any
    scriptNames entry missing from both `ogVerbSpec` and `ogInternal`.
  - Add `"tmux-reap-pane"` to the sorted `RequiredScripts` list in `generator/paths/paths.go`
    (:33) so the template can `{{index .Paths.Scripts "tmux-reap-pane"}}`.

- [ ] **Step 4: hook wiring — `config/tmux.conf.tmpl` + `config/tmux.conf.reference.nix`**
  - Clear block (~line 393, beside `set-hook -gu after-kill-pane`): add `set-hook -gu pane-exited`
    and `set-hook -gu pane-died`. Bare `-gu` clears every index — safe for tmux-remux's
    `pane-exited[99]` because `{{.PersistBlock}}` is sourced at the end of the template, after the
    clears, on every load/reload (same protection `after-kill-pane[99]` already relies on).
  - Replace the stale comment at :479-481 ("pane-exited is a silent no-op on the pinned tmux…")
    with the new setters and a short comment: measured hook matrix (exit/signal → `pane-exited`,
    corpse → `pane-died`, structural kills → nothing), why index 0 (tmux-og's primary-hook
    convention; remux sits at [99]), why `run-shell -b` + `#{q:hook_pane}`.
  - Setters:
    `set-hook -g pane-exited 'run-shell -b "{{index .Paths.Scripts "tmux-reap-pane"}} #{q:hook_pane}"'`
    `set-hook -g pane-died   'run-shell -b "{{index .Paths.Scripts "tmux-reap-pane"}} #{q:hook_pane}"'`
  - Mirror the identical edit in `config/tmux.conf.reference.nix` (the two files are edited
    together — its header and the extraction check's diff demand it).

- [ ] **Step 5: backstop cadence — `scripts/tmux-update-icons.sh`**
  - Line 93 (`[[ -n ${1:-} ]] || claude_reap_dead_panes "$rows"`): additionally gate the reap on
    `((CLAUDE_NOW % 60 == 0))`, so the per-tick caller reaps ~once a minute while arming/presence
    stamping keep the 5s cadence (`CLAUDE_LIVE_SWEEP_FRESH=15` is calibrated to it). Monitor-hook
    caller (`$1` non-empty) still never reaps.
  - Update the comment block at :57-93 (the sweep's third job is now a backstop; hook owns the
    common path; measured structural-kill gap).

- [ ] **Step 6: unit tests — `tests/prune-stale-state.bats`, `tests/agent-liveness.bats`**
  - prune-stale-state.bats: update "reap drops dead pane files…" to the six-dir set (add
    tasks/issues fixtures + asserts).
  - agent-liveness.bats (required by the Step 5 cadence change): `setup_sweep` exports
    `CLAUDE_NOW=100`, and `100 % 60 != 0` would skip the reap — override `CLAUDE_NOW=120` (a
    multiple of 60) inside the two reap tests only ("sweep calls claude_reap_dead_panes with the
    fetched rows" :250, "sweep still reaps when both arm and stamp are off" :280). Do NOT change
    `setup_sweep` globally: the live-stamp tests (:308-309) assert the stamp content equals
    `"100"`.
  - New `claude_reap_pane` tests, fake `tmux` on PATH (agent-detect-arm.bats idiom):
    - deletes exactly the named pane's six files, keeps a sibling pane's;
    - rejects `""`, `garbage`, `%12x`, `5; rm -rf /` — no deletions;
    - guard: state file with `session=alpha`, fake tmux `has-session` fails → nothing deleted;
    - guard pass: fake tmux `has-session` succeeds → deleted;
    - no `panes/<id>` file (screen-only pane) → screen/tasks files deleted unguarded;
    - accepts bare `5` and `%5` equally.
- [ ] **Step 7: live hook test — `tests/reap-pane-hook.bats` + flake check**
  - New bats suite on the tick-floor.bats idiom: `TMUX_BIN` wrapped server, `-L` socket, scratch
    HOME/XDG, scratch `CLAUDE_STATUS_DIR` (the TMUX_BIN conf-assertion at flake.nix:1340 requires
    exporting the scratch dirs).
    - Case 1: session `alpha`, two panes; seed all six state files for both pane ids (with
      `session=alpha` in the `panes/` files); exit one pane's command → within a few seconds
      exactly that pane's six files are gone, sibling's intact.
    - Case 2: `remain-on-exit on` window; command exits → corpse, `pane-died` path, same
      deletion, and the corpse's OSC progress clear is attempted (assert via the file deletions
      only — progress emit is best-effort).
    - Case 3 (cross-server guard): second wrapped server on its own `-L` socket, session `beta`,
      sharing the scratch `CLAUDE_STATUS_DIR`; its pane id collides with a live `alpha` pane id
      whose `panes/<id>` names `session=alpha`; kill B's pane process → hook fires on B, guard
      sees no local `alpha`, A's file survives. (Same-named-session residual is accepted per
      SPEC.md and not tested.)
  - New `reap-pane-hook-tests` runCommand in flake.nix beside `tick-floor-tests` (same
    nativeBuildInputs shape: bash, bats, coreutils, gnugrep, `(mkTmux pkgs)`; `TMUX_BIN` set;
    LANG/LC_ALL).
- [ ] **Step 8: docs — `CLAUDE.md`**
  - `tmux-update-icons` row: the every-5th-tick reap is now the ~60s backstop; pane-death cleanup
    is hook-driven (`pane-exited`/`pane-died` → `tmux-reap-pane`, single-id, ownership-guarded).
  - "Watcher registry files" bullet: removal is now primarily the hook, backstop sweep + restart
    prune unchanged; superseded-watcher invariant restated.
  - Script Roles table gains a `tmux-reap-pane` row.

- [ ] **Step 9: gate + hardware verify**
  - `nix build .#default`, `nix flake check`, `nix build .#lint` — all green.
  - Hardware drive per SPEC.md "Hardware verify": scratch server (`TMUX_TMPDIR=/tmp/og-$$`,
    scratch `CLAUDE_STATUS_DIR`), agent process in a pane, kill it → exactly that pane's files
    go; second scratch server sharing the dir → foreign-session state survives. `kill-server`
    after; never the live server, never the real `/tmp/claude-status`.

- [ ] **Step 10: follow-up issue + PR**
  - `gh issue create` for the daemon half (`healDeadRenderers` ← `pane-died`), carrying the
    dispatcher's three constraints verbatim (resetWindow-not-retireMirror/#547;
    deadRendererStrikes cap under event drive; `#{pane_dead}`+`@bridge_pane` key load-bearing,
    user floats not reaped). Reference #647.
  - PR body: `Closes #647`, names the follow-up issue, states the daemon half is deliberately
    deferred (dispatcher ruling), `## Plan` section, proof of work (gate output + hardware
    verify transcript).

## Commit plan

One commit: `feat(scripts): reap pane state from pane-exited/pane-died hooks (#647)` — code, tests,
docs, and `docs/superpowers/plans/2026-09-16-pane-death-hooks.md` +
`docs/superpowers/specs/2026-09-16-pane-death-hooks-design.md` (copies of PLAN.md/SPEC.md per the
tracked-plans rule) land together.
