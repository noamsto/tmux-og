package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnderModal(t *testing.T) {
	got := underModal("#{pane_id}")
	want := "#{?window_modal_pane,#{P:#{?pane_last,#{pane_id},}},#{pane_id}}"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVolatileFieldsModalSwap(t *testing.T) {
	if len(volatileFields) != 23 {
		t.Fatalf("len = %d, want 23", len(volatileFields))
	}
	for _, tc := range []struct {
		idx  int
		want string
	}{
		{6, underModal("#{pane_current_path}")},
		{9, underModal("#{pane_current_command}")},
		{20, underModal("#{@bridge_proc}")},
		{22, "#{@bridge_usage}"},
	} {
		if volatileFields[tc.idx] != tc.want {
			t.Fatalf("volatileFields[%d] = %q, want %q", tc.idx, volatileFields[tc.idx], tc.want)
		}
	}
}

func TestBranchDisplay(t *testing.T) {
	if got := branchDisplay("feat/x", "/anything"); got != "feat/x" {
		t.Fatalf("got %q, want feat/x", got)
	}
}

func TestDirDisplay(t *testing.T) {
	if got := dirDisplay("/repo", "/repo"); got != "./" {
		t.Fatalf("at root = %q, want ./", got)
	}
	if got := dirDisplay("/repo/src/app", "/repo"); got != "./src/app" {
		t.Fatalf("subdir = %q, want ./src/app", got)
	}
}

func TestSessionSegmentBranchVariant(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		iconSession: "S", iconBranch: "B",
		thmRed: "#f00", thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd", claudeFg: "",
	}
	got := sessionSegment(a, false)
	want := "#[fg=#c6a] #[range=left]S work#[norange]  #[fg=#89b,bold]B feat/x"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestSessionSegmentIssueVariant(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x",
		issueID: "ENG-7", issueBranch: "feat/x", issueProvider: "linear", issueTitle: "Do it",
		iconSession: "S", iconLinear: "L", iconGitHub: "G",
		thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd", claudeFg: "",
	}
	got := sessionSegment(a, false)
	want := "#[fg=#c6a] #[range=left]S work#[norange]  #[fg=#89b,bold]L ENG-7 #[fg=#cdd,nobold]Do it"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestSessionSegmentCrewBadge(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		crewName: "coral", crewColor: "colour210", windowHasAgent: "1",
		iconSession: "S", iconBranch: "B",
		thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
	}
	got := sessionSegment(a, false)
	want := "#[fg=#c6a] #[range=left]S work#[norange]  #[fg=colour210]coral  #[fg=#89b,bold]B feat/x"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestSessionSegmentCrewBadgeColorFallback(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		crewName: "coral", crewColor: "", windowHasAgent: "1",
		iconSession: "S", iconBranch: "B",
		thmMauve: "#c6a", thmBlue: "#89b",
	}
	if got := sessionSegment(a, false); !strings.Contains(got, "#[fg=#c6a]coral  ") {
		t.Fatalf("empty crew-color should fall back to mauve, got %q", got)
	}
}

func TestSessionSegmentCrewBadgeNoAgent(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		crewName: "coral", crewColor: "colour210", windowHasAgent: "",
		iconSession: "S", iconBranch: "B",
		thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
	}
	if got := sessionSegment(a, false); strings.Contains(got, "coral") {
		t.Fatalf("badge should be hidden when window has no live agent, got %q", got)
	}
}

func TestSessionSegmentNoCrewBadge(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		iconSession: "S", iconBranch: "B", thmMauve: "#c6a", thmBlue: "#89b",
	}
	if got := sessionSegment(a, false); strings.Contains(got, "coral") || strings.Count(got, "#[fg=") != 2 {
		t.Fatalf("untagged window should have no badge segment, got %q", got)
	}
}

func TestSessionSegmentPrefixColor(t *testing.T) {
	a := args{session: "s", iconSession: "S", thmRed: "#f00", thmMauve: "#c6a", branch: "m", iconBranch: "B", thmBlue: "#89b"}
	got := sessionSegment(a, true)
	if !strings.HasPrefix(got, "#[fg=#f00,bold] #[range=left]S s") {
		t.Fatalf("prefix variant = %q", got)
	}
}

