# Side panes keep their intended width (#1221) — plan

Spec: `docs/superpowers/specs/2026-11-23-pane-pref-width-design.md` (accepted).
One generic guard (`tmux-pane-pref-width`) replaces `tmux-carousel-guard`; a
side pane declares `@pane_pref_width "<pct>,<min>,<max>,<keep>"`.

## Shared literals (every step uses these byte-for-byte)

```
SIZE = #{window_width}x#{window_height}
COUNT = #{n:#{P:#{?pane_floating_flag,,x}}}
HOLDVAL = SIZE_COUNT  (`_`, not `:`, which ends tmux's `s///`)
KEY = SIZE_COUNT
HELD = #{==:#{@pane_pref_hold},KEY}
GREW = #{&&:#{==:#{s/_.*//:@pane_pref_hold},SIZE},#{e|>|:COUNT,#{s/^.*_//:@pane_pref_hold}}}
SIG  = #{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}
MIN  = #{s/^[^,]*,([^,]*),.*/\1/:@pane_pref_width}
MAX  = #{s/^[^,]*,[^,]*,([^,]*),.*/\1/:@pane_pref_width}
KEEP = #{s/^.*,//:@pane_pref_width}
SIDE = #{||:#{@claude_img_src},#{==:#{@pane_label},crew-dash-side}}
TRIP = #{?pane_floating_flag,,#{?@pane_pref_width,#{?#{&&:#{||:#{e|<|:#{pane_width},MIN},#{&&:#{e|>|:MAX,0},#{e|>|:#{pane_width},MAX}}},#{e|>|:#{window_width},#{e|+|:MIN,KEEP}}},1,},#{?SIDE,1,}}}
GATE = #{&&:#{&&:#{!:#{@bridge_win}},#{!=:#{@crew_grid},1}},#{&&:#{!:#{window_zoomed_flag}},#{&&:#{P:TRIP},#{&&:#{!:HELD},#{!=:SIG,#{@pane_pref_sig}}}}}}
STALE = #{&&:#{@pane_pref_hold},#{!:HELD}}   (ratchet: if GREW set the hold to KEY, else unset it)
HOLD = #{&&:#{&&:#{!:#{pane_floating_flag}},#{!:#{m:*-Z*,#{hook_arguments}}}},#{&&:#{&&:#{!:#{@bridge_win}},#{!=:#{@crew_grid},1}},#{&&:#{P:#{?pane_floating_flag,,#{?#{||:#{@pane_pref_width},SIDE},1,}}},#{!=:KEY,#{@pane_pref_hold}}}}}
Defaults (script only): crew-dash-side label -> 32,50,80,80 ; @claude_img_src -> 40,20,0,40
```

(Upper-case names are textual macros; expand them inline. The hook lines are
one line each, as today.)

Hooks (`config/tmux.conf.tmpl`, replacing the two `[30]` lines and their comment):

```
set-hook -g window-layout-changed[30] { if -F 'STALE' { if -F 'GREW' { set -wF @pane_pref_hold 'HOLDVAL' } { set -wu @pane_pref_hold } } ; if -F 'GATE' { set -wF @pane_pref_sig 'SIG' ; run-shell -b "{{index .Paths.Scripts "tmux-pane-pref-width"}} #{q:window_id}" } }
set-hook -g window-resized[30]        { <same body> }
set-hook -g after-resize-pane[10]     { if -F 'HOLD' { set -wF @pane_pref_hold 'HOLDVAL' } }
```

## Steps

### S1 — guard script (sonnet)  ∥ S2, S3

`git mv scripts/tmux-carousel-guard.sh scripts/tmux-pane-pref-width.sh`, then
rewrite per spec "Guard script":

- One `list-panes -t <win>` read: window width, zoomed, `@bridge_win`,
  `@crew_grid`, pane id, floating, left, top, width, height,
  `@pane_pref_width`, `@claude_img_src`, `@pane_label`, `#{window_layout}`,
  SIG. Zoomed (non-mirror) → `set -wu @pane_pref_sig`, exit; mirror/grid → exit.
- Stamp the default on each tiled unstamped side pane (`set -p -t <pane>
  @pane_pref_width <default>`) and use it for this run.
