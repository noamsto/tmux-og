package main

import (
	"strings"
	"testing"
)

func TestRenderHintsRemoteRow(t *testing.T) {
	m := tuiModel{width: 200, visible: []listItem{{remoteHost: "tp-g6"}}, cursor: 0}
	hints := stripANSI(m.renderHints())
	if !strings.Contains(hints, "^o:browse") {
		t.Errorf("hints = %q, want ^o:browse on a remote row", hints)
	}
}

func TestRenderHintsNonRemoteRow(t *testing.T) {
	m := tuiModel{width: 200, visible: []listItem{{target: "tmux-og"}}, cursor: 0}
	hints := stripANSI(m.renderHints())
	if strings.Contains(hints, "^o") {
		t.Errorf("hints = %q, must not advertise ^o off a remote row", hints)
	}
}

func TestRenderHintsMirrorWindowRow(t *testing.T) {
	m := tuiModel{
		windowMode: true,
		width:      200,
		visible:    []listItem{{target: "g6-main:1", bridgeHost: "tp-g6"}},
		cursor:     0,
	}
	hints := stripANSI(m.renderHints())
	if !strings.Contains(hints, "^o:browse") {
		t.Errorf("hints = %q, want ^o:browse on a mirror window row", hints)
	}
}

func TestRenderHintsEmitMode(t *testing.T) {
	m := tuiModel{width: 200, emitPath: "/tmp/emit", visible: []listItem{{target: "tmux-og"}}, cursor: 0}
	hints := stripANSI(m.renderHints())
	if !strings.Contains(hints, "enter:pick") {
		t.Errorf("hints = %q, want enter:pick in emit mode", hints)
	}
	if strings.Contains(hints, "^x") {
		t.Errorf("hints = %q, must not advertise ^x in emit mode", hints)
	}
}

func TestRenderHintsNonEmitMode(t *testing.T) {
	m := tuiModel{width: 200, visible: []listItem{{target: "tmux-og"}}, cursor: 0}
	hints := stripANSI(m.renderHints())
	if !strings.Contains(hints, "enter:open") {
		t.Errorf("hints = %q, want enter:open outside emit mode", hints)
	}
	if !strings.Contains(hints, "^x:kill") {
		t.Errorf("hints = %q, want ^x:kill outside emit mode", hints)
	}
}

func TestWithHostBadge(t *testing.T) {
	m := tuiModel{width: 40, emitHost: "tp-g6"}
	row := stripANSI(m.withHostBadge("  q"))
	if !strings.HasSuffix(row, "tp-g6 ") {
		t.Errorf("row = %q, want the host badge right-aligned", row)
	}
	if got := visibleWidth(row); got != 40 {
		t.Errorf("visibleWidth = %d, want 40", got)
	}
}

func TestWithHostBadgeDroppedWhenNarrow(t *testing.T) {
	m := tuiModel{width: 8, emitHost: "tp-g6"}
	if got := m.withHostBadge("  query"); got != "  query" {
		t.Errorf("row = %q, want the badge dropped rather than overflowing", got)
	}
}

func TestWithHostBadgeAbsentLocally(t *testing.T) {
	m := tuiModel{width: 40}
	if got := m.withHostBadge("  q"); got != "  q" {
		t.Errorf("row = %q, want no badge without an emit host", got)
	}
}

func TestWithScopeBadgeAbsentLocally(t *testing.T) {
	m := tuiModel{width: 40}
	if got := m.withScopeBadge("  q"); got != "  q" {
		t.Errorf("row = %q, want no badge in scopeLocal", got)
	}
}

func TestWithScopeBadgeHost(t *testing.T) {
	m := tuiModel{width: 40, scope: hostScope{kind: scopeHost, host: "tp-g6"}}
	row := stripANSI(m.withScopeBadge("  q"))
	if !strings.HasSuffix(row, "tp-g6 ") {
		t.Errorf("row = %q, want the host name right-aligned", row)
	}
	if got := visibleWidth(row); got != 40 {
		t.Errorf("visibleWidth = %d, want 40", got)
	}
}

func TestWithScopeBadgeAll(t *testing.T) {
	m := tuiModel{width: 40, scope: hostScope{kind: scopeAll}}
	row := stripANSI(m.withScopeBadge("  q"))
	if !strings.HasSuffix(row, "all hosts ") {
		t.Errorf("row = %q, want the all-hosts label right-aligned", row)
	}
}

func TestWithScopeBadgeDroppedWhenNarrow(t *testing.T) {
	m := tuiModel{width: 8, scope: hostScope{kind: scopeHost, host: "tp-g6"}}
	if got := m.withScopeBadge("  query"); got != "  query" {
		t.Errorf("row = %q, want the badge dropped rather than overflowing", got)
	}
}

