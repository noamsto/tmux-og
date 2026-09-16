package daemon

import (
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// noticeUnchangedLayout is a one-pane window, matching reconcilededup_test.go's
// fixture: small enough that "unchanged" and "changed" are unambiguous, and
// single-pane so a pane-set change (adding one) is the simplest possible gate
// 1 trigger.
const noticeUnchangedLayout = "bd67,190x45,0,0,3"

// noticeTwoPaneLayout is a %layout-change reporting the SAME window split into
// two panes — a pane set reconcileLayoutFrom's gate 1 must reject as stale
// once the read reports the remote already back to noticeUnchangedLayout.
const noticeTwoPaneLayout = "beef,190x45,0,0{95x45,0,0,3,95x45,95,0,4}"

// noticeReshapedLayout is reconcilelayout_test.go's tiledLayout's same two
// panes at a different split — same pane set and order, different cell
// widths, no floats — the geometry-only reshape gate 5 now applies straight
// from the notification.
const noticeReshapedLayout = "abcd,190x45,0,0{100x45,0,0,0,89x45,101,0,1}"

// noticeReshapedLayoutC is a second reshape past noticeReshapedLayout, for
// TestNoticeStaleGeometryHeals: the trailing re-read reports the remote has
// already moved on again, so a second pass has to reapply and reseed before
// converging.
const noticeReshapedLayoutC = "ef01,190x45,0,0{80x45,0,0,0,109x45,80,0,1}"

// noticeLine builds a %layout-change line and runs it through
// controlmode.ParseLine, so every test exercises the real field split rather
// than constructing Args by hand. flags == "" produces the 3-field form: tmux
// omits an empty window_printable_flags field rather than emit it blank.
func noticeLine(win, layout, visible, flags string) controlmode.Line {
	text := "%layout-change " + win + " " + layout + " " + visible
	if flags != "" {
		text += " " + flags
	}
	return controlmode.ParseLine(text)
}

// noticeFake is the Config seam for reconcileLayoutFrom tests. LocalTmux
// records every argv it's called with. When sent is non-nil, a call reached
// before anything is on the wire fails the test outright: a fork must never
// precede the read a pure gate has already decided to take.
type noticeFake struct {
	t     *testing.T
	sent  interface{ Len() int }
	local string

	localTmux []string
}

func (f *noticeFake) config() Config {
	return Config{
		LocalTmux: func(args ...string) error {
			f.localTmux = append(f.localTmux, strings.Join(args, " "))
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			for _, a := range args {
				if a == "#{window_zoomed_flag}" {
					return f.local, nil
				}
			}
			return "", nil
		},
	}
}

// TestNoticeNoOpWritesNothing is gate 3's whole point: a notification that
// changes nothing — a duplicate, an echo of the daemon's own verb, or the
// trailing line of a push/pop-zoom bracket once its predecessor has already
// landed — costs zero remote round-trips, not merely the one cheap read the
// pre-notification dedup paid.
func TestNoticeNoOpWritesNothing(t *testing.T) {
	cases := []struct {
		name  string
		flags string
		local string
	}{
		{"unzoomed", "*", "0\n"},
		{"zoomed", "*Z", "1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &mirrorWindow{
				remoteID:    "@1",
				localWin:    "@101",
				remotePanes: []string{"%3"},
				localPanes:  []string{"%l3"},
				layout:      noticeUnchangedLayout,
				appliedZoom: c.local == "1\n",
			}
			rt, sent := scriptedRT("")
			// No sent guard here: gate 3's own fork IS what runs before any
			// read on this path — that's the no-op being tested, not a bug.
			fake := &noticeFake{local: c.local}
			l := noticeLine("@1", noticeUnchangedLayout, noticeUnchangedLayout, c.flags)

			if got := reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt); got {
				t.Errorf("retire = true, want false")
			}
			if sent.Len() != 0 {
				t.Errorf("sent %q, want nothing: gate 3's no-op must cost zero remote round-trips", sent.String())
			}
			if len(fake.localTmux) != 0 {
				t.Errorf("LocalTmux calls = %v, want none", fake.localTmux)
			}

			// Negative control: today's read-first entry cannot keep the
			// stream silent even on the identical fixture, which is what
			// makes the assertion above bite rather than pass vacuously.
			t.Run("negative control", func(t *testing.T) {
				w := &mirrorWindow{
					remoteID:    "@1",
					localWin:    "@101",
					remotePanes: []string{"%3"},
					localPanes:  []string{"%l3"},
					layout:      noticeUnchangedLayout,
				}
				rt, sent := scriptedRT("")
				fake := &noticeFake{local: c.local}
				reconcileLayout(fake.config(), w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)
				if sent.Len() == 0 {
					t.Error("sent nothing: want the read-first entry to write a display-message even here")
				}
			})
		})
	}
}

