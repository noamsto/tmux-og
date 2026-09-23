#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# The "is an agent running" gate of the usage poller. What's worth pinning is
# WHEN the gate runs relative to the .last-tick stamp: a tick with no agent
# pane must leave the stamp alone, or the first tick after an agent starts is
# refused for another whole refresh window and the segment reappears showing
# the previous session's numbers. Tick-run then gates each provider on its OWN
# agent being open, and clears the cache of every agent that isn't — but only
# after a successful scan, so a failed list-panes never reads as "all closed".
#
# Fakes: tmux answers list-panes from $FAKE_PANES; the four providers are
# stubs that log their name (see make_agent_usage).

load helper

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	export USAGE_LOG="$BATS_TEST_TMPDIR/usage.log"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/cache"
	unset TMUX TMUX_PANE OPENROUTER_API_KEY

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$1" in
		list-panes) printf '%s\n' "$FAKE_PANES" ;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"

	export PATH="$FAKEBIN:$PATH"
	export HOME="$BATS_TEST_TMPDIR"
	# One authed CLI, so a pass that reaches the providers logs exactly one line.
	mkdir -p "$HOME/.claude"
	echo '{}' >"$HOME/.claude/.credentials.json"

	make_agent_usage
}

last_tick() { echo "$OG_AGENT_USAGE_DIR/.last-tick"; }

@test "tick: no agent pane leaves .last-tick unstamped" {
	export FAKE_PANES='bash
fish'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	[ ! -e "$(last_tick)" ]
}

@test "tick: an agent pane stamps .last-tick" {
	export FAKE_PANES='bash
claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	[ -e "$(last_tick)" ]
}

@test "tick: a nix-wrapped agent basename still counts" {
	export FAKE_PANES='/nix/store/abc-cursor-agent/bin/.cursor-agent-wrapped'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	[ -e "$(last_tick)" ]
}

@test "tick: a fresh stamp short-circuits before the gate forks tmux" {
	export FAKE_PANES='claude'
	mkdir -p "$OG_AGENT_USAGE_DIR"
	touch "$(last_tick)"
	# A tmux that fails the test if called at all: inside the refresh window the
	# tick must return on the mtime check alone.
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		printf 'called\n' >>"$USAGE_LOG"
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	[ ! -s "$USAGE_LOG" ]
}

@test "tick-run: no agent pane runs no provider" {
	export FAKE_PANES='bash'
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -s "$USAGE_LOG" ]
}

@test "tick-run: an agent pane runs the authed provider" {
	export FAKE_PANES='claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(cat "$USAGE_LOG")" = "claude" ]
}

@test "tick-run: an unreachable tmux is treated as no agent" {
	export FAKE_PANES='claude'
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		exit 1
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -s "$USAGE_LOG" ]
}

@test "tick-run: an authed agent with no pane open is not polled" {
	export FAKE_PANES='claude'
	mkdir -p "$HOME/.config/cursor"
	echo '{}' >"$HOME/.config/cursor/auth.json"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(cat "$USAGE_LOG")" = "claude" ]
}

@test "tick-run: an open pi with an auth.json runs the pi provider" {
	export FAKE_PANES='pi'
	mkdir -p "$HOME/.pi/agent"
	echo '{}' >"$HOME/.pi/agent/auth.json"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(cat "$USAGE_LOG")" = "pi" ]
}

@test "tick-run: a closed agent's cache is cleared, an open one's kept" {
	export FAKE_PANES='claude'
	mkdir -p "$OG_AGENT_USAGE_DIR"
	# cursor-agent's cache is keyed "cursor", not its pane command.
	for f in claude pi cursor; do echo '{}' >"$OG_AGENT_USAGE_DIR/$f.json"; done
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -e "$OG_AGENT_USAGE_DIR/pi.json" ]
	[ ! -e "$OG_AGENT_USAGE_DIR/cursor.json" ]
	[ -e "$OG_AGENT_USAGE_DIR/claude.json" ]
}

@test "tick-run: a failed pane scan clears no cache" {
	mkdir -p "$OG_AGENT_USAGE_DIR"
	echo '{}' >"$OG_AGENT_USAGE_DIR/claude.json"
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		exit 1
	EOF
	chmod +x "$FAKEBIN/tmux"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ -e "$OG_AGENT_USAGE_DIR/claude.json" ]
}
