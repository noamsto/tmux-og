#!/usr/bin/env bats
# prefix + Z (#970): the status bar folds to one line and back. Runs the real
# tmux-status-fold and tmux-reflow-windows against a private, config-less tmux
# server (bare `tmux` calls in the scripts resolve to it via TMUX_TMPDIR), the
# same pattern as reflow-fanout.bats, so it needs the plain tmux the nix check
# pins rather than tmux-og's wrapper (whose conf sets base-index 1).

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
		-e 's|@MAX_ICONS@|5|g' \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-reflow-windows.sh >"$REFLOW"
	chmod +x "$REFLOW"

	FOLD="$TDIR/fold.sh"
	sed \
		-e "s|@reflow@|$REFLOW|g" \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-status-fold.sh >"$FOLD"
	chmod +x "$FOLD"

	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"

	# Narrow client + many windows so the grid needs several status lines.
	tmux -f /dev/null new-session -d -s S -x 60 -y 50
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
	for _ in 1 2 3 4 5 6 7; do
		tmux new-window -d
	done
	bash "$REFLOW" S 60 --force >/dev/null 2>&1
	WANT=$(tmux show -v -t S @reflow_lines)
	[ "$WANT" -ge 3 ]
	[ "$(status_lines)" -eq "$WANT" ]
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	tmux -L out kill-server 2>/dev/null || true
	tmux kill-server 2>/dev/null || true
}

# tmux prints a one-line bar as `on`.
status_lines() {
	local v
	v=$(tmux show -v -t S status)
	[[ $v == on ]] && v=1
	echo "$v"
}

# What the prefix + Z bind does: flip the flag in one command, then hand over to the fold script.
press() {
	tmux set -F -t S @og_status_folded '#{?#{==:#{@og_status_folded},1},0,1}'
	"$FOLD" S 60 >/dev/null 2>&1
}

press_bg() {
	tmux set -F -t S @og_status_folded '#{?#{==:#{@og_status_folded},1},0,1}'
	"$FOLD" S 60 >/dev/null 2>&1 &
}

# Log every `status` value a set-option leaves behind, so a test can see that
# no intermediate line count was shown.
log_status() {
	STATUS_LOG="$TDIR/status.log"
	: >"$STATUS_LOG"
	tmux set-hook -g after-set-option "run-shell 'echo #{status} >>$STATUS_LOG'"
}

@test "fold: ends at one line with the folded flag on" {
	press
	[ "$(tmux show -v -t S @og_status_folded)" = 1 ]
	[ "$(status_lines)" = 1 ]
	tmux show -v -t S 'status-format[0]' | grep -Fq '#{session_name}'
}

@test "fold: a reflow while folded keeps one line, even for a new window" {
	press
	tmux new-window -d
	bash "$REFLOW" S 60 >/dev/null 2>&1
	bash "$REFLOW" S 60 --force >/dev/null 2>&1
	[ "$(status_lines)" = 1 ]
	tmux show -v -t S 'status-format[0]' | grep -Fq '#{session_name}'
}

@test "unfold: restores the line count reflow computes now" {
	press
	tmux new-window -d
	tmux new-window -d
	press
	[ "$(tmux show -v -t S @og_status_folded)" = 0 ]
	local want
	want=$(tmux show -v -t S @reflow_lines)
	[ "$(status_lines)" -eq "$want" ]
	# The grid's own formats are back, not the folded line.
	tmux show -v -t S 'status-format[1]' | grep -Fq 'window_index'
}

@test "two rapid toggles end unfolded" {
	press_bg
	press_bg
	wait
	[ "$(tmux show -v -t S @og_status_folded)" = 0 ]
	[ "$(status_lines)" -eq "$WANT" ]
}

@test "three rapid toggles end folded" {
	press_bg
	press_bg
	press_bg
	wait
	[ "$(tmux show -v -t S @og_status_folded)" = 1 ]
	[ "$(status_lines)" = 1 ]
}

