package main

// Link picker popup (#1068): a standalone bubbletea model listing the URLs and
// paths in the invoking pane, with the hard line breaks Claude Code and pi
// leave inside a long token rejoined by linkjoin. Like which-key it shares no
// state with tuiModel and reuses only its package-level helpers.
//
// The model never executes: RunLinks performs the picked action after the
// program returns but before the process exits, which is what closes the
// popup. The opener runs in its own session so a cold-started browser outlives
// the popup's pty.

import (
	"errors"
	"fmt"
	imgcolor "image/color"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/noamsto/tmux-og/picker/linkjoin"
)

type linksAction int

const (
	linksNone linksAction = iota
	linksOpen
	linksCopy
)

// linksScrollback is how many history rows above the screen are scanned.
const linksScrollback = "-2000"

// linksOpenWait bounds how long the popup waits on the opener: xdg-open's
// generic path runs the browser in the foreground, so waiting for its exit
// would hold the popup open for as long as the browser lives.
const linksOpenWait = 2 * time.Second

type linksModel struct {
	links   []linkjoin.Link
	visible []int // indices into links surviving the query, in display order
	cursor  int

	query string

	width, height int
	ready         bool

	tmuxOpts map[string]string
	theme    string

	action linksAction
	picked linkjoin.Link
}

// --- Entry point ---

// RunLinks launches the link picker for OG_PICKER_ORIGIN_PANE.
func RunLinks() error {
	origin := os.Getenv("OG_PICKER_ORIGIN_PANE")
	client := os.Getenv("OG_PICKER_CLIENT")
	if origin == "" {
		return errors.New("links: OG_PICKER_ORIGIN_PANE unset")
	}

	out, err := exec.Command("tmux", linksCaptureArgv(origin)...).Output() //nolint:gosec // G204: fixed binary, argv passed without a shell
	if err != nil {
		return fmt.Errorf("links: capture %s: %w", origin, err)
	}
	width, capture, err := parseLinksOutput(string(out))
	if err != nil {
		return err
	}

	opts := readTmuxOpts()
	m := newLinksModel(linkjoin.Extract(capture, width), opts, themeFromOpts(opts))
	final, err := tea.NewProgram(m, programOptions()...).Run()
	if err != nil {
		return err
	}
	done, ok := final.(linksModel)
	if !ok {
		return nil
	}
	cmd, verb := linksActionCmd(done.action, done.picked.Text, origin, client)
	if cmd == nil {
		return nil
	}
	if detail, err := runLinksCmd(cmd, linksOpenWait); err != nil {
		if detail == "" {
			detail = err.Error()
		}
		// The popup is about to close, so the flash on the client is the report.
		_ = exec.Command("tmux", linksFailArgv(client, verb, detail)...).Run() //nolint:gosec // G204: fixed binary, argv passed without a shell
	}
	return nil
}

// linksCaptureArgv reads the pane width and its scrollback in one tmux call:
// the width is the first output line, the capture the rest.
func linksCaptureArgv(pane string) []string {
	return []string{
		"display-message", "-p", "-t", pane, "#{pane_width}",
		";", "capture-pane", "-p", "-J", "-e", "-t", pane, "-S", linksScrollback,
	}
}

// parseLinksOutput splits linksCaptureArgv's output. Extract divides by the
// width, so anything below one cell is refused.
func parseLinksOutput(out string) (width int, capture string, err error) {
	first, capture, _ := strings.Cut(out, "\n")
	width, err = strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0, "", fmt.Errorf("links: bad pane width %q: %w", first, err)
	}
	if width < 1 {
		return 0, "", fmt.Errorf("links: bad pane width %d", width)
	}
	return width, capture, nil
}

// linksActionCmd builds the command that carries out action on text, with the
// verb its failure message names; nil for linksNone.
func linksActionCmd(action linksAction, text, origin, client string) (cmd *exec.Cmd, verb string) {
	if action == linksOpen {
		// og-open routes through the pane's own bridge state, so it needs the
		// origin pane, not the popup's. Setsid: the popup's pty closes with the
		// picker and a cold-started browser must not get its SIGHUP.
		cmd = exec.Command("og-open", text) //nolint:gosec // G204: fixed binary, argv passed without a shell
		cmd.Env = append(os.Environ(), "TMUX_PANE="+origin)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd, "open"
	}
	if action == linksCopy {
		argv := []string{"set-buffer", "-w"}
		if client != "" {
			argv = append(argv, "-t", client)
		}
		return exec.Command("tmux", append(argv, "--", text)...), "copy" //nolint:gosec // G204: fixed binary, argv passed without a shell
	}
	return nil, ""
}

