package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCollectorArgvCarriesNotModalFilter(t *testing.T) {
	for name, argv := range map[string][]string{
		"panesSnapshotArgv": panesSnapshotArgv(),
		"windowsArgv":       windowsArgv(),
	} {
		i := slices.Index(argv, "-f")
		if i == -1 || i+1 >= len(argv) || argv[i+1] != notModalFilter {
			t.Fatalf("%s = %v, want -f %q", name, argv, notModalFilter)
		}
	}
}

func TestAgentPriority(t *testing.T) {
	cases := []struct {
		name string
		c    agentCounts
		want string
	}{
		{"error wins over everything", agentCounts{errorCnt: 1, waiting: 1, done: 1}, "error"},
		{"waiting beats denied/compacting/processing/done/idle",
			agentCounts{waiting: 1, denied: 1, compacting: 1, processing: 1, done: 1, idle: 1}, "waiting"},
		{"denied beats compacting/processing/done/idle",
			agentCounts{denied: 1, compacting: 1, processing: 1, done: 1, idle: 1}, "denied"},
		{"compacting beats processing/done/idle",
			agentCounts{compacting: 1, processing: 1, done: 1, idle: 1}, "compacting"},
		{"processing beats done/idle", agentCounts{processing: 1, done: 1, idle: 1}, "processing"},
		{"done beats idle", agentCounts{done: 1, idle: 1}, "done"},
		{"idle alone", agentCounts{idle: 1}, "idle"},
		{"all zero", agentCounts{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentPriority(c.c); got != c.want {
				t.Errorf("agentPriority(%+v) = %q, want %q", c.c, got, c.want)
			}
		})
	}
}

func TestAgentStateOrderMatchesPriority(t *testing.T) {
	// agentStateOrder is what groupWindowsByState (Task 2) walks to decide
	// header order; it must name every state agentPriority can return, in
	// the same order, or the two would silently diverge.
	if len(agentStateOrder) != 7 {
		t.Fatalf("agentStateOrder has %d entries, want 7 (error/waiting/denied/compacting/processing/done/idle)",
			len(agentStateOrder))
	}
	want := []string{"error", "waiting", "denied", "compacting", "processing", "done", "idle"}
	for i, s := range want {
		if agentStateOrder[i] != s {
			t.Errorf("agentStateOrder[%d] = %q, want %q", i, agentStateOrder[i], s)
		}
	}
}

