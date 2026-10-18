package daemon

import (
	"fmt"
	"io"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// predictModel is the predictor's local copy of one remote pane's screen,
// kept by a headless vt emulator. Everything but the drain goroutine runs on
// the owning sink's pump goroutine, so the flags are plain bools.
type predictModel struct {
	e    *vt.Emulator
	w, h int

	// hidden, insert and origin mirror the remote's DECTCEM, IRM and DECOM.
	hidden, insert, origin bool
	// slotUsed is set once the app saves the cursor itself (ESC 7 / CSI s):
	// the predictor's paint uses the same single save slot.
	slotUsed bool
	// poisoned is set by a sequence too costly to model; every later feed fails.
	poisoned bool

	title titleFilter
	fbuf  []byte

	// drained closes when the goroutine servicing the emulator's reply pipe exits.
	drained chan struct{}
}

// newPredictModel builds a model of a w x h pane from a render.Seed payload
// and the modes the seed does not carry. It returns nil if vt rejects any of
// the bytes.
func newPredictModel(w, h int, seed []byte, m predictModes) *predictModel {
	e := vt.NewEmulator(w, h)
	e.SetScrollbackSize(0)
	pm := &predictModel{e: e, w: w, h: h, drained: make(chan struct{})}

	e.RegisterEscHandler('7', func() bool {
		pm.slotUsed = true
		return false
	})
	e.RegisterCsiHandler('s', func(ansi.Params) bool {
		pm.slotUsed = true
		return false
	})
	// vt prints REP's char n times, n up to 65535, so a few remote bytes
	// would cost the pump seconds; and tmux caps it at the line end, prints
	// nothing over a pending wrap or after any C0 or CSI. No REP is modelled.
	e.RegisterCsiHandler('b', func(ansi.Params) bool {
		pm.poisoned = true
		return true
	})
	// SetCallbacks replaces the whole set, so it is called once, here.
	e.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { pm.hidden = !visible },
		EnableMode:       func(mode ansi.Mode) { pm.setMode(mode, true) },
		DisableMode:      func(mode ansi.Mode) { pm.setMode(mode, false) },
	})

	// vt answers queries (DSR, DA) by writing to an io.Pipe; with no reader the
	// next such write blocks Write, and with it the pump.
	go func() {
		defer close(pm.drained)
		buf := make([]byte, 256)
		for {
			if _, err := e.Read(buf); err != nil {
				return
			}
		}
	}()

	if !pm.feed(seed) || !pm.feed(m.bytes(h)) {
		pm.close()
		return nil
	}
	return pm
}

// bytes renders the modes as the sequences that establish them. DECSTBM homes
// the cursor, so the CUP after it restores the position; DECOM goes last so
// that CUP is still absolute.
func (m predictModes) bytes(h int) []byte {
	var b []byte
	if !m.cursorVisible {
		b = append(b, "\x1b[?25l"...)
	}
	if m.insert {
		b = append(b, "\x1b[4h"...)
	}
	if !m.wrap {
		b = append(b, "\x1b[?7l"...)
	}
	if m.top != 0 || m.bottom != h-1 {
		b = fmt.Appendf(b, "\x1b[%d;%dr\x1b[%d;%dH", m.top+1, m.bottom+1, m.cy+1, m.cx+1)
	}
	if m.origin {
		b = append(b, "\x1b[?6h"...)
	}
	return b
}

func (pm *predictModel) setMode(mode ansi.Mode, on bool) {
	switch mode {
	case ansi.ModeInsertReplace:
		pm.insert = on
	case ansi.ModeOrigin:
		pm.origin = on
	}
}

// feed writes p to the emulator. The recover is deliberate: vt is pre-1.0 and
// p is remote-controlled, so a parser panic must cost the model, not the
// daemon.
func (pm *predictModel) feed(p []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	pm.fbuf = pm.title.filter(pm.fbuf[:0], p)
	_, err := pm.e.Write(pm.fbuf)
	return err == nil && !pm.poisoned
}

// close ends the drain goroutine and is safe to repeat. It closes the reply
// pipe's writer so the goroutine's Read returns EOF; later vt reply writes
// fail with ErrClosedPipe, which vt ignores. It never calls e.Close(), whose
// closed flag is unsynchronised against Read (the race agentdetect/screen
// documents), and never writes a wake byte, since an io.Pipe write blocks
// until read and a reader that already exited would wedge the pump forever.
func (pm *predictModel) close() {
	if pm == nil {
		return
	}
	if c, ok := pm.e.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

func (pm *predictModel) cursor() (x, y int) {
	p := pm.e.CursorPosition()
	return p.X, p.Y
}

func (pm *predictModel) alt() bool { return pm.e.IsAltScreen() }

func (pm *predictModel) cell(x, y int) *uv.Cell { return pm.e.CellAt(x, y) }

const (
	tfText uint8 = iota
	tfEsc
	tfTitle
)

// titleFilter drops the body of tmux's ESC k window-rename string, which vt
// would print as text. As in tmux, the title ends at CAN, SUB or any ESC, and
// a C0 control after an ESC executes without ending it. Only the k and the
// body are dropped: the ESCs around them may end a string vt is still in.
type titleFilter struct{ st uint8 }

func (f *titleFilter) filter(dst, p []byte) []byte {
	for _, c := range p {
		switch f.st {
		case tfTitle:
			switch c {
			case 0x1b:
				f.st = tfEsc
			case 0x18, 0x1a:
				f.st = tfText
			default:
				continue
			}
		case tfEsc:
			switch c {
			case 'k':
				f.st = tfTitle
				continue
			case 0x18, 0x1a:
				f.st = tfText
			default:
				if c >= 0x20 {
					f.st = tfText
				}
			}
		default:
			if c == 0x1b {
				f.st = tfEsc
			}
		}
		dst = append(dst, c)
	}
	return dst
}
