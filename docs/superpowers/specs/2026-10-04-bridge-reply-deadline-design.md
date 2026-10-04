# Spec: reply deadline for control-mode round-trips (#900)

Spec-critic: accept after one revision. Tier: deep. kind: implement.

## Problem and violated invariant

`one(rt, …)` / `rt(...)` waits in `readReplyRouting` → `reader.Next()` with no
deadline outside the identity read (`armIdentityDeadline`, 30s, only around
`readIdentity`). `docs/agents/bridge-daemon.md` residual (1)/(3) records the
trigger: a socket holder replaces our subscription format
(`refresh-client -t <ours> -B`) with a value holding a raw newline plus a
well-formed unterminated `%begin <t> <n> 1`. `controlmode.Reader` keeps every
later reply and subscription line as body (memory bounded by `MaxBody`, pane
output still lifted), so the main loop's next round-trip never returns.
`localSessionGone`, the reconnect path and every other `runConn` exit are
starved. Plain network stalls share the shape.

Invariant to restore: **every wait for control-stream data on the main-loop
goroutine is bounded; a wait that cannot be satisfied poisons the connection
(drop + reattach) instead of being skipped or extended.**

## Measured evidence

Scratch `tmux` 3.7c, one loopback control client, `1000x300` pane filled per-cell
with 256-colour SGR (probe: /tmp/rtt-probe, 5 samples):

| command | reply body | wall time |
| --- | --- | --- |
| `display-message -p ok` | 51 B | 15–75 µs |
| `list-windows -F …` | 71 B | 17–51 µs |
| `capture-pane -e -p` (1000x300 dense) | 3.45 MB | 49–57 ms |
| `capture-pane -e -p` (small pane) | 575 B | 4 ms |

