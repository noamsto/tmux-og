#!/usr/bin/env bats
# shellcheck disable=SC2016,SC2009 # literal #{...}/$ fixtures must not expand here; the probe's
# positive control mirrors the binding's own `ps -o comm= -t <pane tty>` lookup
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for the `S-Enter` binding (#698): a real keypress,
# delivered by a real attached client, driving the real emitted conf, asserted on
# the BYTES each kind of pane receives.
#
# Why not a conf grep: `send-keys` reaches a pane's process but never the key
# table, so which branch a keypress takes is only observable with a client
# attached — and #698 is about the bytes (pi's alt+enter queues a follow-up,
# it does not insert a newline), not about the text being present.

setup() {
	IN="lz698-in-${BATS_TEST_NUMBER}-$$"
	OUT="lz698-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# tmux takes default-shell from $SHELL. Pin it so the pane's `ps -o comm=`
	# line is a shell that certainly exists in the sandbox.
	SHELL="$(command -v bash)"
	export SHELL
	# The poller and sweep monitor hooks fire inside this test server, and the
	# sweep reaches two functions that delete files under these dirs (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	# The binding branches on `ps -o comm= -t <pane tty>`, so the pane's process
	# has to BEAR the name: a symlink to sh is the cheapest process that does.
	BIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$BIN"
	local name
	for name in pi amp; do
		ln -s "$(command -v sh)" "$BIN/$name"
	done
	PROBE="$BATS_TEST_TMPDIR/probe.sh"
	cat >"$PROBE" <<-'EOF'
		#!/bin/sh
		# Create the file before cat reads it, so the test can wait for "armed"
		# instead of racing the pane's startup.
		stty raw -echo 2>/dev/null
		: >"$1"
		cat >"$1"
	EOF
	chmod +x "$PROBE"

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat the keys under test.
	inner set-option -g @splash_shown 1
	attach_client
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# A real attached client for the inner server: its keys come from a second
# server's pane, which is where a keypress can actually reach the key table.
attach_client() {
	outer new-session -d -x 200 -y 50 "env -u TMUX $TMUX_BIN -L $IN attach -t s"
	OPANE="$(outer list-panes -F '#{pane_id}' | head -1)"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && return 0
		sleep 0.1
	done
	printf 'the inner server never got an attached client\n' >&2
	return 1
}

# kitty's bytes for Shift+Enter, injected raw: the key table is what is under
# test, so the client's own encoder must not be in the path.
shift_enter() {
	outer send-keys -H -t "$OPANE" 1b 5b 31 33 3b 32 75
	sleep 0.3
}

probe_window() { # cmd -> prints "window_id pane_id"
	inner new-window -d -t s: -P -F '#{window_id} #{pane_id}' "$1"
}

arm_probe() { # target outfile expected-process-name
	local target="$1" out="$2" expect="$3" deadline=$((SECONDS + 10)) tty
	inner select-window -t "${target%% *}"
	inner select-pane -t "${target##* }"
	while ((SECONDS < deadline)); do
		[[ -e $out ]] && break
		sleep 0.1
	done
	[ -e "$out" ] || {
		printf 'probe pane never armed (%s missing)\n' "$out" >&2
		return 1
	}
	# The binding branches on this exact lookup, so a sandbox that cannot see
	# the pane's process name would otherwise fail every branch as a silent
	# "expected CSI-u, got Alt+Enter".
	tty="$(inner display -p -t "${target##* }" '#{pane_tty}')"
	ps -o comm= -t "$tty" | grep -qx -- "$expect" || {
		printf 'pane tty %s does not show %s: [%s]\n' \
			"$tty" "$expect" "$(ps -o comm= -t "$tty" | tr '\n' ' ')" >&2
		return 1
	}
}

wait_bytes() { # outfile expected-hex
	local deadline=$((SECONDS + 10)) got=""
	while ((SECONDS < deadline)); do
		got="$(od -An -tx1 -v "$1" 2>/dev/null | tr -d ' \n')"
		[[ $got == "$2" ]] && return 0
		[[ -n $got ]] && break
		sleep 0.1
	done
	printf 'expected %s, got %s\n' "$2" "${got:-<nothing>}" >&2
	return 1
}

@test "Shift+Enter reaches a pi pane as the raw CSI-u sequence" {
	out="$BATS_TEST_TMPDIR/pi.out"
	arm_probe "$(probe_window "$BIN/pi $PROBE $out")" "$out" pi
	shift_enter
	wait_bytes "$out" 1b5b31333b3275
}

@test "Shift+Enter still reaches an amp pane as backslash + Enter" {
	out="$BATS_TEST_TMPDIR/amp.out"
	arm_probe "$(probe_window "$BIN/amp $PROBE $out")" "$out" amp
	shift_enter
	wait_bytes "$out" 5c0d
}

@test "Shift+Enter still reaches any other pane as Alt+Enter" {
	out="$BATS_TEST_TMPDIR/other.out"
	arm_probe "$(probe_window "$SHELL $PROBE $out")" "$out" bash
	shift_enter
	wait_bytes "$out" 1b0d
}
