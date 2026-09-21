#!/usr/bin/env bats
# Tests the #137 auto-trigger in tmux-update-icons.sh: a genuine in-place
# branch transition (previous @branch non-empty and different) re-fires the
# issue stamp, but the initial seed (no previous @branch) and a steady-state
# tick (unchanged branch) must not.
#
# Runs the real script against a private, config-less tmux server (like
# reflow-fanout.bats) so bare `tmux`/`git` calls inside it see a real window
# with a real pane cwd, not a fake.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"

	TDIR="$BATS_TEST_TMPDIR"
	export TMUX_TMPDIR="$TDIR/tmux"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	export CLAUDE_STATUS_DIR="$TDIR/claude-status"
	mkdir -p "$CLAUDE_STATUS_DIR"
	export TMPDIR="$TDIR"

	# Fake reflow: update-icons kicks it on every branch change; a no-op is fine
	# here, this file only asserts on the issue-stamp trigger.
	FAKE_REFLOW="$TDIR/fake-reflow"
	cat >"$FAKE_REFLOW" <<-EOF
		#!/bin/sh
		exit 0
	EOF
	chmod +x "$FAKE_REFLOW"

	# Fake issue stamp: records its argv so we can assert whether/how it fired.
	STAMP_LOG="$TDIR/stamp.log"
	FAKE_STAMP="$TDIR/fake-issue-stamp"
	cat >"$FAKE_STAMP" <<-EOF
		#!/bin/sh
		echo "\$*" >>"$STAMP_LOG"
	EOF
	chmod +x "$FAKE_STAMP"
	export ISSUE_STAMP_BIN="$FAKE_STAMP"

	# Runnable update-icons with Nix placeholders resolved; @issue_stamp@ is left
	# raw (unsubstituted) on purpose — ISSUE_STAMP_BIN above wins per the
	# ${ISSUE_STAMP_BIN:-@issue_stamp@} pattern (same as AGENT_DETECT_BIN).
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

	REPO="$TDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" branch -q -M main

	tmux -f /dev/null new-session -d -s S -c "$REPO" -x 200 -y 50
	tmux set -g base-index 0
	local v
	for v in thm_bg thm_mauve thm_subtext_0 thm_fg thm_overlay_0 thm_overlay_1 thm_peach thm_green thm_red; do
		tmux set -g "@$v" "#000000"
	done
}

teardown() {
	tmux kill-server 2>/dev/null || true
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

@test "initial seed (no previous branch) does not fire the stamp" {
	run_update_icons
	[ "$(tmux show -wv -t S:0 @branch)" = "main" ]
	sleep 0.2
	[ ! -s "$STAMP_LOG" ]
}

@test "a genuine in-place branch transition fires the stamp" {
	run_update_icons # seed @branch=main
	git -C "$REPO" checkout -q -b feat/42-foo
	run_update_icons
	wait_for "$STAMP_LOG"
	grep -q 'feat/42-foo' "$STAMP_LOG"
	# git root, not $REPO verbatim: tmux's pane_current_path resolves symlinks
	# (e.g. macOS /tmp -> /private/tmp), same as `git rev-parse --show-toplevel`.
	local top
	top="$(git -C "$REPO" rev-parse --show-toplevel)"
	# target/worktree/branch — window -t is session_id:index ($N:0).
	local sid
	sid="$(tmux display-message -t S -p '#{session_id}')"
	[ "$(cat "$STAMP_LOG")" = "$sid:0 $top feat/42-foo" ]
}

@test "steady state (unchanged branch) does not re-fire" {
	run_update_icons # seed
	git -C "$REPO" checkout -q -b feat/42-foo
	run_update_icons # transition, fires once
	wait_for "$STAMP_LOG"
	: >"$STAMP_LOG"

	run_update_icons # no branch change this tick
	sleep 0.2
	[ ! -s "$STAMP_LOG" ]
}
