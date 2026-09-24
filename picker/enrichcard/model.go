package main

import (
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/noamsto/tmux-og/picker/enrichstate"
)

// cfg holds the launch-time flags: target, the PR-poller and issue-stamp
// binaries to spawn for refresh, the theme palette, and the enrich glyphs
// (raw — NOT ##-escaped).
type cfg struct {
	target, prEnrichBin, issueStampBin string
	// bridgeCtlBin/bridgeSock/bridgePane give a mirror window's [r] a ctl
	// handle to the remote's own poller (D5). All three are empty on a
	// non-mirror window, and can also be empty on a mirror whose bind
	// couldn't resolve @bridge_sock/@bridge_pane — hasBridgeHandle is the
	// single place that distinguishes "no handle" from "has one".
	bridgeCtlBin, bridgeSock, bridgePane                                                string
	fg, mauve, red, green, peach, blue, overlay0, subtext0                              string
	icLinear, icGitHub, icPending, icSuccess, icFailure, icMerged, icClosed, icConflict string
	icDraft                                                                             string
}

type model struct {
	cfg           cfg
	win           winState
	mirror        bool // true when the window is a bridge mirror; win came from @bridge_* only
	baseBranch    string
	width, height int
	refreshing    bool
	// sending is the bridged-refresh re-entrancy guard, distinct from
	// refreshing: prBlock renders refreshing as "⧗ #N refreshing…", which
	// would misdescribe a local socket write as a PR fetch. Set on dispatch,
	// cleared by bridgeRefreshDoneMsg.
	sending      bool
	flash        string    // transient footer note ("opened ↗", a ctl error, …)
	flashIsError bool      // true renders flash in c.red instead of c.green
	flashUntil   time.Time // when flash clears; zero means nothing to clear
}

const (
	widthFloor  = 34 // below this: drop the worktree path line, truncate harder
	heightFloor = 12 // below this: drop the branch + Claude blocks, keep identity + footer
)

func (m model) sty(hex string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
}

func (m model) colorFor(r enrichstate.ColorRole) string {
	switch r {
	case enrichstate.ColorMerged:
		return m.cfg.mauve
	case enrichstate.ColorClosed:
		return m.cfg.overlay0
	case enrichstate.ColorFailure:
		return m.cfg.red
	case enrichstate.ColorPending:
		return m.cfg.peach
	case enrichstate.ColorReviewRequired:
		return m.cfg.overlay0
	default:
		return m.cfg.green
	}
}

func (m model) glyphFor(r enrichstate.GlyphRole) string {
	switch r {
	case enrichstate.GlyphMerged:
		return m.cfg.icMerged
	case enrichstate.GlyphClosed:
		return m.cfg.icClosed
	case enrichstate.GlyphConflict:
		return m.cfg.icConflict
	case enrichstate.GlyphFailure:
		return m.cfg.icFailure
	case enrichstate.GlyphPending:
		return m.cfg.icPending
	default:
		return m.cfg.icSuccess
	}
}

func (m model) titleWidth() int {
	w := m.width - 6 // border + padding
	if w < 10 {
		return 10
	}
	if w > 80 {
		return 80
	}
	return w
}

func truncate(s string, max int) string {
	if max <= 1 || lipgloss.Width(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) > max-1 {
		r = r[:max-1]
	}
	return string(r) + "…"
}

func (w winState) noURLReason() string {
	if w.issueStampError != "" {
		return w.issueStampError
	}
	return "stamp failed"
}

func (m model) issueBlock() string {
	c, w := m.cfg, m.win
	if w.issueID == "" {
		return m.sty(c.overlay0).Render("no issue")
	}
	glyph := c.icGitHub
	if w.issueProvider == "linear" {
		glyph = c.icLinear
	}
	head := m.sty(c.blue).Bold(true).Render(glyph + "  " + w.issueID)
	title := m.sty(c.fg).Render(truncate(w.issueTitle, m.titleWidth()))
	if w.issueURL == "" {
		noURL := m.sty(c.overlay0).Render(truncate("no url — "+w.noURLReason(), m.titleWidth()))
		return lipgloss.JoinVertical(lipgloss.Left, head, title, noURL)
	}
	return lipgloss.JoinVertical(lipgloss.Left, head, title)
}

