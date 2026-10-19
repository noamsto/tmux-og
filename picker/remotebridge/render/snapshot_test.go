package render

import (
	"strings"
	"testing"
)

func TestSeedAltScreenAndCursor(t *testing.T) {
	out := string(Seed([]byte("hello"), 2, 0, true, true, nil, nil, nil))
	if !strings.Contains(out, "\x1b[?1049h") {
		t.Error("should enter alternate screen")
	}
	if !strings.Contains(out, "\x1b[?1h") {
		t.Error("should set application cursor keys")
	}
	if !strings.Contains(out, "hello") {
		t.Error("should include captured content")
	}
	if !strings.HasSuffix(out, "\x1b[1;3H") {
		t.Errorf("should end positioning cursor at row1 col3: %q", out)
	}
}

func TestSeedPlainNoAlt(t *testing.T) {
	out := string(Seed([]byte("x"), 0, 0, false, false, nil, nil, nil))
	if strings.Contains(out, "1049h") || strings.Contains(out, "\x1b[?1h") {
		t.Error("plain seed must not set alt/app-cursor modes")
	}
}

// TestSeedClearsAltScreenAndCursorKeys: a remote that left the alt screen or
// application cursor keys during a gap reports them off, and the seed must
// emit the DECRST clears — asserting only the "on" modes leaves the mirror
// stuck in a mode the remote already left (#803).
func TestSeedClearsAltScreenAndCursorKeys(t *testing.T) {
	out := string(Seed([]byte("x"), 0, 0, false, false, nil, nil, nil))
	if !strings.Contains(out, "\x1b[?1049l\x1b[m") {
		t.Errorf("seed must clear the alt screen: %q", out)
	}
	if !strings.Contains(out, "\x1b[?1l") {
		t.Errorf("seed must clear application cursor keys: %q", out)
	}
	if strings.Contains(out, "\x1b[?1049h") || strings.Contains(out, "\x1b[?1h") {
		t.Errorf("seed must not set alt/app-cursor modes the remote does not have: %q", out)
	}
}

// TestSeedTerminalModes walks every bracketed-paste/focus-reporting
// combination: both modes are cleared before either is set, and each is set
// exactly when the remote reports it on (#804).
func TestSeedTerminalModes(t *testing.T) {
	for _, tc := range []struct {
		bpaste, focus bool
	}{
		{false, false},
		{true, false},
		{false, true},
		{true, true},
	} {
		m := TerminalModes{BracketedPaste: tc.bpaste, FocusReporting: tc.focus}
		out := string(Seed([]byte("x"), 0, 0, false, false, nil, &m, nil))
		for _, mode := range []string{"2004", "1004"} {
			if !strings.Contains(out, "\x1b[?"+mode+"l") {
				t.Errorf("%+v: seed %q missing clear ?%sl", m, out, mode)
			}
		}
		if got := strings.Contains(out, "\x1b[?2004h"); got != tc.bpaste {
			t.Errorf("%+v: seed %q sets ?2004h = %v, want %v", m, out, got, tc.bpaste)
		}
		if got := strings.Contains(out, "\x1b[?1004h"); got != tc.focus {
			t.Errorf("%+v: seed %q sets ?1004h = %v, want %v", m, out, got, tc.focus)
		}
		if l, h := strings.Index(out, "\x1b[?2004l"), strings.Index(out, "\x1b[?2004h"); h >= 0 && l > h {
			t.Errorf("%+v: bracketed-paste clear must precede the set: %q", m, out)
		}
		if l, h := strings.Index(out, "\x1b[?1004l"), strings.Index(out, "\x1b[?1004h"); h >= 0 && l > h {
			t.Errorf("%+v: focus clear must precede the set: %q", m, out)
		}
	}
}

// TestSeedTerminalModesBeforeTheRepaint: the mode clears/sets must precede the
// repaint like the alt/app-cursor clears, so a listener parsing the seed sees
// every DECSET before the 2J/captured content.
func TestSeedTerminalModesBeforeTheRepaint(t *testing.T) {
	m := TerminalModes{BracketedPaste: true, FocusReporting: true}
	out := string(Seed([]byte("x"), 0, 0, false, false, nil, &m, nil))
	paint := strings.Index(out, "\x1b[2J")
	if paint < 0 {
		t.Fatalf("seed has no repaint: %q", out)
	}
	for _, seq := range []string{"\x1b[?2004l", "\x1b[?2004h", "\x1b[?1004l", "\x1b[?1004h"} {
		if at := strings.Index(out, seq); at < 0 || at > paint {
			t.Errorf("seed %q must carry %s before the repaint", out, seq)
		}
	}
}

// TestSeedUnknownTerminalModesLeavesModesAlone: a nil TerminalModes (the
// remote's state could not be read) emits no bracketed-paste or focus DECSET
// at all, set or clear.
func TestSeedUnknownTerminalModesLeavesModesAlone(t *testing.T) {
	out := string(Seed([]byte("x"), 0, 0, false, false, nil, nil, nil))
	for _, mode := range []string{"2004", "1004"} {
		if strings.Contains(out, "\x1b[?"+mode) {
			t.Errorf("seed %q touches mode ?%s with the state unknown", out, mode)
		}
	}
}

