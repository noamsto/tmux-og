#!/usr/bin/env bats
# Covers the reflow grid math (scripts/lib-reflow.sh): how a row's columns are
# sized from per-window widths, and which label detail + row count the ladder
# settles on. See scripts/tmux-reflow-windows.sh for how the real script derives
# the floor/want lists and the single-line totals from live window data.

load helper

setup() {
	setup_lib_reflow
}

# Sum of REPLY_COLWS plus the per-slot overhead and separators — the width the
# row actually renders at.
row_width() {
	local per=$1 overhead=$2 sep_width=$3 w sum=0
	local -a colws
	read -ra colws <<<"$REPLY_COLWS"
	for w in "${colws[@]}"; do sum=$((sum + w)); done
	echo $((sum + per * overhead + (per - 1) * sep_width))
}

# reflow_fit_columns PER AVAILABLE OVERHEAD SEP_WIDTH FLOORS WANTS

@test "fit_columns: every column takes its own want, not the widest window's" {
	# 4 windows, 2 columns: column 0 stacks windows 0+2, column 1 stacks 1+3.
	reflow_fit_columns 2 200 10 3 "5 5 5 5" "30 12 20 8"
	[ "$REPLY_COLWS" = "30 12" ]
	[ "$REPLY_FITS" = 1 ]
	[ "$REPLY_MIN_STARVED" = 0 ]
}

@test "fit_columns: a column is sized by the widest window stacked in it" {
	reflow_fit_columns 3 300 10 3 "0 0 0 0 0 0" "10 20 30 40 5 5"
	# col0 = max(w0,w3) = 40, col1 = max(w1,w4) = 20, col2 = max(w2,w5) = 30
	[ "$REPLY_COLWS" = "40 20 30" ]
	[ "$REPLY_FITS" = 1 ]
}

@test "fit_columns: slack goes to the columns with the most unmet demand" {
	# budget = 60 - 3 - 2*10 = 37; floors 10+10 = 20, so 17 cells of slack
	# against demands of 20 and 30.
	reflow_fit_columns 2 60 10 3 "10 10" "30 40"
	[ "$REPLY_COLWS" = "17 20" ]
	[ "$REPLY_FITS" = 0 ]
	[ "$REPLY_MIN_STARVED" = 17 ]
	[ "$(row_width 2 10 3)" -eq 60 ]
}

@test "fit_columns: incompressible floors are honoured before any slack (#271)" {
	# The live 8-window/123-col session from the report: the old single colw
	# came out at 13 while two columns needed 15 and 17, so their slots rendered
	# wide and shifted every column to their right.
	reflow_fit_columns 3 118 24 3 "11 4 17 15 6 4 15 4" "23 28 34 35 35 28 35 28"
	[ "$REPLY_COLWS" = "16 7 17" ]
	[ "$REPLY_FITS" = 0 ]
	[ "$(row_width 3 24 3)" -eq 118 ]
}

@test "fit_columns: floors that cannot fit split the budget evenly" {
	# budget = 60 - 3 - 2*10 = 37 against floors of 30+30: no allocation keeps
	# both, so the caller is handed even columns and clips identities to match.
	reflow_fit_columns 2 60 10 3 "30 30" "40 40"
	[ "$REPLY_COLWS" = "19 18" ]
	[ "$REPLY_FITS" = 0 ]
	[ "$(row_width 2 10 3)" -eq 60 ]
}

@test "fit_columns: columns never go below the min-colw floor" {
	# Pathologically narrow: the even split would be 0 cells per column.
	reflow_fit_columns 3 40 10 3 "20 20 20" "40 40 40"
	[ "$REPLY_COLWS" = "6 6 6" ]
	[ "$REPLY_FITS" = 0 ]
}

# reflow_clip_rests RESTS OVERSHOOT MIN_REST

@test "clip_rests: the overshoot comes off the widest label" {
	reflow_clip_rests "40 20 10" 5 16
	[ "$REPLY_OK" = 1 ]
	[ "$REPLY_RESTS" = "35 20 10" ]
}

@test "clip_rests: the widest labels level off before a shorter one is touched" {
	# 40 and 38 meet at 34 (6 + 4 cells); the 10 never pays.
	reflow_clip_rests "40 38 10" 10 16
	[ "$REPLY_OK" = 1 ]
	[ "$REPLY_RESTS" = "34 34 10" ]
}

@test "clip_rests: an integer cap's surplus goes back to a label it cut" {
	# Only cap 39 recovers 2 cells, and it recovers 3 -- the spare cell returns
	# to a clipped label, never to one this pass left alone.
	reflow_clip_rests "41 40" 2 16
	[ "$REPLY_OK" = 1 ]
	[ "$REPLY_RESTS" = "40 39" ]
}

@test "clip_rests: refuses when the cap would sink below the floor" {
	reflow_clip_rests "20 20" 30 16
	[ "$REPLY_OK" = 0 ]
	[ "$REPLY_RESTS" = "20 20" ]
}

@test "clip_rests: nothing to recover leaves every label whole" {
	reflow_clip_rests "40 20" 0 16
	[ "$REPLY_OK" = 1 ]
	[ "$REPLY_RESTS" = "40 20" ]
}

# reflow_pick_layout FLOORS WANTS_LONG WANTS_SHORT TOTAL_LONG TOTAL_SHORT
#                     TOTAL AVAILABLE ZOOM_EXTRA OVERHEAD SEP_WIDTH
#                     MAX_WIN_LINES LONG_TRUNC_FLOOR

@test "pick_layout: long labels fit on a single line" {
	reflow_pick_layout "0 0 0" "30 30 30" "10 10 10" 60 30 3 80 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = long ]
	[ "$REPLY_NEEDS_MULTILINE" = 0 ]
	[ "$REPLY_PER" = 3 ]
}