func (m model) prBlock() string {
	c, w := m.cfg, m.win
	if w.prNumber == "" || w.prNumber == "none" {
		return m.sty(c.overlay0).Render("no PR")
	}
	if m.refreshing {
		return m.sty(c.peach).Render("⧗ #" + w.prNumber + " refreshing…")
	}
	cr, gr := enrichstate.Classify(w.prState, w.prCheck, w.prMergeable)
	glyph := m.glyphFor(gr)
	progress := ""
	if gr == enrichstate.GlyphPending {
		if pie := enrichstate.Pie(w.prProgress); pie != "" {
			glyph, progress = pie, w.prProgress
		}
	}
	if enrichstate.Draft(w.prState, w.prDraft) {
		glyph = c.icDraft + " " + glyph
	}
	numStyle := m.sty(m.colorFor(cr))
	if rc, ok := enrichstate.ReviewColor(w.prState, w.prReview); ok {
		numStyle = m.sty(m.colorFor(rc))
	}
	if enrichstate.AutoMerge(w.prState, w.prAutoMerge) {
		numStyle = numStyle.Underline(true)
	}
	badge := m.sty(m.colorFor(cr)).Render(glyph+" ") + numStyle.Render("#"+w.prNumber)
	if progress != "" {
		badge += m.sty(c.overlay0).Render("  " + progress + " checks")
	}
	title := m.sty(c.fg).Render(truncate(w.prTitle, m.titleWidth()))
	return lipgloss.JoinVertical(lipgloss.Left, badge, title)
}