- Parse with `IFS=, read -r pct min max keep`; all four must match
  `^[0-9]+$`, `min >= 1`, `pct` 1..100; else skip the pane.
- Candidate when `w < min || (max > 0 && w > max)`.
- Partner: tiled pane `p` with `left[p] + width[p] + 1 == left[c]`,
  `top` and `height` equal. Right edge (`left+width == ww`): resized = c,
  span = partnered ? `pw+1+cw` : ww. Else partnered: resized = partner,
  span = `pw+1+cw`. Else skip.
- target = `span*pct/100`, clamp to `[min, max or ∞]`; cap = `span-1-keep`;
  `cap < min` → skip; `target = min(target, cap)`; cols = target, or
  `span-1-target` when the partner is resized.
- Protected (snapshot): non-option tiled panes ≥ 20 (`<%id>` list) must stay
  ≥ 20; other option panes ≥ their own min (`<%id>` list) must stay ≥ MIN
  (per-pane format MIN inside `#{P:}`).
- Apply exactly as today's shape: `tmux if -F -t $c "$still_tiled"
  "resize-pane -t $resized -x $cols ; set -wu -t $target @pane_pref_hold ;
  if -F -t $c '$restored' { $clear_marker } { select-layout -t $target
  '$snapshot' ; set -wF -t $target @pane_pref_sig '$SIG_FMT' }"
  "$clear_if_zoomed"`, `restored` = c in `[min, max]` (literal numbers) and
  nothing protected crushed. One pane per run.
- Header comment rewritten (no history narration), `shellcheck` clean.

### S2 — wiring (sonnet)  ∥ S1, S3

- `config/tmux.conf.tmpl`: the three hooks above + a rewritten comment block
  (gate, hold, stale clear, marker semantics, why `[30]`/`[10]`).
- `config/tmux.conf.nix`: rename both `"tmux-carousel-guard"` entries.
- `generator/paths/paths.go`: rename the `RequiredScripts` entry
  `"tmux-carousel-guard"` → `"tmux-pane-pref-width"` (the template resolves
  the path through `.Paths.Scripts`, validated against this list); run
  `go test ./paths/...` in `generator/`.
- `config/tmux.conf.reference.nix`: the frozen byte-identity oracle the
  extraction check diffs the rendered conf against (`flake.nix` `entryCheck`),
  so the comment block and the three hook lines must match the rendered
  template exactly; verify with that check
  (`nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions`), not grep.
- `flake.nix`: `carousel-guard-conf-assertions` → `pane-pref-width-conf-assertions`
  (greps both `[30]` lines for `bin/tmux-pane-pref-width #{q:window_id}`, the
  `after-resize-pane[10]` line for `@pane_pref_hold`, and that the bare
  `set-hook -gu after-resize-pane` precedes it and nothing re-clears it after;
  and that the two `[30]` bodies are byte-identical in the rendered conf);
  `carousel-guard-tests` →
  `pane-pref-width-tests` (scripts `tmux-pane-pref-width.sh`, test
  `pane-pref-width.bats`; keep the store-built `tmux-dwindle` access the
  existing check has).
- Verify: `nix build .#default`, the extraction check, and the renamed conf
  assertions check.

### S3 — crew-dash side stamp (haiku)  ∥ S1, S2

`scripts/tmux-crew-dash.sh` `open_side` only:
- after creation, also `set-option -p -t "$sid" @pane_pref_width 32,50,80,80`
  (constant `SIDE_PREF` next to `SIDE_MIN`..`SIDE_KEEP`, derived values
  match them);