@test "pick_layout: zoom_extra can tip a fitting single line into the grid" {
	reflow_pick_layout "0 0 0" "30 30 30" "10 10 10" 78 30 3 80 4 10 3 3 24
	[ "$REPLY_NEEDS_MULTILINE" = 1 ]
}

@test "pick_layout: rung 1.5 keeps the row and clips the widest label" {
	# 6 cells over (zoom marker included): shaving them off the 40-cell label
	# beats a second status row, which would cap every label at its column too.
	reflow_pick_layout "0 0 0" "30 30 30" "10 10 10" 82 30 3 80 4 10 3 3 24 "40 20 20" 16
	[ "$REPLY_LABELS_MODE" = long ]
	[ "$REPLY_NEEDS_MULTILINE" = 0 ]
	[ "$REPLY_PER" = 3 ]
	[ "$REPLY_RESTS" = "34 20 20" ]
}

@test "pick_layout: rung 1.5 hands the row to the grid below the clip floor" {
	# 120 cells over, with only 12 to give above the floor.
	reflow_pick_layout "0 0 0" "30 30 30" "10 10 10" 200 30 3 80 0 10 3 3 24 "20 20 20" 16
	[ "$REPLY_NEEDS_MULTILINE" = 1 ]
	[ -z "$REPLY_RESTS" ]
}

@test "pick_layout: a row that already fits clips nothing" {
	reflow_pick_layout "0 0 0" "30 30 30" "10 10 10" 60 30 3 80 0 10 3 3 24 "40 20 20" 16
	[ "$REPLY_NEEDS_MULTILINE" = 0 ]
	[ -z "$REPLY_RESTS" ]
}

@test "pick_layout: long grid takes the fewest rows that still fit every want" {
	# 6 windows wanting 30 each: 3 columns need 3*30 + 3*10 + 2*3 = 126 > 120,
	# so it settles on 2 columns over 3 rows.
	reflow_pick_layout "0 0 0 0 0 0" "30 30 30 30 30 30" "10 10 10 10 10 10" \
		1000 30 6 120 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = long ]
	[ "$REPLY_NEEDS_MULTILINE" = 1 ]
	[ "$REPLY_PER" = 2 ]
	[ "$REPLY_COLWS" = "30 30" ]
}

@test "pick_layout: rung 2.5 keeps long labels while starved columns clear the floor" {
	# 7 windows over 3 rows -> 3 columns, which cannot reach a want of 30 but
	# stays above LONG_TRUNC_FLOOR, so the branch is clipped rather than swapped
	# for a compact id.
	reflow_pick_layout "0 0 0 0 0 0 0" "30 30 30 30 30 30 30" "10 10 10 10 10 10 10" \
		1000 30 7 120 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = long ]
	[ "$REPLY_PER" = 3 ]
	[ "$REPLY_MIN_STARVED" -ge 24 ]
}

@test "pick_layout: LONG_TRUNC_FLOOR guard falls through to short labels" {
	# 10 windows in 60 cells leaves each column far below the floor, so the
	# ladder must not settle on a sliver-wide long grid.
	reflow_pick_layout "0 0 0 0 0 0 0 0 0 0" \
		"30 30 30 30 30 30 30 30 30 30" "8 8 8 8 8 8 8 8 8 8" \
		1000 40 10 60 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = short ]
}

@test "pick_layout: short labels fit on a single line" {
	reflow_pick_layout "0 0 0 0 0" "30 30 30 30 30" "8 8 8 8 8" 500 40 5 50 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = short ]
	[ "$REPLY_NEEDS_MULTILINE" = 0 ]
}

@test "pick_layout: short grid when the compact line does not fit either" {
	reflow_pick_layout "0 0 0 0 0" "30 30 30 30 30" "8 8 8 8 8" 500 200 5 50 0 10 3 3 24
	[ "$REPLY_LABELS_MODE" = short ]
	[ "$REPLY_NEEDS_MULTILINE" = 1 ]
	[ "$REPLY_COLWS" = "8 8" ]
}

@test "pick_layout: a taller row cap buys wider columns and richer labels" {
	# The same 8-window session: 3 rows means 3 columns and compact ids, while a
	# tall client's 4th row leaves 2 columns wide enough for long labels.
	local floors="11 4 17 15 6 4 15 4"
	local wants="23 28 34 35 35 28 35 28"
	reflow_pick_layout "$floors" "$wants" "$wants" 900 900 8 118 0 24 3 3 24
	[ "$REPLY_PER" = 3 ]
	[ "$REPLY_LABELS_MODE" = short ]

	reflow_pick_layout "$floors" "$wants" "$wants" 900 900 8 118 0 24 3 4 24
	[ "$REPLY_PER" = 2 ]
	[ "$REPLY_LABELS_MODE" = long ]
	[ "$(row_width 2 24 3)" -le 118 ]
}

@test "pick_layout: grid columns always hold every window's floor when they can" {
	# The #271 invariant: with floors that fit the budget, no window's
	# incompressible identity may exceed the column it lands in.
	local floors="11 4 17 15 6 4 15 4"
	local wants="23 28 34 35 35 28 35 28"
	reflow_pick_layout "$floors" "$wants" "$wants" 900 900 8 118 0 24 3 3 24
	local -a colws f
	read -ra colws <<<"$REPLY_COLWS"
	read -ra f <<<"$floors"
	local i c
	for i in "${!f[@]}"; do
		c=$((i % REPLY_PER))
		[ "${f[i]}" -le "${colws[c]}" ]
	done
}
