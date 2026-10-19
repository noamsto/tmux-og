# Bridge: parked-cursor reshape hold (#1002, revision)

Revises `2026-10-18-bridge-reshape-hold-design.md`. The mechanism, its evidence
and why synchronized output does not help are unchanged and live there. This
revision replaces how a held pane is drawn, what may write to it while it is
held, and when it is released.

## Why the first design was not enough

A held pane sits on the local alternate screen, which never reflows and has no
history. That fixes growth. A height shrink is worse there, though:
`screen_resize_y` first deletes rows below the cursor (`eat_empty`), then,
with `GRID_HISTORY` off, deletes the rest from the top. A regrow only adds
blank rows at the bottom. On the main screen the same shrink pushes the top rows
into history and the regrow pulls them back.

With the hold's cursor wherever the app left it (usually low), every pass that
shrinks a held pane and later regrows it shifted the held rows up until the
seed. Four review rounds found such paths one at a time:

- a locally zoomed close (push/pop-zoom);
- an unzoomed split of the top of a stacked pair (Append + swap);
- rotate, preset and remote resize that shrinks then regrows;
- zoom-on from a shrunk cell;
- a hold carried into a later pass;
- a timed-out release;
- an unawaited exit release followed by a back-to-back reconcile;
- `resetWindow` inside a held pass;
- a late hold on an already-shrunk pane, where the per-row CUPs clamp onto the
  last row.

Gating which passes may hold kept leaving another one.

A review round on the parked-cursor revision itself then found four more:

- an earlier hold, or a seed in flight, could satisfy a later hold's barrier,
  because every hold parked at the same cell (fix: the per-hold park column);
- a hold landing after the pane narrowed wrapped each wide row into the row
  below (fix: autowrap off around the rows);
- a `resetWindow` merge-back left the pane parked with an unheld sink (fix:
  `oweReseed`);
- a DCS/OSC/APC open across the hold made keyneg forward a terminal query that
  local tmux, returned to ground by the hold's `ESC \`, answered to the
  remote (fix: `kn.EndRegion()` at hold and release).

## Design: park the cursor, freeze the pane

**A held pane's cursor sits on row 0 (screen row 1), and nothing moves it until
the pane is released.** The column is per hold (see the barrier). Two
consequences follow:

- With `cy = 0`, a shrink's `eat_empty` step can delete every row except row 1
  from the bottom, so it never reaches the top. Every row stays at its own
  position or goes blank.
- A regrow adds blank rows at the bottom.

That holds for any sequence of shrinks, regrows and width changes on any path:
kill, split, swap, rotate, preset, `resize-window`, zoom and unzoom,
push/pop-zoom, a rebuild's split/select-layout before `respawn-pane`, or a
later pass or a later reconcile call. Width needs nothing more: the alternate
screen does not reflow, and clipped cells survive a narrow-and-widen round trip
(measured).

### Hold bytes (`holdBytes`)

```
ESC \  ESC[?1049h  ESC[?7l
for i = h … 1:   ESC[<i>;1H ESC[m ESC[2K <row i>
ESC[?7h                      (only if the app had wrap on)
ESC[1;<col>H                 col = 1 + tok%8, clamped to the pane width
```

- **`ESC \` first.** It ends any OSC/DCS/APC string the last frame left open
  and returns local tmux's input parser to ground (see keyneg below).
- **Autowrap off.** The read header carries `#{wrap_flag}`. `holdBytes` writes
  `ESC[?7l` after `?1049h`, the rows, then `ESC[?7h` only if the app had wrap
  on. A hold that lands after the pane narrowed would otherwise wrap each wide
  row into the row below. The restore is a best-effort snapshot: `render.Seed`
  now carries the app's DECAWM (`?7h`/`?7l` from the `#{wrap_flag}` read with
  the seed), so a release never leaves autowrap off past the seed.
- **Bottom-up order.** A late hold, landing on a pane that already shrank below
  `h`, clamps each CUP past the bottom onto the last row. Each lower row then
  overwrites it, and `ESC[2K` clears what a longer row left behind, so the last
  row ends up holding its own content. Measured (10-row pane, 15-row hold):
  top-down gives `row01…row09 row15`; bottom-up gives `row01…row10`.
- **Park CUP last.** The cursor reaches the park cell only after every row is
  written, so the barrier can tell a fully parsed hold apart.
