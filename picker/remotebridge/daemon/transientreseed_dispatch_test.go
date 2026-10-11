package daemon

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// The float layouts below differ from tiledFloatLayout in the float's x alone
// (floatMovedLayout) or in the tiled split alone (tiledMovedLayout).
const (
	floatMovedLayout = "9999,190x45,0,0{95x45,0,0,0,94x45,96,0,1,18x6,40,6,9}<18x6,40,6,9>"
	tiledMovedLayout = "9999,190x45,0,0{90x45,0,0,0,99x45,91,0,1,18x6,11,6,9}<18x6,11,6,9>"
)

// burstKeys returns the out indices coalesceLayoutBursts flagged, sorted.
func burstKeys(burst map[int]bool) []int {
	var keys []int
	for i, b := range burst {
		if b {
			keys = append(keys, i)
		}
	}
	slices.Sort(keys)
	return keys
}

func TestCoalesceLayoutBurstsFlagsADifferingDroppedNotice(t *testing.T) {
	const (
		a = transientThreePane
		b = transientFourPane
		c = noticeReshapedLayout
	)
	tests := []struct {
		name  string
		lines []controlmode.Line
		want  []int
	}{
		{"out and back", []controlmode.Line{noticeLine("@1", b, b, "*"), noticeLine("@1", a, a, "*")}, []int{0}},
		{"identical duplicates", []controlmode.Line{noticeLine("@1", a, a, "*"), noticeLine("@1", a, a, "*")}, nil},
		{"two shapes", []controlmode.Line{noticeLine("@1", b, b, "*"), noticeLine("@1", c, c, "*")}, []int{0}},
		{"zoom flag only", []controlmode.Line{noticeLine("@1", a, a, "*Z"), noticeLine("@1", a, a, "*")}, []int{0}},
		{"unparsable, identical", []controlmode.Line{controlmode.ParseLine("%layout-change @1 junk junk"), controlmode.ParseLine("%layout-change @1 junk junk")}, nil},
		{"unparsable, differing", []controlmode.Line{controlmode.ParseLine("%layout-change @1 junk junk"), controlmode.ParseLine("%layout-change @1 junk other")}, []int{0}},
		{"single line", []controlmode.Line{noticeLine("@1", a, a, "*")}, nil},
		{
			// A float moves no tiled pane, so the mirror owes nothing for it.
			"float geometry only",
			[]controlmode.Line{noticeLine("@1", tiledFloatLayout, tiledFloatLayout, "*"), noticeLine("@1", floatMovedLayout, floatMovedLayout, "*")},
			nil,
		},
		{
			"tiled shape with floats present",
			[]controlmode.Line{noticeLine("@1", tiledMovedLayout, tiledMovedLayout, "*"), noticeLine("@1", tiledFloatLayout, tiledFloatLayout, "*")},
			[]int{0},
		},
		{
			// @1's dropped notice differs; @2's is an identical duplicate.
			"windows do not interfere",
			[]controlmode.Line{noticeLine("@1", b, b, "*"), noticeLine("@2", a, a, "*"), noticeLine("@1", a, a, "*"), noticeLine("@2", a, a, "*")},
			[]int{0},
		},
		{
			// The differing window's survivor sits at out index 1, behind a rename.
			"flag follows the survivor's index",
			[]controlmode.Line{
				noticeLine("@2", a, a, "*"), noticeLine("@1", b, b, "*"),
				{Kind: controlmode.WindowRenamed, Args: []string{"@1"}, Data: []byte("n")},
				noticeLine("@1", a, a, "*"), noticeLine("@2", a, a, "*"),
			},
			[]int{1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, burst := coalesceLayoutBursts(tc.lines)
			if got := burstKeys(burst); !slices.Equal(got, tc.want) {
				t.Errorf("flagged out indices = %v, want %v (out = %+v)", got, tc.want, out)
			}
			for i := range burst {
				if out[i].Kind != controlmode.LayoutChange {
					t.Errorf("burst[%d] flags a %v line, want only %%layout-change survivors", i, out[i].Kind)
				}
			}
		})
	}
}

