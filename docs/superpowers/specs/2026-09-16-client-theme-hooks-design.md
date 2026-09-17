# Follow the terminal's reported theme (#663)

## Problem

Light/dark in tmux-og is driven by the external `theme-toggle` alone. It writes
`$XDG_STATE_HOME/theme-state.json`, sets `@catppuccin_flavor`, clears `@thm_*`
and re-sources `~/.config/tmux/tmux.conf`. The re-source re-runs catppuccin,
`tmux-apply-theme-colors` and `og-remote-theme`. Nothing reacts when the
terminal itself changes theme. That covers a kitty or ghostty following the OS
at sunset. It also covers a light terminal ssh-ing into a headless host, which
has no `theme-toggle` and so always renders mocha.

tmux 3.6+ reports the terminal's theme to hooks. This spec wires them up.

## Measured facts (pinned tmux e880cf6, `next-3.9`)

Source is `server-client.c` `server_client_report_theme` and
`tty.c` `tty_start_tty`. Every bullet below was observed on a scratch server
with a nested real client. The synthetic report was
`send-keys -l $'\e[?997;2n'` into the outer pane.

- **Trigger.** A real client subscribes with `\e[?2031h\e[?996n` at tty start
  (for `TERM_VT100LIKE` terminals). The terminal answers `\e[?997;1n` (dark)
  or `\e[?997;2n` (light) once at attach, then again on every theme change.
  `tty-keys.c` turns the answer into `KEYC_REPORT_{DARK,LIGHT}_THEME`.
- **The hook fires on every report, not only on a change.** Sending the same
  light report twice fired `client-light-theme` twice. The change check in
  `server_client_report_theme` gates only tmux's own redraw. A consumer that
  must be cheap on a repeat needs its own guard.
- **Per client.** Two real clients reporting different themes each fire only
  for themselves. `#{client_theme}` is per client (`light`/`dark`, empty while
  unknown) and `#{hook_client}` names the reporter.
- **Control-mode clients never fire.** They have no tty, so there is no
  `tty_start` and no key parsing. Their stdin is parsed as commands: piping
  `\e[?997;2n` into `tmux -C attach` gives `parse error: unknown command`, fires
  nothing and leaves `client_theme` empty. So a remote-bridge attach cannot flip
  the theme, and no filter is needed.
- **A terminal that never answers never fires.** Behaviour there is exactly
  today's.
- **Version.** Both hooks exist on tmux 3.7c (`show-hooks -g`). On a server
  that lacks a hook, `set-hook` fails at *execution* with `invalid option`. It
  is not a parse error, so the rest of a sourced file still runs (checked on
  3.7c with an unknown hook name, where later `set` lines still applied). The
  string-body `if-shell` guard that `floatNewPaneGuard` needs for an unknown
  *flag* is therefore unnecessary here.

## Rework (implemented after the initial version below): tmux-only apply + ssh-client rule

The design below (§1–§6) is the ORIGINAL accepted design and is left intact as
the historical record. Two decisions changed after initial implementation,
both driven by direct user feedback on the first version of this feature:

1. **The handler never applies through `theme-toggle`, and never writes
   `theme-state.json`.** §1's "Feed the file. Yes." decision is REVERSED: a
   terminal report now converges tmux state alone (`@catppuccin_flavor`,
   `@thm_*`) and leaves the file untouched. `theme-toggle` remains the file's
   only writer, and is not invoked by this handler under any circumstance
   (not "when absent falls back to tmux-only" — ALWAYS tmux-only now,
   `theme-toggle`'s presence on PATH is irrelevant to this handler). The file
   is read exactly once per server lifetime, as a **cold-start seed** for
   `@catppuccin_flavor` when the option is still empty at config load (see
   `config/tmux.conf.reference.nix`'s `pluginConfigs`) — never again while the
   server is up.

   This inverts §1's rationale directly: "Replace the file? No... Feed the
   file. Yes." is now "Feed the file? No — only `theme-toggle` feeds it. Adapt
   the readers instead" (see the new "Renderer consistency" bullet below,
   which is what §1's own "Out of scope / follow-ups" bullet anticipated and
   deferred — it is now in scope and shipped).

2. **SSH-client rule (new, §2 addendum).** A report from a client that
   attached over ssh (`#{I/e:SSH_CONNECTION}` non-empty for that client, the
   same per-client interrogation `tmux-splash-maybe.sh`/#649 established) is
   IGNORED while at least one other attached, non-control-mode client is NOT
   ssh; when every attached non-control client is ssh (a headless server),
   ssh reports ARE followed. Mechanism: the hook body stamps
   `@og_client_theme_client` (via `set -gF`, format-expanded from
   `#{hook_client}` — `set -g` alone does NOT expand a format value, confirmed
   against `cmd-set-option.c`'s `args_has(args, 'F')` gate) synchronously
   alongside `@og_client_theme_want`, in the same brace-block command list —
   the handler's main loop reads both fresh every iteration and gates
   authoritatively on the freshly-paired reporter. The job's own argv (also
   `#{hook_client}`, passed as a `run-shell` argument) is only a cheap
   pre-lock fast path, never the authority, since argv is fixed to whichever
   report started that particular background job and can be stale by the
   time the loop reaches a newer want.

3. **Renderer consistency (new scope).** Because a terminal report can now
   leave tmux state and `theme-state.json` disagreeing (by design — see
   point 1), every in-repo reader was adapted to prefer the LIVE tmux flavor
   when running under tmux, falling back to the file only outside tmux or
   before `@catppuccin_flavor` is set. `lib-claude.sh`'s `setup_claude_colors`
   takes an optional pre-expanded-flavor argument (threaded fork-free through
   the hot `tmux-update-icons.sh` 1s path and the Go statusline's `--flavor`
   CLI flag), else falls back to one `show-options` fork when `$TMUX` is set,
   else the file. The Go `themestate.Detect()` call sites in `picker/tui.go`
   and `picker/whichkey.go` derive theme from their already-fetched
   `readTmuxOpts()` map (zero new forks); `picker/splash/main.go` (a separate
   binary) does its own one-time `show-options` fork. The external
   `themestate` module itself is unmodified — every adaptation is at the call
   site.

See `tests/client-theme.bats` and `tests/lib-claude-theme.bats` for the live
and unit proof of all three points, and `CLAUDE.md`'s `tmux-client-theme` row
and "Theme support" bullet for the shipped-behavior summary.

## Design (original)

### 1. Precedence: the most recent explicit signal wins, and every signal converges the file and tmux

There are two kinds of signal: a terminal theme report and a `theme-toggle`
run. Both are deliberate. A report fires only at attach or when the terminal's
theme changes. After either signal, `theme-state.json`, `@catppuccin_flavor`
and `@thm_*` must agree. The shell reader (`lib-claude.sh`) and the Go readers
(`themestate.Detect`) read the file while tmux renders the flavor, so a
disagreement paints dark statusline segments on a latte bar.

**Superseded by the Rework section above** — a report no longer feeds the
file at all; only `theme-toggle` does, and the readers adapt instead. Kept
here for the historical rationale that motivated the original approach.

- **Replace the file?** No. Five readers, the external `themestate` module and
  `theme-toggle` itself all depend on it. That rewrite is out of scope.
- **Trigger a re-application only?** No. That leaves the file and the palette
  disagreeing whenever the report differs from the file.
- **Feed the file.** Yes. A report that differs from the current state is
  applied through `theme-toggle apply <theme>` when it is on PATH. That tool
  owns the file, re-themes fish/bat/delta and already runs the tmux reload.
  Where it is absent (a headless host), tmux-og applies the tmux half itself
  and writes the file in theme-toggle's schema
  (`{theme, timestamp, failed: [], version: 1}`, atomic tmp+mv).

Loop safety: `theme-toggle` changing kitty's colours makes kitty report the
theme that was just applied. The guard in §3 sees the file already agree and
exits.

**Escape hatch.** A terminal pinned to a theme opposite the desktop's would
revert the desktop on every attach. `set -g @og_follow_client_theme off` (for
example in `extraConfig`) disables the handler at runtime. Any other value,
including unset, leaves it on. A tmux user option instead of a Nix option: the
decision is per-server and live-toggleable, and it avoids plumbing through
`home-manager.nix`, `config.toml`, the generator, `initcfg` and the reference
nix for a value read only on a rare event.

### 2. Server-global vs per-client: last report wins

`@catppuccin_flavor` and `@thm_*` are server-global, and tmux has no per-client
scope for user options. The most recent report wins. Reports are events, not a
steady state, so two attached clients that disagree do not oscillate. The one
that last attached or last changed theme holds. "First client wins" was
rejected: it would ignore a real theme change on the only terminal the user is
looking at.

### 3. What the hook runs

A new script, `tmux-client-theme` (no arguments; it reads `@og_client_theme_want`), backgrounded:

```
set-hook -g 'client-light-theme[40]' { set -g @og_client_theme_want light ; run-shell -b "<tmux-client-theme>" }
set-hook -g 'client-dark-theme[40]'  { set -g @og_client_theme_want dark ; run-shell -b "<tmux-client-theme>" }
```

The theme is a literal per hook, never `#{client_theme}`, so no format value
reaches a shell. The body must be a `{ … ; … }` block. The same two commands
as a quoted value joined by `\;` get re-split into too many `set-hook`
arguments, and the hook is silently never set (measured during
implementation).

**Serialization.** The hook stamps `@og_client_theme_want` *synchronously*, on
the server's command queue, so the stamps land in report order. The
backgrounded jobs are then free to start in any order. A job never trusts an
argument. Under a lock it re-reads the newest want, so "last report wins"
means the report that arrived last, not the job that finished last. Any
number of queued jobs collapse to one apply per distinct want.

The script:

1. Takes the lock with `acquire_lock` (`lib-log.sh`, a portable mkdir lock),
   at `${TMPDIR:-/tmp}/og-client-theme-$UID.lock`: per user, because the state
   file is per user. `acquire_lock` does not block, so the job retries with a
   short sleep, bounded by `OG_LOCK_STALE_SECONDS`, and exits quietly when the
   bound runs out. The holder's loop (step 2) re-reads the want, so a
   report whose job gave up is still applied if it was the newest.
2. Under the lock, loops:
   1. Reads `#{@og_follow_client_theme}|#{@og_client_theme_want}|#{@catppuccin_flavor}`
      in one `display-message -p`. Exits when the option is `off` or the want
      is not `light`/`dark`.
   2. Reads the file's theme with the same fork-free regex `lib-claude.sh` uses
      (default `dark`). **No-op guard:** leaves the loop when the file theme
      equals the want and the flavor already matches (`latte` ↔ light, else
      mocha). A repeat report costs one `tmux` fork and no reload.
   3. **Bound.** Leaves the loop when the want equals the last want *this job*
      applied, even if the guard fails. An apply that did not converge (the
      file cannot be written, theme-toggle failed partway, the reload was
      rejected) is logged once through `log_event` and never retried in a
      loop. The next report or toggle retries it. The loop therefore runs at
      most once per distinct want the job observes.
   4. Applies the want (step 3 or 4), records it as applied, then loops again.
      A want that changed while the apply ran is then applied.
3. When `theme-toggle` is on PATH, runs `theme-toggle apply <want>` (not
   `exec`: the lock is held until the loop ends). Its `update_tmux` clears
   `@thm_*`, sets the flavor and re-sources the config, and it carries its own
   source-failure replay.
4. Otherwise:
   1. Writes the file: `mkdir -p` the state dir, then an atomic tmp+mv with
      its own `mktemp` name rather than theme-toggle's fixed `.tmp`. A failed
      write is not fatal. The tmux half still applies, and the bound in 2.3
      stops the loop.
   2. Clears every `@thm_*`. catppuccin sets them with `-ogq`, so a second
      flavor never loads over the first.
   3. Sets `@catppuccin_flavor`.
   4. Runs `source-file ~/.config/tmux/tmux.conf`, the path `prefix + r`
      already binds.
   5. **Recovery.** Replays the palette directly when that file is missing (the
      wrapper used without the HM module), when `source-file` exits non-zero
      (#407: a live server rejecting a newer config, which is exactly when
      a hook from the older config runs), or when `@thm_bg` is still empty
      afterwards. The replay is `run-shell` of the catppuccin plugin, then
      `tmux-apply-theme-colors`, then `og-remote-theme`. All three are store
      paths baked into the script at build time, which is the equivalent of
      the replay theme-toggle's `update_tmux` greps out of the config.
      Palette, borders and mirrors still follow. Only tmux-fingers keeps its
      previous snapshot until the next successful reload. A theme report must
      never leave the bar colorless.

Residual, stated plainly: a manual `theme-toggle` run does not take this lock,
so it can interleave with a report's apply. Both converge to their own theme
through the same reload, and the next report or toggle settles it.
`theme-toggle` is external, and adding a lock there is a follow-up.

Both branches end in the one existing reload path, the config re-source. That
re-source re-runs catppuccin, `tmux-apply-theme-colors` and `og-remote-theme`.
`og-remote-theme`'s `@og_theme_applied` stamp still makes a same-flavor reload
free, and a flavor change fans out to every mirror as today.
`tmux-apply-theme-colors.sh` is not edited (#669 owns its format string).

### 4. Remote bridge

This needs no change, because control-mode clients never fire (measured
above). The existing fan-out still carries a local change to mirrors. A
reported change on the laptop re-sources, the re-source runs
`og-remote-theme`, and that sends the `theme` ctl verb.

### 5. Version guard and reload hygiene

No `if-shell` guard (see Measured facts). The reload-idempotence block gets
index-scoped clears, `set-hook -gu 'client-light-theme[40]'` and the dark
twin, above the setters. They are scoped to `[40]` rather than bare: nothing
else in tmux-og uses these hooks, so a bare clear would only wipe a user's own
consumer on another index.

### 6. Index slot

`[40]`. tmux-remux wires `[99]`, and tmux-og's other hooks use
`[0]`/`[10]`/`[20]`/`[50]`/`[60]` on other events. `[40]` leaves room on both
sides.

## Out of scope / follow-ups

- The bridge `theme` verb still needs `theme-toggle` on the remote (#545).
  Falling back to `tmux-client-theme`'s tmux-only branch there would theme
  headless mirrors too. That is a separate change.
- `theme-toggle` could take the same lock so a manual toggle serializes with
  report applies.
- ~~`themestate` and `lib-claude.sh` could read `@catppuccin_flavor` instead
  of the file, which would retire the file write. That touches the external
  module.~~ **Shipped in the Rework above** — the file write is retired for
  every terminal-report path (never touches it), and `lib-claude.sh` plus
  the Go `themestate.Detect()` call sites now prefer the live flavor. The
  external `themestate` module itself was not touched, only its call sites.

## Acceptance

**Superseded by the Rework section's behavior — see `tests/client-theme.bats`
and `tests/lib-claude-theme.bats` for the current, shipped test list. The
bullets below are the ORIGINAL acceptance criteria and are kept for
historical reference; the state-file assertions and the `theme-toggle`-apply
assertion no longer hold post-rework.**

- The bats test runs on a scratch server with the wrapped config. It uses a
  nested real client and a synthetic `\e[?997;2n` report, and asserts the
  flavor becomes `latte`, `@thm_bg` is the latte base, `@og_theme_applied`
  becomes `light` and the file says `light`. It then asserts that a repeat
  report does not re-source the config (a counting wrapper at
  `~/.config/tmux/tmux.conf`). It also asserts `@og_follow_client_theme off`
  suppresses the handler, and that a fake `theme-toggle` on PATH receives
  `apply light` in place of the local reload.
- Two opposite reports fired back to back (light then dark, from two nested
  clients) end with flavor, `@thm_bg` and the file all agreeing on the
  last one (dark).
- With `~/.config/tmux/tmux.conf` replaced by a file the server rejects, a
  report still ends with a non-empty `@thm_bg` of the reported flavor (the
  recovery replay).
- With an unwritable state file, one report produces exactly one reload
  (counting wrapper) and the job exits (the lock dir is gone afterwards).
- `nix build .#default`, `nix flake check` and `nix build .#lint` all pass.
- CLAUDE.md is updated: the Theme support convention, the `og-remote-theme` row

### Acceptance (rework, current)

- A report applies flavor + triggers exactly one reload, never creates or
  modifies `theme-state.json`, and never invokes a fake `theme-toggle`
  planted on PATH.
- A repeat report is a no-op (reload count unchanged).
- `@og_follow_client_theme off` suppresses the handler.
- An ssh-client report is ignored while a local (non-ssh) client is
  attached, and followed when only ssh clients are attached to the server.
- `@og_theme_applied` moves (not just ends at the target) after an applied
  report, proving `og-remote-theme`'s mirror fan-out stamp is still driven
  by real reports.
- Renderer consistency: a state file saying dark while the live tmux flavor
  is latte makes `lib-claude.sh`'s `setup_claude_colors` pick latte colors
  (and the equivalent for the Go `themestate.Detect()` call sites, via unit
  tests on their pure mapping helpers).
- `nix build .#default`, `nix flake check` and `nix build .#lint` all pass;
  CI green on `x86_64-linux`, `aarch64-darwin`, and lint.
  and a `tmux-client-theme` row.
