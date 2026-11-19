# Status bar: raise light-theme (Latte) contrast (#1203)

## Mechanism

The status bar, the folded line, the pane-border/park chrome and `picker/statusline` read `#{@thm_*}` directly, so a Latte colour can only change by editing every format. Precedent for sharing a table between Go and bash: `picker_generate host-colors` (the Go package owns slot assignment, bash calls it).

Design (one source, no copied table):

- `picker/palette` gains `StatusRoles []NamedRole` (status-bar roles; `Role` already carries dark and light option+hex). Dark halves are today's options verbatim (Mocha unchanged); light halves are darker Latte options.
- `picker_generate status-roles <flavor>` prints `name option` per role (same pattern as `runHostColors`).
- `tmux-apply-theme-colors.sh` (runs at config load and on every theme change, after catppuccin) calls it and stamps `set -gF @og_c_<name> "#{<option>}"`, so the resolved hex is a server option that follows the flavor.
- Every status-bar format (`tmux-reflow-windows.sh`, `tmux-status-fold.sh`, `tmux-status-autofold.sh` if it renders colour, `generator/render/status.go` flags for `picker/statusline`, `park.go` `parkDimStyle`) swaps `#{@thm_X}` for `#{@og_c_<role>}` for the failing roles only. Rendered Mocha colour is identical because the option resolves to the same `@thm_*`.
- Host tints: `hostcolor.Palette` slot names stay; reflow stamps `#{@og_c_host_<slot>}` instead of `#{@thm_<slot>}`; `statusline.hostHex` receives the same options as flags.

## Decisions (supersede Mechanism where they differ)

**Colour source.** `palette.StatusRoles` (new, in `picker/palette`). A `Role` may carry a literal light hex (`LightOpt == ""`, `Env(true)` returns ""). Light choices: dim/divider/marker/ago/prefixes/pane-border chrome/default-host fallback owner -> `@thm_overlay_2` (3.49); PR closed + review_required -> `@thm_subtext_1` (4.37); PR pending -> `@thm_maroon`; PR success/approved -> `@thm_teal`; park (on mantle) -> `@thm_overlay_2` (3.25). Host tints, five slots: literal darkened hexes (only maroon/teal clear 3:1 as Latte options): peach #c4500a, teal #179299, pink #c03fa0, yellow #a8650c, sky #0a7fa8, each >= 3.8 on base/mantle; the executor verifies pairwise separation and swaps a hue if two are too close (`TestStatusHostTintsDistinct`). Roles for hosts join `RowRoles` (so `TestRowRolesCoverRowBuilders` and the SelBg/mantle pairs cover them); the session picker's `hostLightRoles` (`picker/remote.go`) uses the same roles, so bar and picker agree; update `picker/remote_test.go` (~1524-1534). Default-host fallback (`#{@thm_peach}` in tmpl, reflow, `FOLD_HC`) -> the peach host role.

**Plumbing.** `picker_generate status-roles <flavor>` prints `name value` per line, value = an `@thm_x` option (Mocha always; most Latte) or a literal `#rrggbb`. `tmux-apply-theme-colors.sh` reads the flavor from `@catppuccin_flavor`, and per line runs `tmux set -gF @og_c_<name> "#{<opt>}"` for an option or `tmux set -g @og_c_<name> <hex>` for a literal. Unit test covers both cases (latte host_* print hexes, mocha prints options).

**Nix.** Keep `tmux-apply-theme-colors` in `scriptNames` (config/tmux.conf.nix:301; `script.*` is generated from it and lines 523/836 reference it) and only ADD it to `scriptsWithPickerBin` (~554) so `@picker_generate@` is substituted. `tmux-client-theme` replays the built script by store path: no change.

**Unset behaviour: leave unset.** Formats use bare `#{@og_c_X}` (no inline `@thm_*` fallback, hot path stays short). The apply script skips the stamping block when `@picker_generate@` is still a literal `@...` (test copy) or `status-roles` fails/prints nothing, without aborting the pane-border/fingers sections. Pane-border chrome: `pane-border-style` cannot hold a nested format, so the script stamps first, then reads the value back (`tmux show -gv @og_c_dim`) and interpolates it like it does for `$thm_*` today; when unset it keeps `$thm_overlay_1`.

**Call sites to change** (all in scope): `config/tmux.conf.tmpl` ~437 (statusline flags keep their names `--thm-overlay1/peach/green/pink/yellow/sky`, values become `#{@og_c_*}`, so remote-m2-integration lines ~4152 and ~7978 are unchanged) and ~459; `config/tmux.conf.reference.nix` ~1051/~1073 (edited with the tmpl; extraction check diffs them); `scripts/tmux-reflow-windows.sh` (marker, sep, PRCOLOR, PRNUM, AGO, HOST, prefixes, `FOLD_FMT`/`FOLD_RIGHT`/`FOLD_HC`, host_color stamp ~597 -> `#{@og_c_host_<slot>}`); `picker/remotebridge/daemon/park.go` `parkDimStyle`; `generator/render/status.go` `crewRepoTag` peach. `tmux-status-fold.sh`/`autofold.sh` render no colour. which-key (`generator/render/keys.go`) out of scope.

