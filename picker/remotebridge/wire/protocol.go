// Package wire is the leaf package for the daemon<->renderer framed
// protocol. It exists separately from daemon so that render (which paints
// frames) and daemon (which now also produces seed bytes via render.Seed)
// can both depend on it without an import cycle.
package wire

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

type FrameType byte

const (
	FrameHello  FrameType = 1 // renderer->daemon: payload = remote pane id ("%N")
	FrameSeed   FrameType = 2 // daemon->renderer: payload = initial screen bytes
	FrameOutput FrameType = 3 // daemon->renderer: payload = live pane bytes
	FrameResize FrameType = 4 // daemon->renderer: payload = 8 bytes (w,h uint32 BE)
	FrameInput  FrameType = 5 // renderer->daemon: payload = stdin bytes
	FrameCtl    FrameType = 6 // ctl->daemon: payload = NUL-separated argv (version, verb, args)
	FrameCtlAck FrameType = 7 // daemon->ctl: empty payload = accepted, else the error text
)

// CtlProtocolVersion is argv[0] of every FrameCtl. The daemon is launched from
// the caller's PATH and is long-lived, while ctl's path is baked into the tmux
// config and swaps on `prefix + r` — so a config reload or a tmux-og bump can
// point a new ctl at an already-running old daemon. Bump this whenever the verb
// table changes at all — a new verb included, since an old daemon answers one
// with "unknown verb" and the new keybind looks broken rather than stale — so
// the mismatch is a message rather than a silently-ignored gesture.
const CtlProtocolVersion = "6"

// EncodeArgv packs argv as NUL-separated fields. NUL-separated rather than
// space-joined so a window name containing spaces or quotes survives without a
// second layer of quoting.
func EncodeArgv(argv []string) []byte {
	var b []byte
	for i, a := range argv {
		if i > 0 {
			b = append(b, 0)
		}
		b = append(b, a...)
	}
	return b
}

// DecodeArgv unpacks an EncodeArgv payload. An empty payload decodes to no
// fields rather than one empty field, so a malformed frame is distinguishable
// from a frame carrying one empty argument.
func DecodeArgv(p []byte) []string {
	if len(p) == 0 {
		return nil
	}
	parts := strings.Split(string(p), "\x00")
	return parts
}

type Frame struct {
	Type    FrameType
	Payload []byte
}

func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// maxFrameSize bounds the wire-supplied payload length so a corrupt or
// malicious header can't trigger a multi-GiB allocation. Far above any real
// seed/output frame.
const maxFrameSize = 16 << 20 // 16 MiB

// WriteStream emits payload as one or more frames, splitting only when it
// exceeds maxFrameSize (each resulting frame then strictly below it), in
// order. The renderer writes FrameSeed/FrameOutput payloads verbatim and in
// order (render/renderer.go), so splitting a byte-stream payload across
// frames and concatenating them back at the reader is byte-identical at the
// pty. Only FrameOutput and FrameSeed carry a plain byte stream that
// tolerates this: FrameCtl packs NUL-separated argv and FrameResize/
// FrameHello carry structured payloads, and cutting either of those
// mid-payload corrupts it, so any other type is rejected outright.
func WriteStream(w io.Writer, t FrameType, payload []byte) error {
	if t != FrameOutput && t != FrameSeed {
		return fmt.Errorf("wire: WriteStream: frame type %d cannot be split", t)
	}
	if len(payload) <= maxFrameSize {
		return WriteFrame(w, t, payload)
	}
	for len(payload) > 0 {
		n := len(payload)
		if n >= maxFrameSize {
			n = maxFrameSize - 1
		}
		if err := WriteFrame(w, t, payload[:n]); err != nil {
			return err
		}
		payload = payload[n:]
	}
	return nil
}

func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err // io.EOF passes through on clean boundary
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrameSize {
		return Frame{}, fmt.Errorf("frame length %d exceeds max %d", n, maxFrameSize)
	}
	p := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, p); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return Frame{Type: FrameType(hdr[0]), Payload: p}, nil
}

func EncodeResize(w, h int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:], uint32(w))
	binary.BigEndian.PutUint32(b[4:], uint32(h))
	return b
}

func DecodeResize(p []byte) (int, int, error) {
	if len(p) != 8 {
		return 0, 0, fmt.Errorf("resize payload len %d, want 8", len(p))
	}
	return int(binary.BigEndian.Uint32(p[0:])), int(binary.BigEndian.Uint32(p[4:])), nil
}
