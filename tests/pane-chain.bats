#!/usr/bin/env bats
# Tests scripts/tmux-pane-chain.sh (#1222): a pane marked @pane_parent <id> closes
# with that parent. Runs the real script against a private, config-less tmux server
# (like tests/float-group.bats); needs the pinned next-3.9 tmux (mkTmux) for floats.

bats_require_minimum_version 1.5.0

setup() {
	OG_TMUX_DIR="/tmp/og-pc-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX TMUX_PANE
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"
	REAL_TMUX="$(command -v tmux)"
	export REAL_TMUX

	BIN_DIR="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$BIN_DIR"
	printf '#!%s\nexec bash "%s/scripts/tmux-float-group.sh" "$@"\n' "$BASH" "$PWD" >"$BIN_DIR/tmux-float-group"
	printf '#!%s\nexec bash "%s/scripts/tmux-pane-chain.sh" "$@"\n' "$BASH" "$PWD" >"$BIN_DIR/tmux-pane-chain"
	chmod +x "$BIN_DIR/tmux-float-group" "$BIN_DIR/tmux-pane-chain"
	PATH="$BIN_DIR:$PATH"

	tmux -f /dev/null new-session -d -s S -x 200 -y 50 -c "$BATS_TEST_TMPDIR" -- sleep 3600
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	PARENT="$(tmux display-message -p -t "$WIN" '#{pane_id}')"
}

teardown() {
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	"$REAL_TMUX" kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

chain() { bash scripts/tmux-pane-chain.sh "$@"; }
alive() { tmux list-panes -a -F '#{pane_id}' | grep -qx "$1"; }
refute_alive() { ! alive "$1"; }

# child <parent>: a tiled child of <parent>; echoes its id.
child() {
	local id
	id="$(tmux split-window -d -P -F '#{pane_id}' -t "$WIN" -- sleep 3600)"
	tmux set-option -p -t "$id" @pane_parent "$1"
	echo "$id"
}

# float_child <parent>: a floating child of <parent>; echoes its id.
float_child() {
	local id
	if ! id="$(tmux new-pane -d -P -F '#{pane_id}' -t "$WIN" -x 60 -y 10 -X 5 -Y 5 -B heavy -- sleep 3600 2>/dev/null)"; then
		skip "this tmux cannot create a floating pane"
	fi
	tmux set-option -p -t "$id" @pane_parent "$1"
	echo "$id"
}

@test "close-children kills float, side, stashed and grand children; the parent survives" {
	local f s h g
	f="$(float_child "$PARENT")"
	s="$(child "$PARENT")"
	h="$(float_child "$PARENT")"
	tmux set-option -p -t "$h" @float_group crew
	bash scripts/tmux-float-group.sh hide crew "$PARENT"
	tmux list-panes -a -F '#{pane_id}|#{session_name}' | grep -qx "$h|scratch-og-float-stash"
	g="$(child "$s")"

	run chain close-children "$PARENT"
	[ "$status" -eq 0 ]
	refute_alive "$f"
	refute_alive "$s"
	refute_alive "$h"
	refute_alive "$g"
	alive "$PARENT"
}

@test "a parent cycle terminates and the parent survives" {
	local b
	b="$(child "$PARENT")"
	tmux set-option -p -t "$PARENT" @pane_parent "$b"

	run timeout 10 bash scripts/tmux-pane-chain.sh close-children "$PARENT"
	[ "$status" -eq 0 ]
	refute_alive "$b"
	alive "$PARENT"
}

@test "a @bridge_pane child survives and its own children are not traversed" {
	local b g
	b="$(child "$PARENT")"
	tmux set-option -p -t "$b" @bridge_pane 1
	g="$(child "$b")"

	run chain close-children "$PARENT"
	[ "$status" -eq 0 ]
	alive "$b"
	alive "$g"
}

@test "a pane that is not a child is left alone" {
	local other
	other="$(tmux split-window -d -P -F '#{pane_id}' -t "$WIN" -- sleep 3600)"
	child "$PARENT" >/dev/null

	run chain close-children "$PARENT"
	[ "$status" -eq 0 ]
	alive "$other"
}

@test "the pane-exited hook closes the children of an exited parent" {
	local c g other
	other="$(tmux split-window -d -P -F '#{pane_id}' -t "$WIN" -- sleep 3600)"
	tmux set-option -p -t "$PARENT" remain-on-exit off
	c="$(child "$PARENT")"
	g="$(child "$c")"
	tmux set-hook -g pane-exited "run-shell \"OG_PANE_CHAIN_BIN='$BIN_DIR/tmux-pane-chain' bash '$PWD/scripts/tmux-reap-pane.sh' #{q:hook_pane}\""

	kill -9 "$(tmux display-message -p -t "$PARENT" '#{pane_pid}')"
	for _ in $(seq 20); do
		alive "$c" || alive "$g" || break
		sleep 0.1
	done
	refute_alive "$c"
	refute_alive "$g"
	alive "$other"
}

@test "adopt stamps @pane_parent" {
	local c
	c="$(tmux split-window -d -P -F '#{pane_id}' -t "$WIN" -- sleep 3600)"
	run chain adopt "$c" "$PARENT"
	[ "$status" -eq 0 ]
	[ "$(tmux display-message -p -t "$c" '#{@pane_parent}')" = "$PARENT" ]
}

@test "adopt refuses itself and non-pane ids" {
	local c
	c="$(tmux split-window -d -P -F '#{pane_id}' -t "$WIN" -- sleep 3600)"
	run chain adopt "$c" "$c"
	[ "$status" -ne 0 ]
	run chain adopt "$c" "S:0.0"
	[ "$status" -ne 0 ]
	run chain adopt "foo" "$PARENT"
	[ "$status" -ne 0 ]
	[ -z "$(tmux display-message -p -t "$c" '#{@pane_parent}')" ]
}

@test "close-children refuses a non-pane id and kills nothing" {
	local c
	c="$(child "$PARENT")"
	run chain close-children "S:0.0"
	[ "$status" -ne 0 ]
	run chain close-children ""
	[ "$status" -ne 0 ]
	alive "$c"
}
