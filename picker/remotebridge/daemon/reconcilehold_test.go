package daemon

import (
	"errors"
	"fmt"
	"io"
	"net"
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
	case argv[0] == "display-message" && slices.Contains(argv, holdPollFormat):
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
	d := 0
	if dead {
		d = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|1|%d|%d|%d|1\n", remoteID, w, h, d)
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
// barrier poll with a first hold's park, and list-panes with the window before or after the kill.
func holdCloseOut(log *holdLocalLog, reply func() (string, error), killed func() bool) func(...string) (string, error) {
	return func(argv ...string) (string, error) {
		switch log.add(argv) {
		case "read":
			return reply()
		case "poll":
			return "1|0|0\n", nil
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

// TestReconcileHoldRefusedSelectLayoutStaysHeld covers the pass that held a
// pane but never reseeded it: the deferred release leaves it held, since
// ?1049l would expose the reflowed main screen, and marks it for a reseed whose
// seed releases it.
func TestReconcileHoldRefusedSelectLayoutStaysHeld(t *testing.T) {
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
	if n, ok := settleDirty(t, sink, 5*time.Second); !ok || n != 1 {
		t.Errorf("takeDirty = %d, %v; want the release's one drop", n, ok)
	}
	if !sink.isHeld() {
		t.Error("isHeld = false; the release must leave the pane for the reseed")
	}
	requireSilence(t, peer)
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

// holdReadLocals is the local panes a hold read's argv names, in order.
func holdReadLocals(argv []string) []string {
	var locals []string
	for i := 0; i+4 < len(argv); i++ {
		if argv[i] == "display-message" && argv[i+4] == holdHeaderFormat {
			locals = append(locals, argv[i+3])
		}
	}
	return locals
}

// holdReadFor answers a hold read for every local pane it names, each at the
// dims of the remote pane remoteOf maps it to.
func holdReadFor(argv []string, remoteOf map[string]string, dims map[string][2]int) string {
	var b strings.Builder
	for _, l := range holdReadLocals(argv) {
		id := remoteOf[l]
		b.WriteString(holdReply(id, dims[id][0], dims[id][1]))
	}
	return b.String()
}

// TestReconcileHoldHoldsEveryPassKind: the parked cursor keeps a held pane's
// rows in place through any shrink and regrow, so passes the earlier design
// skipped now hold too; in a zoomed window only the zoomed pane is read.
func TestReconcileHoldHoldsEveryPassKind(t *testing.T) {
	L := holdTwoPane(t)
	order := RemotePaneOrder(L)
	cell := map[string][2]int{"%3": {95, 45}, "%4": {94, 45}}
	grown := map[string][2]int{"%3": {190, 45}, "%4": {190, 45}}
	tests := []struct {
		name         string
		remote       []string
		localPanes   []string
		structural   bool
		zoomed       bool
		appliedZoom  bool
		remoteActive string
		dims         map[string][2]int
		wantLocals   []string
		wantHeld     []string
	}{
		{"append splits the last pane", []string{"%3"}, []string{"%l3"}, true, false, false, "%3", grown, []string{"%l3"}, []string{"%3"}},
		{"swap moves a pane through another's cell", []string{"%4", "%3"}, []string{"%l4", "%l3"}, true, false, false, "%3", cell, []string{"%l3", "%l4"}, []string{"%3", "%4"}},
		{"zoom-on transition", order, []string{"%l3", "%l4"}, false, true, false, "%3", cell, []string{"%l3"}, []string{"%3"}},
		{"locally zoomed structural kill", []string{"%3", "%4", "%6"}, []string{"%l3", "%l4", "%l6"}, true, true, true, "%4", grown, []string{"%l4"}, []string{"%4"}},
		{"locally zoomed geometry-only", order, []string{"%l3", "%l4"}, false, true, true, "%4", cell, []string{"%l4"}, []string{"%4"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := NewRouter()
			sinks := map[string]*outputSink{}
			peers := map[string]net.Conn{}
			for _, id := range order {
				sinks[id], peers[id] = holdPipe(t)
				router.Register(id, sinks[id])
			}
			w := &mirrorWindow{
				remoteID: "@1", localWin: "@101",
				remotePanes: tc.remote, localPanes: tc.localPanes,
				layout: "stale", appliedZoom: tc.appliedZoom,
			}
			remoteOf := map[string]string{}
			for i, l := range tc.localPanes {
				remoteOf[l] = tc.remote[i]
			}
			log := &holdLocalLog{}
			cfg := Config{
				LocalTmuxOut: func(argv ...string) (string, error) {
					if log.add(argv) == "poll" {
						return strings.Repeat("1|0|0\n", len(tc.wantHeld)), nil
					}
					return holdReadFor(argv, remoteOf, tc.dims), nil
				},
			}
			held := holdReshaped(cfg, w, L, tc.remote, order, tc.structural, tc.zoomed, tc.remoteActive, router)

			var want []*outputSink
			for _, id := range tc.wantHeld {
				want = append(want, sinks[id])
			}
			if !slices.Equal(held, want) {
				t.Fatalf("held %d sinks, want %v", len(held), tc.wantHeld)
			}
			if got := log.snapshot(); !slices.Equal(got, []string{"read", "poll"}) {
				t.Errorf("local calls = %v, want the read and the poll", got)
			}
			if got := holdReadLocals(log.argv[0]); !slices.Equal(got, tc.wantLocals) {
				t.Errorf("read panes %v, want %v", got, tc.wantLocals)
			}
			for _, id := range order {
				if !slices.Contains(tc.wantHeld, id) {
					requireSilence(t, peers[id])
					continue
				}
				f := readHoldFrame(t, peers[id])
				if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
					t.Fatalf("%s frame = %v %q, want the hold output", id, f.Type, f.Payload)
				}
			}
		})
	}
}

// TestReconcileHoldHoldsAnAlternateScreenPane: an app on its own alternate
// screen is held like any other pane. ?1049h is a no-op there, but the rows
// and the parked cursor are still written, and the read never asks.
func TestReconcileHoldHoldsAnAlternateScreenPane(t *testing.T) {
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
			if log.add(argv) == "poll" {
				return "1|0|0\n", nil
			}
			if slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, "alternate_on") }) {
				t.Errorf("read argv %v asks alternate_on", argv)
			}
			return holdReply("%3", 120, 45), nil
		},
	}
	order := RemotePaneOrder(L)
	if held := holdReshaped(cfg, w, L, order, order, false, false, "%3", router); !slices.Equal(held, []*outputSink{sink}) {
		t.Fatalf("held = %v, want the pane", held)
	}
	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("frame = %v %q, want the hold output", f.Type, f.Payload)
	}
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
	if held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), true, false, "%3", router); held != nil {
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
	if held := holdReshaped(cfg, w, L, order, order, false, false, "%3", router); held != nil {
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
	if held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), true, false, "%3", router); held != nil {
		t.Fatalf("held = %v, want nothing", held)
	}
	if len(calls) != 0 {
		t.Errorf("local calls = %v, want none", calls)
	}
	requireSilence(t, peer)
}

