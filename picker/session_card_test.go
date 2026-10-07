package main

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func cardWin(idx int, name string, mod func(*windowData)) cardWindow {
	w := windowData{session: "proj", index: idx, name: name, procs: []string{"nvim"}, branch: "main"}
	if mod != nil {
		mod(&w)
	}
	return cardWindow{w: w, activity: 1000 - 17*60 + 60}
}

func renderTestCard(c sessionCard, width int) []string {
	c.name = "proj"
	return strings.Split(stripANSI(renderSessionCard(c, nil, "dark", width, 40, 1060)), "\n")
}

func assertNoWiderThan(t *testing.T, lines []string, width int) {
	t.Helper()
	for _, l := range lines {
		if w := visibleWidth(l); w > width {
			t.Errorf("line is %d cells, width %d: %q", w, width, l)
		}
	}
}

func TestSessionCardAlignsColumnsAcrossWideGlyphs(t *testing.T) {
	c := sessionCard{windows: []cardWindow{
		cardWin(1, "editor", nil),
		cardWin(2, "日本語ウィンドウ", func(w *windowData) { w.branch = "feat/412-pin"; w.prPlain = " # #418" }),
		cardWin(3, "logs", func(w *windowData) { w.procs = []string{"nvim", "claude"}; w.branch = "fix/logs" }),
	}}
	lines := renderTestCard(c, 100)
	var rows []string
	for _, l := range lines {
		if strings.HasSuffix(strings.TrimRight(l, " "), "17m") {
			rows = append(rows, l)
		}
	}
	if len(rows) < 3 {
		t.Fatalf("expected 3 window rows, got %q", lines)
	}
	// The age column right-aligns: every row ends at the same cell.
	end := visibleWidth(strings.TrimRight(rows[0], " "))
	for _, r := range rows {
		if got := visibleWidth(strings.TrimRight(r, " ")); got != end {
			t.Errorf("row ends at %d, want %d: %q", got, end, r)
		}
	}
	assertNoWiderThan(t, lines, 100)
}

func TestSessionCardNeverExceedsNarrowWidth(t *testing.T) {
	c := sessionCard{
		path: "/very/long/path/that/keeps/going/and/going",
		windows: []cardWindow{
			cardWin(1, "a-rather-long-window-name", func(w *windowData) {
				w.branch = "feat/412-a-rather-long-branch-name"
				w.labelID = "gh #412"
				w.prPlain = " # #418"
			}),
			cardWin(2, "x", func(w *windowData) { w.agent = agentCounts{waiting: 1} }),
		},
		capture: strings.Repeat("wide line ", 20) + "\nsecond",
	}
	for _, width := range []int{12, 24, 40, 60} {
		assertNoWiderThan(t, renderTestCard(c, width), width)
	}
}

func TestSessionCardSingleWindowHasNoActiveMarker(t *testing.T) {
	c := sessionCard{windows: []cardWindow{cardWin(1, "solo", func(w *windowData) { w.active = true })}}
	out := strings.Join(renderTestCard(c, 80), "\n")
	if strings.Contains(out, "▸") {
		t.Errorf("a one-window session marks nothing active: %q", out)
	}
	if !strings.Contains(out, "1 window") || strings.Contains(out, "1 windows") {
		t.Errorf("header must say 1 window: %q", out)
	}
}

func TestSessionCardMirrorRowUsesBridgeLabels(t *testing.T) {
	c := sessionCard{windows: []cardWindow{
		cardWin(1, "mirror", func(w *windowData) {
			w.bridgeWin = true
			w.branch = ""
			w.bridgeName = "remote-issue"
			w.procs = []string{"btop"}
		}),
	}}
	out := strings.Join(renderTestCard(c, 80), "\n")
	if !strings.Contains(out, "remote-issue") {
		t.Errorf("mirror row should show the bridge name: %q", out)
	}
}

func TestSessionCardBlockedAgentGetsDetailLine(t *testing.T) {
	cw := cardWin(2, "dispatcher", func(w *windowData) { w.agent = agentCounts{waiting: 1} })
	cw.detail = "Waiting · nix build"
	c := sessionCard{windows: []cardWindow{cardWin(1, "main", nil), cw}}
	lines := renderTestCard(c, 80)
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "dispatcher") })
	if i < 0 || i+1 >= len(lines) || !strings.Contains(lines[i+1], "Waiting · nix build") {
		t.Fatalf("detail line must follow its window row: %q", lines)
	}
	if strings.Index(lines[i+1], "Waiting") <= strings.Index(lines[i], "dispatcher") {
		t.Errorf("detail should be indented under the row: %q", lines[i:i+2])
	}
}

func TestSessionCardSanitizesHostileText(t *testing.T) {
	hostile := "evil\x1b]52;c;AAAA\x07\x1b[2J\u009bname"
	cw := cardWin(1, hostile, func(w *windowData) {
		w.branch = hostile
		w.labelID = hostile
		w.bridgeHost = hostile
	})
	cw.detail = hostile
	c := sessionCard{path: hostile, windows: []cardWindow{cw}, capture: "ok\x1b]0;title\x07 line"}
	out := renderSessionCard(c, nil, "dark", 80, 40, 1060)
	for _, bad := range []string{"\x1b]", "\x1b[2J", "\u009b", "\x07"} {
		if strings.Contains(out, bad) {
			t.Errorf("output carries %q: %q", bad, out)
		}
	}
}

