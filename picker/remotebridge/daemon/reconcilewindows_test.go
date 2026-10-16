package daemon

import (
	"net"
	"reflect"
	"strings"
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

// A remote add follows the remote's active window locally. In a placed host
// session that select-window happens only while the host's current window is
// one of ours: the user on a local window, or on another daemon's mirror,
// stays where they are.
func TestReconcileAddFollowsOnlyFromAnOwnWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		placed     bool
		currentWin string
		wantSelect bool
	}{
		{"dedicated follows", false, "", true},
		{"placed on an own window follows", true, "/s|1", true},
		{"placed on another daemon's mirror stays", true, "/other", false},
		{"placed on a local window stays", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selects [][]string
			cfg := Config{
				RemoteSession: "rem",
				LocalSess:     "work",
				SockPath:      "/s|1",
				LocalArea:     func() (int, int) { return 0, 0 }, // cv.need false: no ConvergeCmd round-trip
				LocalTmux: func(args ...string) error {
					if args[0] == "select-window" {
						selects = append(selects, args)
					}
					return nil
				},
				LocalTmuxOut: func(args ...string) (string, error) {
					switch args[0] {
					case "new-window":
						return "@200\n", nil
					case "list-panes":
						return "%l0 0\n", nil
					case "display-message":
						return tc.currentWin + "\n", nil
					}
					return "", nil
				},
			}
			if tc.placed {
				cfg.Placed, cfg.LocalTarget = true, "$4"
			}
			rt, _ := scriptedRT(strings.Join([]string{
				"%begin 1 1 1", "1 @5 1 five", "%end 1 1 1", // list-windows: @5 is new and active
				"%begin 1 2 1", "@5 bd67,190x45,0,0,0 %0 0", "%end 1 2 1", // readLayout
				"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeed(%0): cursor
				"%begin 1 4 1", "SEED", "%end 1 4 1", // PaneSeed(%0): capture
			}, "\n") + "\n")
			reg := newRegistry()

			reconcileWindows(cfg, func(string) {}, NewRouter(), hellos(map[string]net.Conn{"%0": drainedPipe(t)}), newCtlState(), reg, newConverger(), rt)

			if _, ok := reg.byRemoteID("@5"); !ok {
				t.Fatal("@5 was not mirrored: the add this test gates never happened")
			}
			var want [][]string
			if tc.wantSelect {
				want = [][]string{{"select-window", "-t", "@200"}}
			}
			if !reflect.DeepEqual(selects, want) {
				t.Errorf("select-window calls = %q, want %q", selects, want)
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
