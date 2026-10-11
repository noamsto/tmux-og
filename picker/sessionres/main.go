// Command tmux-session-resources stamps each local session's own CPU/memory
// totals onto the session option @og_session_res, so a remote bridge can
// subscribe to them instead of paying an `ssh ps` round-trip per host (#693).
// It runs on every tmux-og host, armed by the @og-res-tick monitor hook: a
// control client renders no status line, so nothing driven from
// status-format[0] ever runs on a host whose only clients are bridges.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/proctree"
)

const resOption = "@og_session_res"

// paneAgentOption is the per-pane companion of resOption: the agent command
// found anywhere in that pane's process tree, or unset when there is none. It
// is what lets a consumer (the bridge's paste gate, the mirror process icon)
// see an agent a wrapper script runs as a child of the pane's foreground
// shell, where pane_current_command only reports the shell.
const paneAgentOption = "@og_pane_agent"

// psBin is set at link time (picker/default.nix): the monitor hook this runs
// from inherits the server's PATH, which a headless tmux-startup.service leaves
// empty. It is the store's procps on Linux and Apple's own /bin/ps on darwin,
// because the store's adv_cmds ps lacks the entitlement its rss keyword needs.
// "ps" is the go-build fallback.
var psBin = "ps"

// psEnv overrides psBin, for the darwin build sandbox: it cannot exec /bin/ps,
// so a test supplies a stand-in there.
const psEnv = "OG_PS_BIN"

func psPath() string {
	if p := os.Getenv(psEnv); p != "" {
		return p
	}
	return psBin
}

func main() {
	if len(os.Args) != 2 || os.Args[1] != "--tick" {
		fmt.Fprintln(os.Stderr, "usage: tmux-session-resources --tick")
		os.Exit(2)
	}

	// Every failure below exits 0 in silence: this is a 5s background poller
	// with no terminal of its own, so a complaint lands in whatever pane the
	// monitor hook happens to run under.
	clients, err := exec.Command("tmux", "list-clients", "-F", "#{client_control_mode}").Output()
	if err != nil || !parseControlClients(string(clients)) {
		return
	}

	panes, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{session_id}|#{pane_id}|#{pane_pid}").Output()
	if err != nil {
		return
	}
	paneRows := parsePaneRows(string(panes))
	if len(paneRows) == 0 {
		return
	}
	sessionRoots := make(map[string][]int)
	paneRoots := make(map[string][]int)
	for _, r := range paneRows {
		sessionRoots[r.session] = append(sessionRoots[r.session], r.pid)
		paneRoots[r.pane] = append(paneRoots[r.pane], r.pid)
	}

	ps, err := exec.Command(psPath(), proctree.PSArgs...).Output() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	if err != nil {
		return
	}

	// runtime.NumCPU() retires the old `getconf _NPROCESSORS_ONLN` the ssh leg
	// had to use — `nproc` is coreutils-only and absent on macOS, and a Go
	// binary needs neither.
	cores := runtime.NumCPU()
	now := time.Now().Unix()
	rows := make(map[string]string)
	for sess, t := range proctree.Aggregate(sessionRoots, string(ps)) {
		rows[sess] = stampValue(t, cores, now)
	}
	// A second, per-pane aggregate over the same ps table: the session rows
	// keep their exact meaning, and each pane gets the agent its own tree
	// carries.
	paneAgents := paneAgentMap(paneRoots, string(ps))
	argv := setOptionArgv(rows)
	paneArgv := paneAgentArgv(paneAgents)
	if len(argv) > 0 && len(paneArgv) > 0 {
		argv = append(argv, ";")
	}
	argv = append(argv, paneArgv...)
	if argv == nil {
		return
	}
	_ = exec.Command("tmux", argv...).Run() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
}

// parseControlClients is the gate: a pass runs only while something is bridged
// to this host, so a host nobody mirrors pays nothing. A control-mode client is
// exactly what a remote bridge attaches, and it is the only consumer of what
// this binary stamps. Anything that is not a literal "1" — no clients, a failed
// read's empty string, garbage — reads as "nobody is watching".
func parseControlClients(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "1" {
			return true
		}
	}
	return false
}

