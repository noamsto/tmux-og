# Fix the reply-deadline wedge flakes (#1145) — design

Two remote-m2 integration tests that wedge the bridge daemon with a forged,
never-terminated `%begin` fail intermittently in CI:

- **A** `a forged unterminated %begin is bounded by the reply deadline and the mirror reattaches`
  (`tests/remote-m2-integration.bats:7627`): main run 43871384224 (x86_64-linux,
  4964b4dd), shard 2, at `[ "$painted" = yes ]` after 58348 ms. No daemon log in
  the CI output (the assertion dumps nothing).
- **B** `a local session that vanishes during a wedged reply ends the daemon`
  (`:7686`): main run 43842917456 (aarch64-darwin, e5545f76), shard 2, at the
  `[ "$parked" = yes ] || { …; false }` guard after 42031 ms.

Correction to the issue text: #1135 (e5545f76) changed **both** tests' settle
beat from `sleep 1` to `sleep 0.3` (`git show e5545f76`, hunks at old 7577 and
7637); B's single post-split sample also became a 10 × 0.1 s poll.

## Mechanisms (measured, not inferred)

Facts used below, each checked in source:

- tmux writes a `%subscription-changed` only from a per-client 1 s timer
  (`monitor.c` `monitor_timer`, `tv_sec = 1`), so a forged report lands up to
  1 s after the `set-option`, at a phase the test cannot see.
- `controlmode.Reader` closes a block only on a guard repeating the `%begin`'s
  fields (`parse.go` `inBlock`), so `%begin 999999 999999 1` makes the daemon
  deaf to everything after it; the first round trip it then waits on parks
  until the reply deadline fires.
- An idle, deaf daemon starts no round trip until `agentStatusBackstopInterval`
  (30 s, `agentstatus.go:26`).

### A: a daemon bug — a deadline drop can tear the mirror down instead of reattaching

Reproduced locally: A at `sleep 1` failed 3/50 under load (and 1/60 with an
instrumented daemon). In every failure the **local** mirror server was gone
(`no server running on …/m2dst`) and the daemon log ends
`reply deadline 2s exceeded …; dropping the control connection` →
`size owner: $0: env teardown skipped: read failed` — a teardown, no reattach.
The instrumented daemon printed, in the failing run only:
`DBG connEnd: async drain at stream end stalled=true`.

Chain (code references on main):

1. The guard fires: `armStall` marks the stream stalled, then `c.close()` →
   `child.Close()` closes the transport child's stdin, then our read end
   (`cmd/daemon/main.go:966`).
2. The child (`tmux -C` client; ssh in production) sees EOF on stdin and prints
   `%exit`. If it does so before our read end closes, the pump reads it.
3. The reader returns it as an `Exit` line: at top level as-is
   (`parse.go` `topLevel`), and inside the open forged block held, then
   returned as genuine at EOF (`finish`). So the forged block is not needed
   for the bug: any own close (a reply, write or identity deadline) can race
   its transport client's `%exit` into the pump.
4. The stalled round trip's `readReplyRouting` passes it to `handleAsideLine`,
   which queues it on `c.async`; the round-trip wrapper discards only the
   reply (`daemon.go:906-919`).
5. `connStreamEnded` drains the async queue; `dispatch` returns true for
   `controlmode.Exit` (the only case that does, `daemon.go:1608`), so the
   verdict is `connEnd` → `teardown()` → the daemon exits and its mirror
   windows, then the DST session and server, go away. The paint loop then
   polls a dead mirror for its full ~55 s.

The violated invariant: **a `%exit` read after the daemon closed the
connection itself is the transport client's echo of that close, not the
remote ending the client; it must never turn a reattachable drop into a
teardown.** The race window is a preemption between `in.Close()` and
`out.Close()`, hence load-dependent and rare. The CI x86 failure's timing
(58 s ≈ a fast wedge plus the whole 100-try paint loop) is consistent with it;
no other paint-failure mechanism reproduced (0/50 unloaded quick-path runs,
0/50 + 0/50 loaded runs at 0.3 s).

A deterministic Go reproduction exists: a fake transport
that yields `%exit` then EOF once closed, a forged `%begin` on the wire, a 50 ms
reply deadline, one round trip, then `connStreamEnded` → `connEnd` on main
(red), `connDrop` with the fix (green).

### A, latent: the wedge itself is bimodal and brushes its 40 s budget

At 0.3 s the forged report sometimes lands mid-split (wedge in ~4 s) and
sometimes on an idle daemon (wedge only at the 30 s backstop). Heavy load
(1 CPU, 6 stress-ng hogs): wedge times {4–6 s: 35 runs, 38–40 s: 15 runs},
max 40 s against `deadline=$((SECONDS + 40))`. CI darwin's passing A took
39225 ms. One more scheduling delay fails A at `wedged = yes`.

