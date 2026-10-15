package daemon

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// startupFake answers the local-tmux calls mirrorStartupWindows makes:
// list-windows (firstMirrorWindow) always returns @100, new-window
// (createMirrorWindow) returns @101, @102, … in call order, list-panes
// answers with one local pane, and every LocalTmux/LocalTmuxOut call is
// recorded on the shared log in the order it happened.
type startupFake struct {
	log     *callLog
	nextWin int
}

func newStartupFake() (*startupFake, Config) {
	f := &startupFake{log: &callLog{}, nextWin: 101}
	cfg := Config{
		LocalArea: func() (int, int) { return 80, 24 },
		LocalTmux: func(args ...string) error {
			f.log.add(args)
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			f.log.add(args)
			switch args[0] {
			case "list-windows":
				return "@100\n", nil
			case "new-window":
				id := "@" + strconv.Itoa(f.nextWin)
				f.nextWin++
				return id + "\n", nil
			case "list-panes":
				return "%l0 0\n", nil
			}
			return "", nil
		},
	}
	return f, cfg
}

func (f *startupFake) killWindows() []string {
	var kills []string
	for _, c := range f.log.snapshot() {
		if len(c) >= 3 && c[0] == "kill-window" && c[1] == "-t" {
			kills = append(kills, c[2])
		}
	}
	return kills
}

func (f *startupFake) newWindowCount() int {
	n := 0
	for _, c := range f.log.snapshot() {
		if len(c) > 0 && c[0] == "new-window" {
			n++
		}
	}
	return n
}

// nameState replays the recorded calls and returns the last @window_bridge_name
// write for localWin (its value, or unset) and the last rename-window name. The
// fake's LocalTmux never runs real tmux, so this is the tmux-name oracle the
// #845 tests assert on.
func (f *startupFake) nameState(localWin string) (bridge string, bridgeSet bool, windowName string) {
	for _, c := range f.log.snapshot() {
		switch {
		case len(c) >= 6 && c[0] == "set-option" && c[3] == localWin && c[4] == "-u" && c[5] == "@window_bridge_name":
			bridge, bridgeSet = "", false
		case len(c) >= 6 && c[0] == "set-option" && c[3] == localWin && c[4] == "@window_bridge_name":
			bridge, bridgeSet = c[5], true
		case len(c) >= 4 && c[0] == "rename-window" && c[2] == localWin:
			windowName = c[3]
		}
	}
	return bridge, bridgeSet, windowName
}

// startupScript builds a setupWindowRT script for a run of windows, one
// ConvergeCmd block (always empty — the fake succeeds with no data) followed
// by a readLayout block per window, numbered sequentially from seq across the
// whole script as setupWindowRT requires. answerID lets a window's readLayout
// answer for a different window id, simulating one that vanished between
// list-windows and readLayout.
func startupScript(answerID ...string) string {
	const layout = "b25d,80x24,0,0,0"
	var lines []string
	seq := 1
	for _, id := range answerID {
		lines = append(lines, "%begin 1 "+strconv.Itoa(seq)+" 1", "%end 1 "+strconv.Itoa(seq)+" 1")
		seq++
		lines = append(lines, "%begin 1 "+strconv.Itoa(seq)+" 1", id+" "+layout+" %0 0", "%end 1 "+strconv.Itoa(seq)+" 1")
		seq++
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestStartupSkipsAWindowThatVanishes pins the middle-of-three case: @2 closes
// between list-windows and its readLayout (its reply falls back to @1), and
// startup must mirror @1 and @3 anyway, dropping only @2 and its half-built
// local window.
func TestStartupSkipsAWindowThatVanishes(t *testing.T) {
	f, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}, {id: "@3"}}

	rt := setupWindowRT(startupScript("@1", "@1", "@3"))

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, cst, reg, cv, rt)
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "" {
		t.Errorf("placeholder = %q, want \"\": @1 claimed it", placeholder)
	}

	if mw, ok := reg.byRemoteID("@1"); !ok || mw.localWin != "@100" {
		t.Errorf("reg @1 = %+v, ok=%v, want localWin @100", mw, ok)
	}
	if mw, ok := reg.byRemoteID("@3"); !ok || mw.localWin != "@102" {
		t.Errorf("reg @3 = %+v, ok=%v, want localWin @102", mw, ok)
	}
	if _, ok := reg.byRemoteID("@2"); ok {
		t.Error("registry has @2: it vanished and must not survive setup")
	}
	if _, ok := cv.last["@2"]; ok {
		t.Error("cv still records a cap for @2: forget was skipped")
	}

	if kills := f.killWindows(); len(kills) != 1 || kills[0] != "@101" {
		t.Errorf("kill-window calls = %v, want exactly [@101]", kills)
	}
}

