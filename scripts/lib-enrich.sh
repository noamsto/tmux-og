#!/usr/bin/env bash
# Shared issue/PR enrichment utilities for tmux scripts.
# Sourced (not executed) — provides constants and functions.
# Functions use the REPLY convention (set REPLY instead of echoing) to avoid
# subshell forks, matching lib-icons.sh / lib-claude.sh.

# shellcheck disable=SC2034  # used by scripts that source this library

# PR-state cache dir; consumed by the PR enrichment poller. Overridable for the
# same reason as the stamp lock below — a test must not touch the real cache.
ENRICH_CACHE_DIR="${OG_ENRICH_CACHE_DIR:-/tmp/og-pr}"
# Suffix for the cache dir's per-server state: the tick gates' stamps and the
# pending-checks markers. The dir is a bare /tmp path shared by every tmux server
# on the machine, but a pass enriches only the windows of the server that
# launched it — so an unsuffixed stamp lets any other server's tick (a scratch
# or test server) consume the gate and starve the real one (#705). $TMUX is set
# by run-shell, so this costs no fork; empty outside tmux, where nothing ticks.
ENRICH_SRV=""
if [[ -n ${TMUX:-} ]]; then
	ENRICH_SRV=".srv${TMUX%%,*}"
	ENRICH_SRV=${ENRICH_SRV//[^A-Za-z0-9._-]/_}
fi
# Per-window stamp lock (#137 conflict safety). Overridable so tests don't share
# a real machine's /tmp dir across concurrent bats runs.
ENRICH_STAMP_LOCK_DIR="${OG_ENRICH_LOCK_DIR:-/tmp/og-enrich-lock}"

# Enrich glyphs (substituted at Nix build time from enrichIconSetRaw). Text-only;
# the status template applies color and appends process/claude icons separately.
ENRICH_ICON_LINEAR="@enrich_icon_linear@"
ENRICH_ICON_GITHUB="@enrich_icon_github@"
ENRICH_ICON_PENDING="@enrich_icon_pending@"
ENRICH_ICON_SUCCESS="@enrich_icon_success@"
ENRICH_ICON_FAILURE="@enrich_icon_failure@"
ENRICH_ICON_MERGED="@enrich_icon_merged@"
ENRICH_ICON_CLOSED="@enrich_icon_closed@"
ENRICH_ICON_CONFLICT="@enrich_icon_conflict@"
ENRICH_ICON_DRAFT="@enrich_icon_draft@"

# The pending glyph's progress variant, filled by share of checks finished. Not a
# user icon key: nf-md-circle_slice_1…8, the same frames as CLAUDE_SPINNER_FRAMES.
ENRICH_PIE_GLYPHS=("󰪞" "󰪟" "󰪠" "󰪡" "󰪢" "󰪣" "󰪤" "󰪥")

# branch_to_linear_key BRANCH
# Extracts a Linear issue key (TEAM-123) from a branch name.
# Requires letters before the dash (pure-numeric prefixes are GitHub issues).
# Assumes the repo's type/id-desc branch convention (CLAUDE.md): a slashless
# branch like 'fix-208-x' is intentionally treated as Linear key FIX-208.
# Sets REPLY to the uppercased key, or empty if no match.
branch_to_linear_key() {
	local branch="$1"
	REPLY=""
	# Take the last path segment, then match <letters>-<digits> at its start.
	local slug="${branch##*/}"
	if [[ $slug =~ ^([A-Za-z]+-[0-9]+) ]]; then
		REPLY="${BASH_REMATCH[1]^^}"
	fi
}

# branch_to_gh_issue_number BRANCH
# Extracts a GitHub issue number from a branch name. Matches:
#   ^<digits>-         247-fix-bug
#   /<digits>-         feature/247-foo
#   ^gh-<digits>       gh-247
#   ^issue-<digits>    issue-247
# Sets REPLY to the number, or empty if no match.
branch_to_gh_issue_number() {
	local branch="$1"
	REPLY=""
	local slug="${branch##*/}"
	if [[ $slug =~ ^(gh|issue)-([0-9]+) ]]; then
		REPLY="${BASH_REMATCH[2]}"
	elif [[ $slug =~ ^([0-9]+)- ]]; then
		REPLY="${BASH_REMATCH[1]}"
	fi
}

# sanitize_title RAW
# Strips CR, LF, ESC control chars plus ' and # (titles ride single-quoted
# #() argv in status-format[0]; ' breaks the shell quoting, # is a tmux format
# char), then hard-truncates to 50 chars. Sets REPLY to the cleaned title.
sanitize_title() {
	local clean="${1//$'\r'/}"
	clean="${clean//$'\n'/}"
	clean="${clean//$'\033'/}"
	clean="${clean//\'/}"
	clean="${clean//\#/}"
	REPLY="${clean:0:50}"
}

# truncate_ellipsis STR MAX
# If STR exceeds MAX display chars, truncate to MAX-1 and append "…".
# Sets REPLY to the (possibly shortened) string.
truncate_ellipsis() {
	local str="$1" max="$2"
	if ((${#str} > max)); then
		REPLY="${str:0:max-1}…"
	else
		REPLY="$str"
	fi
}

# branch_sha1 BRANCH
# Computes a stable cache key (sha1 hex) for a branch name.
# Sets REPLY to the 40-char hex digest.
branch_sha1() {
	local out
	out="$(printf '%s' "$1" | sha1sum)"
	REPLY="${out%% *}"
}

# collapse_check_rollup ROLLUP_JSON
# Maps a gh `statusCheckRollup` array (a CheckRun | StatusContext union) to a
# single state. CheckRun entries carry .status + .conclusion; StatusContext
# entries (Travis/CircleCI-v1/commit-status API) carry .state instead.
# Priority: empty → none;
#   any FAILURE/ERROR/CANCELLED/TIMED_OUT/ACTION_REQUIRED/STALE → failure;
#   else any IN_PROGRESS/QUEUED/PENDING/EXPECTED, or an unfinished CheckRun
#     (empty conclusion) → pending;
#   else success.
# Sets REPLY to one of: failure | pending | success | none, and REPLY_PROGRESS
# to "<finished>/<total>" when REPLY is pending ("" otherwise). One jq fork.
collapse_check_rollup() {
	local state="" progress=""
	{
		IFS= read -r state
		IFS= read -r progress
	} < <(jq -r '
		def pending:
			((.status // "") | ascii_upcase | (. == "IN_PROGRESS" or . == "QUEUED" or . == "PENDING"))
			or ((.state // "") | ascii_upcase | (. == "EXPECTED" or . == "PENDING"))
			or (.__typename == "CheckRun" and ((.conclusion // "") == ""));
		(if length == 0 then "none"
		elif any(.[]; (.conclusion // .state // "") | ascii_upcase
			| . == "FAILURE" or . == "ERROR" or . == "CANCELLED"
			or . == "TIMED_OUT" or . == "ACTION_REQUIRED" or . == "STALE") then "failure"
		elif any(.[]; pending) then "pending"
		else "success"
		end),
		"\([.[] | select(pending | not)] | length)/\(length)"
	' <<<"$1" 2>/dev/null) || true
	REPLY="${state:-none}"
	REPLY_PROGRESS=""
	if [[ $REPLY == pending ]]; then REPLY_PROGRESS="$progress"; fi
}

# pr_pie_glyph PROGRESS
# "<finished>/<total>" → the ENRICH_PIE_GLYPHS slice filled to the share
# finished. Sets REPLY, or "" when PROGRESS is not a usable count.
pr_pie_glyph() {
	REPLY=""
	[[ $1 =~ ^([0-9]+)/([0-9]+)$ ]] || return 0
	local finished=${BASH_REMATCH[1]} total=${BASH_REMATCH[2]}
	((total > 0 && finished <= total)) || return 0
	REPLY="${ENRICH_PIE_GLYPHS[finished * 7 / total]}"
}

# split_pr_badge BADGE
# Splits a REPLY_PR badge (" <glyph> #<n>") at its number so a renderer can style
# the halves apart. Sets REPLY_GLYPH (" <glyph> ", trailing space included) and
# REPLY_NUM ("#<n>"); both "" for an empty BADGE.
split_pr_badge() {
	REPLY_GLYPH=""
	REPLY_NUM=""
	[[ -n $1 ]] || return 0
	REPLY_GLYPH="${1%#*}"
	REPLY_NUM="#${1##*#}"
}

# pr_cache_decision FORCE CACHE_EXISTS CACHE_CONTENT AGE TTL TTL_NONE [TTL_TERMINAL]
# Decides whether the PR poller may serve a cached result or must hit gh.
# An empty-array ("[]") cache is "no PR", which expires on TTL_NONE (the
# none→PR transition is what a user waits on after `gh pr create`); a
# merged/closed PR is terminal and expires on TTL_TERMINAL (defaults to TTL;
# the substring match is safe because gh emits minified JSON and any quote
# inside a title is escaped); a real open PR uses TTL. --force and a missing
# cache always fetch. Sets REPLY to "serve" or "fetch".
pr_cache_decision() {
	local force="$1" exists="$2" content="$3" age="$4" ttl="$5" ttl_none="$6" ttl_terminal="${7:-$5}"
	if ((force != 0)) || ((exists == 0)); then
		REPLY="fetch"
		return
	fi
	if [[ $content == "[]" ]]; then
		ttl=$ttl_none
	elif [[ $content == *'"state":"MERGED"'* || $content == *'"state":"CLOSED"'* ]]; then
		ttl=$ttl_terminal
	fi
	if ((age < ttl)); then
		REPLY="serve"
	else
		REPLY="fetch"
	fi
}

# provider_priority_list
# Returns the configured issue-tracker providers in priority order.
# The @providers@ placeholder is substituted at Nix build time from
# programs.tmux-og.enrich.providers. Sets REPLY to a space-separated list.
provider_priority_list() {
	REPLY="@providers@"
}

# parse_explicit_issue_id ID
# Splits an explicit issue id — the same GH-<number> / bare-key convention the
# issue-tracking skill already uses for `issue add <ID>` — into a provider and
# its provider-local id, for tmux-issue-stamp's explicit-id mode (bypasses
# branch-regex derivation when the id is already known, e.g. right after
# `gh issue create`). GH-<number> (case-insensitive prefix) is GitHub;
# anything else is treated as a Linear key and upper-cased.
# REPLY_LOCAL is validated (numeric for github, LETTERS-DIGITS for linear) and
# left empty on a malformed id (e.g. "GH-" with no digits, or "-URL") — a
# provider script would otherwise pass an unvalidated value straight to
# `gh`/`linear` as a positional arg, where a leading "-" risks being read as a
# flag. The caller must treat an empty REPLY_LOCAL as "no id", not silently
# fall back to branch derivation (that would violate explicit-id mode's
# contract of never deriving from the branch).
# Sets REPLY_PROVIDER (github|linear) and REPLY_LOCAL (bare number for github,
# uppercased key for linear; empty if malformed).
parse_explicit_issue_id() {
	local raw="$1"
	REPLY_LOCAL=""
	case "$raw" in
	[Gg][Hh]-*)
		REPLY_PROVIDER="github"
		local num="${raw#*-}"
		[[ $num =~ ^[0-9]+$ ]] && REPLY_LOCAL="$num"
		;;
	*)
		REPLY_PROVIDER="linear"
		local key="${raw^^}"
		[[ $key =~ ^[A-Z]+-[0-9]+$ ]] && REPLY_LOCAL="$key"
		;;
	esac
	# A failed match above must not leak as this function's own exit status —
	# callers run under `set -e`-style scripts and don't wrap this call in `run`
	# or `||`; a malformed id is a normal, handled outcome (REPLY_LOCAL empty),
	# not a function failure.
	return 0
}

# build_window_label MODE PROVIDER ISSUE_ID ISSUE_TITLE PR_NUMBER PR_STATE \
#                    PR_CHECK_STATE BRANCH PANE_PATH [PR_MERGEABLE] [TASK] \
#                    [AI_NAME] [PR_DRAFT] [PR_PROGRESS]
# MODE is "short" or "long". Composes the text-only window label (no color, no
# process/claude icons — the status template adds those). The issue id is taken
# from a stamped @issue_id or, if absent, derived from the branch (provider
# priority); the branch remainder after the id is the fallback title. Issue
# windows show "<provider> <id>[ <title>]"; feature branches with no issue show
# the branch (long=full, short=basename); default-branch (main/master) and
# branch-less windows show AI_NAME (a title Claude set for itself, when present),
# else TASK ("what Claude is doing", self-reported), else the directory
# basename — so multiple Claudes in one checkout stay distinct.
#
# The PR indicator is NOT folded into the name. It is returned as its own
# segment so the status template can color just the PR by check state and keep
# window_name / pickers free of color codes. The name itself is also split into
# a bold-able identity prefix and a remainder.
#
# Sets:
#   REPLY      full plain name (no PR); always REPLY_ID + REPLY_REST
#   REPLY_ID   bold-able identity prefix ("<provider> <id>") or "" (plain branch)
#   REPLY_REST remainder after the id (title / branch / dir basename)
#   REPLY_PR   plain PR segment " <glyph> #<num>" or "" when there is no PR
build_window_label() {
	local mode="$1" provider="$2" issue_id="$3" issue_title="$4"
	local pr_number="$5" pr_state="$6" pr_check="$7" branch="$8" pane_path="$9"
	local pr_mergeable="${10:-}" task="${11:-}" ai_name="${12:-}" pr_draft="${13:-}" pr_progress="${14:-}"
	local provider_icon pr_glyph=""
	REPLY=""
	REPLY_ID=""
	REPLY_REST=""
	REPLY_PR=""

	# Resolve issue identity: a stamped @issue_id wins; otherwise derive it from
	# the branch (so issue windows get the compact id + special format even before
	# or without issue-stamp). The branch remainder after the id serves as the
	# title when no stamped title exists.
	local rid="$issue_id" rprov="$provider" rtitle="$issue_title"
	if [[ -z $rid && -n $branch ]]; then
		local provs p
		provider_priority_list
		provs="$REPLY"
		for p in $provs; do
			if [[ $p == "linear" ]]; then
				branch_to_linear_key "$branch"
				if [[ -n $REPLY ]]; then
					rid="$REPLY"
					rprov="linear"
					break
				fi
			elif [[ $p == "github" ]]; then
				branch_to_gh_issue_number "$branch"
				if [[ -n $REPLY ]]; then
					rid="$REPLY"
					rprov="github"
					break
				fi
			fi
		done
	fi
	# Title fallback: branch slug remainder. Applies to stamped ids too — the
	# stamp writes an id with no title when the provider CLI is absent, and
	# that must not render less detail than an unstamped window would.
	if [[ -n $rid && -z $rtitle && -n $branch ]]; then
		local slug="${branch##*/}"
		if [[ $slug =~ ^([A-Za-z]+-[0-9]+|gh-[0-9]+|issue-[0-9]+|[0-9]+)-(.+)$ ]]; then
			rtitle="${BASH_REMATCH[2]}"
		fi
	fi

	# PR indicator (glyph + number) is independent of issue detection — a branch
	# can have a PR with no tracked issue. The number lets you locate a window's
	# PR at a glance. Returned as its own segment (REPLY_PR), never fused into
	# the name, so the template can color it by check state in isolation.
	if [[ -n $pr_number && $pr_number != "none" ]]; then
		# Terminal states are historical: a leftover pending/failed rollup must
		# not mask them, so merged/closed glyphs win over check state. A closed
		# (non-merged) PR is dead/superseded — distinct dim glyph, never a live
		# check. Then conflicts trump check state — a conflicting PR needs a
		# rebase, which re-runs checks anyway (open PRs only).
		if [[ $pr_state == "merged" ]]; then
			pr_glyph="$ENRICH_ICON_MERGED"
		elif [[ $pr_state == "closed" ]]; then
			pr_glyph="$ENRICH_ICON_CLOSED"
		elif [[ $pr_mergeable == "conflicting" ]]; then
			pr_glyph="$ENRICH_ICON_CONFLICT"
		else
			case "$pr_check" in
			failure) pr_glyph="$ENRICH_ICON_FAILURE" ;;
			pending)
				pr_pie_glyph "$pr_progress"
				pr_glyph="${REPLY:-$ENRICH_ICON_PENDING}"
				;;
			*) pr_glyph="$ENRICH_ICON_SUCCESS" ;;
			esac
		fi
		# Draft is orthogonal to check state — a draft PR still runs CI — so it
		# prepends its own glyph instead of winning the state glyph. gh clears
		# isDraft on merge, and on a closed PR the closed glyph already says
		# "dead", so terminal states carry no draft marker.
		if [[ $pr_draft == 1 && $pr_state != "merged" && $pr_state != "closed" ]]; then
			pr_glyph="${ENRICH_ICON_DRAFT} ${pr_glyph}"
		fi
		REPLY_PR=" ${pr_glyph} #${pr_number}"
	fi

	if [[ -n $rid ]]; then
		if [[ $rprov == "linear" ]]; then
			provider_icon="$ENRICH_ICON_LINEAR"
		else
			provider_icon="$ENRICH_ICON_GITHUB"
		fi
		REPLY_ID="${provider_icon} ${rid}"
		if [[ $mode == "long" && -n $rtitle ]]; then
			REPLY_REST=" ${rtitle}"
		fi
	elif [[ -n $branch && $branch != "main" && $branch != "master" ]]; then
		if [[ $mode == "long" ]]; then
			REPLY_REST="${branch}"
		else
			REPLY_REST="${branch##*/}"
		fi
	elif [[ -n $ai_name ]]; then
		# Default-branch / branch-less window: the title Claude set for itself
		# (claude-status-update name set) reads better than the raw prompt below.
		REPLY_REST="$ai_name"
	elif [[ -n $task ]]; then
		# No AI title yet — show the raw self-reported task so sibling windows in
		# one checkout don't all read alike while the title generates.
		REPLY_REST="$task"
	else
		# Last resort: the repo dir basename. A bare "main" labels every
		# default-branch window identically across repos.
		REPLY_REST="${pane_path##*/}"
	fi

	REPLY="${REPLY_ID}${REPLY_REST}"
}
