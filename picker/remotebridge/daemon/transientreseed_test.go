package daemon

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// A remote reshape that goes out and back before the mirror reads it leaves the
// read equal to the mirror's applied state, yet the panes were resized and
// repainted by the app in between, so the mirror's screens are garbled. These
// tests pin that a notice sent down the read-first path reseeds the panes the
// read says are visible, and that nothing cheaper-looking does.

const (
	transientThreePane = "abc1,190x45,0,0{60x45,0,0,0,60x45,61,0,1,68x45,122,0,2}"
	// The same window one split further along: the pane set a transient notice
	// carries when the remote opened a fourth pane and closed it again.
	transientFourPane = "abc2,190x45,0,0{40x45,0,0,0,40x45,41,0,1,40x45,82,0,2,68x45,123,0,3}"
)

// frameLog records every frame a sink's pump writes to its renderer socket.
type frameLog struct {
	mu     sync.Mutex
	frames []wire.Frame
}

func (f *frameLog) seeds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, fr := range f.frames {
		if fr.Type == wire.FrameSeed {
			out = append(out, string(fr.Payload))
		}
	}
	return out
}

// transientSink registers a real outputSink for id on router and records the
// frames it writes.
func transientSink(t *testing.T, router *Router, id string) *frameLog {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
	router.Register(id, newOutputSink(conn, nil))

	log := &frameLog{}
	go func() {
		for {
			fr, err := wire.ReadFrame(peer)
			if err != nil {
				return
			}
			log.mu.Lock()
			log.frames = append(log.frames, fr)
			log.mu.Unlock()
		}
	}()
	return log
}

// readReply is the reply readLayout consumes, numbered from seq.
func readReply(seq int, layout, active, zoom string) string {
	return fmt.Sprintf("%%begin 1 %d 1\n@1 %s %s %s\n%%end 1 %d 1\n", seq, layout, active, zoom, seq)
}

// transientScript is readLayout's reply, one seed reply pair per screen, then a
// trailing re-read for a pass that closes with one. Extra replies are unread
// when the code under test does not need them.
func transientScript(layout, active, zoom string, screens ...string) string {
	var sb strings.Builder
	sb.WriteString(readReply(1, layout, active, zoom))
	seq := 2
	for _, s := range screens {
		sb.WriteString(floatSeedScript(seq, s))
		seq += 2
	}
	sb.WriteString(readReply(seq, layout, active, zoom))
	return sb.String()
}

