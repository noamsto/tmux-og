package main

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/noamsto/tmux-og/picker/enrichstate"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func testCfg() cfg {
	return cfg{
		target: "$0:@0", prEnrichBin: "/bin/true", issueStampBin: "/bin/true",
		fg: "#cdd6f4", mauve: "#cba6f7", red: "#f38ba8",
		green: "#a6e3a1", peach: "#fab387", blue: "#89b4fa",
		overlay0: "#6c7086", subtext0: "#a6adc8",
		icLinear: "L", icGitHub: "G", icPending: "P", icSuccess: "S",
		icFailure: "F", icMerged: "M", icClosed: "C", icConflict: "X",
		icDraft: "D",
	}
}

func render(m model) string { return stripANSI(m.card()) }

// bridgedCfg is testCfg plus a resolved bridge handle — a mirror window whose
// bind could resolve @bridge_sock/@bridge_pane.
func bridgedCfg() cfg {
	c := testCfg()
	c.bridgeCtlBin = "/bin/true"
	c.bridgeSock = "/tmp/bridge.sock"
	c.bridgePane = "%3"
	return c
}

func TestCardFullIssueAndPR(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		issueProvider: "linear", issueID: "ENG-6794", issueTitle: "Carousel nav",
		issueURL: "https://linear.app/x/issue/ENG-6794",
		prNumber: "103", prState: "open", prCheck: "success", prMergeable: "mergeable",
		prTitle: "kitty nav", branch: "feat/103-kitty-nav",
	}}
	out := render(m)
	for _, want := range []string{"ENG-6794", "Carousel nav", "#103", "kitty nav", "[r] refresh", "[q] close"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q\n%s", want, out)
		}
	}
}

func TestCardNoIssueNoPR(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{branch: "main"}}
	out := render(m)
	if !strings.Contains(out, "no issue") || !strings.Contains(out, "no PR") {
		t.Errorf("expected no-issue/no-PR fallbacks\n%s", out)
	}
}

func TestCardMergedGlyph(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		prNumber: "103", prState: "merged", prCheck: "pending", branch: "b"}}
	out := render(m)
	if !strings.Contains(out, "M #103") { // merged glyph wins over pending check
		t.Errorf("expected merged glyph 'M #103'\n%s", out)
	}
}

func TestCardDraftGlyph(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		prNumber: "103", prState: "open", prCheck: "success", prDraft: "1", branch: "b"}}
	out := render(m)
	if !strings.Contains(out, "D S #103") { // draft marker ahead of the check glyph
		t.Errorf("expected draft badge 'D S #103'\n%s", out)
	}
}

func TestCardEmptyBranchDisablesRefresh(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{prNumber: "103", prState: "open"}}
	out := render(m)
	if !strings.Contains(out, "no branch") || strings.Contains(out, "[r] refresh") {
		t.Errorf("empty branch should disable refresh\n%s", out)
	}
}

func TestCardRefreshingSpinner(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, refreshing: true, win: winState{
		prNumber: "103", prState: "open", prCheck: "success", branch: "b"}}
	if !strings.Contains(render(m), "refreshing") {
		t.Errorf("expected refreshing spinner")
	}
}

func TestHandleKeyQuitAndActions(t *testing.T) {
	base := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		issueURL: "http://x", prURL: "http://y", branch: "b", prNumber: "1"}}

	if _, cmd := base.handleKey("q"); cmd == nil {
		t.Error("q should return a quit cmd")
	}
	if _, cmd := base.handleKey("ctrl+c"); cmd == nil {
		t.Error("ctrl+c should return a quit cmd")
	}
	m2, cmd := base.handleKey("r")
	if cmd == nil || !m2.(model).refreshing {
		t.Error("r with a branch should start refreshing")
	}

	noBranch := base
	noBranch.win.branch = ""
	if m3, _ := noBranch.handleKey("r"); m3.(model).refreshing {
		t.Error("r with empty branch must NOT start refreshing")
	}
}

// TestFooterBridgeStates covers the four-row [r] contract from design D5.
func TestFooterBridgeStates(t *testing.T) {
	tests := []struct {
		name string
		m    model
		want string
	}{
		{"non-mirror", model{cfg: testCfg(), width: 60, height: 18, win: winState{branch: "b"}}, "[r] refresh"},
		{"mirror no branch", model{cfg: bridgedCfg(), width: 60, height: 18, mirror: true, win: winState{}}, "[r] no branch"},
		{"mirror no bridge handle", model{cfg: testCfg(), width: 60, height: 18, mirror: true, win: winState{branch: "b"}}, "[r] no bridge"},
		{"mirror with handle", model{cfg: bridgedCfg(), width: 60, height: 18, mirror: true, win: winState{branch: "b"}}, "[r] refresh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out := render(tc.m); !strings.Contains(out, tc.want) {
				t.Errorf("footer missing %q\n%s", tc.want, out)
			}
		})
	}
}

