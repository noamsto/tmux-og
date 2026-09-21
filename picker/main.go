// tmux-picker-generate runs the bubbletea session/window picker TUI.
//
// Usage:
//
//	tmux-picker-generate --tui              # session picker
//	tmux-picker-generate --tui --windows    # window picker (add --agent to filter)
//	tmux-picker-generate --tui --wall       # same, as a grid of live pane captures
//	tmux-picker-generate --which-key        # which-key keybind popup (#629)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/noamsto/themestate"
	"github.com/noamsto/tmux-og/picker/proctree"
)

// Build-time constants injected via icons_generated.go:
//   iconMap, fallbackIcon, maxIconsPicker
//   claudeSpinnerFrames, claudeIconWaiting, claudeIconCompacting,
//   claudeIconDone, claudeIconIdle, claudeIconError, claudeIconDenied
//   iconSession, iconDir, iconBranch (defaults, overridden by env/tmux)

// Staleness thresholds (seconds)
const (
	staleWaiting    = 30
	staleCompacting = 60
	staleProcessing = 300
	staleDone       = 60
	staleError      = 120
	staleDenied     = 60
)

// Catppuccin hex colors per theme
var claudeColors = map[string]map[string]string{
	"dark": {
		"waiting": "#fab387", "compacting": "#89dceb", "processing": "#94e2d5",
		"done": "#a6e3a1", "idle": "#6c7086", "error": "#f38ba8", "denied": "#f9e2af",
	},
	"light": {
		"waiting": "#fe640b", "compacting": "#04a5e5", "processing": "#179299",
		"done": "#40a02b", "idle": "#6c6f85", "error": "#d20f39", "denied": "#df8e1d",
	},
}

type sessionData struct {
	name       string
	path       string
	activity   int64
	procs      []string // unique process names
	panePIDs   []int    // shell PIDs for resource collection
	bridgeHost string   // @bridge_host — ssh host this session mirrors, "" when local
	current    bool     // this is the session the invoking tmux client is attached to
	agent      agentCounts
	cpuPct     float64 // total CPU% across all descendant processes
	memMB      float64 // total RSS in MiB across all descendant processes
	resUnknown bool    // mirror whose host has not reported yet: render "-", never the renderer's own figures
	cores      float64 // cores of the machine these processes run on; 0 means this one
	// @bridge_res — "<cpu> <mem> <cores> <epoch> <agents>", written by the bridge daemon
	// from the remote's own poller. The epoch is the daemon's LOCAL clock at the
	// moment it stamped, so a reader compares it against its own with no skew
	// correction, and it stops advancing when the bridge or the poller dies.
	bridgeRes string
}

type windowData struct {
	session string
	index   int
	name    string
	zoomed  bool
	branch  string
	active  bool // currently active window in its session
	procs   []string
	agent   agentCounts
	// Enrich identity, read from the window options build_window_label already
	// stamped (single source of truth — the status bar reads the same).
	labelID     string // @window_label_id   — "<provider> <id>" or ""
	labelRest   string // @window_label_rest_long — issue title (leading space)
	bridgeName  string // @window_bridge_name — daemon-owned remote window name, or ""
	bridgePane  string // @bridge_pane — remote pane id this window mirrors, or ""
	bridgeSock  string // @bridge_sock — ctl socket of the daemon mirroring this session
	bridgeHost  string // @bridge_host — ssh host the mirror session lives on, or ""
	prPlain     string // @window_pr_plain   — " <glyph> #<n>" or ""
	prState     string // @pr_state
	prCheck     string // @pr_check_state
	prMergeable string // @pr_mergeable
	prReview    string // @pr_review — approved|changes_requested|review_required|""
	prAutoMerge string // @pr_auto_merge — "1" or ""
	crewName    string // @crew_name  — agent codename (fan-out harness) or ""
	crewColor   string // @crew_color — tmux colour code paired with the codename
}

type agentCounts struct {
	waiting, compacting, processing, done, idle, errorCnt, denied int
	bg                                                            int // summed background shells, orthogonal to the state counts
	allStale                                                      bool
	anyUnseen                                                     bool
	issues                                                        []string // union of self-reported issue ids
}

// agentPaneInfo holds parsed pane file data with window-level targeting.
type agentPaneInfo struct {
	session string
	winIdx  int
	state   string
	ts      int64
	stale   bool
	unseen  bool
	bg      int
	issues  []string
}

