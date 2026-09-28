package daemon

import (
	"reflect"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// TestReconcileClearsAnEmptyRemoteName pins #845's steady-state half: a remote
// window renamed to "" (reconcile re-asserts every name each pass) must clear
// the mirror's stale @window_bridge_name and tmux name, not no-op through the
// empty-name early return.
func TestReconcileClearsAnEmptyRemoteName(t *testing.T) {
	var got [][]string
	cfg := Config{
		RemoteSession: "rem",
		LocalTmux: func(args ...string) error {
			got = append(got, args)
			return nil
		},
		Reflow: func() {},
	}
	reg := newRegistry()
	reg.add("@1", "@101")

	// windowListFormat: index id active name; a missing name field is "".
	rt, _ := scriptedRT("%begin 1 1 1\n1 @1 1\n%end 1 1 1\n")

	reconcileWindows(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), rt)

	want := [][]string{
		{"set-option", "-w", "-t", "@101", "-u", "@window_bridge_name"},
		{"rename-window", "-t", "@101", "#{b:pane_current_path}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reconcileWindows calls = %v, want %v", got, want)
	}
}

// Pins reconcileWindows' every-path-reflows invariant against the three ways
// its round-trip can bail.
func TestReconcileWindowsReflowsOnEarlyReturn(t *testing.T) {
	cases := []struct {
		name  string
		reply controlmode.Line
		ok    bool
	}{
		{"list-windows errors", controlmode.Line{Kind: controlmode.Error}, true},
		{"round-trip lost", controlmode.Line{}, false},
		{"empty window list", controlmode.Line{Kind: controlmode.End}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reflowed := 0
			cfg := Config{
				RemoteSession: "rem",
				LocalTmux:     func(...string) error { return nil },
				Reflow:        func() { reflowed++ },
			}
			reg := newRegistry()
			reg.add("@1", "@101")
			rt := func(...string) replies {
				return func() (controlmode.Line, bool) { return c.reply, c.ok }
			}

			reconcileWindows(cfg, func(string) {}, NewRouter(),
				noHellos, newCtlState(), reg, newConverger(), rt)

			if reflowed != 1 {
				t.Fatalf("reflow called %d times, want 1", reflowed)
			}
			if _, ok := reg.byRemoteID("@1"); !ok {
				t.Fatal("early return must leave the mirror registry alone")
			}
		})
	}
}

// TestReadLayoutCarriesTheZoomFlag pins the third field: zoom emits no
// notification of its own, so the flag has to ride on the layout read that a
// ctl zoom's reconcile performs.
func TestReadLayoutCarriesTheZoomFlag(t *testing.T) {
	cfg := Config{RemoteSession: "sess"}
	for _, tc := range []struct {
		reply string
		want  bool
	}{
		{"@1 bd67,190x45,0,0,3 %7 1", true},
		{"@1 bd67,190x45,0,0,3 %7 0", false},
		{"@1 bd67,190x45,0,0,3 %7", false}, // no flag: never guess a zoom
	} {
		rt, _ := scriptedRT("%begin 1 1 1\n" + tc.reply + "\n%end 1 1 1\n")
		_, active, zoomed, err := readLayout(rt, cfg, "@1")
		if err != nil {
			t.Fatalf("readLayout(%q): %v", tc.reply, err)
		}
		if active != "%7" {
			t.Errorf("readLayout(%q) active = %q, want %%7", tc.reply, active)
		}
		if zoomed != tc.want {
			t.Errorf("readLayout(%q) zoomed = %v, want %v", tc.reply, zoomed, tc.want)
		}
	}
}