// linksFailArgv is the tmux argv flashing a failed action. The detail is
// sanitized, with '#' and '%' doubled so tmux expands neither as a format nor
// through strftime.
func linksFailArgv(client, verb, detail string) []string {
	msg := "links: " + verb + " failed: " + sanitizeStatusText(detail)
	argv := []string{"display-message"}
	if client != "" {
		argv = append(argv, "-c", client)
	}
	return append(argv, linksFlashEscaper.Replace(msg))
}

var linksFlashEscaper = strings.NewReplacer("#", "##", "%", "%%")

// runLinksCmd starts cmd and waits up to wait for it. A command still running
// then is left to finish on its own and reported as success. On an early
// failure it returns the command's stderr. Stderr goes to a file, not a pipe:
// a browser the opener leaves running would hold a pipe open.
func runLinksCmd(cmd *exec.Cmd, wait time.Duration) (string, error) {
	f, err := os.CreateTemp("", "og-links-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case runErr := <-done:
		if runErr == nil {
			return "", nil
		}
		detail, _ := os.ReadFile(f.Name())
		return strings.TrimSpace(string(detail)), runErr
	case <-time.After(wait):
		return "", nil
	}
}

// --- Model ---

func newLinksModel(links []linkjoin.Link, opts map[string]string, theme string) linksModel {
	m := linksModel{links: links, tmuxOpts: opts, theme: theme}
	return m.rebuildVisible()
}

func (m linksModel) rebuildVisible() linksModel {
	type scored struct{ idx, score int }
	var hits []scored
	for i, l := range m.links {
		if s := fuzzyScore(l.Text, m.query); s >= 0 {
			hits = append(hits, scored{i, s})
		}
	}
	if m.query != "" {
		sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	}
	m.visible = make([]int, len(hits))
	for i, h := range hits {
		m.visible[i] = h.idx
	}
	m.cursor = 0
	return m
}

// current is the link under the cursor.
func (m linksModel) current() (linkjoin.Link, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return linkjoin.Link{}, false
	}
	return m.links[m.visible[m.cursor]], true
}

func (m linksModel) Init() tea.Cmd { return nil }

func (m linksModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m linksModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "ctrl+p":
		m.cursor = max(m.cursor-1, 0)
	case "down", "ctrl+n":
		m.cursor = min(m.cursor+1, max(len(m.visible)-1, 0))
	case "enter":
		l, ok := m.current()
		if !ok {
			return m, nil
		}
		m.picked = l
		m.action = linksCopy
		if linkjoin.Openable(l.Text) {
			m.action = linksOpen
		}
		return m, tea.Quit
	case "ctrl+y":
		l, ok := m.current()
		if !ok {
			return m, nil
		}
		m.picked = l
		m.action = linksCopy
		return m, tea.Quit
	case "backspace":
		if m.query != "" {
			runes := []rune(m.query)
			m.query = string(runes[:len(runes)-1])
			m = m.rebuildVisible()
		}
	default:
		if text, ok := printableKeyText(key); ok {
			m.query += text
			m = m.rebuildVisible()
		}
	}
	return m, nil
}

// --- Rendering ---

// color reads a Catppuccin color from tmux options, falling back by theme.
func (m linksModel) color(tmuxOpt, darkFallback, lightFallback string) imgcolor.Color {
	return lipgloss.Color(m.colorHex(tmuxOpt, darkFallback, lightFallback))
}

func (m linksModel) colorHex(tmuxOpt, darkFallback, lightFallback string) string {
	if v, ok := m.tmuxOpts[tmuxOpt]; ok && v != "" {
		return v
	}
	if m.theme == "light" {
		return lightFallback
	}
	return darkFallback
}

