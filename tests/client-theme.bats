#!/usr/bin/env bats
# Live proof for #663: the client-light-theme[40]/client-dark-theme[40] hooks
# drive tmux-client-theme.sh end to end on the wrapped server. An INNER
# wrapped server holds the real session; an OUTER, config-less scratch server
# (mkTmux's raw tmux, never the wrapped one) hosts a window whose command is a
# real `attach` client into the inner session -- that gives the inner attach
# client a genuine pty, so a literal `send-keys -l` into the OUTER pane is read
# by the inner client as a terminal response, exactly like a real terminal's
# \e[?997;{1,2}n theme report (measured, spec #663 "Measured facts"). Control
# clients never fire these hooks (measured), so no control-mode leg is tested
# here -- its only observable is an absence.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	OUTER_TMUX="${OUTER_TMUX:?set OUTER_TMUX to the raw tmux binary}"
	TMUX_CONF="${TMUX_CONF:?set TMUX_CONF to the generated tmux.conf}"

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	unset TMUX

	# Mandatory (#603/#647 precedent): the inner server's other monitor hooks
	# fire too and write under these dirs, whose defaults are the real /tmp trees.
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	mkdir -p "$CLAUDE_STATUS_DIR"/{panes,screen,interrupt,tasks,issues,watchers}

	export OG_CLIENT_THEME_LOCK="$BATS_TEST_TMPDIR/client-theme.lock"

	# DANGER: the developer PATH may hold a real theme-toggle. Strip any PATH
	# dir that has one before either server starts -- a server's PATH is fixed
	# at start, so filtering later would be too late.
	mkdir -p "$BATS_TEST_TMPDIR/bin"
	local dirs=() d filtered=()
	IFS=: read -ra dirs <<<"$PATH"
	for d in "${dirs[@]}"; do
		[ -x "$d/theme-toggle" ] && continue
		filtered+=("$d")
	done
	local joined
	joined=$(
		IFS=:
		echo "${filtered[*]}"
	)
	export PATH="$BATS_TEST_TMPDIR/bin:$joined"
}

teardown() {
	inner kill-server 2>/dev/null || true
	outer kill-server 2>/dev/null || true
	[ -n "${INNER_TMPDIR:-}" ] && rm -rf "$INNER_TMPDIR"
	[ -n "${OUTER_TMPDIR:-}" ] && rm -rf "$OUTER_TMPDIR"
	return 0
}

inner() {
	TMUX_TMPDIR="$INNER_TMPDIR" timeout --foreground 30s "$TMUX_BIN" -L s "$@"
}

# -f /dev/null on every call, not just the server-starting one: tmux only
# reads it for that first call anyway, but a private HOME here also carries
# $HOME/.config/tmux/tmux.conf (the INNER server's reload target) -- without
# this, the outer server's own cold start would find and source that same
# file, double-counting the "reloads" marker.
outer() {
	TMUX_TMPDIR="$OUTER_TMPDIR" timeout --foreground 30s "$OUTER_TMUX" -f /dev/null -L s "$@"
}

wait_for() {
	# wait_for <seconds> <command...> -- polls until <command...> exits 0.
	local timeout=$1
	shift
	local remaining=$timeout
	while ((remaining-- > 0)); do
		"$@" && return 0
		sleep 1
	done
	return 1
}

client_count() { inner list-clients -t main -F '#{client_name}' 2>/dev/null | grep -c .; }
has_client_count() { [ "$(client_count)" -eq "$1" ]; }
flavor_is() { [ "$(inner show-options -gv @catppuccin_flavor 2>/dev/null)" = "$1" ]; }
applied_is() { [ "$(inner show-options -gv @og_theme_applied 2>/dev/null)" = "$1" ]; }
thm_bg_is() { [ "$(inner show-options -gv @thm_bg 2>/dev/null)" = "$1" ]; }
lock_gone() { [ ! -e "$OG_CLIENT_THEME_LOCK" ]; }
reload_count() { wc -l <"$RELOADS" 2>/dev/null || echo 0; }

