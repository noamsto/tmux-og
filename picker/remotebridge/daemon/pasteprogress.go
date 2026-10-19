package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Paste progress (#1007). The swallowed ctrl+v gives no sign of the upload
// behind it, so the paste goroutine paints a spinner on the pasting client's
// status line until the path lands or a failure replaces it. Each frame is a
// display-message with a finite ttl of a few ticks, so a stalled or dead
// daemon leaves a message that expires by itself rather than one stuck on
// screen.

const (
	// pasteTick is the spinner refresh: one local tmux fork per tick, only
	// while a paste runs.
	pasteTick = 250 * time.Millisecond
	// pasteFrameTTLTicks is how many ticks a frame survives unrefreshed.
	pasteFrameTTLTicks = 4
	// pasteFailTTL matches the 5s notifyLocal shows every other message for.
	pasteFailTTL = 5 * time.Second

	pasteDoneMsg = "tmux-og: image pasted"
	pasteDoneTTL = 1500 * time.Millisecond
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// pasteOut routes everything one paste shows to the one client that pasted.
// client is empty when none is attached (or the handler has no status hook),
// in which case messages fall back to notify and there is no progress.
type pasteOut struct {
	h      *pasteHandler
	client string
}

// output resolves the pasting client once, on the paste goroutine.
func (h *pasteHandler) output() pasteOut {
	o := pasteOut{h: h}
	if h.clientFor != nil && h.status != nil {
		o.client = h.clientFor()
	}
	return o
}

func (o pasteOut) say(msg string) { o.sayFor(msg, pasteFailTTL) }

func (o pasteOut) sayFor(msg string, ttl time.Duration) {
	if o.client == "" {
		o.h.notify(msg)
		return
	}
	o.h.status(o.client, msg, ttl)
}

// pasteProgress is the ticker goroutine behind one paste's spinner.
type pasteProgress struct {
	label atomic.Pointer[string]
	done  chan struct{}
	wg    sync.WaitGroup
	once  sync.Once
}

// startProgress paints the first frame at once and refreshes it until stop.
// With no client it returns a progress whose methods are no-ops.
func (o pasteOut) startProgress(label string) *pasteProgress {
	p := &pasteProgress{done: make(chan struct{})}
	p.set(label)
	if o.client == "" {
		return p
	}
	tick := o.h.tick
	if tick <= 0 {
		tick = pasteTick
	}
	start := time.Now()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(tick)
		defer t.Stop()
		for frame := 0; ; frame++ {
			o.h.status(o.client, fmt.Sprintf("%s %s · %.1fs", spinnerFrames[frame%len(spinnerFrames)], *p.label.Load(), time.Since(start).Seconds()), pasteFrameTTLTicks*tick)
			select {
			case <-p.done:
				return
			case <-t.C:
			}
		}
	}()
	return p
}

func (p *pasteProgress) set(label string) { p.label.Store(&label) }

// stop ends the ticker and waits for it, so no frame can land after the
// message the caller shows next. Safe to call more than once.
func (p *pasteProgress) stop() {
	p.once.Do(func() { close(p.done) })
	p.wg.Wait()
}

// formatSize renders a byte count for the upload label.
func formatSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return strconv.Itoa(n) + " B"
}

// activeClient names the mirror session's most recently active client — the
// one whose keyboard just sent the ctrl+v — or "" when none can be found.
// The first listed wins a tie.
func activeClient(cfg Config) string {
	if cfg.LocalTmuxOut == nil {
		return ""
	}
	out, err := cfg.LocalTmuxOut("list-clients", "-t", cfg.target(), "-F", "#{client_activity}|#{client_name}")
	if err != nil {
		return ""
	}
	var best string
	var bestAct int64 = -1
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		act, name, ok := strings.Cut(line, "|")
		n, perr := strconv.ParseInt(act, 10, 64)
		if !ok || perr != nil || name == "" {
			continue
		}
		if n > bestAct {
			best, bestAct = name, n
		}
	}
	return best
}
