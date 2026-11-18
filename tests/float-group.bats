#!/usr/bin/env bats
# Tests scripts/tmux-float-group.sh (#1139): hide and show a named group of
# floating panes together. The pin has no native hide-pane, so a hidden float is
# parked in its own window of the session scratch-og-float-stash and floated back
# with join-pane + break-pane -W.
#
# Runs the real script against a private, config-less tmux server (like
# tests/float-nudge.bats). Needs the pinned next-3.9 tmux (mkTmux in flake.nix's
# check): the stock tmux has no floating panes and no break-pane -W.

bats_require_minimum_version 1.5.0

setup() {
	OG_TMUX_DIR="/tmp/og-fg-$$-${BATS_TEST_NUMBER}"
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
	SHIM_DIR="$BATS_TEST_TMPDIR/shim"
	export OG_SHIM_LOG="$BATS_TEST_TMPDIR/shim.log"
	: >"$OG_SHIM_LOG"

	# -f /dev/null: the real config would arm hooks that move floats behind the
	# assertions below.
	tmux -f /dev/null new-session -d -s S -x 200 -y 50
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	TILE="$(tmux display-message -p -t "$WIN" '#{pane_id}')"
	tmux split-window -d -t "$WIN"
	STASH=scratch-og-float-stash
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	# The real binary, not the PATH shim: a broken shim must not leak the server.
	"$REAL_TMUX" kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
}

# A tmux in front of the real one that logs notifications and can hide `-W` from
# break-pane's usage or fail join-pane or list-windows -a. Installed per test.
use_shim() {
	mkdir -p "$SHIM_DIR"
	printf '#!%s\n' "$BASH" >"$SHIM_DIR/tmux"
	cat >>"$SHIM_DIR/tmux" <<'EOF'
if [[ ${1:-} == display-message && ${2:-} != -p ]]; then
	printf '%s\n' "$*" >>"$OG_SHIM_LOG"
fi
if [[ -n ${OG_SHIM_HIDE_W:-} && ${1:-} == list-commands && ${2:-} == break-pane ]]; then
	"$REAL_TMUX" "$@" | sed 's/W\]/]/'
	exit 0
fi
if [[ -n ${OG_SHIM_FAIL_JOIN:-} && ${1:-} == join-pane ]]; then
	exit 1
fi
if [[ -n ${OG_SHIM_SLEEP_JOIN:-} && ${1:-} == join-pane ]]; then
	exec sleep 30
fi
if [[ -n ${OG_SHIM_FAIL_LISTWIN:-} && ${1:-} == list-windows && ${2:-} == -a ]]; then
	exit 1
fi
exec "$REAL_TMUX" "$@"
EOF
	chmod +x "$SHIM_DIR/tmux"
	PATH="$SHIM_DIR:$PATH"
}

# new_float <window> <w> <h> <x> <y> [group]: a heavy-bordered float stamped with
# its creation geometry. Echoes the pane id.
new_float() {
	local id
	if ! id="$(tmux new-pane -d -P -F '#{pane_id}' -t "$1" -x "$2" -y "$3" -X "$4" -Y "$5" -B heavy 2>/dev/null)"; then
		skip "this tmux cannot create a floating pane"
	fi
	tmux set -p -t "$id" @float_geom "$2 $3 $4 $5"
	[[ -z ${6:-} ]] || tmux set -p -t "$id" @float_group "$6"
	echo "$id"
}

snap() {
	tmux display-message -p -t "$1" \
		'#{pane_floating_flag}|#{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}|#{@float_geom}|#{pane_border_lines}'
}

in_window() { tmux list-panes -t "${2:-$WIN}" -F '#{pane_id}' | grep -qx "$1"; }
stash_exists() { tmux has-session -t "=$STASH" 2>/dev/null; }
float_group() { bash scripts/tmux-float-group.sh "$@"; }
messages() { cat "$OG_SHIM_LOG"; }
zoomed_tile() { tmux list-panes -t "${1:-$WIN}" -f '#{pane_zoomed_flag}' -F '#{pane_id}'; }
tile_heights() { tmux list-panes -t "$WIN" -f '#{!:#{pane_floating_flag}}' -F '#{pane_height}' | tr '\n' ' '; }

