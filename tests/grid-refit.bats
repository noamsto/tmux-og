#!/usr/bin/env bats
# Tests scripts/tmux-grid-refit.sh (#749): the responsive dispatcher grid
# layout. The script reads only the dispatcher-published hints (window
# @crew_grid / @crew_grid_main_pct, pane @crew_role), keeps the lead pane as the
# main pane, and chooses main-vertical vs main-horizontal from the window's
# size — no-op on a non-grid or zoomed window. Runs the real script against a
# private, config-less tmux server (like tests/float-refit.bats), so its bare
# `tmux` calls hit real windows and panes, not fakes.
#
# The hook-coexistence case needs the pinned mkTmux (the grid-refit-tests
# check): it creates a real floating pane, which nixpkgs' stock tmux advertises
# via `list-commands new-pane` and then rejects at parse time. The #760
# layout-change regression likewise only reproduces on the pinned next-3.9.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	REPO_ROOT="$(cd "$(dirname "$BATS_TEST_FILENAME")/.." && pwd)"
	GRID="$REPO_ROOT/scripts/tmux-grid-refit.sh"
	FLOAT_SCRIPT="$REPO_ROOT/scripts/tmux-float-refit.sh"

	export TMUX_TMPDIR="/tmp/og-gr-$$-${BATS_TEST_NUMBER}"
	rm -rf "$TMUX_TMPDIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	# A private TMUX_TMPDIR does not isolate CLAUDE_STATUS_DIR (CLAUDE.md: it
	# defaults to a bare /tmp path shared by every tmux server on the machine)
	# — isolate it anyway rather than assume this script never grows a read.
	export CLAUDE_STATUS_DIR="$TMUX_TMPDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"

	# -f /dev/null: a real config would arm the window-resized hooks that run
	# these very scripts, invalidating the before/after comparisons below.
	# Four panes: one lead plus three roles, the shape a live grid has.
	tmux -f /dev/null new-session -d -s S -c "$TMUX_TMPDIR" -x 200 -y 50
	WIN="$(tmux display-message -p -t S '#{window_id}')"
	tmux split-window -t "$WIN"
	tmux split-window -t "$WIN"
	tmux split-window -t "$WIN"
	LEAD=""
	ROLE_PANES=()
}

teardown() {
	tmux kill-server 2>/dev/null || true
	rm -rf "$TMUX_TMPDIR"
}

# make_grid <lead_position>: tag the pane at the 1-based position
# <lead_position> as the lead and the rest as roles, and set @crew_grid=1.
# Leaves the lead pane id in LEAD and the rest in ROLE_PANES.
make_grid() {
	local lead_pos=$1 id i
	mapfile -t _ids < <(tmux list-panes -t "$WIN" -F '#{pane_id}')
	LEAD="${_ids[$((lead_pos - 1))]}"
	ROLE_PANES=()
	for i in "${!_ids[@]}"; do
		id="${_ids[$i]}"
		if [[ $id == "$LEAD" ]]; then
			tmux set-option -p -t "$id" @crew_role lead
		else
			tmux set-option -p -t "$id" @crew_role reviewer
			ROLE_PANES+=("$id")
		fi
	done
	tmux set-option -w -t "$WIN" @crew_grid 1
}

first_pane() { tmux list-panes -t "$WIN" -F '#{pane_id}' | head -1; }

# arm_grid_hooks — source the *production* grid-refit hook lines out of the
# rendered conf. That is what makes the #760 regression exercise the real
# `window-layout-changed` wiring rather than a hand-rolled copy. It only
# requires TMUX_OG_CONF to be set: on a pre-fix conf (no layout-change setter)
# it still sources the window-resized[10] line, and the caller's behavioral
# assertion then fails — that red is the regression's whole point.
arm_grid_hooks() {
	[ -n "${TMUX_OG_CONF:-}" ] || return 1
	[ -f "$TMUX_OG_CONF" ] || return 1
	HOOKS_FILE="$BATS_TEST_TMPDIR/grid-hooks.conf"
	grep -E '^[[:space:]]*set-hook .*tmux-grid-refit' "$TMUX_OG_CONF" >"$HOOKS_FILE"
	tmux source-file "$HOOKS_FILE"
}

# #760 reproduces only on the pinned next-3.9: upstream <=3.7c already collapses
# the freed cells back to the pre-split sibling, so on such a tmux the behavior
# assertions below hold with no fix at all. Skip rather than report a green that
# proves nothing (gate on the live server's version, never `tmux -V`, per CLAUDE.md).
layout_bug_reproduces() {
	case "$(tmux display-message -p '#{version}')" in
	next-3.9*) return 0 ;;
	*) return 1 ;;
	esac
}

