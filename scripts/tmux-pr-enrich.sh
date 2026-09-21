#!/usr/bin/env bash
# Background PR enrichment poller. Three entry modes:
#   --tick                  cheap gate; daemonize a full pass if .last-tick stale
#   --target T --branch B [--dir D]
#                           enrich one window's branch (with --force to bypass
#                           TTL); D is a checkout dir giving gh its repo context
#   --mock-* ...            write mock @pr_* options directly (no gh), for tests
# Always exits 0. Writes @pr_number @pr_title @pr_state @pr_check_state @pr_url
# @pr_mergeable @pr_draft @pr_branch @pr_review @pr_auto_merge @pr_check_progress.
#
# gh resolves the repo from its cwd, and this poller's own cwd is the tmux
# server's (usually not a repo at all) — so every gh call must run inside a
# checkout of the branch's repo: D in single-target mode, the window's
# @worktree/@git_root in the full pass.
set -uo pipefail

# shellcheck source=/dev/null
source @lib_enrich@
# shellcheck source=/dev/null
source @lib_log@

REFRESH_SECONDS="@pr_refresh_seconds@"
CHECK_REFRESH_SECONDS="@pr_check_refresh_seconds@"
TTL=60
# A cached "no PR" expires faster: none→PR is the transition a user actively
# waits on right after `gh pr create`; PR-state changes are less urgent. Applies
# only where an open PR hasn't been ruled out — single-target mode and a failed
# batch. In a full pass the batch itself surfaces the new PR (fetch_terminal_pr).
TTL_NONE=15
# Merged/closed PRs are terminal: re-polling them every TTL wastes two serial
# gh calls per branch. The long TTL (not infinity) still catches a reopened PR
# eventually; --force (prefix+i r) checks immediately.
TTL_TERMINAL=3600

# A repo whose checks were last seen pending re-polls them on this shorter clock,
# so the badge's progress pie moves while CI runs. Capped by the settled cadence.
PENDING_CHECK_SECONDS=30
((PENDING_CHECK_SECONDS > CHECK_REFRESH_SECONDS)) && PENDING_CHECK_SECONDS=$CHECK_REFRESH_SECONDS
# Checks can sit pending for hours (a required status stuck at EXPECTED, a
# deployment awaiting approval). This long after a repo was first seen pending,
# it drops back to the settled cadence rather than spend the shared GraphQL
# budget 120 times an hour.
PENDING_MAX_SECONDS=600
# The gate stamps .last-pending-tick when it dispatches and the pass stamps the
# marker a few forks later, so on the next 5s-aligned dispatch the marker can be
# a second short of PENDING_CHECK_SECONDS. One hook period of slack keeps that
# dispatch from being a no-op that pushes the re-poll out to 60s.
PENDING_DUE_SLACK=5

# Notification seam. A value still starting with '@' means the placeholder was
# never substituted (raw script under bats, or notifications disabled at build
# time) — the single "off" mechanism. Assignment only; no fork, so the --tick
# gate stays exactly as cheap as it is today.
NOTIFY_BIN="${OG_NOTIFY_BIN:-@notify@}"

# --- arg parse ---
mode="tick"
target="" branch="" dir="" force=0
mock_number="" mock_state="" mock_check="" mock_title="" mock_url="" mock_mergeable="" mock_draft=""
mock_review="" mock_auto_merge="" mock_progress=""
while (($#)); do
	case "$1" in
	--tick) mode="tick" ;;
	--tick-run) mode="tickrun" ;;
	--tick-run-pending) mode="tickrunpending" ;;
	--target)
		target="$2"
		shift
		;;
	--branch)
		branch="$2"
		shift
		;;
	--dir)
		dir="$2"
		shift
		;;
	--force) force=1 ;;
	--mock-pr-number)
		mock_number="$2"
		mode="mock"
		shift
		;;
	--mock-pr-state)
		mock_state="$2"
		shift
		;;
	--mock-check-state)
		mock_check="$2"
		shift
		;;
	--mock-pr-title)
		mock_title="$2"
		shift
		;;
	--mock-pr-url)
		mock_url="$2"
		shift
		;;
	--mock-mergeable)
		mock_mergeable="$2"
		shift
		;;
	--mock-draft)
		mock_draft="$2"
		shift
		;;
	--mock-review)
		mock_review="$2"
		shift
		;;
	--mock-auto-merge)
		mock_auto_merge="$2"
		shift
		;;
	--mock-check-progress)
		mock_progress="$2"
		shift
		;;
	*) ;;
	esac
	shift