// TestReconcileHoldForceHoldsAnUnchangedSurvivor: a structural pass holds a
// survivor already at its target dims, since the local kill can still grow it
// and shrink it back.
func TestReconcileHoldForceHoldsAnUnchangedSurvivor(t *testing.T) {
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
			if log.add(argv) == "poll" {
				return "1|0|0\n", nil
			}
			return holdReply("%3", L.W, L.H), nil
		},
	}
	held := holdReshaped(cfg, w, L, w.remotePanes, RemotePaneOrder(L), true, false, "%3", router)
	if want := []*outputSink{sink}; !slices.Equal(held, want) {
		t.Fatalf("held = %v, want %v", held, want)
	}
	if got := log.snapshot(); !slices.Equal(got, []string{"read", "poll"}) {
		t.Errorf("local calls = %v, want the read and the poll", got)
	}
	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("frame = %v %q, want the hold output", f.Type, f.Payload)
	}
}

// holdPassFake is a local tmux whose tiled panes follow kill-pane and
// split-window, and list-panes adds extra. The n'th poll answers polls[n-1],
// the last entry repeating, or a first hold's park when polls is empty; the
// first select-layout is refused.
type holdPassFake struct {
	log   *holdLocalLog
	read  string
	extra string
	polls []string

	mu       sync.Mutex
	panes    []string
	selects  int
	splitIDs int
	polled   int
}