// TestNoticeZoomOnReads is #413 arriving as a notification: the flag comes on
// against an unchanged layout while the mirror is still unzoomed, so gate 3
// must read rather than trust the line, and the zoom is then applied from the
// read's own snapshot.
func TestNoticeZoomOnReads(t *testing.T) {
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      noticeUnchangedLayout,
	}
	script := strings.Join([]string{
		"%begin 1 1 1", noticeUnchangedLayout + " %3 1", "%end 1 1 1", // readLayout
		"%begin 1 2 1", noticeUnchangedLayout + " %3 1", "%end 1 2 1", // trailing re-read: converged
	}, "\n") + "\n"
	rt, sent := scriptedRT(script)
	// Gate 3 sees the notification's zoom flag disagree with appliedZoom and
	// sends this to the read-first path.
	fake := &noticeFake{local: "0\n"}
	l := noticeLine("@1", noticeUnchangedLayout, noticeUnchangedLayout, "*Z")

	reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (gate 3: flag mismatch)", sent.String())
	}
	found := false
	for _, c := range fake.localTmux {
		if strings.Contains(c, "resize-pane -Z") {
			found = true
		}
	}
	if !found {
		t.Errorf("LocalTmux calls = %v, want resize-pane -Z applied from the read", fake.localTmux)
	}
}

// TestNoticeUnzoomTransientReads is the push/pop-zoom bracket's transient
// middle line: the flag arrives off against an unchanged layout while the
// mirror is still zoomed, which gate 3 cannot tell apart from a real unzoom
// without reading. Here the read finds the remote still zoomed (the bracket's
// own trailing line hasn't landed yet), so nothing must flap.
func TestNoticeUnzoomTransientReads(t *testing.T) {
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      noticeUnchangedLayout,
		appliedZoom: true,
	}
	script := strings.Join([]string{
		"%begin 1 1 1", noticeUnchangedLayout + " %3 1", "%end 1 1 1", // readLayout: still zoomed
	}, "\n") + "\n"
	rt, sent := scriptedRT(script)
	// Gate 3 sees the notification's zoom flag disagree with appliedZoom and
	// sends this to the read-first path.
	fake := &noticeFake{local: "1\n"}
	l := noticeLine("@1", noticeUnchangedLayout, noticeUnchangedLayout, "*")

	reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (gate 3: flag mismatch)", sent.String())
	}
	// reconcileSnapshot's own dedup returns right after this read — the layout
	// is unchanged and the read's zoom agrees with the mirror's — so a second
	// round-trip here would mean the dedup missed and fell through to the pass
	// loop's own trailing re-read.
	if n := strings.Count(sent.String(), readLayoutFmt); n != 1 {
		t.Errorf("readLayout format appears %d times, want exactly 1: reconcileSnapshot's dedup must not fall through", n)
	}
	for _, c := range fake.localTmux {
		if strings.Contains(c, "resize-pane -Z") {
			t.Errorf("LocalTmux calls = %v, want no resize-pane -Z: the read found no real unzoom", fake.localTmux)
		}
		if strings.Contains(c, "select-layout") {
			t.Errorf("LocalTmux calls = %v, want no select-layout: the tiled shape never moved", fake.localTmux)
		}
	}
}

