package daemon

import (
	"reflect"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestPlanFloatOps(t *testing.T) {
	cellA := controlmode.PaneCell{ID: "%2", W: 30, H: 7, X: 9, Y: 3}
	cellAMoved := controlmode.PaneCell{ID: "%2", W: 40, H: 10, X: 0, Y: 0}
	cellB := controlmode.PaneCell{ID: "%5", W: 20, H: 5, X: 1, Y: 1}

	// farEdge is a borderless float flush against the far edge: its outer box
	// (cell + 2 on each axis) reaches past a 99-column window, so tmux's clamp
	// slides it back there but leaves it alone in a 100-column one.
	farEdge := controlmode.PaneCell{ID: "%7", W: 89, H: 12, X: 10, Y: 5}
	at := func(c controlmode.PaneCell, w, h int) appliedFloat { return appliedFloat{c, w, h} }

	tests := []struct {
		name       string
		have       map[string]appliedFloat
		want       []controlmode.PaneCell
		winW, winH int
		ops        floatOps
	}{
		{
			name: "no-op",
			have: map[string]appliedFloat{"%2": at(cellA, 100, 40)},
			want: []controlmode.PaneCell{cellA},
			winW: 100, winH: 40,
			ops: floatOps{},
		},
		{
			name: "add-only",
			have: map[string]appliedFloat{},
			want: []controlmode.PaneCell{cellA, cellB},
			winW: 100, winH: 40,
			ops: floatOps{Add: []controlmode.PaneCell{cellA, cellB}},
		},
		{
			name: "remove-only",
			have: map[string]appliedFloat{"%2": at(cellA, 100, 40), "%5": at(cellB, 100, 40)},
			want: nil,
			winW: 100, winH: 40,
			ops: floatOps{Remove: []string{"%2", "%5"}},
		},
		{
			name: "move-only",
			have: map[string]appliedFloat{"%2": at(cellA, 100, 40)},
			want: []controlmode.PaneCell{cellAMoved},
			winW: 100, winH: 40,
			ops: floatOps{Move: []controlmode.PaneCell{cellAMoved}},
		},
		{
			name: "simultaneous add, remove and move",
			// %2 survives but moves, %5 is dropped, %9 is new.
			have: map[string]appliedFloat{"%2": at(cellA, 100, 40), "%5": at(cellB, 100, 40)},
			want: []controlmode.PaneCell{cellAMoved, {ID: "%9", W: 15, H: 4, X: 2, Y: 2}},
			winW: 100, winH: 40,
			ops: floatOps{
				Remove: []string{"%5"},
				Add:    []controlmode.PaneCell{{ID: "%9", W: 15, H: 4, X: 2, Y: 2}},
				Move:   []controlmode.PaneCell{cellAMoved},
			},
		},
		{
			name: "unchanged cell, window shrinks so the outer box clamps differently",
			have: map[string]appliedFloat{"%7": at(farEdge, 100, 32)},
			want: []controlmode.PaneCell{farEdge},
			winW: 99, winH: 32,
			ops: floatOps{Move: []controlmode.PaneCell{farEdge}},
		},
		{
			name: "unchanged cell, window regrows back out of the clamp",
			have: map[string]appliedFloat{"%7": at(farEdge, 99, 32)},
			want: []controlmode.PaneCell{farEdge},
			winW: 100, winH: 32,
			ops: floatOps{Move: []controlmode.PaneCell{farEdge}},
		},
		{
			name: "window changes but the outer box is identical",
			have: map[string]appliedFloat{"%2": at(cellA, 100, 40)},
			want: []controlmode.PaneCell{cellA},
			winW: 120, winH: 50,
			ops: floatOps{},
		},
		{
			name: "cell changes at an unchanged window size",
			have: map[string]appliedFloat{"%7": at(farEdge, 100, 32)},
			want: []controlmode.PaneCell{{ID: "%7", W: 89, H: 12, X: 9, Y: 5}},
			winW: 100, winH: 32,
			ops: floatOps{Move: []controlmode.PaneCell{{ID: "%7", W: 89, H: 12, X: 9, Y: 5}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planFloatOps(tt.have, tt.want, tt.winW, tt.winH)
			if !reflect.DeepEqual(got, tt.ops) {
				t.Errorf("planFloatOps(%v, %v, %d, %d) = %+v, want %+v", tt.have, tt.want, tt.winW, tt.winH, got, tt.ops)
			}
		})
	}
}

// TestPlanFloatOpsRemoveIsSorted pins Remove's ordering against map iteration
// order, which Go deliberately randomizes: a single-case assertion could pass
// by luck, so this drives enough distinct removed ids that an unsorted result
// would eventually mismatch across runs.
func TestPlanFloatOpsRemoveIsSorted(t *testing.T) {
	have := map[string]appliedFloat{
		"%9": {}, "%2": {}, "%5": {}, "%1": {}, "%7": {},
	}
	want := []string{"%1", "%2", "%5", "%7", "%9"}

	got := planFloatOps(have, nil, 100, 40).Remove
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Remove = %v, want %v", got, want)
	}
}
