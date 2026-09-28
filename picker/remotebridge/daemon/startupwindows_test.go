package daemon

import (
	"errors"
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
