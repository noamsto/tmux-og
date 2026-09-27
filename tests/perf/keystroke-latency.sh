#!/usr/bin/env bash
# Keystroke-echo latency through the remote bridge under synthetic load
# (#793). Builds a "halo-shaped" chain -- a remote server (socket rem),
# mirrored by the real bridge daemon (--test-local) onto a local server
# (socket loc) with a real status-drawing client attached -- and runs
# picker/latencyprobe against the local mirror of the remote session's first
# pane. See docs/agents/performance.md for the method and findings.
#
# Usage: tests/perf/keystroke-latency.sh [scenario...]
#   scenarios (default: all): quiet busy churn attach reattach
# Env: TMUX_BIN (tmux binary under test, default ./result/bin/tmux),
#      SAMPLES (probe sample count, default 400),
#      VLOG=1 (run the remote server under `tmux -v` and print refit-hook
#      fork counts after busy and reattach).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMUX_BIN="${TMUX_BIN:-./result/bin/tmux}"
SAMPLES="${SAMPLES:-400}"

W="$(mktemp -d /tmp/og-perf.XXXX)"
export TMUX_TMPDIR="$W"
export CLAUDE_STATUS_DIR="$W/status"
unset TMUX
mkdir -p "$CLAUDE_STATUS_DIR" "$W/bin" "$W/vlog"
: >"$W/daemon.pids"

cleanup() {
	if [ -f "$W/daemon.pids" ]; then
		while read -r pid; do kill "$pid" 2>/dev/null || true; done <"$W/daemon.pids"
	fi
	pkill -f "$W/bin/daemon" 2>/dev/null || true
	for sock in rem loc obs obs2; do
		"$TMUX_BIN" -L "$sock" kill-server >/dev/null 2>&1 || true
	done
	rm -rf "$W"
}
trap cleanup EXIT INT TERM

(cd "$REPO_ROOT/picker" && go build -o "$W/bin/daemon" ./remotebridge/cmd/daemon)
(cd "$REPO_ROOT/picker" && go build -o "$W/bin/renderer" ./remotebridge/cmd/renderer)
(cd "$REPO_ROOT/picker" && go build -o "$W/bin/latencyprobe" ./latencyprobe)

# The raw (unwrapped) tmux dir, for the daemon's own PATH: og-remote-open
# hands the daemon the server's PATH, where the raw binary sits ahead of the
# Nix wrapper (~32ms/call vs ~1.3ms). .tmux-wrapped sits next to the wrapper
# script and symlinks to the store's unwrapped tmux.
wrapped_marker="$(dirname "$TMUX_BIN")/.tmux-wrapped"
if [ -e "$wrapped_marker" ]; then
	RAW_TMUX_DIR="$(dirname "$(readlink -f "$wrapped_marker")")"
else
	RAW_TMUX_DIR="$(dirname "$TMUX_BIN")"
fi

# Synthetic agent-like pane: title churn + a burst of output + a spinner
# line every 50ms -- the shape of load a real Claude Code pane produces.
cat >"$W/agent.sh" <<'EOF'
#!/usr/bin/env bash
i=0
while :; do
	i=$((i + 1))
	printf '\033]2;\xe2\x9c\xb3 task %d\007' "$i"
	head -c 1500 /dev/urandom | base64 -w 120
	printf '\033[2K\r\xe2\xa0\x8b thinking %d' "$i"
	sleep 0.05
done
EOF
chmod +x "$W/agent.sh"

new_agent_window() {
	# new_agent_window <session> -- a plain synthetic-agent window.
	"$TMUX_BIN" -L rem new-window -d -t "$1": "exec -a claude bash $W/agent.sh"
}

new_grid_window() {
	# new_grid_window <session> -- a 3-pane dispatcher grid: a lead pane
	# running the agent load, two role panes idling on cat.
	local sess=$1 wid panes
	wid="$("$TMUX_BIN" -L rem new-window -d -P -F '#{window_id}' -t "$sess": "exec -a claude bash $W/agent.sh")"
	"$TMUX_BIN" -L rem set-option -w -t "$wid" @crew_grid 1
	"$TMUX_BIN" -L rem split-window -d -t "$wid" cat
	"$TMUX_BIN" -L rem split-window -d -t "$wid" cat
	mapfile -t panes < <("$TMUX_BIN" -L rem list-panes -t "$wid" -F '#{pane_id}')
	"$TMUX_BIN" -L rem set-option -p -t "${panes[0]}" @crew_role lead
	"$TMUX_BIN" -L rem set-option -p -t "${panes[1]}" @crew_role reviewer
	"$TMUX_BIN" -L rem set-option -p -t "${panes[2]}" @crew_role reviewer
}

