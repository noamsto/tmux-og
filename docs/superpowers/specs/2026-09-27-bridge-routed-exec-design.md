# Bridge daemon: route %output while a window-set exec runs (#808)

## Problem

When a mirrored remote window is added or removed, keystroke echo through the
bridge stalls ~40–65 ms once per event (uncapped, #807's build). Both tmux
servers stay clean, so the time is inside the Go daemon.

## Diagnosis (measured)

Temporary daemon tracing (`OG_TRACE808`, not shipped) stamped every `%output`
line as the `ctlPump` goroutine read it and again when `Router.Route` delivered
it, and timed every main-loop phase and every local tmux exec. `churn`
harness, CPUQuota=400%, 32-core host, load ~10:

- `%output` for the probed pane waited **up to 95 ms** between arrival and
  routing. Every wait over 20 ms lines up with a window add or remove.
- Inside `addWindow` (110–225 ms total at this cap) the waits cover exactly
  two stretches of **local tmux execs**, with routing resuming between them:
  1. `createMirrorWindow` (`new-window`, 23–51 ms) plus
     `stampMirrorWindow` (4× `set-option`, `set-option`, `rename-window`):
     40–90 ms.
  2. `setupWindow`'s shaping: `resize-window`/`select-layout` (PlanWindow),
     `list-panes`, the zoom `if`, `respawn-pane`, two `set-option`: ~40 ms.
- `closeWindow` (`kill-window`) and the labels/agents flushes right after an
  add (`set-option` runs) add 20–45 ms stalls of the same kind.
- Round-trips (`readReplyRouting`) and the hello wait (`waitHellos`) do
  **not** stall output: they route `%output` as they walk the stream.

Mechanism: `ctlPump` only buffers. The main goroutine is the stream's
only consumer, and a local exec (`cfg.LocalTmux`/`LocalTmuxOut`, a fork+exec
plus a local-server round-trip) blocks it while `%output` accumulates in the
pump channel. Keystroke sends are unaffected (`connHolder.send` writes
straight to the stream).

## Goal

A local exec issued from a window-set operation no longer holds `%output`.
Add/remove spikes on the churn probe meet the bars in Acceptance. Every existing
ordering invariant holds unchanged.

## Non-goals

- Pane-shaping paths: `reconcileLayout`/`reconcileLayoutFrom`,
  `resetWindow`, `applyPaneOps`, float reconcile on an existing window,
  reseeds, the maintenance sweep and its heals. Their execs keep blocking
  routing on purpose (see Ordering).
- Batching or removing execs. Measured: `new-window` alone is 23–51 ms, so
  fewer execs cannot close the stall.
- The pump, `Router`, sink and seed machinery. `render.Seed` (#803) and
  `isDismissKey` (#789) have open PRs and are not touched.

## Design

### `routeWhile`: an exec that reads the stream like a hello wait

A new helper runs one exec on a helper goroutine. While the exec runs, the
calling main-loop goroutine keeps consuming the control stream, with
`waitHellos`' semantics:

- every line is claimed (`claimSeq`), so ordinals stay exact;
- `%output` is routed (`handleAsideLine` → `Router.Route`);
- any other notification is queued on the async queue for `settle`, as a
  round-trip does;
- a reply block is dropped, **unless a round-trip may still be waiting for
  it** (see below).

When the exec returns, the helper stops reading and returns the exec's
result. Every stream, router and async-queue access stays on the main
goroutine; only the fork+exec moves. With no live connection
(`hold.get() == nil`) the exec runs inline.

### Replies a round-trip still awaits: park and stop

`waitHellos` drops every reply. It relies on no round-trip being outstanding
across the wait. `routeWhile` makes that structural instead of assumed:

- The stream records `awaitHigh`: the highest command ordinal any round-trip
  batch has asked to read. It is updated in `newRoundTrip` after `stampAll`.
  Fire-and-forget `send`s never raise it.
- A reply whose ordinal is nonzero and `<= awaitHigh` may belong to a batch
  whose `next()` has not run yet: `PaneSeeds`' iterator reads lazily. On such
  a reply `routeWhile` **parks** it in a one-slot `stream.parked` and **stops
  reading** for the rest of the exec. Every later line, including `%output`,
  stays in the pump for the round-trip's reader. That is the exact stream
  position the old code would have stopped at, so seed-before-output
  (`PaneSeeds`' per-pane delivery) holds.
- `readReplyRouting(want)` checks the slot first. If the slot holds `want`, it
  returns that line. If it holds an earlier ordinal (an abandoned or
  fire-and-forget reply that raced a batch), it drops it, as the old reader
  would have when walking past it, and reads on. The reply order in the
  stream means the slot never holds an ordinal above `want`.
- **While the slot is occupied, `routeWhile` reads nothing**: it runs the
  exec with no stream reads, exactly the old behaviour. Reading on would
  route output that follows the parked reply before its reader has handled
  it. A lazy batch whose replies span two execs therefore stays correct.
- **`runConn` clears the slot at the top of every pass.** No operation is in
  flight there, so a parked reply's reader has gone and it is abandoned. The
  old reader would have dropped it too. This bounds how long a stale slot can
  disable routing to the rest of one operation.
- Replies above `awaitHigh`, and blocks flagged 0 (remote hooks), are dropped
  as `waitHellos` drops them. These are the common case under typing load:
  every keystroke's `send-keys` and its `og-fanout` barrier answer here.

### A geometry notice stops the read too

Review found that a `%layout-change` arriving during a routed exec was queued
while the output behind it kept flowing. That output reached the renderer
before `settle`'s local reshape. `routeWhile` now treats the notice like an
awaited reply:
- it queues the notice and stops reading for the rest of the exec;
- it reads nothing at entry while the async queue still holds one.

That gate only works if every undispatched notice is still on the queue.
`settle` therefore drains it in place (`asyncQueue.drain`): each notice
leaves the queue only as its own dispatch starts, so a `%window-renamed`
dispatched ahead of a `%layout-change` in the same batch cannot read past it.

Round-trips inside the same operation still read past it: that is the
existing read-first transient, not widened.

### Stream end during an exec

On a closed channel `routeWhile` stops reading and waits for the exec. The
next main-loop reader sees the same closed channel and takes its existing EOF
path (`connDrop`, a failed round-trip).

### Scope: a routing Config for window-set operations only

`Config` gains `routing(run)`. It returns a copy whose exec-backed hooks
(`LocalTmux`, `LocalTmuxOut`, `LocalArea`, `Reflow`, `LocalPanes`) each run
through `run`. `Run` builds one such copy, `flowCfg`, whose `run` is
`routeWhile` over the connection currently in `hold`. The plain `cfg` is used
everywhere else.

`flowCfg` is passed only to operations that create, close or rename **whole
mirror windows**, or write options. None of them changes the geometry or
content of a pane that already has a registered sink:

| Call site | Why it is safe |
| --- | --- |
| dispatch `%window-add` → `addWindow` (→ `setupWindow` on a new window) | Its panes have no sink until `wireRenderer`, which runs right after that pane's capture reply is read. Earlier output goes to the Router's pre-registration buffer, as it already does during `setupWindow`'s round-trips. |
| dispatch `%window-close` → `closeWindow` | Unregisters every pane of the window before its one exec (`kill-window`). |
| dispatch `%window-renamed` → `applyMirrorName` | Option write and `rename-window`. |
| every `reconcileWindows` call in `Run` (startup, `settle`, `repair`) | Add, close and rename of whole windows, plus `select-window`. |

`setupWindow`'s tail, `reconcileFloats`, runs after the new window's tiled
sinks are registered. It is still safe:
- a fresh `mirrorWindow` has no local floats, so no float is moved or resized;
- a float is a `new-pane -X/-Y` overlay that reshapes no tiled pane;
- the new float's own sink is registered only after its capture reply is
  read.

Output routed during those execs for the just-seeded tiled panes is
post-capture output, and it belongs after their seeds.

Everything else keeps the plain `cfg`:
- `retireMirror` and `resetWindow` reached from a layout reconcile. They
  rebuild a window whose panes are registered, so they are pane-shaping.
- The labels/agents flushes. They run after this pass's `settle`, and the
  loop's blocking `select` has no arm for a non-empty async queue. A
  notification they queued, a `%pause` say, would wait for the next stream
  line or tick. Their own round-trips already have that property today, and
  this change does not widen it.

### Why pane-shaping paths stay blocking

`reconcileLayoutFrom`'s geometry-only fast path applies the notification's
layout with no round-trip. `select-layout` reshapes the local pane, then
`FrameResize` and the reseed follow. Today no `%output` is routed between the
`%layout-change` and the local reshape on that path. Routing during the
`select-layout` exec would send post-reshape output into a pane that is still
the old size: a transient garble until the reseed.

The read-first reconcile paths already route such output during their
`readLayout` round-trip. That accepted transient is repaired by the reseed and
`markReshaped`. It is not widened here: the whole pane-shaping family stays on
the plain `cfg`.

### The routing Config never reaches another goroutine

`routeWhile` is only correct on the main-loop goroutine. A second concurrent
reader would reorder lines. Goroutines started on in-scope paths
(`pumpInput`'s paste handler, via `cfg.paster()`) must get plain hooks:
`routing()` keeps the original hooks in the copy, and `paster()` restores
them before building its closures. No other goroutine is started with a
Config on these paths. `watchLocalClient`, `acceptConns` and the theme retry
are built from the plain `cfg`.

`flowCfg` is built after `Run` stamps its per-run hooks onto `cfg`
(`RendererDied`, `InputSeen`, `SendCtl`), so the copy carries them.

## Ordering invariants (unchanged, and why)

| Invariant | Holds because |
| --- | --- |
| Reply ordinals and `og-fanout` barriers (#715/#723) | Every consumed line passes `claimSeq`, the same as `waitHellos`. |
| Seed-before-output (`PaneSeeds` iterator, #430) | A reply a batch may await parks and stops routing at its exact stream position. |
| Notifications never dispatched reentrantly | Queued on the async queue, as during a round-trip. |
| `%pause`/`%continue` | Queued, so `handlePause`/`handleContinue` still run from `settle`. Output for a paused sink is dropped by the sink, as today. |
| Reshape → FrameResize → reseed | Those paths do not use `flowCfg`. The geometry-only fast path still routes nothing before the reshape. The read-first paths' existing transient is unchanged. |

## Acceptance

- Before/after `tests/perf/keystroke-latency.sh churn`, p99 and max for the
  mirror probe, interleaved A/B at CPUQuota=400% with host load reported.
  Bar: the after build's churn p99 is below the before build's in every
  interleaved pair.
- Daemon-side outwait, from the same tracing applied temporarily to the after
  build. Bar: no `%0` wait over 15 ms falls inside a window-set operation's
  exec. Any residual wait is attributed to the out-of-scope phase that
  caused it, and that attribution is the explanation the issue asks for.
- Go tests for `routeWhile`:
  - routes `%output` while the exec is blocked (red on the old code);
  - queues notifications;
  - drops unawaited replies, and a later round-trip still finds its reply;
  - parks an awaited reply and routes nothing after it until the
    round-trip reads it;
  - handles EOF mid-exec;
  - skips reading while the park slot is occupied;
  - a mid-batch exec parks at an `og-fanout` barrier, and the batch's next
    `next()` still returns its own reply;
  - `paster()` of a routing Config holds plain hooks.
- `go test -race` on the daemon package: test fakes now run the exec on a
  helper goroutine.
- `docs/agents/bridge-daemon.md` and `performance.md` updated.
- `nix build .#default`, `nix flake check`, `nix build .#lint` green.
