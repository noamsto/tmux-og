#!/usr/bin/env bats
# prefix + Z (#970): the status bar folds to one line and back. Runs the real
# tmux-status-fold and tmux-reflow-windows against a private, config-less tmux
# server (bare `tmux` calls in the scripts resolve to it via TMUX_TMPDIR), the
# same pattern as reflow-fanout.bats, so it needs the plain tmux the nix check
# pins rather than tmux-og's wrapper (whose conf sets base-index 1).

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	export TMPDIR="$TDIR"
	export OG_FOLD_STEP_DELAY=0.01

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
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$REFLOW|g" \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-status-fold.sh >"$FOLD"
	chmod +x "$FOLD"

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
	tmux kill-server 2>/dev/null || true
}

# tmux prints a one-line bar as `on`.
status_lines() {
	local v
	v=$(tmux show -v -t S status)
	[[ $v == on ]] && v=1
	echo "$v"
}

# What the prefix + Z bind does: flip the flag in one command, then animate.
press() {
	tmux set -F -t S @og_status_folded '#{?#{==:#{@og_status_folded},1},0,1}'
	"$FOLD" S 60 >/dev/null 2>&1
}

press_bg() {
	tmux set -F -t S @og_status_folded '#{?#{==:#{@og_status_folded},1},0,1}'
	"$FOLD" S 60 >/dev/null 2>&1 &
}

# Log every `status` value a set-option leaves behind, so a test can see which
# line counts were stepped through.
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
	[ "$(tmux show -v -t S @og_status_cap)" = "" ]
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
	[ "$(tmux show -v -t S @og_status_cap)" = "" ]
}

@test "three rapid toggles end folded" {
	press_bg
	press_bg
	press_bg
	wait
	[ "$(tmux show -v -t S @og_status_folded)" = 1 ]
	[ "$(status_lines)" = 1 ]
}

@test "fold steps through every line count" {
	log_status
	press
	# The hook's run-shell is queued like any command: this one returns after it.
	tmux run-shell true
	tmux set-hook -gu after-set-option
	local k want
	for ((k = WANT - 1; k >= 1; k--)); do
		want=$k
		[[ $k == 1 ]] && want=on
		grep -qx "$want" "$STATUS_LOG"
	done
}

@test "a bridge mirror window jumps straight to the target" {
	tmux set -w -t S @bridge_win 1
	log_status
	press
	# The hook's run-shell is queued like any command: this one returns after it.
	tmux run-shell true
	tmux set-hook -gu after-set-option
	[ "$(status_lines)" = 1 ]
	# Only the starting height and the end: no intermediate line count.
	run grep -vxE "$WANT|on" "$STATUS_LOG"
	[ -z "$output" ]
	press
	[ "$(status_lines)" -eq "$WANT" ]
}
