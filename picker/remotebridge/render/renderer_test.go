package render

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func TestRendererPaintsAndForwards(t *testing.T) {
	client, server := net.Pipe()
	var out bytes.Buffer
	in := bytes.NewBufferString("ls\r")
	noRaw := func() (func() error, error) { return func() error { return nil }, nil }

	done := make(chan error, 1)
	go func() { done <- Run(client, "%3", in, &out, noRaw, nil) }()

	// Expect Hello first.
	f, err := wire.ReadFrame(server)
	if err != nil || f.Type != wire.FrameHello || string(f.Payload) != "%3" {
		t.Fatalf("hello = %v %q err %v", f.Type, f.Payload, err)
	}
	// Send a seed + one output frame, then close.
	_ = wire.WriteFrame(server, wire.FrameSeed, []byte("SEED"))
	_ = wire.WriteFrame(server, wire.FrameOutput, []byte("OUT"))

	// Expect the forwarded stdin as an Input frame.
	fi, err := wire.ReadFrame(server)
	if err != nil || fi.Type != wire.FrameInput || string(fi.Payload) != "ls\r" {
		t.Fatalf("input = %v %q err %v", fi.Type, fi.Payload, err)
	}
	_ = server.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after conn close")
	}
	if got := out.String(); got != "SEEDOUT" {
		t.Errorf("painted %q, want %q", got, "SEEDOUT")
	}
	_ = io.EOF
}

func TestRendererRecordsResize(t *testing.T) {
	client, server := net.Pipe()
	var out bytes.Buffer
	noRaw := func() (func() error, error) { return func() error { return nil }, nil }

	var gotW, gotH int
	rec := func(w, h int) { gotW, gotH = w, h }

	done := make(chan error, 1)
	go func() { done <- Run(client, "%3", bytes.NewReader(nil), &out, noRaw, rec) }()

	f, err := wire.ReadFrame(server) // Hello
	if err != nil || f.Type != wire.FrameHello {
		t.Fatalf("hello: %v %v", f.Type, err)
	}
	_ = wire.WriteFrame(server, wire.FrameResize, wire.EncodeResize(159, 52))
	_ = wire.WriteFrame(server, wire.FrameSeed, []byte("SEED"))
	// Give the paint loop a moment, then close.
	time.Sleep(50 * time.Millisecond)
	_ = server.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after conn close")
	}
	if gotW != 159 || gotH != 52 {
		t.Errorf("recorded %dx%d, want 159x52", gotW, gotH)
	}
	if out.String() != "SEED" {
		t.Errorf("painted %q, want SEED", out.String())
	}
}
