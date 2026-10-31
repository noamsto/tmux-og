#!/usr/bin/env bash
# Grow a tiled image-carousel pane back to a usable width after a layout
# change squeezed it (#1056).
#
# tmux keeps a pane at its minimum width through every later resize, reattach
# and remux restore: a 1-column carousel stays 1 column until something grows
# it explicitly. The carousel viewer is identified by @claude_img_src
# (<srv>-<host pane number>), stamped by its launcher before the viewer runs.
#   args: <target-window>   (the layout-change/resize hook passes #{window_id})
# The hook gate writes @carousel_guard_sig before forking this run, so events
# at an unchanged geometry skip. The marker means "tried and could not fix":
# it is cleared on a successful restore, in the same command list as the
# resize, because the same action can squeeze a restored carousel back to the
# identical geometry and a lingering marker would then suppress it. It is also
# cleared when the window is or became zoomed; a mirror or grid window is
# never written.
set -uo pipefail

# The hook gate's `20` and `61` (MIN_COLS + 1 + MAIN_MIN_COLS) are literals.
MIN_COLS=20
MAIN_MIN_COLS=40
SHARE_PCT=40

target=${1:-}
[[ -z $target ]] && exit 0

declare -A left top width height
carousels=()
wwidth=0
while IFS='|' read -r ww zoomed bridge grid pane floating pl pt pw ph src; do
	# The window can change between the gate and this backgrounded run. Only a
	# zoom needs the marker cleared, so an unzoom back to this same geometry is
	# restored instead of suppressed; a mirror window is daemon-owned and never
	# written, and the gate writes no marker in a mirror or grid window.
	if [[ $zoomed == 1 || $bridge == 1 || $grid == 1 ]]; then
		[[ $zoomed == 1 && $bridge != 1 ]] && tmux set -wu -t "$target" @carousel_guard_sig 2>/dev/null
		exit 0
	fi
	wwidth=$ww
	[[ $floating == 1 ]] && continue
	left[$pane]=$pl
	top[$pane]=$pt
	width[$pane]=$pw
	height[$pane]=$ph
	[[ -n $src ]] && ((pw < MIN_COLS)) && carousels+=("$pane|$src")
done < <(tmux list-panes -t "$target" -F '#{window_width}|#{window_zoomed_flag}|#{?@bridge_win,1,}|#{@crew_grid}|#{pane_id}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{@claude_img_src}' 2>/dev/null)

# Re-checked inside the apply list: resize-pane unzooms a zoomed window, and a
# zoom, mirror or grid flag can land between the read above and the resize.
# Only a zoom clears the marker (see above); a mirror window is never written.
still_tiled='#{&&:#{&&:#{!:#{window_zoomed_flag}},#{!:#{@bridge_win}}},#{!=:#{@crew_grid},1}}'
clear_marker="set -wu -t $target @carousel_guard_sig"
clear_if_zoomed="if -F -t $target '#{&&:#{window_zoomed_flag},#{!:#{@bridge_win}}}' '$clear_marker'"

# resize-pane -x adjusts the pane's cell in its nearest left-right row and
# takes the columns from the next cell, or from the previous one when the cell
# is last in that row. Geometry cannot tell which pane of a nested row is
# last, so only two moves are safe:
#   - a carousel at the window's right edge is last at every level, so
#     resizing it moves its left border and the columns come from its left;
#   - a host directly left of the carousel with the same rows ends a cell
#     whose next cell holds the carousel, so shrinking the host moves their
#     shared border.
# Any other carousel is declined, because resizing it would take the columns
# from the pane on its right; the gate's marker stops further forks here.
for entry in "${carousels[@]}"; do
	c=${entry%%|*}
	src=${entry#*|}
	host="%${src##*-}"
	cl=${left[$c]} ct=${top[$c]} cw=${width[$c]} ch=${height[$c]}
	paired=0
	if [[ -n ${width[$host]:-} ]]; then
		hw=${width[$host]}
		((left[$host] + hw + 1 == cl && top[$host] == ct && height[$host] == ch)) && paired=1
	fi
	if ((cl + cw == wwidth)); then
		resized=$c
		space=$wwidth
		((paired)) && space=$((hw + 1 + cw))
	elif ((paired)); then
		resized=$host
		space=$((hw + 1 + cw))
	else
		continue
	fi
	((space < MIN_COLS + 1 + MAIN_MIN_COLS)) && continue
	tw=$((space * SHARE_PCT / 100))
	((tw < MIN_COLS)) && tw=$MIN_COLS
	cols=$tw
	[[ $resized == "$host" ]] && cols=$((space - 1 - tw))
	# One carousel per run: the resize changes the geometry, so the gate fires
	# again and the next run handles any other squeezed carousel against fresh
	# widths rather than the ones read above.
	tmux if -F -t "$c" "$still_tiled" \
		"resize-pane -t $resized -x $cols ; if -F -t $c '#{e|>=|:#{pane_width},$MIN_COLS}' '$clear_marker'" \
		"$clear_if_zoomed" 2>/dev/null
	exit 0
done
exit 0
