#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
bats_require_minimum_version 1.5.0
# A mouse drag on a mirrored float's border reaches the REMOTE float (#797):
# a real daemon (--test-local, two tmux -L servers, no ssh), the real emitted
# conf on the local server, and a real attached client fed SGR mouse input from
# a pty host. Without the drag binds the local float moves and the remote one
# never does; with them both land on the same geometry.
#
# SRC is the raw pinned binary with a minimal conf (the "remote"); DST is the
# wrapped tmux with the emitted conf, and that wrapper is first on PATH, so the
# daemon's local commands and the conf's own ctl run exactly as they would for
# a user. The local server's status line sits on top, so every SGR row carries
# its height.
#
# The same harness also covers the TILED divider case (#823): dragging the
# border between two tiled mirror panes must resize the REMOTE panes, routed
# through ctl `drag` -> the daemon's `tile-layout` verb, guarded so a stale or
# reordered layout is refused and the mirror snaps back to the remote's truth.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TMUX_RAW="${TMUX_RAW:?set TMUX_RAW to the raw pinned binary}"
	DAEMON="${DAEMON:?set DAEMON to the built og-remote-bridge-daemon}"
	RENDERER="${RENDERER:?set RENDERER to the built og-remote-bridge-renderer}"
	CTL="${CTL:?set CTL to the built og-remote-bridge-ctl}"

	# A short fixed dir: tmux -L resolves under $TMUX_TMPDIR and the unix
	# socket path limit is 108 chars.
	OG_TMUX_DIR="/tmp/og-fd-bats-$$"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX

	TEST_HOME="$BATS_TEST_TMPDIR/home"
	mkdir -p "$TEST_HOME"
	export HOME="$TEST_HOME"
	export XDG_CACHE_HOME="$TEST_HOME/.cache"
	export XDG_CONFIG_HOME="$TEST_HOME/.config"
	export XDG_STATE_HOME="$TEST_HOME/.local/state"
	export TERM=xterm-256color
	# The emitted conf's monitor hooks sweep these dirs; keep them private (#603).
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	PATH="$(dirname "$TMUX_BIN"):$PATH"

	SRC_CONF="$BATS_TEST_TMPDIR/src.conf"
	printf 'set -g base-index 1\nset -g pane-base-index 1\nset -g pane-border-status top\nset -g window-size latest\nset -g aggressive-resize on\n' >"$SRC_CONF"
	SRC="$TMUX_RAW -L fdsrc -f $SRC_CONF"
	DST="$TMUX_BIN -L fddst"
	OBS="$TMUX_RAW -L fdobs -f /dev/null"
	SOCK="$BATS_TEST_TMPDIR/d.sock"
}

teardown() {
	[[ -n ${DAEMON_PID:-} ]] && kill "$DAEMON_PID" 2>/dev/null
	[[ -n ${DAEMON_PID:-} ]] && wait "$DAEMON_PID" 2>/dev/null
	$OBS kill-server 2>/dev/null || true
	$DST kill-server 2>/dev/null || true
	$SRC kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
	return 0
}

BUDGET_SECS=12

