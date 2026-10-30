# Dwindle auto split and balance on close — implementation plan (#1026)

Spec: `docs/superpowers/specs/2026-10-30-dwindle-auto-tiling-design.md`.
Every tmux repro or test runs on its own socket with its own `TMUX_TMPDIR` and
`CLAUDE_STATUS_DIR`. Never read `.env*`, credentials or keys; never print a
secret value.

## File list

| File | Purpose |
|---|---|
| `picker/remotebridge/controlmode/rebalance.go` (new) | `RebalanceClosedCell(prev, cur string) (Layout, bool, error)`: the cell diff + even-out on parsed trees, v1/v2 in, v1 `Raw` out |
| `picker/remotebridge/controlmode/rebalance_test.go` (new) | unit cases for the above |
| `picker/dwindle/main.go` (new) | `tmux-dwindle balance <@win> <cache>`: one `display-message` read, prechecks, order check, one guarded `if-shell -F … select-layout` |
| `picker/dwindle/main_test.go` (new) | state parsing, order check, guard/command text, no-op paths (fake runner, no tmux) |
| `picker/default.nix` | add `"dwindle"` to `subPackages`; `mv $out/bin/dwindle $out/bin/tmux-dwindle` |
| `config/tmux.conf.nix` | `dwindleEnable ? true` arg; `picker-dwindle-bin`; `bin.tmux-dwindle` in `pathsToml`; `dwindle.enable` in `configToml`; pass both to the reference import |
| `config/tmux.conf.reference.nix` | new args `dwindleEnable`, `picker-dwindle-bin`; `dwindleFlag`; the same new tmux.conf lines as the template (two-file rule) |
| `config/tmux.conf.tmpl` | `set -gw @dwindle`, `prefix + Enter`, `prefix + A`, `window-layout-changed[20]` hook |
| `generator/config/config.go` (+ `config_test.go`) | `Dwindle{Enable}` under `[dwindle]`; absent key defaults to `true` |
| `generator/render/render.go` (+ `status_test.go`) | `DwindleFlag` (`onOff`) |
| `generator/initcfg/render.go` | commented `[dwindle] enable` line + its description (`TestRenderCoversEveryField` enforces) |
| `generator/paths/paths.go` (+ `paths_test.go` if it enumerates) | `"tmux-dwindle"` in `RequiredBin` |
| `modules/home-manager.nix` | `dwindle.enable` option (default `true`), passed as `dwindleEnable` |
| `flake.nix` | extraction matrix entries 11/12 move `dwindleEnable`; new `dwindle-tests` check |
| `tests/dwindle.bats` (new) | behaviour on a scratch server with the real wrapped tmux + rendered conf + attached client |
| `README.md`, `docs/agents/scripts.md`, `docs/agents/performance.md`, `CLAUDE.md` | keys + option; `tmux-dwindle` row; gate cost; the "Who runs what" hook list |

## Shared definitions (used verbatim by several steps)

```
COUNT = #{n:#{P:#{?pane_floating_flag,,x}}}
GEOM  = #{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}
SIG   = COUNT_#{window_zoomed_flag}_#{window_width}x#{window_height}_GEOM
WIDE  = #{e|>|:#{pane_width},#{e|*|:#{pane_height},2}}
```

