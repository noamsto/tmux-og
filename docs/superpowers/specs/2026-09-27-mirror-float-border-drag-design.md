# Mirror float border drag reaches the remote (#797) — design

## Problem

A mouse drag on a **mirrored float's border** moves or resizes only the local
mirror float. The remote float keeps its geometry, and the daemon never
re-asserts it. So the renderer paints the remote screen at its real size into a
pane of a different size, and a click in the local-only columns reaches the
remote program at a column outside its pane.

**Violated invariant:** a `@bridge_pane` float's local geometry equals its
remote float's geometry, except for the `outerFromCell` clamp into the local
window. Every other structural gesture on a mirror already holds this
invariant by routing through a ctl verb (`zoom`, `layout`, `rotate`, the #769
menus).

### Evidence (measured on the pinned `next-3.9`, this worktree)

This is the issue's setup: a vanilla `-L m2src` remote and a `-L m2dst` local
with `mouse on`, daemon `--test-local`, the remote float `new-pane -x 40 -y 12
-X 10 -Y 5`, and a pty-hosted client on `-L m2obs` fed SGR mouse input.

| step | remote float | local mirror float |
|---|---|---|
| before | `11,6 38x10` | `11,6 38x10` |
| left-border drag 5 cols left, +2s | `11,6 38x10` | `6,6 43x10` |

**Tiled mirror panes diverge the same way (measured, same harness).** A
two-pane mirror had both sides at `50x28 | 49x28`. A drag on the divider moved
it 10 columns, and 2s later the local panes were `40x28 | 59x28` while the
remote stayed at `50x28 | 49x28`.

### Why local tmux does this

The drag goes through tmux's stock root bindings. Neither is overridden by
tmux-og (`rg MouseDrag` over the config, the generator and the plugins finds
nothing):

- `MouseDrag1Border { resize-pane -M }`: for a float,
  `cmd_resize_pane_mouse_resize_move_floating`. It resizes when you drag an
  edge or a corner, and **moves** the float when you drag the top border.
- `M-MouseDrag1Border { move-pane -M }`: `cmd_join_pane_mouse_move`, a
  float-only move.

Both bindings install a C `mouse_drag_update` callback. That callback runs on
every later drag event **before** the key tables are consulted, and it fires
no hook, no `window-layout-changed` and no control notification
(`layout_set_size` + `layout_fix_panes` only). So nothing observable happens
during the drag. The one event that reaches a key table afterwards is the
**drag end**: `MouseDragEnd1<loc>`, where `<loc>` is wherever the button was
released (`server-client.c:1031-1052`).

## Decision: route the drag to the remote

| | Route (chosen) | Refuse the local gesture |
|---|---|---|
| user sees | the float follows the drag live (local tmux draws it), then the remote takes the new geometry and the mirror confirms it | the border cannot be dragged on a mirror float |
| faithful mirror | yes: the remote program is resized, so the pane has no dead cells | yes, but the gesture is lost |
| mechanism | a drag-end trigger plus a ctl verb, the same shape as `prefix + z` → `zoom` | a gated bind with no `resize-pane -M` |
| failure mode | a drag end the trigger misses leaves a divergence until the next drag (see Residual) | none |

Routing is viable because the drag end **can** be intercepted without
patching tmux. So the preference the dispatcher stated, which also keeps the
mirror faithful, wins. Refusing stays the fallback if review finds the trigger
unsound.

## Mechanism

### 1. Drag start: mark the drag (config, generated)

Two new root bindings, generated in `generator/render` beside `menuBinds`.
Each is gated on the mouse target (`-t =`): it is a live mirror pane
(`bridgeGate`) **and** a float (`#{pane_floating_flag}`).

```
MouseDrag1Border   if -F -t = '<gate && floating>' { <stash> ; resize-pane -M ; switch-client -T og-bridge-drag } "resize-pane -M"
M-MouseDrag1Border if -F -t = '<gate && floating>' { <stash> ; move-pane -M   ; switch-client -T og-bridge-drag } "move-pane -M"
```

