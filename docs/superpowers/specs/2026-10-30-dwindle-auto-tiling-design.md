# Dwindle-style auto split and balance on close (#1026)

## Goal

Hyprland's dwindle layout, for tmux panes:

1. **Smart split** — a new split picks its direction from the active pane's
   shape: wide → side by side (`-h`), tall → stacked (`-v`).
2. **Balance on close** — when a tiled pane leaves a window, the remaining
   children of the layout cell it left are evened out. Every other cell keeps
   its sizes, including ones the user set by hand.

On by default. `programs.tmux-og.dwindle.enable` sets the default; `prefix + A`
toggles balance-on-close for the current window.

## Measured facts this design rests on

Scratch server (`TMUX_TMPDIR=/tmp/og-…`, `-L probe`, `-f /dev/null`), the live
`next-3.9` binary, and the pinned `tmux-upstream` source:

- **Every close path fires `window-layout-changed` on the source window**:
  `kill-pane`, the process exiting (`pane-exited` also fires), `break-pane`
  (there is no `after-break-pane` hook: `invalid option`), and
  `join-pane`/`move-pane` (`layout_close_pane` notifies). A float open/close
  fires it too, without changing the tiled set.
- **Hooks run deferred.** By the time a `window-layout-changed` hook body
  expands its formats, the pane is already gone: `#{window_layout}` is the
  post-close layout, and nothing names the pane that left (`hook_pane` is
  empty). So the pre-close layout has to be cached.
- **tmux unzooms a window before closing a pane in it.** Zoom pane A, kill pane
  B: both resulting hook runs see `window_zoomed_flag=0`. Zoom itself fires
  `window-layout-changed` with the flag at 1. So "the window was zoomed" can
  only be known from state cached at the previous layout change.
- **`#{window_layout}` is per client.** In a hook, or for an ordinary client,
  it is the v2 JSON form (`{"V":2,"L":{…}}`, floats included as `"z"` leaves);
  for a control client without `new-layouts` it is v1. A cached string can be
  either.
- **`select-layout -E` cannot target one cell.** `layout_spread_out` spreads
  the target pane's parent, and if that is already even it walks up and spreads
  the grandparent — exactly the "other group" the owner wants left alone. It
  also cannot address a cell whose children are all containers.
- **A v1 `select-layout <string>` assigns tiled panes to leaf cells in pane-list
  order**, ignoring the ids in the string (`layout_assign_fallback_tiled`),
  keeps floats in place, and resizes the window to the string's size. The
  bridge daemon already applies v1 strings safely by guarding with `if-shell
  -F` on `#{P/i:…}` pane order, window size and zoom (`tileLayoutCommand`,
  `picker/remotebridge/daemon/ctl.go`).
- **Nested `if -F` ordering:** in `if -F c { if -F d { run-shell "… #{@x}" } ;
  set -w @x new }` the `run-shell` expands `#{@x}` before the `set` runs
  (measured: `arg=old`). `#{q:@x}` passes a v2 JSON value through `sh` intact.
