#!/usr/bin/env bats
# shellcheck disable=SC2016 # the $-prefixed tmux session id is literal, not an expansion
# API-budget shape of a full enrichment pass. Unusually, these assert on the gh
# calls a pass makes rather than the tmux options it writes: the poller shares a
# 5000/hr GraphQL bucket with every other tool on the machine, so how often it
# asks is the behaviour worth pinning.
#
# Fakes: gh logs its argv and answers from env; tmux answers list-windows from
# $FAKE_WINDOWS and swallows set-option.

load helper

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	export GH_LOG="$BATS_TEST_TMPDIR/gh.log"
	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	export TMUX_SRV_LOG="$BATS_TEST_TMPDIR/tmux-srv.log"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/cache"
	unset TMUX TMUX_PANE

	cat >"$FAKEBIN/gh" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$*" >>"$GH_LOG"
		case "$*" in
		*"--json headRefName,statusCheckRollup"*) printf '%s' "$GH_CHECK_JSON" ;;
		*"--state open --limit 100"*)
			[ -n "${GH_BATCH_FAIL:-}" ] && exit 1
			printf '%s' "$GH_BATCH_JSON"
			;;
		*"--head feat/has-pr --state open"*) printf '%s' "${GH_HEAD_JSON:-[]}" ;;
		*) printf '[]' ;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/gh"

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$1" in
		list-windows) printf '%s\n' "$FAKE_WINDOWS" ;;
		display-message) printf '\n' ;;
		set-option)
			printf '%s\n' "$*" >>"$TMUX_LOG"
			printf '%s %s\n' "${TMUX%%,*}" "$*" >>"$TMUX_SRV_LOG"
			;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"

	export PATH="$FAKEBIN:$PATH"
	export HOME="$BATS_TEST_TMPDIR" # keep git off any real user config

	REPO="$BATS_TEST_TMPDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init

	# One window per branch, same repo: one has an open PR, one has none.
	export FAKE_WINDOWS='$1:@1|'"$REPO"'||feat/has-pr|
$1:@2|'"$REPO"'||feat/no-pr|'
	export GH_BATCH_JSON='[{"number":7,"title":"t","url":"u","state":"OPEN","statusCheckRollup":[],"mergeable":"MERGEABLE","isDraft":false,"reviewDecision":"APPROVED","autoMergeRequest":{"enabledAt":"2026-09-15T00:00:00Z"},"headRefName":"feat/has-pr"}]'
	export GH_CHECK_JSON='[{"headRefName":"feat/has-pr","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]}]'

	make_pr_enrich
}

# Counts logged gh calls matching a fixed string.
gh_calls() {
	grep -cF -- "$1" "$GH_LOG" || true
}

@test "pass: a branch with an open PR costs only the repo batch" {
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--head feat/has-pr')" -eq 0 ]
	[ "$(gh_calls '--json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName')" -eq 1 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 1 ]
	grep -q -- '@pr_check_state success' "$TMUX_LOG"
}

@test "pass: a PR-less branch is settled with one --state all call, never --state open" {
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--head feat/no-pr --state all --limit 1')" -eq 1 ]
	[ "$(gh_calls '--head feat/no-pr --state open --limit 1')" -eq 0 ]
}

@test "pass: a second pass re-asks nothing for the PR-less branch" {
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	# Identity stays fresh on every pass, but check state remains cached until
	# the independent, slower cadence expires. The terminal answer for feat/no-pr
	# is also cached for TTL_TERMINAL.
	[ "$(gh_calls '--json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName')" -eq 2 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 1 ]
	[ "$(gh_calls '--head feat/no-pr')" -eq 1 ]
}

@test "force refresh fetches the current branch's check rollup immediately" {
	run bash "$PR_ENRICH_SCRIPT" --target '$1:@1' --branch feat/has-pr --dir "$REPO" --force
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--head feat/has-pr --state open --limit 1 --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,statusCheckRollup')" -eq 1 ]
}

