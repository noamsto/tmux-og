# Status Bar and Window Grid

How the multi-line status bar is laid out and how `tmux-reflow-windows` sizes the grid.

## Two Icon Variables

- `@window_icon_display` — unpadded, used in `automatic-rename-format` (window tab names) and top-right status
- `@window_icon_padded` — fixed-width padded to `MAX_ICONS * 3 + 2` cells, used in status-format ENTRY for `|` separator alignment. Set by both `tmux-update-icons` (every 1s) and `tmux-reflow-windows` (on layout events)

Icon display width is computed per-icon from Unicode codepoint: nerd font PUA (U+E000-F8FF, U+F0000+) = 1 cell, emoji/other = 2 cells, plus 1 space each. `wc -L` is unreliable for nerd font glyphs (reports 0 for PUA range) and emoji with variant selectors.

## Status Bar Layout

- **Line 0** (status-format[0]): Global — session name, git branch, directory, claude status (left; session agent icon is `claudeSegment` over hook+screen files — `aggregateSession` unions `panes/` and `screen/`); active pane icon + command (right)
- **Lines 1-4** (status-format[1-4]): Window list, dynamically reflowed. Single-line mode unsets session overrides to fall back to global format. Multi-line mode sets per-session overrides with `├─`/`╰─` tree prefixes.

tmux treats session-level `status-format` as all-or-nothing: setting any index at session level overrides ALL indices. That's why reflow must copy `FMT0` from global when setting session-level formats.

tmux renders at most 5 status lines, so the window grid is capped at 4 rows — and reflow only unlocks the 4th above `TALL_CLIENT_ROWS` (40) of `client_height`, since a 5-line status bar is only affordable on a tall client. Row 3 is bounded by `@window_split3`, which stays 999 at the baseline 3-row cap so row 4 renders empty.

