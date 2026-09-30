# Newborn agent pane repaint (#883) — design

## Problem

Agent TUIs (Claude Code, pi) sometimes come up garbled in a freshly created
pane and stay that way until the user zooms the pane in and out. A zoom is two
real size changes, so the app does a full repaint. A signal alone does nothing:
Node emits `process.stdout` `'resize'` only when the size really changed
(dispatcher-verified; `kill -WINCH` logs no resize event). The bug is
intermittent, so it looks like a race between app startup and a resize.

## Measured timeline (scratch server, wrapped tmux, node size probe)

Reproducer: `-L probe` scratch server with the built conf, a real attached
client (200x50, from a second scratch server), then `dispatch`'s own sequence,
copied from the installed `dispatch` script: `new-window -d` →
`resize-window -x <client_w> -y <client_h - status>` →
`set-option window-size latest` → `send-keys` launch → (grid) `split-window`
per role, each role launched right after its split → `tmux-grid-refit`. The
probe logs its boot size, "ready" (first render, at +1.5 s), and every
`SIGWINCH`/`'resize'`. Times are ms from the first step.

Grid path, 3 pre-existing windows:

```
    67 launch lead
   101 lead boot 200x46
   159 split spec-critic        (lead is now 200x22)
   263 lead resize 119x46       (grid-refit, via window-layout-changed)
   518 split plan-critic
   873 refit done
  1605 lead ready 119x46
 spec-critic: boot 200x23 → resize 80x46 → resize 80x23 (2nd split + refit)
```

New-window path, 15 pre-existing windows (status bar at a row boundary):

```
    98 launch lead
   136 boot 200x43
  1641 ready 200x43
  4137 resize 200x44            (status went 5 → 4 rows: height-only)
```

So the resizes that hit a newborn agent pane after its process started are:

1. **Grid split + `tmux-grid-refit` relayout**: land 60–900 ms after launch,
   inside the app's startup. `dispatch` launches the lead *before* it splits
   the role panes, and launches each role before the next split, so every
   pane but the last gets resized while its app is booting. Real Claude
   renders in bursts from +0.7 s to +1.75 s after launch (measured with
   `#{pane_output_generation}`), so these resizes land mid-startup.
2. **Status-row reflow** (`tmux-reflow-windows` changing `status` 2–5):
   height-only, lands seconds later, after the app is ready. It hits every
   pane of the session, newborn or not.
3. **`aggressive-resize` on first view**: no resize measured once `dispatch`
   pre-sizes the window, except when the status row count changed in between
   (same as 2).

A resize after the app is steady is harmless: real Claude and pi repaint
cleanly after a resize at +6 s. Neither app showed a visible garble in
isolation on this host, so the race needs load to hit. Every resize that can
race startup lands in the first second; nothing tmux-og does after that can
garble the app.

Upstream root cause, reported and not fixed here: `dispatch` (other repo)
launches the lead and each role before the grid is fully split and laid out.
Splitting every pane and running `refit_grid` *before* the `send-keys`
launches would remove resize 1 at its source.

## Goals

- A newborn agent pane whose size changed since birth gets exactly one real
  size change after its app is up, so it does a full repaint.
- It works for single-pane windows, where `resize-pane -Z` does nothing, and
  for multi-pane (grid) windows.

## Non-goals

- Preventing the grid/dispatch resizes. They come from `dispatch`, which is out
  of scope.
- Status-row reflow resizes after startup. They are steady-state resizes and
  the apps handle them.
- Bridge mirror windows (`@bridge_win`), floats, and non-agent panes.

## Design

Chosen: **(b) a one-shot, gated, post-ready nudge.** (a), prevention, is not
available on the tmux-og side: the harmful resizes come from `dispatch`'s
ordering and from structural status reflow. The nudge covers every source,
including a future one.

### 1. Birth stamp (fork-free)

`after-new-window[30]`, `after-split-window[30]` and `after-new-session[30]`
run `set -pF @og_birth_size '#{pane_width}x#{pane_height}'`, in-process with no
fork. Measured: the hook's current pane is the new pane, `-d` included. These
events are per creation, not per keystroke or resize. `after-split-window` gets
a bare `set-hook -gu` in the clear block so a reload stays idempotent (the
other two are already cleared). Floats (`new-pane`) and the renderer panes
`respawn-pane` makes get no stamp and are never nudged.

