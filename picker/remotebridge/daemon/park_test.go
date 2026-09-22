package daemon

import (
	"strings"
	"testing"
	"time"
)

// tryRecv reports whether ch had a value ready, without blocking — the same
// non-blocking read poke and arm/disarm themselves use.
func tryRecv(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestParkWakerPokeWhileDisarmedDoesNotDeliver(t *testing.T) {
	w := newParkWaker()
	w.poke()
	if tryRecv(w.C()) {
		t.Error("poke while disarmed must not queue a wake")
	}
}

func TestParkWakerPokeWhileArmedDeliversExactlyOne(t *testing.T) {
	w := newParkWaker()
	w.arm()
	w.poke()
	w.poke() // must not block on the already-full buffer

	if !tryRecv(w.C()) {
		t.Fatal("armed poke must queue a wake")
	}
	if tryRecv(w.C()) {
		t.Error("two pokes before any read must coalesce into one wake")
	}
}

func TestParkWakerArmDrainsAStalePoke(t *testing.T) {
	w := newParkWaker()
	w.arm()
	w.poke()
	w.disarm()

	w.arm() // a fresh park; the poke above answered nothing this park asked
	if tryRecv(w.C()) {
		t.Error("arm must drain a stale poke left by an earlier park")
	}
}

func TestFocusEdgeNoWakeWhenViewingAtReset(t *testing.T) {
	mtime := time.Now()
	viewing := true
	f := &focusEdge{
		nudged:  func() (time.Time, bool) { return mtime, true },
		viewing: func() bool { return viewing },
	}
	f.reset()

	mtime = mtime.Add(time.Second) // a touch, but nothing changed about who's looking
	if f.poll() {
		t.Error("must not wake — the user was already viewing at park entry")
	}
}

func TestFocusEdgeWakesOnFalseToTrueAfterMtimeAdvance(t *testing.T) {
	mtime := time.Now()
	viewing := false
	f := &focusEdge{
		nudged:  func() (time.Time, bool) { return mtime, true },
		viewing: func() bool { return viewing },
	}
	f.reset()

	viewing = true
	mtime = mtime.Add(time.Second)
	if !f.poll() {
		t.Error("want a wake on the false->true viewing edge")
	}
}

func TestFocusEdgeNoWakeWithoutAnMtimeAdvance(t *testing.T) {
	mtime := time.Now()
	viewing := false
	f := &focusEdge{
		nudged:  func() (time.Time, bool) { return mtime, true },
		viewing: func() bool { return viewing },
	}
	f.reset()

	viewing = true // nothing touched the nudge file to explain it
	if f.poll() {
		t.Error("must not wake without an mtime advance — viewing is re-read only on a touch")
	}
}

func TestFocusEdgeNoSecondWakeWhileStillViewing(t *testing.T) {
	mtime := time.Now()
	viewing := false
	f := &focusEdge{
		nudged:  func() (time.Time, bool) { return mtime, true },
		viewing: func() bool { return viewing },
	}
	f.reset()

	viewing = true
	mtime = mtime.Add(time.Second)
	if !f.poll() {
		t.Fatal("want a wake on the first edge")
	}

	mtime = mtime.Add(time.Second) // another touch; still viewing, not a new edge
	if f.poll() {
		t.Error("must not wake again while still viewing")
	}
}

// argvKey turns an argv into a comparable key, so a set of expected calls can
// be checked against the recorded calls regardless of registry iteration
// order across windows.
func argvKey(argv []string) string { return strings.Join(argv, "|") }

func TestDimMirrorStampsBothStyleOptionsOnEveryWindow(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	reg.add("@2", "@102")
	var calls [][]string
	cfg := Config{
		LocalSess: "host-work",
		LocalTmux: func(args ...string) error { calls = append(calls, args); return nil },
	}

	dimMirror(cfg, reg)

	want := []string{
		argvKey([]string{"set-option", "-w", "-t", "@101", "window-style", parkDimStyle}),
		argvKey([]string{"set-option", "-w", "-t", "@101", "window-active-style", parkDimStyle}),
		argvKey([]string{"set-option", "-w", "-t", "@102", "window-style", parkDimStyle}),
		argvKey([]string{"set-option", "-w", "-t", "@102", "window-active-style", parkDimStyle}),
	}
	got := map[string]bool{}
	for _, argv := range calls {
		got[argvKey(argv)] = true
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want exactly %d stamps (2 windows x 2 options)", calls, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing call %q among %v", w, calls)
		}
	}
}

func TestUndimMirrorUnsetsBothStyleOptionsOnEveryWindow(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	var calls [][]string
	cfg := Config{
		LocalSess: "host-work",
		LocalTmux: func(args ...string) error { calls = append(calls, args); return nil },
	}

	undimMirror(cfg, reg)

	want := []string{
		argvKey([]string{"set-option", "-w", "-u", "-t", "@101", "window-style"}),
		argvKey([]string{"set-option", "-w", "-u", "-t", "@101", "window-active-style"}),
	}
	got := map[string]bool{}
	for _, argv := range calls {
		got[argvKey(argv)] = true
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want exactly %d unstamps", calls, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing call %q among %v", w, calls)
		}
	}
}

func TestDimMirrorNoOpWithNoLocalSession(t *testing.T) {
	reg := newRegistry()
	reg.add("@1", "@101")
	called := false
	cfg := Config{
		LocalTmux: func(args ...string) error { called = true; return nil },
	}

	dimMirror(cfg, reg)

	if called {
		t.Error("dimMirror must no-op with an empty LocalSess, matching setBridgeState")
	}
}

func TestWakeBackoffMatchesTheDesignedSchedule(t *testing.T) {
	b := WakeBackoff(time.Now)
	if b.Base != 500*time.Millisecond || b.Ceiling != 5*time.Second ||
		b.MaxAttempts != 10 || b.MaxElapsed != 30*time.Second {
		t.Errorf("WakeBackoff = %+v, want Base 500ms, Ceiling 5s, MaxAttempts 10, MaxElapsed 30s", b)
	}
}
