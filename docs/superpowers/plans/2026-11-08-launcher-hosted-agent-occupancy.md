# Launcher-hosted agent occupancy (#1091)

## Problem and invariant

`@window_has_agent` (window occupancy) is derived only from `pane_current_command ∈ $AGENT_COMMANDS`.
A launcher-hosted agent (`bash …/dispatcher` loop, `claude` its child in the same foreground pgrp) reports
`bash`, so occupancy is empty and the #692 sweep clears `@window_ai_name`/`@window_task` of a live agent.

Invariant to restore: *a pane hosting a live agent counts as agent occupancy; a pane whose agent exited
(clean or not) does not, even while its launcher shell stays alive.*

## Mechanism (in-repo only)

Positive, per-pane hosted-agent evidence = **hook-written `panes/<id>` file carrying `agent_pid=<N>`, where
`kill -0 N` succeeds and the file's `server=` (when present) equals this server's pid.**

- Writer: `claude-status-update` records `agent_pid` in `panes/<id>` — the nearest ancestor of the hook whose
  normalized `comm` ∈ `$AGENT_COMMANDS`. Resolved by an ancestor walk (`ps -o ppid=,comm= -p`) **only when
  the stored pid is absent or dead** (once per session; resume/relaunch gets a new pid, so re-resolves), never
  on every hook. `CLAUDE_AGENT_PID` env overrides (test seam, and a hook any wrapper can use). Unresolved →
  no line → fails closed to today's behaviour.
- Exit: clean exit → `SessionEnd` hook (`status.sh clear`) already deletes `panes/<id>` → no evidence.
  Crash / SIGKILL → file lingers but `kill -0` fails → no evidence. Both clear the title at the next sweep,
  with the launcher still alive and with no client attached.
- Reader: new `claude_pane_hosts_agent PANE_ID SERVER_PID` in `lib-claude.sh` (sets `REPLY`, returns 0/1).
  Fork-free: one `[[ -f ]]` for the common (no-file) pane; for a pane with a file, a `read` loop plus builtin
  `kill -0`. Server pid defined once, by the caller: `srv=${SERVER_PID:-}; [[ -n $srv ]] || { srv=${TMUX:-}; srv=${srv#*,}; srv=${srv%%,*}; }`
  (`$TMUX` is `sock,pid,idx`; `$SERVER_PID` is set by `main` on the per-tick path). An empty `srv` against a file
  carrying `server=` fails closed (no evidence). Used in `arm_agent_detect` and `tmux-shell-prompt.sh`. Ownership guard (server match) keeps a same-uid scratch server's colliding pane ids from
  matching the real server's files, and the helper only ever feeds *additive* decisions + the existing
  non-destructive sweep, so no new deletion path exists.
- Known bounded residuals (documented): pid reuse after a crash can keep occupancy until that pid dies; a nested
  `claude -p` in the same pane fires its own `SessionEnd clear`, deleting `panes/<id>` until the outer agent's next
  hook write re-creates it and re-resolves `agent_pid`.
