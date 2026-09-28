package daemon

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// #487: a mirror's local window can go away by a route this daemon never saw —
// its last renderer pane exiting after a failed seed, a local kill. w.localWin
// then names a dead window, and nothing retired the entry, so every later
// %layout-change re-ran the same list-panes against it and re-failed
// identically, forever. A pass that fails because the window is gone has to
// hand the mirror back for a rebuild instead of leaving the dead id in place.
func TestApplyPaneOpsFailureRetiresAMirrorWhoseWindowIsGone(t *testing.T) {
	var mu sync.Mutex
	var trace []string

	cfg := Config{
		LocalTmux: func(argv ...string) error {
			mu.Lock()
			defer mu.Unlock()
			trace = append(trace, strings.Join(argv, " "))
			return nil
		},
		LocalTmuxOut: func(argv ...string) (string, error) {
			// refreshLocalPanes' list-panes is how the daemon meets the dead
			// window; display-message is the liveness check that follows.
			// tmux answers the latter with exit 0 and EMPTY output.
			if argv[0] == "list-panes" {
				return "", errors.New("can't find window: @143")
			}
			return "\n", nil
		},
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@143",
		remotePanes: []string{"%3"}, localPanes: []string{"%l3"},
	}

	ops := paneOps{Append: []string{"%4"}}
	err := applyPaneOps(cfg, w, ops, controlmode.Layout{}, []string{"%3"}, []string{"%3", "%4"},
		func(string) {}, NewRouter(), noHellos, nil)
	if err == nil {
		t.Fatal("applyPaneOps err = nil, want the list-panes failure")
	}
	if !resetLostWindow(cfg, w) {
		t.Fatal("resetLostWindow = false, want true: the window is gone, so a retry can never clear this")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(trace) != 0 {
		t.Errorf("acted on a dead window: %v", trace)
	}
}

// End-to-end through reconcileLayout: the remote grew a pane (what the ctl
// `tool` verb's split does), the local window is gone, and the pass must report
// retire rather than returning quietly and leaving the dead id for the next
// %layout-change to re-fail on.
func TestReconcileLayoutReportsRetireWhenTheWindowIsGone(t *testing.T) {
	const layout = "bd67,190x45,0,0[190x22,0,0,3,190x22,0,23,4]"
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@143",
		remotePanes: []string{"%3"}, localPanes: []string{"%l3"},
	}

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + layout + " %3 0", "%end 1 1 1", // readLayout
	}, "\n") + "\n")

	cfg := Config{
		LocalTmux: func(...string) error { return nil },
		LocalTmuxOut: func(argv ...string) (string, error) {
			if argv[0] == "list-panes" {
				return "", errors.New("can't find window: @143")
			}
			return "\n", nil // display-message on a dead window: exit 0, empty
		},
	}

	if retire := reconcileLayout(cfg, w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt); !retire {
		t.Fatal("reconcileLayout retire = false, want true so the caller rebuilds the mirror")
	}
}

// A failure that is NOT a dead window must leave the mirror alone: retiring on
// a transient error would tear down a live window in order to rebuild it.
func TestResetLostWindowKeepsALiveMirror(t *testing.T) {
	cfg := Config{LocalTmuxOut: func(...string) (string, error) { return "@143\n", nil }}
	w := &mirrorWindow{remoteID: "@1", localWin: "@143"}
	if resetLostWindow(cfg, w) {
		t.Error("resetLostWindow = true for a window tmux still reports; want false")
	}
}

// localWindowGone must key on the returned id, never on the error: tmux exits 0
// with empty output for `display-message -p -t @<dead>` (#152/#169), so an
// error-based check reports every dead window as alive. It must equally not
// retire on an unreadable answer, which would rebuild a healthy mirror.
func TestLocalWindowGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"dead window answers empty at exit 0", "\n", nil, true},
		{"some other window", "@7\n", nil, true},
		{"alive", "@143\n", nil, false},
		{"alive, untrimmed", "  @143  \n", nil, false},
		{"unreadable is not evidence of death", "", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{LocalTmuxOut: func(...string) (string, error) { return tc.out, tc.err }}
			if got := localWindowGone(cfg, "@143"); got != tc.want {
				t.Errorf("localWindowGone(%q, %v) = %v, want %v", tc.out, tc.err, got, tc.want)
			}
		})
	}

	// No read seam wired (the daemon's test/degraded paths) is not evidence either.
	if localWindowGone(Config{}, "@143") {
		t.Error("localWindowGone with no LocalTmuxOut = true, want false")
	}
}
