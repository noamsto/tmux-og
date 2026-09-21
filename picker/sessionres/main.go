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

	panes, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{session_id}|#{pane_pid}").Output()
	if err != nil {
		return
	}
	roots := parsePanePIDs(string(panes))
	if len(roots) == 0 {
		return
	}

	ps, err := exec.Command("ps", proctree.PSArgs...).Output()
	if err != nil {
		return
	}

	// runtime.NumCPU() retires the old `getconf _NPROCESSORS_ONLN` the ssh leg
	// had to use — `nproc` is coreutils-only and absent on macOS, and a Go
	// binary needs neither.
	cores := runtime.NumCPU()
	now := time.Now().Unix()
	rows := make(map[string]string)
	for sess, t := range proctree.Aggregate(roots, string(ps)) {
		rows[sess] = stampValue(t, cores, now)
	}
	argv := setOptionArgv(rows)
	if argv == nil {
		return
	}
	_ = exec.Command("tmux", argv...).Run()
}

// parseControlClients is the gate: a pass runs only while something is bridged
// to this host, so a host nobody mirrors pays nothing. A control-mode client is
// exactly what a remote bridge attaches, and it is the only consumer of what
// this binary stamps. Anything that is not a literal "1" — no clients, a failed
// read's empty string, garbage — reads as "nobody is watching".
func parseControlClients(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "1" {
			return true
		}
	}
	return false
}

// parsePanePIDs groups every pane's pid by session id, the root set proctree
// walks from. Keyed by id, never name, because the id is what setOptionArgv
// targets. Split at the LAST "|": pane_pid is the trailing field.
func parsePanePIDs(out string) map[string][]int {
	roots := make(map[string][]int)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.LastIndex(line, "|")
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[i+1:])
		if err != nil || pid <= 0 {
			continue
		}
		sess := line[:i]
		roots[sess] = append(roots[sess], pid)
	}
	return roots
}

// stampValue quantises one session's totals for the wire. It is not rendering:
// the picker's formatCPU/formatMem stay the sole owners of display precision,
// and a second copy of "<1%" or M-vs-G across this boundary would be two things
// to keep in step. %.1f is what keeps a sub-1% session distinguishable from an
// idle one — 0 is a real measurement, not an absent one.
//
// The epoch exists ONLY so the value moves on every pass. A tmux option
// outlives the process that wrote it, so re-reading it can never tell a live
// poller from a dead one; the absence of notifications is the daemon's only
// evidence. It dedupes on every field but this one, so the tick costs it no
// local write.
//
// The last field is the agent commands found in the session's tree,
// comma-joined, or "-" for none: a mirror pane's own command is the renderer
// and its @bridge_proc names only the remote pane's group leader, so an agent a
// restore chain relaunched under a shell reaches the picker's Procs column by
// this field alone.
//
// By construction the value holds no "|". That matters: the picker reads
// session-scoped @bridge_* out of a pipe-delimited list-panes -a format whose
// parse fails CLOSED on a wrong field count, so a pipe here would not garble
// one column, it would drop the whole session from the picker.
func stampValue(t proctree.Totals, cores int, now int64) string {
	agents := "-"
	if len(t.AgentCmds) > 0 {
		agents = strings.Join(t.AgentCmds, ",")
	}
	return fmt.Sprintf("%.1f %.0f %d %d %s", t.CPUPct, t.MemMB, cores, now, agents)
}

// setOptionArgv batches every session's stamp into one tmux argv, the way the
// bridge's own shippers batch theirs. rows is keyed by session id ($N), never
// name: set-option's -t is a target-pane, and a bare name like "0" or "2" — the
// names tmux hands out by default — also resolves as a pane index in the
// current window, so a name-keyed stamp lands on whichever session is current
// (measured: `set-option -t 0` from run-shell wrote session zed). The argv is
// exec'd without a shell, so the "$" needs no quoting. Sorted so the argv is
// deterministic: map order would otherwise make it untestable.
func setOptionArgv(rows map[string]string) []string {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)

	var argv []string
	for _, name := range names {
		if len(argv) > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv, "set-option", "-t", name, resOption, rows[name])
	}
	return argv
}
