// Package agentstate owns the agent priority order. Go consumers call Pick;
// the shell side runs a function generated from Order (Shell), pinned to
// scripts/lib-claude.sh by the agent-priority-drift flake check (after editing
// Order: go test -update rewrites the golden; paste it between the markers) — a forked
// Go call per window per tick would cost far more than the bash function.
package agentstate

import (
	"fmt"
	"strings"
)

// Order lists agent states from highest to lowest priority. "interrupted" is
// shell-only: it comes from a transcript-tail scrape the Go side never does.
var Order = []string{
	"error", "waiting", "denied", "compacting", "interrupted", "processing", "done", "idle",
}

// shellArgs maps each state to its positional argument in the generated
// function. The order is the historical claude_priority_state signature,
// which callers (and tests) depend on.
var shellArgs = []struct{ state, name string }{
	{"waiting", "w"}, {"compacting", "k"}, {"processing", "p"}, {"done", "d"},
	{"idle", "i"}, {"error", "e"}, {"denied", "dn"}, {"interrupted", "int"},
}

// Pick returns the highest-priority state for which has reports true, or "".
func Pick(has func(state string) bool) string {
	for _, s := range Order {
		if has(s) {
			return s
		}
	}
	return ""
}

// Shell renders the claude_priority_state bash function, shfmt-formatted.
func Shell() string {
	var b strings.Builder
	b.WriteString("claude_priority_state() {\n")
	b.WriteString("\tlocal w=$1 k=$2 p=$3 d=$4 i=$5 e=${6:-0} dn=${7:-0} int=${8:-0}\n")
	for n, s := range Order {
		kw := "if"
		if n > 0 {
			kw = "elif"
		}
		fmt.Fprintf(&b, "\t%s ((%s > 0)); then\n\t\tREPLY=%q\n", kw, argName(s), s)
	}
	b.WriteString("\telse\n\t\tREPLY=\"\"\n\tfi\n}\n")
	return b.String()
}

func argName(state string) string {
	for _, a := range shellArgs {
		if a.state == state {
			return a.name
		}
	}
	panic("agentstate: no shell arg for " + state)
}