@test "fold and unfold are instant: only the start and end line counts show" {
	log_status
	press
	tmux run-shell true
	run grep -vxE "$WANT|on" "$STATUS_LOG"
	[ -z "$output" ]
	[ "$(status_lines)" = 1 ]
	: >"$STATUS_LOG"
	press
	tmux run-shell true
	tmux set-hook -gu after-set-option
	run grep -vxE "$WANT|on" "$STATUS_LOG"
	[ -z "$output" ]
	[ "$(status_lines)" -eq "$WANT" ]
}

# Render the folded line for real: the session lives on the default server, a
# second server (-L out) runs a client attached to it, and the drawn status row
# is read back off that client's pane. list= scrolling is only visible in a
# client. Windows are labelled `lab<idx>` with icon `I<idx>`.
fold_render() {
	local width=$1 cur=$2 i
	tmux set -F -t S @og_status_folded 1
	tmux -L out kill-server 2>/dev/null || true
	sleep 0.2
	tmux -L out new-session -d -x "$width" -y 6 "env -u TMUX tmux attach -t S"
	bash "$REFLOW" S "$width" --force >/dev/null 2>&1
	# Reflow stamps the labels; override them afterwards, the format reads them live.
	for i in $(tmux list-windows -t S -F '#{window_index}'); do
		tmux set -w -t "S:$i" @window_label_id "lab$i"
		tmux set -w -t "S:$i" @window_label_rest_long ""
		tmux set -w -t "S:$i" @window_icon_display "I$i"
	done
	tmux select-window -t "S:$cur"
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		sleep 0.3
		LINE=$(tmux -L out capture-pane -p | grep -v '^$' | tail -1)
		[[ $LINE == *"lab$cur"* ]] && return 0
	done
}

@test "folded: windows keep index order with the current one expanded in place" {
	fold_render 100 3
	[[ $LINE =~ 0\ I0.*1\ I1.*2\ I2.*3:\ lab3.*4\ I4.*5\ I5.*6\ I6.*7\ I7 ]]
	[[ $LINE != *‹* && $LINE != *›* ]]
}

@test "folded overflow: the current window stays visible with a left marker at the end" {
	fold_render 34 7
	[[ $LINE == *"7: lab7"* ]]
	[[ $LINE == *‹* ]]
	[[ $LINE != *›* ]]
}

@test "folded overflow: a middle current window shows both markers" {
	fold_render 28 3
	[[ $LINE == *"3: lab3"* ]]
	[[ $LINE == *‹* ]]
	[[ $LINE == *›* ]]
}

@test "folded: a placed mirror window shows the remote glyph, and its host when current" {
	tmux set -g @icon_remote "R>"
	tmux set -g @thm_peach "#fab387"
	tmux set -w -t S:2 @bridge_placed 1
	tmux set -w -t S:2 @bridge_host labhost
	tmux set -w -t S:2 @window_host_color '#{@thm_peach}'
	fold_render 100 5
	[[ $LINE == *"2R>"* || $LINE == *"2 R>"* ]]
	fold_render 100 2
	[[ $LINE =~ 2:.*R\>\ labhost ]]
	# The host colour (#fab387) reaches the cell: dropping #[fg=FOLD_HC] fails here.
	[[ $(tmux -L out capture-pane -ep | grep -v '^$' | tail -1) == *"38;2;250;179;135m"* ]]
}

@test "folded: the right segment shows when wide and goes first when narrow" {
	tmux set -g @og_usage_short "claude 80%"
	fold_render 100 3
	[[ $LINE == *"claude 80%"* ]]
	fold_render 38 3
	[[ $LINE != *"claude 80%"* ]]
	[[ $LINE == *"3: lab3"* ]]
	[[ $LINE == *‹* ]]
}

@test "folded: a width that fits the list and session name but not the right segment drops only the segment" {
	tmux set -g @og_usage_short "claude 80%"
	fold_render 50 3
	[[ $LINE == *"3: lab3"* ]]
	[[ $LINE != *‹* && $LINE != *›* ]]
	[[ $LINE != *"claude 80%"* ]]
}
