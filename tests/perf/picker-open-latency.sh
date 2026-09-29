#!/usr/bin/env bash
# Picker open latency (#874): launch -> first list frame, session and window
# mode, on a scratch tmux server with a realistic fixture. See
# docs/agents/performance.md ("Picker open latency").
#
# Usage: tests/perf/picker-open-latency.sh [session|window|forks|keys|fixture]...
#   default: session window forks keys
#   keys     keypress -> repaint of the running picker, both modes: cursor
#            moves, typing a filter, a toggle, and a held-key burst (picker/keyprobe)
#   fixture  builds the scratch server, writes `export` lines for it to
#            $FIXTURE_ENV_FILE (default stdout), and waits (^C).
#            tests/perf/gotorque-picker.sh runs its campaign against it.
# Env: PICKER_BIN  picker binary under test (default: built from ./picker)
#      OPENS       opens per mode (default 30)
#      KEYS        samples per key scenario (default 100)
#      FIXTURE_AGENTS  agent panes in the fixture (default 3; agent state goes
#                  stale with time, so the gotorque fixture uses 0)
#      TMUX_BIN    wrapped tmux under test (default ./result/bin/tmux); its raw
#                  binary drives the server and sits first on the picker's PATH,
#                  as it does for a popup (`show -F` needs tmux-next, not stock)
# Milestones come from OG_PICKER_TRACE (picker/trace.go), timed from a launcher
# stamp taken in the pane's shell, like the real launcher's. The keypress ->
# display-popup leg is tmux's and is not measured. Records `uptime` per run:
# the numbers only compare within one load reading.
# Linux only: GNU date/tar, strace, `sleep infinity`, bash 4 associative arrays.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OPENS="${OPENS:-30}"
KEYS="${KEYS:-100}"
TMUX_BIN="$(realpath "${TMUX_BIN:-./result/bin/tmux}")"
wrapped_marker="$(dirname "$TMUX_BIN")/.tmux-wrapped"
if [ -e "$wrapped_marker" ]; then
	RAW_TMUX="$(readlink -f "$wrapped_marker")"
else
	RAW_TMUX="$TMUX_BIN"
fi
RAW_TMUX_DIR="$(dirname "$RAW_TMUX")"
export PATH="$RAW_TMUX_DIR:$PATH"

W="$(mktemp -d /tmp/og-picker-perf.XXXX)"
# Built before HOME moves into $W: go would otherwise fill a module cache there.
if [ -z "${PICKER_BIN:-}" ]; then
	PICKER_BIN="$W/picker"
	(cd "$REPO_ROOT/picker" && go build -o "$PICKER_BIN" .)
