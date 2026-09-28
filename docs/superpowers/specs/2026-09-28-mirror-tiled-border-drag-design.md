# Tiled mirror border drag reaches the remote (#823) — design

## Problem

A mouse drag on the divider between two **tiled** panes of a mirror window
resizes only the local renderer panes. The remote panes keep their sizes, so
each remote program renders at its old size into a pane of a different size,
and nothing reconciles the local layout back.

**Violated invariant:** a mirror window's local tiled layout equals the
remote window's tiled layout (cell for cell, local pane *i* rendering remote
pane *i*). The reconcile holds it for every change that starts on the remote;
every structural gesture that starts locally holds it by routing through a ctl
verb (`zoom`, `layout`, `rotate`, the #769 menus, and since #797 a mirror
float's border drag). A tiled border drag is the one gesture left that
reshapes the local mirror behind the daemon's back.

### Evidence (measured, from the issue)

The #797 harness: a vanilla `-L m2src` remote, a `-L m2dst` local with
`mouse on`, daemon `--test-local`, a pty-hosted client fed SGR mouse input.
Remote window split with `split-window -h`, divider dragged 10 columns left.

| step | remote tiled panes | local mirror panes |
|---|---|---|
| before | `50x28 \| 49x28` | `50x28 \| 49x28` |
| divider drag, +2s | `50x28 \| 49x28` | `40x28 \| 59x28` |

### Why

`MouseDrag1Border { resize-pane -M }` is tmux's stock bind. For a tiled pane
it installs `cmd_resize_pane_mouse_update_tiled` as the client's C
`mouse_drag_update` callback, which reshapes the local layout on every motion
event with no hook and no control notification. #797 replaced that bind with a
version-gated `if-shell -F -t =` whose mirror branch fires only for a
`@bridge_pane` **float** (`bridgeGate && pane_floating_flag`); a tiled mirror
pane still takes the stock else-branch.

## Decision (owner): sync the drag to the remote

At drag end the local window's tiled layout is sent to the daemon, which
applies it to the remote window with a remote `select-layout`. Refusing the
gesture was the rejected alternative.

## Contract this touches (map)

The layout string and the pane order are owned by the reconcile.

| edge | symbol / file | disposition |
|---|---|---|
| remote layout → local | `readLayout`/`%layout-change` → `controlmode.ParseLayout` → `Layout.Raw` (v1, tiled-only, remote ids) → `applyLayout`'s local `select-layout` | **compatible** — `ParseLayout`'s output stays byte-identical (the parse is refactored, not changed; existing `layout_test.go` pins it) |
| pane order | `RemotePaneOrder(L)` (tree leaf order) = local pane-list order the reconcile creates (`PlanWindow`, `applyPaneOps`) | **compatible** — unchanged; the new path reads it, never writes it |
| local ↔ remote pane id | `@bridge_pane` pane option, stamped by `spawnRenderer` | **compatible** — read-only here |
| dedup | `mirrorWindow.layout` (last Raw applied locally), compared in `reconcileLayoutFrom`, `reconcileSnapshot`, `applyLayout` | **changed** — a new intent clears it (below); `applyPaneOps` already clears it for the same reason |
| ctl intents | `ctlState.wantLayout`, `takeIntents`, `settle` (`daemon.go`) | **changed** — each layout intent carries the layout a drag sent (or `""`) |
| local → remote layout | **new**: ctl `drag` resolves the stashed pane; tiled → `controlmode.TiledLayout` (local ids → remote ids) → daemon verb `tile-layout` → guarded remote `select-layout` | **new** |
| drag binds | `generator/render/drags.go` + its frozen twin `config/tmux.conf.reference.nix` | **changed** — `MouseDrag1Border`'s gate widens to every mirror pane; the drag-end body calls ctl `drag` (was `float-drag`) |
| float drag | ctl `float-drag` → `float-geom` (#797) | **compatible** — same `float-geom` request, now reached through ctl `drag`'s float branch |
| verb cross-check | `tests/menu-bind-integration.bats` "the drag block sends only float-drag" | **changed** — expects `drag`, and `tile-layout` in ctl.go |
| whichkey | `picker/whichkey.go` skips `og-bridge-drag` by table name | **compatible** |

Absent stages: nothing persists the layout (no persistence edge); no batch or
retry path exists for a ctl request (a failure shows in the status line via
`--display-error`, like every ctl verb).

## Design

### 1. Drag start (generator)

`dragBinds` gates each stock drag bind per its command:

- `MouseDrag1Border { resize-pane -M }` — the only stock drag that reshapes a
  tiled pane — takes the mirror branch for **any** mirror pane: gate
  `bridgeGate` alone.
- `M-MouseDrag1Border`/`M-MouseDrag1Pane { move-pane -M }` keep the float-only
  gate: `cmd_join_pane_mouse_update` returns early for a tiled pane
  (`cmd-join-pane.c`), so they never reshape a tiled layout.

The mirror branch is unchanged: stash `#{pane_id}` in `@og_bridge_drag`, run
the stock command, `switch-client -T og-bridge-drag`. The 160 drag-end binds
are unchanged except the body: `ctl drag #{q:@og_bridge_drag}`. The note
strings describe both cases. `config/tmux.conf.reference.nix` mirrors every
byte (the extraction check diffs them).

A local non-mirror window, and a user float inside a mirror window, still
fail `bridgeGate` and take the stock else-branch, exactly as today.

### 2. ctl `drag <local-pane>` (renamed from `float-drag`)

One `display-message -p -t <local-pane>` reads, atomically:
`@bridge_pane | floating | left | top | width | height | winW | winH |
#{P:#{pane_id}=#{@bridge_pane} } | #{window_layout}`.

- Floating → `float-geom <remote> <local> x y w h winW winH` exactly as #797.
- Tiled → build a local→remote id map from the `P:` pairs (only values that
  are `%digits`), call `controlmode.TiledLayout(window_layout, map)`, and send
  `tile-layout <remote pane> <L.Raw>`.

`#{window_layout}` from a command client is the **v2 JSON** dump
(`format_cb_window_layout` sets the old format only for a control client
without `new-layouts`; measured), with floats as `"z"` leaves anywhere in the
tree. `TiledLayout` accepts either format, as `ParseLayout` does.

### 3. `controlmode.TiledLayout(s, id func(string) (string, bool))`

Parse `s` in either format, prune float leaves (collapsing single-child
splits, as `ParseLayout` does), replace every remaining leaf id with `id(leaf)`
— an unknown id, or a result that is not `%digits`, is an error — and return a
`Layout` whose `Raw` is **always** re-serialized from the tree with a fresh
checksum (never `s` echoed), `Panes` in tree order with the new ids, `W`/`H`
the root (window) size, and `Floats` nil.

`ParseLayout` and `TiledLayout` share one parse helper (tree + floats, both
formats) so the grammar lives once. `ParseLayout`'s results are unchanged,
including `Raw == s` for a v1 input with no floats.

Why the tree order is right: the reconcile creates local panes in the remote's
tree-leaf order and applies layouts positionally, so a local drag (which
changes sizes, never structure) leaves local leaf *i* rendering remote pane
*i*. Mapping ids through `@bridge_pane` rather than trusting position makes a
desynced mirror fail the remote guard instead of reshaping the wrong panes.

### 4. Daemon verb `tile-layout <remote-pane> <layout>`

`build` runs `TiledLayout(arg, identity-with-validation)` — re-parsing and
re-serializing, so the socket peer never forwards raw command text — and emits
one guarded remote command:

```
if-shell -t <pane> -F '<cond>' 'select-layout -t <pane> <quoted Raw>'
cond = #{&&:#{==:#{P/i:#{?pane_floating_flag,,#{pane_id} }},<ids in order, each + " ">},
        #{&&:#{==:#{window_width}x#{window_height},<W>x<H>},#{==:#{window_zoomed_flag},0}}}
```

- **Order guard.** A v1 `select-layout` assigns tiled panes to cells
  positionally in the window's **pane-list** order, skipping floats
  (`layout_assign_fallback_tiled`). `#{P/i:}` walks that same list
  (`SORT_INDEX`, `window_pane_index`); a bare `#{P:}` does **not** — its
  default is `SORT_CREATION`, pane-id order (`format.c` `case 'P'`). Measured
  on the pinned tmux after `rotate-window`: `list-panes` `%2 %1 %3`, `P/i`
  `%2 %1 %3`, bare `P` `%1 %2 %3`. So the guard compares exactly what the
  assignment will use. A remote that re-tiled, gained or lost a pane since the
  drag fails it and is left alone. Measured: matching order applies (`50/49` →
  `40/59`, float at `6,4 18x6` untouched); swapped order is a no-op.
- **Size guard.** The mirror window is `resize-window`ed to the remote's size
  (`FitWindowCmd`), so the layout's root equals the remote window unless the
  remote resized since; `select-layout` would otherwise `window_resize` to the
  string's size and rescale. Mismatch → no-op.
- **Zoom guard.** `select-layout` unzooms; a zoomed remote is left alone.
- **Floats untouched.** The string is v1 tiled-only; `layout_parse` detaches
  and re-attaches every floating cell for a version-1 string ("Preserve
  floating panes for version 1").
- Flags: `layout: true` (reconcile after), not `moves` (select-layout keeps
  the active pane); the request also carries `S` for §5.

### 5. Relayout intent (convergence without a bounce)

Whether the remote's `%layout-change` for our `select-layout` surfaces as its
own line (dispatched to `reconcileLayoutFrom` when `settle` drains async
lines) or only through the verb's `wantLayout` intent (`settle` →
`reconcileLayout` → `readLayout`, ordered after the command on the same
connection), the reconcile applies the remote's truth. The gap is the dedup:
`mirrorWindow.layout` records the last remote `Raw` applied locally, and the
drag reshaped the local window without changing it.

So the verb's intent carries the layout it sent, `S` (the re-serialized,
remote-id tiled string). Before reconciling that window, `settle` clears
`mw.layout` **only when `mw.layout != S`** — i.e. when the daemon's belief
about the local shape is stale:

- **Success, notification already applied this round:** `mw.layout == S`, no
  clear, the intent's pass hits the fast path. Every pane is reseeded once.
- **Success, no notification line:** `mw.layout` is the pre-drag `Raw`, so
  the clear lets the pass apply the remote's new `Raw` (a same-geometry local
  `select-layout`, no visual change) and FrameResize + reseed the resized panes
  — the same path a remote-initiated resize takes.
