# Client theme hooks: implementation plan (#663)

Spec: `docs/superpowers/specs/2026-09-16-client-theme-hooks-design.md` (accepted).

## Files

| File | Change |
|------|--------|
| `scripts/tmux-client-theme.sh` | new: the handler |
| `config/tmux.conf.nix` | register the script in `scriptNames` **and** `ogInternal`, and add a builder that substitutes its placeholders |
| `generator/paths/paths.go` | add `tmux-client-theme` to `RequiredScripts`, sorted |
| `config/tmux.conf.tmpl` | index-scoped hook clears plus the two setters |
| `config/tmux.conf.reference.nix` | the same lines, byte-identical output |
| `tests/client-theme.bats` | new: live scratch-server test |
| `flake.nix` | a `client-theme-tests` check that runs it |
| `CLAUDE.md` | Theme support convention, `og-remote-theme` row, new script row |

## Steps

### 1. Handler script — `scripts/tmux-client-theme.sh` (implement: sonnet)

Bash, tabs (shfmt), `set -uo pipefail`. Placeholders with the PATH fallback
idiom `og-remote-theme.sh` uses for `@bridge_ctl@`, so the raw script runs
under bats:

- `@lib_log@`: source it for `acquire_lock` and `log_event`. When the
  placeholder is unsubstituted, fall back to `$(dirname "$0")/lib-log.sh`.
- `@catppuccin@`: `catppuccin.tmux` path.
- `@bash@`: bash path, because catppuccin is run as `run-shell "<bash> <path>"`,
  exactly as `pluginRunShells` does.
- `@apply_theme_colors@`, `@remote_theme@`: store bin paths.

Logic, per spec §3:

1. `lock="${OG_CLIENT_THEME_LOCK:-${TMPDIR:-/tmp}/og-client-theme-$UID.lock}"`.
   Retry `acquire_lock "$lock"` with `sleep 0.2` until
   `OG_LOCK_STALE_SECONDS` elapses, then `exit 0`.
2. `applied=""`. Loop:
   - `IFS='|' read -r follow want flavor < <(tmux display-message -p '#{@og_follow_client_theme}|#{@og_client_theme_want}|#{@catppuccin_flavor}')`
   - `[[ $follow == off ]] && exit 0`. `case $want in light|dark) ;; *) exit 0;; esac`.
   - Read the file theme with lib-claude's regex:
     `state="${XDG_STATE_HOME:-$HOME/.local/state}/theme-state.json"`, default
     `dark`.
   - `wanted_flavor`: `latte` for light, `mocha` otherwise. When
     `file_theme == want && flavor == wanted_flavor`, break.
   - When `want == applied`, `log_event client-theme nonconverged want "$want"`
     and break.
   - Apply, then set `applied=$want`.
3. Apply, with `theme-toggle` on PATH: `theme-toggle apply "$want" >/dev/null 2>&1`.
4. Apply, local branch:
   - `mkdir -p` the state dir. `tmp=$(mktemp "$state.XXXXXX")`, then `printf` the JSON
     `{"theme":"$want","timestamp":"$(date -Iseconds)","failed":[],"version":1}`,
     then `mv -f`. On any failure remove the tmp and continue.
     `date -Iseconds` is GNU-only, so use `date +%Y-%m-%dT%H:%M:%S%z` for
     macOS portability (`tests/check-portability.sh` runs in check).
   - Clear `@thm_*` with exactly two forks. One read,
     `tmux show-options -g -F '#{option_name}'`. Then build one argv array
     `set -gu @thm_a ';' set -gu @thm_b ...` and make a single `tmux "${args[@]}"`
     call. Never fork per option.
   - `tmux set -g @catppuccin_flavor "$wanted_flavor"`.
   - `conf="$HOME/.config/tmux/tmux.conf"`. When
     `[[ -r $conf ]] && tmux source-file "$conf"` succeeds **and**
     `@thm_bg` is non-empty, the reload is done. Otherwise replay:
     `tmux run-shell "@bash@ @catppuccin@"`, `tmux run-shell "@apply_theme_colors@"`,
     `tmux run-shell -b "@remote_theme@"`.

Script header: a short WHY comment. The hook fires on every report, so the
guard lives here, along with the lock and the want re-read. A `theme-toggle`
run slower than the 60s stale window can let a second job steal the lock. That
is acceptable, because every job re-reads the newest want. Run `shellcheck`.

### 2. Build wiring (implement: sonnet)