Hook body (`window-layout-changed[20]`, template and reference byte-identical
apart from the binary's path interpolation):

```
set-hook -g window-layout-changed[20] { if -F '#{&&:#{==:#{@dwindle},on},#{&&:#{!:#{@bridge_win}},#{!=:SIG,#{s/[|].*//:@dwindle_cache}}}}' { if -F '#{&&:#{e|<|:COUNT,#{s/_.*//:@dwindle_cache}},#{&&:#{m/r:^[0-9]+_0_,#{@dwindle_cache}},#{&&:#{!:#{window_zoomed_flag}},#{!=:#{@crew_grid},1}}}}' { run-shell -b "<tmux-dwindle> balance #{q:window_id} #{q:@dwindle_cache}" } ; set -wF @dwindle_cache 'SIG|#{window_layout}' } }
```

`tmux-dwindle` state read (fields `|`-separated, layout last):
`#{@dwindle}|#{window_zoomed_flag}|#{@crew_grid}|#{@bridge_win}|#{window_width}x#{window_height}|#{P/i:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}|#{window_layout}`.

## Steps

- [ ] **Step 1: controlmode rebalance tests (red).** Create
  `picker/remotebridge/controlmode/rebalance_test.go`, table-driven over
  `RebalanceClosedCell(prev, cur)`, with layouts built from tmux's own dumps
  (capture the v2 ones from a scratch server; v1 from `writeCell` + checksum).
  Cases and expected result:
  1. v1, three columns 66/66/66 at 200x50, prev has `%0 %1 %2`, cur is tmux's
     post-kill `%0`=66 `%2`=133 → changed, widths 99/100, x offsets 0/100,
     `Panes` order `%0 %2`, `Raw` checksum equals `layoutChecksum(body)`.
  2. Same in v2 JSON (prev and cur), and mixed (v2 prev, v1 cur) → same `Raw`.
  3. `{[A/B] | [C/D/E]}` with A/B hand-sized (e.g. A h=10, B h=39), kill D:
     cur has C/E uneven → C/E heights even (sum preserved), A/B cells
     byte-identical to cur's, both columns' widths unchanged.
  4. `k == 2` collapse (`{A | [B/C]}` kill C) → unchanged (`false`).
  5. Container-only children: `{[A/B] | X | [C/D]}` kill X → the two columns
     even out; each column's internal heights unchanged (perpendicular
     rescale) — and a same-axis nested child (`{A | {B|C} | D}` shape via
     prev/cur strings) rescales B/C proportionally, largest remainder.
  6. v2 with a float leaf in both prev and cur → float ignored, same result as
     case 1.
  7. Two panes gone; one pane arrived; parent cell not found in cur
     (cur restructured, e.g. `even-vertical`); already even → each `false`, no
     error.
  8. Minimum size: a child so small that rescaling would push a leaf below 1
     cell → `false`.
  9. Cached tree differs from live at equal geometry
     (`{[A/B] | [C/D]}` cached as `[{A|C} / {B|D}]`), kill `D` → `false`
     (the cached parent of `D` is `{B|D}` with `k == 2`, a collapse).
  Command: `cd picker && go test ./remotebridge/controlmode/ -run Rebalance`
  → fails to compile (`RebalanceClosedCell` undefined).

- [ ] **Step 2: implement `RebalanceClosedCell`.** New file
  `picker/remotebridge/controlmode/rebalance.go`, same package (uses the
  unexported `node`, `parseTree`, `pruneFloats`, `writeCell`, `collectLeaves`,
  `layoutChecksum`). Algorithm exactly as spec steps 2–4: parse both, prune
  floats; leaf-id sets must satisfy `old − new = {P}`, `new ⊆ old`; find `P`'s
  parent `C` (`k ≥ 3`, else false); find `Q` in cur with `C.kind` whose
  children's leaf-id sets equal `C`'s minus `P`, in order; sizes along the axis
  (`w` for `{`, `h` for `[`), `T = Σ`, each `T/n`, last `T − (n−1)·(T/n)`;
  already equal → false; `resize(child, axis, size)` recursion (perpendicular:
  all children get `size`; same axis: proportional, largest remainder,
  `size − (m−1)` among `m` children; any leaf `< 1` → false); recompute
  offsets inside `Q` from `Q.x/Q.y` (+size+1 per sibling); return
  `Layout{W,H,Panes (DFS), Raw: "%04x,"+body}`. Doc comment states the
  contract in one paragraph. Command: `cd picker && go test
  ./remotebridge/controlmode/` → all pass.

- [ ] **Step 3: `picker/dwindle` tests (red).** Create
  `picker/dwindle/main_test.go` against a `run(args []string, tmux runner)`
  seam where `runner` is `func(args ...string) (string, error)` (a fake
  records calls and returns canned `display-message` output). Cases:
  1. Happy path (case-1 layouts): exactly two runner calls — the state read
     (`display-message -p -t @1 <state fmt>`) and `if-shell -t @1 -F <guard>
     <cmd>`; `<cmd>` is `select-layout -t @1 '<Raw>'`; `<guard>` contains
     `#{==:#{@dwindle},on}`, `#{==:#{window_zoomed_flag},0}`,
     `#{!=:#{@crew_grid},1}`, `#{!:#{@bridge_win}}` and the literal
     `200x50|<GEOM as read>`.
  2. Prechecks: `@dwindle` off, zoomed `1`, `@crew_grid` `1`, `@bridge_win`
     set → one call (the read), no `if-shell`.
  3. Pane-list order (from the `#{P/i:}` field) differing from `Panes` order
     → no `if-shell`.
  4. Bad args: window id not `^@[0-9]+$`, cache with no `|`, empty layout
     half, unparsable layout → exit 0, no tmux call beyond what is needed
     (bad window id: none).
  5. Read failure (runner error) → no further call.
  Command: `cd picker && go test ./dwindle/` → fails to compile.

