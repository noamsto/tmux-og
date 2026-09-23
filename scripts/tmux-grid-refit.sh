#!/usr/bin/env bash
# Lay a dispatcher "grid" window out responsively, keeping @crew_role=lead as
# the main pane. tmux-og owns the layout policy; the dispatcher owns every hint
# this reads (window @crew_grid / @crew_grid_main_pct, pane @crew_role) and calls
# this after adding or removing a role pane. Nothing here ever writes a @crew_*
# option.
#   args: <target-window>   (the window-resized hook passes #{window_id})
# No-op unless the window carries @crew_grid=1, so a window that is not a crew
# grid — and any non-tmux-og server — is untouched. Silent, cheap and
# convergent: @grid_refit_sig caches the last applied decision, so an unchanged
# grid costs no tmux command and emits no reflow notification.
set -uo pipefail

target=${1:-}
[[ -z $target ]] && exit 0

# read_opt <option> <default> -> stdout: the window option's value, or <default>
# when it is unset (show-options prints "invalid option" to stderr and exits 1).
read_opt() {
	local val
	val=$(tmux show-options -w -v -t "$target" "$1" 2>/dev/null) || val=""
	[[ -n $val ]] && printf '%s' "$val" || printf '%s' "$2"
}

[[ "$(read_opt @crew_grid 0)" == 1 ]] || exit 0

# Zoom is user state: a zoomed grid is left exactly as the user left it.
[[ "$(tmux display-message -p -t "$target" '#{window_zoomed_flag}' 2>/dev/null)" == 1 ]] && exit 0

# The lead pane is the main pane. No lead -> not a grid we can lay out.
lead=$(tmux list-panes -t "$target" -f '#{==:#{@crew_role},lead}' -F '#{pane_id}' 2>/dev/null | head -1)
[[ -n $lead ]] || exit 0

pane_ids=$(tmux list-panes -t "$target" -F '#{pane_id}' 2>/dev/null)
np=$(printf '%s\n' "$pane_ids" | grep -c .) || true
((np > 1)) || exit 0

read -r w h <<<"$(tmux display-message -p -t "$target" '#{window_width} #{window_height}' 2>/dev/null)"
[[ $w =~ ^[0-9]+$ && $h =~ ^[0-9]+$ ]] || exit 0

# Lead's share of the window; out-of-range/non-integer falls back to 60.
pct=$(read_opt @crew_grid_main_pct 60)
[[ $pct =~ ^[0-9]+$ ]] && ((pct >= 1 && pct <= 99)) || pct=60
# Minimum role-area width below which the roles get too narrow a column.
min_cols=$(read_opt @grid_refit_min_role_cols 30)
[[ $min_cols =~ ^[0-9]+$ ]] || min_cols=30
# Cells are ~2:1 tall, so main-vertical needs this much width per unit of height.
aspect=$(read_opt @grid_refit_aspect 2)
[[ $aspect =~ ^[0-9]+$ ]] && ((aspect >= 1)) || aspect=2

role_w=$((w - w * pct / 100))
if ((w >= aspect * h && role_w >= min_cols)); then
	layout=main-vertical
	opt=main-pane-width
else
	layout=main-horizontal
	opt=main-pane-height
fi

sig="$layout:$pct:$np:${w}x${h}:$min_cols:$aspect"
[[ "$(read_opt @grid_refit_sig '')" == "$sig" ]] && exit 0

# Claim before mutating: window-resized is backgrounded, so concurrent refits
# can be in flight during a drag, and the swap below is the one non-idempotent
# step. The loser of the claim exits above instead of swapping a second time.
tmux set-option -w -t "$target" @grid_refit_sig "$sig" 2>/dev/null || true

first=$(printf '%s\n' "$pane_ids" | head -1)
[[ $lead == "$first" ]] || tmux swap-pane -d -s "$lead" -t "$first" 2>/dev/null || true

tmux set-window-option -t "$target" "$opt" "${pct}%" 2>/dev/null || true
tmux select-layout -t "$target" "$layout" 2>/dev/null || true
exit 0
