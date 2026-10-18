package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

const holdEnterAlt = "\x1b\\\x1b[?1049h"

// holdLocalLog records every local tmux call in order, naming the hold read
// "read" and the barrier probe "poll" and every other call by its argv[0].
type holdLocalLog struct {
	mu      sync.Mutex
	entries []string
	argv    [][]string
}

func (l *holdLocalLog) add(argv []string) string {
	name := argv[0]
	switch {
	case argv[0] == "display-message" && slices.Contains(argv, holdHeaderFormat):
		name = "read"
	case argv[0] == "display-message" && slices.Contains(argv, "#{alternate_on}"):
		name = "poll"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, name)
	l.argv = append(l.argv, append([]string(nil), argv...))
	return name
}

func (l *holdLocalLog) index(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Index(l.entries, name)
}

func (l *holdLocalLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// holdReply is a hold read's answer for one live main-screen pane,
// alternate-screen on, with exactly h rows after its header.
func holdReply(remoteID string, w, h int) string {
	return holdReplyDead(remoteID, w, h, false)
}

// holdReplyDead is holdReply for a pane whose pane_dead is set as given.
func holdReplyDead(remoteID string, w, h int, dead bool) string {
	return holdReplyFull(remoteID, w, h, h, dead)
}

// holdReplyWin is holdReply for a window winH rows tall.
func holdReplyWin(remoteID string, w, h, winH int) string {
	return holdReplyFull(remoteID, w, h, winH, false)
}

func holdReplyFull(remoteID string, w, h, winH int, dead bool) string {
	d := 0
	if dead {
		d = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|0|1|%d|%d|0|5|%d|%d\n", remoteID, w, h, d, winH)
	for i := range h {
		fmt.Fprintf(&b, "row%d\n", i)
	}
	return b.String()
}

// holdCloseWindow is a two-pane mirror whose %4 closes on the remote.
func holdCloseWindow() *mirrorWindow {
	return &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}
}

const holdOnePane = "bd67,190x45,0,0,3"

// holdCloseOut answers the close's local reads: the hold read with reply, the
// barrier poll with 1, and list-panes with the window before or after the kill.
func holdCloseOut(log *holdLocalLog, reply func() (string, error), killed func() bool) func(...string) (string, error) {
	return func(argv ...string) (string, error) {
		switch log.add(argv) {
		case "read":
			return reply()
		case "poll":
			return "1\n", nil
		case "list-panes":
			if killed() {
				return "%l3 0\n", nil
			}
			return "%l3 0\n%l4 0\n", nil
		}
		return "", nil
	}
}

// TestReconcileHoldCloseHoldsTheSurvivorBeforeTheKill is the fix's ordering: the
// survivor is parked on the alternate screen, and confirmed there, before the
// local kill-pane reflows it; the reseed then releases it, so the deferred
// release writes nothing.
func TestReconcileHoldCloseHoldsTheSurvivorBeforeTheKill(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + holdOnePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1",
		"%begin 1 3 1", "SURVIVOR", "%end 1 3 1",
		"%begin 1 4 1", "@1 " + holdOnePane + " %3 0", "%end 1 4 1",
	}, "\n") + "\n")

	log := &holdLocalLog{}
	killed := false
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			if log.add(argv) == "kill-pane" {
				killed = true
			}
			return nil
		},
		LocalTmuxOut: holdCloseOut(log, func() (string, error) { return holdReply("%3", 95, 45), nil }, func() bool { return killed }),
	}
	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	read, poll, kill := log.index("read"), log.index("poll"), log.index("kill-pane")
	if read < 0 || poll < 0 || kill < 0 || read >= poll || poll >= kill {
		t.Fatalf("local calls = %v, want the hold read, then the poll, then kill-pane", log.snapshot())
	}

	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("first frame = %v %q, want the hold output", f.Type, f.Payload)
	}
	if !strings.Contains(string(f.Payload), "row44") {
		t.Errorf("hold payload %q is missing the captured rows", f.Payload)
	}
	if f := readHoldFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("second frame = %v %q, want the resize", f.Type, f.Payload)
	}
	if f := readHoldFrame(t, peer); f.Type != wire.FrameSeed {
		t.Fatalf("third frame = %v %q, want the seed", f.Type, f.Payload)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldRefusedSelectLayoutReleases covers the pass that held a pane
// but never reseeded it: the deferred release takes it off the alternate
// screen and marks it for a reseed from the remote.
func TestReconcileHoldRefusedSelectLayoutReleases(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + holdOnePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "@1 " + holdOnePane + " %3 0", "%end 1 2 1",
	}, "\n") + "\n")

	log := &holdLocalLog{}
	killed := false
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			switch log.add(argv) {
			case "kill-pane":
				killed = true
			case "select-layout":
				return errors.New("select-layout refused")
			}
			return nil
		},
		LocalTmuxOut: holdCloseOut(log, func() (string, error) { return holdReply("%3", 95, 45), nil }, func() bool { return killed }),
	}
	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if log.index("select-layout") < 0 {
		t.Fatalf("local calls = %v, want a select-layout to refuse", log.snapshot())
	}
	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("first frame = %v %q, want the hold output", f.Type, f.Payload)
	}
	requireFrame(t, peer, wire.FrameOutput, holdLeaveAlt)
	if n, ok := settleDirty(t, sink, 5*time.Second); !ok || n != 1 {
		t.Errorf("takeDirty = %d, %v; want the release's one drop", n, ok)
	}
}