- **Every option write is server-wide work.** `options_push_changes` ends in
  `recalculate_sizes()` and a redraw of every attached client, for any option
  name, user options included (`options.c`). tmux-next re-fires
  `window-layout-changed` on every window for each `refresh-client -C`
  (`config/tmux.conf.tmpl`, #793), so a hook that writes an option on every
  event multiplies that work by the window count. A write must happen only
  when something the cache holds actually changed.
- **Format pieces the gate needs work on the pinned binary:** `#{s/_.*//:@x}`
  extracts a leading field, `#{s/[|].*//:@x}` everything before the first `|`,
  `#{m/r:^[0-9]+_0_,…}` a regex test; an unset option gives `""`, and
  `#{e|<|:3,}` is `0`.

## Balance on close: two designs

### A. `select-layout -E` from a post-close hook

`after-kill-pane` / `pane-exited` run `select-layout -E -t <survivor>`. Rejected:
the walk-up spreads unrelated cells when the parent is already even; there is
no `after-break-pane` hook; a parent whose remaining children are all
containers has no leaf to target; and knowing which survivor sits in the
closed pane's cell still needs the pre-close tree.

### B. Cache the layout, diff it, rewrite only the changed cell (chosen)

- **One cached option, written only on a real change.** The window option
  `@dwindle_cache` holds `SIG|LAYOUT`, where `LAYOUT` is `#{window_layout}` and
  `SIG` is a client-independent signature:

  ```
  SIG = #{n:#{P:#{?pane_floating_flag,,x}}}_#{window_zoomed_flag}_#{window_width}x#{window_height}_#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}
  ```

  (tiled count, zoom, window size, every tiled pane's geometry; the same
  `#{P:}` geometry `tmux-grid-refit`'s gate uses, never `window_layout`, which
  renders per client). `_` appears only as the three field separators, and no
  field contains `|`. `SIG` keys on leaf geometry, not tree shape: two trees
  with identical pane geometry (only reachable through `select-layout
  <string>`) leave `LAYOUT` stale without a write. A close then diffs against
  the wrong tree, and steps 2–3 or the apply guard turn that into an untouched
  exit in all but the case where the stale cell happens to match, which evens
  one cell the pane did sit beside.
- **The hook.** `window-layout-changed[20]`, beside the grid hook at `[0]`:

  ```
  if -F '#{&&:#{==:#{@dwindle},on},#{&&:#{!:#{@bridge_win}},#{!=:SIG,#{s/[|].*//:@dwindle_cache}}}}' {
    if -F '#{&&:#{e|<|:COUNT,#{s/_.*//:@dwindle_cache}},#{&&:#{m/r:^[0-9]+_0_,#{@dwindle_cache}},#{&&:#{!:#{window_zoomed_flag}},#{!=:#{@crew_grid},1}}}}' {
      run-shell -b "<tmux-dwindle> balance #{q:window_id} #{q:@dwindle_cache}"
    }
    set -wF @dwindle_cache 'SIG|#{window_layout}'
  }
  ```

  `COUNT` is the signature's first field. An event whose signature equals the
  cached one (every no-op `refresh-client -C` re-fire, a float that only moved)
  evaluates one `if -F` and writes nothing. A real change writes the one option
  once. It forks only when the tiled count dropped below the cached count in an
  eligible window. Splits, float open/close and `select-layout` never drop the
  tiled count, so they fork nothing. The inner `run-shell` expands
  `@dwindle_cache` before the `set` replaces it (measured above), so the binary
  gets the pre-close cache as an argument.
- **`tmux-dwindle`, a new Go binary** (`picker/dwindle`, `subPackages` +
  `postInstall` rename in `picker/default.nix`, a `bin.tmux-dwindle` path in
  `paths.toml`/`generator/paths`). The pure tree work is a new exported
  function in `picker/remotebridge/controlmode` (`rebalance.go`), beside the
  unexported tree type, parser, `pruneFloats`, `writeCell` and
  `layoutChecksum` it needs; it reads both v1 and v2 and prunes floats.
  `picker/dwindle` only does tmux I/O:
  1. Parse the cached (pre-close) layout from the `LAYOUT` half of its
     argument and read the current state with one `display-message`: the
     guard fields below plus `#{window_layout}`.
  2. Exactly one tiled pane must have left (`old − new = {P}`) and none
     arrived (`new ⊆ old`); otherwise exit 0 untouched (`kill-pane -a`, two
     closes coalesced, a swap/rotate the cache missed).
  3. `P`'s parent cell `C` in the old tree had `k` children. `k == 2`: `C`
     collapsed and the sibling inherited all of it — that *is* dwindle, so exit.
     `k ≥ 3`: find the current cell `Q` with `C`'s orientation whose children,
     by pane-id set, are `C`'s children minus `P`, in order. Not found → exit.
  4. Even `Q`'s children along `Q`'s axis, keeping their total: with `T` the sum
     of their sizes and `n` the count, each gets `T/n` and the last takes the
     remainder (the same split tmux's `layout_spread_cell` computes). With
     `pane-border-status` top/bottom, the child touching that window edge
     loses a row to its status line, so — like `layout_spread_cell` — the
     pane heights are evened and that child's cell gets one extra row (the
     binary reads the option in its state read). Already
     even → exit. Each resized child's subtree is rescaled: a perpendicular
     container gives every child the new size; a same-axis container splits
     the new size among its children in proportion to their old sizes (largest
     remainder); offsets are recomputed inside `Q` only. Any leaf that would
     drop below 1 cell → exit untouched.
  5. **Positional precondition.** A v1 `select-layout` hands tiled panes to
     leaves in pane-list order, so the emitted tree's leaf order (depth-first)
     must equal the tiled pane-list order step 1 read (`#{P/i:}`, which walks
     the list; bare `#{P:}` sorts by id, per `tileLayoutCommand`). Differ →
     exit untouched. (No tmux command probed — `split-window -b/-f/-fb`,
     `move-pane -b`, `rotate-window` — produced a divergence, but nothing
     guarantees it, and a wrong guess swaps panes.)
  6. Apply with one guarded command:
     `if-shell -F '<guard>' 'select-layout -t @W <v1 string>'`. The guard
     re-checks at apply time, inside the server, that the mode is still on, the
     window is not zoomed, not `@crew_grid=1`, not a mirror, and that
     `#{window_width}x#{window_height}` plus every tiled pane's
     `id.left.top.width.height` in `#{P/i:}` order is byte-identical to what
     step 1 read. Step 1's `#{P/i:}` ids are, by step 5, the emitted leaf
     order, so the guard pins the positional assignment and "nothing moved
     since I looked" together. A stale guard is a silent no-op.
- The apply fires `window-layout-changed` again; the tiled count is unchanged,
  so the hook only re-stamps.

## Exclusions

All are checked in the hook's `if -F` gate (so an excluded close forks
nothing) and again in the apply guard:

