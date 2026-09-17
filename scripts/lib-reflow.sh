#!/usr/bin/env bash
# Pure window-grid layout math for tmux-reflow-windows. Sourced (not
# executed) — no tmux calls, no Nix build-time placeholders, so it's directly
# testable under bats (tests/reflow.bats).
# Functions use the REPLY convention (set REPLY* instead of echoing) to avoid
# subshell forks, matching lib-icons.sh / lib-claude.sh / lib-enrich.sh.
# Accumulators assign (sum=$((sum + x))) rather than ((sum += x)): a standalone
# arithmetic command whose result is 0 exits 1, which aborts under errexit.

# shellcheck disable=SC2034  # REPLY* outputs are used by callers

# Narrowest a starved column may get. Only reached once even the incompressible
# parts overflow the row, where running past the row width is unavoidable.
REFLOW_MIN_COLW=6

# reflow_fit_columns PER AVAILABLE OVERHEAD SEP_WIDTH FLOORS WANTS
#                    [PRS] [MAX_REST]
#
# Size the PER columns of one grid row. FLOORS and WANTS are space-separated
# per-window widths in window order; the window at position p sits in column
# p % PER, so a column takes the max over the windows stacked in it. A floor is
# the part the renderer cannot shrink (issue id + agent badge + zoom marker); a
# want additionally covers the whole branch/title, uncapped.
#
# Columns are sized independently because alignment only needs a width to match
# *down* a column, never across — charging every column the widest window's
# width (one uniform colw) is what overflowed the row in issue #271.
#
# PRS are the per-window PR badge widths. The badge forms a column of its own
# beside the label, sized the same way and for the same reason (#688): the
# per-column max leaves the budget here rather than riding OVERHEAD, so a badge
# is charged to the one column that holds it.
#
# MAX_REST caps how much of a want is claimed while the row is tight, so one
# very long title cannot stretch the grid; 0 disables the cap. It is a cap on
# the claim, not on the label: a row that fits its capped wants hands the
# leftover out toward the real ones below.
#
# Sets REPLY_COLWS (space-separated column widths), REPLY_PR_COLWS (per-column
# PR widths, same order), REPLY_FITS (1 when every column reached its capped
# want) and REPLY_MIN_STARVED (narrowest width among the columns that did not,
# 0 when none did).
reflow_fit_columns() {
	local per=$1 available=$2 overhead=$3 sep_width=$4 max_rest=${8:-0}
	local -a floors wants prs
	read -ra floors <<<"$5"
	read -ra wants <<<"$6"
	read -ra prs <<<"${7:-}"
	((per < 1)) && per=1

	local -a cf=() cw=() cm=() cp=() width=() demand=()
	local i c n=${#floors[@]} want pr
	for ((c = 0; c < per; c++)); do
		cf[c]=0
		cw[c]=0
		cm[c]=0
		cp[c]=0
	done
	for ((i = 0; i < n; i++)); do
		c=$((i % per))
		((floors[i] > cf[c])) && cf[c]=${floors[i]}
		want=${wants[i]}
		((max_rest > 0 && want > floors[i] + max_rest)) && want=$((floors[i] + max_rest))
		((want > cw[c])) && cw[c]=$want
		((wants[i] > cm[c])) && cm[c]=${wants[i]}
		pr=${prs[i]:-0}
		((pr > cp[c])) && cp[c]=$pr
	done

	local budget=$((available - (per - 1) * sep_width - per * overhead))
	local sum_f=0 sum_w=0 sum_m=0
	for ((c = 0; c < per; c++)); do
		sum_f=$((sum_f + cf[c]))
		sum_w=$((sum_w + cw[c]))
		sum_m=$((sum_m + cm[c]))
		budget=$((budget - cp[c]))
	done
	((budget < 0)) && budget=0
	REPLY_PR_COLWS="${cp[*]}"

	if ((sum_w <= budget)); then
		REPLY_FITS=1
		REPLY_MIN_STARVED=0
		if ((sum_m <= budget)); then
			# Every label fits whole; anything past that is trailing padding.
			REPLY_COLWS="${cm[*]}"
			return
		fi
		# The cap held a column below the label it has to show, and the row has
		# cells left over — hand them out toward the real want, in proportion to
		# what the cap took, so a column already showing everything stays put.
		local slack=$((budget - sum_w)) total_demand=0 handed=0 leftover
		for ((c = 0; c < per; c++)); do
			demand[c]=$((cm[c] - cw[c]))
			total_demand=$((total_demand + demand[c]))
		done
		for ((c = 0; c < per; c++)); do
			width[c]=$((cw[c] + slack * demand[c] / total_demand))
			handed=$((handed + width[c] - cw[c]))
		done
		leftover=$((slack - handed))
		for ((c = 0; c < per && leftover > 0; c++)); do
			((width[c] < cm[c])) || continue
			width[c]=$((width[c] + 1))
			leftover=$((leftover - 1))
		done
		REPLY_COLWS="${width[*]}"
		return
	fi
	REPLY_FITS=0

	if ((sum_f > budget)); then
		# Even the incompressible parts overflow. Split the budget evenly and
		# let the caller clip identities to match; REFLOW_MIN_COLW keeps a
		# column from degrading to a bare ellipsis.
		local even=$((budget / per)) extra=$((budget % per))
		for ((c = 0; c < per; c++)); do
			width[c]=$((even + (c < extra ? 1 : 0)))
			((width[c] < REFLOW_MIN_COLW)) && width[c]=$REFLOW_MIN_COLW
		done
	else
		# Floors first, then hand the slack out in proportion to unmet demand.
		# give <= demand holds for every column because slack < total_demand
		# (sum_w > budget), so none overshoots its want and needs clamping.
		local slack=$((budget - sum_f)) total_demand=0 handed=0 leftover
		for ((c = 0; c < per; c++)); do
			demand[c]=$((cw[c] - cf[c]))
			total_demand=$((total_demand + demand[c]))
		done
		for ((c = 0; c < per; c++)); do
			width[c]=$((cf[c] + slack * demand[c] / total_demand))
			handed=$((handed + width[c] - cf[c]))
		done
		# Integer division leaves up to per-1 cells unspent; give them to the
		# columns still short of their want.
		leftover=$((slack - handed))
		for ((c = 0; c < per && leftover > 0; c++)); do
			((width[c] < cw[c])) || continue
			width[c]=$((width[c] + 1))
			leftover=$((leftover - 1))
		done
	fi

	# Seeded out of band so a column starved to 0 cells reports 0 instead of
	# colliding with the "nothing starved" encoding. REPLY_FITS=0 guarantees a
	# starved column here, so the seed never survives.
	REPLY_MIN_STARVED=-1
	for ((c = 0; c < per; c++)); do
		((width[c] >= cw[c])) && continue
		((REPLY_MIN_STARVED < 0 || width[c] < REPLY_MIN_STARVED)) && REPLY_MIN_STARVED=${width[c]}
	done
	REPLY_COLWS="${width[*]}"
}

# reflow_clip_rests RESTS OVERSHOOT MIN_REST
#
# Shave OVERSHOOT cells off the per-window rest widths in RESTS (the branch or
# title after the identity, window order), water-filled: every rest above a
# common cap is cut to it, so the labels that lose text are the ones that had
# text to spare and a short one is never touched. The cap may not sink below
# MIN_REST.
#
# Sets REPLY_RESTS (space-separated allowed widths) and REPLY_OK (1 when the
# overshoot was recovered at or above MIN_REST, 0 when it could not be and
# REPLY_RESTS is the input unchanged).
reflow_clip_rests() {
	local overshoot=$2 min_rest=$3
	local -a rests
	read -ra rests <<<"$1"
	local i n=${#rests[@]} cap hi=0 lo saved best=-1 surplus
	local -a clipped=()

	if ((overshoot <= 0)); then
		REPLY_RESTS="${rests[*]}"
		REPLY_OK=1
		return
	fi

	for ((i = 0; i < n; i++)); do ((rests[i] > hi)) && hi=${rests[i]}; done
	# saved(cap) is non-increasing in cap, so binary search the largest cap that
	# still recovers the overshoot -- the least clipping that does the job.
	lo=$min_rest
	while ((lo <= hi)); do
		cap=$(((lo + hi) / 2))
		saved=0
		for ((i = 0; i < n; i++)); do ((rests[i] > cap)) && saved=$((saved + rests[i] - cap)); done
		if ((saved >= overshoot)); then
			best=$cap
			lo=$((cap + 1))
		else
			hi=$((cap - 1))
		fi
	done

	if ((best < 0)); then
		REPLY_RESTS="${rests[*]}"
		REPLY_OK=0
		return
	fi

	saved=0
	for ((i = 0; i < n; i++)); do
		((rests[i] > best)) || continue
		saved=$((saved + rests[i] - best))
		rests[i]=$best
		clipped+=("$i")
	done
	# An integer cap saves more than asked by up to per-window rounding; hand the
	# surplus back a cell at a time, like reflow_fit_columns' leftover pass, and
	# only to the labels this pass actually cut.
	surplus=$((saved - overshoot))
	for i in "${clipped[@]}"; do
		((surplus > 0)) || break
		rests[i]=$((rests[i] + 1))
		surplus=$((surplus - 1))
	done

	REPLY_RESTS="${rests[*]}"
	REPLY_OK=1
}

# reflow_pick_layout FLOORS WANTS_LONG WANTS_SHORT TOTAL_LONG TOTAL_SHORT
#                     TOTAL AVAILABLE ZOOM_EXTRA OVERHEAD SEP_WIDTH
#                     MAX_WIN_LINES LONG_TRUNC_FLOOR [RESTS_LONG SINGLE_CLIP_FLOOR]
#                     [PRS MAX_REST]
#
# WANTS_* are uncapped and PRS/MAX_REST are passed straight through to
# reflow_fit_columns, which owns the cap and the PR column; see its header.
#
# Detail ladder: long on one row -> long on one row with the widest labels
# clipped (rung 1.5) -> long grid with every column at its full want -> long
# grid with starved columns (rung 2.5) -> short (compact id) on one row ->
# short grid at full want -> short grid packed to fit. Keeping the branch
# clipped beats dropping it for a bare id; a compact line still beats illegible
# slivers, so it is the deeper rung.
#
# Each grid rung takes the fewest rows that satisfy it, since fewer rows means
# more columns per row and therefore narrower columns.
#
# Sets REPLY_LABELS_MODE (long|short), REPLY_COLWS / REPLY_PR_COLWS
# (space-separated column widths, valid whenever REPLY_NEEDS_MULTILINE=1),
# REPLY_NEEDS_MULTILINE (0|1), REPLY_PER (columns per row) and REPLY_RESTS
# (rung 1.5's per-window rest widths, empty on every other rung).
reflow_pick_layout() {
	local floor_list=$1 want_long_list=$2 want_short_list=$3
	local total_long=$4 total_short=$5 total=$6 available=$7 zoom_extra=$8
	local overhead=$9 sep_width=${10} max_win_lines=${11} long_trunc_floor=${12}
	local rests_long=${13:-} single_clip_floor=${14:-0}
	local pr_list=${15:-} max_rest=${16:-0}

	REPLY_RESTS=""

	if ((total_long + zoom_extra <= available)); then
		REPLY_LABELS_MODE=long
		REPLY_NEEDS_MULTILINE=0
		REPLY_PER=$total
		REPLY_COLWS=""
		REPLY_PR_COLWS=""
		return
	fi

	# Rung 1.5: one row still, with the overshoot shaved off the widest labels.
	# A row is worth more than the tail of a title -- a grid costs a whole
	# screen row AND caps every label at its column, so it shows less text than
	# a clipped single line does. Taken only while the clip leaves
	# SINGLE_CLIP_FLOOR cells of branch/title; a floor of 0 disables the rung.
	if ((single_clip_floor > 0)); then
		reflow_clip_rests "$rests_long" $((total_long + zoom_extra - available)) "$single_clip_floor"
		if ((REPLY_OK)); then
			REPLY_LABELS_MODE=long
			REPLY_NEEDS_MULTILINE=0
			REPLY_PER=$total
			REPLY_COLWS=""
			REPLY_PR_COLWS=""
			return
		fi
		REPLY_RESTS=""
	fi

	REPLY_NEEDS_MULTILINE=1
	# Fewest columns the row cap allows -- the widest a column can ever be, and
	# what both truncating rungs settle on.
	local widest_per=$(((total + max_win_lines - 1) / max_win_lines))
	local rows per

	for ((rows = 1; rows <= max_win_lines; rows++)); do
		per=$(((total + rows - 1) / rows))
		reflow_fit_columns "$per" "$available" "$overhead" "$sep_width" "$floor_list" "$want_long_list" "$pr_list" "$max_rest"
		if ((REPLY_FITS)); then
			REPLY_LABELS_MODE=long
			REPLY_PER=$per
			return
		fi
	done

	# Rung 2.5: long labels with starved columns. Taken only while every starved
	# column still clears LONG_TRUNC_FLOOR (id + ~12 chars of branch); below
	# that the grid is slivers, so fall through to the short ladder.
	reflow_fit_columns "$widest_per" "$available" "$overhead" "$sep_width" "$floor_list" "$want_long_list" "$pr_list" "$max_rest"
	if ((REPLY_MIN_STARVED >= long_trunc_floor)); then
		REPLY_LABELS_MODE=long
		REPLY_PER=$widest_per
		return
	fi

	REPLY_LABELS_MODE=short
	if ((total_short + zoom_extra <= available)); then
		REPLY_NEEDS_MULTILINE=0
		REPLY_PER=$total
		REPLY_COLWS=""
		REPLY_PR_COLWS=""
		return
	fi

	for ((rows = 1; rows <= max_win_lines; rows++)); do
		per=$(((total + rows - 1) / rows))
		reflow_fit_columns "$per" "$available" "$overhead" "$sep_width" "$floor_list" "$want_short_list" "$pr_list" "$max_rest"
		if ((REPLY_FITS)); then
			REPLY_PER=$per
			return
		fi
	done

	# Deepest rung: compact ids packed into the widest columns the row cap
	# allows, starved and clipped as needed.
	reflow_fit_columns "$widest_per" "$available" "$overhead" "$sep_width" "$floor_list" "$want_short_list" "$pr_list" "$max_rest"
	REPLY_PER=$widest_per
}
