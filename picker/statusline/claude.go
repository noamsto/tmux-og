package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// statePalette holds the per-state catppuccin hues for one theme, mirroring
// the H_* values in lib-claude.sh setup_claude_colors.
type statePalette struct {
	waiting, compacting, processing, done, idle, errorC, denied string
}

func claudePalette(theme string) statePalette {
	if theme == "light" { // Latte
		return statePalette{
			waiting: "#fe640b", compacting: "#04a5e5", processing: "#179299",
			done: "#40a02b", idle: "#6c6f85", errorC: "#d20f39", denied: "#df8e1d",
		}
	}
	// Mocha (default)
	return statePalette{
		waiting: "#fab387", compacting: "#89dceb", processing: "#94e2d5",
		done: "#a6e3a1", idle: "#6c7086", errorC: "#f38ba8", denied: "#f9e2af",
	}
}

func (p statePalette) hue(state string) string {
	switch state {
	case "waiting":
		return p.waiting
	case "compacting":
		return p.compacting
	case "processing":
		return p.processing
	case "done":
		return p.done
	case "idle":
		return p.idle
	case "error":
		return p.errorC
	case "denied":
		return p.denied
	}
	return ""
}

// fadedHue eases a state's hue toward the dim idle hue by pct (0..100).
// unseen pins to full color. Empty string for an unknown state.
func (p statePalette) fadedHue(state string, pct int, unseen bool) string {
	base := p.hue(state)
	if base == "" {
		return ""
	}
	if unseen {
		pct = 0
	}
	if pct <= 0 {
		return base
	}
	return fadeHex(base, p.idle, pct)
}

// fadeHex linearly interpolates between two #rrggbb colors; pct 0 = from, 100 = to.
func fadeHex(from, to string, pct int) string {
	fr, fg, fb := hexBytes(from)
	tr, tg, tb := hexBytes(to)
	return fmt.Sprintf("#%02x%02x%02x",
		fr+(tr-fr)*pct/100,
		fg+(tg-fg)*pct/100,
		fb+(tb-fb)*pct/100)
}

func hexBytes(h string) (int, int, int) {
	r, _ := strconv.ParseInt(h[1:3], 16, 0)
	g, _ := strconv.ParseInt(h[3:5], 16, 0)
	b, _ := strconv.ParseInt(h[5:7], 16, 0)
	return int(r), int(g), int(b)
}

type counts struct {
	processing, waiting, compacting, done, idle, errorN, denied, total int
}

func (c counts) priorityState() string {
	switch {
	case c.errorN > 0:
		return "error"
	case c.waiting > 0:
		return "waiting"
	case c.denied > 0:
		return "denied"
	case c.compacting > 0:
		return "compacting"
	case c.processing > 0:
		return "processing"
	case c.done > 0:
		return "done"
	case c.idle > 0:
		return "idle"
	}
	return ""
}

func (c *counts) tally(state string) {
	c.total++
	switch state {
	case "processing":
		c.processing++
	case "waiting":
		c.waiting++
	case "compacting":
		c.compacting++
	case "done":
		c.done++
	case "idle":
		c.idle++
	case "error":
		c.errorN++
	case "denied":
		c.denied++
	}
}

// fadePct mirrors read_pane_state: bright until the state's staleness threshold,
// then linear ramp to 100 over 45s.
func fadePct(state string, now, ts int64) int {
	if ts == 0 {
		return 0
	}
	const fadeDuration = 45
	start := map[string]int64{
		"waiting": 30, "compacting": 60, "processing": 300,
		"done": 60, "error": 120, "denied": 60,
	}[state]
	if start == 0 {
		return 0
	}
	age := now - ts
	if age <= start {
		return 0
	}
	if age >= start+fadeDuration {
		return 100
	}
	return int((age - start) * 100 / fadeDuration)
}

type sessionAgg struct {
	counts  counts
	minFade int
	unseen  bool
	issues  []string
	lastTs  int64
}

// screenOverrideMaxAge matches read_pane_state / picker screenOverrideMaxAge:
// only hook states a missed completion hook can leave stuck are eligible.
// waiting/error/denied stay hook-owned. Constants match fadePct's start table.
func screenOverrideMaxAge(state string) int64 {
	switch state {
	case "compacting":
		return 60
	case "processing":
		return 300
	case "done":
		return 60
	}
	return 0
}

type paneFile struct {
	state, sess string
	ts          int64
	unseen      bool
	ok          bool
}

func readPaneFile(path string) paneFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return paneFile{}
	}
	var pf paneFile
	pf.ok = true
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			pf.state = v
		case "session":
			pf.sess = v
		case "timestamp":
			pf.ts, _ = strconv.ParseInt(v, 10, 64)
		case "unseen":
			pf.unseen = v == "1"
		}
	}
	return pf
}