# settle -- waits for the backgrounded job to have released its lock (a job
# already gone before the first check just means it finished faster than we
# could observe it holding the lock), plus a trailing buffer for the negative
# assertions that call this with no earlier positive wait of their own.
settle() {
	wait_for 20 lock_gone
	sleep 1
}

# report OUTER-WINDOW-ID light|dark -- fakes the terminal's theme response by
# writing the literal escape sequence into the outer pane the inner client's
# stdin reads from (spec #663 "Measured facts").
report() {
	local target=$1 kind=$2 seq
	case $kind in
	light) seq=$'\e[?997;2n' ;;
	dark) seq=$'\e[?997;1n' ;;
	esac
	outer send-keys -t "$target" -l "$seq"
}

# start_client -- prints the id of a new outer window whose command is a real
# `attach` into the inner "main" session, giving that client a genuine pty.
# -u SSH_CONNECTION is explicit, not relying on absence: ambient CI/test-runner
# environment must never leak SSH_CONNECTION through and flip what should be
# the "local" case.
start_client() {
	outer new-window -d -P -F '#{window_id}' -t outer: -- \
		env -u TMUX -u SSH_CONNECTION TERM=xterm-256color TMUX_TMPDIR="$INNER_TMPDIR" "$TMUX_BIN" -L s attach -t main
}

# start_client_ssh -- same as start_client, but forces SSH_CONNECTION into the
# attach client's environment so is_ssh_client sees it as ssh-attached.
start_client_ssh() {
	outer new-window -d -P -F '#{window_id}' -t outer: -- \
		env -u TMUX SSH_CONNECTION='1.2.3.4 22 5.6.7.8 22' TERM=xterm-256color TMUX_TMPDIR="$INNER_TMPDIR" "$TMUX_BIN" -L s attach -t main
}

# theme_setup -- common heavy lifting: both servers, one attached client, and
# the reload baseline (the outer tmux may answer the inner client's \e[?996n
# subscribe with its own theme at attach, so baseline is a count, not 0).
theme_setup() {
	INNER_TMPDIR="$(mktemp -d /tmp/ogct.XXXXXX)"
	OUTER_TMPDIR="$(mktemp -d /tmp/ogct.XXXXXX)"
	RELOADS="$BATS_TEST_TMPDIR/reloads"
	: >"$RELOADS"

	mkdir -p "$HOME/.config/tmux"
	printf 'run-shell "echo x >> %s"\nsource-file %s\n' "$RELOADS" "$TMUX_CONF" \
		>"$HOME/.config/tmux/tmux.conf"

	mkdir -p "$XDG_STATE_HOME"
	printf '{"theme":"dark","timestamp":"2024-01-01T00:00:00+0000","failed":[],"version":1}\n' \
		>"$XDG_STATE_HOME/theme-state.json"

	inner new-session -d -s main -x 80 -y 24
	outer new-session -d -s outer -x 80 -y 24

	CLIENT1="$(start_client)"
	wait_for 20 has_client_count 1
	settle
	BASELINE="$(reload_count)"
}

@test "light report: flavor latte, thm_bg set, applied light, one reload, state file untouched" {
	theme_setup

	local state_before applied_before
	state_before="$(cat "$XDG_STATE_HOME/theme-state.json")"
	applied_before="$(inner show-options -gv @og_theme_applied 2>/dev/null)"

	report "$CLIENT1" light
	wait_for 20 applied_is light
	settle

	[ "$(($(reload_count) - BASELINE))" -eq 1 ]
	flavor_is latte
	thm_bg_is '#eff1f5'
	lock_gone

	# The stamp must have moved, not merely ended up at the target -- proves
	# this report drove the apply rather than some earlier state.
	[ "$(inner show-options -gv @og_theme_applied 2>/dev/null)" != "$applied_before" ]

	# The handler is tmux-only now: theme-state.json is theme-toggle's alone
	# and must be byte-unchanged.
	[ "$(cat "$XDG_STATE_HOME/theme-state.json")" = "$state_before" ]
}

