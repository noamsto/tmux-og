package probekit

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// RuneCellWidth returns the display width of a single rune, handling nerd font
// PUA (1 cell) and deferring to go-runewidth for everything else.
func RuneCellWidth(r rune) int {
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

// IconCellWidth returns the display width of an icon string.
// VS16 emoji are stripped at build time (process-icons.nix) to avoid lipgloss
// width miscalculation (charmbracelet/lipgloss#55, #562).
func IconCellWidth(s string) int {
	w := 0
	for _, r := range s {
		w += RuneCellWidth(r)
	}
	return w
}

// TruncateCells clamps s to max display cells (rune- and width-aware), reserving
// one cell for an ellipsis when it has to cut. Byte slicing would split
// multibyte titles; this counts cells like the rest of the picker.
func TruncateCells(s string, max int) string {
	if IconCellWidth(s) <= max {
		return s
	}
	budget := max - 1
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := RuneCellWidth(r)
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}