- **Per-hold column.** `hold()` returns a per-sink counter `tok`, and the hold
  parks at `ESC[1;<col>H` with `col = 1 + tok%8`, clamped to the pane width. A
  duplicate hold the pump drops while held still writes its park CUP.

### Barrier (`awaitAlt`)

The poll reads `#{alternate_on}|#{cursor_y}|#{cursor_x}`. Each pane's hold is
confirmed at its own `1|0|<col-1>`. The column is per hold so that an earlier
hold, or a seed in flight, cannot satisfy this hold's barrier. The rest is
unchanged: exact line count, the `holdWait` (200 ms) bound, stop on an exec
error. A hold that times out is still in flight. It still lands, writes
row by row in place at whatever size the pane has reached by then, and parks
the cursor.

### What may write to a held pane

The pump owns `held`. It also publishes it on the sink as an atomic
(`outputSink.isHeld`), for tests and logging only: a snapshot taken at read time
can miss a queued hold, or report held while a seed is already queued, so no
decision is made from it. While a pane is held, nothing but the release may
move its cursor:

- **`%output` is filtered, then discarded.** A `FrameOutput` batch that
  arrives while held is run through the stateful stream filters (`kn.Feed`,
  `gfx.Filter` + `hasImages`) and then dropped. It is not written, so
  `streamGround` does not observe it, the predictor is not fed it, and the
  mouse tracker, which mirrors the bytes written to the renderer, does not see
  it either. The seed's clear-then-set brings the tracker up to date.
  - The screen effects are not lost. `PaneSeeds` captures after every
    `%output` routed before its reply, so the seed carries them. Output that
    arrives after a release-as-reseed is carried by the reseed's capture.
  - `gfx.Filter` retains kitty stores as usual, and `gfx.Replay` writes them
    just before the seed.
  - Lost: the non-screen effects of discarded output (BEL, OSC 52, title
    changes). Mode changes are restored by the seed's clear-then-set.
  - Writing the deferred bytes before the seed is wrong. They are the remote
    stream for the real cursor, so on the held copy they overwrite row 0. The
    renderer does not join frames into one pty write: its 4 KiB bufio writer
    flushes whenever its reader runs dry. Even within one write, tmux reads
    the pty in chunks and can redraw between them.
- **Prediction is off.** `onHold` already unpaints and drops the model.
  - While held, a key frame only runs `pred.reset()`: no prediction, no paint.
  - A `framePredictSeed` is ignored, because a model built while held would
    paint onto the held copy.
  - Once released, `onKeys` requests a model again on `predictModelRetry`.
- **A held pane is not held again, and the pump decides that in stream
  order.** `holdReshaped` reads and enqueues a hold for every holdable
  candidate, without checking whether it is already held. When a `frameHold`
  reaches the pump while `held` is already true, the pump drops it: the rows
  are not rewritten and the deadline is not restarted, but its park CUP is
  still written. So the copy stays the pre-reshape
  content of the first hold. If a seed came between the two holds, the second
  is written after it, in stream order. Its capture is then either the old copy
  or the seed's content, and both are legitimate.
  - Neither local `alternate_on` nor an `isHeld` snapshot can make this call.
    A seed already written but not yet parsed reads `alternate_on` 1. A hold
    still queued reads `isHeld` false, and a seed still queued reads it true.
  - A pane the remote app keeps on the alternate screen (vim, less) is held
    too. `?1049h` is a no-op there; the rows and the parked cursor are written.
    The seed restores the app's screen (`?1049l`, then `?1049h` and its
    content). Its shrink-then-regrow shifts rows on origin/main as well, since
    an alternate screen has no history either way.

- **keyneg is told about the hold boundary.** The hold's leading `ESC \`
  returns local tmux's parser to ground, so the pump calls `kn.EndRegion()`
  when a hold is written and when it is released (seed or deadline). Without
  it, a DCS/OSC/APC left open across the hold would make keyneg forward a
  terminal query as payload that local tmux, now in ground, answers back to
  the remote. A string spanning the boundary is then scanned as ground
  (queries stripped, the safe direction).

### Release

- **The seed releases.** `render.Seed` leads with `?1049l` and then `ESC[m`.
  The pump writes the graphics replay and the seed, then clears `held`.
