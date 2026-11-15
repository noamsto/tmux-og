# remote-m2 integration suite: bounded runtime and flake fixes (#1124)

## Findings that shape the plan

Evidence is from the 2026-11-15 CI logs and local runs of the suite under
`nix develop .#checks.x86_64-linux.remote-m2-integration-tests`.

- **Root cause of the macOS "hang": a leaked daemon holds the build's output
  pipe open.** The cancelled job `132849933793` printed all 151 tests `ok`,
  then `✔ remote-m2-integration-tests (15m01s)` at 15:21:48, the cap second.
  The passing attempt took 9m11s. The suite's own test time is about 6.6 min
  on a loaded Linux box (`bats --timing`, sum of 151 tests), so the rest is
  the derivation not finishing after the last test. Reproduced locally:
  `bats -f '<AEYE_CELL_PX drop test>'` passes in seconds, yet the `bats`
  process does not exit (observed 280 s and counting). The test never stops
  its daemon (`bridge_up`'s `daemon_pid` is registered nowhere and the test
  has no `kill`). The daemon holds fd 3 (a dup of bats's stdout, i.e. the
  nix builder's output pipe), and after teardown killed both servers it sits
  in its reconnect loop (`couldn't resolve socket directory ...`) for
  `MaxElapsed` (10 min, `daemon/backoff.go`). Nix and bats both wait for the
  pipe to close. Whether the wait ends or the 30-min job cap hits first
  depends on when in the job the suite starts: nondeterministic, silent, no
  TAP in CI until the build ends.
- **AEYE_CELL_PX "flake" (`not ok 81/82`) is the same leak, seen from the
  teardown side.** Both logs: `(from function 'teardown' ... line 109) rm -rf
  "${OG_TMUX_DIR:-}" failed: Directory not empty`. The test body passed. The
  live daemon's reattach (`attach-session` carries `CMD_STARTSERVER`) can
  start a fresh server on the vacated socket during `rm -rf`.
- **`not ok 34` (relay capability)** fails at the last assertion
  (`grep -qF "$expected" "$f2"`, line 2488) although the marker, the env flip
  and the transport check passed. Cause to be established by reproduction
  (hypotheses: a reseed or redraw interleaved into the sixel's one-second
  straddle gap; the local drop policy lagging the env publish. The code
  stores the policy before publishing the env, so the second is unlikely).
- Under load the local full run also failed `not ok 37` (AEYE_CELL_PX zero
  size, `pane_cell_px`) and `not ok 121` (86 s). Both are to be classified in
  Step 7 as in scope (same helper/wait family) or reported as follow-ups.

## File list

- `tests/remote-m2-integration.bats` - teardown reaps every background job,
  bounded stop/kill helpers, relay test fix, a suite-end leak check.
- `flake.nix` (`remote-m2-integration-tests`) - `BATS_TEST_TIMEOUT`,
  `--timing`, a `timeout` around the whole bats call.
- `picker/remotebridge/...` - only if reproduction shows the daemon is wrong
  (then a Go unit test beside it). Candidate: none yet.
- `docs/agents/bridge-daemon.md` - harness contract (settled in Step 4 by
  reading the doc).
- `docs/superpowers/plans/2026-11-15-remote-m2-suite-reliability.md` - this plan.

## Steps

- [x] **Step 1: baseline timings** (done): full `--timing` run, 151 results.
- [ ] **Step 2: reproduce the leak (red).** Command, run in the devshell:
  `bats -f 'a control-connection drop is followed by the repair
  re-publishing AEYE_CELL_PX' tests/remote-m2-integration.bats` with a
  watchdog: expect the `bats` process still alive N seconds after `ok` is
  printed, and a surviving `og-remote-bridge-daemon` whose argv contains the
  run's tmpdir. Record time-to-exit. Also loop it under CPU load for the
  `Directory not empty` teardown failure.
- [ ] **Step 3: audit table of waits and background starts.** Grep
  `tests/remote-m2-integration.bats` for: every `"$DAEMON"` background start
  (bridge_up, size_daemon_up, bridge_up_placed, ~21 inline `daemon_pid=$!`
  sites), `sampler_pid=$!`, `wait "$`, `until`/`while` loops without a
  `SECONDS` deadline, FIFO redirections (`>"$fifo"`, `mkfifo`), `$CTL` calls,
  and `ps` use (`transport_child_of`). Record each as already bounded or
  newly bounded in the PR body. Bound real gaps the way the suite does
  (deadline variable plus diagnostic to fd 3).
- [ ] **Step 4: teardown reaps every background job.** In
  `tests/remote-m2-integration.bats`: a `reap_jobs` helper called first in
  `teardown` (before the servers die): for each pid in `jobs -p` send CONT,
  TERM, poll `kill -0` for at most 10 s, then KILL, then `wait`. Covers every
  inline start site and `sampler_pid` with one change (they are all `&` jobs
  of the test shell, which teardown shares). Keep `size_daemons_stop` for the
  pid-file entries, with the same bounded stop. Verify empirically that
  `jobs -p` is non-empty in teardown, on the normal path and on the
  timeout path. Also pass `3>&-` nowhere (not needed once reaped).
  Proof: Step 2 reproduction is green (bats exits right after the last
  `ok`; no surviving daemon), and the loop of the AEYE test is green.
- [ ] **Step 5: leak check as a tripwire.** `teardown_file`: fail the suite
  naming the test if any `og-remote-bridge-daemon` whose argv contains
  `$BATS_RUN_TMPDIR` is still alive after the last teardown (kill it first so
  the build cannot stall). A leaked daemon becomes a loud failure instead of
  a silent pipe hold. Bound the teardown `kill-server` calls with `timeout`
  (external commands only: `wait` is bounded by Step 4's poll, not `timeout`).
- [ ] **Step 6: per-test bound, suite bound, fail-fast proof.** `flake.nix`:
  `BATS_TEST_TIMEOUT` sized from the slowest passing test measured on both
  platforms with margin (Linux slowest passing 21 s; darwin is 1.5-2x, so
  start at 120 s and revisit with the darwin `--timing` from PR CI), `bats
  --timing`, and `timeout` around the bats call so an overrun fails with the
  TAP so far instead of being cancelled. Proof: temporary `sleep 3600` in one
  test with `BATS_TEST_TIMEOUT=20`: expect `not ok N <name> # timeout after
  20s`, that test's teardown runs, the next test runs. Then remove the
  `sleep`. If bats' timeout does not interrupt a blocked foreground child,
  the fallback is the outer `timeout` plus TAP naming the last test.
- [ ] **Step 7: test 34 and the other load failures.** Reproduce under load
  with `--no-tempdir-cleanup`, diff the pipe file against the expected bytes,
  fix at the root cause (test, or daemon plus a Go unit test if the daemon is
  wrong). If the loop never goes red, state why and prove the invariant
  through the production path another way. Classify `not ok 37` and `121`
  from the baseline run.
- [ ] **Step 8: darwin runtime.** After Step 4, the derivation should end
  right after the last test. Push the PR, read the darwin `--timing` from CI.
  If the suite still runs near the budget, cut dominant fixed sleeps without
  removing assertions, or flag a sharding or `ci.yml` timeout change as a
  human decision. The one-process-per-derivation tail (nothing else running
  for the last ~8 min of the cancelled job) is noted in the PR.
- [ ] **Step 9: gates.** `nix build .#lint --accept-flake-config`,
  `nix flake check --no-build`, `nix build .#default`, `nix flake check`.
  Record red/green commands, counts, load method and revisions in
  `REVIEW_NOTES.md` and the PR `## Testing`.

## Acceptance

- [ ] Forced hang yields `not ok N <name>` within the bound -> Step 6 output.
- [ ] Each Linux flake has a stated root cause and a red-then-green loop
  (command and count in the PR body) -> Steps 2/4, 7.
- [ ] Unbounded waits in helpers bounded and listed in the PR -> Steps 3-5.
- [ ] `nix build .#lint`, `nix flake check --no-build`, `nix build .#default`,
  `nix flake check` pass -> Step 9.
- [ ] PR CI green on linux and darwin -> after push (Step 8).
