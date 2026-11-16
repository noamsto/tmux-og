#!/usr/bin/env bash
# Open `crew dash` in a labelled floating pane, or bring the open one forward (#1139).
#
#   tmux-crew-dash.sh [--detached] [pane-id]
#
# The window is the pane's window; pane-id falls back to $TMUX_PANE. A bind's
# run-shell sets no TMUX_PANE and a bare display-message resolves tmux's "best"
# client, so the bind passes #{q:pane_id} explicitly; a dispatcher calls this from
# a worker pane, where $TMUX_PANE is set. --detached leaves focus where it is.
#
# One dash per window: a float labelled crew-dash is focused (a plain list-panes
# scan, last match wins), a crew-group copy hidden by `tmux-float-group hide crew`
# is restored, else a new one is created. The build substitutes the sibling's
# store path into FLOAT_GROUP (a pane shell's PATH may not carry the scripts);
# OG_FLOAT_GROUP_BIN is the test seam, and the raw placeholder is not executable.
#
# The new-pane flags and the @float_geom stamp below are separate literals nothing
# compares: change both together.
set -uo pipefail

STASH=scratch-og-float-stash
LABEL=crew-dash
GROUP=crew
FLOAT_GROUP="${OG_FLOAT_GROUP_BIN:-@float_group@}"

detached=0
pane=""
for arg in "$@"; do
	case $arg in
	--detached) detached=1 ;;
	%*) pane=$arg ;;
	esac
done
pane=${pane:-${TMUX_PANE:-}}
[[ $pane == %* ]] || exit 0

notify() { tmux display-message "tmux-crew-dash: $*"; }

# The path comes last so a '|' inside it stays in the final field.
IFS='|' read -r win bridge_win cwd <<<"$(tmux display-message -p -t "$pane" '#{window_id}|#{@bridge_win}|#{pane_current_path}' 2>/dev/null)"
[[ $win == @* ]] || exit 0

focus() { ((detached)) || tmux select-pane -t "$1"; }

id=""
while IFS='|' read -r pid floating label; do
	[[ $floating == 1 && $label == "$LABEL" ]] && id=$pid
done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}')
if [[ -n $id ]]; then
	focus "$id"
	exit 0
fi

# The group is checked too: `show crew` restores only crew-group panes.
stashed=0
while IFS='|' read -r pid label home grp; do
	[[ $label == "$LABEL" && $home == "$win" && $grp == "$GROUP" ]] && stashed=1
done < <(tmux list-panes -s -t "$STASH" -F '#{pane_id}|#{@pane_label}|#{@float_group_home}|#{@float_group}' 2>/dev/null)
if ((stashed)); then
	[[ -x $FLOAT_GROUP ]] || {
		notify "tmux-float-group is not installed"
		exit 0
	}
	args=(show "$GROUP" "$pane")
	((detached)) && args+=(--no-focus)
	exec "$FLOAT_GROUP" "${args[@]}"
fi

command -v crew >/dev/null || {
	notify "crew is not on PATH"
	exit 0
}

# -A is a z-order flag (the float stays above a zoomed pane), absent on older servers.
args=(new-pane -t "$win" -x 90% -y 90% -X 5% -Y 5% -B heavy -P -F '#{pane_id}')
[[ $(tmux list-commands new-pane 2>/dev/null) =~ \[-[a-zA-Z]*A[a-zA-Z]*\] ]] && args+=(-A)
((detached)) && args+=(-d)
# In a mirror window pane_current_path is the daemon renderer's cwd, not the user's.
# -c is format-expanded, so a '#' in the path is doubled.
[[ -n $bridge_win || -z $cwd ]] || args+=(-c "${cwd//#/##}")

id="$(tmux "${args[@]}" 'exec crew dash' 2>/dev/null)"
[[ $id == %* ]] || {
	notify "could not open a floating pane"
	exit 0
}

# Same stamp the float binds write, so tmux-float-refit keeps the dash responsive.
tmux set-option -p -t "$id" @float_geom '90% 90% 5% 5%' \; \
	set-option -p -t "$id" remain-on-exit off \; \
	set-option -p -t "$id" @pane_label "$LABEL" \; \
	set-option -p -t "$id" @float_group "$GROUP"
exit 0
