# Newborn agent pane repaint (#883) — plan

Spec: `docs/superpowers/specs/2026-09-30-newborn-agent-repaint-design.md`
(spec-critic: accept). Old-behaviour build for the red run:
`$SP/old-tmux` (the wrapped tmux at `5a3c672`, pinned as a GC root).

`$SP` is the implementing session's scratchpad directory (not committed).
`$SP/old-tmux` is `nix build .#default` at `5a3c672`. The reproducer's timeline
is recorded in the spec's "Measured timeline" section.

## File list

- `config/tmux.conf.tmpl`: add `set-hook -gu after-split-window` to the clear
  block, plus the three fork-free `[30]` birth-stamp hooks and their comment.
- `config/tmux.conf.reference.nix`: the same text in `hooksText`. This is the
  byte-identity oracle, and the two files are edited together.
- `scripts/tmux-agent-repaint.sh` (new): the worker (wait for ready, gate,
  slot, nudge).
- `scripts/tmux-update-icons.sh`: add `#{@og_birth_size}` to the
  `arm_agent_detect` row, plus the sweep-only atomic claim. New
  `@agent_repaint@` placeholder.
- `config/tmux.conf.nix`: `tmux-agent-repaint` in `scriptNames` and
  `ogInternal`; the `@agent_repaint@` substitution in `mkScriptIcons`.
- `tests/agent-repaint-integration.bats` (new): scratch-server regression
  through the built wrapper.
- `flake.nix`: `agent-repaint-integration-tests` check (reduced conf, as
  `rename-bind-integration-tests`).
- `docs/agents/scripts.md`: new `tmux-agent-repaint` row, and the
  `tmux-update-icons` row gains the claim.
- `docs/agents/performance.md`: hook-budget note for the birth stamp and the
  sweep claim.
- `docs/superpowers/specs/2026-09-30-newborn-agent-repaint-design.md`,
  `docs/superpowers/plans/2026-09-30-newborn-agent-repaint.md`: committed
  alongside the code.

## Steps

