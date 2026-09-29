package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestComposeFrameMatchesLipgloss holds the fast composition to the lipgloss
// one byte for byte, across the states a popup is actually in.
func TestComposeFrameMatchesLipgloss(t *testing.T) {
	previews := map[string]string{
		"plain":           "one\ntwo\nthree",
		"ansi":            "\x1b[32mgreen\x1b[0m  text\x1b[49m\n\x1b[1mbold\x1b[0m",
		"trailing colour": "\x1b[48;5;236m      \x1b[0m\nhi   \x1b[0m",
		"wide glyphs":     "日本語のテキスト\n🙂 emoji 👨‍👩‍👧 family\ncafé",
		"overwide":        strings.Repeat("x", 500),
		"tab":             "a\tb",
		"control bytes":   "a\vb\nx\x00y\n\f\n\x7f\nu\u2028v\nc1\u0085",
		"empty":           "",
		"many lines":      strings.Repeat("line\n", 200),
	}
	sizes := [][2]int{{200, 50}, {120, 40}, {80, 24}, {40, 12}, {20, 8}}
	type build struct {
		name string
		make func() []listItem
		win  bool
	}
	builds := []build{
		{"sessions", func() []listItem {
			snap := panesSnapshot{}
			for i := 0; i < 30; i++ {
				snap = append(snap, fmt.Sprintf("%%%d|sess%d|0|/home/u/proj%d|%d|||fish|%d|||", i, i, i, 1000+i, 100+i))
			}
			snap = append(snap, "%90|日本語|0|/tmp/日本|5|||vim|900|||")
			return buildSessionItems(map[string]string{}, snap, nil, "dark", false, "sess3")
		}, false},
		{"windows", func() []listItem {
			var ws []windowData
			for i := 0; i < 30; i++ {
				ws = append(ws, windowData{session: fmt.Sprintf("s%d", i/5), index: i % 5, name: fmt.Sprintf("win%d", i), branch: fmt.Sprintf("feat/%d", i)})
			}
			ws = append(ws, windowData{session: "sé", index: 0, name: "日本語 🙂", branch: "ブランチ"})
			return renderWindowItemsWith(ws, map[string]int64{}, map[string]string{}, nil, "dark", 0, false)
		}, true},
	}
	for _, b := range builds {
		for _, sz := range sizes {
			for name, content := range previews {
				for _, variant := range []struct {
					query, host string
					preview     bool
					down        int
				}{
					{"", "", true, 0}, {"", "", false, 0}, {"s", "", true, 3}, {"zzzznomatch", "", true, 0},
					{strings.Repeat("longquery", 30), "", true, 1}, {"", "remote-host", true, 2},
				} {
					t.Run(fmt.Sprintf("%s/%dx%d/%s/q=%.6s/host=%s/preview=%v", b.name, sz[0], sz[1], name, variant.query, variant.host, variant.preview), func(t *testing.T) {
						m := newPickerModel(b.win, false, false, map[string]string{}, "dark", b.make(), "")
						next, _ := m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
						m = next.(tuiModel)
						m.showPreview = variant.preview
						m.emitHost = variant.host
						m.query = variant.query
						m = m.withFilter()
						m.cursor = m.firstSelectable(0)
						for i := 0; i < variant.down; i++ {
							m = m.moveCursor(1)
						}
						m.preview.SetContent(content)
						want := m.composeFrameLipgloss()
						if got := m.renderFrame(); got != want {
							t.Fatalf("renderFrame differs from the lipgloss composition\n got: %q\nwant: %q", got, want)
						}
					})
				}
			}
		}
	}
}

// TestComposeFrameTakesTheFastPath keeps the equivalence test honest: on an
// ordinary frame the fast composition must not be declining to the fallback.
func TestComposeFrameTakesTheFastPath(t *testing.T) {
	m := benchModel(&testing.B{}, false)
	if _, ok := m.composeFrame(); !ok {
		t.Fatal("composeFrame declined an ordinary 200x50 frame")
	}
	m.preview.SetContent("a\vb")
	if _, ok := m.composeFrame(); ok {
		t.Fatal("composeFrame accepted a preview with a vertical tab")
	}
}

func TestPadFrameLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		w    int
		want string
		ok   bool
	}{
		{"ab", 5, "ab   ", true},
		{"abcde", 5, "abcde", true},
		{"abcdef", 5, "", false},
		{"a\tb", 9, "", false},
		{"a\rb", 9, "", false},
		{"a\vb", 9, "", false},
		{"a\x7fb", 9, "", false},
		{"a\u2028b", 9, "", false},
		{"a\u0085b", 9, "", false},
		{"café", 6, "café  ", true},
		{"\x1b[31mab\x1b[0m", 4, "\x1b[31mab\x1b[0m  ", true},
		{"日本", 5, "日本 ", true},
	} {
		got, ok := padFrameLine(tc.line, tc.w)
		if got != tc.want || ok != tc.ok {
			t.Errorf("padFrameLine(%q, %d) = %q, %v; want %q, %v", tc.line, tc.w, got, ok, tc.want, tc.ok)
		}
	}
}
