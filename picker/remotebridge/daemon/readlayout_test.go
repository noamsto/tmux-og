package daemon

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// A dead @5 target falls back to the session's current window (@0) from a
// control client; the reply's leading #{window_id} is the only tell.
func TestReadLayoutRejectsAnotherWindowsReply(t *testing.T) {
	const layout = "bd67,190x45,0,0,0"
	cfg := Config{RemoteSession: "sess"}

	rt, _ := scriptedRT("%begin 1 1 1\n@0 " + layout + " %0 0\n%end 1 1 1\n")
	if _, _, _, err := readLayout(rt, cfg, "@5"); err == nil {
		t.Fatal("readLayout err = nil, want an error: the reply answered @0, not the @5 asked for")
	} else if !strings.Contains(err.Error(), "@0") || !strings.Contains(err.Error(), "gone") {
		t.Errorf("readLayout err = %q, want it to name the answering window @0 and say the window is gone", err)
	} else if !errors.Is(err, errWindowGone) {
		t.Errorf("readLayout err = %q, want errors.Is(err, errWindowGone)", err)
	}
}

func TestReadLayoutAcceptsAMatchingReply(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"
	cfg := Config{RemoteSession: "sess"}

	rt, _ := scriptedRT("%begin 1 1 1\n@5 " + layout + " %3 1\n%end 1 1 1\n")
	L, active, zoomed, err := readLayout(rt, cfg, "@5")
	if err != nil {
		t.Fatalf("readLayout: %v", err)
	}
	if L.Raw != layout {
		t.Errorf("L.Raw = %q, want %q", L.Raw, layout)
	}
	if active != "%3" {
		t.Errorf("active = %q, want %%3", active)
	}
	if !zoomed {
		t.Error("zoomed = false, want true")
	}
}

// @5 closes between addWindow's list-windows and its readLayout, which then
// answers for @0, whose mirror already feeds %0. Mirroring that reply would
// re-register %0 to @5's renderer, and @5's %window-close would unregister it.
func TestAddWindowGoneMidAddLeavesOtherMirrorsFeed(t *testing.T) {
	const layout = "bd67,190x45,0,0,0" // its one pane is %0, the live mirror's

	router := NewRouter()
	sentinel := newOutputSink(drainedPipe(t), nil)
	router.Register("%0", sentinel)

	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()

	cfg := Config{
		RemoteSession: "rem",
		LocalSess:     "m",
		LocalArea:     func() (int, int) { return 0, 0 }, // cv.need false: no ConvergeCmd round-trip
		LocalTmux:     func(...string) error { return nil },
		LocalTmuxOut: func(args ...string) (string, error) {
			switch args[0] {
			case "new-window":
				return "@200\n", nil
			case "list-panes":
				return "%l0 0\n", nil
			}
			return "", nil
		},
	}

	// The seed replies are never consumed with the id check in place; they let
	// the setup reach wireRenderer when it is missing.
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "1 @5 0 five", "%end 1 1 1", // list-windows
		"%begin 1 2 1", "@0 " + layout + " %0 0", "%end 1 2 1", // readLayout: fallback answers @0
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeed(%0): cursor
		"%begin 1 4 1", "SEED", "%end 1 4 1", // PaneSeed(%0): capture
	}, "\n") + "\n")

	addWindow(cfg, func(string) {}, router, hellos(map[string]net.Conn{"%0": drainedPipe(t)}), cst, reg, cv, rt, "@5")

	if _, ok := reg.byRemoteID("@5"); ok {
		t.Error("registry has @5: a mirror for a window that was already gone must not survive setup")
	}
	if router.sink("%0") != sentinel {
		t.Fatal("%0's sink was replaced: the dead window's fallback-mirrored setup stole the live mirror's feed")
	}

	closeWindow(cfg, router, cst, reg, cv, "@5")

	if router.sink("%0") != sentinel {
		t.Error("%0's sink changed after closeWindow(@5): a notification for a window never registered must be a no-op")
	}
}

// A reconcile whose read answers for another window applies nothing to w.
func TestReconcileLayoutIgnoresAnotherWindowsReply(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"
	const otherLayout = "bd67,190x45,0,0,7"
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      layout,
	}

	rt, sent := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@0 " + otherLayout + " %0 0", "%end 1 1 1", // readLayout: fallback answers @0
	}, "\n") + "\n")

	cfg := Config{
		LocalTmux: func(...string) error {
			t.Fatal("unexpected LocalTmux call: readLayout must reject the reply before any reshape")
			return nil
		},
	}

	retire := reconcileLayout(cfg, w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if retire {
		t.Error("retire = true, want false: a rejected reply is not a gone-window signal")
	}
	if w.layout != layout {
		t.Errorf("w.layout = %q, want unchanged %q", w.layout, layout)
	}
	if got := strings.Join(w.remotePanes, ","); got != "%3" {
		t.Errorf("w.remotePanes = %v, want unchanged [%%3]", w.remotePanes)
	}
	for _, want := range []string{"select-layout", "split-window", "kill-pane", "respawn-pane"} {
		if got := sent.String(); strings.Contains(got, want) {
			t.Errorf("sent %q, want no %s (readLayout rejected the reply before any reshape)", got, want)
		}
	}
}