# A copy of the window-layout-changed[20] dwindle hook in config/tmux.conf.tmpl
# with `tmux-dwindle balance` swapped for an even-vertical select-layout, which
# is what it does to a stacked column.
install_dwindle_hook() {
	local hook
	read -r -d '' hook <<'HOOK' || true
if -F '#{&&:#{==:#{@dwindle},on},#{!:#{@bridge_win}}}' { if -F '#{!=:#{n:#{P:#{?pane_floating_flag,,x}}}_#{window_zoomed_flag}_#{window_width}x#{window_height}_#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }},#{s/[|].*//:@dwindle_cache}}' { if -F '#{&&:#{e|<|:#{n:#{P:#{?pane_floating_flag,,x}}},#{s/_.*//:@dwindle_cache}},#{&&:#{m/r:^[0-9]+_0_,#{@dwindle_cache}},#{&&:#{!:#{window_zoomed_flag}},#{!=:#{@crew_grid},1}}}}' { select-layout even-vertical } ; set -wF @dwindle_cache '#{n:#{P:#{?pane_floating_flag,,x}}}_#{window_zoomed_flag}_#{window_width}x#{window_height}_#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}|#{window_layout}' } } { if -F '#{@dwindle_cache}' { set -wu @dwindle_cache } }
HOOK
	tmux set-hook -g 'window-layout-changed[20]' "$hook"
	tmux set -gw @dwindle on
}

@test "hide then show keeps the float flag, geometry, stamp and border" {
	local f before
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	before="$(snap "$f")"
	[[ $before == 1\|* ]]

	run float_group hide crew "$TILE"
	[ "$status" -eq 0 ]
	run ! in_window "$f"
	stash_exists

	run float_group show crew "$TILE"
	[ "$status" -eq 0 ]
	in_window "$f"
	[ "$(snap "$f")" = "$before" ]
	[ "$(tmux display-message -p -t "$f" '#{@float_group_home}#{@float_group_cells}')" = "" ]
	[ "$(tmux show-options -pv -t "$f" remain-on-exit)" = "off" ]
}

@test "a float without @float_geom comes back at its saved cells" {
	local f before
	f="$(new_float "$WIN" 60 15 30 8 crew)"
	tmux set -p -u -t "$f" @float_geom
	before="$(snap "$f")"

	float_group hide crew "$TILE"
	[ "$(tmux display-message -p -t "$f" '#{@float_group_cells}')" = "60 15 30 8" ]
	float_group show crew "$TILE"

	[ "$(snap "$f")" = "$before" ]
}

@test "the stash session is gone once the last pane is restored" {
	local a b
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	b="$(new_float "$WIN" 40% 40% 50% 50% crew)"

	float_group hide crew "$TILE"
	stash_exists
	[ "$(tmux list-panes -s -t "$STASH" | wc -l)" -eq 2 ]
	[ "$(tmux list-windows -t "$STASH" | wc -l)" -eq 2 ]

	float_group show crew "$TILE"
	run ! stash_exists
	in_window "$a"
	in_window "$b"
	[ "$(tmux display-message -p -t "$a" '#{pane_floating_flag}')" = 1 ]
	[ "$(tmux display-message -p -t "$b" '#{pane_floating_flag}')" = 1 ]
}

@test "hiding hands focus to a tiled pane" {
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	tmux select-pane -t "$f"

	float_group hide crew "$TILE"

	[ "$(tmux display-message -p -t "$WIN" '#{pane_floating_flag}')" = 0 ]
}

