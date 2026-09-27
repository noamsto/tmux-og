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

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TMUX_RAW="${TMUX_RAW:?set TMUX_RAW to the raw pinned binary}"
	DAEMON="${DAEMON:?set DAEMON to the built og-remote-bridge-daemon}"
	RENDERER="${RENDERER:?set RENDERER to the built og-remote-bridge-renderer}"

	# A short fixed dir: tmux -L resolves under $TMUX_TMPDIR and the unix
	# socket path limit is 108 chars.
	export TMUX_TMPDIR="/tmp/og-fd-bats-$$"
	rm -rf "$TMUX_TMPDIR"
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
	rm -rf "$TMUX_TMPDIR"
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

mirror_of() {
	$DST list-panes -a -F '#{pane_id}|#{@bridge_pane}' 2>/dev/null | awk -F'|' -v r="$1" '$2 == r {print $1}'
}

# geom prints a pane's inner box, the unit both servers agree on.
geom() {
	$1 display-message -p -t "$2" '#{pane_left},#{pane_top} #{pane_width}x#{pane_height}'
}

# field reads one format of the local float.
field() {
	$DST display-message -p -t "$LF" "#{$1}"
}

# sgr sends one SGR mouse report, button code $1 at window cell ($2,$3), $4 = M
# (press/motion) or m (release). Coordinates are 0-based window cells; the
# report is 1-based client cells, and the status line sits above the window.
sgr() {
	local top
	top="$($DST display-message -p -t "$LF" '#{?#{==:#{status-position},top},#{status},0}')"
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
