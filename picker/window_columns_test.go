package main

import (
	"strings"
	"testing"
	"time"
)

// stubPS replaces the ps read for one test, clears the window resource cache
// before and after, and returns a counter of how many times ps ran.
func stubPS(t *testing.T, table string) *int {
	t.Helper()
	calls := 0
	orig := psOutput
	psOutput = func() ([]byte, error) {
		calls++
		return []byte(table), nil
	}
	reset := func() {
		windowResourceCache.Lock()
		windowResourceCache.result, windowResourceCache.ts = nil, time.Time{}
		windowResourceCache.Unlock()
	}
	reset()
	t.Cleanup(func() { psOutput = orig; reset() })
	return &calls
}

func windowRowsOf(items []listItem) []listItem {
	var rows []listItem
	for _, it := range items {
		if it.target != "" && !it.isHeader {
			rows = append(rows, it)
		}
	}
	return rows
}

// mixedWindows is a two-window local session plus a mirrored session whose
// windows carry a host.
func mixedWindows() []windowData {
	return []windowData{
		{session: "proj", index: 1, name: "a", path: "/home/noams/src/proj", active: true,
			labelID: "L ENG-1", labelRest: " first", prPlain: "  #10", prState: "open", prCheck: "success",
			cpuPct: 12, memMB: 300, resKnown: true},
		{session: "proj", index: 2, name: "b", path: "/srv/other", cpuPct: 250, memMB: 2048, resKnown: true},
		{session: "tp-g6-money", index: 1, name: "m", bridgeHost: "tp-g6", path: "/home/remote/money"},
	}
}

func col(s, needle string) int {
	before, _, ok := strings.Cut(s, needle)
	if !ok {
		return -1
	}
	return visibleWidth(before)
}

func TestWindowColumnHeaderLabelsAlignWithCells(t *testing.T) {
	t.Setenv("HOME", "/home/noams")
	items := renderWindowItemsWith(mixedWindows(), nil, map[string]string{}, nil, "dark", 0, false)
	hdr := items[0]
	if !hdr.isColumnHeader || hdr.target != "" {
		t.Fatalf("items[0] must be the unselectable column-label row, got %+v", hdr)
	}
	hd := stripANSI(hdr.display)
	for _, word := range []string{"Window", "Host", "Procs", "CPU", "Mem", "Path"} {
		if !strings.Contains(hd, word) {
			t.Errorf("header is missing %q: %q", word, hd)
		}
	}
	if !strings.Contains(hd, "CPU / "+iconMem) {
		t.Errorf("CPU and Mem labels should flank the separator: %q", hd)
	}

	rows := windowRowsOf(items)
	if len(rows) != 3 {
		t.Fatalf("want 3 window rows, got %d", len(rows))
	}
	local, mirror := stripANSI(rows[0].display), stripANSI(rows[2].display)
	if h, r := col(hd, iconHost), col(mirror, "tp-g6"); h != r {
		t.Errorf("host column: header at %d, row at %d\nhdr=%q\nrow=%q", h, r, hd, mirror)
	}
	for _, row := range []string{local, mirror} {
		if h, r := col(hd, " / "), col(row, " / "); h != r {
			t.Errorf("' / ' at %d in the header, %d in a row\nhdr=%q\nrow=%q", h, r, hd, row)
		}
		if h, r := col(hd, iconDir), col(row, iconDir); h != r {
			t.Errorf("path glyph at %d in the header, %d in a row\nhdr=%q\nrow=%q", h, r, hd, row)
		}
	}
}

func TestWindowColumnHeaderOnStateGroupedList(t *testing.T) {
	windows := mixedWindows()
	windows[0].agent = agentCounts{waiting: 1}
	items := renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", 0, true)
	if !items[0].isColumnHeader {
		t.Fatal("state-grouped list must lead with the column-label row too")
	}
	wantDisplay, wantPlain := stateGroupHeader("waiting", "dark", "")
	h := items[1]
	if !h.isHeader || h.display != wantDisplay || h.plain != wantPlain {
		t.Errorf("state group header changed: %q / %q, want %q / %q", h.display, h.plain, wantDisplay, wantPlain)
	}
}

