package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTracerOffIsInert(t *testing.T) {
	tr := newTracer("", "")
	tr.mark("start")
	tr.firstFrame()
	if opts := tr.programOptions(); opts != nil {
		t.Errorf("programOptions = %v, want none when tracing is off", opts)
	}
	if tr.framed.Load() {
		t.Error("firstFrame armed the paint hook with tracing off")
	}
}

func TestTracerTimesFromLauncherStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace")
	launched := time.Now().Add(-50 * time.Millisecond)
	tr := newTracer(path, strconv.FormatInt(launched.UnixNano(), 10))
	tr.mark("start")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event string
	var us int64
	if _, err := fmt.Sscan(string(b), &event, &us); err != nil || event != "start" {
		t.Fatalf("trace line %q: %v", b, err)
	}
	if us < 50_000 {
		t.Errorf("start = %dµs, want >= 50000: timed from the launcher stamp, not from process start", us)
	}
}