**Pinned old text to update with the new text**: `flake.nix:1161` host-badge-conf-assertions grep (`#{@thm_peach}` -> `#{@og_c_host_peach}`); `tests/remote-m2-integration.bats` ~5099 `PARK_DIM_STYLE`; `tests/reflow-host-colour.bats` line 80 `picker_token` and lines ~125/~218 (marker text, overlay_1/subtext_0); `tests/status-fold.bats` ~233-236 (`@window_host_color` seed). Suites whose setup loops set `thm_*` and render or pin these formats get the needed `@og_c_*` seeded in setup, or are confirmed not to assert colour: status-fold, status-autofold, update-icons-*, reflow-fanout, reflow-host-colour, launcher-hosted-agent, pane-border-format. Run only targeted bats files.

**Tests.** Extend palette_test `latte` map with every new LightOpt; `darkLiterals`/`namedRoles()` for StatusRoles; `TestLightHexMatchesLatte` skips literal roles; `TestStatusDarkLiterals`. Production-path bats: run the built apply script on a scratch server (own TMUX_TMPDIR/CLAUDE_STATUS_DIR/XDG_DATA_HOME), flavors mocha and latte, assert every `@og_c_*` is a non-empty hex and Mocha values equal the old `@thm_*` hex. Measuring: a committed `TestStatusPairsReport` (`go test ./palette/ -run TestStatusPairsReport -v`) logs each pair's ratio; the before numbers come from running it against the old options.

**Evidence.** Red demo = command plus output (lower one StatusRoles LightOpt to `@thm_overlay_1`; `go test ./palette/ -run TestLightPairsMeetFloor`; restore). Mocha pins named in PR: TestDarkLiterals, TestStatusDarkLiterals, status-roles mocha unit test, reference-diff extraction check, apply-script bats. Captures must be a visible PNG (render `capture-pane -e` through the repo's existing capture tooling or a terminal screenshot), full and folded bar, before/after, light theme.

## Steps

1. Measure: a Go test helper prints ratio of every status-bar fg/bg pair on Latte (fgs from the table in the task: overlay1, peach, green, overlay0, yellow, pink, sky; plus `subtext_0` usage, mauve, red, blue, teal used on `@thm_bg`; folded line; park overlay0 on mantle). Record before-ratios for the PR table.
2. `palette.StatusRoles` + `StatusPairs` (fg role on Base or Mantle, floor 3.0, or 4.5 for primary text) wired into `LightPairs`, so `TestLightPairsMeetFloor` covers them. Light choices and host tints: see Decisions (single source of colour choices).
3. `picker` `status-roles` subcommand + unit test (output covers every role, flavor `mocha` returns the dark options).
4. Wire `tmux-apply-theme-colors.sh`, formats and `statusline` flags to `@og_c_*`; update bats that pin the old format text; keep `@thm_*` where the pair already passes.
5. Tests: `TestStatusDarkLiterals` pins Mocha options byte-identical to the old `@thm_*`; extended `TestLightPairsMeetFloor` (show red by lowering one role, then green); a bats check that every `@og_c_<name>` referenced in the scripts/generator exists in `status-roles` output (no dangling option renders uncoloured).
6. Docs: `docs/agents/theme.md` (extend the #1194 bullet: status bar covered, the `@og_c_*` indirection, host tints now distinct in light), `docs/agents/status-bar.md`, `docs/agents/scripts.md` rows for the touched scripts.
7. Light-theme captures of the full and folded bar, before and after (scratch tmux server per CLAUDE.md, `agent-browser` not needed; render with `capture-pane -e` to text/PNG). PR body: pair table before→after.
8. Gate: `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Risks

- `@og_c_*` unset before apply-theme-colors runs, or the stamping block skipped: segments render uncoloured (accepted, see Decisions: leave unset); the production-path bats pins that both flavors stamp.

## Critic notes folded in

- Every new bats file gets its own `checks.<system>` entry in `flake.nix` (copy the `reflow-host-colour-tests` entry, ~2808; copy only the files it reads; the reference check copies every file it scans). No check globs `tests/*.bats`.
- The skip guard in `tmux-apply-theme-colors.sh` is `[[ $gen == @* ]]` (never compare to the full placeholder string; `replaceStrings` would rewrite it).
- Name the role for each statusline flag, including its other consumers: `--thm-peach` also feeds the agent-usage warn colour (`usage.go:188`), `--thm-green` the usage ok colour (`usage.go:190`); repoint them to the PR-pending/success roles' host-neutral equivalents, and `crewRepoTag` peach (`status.go:91`) to the default-host peach role.
- Host-tint hex ratios are >= 3.0 (teal ~3.07 on mantle), not 3.8; expect `TestStatusHostTintsDistinct` to force a hue swap (peach vs yellow, teal vs sky).
- `picker/remote_test.go:1523` (`allowed` map) updates with ~1524-1534.
- Existing flake checks copying the raw apply script (~740, 761, 2400, 2830, 3007) must keep passing via the skip guard; confirm none asserts the stamping.
- Out of scope: `scripts/og-notify.sh:108-109` Latte peach/green (message line, not the bar); mention in the PR.
