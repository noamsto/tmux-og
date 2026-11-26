# Plan — #1285: remote-m2 dial-fails race and shard-2 hang (spec: ../specs/2026-11-26-remote-m2-dial-fails-race-design.md)

Scope: `tests/remote-m2-integration.bats` (one test body + teardown),
`docs/agents/bridge-daemon.md` (test-seam paragraph), committed plan/spec docs
under `docs/superpowers/{plans,specs}/2026-11-26-remote-m2-dial-fails-race{,-design}.md`.
No production Go change. No flake.nix change (shard assignment is by test name +
weight; the test keeps its exact name, so it stays in shard 2 and its weight entry
in `tests/remote-m2-weights.json` still matches).

Dev env for every bats run (no `go build`, pinned tmux next-3.9 on PATH):
`nix develop .#checks.x86_64-linux.remote-m2-integration-tests-2 --accept-flake-config -c …`,
always with `env -u TMUX`, a fresh `d=$(mktemp -d)` for any scratch, and
`CLAUDE_STATUS_DIR=$d/status XDG_DATA_HOME=$d/data` on the command line (the
suite's setup already sets its own `TMUX_TMPDIR=/tmp/og-m2-bats-$$`).

## Step 1 — red evidence on the unmodified test (lead, read-only on the tree)

1a. Mechanism probe (done during spec, recorded): socket mv-away / bg
    `tmux -C attach-session` / mv-back → 15/40 orphaned.
1b. Old test, statistical red under starvation: copy the unmodified file to the
    scratch dir; add a PATH wrapper dir in `$d/bin` with a `tmux` script that,
    only when its argv contains `-C attach-session`, sleeps a random 0–80 ms
    (`$RANDOM`) and then `exec`s the real tmux (absolute path resolved before
    PATH is prefixed) — models a slow dial start, legal on a loaded runner. Run
    `bats --filter 'a replacement whose dial fails' <copy>` N=30 times under
    `systemd-run --user --scope -p CPUQuota=30%` (throttles the test's own
    process tree; no load generator), recording per run: TAP result, and whether
    a `tmux … m2src …` server process from that run survived teardown (the
    orphan → the hang ingredient; reap it by recorded pid if so). Expected: some
    `not ok` at `final_termname` and/or survivors. If 0/30, record that the
    counterfactual did not reproduce locally and rely on 1a + the CI log
    (reviewer to assess, per EVIDENCE_REVIEW).

## Step 2 — rewrite the dial-fails test (execute subagent; file: tests/remote-m2-integration.bats, the `@test "a replacement whose dial fails leaves the mirror live and the old identity advertised"` body and its header comment only)

Keep the test name byte-identical. New body:
1. Same setup: `$SRC new-session -d -s rem -x 100 -y 30`, `$DST new-session -d -s host-sess -x 100 -y 30`,
   `attach_pty_client tmux-256color`, `bridge_up 1 gxr7`, `pane`, `old_transport` (non-empty),
   `attach_pty_client foot`.
2. Fail every dial from here on: `: >"$OG_DAEMON_TEST_OUTAGE_FILE"` (the existing
   seam; SRC's socket is never moved).
3. A local helper (inline in the test or a small function right above it)
   `gxr7_failures` = `grep -c 'keeping the current connection' "$BATS_TEST_TMPDIR/gxr7.log"` (0 when absent).
4. Press 1: `run "$CTL" --sock "$sock" carousel "$pane"`; record status/output.
   Poll (≤ 10 s, 0.1 s steps, SECONDS-based deadline like `wait_bridge_state`) for `gxr7_failures ≥ 1`.
5. Repeat presses (each `run`, each status/output recorded and later asserted
   non-zero + `*"press again"*`), at most 10, each followed by a ≤ 2 s poll, until
   `gxr7_failures ≥ 2` — proves a press after a failed replacement raises (and
   fails) again; a stuck in-flight seam would nack forever without a second dial.
6. Liveness after both failures: `$SRC send-keys -t rem "printf '%s_%s\\n' GXR7 LIVE_$$" Enter`;
   poll `$DST capture-pane -p -t host-sess:1.0` for `GXR7_LIVE_$$` (≤ 40 × 0.15 s).
   The typed text never contains the joined marker, only the printf output does.
7. With the outage STILL in force: `new_transport="$(transport_child)"`,
   `final_termname="$(control_termname)"`, `$DST has-session -t '=host-sess'` status.
8. `kill`/`wait` the daemon, `tmux -L m2obs kill-server`, then `rm -f "$OG_DAEMON_TEST_OUTAGE_FILE"`.
9. Assertions: both failure counts reached (≥1 then ≥2), every press nacked with
   "press again", `live = yes`, `new_transport = old_transport`,
   `final_termname = tmux-256color`, DST alive. On a poll timeout dump
   `tail -40 gxr7.log >&3` before failing (pattern used elsewhere in the file).
Header comment rewritten: why the outage file, not a moved socket
(`attach-session` is `CMD_STARTSERVER`; a dial straddling a move-back unlinks
the restored socket and orphans SRC, which holds bats' fd 3 — #1285), why the
outage stays in force through the reads, why the marker is split.
Gate: `shellcheck` via the lint build later; quick run: `bats --filter 'a replacement whose dial fails'` in the dev env, green.

## Step 3 — teardown tripwire for leaked scratch tmux servers (execute subagent; file: tests/remote-m2-integration.bats `teardown()` only)

After the existing `kill-server` lines and alongside the daemon tripwire (both
computed before either fails; `rm -rf "$OG_TMUX_DIR"` still always runs; final
status `[ -z "$leaked" ] && [ -z "$leaked_tmux" ]`):
- bounded poll (≤ 30 × 0.1 s; normally one `ps` pass) for processes where
  `ps -Aww -o pid=,args=` field 2 (argv0) is `tmux` or ends in `/tmux`, AND
  `index($0, ENVIRON["TEST_DIR"])` (`$BATS_TEST_TMPDIR`); pattern via ENVIRON,
  as the daemon tripwire. Covers SRC/DST/DST2 (started `-f $BATS_TEST_TMPDIR/*.conf`)
  and OBS (its pane command embeds `$DST`), and the OBS pane's own tmux clients
  while they finish dying (hence the poll).
- survivors: `kill -9`, `printf 'teardown: leaked tmux pid(s) %s(args …)\n' >&3`, fail.
Comment ties it to #1285 / #1126 (a server holding fd 3 hangs the suite with no
name; this makes it a named per-test failure).

## Step 4 — docs (execute subagent, after 2–3; files: docs/agents/bridge-daemon.md + the two docs/superpowers files)

- bridge-daemon.md, the test-seam paragraph around the `CMD_STARTSERVER` /
  `OG_DAEMON_TEST_OUTAGE_FILE` text (~L250): a test that needs the dial to fail
  uses the outage file; a moved-aside socket ALONE lets a dial straddle the
  move-back and unlink the restored path (`client.c` / `server.c` unlink before
  bind), orphaning SRC, which holds bats' fd 3 → suite-end hang (#1285); the
  teardown tmux tripwire turns any such orphan into a named failure.
- Commit spec.md as `docs/superpowers/specs/2026-11-26-remote-m2-dial-fails-race-design.md`
  and this plan as `docs/superpowers/plans/2026-11-26-remote-m2-dial-fails-race.md` (lead copies).

## Step 5 — verification (lead)

5a. Tripwire positive control (scratch only, never committed): a copy of the
    file with one extra test that starts `$SRC new-session`, then moves SRC's
    socket aside so teardown's kill-server misses it → expect `not ok` with the
    `leaked tmux` line, and bats returns (no hang; wrapped in `timeout 120`).
5b. Green under the step-1b harness (same wrapper, same CPUQuota, N=30) on the
    NEW file: 30/30 ok, zero surviving servers.
5c. Stress the new test plain (no wrapper) under CPUQuota=30%, N=30: 30/30.
5d. `nix build .#checks.x86_64-linux.remote-m2-integration-tests-2 --accept-flake-config`,
    then `--rebuild` twice: green, no 14m hang.
5e. `nix build .#lint --accept-flake-config`; `cd picker && go test -race ./...`
    (acceptance; Go untouched).
5f. `nix build .#checks.x86_64-linux.remote-m2-shard-coverage` (test name/shard map unchanged).

Commit after steps 2–4 in one commit (`test(remote-m2): fail the replacement dial with the outage file, tripwire leaked tmux servers (#1285)`), before review.

## Outcome (recorded evidence)

Every local bats run unset `TMUX` and used scratch `HOME`, `XDG_CONFIG_HOME`,
`CLAUDE_STATUS_DIR` and `XDG_DATA_HOME`. Without `XDG_CONFIG_HOME` pointing at
an empty dir, the dial's auto-started server (and OBS, which has no `-f`) loads
the developer's own tmux config. That makes each auto-start take ~0.7 s, so
press 2 never dials and the race disappears locally. It also writes into the
shared `/tmp/claude-status-<uid>`.

- Timeline trace of the old test (CI-like env): press 2's dial started 28 ms
  before the `mv` back; the liveness poll exited 7 ms after starting (it
  matched the typed command's echo).
- Old test, dial start jittered 0–39 ms by a PATH `tmux` wrapper, 40 runs:
  37 ok, 3 `not ok … line 7373 [ "$final_termname" = tmux-256color ]` (the CI
  line). Each of the 3 left bats hung, with an orphaned
  `tmux -L m2src -f …/src.conf new-session` server holding the pipe bats reads.
- New test, same harness, 40 runs: 40 ok, 0 hung, 0 surviving servers.
- New test under `systemd-run --user --scope -p CPUQuota=30%`, 30 runs: 30 ok.
- Tripwire positive control: an extra test orphans SRC (moves its socket to
  `.bak`). With the old teardown the test reports `ok` but bats hangs until
  the orphan is killed. With the new teardown it is `not ok`, the
  `teardown: leaked tmux server(s)` line names the server, and bats returns
  in 5 s.
- `nix build .#checks.x86_64-linux.remote-m2-integration-tests-2` passed, and
  passed again on 2 `--rebuild` runs (`ok 24 … in 639ms` / `650ms`).
  `remote-m2-shard-coverage`, `nix build .#lint` and
  `picker-go-tests` / `go test -race ./...` (Go untouched) all passed.