func TestBridgeRefreshArgv(t *testing.T) {
	got := bridgeRefreshArgv("/tmp/bridge.sock", "%7")
	want := []string{"--sock", "/tmp/bridge.sock", "enrich-refresh", "%7"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHandleKeyBridgedRefreshDispatchesAndGuardsReentry(t *testing.T) {
	base := model{cfg: bridgedCfg(), width: 60, height: 18, mirror: true, win: winState{branch: "b"}}

	m1, cmd := base.handleKey("r")
	if cmd == nil {
		t.Fatal("r on a mirror with a bridge handle should dispatch the ctl cmd")
	}
	m1s := m1.(model)
	if !m1s.sending {
		t.Error("dispatching the bridged refresh should set sending")
	}
	if m1s.refreshing {
		t.Error("the bridged path has nothing local to converge on and must not set refreshing")
	}

	// A held press while sending must not queue a second remote --force pass.
	if _, cmd2 := m1s.handleKey("r"); cmd2 != nil {
		t.Error("a second r while sending should be a no-op")
	}
}

func TestHandleKeyMirrorNoBridgeIsInert(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, mirror: true, win: winState{branch: "b"}}
	if _, cmd := m.handleKey("r"); cmd != nil {
		t.Error("r on a mirror with no bridge handle must be inert")
	}
}

func TestHandleKeyMirrorNoBranchIsInert(t *testing.T) {
	m := model{cfg: bridgedCfg(), width: 60, height: 18, mirror: true, win: winState{}}
	if _, cmd := m.handleKey("r"); cmd != nil {
		t.Error("r on a mirror with no branch must stay inert even with a bridge handle (D5)")
	}
}

func TestBridgeRefreshDoneMsgFlashesRealOutcome(t *testing.T) {
	base := model{cfg: bridgedCfg(), width: 60, height: 18, sending: true}

	okM, _ := base.Update(bridgeRefreshDoneMsg{})
	ok := okM.(model)
	if ok.sending {
		t.Error("a done msg must clear sending on success")
	}
	if ok.flash != "refresh sent ↗" {
		t.Errorf("flash = %q, want the success flash", ok.flash)
	}
	if ok.flashIsError {
		t.Error("a success flash must not be marked as an error")
	}
	if !ok.flashUntil.After(time.Now().Add(flashConfirmDuration - time.Second)) {
		t.Error("success flash should carry the confirm-duration deadline")
	}

	failM, _ := base.Update(bridgeRefreshDoneMsg{errText: "bridge daemon unreachable"})
	fail := failM.(model)
	if fail.sending {
		t.Error("a done msg must clear sending on failure too")
	}
	if fail.flash != "bridge daemon unreachable" {
		t.Errorf("flash = %q, want the ctl's own error text", fail.flash)
	}
	if !fail.flashIsError {
		t.Error("a ctl error flash must be marked as an error")
	}
	if !fail.flashUntil.After(time.Now().Add(flashConfirmDuration)) {
		t.Error("a ctl error should carry the longer error-duration deadline, not the confirm one")
	}
}

// TestFooterFlashColor pins the color-by-outcome contract (#762): an error
// flash renders in c.red, a confirmation flash in c.green.
func TestFooterFlashColor(t *testing.T) {
	cfg := testCfg()
	m := model{cfg: cfg}

	errM := model{cfg: cfg, width: 60, height: 18, flash: "boom", flashIsError: true}
	wantErr := m.sty(cfg.red).Render("boom")
	if got := errM.footer(); !strings.Contains(got, wantErr) {
		t.Errorf("footer() = %q, want it to contain the c.red-rendered flash %q", got, wantErr)
	}

	okM := model{cfg: cfg, width: 80, height: 18, flash: "opened ↗", flashIsError: false}
	wantOK := m.sty(cfg.green).Render("opened ↗")
	if got := okM.footer(); !strings.Contains(got, wantOK) {
		t.Errorf("footer() = %q, want it to contain the c.green-rendered flash %q", got, wantOK)
	}
}

// TestFlashDeadline is the mutation-check target for the tick's flash expiry:
// a flash must survive a tick before its deadline and be gone after it.
func TestFlashDeadline(t *testing.T) {
	notYet := model{cfg: testCfg(), width: 60, height: 18, flash: "still here", flashUntil: time.Now().Add(time.Hour)}
	if m, _ := notYet.Update(tickMsg{}); m.(model).flash != "still here" {
		t.Errorf("flash before its deadline must survive a tick, got %q", m.(model).flash)
	}

	expired := model{cfg: testCfg(), width: 60, height: 18, flash: "gone now", flashUntil: time.Now().Add(-time.Second)}
	if m, _ := expired.Update(tickMsg{}); m.(model).flash != "" {
		t.Errorf("flash past its deadline must clear on the next tick, got %q", m.(model).flash)
	}
}

func TestIssueStampArgs(t *testing.T) {
	if got, want := issueStampArgs("$0:@0", "/repo", "feat/x", ""), []string{"$0:@0", "/repo", "feat/x"}; !slices.Equal(got, want) {
		t.Errorf("issueStampArgs with no explicit id = %v, want %v", got, want)
	}
	if got, want := issueStampArgs("$0:@0", "/repo", "main", "GH-42"), []string{"$0:@0", "/repo", "main", "GH-42"}; !slices.Equal(got, want) {
		t.Errorf("issueStampArgs with explicit id = %v, want %v", got, want)
	}
}

func TestCardIssueNoURLShowsStampError(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		issueID: "ENG-9001", issueStampError: "No API key configured", branch: "b",
	}}
	out := render(m)
	if !strings.Contains(out, "no url — No API key configured") {
		t.Errorf("expected stamp error reason in card\n%s", out)
	}
}

