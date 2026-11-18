# Light-theme contrast sweep (#1194)

## Mechanism

Pickers read `@thm_*` tmux options live; the hex literals in call sites are only fallbacks for an unset option. In Latte the live values are the Latte palette, so a fix must change **which palette role** a style uses in light mode, not just a fallback hex. Mocha must stay byte-identical, so a role carries separate dark and light `(option, hex)` pairs.

Measured Latte ratios (WCAG): on base `#eff1f5`: text 7.06, subtext1 5.53, subtext0 4.37, overlay2 3.49, overlay1 2.83, overlay0 2.30, peach 2.64, maroon 3.48, red 4.80, mauve 4.79, blue 4.34, lavender 2.81, green 2.96, teal 3.31, yellow 2.31. Current selection bg surface2 `#acb0be` caps every foreground at 3.69 (text) — no fg fix works, so the selection bg changes (mantle `#e6e9ef`: text 6.57, subtext1 5.14, subtext0 4.06, overlay2 3.25, red 4.46, mauve 4.45, blue 4.04, maroon 3.23, teal 3.08).

## Design

New package `picker/palette` (stdlib only; importable by `picker`, `enrichcard`, `splash`):

- `Role{DarkOpt, DarkHex, LightOpt, LightHex}`; `Role.Opt(light)`, `Role.Hex(light)`.
- Role vars: `Text`, `Primary` (paths/names: dark subtext0 / light subtext1), `Dim` (hints, placeholder, separators-as-text: dark surface2 / light subtext0), `Faint` (overlay1 → overlay2), `Rule` (borders, rules: dark surface1 / light overlay2), `Accent` (lavender → blue), `Highlight` (peach → maroon), `Warn` (red), `Success` (green → teal), `Merged`/`Host` (mauve), `Blue`, `Muted` (overlay0 → overlay2), `LevelMed` (yellow → teal), `SelBg` (surface2 → mantle), `Base`, `Mantle`.
- `Pair{Surface, Name string; Fg, Bg Role; Floor float64}` and `LightPairs []Pair`: one table; styles consume the same `Role` vars the pairs reference.
- `Contrast(hexA, hexB) float64`.
- `palette_test.go`: every `LightPairs` pair ≥ its floor (4.5 primary, 3.0 secondary); every role's `LightHex` equals the Latte hex of its `LightOpt` (a small named Latte table in the test guards typos); every role's dark pair equals the pre-change Mocha literal (guards "Mocha unchanged").

## Steps

1. **palette package + tests** (`picker/palette/{palette.go,palette_test.go}`). Gate: `go test ./palette/`.
2. **picker views**: `thmColor`/`thmColorHex` (`tui.go`), `color`/`colorHex` (`whichkey.go`, `links.go`) take a `palette.Role`; convert every call site in `render_list.go`, `render_wall.go`, `render_frame.go`, `whichkey.go`, `links.go`; selection bg via `SelBg`. Files: those plus `tui.go`. Gate: `go test ./...` in `picker/` (golden/dark tests unchanged).
3. **row builders (ANSI strings)**: the `envOrMap("THM_*", …)` blocks in `tui.go` (session rows ~3139, window rows ~3522, 2905, 3351), `session_card.go`, `remote*.go`, `main.go` `newResourceColors`, `hostColorFunc` become role-driven (`palette.Resolve` gives env name `THM_<OPT>`, option, fallback hex per theme). `hostColorFunc` gets a picker-local light remap of host slots onto roles that clear 3:1 (peach→maroon, teal→teal, pink→maroon, yellow→teal, sky→teal); `hostcolor.Palette` (shared with the status line) untouched. Gate: `go test ./...`, mocha row tests unchanged.
4. **enrichcard + splash**: enrichcard's `cfg` hexes are overridden in light from `palette` roles (overlay0→Muted, subtext0→Primary), reading `@catppuccin_flavor` and the light option once at start; splash `accent()`/`subtext()` use `palette` roles. Do not touch `generator/`. Gate: package tests.
5. **LightPairs completeness**: enumerate every light pair from steps 2–4 (surface, role, fg, bg) in `palette.LightPairs`; a test greps nothing — completeness is by construction since styles use the roles; list the surfaces in the PR table.
6. **Status-bar audit** (no edits): small script in scratchpad measures `@thm_*` pairs from `scripts/tmux-reflow-windows.sh`, `scripts/tmux-status-fold.sh`, `picker/statusline`, window grid; failing pairs listed in the PR body.
7. **Docs**: `docs/agents/theme.md` + `docs/agents/picker.md` note the floor, roles, test.
8. **Evidence**: red/green demonstration (lower one pair, show the test fail, restore); before/after light renders (session picker with selected row, which-key, enrich card) captured as ANSI→HTML→PNG.

