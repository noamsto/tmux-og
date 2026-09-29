package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// OG_PICKER_TRACE=<file> appends one "<event> <µs>" line per startup milestone
// to <file>, timed from OG_PICKER_T0 (unix ns, stamped by the launcher) or from
// process start when that is unset, and exits shortly after the first list
// paint. tests/perf/picker-open-latency.sh reads it; unset, every hook is a
// no-op.
var trace = newTracer(os.Getenv("OG_PICKER_TRACE"), os.Getenv("OG_PICKER_T0"))

type tracer struct {
	path string
	// exits: the run ends 30ms after the first paint. Only when the launcher
	// stamp is set too, so a stray OG_PICKER_TRACE cannot close real pickers.
	exits bool
	t0    time.Time
	once  sync.Once

	framed, painted atomic.Bool
}

func newTracer(path, t0ns string) *tracer {
	t := &tracer{path: path, t0: time.Now()}
	if ns, err := strconv.ParseInt(t0ns, 10, 64); err == nil && ns > 0 {
		t.t0 = time.Unix(0, ns)
		t.exits = true
	}
	return t
}

func (t *tracer) mark(event string) {
	if t.path == "" {
		return
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %d\n", event, time.Since(t.t0).Microseconds())
}

// firstFrame records the first frame that draws the list.
func (t *tracer) firstFrame() {
	if t.path == "" {
		return
	}
	t.once.Do(func() {
		t.mark("first_frame")
		t.framed.Store(true)
	})
}

// programOptions taps the renderer's output so "paint" is the first write after
// the first list frame: bubbletea flushes on a frame ticker, so View() returning
// is not yet a painted screen. When tracing it also ends the run shortly after,
// letting the harness time the next open.
func (t *tracer) programOptions() []tea.ProgramOption {
	if t.path == "" || !t.exits {
		return nil
	}
	return []tea.ProgramOption{tea.WithOutput(&tracedOutput{File: os.Stdout, t: t})}
}

type tracedOutput struct {
	*os.File
	t *tracer
}

func (o *tracedOutput) Write(p []byte) (int, error) {
	n, err := o.File.Write(p)
	if o.t.framed.Load() && o.t.painted.CompareAndSwap(false, true) {
		o.t.mark("paint")
		time.AfterFunc(30*time.Millisecond, func() { os.Exit(0) })
	}
	return n, err
}
