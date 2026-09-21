# Fix: `notify_prune` uses one machine-wide marker and no ownership (same bug class as #676)

## Problem

`scripts/lib-notify.sh` `notify_prune SERVER_START` gates on a single
`/tmp/og-notify/.server_start` while the event store `/tmp/og-notify/events`
is shared by **every** wrapped-tmux server on the machine (same shape as the
shared `CLAUDE_STATUS_DIR`). Its staleness test is only `file mtime < this
booting server's start_time`, and the marker is a single file.

Consequences with two live servers A (booted first) and B:

- **Ping-pong marker.** `notify_prune` returns early only when the shared
  marker equals *this* server's `start_time`. A ≠ B, so every emit by either
  server rescans the event dir and rewrites the shared marker. The gate is not
  a gate.
- **Live events deleted.** B's scan uses B's (later) `start_time` as the
  cutoff, so every event A wrote before B booted — still valid for A's live
  windows — is deleted. A dead server's `window=@N` *is* actively misleading,
  but a *live* server's is not; mtime alone cannot tell them apart.

The comment says it "structurally mirrors `claude_prune_stale_state`", which
now differs: #706 gave that function a per-server marker gate
(`.server_start.<pid>`, content = start_time) and a `server=` PID-liveness
ownership check (`kill -0` + `ps -o comm=` matching `tmux`).

## Root cause

Timing-based staleness (`mtime < my start_time`) cannot distinguish "written by
a server that has since died" from "written moments ago by a different,
still-running server." Only per-event ownership can. And one shared marker
cannot gate two independent server generations.

## Fix

