#!/usr/bin/env bash
# Resize a floating pane from the keyboard, and keep a hand-resized float's
# size across later window resizes.
#
#   tmux-float-nudge.sh <pane-id> <L|R|U|D> [step]   nudge by step (default 5)
#   tmux-float-nudge.sh <pane-id> stamp              rewrite @float_geom only
#
# Upstream resize-pane on a float only ever GROWS it: -L/-U flip which edge
# moves, never the sign, and nothing clamps — holding M-Left walks a float off
# the left of the window. It also keeps no memory of @float_geom, so a float
# tmux-float-refit refits is always the creation percentages: a hand resize that
# does not rewrite the stamp is reverted by the next window-resized.
#
# So this script owns both halves. `dir` resizes by step inside the window —
# grow toward the pressed direction while step cells of room remain, else shrink
# from that edge (the edge-anchored rule), else no-op — and the stamp rewrites
# @float_geom preserving each field's own unit (percentage stays a percentage,
# cell stays a cell), then marks the float current for the window size so a
# refit at the same size is a no-op and the user size survives exactly.
set -uo pipefail

pane=${1:-}
mode=${2:-}
step=${3:-5}

# A stray drag end (the table's stash already cleared) must be a silent no-op.
[[ $pane == %* ]] || exit 0
[[ $mode == L || $mode == R || $mode == U || $mode == D || $mode == stamp ]] || exit 0
[[ $step =~ ^[0-9]+$ ]] || exit 0

# Unstamped floats are the user's geometry, not ours: upstream's own clamp keeps
# them on screen, and there is no percentage to reapply either way.
read -r floating geom <<<"$(tmux display-message -p -t "$pane" '#{pane_floating_flag} #{@float_geom}' 2>/dev/null)"
[[ $floating == 1 && -n $geom ]] || exit 0

if [[ $mode != stamp ]]; then
	read -r win_w win_h width height left top <<<"$(
		tmux display-message -p -t "$pane" \
			'#{window_width} #{window_height} #{pane_width} #{pane_height} #{pane_left} #{pane_top}' 2>/dev/null
	)"
	# The refit speaks outer cells (inner size = resolved-2, inner offset =
	# resolved+1, measured), so the outer box is (left-1)+(width+2) by
	# (top-1)+(height+2); room is what is left of the window on that side.
	case $mode in
	L) room=$((left - 1)) ;;
	R) room=$((win_w - (left - 1) - (width + 2))) ;;
	U) room=$((top - 1)) ;;
	D) room=$((win_h - (top - 1) - (height + 2))) ;;
	esac
	case $mode in
	L | R) fits=$((width > step)) ;;
	U | D) fits=$((height > step)) ;;
	esac
	if ((room >= step)); then
		tmux resize-pane -t "$pane" -"$mode" "$step" 2>/dev/null
	elif ((fits)); then
		tmux resize-pane -t "$pane" -"$mode" "-$step" 2>/dev/null
	fi
fi

read -r win_w win_h width height left top <<<"$(
	tmux display-message -p -t "$pane" \
		'#{window_width} #{window_height} #{pane_width} #{pane_height} #{pane_left} #{pane_top}' 2>/dev/null
)"
[[ -n $win_w ]] || exit 0

# field <old> <resolved> <window-size>: keep the old field's unit. A field that
# carried '%' becomes its percentage of the window (tmux truncates the same way,
# args_string_percentage's (curval * pct) / 100); a cell field stays cells. A
# negative resolved offset cannot be a percentage (strtonum rejects it) and
# falls back to cells.
field() {
	local old=$1 resolved=$2 size=$3
	if [[ $old == *%* && $resolved -ge 0 ]]; then
		printf '%d%%' $((resolved * 100 / size))
	else
		printf '%d' "$resolved"
	fi
}
read -r old_w old_h old_x old_y <<<"$geom"
new_w=$(field "$old_w" $((width + 2)) "$win_w")
new_h=$(field "$old_h" $((height + 2)) "$win_h")
new_x=$(field "$old_x" $((left - 1)) "$win_w")
new_y=$(field "$old_y" $((top - 1)) "$win_h")

tmux set-option -p -t "$pane" @float_geom "$new_w $new_h $new_x $new_y"
tmux set-option -p -t "$pane" @float_refit_size "${win_w}x${win_h}"