done

# --- helper definitions (defined before any code path calls them) ---

# notify_pr_change TARGET PREV NUMBER TITLE STATE CHECK
# One notification per genuinely new PR/CI event. PREV is write_pr_options'
# pre-write snapshot, number|state|check|mergeable|draft|review|auto_merge|progress;
# only fields 2-3 are read, and the parser's trailing `_` absorbs the rest. Only
# two things fire:
# a @pr_state flip TO merged, and a @pr_check_state flip TO failure or success.
# `pending`, `closed`, `conflicting` and draft flips never do.
#
# An EMPTY prior field is discovery, not an event — the first stamp is just the
# poller learning what already existed, and a brand-new worktree must not fire a
# burst. Note the sentinel's shape: the "no PR" write puts the literal `none` in
# @pr_number while @pr_state and @pr_check_state are written EMPTY, so the gate
# tests those two for emptiness rather than looking for `none` in them.
#
# merged wins over a same-write check flip (one notification, not two), matching
# the merged-before-check precedence the badge renderers already use.
#
# Adds no tmux call: every value is already an argument in hand.
notify_pr_change() {
	[[ $NOTIFY_BIN == @* ]] && return 0
	local tgt="$1" prev="$2" number="$3" title="$4" state="$5" check="$6"
	local p_state p_check
	IFS='|' read -r _ p_state p_check _ _ <<<"$prev"
	local level="" head=""
	if [[ $state == merged && -n $p_state && $p_state != merged ]]; then
		level="info" head="PR merged"
	elif [[ -n $p_check && $check != "$p_check" ]]; then
		case "$check" in
		failure)
			level="error" head="checks failed"
			;;
		success)
			level="info" head="checks passed"
			;;
		esac
	fi
	[[ -n $level ]] || return 0
	# --window takes the $SESSION_ID:@WINDOW_ID target run_full_pass already
	# builds; the router resolves it and normalizes the stored field to @N. Safe
	# as argv — nothing re-expands the leading $ the way a run-shell would.
	detach "$NOTIFY_BIN" emit --source pr --level "$level" \
		--window "$tgt" --title "$head" --body "#$number $title"
	return 0
}

# write_pr_options TARGET NUMBER TITLE STATE CHECK URL MERGEABLE BRANCH [DRAFT] \
#                  [REVIEW] [AUTO_MERGE] [PROGRESS]
write_pr_options() {
	# Only the badge-driving options are captured before writing so we can skip
	# the (cache-bypassing) reflow when unchanged.
	local prev
	prev=$(tmux display-message -t "$1" -p '#{@pr_number}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_review}|#{@pr_auto_merge}|#{@pr_check_progress}')
	tmux set-option -t "$1" -w @pr_number "$2"
	tmux set-option -t "$1" -w @pr_title "$3"
	tmux set-option -t "$1" -w @pr_state "$4"
	tmux set-option -t "$1" -w @pr_check_state "$5"
	tmux set-option -t "$1" -w @pr_url "$6"
	tmux set-option -t "$1" -w @pr_mergeable "${7:-}"
	# "1"/empty — an additive badge marker, not a state of its own.
	tmux set-option -t "$1" -w @pr_draft "${9:-}"
	# Style the badge's #<n> half: approved|changes_requested|review_required, and "1"/empty.
	tmux set-option -t "$1" -w @pr_review "${10:-}"
	tmux set-option -t "$1" -w @pr_auto_merge "${11:-}"
	# "<finished>/<total>" while checks are pending — picks the pie slice.
	tmux set-option -t "$1" -w @pr_check_progress "${12:-}"
	# Tags the branch this PR data describes so displays can hide it once the
	# pane cd's to a different branch (no wt switch re-stamps @pr_*). Mirrors
	# @issue_branch.
	tmux set-option -t "$1" -w @pr_branch "${8:-}"
	log_enabled && log_event enrich event pr target "$1" number "$2" state "$4" check "$5" mergeable "${7:-}" draft "${9:-}" review "${10:-}" auto_merge "${11:-}" progress "${12:-}"
	if [[ $prev != "$2|$4|$5|${7:-}|${9:-}|${10:-}|${11:-}|${12:-}" ]]; then
		@reflow@ "$(tmux display-message -t "$1" -p '#{session_name}')" --force >/dev/null 2>&1 &
	fi
	notify_pr_change "$1" "$prev" "$2" "$3" "$4" "$5"
}

