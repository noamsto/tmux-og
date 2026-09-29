#!/usr/bin/env bats
# Tests scripts/tmux-float-nudge.sh (#864): the keyboard float resize and the
# @float_geom rewrite that keeps a hand-resized float across later window
# resizes. Upstream resize-pane on a float only ever GROWS it (no shrink flag)
# and never clamps, and @float_geom is written only at creation, so a hand
# resize is reverted by the next tmux-float-refit unless the stamp is rewritten.
#
# Runs the real script against a private, config-less tmux server (like
# tests/float-refit.bats), so its bare `tmux` calls hit real windows/panes and
# the real upstream clamping/percentage arithmetic runs, not a stand-in. Needs
# the pinned next-3.9 tmux (mkTmux in flake.nix's float-nudge-tests check).

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	OG_TMUX_DIR="/tmp/og-fn-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	# -f /dev/null: the real config would arm hooks that mutate @float_geom /
	# resize the float behind the assertions below.
	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	FLOAT=""
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

# A -B heavy float at 50%/90%/48%/5% of 200x50 resolves to 98x43+97+3 (see
# float-refit.bats' header for the -2 size / +1 offset arithmetic). Leaves its
# pane id in $FLOAT.
make_float() {
	if ! tmux new-pane -t "$WIN" -x 50% -y 90% -X 48% -Y 5% -B heavy 2>/dev/null; then
		skip "this tmux cannot create a floating pane"
	fi
	local floating
	floating="$(tmux list-panes -t "$WIN" -F '#{pane_floating_flag}' | tr -d '\n')"
	case "$floating" in
	*1*) ;;
	*)
		skip "this tmux does not report pane_floating_flag"
		;;
	esac
	FLOAT="$(tmux list-panes -t "$WIN" -f '#{pane_floating_flag}' -F '#{pane_id}' | tail -1)"
}

geom() { tmux display-message -p -t "$FLOAT" '#{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}'; }
stamp() { tmux display-message -p -t "$FLOAT" '#{@float_geom}'; }
refit_size() { tmux display-message -p -t "$FLOAT" '#{@float_refit_size}'; }

@test "grows left and rewrites a percentage stamp" {
	make_float
	tmux set -p -t "$FLOAT" @float_geom '50% 90% 48% 5%'
	[ "$(geom)" = "98x43+97+3" ]

	bash scripts/tmux-float-nudge.sh "$FLOAT" L 5

	# room 96 >= 5: grow left by 5 -> 103x43+92+3.
	[ "$(geom)" = "103x43+92+3" ]
	# (103+2)*100/200=52, (43+2)*100/50=90, (92-1)*100/200=45, (3-1)*100/50=4.
	[ "$(stamp)" = "52% 90% 45% 4%" ]
	[ "$(refit_size)" = "200x50" ]
}

@test "all four directions change the float's geometry" {
	make_float
	tmux set -p -t "$FLOAT" @float_geom '50% 90% 48% 5%'
	local before after
	for d in L R U D; do
		before="$(geom)"
		bash scripts/tmux-float-nudge.sh "$FLOAT" "$d" 5
		after="$(geom)"
		[ "$before" != "$after" ]
	done
}

@test "at the edge it shrinks instead of escaping the window" {
	make_float
	tmux set -p -t "$FLOAT" @float_geom '50% 90% 48% 5%'
	# Flush against the left edge: pane_left 1, so no room to grow left.
	tmux move-pane -t "$FLOAT" -X 0 -Y 5
	local w0 l0
	w0="$(tmux display-message -p -t "$FLOAT" '#{pane_width}')"
	l0="$(tmux display-message -p -t "$FLOAT" '#{pane_left}')"
	[ "$l0" = "1" ]

	bash scripts/tmux-float-nudge.sh "$FLOAT" L 5

	# Shrink from the left edge: width-5, left+5 — never negative.
	[ "$(tmux display-message -p -t "$FLOAT" '#{pane_width}')" = "$((w0 - 5))" ]
	[ "$(tmux display-message -p -t "$FLOAT" '#{pane_left}')" = "$((l0 + 5))" ]
}

@test "an unstamped float is ignored" {
	make_float
	local before
	before="$(geom)"

	run bash scripts/tmux-float-nudge.sh "$FLOAT" L 5
	[ "$status" -eq 0 ]

	[ "$(geom)" = "$before" ]
	[ -z "$(stamp)" ]
	[ -z "$(refit_size)" ]
}

@test "a cell-stamped float keeps cells for size and percentages for offsets" {
	make_float
	# The enrich card's shape: size in cells, offsets percentages.
	tmux set -p -t "$FLOAT" @float_geom '64 18 20% 15%'

	bash scripts/tmux-float-nudge.sh "$FLOAT" L 5

	# Grow left -> 103x43+92+3; size fields stay cells (+2), offsets percentages.
	[ "$(geom)" = "103x43+92+3" ]
	[ "$(stamp)" = "105 45 45% 4%" ]
}

@test "a non-floating pane is a no-op" {
	tmux split-window -t "$WIN"
	local tiled
	tiled="$(tmux display-message -p -t "$WIN" '#{pane_id}')"
	# No @float_geom anyway, but the floating-flag branch must also reject it.
	local before
	before="$(tmux display-message -p -t "$tiled" '#{pane_width}x#{pane_height}')"

	run bash scripts/tmux-float-nudge.sh "$tiled" L 5
	[ "$status" -eq 0 ]

	[ "$(tmux display-message -p -t "$tiled" '#{pane_width}x#{pane_height}')" = "$before" ]
}

@test "an empty or non-pane argument is a silent no-op" {
	run bash scripts/tmux-float-nudge.sh "" L 5
	[ "$status" -eq 0 ]
	run bash scripts/tmux-float-nudge.sh "not-a-pane" stamp
	[ "$status" -eq 0 ]
	run bash scripts/tmux-float-nudge.sh "%1" bogus 5
	[ "$status" -eq 0 ]
}
