#!/usr/bin/env bash
# Per-call cost of tmux-update-icons (#1112): the 1s `#()` job in
# status-format[0] ("tick" driver) and the 5s @og-sweep-tick hook ("sweep"
# driver), on a scratch server holding NWIN windows x PANES panes, NAGENT of
# them agent windows. Reports execs per call (strace), wall ms, client CPU ms
# and tmux server CPU ms per call. See docs/agents/performance.md.
#
# Usage: tests/perf/update-icons-cost.sh [NWIN [PANES [NAGENT]]]   (20 2 5)
# Env: UPDATE_ICONS_BIN (the tmux-update-icons under test; default: the one in
#        the built wrapper's `*-tmux-wrapper-bin/bin`, next to the raw tmux
#        that the scratch server and the job both use),
#      TMUX_BIN (the built wrapper, default ./result/bin/tmux),
#      K (calls per timing run, default 60),
#      REPS (repetitions, default 3).
# Needs strace and GNU coreutils; run inside the devshell. Manual: the numbers
# are timing-dependent, so this is outside `nix flake check`.
#
# The table's Windows/Panes columns are server totals (the `other` session's
# window included), not the arguments.
set -euo pipefail

command -v strace >/dev/null || {
	echo "update-icons-cost: strace not on PATH" >&2
	exit 1
}

NWIN="${1:-20}"
PANES="${2:-2}"
NAGENT="${3:-5}"
K="${K:-60}"
REPS="${REPS:-3}"

TMUX_BIN="$(realpath "${TMUX_BIN:-./result/bin/tmux}")"
WRAP_DIR="$(grep -o '/nix/store/[^ :"'"'"']*-tmux-wrapper-bin/bin' "$TMUX_BIN" | head -n 1)"
[ -n "$WRAP_DIR" ] || {
	echo "update-icons-cost: no *-tmux-wrapper-bin/bin in $TMUX_BIN" >&2
	exit 1
}
BIN="${UPDATE_ICONS_BIN:-$WRAP_DIR/tmux-update-icons}"
export PATH="$WRAP_DIR:$PATH"

R="$(mktemp -d /tmp/og-icons-cost.XXXX)"
export TMUX_TMPDIR="$R"
export CLAUDE_STATUS_DIR="$R/status"
export XDG_DATA_HOME="$R/data"
unset TMUX
mkdir -p "$CLAUDE_STATUS_DIR/panes" "$CLAUDE_STATUS_DIR/screen" "$XDG_DATA_HOME"
chmod 700 "$CLAUDE_STATUS_DIR"

t() { tmux -L probe "$@"; }
cleanup() {
	t kill-server >/dev/null 2>&1 || true
	rm -rf "$R"
}
trap cleanup EXIT INT TERM

# Every window sits in a git repo with @branch/@worktree already set, so the
# steady state forks no git beyond the one poll for the invoking window.
REPO="$R/repo"
mkdir -p "$REPO"
git -C "$REPO" init -q -b main

# pane_current_command is `claude`. The wrapper's coreutils is one multicall
# binary that rejects a renamed argv[0], so `exec -a claude sleep` dies at once;
# a bash blocked on the pane's tty keeps the pane alive instead.
AGENT_CMD="exec -a claude bash -c 'read -r'"
new_window() { # new_window <index> -- the first NAGENT windows run an agent
	local cmd=()
	[ "$1" -lt "$NAGENT" ] && cmd=(bash -c "$AGENT_CMD")
	if [ "$1" -eq 0 ]; then
		t -f /dev/null start-server \; set -g default-shell "$(command -v bash)" \; \
			new-session -d -s work -x 200 -y 50 -c "$REPO" "${cmd[@]}"
	else
		t new-window -d -t work: -c "$REPO" "${cmd[@]}"
	fi
}
for ((i = 0; i < NWIN; i++)); do new_window "$i"; done
for w in $(t list-windows -t work -F '#{window_id}'); do
	for ((j = 1; j < PANES; j++)); do t split-window -d -t "$w" -c "$REPO"; done
done
t new-session -d -s other -x 200 -y 50 -c "$REPO"
for w in $(t list-windows -a -F '#{window_id}'); do
	t set -w -t "$w" @branch main \; set -w -t "$w" @worktree "$REPO"
done

# State files so the agent panes take the live-agent path.
now="$(date +%s)"
while IFS='|' read -r id cmd; do
	[ "$cmd" = claude ] || continue
	printf 'state=idle\ntimestamp=%s\nunseen=0\nsession=s\n' "$((now - 30))" \
		>"$CLAUDE_STATUS_DIR/panes/${id#%}"
done < <(t list-panes -a -F '#{pane_id}|#{pane_current_command}')

SPID="$(t display -p '#{pid}')"
STIME="$(t display -p '#{start_time}')"
DRV_TMUX="$(t display -p '#{socket_path}'),$SPID,0"
WINS="$(t list-windows -a | wc -l)"
PANES_TOTAL="$(t list-panes -a | wc -l)"
# Agent arming is first-pass only; disable it so every timed call is steady state.
export AGENT_DETECT_BIN='@off'

