# Side panes keep their intended width (#1221) — design

## Problem

Side panes (the `crew-dash-side` split, the aeye image carousel) are meant to
hold a set width beside the main pane. tmux has no notion of that: it spreads
every window-width change evenly over the cells of a row, hands a closed
pane's columns to one neighbour, and `tmux-dwindle balance` evens the cell a
pane left. Nothing ever re-applies the side pane's own sizing rule. Only the
carousel has a guard (#1056, `tmux-carousel-guard`), and it only fixes a
carousel squeezed under 20 columns.

Owner's live server (250-column windows): `crew-dash-side` is designed as 32%
of the window clamped to 50..80, but measured at 64, 80, 98, 98, 125.

### Repros (scratch servers, `result/bin/tmux` of this branch's base)

| path | setup | result |
|---|---|---|
| shrink: restore/reattach from a narrower client | full conf; remux-style restore (`new-session -d`, `split-window`, `select-layout` of a 250-wide snapshot with the side at 80), then a 160x50 pty client attaches | side 80 → **35** (below the 50 floor) |
| grow: sibling close + dwindle | full conf (`@dwindle on`); main / mid / side(80), kill mid | side 80 → **125** (half the window; the owner's 125) |
| grow: sibling close, dwindle off | raw tmux, same layout | side 80 → **165** (closed pane's columns go to the next sibling) |
| grow: window grows | full conf; side created at 51 (32% of 160), window resized to 250 | side 51 → **96** (the owner's 98) |
| no-op control | raw tmux, 250 → 100 → 250 | 80 → 5 → 80 (even spread is symmetric while nothing hits the 1-column floor) |

The mechanism is generic: any change of window width or tiled pane count can
move a side pane off its rule, and nothing moves it back.

## Design

One generic guard replaces the carousel guard: a pane declares its preferred
width as a pane option, and the existing `window-layout-changed[30]` /
`window-resized[30]` hooks restore it when it is outside its band.

### The option

`@pane_pref_width` (pane option) = `<pct>,<min>,<max>,<keep>` — four
non-negative integers, all required:

- `pct`: preferred share, in percent, of the *span* the pane shares with its
  partner (see Donor rules): partner + 1 + pane when partnered, else the
  window width. For a side pane split off a full-height main pane the span is
  the window, so this matches `tmux-crew-dash --side`'s 32%-of-window
  creation rule; for the carousel it is #1056's "40% of the pair".
- `min`: floor in columns. The pane is "off" below it.
- `max`: ceiling in columns; `0` means no ceiling. The pane is "off" above it.
- `keep`: columns the donor (the pane that gives or takes the columns, see
  Donor rules) must keep.

Restore target: `clamp(span * pct / 100, min, max)`, then capped at
`span - 1 - keep`; a cap below `min` means the row is too narrow, and the pane
is left alone.

Commas, not colons, because the gate parses the fields with tmux's `s///`
format modifier, and a `:` inside its regex ends the modifier list (measured:
`#{s/^([^:]*):.*/\1/:@x}` expands to empty). `0` and not an empty field for
"no ceiling", because an empty capture group expands to a literal `1`
(measured on next-3.9: `32,50,,80` reads max `1`).

Writers:

| pane | value | where |
|---|---|---|
| `crew-dash-side` | `32,50,80,80` | `tmux-crew-dash --side` at creation and in its `@remux_relaunch` string (tmux-remux restores no pane options) |
| any tiled pane labelled `crew-dash-side` without the option | `32,50,80,80` | stamped by the guard itself the first time its gate sees one: dashes opened before this change, a restore whose relaunch predates it, and the REMOTE side pane in a mirror (the bridge daemon's `crew-dash-side` create command sets only `@pane_label`) |
| carousel (`@claude_img_src`, tiled) without the option | `40,20,0,40` | stamped by the guard itself the same way (aeye's launcher lives outside this repo; a value it stamps later wins) |

The defaults live in the guard script only; the gate just trips on "a tiled
pane carrying the label or `@claude_img_src` but no option". `40,20,0,40`
keeps the carousel's #1056 behaviour exactly: only a carousel under 20 columns
is restored, to 40% of the pair, and the row must be at least 61 wide
(20 + 1 + 40).

### Trip rule: band, not exact

A pane is restored only when it is outside `[min, max]`; a width inside the
band is accepted. So the dash at 64 in a 250 window stays; 35, 96, 98, 125 and
165 are restored to 80 (or to `clamp(32%)` of a narrower window). This keeps
the guard quiet on every ordinary layout change and leaves room for a user
who likes the dash a bit narrower.

### Hand-resize rule

A deliberate `resize-pane` (keyboard `prefix + M-arrow`, a mouse border drag)
holds the window's layout until the window changes size or any tiled pane
closes; a split keeps it. A border drag runs `resize-pane -M` once, at drag start (the motions
that follow go through tmux's `mouse_drag_update`), so the hook fires once per
drag, before the move; the stamp does not depend on widths, so the hold still
covers the drag's result. A plain border click with no drag also stamps a hold
(harmless: it only defers a restore to the next window resize or pane close).
Cost: closing a pane added after the hand resize also ends the hold.

- A new `after-resize-pane[10]` hook stamps window option `@pane_pref_hold` =
  `<window_width>x<window_height>_<tiled count>` (`_`, because `:` ends
  `s///`) — only in a window that has
  a tiled side pane (one with `@pane_pref_width`, the `crew-dash-side` label
  or `@claude_img_src`: the gate's trip set, so a not-yet-stamped dash holds
  too), is not a mirror, not a grid, when the
  resized pane is tiled, the arguments are not a zoom toggle
  (`#{hook_arguments}` without `-Z`; zoom fires this hook too, and a hold
  stamped on unzoom would suppress the restore of a squeeze that happened
  while zoomed), and the window is not already held. A second hand resize while held keeps
  the stamp.
- The guard's gate skips while the stamp equals the current key
  (`<window_width>x<window_height>_<tiled count>`).
  This depends on the hold landing before the resize's own
  `window-layout-changed[30]` gate runs: the `after-*` hook is inserted into
  the issuing client's queue right after the command, while the layout
  notification goes to the global queue, which runs later. A bats case drives
  a hand resize the way a key binding does and asserts it sticks.
- A stale hold is dropped in-process: each `[30]` hook body first runs
  `if -F '<hold set and stamp != key>' { if -F '<size unchanged and count grew>' { set -wF @pane_pref_hold '<key>' } { set -wu @pane_pref_hold } }`:
  a split ratchets the stamp to the new key, so a size
  change or a pane close clears it even when nothing trips at that moment
  (otherwise a 250 → 160 → 250 round trip would bring the old size back and
  re-arm a hold the user's resize no longer explains). The write happens only when a stale hold exists.
- The guard's own `resize-pane` also fires the hook; its apply list unsets
  `@pane_pref_hold` right after the `resize-pane` (measured: an `after-*` hook
  runs before the next command of the same list).

Why size + a count floor and not "until the next window resize" alone: a
sibling closing is one of the disturbing events this issue is about, and
dwindle's evening arrives as a `select-layout` at an unchanged size, which a
size-only key could not tell from a hand resize that preceded it. Why a floor
and not equality: a split must not drop a hand resize (splitting the main pane
after widening the side snapped it back). Why not side widths: a border drag
runs `resize-pane -M` once at drag start and its motions fire no hook, so the
stamp would carry the pre-drag widths.

### Gate (in-process, no fork when nothing is off)

Both `[30]` hooks keep one identical body. They fork the guard only when all
hold:

1. not a mirror (`@bridge_win`), not a grid (`@crew_grid=1`), not zoomed;
2. some tiled pane trips: either an unstamped side pane
   (`@claude_img_src` set or `@pane_label` = `crew-dash-side`, and
   `@pane_pref_width` unset), or a pane with
   `@pane_pref_width` whose width is under `min` or over a non-zero `max`,
   in a window wider than `min + keep` (the old literal `61` generalised);
3. the hold is not held: `@pane_pref_hold` (`WxH_<tiled count>`) differs from
   the current key `WxH_<tiled count>`;
4. the tiled geometry signature differs from `@pane_pref_sig` (the old
   `@carousel_guard_sig`, same format and same "written before the fork,
   cleared on a successful restore" semantics).

The per-pane fields are parsed inside `#{?<has option>,…,}` so a pane without
the option costs one option lookup, no regex.

### Guard script

`scripts/tmux-carousel-guard.sh` becomes `scripts/tmux-pane-pref-width.sh`
(renamed, one script, the carousel is just one writer). Per run, for the
target window:

1. One `list-panes` read (as today). Zoomed → clear the marker and exit;
   mirror or grid → exit.
2. Stamp the default on every tiled unstamped side pane (`32,50,80,80` for
   the `crew-dash-side` label, else `40,20,0,40` for a carousel).
3. Candidates: tiled panes with a well-formed option and a width outside the
   band. A malformed value is ignored (the marker then stops forks at that
   geometry).
4. Donor rules, unchanged from #1056 but keyed on geometry for every pane:
   the pane's *partner* is the tiled pane directly left of it with the same
   top and height (for a carousel #1056 required that pane to be its host;
   geometry alone is the generic rule, and the stale-view guard plus the
   rollback check back it). A pane at the window's right edge is resized
   itself; a partnered pane elsewhere resizes the partner to
   `span - 1 - target`; anything else is declined. `keep` is measured on the
   partner, the pane directly left, not on "the main pane": in a
   main | mid | dash row a squeezed dash is declined when mid cannot give it
   `min` columns and still keep 80, even if main could. Declining is the safe
   side (it never crushes); such a dash stays squeezed.
5. One pane per run, inside the same `if -F` stale-view guard (zoom, mirror,
   grid, signature unchanged) as today: `resize-pane`, unset
   `@pane_pref_hold`, then keep the result only if the pane is inside its
   band and no protected pane was crushed, else `select-layout <snapshot>`
   and re-stamp the marker. Protected: every other tiled pane that was at
   least 20 columns and has no option (must stay ≥ 20), and every other
   option pane that was at least its own `min` (must stay ≥ its `min`) —
   this generalises today's crew-dash-side ≥ 50 special case.

### Events covered

| event | how it reaches the guard |
|---|---|
| client attach / client resize / reattach from a narrower or wider terminal (window-size latest) | `window-resized[30]` |
| window resize of any kind | `window-resized[30]` |
| another pane in the window closing | `window-layout-changed[30]` on the close |
| dwindle balance (async `select-layout` after a close) | `window-layout-changed[30]` on dwindle's apply; whichever of dwindle and the guard applies last, the other re-fires (dwindle forks only on a pane-count drop, so it cannot loop) |
| remux restore | restored layout fires `window-layout-changed`; the dash's relaunch re-stamps the label and option and runs `tmux set-hook -R -t "$TMUX_PANE" window-layout-changed` (measured: `-R -t <pane>` runs the hooks against that pane's window, a bare `-R` against the client's current window), so a snapshot taken while the dash was squeezed is still restored even when the attaching client's size matches. `-R` runs every index of the hook: grid-refit's gate (skips a non-grid window), dwindle's `[20]` (re-seeds its cache at most; no pane-count drop, so no balance), the guard `[30]`, autofold `[40]` (gated on floats). The string stays POSIX and fish-safe like its neighbours (`;`-separated, `"$TMUX_PANE"`). |
| mirror windows | out of scope locally: the mirror's geometry is the remote's and the daemon owns the window, so the local guard skips `@bridge_win` (as #1056 did). The fix runs remote-side, in the remote host's own tmux-og: measured on a scratch server, a control-mode client attaching with `refresh-client -C 160x50` (what the bridge does) fires `window-resized` there and squeezes an 80-column side pane to 35, which is exactly the remote guard's trigger; the remote side pane is found by its `crew-dash-side` label and stamped by that guard. A remote on an older tmux-og keeps today's behaviour. Bridge reseed rebuilds only local mirror windows and changes no remote layout, so it needs nothing. A remote remux restore of a daemon-created side pane is out of scope: that pane has no `@remux_relaunch`, so it comes back as a plain shell either way. |

### Not covered / out of scope

- Float geometry (`tmux-float-refit`), the parent-close mechanism (separate
  worker on `tmux-crew-dash.sh`).
- A border drag inside a mirror window reaches the remote as a `tile-layout`
  (`select-layout`), not a `resize-pane`, so it stamps no hold on the remote;
  a drag that takes the remote dash outside its band is restored there.
  Keyboard resizes in a mirror use the `resize` verb (a remote
  `resize-pane`) and do hold.
- `#{hook_arguments}` is empty on a resident server older than the one this
  was measured on (#407); there, a zoom toggle also stamps a hold, which only
  delays a restore until the next window resize or pane close.
- aeye's launcher stamping its own `@pane_pref_width` (other repo; follow-up).
- A reloaded resident server keeps a stale `@carousel_guard_sig` on windows
  the old guard touched. Nothing reads it any more; it is left to die with the
  window rather than adding a sweep.

### Rename reach

`tmux-carousel-guard` → `tmux-pane-pref-width` everywhere it is named:
`config/tmux.conf.nix` (both script lists), `generator/paths/paths.go` (`RequiredScripts`), `config/tmux.conf.tmpl` (both
`[30]` hooks and their comment), `config/tmux.conf.reference.nix` (comment
block and both hooks), `flake.nix` (`carousel-guard-conf-assertions`,
`carousel-guard-tests`), `tests/carousel-guard.bats`, `docs/agents/scripts.md`,
`performance.md`, `floats.md`, `bridge-graphics-paste.md`, and the
`tmux-crew-dash.sh` header.

## Acceptance → tests

`tests/pane-pref-width.bats` (renamed from `carousel-guard.bats`, the
production hook lines sourced as today), on a scratch server:

- a side pane shrunk by a narrower window, restored to `clamp(32%)`;
- a side pane grown past `max` by a sibling closing (dwindle off) and by
  dwindle's balance (store-built `tmux-dwindle`), restored to 80;
- a dash inside its band (e.g. 64) forks nothing;
- `@crew_grid`, zoomed, mirror and floating panes untouched / no fork;
- a window narrower than `min + 1 + keep` forks nothing;
- a hand `resize-pane` outside the band sticks; the next window resize
  restores it; a `resize-pane -Z` stamps no hold;
- a hold survives a split of the main pane (either axis), and a 250 → 160 → 250
  round trip (and closing a pane that existed at the hand resize) clears it so
  the dash is restored at 250;
- an unstamped `crew-dash-side` pane gets `32,50,80,80` and is restored;
- the carousel: unstamped carousel gets `40,20,0,40`, a squeezed one is
  restored to 40% of the pair (the #1056 cases carried over with their width
  assertions: donor rules, rollback, stale view, ping-pong, dwindle race);
- gate literals / signature / hold key match the script.

`tests/crew-dash-launch.bats`: `--side` stamps `@pane_pref_width 32,50,80,80`
and the relaunch string carries it and the `set-hook -R`. No daemon change:
the remote guard stamps the remote side pane by its label.
