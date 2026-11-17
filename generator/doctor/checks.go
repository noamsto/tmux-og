// Command og-doctor reports which optional tools tmux-og's features depend on
// are present, locally and on each configured remote bridge host.
package main

// Result is one checked tool and whether the feature it gates is available.
type Result struct {
	Name    string
	Feature string
	OK      bool
	Detail  string // empty when OK; else a short reason ("not found", "no answer", ...)
}

// checkItem pairs a binary name with the short, human-readable feature it
// gates, e.g. "carousel graphics" for resvg.
type checkItem struct {
	Name    string
	Feature string
}

// localChecklist is probed via exec.LookPath against this host's PATH.
var localChecklist = []checkItem{
	{"tmux", "core"},
	{"ssh", "remote bridge (host connections)"},
	{"gh", "PR/issue enrichment"},
	{"resvg", "carousel graphics rendering"},
	{"zoxide", "picker directory suggestions"},
	{"jq", "JSON parsing (usage/status data)"},
	{"curl", "agent usage limit polling"},
	{"chafa", "splash art generation"},
	{"socat", "remote bridge transport"},
	{"btop", "resource monitoring tool bind"},
	{"sesh", "session connect across mirrors"},
	{"lazygit", "tool bind (prefix+g)"},
	{"yazi", "tool bind (prefix+y)"},
	{"prdash", "tool bind (prefix+p)"},
	{"crew", "crew dash float (prefix+H)"},
}

// remoteChecklist is probed on each configured remote host's own PATH via ssh.
var remoteChecklist = []checkItem{
	{"tmux-claude-images", "bridge graphics (prefix+I)"},
	{"resvg", "carousel graphics rendering"},
	{"claude-status-update", "remote agent status"},
	{"tmux-reflow-windows", "remote window labels"},
	{"og-remote-picker", "remote-side picker (prefix+s ^o)"},
	{"tmux-pr-enrich", "enrich refresh (prefix+i r)"},
	{"gh", "PR/issue enrichment"},
	{"lazygit", "tool bind (prefix+g)"},
	{"yazi", "tool bind (prefix+y)"},
	{"prdash", "tool bind (prefix+p)"},
	{"theme-toggle", "theme fan-out"},
}
