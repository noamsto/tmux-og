package daemon

import "slices"

// paneOps is the local pane surgery that turns a mirror window's current pane
// order into the remote's. Phases are applied in field order: Remove, Append,
// Swaps. Reset short-circuits all of them.
//
// Local panes are addressed by 0-based index (the daemon forces a window-level
// pane-base-index 0 — daemon.go), and the mirror's core invariant is that local
// pane i renders want[i].
type paneOps struct {
	// Reset means no current pane survives, so there is nothing to split off
	// and nothing to keep: the window must be torn down and rebuilt. Killing
	// every pane instead would make tmux destroy the window itself, leaving a
	// registry entry whose localWin no longer exists.
	Reset bool
	// Remove lists local indices to kill, descending, so each kill only shifts
	// indices above ones already handled.
	Remove []int
	// Append lists remote pane ids to add, in want order. They land at the tail
	// by splitting the current last pane, so the k-th append is at
	// len(survivors)+k.
	Append []string
	// Swaps lists local index transpositions to apply in order, after Remove
	// and Append, to reach want order exactly.
	Swaps [][2]int
}

// planPaneOps diffs the remote pane order a mirror window currently renders
// (have) against the order the remote now reports (want).
//
// It replaces the three special cases the mirror shipped with — identical,
// pure tail-append, pure tail-removal — which between them cannot express a
// mid-list insert or removal. Those are not exotic: splitting or killing any
// pane that is not last in layout-traversal order produces one (a remote
// 3-pane window %0 %1 %2 split at %0 reports %0 %3 %1 %2), and the old code
// logged "unsupported pane reshuffle" and left the mirror silently stale.
func planPaneOps(have, want []string) paneOps {
	wantSet := make(map[string]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}

	var survivors []string
	var ops paneOps
	for i, h := range slices.Backward(have) {
		if !wantSet[h] {
			ops.Remove = append(ops.Remove, i)
		}
	}
	for _, id := range have {
		if wantSet[id] {
			survivors = append(survivors, id)
		}
	}

	// No survivor means no pane to split off for the appends. Rebuild instead.
	if len(survivors) == 0 {
		return paneOps{Reset: true}
	}

	haveSet := make(map[string]bool, len(survivors))
	for _, id := range survivors {
		haveSet[id] = true
	}
	cur := append([]string(nil), survivors...)
	for _, id := range want {
		if !haveSet[id] {
			ops.Append = append(ops.Append, id)
			cur = append(cur, id)
		}
	}

	// cur now holds want's elements, possibly in the wrong order. Selection
	// sort into place: each mismatch costs exactly one transposition.
	for i := range want {
		if cur[i] == want[i] {
			continue
		}
		for j := i + 1; j < len(cur); j++ {
			if cur[j] == want[i] { //nolint:gosec // callers pass unique pane ids, so cur ends up len(want) long
				cur[i], cur[j] = cur[j], cur[i]
				ops.Swaps = append(ops.Swaps, [2]int{i, j})
				break
			}
		}
	}
	return ops
}
