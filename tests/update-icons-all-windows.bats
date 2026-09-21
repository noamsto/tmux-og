#!/usr/bin/env bats
bats_require_minimum_version 1.5.0
# Regression for #580: tmux-update-icons is invoked from status-format[0], which
# tmux only evaluates for a client drawing a status line. A session with no
# attached client never got its own pass, so @window_icon_padded stayed unset
# (empty) rather than the MAX_ICONS*3+2 empty pad. One invocation on A must
# stamp every list-windows -a row.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen"
	export TMPDIR="$TDIR"

	# update-icons throttles its presence sweep on CLAUDE_NOW % 5, and that sweep
	# reaps state files. Pin it like the other update-icons suites (#373).
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))

	# #671's backstop matches pane_current_command against $AGENT_COMMANDS
	# (env-var test seam, same as AGENT_DETECT_BIN); the sed pipeline below
	# doesn't substitute @AGENT_COMMANDS@, so without this it stays the
	# literal, never-matching placeholder and @window_has_agent never flips.
	export AGENT_COMMANDS="claude"

	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	# MAX_ICONS=2 → empty pad is 8 cells, the measured discriminator in #580.
	MAX_ICONS=2
	PAD_LEN=$((MAX_ICONS * 3 + 2))
	export PAD_LEN

	UPDATE_ICONS="$TDIR/update-icons.sh"
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@/["claude"]="C"/' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e "s|@MAX_ICONS@|$MAX_ICONS|g" \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	REPO="$TDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" branch -q -M main

	# pane_current_command is the executable basename. sleep/cat are coreutils
	# multicall (argv0 dispatch), and a shebang script reports as `sh`, so copy
	# bash to a file named claude and use it as default-shell for one window.
	mkdir -p "$TDIR/bin"
	cp -L "$(command -v bash)" "$TDIR/bin/claude"
	chmod +x "$TDIR/bin/claude"
	# A second manifest command (pi) for the coexisting-agent case: same trick,
	# so pane_current_command is the literal `pi`.
	cp -L "$(command -v bash)" "$TDIR/bin/pi"
	chmod +x "$TDIR/bin/pi"

	tmux -f /dev/null new-session -d -s A -c "$REPO" -x 200 -y 50
	tmux new-session -d -s B -c "$REPO" -x 200 -y 50
	tmux set -g default-shell "$TDIR/bin/claude"
	tmux set -g default-command ''
	tmux new-window -t B -c "$REPO"
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

padded_of() {
	tmux show -wv -t "$1" @window_icon_padded 2>/dev/null || true
}

display_of() {
	tmux show -wv -t "$1" @window_icon_display 2>/dev/null || true
}

opt_of() {
	tmux show -wv -t "$1" "$2" 2>/dev/null || true
}

# sweep_tick — one client-independent pass (the @og-sweep-tick monitor hook's
# shape), which reconciles occupancy and the option half of the #671 reset.
sweep_tick() {
	OG_TICK_SWEEP=1 bash "$UPDATE_ICONS"
}

# bare_id PANE — a pane id without its '%'.
bare_id() {
	local id="$1"
	printf '%s' "${id#%}"
}

@test "one pass on A stamps @window_icon_padded on every window including unattached B" {
	# Before the pass, B's windows must still be unstamped (the bug's red).
	[ -z "$(padded_of B:0)" ]
	[ -z "$(padded_of B:1)" ]

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}

	local n=0 sess idx padded disp
	while IFS='|' read -r sess idx; do
		[ -n "$sess" ] || continue
		n=$((n + 1))
		padded=$(padded_of "$sess:$idx")
		[ ${#padded} -eq "$PAD_LEN" ] ||
			{ echo "$sess:$idx padded len ${#padded} want $PAD_LEN value [$padded]" && false; }
	done < <(tmux list-windows -a -F '#{session_name}|#{window_index}')
	[ "$n" -eq 3 ] || { echo "expected 3 windows, got $n" && false; }

	# Shell windows: empty pad (spaces), empty display.
	disp=$(display_of A:0)
	[ -z "$disp" ]
	padded=$(padded_of A:0)
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ]

	padded=$(padded_of B:0)
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ]

	# B:1's executable is named claude → ICON_MAP hit, display is the mapped glyph.
	disp=$(display_of B:1)
	[ "$disp" = "C" ] || { echo "B:1 display [$disp] want [C]; cmd=$(tmux list-panes -t B:1 -F '#{pane_current_command}')" && false; }
}