- The ancestor walk compares `basename` of `ps -o comm=` (also `args=` argv[0`) through `normalize_wrapped_cmd`
  (comm is cut at 15 chars; darwin prints a full path). The real comm of the live `%837` launcher→`claude` is checked
  once and recorded in the PR.

## Consumer map (disposition)

| consumer | disposition |
| --- | --- |
| `arm_agent_detect` loop: `win_has`, `live/` stamp | **changed**: hosted pane takes the agent path |
| `arm_agent_detect` loop: `pipe-pane` arming, `@og_birth_size` repaint | **skipped by design**: `agent-detect` resolves its manifest from `pane_current_command` (`picker/agentdetect/main.go:66-73`), so on a `bash` pane it exits at once, the pipe closes and every sweep would re-fork it. Test A asserts no `pipe-pane` for the hosted pane |
| other readers of the `panes/<id>` format (`picker/main.go`, `picker/statusline/claude.go`, remotebridge `agentstatus.go`, mark-seen rewrite, `claude_prune_stale_state`/`claude_pane_owned_elsewhere`) | **compatible**: key=value readers ignore the new `agent_pid` key; mark-seen's `grep -v '^unseen='` preserves it (test) |
| `arm_agent_detect` #692 sweep branch (`@window_has_agent` set; stale `@window_ai_name` clear) | **compatible** (reads `win_has`); test: sweep keeps title; clears after exit |
| per-tick `has_agent` in `tmux-update-icons` + `claude_clear_window_naming` | **changed**: also scans window pane ids with helper |
| `tmux-shell-prompt.sh` leading `pcc` check | **changed**: hosted-agent pane at a nested prompt exits early (must not delete `panes/`) |
| `tmux-shell-prompt.sh` `still_has_agent` | **changed**: counts hosted panes |
| dispatcher standby overlay (#1083) | **compatible** once `@window_has_agent=1`; confirmed it could not fire before (test) |
| `tmux-reflow-windows` crew/FOLD_AGENT, `tmux-apply-theme-colors` border/codename chains, Go `picker`/`statusline` crew gating | **compatible**: all read `@window_has_agent` only |
| `read_pane_state` floor / `claude_reap_*` / prune | **compatible**: unchanged; `live/` stamp of a hosted pane just makes the floor apply to it |
| bridge mirror windows | **compatible**: `@bridge_win` skipped at every site, test retained |
| `@window_manual_name` | **compatible**: untouched |

## File list

- `scripts/lib-claude.sh` — add `claude_pane_hosts_agent`.
- `scripts/claude-status-update.sh` — resolve/preserve `agent_pid`, `@AGENT_COMMANDS@`, write the line.
- `config/tmux.conf.nix` — add `@AGENT_COMMANDS@` → `agentCommands` to `mkScriptWithLog`'s `replaceStrings` (builder of `claude-status-update`); keep the `${AGENT_COMMANDS:-@AGENT_COMMANDS@}` default so raw tests work.
- `scripts/tmux-update-icons.sh` — sweep loop + per-tick `has_agent` use the helper.
- `scripts/tmux-shell-prompt.sh` — the two checks above.
- `tests/launcher-hosted-agent.bats` (new) — regressions; `tests/claude-issues.bats`-style raw run for writer.
- `docs/agents/agent-state.md`, `docs/agents/scripts.md` — document the occupancy rule.

## Steps

- [ ] **Step 1: failing sweep regression (red on old behaviour)** — `tests/launcher-hosted-agent.bats`: fake-`tmux`
  harness like `tests/agent-detect-arm.bats`: the fake `tmux` logs every call and answers `list-panes` with a row of exactly the
  12 fields the loop reads (`pane_id|cmd|piped|wid|bridge|ha|manual|birth|seen|ai|task|session`, e.g.
  `%7|bash|0|@3||||||ai-title||sess`). `CLAUDE_STATUS_DIR` 0700 tmp, `panes/7` with `state=idle`, `server=<N>`,
  `agent_pid=$$` (a live pid), `export TMUX=/x/sock,<N>,0`. Entry point (lib-claude first — the raw script's
  `source @lib_claude@` loads nothing, as in `tests/agent-liveness.bats`):
  `CLAUDE_NOW=100 bash -c 'source scripts/lib-claude.sh; source scripts/tmux-update-icons.sh; arm_agent_detect force'`.
  Test A: `set -qw -t @3 @window_has_agent 1` logged, no `@window_ai_name ""` write, no `pipe-pane` for `%7`.
  Test B: `agent_pid` of a reaped pid (`sleep 0 & p=$!; wait`) → `@window_ai_name ""` and `@window_naming_dirty 1` logged
  (crash clears the title). Test C: no `panes/7` → clears (unchanged). Test D: `server=` mismatch, and unset `TMUX` with a
  `server=` line → clears (fail closed). Test E: `@bridge_win=1` row skipped. Test H: after `claude-status-update clear`
  (clean SessionEnd) the next sweep clears the title with the launcher still the pane command.
  Run `bats tests/launcher-hosted-agent.bats` before Steps 2–3 → A fails on the `@window_ai_name ""` assertion (confirm the
  failing line is that assertion, not a missing-function error: the old `case` branch simply clears), B/C/D/E pass.
- [ ] **Step 2: helper** — `lib-claude.sh` `claude_pane_hosts_agent`; unit-run inside the same bats file.
- [ ] **Step 3: sweep loop** — in `arm_agent_detect`, in the `case` default branch, before the birth-grace
  handling: if `claude_pane_hosts_agent "$pid" "$srv"` (srv defined as above, once before the loop) → set
  `win_has[$wid]=1` and the `live/` stamp only; `continue` before the `pipe-pane`/birth-repaint lines (skipped by design,
  see the map). Run `bats tests/launcher-hosted-agent.bats tests/agent-detect-arm.bats tests/agent-liveness.bats` → green.
- [ ] **Step 4: per-tick `has_agent`** — after the `win_procs` loop, when `has_agent` empty and window not a
  mirror, loop `win_panes[$wkey]` ids with the helper (`$SERVER_PID`). Test F in the new bats file via the real-tmux
  harness (scratch server on its own `TMUX_TMPDIR`/state dirs, setup copied from `update-icons-dispatcher.bats`): a window whose
  pane runs a *looping* launcher script (`while :; do ./claude-copy; done` — not `bash -c 'cmd'`, which execs and hides the bug)
  hosting a `claude` copy; assert `pane_current_command` is `bash` first; fresh `panes/<id>` with `agent_pid` of the child;
  after `update-icons` `@window_has_agent` reads `1`, and a dispatcher-named window gets the standby overlay when a worker window
  exists (update-icons run twice: the overlay reads the pre-tick `@window_has_agent`). Test G (doubles as the repro of the
  revert sequence: with `naming_read` empty the per-tick path resets `@window_ai_name` every tick with no transition, so `names/`
  survives — recorded in the PR): kill the child, launcher stays → after update-icons `@window_has_agent` empty and `names/<id>` deleted.
- [ ] **Step 5: shell-prompt** — `tmux-shell-prompt.sh`: hosted check after the `pcc` agent check (exit 0, no clear),
  and in `still_has_agent`. Extend `tests/pane-shell-prompt.bats`: launcher-hosted live agent keeps `panes/<id>` and window naming;
  after pid death the existing clear path runs.
- [ ] **Step 6: writer** — `claude-status-update.sh`: `agent_pid` resolve/preserve + write line; `@AGENT_COMMANDS@`
  seam; `config/tmux.conf.nix` substitution. Tests (raw run, `CLAUDE_AGENT_PID` seam and a fake `ps`): line written once,
  preserved while alive, re-resolved when dead, absent when unresolved; `SessionEnd clear` leaves no file; mark-seen keeps the
  `agent_pid` line. `bats tests/claude-issues.bats tests/launcher-hosted-agent.bats` → green.
- [ ] **Step 7: counterfactual evidence** — `cp -r scripts tests picker/mirrorname picker/claudestatus` (whatever the bats load) into a
  scratch dir under the scratchpad, `git show origin/main:scripts/tmux-update-icons.sh` over its `scripts/tmux-update-icons.sh`
  (old production behaviour), run `bats tests/launcher-hosted-agent.bats` from that dir (bats resolves `scripts/` relative to PWD)
  → record the Test A and F assertion failures; the same command in the worktree → green. The working tree is never reverted.
- [ ] **Step 8: docs** — `docs/agents/agent-state.md` (#671 bullet: new occupancy rule, `agent_pid`, residual) and the
  `tmux-update-icons`/`tmux-shell-prompt`/`claude-status-update` rows of `docs/agents/scripts.md`.
- [ ] **Step 9: gates** — `nix build .#default`, `nix flake check`, `nix build .#lint` (inside the devshell).

## Implementer notes (critic, non-blocking)

- Add one real-tmux `OG_TICK_SWEEP=1` pass (via `tmux run-shell` in the scratch server) asserting `@window_ai_name` survives, so the sweep path's `$TMUX` is proven, not hand-exported.
- Test F child: start it non-interactively (`sleep inf </dev/null`-style or `-c 'read -r _ </dev/tty'`), not a bare interactive bash copy; assert `pane_current_command` is `bash`.
- `normalize_wrapped_cmd` is not in `lib-claude.sh`/`claude-status-update.sh`: the writer defines its own copy (`return 0`, `set -e` safe). Cap the ancestor walk depth (8) so a host where resolution never succeeds does not fork unboundedly per hook.
- Guard the new call with `declare -F claude_pane_hosts_agent` (or source lib-claude in raw-sourcing suites) so `tests/agent-detect-arm.bats` stays quiet.
- Test H runs `claude-status-update` raw with `--pane %7` and fake tmux on PATH so it truly reaches the delete.
- Consumer map additions: `tmux-agent-usage.sh` (`@og_pane_agent`) and `tmux-carousel-restore.sh` match `pane_current_command` against `$AGENT_COMMANDS` and do not gate on occupancy — out of scope, unchanged (recorded as compatible).

## Acceptance

- [ ] sweep regression red on old / green on fix → Step 1/7 output.
- [ ] hosted agent exit clears title → Tests B and G.
- [ ] `@window_has_agent` reads `1` for hosted agent → Test F.
- [ ] consumer dispositions → table above, test or code refs in the PR ledger.
- [ ] `agent-state.md` → Step 8.
- [ ] `nix build .#default` / `nix flake check` / `nix build .#lint` → Step 9.