@test "the tiled layout is unchanged by hide and show" {
	local f before after
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	before="$(tmux list-panes -t "$WIN" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id} #{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}')"

	float_group hide crew "$TILE"
	float_group show crew "$TILE"

	after="$(tmux list-panes -t "$WIN" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id} #{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}')"
	[ "$before" = "$after" ]
}

@test "untagged floats and other groups are untouched" {
	local tagged untagged other
	tagged="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	untagged="$(new_float "$WIN" 40% 40% 50% 50%)"
	other="$(new_float "$WIN" 30% 30% 10% 60% review)"
	local before_u before_o
	before_u="$(snap "$untagged")"
	before_o="$(snap "$other")"

	float_group hide crew "$TILE"

	run ! in_window "$tagged"
	in_window "$untagged"
	in_window "$other"
	[ "$(snap "$untagged")" = "$before_u" ]
	[ "$(snap "$other")" = "$before_o" ]
}

@test "a float with @bridge_pane is never touched" {
	local mine bridged
	mine="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	bridged="$(new_float "$WIN" 40% 40% 50% 50% crew)"
	tmux set -p -t "$bridged" @bridge_pane '%9'

	float_group hide crew "$TILE"

	run ! in_window "$mine"
	in_window "$bridged"
	[ "$(tmux display-message -p -t "$bridged" '#{pane_floating_flag}')" = 1 ]
}

@test "a second window's tagged floats are unaffected" {
	local win2 tile2 a b
	win2="$(tmux new-window -d -t S -P -F '#{window_id}')"
	tile2="$(tmux display-message -p -t "$win2" '#{pane_id}')"
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	b="$(new_float "$win2" 50% 50% 5% 5% crew)"

	float_group hide crew "$TILE"
	in_window "$b" "$win2"
	run ! in_window "$a"

	float_group hide crew "$tile2"
	run ! in_window "$b" "$win2"

	float_group show crew "$TILE"
	in_window "$a"
	run ! in_window "$b" "$win2"
}

@test "a pane id acts on that pane's window with TMUX_PANE unset" {
	local win2 tile2 a b
	win2="$(tmux new-window -d -t S -P -F '#{window_id}')"
	tile2="$(tmux display-message -p -t "$win2" '#{pane_id}')"
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	b="$(new_float "$win2" 50% 50% 5% 5% crew)"

	run env -u TMUX_PANE bash scripts/tmux-float-group.sh hide crew "$tile2"
	[ "$status" -eq 0 ]

	in_window "$a"
	run ! in_window "$b" "$win2"
}

@test "TMUX_PANE is the fallback for the pane id" {
	local a
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"

	TMUX_PANE="$TILE" float_group hide crew

	run ! in_window "$a"
}

@test "toggle hides visible tagged floats, else shows" {
	local a
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"

	float_group toggle crew "$TILE"
	run ! in_window "$a"
	float_group toggle crew "$TILE"
	in_window "$a"
	[ "$(tmux display-message -p -t "$a" '#{pane_floating_flag}')" = 1 ]
}

@test "show focuses the restored float; --no-focus keeps the caller's pane" {
	local a
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	float_group hide crew "$TILE"
	tmux select-pane -t "$TILE"

	float_group show crew "$TILE" --no-focus
	in_window "$a"
	[ "$(tmux display-message -p -t "$WIN" '#{pane_id}')" = "$TILE" ]

	float_group hide crew "$TILE"
	tmux select-pane -t "$TILE"
	float_group show crew "$TILE"
	[ "$(tmux display-message -p -t "$WIN" '#{pane_id}')" = "$a" ]
}

@test "hide and show are refused in a @bridge_win window" {
	use_shim
	local a
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"

	tmux set -w -t "$WIN" @bridge_win 1
	run float_group hide crew "$TILE"
	[ "$status" -eq 0 ]
	in_window "$a"
	run ! stash_exists
	messages | grep -q 'mirror window'

	tmux set -w -t "$WIN" -u @bridge_win
	float_group hide crew "$TILE"
	run ! in_window "$a"

	: >"$OG_SHIM_LOG"
	tmux set -w -t "$WIN" @bridge_win 1
	run float_group show crew "$TILE"
	[ "$status" -eq 0 ]
	run ! in_window "$a"
	stash_exists
	messages | grep -q 'mirror window'
}