| Case | How it is excluded |
|---|---|
| Mode off (global or window) | `#{==:#{@dwindle},on}` is the outer gate: no write, no fork |
| Dispatcher grid (`@crew_grid=1`) | balance gate requires `#{!=:#{@crew_grid},1}`; `tmux-grid-refit` keeps owning that layout. The cache is still kept, so a window that stops being a grid diffs against a truthful cache |
| Float closed | the tiled count (`COUNT`) does not drop |
| Zoomed window | balance gate requires the live `window_zoomed_flag` and the cached zoom field (`#{m/r:^[0-9]+_0_,…}`) to be 0 — tmux unzooms before the close, and zoom fires `window-layout-changed`, so the cache recorded it |
| `@bridge_win` mirror | outer gate requires `#{!:#{@bridge_win}}`: no write, no fork, never a local `select-layout`; the remote balances when it runs tmux-og with the mode on, and the daemon mirrors the result |
| tmux-remux restore | restore creates panes (`new-window`, `split-window`) and applies `select-layout <saved>`; none of those drops the tiled count, so the gate never forks and saved sizes stay exact. Undo (`prefix + u`) re-creates a pane, same. One edge: a restored pane whose relaunch exits before remux's `select-layout` is a real close. It forks; the binary then either diffs against a layout remux has since replaced (structure mismatch or a stale guard → untouched) or evens only the one cell that pane left, which is what a close does anywhere. Every other cell keeps its saved sizes |
| Cache never written (window predates the config load) | the count field is `""`, so `#{e|<|:N,}` is `0`; the first event writes the cache |

A tiled pane turned into a float (`prefix + @`, `break-pane -W`) leaves the
tiled set, so it balances like a close. A float joined back (`join-pane`) only
grows the set.

## Smart split

`prefix + Enter` (free in the prefix table: checked against the live
`list-keys -T prefix`, the template and `generator/render/stock*.txt`), note
`Split pane by its shape`:

- Wide means `pane_width > 2 * pane_height`. A cell is about twice as tall as it
  is wide, so this compares the pane's pixel width with its pixel height, the
  comparison Hyprland makes. The constant matches `tmux-grid-refit`'s
  `@grid_refit_aspect` default for the same reason. Strict `>`: a
  pixel-square pane (`width == 2 * height`) splits stacked, as Hyprland does.
- Local: `if -F '<wide>' { split-window -h -c "#{pane_current_path}" } {
  split-window -v -c "#{pane_current_path}" }` — in-process, no fork.
- Mirror (`BridgeGate`): `run-shell "<ctl> #{?<wide>,split-h,split-v}
  #{q:@bridge_pane}"`, the existing `og-remote-bridge-ctl split-h`/`split-v`
  path, deciding from the mirror pane's local size, which tracks the remote
  pane it renders.
- Always smart, independent of `@dwindle`: the key is pressed on purpose, and
  the mode governs what happens on close.

**Explicit binds stay explicit.** `|` and `_` are unchanged, and tmux's own `%`
and `"` stay unbound (the config retired them for `|`/`_`). Making either pair
smart while the mode is on would remove the only way to force a direction and
give one key two meanings depending on a toggle. The right-click menu's
Horizontal/Vertical entries are labelled with a direction and stay as they are.

## Option and toggle

- `programs.tmux-og.dwindle.enable` (bool, default `true`) → `dwindleEnable`
  (`config/tmux.conf.nix`) → `config.toml` `[dwindle] enable` → generator
  `Config.Dwindle.Enable` → `set -gw @dwindle "on|off"` in the template, and the
  same line in `config/tmux.conf.reference.nix` (two-file rule). Off-Nix
  (`og init`), an absent key defaults to `true` too (`applyDefaults`), so every
  install path is on by default.
- Window override: `set -w @dwindle off|on`. `#{@dwindle}` resolves the window's
  option first, then the global window default.