// TestNoticeStaleAppliedZoomReads is gate 3's mismatch case: an unzoom
// notification cannot be trusted while the mirror still records itself
// zoomed, so the read-first path must establish the remote state.
func TestNoticeStaleAppliedZoomReads(t *testing.T) {
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      noticeUnchangedLayout,
		appliedZoom: true,
	}
	// Empty on purpose: readLayout's display-message reaches the wire before
	// it fails on EOF, and the assertion is only that it was issued — there is
	// nothing here to reply to it.
	rt, sent := scriptedRT("")
	fake := &noticeFake{}
	l := noticeLine("@1", noticeUnchangedLayout, noticeUnchangedLayout, "*")

	reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (applied zoom state mismatched)", sent.String())
	}
}

// TestNoticeChangedLayoutUnzoomReadsWhenMirrorZoomed covers the case where a
// flag-off notification also changes geometry. The zoom mismatch must win over
// the geometry-only fast path, since a push/pop-zoom bracket can emit a
// transient unzoomed layout.
func TestNoticeChangedLayoutUnzoomReadsWhenMirrorZoomed(t *testing.T) {
	w := shapedMirror(t)
	w.appliedZoom = true
	// Empty on purpose: the read reaches the wire and then fails on EOF. The
	// LocalTmux seam fails if notification application happens first.
	rt, sent := scriptedRT("")
	cfg := zoomGateFake(t, sent)
	l := noticeLine("@1", noticeReshapedLayout, noticeReshapedLayout, "*")

	reconcileLayoutFrom(cfg, w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message before applying changed geometry", sent.String())
	}
}

// TestNoticePaneSetChangeReads is gate 1: the notification's pane set has
// moved past what the mirror last saw, so it needs the active pane for
// focus-follow and must not be trusted at face value — the read here finds
// the remote already back to one pane, and nothing structural is derived from
// the stale notification.
func TestNoticePaneSetChangeReads(t *testing.T) {
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      noticeUnchangedLayout,
	}
	script := strings.Join([]string{
		"%begin 1 1 1", noticeUnchangedLayout + " %3 0", "%end 1 1 1", // readLayout: remote already back
	}, "\n") + "\n"
	rt, sent := scriptedRT(script)
	fake := &noticeFake{t: t, sent: sent, local: "0\n"}
	l := noticeLine("@1", noticeTwoPaneLayout, noticeTwoPaneLayout, "*")

	reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (gate 1: pane set differs)", sent.String())
	}
	for _, c := range fake.localTmux {
		if strings.Contains(c, "split-window") {
			t.Errorf("LocalTmux calls = %v, want no split-window: the read found the remote already back", fake.localTmux)
		}
	}
}

// TestNoticeFloatChangeReads is gate 2: tiledFloatLayout shares tiledLayout's
// Raw by construction (see reconcilelayout_test.go), so this is the one case
// where the float gate has to fire before gate 3 would otherwise have called
// it a no-op.
func TestNoticeFloatChangeReads(t *testing.T) {
	w := shapedMirror(t)
	script := strings.Join([]string{
		"%begin 1 1 1", tiledLayout + " %0 0", "%end 1 1 1", // readLayout: floats unchanged
	}, "\n") + "\n"
	rt, sent := scriptedRT(script)
	fake := &noticeFake{t: t, sent: sent, local: "0\n"}
	l := noticeLine("@1", tiledFloatLayout, tiledFloatLayout, "*")

	reconcileLayoutFrom(fake.config(), w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (gate 2: floats differ)", sent.String())
	}
}