// A remote window name reaches us through @window_bridge_name in the daemon's
// escaped form, because the status line collapses the doubling when it draws.
// The picker draws its own rows, so it has to undo the escape first.
func TestDecodeBridgeName(t *testing.T) {
	cases := map[string]string{
		"pr##367":               "pr#367",
		"a####b":                "a##b",
		"plain-name":            "plain-name",
		"":                      "",
		"[nix-amd-ai 󰪣 󰘭 ##46]": "[nix-amd-ai 󰪣 󰘭 #46]",
	}
	for in, want := range cases {
		if got := decodeBridgeName(in); got != want {
			t.Errorf("decodeBridgeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionHeaderLabelsAndAlignment(t *testing.T) {
	snap := panesSnapshot{
		"%1|tmux-og|0|/home/noams/git/tmux-og|1900000300||fish|1|||",
		"%2|tp-g6-money|0|/home/noams/src|1900000200|tp-g6|fish|1||/home/noams/src|",
	}
	items := buildSessionItems(nil, snap, nil, "dark", false, "")
	hdr := items[0]
	if !hdr.isColumnHeader {
		t.Fatal("items[0] must be flagged as the column header for the sticky pin")
	}
	for _, word := range []string{"Session", "Host", "Procs", "CPU", "Mem", "Path"} {
		if !strings.Contains(hdr.plain, word) {
			t.Errorf("header is missing the label %q: %q", word, hdr.plain)
		}
	}
	for _, glyph := range []string{iconHost, iconProcs, iconCPU, iconMem} {
		if !strings.Contains(hdr.plain, glyph) {
			t.Errorf("header is missing glyph %q: %q", glyph, hdr.plain)
		}
	}
	// CPU and Mem must be distinguishable, not the same glyph twice.
	if iconCPU == iconMem {
		t.Error("CPU and Mem share a glyph")
	}
	// The Host column has to start at the same cell in the header and in a row,
	// which len()-based padding would get wrong: a glyph is 1 cell, 4 bytes.
	col := func(s, needle string) int { return visibleWidth(s[:strings.Index(s, needle)]) }
	if h, r := col(hdr.plain, iconHost), col(items[2].plain, "tp-g6 "); h != r {
		t.Errorf("host column starts at %d in the header but %d in the row", h, r)
	}
	// The CPU and Mem labels mirror each other around the separator: CPU ends on
	// its left, Mem starts on its right, reading as one CPU / Mem unit.
	if !strings.Contains(hdr.plain, "CPU / "+iconMem) {
		t.Errorf("CPU and Mem labels should flank the separator: %q", hdr.plain)
	}
	// Both header and rows must agree on where the resource field ends, or Path
	// drifts: the label moving left may not change the field's width.
	if h, r := col(hdr.plain, " / "), col(items[1].plain, " / "); h != r {
		t.Errorf("the ' / ' sits at %d in the header but %d in a row", h, r)
	}
	if h, r := col(hdr.plain, iconDir), col(items[1].plain, iconDir); h != r {
		t.Errorf("path column starts at %d in the header but %d in the row", h, r)
	}
}

// A mirror pane's own pane_current_command is the bridge renderer, not the
// remote's real command — @bridge_proc carries the remote's, and must win
// when non-empty (#513).
func TestSessionsBridgeProcOverride(t *testing.T) {
	snap := panesSnapshot{
		"%1|mirror-sess|0|/home/noams/git/tmux-og|1900000300|tp-g6|fish|1|claude|/srv/remote/repo|",
		"%2|local-sess|0|/home/noams/src|1900000200||bash|2||/ignored|",
	}
	sessions := snap.sessions()
	byName := map[string]sessionData{}
	for _, s := range sessions {
		byName[s.name] = s
	}
	mirror, ok := byName["mirror-sess"]
	if !ok {
		t.Fatal("mirror-sess missing from sessions()")
	}
	if len(mirror.procs) != 1 || mirror.procs[0] != "claude" {
		t.Errorf("mirror-sess procs = %v, want [claude] (bridge_proc must override the renderer's fish)", mirror.procs)
	}
	if mirror.path != "/srv/remote/repo" {
		t.Errorf("mirror-sess path = %q, want the remote's @bridge_session_path, not the launcher's cwd", mirror.path)
	}
	local, ok := byName["local-sess"]
	if !ok {
		t.Fatal("local-sess missing from sessions()")
	}
	if len(local.procs) != 1 || local.procs[0] != "bash" {
		t.Errorf("local-sess procs = %v, want [bash] (empty bridge_proc must fall through to pane_current_command)", local.procs)
	}
}

// sortSessionsForDisplay orders by activity desc, then name asc, and nothing
// else: a current session keeps its rank even beside a mirror it collides
// with (#551).
func TestSortSessionsForDisplay(t *testing.T) {
	names := func(sessions []sessionData) []string {
		out := make([]string, len(sessions))
		for i, s := range sessions {
			out[i] = s.name
		}
		return out
	}

	cases := []struct {
		name     string
		sessions []sessionData
		want     []string
	}{
		{
			name: "unique display names: activity desc",
			sessions: []sessionData{
				{name: "b", activity: 100},
				{name: "a", activity: 200},
			},
			want: []string{"a", "b"},
		},
		{
			name: "equal activity: name asc",
			sessions: []sessionData{
				{name: "dup", activity: 100},
				{name: "dup", activity: 200},
			},
			want: []string{"dup", "dup"},
		},
		{
			name: "same-name different-host, local is current: keeps its activity rank",
			sessions: []sessionData{
				{name: "tmux-og", bridgeHost: "", activity: 100, current: true},
				{name: "g6-tmux-og", bridgeHost: "g6", activity: 50},
			},
			want: []string{"tmux-og", "g6-tmux-og"},
		},
		{
			name: "same-name different-host, mirror is current: keeps its activity rank",
			sessions: []sessionData{
				{name: "g6-tmux-og", bridgeHost: "g6", activity: 100, current: true},
				{name: "tmux-og", bridgeHost: "", activity: 50},
			},
			want: []string{"g6-tmux-og", "tmux-og"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sortSessionsForDisplay(c.sessions)
			if got := names(c.sessions); !slices.Equal(got, c.want) {
				t.Errorf("order = %v, want %v", got, c.want)
			}
		})
	}
}

// buildSessionItems marks `current` on the row the client is attached to —
// the flag sinkCurrentMatchBelowPeer reads once a query is typed.
func TestBuildSessionItemsMarksCurrent(t *testing.T) {
	snap := panesSnapshot{
		"%1|tmux-og|0|/home/noams/git/tmux-og|1900000300||fish|1|||",
		"%2|g6-tmux-og|0|/home/noams/src|1900000100|g6|fish|2||/home/noams/src|",
	}
	items := buildSessionItems(nil, snap, nil, "dark", false, "tmux-og")
	// items[0] is the column-header row.
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (header + 2 sessions)", len(items))
	}
	if items[1].target != "tmux-og" || !items[1].current {
		t.Errorf("items[1] = %q current=%v, want tmux-og current=true (most recent activity, unsunk)", items[1].target, items[1].current)
	}
	if items[2].target != "g6-tmux-og" || items[2].current {
		t.Errorf("items[2] = %q current=%v, want g6-tmux-og current=false", items[2].target, items[2].current)
	}
}

func TestParseShowOptionsLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantName  string
		wantValue string
		wantOK    bool
	}{
		{"normal", "@opt value", "@opt", "value", true},
		{"empty value, trailing space", "@opt ", "@opt", "", true},
		{"internal spaces", "@opt some value with spaces", "@opt", "some value with spaces", true},
		{"no space at all", "@opt", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, value, ok := parseShowOptionsLine(c.line)
			if name != c.wantName || value != c.wantValue || ok != c.wantOK {
				t.Errorf("parseShowOptionsLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.line, name, value, ok, c.wantName, c.wantValue, c.wantOK)
			}
		})
	}
}

