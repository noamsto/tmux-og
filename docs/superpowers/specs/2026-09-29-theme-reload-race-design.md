# Theme reload race and the `catppuccin.tmux returned 1` overlay (#871)

## Problem

A mirror session showed a view-mode overlay reading
`'<bash-interactive> …/catppuccin/catppuccin.tmux' returned 1` over a pane. Two
defects combine: something makes the catppuccin plugin exit 1 during a theme
change, and that failure's message is painted into a pane instead of going
nowhere.

## Root cause (measured)

### Why `catppuccin.tmux` exits 1

`catppuccin.tmux` is two `tmux source` calls; its exit status is the second
one's (`catppuccin_tmux.conf`). That file loads the palette
(`source -F themes/catppuccin_<flavor>_tmux.conf`, `set -ogq @thm_*`), then 15
more `source -F status/*.conf`, then sets styles from the palette:
`set -gF message-style "fg=#{@thm_teal},bg=…"`, `menu-selected-style`,
`status-style`, …

- With `@thm_*` unset, those lines expand to `fg=,bg=default,…` and tmux
  rejects them: `invalid style: fg=,bg=default,align=centre`, rc=1 (measured on
  a next-3.9 scratch server). One failed command makes `tmux source` exit 1.
- The palette can be emptied mid-source: a `source-file` issued by a command
  client reads every file **through that client** (`file.c` `file_read`: only
  `c == NULL || CLIENT_ATTACHED` reads server-side), so each of the 17 nested
  `source -F` is an async round trip, and other clients' commands run in the
  gaps.
- Two writers clear the palette (`set -ogq` never overwrites, so each clears
  `@thm_*` before loading a new flavor), and **one desktop toggle runs both at
  once**:
  1. `theme-toggle`'s `update_tmux` (nix-config, `theme-apply.sh:638`): a
     per-variable `set -gu` loop, then `set @catppuccin_flavor`, then
     `source-file ~/.config/tmux/tmux.conf`.
  2. The `client-*-theme` hook's `tmux-client-theme` (this repo): one batched
     clear, `set @catppuccin_flavor`, `source-file` the same config.

  `theme-toggle apply` runs `update_kitty` and `update_tmux` in parallel
  (`_run_updates_parallel`, `&` + `wait`), and kitty's theme report is what
  fires the hook — the live server has `@og_client_theme_want` stamped by
  `/dev/pts/4` (`xterm-kitty`), so kitty reports DSR 997 today. Each reload's
  clear can land inside the other's catppuccin source.
- Repro: a scratch wrapped server, a clear burst concurrent with
  `tmux source-file <generated conf>` → 5 of 10 runs printed
  `'<bash-interactive> …/catppuccin.tmux' returned 1` — the exact overlay text —
  and some also `'…/tmux-fingers.tmux' returned 1` (fingers' hint styles come
  from the same palette).

### Why the failure lands on a pane, and which one

- `run-shell` (sync) prints its `'…' returned N` line through the issuing
  client: to a command client's **stdout** when it has no session
  (`server_client_print`).
- `tmux-client-theme` runs `tmux source-file "$conf" 2>/dev/null` and, on
  recovery, `tmux run-shell "@bash@ @catppuccin@"` — stdout not redirected. So
  the line becomes `tmux-client-theme`'s stdout.
- `tmux-client-theme` is itself a `run-shell -b` job of the hook, with no `-t`.
  A `-b` job's output goes to `cmd_run_shell_print` → the target client's
  session's active pane, in view mode (`cmd-run-shell.c:79`). The target client
  is the reporting terminal client.
- So the overlay was **local**: painted on the active pane of the reporting
  client's session, which was the mirror session `halo-toddl` — i.e. a local
  renderer pane. `@bash@` in `tmux-client-theme` resolves to the same
  `bash-interactive-5.3p15` path as the config's line 31, so both of its reload
  steps produce exactly the observed text.
- theme-toggle's own reload cannot be the painter: it captures `source-file`
  output (`src_err=$(… 2>&1)`), and its fallback replay prints the plugin path
  with no bash prefix.

### A second, remote path to the same overlay

The bridge `theme` verb (`ctl.go`) runs on the remote
`run-shell -b -t <mirrored pane> 'exec /bin/sh -c "command -v theme-toggle … &&
exec theme-toggle apply <t>"'`. With `-t`, anything the job prints **and its
non-zero exit** land in view mode on that remote pane. Measured on a scratch
server: with no `theme-toggle` on PATH the `&&` list exits 1 and the pane shows
`'exec /bin/sh -c …' returned 1` in view mode. theme-toggle's own failures do
the same (its fallback `run-shell "$plugin"` prints to stdout; `apply` can echo
and exit non-zero). `themeProbeCmd`'s comment records that a remote view-mode
overlay on a mirrored pane wedges the mirror (`%pause`/`%continue` never fires
again). A headless remote without theme-toggle — the case #545 calls "silent
per call" — gets one on every toggle.

## Requirements

- **R1 — no overlay from a theme reload this repo owns.** Nothing
  `tmux-client-theme` or anything it runs prints may reach a pane, and the job
  always exits 0 (a `-b` job's own non-zero exit prints `returned N` into the
  same pane, whatever its stdout).
- **R2 — the bridge theme verb is silent and succeeds.** The remote job emits
  nothing and exits 0 whatever theme-toggle does, including when it is absent.
- **R3 — one desktop toggle, one tmux reload.** `tmux-client-theme` must not
  reload while a `theme-toggle` run is in flight; it waits for it (bounded),
  then re-reads its no-op guard. In the kitty-echo flow theme-toggle has already
  converged tmux, so the hook becomes a no-op and the two reloads never overlap.