func TestSessionCardTailKeepsBackgroundReset(t *testing.T) {
	c := sessionCard{windows: []cardWindow{cardWin(1, "w", nil)}, capture: "one\ntwo"}
	out := renderSessionCard(c, nil, "dark", 40, 40, 1060)
	if !strings.HasSuffix(out, "\033[49m") {
		t.Errorf("each tail line ends with the background reset: %q", out)
	}
}

func TestSessionCardZeroWidthRendersNothing(t *testing.T) {
	if got := renderSessionCard(sessionCard{}, nil, "dark", 0, 10, 0); got != "" {
		t.Errorf("zero width must render nothing, got %q", got)
	}
}

func cardRow(sess string, idx int, name, pane string, extra ...string) string {
	f := make([]string, 38)
	f[0], f[1], f[2], f[3], f[4], f[5] = sess, strconv.Itoa(idx), name, "0", "zsh", "1"
	f[37] = "@1"
	return strings.Join(append(f, append([]string{"1000", pane}, extra...)...), "|")
}

func TestParseSessionCardDropsRowsWithStrayPipes(t *testing.T) {
	good := cardRow("proj", 1, "good", "%1")
	bad := strings.Replace(cardRow("proj", 2, "bad", "%2"), "|bad|", "|ba|d|", 1)
	out := "\n" + good + "\n" + bad + "\n" + cardSep + "\n/home/x\n" + cardSep + "\ncaptured\n" + cardSep + "\nstill capture"
	card, panes, ok := parseSessionCard(out[1:])
	if !ok || len(card.windows) != 1 || card.windows[0].w.name != "good" {
		t.Fatalf("only the intact row survives: ok=%v card=%+v", ok, card)
	}
	if card.path != "/home/x" {
		t.Errorf("path = %q", card.path)
	}
	if !strings.Contains(card.capture, "still capture") || !strings.Contains(card.capture, cardSep) {
		t.Errorf("a separator inside the capture must stay in it: %q", card.capture)
	}
	if _, ok := panes["1"]; !ok || len(panes) != 1 {
		t.Errorf("pane map = %v", panes)
	}
}

func TestSessionCardArgvIsOneInvocation(t *testing.T) {
	argv := sessionCardArgv("proj", "%9")
	count := func(cmd string) int {
		n := 0
		for i, a := range argv {
			if a == cmd && (i == 0 || argv[i-1] == ";") {
				n++
			}
		}
		return n
	}
	if count("list-panes") != 1 || count("capture-pane") != 1 {
		t.Errorf("want one list-panes and one capture-pane: %v", argv)
	}
	if !slices.Contains(argv, "=proj") || !slices.Contains(argv, notModalFilter) {
		t.Errorf("session must be addressed exactly and modal panes filtered: %v", argv)
	}
}

func TestLoadPreviewRoutesSessionRowsThroughOneTmuxCall(t *testing.T) {
	origRun, origCap := cardRun, previewCapture
	t.Cleanup(func() { cardRun, previewCapture = origRun, origCap })
	var calls [][]string
	cardRun = func(argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return []byte("\n" + cardRow("proj", 1, "w", "%1") + "\n" + cardSep + "\n/p\n" + cardSep + "\ntail"), nil
	}
	captures := 0
	previewCapture = func(string) ([]byte, error) { captures++; return []byte("plain"), nil }

	m := sessionPreviewModel("proj")
	msg := m.loadPreviewCmd()()
	pm, ok := msg.(previewMsg)
	if !ok || !strings.Contains(stripANSI(pm.content), "1 window") || !pm.scrollTop {
		t.Fatalf("session row should preview as a card: %#v", msg)
	}
	if len(calls) != 1 || captures != 0 {
		t.Errorf("one tmux call, no separate capture: calls=%d captures=%d", len(calls), captures)
	}

	// A failed chain falls back to the plain capture.
	cardRun = func([]string) ([]byte, error) { return nil, errors.New("boom") }
	msg = m.loadPreviewCmd()()
	if pm, ok := msg.(previewMsg); !ok || !strings.Contains(pm.content, "plain") {
		t.Errorf("fallback should be the plain capture: %#v", msg)
	}

	// Window mode keeps the plain capture and never issues the card call.
	calls = nil
	m.windowMode = true
	m.loadPreviewCmd()()
	if len(calls) != 0 {
		t.Errorf("window mode must not build a card: %v", calls)
	}
}

func sessionPreviewModel(sess string) tuiModel {
	m := newPickerModel(false, false, false, map[string]string{}, "dark",
		[]listItem{{target: sess, session: sess}}, "")
	m.showPreview = true
	m.width, m.height = 100, 40
	m.cursor = m.firstSelectable(0)
	return m
}