- **Guard or command failed:** the remote still reports the old `Raw`; the
  clear forces the pass to re-apply it, and the local window snaps back.
- **Drag that moved nothing:** `S` equals the remote `Raw` equals `mw.layout`,
  no clear, no pass work.

No loop: nothing local reacts to the reconcile's own `select-layout` by
messaging the remote. No bounce: on success the local window already shows the
state the reconcile applies.

`ctlState.wantLayout` changes from `map[string]bool` to `map[string]string`
(window → `S`, `""` for every other layout verb); a later non-empty `S` for the
same window replaces an earlier one (the local window shows the last drag),
and `""` never overwrites a pending `S`. `takeIntents` returns the map. No
other intent changes.

## Acceptance

1. Integration (`tests/float-drag-integration.bats`, the #797 harness): a
   10-column divider drag on a two-pane tiled mirror ends with remote and local
   tiled pane sizes equal and the remote's changed (red on `main`, green with
   the fix). Plus: a tiled drag with a mirror float open leaves the float's
   geometry untouched on both sides; a 3-pane nested remote layout; a local
   non-mirror window's divider drag still resizes locally; a drag on a
   `rotate-window`ed remote (list order ≠ pane-id order) syncs; a crafted
   `tile-layout` with the wrong pane order leaves the remote alone and snaps the
   locally reshaped mirror back (guard + relayout).
