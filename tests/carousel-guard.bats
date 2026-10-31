#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
# Tests the carousel width guard (#1056): the window-layout-changed[30] and
# window-resized[30] hooks that fork scripts/tmux-carousel-guard.sh when a
# tiled image-carousel pane (@claude_img_src) is squeezed under 20 columns, and
# the script that grows it back to 40% of the space it splits with its host.
# tmux never grows a squeezed pane on its own, so without the guard every case
# that expects a restore stays at 1 column.
#
# Why a running server and not a conf grep: the interesting part is what tmux
# does with the gate (it must fork exactly when the guard can help, and never
# twice at one geometry) and what the resize does to the layout. The hook lines
# are the production ones — from the rendered conf when TMUX_OG_CONF is set
# (the carousel-guard-tests check), else from config/tmux.conf.tmpl — with the
# guard path swapped for a recorder that logs each fork and then execs the
# repo script, so "how many times did the gate fork" is a line count.
#
# Harness rules this file has to get right or it cannot fail:
#   - Every non-trivial geometry is built with the hooks unarmed, then the
#     hooks are armed and one event is fired: an intermediate layout of the
#     build can trip the gate and restore the carousel before the case starts.
#   - `fire_same` re-applies the window's own layout: it fires
#     window-layout-changed at an unchanged geometry, which a same-size
#     resize-window does not reliably do.
#   - "Zero forks" is a fixed settle followed by an empty fork log and an
#     unset @carousel_guard_sig (the gate writes it synchronously before it
#     forks); where cheap, a positive control afterwards proves the hooks were
#     armed and the negative was not vacuous.
#   - Every pane runs `sleep 600`, so no shell prompt moves anything.

# Retry budget for the "poll until <condition>" loops below (0.1s sleeps, so
# POLL_TRIES * 0.1 is the wall-clock bound in seconds): a backgrounded guard
# job has to fork, exec bash, and take its turn on tmux's server thread, which
# CI's CPU contention can stretch well past a quiet dev box (#827).
POLL_TRIES=300
# Wall-clock wait before asserting that nothing was forked.
SETTLE=1

# Every test call names its own socket, so a stray TMUX can never reach the
# user's server. The guard's own bare tmux calls run inside run-shell jobs,
# which inherit TMUX pointing at this server.
tmux() { command tmux -L og-cg "$@"; }

