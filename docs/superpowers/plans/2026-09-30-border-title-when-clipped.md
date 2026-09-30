# Border title only when the status label is clipped (#885)

## Finding

The screenshot's title is cut at the **source**, not by reflow. The rest
`re-rank claude 5.5 ladder: cap sonnet at high, dem` is exactly 50 characters:
`sanitize_title` (`scripts/lib-enrich.sh`) hard-truncates every issue/PR title
to 50 at stamp time, so `@issue_title` → `@window_label_rest_long` →
(remote) → `@bridge_label_rest_long` → the mirror's `@window_label_rest_long`
never holds more. The status label was not trimmed at all — the border was
pure duplication. A second cap sits on the mirror path: the daemon's
`labelTextMaxRunes = 120` (`picker/remotebridge/daemon/windowlabels.go`)
truncates `@bridge_issue_title`, `@bridge_pr_title`, `@bridge_label_id`,
`@bridge_label_rest_long`.

## Design

- **`@window_label_clipped`** (window option, reflow-owned): `1` when the
  label the status bar actually draws (`@window_label_id_disp` +
  `@window_label_disp`, unpadded) differs from the full identity
  (`@window_label_id` + `@window_label_rest_long`) — covers rung 1.5 clipping,
  short mode dropping/shortening the rest, and grid column truncation of id or
  rest. Unset otherwise. Written for mirror windows too (reflow computes their
  labels). Written only when the value changes: the old value is read in the
  existing `list-windows` FMT, and the set/unset rides the existing per-window
  argv `tmux` exec — no new fork.
- **Border**: `TITLE_SHOWN='#{?@window_label_clipped,<TITLE_RAW>,}'` replaces
  `TITLE_RAW` in `TITLE_SEG`. `ANCHOR_COND` becomes
  `#{&&:ANCHOR,#{||:#{!=:TITLE_SHOWN,},#{||:#{!=:CODENAME,},#{&&:#{!=:TITLE_RAW,},#{!=:LEAD_STATE,}}}}}`
  — the `TITLE_RAW && LEAD_STATE` arm keeps a state-only anchor (agent exited,
  `@crew_state` still set; or a mirror lead with state but no codename) drawing
  `━━ <glyph> <state> ━━` exactly where it drew its state before, and a
  state-only anchor with no title stays plain, as today. Codename/state
  segments, W math and all other branches unchanged; an anchor whose only
  content would be an unclipped title falls to `PLAIN_BRANCH`.
