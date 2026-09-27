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
// scripts write to usageCacheDir/<agent>.json. Balance is a fallback tier
// used only when Spend.RemainingUSD is nil (no per-key cap known); the two
// are mutually exclusive by construction since a provider script only ever
// writes one.
type usageCache struct {
	Windows []usageWindow `json:"windows"`
	Monthly *usageWindow  `json:"monthly"`
	Spend   *usageSpend   `json:"spend"`
	Balance *usageBalance `json:"balance,omitempty"`
}

type usageWindow struct {
	Label   string  `json:"label"`
	Pct     float64 `json:"pct"`
	ResetAt int64   `json:"reset_at,omitempty"`
}

// usageSpend is the dollar figure an agent has spent over Period ("month",
// "cycle"). The segment renders USD, LimitUSD when the provider knows a
// budget, and Label as a trailing suffix (e.g. "$1.20 mo") for every agent
// except cursor, whose rendering predates the label suffix and stays
// unchanged. RemainingUSD and RemainingLabel carry a provider's remaining
// per-key cap (e.g. pi's OpenRouter key), rendered as its own "$<amt> left"
// clause; RemainingLabel mirrors Label's day/wk/mo/cap mapping, but describes
// when the remaining figure resets rather than the spend. When a key has no
// cap (RemainingUSD nil), usageCache.Balance is the fallback: a top-level
// account balance rendered as its own distinct "$<amt> acct left" clause so
// it's never confused with the per-key figure.
type usageSpend struct {
	Label          string   `json:"label"`
	USD            float64  `json:"usd"`
	Period         string   `json:"period"`
	LimitUSD       *float64 `json:"limit_usd,omitempty"`
	RemainingUSD   *float64 `json:"remaining_usd,omitempty"`
	RemainingLabel string   `json:"remaining_label,omitempty"`
}

// usageBalance is a provider's top-level account balance, used as a fallback
// spend clause only when the key has no per-key cap (usageSpend.RemainingUSD
// is nil).
type usageBalance struct {
	USDRemaining float64 `json:"usd_remaining"`
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

// parseBridgeUsage decodes a mirror session's @bridge_usage option into the
// same cache shape the local provider caches use. The daemon is the sole
// sanitizer (docs/agents/bridge-shipped-state.md); this only re-types the
// JSON, so empty or malformed input yields nil rather than a partial read.
func parseBridgeUsage(v string) map[string]usageCache {
	if v == "" {
		return nil
	}
	var out map[string]usageCache
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

// usageFor selects the caches and open gate for the usage segment. A mirror
// session (@bridge_host set) renders the remote host's figures from
// @bridge_usage, gated by every key present — the daemon already applied the
// remote's own live open gate — and never touches local caches or localOpen,
// so a mirror never shows local figures. A local session reads the cheap
// local caches first and calls localOpen only once there's data worth gating.
func usageFor(a args, localDir string, localOpen func() map[string]bool, now int64) string {
	if a.bridgeHost != "" {
		caches := parseBridgeUsage(a.bridgeUsage)
		if len(caches) == 0 {
			return ""
		}
		open := make(map[string]bool, len(caches))
		for agent := range caches {
			open[agent] = true
		}
		return usageSegment(a, caches, open, now)
	}
	caches := loadUsageCaches(localDir)
	if len(caches) == 0 {
		return ""
	}
	open := localOpen()
	if len(open) == 0 {
		return ""
	}
	return usageSegment(a, caches, open, now)
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

// usageSegment renders "<icon> <pct>·<label> … $<usd>[/$<limit>][ <label>][ ·
// $<remaining> left[/<label>]]" per open agent with data, joined and
// trailing-padded for the right-aligned group. The monthly window only
// appears at/above the configured threshold; spend always appears when
// cached, $0 included, with the budget appended when the provider knows one
// and, for every agent but cursor, the spend's own Label appended as a suffix
// (cursor's rendering predates the label suffix and is pinned unchanged). The
// "/$<limit>" clause is dropped when RemainingUSD is set and RemainingLabel
// is "cap" — a lifetime cap's remaining figure already implies the limit, so
// showing both is redundant — but kept for a resetting cap (day/wk/mo) or
// when there's no remaining figure at all. RemainingUSD, when set, renders as
// its own "$<amt> left" clause (with "/<label>" appended unless the label is
// "cap"), joined onto the spend clause with " · ". When RemainingUSD is nil
// (no per-key cap known), Balance is used instead as a distinct "$<amt> acct
// left" clause, so it's never confused with the per-key figure; the two
// fallback tiers are mutually exclusive by construction (a provider script
// only ever writes one), and RemainingUSD always wins if both are somehow
// set. An agent with nothing to show (e.g. an uncapped enterprise tier) drops
// out entirely.
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
		var spendClause string
		if c.Spend != nil {
			spendClause = "$" + usageDollars(c.Spend.USD)
			lifetimeCap := c.Spend.RemainingUSD != nil && c.Spend.RemainingLabel == "cap"
			if c.Spend.LimitUSD != nil && !lifetimeCap {
				spendClause += "/$" + usageDollars(*c.Spend.LimitUSD)
			}
			// cursor's rendering predates the label suffix and is pinned
			// unchanged; this is not a data-driven distinction.
			if agent != "cursor" && c.Spend.Label != "" {
				spendClause += " " + c.Spend.Label
			}
			if c.Spend.RemainingUSD != nil {
				v := *c.Spend.RemainingUSD
				left := "$" + usageDollars(v) + " left"
				if v < 0 {
					left = "-$" + usageDollars(-v) + " left"
				}
				if c.Spend.RemainingLabel != "" && c.Spend.RemainingLabel != "cap" {
					left += "/" + c.Spend.RemainingLabel
				}
				spendClause += " · " + left
			} else if c.Balance != nil {
				v := c.Balance.USDRemaining
				acctLeft := "$" + usageDollars(v) + " acct left"
				if v < 0 {
					acctLeft = "-$" + usageDollars(-v) + " acct left"
				}
				spendClause += " · " + acctLeft
			}
		}
		if spendClause != "" {
			parts = append(parts, "#[fg="+a.thmSubtext0+"]"+spendClause)
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
