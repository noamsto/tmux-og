package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/claudestatus"
)

// cardSep separates the chained commands' outputs in loadSessionCard's one tmux
// invocation. Only the first two occurrences split: the capture section comes
// last and is untrusted, so a pane that prints the separator cannot add one.
const cardSep = "@@og-card-sep@@"

// cardRowFields is windowsFormat's 38 fields plus window_activity and pane_id.
// The count is exact so a '|' in a path or name fails the row closed, like the
// window picker's parse.
const cardRowFields = 40

// sessionCardArgv builds the card's single tmux invocation: the session's pane
// rows, its path, then a capture of capTarget. sess is addressed with the `=`
// exact-match prefix.
func sessionCardArgv(sess, capTarget string) []string {
	return []string{
		"list-panes", "-s", "-t", "=" + sess, "-f", notModalFilter, "-F",
		windowsFormat + "|#{window_activity}|#{pane_id}",
		";", "display-message", "-p", cardSep,
		";", "display-message", "-p", "-t", "=" + sess, "#{session_path}",
		";", "display-message", "-p", cardSep,
		";", "capture-pane", "-t", capTarget, "-p", "-e",
	}
}

// cardWindow is one roster line's data.
type cardWindow struct {
	w        windowData
	activity int64
	detail   string // second line for a blocked agent, "" when none
}

// sessionCard is everything renderSessionCard draws.
type sessionCard struct {
	name    string
	path    string
	windows []cardWindow
	capture string
}

var cardPaneIDRe = regexp.MustCompile(`^[0-9]+$`)

// parseSessionCard turns sessionCardArgv's output into a card. ok is false
// when the output is not the expected three sections or has no window rows.
func parseSessionCard(out string) (card sessionCard, panes map[string]paneMapping, ok bool) {
	rowsPart, rest, found := strings.Cut("\n"+out, "\n"+cardSep+"\n")
	if !found {
		return sessionCard{}, nil, false
	}
	pathPart, capture, found := strings.Cut(rest, "\n"+cardSep+"\n")
	if !found {
		// An empty session path leaves "\nSEP\n" glued to the first separator.
		pathPart, capture, found = strings.Cut("\n"+rest, "\n"+cardSep+"\n")
		if !found {
			return sessionCard{}, nil, false
		}
	}

	var rows []string
	activity := map[int]int64{}
	panes = map[string]paneMapping{}
	for line := range strings.SplitSeq(strings.TrimSpace(rowsPart), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != cardRowFields {
			continue
		}
		idx, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		if a, err := strconv.ParseInt(parts[38], 10, 64); err == nil {
			activity[idx] = max(activity[idx], a)
		}
		if id := strings.TrimPrefix(parts[39], "%"); cardPaneIDRe.MatchString(id) {
			panes[id] = paneMapping{session: parts[0], winIdx: idx}
		}
		rows = append(rows, strings.Join(parts[:38], "|"))
	}
	if len(rows) == 0 {
		return sessionCard{}, nil, false
	}

	windows := windowsFromRows(rows)
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].index < windows[j].index })
	card.windows = make([]cardWindow, len(windows))
	for i, w := range windows {
		card.windows[i] = cardWindow{w: w, activity: activity[w.index]}
	}
	card.name = windows[0].session
	card.path = strings.TrimSpace(pathPart)
	// A mirror's own session_path is the launcher's cwd.
	if windows[0].bridgeWin {
		card.path = windows[0].path
	}
	card.capture = strings.TrimRight(capture, "\n ")
	return card, panes, true
}

// attachCardAgents merges agent state into the card's windows and, for a
// window whose agent is blocked, reads the pane's task self-report as the
// detail line. Reads only trusted local state files; no forks.
func attachCardAgents(windows []cardWindow, panes map[string]paneMapping) {
	if len(windows) == 0 {
		return
	}
	root := claudestatus.Dir()
	if !claudestatus.Trusted(root) {
		return
	}
	agentPanes := collectAgentPanesFrom(
		filepath.Join(root, "panes"), filepath.Join(root, "screen"), filepath.Join(root, "issues"),
		panes, time.Now().Unix(),
	)
	byWin := aggregateAgentByWindow(agentPanes)
	// The lowest pane id wins, so the detail does not flip with map order.
	sort.Slice(agentPanes, func(i, j int) bool {
		a, _ := strconv.Atoi(agentPanes[i].paneID)
		b, _ := strconv.Atoi(agentPanes[j].paneID)
		return a < b
	})
	for i := range windows {
		cw := &windows[i]
		w := &cw.w
		cc, ok := byWin[fmt.Sprintf("%s:%d", w.session, w.index)]
		if !ok {
			continue
		}
		w.agent = *cc
		state := agentPriority(*cc)
		if state != "waiting" && state != "denied" && state != "error" {
			continue
		}
		for _, p := range agentPanes {
			if p.session != w.session || p.winIdx != w.index || p.state != state || !cardPaneIDRe.MatchString(p.paneID) {
				continue
			}
			task := ""
			if data, err := os.ReadFile(filepath.Join(root, "tasks", p.paneID)); err == nil { //nolint:gosec // G304: path built from a validated numeric pane id under the trusted status dir
				task = sanitizeStatusText(string(data))
			}
			cw.detail = agentStateLabel[state]
			if task != "" {
				cw.detail += " · " + task
			}
			break
		}
	}
}

