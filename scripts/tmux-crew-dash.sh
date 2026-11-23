#!/usr/bin/env bash
# Toggle `crew dash` in a labelled floating pane: open or restore it, or dismiss a visible one (#1139, #1186).
#
#   tmux-crew-dash.sh [--side [--toggle]] [--detached] [pane-id]
#
# The window is the pane's window; pane-id falls back to $TMUX_PANE. A bind's
# run-shell sets no TMUX_PANE and a bare display-message resolves tmux's "best"
# client, so the bind passes #{q:pane_id} explicitly; a dispatcher calls this from
# a worker pane, where $TMUX_PANE is set. --detached leaves focus where it is.
#
# One dash per window, and the press toggles it (#1186). On a plain press (neither
# --detached nor --side; --side --toggle is below) a visible dash is dismissed: a float labelled crew-dash (a plain list-panes scan,
# last match wins) is hidden by `tmux-float-group hide crew`, so the next press
# restores it with its state; a tiled pane whose foreground process is `crew dash`
# is closed (a tile cannot be stashed without disturbing the layout), as is a
# visible crew-dash-side pane (found by label). Else a crew-group copy hidden by
# `tmux-float-group hide crew` is restored, else a new one is created. --detached
# and a plain --side never dismiss: a visible dash is only focused or left alone. The
# build substitutes the sibling's store path into FLOAT_GROUP (a pane shell's PATH may not carry the scripts);
# OG_FLOAT_GROUP_BIN is the test seam, and the raw placeholder is not executable.
#
# In a mirror window (@bridge_win with @bridge_pane and @bridge_sock) the dash runs
# on the REMOTE host: the bridge ctl `tool` verb opens it there and the mirror
# reconciles it. Nothing is created locally, and --detached cannot suppress the
# verb's focus. The build substitutes the ctl's store path into @bridge_ctl@;
# OG_BRIDGE_CTL_BIN is the test seam, and the raw placeholder is not executable.
# The verb gets a trailing `toggle` only on a plain press or `--side --toggle`
# (never --detached): the remote kills a visible float, else closes a tiled
# crew-dash-side pane, else opens the float (`--side --toggle` closes the side
# pane only). A @bridge_win window lacking either option keeps the local path below.
#
# --side (#1185) opens the dash as a tiled split on the right of the pane instead
# (label crew-dash-side, `crew dash --narrow` when this crew lists the flag), for
# a dispatcher that wants it persistent beside the agent. The split takes 32% of
# the window clamped to 50..80 columns and leaves the target at least 80; a window
# too narrow for that, or a @crew_grid window (tmux-grid-refit re-tiles every
# tiled pane there), falls back to the float below with a notification. A float
# and a side dash coexist; --side never touches the float. `--side --toggle` closes a
# visible side pane and leaves the float alone; a plain press dismisses the float first, else the side pane. A mirror window sends the
# `crew-dash-side` tool, so the REMOTE side gets the split too; a daemon older than
# that tool answers `unknown tool`, and the script retries once with the `crew-dash`
# float tool and tells the user. @remux_relaunch re-stamps the label and
# re-execs the dash, so a remux restore keeps the pane findable. The pane carries
# @pane_pref_width 32,50,80,80 and tmux-pane-pref-width resizes it back into 50..80
# after a resize, a sibling close or a restore.
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
# The side pane's @pane_pref_width (pct,min,max,keep): the same 32%/50/80/80 as the split rule, kept by tmux-pane-pref-width.
SIDE_PREF="32,50,80,80"
FLOAT_GROUP="${OG_FLOAT_GROUP_BIN:-@float_group@}"
BRIDGE_CTL="${OG_BRIDGE_CTL_BIN:-@bridge_ctl@}"

detached=0
side=0
toggle=0
pane=""
for arg in "$@"; do
	case $arg in
	--detached) detached=1 ;;
	--side) side=1 ;;
	--toggle) toggle=1 ;;
	%*) pane=$arg ;;
	esac
done
# --side keeps its idempotent focus unless --toggle: only a plain press dismisses the float.
dismiss=1
((detached)) && toggle=0
((detached || side)) && dismiss=0
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
		# The reason is kept; '#' is doubled for display-message. toggle rides only a plain press or --side --toggle.
		bridge_tool() { err="$("$BRIDGE_CTL" --sock "$bridge_sock" tool "$bridge_pane" "$1" "$bridge_dir" "${@:2}" 2>&1 >/dev/null)"; }
		bridge_side() { if ((toggle)); then bridge_tool crew-dash-side toggle; else bridge_tool crew-dash-side; fi; }
		if ((side)); then
			if ! bridge_side; then
				# A daemon older than crew-dash-side answers `unknown tool`: retry once with the float.
				if [[ $err == *"unknown tool"* ]] && bridge_tool crew-dash; then
					notify "no remote side panel yet, opened the crew dash float on the remote host"
					exit 0
				fi
				notify "could not open crew dash on the remote host: ${err//#/##}"
			fi
		else
			# toggle rides only a plain press; an empty array trips set -u on bash < 4.4.
			if ((dismiss)); then bridge_tool crew-dash toggle; else bridge_tool crew-dash; fi || notify "could not open crew dash on the remote host: ${err//#/##}"
		fi
		exit 0
	fi
