#!/usr/bin/env bats
# Per-host colour of placed mirror tabs and the session host marker (#1102).
# Runs the real tmux-reflow-windows (with @picker_generate@ resolved to the real
# tmux-picker-generate, passed in $PICKER_GENERATE by the flake check) against a
# private, config-less tmux server, same pattern as reflow-fanout.bats, so it
# needs the plain tmux that check pins. A config-less server draws no line 0, so
# the tests read what reflow stamps and expand it with display-message; the real
# tmux-statusline render is covered by its Go tests.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"
	[[ -x ${PICKER_GENERATE:-} ]] || skip "PICKER_GENERATE not set"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	export TMPDIR="$TDIR"

	REFLOW="$TDIR/reflow.sh"
	local licons lenrich
	licons="$TDIR/lib-icons.sh"
	lenrich="$TDIR/lib-enrich.sh"
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e 's/@providers@/linear github/g' \
		-e 's/@enrich_icon_linear@/L/g' -e 's/@enrich_icon_github@/G/g' \
		-e 's/@enrich_icon_pending@/P/g' -e 's/@enrich_icon_success@/S/g' \
		-e 's/@enrich_icon_failure@/F/g' -e 's/@enrich_icon_merged@/M/g' \
		-e 's/@enrich_icon_closed@/X/g' -e 's/@enrich_icon_conflict@/C/g' \
		scripts/lib-enrich.sh >"$lenrich"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_enrich@|$lenrich|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@lib_reflow@|$PWD/scripts/lib-reflow.sh|g" \
		-e "s|@picker_generate@|$PICKER_GENERATE|g" \
		-e 's|@MAX_ICONS@|5|g' \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-reflow-windows.sh >"$REFLOW"
	chmod +x "$REFLOW"

	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	tmux set -g base-index 0
	tmux set -g @icon_remote "R"
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
	tmux set -g @thm_peach "#fab387"
	tmux set -g @thm_teal "#94e2d5"
	tmux set -g @thm_pink "#f5c2e7"
	tmux set -g @thm_yellow "#f9e2af"
	tmux set -g @thm_sky "#89dceb"
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

stamp_placed() {
	local w=$1 host=$2
	tmux set -wq -t "S:$w" @bridge_win 1
	tmux set -wq -t "S:$w" @bridge_placed 1
	tmux set -wq -t "S:$w" @bridge_host "$host"
	tmux set -wq -t "S:$w" @bridge_label_id x
	tmux set-window-option -t "S:$w" automatic-rename off
}

# The colour token the picker assigns $2 within the configured set $1.
picker_token() {
	local hosts=$1 host=$2
	"$PICKER_GENERATE" host-colors "$hosts" "$host" | awk -v h="$host" '$1 == h { printf "#{@thm_%s}", $2 }'
}

@test "placed windows from two hosts take the picker's per-host colours" {
	tmux set -g @remote_bridge_hosts "lab serverless"
	tmux new-window -d
	tmux new-window -d
	stamp_placed 1 serverless
	stamp_placed 2 lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(tmux show -wv -t S:1 @window_host_color)" = "$(picker_token "lab serverless" serverless)" ]
	[ "$(tmux show -wv -t S:2 @window_host_color)" = "$(picker_token "lab serverless" lab)" ]
	[ "$(tmux show -wv -t S:1 @window_host_color)" != "$(tmux show -wv -t S:2 @window_host_color)" ]
	# A local window stays uncoloured, so its index keeps the tab colour.
	[ -z "$(tmux show -wv -t S:0 @window_host_color)" ]
}

@test "five hosts get five distinct colours (the picker's collision probe)" {
	local hosts="aa bb cc dd ee" i=1 h
	tmux set -g @remote_bridge_hosts "$hosts"
	for h in $hosts; do
		tmux new-window -d
		stamp_placed $i "$h"
		i=$((i + 1))
	done

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(for i in 1 2 3 4 5; do tmux show -wv -t "S:$i" @window_host_color; done | sort -u | wc -l)" -eq 5 ]
}