- The else-branch is the stock command, as a **string** branch, the way
  `menuBinds` does it. tmux parses a string branch only when it runs, and
  `move-pane -M` is next-only. The block is version-gated like the menu block
  (see Compatibility).
- `<stash>` is `set -F @og_bridge_drag '#{pane_id}'`, a **session** option
  holding the local float's pane id. It is written by the command list that
  enters the table, and read only from inside that table, so it can never
  outlive the drag it names. The drag end needs it because its own mouse
  target is wherever the button was released, not the float.
- Tiled mirror panes, and every non-mirror pane, keep the stock behaviour
  byte for byte (the gate is false).

### 2. Drag end: route the geometry (config, generated)

`switch-client -T og-bridge-drag` makes the **next key** the client produces
look up that table first. During a drag, that next key is always the drag end:
drag updates skip the key tables entirely. After the matched binding runs,
tmux resets the client to the root table (a non-repeat binding), so the table
is one-shot.

The table binds `MouseDragEnd1<loc>` for **every** mouse location tmux
defines (`KEYC_MOUSE_STRING`, `tmux.h:277-297`): `Pane`, `Status`,
`StatusLeft`, `StatusRight`, `StatusDefault`, `ScrollbarUp`,
`ScrollbarSlider`, `ScrollbarDown`, `Empty`, `Border` and `Control0..9`.

Each location is bound under **every combination of the `M-`, `C-` and `S-`
modifiers**. `server_client_check_mouse` ORs `KEYC_META`, `KEYC_CTRL` and
`KEYC_SHIFT` into the key from the *releasing* event's button byte
(`server-client.c:1124-1130`), and a modifier can be pressed or released
during the drag. That gives 20 locations × 8 combinations = 160 bindings,
generated from two Go lists, so the table is complete by construction.

Each binding is one line, and every format in it is `#{q:}`-quoted, as
`tests/conf-shell-quoting.bats` requires of a `run-shell` shell string:

```
bind-key -T og-bridge-drag <mods>MouseDragEnd1<loc> run-shell "<bridgeCtl> float-drag #{q:@og_bridge_drag}"
```

**The binding hands over the stashed local pane id, and nothing else.** A
tmux target is not format-expanded (`cmd_find_target` never calls
`format_*`), so `run-shell -t '#{@og_bridge_drag}'` cannot retarget onto the
float. Measured on a scratch server with the stashed pane inactive, the
command ran with no pane at all. Every other value this binding could expand
comes from the *release* target, which may be another pane or the status
line. So ctl resolves the float itself (§2a).

`Any` is not used: it would also match a keyboard key typed mid-drag,
swallowing it.

`@og_bridge_drag` is a session option. It resolves the same wherever in the
client's session the button is released, and so does `@bridge_sock`, which
`bridgeCtl` reads.

### 2a. `ctl float-drag` resolves the float (local, `cmd/ctl`)

`og-remote-bridge-ctl float-drag <local-pane>` is a ctl-side gesture, like
the `focus` sequence stamping ctl already does. It checks that the argument
matches `^%[0-9]+$`, then runs one local
`tmux display-message -p -t <local-pane> '#{@bridge_pane}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{window_width}|#{window_height}'`,
`|`-delimited per the repo convention. It fails, through the usual
`--display-error` path, when the pane is gone, carries no `@bridge_pane`, or
is not floating, or when the resolved `@bridge_pane` does not match
`^%[0-9]+$`. Otherwise it sends the daemon
`float-geom <@bridge_pane> <x> <y> <w> <h> <winW> <winH>`. The daemon verb
stays a pure translation of explicit arguments, validated and testable like
every other verb. The local pane lookup lives on the host where the local
panes are.

### 3. The `float-geom` verb (daemon, `ctl.go`)

