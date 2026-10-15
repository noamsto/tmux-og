package daemon

import (
	"reflect"
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

func TestPlacedViewing(t *testing.T) {
	const sockA = "/tmp/og-into-a|b.sock"
	for _, tc := range []struct {
		name, out string
		want      bool
	}{
		{"a client on an own window", "/tmp/og-into-other.sock\n\n" + sockA + "\n", true},
		{"only other socks and local windows", "/tmp/og-into-other.sock\n\n", false},
		{"no clients", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := placedViewing(tc.out, sockA); got != tc.want {
				t.Errorf("placedViewing(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// Placed viewing asks the host session's clients for their window's sock by
// session id; dedicated keeps its server-wide session-name read.
func TestLocalViewingPlacedReadsOwnWindowsOnly(t *testing.T) {
	var argv []string
	reply := ""
	cfg := Config{
		LocalSess:   "work",
		LocalTarget: "$4",
		Placed:      true,
		SockPath:    "/tmp/og-into-1.sock",
		LocalTmuxOut: func(args ...string) (string, error) {
			argv = args
			return reply, nil
		},
	}
	reply = "\n/tmp/og-into-2.sock\n"
	if localViewing(cfg) {
		t.Error("placed: a client on a local window or another daemon's mirror read as viewing")
	}
	if want := []string{"list-clients", "-t", "$4", "-F", "#{@bridge_sock}"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("placed asked %q, want %q", argv, want)
	}
	reply = "\n/tmp/og-into-1.sock\n"
	if !localViewing(cfg) {
		t.Error("placed: a client on an own window did not read as viewing")
	}

	cfg.Placed, cfg.LocalTarget = false, ""
	reply = "work\n"
	if !localViewing(cfg) {
		t.Error("dedicated: a client on the mirror session did not read as viewing")
	}
	if want := []string{"list-clients", "-F", "#{client_session}"}; !reflect.DeepEqual(argv, want) {
		t.Errorf("dedicated asked %q, want %q", argv, want)
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

func TestRestoreBackoffMatchesTheDesignedSchedule(t *testing.T) {
	b := RestoreBackoff(time.Now)
	if b.Base != 1*time.Second || b.Ceiling != 5*time.Second ||
		b.MaxAttempts != 30 || b.MaxElapsed != 60*time.Second {
		t.Errorf("RestoreBackoff = %+v, want Base 1s, Ceiling 5s, MaxAttempts 30, MaxElapsed 60s", b)
	}
}

func TestProbeBackoffIsOneImmediateAttempt(t *testing.T) {
	b := probeBackoff(time.Now)
	start := time.Now()
	if d, ok := b.Next(1, start); d != 0 || !ok {
		t.Errorf("Next(1, start) = (%v, %v), want (0, true)", d, ok)
	}
	if _, ok := b.Next(2, start); ok {
		t.Error("Next(2, start) = ok true, want false: probeBackoff is one attempt")
	}
}
