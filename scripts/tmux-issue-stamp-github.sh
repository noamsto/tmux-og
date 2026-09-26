#!/usr/bin/env bash
# GitHub Issues provider for tmux-issue-stamp.
# Usage: tmux-issue-stamp-github <worktree-path> <branch> [<explicit-number>]
# <explicit-number> (optional): skip branch-regex derivation and resolve this
# issue/PR number directly (explicit-id mode — see tmux-issue-stamp).
# Output: four lines on stdout — id, title, url, error (empty for unset
# fields).
# Errors → empty output, exit 0.
set -uo pipefail

# shellcheck source=/dev/null
source @lib_enrich@

worktree="${1:-}"
branch="${2:-}"
explicit_num="${3:-}"
id="" title="" url="" raw_err=""

# Only a github.com origin qualifies.
if [[ -d $worktree ]]; then
	origin="$(git -C "$worktree" remote get-url origin 2>/dev/null)" || origin=""
else
	origin=""
fi
if [[ $origin != *github.com* ]]; then
	printf '\n\n\n\n'
	exit 0
fi

if [[ -n $explicit_num ]]; then
	num="$explicit_num"
else
	branch_to_gh_issue_number "$branch"
	num="$REPLY"
	if [[ -z $num ]]; then
		printf '\n\n\n\n'
		exit 0
	fi
fi
id="#$num"

# Fetch title + url via gh from within the worktree (repo context). Bounded so
# a network stall can't hold tmux-issue-stamp's per-window lock long enough for
# a concurrent trigger to see it as stale and steal it mid-fetch.
if command -v gh >/dev/null 2>&1; then
	errf="$(mktemp)"
	trap 'rm -f "$errf"' EXIT
	json="$(cd "$worktree" && timeout 15 gh issue view "$num" --json number,title,url 2>"$errf")" || json=""
	raw_err="$(head -n1 "$errf" 2>/dev/null)"
	rm -f "$errf"
	trap - EXIT
	if [[ -n $json ]]; then
		title="$(jq -r '.title // ""' <<<"$json" 2>/dev/null)" || title=""
		url="$(jq -r '.url // ""' <<<"$json" 2>/dev/null)" || url=""
	fi
fi

if [[ -n $title ]]; then
	sanitize_title "$title"
	title="$REPLY"
fi

err=""
if [[ -z $title && -z $url && -n $raw_err ]]; then
	sanitize_stamp_error "$raw_err"
	err="$REPLY"
fi

printf '%s\n%s\n%s\n%s\n' "$id" "$title" "$url" "$err"
exit 0