# mirror_up mirrors a remote window holding a tiled split and one float, then
# attaches a real client to the mirror. Sets RF (remote float), LF (its local
# mirror) and LT (the local mirror of the remote's right-hand tiled pane). Any
# args override the remote float's new-pane geometry/border flags (default
# -x 40 -y 12 -X 10 -Y 5).
mirror_up() {
	local float_args=("$@")
	((${#float_args[@]})) || float_args=(-x 40 -y 12 -X 10 -Y 5)
	$SRC new-session -d -s rem -x 100 -y 30
	local rt
	rt="$($SRC split-window -d -h -t rem -P -F '#{pane_id}' "sleep 300")"
	RF="$($SRC new-pane -d -t rem "${float_args[@]}" -P -F '#{pane_id}' "sleep 300")"
	$DST new-session -d -s host-sess -x 100 -y 30
	# The splash popup would take the first click.
	$DST set-option -g @splash_shown 1
	"$DAEMON" --test-local --src-socket fdsrc --dst-socket fddst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$SOCK" >"$BATS_TEST_TMPDIR/daemon.log" 2>&1 &
	DAEMON_PID=$!
	$OBS new-session -d -s obs -x 100 -y 32 "env -u TMUX TERM=xterm-256color $TMUX_BIN -L fddst attach -t host-sess"

	local deadline=$((SECONDS + BUDGET_SECS))
	LF="" LT=""
	while ((SECONDS < deadline)); do
		LF="$(mirror_of "$RF")"
		LT="$(mirror_of "$rt")"
		if [[ -n $LF && -n $LT ]] && [[ "$(geom "$DST" "$LF")" == "$(geom "$SRC" "$RF")" ]] &&
			[[ "$($DST list-clients -t host-sess 2>/dev/null | wc -l)" -ge 1 ]]; then
			return 0
		fi
		sleep 0.1
	done
	printf 'mirror never came up: LF=%s LT=%s local=%s remote=%s\n--- daemon log ---\n' \
		"$LF" "$LT" "$(geom "$DST" "$LF" 2>&1)" "$(geom "$SRC" "$RF")" >&3
	tail -30 "$BATS_TEST_TMPDIR/daemon.log" >&3
	return 1
}

# mirror_tiled_up mirrors a remote window holding just a -h tiled split (no
# float), then attaches a real client to the mirror. Any args are eval'd, in
# order, after the base split — each may reference $rl/$rt (the remote left/
# right pane ids, local to this function) to shape the layout further before
# the daemon starts. Sets RL, RT (remote left/right tiled panes) and LL, LT
# (their local mirrors).
mirror_tiled_up() {
	local rl rt
	rl="$($SRC new-session -d -s rem -x 100 -y 30 -P -F '#{pane_id}')"
	rt="$($SRC split-window -d -h -t rem -P -F '#{pane_id}' "sleep 300")"
	local shape
	for shape in "$@"; do
		eval "$shape"
	done
	RL="$rl" RT="$rt"
	$DST new-session -d -s host-sess -x 100 -y 30
	# The splash popup would take the first click.
	$DST set-option -g @splash_shown 1
	"$DAEMON" --test-local --src-socket fdsrc --dst-socket fddst \
		--session rem --window 1 --local-sess host-sess \
		--renderer "$RENDERER" --sock "$SOCK" >"$BATS_TEST_TMPDIR/daemon.log" 2>&1 &
	DAEMON_PID=$!
	$OBS new-session -d -s obs -x 100 -y 32 "env -u TMUX TERM=xterm-256color $TMUX_BIN -L fddst attach -t host-sess"

	local deadline=$((SECONDS + BUDGET_SECS))
	LL="" LT=""
	while ((SECONDS < deadline)); do
		LL="$(mirror_of "$RL")"
		LT="$(mirror_of "$RT")"
		if [[ -n $LL && -n $LT ]] && [[ "$(tiled_geoms "$DST")" == "$(tiled_geoms "$SRC")" ]] &&
			[[ "$($DST list-clients -t host-sess 2>/dev/null | wc -l)" -ge 1 ]]; then
			return 0
		fi
		sleep 0.1
	done
	printf 'tiled mirror never agreed: LL=%s LT=%s\nlocal:\n%s\nremote:\n%s\n--- daemon log ---\n' \
		"$LL" "$LT" "$(tiled_geoms "$DST" 2>&1)" "$(tiled_geoms "$SRC")" >&3
	tail -30 "$BATS_TEST_TMPDIR/daemon.log" >&3
	return 1
}

mirror_of() {
	$DST list-panes -a -F '#{pane_id}|#{@bridge_pane}' 2>/dev/null | awk -F'|' -v r="$1" '$2 == r {print $1}'
}

# other_tiled_pane returns the non-floating local pane other than $1 in its
# window (mirror_up doesn't stash the local left tiled pane's id, only $LT).
other_tiled_pane() {
	$DST list-panes -a -F '#{pane_floating_flag}|#{pane_id}' 2>/dev/null | awk -F'|' -v x="$1" '$1 == 0 && $2 != x {print $2}'
}

# leftmost_tiled_pane returns the tiled pane with the smallest pane_left in
# the mirrored window on the given server ($SRC or $DST).
leftmost_tiled_pane() {
	local srv="$1" winid
	if [[ $srv == "$SRC" ]]; then
		winid=rem
	else
		winid="$($DST display-message -p -t "$LT" '#{window_id}')"
	fi
	$srv list-panes -t "$winid" -F '#{pane_floating_flag}|#{pane_left}|#{pane_id}' |
		awk -F'|' '$1 == 0' | sort -t'|' -k2,2n | head -1 | cut -d'|' -f3
}

# geom prints a pane's inner box, the unit both servers agree on.
geom() {
	$1 display-message -p -t "$2" '#{pane_left},#{pane_top} #{pane_width}x#{pane_height}'
}

# tiled_geoms prints the tiled (non-floating) panes of the mirrored window as
# sorted "left,top WxH" lines: the remote's `rem` window on SRC, the mirror
# window on DST (found from a local tiled pane's #{window_id}).
tiled_geoms() {
	local srv="$1" winid
	if [[ $srv == "$SRC" ]]; then
		winid=rem
	else
		winid="$($DST display-message -p -t "$LT" '#{window_id}')"
	fi
	$srv list-panes -t "$winid" -F '#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}' |
		awk -F'|' '$1 == 0 {print $2","$3" "$4"x"$5}' | sort
}

# field reads one format of the local float.
field() {
	$DST display-message -p -t "$LF" "#{$1}"
}

# pfield reads one format of a given local pane.
pfield() {
	$DST display-message -p -t "$1" "#{$2}"
}

# sgr sends one SGR mouse report, button code $1 at window cell ($2,$3), $4 = M
# (press/motion) or m (release). Coordinates are 0-based window cells; the
# report is 1-based client cells, and the status line sits above the window.
sgr() {
	local top
	top="$($DST display-message -p -t host-sess '#{?#{==:#{status-position},top},#{status},0}')"
	case "$top" in on) top=1 ;; off) top=0 ;; esac
	$OBS send-keys -t obs -l $'\e[<'"$1;$(($2 + 1));$(($3 + 1 + top))$4"
	sleep 0.1
}

# wait_agree waits up to 2s for the remote float and its mirror to report the
# same geometry, which must also differ from $1 (the geometry before the
# drag) unless $2 is passed (a snap-back drag, whose final geometry equals
# the pre-drag one).
wait_agree() {
	local before="$1" allow_same="${2:-}" l r deadline=$((SECONDS + 2))
	while :; do
		l="$(geom "$DST" "$LF")"
		r="$(geom "$SRC" "$RF")"
		if [[ $l == "$r" ]] && { [[ -n $allow_same ]] || [[ $l != "$before" ]]; }; then
			return 0
		fi
		((SECONDS < deadline)) || break
		sleep 0.1
	done
	printf 'no agreement within 2s: before=%s local=%s remote=%s\n--- daemon log ---\n' "$before" "$l" "$r" >&3
	tail -20 "$BATS_TEST_TMPDIR/daemon.log" >&3
	return 1
}

# wait_tiled_agree is wait_agree's counterpart for the whole tiled layout: it
# waits up to 2s for every tiled pane's geometry to agree between the mirror
# and the remote.
wait_tiled_agree() {
	local before="$1" allow_same="${2:-}" l r deadline=$((SECONDS + 2))
	while :; do
		l="$(tiled_geoms "$DST")"
		r="$(tiled_geoms "$SRC")"
		if [[ $l == "$r" ]] && { [[ -n $allow_same ]] || [[ $l != "$before" ]]; }; then
			return 0
		fi
		((SECONDS < deadline)) || break
		sleep 0.1
	done
	printf 'no tiled agreement within 2s: before=%s\nlocal:\n%s\nremote:\n%s\n--- daemon log ---\n' "$before" "$l" "$r" >&3
	tail -20 "$BATS_TEST_TMPDIR/daemon.log" >&3
	return 1
}

@test "a left-border drag on a mirror float resizes the REMOTE float" {
	mirror_up
	local before x y
	before="$(geom "$DST" "$LF")"
	x=$(($(field pane_left) - 1))
	y=$(($(field pane_top) + 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 5)) "$y" M
	sgr 0 $((x - 5)) "$y" m
	wait_agree "$before"
	# The left edge followed the drag and the width grew by the same 5 cells.
	local w0="${before#* }"
	w0="${w0%x*}"
	[[ "$(geom "$SRC" "$RF")" == "$((x - 4)),"*" $((w0 + 5))x"* ]]
}

@test "a top-border drag moves the REMOTE float" {
	mirror_up
	local before x y w
	before="$(geom "$DST" "$LF")"
	w="$(field pane_width)"
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) - 1))
	sgr 0 "$x" "$y" M
	sgr 32 $((x + 4)) $((y + 3)) M
	sgr 0 $((x + 4)) $((y + 3)) m
	wait_agree "$before"
	# A move, not a resize.
	[[ "$(geom "$SRC" "$RF")" == *" ${w}x"* ]]
}