func (f *holdPassFake) config() Config {
	return Config{
		LocalTmux: func(argv ...string) error {
			name := f.log.add(argv)
			f.mu.Lock()
			defer f.mu.Unlock()
			switch name {
			case "kill-pane":
				f.panes = slices.DeleteFunc(f.panes, func(p string) bool { return p == argv[2] })
			case "split-window":
				f.splitIDs++
				f.panes = append(f.panes, fmt.Sprintf("%%n%d", f.splitIDs))
			case "select-layout":
				f.selects++
				if f.selects == 1 {
					return errors.New("select-layout refused")
				}
			}
			return nil
		},
		LocalTmuxOut: func(argv ...string) (string, error) {
			name := f.log.add(argv)
			f.mu.Lock()
			defer f.mu.Unlock()
			switch name {
			case "read":
				return f.read, nil
			case "poll":
				f.polled++
				if len(f.polls) == 0 {
					return "1|0|0\n", nil
				}
				return f.polls[min(f.polled, len(f.polls))-1], nil
			case "list-panes":
				var b strings.Builder
				for _, p := range f.panes {
					b.WriteString(p + " 0\n")
				}
				b.WriteString(f.extra)
				return b.String(), nil
			}
			return "", nil
		},
	}
}

// nthIndex is the position of the n'th (from 1) occurrence of name in entries.
func nthIndex(entries []string, name string, n int) int {
	for i, e := range entries {
		if e == name {
			n--
			if n == 0 {
				return i
			}
		}
	}
	return -1
}

// TestReconcileHoldCarriesAcrossPasses: pass 1 holds the survivor of a close
// but its select-layout is refused, so no seed releases it. Pass 2 (a split)
// reads and enqueues it again; its pump, still holding, writes only that
// hold's park, which pass 2's barrier waits for, and nothing leaves the
// alternate screen between the passes, which would expose the reflowed main
// screen. Pass 2's seed releases it.
func TestReconcileHoldCarriesAcrossPasses(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()

	const split = "0000,190x45,0,0{95x45,0,0,3,94x45,96,0,5}"
	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + holdOnePane + " %3 0", "%end 1 1 1",
		"%begin 1 2 1", "@1 " + split + " %3 0", "%end 1 2 1",
		"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1",
		"%begin 1 4 1", "SURVIVOR", "%end 1 4 1",
		"%begin 1 5 1", "@1 " + split + " %3 0", "%end 1 5 1",
	}, "\n") + "\n")

	log := &holdLocalLog{}
	fake := &holdPassFake{log: log, read: holdReply("%3", 95, 45), panes: []string{"%l3", "%l4"}, polls: []string{"1|0|0\n", "1|0|1\n"}}
	reconcileLayout(fake.config(), w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	got := log.snapshot()
	read, poll, kill := nthIndex(got, "read", 1), nthIndex(got, "poll", 1), nthIndex(got, "kill-pane", 1)
	refused, read2, poll2 := nthIndex(got, "select-layout", 1), nthIndex(got, "read", 2), nthIndex(got, "poll", 2)
	splitAt := nthIndex(got, "split-window", 1)
	if read < 0 || read >= poll || poll >= kill || kill >= refused || refused >= read2 || read2 >= poll2 || poll2 >= splitAt {
		t.Fatalf("local calls = %v, want read, poll, kill-pane, select-layout, then pass 2's read and poll before split-window", got)
	}

	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("first frame = %v %q, want pass 1's hold output", f.Type, f.Payload)
	}
	requireFrame(t, peer, wire.FrameOutput, "\x1b[1;2H")
	if f := readHoldFrame(t, peer); f.Type != wire.FrameResize {
		t.Fatalf("third frame = %v %q, want pass 2's resize with no release or second hold ahead of it", f.Type, f.Payload)
	}
	if f := readHoldFrame(t, peer); f.Type != wire.FrameSeed {
		t.Fatalf("fourth frame = %v %q, want pass 2's seed", f.Type, f.Payload)
	}
	requireSilence(t, peer)
	if sink.isHeld() {
		t.Error("isHeld = true after pass 2's seed")
	}
	if n, ok := settleDirty(t, sink, 200*time.Millisecond); ok {
		t.Errorf("takeDirty = %d, true; the seed released both holds", n)
	}
}

