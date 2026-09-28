package daemon

import (
	"strings"
	"testing"
)

// TestReconcileLayoutDedupsUnchangedLayout is #431: a resize burst or a
// spurious/duplicate %layout-change must not pay for a full pane diff and
// PaneSeed storm when the remote's layout and zoom state haven't actually
// moved since the last reconcile.
func TestReconcileLayoutDedupsUnchangedLayout(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      layout,
	}

	rt, sent := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + layout + " %3 0", "%end 1 1 1", // readLayout
	}, "\n") + "\n")

	cfg := Config{
		LocalTmux: func(...string) error {
			t.Fatal("unexpected LocalTmux call on dedup path")
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	if strings.Contains(sent.String(), "capture-pane") {
		t.Errorf("sent %q, want no capture-pane (no PaneSeed round-trip)", sent.String())
	}
	if strings.Contains(sent.String(), "select-layout") {
		t.Errorf("sent %q, want no select-layout (no reshape)", sent.String())
	}
}

// TestReconcileLayoutFallsThroughOnZoomChange is #413 guarded against
// regressing via #431's dedup: #{window_layout} is deliberately the unzoomed
// geometry, so a zoom-only %layout-change reports the same L.Raw as last time
// even though the zoom flag itself moved. The dedup check must not treat that
// as "nothing to do" — it has to fall through and actually apply the zoom.
//
// appliedZoom false is also what dropMirroredPanes leaves before setupWindow's
// select-layout; without clearing a stale true, resetWindow's rebuild would
// skip this path on the next reconcile.
func TestReconcileLayoutFallsThroughOnZoomChange(t *testing.T) {
	const layout = "bd67,190x45,0,0,3"
	w := &mirrorWindow{
		remoteID:    "@1",
		localWin:    "@101",
		remotePanes: []string{"%3"},
		localPanes:  []string{"%l3"},
		layout:      layout,
	}

	rt, _ := scriptedRT(strings.Join([]string{
		"%begin 1 1 1", "@1 " + layout + " %3 1", "%end 1 1 1", // readLayout (dedup check + pass 0)
		"%begin 1 2 1", "@1 " + layout + " %3 1", "%end 1 2 1", // trailing re-read: unchanged, stop
	}, "\n") + "\n")

	var calls []string
	cfg := Config{
		LocalTmux: func(args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return nil
		},
	}

	reconcileLayout(cfg, w, func(string) {}, NewRouter(), noHellos, newCtlState(), newConverger(), rt)

	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "if -F") || !strings.Contains(joined, "resize-pane -Z") {
		t.Errorf("LocalTmux calls = %q, want an if -F zoom assert (fell through the dedup check)", joined)
	}
	if !strings.Contains(joined, "resize-window") {
		t.Errorf("LocalTmux calls = %q, want a resize-window (applyLayout ran)", joined)
	}
	if !w.appliedZoom {
		t.Error("appliedZoom = false after zoom assert, want true")
	}
}
