# Plan: replace polling with tmux events where it removes work (#1000)

Spec: `docs/superpowers/specs/2026-11-02-event-hooks-design.md` (survey of
every polling site, decisions, designs). Two sites change: agent-detect's
geometry poll becomes a `pane-resized` wait, and `tmux-agent-repaint`'s
readiness wait reads `#{pane_last_output_time}`. Everything else is kept with
its reason in the spec.

## File list

| File | Purpose |
| --- | --- |
| `picker/agentdetect/geometry.go` (new) | `geometryWaitArgs`, `runGeometryRound` (one `display ; wait-for -E` client), `watchGeometry` (round loop, settle delay, reject count) |
| `picker/agentdetect/geometry_test.go` (new) | Unit tests for `watchGeometry` with a fake round; live client-less scratch-server test for the real round |
| `picker/agentdetect/main.go` | Start the watcher goroutine with a cancelled-on-return context; consume geometry reads; poll with `paneInfo` only after fallback; keep `stillOwner` on the 5 s tick; update the `geometryInterval` comment |
| `scripts/tmux-agent-repaint.sh` | Readiness wait: one `display` per check reading `pane_last_output_time` together with the info fields the nudge needs |
| `tests/agent-repaint-ready.bats` (new) | Client-less scratch-server tests for the readiness read and the nudge; tmux-call counter via a logging `tmux` wrapper on `PATH`. Complements the existing end-to-end `tests/agent-repaint-integration.bats` (sweep → claim → nudge), which stays unchanged and must stay green |
| `flake.nix` | `checks.agent-repaint-ready-tests` running the new bats file with `mkTmux` |
| `tests/perf/agent-detect-forks.sh` (new) | Fork/RSS harness: idle, resize series, drag |
| `docs/agents/performance.md` | New section: survey summary, decisions, before/after numbers, regression guards |
| `docs/agents/scripts.md` | `tmux-agent-repaint` row: readiness read |
| `docs/agents/agent-state.md` | Watcher geometry: event-driven with poll fallback |
| `docs/agents/tmux-gotchas.md` | `wait-for -E` flag order and payload key names |
| `docs/superpowers/specs/2026-11-02-event-hooks-design.md`, this plan | Committed with the code |

Nothing under `picker/remotebridge/` changes.

## Steps

- [ ] **Step 1: failing unit tests for `watchGeometry`** — `picker/agentdetect/geometry_test.go`.
  Signature the tests pin (package `main`):
  `type geom struct{ cols, rows int }`;
  `type geometryRound func(ctx context.Context, report func(geom)) (printed bool, err error)`;
  `func watchGeometry(ctx context.Context, round geometryRound, out chan<- geom, settle time.Duration) (rejected bool)`;
  `const geometryRejectLimit = 3`.
  Cases, each with `settle = time.Millisecond` and a fake round driven by a scripted slice of `{geom, printed, err}`:
  1. three rounds `printed=false, err!=nil` → returns `true` after exactly 3 calls, nothing sent;
  2. two rejects, then a printing round → no fallback; the geometry is sent; a further two rejects plus a printing round still no fallback (the count reset);
  3. printing round returning `err != nil` (pane gone) → count reset, loop continues;
  4. every reported geometry arrives on `out` in order;
  5. ctx cancelled while blocked on `out` or inside a round → returns `false` promptly (≤ 1 s), no goroutine left (the test waits on the return).
  Command: `cd picker && go test ./agentdetect/ -run TestWatchGeometry` → fails to compile (symbols missing).