// zoomGateFake is the Config seam for the zoom gates: LocalTmux fatals if it is
// called before anything reaches the wire, since a zoomed reshape must never
// apply the notification's layout without first reading the active pane the
// toggle needs.
func zoomGateFake(t *testing.T, sent interface{ Len() int }) Config {
	t.Helper()
	return Config{
		LocalTmux: func(args ...string) error {
			if sent.Len() == 0 {
				t.Fatalf("LocalTmux(%v) called before any read reached the wire", args)
			}
			return nil
		},
	}
}

// TestNoticeZoomedReshapeReads is gate 4: a layout change arriving with the
// zoom flag on needs the active pane for the -Z toggle and the zoomed pane's
// dims, neither of which the notification carries, so a zoomed reshape always
// reads — gate 5 never gets a chance at it, however narrow the geometry
// change.
func TestNoticeZoomedReshapeReads(t *testing.T) {
	w := shapedMirror(t)
	// Empty on purpose: the read reaches
	// the wire and then fails on EOF, and the assertion only needs it issued.
	rt, sent := scriptedRT("")
	cfg := zoomGateFake(t, sent)
	l := noticeLine("@1", noticeReshapedLayout, noticeReshapedLayout, "*Z")

	reconcileLayoutFrom(cfg, w, l, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if !strings.Contains(sent.String(), "window_zoomed_flag") {
		t.Errorf("sent %q, want a readLayout display-message (gate 4: zoom flag on)", sent.String())
	}
}

// readLayoutFmt is the -F value readLayout's display-message sends: its
// presence in the stream trace is what pins "a read happened", and counting
// it distinguishes gate 5's dropped leading read from the trailing re-read
// that stays.
const readLayoutFmt = "#{window_layout} #{pane_id} #{window_zoomed_flag}"

// geometryOrderingFake is the Config seam for the geometry-only tests.
// LocalTmux argv lands in log, the trace the control stream also writes into,
// so select-layout's position can be compared against the stream's reads.
// LocalTmuxOut stays out of the log: it answers #{window_zoomed_flag} with
// "0\n" and records len(log.entries) into zoomReads.
func geometryOrderingFake(log *orderedLog, zoomReads *[]int) Config {
	return Config{
		LocalTmux: func(args ...string) error {
			log.append(strings.Join(args, " "))
			return nil
		},
		LocalTmuxOut: func(args ...string) (string, error) {
			for _, a := range args {
				if a == "#{window_zoomed_flag}" {
					*zoomReads = append(*zoomReads, len(log.entries))
					return "0\n", nil
				}
			}
			return "", nil
		},
	}
}

// TestNoticeGeometryOnlyAppliesFromNotification is gate 5 itself: a
// geometry-only reshape enters the pass loop straight from the notification's
// layout, with no leading read — select-layout is applied from L.Raw, and the
// wire carries only the trailing re-read that closes the pass.
func TestNoticeGeometryOnlyAppliesFromNotification(t *testing.T) {
	router := NewRouter()
	router.Register("%0", newOutputSink(drainedPipe(t), nil))
	router.Register("%1", newOutputSink(drainedPipe(t), nil))

	w := shapedMirror(t)
	script := strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0", "%end 1 1 1", // PaneSeed(%0): cursor
		"%begin 1 2 1", "SEED0", "%end 1 2 1", // PaneSeed(%0): capture
		"%begin 1 3 1", "0 0 0 0", "%end 1 3 1", // PaneSeed(%1): cursor
		"%begin 1 4 1", "SEED1", "%end 1 4 1", // PaneSeed(%1): capture
		"%begin 1 5 1", noticeReshapedLayout + " %0 0", "%end 1 5 1", // trailing re-read: converged
	}, "\n") + "\n"

	log := &orderedLog{}
	var zoomReads []int
	rt := scriptedRTRouterW(script, router, log)
	cfg := geometryOrderingFake(log, &zoomReads)
	l := noticeLine("@1", noticeReshapedLayout, noticeReshapedLayout, "*")

	if got := reconcileLayoutFrom(cfg, w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); got {
		t.Errorf("retire = true, want false")
	}

	selIdx := log.indexContainingAll("select-layout")
	if selIdx < 0 {
		t.Fatalf("LocalTmux calls = %v, want select-layout applying the notification's layout", log.entries)
	}
	if !strings.Contains(log.entries[selIdx], noticeReshapedLayout) {
		t.Errorf("select-layout call %q, want the notification's own Raw %q", log.entries[selIdx], noticeReshapedLayout)
	}
	trace := strings.Join(log.entries, "\n")
	if n := strings.Count(trace, readLayoutFmt); n != 1 {
		t.Errorf("readLayout format appears %d times, want exactly 1: the trailing read only, gate 5 drops the leading one", n)
	}
	for _, idx := range zoomReads {
		if idx <= selIdx {
			t.Errorf("local zoom check recorded at %d, want it after select-layout (index %d): the pass loop's zoom assert must come after its own shape, never before", idx, selIdx)
		}
	}
	if w.layout != noticeReshapedLayout {
		t.Errorf("w.layout = %q, want %q", w.layout, noticeReshapedLayout)
	}
}

