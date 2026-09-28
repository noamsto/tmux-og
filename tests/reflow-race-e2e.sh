#!/usr/bin/env bash
# End-to-end reflow race check: a burst of window closes must leave the status
# line exactly as a from-scratch reflow would draw it.
#
# Manual, Linux only (needs util-linux script(1) for a real attached client);
# not part of `nix flake check`. Run after `nix build .#default`:
#
#   tests/reflow-race-e2e.sh [--mode burst|freeze] [--windows N] [--trials T]
#                            [--jump-to M] [TMUX_BIN]
#
#   burst   kill N windows down to 1, let the hook-fired reflows settle
#   freeze  the same, then add windows back up to whatever count the burst
#           stamped into @reflow_key, in one chained command (5 if the key
#           was truthful). --jump-to M forces that target instead, for a
#           like-for-like control against counts a broken build poisoned.
#
# Each trial snapshots the reflow state, runs one forced reflow, and snapshots
# again. It exits non-zero if any trial's settled state differs: key_ok=0 is a
# @reflow_key describing another window count, render_ok=0 is the layout itself.
set -uo pipefail

MODE=burst
N=10
TRIALS=5
TMUX_BIN=./result/bin/tmux
while (($#)); do
	case "$1" in
	--mode) MODE=$2 && shift ;;
	--windows) N=$2 && shift ;;
	--trials) TRIALS=$2 && shift ;;
	--jump-to) JUMP_TO=$2 && shift ;;
	*) TMUX_BIN=$1 ;;
	esac
	shift
done
if [[ -n ${JUMP_TO:-} ]] && ! [[ $JUMP_TO =~ ^[0-9]+$ && $JUMP_TO -ge 2 ]]; then
	echo "--jump-to needs a window count of at least 2" >&2
	exit 2
fi
case "$MODE" in burst | freeze) ;; *)
	echo "unknown --mode: $MODE" >&2
	exit 2
	;;
esac
if ! script --version 2>/dev/null | grep -q util-linux; then
	echo "needs util-linux script(1)" >&2
	exit 2
fi
[[ -x $TMUX_BIN ]] || {
	echo "no tmux at $TMUX_BIN (run nix build .#default)" >&2
	exit 2
}
TMUX_BIN=$(realpath "$TMUX_BIN")
unset TMUX

SESS=reflowrace
T=()
clientpid=""
root=""

# shellcheck disable=SC2329  # invoked by the EXIT trap
cleanup() {
	[[ -n $clientpid ]] && kill "$clientpid" 2>/dev/null
	((${#T[@]})) && "${T[@]}" kill-server 2>/dev/null
	[[ -n $root ]] && rm -rf "$root"
}
trap cleanup EXIT

snap() {
	local fields sf labels
	fields=$("${T[@]}" display-message -t "$SESS" -p '#{session_windows}|#{@reflow_key}|#{@window_split}|#{@window_split2}|#{@window_split3}|#{@window_per}')
	fields+="|status=$("${T[@]}" show-options -t "$SESS" -qv status)"
	sf=$("${T[@]}" show-options -t "$SESS" status-format 2>/dev/null | md5sum | cut -c1-8)
	labels=$("${T[@]}" list-windows -t "$SESS" -F '#{@window_label_disp}|#{@window_label_id_disp}|#{@window_pr_disp}' | md5sum | cut -c1-8)
	printf '%s|sf=%s|labels=%s' "$fields" "$sf" "$labels"
}

# Settled means no reflow for the session is running and the lock is free,
# twice in a row half a second apart.
reflows_idle() {
	[[ ! -d $TMPDIR/og-reflow.lock.$SESS ]] && ! pgrep -f "tmux-reflow-windows $SESS" >/dev/null
}
settle() {
	for _ in $(seq 1 400); do
		if reflows_idle; then
			sleep 0.5
			reflows_idle && return 0
		fi
		sleep 0.2
	done
	echo "reflows never settled" >&2
	return 1
}

failed=0
for trial in $(seq 1 "$TRIALS"); do
	# Short path: tmux's socket lives under TMUX_TMPDIR, capped near 108 bytes.
	root=$(mktemp -d /tmp/og614.XXXXXX)
	mkdir -p "$root"/{tmux,state,tmp,cwd,data,cache}
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$root/status"
	export TMUX_TMPDIR="$root/tmux" CLAUDE_STATUS_DIR="$root/status" XDG_STATE_HOME="$root/state"
	export TMPDIR="$root/tmp" XDG_DATA_HOME="$root/data" XDG_CACHE_HOME="$root/cache"
	T=("$TMUX_BIN" -L reflowrace)

	"${T[@]}" new-session -d -s "$SESS" -c "$root/cwd" -x 120 -y 40
	for _ in $(seq 2 "$N"); do "${T[@]}" new-window -d -t "$SESS" -c "$root/cwd"; done
	script -qec "env TMUX_TMPDIR=$TMUX_TMPDIR $TMUX_BIN -L reflowrace attach -t $SESS" /dev/null >/dev/null 2>&1 &
	clientpid=$!
	for _ in $(seq 1 50); do
		[[ $("${T[@]}" display-message -t "$SESS" -p '#{client_width}' 2>/dev/null) =~ ^[1-9][0-9]*$ ]] && break
		sleep 0.1
	done
	settle || failed=1

	"${T[@]}" list-windows -t "$SESS" -F '#{window_id}' | tail -n +2 | while read -r id; do
		"${T[@]}" kill-window -t "$id"
	done
	settle || failed=1

	extra=""
	if [[ $MODE == freeze ]]; then
		k=$(snap | cut -d'|' -f2)
		k=${k%%:*}
		m=5
		[[ $k =~ ^[0-9]+$ ]] && ((k > 1)) && m=$k
		[[ -n ${JUMP_TO:-} ]] && m=$JUMP_TO
		chain=()
		for _ in $(seq 2 "$m"); do chain+=(new-window -d -t "$SESS" -c "$root/cwd" ';'); done
		unset 'chain[${#chain[@]}-1]'
		"${T[@]}" "${chain[@]}"
		settle || failed=1
		extra=" stamped_after_kills=$k jumped_to=$m"
	fi
	after=$(snap)

	"${T[@]}" run-shell "$("${T[@]}" show-options -gv @reflow_bin) $SESS $("${T[@]}" display-message -t "$SESS" -p '#{client_width}') --force"
	settle || failed=1
	fresh=$(snap)

	key_ok=0
	[[ $(cut -d'|' -f2 <<<"$after") == "$(cut -d'|' -f2 <<<"$fresh")" ]] && key_ok=1
	render_ok=0
	[[ $(cut -d'|' -f1,3- <<<"$after") == "$(cut -d'|' -f1,3- <<<"$fresh")" ]] && render_ok=1
	((key_ok && render_ok)) || failed=1
	echo "mode=$MODE N=$N trial=$trial key_ok=$key_ok render_ok=$render_ok$extra"
	echo "  settled: $after"
	echo "  fresh:   $fresh"

	kill "$clientpid" 2>/dev/null
	wait "$clientpid" 2>/dev/null
	clientpid=""
	"${T[@]}" kill-server 2>/dev/null
	rm -rf "$root"
	root=""
done
exit "$failed"
