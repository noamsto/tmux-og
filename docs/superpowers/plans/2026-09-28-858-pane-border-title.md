# Plan: full window title on the anchor pane's border (#858, owner revision)

## Owner decision B (dispatcher, after the override finding)

tmux-og owns the border format; the dispatcher only publishes data. The dispatcher
today sets window-level `pane-border-format` (`publish_grid_lead`, dispatch.sh ~3980,
dispatch-resume.sh:1084) and per-pane format/styles (`decorate_pane`), which override
this repo's `setw -g`. This PR does NOT touch the dispatcher; local dispatcher grids
keep today's border until the companion change (reported to the dispatcher as
`follow-ups (untracked)`) stops setting those. The PR body states this and lists the
option contract the format reads. Tests model the override explicitly (case 13).

Option contract read by the format:
- pane: `@pane_label`, `@claude_img_src`, `@crew_role`, `@crew_state`, `@crew_detail`,
  `@crew_source`, `@crew_role_color`; mirror copies `@bridge_crew_role`,
  `@bridge_crew_state`, `@bridge_crew_role_color` (detail/source are not shipped).
- window: `@crew_name`, `@crew_color`, `@window_has_agent`, `@window_label_id`,
  `@window_label_rest_long`, `@bridge_win`, `@bridge_crew_name`, `@bridge_crew_color`.