- [ ] **Step 4: implement `picker/dwindle/main.go`.** `package main`;
  `main()` wires `run(os.Args[1:], execTmux)` where `execTmux` runs
  `exec.Command("tmux", args...)` and returns trimmed stdout. Usage:
  `balance <@window> <cache>`; anything else exits 2 with a usage line on
  stderr; every runtime no-op exits 0 silently (a hook job's output is
  discarded). Split the read into 7 fields (`strings.SplitN(out, "|", 7)`),
  prechecks, `controlmode.RebalanceClosedCell(prevLayout, curLayout)`, order
  check against ids parsed from the `#{P/i:}` field (`%N.l.t.w.h` tokens),
  build guard + command; quote `Raw` in single quotes (it holds only
  `[0-9a-f,x{}\[\]]`). Command: `cd picker && go test ./dwindle/ && go vet
  ./dwindle/` → pass.

- [ ] **Step 5: package the binary.** `picker/default.nix`: append
  `"dwindle"` to `subPackages`, add `mv $out/bin/dwindle
  $out/bin/tmux-dwindle` to `postInstall`. `config/tmux.conf.nix`:
  `picker-dwindle-bin = "${picker-generate}/bin/tmux-dwindle";` beside
  `picker-session-res-bin`; `tmux-dwindle = picker-dwindle-bin;` in
  `pathsToml.bin`; `inherit picker-dwindle-bin;` into the reference import.
  `config/tmux.conf.reference.nix`: add `picker-dwindle-bin,` and
  `dwindleEnable,` to the args and the `dwindleFlag` let (mirrors
  `resumeCarouselFlag`); `config/tmux.conf.nix` gains the `dwindleEnable ?
  true` arg and passes `inherit dwindleEnable;` to the reference import here
  too, so the reference stays evaluable from this step on.
  `generator/paths/paths.go`: add `"tmux-dwindle"` to `RequiredBin`
  (sorted). Command: `nix build .#default && ls -l result/bin/tmux` and
  `nix build .#default --print-out-paths` succeeds; `cd generator && go test
  ./paths/` passes (update `paths_test.go` fixtures only if it enumerates
  `RequiredBin` literally).

- [ ] **Step 6: generator config + flag.** `generator/config/config.go`:
  `Dwindle Dwindle \`toml:"dwindle"\`` and `type Dwindle struct{ Enable bool
  \`toml:"enable"\` }`; in `applyDefaults`, `if !md.IsDefined("dwindle",
  "enable") { c.Dwindle.Enable = true }` with a one-line why (on by default
  on every install path). Tests in `config_test.go`: absent key → `true`;
  `enable = false` → `false`; `Defaults().Dwindle.Enable == true`.
  `generator/render/render.go`: `DwindleFlag string` = `onOff(cfg.Dwindle.Enable)`;
  add both-ways rows to `TestFlagStringsBothWays`. `generator/initcfg/render.go`:
  `[dwindle]` section with `writeCommented(&b, "enable",
  boolLit(d.Dwindle.Enable), "dwindle.enable")` and a description entry
  (`"Dwindle auto-balance: even out the split group a closed pane leaves.
  prefix + A toggles it per window."`). Command: `cd generator && go test
  ./...` → pass (fails before the initcfg line: `TestRenderCoversEveryField`).

