# Float border identity (#1218)

Every float (`#{pane_floating_flag}`) draws its own border label and a dedicated
accent, independent of the window's `@crew_color`/`@crew_role_color`/`@bridge_crew_*`
and of a window-level dispatcher `pane-border-format`/`-style`.

## Measured facts (pinned next-3.9, scratch server)

- `new-pane` has `-S`/`-R` style flags, but **no `after-new-pane` hook** exists.
- Scope precedence is pane > window > global, so a global-only fix cannot reach a float in a
  window with a window-level override (rule 4).
- `set -p pane-border-format '#{E:@og_float_fmt}'` stores the literal indirection; a pane-level
  stamp therefore follows theme switches with no refresh.
- `window-layout-changed` fires on float create, `break-pane -W` and `join-pane`.
  A global `window-layout-changed[50]` hook running `run-shell -C "#{P:...}"` stamped
  new floats and unstamped joined (tiled) panes, with no fork.

## Design (chosen over "stamp in every float creator")

Two approaches considered:

1. Stamp pane-level options in each creator (`mkFloat`, `tmux-crew-dash`, `remotepick.go`,
   `ctl.go`, `floatgeom.go`, the `@` toggle). Rejected: six writers, a hand-made or
   mouse-created float is missed, and the `@` toggle needs a matching unstamp.
2. **One hook** stamps every float and unstamps every tile (chosen). Covers every creator and
   the `@`-toggle / `tmux-float-group` round trips by construction.

Pieces:

- `palette.StatusFloat` role `float` (Mocha `@thm_teal` `#94e2d5`, Latte `@thm_teal` `#179299`),
  added to `StatusRoles` and `LightPairs`; stamped as `@og_c_float` by the apply script.
  Teal is distinct from the mauve active border and from the green `●`.
- `tmux-apply-theme-colors.sh`:
  - A NEW first branch `FLOAT2_BRANCH` keyed on `#{pane_floating_flag}`: label =
    `#{?@pane_label,#{@pane_label},#{?@bridge_proc,#{@bridge_proc},#{pane_current_command}}}`,
    accent (`og_float`) when active, `og_dim` otherwise. The existing `@pane_label` branch
    (`FLOAT_BRANCH`, mauve, `ROLE_COLOR_CHAIN_*` border) stays UNCHANGED as the second branch:
    the tiled `crew-dash-side` pane (`split-window` + `@pane_label`, `tmux-crew-dash.sh:177`) and
    bats tests 115/121 keep rendering exactly as today (rule 3).
  - Styles gain a float arm keyed on `pane_floating_flag` only: active fg = `og_float`,
    inactive fg = `og_dim`, ahead of the aeye/`ROLE_COLOR_CHAIN_*` arms.
  - `og_float=$(tmux show -gv @og_c_float)`, falling back to `$thm_teal` (mirrors the `og_dim`
    fallback; the bats run uses the raw script copy where role stamping is skipped).
  - Publishes globals `@og_float_fmt` (the float format) and `@og_float_astyle`/`@og_float_style`
    (full `bg=..,fg=..` strings, colours interpolated at apply time). The pane stamp is the literal
    `#{E:@og_float_*}`, so a theme switch (apply script re-run) refreshes stamped panes.
