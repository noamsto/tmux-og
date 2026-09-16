#!/usr/bin/env bats

load helper

setup() {
	# Export before sourcing: lib-claude derives CLAUDE_*_DIR from this at source time.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	unset TMUX TMUX_PANE
	setup_lib_claude
	mkdir -p "$CLAUDE_NAMES_DIR" "$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_PANES_DIR" \
		"$CLAUDE_INTERRUPT_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_WATCHERS_DIR" "$CLAUDE_LIVE_DIR"
}

seed_reap_files() {
	local id="$1" dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		printf 'x' >"$dir/$id"
	done
}

# Fake tmux on PATH. $1 is has-session's exit status (0 = session exists).
# display-message (claude_progress_emit) fails closed; the helper never aborts.
install_fake_tmux() {
	local has_session="${1:-1}"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		case "\$*" in
		*"has-session"*) exit $has_session ;;
		*) exit 1 ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
}

# A server that booted mid-2017 — after the fixed "old" mtime below, before now.
SERVER_START=1500000000

# stamp FILE — write FILE and backdate it to 2000 (older than any real server).
stamp() {
	printf 'x' >"$1"
	touch -t 200001010000 "$1"
}

@test "prune drops files older than server start, keeps fresh ones" {
	stamp "$CLAUDE_NAMES_DIR/8"            # pre-restart: stale
	printf 'fresh' >"$CLAUDE_NAMES_DIR/10" # written now: current server
	claude_prune_stale_state "$SERVER_START"
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ -e "$CLAUDE_NAMES_DIR/10" ]
}

@test "prune sweeps every pane-keyed dir" {
	stamp "$CLAUDE_NAMES_DIR/8"
	stamp "$CLAUDE_TASKS_DIR/8"
	stamp "$CLAUDE_PANES_DIR/8"
	stamp "$CLAUDE_INTERRUPT_DIR/8"
	claude_prune_stale_state "$SERVER_START"
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ ! -e "$CLAUDE_TASKS_DIR/8" ]
	[ ! -e "$CLAUDE_PANES_DIR/8" ]
	[ ! -e "$CLAUDE_INTERRUPT_DIR/8" ]
}

@test "prune records the server start marker" {
	claude_prune_stale_state "$SERVER_START"
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start")" = "$SERVER_START" ]
}

@test "prune is a no-op for the same server (marker gate)" {
	claude_prune_stale_state "$SERVER_START"
	# A stale file appearing after the gate is set must survive — the scan only
	# runs once per server, not every tick.
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state "$SERVER_START"
	[ -e "$CLAUDE_NAMES_DIR/8" ]
}

@test "prune re-runs when the server start changes" {
	claude_prune_stale_state "$SERVER_START"
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state $((SERVER_START + 1000))
	[ ! -e "$CLAUDE_NAMES_DIR/8" ]
	[ "$(cat "$CLAUDE_STATUS_DIR/.server_start")" = "$((SERVER_START + 1000))" ]
}

@test "prune with empty server start is a no-op" {
	stamp "$CLAUDE_NAMES_DIR/8"
	claude_prune_stale_state ""
	[ -e "$CLAUDE_NAMES_DIR/8" ]
	[ ! -e "$CLAUDE_STATUS_DIR/.server_start" ]
}

@test "reap drops dead pane files across panes/screen/interrupt/tasks/issues/watchers, keeps live ones" {
	local rows
	rows="$(printf '%%3|codex|0\n%%5|fish|0\n')"
	local dir id
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		for id in 3 5 8; do
			printf 'x' >"$dir/$id"
		done
	done
	claude_reap_dead_panes "$rows"
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ ! -e "$dir/8" ]
		[ -e "$dir/3" ]
		[ -e "$dir/5" ]
	done
}

@test "reap never touches CLAUDE_LIVE_DIR" {
	local rows
	rows="$(printf '%%3|codex|0\n')"
	printf 'x' >"$CLAUDE_LIVE_DIR/8"
	claude_reap_dead_panes "$rows"
	[ -e "$CLAUDE_LIVE_DIR/8" ]
}

@test "reap with empty rows is a no-op" {
	printf 'x' >"$CLAUDE_PANES_DIR/8"
	claude_reap_dead_panes ""
	[ -e "$CLAUDE_PANES_DIR/8" ]
}

@test "reap with a live pane id that has no files is a no-op" {
	local rows
	rows="$(printf '%%3|codex|0\n')"
	claude_reap_dead_panes "$rows"
}

@test "reap_pane deletes exactly the named pane's six files, keeps a sibling's" {
	install_fake_tmux 0
	seed_reap_files 5
	seed_reap_files 8
	claude_reap_pane 5
	local dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ ! -e "$dir/5" ]
		[ -e "$dir/8" ]
	done
}

@test "reap_pane rejects empty, garbage, %12x, and command-injection ids" {
	install_fake_tmux 0
	seed_reap_files 5
	claude_reap_pane ""
	claude_reap_pane garbage
	claude_reap_pane '%12x'
	claude_reap_pane '5; rm -rf /'
	local dir
	for dir in "$CLAUDE_PANES_DIR" "$CLAUDE_SCREEN_DIR" "$CLAUDE_INTERRUPT_DIR" \
		"$CLAUDE_TASKS_DIR" "$CLAUDE_ISSUES_DIR" "$CLAUDE_WATCHERS_DIR"; do
		[ -e "$dir/5" ]
	done
}

@test "reap_pane guard: has-session failure keeps files" {
	install_fake_tmux 1
	printf 'state=idle\nsession=alpha\n' >"$CLAUDE_PANES_DIR/5"
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	[ -e "$CLAUDE_PANES_DIR/5" ]
	[ -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "reap_pane guard: has-session success deletes files" {
	install_fake_tmux 0
	printf 'state=idle\nsession=alpha\n' >"$CLAUDE_PANES_DIR/5"
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	claude_reap_pane 5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
}

@test "reap_pane screen-only pane deletes unguarded" {
	install_fake_tmux 1
	printf 'x' >"$CLAUDE_SCREEN_DIR/5"
	printf 'x' >"$CLAUDE_TASKS_DIR/5"
	claude_reap_pane 5
	[ ! -e "$CLAUDE_SCREEN_DIR/5" ]
	[ ! -e "$CLAUDE_TASKS_DIR/5" ]
}

@test "reap_pane accepts bare 5 and %5 equally" {
	install_fake_tmux 0
	seed_reap_files 5
	claude_reap_pane 5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
	seed_reap_files 5
	claude_reap_pane %5
	[ ! -e "$CLAUDE_PANES_DIR/5" ]
}
