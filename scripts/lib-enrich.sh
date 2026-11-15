#!/usr/bin/env bash
# Shared issue/PR enrichment utilities for tmux scripts.
# Sourced (not executed) — provides constants and functions.
# Functions use the REPLY convention (set REPLY instead of echoing) to avoid
# subshell forks, matching lib-icons.sh / lib-claude.sh.

# shellcheck disable=SC2034  # used by scripts that source this library

# PR-state cache dir: the Go poller's default and tmux-issue-stamp's backfill
# tick dir. Overridable so a test must not touch the real cache.
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

# Provider glyphs (substituted at Nix build time from enrichIconSetRaw). Text-only;
# the status template applies color and appends process/claude icons separately.
ENRICH_ICON_LINEAR="@enrich_icon_linear@"
ENRICH_ICON_GITHUB="@enrich_icon_github@"

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
# Strips CR, LF, ESC control chars plus ', # and | (tmux format chars / delimiter),
# then hard-truncates to 256 chars, GitHub's title limit. Sets REPLY to the cleaned
# title.
sanitize_title() {
	local clean="${1//$'\r'/}"
	clean="${clean//$'\n'/}"
	clean="${clean//$'\033'/}"
	clean="${clean//\'/}"
	clean="${clean//\#/}"
	clean="${clean//|/ }"
	REPLY="${clean:0:256}"
}

# sanitize_stamp_error RAW
# Strips ANSI CSI sequences and control chars from a captured stderr line,
# redacts token-shaped substrings (this is a third-party CLI's stderr, which
# on a verbose auth-failure path could echo back a bearer token, an
# Authorization header, credentials embedded in a URL, or a token/api_key/
# secret query-string param), then clamps to 120 chars. Sets REPLY to the
# cleaned text. Best-effort, not exhaustive: an unlabeled opaque secret under
# 20 chars, in a shape none of these rules names, can still survive.
sanitize_stamp_error() {
	local clean
	clean="$(printf '%s' "$1" | sed -E $'s/\x1b\\[[0-9;]*[a-zA-Z]//g')"
	# Labeled forms first (keep the label, drop the value), then a generic
	# catch-all for any other long opaque token. `Bearer` runs before
	# `Authorization:` — an "Authorization: Bearer <token>" line otherwise
	# has its value-capture eat only the literal word "Bearer", leaving the
	# real token for a now-unmatchable Bearer rule.
	clean="$(printf '%s' "$clean" | sed -E \
		-e 's/[Bb]earer[[:space:]]+[^[:space:]]+/Bearer [redacted]/g' \
		-e 's/([Aa]uthorization:[[:space:]]*)[^[:space:]]+/\1[redacted]/g' \
		-e 's#://[^/[:space:]@]+:[^/[:space:]@]+@#://[redacted]@#g' \
		-e 's/([?&])(token|api[_-]?key|access[_-]?token|secret)=[^&[:space:]]+/\1\2=[redacted]/gI' \
		-e 's/\b[A-Za-z0-9_-]{20,}\b/[redacted]/g')"
	clean="${clean//[[:cntrl:]]/}"
	REPLY="${clean:0:120}"
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

# build_window_label MODE PROVIDER ISSUE_ID ISSUE_TITLE PR_NUMBER PR_GLYPH \
#                    BRANCH PANE_PATH [TASK] [AI_NAME]
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
# window_name / pickers free of color codes. PR_GLYPH is composed once by the Go
# poller (enrichstate.Glyph) and arrives as @pr_glyph; it is rendered verbatim.
# The name itself is also split into a bold-able identity prefix and a remainder.
#
# Sets:
#   REPLY      full plain name (no PR); always REPLY_ID + REPLY_REST
#   REPLY_ID   bold-able identity prefix ("<provider> <id>") or "" (plain branch)
#   REPLY_REST remainder after the id (title / branch / dir basename)
#   REPLY_PR   plain PR segment " <glyph> #<num>" or "" when there is no PR
build_window_label() {
	local mode="$1" provider="$2" issue_id="$3" issue_title="$4"
	local pr_number="$5" pr_glyph="$6" branch="$7" pane_path="$8"
	local task="${9:-}" ai_name="${10:-}"
	local provider_icon
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
		REPLY_PR=" ${pr_glyph:+$pr_glyph }#${pr_number}"
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
