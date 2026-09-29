package daemon

import (
	"fmt"
	"strconv"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// floatBorder is the border style every mirrored float is created with.
// floatInset is the resulting cell-to-flag offset: probed against tmux
// next-3.8, `new-pane -x 60 -y 20 -X 10 -Y 5` yields cell `58x18,11,6`, and
// `resize-pane`/`move-pane` round-trip the same 1-cell inset independently.
// The inset is a property of *having* a border, not of which style — heavy,
// simple, double, and the tmux default all measured the same — and drops to 0
// only for `-B none`.
const (
	floatBorder = "heavy"
	floatInset  = 1
)

// outerFromCell converts a float's layout cell (the inner, usable pane box —
// what the renderer sees, equal to #{pane_width}x#{pane_height}) into the
// outer box tmux's -x/-y/-X/-Y flags speak, clamped to a window of size
// winW x winH.
//
// Takes the window's size rather than just the cell: a `-B none` remote float
// has a zero-inset cell, so a bare cell+2 could size or place the outer box
// past the window edge tmux itself would clamp to. The box is kept whole inside
// the window rather than each axis clamped on its own — a borderless cell flush
// against the far edge otherwise yields an offset and a width whose sum is one
// past the window. Size wins over position: the float keeps the remote's dims
// and slides back in.
func outerFromCell(c controlmode.PaneCell, winW, winH int) (w, h, x, y int) {
	w = min(c.W+floatInset*2, winW)
	h = min(c.H+floatInset*2, winH)
	x = clampOffset(c.X-floatInset, w, winW)
	y = clampOffset(c.Y-floatInset, h, winH)
	return w, h, x, y
}

// clampOffset places a box of length size inside a window of length bound: not
// past the far edge, and never negative — a cell flush against the near border
// already sits at inset 0. size is the clamped one, so the far-edge answer
// cannot itself go negative.
func clampOffset(off, size, bound int) int {
	if off+size > bound {
		off = bound - size
	}
	if off < 0 {
		off = 0
	}
	return off
}

// clampInner is c clamped into a winW x winH window by outerFromCell's rule,
// back in the inner box: the reconcile places a local float the same way.
func clampInner(c controlmode.PaneCell, winW, winH int) controlmode.PaneCell {
	ow, oh, ox, oy := outerFromCell(c, winW, winH)
	return controlmode.PaneCell{ID: c.ID, X: ox + floatInset, Y: oy + floatInset, W: ow - 2*floatInset, H: oh - 2*floatInset}
}

// floatCreateArgv returns the argv that creates a local float mirroring cell
// c: -d so a reconcile-driven add never yanks focus, -A so the float survives
// a zoom (both probed against tmux next-3.8), -P -F '#{pane_id}' so the
// caller can capture the new local pane id from LocalTmuxOut.
func floatCreateArgv(localWin string, c controlmode.PaneCell, winW, winH int) []string {
	w, h, x, y := outerFromCell(c, winW, winH)
	return []string{
		"new-pane", "-d", "-P", "-F", "#{pane_id}",
		"-t", localWin,
		"-B", floatBorder, "-A",
		"-x", strconv.Itoa(w), "-y", strconv.Itoa(h),
		"-X", strconv.Itoa(x), "-Y", strconv.Itoa(y),
	}
}

// floatResizeTrigger is true exactly when resize-pane -y adds a row to the
// height it is given: cmd-resize-pane.c bumps it under pane-border-status top
// when the pane's yoff is 1, and under bottom when its bottom edge sits one
// above the window's last row. Floats are not exempt, and top-floating and
// bottom-floating count as off. new-pane -y has no such bump.
const floatResizeTrigger = "#{||:#{&&:#{==:#{pane-border-status},top},#{==:#{pane_top},1}},#{&&:#{==:#{pane-border-status},bottom},#{==:#{e|+:#{pane_top},#{pane_height}},#{e|-:#{window_height},1}}}}"

// floatResizeCmd returns the tmux command that sizes pane's outer box to
// exactly w x h, asking for one row less where resize-pane would add one.
// resize-pane does not format-expand -y, so the compensation is an if-shell
// branch; it reads the pane's position when it runs, so it stays exact
// whether it runs before or after a move.
func floatResizeCmd(pane string, w, h int) string {
	return fmt.Sprintf("if-shell -t %s -F %s %s %s", pane, tmuxQuote(floatResizeTrigger),
		tmuxQuote(fmt.Sprintf("resize-pane -t %s -x %d -y %d", pane, w, h-1)),
		tmuxQuote(fmt.Sprintf("resize-pane -t %s -x %d -y %d", pane, w, h)))
}

// floatResizeArgv returns the argv that resizes an existing local float to
// cell c's outer box, leaving the pane alone if it is no longer floating.
func floatResizeArgv(localPane string, c controlmode.PaneCell, winW, winH int) []string {
	w, h, _, _ := outerFromCell(c, winW, winH)
	return []string{"if-shell", "-t", localPane, "-F", "#{pane_floating_flag}", floatResizeCmd(localPane, w, h)}
}

// floatMoveArgv returns the argv that moves an existing local float to cell
// c's outer offset.
func floatMoveArgv(localPane string, c controlmode.PaneCell, winW, winH int) []string {
	_, _, x, y := outerFromCell(c, winW, winH)
	return []string{"move-pane", "-t", localPane, "-X", strconv.Itoa(x), "-Y", strconv.Itoa(y)}
}

// floatGeomStamp returns the @float_geom option value for cell c: its outer
// box as "<w> <h> <x> <y>", matching scripts/tmux-float-refit.sh's read order
// (`read -r width height xoff yoff`), which feeds the four values straight
// back into resize-pane -x/-y and move-pane -X/-Y.
func floatGeomStamp(c controlmode.PaneCell, winW, winH int) string {
	w, h, x, y := outerFromCell(c, winW, winH)
	return strconv.Itoa(w) + " " + strconv.Itoa(h) + " " + strconv.Itoa(x) + " " + strconv.Itoa(y)
}
