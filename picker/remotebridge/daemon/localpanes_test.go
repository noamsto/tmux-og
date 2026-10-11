package daemon

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestParseLocalPaneList(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantTiled  []string
		wantFloats []string
	}{
		{
			name:      "all tiled",
			out:       "%1 0\n%2 0\n%3 0\n",
			wantTiled: []string{"%1", "%2", "%3"},
		},
		{
			// The ordering that breaks index addressing: tmux reports the float
			// in the middle, so the second mirrored pane is the window's third.
			name:       "float mid-order",
			out:        "%1 0\n%9 1\n%2 0\n%3 0\n",
			wantTiled:  []string{"%1", "%2", "%3"},
			wantFloats: []string{"%9"},
		},
		{
			name:       "float last",
			out:        "%1 0\n%2 0\n%9 1\n",
			wantTiled:  []string{"%1", "%2"},
			wantFloats: []string{"%9"},
		},
		{
			name:       "blank lines and padding are skipped",
			out:        "\n  %1 0  \n\n%9 1\n",
			wantTiled:  []string{"%1"},
			wantFloats: []string{"%9"},
		},
		{
			name: "a line without a pane id is not a pane",
			out:  "no-such-pane 0\n",
		},
		{name: "empty listing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tiled, floats := parseLocalPaneList(tc.out)
			if !reflect.DeepEqual(tiled, tc.wantTiled) {
				t.Errorf("tiled = %v, want %v", tiled, tc.wantTiled)
			}
			if !reflect.DeepEqual(floats, tc.wantFloats) {
				t.Errorf("floats = %v, want %v", floats, tc.wantFloats)
			}
		})
	}
}

func TestLocalPaneAtRejectsOutOfRange(t *testing.T) {
	w := &mirrorWindow{localPanes: []string{"%1", "%2"}}
	if id, ok := localPaneAt(w, 1); !ok || id != "%2" {
		t.Errorf("localPaneAt(1) = %q,%v; want %%2,true", id, ok)
	}
	for _, i := range []int{-1, 2} {
		if _, ok := localPaneAt(w, i); ok {
			t.Errorf("localPaneAt(%d) accepted an out-of-range index", i)
		}
	}
}

// recordingCfg is a Config whose tmux calls are recorded rather than run, with
// list-panes answered from a canned listing.
func recordingCfg(listing string, got *[][]string) Config {
	return Config{
		LocalTmux: func(args ...string) error {
			*got = append(*got, args)
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			*got = append(*got, args)
			return listing, nil
		},
	}
}