@test "one pass on A stamps @window_icon_padded on a session named with a pipe" {
	# tmux forbids '.' and ':' in session names; '|' is legal and would shift a
	# middle #{session_name} field in list-panes -F, so wkey is wrong and the
	# pad never lands — the same #580 discriminator.
	# Restore a normal default-shell so this session is a shell window (empty
	# pad), matching A:0 — setup() pointed default-shell at a `claude` binary
	# only to give B:1 an ICON_MAP hit.
	tmux set -g default-shell "$(command -v bash)"
	if ! tmux new-session -d -s 'a|b' -c "$REPO" -x 200 -y 50 2>/dev/null; then
		# skip citing version if this tmux rejects '|'-named sessions
		skip "tmux $(tmux -V) rejects session names containing |"
	fi

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}

	local line sid sname padded
	sid=""
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		sname="${line#*|}"
		if [ "$sname" = "a|b" ]; then
			sid="${line%%|*}"
			break
		fi
	done < <(tmux list-sessions -F '#{session_id}|#{session_name}')
	[ -n "$sid" ] || { echo "could not resolve session id for a|b" && false; }

	padded=$(padded_of "$sid:0")
	[ "$padded" = "$(printf '%*s' "$PAD_LEN" '')" ] ||
		{ echo "$sid:0 padded [$padded] want $PAD_LEN spaces" && false; }
}

# #671 backstop: a shell with no OSC 133 support never fires
# tmux-shell-prompt's event trigger, so tmux-update-icons' own per-window
# has_agent transition compare is the only path that can ever clear a window's
# naming/crew display once its last agent exits.
@test "backstop clears naming/crew display once B:1's foreground command is no longer an agent" {
	local pane bare
	pane="$(tmux list-panes -t B:1 -F '#{pane_id}')"
	bare="${pane#%}"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks,issues}

	tmux set -w -t B:1 @window_ai_name "Old AI Name"
	tmux set -w -t B:1 @window_task "old task"
	tmux set -w -t B:1 @crew_name coral
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'old task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ "$(opt_of B:1 @window_has_agent)" = 1 ]

	# Agent exits to a shell with no OSC 133 support: swap the pane's
	# foreground command from the fixture `claude` binary to a plain shell —
	# no hook fires on this, so only a later backstop pass can notice.
	tmux respawn-pane -k -t B:1 -- "$(command -v bash)"
	local tries=20
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t B:1 '#{pane_current_command}')" != claude ] && break
		sleep 0.1
	done

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ -z "$(opt_of B:1 @window_has_agent)" ]
	[ -z "$(opt_of B:1 @window_ai_name)" ]
	[ -z "$(opt_of B:1 @window_task)" ]
	[ ! -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ]
	# Hard constraint: @crew_name is dispatcher-owned and never touched.
	[ "$(opt_of B:1 @crew_name)" = coral ]
}

# claude_win — the B window whose pane runs the fixture `claude` binary, as
# "B:<index>". The suite's other cases hard-code B:1 (a session created before
# default-shell is repointed); discovering it keeps these #692 cases correct
# regardless of the server's base-index.
claude_win() {
	local idx cmd
	while IFS='|' read -r idx cmd; do
		if [ "$cmd" = claude ]; then
			printf 'B:%s' "$idx"
			return 0
		fi
	done < <(tmux list-panes -s -t B -F '#{window_index}|#{pane_current_command}')
	return 1
}