tmux has no "now" format (`current_time` expands empty), so the stamp holds
the size only. "Newborn" means *the stamp is still there*: it is consumed at
the pane's first agent sighting, whatever the outcome, so a pane gets one nudge
decision in its life. A pane that never runs an agent does not keep the stamp
forever: the sweep counts, in `@og_birth_seen`, the sweeps a stamped pane is
seen running a non-agent command, and drops the stamp after 12 of them (about
60 s). Only an agent started within about 60 s of its pane's creation is
therefore a candidate.

### 2. Trigger: the existing agent sighting pass, sweep caller only

`arm_agent_detect` (`tmux-update-icons`) already walks `list-panes -a` and
matches agent commands against `$AGENT_COMMANDS`. Add `#{@og_birth_size}` to
its row format, after the `#{window_id}` canary, since it is a closed token.
Only the client-independent `@og-sweep-tick` caller (`$1` non-empty, every
5 s) triggers. The per-tick `#()` caller runs once per attached client in the
same second, so it would race itself.

For an agent row with a non-empty stamp on a non-mirror window, the sweep
**claims the pane atomically inside the tmux server**, in one command (the sweep also keeps the `@og_birth_seen` grace counter
described in section 1 for stamped panes that are not running an agent):

```
tmux if -F -t <pane> '#{@og_birth_size}' "run-shell -b -t <pane> '<repaint> #{q:pane_id} #{q:@og_birth_size}' ; set -pu -t <pane> @og_birth_size"
```

tmux runs one client's command list without interleaving another's, so two
overlapping sweeps can never both claim a pane. The loser's `if -F` sees an
empty stamp. This costs one fork per newborn agent pane in its life, and none
on any other row. The worker (`@agent_repaint@`, a store-path placeholder pinned
like `@reflow@`) is started by tmux's `run-shell -b`, with no `& disown` in the
sweep. The size comparison is left to the worker, so a resize between sighting
and ready still counts.

### 3. Worker: `tmux-agent-repaint <pane> <birth-size>`

The stamp is already consumed when the worker starts (the claim above).

1. **Wait for ready**: at least 3 s after spawn (spawn is at or after launch;
   measured startup ends by +1.75 s), then until `#{pane_output_generation}`
   is unchanged across a 1 s sample, capped at 12 s after spawn. A busy agent,
   such as a dispatched worker already processing its prompt, never goes quiet
   and takes the cap, which is steady state by then. An empty
   `pane_output_generation` (a resident server that predates the format)
   counts as quiet. A vanished pane exits.
2. **Final gate** (checked once, exit on failure): the size differs from the
   birth size; the pane is not floating; the window is not `@bridge_win` and
   holds no floating pane (`#{P:…pane_floating_flag}`, which also covers a
   modal popup).
3. **Wait for a nudge slot**, polling every 0.25 s up to 8 s after ready. That
   bound fits the lead plus several role workers nudging in turn, at about
   0.5 s each. A slot is free when:
   - the window is **not zoomed**. A zoom means a sibling worker's nudge or a
     user zoom, so the worker waits and never exits on it. Only a zoom still
     present at the bound ends the worker, and then it changes nothing;
   - and, for a multi-pane window, the target is the window's active pane
     **or** no client has the window current (`#{window_active_clients}` is
     0). A zoom makes its pane active, so zooming a background pane while a
     user types into the window would send those keys to the wrong agent. When
     the slot never frees (the user keeps typing in another pane of a watched
     window), the pane goes un-nudged. That is a documented gap: the user is
     looking at the pane and can zoom it by hand.
