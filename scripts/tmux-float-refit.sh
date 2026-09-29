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

# The window-resized hook sets @float_refit_pending before forking this run,
# so the rest of a resize burst skips while it is on its way. Clearing it in
# the same command list as this read — the script's first tmux call — means
# any event it suppressed predates the read.
while IFS='|' read -r pane geom wsize fsize; do
	read -r width height xoff yoff <<<"$geom"
	# Floats created outside the binds (a mouse Ctrl-drag) carry no stamp, so
	# there is no percentage to reapply — and none is needed: upstream's own
	# clamp (which has no notion of @float_geom) already keeps them fully
	# inside the window on every resize. Touching one here would mean
	# inventing a percentage the user never chose and overwriting their
	# hand-placed geometry with it.
	[[ -n $yoff ]] || continue
	# The hook gate forks when ANY stamped float is stale, so a float already
	# stamped for this window's size is skipped per pane: re-applying its
	# @float_geom would only re-round the percentage against the same window
	# and could shift it a cell, and skipping is exactly how a hand resize
	# (tmux-float-nudge) keeps the user's size. Per pane, so a float opened
	# later (no stamp) always refits once.
	[[ $fsize == "$wsize" ]] && continue
	# Stamped before the refit so a resize landing mid-refit forks its own run.
	tmux set-option -p -t "$pane" @float_refit_size "$wsize"
	tmux resize-pane -t "$pane" -x "$width" -y "$height"
	tmux move-pane -t "$pane" -X "$xoff" -Y "$yoff"
done < <(tmux list-panes -t "$target" -f '#{pane_floating_flag}' \
	-F '#{pane_id}|#{@float_geom}|#{window_width}x#{window_height}|#{@float_refit_size}' \; set-option -wu -t "$target" @float_refit_pending 2>/dev/null)
