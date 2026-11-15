#!/usr/bin/env bash
# Tag a window as a worktree window — @worktree/@branch/@git_root, then @issue_*
# via the existing stamp. Idempotent, navigation-free, CLAUDECODE-agnostic. The
# single place that defines HOW a window gets tagged: fired at window creation
# (after-new-window/after-new-session) so any window — whoever created it (a raw
# `tmux new-window`, a dispatcher, a tmux-remux restore) — becomes a first-class
# worktree window, and reused by the worktrunk post-switch hook for navigation.
#
# Usage:
#   tmux-reconcile-window <target>               # cwd-derived (creation hooks)
#   tmux-reconcile-window <target> --cwd-move     # cwd-derived (#596 move detector)
#   tmux-reconcile-window <target> <worktree> <branch>  # explicit (post-switch nav)
# <target> is any tmux target; the creation hooks pass #{window_id} (globally
# unique), which sidesteps both numeric-session ambiguity and $-reexpansion.
# Explicit mode trusts its caller to name a worktree a pane is in, or moving to.
# --cwd-move marks a call from tmux-update-icons's cwd-move detector rather than
# a creation hook — see the forced-reflow gate below for why that distinction
# can't be recovered from @worktree state alone.
set -uo pipefail

target="${1:-}"
[[ -z $target ]] && exit 0

# Remote-bridge mirror window (#167 @bridge_win opt-out): never tag it as a
# worktree window — its cwd is the launcher's repo, not the remote content.
[[ $(tmux show-options -t "$target" -wqv @bridge_win 2>/dev/null) == 1 ]] && exit 0

# Empty when enrich is disabled (Nix build-time substitution).
issue_stamp="@issue_stamp@"

cwd_move=0
if [[ -n ${2:-} && -n ${3:-} ]]; then
	# Explicit mode: the caller (worktrunk) knows the worktree/branch
	# authoritatively. Avoids reading pane_current_path, which lags behind the
	# async send-keys `cd` the post-switch take-over/match branches issue.
	top="$2"
	br="$3"
	root="$2"
else
	[[ ${2:-} == "--cwd-move" ]] && cwd_move=1
	# cwd mode: derive from the target window's active pane. Read the path PINNED
	# to the target — a bare #{pane_current_path} in a hook resolves against the
	# attached client's active window, not the just-created one.
	cwd=$(tmux display-message -t "$target" -p '#{pane_current_path}' 2>/dev/null) || exit 0
	[[ -z $cwd ]] && exit 0
	git -C "$cwd" rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0
	top=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null) || exit 0
	[[ -z $top ]] && exit 0
	# Real branch name (matches tmux-update-icons / tmux-branch-display); empty on
	# a detached HEAD, which is fine — tags still set, stamp self-bails below.
	br=$(git -C "$cwd" branch --show-current 2>/dev/null) || br=""
	root="$top"
fi

# Idempotent: skip the writes (and the redundant issue-stamp) when nothing changed.
cur_wt=$(tmux show-options -t "$target" -wqv @worktree 2>/dev/null)
cur_br=$(tmux show-options -t "$target" -wqv @branch 2>/dev/null)
[[ $cur_wt == "$top" && $cur_br == "$br" ]] && exit 0

tmux set-option -t "$target" -w @worktree "$top"
tmux set-option -t "$target" -w @git_root "$root"
if [[ -n $br ]]; then
	tmux set-option -t "$target" -w @branch "$br"
elif [[ -n $cur_wt ]]; then
	# Re-tag onto a detached HEAD: otherwise the previous repository's branch
	# stays in place beside the new @worktree, and tmux-pr-enrich groups the
	# window under the new @worktree but queries the OLD repo's branch name
	# inside it.
	tmux set-option -t "$target" -wu @branch 2>/dev/null
fi

if [[ -n $cur_wt && $cur_wt != "$top" ]]; then
	# @worktree actually changed on a re-tag: only a successful fetch
	# overwrites these, and nothing reads @pr_branch, so the old repo's PR
	# badge/URL would otherwise survive the move. Clearing @pr_state and
	# @pr_check_state specifically also keeps notify_pr_change from firing a
	# false "PR merged"/"checks failed" toast on the next fetch, since it reads
	# an empty prior field as discovery.
	tmux set-option -t "$target" -wu @pr_number 2>/dev/null
	tmux set-option -t "$target" -wu @pr_title 2>/dev/null
	tmux set-option -t "$target" -wu @pr_state 2>/dev/null
	tmux set-option -t "$target" -wu @pr_check_state 2>/dev/null
	tmux set-option -t "$target" -wu @pr_url 2>/dev/null
	tmux set-option -t "$target" -wu @pr_mergeable 2>/dev/null
	tmux set-option -t "$target" -wu @pr_draft 2>/dev/null
	tmux set-option -t "$target" -wu @pr_review 2>/dev/null
	tmux set-option -t "$target" -wu @pr_auto_merge 2>/dev/null
	tmux set-option -t "$target" -wu @pr_check_progress 2>/dev/null
	tmux set-option -t "$target" -wu @pr_glyph 2>/dev/null
	tmux set-option -t "$target" -wu @pr_branch 2>/dev/null
fi

# Derive @issue_* from the branch (enrich only; placeholder empty when disabled).
if [[ -n $issue_stamp && -n $br ]]; then
	"$issue_stamp" "$target" "$top" "$br" >/dev/null 2>&1 &
	disown
fi

if [[ $cwd_move -eq 1 ]]; then
	# Force a reflow only for a call from the #596 cwd-move detector, never a
	# bare creation-hook call: window creation is excluded (the after-new-window
	# hook already reflows) and explicit mode is excluded (worktrunk's
	# post-switch already reflows too, so every `wt switch` would otherwise pay
	# an extra forced reflow). `-n $cur_wt` used to stand in for "this is a
	# re-tag, not creation", but a window untagged since creation (no @worktree
	# at all — e.g. it started in a non-git cwd) hits this same first-time-tag
	# shape on its first successful move into a repo, and that call is
	# indistinguishable from creation by @worktree state alone (#605) — hence a
	# caller-supplied flag instead. Needed at all because this otherwise relies
	# on tmux-issue-stamp to reflow, and that only runs when enrich is enabled
	# AND the branch is non-empty.
	@reflow@ "$(tmux display-message -t "$target" -p '#{session_name}')" --force >/dev/null 2>&1 &
	disown
fi

exit 0