@test "orphaned stash panes are reaped on the next call" {
	local win2 a b
	win2="$(tmux new-window -d -t S -P -F '#{window_id}')"
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	b="$(new_float "$win2" 50% 50% 5% 5% crew)"
	float_group hide crew "$b"
	stash_exists

	tmux kill-window -t "$win2"
	float_group show crew "$TILE"

	run ! stash_exists
	[[ " $(tmux list-panes -a -F '#{pane_id}' | tr '\n' ' ') " != *" $b "* ]]
	in_window "$a"
}

@test "refused with a message when break-pane has no -W" {
	use_shim
	local a before
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	before="$(snap "$a")"

	OG_SHIM_HIDE_W=1 run float_group hide crew "$TILE"
	[ "$status" -eq 0 ]

	in_window "$a"
	[ "$(snap "$a")" = "$before" ]
	run ! stash_exists
	messages | grep -q 'no break-pane -W'
}

@test "a failed hide leaves the float visible and reports it once per float" {
	use_shim
	local a b
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	b="$(new_float "$WIN" 40% 40% 50% 50% crew)"

	OG_SHIM_FAIL_JOIN=1 run float_group hide crew "$TILE"
	[ "$status" -eq 0 ]

	in_window "$a"
	in_window "$b"
	[ "$(tmux display-message -p -t "$a" '#{pane_floating_flag}|#{@float_group_home}|#{@float_group_cells}')" = "1||" ]
	[ "$(messages | grep -c 'could not hide')" -eq 2 ]
	run ! stash_exists
}

@test "tag marks a floating pane and ignores a tiled one" {
	use_shim
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5%)"

	run float_group tag crew "$f"
	[ "$status" -eq 0 ]
	[ "$(tmux display-message -p -t "$f" '#{@float_group}')" = "crew" ]

	run float_group tag crew "$TILE"
	[ "$status" -eq 0 ]
	[ -z "$(tmux display-message -p -t "$TILE" '#{@float_group}')" ]
	messages | grep -q 'not a floating pane'
}

@test "bad arguments are a silent no-op" {
	run float_group
	[ "$status" -eq 0 ]
	run float_group bogus crew "$TILE"
	[ "$status" -eq 0 ]
	run float_group hide "" "$TILE"
	[ "$status" -eq 0 ]
	run float_group hide crew "not-a-pane"
	[ "$status" -eq 0 ]
	run ! stash_exists
}

@test "hide and show keep a zoomed tile zoomed" {
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	tmux select-pane -t "$TILE"
	tmux resize-pane -Z -t "$TILE"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group hide crew "$TILE"
	run ! in_window "$f"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group show crew "$TILE" --no-focus
	in_window "$f"
	[ "$(zoomed_tile)" = "$TILE" ]
	[ "$(tmux display-message -p -t "$WIN" '#{pane_id}')" = "$TILE" ]
}

@test "a focused float over a zoomed tile survives hide and show, focus included" {
	local f
	# -A: only a float above the zoom can take focus without unzooming.
	f="$(tmux new-pane -d -A -P -F '#{pane_id}' -t "$WIN" -x 50% -y 50% -X 5% -Y 5% -B heavy)" || skip "this tmux cannot create a floating pane"
	tmux set -p -t "$f" @float_geom "50% 50% 5% 5%"
	tmux set -p -t "$f" @float_group crew
	tmux select-pane -t "$TILE"
	tmux resize-pane -Z -t "$TILE"
	tmux select-pane -Z -t "$f"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group hide crew "$TILE"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group show crew "$TILE"
	[ "$(zoomed_tile)" = "$TILE" ]
	[ "$(tmux display-message -p -t "$WIN" '#{pane_id}')" = "$f" ]
}