- **The pass's exit release is now a reseed request.**
  - `reconcileSnapshot` still defers a `frameRelease` on every sink it held.
  - Once a seed has already released the pane, the pump drops it, as now.
  - Otherwise the pump keeps the pane held and raises `dropped`, so
    `reseedDropped` repaints from the remote, and that seed releases.
  - The old immediate `?1049l` went away because it exposed the main screen
    reflowed by `screen_alternate_off`: the original junk.
- **A `resetWindow` merge-back owes a reseed.** The re-registered sink is
  marked `oweReseed`, so its seed releases a pane left parked on the alternate
  screen.
- **Deadline.** `holdFreezeMax` (1 s) is a named constant next to `holdWait`.
  - It is longer than `holdWait` because it bounds the hold-to-seed time,
    which includes a reseed's WAN round trip. `holdWait` bounds only a local
    barrier.
  - It is a pump timer started when the hold is written. If it fires while the
    pane is still held, the pump writes `?1049l` first (restoring the real
    cursor and pen), raises `dropped` and clears `held`. Live output flows
    again from the next frame, and the reseed repaints what was discarded.
  - No pane is held, or has its output discarded, longer than `holdFreezeMax`.
- **The loop-top release is removed.** Carrying a hold across passes is safe,
  and releasing between passes exposed the reflowed main screen.

### Which passes hold

The gates added in review rounds 3 and 4 are removed: Append, Swaps, `zoomed`,
`w.appliedZoom`, the `#{window_height}` check, and the loop-top release. Each
excluded a shrink-then-regrow path, which the parked cursor now makes safe.

