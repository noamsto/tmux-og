#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells
# #1136: tmux-update-icons swaps a just/make pane's name for the command it runs
# (resolution rule: runner-child.bats). Drives the script on a private server:
# one ps per tick, none without a runner pane.

setup() {
	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$TDIR/data"
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	export TMPDIR="$TDIR"
	mkdir -p "$TMUX_TMPDIR" "$TDIR/bin"
	# shellcheck disable=SC2174
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,tasks,names}
	unset TMUX
	OG_TEST_ISOLATED=1
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5)) AGENT_COMMANDS="claude"

	printf '#!/bin/sh\nexit 0\n' >"$TDIR/fake-reflow"
	chmod +x "$TDIR/fake-reflow"
	sed -e 's/@ICON_MAP@/["just"]="J" ["sh"]="S" ["sleep"]="Z"/' -e 's/@FALLBACK_ICON@/F/' scripts/lib-icons.sh >"$TDIR/lib-icons.sh"
	sed \
		-e "s|@lib_icons@|$TDIR/lib-icons.sh|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$TDIR/fake-reflow|g" \
		-e "s|@MAX_ICONS@|3|g" \
		scripts/tmux-update-icons.sh >"$TDIR/update-icons.sh"

	# Logs only the whole-table reads this feature adds; lib-claude's `ps -o`
	# liveness probes pass through unlogged.
	REAL_PS="$(command -v ps)"
	cat >"$TDIR/bin/ps" <<-EOF
		#!/bin/sh
		case "\$1" in -wwAo) echo ps >>"$TDIR/ps.log" ;; esac
		exec "$REAL_PS" "\$@"
	EOF
	chmod +x "$TDIR/bin/ps"
	export PATH="$TDIR/bin:$PATH"
	: >"$TDIR/ps.log"

	# pane_current_command is the executable basename: bash copied to `just`.
	cp -L "$(command -v bash)" "$TDIR/bin/just"
	command -v tmux >/dev/null || skip "tmux not on PATH"
	tmux -f /dev/null new-session -d -s S -c "$TDIR" -x 200 -y 50
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	PANE="$(tmux display-message -p -t S '#{pane_id}')"
}

teardown() {
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	tmux kill-server 2>/dev/null || true
}

wait_cmd() {
	local tries=30
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$PANE" '#{pane_current_command}')" = "$1" ] && return 0
		sleep 0.1
	done
	return 1
}

wait_cmd_not() {
	local tries=30
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$PANE" '#{pane_current_command}')" != "$1" ] && return 0
		sleep 0.1
	done
	return 1
}

display_of() { tmux show -wv -t "$WIN" @window_icon_display 2>/dev/null || true; }

@test "no runner pane: update-icons reads no process table" {
	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	[ ! -s "$TDIR/ps.log" ]
}

@test "just pane running sleep shows the child's icon, then the shell's once it ends" {
	tmux respawn-pane -k -t "$PANE" -- "$TDIR/bin/just" -c 'sleep 30; true'
	wait_cmd just
	local tries=30
	while ((tries-- > 0)); do
		# shellcheck disable=SC2009  # no pgrep on darwin
		ps -A -o ppid= | grep -qx " *$(tmux display-message -p -t "$PANE" '#{pane_pid}')" && break
		sleep 0.1
	done

	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	during="$(display_of)"
	echo "during: [$during]"
	[ "$(wc -l <"$TDIR/ps.log")" -eq 1 ]
	[ "$during" = "Z" ]
	[ "$(tmux show -v -t S @active_pane_icon)" = "Z" ]

	tmux respawn-pane -k -t "$PANE" -- sh -c 'sleep 30'
	# Darwin names `sh -c 'sleep 30'` differently from Linux (sh vs sleep).
	wait_cmd_not just
	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	after="$(display_of)"
	echo "after: [$after]"
	[[ $after == S || $after == Z ]]
}

@test "idle just pane keeps the runner's own icon" {
	tmux respawn-pane -k -t "$PANE" -- "$TDIR/bin/just" -c 'read -r x'
	wait_cmd just
	run bash "$TDIR/update-icons.sh" S
	[ "$status" -eq 0 ]
	[ "$(display_of)" = "J" ]
	[ "$(tmux show -v -t S @active_pane_icon)" = "J" ]
}