func TestRenderHintsScopeHintGatedOnConfiguredHosts(t *testing.T) {
	m := tuiModel{width: 200, visible: []listItem{{target: "tmux-og"}}, cursor: 0}
	hints := stripANSI(m.renderHints())
	if strings.Contains(hints, "scope") {
		t.Errorf("hints = %q, must not advertise scope with no configured hosts", hints)
	}
}

func TestRenderHintsScopeHintHighlightedWhenActive(t *testing.T) {
	m := tuiModel{
		width:    200,
		visible:  []listItem{{target: "tmux-og"}},
		cursor:   0,
		tmuxOpts: map[string]string{"@remote_bridge_hosts": "tp-g6"},
		scope:    hostScope{kind: scopeHost, host: "tp-g6"},
	}
	hints := m.renderHints()
	if !strings.Contains(hints, "⇥") {
		t.Errorf("hints = %q, want the scope hint advertised with hosts configured", hints)
	}
}

func TestHintsNameTheRemoteKill(t *testing.T) {
	// ^x on a mirror row is unconfirmed and lands on another machine, so the
	// footer has to say so while the row is merely selected (#393).
	mirror := listItem{target: "s:1", bridgePane: "%7", bridgeSock: "/tmp/b.sock"}
	local := listItem{target: "s:2"}

	m := tuiModel{windowMode: true, width: 200, visible: []listItem{mirror, local}}
	if got := stripANSI(m.renderHints()); !strings.Contains(got, "^x:kill remote") {
		t.Errorf("hints on a mirror row = %q, want ^x:kill remote", got)
	}
	m.cursor = 1
	if got := stripANSI(m.renderHints()); !strings.Contains(got, "^x:kill") || strings.Contains(got, "kill remote") {
		t.Errorf("hints on a local row = %q, want a plain ^x:kill", got)
	}
}

// stickyItems is a session-mode list: column header, sessions, Remote section,
// New session section — the shape renderList's pinned line has to cope with.
func stickyItems() []listItem {
	items := []listItem{{display: "COLHDR", plain: "COLHDR", isColumnHeader: true}}
	for _, n := range []string{"tmux-og", "tp-g6-money", "nix-config", "aeye", "agent-smith"} {
		items = append(items, listItem{target: n, display: n, plain: n, session: n})
	}
	items = append(items, listItem{
		isHeader: true, isRemoteHeader: true,
		headerLabel: "Remote", headerIcon: "H",
		display: "── Remote ──", plain: "── Remote ──",
	})
	for _, n := range []string{"tp-g6", "work", "spare"} {
		items = append(items, listItem{target: "remote:" + n, display: n, plain: n, isRemoteRow: true, remoteHost: "tp-g6"})
	}
	items = append(items, listItem{
		isHeader: true, isZoxideHeader: true,
		headerLabel: "New session", headerIcon: "D",
		display: "── New session ──", plain: "── New session ──",
	})
	for _, n := range []string{"~/src/foo", "~/src/bar"} {
		items = append(items, listItem{createPath: n, display: n, plain: n})
	}
	return items
}

func TestGoverningHeaderIdx(t *testing.T) {
	items := stickyItems()
	cases := map[int]int{
		0:  0, // the column header labels itself
		3:  0, // a session row is labelled by the column header
		6:  6, // the Remote divider labels itself
		8:  6, // a remote row is labelled by the Remote divider
		10: 10,
		12: 10,
	}
	for start, want := range cases {
		if got := governingHeaderIdx(items, start); got != want {
			t.Errorf("governingHeaderIdx(start=%d) = %d, want %d", start, got, want)
		}
	}
	// Past the end clamps rather than panicking.
	if got := governingHeaderIdx(items, len(items)+5); got != 10 {
		t.Errorf("out-of-range start gave %d, want 10", got)
	}
	// Under a query nothing above matches: no header to pin.
	if got := governingHeaderIdx([]listItem{{target: "a"}, {target: "b"}}, 1); got != -1 {
		t.Errorf("headerless list gave %d, want -1", got)
	}
}

func renderedLines(m tuiModel) []string {
	out := strings.Split(m.renderList(), "\n")
	for i := range out {
		out[i] = strings.TrimRight(stripANSI(out[i]), " ")
	}
	return out
}

