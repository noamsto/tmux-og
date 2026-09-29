// Variant C: the 1 s window-naming path of scripts/tmux-update-icons.sh in Go,
// with the same collapsed call pattern as variant B (one tmux read, one tmux
// write only when something changed, one guarded git per polled window).
//
// Out of scope, as in every variant: the 5 s arming sweep, carousel/remux
// stamping, the cwd-move reconcile, the reflow kick, agent-state file parsing,
// and the branch-transition path. Each exits 3 rather than run a path the
// fixture does not exercise.
package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

//go:embed icons.tsv
var iconsTSV string

const paneFormat = "P|#{pane_id}|#{session_id}|#{window_index}|#{pane_index}|#{pane_current_path}|#{pane_current_command}|#{@branch}|#{pane_floating_flag}|#{@worktree}|#{@window_cwd_seen}|#{?window_modal_pane,#{pane_last},#{pane_active}}|#{window_active}|#{s/[|]/ /:@window_ai_name}|#{s/[|]/ /:@remux_relaunch}|#{@window_icon_display}|#{@window_icon_padded}|#{@window_claude_ago}|#{automatic-rename}|#{@active_pane_icon}|#{@claude_session_fg}|#{@crew_name}|#{@crew_seen}|#{@bridge_win}|#{@bridge_proc}|#{@claude_img_src}|#{@window_has_agent}|#{@window_manual_name}|#{@window_naming_dirty}|#{@window_task}"

func outOfScope(what string) {
	fmt.Fprintf(os.Stderr, "update-icons bake-off: %s is out of scope\n", what)
	os.Exit(3)
}

type icons struct {
	max    int
	agents map[string]bool
	glyph  map[string]string
}

func loadIcons() icons {
	ic := icons{agents: map[string]bool{}, glyph: map[string]string{}}
	for _, line := range strings.Split(iconsTSV, "\n") {
		key, val, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch key {
		case "#max":
			ic.max, _ = strconv.Atoi(val)
		case "#agents":
			for _, a := range strings.Fields(val) {
				ic.agents[a] = true
			}
		default:
			ic.glyph[key] = val
		}
	}
	return ic
}

func normalizeWrapped(cmd string) string {
	if strings.HasPrefix(cmd, ".") && strings.HasSuffix(cmd, "-wrapped") {
		return strings.TrimSuffix(cmd[1:], "-wrapped")
	}
	return cmd
}

// cellWidth mirrors _icon_cell_width in scripts/lib-icons.sh (first rune only).
func cellWidth(s string) int {
	r, _ := utf8.DecodeRuneInString(s)
	cp := int(r)
	switch {
	case r == utf8.RuneError || cp == 0:
		return 2
	case (cp >= 0xE000 && cp <= 0xF8FF) || cp >= 0xF0000:
		return 1
	case cp >= 0x1F000:
		return 2
	case cp >= 0x231A && cp <= 0x231B,
		cp >= 0x23E9 && cp <= 0x23EC,
		cp == 0x23F0 || cp == 0x23F3,
		cp >= 0x25FD && cp <= 0x25FE,
		cp >= 0x2614 && cp <= 0x2615,
		cp >= 0x2648 && cp <= 0x2653,
		cp == 0x267F || cp == 0x2693 || cp == 0x26A1,
		cp >= 0x26AA && cp <= 0x26AB,
		cp >= 0x26BD && cp <= 0x26BE,
		cp >= 0x26C4 && cp <= 0x26C5,
		cp == 0x26CE || cp == 0x26D4 || cp == 0x26EA,
		cp >= 0x26F2 && cp <= 0x26F3,
		cp == 0x26F5 || cp == 0x26FA || cp == 0x26FD,
		cp == 0x2705,
		cp >= 0x270A && cp <= 0x270B,
		cp == 0x2728 || cp == 0x274C || cp == 0x274E,
		cp >= 0x2753 && cp <= 0x2755,
		cp == 0x2757,
		cp >= 0x2795 && cp <= 0x2797,
		cp == 0x27B0 || cp == 0x27BF,
		cp >= 0x2B1B && cp <= 0x2B1C,
		cp == 0x2B50 || cp == 0x2B55:
		return 2
	}
	return 1
}

