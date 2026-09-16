# SPEC — #646: drive dead-agent detection from OSC 133 pane-command/shell-prompt hooks

## Problem

No hook fires today when an agent (Claude Code, codex, cursor, pi) exits back to a
shell, so its last state (`processing`/`done`/…) lives until the pane dies or the
server restarts. The dead-agent floor (`claudeStatus.assumeDeadAfter`, default off) is
a polling inference: `tmux-update-icons`' presence sweep stamps `live/<pane_id>` for panes
whose `pane_current_command` is an agent, and `read_pane_state`
(`scripts/lib-claude.sh`) *withdraws* (returns 1, nothing on disk changes) a stale state
once the stamps stop advancing. Two gaps follow from "withdraw, don't remove":

1. The veto lives in `read_pane_state`, so only shell consumers honour it —
   `picker/statusline` and the pickers read `panes/` directly and keep rendering the
   withdrawn state (the consumer asymmetry).
2. It is inference with a ~15s+ floor, not the event itself.

The pinned tmux (e880cf6, `tmux -V` reports `next-3.9`) ships OSC 133
`pane-command-started` / `pane-command-finished` / `pane-shell-prompt` hooks
(CHANGES 3.7c→3.8). This spec replaces the *inferred* withdrawal with an *event-driven
clear* on the shell's own prompt mark, while keeping the floor as a backstop for shells
that emit no OSC 133.

## Measured hook semantics (pinned binary, scratch server, `-f /dev/null`, 2026-09-16)

OSC 133 mark → event mapping (from `input.c` `input_osc_133`, verbatim):

| Mark | Grid effect | Event fired | Payload of note |
|---|---|---|---|
| `ESC]133;A` / `ESC]133;N` | start-prompt | `pane-shell-prompt` | target pane; `last_prompt_time` set |
| `ESC]133;P` | prompt (secondary) | **none** | grid only |
| `ESC]133;B` / `ESC]133;I` | start-command | **none** | grid only |
| `ESC]133;C` | start-output | `pane-command-started` | `hook_command_start_time` |
| `ESC]133;D[;<status>]` | end-output | `pane-command-finished` | `hook_command_status`, `hook_command_start_time`, `hook_command_end_time`, `hook_command_duration` |

Measured in the hook's `run-shell` (the pane is alive at prompt time):
- `#{hook_pane}` → `%N`; `#{session_name}` → the session name; `#{window_id}` → `@N`
  — all safe to use.
- `#{session_id}` resolves to `$N` but `run-shell`'s `sh -c` then **re-expands the
  leading `$`** (e.g. `$0` → `sh`), so it is garbage in the handler — never use it
  (the same #95-era trap the `after-new-window` comment already records).
- `#{hook_session_name}` / `#{hook_session_id}` / `#{hook_window_id}` are **empty**:
  `events_fire_pane` payloads only `pane`/`window`, not `session` (unlike
  `events_fire_winlink`), so the ownership guard reads the *plain* `#{session_name}`
  from the target, not any `hook_` session form.
- `#{pane_current_command}` resolves but, at every OSC-133 event, it is the **foreground
  process-group leader's argv[0]** (`osdep_get_name` reads `/proc/<tcgetpgrp>/cmdline`,
  first field). Measured values across the agent lifecycle:

