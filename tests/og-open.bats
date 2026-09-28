#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
bats_require_minimum_version 1.5.0 # run !
# og-open (#854): the $BROWSER-aware opener every server-started pane gets.
# Checks whether the pane's session is bridged (a registered control client
# is attached); bridged, it validates the URL then appends/replaces a record
# on the session, otherwise it hands the argument off untouched to the
# platform opener. tmux, xdg-open, open and uname are fakes on PATH; nothing
# here touches a real server.

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	export OPENED="$BATS_TEST_TMPDIR/opened"
	: >"$TMUX_LOG"
	: >"$OPENED"

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$TMUX_LOG"
		case "$1" in
		list-clients)
			printf '%s\n' "${TMUX_STUB_CLIENTS:-}"
			exit "${TMUX_STUB_RC:-0}"
			;;
		set-option)
			exit 0
			;;
		esac
		exit 0
	EOF

	cat >"$FAKEBIN/xdg-open" <<-'EOF'
		#!/bin/sh
		echo "xdg-open $* BROWSER=${BROWSER-unset}" >>"$OPENED"
	EOF

	cat >"$FAKEBIN/open" <<-'EOF'
		#!/bin/sh
		echo "open $* BROWSER=${BROWSER-unset}" >>"$OPENED"
	EOF

	cat >"$FAKEBIN/uname" <<-'EOF'
		#!/bin/sh
		echo "${UNAME_STUB:-Linux}"
	EOF

	chmod +x "$FAKEBIN"/*
	export PATH="$FAKEBIN:$PATH"

	unset TMUX TMUX_PANE

	SCRIPT="${OG_OPEN:-$BATS_TEST_DIRNAME/../scripts/og-open.sh}"
}

assert_rejected() {
	: >"$TMUX_LOG"
	: >"$OPENED"
	run bash "$SCRIPT" "$@"
	[ "$status" -eq 2 ]
	if grep -q '^set-option' "$TMUX_LOG"; then
		return 1
	fi
	[ ! -s "$OPENED" ]
}

assert_rejected_unbridged() {
	unset TMUX TMUX_PANE
	assert_rejected "$@"
}

assert_rejected_bridged() {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_CLIENTS='1|10'
	assert_rejected "$@"
}

assert_not_bridged() {
	run bash "$SCRIPT" https://example.invalid/x
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "xdg-open https://example.invalid/x BROWSER=unset" ]
	! grep -q '^set-option' "$TMUX_LOG"
}

@test "rejects: no argument" {
	assert_rejected_unbridged
}

@test "rejects: two arguments" {
	assert_rejected_unbridged https://example.invalid/a https://example.invalid/b
}

@test "rejects: no argument, bridged" {
	assert_rejected_bridged
}

@test "rejects: two arguments, bridged" {
	assert_rejected_bridged https://example.invalid/a https://example.invalid/b
}

@test "rejects: unsupported scheme (ftp), bridged" {
	assert_rejected_bridged ftp://x
}

@test "rejects: file scheme, bridged" {
	assert_rejected_bridged file:///etc/passwd
}

@test "rejects: javascript scheme, bridged" {
	assert_rejected_bridged 'javascript:alert(1)'
}

@test "rejects: bare https scheme, bridged" {
	assert_rejected_bridged https://
}

@test "rejects: bare http scheme, bridged" {
	assert_rejected_bridged http://
}

@test "rejects: uppercase scheme, bridged" {
	assert_rejected_bridged HTTPS://x
}

@test "rejects: embedded space, bridged" {
	assert_rejected_bridged 'https://a b'
}

@test "rejects: embedded tab, bridged" {
	assert_rejected_bridged $'https://a\tb'
}

@test "rejects: embedded newline, bridged" {
	assert_rejected_bridged $'https://a\nb'
}

@test "rejects: trailing semicolon, bridged" {
	assert_rejected_bridged 'https://a;'
}

@test "rejects: 4097-byte URL (over the cap), bridged" {
	local url
	url="https://$(head -c 4089 /dev/zero | tr '\0' 'a')"
	[ "$(printf %s "$url" | wc -c)" -eq 4097 ]
	assert_rejected_bridged "$url"
}

@test "rejects: multibyte URL over the byte cap but under the character cap, bridged" {
	# 4000 ASCII bytes (scheme + padding) + 50 two-byte characters: 4100
	# bytes total, 4050 characters — a wc -c check catches this; a
	# character-counting check (${#url} under a UTF-8 locale) would not.
	local url _i
	url="https://$(head -c 3992 /dev/zero | tr '\0' 'a')"
	for _i in $(seq 1 50); do url+="é"; done
	export LC_ALL=C.UTF-8
	[ "$(printf %s "$url" | wc -c)" -eq 4100 ]
	assert_rejected_bridged "$url"
}

@test "accepts a 4096-byte URL (boundary), unbridged" {
	local url
	url="https://$(head -c 4088 /dev/zero | tr '\0' 'a')"
	[ "$(printf %s "$url" | wc -c)" -eq 4096 ]
	run bash "$SCRIPT" "$url"
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "xdg-open $url BROWSER=unset" ]
}

@test "unbridged: file URL passes through untouched" {
	run bash "$SCRIPT" file:///tmp/x.html
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "xdg-open file:///tmp/x.html BROWSER=unset" ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "unbridged: plain path passes through untouched" {
	run bash "$SCRIPT" /tmp/doc/index.html
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "xdg-open /tmp/doc/index.html BROWSER=unset" ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "unbridged: argument with a space passes through verbatim" {
	run bash "$SCRIPT" 'https://example.invalid/a b'
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "xdg-open https://example.invalid/a b BROWSER=unset" ]
	run ! grep -q '^set-option' "$TMUX_LOG"
}

@test "bridged: appends a record when the log is under the cap" {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_CLIENTS=$'0|10\n1|10'
	run bash "$SCRIPT" https://example.invalid/x
	[ "$status" -eq 0 ]
	local line1 line2
	line1="$(sed -n '1p' "$TMUX_LOG")"
	line2="$(sed -n '2p' "$TMUX_LOG")"
	[ "$line1" = 'list-clients -t %3 -F #{?#{&&:#{client_control_mode},#{==:#{client_name},#{@og_open_client}}},1,0}|#{n:@og_open_url}' ]
	[[ $line2 =~ ^set-option\ -a\ -t\ %3\ @og_open_url\ \ [0-9]+-[0-9]+\|https://example\.invalid/x$ ]]
	[ ! -s "$OPENED" ]
}

@test "bridged: still appends when the log is exactly at the cap" {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_CLIENTS='1|4096'
	run bash "$SCRIPT" https://example.invalid/x
	[ "$status" -eq 0 ]
	local line2
	line2="$(sed -n '2p' "$TMUX_LOG")"
	[[ $line2 =~ ^set-option\ -a\ -t\ %3\ @og_open_url\ \ [0-9]+-[0-9]+\|https://example\.invalid/x$ ]]
}

@test "bridged: replaces the record when the log is over the cap" {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_CLIENTS='1|4097'
	run bash "$SCRIPT" https://example.invalid/x
	[ "$status" -eq 0 ]
	local line2
	line2="$(sed -n '2p' "$TMUX_LOG")"
	[[ $line2 =~ ^set-option\ -t\ %3\ @og_open_url\ \ [0-9]+-[0-9]+\|https://example\.invalid/x$ ]]
}

@test "not bridged: TMUX unset" {
	export BROWSER=og-open
	assert_not_bridged
}

@test "not bridged: TMUX set, TMUX_PANE unset" {
	export TMUX="/tmp/s,1,0"
	assert_not_bridged
}

@test "not bridged: only unregistered control clients" {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_CLIENTS='0|10'
	assert_not_bridged
}

@test "not bridged: list-clients fails" {
	export TMUX="/tmp/s,1,0"
	export TMUX_PANE="%3"
	export TMUX_STUB_RC=1
	assert_not_bridged
}

@test "not bridged, Darwin: opens via open" {
	export UNAME_STUB=Darwin
	run bash "$SCRIPT" https://example.invalid/x
	[ "$status" -eq 0 ]
	[ "$(cat "$OPENED")" = "open https://example.invalid/x BROWSER=unset" ]
}
