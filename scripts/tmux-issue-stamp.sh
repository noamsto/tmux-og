#!/usr/bin/env bash
# One-shot issue-identity dispatcher. Runs from the worktrunk post-switch hook
# tail. Iterates configured providers in priority order; first complete
# (id) wins. Writes @issue_* window options on the target. Always exits 0.
#
# Usage: tmux-issue-stamp <target> <worktree-path> <branch> [<explicit-id>]
#   <target> is a tmux target (e.g. "$session:$window" or "$N" session id form).
#   <explicit-id> (optional) skips branch-regex derivation and resolves this id
#   directly — used by `claude-status-update enrich <ID>` right after an issue
#   or PR is created mid-session, before/without a branch encoding it.
set -uo pipefail

# shellcheck source=/dev/null
source @lib_enrich@
# shellcheck source=/dev/null
source @lib_log@

# --- backfill: self-healing retry for a window stuck with an id but no
# title/url (a transient provider failure at post-switch time; #599) ---
BACKFILL_TICK_SECONDS=30
BACKFILL_MAX_TRIES=5
# Defensive cap on candidates per sweep, mirroring tmux-pr-enrich.sh's 30-branch
# cap on a full pass — partial stamps should be rare, but nothing should fork
# unbounded provider chains off a single tick.
BACKFILL_SWEEP_CAP=20

# run_backfill_pass — scan every window for a partial stamp (id set, title or
# url still empty) and re-run the one-shot path on each, up to BACKFILL_MAX_TRIES
# per window. Title/url are read as presence-booleans (#{?#{@issue_title},1,}),
# not their text, since sanitize_title doesn't strip '|' and this format uses
# '|' as its delimiter — the sweep only needs emptiness, never the text. The
# remaining fields are still literal text and could themselves contain a '|'
# (git permits it in a branch name), same as tmux-pr-enrich.sh's -F format.
run_backfill_pass() {
	local windows
	mapfile -t windows < <(tmux list-windows -a -F \
		'#{session_id}:#{window_id}|#{@issue_id}|#{?#{@issue_title},1,}|#{?#{@issue_url},1,}|#{@issue_branch}|#{@issue_explicit_id}|#{@worktree}|#{@git_root}|#{@bridge_win}|#{@issue_backfill_tries}' \
		2>/dev/null)

	local line tgt id has_title has_url br explicit_id2 wt gr bw tries d count=0
	for line in "${windows[@]}"; do
		[[ -z $line ]] && continue
		IFS="|" read -r tgt id has_title has_url br explicit_id2 wt gr bw tries <<<"$line"
		[[ -z $id || -z $br ]] && continue
		[[ $bw == 1 ]] && continue
		[[ $has_title == 1 && $has_url == 1 ]] && continue
		[[ $tries =~ ^[0-9]+$ ]] || tries=0
		((tries >= BACKFILL_MAX_TRIES)) && continue
		((count >= BACKFILL_SWEEP_CAP)) && continue
		d="${wt:-$gr}"
		[[ -z $d || ! -d $d ]] && continue
		((++count))
		if [[ -n $explicit_id2 ]]; then
			"${BASH_SOURCE[0]}" "$tgt" "$d" "$br" "$explicit_id2" &
		else
			"${BASH_SOURCE[0]}" "$tgt" "$d" "$br" &
		fi
	done
	wait
}

if [[ ${1:-} == "--backfill" ]]; then
	mkdir -p "$ENRICH_CACHE_DIR" 2>/dev/null
	last_tick="$ENRICH_CACHE_DIR/.last-backfill-tick$ENRICH_SRV"
	if [[ -f $last_tick ]]; then
		tick_age=$((EPOCHSECONDS - $(file_mtime "$last_tick")))
		((tick_age < BACKFILL_TICK_SECONDS)) && exit 0
	fi
	touch "$last_tick"
	detach "${BASH_SOURCE[0]}" --backfill-run
	exit 0
fi

if [[ ${1:-} == "--backfill-run" ]]; then
	run_backfill_pass
	exit 0
fi

target="${1:-}"
worktree="${2:-}"
branch="${3:-}"
explicit_id="${4:-}"

