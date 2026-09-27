package render

import (
	_ "embed"
	"strings"

	"github.com/noamsto/tmux-og/generator/paths"
)

// stockMenusText is the pinned tmux's own `list-keys -T <table> <key>` line for
// each default menu binding, verbatim. It is data rather than Go literals so the
// non-mirror branch is provably tmux's stock command, and a flake check diffs it
// against the pinned binary so an upstream menu change is classified before it
// ships.
//
//go:embed stockmenus.txt
var stockMenusText string

// stockMenuVersion is the #{version} stockMenusText was captured from. The menu
// block is %if-gated on it: the stock menus carry next-only commands, and a
// block tmux builds at source time would make an older resident server reject
// the whole config (#407).
const stockMenuVersion = "next-3.9"

type stockMenu struct{ table, key, cmd string }

var stockMenus = parseStockMenus(stockMenusText)

// parseStockMenus strips each line's `bind-key -T <table> <key>` prefix, column
// padding included. A malformed line panics: the file is build input.
func parseStockMenus(text string) []stockMenu {
	var out []stockMenu
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "bind-key" || f[1] != "-T" {
			panic("stockmenus.txt: malformed line: " + line)
		}
		rest := line
		for range 4 {
			rest = strings.TrimLeft(rest, " ")
			rest = rest[strings.IndexByte(rest, ' '):]
		}
		out = append(out, stockMenu{table: f[2], key: f[3], cmd: strings.TrimLeft(rest, " ")})
	}
	return out
}

// A menu item's command is format-expanded when the menu is built and parsed
// again when the item is chosen, so every run-time format in a mirror item is
// escaped once per layer above it: esc1 for display-menu's build, esc2 when a
// run-shell -C expansion sits above that too. Without it, a remote-derived
// value would be spliced into command text tmux parses again.
func esc1(s string) string { return strings.ReplaceAll(s, "#", "##") }

func esc2(s string) string { return esc1(esc1(s)) }

var doubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)

// tmuxDoubleQuote escapes tmux args_escape's set for a double-quoted word.
func tmuxDoubleQuote(s string) string { return `"` + doubleQuoteEscaper.Replace(s) + `"` }

// ctlRun is the remote branch the structural keybinds run for verb, unescaped.
func ctlRun(p *paths.Paths, verb, args string) string {
	return `run-shell "` + bridgeCtl(p) + " " + verb + " #{q:@bridge_pane}" + args + `"`
}

// renamePrompt is the `,` keybind's mirror branch.
func renamePrompt(p *paths.Paths) string {
	return `command-prompt -I'#{@window_bridge_name}' { run-shell "` + bridgeCtl(p) + ` rename #{q:@bridge_pane} #{qs:1}" %1 }`
}

func mirrorWindowMenu(p *paths.Paths, pos string) string {
	return `display-menu -T "#[align=centre]#{window_index}:#{window_name}" ` + pos +
		` "#{?#{>:#{session_windows},1},,-}Swap Left" l { swap-window -t :-1 }` +
		` "#{?#{>:#{session_windows},1},,-}Swap Right" r { swap-window -t :+1 }` +
		` '' Kill X { ` + esc1(ctlRun(p, "kill-window", "")) + ` }` +
		` Respawn R { ` + esc1(ctlRun(p, "respawn-window", "")) + ` }` +
		` Rename n { ` + esc1(renamePrompt(p)) + ` }` +
		` '' "New Window" w { ` + esc1(ctlRun(p, "new-window", "")) + ` }`
}

// paneMenuLocalItems is the stock pane menu's copy-mode, paste and mouse-word
// span, which only touches the renderer pane and so stays verbatim on a mirror.
const paneMenuLocalItems = `"#{?#{m/r:(copy|view)-mode,#{pane_mode}},Go To Top,}" < { send-keys -X history-top }` +
	` "#{?#{m/r:(copy|view)-mode,#{pane_mode}},Go To Bottom,}" > { send-keys -X history-bottom } ''` +
	` "#{?#{==:#{pane_mode},copy-mode},#{?copy_line_numbers,Hide Line Numbers,Show Line Numbers},}" L { send-keys -X line-numbers-toggle }` +
	` "#{?#{==:#{pane_mode},copy-mode},#{?refresh_active,Refresh Off,Refresh On},}" r { send-keys -X refresh-toggle } ''` +
	` "#{?#{&&:#{buffer_size},#{!:#{pane_in_mode}}},Paste #[underscore]#{=/9/...:buffer_sample},}" p { paste-buffer } ''` +
	` "#{?mouse_word,Search For #[underscore]#{=/9/...:mouse_word},}" C-r { if-shell -F "#{?#{m/r:(copy|view)-mode,#{pane_mode}},0,1}" "copy-mode -t=" ; send-keys -X -t = search-backward -- "#{q:mouse_word}" }` +
	` "#{?mouse_word,Type #[underscore]#{=/9/...:mouse_word},}" C-y { copy-mode -q ; send-keys -l "#{q:mouse_word}" }` +
	` "#{?mouse_word,Copy #[underscore]#{=/9/...:mouse_word},}" c { copy-mode -q ; set-buffer "#{q:mouse_word}" }` +
	` "#{?mouse_line,Copy Line,}" l { copy-mode -q ; set-buffer "#{q:mouse_line}" } ''` +
	` "#{?mouse_hyperlink,Type #[underscore]#{=/9/...:mouse_hyperlink},}" C-h { copy-mode -q ; send-keys -l "#{q:mouse_hyperlink}" }` +
	` "#{?mouse_hyperlink,Copy #[underscore]#{=/9/...:mouse_hyperlink},}" h { copy-mode -q ; set-buffer "#{q:mouse_hyperlink}" } ''`