2. Unit: `TiledLayout` — v2 with a float leaf in the middle of a split, v1
   with a trailing float section, a 3+ pane nested layout, unknown id / non
   `%digits` mapping errors, checksum validity (round-trip through
   `ParseLayout`), `Raw` never echoing a hostile checksum prefix; `ParseLayout`
   results unchanged; for the same tree, `TiledLayout(x).Raw` equals
   `ParseLayout(v2 of x).Raw` (the byte equality §5's stale check relies on). ctl `drag` tiled branch (fake tmux output); daemon
   `tile-layout` command text (`P/i`) and rejection of malformed input;
   relayout intent coalescing and the clear-only-when-stale rule; drag bind
   gates per stock command.
3. `nix build .#default`, `nix flake check`, `nix build .#lint` pass.
4. `docs/agents/bridge-daemon.md`, `docs/agents/floats.md`, README updated.

## Residual (by design, same as #797)

A keyboard key, wheel event, or other button mid-drag drops the client out of
`og-bridge-drag` before the drag end; that drag stays local until the next
routed drag or remote layout change. An older resident server (#407) keeps the
stock local-only drag. A drag whose ctl resolution fails before reaching the
daemon (the stashed pane vanished, or a tiled pane in the mirror has no
`%digits` `@bridge_pane` — a mirror the reconcile already treats as desynced)
registers no intent: the error shows in the status line (`--display-error`)
and the local shape stays until the next remote layout change. A remote tmux
whose `#{P/i:}` does not walk list order, or a remote window whose pane list
is not in its tree-leaf order (never observed: `rotate-window` and
`split-window -fvb` both keep them equal), fails the guard, so the drag snaps
back rather than reshaping the wrong panes.

## Out of scope

#814 (`pumpInput`/`isDismissKey`/`skipX10Mouse`) and #837 (`runMirror`'s
startup loop).
