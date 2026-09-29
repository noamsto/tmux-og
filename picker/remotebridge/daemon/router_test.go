package daemon

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRouterRoutesByPane(t *testing.T) {
	r := NewRouter()
	var a, b bytes.Buffer
	r.Register("%1", &a)
	r.Register("%2", &b)

	r.Route("%1", []byte("one"))
	r.Route("%2", []byte("two"))
	r.Route("%1", []byte("-more"))
	r.Route("%9", []byte("dropped")) // unregistered: dropped and counted
	r.Unregister("%9")

	if a.String() != "one-more" {
		t.Errorf("pane %%1 got %q, want %q", a.String(), "one-more")
	}
	if b.String() != "two" {
		t.Errorf("pane %%2 got %q, want %q", b.String(), "two")
	}

	r.Unregister("%1")
	r.Route("%1", []byte("after-unregister"))
	if a.String() != "one-more" {
		t.Errorf("pane %%1 received after unregister: %q", a.String())
	}
	if r.dropped != 2 {
		t.Errorf("dropped = %d, want 2", r.dropped)
	}
}

func TestRouterBuffersBeforeRegisterInOrder(t *testing.T) {
	r := NewRouter()
	r.Route("%1", []byte("before-1"))
	r.Route("%1", []byte("before-2"))

	var sink bytes.Buffer
	r.Register("%1", &sink)
	r.Route("%1", []byte("after"))

	if got, want := sink.String(), "before-1before-2after"; got != want {
		t.Fatalf("routed output = %q, want %q", got, want)
	}
}

func TestRouterBoundsPreRegistrationBuffer(t *testing.T) {
	r := NewRouter()
	for range routerPendingMaxFrames + 1 {
		r.Route("%1", []byte("x"))
	}

	p := r.pending["%1"]
	if p == nil {
		t.Fatal("no pending buffer for %1")
	}
	if got := len(p.frames); got != routerPendingMaxFrames {
		t.Fatalf("pending frames = %d, want %d", got, routerPendingMaxFrames)
	}
	if r.dropped != 1 {
		t.Fatalf("dropped = %d, want 1", r.dropped)
	}

	var sink bytes.Buffer
	r.Register("%1", &sink)
	if got, want := sink.Len(), routerPendingMaxFrames; got != want {
		t.Fatalf("flushed bytes = %d, want %d", got, want)
	}
}

// routeDropLocked logs a reason once; the buffer cap must still hold for every
// later overflow, not just the first.
func TestRouterKeepsBoundingAfterFirstDrop(t *testing.T) {
	r := NewRouter()
	for range routerPendingMaxFrames + 3 {
		r.Route("%1", []byte("x"))
	}

	p := r.pending["%1"]
	if p == nil {
		t.Fatal("no pending buffer for %1")
	}
	if got := len(p.frames); got != routerPendingMaxFrames {
		t.Fatalf("pending frames = %d, want %d", got, routerPendingMaxFrames)
	}
	if r.dropped != 3 {
		t.Fatalf("dropped = %d, want 3", r.dropped)
	}
}