// TestReconcileHoldDesyncRebuildReleasesNothingFirst: a pass that held a pane
// and then finds the window desynced rebuilds with the pane still held; the
// release waits for the pass's exit, so nothing exposes the reflowed main
// screen ahead of the rebuild.
func TestReconcileHoldDesyncRebuildReleasesNothingFirst(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + holdOnePane + " %3 0", "%end 1 1 1",
	}, "\n") + "\n")

	log := &holdLocalLog{}
	fake := &holdPassFake{log: log, read: holdReply("%3", 95, 45), panes: []string{"%l3", "%l4"}, extra: "%lx 0\n"}
	cfg := fake.config()
	cfg.LocalArea = func() (int, int) { return 0, 0 }
	reconcileLayout(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	got := log.snapshot()
	read, poll, kill := nthIndex(got, "read", 1), nthIndex(got, "poll", 1), nthIndex(got, "kill-pane", 1)
	if read < 0 || read >= poll || poll >= kill {
		t.Fatalf("local calls = %v, want the hold read and poll ahead of the rebuild's kill-pane", got)
	}
	if nthIndex(got, "poll", 2) >= 0 {
		t.Errorf("local calls = %v, want no poll after the hold's", got)
	}

	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) {
		t.Fatalf("first frame = %v %q, want the hold output", f.Type, f.Payload)
	}
	for {
		_ = peer.SetDeadline(time.Now().Add(200 * time.Millisecond))
		f, err := wire.ReadFrame(peer)
		if err != nil {
			break
		}
		if f.Type == wire.FrameOutput && string(f.Payload) == holdLeaveAlt {
			t.Fatal("a ?1049l reached the held pane")
		}
	}
}

// TestReconcileHoldAwaitAlt pins the barrier's outcomes for a hold.
func TestReconcileHoldAwaitAlt(t *testing.T) {
	saved := holdWait
	holdWait = 20 * time.Millisecond
	t.Cleanup(func() { holdWait = saved })
	locals := []string{"%l3", "%l4"}
	parked := []string{"1|0|0", "1|0|2"}

	counting := func(out string, err error) (Config, *int) {
		n := 0
		return Config{LocalTmuxOut: func(...string) (string, error) {
			n++
			return out, err
		}}, &n
	}

	tests := []struct {
		name  string
		want  []string
		out   string
		err   error
		ok    bool
		polls func(int) bool
	}{
		{"hold confirmed first try", parked, "1|0|0\n1|0|2\n", nil, true, func(n int) bool { return n == 1 }},
		{"hold short reply times out", parked, "1|0|0\n", nil, false, func(n int) bool { return n >= 2 }},
		{"hold not yet parked times out", parked, "1|0|0\n1|5|2\n", nil, false, func(n int) bool { return n >= 2 }},
		{"earlier hold's column times out", parked, "1|0|0\n1|0|1\n", nil, false, func(n int) bool { return n >= 2 }},
		{"hold exec error stops at once", parked, "", errors.New("exec failed"), false, func(n int) bool { return n == 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, n := counting(tc.out, tc.err)
			if got := awaitAlt(cfg, locals, tc.want); got != tc.ok {
				t.Fatalf("awaitAlt(want %q) on %q = %v, want %v", tc.want, tc.out, got, tc.ok)
			}
			if !tc.polls(*n) {
				t.Errorf("polled %d times", *n)
			}
		})
	}
}

