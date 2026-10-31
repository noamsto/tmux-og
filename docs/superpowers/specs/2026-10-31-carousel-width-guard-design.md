# Carousel width guard (#1056) — design

## Problem

An aeye image-carousel side split can end up 1 column wide. tmux then keeps it
there: a pane at the minimum cannot shrink further, and growth is shared
evenly, so every later window resize, reattach or tmux-remux restore preserves
the squeeze (measured: 152/1 at 154 → 78/1 at 80 → 115/38 at 154; a fresh
`resize-window` after `resize-pane -x 152` keeps the carousel at 1). The
investigation (`findings.md` in the branch's crew artifacts) found no automatic
trigger; every reproduction is an explicit full-width action
(`resize-pane -x 100%`, a repeated `bind -r` resize, a far-right border drag,
`main-vertical` applied at 80 columns). The owner chose a self-healing guard
over chasing the trigger.

## Goal

Whenever a tiled carousel pane is narrower than a minimum, restore it to a
sane width, from the hooks that fire on every layout or size change, without
forking on the hot path and without fighting any other geometry owner.

Non-goals: a bottom (top/bottom) carousel squeezed in height; floating
carousels (`tmux-float-refit` owns them); mirror windows (the remote runs this
same guard on its own server); preventing the trigger.

Deployment assumption: the squeezes were reported in mirror windows, whose
geometry is the remote's. The fix takes effect there only once the remote host
runs a tmux-og build with this guard and has reloaded its conf. The
investigation read `halo`'s hooks without these refit hooks present, so `halo`
may be on an older conf; that is a rollout step, not part of this change.

## Recognising a carousel pane

aeye's launcher (`tmux-claude-images`, `prefix + I`) stamps the viewer pane
with `@claude_img_src` (`<srv>-<host pane number>`) right after `split-window`;
`tmux-carousel-restore` (the remux relaunch) stamps it before it execs the
viewer. So:

- carousel = non-empty pane option `@claude_img_src`, and
- tiled = `#{!:#{pane_floating_flag}}`.

The split axis is deliberately not consulted. `@claude_img_axis` is unreliable:
`tmux-carousel-restore` stamps a literal `side` for every restored viewer, and
aeye's `s` toggle runs `move-pane` before it rewrites the option, so the
layout-change event sees the old axis. Width alone is the test: a bottom
carousel spans its host's column, so it is under 20 columns only when that
whole column is, and restoring the width is then right as well.

`#{m:*/bin/aeye *,#{pane_start_command}}` (the border's fallback) is not
needed: both launch paths that create a tiled viewer stamp the option before
the viewer runs.
`@bridge_img_src` (a mirrored viewer) is irrelevant: mirror windows are
skipped wholesale.

The host pane is `%<number>` from the `@claude_img_src` suffix (the same
derivation `tmux-update-icons` uses for the remux stamp).

## Thresholds

- **`MIN_COLS = 20`** — a carousel narrower than this is squeezed. Every
  squeezed width observed was 1–2; the narrowest legitimate, proportional
  carousel measured was 23 (a 141/108 layout restored at 80 columns, `h1.sh`),
  which must stay untouched. 20 sits between the two.
- **`MAIN_MIN_COLS = 40`** — the host keeps at least this many columns after a
  restore. Below it the agent pane beside the carousel is no longer usable.
- **`SHARE_PCT = 40`** — restore target as a share of the space the carousel
  splits with its host. A fresh `prefix + I` split is 50%; 40% leaves the
  agent pane the larger half after a restore. Recording a "last healthy width"
  was rejected: it needs an option write on every healthy layout change, and
  every option write recalculates sizes and redraws every client (#793).

## Restore target

The host is *paired* when it is a tiled pane in the same window directly left
of the carousel (`host_left + host_width + 1 == carousel_left`) with the
identical top and height: the cell the two split.

`resize-pane -x` (tmux `layout_resize_pane`) walks up to the pane's nearest
left-right ancestor and adjusts the child on the pane's path, borrowing from
its next sibling, unless that child is the last one, in which case the previous
sibling is adjusted and the pane's left border moves. Geometry alone cannot
tell which pane of a nested row is last, so the donor is chosen by three rules:

1. **Carousel at the window's right edge** (`left + width == window_width`): it
   is the last child at every left-right level, so
   `resize-pane -t <carousel> -x <target>` moves its left border and takes the
   columns from the cell on its left. `S = host_width + 1 + carousel_width`
   when paired, else `S = window_width`.
2. **Otherwise, a paired host**: the host ends a cell whose next sibling holds
   the carousel, so `resize-pane -t <host> -x <S - 1 - target>` with
   `S = host_width + 1 + carousel_width` moves their shared border.
3. **Otherwise the carousel is declined**: resizing it would take the columns
   from the pane on its right (measured: `[A 100 | [H 15 / C 15] | X 37]` at
   154 went to `A 90, H/C 61, X 1`). The gate's marker stops further forks at
   that geometry.

If `S < MIN_COLS + 1 + MAIN_MIN_COLS` (61) the guard does nothing (too narrow
for a usable pair). Otherwise `target = max(MIN_COLS, S * SHARE_PCT / 100)`. In
the reported case (154 wide, 152/1) that is 61, leaving the agent pane 92.

Using the pair rather than the window keeps a three-pane `A | H | C` window from
crushing `H`. Resizing the host whenever it sat on the left was rejected: in
`[[K | H] / Y] | C` the host is the last pane of its nested row, so resizing it
moved its own left border and left the carousel at 1 column. The guard restores
the first fixable carousel per run, skipping declined ones; the resize re-fires
the gate for any other one, against fresh widths.

**Verify and roll back.** The rules above are a first guess: tmux takes the
columns from one sibling cell in the layout tree, which geometry cannot see.
Measured leaks at the right edge: even-horizontal `38/38/74/1` at 154 went to
`38/38/14/61`; `[[K 25 | H 70] / Y] | C` at 98 left K at 6;
`[A 100 | H 51 | [C / D]]` left H at 1. So the script also snapshots
`#{window_layout}` in its `list-panes` read (its own client renders and
re-parses that format, so `select-layout` restores the exact geometry), and
the apply is one command list:

```
if -F -t <carousel> '<still tiled>' {
  set -w -t <window> @carousel_guard_busy 1 ; resize-pane -t <pane> -x <cols> ;
  if -F -t <carousel> '<carousel ≥ 20 and no other tiled pane < 20>'
    { set -wu -t <window> @carousel_guard_sig }
    { select-layout -t <window> '<snapshot>' ; set -wF -t <window> @carousel_guard_sig '<SIG>' }
} { <clear the marker if zoomed> } ; set -wu -t <window> @carousel_guard_busy
```

A rollback re-stamps the marker with the restored geometry in the gate's own
SIG format, so the layout changes the apply itself causes fork nothing.
`@carousel_guard_busy` (window option, tmux-og-owned) is set only while the
apply runs and the gate skips while it is set, so the guard's own
resize/rollback never forks a peer mid-apply. It is cleared by the outer list,
so a failing resize or `select-layout`, which skips the rest of its own
branch, cannot leave it set.

## Wiring

A new script `scripts/tmux-carousel-guard.sh <window_id>`, forked by two new
gated hooks:

```
set-hook -g window-layout-changed[30] { if -F '<GATE>' { set -wF @carousel_guard_sig '<SIG>' ; run-shell -b "<guard> #{q:window_id}" } }
set-hook -g window-resized[30]        { …same body… }
```

`[30]` on `window-layout-changed` runs after grid-refit (index 0) and dwindle
(`[20]`); on `window-resized` it sits beside float-refit (0), grid-refit
(`[10]`) and the bridge nudge (`[20]`). The existing bare `set-hook -gu` of both
hooks clears every index on reload. `window-layout-changed` alone covers
split/kill/move, `select-layout`, `resize-pane` and (on tmux-next) window
resizes; `window-resized` is kept for a server whose resize does not refire
layout-changed. Both carry the byte-identical body.

**GATE** (in-process, no fork), all of:

- `#{!:#{@bridge_win}}` — not a mirror window (daemon-owned geometry);
- `#{!:#{@carousel_guard_busy}}` — not mid-apply (see "Verify and roll back");
- `#{!=:#{@crew_grid},1}` — not a dispatcher grid (`tmux-grid-refit` owns it);
- `#{!:#{window_zoomed_flag}}` — zoom is user state; unzoom refires
  `window-layout-changed`, so the guard re-evaluates then;
- `#{e|>=|:#{window_width},61}` — cheap prefilter for the too-narrow rule;
- `#{P:…}` finds a pane with `@claude_img_src`, not floating,
  `pane_width < 20`;
- `SIG != #{@carousel_guard_sig}`.

**SIG** = `#{window_width}x#{window_height}:` + every tiled pane's
`id.left.top.width.height` (the tiled-geometry part of the grid gate's
signature). The gate writes it to the window option `@carousel_guard_sig`
right before forking. That one marker does two jobs:

- burst de-duplication: `resize-pane` alone fires `window-layout-changed`
  twice, and a bridge `refresh-client -C` refires both hooks on every window;
  every event at the same geometry is skipped until something moves;
- termination: if the script declines (pair too narrow) or tmux cannot grow the
  carousel (every neighbour at its minimum), geometry does not change, so the
  next event matches the marker and nothing forks again. A partial grow below
  `MIN_COLS`, or a restore that crushed another pane, is rolled back and the
  marker re-stamped with the restored geometry, so it matches too.

The marker must mean "tried and could not fix", never "fixed": the triggers
are deterministic, so the same action can squeeze a restored carousel back to
the identical geometry (same pane ids, 152/1 at 154), which a lingering marker
would then suppress forever. So a successful restore — a resize that brings
the carousel to `MIN_COLS` or more and leaves every other tiled pane at
`MIN_COLS` or more — clears the marker in the same tmux command list as the
resize (`<pane>` is the carousel or the host, per "Restore target"; the full
list is under "Verify and roll back").

One option write per successful restore, which is rare. A declined restore
leaves the marker as the gate wrote it; a rolled-back one re-stamps it with the
restored geometry, so termination above still holds. A marker left behind by a
run killed before acting suppresses only that exact geometry until anything
moves.

The script re-reads the window in one `list-panes` and re-applies the zoom,
grid and mirror checks (the window can change between gate and backgrounded
run) and exits when one holds. It then restores the first squeezed carousel it
can per "Restore target", inside an `if -F` that repeats those checks
atomically (`resize-pane` unzooms a zoomed window) and skips the resize when
one holds. Either way the script clears the marker on a successful restore, or
when the window is (or became) zoomed, so an unzoom at the same geometry is
restored; a mirror or grid window is never written.

## Coexistence and ping-pong

- **The guard itself**: it only ever grows a carousel that is below 20 to ≥ 20
  (or does nothing), and the gate only trips below 20; its own resize cannot
  re-trip it.
- **Dwindle** (`[20]`): it forks `balance` only when the tiled pane count
  dropped. A guard resize keeps the count, so dwindle only re-caches. When a
  pane close triggers both, they may race (both `-b`): whichever lands last
  fires `window-layout-changed` again; if the carousel is squeezed after
  dwindle's balance, the guard fires once more on the new geometry, and
  dwindle never re-forks (count unchanged). Bounded.
- **Grid refit**: grid windows are skipped by both gate and script.
- **Float refit**: only touches floats, which the guard ignores; `resize-pane`
  fires no `window-resized`.
- **A user deliberately shrinking the carousel below 20** gets it restored —
  the accepted cost of a self-heal; `resize-pane -Z`/closing the carousel are
  the ways to get it out of the way.

## Tests (`tests/carousel-guard.bats`)

Scratch server per test (own `-L`/`TMUX_TMPDIR`/`CLAUDE_STATUS_DIR`, `TMUX`
unset, `-f /dev/null`), production hook lines sourced from `TMUX_OG_CONF`
(the rendered conf, set by the flake check) or, when unset, extracted from
`config/tmux.conf.tmpl` with the script placeholder replaced by the repo
script. The guard path in the sourced lines is swapped for a recorder wrapper
that logs each fork, then execs the real script.

1. A 154-wide main + side carousel squeezed to 1 (`resize-pane` main `-x 152`)
   is restored to 61 (fails without the guard).
2. A wide carousel through resizes: unchanged, zero forks.
3. Zoomed window, `@crew_grid=1` window, floating carousel, `@bridge_win`
   window: zero forks, width unchanged.
4. Too-narrow window (50 wide, carousel at 1): zero forks. Wide window but
   too-narrow pair (`A | H | C` with `H + C` < 61): exactly one fork, nothing
   changed, and further events fork nothing. A carousel with no safe donor
   (`[A 100 | [H 15 / C 15] | X 37]`): one fork, geometry unchanged.
   Donor rules: a middle carousel shrinks its host (`H | C | X`, X untouched);
   a right-edge carousel beside a nested row (`[[K | H] / Y] | C`) takes its
   columns from its left. A restore that would crush a neighbour
   (even-horizontal `38/38/74/1`) is rolled back: one fork, the pre-event
   layout, the marker equal to the live signature, and the layout-change
   counter flat afterwards. A window with `@carousel_guard_busy` set forks
   nothing.
5. Re-squeeze: restore, squeeze again to the identical geometry, restored
   again (red if the marker survives a successful restore).
6. No ping-pong: after one restore, further resizes and an idle settle fork
   nothing more and the layout stays put; with dwindle armed (`@dwindle on`,
   `TMUX_OG_CONF` only), closing a pane next to a squeezed carousel settles.
7. Gate/script threshold literals agree (the gate's `20`/`61` vs the script's
   constants), the script's `SIG_FMT` equals the gate's SIG, and both hook
   bodies are byte-identical.

Wire a `carousel-guard-tests` flake check (pinned `mkTmux`, `TMUX_OG_CONF`) and
extend `grid-conf-assertions`-style ordering checks for the two `[30]` setters.

## Docs

- `docs/agents/scripts.md`: a `tmux-carousel-guard` row (trigger, gate,
  thresholds, `@carousel_guard_sig` ownership).
- `docs/agents/performance.md`: the carousel gate beside the refit gates.
- `docs/agents/bridge-graphics-paste.md` (owns the `prefix + I` carousel): one
  line pointing at the guard and why mirrors skip it.
- This spec and its plan are committed under `docs/superpowers/specs/` and
  `docs/superpowers/plans/` in the same PR.
