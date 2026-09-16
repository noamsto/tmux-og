# Reset window naming/crew display when a window's agents quit (#671)

## Problem

After the last agent in a window exits to a shell, the window keeps its
agent-era identity forever: `@window_ai_name`/`@window_task` (and their
self-report files) keep driving `build_window_label`, and the crew codename
badge (`@crew_name`/`@crew_color`, stamped by the external `dispatch`/`crew`
fan-out harness) keeps rendering everywhere it's read, even though no agent is
left in the window.

## Hard constraint

Never unset `@crew_name`/`@crew_color` (session/window options the dispatcher
uses to key worker-window occupancy and reap — see `crew occupants`) or
dispatch's per-pane `@crew_role`/`@crew_state`/`@crew_role_color`. This feature
only clears tmux-og-owned state and display-gates rendering; it never writes
to a dispatcher-owned option.

## Definition

"Window has no live agent" := no pane in the window whose `pane_current_command`
(after `normalize_wrapped_cmd`) is in `$AGENT_COMMANDS` — the same manifest
test `arm_agent_detect` (tmux-update-icons.sh) and `tmux-shell-prompt.sh`
already use. `@bridge_win` mirror windows are always excluded (daemon-owned,
two-writer race — CLAUDE.md).

`@ai_naming` (WORKER_TASK.md's "respect the `@ai_naming` gate"): a non-issue
by construction, not a separate gate to add. `@window_ai_name`/`names/<pane>`
are only ever populated when `@ai_naming` is on (`claude-plugin/scripts/status.sh`'s
seeding path is gated on it) — with it off, there is never anything there to
clear, so `claude_clear_window_naming`'s unconditional-when-not-manual
name/task clear is naturally a no-op on an `@ai_naming`-off install.

## New shadow option: `@window_has_agent`

A window option tmux-og owns outright (unlike `@crew_name`, no shadow-compare
needed — we write it directly and read our own last-written value back via the
same batched `list-panes` call, exactly like `@window_task`/`@window_ai_name`
already do). `"1"` when the window has a live agent, `""`/unset otherwise.
Never set for `@bridge_win` windows (left untouched).

This is the single fact every display consumer gates on. `@crew_name` itself
is never touched.

## Manual-rename durability: `@window_manual_name`

Plan-critic finding (blocking, round 1): gating the naming clear on
`#{automatic-rename}` is unsound, because `tmux-update-icons.sh:623-629`
**already forces `automatic-rename` back ON for every non-bridge window, every
tick**, regardless of why it went off — including a genuine user
`rename-window`. That reassert exists to self-heal tmux-remux's `new-window -n`
(which also flips it off), and it doesn't distinguish that case from a real
manual rename. Consequence if left as originally planned: a manual rename
reverts within ~1s regardless of this feature, so "user-renamed window keeps
its name" (an explicit acceptance criterion) is untestable and the gate itself
is on a value tmux-og is actively fighting.

Fix: add a second, durable, tmux-og-owned marker, `@window_manual_name`
("1"/unset), stamped only by the user's own rename gesture and never touched
by the automatic-rename reassert logic:

- `config/tmux.conf.tmpl` + `.reference.nix`, the local (non-bridge) arm of
  the `prefix + ,` rename bind (`bind-key -N 'Rename current window' , ...`):
  change `{ command-prompt -I'#W' { rename-window -- '%%' } }` to
  `{ command-prompt -I'#W' { rename-window -- '%%' ; set-window-option @window_manual_name 1 } }`
  (no `-t`: runs against the window the keypress targeted, same as
  `rename-window` itself).
- `scripts/tmux-update-icons.sh`: add `#{@window_manual_name}` to the batched
  `list-panes -a -F` format, track `win_cur_manual[$wkey]` (first-pane-wins,
  same as the other cached window options), and change the reassert-on-tick
  condition at line ~627 from
  `elif [[ ${win_cur_rename[$wkey]:-} != 1 ]]; then` to
  `elif [[ ${win_cur_rename[$wkey]:-} != 1 && ${win_cur_manual[$wkey]:-} != 1 ]]; then`
  — a manually-named window's `automatic-rename off` is now left alone; a
  tmux-remux-restored window (never manually renamed, `@window_manual_name`
  unset) is still self-healed exactly as today.

This is a real, if narrow, fix to a pre-existing gap (manual renames were not
actually durable before this feature), not scope creep invented for its own
sake — it's required for the "user-renamed window → name kept" acceptance
criterion to be true at all. No mechanism to return a window to automatic
naming is added (none exists today either); out of scope for #671.

## Changes

### 1. `scripts/lib-claude.sh` — new function `claude_clear_window_naming`

```
claude_clear_window_naming WINDOW_TARGET MANUAL_NAME PANE_ID...
```

- Always: clears `@window_has_agent` on the window (`tmux set -qw`), and
  removes `issues/<pane>` for every `PANE_ID` — issue self-reports "die with
  the pane or CC session" per the existing CLAUDE.md bullet, and losing their
  agent is exactly that death, independent of naming/display mode.
- Only when `MANUAL_NAME != 1` (the window has never had `@window_manual_name`
  stamped by the user's own `prefix + ,` rename): clears
  `@window_ai_name`/`@window_task` (window options) and removes
  `names/<pane>`/`tasks/<pane>` for every `PANE_ID`. When it's `1` (user
  renamed), naming state is left untouched — nothing to gate the display of
  (the badge/naming pipeline doesn't drive a manually-set window name anyway),
  and nothing to fight the user over.
- Callers pass the window's already-known `@window_manual_name` value
  (backstop has it from the batched read already; the event hook forks one
  `display-message` for it, folded with the `@bridge_win` read below) rather
  than the function re-deriving it, so this stays fork-free on the hot path.

Document the issues-clear decision inline with a comment (the CLAUDE.md
"Issue self-report" bullet already states files die with the pane/session —
this makes "no live agent in the window" a second death trigger).

### 2. `scripts/tmux-shell-prompt.sh` — event trigger

Hook wiring gains two more format args: the firing pane's window id
(`#{q:window_id}` — resolves in the hook-pane's context the same way
`#{qs:session_name}` already does) and `#{q:@window_has_agent}` (the window's
*current* stamped value, read for free in the same hook-context substitution
— no extra fork to fetch it).

`pane-shell-prompt` fires on **every prompt redraw of every shell pane**, not
just on agent exit (plan-critic non-blocking note, round 1) — unlike
`claude_clear_agent_state`, which already short-circuits fork-free on a
missing state file, a naive window-wide scan here would fork on every prompt
draw in every plain shell pane in the fleet. So gate on the passed-through
`@window_has_agent` first, before any new fork:

0. If the new 5th arg (`@window_has_agent` at hook-fire time) is not `1`:
   return immediately — either already cleared, or the window never had an
   agent to begin with (nothing new to do; the backstop is the ground truth
   for the rare case this is stale).
1. Otherwise, one combined fork:
   `tmux display-message -p -t "$window_id" '#{@bridge_win}|#{@window_manual_name}'`.
   Bail if `@bridge_win` is set (mirror windows are daemon-owned).
2. `tmux list-panes -t "$window_id" -F '#{pane_id}|#{pane_current_command}'` —
   one fork. Normalize each command, check against `$AGENT_COMMANDS`.
3. If any pane still runs an agent: nothing to do, return.
4. Otherwise: call
   `claude_clear_window_naming "$window_id" "$manual" <pane ids from step 2>`,
   then force reflow. Use the **guarded-variable** form
   (`scripts/claude-status-update.sh:73`'s shape — `REFLOW_BIN="@reflow@"`
   near the top of the script), not the bare `@reflow@ ... &` call some other
   scripts use, so an unsubstituted placeholder (the raw-script test seam
   other scripts in this repo rely on, even though this particular hook's own
   bats coverage runs the substituted wrapped-tmux copy — see §5b for the
   real substitution this placeholder needs) degrades explicitly rather than
   trying to exec a literal `@reflow@`. **Do not** copy
   `claude-status-update.sh:95`'s exact `return 0` form, though — that one
   sits inside a function (`window_stamp()`); `tmux-shell-prompt.sh` is a
   top-level script, where `return` is an error and (as the last command of an
   `&&`, under `set -e`) would abort the hook non-zero. Use an explicit `if`:
   ```bash
   if [[ $REFLOW_BIN != @* ]]; then
       "$REFLOW_BIN" "${3:-}" --force >/dev/null 2>&1 &
   fi
   ```

Reachability note: the event path only fires anything (past step 0) when
`@window_has_agent` was already `1` — and the **only** writer of that `1` is
`tmux-update-icons`' `status-format[0]` `#()` poller. On a host whose only
clients are control-mode remote-bridge transports, that poller never ticks at
all (CLAUDE.md, #603/#596), so there the event path is inert and the backstop
(§3, which the bridge's own daemon session doesn't run either — no local
poller — but the *launcher* side does) is the only path that can ever apply.
Same limitation class as every other `status-format[0]`-driven mechanism in
this repo; worth one sentence in the CLAUDE.md bullet (§9), not a new
mechanism.

### 3. `scripts/tmux-update-icons.sh` — backstop

Plan-critic finding (blocking, round 2): bash has no nested/multi-dimensional
arrays, so `win_panes[$wkey]+=(...)` on a `-A` (associative) array is a hard
error, and the round-1 draft's array framing was unexecutable — it also
self-contradicted its own "space-separated" description elsewhere. Fixed
below to follow the exact pattern the file already uses for `win_procs`
(`tmux-update-icons.sh:278-282`): a plain space-joined string, safe because
bare pane ids are digits-only.

In the existing batched-read loop (the one that already builds `win_procs`):
- Add `win_panes` to the `declare -A pane_to_win win_procs win_pane_path ...`
  line at ~189-190 (alongside `win_cur_has_agent win_cur_manual`, also new —
  without `declare -A` a `$wkey`-subscripted assignment is evaluated as an
  *arithmetic* subscript and silently collapses every window onto index 0).
- Accumulate `win_panes[$wkey]="${win_panes[$wkey]:+${win_panes[$wkey]} }${pane_id#%}"`
  — same loop, no new fork, same shape as the `win_procs` accumulation later
  in the loop. Placement matters: put it **beside `pane_to_win["${pane_id#%}"]=...`**
  (line ~233), i.e. before the `[[ -z $proc ]] && continue` guard at line ~277
  that the `win_procs` dedup sits behind — not literally "next to `win_procs`".
  A pane with an empty `pane_current_command` (e.g. a `remain-on-exit` corpse)
  hits that `continue` and would otherwise never enter the pane-id list, and
  its `names`/`tasks`/`issues` files would survive the clear.
- Add `#{@window_has_agent}` and `#{@window_manual_name}` to the batched
  `list-panes -a -F` format string, placed as fixed middle fields **before**
  `#{@window_task}` (which must stay last — it's free-form and unsanitized,
  per the existing comment block at lines 198-221) and update the matching
  `read -r ... cur_has_agent cur_manual ... cur_task` variable list at the
  same position. Both are closed "1"/"" tokens, safe as fixed fields like
  `@crew_name`. Read into `win_cur_has_agent[$wkey]`/`win_cur_manual[$wkey]`
  on first-pane-wins, same as the other cached window-option reads.

In the per-window loop (next to the existing `@crew_seen` shadow-compare
block, which is the established site for "compare a computed window-level
fact against the read-back option and write+reflow only on change"):

```
has_agent=""
if [[ ${win_cur_bridge[$wkey]:-} != 1 ]]; then
    # shellcheck disable=SC2086  # win_procs is a space-joined string; word-split intentionally
    for p in ${win_procs[$wkey]:-}; do
        normalize_wrapped_cmd "$p"
        case " $AGENT_COMMANDS " in *" $REPLY "*) has_agent=1; break ;; esac
    done
fi
if [[ $has_agent != "${win_cur_has_agent[$wkey]:-}" ]]; then
    if [[ -n $has_agent ]]; then
        tmux set -qw -t "$target" @window_has_agent 1
    else
        # shellcheck disable=SC2086  # win_panes is a space-joined string of bare pane ids; word-split intentionally into positional args
        claude_clear_window_naming "$target" "${win_cur_manual[$wkey]:-}" ${win_panes[$wkey]:-}
    fi
    sess_need_reflow[$s]=1
fi
```

(Each `# shellcheck disable=SC2086` binds to the single command immediately
below it — the repo's own precedent for this exact construct is
`scripts/lib-icons.sh:71-72` — so it must sit directly above the `for` line
and directly above the `claude_clear_window_naming` call, not once for the
whole block.)

This writes `@window_has_agent`/runs `claude_clear_window_naming` via a direct
`tmux set`/function call on the rare transition tick, **not** batched into the
loop's `tmux_cmds` string (flushed via `tmux source -` at line ~640, the
file's usual steady-state-cheap mechanism) — deliberate, since
`claude_clear_window_naming` itself issues multiple `tmux set`/`rm -f` calls
that don't fit the single-line `tmux source -` batch format, and this whole
branch only runs on a genuine `has_agent` transition, not every tick.

`@bridge_win` windows never enter the `has_agent=1` branch and their cached
`win_cur_has_agent` is never populated (field stays empty for them in
practice), so this whole block is a no-op for mirrors — matches "skip them".

This reuses the loop's existing batched read and existing forced-reflow
mechanism (`sess_need_reflow[$s]=1` → the loop tail's `@reflow@ ... --force`)
— no new fork on the steady-state tick, exactly like the `@crew_seen` block it
sits beside.

#### Manual-rename reassert fix (see "Manual-rename durability" above)

Also in this script: change the automatic-rename reassert condition at
line ~627 to respect `@window_manual_name`, as detailed above.

### 4. `scripts/tmux-reflow-windows.sh` — gate the multi-line grid badge

Add `#{@window_has_agent}` to `FMT` (a token-safe fixed field, placed next to
`#{@crew_name}` and, like it, before the free-form `#{@window_task}` which
must stay last), and a new `hasagent` read variable (a short abbreviation
trips the repo's `typos` pre-commit hook, so spell the whole word) at the
matching position in the `while IFS='|' read -r ...` list (line ~157).

Gate **inside the `else` (non-bridge) arm** at lines 193-212 — i.e. literally
guarded by `[[ $bridge != 1 ]]`, not "after the branch" (the bridge arm at
lines 170-192 unconditionally sets `crew="$bcrew"` and must be left alone —
mirrors are daemon display-owned and out of scope here). Concretely, inside
that `else` block:

```
[[ $hasagent == 1 ]] || crew=""
```

added anywhere after `build_window_label`'s two calls in that same arm (it
only needs to run before the shared `win_crew[$idx]=...`/width-measurement
block at lines ~241-255 that both arms fall through to). No further changes
needed there — an empty `crew` already suppresses the badge
(`if [[ -n $crew ]]`).

### 5a. `config/tmux.conf.tmpl` + `config/tmux.conf.reference.nix` — hook wiring + single-line badge + manual-rename bind

Edited together (frozen-oracle rule in the reference file's header — the
nix-side helper `bridgeOpt` lives in `config/tmux.conf.reference.nix:134`,
**not** `config/tmux.conf.nix`; get this right, CLAUDE.md:365 has the same
stale pointer). No new `config/tmux.conf.nix` **parameter** needed for these
two edits — they reuse existing option names — but see §5b for a *build*-level
edit this feature does need.

- `pane-shell-prompt` hook: append `#{q:window_id}` and `#{q:@window_has_agent}`
  as 4th/5th format args, in both files.
- The `prefix + ,` rename bind's non-bridge arm: append
  `; set-window-option @window_manual_name 1` inside its `command-prompt`
  block, per "Manual-rename durability" above.
- `status-format[1]`'s single-line crew badge fragment: wrap the existing
  `#{?${bridgeOpt "crew_name"},<badge>,}` (`.tmpl`:
  `#{?{{index .BridgeOpt "crew_name"}},<badge>,}`) condition so a **non-bridge**
  window additionally requires `@window_has_agent`:

  ```
  #{?#{&&:#{?#{@bridge_win},1,#{@window_has_agent}},${bridgeOpt "crew_name"}},<badge>,}
  ```

  For a bridge window the inner ternary collapses to literal `1`, so the `&&`
  reduces to exactly today's `${bridgeOpt "crew_name"}` check — bridge
  behavior is provably unchanged (verified: `format_skip` walks nested
  `#{…}` when splitting `&&`'s operands, and the identical nesting shape
  already ships at `.tmpl:352` for the PR-number ternary). For a local window
  it additionally requires `@window_has_agent`.

### 5b. `config/tmux.conf.nix` — substitute `@reflow@` into `tmux-shell-prompt`

Plan-critic finding (blocking, round 1): `tmux-shell-prompt` is built by
`mkScriptShellPrompt` (`config/tmux.conf.nix:232-238`), whose
`builtins.replaceStrings` list is currently only
`["@lib_claude@" "@AGENT_COMMANDS@"]`. §2's `@reflow@ ... &` call would ship
as a literal, never-substituted placeholder — `[[ $REFLOW_BIN == @* ]]` guards
against exactly that, but it means the event path's forced reflow silently
**never runs**, and nothing else recovers it (the backstop's own transition
compare has already converged on the same values the event path just wrote,
so it never re-fires `sess_need_reflow`). Fix: extend `mkScriptShellPrompt`'s
`replaceStrings` pair to
`["@lib_claude@" "@AGENT_COMMANDS@" "@reflow@"]` /
`["${lib-claude}" agentCommands "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"]`.

### 6. `picker/main.go` — session/window picker rows

Plan-critic finding (blocking, round 1): this file has a **strict** field-count
guard (`parseWindowPaneRows`, `if len(parts) != 34 { continue }`) and a test
helper constant (`picker/main_test.go:254`, `const n = 34`) that both must move
in lockstep with the format string, or every row is silently dropped
(`collectWindows` returns nothing, the window picker renders empty) and
`go test ./...`/`picker-go-tests` fails on the now-34-vs-35 mismatch.

- Add `#{@window_has_agent}` to the `list-panes -a -F` format in
  `collectWindows`, appended **last**, after `#{@bridge_pr_auto_merge}` — new
  index 34 (0-based, 35th field).
- Bump the guard in `parseWindowPaneRows` from `len(parts) != 34` to
  `len(parts) != 35`.
- Bump `const n = 34` to `const n = 35` in `picker/main_test.go`'s
  `windowPaneRow` helper.
- Parse the new field (`hasAgent := field(parts, 34) == "1"`) and, only when
  `!bridgeWin`, blank `crewName`/`crewColor` when `!hasAgent`. Bridge rows are
  untouched (they already source `crewName`/`crewColor` from the
  `@bridge_crew_*` fields at their own indices, unconditionally).

Plan-critic finding (blocking, round 2): bumping `n` to 35 alone breaks
`TestParseWindowPaneRowsLocalUnchanged` (`picker/main_test.go:310`) — a
non-bridge row that asserts `wi.crewName == "local-crew"` (line 328), whose
padded 35th field is `""` (no live agent) and would now read `crewName == ""`.
Fix in the same change: that test's `windowPaneRow(...)` call supplies only
28 positional fields today (indices 0-27; `windowPaneRow` zero-pads the rest)
— appending a bare `"1"` lands it at index 28 (`bridgeHost`), not 34. Append
**six empty strings then `"1"`** (`"", "", "", "", "", "", "1"`) to reach
index 34 (it's the "local window renders its own values" test, i.e. the
live-agent case). Same six-empty-then-value arithmetic applies to the new
sibling test below.
Add a new sibling test asserting the actual regression this feature adds: a
non-bridge row with the 35th field `""` (or simply omitted, since padding
already yields `""`) blanks `crewName`/`crewColor` while `labelID`/`branch`
are unaffected.

### 7. `picker/statusline/main.go` — top-right status line

- Add `#{@window_has_agent}` to `volatileFields`.
- Parse into a new field on the fetch result struct.
- Gate the existing `if a.crewName != "" { ... }` badge block (the non-bridge
  arm — the bridge arm above it, keyed on `a.bridgeWin`, is untouched) with
  `&& a.windowHasAgent == "1"`.

Plan-critic finding (blocking, round 2): this gate breaks
`TestSessionSegmentCrewBadge` and `TestSessionSegmentCrewBadgeColorFallback`
(`picker/statusline/main_test.go:52,66`), both of which build an `args{crewName:
"coral", ...}` literal with no `windowHasAgent` field and assert the exact
rendered badge string is present. Fix in the same change: set
`windowHasAgent: "1"` in both existing `args` literals (lines ~55, ~69), and
add a new case mirroring `TestSessionSegmentNoCrewBadge` (line ~78) with
`crewName: "coral", windowHasAgent: ""` asserting the badge is absent. The
bridge-arm tests (`TestSessionSegmentBridgeWinStopsAtPill`,
`TestSessionSegmentBridgeFullIdentity`) are unaffected and need no change.

### 8. Pane borders (dispatch-owned) — cannot be gated cleanly, document only

`pane-border-style`/`pane-border-format`/`pane-border-status top` on a
dispatcher-lead or role-grid window are set by `dispatch`/`crew` itself as
**window/pane-scope option overrides** (`adapters/core/dispatch.sh`,
`dispatch-resume.sh`, `dispatcher.sh` in the external `dispatcher` repo) —
static strings baked with `#{@crew_name}`/`#{@crew_role}`/`#{@crew_state}`
inline, no conditional. tmux-og's own `setw -g pane-border-style`/
`pane-border-format` (in `tmux.conf.tmpl`/`reference.nix`) is the FALLBACK a
window-scope override always wins over, and it already only applies to
`@bridge_*`-fed mirror borders — it has no reach into a window dispatch has
overridden. There is no tmux-og-owned seam to gate this display-side without
either (a) writing to dispatch's own `pane-border-format`/`pane-border-style`
options (explicitly forbidden — "do not unset dispatch's window options",
and rewriting them to a different string is the same class of write), or
(b) dispatch itself consulting `@window_has_agent` in the format string it
sets. **Not implemented here.** Document as a known limitation in the PR body
and propose the dispatcher-side follow-up: dispatch's own
`pane-border-format` strings could consult `#{@window_has_agent}` (a tmux-og
option, safe for dispatch to read) the same way tmux-og's `bridgeOpt`
conditions do, once tmux-og ships it.

### 9. CLAUDE.md

- "Name self-report files" bullet: note it's cleared (file + `@window_ai_name`)
  when the window's last agent exits and the window has never been manually
  renamed (`@window_manual_name` unset) (#671).
- "Task self-report files" bullet: same note for `@window_task`.
- "Issue self-report files" bullet: same note — issues always clear on
  window-wide agent exit, regardless of manual naming.
- `tmux-update-icons` row (Script Roles table): one clause noting the new
  `@window_has_agent`/`@window_manual_name` shadow writes + naming-reset
  backstop + the amended automatic-rename reassert condition.
- One new bullet under Key Conventions (sibling to "Dead-agent detection")
  describing the `@window_has_agent` mechanism end to end: the manifest test,
  the event trigger (`pane-shell-prompt` → `tmux-shell-prompt.sh`), the
  backstop (`tmux-update-icons`' per-window loop), the new
  `@window_manual_name` marker and how it makes a manual `prefix + ,` rename
  durable against the existing automatic-rename reassert, issues always
  clearing, every display consumer gated (reflow grid, single-line
  status-format[1], both pickers, statusline), `@bridge_win` exclusion, the
  documented pane-border limitation (§8), and the reachability note from §2
  (the event path only ever fires past its early-return when the backstop
  poller has already run at least once — inert on a control-mode-only host,
  same limitation class as every other `status-format[0]`-driven mechanism
  here).

## Tests (bats, scratch server per CLAUDE.md conventions)

Plan-critic finding (blocking, round 1): `flake.nix` registers **one explicit
check derivation per `.bats` file** (no `tests/*.bats` auto-discovery), so a
brand-new, unregistered suite would exist in the tree and never run under
`nix flake check` — untested tests. Append new cases into **already-registered**
suites instead of inventing a new file, matching the harness shape each already
has (own `TMUX_TMPDIR`/`-L` socket/`CLAUDE_STATUS_DIR`):

- Event-path cases (1, 2, 3, 5, 6 below) → append to
  `tests/pane-shell-prompt.bats` (already covers OSC 133 / `claude_clear_agent_state`
  with exactly this harness). Its `setup()` currently does
  `mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}`
  — no `names` — add `names` to that brace list before asserting on
  `names/<pane>` removal (case 1).
- Backstop case (4) → append to `tests/update-icons-all-windows.bats` (already
  registered, already drives `tmux-update-icons` over a full window set).
  That suite's `sed` pipeline (lines ~39-44) substitutes `@lib_icons@`,
  `@lib_claude@`, `@reflow@`, `@MAX_ICONS@` but leaves `@AGENT_COMMANDS@`
  literal, so the manifest test in §3 would never match — add
  `export AGENT_COMMANDS="claude"` (or equivalent) to that suite's `setup()`;
  it already builds a `claude`-named fixture shell (line ~59) to key off.
- Static single-line status-format[1] text assertion → **not**
  `tests/pane-border-format.bats` (plan-critic correction, round 3: that suite
  starts a live scratch server against `/dev/null` and reads back
  `pane-border-format` — it has no access to the generated conf text at all,
  so there's nothing to grep there). Instead add a small `grep`-based check
  derivation in `flake.nix` modeled on `float-conf-assertions`
  (`flake.nix:476-507`), which already does exactly this shape — a derivation
  with `CONF = tmuxConfig.tmuxConf` and one `grep` over it. This is not a new
  unregistered `.bats` suite (the earlier "must append to a registered suite"
  constraint was about `.bats` auto-discovery, which doesn't apply to a
  `flake.nix` derivation defined and checked in the same file).

`tests/reflow.bats` currently has **no** crew-badge fixtures to reuse (grep
confirms) — case 1's grid-badge assertion needs new fixtures authored from
scratch there (or inline in whichever suite ends up owning it), including
driving `tmux-reflow-windows` **directly** rather than relying on a live
poller tick — `tmux-reflow-windows` exits early when `#{client_width}` is
empty, and a `new-session -d` scratch server (the `pane-shell-prompt.bats`
pattern) has no attached client, so the test must either attach one or invoke
the reflow binary explicitly and read `status-format[1..]` back. Pin the
owning suite before starting: `tests/pane-shell-prompt.bats` runs the
**wrapped** tmux (`flake.nix`'s `TMUX_BIN = tmuxConfig.tmux-wrapped`) and does
**not** have `tmux-reflow-windows` on `PATH` as a bare name — either grep the
store path out of the generated conf the way `tests/test-display.sh:131`
already does, or add the `tmux-reflow-windows` derivation to that check's
`nativeBuildInputs` in `flake.nix`. Decide and state which before writing
case 1. Either way, invoking it directly also requires an explicit numeric
width as `$2` (`tmux-reflow-windows.sh` exits early on a non-numeric `WIDTH`)
— call it the way `tests/test-display.sh:132` does, e.g.
`"$REFLOW_BIN" s 200 --force`, not just a session name.

1. Agent exits via a real OSC 133 prompt redraw (single-pane window) → in one
   assertion: `@window_ai_name`/`@window_task` window options empty,
   `names/<pane>`/`tasks/<pane>`/`issues/<pane>` files gone,
   `@window_has_agent` empty, `@crew_name` **still set** (hard constraint),
   crew badge absent from the reflow-rendered grid line for that window
   (reflow invoked directly per the note above).
2. Second pane in the same window still runs a live agent (manifest command)
   → after the first pane's prompt fires, nothing is cleared, `@window_has_agent`
   stays `1`, badge still renders.
3. Agent relaunches in the same window after clearing → next
   `tmux-update-icons` tick sets `@window_has_agent` back to `1` and the badge
   reappears, with no re-stamp of `@crew_name`/`@crew_color` needed (dispatch
   never touched again).
4. A shell with no OSC 133 support (no `pane-shell-prompt` fires) → the
   `tmux-update-icons` backstop still clears/hides within one invocation once
   the pane's foreground command is no longer an agent.
5. User manually renames the window (`prefix + ,` bind, i.e.
   `rename-window` + the new `@window_manual_name` stamp) before the agent
   exits → agent exit still clears `issues/<pane>` and `@window_has_agent`
   (badge hides), but `@window_ai_name`/`@window_task` and their files are
   left untouched, and — the actual regression this closes — a subsequent
   `tmux-update-icons` tick does **not** flip `automatic-rename` back on for
   that window (assert `#{automatic-rename}` stays `0` and the window's real
   name is unchanged across a tick, proving the reassert-condition amendment
   in §3 actually holds).
6. `@bridge_win` mirror window with an agent-shaped `pane_current_command` →
   entirely untouched by both the event hook and the backstop:
   `@window_has_agent` never written, no naming state cleared.

## Verification

- `nix build .#default`
- `nix flake check` (bats, incl. the extended suites above, + Go
  (`picker-go-tests`, incl. the bumped `n = 35` fixture constant) +
  `tmux-format-delimiter-assertions` over the new FMT fields)
- `nix build .#lint` (shellcheck over the new bash, incl. the disable
  documented in §3)

## Coordination

Rebase onto `main` before opening the PR — a parallel worker (#663) is
touching theme detection hooks / `tmux-apply-theme-colors` / config-load
run-shells, which this plan does not touch.