// mirrorPaneMenu keeps two distinct gestures: Respawn asks the REMOTE to
// restart the program in the pane (stock label and key), and Reconnect redials
// the local renderer (#547) — the un-wedge gesture, moved off R because one
// menu cannot carry two items on one key.
func mirrorPaneMenu(p *paths.Paths, pos string) string {
	return `display-menu -T "#[align=centre]#{pane_index} (#{pane_id})" ` + pos + " " + paneMenuLocalItems +
		` "#{?#{!:#{pane_floating_flag}},Horizontal Split,}" h { ` + esc1(ctlRun(p, "split-h", "")) + ` }` +
		` "#{?#{!:#{pane_floating_flag}},Vertical Split,}" v { ` + esc1(ctlRun(p, "split-v", "")) + ` }` +
		` '' "#{?#{&&:#{!:#{pane_floating_flag}},#{>:#{window_panes},1}},Swap Up,}" u { ` + esc1(ctlRun(p, "swap", " U")) + ` }` +
		` "#{?#{&&:#{!:#{pane_floating_flag}},#{>:#{window_panes},1}},Swap Down,}" d { ` + esc1(ctlRun(p, "swap", " D")) + ` }` +
		` '' Kill X { ` + esc1(ctlRun(p, "kill-pane", "")) + ` }` +
		` Respawn R { ` + esc1(ctlRun(p, "respawn-pane", "")) + ` }` +
		` Reconnect e { respawn-pane -k }` +
		` "#{?#{>:#{window_panes},1},,-}#{?window_zoomed_flag,Unzoom,Zoom}" z { ` + esc1(ctlRun(p, "zoom", "")) + ` }`
}

// mirrorSessionMenu keeps the stock run-shell -C shape, which the Switch-To
// loop's #{S:} needs; that expansion is the second layer esc2 accounts for.
// The title and the Switch-To loop also keep stock's bare #{session_name}
// expansion under -C, a pre-existing tmux re-parse exposure (see
// docs/agents/bridge-daemon.md's Expansion layers section).
func mirrorSessionMenu(p *paths.Paths) string {
	menu := `display-menu -t= -xM -yW -T '#[align=centre]#{session_name}'  #{S/t:#{?#{&&:#{<:#{loop_index},6},#{!:#{session_active}}},'Switch To #[underscore]#{session_name}' '' {switch-client -t=#{session_id}#} ,}}` +
		` '' 'Renumber' 'N' {move-window -r}` +
		` 'Detach' 'd' {` + esc2(`run-shell -b '`+p.Scripts["og-remote-detach"]+` #{qs:session_name}'`) + `}` +
		` '' 'New Session' 's' {new-session}` +
		` 'New Window' 'w' {` + esc2(`run-shell '`+bridgeCtl(p)+` new-window #{q:@bridge_pane}'`) + `}`
	return "run-shell -C " + tmuxDoubleQuote(menu)
}

func mirrorEmptyMenu(p *paths.Paths) string {
	return `display-menu -T "#[align=centre]#{window_index}:#{window_name}" -t = -x M -y M "New Window" w { ` +
		esc1(ctlRun(p, "new-window", "")) + ` }`
}

// menuBinds re-binds tmux's default menus behind bridgeGate. The non-mirror
// branch is the stock command as a string, not a brace block: a string branch
// is parsed only when it runs, so a same-version server lacking one of its
// commands fails that menu rather than the config load.
func menuBinds(p *paths.Paths) string {
	paneM := mirrorPaneMenu(p, "-t = -x M -y M")
	mirrors := map[string]struct{ note, mirror string }{
		"prefix <":                    {"Display window menu", mirrorWindowMenu(p, "-x W -y W")},
		"prefix >":                    {"Display pane menu", mirrorPaneMenu(p, "-x P -y P")},
		"root MouseDown3Pane":         {"Display pane menu", `if-shell -F -t = "#{||:#{mouse_any_flag},#{&&:#{pane_in_mode},#{?#{m/r:(copy|view)-mode,#{pane_mode}},0,1}}}" { select-pane -t = ; send-keys -M } { ` + paneM + ` }`},
		"root M-MouseDown3Pane":       {"Display pane menu", paneM},
		"root MouseDown3Status":       {"Display window menu", mirrorWindowMenu(p, "-t = -x W -y W")},
		"root M-MouseDown3Status":     {"Display window menu", mirrorWindowMenu(p, "-t = -x W -y W")},
		"root MouseDown3StatusLeft":   {"Display session menu", mirrorSessionMenu(p)},
		"root M-MouseDown3StatusLeft": {"Display session menu", mirrorSessionMenu(p)},
		"root MouseDown3Empty":        {"Display new pane/window menu", mirrorEmptyMenu(p)},
		"root M-MouseDown3Empty":      {"Display new pane/window menu", mirrorEmptyMenu(p)},
	}
	lines := []string{`%if "#{==:#{version},` + stockMenuVersion + `}"`}
	for _, s := range stockMenus {
		m, ok := mirrors[s.table+" "+s.key]
		if !ok {
			panic("stockmenus.txt: no mirror menu for " + s.table + " " + s.key)
		}
		var b strings.Builder
		b.WriteString("bind-key ")
		if m.note != "" {
			b.WriteString("-N '" + m.note + "' ")
		}
		b.WriteString("-T " + s.table + " " + s.key + " if-shell -F ")
		// Mouse bindings gate on the event's target, not the current pane.
		if s.table == "root" {
			b.WriteString("-t = ")
		}
		b.WriteString("'" + bridgeGate + "' { " + m.mirror + " } " + tmuxDoubleQuote(s.cmd))
		lines = append(lines, b.String())
	}
	return strings.Join(append(lines, "%endif"), "\n")
}
