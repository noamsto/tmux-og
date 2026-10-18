# Bridge: hold a mirror pane on the alternate screen across a local reshape (#1002)

## Problem

A split pane closes inside a mirror window. The neighbour that takes over the
space briefly shows garbled content (wrapped or misplaced lines, stale rows
across the gained area), then snaps to the correct repaint. The survivor should
go straight from its old content to the remote's repaint at the new size.

## Confirmed mechanism

**Local tmux reflows the survivor's local grid the instant the daemon's local
`kill-pane` grows it. That grid and its history are not the remote's, so the
reflow produces rows neither side ever showed.** They stay on screen until the
reshape seed lands: the local exec chain (`kill-pane`, `list-panes`,
`resize-window`, `select-layout`) plus one `capture-pane` round trip.

The local grid diverges from the remote's in two ways, and both are permanent
properties of a mirror pane:

1. **Local history is not remote history.** Every seed starts with `ESC[2J`.
   On the main screen, tmux's `scroll-on-clear` copies the whole visible
   screen into the pane's history (see the predictive-echo spec), and a reshape
   seeds twice (`markReshaped`). The pane's pre-renderer content (the launcher's
   placeholder, the window's first shell) is in that history too. The remote
   history holds none of it.
2. **Wrap flags differ.** A seed writes captured rows joined by hard `\r\n`,
   so a seeded line that wrapped on the remote is two unwrapped lines locally.
   Live `%output` wraps naturally, so those lines carry `GRID_LINE_WRAPPED`.

When the survivor widens, `grid_reflow` joins its wrapped lines. The screen
holds fewer lines, so the visible window slides up into local history and
shows its tail at the top: rows from an old seed copy or the pre-renderer
pane. When it grows taller, `screen_resize_y` pulls scrolled history lines back
in, with the same local-only source.

### Evidence

Repro: a scratch "remote" server (`-L`, its own `TMUX_TMPDIR` and
`CLAUDE_STATUS_DIR`) and a `--test-local` mirror daemon, 120x30, two panes
side by side. The survivor shows 12 lines of 87 characters, which wrap at its
60 columns. The neighbour is killed on the remote. The survivor's local
frames are sampled every ~3 ms by a control client on the mirror server
issuing `capture-pane -p` in a loop.

- Survivor content arrived as **live output**, local pane pre-filled with a
  known `LOCALJUNK<n>` banner. Two of three runs on origin/main (234ca42) caught
  a frame with junk rows:

  ```
  garbled fr0014:   (rows in neither the pre-close nor the settled frame)
  LOCALJUNK1
  LOCALJUNK10
  LOCALJUNK11
  frames=303 garbled=2
  ```

  The same run with the user's own zsh as the local placeholder exposed its
  `zsh-newuser-install` banner above the joined lines for ~23 ms (+214.8 ms →
  +237.4 ms), before the seed.
- Survivor content arrived through the **startup seed** (no wrap flags): the
  stale frame is the pre-close rows unchanged in the widened pane for ~110 ms.
  No join happens, so nothing outside the pre-close frame appears.
- Vertical split, history from live scrolling: the gained rows come from
  history that matches the remote's, so the transition is clean. The defect
  needs a local history that differs, which every mirror has after its first
  seed.

The window is short on a local socket. In production it adds an ssh round
trip and the daemon's local execs, so the garbled frame lasts tens to hundreds
of milliseconds.

### Mechanisms ruled out