// TestSeedClearsModesBeforeTheRepaint: ?1049l restores the saved main screen,
// so it (and the app-cursor clear) must precede the 2J/captured repaint, or
// the restore would clobber the freshly reseeded contents.
func TestSeedClearsModesBeforeTheRepaint(t *testing.T) {
	out := string(Seed([]byte("x"), 0, 0, true, true, nil, nil, nil))
	paint := strings.Index(out, "\x1b[2J")
	if paint < 0 {
		t.Fatalf("seed has no repaint: %q", out)
	}
	for _, clear := range []string{"\x1b[?1049l\x1b[m", "\x1b[?1l"} {
		at := strings.Index(out, clear)
		if at < 0 {
			t.Errorf("seed missing clear %s: %q", clear, out)
			continue
		}
		if at > paint {
			t.Errorf("clear %s must precede the repaint: %q", clear, out)
		}
	}
	if strings.Index(out, "\x1b[?1049l\x1b[m") > strings.Index(out, "\x1b[?1049h") {
		t.Errorf("alt-screen clear must precede the set: %q", out)
	}
	if strings.Index(out, "\x1b[?1l") > strings.Index(out, "\x1b[?1h") {
		t.Errorf("app-cursor clear must precede the set: %q", out)
	}
}

func TestSeedResetsAttrsBeforeErase(t *testing.T) {
	for _, alt := range []bool{false, true} {
		out := string(Seed([]byte("x"), 0, 0, alt, false, nil, nil, nil))
		if !strings.HasPrefix(out, "\x1b[m") {
			t.Errorf("alt=%v: erase must follow an SGR reset, or it fills with the live stream's background: %q", alt, out)
		}
	}
}

// TestSeedResetsAttrsAfterAltScreenRestore: ?1049l restores the pen saved at
// ?1049h, so the erase after it needs its own SGR reset, in both alt cases.
func TestSeedResetsAttrsAfterAltScreenRestore(t *testing.T) {
	for _, alt := range []bool{false, true} {
		out := string(Seed([]byte("x"), 0, 0, alt, false, nil, nil, nil))
		_, after, found := strings.Cut(out, "\x1b[?1049l")
		if !found {
			t.Fatalf("alt=%v: seed has no alt-screen clear: %q", alt, out)
		}
		if !strings.HasPrefix(after, "\x1b[m") {
			t.Errorf("alt=%v: bytes after ?1049l must reset attrs before the erase: %q", alt, out)
		}
	}
}

// TestSeedMouseModes walks every MouseMode combination: all five modes are
// cleared before any is set, tracking granularity sets at most one of
// 1003 > 1002 > 1000 (tmux keeps only the last one set), and each encoding
// extension is set exactly when its flag is.
func TestSeedMouseModes(t *testing.T) {
	for bits := range 1 << 5 {
		m := MouseMode{Standard: bits&1 != 0, Button: bits&2 != 0, All: bits&4 != 0, SGR: bits&8 != 0, UTF8: bits&16 != 0}
		out := string(Seed([]byte("x"), 0, 0, false, false, &m, nil, nil))
		want := map[string]bool{
			"1003": m.All,
			"1002": m.Button && !m.All,
			"1000": m.Standard && !m.Button && !m.All,
			"1006": m.SGR,
			"1005": m.UTF8,
		}
		lastClear := -1
		for mode, set := range want {
			clear := strings.Index(out, "\x1b[?"+mode+"l")
			if clear < 0 {
				t.Errorf("%+v: seed %q missing clear ?%sl", m, out, mode)
			}
			lastClear = max(lastClear, clear)
			if got := strings.Contains(out, "\x1b[?"+mode+"h"); got != set {
				t.Errorf("%+v: seed %q sets ?%sh = %v, want %v", m, out, mode, got, set)
			}
		}
		for mode := range want {
			if h := strings.Index(out, "\x1b[?"+mode+"h"); h >= 0 && h < lastClear {
				t.Errorf("%+v: seed %q sets ?%sh before the last clear", m, out, mode)
			}
		}
	}
}

// TestSeedUnknownMouseModeLeavesModesAlone: a nil MouseMode (the remote's
// state could not be read) emits no mouse DECSET at all, set or clear.
func TestSeedUnknownMouseModeLeavesModesAlone(t *testing.T) {
	out := string(Seed([]byte("x"), 0, 0, true, true, nil, nil, nil))
	for _, mode := range []string{"1000", "1002", "1003", "1005", "1006"} {
		if strings.Contains(out, "\x1b[?"+mode) {
			t.Errorf("seed %q touches mouse mode ?%s with the state unknown", out, mode)
		}
	}
}

// TestSeedWrapMode: DECAWM is set or cleared to the remote's state, and left
// alone when it is unknown — tmux keeps one per pane, shared by the main and
// alternate screens, and ?1049h/l does not restore it, so only the seed can
// undo a ?7l written by a reshape hold.
func TestSeedWrapMode(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		wrap *bool
		want string
	}{
		{"on", &on, "\x1b[?7h"},
		{"off", &off, "\x1b[?7l"},
		{"unknown", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := string(Seed([]byte("x"), 0, 0, false, false, nil, nil, tc.wrap))
			for _, seq := range []string{"\x1b[?7h", "\x1b[?7l"} {
				if got, want := strings.Contains(out, seq), seq == tc.want; got != want {
					t.Errorf("seed %q contains %q = %v, want %v", out, seq, got, want)
				}
			}
		})
	}
}
