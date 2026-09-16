#!/usr/bin/env bats
# Live proof for #646: pane-shell-prompt (OSC 133;A) on the wrapped server clears
# an exited agent's state from the shared files AND the bridge options, and its
# pane_current_command discriminator refuses a nested prompt inside a still-running
# agent. A shell that emits no OSC 133 leaves the state alone (the dead-agent
# floor is the only withdrawal path), and a colliding pane id on a second server
# cannot clear a foreign session's file (the ownership guard).

load helper

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	SOCKET="og-pane-shell-prompt-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# Mandatory (#603/#647 precedent): these hooks fire inside this test server
	# and delete files under these dirs, whose defaults are the real /tmp trees.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}
}

teardown() {
	t kill-server 2>/dev/null || true
	if [ -n "${SCRATCH_SOCK:-}" ]; then
		timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" kill-server 2>/dev/null || true
	fi
	return 0
}

t() {
	timeout --foreground 30s "$TMUX_BIN" -L "$SOCKET" "$@"
}

tb() {
	timeout --foreground 30s "$TMUX_BIN" -S "$SCRATCH_SOCK" "$@"
}

bare_id() {
	local id="$1"
	id="${id#%}"
	printf '%s' "$id"
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

# seed_shell_state PANE_ID SESSION — state + screen + interrupt + the two
# bridge options, the exact set claude_clear_agent_state clears. Hand-writes
# panes/<id> directly; seed_shell_state_via_writer (below) exercises the real
# writer instead, so at least one test breaks if that format drifts.
seed_shell_state() {
	local id sess
	id="$(bare_id "$1")"
	sess="$2"
	printf 'state=processing\ntimestamp=1000000000\nsession=%s\n' "$sess" \
		>"$CLAUDE_STATUS_DIR/panes/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/screen/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/interrupt/$id"
	t set-option -p -t "$1" @claude_status "processing 1000000000 0"
	t set-option -p -t "$1" @agent_screen "processing 1000000000"
}

# seed_shell_state_via_writer PANE_ID SESSION — same net effect as
# seed_shell_state, but panes/<id> is written by the real claude-status-update
# writer (claude-status-update.sh ~:479/:585) instead of a hand-rolled printf,
# so a writer format change (the session=/state= shape) breaks this suite too.
# --session is passed explicitly rather than relying on the writer's own
# `tmux display-message -p -t "$pane_id" '#{session_name}'` fallback, since
# that call targets the default tmux server, not this test's `-L $SOCKET` one.
# bridge_stamp inside the writer no-ops here (no $TMUX in this shell), so the
# two bridge options are still set by hand, like screen/interrupt.
seed_shell_state_via_writer() {
	local id sess
	id="$(bare_id "$1")"
	sess="$2"
	CLAUDE_STATUS_DIR="$CLAUDE_STATUS_DIR" bash scripts/claude-status-update.sh \
		processing --pane "$id" --session "$sess"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/screen/$id"
	printf 'x\n' >"$CLAUDE_STATUS_DIR/interrupt/$id"
	t set-option -p -t "$1" @claude_status "processing 1000000000 0"
	t set-option -p -t "$1" @agent_screen "processing 1000000000"
}

three_gone() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt; do
		[ ! -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

three_present() {
	local id d
	id="$(bare_id "$1")"
	for d in panes screen interrupt; do
		[ -e "$CLAUDE_STATUS_DIR/$d/$id" ] || return 1
	done
}

# The pane shell is bash: its builtin printf is fork-free, so pane_current_command
# stays `bash` when we printf the mark — exactly the "shell is at the prompt" case.

@test "agent exits to prompt: files and bridge options cleared" {
	t new-session -d -s one -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t one -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state_via_writer "$id" one
	three_present "$id"

	t send-keys -t "$id" 'printf "\033]133;A\033\\"' Enter
	wait_for 5 three_gone "$id"
	# -F '#'{option_value}' is the raw read: bare show-options prints an empty
	# option as '' (the quote-escaping template), which -z would read as non-empty.
	[ -z "$(t show-options -p -t "$id" -F '#{option_value}' @claude_status 2>/dev/null)" ]
	[ -z "$(t show-options -p -t "$id" -F '#{option_value}' @agent_screen 2>/dev/null)" ]
}

@test "nested prompt inside a live agent: not cleared" {
	# pane_current_command comes from osdep_get_name: on Linux it reads
	# /proc/<pgrp>/cmdline (argv[0]), so `exec -a pi bash ...` fakes an agent
	# fine there — but on macOS it reads the kernel process name via
	# proc_pidinfo/pbsi_comm (tmux e880cf63, osdep-darwin.c), which tracks the
	# executable's own file name and ignores an argv[0] rewrite. exec -a pi
	# therefore still reports `bash` on darwin, the handler correctly sees a
	# non-agent foreground, and the state gets cleared — a false test failure,
	# not a handler bug. Use a real file named `pi` (a copy, not a symlink: a
	# symlink's kernel name is the target's) so the kernel process name is
	# `pi` on both platforms.
	mkdir -p "$TEST_HOME/bin"
	cp "$(command -v bash)" "$TEST_HOME/bin/pi"
	chmod +x "$TEST_HOME/bin/pi"
	# A script for the copied binary to interpret, so pane_current_command
	# (the interpreter's own kernel name) reads `pi`, not the script's name.
	cat >"$TEST_HOME/pi-script.sh" <<'EOF'
printf "\033]133;A\033\\"
sleep 3
EOF

	t new-session -d -s two -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t two -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state "$id" two
	three_present "$id"

	# The fake agent's file name is `pi` (in the manifest) while it runs and
	# emits a nested A — the handler must read it as a live agent and skip.
	# One send-keys argument: multiple args concatenate with no separator.
	t send-keys -t "$id" "$TEST_HOME/bin/pi $TEST_HOME/pi-script.sh" Enter
	sleep 1
	three_present "$id"
	sleep 3 # let the fake agent drain before teardown
}

@test "no OSC 133 emitted: state stays (the floor is the only withdrawal)" {
	t new-session -d -s three -x 80 -y 24 -c "$PWD" -- bash
	local id
	id="$(t list-panes -t three -F '#{pane_id}')"
	[ -n "$id" ]
	seed_shell_state "$id" three
	three_present "$id"

	t send-keys -t "$id" 'echo hi' Enter
	sleep 2
	three_present "$id"
}

@test "cross-server guard: a colliding pane id in a foreign session is skipped" {
	t new-session -d -s alpha -x 80 -y 24 -c "$PWD" -- bash
	local a_id
	a_id="$(t list-panes -t alpha -F '#{pane_id}')"
	[ -n "$a_id" ]
	# Seed alpha's own panes/<id> file; the option seeding is irrelevant here.
	printf 'state=processing\ntimestamp=1000000000\nsession=alpha\n' \
		>"$CLAUDE_STATUS_DIR/panes/$(bare_id "$a_id")"

	SCRATCH_SOCK="$BATS_TEST_TMPDIR/scratch.sock"
	tb new-session -d -s beta -x 80 -y 24 -c "$PWD" -- bash
	local b_id
	b_id="$(tb list-panes -t beta -F '#{pane_id}')"
	[ -n "$b_id" ]
	# Both are first panes: the ids collide (per-server %N).
	[ "$a_id" = "$b_id" ]

	# beta's prompt fires the hook; the handler reads session=alpha vs beta and
	# must refuse to clear.
	tb send-keys -t "$b_id" 'printf "\033]133;A\033\\"' Enter
	sleep 2
	[ -e "$CLAUDE_STATUS_DIR/panes/$(bare_id "$a_id")" ]
}

# Unit-level: claude_clear_agent_state's ownership guard, exercised by sourcing
# lib-claude.sh directly — no live tmux server needed (claude_progress_emit and
# the trailing `tmux set` both self-guard with no server on PATH/reachable).

@test "ownership guard: empty caller session does not clear (fails closed)" {
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh
	mkdir -p "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR"
	printf 'state=processing\ntimestamp=1000000000\nsession=real\n' >"$CLAUDE_PANES_DIR/9"
	: >"$CLAUDE_SCREEN_DIR/9"
	: >"$CLAUDE_INTERRUPT_DIR/9"

	claude_clear_agent_state 9 ""

	[ -e "$CLAUDE_PANES_DIR/9" ]
	[ -e "$CLAUDE_SCREEN_DIR/9" ]
	[ -e "$CLAUDE_INTERRUPT_DIR/9" ]
}

@test "ownership guard: unterminated session= line with mismatching session does not clear" {
	# shellcheck source=/dev/null
	source scripts/lib-claude.sh
	mkdir -p "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR"
	# No trailing newline after the last (session=) line.
	printf 'state=processing\ntimestamp=1000000000\nsession=real' >"$CLAUDE_PANES_DIR/9"
	: >"$CLAUDE_SCREEN_DIR/9"
	: >"$CLAUDE_INTERRUPT_DIR/9"

	claude_clear_agent_state 9 other

	[ -e "$CLAUDE_PANES_DIR/9" ]
	[ -e "$CLAUDE_SCREEN_DIR/9" ]
	[ -e "$CLAUDE_INTERRUPT_DIR/9" ]
}
