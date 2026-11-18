#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} formats are literal tmux formats
# Auto-fold under a covering float (#1191). Runs the real tmux-status-autofold
# and tmux-reflow-windows against a private, config-less tmux server with real
# floating panes (bare `tmux` calls in the scripts resolve to it via
# TMUX_TMPDIR), plus the window-layout-changed / session-window-changed gate
# with a stub in place of the script to count forks. Needs a tmux with floating
# panes (`tmux new-pane`).

setup() {
	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	export XDG_DATA_HOME="$TDIR/data"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX KITTY_LISTEN_ON
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

	AUTOFOLD="$TDIR/autofold.sh"
	sed \
		-e "s|@lib_float_cover@|$PWD/scripts/lib-float-cover.sh|" \
		-e "s|@reflow@|$REFLOW|g" \
		-e "1s|.*|#!$BASH|" \
		scripts/tmux-status-autofold.sh >"$AUTOFOLD"
	chmod +x "$AUTOFOLD"

	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"

	tmux -f /dev/null new-session -d -s S -x 100 -y 40
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
	for _ in 1 2 3 4 5 6 7; do
		tmux new-window -d
	done
	tmux select-window -t S:0
	# A narrow client width makes the unfolded bar need several lines.
	bash "$REFLOW" S 30 --force >/dev/null 2>&1
	WANT=$(tmux show -v -t S @reflow_lines)
	[ "$WANT" -ge 3 ]
	[ "$(status_lines)" -eq "$WANT" ]
	SID=$(tmux display-message -p -t S '#{session_id}')
	WIN=$(tmux display-message -p -t S:0 '#{window_id}')
	WIN2=$(tmux display-message -p -t S:1 '#{window_id}')
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	tmux kill-server 2>/dev/null || true
}

# tmux prints a one-line bar as `on`.
status_lines() {
	local v
	v=$(tmux show -v -t S status)
	[[ $v == on ]] && v=1
	echo "$v"
}

# autofold [WINDOW]: what the hook gate forks.
autofold() {
	"$AUTOFOLD" "$SID" "${1:-$WIN}" 30
}

# make_float WIDTH HEIGHT X Y [new-pane flags...]: a float in $FLOAT, in $WIN.
make_float() {
	local w=$1 h=$2 x=$3 y=$4
	shift 4
	if ! FLOAT=$(tmux new-pane -P -F '#{pane_id}' -t "$WIN" -x "$w" -y "$h" -X "$x" -Y "$y" "$@" 'sleep 600' 2>/dev/null); then
		skip "this tmux cannot create a floating pane"
	fi
}

# A float spanning the whole window: every margin is zero.
cover_float() {
	local w h
	w=$(tmux display-message -p -t "$WIN" '#{window_width}')
	h=$(tmux display-message -p -t "$WIN" '#{window_height}')
	make_float "$w" "$h" 0 0 "$@"
}

folded() { tmux show -v -t S @og_status_folded 2>/dev/null || true; }
autofolded() { tmux show -v -t S @og_status_autofolded 2>/dev/null || true; }

assert_folded() {
	[ "$(folded)" = 1 ]
	[ "$(autofolded)" = 1 ]
	[ "$(status_lines)" = 1 ]
}

assert_unfolded() {
	[ "$(folded)" = 0 ]
	[ -z "$(autofolded)" ]
	[ "$(status_lines)" -eq "$WANT" ]
}

@test "a covering float folds the bar to one line" {
	cover_float
	autofold
	assert_folded
}

@test "closing the covering float unfolds the bar" {
	cover_float
	autofold
	assert_folded
	tmux kill-pane -t "$FLOAT"
	autofold
	assert_unfolded
}

@test "hiding the covering float in another window unfolds the bar" {
	cover_float
	autofold
	assert_folded
	tmux join-pane -d -s "$FLOAT" -t "$WIN2"
	autofold "$WIN"
	assert_unfolded
}

@test "switching to a float-free window unfolds the bar" {
	cover_float
	autofold
	assert_folded
	autofold "$WIN2"
	assert_unfolded
}

@test "a small float does not fold" {
	make_float 40 10 30 15
	autofold
	[ -z "$(folded)" ]
	[ "$(status_lines)" -eq "$WANT" ]
}

