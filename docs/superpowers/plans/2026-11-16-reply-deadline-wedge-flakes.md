# Fix the reply-deadline wedge flakes (#1145)

Spec: `docs/superpowers/specs/2026-11-16-reply-deadline-wedge-flakes-design.md`. Branch
`feat/1145-test-remote-m2-fix-the-reply-deadline-we`, base `origin/main`
(4964b4dd). Commit from inside the devshell (direnv) so pre-commit runs.

Orchestration consult: not tripped — the production change is one function
and its constructor at a single choke point; the rest is test and docs.

Files touched:

- `picker/remotebridge/daemon/daemon.go` — `startCtlPump` takes the closed flag, drops `Exit` once it is set.
- `picker/remotebridge/daemon/conn.go` — `newCtlConn` builds `c`, then starts the pump with `&c.dead`.
- `picker/remotebridge/daemon/daemon_test.go`, `picker/remotebridge/daemon/reconcilehello_test.go` — the two direct `startCtlPump` callers pass `new(atomic.Bool)`.
- `picker/remotebridge/daemon/deadline_test.go` — new transport double + regression test.
- `tests/remote-m2-integration.bats` — tests A (≈7620-7680) and B (≈7682-7740).
- `docs/agents/bridge-daemon.md` — the #900 bullet (line ≈621) gains the own-close `%exit` invariant.
- `docs/superpowers/specs/2026-11-16-reply-deadline-wedge-flakes-design.md`, `docs/superpowers/plans/2026-11-16-reply-deadline-wedge-flakes.md` — the accepted spec and this plan.

## Prototype evidence already in hand

Scratch copies with the pump fix, 1 CPU + 6 hogs: A (SIGSTOP-ordered) 50/50
pass, wedge 5–6 s; A at 1 s 60/60 pass on CPUs 0-3 + 4 hogs (3/50 and 1/60
before the fix). Revised-B counts are in the spec's evidence table.

Execution: Steps 1–2 Go, Steps 3–4 bats, Step 5 docs (after Steps 1–4 so it
cites the final names), Steps 6–7 measurement and gates, which change no
tracked file.

## Steps

- [x] **Step 1: regression test, red on main.** In `deadline_test.go` add a
  transport double `exitOnCloseConn` (Read: returns its script bytes, then
  blocks until Close; after Close returns `"%exit\n"` once, then `io.EOF`;
  Write discards; Close idempotent via `sync.Once`). Its doc comment: it
  models the transport's tmux client, which prints `%exit` once its input is
  closed, while our read side can still deliver that line. Add
  `TestReplyDeadlineDropIsNotARemoteExit`: script
  `"%begin 999999 999999 1\n"` (a forged block that never closes), `boundConn`
  with a 50 ms reply deadline, `one(c.rt, "display-message -p x")` must fail,
  then `c.connStreamEnded(func(l controlmode.Line, _ bool) bool { return
  l.Kind == controlmode.Exit })` must return `connDrop` (the production
  dispatch returns true only for `Exit`, `daemon.go:1608`); the verdict is
  the contract. Doc comment states the invariant (a `%exit` our own close
  provokes is not the remote ending the client), not the issue number.
  Run `cd picker && go test -count=1 -run TestReplyDeadlineDropIsNotARemoteExit ./remotebridge/daemon/`
  against unchanged production code: must FAIL with `verdict = …, want connDrop`
  (record the line). Gate: the failure is the assertion, not a build error.

- [x] **Step 2: the pump fix, green.** `startCtlPump(rd *controlmode.Reader,
  closed *atomic.Bool)`; in its loop, after `rd.Next()`, `continue` past a
  line with `l.Kind == controlmode.Exit && closed.Load()`, with a short
  comment: once the daemon has closed the connection itself, a `%exit` is the
  transport client's echo of that close (its input closed), not the remote
  ending the client, and delivering it would turn a reattachable drop into a
  teardown; a genuine `%exit` racing the close degrades to a drop.
  `newCtlConn`: build `c` without `pump`, then `c.pump = startCtlPump(rd,
  &c.dead)` before `c.watch`/`c.rt` are built. Update the two test callers to
  pass `new(atomic.Bool)` (add the `sync/atomic` import). Run
  `cd picker && go test -count=1 -race ./remotebridge/...`: green, including
  the new test and every existing `TestReplyDeadline*`/reattach test. Run
  `golangci-lint run ./remotebridge/...` and `nilaway ./remotebridge/...` from
  `picker/` (devshell).