@test "an M-drag (move-pane -M) moves the REMOTE float" {
	mirror_up
	local before x y
	before="$(geom "$DST" "$LF")"
	x=$(($(field pane_left) - 1))
	y=$(($(field pane_top) + 3))
	# Button code 8 is Meta; 32 marks motion.
	sgr 8 "$x" "$y" M
	sgr 40 $((x + 6)) $((y + 2)) M
	sgr 8 $((x + 6)) $((y + 2)) m
	wait_agree "$before"
}

@test "a drag released over another pane, with focus moved off the float, still routes the float" {
	mirror_up
	local before x y
	before="$(geom "$DST" "$LF")"
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) + $(field pane_height)))
	sgr 0 "$x" "$y" M
	sgr 32 "$x" $((y + 3)) M
	# Wander right, off the float and over the tiled pane, and move the local
	# active pane off the float: the drag end's own target is that tiled pane,
	# so only the stashed id can name the float.
	sgr 32 $(($(field pane_left) + $(field pane_width) + 10)) $((y + 3)) M
	$DST select-pane -t "$LT"
	sgr 0 $(($(field pane_left) + $(field pane_width) + 10)) $((y + 3)) m
	wait_agree "$before"
}

@test "an Alt-drag from inside a mirror float moves the REMOTE float" {
	mirror_up
	local before x y
	before="$(geom "$DST" "$LF")"
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) + 3))
	# Button code 8 is Meta; 40 marks motion with Meta held.
	sgr 8 "$x" "$y" M
	sgr 40 $((x + 6)) $((y + 2)) M
	sgr 8 $((x + 6)) $((y + 2)) m
	wait_agree "$before"
}