// TestStartupSkipsAVanishedWindowWhoseCapErrored covers the stream a real
// remote sends for a vanished window: its ConvergeCmd draws %error, not an
// empty reply. Startup continues past it, so the next window's replies must
// still line up with its own commands.
func TestStartupSkipsAVanishedWindowWhoseCapErrored(t *testing.T) {
	_, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}, {id: "@3"}}

	script := strings.Replace(startupScript("@1", "@1", "@3"),
		"%begin 1 3 1\n%end 1 3 1\n", "%begin 1 3 1\nno such window: @2\n%error 1 3 1\n", 1)
	if !strings.Contains(script, "%error 1 3 1") {
		t.Fatal("fixture did not inject the error reply")
	}
	rt := setupWindowRT(script)

	if _, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, cv, rt); err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if _, ok := reg.byRemoteID("@2"); ok {
		t.Error("registry has @2: it vanished and must not survive setup")
	}
	if mw, ok := reg.byRemoteID("@3"); !ok || mw.localWin != "@102" {
		t.Errorf("reg @3 = %+v, ok=%v, want localWin @102", mw, ok)
	}
	if _, ok := cv.last["@2"]; ok {
		t.Error("cv still records a cap for @2: forget was skipped")
	}
}

// TestStartupVanishedFirstWindowKeepsThePlaceholder pins the placeholder rule:
// the first remote window takes the launcher's initial window, and when IT
// vanishes the placeholder is not killed — it is left for the next remote
// window to claim, so a vanished first window costs no window at all.
func TestStartupVanishedFirstWindowKeepsThePlaceholder(t *testing.T) {
	f, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}}

	rt := setupWindowRT(startupScript("@2", "@2"))

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, cst, reg, cv, rt)
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "" {
		t.Errorf("placeholder = %q, want \"\": @2 claimed it", placeholder)
	}

	if _, ok := reg.byRemoteID("@1"); ok {
		t.Error("registry has @1: it vanished and must not survive setup")
	}
	if mw, ok := reg.byRemoteID("@2"); !ok || mw.localWin != "@100" {
		t.Errorf("reg @2 = %+v, ok=%v, want localWin @100 (the reused placeholder)", mw, ok)
	}

	if kills := f.killWindows(); len(kills) != 0 {
		t.Errorf("kill-window calls = %v, want none: the placeholder must not be killed", kills)
	}
	if n := f.newWindowCount(); n != 0 {
		t.Errorf("new-window calls = %d, want 0: @2 re-takes the unclaimed placeholder", n)
	}
}

// TestStartupEveryWindowVanished pins the all-gone case: neither remote window
// survives to readLayout, so no local window is ever created beyond the
// placeholder, and nothing is ever killed — the caller decides what becomes of
// an unclaimed placeholder, not this function.
func TestStartupEveryWindowVanished(t *testing.T) {
	f, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}}

	rt := setupWindowRT(startupScript("@9", "@9"))

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, cst, reg, cv, rt)
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "@100" {
		t.Errorf("placeholder = %q, want @100: unclaimed", placeholder)
	}
	if !reg.empty() {
		t.Errorf("registry not empty: %v", reg.all())
	}
	if len(cv.last) != 0 {
		t.Errorf("cv records caps %v, want none: forget was skipped", cv.last)
	}
	if kills := f.killWindows(); len(kills) != 0 {
		t.Errorf("kill-window calls = %v, want none", kills)
	}
	if n := f.newWindowCount(); n != 0 {
		t.Errorf("new-window calls = %d, want 0: @2 re-takes the unclaimed placeholder", n)
	}
}