func main() {
	args := os.Args[1:]
	flags := map[string]bool{}
	for _, a := range args {
		flags[a] = true
	}

	var err error
	if flags["--which-key"] {
		err = RunWhichKey()
	} else {
		err = runTUI(flags["--windows"], flags["--agent"], flags["--wall"], flags["--remote-pick"])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Data collection
// ---------------------------------------------------------------------------

// panesSnapshot is one `tmux list-panes -a` shared by the collectors that would
// otherwise each fork their own. The picker's open latency is dominated by
// round-trips queued behind the single-threaded tmux server, and session mode
// needs both derivations before it can paint.
type panesSnapshot []string

// collectPanesSnapshot fetches the union of the fields sessions() and paneMap()
// read. Fields are pipe-delimited; wrong field count fails closed (session_path
// and pane_current_command may contain |). @bridge_host is mid-format; pane_pid
// is the trailing field and is never empty on a live pane. @bridge_proc is
// appended last: a mirror pane's own pane_current_command is the bridge
// renderer, not the remote's real command (#513). @bridge_session_path follows
// it for the same reason: a mirror's own session_path is the launcher's cwd.
// @bridge_res is appended after both, and every later field must go after it
// too: each one is positional, so a mid-format insert shifts all the rest.
func collectPanesSnapshot() panesSnapshot {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_id}|#{session_name}|#{window_index}|#{session_path}|#{session_last_attached}|#{@bridge_host}|#{pane_current_command}|#{pane_pid}|#{@bridge_proc}|#{@bridge_session_path}|#{@bridge_res}").Output()
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// collectSessions is the standalone form, for the async callers that hold no
// snapshot of their own (the remote probe and zoxide's exclusion set).
func collectSessions() []sessionData {
	return collectPanesSnapshot().sessions()
}

func (snap panesSnapshot) sessions() []sessionData {
	type sessInfo struct {
		path       string
		activity   int64
		seen       map[string]bool
		procs      []string
		panePIDs   []int
		bridgeHost string
		bridgeRes  string
	}
	m := make(map[string]*sessInfo)

	for _, line := range snap {
		parts := strings.Split(line, "|")
		if len(parts) != 11 {
			continue
		}
		name, path, actStr, proc := parts[1], parts[3], parts[4], parts[6]
		// A mirror pane runs the bridge renderer; @bridge_proc carries what the
		// remote pane is really running.
		if bp := parts[8]; bp != "" {
			proc = bp
		}
		// A mirror's session_path names a local directory unrelated to the
		// remote session, so an absent stamp renders no path rather than that one.
		if parts[5] != "" {
			path = parts[9]
		}
		// Expand %h (tmux may store literal %h for home dir)
		if home := os.Getenv("HOME"); home != "" {
			path = strings.Replace(path, "%h", home, 1)
		}
		act, _ := strconv.ParseInt(actStr, 10, 64)

		si, ok := m[name]
		if !ok {
			// @bridge_res is session-scoped, so the first pane's copy is the
			// session's, not an approximation of it.
			si = &sessInfo{path: path, activity: act, seen: make(map[string]bool), bridgeHost: parts[5], bridgeRes: parts[10]}
			m[name] = si
		}
		if act > si.activity {
			si.activity = act
		}
		if proc != "" && !si.seen[proc] {
			si.seen[proc] = true
			si.procs = append(si.procs, proc)
		}
		if pid, err := strconv.Atoi(parts[7]); err == nil && pid > 0 {
			si.panePIDs = append(si.panePIDs, pid)
		}
	}

	sessions := make([]sessionData, 0, len(m))
	for name, si := range m {
		sessions = append(sessions, sessionData{
			name:       name,
			path:       si.path,
			activity:   si.activity,
			procs:      si.procs,
			panePIDs:   si.panePIDs,
			bridgeHost: si.bridgeHost,
			bridgeRes:  si.bridgeRes,
		})
	}
	return sessions
}

func collectSessionActivity() map[string]int64 {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}|#{session_last_attached}").Output()
	if err != nil {
		return nil
	}
	m := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 2 {
			continue
		}
		act, _ := strconv.ParseInt(parts[1], 10, 64)
		m[parts[0]] = act
	}
	return m
}

// stripTmuxColors removes tmux #[...] style markup and leading/trailing spaces.
func stripTmuxColors(s string) string {
	var sb strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '#' && s[i+1] == '[' {
			// Skip until closing ]
			j := strings.IndexByte(s[i:], ']')
			if j >= 0 {
				i += j + 1
				continue
			}
		}
		sb.WriteByte(s[i])
		i++
	}
	return strings.TrimSpace(sb.String())
}

// decodeBridgeName undoes the daemon's escape of @window_bridge_name. The daemon
// doubles every '#' because tmux collapses the pair again when it draws the
// status line; a '#{@window_bridge_name}' read hands back the stored value with
// the doubling intact, so a caller that renders the name itself — the picker
// draws its own rows — must undo it or every '#' shows up twice.
func decodeBridgeName(s string) string {
	return strings.ReplaceAll(s, "##", "#")
}

// winKey identifies a window across the panes that share it.
type winKey struct {
	sess string
	idx  int
}

// winInfo is one window's merged state across its panes, as parsed by
// parseWindowPaneRows. Package-scope so the pure parse is testable.
type winInfo struct {
	name        string
	zoomed      bool
	active      bool
	branch      string
	path        string // pane_current_path for git branch fallback
	bridgeWin   bool   // @bridge_win == "1" — arms the bridge substitution below and skips collectWindows' git fallback
	labelID     string
	labelRest   string
	bridgeName  string
	bridgePane  string
	bridgeSock  string
	bridgeHost  string
	prPlain     string
	prState     string
	prCheck     string
	prMergeable string
	prReview    string
	prAutoMerge string
	crewName    string
	crewColor   string
	seen        map[string]bool
	procs       []string
}

