#!/usr/bin/env bats
# Tests the cwd-move re-stamp (#596) in tmux-update-icons.sh: a window whose
# first non-floating pane cd's into a different worktree fires the reconciler
# once and memoizes the move via @window_cwd_seen, so a settled window (or one
# still inside the same worktree) never forks anything on a later tick.
#
# Runs the real script against a private, config-less tmux server (like
# update-icons-enrich-trigger.bats) so bare `tmux`/`git` calls inside it see
# real windows/panes, not fakes.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	export TMPDIR="$TDIR"

	# Fake reflow: update-icons kicks it on every relevant change; a no-op is
	# fine here, this file only asserts on the reconcile trigger.
	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	# Fake reconciler: records its argv so we can assert whether/how it fired.
	RECONCILE_LOG="$TDIR/reconcile.log"
	FAKE_RECONCILE="$TDIR/fake-reconcile"
	cat >"$FAKE_RECONCILE" <<-EOF
		#!/bin/sh
		echo "\$*" >>"$RECONCILE_LOG"
	EOF
	chmod +x "$FAKE_RECONCILE"
	export RECONCILE_BIN="$FAKE_RECONCILE"

	# Runnable update-icons with Nix placeholders resolved; @reconcile@ (and
	# @issue_stamp@) are left raw (unsubstituted) on purpose — RECONCILE_BIN
	# above wins per the ${RECONCILE_BIN:-@reconcile@} pattern (same as
	# AGENT_DETECT_BIN / ISSUE_STAMP_BIN in the enrich-trigger test).
	UPDATE_ICONS="$TDIR/update-icons.sh"
	licons="$TDIR/lib-icons.sh"
	sed -e 's/@ICON_MAP@//' -e 's/@FALLBACK_ICON@//' scripts/lib-icons.sh >"$licons"
	sed \
		-e "s|@lib_icons@|$licons|g" \
		-e "s|@lib_claude@|$PWD/scripts/lib-claude.sh|g" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|g" \
		-e "s|@reflow@|$FAKE_REFLOW|g" \
		-e 's|@MAX_ICONS@|5|g' \
		scripts/tmux-update-icons.sh >"$UPDATE_ICONS"

	REPO_A="$TDIR/repo-a"
	REPO_B="$TDIR/repo-b"
	mkrepo "$REPO_A"
	mkrepo "$REPO_B"
	NONGIT="$TDIR/nongit"
	mkdir -p "$NONGIT"

	tmux -f /dev/null new-session -d -s S -c "$REPO_A" -x 200 -y 50
	tmux set -g base-index 0
	# Address the window by id, never "S:0": base-index is only pinned after the
	# session exists, and a tmux whose config sets base-index 1 (this repo's own
	# wrapper, which a local `bats` run picks up off PATH) numbers that first
	# window 1. WIN is stable either way.
	WIN=$(tmux display-message -p -t S '#{window_id}')
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

mkrepo() {
	mkdir -p "$1"
	git -C "$1" init -q
	git -C "$1" config user.email t@t
	git -C "$1" config user.name t
	git -C "$1" config commit.gpgsign false
	git -C "$1" commit -q --allow-empty -m init
	git -C "$1" branch -q -M main
}

# Canonicalized path, the same way tmux reports pane_current_path (and the way
# tmux-worktree-match derives w_phys) — a plain string compare against a
# possibly-symlinked tmp dir (e.g. macOS /tmp -> /private/tmp) would flake.
physical() {
	(cd -P -- "$1" 2>/dev/null && pwd -P)
}

run_update_icons() {
	bash "$UPDATE_ICONS" S >/dev/null 2>&1 || true
}

wait_for() {
	for _ in $(seq 1 40); do
		[[ -s $1 ]] && return 0
		sleep 0.05
	done
	return 1
}

@test "moves repo: fires once, memoizes the new cwd" {
	local top_a top_b pane_id
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	# Pre-stamp the settled state, as if the window's creation-time reconcile
	# already ran for repo A.
	tmux set -w -t "$WIN" @worktree "$top_a"
	tmux set -w -t "$WIN" @window_cwd_seen "$top_a"
	run_update_icons
	[ ! -s "$RECONCILE_LOG" ]

	pane_id="$(tmux display -t "$WIN" -p '#{pane_id}')"
	tmux respawn-pane -k -t "$WIN" -c "$REPO_B"
	run_update_icons
	wait_for "$RECONCILE_LOG"

	top_b="$(git -C "$REPO_B" rev-parse --show-toplevel)"
	[ "$(cat "$RECONCILE_LOG")" = "$pane_id --cwd-move" ]
	[ "$(tmux show -wv -t "$WIN" @window_cwd_seen)" = "$top_b" ]

	# Settles: no re-fire on a later tick over the same (now-seen) cwd.
	run_update_icons
	sleep 0.2
	[ "$(wc -l <"$RECONCILE_LOG")" -eq 1 ]
}

