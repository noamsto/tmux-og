package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// usageCache mirrors the normalized JSON the tmux-agent-usage-* provider
// scripts write to usageCacheDir/<agent>.json.
type usageCache struct {
	Windows []usageWindow `json:"windows"`
	Monthly *usageWindow  `json:"monthly"`
	Spend   *usageSpend   `json:"spend"`
}

type usageWindow struct {
	Label   string  `json:"label"`
	Pct     float64 `json:"pct"`
	ResetAt int64   `json:"reset_at,omitempty"`
}

// usageSpend is the dollar figure an agent has spent over Period ("month",
// "cycle"). Label and Period describe the figure for other consumers; the
// segment renders only USD and, when the provider knows a budget, LimitUSD.
type usageSpend struct {
	Label    string   `json:"label"`
	USD      float64  `json:"usd"`
	Period   string   `json:"period"`
	LimitUSD *float64 `json:"limit_usd,omitempty"`
}

const usageCacheDir = "/tmp/og-agent-usage"

// usageAgentOrder fixes the left-to-right agent order in the segment.
var usageAgentOrder = []string{"claude", "codex", "cursor", "pi"}

// loadUsageCaches reads whatever provider caches exist. Missing or malformed
// files are skipped — the poller rewrites them atomically on the next pass.
func loadUsageCaches(dir string) map[string]usageCache {
	out := map[string]usageCache{}
	for _, agent := range usageAgentOrder {
		data, err := os.ReadFile(filepath.Join(dir, agent+".json"))
		if err != nil {
			continue
		}
		var c usageCache
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		out[agent] = c
	}
	return out
}

// openAgents is the display gate: the set of agents (keyed like
// usageAgentOrder) with a local pane running their command. Only local panes
// count — #513 once let a mirror's @bridge_proc open the gate, but per-agent,
// that would show a remote agent's column from a local cache the poller never
// refreshes, so a mirror pane's bridge-renderer command simply matches nothing.
// A failed list-panes marks every agent open — a transient tmux error
// shouldn't blink the segment off.
func openAgents() map[string]bool {
	open := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_current_command}").Output()
	if err != nil {
		for _, agent := range usageAgentOrder {
			open[agent] = true
		}
		return open
	}
	known := map[string]string{"claude": "claude", "codex": "codex", "cursor-agent": "cursor", "pi": "pi"}
	for line := range strings.Lines(string(out)) {
		base := path.Base(strings.TrimSpace(line))
		if m := wrappedRe.FindStringSubmatch(base); m != nil {
			base = m[1]
		}
		if agent, ok := known[base]; ok {
			open[agent] = true
		}
	}
	return open
}

func usageColor(pct float64, a args) string {
	switch {
	case pct >= 90:
		return a.thmRed
	case pct >= 70:
		return a.thmPeach
	default:
		return a.thmGreen
	}
}

func (a args) usageIcon(agent string) string {
	switch agent {
	case "claude":
		return a.iconUsageClaude
	case "codex":
		return a.iconUsageCodex
	case "pi":
		return a.iconUsagePi
	default:
		return a.iconUsageCursor
	}
}

// usageResetGlyph is a 1-cell nerd refresh mark. The previous ↻ (U+21BB) reads
// dense next to the %·label run and often measures 2 cells in terminal fonts.
const usageResetGlyph = "󰑐" // nerd: nf-md-refresh

// usageResetSuffix appends a " <glyph> <dur>" countdown to a nearly-exhausted
// window, the moment the reset time starts to matter. Duration granularity
// matches the window's own: minutes under an hour, hours under two days, then days.
func usageResetSuffix(w usageWindow, now int64) string {
	if w.Pct < 90 || w.ResetAt <= now {
		return ""
	}
	d := w.ResetAt - now
	switch {
	case d < 3600:
		return fmt.Sprintf(" %s %dm", usageResetGlyph, max(d/60, 1))
	case d < 172800:
		return fmt.Sprintf(" %s %dh", usageResetGlyph, d/3600)
	default:
		return fmt.Sprintf(" %s %dd", usageResetGlyph, d/86400)
	}
}

// usageDollars keeps cents only under $10, where they are still significant.
func usageDollars(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// usageSegment renders "<icon> <pct>·<label> … $<usd>[/$<limit>]" per open
// agent with data, joined and trailing-padded for the right-aligned group. The
// monthly window only appears at/above the configured threshold; spend always
// appears when cached, $0 included, with the budget appended when the provider
// knows one; an agent with nothing to show (e.g. an uncapped enterprise tier)
// drops out entirely.
func usageSegment(a args, caches map[string]usageCache, open map[string]bool, now int64) string {
	if a.usageMonthlyThreshold <= 0 {
		return ""
	}
	render := func(w usageWindow) string {
		s := "#[fg=" + usageColor(w.Pct, a) + "]" + fmt.Sprintf("%.0f%%·%s", w.Pct, w.Label)
		if suf := usageResetSuffix(w, now); suf != "" {
			s += "#[fg=" + a.thmSubtext0 + "]" + suf
		}
		return s
	}
	var blocks []string
	for _, agent := range usageAgentOrder {
		c, ok := caches[agent]
		if !ok || !open[agent] {
			continue
		}
		var parts []string
		for _, w := range c.Windows {
			parts = append(parts, render(w))
		}
		if c.Monthly != nil && c.Monthly.Pct >= float64(a.usageMonthlyThreshold) {
			parts = append(parts, render(*c.Monthly))
		}
		if c.Spend != nil {
			s := "#[fg=" + a.thmSubtext0 + "]$" + usageDollars(c.Spend.USD)
			if c.Spend.LimitUSD != nil {
				s += "/$" + usageDollars(*c.Spend.LimitUSD)
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			continue
		}
		blocks = append(blocks, "#[fg="+a.thmSubtext0+"]"+a.usageIcon(agent)+" "+strings.Join(parts, " "))
	}
	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "  ") + "  "
}