func TestSessionSegmentBridgeWinStopsAtPill(t *testing.T) {
	a := args{
		session: "work", branch: "feat/x", panePath: "/repo",
		issueID: "ENG-7", issueBranch: "feat/x", issueProvider: "linear",
		crewName: "coral", crewColor: "colour210",
		bridgeWin:   "1",
		iconSession: "S", iconBranch: "B", iconLinear: "L",
		thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
	}
	want := "#[fg=#c6a] #[range=left]S work#[norange]  "
	if got := sessionSegment(a, false); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestSessionSegmentBridgeFullIdentity(t *testing.T) {
	a := args{
		session: "work", panePath: "/repo",
		bridgeWin: "1", bridgeHost: "g6",
		bridgeCrewName: "coral", bridgeCrewColor: "colour210",
		bridgeLabelID: "L ENG-7", bridgeLabelRestLong: " Do it",
		iconSession: "S", iconRemote: "R",
		thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd", thmPeach: "#fab",
	}
	got := sessionSegment(a, false)
	want := "#[fg=#c6a] #[range=left]S work#[norange]  " +
		"#[fg=#fab]R g6  " +
		"#[fg=colour210]coral  " +
		"#[fg=#89b,bold]L ENG-7#[fg=#cdd,nobold] Do it"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestSessionSegmentBridgeIdlessRest(t *testing.T) {
	a := args{
		session: "work", panePath: "/repo",
		bridgeWin: "1", bridgeHost: "g6",
		bridgeLabelID: "", bridgeLabelRestLong: "feat/x",
		iconSession: "S", iconRemote: "R", iconBranch: "B",
		thmMauve: "#c6a", thmBlue: "#89b", thmPeach: "#fab",
	}
	got := sessionSegment(a, false)
	want := "#[fg=#c6a] #[range=left]S work#[norange]  " +
		"#[fg=#fab]R g6  " +
		"#[fg=#89b,bold]B feat/x"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

func TestLastGoodRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readLastGood(dir, "work"); ok {
		t.Fatal("cold cache should miss")
	}
	line := "#[align=left]painted line"
	writeLastGood(dir, "work", line)
	got, ok := readLastGood(dir, "work")
	if !ok || got != line {
		t.Fatalf("round-trip = %q,%v, want %q,true", got, ok, line)
	}
}

func TestLastGoodSessionIsolated(t *testing.T) {
	dir := t.TempDir()
	writeLastGood(dir, "a/b", "line-ab")
	writeLastGood(dir, "c d", "line-cd")
	if got, _ := readLastGood(dir, "a/b"); got != "line-ab" {
		t.Fatalf("session with slash = %q", got)
	}
	if got, _ := readLastGood(dir, "c d"); got != "line-cd" {
		t.Fatalf("session with space = %q", got)
	}
}

func TestRenderLineFull(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	os.MkdirAll(dir+"/issues", 0o755)
	os.WriteFile(dir+"/panes/1", []byte("state=processing\ntimestamp=9000\nsession=work\n"), 0o644)
	now := int64(9000)

	a := args{
		session: "work", branch: "feat/x", panePath: "/repo", gitRoot: "/repo",
		iconSession: "S", iconBranch: "B", iconDir: "D",
		thmBg: "#000", thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
		thmSubtext0: "#9a8", thmOverlay1: "#777",
		paneIcon: "I", paneCmd: ".nvim-wrapped",
	}

	got := renderLine(a, dir, "dark", false, now, "", nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S work#[norange]  #[fg=#89b,bold]B feat/x" +
		"  #[fg=#9a8,nobold]D ./" +
		"  #[fg=#777]#[fg=#94e2d5]󰪞#[fg=default] " +
		" #[align=right]" +
		"#[fg=#9a8]#{p-17:#{=/16/…:#{l:I nvim}}} "
	if got != want {
		t.Fatalf("renderLine\n got %q\nwant %q", got, want)
	}
}

// TestRenderLineBridgeWinSuppressesDir mirrors TestRenderLineFull but with
// @bridge_win set: dir must drop out (it describes the host repo the mirror
// daemon launched from, not the remote content), while the session pill and
// pane-command segment survive untouched.
func TestRenderLineBridgeWinSuppressesDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)
	now := int64(9000)

	a := args{
		session: "work", branch: "feat/x", panePath: "/repo", gitRoot: "/repo",
		bridgeWin:   "1",
		iconSession: "S", iconBranch: "B", iconDir: "D",
		thmBg: "#000", thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
		thmSubtext0: "#9a8", thmOverlay1: "#777",
		paneIcon: "I", paneCmd: ".nvim-wrapped",
	}

	got := renderLine(a, dir, "dark", false, now, "", nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S work#[norange]  " +
		"  #[fg=#777]" +
		" #[align=right]" +
		"#[fg=#9a8]#{p-17:#{=/16/…:#{l:I nvim}}} "
	if got != want {
		t.Fatalf("renderLine bridge\n got %q\nwant %q", got, want)
	}
}

// TestRenderLineBridgeHost: a mirror window names the machine it really runs on
// right after the session pill, so it can't read as a local window.
func TestRenderLineBridgeHost(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)

	a := args{
		session: "g6-main", bridgeWin: "1", bridgeHost: "g6",
		iconSession: "S", iconRemote: "R",
		thmBg: "#000", thmMauve: "#c6a", thmSubtext0: "#9a8", thmOverlay1: "#777",
		thmPeach: "#fab",
		paneIcon: "I", paneCmd: "zsh",
	}

	got := renderLine(a, dir, "dark", false, 9000, "", nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S g6-main#[norange]  " +
		"#[fg=#fab]R g6  " +
		"  #[fg=#777]" +
		" #[align=right]" +
		"#[fg=#9a8]#{p-17:#{=/16/…:#{l:I zsh}}} "
	if got != want {
		t.Fatalf("renderLine bridge host\n got %q\nwant %q", got, want)
	}
}

// TestRenderLineBridgeStateDisconnected: a mirror with a dropped control
// connection appends a red marker after the host badge, additive like
// @pr_draft on a PR badge — the host badge itself is untouched.
func TestRenderLineBridgeStateDisconnected(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)

	a := args{
		session: "g6-main", bridgeWin: "1", bridgeHost: "g6", bridgeState: "disconnected",
		iconSession: "S", iconRemote: "R",
		thmBg: "#000", thmMauve: "#c6a", thmSubtext0: "#9a8", thmOverlay1: "#777",
		thmPeach: "#fab", thmRed: "#f00",
		paneIcon: "I", paneCmd: "zsh",
	}

	got := renderLine(a, dir, "dark", false, 9000, "", nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S g6-main#[norange]  " +
		"#[fg=#fab]R g6  " +
		"#[fg=#f00]" + bridgeDisconnectedGlyph + "  " +
		"  #[fg=#777]" +
		" #[align=right]" +
		"#[fg=#9a8]#{p-17:#{=/16/…:#{l:I zsh}}} "
	if got != want {
		t.Fatalf("renderLine bridge disconnected\n got %q\nwant %q", got, want)
	}
}

// TestRenderLineBridgeStateParked: a parked mirror (daemon stopped retrying,
// waiting for a keypress) gets its own glyph and "press a key" hint instead
// of the disconnected badge.
func TestRenderLineBridgeStateParked(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)

	a := args{
		session: "g6-main", bridgeWin: "1", bridgeHost: "g6", bridgeState: "parked",
		iconSession: "S", iconRemote: "R",
		thmBg: "#000", thmMauve: "#c6a", thmSubtext0: "#9a8", thmOverlay1: "#777",
		thmPeach: "#fab", thmRed: "#f00",
		paneIcon: "I", paneCmd: "zsh",
	}

	got := renderLine(a, dir, "dark", false, 9000, "", nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S g6-main#[norange]  " +
		"#[fg=#fab]R g6  " +
		"#[fg=#777]" + bridgeParkedGlyph + " offline — press a key  " +
		"  #[fg=#777]" +
		" #[align=right]" +
		"#[fg=#9a8]#{p-17:#{=/16/…:#{l:I zsh}}} "
	if got != want {
		t.Fatalf("renderLine bridge parked\n got %q\nwant %q", got, want)
	}
}

func TestPaneSlot(t *testing.T) {
	got := paneSlot("I", "nvim", false)
	want := "#{p-17:#{=/16/…:#{l:I nvim}}}"
	if got != want {
		t.Fatalf("paneSlot\n got %q\nwant %q", got, want)
	}
}

// TestPaneSlotEmptyIcon covers the empty @active_pane_icon case (#260): the pad
// swallows the missing icon's cells, so the slot's shape — modifiers, widths,
// leading space before cmd — stays the same regardless of icon width.
func TestPaneSlotEmptyIcon(t *testing.T) {
	got := paneSlot("", "fish", false)
	want := "#{p-17:#{=/16/…:#{l: fish}}}"
	if got != want {
		t.Fatalf("paneSlot empty icon\n got %q\nwant %q", got, want)
	}
}

func TestPaneSlotStripsFormatChars(t *testing.T) {
	for _, tc := range []struct{ icon, cmd, want string }{
		{"I", "foo}bar", "#{p-17:#{=/16/…:#{l:I foobar}}}"},
		{"I", "a#[bold]b", "#{p-17:#{=/16/…:#{l:I a[bold]b}}}"},
		{"I", "x{y", "#{p-17:#{=/16/…:#{l:I xy}}}"},
		{"#{", "sh", "#{p-17:#{=/16/…:#{l: sh}}}"},
	} {
		if got := paneSlot(tc.icon, tc.cmd, false); got != tc.want {
			t.Errorf("paneSlot(%q, %q)\n got %q\nwant %q", tc.icon, tc.cmd, got, tc.want)
		}
	}
}

// TestPaneSlotAdjacentToUsage covers the #575 fix: adjacentToUsage=true flips
// the pad modifier to right-pad (no minus), pushing the reserved blank cells
// after the command instead of before it, mirroring TestPaneSlot/
// TestPaneSlotEmptyIcon's style for the false case.
func TestPaneSlotAdjacentToUsage(t *testing.T) {
	if got, want := paneSlot("I", "nvim", true), "#{p17:#{=/16/…:#{l:I nvim}}}"; got != want {
		t.Fatalf("paneSlot adjacent\n got %q\nwant %q", got, want)
	}
	if got, want := paneSlot("", "bash", true), "#{p17:#{=/16/…:#{l: bash}}}"; got != want {
		t.Fatalf("paneSlot adjacent empty icon\n got %q\nwant %q", got, want)
	}
}

// TestRenderLineUsageAdjacentToPaneSlot is the format-string-level regression
// guard for #575: with a non-empty usage, renderLine must wire
// adjacentToUsage=true through to paneSlot, so usage's output is immediately
// followed by the right-padded (#{p17:...}, no minus) slot with no
// characters in between.
func TestRenderLineUsageAdjacentToPaneSlot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/panes", 0o755)

	a := args{
		session: "work", branch: "feat/x", panePath: "/repo", gitRoot: "/repo",
		iconSession: "S", iconBranch: "B", iconDir: "D",
		thmBg: "#000", thmMauve: "#c6a", thmBlue: "#89b", thmText: "#cdd",
		thmSubtext0: "#9a8", thmOverlay1: "#777",
		paneIcon: "I", paneCmd: "bash",
	}
	usage := "#[fg=#0f0]42%·5h  "

	got := renderLine(a, dir, "dark", false, 9000, usage, nil)
	want := "#[align=left,bg=#000]" +
		"#[fg=#c6a] #[range=left]S work#[norange]  #[fg=#89b,bold]B feat/x" +
		"  #[fg=#9a8,nobold]D ./" +
		"  #[fg=#777]" +
		" #[align=right]" +
		usage +
		"#[fg=#9a8]#{p17:#{=/16/…:#{l:I bash}}} "
	if got != want {
		t.Fatalf("renderLine usage adjacency\n got %q\nwant %q", got, want)
	}
}

// TestPaneSlotPadDirectionLiveTmux confirms the pad-direction fact this whole
// fix depends on against a real tmux, not just Go string construction: the
// literal-format tests above would pass identically regardless of which way
// #{p17:}/#{p-17:} actually pad, since that's tmux's own render-time
// behaviour. Mirrors remotebridge/cmd/daemon's
// TestReflowRunShellArgsSurvivesFormatInjection for the tmux-presence check,
// including its fail-not-skip branch: picker/default.nix's plain `picker`
// derivation (built by `nix build .#default`, no tmux, no
// OG_REQUIRE_TMUX) also runs `go test ./statusline`, so a missing tmux
// under OG_REQUIRE_TMUX (set by pickerChecked's checkPhase in
// flake.nix, which also adds pkgs.tmux) means that input was pruned, not
// that this is a dev machine.
func TestPaneSlotPadDirectionLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}

	// tmux never unlinks a socket file on exit, so a server started in the
	// ambient TMUX_TMPDIR leaves a dead entry in the user's runtime dir on
	// every run. A private short dir (the path is capped at ~108 bytes) keeps
	// the leftover inside what the cleanup removes.
	tmpdir, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	const socket = "s"
	env := append(os.Environ(), "TMUX_TMPDIR="+tmpdir)

	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "t1")
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command("tmux", "-L", socket, "kill-server")
		stop.Env = env
		_ = stop.Run()
	})

	// Wraps the rendered format in sentinel brackets and strips only those
	// plus the trailing newline — a stray strings.TrimSpace would erase the
	// exact leading/trailing pad cells this test exists to measure.
	render := func(icon, cmd string, adjacentToUsage bool) string {
		format := "[" + paneSlot(icon, cmd, adjacentToUsage) + "]"
		show := exec.Command("tmux", "-L", socket, "display-message", "-p", "-t", "t1", "-F", format)
		show.Env = env
		out, err := show.Output()
		if err != nil {
			t.Fatalf("display-message: %v", err)
		}
		s := strings.TrimSuffix(string(out), "\n")
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		return s
	}

	for _, adjacent := range []bool{false, true} {
		content := " sh" // empty icon leaves its own leading space before "sh"
		got := render("", "sh", adjacent)
		if n := len([]rune(got)); n != paneSlotPad {
			t.Fatalf("short cmd adjacentToUsage=%v: rendered %d cells, want %d (%q)", adjacent, n, paneSlotPad, got)
		}
		fill := strings.Repeat(" ", paneSlotPad-len([]rune(content)))
		want := fill + content
		if adjacent {
			want = content + fill
		}
		if got != want {
			t.Fatalf("short cmd adjacentToUsage=%v\n got %q\nwant %q", adjacent, got, want)
		}
	}

	for _, adjacent := range []bool{false, true} {
		got := render("", "a-very-long-command-name-indeed", adjacent)
		if n := len([]rune(got)); n != paneSlotPad {
			t.Fatalf("long cmd adjacentToUsage=%v: rendered %d cells, want %d (already-full box, no room for padding either way) (%q)", adjacent, n, paneSlotPad, got)
		}
	}
}