// TestStartupOtherSetupErrorsStayFatal pins that errWindowGone is the ONLY
// setupWindow error startup skips: a script that runs out mid-ConvergeCmd
// (the control connection closing) must still fail the whole open.
func TestStartupOtherSetupErrorsStayFatal(t *testing.T) {
	_, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}}

	rt := setupWindowRT("") // no replies at all: @1's ConvergeCmd round-trip fails closed

	_, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, cst, reg, cv, rt)
	if err == nil {
		t.Fatal("mirrorStartupWindows err = nil, want a fatal error from the closed connection")
	}
	if errors.Is(err, errWindowGone) {
		t.Errorf("err = %q, want a failure other than errWindowGone", err)
	}
}

// TestStartupTakenOverPlaceholderDropsTheDeadName pins #845: the first remote
// window vanishes and the successor that claims the placeholder has an empty
// name, so the surviving mirror must drop the vanished window's name from both
// @window_bridge_name and the tmux window name.
func TestStartupTakenOverPlaceholderDropsTheDeadName(t *testing.T) {
	f, cfg := newStartupFake()
	reg := newRegistry()
	cv := newConverger()
	cst := newCtlState()
	remoteWins := []remoteWindow{{id: "@1", name: "dead"}, {id: "@2"}}

	rt := setupWindowRT(startupScript("@2", "@2"))

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, cst, reg, cv, rt)
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "" {
		t.Errorf("placeholder = %q, want \"\": @2 claimed it", placeholder)
	}
	mw, ok := reg.byRemoteID("@2")
	if !ok || mw.localWin != "@100" {
		t.Fatalf("reg @2 = %+v, ok=%v, want localWin @100 (the reused placeholder)", mw, ok)
	}
	if _, ok := reg.byRemoteID("@1"); ok {
		t.Error("registry has @1: it vanished and must not survive setup")
	}
	if kills := f.killWindows(); len(kills) != 0 {
		t.Errorf("kill-window calls = %v, want none: the placeholder was claimed", kills)
	}
	bridge, bridgeSet, windowName := f.nameState("@100")
	if bridgeSet {
		t.Errorf("@window_bridge_name still set to %q on the surviving mirror, want unset", bridge)
	}
	if windowName != "#{b:pane_current_path}" {
		t.Errorf("surviving mirror window name = %q, want #{b:pane_current_path} (not the dead name)", windowName)
	}
}

// TestStartupDropsUnclaimedPlaceholder pins dropUnclaimedPlaceholder's own
// contract, in isolation from mirrorStartupWindows: it kills the placeholder
// only when one was left unclaimed AND the registry is not empty (an empty
// registry means the whole session is already being torn down by the
// reg.empty() check that runs first).
func TestStartupDropsUnclaimedPlaceholder(t *testing.T) {
	t.Run("kills the placeholder when other windows survived", func(t *testing.T) {
		f, cfg := newStartupFake()
		reflowed := false
		cfg.Reflow = func() { reflowed = true }
		reg := newRegistry()
		reg.add("@3", "@102")

		dropUnclaimedPlaceholder(cfg, reg, "@100")

		if kills := f.killWindows(); len(kills) != 1 || kills[0] != "@100" {
			t.Errorf("kill-window calls = %v, want exactly [@100]", kills)
		}
		if !reflowed {
			t.Error("reflow was not called")
		}
	})

	t.Run("no-op on an empty placeholder", func(t *testing.T) {
		f, cfg := newStartupFake()
		cfg.Reflow = func() { t.Error("reflow called for an empty placeholder") }
		reg := newRegistry()
		reg.add("@3", "@102")

		dropUnclaimedPlaceholder(cfg, reg, "")

		if kills := f.killWindows(); len(kills) != 0 {
			t.Errorf("kill-window calls = %v, want none", kills)
		}
	})

	t.Run("no-op on an empty registry", func(t *testing.T) {
		f, cfg := newStartupFake()
		cfg.Reflow = func() { t.Error("reflow called for an empty registry") }
		reg := newRegistry()

		dropUnclaimedPlaceholder(cfg, reg, "@100")

		if kills := f.killWindows(); len(kills) != 0 {
			t.Errorf("kill-window calls = %v, want none", kills)
		}
	})
}

