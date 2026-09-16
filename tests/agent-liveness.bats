#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# Tests the dead-agent floor: the presence sweep in tmux-update-icons.sh (writer)
# and the read_pane_state veto it feeds in lib-claude.sh (reader).

load helper

setup() {
	# Export before sourcing: lib-claude derives CLAUDE_*_DIR (including the
	# live/ stamps these tests write) from this at source time.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	PANE_DIR="$BATS_TEST_TMPDIR/panes"
	mkdir -p "$PANE_DIR"
}

# load_lib [ASSUME_DEAD_AFTER]
# Sources lib-claude with the floor set. No argument leaves the env override
# unset, so the shipped `@assume_dead_after@` placeholder is what gets parsed.
load_lib() {
	if [[ -n ${1:-} ]]; then
		export CLAUDE_ASSUME_DEAD_AFTER="$1"
	else
		unset CLAUDE_ASSUME_DEAD_AFTER
	fi
	setup_lib_claude
}

# write_pane FILE STATE AGE_SECONDS [TRANSCRIPT_PATH]
write_pane() {
	local ts=$((CLAUDE_NOW - $3))
	{
		echo "state=$2"
		echo "timestamp=$ts"
		[[ -n ${4:-} ]] && echo "transcript=$4"
	} >"$1"
	return 0
}

# write_live ID AGE_SECONDS — a presence stamp aged AGE_SECONDS.
write_live() {
	mkdir -p "$CLAUDE_LIVE_DIR"
	printf '%s\n' "$((CLAUDE_NOW - $2))" >"$CLAUDE_LIVE_DIR/$1"
}

# --- reader: the veto is unreachable while the floor is off ---

@test "unsubstituted placeholder parses as 0 (floor off)" {
	load_lib
	[ "$CLAUDE_ASSUME_DEAD_AFTER" -eq 0 ]
}

