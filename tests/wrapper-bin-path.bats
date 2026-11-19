#!/usr/bin/env bats
# shellcheck disable=SC2016 # the probe script is written with a literal $PATH
# Behavioural coverage for the wrapper-bin PATH refresh: the real generated conf
# sourced into a real server whose global PATH names an OLD wrapper-bin, asserted
# on the PATH a pane created inside the server actually gets.
#
# Why not a conf grep: the substitution is a nested tmux format, and only a
# server can say what it expands to (a colon inside a pattern silently
# breaks it).

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	RAW_TMUX="${RAW_TMUX:?set RAW_TMUX to the unwrapped tmux}"
	NEW_BIN="${NEW_BIN:?set NEW_BIN to the wrapper bin dir of the conf}"
	CONF="${CONF:?set CONF to the generated conf}"
	SOCK="lz1207-${BATS_TEST_NUMBER}-$$"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export XDG_DATA_HOME="$TEST_HOME/.local/share"
	export TERM=xterm-256color
	SHELL="$(command -v sh)"
	export SHELL
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	unset TMUX
}

teardown() {
	timeout 10s "$RAW_TMUX" -L "$SOCK" kill-server 2>/dev/null || true
	return 0
}

raw() { timeout --foreground 30s "$RAW_TMUX" -L "$SOCK" "$@"; }

# The PATH of a pane spawned the way a key bind spawns one: by a command an
# attached client issues.
pane_path() {
	local out="$BATS_TEST_TMPDIR/pane-path" probe="$BATS_TEST_TMPDIR/probe.sh"
	rm -f "$out"
	printf '#!/bin/sh\nprintf %%s "$PATH" >%s\n' "$out" >"$probe"
	chmod +x "$probe"
	# A control client attached to the server: commands it issues spawn with the
	# server's global PATH, where a bare command client would contribute its own.
	{
		printf 'new-window -d %s\n' "$probe"
		sleep 2
	} | raw -C attach >/dev/null
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)) && [[ ! -s $out ]]; do sleep 0.1; done
	cat "$out"
}

wrapper_segments() { # PATH -> one wrapper-bin segment per line
	tr ':' '\n' <<<"$1" | grep -- '-tmux-wrapper-bin/bin$' || true
}

@test "a reload swaps the old wrapper bin for the current one in new panes" {
	local old=/nix/store/00000000000000000000000000000000-tmux-wrapper-bin/bin
	PATH="$old:/usr/bin:/bin:$PATH" raw -f /dev/null new-session -d -s s
	raw set-option -g default-shell "$SHELL"
	raw source-file "$CONF"
	raw source-file "$CONF" # idempotent

	run pane_path
	[ "$status" -eq 0 ]
	segs="$(wrapper_segments "$output")"
	[ "$segs" = "$NEW_BIN" ]
	[ "${output%%:*}" = "$NEW_BIN" ]
	[[ ":$output:" != *":$old:"* ]]
}

@test "an old wrapper bin in the middle or at the end is removed too" {
	local mid=/nix/store/11111111111111111111111111111111-tmux-wrapper-bin/bin
	local end=/nix/store/22222222222222222222222222222222-tmux-wrapper-bin/bin
	PATH="/usr/bin:$mid:/bin:$PATH:$end" raw -f /dev/null new-session -d -s s
	raw set-option -g default-shell "$SHELL"
	raw source-file "$CONF"

	run pane_path
	segs="$(wrapper_segments "$output")"
	[ "$segs" = "$NEW_BIN" ]
	[[ $output != *: ]]
}

@test "a PATH holding only an old wrapper bin is swapped" {
	local old=/nix/store/33333333333333333333333333333333-tmux-wrapper-bin/bin
	PATH="$old" "$RAW_TMUX" -L "$SOCK" -f /dev/null new-session -d -s s "$(command -v sleep) 60"
	raw set-option -g default-shell "$SHELL"
	raw source-file "$CONF"

	run raw show-environment -g PATH
	[ "$status" -eq 0 ]
	[ "$output" = "PATH=$NEW_BIN" ]
}

@test "a fresh start through the wrapper has exactly one wrapper bin, first" {
	"$TMUX_BIN" -L "$SOCK" new-session -d -s s
	raw set-option -g default-shell "$SHELL"

	run raw show-environment -g PATH
	[ "$status" -eq 0 ]
	p="${output#PATH=}"
	segs="$(wrapper_segments "$p")"
	[ "$segs" = "$NEW_BIN" ]
	[ "${p%%:*}" = "$NEW_BIN" ]
}