- `prefix + A` (`Toggle dwindle auto-balance for this window`):
  `set -wF @dwindle '#{?#{==:#{@dwindle},on},off,on}' ; display-message
  "dwindle auto-balance: #{@dwindle} (this window)"`. In a mirror it only
  displays `dwindle: not mirrored - toggle it on the remote` (the float/tile
  bind's wording), and in a `@crew_grid=1` window only `dwindle: grid window -
  tmux-grid-refit owns this layout`; neither flips the option, since it has no
  effect there.
- Turning the mode back on for a window: the cache was not maintained while
  off, so it may be stale. The first close after re-enabling then diffs
  against a stale tree: either more than one pane differs, the parent cell is
  not found, or the guard sees different geometry, and each of those exits
  untouched (worst case: that one close does not balance). The close itself
  rewrites the cache, so every later close is exact.

## Cost

- Mode on, no-op event (the #793 `refresh-client -C` re-fire on an unchanged
  window, a float move): one in-process `if -F` that expands the signature (a
  `#{P:}` loop over the window's panes) and compares it with the cache. No
  option write, no fork.
- Mode on, real change (split, close, pane or window resize, zoom, preset): one
  `set -wF` (one `recalculate_sizes` + redraw, on top of the one the change
  itself already caused), plus one fork only on a tiled-count drop in an
  eligible window. A storm with real size changes costs one write per window
  per change, the same as the float/grid gates' pending markers. Unlike those
  gates this one is on in every window by default, so a mouse border drag
  writes the cache once per motion step.
- Mode off: one `if -F` evaluation per event, nothing else.
- Pinned by a bats case: plant a cache whose `SIG` half matches the window
  and whose `LAYOUT` half is a sentinel, fire a layout-changed event that
  changes nothing, and assert the sentinel survives (a write would replace it).
- Smart split, toggle: in-process; the mirror split's `run-shell` is the same
  one `|`/`_` already pay.

## Version gating

No new hook or command flag. `#{n:}`, `#{P/i:}`, `#{e|…|:}` and
`pane_floating_flag` exist on every server this config supports (the `-B`
monitor hooks already require 3.8). On a server without floats,
`pane_floating_flag` expands empty and every pane counts as tiled, which is
correct there. The parser takes v1 or v2, so a pre-v2 resident server works
unchanged.

## Testing

- **Go unit tests** (`picker/remotebridge/controlmode`, `rebalance_test.go`):
  the cell diff and rebalance on layout strings, both v1 and v2: three columns minus the middle; a nested cell where
  a hand-sized sibling group must be byte-identical afterwards; `k == 2`
  collapse → no change; a cell whose children are all containers; floats in a
  v2 string; two panes gone / one arrived / structure mismatch / already even
  → no change; minimum-size abort; the emitted v1 checksum validates; a cached
  tree that differs from the live one at equal geometry. In `picker/dwindle`
  (I/O only): a pane-list order that differs from the leaf order issues no
  `select-layout`, and the emitted command carries the guard.
- **bats on a scratch server** (`tests/dwindle.bats`, new check
  `dwindle-tests` built like `float-tool-focus-tests`: the real wrapped tmux and
  rendered conf, plus an attached client for keypresses). Each new-behaviour
  case fails on `main`:
  - three equal columns, kill the middle: the two left are within 1 column;
  - `[A/B] | [C/D/E]` with A/B resized by hand: kill D, C and E even out, A/B's
    heights and both columns' widths unchanged;
  - `prefix + Enter` on a wide pane splits side by side, on a tall pane stacked;
  - mode off globally (`set -gw @dwindle off`), and per window via `prefix + A`:
    a close leaves tmux's own sizes;
  - no-op on a `@crew_grid=1` window, on a float close, on a zoomed window;
  - a no-op layout event writes no cache (the sentinel case under Cost);
  - the rendered `prefix + Enter` line routes a mirror through
    `og-remote-bridge-ctl #{?…,split-h,split-v} #{q:@bridge_pane}` (a conf
    assertion: driving the mirror branch needs a live bridge, which the
    daemon's own tests own).
  Negative cases wait until the cache's count field shows the post-close count
  (proof the gate ran and chose not to fork) instead of sleeping.
- The extraction check (`og generate` vs the frozen reference) covers the new
  template lines, and matrix entries 11/12 move `dwindleEnable` both ways.

## Docs

README (prefix bindings table: `Enter`, `A`; the option), a `tmux-dwindle` row
in `docs/agents/scripts.md`, and a line in `docs/agents/performance.md` on the
`window-layout-changed[20]` gate's cost.

## Out of scope

Balancing on split (Hyprland halves the target node, and tmux's split already
does that); remote-side behaviour beyond what the remote's own tmux-og does;
the right-click menu.
