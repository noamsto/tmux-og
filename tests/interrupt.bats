#!/usr/bin/env bats

load helper

setup() {
	# Export before sourcing: lib-claude derives CLAUDE_*_DIR (including the
	# interrupt verdict cache these tests write) from this at source time.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	setup_lib_claude
	PROGRESS_LOG="$BATS_TEST_TMPDIR/progress.log"
	: >"$PROGRESS_LOG"
	# shellcheck disable=SC2329  # invoked from read_pane_state
	claude_progress_emit() {
		printf '%s %s\n' "$1" "$2" >>"$PROGRESS_LOG"
	}
	PANE_DIR="$BATS_TEST_TMPDIR/panes"
	mkdir -p "$PANE_DIR"
}

# write_pane FILE STATE AGE_SECONDS [TRANSCRIPT_PATH]
write_pane() {
	local ts=$((CLAUDE_NOW - $3))
	{
		echo "state=$2"
		echo "timestamp=$ts"
		echo "session=work"
		[[ -n ${4:-} ]] && echo "transcript=$4"
	} >"$1"
	return 0
}

# write_screen FILE STATE — the agent-detect scraper reading for FILE's pane id.
write_screen() {
	mkdir -p "$CLAUDE_SCREEN_DIR"
	{
		echo "state=$2"
		echo "timestamp=$CLAUDE_NOW"
	} >"$CLAUDE_SCREEN_DIR/${1##*/}"
}

@test "read_pane_state: stale processing with marker at tail → interrupted" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' \
		'{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}' \
		'{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 60 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "interrupted" ]
	[ "$REPLY_FADE" -eq 0 ]
	[ "$REPLY_UNSEEN" -eq 1 ]
	grep -qx 'p1 clear' "$PROGRESS_LOG"
}

@test "read_pane_state: fresh processing is not checked → processing" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"text":"[Request interrupted by user]"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 2 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
	[ ! -s "$PROGRESS_LOG" ]
}

@test "read_pane_state: stale processing without marker (long tool) → processing" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 120 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "read_pane_state: marker present but not near tail (resumed) → processing" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' \
		'{"text":"[Request interrupted by user]"}' \
		'{"type":"user","message":"resumed"}' \
		'{"type":"assistant","message":"back to work"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 120 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "read_pane_state: stale processing with no transcript path → processing" {
	write_pane "$PANE_DIR/p1" processing 120 ""
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "read_pane_state: transcript file missing → processing" {
	write_pane "$PANE_DIR/p1" processing 120 "$BATS_TEST_TMPDIR/absent.jsonl"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "read_pane_state: interrupt verdict is stamped to the cache" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"text":"[Request interrupted by user]"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 60 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "interrupted" ]
	[ "$(cat "$CLAUDE_INTERRUPT_DIR/p1")" = "1" ]
}

@test "read_pane_state: cached verdict wins while the transcript is unchanged" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"type":"assistant","text":"no marker here"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 60 "$tr"
	# A stamp newer than the transcript must be trusted verbatim — re-reading
	# this marker-free transcript would say processing instead.
	mkdir -p "$CLAUDE_INTERRUPT_DIR"
	printf '1\n' >"$CLAUDE_INTERRUPT_DIR/p1"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "interrupted" ]
}

@test "read_pane_state: transcript append invalidates the cached verdict" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"text":"[Request interrupted by user]"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 60 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "interrupted" ]
	printf '%s\n' '{"type":"user","message":"resumed"}' '{"type":"assistant","message":"back"}' >>"$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
	[ "$(cat "$CLAUDE_INTERRUPT_DIR/p1")" = "0" ]
}

@test "read_pane_state: terminal states are never reclassified" {
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"text":"[Request interrupted by user]"}' >"$tr"
	write_pane "$PANE_DIR/p1" "done" 120 "$tr"
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "done" ]
}

@test "claude_priority_state: interrupted outranks processing/done/idle" {
	claude_priority_state 0 0 1 1 1 0 0 1
	[ "$REPLY" = "interrupted" ]
}

@test "claude_priority_state: error/waiting/compacting outrank interrupted" {
	claude_priority_state 0 0 0 0 0 1 0 1
	[ "$REPLY" = "error" ]
	claude_priority_state 1 0 0 0 0 0 0 1
	[ "$REPLY" = "waiting" ]
	claude_priority_state 0 1 0 0 0 0 0 1
	[ "$REPLY" = "compacting" ]
}

@test "interrupted resolves to a glyph and a color" {
	claude_state_icon interrupted
	[ -n "$REPLY" ]
	setup_claude_colors
	claude_faded_hex interrupted
	[ -n "$REPLY" ]
}

# The screen scraper only tells an active spinner from a quiet input box, so it
# must never downgrade a human-blocking hook state (it reads those as idle).

@test "read_pane_state: stale waiting is NOT downgraded by a screen idle reading" {
	write_pane "$PANE_DIR/p1" waiting 60 # past CLAUDE_STALE_WAITING (30)
	write_screen "$PANE_DIR/p1" idle
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "waiting" ]
}

@test "read_pane_state: stale error/denied survive a screen idle reading" {
	write_pane "$PANE_DIR/p1" error 300
	write_screen "$PANE_DIR/p1" idle
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "error" ]

	write_pane "$PANE_DIR/p2" denied 300
	write_screen "$PANE_DIR/p2" idle
	read_pane_state "$PANE_DIR/p2"
	[ "$REPLY" = "denied" ]
}

@test "read_pane_state: stale active state is still corrected from the screen" {
	write_pane "$PANE_DIR/p1" compacting 120 # past CLAUDE_STALE_COMPACTING (60)
	write_screen "$PANE_DIR/p1" idle
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "idle" ]

	: >"$PROGRESS_LOG"
	write_pane "$PANE_DIR/p1" processing 400 # past CLAUDE_STALE_PROCESSING (300)
	write_screen "$PANE_DIR/p1" idle
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "idle" ]
	grep -qx 'p1 clear' "$PROGRESS_LOG"
}

@test "claude_priority_state: denied outranks compacting/processing, loses to waiting/error" {
	# args: waiting compacting processing done idle error denied interrupted
	claude_priority_state 0 1 1 0 0 0 1 0
	[ "$REPLY" = "denied" ]
	claude_priority_state 1 0 0 0 0 0 1 0
	[ "$REPLY" = "waiting" ]
	claude_priority_state 0 0 0 0 0 1 1 0
	[ "$REPLY" = "error" ]
}
