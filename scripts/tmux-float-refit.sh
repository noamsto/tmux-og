#!/usr/bin/env bash
# Reassert each floating pane's declared geometry after its window resized.
#
# tmux resolves new-pane's -x/-y/-X/-Y percentages once, at creation, into
# absolute cells: a floating cell keeps only {sx,sy,xoff,yoff}, with no memory
# of "90%". Upstream (tmux/tmux#5582, f8fc53b6) now runs
# layout_clamp_floating_panes() on every resize and keeps a float fully inside
# the window — shrinking it to fit a smaller client, or nudging its offset
# back on screen — but that clamp only ever shrinks or repositions the
# existing absolute cell; it never re-derives the creation percentage. So a
# float made at 50% of an 80-column client is still ~40 columns wide after
# growing to 200, because upstream has nothing telling it "50%" — only the
# absolute cell it resolved at creation (clamped or not). @float_geom carries
# that percentage forward for this script to reapply on every resize.
#   args: <target-window>   (the window-resized hook passes #{window_id})
# Both commands re-derive their percentages against the window's current size,
# and each no-ops when the value is unchanged, so this never churns a redraw.
set -uo pipefail

target=${1:-}
[[ -z $target ]] && exit 0

while IFS='|' read -r pane geom; do
	read -r width height xoff yoff <<<"$geom"
	# Floats created outside the binds (a mouse Ctrl-drag) carry no stamp, so
	# there is no percentage to reapply — and none is needed: upstream's own
	# clamp (which has no notion of @float_geom) already keeps them fully
	# inside the window on every resize. Touching one here would mean
	# inventing a percentage the user never chose and overwriting their
	# hand-placed geometry with it.
	[[ -n $yoff ]] || continue
	tmux resize-pane -t "$pane" -x "$width" -y "$height"
	tmux move-pane -t "$pane" -X "$xoff" -Y "$yoff"
done < <(tmux list-panes -t "$target" -f '#{pane_floating_flag}' \
	-F '#{pane_id}|#{@float_geom}' 2>/dev/null)
