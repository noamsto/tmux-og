#!/usr/bin/env bats
# Live proof for #647: pane-exited / pane-died on the wrapped server delete
# exactly one pane's claude-status files, and a second server sharing
# CLAUDE_STATUS_DIR cannot reap a foreign session's colliding pane id.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	SOCKET="og-reap-pane-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# Mandatory (#603): these hooks fire inside this test server and
	# reach functions that delete files under these dirs, whose defaults are
	# the developer's real /tmp trees.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}
}

teardown() {
	t kill-server 2>/dev/null || true
	# Case 3 runs a second wrapped server on its own socket; belt-and-suspenders
	# in case an assertion failure skipped its own cleanup.
	if [ -n "${SCRATCH_SOCK:-}" ]; then
		timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" kill-server 2>/dev/null || true
		tmux -S "$SCRATCH_SOCK" kill-server 2>/dev/null || true
	fi
	return 0
}

t() {
	timeout --foreground 30s "$TMUX_BIN" -L "$SOCKET" "$@"
}

tb() {
	timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" "$@"
}

wait_for() {
	# wait_for <seconds> <command...> -- polls until <command...> exits 0.
	local timeout=$1
	shift
	local remaining=$timeout
	while ((remaining-- > 0)); do
		"$@" && return 0
		sleep 1
	done
	return 1
}

# Files on disk are the bare N (lib-claude strips %).
bare_id() {
	local id="$1"
	id="${id#%}"
	printf '%s' "$id"
}

seed_pane_state() {
	local id sess
	id="$(bare_id "$1")"
	sess="$2"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}
	printf 'state=processing\ntimestamp=1000000000\nsession=%s\n' "$sess" \
		>"$CLAUDE_STATUS_DIR/panes/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/screen/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/interrupt/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/tasks/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/issues/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/watchers/$id"
}

six_gone() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt tasks issues watchers; do
		[ ! -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

six_present() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt tasks issues watchers; do
		[ -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

kill_pane_cmd() {
	# $1 = helper (t or tb), $2 = pane id. SIGKILL the pane process — measured
	# as pane-exited (remain-on-exit off) / pane-died (on).
	local helper=$1 pane=$2 pid
	pid="$("$helper" display-message -p -t "$pane" '#{pane_pid}')"
	kill -9 "$pid"
}

@test "pane-exited deletes exactly that pane's six files; sibling stays" {
	t new-session -d -s alpha -x 80 -y 24 -c "$PWD" -- sleep 300
	local live dead
	live="$(t list-panes -t alpha -F '#{pane_id}')"
	dead="$(t split-window -P -F '#{pane_id}' -t alpha -c "$PWD" -- sleep 300)"
	[ -n "$live" ]
	[ -n "$dead" ]
	[ "$live" != "$dead" ]

	seed_pane_state "$live" alpha
	seed_pane_state "$dead" alpha
	six_present "$live"
	six_present "$dead"

	kill_pane_cmd t "$dead"
	wait_for 5 six_gone "$dead"
	six_present "$live"
}

@test "pane-died corpse: six files go while list-panes still shows pane_dead" {
	t new-session -d -s corpse -x 80 -y 24 -c "$PWD" -- sleep 300
	t set-option -w -t corpse remain-on-exit on
	local id
	id="$(t list-panes -t corpse -F '#{pane_id}')"
	[ -n "$id" ]
	seed_pane_state "$id" corpse
	six_present "$id"

	kill_pane_cmd t "$id"
	wait_for 5 six_gone "$id"
	[ "$(t list-panes -t corpse -F '#{pane_dead}')" = 1 ]
}

@test "cross-server guard: colliding pane id with session=alpha is skipped on beta" {
	t new-session -d -s alpha -x 80 -y 24 -c "$PWD" -- sleep 300
	local a_live a_dead
	a_live="$(t list-panes -t alpha -F '#{pane_id}')"
	a_dead="$(t split-window -P -F '#{pane_id}' -t alpha -c "$PWD" -- sleep 300)"

	SCRATCH_SOCK="$BATS_TEST_TMPDIR/scratch.sock"
	tb new-session -d -s beta -x 80 -y 24 -c "$PWD" -- sleep 300
	local b_live b_dead
	b_live="$(tb list-panes -t beta -F '#{pane_id}')"
	b_dead="$(tb split-window -P -F '#{pane_id}' -t beta -c "$PWD" -- sleep 300)"

	[ "$a_dead" = "$b_dead" ]
	[ "$a_live" = "$b_live" ]

	seed_pane_state "$a_dead" alpha
	six_present "$a_dead"

	kill_pane_cmd tb "$b_dead"
	sleep 3
	six_present "$a_dead"

	kill_pane_cmd t "$a_dead"
	wait_for 5 six_gone "$a_dead"
}
