package render

import (
	"bytes"
	"fmt"
)

// MouseMode is which mouse-tracking DECSET modes tmux reports set on a
// pane. Standard/Button/All are mutually exclusive tracking granularity
// (1000/1002/1003); SGR/UTF8 are independent encoding extensions
// (1006/1005) layered on top.
type MouseMode struct {
	Standard, Button, All bool
	SGR, UTF8             bool
}

// TerminalModes is the pane's bracketed-paste and focus-reporting DECSET
// state, seeded clear-then-set like the mouse because a mirror pane learns
// modes only from bytes it is handed and so must have them restored across a
// gap.
//
// tmux exposes both in one comma-separated list, #{pane_private_modes}
// (bracketed paste ?2004, focus reporting ?1004), so the pair is read and
// seeded atomically: there is no per-mode format for focus reporting, and
// #{bracket_paste_flag} (the scalar, from tmux 3.7) would leave focus unknown
// whenever it was the only readable half of the pair.
type TerminalModes struct {
	BracketedPaste bool
	FocusReporting bool
	// CursorVisible is DECTCEM (?25), carried in the same list. Seed writes it
	// last, after the final cursor move, so nothing in the repaint re-shows it.
	CursorVisible bool
	// BidiDirect is BDSM (ANSI mode 8) and BidiAuto is ?2501, both read as
	// tokens 8 and 2501 of the same list. They are set-only: a remote that
	// cannot report them (any stock tmux) lists neither, so off and unknown both
	// emit nothing rather than clearing a mode the mirror may have learned live.
	BidiDirect bool
	BidiAuto   bool
}

// CursorStyle is the DECSCUSR cursor shape ("block", "underline", "bar" or
// "default", tmux's #{cursor_shape}) and blink. "default" is written as
// CSI 0 SP q, which drops the blink bit: the terminal's own default decides.
type CursorStyle struct {
	Shape string
	Blink bool
}

// Extra is pane state a mirror pane learns only from bytes it is handed. Each
// nil field means unknown and leaves the local pane as it is.
//
// Colour is tmux's #{cursor_colour}, already sanitized: "none" and any value
// that is not #rrggbb (colourN, a colour name, default, terminal) reset the
// cursor colour with OSC 112, because the local tmux's OSC 12 only parses
// #rrggbb, rgb: and X11 names.
//
// Key mode (#{pane_key_mode}) is deliberately NOT restored: keyneg strips every
// live CSI > 4 ... m / n (#338), so a seeded mode would go stale when the
// remote app exits and CSI-u-encode Ctrl keys for a shell.
type Extra struct {
	Style  *CursorStyle
	Colour *string
}

// clearScreen blanks the pane and homes the cursor without adding a line to
// its history. Under scroll-on-clear tmux copies the visible screen into
// history on ED2 and on ED0 at home, so every seed would otherwise stack one
// more copy of the screen into the mirror (#1239). Erasing row 1 first leaves
// nothing to copy, and the ED0 from row 2 then no longer starts at home.
const clearScreen = "\x1b[H\x1b[2K\x1b[2;1H\x1b[J\x1b[H"

