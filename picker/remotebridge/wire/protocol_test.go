package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := []struct {
		t FrameType
		p []byte
	}{
		{FrameHello, []byte("%3")},
		{FrameSeed, []byte("\x1b[2J\x1b[Hhello")},
		{FrameOutput, []byte{0x00, 0xff, 0x1b, '\n'}}, // binary-safe
		{FrameInput, []byte("ls\r")},
	}
	for _, x := range payloads {
		if err := WriteFrame(&buf, x.t, x.p); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	for i, x := range payloads {
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame[%d]: %v", i, err)
		}
		if f.Type != x.t || !bytes.Equal(f.Payload, x.p) {
			t.Errorf("frame[%d] = %d/%q, want %d/%q", i, f.Type, f.Payload, x.t, x.p)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after last frame, got %v", err)
	}
}

func TestReadFrameTruncatedPayload(t *testing.T) {
	var hdr [5]byte
	hdr[0] = byte(FrameSeed)
	binary.BigEndian.PutUint32(hdr[1:], 5) // header promises 5 payload bytes
	buf := append(hdr[:], []byte("ab")...) // connection dies after 2 of them

	if _, err := ReadFrame(bytes.NewReader(buf)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame(truncated payload) = %v, want io.ErrUnexpectedEOF", err)
	}
}

// io.ReadFull only returns bare io.EOF when zero bytes are read, so this is
// the one truncation shape that actually exercises ReadFrame's EOF->
// ErrUnexpectedEOF conversion on the payload read (the 2-of-5 case above
// already returns io.ErrUnexpectedEOF from io.ReadFull itself).
func TestReadFrameZeroPayloadBytes(t *testing.T) {
	var hdr [5]byte
	hdr[0] = byte(FrameSeed)
	binary.BigEndian.PutUint32(hdr[1:], 5) // header promises 5 payload bytes
	buf := hdr[:]                          // connection dies before any of them arrive

	if _, err := ReadFrame(bytes.NewReader(buf)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame(zero payload bytes) = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameOversizedLength(t *testing.T) {
	var hdr [5]byte
	hdr[0] = byte(FrameSeed)
	binary.BigEndian.PutUint32(hdr[1:], maxFrameSize+1)

	if _, err := ReadFrame(bytes.NewReader(hdr[:])); err == nil {
		t.Fatal("ReadFrame(oversized length) = nil error, want error")
	}
}

func TestReadFrameCleanCloseAtBoundary(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame(empty reader) = %v, want io.EOF", err)
	}
}

type countingWriter struct {
	w     io.Writer
	calls int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	return c.w.Write(p)
}

func TestWriteFrameSingleSyscall(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"non-empty payload", []byte("\x1b[2J\x1b[Hhello")},
		{"nil payload", nil},
		{"empty payload", []byte{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			cw := &countingWriter{w: &buf}
			if err := WriteFrame(cw, FrameSeed, c.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			if cw.calls != 1 {
				t.Errorf("Write calls = %d, want 1", cw.calls)
			}
			f, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if f.Type != FrameSeed || !bytes.Equal(f.Payload, c.payload) {
				t.Errorf("frame = %d/%q, want %d/%q", f.Type, f.Payload, FrameSeed, c.payload)
			}
		})
	}
}

func TestResizeCodec(t *testing.T) {
	p := EncodeResize(210, 52)
	w, h, err := DecodeResize(p)
	if err != nil || w != 210 || h != 52 {
		t.Fatalf("DecodeResize = %d,%d,%v want 210,52,nil", w, h, err)
	}
	if _, _, err := DecodeResize([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeResize(short) expected error")
	}
}

func TestEncodeDecodeResizeRoundTrip(t *testing.T) {
	for _, c := range []struct{ w, h int }{{80, 24}, {210, 52}, {1, 1}} {
		w, h, err := DecodeResize(EncodeResize(c.w, c.h))
		if err != nil || w != c.w || h != c.h {
			t.Errorf("round-trip %dx%d -> %dx%d err %v", c.w, c.h, w, h, err)
		}
	}
	if _, _, err := DecodeResize([]byte{1, 2, 3}); err == nil {
		t.Error("short payload must error")
	}
}