func (m model) branchBlock() string {
	c, w := m.cfg, m.win
	dir := w.worktree
	if dir == "" {
		dir = w.gitRoot
	}
	var lines []string
	if w.branch != "" {
		head := w.branch
		if m.baseBranch != "" {
			head += "  →  " + m.baseBranch
		}
		lines = append(lines, m.sty(c.subtext0).Render(head))
	}
	if dir != "" && m.width >= widthFloor {
		lines = append(lines, m.sty(c.overlay0).Render(truncate(dir, m.titleWidth())))
	}
	if len(lines) == 0 {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (m model) claudeBlock() string {
	c, w := m.cfg, m.win
	parts := []string{}
	if w.paneIcon != "" {
		parts = append(parts, w.paneIcon)
	}
	if w.task != "" {
		parts = append(parts, w.task)
	}
	if w.claudeAgo != "" {
		parts = append(parts, w.claudeAgo)
	}
	if len(parts) == 0 {
		return ""
	}
	return m.sty(c.subtext0).Render(strings.Join(parts, "  ·  "))
}

// hasBridgeHandle reports whether the launch-time flags gave this card a ctl
// handle to reach the bridge daemon through. Flags, not an option read — see
// the cfg field comment.
func (m model) hasBridgeHandle() bool {
	return m.cfg.bridgeSock != "" && m.cfg.bridgePane != ""
}

// footer renders the four [r] states from the design's contract (D5): a
// missing branch or a missing bridge handle both stay inert, and only a
// mirror with a handle takes the ctl route rather than the local poller.
func (m model) footer() string {
	c := m.cfg
	plain := m.sty(c.subtext0)
	items := []string{plain.Render("[o] issue"), plain.Render("[p] PR")}
	switch {
	case m.win.branch == "":
		items = append(items, m.sty(c.overlay0).Render("[r] no branch"))
	case m.mirror && !m.hasBridgeHandle():
		items = append(items, m.sty(c.overlay0).Render("[r] no bridge"))
	default:
		items = append(items, plain.Render("[r] refresh"))
	}
	items = append(items, plain.Render("[q] close"))
	const sep = "   "
	if m.flash != "" {
		flashColor := c.green
		if m.flashIsError {
			flashColor = c.red
		}
		// Truncate to what's actually left on the row, not the full panel
		// width — the fixed [o]/[p]/[r]/[q] items already eat most of it, and
		// m.flash can carry an arbitrary CLI error message.
		budget := max(m.titleWidth()-lipgloss.Width(strings.Join(items, sep))-lipgloss.Width(sep), 4)
		items = append(items, m.sty(flashColor).Render(truncate(m.flash, budget)))
	}
	return strings.Join(items, sep)
}

// card renders the full bordered popup. Pure over model state (no tmux calls).
func (m model) card() string {
	rows := []string{m.issueBlock(), "", m.prBlock()}
	if m.height >= heightFloor {
		rows = append(rows, "", m.branchBlock())
		if cb := m.claudeBlock(); cb != "" {
			rows = append(rows, "", cb)
		}
	}
	rows = append(rows, "", m.footer())
	inner := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(m.cfg.overlay0)).
		Padding(0, 1).
		Render(inner)
}

type tickMsg struct{}
type refreshDoneMsg struct{}
type bridgeRefreshDoneMsg struct{ errText string } // errText == "" is success
type openDoneMsg struct{ errText string }          // errText == "" is success

const (
	// ctlErrorPrefix matches og-remote-bridge-ctl's own errorPrefix
	// (remotebridge/cmd/ctl/main.go) and is stripped from a captured failure
	// so the flash shows the ctl's message alone, not the binary name
	// repeated next to a footer already labeled [r].
	ctlErrorPrefix = "og-remote-bridge-ctl: "

	// Flash lifetimes follow house precedent rather than an invented number:
	// 5s matches the ctl's own `display-message -d 5000` on its failure path
	// (cmd/ctl/main.go); 2s is the confirmation duration for "opened ↗" and
	// "refresh sent ↗".
	flashErrorDuration   = 5 * time.Second
	flashConfirmDuration = 2 * time.Second
)

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func openCmd(url string) tea.Cmd {
	return func() tea.Msg {
		if err := exec.Command("xdg-open", url).Start(); err != nil { // Linux-only (parity w/ old keybind)
			return openDoneMsg{errText: err.Error()}
		}
		return openDoneMsg{}
	}
}

// issueStampArgs builds tmux-issue-stamp's positional argv: target, dir,
// branch, plus the explicit id as a 4th element when non-empty. explicitID is
// only ever non-empty for a window stamped via `claude-status-update enrich
// <ID>` (#137) — its real branch doesn't encode the issue, so a 3-arg
// re-invocation would fall through to branch derivation and wipe a correct id
// instead of re-resolving it (#599).
func issueStampArgs(target, dir, branch, explicitID string) []string {
	args := []string{target, dir, branch}
	if explicitID != "" {
		args = append(args, explicitID)
	}
	return args
}

// shouldStampIssue reports whether refreshCmd should also re-run the
// issue-stamp binary. dir == "" means a reclaimed worktree: branch derivation
// would find nothing and github's origin check would fail open, wiping a
// valid stamp the same way an unresolved backfill-sweep candidate would
// (#599) — so refresh must skip that leg rather than risk the wipe.
func shouldStampIssue(c cfg, w winState, dir string) bool {
	return c.issueStampBin != "" && w.branch != "" && dir != ""
}

// refreshCmd runs the PR poller's and (when known) the issue-identity
// stamp's single-target --force-equivalent passes concurrently and BLOCKS
// until both exit, then signals done. This converges the spinner
// deterministically rather than guessing from a value-diff.
func refreshCmd(c cfg, w winState) tea.Cmd {
	dir := w.worktree
	if dir == "" {
		dir = w.gitRoot
	}
	return func() tea.Msg {
		var wg sync.WaitGroup
		wg.Go(func() {
			_ = exec.Command(c.prEnrichBin, "--target", c.target, "--branch", w.branch, "--dir", dir, "--force").Run()
		})
		if shouldStampIssue(c, w, dir) {
			wg.Go(func() {
				_ = exec.Command(c.issueStampBin, issueStampArgs(c.target, dir, w.branch, w.issueExplicitID)...).Run()
			})
		}
		wg.Wait()
		return refreshDoneMsg{}
	}
}

// bridgeRefreshArgv is the ctl invocation for the enrich-refresh verb, split
// out of bridgeRefreshCmd so a test can assert it without executing a binary.
func bridgeRefreshArgv(sock, pane string) []string {
	return []string{"--sock", sock, "enrich-refresh", pane}
}

// bridgeRefreshCmd runs the ctl synchronously — a tea.Cmd is already a
// goroutine, so the UI never blocks — and relies on the ctl's own 2s
// overallTimeout rather than a context of its own.
//
// Output is captured, never inherited: the card is a bubbletea altscreen
// program and the ctl reports failure via fmt.Fprintln(os.Stderr, …)
// (remotebridge/cmd/ctl/main.go), which would paint over the card.
func bridgeRefreshCmd(bin, sock, pane string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command(bin, bridgeRefreshArgv(sock, pane)...).CombinedOutput()
		if err == nil {
			return bridgeRefreshDoneMsg{}
		}
		text := strings.TrimPrefix(strings.TrimSpace(string(out)), ctlErrorPrefix)
		if text == "" {
			text = err.Error()
		}
		return bridgeRefreshDoneMsg{errText: text}
	}
}

