package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type tip struct {
	Key   string
	Label string
}

type frameMsg struct{}
type timeoutMsg struct{}

type model struct {
	theme         string
	tips          []tip
	prefix        string
	timeoutSec    int
	static        bool
	gradient      []string
	frame         int
	width, height int
	deck          []artGrid // breathing + drifting-z loop
	small         artGrid   // single static frame for small viewports
}

func newModel(theme string, tips []tip, prefix string, timeoutSec int, static bool) model {
	anchors, ok := gradientAnchors[theme]
	if !ok {
		anchors = gradientAnchors["dark"]
	}
	m := model{
		theme:      theme,
		tips:       tips,
		prefix:     prefix,
		timeoutSec: timeoutSec,
		static:     static,
		gradient:   buildGradient(anchors, 48),
		deck:       loadDeck(),
		small:      parseArt(catSmall),
	}
	if static {
		// Past the dissolve-in threshold so colorizeArt renders the small
		// frame fully resolved from the first paint, with no reveal noise.
		m.frame = introFrames
	}
	return m
}

// deckStep is ticks per deck frame: 2 * 50ms = 100ms ≈ the source loop rate
// (100 frames over ~10s). The plasma shimmer still updates every tick.
const deckStep = 2

// currentFrame is the deck frame for the current tick (loops); falls back to
// the small frame when the deck is empty.
func (m model) currentFrame() artGrid {
	if len(m.deck) == 0 {
		return m.small
	}
	return m.deck[(m.frame/deckStep)%len(m.deck)]
}

func frameCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{} })
}

func timeoutCmd(sec int) tea.Cmd {
	return tea.Tick(time.Duration(sec)*time.Second, func(time.Time) tea.Msg { return timeoutMsg{} })
}

func (m model) Init() tea.Cmd {
	if m.static {
		// No periodic redraw loop at all — the frame is frozen, so there is
		// nothing for a tick to change.
		if m.timeoutSec <= 0 {
			return nil
		}
		return timeoutCmd(m.timeoutSec)
	}
	if m.timeoutSec <= 0 {
		return frameCmd() // no auto-dismiss (on-demand launch)
	}
	return tea.Batch(frameCmd(), timeoutCmd(m.timeoutSec))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m, tea.Quit
	case timeoutMsg:
		return m, tea.Quit
	case frameMsg:
		m.frame++
		return m, frameCmd()
	}
	return m, nil
}

func (m model) accent() string {
	if m.theme == "light" {
		return "#7287fd"
	}
	return "#b4befe"
}

func (m model) subtext() string {
	if m.theme == "light" {
		return "#6c6f85"
	}
	return "#a6adc8"
}

// introFrames is the dissolve-in length (~1s at 50ms/frame): each glyph starts
// as braille static and resolves into the real art at a hash-staggered frame.
const introFrames = 20

// brailleStatic returns a random-ish braille pattern (U+2800 + 8-bit dot mask)
// for the dissolve-in, so the "assembling" noise matches the braille art.
func brailleStatic(x, y, frame int) rune {
	return rune(0x2800 + int(cellHash(x, y, frame)*256)) //nolint:gosec // value is bounded to a single braille byte
}

// colorizeArt renders the mascot: a dissolve-in intro (braille static settling
// into the art), then a plasma field driving both gradient color and brightness
// per glyph (organic shimmer rather than a linear color stripe).
func (m model) colorizeArt(a artGrid) string {
	t := float64(m.frame) * 0.11
	var sb strings.Builder
	for y, line := range a.lines {
		x := 0
		for _, r := range line {
			if r == ' ' {
				sb.WriteRune(' ')
				x++
				continue
			}
			v := plasma(x, y, t)
			glyph := r
			if revealAt := cellHash(x, y, 0) * introFrames; float64(m.frame) < revealAt {
				glyph = brailleStatic(x, y, m.frame)
				v *= 0.6
			}
			hex := shade(m.gradient[int(v*float64(len(m.gradient)-1))], v)
			sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(glyph)))
			x++
		}
		if y < len(a.lines)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func (m model) renderTips() string {
	if len(m.tips) == 0 {
		return ""
	}
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.accent())).Bold(true)
	lblStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.subtext()))

	keyOf := func(t tip) string { return strings.ReplaceAll(t.Key, "prefix", m.prefix) }
	// Fixed key and label column widths so both halves line up in a grid.
	keyW, labelW := 0, 0
	for _, t := range m.tips {
		if n := lipgloss.Width(keyOf(t)); n > keyW {
			keyW = n
		}
		if n := lipgloss.Width(t.Label); n > labelW {
			labelW = n
		}
	}

	half := (len(m.tips) + 1) / 2
	var rows []string
	for i := range half {
		left := m.tipCell(m.tips[i], keyOf, keyStyle, lblStyle, keyW, labelW)
		right := ""
		if j := i + half; j < len(m.tips) {
			right = m.tipCell(m.tips[j], keyOf, keyStyle, lblStyle, keyW, labelW)
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, left, "    ", right))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m model) tipCell(t tip, keyOf func(tip) string, keyStyle, lblStyle lipgloss.Style, keyW, labelW int) string {
	key := keyStyle.Width(keyW).Render(keyOf(t))
	label := lblStyle.Width(labelW).Render(t.Label)
	return lipgloss.JoinHorizontal(lipgloss.Top, key, "  ", label)
}

// selectedFrame is the artGrid render() will colorize this tick: the small
// single-frame fallback in static mode (the bandwidth-light remote path),
// otherwise the animated deck frame if it fits the popup, falling back to
// the small frame.
func (m model) selectedFrame() (artGrid, bool) {
	if m.static {
		return m.small, true
	}
	if cur := m.currentFrame(); fits(cur, m.width, m.height) {
		return cur, true
	}
	if fits(m.small, m.width, m.height) {
		return m.small, true
	}
	return artGrid{}, false
}

func (m model) render() string {
	var blocks []string
	if frame, ok := m.selectedFrame(); ok {
		blocks = append(blocks, m.colorizeArt(frame))
	}
	wordmark := lipgloss.NewStyle().Foreground(lipgloss.Color(m.accent())).Bold(true).Render("l a z y t m u x")
	blocks = append(blocks, wordmark, "")
	if tips := m.renderTips(); tips != "" {
		blocks = append(blocks, tips, "")
	}
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color(m.subtext())).Italic(true).Render("press any key to dismiss")
	blocks = append(blocks, hint)
	return lipgloss.JoinVertical(lipgloss.Center, blocks...)
}

func (m model) View() tea.View {
	w, h := m.width, m.height
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	content := lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, m.render())
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}
