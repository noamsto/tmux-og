#!/usr/bin/env bats
# The bell producer's MECHANISM, on the pinned next-3.8 tmux (mkTmux in
# flake.nix, never pkgs.tmux), on a private -L socket with -f /dev/null. This is
# what makes "bell notifies" verified rather than assumed: the generated-conf
# check asserts the hook is wired, this asserts that a real bell fires it.
#
# No client attaches and none is needed: alert-bell fires with zero clients, so
# the event lands on the history path. Covers BELL ONLY — monitor-activity stays
# off by design, so the activity path has no live trigger; its wiring is covered
# by the conf check instead.
#
# TMUX_TMPDIR is a short per-run /tmp dir: `tmux -L` resolves to
# "$TMUX_TMPDIR/tmux-<uid>/<name>" and a long bats tmpdir blows the 108-char
# unix socket limit. The socket name is per-run so this never adopts a server
# left by another run.

load helper

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"
	OG_TMUX_DIR="/tmp/og-notify-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX TMUX_PANE

	SOCK="notify-$$-${BATS_TEST_NUMBER}"
	REAL_TMUX="$(command -v tmux)"
	TM=("$REAL_TMUX" -L "$SOCK" -f /dev/null)

	# The router calls a bare `tmux` (correct in production). Pin that call to
	# THIS server with a shim on PATH, exported before the server starts so
	# run-shell children inherit it: without it a run-shell child with no TMUX
	# would fall back to the default socket and could reach the live server.
	SHIM="$BATS_TEST_TMPDIR/shim"
	mkdir -p "$SHIM"
	cat >"$SHIM/tmux" <<-EOF
		#!/bin/sh
		exec "$REAL_TMUX" -L "$SOCK" "\$@"
	EOF
	chmod +x "$SHIM/tmux"
	export PATH="$SHIM:$PATH"

	# Export before the first tmux command: the server snapshots this env.
	export OG_NOTIFY_DIR="$BATS_TEST_TMPDIR/notify"
	make_notify_router
}

teardown() {
	[ -n "${SOCK:-}" ] && "$REAL_TMUX" -L "$SOCK" kill-server 2>/dev/null
	rm -rf "${OG_TMUX_DIR:-}"
	return 0
}

@test "a real bell in a background window writes an event file" {
	"${TM[@]}" new-session -d -s s1 -x 80 -y 24
	# Same shape the generated conf uses: indexed hook, run-shell -b, the router
	# invoked with --window '#{window_id}' (never #{session_id}: run-shell's
	# sh -c would re-expand its leading $).
	"${TM[@]}" set-hook -g "alert-bell[20]" \
		"run-shell -b \"bash $NOTIFY_ROUTER emit --source bell --level warn --window #{window_id} --title bell\""
	"${TM[@]}" new-window -d "printf '\a'; sleep 10"

	found=""
	for _ in $(seq 1 80); do
		for f in "$OG_NOTIFY_DIR"/events/*; do
			[ -f "$f" ] && found="$f" && break
		done
		[ -n "$found" ] && break
		sleep 0.1
	done
	[ -n "$found" ]
	cat "$found" >&2 # dump on failure so a future flake is diagnosable
	grep -qx 'source=bell' "$found"
	grep -qx 'level=warn' "$found"
	grep -qE '^window=@[0-9]+$' "$found"
	grep -qx 'routed=history' "$found" # zero clients attached
}
