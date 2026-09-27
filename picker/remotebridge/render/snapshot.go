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
}

// Seed paints captured as a full-screen repaint. A nil mouse or nil modes
// means the remote's state is unknown, and leaves the local pane's modes as
// they are: a transient read failure must not turn a working mirror
// mouse-deaf, nor clear a paste/focus mode it can no longer read.
func Seed(captured []byte, cursorX, cursorY int, altScreen, appCursorKeys bool, mouse *MouseMode, modes *TerminalModes) []byte {
	var b bytes.Buffer
	// Both the alt-screen switch and ED erase with the CURRENT background, and
	// the live stream this seed interrupts leaves one set, so without this reset
	// the erase floods the whole pane with that colour.
	b.WriteString("\x1b[m")
	// Clear then set, the same as writeMouseMode: a mode the remote turned off
	// during a gap must not survive the reseed. ?1049l restores the saved main
	// screen, so both clears run before the repaint below.
	b.WriteString("\x1b[?1049l")
	if altScreen {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[?1l")
	if appCursorKeys {
		b.WriteString("\x1b[?1h")
	}
	if modes != nil {
		writeTerminalModes(&b, *modes)
	}
	if mouse != nil {
		writeMouseMode(&b, *mouse)
	}
	b.WriteString("\x1b[2J\x1b[H") // clear + home
	b.Write(captured)
	fmt.Fprintf(&b, "\x1b[%d;%dH", cursorY+1, cursorX+1)
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