@test "floor off: a fully dead-looking pane still reports its stale state" {
	load_lib
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

# --- reader: the veto fires only on positive, fresh evidence ---

@test "stale state + lagging stamp + fresh sweep → withdrawn" {
	load_lib 60
	PROGRESS_LOG="$BATS_TEST_TMPDIR/progress.log"
	: >"$PROGRESS_LOG"
	# shellcheck disable=SC2329  # invoked from read_pane_state
	claude_progress_emit() {
		printf '%s %s\n' "$1" "$2" >>"$PROGRESS_LOG"
	}
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 600
	write_live .sweep 0
	run read_pane_state "$PANE_DIR/p1"
	[ "$status" -eq 1 ]
	grep -qx 'p1 clear' "$PROGRESS_LOG"
}

@test "a stamp the sweep is still refreshing keeps the state" {
	load_lib 60
	PROGRESS_LOG="$BATS_TEST_TMPDIR/progress.log"
	: >"$PROGRESS_LOG"
	# shellcheck disable=SC2329  # invoked from read_pane_state
	claude_progress_emit() {
		printf '%s %s\n' "$1" "$2" >>"$PROGRESS_LOG"
	}
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 2
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
	[ ! -s "$PROGRESS_LOG" ]
}

@test "a stale sweep deactivates presence entirely" {
	load_lib 60
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 600
	write_live .sweep 300
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "a missing per-pane stamp is not evidence" {
	load_lib 60
	write_pane "$PANE_DIR/p1" processing 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "a missing sweep stamp is not evidence" {
	load_lib 60
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 600
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "a half-written stamp is read as absent, not as decades stale" {
	load_lib 60
	write_pane "$PANE_DIR/p1" processing 600
	write_live .sweep 0
	mkdir -p "$CLAUDE_LIVE_DIR"
	printf '17' >"$CLAUDE_LIVE_DIR/p1" # truncated mid-write: no newline
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "a fresh state is never withdrawn, however old the stamp" {
	load_lib 60
	write_pane "$PANE_DIR/p1" processing 5
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

# --- reader: which states may be withdrawn ---

@test "waiting is never withdrawn" {
	load_lib 60
	write_pane "$PANE_DIR/p1" waiting 600
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "waiting" ]
}

@test "error is never withdrawn" {
	load_lib 60
	write_pane "$PANE_DIR/p1" error 600
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "error" ]
}

@test "idle is never withdrawn" {
	load_lib 60
	write_pane "$PANE_DIR/p1" idle 600
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "idle" ]
}

@test "done is withdrawn once past its own staleness threshold" {
	load_lib 60
	write_pane "$PANE_DIR/p1" "done" 600
	write_live p1 600
	write_live .sweep 0
	run read_pane_state "$PANE_DIR/p1"
	[ "$status" -eq 1 ]
}

@test "an interrupted turn survives the veto" {
	load_lib 60
	local tr="$BATS_TEST_TMPDIR/t.jsonl"
	printf '%s\n' '{"text":"[Request interrupted by user]"}' >"$tr"
	write_pane "$PANE_DIR/p1" processing 600 "$tr"
	write_live p1 600
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "interrupted" ]
}

# --- reader: the sub-15s clamp ---

@test "a threshold under the sweep window is clamped to it" {
	load_lib 5
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 10 # older than 5s, younger than the 15s floor
	write_live .sweep 0
	read_pane_state "$PANE_DIR/p1"
	[ "$REPLY" = "processing" ]
}

@test "a clamped threshold still withdraws past the floor" {
	load_lib 5
	write_pane "$PANE_DIR/p1" processing 600
	write_live p1 20
	write_live .sweep 0
	run read_pane_state "$PANE_DIR/p1"
	[ "$status" -eq 1 ]
}

# --- writer: the presence sweep in tmux-update-icons ---

# setup_sweep [ASSUME_DEAD_AFTER]
# A fake tmux reporting one agent pane (%3, unpiped) and one shell pane (%5).
# The pipe-pane handler records whether .sweep already existed when it ran, which
# is the write ordering the reader depends on. lib-claude is a build-time
# placeholder in the raw script, so the constants it would supply are injected
# here, exactly as AGENT_COMMANDS already is.
setup_sweep() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		case "\$*" in
		*"list-panes"*) printf '%%3|codex|0\n%%5|fish|0\n' ;;
		*"pipe-pane"*)
			[ -e "$BATS_TEST_TMPDIR/live/.sweep" ] && echo early >>"$BATS_TEST_TMPDIR/order.log"
			echo "\$@" >>"$BATS_TEST_TMPDIR/pipe.log" ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
	export AGENT_DETECT_BIN="agent-detect"
	export AGENT_COMMANDS="claude codex"
	export CLAUDE_LIVE_DIR="$BATS_TEST_TMPDIR/live"
	# Multiple of 5 so the every-5th-tick throttle lets the sweep run.
	export CLAUDE_NOW=100
	: >"$BATS_TEST_TMPDIR/pipe.log"
	: >"$BATS_TEST_TMPDIR/order.log"
	if [[ -n ${1:-} ]]; then
		export CLAUDE_ASSUME_DEAD_AFTER="$1"
	else
		unset CLAUDE_ASSUME_DEAD_AFTER
	fi
}

@test "sweep: floor off writes nothing" {
	setup_sweep
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -d "$BATS_TEST_TMPDIR/live" ]
}

@test "sweep calls claude_reap_dead_panes with the fetched rows" {
	setup_sweep
	# setup_sweep's CLAUDE_NOW=100 fails the 60s reap gate (100 % 60 != 0).
	export CLAUDE_NOW=120
	run bash -c '
		claude_reap_dead_panes() { printf "%s" "$1" >"'"$BATS_TEST_TMPDIR"'/reap.log"; }
		source scripts/tmux-update-icons.sh
		arm_agent_detect
	'
	[ "$status" -eq 0 ]
	[[ "$(cat "$BATS_TEST_TMPDIR/reap.log")" == *'%3'*'codex'* ]]
}

@test "a non-empty first argument (the monitor-hook sweep caller) skips the reap entirely" {
	# claude_reap_dead_panes deletes from CLAUDE_STATUS_DIR by checking pane
	# ids against THIS CALLER's own list-panes -a -- safe for the per-tick
	# status-format caller (gated by this server's own attached client), but
	# the monitor-hook sweep runs on a client-independent timer on every
	# wrapped-tmux server on the machine, which shares that same /tmp
	# directory regardless of TMUX_TMPDIR/-L isolation. A non-empty first
	# argument is exactly how main() marks that caller (see its
	# OG_TICK_SWEEP dispatch), so arm_agent_detect must not reap on it.
	setup_sweep
	run bash -c '
		claude_reap_dead_panes() { printf "%s" "$1" >"'"$BATS_TEST_TMPDIR"'/reap.log"; }
		source scripts/tmux-update-icons.sh
		arm_agent_detect force
	'
	[ "$status" -eq 0 ]
	[ ! -s "$BATS_TEST_TMPDIR/reap.log" ]
}