- `config/tmux.conf.tmpl` hook `window-layout-changed[60]` (index 60: `tests/dwindle.bats`
  installs a probe on [50]); exact text, one line, gate first:
  ```
  set-hook -g window-layout-changed[60] { if -F '#{||:#{P:#{?#{&&:#{pane_floating_flag},#{!:#{@og_float_stamped}}},x,}},#{P:#{?#{&&:#{!:#{pane_floating_flag}},#{@og_float_stamped}},x,}}}' { run-shell -C "#{P:#{?#{&&:#{pane_floating_flag},#{!:#{@og_float_stamped}}},set -p -t #{pane_id} @og_float_stamped 1 \; set -p -t #{pane_id} pane-border-style '##{E:@og_float_style#}' \; set -p -t #{pane_id} pane-active-border-style '##{E:@og_float_astyle#}' \; #{?pane_modal_flag,,set -p -t #{pane_id} pane-border-format '##{E:@og_float_fmt#}' \; },}#{?#{&&:#{!:#{pane_floating_flag}},#{@og_float_stamped}},set -pu -t #{pane_id} @og_float_stamped \; set -pu -t #{pane_id} pane-border-style \; set -pu -t #{pane_id} pane-active-border-style \; set -pu -t #{pane_id} pane-border-format \; ,}}" } }
  ```
  Escaping (`##{E:...}`, `#{q:}` where needed) is settled by the stored-literal bats assertion
  below, not by assumption; the executor adjusts the quoting until `show -pv` returns the literal
  `#{E:@og_float_fmt}`. A mirror window's float is stamped too (daemon writes no border options).
  Stamp loss: if `@og_float_stamped` survives (remux) without its options, the gate skips the
  float; the stamp writes the marker FIRST in the same list, so a partial state needs a
  mid-list failure and the global format still gives the float branch (a window override aside).
  Documented as a leftover.
- `config/tmux.conf.reference.nix` changes with the template.

## File list

- `picker/palette/palette.go`, `picker/palette/palette_test.go` — add `float` role, pinned tables.
- `scripts/tmux-apply-theme-colors.sh` — float branch, float styles, `@og_float_*` globals.
- `config/tmux.conf.tmpl`, `config/tmux.conf.reference.nix` — hook `[60]`, placed after the bare `set-hook -gu window-layout-changed` (tmpl ~line 496), next to the `[40]` hook.
- `tests/pane-border-format.bats` — new cases; reads the hook with
  `grep '^set-hook -g window-layout-changed\[60\]' config/tmux.conf.tmpl` (as
  `tests/status-autofold.bats:275` does).
- `tests/status-colors.bats` — add `[float]=teal` to `MOCHA_OPT`; the latte case asserts `og_c_float` equals `HEX[teal]` (its role-count assertion otherwise fails with the 11th role).
- `flake.nix` — `pane-border-format-tests`: `mkdir config; cp ${./config/tmux.conf.tmpl} config/tmux.conf.tmpl`
  (pattern at flake.nix:2778) and add `pkgs.gnugrep pkgs.gnused` to `nativeBuildInputs`.
  The template contains `{{...}}` placeholders; the test substitutes none (the hook line has none).
- `docs/agents/status-bar.md`, `docs/agents/floats.md`, `docs/agents/theme.md` (role list),
  `docs/agents/scripts.md` (apply-script row), `docs/agents/performance.md` (layout-changed gate list).

## Consumer map (shared contract: pane-level border stamp + `@og_float_*` globals)

| consumer | disposition |
| --- | --- |
| dispatcher pane-level overrides on tiles | untouched unless the pane floated and re-tiled: stamp overwrites and unstamp unsets its pane-level border options (accepted leftover; re-decorate restores) |
| dispatcher window-level override | beaten by the pane-level stamp on floats only (rule 4); tiles still show it |
| popup-floats (`display-popup`, modal) | keep pane-level `#{pane_title}` format (format arm skipped when `pane_modal_flag`); get the float styles |
| `tmux-float-group` hide/show | stash join tiles the pane -> layout-changed unstamps; `break-pane -W` restore -> restamps |
| `@` toggle (`break-pane -W` / `join-pane`) | stamp / unstamp by the same hook |
| plain `break-pane -s <float>` / `prefix + !` | fires `window-linked` only, so a second hook `window-linked[60]` (same body) unstamps |
| `^o` remotepick float (modal, not a popup) | format arm skipped like popups (`pane_modal_flag`), so under a window-level dispatcher format it keeps that label: known limit, colour still float accent |
| `tests/status-colors.bats` (role count) | changed: `[float]=teal` added |
| tiled `crew-dash-side` (`@pane_label`, tiled) | unchanged: existing `@pane_label` branch kept (rule 3) |
| mirror windows / `@bridge_pane` floats | stamped (daemon writes no border options); label uses `@bridge_proc` |
| tmux-remux persistence | never persists floats; a marker surviving without its options is not re-stamped |
| `tmux.conf.reference.nix` | changes with the template; proven by the reference diff check |

