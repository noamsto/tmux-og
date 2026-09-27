#!/usr/bin/env bats
# shellcheck disable=SC2016,SC2089,SC2090 # the escaped $S_A/$S_B inside QUOTE/SUBST are the literal payload text a remote-derived name carries; they are expanded by run-shell's spawned shell at execution time, never by this file
bats_require_minimum_version 1.5.0 # run !
# Real-server coverage for #783: og-remote-open names a local mirror session
# `${host}-${sess}` straight from the remote, and tmux's stock
# MouseDown3StatusLeft/M-MouseDown3StatusLeft binds (still live in our config)
# format-expand that name twice over — once building the run-shell -C command,
# again drawing display-menu's title and its "Switch To" loop over every other
# session. A quote break-out or a `#(...)` in a remote session name therefore
# runs local commands on a right-click of the session pill. Same harness shape
# as tests/rename-bind-integration.bats (#367) and
# tests/menu-bind-integration.bats (#769) — a conf grep can only say the bind
# text is there; only a real attached client's real right-click can prove what
# tmux does with it.
#
# ssh/the bridge daemon/renderer/ctl/tmux-reflow-windows are fakes on PATH; the
# launcher (scripts/og-remote-open.sh, or $OPEN_SRC) is the real script, driving
# the real inner -L server through a `tmux` shim that execs the built wrapper.
#
# TMUX_TMPDIR is a fresh, short /tmp/og783.XXXXXX, not $BATS_TEST_TMPDIR: it is
# both the inner/outer servers' own socket dir and (via the launcher's
# sock_dir) the bridge daemon's socket dir, and a long path in either blows
# AF_UNIX's ~104-byte limit ("File name too long").