@test "wide window: main-vertical at the configured share, lead promoted to first" {
	# Lead is the LAST pane, so the swap path is exercised, not just the order.
	make_grid 4

	run bash "$GRID" "$WIN"
	[ "$status" -eq 0 ]

	[ "$(first_pane)" = "$LEAD" ]
	[ "$(tmux show-options -w -v -t "$WIN" main-pane-width)" = "60%" ]

	local lead_w role_w
	lead_w="$(tmux display-message -p -t "$LEAD" '#{pane_width}')"
	# ~60% of 200; slack for the layout's border accounting.
	[ "$lead_w" -ge 115 ]
	for role in "${ROLE_PANES[@]}"; do
		role_w="$(tmux display-message -p -t "$role" '#{pane_width}')"
		[ "$lead_w" -gt "$role_w" ]
	done
}

@test "narrow/tall window: main-horizontal at the configured share" {
	tmux resize-window -t "$WIN" -x 60 -y 40
	make_grid 3

	run bash "$GRID" "$WIN"
	[ "$status" -eq 0 ]

	[ "$(first_pane)" = "$LEAD" ]
	[ "$(tmux show-options -w -v -t "$WIN" main-pane-height)" = "60%" ]

	local lead_top role_top
	lead_top="$(tmux display-message -p -t "$LEAD" '#{pane_top}')"
	# The lead spans the window width and the roles sit in a row below it.
	[ "$(tmux display-message -p -t "$LEAD" '#{pane_width}')" -eq 60 ]
	for role in "${ROLE_PANES[@]}"; do
		role_top="$(tmux display-message -p -t "$role" '#{pane_top}')"
		[ "$role_top" -gt "$lead_top" ]
	done
}

@test "window without @crew_grid is untouched" {
	make_grid 3
	tmux set-option -w -u -t "$WIN" @crew_grid

	local before after
	before="$(tmux display-message -p -t "$WIN" '#{window_layout}')"

	run bash "$GRID" "$WIN"
	[ "$status" -eq 0 ]

	after="$(tmux display-message -p -t "$WIN" '#{window_layout}')"
	[ "$before" = "$after" ]
	# No signature write either: the script returned before claiming.
	run tmux show-options -w -v -t "$WIN" @grid_refit_sig
	[ "$status" -ne 0 ]
}

@test "zoomed grid window is untouched" {
	make_grid 3
	tmux resize-pane -t "$WIN" -Z
	[ "$(tmux display-message -p -t "$WIN" '#{window_zoomed_flag}')" = 1 ]

	local before after
	before="$(tmux display-message -p -t "$WIN" '#{window_layout}')"

	run bash "$GRID" "$WIN"
	[ "$status" -eq 0 ]

	after="$(tmux display-message -p -t "$WIN" '#{window_layout}')"
	[ "$before" = "$after" ]
	run tmux show-options -w -v -t "$WIN" @grid_refit_sig
	[ "$status" -ne 0 ]

	tmux resize-pane -t "$WIN" -Z
}

@test "idempotent: repeated calls converge and never re-notify" {
	make_grid 3

	bash "$GRID" "$WIN"
	local one sig
	one="$(tmux display-message -p -t "$WIN" '#{window_layout}')"
	sig="$(tmux show-options -w -v -t "$WIN" @grid_refit_sig)"
	[ -n "$sig" ]

	local i
	for i in 1 2 3; do
		bash "$GRID" "$WIN"
		[ "$(tmux display-message -p -t "$WIN" '#{window_layout}')" = "$one" ]
	done
	[ "$(tmux show-options -w -v -t "$WIN" @grid_refit_sig)" = "$sig" ]
}

@test "concurrent refits cannot demote the lead" {
	make_grid 4

	local last i p1 p2
	for i in 1 2 3 4 5; do
		# Re-arm: move the lead away from first and clear the signature so
		# both invocations decide to lay the window out.
		last="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | tail -1)"
		[ "$last" = "$LEAD" ] || tmux swap-pane -d -s "$LEAD" -t "$last"
		tmux set-option -w -u -t "$WIN" @grid_refit_sig

		bash "$GRID" "$WIN" &
		p1=$!
		bash "$GRID" "$WIN" &
		p2=$!
		wait "$p1" "$p2"

		# Assert before the next re-arm and before any healing run: a final
		# serial call would re-apply a demoted lead (the signature carries
		# $lead and the skip needs lead==first), masking a broken lock.
		[ "$(first_pane)" = "$LEAD" ]
	done

	# The mkdir lock serializes the non-idempotent swap, so no racing pair
	# leaves the lead demoted to a role column (#749's exact symptom).
	bash "$GRID" "$WIN"
	[ "$(tmux show-options -w -v -t "$WIN" main-pane-width)" = "60%" ]
}

@test "no target argument: no-op, exits 0" {
	run bash "$GRID"
	[ "$status" -eq 0 ]

	run bash "$GRID" ""
	[ "$status" -eq 0 ]
}