4. **Nudge**:
   - **One tiled pane**: save the window-local `window-size`
     (`show -wqv`, empty = inherited). Then `resize-window -x W-1 -y H-1`,
     sleep 0.3 s, `resize-window -x W -y H`, and restore the option (`set -wu`
     when it was inherited). Both dimensions change, as a zoom's do. Two
     separate calls, so the app gets two real size changes and not one
     coalesced no-op. The claim guarantees one worker per pane, and a
     single-pane window has one pane, so no second worker can interleave the
     save/restore.
   - **Several tiled panes**: zoom the target, but only if the slot condition
     still holds, checked atomically in the same command
     (`if -F '<slot>' { resize-pane -Z -t <pane> }`). The window's active pane
     and last pane (`#{P:#{?pane_active,…}}`, `#{P:#{?pane_last,…}}`) are
     captured inside that same `if -F`, before its zoom, so a sibling's
     mid-zoom focus is never read as the state to restore. If the slot no
     longer holds, go back to waiting. Sleep 0.3 s, then unzoom only if the window is
     still zoomed on this pane (`if -F` on `window_zoomed_flag` +
     `pane_active`). When the active pane moved, restore last then active
     (`select-pane -t <last> ; select-pane -t <active>`), so both the active
     pane and `{last}` end as they were. Measured: a zoom resizes only the
     zoomed pane, and the other panes get no `SIGWINCH`.

### Interaction with the refit stamps

- Grid gate: skips a zoomed window, and unzoom restores the exact geometry
  signature, so `tmux-grid-refit` does not fork. The single-pane path changes
  `window_width`/`height` twice, but the grid gate needs `@crew_grid=1`, and
  `tmux-grid-refit` no-ops on a single pane anyway.
- Float gate: the worker never runs in a window with floats.
- Bridge `@bridge_nudge` gate: mirror windows are excluded.

### Known, accepted over-reach

- The birth reference is pane creation, not app launch, bounded by the ~60 s
  grace (`@og_birth_seen`, 12 sweeps). An agent started within that window, in
  a pane resized in between (for example by a status-row reflow), is nudged
  once at its first sighting: a visible 0.3 s zoom, or a one-cell shrink for a
  single pane, of an agent that just rendered. An agent started later finds
  the stamp already dropped and is never nudged.
- A window zoomed (or a slot busy) for the whole 8 s is not nudged.
- A tmux-remux restore recreates every pane, so each restored agent pane that
  the restore layout resized is nudged, in one burst.
- A window holding any float (or a modal popup) is never nudged.
- A background nudge in a window with no `{last}` pane (a `-d`-built grid)
  leaves the target as `{last}`, because an empty `{last}` cannot be restored.
  The active pane is always restored.

### Performance

Hook budget: three fork-free `set -pF` on creation events. Sweep: one extra
format field per row, and one tmux call (the claim) per newborn agent pane
in its life. The worker makes about 15–60 tmux calls over ≤ 20 s, once per
newborn agent pane.

## Testing

`tests/agent-repaint-integration.bats`, a scratch server with the built
wrapper and a real attached client. The agent stand-in is a bash probe
exec'd as `pi` (so `pane_current_command` matches `$AGENT_COMMANDS`) that logs
every size it sees on `WINCH`, only when the size changed, which is Node's
rule.

1. Single-pane newborn window, resized after launch: the probe logs a real
   size change after its last pre-ready size, and the window ends at its
   original size with `window-size` restored. Red before (no stamp, no worker).
2. Grid: a lead and two role probes, each launched before the next split,
   then a relayout, all while no client views the window. **Every** probe gets
   exactly one nudge (a zoom-sized change and back), and a probe gets no size
   event from a sibling's nudge. Zoom is off at the end, the active and last
   panes are restored, and the grid geometry is unchanged.
2b. Grid window being viewed, target not active: no zoom happens while the
   user's pane is active. Then the active pane moves to the target, and the
   nudge lands.
3. A newborn agent pane never resized: no size event after ready (no nudge).
4. A `@bridge_win` window, or a window with a float: no nudge.

The test proves the nudge mechanism on the production path, not that a real
app's garble is gone: the garble itself did not reproduce in isolation on this
host.

Plus `nix build .#default`, `nix flake check`, `nix build .#lint`. Docs: a
`scripts.md` row for `tmux-agent-repaint`, the `tmux-update-icons` row, and the
hook budget in `performance.md`.
