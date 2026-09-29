#!/usr/bin/env bats
# Tests claude_pane_ids (lib-claude.sh) and its use in claude-status.sh's
# session aggregation: a screen-only pane (no hook file) must be enumerated,
# and a pane present in both dirs must be counted exactly once.
# shellcheck disable=SC2154 # total/count_* are globals set by the sourced
# claude-status.sh functions (setup_claude_status_functions), not this file

load helper

setup() {
	CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR"
	export CLAUDE_STATUS_DIR
	# BATS_TEST_TMPDIR already exists (0755) -- chmod, not mkdir -m, to make it
	# owner-only before the source-time trust check.
	chmod 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen"
	CLAUDE_NOW=100000
	setup_claude_status_functions
}

hook() { printf 'state=%s\ntimestamp=%s\nsession=%s\n' "$2" "$3" "${4:-sess}" >"$CLAUDE_STATUS_DIR/panes/$1"; }
screen() { printf 'state=%s\ntimestamp=%s\n' "$2" "$3" >"$CLAUDE_STATUS_DIR/screen/$1"; }

@test "claude_pane_ids: screen-only pane is included" {
	screen 7 processing $((CLAUDE_NOW - 1))
	run claude_pane_ids
	[ "$status" -eq 0 ]
	[ "$output" = "7" ]
}

@test "claude_pane_ids: pane in both dirs is deduped to one entry" {
	hook 3 processing $((CLAUDE_NOW - 5))
	screen 3 idle $((CLAUDE_NOW - 1))
	run claude_pane_ids
	[ "$status" -eq 0 ]
	[ "$(echo "$output" | wc -l)" -eq 1 ]
	[ "$output" = "3" ]
}

@test "claude_pane_ids: union of hook-only and screen-only ids, no duplicates" {
	hook 3 processing $((CLAUDE_NOW - 5))
	screen 7 idle $((CLAUDE_NOW - 1))
	run claude_pane_ids
	[ "$status" -eq 0 ]
	local sorted
	sorted="$(echo "$output" | sort | tr '\n' ' ')"
	[ "$sorted" = "3 7 " ]
}

@test "claude_pane_ids: neither dir populated emits nothing" {
	run claude_pane_ids
	[ "$status" -eq 0 ]
	[ -z "$output" ]
}

@test "count_for_session: surfaces a screen-only pane (no hook, no session field)" {
	screen 7 processing $((CLAUDE_NOW - 1))
	# Screen files carry no session= field, so read_pane_state's no-hook path
	# leaves REPLY_SESSION empty — count_for_session's own session-matching
	# concern is orthogonal to this task; what we assert is that the
	# screen-only id reaches read_pane_state at all (total goes from 0 to 1).
	count_for_session ""
	[ "$total" -eq 1 ]
	[ "$count_processing" -eq 1 ]
}

@test "count_for_session: pane in both dirs counted once, not twice" {
	hook 3 processing $((CLAUDE_NOW - 5)) mysession
	screen 3 idle $((CLAUDE_NOW - 1))
	count_for_session mysession
	[ "$total" -eq 1 ]
	[ "$count_processing" -eq 1 ]
}
