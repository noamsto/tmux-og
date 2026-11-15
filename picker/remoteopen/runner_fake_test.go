package remoteopen

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// recordingRunner stands in for execRunner: it records every cmdSpec in order
// and answers from answer (nil answers "", 0). Like execRunner, captured
// stdout loses its trailing newlines and uncaptured stdout returns "".
type recordingRunner struct {
	answer func(cmdSpec) (stdout string, status int)

	mu    sync.Mutex
	calls []cmdSpec
}

func (r *recordingRunner) Run(c cmdSpec) (string, int, error) {
	c.Args = slices.Clone(c.Args)
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if r.answer == nil {
		return "", 0, nil
	}
	out, status := r.answer(c)
	if !c.captures() {
		out = ""
	}
	return strings.TrimRight(out, "\n"), status, nil
}

func (r *recordingRunner) recorded() []cmdSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// lines renders each recorded call with callLine, in call order.
func (r *recordingRunner) lines() []string {
	calls := r.recorded()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = callLine(c)
	}
	return out
}

// callLine renders a call as "name arg1 arg2 ...", joined by single spaces
// without quoting, like the bats fakes' TMUX_LOG lines.
func callLine(c cmdSpec) string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

func TestRecordingRunner(t *testing.T) {
	r := &recordingRunner{answer: func(c cmdSpec) (string, int) {
		if c.Name == "tmux" {
			return "v\n\n", 1
		}
		return "ignored\n", 0
	}}
	out, status, err := r.Run(cmdSpec{Name: "tmux", Args: []string{"show-options", "-qv", "@x"}, Capture: true})
	if out != "v" || status != 1 || err != nil {
		t.Fatalf("got (%q, %d, %v), want (\"v\", 1, nil)", out, status, err)
	}
	if out, _, _ := r.Run(cmdSpec{Name: "ssh", Args: []string{"h"}}); out != "" {
		t.Fatalf("uncaptured stdout = %q, want empty", out)
	}
	want := []string{"tmux show-options -qv @x", "ssh h"}
	if got := r.lines(); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}