// noticeReshapedFloatLayout is noticeReshapedLayout's reshape in the v2 JSON a
// %layout-change carries once the client opted into new layouts, with remote
// float %9 at float9's cell — the float shapedMirror is given below.
const noticeReshapedFloatLayout = `{"V":2,"L":{"t":"h","w":190,"h":45,"x":0,"y":0,"c":[` +
	`{"t":"p","w":100,"h":45,"x":0,"y":0,"a":true,"i":0,"I":"%0"},` +
	`{"t":"p","w":89,"h":45,"x":101,"y":0,"i":1,"I":"%1"},` +
	`{"t":"p","w":18,"h":6,"x":11,"y":6,"i":2,"z":0,"I":"%9"}]}}`

// TestNoticeGeometryOnlyBehindAMirroredFloatAppliesFromNotification is gate 5
// on a window holding a mirrored float: the tiled-only select-layout leaves the
// float in place, so the float needs no read, no kill and no re-add — the pass
// runs straight from the notification exactly as it does on a float-free
// window.
func TestNoticeGeometryOnlyBehindAMirroredFloatAppliesFromNotification(t *testing.T) {
	router := NewRouter()
	router.Register("%0", newOutputSink(drainedPipe(t), nil))
	router.Register("%1", newOutputSink(drainedPipe(t), nil))

	w := shapedMirror(t)
	w.localFloats["%9"] = "%l9"
	w.floatGeom["%9"] = float9
	L := mustLayout(t, noticeReshapedFloatLayout)
	script := strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0", "%end 1 1 1", // PaneSeed(%0): cursor
		"%begin 1 2 1", "SEED0", "%end 1 2 1", // PaneSeed(%0): capture
		"%begin 1 3 1", "0 0 0 0", "%end 1 3 1", // PaneSeed(%1): cursor
		"%begin 1 4 1", "SEED1", "%end 1 4 1", // PaneSeed(%1): capture
		"%begin 1 5 1", noticeReshapedFloatLayout + " %0 0", "%end 1 5 1", // trailing re-read: converged
	}, "\n") + "\n"

	log := &orderedLog{}
	var zoomReads []int
	rt := scriptedRTRouterW(script, router, log)
	cfg := geometryOrderingFake(log, &zoomReads)
	l := noticeLine("@1", noticeReshapedFloatLayout, noticeReshapedFloatLayout, "*")

	if got := reconcileLayoutFrom(cfg, w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt); got {
		t.Errorf("retire = true, want false")
	}

	selIdx := log.indexContainingAll("select-layout", L.Raw)
	if selIdx < 0 {
		t.Fatalf("calls = %v, want select-layout applying the notification's tiled-only Raw %q", log.entries, L.Raw)
	}
	trace := strings.Join(log.entries, "\n")
	if n := strings.Count(trace, readLayoutFmt); n != 1 {
		t.Errorf("readLayout format appears %d times, want exactly 1: the trailing read only, no leading read for the float", n)
	}
	if first := strings.Index(trace, readLayoutFmt); first >= 0 && first < strings.Index(trace, "select-layout") {
		t.Errorf("a layout read reached the wire before select-layout: %v", log.entries)
	}
	for _, verb := range []string{"kill-pane", "new-pane"} {
		if idx := log.indexContainingAll(verb); idx >= 0 {
			t.Errorf("issued %q, want the mirrored float left alone", log.entries[idx])
		}
	}
	if w.localFloats["%9"] != "%l9" {
		t.Errorf("localFloats = %v, want the mirrored float still recorded", w.localFloats)
	}
	if w.layout != L.Raw {
		t.Errorf("w.layout = %q, want %q", w.layout, L.Raw)
	}
}

