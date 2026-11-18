#!/usr/bin/env bats
# shellcheck disable=SC2016 # the #{...} formats are literal tmux formats
# Live proof for #1189: tmux-smart-nav against real floating panes on a
# config-less scratch server. A float covering the view keeps focus; a smaller
# float exits to the tile visible beside it. Needs a tmux with floating panes
# (mkTmux in flake.nix's smart-nav-live-tests check).

setup() {
	OG_TMUX_DIR="$(mktemp -d /tmp/og-snl.XXXXXX)"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	export XDG_DATA_HOME="$OG_TMUX_DIR/data"
	export CLAUDE_STATUS_DIR="$OG_TMUX_DIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR" "$XDG_DATA_HOME"
	unset TMUX KITTY_LISTEN_ON
	OG_TEST_ISOLATED=1
	command -v tmux >/dev/null || skip "tmux not on PATH"

	local root
	root="$(dirname "$BATS_TEST_DIRNAME")/scripts"
	NAV="$OG_TMUX_DIR/tmux-smart-nav.sh"
	sed "s|@lib_float_cover@|$root/lib-float-cover.sh|" "$root/tmux-smart-nav.sh" >"$NAV"

	# The script's bare `tmux` must reach the scratch server.
	mkdir -p "$OG_TMUX_DIR/bin"
	printf '#!%s\nexec "%s" -L probe "$@"\n' "$(command -v bash)" "$(command -v tmux)" >"$OG_TMUX_DIR/bin/tmux"
	chmod +x "$OG_TMUX_DIR/bin/tmux"
	REAL_TMUX="$(command -v tmux)"

	t -f /dev/null new-session -d -s S -x 100 -y 40
	WIN="$(t display-message -p -t S '#{window_id}')"
	LEFT="$(t display-message -p -t S '#{pane_id}')"
	RIGHT="$(t split-window -h -d -P -F '#{pane_id}' -t "$LEFT")"
}

teardown() {
	# bats runs teardown even after a skip in setup: only kill a server setup isolated.
	[[ -n ${OG_TEST_ISOLATED:-} ]] || return 0
	t kill-server 2>/dev/null || true
	[ -n "${OG_TMUX_DIR:-}" ] && rm -rf "$OG_TMUX_DIR"
	return 0
}

t() { timeout --foreground 30s "$REAL_TMUX" -L probe "$@"; }

# make_float WIDTH HEIGHT X Y: a float in $FLOAT, focused.
make_float() {
	if ! FLOAT="$(t new-pane -P -F '#{pane_id}' -t "$WIN" -x "$1" -y "$2" -X "$3" -Y "$4" -B heavy 'sleep 600' 2>/dev/null)"; then
		skip "this tmux cannot create a floating pane"
	fi
	t select-pane -t "$FLOAT"
}

# press DIR(L|R|U|D): one smart-nav press from the focused float, no kitty.
press() {
	local dir
	case "$1" in L) dir=left ;; R) dir=right ;; U) dir=up ;; D) dir=down ;; esac
	PATH="$OG_TMUX_DIR/bin:$PATH" bash "$NAV" "$1" "$dir" 0 0 1 "$FLOAT" "$WIN"
}

active() { t display-message -p -t "$WIN" '#{pane_id}'; }

@test "a float covering the view keeps focus in every direction" {
	make_float 100% 100% 0% 0%
	[ "$(active)" = "$FLOAT" ]
	for d in L R U D; do
		press "$d"
		[ "$(active)" = "$FLOAT" ]
	done
}

@test "a smaller float exits to the tile visible beside it" {
	make_float 50% 50% 25% 25%
	[ "$(active)" = "$FLOAT" ]
	press L
	[ "$(active)" = "$LEFT" ]
	t select-pane -t "$FLOAT"
	press R
	[ "$(active)" = "$RIGHT" ]
}