// buildProcIcons mirrors build_proc_icons: icon string (trailing space per
// icon) and its display width.
func (ic icons) buildProcIcons(procs []string) (string, int) {
	var sb strings.Builder
	dw, count := 0, 0
	for _, p := range procs {
		if count >= ic.max {
			break
		}
		icon := ic.glyph[normalizeWrapped(p)]
		if icon == "" {
			continue
		}
		sb.WriteString(icon)
		sb.WriteByte(' ')
		dw += cellWidth(icon) + 1
		count++
	}
	return sb.String(), dw
}

type window struct {
	key                                                                string
	sess, idx                                                          string
	path, cwd                                                          string
	cwdSet                                                             bool
	branch, task, aiName, display, padded, ago, rename, crew, crewSeen string
	bridge, hasAgent, manual, namingDirty                              string
	procs                                                              []string
	poison                                                             bool
}

type session struct {
	activeIcon, sessionFg string
	activeWin, activeProc string
}

func statOwned(path string) (*syscall.Stat_t, bool) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return nil, false
	}
	return &st, int(st.Uid) == os.Getuid()
}

// checkStateDir is the steady state of claude_status_dir_trusted plus the
// marker-gated claude_prune_stale_state and an empty claude_pane_ids.
func checkStateDir(serverStart, serverPid string) {
	dir := os.Getenv("CLAUDE_STATUS_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/tmp/claude-status-%d", os.Getuid())
	}
	st, owned := statOwned(dir)
	if st == nil || !owned || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		outOfScope("untrusted state dir")
	}
	mk, owned := statOwned(dir + "/.owner-only")
	if mk == nil || !owned || mk.Mode&syscall.S_IFMT != syscall.S_IFREG {
		outOfScope("owner-only marker missing")
	}
	gate, err := os.ReadFile(dir + "/.server_start." + serverPid)
	if err != nil || strings.TrimRight(string(gate), "\n") != serverStart {
		outOfScope("prune sweep")
	}
	for _, sub := range []string{"/panes", "/screen", "/tasks", "/names"} {
		entries, err := os.ReadDir(dir + sub)
		if err == nil && len(entries) > 0 {
			outOfScope("agent state files")
		}
	}
}