func (m linksModel) View() tea.View {
	var content string
	if !m.ready {
		content = "Loading..."
	} else {
		rows := []string{m.renderTitle()}
		rows = append(rows, m.renderBody()...)
		rows = append(rows, m.renderSearch(), m.renderHints())
		content = strings.Join(rows, "\n")
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// bodyHeight is what the title, search and hint rows leave.
func (m linksModel) bodyHeight() int {
	return max(m.height-3, 1)
}

func (m linksModel) renderTitle() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(m.color("@thm_lavender", "#b4befe", "#7287fd"))
	dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
	line := " " + title.Render("Links") + dim.Render(" "+strconv.Itoa(len(m.visible)))
	return fitVisibleWidth(line, m.width)
}

// renderBody is exactly bodyHeight rows, scrolled to keep the cursor visible.
func (m linksModel) renderBody() []string {
	h := m.bodyHeight()
	rows := make([]string, 0, h)
	switch {
	case len(m.links) == 0:
		dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
		rows = append(rows, fitVisibleWidth(" "+dim.Render("no links in this pane"), m.width))
	case len(m.visible) == 0:
		dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
		rows = append(rows, fitVisibleWidth(" "+dim.Render("no matches"), m.width))
	default:
		start := max(m.cursor-h/2, 0)
		if start+h > len(m.visible) {
			start = max(len(m.visible)-h, 0)
		}
		for i := start; i < min(start+h, len(m.visible)); i++ {
			rows = append(rows, m.renderRow(m.links[m.visible[i]], i == m.cursor))
		}
	}
	for len(rows) < h {
		rows = append(rows, fitVisibleWidth("", m.width))
	}
	return rows
}

func (m linksModel) renderRow(l linkjoin.Link, selected bool) string {
	overlay := lipgloss.NewStyle().Foreground(m.color("@thm_overlay_0", "#6c7086", "#9ca0b0"))
	tag := overlay.Render(fitVisibleWidth(l.Kind.String(), 4))

	marker := ""
	if l.Rows > 1 {
		marker = " +" + strconv.Itoa(l.Rows)
	}
	linkW := max(m.width-1-4-1-visibleWidth(marker), 0)
	line := " " + tag + " " + truncateVisibleWidth(sanitizeStatusText(elideLeft(l.Text, min(linkW, statusTextMaxRunes))), linkW)
	if marker != "" {
		line += overlay.Render(marker)
	}

	fitted := fitVisibleWidth(line, m.width)
	if !selected {
		return fitted
	}
	// Every reset, lipgloss's "\033[m" and fitVisibleWidth's "\033[0m" alike,
	// would drop the background for the text after it, padding included.
	selBgHex := m.colorHex("@thm_surface_2", "#45475a", "#acb0be")
	restore := "\033[39m" + ansiBg(selBgHex)
	keepBg := strings.NewReplacer("\033[m", restore, "\033[0m", restore)
	return lipgloss.NewStyle().Background(lipgloss.Color(selBgHex)).Render(keepBg.Replace(fitted))
}

// elideLeft keeps the rightmost cells-1 cells of s behind a "…" when s is wider
// than cells: the tail is what tells sibling paths apart. s carries no escapes.
func elideLeft(s string, cells int) string {
	if visibleWidth(s) <= cells {
		return s
	}
	if cells < 1 {
		return ""
	}
	runes := []rune(s)
	w, i := 0, len(runes)
	for i > 0 && w+runeCellWidth(runes[i-1]) <= cells-1 {
		i--
		w += runeCellWidth(runes[i])
	}
	return "…" + string(runes[i:])
}

func (m linksModel) renderSearch() string {
	blue := lipgloss.NewStyle().Foreground(m.color("@thm_blue", "#89b4fa", "#1e66f5"))
	return fitVisibleWidth(" "+blue.Render("/")+" "+m.query+"█", m.width)
}

func (m linksModel) renderHints() string {
	dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
	key := lipgloss.NewStyle().Foreground(m.color("@thm_lavender", "#b4befe", "#7287fd"))
	hint := func(k, desc string) string { return key.Render(k) + dim.Render(":"+desc) }

	parts := []string{hint("esc", "close")}
	if l, ok := m.current(); ok {
		verb := "copy"
		if linkjoin.Openable(l.Text) {
			verb = "open"
		}
		parts = []string{hint("enter", verb), hint("^y", "copy"), hint("esc", "close")}
	}
	return fitVisibleWidth(" "+strings.Join(parts, dim.Render(" · ")), m.width)
}
