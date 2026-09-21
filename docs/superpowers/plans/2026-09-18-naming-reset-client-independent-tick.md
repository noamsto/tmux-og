# Naming reset on the client-independent tick (#692)

## Problem

`@window_has_agent` is written only by `tmux-update-icons`' per-window loop,
which runs from the `status-format[0]` `#()` caller. tmux returns `0` from
`status_line_size()` for a control-mode client, so on a host whose only clients
are remote-bridge transports that loop never runs (the #603 class). Both #671
triggers are gated on that option:

- **Event** — `scripts/tmux-shell-prompt.sh` returns early unless the
  passed-through `@window_has_agent` is `1` (and the option is never written,
  so it is never `1`).
- **Backstop** — the per-tick loop that would detect the occupancy transition
  never runs.

On such a host the window-wide naming reset can never fire and
`@window_ai_name` / `@window_task` outlive the agent that set them. That is
invisible locally (no status line is ever drawn) but it reaches a mirror:
`picker/remotebridge/daemon/windowlabels.go` ships `@window_label_rest_long`,
which reflow composed from those two options **without gating them on
`@window_has_agent`** (`scripts/tmux-reflow-windows.sh`'s `build_window_label`
consumes `@window_ai_name`/`@window_task` unconditionally; only the crew badge
is gated).

**Violated invariant:** every window-scoped naming consumer must reflect live
agent occupancy, on every host — including one whose only client is a
control-mode bridge. Today occupancy is derived from a client-gated poller.

## Mechanism

The reset has two halves of very different safety:

- **option writes** — `@window_has_agent ""`, `@window_ai_name ""`,
  `@window_task ""` — per-server (`tmux set -w` on this server's own windows),
  safe on a client-independent timer.
- **file deletions** — `issues/`, `names/`, `tasks/` under
  `CLAUDE_STATUS_DIR` — a bare `/tmp` path shared by every tmux server on the
  machine, so unsafe there (a scratch server's pane ids collide with the real
  server's). That is why the whole call is client-gated today.

The fix runs the **option half** from the client-independent sweep
(`arm_agent_detect`, driven by the `@og-sweep-tick` monitor hook) and gates the
per-tick task/name file reads on live occupancy so a lingering self-report file
cannot re-stamp an option the sweep just cleared.

Two design constraints were verified against the repo and are load-bearing:

1. **The sweep must enter on stale naming, not only on a `1→0` option
   transition.** On the reported host `@window_has_agent` was *never written*,
   so it is never `1` and an option-transition-gated clear skips the very
   window that has the stale name. The sweep therefore reads
   `@window_ai_name`/`@window_task` in the same row and clears whenever the
   window has no live agent **and** carries either a set option or stale naming
   (a manually-named window's naming is not stale and is never cleared).
2. **The file half cannot stay coupled to the option transition.** The
   per-tick clear fires only on a `1→0` transition of `@window_has_agent`. Once
   the sweep clears that option first (always on a bridge-only host; a race on
   a host with a client), a later per-tick pass sees
   `has_agent == win_cur_has_agent == ""` and never calls
   `claude_clear_window_naming`, stranding `names/`/`tasks/`/`issues/` under
   the shared dir — and a stranded `names/`/`tasks/` file is exactly what the
   now-ungated read would re-stamp when a real client next attaches. So the
   sweep marks a cleared window `@window_naming_dirty 1` **before** it clears
   the options; the per-tick loop consumes that mark on its next no-agent pass
   (where the shared-dir deletion is client-gated) and unsets it last, after
   the files are gone. The marker is **not** occupancy state — occupancy is
   still the same one-round-trip derivation from the sweep's existing
   `list-panes -a` rows.

The forced reflow on the sweep is load-bearing too: a control-mode client
reports a real `client_width` (measured: 197 for the control clients on this
box), so reflow runs on a bridge-only host, and it is reflow that recomposes
`@window_label_rest_long` from the cleared options. The option write alone does
not update the label the daemon ships.

## Changes

### 1. `scripts/lib-claude.sh` — split the option half out of `claude_clear_window_naming`

Add `claude_clear_window_display WINDOW_TARGET MANUAL_NAME`, the
**deletion-free** option half of the #671 reset:

```bash
# claude_clear_window_display WINDOW_TARGET MANUAL_NAME
# The option half of the #671 reset, safe on a client-independent timer: no
# file deletions, because CLAUDE_STATUS_DIR is a bare /tmp path shared by every
# tmux server on the machine, and the sweep that calls this runs on every
# server. Always clears @window_has_agent; clears @window_ai_name/@window_task
# only when MANUAL_NAME != 1, the same rule claude_clear_window_naming applies
# to the file half. @crew_name/@crew_color are dispatcher-owned and never
# touched.
claude_clear_window_display() {
	local target="$1" manual="$2"

	tmux set -qw -t "$target" @window_has_agent ""
	[[ $manual == 1 ]] && return 0
	tmux set -qw -t "$target" @window_ai_name "" \; set -qw -t "$target" @window_task ""
}
```

Refactor `claude_clear_window_naming` to delete its files, delegate the option
writes to the new function, and consume the dirty mark **last** (the mark
protects the shared-dir deletion as a whole; clearing it before the `rm`s
would strand them on a failed `rm` or a crash):

```bash
claude_clear_window_naming() {
	local target="$1" manual="$2"
	shift 2
	local id

	for id in "$@"; do
		id="${id#%}"
		rm -f "$CLAUDE_ISSUES_DIR/$id"
	done
	claude_clear_window_display "$target" "$manual"
	if [[ $manual != 1 ]]; then
		for id in "$@"; do
			id="${id#%}"
			rm -f "$CLAUDE_NAMES_DIR/$id" "$CLAUDE_TASKS_DIR/$id"
		done
	fi
	# Consume the client-independent sweep's mark (#692): it cleared the options
	# but must not delete from the shared /tmp state dir, so this — the one path
	# that can — is what discharges the deletion once a real client is present.
	tmux set -qw -t "$target" @window_naming_dirty ""
}
```

Update the `claude_clear_window_naming` doc comment to name the shared helper,
the dirty mark, and the two callers it keeps (the event hook and the per-tick
backstop).

### 2. `scripts/tmux-update-icons.sh` — occupancy on the sweep

**a. A `REFLOW_BIN` test seam** (the new code and the loop tail both need it):

```bash
# Store path to tmux-reflow-windows. Substituted at Nix build time; the loop
# tail and the #692 occupancy sweep both force a reflow from here, and
# ${REFLOW_BIN:-...} lets tests inject a real path (same seam shape as
# AGENT_DETECT_BIN above). An unsubstituted @reflow@ disables the forced
# reflow rather than exec'ing a literal placeholder.
REFLOW_BIN="${REFLOW_BIN:-@reflow@}"
```

Replace the loop tail's bare `@reflow@ "${sess_name[$s]}" --force …` with
`"$REFLOW_BIN" "${sess_name[$s]}" --force …` (identical behaviour when
substituted).

**b. Widen `arm_agent_detect`'s one `list-panes -a` round-trip** from
`#{pane_id}|#{pane_current_command}|#{pane_pipe}` to:

```
#{pane_id}|#{pane_current_command}|#{pane_pipe}|#{window_id}|#{@bridge_win}|#{@window_has_agent}|#{@window_manual_name}|#{@window_ai_name}|#{s/[|]/ /:@window_task}|#{session_name}
```

`#{session_name}` is last because it may contain `|`; `#{window_id}` is the
row's canary (only a real window id matches `^@[0-9]+$`, and the one field
ahead of it that is not a closed token — `#{pane_current_command}` — can in
principle contain a `|`, which shifts the canary to something that fails the
match). `@window_task` is free-form, so its row copy runs through tmux's
`s/[|]/ /` substitution to keep it pipe-free (same device the bridge daemon
uses for its free-form fields; the bracket expression is load-bearing). This
writes a literal space into `s/[|]/ /` — no tab, so the repo's
`tmux-format-delimiter-assertions` is satisfied.
`claude_reap_dead_panes "$rows"` only parses the first field, so the widening
does not affect it.

**c. Fold the per-window occupancy accumulation into the existing per-pane
loop** (one pass, no new round-trip, no new occupancy state). Read the extra
fields, fail closed on a row whose window id is not `@N`, then keep the
existing arm/stamp work exactly as it is:

```bash
local -A win_has=() win_cur=() win_manual=() win_bridge=() win_sess=() win_ai=() win_task=()
local pid cmd piped wid bridge ha manual ai task sname
while IFS='|' read -r pid cmd piped wid bridge ha manual ai task sname; do
	[[ -n $pid ]] || continue
	# Fail closed on a shifted row: only a real window id may claim that shape.
	if [[ $wid =~ ^@[0-9]+$ ]]; then
		win_cur[$wid]="$ha"
		win_manual[$wid]="$manual"
		win_bridge[$wid]="$bridge"
		win_sess[$wid]="$sname"
		win_ai[$wid]="$ai"
		win_task[$wid]="$task"
	fi
	normalize_wrapped_cmd "$cmd"
	case " $AGENT_COMMANDS " in *" $REPLY "*) ;; *) continue ;; esac
	[[ $wid =~ ^@[0-9]+$ ]] && win_has[$wid]=1
	((stamp)) && printf '%s\n' "$CLAUDE_NOW" >"$CLAUDE_LIVE_DIR/${pid#%}"
	[[ $piped == 0 ]] || continue
	((arm)) && tmux pipe-pane -o -t "$pid" "$AGENT_DETECT_BIN ${pid#%}"
done <<<"$rows"
```

The old `((arm || stamp)) || return 0` early-return is removed: with neither
arming nor stamping the loop now only does the fork-free occupancy match, and
the `.sweep` write stays strictly after the loop as today. (`win_ai`/`win_task`
being absent from `$row` fields for a shifted row is the fail-closed case —
they stay unset, so no stale-naming clear is attempted for that window.)

**d. Reconcile occupancy from the accumulated maps — sweep caller only.** The
per-tick caller ($1 empty) already owns this transition (with the file
deletions), so gating on a non-empty `$1` keeps the 1s path unchanged and makes
the sweep the sole client-independent writer:

```bash
# #692: on the client-independent sweep, make @window_has_agent track live
# occupancy and reset naming display on any window that has no live agent but
# still carries naming state. Entering only on a 1->0 option transition would
# miss the reported host outright: @window_has_agent is never written there, so
# it is never 1 and the stale @window_ai_name/@window_task would never clear
# clear. A manually-named window's naming is not stale and is
# left alone, matching claude_clear_window_display. The per-tick caller ($1
# empty) does the same with claude_clear_window_naming, which also deletes
# names/tasks/issues; here only the option half is safe (shared /tmp dir), so
# the deletion is owed via the @window_naming_dirty mark, stamped BEFORE the
# clear so a crash mid-pair still leaves the deletion owed. Mirrors are
# daemon-owned and skipped, matching the per-tick loop.
if [[ -n ${1:-} ]]; then
	local -A sess_reflow=()
	for wid in "${!win_cur[@]}"; do
		[[ ${win_bridge[$wid]:-} == 1 ]] && continue
		if [[ -n ${win_has[$wid]:-} ]]; then
			[[ ${win_cur[$wid]:-} == 1 ]] && continue
			tmux set -qw -t "$wid" @window_has_agent 1
		else
			stale=""
			[[ -n ${win_cur[$wid]:-} ]] && stale=1
			if [[ ${win_manual[$wid]:-} != 1 && (-n ${win_ai[$wid]:-} || -n ${win_task[$wid]:-}) ]]; then
				stale=1
			fi
			[[ -n $stale ]] || continue
			tmux set -qw -t "$wid" @window_naming_dirty 1
			claude_clear_window_display "$wid" "${win_manual[$wid]:-}"
		fi
		sess_reflow[${win_sess[$wid]:-}]=1
	done
	for s in "${!sess_reflow[@]}"; do
		[[ -n $s && $REFLOW_BIN != @* ]] || continue
		"$REFLOW_BIN" "$s" --force >/dev/null 2>&1 &
		disown 2>/dev/null || true
	done
fi
```

A session name (not a window id) is passed to reflow so its own `scratch-*`
skip still applies. `stale` for a manual window is only ever the `win_cur`
arm.

**e. Gate the per-tick task/name file reads on live occupancy.** Hoist the
existing `has_agent` computation (already there for the transition compare)
above the task block and reuse it, then require it for both reads:

```bash
has_agent=""
if [[ ${win_cur_bridge[$wkey]:-} != 1 ]]; then
	# shellcheck disable=SC2086  # win_procs is a space-joined string; word-split intentionally
	for p in ${win_procs[$wkey]:-}; do
		normalize_wrapped_cmd "$p"
		case " $AGENT_COMMANDS " in *" $REPLY "*)
			has_agent=1
			break
			;;
		esac
	done
fi
```

Task read becomes:

```bash
task=""
[[ -n $has_agent && -f "$CLAUDE_TASKS_DIR/${win_active_pane[$wkey]}" ]] &&
	IFS= read -r task <"$CLAUDE_TASKS_DIR/${win_active_pane[$wkey]}"
```

and the name read likewise gains `-n $has_agent &&`. An agent-free window still
compares the now-empty `task`/`ai_name` against the read-back options, so it
still clears stale options — the gate only stops a lingering *file* from
re-stamping one.

**f. Consume the dirty mark and keep the transition compare.** Add
`#{@window_naming_dirty}` to the batched `list-panes -a -F` format as a fixed
middle field (a `1`/`""` token) beside `#{@window_manual_name}` — before the
free-form `#{@window_task}` — and a matching `cur_naming_dirty` read variable →
`win_cur_naming_dirty[$wkey]` (first-pane-wins, same as the other cached window
options). Then the occupancy block drops its now-duplicate `has_agent=`
computation and becomes:

```bash
clear_needed=""
if [[ -z $has_agent && -n ${win_cur_naming_dirty[$wkey]:-} ]]; then
	clear_needed=1
fi
if [[ -z ${win_poison[$wkey]:-} && ${win_cur_bridge[$wkey]:-} != 1 && ($has_agent != "${win_cur_has_agent[$wkey]:-}" || -n $clear_needed) ]]; then
	if [[ -n $has_agent ]]; then
		tmux set -qw -t "$target" @window_has_agent 1
	else
		# ... existing pane-id-target comment ...
		# shellcheck disable=SC2086  # win_panes is a space-joined string of bare pane ids; word-split intentionally into positional args
		claude_clear_window_naming "%${win_panes[$wkey]%% *}" "${win_cur_manual[$wkey]:-}" ${win_panes[$wkey]:-}
	fi
	sess_need_reflow[$s]=1
fi
```

The dirty trigger is gated on `-z $has_agent` so a window that gained a new
agent while the mark was outstanding does not re-fire every tick: the mark
survives until the window is genuinely agent-free, then the transition fires
once, `claude_clear_window_naming` discharges the deletion and clears the mark
last.

### 3. `tests/update-icons-all-windows.bats` — regression coverage

The suite already drives `tmux-update-icons` directly (sed-substituted libs +
`@reflow@` → `$FAKE_REFLOW`), sets `AGENT_COMMANDS=claude`, builds a
`claude`-named fixture shell, and sets its own `TMUX_TMPDIR`/`CLAUDE_STATUS_DIR`.
Add a `sweep_tick()` helper (`OG_TICK_SWEEP=1 bash "$UPDATE_ICONS"`) and these
cases:

1. **Never-stamped host clears stale naming (the issue's exact state).** A
   shell window with no agent, `@window_has_agent` unset, and seeded
   `@window_ai_name`/`@window_task` + `names/`/`tasks/`/`issues/` files. Sweep →
   both options empty and `@window_naming_dirty` = `1`; the three files still
   exist. One per-tick `bash "$UPDATE_ICONS" A` pass → the three files gone and
   `@window_naming_dirty` empty.
2. **Detached agent exit → reattach heals, with no deletion on the sweep.**
   B:1 runs the fixture `claude`; seed the naming options/files + `@crew_name`.
   Sweep → `@window_has_agent` = `1` on B:1 (`@crew_name` untouched). Swap B:1
   to a plain shell with the existing idiom
   (`tmux respawn-pane -k -t B:1 -- "$(command -v bash)"` plus the
   `pane_current_command` poll already used lower in this file) and run the
   sweep again: `@window_has_agent`/`@window_ai_name`/`@window_task` empty,
   `@crew_name` still `coral`, the three files still present and
   `@window_naming_dirty` = `1`. Then one per-tick pass: files gone, mark
   empty.
3. **Bridge window excluded on the sweep.** Mark B:1 `@bridge_win 1` (running
   the fixture claude) with seeded `@window_ai_name`/file; the sweep leaves
   every one untouched and never writes `@window_has_agent` or
   `@window_naming_dirty`.
4. **Per-tick lingering-file gate.** Seed a `names/`/`tasks/` file on a plain
   shell window whose options are empty and with **no** dirty mark; one per-tick
   pass must leave `@window_ai_name`/`@window_task` empty.
5. **Manual window keeps its naming.** Seed `@window_manual_name 1` +
   `@window_ai_name`/`@window_task` + files on an agent-free window; the sweep
   leaves the naming untouched (and does not churn the dirty mark once
   `@window_has_agent` is empty).
6. **Coexisting different manifest agent.** A window running a non-claude
   manifest agent (`pi`, a copied bash named `pi`, `AGENT_COMMANDS` extended to
   `claude pi`) with a seeded stale `@window_ai_name`: the sweep sets
   `@window_has_agent` = `1` and leaves the name — pinning #671's generic
   "window has a live agent" definition. Documented as a known residual in the
   PR body (see below).

**Red evidence:** with the two production scripts restored from `HEAD` (copies
saved and restored so the working tree is preserved), cases 1, 2 and 4 fail —
the old sweep never writes `@window_has_agent`, never clears a never-stamped
window's stale naming, and the ungated per-tick read re-stamps the lingering
file. Commands and observed failures go in the PR's `## Evidence`.

### 4. `CLAUDE.md`

- `tmux-update-icons` Script Roles row: note the sweep's `#{window_id}`/
  `@bridge_win`/`@window_has_agent`/`@window_manual_name`/`@window_ai_name`/
  `@window_task` widening and that it now also reconciles occupancy
  (option-only, no deletions) on the client-independent tick, marking the
  window `@window_naming_dirty` so the client-gated per-tick pass discharges
  the shared-dir deletion.
- The "#671 window naming/crew reset" bullet: replace the "inert on a host
  whose only clients are control-mode remote-bridge transports" clause with the
  new mechanism — the sweep's occupancy pass writes `@window_has_agent` and
  clears naming/options whenever a window has no live agent (including one
  whose option was never stamped), the per-tick reads are gated on occupancy,
  and only the file deletions stay client-gated (via `@window_naming_dirty`).
- The Task/Name self-report bullets: note the reads are gated on live occupancy
  (#692).

## Known residual (documented in the PR body)

`@window_ai_name`/`@window_task` are Claude-owned, but #671's occupancy
definition is generic (`$AGENT_COMMANDS`, pi included). A window that keeps
running a *different* manifest agent after Claude exits therefore keeps its
Claude naming — by that definition the window still has a live agent. The stale
name is cleared when the window's last agent (of any kind) exits, which is what
this change now reaches on a bridge-only host. Case 6 pins that semantics.
Tightening it to Claude-only occupancy is a separate change (it would need a
claude-only command list rather than `$AGENT_COMMANDS`).

## Verification

```bash
nix build .#default
nix flake check      # bats (update-icons-all-windows.bats, pane-shell-prompt.bats, agent-detect-arm.bats) + Go + tmux-format-delimiter-assertions
nix build .#lint     # alejandra/statix/deadnix/shellcheck/shfmt/typos
```

Affected consumers from the contract map: `arm_agent_detect` callers (sweep
hook + per-tick), `claude_clear_window_naming`/`claude_clear_window_display`
callers (event hook + per-tick backstop), the per-tick task/name read block,
`picker/remotebridge/daemon/windowlabels.go` (the shipped label), and the
`tests/*` suites named above.

## Out of scope

- The shared-directory pruning hazard itself (documented in CLAUDE.md).
- Why the self-report files were reaped while the options were not.
- Any dispatcher-owned option write (`@crew_name`/`@crew_color`/`@crew_role`).
- Claude-only (vs generic) occupancy for the naming reset — see "Known
  residual".

## Review round 1 extension: the event path takes the same split

The first review found the invariant half-done. The sweep does only option
writes, as planned — but the *only* arming signal for `pane-shell-prompt`'s
reset is `@window_has_agent`, and this change starts writing it from a
client-independent timer. `tmux-shell-prompt.sh` (a server-side hook, so it
fires with or without an attached client) still called the deleting
`claude_clear_window_naming`, so the change made a never-client host — the
bridge-only class this issue targets — able to delete `names/`/`tasks/`/
`issues/` under the machine-global `CLAUDE_STATUS_DIR` where it previously
could not. Measured: a second, client-less wrapped-tmux server whose `%0` ran a
manifest agent, sweep-armed, deleted the first server's live pane `%0` state
when a shell prompt answered.

Fixed by giving the event path the same contract as the sweep: it clears the
options via `claude_clear_window_display`, stamps `@window_naming_dirty 1`
before the clear, and lets the client-gated per-tick pass do the `rm`s. All
shared-dir deletion is now on one client-gated path. This is a deliberate
change to #671's event-trigger contract (which used to delete immediately, and
`tests/pane-shell-prompt.bats` pinned it client-less); the visible naming
reset is unchanged and still immediate, and only the file `rm` is deferred to
the next client-gated pass.