// drain hands each dispatch the flag of the line it is dispatching, even when
// an earlier dispatch has already popped lines off the front.
func TestDrainHandsTheBurstFlagToTheSurvivorOnly(t *testing.T) {
	const (
		a = transientThreePane
		b = transientFourPane
	)
	// The flagged survivor leads three, so an index counted from the wrong end
	// misses it.
	async := &asyncQueue{}
	async.push(noticeLine("@1", b, b, "*"))
	async.push(noticeLine("@1", a, a, "*"))
	async.push(controlmode.Line{Kind: controlmode.WindowRenamed, Args: []string{"@1"}, Data: []byte("n")})
	async.push(noticeLine("@2", a, a, "*"))

	type seen struct {
		kind  controlmode.Kind
		win   string
		burst bool
	}
	var got []seen
	async.drain(func(l controlmode.Line, burst bool) bool {
		got = append(got, seen{l.Kind, l.Args[0], burst})
		return false
	})

	want := []seen{
		{controlmode.LayoutChange, "@1", true},
		{controlmode.WindowRenamed, "@1", false},
		{controlmode.LayoutChange, "@2", false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("dispatched = %+v, want %+v", got, want)
	}
}

// A survivor equal to the applied state is the out-and-back's tail: with the
// burst flag it must read and reseed every tiled pane.
func TestDispatchLayoutChangeBurstSeedsEveryTiledPane(t *testing.T) {
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
	rt := scriptedRTRouterW(transientScript(transientThreePane, "%0", "0", "SEED0", "SEED1", "SEED2"), router, sent)
	f := &layoutTmux{windowID: "@101\n"}
	l := noticeLine("@1", transientThreePane, transientThreePane, "*")

	if retire := dispatchLayoutChange(f.config(), w, l, true, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	assertTransientSeeds(t, router, logs, sent, w.remotePanes, w.remotePanes)
	if w.reshapeOwed {
		t.Error("reshapeOwed still set after the pass consumed it")
	}
}

// The same duplicate notice without the burst flag stays silent.
func TestDispatchLayoutChangeNoBurstStaysSilent(t *testing.T) {
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

	if retire := dispatchLayoutChange(fake.config(), w, l, false, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	if len(sent.entries) != 0 {
		t.Errorf("wrote %q, want nothing: a duplicate notice costs zero round-trips", streamText(sent))
	}
	for id, log := range logs {
		if got := log.seeds(); len(got) != 0 {
			t.Errorf("%s got seeds %q, want none", id, got)
		}
	}
}

// A burst whose survivor is a real reshape applies it and seeds once: the mark
// is consumed at reconcileSnapshot's entry, so the shaped branch's reseed is
// the only one and the owed reseed does not add a second batch.
func TestDispatchLayoutChangeBurstWithRealReshapeSeedsOnce(t *testing.T) {
	seedCounts := func(burst bool) (counts map[string]int, reshapeOwed bool) {
		router := NewRouter()
		router.Register("%0", newOutputSink(drainedPipe(t), nil))
		router.Register("%1", newOutputSink(drainedPipe(t), nil))
		w := shapedMirror(t)
		script := strings.Join([]string{
			"%begin 1 1 1", "0 0 0 0 0 0 0 0 0", "%end 1 1 1", // PaneSeed(%0): cursor
			"%begin 1 2 1", "SEED0", "%end 1 2 1", // PaneSeed(%0): capture
			"%begin 1 3 1", "0 0 0 0 0 0 0 0 0", "%end 1 3 1", // PaneSeed(%1): cursor
			"%begin 1 4 1", "SEED1", "%end 1 4 1", // PaneSeed(%1): capture
			"%begin 1 5 1", "@1 " + noticeReshapedLayout + " %0 0", "%end 1 5 1", // trailing re-read: converged
		}, "\n") + "\n"
		log := &orderedLog{}
		var zoomReads []int
		rt := scriptedRTRouterW(script, router, log)
		cfg := geometryOrderingFake(log, &zoomReads)
		l := noticeLine("@1", noticeReshapedLayout, noticeReshapedLayout, "*")

		if retire := dispatchLayoutChange(cfg, w, l, burst, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
			t.Errorf("burst=%v: retire = true, want false", burst)
		}
		trace := streamText(log)
		return map[string]int{
			"%0": strings.Count(trace, captureCmd("%0")),
			"%1": strings.Count(trace, captureCmd("%1")),
		}, w.reshapeOwed
	}

	plain, _ := seedCounts(false)
	burst, owed := seedCounts(true)
	want := map[string]int{"%0": 1, "%1": 1}
	if !maps.Equal(plain, want) {
		t.Errorf("shape reseed captures = %v, want %v", plain, want)
	}
	if !maps.Equal(burst, want) {
		t.Errorf("burst captures = %v, want %v: the mark is consumed at entry, so no second seed batch", burst, want)
	}
	if owed {
		t.Error("reshapeOwed still set after the pass consumed it")
	}
}

// A burst of notices that differ only in a float's geometry reaches dispatch
// unflagged and moves the float alone: no tiled pane is captured.
func TestDispatchLayoutChangeFloatOnlyBurstSeedsNoTiledPane(t *testing.T) {
	lines := []controlmode.Line{
		noticeLine("@1", tiledFloatLayout, tiledFloatLayout, "*"),
		noticeLine("@1", floatMovedLayout, floatMovedLayout, "*"),
	}
	out, burst := coalesceLayoutBursts(lines)
	if len(out) != 1 || burst[0] {
		t.Fatalf("coalesced = %d lines, burst = %v, want one unflagged survivor", len(out), burst)
	}

	router := NewRouter()
	for _, id := range []string{"%0", "%1", "%9"} {
		router.Register(id, newOutputSink(drainedPipe(t), nil))
	}
	w := shapedMirror(t)
	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9
	script := strings.Join([]string{
		"%begin 1 1 1", "@1 " + floatMovedLayout + " %0 0", "%end 1 1 1", // readLayout: the float moved
	}, "\n") + "\n" + floatSeedScript(2, "FLOAT")
	log := &orderedLog{}
	rt := scriptedRTRouterW(script, router, log)
	f := &layoutTmux{windowID: "@101\n"}

	if retire := dispatchLayoutChange(f.config(), w, out[0], burst[0], func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); retire {
		t.Errorf("retire = true, want false")
	}

	trace := streamText(log)
	for _, id := range []string{"%0", "%1"} {
		if strings.Contains(trace, captureCmd(id)) {
			t.Errorf("captured tiled pane %s, want none for a float-only burst: %q", id, trace)
		}
	}
	if !strings.Contains(trace, captureCmd("%9")) {
		t.Errorf("trace %q lacks the moved float's repaint, so the test did not reach the float branch", trace)
	}
}
