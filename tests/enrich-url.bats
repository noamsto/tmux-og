#!/usr/bin/env bats
# A window whose checkout is gone (merged PR, worktree removed, window left open)
# must still refresh its PR badge from the url it already carries. Runs on a real
# scratch tmux server (own TMUX_TMPDIR, own CLAUDE_STATUS_DIR) with a stub gh.

load helper

PR_URL=https://github.com/acme/widgets/pull/42

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	export GH_LOG="$BATS_TEST_TMPDIR/gh.log"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/cache"
	export TMUX_TMPDIR="$BATS_TEST_TMPDIR/tmux"
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/status"
	mkdir -p "$TMUX_TMPDIR" "$CLAUDE_STATUS_DIR"
	unset TMUX TMUX_PANE

	cat >"$FAKEBIN/gh" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$GH_LOG"
		case "$*" in
		"pr view https://github.com/acme/widgets/pull/42 "*)
			printf '%s' '{"number":42,"title":"t","url":"https://github.com/acme/widgets/pull/42","state":"MERGED","mergeable":"UNKNOWN","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"statusCheckRollup":[]}'
			;;
		*) printf '[]' ;;
		esac
	EOF
	chmod +x "$FAKEBIN/gh"
	export PATH="$FAKEBIN:$PATH"
	export HOME="$BATS_TEST_TMPDIR"

	tmux -f /dev/null new-session -d -c "$BATS_TEST_TMPDIR" -s s -n w
	WIN=$(tmux list-windows -t s -F '#{window_id}')
	make_pr_enrich
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

gone_window() {
	local gone="$BATS_TEST_TMPDIR/deleted-worktree"
	tmux set-option -t "$WIN" -w @worktree "$gone"
	tmux set-option -t "$WIN" -w @git_root "$gone"
	tmux set-option -t "$WIN" -w @branch feat/x
	tmux set-option -t "$WIN" -w @pr_state open
}

@test "gone checkout with a PR url refreshes state from the url" {
	gone_window
	tmux set-option -t "$WIN" -w @pr_url "$PR_URL"
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_state)" = merged ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_number)" = 42 ]
}

@test "gone checkout with no PR url is skipped: no gh call, options unchanged" {
	gone_window
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_state)" = open ]
	[ ! -s "$GH_LOG" ]
}

@test "gone checkout with a malformed PR url is skipped" {
	gone_window
	tmux set-option -t "$WIN" -w @pr_url 'https://example.com/pull/42'
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$(tmux show-options -t "$WIN" -wqv @pr_state)" = open ]
	[ ! -s "$GH_LOG" ]
}

@test "a mirror window with a PR url stays skipped" {
	gone_window
	tmux set-option -t "$WIN" -w @pr_url "$PR_URL"
	tmux set-option -t "$WIN" -w @bridge_win 1
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$(tmux show-options -t "$WIN" -wqv @pr_state)" = open ]
	[ ! -s "$GH_LOG" ]
}

@test "a terminal answer is cached: second pass makes no further gh call" {
	gone_window
	tmux set-option -t "$WIN" -w @pr_url "$PR_URL"
	bash "$PR_ENRICH_SCRIPT" --tick-run
	bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$(grep -c 'pr view' "$GH_LOG")" -eq 1 ]
}
