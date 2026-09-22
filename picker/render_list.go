package main

// This file holds the list shape's renderer and its chrome (search bar,
// separator, hint line), split out of tui.go (#286).

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

func (m tuiModel) renderList() string {
	h := m.listHeight()
	w := m.listWidth()

	selBgHex := m.thmColorHex("@thm_surface_2", "#45475a", "#acb0be")
	selBg := lipgloss.Color(selBgHex)
	selStyle := lipgloss.NewStyle().
		Background(selBg)
	// ANSI reset (\033[0m) inside display strings kills the background.
	// Replace resets with "reset fg + re-apply bg" so background persists.
	selResetKeepBg := "\033[39m" + ansiBg(selBgHex) // reset fg only, re-set bg

	markGlyph := lipgloss.NewStyle().
		Foreground(m.thmColor("@thm_peach", "#fab387", "#fe640b")).
		Render("✓")

	// One pinned line carries whichever header governs the rows beneath it: the
	// column glyphs through the session list, the divider once inside Remote or
	// New session, the group row in window mode.
	bodyH, start := h, m.scrollStart(h)
	pin := -1
	if h > 1 {
		if governingHeaderIdx(m.visible, start) >= 0 {
			bodyH = h - 1
			start = m.scrollStart(bodyH)
			pin = governingHeaderIdx(m.visible, start)
			// At the top of the list the pinned row IS visible[start]; advance
			// past it rather than drawing it twice and wasting the line.
			if pin == start {
				start++
			}
		}
	}

	lines := make([]string, 0, h)
	if pin >= 0 {
		lines = append(lines, fitVisibleWidth("  "+m.renderHeaderItem(m.visible[pin], w), w))
	}
	for i := start; i < start+bodyH && i < len(m.visible); i++ {
		item := m.visible[i]
		marked := m.isMarked(item)
		switch {
		case item.headerLabel != "":
			lines = append(lines, fitVisibleWidth("  "+m.renderHeaderItem(item, w), w))
		case i == m.cursor:
			prefix := "▶ "
			if marked {
				prefix = "▶" + markGlyph
			}
			patched := strings.ReplaceAll(item.display, "\033[0m", selResetKeepBg)
			line := fitVisibleWidth(prefix+patched, w)
			lines = append(lines, selStyle.Render(line))
		case marked:
			lines = append(lines, fitVisibleWidth(" "+markGlyph+item.display, w))
		default:
			lines = append(lines, fitVisibleWidth("  "+item.display, w))
		}
	}
	empty := strings.Repeat(" ", w)
	for len(lines) < h {
		lines = append(lines, empty)
	}

	return strings.Join(lines, "\n")
}

// governingHeaderIdx returns the index of the header that labels the rows at
// start — start itself when that is the header. -1 under a query, where the
// column row is filtered out with everything else that does not match.
func governingHeaderIdx(items []listItem, start int) int {
	if start >= len(items) {
		start = len(items) - 1
	}
	for i := start; i >= 0; i-- {
		if items[i].isHeader || items[i].isColumnHeader {
			return i
		}
	}
	return -1
}

// renderHeaderItem draws a header at the real popup width. A section divider
// carries a label and a glyph rather than a finished rule because the collectors
// that build these items do not know the width — they emit 220 dashes and leave
// the clipping to the renderer.
func (m tuiModel) renderHeaderItem(item listItem, w int) string {
	if item.headerLabel == "" {
		return item.display
	}
	accent := lipgloss.NewStyle().
		Foreground(m.thmColor("@thm_lavender", "#b4befe", "#7287fd"))
	rule := lipgloss.NewStyle().
		Foreground(m.thmColor("@thm_surface_1", "#45475a", "#9ca0b0"))

	head := rule.Render("──") + " "
	if item.headerIcon != "" {
		head += accent.Render(item.headerIcon) + " "
	}
	head += accent.Render(item.headerLabel) + " "

	// -2 for the two leading spaces every list line carries.
	fill := w - 2 - visibleWidth(head)
	if fill < 1 {
		return head
	}
	return head + rule.Render(strings.Repeat("─", fill))
}

func (m tuiModel) renderSeparator() string {
	sepColor := lipgloss.NewStyle().
		Foreground(m.thmColor("@thm_surface_1", "#45475a", "#9ca0b0"))
	return sepColor.Render(strings.Repeat("─", m.innerWidth()))
}

