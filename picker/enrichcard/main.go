package main

import (
	"flag"
	"os"

	tea "charm.land/bubbletea/v2"
)

func main() {
	var c cfg
	flag.StringVar(&c.target, "target", "", "session_id:window_id to inspect")
	flag.StringVar(&c.prEnrichBin, "pr-enrich-bin", "tmux-pr-enrich", "path to the PR poller binary")
	flag.StringVar(&c.bridgeCtlBin, "bridge-ctl-bin", "", "path to og-remote-bridge-ctl, for [r] in a mirror")
	flag.StringVar(&c.bridgeSock, "bridge-sock", "", "bridge daemon socket, for [r] in a mirror")
	flag.StringVar(&c.bridgePane, "bridge-pane", "", "remote pane id, for [r] in a mirror")
	flag.StringVar(&c.issueStampBin, "issue-stamp-bin", "tmux-issue-stamp", "path to the issue-identity stamp binary")
	flag.StringVar(&c.fg, "thm-fg", "", "")
	flag.StringVar(&c.mauve, "thm-mauve", "", "")
	flag.StringVar(&c.red, "thm-red", "", "")
	flag.StringVar(&c.green, "thm-green", "", "")
	flag.StringVar(&c.peach, "thm-peach", "", "")
	flag.StringVar(&c.blue, "thm-blue", "", "")
	flag.StringVar(&c.overlay0, "thm-overlay0", "", "")
	flag.StringVar(&c.subtext0, "thm-subtext0", "", "")
	flag.StringVar(&c.icLinear, "icon-linear", "", "")
	flag.StringVar(&c.icGitHub, "icon-github", "", "")
	flag.StringVar(&c.icPending, "icon-pending", "", "")
	flag.StringVar(&c.icSuccess, "icon-success", "", "")
	flag.StringVar(&c.icFailure, "icon-failure", "", "")
	flag.StringVar(&c.icMerged, "icon-merged", "", "")
	flag.StringVar(&c.icClosed, "icon-closed", "", "")
	flag.StringVar(&c.icConflict, "icon-conflict", "", "")
	flag.StringVar(&c.icDraft, "icon-draft", "", "")
	flag.Parse()
	c = lightCfg(c, readGlobalOptions())

	opts := readWindowState(c.target)
	w := resolve(opts)
	dir := w.worktree
	if dir == "" {
		dir = w.gitRoot
	}
	// detectBaseBranch shells `git -C <dir>` against dir, which in a mirror
	// is a path on the remote — the same path can exist locally as an
	// unrelated repo, so skip it rather than risk a base branch read from
	// the wrong checkout (design D3/I5).
	var base string
	if !opts.mirror {
		base = detectBaseBranch(dir)
	}
	m := model{cfg: c, win: w, mirror: opts.mirror, baseBranch: base}
	if _, err := tea.NewProgram(m).Run(); err != nil {
		os.Exit(1)
	}
}