@test "hiding a zoomed float leaves the window unzoomed" {
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	tmux select-pane -t "$f"
	tmux resize-pane -Z -t "$f"

	float_group hide crew "$TILE"

	[ "$(tmux display-message -p -t "$WIN" '#{window_zoomed_flag}')" = 0 ]
}

@test "show keeps uneven tiled sizes when the dwindle hook is armed" {
	local f before second
	tmux split-window -d -t "$WIN"
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	install_dwindle_hook
	second="$(tmux list-panes -t "$WIN" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id}' | sed -n 2p)"
	tmux resize-pane -t "$TILE" -y 25
	tmux resize-pane -t "$second" -y 6
	before="$(tile_heights)"
	[ "$(echo "$before" | tr ' ' '\n' | sort -u | grep -c .)" -gt 1 ]

	float_group hide crew "$TILE"
	float_group show crew "$TILE"

	in_window "$f"
	[ "$(tile_heights)" = "$before" ]
	[ "$(tmux display-message -p -t "$WIN" '#{@dwindle}')" = on ]
	[ -n "$(tmux display-message -p -t "$WIN" '#{@dwindle_cache}')" ]
}

@test "show leaves a window-local @dwindle value as it was" {
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	install_dwindle_hook
	tmux set -w -t "$WIN" @dwindle on

	float_group hide crew "$TILE"
	float_group show crew "$TILE"
	[ "$(tmux show-options -wqv -t "$WIN" @dwindle)" = on ]

	tmux set -w -t "$WIN" -u @dwindle
	float_group hide crew "$TILE"
	float_group show crew "$TILE"
	[ -z "$(tmux show-options -wqv -t "$WIN" @dwindle)" ]
	[ "$(tmux display-message -p -t "$WIN" '#{@dwindle}')" = on ]
}

@test "a failed list-windows does not reap hidden floats" {
	use_shim
	local a
	a="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	float_group hide crew "$TILE"
	stash_exists

	OG_SHIM_FAIL_LISTWIN=1 run float_group hide crew "$TILE"
	[ "$status" -eq 0 ]

	stash_exists
	[[ " $(tmux list-panes -a -F '#{pane_id}' | tr '\n' ' ') " == *" $a "* ]]
	float_group show crew "$TILE"
	in_window "$a"
}

@test "hide, show, hide keeps the window zoomed for a float made without -A" {
	local f
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	tmux select-pane -t "$TILE"
	tmux resize-pane -Z -t "$TILE"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group hide crew "$TILE"
	float_group show crew "$TILE"
	in_window "$f"
	[ "$(zoomed_tile)" = "$TILE" ]

	float_group hide crew "$TILE"
	run ! in_window "$f"
	[ "$(zoomed_tile)" = "$TILE" ]
}

@test "a show killed between the dwindle pause and resume leaves @dwindle restored" {
	local f pid child
	f="$(new_float "$WIN" 50% 50% 5% 5% crew)"
	install_dwindle_hook
	tmux set -w -t "$WIN" @dwindle on
	float_group hide crew "$TILE"

	# Stall the script inside the paused window: a join-pane that sleeps.
	use_shim
	OG_SHIM_SLEEP_JOIN=1 bash scripts/tmux-float-group.sh show crew "$TILE" &
	pid=$!
	for _ in $(seq 50); do
		[ "$(tmux show-options -wqv -t "$WIN" @dwindle)" = off ] && break
		sleep 0.1
	done
	[ "$(tmux show-options -wqv -t "$WIN" @dwindle)" = off ]
	child=$(pgrep -P "$pid")
	kill -TERM "$pid"
	wait "$pid" || true
	# The orphaned shim sleep would hold bats' fds open for its full 30 s.
	[[ -z $child ]] || kill "$child" 2>/dev/null || true

	[ "$(tmux show-options -wqv -t "$WIN" @dwindle)" = on ]
}