| Moment | `#{pane_current_command}` |
|---|---|
| fish `preexec` (`C`) | `fish` (command not yet exec'd) |
| fish `postexec` (`D`) | `fish` (command already exited) |
| fish prompt (`A`) after agent exit | `fish` |
| nested `A` emitted while a fake agent (`bash` script) is still running | `bash` (the agent's argv[0]) |

- A SIGINT (Ctrl-C) on the foreground agent fires `pane-command-finished` with
  `hook_command_status=130` **and then** `pane-shell-prompt` — the shell redraws its
  prompt on every foreground-job death, so `pane-shell-prompt` is kill-safe.
- Events fire with zero attached clients (all measurements ran on a detached
  `-d` server): `input_osc_133` → `events_fire_pane` is in the pane-output path, not the
  client path. This is what keeps a bridge-only remote (control-mode client, no status
  line) covered.

**Fish 4.9.3 emits by default** (captured raw from a live pane): `OSC 133;A;click_events=1`,
`OSC 133;B`, `OSC 133;C;cmdline_url=…`, `OSC 133;D;<status>` — plus `OSC 7`/`OSC 0`/`OSC 11;?`
(unrelated). The `A` mark carries a trailing `;click_events=1` parameter; tmux's `case 'A'`
ignores the tail and fires the prompt event.

## Answers to the open questions

### Q1 — shell coverage and fallback
Fish emits the marks (verified). Shells without OSC 133 integration (bash/zsh without
terminal-shell-integration, a remote host's arbitrary shell) emit **nothing**, so no event
fires and the hook is inert. The dead-agent floor stays as the only mechanism there —
"no OSC 133 emitted → floor behaviour unchanged" is both a fallback and an acceptance
criterion.

### Q2 — bridge (mirrored panes)
A mirrored pane's OSC 133 output arrives on the **remote** server (the real shell lives
there; the local renderer pane is not a shell and emits nothing). The hook wiring lands in
tmux-og's shared `config/tmux.conf.tmpl`, present on any host rebuilt from this revision —
so a current remote fires the handler server-side like a local host. The handler must
therefore clear not only the shared files but also the **pane options** `@claude_status`
and `@agent_screen`: the existing agent-status subscription shipper
(`picker/remotebridge/daemon/agentstatus.go`) already carries option changes to the mirror
and already deletes the mirror's local `panes/<id>`/`screen/<id>` when a carried row has an
empty `state`/`screenState` (`removeFiles`/`removeScreenFile` — verified). No
`picker/remotebridge/**` change is needed; a remote too old to wire the hook emits no event
and keeps the floor. (The #657 daemon-heal constraint is therefore untouched.)

### Q3 — agent killed rather than exiting
`pane-shell-prompt` fires whenever the shell redraws its prompt, which it does after a
foreground job dies by any means (verified SIGINT → `D;130` then `A`). A pane whose
*shell* dies is out of scope here: `pane-exited`/`pane-died` → `tmux-reap-pane` (#647) own
that path.

### Q4 — the floor survives as a backstop
Keep `claudeStatus.assumeDeadAfter`/`live/`/`.sweep`/`read_pane_state`'s veto unchanged:
it is the only mechanism for non-OSC-133 shells and old remotes, and it is already tested
(`agent-liveness.bats`). Precedence is naturally "event wins, floor falls back": the event
*clears* the files outright (immediate), so a later sweep finds no agent state to withdraw
and stops stamping the pane; where the event never fires, the floor's derived withdrawal
applies exactly as today. No conflict, no ordering work.

### Q5 — which state is cleared
Clear the agent **state**, so every consumer — shell *and* Go — stops rendering it:
- `panes/<id>` — the hook state file (the consumer-asymmetry fix: `picker/statusline` and
  the pickers read this directly and never applied the floor veto),
- `screen/<id>` — the screen-scraped state (codex/cursor/pi),
- `interrupt/<id>` — the derived-state cache keyed to this pane,
- `@claude_status` and `@agent_screen` pane options — the bridge shipper's source (Q2),
- `claude_progress_emit clear` — clear a lingering OSC 9;4 progress bar, the same
  bookkeeping `claude_reap_pane` and `claude-status-update clear` do.

**Not cleared:** `tasks/`, `issues/`, `names/`. Those are the workspace *identity* the
floor never touched; they are owned by `claude-status-update clear` (clean SessionEnd) and
`claude_reap_pane` (pane death), and a killed agent leaving its title until the window
closes is today's behaviour, out of scope.

### Q6 — false positives (nested prompts)
The discriminator is `pane_current_command` at `pane-shell-prompt` time:
- a **nested prompt** inside a still-running agent carries the agent's argv[0] (the agent
  remains the foreground process-group leader; its child shell doesn't take the terminal),
- a **real exit** carries the shell.

So the handler clears only when `pane_current_command` (normalized via
`normalize_wrapped_cmd`) is **not** in the agent manifest (`@AGENT_COMMANDS@` — the same
list and normalization the sweep already uses to *arm* detection). An empty/unreadable
`pane_current_command` fails closed (skip); a later prompt self-heals once the value is
readable. This is strictly stronger than nesting-signal arithmetic: it needs no
`D`-nesting counter and no `hook_command_status` parsing. An agent launched under a
non-manifest argv[0] (pipx/wrapper) would read its own nested prompt as an exit and
clear — but that gap is *inherited, not introduced*: the sweep already arms detection
from exactly this `@AGENT_COMMANDS@` list, so such a pane was never swept/stamped either,
and the floor stays as backstop.

Known limitation, stated plainly: `Ctrl-Z` suspending an agent and running shell commands
reads like an exit and clears; the next agent hook write re-stamps, and the floor's own
sweep would stop stamping (`pane_current_command` is the shell) and eventually withdraw too
— the hook is merely instantaneous where the floor was thresholded.

## Design

### 1. `claude_clear_agent_state PANE_ID SESSION` — new function in `scripts/lib-claude.sh`

The clear + the shared-directory ownership guard, in one testable place (mirrors
`claude_reap_pane`'s shape):

- **Fail closed on args:** PANE_ID must match `^%[0-9]+$` (same posture as the sweep's row
  check, #373); empty/unknown SESSION is handled by the guard, not a fatal.
- **Ownership guard, stronger than #647 because the pane is alive:** `CLAUDE_STATUS_DIR` is
  a bare `/tmp/claude-status` shared by every server and pane ids are per-server `%N`
  counters. If `panes/<id>` exists and carries a non-empty `session=` field, clear only
  when that field equals SESSION (the firing pane's own `#{session_name}`, available
  because the pane is alive). A mismatch is a foreign server's colliding id → return
  without clearing. A `rename-session` between the last write and the exit fails closed and
  waits for the backstop (same graceful degradation as #647). If `panes/<id>` is absent (a
  screen-only pane: codex/cursor/pi never get one) or its `session=` is empty, clear
  unguarded — the same accepted residual #647 documents for screen-only panes, made no
  worse by the fact that the screen state itself is already last-writer-wins across
  colliding ids.
- **Grouped-session residual (same posture as #647's rename-session note):** in a session
  group `#{session_name}` resolves to the most-recent group member, which can differ from
  the `session=` name stored in `panes/<id>` — the guard then fails closed and the clear is
  skipped, degrading to the floor. tmux-og doesn't use session groups; note this in
  CLAUDE.md, not a design change.
- **Short-circuit** before any option/tty write when none of `panes/<id>`, `screen/<id>`,
  `interrupt/<id>` exist: an agent-free pane's prompt (nearly every prompt) costs one
  `[[ -f ]]` and exits, skipping the two `tmux set` forks and the tty write.
- Clear exactly: `panes/<id>`, `screen/<id>`, `interrupt/<id>` ; `claude_progress_emit
  <id> clear` ; `tmux set -pq -t <pane> @claude_status "" \; set -pq -t <pane>
  @agent_screen ""` (best-effort, pane options are per-server so no ownership question).
- `names/`, `live/`, `tasks/`, `issues/`, `watchers/` are untouched (see Q5; `names/`/
  `live/` are monotonic-id litter owned by `claude_prune_stale_state`; `watchers/` dies
  with the pane via #647 or the sweep).

### 2. `scripts/tmux-shell-prompt.sh` — new thin hook entry point

Same guard-source pattern as `tmux-reap-pane.sh`: `[[ -f "@lib_claude@" ]] && source` else
`exit 0`, so the raw script still runs under bats. Carries the discriminator:
`AGENT_COMMANDS="${AGENT_COMMANDS:-@AGENT_COMMANDS@}"` (substituted exactly like
`tmux-update-icons.sh`), normalizes the passed `pane_current_command`
(`normalize_wrapped_cmd`), and calls `claude_clear_agent_state` only when the normalized
command is a non-empty non-manifest value. Reads `#{hook_pane} #{pane_current_command}
#{session_name}` from the hook invocation.

### 3. Hook wiring — `config/tmux.conf.tmpl`

- Clear block gains `set-hook -gu pane-shell-prompt`, so `prefix + r` stays idempotent and
  a rebuild with the feature off cannot leave a hook at a GC'd store path (the #647 rule).
- One setter at **index 0** (tmux-og owns 0; tmux-remux sits at `[99]`):
  `set-hook -g pane-shell-prompt 'run-shell -b "{{index .Paths.Scripts
  "tmux-shell-prompt"}} #{q:hook_pane} #{q:pane_current_command} #{q:session_name}"'`
  — `-g` matches the #647 `pane-exited`/`pane-died` wiring (verified firing),
  `run-shell -b` keeps the fork off the server's command queue, `#{q:…}` is house quoting.
  `pane-command-started`/`pane-command-finished` are deliberately **not** wired: the prompt
  event alone is the correct, conservative "back at a prompt" signal and avoids the
  dual-firing cost and the `D`-mark/exit-status ambiguity.
- `config/tmux.conf.nix`: add `"tmux-shell-prompt"` to `scriptNames`, route it through
  `mkScriptWithLibs` (same as `tmux-reap-pane`), and add `@AGENT_COMMANDS@` to its
  substitution list. `config/tmux.conf.reference.nix` in lockstep if the generator/
  reference comparison requires it (plan detail).

### 4. No change to the floor (`assumeDeadAfter`, sweep, `read_pane_state`)

They stay byte-for-byte as the backstop (Q4). The new `pane-shell-prompt` clear and the
floor's withdrawal can both exist: the clear removes files, the withdrawal is a derived
returns-1 on a file that (when the hook has run) no longer exists.

## Non-goals / invariants

- Never a directory sweep from a hook path — one pane id, three unlinks, two option writes.
- No `pane-command-finished` wiring (status/exit-code parsing adds nothing the prompt
  discriminator doesn't already settle).
- No `picker/remotebridge/**` changes (Q2 rides the existing `agentstatus.go` shipper,
  untouched; #657's heal code ownership is respected).
- No float binds, no `pane-border-format` (#648's files).
- `claude_prune_stale_state`, `claude_reap_dead_panes`, `claude_reap_pane` untouched.

## Acceptance criteria

1. Spec + plan committed under `docs/superpowers/specs/` and `/plans/`.
2. bats, scratch server (own `TMUX_TMPDIR` + `-L`, scratch `CLAUDE_STATUS_DIR`), loading
   the real wrapped config, `printf` of real `ESC]133;` sequences:
   - **agent exits to prompt → state cleared** (state files gone, `@claude_status`/
     `@agent_screen` empty),
   - **nested shell prompt inside a still-running agent → not cleared** (`pane_current_command`
     still the agent),
   - **no OSC 133 emitted → floor behaviour unchanged** (floor tests still green, and the
     hook handler never fires).
   The test wires the same `set-hook -g` as the config so firing is proven on the wrapped
   server, not just the function.
3. `nix build .#default`, `nix flake check`, `nix build .#lint` all pass (three separate
   commands).
4. CLAUDE.md "Dead-agent floor" section updated to describe the event-driven clear, the
   event/floor precedence, and the remaining floor coverage (non-OSC-133 shells, old
   remotes).

## Hardware verify (per task doc)

Scratch server: fish pane, seed `panes/<id>` + `screen/<id>` + options, run a foreground
command that exits (or `kill`) → within ~1s the files are gone and options empty. A second
scratch server against the same `CLAUDE_STATUS_DIR` with a colliding pane id cannot clear
the first server's state (session mismatch). A nested `ESC]133;A` emitted from inside a
still-running foreground process does not clear. `TMUX_TMPDIR=/tmp/og-$$`,
scratch-exported `CLAUDE_STATUS_DIR`, `kill-server` when done. Never the live server, never
the real `/tmp/claude-status`.