- [x] **Step 3: test A, deterministic wedge + diagnostics.** Replace A's
  retry loop (from the first `$SRC set-option -g @og900evil …` through the
  `[ "$wedged" = yes ] || {…}` guard) with:
  1. `$SRC set-option -g @og900evil x` then the existing
     `refresh-client -t "$cc" -B 'og900evil::#{@og900evil}'` (benign value
     first, so the subscription's initial report is harmless).
  2. A comment: tmux writes a subscription change only from a 1 s timer, so
     the report cannot be ordered against the daemon's own replies; holding
     the daemon off its stream queues the split's `%layout-change` first and
     the forged `%begin` behind it within one tick, so the daemon reads the
     notification and parks in the round trip it starts for it. Nothing
     between STOP and CONT may fail (a stopped daemon would outlive the test).
  3. `kill -STOP "$daemon_pid"`; `$SRC split-window -d -t rem || true`;
     `$SRC set-option -g @og900evil "x$(printf '\n%%begin 999999 999999 1')" || true`;
     `sleep 1.5` (one 1 s tick plus margin); `kill -CONT "$daemon_pid"`.
  4. Wait for `reply deadline|write deadline` in `$log`, polling 0.1 s, budget
     40 s (comment: a starved server that writes the report after the CONT
     leaves the daemon deaf and idle, and the 30 s agent-status backstop then
     wedges it). On timeout keep the existing log dump + `false`.
  Keep the paint loop and `wait_bridge_state "" replydl "$log"`. Replace the
  bare `[ "$painted" = yes ]` with a guard that, on failure, prints to fd 3:
  `@bridge_state`, the mirror's `list-panes` (`#{pane_index}|#{@bridge_pane}|#{pane_dead}`),
  the remote's active pane id, and `tail -40 "$log"`, then `false`. Update the
  test's header comment so it no longer says the loop retries.

- [x] **Step 4: test B, robust loop.** In B: set the benign value
  (`$SRC set-option -g @og900evil2 x`) before the loop and drop the one-time
  `cc=`/`[ -n "$cc" ]`/`refresh-client` lines. Each iteration:
  1. arm every control client not armed yet: `for cc in $($SRC list-clients
     -F '#{client_name}|#{client_control_mode}' | sed -n 's/|1$//p')`; skip
     one already in the space-separated `armed` list (`case " $armed " in
     *" $cc "*) continue ;; esac`), else `$SRC refresh-client -t "$cc" -B
     'og900evil2::#{@og900evil2}' && armed="$armed $cc"`. Zero clients
     (mid-reattach) arms nothing this iteration; two (an old client lingering
     beside the new one) arms both, never a two-name `-t`. A failed arm is not
     recorded, so the next iteration retries it;
  2. `drops="$(grep -Ec 'reply deadline|write deadline' "$log" || true)"` (and the same `grep -Ec` for the later comparison);
  3. set the forged value (unchanged), `sleep 1.5`;
  4. `split="$($SRC split-window -d -P -F '#{pane_id}' -t rem 2>/dev/null || true)"`;
  5. a 10 × 0.1 s probe for the split's id among
     `$DST list-panes -t host-sess:1 -F '#{@bridge_pane}' 2>/dev/null` (`grep -qx -- "$split"`; keep the redirect, the mirror window churns during a reattach),
     `wired=yes` and break on a hit;
  6. parked only if `$split` is non-empty, `wired=no`, and the deadline count
     still equals `drops`; else `[ -z "$split" ] || $SRC kill-pane -t "$split" 2>/dev/null || true`
     and iterate. `src_n`/`dst_n` go away.
  Rewrite the block comment: why 1.5 s (one tick plus margin, so the report
  lands before the split and the split's layout read is the held wait), why
  re-arm (a wedge the probe cannot see — the report landing after the new
  pane is wired, while its seed is in flight — ends in a deadline and a fresh
  control client that carries no subscription), why the deadline-count check
  (a mismatch seen during a reattach is not a wedge), why the kill (room for
  the next split), why identity and not counts (after a kill against a deaf
  daemon the counts can match while the mirror is stale). Keep the rename, the 300-tick `wait_daemon_exit` and the
  final comment.

- [x] **Step 5: docs.** In `docs/agents/bridge-daemon.md` #900 bullet, after
  the sentence about the read wrapper discarding the synthesized `End`, add:
  the same holds for `%exit`: once the daemon has closed a connection itself
  (`dead`, set before the transport is touched), the pump drops a `%exit`,
  since the transport client prints one when its input closes and the reader
  hands it over at top level or, held in an open block, at the EOF the close
  causes; delivered, `dispatch` would read it as the remote ending the client
  and tear the mirror down instead of reattaching. A genuine `%exit` that
  races the close degrades to a drop, and the reattach then ends on the
  refused attach. Pinned by `TestReplyDeadlineDropIsNotARemoteExit`. Copy the
  accepted spec and this plan into `docs/superpowers/specs/…-design.md` and
  `docs/superpowers/plans/…md` (drop scratch-only paths from both).

- [x] **Step 6: measure the repo version (evidence for the PR).** Build the
  worktree's tools (`nix build .#go-tools`) and run the
  repo's own `tests/remote-m2-integration.bats` (no instrumentation) through
  a scratch harness (private tmux tmpdir per run, pinned tmux,
  sandbox-like shell, `taskset` + `stress-ng`), recording `uptime` before and
  after each batch:
  - A: 60 runs, CPUs 0-3 + 4 hogs, 8 parallel; 50 runs, 1 CPU + 6 hogs.
  - B: 50 runs, 1 CPU + 6 hogs; and B with `--reply-timeout 10m --seed-timeout 10m`
    (scratch copy): 50 runs, must all fail (sensitivity: a pass there would be
    a live daemon read as parked, exiting through the #680 probe).
  - B's loop with the beat forced to 0.3 s (scratch copy): 50 runs, 0 failures
    (shows the re-arm recovers unseen wedges).
  Target 0 failures for A and B. Any failure → stop, diagnose from the stats,
  back to the relevant step.

- [x] **Step 7: gates.** `nix build .#lint --accept-flake-config`,
  `nix flake check --no-build`, `nix flake check` (includes the four
  `remote-m2-integration-tests-<n>` shards and `remote-m2-shard-coverage`),
  `nix build .#default`. Commit in logical commits (daemon fix + Go test;
  bats; docs/spec/plan).

## Evidence carried into the PR

Before/after rates (spec tables) with the harness parameters stated (taskset CPUs, stress-ng hogs, parallelism, pinned tmux-next-3.9, per-run private TMUX_TMPDIR, sandbox-like shell), the Go red line from Step 1, the daemon-log
lines of the reproduced failures, Step 6's rates with load, and the gate
results, one line each under `## Testing`.