A candidate is a tiled pane present before and after the pass, with an
unpaused sink and a local pane. In a zoomed window, only the zoomed pane is a
candidate, because it is the only one the pass reseeds (`zoomHides`, #557).
The read then decides:

- It needs `alternate-screen` on and `pane_dead` 0. A pane that is already
  held is read and enqueued like any other; the pump drops the duplicate hold.
- It needs either a structural pass (`force`) or dims that differ from the
  target.
- On a close, a local `kill-pane` can give the freed space to a different
  neighbour than the remote did. next-3.9 gives it to the next cell; 3.6a and
  3.7c give it to the previous one. So a survivor whose final dims equal its
  target may still have grown and shrunk back. That is why a structural pass
  holds every candidate.

The read header is
`#{@bridge_pane}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{pane_dead}|#{wrap_flag}`
(6 fields). It drops `alternate_on`, the cursor fields and `#{window_height}`.

For a pane already on the alternate screen, the barrier can pass before the
hold is parsed, if the app's cursor already sits at that hold's row-0 column.
The shrink then deletes from the bottom anyway, and the late hold writes its
rows in place.

## What the user sees

- **On a reshape:** the pre-reshape rows stay in place, the gained area is
  blank, and the rows a shrink cut off are blank. Then the remote's capture
  lands, then the app's repaint, then the confirmation reseed.
- **Live output and echo of the held pane** freeze from the hold until the
  seed. That is about 30 ms of local forks plus one `PaneSeeds` round trip,
  and in split passes also a renderer spawn and `seedRenderer`. The freeze is
  capped at `holdFreezeMax`. The PR reports measured hold-to-seed latency
  (p50 and max, idle and under CPU load).

## Residuals

- **Final shrink.** A pane whose final height is smaller (a split of the
  active pane) shows its top rows, and the bottom rows, the prompt included,
  stay hidden until the seed. The remote itself shows the bottom rows.
  Pre-shifting the copy would match the remote, but would show rows at
  positions they never occupied whenever the remote app repaints differently.
- **Deadline release.** A release forced by the deadline (the seed never came,
  or came after 1 s) restores the reflowed main screen until the reseed. That
  is the original junk, and it is bounded by the reseed. For an
  alternate-screen app, it is the app's main screen.
- **A sequence split across the release.** A discarded batch that ends
  mid-sequence leaves `kn` or `gfx` holding the tail. The tail is completed
  after the seed, as for any seed landing mid-stream today.
- **Discarded output while held.** Its non-screen side effects (BEL, OSC 52,
  title changes) are lost. Its screen effects arrive with the seed.
- **An unseeded `wireRenderer`.** After a seed error the pane is wired
  unseeded and its new sink starts with `held` false, while the local pane may
  still sit parked on the alternate screen. Live output then lands on the copy
  until the next seed. (A `resetWindow` merge-back no longer has this: the
  sink is marked `oweReseed`, and its seed releases the pane.)
- **Barrier passes early for an alternate-screen app** whose cursor already
  sits at the hold's row-0 column. Its shrink still deletes from the bottom.
- **Narrow panes (known, shipped as a follow-up).** On a pane narrower than 8
  columns, `parkCol` = `min(1+tok%8, max(1,w))` parks consecutive holds at the
  same column. A later hold's barrier can then be satisfied by an earlier hold
  still on screen, the race the token closes for wider panes, and a reshape can
  land on a seed still in flight. It heals at the next seed or at
  `holdFreezeMax` (1 s). Offered fix: `1 + tok%min(8, max(1, w))`; width 1
  stays a residual.
- **DECOM, scroll region and charset (known, follow-up).** `?1049h` resets
  neither the app's origin mode with a scroll region nor its G0/G1 charset.
  The hold's CUPs are then region-relative: rows land misplaced, the park lands
  off row 0 and the barrier never confirms (costing `holdWait` per pass). SO/SI
  cells replay under the stream's charset, so text shows as line-drawing
  glyphs. It heals at the seed or `holdFreezeMax`. Offered fix: write
  `ESC(B ESC)0 SI` after `?1049h`, and `ESC[?6l`, or keep DECOM as a residual.
  Probes: `decom-probe.sh`, `acs-probe.sh` (worker scratchpad).
- **A DCS terminator split around the hold (known, follow-up).** When keyneg
  has passed a lone trailing ESC inside a DCS (`escPending`), local tmux reads
  the hold's `ESC \` as DCS payload and stays in the DCS, swallowing the hold,
  the seed and later output. The pane stays blank until the remote's next ST;
  `holdFreezeMax` does not heal it, because the deadline's `?1049l` is swallowed
  too. keyneg itself over-strips (safe for queries). Offered fix: keyneg
  reports the pending ESC (e.g. `PendingDCSEscape()`), and the pump writes a
  single `\` ahead of the hold.
- Carried over from the first spec:
  - the blank flash if the hold frame spans two pty reads;
  - a split OSC/DCS at the hold boundary is dispatched truncated;
  - predictions painted at the read are captured into the copy;
  - the first seed is the remote's rewrap of an app that repaints on SIGWINCH.

## Acceptance (dispatcher, for this revision)

- **Probe matrix: 0 positional violations.** Every one of the 11 probe paths
  shows 0 violations. The oracle: every row of every intermediate frame is
  blank or equal to the pre-reshape row at the same position. The paths:
  - split+swap (-v and -h);
  - zoomed kill;
  - widen;
  - rotate;
  - preset;
  - resize (shrink-y then grow-y);
  - zoom-on;
  - zoom-off;
  - close -v and close -h.
- **Fault paths: 0 violations.** The same holds on the 4 round-4 fault paths:
  - the loop-top timeout, now a hold carried across passes;
  - an unawaited exit release followed by a back-to-back call;
  - `resetWindow` inside a held pass;
  - a late hold on an already-shrunk pane.
- **Existing #1002 bats cases stay green:**
  - The split-swap case changes from "takes no hold" to "keeps the bottom
    pane's rows in place".
  - The cross-pass case also asserts that pass 1 held the pane.
- **New bats cases for the deterministic paths:** rotate and zoom-on. A late
  hold on an already-shrunk pane is pinned by the `holdBytes` test instead.
- **Bats harness rules (all six cases):**
  - All six stall the daemon's local reshape for 1.5 s and so start the daemon
    with the test-only `--hold-freeze-max 10s`.
  - Every sample is checked; there are no pre-stall drops.
  - The sampler header is
    `#{pane_width} #{window_panes} #{pane_height} #{alternate_on}`, and each
    case's guard pins its reshaped cell.
  - Every case asserts that the last sample shows the hold released
    (`alternate_on` 0).
  - Reproducing the origin/main red needs a daemon wrapper that drops the
    `--hold-freeze-max` flag.
- **Go tests:** the per-hold token and park column, the 6-field header,
  `holdBytes` autowrap, the barrier's `1|0|<col-1>`, the `oweReseed` merge-back,
  and `EndRegion` at hold and release.
- **Freeze:** hold-to-seed latency measured. A forced-deadline test shows a
  seed that never arrives releases at about 1 s and reseeds.
- **Predictor:** a test that keys during a hold paint nothing, a model seed
  during a hold is ignored, output during a hold is neither written nor
  observed, and prediction resumes after the seed.
- **Full gate:** `nix build .#lint`, `nix flake check`, `nix build .#default`.