# branch_cache_key DIR BRANCH — sets REPLY to the cache key (sha1). Scoped by
# the repo's git common dir so identical branch names in different repos don't
# share a cache slot; worktrees of one repo resolve to the same key.
branch_cache_key() {
	local d="$1" repo=""
	[[ -n $d ]] && repo="$(git -C "$d" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
	branch_sha1 "${repo}|$2"
}

# pending_marker REPO_ID — sets REPLY to the repo's pending-checks marker. Its
# presence means the repo's last applied rollup had a pending PR; its mtime is
# when that repo's last checks refresh started; its content is the epoch the
# repo was first seen pending.
pending_marker() {
	branch_sha1 "$1"
	REPLY="$ENRICH_CACHE_DIR/$REPLY.checks-pending$ENRICH_SRV"
}

# arm_pending_marker REPO_ID — create the repo's pending marker if it has none.
# First-seen is written only here, so later touches move the mtime and never the
# content. Dropping .last-pending-tick undoes a pass's push-ahead (set when every
# marker was past PENDING_MAX_SECONDS), so the new repo's fast cadence starts on
# the next tick. Leaves REPLY as the marker path.
arm_pending_marker() {
	pending_marker "$1"
	[[ -f $REPLY ]] && return
	printf '%s\n' "$EPOCHSECONDS" >"$REPLY"
	rm -f "$ENRICH_CACHE_DIR/.last-pending-tick$ENRICH_SRV"
}

# fetch_branch_pr DIR BRANCH [KEY]  → echoes cache JSON path, refreshing via
# gh if stale. DIR is a checkout of the branch's repo; KEY is the precomputed
# cache key (derived from DIR+BRANCH when absent, saving a git fork for
# callers that already hold it). Asks for the open PR first, then --state all
# for a merged/closed one, so an open PR always wins over an older closed one
# on the same head.
fetch_branch_pr() {
	local d="$1" b="$2" key="${3:-}"
	if [[ -z $key ]]; then
		branch_cache_key "$d" "$b"
		key="$REPLY"
	fi
	fetch_pr_cached "$d" "$b" "$key" open+all "$TTL_NONE"
}

# fetch_terminal_pr DIR BRANCH KEY  → echoes cache JSON path. For a branch the
# repo batch has already proven has no OPEN PR: merged, closed and never-had-one
# are all terminal, so one --state all call settles it and the answer holds for
# TTL_TERMINAL even when it is "[]". Nothing is lost by that long TTL — a PR
# opened later appears in the next pass's batch by headRefName, no lookup needed.
fetch_terminal_pr() {
	fetch_pr_cached "$1" "$2" "$3" all "$TTL_TERMINAL"
}