[[ -z $target || -z $branch ]] && exit 0

# Remote-bridge mirror window (#167 @bridge_win opt-out): its identity is the
# remote content it mirrors, not this repo/branch — never stamp it.
[[ $(tmux show-options -t "$target" -wqv @bridge_win 2>/dev/null) == 1 ]] && exit 0

# Serialize every stamp trigger (post-switch, the auto branch-transition
# trigger in tmux-update-icons, and the explicit `enrich` subcommand) through a
# per-window lock: they can all fire around the same moment and would
# otherwise interleave writes from stale/duplicate provider calls. Keyed by
# window_id (stable across the "$session:$idx" / pane-id / explicit-mode
# target shapes callers pass). Non-blocking — a losing fire is redundant, the
# winner's write already covers it, so the 1s poller never blocks.
win_id="$(tmux display-message -t "$target" -p '#{window_id}' 2>/dev/null)" || exit 0
[[ -z $win_id ]] && exit 0
mkdir -p "$ENRICH_STAMP_LOCK_DIR" 2>/dev/null
if ! acquire_lock "$ENRICH_STAMP_LOCK_DIR/${win_id#@}.lock"; then
	# Logged (not just a silent exit): a stuck holder or an unwritable lock dir
	# would otherwise make every future stamp on this window vanish with zero
	# trace — indistinguishable from "nothing changed".
	log_enabled && log_event enrich event stamp_skip_locked win_id "$win_id"
	exit 0
fi

# A window's branch can move between the backfill sweep's list-windows scan
# and this invocation reaching the lock — providers can block up to 15s each,
# and a losing concurrent switch gets no automatic retry (#599). Trust the
# worktree's live branch over a possibly-stale argument.
if [[ -n $worktree ]]; then
	live_branch="$(git -C "$worktree" branch --show-current 2>/dev/null)" || live_branch=""
	[[ -n $live_branch ]] && branch="$live_branch"
fi

# The pane just cd'd into the worktree but update-icons' 1s tick hasn't
# refreshed @branch/@git_root yet — stamp them now so dir-display doesn't
# briefly render the worktree path relative to the parent repo's root.
if [[ -n $worktree ]]; then
	tmux set-option -t "$target" -w @branch "$branch"
	tmux set-option -t "$target" -w @git_root "$worktree"
fi

run_provider() {
	case "$1" in
	linear) @issue_stamp_linear@ "$worktree" "$branch" "${2:-}" ;;
	github) @issue_stamp_github@ "$worktree" "$branch" "${2:-}" ;;
	*) printf '\n\n\n\n' ;;
	esac
}

chosen_provider="" id="" title="" url="" err=""
if [[ -n $explicit_id ]]; then
	parse_explicit_issue_id "$explicit_id"
	# A malformed id (REPLY_LOCAL empty) must stay a hard "no id" here — calling
	# run_provider with an empty local id would make the provider script's own
	# `[[ -n $explicit_num ]]` guard read "no explicit id" and silently fall
	# back to branch derivation, breaking explicit-id mode's contract of never
	# deriving from the branch.
	if [[ -n $REPLY_LOCAL ]]; then
		chosen_provider="$REPLY_PROVIDER"
		mapfile -t out < <(run_provider "$chosen_provider" "$REPLY_LOCAL")
		id="${out[0]:-}"
		title="${out[1]:-}"
		url="${out[2]:-}"
		err="${out[3]:-}"
	fi
else
	provider_priority_list
	read -r -a providers <<<"$REPLY"
	for p in "${providers[@]}"; do
		mapfile -t out < <(run_provider "$p")
		if [[ -n ${out[0]:-} ]]; then
			chosen_provider="$p"
			id="${out[0]:-}"
			title="${out[1]:-}"
			url="${out[2]:-}"
			err="${out[3]:-}"
			break
		fi
	done
fi

