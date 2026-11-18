#!/usr/bin/env bash
# tmux-status-autofold: fold the status bar while a float covers the view, and
# unfold it again once nothing covers it. Forked by the window-layout-changed /
# session-window-changed gates (only when the float geometry signature changed)
# with the session id, window id and client width.
# @og_status_autofolded marks a fold this script made, so a manual fold (prefix
# + Z, which clears the marker) is never undone here. The fold itself is
# tmux-reflow-windows (--force), the one place that knows what a folded bar is.
# args: <session-id> <window-id> <client-width>
set -u
# shellcheck source=/dev/null  # Nix store path substituted at build time
source @lib_float_cover@

session_id=${1:-}
window_id=${2:-}
width=${3:-}
REFLOW=${OG_AUTOFOLD_REFLOW:-@reflow@}

[[ -n $session_id && -n $window_id ]] || exit 0

IFS='|' read -r win_width win_height folded autofolded bridge_win session_name < <(
	tmux display-message -p -t "$window_id" '#{window_width}|#{window_height}|#{@og_status_folded}|#{@og_status_autofolded}|#{@bridge_win}|#{session_name}'
)
[[ -n $session_name ]] || exit 0
# Scratch sessions run their own hints bar (see tmux-reflow-windows).
case "$session_name" in
scratch-*) exit 0 ;;
esac
[[ -z $bridge_win ]] || exit 0

# Modal floats are transient chrome and bridge floats mirror a remote's: neither
# counts as covering.
covers=0
while IFS='|' read -r floating modal bridge_pane left top pane_width pane_height; do
	[[ $floating == 1 && $modal != 1 && -z $bridge_pane ]] || continue
	float_covers_view "$left" "$top" "$pane_width" "$pane_height" "$win_width" "$win_height"
	if [[ $REPLY == 1 ]]; then
		covers=1
		break
	fi
done < <(tmux list-panes -t "$window_id" -F '#{pane_floating_flag}|#{pane_modal_flag}|#{@bridge_pane}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}')

if [[ $covers == 1 && $folded != 1 ]]; then
	tmux set-option -t "$session_id" @og_status_folded 1
	tmux set-option -t "$session_id" @og_status_autofolded 1
elif [[ $covers == 0 && $autofolded == 1 ]]; then
	tmux set-option -t "$session_id" -u @og_status_autofolded
	tmux set-option -t "$session_id" @og_status_folded 0
else
	exit 0
fi
"$REFLOW" --force "$session_name" "$width" >/dev/null 2>&1
