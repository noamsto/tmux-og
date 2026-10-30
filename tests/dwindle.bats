#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for dwindle auto-tiling (#1026): `prefix + Enter` splits
# by the active pane's shape, and a closed tiled pane's split group is evened
# out on `window-layout-changed` while every other cell keeps its sizes.
#
# Why a running server and not a conf grep: the interesting part is what tmux
# DOES with the hook and the binds. Only case 10 reads the rendered conf, for
# the mirror branch of the `Enter` bind, which needs a live bridge to drive.
#
# Harness rules this file has to get right or it cannot fail:
#   - A key binding fires only for an ATTACHED CLIENT. The client is a second
#     `-L` server whose pane runs a real `tmux attach`; only the keypress cases
#     (3, 5) pay for it.
#   - Hooks run deferred, so "nothing happened" can never be asserted after a
#     bare sleep. Every negative case ends with a positive control: a second
#     window with the mode on whose own close balances. Hooks run in event
#     order, so once the control balanced, the first window's hook has run.
#   - Every pane runs `sleep 600`, so no shell prompt or init noise moves
#     anything and no pane exits on its own.
#   - The cache must be current before a close: tmux-dwindle diffs against the
#     layout the previous event stored, so `settle_cache` waits for the cache's
#     SIG half to equal the live SIG before each kill.

# Retry budget for the poll loops below (0.1s sleeps, so POLL_TRIES * 0.1 is
# the wall-clock bound in seconds).
POLL_TRIES=100

# Client-independent window signature the hook's gate compares against the
# cache (plan "Shared definitions", byte-for-byte).
SIG='#{n:#{P:#{?pane_floating_flag,,x}}}_#{window_zoomed_flag}_#{window_width}x#{window_height}_#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}'

setup() {
	IN="dw1026-in-${BATS_TEST_NUMBER}-$$"
	OUT="dw1026-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# The poller and sweep hooks fire inside this test server, and the sweep
	# reaches functions that delete files under these dirs, whose defaults are
	# the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	SHELL="$(command -v bash)"
	export SHELL

	# The base window keeps the session alive whichever pane a case kills.
	inner new-session -d -s s -x 200 -y 50 'sleep 600'
	# The splash popup would eat a keypress.
	inner set-option -g @splash_shown 1
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# A real attached client for the inner server: its keys come from a second
# server's pane, which is where a keypress can actually reach the key table.
attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	OPANE="$(outer list-panes -F '#{pane_id}' | head -1)"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
	CLIENT="$(inner list-clients -F '#{client_name}' | head -1)"
	PREFIX="$(inner show-options -gv prefix)"
}

press() { # key
	outer send-keys -t "$OPANE" "$PREFIX"
	sleep 0.2
	outer send-keys -t "$OPANE" "$1"
	sleep 0.5
}

# wait_for <cmd...>: poll until the command succeeds.
wait_for() {
	local tries=0
	while ((tries < POLL_TRIES)); do
		"$@" && return 0
		sleep 0.1
		((tries++)) || true
	done
	return 1
}

# mk_columns <n> [mode]: a detached window of n even columns, each running
# `sleep 600`. A given mode is stamped as the window's @dwindle before the first
# split, so every layout event after it already sees it. Prints
# "<window id> <pane ids, left to right>".
mk_columns() {
	local n=$1 win p i
	local -a panes
	win="$(inner new-window -d -P -F '#{window_id}' -t s: 'sleep 600')"
	[ -z "${2:-}" ] || inner set-option -w -t "$win" @dwindle "$2"
	panes=("$(inner list-panes -t "$win" -F '#{pane_id}')")
	for ((i = 1; i < n; i++)); do
		p="$(inner split-window -d -h -t "${panes[-1]}" -P -F '#{pane_id}' 'sleep 600')"
		panes+=("$p")
	done
	inner select-layout -t "$win" even-horizontal
	echo "$win ${panes[*]}"
}

