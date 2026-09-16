package daemon

import (
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// layoutFlagAlphabet is window_printable_flags' output alphabet
// (window_printable_flags, window.c:1289-1317, pinned tree): activity, bell,
// silence, current, last, marked, modal, zoomed.
const layoutFlagAlphabet = "#!~*-MOZ"

// layoutNotice is what a %layout-change line carries that readLayout would
// otherwise fetch: the unzoomed layout string and the zoom flag.
type layoutNotice struct {
	layout string
	zoomed bool
}

// parseLayoutNotice reads the layout string and zoom flag out of a
// %layout-change line. Fields are checked by shape, not just count: an
// overlong dump empties #{window_layout} and shifts the rest left, and an
// empty flags field is dropped rather than emitted. Anything unexpected —
// including a flag character this alphabet lacks — reports ok=false, so the
// caller falls back to a read rather than act on a misread line.
func parseLayoutNotice(l controlmode.Line) (layoutNotice, bool) {
	switch len(l.Args) {
	case 3:
		if !layoutShaped(l.Args[1]) || !layoutShaped(l.Args[2]) {
			return layoutNotice{}, false
		}
		return layoutNotice{layout: l.Args[1]}, true
	case 4:
		if !layoutShaped(l.Args[1]) || !layoutShaped(l.Args[2]) || !flagsShaped(l.Args[3]) {
			return layoutNotice{}, false
		}
		return layoutNotice{layout: l.Args[1], zoomed: strings.ContainsRune(l.Args[3], 'Z')}, true
	default:
		return layoutNotice{}, false
	}
}

// layoutShaped reports whether s starts like a layout dump: v2 JSON, or v1's
// four-digit lowercase hex checksum, a comma, then at least one more byte.
func layoutShaped(s string) bool {
	if strings.HasPrefix(s, `{"V":`) {
		return true
	}
	if len(s) < 6 || s[4] != ',' {
		return false
	}
	for i := range 4 {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// flagsShaped reports whether s is a non-empty run of layoutFlagAlphabet.
func flagsShaped(s string) bool {
	return s != "" && strings.Trim(s, layoutFlagAlphabet) == ""
}
