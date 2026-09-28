#!/usr/bin/env bash
# Smart tmux↔kitty navigation. At a tmux edge inside kitty, hand focus to the
# neighbouring kitty window; otherwise move within tmux. When the window has
# floats, resolve the nearest geometrically directional pane ourselves: tmux's
# select-pane only understands the tiled layout.
# args: <flag> <kitty-dir> <zoomed> <at-edge> <origin-floating> <has-floats> <pane-id> <window-id>
# no set -e: a failed query or kitty command falls through to select-pane.
set -u
flag=$1 dir=$2 zoomed=$3 edge=$4 origin_floating=$5 has_floats=$6 origin_id=$7 window_id=$8

[ "$zoomed" = 1 ] && exit 0

candidate=
geometry_ok=1
if [ "$has_floats" = 1 ]; then
	# One snapshot keeps the origin and candidates on the same layout revision.
	if geometry=$(tmux list-panes -t "$window_id" -F '#{pane_id}|#{pane_floating_flag}|#{pane_modal_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}' 2>/dev/null); then
		:
	else
		geometry_ok=0
		geometry=
	fi
	origin=
	while IFS='|' read -r pane_id pane_floating pane_modal pane_left pane_top pane_width pane_height; do
		[ "$pane_id" = "$origin_id" ] && {
			origin="$pane_left $pane_top $pane_width $pane_height"
			break
		}
	done <<<"$geometry"

	if [ -n "$origin" ]; then
		read -r origin_left origin_top origin_width origin_height <<<"$origin"
		origin_right=$((origin_left + origin_width))
		origin_bottom=$((origin_top + origin_height))
		origin_center_x=$((origin_left * 2 + origin_width))
		origin_center_y=$((origin_top * 2 + origin_height))
		best_score=
		while IFS='|' read -r pane_id pane_floating pane_modal pane_left pane_top pane_width pane_height; do
			[ "$pane_id" = "$origin_id" ] && continue
			[ "$pane_modal" = 1 ] && continue
			pane_right=$((pane_left + pane_width))
			pane_bottom=$((pane_top + pane_height))
			pane_center_x=$((pane_left * 2 + pane_width))
			pane_center_y=$((pane_top * 2 + pane_height))
			case "$flag" in
			L)
				[ "$pane_center_x" -lt "$origin_center_x" ] || continue
				gap=$((origin_left - pane_right))
				[ "$gap" -lt 0 ] && gap=0
				perpendicular_start=$pane_top
				perpendicular_end=$pane_bottom
				origin_perpendicular_start=$origin_top
				origin_perpendicular_end=$origin_bottom
				;;
			R)
				[ "$pane_center_x" -gt "$origin_center_x" ] || continue
				gap=$((pane_left - origin_right))
				[ "$gap" -lt 0 ] && gap=0
				perpendicular_start=$pane_top
				perpendicular_end=$pane_bottom
				origin_perpendicular_start=$origin_top
				origin_perpendicular_end=$origin_bottom
				;;
			U)
				[ "$pane_center_y" -lt "$origin_center_y" ] || continue
				gap=$((origin_top - pane_bottom))
				[ "$gap" -lt 0 ] && gap=0
				perpendicular_start=$pane_left
				perpendicular_end=$pane_right
				origin_perpendicular_start=$origin_left
				origin_perpendicular_end=$origin_right
				;;
			D)
				[ "$pane_center_y" -gt "$origin_center_y" ] || continue
				gap=$((pane_top - origin_bottom))
				[ "$gap" -lt 0 ] && gap=0
				perpendicular_start=$pane_left
				perpendicular_end=$pane_right
				origin_perpendicular_start=$origin_left
				origin_perpendicular_end=$origin_right
				;;
			esac
			if [ "$perpendicular_start" -lt "$origin_perpendicular_end" ] && [ "$perpendicular_end" -gt "$origin_perpendicular_start" ]; then
				overlaps=0 perpendicular_gap=0
			elif [ "$perpendicular_start" -ge "$origin_perpendicular_end" ]; then
				overlaps=1 perpendicular_gap=$((perpendicular_start - origin_perpendicular_end))
			else
				overlaps=1 perpendicular_gap=$((origin_perpendicular_start - perpendicular_end))
			fi
			# A tiled origin enters a float first; a floating origin exits to tile.
			if [ "$origin_floating" = "$pane_floating" ]; then preference=1; else preference=0; fi
			pane_number=${pane_id#%}
			better=0
			if [ -z "$best_score" ] || [ "$overlaps" -lt "$best_overlaps" ]; then
				better=1
			elif [ "$overlaps" -eq "$best_overlaps" ] && [ "$perpendicular_gap" -lt "$best_perpendicular_gap" ]; then
				better=1
			elif [ "$overlaps" -eq "$best_overlaps" ] && [ "$perpendicular_gap" -eq "$best_perpendicular_gap" ] && [ "$gap" -lt "$best_gap" ]; then
				better=1
			elif [ "$overlaps" -eq "$best_overlaps" ] && [ "$perpendicular_gap" -eq "$best_perpendicular_gap" ] && [ "$gap" -eq "$best_gap" ] && [ "$preference" -lt "$best_preference" ]; then
				better=1
			elif [ "$overlaps" -eq "$best_overlaps" ] && [ "$perpendicular_gap" -eq "$best_perpendicular_gap" ] && [ "$gap" -eq "$best_gap" ] && [ "$preference" -eq "$best_preference" ] && [ "$pane_number" -lt "$best_pane_number" ]; then
				better=1
			fi
			if [ "$better" = 1 ]; then
				best_score=1 best_overlaps=$overlaps best_perpendicular_gap=$perpendicular_gap best_gap=$gap best_preference=$preference best_pane_number=$pane_number candidate=$pane_id
			fi
		done <<<"$geometry"
	fi
fi

[ -n "$candidate" ] && {
	tmux select-pane -t "$candidate"
	exit 0
}
if [ "$geometry_ok" = 1 ] && [ "$edge" = 1 ] && [ -n "${KITTY_LISTEN_ON:-}" ] && command -v kitty >/dev/null 2>&1; then
	# timeout so a stalled kitty remote-control socket can't hang the C-hjkl bind.
	timeout 1 kitty @ action neighboring_window "$dir" 2>/dev/null && exit 0
fi
tmux select-pane -"$flag"