// identityIndex is the position of the first `set-option -w -t <win> <opt> …`
// call in calls, or -1.
func identityIndex(calls [][]string, win, opt string) int {
	for i, c := range calls {
		if len(c) >= 5 && c[0] == "set-option" && c[1] == "-w" && c[2] == "-t" && c[3] == win && c[4] == opt {
			return i
		}
	}
	return -1
}

// Every mirror window carries its daemon's identity before @bridge_win, the
// gate half a keybind fires on: a window that is a mirror but has no window
// socket yet would hand the gate the session's, which in a shared host session
// is nobody's. A claimed placeholder was not made by createMirrorWindow, so the
// stamp re-applies the identity there too.
func TestStampMirrorWindowStampsIdentityBeforeBridgeWin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		placed bool
		opts   [][2]string
	}{
		{"dedicated", false, [][2]string{{"@bridge_sock", "/s|1.sock"}, {"@bridge_host", "lab"}, {"@bridge_session", "rem sess"}}},
		{"placed", true, [][2]string{{"@bridge_sock", "/s|1.sock"}, {"@bridge_host", "lab"}, {"@bridge_session", "rem sess"}, {"@bridge_placed", "1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cfg := newStartupFake()
			cfg.SockPath, cfg.RemoteHost, cfg.RemoteSession = "/s|1.sock", "lab", "rem sess"
			cfg.LocalSess, cfg.Placed = "work", tc.placed
			if tc.placed {
				cfg.LocalTarget = "$4"
			}

			stampMirrorWindow(cfg, "@7", "editor")

			calls := f.log.snapshot()
			prev := -1
			for _, o := range tc.opts {
				i := identityIndex(calls, "@7", o[0])
				if i < 0 {
					t.Fatalf("no %s stamp on @7 among %q", o[0], calls)
				}
				if want := []string{"set-option", "-w", "-t", "@7", o[0], o[1]}; !reflect.DeepEqual(calls[i], want) {
					t.Errorf("%s stamp = %q, want %q", o[0], calls[i], want)
				}
				if i <= prev {
					t.Errorf("%s stamped at call %d, want after call %d", o[0], i, prev)
				}
				prev = i
			}
			if win := identityIndex(calls, "@7", "@bridge_win"); win <= prev {
				t.Errorf("@bridge_win stamped at call %d, want after the identity (call %d)", win, prev)
			}
			if !tc.placed && identityIndex(calls, "@7", placedOption) >= 0 {
				t.Errorf("a dedicated mirror window was stamped %s: %q", placedOption, calls)
			}
		})
	}
}

