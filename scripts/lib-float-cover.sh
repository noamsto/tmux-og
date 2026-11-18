#!/usr/bin/env bash
# Pure floating-pane visibility math. Sourced (not executed): no tmux calls, no
# Nix build-time placeholders, so it is directly testable under bats
# (tests/float-cover.bats). Functions set REPLY instead of echoing, to avoid a
# subshell fork (same convention as lib-reflow.sh).
#
# All geometry is cells in window coordinates (#{pane_left}/#{pane_top} and
# #{pane_width}/#{pane_height}); a pane spans [left, left+width) x [top, top+height).

# shellcheck disable=SC2034  # REPLY is the output, used by callers

# The smallest uncovered tile strip that still counts as "visible" on a side.
FLOAT_VISIBLE_COLS=8
FLOAT_VISIBLE_ROWS=4

# float_covers_view LEFT TOP WIDTH HEIGHT WIN_WIDTH WIN_HEIGHT
#
# REPLY=1 when the float leaves less than FLOAT_VISIBLE_COLS of tile on both
# its left and right and less than FLOAT_VISIBLE_ROWS on both its top and
# bottom, i.e. no side of the window shows a usable strip of what is under it;
# else 0. A float sticking out past the window edge counts as a zero margin.
float_covers_view() {
	local left=$1 top=$2 width=$3 height=$4 win_width=$5 win_height=$6
	local right_margin=$((win_width - left - width))
	local bottom_margin=$((win_height - top - height))
	if [ "$left" -lt "$FLOAT_VISIBLE_COLS" ] && [ "$right_margin" -lt "$FLOAT_VISIBLE_COLS" ] &&
		[ "$top" -lt "$FLOAT_VISIBLE_ROWS" ] && [ "$bottom_margin" -lt "$FLOAT_VISIBLE_ROWS" ]; then
		REPLY=1
	else
		REPLY=0
	fi
}

# float_tile_visible DIR O_LEFT O_TOP O_WIDTH O_HEIGHT T_LEFT T_TOP T_WIDTH T_HEIGHT
#
# DIR is the move direction (L, R, U or D). The O_* box is the origin float, the
# T_* box a tiled candidate on that side. REPLY=1 when at least
# FLOAT_VISIBLE_COLS (L/R) or FLOAT_VISIBLE_ROWS (U/D) of the tile lies beyond
# the float's near edge, uncovered by it; else 0.
float_tile_visible() {
	local dir=$1 o_left=$2 o_top=$3 o_width=$4 o_height=$5
	local t_left=$6 t_top=$7 t_width=$8 t_height=$9
	local strip
	case "$dir" in
	L) strip=$((o_left < t_left + t_width ? o_left - t_left : t_width)) ;;
	R) strip=$((t_left + t_width - (o_left + o_width > t_left ? o_left + o_width : t_left))) ;;
	U) strip=$((o_top < t_top + t_height ? o_top - t_top : t_height)) ;;
	D) strip=$((t_top + t_height - (o_top + o_height > t_top ? o_top + o_height : t_top))) ;;
	*)
		REPLY=0
		return 0
		;;
	esac
	local need=$FLOAT_VISIBLE_COLS
	case "$dir" in U | D) need=$FLOAT_VISIBLE_ROWS ;; esac
	if [ "$strip" -ge "$need" ]; then REPLY=1; else REPLY=0; fi
}