- **Full title at source**: `sanitize_title` cap 50 → 256 (GitHub's title
  limit; Linear's is similar). The daemon's title cap goes 120 → 257
  via a new `labelTitleMaxRunes = 257` (256 + the rest's leading space) applied
  to the two titles and the label rest only; the regex-validated fields
  keep 120. Line 0 (`picker/statusline`) previously got ≤50-char titles for
  free; it now clips them itself so line 0 renders as today: the local
  `issueTitle` at 50 runes, and the mirror **id arm's** `bridgeLabelRestLong`
  at 51 (it carries a leading space). The mirror branch-only arm carries a
  branch name, not a sanitize_title output, which reached line 0 at up to 120
  runes (the old daemon cap); it is clipped at 120 so line 0 stays as today now
  that the daemon cap is 257.
- **Visible change, intended**: with titles up to 256 chars, rung 1.5
  (`reflow_clip_rests`) can give a long-titled window more of a single row than
  the old 50; the grid stays capped by `MAX_REST_WIDTH`. Documented in
  status-bar.md. Windows on a live server lack `@window_label_clipped` until
  the next forced reflow (home-manager activation reflows every session).

## Consumer map (`@issue_title` / `@pr_title` length, `@window_label_rest_long`)

| consumer | file | disposition |
| --- | --- | --- |
| reflow label build | `scripts/tmux-reflow-windows.sh`, `build_window_label` | compatible (visible) — widths measured, grid capped by `MAX_REST_WIDTH`; single-line rung 1.5 may show more than 50 chars of a long title (intended, documented) |
| status-format[1..4] | reflow / `config/tmux.conf.tmpl` | compatible — render `@window_label_disp` (display copy), never the raw rest |
| status line 0 | `picker/statusline/main.go` (`issueTitle`, `bridgeLabelRestLong`) | **changed** — local title clip 50, mirror id-arm rest clip 51, branch-only arm clip 120 (all = today's effective caps); tests in `main_test.go` |
| picker rows | `picker/tui.go` `truncID` | compatible — `truncateCells` to budget |
| enrich card | `picker/enrichcard/model.go` | compatible — `truncate(…, titleWidth())` |
| daemon shipping | `windowlabels.go` `cleanLabelValue` | **changed** — fields 15/16/21 (titles, label rest) use new `labelTitleMaxRunes = 257`, field 20 (label id) keeps 120; `windowlabels_test.go` loop split per field (300-rune input exceeds both) |
| pane border | `scripts/tmux-apply-theme-colors.sh` | **changed** — gated; W clip unchanged |
| tmux-issue-stamp / pr-enrich writes | argv `set-option` | compatible — argv, no reparse; `#`/`'` still stripped |
| backfill/stamp tests | `tests/enrich.bats` | **changed** — cap pin 50 → 256 |

## File list

- `scripts/tmux-reflow-windows.sh` — compute + change-gated write of `@window_label_clipped`.
- `scripts/tmux-apply-theme-colors.sh` — gate title segment on it.
- `scripts/lib-enrich.sh` — `sanitize_title` cap 256.
- `picker/statusline/main.go`, `picker/statusline/main_test.go` — line-0 clip at 50 runes.
- `picker/remotebridge/daemon/windowlabels.go`, `windowlabels_test.go` — `labelTitleMaxRunes = 257` for the two titles and the label rest.
- `tests/pane-border-format.bats` — title helpers + the `#(...)` injection test set `@window_label_clipped`; new unclipped cases; drawn flip test driving the real reflow.
- `tests/reflow-fanout.bats` — integration regression (real reflow stamps `@window_label_clipped`; border resolved through the real theme script).
- `tests/enrich.bats` — cap pin.
- `flake.nix` — UTF-8 locale for `pane-border-format-tests` (Step 7 runs the real reflow).
- `docs/agents/status-bar.md`, `docs/agents/scripts.md`, `docs/agents/enrichment.md` (if it states the 50 cap) — docs.

## Steps

- [ ] **Step 1: failing integration test** — `tests/reflow-fanout.bats`: new test. Sets the extra `@thm_*` the theme script reads, runs `scripts/tmux-apply-theme-colors.sh`, captures `FMT=$(tmux -u show -gv pane-border-format)`. Stamps S:0 with `@branch feat/885-x`, `@issue_branch feat/885-x`, `@issue_id '#885'`, and a fixed ~120-char `@issue_title` held for the whole test. (a) `bash "$REFLOW" S 400 --force` (row fits whole) → `@window_label_clipped` unset and `display-message -p -t S:0 -F "$FMT"` with `#[…]` stripped = `━━━━━`. (b) width-only flip: `bash "$REFLOW" S 90 --force` (same title, label must clip) → `@window_label_clipped` = 1, border contains the title text past char 50 (proves the source is no longer cut) and more than `@window_label_disp` shows. (c) `bash "$REFLOW" S 400 --force` → option unset again, border plain again. (d) window-count flip: at width 400 (S:0 alone fits whole, per (a)), add 3 windows via `tmux new-window -d` each with a 60-char ASCII `@branch` (shorter than S:0's title so S:0 stays widest) + reflow at 400 → clipped=1 on S:0 (the four long labels cannot share one 400-col row whole); kill them + reflow at 400 → unset. All reads via `tmux -u`. Run: `nix build .#checks.x86_64-linux.reflow-fanout-tests` → fails on the base tree (option never written; border shows title in (a)).
- [ ] **Step 2: failing source-cap test** — `tests/enrich.bats`: change the truncation test to 256 (300-char input → 256 out; a 120-char title survives whole). `picker/statusline/main_test.go`: a 120-char local `issueTitle` renders its first 50 runes; a mirror id-arm rest of `" "+50 chars` renders whole (leading space kept) and one of `" "+120` renders 51 runes; a branch-only mirror rest of 100 runes renders whole, 200 renders 120. Run `nix build .#checks.x86_64-linux.enrich-tests` / `(cd picker && go test ./statusline/)` → fail.
- [ ] **Step 3: reflow stamp** — `scripts/tmux-reflow-windows.sh`: add `#{@window_label_clipped}` to FMT right after `#{@window_has_agent}` (closed token) and `wclip` to the `read -r` list right after `hasagent` (same position — a mismatch shifts every later field), stored in `win_clip_prev[$idx]`. In the display loop, after the final `win_id_disp`/unpadded rest is known (both the single-line `continue` arm and the grid arm, before `pad_to_width`), set `win_clip[$idx]=1` if `"${id_disp}${rest_disp}" != "${win_id[$idx]}${win_rest_long[$idx]}"`, else empty. In the per-window exec, build argv as an array and append `';' set -w -t "$target" @window_label_clipped 1` or `';' set -wu -t "$target" @window_label_clipped` only when `win_clip != win_clip_prev`. If the drawn test (Step 7) shows the border does not redraw on a user-option change, change the batched `refresh-client -S` to `refresh-client` only when some clip flag changed (same `source -` batch, no fork).
- [ ] **Step 4: border gate** — `scripts/tmux-apply-theme-colors.sh`: `TITLE_SHOWN="#{?@window_label_clipped,${TITLE_RAW},}"`; use it in `TITLE_SEG` (both the presence test and the clip body); `ANCHOR_COND="#{&&:${ANCHOR},#{||:#{!=:${TITLE_SHOWN},},#{||:#{!=:${CODENAME},},#{&&:#{!=:${TITLE_RAW},},#{!=:${LEAD_STATE},}}}}}"`. Comment one line on why.
- [ ] **Step 5: update border unit tests** — `tests/pane-border-format.bats`: `title()`, the two long-title tests ("long title is clipped…", "drawn border…") and the `#(...)` injection test ("a #(...) carried in a codename or title renders as text") also set `@window_label_clipped 1`, and the injection test additionally asserts the rendered output contains `#(touch $OG_TMUX_DIR/ran-title)` so the title half is really exercised; add "unclipped label: anchor shows codename/state but no title", "unclipped, no codename/state: plain bar", and "state-only anchor (pane `@crew_state done`, no `@window_has_agent`/`@crew_name`), title stamped, clipped unset → `━━ ✓ done ━━`" plus the same with clipped=1 → `━━ ✓ done · #858 feat: full title ━━`. Run `nix build .#checks.x86_64-linux.pane-border-format-tests` (name per flake) → green; Step 1 test green.
- [ ] **Step 6: source caps** — `sanitize_title` `${clean:0:256}` + comment; `labelTitleMaxRunes = 257` in `windowlabels.go` for fields issueTitle/prTitle/labelRest (labelID stays at labelTextMaxRunes), test loop split per field; drop the stale `#() argv in status-format[0]` rationale from the sanitize_title comment; in `picker/statusline/main.go` add a local `clipRunes(s string, n int) string` helper and apply it: `a.issueTitle` at 50, id-arm `a.bridgeLabelRestLong` at 51, branch-only arm at 120. Step 2 tests green; `go test ./...` in `picker`.
- [ ] **Step 7: drawn flip** — in `tests/pane-border-format.bats` (its check already exports HOME and copies `scripts/`), a test that builds a runnable reflow the same way reflow-fanout.bats does (sed the `@lib_*@` placeholders to repo paths), sets `LANG`/`LC_ALL=C.UTF-8` for the `pane-border-format-tests` check in `flake.nix` (reflow measures display width) and keeps labels ASCII with the enrich icons sed to ASCII as reflow-fanout.bats does, attaches a real client via the `og-outer` pattern with `pane-border-status top`, then: long title + reflow at a clipping width → captured line 1 contains the title; reflow at a fitting width (via `resize-window` + reflow with that width) → line 1 has no title. If it does not flip, apply Step 3's contingency. Run `nix build .#checks.x86_64-linux.pane-border-format-tests`.
- [ ] **Step 8: docs** — `docs/agents/status-bar.md` "Pane border labels" item 4 + `@window_label_clipped` semantics, the rung-1.5 visible change and the live-server-until-next-reflow note, and that a mirror gets the full title only once the remote host runs the new cap too; `docs/agents/scripts.md` rows for `tmux-reflow-windows` (writes `@window_label_clipped`) and `tmux-apply-theme-colors` (gates on it); `docs/agents/enrichment.md` / any doc mentioning the 50 cap.
- [ ] **Step 9: gate** — `nix build .#default`, `nix flake check`, `nix build .#lint` all pass.

## Acceptance

- [ ] Bats regression red→green on a scratch server: fits → no title; trimmed → full title; flip updates → Steps 1, 5, 7 (`nix build .#checks.<sys>.reflow-fanout-tests`, red on the base tree).
- [ ] Source title was cut → full title test → Step 2/6 (`tests/enrich.bats` 120-char survives; border in Step 1 shows beyond 50 chars).
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 9.
- [ ] Docs updated → Step 8.