# #692: the client-independent sweep owns the option half of the #671 reset on
# a host whose only clients are remote-bridge transports (no status line, so
# the per-tick loop never runs). It must clear naming for a window whose
# @window_has_agent was NEVER stamped — the reported host's exact state, where
# an option-transition-gated clear would skip it — and must not delete from the
# shared CLAUDE_STATUS_DIR; the deletion is owed to the client-gated per-tick
# pass via @window_naming_dirty.
@test "sweep clears a never-stamped window's stale naming and owes the file deletion" {
	local bare
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks,issues}
	tmux set -w -t A @window_ai_name "stale ai"
	tmux set -w -t A @window_task "stale task"
	tmux set -w -t A @crew_name coral
	printf 'stale ai\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'stale task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"
	# The bug's precondition: the option is unset because this host never ran
	# the per-tick loop.
	[ -z "$(opt_of A @window_has_agent)" ]

	run sweep_tick
	[ "$status" -eq 0 ] || { echo "sweep exited $status: $output" && false; }
	[ -z "$(opt_of A @window_ai_name)" ]
	[ -z "$(opt_of A @window_task)" ]
	[ "$(opt_of A @window_naming_dirty)" = 1 ]
	# Hard constraint: @crew_name is dispatcher-owned and never touched.
	[ "$(opt_of A @crew_name)" = coral ]
	# No deletion on the client-independent path (shared /tmp dir).
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/issues/$bare" ]

	# The per-tick pass is the client-gated one: it discharges the owed
	# deletion and clears the mark.
	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ -z "$(opt_of A @window_ai_name)" ]
	[ -z "$(opt_of A @window_task)" ]
	[ -z "$(opt_of A @window_naming_dirty)" ]
	[ ! -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ]
}

@test "sweep clears naming on an agent exit, then the per-tick pass deletes the files" {
	local cwin bare
	cwin="$(claude_win)"
	bare="$(bare_id "$(tmux list-panes -t "$cwin" -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks,issues}
	tmux set -w -t "$cwin" @window_ai_name "Old AI Name"
	tmux set -w -t "$cwin" @window_task "old task"
	tmux set -w -t "$cwin" @crew_name coral
	printf 'Old AI Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'old task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"
	printf 'ISSUE-1\n' >"$CLAUDE_STATUS_DIR/issues/$bare"

	# Live agent: the sweep stamps occupancy and leaves the naming alone.
	run sweep_tick
	[ "$status" -eq 0 ]
	[ "$(opt_of "$cwin" @window_has_agent)" = 1 ]
	[ "$(opt_of "$cwin" @window_ai_name)" = "Old AI Name" ]

	# Agent exits with no client attached (the reported class): swap the
	# foreground command to a plain shell — no hook fires on this.
	tmux respawn-pane -k -t "$cwin" -- "$(command -v bash)"
	local tries=20
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$cwin" '#{pane_current_command}')" != claude ] && break
		sleep 0.1
	done

	run sweep_tick
	[ "$status" -eq 0 ]
	[ -z "$(opt_of "$cwin" @window_has_agent)" ]
	[ -z "$(opt_of "$cwin" @window_ai_name)" ]
	[ -z "$(opt_of "$cwin" @window_task)" ]
	[ "$(opt_of "$cwin" @window_naming_dirty)" = 1 ]
	[ "$(opt_of "$cwin" @crew_name)" = coral ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/issues/$bare" ]

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ -z "$(opt_of "$cwin" @window_naming_dirty)" ]
	[ ! -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
	[ ! -e "$CLAUDE_STATUS_DIR/issues/$bare" ]
}

@test "sweep leaves a bridge window's naming and options untouched" {
	local cwin bare
	cwin="$(claude_win)"
	bare="$(bare_id "$(tmux list-panes -t "$cwin" -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks,issues}
	tmux set -w -t "$cwin" @bridge_win 1
	tmux set -w -t "$cwin" @window_has_agent 1
	tmux set -w -t "$cwin" @window_ai_name "Bridge Name"
	tmux set -w -t "$cwin" @window_task "bridge task"
	printf 'Bridge Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"

	run sweep_tick
	[ "$status" -eq 0 ]
	# A mirror is daemon-owned: the sweep never writes these, even though the
	# pane runs the fixture claude and has_agent is stamped.
	[ "$(opt_of "$cwin" @window_has_agent)" = 1 ]
	[ -z "$(opt_of "$cwin" @window_naming_dirty)" ]
	[ "$(opt_of "$cwin" @window_ai_name)" = "Bridge Name" ]
	[ "$(opt_of "$cwin" @window_task)" = "bridge task" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
}

@test "per-tick ignores a lingering self-report file on an agent-free window" {
	local bare
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	printf 'stale ai\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'stale task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"

	# Options empty, no dirty mark: exactly the state the sweep leaves before
	# the per-tick pass runs. A's window is a shell, so there is no live agent —
	# the read must be skipped rather than re-stamp the file's contents.
	[ -z "$(opt_of A @window_ai_name)" ]
	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ -z "$(opt_of A @window_ai_name)" ]
	[ -z "$(opt_of A @window_task)" ]
}