@test "sweep still reaps when both arm and stamp are off" {
	# arm=0 (AGENT_DETECT_BIN falls back to the unsubstituted placeholder) and
	# stamp=0 (no ASSUME_DEAD_AFTER) together used to bail before the
	# list-panes roundtrip ever happened — the reap must not share that fate,
	# since it (unlike arm/stamp) has to work regardless of either feature.
	setup_sweep
	# setup_sweep's CLAUDE_NOW=100 fails the 60s reap gate (100 % 60 != 0).
	export CLAUDE_NOW=120
	run bash -c '
		unset AGENT_DETECT_BIN
		claude_reap_dead_panes() { printf "%s" "$1" >"'"$BATS_TEST_TMPDIR"'/reap.log"; }
		source scripts/tmux-update-icons.sh
		arm_agent_detect
	'
	[ "$status" -eq 0 ]
	[[ "$(cat "$BATS_TEST_TMPDIR/reap.log")" == *'%3'*'codex'* ]]
}

@test "sweep: a zero threshold writes nothing" {
	setup_sweep 0
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -d "$BATS_TEST_TMPDIR/live" ]
}

@test "sweep: stamps the agent pane and the completed pass" {
	setup_sweep 60
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ "$(cat "$BATS_TEST_TMPDIR/live/3")" = "100" ]
	[ "$(cat "$BATS_TEST_TMPDIR/live/.sweep")" = "100" ]
}

@test "sweep: does not stamp a non-agent pane" {
	setup_sweep 60
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -e "$BATS_TEST_TMPDIR/live/5" ]
}

@test "sweep: stamps a nix-wrapped agent pane" {
	setup_sweep 60
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		case "\$*" in
		*"list-panes"*) printf '%%3|.codex-wrapped|0\n' ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ "$(cat "$BATS_TEST_TMPDIR/live/3")" = "100" ]
}

@test "sweep: .sweep is written after the per-pane stamps" {
	setup_sweep 60
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ -s "$BATS_TEST_TMPDIR/pipe.log" ] # the ordering probe actually ran
	[ ! -s "$BATS_TEST_TMPDIR/order.log" ]
}

@test "sweep: stamps even when agent-detect is not wired" {
	setup_sweep 60
	run bash -c 'unset AGENT_DETECT_BIN; source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -s "$BATS_TEST_TMPDIR/pipe.log" ]
	[ -e "$BATS_TEST_TMPDIR/live/3" ]
}

@test "sweep: a failed list-panes writes no .sweep" {
	# Without the bail this stamped .sweep anyway, and a transient tmux failure
	# withdrew a running agent's state.
	setup_sweep 60
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$*" in
		*"list-panes"*) exit 1 ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -e "$BATS_TEST_TMPDIR/live/.sweep" ]
}

@test "sweep: a pass that succeeds with no panes still completes" {
	# The other half of the bail: "the call failed" and "the call found nothing"
	# must not look alike.
	setup_sweep 60
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$*" in
		*"list-panes"*) : ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash -c 'source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ -e "$BATS_TEST_TMPDIR/live/.sweep" ]
	# The blank row must not have been stamped as a pane.
	[ "$(find "$BATS_TEST_TMPDIR/live" -type f -not -name .sweep | wc -l)" -eq 0 ]
}

@test "sweep: the every-5th-tick throttle gates the stamps too" {
	setup_sweep 60
	run bash -c 'export CLAUDE_NOW=101; source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -d "$BATS_TEST_TMPDIR/live" ]
}

# --- prune: stamps don't outlive a tmux server ---

@test "claude_prune_stale_state drops a previous server's stamps" {
	load_lib 60
	write_live p1 600
	write_live .sweep 600
	claude_prune_stale_state "$((CLAUDE_NOW + 10))"
	[ ! -e "$CLAUDE_LIVE_DIR/p1" ]
	# .sweep is a dotfile and escapes the glob on purpose: a stale one already
	# deactivates presence by content, and the next sweep overwrites it.
	[ -e "$CLAUDE_LIVE_DIR/.sweep" ]
}
