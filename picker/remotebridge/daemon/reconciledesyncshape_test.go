package daemon

import (
	"errors"
	"testing"
)

// tmux refuses a layout whose pane count disagrees with the window's, and it
// counts a DEAD pane — which a mirror window carries by design, since
// remain-on-exit keeps a dying renderer from taking the session (#547). On the
// non-structural path there is no applyPaneOps to raise errLocalPanesDesynced,
// so without this the mirror keeps stale geometry for good while its renderers
// paint the remote's current screen into it (#672).
func TestApplyLayoutReportsAPaneCountDesync(t *testing.T) {
	f := &layoutTmux{
		selectLayoutErr:  errors.New("have 3 panes but need 2"),
		paneCountListing: "%l0 0\n%l1 0\n%lcorpse 0\n",
	}
	w := mirrorWithFloat()
	L := mustLayout(t, tiledFloatLayout)

	ok, desynced := applyLayout(f.config(), w, L)
	if ok {
		t.Fatal("applyLayout ok = true, want false: select-layout was refused")
	}
	if !desynced {
		t.Fatal("desynced = false, want true: 3 local tiled panes against 2 remote")
	}
	if w.layout == L.Raw {
		t.Error("w.layout recorded a shape that never landed")
	}
}

// Floats are not counted by select-layout (measured), and refreshLocalPanes
// drops them for the same reason — so a window whose only extra pane floats is
// in agreement, and a rebuild would be pure churn.
func TestApplyLayoutDoesNotCountFloatsAsADesync(t *testing.T) {
	f := &layoutTmux{
		selectLayoutErr:  errors.New("invalid layout"),
		paneCountListing: "%l0 0\n%l1 0\n%l9 1\n",
	}
	w := mirrorWithFloat()
	L := mustLayout(t, tiledFloatLayout)

	if ok, desynced := applyLayout(f.config(), w, L); ok || desynced {
		t.Fatalf("applyLayout = (%v, %v), want (false, false): the float is not an extra pane", ok, desynced)
	}
}

// A live window always holds a pane, so an empty listing is a window that has
// gone — the retire path's business, not a count to rebuild on.
func TestApplyLayoutTreatsAnEmptyListingAsNoEvidence(t *testing.T) {
	f := &layoutTmux{selectLayoutErr: errors.New("can't find window: @101"), paneCountListing: "\n"}
	w := mirrorWithFloat()
	L := mustLayout(t, tiledFloatLayout)

	if _, desynced := applyLayout(f.config(), w, L); desynced {
		t.Error("desynced = true on an empty listing, want the retire path left to own it")
	}
}

// The rebuild this triggers reaps w.localPanes[1:], so a desync has to leave
// that list fresh — a stale one spares the pane that caused the mismatch, and
// setupWindow's own select-layout is then refused all over again.
func TestPaneCountDesyncAdoptsTheFreshPaneList(t *testing.T) {
	f := &layoutTmux{
		selectLayoutErr:  errors.New("have 3 panes but need 2"),
		paneCountListing: "%l0 0\n%l1 0\n%lcorpse 0\n",
	}
	w := mirrorWithFloat()
	L := mustLayout(t, tiledFloatLayout)

	applyLayout(f.config(), w, L)

	want := []string{"%l0", "%l1", "%lcorpse"}
	if len(w.localPanes) != len(want) {
		t.Fatalf("w.localPanes = %v, want %v so the rebuild reaps the extra pane", w.localPanes, want)
	}
	for i := range want {
		if w.localPanes[i] != want[i] {
			t.Fatalf("w.localPanes = %v, want %v", w.localPanes, want)
		}
	}
}

// A pass that reports no desync decided to do nothing, so it must not leave the
// caller's view of the window rewritten.
func TestNoDesyncLeavesLocalPanesAlone(t *testing.T) {
	f := &layoutTmux{
		selectLayoutErr:  errors.New("invalid layout"),
		paneCountListing: "%other0 0\n%other1 0\n",
	}
	w := mirrorWithFloat()
	before := append([]string(nil), w.localPanes...)
	L := mustLayout(t, tiledFloatLayout)

	applyLayout(f.config(), w, L)

	if len(w.localPanes) != len(before) || w.localPanes[0] != before[0] {
		t.Errorf("w.localPanes = %v, want %v untouched", w.localPanes, before)
	}
}

// A shape that applies asks nothing: the extra read is an error-path cost only.
func TestApplyLayoutSuccessDoesNotReadPanes(t *testing.T) {
	f := &layoutTmux{paneCountListing: "%l0 0\n"}
	w := mirrorWithFloat()
	L := mustLayout(t, tiledFloatLayout)

	if ok, desynced := applyLayout(f.config(), w, L); !ok || desynced {
		t.Fatalf("applyLayout = (%v, %v), want (true, false)", ok, desynced)
	}
	if got := f.verbs("list-panes"); got != nil {
		t.Errorf("issued %v, want no pane read on the pass that worked", got)
	}
}
