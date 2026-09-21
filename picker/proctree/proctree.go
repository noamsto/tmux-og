// Package proctree walks a ps process table into per-root CPU/memory totals.
// It is the one place the tree walk lives: the picker's local leg, its ssh
// fallback and the remote session-resource poller all feed it a `ps PSArgs`
// table.
package proctree

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/noamsto/tmux-og/picker/agentdetect/manifest"
)

// PSArgs is the process table every leg reads. -A (POSIX all-processes), not
// -e: BSD ps on macOS reads -e as "show environment". No --no-headers either:
// it's GNU-only and errors on BSD ps — the header row it leaves behind is
// skipped by Aggregate, where "PID" parses to 0. The trailing comm column is
// what lets an agent be found by its process tree rather than by its pane's
// foreground command; a table without it (an older remote, a test fixture)
// simply reports no tree agents.
var PSArgs = []string{"-Ao", "pid,ppid,pcpu,rss,comm"}

// Totals holds aggregated CPU and memory for a session.
type Totals struct {
	CPUPct float64
	MemMB  float64
	// AgentCmds are the agent command names found anywhere in the session's
	// process tree, deduped and sorted. pane_current_command names a pane's
	// process-group leader, so an agent a shell chain relaunched — tmux-remux's
	// restore runs `cat-scrollback …; <agent>; exec <shell>` under one
	// non-interactive shell, which has no job control, so the agent shares the
	// shell's group — is invisible there and only the tree shows it.
	AgentCmds []string
}

// agentCommands is the set of process names the agent manifests match — the
// same list @AGENT_COMMANDS compiles into the shell scripts and agent-detect
// reads, so the three cannot diverge. Loaded once from the embedded manifests;
// a load failure yields an empty set, which degrades to pane-command-only
// detection.
var agentCommands = sync.OnceValue(func() map[string]bool {
	manifests, err := manifest.Load()
	if err != nil {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(manifests))
	for _, m := range manifests {
		for _, c := range m.MatchCommands {
			set[c] = true
		}
	}
	return set
})

var wrappedRe = regexp.MustCompile(`^\.(.*)-wrapped$`)

// procName reduces a ps comm field to the name the manifests and iconMap are
// keyed by: BSD ps prints the executable's full path, and a makeWrapper nix
// build names it `.foo-wrapped`.
func procName(comm string) string {
	name := filepath.Base(strings.TrimSpace(comm))
	if m := wrappedRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// Aggregate sums CPU% and RSS over each root PID's whole process tree, given
// one `ps PSArgs` table, and collects the agent commands found in it. Pure,
// and the only place the tree walk lives.
func Aggregate(rootPIDs map[string][]int, psOut string) map[string]Totals {
	children := make(map[int][]int)
	type procInfo struct {
		cpu  float64
		rss  int64 // KiB
		comm string
	}
	procs := make(map[int]*procInfo)

	for _, line := range strings.Split(strings.TrimSpace(psOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		ppid, _ := strconv.Atoi(fields[1])
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		rss, _ := strconv.ParseInt(fields[3], 10, 64)
		if pid <= 0 {
			continue
		}
		info := &procInfo{cpu: cpu, rss: rss}
		if len(fields) > 4 {
			info.comm = procName(strings.Join(fields[4:], " "))
		}
		procs[pid] = info
		children[ppid] = append(children[ppid], pid)
	}

	agents := agentCommands()
	result := make(map[string]Totals, len(rootPIDs))
	for key, pids := range rootPIDs {
		var totalCPU float64
		var totalRSS int64
		seen := make(map[string]bool)
		var found []string
		for _, root := range pids {
			queue := []int{root}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				if p, ok := procs[cur]; ok {
					totalCPU += p.cpu
					totalRSS += p.rss
					if p.comm != "" && agents[p.comm] && !seen[p.comm] {
						seen[p.comm] = true
						found = append(found, p.comm)
					}
				}
				queue = append(queue, children[cur]...)
			}
		}
		sort.Strings(found)
		result[key] = Totals{
			CPUPct:    totalCPU,
			MemMB:     float64(totalRSS) / 1024.0,
			AgentCmds: found,
		}
	}
	return result
}
