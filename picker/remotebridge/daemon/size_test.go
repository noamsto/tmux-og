package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/graphics"
)

func TestConvergeCmd(t *testing.T) {
	// The per-window form (@id:WxH) — the whole-client form loses to any other
	// client attached to the remote session.
	if got := ConvergeCmd("@3", 210, 52); got != "refresh-client -C @3:210x52" {
		t.Errorf("ConvergeCmd = %q, want %q", got, "refresh-client -C @3:210x52")
	}
}

// The cap is discarded for a window tmux is not sizing, so the opt-out is what
// makes ConvergeCmd reach every mirrored window rather than only the remote's
// current one (#478).
func TestAggressiveResizeOffCmd(t *testing.T) {
	if got := AggressiveResizeOffCmd("@3"); got != "set-option -w -t @3 aggressive-resize off" {
		t.Errorf("AggressiveResizeOffCmd = %q, want %q", got, "set-option -w -t @3 aggressive-resize off")
	}
	// Window-scoped: a broader set would also change remote windows the bridge
	// does not mirror.
	if !strings.Contains(AggressiveResizeOffCmd("@3"), " -w ") {
		t.Error("the opt-out must be window-scoped")
	}
}

// Without a size of its own the control client is 80 columns and no rows, and
// under `window-size latest` a window created on the remote is born at that.
func TestClientSizeCmd(t *testing.T) {
	// The whole-client form: no @id, so it is the client's own size rather
	// than a per-window cap.
	if got := ClientSizeCmd(210, 52); got != "refresh-client -C 210x52" {
		t.Errorf("ClientSizeCmd = %q, want %q", got, "refresh-client -C 210x52")
	}
	if strings.Contains(ClientSizeCmd(210, 52), "@") {
		t.Error("the client size must not carry a window target")
	}
}

// The client size shares the converger's map, so it re-sends on a change and is
// skipped otherwise — and never collides with a window, whose ids are all @N.
func TestConvergerTracksClientSizeSeparately(t *testing.T) {
	cv := newConverger()

	if !cv.need(clientSizeKey, 100, 30) {
		t.Fatal("first client size should be needed")
	}
	if cv.need(clientSizeKey, 100, 30) {
		t.Error("re-asserting the same client size should be skipped")
	}
	// A window at the same size is tracked independently of the client.
	if !cv.need("@1", 100, 30) {
		t.Error("a window's first cap should be needed, not shadowed by the client")
	}
	if !cv.need(clientSizeKey, 120, 40) {
		t.Error("a changed client size should be needed")
	}
	if strings.HasPrefix(clientSizeKey, "@") {
		t.Errorf("clientSizeKey %q could collide with a remote window id", clientSizeKey)
	}
}

func TestConvergerNeedTracksPerWindow(t *testing.T) {
	cv := newConverger()

	if !cv.need("@1", 100, 30) {
		t.Fatal("first assertion for @1 should be needed")
	}
	if cv.need("@1", 100, 30) {
		t.Error("re-asserting the same size for @1 should be skipped")
	}
	// A sibling window has its own last-asserted size.
	if !cv.need("@2", 100, 30) {
		t.Error("first assertion for @2 should be needed")
	}
	if !cv.need("@1", 120, 40) {
		t.Error("a changed size for @1 should be needed")
	}

	// A closed window forgets, so a re-add re-asserts rather than assuming the
	// remote still holds the old cap.
	cv.forget("@1")
	if !cv.need("@1", 120, 40) {
		t.Error("after forget, @1 should re-assert")
	}
}

// The invariant: the recorded size is never ahead of what the remote was told.
// need() records before the write, so a write that never happened has to be
// undone or the next tick skips the window forever.
func TestConvergerUnrecordRetriesAfterAFailedWrite(t *testing.T) {
	cv := newConverger()

	if !cv.need("@1", 100, 30) {
		t.Fatal("first assertion for @1 should be needed")
	}
	cv.unrecord("@1", 100, 30)
	if !cv.need("@1", 100, 30) {
		t.Error("after a failed write @1 should re-assert, not be treated as told")
	}
}

// setupWindow and watchLocalClient both assert, on different goroutines, so an undo
// can arrive after the other one has recorded a different size — which is by
// then the current truth and must survive.
func TestConvergerUnrecordKeepsASizeAssertedInBetween(t *testing.T) {
	cv := newConverger()

	cv.need("@1", 100, 30)
	if !cv.need("@1", 120, 40) {
		t.Fatal("a changed size for @1 should be needed")
	}
	cv.unrecord("@1", 100, 30)
	if cv.need("@1", 120, 40) {
		t.Error("undoing the stale 100x30 write cleared the newer 120x40 record")
	}
}

// A filtered daemon caps only the windows its selector picked, and still sends
// the whole-client size: without it, a control client that set only caps is
// never ignored and contributes 80x0 to every other window.
func TestFilteredSizePassCapsOnlySelectedWindows(t *testing.T) {
	_, reg, cv, _, _ := filteredReconcile(t)
	tick := make(chan time.Time)
	stop := make(chan struct{})
	nudged := func() (time.Time, bool) { return time.Now(), true }
	area := func() (int, int) { return 120, 40 }
	view := &Viewing{Relay: graphics.NewRelaySource(graphics.Relay{})}
	var sent []string
	send := func(s ...string) bool { sent = append(sent, s...); return true }

	done := make(chan struct{})
	go func() {
		watchLocalClient(area, nudged, func() string { return "" }, noResolveView, view, "rem", reg, cv, send, stop, tick)
		close(done)
	}()
	tick <- time.Now()
	close(stop)
	<-done

	if want := []string{ClientSizeCmd(120, 40), ConvergeCmd("@1", 120, 40)}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent = %q, want %q", sent, want)
	}
}
