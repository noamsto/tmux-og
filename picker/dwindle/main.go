// Command tmux-dwindle evens out the split group a closed pane left behind.
// The window-layout-changed hook forks it as `balance <@window> <cache>` only
// when a tiled pane has just left; cache is the hook's "SIG|LAYOUT" record, so
// everything after the first "|" is the layout from before the close. It reads
// the window once, asks controlmode.RebalanceClosedCell for the evened layout
// and applies it with one guarded select-layout. A hook job's output is
// discarded, so every runtime no-op exits 0 in silence; only a bad invocation
// exits 2.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

const usage = "usage: tmux-dwindle balance <@window> <cache>"

// Pieces shared by the state read and the apply guard, so the guard compares
// exactly what was read.
const (
	sizeExpr = "#{window_width}x#{window_height}"
	// #{P/i:} walks the pane list, the order select-layout assigns a v1
	// layout's leaves in; a bare #{P:} sorts by pane id instead.
	panesExpr = "#{P/i:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}"

	stateFormat = "#{@dwindle}|#{window_zoomed_flag}|#{@crew_grid}|#{@bridge_win}|" +
		sizeExpr + "|" + panesExpr + "|#{window_layout}"
)

const stateFields = 7

var windowRe = regexp.MustCompile(`^@[0-9]+$`)

type runner func(args ...string) (string, error)

func main() {
	os.Exit(run(os.Args[1:], execTmux))
}

func execTmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output() //nolint:gosec // argv is built here and exec'd directly, no shell
	return strings.TrimSpace(string(out)), err
}

func run(args []string, tmux runner) int {
	if len(args) != 3 || args[0] != "balance" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	win, cache := args[1], args[2]
	if !windowRe.MatchString(win) {
		return 0
	}
	_, prevLayout, ok := strings.Cut(cache, "|")
	if !ok || prevLayout == "" {
		return 0
	}

	out, err := tmux("display-message", "-p", "-t", win, stateFormat)
	if err != nil {
		return 0
	}
	f := strings.SplitN(out, "|", stateFields)
	if len(f) != stateFields {
		return 0
	}
	dwindle, zoomed, grid, bridge, size, panes, curLayout := f[0], f[1], f[2], f[3], f[4], f[5], f[6]
	if dwindle != "on" || zoomed != "0" || grid == "1" || bridge != "" {
		return 0
	}

	layout, changed, err := controlmode.RebalanceClosedCell(prevLayout, curLayout)
	if err != nil || !changed {
		return 0
	}

	// A v1 select-layout assigns its leaves to panes positionally, so the
	// pane list must already be in the layout's leaf order.
	if !sameOrder(panes, layout.Panes) {
		return 0
	}
	// The literal sits in a format's comma-separated operand.
	if strings.ContainsAny(panes, ",}") {
		return 0
	}

	// The view above may be stale by the time this runs; the guard re-reads
	// the same facts in the server and turns a stale apply into a no-op.
	guard := "#{&&:#{==:#{@dwindle},on},#{&&:#{==:#{window_zoomed_flag},0},#{&&:#{!=:#{@crew_grid},1},#{&&:#{!:#{@bridge_win}}," +
		"#{==:" + sizeExpr + "|" + panesExpr + "," + size + "|" + panes + "}}}}}"
	cmd := fmt.Sprintf("select-layout -t %s '%s'", win, layout.Raw)
	_, _ = tmux("if-shell", "-t", win, "-F", guard, cmd)
	return 0
}

// sameOrder reports whether the "%N.l.t.w.h " tokens in panes name the same
// panes, in order, as cells.
func sameOrder(panes string, cells []controlmode.PaneCell) bool {
	tokens := strings.Fields(panes)
	if len(tokens) != len(cells) {
		return false
	}
	for i, tok := range tokens {
		id, _, _ := strings.Cut(tok, ".")
		if id != cells[i].ID {
			return false
		}
	}
	return true
}