- `config/tmux.conf.nix`: add `"tmux-client-theme"` to `scriptNames` **and** to
  `ogInternal`. It is a hook handler with no user verb, like `tmux-shell-prompt`.
  The `ogPartitionOk` assert fails evaluation otherwise. Add
  `mkScriptClientTheme` next to `mkRemoteScript`, substituting `@lib_log@`,
  `@catppuccin@` (`${catppuccin}/share/tmux-plugins/catppuccin/catppuccin.tmux`),
  `@bash@` (`${pkgs.bash}/bin/bash`), `@apply_theme_colors@` and
  `@remote_theme@` (`${script.<name>}/bin/<name>`). Add a
  `else if name == "tmux-client-theme" then mkScriptClientTheme name` branch.
- `generator/paths/paths.go`: add it to `RequiredScripts` in sorted position.

### 3. Config lines (implement: sonnet). Depends on 2.

`config/tmux.conf.tmpl`, below `set-hook -gu pane-shell-prompt` in the reload
clear block:

```
set-hook -gu 'client-light-theme[40]'
set-hook -gu 'client-dark-theme[40]'
```

After the `run-shell -b "{{index .Paths.Scripts "og-remote-theme"}}"` line, add a
comment block (spec §2–§6 in brief) and:

```
set-hook -g 'client-light-theme[40]' { set -g @og_client_theme_want light ; run-shell -b "{{index .Paths.Scripts "tmux-client-theme"}}" }
set-hook -g 'client-dark-theme[40]' { set -g @og_client_theme_want dark ; run-shell -b "{{index .Paths.Scripts "tmux-client-theme"}}" }
```

Mirror both in `config/tmux.conf.reference.nix` with
`${script.tmux-client-theme}/bin/tmux-client-theme` and 4-space indent, so
`tmux-conf-extraction-assertions` stays byte-identical.

**Verify** on a scratch server (own `TMUX_TMPDIR`, `-L`, `CLAUDE_STATUS_DIR`)
that `show-hooks -g` lists both `[40]` entries as a two-command list, and that
a re-source leaves exactly one entry per index.

### 4. Live test — `tests/client-theme.bats` (implement: sonnet). Depends on 1–3.

Pattern from `tests/pane-shell-prompt.bats`: `TMUX_BIN` is the wrapped
binary, with a private `HOME`/`XDG_*`/`CLAUDE_STATUS_DIR`/`OG_*` dir set and
`OG_CLIENT_THEME_LOCK` in `BATS_TEST_TMPDIR`. Setup:

- A **PATH filter**: drop any PATH dir holding a `theme-toggle`, so a
  developer's real toggle can never run against the test, and prepend
  `$BATS_TEST_TMPDIR/bin`.
- `~/.config/tmux/tmux.conf` as a counting wrapper:
  `run-shell "echo x >> $BATS_TEST_TMPDIR/reloads"` then
  `source-file $TMUX_CONF`. `TMUX_CONF` is passed from flake.nix.
- Seed the state file `dark`, then start the inner server with
  `new-session -d -s main`.
- An outer scratch server on the raw tmux (`OUTER_TMUX`, the unwrapped
  binary, `-f /dev/null`) with a window running
  `env -u TMUX TERM=xterm-256color $TMUX_BIN -L $SOCKET attach -t main`.
  Wait for the inner `list-clients` to show it.
- Helper `report <outer-target> light|dark`:
  `send-keys -l $'\e[?997;2n'` (light) or `;1n` (dark).

Every assertion polls with `wait_for`. The outer tmux may answer the inner
client's `\e[?996n` at attach with its own theme. After attaching, wait for
settle (lock dir absent, plus 1s), then take the reload baseline as a count
rather than assuming 0. The cases below read "reloads" as the delta from that
baseline.

**Sandbox first:** write setup plus case 1, then run that check alone
(`nix build .#checks.x86_64-linux.client-theme-tests`) before writing cases
2–7, so a pty or terminfo problem in the nix sandbox surfaces early.

Cases:

1. A light report sets flavor `latte`, `@thm_bg` `#eff1f5`,
   `@og_theme_applied` `light`, and the file `"theme": "light"`. Reloads is 1.
2. A repeat light report: after a settle wait (poll that the lock dir is gone,
   plus 1s), reloads is still 1 and the flavor is unchanged.
3. `@og_follow_client_theme off`, then a light report: flavor stays `mocha`
   and reloads is 0.
4. A fake `theme-toggle` in `$BATS_TEST_TMPDIR/bin` logs `$*`. The inner
   server is started with it on PATH. A light report puts `apply light` in the
   log, and reloads is 0 (the fake does not reload).
5. Two nested clients (two outer windows). A dark report on client A, then a
   light report on client B at once: the final flavor `latte`, `@thm_bg`
   `#eff1f5` and file `light` all agree.