@test "the session marker lists the sorted hosts and expands to coloured hex" {
	tmux set -g @remote_bridge_hosts "lab serverless"
	tmux new-window -d
	tmux new-window -d
	stamp_placed 1 serverless
	stamp_placed 2 lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	local lab srv
	lab="$(picker_token "lab serverless" lab)"
	srv="$(picker_token "lab serverless" serverless)"
	[ "$(tmux show -v @session_host_marker)" = "#[fg=#{@thm_overlay_1}]R #[fg=${lab}]lab#[fg=#{@thm_overlay_1}] · #[fg=${srv}]serverless" ]

	local expanded
	expanded="$(tmux display-message -t S -p '#{E:@session_host_marker}')"
	[[ $expanded == *'#[fg=#000000]R '* ]]
	[[ $expanded =~ \#\[fg=\#[0-9a-f]{6}\]lab ]]
	[[ $expanded =~ \#\[fg=\#[0-9a-f]{6}\]serverless ]]
	[[ $expanded == *' · '* ]]
	[[ $expanded != *'#{'* ]]
}

@test "no placed window means no marker, and a departed one clears it" {
	tmux set -g @remote_bridge_hosts "lab"
	bash "$REFLOW" S 200 --force >/dev/null 2>&1
	[ -z "$(tmux show -v @session_host_marker)" ]
	[ -z "$(tmux show -wv -t S:0 @window_host_color)" ]

	tmux new-window -d
	stamp_placed 1 lab
	bash "$REFLOW" S 200 --force >/dev/null 2>&1
	[ -n "$(tmux show -v @session_host_marker)" ]

	tmux set -wu -t S:1 @bridge_placed
	bash "$REFLOW" S 200 --force >/dev/null 2>&1
	[ -z "$(tmux show -v @session_host_marker)" ]
	[ -z "$(tmux show -wv -t S:1 @window_host_color)" ]
}

@test "a host with unsafe characters is left out of the marker" {
	tmux new-window -d
	stamp_placed 1 'bad#{host}'
	tmux new-window -d
	stamp_placed 2 lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[[ "$(tmux show -v @session_host_marker)" != *'bad'* ]]
	[[ "$(tmux show -v @session_host_marker)" == *'lab'* ]]
}

@test "a user@host name is coloured and listed in the marker" {
	tmux set -g @remote_bridge_hosts "user@lab"
	tmux new-window -d
	stamp_placed 1 user@lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	local tok
	tok="$(picker_token "user@lab" user@lab)"
	[ -n "$tok" ]
	[ "$(tmux show -wv -t S:1 @window_host_color)" = "$tok" ]
	[[ "$(tmux show -v @session_host_marker)" == *"#[fg=${tok}]user@lab" ]]
}

@test "hosts with parens, spaces or a leading symbol are left out of the marker" {
	tmux new-window -d
	stamp_placed 1 'a(b'
	tmux new-window -d
	stamp_placed 2 'a b'
	tmux new-window -d
	stamp_placed 3 '@x'
	tmux new-window -d
	stamp_placed 4 lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[ "$(tmux show -v @session_host_marker | sed 's/#\[[^]]*\]//g')" = "R lab" ]
}

@test "the folded line carries the marker after the session name" {
	tmux set -g @remote_bridge_hosts "lab"
	tmux new-window -d
	stamp_placed 1 lab
	tmux set -t S @og_status_folded 1

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	[[ "$(tmux show -v status-format[0])" == *'#{session_name}#{?#{@session_host_marker}, #{E:@session_host_marker},}'* ]]
	[[ "$(tmux display-message -t S -p '#{E:@session_host_marker}')" == *lab* ]]
}

@test "without the picker binary the marker renders uncoloured and tabs fall back to peach" {
	tmux set -g @remote_bridge_hosts "lab"
	tmux new-window -d
	stamp_placed 1 lab
	sed -e "s|$PICKER_GENERATE|@picker_generate@|g" "$REFLOW" >"$REFLOW.nobin"

	bash "$REFLOW.nobin" S 200 --force >/dev/null 2>&1

	[ -z "$(tmux show -wv -t S:1 @window_host_color)" ]
	[[ "$(tmux show -v @session_host_marker)" == *'#[fg=#{@thm_subtext_0}]lab' ]]
}
