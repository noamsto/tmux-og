# Plan: gate the refit hooks behind in-process format checks (#793)

Spec: `docs/superpowers/specs/2026-09-27-typing-lag-resize-storm-design.md`.

## File list

| File | Purpose |
|---|---|
| `config/tmux.conf.tmpl` | Wrap the three refit `set-hook` lines (window-resized, window-resized[10], window-layout-changed) in one-line `if -F` gates; comment why. |
| `config/tmux.conf.reference.nix` | Byte-for-byte reference of the rendered conf (`tmux-conf-extraction-assertions`): mirror the three hook lines and their comments. |
| `scripts/tmux-grid-refit.sh` | Read size, tiled pane ids, geometry signature and stored `@grid_refit_sig` in one `display-message`; stamp `@grid_refit_layout` on the fast-path sig-match exit only. |
| `scripts/tmux-float-refit.sh` | Stamp `@float_refit_size` per float (`set-option -pF`) before refitting it. |
| `tests/grid-refit.bats` | Regression guard: fork-count tests against the production hook lines on a `-v` server, plus the real-resize, stale-stamp and settle cases. |
| `picker/latencyprobe/main.go` | The committed keystroke-echo probe (control-mode `send-keys` → `%output` marker, prints p50/p95/p99/max). |
| `picker/latencyprobe/main_test.go` | Unit test for `%output` octal unescaping and percentile selection. |
| `flake.nix` | Add `go test ./latencyprobe/...` to `pickerChecked`'s explicit `checkPhase` list so the probe's unit test runs in `nix flake check`. |
| `tests/perf/keystroke-latency.sh` | Harness: halo-shaped remote + bridge daemons + local server with a real client; runs the quiet/busy/churn/attach/reattach scenarios with the probe against a given `TMUX_BIN`. |
| `docs/agents/performance.md` | Method, harness usage, ranked findings, before/after table, residuals and follow-ups. |
| `docs/agents/scripts.md` | `tmux-float-refit` / `tmux-grid-refit` rows: the hook gates and the stamps they own. |
| `docs/agents/floats.md` | One line: `@float_refit_size` is float-refit's per-pane stamp; never set it at window/global level. |
| `CLAUDE.md` | Deep-dive table row for `docs/agents/performance.md`. |
| `docs/superpowers/specs/2026-09-27-typing-lag-resize-storm-design.md`, this plan | Committed alongside, per CLAUDE.md. |

Nothing outside this list changes. `flake.nix` changes only by the one
`checkPhase` line; its wiring greps (`flake.nix:530`, `:552-560`) match `set-hook -g window-resized .*/bin/tmux-float-refit #\{q:window_id\}`
etc. with `.*`, which still matches the wrapped one-line hooks, and
`grid-refit-tests` already runs `grid-refit.bats` with the pinned tmux and
`TMUX_OG_CONF`.

## Conventions for every step