fi

focus() { ((detached)) || tmux select-pane -t "$1"; }

# Side mode. Returns 0 when it handled the call (focused, created or failed
# loudly); 1 to fall through to the float path.
open_side() {
	local sid="" pid floating label grid ww pw pfl zoom w narrow=""
	# A mirror window is daemon-owned: a local tiled pane would fight its layout mirroring.
	if [[ -n $bridge_win ]]; then
		notify "no side dash in a mirror window, opened the crew dash float instead"
		return 1
	fi
	while IFS='|' read -r pid floating label; do
		[[ $floating != 1 && $label == "$SIDE_LABEL" ]] && sid=$pid
	done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}')
	if [[ -n $sid ]]; then
		if ((toggle)); then
			tmux kill-pane -t "$sid"
		else
			focus "$sid"
		fi
		return 0
	fi

	command -v crew >/dev/null || {
		notify "crew is not on PATH"
		return 0
	}

	IFS='|' read -r grid ww pw pfl zoom <<<"$(tmux display-message -p -t "$pane" '#{@crew_grid}|#{window_width}|#{pane_width}|#{pane_floating_flag}|#{window_zoomed_flag}' 2>/dev/null)"
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
	# pane_width reads the zoomed pane's full-window width, but split-window unzooms first and splits the real cell.
	if [[ $zoom == 1 ]]; then
		notify "window is zoomed, opened the crew dash float instead"
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
	# The set-hook -R re-runs the window's layout hooks so the width guard re-checks the restored pane (tmux-remux restores no pane options).
	tmux set-option -p -t "$sid" @pane_label "$SIDE_LABEL" \; \
		set-option -p -t "$sid" @pane_pref_width "$SIDE_PREF" \; \
		set-option -p -t "$sid" @remux_relaunch "tmux set -p @pane_label $SIDE_LABEL; tmux set -p @pane_pref_width $SIDE_PREF; tmux set-hook -R -t \"\$TMUX_PANE\" window-layout-changed; exec crew dash$narrow"
	return 0
}
if ((side)) && open_side; then
	exit 0
fi

id=""
idgrp=""
while IFS='|' read -r pid floating label grp; do
	[[ $floating == 1 && $label == "$LABEL" ]] && id=$pid idgrp=$grp
done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}|#{@float_group}')
if [[ -n $id ]]; then
	if ((! dismiss)); then
		focus "$id"
		exit 0
	fi
	[[ -x $FLOAT_GROUP ]] || {
		notify "tmux-float-group is not installed"
		exit 0
	}
	# `hide crew` skips a float outside the crew group, so a re-tagged dash joins it first.
	# tmux-float-group refuses a mirror window, so a dash there is closed instead.
	[[ -z $bridge_win ]] || {
		tmux kill-pane -t "$id"
		exit 0
	}
	[[ $idgrp == "$GROUP" ]] || "$FLOAT_GROUP" tag "$GROUP" "$id"
	exec "$FLOAT_GROUP" hide "$GROUP" "$pane"
fi

# A visible side dash (#1185) is found by its label, no process inspection.
if ((dismiss)); then
	while IFS='|' read -r pid floating label; do
		[[ $floating != 1 && $label == "$SIDE_LABEL" ]] || continue
		tmux kill-pane -t "$pid"
		exit 0
	done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}')
fi

# A hand-typed tiled dash (no label): the foreground process group of the pane's tty runs `crew dash`
# (the same argv a float runs). pane_current_command alone says only `crew`,
# which a worker pane's `crew await` shares. The first match in index order wins.
if ((dismiss)); then
	while IFS='|' read -r pid floating tty; do
		[[ $floating == 0 && $tty == /dev/* ]] || continue
		while read -r stat args; do
			[[ $stat == *+* && $args =~ ^([^[:space:]]*/)?\.?crew(-wrapped)?[[:space:]]+dash([[:space:]]|$) ]] || continue
			tmux kill-pane -t "$pid"
			exit 0
		done < <(ps -ww -t "${tty#/dev/}" -o stat=,args= 2>/dev/null)
	done < <(tmux list-panes -t "$win" -F '#{pane_id}|#{pane_floating_flag}|#{pane_tty}')
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
args=(new-pane -t "$win" -x 96% -y 94% -X 2% -Y 3% -B heavy -P -F '#{pane_id}')
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
tmux set-option -p -t "$id" @float_geom '96% 94% 2% 3%' \; \
	set-option -p -t "$id" remain-on-exit off \; \
	set-option -p -t "$id" @pane_label "$LABEL" \; \
	set-option -p -t "$id" @float_group "$GROUP"
exit 0
