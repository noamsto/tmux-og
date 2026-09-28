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
	export TMUX_SET_LOG="$BATS_TEST_TMPDIR/tmux-set.log"
	unset TMUX TMUX_PANE OPENROUTER_API_KEY

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$1" in
		list-panes) printf '%s\n' "$FAKE_PANES" ;;
		set) printf '%s\n' "$*" >>"$TMUX_SET_LOG" ;;
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

# --- CACHE_DIR resolution (no OG_AGENT_USAGE_DIR override) ---

@test "tick: with no override, resolves under XDG_RUNTIME_DIR and creates it 0700" {
	unset OG_AGENT_USAGE_DIR
	export XDG_RUNTIME_DIR="$BATS_TEST_TMPDIR/xdg"
	mkdir -p "$XDG_RUNTIME_DIR"
	export FAKE_PANES='claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	local dir="$XDG_RUNTIME_DIR/og-agent-usage-$UID"
	[ -d "$dir" ]
	[ "$(stat -c %a "$dir")" = 700 ]
	[ -e "$dir/.last-tick" ]
}

@test "tick: a relative XDG_RUNTIME_DIR is ignored, TMPDIR's trailing slash stripped" {
	unset OG_AGENT_USAGE_DIR
	export XDG_RUNTIME_DIR="rel"
	export TMPDIR="$BATS_TEST_TMPDIR/tmp/"
	mkdir -p "$TMPDIR"
	export FAKE_PANES='claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	local dir="$BATS_TEST_TMPDIR/tmp/og-agent-usage-$UID"
	[ -d "$dir" ]
	[ "$(stat -c %a "$dir")" = 700 ]
}

@test "tick: with no XDG_RUNTIME_DIR at all, TMPDIR's trailing slash is stripped" {
	unset OG_AGENT_USAGE_DIR XDG_RUNTIME_DIR
	export TMPDIR="$BATS_TEST_TMPDIR/tmp/"
	mkdir -p "$TMPDIR"
	export FAKE_PANES='claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	local dir="$BATS_TEST_TMPDIR/tmp/og-agent-usage-$UID"
	[ -d "$dir" ]
}

# --- owner-only gate ---

@test "tick-run: a world/group-readable cache dir is refused, leaves the cache untouched" {
	export FAKE_PANES='claude'
	mkdir -m 755 "$OG_AGENT_USAGE_DIR"
	echo '{}' >"$OG_AGENT_USAGE_DIR/claude.json"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -s "$USAGE_LOG" ]
	[ ! -s "$TMUX_SET_LOG" ]
	[ -e "$OG_AGENT_USAGE_DIR/claude.json" ]
}

@test "tick-run: OG_AGENT_USAGE_DIR as a symlink to a 0700 dir is refused" {
	export FAKE_PANES='claude'
	local real="$BATS_TEST_TMPDIR/real-cache"
	mkdir -m 700 "$real"
	echo '{}' >"$real/claude.json"
	ln -s "$real" "$OG_AGENT_USAGE_DIR"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -s "$USAGE_LOG" ]
	[ -e "$real/claude.json" ]
}

@test "tick: a fresh stamp in a world-readable dir is not trusted" {
	export FAKE_PANES='claude'
	mkdir -m 755 "$OG_AGENT_USAGE_DIR"
	touch "$(last_tick)"
	local before
	before=$(stat -c %Y "$(last_tick)")
	run bash "$AGENT_USAGE_SCRIPT" --tick
	[ "$status" -eq 0 ]
	[ "$(stat -c %Y "$(last_tick)")" = "$before" ]
	[ ! -s "$USAGE_LOG" ]
}

@test "tick-run: a fresh nonexistent override dir is created 0700 and the provider runs" {
	export FAKE_PANES='claude'
	rm -rf "$OG_AGENT_USAGE_DIR"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ -d "$OG_AGENT_USAGE_DIR" ]
	[ "$(stat -c %a "$OG_AGENT_USAGE_DIR")" = 700 ]
	[ "$(cat "$USAGE_LOG")" = "claude" ]
}

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
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
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
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
	# cursor-agent's cache is keyed "cursor", not its pane command.
	for f in claude pi cursor; do echo '{}' >"$OG_AGENT_USAGE_DIR/$f.json"; done
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -e "$OG_AGENT_USAGE_DIR/pi.json" ]
	[ ! -e "$OG_AGENT_USAGE_DIR/cursor.json" ]
	[ -e "$OG_AGENT_USAGE_DIR/claude.json" ]
}

@test "tick-run: a failed pane scan clears no cache" {
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
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

@test "tick-run: publishes the surviving caches onto @og_agent_usage" {
	export FAKE_PANES='claude'
	mkdir -m 700 "$OG_AGENT_USAGE_DIR"
	echo '{"windows":[{"label":"5h","pct":42}]}' >"$OG_AGENT_USAGE_DIR/claude.json"
	# codex isn't open, so tick-run clears this before publish_usage reads
	# the cache dir — the assertion below confirms it's absent either way.
	echo '{"windows":[{"label":"5h","pct":99}]}' >"$OG_AGENT_USAGE_DIR/codex.json"
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$TMUX_SET_LOG")" -eq 1 ]
	local line
	line=$(cat "$TMUX_SET_LOG")
	[[ $line == "set -g @og_agent_usage "* ]]
	run jq -e '.claude.windows[0].pct == 42 and (has("codex")|not)' <<<"${line#set -g @og_agent_usage }"
	[ "$status" -eq 0 ]
}

@test "tick-run: no surviving cache publishes an unset" {
	export FAKE_PANES='claude'
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(cat "$TMUX_SET_LOG")" = "set -gu @og_agent_usage" ]
}

@test "tick-run: no agent open never calls tmux set" {
	export FAKE_PANES='bash'
	run bash "$AGENT_USAGE_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ ! -s "$TMUX_SET_LOG" ]
}
