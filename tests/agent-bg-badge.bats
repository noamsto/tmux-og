#!/usr/bin/env bats

load helper

setup() {
	CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR"
	export CLAUDE_STATUS_DIR
	# BATS_TEST_TMPDIR already exists (0755) -- chmod, not mkdir -m, to make it
	# owner-only before the source-time trust check.
	chmod 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen"
	setup_lib_claude
	CLAUDE_NOW=100000 # pin the clock
}

hook() { printf 'state=%s\ntimestamp=%s\n' "$2" "$3" >"$CLAUDE_STATUS_DIR/panes/$1"; }
screen_bg() { printf 'state=%s\ntimestamp=%s\nbg=%s\n' "$2" "$3" "$4" >"$CLAUDE_STATUS_DIR/screen/$1"; }

@test "bg count comes off the screen file even when a fresh hook wins the state" {
	hook 3 waiting $((CLAUDE_NOW - 5))
	screen_bg 3 idle $((CLAUDE_NOW - 1)) 2
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY" = waiting ]
	[ "$REPLY_BG" = 2 ]
}

@test "no screen file means no badge" {
	hook 3 processing $((CLAUDE_NOW - 5))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY_BG" = 0 ]
}

@test "a non-numeric bg field is ignored rather than rendered" {
	screen_bg 4 idle $((CLAUDE_NOW - 1)) 'x; rm -rf /'
	read_pane_state "$CLAUDE_STATUS_DIR/panes/4"
	[ "$REPLY_BG" = 0 ]
}

@test "badge renders the count and is empty at zero" {
	setup_claude_colors
	claude_bg_badge 0
	[ -z "$REPLY" ]
	claude_bg_badge 3
	[[ $REPLY == *3* ]]
}

@test "bg never enters the state priority" {
	claude_priority_state 0 0 0 0 1 0 0 0
	[ "$REPLY" = idle ]
}