- [ ] **Step 2: failing live test, client-less** — same file, `TestGeometryWaitFiresWithoutClients`.
  `TMUX_TMPDIR` is a short `os.MkdirTemp("/tmp", "og")` (removed in `t.Cleanup`), not `t.TempDir()`: on the darwin build `t.TempDir()/tmux-<uid>/default` can pass the ~104-byte AF_UNIX limit. Default socket, as the existing live test uses. `new-session -d -x 80 -y 24 "sleep 600"` (no client ever attaches), read the pane id, `ctx` with `t.Cleanup(cancel)`, start `watchGeometry(ctx, runGeometryRound(paneID), out, 50*time.Millisecond)` in a goroutine, receive the first read and require `80x24` (window `-x 80 -y 24`, one pane). Then `resize-window -t <win> -x 100 -y 30` and require `100x30` on `out` within 2 s (the old poll's bound was 5 s). Then assert `wait-for -l -E pane-resized` lists one waiter; cancel `ctx`, and within 2 s it lists none (the killed client's sink is gone). Also assert `list-clients` is empty throughout (the waiter is not an attached client). If `new-session` fails: `t.Fatal` when `OG_REQUIRE_TMUX` is set (the `pickerChecked` checkPhase sets it, as `sessionres_test.go` relies on), else `t.Skip` — so the client-less proof cannot silently skip in `nix flake check` on either platform.
  Command: `cd picker && go test ./agentdetect/ -run TestGeometryWaitFiresWithoutClients` → fails to compile.

