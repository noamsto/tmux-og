# Plan: reply deadline for control-mode round-trips (#900)

Spec: `docs/superpowers/specs/2026-10-04-bridge-reply-deadline-design.md`
(spec-critic: accept after one revision). Plan-critic: accept after one revision.
Consult: **not consulted** — the change lives in one package
(`picker/remotebridge/daemon`) with one mechanism and one contract; a
decomposition would add no ordering/interface signal the spec does not already
carry. (Deep rule: no consult; false-negative recovery stays available if this
plan exhausts its critic cap.)
Execution model: pi has no subagents; the lead implements in its own pane.
Critic/review stages go to the `spec-critic`/`plan-critic`/`reviewer` grid panes.

## File list

| file | change |
| --- | --- |
| `picker/remotebridge/daemon/daemon.go` | `Config.ReplyTimeout/SeedTimeout` + accessors and constants; `stream` guard fields, `isClosed`, `armReply`; arm/disarm in `newRoundTrip` + `stampAll`; `ctlPump.done`/`alive` |
| `picker/remotebridge/daemon/conn.go` | `ctlConn.dead/replyTimeout/seedTimeout`, `armStall`, `live()`, close order; `dialConn` wiring; `bind` enabling; `attemptCycle` liveness gate; `replaceConn` prime/liveness gates; `primeClient` verdict |
| `picker/remotebridge/cmd/daemon/main.go` | `--reply-timeout` / `--seed-timeout` flags + env, wired into `Config` |
| `picker/remotebridge/daemon/deadline_test.go` | NEW: unit tests 1–3, attach-boundary tests 4–5 |
| `picker/remotebridge/daemon/deadline_live_test.go` | NEW: live-tmux forged-`%begin` deadline test |
| `tests/remote-m2-integration.bats` | existing suite: append the forged-`%begin` recovery test + local-session-gone variant (flake.nix's `remote-m2-integration-tests` runs this path) |
| `docs/agents/bridge-daemon.md` | residuals (1)/(3) + deadline subsection (values, poison rationale, audit, residuals) |
| `picker/remotebridge/daemon/themeprobe.go` | refresh the stale "before repair's timeout exists" comment |
| `docs/superpowers/specs/2026-10-04-bridge-reply-deadline-design.md` | NEW: the accepted spec (repo convention, like #898) |
| `docs/superpowers/plans/2026-10-04-bridge-reply-deadline.md` | NEW: this plan (repo convention, like #898) |
| `REVIEW_NOTES.md` (untracked, worktree root) | finding ledger scratch (never committed) |

## Steps

- [ ] **Step 1: Red evidence against HEAD in a scratch worktree.**
  `git worktree add --detach /tmp/og900-red HEAD`; add
  `/tmp/og900-red/picker/remotebridge/daemon/red_probe_test.go` using **HEAD-only
  APIs** (`newScriptConn`, `newCtlConn`, `bind`, `one`, `NewRouter`):
  `TestRedWedgeOffline` runs `one(c.rt, "display-message -p x")` on a
  never-answering conn in a goroutine and fails after a 5s watchdog;
  `TestRedWedgeLive` (live tmux) forges `@evil` + `refresh-client -t <cc> -B`
  and asserts the same. Prove: `cd /tmp/og900-red/picker && go test
  ./remotebridge/daemon/ -run TestRedWedge -count=1 -timeout 60s` — record both
  failure outputs (the hang is the red evidence; a compile error is not).
  Then `git worktree remove --force /tmp/og900-red`. The probe files are never
  committed.

- [ ] **Step 2: stream guard plumbing** (`daemon.go`; no behavior while the
  guard is nil). Declare `type stallGuard func(d time.Duration, what string)
  (disarm func())` here (Step 3 fills it); add to `stream`: `guard
  stallGuard`, `replyTimeout`, `seedTimeout`; `func (s *stream) isClosed()
  bool`; `func (s *stream) replyDeadline(cmd string) time.Duration` (the only
  classifier: `capture-pane ` prefix ⇒ `seedTimeout`, else `replyTimeout`);
  `func (s *stream) armReply(cmd string) (disarm func())` — nil guard or
  `d <= 0` ⇒ no-op, else `s.guard(d, fmt.Sprintf("reply deadline %v exceeded
  for %q", d, cmd))` with `d := s.replyDeadline(cmd)`. In `newRoundTrip`'s
  iterator arm before `readReplyRouting` and disarm after; in `stampAll` arm
  `write deadline %v exceeded` around `s.w.Flush()` and disarm after (both
  before any early return). Add `done atomic.Bool` to `ctlPump`, set by the
  goroutine before it returns, and `func (p *ctlPump) alive() bool`.
  Prove: `cd picker && go build ./... && go vet ./remotebridge/daemon/`.

- [ ] **Step 3: connection guard + config + close order** (`daemon.go`,
  `conn.go`). `Config.ReplyTimeout`/`SeedTimeout` with
  `defaultReplyTimeout = 30 * time.Second`, `defaultSeedTimeout = 120 *
  time.Second` and `replyTimeout()`/`seedTimeout()` accessors (0 ⇒ default).
  `ctlConn` gains `dead atomic.Bool`, `replyTimeout`, `seedTimeout`.
  `dialConn` sets both timeouts on both branches (`Ctl` and `Dial`).
  `close()` becomes: `c.dead.Store(true)` first, then `c.rwc.Close()`, then
  `c.st.close()` (comment: a fired guard must unpark a blocked Flush while
  `stampAll` holds `stream.mu`). `armStall(d, what)`:
  `time.AfterFunc(d, func(){ fmt.Fprintf(os.Stderr, "daemon: %s; dropping the
  control connection\n", what); c.close() })`; returns `func() { t.Stop() }`
  (never `t.Stop` itself — its `func() bool` does not match the disarm type),
  no-op when `d <= 0` (fills the `stallGuard` type and backs the
  `stream.replyDeadline` classifier declared in Step 2). `live()` = `!c.dead.Load() && !c.st.isClosed() && c.pump.alive()`.
  `bind` sets `c.st.guard = c.armStall`, `c.st.replyTimeout = c.replyTimeout`,
  `c.st.seedTimeout = c.seedTimeout` before rebuilding `newRoundTrip`.
  Prove: build + vet, and `go test ./remotebridge/daemon/ -run
  'TestConnHolder|TestDialConn|TestReattachBinds' -count=1`.

- [ ] **Step 4: attach-boundary liveness gates** (`conn.go`). `primeClient`
  returns `bool`: the `w <= 0 || h <= 0` early return becomes `return c.live()`
  (a zero/unset `LocalArea` is not a prime failure — the old code no-opped past
  it), then arm `c.armStall(cfg.replyTimeout(), …)` around the whole batch
  (write included), `false` when any `reply()` returns `ok == false`, else
  `c.live()`. `replaceConn`: `if !primeClient(...) { next.close(); log;
  return nil, notReplaced }` **before** `hold.close()`; after `repair()` true,
  `if !next.live() { log; return next, replaced }`. `attemptCycle`: after
  `if !repair() { return nil, cycleTerminal }`, `if !next.live() { log;
  hold.close(); continue }` — same `bo`/attempt loop, so the schedule advances
  to `cycleExhausted`/park.
  Prove: build + vet + `go test ./remotebridge/daemon/ -run
  'TestReattach|TestAttemptCycle|TestReplace|TestReopen' -count=1`.

- [ ] **Step 5: daemon flags** (`cmd/daemon/main.go`). Add
  `replyTimeout := flag.Duration("reply-timeout",
  envDurationDefault("OG_DAEMON_REPLY_TIMEOUT", 0), "test only: bound one
  control reply wait (0 = production)")` and the seed twin; set
  `cfg.ReplyTimeout`/`cfg.SeedTimeout`.
  Prove: `cd picker && go build ./... && go run ./remotebridge/cmd/daemon -h
  2>&1 | grep -E 'reply-timeout|seed-timeout'`.

- [ ] **Step 6: unit tests** (`deadline_test.go`), run and iterate:
  - 1 `TestReplyDeadlineClosesTheConnectionAndNeverReadsALateReply` — never-answering
    `scriptConn` via `dialConn(Config{Ctl: conn, ReplyTimeout: 50ms})` + `bind`;
    `one` false within ~2×; `conn.isClosed()`; a second round trip false
    immediately; a block appended after the close is never consumed.
  - 2 `TestReplyDeadlineClassSplit` — `replyDeadline` classification asserted
    deterministically, plus a wide-margin behavioral half:
    `ReplyTimeout=250ms, SeedTimeout=5s`, replies delayed ~1s — the
    `display-message` one fails (4× margin) and the `capture-pane` one
    succeeds (5×).
  - 3 `TestWriteDeadlineClosesAStalledTransport` — fake `io.ReadWriteCloser`
    whose `Write` blocks; `bind` first (else `st.guard` is nil and the test
    passes vacuously); `send` false within the timeout and the conn closed.
  - 4 `TestRepairPoisonDoesNotReportConnected` — `reattach` where every dial
    answers `identityMatch` then stalls, the repair stub issues a real bound
    round trip and returns `true`: assert dial count `Retry.MaxAttempts`, no
    `cycleConnected`, and park entered (`parkStop`).
  - 5 `TestReplacePrimeTimeoutKeepsTheOldConnection` — `replaceConn` where the
    dial answers identity then stalls: `notReplaced`, old conn still published
    and open, repair never called. Plus
    `TestReplaceZeroLocalAreaStillReplaces`: `LocalArea` 0x0 and a healthy
    dial ⇒ `replaced` (the early return is not a failure). Test 1 asserts the
    disposition through `c.live()` and `st.isClosed()` (a generic `ctlConn`
    has no `isClosed` of its own).
  Prove: `cd picker && go test ./remotebridge/daemon/ -run
  'Deadline|RepairPoison|Replace(Prime|Zero)' -count=1 -v`.

- [ ] **Step 7: live-tmux test** (`deadline_live_test.go`),
  `TestReplyDeadlineAgainstLiveForgedBegin`: `requireLiveTmux`,
  `startIsolatedTmux`, attach a control client, wrap it in `newCtlConn` +
  `bind`, forge the raw-newline `%begin` value and `refresh-client -t <cc> -B`
  from the server, then `one(c.rt, "display-message -p ok")` returns false at a
  test-scaled deadline with the transport closed.
  Prove: `cd picker && go test ./remotebridge/daemon/ -run
  TestReplyDeadlineAgainstLiveForgedBegin -count=1 -v`.

- [ ] **Step 8: bats integration tests** (`tests/remote-m2-integration.bats`).
  (`--reply-timeout 2s --seed-timeout 2s` so either class can fire.)
  - `@test "a forged unterminated %begin is bounded by the reply deadline and
    the mirror reattaches"`: start the mirror, forge as in test 6, grep the
    daemon log for `reply deadline`/`write deadline`, then `wait_bridge_state
    ""` + a live-paint check.
  - `@test "a local session that vanishes during the wedged reply ends the
    daemon"`: forge, immediately `$DST rename-session -t host-sess mirror-x`,
    then `wait_daemon_exit` (budget ≥ 300 iterations at 0.1s).
  Prove: `bats -f "reply deadline|vanishes during the wedged"
  tests/remote-m2-integration.bats`.

- [ ] **Step 9: docs** (`docs/agents/bridge-daemon.md` + the `themeprobe.go`
  comment). Rewrite residual
  (1)'s "no reply timeout" clause and residual (3) to name the deadline; add a
  subsection: values + 2 Mbit/s floor arithmetic, per-class split, poison
  rationale, attach-boundary `live()` gate, the blocking-point audit, the
  LocalTmux-exec residual, and the post-repair-wedge residual.
  Prove: `nix build .#lint` (typos/formatting) — run in Step 10.

- [ ] **Step 10: fast deterministic gate.** Scoped first:
  `cd picker && go build ./... && go vet ./remotebridge/... && go test -race
  ./remotebridge/daemon/...` — then the repo-canonical full gate:
  `nix flake check` (bats + Go lint/tests + conf assertions) and
  `nix build .#lint`. Fix and re-run until green. Record exact commands and
  results for the PR `## Testing`.

- [ ] **Step 11: code-review gate (pi grid).** Build
  `<crew_dir>/artifacts/<branch>/review.diff` (`git diff "$base_ref"...HEAD`),
  resolve the roster with `reviewer-roster --base "$base"` (or
  `resolve-roster.sh`), assign `seam: review` + roster path to the `reviewer`
  pane, await its verdict, ingest with receiving-code-review discipline, fix
  and re-request at most once more. Then post the `review:<crew>` seam marker
  (`review_mode: full`) — on pi the pane's latest verdict is the gate.

- [ ] **Step 12: finish.** Copy the accepted `spec.md`/`plan.md` into
  `docs/superpowers/specs/2026-10-04-bridge-reply-deadline-design.md` and
  `docs/superpowers/plans/2026-10-04-bridge-reply-deadline.md` (repo convention,
  #898), commit them with the code. Then `deslop` skill over the diff, deslop
  seam, pre-push peek, `git push`, `gh pr create` (closes #900,
  `tracker: github`), file
  follow-up issues if any, `pr_open` with the acceptance ledger, metrics
  snapshot, pre-done peek, `done`, release the role panes with
  `{"final":true}`.

## Acceptance checklist

| task `## Acceptance` item | step(s) | evidence |
| --- | --- | --- |
| `nix flake check` (lint, nilaway, race) passes | 10 | command + green output |
| `nix build .#lint` passes | 9, 10 | command + green output |
| timeout ⇒ drop ⇒ reconnect with backoff; no reply matched to the wrong request | 6 (tests 1, 3, 4), 8 | `go test` + bat assertions |
| `localSessionGone` (or equivalent) fires while a round-trip waits | 8 (variant) | `wait_daemon_exit` green |
| live scratch-tmux never-ending `%begin` shows recovery | 7, 8 | live Go test + bats green |

## Risk notes

- Deadline values are a false-positive/​recovery-latency tradeoff; §5 of the
  spec fixes the floor at 2 Mbit/s with ≥4× headroom on realistic bodies and
  documents the `MaxBody` cap's 1.8× margin.
- The close-order swap (transport-first) is load-bearing for the write guard;
  Step 3's comment must say why, and Step 6 test 3 fails if it regresses.
- `attemptCycle`'s new `continue` must keep the same `bo` (never rebuild the
  schedule) or the hot-loop finding returns; test 4 pins it.