func TestRenderListPinsColumnHeaderWithoutRepeatingIt(t *testing.T) {
	m := tuiModel{width: 46, height: 12, visible: stickyItems(), cursor: 1}
	lines := renderedLines(m)
	if !strings.Contains(lines[0], "COLHDR") {
		t.Fatalf("line 0 = %q, want the pinned column header", lines[0])
	}
	// Pinned and also drawn inline would waste one of very few rows.
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "COLHDR") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("column header appears %d times, want 1:\n%s", n, strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], "tmux-og") {
		t.Errorf("line 1 = %q, want the first session row", lines[1])
	}
}

func TestRenderListPinsTheSectionYouScrolledInto(t *testing.T) {
	items := stickyItems()
	m := tuiModel{width: 46, height: 12, visible: items, cursor: len(items) - 1}
	lines := renderedLines(m)
	if !strings.Contains(lines[0], "Remote") {
		t.Fatalf("line 0 = %q, want the Remote divider pinned once scrolled past it", lines[0])
	}
	if strings.Contains(lines[0], "COLHDR") {
		t.Errorf("column header must not stay pinned below its own section")
	}
}

func TestRenderListHeightIsConstantAcrossScroll(t *testing.T) {
	items := stickyItems()
	want := -1
	for cursor := range items {
		m := tuiModel{width: 46, height: 12, visible: items, cursor: cursor}
		got := len(strings.Split(m.renderList(), "\n"))
		if want == -1 {
			want = got
		}
		if got != want {
			t.Fatalf("cursor=%d rendered %d lines, want %d — the list must not shift height as it scrolls", cursor, got, want)
		}
	}
}

func TestRenderListKeepsCursorRowVisible(t *testing.T) {
	items := stickyItems()
	for cursor := range items {
		if items[cursor].isHeader || items[cursor].isColumnHeader {
			continue
		}
		m := tuiModel{width: 46, height: 12, visible: items, cursor: cursor}
		if !strings.Contains(m.renderList(), "▶ ") {
			t.Errorf("cursor=%d (%s) is off-screen", cursor, items[cursor].plain)
		}
	}
}

func TestRenderHeaderItemFillsExactWidth(t *testing.T) {
	m := tuiModel{width: 46, height: 12}
	item := listItem{isHeader: true, headerLabel: "Remote", headerIcon: "H", display: "unused"}
	line := m.renderHeaderItem(item, 46)
	// The two leading spaces every list line carries are added by renderList.
	if got := visibleWidth(line); got != 44 {
		t.Errorf("header width = %d, want 44 (46 minus the row indent)", got)
	}
	plain := stripANSI(line)
	if !strings.Contains(plain, "Remote") || !strings.Contains(plain, "H") {
		t.Errorf("header = %q, want the label and its glyph", plain)
	}
	if !strings.HasSuffix(plain, "─") {
		t.Errorf("header = %q, want the rule running to the edge", plain)
	}
}

func TestRenderHeaderItemNarrowDoesNotOverflow(t *testing.T) {
	m := tuiModel{width: 8, height: 12}
	item := listItem{isHeader: true, headerLabel: "New session", headerIcon: "D"}
	if got := visibleWidth(m.renderHeaderItem(item, 8)); got > 20 {
		t.Errorf("narrow header width = %d; renderList clips, but it must not build a rule from a negative fill", got)
	}
}

func TestRenderHeaderItemPassesThroughNonSectionHeaders(t *testing.T) {
	m := tuiModel{width: 46, height: 12}
	item := listItem{isColumnHeader: true, display: "COLHDR-DISPLAY"}
	if got := m.renderHeaderItem(item, 46); got != "COLHDR-DISPLAY" {
		t.Errorf("got %q, want the column header's own display untouched", got)
	}
}

// In wall mode letters navigate the grid rather than type, so the empty-query
// placeholder must say "/ to filter..." instead of the list's "type to
// filter..." — unless the filter prompt is already open, where it types like
// everywhere else.
func TestRenderSearchPlaceholder(t *testing.T) {
	cases := []struct {
		name     string
		mode     pickerMode
		querying bool
		want     string
		notWant  string
	}{
		{"list mode", modeList, false, "type to filter...", ""},
		{"wall mode, not querying", modeWall, false, "/ to filter...", "type to filter..."},
		{"wall mode, querying", modeWall, true, "type to filter...", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := tuiModel{width: 60, mode: c.mode, querying: c.querying}
			search := stripANSI(m.renderSearch())
			if !strings.Contains(search, c.want) {
				t.Errorf("search = %q, want %q", search, c.want)
			}
			if c.notWant != "" && strings.Contains(search, c.notWant) {
				t.Errorf("search = %q, must not contain %q", search, c.notWant)
			}
		})
	}
}