// parseWindowPaneRows parses `list-panes -a` rows (one per pane) into
// per-window records, merging panes that share a window. Pure — no exec — so
// the bridge substitution below is reachable from a test.
func parseWindowPaneRows(lines []string) ([]winKey, map[winKey]*winInfo) {
	m := make(map[winKey]*winInfo)
	// Preserve ordering
	var order []winKey

	field := func(parts []string, i int) string {
		if len(parts) > i {
			return parts[i]
		}
		return ""
	}
	for _, line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) != 35 {
			continue
		}
		sess := parts[0]
		idx, _ := strconv.Atoi(parts[1])
		wName := stripTmuxColors(parts[2])
		zoomed := parts[3] == "1"
		proc := parts[4]
		// A mirror pane runs the bridge renderer; @bridge_proc carries what the
		// remote pane is really running.
		if bp := field(parts, 29); bp != "" {
			proc = bp
		}
		active := parts[5] == "1"
		branch := stripTmuxColors(field(parts, 6))
		panePath := field(parts, 7)

		k := winKey{sess, idx}
		wi, ok := m[k]
		if !ok {
			bridgeWin := field(parts, 19) == "1"
			labelID := field(parts, 8)
			labelRest := field(parts, 9)
			prPlain := field(parts, 10)
			prState := field(parts, 11)
			prCheck := field(parts, 12)
			prMergeable := field(parts, 13)
			prReview := field(parts, 30)
			prAutoMerge := field(parts, 31)
			crewName := field(parts, 14)
			crewColor := field(parts, 15)
			bridgeName := decodeBridgeName(field(parts, 16))

			// A mirror window's identity lives on the remote, not in this
			// window's own options (which carry the launcher's residue) —
			// substitute the bridge copies wholesale and clear branch, which
			// is what arms collectWindows' git fallback below.
			if bridgeWin {
				crewName = field(parts, 20)
				crewColor = field(parts, 21)
				labelID = field(parts, 22)
				labelRest = field(parts, 23)
				prPlain = field(parts, 24)
				prState = field(parts, 25)
				prCheck = field(parts, 26)
				prMergeable = field(parts, 27)
				prReview = field(parts, 32)
				prAutoMerge = field(parts, 33)
				branch = ""
				// A remote window with no detected issue has no bridge id:
				// fall back to the bridge rest raw, not decodeBridgeName — it
				// isn't '#'-doubled the way @window_bridge_name is.
				if labelID == "" && labelRest != "" {
					bridgeName = labelRest
				}
			} else if field(parts, 34) != "1" {
				// No live agent in this window: the crew codename badge
				// outlived the agent that earned it (#671).
				crewName = ""
				crewColor = ""
			}

			wi = &winInfo{
				name: wName, zoomed: zoomed, active: active, branch: branch, path: panePath,
				bridgeWin:   bridgeWin,
				labelID:     labelID,
				labelRest:   labelRest,
				prPlain:     prPlain,
				prState:     prState,
				prCheck:     prCheck,
				prMergeable: prMergeable,
				prReview:    prReview,
				prAutoMerge: prAutoMerge,
				crewName:    crewName,
				crewColor:   crewColor,
				bridgeName:  bridgeName,
				bridgePane:  field(parts, 17),
				bridgeSock:  field(parts, 18),
				bridgeHost:  field(parts, 28),
				seen:        make(map[string]bool),
			}
			m[k] = wi
			order = append(order, k)
		}
		if proc != "" && !wi.seen[proc] {
			wi.seen[proc] = true
			wi.procs = append(wi.procs, proc)
		}
	}
	return order, m
}

func collectWindows() []windowData {
	// Fetch both @branch and pane path basename. The window_name contains
	// icons/colors from automatic-rename-format so we reconstruct a clean name.
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}|#{window_index}|#{b:pane_current_path}|#{window_zoomed_flag}|#{pane_current_command}|#{window_active}|#{@branch}|#{pane_current_path}|#{@window_label_id}|#{@window_label_rest_long}|#{@window_pr_plain}|#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@crew_name}|#{@crew_color}|#{@window_bridge_name}|#{@bridge_pane}|#{@bridge_sock}|#{@bridge_win}|#{@bridge_crew_name}|#{@bridge_crew_color}|#{@bridge_label_id}|#{@bridge_label_rest_long}|#{@bridge_pr_plain}|#{@bridge_pr_state}|#{@bridge_pr_check_state}|#{@bridge_pr_mergeable}|#{@bridge_host}|#{@bridge_proc}|#{@pr_review}|#{@pr_auto_merge}|#{@bridge_pr_review}|#{@bridge_pr_auto_merge}|#{@window_has_agent}").Output()
	if err != nil {
		return nil
	}

	order, m := parseWindowPaneRows(strings.Split(strings.TrimSpace(string(out)), "\n"))

	// Fill missing branches by running git in each pane's working directory.
	// Parallel: one git fork per window, all in flight at once — the picker's
	// first paint waits on the slowest single call, not their sum. Each goroutine
	// writes a distinct *winInfo, so no shared-state guard is needed. Skipped on
	// a mirror row: its cleared branch is the bridge substitution above, not a
	// gap to fill, and the git call would read the launcher's own repo, not the
	// remote's (SPEC R5.3).
	var wg sync.WaitGroup
	for _, k := range order {
		wi := m[k]
		if wi.branch == "" && wi.path != "" && !wi.bridgeWin {
			wg.Add(1)
			go func(wi *winInfo) {
				defer wg.Done()
				if out, err := exec.Command("git", "-C", wi.path, "branch", "--show-current").Output(); err == nil {
					wi.branch = strings.TrimSpace(string(out))
				}
			}(wi)
		}
	}
	wg.Wait()

	windows := make([]windowData, 0, len(order))
	for _, k := range order {
		wi := m[k]
		windows = append(windows, windowData{
			session:     k.sess,
			index:       k.idx,
			name:        wi.name,
			zoomed:      wi.zoomed,
			active:      wi.active,
			branch:      wi.branch,
			procs:       wi.procs,
			labelID:     wi.labelID,
			labelRest:   wi.labelRest,
			bridgeName:  wi.bridgeName,
			bridgePane:  wi.bridgePane,
			bridgeSock:  wi.bridgeSock,
			bridgeHost:  wi.bridgeHost,
			prPlain:     wi.prPlain,
			prState:     wi.prState,
			prCheck:     wi.prCheck,
			prMergeable: wi.prMergeable,
			prReview:    wi.prReview,
			prAutoMerge: wi.prAutoMerge,
			crewName:    wi.crewName,
			crewColor:   wi.crewColor,
		})
	}
	return windows
}