State widget (`STATE_GLYPH`, same vocabulary/colours as the dispatcher's `state_glyph`):
working ● green, idle ○ overlay_1, blocked ⚠ peach, done/pr_open ✓ green,
failed/exited ✗ red, unknown non-empty ○ overlay_1; each glyph `#[fg=X]G#[default]`
(`#[default]` returns to the border style's base, one attribute per block). The
script additionally reads `@thm_peach`, `@thm_red`. `STATE` = `#{?@bridge_crew_role,#{@bridge_crew_state},#{@crew_state}}` — the same selector as ROLE_NAME, so a bridged pane's role and state always come from the same side.

## Design

`scripts/tmux-apply-theme-colors.sh` builds `pane-border-format`. New branch
order, first match wins (one style attribute per `#[…]`, #648):

1. **Float label** — `@pane_label` → today's branch, byte-identical.
2. **aeye carousel split** — `AEYE` = `#{||:#{@claude_img_src},#{m:*/bin/aeye *,#{pane_start_command}}}`
   → `━━ aeye ━━`, explicit fg (mauve when `pane_active`, else `#[bg]#[fg=overlay_1]`,
   exactly like the float branch). `@claude_img_src` is aeye's own pane option
   (`tmux-update-icons.sh` keys the carousel restore off it); the start-command
   match is the owner's detector; the OR also covers a remux-relaunched viewer
   whose start command is `tmux-carousel-restore`, not `bin/aeye`.
3. **Role pane** — `ROLE_NAME` = `#{?@bridge_crew_role,#{@bridge_crew_role},#{@crew_role}}`;
   when non-empty and `#{!=:ROLE_NAME,lead}`:
   `━━ #[bold]<role>#[nobold]` + (STATE ? ` <glyph> <state>` : '') + ` ━━`
   (owner example `━━ plan-critic ● ━━`; the state word is kept as today's mirror
   border shows it). No fg: role colour comes from the border style (below). Never the title.
4. **Anchor pane** — `ANCHOR` = `#{&&:#{&&:#{pane_at_top},#{pane_at_left}},#{!:#{pane_floating_flag}}}`
   and (TITLE or CODENAME non-empty):
   segments joined by ` · `, each present only when non-empty:
   `#[bold]CODENAME#[nobold]`, `LEAD_STATE`, `#{=/W/…:TITLE}`; wrapped `━━ … ━━`.
   `LEAD_STATE` = `<glyph> <state>` + (`@crew_source`==watchdog ? ` (watchdog)` : '')
   + (`@crew_detail` ? ` · <detail>` : '') — detail/source local only.
   No fg (crew colour / mauve via the style).
   - `TITLE` = `#{@window_label_id}#{@window_label_rest_long}` — reflow writes these for
     every window, mirrors included, from `@bridge_label_*` with the `@window_bridge_name`
     fallback (`tmux-reflow-windows.sh:185-206,499-502`); full, unclipped. Bridge copies
     are sanitized daemon-side (`#[…]`, `|`, control bytes dropped, capped —
     `bridge-shipped-state.md`); option values are not re-expanded, so `#(…)` never runs.
   - `CODENAME` = `#{?@bridge_win,#{@bridge_crew_name},#{?@window_has_agent,#{@crew_name},}}`
     — local `@crew_name` gated on `@window_has_agent` like the status bar badge (#671).
   - `W` = `pane_width − 9 − (CODENAME ? w(CODENAME)+3 : 0) − (STATE ? w(LEAD_STATE_PLAIN)+3 : 0)`
     (LEAD_STATE_PLAIN = the same text without `#[…]`, measured with `#{w:…}` — display width, never `#{n:…}` bytes), floored at 1 as
     `#{?#{e|<|:X,1},1,X}` (0 = no clip, negative keeps the tail). Drawn label area =
     `pane_width − 2` (measured on pinned next-3.9 with a real attached client: 40-wide
     pane → 38 label cells from x=2); 7 cells = `━━ ` + ` ━━` + the `…` tmux appends
     beyond the limit.
   - A lead pane (`@crew_role lead` / `@bridge_crew_role lead`) falls through step 3 to
     here; the word `lead` is never printed. In a grid the lead is top-left.
   - A zoomed pane has `pane_at_top`/`pane_at_left` = 1, so it shows the title; hidden
     panes are not drawn.
   - If the anchor is a float/aeye/role pane, an earlier branch wins → no title (owner: don't move it).
5. **Plain split** — today's tail, unchanged: active multi-pane unzoomed → mauve `━━ ● ━━`,
   else plain `━━━━━`. Today's non-role `@bridge_crew_name` branch is dropped (the codename
   now rides only the anchor).

**Border colour** — ownership of `pane-border-style` / `pane-active-border-style` moves
from `config/tmux.conf.tmpl:620-621` (and its mirror `config/tmux.conf.reference.nix:1276-1277`,
both lines deleted along with their comment) into `scripts/tmux-apply-theme-colors.sh`, beside
the format — so the bats file evaluates the real shipped strings (`-f /dev/null` server; the
check copies only scripts/ and tests/). They keep `#{@thm_bg}`-style render-time reads where
possible; the script sets them with `setw -g`. The dispatcher's per-pane styles become
redundant: fg = AEYE ? mauve|overlay_1 : `@bridge_crew_role_color` → `@crew_role_color`
→ `@bridge_crew_color` → (`@window_has_agent` ? `@crew_color`) → mauve|overlay_1.
Mirror behaviour unchanged.

**aeye in a mirror**: not detectable locally — the remote viewer's
`pane_current_command` is `fish` (checked live), so `@bridge_proc` cannot name it, and
`@claude_img_src` is not shipped. Out of scope (daemon change); filed as a GitHub
follow-up issue.

## File list

- `scripts/tmux-apply-theme-colors.sh` — new format + comment.
- `config/tmux.conf.tmpl`, `config/tmux.conf.reference.nix` — delete only the border-style comment + two style lines (tmpl 617-621 / reference 1273-1277); keep `pane-border-status top`, the `━━━━━` placeholder and `pane-border-lines`.
- `tests/pane-border-format.bats` — cases below; existing cases updated where the design changed.
- `docs/agents/status-bar.md` — "Pane border labels" section.
- `docs/agents/scripts.md` — `tmux-apply-theme-colors` row.
- `docs/superpowers/plans/2026-09-28-858-pane-border-title.md` — this plan.

## Steps

- [ ] **Step 1: tests first** — `tests/pane-border-format.bats`. Helpers: `plain()` strips
  directives (`sed 's/#\[[^]]*\]//g'`); `render <pane>` = `plain "$(tmux -u display-message -p -t <pane> -F "$FMT")"`;
  `title()` sets window `@window_label_id '#858 '`, `@window_label_rest_long 'feat: full title'`;
  `fake_aeye()` writes an executable `$OG_TMUX_DIR/bin/aeye` (`#!/bin/sh` + `exec sleep 60`).
  setup() also sets `@thm_peach '#fab387'`, `@thm_red '#f38ba8'` (comment: ten values).
  `style <pane> <active|inactive>` = `tmux -u display -p -t <pane> '#{E:pane-border-style}'` /
  `'#{E:pane-active-border-style}'` — the options the script now sets.
  Existing case "bridged crew role renders bold role + state" → unchanged setup; still
  green (STATE follows `@bridge_crew_role`), expectation tightened to plain `━━ reviewer ● working ━━`.
  Existing case "bridged crew name (no role)" → also sets `setw @bridge_win 1`, asserts
  `━━ coral ━━` (no title set); plus the negative: without `@bridge_win` → `━━━━━`. Rest unchanged.
  New cases (assertions on `render`):
  1. float `@pane_label lazygit` + title (file's clientless `new-pane -O … sh` form, skip on failure) → float `━━ lazygit ━━`, no title.
  2. aeye by start command (local window: `@crew_name coral`, `@window_has_agent 1`, `@crew_color colour99`, title): `split-window -h "$OG_TMUX_DIR/bin/aeye 7"` → aeye pane `━━ aeye ━━` (no `coral`, no title), `style <aeye> inactive` contains `fg=#7f849c` and not `colour99`, while `style <anchor> inactive` contains `colour99` (the non-vacuous control); anchor `━━ coral · #858 feat: full title ━━`. Variant: plain `split-window -h` + pane `@claude_img_src 1-%1` → `━━ aeye ━━`.
  3. mirror role pane: `@bridge_win 1`; split, right pane `@bridge_crew_role reviewer`, `@bridge_crew_state working` + title → right `━━ reviewer ● working ━━`, no title; with pane `@bridge_crew_role_color colour114`, `style <pane> inactive` contains `fg=colour114`.
  4. mirror crew name only (single pane, `@bridge_win 1`, `@bridge_crew_name coral`) + title → `━━ coral · #858 feat: full title ━━`.
  5. local grid: window `@crew_name coral`, `@window_has_agent 1`, title; top-left A `@crew_role lead` + `@crew_state blocked` + `@crew_source watchdog` + `@crew_detail 'awaiting reply'`; split B `@crew_role plan-critic` + `@crew_state working` + `@crew_role_color colour111`; split C `@crew_role reviewer` (no state) → A `━━ coral · ⚠ blocked (watchdog) · awaiting reply · #858 feat: full title ━━` (no `lead`), B `━━ plan-critic ● working ━━` with `style B inactive` containing `fg=colour111`, C `━━ reviewer ━━`; title in exactly one render.
  5b. mirror lead state: `@bridge_win 1`, `@bridge_crew_name coral`, anchor `@bridge_crew_role lead` `@bridge_crew_state idle` → `━━ coral · ○ idle · #858 feat: full title ━━`.
  6. local `@crew_name coral` without `@window_has_agent` → anchor `━━ #858 feat: full title ━━`.
  7. plain single pane + title → `━━ #858 feat: full title ━━`.
  8. split, no roles, title: focus right → right `━━ ● ━━`, left (anchor, inactive) = title; focus left → left title, right `━━━━━`. Title exactly once in both.
  9. lead + aeye: A `@crew_role lead`, aeye split to the right → title exactly once (A), aeye `━━ aeye ━━`.
  10. aeye as anchor (`split-window -hb` puts it left) → no pane renders the title.
  11. long title: `resize-window -t "$WIN" -x 40`, rest = 100 `a`, no codename → render EQUALS `━━ #858 ` + 26 `a` + `…` + ` ━━`; with `@window_has_agent 1` + `@crew_name coral` (W = 40−9−8 = 23) → `━━ coral · #858 ` + 18 `a` + `…` + ` ━━`. Built from literal pieces (`printf 'a%.0s' $(seq N)`); no `wc -m` (sandbox C locale).
  12. dispatcher override modelled: set a window-level `pane-border-format` (any literal, e.g. `DISPATCH-LEAD`; pane-level `DISPATCH-ROLE`) on the window → render uses it; likewise a pane-level `pane-border-format` on a role pane (decorate_pane's) wins (documents that local grids keep today's border until the companion change); `set -wu` → tmux-og's anchor border returns.
  13. glyph vocabulary: `@crew_state` in {idle, done, pr_open, failed, exited, bogus} on a single local crew pane → ○ ✓ ✓ ✗ ✗ ○.
  14. drawn border (fd-3 marker `# drawn-border ran` so a silent skip is visible in the check log): server sets `pane-border-status top`; outer `tmux -L og-outer -f /dev/null new-session -d -x 40 -y 12 "env -u TMUX tmux -u -L default attach -t S"` + `status off`; poll inner `list-clients` ≤5s, skip if absent; `tmux -u -L og-outer capture-pane -p` line 1 contains `…` and ends with ` ━━`. Teardown adds `tmux -L og-outer kill-server`.
  Run with the pinned unwrapped binary first on PATH (a temp dir symlinking `.tmux-wrapped` from `nix build .#default` as `tmux`) → new cases fail.
- [ ] **Step 2: implement** the format in `scripts/tmux-apply-theme-colors.sh` (bash pieces `AEYE`, `ROLE_NAME`, `ROLE_STATE`, `CODENAME`, `TITLE`, `W`, spliced into one `setw -g`), update its comment. Border-style edits in `tmux.conf.tmpl` + `reference.nix`. Bats → green; `shellcheck` clean.
- [ ] **Step 3: docs** — (check `docs/agents/theme.md` for a border-format description; update if present) status-bar.md section (branch order; anchor = top-left not focus; `lead` suppressed; codename moved off non-anchor mirror panes; title-less active split keeps ●; aeye detection; local role colour) + scripts.md row; commit plan.
- [ ] **Step 4: gate** — `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Acceptance

- Scratch-server render tests: float (1), mirror role (3), mirror crew-name-only (4), local `@crew_role` grid (5), plain single (7), active vs inactive split (8), long title (11, 14), aeye / lead+aeye / grid with exactly-once assertions (2, 5, 9, 10) → `bats tests/pane-border-format.bats`, run by `nix flake check` (`pane-border-format-tests`).
- Existing conf assertions / theme tests pass → `nix flake check`.
- `nix build .#default`, `nix flake check`, `nix build .#lint` pass; docs updated.