@test "sweep leaves a manually-named window's naming alone (no churn)" {
	local bare
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	tmux set -w -t A @window_manual_name 1
	tmux set -w -t A @window_ai_name "My Name"
	tmux set -w -t A @window_task "my task"
	printf 'My Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'my task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"

	run sweep_tick
	[ "$status" -eq 0 ]
	[ "$(opt_of A @window_ai_name)" = "My Name" ]
	[ "$(opt_of A @window_task)" = "my task" ]
	# Never cleared, so no deletion is owed.
	[ -z "$(opt_of A @window_naming_dirty)" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
}

@test "sweep pins #671's generic occupancy: a coexisting different agent keeps the name" {
	local cwin bare
	cwin="$(claude_win)"
	bare="$(bare_id "$(tmux list-panes -t "$cwin" -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	tmux set -w -t "$cwin" @window_ai_name "Old Claude Name"
	printf 'Old Claude Name\n' >"$CLAUDE_STATUS_DIR/names/$bare"

	# Swap the fixture claude for pi, a different manifest agent.
	tmux respawn-pane -k -t "$cwin" -- "$TDIR/bin/pi"
	local tries=20
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$cwin" '#{pane_current_command}')" = pi ] && break
		sleep 0.1
	done
	export AGENT_COMMANDS="claude pi"

	run sweep_tick
	[ "$status" -eq 0 ]
	# Generic occupancy: pi is a live agent, so this is not a 1->0 transition
	# and the stale Claude name is deliberately kept (#671's Definition). The
	# name clears on the window's next agent exit — the documented residual.
	[ "$(opt_of "$cwin" @window_has_agent)" = 1 ]
	[ "$(opt_of "$cwin" @window_ai_name)" = "Old Claude Name" ]
	[ -z "$(opt_of "$cwin" @window_naming_dirty)" ]
}

# @window_ai_name is a plain user-settable window option, so its row copy is
# s/[|]/ /-wrapped like @window_task's: the row's only canary (window_id) sits
# ahead of both, and an unwrapped '|' here shifts the task into session_name and
# leaves win_ai/win_task empty, so the sweep's stale test never fires and the
# name the sweep exists to clear survives (#692 review, round 1 MEDIUM).
@test "sweep clears a stale name whose own value opens with a '|'" {
	local bare
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	tmux set -w -t A @window_ai_name '|'
	tmux set -w -t A @window_task 'stale task'
	printf '|\n' >"$CLAUDE_STATUS_DIR/names/$bare"

	run sweep_tick
	[ "$status" -eq 0 ]
	[ -z "$(opt_of A @window_ai_name)" ]
	[ -z "$(opt_of A @window_task)" ]
	[ "$(opt_of A @window_naming_dirty)" = 1 ]
}

# #714: the per-tick row's @window_ai_name is a fixed middle field ahead of
# @window_has_agent/@window_manual_name/@window_naming_dirty/@window_task, and
# the row's only canary (pane_active) sits before it. An unwrapped '|' in the
# option shifts those fields left by one: @window_manual_name reads as the
# (empty) dirty mark, so a manually-named window loses its protection, and the
# dirty mark reads as the task text, so the #692 discharge fires and deletes
# the window's name files.
@test "per-tick row survives a '|' in @window_ai_name on a manually-named window" {
	local bare
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	tmux set -w -t A @window_manual_name 1
	tmux set -w -t A @window_ai_name 'a|b'
	tmux set -w -t A @window_task 'my task'
	printf 'a b\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'my task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ "$(opt_of A @window_task)" = "my task" ]
	[ -z "$(opt_of A @window_naming_dirty)" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
}

# #712: SERVER_PID is this server's own #{pid}, and claude_prune_stale_state
# needs it to protect a live foreign server's state files. When it cannot be
# resolved the script must skip the destructive sweep rather than fall back to
# mtime alone (which can delete a different, live server's files, #676) and log
# the skip. A legacy (no server=) stale panes file keyed to a LIVE pane id is
# the discriminator: the mtime sweep deletes it, the skip keeps it, and the id
# being live keeps claude_reap_dead_panes (which fires at CLAUDE_NOW % 60 == 0)
# from removing it in either version.
@test "unresolvable server pid skips the destructive prune and logs it" {
	local bare stale
	bare="$(bare_id "$(tmux list-panes -t A -F '#{pane_id}' | head -1)")"
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	stale="$CLAUDE_STATUS_DIR/panes/$bare"
	printf 'state=waiting\ntimestamp=1\nsession=A\n' >"$stale"
	touch -t 200001010000 "$stale"

	# A tmux shim that fails only the pid lookup and forwards everything else
	# to the real binary by ABSOLUTE path — a bare-name exec would recurse into
	# this same shim.
	local real_tmux shim="$TDIR/shim"
	real_tmux="$(command -v tmux)"
	mkdir -p "$shim"
	cat >"$shim/tmux" <<-EOF
		#!/bin/sh
		if [ "\$*" = "display-message -p #{pid}" ]; then
		    exit 1
		fi
		exec "$real_tmux" "\$@"
	EOF
	chmod +x "$shim/tmux"

	export OG_DEBUG_SENTINEL="$TDIR/debug.on"
	export XDG_STATE_HOME="$TDIR/state"
	: >"$OG_DEBUG_SENTINEL"

	run env PATH="$shim:$PATH" bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ] || {
		echo "update-icons exited $status: $output"
		false
	}
	[ -e "$stale" ] || {
		echo "prune ran with an unresolved server pid: $stale deleted"
		false
	}
	grep -q 'prune_skipped' "$XDG_STATE_HOME/og/events.log" || {
		echo "skip was not logged: $(cat "$XDG_STATE_HOME/og/events.log" 2>/dev/null)"
		false
	}
}