new_float_window() {
	# new_float_window <session> -- a window holding a stamped floating
	# pane, the shape every dispatched tool float takes (mkFloat in
	# generator/render/keys.go).
	local sess=$1 wid float
	wid="$("$TMUX_BIN" -L rem new-window -d -P -F '#{window_id}' -t "$sess": cat)"
	"$TMUX_BIN" -L rem new-pane -t "$wid" -x 60% -y 60% -X 20% -Y 20% -B heavy cat
	float="$("$TMUX_BIN" -L rem list-panes -t "$wid" -f '#{pane_floating_flag}' -F '#{pane_id}')"
	"$TMUX_BIN" -L rem set-option -p -t "$float" @float_geom '60% 60% 20% 20%'
}

daemon_sock_n=0
start_daemon() {
	# start_daemon <remote-session> <local-session>
	local remote_sess=$1 local_sess=$2 sock reflow
	daemon_sock_n=$((daemon_sock_n + 1))
	sock="$W/d${daemon_sock_n}.sock"
	reflow="$("$TMUX_BIN" -L loc show -gv @reflow_bin)"
	[ "$daemon_sock_n" -eq 1 ] && date +%s%N >"$W/attach_t0"
	PATH="$RAW_TMUX_DIR:$PATH" "$W/bin/daemon" \
		--test-local --src-socket rem --dst-socket loc \
		--session "$remote_sess" --window 1 --local-sess "$local_sess" \
		--renderer "$W/bin/renderer" --sock "$sock" --reflow "$reflow" \
		>>"$W/daemon.log" 2>&1 &
	echo $! >>"$W/daemon.pids"
}

# start_chain <load|noload> -- (re)build the remote+local halo from scratch.
start_chain() {
	local mode=$1
	if [ -f "$W/daemon.pids" ]; then
		while read -r pid; do kill "$pid" 2>/dev/null || true; done <"$W/daemon.pids"
	fi
	: >"$W/daemon.pids"
	for sock in rem loc obs obs2; do
		"$TMUX_BIN" -L "$sock" kill-server >/dev/null 2>&1 || true
	done
	daemon_sock_n=0
	: >"$W/daemon.log"
	sleep 0.3

	if [ "${VLOG:-0}" = 1 ]; then
		rm -f "$W"/vlog/*.log
		(cd "$W/vlog" && "$TMUX_BIN" -v -L rem new-session -d -s work -x 200 -y 50 cat)
	else
		"$TMUX_BIN" -L rem new-session -d -s work -x 200 -y 50 cat
	fi

	if [ "$mode" = load ]; then
		for _ in 1 2 3 4 5 6 7; do new_agent_window work; done
		for _ in 1 2 3; do new_grid_window work; done
		new_float_window work
		"$TMUX_BIN" -L rem new-session -d -s work2 -x 200 -y 50 cat
		for _ in 1 2 3 4; do new_agent_window work2; done
	fi

	"$TMUX_BIN" -L loc new-session -d -s mirror -x 200 -y 50
	"$TMUX_BIN" -L obs new-session -d -s obs -x 200 -y 52 "env -u TMUX $TMUX_BIN -L loc attach -t mirror"
	sleep 1
	start_daemon work mirror

	if [ "$mode" = load ]; then
		"$TMUX_BIN" -L loc new-session -d -s mirror2 -x 200 -y 50
		start_daemon work2 mirror2
	fi
}

# wait_for_bridge_pane -- sets LOCAL_PANE to the local mirror of remote %0
# (session work's cat pane), waiting up to 10s for the daemon to mirror it.
wait_for_bridge_pane() {
	local deadline=$((SECONDS + 10))
	LOCAL_PANE=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		LOCAL_PANE="$("$TMUX_BIN" -L loc list-panes -a -F '#{pane_id} #{@bridge_pane}' | awk '$2=="%0"{print $1}')"
		[ -n "$LOCAL_PANE" ] && return 0
		sleep 0.05
	done
	echo "keystroke-latency: timed out waiting for the bridge pane" >&2
	return 1
}

run_probe() {
	local out
	out="$("$W/bin/latencyprobe" -tmux "$TMUX_BIN" -L loc -t mirror -pane "$LOCAL_PANE" \
		-n "$SAMPLES" -interval 30ms -label "$1")"
	echo "$out"
	# An aborted probe means echoes stopped arriving at all — a bridge fault,
	# not latency; this chain's daemon log is the evidence.
	if [[ $out == *aborted=true* ]]; then
		echo "$1 daemon.log tail:"
		tail -n 8 "$W/daemon.log" | sed 's/^/  /'
	fi
}

# report_refit_forks <label> -- with VLOG=1, sum the refit-hook job_run lines
# across this run's remote -v log (each fork logs a start and an exit line).
report_refit_forks() {
	[ "${VLOG:-0}" = 1 ] || return 0
	local count jobs
	count="$(grep -hoE 'job_run: cmd=.*/tmux-(grid|float)-refit ' "$W"/vlog/*.log 2>/dev/null | wc -l)"
	jobs=$((count / 2))
	echo "$1 refit_jobs=$jobs (log lines=$count)"
}