func TestCardIssueNoURLFallsBackToGenericReason(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		issueID: "ENG-9001", branch: "b",
	}}
	out := render(m)
	if !strings.Contains(out, "no url — stamp failed") {
		t.Errorf("expected generic stamp-failed reason in card\n%s", out)
	}
}

func TestHandleKeyOpenIssueWithNoURLFlashesReason(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		issueID: "ENG-9001", issueStampError: "No API key configured", branch: "b",
	}}
	m2, cmd := m.handleKey("o")
	if cmd != nil {
		t.Error("o on an issue with no url must not dispatch xdg-open")
	}
	if got, want := m2.(model).flash, "no url — No API key configured"; got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
	if !m2.(model).flashIsError {
		t.Error("the no-url flash must be marked as an error")
	}
}

func TestOpenDoneMsgFlashesFailure(t *testing.T) {
	base := model{cfg: testCfg(), width: 60, height: 18}
	m, cmd := base.Update(openDoneMsg{errText: "no such file"})
	if cmd != nil {
		t.Error("openDoneMsg handling should not dispatch a further cmd")
	}
	if got, want := m.(model).flash, "open failed: no such file"; got != want {
		t.Errorf("flash = %q, want %q", got, want)
	}
	if !m.(model).flashIsError {
		t.Error("an open failure flash must be marked as an error")
	}
}

// TestBranchBlockLongBranchStaysInsideCardWidth: branchBlock() must truncate
// the "branch → base" head line — it renders raw window/git data with no
// upper bound, and previously skipped truncate() entirely.
func TestBranchBlockLongBranchStaysInsideCardWidth(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, baseBranch: "main", win: winState{
		branch: "feat/768-enrichcard-truncate-by-display-width-not-and-then-some-more",
	}}
	out := render(m)
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("rendered line exceeds card width %d (got %d): %q", m.width, w, line)
		}
	}
}

// TestFooterLongFlashStaysInsideCardWidth: footer() must truncate m.flash to
// the room actually left on its row, not the full panel width, or a long
// flash (an exec error, a CLI stderr line) overflows the card's border.
func TestFooterLongFlashStaysInsideCardWidth(t *testing.T) {
	cases := []struct {
		name  string
		flash string
	}{
		{
			name:  "ascii",
			flash: `open failed: exec: "xdg-open": executable file not found in $PATH`,
		},
		{
			// 8 leading 2-cell emoji so the old rune-count cut (which counts
			// each emoji as one rune) lands inside them, well short of the
			// display width it thinks it consumed.
			name:  "emoji",
			flash: "🚨🚨🚨🚨🚨🚨🚨🚨 open failed: xdg-open not found",
		},
		{
			name:  "cjk",
			flash: "打开失败: 找不到可执行文件 xdg-open 在 $PATH 中",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := model{cfg: testCfg(), width: 60, height: 18, win: winState{branch: "b"}}
			m.flash = tc.flash
			out := render(m)
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > m.width {
					t.Errorf("rendered line exceeds card width %d (got %d): %q", m.width, w, line)
				}
			}
		})
	}
}

