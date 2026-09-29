# Theme reload race — implementation plan (#871)

Spec: `docs/superpowers/specs/2026-09-29-theme-reload-race-design.md`.

Order is test-first: each test is committed-ready and shown red against the
current code before the fix that turns it green.

## Step 1 — client-theme.bats: T2, T1, T4 (red first)

File: `tests/client-theme.bats` only.

- Helper `pane_modes`: `inner list-panes -a -F '#{pane_id}|#{pane_in_mode}'`,
  and `no_pane_in_mode` = no row ends in `|1`.
- Helper `write_fake_toggle BODY`: writes `$BATS_TEST_TMPDIR/bin/theme-toggle`
  (already first on the servers' PATH via `setup`) as a `#!/bin/sh` POSIX
  script — the nix sandbox has no `/usr/bin/env`. `INNER_TMPDIR`, `TMUX_BIN`
  and the output paths are baked into the script at write time
  (`INNER_TMPDIR` is a plain, unexported variable of `theme_setup`); the fake
  talks to the inner server with `TMUX_TMPDIR=<baked> <baked TMUX_BIN> -L s`
  and never reads `$TMUX`. Its argv (`/bin/sh …/bin/theme-toggle`) matches
  the Step 2 regex.
- **T2** "a failing plugin in the reload never paints a pane": reload target
  `$HOME/.config/tmux/tmux.conf` = `run-shell "exit 1"` + `source-file
  $TMUX_CONF` + the reloads marker. Light report → `wait_for applied_is light`
  → `settle` → `no_pane_in_mode`, `thm_bg_is '#eff1f5'`.
- **T1** "report during a theme-toggle run: one reload, no overlay, catppuccin
  never fails": the fake, started in the background just before `report
  light`, loops for ~3s clearing every `@thm_*` (one `show-options -g` read,
  one `set -gu` per var, as `update_tmux` does), then `set -g
  @catppuccin_flavor latte`, then `source-file $HOME/.config/tmux/tmux.conf`
  with stdout+stderr captured to `$BATS_TEST_TMPDIR/toggle.out`, then exits.
  After the fake exits and `settle`: `no_pane_in_mode`; `toggle.out` has no
  `returned`; reloads − baseline = 1; `flavor_is latte`; `thm_bg_is
  '#eff1f5'`.
- **T4** "a failed theme-toggle reload is still recovered": the fake clears
  the palette, sets flavor latte, exits 1 (no source). Start it, `wait` for
  it, then `report light` → `wait_for 20 thm_bg_is '#eff1f5'`.
- Red evidence: run the suite against the current build (Step 2 not yet
  applied) N times: T2 fails deterministically (pane in view mode), T1 fails
  on the pane/reload-count assertions, T4 fails (`@thm_bg` stays empty with
  the flavor-only guard). Record the counts.

## Step 2 — tmux-client-theme.sh (R1, R3, R4a) + `@ps@`

Files: `scripts/tmux-client-theme.sh`, `config/tmux.conf.nix`
(`mkScriptClientTheme`).

- After sourcing lib-log: `exec >/dev/null 2>&1` with a one-line why (a `-b`
  hook job's output is painted into the reporting client's pane).
- `ps_bin="@ps@"`, falling back to `ps` when unsubstituted (lib_log pattern).
- `theme_toggle_running`: capture first, then match — no pipe, because
  under `pipefail` a `grep -q` that exits early SIGPIPEs `ps` and the
  pipeline reports 141 ("not running"): `out=$("$ps_bin" -U "$UID" -ww -o
  args= 2>/dev/null)`, then `[[ $out =~ $re ]]` with
  `re='(^|[[:space:]/])theme-toggle([[:space:]]|$)'` in a variable.
- Before the lock loop: `while theme_toggle_running && ((SECONDS <
  deadline)); do sleep 0.2; done`, sharing the existing `deadline`
  (computed once, before both waits).
- Loop read adds `#{@thm_bg}` as a fifth `|` field; guard becomes
  `[[ $flavor == "$wanted_flavor" && -n $bg ]] && break`.
- End with explicit `exit 0`.
- `mkScriptClientTheme`: add `"@ps@"` → `"${pkgs.procps}/bin/ps"`.
- `shellcheck scripts/tmux-client-theme.sh`.

## Step 3 — bridge theme verb (R2) + T3

Files: `picker/remotebridge/daemon/ctl.go`, `picker/remotebridge/daemon/ctl_test.go`.

- `themeApplyScript`: `command -v theme-toggle >/dev/null 2>&1 || exit 0;
  theme-toggle apply %s >/dev/null 2>&1; exit 0`. Update the verb comment:
  a `-t` job's output and non-zero exit land in view mode on the mirrored
  pane, which wedges it.
- T3 `TestThemeVerbNeverOverlaysPane` (skip without tmux, like its
  neighbours): `startIsolatedTmux(t, "PATH="+dir)` with **no host PATH**
  (as the test at `ctl_test.go:969` does) — the host carries a real
  theme-toggle that would flip the desktop, and would hide the absent case.
  The body needs nothing from PATH: `/bin/sh` is absolute and `command -v` is
  a builtin. The fake is `#!/bin/sh`.
  build the verb against `%0` and feed that one command line to
  `tmux source-file -` on stdin (tmux parses it exactly as it parses a line
  on the control stream), wait for the job to finish (poll ≤3s for a marker
  the fake touches, or a fixed 1s when there is no fake), assert
  `#{pane_in_mode}` is 0. Cases: no
  `theme-toggle` in `dir`; a fake `theme-toggle` that echoes and exits 1.
  Red with the old body (the no-theme-toggle case paints `returned 1`).
- Keep `TestThemeVerbBuildsSilentRemoteApply` green (no single quotes, no
  `echo`, no `split-window`/`display-message`).

## Step 4 — docs (R5)

- `docs/agents/theme.md`: the race and its fix (one toggle → one reload;
  hook defers while theme-toggle runs; guard needs a loaded palette and
  therefore reloads a never-loading palette on every report; hook output
  never reaches a pane).
- `docs/agents/scripts.md` `tmux-client-theme` row: deferral, `@ps@`,
  silenced stdout, palette guard.
- `docs/agents/bridge-daemon.md` theme fan-out row: the verb exits 0 and
  prints nothing; a remote without theme-toggle used to overlay (and wedge)
  the mirrored pane.

## Step 5 — gates

- `nix build .#default`
- `nix build .#checks.x86_64-linux.client-theme-tests`,
  `.#checks.x86_64-linux.remote-tests` (runs `remote-theme.bats`) and
  `.#checks.x86_64-linux.picker-go-tests`, then full `nix flake check`
- `nix build .#lint`

## Step 6 — commit and follow-up

- Commit the spec and this plan with the code, in the same PR.

- File a GitHub issue for `enrich-refresh`'s identical `run-shell -b -t`
  overlay/wedge exposure.