split() { # -h|-v <target pane>
	inner split-window -d "$1" -t "$2" -P -F '#{pane_id}' 'sleep 600'
}

# A tiled pane field, one per line in pane-list order; floats are skipped.
tiled() { # window format
	inner list-panes -t "$1" -F "#{pane_floating_flag}|$2" | awk -F'|' '$1 != 1 { print $2 }'
}

widths() { tiled "$1" '#{pane_width}' | paste -sd' ' -; }

# Exactly two tiled panes whose widths differ by at most 1.
even_widths() {
	local a b
	read -r a b < <(widths "$1")
	[ -n "$b" ] && ((a - b <= 1 && b - a <= 1))
}

# Two widths that differ by more than 1: tmux's own post-kill sizes.
uneven_widths() {
	local a b
	read -r a b < <(widths "$1")
	[ -n "$b" ] && ((a - b > 1 || b - a > 1))
}

pane_h() { inner display-message -p -t "$1" '#{pane_height}'; }
pane_w() { inner display-message -p -t "$1" '#{pane_width}'; }

within1() { ((($1) - ($2) <= 1 && ($2) - ($1) <= 1)); }

cache_count() { inner display-message -p -t "$1" '#{s/_.*//:@dwindle_cache}'; }
cache_sig() { inner display-message -p -t "$1" '#{s/[|].*//:@dwindle_cache}'; }
live_sig() { inner display-message -p -t "$1" "$SIG"; }
cache_full() { inner display-message -p -t "$1" '#{@dwindle_cache}'; }

count_is() { [ "$(cache_count "$1")" = "$2" ]; }

cache_settled() {
	local c
	c="$(cache_sig "$1")"
	[ -n "$c" ] && [ "$c" = "$(live_sig "$1")" ]
}

settle_cache() {
	wait_for cache_settled "$1" || {
		echo "cache never caught up with $1" >&2
		echo "  cache: $(cache_full "$1")" >&2
		echo "  live:  $(live_sig "$1")" >&2
		return 1
	}
}

# The positive control (see the header): a fresh three-column window with the
# mode on, closed in the middle, must balance. Fails if it never does.
prove_hooks_ran() {
	local ids
	read -ra ids <<<"$(mk_columns 3 on)"
	settle_cache "${ids[0]}"
	inner kill-pane -t "${ids[2]}"
	wait_for even_widths "${ids[0]}" || {
		echo "control window never balanced: $(widths "${ids[0]}")" >&2
		return 1
	}
}

@test "1. closing the middle of three equal columns evens the other two" {
	local ids
	read -ra ids <<<"$(mk_columns 3)"
	settle_cache "${ids[0]}"
	inner kill-pane -t "${ids[2]}"
	wait_for even_widths "${ids[0]}" || {
		echo "widths after close: $(widths "${ids[0]}")" >&2
		false
	}
}

@test "2. closing in one split group leaves the other group's sizes alone" {
	local win a b c d e
	local ah bh aw cw sum
	win="$(inner new-window -d -P -F '#{window_id}' -t s: 'sleep 600')"
	a="$(inner list-panes -t "$win" -F '#{pane_id}')"
	c="$(split -h "$a")"
	b="$(split -v "$a")"
	d="$(split -v "$c")"
	e="$(split -v "$d")"
	inner resize-pane -t "$a" -y 10
	# Shrink C and E so D holds most of the column: whoever tmux hands D's rows
	# to ends up far from the other, and an even-out is an observable change.
	inner resize-pane -t "$c" -y 6
	inner resize-pane -t "$e" -y 6
	ah="$(pane_h "$a")"
	bh="$(pane_h "$b")"
	aw="$(pane_w "$a")"
	cw="$(pane_w "$c")"
	sum=$(($(pane_h "$c") + $(pane_h "$d") + $(pane_h "$e") + 1))
	[ "$ah" = 10 ]
	settle_cache "$win"

	inner kill-pane -t "$d"
	wait_for within1_ce "$c" "$e" || {
		echo "C/E heights after close: $(pane_h "$c") $(pane_h "$e")" >&2
		false
	}
	[ "$(pane_h "$a")" = "$ah" ]
	[ "$(pane_h "$b")" = "$bh" ]
	[ "$(pane_w "$a")" = "$aw" ]
	[ "$(pane_w "$c")" = "$cw" ]
	[ "$(($(pane_h "$c") + $(pane_h "$e")))" = "$sum" ]
}

