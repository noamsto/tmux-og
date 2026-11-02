#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
# Tests the readiness wait of the newborn-agent repaint nudge (#883, #1000):
# scripts/tmux-agent-repaint.sh waits out startup and an output-quiet moment,
# then does one real size change pair (resize-window down and back).
#
# Runs on a scratch server with no client attached: the script is started by
# the client-independent @og-sweep-tick claim, so its readiness read must work
# without one. `pane-resized` counts real size changes in @resizes (unset until
# the first), so a nudge is `@resizes` = 2.
#
# A logging tmux wrapper sits first on PATH (the script calls bare `tmux`) and
# appends its argv to calls.log, so "how many reads did the wait cost" is a
# line count. The script is always run as `bash <script>`: the nix sandbox has
# no /usr/bin for its shebang.

# Retry budget for the "poll until <condition>" loops (0.1s sleeps, so
# POLL_TRIES * 0.1 is the wall-clock bound in seconds).
POLL_TRIES=300

setup() {
	unset TMUX TMUX_PANE
	REAL_TMUX="$(command -v tmux)" || skip "tmux not on PATH"
	REPO_ROOT="$(cd "$(dirname "$BATS_TEST_FILENAME")/.." && pwd)"
	SCRIPT="$REPO_ROOT/scripts/tmux-agent-repaint.sh"

	# Short and fixed: $BATS_TEST_TMPDIR/tmux-<uid>/default can pass the
	# ~104-byte AF_UNIX socket path limit (darwin build tmpdir).
	TMUX_TMPDIR="/tmp/ogar-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"

	CALLS="$BATS_TEST_TMPDIR/calls.log"
	mkdir -p "$BATS_TEST_TMPDIR/bin"
	cat >"$BATS_TEST_TMPDIR/bin/tmux" <<EOF
#!$(command -v bash)
printf '%s\n' "\$*" >>'$CALLS'
exec '$REAL_TMUX' "\$@"
EOF
	chmod +x "$BATS_TEST_TMPDIR/bin/tmux"
	export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "${TMUX_TMPDIR:-}"
}

# start_server <pane command> — one detached single-pane window, no client.
start_server() {
	tmux -f /dev/null new-session -d -s S -x 80 -y 24 "$1" 3>&-
	PANE="$(tmux display-message -p -t S '#{pane_id}')"
	tmux set-hook -g pane-resized 'set -gF @resizes "#{e|+:#{@resizes},1}"'
}

pane_size() { tmux display-message -p -t "$PANE" '#{pane_width}x#{pane_height}'; }
resizes() { tmux show -gqv @resizes; }
resized_twice() { [[ "$(resizes)" == 2 ]]; }
has_output() { [[ -n "$(tmux display-message -p -t "$PANE" '#{pane_last_output_time}')" ]]; }

# wait_for <cmd...> — poll until the predicate holds, then report its verdict.
wait_for() {
	local i
	for ((i = 0; i < POLL_TRIES; i++)); do
		"$@" && return 0
		sleep 0.1
	done
	"$@"
}

@test "quiet pane nudges once ready, on one pane_last_output_time read" {
	start_server "sh -c 'printf hi; exec sleep 600'"
	local before
	before="$(pane_size)"
	wait_for has_output
	: >"$CALLS"

	run bash "$SCRIPT" "$PANE" 1x1
	[ "$status" -eq 0 ]
	wait_for resized_twice
	[ "$(resizes)" = 2 ]
	[ "$(pane_size)" = "$before" ]

	local reads read_line resize_line
	reads="$(grep -c '^display .*pane_last_output_time' "$CALLS" || true)"
	read_line="$(grep -n -m1 '^display .*pane_last_output_time' "$CALLS" | cut -d: -f1)"
	resize_line="$(grep -n -m1 '^resize-window ' "$CALLS" | cut -d: -f1)"
	if [[ $reads -ne 1 || -z $read_line || -z $resize_line || $read_line -ge $resize_line ]]; then
		printf 'want one pane_last_output_time display before the first resize-window; reads=%s read_line=%s resize_line=%s\n' \
			"$reads" "$read_line" "$resize_line" >&2
		cat "$CALLS" >&2
		return 1
	fi
}

@test "busy pane waits for quiet before nudging" {
	start_server "sh -c 'i=0; while [ \$i -lt 30 ]; do echo busy; sleep 0.2; i=\$((i + 1)); done; exec sleep 600'"
	: >"$CALLS"

	bash "$SCRIPT" "$PANE" 1x1 3>&- &
	local pid=$!
	sleep 5
	# The worker must not outlive a failed assertion.
	if [[ -n "$(resizes)" ]]; then
		kill "$pid" 2>/dev/null || true
		printf '@resizes=%s at 5 s, while the pane was still printing\n' "$(resizes)" >&2
		return 1
	fi
	wait_for resized_twice
	[ "$(resizes)" = 2 ]
	wait "$pid"
}

@test "busy-forever pane is nudged at the 12 s cap" {
	start_server "sh -c 'i=0; while [ \$i -lt 300 ]; do echo x; sleep 0.2; i=\$((i + 1)); done; exec sleep 600'"

	bash "$SCRIPT" "$PANE" 1x1 3>&- &
	local pid=$!
	sleep 10
	# The worker must not outlive a failed assertion.
	if [[ -n "$(resizes)" ]]; then
		kill "$pid" 2>/dev/null || true
		printf '@resizes=%s at 10 s, before the 12 s cap\n' "$(resizes)" >&2
		return 1
	fi
	local i
	for ((i = 0; i < 60; i++)); do
		resized_twice && break
		sleep 0.1
	done
	[ "$(resizes)" = 2 ]
	wait "$pid"
}

@test "never-output pane counts as quiet" {
	start_server 'sleep 600'
	[ -z "$(tmux display-message -p -t "$PANE" '#{pane_last_output_time}')" ]

	bash "$SCRIPT" "$PANE" 1x1 3>&- &
	local pid=$!
	wait_for resized_twice
	[ "$(resizes)" = 2 ]
	wait "$pid"
}

@test "birth size equal to the current size is a no-op" {
	start_server 'sleep 600'

	run bash "$SCRIPT" "$PANE" "$(pane_size)"
	[ "$status" -eq 0 ]
	[ -z "$(resizes)" ]
}
