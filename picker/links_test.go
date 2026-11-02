package main

import (
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/noamsto/tmux-og/picker/linkjoin"
)

func testLinksModel(links ...linkjoin.Link) linksModel {
	m := newLinksModel(links, map[string]string{}, "mocha")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	return next.(linksModel)
}

func linksKey(t *testing.T, m linksModel, key tea.KeyPressMsg) linksModel {
	t.Helper()
	next, _ := m.handleKey(key)
	return next.(linksModel)
}

func TestLinksTypedSpaceReachesQuery(t *testing.T) {
	m := testLinksModel(linkjoin.Link{Text: "/tmp/a", Kind: linkjoin.Path, Rows: 1})
	m = linksKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})
	if m.query != " " {
		t.Fatalf("query = %q, want a single space", m.query)
	}
}

func TestLinksEnterOpensURLCopiesPath(t *testing.T) {
	tests := []struct {
		name string
		link linkjoin.Link
		want linksAction
	}{
		{"https url", linkjoin.Link{Text: "https://x.io/a", Kind: linkjoin.URL, Rows: 1}, linksOpen},
		{"file url", linkjoin.Link{Text: "file:///tmp/a", Kind: linkjoin.URL, Rows: 1}, linksOpen},
		{"path", linkjoin.Link{Text: "/tmp/a", Kind: linkjoin.Path, Rows: 1}, linksCopy},
		{"git@ url", linkjoin.Link{Text: "git@github.com:o/r.git", Kind: linkjoin.URL, Rows: 1}, linksCopy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := linksKey(t, testLinksModel(tt.link), tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.action != tt.want || m.picked != tt.link {
				t.Errorf("action, picked = %v, %+v; want %v, %+v", m.action, m.picked, tt.want, tt.link)
			}
		})
	}
}

