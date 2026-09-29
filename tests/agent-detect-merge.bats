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
screen() { printf 'state=%s\ntimestamp=%s\n' "$2" "$3" >"$CLAUDE_STATUS_DIR/screen/$1"; }

@test "fresh hook wins over screen" {
	hook 3 processing $((CLAUDE_NOW - 5))
	screen 3 idle $((CLAUDE_NOW - 1))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY" = processing ]
}

@test "fresh native Codex hook state wins over screen" {
	hook 3 waiting $((CLAUDE_NOW - 5))
	screen 3 processing $((CLAUDE_NOW - 1))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY" = waiting ]
}

@test "stale hook + screen -> screen" {
	hook 3 processing $((CLAUDE_NOW - 400))
	screen 3 idle $((CLAUDE_NOW - 1))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY" = idle ]
}

@test "no hook + screen -> screen" {
	screen 7 processing $((CLAUDE_NOW - 1))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/7"
	[ "$REPLY" = processing ]
}

@test "idle hook never overridden by screen" {
	hook 3 idle $((CLAUDE_NOW - 5000))
	screen 3 processing $((CLAUDE_NOW - 1))
	read_pane_state "$CLAUDE_STATUS_DIR/panes/3"
	[ "$REPLY" = idle ]
}

@test "neither -> failure" {
	run read_pane_state "$CLAUDE_STATUS_DIR/panes/999"
	[ "$status" -ne 0 ]
}