// TestReconcileHoldBarrierWaitsForItsOwnHold: pass 1's hold is still on screen,
// parked at its own column, when pass 2 holds the pane again behind pass 1's
// seed. The barrier must not take hold 1's park for hold 2's: until the seed
// and hold 2 land, the pane still reports hold 1.
func TestReconcileHoldBarrierWaitsForItsOwnHold(t *testing.T) {
	sink, peer := holdPipe(t)
	router := NewRouter()
	router.Register("%3", sink)
	w := holdCloseWindow()
	L, err := controlmode.ParseLayout(holdOnePane)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	order := RemotePaneOrder(L)

	log := &holdLocalLog{}
	fake := &holdPassFake{log: log, read: holdReply("%3", 95, 45), polls: []string{"1|0|0\n"}}
	if held := holdReshaped(fake.config(), w, L, w.remotePanes, order, true, false, "%3", router); !slices.Equal(held, []*outputSink{sink}) {
		t.Fatalf("pass 1 held = %v, want the pane", held)
	}
	if fake.polled != 1 {
		t.Fatalf("pass 1 polled %d times, want 1", fake.polled)
	}
	sink.enqueue(wire.FrameSeed, []byte(holdLeaveAlt+"SEED1"))

	fake.polled = 0
	fake.polls = []string{"1|0|0\n", "1|0|0\n", "1|0|1\n"}
	if held := holdReshaped(fake.config(), w, L, w.remotePanes, order, true, false, "%3", router); !slices.Equal(held, []*outputSink{sink}) {
		t.Fatalf("pass 2 held = %v, want the pane", held)
	}
	if fake.polled != 3 {
		t.Errorf("pass 2 polled %d times, want 3: only hold 2's own park confirms it", fake.polled)
	}

	f := readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasSuffix(string(f.Payload), "\x1b[1;1H") {
		t.Fatalf("first frame = %v %q, want hold 1 parked at column 1", f.Type, f.Payload)
	}
	requireFrame(t, peer, wire.FrameSeed, holdLeaveAlt+"SEED1")
	f = readHoldFrame(t, peer)
	if f.Type != wire.FrameOutput || !strings.HasPrefix(string(f.Payload), holdEnterAlt) || !strings.HasSuffix(string(f.Payload), "\x1b[1;2H") {
		t.Fatalf("third frame = %v %q, want hold 2 parked at column 2", f.Type, f.Payload)
	}
}

// TestReconcileHoldMergeBackOwesAReseed: a rebuild that fails before the
// respawn re-registers the kept pane's conn behind a fresh sink, which knows
// nothing of a hold the pane may still be parked in; it is owed a reseed, whose
// seed releases it.
func TestReconcileHoldMergeBackOwesAReseed(t *testing.T) {
	kept, peer := net.Pipe()
	t.Cleanup(func() {
		_ = kept.Close()
		_ = peer.Close()
	})
	go func() { _, _ = io.Copy(io.Discard, peer) }()

	cfg := Config{
		LocalArea:    func() (int, int) { return 80, 24 },
		LocalTmux:    func(...string) error { return nil },
		LocalTmuxOut: func(...string) (string, error) { return "%l1 0\n", nil },
	}
	w := &mirrorWindow{
		remoteID: "@1", localWin: "@101",
		remotePanes: []string{"%1"}, localPanes: []string{"%l1"},
		conns: map[string]net.Conn{"%1": kept},
	}
	router := NewRouter()
	err := resetWindow(cfg, w, func(string) {}, router, noHellos, newCtlState(), newConverger(), setupWindowRT(strings.Join([]string{
		"%begin 1 1 1", "%end 1 1 1",
		"%begin 1 2 1", "%error 1 2 1",
	}, "\n")+"\n"))
	if err == nil {
		t.Fatal("resetWindow err = nil, want setupWindow failure")
	}
	s := router.sink("%1")
	if s == nil {
		t.Fatal("router.sink(%1) = nil, want the merged-back sink")
	}
	t.Cleanup(func() {
		s.Close()
		s.Wait()
	})
	if n, ok := settleDirty(t, s, 5*time.Second); !ok || n < 1 {
		t.Errorf("takeDirty = %d, %v; want a reseed owed", n, ok)
	}
}