@test "resize fires both indexed window-resized hooks" {
	# Grow the window: upstream's float clamp only ever shrinks, so a float
	# that grows is proof tmux-float-refit's hook actually ran.
	tmux resize-window -t "$WIN" -x 100 -y 30
	make_grid 3

	if ! tmux new-pane -t "$WIN" -x 90% -y 90% -X 5% -Y 5% -B heavy 2>/dev/null; then
		skip "this tmux cannot create a floating pane"
	fi
	local floating
	floating="$(tmux list-panes -t "$WIN" -F '#{pane_floating_flag}' | tr -d '\n')"
	case "$floating" in
	*1*) ;;
	*) skip "this tmux does not report pane_floating_flag" ;;
	esac
	FLOAT="$(tmux list-panes -t "$WIN" -f '#{pane_floating_flag}' -F '#{pane_id}')"
	tmux set-option -p -t "$FLOAT" @float_geom '90% 90% 5% 5%'

	# Index 0 = float refit, index 10 = grid refit: neither clobbers the other.
	tmux set-hook -g window-resized "run-shell -b 'bash $FLOAT_SCRIPT #{q:window_id}'"
	tmux set-hook -g window-resized[10] "run-shell -b 'bash $GRID #{q:window_id}'"

	tmux resize-window -t "$WIN" -x 200 -y 50

	local gw fw tries
	gw=""
	fw=0
	tries=0
	while ((tries < 60)); do
		gw="$(tmux show-options -w -v -t "$WIN" main-pane-width 2>/dev/null || true)"
		fw="$(tmux display-message -p -t "$FLOAT" '#{pane_width}' 2>/dev/null || true)"
		[[ $gw == "60%" && ${fw:-0} -ge 170 ]] && break
		sleep 0.1
		((tries++)) || true
	done
	# grid-refit fired (layout applied) and float-refit fired (float grew to
	# its 90% creation width against the larger window).
	[ "$gw" = "60%" ]
	[ "${fw:-0}" -ge 170 ]
}

@test "carousel split then kill re-normalises the grid via the production hook (#760)" {
	arm_grid_hooks || skip "TMUX_OG_CONF unset (run via the grid-refit-tests derivation)"
	layout_bug_reproduces || skip "#760 reproduces only on the pinned next-3.9 tmux this server is not"

	# make_grid 1 puts the lead first, so the split below is the aeye shape:
	# a carousel-like pane split off the lead, then killed. No resize fires.
	make_grid 1
	bash "$GRID" "$WIN"

	local before carousel lead_now tries
	before="$(tmux display-message -p -t "$LEAD" '#{pane_width}')"
	[ "$before" -ge 115 ]

	tmux split-window -h -t "$LEAD"
	carousel="$(tmux list-panes -t "$WIN" -F '#{pane_id}' | tail -1)"

	tries=0
	lead_now=0
	while ((tries < 30)); do
		lead_now="$(tmux display-message -p -t "$LEAD" '#{pane_width}' 2>/dev/null || echo 0)"
		[ "$lead_now" -ge 115 ] && break
		sleep 0.1
		((tries++)) || true
	done
	# While the carousel is open the lead keeps its share (the carousel joins
	# the role column) — not the ~59 the raw redistribution left.
	[ "$lead_now" -ge 115 ]

	tmux kill-pane -t "$carousel"

	tries=0
	lead_now=0
	while ((tries < 30)); do
		lead_now="$(tmux display-message -p -t "$LEAD" '#{pane_width}' 2>/dev/null || echo 0)"
		[ "$lead_now" = "$before" ] && break
		sleep 0.1
		((tries++)) || true
	done
	# Closing the carousel restores exactly the grid the window had before.
	[ "$lead_now" = "$before" ]
}

@test "a floating pane does not churn the grid layout (#760 float filter)" {
	arm_grid_hooks || skip "TMUX_OG_CONF unset (run via the grid-refit-tests derivation)"
	grep -q 'window-layout-changed' "$HOOKS_FILE" || skip "rendered conf carries no window-layout-changed grid hook"

	tmux resize-window -t "$WIN" -x 200 -y 50
	make_grid 3
	bash "$GRID" "$WIN"

	local sig lead_before float
	sig="$(tmux show-options -w -v -t "$WIN" @grid_refit_sig)"
	[ -n "$sig" ]
	lead_before="$(tmux display-message -p -t "$LEAD" '#{pane_width}')"

	if ! tmux new-pane -t "$WIN" -x 90% -y 90% -X 5% -Y 5% -B heavy 2>/dev/null; then
		skip "this tmux cannot create a floating pane"
	fi
	float="$(tmux list-panes -t "$WIN" -f '#{pane_floating_flag}' -F '#{pane_id}')"
	[ -n "$float" ]
	sleep 0.5
	# window-layout-changed fires on the float, but the float is excluded from
	# the tiled set, so the signature and the lead width are untouched.
	[ "$(tmux show-options -w -v -t "$WIN" @grid_refit_sig)" = "$sig" ]
	[ "$(tmux display-message -p -t "$LEAD" '#{pane_width}')" = "$lead_before" ]

	tmux kill-pane -t "$float"
	sleep 0.5
	[ "$(tmux show-options -w -v -t "$WIN" @grid_refit_sig)" = "$sig" ]
	[ "$(tmux display-message -p -t "$LEAD" '#{pane_width}')" = "$lead_before" ]
}
