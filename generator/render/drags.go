package render

import (
	_ "embed"
	"strings"

	"github.com/noamsto/tmux-og/generator/paths"
)

// dragStockText is the pinned tmux's own `list-keys -T root <key>` line for
// each stock float-drag binding, verbatim. Not in stockmenus.txt: menuBinds
// panics on an entry with no mirror menu.
//
//go:embed stockdrags.txt
var dragStockText string

var dragStock = parseStockMenus(dragStockText, "stockdrags.txt")

// dragBindTable is the one-shot table a drag start switches the client into;
// picker/whichkey.go skips it by name.
const dragBindTable = "og-bridge-drag"

// dragLocations is every mouse location tmux defines (KEYC_MOUSE_STRING), and
// dragModifiers every C/M/S combination server_client_check_mouse ORs into a
// mouse key from the RELEASE event, in tmux's own print order: the drag-end
// table is complete by construction.
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

// dragBinds routes a mirror float's drag to the remote (#797). tmux's float
// drag runs as a C mouse_drag_update callback that fires no hook or
// notification, so the only event that reaches a key table is the drag end
// (MouseDragEnd1<release location>). Each stock float-drag bind is re-bound
// behind bridgeGate && pane_floating_flag: it stashes the float's local pane
// id (the drag end's own target is wherever the button came up) and enters
// dragBindTable, whose next key is always the drag end, since drag updates
// skip key tables. That key hands the id to ctl float-drag. Like menuBinds,
// the else-branch is the stock command as a string, parsed only when it
// runs, and the block is version-gated (#407).
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