## Steps

- [ ] **Step 0: measure style scope.** On a scratch server (own socket/state dir) with the bare
  pinned tmux: set a pane-level `pane-active-border-style '#{E:@opt}'` on a float inside a window
  that has a window-level style, draw with a pty client, `capture-pane -e` the float border row;
  confirm the float colour wins. Already measured: stored value is the literal and
  `display-message '#{E:@opt}'` expands. If the drawn style does not take effect, stop and report
  (task rule 4).
- [ ] **Step 1: failing tests (palette).** `picker/palette/palette_test.go`: add `float` to the
  pinned tables `darkLiterals`, the `old` map in `TestStatusDarkLiterals`, and `oldLatte`.
  Proof: `cd picker && go test ./palette/` red (role missing).
- [ ] **Step 2: palette role.** Add `StatusFloat` to `palette.go` + `StatusRoles`; LightPairs entry
  derives from the table; update `tests/status-colors.bats` (`MOCHA_OPT`, latte case). Proof: `go test ./palette/` green (Latte teal on base >= 3:1) and `bats tests/status-colors.bats` green (it needs the built apply script, so run it as `nix build .#checks.x86_64-linux.status-colors-tests`).
- [ ] **Step 3: failing bats cases** in `tests/pane-border-format.bats`: (a) labelled float in a
  window with `@crew_color` + `@window_has_agent` renders accent not crew colour (via
  `display-message -p` of the style / `capture-pane -e` drawn border); (b) unlabelled float renders
  `━━ <command> ━━`; mirror float uses `@bridge_proc`; (c) float in a window with window-level
  `pane-border-format`/`-style` still gets float decoration after the hook runs; (d) `join-pane`
  of the float leaves the tile without stamp and the window override wins again; popup float
  keeps `#{pane_title}`; (e) a TILED pane with `@pane_label` still renders `━━ <label> ━━` with
  the crew colour in its style; (f) `show -pv -t $float pane-border-format` equals the literal
  `#{E:@og_float_fmt}`; after re-running the apply script with a different `@thm_teal` the float's
  resolved style reports the new hex; a command change changes the rendered label. Existing tiled
  cases untouched. Proof: `bats tests/pane-border-format.bats` red for (a)-(f) for the right
  reason (assertion, not setup error: check each fails before Steps 4-5).
- [ ] **Step 4: apply script.** Implement float branch, styles, globals. Proof: cases (a),(b) green,
  all old cases still green.
- [ ] **Step 5: hook.** Add the `[60]` hook above to the template and reference.nix; wire
  `flake.nix` (file list). Proof: cases (c)-(f) green; the reference-diff check and conf
  assertions inside `nix flake check` pass.
- [ ] **Step 6: docs.** Update status-bar.md "Pane border labels" (float branch first, label rule,
  accent, hook stamp, dispatcher override), floats.md (stamp, `@og_float_stamped`, hook, unstamp on
  tile), theme.md (`float` role). Proof: `rg` shows the three docs mention `@og_float_stamped`.
- [ ] **Step 7: gates.** `nix build .#default`, `nix flake check`, `nix build .#lint`.
- [ ] **Step 8: manual evidence.** Scratch server on its own socket/state dir, pty client, windows
  with `@crew_color`; open a `prefix + g`-style and a `crew dash`-style float; `capture-pane -e` the
  border rows showing the teal accent; paste into the PR.

## Acceptance map

| item | evidence |
| --- | --- |
| bats cases a-d + old cases | `bats tests/pane-border-format.bats` (Steps 3-5) |
| docs updated | Step 6 |
| `nix build .#default` / `nix flake check` / `nix build .#lint` | Step 7 |
| manual capture | Step 8, pasted in PR |

## Risks / limits stated up front

- One frame of old decoration between float creation and the hook (same-tick; not measurable in a
  settled capture).
- A pane-level `pane-border-format` a dispatcher sets on a float itself is overwritten; a pane-level
  one on a *tile* is never touched.
- Performance: the gate is an in-process format expansion; `performance.md` hook-fork budget is
  unchanged (no new fork on the common path).
