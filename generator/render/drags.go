// Why this file exists: tmux's own float drag (root MouseDrag1Border /
// M-MouseDrag1Border) installs a C mouse_drag_update callback that fires on
// every later drag event *before* any key table is consulted, and that
// callback fires no hook, no window-layout-changed and no control
// notification. So nothing observable happens while the mirror's local float
// is being dragged. The one event that DOES reach a key table afterwards is
// the drag end, MouseDragEnd1<release-location> (server-client.c). The start
// bind stashes the float's local pane id in a session option (a mouse target
// is not format-expanded, and the release target is not the float) and
// switches the client into a one-shot table; the NEXT key the client
// produces is always that drag end, since drag updates skip key tables
// entirely, so it runs ctl float-drag on the stashed pane and resolves the
// float's geometry onto the remote (#797). Every location under every
// modifier combination is bound because check_mouse ORs the release event's
// modifiers into the key, and a modifier can be pressed or released mid-drag.
package render

import (
	_ "embed"
	"strings"

	"github.com/noamsto/tmux-og/generator/paths"
)

// dragStockText is the pinned tmux's own `list-keys -T root <key>` line for
// each stock float-drag binding, verbatim, same shape as stockmenus.txt. It
// cannot live in stockmenus.txt: menuBinds panics on any entry with no mirror
// menu, and a drag bind has no menu — its mirror is the drag-end table below.
//
//go:embed stockdrags.txt
var dragStockText string

var dragStock = parseStockMenus(dragStockText, "stockdrags.txt")

// dragBindTable is the one-shot table a drag start switches the client into.
// picker/whichkey.go skips it by this name (its 160 rows are mouse-only, not
// keys a person picks from a menu).
const dragBindTable = "og-bridge-drag"

// dragLocations is every mouse location tmux defines (KEYC_MOUSE_STRING,
// tmux.h). dragModifiers is every combination of the three modifier bits
// server_client_check_mouse ORs into a mouse key, in the canonical order tmux
// itself prints them (key-string.c): C before M before S. Together they make
// the drag-end table complete by construction: 20 locations x 8 modifier
// combinations = 160 keys.
var dragLocations = []string{
	"Pane", "Status", "StatusLeft", "StatusRight", "StatusDefault",
	"ScrollbarUp", "ScrollbarSlider", "ScrollbarDown", "Empty", "Border",
	"Control0", "Control1", "Control2", "Control3", "Control4",
	"Control5", "Control6", "Control7", "Control8", "Control9",
}

var dragModifiers = []string{"", "M-", "C-", "S-", "C-M-", "M-S-", "C-S-", "C-M-S-"}

// The notes bind-note-assertions requires of every bind.
const (
	dragStartNote = "Resize or move a pane by its border"
	dragEndNote   = "Route a mirror float border drag to the remote"
)

// dragBinds re-binds tmux's two float-drag root bindings behind bridgeGate &&
// pane_floating_flag, then a %if-gated, drag-end table that hands the drag
// off to ctl. Like menuBinds, the non-mirror branch is the stock command as a
// string, not a brace block: a string branch is parsed only when it runs, so
// a same-version server missing a command fails only that drag, never the
// config load (#407).
func dragBinds(p *paths.Paths) string {
	gate := "#{&&:" + bridgeGate + ",#{pane_floating_flag}}"
	lines := []string{`%if "#{==:#{version},` + stockMenuVersion + `}"`}
	for _, s := range dragStock {
		mirror := "set -F @og_bridge_drag '#{pane_id}' ; " + s.cmd + " ; switch-client -T " + dragBindTable
		lines = append(lines, "bind-key -N '"+dragStartNote+"' -T "+s.table+" "+s.key+" if-shell -F -t = '"+gate+"' { "+mirror+" } "+tmuxDoubleQuote(s.cmd))
	}
	body := `run-shell "` + bridgeCtl(p) + ` float-drag #{q:@og_bridge_drag}"`
	for _, loc := range dragLocations {
		for _, mod := range dragModifiers {
			lines = append(lines, "bind-key -N '"+dragEndNote+"' -T "+dragBindTable+" "+mod+"MouseDragEnd1"+loc+" "+body)
		}
	}
	return strings.Join(append(lines, "%endif"), "\n")
}