@test "a flush float dragged past its edge snaps back to the remote" {
	mirror_up -x 40 -y 12 -X 0 -Y 5
	local before x y
	before="$(geom "$DST" "$LF")"
	# Grab the top border far enough in that the drag 8 cells left stays on
	# screen: an SGR report left of column 1 is dropped and would move nothing.
	x=$(($(field pane_left) + 20))
	y=$(($(field pane_top) - 1))
	sgr 8 "$x" "$y" M
	sgr 40 $((x - 8)) "$y" M
	sgr 8 $((x - 8)) "$y" m
	wait_agree "$before" allow_same
}

@test "a borderless remote float lands on the local inner box" {
	mirror_up -B none -x 40 -y 12 -X 10 -Y 5
	local before x y
	before="$(geom "$DST" "$LF")"
	x=$(($(field pane_left) - 1))
	y=$(($(field pane_top) + 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 5)) "$y" M
	sgr 0 $((x - 5)) "$y" m
	wait_agree "$before"
}

# The cases below put a float on a row where resize-pane -y adds a row under
# pane-border-status top (pane_top 1) or bottom (bottom edge one above the
# window's last row). Each asserts the height, not just agreement: the bump
# can land on both servers alike.

# rel prints the local float's "pane_top pane_height".
rel() {
	printf '%s %s' "$(field pane_top)" "$(field pane_height)"
}

@test "a top-border drag onto the top row keeps the float's height" {
	mirror_up
	local before x y h
	before="$(geom "$DST" "$LF")"
	h="$(field pane_height)"
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) - 1))
	sgr 0 "$x" "$y" M
	sgr 32 "$x" 0 M
	sgr 0 "$x" 0 m
	wait_agree "$before"
	[[ "$(rel)" == "1 $h" ]]
}

@test "an Alt-drag past the top edge lands on the top row with its height" {
	mirror_up
	local before x y h
	before="$(geom "$DST" "$LF")"
	h="$(field pane_height)"
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) + 3))
	# Grabbed 4 rows into the float, released on row 0: the float overshoots
	# the top edge and the clamp brings it back to the top row.
	sgr 8 "$x" "$y" M
	sgr 40 "$x" 0 M
	sgr 8 "$x" 0 m
	wait_agree "$before"
	[[ "$(rel)" == "1 $h" ]]
}