func TestRouterBoundsTotalPreRegistrationBuffer(t *testing.T) {
	r := NewRouter()
	frame := bytes.Repeat([]byte("x"), routerPendingMaxBytes)
	r.pendingMaxTotalBytes = 2 * len(frame)

	r.Route("%1", frame)
	r.Route("%2", frame)
	r.Route("%3", frame)

	if got, want := r.pendingBytes, 2*len(frame); got != want {
		t.Fatalf("pending bytes = %d, want %d", got, want)
	}
	if got, want := len(r.pending), 2; got != want {
		t.Fatalf("pending panes = %d, want %d", got, want)
	}
	if got, want := r.dropped, 1; got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func TestRouterRouteDropsLogReasons(t *testing.T) {
	r := NewRouter()
	r.pendingLifetime = time.Hour
	logs := captureRouterStderr(t, func() {
		r.Register("%nil", nil)
		r.Route("%nil", []byte("dropped"))

		r.Register("%gone", &bytes.Buffer{})
		r.Unregister("%gone")
		r.Route("%gone", []byte("dropped"))

		r.Route("%oversized", bytes.Repeat([]byte("x"), routerPendingMaxBytes+1))
		for i := 0; i <= routerPendingMaxFrames; i++ {
			r.Route("%per-pane", []byte("x"))
		}

		r.pendingMaxTotalBytes = 1
		r.Route("%total", []byte("xx"))
	})

	for _, reason := range []string{
		"registered without a sink",
		"pane is gone",
		"frame exceeds per-pane limit",
		"per-pane buffer full",
		"total pre-registration buffer full",
	} {
		if !strings.Contains(logs, reason) {
			t.Errorf("logs = %q, want reason %q", logs, reason)
		}
	}
}

func TestRouterRouteGoneDropsLogOnce(t *testing.T) {
	r := NewRouter()
	r.Register("%gone", &bytes.Buffer{})
	r.Unregister("%gone")

	logs := captureRouterStderr(t, func() {
		for range 100 {
			r.Route("%gone", []byte("dropped"))
		}
	})

	if got, want := strings.Count(logs, "\n"), 1; got != want {
		t.Fatalf("gone-pane log lines = %d, want %d; logs = %q", got, want, logs)
	}
	if got, want := r.dropped, 100; got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func TestRouterRouteDropLoggingStateBounded(t *testing.T) {
	r := NewRouter()
	frame := bytes.Repeat([]byte("x"), routerPendingMaxBytes+1)
	logs := captureRouterStderr(t, func() {
		for i := range 1000 {
			r.Route("%oversized-"+strconv.Itoa(i), frame)
		}
	})

	if got, want := strings.Count(logs, "\n"), 1; got != want {
		t.Fatalf("oversized-pane log lines = %d, want %d", got, want)
	}
	if got, want := len(r.routeDropLogged), 1; got != want {
		t.Fatalf("route drop logging state entries = %d, want %d", got, want)
	}
	if got, want := r.dropped, 1000; got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func captureRouterStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = write
	f()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = old
	defer func() { _ = read.Close() }()
	logs, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return string(logs)
}

func TestRouterExpiryCleansPendingState(t *testing.T) {
	r := NewRouter()
	r.pendingLifetime = time.Hour
	r.Route("%1", []byte("before-expiry"))
	p := r.pending["%1"]

	r.expirePending("%1", p)

	if _, ok := r.pending["%1"]; ok {
		t.Fatal("expired pane still has pending output")
	}
	if got := r.pendingBytes; got != 0 {
		t.Fatalf("pending bytes = %d, want 0", got)
	}
	if got, want := r.dropped, 1; got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func TestRouterRegisterNilDropsPendingOutput(t *testing.T) {
	r := NewRouter()
	r.Route("%1", []byte("before-register"))

	r.Register("%1", nil)

	if _, ok := r.pending["%1"]; ok {
		t.Fatal("nil registration left pending output")
	}
	if got, want := r.pendingBytes, 0; got != want {
		t.Fatalf("pending bytes = %d, want %d", got, want)
	}
	if got, want := r.dropped, 1; got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

func TestRouterGoneStateExpires(t *testing.T) {
	r := NewRouter()
	r.goneLifetime = time.Hour
	r.Register("%1", &bytes.Buffer{})
	r.Unregister("%1")
	g := r.gone["%1"]

	r.expireGone("%1", g)

	if _, ok := r.gone["%1"]; ok {
		t.Fatal("expired closed pane state remains")
	}
	r.Route("%1", []byte("after-expiry"))
	if _, ok := r.pending["%1"]; !ok {
		t.Fatal("pane did not become bufferable after closed state expiry")
	}
}

func TestRouterUnregisterDropsPendingOutput(t *testing.T) {
	r := NewRouter()
	r.Route("%1", []byte("before-close"))
	r.Unregister("%1")

	var sink bytes.Buffer
	r.Register("%1", &sink)
	if sink.Len() != 0 {
		t.Fatalf("flushed output for unregistered pane: %q", sink.String())
	}

	r.Route("%1", []byte("after-register"))
	if got, want := sink.String(), "after-register"; got != want {
		t.Fatalf("routed output = %q, want %q", got, want)
	}
	if r.dropped != 1 {
		t.Fatalf("dropped = %d, want 1", r.dropped)
	}
}

func TestRouterDemuxAcrossWindows(t *testing.T) {
	r := NewRouter()
	var a, b capBuf      // capBuf from daemon_test.go (same package)
	r.Register("%1", &a) // window @1's pane
	r.Register("%9", &b) // window @2's pane
	r.Route("%1", []byte("A1"))
	r.Route("%9", []byte("B9"))
	r.Route("%99", []byte("DROP")) // no sink registered
	if a.String() != "A1" || b.String() != "B9" {
		t.Fatalf("misrouted: a=%q b=%q", a.String(), b.String())
	}
	// Unregister %1 (its window closed); further output for it is dropped.
	r.Unregister("%1")
	r.Route("%1", []byte("X"))
	if a.String() != "A1" {
		t.Errorf("output after unregister leaked: %q", a.String())
	}
}

func TestCloseWindowUnregistersOnlyItsPanes(t *testing.T) {
	reg := newRegistry()
	w1 := reg.add("@1", "@101")
	w1.remotePanes = []string{"%1", "%2"}
	w2 := reg.add("@2", "@102")
	w2.remotePanes = []string{"%9"}
	router := NewRouter()
	var s1, s2, s9 capBuf
	router.Register("%1", &s1)
	router.Register("%2", &s2)
	router.Register("%9", &s9)
	cfg := Config{LocalTmux: func(...string) error { return nil }}

	closeWindow(cfg, router, newCtlState(), reg, newConverger(), "@1")

	router.Route("%1", []byte("x"))
	router.Route("%2", []byte("y"))
	router.Route("%9", []byte("z"))
	if s1.String() != "" || s2.String() != "" {
		t.Errorf("closed window's panes still routed: %q %q", s1.String(), s2.String())
	}
	if s9.String() != "z" {
		t.Errorf("sibling window's pane stopped routing: %q", s9.String())
	}
	if _, ok := reg.byRemoteID("@1"); ok {
		t.Error("@1 still in registry after close")
	}
}
