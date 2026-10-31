# Carousel width guard (#1056) — plan

Spec: `docs/superpowers/specs/2026-10-31-carousel-width-guard-design.md`
(accepted). Orchestration consult: not run — the change is one script, two
hook lines, one bats file, flake wiring and docs, all in one well-mapped seam
(the refit hooks); an opus plan suffices.

Shared constants (spec "Thresholds"): `MIN_COLS=20`, `MAIN_MIN_COLS=40`,
`SHARE_PCT=40`; the gate's window floor is `61` = `MIN_COLS + 1 +
MAIN_MIN_COLS`.

Scratch-server rule for every step that runs tmux: explicit `-L`/`-S` or a
private `TMUX_TMPDIR`, own `CLAUDE_STATUS_DIR`, `TMUX` unset, never a bare
`kill-server` or unscoped `set -g` against the live server.

## Step 1 — `scripts/tmux-carousel-guard.sh` and its registration

`implement: sonnet`. Files: `scripts/tmux-carousel-guard.sh` (new),
`config/tmux.conf.nix`, `generator/paths/paths.go`.

Script (bash, tabs, `set -uo pipefail`, header comment in the style of
`scripts/tmux-float-refit.sh`: what, args, why):

1. `target=${1:-}`; empty → `exit 0`.
2. One read: `tmux list-panes -t "$target" -F
   '#{window_width}|#{window_zoomed_flag}|#{?@bridge_win,1,}|#{@crew_grid}|#{pane_id}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{@claude_img_src}'`
   (`@claude_img_src` last so a stray `|` only lands in the last field), stderr
   to `/dev/null` like `tmux-float-refit`'s read (the window may be gone).
   Window-level fields: zoomed `1`, bridge `1` or `@crew_grid` `1` → `exit 0`.
   Skip floating panes. Record each tiled pane's left/top/width/height in
   associative arrays; collect panes with non-empty src and width `< MIN_COLS`.
3. Per collected carousel `c`: host `h="%${src##*-}"`. If `h` is a recorded
   tiled pane, horizontally adjacent (`hl + hw + 1 == cl` or
   `cl + cw + 1 == hl`) and vertically overlapping (`ht < ct + ch && ct < ht +
   hh`) → `S = hw + 1 + cw`; else `S = window_width`. `S < MIN_COLS + 1 +
   MAIN_MIN_COLS` → skip. `tw = S * SHARE_PCT / 100`, floored at `MIN_COLS`.
4. `tmux resize-pane -t "$c" -x "$tw" \; if -F -t "$c" "#{e|>=|:#{pane_width},$MIN_COLS}" "set -wu -t $target @carousel_guard_sig"`
   — one command list, so the marker is cleared only on a successful restore.

Registration: add `tmux-carousel-guard` wherever `tmux-grid-refit` is listed in
`config/tmux.conf.nix` (the script name lists and the `ogInternal` partition —
no human runs it) and in `generator/paths/paths.go`'s required-scripts list,
keeping each list's ordering convention. `shellcheck` and `shfmt -d` clean.

Gate commands: `shellcheck scripts/tmux-carousel-guard.sh`,
`shfmt -d scripts/tmux-carousel-guard.sh`, `(cd generator && go test ./...)`.

## Step 2 — the two `[30]` hook setters

`implement: sonnet`. Files: `config/tmux.conf.tmpl`,
`config/tmux.conf.reference.nix` (the frozen oracle; its header requires
editing both together, byte-identical apart from the script path syntax:
`{{index .Paths.Scripts "tmux-carousel-guard"}}` in the template,
`${script.tmux-carousel-guard}/bin/tmux-carousel-guard` in the reference).

Place them right after the dwindle `window-layout-changed[20]` setter, with a
comment block in the surrounding style explaining: squeeze is sticky (#1056),
marker semantics (written before fork, skipped while equal; cleared by the
script only on success), skips (mirror, grid, zoom, float), `[30]` ordering
after dwindle `[20]` / beside refit indices, and that the bare `set-hook -gu`
above clears it on reload. Body, identical on both events:

```
set-hook -g window-layout-changed[30] { if -F '#{&&:#{&&:#{&&:#{!:#{@bridge_win}},#{!=:#{@crew_grid},1}},#{&&:#{!:#{window_zoomed_flag}},#{e|>=|:#{window_width},61}}},#{&&:#{P:#{?#{&&:#{&&:#{@claude_img_src},#{!:#{pane_floating_flag}}},#{e|<|:#{pane_width},20}},1,}},#{!=:#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }},#{@carousel_guard_sig}}}}' { set -wF @carousel_guard_sig '#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}' ; run-shell -b "<guard> #{q:window_id}" } }
set-hook -g window-resized[30]        { <same body> }
```

Before committing, verify the gate on a scratch server (`tmux -L probe -f
/dev/null`, private `TMUX_TMPDIR`): `display -p` of the gate is `1` for a
154-wide window with a 1-column `@claude_img_src` pane and `0` once
`@carousel_guard_sig` holds the SIG, `0` for a 40-column carousel.

Gate commands: `nix build .#default` (renders the conf; the extraction check
lives in `nix flake check`).

## Step 3 — `tests/carousel-guard.bats`

`implement: opus` (test harness design, async polling). Files:
`tests/carousel-guard.bats` (new).

Harness, modelled on `tests/grid-refit.bats` (`POLL_TRIES`, setup/teardown,
private `TMUX_TMPDIR` under `/tmp/og-cg-$$-N`, `CLAUDE_STATUS_DIR`, `TMUX`
unset, `tmux -f /dev/null new-session -d -x 154 -y 40`, teardown `tmux
kill-server` on that private socket only):

