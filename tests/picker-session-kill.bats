#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} fixtures are literal tmux formats
bats_require_minimum_version 1.5.0 # run !
# Killing the session the picker was opened from (#884). The picker float lives
# in the client's window, so killing that window takes the float with it and
# `display-popup` exits 129 (128+SIGHUP). The launcher must treat that as the
# clean end of a kill the picker itself performed — not a `returned 129` on the
# binding — while a genuine picker failure still exits non-zero.
#
# Same attached-client harness as tests/popup-float.bats: a key binding fires
# only for an attached client, so the outer server's pane runs a real
# `tmux attach`.

setup() {
	IN="psk884-in-${BATS_TEST_NUMBER}-$$"
	OUT="psk884-out-${BATS_TEST_NUMBER}-$$"
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
	# tmux takes default-shell from $SHELL, and the pane's command runs under it.
	SHELL="$(command -v bash)"
	export SHELL

	inner new-session -d -s s -x 200 -y 50
	# A second session survives the kill, so the client has somewhere to land
	# and the outer pane can still be read back afterwards.
	inner new-session -d -s other -x 200 -y 50
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

# The pane id of the modal float in window WIN, or empty.
modal_float() { # win
	local win="$1" got deadline
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		got="$(inner list-panes -t "$win" -F '#{pane_id}|#{pane_modal_flag}' 2>/dev/null |
			awk -F'|' '$2 == "1" { print $1 }')"
		[ -n "$got" ] && {
			printf '%s' "$got"
			return 0
		}
		sleep 0.1
	done
	return 1
}

@test "killing the current session from the session picker paints no run-shell error" {
	local win deadline
	win="$(inner display-message -p -t s: '#{window_id}')"

	press s
	modal_float "$win" >/dev/null || {
		echo "prefix + s opened no modal float" >&2
		inner list-panes -a -F '#{pane_id}|#{pane_modal_flag}|#{window_id}' >&2
		false
	}

	send C-x

	# The picker kills the session it was opened from; wait for it to go.
	deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		inner has-session -t '=s' 2>/dev/null || break
		sleep 0.1
	done
	run inner has-session -t '=s'
	[ "$status" -ne 0 ] || {
		echo "session s survived ctrl+x" >&2
		false
	}

	# detach-on-destroy off lands the client on the surviving session.
	deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[ "$(inner list-clients -F '#{client_session}')" = other ] && break
		sleep 0.1
	done
	[ "$(inner list-clients -F '#{client_session}')" = other ] || {
		echo "client did not land on 'other'" >&2
		inner list-clients -F '#{client_session}' >&2
		false
	}

	# The regression: run-shell prints `'…tmux-session-picker …' returned 129`.
	# Bounded wait for the message to paint, then assert it never did.
	sleep 1
	local screen
	screen="$(outer capture-pane -p -t "$OPANE")"
	[[ $screen != *returned* ]] || {
		echo "run-shell error painted on the client:" >&2
		printf '%s\n' "$screen" >&2
		false
	}
}

@test "killing the current window from the window picker paints no run-shell error" {
	local win deadline
	# Session s needs a second window so the picker has more than one to show.
	inner new-window -d -t s: -n two
	win="$(inner display-message -p -t s:1 '#{window_id}')"

	press w
	modal_float "$win" >/dev/null || {
		echo "prefix + w opened no modal float" >&2
		inner list-panes -a -F '#{pane_id}|#{pane_modal_flag}|#{window_id}' >&2
		false
	}

	# Ctrl+x kills the selected window in the window picker. The selected window
	# is @1 (the popup's host window), so the popup is destroyed.
	send C-x

	# detach-on-destroy off lands the client on the surviving window (window 2).
	deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[ "$(inner display-message -p -t s: '#{window_id}')" != "$win" ] && break
		sleep 0.1
	done
	[ "$(inner display-message -p -t s: '#{window_id}')" != "$win" ] || {
		echo "window $win survived ctrl+x" >&2
		inner list-windows -t s: -F '#{window_id}' >&2
		false
	}

	# The regression: run-shell prints `'…tmux-window-picker …' returned 129`.
	# Bounded wait for the message to paint, then assert it never did.
	sleep 1
	local screen
	screen="$(outer capture-pane -p -t "$OPANE")"
	[[ $screen != *returned* ]] || {
		echo "run-shell error painted on the client:" >&2
		printf '%s\n' "$screen" >&2
		false
	}
}

@test "the wrapper exits 0 when its own host window is killed while the window picker popup is open" {
	local client other_pane rcfile deadline
	client="$(inner list-clients -F '#{client_name}')"
	inner new-window -d -t s: -n two
	other_pane="$(inner list-panes -t s:2 -F '#{pane_id}' | head -1)"
	rcfile="$BATS_TEST_TMPDIR/rc"

	# Drive the production launcher from window 2, targeting the client on s.
	# The popup opens in window 1 (the client's window).
	inner send-keys -t "$other_pane" \
		"tmux-window-picker --client '$client'; echo rc=\$? >'$rcfile'" Enter

	# Wait for the popup to open, then kill its host window under it.
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		[ "$(inner list-panes -a -F '#{pane_modal_flag}' | grep -c '^1' || true)" -ge 1 ] && break
		sleep 0.1
	done
	inner kill-window -t s:1

	deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)) && [ ! -s "$rcfile" ]; do
		sleep 0.1
	done
	[ -s "$rcfile" ] || {
		echo "wrapper recorded no exit status" >&2
		false
	}
	grep -qx 'rc=0' "$rcfile" || {
		echo "wrapper exited non-zero: $(cat "$rcfile")" >&2
		false
	}
}

@test "the wrapper exits 0 when its own host session is killed while the popup is open" {
	local client other_pane rcfile deadline
	client="$(inner list-clients -F '#{client_name}')"
	other_pane="$(inner list-panes -t other -F '#{pane_id}' | head -1)"
	rcfile="$BATS_TEST_TMPDIR/rc"

	# Drive the production launcher from a surviving session's pane, targeting
	# the client on s. This is the run-shell path with the popup pinned to s.
	inner send-keys -t "$other_pane" \
		"tmux-session-picker --client '$client' --current s; echo rc=\$? >'$rcfile'" Enter

	# Wait for the popup to open, then kill its host session under it.
	deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		[ "$(inner list-panes -a -F '#{pane_modal_flag}' | grep -c '^1' || true)" -ge 1 ] && break
		sleep 0.1
	done
	inner kill-session -t '=s'

	deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)) && [ ! -s "$rcfile" ]; do
		sleep 0.1
	done
	[ -s "$rcfile" ] || {
		echo "wrapper recorded no exit status" >&2
		false
	}
	grep -qx 'rc=0' "$rcfile" || {
		echo "wrapper exited non-zero: $(cat "$rcfile")" >&2
		false
	}
}