// ---------------------------------------------------------------------------
// Resource usage (CPU + memory per session)
// ---------------------------------------------------------------------------

// sessionResources holds aggregated CPU and memory for a session.
type sessionResources struct {
	cpuPct float64
	memMB  float64
	// agentCmds are the agent command names found anywhere in the session's
	// process tree, deduped and sorted. pane_current_command names a pane's
	// process-group leader, so an agent a shell chain relaunched — tmux-remux's
	// restore runs `cat-scrollback …; <agent>; exec <shell>` under one
	// non-interactive shell, which has no job control, so the agent shares the
	// shell's group — is invisible there and only the tree shows it.
	agentCmds []string
}

// psArgs is the process table both the local and the remote leg read.
var psArgs = proctree.PSArgs

// aggregateResources wraps proctree.Aggregate: sessionResources' fields are
// unexported, so a type alias to proctree.Totals isn't possible, and this
// converts between the two.
func aggregateResources(rootPIDs map[string][]int, psOut string) map[string]sessionResources {
	totals := proctree.Aggregate(rootPIDs, psOut)
	result := make(map[string]sessionResources, len(totals))
	for key, t := range totals {
		result[key] = sessionResources{cpuPct: t.CPUPct, memMB: t.MemMB, agentCmds: t.AgentCmds}
	}
	return result
}

// resourceCache holds the last result to avoid re-running ps every 1s tick.
var resourceCache struct {
	sync.Mutex
	result map[string]sessionResources
	ts     time.Time
}

const resourceCacheTTL = 5 * time.Second

// collectSessionResources returns per-session CPU% and RSS by walking the
// process tree from each tmux pane PID. Runs ps once and builds a parent→children
// map to sum all descendants. Results are cached for 5s since resource data
// changes slowly relative to the 1s TUI refresh rate.
func collectSessionResources(sessions []sessionData) map[string]sessionResources {
	resourceCache.Lock()
	if time.Since(resourceCache.ts) < resourceCacheTTL && resourceCache.result != nil {
		cached := resourceCache.result
		resourceCache.Unlock()
		return cached
	}
	resourceCache.Unlock()

	sessionPIDs := make(map[string][]int, len(sessions))
	for _, s := range sessions {
		if len(s.panePIDs) > 0 {
			sessionPIDs[s.name] = s.panePIDs
		}
	}

	psOut, err := exec.Command("ps", psArgs...).Output()
	if err != nil {
		return nil
	}
	result := aggregateResources(sessionPIDs, string(psOut))

	resourceCache.Lock()
	resourceCache.result = result
	resourceCache.ts = time.Now()
	resourceCache.Unlock()

	return result
}

func mergeResources(sessions []sessionData, res map[string]sessionResources) {
	for i := range sessions {
		if r, ok := res[sessions[i].name]; ok {
			sessions[i].cpuPct = r.cpuPct
			sessions[i].memMB = r.memMB
			mergeAgentCmds(&sessions[i], r.agentCmds)
		}
	}
}

// mergeAgentCmds appends the agent commands found in a session's process tree
// to its proc list, so a relaunched agent still renders its program icon when
// the pane's foreground command is the shell that launched it.
func mergeAgentCmds(s *sessionData, cmds []string) {
	for _, c := range cmds {
		if slices.Contains(s.procs, c) {
			continue
		}
		s.procs = append(s.procs, c)
	}
}

// formatCPU returns a compact CPU% string. Busy but under 1% reads as "<1%",
// not a flat 0%: a single process ticking over lands there on either leg.
func formatCPU(cpuPct float64) string {
	if cpuPct > 0 && cpuPct < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", cpuPct)
}

// cpuColWidth returns the reserved column width for CPU%, sized to the
// worst-case value (numCPU * 100%) so the column doesn't shift between
// renders when usage crosses a digit boundary (e.g. 99% → 300%).
func cpuColWidth() int {
	return len(formatCPU(numCPU * 100))
}

// formatMem returns a compact memory string (M or G).
func formatMem(memMB float64) string {
	if memMB < 1024 {
		return fmt.Sprintf("%.0fM", memMB)
	}
	return fmt.Sprintf("%.1fG", memMB/1024.0)
}

// resourceColors holds themed ANSI color codes for resource level coloring.
type resourceColors struct {
	low, med, high, crit string
}

