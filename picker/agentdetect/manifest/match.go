package manifest

import (
	"regexp"
	"strings"
)

// Match resolves a pane's screen into its winning state plus any counted
// flags. Flags are evaluated independently of the state — a background shell
// outlives the turn that started it, so it must survive the pane falling back
// to idle — and are dropped wholesale when no rule matches, since a pane with
// no agent has nothing to count.
func Match(m Manifest, screenText, title string, altScreen bool) (string, map[string]int, bool) {
	if altScreen {
		return "", nil, false
	}
	for _, r := range m.Rules {
		region := regionText(r.Region, screenText, title)
		if ruleMatches(r, region) {
			if r.State == "skip" {
				return "", nil, false
			}
			return r.State, matchFlags(m, screenText, title), true
		}
	}
	return "", nil, false
}

func matchFlags(m Manifest, screenText, title string) map[string]int {
	var out map[string]int
	for _, f := range m.Flags {
		re, err := regexp.Compile("(?i)" + f.Regex)
		if err != nil {
			continue
		}
		sub := re.FindStringSubmatch(regionText(f.Region, screenText, title))
		if len(sub) < 2 {
			continue
		}
		n := atoiDefault(sub[1], 0)
		if n <= 0 {
			continue
		}
		if out == nil {
			out = make(map[string]int, len(m.Flags))
		}
		out[f.Name] = n
	}
	return out
}

func regionText(sel, screenText, title string) string {
	switch {
	case sel == "title":
		return title
	case sel == "whole":
		return joinWrapped(screenText)
	case strings.HasPrefix(sel, "last_lines:"):
		n := atoiDefault(strings.TrimPrefix(sel, "last_lines:"), 1)
		return lastNonEmpty(screenText, n)
	case strings.HasPrefix(sel, "prompt_block:"):
		return promptBlock(screenText, atoiDefault(strings.TrimPrefix(sel, "prompt_block:"), 1))
	default:
		return screenText
	}
}

func ruleMatches(r Rule, region string) bool {
	lc := strings.ToLower(region)
	for _, s := range r.Contains {
		if !strings.Contains(lc, strings.ToLower(s)) {
			return false
		}
	}
	if r.Regex != "" {
		if re, err := regexp.Compile("(?i)" + r.Regex); err != nil || !re.MatchString(region) {
			return false
		}
	}
	for _, n := range r.Not {
		if predMatches(n, lc) {
			return false
		}
	}
	return len(r.Contains) > 0 || r.Regex != "" || len(r.Not) > 0
}

func predMatches(p Predicate, lc string) bool {
	for _, s := range p.Contains {
		if !strings.Contains(lc, strings.ToLower(s)) {
			return false
		}
	}
	if p.Regex != "" {
		if re, err := regexp.Compile("(?i)" + p.Regex); err != nil || !re.MatchString(lc) {
			return false
		}
	}
	for _, n := range p.Not {
		if predMatches(n, lc) {
			return false
		}
	}
	return len(p.Contains) > 0 || p.Regex != "" || len(p.Not) > 0
}

func joinWrapped(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func lastNonEmpty(s string, n int) string {
	lines := strings.Split(s, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append([]string{lines[i]}, kept...)
		}
	}
	return strings.Join(kept, "\n")
}

// promptBlock returns the last `❯` line that has a box border within the next
// four lines, plus the following non-empty lines up to n in total, or "" when
// no such prompt exists. Anchoring on the prompt box keeps rows drawn below the
// footer (a task panel) from shifting it, and excludes scrollback above it. A
// `❯` without a nearby border (menu cursor, panel row) is not a prompt.
func promptBlock(s string, n int) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), "❯") || !borderWithin(lines[i+1:], 4) {
			continue
		}
		block := []string{lines[i]}
		for _, l := range lines[i+1:] {
			if len(block) >= n {
				break
			}
			if strings.TrimSpace(l) != "" {
				block = append(block, l)
			}
		}
		return strings.Join(block, "\n")
	}
	return ""
}

func borderWithin(lines []string, n int) bool {
	for _, l := range lines[:min(n, len(lines))] {
		if strings.Contains(l, "───") {
			return true
		}
	}
	return false
}

func atoiDefault(s string, d int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return d
		}
		n = n*10 + int(c-'0')
	}
	if s == "" {
		return d
	}
	return n
}