`float-geom <pane> <x> <y> <w> <h> <winW> <winH>` takes 6 args, all
validated as decimal integers with bounds: `w`, `h` in `1..9999`; `winW`,
`winH` in `3..9999`, the smallest window a bordered box fits in; `x`, `y` in `-9999..9999`, because a float dragged partly off
screen reports a negative `pane_left` (measured `-5`). The request carries the
**local inner box** (`pane_*` equals the float's layout cell).

The verb clamps the box into the local window with the same rule the
reconcile uses to place the local float. It runs
`outerFromCell(cell, winW, winH)`, where size wins and the box slides back
in, and then derives the inner box back from that clamped **outer** box:
`x = ox + floatInset`, `y = oy + floatInset`, `w = ow - 2*floatInset`,
`h = oh - 2*floatInset`. The inner box below is that clamped one. This assumes the remote window is
the mirror window's size, as the reconcile already does (the daemon converges
the remote client to the local size). Sending the
unclamped inner box would let the reconcile snap the local float away from
where the remote put it. The daemon then sends one remote command:

```
if -F -t %R '#{pane_floating_flag}' {
  if -F -t %R '#{==:#{pane-border-lines},none}'
    { move-pane -t %R -X <x> -Y <y> ; resize-pane -t %R -x <w> -y <h> }
    { move-pane -t %R -X <x-1> -Y <y-1> ; resize-pane -t %R -x <w+2> -y <h+2> }
}
```

- **The inner box is the unit.** `-X/-Y/-x/-y` speak the remote float's
  **outer** box, and the inset depends on that float's own border.
  `window_pane_get_pane_lines` reads a float's `pane-border-lines` from its
  pane options, which is exactly what `#{pane-border-lines}` evaluates on the
  pane target. Measured: `single`/`double`/`heavy` floats and a `-B none`
  float all land on the requested inner box. So the remote program ends up
  exactly the size of the local pane, whatever border the remote float has.
- **Move before resize.** `resize-pane -y` bumps the height by one when
  `pane-border-status top` and `yoff == 1`, and that test reads the *current*
  yoff. Moving first means the test sees the final position.
- **The outer guard is `pane_floating_flag`.** If the remote pane was re-tiled
  after the drag began, the verb does nothing instead of `resize-pane -x` on a
  tiled pane.
- **Verb flags: `layout: true`, nothing else.** The verb schedules a layout
  reconcile for the window. The remote `move-pane -X/-Y`
  (`cmd_join_pane_move`) and `resize-pane -x/-y` do not change the remote
  active pane, so the verb is not `moves`; nor is it `reseed`, `windows` or
  `needsView`. Locally, tmux's own drag calls `window_set_active_pane` with no
  `after-select-pane`, so local focus can differ from remote focus after any
  border drag. That predates this change (the stock `MouseDown1Border
  select-pane -M` does it too) and is out of scope.