## Acceptance

All task-doc checkboxes; `nix build .#default`, `nix flake check`, `nix build .#lint` pass.

## Risks

- Mantle selection bg is close to base (1.07:1) — selection is carried by the `▶` marker plus bg; alternative crust fails maroon (2.97). Noted in PR.
- Host slots collapse onto 2 roles in light (palette cannot supply 5 hues ≥3:1); documented.

## Revision 1 (plan-critic: revise, all folded)

- **Floors are explicit per pair.** `Pair.Floor` is 4.5 for names, paths and the selected row's text; 3.0 for hints, separators, icons and hued values (host tints, CPU/Mem level hues, PR/agent badges), because no Latte accent reaches 4.5 on `SelBg` (best hue: red 4.46). Idle path/CPU/Mem values use a new role `Quiet` (dark overlay1 → light subtext1, ≥4.5 on base and SelBg), so Mocha keeps its overlay1. `LightPairs` is written out in code, not deferred.
- **Every fg role is paired with every bg it draws on.** Row-builder roles × {Base, SelBg} are generated by a loop over a `RowRoles` list that the row builders also read; a test asserts each role used by the list/row styles is in `RowRoles`. Chrome pairs (hints, search, borders, which-key, links, wall, enrich card, splash) are explicit.
- **Added surfaces:** `claudeColors["light"]` (main.go): waiting→maroon, done/denied→teal, compacting→blue-or-teal, as roles; dark map byte-identical. Enrich card: every cfg colour gets a light role (green→Success, peach→Highlight, blue→Accent/primary, overlay0→Muted, subtext0→Primary). The card reads `@catppuccin_flavor` plus the needed light `@thm_*` options with one `tmux show-options -g` call in `enrichcard/main.go`; `generator/` stays untouched. Splash gradient (decorative, modulated brightness) is declared exempt; `accent()/subtext()` are covered. Faint SGR 2 on stale icons and `remotebridge/daemon/park.go` (overlay0 on mantle) are listed as unmeasured/audit-only in the PR.
- **Theme source for row builders:** `palette.Resolve(opts, role)` treats light as `opts["@catppuccin_flavor"] == "latte"` with no `themestate` fallback (callers that already hold a `theme` pass it through `palette.ResolveTheme`), so empty-opts tests stay Mocha and deterministic. No signature churn beyond `colorHex`/`color`.
- **Test call sites:** `whichkey_test.go:402` and `links_test.go:227` switch to the new `colorHex(palette.SelBg)` form.
- **Mocha trap:** each `Role` keeps its call site's dark literal (`SelBg` dark = `@thm_surface_2`/`#45475a`; `Dim` dark = `#585b70`). Mocha-covering tests: render_list_test, whichkey_test, links_test, window_picker_test, wall_test, tui_test, session_card_test, remote_wins_test.
- **Host slot collapse** (breaks one-host-one-colour vs the status bar): documented in picker.md and the PR.
- **Status-bar audit** also covers generated `@thm_*` formats (`generator/render`, `config/tmux.conf.nix`) and the mirror-tab host colour.
- The red/green demonstration runs after `LightPairs` is complete.