within1_ce() { within1 "$(pane_h "$1")" "$(pane_h "$2")"; }

@test "3. prefix + Enter splits a wide pane side by side and a tall one stacked" {
	attach_client
	local ids win p0 tall w h
	read -ra ids <<<"$(mk_columns 1)"
	win="${ids[0]}"
	p0="${ids[1]}"
	inner select-window -t "$win"
	inner select-pane -t "$p0"
	w="$(pane_w "$p0")"
	h="$(pane_h "$p0")"
	((w > 2 * h))

	press Enter
	wait_for two_panes "$win" || {
		echo "no split after prefix + Enter" >&2
		false
	}
	[ "$(tiled "$win" '#{pane_left}' | sort -u | wc -l)" = 2 ]
	[ "$(tiled "$win" '#{pane_top}' | sort -u | wc -l)" = 1 ]

	# A new 40-column pane is active and narrower than twice its height.
	tall="$(inner split-window -h -l 40 -t "$p0" -P -F '#{pane_id}' 'sleep 600')"
	inner select-pane -t "$tall"
	w="$(pane_w "$tall")"
	h="$(pane_h "$tall")"
	((w < 2 * h))
	press Enter
	wait_for stacked_under "$win" "$tall" || {
		echo "tall pane did not split stacked" >&2
		inner list-panes -t "$win" -F '#{pane_id} #{pane_left},#{pane_top} #{pane_width}x#{pane_height}' >&2
		false
	}
}

two_panes() { [ "$(tiled "$1" '#{pane_id}' | wc -l)" = 2 ]; }

# The pane sits in a column of two or more panes.
stacked_under() { # window pane
	local left
	left="$(inner display-message -p -t "$2" '#{pane_left}')"
	[ "$(tiled "$1" '#{pane_left}' | grep -cx "$left")" -ge 2 ]
}

@test "4. with the mode off globally a close keeps tmux's own sizes" {
	local ids after
	inner set-option -gw @dwindle off
	read -ra ids <<<"$(mk_columns 3)"
	inner kill-pane -t "${ids[2]}"
	after="$(widths "${ids[0]}")"
	uneven_widths "${ids[0]}"

	prove_hooks_ran

	[ "$(widths "${ids[0]}")" = "$after" ]
}

@test "5. prefix + A turns balancing off for one window and back on" {
	attach_client
	local ids win after
	read -ra ids <<<"$(mk_columns 3)"
	win="${ids[0]}"
	inner select-window -t "$win"
	settle_cache "$win"

	press A
	[ "$(inner show-options -wv -t "$win" @dwindle)" = off ]
	inner show-messages -t "$CLIENT" | grep -F 'dwindle auto-balance: off'

	inner kill-pane -t "${ids[2]}"
	after="$(widths "$win")"
	uneven_widths "$win"
	prove_hooks_ran
	[ "$(widths "$win")" = "$after" ]

	inner select-window -t "$win"
	press A
	[ "$(inner show-options -wv -t "$win" @dwindle)" = on ]
}

@test "6. a dispatcher grid window (@crew_grid=1) is never balanced" {
	local ids after
	read -ra ids <<<"$(mk_columns 3)"
	settle_cache "${ids[0]}"
	inner set-option -w -t "${ids[0]}" @crew_grid 1
	inner kill-pane -t "${ids[2]}"
	after="$(widths "${ids[0]}")"
	uneven_widths "${ids[0]}"
	wait_for count_is "${ids[0]}" 2 || {
		echo "cache count never reached 2: $(cache_full "${ids[0]}")" >&2
		false
	}
	[ "$(widths "${ids[0]}")" = "$after" ]
}