setup() {
	IN="msn783-in-${BATS_TEST_NUMBER}-$$"
	OUT="msn783-out-${BATS_TEST_NUMBER}-$$"
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	SHELL="$(command -v bash)"
	export SHELL
	# The poller and sweep monitor hooks fire inside this test server, and the
	# sweep reaches functions that delete files under these dirs — whose
	# defaults are the developer's real /tmp trees (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"

	OG_TMUX_DIR="$(mktemp -d /tmp/og783.XXXXXX)"
	export TMUX_TMPDIR="$OG_TMUX_DIR"

	# In the server's GLOBAL environment before it starts: run-shell's spawned
	# shell inherits it there, and no path characters ride inside a session
	# name (the names under test carry the literal text "$S_A"/"$S_B").
	export S_A="$BATS_TEST_TMPDIR/sa"
	export S_B="$BATS_TEST_TMPDIR/sb"
	export S_C="$BATS_TEST_TMPDIR/sc"

	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	# Execs the real wrapped tmux against the same inner -L server every
	# `inner` call in this file targets, so launcher-created state and
	# test-created state live on one server. NO_SWITCH stands in for "no
	# attached client to switch": the launcher's final switch-client would
	# otherwise abort it under `set -euo pipefail` (measured: "no current
	# client", exit 1).
	cat >"$FAKEBIN/tmux" <<-EOF
		#!/bin/sh
		if [ -n "\${NO_SWITCH:-}" ] && [ "\$1" = switch-client ]; then
			exit 0
		fi
		exec "$TMUX_BIN" -L "$IN" "\$@"
	EOF

	# A canned probe reply — this stub does not interpret the script it
	# receives, it only recognizes the launcher's fixed leading marker
	# (`: og-probe;`), same convention as tests/remote-cold-start.bats.
	cat >"$FAKEBIN/ssh" <<-'EOF'
		#!/bin/sh
		cmd="$*"
		case "$cmd" in
		*"bash -s"*) cmd="$cmd
		$(cat)" ;;
		esac
		case "$cmd" in
		*": og-probe;"*)
			printf 'os=Linux\nuid=1000\ntmux=/usr/bin/tmux\ntmpdir=/run/user/1000\nsess=%s\nwin=1\n' "$REMOTE_SESSION"
			exit 0
			;;
		*list-windows*) echo 1 ;;
		esac
		exit 0
	EOF

	for stub in og-remote-bridge-daemon og-remote-bridge-renderer tmux-reflow-windows; do
		printf '#!/bin/sh\nexit 0\n' >"$FAKEBIN/$stub"
	done
	cat >"$FAKEBIN/og-remote-bridge-ctl" <<-'EOF'
		#!/bin/sh
		if [ -n "${FAKE_CTL_ERROR:-}" ]; then
			printf '%s\n' "$FAKE_CTL_ERROR" >&2
			exit 1
		fi
		exit 0
	EOF

	chmod +x "$FAKEBIN"/*
	export PATH="$FAKEBIN:$PATH"

	# Same @lib_remote@ substitution Nix does at build time. OPEN_SRC points
	# this same suite at an older launcher, to check it goes red there.
	LAUNCHER="$BATS_TEST_TMPDIR/og-remote-open"
	sed "s|@lib_remote@|$PWD/scripts/lib-remote.sh|g" \
		"${OPEN_SRC:-scripts/og-remote-open.sh}" >"$LAUNCHER"
	export LAUNCHER

	# shellcheck source=/dev/null
	source "$PWD/scripts/lib-remote.sh"

	# The issue's two payloads (spec A1/A2): a quote break-out (executes when
	# format-expanded as the menu TITLE, or as a "Switch To" LOOP item) and a
	# `##(...)` command substitution (executes wherever it is format-expanded:
	# title or loop item — it is a self-contained format job, never breaking
	# the surrounding command syntax).
	#
	# QUOTE's tail is `run-shell 'touch $S_A' #`, not the issue's bare
	# `display-menu -T 'y`: the whole run-shell -C argument is ONE
	# semicolon-split command list, format-expanded once, so QUOTE's own text
	# also lands (raw) in the "Rename" item's `-I '#S'` prefill further down
	# the same bind. Reopening a nested display-menu there (the issue's literal
	# payload) makes THAT reconstructed command malformed ("argument N must be
	# \"string\""), which voids the WHOLE command list — nothing runs, not even
	# our own run-shell. A trailing `#` comments out everything after it
	# instead (measured: tmux's command parser, not the shell), which is
	# self-contained and fires regardless of what follows. The extra `''`
	# balances the loop item's own (name,key,command) triple when this same
	# text lands there instead of at the title.
	QUOTE="x' '' '' ; run-shell 'touch \$S_A' #"
	SUBST='x##(touch $S_B)'
	# The issue's literal quote payload, verbatim. It fires on a mirror
	# window, whose session menu (#769) has no Rename item for it to break.
	LITERAL="x' '' ; run-shell 'touch \$S_C' ; display-menu -T 'y"
	export QUOTE SUBST LITERAL

	inner new-session -d -s s -x 200 -y 50
	# The splash popup would eat the click and repaint the status line.
	inner set-option -g @splash_shown 1
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	[[ -n ${SLEEP_PID:-} ]] && kill "$SLEEP_PID" 2>/dev/null
	rm -rf "${OG_TMUX_DIR:-}" 2>/dev/null || true
	return 0
}

inner() { timeout --foreground 30s "$TMUX_BIN" -L "$IN" "$@"; }
outer() { timeout --foreground 30s "$TMUX_BIN" -L "$OUT" "$@"; }

# A real attached client for the inner server: its clicks come from a second
# server's pane, which is where a right-click can actually reach the key table.
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

# A right click (SGR mouse button 2) at a 1-based column/row. Raw bytes, not
# `-l`: the client's extended keys re-encode a literal ESC. Press only — the
# release would close the menu the press just opened.
click() { # col row
	# shellcheck disable=SC2046 # word-split on purpose: -H takes one hex byte per argument
	outer send-keys -t "$OPANE" -H $(printf '\e[<2;%d;%dM' "$1" "$2" | od -An -tx1)
	sleep 0.3
}

screen() { outer capture-pane -p -t "$OPANE"; }

# A menu opened via click is drawn asynchronously; poll instead of a
# single-shot screen grep.
wait_for_screen() { # expected text
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		screen | grep -qF "$1" && return 0
		sleep 0.1
	done
	printf 'timed out waiting for screen text "%s"; screen was:\n' "$1" >&2
	screen >&2
	return 1
}

# A `#(...)`/injected command is asynchronous, so "it never ran" is only a
# claim once the whole window has elapsed with nothing there.
wait_for_sentinel() { # path
	local deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		[[ -e $1 ]] && return 0
		sleep 0.1
	done
	return 1
}

switch_client_to() { # target (session id or exact name)
	inner switch-client -c "$(inner list-clients -F '#{client_name}')" -t "$1"
	sleep 0.3
}

# Flips the #769 mirror gate the session menu branches on, the way the daemon
# stamps a mirror: @bridge_win on the window, @bridge_pane on the pane.
gate_mirror() { # session target
	inner set-option -w -t "$1:" @bridge_win 1
	inner set-option -p -t "$(inner list-panes -t "$1:" -F '#{pane_id}' | head -1)" @bridge_pane '%42'
}

# Closes whatever menu the previous click opened, so its screen real estate
# and mouse-target state can't bleed into the next click.
dismiss_menu() {
	outer send-keys -t "$OPANE" Escape
	sleep 0.2
}

# Runs the real launcher against host "h", with the ssh stub answering
# REMOTE_SESSION as the remote's live (and only) session/window. `no_switch`
# (any non-empty value) tells the tmux shim to no-op switch-client, for a
# case with no attached client.
open_remote() { # remote-sess-name [no_switch]
	# shellcheck disable=SC2034 # NO_SWITCH is read by $FAKEBIN/tmux, not this shell
	REMOTE_SESSION="$1" NO_SWITCH="${2:-}" OG_REMOTE_TMPDIR=/run/user/1000 \
		bash "$LAUNCHER" h
}

# --- harness: proves the injection fires at all, before the fix is trusted ---

@test "harness: positive control" {
	attach_client
	local qid sid
	qid="$(inner new-session -d -P -F '#{session_id}' -s "h-$QUOTE")"
	inner set-option -t "$qid" @bridge_host h
	inner set-option -t "$qid" @bridge_session "$QUOTE"
	sid="$(inner new-session -d -P -F '#{session_id}' -s "h-$SUBST")"
	inner set-option -t "$sid" @bridge_host h
	inner set-option -t "$sid" @bridge_session "$SUBST"

	# A bare switch onto the QUOTE session has been seen to fire its payload
	# in this two-server harness, so sentinels are cleared right after each
	# switch: what is asserted is what the CLICK adds.

	# Click 1: attached to QUOTE, title x quote. run-shell -C splits QUOTE's own
	# breakout into its own top-level command (measured), which runs directly —
	# but it also spends the click's mouse-target on the FIRST display-menu, so
	# a would-be SECOND display-menu for the loop (measured: "no mouse target")
	# never opens; loop x subst is not observable from this click.
	switch_client_to "$qid"
	rm -f "$S_A" "$S_B"
	click 3 1
	wait_for_sentinel "$S_A" # title x quote
	rm -f "$S_A" "$S_B"
	dismiss_menu

	# Click 2: attached to SUBST. SUBST never splits the command (a `#(...)`
	# job is a self-contained format token, not command syntax), so the SAME
	# single display-menu call renders both its own title and every "Switch
	# To" loop item — QUOTE's breakout in the loop fires there too.
	switch_client_to "$sid"
	rm -f "$S_A" "$S_B"
	click 3 1
	wait_for_sentinel "$S_B" # title x subst
	wait_for_sentinel "$S_A" # loop x quote
	rm -f "$S_A" "$S_B"
	dismiss_menu

	# Click 3: attached to the plain base session "s" — title is inert, so
	# this click's single display-menu call renders a "Switch To" loop entry
	# for BOTH QUOTE and SUBST, completing loop x subst (the one case click 1
	# cannot observe).
	switch_client_to s
	rm -f "$S_A" "$S_B"
	click 3 1
	wait_for_sentinel "$S_B" # loop x subst
	wait_for_sentinel "$S_A" # loop x quote (again)
	dismiss_menu

	# Click 4: the issue's literal payload as the title of the mirror-branch
	# session menu.
	local lid
	lid="$(inner new-session -d -P -F '#{session_id}' -s "h-$LITERAL")"
	inner set-option -t "$lid" @bridge_host h
	inner set-option -t "$lid" @bridge_session "$LITERAL"
	gate_mirror "$lid"
	switch_client_to "$lid"
	rm -f "$S_C"
	click 3 1
	wait_for_sentinel "$S_C" # title x literal quote, mirror branch
}

# --- fixed: the launcher's sanitized names are inert under the same click ---

@test "fixed: the hostile names open as inert mirrors" {
	attach_client

	run open_remote "$QUOTE"
	[ "$status" -eq 0 ]
	run open_remote "$SUBST"
	[ "$status" -eq 0 ]
	run open_remote "$LITERAL"
	[ "$status" -eq 0 ]

	mirror_name_part h
	local hpart="$REPLY"
	mirror_name_part "$QUOTE"
	local qsess="${hpart}-${REPLY}"
	mirror_name_part "$SUBST"
	local ssess="${hpart}-${REPLY}"
	mirror_name_part "$LITERAL"
	local lsess="${hpart}-${REPLY}"

	local sessions
	sessions="$(inner list-sessions -F '#{session_name}')"
	grep -qxF "$qsess" <<<"$sessions"
	grep -qxF "$ssess" <<<"$sessions"
	[ "$(inner show-options -t "$qsess" -qv @bridge_session)" = "$QUOTE" ]
	[ "$(inner show-options -t "$ssess" -qv @bridge_session)" = "$SUBST" ]
	[ "$(inner show-options -t "$lsess" -qv @bridge_session)" = "$LITERAL" ]

	rm -f "$S_A" "$S_B" "$S_C"
	switch_client_to "=$qsess"
	click 3 1
	wait_for_screen "$qsess"
	wait_for_screen "Switch To $ssess"
	run ! wait_for_sentinel "$S_A"
	run ! wait_for_sentinel "$S_B"

	dismiss_menu
	switch_client_to "=$ssess"
	click 3 1
	wait_for_screen "$ssess"
	wait_for_screen "Switch To $qsess"
	run ! wait_for_sentinel "$S_A"
	run ! wait_for_sentinel "$S_B"

	dismiss_menu
	gate_mirror "=$lsess"
	switch_client_to "=$lsess"
	click 3 1
	wait_for_screen "$lsess"
	wait_for_screen "Switch To $qsess"
	run ! wait_for_sentinel "$S_C"
	run ! wait_for_sentinel "$S_A"
	run ! wait_for_sentinel "$S_B"
}

# --- identity: the raw pair, not the name, is what's found again ------------

@test "identity: colliding names get distinct mirrors, found again by pair" {
	mirror_name_part h
	local hpart="$REPLY"
	mirror_name_part "a.b"
	local base="${hpart}-${REPLY}"
	local suffixed="${base}-remote"

	run open_remote "a.b" 1
	[ "$status" -eq 0 ]
	run open_remote "a_b" 1
	[ "$status" -eq 0 ]

	local sessions
	sessions="$(inner list-sessions -F '#{session_name}')"
	grep -qxF "$base" <<<"$sessions"
	grep -qxF "$suffixed" <<<"$sessions"
	[ "$(inner show-options -t "$base" -qv @bridge_session)" = "a.b" ]
	[ "$(inner show-options -t "$suffixed" -qv @bridge_session)" = "a_b" ]

	# Reopening each resolves by pair, not by first-free name: still exactly
	# two sessions under this base, session ids may churn but the names don't.
	run open_remote "a.b" 1
	[ "$status" -eq 0 ]
	run open_remote "a_b" 1
	[ "$status" -eq 0 ]
	sessions="$(inner list-sessions -F '#{session_name}')"
	[ "$(grep -cxF -e "$base" -e "$suffixed" <<<"$sessions")" -eq 2 ]

	inner kill-session -t "=$base"
	run open_remote "a_b" 1
	[ "$status" -eq 0 ]
	sessions="$(inner list-sessions -F '#{session_name}')"
	grep -qxF "$suffixed" <<<"$sessions"
	run ! grep -qxF "$base" <<<"$sessions"
}

@test "identity: a pre-#783 raw-named mirror is retired on reopen" {
	local old_sock="$BATS_TEST_TMPDIR/old.sock"
	sleep 60 &
	SLEEP_PID=$!
	echo "$SLEEP_PID" >"${old_sock}.pid"

	# -s format-expands its value (like og-remote-open's own -s / -n): a
	# literal single '#' would execute "(y)" as a job and collapse to "h-x"
	# (measured). '##' collapses to one literal '#' without running it, giving
	# the session its real single-# name — matching a pre-#783 daemon's own
	# already-collapsed `new-session -s "$local_sess"`.
	inner new-session -d -s 'h-x##(y)'
	inner set-option -t 'h-x#(y)' @bridge_host h
	inner set-option -t 'h-x#(y)' @bridge_session 'x##(y)'
	inner set-option -t 'h-x#(y)' @bridge_sock "$old_sock"

	# The ping fails with a non-protocol error, so ownership of the pidfile's
	# pid is never proven — the process must survive.
	export FAKE_CTL_ERROR='dial: connection refused'
	run open_remote 'x##(y)' 1
	[ "$status" -eq 0 ]

	mirror_name_part h
	local hpart="$REPLY"
	mirror_name_part 'x##(y)'
	local newsess="${hpart}-${REPLY}"

	run inner has-session -t '=h-x#(y)'
	[ "$status" -ne 0 ]
	run inner has-session -t "=$newsess"
	[ "$status" -eq 0 ]
	[ "$(inner show-options -t "$newsess" -qv @bridge_session)" = 'x##(y)' ]
	kill -0 "$SLEEP_PID"
}