func TestWindowMirrorRowShowsAndSearchesHost(t *testing.T) {
	items := renderWindowItemsWith(mixedWindows(), nil, map[string]string{}, nil, "dark", 0, false)
	rows := windowRowsOf(items)
	mirror := rows[2]
	if !strings.Contains(mirror.plain, "tp-g6") {
		t.Errorf("mirror row should show its host: %q", mirror.plain)
	}
	if strings.Contains(rows[0].plain, "tp-g6") {
		t.Errorf("local row must not carry a host: %q", rows[0].plain)
	}

	// The session name carries the <host>- prefix, so the host rides on the
	// name; a session without that prefix needs it added to the search text.
	ws := []windowData{{session: "money", index: 1, name: "m", bridgeHost: "tp-g6"}}
	row := windowRowsOf(renderWindowItemsWith(ws, nil, map[string]string{}, nil, "dark", 0, false))[0]
	if !strings.Contains(row.searchText, "tp-g6") {
		t.Errorf("searchText %q must contain the host", row.searchText)
	}

	m := newPickerModel(true, false, false, map[string]string{}, "dark", renderWindowItemsWith(ws, nil, map[string]string{}, nil, "dark", 0, false), "")
	m.query = "tpg6"
	m = m.withFilter()
	if len(windowRowsOf(m.visible)) != 1 {
		t.Errorf("a query on the host should match the mirror window, visible=%v", m.visible)
	}
}

func TestWindowPathRendersShortened(t *testing.T) {
	t.Setenv("HOME", "/home/noams")
	rows := windowRowsOf(renderWindowItemsWith(mixedWindows(), nil, map[string]string{}, nil, "dark", 0, false))
	if !strings.Contains(rows[0].plain, iconDir+" ~/src/proj") {
		t.Errorf("path should render ~-shortened behind the dir glyph: %q", rows[0].plain)
	}
	if !strings.Contains(rows[1].plain, "/srv/other") {
		t.Errorf("a path outside HOME renders as is: %q", rows[1].plain)
	}
}

func TestWindowSessionHeaderAggregateAndCount(t *testing.T) {
	windows := mixedWindows()
	panes := []agentPaneInfo{
		{session: "proj", winIdx: 1, state: "waiting"},
		{session: "proj", winIdx: 2, state: "waiting"},
	}
	items := renderWindowItemsWith(windows, nil, map[string]string{}, panes, "dark", 0, false)
	var proj, mirror listItem
	for _, it := range items {
		switch it.groupKey {
		case "proj":
			if it.isHeader {
				proj = it
			}
		case "tp-g6-money":
			if it.isHeader {
				mirror = it
			}
		}
	}
	if !strings.Contains(proj.plain, claudeStateIcon("waiting")) || !strings.Contains(proj.plain, "2 win") {
		t.Errorf("session header should show the aggregate agent icon and window count: %q", proj.plain)
	}
	if !proj.hasActiveAgent {
		t.Error("hasActiveAgent semantics changed")
	}
	if strings.Contains(mirror.plain, "tp-g6-") || !strings.Contains(mirror.plain, "money") || !strings.Contains(mirror.plain, "1 win") {
		t.Errorf("mirror header should show the display name without the host prefix: %q", mirror.plain)
	}
	if mirror.target != "tp-g6-money" || mirror.session != "tp-g6-money" || mirror.groupKey != "tp-g6-money" {
		t.Errorf("mirror header must keep the raw session name as target/session/groupKey: %+v", mirror)
	}
}