- [ ] **Step 7: bats tests (red) + check.** Create `tests/dwindle.bats`
  modelled on `tests/float-tool-focus.bats`: `inner`/`outer` servers on
  per-test `-L` sockets, `TMUX_BIN` (the built wrapper), isolated `HOME`,
  `XDG_*`, `CLAUDE_STATUS_DIR`, `OG_ENRICH_*`, `OG_AGENT_USAGE_DIR`,
  `@splash_shown 1`, `attach_client` (200x50) only in the keypress cases.
  Helpers: `wait_for <cmd>` (poll 0.1s up to 30s, `POLL_TRIES` like
  `grid-refit.bats`), `cache_count <win>` =
  `display -p -t <win> '#{s/_.*//:@dwindle_cache}'`, `widths <win>`.
  Every pane runs `sleep 600` (no shell init noise); windows are created at
  200x50 via the session size. Cases:
  1. three panes `select-layout even-horizontal`, `kill-pane` the middle →
     `wait_for` the two widths within 1 of each other (on `main`: 66/133,
     timeout → red).
  2. `[A/B] | [C/D/E]`: build with `split-window -h`, `split-window -v` in
     each column, `resize-pane -t A -y 10`; record A/B heights and both column
     widths; kill D → `wait_for` C/E heights within 1; A/B heights and column
     widths unchanged.
  3. (attached client) `prefix + Enter` in the full-width single pane → two
     panes side by side (`pane_left` differs, `pane_top` equal); then
     `split-window -h -l 40` (the new 40-wide pane is active; 40 < 2·height)
     → `prefix + Enter` → stacked (same `pane_left`).
  4. `set -gw @dwindle off`: three columns, kill middle; positive control in a
     second window with the mode on (`set -w @dwindle on` there) whose own close
     balances; once the control balanced, the first window still shows tmux's
     66/133.
  5. (attached client) `prefix + A` in window 1 → `show -wv @dwindle` is `off`
     and the status message shows `dwindle auto-balance: off`; close → same
     control-window proof → 66/133 kept; `prefix + A` again → `on`.
  6. `@crew_grid 1` (no `lead` pane, so grid-refit no-ops): three columns,
     kill middle → `wait_for` `cache_count` = 2 → widths still 66/133.
  7. float close: an uneven two-column window (66/133, built with the mode
     off for that window, then `set -w @dwindle on` and one `resize-pane` to
     write the cache), open `new-pane -d` (a float: not in `SIG`, so no write),
     kill the float, then the control-window proof → tiled widths unchanged
     and the cache unchanged.
  8. zoomed: three columns, `resize-pane -Z` the first, kill the middle →
     `wait_for` `cache_count` = 2 → widths are tmux's (no balance).
  9. no-op event writes nothing: after any settled layout, set
     `@dwindle_cache` to `"$(display -p '<SIG>')|SENTINEL"`, then fire a
     no-change layout event (`select-layout -E` on an already even window;
     if a probe shows it does not fire `window-layout-changed`, use
     `resize-pane -t <pane> -x <its width>`), positive-control sync, assert
     the cache still ends in `|SENTINEL`.
  10. conf assertion on `$TMUX_OG_CONF`: the `Enter` bind line carries
     `split-h,split-v} #{q:@bridge_pane}` in its bridge branch and
     `split-window -h` / `split-window -v` in its local branch.
  `flake.nix`: add `dwindle-tests` like `float-tool-focus-tests` (import
  `config/tmux.conf.nix` with `mkTmux`, `enrichEnable = false`,
  `agentUsageEnable = false`; `nativeBuildInputs` bash bats coreutils gnugrep
  gawk; `TMUX_BIN`, `TMUX_OG_CONF = <cfg>.tmuxConf`, `LANG`/`LC_ALL`
  `C.UTF-8`). Command (before Step 8): `nix build .#default && TMUX_BIN=$PWD/result/bin/tmux
  TMUX_OG_CONF=<conf path from the wrapper's -f> bats tests/dwindle.bats` →
  cases 1, 2, 3, 5, 10 fail (new behaviour); 4, 6, 7, 8, 9 may pass or fail
  but are not required red.