- relaunch string (stored literally, so inside the script's double-quoted
  bash string write `\"\$TMUX_PANE\"`; an unescaped `$TMUX_PANE` would expand
  at creation to the caller's pane id): `tmux set -p @pane_label
  crew-dash-side; tmux set -p @pane_pref_width 32,50,80,80; tmux set-hook -R
  -t "$TMUX_PANE" window-layout-changed; exec crew dash$narrow`;
- header: replace the carousel-guard sentence with the `@pane_pref_width`
  one.
- `tests/crew-dash-launch.bats`: the two relaunch-string assertions updated
  as exact strings (they pin the literal `"$TMUX_PANE"`);
  add "--side stamps @pane_pref_width 32,50,80,80".
- Verify: `bats tests/crew-dash-launch.bats` (devshell tmux).

### S4 — guard tests (sonnet), after S1+S2

`git mv tests/carousel-guard.bats tests/pane-pref-width.bats`; arm all three
production hook lines (grep `tmux-pane-pref-width` and the
`after-resize-pane[10]` line; recorder swap as today; assert 3 lines).
Rename marker reads to `@pane_pref_sig`. Carry every #1056 case with its
width assertions (carousel defaults to `40,20,0,40`, stamped by the guard);
the "side crew dash under 50 rolled back" case now uses an option pane.
New cases (a 250x50 window, main + side `32,50,80,80` labelled
`crew-dash-side` unless noted):
1. window 250 → 160: side restored to 51; positive fork count 1.
2. sibling close, dwindle off: main/mid/side(80) kill mid → 165 → 80.
3. dwindle balance (store tmux-dwindle and the production `[20]` hook, as the
   existing race case does): ends at 80, settles.
4. side at 64 in 250: no fork, marker unset.
5. side grown to 96 by 160 → 250 resize: restored to 80.
6. `@crew_grid 1` window / zoomed window / floating option pane: no fork.
7. window narrower than min+1+keep (e.g. 120): no fork.
8. hand resize (`resize-pane -t side -x 120` from a client command, as a key
   bind runs it) sticks, `@pane_pref_hold` = `250x50_2`, no fork; then resize-window
   to 200 → restored to clamp(32% of 200)=64.
9. `resize-pane -Z` twice: no hold.
10. A-B-A: hand resize to 120 at 250, resize-window 160 (side in band →
    hold cleared, no fork needed), resize-window 250 → side restored to 80.
11. unstamped `crew-dash-side` pane grown to 125 → stamped `32,50,80,80`
    and restored to 80.
12. guard's own resize leaves no hold (`@pane_pref_hold` unset after a restore).
12a. hand resize side to 120, then `split-window -v` of main: side stays 120,
    no fork, hold ratcheted to `250x50_3` (a split must not drop a hand resize).
12b. same with `split-window -h` of main.
12d. hand resize side to 120, `split-window -h` of main, kill that new pane:
    side restored to 80, hold unset, one fork; same under dwindle (never left
    at 125); a `split-window -v` then close also ends the hold.
12e. side split off main, b split off main, kill main, b resized so the side
    is 125: fire the layout, side restored to 80 (the script reads the side
    pane by id when pane order differs from creation order).
12c. main|mid|side, side hand-resized to 120 (hold count 3), kill mid: hold
    cleared and the side restored to its band.
13. literal check: both `[30]` bodies identical; script `SIG_FMT` equals the
    gate's SIG; defaults in the script match the spec.
Verify: `bats tests/pane-pref-width.bats` with `TMUX_OG_CONF` unset
(template path) and via `nix build .#checks.x86_64-linux.pane-pref-width-tests`.

### S5 — docs (haiku), after S1–S4

`docs/agents/scripts.md` (row renamed and rewritten: option grammar, defaults,
gate, hold, donor/rollback — including that any tiled non-zoom `resize-pane`
in a window with a side pane stamps the hold, even a vertical resize of an
unrelated split; crew-dash row: stamp + relaunch), `floats.md`
(`--side` paragraph: width kept by `tmux-pane-pref-width`),
`performance.md` (section renamed "Side-pane width guard gate": fork
conditions, stale-hold write, hold hook is format-only),
`persist.md` (dash relaunch re-stamps + `set-hook -R`),
`bridge-graphics-paste.md` (carousel bullet → new guard),
`bridge-daemon.md` (remote `crew-dash-side` kept by the remote's own guard via
its label; mirrors skipped locally). Grep for any remaining
`carousel-guard`/`carousel_guard` outside `docs/superpowers/` and fix.

### S6 — gate (lead)

`nix build .#default`, `nix flake check`, `nix build .#lint`.

## Risks

- tmux format parsing of `s///` with `,` inside `#{?…}` branches: S2 verifies
  each macro on a scratch server before wiring (`display -p` with a sample
  pane option) — measured already for MIN/MAX/KEEP.
- Hook ordering (hold before the resize's own `[30]`): pinned by case 8.
- dwindle race: case 3.
