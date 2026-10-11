package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// #448: a mirror pane can leave by a route this daemon never saw — a renderer
// that died, a local kill, the window picker's ^x. The believed pane list then
// still names it, and since local pane i is assumed to render remote pane i,
// every kill and swap after the gap lands on a live neighbour. The panes that
// should have gone survive into a select-layout that refuses them
// ("have 2 panes but need 1"), and the daemon logs `can't find pane` on ids
// that are already gone.
func TestApplyPaneOpsRejectsADesyncedWindow(t *testing.T) {
	var mu sync.Mutex
	var trace []string

	// The window really holds one pane; the daemon believes it holds two,
	// having never seen %l4 go.
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			mu.Lock()
			defer mu.Unlock()
			trace = append(trace, strings.Join(argv, " "))
			return nil
		},
		LocalTmuxOut: func(...string) (string, error) { return "%l3 0\n", nil },
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}

	ops := paneOps{Remove: []int{1}}
	err := applyPaneOps(cfg, w, ops, controlmode.Layout{}, []string{"%3", "%4"}, []string{"%3"},
		func(string) {}, NewRouter(), newCtlState(), noHellos, nil)

	if !errors.Is(err, errLocalPanesDesynced) {
		t.Fatalf("err = %v, want errLocalPanesDesynced", err)
	}
	// Nothing may be mutated on a mapping known to be broken: killing by the
	// stale index would have taken the live survivor.
	mu.Lock()
	defer mu.Unlock()
	if len(trace) != 0 {
		t.Errorf("desynced window was mutated: %v", trace)
	}
}
