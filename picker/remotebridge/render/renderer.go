package render

import (
	"bufio"
	"errors"
	"io"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// Run drives one renderer over conn: sends Hello(paneID), then paints Seed/Output
// to out and forwards in -> Input frames, until conn EOF. rawSetup is injected so
// tests can skip real tty setup; production passes render.MakeRaw(fd). recordResize
// is invoked with the decoded dims of each FrameResize; size stays daemon-authoritative
// so the renderer only records them (for its painter) and never resizes anything.
func Run(conn io.ReadWriteCloser, paneID string, in io.Reader, out io.Writer, rawSetup func() (func() error, error), recordResize func(w, h int)) error {
	if err := wire.WriteFrame(conn, wire.FrameHello, []byte(paneID)); err != nil {
		return err
	}
	restore, err := rawSetup()
	if err != nil {
		return err
	}
	if restore != nil {
		defer restore() //nolint:errcheck // restore of terminal mode at exit; failure is not actionable
	}

	// stdin -> Input frames
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				if werr := wire.WriteFrame(conn, wire.FrameInput, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// daemon frames -> paint
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(out)
	// The in-loop flush below only fires when the loop keeps going; an abrupt
	// exit (read error, EOF mid-frame-burst) must still flush whatever the
	// last iteration already painted but hadn't flushed yet, or it's lost.
	defer writer.Flush() //nolint:errcheck // final flush on the exit path; the write error already surfaced
	for {
		f, err := wire.ReadFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch f.Type {
		case wire.FrameSeed, wire.FrameOutput:
			if _, werr := writer.Write(f.Payload); werr != nil {
				return werr
			}
		case wire.FrameResize:
			// Size is daemon-authoritative — the renderer records the dims
			// for its painter but never resizes anything (no refresh-client).
			if w, h, err := wire.DecodeResize(f.Payload); err == nil && recordResize != nil {
				recordResize(w, h)
			}
		case wire.FrameHello, wire.FrameInput, wire.FrameCtl, wire.FrameCtlAck:
			// Renderer-bound traffic is seed/output/resize only.
		}
		// Flush only when nothing else is already buffered to read, so a
		// burst of frames paints in one write but a lone frame still lands
		// immediately (no added latency for interactive output).
		if reader.Buffered() == 0 {
			if werr := writer.Flush(); werr != nil {
				return werr
			}
		}
	}
}