- [ ] **Step 1: write the failing regression test**
  `tests/agent-repaint-integration.bats`, modelled on
  `tests/rename-bind-integration.bats`'s setup/teardown: `IN`/`OUT` `-L`
  sockets, a `TMUX_BIN` wrapper, `HOME`/`XDG_*`/`CLAUDE_STATUS_DIR`/
  `OG_ENRICH_*`/`OG_AGENT_USAGE_DIR` under `$BATS_TEST_TMPDIR`, `SHELL=bash`,
  `@splash_shown 1`, and `kill-server` on both sockets in teardown.
  - Agent stand-in: copy `bash` to `$BATS_TEST_TMPDIR/bin/pi`, so
    `pane_current_command` is `pi`, which is in `$AGENT_COMMANDS`. Run it on
    `probe.sh`, a heredoc'd script. It prints one line (`probe up`), then logs
    `stty size </dev/tty` to `$LOG` at start (`boot W H`) and on every `WINCH`,
    only when the size changed (`size W H`), which is Node's rule. It idles in
    `sleep 0.1 & wait $!` so the trap runs at once.
  - Helper `wait_for <secs> <cmd…>` (bounded poll, 0.2 s).
  - Test 1 (single pane, resized): create the window with
    `new-window -d -P -F '#{window_id} #{pane_id}' "pi probe.sh"`, wait for
    `boot`, then `resize-window -t @w -x 100 -y 30 ; set -wu -t @w
    window-size` (two separate tmux calls from bats). The unset leaves the detached window at 100x30 (measured)
    with the option inherited. Settled = wait until the log's last line equals
    `display -p -t %p '#{pane_width} #{pane_height}'`, because the probe logs
    asynchronously on WINCH. Wait ≤ 25 s for two more `size` lines after it.
    The first differs from settled, and the last equals settled. Then assert
    `#{window_width}x#{window_height}` is 100x30 and `show -wqv -t @w
    window-size` is **empty**. That can only hold if the nudge restored the
    inherited option, because its own `resize-window` sets `manual`.
  - Test 2 (grid, not viewed): `new-window -d` lead probe, then
    `split-window -d -h` role probe after the lead's `boot`, then
    `select-layout -t @w main-vertical`, then `select-pane -t <role>` and
    `select-pane -t <lead>`. That gives a known baseline, active = lead and
    last = role: a `-d`-built window has no `{last}`, and no `select-pane`
    sequence can restore an empty one. Record `window_layout`, the active
    pane, `{last}` (`#{P:#{?pane_last,#{pane_id},}}`), and each log's settled
    size (wait-until-equal, as in test 1). Wait ≤ 30 s until **each** log shows a
    zoom-sized change (the window size) followed by a return to its settled
    size, and no other change. Assert `window_zoomed_flag` is 0, the active
    and last panes are unchanged, and `window_layout` is unchanged.
  - Test 3 (never resized): `new-window -d` probe, no resize. Wait 12 s
    (> sweep 5 s + ready ≥ 3 s + quiet 1 s + slack), then assert no `size`
    line.
  - Test 4 (viewed grid, target not active): attach a client (outer server,
    as `attach_client` in rename-bind), make the grid window current, and keep
    the lead active. Use a `split-window -d` so focus stays on the lead. The
    role pane is resized after launch (split + layout). Timing is anchored on
    the observable claim, not on fixed sleeps: poll until the role pane's
    `@og_birth_size` is empty (the sweep claimed it, so the worker is spawned
    at t_s). The worker is in its slot wait from t_s+4 s (3 s floor + a 1 s
    quiet sample on a quiet probe) until at least t_s+12 s (ready + 8 s). At
    t_s+6 s, assert the role log has no zoom-sized change and the active pane
    is still the lead. Then `select-pane` the role and assert its nudge lands
    within 4 s.
  - Test 5 (excluded windows): (a) a probe window with `@bridge_win 1`, and (b)
    a probe window holding a float (`new-pane -d -x 20 -y 5 'sleep 99'`), both
    resized after launch. After 12 s, neither log has a size line after the
    settled one, and the float's geometry is unchanged.
  - Stage the new file at once (`git add tests/agent-repaint-integration.bats`),
    since the flake copies only tracked files.
  - Command, on the old build (red): `TMUX_BIN=$SP/old-tmux/bin/tmux bats
    tests/agent-repaint-integration.bats`. Expected: tests 1, 2 and 4 fail
    their "nudge landed" assertions (no stamp, no worker); tests 3 and 5 pass
    vacuously.

- [ ] **Step 2: birth-stamp hooks** in `config/tmux.conf.tmpl` and
  `config/tmux.conf.reference.nix` (identical text). Add
  `set-hook -gu after-split-window` after `set-hook -gu window-layout-changed`.
  After the `after-new-window[10]`/`after-new-session[10]` reconcile hooks, add:
  ```
  # Birth size for tmux-agent-repaint (#883): fork-free, one per pane creation.
  set-hook -g after-new-window[30]   'set -pF @og_birth_size "#{pane_width}x#{pane_height}"'
  set-hook -g after-split-window[30] 'set -pF @og_birth_size "#{pane_width}x#{pane_height}"'
  set-hook -g after-new-session[30]  'set -pF @og_birth_size "#{pane_width}x#{pane_height}"'
  ```
  with a short comment on why (a size-only stamp, consumed at first agent
  sighting). Prove it: `nix build .#default`, then on a scratch server
  (`TMUX_TMPDIR=/tmp/og-$$ … -L probe`) create a window and a split, and check
  that `list-panes -a -F '#{pane_id} #{@og_birth_size}'` shows a stamp on both.
  `nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions` stays
  green.