6. The conf wrapper replaced by `bogus-command-xyz` (rejected), then a light
   report: `@thm_bg` is `#eff1f5`, non-empty.
7. Skipped when `id -u` is 0, or when the dir proves writable after chmod.
   The state file made unwritable (`chmod 555` on its dir after seeding) and
   the wrapper conf back to counting. A light report gives reloads exactly 1
   within the settle window, and the lock dir is gone.

The **control-client** fact stays in the spec as a measured note and is not
re-tested: its only observable is an absence.

`teardown`: kill both servers, `chmod` dirs back to writable.

### 5. Flake check (implement: sonnet). Depends on 4.

A `client-theme-tests` entry in `flake.nix` beside `osc-133-dead-agent-tests`:
`nativeBuildInputs = [bash bats coreutils gnugrep (mkTmux pkgs)]`,
`TMUX_BIN = "${tmuxConfig.tmux-wrapped}/bin/tmux"`,
`TMUX_CONF = "${tmuxConfig.tmuxConf}"`, `OUTER_TMUX` = the raw tmux bin
(`"${mkTmux pkgs}/bin/tmux"`), and `LANG`/`LC_ALL` `C.UTF-8`. Copy `scripts` and
`tests`, then run `bats tests/client-theme.bats`.

### 6. CLAUDE.md (worker)

- Theme support convention: file plus terminal reports, the precedence rule,
  the `@og_follow_client_theme off` escape hatch, and that control clients
  never fire.
- The `og-remote-theme` row: it also runs off a reported terminal theme
  change.
- A new `tmux-client-theme` row in the Script Roles table.

### 7. Gate

`nix build .#default`, then `nix flake check`, then `nix build .#lint`.
`shellcheck scripts/tmux-client-theme.sh`.

## Ordering

1 ∥ 2 → 3 → 4 → 5 → 6 → 7

## Rework (#673)

Post-merge user feedback reworked this feature along three axes; spec amended
at `docs/superpowers/specs/2026-09-16-client-theme-hooks-design.md`'s "Rework"
section, which is the authoritative description of current behavior. Summary:

1. **tmux-only apply.** `scripts/tmux-client-theme.sh` no longer ever invokes
   `theme-toggle` or writes `theme-state.json` — it only sets
   `@catppuccin_flavor`/`@thm_*` and replays the existing reload path. The
   file is now a cold-start seed only (read once at config load when
   `@catppuccin_flavor` is empty), never read again while the server is up.
2. **SSH-client rule.** A report from an ssh-attached client is ignored
   while any other attached client is local (non-ssh, non-control); an
   all-ssh/headless server's reports are followed. Authority lives in a
   tmux-stamped option (`@og_client_theme_client`, `set -gF` from
   `#{hook_client}`, stamped alongside `@og_client_theme_want` in the same
   hook brace-block) read fresh inside the handler's main loop — not in the
   job's own argv, which is only a cheap pre-lock fast path. This matters:
   argv is fixed to whichever report started a given background job and can
   be stale by the time that job's loop reaches a different, newer want: it
   must not be the authority for the current pairing between `want` and its
   reporter.
3. **Renderer consistency.** Every in-repo reader of the old
   `theme-state.json` (`lib-claude.sh`'s `setup_claude_colors`, and the Go
   `themestate.Detect()` call sites in `picker/statusline`, `picker/tui.go`,
   `picker/whichkey.go`, `picker/splash`) now prefers the live tmux
   `@catppuccin_flavor` when running under tmux, falling back to the file
   only outside tmux or before the option is set — fork-free on the
   confirmed hot 1s path (`tmux-update-icons.sh`, via a new pre-expanded `$5`
   positional threaded from `status-format[0]`, mirroring the script's
   existing `$2`/`$3`/`$4` pattern) and zero-new-fork on `tui.go`/
   `whichkey.go` (they already called `readTmuxOpts()`). The external
   `themestate` module itself was not modified — every adaptation is at the
   call site.

New/changed files beyond the original table: `tests/lib-claude-theme.bats`
(new unit suite for `setup_claude_colors`'s precedence) plus a matching
`lib-claude-theme-tests` flake check; `tests/client-theme.bats` rewritten
(no state-file assertions, two new ssh-client cases, an explicit
`@og_theme_applied`-moved assertion); `scripts/tmux-update-icons.sh` (thread
the flavor arg); `picker/main.go`/`tui.go`/`whichkey.go`/`splash/main.go`
(the renderer call-site adaptation, plus small table tests for the new pure
mapping helpers in `picker/main_test.go` and
`picker/statusline/main_test.go`).