- [ ] **Step 3: implement `geometry.go`** — `picker/agentdetect/geometry.go`. (implement: escalated)
  - `geometryWaitArgs(paneID string) []string` returns
    `display -p -t %N "#{pane_width} #{pane_height}" ; wait-for -E -F "#{==:#{pane},%N}" pane-resized` as argv (`";"` its own element; flags before the event name — after it they are a parse error).
  - `runGeometryRound(paneID string) geometryRound`: `exec.CommandContext(ctx, "tmux", geometryWaitArgs(paneID)...)` with the same `//nolint:gosec` reason as `paneInfo`; `StdoutPipe`; read the first line; parse with the existing `parsePaneInfo` rule (two ints; a dead pane's empty fields are not a geometry); call `report` on success and set `printed`; drain the rest; return `cmd.Wait()`'s error.
  - `watchGeometry`: loop until `ctx.Done()`: run a round; forward each report to `out` with `select` on `ctx.Done()`; `printed` resets the reject count; `!printed && err != nil` increments it and at `geometryRejectLimit` returns `true`; `!printed && err == nil` (a wake whose display read was not a geometry) neither counts nor resets; then wait `settle` (select on `ctx.Done()`) and loop. Return `false` on cancel.
  - Doc comment states the no-gap invariant (the read and the wait are one command list), the version gate (parse rejection on a resident server, #407; never `tmux -V`), and the residuals: a SIGKILLed watcher's waiter lingers until that pane resizes or the server exits; and a resident server built from a pin between upstream d29aa121 (`wait-for -E` added) and 0e1f6ed8 (`cmd_wait_for_client_lost`, present in the current pin) keeps a killed waiter's wait item and client struct until that pane next resizes or the server restarts — the parser cannot tell those builds apart (all report `next-3.x`).
  Command: `cd picker && go test -race ./agentdetect/ -run 'TestWatchGeometry|TestGeometryWaitFiresWithoutClients'` → PASS.

- [ ] **Step 4: wire it into the watcher loop** — `picker/agentdetect/main.go`. (implement: escalated)
  - Constant `geometrySettle = time.Second` beside `geometryInterval`; rewrite the `geometryInterval` comment: it is now the ownership tick plus the fallback poll, and the event is primary.
  - After `registerWatcher` succeeds: `ctx, cancel := context.WithCancel(context.Background())`, `defer cancel()`; `geomC := make(chan geom)`; `fallbackC := make(chan struct{})`; `go func() { if watchGeometry(ctx, runGeometryRound(paneID), geomC, geometrySettle) { close(fallbackC) } }()`.
  - New select arms: `case g := <-geomC:` → if `g.cols != curCols || g.rows != curRows` then `reseed(); emit(scr, m, w)`; `case <-fallbackC:` → `polling = true; fallbackC = nil`.
  - `case <-geometry.C:` keeps `stillOwner`; it runs the `paneInfo` comparison on every tick `if polling`, and in event mode on every `geometryBackstopTicks`-th tick (constant `12`, i.e. 60 s) — insurance against a waiter that blocks but never matches on some build, which the parse-rejection fallback cannot detect. A tick counter in the loop, reset when the comparison runs.
  - Every existing `return` path is covered by `defer cancel()`; no other behaviour changes.
  Commands: `cd picker && go test -race ./agentdetect/...` → PASS; `cd picker && golangci-lint run ./agentdetect/... && nilaway ./agentdetect/...` → clean.

- [ ] **Step 5: failing bats tests for the readiness read** — `tests/agent-repaint-ready.bats`.
  Setup: a short fixed `TMUX_TMPDIR=/tmp/ogar-$$-$BATS_TEST_NUMBER` (AF_UNIX path limit, as `agent-repaint-integration.bats` does) and a private `CLAUDE_STATUS_DIR` per test, the default socket (the script calls bare `tmux`), `-f /dev/null`, no client. The nix sandbox has no `/usr/bin`, so the script's shebang cannot resolve there: every run is `bash scripts/tmux-agent-repaint.sh …` (as `carousel-guard.bats` runs its guard), never direct. A logging wrapper `$BATS_TEST_TMPDIR/bin/tmux` (first on `PATH`) is written at setup with an absolute shebang (`#!$(command -v bash)`, as `carousel-guard.bats` writes its recorder) and the real tmux resolved to an absolute path before `PATH` changes; it appends its argv to `calls.log` and execs that tmux. `set-hook -g pane-resized 'set -gF @resizes "#{e|+:#{@resizes},1}"'` counts real size changes. Teardown `kill-server` on that server only.
  1. *Quiet pane nudges once ready*: one-pane window running `sh -c 'printf hi; exec sleep 600'`; assert `#{pane_last_output_time}` reads non-empty (so the new read is exercised, not the empty-means-quiet path); truncate `calls.log` (setup queries go through the wrapper too); run `bash scripts/tmux-agent-repaint.sh <pane> 1x1`; assert exit 0, `@resizes` = 2, the pane is back at its original size, and `calls.log` holds exactly one `display` whose format contains `pane_last_output_time` before the first `resize-window`.
  2. *Busy pane waits for quiet*: the pane prints a line every 0.2 s for 6 s, then sleeps; run the script in the background; at 5 s after start assert `@resizes` is still unset; then within 12 s assert it reaches 2.
  3. *Never-output pane counts as quiet*: pane `sleep 600` (empty `pane_last_output_time`), script nudges after the 3 s startup sleep (`@resizes` = 2 within 6 s).
  4. *Unchanged birth size is a no-op*: birth equals the current size → exit 0, `@resizes` unset.
  Command: `bats tests/agent-repaint-ready.bats` (devshell `tmux` is the pinned build) → case 1 fails on the call-log assertion (today's script reads `pane_output_generation` twice); others may pass.

- [ ] **Step 6: rewrite the readiness wait** — `scripts/tmux-agent-repaint.sh`.
  Replace the generation pair and the separate `info` read with one loop: `info=$(tmux display -p -t "$pane" "$INFO")`, `INFO` = `#{pane_last_output_time}|` + today's info fields; `IFS='|' read -r last pw ph wid ww wh np floating bridge floats`; a non-numeric or empty `last` counts as 0 (never-output pane, or a resident server predating the format); `quiet_at=$((last + 2))` (one-second resolution: two seconds past the stamp is at least one quiet second); break when `EPOCHSECONDS >= quiet_at` or the 12 s cap is reached; else `sleep $((quiet_at - EPOCHSECONDS))`. The read keeps `2>/dev/null) || exit 0`: the pane can die during the wait. Keep the 3 s startup sleep and everything after the read unchanged. Update the header comment. The readiness bar moves from exactly 1 s of quiet to 1–2 s; `agent-repaint-integration.bats`'s `sleep 12` slack (sweep 5 + ready 3 + quiet) still covers it.
  Commands: `bats tests/agent-repaint-ready.bats` → all pass; `TMUX_BIN=./result/bin/tmux bats tests/agent-repaint-integration.bats` → still passes; `shellcheck scripts/tmux-agent-repaint.sh` and `shfmt -d scripts/tmux-agent-repaint.sh` → clean.

- [ ] **Step 7: register the bats check** — `flake.nix`: `checks.agent-repaint-ready-tests` modelled on `carousel-guard-tests` (`nativeBuildInputs = [pkgs.bats pkgs.coreutils (mkTmux pkgs)]`, copies `scripts` and `tests`, `HOME=$TMPDIR`, `bats tests/agent-repaint-ready.bats`).
  Command: `nix build .#checks.x86_64-linux.agent-repaint-ready-tests .#checks.x86_64-linux.agent-repaint-integration-tests --accept-flake-config` → both build.

- [ ] **Step 8: finish the fork harness** — `tests/perf/agent-detect-forks.sh` (already drafted: idle + resize series). Add the drag phase (every window resized at 10 Hz for 3 s, alternating widths) and report its watcher calls with the harness's own resize calls subtracted; report the RSS of the waiting `tmux` clients (sum of `VmRSS` over the children of the watcher pids, read from `/proc`; reported as `n/a` where `/proc` is absent).
  Command: `shellcheck tests/perf/agent-detect-forks.sh && tests/perf/agent-detect-forks.sh <binary>` → prints idle, resize, drag and RSS lines.

- [ ] **Step 9: measure before and after.**
  - Build `main`'s `agent-detect` (`git archive main picker | tar -x -C <scratch>`, `go build ./agentdetect`) and this branch's; run the fork harness twice per build, alternating, recording `uptime` per run.
  - `nix build .#default` on `main` (into a scratch out-link) and on this branch; `TMUX_BIN=<build> SAMPLES=400 tests/perf/keystroke-latency.sh quiet busy`, two rounds per build, alternating.
  - Count the tmux calls per nudge from the bats call log on both scripts (case 1).
  Evidence: numbers saved under the scratchpad; summary in the PR's `## Testing` and `performance.md`.

- [ ] **Step 10: docs** — `performance.md` gains a "Polling vs events (#1000)" section: the decisions table summarised (link to the spec for the full survey), the before/after tables, the drag-burst trade and waiter RSS, and the regression guards (Step 2's test, `agent-repaint-ready-tests`, the existing `agent-repaint-integration-tests`). `scripts.md`'s `tmux-agent-repaint` row: readiness read. `agent-state.md`: one line on watcher geometry (event, fallback poll, 30 s liveness unchanged). `tmux-gotchas.md`: `wait-for -E` flags precede the name; payload keys are bare (`#{pane}`).
  Command: `nix build .#lint --accept-flake-config` → passes (typos, formatting).

- [ ] **Step 11: full gate** — `nix build .#default --accept-flake-config`, `nix flake check --accept-flake-config` (includes `agent-repaint-integration-tests` and `agent-repaint-ready-tests`), `nix build .#lint --accept-flake-config` → all pass.

## Acceptance

- [ ] Spec/plan listing every polling site with a decision and reason → `docs/superpowers/specs/2026-11-02-event-hooks-design.md` (site table) and this plan, committed.
- [ ] Before/after fork and latency numbers from the `performance.md` harness → Step 9 output in the PR's `## Testing` (`agent-detect-forks.sh` idle/resize/drag/RSS, `keystroke-latency.sh quiet busy`, calls per nudge).
- [ ] Each replaced site works on a client-less server → `TestGeometryWaitFiresWithoutClients` (no client, resize reaches the watcher within 2 s; waiter not in `list-clients`; fatal, not skipped, under `OG_REQUIRE_TMUX`) and `tests/agent-repaint-ready.bats` (no client, nudge after quiet, with a non-empty `pane_last_output_time`), plus the existing end-to-end `agent-repaint-integration-tests`, all in `nix flake check`.
- [ ] `nix build .#lint --accept-flake-config`, `nix flake check`, `nix build .#default` pass → Step 11; CI green on the PR.