if [[ -z $id ]]; then
	# No provider matched: clear any stale stamp left from a previous branch on
	# this window (a stamp is never overwritten by absence, only by presence),
	# then recompute labels so the display falls back to the branch.
	tmux set-option -t "$target" -wu @issue_provider 2>/dev/null
	tmux set-option -t "$target" -wu @issue_id 2>/dev/null
	tmux set-option -t "$target" -wu @issue_title 2>/dev/null
	tmux set-option -t "$target" -wu @issue_url 2>/dev/null
	tmux set-option -t "$target" -wu @issue_branch 2>/dev/null
	tmux set-option -t "$target" -wu @issue_backfill_tries 2>/dev/null
	tmux set-option -t "$target" -wu @issue_explicit_id 2>/dev/null
	tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null
	@reflow@ "$(tmux display-message -t "$target" -p '#{session_name}')" --force >/dev/null 2>&1 &
	log_enabled && log_event enrich event stamp_clear win_id "$win_id" sess "$(tmux display-message -t "$target" -p '#{session_name}' 2>/dev/null || true)"
	# A branch with no issue id still has a PR (the common case) — kick the same
	# immediate fetch the id-found path does below, rather than waiting on
	# tmux-pr-enrich's own full pass (up to prRefreshSeconds, 120s default).
	# Guarded on $worktree: with --dir "" the poller skips its cd and gh would
	# run in the tmux server's cwd, the wrong-repo bug this script's header calls out.
	if [[ -n $worktree ]]; then
		@pr_enrich@ --target "$target" --branch "$branch" --dir "$worktree" --force >/dev/null 2>&1 &
	fi
	disown -a
	exit 0
fi

old_branch="$(tmux show-options -t "$target" -wqv @issue_branch 2>/dev/null)"
old_explicit_id="$(tmux show-options -t "$target" -wqv @issue_explicit_id 2>/dev/null)"

tmux set-option -t "$target" -w @issue_provider "$chosen_provider"
tmux set-option -t "$target" -w @issue_id "$id"
tmux set-option -t "$target" -w @issue_title "$title"
tmux set-option -t "$target" -w @issue_url "$url"
tmux set-option -t "$target" -w @issue_branch "$branch"
log_enabled && log_event enrich event stamp provider "$chosen_provider" id "$id" title "$title" url "$url" win_id "$win_id" sess "$(tmux display-message -t "$target" -p '#{session_name}' 2>/dev/null || true)"

# Backfill bookkeeping (#599): a complete stamp clears the retry counter; a
# partial one increments it — unless the branch or explicit id changed
# underneath this window, restarting the count at attempt #1. The explicit-id
# check matters on its own: a fresh mid-session id on the same branch would
# otherwise inherit a prior id's exhausted counter and never get backfilled.
if [[ -n $title && -n $url ]]; then
	tmux set-option -t "$target" -wu @issue_backfill_tries 2>/dev/null
	tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null
else
	if [[ -n $err ]]; then
		tmux set-option -t "$target" -w @issue_stamp_error "$err"
	else
		tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null
	fi
	if [[ $old_branch == "$branch" && $old_explicit_id == "$explicit_id" ]]; then
		tries="$(tmux show-options -t "$target" -wqv @issue_backfill_tries 2>/dev/null)"
		[[ $tries =~ ^[0-9]+$ ]] || tries=0
	else
		tries=0
	fi
	tmux set-option -t "$target" -w @issue_backfill_tries "$((tries + 1))"
fi

# Persist the raw explicit id (if this was explicit-id mode) so a later
# backfill re-invocation can pass it back as arg 4 instead of falling through
# to branch derivation, which would never match an explicit-id window's real
# branch and would wipe a correct id (#599).
if [[ -n $explicit_id ]]; then
	tmux set-option -t "$target" -w @issue_explicit_id "$explicit_id"
else
	tmux set-option -t "$target" -wu @issue_explicit_id 2>/dev/null
fi

# Recompute window labels now that the issue id/title exist (cache bypass).
@reflow@ "$(tmux display-message -t "$target" -p '#{session_name}')" --force >/dev/null 2>&1 &

# Kick an immediate PR fetch for this branch (likely "none" for a fresh branch).
@pr_enrich@ --target "$target" --branch "$branch" --dir "$worktree" --force >/dev/null 2>&1 &
disown -a

exit 0
