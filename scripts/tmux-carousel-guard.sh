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
# it is cleared only when the carousel reaches MIN_COLS, in the same command
# list as the resize, because the same action can squeeze a restored carousel
# back to the identical geometry and a lingering marker would then suppress it.
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
	# The window can change between the gate and this backgrounded run.
	[[ $zoomed == 1 || $bridge == 1 || $grid == 1 ]] && exit 0
	wwidth=$ww
	[[ $floating == 1 ]] && continue
	left[$pane]=$pl
	top[$pane]=$pt
	width[$pane]=$pw
	height[$pane]=$ph
	[[ -n $src ]] && ((pw < MIN_COLS)) && carousels+=("$pane|$src")
done < <(tmux list-panes -t "$target" -F '#{window_width}|#{window_zoomed_flag}|#{?@bridge_win,1,}|#{@crew_grid}|#{pane_id}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{@claude_img_src}' 2>/dev/null)

for entry in "${carousels[@]}"; do
	c=${entry%%|*}
	src=${entry#*|}
	host="%${src##*-}"
	space=$wwidth
	# The pair the carousel splits with its host, so a third pane beside the
	# host is not crushed to make room.
	if [[ -n ${width[$host]:-} ]]; then
		hl=${left[$host]} ht=${top[$host]} hw=${width[$host]} hh=${height[$host]}
		cl=${left[$c]} ct=${top[$c]} cw=${width[$c]} ch=${height[$c]}
		if ((hl + hw + 1 == cl || cl + cw + 1 == hl)) && ((ht < ct + ch && ct < ht + hh)); then
			space=$((hw + 1 + cw))
		fi
	fi
	((space < MIN_COLS + 1 + MAIN_MIN_COLS)) && continue
	tw=$((space * SHARE_PCT / 100))
	((tw < MIN_COLS)) && tw=$MIN_COLS
	tmux resize-pane -t "$c" -x "$tw" \; \
		if -F -t "$c" "#{e|>=|:#{pane_width},$MIN_COLS}" "set -wu -t $target @carousel_guard_sig"
done
