#!/usr/bin/env bash
# Hide and show a named group of floating panes together (#1139).
#
#   tmux-float-group.sh <hide|show|toggle> <group> [pane-id] [--no-focus]
#   tmux-float-group.sh tag <group> [pane-id]
#
# The window is the pane's window; pane-id falls back to $TMUX_PANE. A bind's
# run-shell sets no TMUX_PANE and a bare display-message resolves tmux's "best"
# client, so a bind passes #{q:pane_id} explicitly.
#
# The pin has no native hide-pane, so a hidden float is parked as a tile in its
# own window of the session scratch-og-float-stash (scratch-* is the hidden-helper
# convention reflow, status-fold and the pickers already skip) and floated back
# into its home window on show. Pane options travel with the pane, so the
# per-pane state needs no side file:
#   @float_group       the group tag (set by `tag` or at creation)
#   @float_group_home  window id the float is hidden from
#   @float_group_cells W H X Y live cells at hide time, the fallback when the
#                      float carries no @float_geom
# One stash window per hidden pane keeps a pile of hidden floats from hitting
# "pane too small". A float carrying @bridge_pane is daemon-owned and is never
# touched; a @bridge_win window is refused outright, because restoring would tile
# a local pane into a daemon-owned window.
set -uo pipefail

STASH=scratch-og-float-stash

action=${1:-}
group=${2:-}
pane=""
focus=1
for arg in "${@:3}"; do
	case $arg in
	--no-focus) focus=0 ;;
	%*) pane=$arg ;;
	esac
done
pane=${pane:-${TMUX_PANE:-}}

case $action in
hide | show | toggle | tag) ;;
*) exit 0 ;;
esac
[[ -n $group && $pane == %* ]] || exit 0

notify() { tmux display-message "tmux-float-group: $*"; }

IFS='|' read -r win bridge_win <<<"$(tmux display-message -p -t "$pane" '#{window_id}|#{@bridge_win}' 2>/dev/null)"
[[ $win == @* ]] || exit 0

if [[ $action == tag ]]; then
	[[ $(tmux display-message -p -t "$pane" '#{pane_floating_flag}') == 1 ]] || {
		notify "not a floating pane"
		exit 0
	}
	tmux set-option -p -t "$pane" @float_group "$group"
	exit 0
fi

[[ $(tmux list-commands break-pane 2>/dev/null) =~ \[-[a-zA-Z]*W[a-zA-Z]*\] ]] || {
	notify "this tmux has no break-pane -W (floating panes)"
	exit 0
}
if [[ -n $bridge_win ]]; then
	notify "float groups do not act on a remote mirror window"
	exit 0
fi

# tagged: the visible, local, tagged floats of the window, as "id|W|H|X|Y" in
# the outer-cell form break-pane takes (inner size +2, inner offset -1).
tagged() {
	local id floating grp bridge w h l t
	TAGGED=()
	while IFS='|' read -r id floating grp bridge w h l t; do
		[[ $floating == 1 && $grp == "$group" && -z $bridge ]] || continue
		TAGGED+=("$id|$((w + 2))|$((h + 2))|$((l - 1))|$((t - 1))")
	done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@float_group}|#{@bridge_pane}|#{pane_width}|#{pane_height}|#{pane_left}|#{pane_top}')
}

stashed() {
	tmux list-panes -s -t "$STASH" -F '#{pane_id}|#{@float_group}|#{@float_group_home}|#{@float_geom}|#{@float_group_cells}' 2>/dev/null
}

# A stashed pane whose home window is gone could never be restored; its stash
# window disappears with it.
reap_orphans() {
	local wins live id grp home rest
	# A failed list-windows must not read as "every home window is gone".
	wins=$(tmux list-windows -a -F '#{window_id}') || return 0
	live="|${wins//$'\n'/|}|"
	while IFS='|' read -r id grp home rest; do
		[[ -n $home && $live != *"|$home|"* ]] && tmux kill-pane -t "$id"
	done < <(stashed)
}

# zoomed_pane: the window's zoomed tile in ZOOMED (empty when not zoomed). Not the
# active pane: a focused float sits above a zoomed tile, and select-pane -Z keeps
# the zoom while moving focus to it.
zoomed_pane() {
	ZOOMED=""
	read -r ZOOMED < <(tmux list-panes -t "$win" -f '#{pane_zoomed_flag}' -F '#{pane_id}')
}

# rezoom <pane>: zoom the tile again if it is still a tile of this window.
rezoom() {
	[[ -n $1 ]] || return 0
	[[ $(tmux display-message -p -t "$1" '#{window_id}|#{pane_floating_flag}|#{window_zoomed_flag}' 2>/dev/null) == "$win|0|0" ]] || return 0
	tmux resize-pane -Z -t "$1"
}

