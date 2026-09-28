#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# Owner-checked, per-user claude-status state dir (#850). A fixed name under
# world-writable /tmp means another account can pre-create it (or symlink it)
# ahead of us; these cases pin the fail-closed trust check in lib-claude.sh
# (claude_status_dir_trusted / claude_status_dir_ensure) and the arm_agent_detect
# gate in tmux-update-icons.sh that depends on it.

load helper

CSU="scripts/claude-status-update.sh"

setup() {
	unset TMUX TMUX_PANE
}

@test "default path matches the shared vector" {
	local vector="${CLAUDE_STATUS_DIR_VECTOR:-$BATS_TEST_DIRNAME/../picker/claudestatus/testdata/default-dir.txt}"
	local want
	want="$(cat "$vector")"
	want="${want//\{uid\}/$(id -u)}"
	# shellcheck disable=SC2016  # single-quoted: expands in the nested bash -c, not here
	run env -u CLAUDE_STATUS_DIR bash -c 'source scripts/lib-claude.sh; printf %s "$CLAUDE_STATUS_DIR"'
	[ "$status" -eq 0 ]
	[ "$output" = "$want" ]
}

@test "claude-status-update against a pre-created 0755 root: exits 0, writes nothing" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	run bash "$CSU" processing --pane %9 --session s
	[ "$status" -eq 0 ]
	[ ! -e "$CLAUDE_STATUS_DIR/panes/9" ]
}

@test "claude-status-update against a root that is a symlink: exits 0, writes nothing into the target" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	local target="$BATS_TEST_TMPDIR/real-target"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$target"
	ln -s "$target" "$CLAUDE_STATUS_DIR"
	run bash "$CSU" processing --pane %9 --session s
	[ "$status" -eq 0 ]
	[ ! -e "$target/panes/9" ]
}

@test "claude-status-update against a missing root: creates it 0700 and writes the pane file" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	run bash "$CSU" processing --pane %9 --session s
	[ "$status" -eq 0 ]
	[ "$(stat -c %a "$CLAUDE_STATUS_DIR")" = "700" ]
	[ -e "$CLAUDE_STATUS_DIR/panes/9" ]
}

@test "lib readers refuse a 0755 root even with a planted pane file" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	printf 'state=processing\ntimestamp=%s\n' "$(date +%s)" >"$CLAUDE_STATUS_DIR/panes/9"
	setup_lib_claude
	[ -z "$CLAUDE_STATUS_TRUSTED" ]
	[[ $CLAUDE_PANES_DIR != "$CLAUDE_STATUS_DIR"/* ]]
	run read_pane_state "$CLAUDE_PANES_DIR/9"
	[ "$status" -ne 0 ]
}

@test "steady state is fork-free: a marked 0700 root trusts with PATH emptied" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	: >"$CLAUDE_STATUS_DIR/.owner-only"
	local empty_path="$BATS_TEST_TMPDIR/empty-bin"
	mkdir -p "$empty_path"
	PATH="$empty_path" setup_lib_claude
	[ "$CLAUDE_STATUS_TRUSTED" = 1 ]
}

@test "a 0777 own root with a symlinked .owner-only marker is refused" {
	# The marker check is -f/-O, not -e: a marker that is a symlink (even to a
	# file we own) fails -L and falls through to the stat check, which refuses
	# the loose mode. We cannot chown a marker to a foreign uid without root,
	# so this exercises the reachable half of the fix; the foreign-owner case
	# (a marker dropped by another account) is the -O branch, same fallthrough.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	mkdir -m 777 "$CLAUDE_STATUS_DIR"
	local real="$BATS_TEST_TMPDIR/owned-file"
	: >"$real"
	ln -s "$real" "$CLAUDE_STATUS_DIR/.owner-only"
	setup_lib_claude
	[ -z "$CLAUDE_STATUS_TRUSTED" ]
}

@test "marker is written on first trusted source" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	[ ! -e "$CLAUDE_STATUS_DIR/.owner-only" ]
	setup_lib_claude
	[ "$CLAUDE_STATUS_TRUSTED" = 1 ]
	[ -e "$CLAUDE_STATUS_DIR/.owner-only" ]
}

@test "prune on an untrusted root writes no .server_start" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	setup_lib_claude
	claude_prune_stale_state 1000
	[ ! -e "$CLAUDE_STATUS_DIR/.server_start" ]
}

# arm_agent_detect's gate: a missing root still arms (agent-detect creates it
# owner-only), a present-but-untrusted root does not (agent-detect would exit
# before registering, so arming would respawn a dead pipe-pane every tick).
# Fakes match agent-liveness.bats' setup_sweep: a stub tmux answering
# list-panes with one unpiped agent pane, pipe-pane logged to a file.
setup_arming_fakes() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		case "\$*" in
		*"list-panes"*) printf '%%3|codex|0\n' ;;
		*"pipe-pane"*) echo "\$@" >>"$BATS_TEST_TMPDIR/pipe.log" ;;
		esac
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
	export AGENT_DETECT_BIN="agent-detect"
	export AGENT_COMMANDS="claude codex"
	# Multiple of 5 so the every-5th-tick throttle lets the sweep run.
	export CLAUDE_NOW=100
	: >"$BATS_TEST_TMPDIR/pipe.log"
}

@test "arming gate: a missing root still arms" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	setup_arming_fakes
	# Real lib-claude first, so CLAUDE_STATUS_TRUSTED reflects the genuine
	# check; tmux-update-icons.sh's own `source @lib_claude@` is unsubstituted
	# here and fails silently (no set -e), same as agent-liveness.bats.
	run bash -c 'source scripts/lib-claude.sh; source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ -s "$BATS_TEST_TMPDIR/pipe.log" ]
}

@test "arming gate: a present 0755 root does not arm" {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	setup_arming_fakes
	run bash -c 'source scripts/lib-claude.sh; source scripts/tmux-update-icons.sh; arm_agent_detect'
	[ "$status" -eq 0 ]
	[ ! -s "$BATS_TEST_TMPDIR/pipe.log" ]
}