@test "pass: a failed batch falls back to the full open-then-all lookup" {
	GH_BATCH_FAIL=1 run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	# Nothing was ruled out, so both heads get the two-call lookup rather than
	# inheriting the batch's authority.
	[ "$(gh_calls '--head feat/has-pr --state open --limit 1')" -eq 1 ]
	[ "$(gh_calls '--head feat/has-pr --state all --limit 1')" -eq 1 ]
	[ "$(gh_calls '--head feat/no-pr --state open --limit 1')" -eq 1 ]
}

@test "pass: review decision and auto-merge are stamped from the identity batch" {
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	grep -q -- '@pr_review approved' "$TMUX_LOG"
	grep -q -- '@pr_auto_merge 1' "$TMUX_LOG"
	grep -q -- '@pr_check_progress $' "$TMUX_LOG"
}

@test "pass: a pending rollup stamps finished/total progress" {
	GH_CHECK_JSON='[{"headRefName":"feat/has-pr","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]}]' \
		run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	grep -q -- '@pr_check_state pending' "$TMUX_LOG"
	grep -q -- '@pr_check_progress 1/2' "$TMUX_LOG"
}

PENDING_CHECK_JSON='[{"headRefName":"feat/has-pr","statusCheckRollup":[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]}]'

# markers — the pending markers currently in the cache dir, one per line.
markers() {
	compgen -G "$OG_ENRICH_CACHE_DIR/*.checks-pending" || true
}

@test "pending: a pending rollup leaves a repo marker, a settled one clears it" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(markers | wc -l)" -eq 1 ]
	# Backdate past PENDING_CHECK_SECONDS so the next pass is due for this repo.
	touch -t 200001010000 "$(markers)"
	run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 2 ]
	[ -z "$(markers)" ]
}

@test "pending: a fresh marker does not re-poll checks yet" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 1 ]
}

@test "pending: a checks-only pass skips the identity batch" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	touch -t 200001010000 "$(markers)"
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run-pending
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName')" -eq 1 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 2 ]
	grep -q -- '@pr_check_state pending' "$TMUX_LOG"
}

@test "pending: a checks-only pass with nothing due calls gh not at all" {
	run bash "$PR_ENRICH_SCRIPT" --tick-run-pending
	[ "$status" -eq 0 ]
	[ ! -s "$GH_LOG" ]
}

# make_gate — the tick gate re-execs itself through detach, so it needs to be
# runnable as a file, with a shebang that resolves inside the nix build sandbox.
# It runs with compgen disabled: nixpkgs' non-interactive bash is built without
# it, and the gate must not depend on it (#705). Sets GATE.
make_gate() {
	GATE="$BATS_TEST_TMPDIR/tmux-pr-enrich-exec"
	{
		printf '#!%s\n' "$BASH"
		printf 'enable -n compgen\n'
		tail -n +2 "$PR_ENRICH_SCRIPT"
	} >"$GATE"
	chmod +x "$GATE"
}

# wait_for_srv SOCKET — wait until a pass on that tmux server has written options.
wait_for_srv() {
	local i
	for ((i = 0; i < 50; i++)); do
		grep -q "^$1 " "$TMUX_SRV_LOG" 2>/dev/null && return 0
		sleep 0.1
	done
	return 1
}

# stamp EPOCH FILE — set FILE's mtime to EPOCH (portable: no GNU touch -d).
stamp() {
	touch -t "$(printf '%(%Y%m%d%H%M.%S)T' "$1")" "$2"
}

@test "pending: the gate's next dispatch re-polls a marker stamped just after it" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$(markers | wc -l)" -eq 1 ]
	local now=$EPOCHSECONDS
	# The gate stamps .last-pending-tick when it dispatches; the pass it launched
	# stamps the marker a moment later. One PENDING_CHECK_SECONDS on, the gate is
	# due again — and the repo must be due in the pass that dispatch launches.
	touch "$OG_ENRICH_CACHE_DIR/.last-tick"
	stamp $((now - 30)) "$OG_ENRICH_CACHE_DIR/.last-pending-tick"
	stamp $((now - 29)) "$(markers)"
	make_gate
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run "$GATE" --tick
	[ "$status" -eq 0 ]
	# The gate detaches its pass; wait for it to reach gh.
	local i
	for ((i = 0; i < 50; i++)); do
		[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 2 ] && break
		sleep 0.1
	done
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 2 ]
}

