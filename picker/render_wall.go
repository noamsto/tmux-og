package main

// This file holds the wall shape's renderer: the tile grid geometry and its
// chrome (bordered live pane captures, hint line), the third of the picker's
// three renderers (#286).

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// A tile needs this much interior to be worth drawing at all — narrower and a
// capture is unreadable, shorter and it shows no context. The minimum only
// derives the grid; the tiles that result then expand to fill the popup.
const (
	wallMinTileInnerW = 34
	wallMinTileInnerH = 8
	wallTileFrame     = 2 // border cells per axis
	wallMaxCols       = 4
	wallMaxRows       = 3
)

// wallGeometry is the tile grid the popup affords for a wall of tiles tiles. 0
// on either axis means the terminal cannot tile at all and renderWall falls back
// to the list.
//
// tiles is the *total* count, not the current page's: deriving the grid from the
// page would resize every tile as the wall pages, which is worse than a last
// page carrying blanks.
func wallGeometry(width, bodyHeight, tiles int) (cols, rows int) {
	cols = min(max(width/(wallMinTileInnerW+wallTileFrame), 0), wallMaxCols)
	rows = min(max(bodyHeight/(wallMinTileInnerH+wallTileFrame), 0), wallMaxRows)
	if cols == 0 || rows == 0 || tiles == 0 {
		return cols, rows
	}
	// Fewer tiles than cells left whole columns and rows blank while the tiles
	// they starved stayed at minimum size, so the grid is clamped to the count.
	cols = min(cols, tiles)
	rows = min(rows, (tiles+cols-1)/cols)
	return cols, rows
}

// wallTileSizes gives the outer size of every column and row. The remainder of
// the division goes to the leading columns/rows, so the grid covers the popup
// exactly rather than leaving a fixed-minimum grid's slack unused.
func wallTileSizes(width, bodyHeight, tiles int) (colWidths, rowHeights []int) {
	cols, rows := wallGeometry(width, bodyHeight, tiles)
	if cols == 0 || rows == 0 {
		return nil, nil
	}
	return shareEvenly(width, cols), shareEvenly(bodyHeight, rows)
}

