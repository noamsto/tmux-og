#!/usr/bin/env bash
# Detach the remote-bridge mirror for a session: SIGTERM its daemon and let the
# daemon's own teardown drop the mirror. Non-destructive by design — the remote
# session only loses a control-mode client, so everything running on it keeps
# running and prefix+s reopens the bridge. The shell twin of the picker's
# stopBridgeDaemon (picker/remote.go), for the keybind path.
#
# og-remote-detach <session-name> [<window-id>]
#
# A placed mirror (@bridge_placed, --into) lives as windows inside the user's own
# session: only those windows may go, never the session. The keybind passes the
# window id, expanded at the keypress, so this run-shell -b script cannot race a
# window switch.
set -euo pipefail

sess="${1:-$(tmux display-message -p '#{session_name}')}"

# Trailing colon, not an =-prefix: display-message takes a target-PANE, where
# "=name" is not a session shorthand and resolves to nothing — silently, rc=0
# with an empty format. The colon form resolves the session unambiguously even
# when its name is numeric (CLAUDE.md, "Session Targeting Gotcha").
target="${2:-$sess:}"
IFS='|' read -r placed sock < <(tmux display-message -p -t "$target" '#{@bridge_placed}|#{@bridge_sock}')
if [[ -z $sock ]]; then
	tmux display-message "og-remote-detach: $sess is not a bridged session"
	exit 0
fi

pid=""
if [[ -r "${sock}.pid" ]]; then
	pid="$(<"${sock}.pid")"
fi

if [[ $placed == 1 ]]; then
	if [[ $pid =~ ^[0-9]+$ ]]; then
		kill -TERM -- "$pid" 2>/dev/null || true
		# The daemon's teardown kills its own windows; wait for them so the
		# fallback below only fires for a daemon that never will.
		for _ in {1..20}; do
			socks="$(tmux list-windows -a -F '#{@bridge_sock}')"
			grep -qxF -- "$sock" <<<"$socks" || exit 0
			sleep 0.1
		done
	fi
	# No live daemon (or wedged past the wait): kill the windows it left. The
	# window id never holds a "|", so everything after the first one is the sock.
	while IFS='|' read -r wid wsock; do
		if [[ $wsock == "$sock" ]]; then
			tmux kill-window -t "$wid" 2>/dev/null || true
		fi
	done < <(tmux list-windows -a -F '#{window_id}|#{@bridge_sock}')
	exit 0
fi

if [[ $pid =~ ^[0-9]+$ ]]; then
	kill -TERM -- "$pid" 2>/dev/null || true
	# The daemon's teardown kills the mirror session itself; wait for it so the
	# fallback below only fires for a daemon that never will.
	for _ in {1..20}; do
		tmux has-session -t "=$sess" 2>/dev/null || exit 0
		sleep 0.1
	done
fi

# No live daemon (already exited, or wedged past the wait): its teardown will
# never run, so the mirror would linger with a dead renderer in every pane.
# Killing it here costs nothing — the remote is untouched on either path.
tmux kill-session -t "=$sess" 2>/dev/null || true
