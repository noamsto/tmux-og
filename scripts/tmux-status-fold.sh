#!/usr/bin/env bash
# tmux-status-fold: animate the status bar between its grid height and one line.
# Bound to `prefix + Z`, which flips @og_status_folded in a single `set -F` and
# then runs this backgrounded with the session name and client width, the same
# arguments the reflow hooks pass. The flag is the intent; this script walks the
# live `status` line count toward it one line at a time, then hands the final
# state to tmux-reflow-windows (--force), the one place that knows what a folded
# or unfolded bar looks like.

# shellcheck source=/dev/null  # Nix store path substituted at build time
source @lib_log@

SESSION=${1:-}
WIDTH=${2:-}
REFLOW=${OG_FOLD_REFLOW:-@reflow@}
DELAY=${OG_FOLD_STEP_DELAY:-0.04}

[[ -n $SESSION ]] || exit 0
# Scratch sessions run their own hints bar (see tmux-reflow-windows).
case "$SESSION" in
scratch-*) exit 0 ;;
esac

# Last press wins: each invocation stamps a token and re-checks it before every
# write, so a superseded run stops mid-animation instead of interleaving steps.
stamp="${TMPDIR:-/tmp}/og-status-fold.${SESSION//\//_}"
token=$EPOCHREALTIME
printf '%s' "$token" >"$stamp" 2>/dev/null
current() {
	local last=""
	[[ -f $stamp ]] && read -r last <"$stamp"
	[[ $last == "$token" ]]
}

# One animator per session. A newer press queues behind the older run, which
# sees its token go stale at the next step and exits; an older press that finds
# itself superseded while still queued just leaves.
lock="${TMPDIR:-/tmp}/og-status-fold.lock.${SESSION//\//_}"
until acquire_lock "$lock"; do
	current || exit 0
	sleep 0.02
done
current || exit 0

# Session id, not name: a numeric name also resolves as a pane index in
# set-option targets.
IFS='|' read -r SID folded cur lines bridge < <(tmux display-message -t "=$SESSION:" -p '#{session_id}|#{@og_status_folded}|#{status}|#{@reflow_lines}|#{@bridge_win}' 2>/dev/null)
[[ -n $SID ]] || exit 0
# tmux spells a one-line bar `on` (and a bare 1 is rejected as a value).
[[ $cur == on ]] && cur=1
[[ $cur =~ ^[1-5]$ ]] || cur=2
[[ $lines =~ ^[1-5]$ ]] || lines=$cur

# set_status LINES: the status option's own spelling of a line count.
set_status() {
	local v=$2
	((v == 1)) && v=on
	tmux set -t "$1" status "$v"
}

reflow() { "$REFLOW" --force "$SESSION" "$WIDTH" >/dev/null 2>&1; }

tmux set -u -t "$SID" @og_status_cap 2>/dev/null

# Every step resizes the window, and in a bridge mirror that drives a remote
# reshape (docs/agents/bridge-daemon.md): jump straight to the end instead of
# sending a burst of them.
if [[ -n $bridge ]]; then
	reflow
	exit 0
fi

if [[ $folded == 1 ]]; then
	for ((k = cur - 1; k >= 1; k--)); do
		current || exit 0
		set_status "$SID" "$k"
		sleep "$DELAY"
	done
	current || exit 0
	reflow
	exit 0
fi

# Unfold. The cap holds reflow at the step already on screen while it restores
# the grid's formats and stamps @reflow_lines, the height to grow to; each step
# raises the cap with the status so a reflow landing mid-animation cannot snap
# it back or ahead.
tmux set -t "$SID" @og_status_cap "$cur"
reflow
lines=$(tmux display-message -t "$SID" -p '#{@reflow_lines}' 2>/dev/null)
if [[ $lines =~ ^[1-5]$ ]]; then
	for ((k = cur + 1; k <= lines; k++)); do
		current || exit 0
		tmux set -t "$SID" @og_status_cap "$k"
		set_status "$SID" "$k"
		sleep "$DELAY"
	done
fi
current || exit 0
tmux set -u -t "$SID" @og_status_cap
reflow
