#!/usr/bin/env bats

bats_require_minimum_version 1.5.0

# A fake `tmux` driven by env vars. It logs `display-popup` invocations to
# $POPUP_LOG so the test can assert whether the splash would have opened.
setup() {
	STUBDIR="$(mktemp -d)"
	POPUP_LOG="$STUBDIR/popup.log"
	SETOPT_LOG="$STUBDIR/setopt.log"
	export POPUP_LOG
	export SETOPT_LOG
	cat >"$STUBDIR/tmux" <<-'EOF'
		#!/bin/sh
		# /bin/sh, not /usr/bin/env: the Nix flake-check sandbox has no /usr/bin.
		# Get the last argument (the format string passed to -p).
		last_arg() { eval "echo \"\${$#}\""; }
		case "$1" in
		show-option) echo "${FAKE_SHOWN:-}";;
		display-message)
			fmt="$(last_arg "$@")"
			case "$fmt" in
			'#{session_name}') echo "s";;
			'#{session_windows}') echo "${FAKE_WINDOWS:-1}";;
			'#{window_panes}') echo "${FAKE_PANES:-1}";;
			'#{pane_current_command}') echo "${FAKE_CMD:-fish}";;
			'#{I/e:SSH_CONNECTION}')
				case "${FAKE_SSH:-unset}" in
				set) echo "1.2.3.4 1111 5.6.7.8 22";;
				unset) echo "";;
				error) exit 1;;
				esac;;
			esac;;
		list-clients) printf '%s %s\n' "${FAKE_CONTROL:-0}" "${FAKE_CLIENT:-/dev/ttys0}";;
		set-option)   echo "$*" >>"$SETOPT_LOG";;
		display-popup) echo "$*" >>"$POPUP_LOG";;
		esac
	EOF
	chmod +x "$STUBDIR/tmux"
	PATH="$STUBDIR:$PATH"
	GATE="$STUBDIR/gate.sh"
	mkgate full
}

# mkgate <remote-mode>: (re)generate $GATE with the given @splash_remote@
# baked in. Runs from the test body (not setup()), since a per-test env var
# prefix on `run` can't reach a substitution already done in setup().
mkgate() {
	sed -e 's#@tmux_splash@#/bin/true#' -e "s#@splash_remote@#$1#" \
		scripts/tmux-splash-maybe.sh >"$GATE"
	chmod +x "$GATE"
}

teardown() { rm -rf "$STUBDIR"; }

@test "fresh single-shell session opens the popup" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
}

@test "already-shown session does not open the popup" {
	FAKE_SHOWN=1 run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "multi-pane session does not open the popup" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=2 run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "session running a program (not a shell) does not open the popup" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=nvim run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "remote attach with mode=skip does not open the popup" {
	mkgate skip
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=set \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "remote attach with mode=static opens the popup with --static" {
	mkgate static
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=set \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
	grep -q -- '--static' "$POPUP_LOG"
	grep -Eq -- '(^| )-c /dev/ttys0( |$)' "$POPUP_LOG"
}

@test "remote attach with mode=full opens the popup without --static" {
	mkgate full
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=set \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
	run ! grep -q -- '--static' "$POPUP_LOG"
	grep -Eq -- '(^| )-c /dev/ttys0( |$)' "$POPUP_LOG"
}

@test "local (removal-marker) attach with mode=skip still opens the normal popup" {
	mkgate skip
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=unset \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
	run ! grep -q -- '--static' "$POPUP_LOG"
}

@test "show-environment lookup error with mode=skip is treated as local, opens the normal popup" {
	mkgate skip
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=error \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
	run ! grep -q -- '--static' "$POPUP_LOG"
}

@test "skip mode does not set @splash_shown, so a later local attach still gets the splash" {
	mkgate skip
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=set \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]

	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_SSH=unset \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
}

@test "control-mode client opens no popup" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_CONTROL=1 \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "control-mode skip does not write @splash_shown" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_CONTROL=1 \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$SETOPT_LOG" ]
}

@test "control-mode skip does not consume the flag, so a following non-control attach still gets the splash" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_CONTROL=1 \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ ! -s "$POPUP_LOG" ]

	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish FAKE_CONTROL=0 \
		run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
}

@test "unresolvable client fails closed" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish run bash "$GATE" s bogus
	[ "$status" -ne 0 ]
	[ ! -s "$POPUP_LOG" ]
}

@test "the popup carries -c <client>" {
	FAKE_SHOWN="" FAKE_WINDOWS=1 FAKE_PANES=1 FAKE_CMD=fish run bash "$GATE" s /dev/ttys0
	[ "$status" -eq 0 ]
	[ -s "$POPUP_LOG" ]
	grep -Eq -- '(^| )-c /dev/ttys0( |$)' "$POPUP_LOG"
}