// TestReconcileHoldReadErrorLeavesThePassAlone: a failed read holds nothing and
// the close runs exactly as it would without the hold.
func TestReconcileHoldReadErrorLeavesThePassAlone(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + holdOnePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "0 0 0 0 0 0 0 0 0", "%end 1 2 1",
		"%begin 1 3 1", "SURVIVOR", "%end 1 3 1",
		"%begin 1 4 1", "@1 " + holdOnePane + " %3 0", "%end 1 4 1",
	}, "\n") + "\n")

	log := &holdLocalLog{}
	killed := false
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			if log.add(argv) == "kill-pane" {
				killed = true
			}
			return nil
		},
		LocalTmuxOut: holdCloseOut(log, func() (string, error) { return "", errors.New("read failed") }, func() bool { return killed }),
	}
	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	if log.index("read") < 0 {
		t.Fatalf("local calls = %v, want the hold read attempted", log.snapshot())
	}
	if log.index("poll") >= 0 {
		t.Errorf("local calls = %v, want no poll after a failed read", log.snapshot())
	}
	if log.index("kill-pane") < 0 {
		t.Errorf("local calls = %v, want kill-pane to still run", log.snapshot())
	}
	if f := readHoldFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("first frame = %v %q, want the resize with no hold ahead of it", f.Type, f.Payload)
	}
	if f := readHoldFrame(t, peer); f.Type != wire.FrameSeed {
		t.Fatalf("second frame = %v %q, want the seed", f.Type, f.Payload)
	}
	requireSilence(t, peer)
}

// holdTwoPane is a side-by-side window of %3 (95x45) and %4 (94x45) in a
// 190x45 root.
func holdTwoPane(t *testing.T) controlmode.Layout {
	t.Helper()
	L, err := controlmode.ParseLayout("0000,190x45,0,0{95x45,0,0,3,94x45,96,0,4}")
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	return L
}

// TestReconcileHoldSkipsPassesThatCanShrinkAPane: on the alternate screen a
// height shrink deletes rows from the top and the regrow adds blanks at the
// bottom, so a pass that can shrink a held pane must neither read nor hold.
func TestReconcileHoldSkipsPassesThatCanShrinkAPane(t *testing.T) {
	L := holdTwoPane(t)
	order := RemotePaneOrder(L)
	tests := []struct {
		name        string
		remote      []string
		localPanes  []string
		ops         paneOps
		zoomed      bool
		appliedZoom bool
		layout      string
	}{
		{"append splits the last pane", []string{"%3"}, []string{"%l3"}, paneOps{Append: []string{"%4"}}, false, false, L.Raw},
		{"swap moves a pane through another's cell", []string{"%4", "%3"}, []string{"%l4", "%l3"}, paneOps{Swaps: [][2]int{{0, 1}}}, false, false, L.Raw},
		{"zoom-on transition", order, []string{"%l3", "%l4"}, paneOps{}, true, false, L.Raw},
		{"locally zoomed structural kill", order, []string{"%l3", "%l4"}, paneOps{Remove: []int{1}}, true, true, L.Raw},
		{"locally zoomed geometry-only", order, []string{"%l3", "%l4"}, paneOps{}, true, true, "stale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink, peer := holdPipe(t)
			router := NewRouter()
			router.Register("%3", sink)
			router.Register("%4", sink)
			w := &mirrorWindow{
				remoteID: "@1", localWin: "@101",
				remotePanes: tc.remote, localPanes: tc.localPanes,
				layout: tc.layout, appliedZoom: tc.appliedZoom,
			}
			log := &holdLocalLog{}
			cfg := Config{
				LocalTmuxOut: func(argv ...string) (string, error) {
					log.add(argv)
					return holdReply("%3", L.W, L.H), nil
				},
			}
			if held := holdReshaped(cfg, w, L, tc.remote, order, tc.ops, tc.zoomed, router); held != nil {
				t.Fatalf("held = %v, want nothing", held)
			}
			if got := log.snapshot(); len(got) != 0 {
				t.Errorf("local calls = %v, want none", got)
			}
			requireSilence(t, peer)
		})
	}
}

