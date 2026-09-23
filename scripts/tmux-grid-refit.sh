#!/usr/bin/env bash
# Lay a dispatcher "grid" window out responsively, keeping @crew_role=lead as
# the main pane. tmux-og owns the layout policy; the dispatcher owns every hint
# this reads (window @crew_grid / @crew_grid_main_pct, pane @crew_role) and calls
# this after adding or removing a role pane. Nothing here ever writes a @crew_*
# option.
#   args: <target-window>   (the window-resized hook passes #{window_id})
# No-op unless the window carries @crew_grid=1, so a window that is not a crew
# grid — and any non-tmux-og server — is untouched. Silent and convergent: an
# unchanged grid issues no state-changing tmux command and emits no reflow
# notification (@grid_refit_sig caches the last applied decision, and the
# read-only probes before it are cheap).
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

# acquire_lock DIR — non-blocking lock via atomic mkdir, the same primitive
# lib-log's acquire_lock uses (`flock` is Linux-only and this flake builds on
# darwin). Returns 0 once the lock is held (an EXIT trap releases it), 1 when a
# live holder owns it. A dir older than the stale window is a crashed holder's
# and is stolen, so a crash can never wedge every later refit.
acquire_lock() {
	local dir=$1 mtime now
	mkdir "$dir" 2>/dev/null && return 0
	if [[ -d $dir ]]; then
		mtime=$(stat -c %Y "$dir" 2>/dev/null || stat -f %m "$dir" 2>/dev/null || echo 0)
		now=$(date +%s)
		((now - mtime < 60)) && return 1
		rmdir "$dir" 2>/dev/null
	else
		rm -f "$dir" 2>/dev/null
	fi
	mkdir "$dir" 2>/dev/null
}

[[ "$(read_opt @crew_grid 0)" == 1 ]] || exit 0

# Zoom is user state: a zoomed grid is left exactly as the user left it.
[[ "$(tmux display-message -p -t "$target" '#{window_zoomed_flag}' 2>/dev/null)" == 1 ]] && exit 0

# The lead pane is the main pane. No lead -> not a grid we can lay out.
lead=$(tmux list-panes -t "$target" -f '#{==:#{@crew_role},lead}' -F '#{pane_id}' 2>/dev/null | head -1)
[[ -n $lead ]] || exit 0

read_panes() { tmux list-panes -t "$target" -F '#{pane_id}' 2>/dev/null; }
pane_ids=$(read_panes)
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

# $lead is part of the signature so a re-tagged lead forces a re-layout, and
# the lead-is-first test below makes a demoted lead (a race that slipped
# through before this lock existed) re-apply instead of caching the breakage.
sig="$layout:$pct:$np:${w}x${h}:$min_cols:$aspect:$lead"
first=$(printf '%s\n' "$pane_ids" | head -1)
if [[ "$(read_opt @grid_refit_sig '')" == "$sig" && $lead == "$first" ]]; then
	exit 0
fi

# Serialize the read-modify-write. window-resized is backgrounded and the
# dispatcher also calls this binary directly, so two refits can be in flight;
# without the lock both would swap the lead and the second swap demotes it.
srv=""
IFS=, read -r _ srv _ <<<"${TMUX:-}"
lockdir="${TMPDIR:-/tmp}/og-grid-refit.lock.${srv:-0}.${target//[^A-Za-z0-9]/_}"
acquire_lock "$lockdir" || exit 0
# shellcheck disable=SC2064  # bake $lockdir now; no locals live past EXIT
trap "rmdir \"$lockdir\" 2>/dev/null" EXIT

# Re-check under the lock: a peer may have applied this very decision while we
# were between the fast check above and the acquire.
first=$(read_panes | head -1)
if [[ "$(read_opt @grid_refit_sig '')" == "$sig" && $lead == "$first" ]]; then
	exit 0
fi

ok=1
[[ $lead == "$first" ]] || tmux swap-pane -d -s "$lead" -t "$first" 2>/dev/null || ok=0
tmux set-window-option -t "$target" "$opt" "${pct}%" 2>/dev/null || ok=0
tmux select-layout -t "$target" "$layout" 2>/dev/null || ok=0
# Cache only a decision that actually landed: a failed run leaves the sig stale
# so the next refit retries instead of caching the breakage.
((ok)) && tmux set-option -w -t "$target" @grid_refit_sig "$sig" 2>/dev/null || true
exit 0