func main() {
	if len(os.Args) < 7 {
		fmt.Fprintln(os.Stderr, "usage: update-icons SESSION RESUME_CLAUDE START_TIME RESUME_CAROUSEL FLAVOR PID")
		os.Exit(2)
	}
	invokeName, serverStart, serverPid := os.Args[1], os.Args[3], os.Args[6]
	ic := loadIcons()
	checkStateDir(serverStart, serverPid)

	raw, err := exec.Command("tmux",
		"list-sessions", "-F", "S|#{session_id}|#{session_name}", ";",
		"list-panes", "-a", "-f", "#{!:#{pane_modal_flag}}", "-F", paneFormat).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tmux read: %v\n", err)
		os.Exit(1)
	}

	sessIDOf := map[string]string{}
	sess := map[string]*session{}
	wins := map[string]*window{}
	var order []string
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "S|"); ok {
			sid, sname, _ := strings.Cut(rest, "|")
			sessIDOf[sname] = sid
			continue
		}
		rest, ok := strings.CutPrefix(line, "P|")
		if !ok {
			continue
		}
		f := strings.SplitN(rest, "|", 29)
		if len(f) < 29 {
			continue
		}
		proc := f[5]
		if f[23] != "" {
			proc = f[23]
		}
		wkey := f[1] + ":" + f[2]
		w := wins[wkey]
		paneActive, windowActive := f[10], f[11]
		s := sess[f[1]]
		if s == nil {
			s = &session{}
			sess[f[1]] = s
		}
		s.activeIcon, s.sessionFg = f[18], f[19]
		if w == nil {
			w = &window{
				key: wkey, sess: f[1], idx: f[2], path: f[4], cwd: "",
				branch: f[6], task: f[28], aiName: f[12], display: f[14], padded: f[15],
				ago: f[16], rename: f[17], crew: f[20], crewSeen: f[21],
				bridge: f[22], hasAgent: f[25], manual: f[26], namingDirty: f[27],
			}
			wins[wkey] = w
			order = append(order, wkey)
		}
		if paneActive != "0" && paneActive != "1" {
			w.poison = true
		}
		if !w.cwdSet && f[7] != "1" {
			w.cwd, w.cwdSet = f[4], true
		}
		if windowActive == "1" {
			s.activeWin = f[2]
		}
		if paneActive == "1" && windowActive == "1" {
			s.activeProc = proc
		}
		if proc == "" {
			continue
		}
		dup := false
		for _, p := range w.procs {
			if p == proc {
				dup = true
				break
			}
		}
		if !dup {
			w.procs = append(w.procs, proc)
		}
	}
	invokeSID := sessIDOf[invokeName]

	var cmds strings.Builder
	set := func(scope, target, name, val string) {
		fmt.Fprintf(&cmds, "%s -t '%s' %s '%s'\n", scope, target, name, val)
	}
	targetDW := ic.max*3 + 2
	for _, key := range order {
		w := wins[key]
		hasAgent := ""
		if w.bridge != "1" {
			for _, p := range w.procs {
				if ic.agents[normalizeWrapped(p)] {
					hasAgent = "1"
					break
				}
			}
		}
		if w.task != "" {
			set("set -qw", key, "@window_task", "")
		}
		if w.aiName != "" {
			set("set -qw", key, "@window_ai_name", "")
		}
		if w.crew != w.crewSeen {
			set("set -qw", key, "@crew_seen", w.crew)
		}
		clearNeeded := hasAgent == "" && w.namingDirty != ""
		if !w.poison && w.bridge != "1" && (hasAgent != w.hasAgent || clearNeeded) {
			if hasAgent == "" {
				outOfScope("agent-left naming clear")
			}
			set("set -qw", key, "@window_has_agent", "1")
		}

		if (w.sess == invokeSID && w.idx == sess[invokeSID].activeWin) || w.branch == "" {
			path := w.cwd
			if path == "" {
				path = w.path
			}
			if currentBranch(path) != w.branch {
				outOfScope("branch transition")
			}
		}

		icon, dw := ic.buildProcIcons(w.procs)
		display := strings.TrimSuffix(icon, " ")
		if w.ago != "" {
			set("set -qw", key, "@window_claude_ago", "")
		}
		if display != w.display {
			set("set -qw", key, "@window_icon_display", display)
		}
		if w.bridge == "1" {
			if w.rename == "1" {
				set("set -qw", key, "automatic-rename", "off")
			}
		} else if w.rename != "1" && w.manual != "1" {
			set("set -qw", key, "automatic-rename", "on")
		}
		pad := targetDW - dw
		if pad < 0 {
			pad = 0
		}
		if padded := icon + strings.Repeat(" ", pad); padded != w.padded {
			set("set -qw", key, "@window_icon_padded", padded)
		}
	}
	for id, s := range sess {
		activeIcon := ""
		if s.activeProc != "" {
			activeIcon = ic.glyph[normalizeWrapped(s.activeProc)]
		}
		if activeIcon != s.activeIcon {
			set("set -q", id, "@active_pane_icon", activeIcon)
		}
		if s.sessionFg != "" {
			set("set -q", id, "@claude_session_fg", "")
		}
	}

	if cmds.Len() == 0 {
		return
	}
	write := exec.Command("tmux", "source", "-")
	write.Stdin = strings.NewReader(cmds.String())
	if err := write.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tmux write: %v\n", err)
		os.Exit(1)
	}
}