@test "a top-left-corner drag up to row 0 grows the float by the rows dragged" {
	mirror_up
	local before x y h
	before="$(geom "$DST" "$LF")"
	h="$(field pane_height)"
	x=$(($(field pane_left) - 1))
	y=$(($(field pane_top) - 1))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 2)) 0 M
	sgr 0 $((x - 2)) 0 m
	wait_agree "$before"
	[[ "$(rel)" == "1 $((h + y))" ]]
}

@test "under pane-border-status bottom, a bottom-border drag to the last row grows the float by the rows dragged" {
	mirror_up
	$SRC set -g pane-border-status bottom
	$DST set -g pane-border-status bottom
	[[ "$($DST display-message -p -t "$LF" '#{pane-border-status}')" == bottom ]]
	# The option change re-lays the tiled panes; let the mirror settle on the
	# remote's geometry before measuring.
	wait_agree "" allow_same
	local before x y h top last
	before="$(geom "$DST" "$LF")"
	h="$(field pane_height)"
	top="$(field pane_top)"
	last=$(($($DST display-message -p -t "$LF" '#{window_height}') - 1))
	x=$(($(field pane_left) + 5))
	y=$((top + h))
	sgr 0 "$x" "$y" M
	sgr 32 "$x" "$last" M
	sgr 0 "$x" "$last" m
	wait_agree "$before"
	[[ "$(rel)" == "$top $((h + last - y))" ]]
}

@test "a borderless remote float on the top row keeps its height when dragged along it" {
	mirror_up -B none -x 40 -y 12 -X 10 -Y 1
	local before x y h
	before="$(geom "$DST" "$LF")"
	h="$(field pane_height)"
	[[ "$(field pane_top)" == 1 ]]
	# An Alt-drag from inside: a press on window row 0 resolves to the tiled
	# pane's border status line, not the float's top border.
	x=$(($(field pane_left) + 5))
	y=$(($(field pane_top) + 3))
	sgr 8 "$x" "$y" M
	sgr 40 $((x + 6)) "$y" M
	sgr 8 $((x + 6)) "$y" m
	wait_agree "$before"
	[[ "$(rel)" == "1 $h" ]]
}

@test "a remote resize of a float on the top row reaches the mirror exactly" {
	mirror_up -x 40 -y 12 -X 10 -Y 0
	local before
	before="$(geom "$DST" "$LF")"
	$SRC resize-pane -t "$RF" -y 9
	wait_agree "$before"
}

# A mouse drag on the divider between two TILED mirror panes reaches the
# REMOTE panes (#823): dragging it locally resized only the local panes while
# the remote kept its old sizes. Routed through ctl `drag` -> the daemon's
# `tile-layout` verb, guarded by a pane-order/size/zoom match so a stale or
# reordered request is refused and the mirror reconciles back to the remote.

@test "a divider drag between two tiled mirror panes resizes the REMOTE panes" {
	mirror_tiled_up
	local before rw0 x y
	before="$(tiled_geoms "$DST")"
	rw0="$($SRC display-message -p -t "$RL" '#{pane_width}')"
	x=$(($(pfield "$LL" pane_left) + $(pfield "$LL" pane_width)))
	y=$(($(pfield "$LL" pane_top) + 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 10)) "$y" M
	sgr 0 $((x - 10)) "$y" m
	wait_tiled_agree "$before"
	[[ "$($SRC display-message -p -t "$RL" '#{pane_width}')" == "$((rw0 - 10))" ]]
}

@test "a tiled divider drag with a mirror float open leaves the float alone" {
	mirror_up
	local ll before_t before_f x y
	ll="$(other_tiled_pane "$LT")"
	before_t="$(tiled_geoms "$DST")"
	before_f="$(geom "$DST" "$LF")"
	x=$(($(pfield "$ll" pane_left) + $(pfield "$ll" pane_width)))
	# Below the default float's row range (Y 5, height 12), so the press lands
	# on the divider, not the float.
	y=$(($(pfield "$ll" pane_top) + $(pfield "$ll" pane_height) - 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 10)) "$y" M
	sgr 0 $((x - 10)) "$y" m
	wait_tiled_agree "$before_t"
	[[ "$(geom "$DST" "$LF")" == "$before_f" ]]
	[[ "$(geom "$SRC" "$RF")" == "$before_f" ]]
}

