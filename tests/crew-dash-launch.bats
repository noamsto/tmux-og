#!/usr/bin/env bats
# Tests scripts/tmux-crew-dash.sh (#1139): open `crew dash` in a labelled floating
# pane, or focus or restore the one this window already has.
#
# Runs the real script against a private, config-less tmux server (like
# tests/float-group.bats), with `crew` stubbed and a shim exposing the sibling
# tmux-float-group. Needs the pinned next-3.9 tmux (mkTmux in flake.nix's check):
# the stock tmux has no floating panes and no break-pane -W.

setup() {
	OG_TMUX_DIR="/tmp/og-cd-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX TMUX_PANE
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"
	REAL_TMUX="$(command -v tmux)"
	export REAL_TMUX
	export OG_SHIM_LOG="$BATS_TEST_TMPDIR/shim.log"
	: >"$OG_SHIM_LOG"

	# crew and tmux-float-group go on PATH before the server starts: the dash pane
	# runs `exec crew dash` with the server's environment, not the script's.
	BIN_DIR="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$BIN_DIR"
	printf '#!%s\nexec sleep 3600\n' "$BASH" >"$BIN_DIR/crew"
	printf '#!%s\nexec bash "%s/scripts/tmux-float-group.sh" "$@"\n' "$BASH" "$PWD" >"$BIN_DIR/tmux-float-group"
	chmod +x "$BIN_DIR/crew" "$BIN_DIR/tmux-float-group"
	PATH="$BIN_DIR:$PATH"

	# -f /dev/null: the real config would arm hooks that move floats behind the
	# assertions below.
	tmux -f /dev/null new-session -d -s S -x 200 -y 50 -c "$BATS_TEST_TMPDIR"
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	TILE="$(tmux display-message -p -t "$WIN" '#{pane_id}')"
	tmux split-window -d -t "$WIN" -c "$BATS_TEST_TMPDIR"

	# A tmux in front of the real one that logs notifications and new-pane calls.
	SHIM_DIR="$BATS_TEST_TMPDIR/shim"
	mkdir -p "$SHIM_DIR"
	printf '#!%s\n' "$BASH" >"$SHIM_DIR/tmux"
	cat >>"$SHIM_DIR/tmux" <<'EOF'
if [[ ${1:-} == display-message && ${2:-} != -p ]] || [[ ${1:-} == new-pane ]]; then
	printf '%s\n' "$*" >>"$OG_SHIM_LOG"
fi
exec "$REAL_TMUX" "$@"
EOF
	chmod +x "$SHIM_DIR/tmux"
	PATH="$SHIM_DIR:$PATH"
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	# The real binary, not the PATH shim: a broken shim must not leak the server.
	"$REAL_TMUX" kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

crew_dash() { bash scripts/tmux-crew-dash.sh "$@"; }
float_group() { bash scripts/tmux-float-group.sh "$@"; }
messages() { cat "$OG_SHIM_LOG"; }

# dash_panes [window]: ids of crew-dash floats in a window; with no window, every
# crew-dash pane of the server, a stashed one (a tile) included.
dash_panes() {
	local scope=(-a) floating=0
	[[ -z ${1:-} ]] || scope=(-t "$1") floating=1
	tmux list-panes "${scope[@]}" -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}' |
		awk -F'|' -v f="$floating" '(!f || $2 == 1) && $3 == "crew-dash" { print $1 }'
}

pane_count() { tmux list-panes -t "$1" | wc -l; }
active_pane() { tmux display-message -p -t "$1" '#{pane_id}'; }

snap() {
	tmux display-message -p -t "$1" \
		'#{pane_floating_flag}|#{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}|#{@float_geom}|#{pane_border_lines}'
}

# first_dash: press once from the tile and echo the dash id.
first_dash() {
	crew_dash "$TILE"
	local id
	id="$(dash_panes "$WIN")"
	[[ $id == %* ]] || skip "this tmux cannot create a floating pane"
	echo "$id"
}

@test "first press creates one labelled float with the stamps and focuses it" {
	local id
	id="$(first_dash)"
	[ "$(dash_panes "$WIN" | wc -l)" -eq 1 ]
	[ "$(pane_count "$WIN")" -eq 3 ]
	[ "$(tmux display-message -p -t "$id" '#{@float_geom}|#{@float_group}|#{@pane_label}')" = "90% 90% 5% 5%|crew|crew-dash" ]
	[ "$(tmux show-options -pv -t "$id" pane-border-lines)" = "heavy" ]
	[ "$(tmux show-options -pv -t "$id" remain-on-exit)" = "off" ]
	[ "$(active_pane "$WIN")" = "$id" ]
}

@test "the dash runs crew dash from the pressed pane's directory" {
	local id
	id="$(first_dash)"
	[ "$(tmux display-message -p -t "$id" '#{pane_current_path}')" = "$BATS_TEST_TMPDIR" ]
	[ "$(tmux display-message -p -t "$id" '#{pane_start_command}')" = '"exec crew dash"' ]
}

@test "a second press focuses the open dash without creating another" {
	local id
	id="$(first_dash)"
	tmux select-pane -t "$TILE"
	[ "$(active_pane "$WIN")" = "$TILE" ]

	run crew_dash "$TILE"
	[ "$status" -eq 0 ]
	[ "$(dash_panes | wc -l)" -eq 1 ]
	[ "$(pane_count "$WIN")" -eq 3 ]
	[ "$(active_pane "$WIN")" = "$id" ]
}

