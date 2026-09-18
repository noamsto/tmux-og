#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
bats_require_minimum_version 1.5.0 # run !
# Behavioural coverage for the tool binds (#679): `prefix + p/g/y` must focus
# the float the window already holds for that tool instead of stacking another
# one at the same geometry. new-pane -A is a Z-ORDER flag — the float stays
# visible above a zoomed pane — not attach-if-exists, so nothing about the old
# bind deduped and every press added a pane.
#
# Why not a conf grep: the project's bind checks read the generated text, which
# can only say the reuse gate is there. #679 is about what tmux DOES with that
# text, so the observer here is a running server driven by a real keypress.
#
# Two things this harness must get right or it cannot fail:
#   - A key binding fires only for an ATTACHED CLIENT. `send-keys` alone reaches
#     the pane's process, never the key table, so the client here is a second
#     `-L` server whose pane runs a real `tmux attach` and receives the keys.
#   - The stub command the float runs must OUTLIVE the press: the bind pins
#     remain-on-exit off, so a command that exits closes its own pane and the
#     assertion would read zero floats on the fixed tree as well as the broken
#     one — a red run that proves nothing.
#
# The bridge gate is off in this scratch server (@bridge_win/@bridge_pane never
# get stamped), so the press takes the LOCAL branch, which is the leg this file
# owns; the remote leg is covered by the daemon's own live-tmux test.

setup() {
	IN="lz679-in-${BATS_TEST_NUMBER}-$$"
	OUT="lz679-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# The poller and sweep hooks fire inside this test server, and the sweep
	# reaches functions that delete files under these dirs — whose defaults are
	# the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	# The yazi bind runs the bare name, so a stub must win on PATH.
	mkdir -p "$BATS_TEST_TMPDIR/bin"
	cat >"$BATS_TEST_TMPDIR/bin/yazi" <<-'EOF'
		#!/bin/sh
		exec sleep 600
	EOF
	chmod +x "$BATS_TEST_TMPDIR/bin/yazi"
	PATH="$BATS_TEST_TMPDIR/bin:$PATH"
	export PATH
	# tmux takes default-shell from $SHELL, and the pane's command runs under it.
	SHELL="$(command -v bash)"
	export SHELL

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat the keypress.
	inner set-option -g @splash_shown 1
	attach_client
	PREFIX="$(inner show-options -gv prefix)"
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
		[[ -n "$(inner list-clients -F '#{client_name}')" ]] && break
		sleep 0.1
	done
	[ -n "$(inner list-clients -F '#{client_name}')" ]
}

send() { outer send-keys -t "$OPANE" "$@"; }

press() { # key
	send "$PREFIX"
	sleep 0.2
	send "$1"
	sleep 0.5
}

# The pane id of the window's floating pane carrying that @pane_label, or empty.
labelled_float() { # label
	inner list-panes -a -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}' |
		awk -F'|' -v l="$1" '$2 == "1" && $3 == l { print $1 }'
}

dump() {
	inner list-panes -a -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}|#{pane_active}' >&2
}

@test "a second tool press focuses the open float instead of stacking another" {
	# The press has to take the local branch; a stray bridge stamp would route it
	# to the ctl verb and this file would assert nothing.
	[ "$(inner display-message -p -t s: '#{&&:#{@bridge_win},#{@bridge_pane}}')" != 1 ]

	press y
	local first
	first="$(labelled_float yazi)"
	[ -n "$first" ] || {
		echo "first press opened no yazi float" >&2
		dump
		false
	}

	# Move focus off the float, so the second press has to focus it rather than
	# merely inheriting the focus new-pane left behind.
	local base
	base="$(inner list-panes -a -F '#{pane_id}|#{pane_floating_flag}' | awk -F'|' '$2 == "0" { print $1; exit }')"
	[ -n "$base" ] || {
		echo "no non-floating pane to focus" >&2
		dump
		false
	}
	inner select-pane -t "$base"

	press y
	# One labelled float, and it is the pane the first press opened: a second
	# float would be a different pane id, and the count would be 2.
	[ "$(labelled_float yazi)" = "$first" ] || {
		echo "second press stacked another float" >&2
		dump
		false
	}
	[ "$(inner display-message -p -t "$first" '#{pane_active}')" = 1 ] || {
		echo "second press did not focus the open float" >&2
		dump
		false
	}
}
