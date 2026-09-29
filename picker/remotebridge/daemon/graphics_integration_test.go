package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

func TestGraphicsEndToEndThroughASink(t *testing.T) {
	cache := t.TempDir()
	png := []byte("\x89PNG fake")
	f := &graphics.SSHFetcher{
		Host: "fake", CacheDir: cache, MaxBytes: 1 << 20,
		Run: func(context.Context, ...string) ([]byte, error) {
			return append([]byte("1700000000 9\n"), png...), nil
		},
	}
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	s := newOutputSink(remote, graphics.New(f, nil))
	defer s.Close()

	// base64("/remote/tmp/diagram.png")
	_, _ = s.Write([]byte("\x1b_Gi=300,a=T,U=1,f=100,c=40,r=20,t=f;L3JlbW90ZS90bXAvZGlhZ3JhbS5wbmc=\x1b\\"))

	got := readAllFrames(t, local, 500*time.Millisecond)
	if strings.Count(got, "\x1bPtmux;") != 1 {
		t.Fatalf("want exactly one passthrough wrapper: %q", got)
	}
	for _, want := range []string{"i=300", "c=40", "r=20", "f=100", "t=f"} {
		if !strings.Contains(got, want) {
			t.Fatalf("control key %q was mutated away: %q", want, got)
		}
	}
	if strings.Contains(got, "L3JlbW90ZS90bXAvZGlhZ3JhbS5wbmc=") {
		t.Fatal("still carrying the remote path")
	}
	ents, _ := os.ReadDir(cache)
	if len(ents) != 1 {
		t.Fatalf("want one cached file, got %d", len(ents))
	}
	b, _ := os.ReadFile(filepath.Join(cache, ents[0].Name()))
	if string(b) != string(png) {
		t.Fatalf("cached bytes = %q", b)
	}
}