### B: test fragility — a wedge the test cannot see leaves it nothing to wedge

Reproduced: B at 0.3 s failed **7/50** on 1 CPU with 6 hogs; each failure's
daemon log is line-for-line the darwin CI log: deadline on the seed's
`display-message -p -t %1 …`, `seed %1: capture-pane failed (wired unseeded …)`,
then `size owner: $0: claimed as client-<new>`. The forged report landed
**after** the new pane was wired, while its seed was in flight: the mirror
already showed n+1 panes, so the pane-count probe saw no wedge; the deadline
dropped the connection, and the fresh control client carries no forged
subscription (`refresh-client -B` is per client), so the remaining ~40 s of
iterations can never wedge again.

With `sleep 1` (pre-#1135) the report lands before the split, the split's
`%layout-change` is swallowed, and the probe sees the mismatch: B at 1 s
**0/50** under the same load, every run parked with no deadline yet. Also
measured at 0.3 s heavy: 8/50 runs only "parked" after a deadline had already
fired (the 1 s poll outlasted the 2 s deadline under load), so the rename
happened outside the wedge; at 1 s: 0/50.

Sensitivity (`--reply-timeout 10m --seed-timeout 10m`, same load): B at 0.3 s
and at 1 s both fail 30/30 — B genuinely depends on the deadline either way.

## Requirements

R1. A deadline drop must reattach, never tear down because of a `%exit` that
our own close provoked. A genuine remote `%exit` the pump hands over before the
daemon closes the connection still ends the mirror as today; one that races our
close (read before it, delivered after, or held in an open block until the EOF
the close causes) is treated as a drop, and the reattach then finds the session
gone and tears down through the refused-attach path. That degradation is
accepted and documented.

R2. A Go test in `picker/remotebridge/daemon` pins R1 through the production
pump → round trip → `connStreamEnded` path, red on main, green with the fix.

R3. Test A asserts the same thing, without its timing race: a forged
unterminated `%begin` is ended by the reply deadline, and the mirror then
paints live output on a fresh control client. Its wedge must not depend on
where the 1 s subscription tick falls.

R4. Test B asserts the same thing, robustly: the local session vanishes while
the daemon is wedged, and the daemon exits. It must not dead-end when a wedge
goes unseen (no state an unseen wedge leaves behind may stop a later iteration
from wedging), must not count a mismatch that a deadline already ended as
"parked", and must stay sensitive to the deadline (fails with an unbounded
one).

R5. A paint failure in A dumps the daemon log and mirror state to fd 3, so a
future CI failure is diagnosable (the 43871384224 log had nothing).

R6. No bare retries, no unexplained sleeps; every wait is tied to a measured
mechanism. `relay_flake_diag` and the shard mechanism stay untouched.

R7. After the fix, 0 failures in ≥ 50 runs of each test under the same load
that reproduced the failures, plus the repo gates.

## Design

**Daemon (R1, R2).** The pump (`startCtlPump`, the daemon's only caller of
`Reader.Next()`) drops a `controlmode.Exit` line once the connection's `dead`
flag is set. `ctlConn.close()` sets `dead` before it touches the transport, so
every `%exit` our close provokes is checked after the flag is set; a remote
`%exit` checked before any close of ours is delivered unchanged, and one that
races it degrades to a drop (R1). One choke point covers every
consumer (main-loop select, `settle`'s async drain, `connStreamEnded`,
`waitHellos`, `routeWhile`). `startCtlPump` takes the flag; `newCtlConn`
builds `c` first and starts the pump with `&c.dead`. The two test call sites
of `startCtlPump` pass a fresh flag. `docs/agents/bridge-daemon.md`'s #900
bullet gains the invariant.

Rejected: gating only `connStreamEnded` (the main-loop select and `settle`
also dispatch the same `Exit`); gating on `stalledOut()` (covers only the
guard, not the other own-close paths, and `dead` is set no later).

**Test A (R3, R5).** Order the forged block behind the daemon's own work
instead of racing the tick: install the subscription with a benign value,
`kill -STOP` the daemon, `split-window` (its `%layout-change` is queued
first), set the forged value, wait 1.5 s (one 1 s subscription tick plus
margin), `kill -CONT`. The daemon reads the notification, starts its round
trip, and parks behind the forged `%begin` until the deadline. Then the
existing deadline-log wait (40 s budget kept: if a starved server writes the
report late, the daemon goes deaf idle and the 30 s backstop still wedges it)
and the existing paint loop. One split instead of up to ten: the marker pane
stays 14 rows. The STOP…CONT span holds only commands that cannot abort the
test (`|| true`), matching the file's existing stalled-daemon test. On a paint
failure, print the daemon log tail and `@bridge_state`/pane list to fd 3.

Rejected: restoring `sleep 1` (always the 30 s backstop path, 33–39 s,
against the 40 s budget); raising the budget (keeps A at ~35 s on half the
runs and still races the tick).

**Test B (R4).** Three changes to the loop, each closing one measured hole:

1. Settle beat `sleep 1.5`: one 1 s subscription tick plus margin, so the
   report normally lands before the split and the split's layout read is the
   held wait (what the existing comment describes; same rationale as A's
   1.5 s). At `sleep 1` it was 0/50 under heavy load vs 7/50 at 0.3 s.
2. Re-arm every fresh control client: each iteration re-reads the daemon's
   control client and, when it differs from the one last armed, installs the
   forged subscription on it (`refresh-client -B` is per client). An unseen
   wedge's deadline and reattach then cost one iteration instead of the rest
   of the run. An iteration that does not park also kills the pane it split,
   so pane space never runs out across iterations.
3. A park counts only if no deadline fired during that iteration (count of
   `reply deadline|write deadline` log lines unchanged across it): a mismatch
   seen while the daemon is already reattaching is not a wedge (8/50 runs at
   0.3 s heavy renamed after the deadline had fired).
4. The wedge probe checks identity, not counts: parked means the pane id the
   split just created never shows up among the mirror's `@bridge_pane` ids
   in the 1 s window (tmux never reuses a pane id). Counts alone can match by
   coincidence once the kill in (2) runs against a daemon already deaf: the
   remote alternates n/n+1 panes while the deaf mirror stays at n+1, so every
   probe reads "live" until the 30 s backstop. Measured: the count-based
   prototype at a forced 0.3 s beat lost one run of 15 this way (54 iterations,
   never parked).

Keep #1135's 10 × 0.1 s probe and the 40 s loop budget. Prototype measurements
are in the evidence table below; the re-arm is shown by running the same loop with
the beat forced back to 0.3 s.

Considered for B and left out: the SIGSTOP ordering (it would rename after a
1 s probe that, under load, outlasts the 2 s deadline, unless the deadline
grows too).

## Prototype evidence (scratch copies, not the repo)

Harness: scratch copies of `tests/` with an instrumented A/B (stats on each
run), run with `bats --filter` under a private tmux tmpdir per run, the flake's
prebuilt `og-remote-bridge-*` and pinned `tmux-next-3.9`, a sandbox-like
`bash --norc --noprofile` as `SHELL`, pinned with `taskset` beside
`stress-ng --cpu N` on the same CPUs. Host 1-min load 18–40 during runs
(another job pushed it to ~200 earlier; every rate below was taken after it
fell).

| run | daemon | load | result |
| --- | --- | --- | --- |
| A at 1 s (main loop) | main | CPUs 0-3, 4 hogs, 8 parallel | 3/50 fail (paint) |
| A at 1 s | main + debug prints | same | 1/60 fail, `connEnd: async drain … stalled=true` |
| A at 1 s | pump fix | same | **0/60** |
| A, SIGSTOP-ordered wedge | pump fix | 1 CPU, 6 hogs, 6 parallel | **0/50**, wedge 5–6 s every run, 49/50 parked on the first round trip after `%layout-change` |
| B at 0.3 s (main) | main | 1 CPU, 6 hogs | 7/50 fail (unseen wedge) |
| B at 1 s | main | 1 CPU, 6 hogs | 0/50 |
| B at 0.3 s / 1 s, deadline 10m | main | 1 CPU, 6 hogs | 30/30 / 30/30 fail (sensitive) |
| revised B (design above), count-based probe, beat forced to 0.3 s | pump fix | 1 CPU, 6 hogs | 14/15 pass; runs parked after 1–4 earlier deadlines (re-arm working); the 1 failure is the count coincidence fixed by item 4 |
| revised B with identity probe, pump fix, 1 CPU + 6 hogs | pump fix | 1 CPU, 6 hogs | 1.5 s 50/50 pass; beat forced to 0.3 s 50/50 pass (7 runs recovered after 1–2 unseen wedges); 1.5 s with deadline 10m 30/30 fail |

## Acceptance

- The Go test is red on main and green with the fix (command and failure line
  in the PR).
- A and B, ≥ 50 runs each under the reproducing loads (CPUs 0-3 + 4 hogs for
  A; 1 CPU + 6 hogs for both), 0 failures, with host load recorded; B with an
  unbounded deadline still fails.
- `nix build .#lint`, `nix flake check --no-build`, `nix flake check`,
  `nix build .#default` pass; CI build job green on x86_64-linux and
  aarch64-darwin.

## Non-goals

- The shard weights (`tests/remote-m2-weights.json`): A moves ~3.6 s → ~5 s,
  B is unchanged at ~12.5 s; not material for the greedy assignment.
- Narrowing `child.Close`'s stdin/stdout ordering: the pump-level filter is
  ordering-independent and covers ssh too.