// stampValue renders one session's "<cpu> <mem> <cores> <tick> <agents>". It
// quantises, it does not render: the picker's formatCPU/formatMem stay the
// sole owners of display precision. %.1f keeps a sub-1% session apart from an
// idle one — 0 is a measurement, not an absence. The tick exists only so the
// value moves every pass (see the daemon's sessionResFormat), and agents is the
// tree's agent commands comma-joined, or "-": a mirror's pane commands are its
// renderers, so a relaunched agent reaches the picker's Procs column this way.
// No field can hold "|", which the picker's pipe-delimited read depends on.
func stampValue(t proctree.Totals, cores int, now int64) string {
	agents := "-"
	if len(t.AgentCmds) > 0 {
		agents = strings.Join(t.AgentCmds, ",")
	}
	return fmt.Sprintf("%.1f %.0f %d %d %s", t.CPUPct, t.MemMB, cores, now, agents)
}

// paneRow is one list-panes row: the pane's session id ($N), its pane id (%N)
// and its root pid. Grouping pids by session gives the session CPU/mem figures,
// grouping them by pane gives the per-pane agent.
type paneRow struct {
	session string
	pane    string
	pid     int
}

// parsePaneRows reads `#{session_id}|#{pane_id}|#{pane_pid}` lines. A row with
// an unusable pid is skipped whole rather than contributing a zero root.
func parsePaneRows(out string) []paneRow {
	var rows []paneRow
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			continue
		}
		pid, err := strconv.Atoi(parts[2])
		if err != nil || pid <= 0 {
			continue
		}
		rows = append(rows, paneRow{session: parts[0], pane: parts[1], pid: pid})
	}
	return rows
}

// paneAgentMap resolves each pane's tree agent over one ps table: the first of
// AgentCmds (already sorted and deduped by Aggregate), or "" when the tree
// carries no agent. EVERY pane in paneRoots gets an entry, empty ones included
// — that is what lets paneAgentArgv UNSET a pane that lost its agent, rather
// than omitting it and leaving a stale @og_pane_agent the daemon keeps reading
// as an agent after the process is gone.
func paneAgentMap(paneRoots map[string][]int, psOut string) map[string]string {
	found := make(map[string]string)
	for pane, t := range proctree.Aggregate(paneRoots, psOut) {
		if len(t.AgentCmds) > 0 {
			found[pane] = t.AgentCmds[0]
		}
	}
	out := make(map[string]string, len(paneRoots))
	for pane := range paneRoots {
		out[pane] = found[pane]
	}
	return out
}

// paneAgentArgv batches every pane's tree-agent stamp into one tmux argv,
// keyed by pane id and sorted so the argv is deterministic. A pane with no
// agent is UNSET, not written empty, so an option the pane's agent left behind
// cannot keep a shell pane reading as an agent after the agent exits. Empty map
// → nil, so the caller can join it to the session argv without a stray `;`.
func paneAgentArgv(panes map[string]string) []string {
	if len(panes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(panes))
	for id := range panes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var argv []string
	for _, id := range ids {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		if agent := panes[id]; agent != "" {
			argv = append(argv, "set-option", "-p", "-t", id, paneAgentOption, agent)
		} else {
			argv = append(argv, "set-option", "-p", "-t", id, "-u", paneAgentOption)
		}
	}
	return argv
}

// setOptionArgv batches every session's stamp into one tmux argv. rows is keyed
// by session id ($N), never name: set-option's -t is a target-pane, so a bare
// name like "2" — tmux's own default naming — also resolves as a pane index in
// the current session's window, and the stamp lands on the current session:
// the mirrored one, with the bridge attached. Exec'd without a shell, so "$"
// needs no quoting. Sorted so the argv is deterministic.
func setOptionArgv(rows map[string]string) []string {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var argv []string
	for _, id := range ids {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv, "set-option", "-t", id, resOption, rows[id])
	}
	return argv
}