func TestThemeFromOpts(t *testing.T) {
	cases := []struct {
		name   string
		flavor string
		want   string
	}{
		{"latte", "latte", "light"},
		{"mocha", "mocha", "dark"},
		{"non-latte dark flavor falls to dark default", "frappe", "dark"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := themeFromOpts(map[string]string{"@catppuccin_flavor": c.flavor})
			if got != c.want {
				t.Errorf("themeFromOpts(%q) = %q, want %q", c.flavor, got, c.want)
			}
		})
	}

	t.Run("unset falls back to the themestate file", func(t *testing.T) {
		// "light" here is distinct from themestate.Detect()'s no-file
		// default of "dark", so this catches the fallback branch being
		// dropped, not just matching the default it would also return.
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		if err := os.WriteFile(filepath.Join(dir, "theme-state.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := themeFromOpts(map[string]string{}); got != "light" {
			t.Errorf("themeFromOpts(unset) = %q, want %q (from the pinned state file)", got, "light")
		}
	})
}

// windowPaneRow builds one list-panes -a row in parseWindowPaneRows' field
// order (see collectWindows' -F string), for tests below.
func windowPaneRow(fields ...string) string {
	const n = 35
	row := make([]string, n)
	copy(row, fields)
	return strings.Join(row, "|")
}