- [ ] **Step 8: template + reference lines.** In `config/tmux.conf.tmpl`
  and, transcribed with Nix interpolation, `config/tmux.conf.reference.nix`
  (two-file rule; `dwindleFlag` let mirrors `resumeCarouselFlag`):
  - beside `set -g @resume_carousel`: `set -gw @dwindle "{{.DwindleFlag}}"`
    with a one-line comment (window-scoped so `set -w @dwindle` overrides it
    per window; `prefix + A` flips that).
  - after the `_` split bind:
    `bind -N 'Split pane by its shape' Enter if-shell -F '{{.BridgeGate}}' { run-shell "{{.BridgeCtl}} #{?WIDE,split-h,split-v} #{q:@bridge_pane}" } { if-shell -F 'WIDE' { split-window -h -c "#{pane_current_path}" } { split-window -v -c "#{pane_current_path}" } }`
    with a two-line comment (pixel-width vs pixel-height: cells ≈ 1:2, strict
    `>`, `|`/`_` stay explicit).
  - `bind -N 'Toggle dwindle auto-balance for this window' A if-shell -F '{{.BridgeGate}}' { display-message "dwindle: not mirrored - toggle it on the remote" } { if-shell -F '#{==:#{@crew_grid},1}' { display-message "dwindle: grid window - tmux-grid-refit owns this layout" } { set -wF @dwindle '#{?#{==:#{@dwindle},on},off,on}' ; display-message "dwindle auto-balance: #{@dwindle} (this window)" } }`
  - after the grid `window-layout-changed` hook: the hook body above with
    `{{index .Paths.Bin "tmux-dwindle"}}` (reference: `${picker-dwindle-bin}`),
    under a comment naming the cache, why a write only happens on a `SIG`
    change (`options_push_changes` → `recalculate_sizes` + redraw, #793), and
    why the cached zoom field (tmux unzooms before a close).
  Command: `nix build .#default` then `TMUX_BIN=… bats tests/dwindle.bats` →
  all 10 pass; `./result/bin/tmux -L probe-$$ -f <conf> start-server \;
  kill-server` parses without error (isolated `TMUX_TMPDIR`).

- [ ] **Step 9: Home Manager option + Nix plumbing.**
  `modules/home-manager.nix`: `dwindle.enable = lib.mkOption { type =
  lib.types.bool; default = true; description = …; }` beside `aiNaming`;
  `dwindleEnable = cfg.dwindle.enable;` in the `tmux.conf.nix` import.
  `config/tmux.conf.nix`: `dwindle.enable = dwindleEnable;` in `configToml`
  (the arg and reference wiring landed in Step 5). `flake.nix` matrix: `dwindleEnable = false;` in `11-all-off`,
  `dwindleEnable = true;` in `12-all-on`. Command: `nix flake check` → the
  extraction check and every other check pass.

- [ ] **Step 10: docs.** `README.md`: prefix table rows
  `<kbd>Enter</kbd> | Split by shape (wide → side by side, tall → stacked)`
  and `<kbd>A</kbd> | Toggle dwindle auto-balance for this window`, plus a
  short "Dwindle auto-tiling" paragraph with `programs.tmux-og.dwindle.enable`
  (default `true`) and the exclusions (grid, float, zoom, mirror, restore).
  `docs/agents/scripts.md`: a `tmux-dwindle` row (invocation: the
  `window-layout-changed[20]` gate's `run-shell -b … balance #{q:window_id}
  #{q:@dwindle_cache}`; owns: `@dwindle_cache` is written only by that hook,
  `@dwindle` read-only; the order precondition and the apply guard).
  `docs/agents/performance.md`: a short section on the gate's per-event cost
  (no write on an unchanged `SIG`, one write per real change, a fork only on a
  tiled-count drop). `CLAUDE.md` "Who runs what" tmux-hooks bullet: add
  `tmux-dwindle` (`window-layout-changed[20]`). Command: `nix build .#lint` →
  pass (typos, markdown hooks).

- [ ] **Step 11: full local gate.** Inside the devshell: `nix build
  .#default`, `nix flake check`, `nix build .#lint` → all three succeed;
  `cd picker && golangci-lint run ./dwindle/ ./remotebridge/controlmode/` →
  clean (also run by `picker-go-lint`).

## Acceptance

| Task acceptance item | Settled by |
|---|---|
| three equal columns, kill middle → equal within 1 | `tests/dwindle.bats` case 1 (red on `main`) |
| nested layout, other group's hand sizes unchanged | case 2 (red on `main`) |
| smart split wide → side by side, tall → stacked | case 3 (red on `main`: `Enter` unbound) |
| option off globally and per window via toggle → tmux default sizes | cases 4 and 5 (5 red on `main`: `A` unbound) |
| no-op on `@crew_grid=1`, float close, zoomed | cases 6, 7, 8 |
| `nix build .#lint`, `nix flake check`, `nix build .#default` pass | Step 11 output |
| CI green (lint + build x86_64-linux + aarch64-darwin), run id cited | `gh run list --branch <branch>` / `gh pr checks` after push |
| spec + plan committed; scripts.md row; README option + keys | Steps 10 and the committed `docs/superpowers/` files |
