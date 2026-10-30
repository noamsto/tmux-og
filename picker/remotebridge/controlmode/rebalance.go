package controlmode

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// RebalanceClosedCell evens out the one split cell that lost a pane. prev is
// the layout before a pane closed and cur the layout after, in either format.
// It reports changed only when exactly one tiled pane P left, cur holds no
// other new pane, P's parent split in prev had three or more children (two is
// a collapse, which tmux sizes itself), and cur still has a split of that kind
// over the same remaining children. That split's children are then resized
// along its axis to equal shares (the last one takes the remainder), and the
// result carries the one cell's new offsets. Every other cell is left exactly
// as cur has it. Any other input, or a resize that would leave a pane under
// one cell, is not an error: it returns changed == false.
func RebalanceClosedCell(prev, cur string) (Layout, bool, error) {
	prevRoot, err := tiledTree(prev)
	if err != nil {
		return Layout{}, false, err
	}
	curRoot, err := tiledTree(cur)
	if err != nil {
		return Layout{}, false, err
	}

	closed, ok := closedPane(prevRoot, curRoot)
	if !ok {
		return Layout{}, false, nil
	}
	parent := parentOf(prevRoot, closed)
	if parent == nil || len(parent.children) < 3 {
		return Layout{}, false, nil
	}
	var kept [][]string
	for _, c := range parent.children {
		if len(c.children) == 0 && c.id == closed {
			continue
		}
		kept = append(kept, leafSet(c))
	}
	q := findSplit(curRoot, parent.kind, kept)
	if q == nil {
		return Layout{}, false, nil
	}

	axis := q.kind
	total := 0
	for _, c := range q.children {
		total += *dim(c, axis)
	}
	n := len(q.children)
	share := total / n
	targets := make([]int, n)
	same := true
	for i, c := range q.children {
		targets[i] = share
		if i == n-1 {
			targets[i] = total - (n-1)*share
		}
		same = same && targets[i] == *dim(c, axis)
	}
	if same {
		return Layout{}, false, nil
	}
	for i, c := range q.children {
		if !resize(c, axis, targets[i]) {
			return Layout{}, false, nil
		}
	}
	place(q)

	out := Layout{W: curRoot.w, H: curRoot.h}
	collectLeaves(curRoot, &out.Panes)
	var sb strings.Builder
	writeCell(curRoot, &sb)
	out.Raw = fmt.Sprintf("%04x,%s", layoutChecksum(sb.String()), sb.String())
	return out, true, nil
}

// tiledTree parses s and returns its tree with float leaves pruned.
func tiledTree(s string) (*node, error) {
	root, floats, err := parseTree(s)
	if err != nil {
		return nil, err
	}
	floatIDs := make(map[string]bool, len(floats))
	for _, f := range floats {
		floatIDs[f.ID] = true
	}
	tiled := pruneFloats(root, floatIDs)
	if tiled == nil {
		return nil, fmt.Errorf("layout: no tiled panes in %q", s)
	}
	return tiled, nil
}

// closedPane returns the one leaf id of prev that cur lacks, if cur holds no
// leaf prev does not.
func closedPane(prev, cur *node) (string, bool) {
	var oldCells, newCells []PaneCell
	collectLeaves(prev, &oldCells)
	collectLeaves(cur, &newCells)
	if len(newCells) != len(oldCells)-1 {
		return "", false
	}
	inNew := make(map[string]bool, len(newCells))
	for _, c := range newCells {
		inNew[c.ID] = true
	}
	inOld := make(map[string]bool, len(oldCells))
	closed := ""
	for _, c := range oldCells {
		inOld[c.ID] = true
		if !inNew[c.ID] {
			closed = c.ID
		}
	}
	for _, c := range newCells {
		if !inOld[c.ID] {
			return "", false
		}
	}
	return closed, closed != ""
}

func parentOf(n *node, id string) *node {
	for _, c := range n.children {
		if len(c.children) == 0 && c.id == id {
			return n
		}
		if p := parentOf(c, id); p != nil {
			return p
		}
	}
	return nil
}

// leafSet returns n's leaf ids, sorted.
func leafSet(n *node) []string {
	var cells []PaneCell
	collectLeaves(n, &cells)
	ids := make([]string, len(cells))
	for i, c := range cells {
		ids[i] = c.ID
	}
	slices.Sort(ids)
	return ids
}

// findSplit returns the first split under n of the given kind whose children's
// leaf sets equal want, in order.
func findSplit(n *node, kind byte, want [][]string) *node {
	if len(n.children) == 0 {
		return nil
	}
	if n.kind == kind && len(n.children) == len(want) {
		match := true
		for i, c := range n.children {
			if !slices.Equal(leafSet(c), want[i]) {
				match = false
				break
			}
		}
		if match {
			return n
		}
	}
	for _, c := range n.children {
		if q := findSplit(c, kind, want); q != nil {
			return q
		}
	}
	return nil
}

// dim returns n's size along the axis a split of that kind lays its children
// out on: width for '{', height for '['.
func dim(n *node, axis byte) *int {
	if axis == '{' {
		return &n.w
	}
	return &n.h
}

// resize sets n's size along axis and propagates it into the subtree. A split
// across the axis gives each child the whole size; a split along it divides
// the size, less its dividers, in proportion to the children's old sizes.
// It reports false if any pane would drop below one cell.
func resize(n *node, axis byte, size int) bool {
	if size < 1 {
		return false
	}
	*dim(n, axis) = size
	if len(n.children) == 0 {
		return true
	}
	if n.kind != axis {
		for _, c := range n.children {
			if !resize(c, axis, size) {
				return false
			}
		}
		return true
	}
	m := len(n.children)
	avail := size - (m - 1)
	if avail < m {
		return false
	}
	old := make([]int, m)
	sum := 0
	for i, c := range n.children {
		old[i] = *dim(c, axis)
		sum += old[i]
	}
	if sum < 1 {
		return false
	}
	shares := make([]int, m)
	rem := make([]int, m)
	given := 0
	for i, o := range old {
		shares[i] = avail * o / sum
		rem[i] = avail * o % sum
		given += shares[i]
	}
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rem[order[a]] > rem[order[b]] })
	for _, i := range order[:avail-given] {
		shares[i]++
	}
	for i, c := range n.children {
		if !resize(c, axis, shares[i]) {
			return false
		}
	}
	return true
}

// place recomputes the offsets of every cell under n from n's own, advancing
// by a child's size plus one divider.
func place(n *node) {
	pos := n.x
	if n.kind == '[' {
		pos = n.y
	}
	for _, c := range n.children {
		if n.kind == '{' {
			c.x, c.y = pos, n.y
		} else {
			c.x, c.y = n.x, pos
		}
		pos += *dim(c, n.kind) + 1
		place(c)
	}
}