@test "repeat light report: no-op, reloads still 1" {
	theme_setup

	report "$CLIENT1" light
	wait_for 20 applied_is light
	settle
	local after_first
	after_first="$(reload_count)"
	[ "$((after_first - BASELINE))" -eq 1 ]

	report "$CLIENT1" light
	settle

	[ "$(reload_count)" -eq "$after_first" ]
	flavor_is latte
}

@test "@og_follow_client_theme off suppresses the handler" {
	theme_setup
	inner set-option -g @og_follow_client_theme off

	report "$CLIENT1" light
	settle

	[ "$(($(reload_count) - BASELINE))" -eq 0 ]
	flavor_is mocha
}

@test "fake theme-toggle on PATH is never invoked, local reload still applies" {
	local log="$BATS_TEST_TMPDIR/toggle.log"
	cat >"$BATS_TEST_TMPDIR/bin/theme-toggle" <<-EOF
		#!/bin/sh
		printf '%s\n' "\$*" >>"$log"
	EOF
	chmod +x "$BATS_TEST_TMPDIR/bin/theme-toggle"

	theme_setup

	report "$CLIENT1" light
	wait_for 20 applied_is light
	settle

	# theme-toggle sits unused on PATH: the handler never shells out to it.
	[ ! -s "$log" ]
	# The handler applies locally via the reload path regardless.
	[ "$(($(reload_count) - BASELINE))" -eq 1 ]
}

@test "two nested clients: last report wins" {
	theme_setup
	local client2
	client2="$(start_client)"
	wait_for 20 has_client_count 2

	report "$CLIENT1" dark
	report "$client2" light
	settle

	wait_for 20 flavor_is latte
	thm_bg_is '#eff1f5'
}

@test "rejected conf still recovers: thm_bg non-empty" {
	theme_setup
	printf 'bogus-command-xyz\n' >"$HOME/.config/tmux/tmux.conf"

	report "$CLIENT1" light
	wait_for 20 applied_is light
	settle

	local bg
	bg="$(inner show-options -gv @thm_bg 2>/dev/null)"
	[ -n "$bg" ]
	[ "$bg" = '#eff1f5' ]
}

@test "ssh report ignored while a local client is attached" {
	theme_setup

	local client_ssh
	client_ssh="$(start_client_ssh)"
	wait_for 20 has_client_count 2

	# light, not dark: the baseline flavor is mocha (dark), so a dark report
	# would be indistinguishable from "nothing happened" even with no ssh
	# gate at all -- light is the only report that actually exercises the
	# gate.
	report "$client_ssh" light
	settle

	# CLIENT1 (local) is still attached, so the ssh report must be ignored.
	flavor_is mocha
	[ "$(reload_count)" -eq "$BASELINE" ]

	# Regression check: a `set -g` (no -F) on @og_client_theme_client would
	# store the literal string "#{hook_client}" instead of format-expanding
	# it -- this is the only place that would catch that, since ssh-gating
	# still "works" via the argv fast path alone even with that bug present.
	local stamped
	stamped="$(inner show-options -gv @og_client_theme_client 2>/dev/null)"
	[ "$stamped" != '#{hook_client}' ]
	[ -n "$stamped" ]
}

@test "ssh report followed when only ssh clients are attached (headless server)" {
	theme_setup

	local client_ssh
	client_ssh="$(start_client_ssh)"
	wait_for 20 has_client_count 2

	# Detach the local client -- only the ssh client remains attached.
	outer kill-window -t "$CLIENT1"
	wait_for 20 has_client_count 1

	# A fresh report issued after the detach: any earlier attach-time report
	# from the ssh client would have been gated out while CLIENT1 was still
	# present, so re-issuing here is what makes this observable.
	report "$client_ssh" light

	wait_for 20 flavor_is latte
	thm_bg_is '#eff1f5'
}