func newResourceColors(tmuxOpts map[string]string) resourceColors {
	return resourceColors{
		low:  ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8")),
		med:  ansiFg(envOrMap("THM_YELLOW", tmuxOpts, "@thm_yellow", "#f9e2af")),
		high: ansiFg(envOrMap("THM_PEACH", tmuxOpts, "@thm_peach", "#fab387")),
		crit: ansiFg(envOrMap("THM_RED", tmuxOpts, "@thm_red", "#f38ba8")),
	}
}

var numCPU = float64(runtime.NumCPU())

// cpuColor thresholds scale with the core count of the machine the processes
// run on, which for a mirror row is the remote's, not this one's:
// low: <10% of total, med: <25%, high: <60%, crit: ≥60%
func (rc resourceColors) cpuColor(pct, cores float64) string {
	if cores < 1 {
		cores = numCPU
	}
	ratio := pct / (cores * 100)
	switch {
	case ratio < 0.10:
		return rc.low
	case ratio < 0.25:
		return rc.med
	case ratio < 0.60:
		return rc.high
	default:
		return rc.crit
	}
}

// memColor thresholds are absolute — memory pressure is memory pressure.
func (rc resourceColors) memColor(mb float64) string {
	switch {
	case mb < 256:
		return rc.low
	case mb < 1024:
		return rc.med
	case mb < 4096:
		return rc.high
	default:
		return rc.crit
	}
}

// ---------------------------------------------------------------------------
// Agent status
// ---------------------------------------------------------------------------

// hookPaneState is one /tmp/claude-status/panes/<pane_id> hook-written state
// file — written by the Claude Code plugin's own hooks.
type hookPaneState struct {
	state     string
	session   string
	timestamp int64
	unseen    bool
}

// screenPaneState is one /tmp/claude-status/screen/<pane_id> scraper-written
// state file (agentdetect) — no session/unseen fields, since the scraper
// never sees the hook's request context, only the pane's screen contents.
type screenPaneState struct {
	state     string
	timestamp int64
	bg        int
}

func readHookPaneStates(dir string) map[string]hookPaneState {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make(map[string]hookPaneState, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var hp hookPaneState
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				switch k {
				case "state":
					hp.state = v
				case "session":
					hp.session = v
				case "timestamp":
					hp.timestamp, _ = strconv.ParseInt(v, 10, 64)
				case "unseen":
					hp.unseen = v == "1"
				}
			}
		}
		if hp.state == "" {
			continue
		}
		out[e.Name()] = hp
	}
	return out
}

func readScreenPaneStates(dir string) map[string]screenPaneState {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make(map[string]screenPaneState, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var sp screenPaneState
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				switch k {
				case "state":
					sp.state = v
				case "timestamp":
					sp.timestamp, _ = strconv.ParseInt(v, 10, 64)
				case "bg":
					sp.bg, _ = strconv.Atoi(v)
				}
			}
		}
		if sp.state == "" {
			continue
		}
		out[e.Name()] = sp
	}
	return out
}

// screenOverrideMaxAge mirrors the max_age table in read_pane_state
// (scripts/lib-claude.sh): only a hook state that a missed completion hook
// can leave stuck (compacting/processing/done) is eligible for a screen
// override. waiting/error/denied are human-blocking states the scraper must
// never downgrade — they look identical to idle on screen.
func screenOverrideMaxAge(state string) int64 {
	switch state {
	case "compacting":
		return staleCompacting
	case "processing":
		return staleProcessing
	case "done":
		return staleDone
	}
	return 0
}

// collectAgentPanes merges the hook-written state (panes/) with the
// screen-scraper's state (screen/) for every pane, hook-first, with the same
// state precedence read_pane_state uses in scripts/lib-claude.sh. Unlike
// read_pane_state, a screen override re-resolves session from the live pane
// map instead of blanking it — this picker's session/window aggregation both
// key off the same field, so blanking it would drop the pane entirely.
func collectAgentPanes(snap panesSnapshot) []agentPaneInfo {
	return collectAgentPanesFrom(
		"/tmp/claude-status/panes", "/tmp/claude-status/screen", "/tmp/claude-status/issues",
		snap.paneMap(), time.Now().Unix(),
	)
}

