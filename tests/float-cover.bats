#!/usr/bin/env bats
# Unit tests for scripts/lib-float-cover.sh: pure geometry, no tmux needed.

setup() {
	# shellcheck source=/dev/null
	source "$(dirname "$BATS_TEST_DIRNAME")/scripts/lib-float-cover.sh"
}

# covers L T W H  (window is 100x40)
covers() {
	float_covers_view "$1" "$2" "$3" "$4" 100 40
	echo "$REPLY"
}

# vis DIR  O_L O_T O_W O_H  T_L T_T T_W T_H
vis() {
	float_tile_visible "$@"
	echo "$REPLY"
}

@test "constants: 8 columns, 4 rows" {
	[ "$FLOAT_VISIBLE_COLS" -eq 8 ]
	[ "$FLOAT_VISIBLE_ROWS" -eq 4 ]
}

@test "covers_view: a float filling the window covers" {
	[ "$(covers 0 0 100 40)" = 1 ]
}

@test "covers_view: margins just under the thresholds still cover" {
	# 7 cols each side, 3 rows each side.
	[ "$(covers 7 3 86 34)" = 1 ]
}

@test "covers_view: 8 columns on the left uncovers" {
	[ "$(covers 8 3 85 34)" = 0 ]
}

@test "covers_view: 8 columns on the right uncovers" {
	[ "$(covers 7 3 85 34)" = 0 ]
}

@test "covers_view: 4 rows on the top uncovers" {
	[ "$(covers 7 4 86 33)" = 0 ]
}

@test "covers_view: 4 rows on the bottom uncovers" {
	[ "$(covers 7 3 86 33)" = 0 ]
}

@test "covers_view: a float overflowing the window covers" {
	[ "$(covers -2 -1 110 45)" = 1 ]
}

@test "tile_visible R: 7 columns beyond the float are hidden, 8 visible" {
	[ "$(vis R 30 5 63 30 51 0 49 40)" = 0 ]
	[ "$(vis R 30 5 62 30 51 0 49 40)" = 1 ]
}

@test "tile_visible L: 7 columns before the float are hidden, 8 visible" {
	[ "$(vis L 7 5 40 30 0 0 50 40)" = 0 ]
	[ "$(vis L 8 5 40 30 0 0 50 40)" = 1 ]
}

@test "tile_visible D: 3 rows below the float are hidden, 4 visible" {
	[ "$(vis D 10 2 80 35 0 20 100 20)" = 0 ]
	[ "$(vis D 10 2 80 34 0 20 100 20)" = 1 ]
}

@test "tile_visible U: 3 rows above the float are hidden, 4 visible" {
	[ "$(vis U 10 3 80 30 0 0 100 19)" = 0 ]
	[ "$(vis U 10 4 80 30 0 0 100 19)" = 1 ]
}

@test "tile_visible: a tile entirely beyond the float is fully visible" {
	[ "$(vis R 0 0 20 20 30 0 10 20)" = 1 ]
	[ "$(vis D 0 0 20 10 0 15 20 4)" = 1 ]
}

@test "tile_visible: a float overflowing past the tile hides it" {
	[ "$(vis R 0 0 120 40 51 0 49 40)" = 0 ]
	[ "$(vis L -5 0 120 40 0 0 50 40)" = 0 ]
	[ "$(vis D 0 -2 100 45 0 20 100 20)" = 0 ]
	[ "$(vis U 0 -2 100 45 0 0 100 19)" = 0 ]
}

@test "tile_visible: an unknown direction is not visible" {
	[ "$(vis X 0 0 10 10 20 0 10 10)" = 0 ]
}