func (m tuiModel) renderSearch() string {
	blue := lipgloss.NewStyle().Foreground(m.thmColor("@thm_blue", "#89b4fa", "#1e66f5"))
	dim := lipgloss.NewStyle().Foreground(m.thmColor("@thm_surface_2", "#585b70", "#9ca0b0"))

	icon := blue.Render("  ")
	var queryStr string
	switch {
	case m.query != "":
		queryStr = m.query + "█"
	case m.mode == modeWall && !m.querying:
		// In wall mode letters navigate the grid, not type — filtering only
		// starts by pressing /, so "type to filter..." would contradict the
		// wall's own hint line (renderWallHints).
		queryStr = dim.Render("/ to filter...") + " "
	default:
		queryStr = dim.Render("type to filter...") + " "
	}

	return lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(m.thmColor("@thm_surface_1", "#45475a", "#9ca0b0")).
		Render(m.withHostBadge(icon + queryStr))
}

// withHostBadge right-aligns the remote host on the search row, so a picker
// that looks identical to the local one still says whose sessions it lists.
// Dropped, never truncated, when the row is too narrow for both: the query
// being typed outranks it.
func (m tuiModel) withHostBadge(row string) string {
	if m.emitHost == "" {
		return row
	}
	badge := lipgloss.NewStyle().
		Foreground(m.thmColor("@thm_mauve", "#cba6f7", "#8839ef")).
		Render(" " + m.emitHost + " ")
	gap := m.width - visibleWidth(row) - visibleWidth(badge)
	if gap < 1 {
		return row
	}
	return row + strings.Repeat(" ", gap) + badge
}

func (m tuiModel) renderHints() string {
	dim := lipgloss.NewStyle().Foreground(m.thmColor("@thm_surface_2", "#585b70", "#9ca0b0"))
	key := lipgloss.NewStyle().Foreground(m.thmColor("@thm_lavender", "#b4befe", "#7287fd"))

	if m.statusMsg != "" {
		red := lipgloss.NewStyle().Foreground(m.thmColor("@thm_red", "#f38ba8", "#d20f39"))
		return fitVisibleWidth(red.Render("  "+m.statusMsg), m.width)
	}

	hint := func(k, desc string) string {
		return key.Render(k) + dim.Render(":"+desc)
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

	item, hasItem := m.currentItem()

	killLabel := "kill"
	if hasItem && item.createPath != "" {
		killLabel = "forget"
	}
	// ^x is unconfirmed here, so a row whose kill lands on another machine has
	// to say so before it is pressed.
	if hasItem && item.bridgePane != "" {
		killLabel = "kill remote"
	}

	// ^/ goes back to the wall in a wall-launched popup, and toggles the preview
	// in every other one — the label has to say which.
	toggleLabel := "preview"
	if m.wallLaunched {
		toggleLabel = "wall"
	}

	// Emit mode never switches or creates — enter only writes the pick back to
	// the wrapper (spec D8) — and inherits no session/window to kill or forget.
	enterLabel := "open"
	switch {
	case m.emitPath != "":
		enterLabel = "pick"
	case len(m.marked) > 0:
		enterLabel = "open " + strconv.Itoa(len(m.marked))
	}

	parts := []string{
		hint("^jk/↑↓", "nav"),
		hint("enter", enterLabel),
	}
	if m.emitPath == "" {
		parts = append(parts, hint("^x", killLabel))
	}
	parts = append(parts,
		hint("^a", agentLabel),
		hint("^s", scratchLabel),
	)
	if m.windowMode {
		groupLabel := "group"
		if m.stateGrouped {
			groupLabel = highlight.Render(groupLabel)
		}
		parts = append(parts, hint("^g", groupLabel))
	}
	if hasItem {
		if _, ok := remotePickHost(item, m.windowMode); ok {
			parts = append(parts, hint("^o", "browse"))
		}
	}
	if (hasItem && m.markable(item)) || len(m.marked) > 0 {
		markLabel := "mark"
		if len(m.marked) > 0 {
			markLabel = highlight.Render(markLabel)
		}
		parts = append(parts, hint("^t", markLabel))
	}
	parts = append(parts,
		hint("^/", toggleLabel),
		hint("M-hjkl", "scroll"),
		hint("q", "quit"),
	)

	// Clipped, not width-styled: a lipgloss Width() wraps this keymap onto a
	// second line at a narrow width, and bodyHeight has reserved exactly one
	// (renderWallHints clips for the same reason).
	return fitVisibleWidth("  "+strings.Join(parts, "  "), m.width)
}