// Init only schedules the first tick; the initial window read is done in main
// (the value-receiver model passed to NewProgram is what bubbletea seeds with,
// so assigning m.win here would not persist).
func (m model) Init() tea.Cmd { return tickCmd() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if !m.refreshing {
			opts := readWindowState(m.cfg.target)
			m.win, m.mirror = resolve(opts), opts.mirror
		}
		// A deadline, never a "clear after one tick" counter: tickCmd is a 1s
		// tea.Tick whose phase relative to the keypress is arbitrary, so a
		// counter reproduces the same 0-1000ms window a flash used to have.
		if !m.flashUntil.IsZero() && !time.Now().Before(m.flashUntil) {
			m.flash = ""
			m.flashUntil = time.Time{}
		}
		return m, tickCmd()
	case refreshDoneMsg:
		m.refreshing = false
		opts := readWindowState(m.cfg.target)
		m.win, m.mirror = resolve(opts), opts.mirror
		return m, nil
	case bridgeRefreshDoneMsg:
		m.sending = false
		if msg.errText != "" {
			m.flash = msg.errText
			m.flashIsError = true
			m.flashUntil = time.Now().Add(flashErrorDuration)
		} else {
			m.flash = "refresh sent ↗"
			m.flashIsError = false
			m.flashUntil = time.Now().Add(flashConfirmDuration)
		}
		return m, nil
	case openDoneMsg:
		if msg.errText != "" {
			m.flash = "open failed: " + msg.errText
			m.flashIsError = true
			m.flashUntil = time.Now().Add(flashErrorDuration)
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m model) handleKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "o":
		if m.win.issueURL != "" {
			m.flash = "opened ↗"
			m.flashIsError = false
			m.flashUntil = time.Now().Add(flashConfirmDuration)
			return m, openCmd(m.win.issueURL)
		} else if m.win.issueID != "" {
			m.flash = "no url — " + m.win.noURLReason()
			m.flashIsError = true
			m.flashUntil = time.Now().Add(flashErrorDuration)
			return m, nil
		}
	case "p":
		if m.win.prURL != "" {
			m.flash = "opened ↗"
			m.flashIsError = false
			m.flashUntil = time.Now().Add(flashConfirmDuration)
			return m, openCmd(m.win.prURL)
		}
	case "r":
		if m.win.branch == "" {
			return m, nil // [r] no branch — inert on both paths, see footer
		}
		if m.mirror {
			if !m.hasBridgeHandle() || m.sending {
				return m, nil // [r] no bridge, or a held press already in flight
			}
			m.sending = true
			return m, bridgeRefreshCmd(m.cfg.bridgeCtlBin, m.cfg.bridgeSock, m.cfg.bridgePane)
		}
		if !m.refreshing {
			m.refreshing = true
			return m, refreshCmd(m.cfg, m.win)
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	w, h := m.width, m.height
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	v := tea.NewView(lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, m.card()))
	v.AltScreen = true
	return v
}