func shareEvenly(total, n int) []int {
	out := make([]int, n)
	base, rem := total/n, total%n
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

func (m tuiModel) renderWall() string {
	// A focused tile (tab) takes over the whole body instead of keeping its
	// grid-cell size, so its capture is readable rather than cropped to the
	// size a shared grid cell affords.
	if m.focused {
		if item, ok := m.currentItem(); ok {
			return strings.Join(m.renderTile(item, true, true, m.width, m.bodyHeight()), "\n")
		}
	}

	tiles := m.tileItems()
	colWidths, rowHeights := wallTileSizes(m.width, m.bodyHeight(), len(tiles))
	if colWidths == nil {
		// Nothing renders a preview under the fallback, so the list takes the whole
		// body — listHeight() would otherwise leave the preview's share of it blank.
		m.showPreview = false
		return m.renderList()
	}
	start := m.wallPage * len(colWidths) * len(rowHeights)

	var lines []string
	for r, outerH := range rowHeights {
		gridRow := make([]string, outerH)
		for c, outerW := range colWidths {
			var tile []string
			if i := start + r*len(colWidths) + c; i < len(tiles) {
				idx := tiles[i]
				selected := idx == m.cursor
				tile = m.renderTile(m.visible[idx], selected, selected && m.focused, outerW, outerH)
			} else {
				tile = blankRows(outerW, outerH)
			}
			for i := range gridRow {
				gridRow[i] += tile[i]
			}
		}
		lines = append(lines, gridRow...)
	}
	return strings.Join(lines, "\n")
}

// renderTile draws one bordered pane capture as outerH lines of exactly outerW
// visible cells.
//
// The box glyphs are composed by hand rather than with a lipgloss border for two
// reasons: fitVisibleWidth ends every line with a reset, which would kill the
// background of a block-level lipgloss style (renderList hand-patches resets for
// the same reason), and lipgloss measures with uniseg while this picker measures
// nerd-font PUA itself — the two disagree on real agent output, which would
// leave the right border ragged.
//
// focused implies selected (only the cursor's tile can be focused) and gets
// its own border color, distinct from plain selection.
func (m tuiModel) renderTile(item listItem, selected, focused bool, outerW, outerH int) []string {
	innerW, innerH := max(outerW-wallTileFrame, 0), max(outerH-wallTileFrame, 0)
	border := ansiFg(m.thmColorHex("@thm_surface_1", "#45475a", "#9ca0b0"))
	switch {
	case focused:
		border = ansiFg(m.thmColorHex("@thm_peach", "#fab387", "#fe640b"))
	case selected:
		border = ansiFg(m.thmColorHex("@thm_mauve", "#cba6f7", "#8839ef"))
	}
	reset := "\033[0m"
	edge := border + "│" + reset

	rows := make([]string, 0, outerH)
	// Wrapped to outerW, not just innerW: tileLabel's ellipsis floor can exceed
	// innerW at small sizes, and below wallTileFrame the two edge glyphs alone
	// already exceed outerW — this is what keeps every row at exactly outerW
	// regardless of what the caller passes.
	rows = append(rows, fitVisibleWidth(border+"┌"+m.tileLabel(item, innerW)+"┐"+reset, outerW))
	for _, line := range m.tileBody(item, innerW, innerH) {
		// fitVisibleWidth's own trailing reset closes the pane's colors; the
		// border color has to be re-asserted after it for the right-edge glyph.
		rows = append(rows, fitVisibleWidth(edge+fitVisibleWidth(line, innerW)+edge, outerW))
	}
	rows = append(rows, fitVisibleWidth(border+"└"+strings.Repeat("─", innerW)+"┘"+reset, outerW))
	return rows
}

// tileLabel is the identity the top border carries, cropped to the tile.
func (m tuiModel) tileLabel(item listItem, innerW int) string {
	label := truncateCells(stripTreePrefix(item.plain), max(innerW-4, 0))
	if label == "" {
		return strings.Repeat("─", innerW)
	}
	mid := "─ " + label + " "
	return mid + strings.Repeat("─", max(innerW-visibleWidth(mid), 0))
}

// stripTreePrefix drops the list's ├─/╰─ session grouping, which a tile hangs off
// nothing to need. The ▸ marker and the leading window index stay: they are what
// tell two tiles on one branch apart.
func stripTreePrefix(s string) string {
	return strings.TrimLeft(strings.TrimSpace(s), "├╰─ ")
}

// tileBody is the innerH content lines: the tail of the target's capture, or a
// placeholder while there is none.
func (m tuiModel) tileBody(item listItem, innerW, innerH int) []string {
	content, ok := m.wallContent[item.target]
	if bad := m.wallBad[item.target]; bad || !ok {
		note := "…"
		if bad {
			note = "(gone)"
		}
		dim := ansiFg(m.thmColorHex("@thm_overlay_1", "#7f849c", "#8c8fa1"))
		lines := make([]string, innerH)
		lines[0] = dim + truncateCells(note, innerW) + "\033[0m"
		return lines
	}
	lines := strings.Split(content, "\n")
	if len(lines) > innerH {
		lines = lines[len(lines)-innerH:]
	}
	for len(lines) < innerH {
		lines = append(lines, "")
	}
	return lines
}

func blankRows(width, height int) []string {
	rows := make([]string, height)
	blank := strings.Repeat(" ", width)
	for i := range rows {
		rows[i] = blank
	}
	return rows
}

// renderWallHints mirrors renderHints' shaping for the wall's own keymap.
func (m tuiModel) renderWallHints() string {
	dim := lipgloss.NewStyle().Foreground(m.thmColor("@thm_surface_2", "#585b70", "#9ca0b0"))
	key := lipgloss.NewStyle().Foreground(m.thmColor("@thm_lavender", "#b4befe", "#7287fd"))

	if len(m.killConfirm) > 0 {
		return m.renderKillConfirm()
	}

	if m.statusMsg != "" {
		red := lipgloss.NewStyle().Foreground(m.thmColor("@thm_red", "#f38ba8", "#d20f39"))
		return fitVisibleWidth(red.Render("  "+m.statusMsg), m.width)
	}

	hint := func(k, desc string) string {
		return key.Render(k) + dim.Render(":"+desc)
	}

	// Below one tile's minimum renderWall draws the list instead. Saying so beats
	// a hint line promising tiles next to a body that has none.
	if cols, rows := wallGeometry(m.width, m.bodyHeight(), len(m.tileItems())); cols == 0 || rows == 0 {
		note := fmt.Sprintf("terminal too small to tile (needs %d×%d) — showing the list",
			wallMinTileInnerW+wallTileFrame, wallMinTileInnerH+wallTileFrame)
		return fitVisibleWidth("  "+dim.Render(note)+"  "+hint("^/", "list")+"  "+hint("q", "quit"), m.width)
	}

	// Focused takes over the whole bar — the hint's job here is naming what is
	// about to eat the next keystroke, not advertising bindings that a relayed
	// printable key would swallow before they ever ran.
	if m.focused {
		label := "…"
		if item, ok := m.currentItem(); ok {
			label = stripTreePrefix(item.plain)
		}
		return fitVisibleWidth("  "+dim.Render("focused: ")+key.Render(label)+
			dim.Render(" — typing relays to the pane")+"  "+hint("esc", "unfocus"), m.width)
	}

	highlight := lipgloss.NewStyle().Foreground(m.thmColor("@thm_peach", "#fab387", "#fe640b"))
	agentLabel := "agents"
	if m.agentOnly {
		agentLabel = highlight.Render(agentLabel)
	}
	scratchLabel := "scratch"
	if m.scratchOnly {
		scratchLabel = highlight.Render(scratchLabel)
	}

	// While the prompt is open every printable types, so the two hints that would
	// otherwise lie about q and / say what actually applies.
	filterHint, quitHint := hint("/", "filter"), hint("q", "quit")
	if m.querying {
		filterHint, quitHint = hint("esc", "done"), hint("^c", "quit")
	}

	parts := []string{
		hint("hjkl", "tiles"),
		hint("[]", "page"),
		hint("enter", "open"),
		hint("tab", "focus"),
		hint("^x", "kill"),
		filterHint,
		hint("^a", agentLabel),
		hint("^s", scratchLabel),
		hint("^/", "list"),
		quitHint,
	}
	if item, ok := m.currentItem(); ok {
		if _, live := remotePickHost(item, true); live {
			parts = append(parts, hint("^o", "browse"))
		}
	}
	if pages := m.wallPageCount(); pages > 1 {
		parts = append(parts, dim.Render(fmt.Sprintf("page %d/%d", m.wallPage+1, pages)))
	}

	// Clipped, not width-styled: the wall's keymap is long enough that a
	// lipgloss Width would wrap it onto a second line, and bodyHeight has
	// reserved exactly one.
	return fitVisibleWidth("  "+strings.Join(parts, "  "), m.width)
}