// collectAgentPanesFrom is collectAgentPanes with its directories, pane map,
// and clock injected — exercised directly by tests so the hook/screen merge
// precedence is pure Go, no tmux or /tmp required.
func collectAgentPanesFrom(hookDir, screenDir, issuesDir string, paneMap map[string]paneMapping, now int64) []agentPaneInfo {
	hooks := readHookPaneStates(hookDir)
	screens := readScreenPaneStates(screenDir)
	if len(hooks) == 0 && len(screens) == 0 {
		return nil
	}

	ids := make(map[string]bool, len(hooks)+len(screens))
	for id := range hooks {
		ids[id] = true
	}
	for id := range screens {
		ids[id] = true
	}

	var result []agentPaneInfo
	for id := range ids {
		hook, hasHook := hooks[id]
		screen, hasScreen := screens[id]

		var state, session string
		var timestamp int64
		var unseen bool

		pm, hasPane := paneMap[id]

		// Orthogonal to the state, so it is taken from the screen file whichever
		// source wins below — only the scraper ever sees background shells, and
		// they outlive the turn that started them.
		bg := screen.bg

		if hasHook {
			state, timestamp, session, unseen = hook.state, hook.timestamp, hook.session, hook.unseen
			if maxAge := screenOverrideMaxAge(state); maxAge > 0 && now-timestamp > maxAge && hasScreen {
				// Hook stale past its own threshold with a live screen
				// reading available — screen is ground truth.
				state, timestamp, unseen = screen.state, screen.timestamp, false
				if hasPane {
					session = pm.session
				}
			}
		} else {
			// Scraper-only pane: the screen file carries no session, so it
			// can only be placed by asking tmux directly.
			if !hasPane {
				continue
			}
			state, timestamp, session = screen.state, screen.timestamp, pm.session
		}
		if session == "" {
			continue
		}

		winIdx := -1
		if hasPane {
			winIdx = pm.winIdx
		}

		result = append(result, agentPaneInfo{
			session: session,
			winIdx:  winIdx,
			state:   state,
			bg:      bg,
			ts:      timestamp,
			stale:   isStale(state, now, timestamp),
			unseen:  unseen,
			issues:  readPaneIssues(filepath.Join(issuesDir, id)),
		})
	}
	return result
}

