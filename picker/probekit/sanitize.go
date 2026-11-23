package probekit

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const StatusTextMaxRunes = 200

// SanitizeStatusText makes remote-derived text safe for a terminal sink: the
// daemon's stripWindowName targets a tmux format instead and keeps C1 and bidi
// controls, which are exactly the threats here (0x9b is a one-byte CSI).
func SanitizeStatusText(s string) string {
	var b strings.Builder
	runes := 0
	space := false
	for i := 0; i < len(s) && runes < StatusTextMaxRunes; {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == 0x1b:
			i = skipEscape(s, i)
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			runes++
			space = false
		}
		if runes < StatusTextMaxRunes {
			b.WriteRune(r)
			runes++
		}
	}
	return strings.TrimSpace(b.String())
}

// skipEscape returns the index just past the escape sequence whose ESC ended
// at i: a CSI through its final byte, a string sequence (OSC, DCS, SOS, PM,
// APC) through BEL or ST, or else the one character after ESC.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		for i++; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return i
	case ']', 'P', 'X', '^', '_':
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return i
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return i + size
}

// DisplayNameCells caps a remote session or window name on a row.
const DisplayNameCells = 40

// DisplayName is the one place a remote session or window name is made
// render-safe; the raw name stays for actions.
func DisplayName(s string) string {
	return TruncateCells(SanitizeStatusText(s), DisplayNameCells)
}
