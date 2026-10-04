package main

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// renderFrame composes the list frame: search row, list (+ preview), bottom
// rule, hint bar. The lipgloss composition below is the reference; composeFrame
// produces the same bytes without re-measuring every line on each of its three
// passes (Style.Render with a Width, then JoinVertical twice), which was most
// of View()'s ~2ms on a 200x50 popup, on every keypress.
func (m tuiModel) renderFrame() string {
	if s, ok := m.composeFrame(); ok {
		return s
	}
	return m.composeFrameLipgloss()
}

// frameBlocks are the body's stacked blocks: the list, then the separator and
// the preview when shown. Both compositions build the body from these.
func (m tuiModel) frameBlocks() []string {
	blocks := []string{m.renderList()}
	if m.showPreview {
		blocks = append(blocks, m.renderSeparator(), m.preview.View())
	}
	return blocks
}

// composeFrameLipgloss is the original composition, kept as the fallback for
// anything composeFrame will not vouch for and as the test oracle.
func (m tuiModel) composeFrameLipgloss() string {
	body := lipgloss.JoinVertical(lipgloss.Left, m.frameBlocks()...)
	borderColor := m.thmColor("@thm_surface_1", "#45475a", "#9ca0b0")
	bordered := lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(borderColor).
		Render(body)
	return lipgloss.JoinVertical(lipgloss.Left, m.renderSearch(), bordered, m.renderHints())
}

// composeFrame measures each line once, pads it to the popup width and joins
// with the bottom rule that renderSearch already draws in the same colour
// (the search row is bordered exactly as the body is). It declines (ok=false)
// where lipgloss would do more than pad; see padFrameLine.
func (m tuiModel) composeFrame() (string, bool) {
	w := m.width
	if w <= 0 {
		return "", false
	}
	search := strings.Split(m.renderSearch(), "\n")
	rule := search[len(search)-1]
	var bodyLines []string
	for _, block := range m.frameBlocks() {
		bodyLines = append(bodyLines, strings.Split(block, "\n")...)
	}
	hintLines := strings.Split(m.renderHints(), "\n")

	out := make([]string, 0, len(search)+len(bodyLines)+len(hintLines)+1)
	pad := func(lines []string) bool {
		for _, line := range lines {
			padded, ok := padFrameLine(line, w)
			if !ok {
				return false
			}
			out = append(out, padded)
		}
		return true
	}
	if !pad(search) || !pad(bodyLines) {
		return "", false
	}
	out = append(out, rule)
	if !pad(hintLines) {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

// padFrameLine right-pads line with spaces to w cells. ok is false when the
// line is wider than w (lipgloss would wrap it) or holds a byte its wrap and
// width passes treat specially: any control character but ESC, DEL, or the
// Unicode line/paragraph separators.
func padFrameLine(line string, w int) (string, bool) {
	multibyte := false
	hasESC := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if (c < 0x20 && c != 0x1b) || c == 0x7f {
			return "", false
		}
		hasESC = hasESC || c == 0x1b
		multibyte = multibyte || c >= 0x80
	}
	if multibyte && hasSpecialRune(line) {
		return "", false
	}
	sw := len(line)
	if multibyte || hasESC {
		sw = lipgloss.Width(line)
	}
	if sw > w {
		return "", false
	}
	if sw == w {
		return line, true
	}
	return line + strings.Repeat(" ", w-sw), true
}

// hasSpecialRune reports whether s holds a C1 control or U+2028/U+2029.
func hasSpecialRune(s string) bool {
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return true
		}
		s = s[n:]
	}
	return false
}