Per-row trailing `│` separators derive from the loop-native `next_window_index` (tmux 3.8's `W:` loop next/previous vars, #104): a separator renders only when `next_window_index` is non-empty (a next window exists in the loop) and, for rows bounded by `@window_split*`, that next window's index still falls within the row. Row-level `├─`/`╰─` prefix selection stays driven by `@window_split*` vs `session_windows` — that's row-structural data from `reflow_fit_columns`'s column-width math, not a per-iteration loop fact, so it's still stamped rather than derived in-loop.

## Grid Column Widths

Columns are sized independently, one width per column rather than one `colw` for the whole grid: alignment only requires a width to match *down* a column, never across, so charging every column the widest window's width overflowed the row (#271). `reflow_fit_columns` (in `lib-reflow.sh`) takes two per-window widths — a **floor** (issue id + agent badge + zoom marker, none of which the renderer can shrink) and a **want** (floor + branch/title) — gives each column its floor, then hands the remaining slack out in proportion to unmet demand. The PR badge forms a column of its own beside the label, sized the same way and for the same reason (#688): `pr_colw` was the widest badge in the *session* and rode `overhead`, so one `#1234` cost the row `per × 7` cells and padded every PR-less window to it.

`MAX_REST_WIDTH` caps what a want may *claim* while the row is tight, so one very long title cannot stretch the grid — but the cap does not survive a row with cells to spare. Once every capped want fits, `reflow_fit_columns` hands the leftover out toward the real labels in proportion to what the cap took, and a column already showing its whole label does not grow, since that would only add trailing padding. Before #688 it returned the capped wants verbatim: the 5-window session behind #686 rendered 206 of 245 cells with two titles ellipsised.

The renderer must never emit a label wider than its column. When even the floors overflow, `tmux-reflow-windows` drops the agent badge first (decoration) and clips the issue id only as a last resort — so `@window_label_id` stays the full identity for the pickers and the single-line global format, while `@window_label_id_disp` is the grid's column-clipped copy.

A row is given up only once clipping can no longer save it (#686). `total_long` charges each window its own PR segment, so a badge arriving (`" <glyph> #<n>"`, 6-7 cells, +2 with the draft marker prepended) can push a full row past `available` — and the grid it falls into costs a whole status line *and* caps every label at `MAX_REST_WIDTH`, so it shows *less* text than the row it replaced. Rung 1.5 therefore shaves the overshoot off the widest labels instead (`reflow_clip_rests`, water-filled: every rest above a common cap is cut to it, so a short label never pays) and keeps one row, falling through to the grid only when a clipped label would drop below `SINGLE_CLIP_FLOOR` (16 cells of branch/title). The clip lands on `@window_label_disp` alone, which is why the global `status-format[1]` renders that rather than the `@labels_mode` ternary over `@window_label_rest_long`/`_short` — those stay the full identity `picker/main.go`, `tmux-statusline` and the bridge's `windowlabels.go` read.

## Bridge resize nudge

A remote-bridge mirror session carries `@bridge_nudge`: the path its daemon's
watcher stats each tick instead of forking a size query (`bridge-daemon.md`,
#433). A reflow pass touches it on the way through — after the empty-width
bail so a no-client pass still does not stamp anything (#235), but before the
cache check, so a cache hit nudges too. That is how `client-resized` and
`client-session-changed` (the two events already routed here) also nudge;
`window-resized` and `client-detached` carry their own gated hooks in the conf.
The bridge publishes the path as a session option rather than a
session-scoped hook because a session hook array replaces the session's view
of the global one (#647) — the shadowing that left a mirror's window bar stale
until an unrelated event (#820).


## Pane border labels

The status bar clips a window's label to its column; the full title lives on a pane border instead (#858). `tmux-apply-theme-colors` owns `pane-border-format` **and** both border styles (`pane-border-style` / `pane-active-border-style`), rebuilt on every theme switch. The format picks the first matching branch per pane:

1. **Float label** — `@pane_label` (yazi, prdash, …): `━━ <label> ━━`, mauve when active.
2. **aeye carousel split** — `@claude_img_src` (aeye's own pane option; survives a remux relaunch, whose start command is `tmux-carousel-restore`) or a start command matching `*/bin/aeye *` (tmux quotes it, and the glob needs the trailing argument): `━━ aeye ━━`, and the border styles drop the crew colour for it. A **mirrored** aeye is not detected — the remote viewer's `pane_current_command` is its shell, so `@bridge_proc` cannot name it, and `@claude_img_src` is not shipped.
3. **Role pane** — `@bridge_crew_role`, else `@crew_role`, other than `lead`: `━━ <role> <glyph> <state> ━━`, never the title. State follows the same selector as the role (`@bridge_crew_state` beside a bridged role, else `@crew_state`); the glyph vocabulary matches the dispatcher's `state_glyph` (working ●, idle ○, blocked ⚠, done/pr_open ✓, failed/exited ✗).
4. **Anchor pane** — the top-left, non-floating pane (`pane_at_top && pane_at_left`; the lead in a dispatcher grid, and the zoomed pane while zoomed). It does not follow focus: `━━ <codename> · <glyph> <state>[ (watchdog)][ · <detail>] · <title> ━━`, each segment only when non-empty and the word `lead` never printed. Title = `@window_label_id` + `@window_label_rest_long` (reflow writes both for mirrors too, from the daemon-sanitized `@bridge_label_*`). Codename = `@bridge_crew_name` in a mirror, else `@crew_name` only while `@window_has_agent` (the status bar's #671 gate). `@crew_detail`/`@crew_source` are local only — the bridge does not ship them.
5. **Plain split** — the active pane of an unzoomed multi-pane window keeps the green `●`; everything else is a plain `━━━━━`. A mirror window's codename no longer repeats on every non-role pane: it rides the anchor only.

If the anchor is itself an aeye or role pane, the window shows no title — it never moves to another pane. A float is never the anchor: the tiled top-left pane under it keeps the title.

**Clipping.** tmux draws a border label in `pane_width − 2` cells starting at x=2 (measured on the pinned tmux with an attached client). The title is clipped with `#{=/W/…:…}`, where `W = pane_width − 9 − (each prefix segment's #{w:} + 3)`: 7 cells go to `━━ `, ` ━━` and the `…` that tmux appends *beyond* W. W is floored at 1, because a 0 limit means "no clip" and a negative one keeps the tail. Widths use `#{w:}` (display cells), never `#{n:}` (bytes).

**Border colour.** fg = aeye → mauve/overlay_1; else `@bridge_crew_role_color` → `@crew_role_color` → `@bridge_crew_color` → `@crew_color` (while `@window_has_agent`) → mauve/overlay_1. Moving the styles out of `tmux.conf` also means `tmux-client-theme`'s recovery replay (catppuccin, then this script) no longer leaves catppuccin's border style in place.

**Dispatcher overrides.** A window- or pane-level `pane-border-format`/`-style` beats these globals. The dispatcher still sets them on local grid windows (`publish_grid_lead`, `decorate_pane`), so local dispatcher grids keep its border until it stops doing so and only stamps the options above. `tests/pane-border-format.bats` pins that precedence.
