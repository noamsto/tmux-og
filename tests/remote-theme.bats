#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# Fanning a light/dark toggle out to the mirrors (#400). The toggle re-sources
# the config, so this script runs on every reload and must stay a no-op unless
# the flavor actually moved.
#
# tmux and the ctl are fakes on PATH; nothing here touches a server or a host.

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	export CTL_LOG="$BATS_TEST_TMPDIR/ctl.log"
	export STAMP="$BATS_TEST_TMPDIR/stamp"
	: >"$TMUX_LOG"
	: >"$CTL_LOG"
	: >"$STAMP"

	# FAKE_PANES is the list-panes -a reply: one "sock|pane" per line.
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$TMUX_LOG"
		case "$*" in
		"show-options -gv @catppuccin_flavor") echo "${FAKE_FLAVOR:-}" ;;
		"show-options -gv @og_theme_applied") cat "$STAMP" ;;
		"set-option -g @og_theme_applied "*) printf '%s' "$4" >"$STAMP" ;;
		list-panes*) printf '%s\n' "${FAKE_PANES:-}" ;;
		esac
		exit 0
	EOF

	cat >"$FAKEBIN/og-remote-bridge-ctl" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$CTL_LOG"
		exit "${FAKE_CTL_RC:-0}"
	EOF

	chmod +x "$FAKEBIN"/*
	export PATH="$FAKEBIN:$PATH"

	# The unsubstituted @bridge_ctl@ placeholder falls back to PATH, which is
	# what puts the fake ctl in play; the shipped script takes a pinned store
	# path instead.
	FANOUT="scripts/og-remote-theme.sh"
}

@test "mocha fans out dark to every mirrored session" {
	export FAKE_FLAVOR="mocha"
	FAKE_PANES="$(printf '/run/sock-a|%%7\n/run/sock-b|%%9\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	grep -q -- "--sock /run/sock-a theme %7 dark" "$CTL_LOG"
	grep -q -- "--sock /run/sock-b theme %9 dark" "$CTL_LOG"
	[ "$(wc -l <"$CTL_LOG")" -eq 2 ]
}

@test "latte is the only flavor that means light" {
	export FAKE_FLAVOR="latte"
	FAKE_PANES="$(printf '/run/sock-a|%%7\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	grep -q -- "theme %7 light" "$CTL_LOG"
}

@test "an unchanged flavor fans out nothing" {
	export FAKE_FLAVOR="mocha"
	FAKE_PANES="$(printf '/run/sock-a|%%7\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$CTL_LOG")" -eq 1 ]

	# The reload a `prefix + r` causes, with the theme where it already was.
	: >"$CTL_LOG"
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ ! -s "$CTL_LOG" ]
}

@test "a local-only pane is skipped, mirrors beside it are not" {
	export FAKE_FLAVOR="latte"
	FAKE_PANES="$(printf '|\n|%%3\n/run/sock-a|%%7\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$CTL_LOG")" -eq 1 ]
	grep -q -- "--sock /run/sock-a" "$CTL_LOG"
}

@test "no panes at all is a clean no-op" {
	export FAKE_FLAVOR="latte"
	export FAKE_PANES=""
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ ! -s "$CTL_LOG" ]
}

@test "one unreachable daemon does not stop the next host" {
	export FAKE_FLAVOR="latte"
	export FAKE_CTL_RC=1
	FAKE_PANES="$(printf '/run/sock-a|%%7\n/run/sock-b|%%9\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$CTL_LOG")" -eq 2 ]
}

@test "the stamp advances before the fan-out, so a failed host is not retried forever" {
	export FAKE_FLAVOR="latte"
	export FAKE_CTL_RC=1
	FAKE_PANES="$(printf '/run/sock-a|%%7\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(cat "$STAMP")" = "light" ]
}

@test "a socket path holding the delimiter keeps its pane" {
	export FAKE_FLAVOR="latte"
	FAKE_PANES="$(printf '/run/sock|a|%%7\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	grep -q -- "--sock /run/sock|a theme %7 light" "$CTL_LOG"
}

@test "two sockets in one session get one verb each" {
	export FAKE_FLAVOR="latte"
	FAKE_PANES="$(printf '/run/sock-a|%%1\n/run/sock-b|%%2\n/run/sock-a|%%3\n/run/sock-b|%%4\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$CTL_LOG")" -eq 2 ]
	grep -q -- "--sock /run/sock-a theme %1 light" "$CTL_LOG"
	grep -q -- "--sock /run/sock-b theme %2 light" "$CTL_LOG"
}

@test "a socket whose first pane has no mirror id uses its first non-empty pane" {
	export FAKE_FLAVOR="latte"
	FAKE_PANES="$(printf '/run/sock-a|\n/run/sock-a|%%5\n')"
	export FAKE_PANES
	run bash "$FANOUT"
	[ "$status" -eq 0 ]
	[ "$(wc -l <"$CTL_LOG")" -eq 1 ]
	grep -q -- "--sock /run/sock-a theme %5 light" "$CTL_LOG"
}
