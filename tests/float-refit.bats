#!/usr/bin/env bats
# Tests scripts/tmux-float-refit.sh (#651): upstream tmux (tmux/tmux#5582,
# f8fc53b6, already pinned) now runs layout_clamp_floating_panes() on every
# resize and keeps every floating pane fully inside its window — but that
# clamp only ever shrinks or repositions the already-resolved absolute cell;
# it has no notion of the percentage a float was created at. This script
# re-issues resize-pane/move-pane with the @float_geom percentages stamped at
# creation, so tmux re-resolves them against the window's CURRENT size.
#
# Runs the real script against a private, config-less tmux server (like
# tests/worktree-match-integration.bats) so its bare `tmux` calls hit real
# windows/panes, not fakes, and so the real upstream clamp is what runs on
# resize-window, not a stand-in for it. This needs the pinned next-3.8 tmux
# (mkTmux in flake.nix's float-refit-tests check) — `list-commands new-pane`
# on nixpkgs' stock tmux advertises -A/-B/-X/-Y but its parser rejects them,
# so a plain `pkgs.tmux` would make every float-creating test skip rather than
# run (see flake.nix's pickerChecked comment on the same trap).
#
# Expected geometry is computed, not hardcoded: for a -B heavy (bordered)
# float, tmux resolves a percentage against the window's raw cell size with
# truncating integer division (arguments.c's args_string_percentage: `(curval
# * pct) / 100`), then insets the result by 1 cell per side for the border —
# -2 on size (layout.c's layout_resize_floating_pane_to), +1 on offset
# (cmd-join-pane.c's move-pane handler). Verified against the pinned tmux
# binary directly before writing these assertions.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	export TMUX_TMPDIR="/tmp/og-fr-$$-${BATS_TEST_NUMBER}"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	# A private TMUX_TMPDIR does not isolate CLAUDE_STATUS_DIR (CLAUDE.md: it
	# defaults to a bare /tmp path shared by every tmux server on the
	# machine) — this script never reads it, but every scratch-server test in
	# this repo isolates it anyway rather than assume that stays true.
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	# -f /dev/null: a real config would arm the window-resized hook that runs
	# this very script automatically, which would invalidate the before/after
	# comparisons below (most of all the "skips" test, whose whole point is
	# that nothing touches the unstamped pane).
	tmux -f /dev/null new-session -d -s S -c "$TMUX_TMPDIR" -x 80 -y 24
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	FLOAT=""
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "$TMUX_TMPDIR"
}

# Echoes "width|height|left|top" for a -B heavy floating pane created (or
# reasserted) at the given percentages of a window of the given cell size —
# see the header comment for the arithmetic this mirrors.
float_geom_expected() {
	local ww=$1 wh=$2 wpct=$3 hpct=$4 xpct=$5 ypct=$6
	printf '%d|%d|%d|%d' \
		"$((ww * wpct / 100 - 2))" "$((wh * hpct / 100 - 2))" \
		"$((ww * xpct / 100 + 1))" "$((wh * ypct / 100 + 1))"
}

# Creates a floating pane in $WIN at 90%/90%/5%/5% (mkFloat's floatFull shape)
# and leaves its pane id in $FLOAT. Skips the test on a tmux build that can't
# create or report floating panes, matching update-icons-cwd-move.bats.
make_float() {
	if ! tmux new-pane -t "$WIN" -x 90% -y 90% -X 5% -Y 5% -B heavy 2>/dev/null; then
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
	FLOAT="$(tmux list-panes -t "$WIN" -f '#{pane_floating_flag}' -F '#{pane_id}')"
}

@test "grows: reasserts the creation percentage against a larger window" {
	make_float
	tmux set -p -t "$FLOAT" @float_geom '90% 90% 5% 5%'

	tmux resize-window -t "$WIN" -x 200 -y 50

	# Upstream's own clamp never grows an in-bounds float, so right after the
	# resize (before the script runs) the pane is still sized off the OLD
	# 80x24 window.
	local before
	before="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}')"
	[ "$before" = "$(float_geom_expected 80 24 90 90 5 5 | cut -d'|' -f1,2)" ]

	bash scripts/tmux-float-refit.sh "$WIN"

	local after
	after="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')"
	[ "$after" = "$(float_geom_expected 200 50 90 90 5 5)" ]
}

@test "shrinks: reasserts the smaller creation percentage, not just upstream's clamp" {
	tmux resize-window -t "$WIN" -x 200 -y 50
	make_float
	tmux set -p -t "$FLOAT" @float_geom '90% 90% 5% 5%'

	tmux resize-window -t "$WIN" -x 100 -y 30

	# Upstream's clamp already shrank the oversized float to fit — but to a
	# different, smaller-than-intended size than the 90% this script targets,
	# so the two are a real discriminator rather than coincidentally equal.
	local clamped
	clamped="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')"
	local expected
	expected="$(float_geom_expected 100 30 90 90 5 5)"
	[ "$clamped" != "$expected" ]

	bash scripts/tmux-float-refit.sh "$WIN"

	local after
	after="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')"
	[ "$after" = "$expected" ]
}

@test "skips a float with no @float_geom stamp" {
	make_float
	# Deliberately no @float_geom stamp — mimics a mouse Ctrl-drag float,
	# whose geometry is the user's and not ours to reassert. Upstream's own
	# clamp already keeps it fully on screen with no help from this script.

	tmux resize-window -t "$WIN" -x 200 -y 50

	local before after
	before="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')"

	bash scripts/tmux-float-refit.sh "$WIN"

	after="$(tmux display-message -p -t "$FLOAT" '#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')"

	[ "$before" = "$after" ]
}

@test "window with only tiled panes: exits 0 and does nothing" {
	tmux split-window -t "$WIN"

	run bash scripts/tmux-float-refit.sh "$WIN"
	[ "$status" -eq 0 ]
}

@test "no target argument: no-op, exits 0" {
	run bash scripts/tmux-float-refit.sh
	[ "$status" -eq 0 ]

	run bash scripts/tmux-float-refit.sh ""
	[ "$status" -eq 0 ]
}