func TestLinksCtrlYCopies(t *testing.T) {
	link := linkjoin.Link{Text: "https://x.io/a", Kind: linkjoin.URL, Rows: 1}
	m := linksKey(t, testLinksModel(link), tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if m.action != linksCopy || m.picked != link {
		t.Errorf("action, picked = %v, %+v; want copy of %+v", m.action, m.picked, link)
	}
}

func TestLinksCursorAndQuit(t *testing.T) {
	m := testLinksModel(
		linkjoin.Link{Text: "/tmp/a", Kind: linkjoin.Path, Rows: 1},
		linkjoin.Link{Text: "/tmp/b", Kind: linkjoin.Path, Rows: 1},
	)
	m = linksKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = linksKey(t, m, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if m.cursor != 1 {
		t.Fatalf("cursor = %d after down, ctrl+n; want 1 (clamped)", m.cursor)
	}
	m = linksKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.picked.Text != "/tmp/b" {
		t.Errorf("picked = %q, want /tmp/b", m.picked.Text)
	}

	m = linksKey(t, testLinksModel(linkjoin.Link{Text: "/tmp/a", Kind: linkjoin.Path, Rows: 1}), tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.action != linksNone {
		t.Errorf("action after esc = %v, want none", m.action)
	}
}

func TestLinksRowsFitWidth(t *testing.T) {
	m := testLinksModel(
		linkjoin.Link{Text: "https://x.io/日本", Kind: linkjoin.URL, Rows: 1},
		linkjoin.Link{Text: "https://example.com/" + strings.Repeat("long/", 30), Kind: linkjoin.URL, Rows: 3},
		linkjoin.Link{Text: "/tmp/" + strings.Repeat("日本", 30), Kind: linkjoin.Path, Rows: 2},
	)
	for _, cursor := range []int{0, 1, 2} {
		m.cursor = cursor
		for i, line := range strings.Split(m.View().Content, "\n") {
			if w := visibleWidth(line); w > m.width {
				t.Errorf("cursor %d row %d is %d cells, wider than %d: %q", cursor, i, w, m.width, line)
			}
		}
	}
}

func TestLinksJoinedMarker(t *testing.T) {
	m := testLinksModel(
		linkjoin.Link{Text: "/tmp/joined", Kind: linkjoin.Path, Rows: 3},
		linkjoin.Link{Text: "/tmp/plain", Kind: linkjoin.Path, Rows: 1},
	)
	rows := m.renderBody()
	if !strings.Contains(stripANSI(rows[0]), "/tmp/joined +3") {
		t.Errorf("joined row %q lacks the +3 marker", stripANSI(rows[0]))
	}
	if strings.Contains(stripANSI(rows[1]), "+") {
		t.Errorf("plain row %q has a marker", stripANSI(rows[1]))
	}
}

func TestLinksFilterRanks(t *testing.T) {
	m := testLinksModel(
		linkjoin.Link{Text: "/tmp/axbxc", Kind: linkjoin.Path, Rows: 1},
		linkjoin.Link{Text: "/tmp/other", Kind: linkjoin.Path, Rows: 1},
		linkjoin.Link{Text: "/tmp/abc", Kind: linkjoin.Path, Rows: 1},
	)
	if want := []int{0, 1, 2}; !reflect.DeepEqual(m.visible, want) {
		t.Fatalf("empty query visible = %v, want %v", m.visible, want)
	}
	for _, r := range "abc" {
		m = linksKey(t, m, tea.KeyPressMsg{Code: r})
	}
	if want := []int{2, 0}; !reflect.DeepEqual(m.visible, want) {
		t.Errorf("query abc visible = %v, want %v (tighter match first, non-match dropped)", m.visible, want)
	}
	m = linksKey(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.query != "ab" {
		t.Errorf("query after backspace = %q, want ab", m.query)
	}
}

func TestLinksEmptyState(t *testing.T) {
	m := testLinksModel()
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "no links in this pane") {
		t.Errorf("view lacks the empty-state line:\n%s", view)
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'y', Mod: tea.ModCtrl}} {
		if got := linksKey(t, m, key); got.action != linksNone {
			t.Errorf("%v with no links set action %v, want none", key, got.action)
		}
	}
}

func TestLinksCaptureArgv(t *testing.T) {
	want := []string{
		"display-message", "-p", "-t", "%5", "#{pane_width}",
		";", "capture-pane", "-p", "-J", "-e", "-t", "%5", "-S", "-2000",
	}
	if got := linksCaptureArgv("%5"); !reflect.DeepEqual(got, want) {
		t.Errorf("linksCaptureArgv = %q, want %q", got, want)
	}
}

func TestParseLinksOutput(t *testing.T) {
	width, capture, err := parseLinksOutput("62\nfirst row\nsecond row\n")
	if err != nil || width != 62 || capture != "first row\nsecond row\n" {
		t.Errorf("parseLinksOutput = %d, %q, %v", width, capture, err)
	}
	for _, bad := range []string{"", "x\nrow\n", "0\nrow\n", "-3\nrow\n"} {
		if _, _, err := parseLinksOutput(bad); err == nil {
			t.Errorf("parseLinksOutput(%q) succeeded, want an error", bad)
		}
	}
}

func mustLinksActionCmd(t *testing.T, action linksAction, text, origin, client string) (*exec.Cmd, string) {
	t.Helper()
	cmd, verb := linksActionCmd(action, text, origin, client)
	if cmd == nil {
		t.Fatalf("linksActionCmd(%v) built no command", action)
	}
	return cmd, verb
}

func TestLinksActionCmd(t *testing.T) {
	open, verb := mustLinksActionCmd(t, linksOpen, "https://x.io/a", "%5", "client0")
	if verb != "open" || !reflect.DeepEqual(open.Args, []string{"og-open", "https://x.io/a"}) {
		t.Errorf("open = %q (%s)", open.Args, verb)
	}
	if open.SysProcAttr == nil || !open.SysProcAttr.Setsid {
		t.Error("open does not run in its own session")
	}
	if !slices.Contains(open.Env, "TMUX_PANE=%5") {
		t.Errorf("open env lacks TMUX_PANE=%%5")
	}

	copyCmd, verb := mustLinksActionCmd(t, linksCopy, "/tmp/a", "%5", "client0")
	if want := []string{"tmux", "set-buffer", "-w", "-t", "client0", "--", "/tmp/a"}; verb != "copy" || !reflect.DeepEqual(copyCmd.Args, want) {
		t.Errorf("copy = %q (%s), want %q", copyCmd.Args, verb, want)
	}
	noClient, _ := mustLinksActionCmd(t, linksCopy, "/tmp/a", "%5", "")
	if want := []string{"tmux", "set-buffer", "-w", "--", "/tmp/a"}; !reflect.DeepEqual(noClient.Args, want) {
		t.Errorf("copy without client = %q, want %q", noClient.Args, want)
	}

	if cmd, _ := linksActionCmd(linksNone, "/tmp/a", "%5", ""); cmd != nil {
		t.Errorf("none built %q", cmd.Args)
	}
}

func TestLinksFailArgv(t *testing.T) {
	got := linksFailArgv("client0", "open", "bad\x1b[31m thing #{x}\n")
	want := []string{"display-message", "-c", "client0", "links: open failed: bad thing ##{x}"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linksFailArgv = %q, want %q", got, want)
	}
	if got := linksFailArgv("", "copy", "boom"); !reflect.DeepEqual(got, []string{"display-message", "links: copy failed: boom"}) {
		t.Errorf("linksFailArgv without client = %q", got)
	}
}