// In a mirror the local pane runs the bridge renderer, so the command beside
// the pane icon must come from @bridge_proc — the icon itself already does, and
// the two disagreeing is what #590 looked like.
func TestPaneCmdDisplayPrefersBridgeProc(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, bridgeProc, want string
	}{
		{"no bridge", ".nvim-wrapped", "", "nvim"},
		{"mirror", "og-remote-bridge-renderer", "claude", "claude"},
		{"mirror unwraps too", "og-remote-bridge-renderer", ".nvim-wrapped", "nvim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneCmdDisplay(tc.cmd, tc.bridgeProc); got != tc.want {
				t.Errorf("paneCmdDisplay(%q, %q) = %q, want %q", tc.cmd, tc.bridgeProc, got, tc.want)
			}
		})
	}
}

func TestThemeFromFlavor(t *testing.T) {
	for _, tc := range []struct{ flavor, want string }{
		{"latte", "light"},
		{"mocha", "dark"},
		{"frappe", "dark"},
	} {
		if got := themeFromFlavor(tc.flavor); got != tc.want {
			t.Errorf("themeFromFlavor(%q) = %q, want %q", tc.flavor, got, tc.want)
		}
	}

	t.Run("empty flavor falls back to the themestate file", func(t *testing.T) {
		// "light" here is distinct from themestate.Detect()'s no-file
		// default of "dark", so this catches the fallback branch being
		// dropped, not just matching the default it would also return.
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		if err := os.WriteFile(filepath.Join(dir, "theme-state.json"), []byte(`{"theme":"light"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := themeFromFlavor(""); got != "light" {
			t.Errorf("themeFromFlavor(\"\") = %q, want %q (from the pinned state file)", got, "light")
		}
	})
}