@test "TMUX_PANE is the fallback when no pane id is given" {
	run env TMUX_PANE="$TILE" bash scripts/tmux-crew-dash.sh
	[ "$status" -eq 0 ]
	[ "$(dash_panes "$WIN" | wc -l)" -eq 1 ]
}

@test "a pane id from another window targets that pane's window" {
	local w2 p2 id
	w2="$(tmux new-window -d -P -F '#{window_id}' -t S: -c "$BATS_TEST_TMPDIR")"
	p2="$(tmux display-message -p -t "$w2" '#{pane_id}')"
	[ "$(active_pane "$WIN")" = "$(tmux display-message -p -t S '#{pane_id}')" ]

	run env -u TMUX_PANE bash scripts/tmux-crew-dash.sh "$p2"
	[ "$status" -eq 0 ]
	id="$(dash_panes "$w2")"
	[[ $id == %* ]]
	[ "$(dash_panes "$WIN" | wc -l)" -eq 0 ]
	[ "$(pane_count "$WIN")" -eq 2 ]
}

@test "each window gets its own dash" {
	local w2 p2
	first_dash >/dev/null
	w2="$(tmux new-window -d -P -F '#{window_id}' -t S: -c "$BATS_TEST_TMPDIR")"
	p2="$(tmux display-message -p -t "$w2" '#{pane_id}')"

	crew_dash "$p2"
	[ "$(dash_panes "$w2" | wc -l)" -eq 1 ]
	[ "$(dash_panes "$WIN" | wc -l)" -eq 1 ]
}

@test "a press after tmux-float-group hide restores the dash instead of duplicating" {
	local id before
	id="$(first_dash)"
	before="$(snap "$id")"
	float_group hide crew "$TILE"
	[ "$(dash_panes "$WIN" | wc -l)" -eq 0 ]
	[ "$(dash_panes | wc -l)" -eq 1 ]

	run crew_dash "$TILE"
	[ "$status" -eq 0 ]
	[ "$(dash_panes | wc -l)" -eq 1 ]
	[ "$(dash_panes "$WIN")" = "$id" ]
	[ "$(snap "$id")" = "$before" ]
	[ "$(active_pane "$WIN")" = "$id" ]
}

@test "--detached creates the dash without taking focus" {
	local before id
	before="$(active_pane "$WIN")"
	run crew_dash --detached "$TILE"
	[ "$status" -eq 0 ]
	id="$(dash_panes "$WIN")"
	[[ $id == %* ]] || skip "this tmux cannot create a floating pane"
	[ "$(active_pane "$WIN")" = "$before" ]
	[ "$(tmux display-message -p -t "$id" '#{@pane_label}|#{@float_group}')" = "crew-dash|crew" ]
}

@test "--detached on a visible dash leaves focus alone" {
	first_dash >/dev/null
	tmux select-pane -t "$TILE"

	run crew_dash --detached "$TILE"
	[ "$status" -eq 0 ]
	[ "$(active_pane "$WIN")" = "$TILE" ]
	[ "$(dash_panes | wc -l)" -eq 1 ]
}

@test "--detached on a stashed dash restores it without taking focus" {
	local id
	id="$(first_dash)"
	float_group hide crew "$TILE"
	tmux select-pane -t "$TILE"

	run crew_dash --detached "$TILE"
	[ "$status" -eq 0 ]
	[ "$(dash_panes "$WIN")" = "$id" ]
	[ "$(dash_panes | wc -l)" -eq 1 ]
	[ "$(active_pane "$WIN")" = "$TILE" ]
}

@test "a stashed dash with no tmux-float-group on PATH reports and creates nothing" {
	first_dash >/dev/null
	float_group hide crew "$TILE"

	run env PATH="$SHIM_DIR:$(dirname "$(command -v bash)")" bash scripts/tmux-crew-dash.sh "$TILE"
	[ "$status" -eq 0 ]
	[ "$(dash_panes | wc -l)" -eq 1 ]
	[ "$(dash_panes "$WIN" | wc -l)" -eq 0 ]
	messages | grep -q 'tmux-float-group is not on PATH'
}

@test "a normal window passes -c with the pressed pane's directory" {
	first_dash >/dev/null
	grep -q -- "^new-pane .* -c $BATS_TEST_TMPDIR " "$OG_SHIM_LOG"
}

@test "a @bridge_win window gets the float without -c" {
	tmux set-option -w -t "$WIN" @bridge_win 1
	run crew_dash "$TILE"
	[ "$status" -eq 0 ]
	[ "$(dash_panes "$WIN" | wc -l)" -eq 1 ]
	grep -q '^new-pane ' "$OG_SHIM_LOG"
	[ "$(grep -c -- ' -c ' "$OG_SHIM_LOG")" -eq 0 ]
}

@test "crew missing from PATH reports and creates no pane" {
	# bash alone on PATH next to the shim: the stub dir (and any real crew) is out.
	run env PATH="$SHIM_DIR:$(dirname "$(command -v bash)")" bash scripts/tmux-crew-dash.sh "$TILE"
	[ "$status" -eq 0 ]
	[ "$(pane_count "$WIN")" -eq 2 ]
	[ "$(dash_panes | wc -l)" -eq 0 ]
	messages | grep -q 'crew is not on PATH'
}