- The fan-out of the `if` is absorbed by the unconditional barrier `stampAll`
  writes behind every command (#723). No new reply accounting is needed.

### 4. Reconcile (unchanged code)

`move-pane` and `resize-pane` each fire `window-layout-changed` on the remote,
so the daemon receives `%layout-change`. A float geometry change already takes
`reconcileLayout`'s read-first path. There `planFloatOps` diffs the mirror's
last-applied cell against the remote's new one and emits a `Move`, and the
local `resize-pane`/`move-pane` puts the float at `outerFromCell(new cell)`.
Because the verb clamped with the same rule, that is the geometry the user
dragged to (clamped), so the move is a no-op on screen, and the renderer is
re-sized and re-seeded as for any geometry change. **The reconcile path is not
modified.**

## Scope

**In:**

- `MouseDrag1Border` and `M-MouseDrag1Border` on a daemon-owned mirror float.
  Every drag shape tmux offers on a float is covered: each edge, each corner,
  and the top-border move.
- The `float-geom` verb, and ctl's `float-drag` resolution.
- The which-key picker (`picker/whichkey.go`) skips the `og-bridge-drag`
  table. Its 160 mouse rows are not keys a person presses from a menu.
- An end-to-end bats test, a generator test, and Go unit tests for the verb.
- Doc updates in `floats.md` and `bridge-daemon.md`.

**Out:**

- **Tiled mirror panes.** They are measured divergent, so a follow-up issue
  will be filed. Routing them needs a different remote command: the local
  window's tiled layout rebuilt as a v1 tiled-only string with remote pane
  order, then a remote `select-layout`. It also touches the pane-order and
  layout-string contract the reconcile owns, and #808 is in flight there. The
  drag-end table built here is reusable for it.
- User floats in a mirror window (`prefix + b`/`k`/`i`, which have no
  `@bridge_pane`). They are local, so a local drag is correct.
- A modal remote float mirrors as a non-modal local float, and it is covered
  like any other.

## Residual (documented, not fixed)

- Any key that is **not a button-1 drag end**, arriving mid-drag, drops the
  client out of `og-bridge-drag` before the drag end: a keyboard key, a wheel
  event (the wheel does not stop a drag, `server-client.c:1031-1035`), or a
  different button pressed mid-drag, which ends the drag as
  `MouseDragEnd<that button>`. The table falls back to root and handles that
  key normally, and the divergence then lasts until the next routed drag or
  remote float change. `Any` would not close this: it would fire the sync at
  a mid-drag geometry, swallow the key, and still miss the real end. And an
  `Any` that re-entered the table could strand the client there with every
  key swallowed, if the drag ended without a key (the modal-pane branch of
  `server_client_check_mouse` clears the drag flag and returns no key).
  One more cost of the fallback: when the first table tried is not root and
  the key is unbound in root too, tmux drops it instead of forwarding it to
  the pane (`server-client.c:1546-1553`). So a plain letter typed mid-drag is
  lost once, the same as it would be after any `switch-client -T`.
- A release **outside every location** (past the window's bottom-right, which
  `server_client_check_mouse` answers with no key at all) leaves tmux's drag
  flag set. The drag end then fires on the next mouse event, still inside the
  table, so this case is caught, just late.
- `@og_bridge_drag` is a **session** option, so two clients of one session
  dragging floats at the same moment share it; the later drag start wins.
- **Mid-drag rendering.** Until the drag ends, the renderer paints the remote
  screen at its old size into the resizing pane. This is transient and is
  what local tmux shows while dragging.

## Compatibility

- The drag binds sit in a `%if "#{==:#{version},next-3.9}"` block, like
  `menuBinds` (#407). An older resident server keeps tmux's own stock
  bindings. Floats, and so mirror floats, exist only on next.
- The ctl protocol version is unchanged. An older daemon answers
  `float-geom` with `unknown verb`, surfaced through `--display-error`,
  exactly like any verb added before (#784/#787 set the precedent).
- Stock drift tripwire: the two stock drag bindings (`root MouseDrag1Border`
  and `root M-MouseDrag1Border`) go in their own pin file,
  `generator/render/stockdrags.txt`, as verbatim `list-keys` lines. They
  cannot go in `stockmenus.txt`: `menuBinds` panics on an entry with no
  mirror menu. The generator reads each else-branch from that file, and the
  existing stock tripwire test in `menu-bind-integration.bats` diffs it
  against `list-keys` on the pinned binary exactly as it does `stockmenus.txt`.

## Acceptance

1. An end-to-end bats test with a real daemon, the real generated config on
   the local server, a pty-hosted client and SGR mouse input. After each of
   these on a mirror float, the remote and local inner geometry agree within
   2s:
   a. a left-border drag, released on the border;
   b. a top-border move, released on the border;
   c. an `M-` drag (`move-pane -M`);
   d. an edge drag released off the float, over another pane, with the
      local active pane moved off the float mid-drag (`select-pane`), which
      proves the stash, not the active pane, picks the float.

   Red on the pre-fix config (the bindings removed), green with the fix.
2. Go unit tests for ctl's `float-drag` resolution (argument check, the
   `display-message` it runs, and the refusals), and for `float-geom`: argument count and integer bounds, the
   clamp into the window (inner box derived from the clamped outer), the
   emitted remote command for the bordered and `none` branches, and the
   `layout`-only flags.
3. A generator test: the drag block's gate, both stock else-branches read
   from `stockdrags.txt`, and the drag-end table holding exactly 20 locations
   × 8 modifier combinations, each running `ctl float-drag #{q:@og_bridge_drag}`,
   and the whole conf passing `tests/conf-shell-quoting.bats`.
4. `docs/agents/floats.md` and `docs/agents/bridge-daemon.md` are updated.
5. `nix build .#default`, `nix flake check` and `nix build .#lint` pass.