// readPaneIssues reads the comma-separated self-reported issue id list for a
// pane. Missing file (the common case) yields nil.
func readPaneIssues(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(line, ",") {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

type paneMapping struct {
	session string
	winIdx  int
}

func (snap panesSnapshot) paneMap() map[string]paneMapping {
	m := make(map[string]paneMapping)
	for _, line := range snap {
		parts := strings.Split(line, "|")
		// Must track collectPanesSnapshot's field count in lockstep with
		// sessions(): a stale count here drops every pane from the map rather
		// than spoiling one column, and the agent state silently empties
		// across the whole picker.
		if len(parts) != 11 {
			continue
		}
		paneID := strings.TrimPrefix(parts[0], "%")
		idx, _ := strconv.Atoi(parts[2])
		m[paneID] = paneMapping{session: parts[1], winIdx: idx}
	}
	return m
}

func aggregateAgentBySession(panes []agentPaneInfo) map[string]*agentCounts {
	result := make(map[string]*agentCounts)
	for _, p := range panes {
		cc, ok := result[p.session]
		if !ok {
			cc = &agentCounts{allStale: true}
			result[p.session] = cc
		}
		if !p.stale {
			cc.allStale = false
		}
		if p.unseen {
			cc.anyUnseen = true
		}
		cc.bg += p.bg
		addAgentState(cc, p.state)
		addIssues(cc, p.issues)
	}
	return result
}

func aggregateAgentByWindow(panes []agentPaneInfo) map[string]*agentCounts {
	result := make(map[string]*agentCounts)
	for _, p := range panes {
		if p.winIdx < 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", p.session, p.winIdx)
		cc, ok := result[key]
		if !ok {
			cc = &agentCounts{allStale: true}
			result[key] = cc
		}
		if !p.stale {
			cc.allStale = false
		}
		if p.unseen {
			cc.anyUnseen = true
		}
		cc.bg += p.bg
		addAgentState(cc, p.state)
		addIssues(cc, p.issues)
	}
	return result
}

func addAgentState(cc *agentCounts, state string) {
	switch state {
	case "waiting":
		cc.waiting++
	case "compacting":
		cc.compacting++
	case "processing":
		cc.processing++
	case "done":
		cc.done++
	case "idle":
		cc.idle++
	case "error":
		cc.errorCnt++
	case "denied":
		cc.denied++
	}
}

func addIssues(cc *agentCounts, ids []string) {
	for _, id := range ids {
		seen := false
		for _, e := range cc.issues {
			if e == id {
				seen = true
				break
			}
		}
		if !seen {
			cc.issues = append(cc.issues, id)
		}
	}
}

func isStale(state string, now, ts int64) bool {
	if ts == 0 {
		return false
	}
	age := now - ts
	switch state {
	case "waiting":
		return age > staleWaiting
	case "compacting":
		return age > staleCompacting
	case "processing":
		return age > staleProcessing
	case "done":
		return age > staleDone
	case "error":
		return age > staleError
	case "denied":
		return age > staleDenied
	}
	return false
}

func mergeAgent(sessions []sessionData, agent map[string]*agentCounts) {
	if agent == nil {
		return
	}
	for i := range sessions {
		if cc, ok := agent[sessions[i].name]; ok {
			sessions[i].agent = *cc
		}
	}
}

func mergeAgentWindows(windows []windowData, agent map[string]*agentCounts) {
	if agent == nil {
		return
	}
	for i := range windows {
		key := fmt.Sprintf("%s:%d", windows[i].session, windows[i].index)
		if cc, ok := agent[key]; ok {
			windows[i].agent = *cc
		}
	}
}

// agentStateOrder mirrors lib-claude.sh's claude_priority_state, minus
// "interrupted" — a shell-only state derived from a transcript-tail scrape
// the Go picker never does (see CLAUDE.md's Remote Agent Status section).
// Single source of truth for both agentPriority (per-window state) and
// window mode's state-grouped header order (#229) — a second, hand-copied
// list here would silently diverge from this one.
var agentStateOrder = []string{
	"error", "waiting", "denied", "compacting", "processing", "done", "idle",
}

// agentStateLabel is the header text for each state in window mode's
// state-grouped view; "" (no agent state at all) is the trailing group.
var agentStateLabel = map[string]string{
	"error": "Error", "waiting": "Waiting", "denied": "Denied",
	"compacting": "Compacting", "processing": "Processing", "done": "Done",
	"idle": "Idle", "": "No agent",
}

func agentStateCount(c agentCounts, state string) int {
	switch state {
	case "error":
		return c.errorCnt
	case "waiting":
		return c.waiting
	case "denied":
		return c.denied
	case "compacting":
		return c.compacting
	case "processing":
		return c.processing
	case "done":
		return c.done
	case "idle":
		return c.idle
	}
	return 0
}

func agentPriority(c agentCounts) string {
	for _, state := range agentStateOrder {
		if agentStateCount(c, state) > 0 {
			return state
		}
	}
	return ""
}

func claudeStateIcon(state string) string {
	now := time.Now().Unix()
	switch state {
	case "processing":
		return claudeSpinnerFrames[int(now)%len(claudeSpinnerFrames)]
	case "waiting":
		return claudeIconWaiting
	case "compacting":
		return claudeIconCompacting
	case "done":
		return claudeIconDone
	case "idle":
		return claudeIconIdle
	case "error":
		return claudeIconError
	case "denied":
		return claudeIconDenied
	}
	return ""
}

func appendAgentIcon(icons string, dw int, cc agentCounts, theme, dim, reset string) (string, int) {
	state := agentPriority(cc)
	if state == "" {
		return icons, dw
	}
	icon := claudeStateIcon(state)
	var color string
	// Dim stale icons, but unseen overrides — stay bright until user looks
	if cc.allStale && !cc.anyUnseen {
		color = dim
	} else {
		colors := claudeColors[theme]
		if hex, ok := colors[state]; ok {
			color = ansiFg(hex)
		}
	}
	icons += color + icon + reset + " "
	dw += 2

	// Additive, never part of agentPriority: background shells outlive the turn
	// that started them, so a pane can hold them in any state. Not dimmed with
	// the rest — an old background shell is more interesting, not less.
	if cc.bg > 0 {
		count := strconv.Itoa(cc.bg)
		var bgColor string
		if hex, ok := claudeColors[theme]["compacting"]; ok {
			bgColor = ansiFg(hex)
		}
		icons += bgColor + claudeIconBG + count + reset + " "
		dw += 2 + len(count)
	}
	return icons, dw
}

func formatIssueIDs(ids []string, limit int) string {
	if len(ids) == 0 {
		return ""
	}
	if len(ids) <= limit {
		return strings.Join(ids, " ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(ids[:limit], " "), len(ids)-limit)
}

// appendIssueIDs appends a dim "ENG-1 GH-2 +N" segment to the icons string.
// Ids are validated ASCII ([A-Za-z0-9_-]), so len() is the display width.
func appendIssueIDs(icons string, dw int, ids []string, cDim, reset string) (string, int) {
	s := formatIssueIDs(ids, 2)
	if s == "" {
		return icons, dw
	}
	return icons + cDim + s + reset + " ", dw + len(s) + 1
}

// ---------------------------------------------------------------------------
// Icon helpers
// ---------------------------------------------------------------------------

// wrappedProcRe strips the makeWrapper shape nix-built binaries report as
// pane_current_command (e.g. ".claude-wrapped") down to the plain name, the
// same normalization scripts/lib-icons.sh and statusline's wrappedRe apply.
var wrappedProcRe = regexp.MustCompile(`^\.(.*)-wrapped$`)

func buildProcIcons(procs []string, maxCount int) (string, int) {
	var sb strings.Builder
	dw := 0
	count := 0
	for _, proc := range procs {
		if count >= maxCount {
			break
		}
		if m := wrappedProcRe.FindStringSubmatch(proc); m != nil {
			proc = m[1]
		}
		icon, ok := iconMap[proc]
		if !ok {
			icon = fallbackIcon
		}
		if icon == "" {
			continue
		}
		sb.WriteString(icon)
		sb.WriteByte(' ')
		dw += iconCellWidth(icon) + 1
		count++
	}
	return sb.String(), dw
}

// runeCellWidth returns the display width of a single rune, handling nerd font
// PUA (1 cell) and deferring to go-runewidth for everything else.
func runeCellWidth(r rune) int {
	switch {
	case r == 0xFE0E || r == 0xFE0F: // variation selectors
		return 0
	case r == 0x200D: // zero-width joiner
		return 0
	case r >= 0x0300 && r <= 0x036F: // combining diacritical marks
		return 0
	case r >= 0x20D0 && r <= 0x20FF: // combining marks for symbols
		return 0
	case (r >= 0xE000 && r <= 0xF8FF) || r >= 0xF0000:
		return 1 // nerd font PUA = 1 cell
	default:
		return runewidth.RuneWidth(r)
	}
}

// iconCellWidth returns the display width of an icon string.
// VS16 emoji are stripped at build time (process-icons.nix) to avoid lipgloss
// width miscalculation (charmbracelet/lipgloss#55, #562).
func iconCellWidth(s string) int {
	runes := []rune(s)
	w := 0
	for _, r := range runes {
		w += runeCellWidth(r)
	}
	return w
}

func padToWidth(s string, currentWidth, targetWidth int) string {
	pad := targetWidth - currentWidth
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// truncateCells clamps s to max display cells (rune- and width-aware), reserving
// one cell for an ellipsis when it has to cut. Byte slicing would split
// multibyte titles; this counts cells like the rest of the picker.
func truncateCells(s string, max int) string {
	if iconCellWidth(s) <= max {
		return s
	}
	budget := max - 1
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := runeCellWidth(r)
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// branchEchoesName reports whether showing a branch would merely restate the
// window name. A worktree window takes its name from the checkout dir, which
// worktrunk derives from the branch by replacing "/" with "-", so the branch
// column would otherwise duplicate the name (name "feat-5-x" vs branch
// "feat/5-x").
func branchEchoesName(branch, name string) bool {
	return branch == name || strings.ReplaceAll(branch, "/", "-") == name
}

// prColors holds the PR badge tints, mirroring the status bar's coloring.
type prColors struct{ success, failure, pending, merged, closed, required, underline, reset string }

// colorPRBadge tints a plain PR badge (" <glyph> #<n>" from @window_pr_plain),
// mirroring the status bar. The glyph half takes the state tint: merged/closed
// PRs → terminal (so a leftover pending/failed rollup can't mask them — closed
// is a dead/superseded PR, dimmed so it can't read as live), a conflicting merge
// or failing checks → failure, pending → pending, else success. The #<n> half of
// an open PR takes its review tint and an auto-merge underline instead. Returns
// "" when there is no PR.
func colorPRBadge(prPlain, state, check, mergeable, review, autoMerge string, c prColors) string {
	badge := strings.TrimSpace(prPlain)
	if badge == "" {
		return ""
	}
	var col string
	switch {
	case state == "merged":
		col = c.merged
	case state == "closed":
		col = c.closed
	case mergeable == "conflicting", check == "failure":
		col = c.failure
	case check == "pending":
		col = c.pending
	default:
		col = c.success
	}
	// A mirror's badge is remote-derived and sanitized, not guaranteed shaped.
	i := strings.LastIndex(badge, "#")
	if i < 0 {
		return col + badge + c.reset
	}
	numCol, underline := col, ""
	if state == "open" {
		switch review {
		case "approved":
			numCol = c.success
		case "changes_requested":
			numCol = c.failure
		case "review_required":
			numCol = c.required
		}
		if autoMerge == "1" {
			underline = c.underline
		}
	}
	return col + badge[:i] + c.reset + numCol + underline + badge[i:] + c.reset
}

// ---------------------------------------------------------------------------
// Theme / tmux helpers
// ---------------------------------------------------------------------------

// ansiFgTmux turns a tmux colour value into an ANSI fg escape. It accepts the
// 256-palette form ("colour210") the crew harness stamps and the "#rrggbb" theme
// form; returns "" for anything else (incl. "default").
func ansiFgTmux(c string) string {
	if n, ok := strings.CutPrefix(c, "colour"); ok {
		if v, err := strconv.Atoi(n); err == nil && v >= 0 && v < 256 {
			return fmt.Sprintf("\033[38;5;%dm", v)
		}
		return ""
	}
	return ansiFg(c)
}

func ansiFg(hex string) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return ""
	}
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

func ansiBg(hex string) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return ""
	}
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
}

// parseShowOptionsLine splits one line of `show -g -F '#{option_name}
// #{option_value}'` output into name and value. #{option_value} in this
// custom format is raw — unquoted, unescaped — unlike the default `show -g`
// template's #{q/a:option_value}, so an empty-string option reads back as
// empty rather than the literal ”. A line with no space (malformed) reports
// ok=false.
func parseShowOptionsLine(line string) (name, value string, ok bool) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return "", "", false
	}
	return line[:i], strings.TrimRight(line[i+1:], " \t\r"), true
}

// readTmuxOpts reads every global option's raw value in one call via a
// custom -F format, which bypasses the default template's quote-escaping —
// so an empty-string option comes back empty, not the literal ”.
func readTmuxOpts() map[string]string {
	out, err := exec.Command("tmux", "show", "-g", "-F", "#{option_name} #{option_value}").Output()
	if err != nil {
		return nil
	}
	m := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if name, value, ok := parseShowOptionsLine(line); ok {
			m[name] = value
		}
	}
	return m
}

// themeFromOpts derives "light"/"dark" from a global-options snapshot
// (readTmuxOpts()'s map), preferring the live tmux flavor over the
// themestate state file. Falls back to themestate.Detect() only when
// @catppuccin_flavor is unset (e.g. before config load, or outside tmux).
func themeFromOpts(opts map[string]string) string {
	switch opts["@catppuccin_flavor"] {
	case "":
		return themestate.Detect()
	case "latte":
		return "light"
	default:
		return "dark"
	}
}

// logEvent fires the og-log-event CLI (best-effort, never blocks the UI).
// Bare-name exec relies on the tmux wrapper's PATH, like our tmux/zoxide calls.
// No-ops when debug is off (the CLI checks the sentinel).
func logEvent(args ...string) {
	exec.Command("og-log-event", args...).Run() //nolint:errcheck
}

func envOrMap(envKey string, tmuxOpts map[string]string, tmuxOpt, fallback string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if v, ok := tmuxOpts[tmuxOpt]; ok && v != "" {
		return v
	}
	return fallback
}