func TestParseWindowPaneRowsBridgeIdentity(t *testing.T) {
	// A bridge row takes the bridge copies wholesale — including over a
	// window carrying its own (launcher-residue) label/crew/PR fields — and
	// clears branch, which is what arms and then disarms collectWindows' git
	// fallback.
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "should-be-cleared", "/some/path",
		"LOCAL-1", " local title", " pr-local", "open", "pending", "mergeable",
		"local-crew", "colour0", "", "", "",
		"1", "raven", "colour1", "BRIDGE-1", " bridge title",
		" pr-bridge", "closed", "success", "conflicting",
	)
	order, m := parseWindowPaneRows([]string{row})
	if len(order) != 1 {
		t.Fatalf("got %d windows, want 1", len(order))
	}
	wi := m[order[0]]
	if !wi.bridgeWin {
		t.Fatal("bridgeWin = false, want true")
	}
	if wi.branch != "" {
		t.Errorf("branch = %q, want cleared", wi.branch)
	}
	if wi.crewName != "raven" || wi.crewColor != "colour1" {
		t.Errorf("crew = %q/%q, want raven/colour1", wi.crewName, wi.crewColor)
	}
	if wi.labelID != "BRIDGE-1" || wi.labelRest != " bridge title" {
		t.Errorf("label = %q/%q, want BRIDGE-1/ bridge title", wi.labelID, wi.labelRest)
	}
	if wi.prPlain != " pr-bridge" || wi.prState != "closed" || wi.prCheck != "success" || wi.prMergeable != "conflicting" {
		t.Errorf("pr = %q/%q/%q/%q, want bridge values", wi.prPlain, wi.prState, wi.prCheck, wi.prMergeable)
	}
}

func TestParseWindowPaneRowsBridgeHost(t *testing.T) {
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "feature/x", "/some/path",
		"", "", "", "", "", "",
		"", "", "", "", "",
		"", "", "", "", "",
		"", "", "", "",
		"tp-g6",
	)
	order, m := parseWindowPaneRows([]string{row})
	wi := m[order[0]]
	if wi.bridgeHost != "tp-g6" {
		t.Errorf("bridgeHost = %q, want tp-g6", wi.bridgeHost)
	}
}

func TestParseWindowPaneRowsLocalUnchanged(t *testing.T) {
	// A non-bridge row (@bridge_win empty) must ignore the bridge fields
	// entirely, even when tmux hands back garbage in them.
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "feature/x", "/some/path",
		"LIN-1", " local title", " pr-local", "open", "pending", "mergeable",
		"local-crew", "colour0", "", "", "",
		"", "garbage-crew", "garbage-color", "GARBAGE-1", " garbage title",
		" garbage-pr", "garbage", "garbage", "garbage",
		"", "", "", "", "", "", "1",
	)
	order, m := parseWindowPaneRows([]string{row})
	wi := m[order[0]]
	if wi.bridgeWin {
		t.Fatal("bridgeWin = true, want false")
	}
	if wi.branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", wi.branch)
	}
	if wi.labelID != "LIN-1" || wi.crewName != "local-crew" {
		t.Errorf("label/crew = %q/%q, want local's own values, not the bridge fields", wi.labelID, wi.crewName)
	}
}

func TestParseWindowPaneRowsLocalNoAgentBlanksCrew(t *testing.T) {
	// A non-bridge window with no live agent (@window_has_agent empty, the
	// trailing field left unset) must blank the crew codename badge while
	// leaving its label/branch identity untouched (#671).
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "feature/x", "/some/path",
		"LIN-1", " local title", " pr-local", "open", "pending", "mergeable",
		"local-crew", "colour0", "", "", "",
	)
	order, m := parseWindowPaneRows([]string{row})
	wi := m[order[0]]
	if wi.bridgeWin {
		t.Fatal("bridgeWin = true, want false")
	}
	if wi.crewName != "" || wi.crewColor != "" {
		t.Errorf("crew = %q/%q, want blanked (no live agent)", wi.crewName, wi.crewColor)
	}
	if wi.labelID != "LIN-1" || wi.branch != "feature/x" {
		t.Errorf("label/branch = %q/%q, want unaffected", wi.labelID, wi.branch)
	}
}

