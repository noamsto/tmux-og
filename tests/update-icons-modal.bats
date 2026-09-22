#!/usr/bin/env bats
bats_require_minimum_version 1.5.0
# Regression for #725: a modal float is chrome, not window content. Without
# the list-panes -f filter and the EFF_ACTIVE swap in tmux-update-icons.sh,
# opening a float over the claude pane makes the FLOAT itself pane_active, so
# the task/name lookup keys off the float's (nonexistent) state files instead
# of the claude pane's — the window's task/name would silently go blank while
# a float is open. See docs/agents/floats.md.
#
# Needs the pinned tmux (mkTmux in flake.nix, not nixpkgs' pkgs.tmux): only it
# has pane_modal_flag/window_modal_pane and accepts `new-pane -O/-K` instead of
# rejecting them at parse time (same trap as tests/pane-border-format.bats).

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,tasks,names}
	export TMPDIR="$TDIR"

	# update-icons throttles its presence sweep on CLAUDE_NOW % 5; pin it like
	# the other update-icons suites (#373).
	export CLAUDE_NOW=$(($(date +%s) / 5 * 5))
	export AGENT_COMMANDS="claude"

	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	MAX_ICONS=2
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

	# pane_current_command is the executable basename: copy bash to files
	# named `claude` (the agent pane) and `picker-x` (the float, a distinct
	# command not in ICON_MAP, mirroring the real picker's own argv0).
	mkdir -p "$TDIR/bin"
	cp -L "$(command -v bash)" "$TDIR/bin/claude"
	chmod +x "$TDIR/bin/claude"
	cp -L "$(command -v bash)" "$TDIR/bin/picker-x"
	chmod +x "$TDIR/bin/picker-x"

	tmux -f /dev/null new-session -d -s S -c "$REPO" -x 200 -y 50
	tmux split-window -t S -c "$REPO"

	CLAUDE_PANE="$(tmux list-panes -t S -F '#{pane_id}' | head -1)"
	tmux respawn-pane -k -t "$CLAUDE_PANE" -- "$TDIR/bin/claude"
	local tries=20
	while ((tries-- > 0)); do
		[ "$(tmux display-message -p -t "$CLAUDE_PANE" '#{pane_current_command}')" = claude ] && break
		sleep 0.1
	done
	CLAUDE_BARE="${CLAUDE_PANE#%}"
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	tmux set -w -t "$WIN" @window_has_agent 1
	# The float must open on top of the claude pane while it's active — EFF_ACTIVE
	# resolves to pane_last, i.e. whatever was active a moment ago.
	tmux select-pane -t "$CLAUDE_PANE"

	printf 'the task\n' >"$CLAUDE_STATUS_DIR/tasks/$CLAUDE_BARE"
	printf 'the name\n' >"$CLAUDE_STATUS_DIR/names/$CLAUDE_BARE"
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

task_of() { tmux show -wv -t "$1" @window_task 2>/dev/null || true; }
name_of() { tmux show -wv -t "$1" @window_ai_name 2>/dev/null || true; }
display_of() { tmux show -wv -t "$1" @window_icon_display 2>/dev/null || true; }
active_icon_of() { tmux show -v -t "$1" @active_pane_icon 2>/dev/null || true; }

@test "modal float over the claude pane: task/name/active-icon still key off the pane under it" {
	if ! tmux new-pane -O -K -t "$CLAUDE_PANE" -x 50% -y 50% "$TDIR/bin/picker-x" 2>/dev/null; then
		skip "this tmux advertises -O/-K but rejects them at parse time"
	fi
	FLOAT_PANE="$(tmux list-panes -t S -F '#{pane_id} #{pane_modal_flag}' | awk '$2==1{print $1; exit}')"
	[ -n "$FLOAT_PANE" ] || skip "this tmux does not report pane_modal_flag"

	# Vacuous-pass guard: the float genuinely must be modal, and the window
	# must name it, or every assertion below would pass for the wrong reason.
	[ "$(tmux display-message -p -t "$FLOAT_PANE" '#{pane_modal_flag}')" = 1 ]
	[ "$(tmux display-message -p -t "$WIN" '#{window_modal_pane}')" = "$FLOAT_PANE" ]

	run bash "$UPDATE_ICONS" S
	[ "$status" -eq 0 ] || { echo "update-icons exited $status: $output" && false; }

	[ "$(task_of "$WIN")" = "the task" ] || { echo "task [$(task_of "$WIN")]" && false; }
	[ "$(name_of "$WIN")" = "the name" ] || { echo "name [$(name_of "$WIN")]" && false; }
	[ "$(active_icon_of S)" = "C" ] || { echo "active icon [$(active_icon_of S)]" && false; }
	with_float="$(display_of "$WIN")"

	# Close the float and re-run: the display must be exactly what it was
	# with the float open — the picker-x pane never contributed an icon.
	tmux kill-pane -t "$FLOAT_PANE"
	run bash "$UPDATE_ICONS" S
	[ "$status" -eq 0 ]
	without_float="$(display_of "$WIN")"
	[ "$with_float" = "$without_float" ] ||
		{ echo "display with float [$with_float] without [$without_float]" && false; }
}
