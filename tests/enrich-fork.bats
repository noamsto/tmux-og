#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031 # bats @test blocks run in subshells; export is intentional
# `gh pr list --head <b>` matches the head branch NAME across forks, so a fork PR
# whose branch is also called `main` used to shadow the repo's own PR — or stamp
# a window that has none with a long-merged fork's state (#926). These drive the
# poller's single-target and full-pass paths against a stub gh that returns fork
# and same-repo PRs sharing a head name.

load helper

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	export GH_LOG="$BATS_TEST_TMPDIR/gh.log"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/cache"
	export TMUX_TMPDIR="$BATS_TEST_TMPDIR/tmux"
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/status"
	mkdir -p "$TMUX_TMPDIR" "$CLAUDE_STATUS_DIR"
	unset TMUX TMUX_PANE

	# Order matters: the checks batch is asked for isCrossRepository only; head
	# lookups carry --head; the identity batch is the one --state open call with
	# no --head.
	cat >"$FAKEBIN/gh" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$GH_LOG"
		case "$*" in
		*"--json headRefName,statusCheckRollup,headRefOid,isCrossRepository"*) printf '%s' "${GH_CHECK_JSON:-[]}" ;;
		*"--head main --state open"*) printf '%s' "${GH_HEAD_JSON:-[]}" ;;
		*"--head main --state all"*) printf '%s' "${GH_HEAD_ALL_JSON:-[]}" ;;
		*"--state open --limit 100 --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName,isCrossRepository"*) printf '%s' "${GH_BATCH_JSON:-[]}" ;;
		*) printf '[]' ;;
		esac
	EOF
	chmod +x "$FAKEBIN/gh"
	export PATH="$FAKEBIN:$PATH"
	export HOME="$BATS_TEST_TMPDIR"

	REPO="$BATS_TEST_TMPDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init

	tmux -f /dev/null new-session -d -c "$REPO" -s s -n w
	WIN=$(tmux list-windows -t s -F '#{window_id}')
	tmux set-option -t "$WIN" -w @worktree "$REPO"
	tmux set-option -t "$WIN" -w @git_root "$REPO"
	tmux set-option -t "$WIN" -w @branch main
	make_pr_enrich
}

teardown() {
	tmux kill-server 2>/dev/null || true
}

# fork_pr — a one-element gh JSON array holding only a merged fork PR on head
# `main` (the #926 shape).
fork_pr() {
	printf '%s' '[{"number":46,"title":"fork","url":"https://github.com/fork/repo/pull/46","state":"MERGED","mergeable":"UNKNOWN","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"isCrossRepository":true}]'
}

# fork_then_same — the fork PR followed by the repo's own open PR, same head.
fork_then_same() {
	fork_pr | jq -c '. + [{"number":7,"title":"ours","url":"https://github.com/acme/widgets/pull/7","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"isCrossRepository":false}]'
}

@test "a fork PR sharing the branch name does not stamp the window" {
	GH_HEAD_JSON="$(fork_pr)"
	export GH_HEAD_JSON GH_HEAD_ALL_JSON='[]'
	run bash "$PR_ENRICH_SCRIPT" --target "$WIN" --branch main --dir "$REPO" --force
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_number)" = none ]
	[ -z "$(tmux show-options -t "$WIN" -wqv @pr_state)" ]
}

@test "a window already stamped with a fork PR clears to no PR" {
	tmux set-option -t "$WIN" -w @pr_number 46
	tmux set-option -t "$WIN" -w @pr_state merged
	GH_HEAD_JSON="$(fork_pr)"
	export GH_HEAD_JSON GH_HEAD_ALL_JSON='[]'
	run bash "$PR_ENRICH_SCRIPT" --target "$WIN" --branch main --dir "$REPO" --force
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_number)" = none ]
	[ -z "$(tmux show-options -t "$WIN" -wqv @pr_state)" ]
}

@test "a same-repo PR wins over an earlier fork PR on the same head" {
	GH_HEAD_JSON="$(fork_then_same)"
	export GH_HEAD_JSON
	run bash "$PR_ENRICH_SCRIPT" --target "$WIN" --branch main --dir "$REPO" --force
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_number)" = 7 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_state)" = open ]
}

@test "check-rollup mapping ignores a fork PR sharing the branch name" {
	export GH_BATCH_JSON='[{"number":7,"title":"ours","url":"https://github.com/acme/widgets/pull/7","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"","autoMergeRequest":null,"headRefName":"main","isCrossRepository":false}]'
	# Fork entry LAST: unfiltered it would overwrite the same-repo rollup and the
	# badge would show failure instead of success.
	export GH_CHECK_JSON='[{"headRefName":"main","isCrossRepository":false,"statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]},{"headRefName":"main","isCrossRepository":true,"statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]}]'
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(tmux show-options -t "$WIN" -wqv @pr_check_state)" = success ]
}

@test "the cached JSON carries no isCrossRepository field" {
	GH_HEAD_JSON="$(fork_then_same)"
	export GH_HEAD_JSON
	run bash "$PR_ENRICH_SCRIPT" --target "$WIN" --branch main --dir "$REPO" --force
	[ "$status" -eq 0 ]
	run grep -rl isCrossRepository "$OG_ENRICH_CACHE_DIR"
	[ "$status" -ne 0 ]
}
