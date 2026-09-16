# PLAN — #657: drive bridge dead-renderer healing from pane-died

Follow-up to #647 (shell/claude-status half, merged #658) and lands on top of #645's v2-layout
reconcile rewrite (merged #662). Scope: `picker/remotebridge/**` only, plus the CLAUDE.md bullet
that documents the mechanism.

Revision 2: the task's suggested mechanism (a session-scoped `pane-died` hook mirroring
`registerResizeHook`) turned out to have a real correctness bug on the pinned tmux, caught by
plan-critic review and confirmed by direct measurement (see below). That revision replaced it with
a connection-level signal instead.

Revision 3: revision 2's wake shape (a bare depth-1 channel, empty-body `select` case) was itself
caught by a second plan-critic pass — `sweeper.sweep()`'s own `windowSweepInterval` floor (1s)
would silently swallow the wake on the exact case this feature exists for (a quiet mirror that just
swept moments before the corpse appeared), since the channel is drained by the `select` regardless
of whether the sweep the wake-up caused actually ran. The same review also flagged that
`pumpInput`'s socket EOF has no ordering guarantee against tmux's own `pane_dead` update, so an
*immediate* forced sweep could legitimately observe `pane_dead=0` and find nothing. This revision
fixes both: the wake now forces the sweep's floor open (bypassing `windowSweepInterval` rather than
merely triggering a loop iteration that might still be floored) and is debounced through a short
fixed delay (shaped like `carouselProbe`'s own timer) rather than firing the moment the connection
closes, giving tmux time to settle `pane_dead` first. Coalescing and the backstop are unchanged.

Revision 4 (targeted fix, applied without a further critique pass — see "Plan-critic cap" below):
a third plan-critic pass found that revision 3's `force()` itself reopens a different constraint.
`resetWindow` (`reconcile.go:622-661`) closes every superseded renderer conn once its rebuild
succeeds — verified directly: `oldConn.Close()` at lines 637, 651 and 660. Each of those closes is
exactly the `wire.ReadFrame` error `died()` is wired onto, so **every successful heal arms its own
follow-up wake ~`deathSweepDelay` later.** By then the just-rebuilt renderer is alive, so that
forced sweep finds the window healthy and — under `healDeadRenderers`'s existing "any healthy pass
returns the budget" rule — deletes the strike entry. A renderer whose crash-to-crash lifetime is
longer than `deathSweepDelay` (250ms) but shorter than `mainLoopTickInterval` (5s) — the common
case for something that crashes shortly after doing a little work, as opposed to dying instantly at
spawn — never accumulates strikes and gets rebuilt forever. This is a direct hit on the task's
own verbatim constraint: "the event path must not bypass or reset the strike count faster than the
sweep did." The fix (Step 2A below) makes the healthy-pass budget return time-aware, so a pass that
lands within one recovery window of the window's own last rebuild does not count as evidence of
recovery — it is the rebuild's own echo, not a second data point.

**Plan-critic cap:** this plan has now gone through the process's full 2-revision cycle (draft →
revise → revise), both against genuinely blocking, verified findings. Revision 4's fix is narrow,
concretely specified by the third critique pass, and independently re-verified against the actual
`resetWindow`/`pumpInput` code above rather than taken on faith. Per the process's own escalation
rule, no fourth critique round was run; this is called out in the PR body under `## Escalated` for
human review rather than silently presented as a clean single-pass plan.

## Verified on the pinned tmux (e880cf6, via a scratch `-L` server, unwrapped binary, `#{hook_pane}`)

- A mirror window always carries `remain-on-exit on` (`stampMirrorWindow`). With it on, a
  renderer's process exiting fires **`pane-died`** only. With it off, the same exit fires
  **`pane-exited`** only. The two are mutually exclusive per exit, confirmed both directions.
  (An earlier pass of this plan claimed `pane-exited` never fires at all — that was a measurement
  artifact: the repro used `#{pane_id}` instead of `#{hook_pane}` in the hook command, which reads
  the *current* pane rather than the one that triggered the hook, and in one case ran on the
  system's own tmux-og-wrapped binary, which bakes in `-f <config>` and so was never a clean
  environment. Corrected and re-verified below.)