@test "a float leaving a visible tile strip on one side does not fold" {
	local w h
	w=$(tmux display-message -p -t "$WIN" '#{window_width}')
	h=$(tmux display-message -p -t "$WIN" '#{window_height}')
	make_float $((w - 12)) "$h" 12 0
	autofold
	[ -z "$(folded)" ]
	[ "$(status_lines)" -eq "$WANT" ]
}

@test "a manual fold survives a covering float opening and closing" {
	tmux set -t S @og_status_folded 1
	bash "$REFLOW" S 30 --force >/dev/null 2>&1
	cover_float
	autofold
	[ -z "$(autofolded)" ]
	tmux kill-pane -t "$FLOAT"
	autofold
	[ "$(folded)" = 1 ]
	[ -z "$(autofolded)" ]
	[ "$(status_lines)" = 1 ]
}

@test "a modal float covering the view does not fold" {
	cover_float -O
	[ "$(tmux display-message -p -t "$FLOAT" '#{pane_modal_flag}')" = 1 ] || skip "modal floats unavailable"
	autofold
	[ -z "$(folded)" ]
	[ "$(status_lines)" -eq "$WANT" ]
}

@test "a bridge float covering the view does not fold" {
	cover_float
	tmux set -p -t "$FLOAT" @bridge_pane 1
	autofold
	[ -z "$(folded)" ]
}

@test "a mirror window never folds" {
	cover_float
	tmux set -w -t "$WIN" @bridge_win 1
	autofold
	[ -z "$(folded)" ]
}

# --- the hook gate -----------------------------------------------------------

# Install the shipped window-layout-changed[40] / session-window-changed[40]
# gates from config/tmux.conf.tmpl with a stub in place of the script.
install_gate() {
	MARK="$TDIR/forks"
	: >"$MARK"
	local stub="$TDIR/stub.sh" conf="$TDIR/gate.conf"
	printf '#!%s\necho "$*" >>%s\n' "$BASH" "$MARK" >"$stub"
	chmod +x "$stub"
	grep -E '^set-hook -g (window-layout-changed|session-window-changed)\[40\]' config/tmux.conf.tmpl |
		sed "s#{{index \.Paths\.Scripts \"tmux-status-autofold\"}}#$stub#g" >"$conf"
	[ "$(wc -l <"$conf")" -eq 2 ]
	! grep -q '{{' "$conf"
	tmux source-file "$conf"
	sleep 0.3
	: >"$MARK"
}

# forks N: wait for the background run-shell jobs, then compare the count.
forks() {
	sleep 0.5
	[ "$(wc -l <"$MARK")" -eq "$1" ]
	: >"$MARK"
}

@test "gate: events with no float and nothing auto-folded fork nothing" {
	install_gate
	tmux split-window -d -t "$WIN"
	forks 0
	tmux kill-pane -t "$(tmux list-panes -t "$WIN" -F '#{pane_id}' | tail -1)"
	forks 0
	tmux select-window -t "$WIN2"
	forks 0
	tmux select-window -t "$WIN"
	forks 0
}

@test "gate: a covering float forks once; an unchanged-geometry event does not fork again" {
	install_gate
	local tile
	tile=$(tmux display-message -p -t "$WIN" '#{pane_id}')
	cover_float
	forks 1
	tmux split-window -d -t "$tile"
	forks 0
}

@test "gate: closing a covering float forks while auto-folded" {
	install_gate
	cover_float
	forks 1
	tmux set -t S @og_status_autofolded 1
	tmux kill-pane -t "$FLOAT"
	forks 1
}

@test "gate: opening a small float forks once" {
	install_gate
	make_float 40 10 30 15
	forks 1
}

@test "gate: closing a float forks nothing when nothing was auto-folded" {
	install_gate
	make_float 40 10 30 15
	forks 1
	tmux kill-pane -t "$FLOAT"
	forks 0
}

@test "gate: a modal float forks nothing" {
	install_gate
	cover_float -O
	[ "$(tmux display-message -p -t "$FLOAT" '#{pane_modal_flag}')" = 1 ] || skip "modal floats unavailable"
	forks 0
}

@test "gate: an inactive window forks nothing" {
	install_gate
	tmux select-window -t "$WIN2"
	forks 0
	cover_float
	forks 0
}