# Production argument vector of status-format[0].
TICK_ARGS=(work '' "$STIME" '' mocha "$SPID")
# The tick's %60 reap must not fire while timing: a base of 60n+61 with
# offsets cycling through 0..58 keeps CLAUDE_NOW%60 in 1..59 (one phase
# discontinuity per 59 calls). Consecutive values otherwise, so one call in
# five takes the %5 arm sweep and the spinner phases advance.
BASE=$((now / 60 * 60 + 61))

# LOOP <mode> <bin> [args...]: N_TICKS calls with consecutive CLAUDE_NOW. The
# sweep driver is `OG_TICK_SWEEP=1 <bin>` with no args, as the hook runs it.
# shellcheck disable=SC2016  # expanded by the inner bash, not here
LOOP='mode=$1 bin=$2; shift 2
for ((i = 0; i < N_TICKS; i++)); do
	if [ "$mode" = sweep ]; then
		CLAUDE_NOW=$((BASE + i % 59)) OG_TICK_SWEEP=1 "$bin"
	else
		CLAUDE_NOW=$((BASE + i % 59)) "$bin" "$@"
	fi >/dev/null 2>&1
done'
drive_argv() { # drive_argv <mode> -- sets DA to the command running LOOP for <mode>
	if [ "$1" = sweep ]; then
		DA=(bash -c "$LOOP" _ sweep "$BIN")
	else
		DA=(bash -c "$LOOP" _ tick "$BIN" "${TICK_ARGS[@]}")
	fi
}
# drive <mode> -- N_TICKS calls; the caller sets N_TICKS.
drive() {
	drive_argv "$1"
	TMUX="$DRV_TMUX" BASE="$BASE" "${DA[@]}"
}

# Server CPU in clock ticks. Field 2 of /proc/<pid>/stat is "(tmux: server)",
# with a space, so strip through the closing paren before indexing.
server_ticks() { awk '{ sub(/.*\) /, ""); print $12 + $13 }' "/proc/$SPID/stat"; }
CLK_TCK="$(getconf CLK_TCK)"

# Warm-up: the first pass writes every window option and arms the change gate.
export N_TICKS=3
drive tick
drive sweep
sleep 0.5

# execs_per_call <mode> -- "<all> <tmux>" successful execs per call, over five
# consecutive calls (one of them takes the %5 list-panes arm).
execs_per_call() {
	local mode=$1 out="$R/strace.out" all tmux_n
	rm -f "$out"
	drive_argv "$mode"
	TMUX="$DRV_TMUX" BASE="$BASE" N_TICKS=5 strace -f -qq -e trace=execve -o "$out" "${DA[@]}" \
		>/dev/null 2>&1
	# The traced `bash -c` itself is one exec.
	all="$(grep -c 'execve(.*= 0$' "$out")"
	tmux_n="$(grep 'execve(.*= 0$' "$out" | grep -c '/tmux"')"
	awk -v a="$all" -v t="$tmux_n" 'BEGIN { printf "%.1f %.1f\n", (a - 1) / 5, t / 5 }'
}

# time_calls <mode> -- "<wall ms> <client cpu ms> <server cpu ms>" per call.
time_calls() {
	local mode=$1 s0 s1 w0 w1 out
	export N_TICKS=$K
	s0="$(server_ticks)"
	w0="$EPOCHREALTIME"
	out="$({ time -p drive "$mode"; } 2>&1)"
	w1="$EPOCHREALTIME"
	s1="$(server_ticks)"
	awk -v w0="$w0" -v w1="$w1" -v k="$K" -v sc="$((s1 - s0))" -v hz="$CLK_TCK" \
		'/^user/ { u = $2 } /^sys/ { s = $2 }
		END { printf "%.1f %.1f %.1f\n", (w1 - w0) * 1000 / k, (u + s) * 1000 / k, sc * 1000 / hz / k }' \
		<<<"$out"
}

awk -v k="$K" -v hz="$CLK_TCK" 'BEGIN { printf "server CPU resolution: %.2f ms per call (one clock tick over K=%d calls)\n", 1000 / hz / k, k }'
echo "| Windows | Panes | driver | execs (tmux) | wall ms | client CPU ms | server CPU ms |"
echo "|---|---|---|---|---|---|---|"
for mode in tick sweep; do
	: >"$R/rows.$mode"
	for ((rep = 0; rep < REPS; rep++)); do
		read -r ex tm < <(execs_per_call "$mode")
		read -r wall cpu srv < <(time_calls "$mode")
		echo "$ex $tm $wall $cpu $srv" >>"$R/rows.$mode"
		printf '| %s | %s | %s | %s (%s) | %s | %s | %s |\n' \
			"$WINS" "$PANES_TOTAL" "$mode" "$ex" "$tm" "$wall" "$cpu" "$srv"
	done
	awk -v w="$WINS" -v p="$PANES_TOTAL" -v m="$mode" \
		'{ for (i = 1; i <= 5; i++) a[i] += $i }
		END { printf "| %s | %s | %s mean | %.1f (%.1f) | %.1f | %.1f | %.1f |\n", w, p, m, a[1] / NR, a[2] / NR, a[3] / NR, a[4] / NR, a[5] / NR }' \
		"$R/rows.$mode"
done
