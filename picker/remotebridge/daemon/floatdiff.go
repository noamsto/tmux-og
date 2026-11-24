package daemon

import (
	"sort"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// floatOps is the local float-pane surgery that turns a mirror window's
// currently-applied floats into the remote's. Modeled on paneOps
// (panediff.go), but a float has no tiled order to preserve — each is
// addressed by its own remote pane id rather than a position — so there is no
// Swaps phase, only Remove/Add/Move.
type floatOps struct {
	// Remove lists remote float pane ids whose local mirror should be killed:
	// present in have, absent from want.
	Remove []string
	// Add lists cells to create a new local float for: present in want,
	// absent from have.
	Add []controlmode.PaneCell
	// Move lists cells for float ids present in both have and want whose
	// geometry, or the local outer box it maps to, differs from the one
	// recorded in have — the float survives, only its position/size needs
	// reasserting.
	Move []controlmode.PaneCell
}

// appliedFloat is the remote cell last applied to a local float and the local
// window size it was applied against. The local outer box is
// outerFromCell(cell, winW, winH), so the same cell lands on a different box
// once tmux's clamp sees another window size.
type appliedFloat struct {
	cell       controlmode.PaneCell
	winW, winH int
}

// outerChanged reports that cell, placed in a winW x winH window, lands on a
// different local outer box than prev did. A window resize can move or shrink a
// float whose remote cell never changed, because tmux clamps the local
// (bordered) box where the remote's borderless one fits; a resize that leaves
// the box alone must not count, or every one would reseed every float.
func outerChanged(prev appliedFloat, cell controlmode.PaneCell, winW, winH int) bool {
	pw, ph, px, py := outerFromCell(cell, prev.winW, prev.winH)
	w, h, x, y := outerFromCell(cell, winW, winH)
	return pw != w || ph != h || px != x || py != y
}

// planFloatOps diffs the float geometry a mirror window last applied locally
// (have, keyed by remote pane id) against what the remote now reports (want),
// for a local window of winW x winH. Pure: no I/O, no tmux calls.
//
// Emitted order is always Remove, then Add, then Move. Remove is built from a
// map, which iterates in no fixed order, so it is sorted; Add and Move follow
// want's own order, which is already deterministic, so neither is resorted.
func planFloatOps(have map[string]appliedFloat, want []controlmode.PaneCell, winW, winH int) floatOps {
	wantByID := make(map[string]controlmode.PaneCell, len(want))
	for _, cell := range want {
		wantByID[cell.ID] = cell
	}

	var ops floatOps
	for id := range have {
		if _, ok := wantByID[id]; !ok {
			ops.Remove = append(ops.Remove, id)
		}
	}
	sort.Strings(ops.Remove)

	for _, cell := range want {
		prev, ok := have[cell.ID]
		if !ok {
			ops.Add = append(ops.Add, cell)
			continue
		}
		if prev.cell != cell || outerChanged(prev, cell, winW, winH) {
			ops.Move = append(ops.Move, cell)
		}
	}
	return ops
}
