package main

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// benchModel is a ready session-mode model with 40 sessions and a full
// preview, the shape of a busy popup.
func benchModel(b *testing.B, windowMode bool) tuiModel {
	b.Helper()
	var items []listItem
	if windowMode {
		windows := make([]windowData, 0, 40)
		for i := 0; i < 40; i++ {
			windows = append(windows, windowData{session: fmt.Sprintf("s%d", i/5), index: i % 5, name: fmt.Sprintf("win%d", i), branch: fmt.Sprintf("feat/%d-work", i)})
		}
		items = renderWindowItemsWith(windows, map[string]int64{}, map[string]string{}, nil, "dark", 0, false)
	} else {
		snap := make(panesSnapshot, 0, 40)
		for i := 0; i < 40; i++ {
			snap = append(snap, fmt.Sprintf("%%%d|sess%d|0|/home/u/proj%d|%d|||fish|%d|||", i, i, i, 1000+i, 100+i))
		}
		items = buildSessionItems(map[string]string{}, snap, nil, "dark", false, "")
	}
	m := newPickerModel(windowMode, false, false, map[string]string{}, "dark", items, "")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = next.(tuiModel)
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "\x1b[32mline %d\x1b[0m  some build output that fills a preview row\x1b[49m\n", i)
	}
	next, _ = m.Update(previewMsg{target: m.currentTarget(), content: strings.TrimRight(sb.String(), "\n")})
	return next.(tuiModel)
}

func BenchmarkView(b *testing.B) {
	for _, mode := range []struct {
		name   string
		window bool
	}{{"session", false}, {"window", true}} {
		b.Run(mode.name, func(b *testing.B) {
			m := benchModel(b, mode.window)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

func BenchmarkUpdateDown(b *testing.B) {
	for _, mode := range []struct {
		name   string
		window bool
	}{{"session", false}, {"window", true}} {
		b.Run(mode.name, func(b *testing.B) {
			m := benchModel(b, mode.window)
			down, up := tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyUp}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := down
				if i%2 == 1 {
					key = up
				}
				next, _ := m.Update(key)
				m = next.(tuiModel)
			}
		})
	}
}

func BenchmarkUpdateTypeAndErase(b *testing.B) {
	m := benchModel(b, false)
	a, bs := tea.KeyPressMsg{Code: 'a', Text: "a"}, tea.KeyPressMsg{Code: tea.KeyBackspace}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := a
		if i%2 == 1 {
			key = bs
		}
		next, _ := m.Update(key)
		m = next.(tuiModel)
	}
}

// BenchmarkRefreshMsg is the 1s tick's Update work: the model rebuilds its
// rows and the filter, on the same goroutine that reads keys.
func BenchmarkRefreshMsg(b *testing.B) {
	for _, mode := range []struct {
		name   string
		window bool
	}{{"session", false}, {"window", true}} {
		b.Run(mode.name, func(b *testing.B) {
			m := benchModel(b, mode.window)
			msg := refreshMsg{items: m.sessionItems}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				next, _ := m.Update(msg)
				m = next.(tuiModel)
			}
		})
	}
}