- [ ] **Step 3: the worker `scripts/tmux-agent-repaint.sh`**
  (implement: escalated), for the concurrency (sibling slots, atomic `if -F`)
  and the restore ordering. `set -uo pipefail`, bash, tabs, and `tmux` on
  PATH, as the other store scripts. Args are `<pane> <birth WxH>`; validate
  `^%[0-9]+$` and `^[0-9]+x[0-9]+$`, else exit 0.
  1. Ready: `start=$SECONDS`. Sleep 3. Loop: `g1=gen`, sleep 1, `g2=gen`,
     using `display -p -t "$pane" '#{pane_output_generation}'`. A failed
     display (the pane is gone) exits 0. Break when `g1 == g2`, including both
     empty, or at `SECONDS - start >= 12`.
  2. One `display -p -t "$pane"` with `|`-delimited fields:
     `pane_width`, `pane_height`, `window_id`, `window_width`,
     `window_height`, `window_panes`, `pane_floating_flag`, `@bridge_win`, and
     `#{P:#{?pane_floating_flag,1,}}`. Exit 0 if `WxH == birth`, the pane is
     floating, `@bridge_win` is set, or any float is present.
  3. Slot loop, polling every 0.25 s up to 8 s after ready. The slot format
     (`SLOT`, one shell constant) is
     `#{&&:#{&&:#{!:#{window_zoomed_flag}},#{!:#{P:#{?pane_floating_flag,1,}}}},#{||:#{==:#{window_panes},1},#{||:#{pane_active},#{==:#{window_active_clients},0}}}}`.
     It re-checks "no float" atomically, so a popup opened during the wait
     blocks the nudge.
  4. Single pane. Run
     `tmux if -F -t "$pane" '#{&&:<slot>,#{==:#{window_panes},1}}' "resize-window -t $wid -x $((W-1)) -y $((H-1))" 'display -p no'`.
     On output `no`, re-read the pane count: if it is >1 go to the multi-pane
     path, else back to the slot loop. The saved `window-size` is from
     `show -wqv -t "$wid" window-size`, read before that command. Then sleep
     0.3, `resize-window -t "$wid" -x "$W" -y "$H"`, and restore the option:
     `set -wu` when the saved value is empty, else `set -w -t "$wid"
     window-size "$saved"`.
  5. Multi pane. Each attempt re-reads `act`/`last`
     (`#{P:#{?pane_active,#{pane_id},}}` / `#{P:#{?pane_last,#{pane_id},}}`)
     right before its zoom, then runs
     `tmux if -F -t "$pane" "$SLOT" "resize-pane -Z -t $pane" 'display -p no'`.
     `no` goes back to the slot loop. The slot requires "not zoomed", so no
     sibling's zoom can start between this zoom and its restore. A sibling
     that read `act`/`last` before this zoom restores them to the same values
     after it. Sleep 0.3. Then unzoom **and** restore focus in **one** atomic
     command list, so a phase-aligned sibling cannot slot in between:
     `tmux if -F -t "$pane" '#{&&:#{window_zoomed_flag},#{pane_active}}' "resize-pane -Z -t $pane$restore"`.
     `$restore` is empty when `act == pane`. Otherwise it is
     ` ; select-pane -t $last ; select-pane -t $act`, where the `$last` part is
     dropped when `last` is empty or equals the target. Keep that order: last
     first, then active, so both end as they were. A comment on this block
     says not to reorder or split it.
  6. `shellcheck scripts/tmux-agent-repaint.sh` is clean, and
     `git add scripts/tmux-agent-repaint.sh` (the flake reads only tracked
     files, so Step 4's `builtins.readFile` would otherwise fail).

- [ ] **Step 4: wire the script** in `config/tmux.conf.nix`. Add
  `"tmux-agent-repaint"` to `scriptNames` and `ogInternal` (sorted as
  neighbours are). In `mkScriptIcons` add `"@agent_repaint@"` →
  `"${script.tmux-agent-repaint}/bin/tmux-agent-repaint"`. Command:
  `nix build .#default` succeeds. The built script,
  `readlink -f "$(sed -n "s|^PATH='\([^']*\)'.*|\1|p" result/bin/tmux)/tmux-update-icons"`
  (the wrapper-bin dir the wrapper prefixes to PATH), contains a `/nix/store/…-tmux-agent-repaint/bin/tmux-agent-repaint`
  path and no literal `@agent_repaint@`. Checked with
  `grep -c '@agent_repaint@'` (expected 0) and
  `grep -o '/nix/store/[a-z0-9]*-tmux-agent-repaint'` (expected a match).

- [ ] **Step 5: sweep claim** in `scripts/tmux-update-icons.sh`
  `arm_agent_detect`:
  - Insert `#{@og_birth_size}` into the row format right after
    `#{@window_manual_name}` (before the free-form `@window_ai_name`), add
    `birth` to the `read -r` list in the same position, and extend the format
    comment by one clause.
  - In the per-row loop, after the agent-command match and the `win_has`
    line, before the `live/` stamp:
    ```bash
    if [[ -n ${1:-} && -n $birth && $bridge != 1 && $REPAINT_BIN != @* ]]; then
    	tmux if -F -t "$pid" '#{@og_birth_size}' "run-shell -b -t $pid '$REPAINT_BIN #{q:pane_id} #{q:@og_birth_size}' ; set -pu -t $pid @og_birth_size"
    fi
    ```
    with `REPAINT_BIN=@agent_repaint@` defined next to the other placeholder
    vars at the top. `run-shell` must come **before** `set -pu`: `run-shell`
    expands `#{q:@og_birth_size}` when it runs, so the reverse order would hand
    the worker an empty birth size. The comment explains the sweep-only claim (per-tick runs
    once per client in the same second) and the atomicity.
  - Command: `shellcheck scripts/tmux-update-icons.sh` is clean;
    `bats tests/update-icons-modal.bats tests/update-icons-all-windows.bats
    tests/update-icons-resume-guard.bats` (with the flake's env: run them via
    `nix build .#checks.x86_64-linux.update-icons-modal-tests` etc.) stays
    green, since the row format changed.

- [ ] **Step 6: flake check** `agent-repaint-integration-tests` in `flake.nix`,
  next to `rename-bind-integration-tests`, with the same reduced conf (enrich
  and agentUsage off). `nativeBuildInputs = [bash bats coreutils gnugrep]`,
  `TMUX_BIN = "${cfg.tmux-wrapped}/bin/tmux"`, and
  `LANG`/`LC_ALL=C.UTF-8`; `cp -r ${./tests} tests; bats
  tests/agent-repaint-integration.bats`. `git add flake.nix` is not needed,
  since it is tracked, but every new file above must be staged before this
  build. Command:
  `nix build .#checks.x86_64-linux.agent-repaint-integration-tests -L`
  passes (green).

- [ ] **Step 7: red/green evidence.** Run
  `TMUX_BIN=$SP/old-tmux/bin/tmux bats tests/agent-repaint-integration.bats`.
  Expected: tests 1, 2 and 4 fail on their nudge assertion; record the
  failing assertion lines. Then run the new build,
  `TMUX_BIN=$(readlink -f result)/bin/tmux bats …`: all pass. Record both in
  the PR `## Testing`.

- [ ] **Step 8: docs.** In `docs/agents/scripts.md`, add a
  `tmux-agent-repaint` row with its driver (the sweep claim), the options it
  reads/writes (`@og_birth_size`, `window-size` save/restore), its gates
  (floats, mirrors, zoom, focus slot), the accepted gaps (float windows, a
  watched window whose target never becomes active, the remux-restore burst,
  birth = pane creation, and an empty `{last}` that ends as the target after a
  background nudge), and why (#883). Add one sentence to the
  `tmux-update-icons` row. In `docs/agents/performance.md`, add a short
  paragraph under the hook/refit-gate section: the three creation-time
  `set -pF` hooks are fork-free; the sweep adds one field and one tmux call
  per newborn agent pane; the nudge never forks a refit (zoom is gated out,
  and single-pane windows are not grids). Command: `nix build .#lint` (typos
  and markdown hooks) passes.

- [ ] **Step 9: full gate**: `nix build .#default`, `nix flake check`,
  `nix build .#lint`. All three green.

## Acceptance

- [ ] Timeline: the reproducer tables in the spec ("Measured timeline"),
  copied into the PR `## Testing`, from `$SP/repro.sh grid|newwin`.
- [ ] Regression through the production entry point: Step 7, red on
  `$SP/old-tmux`, green on the new build, plus the flake check in Step 6.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint`: Step 9
  output.
- [ ] Docs: the Step 8 diff (the `scripts.md` rows, `performance.md`).
- [ ] Upstream root cause (dispatch launches before split/refit) stated in the
  PR body and the final status.