# fetch_pr_cached DIR BRANCH KEY STATES TTL_NONE — shared body of the two
# lookups above. STATES is "open+all" (open PR first, fall back to merged/closed)
# or "all" (one call). TTL_NONE governs how long a "no PR" answer is trusted.
fetch_pr_cached() {
	local d="$1" b="$2" key="$3" states="$4" ttl_none="$5"
	local cache="$ENRICH_CACHE_DIR/$key.json"
	local check_cache="$ENRICH_CACHE_DIR/$key.checks.json"
	local lock="$ENRICH_CACHE_DIR/$key.lock"

	# Serve the cache when the decision says so (fresh + not forced).
	local exists=0 content="" age=0
	if [[ -f $cache ]]; then
		exists=1
		content="$(<"$cache")"
		age=$((EPOCHSECONDS - $(file_mtime "$cache")))
	fi
	pr_cache_decision "$force" "$exists" "$content" "$age" "$TTL" "$ttl_none" "$TTL_TERMINAL"
	if [[ $REPLY == "serve" ]]; then
		printf '%s' "$cache"
		return
	fi

	command -v gh >/dev/null 2>&1 || {
		printf '%s' "$cache"
		return
	}

	# Locked fetch in a subshell: acquire_lock's EXIT trap releases when the
	# subshell exits, scoping the lock to THIS branch's fetch (not the whole
	# pass). If another process holds the lock, skip the fetch and serve the
	# cache. A failed gh call (offline, auth, rate limit) leaves the cache
	# untouched so the last-known PR state keeps showing instead of wiping to
	# "none".
	(
		acquire_lock "$lock" || exit 0
		if [[ -n $d ]]; then cd "$d" 2>/dev/null || exit 0; fi
		local json="" fields="number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest"
		((force)) && fields+=",statusCheckRollup"
		if [[ $states == open+all ]]; then
			json="$(gh pr list --head "$b" --state open --limit 1 \
				--json "$fields" 2>/dev/null)" || exit 0
		fi
		if [[ $json == "[]" || -z $json ]]; then
			json="$(gh pr list --head "$b" --state all --limit 1 \
				--json "$fields" 2>/dev/null)" || exit 0
		fi
		if ((force)); then
			jq 'map(del(.statusCheckRollup))' <<<"$json" >"$cache.tmp.$$" && mv -f "$cache.tmp.$$" "$cache"
			jq 'map({statusCheckRollup: (.statusCheckRollup // [])})' <<<"$json" >"$check_cache.tmp.$$" && mv -f "$check_cache.tmp.$$" "$check_cache"
		else
			printf '%s' "$json" >"$cache.tmp.$$" && mv -f "$cache.tmp.$$" "$cache"
		fi
	)
	printf '%s' "$cache"
}

