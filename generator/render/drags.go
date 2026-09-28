package render

import (
	_ "embed"
	"strings"

	"github.com/noamsto/tmux-og/generator/paths"
)

// dragStockText is the pinned tmux's own `list-keys -T root <key>` line for
// each stock drag binding this file re-binds, verbatim. Not in stockmenus.txt: menuBinds
// panics on an entry with no mirror menu.
//
//go:embed stockdrags.txt
var dragStockText string

var dragStock = parseStockMenus(dragStockText, "stockdrags.txt")

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
	dragStartNote    = "Resize or move a pane by its border"
	dragEndNote      = "Route a mirror border drag to the remote"
	floatDragEndNote = "Restamp a hand-resized float"
)

// dragBindTable is the one-shot table a drag start switches a mirror pane into
// and floatDragTable the one a local float enters; picker/whichkey.go skips
// both by name. A local float's drag fires no hook either, so it needs the
// same drag-end table shape (#864).
const (
	dragBindTable  = "og-bridge-drag"
	floatDragTable = "og-float-drag"
)

// dragBinds routes a mirror pane's border drag to the remote (#797, #823) and
// a local float's to tmux-float-nudge (#864).
// tmux's own drag runs as a C mouse_drag_update callback that fires no hook
// or notification, so the only event that reaches a key table is the drag
// end (MouseDragEnd1<release location>). Each stock drag bind is re-bound to
// stash the drag's pane id (the drag end's own target is wherever the button
// came up) and enter dragBindTable, whose next key is always the drag end,
// since drag updates skip key tables; that key hands the id to ctl drag. A
// non-mirror float enters floatDragTable instead, whose drag end hands the
// id to the nudge script — the stock command runs unchanged in both cases.
// `resize-pane -M` (MouseDrag1Border) is the only stock drag that can
// reshape a TILED pane, so it takes the mirror branch for any mirror pane
// (bridgeGate alone). The `move-pane -M` binds keep the float-only gate:
// cmd_join_pane_mouse_update (cmd-join-pane.c) returns early for a
// non-floating pane, so they never reshape a tiled layout. Like menuBinds,
// the else-branch is the stock command as a string, parsed only when it
// runs, and the block is version-gated (#407).
func dragBinds(p *paths.Paths) string {
	floatGate := "#{&&:" + bridgeGate + ",#{pane_floating_flag}}"
	lines := []string{`%if "#{==:#{version},` + stockMenuVersion + `}"`}
	for _, s := range dragStock {
		gate := floatGate
		if s.cmd == "resize-pane -M" {
			gate = bridgeGate
		}
		mirror := "set -F @og_bridge_drag '#{pane_id}' ; " + s.cmd + " ; switch-client -T " + dragBindTable
		float := "set -F @og_float_drag '#{pane_id}' ; " + s.cmd + " ; switch-client -T " + floatDragTable
		lines = append(lines, "bind-key -N '"+dragStartNote+"' -T "+s.table+" "+s.key+
			" if-shell -F -t = '"+gate+"' { "+mirror+" } { if-shell -F -t = '#{pane_floating_flag}' { "+float+" } "+tmuxDoubleQuote(s.cmd)+" }")
	}
	body := `run-shell "` + bridgeCtl(p) + ` drag #{q:@og_bridge_drag}"`
	floatBody := `run-shell -b "` + p.Scripts["tmux-float-nudge"] + ` #{q:@og_float_drag} stamp"`
	for _, loc := range dragLocations {
		for _, mod := range dragModifiers {
			lines = append(lines, "bind-key -N '"+dragEndNote+"' -T "+dragBindTable+" "+mod+"MouseDragEnd1"+loc+" "+body)
			lines = append(lines, "bind-key -N '"+floatDragEndNote+"' -T "+floatDragTable+" "+mod+"MouseDragEnd1"+loc+" "+floatBody)
		}
	}
	return strings.Join(append(lines, "%endif"), "\n")
}