// aggregateSession unions <dir>/panes and <dir>/screen. Hook-first, with the
// same stale-active screen override as read_pane_state. Screen-only panes (and
// a screen override) join this session only when liveIDs says the pane is here;
// a fresh hook still filters on session=.
func aggregateSession(dir, session string, now int64, liveIDs map[string]bool) sessionAgg {
	agg := sessionAgg{minFade: 100}
	ids := map[string]bool{}
	for _, sub := range []string{"panes", "screen"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ids[e.Name()] = true
		}
	}
	seen := map[string]bool{}
	for id := range ids {
		hook := readPaneFile(filepath.Join(dir, "panes", id))
		screen := readPaneFile(filepath.Join(dir, "screen", id))
		var state, sess string
		var ts int64
		var unseen bool
		if hook.ok {
			state, ts, sess, unseen = hook.state, hook.ts, hook.sess, hook.unseen
			if maxAge := screenOverrideMaxAge(state); maxAge > 0 && now-hook.ts > maxAge && screen.state != "" {
				state, ts, unseen = screen.state, screen.ts, false
				if liveIDs[id] {
					sess = session
				} else {
					sess = ""
				}
			}
			if state == "" || sess != session {
				continue
			}
		} else {
			if !liveIDs[id] || screen.state == "" {
				continue
			}
			state, ts, unseen = screen.state, screen.ts, false
		}
		agg.counts.tally(state)
		if f := fadePct(state, now, ts); f < agg.minFade {
			agg.minFade = f
		}
		if ts > agg.lastTs {
			agg.lastTs = ts
		}
		if unseen {
			agg.unseen = true
		}
		for _, issue := range readIssueFile(filepath.Join(dir, "issues", id)) {
			if issue != "" && !seen[issue] {
				seen[issue] = true
				agg.issues = append(agg.issues, issue)
			}
		}
	}
	return agg
}

func readIssueFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	line, _, _ := strings.Cut(string(data), "\n") // bash collect_pane_issues reads only the first line
	return strings.Split(strings.TrimSpace(line), ",")
}

func formatIssueList(max int, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	if len(ids) <= max {
		return strings.Join(ids, " ")
	}
	return strings.Join(ids[:max], " ") + " +" + strconv.Itoa(len(ids)-max)
}

var spinnerFrames = []string{"󰪞", "󰪟", "󰪠", "󰪡", "󰪢", "󰪣", "󰪤", "󰪥"}

func stateIcon(state string, now int64) string {
	switch state {
	case "processing":
		return spinnerFrames[now%int64(len(spinnerFrames))]
	case "waiting":
		return "󰔟"
	case "compacting":
		return "󰡍"
	case "done":
		return "󰸞"
	case "idle":
		return "󰒲"
	case "error":
		return "󰅚"
	case "denied":
		return "󰔟"
	}
	return ""
}

// haltedStates carry a "last active" time: the session has stopped working, so
// the timestamp is when it halted. Active states (processing/waiting/compacting/
// denied) are live — their icon already conveys them, no time shown.
var haltedStates = map[string]bool{"idle": true, "done": true, "error": true}

// relAgo formats an age in seconds as a single compact unit: 47s, 5m, 2h, 3d.
func relAgo(secs int64) string {
	switch {
	case secs < 60:
		return strconv.FormatInt(secs, 10) + "s"
	case secs < 3600:
		return strconv.FormatInt(secs/60, 10) + "m"
	case secs < 86400:
		return strconv.FormatInt(secs/3600, 10) + "h"
	default:
		return strconv.FormatInt(secs/86400, 10) + "d"
	}
}

// claudeSegment mirrors `claude-status --session <s> --format icon-color`.
func claudeSegment(dir, session, theme string, now int64, liveIDs map[string]bool) string {
	agg := aggregateSession(dir, session, now, liveIDs)
	if agg.counts.total == 0 {
		return ""
	}
	state := agg.counts.priorityState()
	icon := stateIcon(state, now)
	if icon == "" {
		return ""
	}
	pal := claudePalette(theme)
	hue := pal.fadedHue(state, agg.minFade, agg.unseen)
	out := "#[fg=" + hue + "]" + icon + "#[fg=default] "
	if haltedStates[state] && agg.lastTs > 0 && now > agg.lastTs {
		out += "#[fg=" + pal.idle + "]" + relAgo(now-agg.lastTs) + "#[fg=default] "
	}
	if list := formatIssueList(3, agg.issues); list != "" {
		out += "#[fg=" + pal.idle + "]" + list + "#[fg=default] "
	}
	return out
}

// listSessionPaneIDs returns pane ids (without the leading %) in session.
// An empty map on any tmux error — fail closed, never invent membership.
func listSessionPaneIDs(session string) map[string]bool {
	ids := map[string]bool{}
	if session == "" {
		return ids
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-s", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		return ids
	}
	for line := range strings.Lines(string(out)) {
		id := strings.TrimPrefix(strings.TrimSpace(line), "%")
		if id != "" {
			ids[id] = true
		}
	}
	return ids
}