@test "7. closing a float neither balances nor rewrites the cache" {
	local win p0 float widths_before cache_before
	win="$(inner new-window -d -P -F '#{window_id}' -t s: 'sleep 600')"
	inner set-option -w -t "$win" @dwindle off
	p0="$(inner list-panes -t "$win" -F '#{pane_id}')"
	split -h "$p0" >/dev/null
	inner resize-pane -t "$p0" -x 66
	inner set-option -w -t "$win" @dwindle on
	# One real layout change with the mode on writes the cache.
	inner resize-pane -t "$p0" -x 67
	settle_cache "$win"
	uneven_widths "$win"
	widths_before="$(widths "$win")"
	cache_before="$(cache_full "$win")"

	float="$(inner new-pane -d -t "$win" -P -F '#{pane_id}' 'sleep 600')"
	[ "$(inner display-message -p -t "$float" '#{pane_floating_flag}')" = 1 ]
	inner kill-pane -t "$float"
	prove_hooks_ran

	[ "$(widths "$win")" = "$widths_before" ]
	[ "$(cache_full "$win")" = "$cache_before" ]
}

@test "8. a close in a zoomed window is not balanced" {
	local ids after
	read -ra ids <<<"$(mk_columns 3)"
	settle_cache "${ids[0]}"
	inner resize-pane -Z -t "${ids[1]}"
	# The cached zoom field has to record the zoom before the close unzooms.
	settle_cache "${ids[0]}"
	[ "$(inner display-message -p -t "${ids[0]}" '#{window_zoomed_flag}')" = 1 ]
	inner kill-pane -t "${ids[2]}"
	after="$(widths "${ids[0]}")"
	uneven_widths "${ids[0]}"
	wait_for count_is "${ids[0]}" 2 || {
		echo "cache count never reached 2: $(cache_full "${ids[0]}")" >&2
		false
	}
	[ "$(widths "${ids[0]}")" = "$after" ]
}

probe_lines() { wc -l <"$PROBE" 2>/dev/null || echo 0; }

probe_grew() { (($(probe_lines) > PROBE_BEFORE)); }

@test "9. a layout event that changes nothing writes no cache" {
	local ids win p0
	read -ra ids <<<"$(mk_columns 3)"
	win="${ids[0]}"
	p0="${ids[1]}"
	settle_cache "$win"

	# A probe hook after the gate (index 50 > 20) proves the no-op event really
	# fired; hooks of one event run in index order, so by the time the probe
	# line lands the gate has already decided.
	PROBE="$BATS_TEST_TMPDIR/probe"
	: >"$PROBE"
	inner set-hook -g 'window-layout-changed[50]' "run-shell 'echo x >>$PROBE'"
	inner set-option -w -t "$win" @dwindle_cache "$(live_sig "$win")|SENTINEL"

	PROBE_BEFORE="$(probe_lines)"
	inner select-layout -E -t "$p0"
	if ! POLL_TRIES=20 wait_for probe_grew; then
		PROBE_BEFORE="$(probe_lines)"
		inner resize-pane -t "$p0" -x "$(pane_w "$p0")"
		wait_for probe_grew
	fi

	[[ "$(cache_full "$win")" == *'|SENTINEL' ]]
}

@test "10. the Enter bind routes a mirror through the bridge ctl and a local pane through split-window" {
	[ -n "${TMUX_OG_CONF:-}" ] || skip "TMUX_OG_CONF not set"
	local line
	line="$(grep -E "^bind -N 'Split pane by its shape' Enter " "$TMUX_OG_CONF" || true)"
	[ -n "$line" ]
	[[ $line == *'split-h,split-v} #{q:@bridge_pane}'* ]]
	[[ $line == *'split-window -h'* ]]
	[[ $line == *'split-window -v'* ]]
}