- `arm_guard_hooks`: lines matching `set-hook .*tmux-carousel-guard` from
  `$TMUX_OG_CONF` when set (store path `/nix/store/[^ "]*/bin/tmux-carousel-guard`
  replaced), else from `config/tmux.conf.tmpl` (the `{{index .Paths.Scripts
  "tmux-carousel-guard"}}` placeholder replaced); replacement is a recorder
  that appends its argv to `$BATS_TEST_TMPDIR/forks.log` and execs `bash
  $REPO_ROOT/scripts/tmux-carousel-guard.sh "$@"` (absolute paths baked in: a
  `run-shell -b` job inherits the server's env, not the test's).
- `make_carousel <pane> <host>`: `set -p @claude_img_src "x-${host#%}"`.

Build every non-trivial geometry with the hooks unarmed, then arm and fire one
event: an intermediate layout can trip the gate mid-build. A trigger that must
fire at unchanged geometry is `select-layout "$(tmux display -p -t $WIN
'#{window_layout}')"` (a same-size `resize-window` may fire nothing).

Cases (each asserts widths via `list-panes`, forks via `forks.log` line count;
"zero forks" = trigger, `sleep 0.5`, log empty):

1. Restore: main + carousel, `resize-pane -t main -x 152` → poll until
   carousel width is 61 (fails with hooks unarmed — the red proof); marker
   `@carousel_guard_sig` empty afterwards.
2. Re-squeeze: after case 1's restore, squeeze again to the identical 152/1 →
   restored to 61 again.
3. Wide carousel (77/76) through `resize-window` 120 → 154: widths
   proportional/unchanged, zero forks.
4. Skips, each zero forks and carousel still 1 column: zoomed window (squeeze
   with hooks unarmed, zoom main, arm, `resize-window`); `@crew_grid 1`;
   `@bridge_win 1`; floating carousel (`new-pane -x 10 … 2>/dev/null || skip`,
   then confirm `pane_floating_flag` is 1, as `grid-refit.bats` probes floats:
   stock tmux advertises `new-pane` and rejects it at parse time).
5. Too narrow: 50-wide window with a 1-column carousel → zero forks.
   Too-narrow pair in a wide window (`A | H | C`, `H + 1 + C < 61`, window
   154): exactly one fork, geometry unchanged, a further `resize-pane` of `A`
   that leaves H/C as they were plus an extra hook trigger at the same
   geometry adds no fork.
6. Bottom carousel: a carousel stacked under its host in a 15-column column
   (window 154) grows the column; a full-width bottom carousel → zero forks.
7. No ping-pong: after one restore, count forks and a counting
   `window-layout-changed[99]` hook over 1s of idle plus two `resize-window`
   steps: forks stay 1, layout changes come only from the test's own resizes.
   With `TMUX_OG_CONF` set, also arm the dwindle `[20]` line, `@dwindle on`,
   `A | H | C` with C squeezed, kill `A`: poll until C ≥ 20, then the
   counting hook stays flat for 1s (skip without `TMUX_OG_CONF`).
8. Static: the `window-layout-changed[30]` and `window-resized[30]` bodies are
   byte-identical; the gate literals `#{e|<|:#{pane_width},20}` and
   `#{e|>=|:#{window_width},61}` match the script's `MIN_COLS` and
   `MIN_COLS + 1 + MAIN_MIN_COLS`.

Gate commands: `bats tests/carousel-guard.bats`; then temporarily drop the hook
arming in case 1 and confirm it goes red (do not commit that).

## Step 4 — flake checks

`implement: sonnet`. Files: `flake.nix`.

- `carousel-guard-tests`: copy of `grid-refit-tests` (`pkgs.bats`,
  `pkgs.coreutils`, `mkTmux pkgs`, `TMUX_OG_CONF = tmuxConfig.tmuxConf`, cp
  `scripts`, `tests`, `config`; `bats tests/carousel-guard.bats`).
- `carousel-guard-conf-assertions` (or extend `grid-conf-assertions`): both
  `[30]` setters present (grep the `\[30\]` form, like the `[10]` one) with a store-path `bin/tmux-carousel-guard
  #{q:window_id}`, and each bare `set-hook -gu` clear precedes them.

Register both wherever the sibling checks are listed. Gate commands:
`nix build .#checks.x86_64-linux.carousel-guard-tests`,
`nix build .#checks.x86_64-linux.carousel-guard-conf-assertions` (or the
extended one).

## Step 5 — docs

`implement: sonnet`. Files: `docs/agents/scripts.md` (new
`tmux-carousel-guard` row beside `tmux-grid-refit`: trigger hooks, gate,
thresholds and why, restore target, marker semantics, `@carousel_guard_sig`
is tmux-og-owned, skips), `docs/agents/performance.md` (a short "Carousel
guard gate (#1056)" section near the refit gates / dwindle cache gate: what it
forks on, why the marker is cleared only on success, the test that pins it),
`docs/agents/bridge-graphics-paste.md` (one line in the carousel section: the
guard exists, mirrors skip it because the remote runs it, needs the remote's
conf reloaded).

Gate commands: `nix build .#lint` (typos, markdown hooks).

## Verification (lead)

`nix build .#lint`, `nix flake check`, `nix build .#default` all green; the
red proof from step 3 recorded for the PR body.

Ordering: 1 → 2 → 3 → 4 ∥ 5.
