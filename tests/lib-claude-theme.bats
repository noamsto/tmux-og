#!/usr/bin/env bats
# Pure unit coverage for setup_claude_colors' precedence (#663): explicit
# flavor argument > live $TMUX @catppuccin_flavor > theme-state.json fallback.
# No live tmux server anywhere -- a fake `tmux` stub only for the live-flavor
# case.

load helper

setup() {
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	unset TMUX
}

@test "explicit latte argument wins with no tmux or state file" {
	setup_lib_claude
	setup_claude_colors "latte"
	[ "$H_W" = "#fe640b" ]
	[ "$H_K" = "#04a5e5" ]
	[ "$H_P" = "#179299" ]
	[ "$H_D" = "#40a02b" ]
	[ "$H_I" = "#6c6f85" ]
	[ "$H_E" = "#d20f39" ]
	[ "$H_DN" = "#df8e1d" ]
	[ "$H_INT" = "#8839ef" ]
}

@test "explicit mocha argument wins with no tmux or state file" {
	setup_lib_claude
	setup_claude_colors "mocha"
	[ "$H_W" = "#fab387" ]
	[ "$H_K" = "#89dceb" ]
	[ "$H_P" = "#94e2d5" ]
	[ "$H_D" = "#a6e3a1" ]
	[ "$H_I" = "#6c7086" ]
	[ "$H_E" = "#f38ba8" ]
	[ "$H_DN" = "#f9e2af" ]
	[ "$H_INT" = "#cba6f7" ]
}

@test "no argument: live tmux flavor wins over a disagreeing state file" {
	STUBDIR="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STUBDIR"
	cat >"$STUBDIR/tmux" <<-'EOF'
		#!/bin/sh
		case "$*" in
		"show-options -gv @catppuccin_flavor") echo "latte" ;;
		esac
	EOF
	chmod +x "$STUBDIR/tmux"
	PATH="$STUBDIR:$PATH"

	mkdir -p "$XDG_STATE_HOME"
	printf '{"theme":"dark","timestamp":"2024-01-01T00:00:00+0000","failed":[],"version":1}\n' \
		>"$XDG_STATE_HOME/theme-state.json"

	export TMUX=/tmp/fake
	setup_lib_claude
	setup_claude_colors
	[ "$H_W" = "#fe640b" ]
}

@test "no argument, no TMUX: state file theme=light wins" {
	mkdir -p "$XDG_STATE_HOME"
	printf '{"theme":"light","timestamp":"2024-01-01T00:00:00+0000","failed":[],"version":1}\n' \
		>"$XDG_STATE_HOME/theme-state.json"

	setup_lib_claude
	setup_claude_colors
	[ "$H_W" = "#fe640b" ]
}

@test "no argument, no TMUX: state file theme=dark wins" {
	mkdir -p "$XDG_STATE_HOME"
	printf '{"theme":"dark","timestamp":"2024-01-01T00:00:00+0000","failed":[],"version":1}\n' \
		>"$XDG_STATE_HOME/theme-state.json"

	setup_lib_claude
	setup_claude_colors
	[ "$H_W" = "#fab387" ]
}

@test "no argument, no TMUX, no state file: defaults to dark" {
	setup_lib_claude
	setup_claude_colors
	[ "$H_W" = "#fab387" ]
}
