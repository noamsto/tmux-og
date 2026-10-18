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
blank. The seed already begins with `ESC[?1049l` (`render.Seed`), which
restores the main screen, then clears and repaints it from the remote capture.
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
reconciles. One choke point covers them all.

Which panes are held. A pane is held only when all of these hold:

- It is a tiled pane present before and after the pass (`remote ∩ newRemote`).
- It has a registered, unpaused sink.
- It will be reseeded this pass: in a zoomed window only the zoomed pane, as
  the `zoomHides` rule decides.
- The pass is structural (`applyPaneOps` will kill, split or swap), or the
  pane's local dims differ from the target (its `L` cell, or `L.W×L.H` for
  the zoomed pane). A structural pass holds every candidate. A local
  `kill-pane` in a zoomed window runs `window_push_zoom`/`window_pop_zoom`
  (`server-fn.c`), which shrinks the zoomed pane to its cell and regrows it,
  even though its final dims equal the target. A zoomed geometry-only pass
  whose layout string changed is held the same way: its `select-layout`
  unzooms and `assertMirrorZoom` re-zooms.
- Its local pane reports `alternate_on` 0 and `alternate-screen` on. A pane
  already on the alternate screen never reflows. A pane with the option off
  ignores both `?1049h` and `?1049l` (`screen_write_alternateon`/`off`), so
  it cannot be held.

The first three are known without an exec and pick the candidates. The read
in step 1 supplies the rest.

Floats are out of scope: the tiled `select-layout` leaves them alone, and they
reshape only when the remote float changes.

### Steps

1. **Read.** One local exec, batched with `;`. For each candidate local pane
   (found with `localPaneFor`): `display-message -p` of
   `#{@bridge_pane}|#{alternate_on}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{cursor_x}|#{cursor_y}`,
   then `capture-pane -ep`. The capture is exactly `pane_height` rows, so the
   reply splits by count. A header whose `@bridge_pane` is not the expected
   remote id skips that pane: the positional map is stale. A failed exec holds
   nothing, and the pass runs as today.
2. **Hold.** Enqueue on the pane's sink one daemon-internal frame,
   `frameHold`. Its payload is `ESC \`, then `ESC[?1049h`, then each captured
   row placed with its own CUP (`ESC[<row>;1H`), then a CUP to the read cursor.
   - `ESC \` ends any OSC/DCS/APC string the last output frame left open and
     aborts an open escape or CSI. `%output` frames split at arbitrary byte
     boundaries, and without a predictor the pump tracks no ground state. In
     ground, ST is a no-op. The interrupted sequence's tail then prints on the
     held screen, which the seed replaces.
   - CUP per row, not CRLF, so a scroll region cannot scroll the copy.
   - The pump treats the frame like a dims-changing resize for the predictor
     (unpaint, drop the model), observes the bytes through `streamGround`, and
     writes them as `FrameOutput`.
   - A full buffer drops the frame. `hold` reports that, and a dropped hold is
     neither awaited nor released.
3. **Barrier.** The bytes reach local tmux through the renderer's pty
   asynchronously. Poll one batched `display-message -p '#{alternate_on}'` for
   the held panes until all read 1, bounded by `holdWait` (200 ms). Then
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
   - The release covers the refused `select-layout`, the seed error, a dropped
     seed, the reset paths and the trailing passes.
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
- Painted predictions are captured into the copy until the seed.
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

- **Zoom (#413, #557).** Only the pane the pass reseeds is held, so hidden
  panes are never held. Killing a hidden pane holds the zoomed pane across the
  local push/pop-zoom. The `if -F` zoom assert is unchanged.
- **Floats.** Not held and not reshaped by the pass loop.
- **Layout presets, border drags (#787, #797, #823).** A tiled drag already
  reshaped the local panes, so their dims equal the target and nothing is
  held. Presets reconcile like a remote resize.
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
  until it settles. Oracle: every non-blank row (trailing spaces trimmed) of
  every sample is a row of the pre-close or the settled frame. Red on
  origin/main: the stalled frame shows `LOCALJUNK` rows. Green with the hold.
- **Go unit tests.**
  - Hold bytes and the read parse: count split, id mismatch, short reply,
    `alternate-screen` off, already on the alternate screen.
  - Pump: a hold is written as output after the predictor drop. A seed clears
    the held state, so a release after it writes nothing. A release with
    nothing written since the hold writes `ESC[?1049l` and raises `dropped`.
    A hold dropped to a full buffer reports false and is not released.
  - `reconcileSnapshot` on a close sends the read and the hold before
    `kill-pane`, and the barrier poll before the reshape. A refused
    `select-layout` sends a release. A zoomed reshape holds only the zoomed
    pane. A geometry-only pass skips panes whose dims do not change.
- **Existing remote suites pass unchanged (AC3).**
