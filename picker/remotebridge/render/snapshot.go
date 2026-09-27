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

// Seed paints captured as a full-screen repaint. A nil mouse means the
// remote's mouse state is unknown, and leaves the local pane's modes as they
// are: a transient read failure must not turn a working mirror mouse-deaf.
func Seed(captured []byte, cursorX, cursorY int, altScreen, appCursorKeys bool, mouse *MouseMode) []byte {
	var b bytes.Buffer
	// Both the alt-screen switch and ED erase with the CURRENT background, and
	// the live stream this seed interrupts leaves one set, so without this reset
	// the erase floods the whole pane with that colour.
	b.WriteString("\x1b[m")
	// Clear-then-set for the alt screen and application cursor keys, the same
	// way writeMouseMode does for the mouse: a mode the remote turned off during
	// a gap (a %pause, or a frame dropped under sink backpressure) must not
	// survive the reseed. ?1049l restores the saved main screen, so the clears
	// run BEFORE the repaint below and cannot clobber it; the sets re-enter the
	// modes the remote is still in.
	b.WriteString("\x1b[?1049l")
	if altScreen {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[?1l")
	if appCursorKeys {
		b.WriteString("\x1b[?1h")
	}
	if mouse != nil {
		writeMouseMode(&b, *mouse)
	}
	b.WriteString("\x1b[2J\x1b[H") // clear + home
	b.Write(captured)
	fmt.Fprintf(&b, "\x1b[%d;%dH", cursorY+1, cursorX+1)
	return b.Bytes()
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