- **Old-size `%output` painted after the resize.** Pane-shaping paths keep the
  plain `cfg` (#808), so no output reaches a sink between `%layout-change` and
  the local reshape. In the repro the first post-kill frame already has the new
  width and pre-close content.
- **The reseed racing the app's repaint.** Both are ground truth in stream
  order. The garbled frame comes before the first seed.
- **The renderer redrawing from a stale buffer.** The renderer has no buffer
  (`render.Run` writes frames verbatim). The reflow is local tmux's own.

### Why synchronized output (DECSET 2026) does not fix it

Measured in the pinned tmux source (9566f6f):

- `window_pane_resize` calls `screen_write_stop_sync` before resizing. A sync
  opened ahead of the local reshape ends at the reshape, with the reflowed grid
  flushed.
- Sync defers line draws only (`screen_write_should_draw_lines`). An alt-screen
  switch and the layout change redraw through `redraw_damage_window`, which does
  not check `MODE_SYNC`.
- `capture-pane` reads the grid, so sync changes nothing any grid sampler sees.

Upstream control mode has no pane-refresh request, and no `%layout-change`
ordering helps: the reflow is on the local side.

## Design

**Before the daemon reshapes a main-screen mirror pane locally, it moves that
pane onto the local alternate screen holding a copy of its visible rows. The
reshape seed releases it.**

tmux never reflows a pane on the alternate screen
(`screen_resize(&wp->base, sx, sy, wp->base.saved_grid == NULL)`), and the
alternate grid has no history (`screen_alternate_on` clears `GRID_HISTORY`).
A held pane that grows keeps its rows where they were, and the gained area is
blank. A single net shrink looks the same on both screens and matches what the
remote did, but a shrink followed by a regrow does not (`screen_resize_y`
deletes from the top when the cursor is low, and the regrow adds blank rows at
the bottom), so a hold is only taken on a pass where no held pane shrinks and
then regrows before its seed (see "Where"). The seed already begins with `ESC[?1049l` (`render.Seed`),
which restores the main screen, then clears and repaints it from the remote capture.
The main screen's history survives the round trip.

Measured on a scratch server, local only. A pane held content with junk in its
history and 60-column wrapped lines, then its neighbour was killed:

| | After `kill-pane` (120 cols) | After a seed-shaped release |
|---|---|---|
| no hold | `JUNK13`…`JUNK20`, then the joined lines | seed content, history 29 |
| hold | the 60-column pre-close rows, `alternate_on=1` | seed content, history 29 |

### Where

In `reconcileSnapshot`'s pass loop, before the pass's first local reshape:
`applyPaneOps` (structural) or `applyLayout` (geometry-only). Every
daemon-driven local reshape of a tiled pane goes through there: remote close,
split, swap, preset, remote resize, zoom transitions and repair-pass
reconciles. One choke point covers them all, and `holdReshaped` decides there
whether the pass may hold at all.

**A pass holds only when no held pane shrinks and then regrows before its
seed.** A single net shrink looks the same on both screens and matches what the
remote did. A shrink-then-regrow does not: the main screen round-trips the rows
through history, but the alternate screen has no `GRID_HISTORY`, so
`screen_resize_y` deletes rows from the top when the cursor is low and the
regrow adds blank rows at the bottom: held content ends up shifted up. `holdReshaped` therefore returns nil (no read,
no hold: the pre-hold behaviour) for:

- a pass with `ops.Append`: `split-window` shrinks a pane, and the following
  `select-layout` or swap can regrow it;
- a pass with `ops.Swaps`: a swap moves a pane through another pane's cell;
- a zoomed pass (`zoomed`): a zoom-on transition's `select-layout` can shrink
  the pane before the zoom assert grows it;
- a locally zoomed window (`w.appliedZoom`): push/pop-zoom and unzoom plus
  re-zoom shrink and regrow.

It also holds nothing when the read reports the window taller than the target
(`#{window_height}` above `L.H`), because `FitWindowCmd`'s `resize-window`
would shrink heights first.

Evidence, scratch tmux next-3.9, 80x45: an unzoomed split of the top of a
2-pane vertical stack (Append plus a swap). Without a hold the bottom pane
returns `L01..L22`. With a hold a pane returns `L13...` with a blank last row.
Assessment probes also showed rotate/preset/resize/zoom-on violating with the
hold. Chained passes without a seed in between are the cross-pass path, now
released at the loop top (see "Release").

Which panes are held. On a pass that may hold, a pane is held only when all of
these hold:

- It is a tiled pane present before and after the pass (`remote ∩ newRemote`).
- It has a registered, unpaused sink.
- The pass reseeds it, and either the pass removes a pane (`force` is
  `len(ops.Remove) > 0`: a close-only pass holds every candidate) or the
  pane's local dims differ from the target (its `L` cell). `force` holds even
  a survivor whose final size matches its target: the local `kill-pane` can
  give the freed space to a different neighbour than the remote did (tmux
  next-3.9 gives a closed pane's space to the next cell, stock 3.6a and 3.7c
  to the previous one), so the survivor may have grown and then shrunk back.
  Pinned by `TestReconcileHoldForceHoldsAnUnchangedSurvivor`.
- Its local pane reports `alternate_on` 0 and `alternate-screen` on. A pane
  already on the alternate screen never reflows. A pane with the option off
  ignores both `?1049h` and `?1049l` (`screen_write_alternateon`/`off`), so
  it cannot be held.
- Its local pane is not dead (`pane_dead` 0). A renderer corpse keeps an open
  sink channel, so the hold would be queued and the barrier would wait the
  full `holdWait` every pass.

The first three are known without an exec and pick the candidates. The read
in step 1 supplies the rest, and the window-height check.

What still holds: a remote close (kill) in an unzoomed window, which is the
original #1002 bug, and a geometry-only pass in an unzoomed window that does
not shrink in height (a remote `resize-pane`, a drag or a preset, where each
pane is resized once).

Floats are out of scope: the tiled `select-layout` leaves them alone, and they
reshape only when the remote float changes.

### Steps

1. **Read.** One local exec, batched with `;`. For each candidate local pane
   (found with `localPaneFor`): `display-message -p` of
   `#{@bridge_pane}|#{alternate_on}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{cursor_x}|#{cursor_y}|#{pane_dead}|#{window_height}`
   (9 fields), then `capture-pane -ep`. The capture is exactly `pane_height`
   rows, so the reply splits by count. A malformed header, a `@bridge_pane`
   that is not the expected remote id (the positional map is stale) or a
   short capture ends the positional parse: that pane and every later one are
   not held. A failed exec holds nothing, and the pass runs as today.
2. **Hold.** Enqueue on the pane's sink one daemon-internal frame,
   `frameHold`. Its payload is `ESC \`, then `ESC[?1049h`, then each captured
   row placed with its own CUP (`ESC[<row>;1H`), then a CUP to the read cursor.
   - `ESC \` ends any OSC/DCS/APC string the last output frame left open and
     aborts an open escape or CSI. `%output` frames split at arbitrary byte
     boundaries, and without a predictor the pump tracks no ground state. In
     ground, ST is a no-op. The interrupted sequence's tail then prints on the
     held screen, which the seed replaces.
   - CUP per row, not CRLF, so a scroll region cannot scroll the copy.
   - The pump treats the frame as a model drop point for the predictor
     (`predictor.onHold`: a full `reset()` unpaint on the main screen, then the
     model is dropped), observes the bytes through `streamGround`, and writes
     them as `FrameOutput`.
   - A full buffer drops the frame. `hold` reports that, and a dropped hold is
     neither awaited nor released.
3. **Barrier.** The bytes reach local tmux through the renderer's pty
   asynchronously. `awaitAlt(cfg, locals, want)` polls one batched
   `display-message -p '#{alternate_on}'` for the held panes until all read
   `want` (1 after a hold, 0 after a release), bounded by `holdWait` (200 ms). Then
   reshape either way. A late hold still shows the pre-close copy at the new
   size, and the seed releases it.
4. **Release.** The held state is the pump's own, since only the pump knows
   what reached the renderer and in what order. It is set when the pump writes
   a hold, and cleared when it writes a seed (`render.Seed` leads with
   `ESC[?1049l`). When `reconcileSnapshot` returns, it enqueues a release
   frame (`frameRelease`) on every sink it held, seeded or not.
   - The pump drops a release that arrives with nothing held. That is the
     normal path: the seed queued ahead of it already released.
   - Otherwise the pump writes `ESC[?1049l` and marks the sink for a reseed
     (`dropped++` under `s.mu`, which `enqueue` and `takeDirty` share, so
     `reseedDropped` repaints it from the remote once it drains). Output may have landed on the held screen and is lost with it,
     and `screen_alternate_off` restores the hold-time cursor and pen. Ground
     truth is the only safe state after an unplanned exit.
   - The deferred release covers the refused `select-layout`, the seed error,
     a dropped seed and the reset paths at function exit. It does not cover a
     hold carried into a later pass of the same call.
   - **Cross-pass release.** `reconcileSnapshot` can run several passes, and a
     hold from an earlier pass may not be released yet: a refused
     `select-layout` without a pane-count desync sends no seed, a seed is
     dropped or fails, or a seed is still in flight. So at the top of each
     later pass the daemon releases every still-held sink and awaits
     `#{alternate_on}` 0 (`awaitAlt`, bounded by `holdWait`) before
     reshaping. On a timeout it skips holds for that pass and falls back to
     origin/main behaviour. Residual: a released pane whose seed legitimately
     put it on the remote's alternate screen keeps `alternate_on` 1, so that
     pass waits `holdWait` and holds nothing. Pinned by
     `TestReconcileHoldReleasesBeforeALaterPass`,
     `TestReconcileHoldReleaseTimeoutSkipsHolds` and the bats case `a hold
     carried into a later reconcile pass is released before that pass reshapes
     (#1002)`.
   - The release goes to the sink object that was held. A sink closed since
     drops it, as every closed sink drops frames.

### Seed change

`?1049l` restores the cell saved by `?1049h`: the remote app's pen at hold
time. The seed's following `ESC[2J` erases with the current background, which
`render.Seed`'s leading `ESC[m` exists to prevent. So `render.Seed` emits a
second `ESC[m` right after `?1049l`. The same restore already happens today
for a remote alt-screen pane (local `?1049h` from an earlier seed), so this also
closes that latent flood.

### What the user sees

Pre-close rows, then the held rows at the new size with blank gained area, then
the remote's capture, then the app's repaint, then the confirmation reseed.
Every frame is either the old content or the remote's.

The remote's capture is taken the instant the remote reshapes (`markReshaped`).
For an app that repaints on SIGWINCH, it is tmux's own rewrap of the old
screen. That frame is in neither the pre-close nor the settled frame, but the
remote pane shows it too, so it is faithful, not garbled. It cannot be skipped
by holding until the confirmation reseed. An incremental repaint (Ink erasing
its previous lines relative to the cursor) must land on the screen the remote
drew it against, and only the first seed provides that screen. The AC2 oracle
therefore uses a survivor that does not repaint.

Residuals, accepted:

- The hold's `?1049h` clears the pane before its rows are parsed. A client
  redraw between two pty reads of the hold frame shows a blank pane for that
  redraw. The rows usually arrive in the same read.
- Predictions painted at the read are captured into the held copy and shown
  until the seed. Cosmetic.
- A pass that takes no hold (split, swap, zoomed, or a window that shrinks in
  height) keeps the pre-hold reflow, so junk rows remain possible there. A
  zoomed window whose width changes still reflows on the main screen, as it
  did before the hold.
- The general fix is a follow-up, outside this change: park the hold's cursor
  at row 1 (rows written bottom-up, the barrier checking `cursor_y` 0) so a
  shrink deletes from the bottom, and have the pump defer a held pane's
  `%output` until its seed or release. Probes showed it keeps rows in place on
  all eleven reshape paths, and it would re-enable holds on every pass.
- Output the remote sends between the hold and its seed lands on the held
  alternate screen. The seed, captured after it in stream order, restores it.
- A rebuild that fails and re-registers the old conn (`resetWindow`'s
  merge-back path) replaces the held sink. That pane stays on the alternate
  screen until its next seed.
- An OSC or DCS split across the hold boundary is dispatched truncated:
  tmux runs the string's exit handler on the `ESC \` (`input_exit_osc`,
  `input_exit_apc`; a DCS dispatches on ST). CAN exits through the same
  handler, so no byte avoids it.
- A release's reseed lands on the next main-loop wake: `reseedDropped` runs
  from the main loop and waits for the sink to drain. Until then the pane shows
  its restored main screen at the new size. That is no worse than today, since
  these failure paths reseed nothing now.
- In a zoomed window, a structural pass reflows a hidden neighbour that gains
  the closed cell, and that neighbour is not held. It is invisible until
  unzoom, and the unzoom pass reseeds it (#557).
- Remote output that switches the pane to the alternate screen between the
  read and the hold makes the hold overwrite the app's alternate screen. The
  seed restores it. A release without a seed leaves the pane on the main
  screen until the reseed it marks.

### Cost

One local exec per reshape pass with a candidate, plus one or two barrier
polls when a pane is held. No remote round trip. Pane-shaping paths already
hold `%output` across their execs (#808), so live echo waits ~10–30 ms longer
on a reshape.

## Not regressed

- **Zoom (#413, #557).** No hold on a zoomed pass or in a locally zoomed
  window. Every local reshape there shrinks the zoomed pane to its saved cell
  and regrows it: `kill-pane` runs `window_push_zoom`/`window_pop_zoom`
  (`server-fn.c`), and `select-layout`/`resize-window` unzoom before the zoom
  assert re-zooms. The zoom-on transition's `select-layout` can likewise shrink
  the pane before the assert grows it. On the main screen the shrink pushes rows
  into history and the regrow pulls them back, so the rows return in place. On
  the alternate screen there is no `GRID_HISTORY`: `screen_resize_y` deletes
  rows from the top when the cursor is low and the regrow adds blank rows at the
  bottom, so held content shifts up until the seed. Measured on scratch tmux
  next-3.9, 80x45, top pane zoomed, rows `L01`..`L45`, cursor on row 45, hidden
  pane killed: the main screen keeps `L01 L02 L03` first, a held alternate
  screen shows `L24 L25 L26`. The `if -F` zoom assert is unchanged.
- **Split and swap.** An unzoomed split of the top of a stacked pair shrinks
  and regrows a pane through `split-window` plus a swap. No hold, so the
  bottom pane's rows stay in place as on the main screen.
- **Floats.** Not held and not reshaped by the pass loop.
- **Layout presets, border drags, rotate (#787, #797, #823).** A tiled drag
  already reshaped the local panes, so their dims equal the target and nothing
  is held. A preset or remote resize that resizes each pane once and does not
  shrink the window holds as a geometry-only pass. A rotate is a swap pass and
  takes no hold.
- **Reattach repair.** Runs `reconcileLayout` per survivor and gets the hold
  the same way.
- **#951 owed reseed** (same-shape early return) does not reshape, so it holds
  nothing.
- **#233 ordering.** The seed is still written after the reshape. The hold
  precedes the reshape and carries the pane's own content at its old size.

## Tests

- **Bats (AC2)**, in `tests/remote-m2-integration.bats`. Two remote panes, the
  survivor shows wrapped live lines, the local pane starts with a `LOCALJUNK`
  banner. A `tmux` wrapper stalls the daemon's local `select-layout` for 1.5 s,
  as the #808 case does, so the post-kill window is deterministic rather than
  ~20 ms. The test kills the neighbour on the remote and samples the survivor
  until it settles. Oracle: positional: row i (trailing spaces trimmed) of
  every sample is blank or row i of the pre-close or the settled frame. The
  slow wrapper writes a stall marker, which names the samples taken during
  the stall, so the test asserts it sampled the stalled window. Red on
  origin/main: the stalled frame shows `LOCALJUNK` rows. Green with the hold.
- **Bats, zoomed**: `a zoomed mirror keeps the zoomed pane's rows in place
  when the remote closes the hidden pane (#1002)`. The zoomed pane's rows
  stay in their positions across the close (no hold, so the main-screen
  push/pop restores them in place).
- **Bats, split and swap**: `an unzoomed split of the top of a stacked pair
  takes no hold and keeps the bottom pane's rows in place (#1002)`. The
  wrapper stalls after a local `swap-pane`. The oracle samples
  `#{pane_width} #{window_panes} #{alternate_on}` and compares rows as strings:
  no sample shows `alternate_on` 1, and the rows stay in place.
- **Go unit tests.**
  - Hold bytes and the read parse: count split, id mismatch, short reply,
    `alternate-screen` off, already on the alternate screen, dead pane, the
    ninth `window_height` field.
  - Pump: a hold is written as output after the predictor drop. A seed clears
    the held state, so a release after it writes nothing. A release with
    nothing written since the hold writes `ESC[?1049l` and raises `dropped`.
    A hold dropped to a full buffer reports false and is not released.
  - `reconcileSnapshot` on a close sends the read and the hold before
    `kill-pane`, and the barrier poll before the reshape. A refused
    `select-layout` sends a release. A locally zoomed window, a zoomed pass, a
    pass with `ops.Append` or `ops.Swaps`, and a window taller than the target
    hold nothing and run no read. A geometry-only pass skips panes whose dims
    do not change.
- **Existing remote suites pass unchanged (AC3).**
