#!/usr/bin/env bash
# Open `crew dash` in a labelled floating pane, or bring the open one forward (#1139).
#
#   tmux-crew-dash.sh [--side] [--detached] [pane-id]
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
# In a mirror window (@bridge_win with @bridge_pane and @bridge_sock) the dash runs
# on the REMOTE host: the bridge ctl `tool` verb opens it there and the mirror
# reconciles it. Nothing is created locally, and --detached cannot suppress the
# verb's focus. The build substitutes the ctl's store path into @bridge_ctl@;
# OG_BRIDGE_CTL_BIN is the test seam, and the raw placeholder is not executable.
# A @bridge_win window lacking either option keeps the local path below.
#
# --side (#1185) opens the dash as a tiled split on the right of the pane instead
# (label crew-dash-side, `crew dash --narrow` when this crew lists the flag), for
# a dispatcher that wants it persistent beside the agent. The split takes 32% of
# the window clamped to 50..80 columns and leaves the target at least 80; a window
# too narrow for that, or a @crew_grid window (tmux-grid-refit re-tiles every
# tiled pane there), falls back to the float below with a notification. A float
# and a side dash coexist and neither touches the other. A mirror window sends the
# `crew-dash-side` tool, so the REMOTE side gets the split too; a daemon older than
# that tool answers `unknown tool`, and the script retries once with the `crew-dash`
# float tool and tells the user. @remux_relaunch re-stamps the label and
# re-execs the dash, so a remux restore keeps the pane findable. The carousel guard
# (tmux-carousel-guard) keeps a restore from squeezing it under 50 columns.
#
# The new-pane flags and the @float_geom stamp below are separate literals nothing
# compares: change both together.
set -uo pipefail

STASH=scratch-og-float-stash
LABEL=crew-dash
GROUP=crew
SIDE_LABEL=crew-dash-side
SIDE_MIN=50
SIDE_MAX=80
SIDE_KEEP=80
FLOAT_GROUP="${OG_FLOAT_GROUP_BIN:-@float_group@}"
BRIDGE_CTL="${OG_BRIDGE_CTL_BIN:-@bridge_ctl@}"

detached=0
side=0
pane=""
for arg in "$@"; do
	case $arg in
	--detached) detached=1 ;;
	--side) side=1 ;;
	%*) pane=$arg ;;
	esac
done
pane=${pane:-${TMUX_PANE:-}}
[[ $pane == %* ]] || exit 0

notify() { tmux display-message "tmux-crew-dash: $*"; }

# The path comes last so a '|' inside it stays in the final field.
IFS='|' read -r win bridge_win cwd <<<"$(tmux display-message -p -t "$pane" '#{window_id}|#{@bridge_win}|#{pane_current_path}' 2>/dev/null)"
[[ $win == @* ]] || exit 0

# Read apart from the line above: the sock and dir values can hold a '|'.
if [[ -n $bridge_win ]]; then
	bridge_pane="$(tmux display-message -p -t "$pane" '#{@bridge_pane}' 2>/dev/null)"
	bridge_sock="$(tmux display-message -p -t "$pane" '#{@bridge_sock}' 2>/dev/null)"
	if [[ -n $bridge_pane && -n $bridge_sock ]]; then
		bridge_dir="$(tmux display-message -p -t "$pane" '#{@bridge_dir}' 2>/dev/null)"
		if [[ $BRIDGE_CTL == @* ]]; then
			notify "bridge ctl is not installed"
			exit 0
		fi
		# The reason is kept; '#' is doubled for display-message.
		bridge_tool() { err="$("$BRIDGE_CTL" --sock "$bridge_sock" tool "$bridge_pane" "$1" "$bridge_dir" 2>&1 >/dev/null)"; }
		tool=crew-dash
		((side)) && tool=crew-dash-side
		if ! bridge_tool "$tool"; then
			# A daemon older than crew-dash-side answers `unknown tool`: retry once with the float.
			if ((side)) && [[ $err == *"unknown tool"* ]] && bridge_tool crew-dash; then
				notify "no remote side panel yet, opened the crew dash float on the remote host"
				exit 0
			fi
			notify "could not open crew dash on the remote host: ${err//#/##}"
		fi
		exit 0
	fi
fi

focus() { ((detached)) || tmux select-pane -t "$1"; }

# Side mode. Returns 0 when it handled the call (focused, created or failed
# loudly); 1 to fall through to the float path.
open_side() {
	local sid="" pid floating label grid ww pw pfl w narrow=""
	# A mirror window is daemon-owned: a local tiled pane would fight its layout mirroring.
	if [[ -n $bridge_win ]]; then
		notify "no side dash in a mirror window, opened the crew dash float instead"
		return 1
	fi
	while IFS='|' read -r pid floating label; do
		[[ $floating != 1 && $label == "$SIDE_LABEL" ]] && sid=$pid
	done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}')
	if [[ -n $sid ]]; then
		focus "$sid"
		return 0
	fi

	command -v crew >/dev/null || {
		notify "crew is not on PATH"
		return 0
	}

	IFS='|' read -r grid ww pw pfl <<<"$(tmux display-message -p -t "$pane" '#{@crew_grid}|#{window_width}|#{pane_width}|#{pane_floating_flag}' 2>/dev/null)"
	[[ $ww =~ ^[0-9]+$ && $pw =~ ^[0-9]+$ ]] || return 1
	# Splitting a float halves it into two floats, never a tiled pane.
	if [[ $pfl == 1 ]]; then
		notify "target is a floating pane, opened the crew dash float instead"
		return 1
	fi
	if [[ $grid == 1 ]]; then
		notify "grid window re-tiles every pane, opened the crew dash float instead"
		return 1
	fi
	w=$((ww * 32 / 100))
	((w < SIDE_MIN)) && w=$SIDE_MIN
	((w > SIDE_MAX)) && w=$SIDE_MAX
	if ((pw - w - 1 < SIDE_KEEP)); then
		notify "window too narrow for a side dash, opened the crew dash float instead"
		return 1
	fi

	[[ $(crew dash --help </dev/null 2>&1) == *--narrow* ]] && narrow=" --narrow"
	local args=(split-window -h -l "$w" -t "$pane" -P -F '#{pane_id}')
	((detached)) && args+=(-d)
	[[ -z $cwd ]] || args+=(-c "${cwd//#/##}")
	sid="$(tmux "${args[@]}" "exec crew dash$narrow" 2>/dev/null)"
	[[ $sid == %* ]] || {
		notify "could not open a side pane"
		return 0
	}
	# The relaunch runs in the user's default shell, fish included: keep it POSIX and fish-safe.
	tmux set-option -p -t "$sid" @pane_label "$SIDE_LABEL" \; \
		set-option -p -t "$sid" @remux_relaunch "tmux set -p @pane_label $SIDE_LABEL; exec crew dash$narrow"
	return 0
}
if ((side)) && open_side; then
	exit 0
fi

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