func tmuxCalls(got [][]string, verb string) [][]string {
	var out [][]string
	for _, c := range got {
		if len(c) > 0 && c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// A float sits at ordinal 1, so the pane rendering the remote's second pane is
// the window's *third*. Addressing by index would kill the float.
func TestApplyPaneOpsTargetsPaneIDsPastAFloat(t *testing.T) {
	const listing = "%1 0\n%9 1\n%2 0\n%3 0\n"
	var got [][]string
	cfg := recordingCfg(listing, &got)

	w := &mirrorWindow{
		localWin:    "mirror:1",
		remotePanes: []string{"%r1", "%r2", "%r3"},
		localPanes:  []string{"%1", "%2", "%3"},
		conns:       map[string]net.Conn{},
	}
	remote := []string{"%r1", "%r2", "%r3"}
	ops := paneOps{Remove: []int{1}}

	err := applyPaneOps(cfg, w, ops, controlmode.Layout{}, remote, []string{"%r1", "%r3"},
		func(string) {}, NewRouter(), newCtlState(), noHellos, nil)
	if err != nil {
		t.Fatalf("applyPaneOps: %v", err)
	}

	kills := tmuxCalls(got, "kill-pane")
	if len(kills) != 1 {
		t.Fatalf("kill-pane calls = %d, want 1 (%v)", len(kills), got)
	}
	if target := kills[0][len(kills[0])-1]; target != "%2" {
		t.Errorf("kill-pane targeted %q, want %%2 (the pane rendering %%r2)", target)
	}
	for _, c := range got {
		for _, a := range c {
			if strings.Contains(a, "mirror:1.") {
				t.Errorf("pane addressed by window.index: %v", c)
			}
			if a == "%9" {
				t.Errorf("the window's float was targeted: %v", c)
			}
		}
	}
}

// Swaps must keep localPanes parallel to remotePanes, or the next pass targets
// the pre-swap pane.
func TestApplyPaneOpsSwapKeepsLocalPanesParallel(t *testing.T) {
	var got [][]string
	cfg := recordingCfg("%1 0\n%2 0\n", &got)
	w := &mirrorWindow{
		localWin:    "mirror:1",
		remotePanes: []string{"%r1", "%r2"},
		localPanes:  []string{"%1", "%2"},
		conns:       map[string]net.Conn{},
	}
	ops := paneOps{Swaps: [][2]int{{0, 1}}}

	if err := applyPaneOps(cfg, w, ops, controlmode.Layout{}, []string{"%r1", "%r2"},
		[]string{"%r2", "%r1"}, func(string) {}, NewRouter(), newCtlState(), noHellos, nil); err != nil {
		t.Fatalf("applyPaneOps: %v", err)
	}

	swaps := tmuxCalls(got, "swap-pane")
	if len(swaps) != 1 {
		t.Fatalf("swap-pane calls = %d, want 1", len(swaps))
	}
	if want := []string{"swap-pane", "-d", "-s", "%1", "-t", "%2"}; !reflect.DeepEqual(swaps[0], want) {
		t.Errorf("swap-pane = %v, want %v", swaps[0], want)
	}
	if want := []string{"%2", "%1"}; !reflect.DeepEqual(w.localPanes, want) {
		t.Errorf("localPanes = %v, want %v", w.localPanes, want)
	}
}

// A pane that moved to another remote window (#922) must not be unregistered by
// the source window's reconcile: the destination window has already registered
// its own sink for that pane id, and deleting it would leave the moved mirror
// pane frozen on its seed forever. Removing a pane no other window owns still
// unregisters.
func TestApplyPaneOpsUnregistersOnlyPanesThisWindowOwns(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		wantSink bool
	}{
		{"moved to another window keeps the destination's sink", "@2", true},
		{"gone from this window unregisters", "@1", false},
		{"no owner recorded unregisters", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got [][]string
			cfg := recordingCfg("%1 0\n%2 0\n", &got)
			w := &mirrorWindow{
				localWin:    "mirror:1",
				remoteID:    "@1",
				remotePanes: []string{"%r1", "%r2"},
				localPanes:  []string{"%1", "%2"},
				conns:       map[string]net.Conn{},
			}
			router := NewRouter()
			router.Register("%r2", &strings.Builder{})
			cst := newCtlState()
			if tc.owner != "" {
				cst.setWindowPanes(tc.owner, []string{"%r2"})
			}

			err := applyPaneOps(cfg, w, paneOps{Remove: []int{1}}, controlmode.Layout{},
				[]string{"%r1", "%r2"}, []string{"%r1"}, func(string) {}, router, cst, noHellos, nil)
			if err != nil {
				t.Fatalf("applyPaneOps: %v", err)
			}
			router.mu.Lock()
			_, registered := router.sinks["%r2"]
			router.mu.Unlock()
			if registered != tc.wantSink {
				t.Errorf("sink registered = %v, want %v", registered, tc.wantSink)
			}
		})
	}
}

// resetWindow drops the mirrored panes but must not reap a float it never made.
func TestResetWindowLeavesAFloatAlone(t *testing.T) {
	var got [][]string
	cfg := recordingCfg("%1 0\n%9 1\n", &got)
	w := &mirrorWindow{
		localWin:    "mirror:1",
		remotePanes: []string{"%r1", "%r2", "%r3"},
		localPanes:  []string{"%1", "%2", "%3"},
		conns:       map[string]net.Conn{},
	}
	dropMirroredPanes(cfg, w)

	var killed []string
	for _, c := range tmuxCalls(got, "kill-pane") {
		killed = append(killed, c[len(c)-1])
	}
	if want := []string{"%3", "%2"}; !reflect.DeepEqual(killed, want) {
		t.Errorf("killed %v, want %v (never the float %%9, never pane 0)", killed, want)
	}
}

// dropMirroredPanes runs at the start of resetWindow; select-layout in the
// following setupWindow unzooms, so a stale appliedZoom would make the next
// same-layout reconcile dedup and skip assertMirrorZoom forever.
func TestDropMirroredPanesClearsAppliedZoom(t *testing.T) {
	cfg := Config{LocalTmux: func(...string) error { return nil }}
	w := &mirrorWindow{
		localWin:    "mirror:1",
		layout:      "bd67,190x45,0,0,3",
		appliedZoom: true,
		remotePanes: []string{"%r1"},
		localPanes:  []string{"%1"},
	}
	dropMirroredPanes(cfg, w)
	if w.appliedZoom {
		t.Error("appliedZoom still true after dropMirroredPanes, want false")
	}
}