# dwindle_pause / dwindle_resume: the dwindle hook runs on every layout change in
# a window with @dwindle on and evens the tiles when the tiled count dropped, which
# is what restore's join + break-pane -W looks like. @dwindle is switched off for
# the round trip, then put back as it was (window-local value, else inherited)
# together with @dwindle_cache, which the hook drops while the mode is off.
dwindle_pause() {
	DW_PAUSED=0
	[[ $(tmux display-message -p -t "$win" '#{@dwindle}') == on ]] || return 0
	DW_LOCAL=$(tmux show-options -wqv -t "$win" @dwindle)
	DW_CACHE=$(tmux show-options -wqv -t "$win" @dwindle_cache)
	tmux set-option -w -t "$win" @dwindle off
	DW_PAUSED=1
}

dwindle_resume() {
	((DW_PAUSED)) || return 0
	if [[ -n $DW_LOCAL ]]; then
		tmux set-option -w -t "$win" @dwindle "$DW_LOCAL"
	else
		tmux set-option -wu -t "$win" @dwindle
	fi
	[[ -z $DW_CACHE ]] || tmux set-option -w -t "$win" @dwindle_cache "$DW_CACHE"
}

# stash_move <pane>: park the float as the only pane of a fresh stash window.
# The bootstrap pane exists because join-pane needs a target, and goes once the
# float is in; the stash session itself ends with its last window.
stash_move() {
	local boot
	boot="$(tmux new-window -d -t "$STASH:" -P -F '#{pane_id}' sleep 3600 2>/dev/null)" ||
		boot="$(tmux new-session -d -s "$STASH" -P -F '#{pane_id}' sleep 3600 2>/dev/null)" ||
		return 1
	[[ $boot == %* ]] || return 1
	if tmux join-pane -d -s "$1" -t "$boot" 2>/dev/null; then
		tmux kill-pane -t "$boot"
		return 0
	fi
	tmux kill-pane -t "$boot"
	return 1
}

hide() {
	local entry id w h x y active
	tagged
	((${#TAGGED[@]})) || return 0
	zoomed_pane
	active="$(tmux display-message -p -t "$win" '#{pane_id}')"
	for entry in "${TAGGED[@]}"; do
		IFS='|' read -r id w h x y <<<"$entry"
		tmux set-option -p -t "$id" @float_group_home "$win"
		tmux set-option -p -t "$id" @float_group_cells "$w $h $x $y"
		stash_move "$id" && continue
		tmux set-option -p -u -t "$id" @float_group_home
		tmux set-option -p -u -t "$id" @float_group_cells
		notify "could not hide $id; left visible"
	done
	[[ -n $ZOOMED ]] || return 0
	rezoom "$ZOOMED" && tmux select-pane -Z -t "$active" 2>/dev/null
	return 0
}

# tile_target: the tile a restored float is joined before. A closing cell hands
# its space to its next sibling, so splitting a tile with -b (new cell first) and
# floating the pane back out gives the space back to that tile and the tiled
# layout is unchanged; -f, or a join after the tile, hands it to a neighbour.
tile_target() {
	local id active
	TILE=""
	while IFS='|' read -r active id; do
		[[ -z $TILE || $active == 1 ]] && TILE=$id
	done < <(tmux list-panes -t "$win" -f '#{!:#{pane_floating_flag}}' -F '#{pane_active}|#{pane_id}')
}

restore() {
	local id=$1 geom=$2 cells=$3 w h x y
	tile_target
	if [[ -z $TILE ]] || ! tmux join-pane -d -b -s "$id" -t "$TILE" 2>/dev/null; then
		tmux join-pane -d -f -s "$id" -t "$win" 2>/dev/null || return 1
	fi
	tmux set-option -p -u -t "$id" @float_group_home
	tmux set-option -p -u -t "$id" @float_group_cells
	tmux set-option -p -t "$id" remain-on-exit off
	read -r w h x y <<<"$geom"
	if [[ -z $geom ]] || ! tmux break-pane -W -s "$id" -x "$w" -y "$h" -X "$x" -Y "$y" 2>/dev/null; then
		read -r w h x y <<<"$cells"
		tmux break-pane -W -s "$id" -x "$w" -y "$h" -X "$x" -Y "$y" 2>/dev/null
	fi
}

show() {
	local entries=() line id grp home geom cells last="" active zoomed
	while IFS='|' read -r id grp home geom cells; do
		[[ $grp == "$group" && $home == "$win" ]] && entries+=("$id|$geom|$cells")
	done < <(stashed)
	((${#entries[@]})) || return 0
	active="$(tmux display-message -p -t "$win" '#{pane_id}')"
	zoomed_pane
	zoomed=$ZOOMED
	dwindle_pause
	for line in "${entries[@]}"; do
		IFS='|' read -r id geom cells <<<"$line"
		if restore "$id" "$geom" "$cells"; then
			last=$id
		else
			notify "could not restore $id"
		fi
	done
	dwindle_resume
	rezoom "$zoomed"
	if ((focus)); then
		[[ -n $last ]] && tmux select-pane -Z -t "$last"
	else
		tmux select-pane -Z -t "$active" 2>/dev/null
	fi
	return 0
}

reap_orphans
case $action in
hide) hide ;;
show) show ;;
toggle)
	tagged
	if ((${#TAGGED[@]})); then hide; else show; fi
	;;
esac
exit 0
