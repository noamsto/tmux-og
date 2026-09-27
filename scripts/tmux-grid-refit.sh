#!/usr/bin/env bash
# Lay a dispatcher "grid" window out responsively, keeping @crew_role=lead as
# the main pane. tmux-og owns the layout policy; the dispatcher owns every hint
# this reads (window @crew_grid / @crew_grid_main_pct, pane @crew_role) and calls
# this after adding or removing a role pane. Nothing here ever writes a @crew_*
# option.
#   args: <target-window>   (window-resized[10] and window-layout-changed
#                            pass #{q:window_id})
# No-op unless the window carries @crew_grid=1, so a window that is not a crew
# grid — and any non-tmux-og server — is untouched. Silent and convergent: an
# unchanged grid issues no layout-changing tmux command and emits no reflow
# notification — it writes only its own @grid_refit_* bookkeeping options
# (@grid_refit_sig caches the last applied decision, and the read-only probes
# before it are cheap).
#
# Two triggers, because a grid can change shape without a resize (#760): the
# window-resized[10] hook covers a client resize, and window-layout-changed
# covers a split/kill/move of any pane — the aeye carousel toggle splits then
# kills a pane, which never resizes the window, so without the second hook the
# freed cells stayed with a neighbour and the lead never returned to
# @crew_grid_main_pct. Floats are excluded from the pane set below: a float
# open/close fires window-layout-changed too, but it is not part of the tiled
# layout select-layout acts on, so counting it would churn @grid_refit_sig.
set -uo pipefail

target=${1:-}
[[ -z $target ]] && exit 0

# Byte-identical to the grid gate in config/tmux.conf.tmpl (grid-refit.bats
# fails on drift). It carries every input of the decision below, not just
# geometry, so the gate never skips an event that could change the decision.
grid_sig_fmt='#{window_width}x#{window_height}:#{@crew_grid_main_pct}:#{@grid_refit_min_role_cols}:#{@grid_refit_aspect}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height}.#{@crew_role} }}'

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

read_panes() { tmux list-panes -t "$target" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id}' 2>/dev/null; }

# One snapshot, so the stamp below describes exactly the state the verdict
# used. It also folds in the lead lookup and the three read_opt values (#816):
# every tmux call between the pending-clear and the eventual stamp widens the
# window in which a same-signature event still forks a run, so the fast path
# now makes only this call and the stamp. The lead's pane_id cannot contain
# '|'; the three option values are free text, so each is rendered `|`-safe
# with `s/[|]/ /`, matching tmux-update-icons.sh's same idiom. The signature
# is still the last field: it absorbs any '|' left in it as the final `read`
# variable.
#
# The hooks set @grid_refit_pending before forking this run, so the rest of a
# resize burst skips while it is on its way. Clearing it in this command list
# means every event it suppressed predates everything this run reads, so this
# must stay the first tmux call.
IFS='|' read -r is_grid zoomed w h pane_ids stored_sig lead_ids pct_raw min_cols_raw aspect_raw geom <<<"$(tmux display-message -p -t "$target" "#{==:#{@crew_grid},1}|#{window_zoomed_flag}|#{window_width}|#{window_height}|#{P:#{?pane_floating_flag,,#{pane_index}:#{pane_id} }}|#{@grid_refit_sig}|#{P:#{?#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}},#{pane_id} ,}}|#{s/[|]/ /:@crew_grid_main_pct}|#{s/[|]/ /:@grid_refit_min_role_cols}|#{s/[|]/ /:@grid_refit_aspect}|$grid_sig_fmt" \; set-option -wu -t "$target" @grid_refit_pending 2>/dev/null)"
[[ $is_grid == 1 ]] || exit 0

# Zoom is user state: a zoomed grid is left exactly as the user left it.
[[ $zoomed == 1 ]] && exit 0

# The lead pane is the main pane. No lead -> not a grid we can lay out.
# Floats are excluded everywhere (#760): window-layout-changed fires on a float
# open/close, and a float is not part of the tiled set select-layout lays out.
# Two panes tagged lead at once is a bug the dispatcher must not create;
# "first" here is tmux's own iteration order, not a guaranteed tie-break.
read -r lead _ <<<"$lead_ids"
[[ -n $lead ]] || exit 0

[[ $w =~ ^[0-9]+$ && $h =~ ^[0-9]+$ ]] || exit 0
# #{P:} is not layout order on tmux-next (a swap-pane leaves it unchanged);
# pane_index is.
mapfile -t ids < <(tr ' ' '\n' <<<"$pane_ids" | grep . | sort -t: -k1,1n | cut -d: -f2)
np=${#ids[@]}
((np > 1)) || exit 0

# Lead's share of the window; out-of-range/non-integer falls back to 60.
pct=$pct_raw
[[ $pct =~ ^[0-9]+$ ]] && ((pct >= 1 && pct <= 99)) || pct=60
# Minimum role-area width below which the roles get too narrow a column.
min_cols=$min_cols_raw
[[ $min_cols =~ ^[0-9]+$ ]] || min_cols=30
# Cells are ~2:1 tall, so main-vertical needs this much width per unit of height.
aspect=$aspect_raw
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
first=${ids[0]}
if [[ $stored_sig == "$sig" && $lead == "$first" ]]; then
	# Stamp the verified snapshot so the hook gate skips events on it. The
	# under-lock re-check below never stamps: a match there means a peer
	# applied after this snapshot.
	tmux set-option -w -t "$target" @grid_refit_layout "$geom" 2>/dev/null || true
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

# One command list, not three separate client calls: swap-pane and
# select-layout each fire the window-layout-changed hook, which can fork a
# peer (a background run, or the dispatcher's own direct call racing this
# one) before this invocation's next command runs. A peer that connects as a
# new client is queued behind whatever this client's own list still has
# left, so bundling the trailing @grid_refit_sig write into the same list
# guarantees it lands before any such peer's read — closing a race where a
# peer read the pre-write (stale) sig, landed on this same full-apply branch
# instead of the fast path above, and exited without ever stamping
# @grid_refit_layout (only the fast path does), leaving nothing to trigger a
# later confirming run (#827). A failed command aborts the rest of the list
# (verified: an unknown command drops a trailing set-option too), so the sig
# still only lands on a decision that actually applied.
if [[ $lead == "$first" ]]; then
	tmux set-window-option -t "$target" "$opt" "${pct}%" \; select-layout -t "$target" "$layout" \; set-option -w -t "$target" @grid_refit_sig "$sig" 2>/dev/null
else
	tmux swap-pane -d -s "$lead" -t "$first" \; set-window-option -t "$target" "$opt" "${pct}%" \; select-layout -t "$target" "$layout" \; set-option -w -t "$target" @grid_refit_sig "$sig" 2>/dev/null
fi
exit 0
