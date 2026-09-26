package main

import (
	"os/exec"
	"strings"
)

// winState is the live per-window enrichment data, read from tmux window
// options. The card reflects these — it never re-derives issue/PR/claude data.
type winState struct {
	issueProvider, issueID, issueTitle, issueURL            string
	issueExplicitID, issueStampError                        string
	prNumber, prTitle, prState, prCheck, prURL, prMergeable string
	prDraft, prReview, prAutoMerge, prProgress              string
	branch, worktree, gitRoot                               string
	task, claudeAgo, paneIcon                               string
}

// winOpts is the raw parse of one `show-options -w` read: the window's own
// (possibly stale, in a mirror) local values, the daemon-shipped bridge
// values, and whether the window is a mirror at all. resolve (bridge.go)
// turns this into the single winState the rest of the card renders.
type winOpts struct {
	local, bridge winState
	mirror        bool
}

// readWindowState runs one `tmux show-options -w -t <target>` and parses it.
// On any error (e.g. the window closed) it returns the zero winOpts; callers
// keep their last good state.
func readWindowState(target string) winOpts {
	var o winOpts
	out, err := exec.Command("tmux", "show-options", "-w", "-t", target).Output()
	if err != nil {
		return o
	}
	parseWindowOptions(string(out), &o)
	return o
}

// parseWindowOptions parses `show-options -w` lines (`@name value` or
// `@name "quoted value"`) into o; unknown options are ignored. Local and
// bridge (`@bridge_*`) names are parsed into their own winState side by
// side — see resolve in bridge.go for how they're combined.
func parseWindowOptions(out string, o *winOpts) {
	for _, line := range strings.Split(out, "\n") {
		name, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		switch name {
		case "@issue_provider":
			o.local.issueProvider = val
		case "@issue_id":
			o.local.issueID = val
		case "@issue_title":
			o.local.issueTitle = val
		case "@issue_url":
			o.local.issueURL = val
		case "@issue_explicit_id":
			o.local.issueExplicitID = val
		case "@issue_stamp_error":
			o.local.issueStampError = val
		case "@pr_number":
			o.local.prNumber = val
		case "@pr_title":
			o.local.prTitle = val
		case "@pr_state":
			o.local.prState = val
		case "@pr_check_state":
			o.local.prCheck = val
		case "@pr_url":
			o.local.prURL = val
		case "@pr_mergeable":
			o.local.prMergeable = val
		case "@pr_draft":
			o.local.prDraft = val
		case "@pr_review":
			o.local.prReview = val
		case "@pr_auto_merge":
			o.local.prAutoMerge = val
		case "@pr_check_progress":
			o.local.prProgress = val
		case "@branch":
			o.local.branch = val
		case "@worktree":
			o.local.worktree = val
		case "@git_root":
			o.local.gitRoot = val
		case "@window_task":
			o.local.task = val
		case "@window_claude_ago":
			o.local.claudeAgo = val
		case "@active_pane_icon":
			o.local.paneIcon = val
		case "@bridge_win":
			o.mirror = val == "1"
		case "@bridge_issue_provider":
			o.bridge.issueProvider = val
		case "@bridge_issue_id":
			o.bridge.issueID = val
		case "@bridge_issue_title":
			o.bridge.issueTitle = val
		case "@bridge_issue_url":
			o.bridge.issueURL = val
		case "@bridge_pr_number":
			o.bridge.prNumber = val
		case "@bridge_pr_title":
			o.bridge.prTitle = val
		case "@bridge_pr_state":
			o.bridge.prState = val
		case "@bridge_pr_check_state":
			o.bridge.prCheck = val
		case "@bridge_pr_url":
			o.bridge.prURL = val
		case "@bridge_pr_mergeable":
			o.bridge.prMergeable = val
		case "@bridge_pr_draft":
			o.bridge.prDraft = val
		case "@bridge_pr_review":
			o.bridge.prReview = val
		case "@bridge_pr_auto_merge":
			o.bridge.prAutoMerge = val
		case "@bridge_pr_check_progress":
			o.bridge.prProgress = val
		case "@bridge_branch":
			o.bridge.branch = val
		case "@bridge_dir":
			// Already resolved on the remote as worktree || git_root
			// (I2/I5) — lands in worktree so the existing
			// worktree||gitRoot fallback reads it with no special case.
			o.bridge.worktree = val
		}
	}
}

// detectBaseBranch returns the repo's default branch (e.g. "main"/"master") via
// origin/HEAD, or "" if it can't be determined. Run once at launch — the base
// doesn't change during a popup's lifetime, so it stays out of the tick.
func detectBaseBranch(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

// unquote strips a matched surrounding quote pair. tmux show-options quotes
// values needing it with double quotes (e.g. spaces) and renders an empty value
// as a pair of single quotes. Both styles must be stripped, else a cleared
// option like
//
//	@branch ''
//
// parses as that two-quote literal and defeats the empty-value fallbacks/guards.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