Mirror #706 in the notification store, **without touching `lib-claude.sh`**
(owned by #709's concurrent worker):

1. **Producer stamps ownership.** `scripts/og-notify.sh` already resolves
   `#{start_time}` in its single tmux read; add `#{pid}` (the tmux **server**
   pid) and write `server=<pid>` into every event file.
2. **Per-server marker gate + ownership in `notify_prune`.**
   `notify_prune SERVER_START [SERVER_PID]`.
   - With `SERVER_PID`, the gate is `.server_start.<pid>` (content =
     `start_time`), so two live servers each sweep once per boot instead of
     ping-ponging one shared marker. The shared `.server_start` is still
     written after every sweep, and remains the only gate when `SERVER_PID` is
     empty.
   - Events whose `server=` names a different, still-live tmux process are
     protected from deletion regardless of mtime.
   - A sweep removes `.server_start.<pid>` markers whose pid is no longer
     alive.
3. **Local liveness helper.** Add `notify_pid_is_tmux PID` to `lib-notify.sh`,
   mirroring `claude_pid_is_tmux` (lib-claude.sh:86-99) verbatim — `kill -0`
   plus `ps -o comm=` matching `*tmux*`, failing safe toward "yes" when `ps` is
   absent or answers nothing. Deliberately a copy rather than a refactor:
   moving the helper into `lib-log.sh` and repointing `lib-claude.sh` would
   edit a file another worker owns. Comment it as such.

**Ownership granularity.** Each event file carries its own `server=` line, so
protection is per-file (simpler than #706's id → across-dirs map). A legacy
event with no `server=` field stays mtime-prunable — a one-time migration, the
same posture #706 took for legacy `panes/<id>` files.

## Steps

- [x] **Step 1: `scripts/lib-notify.sh` — add `notify_pid_is_tmux` + rework `notify_prune`.**
      Add `notify_pid_is_tmux PID` immediately above `notify_prune` (mirror of
      `claude_pid_is_tmux`). Change the signature to
      `notify_prune SERVER_START [SERVER_PID]`:
      - `marker="$NOTIFY_MARKER"`; `gate="$marker"`;
        `[[ -n $server_pid ]] && gate="$marker.$server_pid"`.
      - Early-return when `$gate` is readable and equals `$server_start`.
      - Inside the existing lock subshell: for each event file (when
        `server_pid` is non-empty) parse the `server=` line exactly as
        `claude_prune_stale_state` parses its `panes/<id>` owner
        (`while IFS='=' read -r key val || [[ -n $key ]]`). If the owner is
        numeric, differs from `$server_pid`, and `notify_pid_is_tmux` says it
        is live (memoized in a `local -A owner_live`), skip the file. Owner
        liveness is evaluated once per distinct owner per pass.
      - Otherwise `file_mtime` and remove when `mt < server_start`.
      - Write `$gate` (per-server), remove `.server_start.<pid>` markers of
        dead pids (skip our own), then write the shared `$marker`.
      Keep `acquire_lock` release-by-EXIT-trap semantics and the
      empty-`server_start` no-op unchanged. Update the function's header
      comment to state the ownership rule and the deliberate
      not-shared-with-lib-claude decision.

- [x] **Step 2: `scripts/og-notify.sh` — stamp `server=` and pass the pid.**
      Extend the single read format `'#{window_id}|…|#{start_time}|#{@thm_red}|…'`
      to `'…|#{start_time}|#{pid}|#{@thm_red}|…'` (pid immediately after
      `start_time`; `session_name` stays last — it is the only free-form field
      and must absorb the remainder). Add `srv_pid` to the `IFS='|' read`
      list at the matching position. Write `printf 'server=%s\n' "$srv_pid"`
      into the event file, after `printf 'ts=%s\n' "$now"` and before
      `source=` (adjacency to `ts` keeps both server-identity lines together).
      Pass both args: `notify_prune "$srv_start" "$srv_pid"`.
      An empty `srv_pid` (no tmux, or a format that resolves empty) degrades to
      exactly today's behavior — an empty `server=` line makes the file
      unprotected, and the 1-arg call keeps the shared gate — so the fix's
      failure mode is the old bug, not a new one. This is deliberate and
      documented, not accidental. `session_name` remains last so a `|` in a
      session name is absorbed as bash `read`'s unsplit remainder, the same
      rule `og-notify.sh` already relies on.

- [x] **Step 3: `tests/notify-router.bats` — update fixtures + add regression.**
      - Extend `active_info`, `background_info`, and the three inline
        `FAKE_INFO` strings with a `#{pid}` field at the new position
        (value `1234`). Extend the line-188 "exact key set" assertion to
        include `server` and the `wc -l` count from 7 to 8.
      - Add an explicit **value** assertion to the exact-key-set case:
        `grep -qx 'server=1234' "$f"`. A key-set assertion alone passes on a
        positional misalignment in the `IFS='|'` read list — the key would
        still read `server` while its value is a theme hex. The value is the
        only link between Step 2's format/read edit and the new field.
      - Add a local `start_fake_tmux_server` helper (copy of
        prune-stale-state.bats:34-42: a copied `bash` named `tmux` idling on a
        FIFO; sets `FAKE_TMUX_PID`) and a `teardown` that kills it.
      - Of the inline `FAKE_INFO` literals, only two are non-empty and need the
        new field (`:130`, `:174`); `:250` is deliberately empty and stays so.
      - Add regression cases mirroring prune-stale-state.bats:121-193:
        `prune protects an event whose server= pid is a different, live
        process`; `prune reaps a stale event whose server= pid is dead`;
        `prune gates per server pid: same pid skips, a different pid still
        sweeps`; `prune removes per-server markers of dead pids`; `prune reaps
        a legacy event with no server= field`; `prune with SERVER_PID omitted
        ignores server= entirely (backward compatible)`.

- [x] **Step 4: `flake.nix` — give `notify-router-tests` `pkgs.procps`.**
      The new `notify_pid_is_tmux` cases fork `ps`; without `procps` on the
      check's `nativeBuildInputs` the comm-match branch cannot run in CI.
      Matches f2a32e8's identical `prune-stale-state-tests` change.

- [x] **Step 5: red-before-green evidence.** Reproducibly, without touching
      the tests: `git stash push -- scripts/lib-notify.sh scripts/og-notify.sh`
      (reverts only the production change to HEAD; `tests/helper.bash` loads
      `$PWD/scripts/lib-notify.sh`, so the new tests run against the old
      library), then `bats tests/notify-router.bats` and record the ownership
      case's assertion failure; `git stash pop` and confirm green. Record
      commands and observed output in the PR `## Evidence`.

- [x] **Step 6: commit the plan document**
      (`docs/superpowers/plans/2026-09-21-fix-notify-prune-ownership.md`)
      alongside the code in this PR — CLAUDE.md "Plans and Specs" requires it,
      and an untracked plan is lost when the worktree is reclaimed (#706 did
      the same).

## Acceptance criteria

- [x] Two live servers no longer ping-pong one shared marker: a second
      server's prune does not delete the first's event file whose `server=`
      names the still-live first server, even when the file is older than the
      second server's `start_time`.
- [x] A dead server's events are still reaped by a later server's boot
      (`server=` pid not alive, or legacy file with no `server=`).
- [x] A server still sweeps at most once per boot (per-server marker gate).
- [x] Per-server markers of dead pids are removed.
- [x] `notify_prune SERVER_START` (no pid) keeps the pre-#710 behavior and the
      shared `.server_start` gate — backward compatible.
- [x] The event file gains a `server=<pid>` line; the history center ignores
      it (its parser is a `case` over known keys).
- [x] `nix build .#default`, `nix flake check`, `nix build .#lint` all pass.

## Evidence and consumer map

`notify_prune` is reached only from `scripts/og-notify.sh` (the router's emit
path). `og-notify-center` reads the event store but never prunes and parses
keys through a `case`, so an added `server=` key is compatible (verified at
`scripts/og-notify-center.sh:74-84`).

| consumer / producer | edge | disposition |
| --- | --- | --- |
| `og-notify.sh` emit | writes event file; calls `notify_prune` | changed: adds `server=`, passes `#{pid}` |
| `og-notify.sh` tmux read | `#{start_time}` plus `#{pid}` | changed: field added |
| `og-notify-center.sh` | reads `ts/source/level/window/session/title/body` | compatible: unknown key ignored |
| `lib-notify.sh notify_prune` | signature gains optional `SERVER_PID` | changed: optional, 1-arg callers unaffected |
| `tests/notify-router.bats` | fixtures + prune cases | changed: fixture field, new cases |
| `tests/notify-producers.bats` | asserts producer argv to `og-notify`, not event fields | compatible: no change |
| `tests/notify-center.bats` | builds its own event fixtures | compatible: no `server=` required |
| `tests/notify-bell-integration.bats` | real tmux, real read | compatible: real `#{pid}` resolves |
| `flake.nix notify-router-tests` | test env | changed: adds `pkgs.procps` |
| `config/tmux.conf.*` | wires `og-notify`; comment only | compatible: no change |

The regression test oracle is the contract ("a live foreign owner's event
survives; a dead owner's does not"), not a copy of the implementation's
predicate.

## Accepted limits (inherited from #706, not regressions)

- **Marker cleanup uses bare `kill -0`.** A reused PID owned by a live non-tmux
  process keeps a dead server's `.server_start.<pid>` marker forever. Harmless:
  the marker is only a gate, and the live server it names re-writes its own.
- **A live foreign owner's events are protected indefinitely.** Even after it
  closes the window an event names, its files survive while the process lives.
  Under-protect is the intended posture — deleting a live server's notification
  is the bug being fixed.
- **`ps` absent or silent** → `notify_pid_is_tmux` cannot tell, and protects
  (under-reap, never over-delete), matching `claude_pid_is_tmux`.