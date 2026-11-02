#!/usr/bin/env bash
# tmux calls made by idle agent-detect watchers (#1000). Builds a scratch
# server with no client attached and no config (so no sweep or hook adds calls
# of its own), runs a manifest-known agent command in N panes, arms
# agent-detect on each the way the sweep does (`pipe-pane -o`), then counts
# the command clients the server accepts while the watchers idle, across a
# series of window resizes, and across a 10 Hz resize drag; also the resident
# size of the tmux clients the watchers wait on. See docs/agents/performance.md.
#
# Usage: tests/perf/agent-detect-forks.sh <agent-detect binary>
# Env: TMUX_BIN (default ./result/bin/tmux; its raw tmux is what runs),
#      PANES (default 8), IDLE (seconds, default 60), RESIZES (default 10),
#      DRAG (seconds, default 3).
set -euo pipefail

AD_BIN="$(realpath "${1:?usage: $0 <agent-detect binary>}")"
TMUX_BIN="$(realpath "${TMUX_BIN:-./result/bin/tmux}")"
PANES="${PANES:-8}"
IDLE="${IDLE:-60}"
RESIZES="${RESIZES:-10}"
DRAG="${DRAG:-3}"

# The wrapper loads the full config; resolve the raw binary it execs so the
# server runs with -f /dev/null, and put it first on PATH, where agent-detect
# finds its own `tmux`.
RAW="$(PATH="$(sed -n "s/^PATH='\([^']*\)'.*/\1/p" "$TMUX_BIN" | head -1):$PATH" command -v tmux)"
[[ -x $RAW ]] || RAW="$TMUX_BIN"

W="$(mktemp -d /tmp/og-adf.XXXX)"
export TMUX_TMPDIR="$W"
export CLAUDE_STATUS_DIR="$W/status"
unset TMUX
# shellcheck disable=SC2174  # only the root itself must be owner-only
mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
mkdir -p "$W/bin" "$W/vlog"
# A pane whose foreground command is named codex: comm is the basename of the
# file exec'd, so a copy of bash blocked in its `read` builtin reads as codex
# to pane_current_command. (A symlink to coreutils' multi-call binary would
# dispatch on the name and exit.)
cp "$(command -v bash)" "$W/bin/codex"
PATH="$(dirname "$RAW"):$PATH"
export PATH

# The default socket inside the private TMUX_TMPDIR, never -L: agent-detect's
# own bare `tmux` calls resolve the default socket, so a named one would leave
# every watcher talking to no server.
t() { "$RAW" -f /dev/null "$@"; }
cleanup() {
	t kill-server >/dev/null 2>&1 || true
	rm -rf "$W"
}
trap cleanup EXIT INT TERM

(cd "$W/vlog" && "$RAW" -f /dev/null -v new-session -d -s adf -x 200 -y 50 "$W/bin/codex -c 'read -r _'")
for ((i = 1; i < PANES; i++)); do
	t new-window -t adf "$W/bin/codex -c 'read -r _'"
done
sleep 0.5
mapfile -t panes < <(t list-panes -s -t adf -F '#{pane_id}')
for p in "${panes[@]}"; do
	t pipe-pane -o -t "$p" "$AD_BIN ${p#%}"
done

deadline=$((SECONDS + 10))
until (($(find "$CLAUDE_STATUS_DIR/watchers" -type f 2>/dev/null | wc -l) >= PANES)); do
	((SECONDS < deadline)) || {
		echo "agent-detect watchers never registered" >&2
		exit 1
	}
	sleep 0.2
done
sleep 2

log="$(echo "$W"/vlog/tmux-server-*.log)"
clients() { grep -c 'new client' "$log" || true; }

# Watcher pids come from the registry (line 1); the tmux client a watcher
# waits on is its child process.
watcher_pids() {
	local f pid
	for f in "$CLAUDE_STATUS_DIR"/watchers/*; do
		[[ -f $f ]] || continue
		read -r pid <"$f" || continue
		[[ -d /proc/$pid ]] && echo "$pid"
	done
}
waiter_pids() {
	local pid
	while read -r pid; do
		cat /proc/"$pid"/task/*/children 2>/dev/null | tr ' ' '\n' | sed '/^$/d'
	done < <(watcher_pids)
}
# Sets REPLY to "<n> <summed VmRSS kB>" for the pids on stdin.
rss_of() {
	local pid kb n=0 sum=0
	while read -r pid; do
		kb=$(awk '/^VmRSS:/ { print $2 }' "/proc/$pid/status" 2>/dev/null) || continue
		[[ -n $kb ]] || continue
		n=$((n + 1))
		sum=$((sum + kb))
	done
	REPLY="$n $sum"
}

c0=$(clients)
sleep "$IDLE"
if [[ -r /proc/self/status ]]; then
	rss_of < <(watcher_pids)
	watchers_rss=$REPLY
	rss_of < <(waiter_pids)
	waiters_rss=$REPLY
else
	watchers_rss=n/a
	waiters_rss=n/a
fi
c1=$(clients)
idle=$((c1 - c0))

wins=$(t list-windows -t adf -F '#{window_id}')
c2=$(clients)
for ((r = 0; r < RESIZES; r++)); do
	for w in $wins; do
		t resize-window -t "$w" -x $((150 + r % 2)) -y 40
	done
	sleep 1
done
sleep 1
c3=$(clients)
# Our own resize-window calls are clients too; take them out.
resize=$((c3 - c2 - RESIZES * PANES))

# A drag delivers resizes at 10 Hz; same-size resizes are no-ops, so alternate.
c4=$(clients)
ticks=$((DRAG * 10))
for ((r = 0; r < ticks; r++)); do
	for w in $wins; do
		t resize-window -t "$w" -x $((150 + r % 2)) -y 40
	done
	sleep 0.1
done
sleep 2
c5=$(clients)
drag=$((c5 - c4 - ticks * PANES))

printf 'agent-detect %s\n' "$AD_BIN"
awk -v p="$PANES" -v s="$IDLE" -v n="$idle" 'BEGIN { printf "panes=%d idle=%ds: %d tmux calls (%.2f/s)\n", p, s, n, n / s }'
printf 'resizes=%d (each window, 1 s apart): %d tmux calls by watchers\n' "$RESIZES" "$resize"
printf 'drag=%ds (10 Hz, each window, +2 s settle): %d tmux calls by watchers\n' "$DRAG" "$drag"
if [[ $waiters_rss == n/a ]]; then
	echo "waiter clients=n/a rss=n/a"
	echo "watchers n=n/a rss=n/a"
else
	read -r n kb <<<"$waiters_rss"
	printf 'waiter clients=%d rss=%d kB\n' "$n" "$kb"
	read -r n kb <<<"$watchers_rss"
	printf 'watchers n=%d rss=%d kB\n' "$n" "$kb"
fi