setup() {
	unset TMUX
	type -P tmux >/dev/null || skip "tmux not on PATH"

	REPO_ROOT="$(cd "$(dirname "$BATS_TEST_FILENAME")/.." && pwd)"
	GUARD="$REPO_ROOT/scripts/tmux-carousel-guard.sh"
	TEMPLATE="$REPO_ROOT/config/tmux.conf.tmpl"

	OG_TMUX_DIR="/tmp/og-cg-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"

	# A run-shell -b job inherits the server's environment, not this test's,
	# so the recorder bakes in absolute paths.
	FORKS="$BATS_TEST_TMPDIR/forks.log"
	: >"$FORKS"
	RECORDER="$BATS_TEST_TMPDIR/carousel-guard-recorder"
	cat >"$RECORDER" <<EOF
#!$(command -v bash)
printf '%s\n' "\$*" >>'$FORKS'
exec '$(command -v bash)' '$GUARD' "\$@"
EOF
	chmod +x "$RECORDER"

	tmux -f /dev/null new-session -d -s S -c "$TMUX_TMPDIR" -x 154 -y 40 'sleep 600' 3>&-
	# A wrapped tmux on PATH loads its baked-in conf alongside -f /dev/null,
	# arming the refit hooks (and, once installed, this very guard against its
	# store path). Start from no layout hooks, as the flake check's bare tmux does.
	tmux set-hook -gu window-layout-changed
	tmux set-hook -gu window-resized
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	MAIN="$(tmux display-message -p -t S '#{pane_id}')"
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

# guard_hook_lines — the two production [30] setters, guard path replaced by
# the recorder.
guard_hook_lines() {
	if [[ -n ${TMUX_OG_CONF:-} ]]; then
		grep -E '^[[:space:]]*set-hook .*tmux-carousel-guard' "$TMUX_OG_CONF" |
			sed -E "s|/nix/store/[^ \"]*/bin/tmux-carousel-guard|$RECORDER|g"
		return
	fi
	local placeholder='{{index .Paths.Scripts "tmux-carousel-guard"}}' line
	while IFS= read -r line; do
		printf '%s\n' "${line//"$placeholder"/$RECORDER}"
	done < <(grep -E '^[[:space:]]*set-hook .*tmux-carousel-guard' "$TEMPLATE")
}

arm_guard_hooks() {
	local hooks="$BATS_TEST_TMPDIR/guard-hooks.conf"
	guard_hook_lines >"$hooks"
	# Both setters, each forking the recorder: a hook still naming the real
	# guard would fork unlogged and turn every zero-fork assertion vacuous.
	[ "$(grep -c . "$hooks")" -eq 2 ]
	[ "$(grep -cF "$RECORDER #{q:window_id}" "$hooks")" -eq 2 ]
	if grep -q 'tmux-carousel-guard' "$hooks"; then return 1; fi
	tmux source-file "$hooks"
}

# make_carousel <pane> <host>: stamp <pane> the way aeye's launcher does.
make_carousel() {
	tmux set-option -p -t "$1" @claude_img_src "x-${2#%}"
}

split_h() { tmux split-window -h -t "$1" -P -F '#{pane_id}' 'sleep 600'; }
split_v() { tmux split-window -v -t "$1" -P -F '#{pane_id}' 'sleep 600'; }

width_of() { tmux display-message -p -t "$1" '#{pane_width}'; }

fork_count() { grep -c . "$FORKS" || true; }

marker() { tmux show-options -w -v -t "$WIN" @carousel_guard_sig 2>/dev/null || true; }

layout() { tmux display-message -p -t "$WIN" '#{window_layout}'; }

fire_same() { tmux select-layout -t "$WIN" "$(layout)"; }

# poll_width <pane> <want>: wait until <pane> is exactly <want> columns.
poll_width() {
	local tries=0
	while ((tries < POLL_TRIES)); do
		[[ "$(width_of "$1")" == "$2" ]] && return 0
		sleep 0.1
		((tries++)) || true
	done
	echo "$1 is $(width_of "$1") columns, want $2" >&3
	return 1
}

# poll_forks <n>: wait until the recorder logged <n> forks.
poll_forks() {
	local tries=0
	while ((tries < POLL_TRIES)); do
		[[ "$(fork_count)" == "$1" ]] && return 0
		sleep 0.1
		((tries++)) || true
	done
	echo "forks: $(fork_count), want $1" >&3
	return 1
}

assert_no_fork() {
	sleep "$SETTLE"
	[ "$(fork_count)" -eq 0 ]
	[ -z "$(marker)" ]
}

# squeezed_pair: MAIN + a side carousel split off it, squeezed to 1 column by
# a full-width resize of MAIN (unarmed). Leaves the carousel in CAR.
squeezed_pair() {
	CAR="$(split_h "$MAIN")"
	make_carousel "$CAR" "$MAIN"
	tmux resize-pane -t "$MAIN" -x 152
	[ "$(width_of "$CAR")" -eq 1 ]
}

# arm_counter: a later-index window-layout-changed hook counting events in
# @lc, to tell the test's own layout changes from any the guard causes.
arm_counter() {
	tmux set-option -g @lc 0
	tmux set-hook -g 'window-layout-changed[99]' 'set -gF @lc "#{e|+:#{@lc},1}"'
}

layout_changes() { tmux show-options -g -v @lc; }

@test "a squeezed side carousel is restored to 40% of the pair" {
	local car
	car="$(split_h "$MAIN")"
	make_carousel "$car" "$MAIN"
	arm_guard_hooks

	tmux resize-pane -t "$MAIN" -x 152

	# 154 columns, 152/1: S = 154, target 61, the agent pane keeps 92.
	poll_width "$car" 61
	[ "$(width_of "$MAIN")" -eq 92 ]
	# A successful restore clears the marker in the resize's own command list.
	[ -z "$(marker)" ]
}

@test "an identical re-squeeze is restored again" {
	local car
	car="$(split_h "$MAIN")"
	make_carousel "$car" "$MAIN"
	arm_guard_hooks
	tmux resize-pane -t "$MAIN" -x 152
	poll_width "$car" 61
	poll_forks 1

	# Same pane ids, same 152/1 at 154: the SIG the gate wrote before the
	# first fork. A marker surviving the restore would suppress this one.
	tmux resize-pane -t "$MAIN" -x 152
	poll_forks 2
	poll_width "$car" 61
}

@test "a wide carousel through resizes forks nothing" {
	local car
	car="$(split_h "$MAIN")"
	make_carousel "$car" "$MAIN"
	[ "$(width_of "$car")" -eq 76 ]
	arm_guard_hooks

	tmux resize-window -t "$WIN" -x 120
	[ "$(width_of "$car")" -ge 20 ]
	tmux resize-window -t "$WIN" -x 154
	assert_no_fork
	[ "$(width_of "$car")" -eq 76 ]
	[ "$(width_of "$MAIN")" -eq 77 ]
}

@test "a zoomed window forks nothing until it is unzoomed" {
	squeezed_pair
	tmux resize-pane -Z -t "$MAIN"
	arm_guard_hooks

	tmux resize-window -t "$WIN" -x 150
	assert_no_fork
	[ "$(width_of "$CAR")" -eq 1 ]

	# Unzoom refires window-layout-changed: the guard re-evaluates then.
	tmux resize-pane -Z -t "$MAIN"
	poll_width "$CAR" 60
}

@test "a dispatcher grid window forks nothing" {
	squeezed_pair
	tmux set-option -w -t "$WIN" @crew_grid 1
	arm_guard_hooks

	fire_same
	assert_no_fork
	[ "$(width_of "$CAR")" -eq 1 ]

	tmux set-option -w -u -t "$WIN" @crew_grid
	fire_same
	poll_width "$CAR" 61
}

@test "a mirror window forks nothing" {
	squeezed_pair
	tmux set-option -w -t "$WIN" @bridge_win 1
	arm_guard_hooks

	fire_same
	assert_no_fork
	[ "$(width_of "$CAR")" -eq 1 ]

	tmux set-option -w -u -t "$WIN" @bridge_win
	fire_same
	poll_width "$CAR" 61
}

@test "a floating carousel forks nothing" {
	# Stock tmux advertises new-pane and rejects it at parse time, so probe by
	# attempting it, then confirm tmux reports the pane as floating.
	FLOAT="$(tmux new-pane -t "$WIN" -x 10 -y 5 -X 3 -Y 3 -P -F '#{pane_id}' 'sleep 600' 2>/dev/null)" ||
		skip "this tmux cannot create a floating pane"
	[ "$(tmux display-message -p -t "$FLOAT" '#{pane_floating_flag}')" = 1 ] ||
		skip "this tmux does not report pane_floating_flag"
	make_carousel "$FLOAT" "$MAIN"
	local fw
	fw="$(width_of "$FLOAT")"
	[ "$fw" -lt 20 ]
	arm_guard_hooks

	tmux resize-window -t "$WIN" -x 150
	fire_same
	assert_no_fork
	[ "$(width_of "$FLOAT")" -eq "$fw" ]

	# Positive control: a tiled carousel in the same window is still guarded.
	local car
	car="$(split_h "$MAIN")"
	make_carousel "$car" "$MAIN"
	tmux resize-pane -t "$MAIN" -x 148
	poll_width "$car" 60
	[ "$(width_of "$FLOAT")" -eq "$fw" ]
}

@test "a window narrower than a usable pair forks nothing" {
	squeezed_pair
	tmux resize-window -t "$WIN" -x 50
	[ "$(width_of "$CAR")" -eq 1 ]
	arm_guard_hooks

	fire_same
	tmux resize-window -t "$WIN" -x 45
	assert_no_fork
	[ "$(width_of "$CAR")" -eq 1 ]

	# Positive control: squeezed again at full width, it is restored.
	tmux resize-window -t "$WIN" -x 154
	tmux resize-pane -t "$MAIN" -x 152
	poll_width "$CAR" 61
}

@test "a too-narrow pair in a wide window forks once and then stays quiet" {
	# A | H | C, C split off H: H + 1 + C = 32 < 61, so the script declines.
	local host car
	host="$(split_h "$MAIN")"
	car="$(split_h "$host")"
	make_carousel "$car" "$host"
	tmux resize-pane -t "$car" -x 1
	tmux resize-pane -t "$MAIN" -x 121
	[ "$(width_of "$host")" -eq 30 ]
	[ "$(width_of "$car")" -eq 1 ]
	local before
	before="$(layout)"
	arm_guard_hooks

	fire_same
	poll_forks 1
	sleep "$SETTLE"
	[ "$(layout)" = "$before" ]
	# The gate's marker stays: "tried and could not fix".
	[ -n "$(marker)" ]

	# A no-op resize of A and another trigger, both at the same geometry.
	tmux resize-pane -t "$MAIN" -x 121
	fire_same
	sleep "$SETTLE"
	[ "$(fork_count)" -eq 1 ]
	[ "$(layout)" = "$before" ]
}

@test "a middle carousel takes its columns from the host, not the pane to its right" {
	# H | C | X, C split off H. Resizing C itself would move its right border
	# and crush X; the guard resizes H instead.
	local car right
	car="$(split_h "$MAIN")"
	right="$(split_h "$car")"
	make_carousel "$car" "$MAIN"
	tmux resize-pane -t "$car" -x 1
	tmux resize-pane -t "$MAIN" -x 90
	[ "$(width_of "$car")" -eq 1 ]
	[ "$(width_of "$right")" -eq 61 ]
	arm_guard_hooks

	fire_same

	# S = 90 + 1 + 1 = 92, target 36.
	poll_width "$car" 36
	[ "$(width_of "$MAIN")" -eq 55 ]
	[ "$(width_of "$right")" -eq 61 ]
}

@test "two squeezed carousels are both restored without crushing a host" {
	# H1 | C1 | X | C2, C1 split off H1 and C2 off X, both at 1 column.
	local c1 x c2
	c1="$(split_h "$MAIN")"
	x="$(split_h "$c1")"
	c2="$(split_h "$x")"
	make_carousel "$c1" "$MAIN"
	make_carousel "$c2" "$x"
	tmux resize-pane -t "$MAIN" -x 74
	tmux resize-pane -t "$c1" -x 1
	tmux resize-pane -t "$c2" -x 1
	[ "$(width_of "$c1")" -eq 1 ]
	[ "$(width_of "$c2")" -eq 1 ]
	[ "$(width_of "$x")" -eq 75 ]
	arm_guard_hooks

	fire_same

	# One carousel per run: the first restore's own layout change forks the
	# run that restores the second.
	poll_width "$c1" 30
	poll_width "$c2" 30
	sleep "$SETTLE"
	[ "$(width_of "$MAIN")" -eq 45 ]
	[ "$(width_of "$x")" -eq 46 ]
	local p
	for p in "$MAIN" "$c1" "$x" "$c2"; do
		[ "$(width_of "$p")" -ge 20 ]
	done
	[ "$(fork_count)" -le 3 ]
}

@test "a bottom carousel in a narrow column grows the column" {
	local host car
	host="$(split_h "$MAIN")"
	tmux resize-pane -t "$host" -x 15
	car="$(split_v "$host")"
	make_carousel "$car" "$host"
	[ "$(width_of "$car")" -eq 15 ]
	arm_guard_hooks

	fire_same

	# The host is above, not beside it: S = window width 154, target 61.
	poll_width "$car" 61
	[ "$(width_of "$host")" -eq 61 ]
	[ "$(width_of "$MAIN")" -eq 92 ]
}

@test "a full-width bottom carousel forks nothing" {
	local car
	car="$(split_v "$MAIN")"
	make_carousel "$car" "$MAIN"
	arm_guard_hooks

	fire_same
	tmux resize-window -t "$WIN" -x 140
	assert_no_fork
	[ "$(width_of "$car")" -eq 140 ]
}

@test "a restore does not ping-pong" {
	local car
	car="$(split_h "$MAIN")"
	make_carousel "$car" "$MAIN"
	arm_guard_hooks
	tmux resize-pane -t "$MAIN" -x 152
	poll_width "$car" 61
	sleep "$SETTLE"
	arm_counter

	# Idle: nothing re-forks and nothing moves.
	sleep "$SETTLE"
	[ "$(layout_changes)" -eq 0 ]
	[ "$(fork_count)" -eq 1 ]

	tmux resize-window -t "$WIN" -x 140
	[ "$(width_of "$car")" -ge 20 ]
	tmux resize-window -t "$WIN" -x 154
	[ "$(width_of "$car")" -ge 20 ]
	local after_resizes
	after_resizes="$(layout_changes)"
	[ "$after_resizes" -ge 2 ]

	# Every layout change from here on would be the guard reacting to itself.
	sleep "$SETTLE"
	[ "$(layout_changes)" -eq "$after_resizes" ]
	[ "$(fork_count)" -eq 1 ]
	[ -z "$(marker)" ]
}

@test "a pane close racing dwindle's balance settles" {
	[[ -n ${TMUX_OG_CONF:-} ]] || skip "TMUX_OG_CONF unset: the dwindle hook names a store binary"
	local hooks="$BATS_TEST_TMPDIR/dwindle-hooks.conf"
	grep -E '^[[:space:]]*set-hook -g window-layout-changed\[20\] .*tmux-dwindle' "$TMUX_OG_CONF" >"$hooks"
	[ "$(grep -c . "$hooks")" -eq 1 ]

	# A | H | C with C squeezed; the pair H + C is wide enough to restore.
	local host car
	host="$(split_h "$MAIN")"
	car="$(split_h "$host")"
	make_carousel "$car" "$host"
	tmux resize-pane -t "$car" -x 1
	tmux resize-pane -t "$MAIN" -x 60
	[ "$(width_of "$car")" -eq 1 ]

	# Dwindle diffs against the layout its previous event cached, so the cache
	# must hold the three-pane layout before the close; the guard is armed
	# after that so it does not restore C first.
	tmux set-option -w -t "$WIN" @dwindle on
	tmux source-file "$hooks"
	fire_same
	local tries=0
	while ((tries < POLL_TRIES)); do
		[[ -n "$(tmux show-options -w -v -t "$WIN" @dwindle_cache 2>/dev/null)" ]] && break
		sleep 0.1
		((tries++)) || true
	done
	[ -n "$(tmux show-options -w -v -t "$WIN" @dwindle_cache)" ]
	arm_guard_hooks
	arm_counter

	tmux kill-pane -t "$MAIN"

	tries=0
	while ((tries < POLL_TRIES)); do
		(("$(width_of "$car")" >= 20)) && break
		sleep 0.1
		((tries++)) || true
	done
	[ "$(width_of "$car")" -ge 20 ]

	# Whichever of dwindle's balance and the guard's resize lands last, the
	# window then goes quiet.
	sleep "$SETTLE"
	local settled
	settled="$(layout_changes)"
	sleep "$SETTLE"
	[ "$(layout_changes)" -eq "$settled" ]
	[ "$(width_of "$car")" -ge 20 ]
	[ "$(fork_count)" -le 2 ]
}

@test "both hook bodies are identical and the gate literals match the script" {
	local lines
	lines="$(grep -E '^set-hook -g (window-layout-changed|window-resized)\[30\] ' "$TEMPLATE")"
	[ "$(grep -c . <<<"$lines")" -eq 2 ]
	local lc rs
	lc="$(grep 'window-layout-changed\[30\]' <<<"$lines" | sed -E 's/^set-hook -g [a-z-]+\[30\] +//')"
	rs="$(grep 'window-resized\[30\]' <<<"$lines" | sed -E 's/^set-hook -g [a-z-]+\[30\] +//')"
	[ -n "$lc" ]
	[ "$lc" = "$rs" ]

	local min_cols main_min_cols
	min_cols="$(sed -n 's/^MIN_COLS=//p' "$GUARD")"
	main_min_cols="$(sed -n 's/^MAIN_MIN_COLS=//p' "$GUARD")"
	[ -n "$min_cols" ]
	[ -n "$main_min_cols" ]
	[[ $lc == *"#{e|<|:#{pane_width},$min_cols}"* ]]
	[[ $lc == *"#{e|>=|:#{window_width},$((min_cols + 1 + main_min_cols))}"* ]]
}