@test "a divider drag in a 3-pane nested layout reaches the remote" {
	# shellcheck disable=SC2016 # eval'd inside mirror_tiled_up, where $SRC/$rt are in scope
	mirror_tiled_up '$SRC split-window -d -v -t "$rt" "sleep 300"'
	local before x y
	before="$(tiled_geoms "$DST")"
	x=$(($(pfield "$LT" pane_left) + 5))
	y=$(($(pfield "$LT" pane_top) + $(pfield "$LT" pane_height)))
	sgr 0 "$x" "$y" M
	sgr 32 "$x" $((y + 3)) M
	sgr 0 "$x" $((y + 3)) m
	wait_tiled_agree "$before"
}

@test "a divider drag on a rotated remote reaches the remote" {
	# shellcheck disable=SC2016 # eval'd inside mirror_tiled_up, where $SRC/$rt are in scope
	mirror_tiled_up \
		'$SRC split-window -d -h -t "$rt" "sleep 300"' \
		'$SRC select-layout -t rem even-horizontal' \
		'$SRC rotate-window -t rem'
	local before ll x y
	before="$(tiled_geoms "$DST")"
	ll="$(leftmost_tiled_pane "$DST")"
	x=$(($(pfield "$ll" pane_left) + $(pfield "$ll" pane_width)))
	y=$(($(pfield "$ll" pane_top) + 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 5)) "$y" M
	sgr 0 $((x - 5)) "$y" m
	wait_tiled_agree "$before"
}

@test "a divider drag in a local non-mirror window still resizes it locally" {
	$DST new-session -d -s host-sess -x 100 -y 30
	$DST set-option -g @splash_shown 1
	$DST split-window -d -h -t host-sess "sleep 300"
	$OBS new-session -d -s obs -x 100 -y 32 "env -u TMUX TERM=xterm-256color $TMUX_BIN -L fddst attach -t host-sess"
	local deadline=$((SECONDS + BUDGET_SECS))
	while ((SECONDS < deadline)); do
		[[ "$($DST list-clients -t host-sess 2>/dev/null | wc -l)" -ge 1 ]] && break
		sleep 0.1
	done
	local ll x y w0
	ll="$($DST list-panes -t host-sess -F '#{pane_left}|#{pane_id}' | awk -F'|' '$1 == 0 {print $2}')"
	w0="$(pfield "$ll" pane_width)"
	x=$(($(pfield "$ll" pane_left) + w0))
	y=$(($(pfield "$ll" pane_top) + 3))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 10)) "$y" M
	sgr 0 $((x - 10)) "$y" m
	local wdeadline=$((SECONDS + 2))
	while ((SECONDS < wdeadline)); do
		[[ "$(pfield "$ll" pane_width)" == "$((w0 - 10))" ]] && break
		sleep 0.1
	done
	[[ "$(pfield "$ll" pane_width)" == "$((w0 - 10))" ]]
}

@test "a tile-layout whose pane order does not match the remote is refused, and the mirror snaps back" {
	mirror_tiled_up
	local remote_before skewed_local layout skewed
	remote_before="$(tiled_geoms "$SRC")"
	$DST resize-pane -t "$LL" -L 10
	skewed_local="$(tiled_geoms "$DST")"
	layout="$($DST display-message -p -t "$LL" '#{window_layout}')"
	# Swap in the remote ids REVERSED (local left -> remote right, local right
	# -> remote left) so the daemon's pane-order guard refuses the request.
	# Placeholders first: a local id and a remote id can collide numerically
	# and corrupt a direct substitution.
	skewed="$layout"
	skewed="${skewed//$LL/@@A@@}"
	skewed="${skewed//$LT/@@B@@}"
	skewed="${skewed//@@A@@/$RT}"
	skewed="${skewed//@@B@@/$RL}"
	run "$CTL" --sock "$SOCK" tile-layout "$RL" "$skewed"
	wait_tiled_agree "$skewed_local"
	[[ "$(tiled_geoms "$SRC")" == "$remote_before" ]]
}
