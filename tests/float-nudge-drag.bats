#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
bats_require_minimum_version 1.5.0
# A mouse drag on a LOCAL (non-mirror) float still restamps @float_geom (#864).
# tmux's float drag fires no hook while it runs — the only event is the
# synthesized MouseDragEnd1<loc> key — so the conf enters a one-shot
# og-float-drag table at drag start and the drag end hands the stashed pane id
# to tmux-float-nudge. Without it the float's size is reverted by the next
# window resize.
#
# Reuses float-drag-integration.bats' shape: a real attached client fed SGR
# mouse reports from a pty host, the real emitted conf on the wrapper server.
# No daemon, no remote server.

setup() {
	TMUX_BIN="${TMUX_BIN:?set TMUX_BIN to the built wrapper}"
	TMUX_RAW="${TMUX_RAW:?set TMUX_RAW to the raw pinned binary}"

	OG_TMUX_DIR="/tmp/og-fnd-$$"
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
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/og-pr"
	export OG_AGENT_USAGE_DIR="$BATS_TEST_TMPDIR/og-agent-usage"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/og-enrich-lock"
	PATH="$(dirname "$TMUX_BIN"):$PATH"

	DST="$TMUX_BIN -L fnddst"
	OBS="$TMUX_RAW -L fndobs -f /dev/null"
}

teardown() {
	$DST kill-server 2>/dev/null || true
	$OBS kill-server 2>/dev/null || true
	rm -rf "${OG_TMUX_DIR:-}"
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

geom() { $DST display-message -p -t "$FLOAT" '#{pane_width}x#{pane_height}+#{pane_left}+#{pane_top}'; }
stamp() { $DST display-message -p -t "$FLOAT" '#{@float_geom}'; }

@test "a local float border drag rewrites @float_geom and the size then sticks" {
	$DST new-session -d -s host-sess -x 100 -y 30
	$DST set-option -g @splash_shown 1
	$DST new-pane -t host-sess -x 50% -y 50% -X 10% -Y 10% -B heavy
	FLOAT="$($DST list-panes -t host-sess -f '#{pane_floating_flag}' -F '#{pane_id}' | tail -1)"
	$DST set-option -p -t "$FLOAT" @float_geom '50% 50% 10% 10%'

	$OBS new-session -d -s obs -x 100 -y 32 "env -u TMUX TERM=xterm-256color $TMUX_BIN -L fnddst attach -t host-sess"
	local deadline=$((SECONDS + 10))
	while ((SECONDS < deadline)); do
		[[ "$($DST list-clients -t host-sess 2>/dev/null | wc -l)" -ge 1 ]] && break
		sleep 0.1
	done

	local before w0
	before="$(geom)"
	w0="$($DST display-message -p -t "$FLOAT" '#{pane_width}')"

	# Drag the right border 10 cells left: press on the border column, one move,
	# release. The release is the drag end the og-float-drag table catches.
	local x y
	x=$(($($DST display-message -p -t "$FLOAT" '#{pane_left}') + w0))
	y=$(($($DST display-message -p -t "$FLOAT" '#{pane_top}') + 2))
	sgr 0 "$x" "$y" M
	sgr 32 $((x - 10)) "$y" M
	sgr 0 $((x - 10)) "$y" m

	local wdeadline=$((SECONDS + 5))
	local after
	while ((SECONDS < wdeadline)); do
		after="$(geom)"
		[[ $after != "$before" ]] && break
		sleep 0.1
	done
	[[ $after != "$before" ]]
	# The drag end handed the stashed pane to tmux-float-nudge: the stamp was
	# rewritten from the dragged geometry (a percentage field stays a percentage)
	# and the float marked current for the window.
	[[ "$(stamp)" == *%* ]]
	[[ "$($DST display-message -p -t "$FLOAT" '#{@float_refit_size}')" == "$($DST display-message -p -t host-sess '#{window_width}x#{window_height}')" ]]
}