# apply_cache_to_target TARGET CACHE_PATH BRANCH
# Also sets the global APPLIED_CHECK to the collapsed check state it wrote, or ""
# when nothing was applied.
apply_cache_to_target() {
	APPLIED_CHECK=""
	local tgt="$1" cache="$2" br="$3"
	# No cache file = this branch was never successfully fetched: keep the
	# last-known options instead of wiping to "none" (offline / rate-limit
	# resilience). A genuine "no PR" answer is a present "[]" cache.
	[[ -f $cache ]] || return
	local json
	json="$(cat "$cache")"
	if [[ $json == "[]" || -z $json ]]; then
		write_pr_options "$tgt" "none" "" "" "" "" "" "$br"
		return
	fi
	# One jq pass emits every identity field on its own line (line-by-line `read`
	# preserves empty fields — a tab/space delimiter would collapse them). PR titles are
	# single-line, so newline-delimiting is safe.
	local number title url state mergeable draft review auto_merge
	{
		IFS= read -r number
		IFS= read -r state
		IFS= read -r mergeable
		IFS= read -r draft
		IFS= read -r review
		IFS= read -r auto_merge
		IFS= read -r url
		IFS= read -r title
	} < <(jq -r '
		(.[0].number // "" | tostring),
		(.[0].state // "" | ascii_downcase),
		(.[0].mergeable // "" | ascii_downcase),
		(if .[0].isDraft then "1" else "" end),
		(.[0].reviewDecision // "" | ascii_downcase),
		(if .[0].autoMergeRequest then "1" else "" end),
		(.[0].url // ""),
		(.[0].title // "")
	' <<<"$json")
	local check_cache="${cache%.json}.checks.json" rollup="[]"
	if [[ -f $check_cache ]]; then
		rollup="$(jq -c '.[0].statusCheckRollup // []' <"$check_cache")"
	elif jq -e '.[0].statusCheckRollup' >/dev/null 2>&1 <<<"$json"; then
		# Preserve check state from combined cache entries during an upgrade.
		rollup="$(jq -c '.[0].statusCheckRollup // []' <<<"$json")"
	fi
	collapse_check_rollup "$rollup"
	local check="$REPLY" progress="$REPLY_PROGRESS"
	APPLIED_CHECK="$check"
	sanitize_title "$title"
	write_pr_options "$tgt" "$number" "$REPLY" "$state" "$check" "$url" "$mergeable" "$br" "$draft" "$review" "$auto_merge" "$progress"
}

# refresh_repo_checks DIR REPO_ID BRANCHES — refresh a repo's open-PR rollups.
# Identity has a separate, faster cache, so routine refreshes never combine the
# two GraphQL selections.
refresh_repo_checks() {
	local d="$1" repo_id="$2"
	local branches=() all_json head obj
	mapfile -t branches <<<"$3"
	command -v gh >/dev/null 2>&1 || return
	all_json="$(cd "$d" 2>/dev/null && gh pr list --state open --limit 100 \
		--json headRefName,statusCheckRollup 2>/dev/null)" || return
	[[ -n $all_json ]] || return

	declare -A checks
	while IFS=$'\t' read -r head obj; do
		[[ -n $head ]] && checks[$head]="$obj"
	done < <(jq -r '.[] | "\(.headRefName)\t\([{statusCheckRollup}])"' <<<"$all_json")

	local br ck check_cache
	for br in "${branches[@]}"; do
		[[ -z $br ]] && continue
		branch_sha1 "$repo_id|$br"
		ck="$REPLY"
		check_cache="$ENRICH_CACHE_DIR/$ck.checks.json"
		if [[ -n ${checks[$br]+x} ]]; then
			printf '%s' "${checks[$br]}" >"$check_cache.tmp.$$" && mv -f "$check_cache.tmp.$$" "$check_cache"
		else
			printf '[]' >"$check_cache.tmp.$$" && mv -f "$check_cache.tmp.$$" "$check_cache"
		fi
	done
}

# enrich_repo_group DIR REPO_ID BRANCHES WINDOWS REFRESH_CHECKS REFRESH_IDENTITY
# One repo's slice of the full pass. REPO_ID is the repo's git common dir
# (already resolved by run_full_pass; reused for cache keys so no git forks
# happen here). BRANCHES is newline-separated; WINDOWS is newline-separated
# "target|branch" lines. One gh call indexes the repo's open PRs by head branch
# (headRefName; each value is a single-element array matching the per-branch
# cache format), so the common case — each worktree has an open PR — costs a
# single API round-trip per repo. A successful batch is authoritative for open
# PRs: heads missing from it have none, which is a terminal answer
# (fetch_terminal_pr). REFRESH_IDENTITY=0 is a checks-only pass: identity is
# served from its cache files as they stand.
enrich_repo_group() {
	local d="$1" repo_id="$2" refresh_checks="$5" refresh_identity="$6"
	local branches=() wlines=()
	mapfile -t branches <<<"$3"
	mapfile -t wlines <<<"$4"

	local br ck cache
	if ((refresh_identity)); then
		declare -A open_pr
		local all_json head obj batch_ok=0
		if command -v gh >/dev/null 2>&1 &&
			all_json="$(cd "$d" 2>/dev/null && gh pr list --state open --limit 100 \
				--json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName 2>/dev/null)" &&
			[[ -n $all_json ]]; then
			batch_ok=1
			while IFS=$'\t' read -r head obj; do
				[[ -n $head ]] && open_pr[$head]="$obj"
			done < <(jq -r '.[] | "\(.headRefName)\t\([.])"' <<<"$all_json")
		fi

		for br in "${branches[@]}"; do
			[[ -z $br ]] && continue
			branch_sha1 "$repo_id|$br"
			ck="$REPLY"
			cache="$ENRICH_CACHE_DIR/$ck.json"
			if [[ -n ${open_pr[$br]+x} ]]; then
				printf '%s' "${open_pr[$br]}" >"$cache.tmp.$$" && mv -f "$cache.tmp.$$" "$cache"
			elif ((batch_ok)); then
				# No open PR for this head, on the batch's authority: only merged,
				# closed or none is left, and the next batch catches a PR opened later.
				cache="$(fetch_terminal_pr "$d" "$br" "$ck")"
			else
				# The batch itself failed (no gh, offline, rate-limited): nothing has
				# been ruled out, so run the full lookup — it serves the cache on
				# failure rather than wiping to "none".
				cache="$(fetch_branch_pr "$d" "$br" "$ck")"
			fi
		done
	fi
	if ((refresh_checks)); then
		refresh_repo_checks "$d" "$repo_id" "$3"
	fi

	local line tgt b2 any_pending=0
	for br in "${branches[@]}"; do
		[[ -z $br ]] && continue
		branch_sha1 "$repo_id|$br"
		cache="$ENRICH_CACHE_DIR/$REPLY.json"
		for line in "${wlines[@]}"; do
			IFS="|" read -r tgt b2 <<<"$line"
			[[ $b2 == "$br" ]] || continue
			apply_cache_to_target "$tgt" "$cache" "$br"
			[[ $APPLIED_CHECK == pending ]] && any_pending=1
		done
	done

	# An existing marker's refresh stamp was already set by run_full_pass, before
	# this group launched.
	if ((any_pending)); then
		arm_pending_marker "$repo_id"
	else
		pending_marker "$repo_id"
		rm -f "$REPLY"
	fi
}

# run_full_pass [PENDING_ONLY] — enrich every window that carries a @branch.
# Windows are grouped by repo (git common dir, derived from @worktree/@git_root);
# each group runs concurrently as one enrich_repo_group. Multi-repo setups pay
# one round-trip per repo, all in flight at once. PENDING_ONLY=1 is the
# checks-only pass: it runs just the repos whose pending marker is due.
run_full_pass() {
	local pending_only="${1:-0}"
	local refresh_checks=0 check_tick="$ENRICH_CACHE_DIR/.last-check-tick$ENRICH_SRV"
	if ((! pending_only)) && { [[ ! -f $check_tick ]] || ((EPOCHSECONDS - $(file_mtime "$check_tick") >= CHECK_REFRESH_SECONDS)); }; then
		refresh_checks=1
		touch "$check_tick"
	fi
	local windows
	mapfile -t windows < <(tmux list-windows -a -F '#{session_id}:#{window_id}|#{@worktree}|#{@git_root}|#{@branch}|#{@bridge_win}' | awk -F'|' 'NF>=5')

	# Unique branches (capped — matches the prior head -n 30 bound) and window
	# lines, grouped by repo. Windows with no resolvable repo checkout are
	# skipped: gh could only run in the server's cwd (the original wrong-repo
	# bug), so they keep their last-known options instead.
	declare -A seen grp_dir grp_branches grp_windows
	local total=0 truncated=0 line tgt wt gr br bw d key sk
	for line in "${windows[@]}"; do
		IFS="|" read -r tgt wt gr br bw <<<"$line"
		[[ -z $br ]] && continue
		# Remote-bridge mirror window (#167 @bridge_win opt-out): no PR to poll
		# for — skip it rather than fetch data for the launcher's repo.
		[[ $bw == 1 ]] && continue
		d="${wt:-$gr}"
		[[ -z $d ]] && continue
		key="$(git -C "$d" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
		# Stale @worktree (dir removed out from under the window): retry with
		# @git_root before giving up.
		if [[ -z $key && -n $gr && $gr != "$d" ]]; then
			d="$gr"
			key="$(git -C "$d" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
		fi
		[[ -z $key ]] && continue
		grp_windows[$key]+="$tgt|$br"$'\n'
		sk="$key|$br"
		[[ -n ${seen[$sk]:-} ]] && continue
		if ((total >= 30)); then
			truncated=1
			continue
		fi
		seen[$sk]=1
		grp_dir[$key]="$d"
		grp_branches[$key]+="$br"$'\n'
		((++total))
	done
	declare -A marker live
	local k m
	for k in "${!grp_branches[@]}"; do
		pending_marker "$k"
		marker[$k]="$REPLY"
		live[$REPLY]=1
	done
	# Only enrich_repo_group clears a marker, and a repo whose last window closed
	# (or whose worktree was removed) never reaches it again — its marker would
	# keep the gate dispatching passes forever. A capped pass may have skipped a
	# live repo, so it removes nothing.
	if ((! truncated)); then
		for m in "$ENRICH_CACHE_DIR"/*.checks-pending"$ENRICH_SRV"; do
			[[ -f $m && -z ${live[$m]:-} ]] && rm -f "$m"
		done
	fi
	((total)) || return

	local due fast=0 first
	for k in "${!grp_branches[@]}"; do
		m="${marker[$k]}"
		due=0
		if [[ -f $m ]]; then
			read -r first <"$m"
			[[ $first =~ ^[0-9]+$ ]] || first=0
			if ((EPOCHSECONDS - first < PENDING_MAX_SECONDS)); then
				fast=1
				((EPOCHSECONDS - $(file_mtime "$m") >= PENDING_CHECK_SECONDS - PENDING_DUE_SLACK)) && due=1
			fi
		fi
		((pending_only && ! due)) && continue
		# Stamp the refresh's start, not its end: the gate measures from its own
		# dispatch, and a concurrent pass must not see this repo as still due.
		[[ -f $m ]] && ((refresh_checks || due)) && touch "$m"
		enrich_repo_group "${grp_dir[$k]}" "$k" "${grp_branches[$k]}" "${grp_windows[$k]}" \
			"$((refresh_checks || due))" "$((! pending_only))" &
	done
	wait
	# Every marker is past PENDING_MAX_SECONDS: push .last-pending-tick a settled
	# refresh ahead so the gate stops launching passes that find nothing due.
	# arm_pending_marker drops it the moment a new repo goes pending.
	if ((pending_only && ! fast)); then
		touch -t "$(printf '%(%Y%m%d%H%M.%S)T' $((EPOCHSECONDS + CHECK_REFRESH_SECONDS)))" "$ENRICH_CACHE_DIR/.last-pending-tick$ENRICH_SRV"
	fi
}

# --- mock mode: write the provided values directly, no gh ---
if [[ $mode == "mock" ]]; then
	[[ -z $target ]] && exit 0
	sanitize_title "$mock_title"
	write_pr_options "$target" "$mock_number" "$REPLY" "$mock_state" "$mock_check" "$mock_url" "$mock_mergeable" "$branch" "$mock_draft" "$mock_review" "$mock_auto_merge" "$mock_progress"
	exit 0
fi

# --- tickrun: the detached child re-invoked itself; run exactly one pass ---
if [[ $mode == "tickrun" ]]; then
	mkdir -p "$ENRICH_CACHE_DIR" 2>/dev/null
	run_full_pass
	exit 0
fi

# --- tickrunpending: a checks-only pass for repos with pending checks ---
if [[ $mode == "tickrunpending" ]]; then
	mkdir -p "$ENRICH_CACHE_DIR" 2>/dev/null
	run_full_pass 1
	exit 0
fi

# --- single-target mode (from dispatcher / force refresh) ---
if [[ -n $target && -n $branch ]]; then
	# Remote-bridge mirror window (#167 @bridge_win opt-out): no PR to poll for
	# — its branch belongs to the launcher's repo, not the remote content.
	[[ $(tmux show-options -t "$target" -wqv @bridge_win 2>/dev/null) == 1 ]] && exit 0
	mkdir -p "$ENRICH_CACHE_DIR" 2>/dev/null
	cache="$(fetch_branch_pr "$dir" "$branch")"
	apply_cache_to_target "$target" "$cache" "$branch"
	# prefix + i r on a pending PR arms the fast cadence now, not at the next
	# full pass. Repo id derived exactly as run_full_pass derives it.
	if [[ $APPLIED_CHECK == pending && -n $dir ]]; then
		repo="$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
		[[ -n $repo ]] && arm_pending_marker "$repo"
	fi
	exit 0
fi

# --- tick mode: cheap gate, then daemonize a full pass ---
last_tick="$ENRICH_CACHE_DIR/.last-tick$ENRICH_SRV"
if ((force == 0)) && [[ -f $last_tick ]]; then
	tick_age=$((EPOCHSECONDS - $(file_mtime "$last_tick")))
	if ((tick_age < REFRESH_SECONDS)); then
		# Glob-and-test keeps a tick with nothing pending fork-free. Not compgen:
		# nixpkgs' non-interactive bash is built without it.
		pending_markers=("$ENRICH_CACHE_DIR"/*.checks-pending"$ENRICH_SRV")
		[[ -e ${pending_markers[0]} ]] || exit 0
		pending_tick="$ENRICH_CACHE_DIR/.last-pending-tick$ENRICH_SRV"
		if [[ -f $pending_tick ]] && ((EPOCHSECONDS - $(file_mtime "$pending_tick") < PENDING_CHECK_SECONDS)); then
			exit 0
		fi
		touch "$pending_tick"
		detach "${BASH_SOURCE[0]}" --tick-run-pending
		exit 0
	fi
fi
# Mark the tick fresh BEFORE daemonizing: best-effort — if the detached pass
# crashes we wait one cycle; --force / the prefix+i r keybind force a retry.
mkdir -p "$ENRICH_CACHE_DIR" 2>/dev/null
touch "$last_tick"

# Detach so the status refresh returns immediately. The child re-invokes with
# --tick-run, which runs run_full_pass once and exits (no re-daemonize).
detach "${BASH_SOURCE[0]}" --tick-run
exit 0