func TestWindowResourcesPlaceholderThenFilled(t *testing.T) {
	calls := stubPS(t, strings.Join([]string{
		"  PID  PPID %CPU   RSS COMMAND",
		"  100     1  2.0 204800 fish",
		"  101   100 10.0 102400 claude",
	}, "\n"))
	windows := []windowData{{session: "s", index: 1, name: "a", procs: []string{"fish"}, panePIDs: []int{100}}}

	// First paint: nothing has called ps, every figure is the placeholder.
	first := windowRowsOf(renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", 0, false))[0]
	if *calls != 0 {
		t.Fatalf("rendering ran ps %d times; first paint must not block on it", *calls)
	}
	if !strings.Contains(first.plain, resourcePlaceholder+" / ") {
		t.Errorf("first paint should carry the placeholder: %q", first.plain)
	}

	mergeWindowResources(windows)
	if *calls != 1 {
		t.Fatalf("merge ran ps %d times, want 1", *calls)
	}
	mergeWindowResources(windows)
	if *calls != 1 {
		t.Errorf("a second merge inside the TTL re-ran ps (%d calls)", *calls)
	}
	if !windows[0].resKnown || windows[0].cpuPct != 12 || windows[0].memMB != 300 {
		t.Errorf("merged resources = %+v, want 12%% / 300M", windows[0])
	}
	filled := windowRowsOf(renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", 0, false))[0]
	if !strings.Contains(filled.plain, "12%") || !strings.Contains(filled.plain, "300M") {
		t.Errorf("filled row should show the figures: %q", filled.plain)
	}
	// An agent relaunched behind a shell shows only in the process tree.
	if !strings.Contains(strings.Join(windows[0].procs, ","), "claude") {
		t.Errorf("procs %v should gain the tree's agent", windows[0].procs)
	}
	if !strings.Contains(filled.plain, stripANSI(func() string { s, _ := buildProcIcons([]string{"claude"}, 5); return s }())) {
		t.Errorf("row should show the agent's icon: %q", filled.plain)
	}
}

func TestWindowMirrorsKeepPlaceholderAndGainNoAgentCmds(t *testing.T) {
	stubPS(t, strings.Join([]string{
		"  PID  PPID %CPU   RSS COMMAND",
		"  100     1  2.0 204800 renderer",
		"  101   100 10.0 102400 claude",
		"  200     1  2.0 204800 renderer",
	}, "\n"))
	windows := []windowData{
		{session: "h-sess", index: 1, name: "a", bridgeHost: "h", procs: []string{"zsh"}, panePIDs: []int{100}},
		{session: "h-sess", index: 2, name: "b", bridgeHost: "h", procs: []string{"zsh"}, panePIDs: []int{200}},
	}
	mergeWindowResources(windows)
	for i, w := range windows {
		if len(w.procs) != 1 || w.procs[0] != "zsh" {
			t.Errorf("window %d procs = %v: a mirror's procs come from @bridge_proc, never the local tree", i, w.procs)
		}
		if w.resKnown {
			t.Errorf("window %d: a mirror must not report the renderer's figures", i)
		}
	}
	for _, r := range windowRowsOf(renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", 0, false)) {
		if !strings.Contains(r.plain, resourcePlaceholder+" / ") {
			t.Errorf("mirror row should render the placeholder: %q", r.plain)
		}
	}
}

func TestWindowNarrowWidthDropsColumnsInOrder(t *testing.T) {
	t.Setenv("HOME", "/home/noams")
	windows := mixedWindows()
	windows[0].path = "/home/noams/a/long/path/to/some/project"

	seen := map[string]bool{}
	for width := 200; width >= 20; width-- {
		items := renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", width, false)
		h := stripANSI(items[0].display)
		path, res, host := strings.Contains(h, "Path"), strings.Contains(h, " / "), strings.Contains(h, "Host")
		if (path && !res) || (res && !host) {
			t.Fatalf("width %d keeps path=%v cpu/mem=%v host=%v: Path must go first, then CPU/Mem, then Host", width, path, res, host)
		}
		seen[strings.Join([]string{boolS(path), boolS(res), boolS(host)}, "")] = true
		// Whatever is shown, the identity column keeps at least its floor.
		row := stripANSI(windowRowsOf(items)[0].display)
		if !strings.Contains(row, "ENG-1") {
			t.Errorf("width %d: identity lost to the optional columns: %q", width, row)
		}
	}
	for _, want := range []string{"111", "011", "001", "000"} {
		if !seen[want] {
			t.Errorf("no width shows the %s (path,cpu/mem,host) combination; saw %v", want, seen)
		}
	}

	all := stripANSI(renderWindowItemsWith(windows, nil, map[string]string{}, nil, "dark", 0, false)[0].display)
	if !strings.Contains(all, "Path") || !strings.Contains(all, "Host") || !strings.Contains(all, " / ") {
		t.Errorf("an unknown width keeps every column: %q", all)
	}
}

func boolS(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func TestWindowColumnHeaderSurvivesModeFiltersNotQueries(t *testing.T) {
	items := renderWindowItemsWith(mixedWindows(), nil, map[string]string{}, []agentPaneInfo{{session: "proj", winIdx: 1, state: "waiting"}}, "dark", 0, false)
	for name, set := range map[string]func(*tuiModel){
		"agent":   func(m *tuiModel) { m.agentOnly = true },
		"scratch": func(m *tuiModel) { m.scratchOnly = true },
	} {
		m := newPickerModel(true, false, false, map[string]string{}, "dark", items, "")
		set(&m)
		m = m.withFilter()
		if len(m.visible) == 0 || !m.visible[0].isColumnHeader {
			t.Errorf("%s filter dropped the pinned column row: %v", name, m.visible)
		}
	}

	m := newPickerModel(true, false, false, map[string]string{}, "dark", items, "")
	if m.isSelectable(m.visible[0]) || m.visible[m.cursor].isColumnHeader {
		t.Error("the column row must never hold the cursor")
	}
	m.query = "proj"
	m = m.withFilter()
	for _, it := range m.visible {
		if it.isColumnHeader {
			t.Error("a query drops the column row, as in session mode")
		}
	}
}

func windowListModel(width, height, cursor int) tuiModel {
	var windows []windowData
	for _, s := range []string{"alpha", "beta", "gamma"} {
		for i := 1; i <= 4; i++ {
			windows = append(windows, windowData{session: s, index: i, name: s, path: "/x"})
		}
	}
	items := renderWindowItemsWith(windows, map[string]int64{"alpha": 3, "beta": 2, "gamma": 1}, map[string]string{}, nil, "dark", 0, false)
	m := newPickerModel(true, false, false, map[string]string{}, "dark", items, "")
	m.width, m.height, m.ready = width, height, true
	m.cursor = cursor
	return m
}

func TestWindowListPinsColumnRowAndGroupRow(t *testing.T) {
	probe := windowListModel(120, 12, 1)
	want := len(strings.Split(probe.renderList(), "\n"))
	for cursor := range probe.visible {
		m := windowListModel(120, 12, cursor)
		if !m.isSelectable(m.visible[cursor]) {
			continue
		}
		lines := renderedLines(m)
		if len(lines) != want {
			t.Fatalf("cursor=%d rendered %d lines, want %d", cursor, len(lines), want)
		}
		if !strings.Contains(lines[0], "Window") {
			t.Errorf("cursor=%d: line 0 = %q, want the pinned column row", cursor, lines[0])
		}
		if n := strings.Count(strings.Join(lines, "\n"), "Window"); n != 1 {
			t.Errorf("cursor=%d: column row drawn %d times", cursor, n)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "▶") {
			t.Errorf("cursor=%d is off-screen", cursor)
		}
		// Once the list has scrolled past a session's header, that header is pinned
		// right under the column row.
		if cursor > 6 && !strings.Contains(lines[1], "win") {
			t.Errorf("cursor=%d: line 1 = %q, want the pinned session header", cursor, lines[1])
		}
		// A click on the cursor's own line lands on the cursor's row.
		top := m.listRowTop()
		for i, l := range strings.Split(m.renderList(), "\n") {
			if strings.Contains(l, "▶") {
				if got, ok := m.listIndexAt(0, top+i); !ok || got != cursor {
					t.Errorf("cursor=%d: click on its line mapped to %d (ok=%v)", cursor, got, ok)
				}
			}
		}
		// The pinned lines are not click targets.
		if _, ok := m.listIndexAt(0, top); ok {
			t.Errorf("cursor=%d: the column row must not be clickable", cursor)
		}
	}
}

func TestParseWindowPaneRowsPIDsAndPath(t *testing.T) {
	t.Setenv("HOME", "/home/noams")
	row := func(sess, idx, bridgeWin, panePath, pid, sessPath string) string {
		f := strings.Split(windowPaneRow(sess, idx, "n", "0", "fish", "1", "", panePath), "|")
		f[19], f[35], f[36] = bridgeWin, pid, sessPath
		return strings.Join(f, "|")
	}
	windows := windowsFromRows([]string{
		row("s", "0", "", "%h/proj", "100", ""),
		row("s", "0", "", "%h/proj", "101", ""),
		row("m", "0", "1", "/launcher/cwd", "200", "/remote/repo"),
		row("m", "1", "1", "/launcher/cwd", "201", ""),
	})
	if len(windows) != 3 {
		t.Fatalf("got %d windows, want 3", len(windows))
	}
	if w := windows[0]; w.path != "/home/noams/proj" || len(w.panePIDs) != 2 || w.panePIDs[0] != 100 || w.panePIDs[1] != 101 {
		t.Errorf("local window path/pids = %q %v", w.path, w.panePIDs)
	}
	if windows[1].path != "/remote/repo" {
		t.Errorf("mirror path = %q, want the @bridge_session_path stamp", windows[1].path)
	}
	if windows[2].path != "" {
		t.Errorf("an unstamped mirror renders no path, got %q", windows[2].path)
	}
	if len(strings.Split(windowsArgv()[len(windowsArgv())-1], "|")) != 37 {
		t.Error("windowsArgv format must carry 37 fields")
	}
}