func TestParseWindowPaneRowsBridgeEmptyIDFallsBackToRest(t *testing.T) {
	// A remote window on a branch with no detected issue has an empty bridge
	// id but a non-empty rest — bridgeName must take that rest raw, not
	// through decodeBridgeName (which would mangle a '#' inside it).
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "", "/some/path",
		"", "", "", "", "", "",
		"", "", "", "", "",
		"1", "raven", "colour1", "", " feature title",
		"", "", "", "",
	)
	order, m := parseWindowPaneRows([]string{row})
	wi := m[order[0]]
	if wi.labelID != "" {
		t.Fatalf("labelID = %q, want empty", wi.labelID)
	}
	if wi.bridgeName != " feature title" {
		t.Errorf("bridgeName = %q, want the raw bridge rest", wi.bridgeName)
	}
}

func TestParseWindowPaneRowsBareBridgeKeepsDecodedWindowName(t *testing.T) {
	// A bare mirror (no bridge id, no bridge rest) falls back to the decoded
	// @window_bridge_name. Reflow stamps the '#'-doubled name into
	// @window_label_rest_long on a bare mirror too, so this fixture carries
	// "feat##1" in BOTH fields — only sourcing bridgeName from the bridge
	// copy (not reflow's) catches a wrong implementation that would render
	// "feat##1" undecoded instead of "feat#1".
	row := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "", "/some/path",
		"", "feat##1", "", "", "", "",
		"", "", "feat##1", "", "",
		"1", "", "", "", "",
		"", "", "", "",
	)
	order, m := parseWindowPaneRows([]string{row})
	wi := m[order[0]]
	if wi.labelID != "" || wi.labelRest != "" {
		t.Fatalf("labelID/labelRest = %q/%q, want both empty", wi.labelID, wi.labelRest)
	}
	if wi.bridgeName != "feat#1" {
		t.Errorf("bridgeName = %q, want feat#1 (decoded)", wi.bridgeName)
	}
}

// Same override as TestSessionsBridgeProcOverride, for the window-mode
// collector: a mirror pane's pane_current_command is the bridge renderer
// ("fish"), and @bridge_proc (the trailing field) carries the remote's real
// command, which must win when non-empty.
func TestParseWindowPaneRowsBridgeProcOverride(t *testing.T) {
	mirrorRow := windowPaneRow(
		"sess", "0", "winname", "0", "fish", "1", "", "/some/path",
		"", "", "", "", "", "",
		"", "", "", "", "",
		"1", "", "", "", "",
		"", "", "", "",
		"tp-g6", "claude",
	)
	localRow := windowPaneRow(
		"sess", "1", "winname2", "0", "bash", "1", "feature/x", "/some/path",
		"", "", "", "", "", "",
		"", "", "", "", "",
		"", "", "", "", "",
		"", "", "", "",
		"", "",
	)
	order, m := parseWindowPaneRows([]string{mirrorRow, localRow})
	if len(order) != 2 {
		t.Fatalf("got %d windows, want 2", len(order))
	}
	mirror := m[order[0]]
	if len(mirror.procs) != 1 || mirror.procs[0] != "claude" {
		t.Errorf("mirror procs = %v, want [claude] (bridge_proc must override the renderer's fish)", mirror.procs)
	}
	local := m[order[1]]
	if len(local.procs) != 1 || local.procs[0] != "bash" {
		t.Errorf("local procs = %v, want [bash] (empty bridge_proc must fall through to pane_current_command)", local.procs)
	}
}

func TestEmptyRemoteHostsOptionYieldsNoSection(t *testing.T) {
	// readTmuxOpts's raw -F read returns an unset/empty option as "" directly
	// (#474), so callers now receive an already-raw empty string rather than
	// the quoted '' show -g used to print.
	if got := parseRemoteHosts(""); got != nil {
		t.Errorf("got %q, want no hosts", got)
	}
	if got := pendingRemoteItems(map[string]string{"@remote_bridge_hosts": ""}, nil); got != nil {
		t.Errorf("got %d rows, want no Remote section", len(got))
	}
}
