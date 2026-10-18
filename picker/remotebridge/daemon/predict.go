package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

const (
	// predictShowAbove and predictHideBelow are the hysteresis band on the
	// median echo latency: a latency inside it keeps the previous verdict.
	predictShowAbove = 30 * time.Millisecond
	predictHideBelow = 20 * time.Millisecond
	// predictSampleMax discards a sample as a key that never echoed (a password
	// prompt) timed against unrelated later output.
	predictSampleMax    = time.Second
	predictSampleWindow = 8
)

// PredictMode selects when mirror panes echo typed keys locally ahead of the
// remote. The zero value is PredictAuto.
type PredictMode int

const (
	PredictAuto PredictMode = iota
	PredictOn
	PredictOff
)

// ParsePredictMode parses the --predict flag value; "" means auto.
func ParsePredictMode(s string) (PredictMode, error) {
	switch s {
	case "", "auto":
		return PredictAuto, nil
	case "on":
		return PredictOn, nil
	case "off":
		return PredictOff, nil
	}
	return PredictAuto, fmt.Errorf("predict: %q is not auto, on or off", s)
}

// predictShared is the daemon-wide latency estimate every predicting sink
// feeds and consults. Sinks sample from their own pump goroutines.
type predictShared struct {
	mode PredictMode

	mu     sync.Mutex
	ring   [predictSampleWindow]time.Duration
	next   int
	filled int
	median time.Duration
	wanted bool
}

// newPredictShared returns nil for PredictOff, which every method treats as
// "never wanted".
func newPredictShared(m PredictMode) *predictShared {
	if m == PredictOff {
		return nil
	}
	return &predictShared{mode: m}
}

// sample records the delay between a key reaching a pump and the next output
// for that pane. The median, not the mean, keeps a few polluted samples in
// either direction from moving the estimate.
func (p *predictShared) sample(d time.Duration) {
	if p == nil || d <= 0 || d > predictSampleMax {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ring[p.next] = d
	p.next = (p.next + 1) % predictSampleWindow
	p.filled = min(p.filled+1, predictSampleWindow)

	sorted := p.ring
	slices.Sort(sorted[:p.filled])
	p.median = sorted[(p.filled-1)/2]
	switch {
	case p.median > predictShowAbove:
		p.wanted = true
	case p.median < predictHideBelow:
		p.wanted = false
	}
}

func (p *predictShared) want() bool {
	if p == nil {
		return false
	}
	if p.mode == PredictOn {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wanted
}

func (p *predictShared) estimate() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.median
}

// predictModes is the remote pane's state the predictor's local model must
// mirror but a capture-pane seed does not carry. cx and cy are the caller's
// to fill from the cursor reply.
type predictModes struct {
	cursorVisible, insert, origin, wrap bool
	top, bottom                         int
	cx, cy                              int
}

func predictModesCmd(paneID string) string {
	return fmt.Sprintf("display-message -p -t %s -F '#{cursor_flag}|#{insert_flag}|#{origin_flag}|#{wrap_flag}|#{scroll_region_upper}|#{scroll_region_lower}'", paneID)
}

// parsePredictModes parses a predictModesCmd reply. ok is false for a dead
// stream, an error reply or any malformed field: a remote tmux that cannot
// report these modes expands the formats empty, and the model must not be
// built from guesses.
func parsePredictModes(l controlmode.Line, ok bool) (predictModes, bool) {
	if !ok || l.Kind == controlmode.Error {
		return predictModes{}, false
	}
	fields := strings.Split(strings.TrimSpace(string(l.Data)), "|")
	if len(fields) != 6 {
		return predictModes{}, false
	}
	var flags [4]bool
	for i, f := range fields[:4] {
		switch f {
		case "0":
		case "1":
			flags[i] = true
		default:
			return predictModes{}, false
		}
	}
	top, err := strconv.Atoi(fields[4])
	if err != nil || top < 0 {
		return predictModes{}, false
	}
	bottom, err := strconv.Atoi(fields[5])
	if err != nil || bottom < top {
		return predictModes{}, false
	}
	return predictModes{
		cursorVisible: flags[0],
		insert:        flags[1],
		origin:        flags[2],
		wrap:          flags[3],
		top:           top,
		bottom:        bottom,
	}, true
}