scenario_quiet() {
	start_chain noload
	wait_for_bridge_pane
	sleep 12
	run_probe quiet
}

scenario_busy() {
	start_chain load
	wait_for_bridge_pane
	sleep 12
	run_probe busy
	report_refit_forks busy
}

scenario_churn() {
	start_chain load
	wait_for_bridge_pane
	sleep 12
	(
		for _ in $(seq 1 12); do
			wid="$("$TMUX_BIN" -L rem new-window -d -P -F '#{window_id}' -t work: "exec -a claude bash $W/agent.sh")"
			sleep 0.6
			"$TMUX_BIN" -L rem kill-window -t "$wid" 2>/dev/null || true
			sleep 0.6
		done
	) &
	local churn_pid=$!
	run_probe churn
	wait "$churn_pid" 2>/dev/null || true
}

scenario_attach() {
	rm -f "$W/attach_t0"
	start_chain load &
	local chain_pid=$! t0 watch_pid
	# Both times count from the first daemon's launch (attach_t0), not from
	# building the synthetic remote load.
	until [ -s "$W/attach_t0" ]; do sleep 0.05; done
	t0=$(<"$W/attach_t0")
	# Timed in the background: the probe below runs for longer than the
	# mirror takes to fill, so a check after it would only time the probe.
	(
		local deadline=$((SECONDS + 30))
		while [ "$SECONDS" -lt "$deadline" ]; do
			if [ "$("$TMUX_BIN" -L loc list-windows -t mirror 2>/dev/null | wc -l)" -ge 12 ]; then
				echo "attach all_mirrored_ms=$((($(date +%s%N) - t0) / 1000000))"
				exit 0
			fi
			sleep 0.05
		done
		echo "attach all_mirrored_ms=timeout"
	) &
	watch_pid=$!
	wait_for_bridge_pane
	echo "attach first_pane_ms=$((($(date +%s%N) - t0) / 1000000))"
	run_probe attach
	wait "$chain_pid"
	wait "$watch_pid" 2>/dev/null || true
}

scenario_reattach() {
	start_chain load
	wait_for_bridge_pane
	sleep 12
	(
		for _ in 1 2 3; do
			"$TMUX_BIN" -L obs2 new-session -d -s obs2 -x 180 -y 45 "env -u TMUX $TMUX_BIN -L loc attach -t mirror"
			sleep 2
			"$TMUX_BIN" -L obs2 kill-server >/dev/null 2>&1 || true
			sleep 2
		done
	) &
	local reattach_pid=$!
	run_probe reattach
	wait "$reattach_pid" 2>/dev/null || true
	report_refit_forks reattach
}

if [ "$#" -eq 0 ]; then
	set -- quiet busy churn attach reattach
fi
for scenario in "$@"; do
	case "$scenario" in
	quiet) scenario_quiet ;;
	busy) scenario_busy ;;
	churn) scenario_churn ;;
	attach) scenario_attach ;;
	reattach) scenario_reattach ;;
	*)
		echo "keystroke-latency: unknown scenario '$scenario'" >&2
		exit 1
		;;
	esac
done
