package manifest

import (
	"regexp"
	"slices"
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
	case sel == "prompt_box":
		return promptBox(screenText)
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

const (
	// A border is a row that starts with boxBorder: typed rows are indented
	// after `❯ `, so a `───` inside typed text is not at column 0.
	boxBorder  = "───"
	footerRows = 4
)

// promptBox returns the input box and its footer, or "" when no box is found:
// the last `❯` line between a top and bottom border, the bottom border, then the
// non-empty rows after it up to the first blank row (footerRows at most). The
// box is found by its borders, so a typed prompt of any height keeps the footer
// in the region, a task panel drawn after the blank row never enters it, and an
// unbordered `❯` (menu cursor, old echo in scrollback) never anchors it.
func promptBox(s string) string {
	lines := strings.Split(s, "\n")
	for b, line := range slices.Backward(lines) {
		if !strings.HasPrefix(line, boxBorder) {
			continue
		}
		prompt := promptLineAbove(lines[:b])
		if prompt < 0 {
			continue
		}
		block := slices.Clone(lines[prompt : b+1])
		for _, l := range lines[b+1:] {
			if strings.TrimSpace(l) == "" || len(block) >= b+1-prompt+footerRows {
				break
			}
			block = append(block, l)
		}
		return strings.Join(block, "\n")
	}
	return ""
}

// promptLineAbove returns the index of the last `❯`-leading line below the
// nearest border in lines, or -1 when there is no top border or no such line.
func promptLineAbove(lines []string) int {
	prompt := -1
	for i, l := range slices.Backward(lines) {
		if strings.HasPrefix(l, boxBorder) {
			return prompt
		}
		if prompt < 0 && strings.HasPrefix(strings.TrimLeft(l, " \t"), "❯") {
			prompt = i
		}
	}
	return -1
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