// TestTruncateWidthAware: truncate() must cut by display width (lipgloss.Width
// of the result), not by rune count — a 2-cell rune sitting on an odd max
// boundary must not push the result over max cells.
func TestTruncateWidthAware(t *testing.T) {
	inputs := []string{
		"hello world",
		"a打开失败",
		"打开失败: 找不到可执行文件",
		"🚨🚨🚨🚨🚨🚨🚨🚨 open failed",
	}
	for _, s := range inputs {
		for max := 3; max <= 12; max++ {
			t.Run(s+"/"+strconv.Itoa(max), func(t *testing.T) {
				got := truncate(s, max)
				if w := lipgloss.Width(got); w > max {
					t.Errorf("truncate(%q, %d) = %q, width %d > max %d", s, max, got, w, max)
				}
			})
		}
	}

	// Exact case: rune-count cut lets a wide rune push the result past max.
	if got := truncate("a打开失败", 4); lipgloss.Width(got) > 4 {
		t.Errorf("truncate(%q, 4) = %q, width %d exceeds 4", "a打开失败", got, lipgloss.Width(got))
	}

	// ASCII-only spot check: current behavior is unchanged by the fix.
	if got := truncate("hello world", 8); got != "hello w…" {
		t.Errorf("truncate(%q, 8) = %q, want %q", "hello world", got, "hello w…")
	}
}

func TestShouldStampIssue(t *testing.T) {
	full := cfg{issueStampBin: "/bin/true"}
	win := winState{branch: "feat/x"}

	cases := []struct {
		name string
		c    cfg
		w    winState
		dir  string
		want bool
	}{
		{"all present", full, win, "/repo", true},
		{"no issue-stamp binary", cfg{}, win, "/repo", false},
		{"no branch", full, winState{}, "/repo", false},
		{"no dir (reclaimed worktree)", full, win, "", false},
	}
	for _, tc := range cases {
		if got := shouldStampIssue(tc.c, tc.w, tc.dir); got != tc.want {
			t.Errorf("%s: shouldStampIssue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCardPendingPieAndProgress(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		prNumber: "103", prState: "open", prCheck: "pending", prProgress: "3/8", branch: "b"}}
	out := render(m)
	if !strings.Contains(out, enrichstate.PieSlices[2]+" #103") {
		t.Errorf("expected pie badge %q\n%s", enrichstate.PieSlices[2]+" #103", out)
	}
	if !strings.Contains(out, "3/8 checks") {
		t.Errorf("expected progress text\n%s", out)
	}
}

func TestCardPendingWithoutProgressKeepsGlyph(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		prNumber: "103", prState: "open", prCheck: "pending", branch: "b"}}
	out := render(m)
	if !strings.Contains(out, "P #103") || strings.Contains(out, "checks") {
		t.Errorf("expected plain pending badge and no progress text\n%s", out)
	}
}

// TestCardMaxWidthBackstop: card()'s outer style must clamp content to
// m.width even when a block skips truncate() entirely — claudeBlock()
// renders w.task with no upper bound today, exactly the scenario #773's
// backstop guards against. Every rendered line must stay within m.width, and
// the border must stay intact on both edges rather than get clipped by a
// naive MaxWidth on the bordered style itself.
func TestCardMaxWidthBackstop(t *testing.T) {
	m := model{cfg: testCfg(), width: 60, height: 18, win: winState{
		branch: "b",
		task:   strings.Repeat("x", 200),
	}}
	out := render(m)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected a bordered multi-line card, got %d lines: %q", len(lines), out)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > m.width {
			t.Errorf("line %d exceeds card width %d (got %d): %q", i, m.width, w, line)
		}
	}
	first, last := lines[0], lines[len(lines)-1]
	if !strings.HasPrefix(first, "╭") || !strings.HasSuffix(first, "╮") {
		t.Errorf("top border missing or clipped: %q", first)
	}
	if !strings.HasPrefix(last, "╰") || !strings.HasSuffix(last, "╯") {
		t.Errorf("bottom border missing or clipped: %q", last)
	}
	for i, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
			t.Errorf("row %d missing side border: %q", i+1, line)
		}
	}
}

// TestCardNoRoomForFrameStaysWithinWidth: at or below the border+padding
// frame size (startup before the first WindowSizeMsg, or an absurdly small
// popup), lipgloss treats MaxWidth(0)/MaxHeight(0) as "unset" rather than
// "clamp to zero" — without card()'s own guard for this case, the backstop
// would silently disengage exactly where it matters most.
func TestCardNoRoomForFrameStaysWithinWidth(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
	}{
		{"zero size (pre-WindowSizeMsg)", 0, 0},
		{"width at frame size", 4, 18},
		{"height at frame size", 60, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := model{cfg: testCfg(), width: tc.width, height: tc.height, win: winState{
				branch: "b",
				task:   strings.Repeat("x", 200),
			}}
			out := render(m)
			for _, line := range strings.Split(out, "\n") {
				if w := lipgloss.Width(line); w > tc.width {
					t.Errorf("line exceeds width %d (got %d): %q", tc.width, w, line)
				}
			}
		})
	}
}