fi
KEYPROBE="$W/keyprobe"
(cd "$REPO_ROOT/picker" && go build -o "$KEYPROBE" ./keyprobe)
export TMUX_TMPDIR="$W"
export CLAUDE_STATUS_DIR="$W/status"
export ZOXIDE_DATA_DIR="$W/zoxide"
export HOME="$W/home"
export GIT_CONFIG_GLOBAL="$W/gitconfig"
export GIT_CONFIG_SYSTEM=/dev/null
unset TMUX TMUX_PANE
# shellcheck disable=SC2174  # only the root itself must be owner-only
mkdir -p -m 700 "$CLAUDE_STATUS_DIR/panes"
mkdir -p "$ZOXIDE_DATA_DIR" "$HOME"
tm() { "$RAW_TMUX" -L probe "$@"; }
cleanup() {
	jobs -p | xargs -r kill 2>/dev/null || true
	tm kill-server >/dev/null 2>&1 || true
	rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Fixture: 8 sessions, 24 windows. Half the windows sit in git repos with no
# @branch stamp (the picker forks git for them), the rest in plain dirs.
# 6 repos, 4 plain dirs, 3 agent panes, 12 zoxide entries.
build_fixture() {
	local i sess
	git config --global user.email t@t
	git config --global user.name t
	git config --global init.defaultBranch main
	for i in 1 2 3 4 5 6; do
		mkdir -p "$W/repos/r$i"
		git -C "$W/repos/r$i" init -q
		git -C "$W/repos/r$i" commit -q --allow-empty -m init
		git -C "$W/repos/r$i" checkout -q -b "feat/$((100 + i))-work"
	done
	for i in 1 2 3 4; do mkdir -p "$W/plain/p$i"; done
	for i in $(seq 1 12); do
		mkdir -p "$W/zx/z$i"
		zoxide add "$W/zx/z$i"
	done

	# Panes with something to preview: 40 coloured lines, then idle on cat.
	cat >"$W/content.sh" <<'CONTENT'
i=0
while [ "$i" -lt 40 ]; do
	printf '\033[32mline %s\033[0m  some build output that fills a preview row\n' "$i"
	i=$((i + 1))
done
exec cat
CONTENT
	tm -f /dev/null new-session -d -s s1 -x 200 -y 50 -c "$W/repos/r1" "sh $W/content.sh"
	tm set -g default-size 200x50
	tm set -g remain-on-exit off
	tm set -g @thm_overlay_1 '#7f849c'
	for i in 2 3 4 5 6 7 8; do
		tm new-session -d -s "s$i" -x 200 -y 50 -c "$W/plain/p$(((i % 4) + 1))" "sh $W/content.sh"
	done
	# 16 more windows spread round-robin, alternating repo and plain cwds.
	for i in $(seq 1 16); do
		sess="s$(((i % 8) + 1))"
		if ((i % 2)); then
			tm new-window -d -t "$sess:" -c "$W/repos/r$(((i % 6) + 1))" "sh $W/content.sh"
		else
			tm new-window -d -t "$sess:" -c "$W/plain/p$(((i % 4) + 1))" "sh $W/content.sh"
		fi
	done
	# Agent panes: the status files the picker aggregates per session/window.
	local now pane
	now=$(date +%s)
	for sess in $(printf '%s\n' s1 s3 s5 | head -n "${FIXTURE_AGENTS:-3}"); do
		pane=$(tm list-panes -t "$sess:" -F '#{pane_id}' | head -1)
		printf 'state=processing\nsession=%s\ntimestamp=%s\n' "$sess" "$now" >"$CLAUDE_STATUS_DIR/panes/$pane"
	done
	sleep 0.5 # let the last new-window settle before anything is timed
}

launch_open() { # mode outfile
	local flags="--tui" mode=$1 out=$2
	[ "$mode" = window ] && flags="--tui --windows"
	rm -f "$out"
	tm new-window -d -t s1: "sh -c 'OG_PICKER_TRACE=$out OG_PICKER_T0=\$(date +%s%N) exec $PICKER_BIN $flags'" >/dev/null
	local i
	for i in $(seq 1 200); do
		grep -q '^paint' "$out" 2>/dev/null && return 0
		sleep 0.05
	done
	echo "no first frame for $mode" >&2
	return 1
}

ms_of() { awk -v k="$2" '$1==k{printf "%.1f\n", $2/1000}' "$1"; }

stats() { # label values...
	local label=$1
	shift
	if [ $# -eq 0 ]; then
		echo "  $label: no samples (event missing from the trace)" >&2
		return 0
	fi
	printf '%s\n' "$@" | sort -n | awk -v l="$label" '{a[NR]=$1} END{
		p95=int(NR*0.95); if (p95<1) p95=1
		printf "  %-14s n=%d median=%.1f p95=%.1f min=%.1f max=%.1f\n", l, NR, a[int((NR+1)/2)], a[p95], a[1], a[NR]}'
}

run_mode() {
	local mode=$1 k out="$W/trace.$1" ev
	declare -A vals
	echo "== $mode mode: $OPENS opens ($(uptime | sed 's/.*load/load/'))"
	launch_open "$mode" "$out" # warm-up: page cache, zoxide db
	for ((k = 0; k < OPENS; k++)); do
		launch_open "$mode" "$out"
		for ev in start tmux agent_panes items model run init window_size first_frame paint; do
			vals[$ev]+="$(ms_of "$out" $ev) "
		done
	done
	for ev in start tmux agent_panes items model run init window_size first_frame paint; do
		# shellcheck disable=SC2086  # word-split the collected samples
		stats "$ev" ${vals[$ev]}
	done
}

# Count execs before the first frame, by binary, from one strace'd open.
run_forks() {
	local mode=$1 out="$W/trace.forks.$1" st="$W/strace.$1" flags="--tui"
	[ "$mode" = window ] && flags="--tui --windows"
	command -v strace >/dev/null || {
		echo "strace not found; skipping forks" >&2
		return 0
	}
	rm -f "$out" "$st"
	local t0
	t0=$(date +%s%N)
	tm new-window -d -t s1: "sh -c 'OG_PICKER_TRACE=$out OG_PICKER_T0=$t0 exec strace -f -ttt -e trace=execve -o $st $PICKER_BIN $flags'" >/dev/null
	local i
	for i in $(seq 1 200); do
		grep -q '^paint' "$out" 2>/dev/null && break
		sleep 0.05
	done
	sleep 0.2
	local ff_us
	ff_us=$(awk '$1=="paint"{print $2}' "$out")
	if [ -z "$ff_us" ]; then
		echo "forks ($mode): no paint recorded" >&2
		return 1
	fi
	echo "== $mode mode: execs before first paint (strace, load-inflated)"
	awk -v t0="$t0" -v ff="$ff_us" '
		/execve\(/ && !/resumed/ && !/ENOENT/ {
			ts=$2*1000000 - t0/1000
			if (ts > ff) next
			match($0, /execve\("[^"]*"/); bin=substr($0, RSTART+8, RLENGTH-9)
			n=split(bin, p, "/"); c[p[n]]++; tot++
		}
		END { for (b in c) printf "  %-12s %d\n", b, c[b]; printf "  %-12s %d\n", "TOTAL", tot }' "$st" | sort
}

# Keypress -> repaint of a running picker (picker/keyprobe). "first" is the
# repaint the key caused, "settled" also covers what lands later (a preview).
run_keys() {
	local mode=$1 flags="--tui" pane toggle=C-a                # agent-only filter
	[ "$mode" = window ] && flags="--tui --windows" toggle=C-g # group by state
	tm new-window -d -t s1: -n keys "sh -c 'exec $PICKER_BIN $flags'"
	pane=$(tm list-panes -t s1:keys -F '#{pane_id}')
	sleep 1
	echo "== $mode mode keys: $KEYS samples per scenario ($(uptime | sed 's/.*load/load/'))"
	probe() { "$KEYPROBE" -tmux "$RAW_TMUX" -L probe -t s1 -size 200x50 -pane "$pane" -label "$1" "${@:2}"; }
	probe "move  " -keys Down,Up -n "$KEYS"
	probe "type  " -keys a,b,BSpace,BSpace -n "$((KEYS / 4 * 4))" # ends on an empty query
	probe "toggle" -keys "$toggle" -n "$((KEYS / 2 * 2))"         # ends toggled back
	probe "burst " -keys Down,Down,Down,Down,Down,Down,Down,Down,Up,Up,Up,Up,Up,Up,Up,Up -burst 16 -n 10
	tm kill-window -t "s1:keys"
}

# Preview captures forked by one held-key burst (16 moves at 30 Hz), from an
# strace'd picker: each cursor move used to fork one capture-pane.
run_burst_forks() {
	local mode=$1 flags="--tui" pane st="$W/strace.burst.$1" before after
	command -v strace >/dev/null || return 0
	[ "$mode" = window ] && flags="--tui --windows"
	rm -f "$st"
	tm new-window -d -t s1: -n burst "sh -c 'exec strace -f -e trace=execve -o $st $PICKER_BIN $flags'"
	pane=$(tm list-panes -t s1:burst -F '#{pane_id}')
	sleep 1.5
	if ! grep -q 'execve' "$st" 2>/dev/null; then
		echo "burst forks ($mode): strace recorded nothing (ptrace blocked?)" >&2
		return 1
	fi
	before=$(grep -c 'execve.*capture-pane' "$st" || true)
	"$KEYPROBE" -tmux "$RAW_TMUX" -L probe -t s1 -size 200x50 -pane "$pane" -label "$mode" \
		-keys Down,Down,Down,Down,Down,Down,Down,Down,Up,Up,Up,Up,Up,Up,Up,Up -burst 16 -n 1 >/dev/null
	sleep 0.3
	after=$(grep -c 'execve.*capture-pane' "$st" || true)
	echo "== $mode mode: capture-pane forks for a 16-key burst at 30 Hz: $((after - before))"
	tm kill-window -t "s1:burst"
}

modes=("$@")
[ ${#modes[@]} -gt 0 ] || modes=(session window forks keys)

build_fixture
echo "picker: $PICKER_BIN   tmux: $("$RAW_TMUX" -V)"
echo "uptime: $(uptime)"
for m in "${modes[@]}"; do
	case $m in
	session | window) run_mode "$m" ;;
	forks)
		run_forks session
		run_forks window
		;;
	keys)
		run_keys session
		run_keys window
		run_burst_forks session
		run_burst_forks window
		;;
	fixture)
		# Built whole, then written in one step: a reader polling the file must
		# never see it half done.
		exports="$(
			printf 'export TMUX_TMPDIR=%q\n' "$TMUX_TMPDIR"
			printf 'export CLAUDE_STATUS_DIR=%q\n' "$CLAUDE_STATUS_DIR"
			printf 'export ZOXIDE_DATA_DIR=%q\n' "$ZOXIDE_DATA_DIR"
			printf 'export HOME=%q\n' "$HOME"
			printf 'export GIT_CONFIG_GLOBAL=%q\n' "$GIT_CONFIG_GLOBAL"
			printf 'export PATH=%q\n' "$PATH"
			printf 'export TMUX=%q\n' "$(tm display-message -p '#{socket_path},#{pid},0')"
		)"
		if [ -n "${FIXTURE_ENV_FILE:-}" ]; then
			printf '%s\n' "$exports" >"$FIXTURE_ENV_FILE.tmp"
			mv "$FIXTURE_ENV_FILE.tmp" "$FIXTURE_ENV_FILE"
		else
			printf '%s\n' "$exports"
		fi
		# Backgrounded and waited on: bash defers a trap while a foreground
		# child runs, so a plain sleep would outlive the kill that ends it.
		sleep infinity &
		wait "$!"
		;;
	*)
		echo "unknown mode $m" >&2
		exit 2
		;;
	esac
done
