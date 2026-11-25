#!/usr/bin/env bats
# Per-host colour of placed mirror tabs and the session host marker (#1102).
# Runs the real tmux-reflow-windows (with @picker_generate@ resolved to the real
# tmux-picker-generate, passed in $PICKER_GENERATE by the flake check) against a
# private, config-less tmux server, same pattern as reflow-fanout.bats, so it
# needs the plain tmux that check pins. A config-less server draws no line 0, so
# the tests read what reflow stamps and expand it with display-message; the real
# tmux-statusline render is covered by its Go tests.

setup() {
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

	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"
	[[ -x ${PICKER_GENERATE:-} ]] || skip "PICKER_GENERATE not set"

	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	tmux set -g base-index 0
	tmux set -g @icon_remote "R"
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
	tmux set -g @og_c_dim "#000000"
	tmux set -g @og_c_host_peach "#fab387"
	tmux set -g @og_c_host_teal "#94e2d5"
	tmux set -g @og_c_host_pink "#f5c2e7"
	tmux set -g @og_c_host_yellow "#f9e2af"
	tmux set -g @og_c_host_sky "#89dceb"
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
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
	"$PICKER_GENERATE" host-colors "$hosts" "$host" | awk -v h="$host" '$1 == h { printf "#{@og_c_host_%s}", $2 }'
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
	[ "$(tmux show -v @session_host_marker)" = "#[fg=#{@og_c_dim}]⇄ #[fg=${lab}]lab#[fg=#{@og_c_dim}] · #[fg=${srv}]serverless" ]

	local expanded
	expanded="$(tmux display-message -t S -p '#{E:@session_host_marker}')"
	[[ $expanded == *'#[fg=#000000]⇄ '* ]]
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

	[ "$(tmux show -v @session_host_marker | sed 's/#\[[^]]*\]//g')" = "⇄ lab" ]
}

@test "the folded line carries the marker after the window list, not the session name" {
	tmux set -g @remote_bridge_hosts "lab"
	tmux new-window -d
	stamp_placed 1 lab
	tmux set -t S @og_status_folded 1

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	local fmt
	fmt="$(tmux show -v status-format[0])"
	[[ $fmt == *'#{W:'*'}#[nolist]#{?#{@session_host_marker},  #{E:@session_host_marker},}#{?'* ]]
	[[ $fmt != *'#{session_name}#{?#{@session_host_marker}'* ]]
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

@test "a foreign-repo tag is stamped one palette colour per repo, empty when hidden" {
	tmux new-window -d
	tmux new-window -d
	tmux new-window -d
	tmux set -wq -t S:0 @crew_repo foreignrepo
	tmux set -wq -t S:1 @crew_repo foreignrepo
	# The session's own repo (basename of session_path) draws no tag.
	tmux set -wq -t S:2 @crew_repo "$(tmux display-message -t S:2 -p '#{b:session_path}')"
	tmux set -wq -t S:3 @crew_repo foreignrepo
	stamp_placed 3 lab

	bash "$REFLOW" S 200 --force >/dev/null 2>&1

	local c0 c1
	c0=$(tmux show -wv -t S:0 @window_repo_color)
	c1=$(tmux show -wv -t S:1 @window_repo_color)
	[[ $c0 =~ ^\#\{@og_c_host_(peach|teal|pink|yellow|sky)\}$ ]]
	[ "$c0" = "$c1" ]
	[ -z "$(tmux show -wv -t S:2 @window_repo_color)" ]
	[ -z "$(tmux show -wv -t S:3 @window_repo_color)" ]
}

@test "a foreign-repo tag is charged to the single-line fit, so the label clips and the PR stays whole" {
	# #1216: the tag is drawn ahead of the badge by status-format[1] but was
	# never counted, so a full row ran past the edge and clipped the PR number.
	tmux set -wq -t S:0 @branch "feat/a-long-branch-name-with-room-to-clip"
	tmux set -wq -t S:0 @pr_number 12
	tmux set -wq -t S:0 @pr_state open
	tmux set -wq -t S:0 @pr_check_state success
	tmux set -wq -t S:0 @pr_glyph S
	tmux new-window -d
	tmux set -wq -t S:1 @branch "feat/another-branch"

	row_is_whole() {
		[ "$(tmux show-options -t S -qv status)" = 2 ] || return 1
		case "$(tmux show -wv -t S:0 @window_label_disp)" in
		*…) return 1 ;;
		esac
	}

	# Narrowest width that renders one row with every label whole.
	local lo=40 hi=200 mid full=0
	while ((lo <= hi)); do
		mid=$(((lo + hi) / 2))
		bash "$REFLOW" S "$mid" --force >/dev/null 2>&1
		if row_is_whole; then
			full=$mid
			hi=$((mid - 1))
		else
			lo=$((mid + 1))
		fi
	done
	[ "$full" -gt 0 ]
	# shellcheck source=/dev/null
	source "$TDIR/lib-icons.sh"
	measure_display_width "$(tmux show -wv -t S:0 @window_label_disp)"
	local base=$REPLY_DW

	tmux set -wq -t S:0 @crew_repo foreignrepo
	bash "$REFLOW" S "$full" --force >/dev/null 2>&1

	[ "$(tmux show-options -t S -qv status)" = 2 ]
	[[ "$(tmux show -wv -t S:0 @window_label_disp)" == *… ]]
	measure_display_width "$(tmux show -wv -t S:0 @window_label_disp)"
	# "foreignrepo " is 12 cells: the label gives up at least that much.
	[ "$((base - REPLY_DW))" -ge 12 ]
	[ "$(tmux show -wv -t S:0 @window_pr_num)" = "#12" ]
}

# #1267: the multi-line grid drew and charged neither the repo tag nor its width.
grid_windows() {
	local i
	for i in 1 2 3 4 5; do tmux new-window -d; done
	for i in 0 1 2 3 4 5; do
		tmux set -wq -t "S:$i" @branch "feat/a-long-branch-name-number-$i-with-plenty-of-room-to-clip-here"
	done
	tmux set -wq -t S:0 @crew_repo foreignrepo
	tmux set -wq -t S:1 @crew_repo "$(tmux display-message -t S:1 -p '#{b:session_path}')"
}

@test "the multi-line grid draws a foreign-repo tag after the index, and not a same-repo one" {
	grid_windows

	bash "$REFLOW" S 140 --force >/dev/null 2>&1

	[ "$(tmux show-options -t S -qv status)" -ge 3 ]
	[ "$(tmux show -wv -t S:0 @window_repo_disp)" = "foreignrepo " ]
	[ -z "$(tmux show -wv -t S:1 @window_repo_disp)" ]

	local row own
	row="$(tmux display-message -t S -p "$(tmux show -v 'status-format[1]')" | sed 's/#\[[^]]*\]//g')"
	[[ $row == *'0: foreignrepo '* ]]
	own="$(tmux display-message -t S:1 -p '#{b:session_path}')"
	[[ $row != *"1: ${own} "* ]]
}

@test "the grid drops the repo tag when its column cannot fit it" {
	grid_windows

	bash "$REFLOW" S 60 --force >/dev/null 2>&1

	# A grid really rendered (single-line mode stamps the tag empty too).
	[ "$(tmux show-options -t S -qv status)" -ge 3 ]
	[ "$(tmux show -v -t S @window_split)" != 999 ]
	[ -z "$(tmux show -wv -t S:0 @window_repo_disp)" ]
}