// Seed paints captured as a full-screen repaint. A nil mouse, modes or wrap
// means the remote's state is unknown, and leaves the local pane's modes as
// they are: a transient read failure must not turn a working mirror
// mouse-deaf, nor clear a paste/focus mode it can no longer read. wrap is
// DECAWM (?7): tmux keeps one per pane, shared by the main and alternate
// screens, and ?1049h/l neither saves nor restores it, so a pane left with
// autowrap off (a reshape hold writes ?7l) stays that way unless a seed sets it.
// A nil extra leaves cursor style and colour alone, the same way.
func Seed(captured []byte, cursorX, cursorY int, altScreen, appCursorKeys bool, mouse *MouseMode, modes *TerminalModes, wrap *bool, extra *Extra) []byte {
	var b bytes.Buffer
	// Both the alt-screen switch and ED erase with the CURRENT background, and
	// the live stream this seed interrupts leaves one set, so without this reset
	// the erase floods the whole pane with that colour.
	b.WriteString("\x1b[m")
	// Clear then set, the same as writeMouseMode: a mode the remote turned off
	// during a gap must not survive the reseed. ?1049l restores the saved main
	// screen, so both clears run before the repaint below.
	b.WriteString("\x1b[?1049l")
	// ?1049l restores the pen saved at ?1049h (a reshape hold, or an earlier
	// alt-screen seed), and the clear below erases with that pen, so reset it again.
	b.WriteString("\x1b[m")
	if altScreen {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[?1l")
	if appCursorKeys {
		b.WriteString("\x1b[?1h")
	}
	if wrap != nil {
		if *wrap {
			b.WriteString("\x1b[?7h")
		} else {
			b.WriteString("\x1b[?7l")
		}
	}
	if modes != nil {
		writeTerminalModes(&b, *modes)
	}
	if mouse != nil {
		writeMouseMode(&b, *mouse)
	}
	if extra != nil {
		writeExtra(&b, *extra)
	}
	b.WriteString(clearScreen)
	b.Write(captured)
	fmt.Fprintf(&b, "\x1b[%d;%dH", cursorY+1, cursorX+1)
	if modes != nil {
		if modes.CursorVisible {
			b.WriteString("\x1b[?25h")
		} else {
			b.WriteString("\x1b[?25l")
		}
	}
	return b.Bytes()
}

// writeTerminalModes clears then sets bracketed paste and focus reporting,
// the same as writeMouseMode: without the clear a mode the remote turned off
// during a gap would survive the reseed; without the set a mode it enabled
// before the mirror attached would never reach the local pane.
func writeTerminalModes(b *bytes.Buffer, modes TerminalModes) {
	b.WriteString("\x1b[?2004l\x1b[?1004l")
	if modes.BracketedPaste {
		b.WriteString("\x1b[?2004h")
	}
	if modes.FocusReporting {
		b.WriteString("\x1b[?1004h")
	}
	if modes.BidiDirect {
		b.WriteString("\x1b[8h")
	}
	if modes.BidiAuto {
		b.WriteString("\x1b[?2501h")
	}
}

// writeMouseMode clears every mouse mode, then sets the true ones: a mode the
// mirror pane picked up earlier (a previous seed, or live output) must not
// survive a seed whose remote has since turned it off.
func writeMouseMode(b *bytes.Buffer, mouse MouseMode) {
	b.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1005l")
	switch {
	case mouse.All:
		b.WriteString("\x1b[?1003h")
	case mouse.Button:
		b.WriteString("\x1b[?1002h")
	case mouse.Standard:
		b.WriteString("\x1b[?1000h")
	}
	if mouse.SGR {
		b.WriteString("\x1b[?1006h")
	}
	if mouse.UTF8 {
		b.WriteString("\x1b[?1005h")
	}
}

// writeExtra restores cursor style and cursor colour.
func writeExtra(b *bytes.Buffer, e Extra) {
	if e.Style != nil {
		if n, ok := decscusr(*e.Style); ok {
			fmt.Fprintf(b, "\x1b[%d q", n)
		}
	}
	if e.Colour != nil {
		if isHexColour(*e.Colour) {
			fmt.Fprintf(b, "\x1b]12;%s\x1b\\", *e.Colour)
		} else {
			b.WriteString("\x1b]112\x1b\\")
		}
	}
}

func decscusr(s CursorStyle) (int, bool) {
	var base int
	switch s.Shape {
	case "default":
		return 0, true
	case "block":
		base = 1
	case "underline":
		base = 3
	case "bar":
		base = 5
	default:
		return 0, false
	}
	if !s.Blink {
		base++
	}
	return base, true
}

func isHexColour(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for _, r := range c[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