// cardRun runs the card's tmux invocation; a test seam.
var cardRun = func(argv []string) ([]byte, error) {
	return exec.Command("tmux", argv...).Output() //nolint:gosec // G204: fixed binary, argv passed without a shell
}

// loadSessionCard fetches and renders the card for sess in one tmux call. ok
// is false when tmux failed the chain or returned no windows, and the caller
// falls back to the plain pane capture.
func loadSessionCard(sess, capTarget string, opts map[string]string, theme string, width, height int) (string, bool) {
	out, err := cardRun(sessionCardArgv(sess, capTarget))
	if err != nil {
		return "", false
	}
	card, panes, ok := parseSessionCard(string(out))
	if !ok {
		return "", false
	}
	attachCardAgents(card.windows, panes)
	return renderSessionCard(card, opts, theme, width, height, time.Now().Unix()), true
}

// cardAge is a compact last-activity age ("now", "5m", "3h", "2d"); the window
// roster has no room for formatSnapshotAge's " ago".
func cardAge(now, ts int64) string {
	if ts <= 0 {
		return ""
	}
	d := time.Duration(now-ts) * time.Second
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// renderSessionCard draws the window roster card at width cells. Every
// untrusted string passes sanitizeStatusText before it is sized or coloured,
// and every emitted line is clamped to width.
func renderSessionCard(c sessionCard, opts map[string]string, theme string, width, height int, now int64) string {
	if width <= 0 {
		return ""
	}
	thm := func(env, opt, fallback string) string { return ansiFg(envOrMap(env, opts, opt, fallback)) }
	cMauve := thm("THM_MAUVE", "@thm_mauve", "#cba6f7")
	cGreen := thm("THM_GREEN", "@thm_green", "#a6e3a1")
	cDim := thm("THM_SUBTEXT_0", "@thm_subtext_0", "#a6adc8")
	cFaint := thm("THM_OVERLAY_1", "@thm_overlay_1", "#7f849c")
	cPeach := thm("THM_PEACH", "@thm_peach", "#fab387")
	prCols := prColors{
		success: cGreen, failure: thm("THM_RED", "@thm_red", "#f38ba8"), pending: cPeach, merged: cMauve,
		closed: thm("THM_OVERLAY_0", "@thm_overlay_0", "#6c7086"), required: thm("THM_OVERLAY_0", "@thm_overlay_0", "#6c7086"),
		underline: "\033[4m", reset: "\033[0m",
	}
	const reset = "\033[0m"
	const dim = "\033[2m"
	clamp := func(s string) string { return truncateVisibleWidth(s, width) }

	// Header.
	path := sanitizeStatusText(c.path)
	if home := os.Getenv("HOME"); home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		path = "~" + path[len(home):]
	}
	count := fmt.Sprintf("%d windows", len(c.windows))
	if len(c.windows) == 1 {
		count = "1 window"
	}
	host := ""
	if len(c.windows) > 0 {
		host = sanitizeStatusText(c.windows[0].w.bridgeHost)
	}
	head := cMauve + truncateCells(sanitizeStatusText(sessionDisplayName(c.name, host)), max(width, 1)) + reset + cDim + "  ·  " + count
	if path != "" {
		head += "  ·  " + path
	}
	lines := []string{clamp(head + reset), ""}

	// Cells.
	type cardLine struct {
		idx, marker, name, icons, branch, badge, age, detail string
		iconDW, badgeDW                                      int
	}
	rows := make([]cardLine, len(c.windows))
	idxDW, nameDW, iconDW, branchDW, badgeDW, ageDW := 0, 0, 0, 0, 0, 0
	for i, cw := range c.windows {
		w := cw.w
		r := &rows[i]
		r.idx = strconv.Itoa(w.index)
		switch {
		case w.active && len(c.windows) > 1:
			r.marker = cGreen + "▸" + reset
		default:
			r.marker = " "
		}
		r.name = truncateCells(sanitizeStatusText(w.name), 24)
		icons, dw := buildProcIcons(w.procs, maxIconsPicker)
		r.icons, r.iconDW = appendAgentIcon(icons, dw, w.agent, theme, dim, reset)
		r.iconDW -= trailingSpaces(r.icons)
		br := w.branch
		if w.bridgeWin {
			br = w.bridgeName
		}
		r.branch = truncateCells(sanitizeStatusText(br), 28)
		var badge []string
		if id := strings.TrimSpace(sanitizeStatusText(w.labelID)); id != "" {
			badge = append(badge, cMauve+id+reset)
			r.badgeDW += iconCellWidth(id)
		}
		pr := sanitizeStatusText(w.prPlain)
		if b := colorPRBadge(pr, w.prState, w.prCheck, w.prMergeable, w.prReview, w.prAutoMerge, prCols); b != "" {
			badge = append(badge, b)
			r.badgeDW += iconCellWidth(strings.TrimSpace(pr))
		}
		if len(badge) == 2 {
			r.badgeDW++
		}
		r.badge = strings.Join(badge, " ")
		r.age = cardAge(now, cw.activity)
		if cw.detail != "" {
			r.detail = sanitizeStatusText(cw.detail)
		}
		idxDW = max(idxDW, len(r.idx))
		nameDW = max(nameDW, iconCellWidth(r.name))
		iconDW = max(iconDW, r.iconDW)
		branchDW = max(branchDW, iconCellWidth(r.branch))
		badgeDW = max(badgeDW, r.badgeDW)
		ageDW = max(ageDW, len(r.age))
	}

	// Fit: name and branch give way, the fixed columns never do. The roster
	// is idx, marker, then each present column separated by a space.
	fixed := func() int {
		n := idxDW + 1 + 1 // idx, gap, marker
		for _, w := range []int{nameDW, iconDW, branchDW, badgeDW, ageDW} {
			if w > 0 {
				n += 1 + w
			}
		}
		return n
	}
	for over := fixed() - width; over > 0; over = fixed() - width {
		switch {
		case branchDW > 8:
			branchDW = max(8, branchDW-over)
		case nameDW > 8:
			nameDW = max(8, nameDW-over)
		case branchDW > 0:
			branchDW = 0
		default:
			over = 0 // fixed columns stay; the per-line clamp covers the rest
		}
		if over == 0 {
			break
		}
	}

	for _, r := range rows {
		var b strings.Builder
		b.WriteString(cFaint + strings.Repeat(" ", idxDW-len(r.idx)) + r.idx + reset + " " + r.marker)
		cell := func(s string, dw, col int, color string) {
			if col == 0 {
				return
			}
			s = strings.TrimRight(s, " ")
			b.WriteString(" ")
			if color != "" && s != "" {
				b.WriteString(color + s + reset)
			} else {
				b.WriteString(s)
			}
			b.WriteString(strings.Repeat(" ", max(0, col-dw)))
		}
		name := truncateCells(r.name, max(nameDW, 1))
		cell(name, iconCellWidth(name), nameDW, "")
		cell(r.icons, r.iconDW, iconDW, "")
		br := truncateCells(r.branch, max(branchDW, 1))
		cell(br, iconCellWidth(br), branchDW, cFaint)
		cell(r.badge, r.badgeDW, badgeDW, "")
		if ageDW > 0 {
			b.WriteString(" " + strings.Repeat(" ", ageDW-len(r.age)) + cDim + r.age + reset)
		}
		lines = append(lines, clamp(b.String()))
		if r.detail != "" {
			lines = append(lines, clamp(strings.Repeat(" ", idxDW+5)+cPeach+r.detail+reset))
		}
	}

	// Active-pane tail.
	label := "── active pane "
	lines = append(lines, "", clamp(cFaint+label+strings.Repeat("─", max(0, width-iconCellWidth(label)))+reset))
	capLines := strings.Split(stripStringEscapes(c.capture), "\n")
	room := max(height-len(lines), 3)
	if len(capLines) > room {
		capLines = capLines[len(capLines)-room:]
	}
	for _, l := range capLines {
		// Reset background per line, as the plain capture preview does, so a
		// pane's colour cannot bleed into the viewport padding.
		lines = append(lines, truncateVisibleWidth(l, width)+"\033[49m")
	}
	return strings.Join(lines, "\n")
}

// trailingSpaces counts the padding buildProcIcons/appendAgentIcon leave after
// the last icon, which is not part of the cell's display width.
func trailingSpaces(s string) int {
	return len(s) - len(strings.TrimRight(s, " "))
}