@test "does not move: several ticks fire zero times, cwd_seen stays unset" {
	local top_a
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	tmux set -w -t "$WIN" @worktree "$top_a"

	run_update_icons
	run_update_icons
	run_update_icons
	sleep 0.2

	[ ! -s "$RECONCILE_LOG" ]
	[ -z "$(tmux show -wv -t "$WIN" @window_cwd_seen 2>/dev/null)" ]
}

@test "already stale: fires once on the first tick, then settles" {
	local top_a pane_id
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	# @worktree names an unrelated directory — as if it outlived a cd, never
	# reconciled since.
	tmux set -w -t "$WIN" @worktree "$TDIR/unrelated"

	pane_id="$(tmux display -t "$WIN" -p '#{pane_id}')"
	run_update_icons
	wait_for "$RECONCILE_LOG"
	[ "$(cat "$RECONCILE_LOG")" = "$pane_id --cwd-move" ]
	[ "$(tmux show -wv -t "$WIN" @window_cwd_seen)" = "$top_a" ]

	run_update_icons
	sleep 0.2
	[ "$(wc -l <"$RECONCILE_LOG")" -eq 1 ]
}

@test "subdirectory cd stays under the worktree: fires zero times" {
	local top_a sub
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	sub="$REPO_A/sub"
	mkdir -p "$sub"
	tmux set -w -t "$WIN" @worktree "$top_a"
	tmux respawn-pane -k -t "$WIN" -c "$sub"

	run_update_icons
	run_update_icons
	sleep 0.2

	[ ! -s "$RECONCILE_LOG" ]
}

@test "bridge mirror window: fires zero times even outside its worktree" {
	tmux set -w -t "$WIN" @bridge_win 1
	tmux set -w -t "$WIN" @worktree "$TDIR/unrelated"

	run_update_icons
	run_update_icons
	sleep 0.2

	[ ! -s "$RECONCILE_LOG" ]
}

@test "non-git cwd: fires once, then settles rather than forking every tick" {
	local pane_id
	pane_id="$(tmux display -t "$WIN" -p '#{pane_id}')"
	tmux respawn-pane -k -t "$WIN" -c "$NONGIT"

	run_update_icons
	wait_for "$RECONCILE_LOG"
	[ "$(cat "$RECONCILE_LOG")" = "$pane_id --cwd-move" ]
	[ "$(tmux show -wv -t "$WIN" @window_cwd_seen)" = "$(physical "$NONGIT")" ]

	run_update_icons
	sleep 0.2
	[ "$(wc -l <"$RECONCILE_LOG")" -eq 1 ]
}

@test ".worktrees boundary: a nested checkout is not inside its parent" {
	local top_a wtdir
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	wtdir="$REPO_A/.worktrees/x"
	mkdir -p "$wtdir"
	tmux set -w -t "$WIN" @worktree "$top_a"
	tmux respawn-pane -k -t "$WIN" -c "$wtdir"

	run_update_icons
	wait_for "$RECONCILE_LOG"

	[ -s "$RECONCILE_LOG" ]
}

@test "floating pane in another repo is not the window's cwd" {
	if ! tmux new-pane -t "$WIN" -x 30 -y 10 -X 10 -Y 10 -c "$REPO_B" 2>/dev/null; then
		skip "this tmux cannot create a floating pane"
	fi
	local floating
	floating="$(tmux list-panes -t "$WIN" -F '#{pane_floating_flag}' | tr -d '\n')"
	case "$floating" in
	*1*) ;;
	*)
		skip "this tmux does not report pane_floating_flag"
		;;
	esac

	local top_a
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	tmux set -w -t "$WIN" @worktree "$top_a"

	run_update_icons
	run_update_icons
	sleep 0.2

	[ ! -s "$RECONCILE_LOG" ]
}

@test "pane focus: selecting a non-floating pane in another repo fires zero times" {
	tmux split-window -t "$WIN" -c "$REPO_B"

	local top_a
	top_a="$(git -C "$REPO_A" rev-parse --show-toplevel)"
	tmux set -w -t "$WIN" @worktree "$top_a"
	tmux set -w -t "$WIN" @window_cwd_seen "$top_a"

	# The window's authority stays the first non-floating pane (repo A), so
	# switching the active pane to the second one (repo B) must not fire —
	# only the FIRST pane in list-panes order decides the window's cwd.
	tmux select-pane -t "$WIN".1

	run_update_icons
	run_update_icons
	sleep 0.2

	[ ! -s "$RECONCILE_LOG" ]
}