@test "pending: a marker for a repo with no window is removed, a live one stays" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$(markers | wc -l)" -eq 1 ]
	local live
	live="$(markers)"
	printf '%s\n' "$EPOCHSECONDS" >"$OG_ENRICH_CACHE_DIR/0000000000000000000000000000000000000000.checks-pending"
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ "$(markers)" = "$live" ]
}

@test "pending: a repo pending past the cap no longer re-polls on the fast cadence" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run
	local now=$EPOCHSECONDS
	# First seen 700s ago (past PENDING_MAX_SECONDS), last refreshed long ago.
	printf '%s\n' "$((now - 700))" >"$(markers)"
	stamp $((now - 100)) "$(markers)"
	GH_CHECK_JSON="$PENDING_CHECK_JSON" run bash "$PR_ENRICH_SCRIPT" --tick-run-pending
	[ "$status" -eq 0 ]
	[ "$(gh_calls '--json headRefName,statusCheckRollup')" -eq 1 ]
	# The marker stays until checks settle, but nothing is left on the fast
	# cadence: the pass pushes .last-pending-tick ahead so the gate stops
	# dispatching no-op passes.
	[ "$(markers | wc -l)" -eq 1 ]
	[ -f "$OG_ENRICH_CACHE_DIR/.last-pending-tick" ]
	[ "$(stat -c %Y "$OG_ENRICH_CACHE_DIR/.last-pending-tick" 2>/dev/null || stat -f %m "$OG_ENRICH_CACHE_DIR/.last-pending-tick")" -gt "$now" ]
}

@test "force refresh of a pending PR arms the fast check cadence" {
	GH_HEAD_JSON='[{"number":7,"title":"t","url":"u","state":"OPEN","mergeable":"MERGEABLE","isDraft":false,"statusCheckRollup":[{"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""}]}]' \
		run bash "$PR_ENRICH_SCRIPT" --target '$1:@1' --branch feat/has-pr --dir "$REPO" --force
	[ "$status" -eq 0 ]
	grep -q -- '@pr_check_state pending' "$TMUX_LOG"
	[ "$(markers | wc -l)" -eq 1 ]
	[ "$(cat "$(markers)")" -le "$EPOCHSECONDS" ]
}

@test "tick: the gate runs with no compgen builtin and still dispatches nothing when idle" {
	make_gate
	mkdir -p "$OG_ENRICH_CACHE_DIR"
	touch "$OG_ENRICH_CACHE_DIR/.last-tick"
	run "$GATE" --tick
	[ "$status" -eq 0 ]
	[[ $output != *"command not found"* ]]
	sleep 0.3
	[ ! -s "$GH_LOG" ]
	[ ! -e "$OG_ENRICH_CACHE_DIR/.last-pending-tick" ]
}

@test "tick: another tmux server's gate stamp does not starve this server's pass (#705)" {
	make_gate
	# Two servers share one cache dir. B ticks first and its pass runs against B's
	# windows; A's tick, inside the same refresh window, must still get its own.
	TMUX=/tmp/sockB,1,0 run "$GATE" --tick
	[ "$status" -eq 0 ]
	wait_for_srv /tmp/sockB
	# Let B's pass finish so A's fetches aren't skipped on a per-branch lock.
	local i
	for ((i = 0; i < 50; i++)); do
		[ -z "$(compgen -G "$OG_ENRICH_CACHE_DIR/*.lock")" ] && break
		sleep 0.1
	done
	TMUX=/tmp/sockA,1,0 run "$GATE" --tick
	[ "$status" -eq 0 ]
	wait_for_srv /tmp/sockA
}

@test "pass: another server's pending markers survive this server's cleanup (#705)" {
	GH_CHECK_JSON="$PENDING_CHECK_JSON" TMUX=/tmp/sockA,1,0 run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	local marker
	marker="$(compgen -G "$OG_ENRICH_CACHE_DIR/*.checks-pending*")"
	[ -f "$marker" ]
	# B has no windows at all, so nothing of A's is live from B's point of view.
	FAKE_WINDOWS='' TMUX=/tmp/sockB,1,0 run bash "$PR_ENRICH_SCRIPT" --tick-run
	[ "$status" -eq 0 ]
	[ -f "$marker" ]
}