// TestReconcileHoldSkipsWhenTheWindowShrinks: a local window taller than the
// target is shrunk by resize-window before select-layout, so the read happens
// but nothing is held and the barrier never polls.
func TestReconcileHoldSkipsWhenTheWindowShrinks(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()
	L, err := controlmode.ParseLayout(holdOnePane)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}

	log := &holdLocalLog{}
	cfg := Config{
		LocalTmuxOut: func(argv ...string) (string, error) {
			log.add(argv)
			return holdReplyWin("%3", 95, 45, L.H+1), nil
		},
	}
	if held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), paneOps{Remove: []int{1}}, false, router); held != nil {
		t.Fatalf("held = %v, want nothing", held)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"read"}) {
		t.Errorf("local calls = %v, want the read and no poll", got)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldDeadPaneIsNotHeld: a corpse's renderer never writes the
// hold, so holding it would only burn the barrier's full wait.
func TestReconcileHoldDeadPaneIsNotHeld(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()
	L, err := controlmode.ParseLayout(holdOnePane)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}

	log := &holdLocalLog{}
	cfg := Config{
		LocalTmuxOut: func(argv ...string) (string, error) {
			log.add(argv)
			return holdReplyDead("%3", 95, 45, true), nil
		},
	}
	if held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), paneOps{Remove: []int{1}}, false, router); held != nil {
		t.Fatalf("held = %v, want nothing", held)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"read"}) {
		t.Errorf("local calls = %v, want the read and no poll", got)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldGeometryOnlySkipsAnUnchangedPane: a geometry-only pass that
// leaves a pane's dims alone does not reflow it, so it is not held.
func TestReconcileHoldGeometryOnlySkipsAnUnchangedPane(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	L := holdTwoPane(t)
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%3", "%4"}, localPanes: []string{"%l3", "%l4"},
	}

	log := &holdLocalLog{}
	cfg := Config{
		LocalTmuxOut: func(argv ...string) (string, error) {
			log.add(argv)
			return holdReply("%3", 95, 45), nil
		},
	}
	order := RemotePaneOrder(L)
	if held := holdReshaped(cfg, w, L, order, order, paneOps{}, false, router); held != nil {
		t.Fatalf("held = %v, want nothing", held)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"read"}) {
		t.Errorf("local calls = %v, want the read and no poll", got)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldNilLocalTmuxOutHoldsNothing: many fakes set only LocalTmux,
// and the structural path must not reach for the missing reader.
func TestReconcileHoldNilLocalTmuxOutHoldsNothing(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	L, err := controlmode.ParseLayout(holdOnePane)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	w := holdCloseWindow()

	var calls []string
	cfg := Config{
		LocalTmux: func(argv ...string) error {
			calls = append(calls, argv[0])
			return nil
		},
	}
	if held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), paneOps{Remove: []int{1}}, false, router); held != nil {
		t.Fatalf("held = %v, want nothing", held)
	}
	if len(calls) != 0 {
		t.Errorf("local calls = %v, want none", calls)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldAwaitHeld pins the barrier's three outcomes.
func TestReconcileHoldAwaitHeld(t *testing.T) {
	saved := holdWait
	holdWait = 20 * time.Millisecond
	t.Cleanup(func() { holdWait = saved })
	locals := []string{"%l3", "%l4"}

	counting := func(out string, err error) (Config, *int) {
		n := 0
		return Config{LocalTmuxOut: func(...string) (string, error) {
			n++
			return out, err
		}}, &n
	}

	t.Run("short reply times out", func(t *testing.T) {
		cfg, n := counting("1\n", nil)
		if awaitHeld(cfg, locals) {
			t.Fatal("awaitHeld = true on one line for two panes")
		}
		if *n < 2 {
			t.Errorf("polled %d times, want retries until holdWait", *n)
		}
	})
	t.Run("exec error stops at once", func(t *testing.T) {
		cfg, n := counting("", errors.New("exec failed"))
		if awaitHeld(cfg, locals) {
			t.Fatal("awaitHeld = true on an exec error")
		}
		if *n != 1 {
			t.Errorf("polled %d times, want 1", *n)
		}
	})
	t.Run("all on passes first try", func(t *testing.T) {
		cfg, n := counting("1\n1\n", nil)
		if !awaitHeld(cfg, locals) {
			t.Fatal("awaitHeld = false with both panes on the alternate screen")
		}
		if *n != 1 {
			t.Errorf("polled %d times, want 1", *n)
		}
	})
}
