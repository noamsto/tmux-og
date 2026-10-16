package daemon

import (
	"net"
	"reflect"
	"slices"
	"strconv"
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

// selectorCfg is cfg narrowed to sel, as runMirror builds it.
func selectorCfg(t *testing.T, cfg Config, sel string) Config {
	t.Helper()
	s, err := ParseWindowSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Select, cfg.filter = s, newWindowFilter(s)
	return cfg
}

// selectedListScript is a filtered list-windows reply followed by the
// ConvergeCmd and readLayout blocks setupWindow reads for each window in mirrored.
func selectedListScript(rows []string, mirrored ...string) string {
	lines := append([]string{"%begin 1 1 1"}, rows...)
	lines = append(lines, "%end 1 1 1")
	seq := 2
	for _, id := range mirrored {
		n := strconv.Itoa(seq)
		lines = append(lines, "%begin 1 "+n+" 1", "%end 1 "+n+" 1")
		n = strconv.Itoa(seq + 1)
		lines = append(lines, "%begin 1 "+n+" 1", id+" b25d,80x24,0,0,0 %0 0", "%end 1 "+n+" 1")
		seq += 2
	}
	return strings.Join(lines, "\n") + "\n"
}

// filteredReconcile runs one filtered reconcileWindows over an empty registry
// against a reply holding a selected @1, an unselected @2 and a malformed @3,
// returning what it sent fire-and-forget and through the round-trip.
func filteredReconcile(t *testing.T) (f *startupFake, reg *registry, cv *converger, sent, issued []string) {
	t.Helper()
	f, cfg := newStartupFake()
	cfg = selectorCfg(t, cfg, "@crew=x")
	cfg.RemoteSession = "rem"
	reg, cv = newRegistry(), newConverger()
	rt := recordingRT(selectedListScript([]string{"1 @1 1 1 one", "2 @2 0 0 two", "3 @3 0 x three"}, "@1"), &issued)

	reconcileWindows(cfg, func(s string) { sent = append(sent, s) }, NewRouter(), noHellos, newCtlState(), reg, cv, rt)
	return f, reg, cv, sent, issued
}

// A filtered reconcile mirrors only a row showing 1, and neither the
// aggressive-resize opt-out nor a cap names a window the selector did not pick.
func TestFilteredReconcileMirrorsOnlySelectedWindows(t *testing.T) {
	f, reg, _, sent, issued := filteredReconcile(t)

	if ids := reg.remoteIDs(); !reflect.DeepEqual(ids, []string{"@1"}) {
		t.Fatalf("registry = %v, want [@1]", ids)
	}
	if n := f.newWindowCount(); n != 1 {
		t.Errorf("new-window calls = %d, want 1", n)
	}
	if !slices.Contains(sent, AggressiveResizeOffCmd("@1")) {
		t.Errorf("sent %q, want %q", sent, AggressiveResizeOffCmd("@1"))
	}
	if !slices.Contains(issued, ConvergeCmd("@1", 80, 24)) {
		t.Errorf("issued %q, want %q", issued, ConvergeCmd("@1", 80, 24))
	}
	for _, cmd := range slices.Concat(sent, issued) {
		if strings.Contains(cmd, "@2") || strings.Contains(cmd, "@3") {
			t.Errorf("command %q names an unselected window", cmd)
		}
	}
}

// A registered window closes only on a row positively showing 0 or on missing
// from a non-empty reply; a malformed row, an error or an empty reply leaves
// every mirror standing. Only a read that was acted on counts as reconciled.
func TestFilteredReconcileClosesOnPositiveEvidenceOnly(t *testing.T) {
	scripted := func(rows ...string) roundTrip { return setupWindowRT(selectedListScript(rows)) }
	fixed := func(l controlmode.Line, ok bool) roundTrip {
		return func(...string) replies { return func() (controlmode.Line, bool) { return l, ok } }
	}
	for _, tc := range []struct {
		name       string
		registered []string
		rt         roundTrip
		wantKept   []string
		wantKilled []string
		read       bool
	}{
		{"0 closes, malformed stays", []string{"@1", "@2", "@3"},
			scripted("1 @1 1 1 one", "2 @2 0 0 two", "3 @3 0 x three"), []string{"@1", "@3"}, []string{"@102"}, true},
		{"missing from a non-empty reply closes", []string{"@1", "@4"},
			scripted("1 @1 1 1 one"), []string{"@1"}, []string{"@104"}, true},
		{"errored reply", []string{"@1", "@2"},
			fixed(controlmode.Line{Kind: controlmode.Error}, true), []string{"@1", "@2"}, nil, false},
		{"lost round-trip", []string{"@1", "@2"},
			fixed(controlmode.Line{}, false), []string{"@1", "@2"}, nil, false},
		{"empty reply", []string{"@1", "@2"},
			scripted(), []string{"@1", "@2"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cfg := newStartupFake()
			cfg = selectorCfg(t, cfg, "@crew=x")
			cfg.RemoteSession = "rem"
			cfg.filter.note()
			reg := newRegistry()
			for _, id := range tc.registered {
				reg.add(id, "@10"+strings.TrimPrefix(id, "@"))
			}

			reconcileWindows(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), tc.rt)

			kept := reg.remoteIDs()
			slices.Sort(kept)
			if !slices.Equal(kept, tc.wantKept) {
				t.Errorf("registry = %v, want %v", kept, tc.wantKept)
			}
			if kills := f.killWindows(); !slices.Equal(kills, tc.wantKilled) {
				t.Errorf("kill-window calls = %v, want %v", kills, tc.wantKilled)
			}
			if n := f.newWindowCount(); n != 0 {
				t.Errorf("new-window calls = %d, want 0", n)
			}
			if cfg.filter.dirty == tc.read {
				t.Errorf("filter dirty = %v after the reconcile, want %v", cfg.filter.dirty, !tc.read)
			}
		})
	}
}

// A %window-add is mirrored only when its row in the filtered re-read shows 1.
func TestFilteredAddWindowMirrorsOnlyASelectedWindow(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		want    bool
		wantNew int
	}{
		{"0", false, 0},
		{"1", true, 1},
	} {
		t.Run("flag "+tc.flag, func(t *testing.T) {
			f, cfg := newStartupFake()
			cfg = selectorCfg(t, cfg, "@crew=x")
			cfg.RemoteSession = "rem"
			reg := newRegistry()
			rt := setupWindowRT(selectedListScript([]string{"1 @5 1 " + tc.flag + " five"}, "@5"))

			addWindow(cfg, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), rt, "@5")

			if _, ok := reg.byRemoteID("@5"); ok != tc.want {
				t.Errorf("@5 mirrored = %v, want %v", ok, tc.want)
			}
			if n := f.newWindowCount(); n != tc.wantNew {
				t.Errorf("new-window calls = %d, want %d", n, tc.wantNew)
			}
		})
	}
}