- Local fast loop for the bats file (pinned tmux + this branch's rendered conf):
  ```bash
  nix build .#default -o /tmp/claude-1000/og793-branch
  RAW=$(dirname "$(readlink -f /tmp/claude-1000/og793-branch/bin/.tmux-wrapped)")
  CONF=$(grep -o -- '-f /nix/store/[^ "]*tmux.conf' /tmp/claude-1000/og793-branch/bin/tmux | head -1 | cut -d' ' -f2)
  PATH="$RAW:$PATH" TMUX_OG_CONF="$CONF" bats tests/grid-refit.bats
  ```
  (`TMUX_OG_CONF` must be rebuilt after any template/script change — the conf
  bakes store paths of the scripts.)
- The "before" conf for red runs is `/tmp/claude-1000/og793-result` (built from
  `main` 2fb6833 before any change); extract its conf the same way.
- Every scratch tmux server uses a private `TMUX_TMPDIR` and
  `CLAUDE_STATUS_DIR` (the bats `setup` already does).
- `shellcheck` + `shfmt -d` (tabs) on every edited shell file.

## Steps

- [ ] **Step 1: guard helpers in `tests/grid-refit.bats`.** Add after
  `arm_grid_hooks`:
  - `arm_refit_hooks` — like `arm_grid_hooks` but greps
    `^[[:space:]]*set-hook .*tmux-(grid|float)-refit` (both scripts' lines)
    into `$HOOKS_FILE` and `tmux source-file`s it; returns 1 when
    `TMUX_OG_CONF` is unset/missing.
  - `start_logged_server` — `tmux kill-server` the setup server, `mkdir
    "$BATS_TEST_TMPDIR/vlog"`, then `(cd "$BATS_TEST_TMPDIR/vlog" && tmux -v -f
    /dev/null new-session -d -s S -c "$TMUX_TMPDIR" -x 200 -y 50)`; re-derive
    `WIN` and re-create the 3 extra splits exactly as `setup` does, so
    `make_grid` keeps working.
  - `refit_jobs <basename>` — `cat "$BATS_TEST_TMPDIR"/vlog/tmux-server-*.log
    | grep -c "job_run: cmd=.*/$1 "` (by basename; the log writes two lines
    per job, so the tests compare counts, never absolute job numbers; prints
    0 when none and never fails the test on grep's exit 1: `|| true`).
  - `storm` — for every window (`tmux list-windows -a -F '#{window_id}'`)
    read its current `#{window_width}x#{window_height}` and build
    `refresh-client -C <win>:<WxH>` lines; feed a control client
    `{ printf 'refresh-client -C 200x50\n'; printf '%s' "$cmds"; sleep 0.5; } |
    tmux -C attach -t S >/dev/null 2>&1`. A same-size storm, the shape the
    bridge daemon produces (spec "Measurement").
  - `settle` — `sleep 1` (lets backgrounded jobs start and log).
  Proof: `bash -n tests/grid-refit.bats` exits 0; `shellcheck -s bash
  tests/grid-refit.bats` clean.

- [ ] **Step 2: failing guard tests (red on `main`'s conf).** Add, each
  skipping via `arm_refit_hooks || skip "TMUX_OG_CONF unset …"` and, where
  floats are involved, the existing "cannot create a floating pane" skip:
  1. `"a same-size resize storm forks no refit job on plain windows (#793)"` —
     `start_logged_server`; `tmux new-window -d -t S:` ×3 (plain windows);
     `arm_refit_hooks`; `settle`; record `g0=$(refit_jobs tmux-grid-refit)`,
     `f0=$(refit_jobs tmux-float-refit)`; `storm`; `settle`; assert both
     counts unchanged.
  2. `"an already laid-out grid forks no refit on a same-size storm, and settles (#793)"` —
     `start_logged_server`; `make_grid 1`; `arm_refit_hooks`; `bash "$GRID" "$WIN"`;
     wait (≤3 s poll) until `tmux show-options -wqv -t "$WIN"
     @grid_refit_layout` is non-empty (the post-apply layout event's run
     stamped it); `settle`; `g0=$(refit_jobs tmux-grid-refit)`; `storm`;
     `settle`; assert unchanged; `settle` again; assert still unchanged
     (settle case: stamp writes must not re-trigger).
  3. `"production hooks: a real resize still refits the grid and a stamped float (#793)"` —
     same body as the existing `"resize fires both indexed window-resized
     hooks"` but with `arm_refit_hooks` (production lines, store scripts)
     instead of the hand-rolled `set-hook`s; asserts `main-pane-width` 60%
     and float width ≥ 170 after `resize-window -x 200 -y 50`.
  4. `"a float opened after a shrink is refit when the window grows back (#793)"` —
     window at 200x50 (`resize-window`), `arm_refit_hooks`; create a float
     `-x 90% -y 90% -X 5% -Y 5%`, stamp `@float_geom '90% 90% 5% 5%'`;
     `resize-window -x 100 -y 30` then `-x 200 -y 50` (poll float width ≥
     170: first refit ran); kill the float; `resize-window -x 100 -y 30`;
     create a new float the same way (its width ≈ 90) and stamp it;
     `resize-window -x 200 -y 50`; poll ≤3 s; assert its width ≥ 170.
  5. `"a stamped float forks no refit on a same-size storm (#793)"` —
     `start_logged_server`; float created + stamped as in 4 on a 100x30
     window, `arm_refit_hooks`, grow to 200x50, poll width ≥ 170; `settle`;
     `f0=$(refit_jobs tmux-float-refit)`; `storm`; `settle`; assert
     unchanged.
  Command: run the local loop with `main`'s conf
  (`TMUX_OG_CONF=<conf from /tmp/claude-1000/og793-result>`). Expected: 1, 2
  and 5 FAIL on the count assertion (record the counts); 3 and 4 PASS
  (behaviour pins); every pre-existing test passes. If 1 does not fail,
  the storm is not producing events — fix the helper before proceeding.

- [ ] **Step 3: gate the hooks in `config/tmux.conf.tmpl`.** Replace lines
  460, 467 and 475 (the three `tmux-float-refit`/`tmux-grid-refit`
  `set-hook -g` lines) with, each on ONE line (`SIG` below is the geometry
  signature `#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}`, inlined literally):
  ```
  set-hook -g window-resized          { if -F '#{P:#{?#{&&:#{&&:#{pane_floating_flag},#{@float_geom}},#{!=:#{window_width}x#{window_height},#{@float_refit_size}}},1,}}' { run-shell -b "{{index .Paths.Scripts "tmux-float-refit"}} #{q:window_id}" } }
  set-hook -g window-resized[10]      { if -F '#{&&:#{==:#{@crew_grid},1},#{!=:SIG,#{@grid_refit_layout}}}' { run-shell -b "{{index .Paths.Scripts "tmux-grid-refit"}} #{q:window_id}" } }
  set-hook -g window-layout-changed   { if -F '#{&&:#{==:#{@crew_grid},1},#{!=:SIG,#{@grid_refit_layout}}}' { run-shell -b "{{index .Paths.Scripts "tmux-grid-refit"}} #{q:window_id}" } }
  ```
  and extend the comment block above them (a few lines): every
  `refresh-client -C @N` recalculates and re-fires these on every window on
  the server, so an unconditional `run-shell` was a fork storm on attach
  (#793); the gates are evaluated in-process; the stamps are owned by the
  scripts; SIG must stay byte-identical to `tmux-grid-refit`'s, and is used
  instead of `#{window_layout}` because tmux-next renders that per client
  (JSON vs legacy for a control client). Proof: `nix build .#default -o
  /tmp/claude-1000/og793-branch` succeeds; on a scratch server started with
  that wrapper, `tmux show-hooks -gw` lists all three with the `if-shell -F`
  bodies; `grep -cE 'set-hook -g window-resized .*/nix/store/[^ ]*/bin/tmux-float-refit
  #\{q:window_id\}'` on the rendered conf is 1 (flake wiring greps still
  match).

- [ ] **Step 4: stamp in `scripts/tmux-grid-refit.sh`.** Define
  `grid_sig_fmt='#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}'` near the top with a comment that
  it must stay byte-identical to the gate in `config/tmux.conf.tmpl`.
  Replace the `pane_ids=$(read_panes)` + `np` + `read -r w h
  <<<"$(tmux display-message …)"` block (lines ~67-72) with one read:
  ```bash
  IFS='|' read -r w h pane_ids geom stored_sig <<<"$(tmux display-message -p -t "$target" "#{window_width}|#{window_height}|#{P:#{?pane_floating_flag,,#{pane_id} }}|$grid_sig_fmt|#{@grid_refit_sig}" 2>/dev/null)"
  ```
  Keep the `^[0-9]+$` checks on `w`/`h`. Derive `np` from the words in
  `$pane_ids` (`read -ra ids <<<"$pane_ids"; np=${#ids[@]}`) and
  `first=${ids[0]}`. In the fast check compare `$stored_sig` (not a fresh
  `read_opt @grid_refit_sig`) and, on that `exit 0` path only, first run
  `tmux set-option -w -t "$target" @grid_refit_layout "$geom" 2>/dev/null ||
  true`. The under-lock re-check keeps `read_panes`/`read_opt` and does not
  stamp. Do not stamp on the apply path. Proof: `shellcheck
  scripts/tmux-grid-refit.sh`, `shfmt -d`; existing `grid-refit.bats`
  script-level tests (they call `$GRID` directly) pass; on a scratch server
  after `bash "$GRID" "$WIN"` twice, `show-options -wv @grid_refit_layout`
  equals `display-message -p "$SIG"` for that window **and** the same
  `display-message` issued over a `tmux -C attach` control client prints the
  identical string.

- [ ] **Step 5: stamp in `scripts/tmux-float-refit.sh`.** Inside the loop,
  after the `[[ -n $yoff ]] || continue` guard and before `resize-pane`, add
  `tmux set-option -p -F -t "$pane" @float_refit_size '#{window_width}x#{window_height}'`,
  plus a one-line comment: stamped before the refit so a resize landing
  mid-refit differs from the stamp and forks its own run; per pane so a
  float opened later has none. Proof: `shellcheck`, `shfmt -d`;
  `PATH="$RAW:$PATH" bats tests/float-refit.bats` passes (it calls the script
  directly).

- [ ] **Step 6: guard green.** Rebuild (`nix build .#default -o
  /tmp/claude-1000/og793-branch`), re-extract `CONF`, run the local bats
  loop. Expected: all of `grid-refit.bats` passes, including tests 1–5 from
  step 2. Also `nix build .#checks.x86_64-linux.grid-refit-tests
  .#checks.x86_64-linux.float-refit-tests` succeed.

- [ ] **Step 7: probe `picker/latencyprobe`.** Port the scratch probe
  (package main, stdlib only): flags `-tmux` (binary), `-L` (socket), `-t`
  (session to attach the control client to), `-pane`, `-n`, `-interval`,
  `-timeout`, `-label`; runs `tmux -L <sock> -C attach -t <sess>` with
  `TMUX` cleared; per sample writes `send-keys -t <pane> -l q<seq>z` and
  waits for the marker in that pane's `%output` (octal-unescaped, rolling
  256-byte buffer); prints `label n=… timeouts=… p50=…ms p95=…ms p99=…ms
  max=…ms`. Split `unescape` and `percentile(sorted, p)` into small funcs.
  `main_test.go` (write first): `unescape(`a\\033b\\134c`)` = "a\x1bb\\c",
  a trailing lone `\\` is kept, and `percentile` of 1..100 at 0.5/0.95/1.0 is
  50/95/100 with an empty slice giving 0. Add `go test ./latencyprobe/...` to
  `pickerChecked`'s `checkPhase` in `flake.nix` (after `go test
  ./proctree/...`). Proof: `cd picker && go test ./latencyprobe/ && go vet
  ./latencyprobe/` pass; `nix build .#checks.x86_64-linux.picker-go-tests`
  succeeds.

- [ ] **Step 8: harness `tests/perf/keystroke-latency.sh`.** Bash, `set
  -euo pipefail`; usage `tests/perf/keystroke-latency.sh [scenario...]`
  (default all: `quiet busy churn attach reattach`); env `TMUX_BIN`
  (default `./result/bin/tmux`), `SAMPLES` (default 400). It:
  - makes `W=$(mktemp -d /tmp/og-perf.XXXX)`, exports `TMUX_TMPDIR=$W`,
    `CLAUDE_STATUS_DIR=$W/status`, unsets `TMUX`; `trap cleanup EXIT INT TERM`
    where `cleanup` runs `kill-server` on sockets `rem loc obs obs2`, kills
    the recorded daemon pids, and `rm -rf "$W"` (rule 8; bounded — no waits).
  - builds `daemon`, `renderer` and `latencyprobe` with `go build` from
    `picker/` into `$W/bin`.
  - resolves the raw tmux dir for the daemons' PATH as
    `$(dirname "$(readlink -f "$(dirname "$TMUX_BIN")/.tmux-wrapped")")`
    (what `og-remote-open` inherits from the server env), falling back to
    `dirname TMUX_BIN`.
  - `W` must stay a short `/tmp/og-perf.XXXX` path (a unix socket under a
    long path fails with "File name too long").
  - writes the synthetic agent loop to `$W/agent.sh` (title OSC + 1500
    base64 bytes + spinner line every 50 ms). `start_chain <load|noload>`
    creates on socket `rem` session `work` (a `cat`
    pane, 7 plain agent windows, 3 grid windows — 3 panes each, lead pane
    runs the agent, 2 role panes run `cat`, `@crew_grid 1`, `@crew_role
    lead`/`reviewer` — and 1 window holding a floating pane `-x 60% -y 60% -X
    20% -Y 20%` stamped `@float_geom '60% 60% 20% 20%'`), and session
    `work2` (4 agent windows); on socket `loc` sessions `mirror`
    and `mirror2`; a status-drawing client from socket `obs` attached to
    `mirror`; then one daemon per remote session (`--test-local
    --src-socket rem --dst-socket loc --session work|work2 --window 1
    --local-sess mirror|mirror2 --renderer … --sock $W/dN.sock --reflow
    $(tmux -L loc show -gv @reflow_bin)`), and waits (≤10 s) for the local
    pane whose `@bridge_pane` is `%0`.
  - scenarios (each calls `start_chain` fresh, then `sleep 12` to settle,
    except `attach`): `quiet` (`start_chain noload`: only the `cat` pane in
    `work`, `work2` absent, one daemon), `busy` (`start_chain load`, probe only), `churn` (remote `new-window`/`kill-window`
    in `work` every 0.6 s ×12 in the background during the probe),
    `attach` (probe starts as soon as the first mirrored pane exists, while
    the daemons still build the mirror; also prints ms until every remote
    window is mirrored), `reattach` (a second client of 180x45 from socket
    `obs2` attaches to `mirror` for 2 s then detaches, ×3, during the probe).
    The probe types into the local mirror of remote `%0`. `churn`, `attach`
    and `reattach` also use `start_chain load`.
  Proof: `shellcheck tests/perf/keystroke-latency.sh`, `shfmt -d`;
  `TMUX_BIN=/tmp/claude-1000/og793-branch/bin/tmux SAMPLES=100
  tests/perf/keystroke-latency.sh quiet` prints one result line with
  timeouts=0 and leaves no `rem`/`loc` server or daemon behind (`pgrep -f
  "$W/bin/daemon"` empty, `ls /tmp/og-perf.*` gone).

- [ ] **Step 9: before/after numbers.** Run the full harness twice per
  build, sequentially (never concurrently — they share CPU):
  `TMUX_BIN=/tmp/claude-1000/og793-result/bin/tmux` (main, "before") and
  `TMUX_BIN=/tmp/claude-1000/og793-branch/bin/tmux` ("after"). Also, once per
  build, a verbose-log count: the harness's `busy` + `reattach` with the
  remote started under `tmux -v` (a `VLOG=1` switch in the harness that
  starts `rem` in `$W/vlog`), then count on the server log — use `grep -cE 'job_run: cmd=.*/tmux-(grid|float)-refit '`
and report jobs = lines / 2 (the `-v` log writes two `job_run` lines per
job), refit forks per scenario, before vs after. Record raw
  lines in the plan notes / PR body. Expected: after-build max during
  `reattach` and `churn` below 100 ms and refit forks near zero; report any
  residual that is not.

- [ ] **Step 10: docs.** `docs/agents/performance.md`: how to run the
  harness, the probe design (control-mode `send-keys` → `%output`, why that
  models the bridge), the load shape, the ranked findings from the spec
  (with the before/after table from step 9), the upstream recalculation
  fan-out, the residuals (daemon reconcile ~40–65 ms per window event; wrapper 32 ms per external `tmux` call)
  with their follow-up issue numbers (filled in step 12), and a "how to
  catch the next regression" note pointing at the bats guard. `CLAUDE.md`:
  add a row `| Latency, forks per event, the resize-hook gates, the probe |
  docs/agents/performance.md |`. `scripts.md`: the two refit rows gain the
  gate + stamp sentences. `floats.md`: the `@float_refit_size` line. Proof:
  `typos` via `nix build .#lint` later; `rg -n performance.md CLAUDE.md`
  finds the row.

- [ ] **Step 11: full gate.** `nix build .#default`, `nix flake check`,
  `nix build .#lint` all succeed.

- [ ] **Step 12: follow-up issues** (after `gh pr create`, per worker
  protocol "Deferred findings"): (a) daemon reconcile of a window add/remove
  stalls `%output` delivery ~40–65 ms; (b) the Nix `tmux` wrapper costs 32 ms per external call (66 PATH
  prefix blocks) — a single combined bin dir would cut it; each with the
  measured numbers and the harness command. Check `gh issue list --search`
  first.

## Acceptance mapping

| Acceptance item | Settled by |
|---|---|
| AC1 PR body has before/after p50/p95 (busy load, window add/remove burst) | Step 9 numbers (`busy`, `churn`, plus `attach`/`reattach`), pasted as a table in the PR body |
| AC2 Probe committed and re-runnable; flake regression guard | Steps 7–8 (probe + harness), steps 1–2/6 (bats guard in `grid-refit-tests`, part of `nix flake check`) |
| AC3 No behaviour change in status bar/reflow; bats + conf assertions green | No reflow/status code touched; step 6 + step 11 `nix flake check` (all bats, float/grid wiring greps, tmux-next38 hook-stored test) |
| AC4 `nix build .#default`, `nix flake check`, `nix build .#lint` pass | Step 11 |
| AC5 Upstream-dominant cost → issue instead of workaround | Step 9 decides: the dominant cost is our hooks (fixed here); the upstream recalculation fan-out is documented in step 10 and only filed if the after-build residual shows it dominating |