// createMirrorWindow stamps the identity in the same tmux invocation as the
// new-window: one command list runs to its end before the server's next
// redraw, so no client ever sees the window unmarked. `{end}` is the window
// `-a` just appended; `;` is its own argv element, tmux's command separator.
func TestCreateMirrorWindowStampsIdentityInTheSameCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			"dedicated",
			Config{LocalSess: "h-s", SockPath: "/s.sock", RemoteHost: "h", RemoteSession: "r s"},
			[]string{
				"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", "h-s:{end}", ";",
				"set-option", "-w", "-t", "h-s:{end}", "@bridge_sock", "/s.sock", ";",
				"set-option", "-w", "-t", "h-s:{end}", "@bridge_host", "h", ";",
				"set-option", "-w", "-t", "h-s:{end}", "@bridge_session", "r s",
			},
		},
		{
			"placed",
			Config{LocalSess: "work", LocalTarget: "$4", Placed: true, SockPath: "/s.sock", RemoteHost: "h", RemoteSession: "r s"},
			[]string{
				"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", "$4:{end}", ";",
				"set-option", "-w", "-t", "$4:{end}", "@bridge_sock", "/s.sock", ";",
				"set-option", "-w", "-t", "$4:{end}", "@bridge_host", "h", ";",
				"set-option", "-w", "-t", "$4:{end}", "@bridge_session", "r s", ";",
				"set-option", "-w", "-t", "$4:{end}", "@bridge_placed", "1",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outCalls [][]string
			cfg := tc.cfg
			cfg.LocalTmux = func(args ...string) error {
				t.Errorf("createMirrorWindow ran a separate LocalTmux %q", args)
				return nil
			}
			cfg.LocalTmuxOut = func(args ...string) (string, error) {
				outCalls = append(outCalls, args)
				return "@42\n", nil
			}
			got, err := createMirrorWindow(cfg)
			if err != nil || got != "@42" {
				t.Fatalf("createMirrorWindow = %q, %v; want @42", got, err)
			}
			if len(outCalls) != 1 {
				t.Fatalf("LocalTmuxOut calls = %q, want exactly one", outCalls)
			}
			if !reflect.DeepEqual(outCalls[0], tc.want) {
				t.Errorf("LocalTmuxOut argv =\n%q\nwant\n%q", outCalls[0], tc.want)
			}
		})
	}
}

// A placed startup takes no placeholder from the host session — its first
// window is the user's — so with none carried over every remote window gets a
// window of its own in $N.
func TestPlacedStartupCreatesEveryWindow(t *testing.T) {
	f, cfg := newStartupFake()
	cfg.LocalSess, cfg.LocalTarget, cfg.Placed = "work", "$4", true
	reg := newRegistry()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}}

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), setupWindowRT(startupScript("@1", "@2")))
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "" {
		t.Errorf("placeholder = %q, want \"\"", placeholder)
	}
	for _, c := range f.log.snapshot() {
		if c[0] == "list-windows" {
			t.Errorf("placed startup read the host session's windows for a placeholder: %q", c)
		}
		if c[0] == "new-window" && c[7] != "$4:{end}" {
			t.Errorf("new-window target = %q, want $4:{end}", c[7])
		}
	}
	if n := f.newWindowCount(); n != 2 {
		t.Errorf("new-window calls = %d, want 2", n)
	}
	if mw, ok := reg.byRemoteID("@1"); !ok || mw.localWin != "@101" {
		t.Errorf("reg @1 = %+v, ok=%v, want localWin @101 (a created window, not the user's @100)", mw, ok)
	}
	if kills := f.killWindows(); len(kills) != 0 {
		t.Errorf("kill-window calls = %v, want none", kills)
	}
}

// A placeholder a rebuild left in the host session is claimed by the first
// remote window, exactly as a dedicated startup claims the launcher's window.
func TestPlacedStartupClaimsTheCarriedPlaceholder(t *testing.T) {
	f, cfg := newStartupFake()
	cfg.LocalSess, cfg.LocalTarget, cfg.Placed, cfg.placeholder = "work", "$4", true, "@9"
	reg := newRegistry()
	remoteWins := []remoteWindow{{id: "@1"}, {id: "@2"}}

	placeholder, err := mirrorStartupWindows(cfg, remoteWins, func(string) {}, NewRouter(), noHellos, newCtlState(), reg, newConverger(), setupWindowRT(startupScript("@1", "@2")))
	if err != nil {
		t.Fatalf("mirrorStartupWindows: %v", err)
	}
	if placeholder != "" {
		t.Errorf("placeholder = %q, want \"\": @1 claimed it", placeholder)
	}
	if mw, ok := reg.byRemoteID("@1"); !ok || mw.localWin != "@9" {
		t.Errorf("reg @1 = %+v, ok=%v, want localWin @9 (the carried placeholder)", mw, ok)
	}
	if mw, ok := reg.byRemoteID("@2"); !ok || mw.localWin != "@101" {
		t.Errorf("reg @2 = %+v, ok=%v, want localWin @101", mw, ok)
	}
	if n := f.newWindowCount(); n != 1 {
		t.Errorf("new-window calls = %d, want 1", n)
	}
}