# #712 (folded from #717's review of #714): @remux_relaunch is a fixed middle
# field of the per-tick row, ahead of @bridge_proc/@window_has_agent/
# @window_manual_name/@window_naming_dirty/@window_task. An unwrapped '|' in it
# shifts those fields left, so a manually-named window loses its protection and
# the #692 clear fires, deleting the window's name/task files. The row copy is
# s/[|]/ /-wrapped like its neighbours.
@test "per-tick row survives a '|' in @remux_relaunch on a manually-named window" {
	local bare pane
	pane="$(tmux list-panes -t A -F '#{pane_id}' | head -1)"
	bare="$(bare_id "$pane")"
	mkdir -p "$CLAUDE_STATUS_DIR"/{names,tasks}
	tmux set -w -t A @window_manual_name 1
	tmux set -w -t A @window_ai_name 'ai name'
	tmux set -w -t A @window_task 'my task'
	tmux set -p -t "$pane" @remux_relaunch 'a|b'
	printf 'ai name\n' >"$CLAUDE_STATUS_DIR/names/$bare"
	printf 'my task\n' >"$CLAUDE_STATUS_DIR/tasks/$bare"

	run bash "$UPDATE_ICONS" A
	[ "$status" -eq 0 ]
	[ "$(opt_of A @window_ai_name)" = "ai name" ]
	[ "$(opt_of A @window_task)" = "my task" ]
	[ -z "$(opt_of A @window_naming_dirty)" ]
	[ -e "$CLAUDE_STATUS_DIR/names/$bare" ]
	[ -e "$CLAUDE_STATUS_DIR/tasks/$bare" ]
}