- **R4 — a report with no theme-toggle running still applies** exactly as
  today (#663 behaviour unchanged: ssh gating, `@og_follow_client_theme`,
  last-want-wins, recovery).
- **R4a — the no-op guard needs a loaded palette.** Deferring makes the hook
  run after theme-toggle, so a theme-toggle whose reload failed (its
  `update_tmux` sets the flavor before sourcing, and its failure path returns
  with `@thm_bg` empty) must not satisfy the guard. The guard becomes "flavor
  is the wanted one **and** `@thm_bg` is non-empty", read in the same
  `display-message` — the same test theme-toggle's own early exit uses. A
  palette that never loads therefore reloads on every report (bounded by the
  lock and the nonconverged break); `theme.md` records it.
- **R5 — docs**: `theme.md`, the `tmux-client-theme` row in `scripts.md`, and
  the theme fan-out row in `bridge-daemon.md`.

## Design

**R3 — defer to an in-flight theme-toggle (chosen).** Before taking its lock,
`tmux-client-theme` waits while a `theme-toggle` process of this user is alive,
bounded by the existing `OG_LOCK_STALE_SECONDS` deadline, then runs its loop as
today. theme-toggle's `update_tmux` finishes before the process exits (it is
awaited inside `_run_updates_parallel`), so the hook then sees the wanted
flavor with a loaded palette and does nothing (R4a covers a failed one).
Detection lists this user's processes with `ps -U "$UID" -ww -o args=` (`-U` is
the real-uid filter in both procps-ng and BSD `ps`; `-ww` keeps a long store
path from being truncated) and matches only when the executable is
`theme-toggle` or `theme-apply-macos` (nix-config ships it under that name on
macOS), either as argv[0] or as the script argument of a sh-family interpreter
with optional flags (`bash -e <path>/theme-toggle …`), because a shebang
script's argv is `<interpreter> <path>/<name> …` while its process name is not
portable. A command that merely names it (`rg theme-toggle`) does not match.
`ps` is pinned by store path (`@ps@` → `${pkgs.procps}/bin/ps`, substituted in
`mkScriptClientTheme` like `@bash@`): the server's PATH is not guaranteed to
carry one, and nixpkgs' darwin `procps` ships `ps` but no `pgrep`. The wait sits
*before* the lock so a future theme-toggle that takes the same lock cannot
deadlock against a hook job waiting on it.

- Rejected: a fixed settle delay before applying — timing, not ordering; a slow
  theme-toggle still overlaps.
- Rejected: retrying `catppuccin.tmux` on failure — the race also breaks other
  palette consumers (tmux-fingers failed in the same probe); it masks the
  symptom per plugin.
- Rejected: a lock shared with theme-toggle as the only fix — it needs a
  nix-config change this repo cannot make; kept as the recommended follow-up.

**R1** — `tmux-client-theme` redirects its own stdout to `/dev/null` once, at
the top: it is a background hook job whose only output channel is a pane.

**R2** — the verb's POSIX body becomes
`command -v theme-toggle >/dev/null 2>&1 || exit 0; theme-toggle apply <t> >/dev/null 2>&1; exit 0`
(no `exec`, so the exit status is the shell's 0). The probe verb is unchanged.

## Out of scope

- `prefix + r`: a user-issued `source-file` from an attached client reports its
  own errors in view mode by tmux design; with R3 no theme reload races it.
- nix-config's `theme-toggle`. Needed there (described in the PR, not edited):
  `update_tmux` should take the same mkdir lock `tmux-client-theme` holds
  (`${TMPDIR:-/tmp}/og-client-theme-$UID.lock`) around its
  clear→set→source, clear `@thm_*` in one batched `tmux` call, and send its
  fallback `run-shell "$plugin"` stdout to `/dev/null`. That closes the residual
  window (a hook job already applying when theme-toggle starts) and the remote
  double-apply case.
- The sibling `enrich-refresh` verb has the same `run-shell -b -t <mirrored
  pane>` shape (its `exec tmux-pr-enrich` exit status and output can land on
  the pane) — filed as a follow-up, not fixed here.

## Acceptance tests

- **T1 (red-capable, real reload path)** — `client-theme.bats`: a fake
  `theme-toggle` on PATH that performs `update_tmux`'s exact sequence against
  the inner server while a light report fires: it first repeats the clear
  burst long enough to cover the old script's immediate reload (hit
  near-certainly), then stops, sets the flavor, sources cleanly and exits. Assert: the fake's captured `source-file` output has no
  `returned`, no inner pane is in a mode, one reload beyond baseline, flavor
  latte and `@thm_bg` latte. Red on the old script (race), green with R3.
- **T2 (deterministic overlay)** — `client-theme.bats`: the reload target
  config carries a failing `run-shell`; after a light report no inner pane is
  in a mode (covers both the stdout line and the job's own exit status). Red
  on the old script, green with R1.
- **T4 (R4a)** — `client-theme.bats`: a fake `theme-toggle` that sets the
  flavor to latte, clears the palette and exits non-zero (theme-toggle's failed
  reload) runs while a light report fires; afterwards `@thm_bg` is latte's.
- **T3** — `ctl_test.go`: run the verb's command on an isolated live tmux
  server, once with no theme-toggle on PATH and once with a fake that prints
  and exits 1; the target pane must not enter a mode. Red on the old body,
  green with R2.
- Existing `client-theme.bats`, `remote-theme.bats`, daemon tests stay green.
