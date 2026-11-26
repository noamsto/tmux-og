# Spec — #1285: remote-m2 shard 2 `final_termname` flake and post-test-40 hang

Branch `feat/1285-fix-remote-m2-integration-tests-2-final`, base `origin/main` @ b3d1d3ca.
Tier deep, test-harness only (no production code change expected).

## Evidence

- CI run 45864376688 attempt 1, job 140063293890 (x86_64-linux, PR #1283), shard
  `remote-m2-integration-tests-2`:
  `not ok 24 a replacement whose dial fails leaves the mirror live and the old identity advertised in 1172ms`,
  failing only at `[ "$final_termname" = tmux-256color ]` (every earlier assertion
  passed: both presses nacked, `live = yes`, `new_transport = old_transport`).
  Tests 25–40 then pass, test 40 is the LAST test of shard 2 (`1..40`), and the
  suite hangs until the 14m `timeout` (`remote-m2 shard 2: suite exceeded 14m`).
  So the "hang after test 40" is a suite-end hang: bats never sees EOF on its
  output pipe.
- tmux server keeps bats' fd 3 (measured: a server started by `tmux -L px new-session -d`
  from a shell with fd 3 open lists `3 -> pipe:[…]` in `/proc/<pid>/fd`). A tmux
  server that outlives its test therefore holds the suite open — the same class
  #1126 fixed for leaked daemons.
- The dial-fails test (`tests/remote-m2-integration.bats:7301`) fails the replacement
  dial by renaming SRC's socket (`mv "$SOCK" "$SOCK.bak"`) and restores it
  (`mv "$SOCK.bak" "$SOCK"`) right before reading `final_termname`.
  The test-local dial is `tmux -L m2src -C attach-session -t =rem`, and
  `attach-session` carries `CMD_STARTSERVER` (already documented in
  `docs/agents/bridge-daemon.md` ~L250 and the `outage_start` comment). With the
  socket path vacated, the dial's tmux client takes the lock, `unlink(path)`s
  (`client.c:159`) and starts a fresh server whose `server_create_socket`
  `unlink`s the path again before `bind` (`server.c:121`). If the test's
  `mv` back lands inside that window, the restored, real SRC socket is unlinked:
  - `control_termname` (`$SRC list-clients …`) can no longer reach the real
    server → `final_termname` is empty → the observed assertion failure;
  - teardown's `$SRC kill-server` reaches the transient server's stale socket, not
    the real one → the real SRC server (session `rem`, `exit-unattached` off)
    survives forever, holding bats' fd 3 → the suite-end hang.
  Standalone probe (pinned tmux next-3.9, scratch `TMUX_TMPDIR`): 40 iterations of
  {server up; `mv` socket away; background `tmux -L px -C attach-session`; sleep
  0–9 ms; `mv` back} → the original server was unreachable by path afterwards in
  15/40 iterations (orphaned).
- Why the window is hit: the press nacks when the replacement is RAISED, not when
  it completes (`handleCtl`), and the test's liveness wait exits almost at once —
  `capture-pane | grep "$marker"` matches the *typed command line*
  (`sh -c 'sleep 2; printf "GXR7_LIVE_…\n"'` echoed by the shell), not the
  printf's output 2s later (CI: the whole test took 1172 ms despite the `sleep 2`).
  So `mv` back runs tens of ms after press 2, while press 2's dial is still
  starting. A second, sibling outcome of the same race: press 2's dial starts
  after the `mv` back, reaches the real server and the replacement SUCCEEDS →
  `final_termname = foot` (same assertion, no leak).

## Violated invariants

1. A test that makes the replacement dial fail must not leave a window in which a
   dial can still reach or recreate SRC's socket path while the test restores and
   reads state (the test's own oracle: "old identity advertised" is only
   meaningful while every dial still fails).
2. No scratch tmux server may outlive its test (the build's output pipe stays
   open otherwise). Today only leaked *daemons* are tripwired in teardown.
3. The liveness proof must observe output produced after the failed dials, not
   the echo of a command typed before them.

## Proposed change (tests/remote-m2-integration.bats + docs)

A. Rewrite the dial-fails test's failure lever to the existing test-only seam
   `OG_DAEMON_TEST_OUTAGE_FILE` (`--test-outage-file`, test-local only; dial argv
   becomes `false` → EOF, identity read fails → `replaceConn` returns
   `notReplaced`, same "keeping the current connection" path). SRC's socket is
   never moved, so no dial can auto-start a server and every `$SRC` read works
   throughout. Keep the outage in force until AFTER `final_termname`,
   `new_transport` and the DST liveness reads, and after `kill`/`wait` of the
   daemon, so no late dial can succeed. Rewrite the header comment accordingly.
B. Make the assertions observe the failures rather than race them: after press 1,
   poll the daemon log for the first "keeping the current connection" line
   (bounded); then press again (bounded retry, every press must nack with
   "press again") until a SECOND such line appears — proving the seam re-arms
   after a failure (the test's "repeatable, not a one-shot latch" claim, which a
   stuck in-flight raise would also have passed before, since `raiseInFlight`
   nacks identically).
C. Liveness after the failures: once both failures are logged, `send-keys` a
   printf whose OUTPUT carries the marker but whose typed text does not (e.g.
   `printf '%s_%s\n' GXR7 LIVE_$$`), and require the marker in the mirror pane
   (bounded poll). That output crosses the daemon's original connection after
   both failed dials.
D. Teardown tripwire (defense-in-depth for invariant 2, generalising #1126's daemon
   tripwire): after the `kill-server` calls, poll (bounded, ~3s, normal case one
   `ps`) for processes whose args contain `tmux` and `$BATS_TEST_TMPDIR` (every
   scratch server in this file is started with `-f $BATS_TEST_TMPDIR/*.conf`, or
   in OBS's case a pane command containing it); survivors are `kill -9`ed, named
   on fd 3, and fail the test — a future orphan becomes a named per-test failure
   instead of a 14-minute anonymous hang. Pattern kept out of awk's argv via
   ENVIRON as the daemon tripwire does.
E. Docs: `docs/agents/bridge-daemon.md` test-seam paragraph — a test that wants a
   dial to fail uses the outage file; a moved-aside socket alone races any dial
   across the move-back (auto-started server unlinks the restored path, orphaning
   SRC, which holds bats' fd 3). Note the teardown tripwire.

Out of scope: production daemon code (it behaves correctly: notReplaced on a failed
dial); park tests' `outage_start`/`outage_end` already set the outage file before
moving the socket and only remove it after moving back, so their dials are `false`
throughout (verified by reading the helpers).

## Acceptance

- Red: the old test fails deterministically when the daemon's dial is delayed past
  the `mv` back (a `tmux` PATH wrapper that sleeps before exec'ing `-C
  attach-session`, scratch dir only); the new test passes under the same wrapper.
- Mechanism probe recorded (above).
- Repeated stress: the new test passes N≥30 consecutive runs under deliberate CPU
  load; the shard-2 check `nix build .#checks.x86_64-linux.remote-m2-integration-tests-2`
  passes locally (with `--rebuild` repeats).
- Tripwire: verified to fire (named failure, no hang) on an artificially orphaned
  SRC server, and to stay silent across the whole shard.
- `nix build .#lint --accept-flake-config` and `cd picker && go test -race ./...` pass.
