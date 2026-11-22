# Status bar: repo tag width accounting and per-repo colour (#1216)

## Problem

`crewRepoTag` (`generator/render/status.go`) draws `<repo> ` ahead of the crew
badge in `status-format[1]`, but `scripts/tmux-reflow-windows.sh` never reads
`@crew_repo`, so no fit total, column floor or overflow drop counts it. A long
line runs past the right edge and clips the trailing PR badge. The tag is also
always `@og_c_host_peach`, indistinguishable from a bridge host badge.

## Design

Width: pull the tag's text through the reflow `FMT` and charge it exactly where
the host badge is charged.

- `FMT` gains one field right after the bridge-host field (read as `crepo`
  between `bhost` and `wname`), evaluated with the *same predicate as the
  format*: `#{?#{&&:#{@crew_repo},#{&&:#{!:#{@bridge_placed}},#{!=:#{@crew_repo},#{b:session_path}}}},#{s/[|]/ /:@crew_repo},}`.
  An invisible tag yields an empty field and `'|'` is squashed, so later columns
  cannot shift. The field-order comment above `FMT` is updated.
- `win_repo_dw[idx]` = width of `"<repo> "` (0 when hidden).
- Charged in `total_long` / `total_short` only. The tag is drawn solely by the
  global single-line `status-format[1]` (`ENTRY`, used by every grid/multi-line
  layout, has no repo fragment), so the grid column floor and the overflow drop
  must not charge it: that would shrink labels for a segment never drawn there.
  Rung 1.5 derives its overshoot from `total_long`, so the label is clipped with
  `…` and the PR badge stays whole. Drawing the tag in the grid is out of scope.

Colour: a deterministic hash of the repo name into the five-name host palette
(`peach teal pink yellow sky`), computed in pure bash: sum of byte values (`printf -v c %d "'$ch"` under
`LC_ALL=C`) mod 5, cached in `repo_color[repo]` (no fork) and stamped as `@window_repo_color` = `#{@og_c_host_<name>}`,
the same indirection `@window_host_color` uses. The format becomes
`#{?#{@window_repo_color},#{E:@window_repo_color},#{@og_c_host_peach}}`, so an
unstamped window falls back to peach.

Why not reuse `tmux-picker-generate host-colors`: that assigns palette slots by
the *order of hosts in `@remote_bridge_hosts`* (stable per configured host
list), so a repo name has no slot and it costs a fork. A pure hash needs no
shared state, is identical across windows, reflows and sessions, and keeps the
repo palette decoupled from the host assignment. Trade-off: two repos can hash
to one colour (five slots); acceptable for a tell-apart cue.

## Steps

1. `generator/render/status.go` and `config/tmux.conf.reference.nix:1072`
   identically: colour via `@window_repo_color` with peach fallback.
   `status_test.go`: set distinct `@og_c_host_peach`/`@og_c_host_teal`; unset
   stamp -> output has `fg=<peach>`; after `@window_repo_color
   '#{@og_c_host_teal}'` -> `fg=<teal>`, not peach.
2. `scripts/tmux-reflow-windows.sh`: FMT field + comment, `crepo` read,
   `win_repo_dw`, charge in totals, `repo_color` hash, `@window_repo_color` in
   `build_window_argv` (empty when the tag is hidden).
3. `tests/reflow-host-colour.bats`: (a) two windows, same foreign repo -> same
   non-empty `@window_repo_color` matching the `@og_c_host_*` pattern; a window
   whose repo equals `basename` of session_path, and a placed mirror, get empty.
   (b) single-line session too narrow for tag+crew+label+PR: `@window_label_disp`
   is shaved by at least the tag width versus the same window without the repo
   and `@window_pr_num` stays whole (red without the fix).
4. Docs: `status-bar.md` (replace the "deferred" sentence; note a late
   `@crew_repo` stamp waits for the next keyed reflow), `scripts.md` reflow row.
5. Gate: `nix build .#default`, `nix flake check`, `nix build .#lint`.
