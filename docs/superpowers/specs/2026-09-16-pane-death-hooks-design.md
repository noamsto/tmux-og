# SPEC — #647: drive pane-death cleanup from pane-died/pane-exited hooks

## Problem

Per-pane claude-status state (`/tmp/claude-status/{panes,screen,interrupt,tasks,issues,watchers}/<pane_id>`)
is cleaned up by polling: `claude_reap_dead_panes` runs inside `tmux-update-icons`' every-5th-tick
full-server sweep, so a dead pane's state lives up to ~5s past its death, and the sweep costs a
full-server `list-panes -a` plus a directory glob-and-compare on every pass. The pinned tmux
(next-3.9, e880cf6) now lists `pane-created`, `pane-died` and `pane-exited` as window-scoped hooks
(verified: `show-hooks -gw` on the wrapped binary) — the blocker recorded in
`config/tmux.conf.tmpl` ("the `pane-exited` hook is a silent no-op on the pinned tmux", #341) is gone.

## Measured hook semantics (pinned binary, scratch server, 2026-09-16)

| Death path | remain-on-exit | Hook that fires | `#{hook_pane}` |
|---|---|---|---|
| command exits / process SIGKILLed | off | `pane-exited` | `%N` ✓ |
| command exits / process SIGKILLed | on | `pane-died` (corpse stays) | `%N` ✓ |
| `kill-pane` | off | `after-kill-pane` only | **empty** |
| `kill-pane` | on | `after-kill-pane` only (pane destroyed anyway) | **empty** |
| `kill-window` / `kill-session` | either | **none** | — |
| `respawn-pane -k` | either | **none** | — |
| last pane of a session exits | off | `pane-exited` fires | `%N` ✓ |

Consequences:

- `pane-exited` + `pane-died` together cover every *process-death* path with the pane id attached.
- *Structural* destruction (`kill-pane`, `kill-window`, `kill-session`, `respawn-pane -k`) fires no
  pane hook, and `after-kill-pane` carries an empty `#{hook_pane}` — it cannot drive targeted
  cleanup. This is the common way panes die (`prefix + x`, closing windows), so a hook-only design
  would strand state. The sweep must survive as a backstop.
- `#{hook_session_name}` is empty on **every** `pane-exited`/`pane-died` firing (re-measured with
  the session still alive), not just the session-destroying case — hook-time formats carry no
  session identity here at all, which is why the ownership guard below reads the state file's own
  `session=` field instead.

## The caveat that must survive

`CLAUDE_STATUS_DIR` is a bare `/tmp/claude-status` shared by every tmux server on the machine, and
pane ids are per-server `%N` counters — two servers can hold a live `%5` each. A hook fires on
whatever server has it set, so the hook path must delete **only the pane id it was handed** and must
**never iterate a directory**. Additionally, because ids collide across servers, the hook path needs
positive evidence the file belongs to *this* server before deleting (see ownership guard below).

## Design

### 1. `claude_reap_pane PANE_ID` — new function in `scripts/lib-claude.sh`

- Fail closed unless the argument matches `^%[0-9]+$` (same posture as the sweep's row check, #373).
- **Ownership guard:** if `$CLAUDE_PANES_DIR/<id>` exists and carries a non-empty `session=<name>`
  field, require `tmux has-session -t "=<name>"` to succeed on *this* server, else return without
  deleting. The pane is gone by the time the hook runs, so the state file's own `session` field is
  the only ownership evidence; a file naming a session this server doesn't have belongs to another
  server sharing the directory. If `panes/<id>` is absent (a screen-only pane — pi/codex/cursor
  never get one), proceed unguarded: refusing would gut hook cleanup for exactly the agents #635
  added, and the residual collision risk is strictly narrower than the glob-and-compare sweep that
  runs today.
- Guard residuals, stated plainly: (a) two servers sharing the directory with a **same-named**
  session and a colliding pane id pass the guard — the hook can then delete the other server's
  file; still strictly narrower than today's id-absence sweep, and accepted. (b) A
  `rename-session` between the last status write and the pane's death makes the guard fail closed
  (the file names a session this server no longer has), so that residue waits for the backstop —
  graceful degradation, not a leak.
- Unlink exactly that id's files in `panes/`, `screen/`, `interrupt/`, `tasks/`, `issues/`,
  `watchers/` — six explicit paths, no globbing. This deliberately widens today's sweep, which
  reaps only four (`panes/screen/interrupt/watchers`): `tasks/` and `issues/` self-reports are
  documented to "die with the pane", and today they actually only die at server restart. The
  backstop sweep's dir list is widened to the same six (§4), so hook and backstop cover an
  identical set.
- `claude_progress_emit <id> clear`: a remain-on-exit corpse keeps its tty and would otherwise keep
  showing its OSC 9;4 progress bar (the sweep does the same per reaped id).
- `names/` and `live/` are deliberately excluded: pane ids are monotonic within a server run, so
  their residue can never attach to a *new* pane — it is harmless litter that the server-start
  prune (`claude_prune_stale_state`) already owns.

### 2. `scripts/tmux-reap-pane.sh` — new script

Thin entry point for the hooks: guard-source `@lib_claude@` (the `[[ -f "@lib_claude@" ]]` pattern
`claude-status-update.sh` already uses for `@lib_log@`, so the raw script still runs under bats) and
call `claude_reap_pane "$1"`.

### 3. Hook wiring — `config/tmux.conf.tmpl`

- Clear block gains `set-hook -gu pane-exited` and `set-hook -gu pane-died`, so `prefix + r`
  reloads stay idempotent and a rebuild with the feature removed cannot leave a hook pointing at a
  garbage-collected store path (the same rule the existing clears encode).
- Setters at **index 0**:
  `set-hook -g pane-exited 'run-shell -b "<store>/bin/tmux-reap-pane #{q:hook_pane}"'` and the same
  for `pane-died`. Index 0 is this file's convention for tmux-og's primary hook on an event
  (reflow owns index 0 on `after-new-window`/`window-unlinked`); tmux-remux occupies
  `pane-exited[99]`, so there is no collision. `run-shell -b` keeps the fork off the server's
  command queue; `#{q:hook_pane}` is the house quoting style.
- Replace the stale "pane-exited is a silent no-op on the pinned tmux" comment — the pin bump to
  next-3.9 (c058692) is what unblocked this issue.
- `config/tmux.conf.reference.nix` is updated in lockstep if the generator/reference comparison
  requires it (plan-phase detail).

### 4. `claude_reap_dead_panes` kept as a backstop at a lower cadence

Justification (measured above): `kill-pane`/`kill-window`/`kill-session`/`respawn-pane -k` fire no
pane hook, and a server crash fires nothing — both leave state only the sweep can reach. (A global
`set-hook -g` applies to existing windows at config load, so pre-existing windows are *not* a
gap.) The sweep's dir list widens from four to the same six the hook reaps (§1), so the
`kill-pane` path cleans `tasks/`/`issues/` too rather than waiting for the server-start prune. The
every-5th-tick pass in `tmux-update-icons` also stamps `live/` presence, and
`CLAUDE_LIVE_SWEEP_FRESH=15` is calibrated to that 5s cadence, so only the *reap call* moves to a
slower gate (~60s), presence stamping unchanged. The client-gating is unchanged: the reap still
never runs from the `@og-sweep-tick` monitor-hook path.

### 5. `claude_prune_stale_state` untouched

The server-start mtime prune covers state orphaned across a restart, which no hook can.

### 6. Daemon half (`healDeadRenderers` ← `pane-died`): OUT OF SCOPE, pending dispatcher routing

`picker/remotebridge/**` is owned by the concurrent #645 worker. A question is posted on the bus;
if routed away, the daemon half splits into a follow-up issue and this PR says so. Note for that
follow-up: mirror windows carry `remain-on-exit on`, so a dead renderer fires `pane-died` (not
`pane-exited`) with the corpse's options still readable; the `deadRendererStrikes` cap and the
`resetWindow`-not-`retireMirror` repair path (#547) must survive any event-driven rework.

## Non-goals / invariants

- Never a directory sweep from a hook path — single pane id, single set of unlinks.
- No deletion on the `@og-sweep-tick` monitor-hook path (it arms `agent-detect` only).
- `watchers/` registry: the hook deletes the *dead pane's own* registration, the same authority the
  sweep has today; the superseded-watcher-never-deletes-its-own-registration invariant is untouched.
- No `after-kill-pane`-based cleanup (`#{hook_pane}` is empty there — cannot target).
- No changes to float binds or `pane-border-format` (#648's files), no `#646` work.

## Acceptance criteria

1. A pane's process exiting (cleanly or via SIGKILL) on a server with this config removes exactly
   that pane's six state files within ~1s; sibling panes' files untouched.
2. Same with `remain-on-exit on` (corpse path, `pane-died`).
3. `kill-pane`/`kill-window` residue is still removed — by the backstop sweep at its lowered
   cadence (test forces the call rather than waiting).
4. Cross-server safety: scratch server B sharing `CLAUDE_STATUS_DIR` with server A cannot delete
   A's state via the hook path when the state file names a session B doesn't have (foreign-session
   names — the same-named-session residual in §1 is accepted and out of scope for this test).
5. bats coverage: a scratch-server test loading the real config (scratch `TMUX_TMPDIR` **and**
   scratch `CLAUDE_STATUS_DIR`) asserting exactly-one-pane deletion on `pane-exited`, plus a
   foreign-session guard test.
6. Gate green: `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Hardware verify (per task doc)

Scratch server, agent in a pane, kill the pane → exactly that pane's files go. Second scratch server
against the same `CLAUDE_STATUS_DIR` → cannot delete the first's state. `TMUX_TMPDIR=/tmp/og-$$`,
scratch-exported `CLAUDE_STATUS_DIR`, `kill-server` when done. Never the live server, never the real
`/tmp/claude-status`.