- **A session-scoped `set-hook -t <session> pane-died '...'` entirely replaces the session's view
  of the global `pane-died` array — it does not add to it.** Confirmed twice (wrapped and
  unwrapped binaries): with a global `pane-died` hook and a session-scoped one both set, only the
  session-scoped one fires; the global one is never invoked for that session. This matters because
  `config/tmux.conf.reference.nix:847-848` already sets a **global** `pane-died` hook to
  `tmux-reap-pane` (#647), which deletes a dying pane's claude-status files
  (`panes/`/`screen/`/`tasks/`/`issues/`/`watchers/` under `/tmp/claude-status`). Mirror-session
  panes carry this state too (`agentShipper` writes it under local pane ids — CLAUDE.md "Remote
  Agent Status"). **Installing a session-scoped `pane-died` hook on the mirror session, as
  originally planned, would silently disable #647's reaping for every mirror session** — a
  regression to this issue's own prerequisite, never mentioned in the original design because it
  was never measured against the real global hook. tmux options (hooks are array-valued options)
  resolve by level, not by merging arrays across levels, so there is no `-a`/append flag that fixes
  this at the session scope; the array only merges within one level.
  - A global-hook-at-a-new-index (mirroring tmux-remux's `[99]` convention, CLAUDE.md "Persist")
    was considered and rejected: a global hook fires for every session on the local server, so it
    would need a `#{?@bridge_win,...}` guard and a nudge path resolved from `#{@bridge_sock}` at
    fire time — feasible — but its *lifecycle* is shared across every concurrently-running bridge
    daemon on this machine (a common case: several remote bridges open at once, each its own
    daemon process, one local tmux server). One daemon's teardown unregistering a hook that other,
    still-live daemons rely on is a cross-process coordination problem this feature does not need
    to take on.
  - `respawn-pane -k` (the bare-redial path `rebindRenderer` handles, #547's Respawn binds) fires
    neither `pane-died` nor `pane-exited`, live pane or dead — tmux respawns the pane's command in
    place without ever marking it dead. This part of the original verification stands unchanged.

## Design

`healDeadRenderers` (`reconcilewindows.go`) already re-derives the dead-renderer set from a fresh
`list-panes` read (`mirrorPaneRows`) every time it runs — it takes no target from its caller. So
whatever signals "something died" needs to do exactly one thing: **wake `runConn`'s select loop
sooner than `mainLoopTickInterval` (5s)**, carrying no payload. Once woken, the loop's existing
unconditional per-iteration `sweeper.sweep(...)` call (itself floored at `windowSweepInterval`, 1s)
does the rest, unchanged.

Given the hook-shadowing problem above, the signal comes from the daemon's own connection to each
renderer instead of a tmux hook. `pumpInput` (`daemon.go:2255`) already runs one goroutine per
renderer connection and already returns the instant `wire.ReadFrame` errors — which is exactly
what a renderer's process exit does to its own end of the unix socket, whether the exit is a crash
(SIGKILL), a clean exit, or the daemon's own deliberate `.Close()` during a reconcile/teardown. This
is per-connection, entirely local to the daemon process, and has no interaction with any tmux hook
array at all — it sidesteps the shadowing problem structurally rather than working around it. It is
also lower-latency than a hook+touch+poll design would have been: no `run-shell -b` fork, no mtime
poll interval, just the read erroring out on the goroutine that was already blocked on it.

A spurious wake (pumpInput exiting because the daemon itself closed the connection during a normal
reconcile, not because of a crash) is harmless: `sweeper.sweep()` re-derives ground truth from a
fresh `list-panes` and finds nothing dead, so `healDeadRenderers` no-ops for that window. The
"don't race a bare respawn" constraint holds for the same reason it held under the original
design — the wake carries no target, so even a spurious or stale wake can only cause a fresh,
accurate re-check, never act on stale information.

`resetWindow`'s round-trips must run on the main loop only (the codebase's own rule, and the
#574 `ctlState.mu` deadlock note this task cites is the concrete precedent for getting this wrong).
`pumpInput` runs on its own per-connection goroutine, not the main loop, so it must not call
`healDeadRenderers` itself — it only arms a seam that `runConn`'s `select` consumes.

That seam is shaped like `carouselProbe` (`carouselprobe.go`), not like `viewReplacer`'s bare
channel: a session-lifetime `*time.Timer`, created once stopped and only ever `Reset`, whose `C()`
is what `runConn` selects on. `wake()` (called from `pumpInput`'s goroutine) arms it only if it
isn't already armed — `Reset`ting an already-pending timer would let a steady trickle of deaths keep
pushing the deadline out and never actually fire, the same "don't let a second press push the first
one's read further out" reasoning `carouselProbe.arm` documents for itself. When the timer fires
(`deathSweepDelay`, 250ms — long enough for tmux's own `pane_dead` update to have landed, short
enough to stay well under the 5s backstop), the `select` case clears the armed flag and calls
`sweeper.force()` — a new one-line method that resets `windowSweeper.lastPass` to the zero time —
before falling through to the next loop iteration, so the very next `sweeper.sweep()` call is
guaranteed to actually run rather than being silently floored by `windowSweepInterval` (1s). The
floor exists to stop a per-stream-line fork storm; it was never meant to defer a corpse a connection
close just reported, and forcing it open costs at most one extra fork per death episode, since the
debounced arm already collapses a burst to one timer fire.

Because the timer is armed only once per quiet period and its fire is what triggers the forced
sweep, several panes dying in the same beat (e.g. every pane in a killed window) collapse to one
forced pass — the "coalesce bursts" requirement falls out of this shape for free, with no separate
coalescing logic needed.

The sweep's own unconditional per-iteration call is untouched, so `loopTick` (5s) remains the
backstop exactly as before: a lost signal (a `pumpInput` goroutine that panics before reaching its
error branch, a race no test finds, or the rare case where `deathSweepDelay` genuinely wasn't long
enough for `pane_dead` to have landed yet) still gets healed, just on the old cadence — the forced
sweep is single-shot, not retried, precisely because the backstop is what makes a single shot an
acceptable design rather than something that needs its own retry budget. Nothing in this plan makes
that path conditional on the new one — it is strictly more robust against *any* failure mode in the
event path than a single delivery mechanism would be, which is the reason to keep it regardless of
which event mechanism was chosen.

One more honest gap, worth stating rather than glossing over: a renderer that never connects at all
(missing `RendererBin`, a hello that times out) leaves a corpse with no `pumpInput` goroutine ever
having existed for it — there is nothing to EOF. That death is backstop-only by construction,
bounded by the same `deadRendererStrikes` cap as every other case; it just isn't what this plan's
event path covers, and the CLAUDE.md update should say so rather than imply every corpse is now
event-driven.

## Steps

- [ ] **Step 1: `deathnudge.go`** (new file, `picker/remotebridge/daemon/`)
  - `deathSweepDelay = 250 * time.Millisecond` const — the wait between a renderer connection
    closing and the forced sweep that looks for its corpse, sized (like `carouselProbeInterval`)
    to give tmux's own `pane_dead` update time to land before the daemon's own re-list, since
    `pumpInput`'s socket EOF has no ordering guarantee against it.
  - `deathNudge` type, shaped like `carouselProbe` (`carouselprobe.go`): `mu sync.Mutex`, `armed
    bool`, `timer *time.Timer`. `newDeathNudge()` builds it with the timer created and immediately
    stopped (mirrors `newCarouselProbe`). `C() <-chan time.Time` returns `timer.C`, the handle
    `runConn` selects on. `wake()` — called from `pumpInput`'s goroutine on a renderer connection
    closing (crash, clean exit, or the daemon's own deliberate close during reconcile, all harmless
    to wake on since the sweep re-checks rather than trusting the call) — arms the timer via
    `Reset(deathSweepDelay)` only if `!armed`, so a steady trickle of deaths cannot keep pushing the
    fire time out. `fired()` clears `armed`, called by `runConn`'s case body once the timer's case
    has actually been taken (mirrors `carouselProbe.due`/`rearm` clearing pending state before the
    next arm is honoured). Doc comment: carries no payload because `mirrorPaneRows` re-derives
    ground truth on every sweep; no `cancel()` needed (unlike `viewReplacer`) — a stale pending fire
    surviving a reconnect costs one extra forced sweep, not a wrong repair. Also name the one race
    `wake()`/`fired()` has that `carouselProbe.arm`/`rearm` avoids (found in the third critique
    pass): a `wake()` landing after the timer has fired but before `runConn`'s case reaches
    `fired()` sees `armed == true` and does not re-arm, so that death rides the already-firing pass
    with less than `deathSweepDelay` of grace rather than getting its own — worst case it falls back
    to the 5s backstop like any other missed race, which the design already accepts in general.

- [ ] **Step 2: wire into `Run()` and `pumpInput`** (`daemon.go`)
  - Add `RendererDied func()` to the `Config` struct, doc comment following the `SendCtl` field's
    own precedent immediately above it ("stamped onto cfg once per Run() so pumpInput can wake the
    main loop without threading a parameter through the whole reconcile call chain"). nil is legal
    (every test that builds a bare `Config{}` directly, and any call path that predates this field).
  - `pumpInput` (`daemon.go:2255`) gains a 5th parameter `died func()`. On the `wire.ReadFrame`
    error branch, call `died()` before `return` if `died != nil` (same nil-guard shape the existing
    `paste != nil` check a few lines below already uses). Doc comment: this fires for every
    connection close, not just crashes — the caller (`sweeper.sweep`, once the debounced timer
    forces it) re-derives which window is actually dead, so a spurious wake costs one no-op forced
    sweep pass, never a wrong repair.
  - Update all 5 call sites to pass `cfg.RendererDied` as the new argument (`cfg` is already in
    scope at every one): `daemon.go:1318`, `daemon.go:1330` (both inside `setupWindow`),
    `daemon.go:1886` (inside `rebindRenderer`), `reconcile.go:589`, `reconcile.go:869`.
  - In `Run()`, alongside `replacer := newViewReplacer(...)` / `carousel := newCarouselProbe()`
    (~L675-678, both session-lifetime for the same reason): construct `death := newDeathNudge()`
    and immediately set `cfg.RendererDied = death.wake` — before any call that might invoke
    `pumpInput` (the earliest is inside the mirror-window setup loop, well after this point in
    `Run()`'s body). `cfg` is a value parameter local to `Run()`, so every later call site that
    receives a copy of it (`setupWindow(cfg, ...)`, `reconcileWindows(cfg, ...)`, etc.) carries the
    field once it is set here.
  - Add `force()` to `windowSweeper` (`reconcilewindows.go`, beside `sweep`/`lastPass`): `s.lastPass
    = time.Time{}`. Doc comment: `windowSweepInterval` exists to stop a per-stream-line fork storm,
    not to defer a corpse a connection close just reported; used only from the death-nudge case
    below. State the real bound rather than undercounting it: a successful heal's own `resetWindow`
    closes the superseded conns, which arms a follow-up force too (that is what Step 2A's recovery
    window exists to keep from being read as a second data point) — so a single death episode costs
    on the order of two forced sweeps, not one, and under sustained reconcile churn (a remote
    repeatedly splitting/closing panes) the forced cadence tops out at one sweep per
    `deathSweepDelay` (4/s) — still far below the per-stream-line storm the floor exists to prevent.
    Note also that a forced sweep whose own `mirrorPaneRows` listing fails still spends `lastPass`
    (`reconcilewindows.go`'s `s.lastPass = time.Now()` precedes the listing read) — the one way a
    force is consumed without doing anything, and harmless since the backstop still applies.
  - In `runConn`'s `select` (`daemon.go:1037-1071`): add
    ```go
    case <-death.C():
        // deathSweepDelay has now elapsed since the first connection close in
        // this batch, giving tmux time to settle pane_dead. force() ensures
        // the sweep below actually runs this pass instead of being floored by
        // windowSweepInterval — a bare wake-up with no force can be silently
        // swallowed by that floor (caught in review), which would leave this
        // no faster than the mainLoopTickInterval backstop it exists to
        // shortcut.
        death.fired()
        sweeper.force()
    ```
    — the case body does not call `sweeper.sweep()` itself (that stays the top-of-loop's job,
    unconditional and floored-unless-forced); it only clears the timer's armed flag and forces the
    floor open so the loop's next pass through the top actually sweeps.
  - No changes to `teardown`, no hook registration/teardown, no changes to `cmd/daemon/main.go` —
    `death` is constructed and wired entirely inside `Run()`, exactly like `carousel`.

- [ ] **Step 2A: keep the strike cap honest under the event path** (`reconcilewindows.go`)
  - Add `deadRendererRecovery = mainLoopTickInterval` const, doc comment: a healthy pass inside this
    window of a window's own last rebuild is that rebuild's own echo (its `resetWindow` closing the
    superseded conns arms the same `died()` this plan wires the sweep to), not evidence the renderer
    is actually staying up — using the sweep's own former cadence as the threshold means the event
    path can never return the budget *faster* than the sweep it replaces did, which is the task's
    own constraint stated as a formula.
  - Add `lastRebuild map[string]time.Time` to `windowSweeper` (beside `deadStrikes`), lazily
    initialized like `deadStrikes` already is.
  - In `healDeadRenderers` (`reconcilewindows.go:202-230`):
    - On the healthy branch (currently `delete(s.deadStrikes, remoteID); continue` at ~L211-216):
      only delete when `s.lastRebuild[remoteID]` is zero or `time.Since(...) >=
      deadRendererRecovery`; otherwise fall through without touching the strike count (still
      `continue` — a window inside its own recovery window is not re-examined for a rebuild this
      pass either way, since it isn't in `dead`).
    - Immediately after `s.deadStrikes[remoteID]++` (~L221): also set
      `s.lastRebuild[remoteID] = time.Now()`.
    - At the cap (`deadRendererStrikes` reached, ~L225-228): leave `lastRebuild` set — a window the
      cap gave up on should not have its strikes clearable by a stray echo either.
  - This makes `TestHealDeadRenderersReturnsTheBudgetAfterAHealthyPass` (existing) need a small
    adjustment: it currently calls a healthy pass immediately after the last strike with no time
    gap, which under this change would now fall inside `deadRendererRecovery` and NOT return the
    budget — the test's own premise (an unrelated later recovery, not the rebuild's own echo)
    should advance a fake clock or otherwise represent time having passed `deadRendererRecovery`
    before asserting the budget returned, or the test's `heal` helper should let the caller inject
    `s.lastRebuild` directly the way it already injects `s` itself. Keep the test's assertion
    (`rebuilds == deadRendererStrikes+1`) — only the setup needs the elapsed-time gap.

- [ ] **Step 3: unit tests**
  - `deathnudge_test.go` (new file), following `carouselprobe_test.go`'s own pattern of asserting
    state directly rather than waiting on real elapsed time (that file's `pendingCount()` test-only
    accessor is the precedent): add an equivalent test-only `isArmed()` accessor.
    - A single `wake()` call sets `armed` true.
    - A second `wake()` call while still armed is a no-op: assert `isArmed()` stays true across N
      calls, trusting `wake`'s own `if !armed` guard as the whole mechanism (no fake-timer interface
      — the package has no such seam today and a one-line guard doesn't need one). This proves the
      "several panes dying in one beat → one forced pass" coalescing property at the primitive level
      without depending on real elapsed time.
    - `fired()` clears `armed` so a later `wake()` re-arms (`isArmed()` false after `fired()`, true
      again after the next `wake()`).
    - One real-time test, generously margined (e.g. assert `C()` has not fired at `deathSweepDelay/2`
      and has fired by `deathSweepDelay*3`), as the one integration point between the state-machine
      tests above and the actual `time.Timer` — this package's tests already use real `time.Sleep`
      waits for other timer/goroutine-adjacent behavior (e.g. `reattach_test.go`), so this is not a
      new pattern.
  - `windowsweep_test.go` (existing file, additive): a new test that `force()` makes the very next
    `sweep()` call run even when called well inside `windowSweepInterval` of the last pass — the
    direct regression test for the floor-swallows-the-wake bug the second plan-critic pass caught.
    `TestWindowSweeperFloorsRepeatedPasses` (existing, unforced) must keep passing unchanged, since
    it pins the floor's normal (non-force) behavior that must survive this change.
  - `deadrendererheal_test.go` (existing file, additive) — the explicit crash-loop-under-event-drive
    test the task itself asks for ("Test this explicitly"), and the regression test for the strike-
    cap bug the third plan-critic pass caught: a window rebuilds, a healthy pass lands immediately
    after (simulating the rebuild's own `died()` echo, inside `deadRendererRecovery`) and must NOT
    return the strike budget; the window dies again and its strike count is the *next* one, not
    reset to 1 — repeated across `deadRendererStrikes` rebuilds, the window still caps, matching
    `TestHealDeadRenderersStopsRebuildingAWindowThatKeepsDying`'s existing assertion shape but with
    a healthy pass interleaved between each death. Separately, a healthy pass landing *after*
    `deadRendererRecovery` has elapsed still returns the budget (the adjusted
    `TestHealDeadRenderersReturnsTheBudgetAfterAHealthyPass` from Step 2A covers this).
  - `pumpInput` calls `died()` exactly once when its connection read errors, and does not call it
    on a clean frame read: drive it with an in-memory `net.Pipe()` (or an existing fake conn used
    elsewhere in this package's tests), close one end, assert the `died` callback fired; separately,
    write a valid `FrameInput` and confirm `died` was not called before the connection is closed.
  - `pumpInput` does not panic or block when `died` is `nil` (the "not every caller wires it"
    contract the field's own doc comment promises).
  - Explicitly re-confirm (not new logic, but named for acceptance-criteria traceability) that the
    existing `deadrendererheal_test.go` suite already covers: strike cap under a repeated-death
    loop (`TestHealDeadRenderersStopsRebuildingAWindowThatKeepsDying`), a healthy pass returning
    the strike budget (`TestHealDeadRenderersReturnsTheBudgetAfterAHealthyPass`), a user float
    corpse left untouched (`TestMirrorPaneRowsGatesOnTheDaemonsOwnStamp`), and the sweep backstop
    working with zero nudge involvement (`windowsweep_test.go`, `deadrendererheal_test.go` — none
    of those tests construct or touch `deathNudge`, and `healDeadRenderers`/`windowSweeper.sweep`
    are unmodified by this plan; only `windowSweeper` gains the new `force` method). No changes
    needed to those files beyond the one additive test above.

- [ ] **Step 4: integration — `tests/remote-m2-integration.bats`**
  - Rework `"killing a renderer process leaves the session standing and heal restores the mirror"`
    (~L2922) so it can actually fail on a regression to sweep-only healing, per the plan-critic's
    finding: the current body sends `printf` marker traffic on the remote every iteration while
    polling, which itself produces `%output` and wakes `runConn` regardless of this change — so no
    deadline value distinguishes the two paths, and killing this daemon's built-in remote traffic
    would leave the test unable to fail even with the whole feature reverted.
    - After the SIGKILL and the existing `saw_corpse` gate, **quiesce**: send nothing to the remote
      while polling for the rebuild — verified against the code this test drives: `loopTick` fires
      unconditionally every `mainLoopTickInterval` regardless of stream traffic, so quiescing removes
      only the *detection* vehicle the old test body used (`send-keys` producing `%output`), not the
      old healing path itself; a reverted daemon still heals in ~5s + round trip under a quiesced
      poll, which is what makes this deadline able to actually distinguish the two paths. Poll `$DST
      display-message -p -t host-sess:1.0 '#{pane_dead}'` (or the pane/window id changing) with a
      deadline budgeted as: `deathSweepDelay` (250ms, the debounce) + one `windowSweepInterval`-scale
      fork (~sub-second) + `resetWindow`'s spawn/hello/seed round trip on a loaded CI box — propose
      4s total, which is generous for that sum and still well short of what a sweep-only path needs
      (5s tick + the same round trip, i.e. structurally >5s). If 4s proves flaky in CI, the fallback
      is asserting the *relative* gap instead (heal time recorded and compared against a fresh
      measurement of the 5s-backstop-only path with `RendererDied` left nil), not a larger absolute
      number.
    - Only after the rebuild is observed, send the `KILLHEAL_$$` marker and confirm the mirror
      repaints — keeps the existing repaint assertion, now decoupled from the timing assertion.
    - Update the comment to describe the event path (connection close, not a hook) with the sweep
      retained as backstop, and to state explicitly why the traffic-during-poll shape was removed.

- [ ] **Step 5: docs — `CLAUDE.md`**
  - Update the "A renderer's exit must not be structural" bullet: replace "so `healDeadRenderers`
    on the maintenance sweep is what finds it" with the event-driven framing — the daemon's own
    `pumpInput` goroutine for that renderer's connection notices the read erroring out (crash,
    clean exit, or the daemon's own close), and a short debounced timer (`deathSweepDelay`, giving
    tmux's own `pane_dead` update time to land) forces the next sweep pass open rather than letting
    `windowSweepInterval`'s floor defer it; the maintenance sweep is kept as a lower-cadence
    backstop for anything that fails to signal, including a renderer that never connected at all
    (no `pumpInput` goroutine ever existed for it). Add a short note on why this is a connection-level
    signal and not a `pane-died` tmux hook: a session-scoped hook was measured to shadow the global
    `pane-died` hook `tmux-reap-pane` already relies on (#647), silently disabling claude-status
    reaping inside mirror sessions. Keep every other clause (resetWindow-not-retireMirror, the
    strikes cap, the `#{pane_dead}`+`@bridge_pane` key, the respawn non-race) — they are unchanged
    by this plan, verified above.

- [ ] **Step 6: gate**
  - `nix build .#default`, `nix flake check`, `nix build .#lint` — three separate commands, all
    green.

## Commit plan

One commit: `feat(bridge): drive dead-renderer healing from renderer connection close (#657)` — the
subject names the mechanism the plan actually settled on (a `pane-died` tmux hook was rejected for
shadowing #647's global hook; see the revision history above), not the task's original framing.
Code, tests, docs, and this plan doc land together. PR body carries `Closes #657` and an
`## Escalated` section summarizing the plan-critic history (three findings across two revision
cycles, the cap, and Step 2A's fix landing without a fourth critique pass).