Size bounds already in the reader (#898): `MaxLine` 1 MiB, `MaxBody` 16 MiB.
Largest ordinary control-stream reply is the remote agent-status backstop,
`list-panes -s` at `agentstatus.go:226` (~1–2 KiB/pane ⇒ ~1 MiB at 500 panes).
Largest capture is `capture-pane -e -p` (`seed.go:46-50`), bounded by
`MaxBody`. A per-cell truecolor fill is ~2× the measured 256-colour body
(~7 MB).

## Design

### 1. A stall guard per connection

New on `ctlConn` (`picker/remotebridge/daemon/conn.go`):

```go
// armStall closes the connection unless disarmed within d.
func (c *ctlConn) armStall(d time.Duration, what string) (disarm func())
// live reports whether this connection can still carry a command.
func (c *ctlConn) live() bool
```

- `armStall` is implemented like `armIdentityDeadline` (`time.AfterFunc`), but
  one-shot and reusable; on fire it records `what` (locked diagnostic string)
  and calls `c.close()`. `d <= 0` ⇒ no-op disarm (tests / disabled). Unlike
  `armIdentityDeadline` it returns **no** "did it fire" verdict; callers detect
  a fired guard through `live()`, which is all any of them need.
- `live()` = `!c.dead && !c.st.isClosed() && c.pump.alive()`. `ctlConn` gains an
  atomic `dead` flag set at the **top** of `close()` (before the transport is
  touched), so a `live()` racing a fired guard cannot momentarily read alive;
  `stream` gains a mutex getter for `closed`; `ctlPump` gains an atomic `done`
  set by its goroutine, so an EOF on a live-but-poisoned stream is caught too —
  not only our own close. It is used at the attach boundary below, never in a
  hot path.
- `ctlConn.close()` current order is stream-first, then transport:

  ```go
  c.st.close()
  _ = c.rwc.Close()
  ```

  That order **deadlocks** against a write-stall guard: `stampAll` holds
  `stream.mu` while blocked in `Flush`, the timer fires, and `c.st.close()`
  waits for the very mutex whose holder the close is meant to unpark. Swap to
  transport-first:

  ```go
  _ = c.rwc.Close()  // unpark any blocked read OR write first
  c.st.close()       // then bar every later send
  ```

  The existing stream-first rationale (bar later sends deterministically) still
  holds for a racing `stampAll`: it errors on the closed transport and latches
  `closed` itself.

### 2. Where the guard is armed

`stream` (`daemon.go`) gains:

- `guard stallGuard` — `func(d time.Duration, what string) (disarm func())`;
  nil in every direct `newStream` test construction ⇒ unchanged behavior.
- `replyTimeout`, `seedTimeout time.Duration` (zero ⇒ off).
- `replyDeadline(cmd string) time.Duration` — `capture-pane ` prefix ⇒ seed,
  everything else ⇒ reply — and `armReply(cmd string) (disarm func())`, the
  one arming helper (`replyDeadline` + guard + log wording + no-op cases).

`ctlConn.bind(router)` enables them (`st.guard = c.armStall`,
`st.replyTimeout = c.replyTimeout`, `st.seedTimeout = c.seedTimeout`) before
rebuilding `newRoundTrip`. **The unbound identity phase keeps only
`armIdentityDeadline`'s 30s** — `bind` runs after `readIdentity`.

Every `stampAll` call site that can run on the main loop is therefore covered
after `bind`; the two pre-`bind` callers are the identity batch (covered by
`armIdentityDeadline`, including its write) and `primeClient` (covered by its
own explicit arm below). `primeClient` is the only post-identity pre-`bind`
round trip, and it arms the reply timeout around its **whole batch** — the
write inside `c.rt(cmds...)` included:

```go
func newRoundTrip(...) {
    ...
    return func() (controlmode.Line, bool) {
        ...
        disarm := st.armReply(cmds[i-1])
        l, ok := readReplyRouting(reader, router, async, st, seq)
        disarm()
        return l, ok
    }
}
```

`stampAll` arms the reply timeout around its `Flush` and disarms it after, so a
stalled write is bounded too (same fire ⇒ close ⇒ drop).

Deadline is **per reply wait**, not per batch: the wedge holds the *first*
unanswered reply, while a genuinely large `PaneSeeds` batch must be allowed its
many slow replies (each `next()` re-arms). The first command of a `PaneSeeds`
batch is `display-message` (cursor), so a wedge inside a reseed trips the
ordinary clause, not the seed one.

### 3. Why a timeout must poison, not skip

In the wedge **no reply block is ever delivered**: the forged unterminated
`%begin` keeps every later block's lines as body, so `stream.seen` stays behind
`sent` with no realignment point in sight. If the daemon carried on, the next
client-flagged block to arrive would be claimed with a stale ordinal
(`s.seen++`), and one of two equally broken things follows: `readReplyRouting`
drops it as an aside and keeps starving the waiter, or a waiter whose expected
ordinal equals that stale claim receives a foreign command's body. `fans`
(barrier) bookkeeping is off by the same gap. There is no resynchronisation
short of an `og-fanout` barrier, and a forged stall may never reach one.

So the only sound response is `close()`: the transport, pump, `stream` ordinals,
`fans`, `awaitHigh` and parked slot are discarded whole, and the next dial
builds a fresh set (`newCtlConn`) — exactly what `connHolder` already does
across a drop. A late reply arriving after the close is never read because the
pump is gone. (Separately, the "skipped reply" shape — a reply that arrives
after a deadline but before any close — would desync the same counters; that is
what the deadline's close prevents ever being observable.)

### 4. Attach-boundary liveness (repair and prime)

A poisoned connection must not be reported as connected, or the retry budget is
reset on every drop and a remote that wedges each new client loops forever
(`reattach` builds `bo := cfg.retrySchedule()` per call; `attemptCycle` returns
`cycleConnected` on `repair() == true`, and `repair` swallows a timed-out
round-trip — `daemon.go` repair returns false only on an empty registry).

- `attemptCycle`: after `if !repair() { return nil, cycleTerminal }`, check
  `next.live()`. Dead ⇒ log it, `next.close()`, `hold.close()`, and `continue`
  the same attempt loop, so the current schedule's attempts/elapsed keep
  advancing toward `cycleExhausted` → park. Repair is rerun from its top on the
  next dial and is idempotent (converger reset, reconcile, reseed, subscribe).
- `replaceConn`: after `if !repair() { return nil, mirrorGone }`, check
  `next.live()`. A dead replacement **after** the point of no return (`hold`
  already closed the old connection) is returned as `replaced`; the caller
  adopts it and `runConn`'s existing drop path reattaches. It never becomes
  `mirrorGone` (a teardown), which is what "nothing here may cost a mirror"
  forbids.
- `primeClient` returns `ok bool`: `false` when any reply fails (`ok == false`
  from the iterator) or `!c.live()` when the batch finishes. `replaceConn`
  treats `false` as `notReplaced`: `next.close()`, old connection untouched —
  the same verdict as a failed dial or identity read, and it happens **before**
  `hold.close()`.

### 5. Values

Minimum supported link rate for bridge use: **~2 Mbit/s (≈250 KB/s) aggregate**
(the stream also carries the live `%output` flood, which shares that budget).

- `defaultReplyTimeout = 30s` — ordinary replies. Worst ordinary body ~1 MiB
  (`list-panes -s` at `agentstatus.go:226`, 500 panes) transfers in ~4.2s at
  the floor (7× headroom); a realistic label/agent poll is a few KB (≫100×).
  A wedge in flight on an ordinary reply recovers in ≤30s.
- `defaultSeedTimeout = 120s` — `capture-pane` replies (`seed.go:46-50`).
  Measured 3.45 MB dense capture ≈ 13.8s at the floor (8.7× headroom); a
  per-cell truecolor worst (~7 MB) ≈ 28s (4.3×). The reader's `MaxBody` 16 MiB
  damage cap would take ~67s (1.8×) — that is the cap on damage, not a
  body supported at the floor; a >7 MB capture is already beyond usable on a
  2 Mbit/s mirror.
- Both are independent of `defaultIdentityTimeout` (30s, unchanged) and far
  under the reconnect budget (`DefaultBackoff` MaxElapsed 10 min).
- **No-progress reset rejected**: a transport wrapper could expose byte
  progress, but for this trigger bytes keep arriving throughout — `%output` and
  notifications are lifted while the forged `%begin` holds the reply — so any
  progress reset (byte- or line-based) would never fire. Wall-clock per reply
  is the only bound that distinguishes the wedge from a slow transfer.
- `Config.ReplyTimeout`, `Config.SeedTimeout` (0 ⇒ default), and test-only
  `--reply-timeout` / `--seed-timeout` flags + `OG_DAEMON_REPLY_TIMEOUT` /
  `OG_DAEMON_SEED_TIMEOUT` env, matching the existing budget knobs
  (`cmd/daemon/main.go`) so the bats suite can shrink them to seconds.

A fired guard logs one line before closing, exact shapes the docs/tests name:

```
daemon: reply deadline 30s exceeded for "display-message -p ok"; dropping the control connection
daemon: write deadline 30s exceeded; dropping the control connection
daemon: reply deadline 120s exceeded for "capture-pane -e -p -t %1"; dropping the control connection
```

### 6. Blocking-point audit on the main-loop goroutine

| wait | today | after |
| --- | --- | --- |
| `one(rt,…)` / any `rt(...)` `next()` (settle, shippers, sweep, reseed, carousel, openurl, continue) | unbounded | per-reply deadline (30s / 120s capture) |
| `stampAll` flush (main-loop `send`/`sendCtl`) | unbounded | reply-timeout write guard |
| `readIdentity` (first attach, reattach, replace) | `armIdentityDeadline` 30s | unchanged |
| `attachRefusal` drain | `min(refusalDrainTimeout, IdentityTimeout)` = 5s | unchanged (noted for completeness) |
| `primeClient` batch (replace path) | unbounded | one reply-timeout guard around write+reads, plus `live()` verdict |
| startup reads (`list-windows`, theme probe, session path) | unbounded | reply-timeout guard; a timeout ends the run with an error/teardown, not a reattach — they precede `runConn` |
| `waitHellos` | `helloTimeout` 10s | unchanged |
| `select` in `runConn` | tickers ready | unchanged; `loopTick` is buffered, so a tick that fired during the wait is delivered on the next pass |
| `LocalTmux` / `LocalTmuxOut` execs | unbounded | **out of scope**: they fork the *local* tmux server, not the control stream; no trigger in this issue. Listed as a doc residual. |
| `park` wait | `parkStop` on `Shutdown`/`localSessionGone` | unchanged |

### 7. Session-gone liveness

With every control-stream wait bounded, the main loop re-enters `runConn`'s
`select` within the deadline of a wedge; `time.NewTicker`'s channel is
buffered, so the tick that fired during the wait is pending and
`localSessionGone`/`sessionGoneTracker` runs on that pass. If the connection
was poisoned, the closed pump drives `connDrop` → `reattach`.

Honest bound while reattach itself runs: `localSessionGone` runs only in
`runConn`'s select and in `park` (and `sessionGoneTracker` needs two consecutive
definite negatives), so a local session that vanishes while reattach walks its
≤10-min `DefaultBackoff` budget is noticed at most one 5s tick after a
connection is re-established, or within ~2 ticks of park entry. That is the
existing #680 posture, unchanged by this task, and the bats variant below uses a
reachable remote so the re-established path is what it exercises.

### 8. Reconnect cannot loop hot

- With §4, a remote that wedges **during repair** every time no longer resets
  the budget: each poisoned attempt advances `bo.Next`, so the schedule
  exhausts (`MaxAttempts` 40 / `MaxElapsed` 10 min) and the mirror parks
  (probe every 2 min, one attempt each).
- A remote that wedges strictly **after** repair, and re-applies the forgery to
  every newly attached control client, still costs one deadline per drop
  (≥30s cadence, not hot; `DefaultBackoff`'s ceiling is 30s) but does not
  accumulate a budget across drops — the same pre-existing shape as any
  connect-then-drop remote. Documented as a residual; not expanded into
  cross-drop accounting by this task.
- A remote that wedges only on the old control client (this issue's trigger is
  per-client `refresh-client -B`) recovers on the first re-dial, because the
  new control client carries no forged subscription.

## Tests (red first)

1. **Unit, deadline closes + late reply never read** (new `deadline_test.go`):
   a `scriptConn` that accepts and never answers; `cfg.ReplyTimeout=50ms`;
   `one(c.rt, "display-message -p x")` returns `false` within ~2×; transport
   `isClosed()`; a second `c.rt(...)()` returns false immediately and a late
   block injected after the close is never consumed (pump gone).
2. **Unit, class split**: `replyDeadline` classified deterministically
   (unit); plus one wide-margin behavior test — `ReplyTimeout=500ms`,
   `SeedTimeout=2s`, a delayed conn answering `display-message` at ~1s fails
   while `capture-pane` at ~1s succeeds.
3. **Unit, write stall**: a fake transport whose `Write` blocks; `send` returns
   false within the timeout and the conn is closed.
4. **reattach, repair poison walks the schedule** (finding 1 contract): every
   dial supplies `identityMatch` and then stalls; the repair stub issues a real
   bound round trip (which times out) and returns `true` regardless. Assert
   bounded dials (`Retry.MaxAttempts`), `cycleExhausted`, and park entered —
   never `cycleConnected` on a dead conn.
5. **replaceConn, prime poison keeps the old connection** (finding 2): dial
   supplies `identityMatch` then stalls; `primeClient` times out ⇒
   `notReplaced`, old conn still live and still published, `repair` never
   called.
6. **bats, the issue trigger** (`tests/remote-m2-integration.bats`): mirror a
   scratch SRC; find the daemon's control client via `list-clients -F
   '#{client_name}|#{client_control_mode}'`; set `@evil` to a raw newline +
   `%begin <t> <n> 1`; `refresh-client -t <cc> -B 'ogevil::#{@evil}'`; with
   `--reply-timeout 2s --seed-timeout 2s` (the forge may catch a `capture-pane`
   wait), assert the daemon log carries the deadline line, the
   mirror drops (`@bridge_state disconnected`) and comes back (empty
   `@bridge_state`, live output painted). Variant: rename the DST mirror
   session right after the forge; `wait_daemon_exit` proves `localSessionGone`
   fires instead of a freeze.
7. **Go live tmux (OG_REQUIRE_TMUX)**, offlines the trigger: real control
   client + a `ctlConn`; forge as above; `one(rt, …)` returns false at a
   test-scaled deadline and the connection closes (reuses `requireLiveTmux`/
   `startIsolatedTmux`).
8. **Red evidence**: on current HEAD, (1) hangs (run under `go test -timeout`),
   (4)'s repair returns connected on a dead conn, (6)/(7) freeze; recorded
   against a scratch worktree at the pre-fix commit.

## Docs

`docs/agents/bridge-daemon.md`: update residuals (1)/(3) — the forged `%begin`
stall is now bounded by the reply deadline (drop + reattach) — and add a short
subsection (deadline values and rate floor, per-class split, poison rationale,
attach-boundary liveness, blocking-point audit, LocalTmux residual,
post-repair-wedge residual). No CLAUDE.md change (its table already routes
bridge-daemon work to that doc).

## Non-goals

- `controlmode` reader framing (#899).
- Reply ordinal/barrier semantics (`stream.claim` unchanged).
- Remote tmux version gating.
- A session-gone watchdog goroutine.
- Cross-drop retry accounting and bounding `LocalTmux`/`LocalTmuxOut` execs.

## Acceptance mapping

| task `## Acceptance` | evidence |
| --- | --- |
| `nix flake check` + `nix build .#lint` | commands, green |
| reply never arrives → timeout, drop, reconnect with backoff, no mismatch | tests 1, 3, 4 (+2) |
| `localSessionGone` (or equivalent exit) fires while a round-trip waits | bats variant in 6 |
| live scratch-tmux never-ending `%begin` shows recovery | bats test 6 (primary), Go live test 7 |