// pollUntil polls cond for up to half a second.
func pollUntil(cond func() bool) bool {
	deadline := time.Now().Add(500 * time.Millisecond)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// dueReshaped returns the panes that came due a confirmation capture, polled
// until it holds want or time runs out. reshapedPanes needs two calls to hand a
// marked pane over, and only once its queue has drained.
func dueReshaped(router *Router, want []string) []string {
	var got []string
	pollUntil(func() bool {
		for _, id := range router.reshapedPanes() {
			if !slices.Contains(got, id) {
				got = append(got, id)
			}
		}
		slices.Sort(got)
		return slices.Equal(got, want)
	})
	return got
}

// streamText is everything the code under test wrote on the control stream.
func streamText(l *orderedLog) string { return strings.Join(l.entries, "") }

func TestTransientReseedZoomFlagMismatchSeedsEveryTiledPane(t *testing.T) {
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1", "%2"}
	w.localPanes = []string{"%l0", "%l1", "%l2"}
	w.layout = transientThreePane

	router := NewRouter()
	logs := map[string]*frameLog{}
	for _, id := range w.remotePanes {
		logs[id] = transientSink(t, router, id)
	}
	sent := &orderedLog{}
	// The notice caught the zoom half of a push/pop-zoom bracket; by the read the
	// remote is unzoomed again, equal to what the mirror applied.
	rt := scriptedRTRouterW(transientScript(transientThreePane, "%0", "0", "SEED0", "SEED1", "SEED2"), router, sent)
	f := &layoutTmux{windowID: "@101\n"}
	l := noticeLine("@1", transientThreePane, transientThreePane, "*Z")

	if retire := reconcileLayoutFrom(f.config(), w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	assertTransientSeeds(t, router, logs, sent, w.remotePanes, w.remotePanes)
}

func TestTransientReseedPaneSetMismatchSeedsEveryTiledPane(t *testing.T) {
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1", "%2"}
	w.localPanes = []string{"%l0", "%l1", "%l2"}
	w.layout = transientThreePane

	router := NewRouter()
	logs := map[string]*frameLog{}
	for _, id := range w.remotePanes {
		logs[id] = transientSink(t, router, id)
	}
	sent := &orderedLog{}
	// The notice names a fourth pane the remote opened and closed again; the read
	// finds the applied three.
	rt := scriptedRTRouterW(transientScript(transientThreePane, "%0", "0", "SEED0", "SEED1", "SEED2"), router, sent)
	f := &layoutTmux{windowID: "@101\n"}
	l := noticeLine("@1", transientFourPane, transientFourPane, "*")

	if retire := reconcileLayoutFrom(f.config(), w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	assertTransientSeeds(t, router, logs, sent, w.remotePanes, w.remotePanes)
	if strings.Contains(streamText(sent), captureCmd("%3")) {
		t.Errorf("captured %%3, which the read never reported")
	}
}

func TestTransientReseedZoomedMirrorSeedsOnlyTheVisiblePane(t *testing.T) {
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1", "%2"}
	w.localPanes = []string{"%l0", "%l1", "%l2"}
	w.layout = transientThreePane
	w.appliedZoom = true

	router := NewRouter()
	logs := map[string]*frameLog{}
	for _, id := range w.remotePanes {
		logs[id] = transientSink(t, router, id)
	}
	sent := &orderedLog{}
	// The notice is the unzoomed transient that opens a bracket; the read finds
	// the remote zoomed on %1, the state the mirror already shows.
	rt := scriptedRTRouterW(transientScript(transientThreePane, "%1", "1", "SEED1"), router, sent)
	f := &layoutTmux{windowID: "@101\n"}
	l := noticeLine("@1", transientThreePane, transientThreePane, "*")

	if retire := reconcileLayoutFrom(f.config(), w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	assertTransientSeeds(t, router, logs, sent, w.remotePanes, []string{"%1"})
}

// A notice equal to the applied state, with the zoom flag agreeing, is the
// duplicate the no-op gate exists for: it must stay silent.
func TestTransientReseedDuplicateNoticeStaysSilent(t *testing.T) {
	w := newRegistry().add("@1", "@101")
	w.remotePanes = []string{"%0", "%1", "%2"}
	w.localPanes = []string{"%l0", "%l1", "%l2"}
	w.layout = transientThreePane

	router := NewRouter()
	logs := map[string]*frameLog{}
	for _, id := range w.remotePanes {
		logs[id] = transientSink(t, router, id)
	}
	sent := &orderedLog{}
	rt := scriptedRTRouterW("", router, sent)
	fake := &noticeFake{local: "0\n"}
	l := noticeLine("@1", transientThreePane, transientThreePane, "*")

	if retire := reconcileLayoutFrom(fake.config(), w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	if len(sent.entries) != 0 {
		t.Errorf("wrote %q, want nothing: a duplicate notice costs zero round-trips", streamText(sent))
	}
	if len(fake.localTmux) != 0 {
		t.Errorf("LocalTmux calls = %v, want none", fake.localTmux)
	}
	if got := dueReshaped(router, nil); len(got) != 0 {
		t.Errorf("reshaped panes = %v, want none", got)
	}
	for id, log := range logs {
		if got := log.seeds(); len(got) != 0 {
			t.Errorf("%s got seeds %q, want none", id, got)
		}
	}
}

// A float appearing moves no tiled pane, so the read that finds it must settle
// the float and leave the tiled panes' screens alone.
func TestTransientReseedFloatOnlyChangeLeavesTiledPanesAlone(t *testing.T) {
	f := &layoutTmux{windowID: "@101\n", newPaneIDs: []string{"%lf\n"}}
	w := shapedMirror(t)

	router := NewRouter()
	logs := map[string]*frameLog{
		"%0": transientSink(t, router, "%0"),
		"%1": transientSink(t, router, "%1"),
	}
	sent := &orderedLog{}
	rt := scriptedRTRouterW(readReply(1, tiledFloatLayout, "%9", "0")+floatSeedScript(2, "FLOAT-SEED"), router, sent)
	l := noticeLine("@1", tiledFloatLayout, tiledFloatLayout, "*")

	reconcileLayoutFrom(f.config(), w, l, func(string) {}, router,
		hellos(map[string]net.Conn{"%9": drainedPipe(t)}), newCtlState(), newConverger(), rt)

	if got := f.verbs("new-pane"); len(got) != 1 {
		t.Fatalf("new-pane calls = %v, want the float mirrored", got)
	}
	trace := streamText(sent)
	for _, id := range []string{"%0", "%1"} {
		if strings.Contains(trace, captureCmd(id)) {
			t.Errorf("captured tiled pane %s on a float-only change", id)
		}
	}
	if got := dueReshaped(router, nil); len(got) != 0 {
		t.Errorf("reshaped panes = %v, want none", got)
	}
	for id, log := range logs {
		if got := log.seeds(); len(got) != 0 {
			t.Errorf("%s got seeds %q, want none", id, got)
		}
	}
}

func logFor(t *testing.T, logs map[string]*frameLog, id string) *frameLog {
	t.Helper()
	log := logs[id]
	if log == nil {
		t.Fatalf("no frame log registered for %s", id)
	}
	return log
}

// assertTransientSeeds checks, over every tiled pane, that exactly wantSeeded
// were captured on the control stream, delivered a FrameSeed and marked
// reshaped.
func assertTransientSeeds(t *testing.T, router *Router, logs map[string]*frameLog, sent *orderedLog, panes, wantSeeded []string) {
	t.Helper()
	trace := streamText(sent)
	for _, id := range panes {
		captured := strings.Contains(trace, captureCmd(id))
		if want := slices.Contains(wantSeeded, id); captured != want {
			t.Errorf("capture-pane for %s sent = %v, want %v\nstream: %q", id, captured, want, trace)
		}
	}
	for _, id := range wantSeeded {
		log := logFor(t, logs, id)
		if !pollUntil(func() bool { return len(log.seeds()) > 0 }) {
			t.Errorf("%s never received a FrameSeed", id)
		}
	}
	// Seeds are enqueued before the reconcile returns, so a hidden pane's would
	// already be in flight; the pause only covers the pump goroutine.
	time.Sleep(100 * time.Millisecond)
	for _, id := range panes {
		if slices.Contains(wantSeeded, id) {
			continue
		}
		if got := logFor(t, logs, id).seeds(); len(got) > 0 {
			t.Errorf("%s received seeds %q, want none: the pane is hidden", id, got)
		}
	}
	if got := dueReshaped(router, wantSeeded); !slices.Equal(got, wantSeeded) {
		t.Errorf("reshaped panes = %v, want %v: each reseeded pane is owed a confirmation capture", got, wantSeeded)
	}
}