// TestNoticeStaleGeometryHeals is the bounded-staleness case: the remote has
// already moved past the geometry gate 5 applied by the time the trailing
// re-read lands, so a second pass reapplies and reseeds against the newer
// layout before converging — the same "run another pass on ground truth"
// behaviour reconcileSnapshot always had, now reachable from a notification.
func TestNoticeStaleGeometryHeals(t *testing.T) {
	router := NewRouter()
	router.Register("%0", newOutputSink(drainedPipe(t), nil))
	router.Register("%1", newOutputSink(drainedPipe(t), nil))

	w := shapedMirror(t)
	script := strings.Join([]string{
		"%begin 1 1 1", "0 0 0 0", "%end 1 1 1", // pass 1 PaneSeed(%0): cursor
		"%begin 1 2 1", "SEED0", "%end 1 2 1", // pass 1 PaneSeed(%0): capture
		"%begin 1 3 1", "0 0 0 0", "%end 1 3 1", // pass 1 PaneSeed(%1): cursor
		"%begin 1 4 1", "SEED1", "%end 1 4 1", // pass 1 PaneSeed(%1): capture
		"%begin 1 5 1", noticeReshapedLayoutC + " %0 0", "%end 1 5 1", // trailing re-read: remote moved on to C
		"%begin 1 6 1", "0 0 0 0", "%end 1 6 1", // pass 2 PaneSeed(%0): cursor
		"%begin 1 7 1", "SEED0", "%end 1 7 1", // pass 2 PaneSeed(%0): capture
		"%begin 1 8 1", "0 0 0 0", "%end 1 8 1", // pass 2 PaneSeed(%1): cursor
		"%begin 1 9 1", "SEED1", "%end 1 9 1", // pass 2 PaneSeed(%1): capture
		"%begin 1 10 1", noticeReshapedLayoutC + " %0 0", "%end 1 10 1", // trailing re-read: converged on C
	}, "\n") + "\n"

	log := &orderedLog{}
	var zoomReads []int
	rt := scriptedRTRouterW(script, router, log)
	cfg := geometryOrderingFake(log, &zoomReads)
	l := noticeLine("@1", noticeReshapedLayout, noticeReshapedLayout, "*")

	reconcileLayoutFrom(cfg, w, l, func(string) {}, router, noHellos, newCtlState(), newConverger(), rt)

	selIdxLast := -1
	for i, e := range log.entries {
		if strings.Contains(e, "select-layout") {
			selIdxLast = i
		}
	}
	if selIdxLast < 0 {
		t.Fatalf("LocalTmux calls = %v, want select-layout applied at least once", log.entries)
	}
	if !strings.Contains(log.entries[selIdxLast], noticeReshapedLayoutC) {
		t.Errorf("last select-layout call %q, want the converged Raw %q", log.entries[selIdxLast], noticeReshapedLayoutC)
	}
	if w.layout != noticeReshapedLayoutC {
		t.Errorf("w.layout = %q, want %q (converged after healing the stale first pass)", w.layout, noticeReshapedLayoutC)
	}
}
