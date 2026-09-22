package main

import (
	"errors"
	"fmt"
	imgcolor "image/color"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// listItem is one row in the picker list.
type listItem struct {
	target         string // tmux target; "" = unselectable (unless remoteHost set)
	display        string // ANSI-rendered display line
	plain          string // display stripped of ANSI (cached for width)
	searchText     string // filterable text (name, branch — no paths/icons)
	isHeader       bool   // session header row
	isColumnHeader bool   // the session list's glyph column-label row
	isZoxideHeader bool   // the "── New session ──" divider
	isRemoteHeader bool   // the "── Remote ──" divider
	// headerLabel/headerIcon let renderList rebuild a section divider at the
	// real popup width, which the collectors do not know.
	headerLabel string
	headerIcon  string
	session     string // owning session name (for kill)
	groupKey    string // window-mode header key this row re-attaches to
	// when filtering: session name, or agent state
	bridgeHost           string // @bridge_host — set when this session mirrors a remote host
	current              bool   // this is the session the invoking tmux client is attached to
	bridgePane           string // window row: @bridge_pane — the remote pane whose window this mirrors
	bridgeSock           string // window row: @bridge_sock — ctl socket of the daemon mirroring it
	hasActiveAgent       bool   // used for --agent filter
	isScratch            bool   // scratch-* session
	createPath           string // zoxide suggestion: dir to create a session at ("" = normal row)
	createName           string // zoxide suggestion: derived session name
	isRemoteRow          bool   // belongs to the Remote section (set even when unselectable)
	remoteHost           string // remote bridge row: ssh host for og-remote-open
	remoteSess           string // remote bridge row: optional remote session name
	remoteContextOnly    bool   // host row: pulled in only as tree context for a matching child, not its own match — unselectable
	displayEnd           string // remote session row: display with the closing tree glyph
	plainEnd             string // remote session row: plain with the closing tree glyph
	remoteRestore        bool   // remote bridge row: sourced from a tmux-remux snapshot, not a live probe — bridging must restore it first
	remoteNeedsAuth      bool   // remote host row: the probe hit an interactive ssh prompt; Enter runs og-remote-auth
	remoteInert          bool   // remote host row: host key changed — Enter must refuse to act, never offer to connect
	remoteTailscaleCheck bool   // remote host row: a Tailscale ACL "check" blocked the probe — Enter must refuse to act, like remoteInert; og-remote-auth cannot clear this
	remoteTailscaleURL   string // remote host row: the login URL captured from the probe's stdout, if any — supplementary only, may be stale
}

// pickerMode selects which renderer draws the body. One model, three
// renderers: modeList draws renderList (+ renderPreview), modeWall renderWall.
type pickerMode int8

const (
	modeList pickerMode = iota
	modeWall
)

// tuiModel is the bubbletea model for the picker.
type tuiModel struct {
	// Data
	allItems     []listItem // unfiltered: sessionItems + remoteItems + zoxideItems
	sessionItems []listItem // session/window rows (base for recombination)
	remoteItems  []listItem // remote host/session rows, loaded async after first paint
	zoxideItems  []listItem // zoxide suggestions, loaded async after first paint
	visible      []listItem // after query + mode filter
	cursor       int

	// Modes
	mode pickerMode
	// wallLaunched records that --wall opened this popup, which is what makes ^/
	// a toggle rather than a one-way trip out of the wall.
	wallLaunched bool
	windowMode   bool
	stateGrouped bool // window mode: group by agent state instead of session (#229)
	agentOnly    bool
	scratchOnly  bool

	// Wall
	wallPage    int
	wallContent map[string]string // target -> last capture
	wallBad     map[string]bool   // targets capture-pane refused, skipped next batch
	// focused is set by tab and cleared by esc: while true, in-scope keystrokes
	// relay to the tile under the cursor instead of navigating the grid (#316).
	focused bool

	// Search
	query string
	// querying is the wall's modal filter prompt: letters navigate the grid, so
	// they only reach the query while it is open.
	querying bool

	// Transient error shown in the hint line (e.g. session-create failure)
	statusMsg string

	// Preview
	preview        viewport.Model
	showPreview    bool
	previewFor     string // target currently loaded in preview
	previewRaw     string // unshifted content for horizontal scroll
	previewXOffset int    // horizontal scroll offset (in cells)

	// Layout
	width, height int
	ready         bool

	// Config
	theme    string
	tmuxOpts map[string]string

	// Remote-pick mode (#356): the picker runs on a remote host over ssh, with
	// no attached tmux client to switch — Enter writes emitPath instead.
	// "" means the ordinary interactive picker.
	emitPath string
	// emitHost is the ssh host the local side reached us by, carried across so
	// the header can name it — the remote cannot derive it. Display only.
	emitHost string
	// zoxideReady is set once zoxideMsg arrives: a nil zoxideItems means both
	// "no suggestions" and "the probe hasn't answered", which recombine's
	// both-empty guard has to tell apart.
	zoxideReady bool
	// currentSession is the raw name of the session the invoking tmux client
	// is attached to (OG_PICKER_CURRENT_SESSION), used to sink it below a
	// same-display-name peer on another host.
	currentSession string
}

// --- Catppuccin palette (dark/light) ---

// thmColor reads a Catppuccin color from tmux options, falling back to defaults.
func (m tuiModel) thmColor(tmuxOpt, darkFallback, lightFallback string) imgcolor.Color {
	return lipgloss.Color(m.thmColorHex(tmuxOpt, darkFallback, lightFallback))
}

func (m tuiModel) thmColorHex(tmuxOpt, darkFallback, lightFallback string) string {
	if v, ok := m.tmuxOpts[tmuxOpt]; ok && v != "" {
		return v
	}
	if m.theme == "light" {
		return lightFallback
	}
	return darkFallback
}

// --- Messages ---

type tickMsg struct{}

// previewTickMsg drives the preview's own reload clock, separate from the 1s
// data tick.
type previewTickMsg struct{}

// wallTickMsg drives the wall's capture clock, separate from the preview's.
type wallTickMsg struct{}

type refreshMsg struct {
	items []listItem
}

// zoxideMsg carries the suggestion rows collected off the first-paint path.
type zoxideMsg struct {
	items []listItem
}

type remoteMsg struct {
	items []listItem
}

// remoteAuthDoneMsg lands when the interactive ssh handshake has exited and the
// popup's pty is back under bubbletea's control. err is ExecProcess's own
// error, not the script's exit status; remoteAuthStartFailure decides which
// kind is worth showing.
type remoteAuthDoneMsg struct {
	err error
}

type previewMsg struct {
	content   string
	target    string
	scrollTop bool
}

// wallMsg carries one batch's captures. content is merged, never swapped in:
// tmux aborts a batch at the first bad target, so a partial reply must not blank
// the tiles it didn't reach. bad names that target, if there was one.
type wallMsg struct {
	content map[string]string
	bad     string
}

// --- Entry point ---

// layoutShowsPreview reports whether the picker opens with the preview shown,
// from @picker_layout. Anything but "list" (including unset) means preview —
// the historical default. ^/ still toggles at runtime.
func layoutShowsPreview(opts map[string]string) bool {
	return envOrMap("PICKER_LAYOUT", opts, "@picker_layout", "preview") != "list"
}

// wallMode maps the --wall flag to the mode the picker opens in.
func wallMode(wall bool) pickerMode {
	if wall {
		return modeWall
	}
	return modeList
}

// newPickerModel assembles the picker's initial model from already-gathered
// inputs — no tmux/ssh/proc calls of its own — so the first-paint wiring
// (including the Remote section, #312) is exercised by a real unit test
// instead of only by a manual tmux check.
func newPickerModel(windowMode, agentOnly, wall bool, opts map[string]string, theme string, items []listItem, emitPath string) tuiModel {
	m := tuiModel{
		mode:         wallMode(wall),
		wallLaunched: wall,
		windowMode:   windowMode,
		agentOnly:    agentOnly,
		showPreview:  layoutShowsPreview(opts),
		theme:        theme,
		tmuxOpts:     opts,
		sessionItems: items,
		wallContent:  map[string]string{},
		wallBad:      map[string]bool{},
		emitPath:     emitPath,
	}
	if !windowMode && emitPath == "" {
		// Host rows are static config and their sessions come from the on-disk
		// cache — render them now so the Remote section exists, and is
		// searchable, from the first paint. remoteCmd's probe (kicked from Init)
		// replaces them in place via remoteMsg (#312, #631). Emit mode builds
		// none: it runs on a host we are not attached to, so a Remote section
		// there would bridge from the wrong side.
		m.remoteItems = pendingRemoteItems(opts, firstPaintBridges(items))
	}
	m = m.recombine().withFilter()
	m.cursor = m.firstSelectable(0)
	if m.mode == modeWall {
		m = m.snapWall()
	}
	return m
}

func runTUI(windowMode, agentOnly, wall, remotePick bool) error {
	var emitPath string
	if remotePick {
		// A bare flag doesn't otherwise stop --remote-pick --windows from
		// emitting a *window* target as a session name.
		if windowMode || wall {
			return errors.New("--remote-pick is incompatible with --windows/--wall")
		}
		emitPath = os.Getenv("OG_PICKER_EMIT")
		if emitPath == "" {
			return errors.New("--remote-pick requires OG_PICKER_EMIT")
		}
	}

	opts := readTmuxOpts()
	theme := themeFromOpts(opts)
	snap := collectPanesSnapshot()
	panes := collectAgentPanes(snap)
	currentSession := os.Getenv("OG_PICKER_CURRENT_SESSION")

	var items []listItem
	if windowMode {
		items = buildWindowItems(opts, panes, theme, 0, false)
	} else {
		items = buildSessionItems(opts, snap, panes, theme, false, currentSession)
	}

	m := newPickerModel(windowMode, agentOnly, wall, opts, theme, items, emitPath)
	m.currentSession = currentSession
	if emitPath != "" {
		m.emitHost = os.Getenv("OG_PICKER_HOST")
	}

	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		return err
	}
	// A failed emit write can't os.Exit from inside Update — that would leave
	// the remote ssh pty in altscreen/raw mode. It sets statusMsg and quits
	// instead, so the error surfaces here only after bubbletea has restored
	// the terminal.
	if fm, ok := final.(tuiModel); ok && fm.emitPath != "" && fm.statusMsg != "" {
		return errors.New(fm.statusMsg)
	}
	return nil
}

// --- Bubbletea interface ---

func (m tuiModel) Init() tea.Cmd {
	// No wall capture here: the grid is derived from a size this model doesn't
	// have yet, and nothing paints before the WindowSizeMsg that brings it.
	cmds := []tea.Cmd{tickCmd(), previewTickCmd(), wallTickCmd(), m.loadPreviewCmd()}
	if !m.windowMode {
		// First paint skips the ps -A fork; kick a full refresh right away so
		// CPU/Mem replace the placeholder without waiting for the 1s tick.
		cmds = append(cmds, m.zoxideCmd(), m.refreshDataCmd())
		if m.emitPath == "" {
			// Emit mode never quits to bridge a further-remote host, so its
			// probe would just be wasted round trips (spec D8).
			cmds = append(cmds, m.remoteCmd())
		}
	}
	return tea.Batch(cmds...)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		widthChanged := msg.Width != m.width
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.preview = viewport.New(
				viewport.WithWidth(m.previewWidth()),
				viewport.WithHeight(m.previewHeight()),
			)
			m.preview.MouseWheelEnabled = true
			m.ready = true
		} else {
			m.preview.SetWidth(m.previewWidth())
			m.preview.SetHeight(m.previewHeight())
		}
		// Window labels are truncated to the terminal width; when it changes,
		// rebuild so the adaptive identity cap tracks the real size. Guarded on
		// change so a fixed-size popup forks the rebuild ~once.
		if m.mode == modeWall {
			// The grid is derived from the size, so a resize changes how many
			// tiles a page holds — and which page the cursor sits on.
			m = m.snapWall()
		}
		if m.windowMode && widthChanged {
			return m, tea.Batch(m.loadPreviewCmd(), m.captureWallCmd(), m.refreshDataCmd())
		}
		return m, tea.Batch(m.loadPreviewCmd(), m.captureWallCmd())

	case tickMsg:
		return m, tea.Batch(tickCmd(), m.refreshDataCmd())

	// This is the only periodic preview load: the data handlers below deliberately
	// don't, because a list rebuild moving what sits under the cursor is picked up
	// here within one interval.
	case previewTickMsg:
		return m, tea.Batch(previewTickCmd(), m.loadPreviewCmd())

	case wallTickMsg:
		return m, tea.Batch(wallTickCmd(), m.captureWallCmd())

	case wallMsg:
		return m.mergeWall(msg), nil

	case refreshMsg:
		// Read the selection before the rebuild invalidates its index.
		keep := m.currentTarget()
		m.sessionItems = msg.items
		m = m.recombine().withFilter()
		if m.cursor >= len(m.visible) || !m.isSelectable(m.visible[m.cursor]) {
			m.cursor = m.firstSelectable(0)
		}
		if m.mode == modeWall {
			// A rebuild can move a header under the cursor, and reorder the rows
			// under the selection; the wall has no non-tileable selection to fall
			// back on, and follows the window rather than the row number.
			m = m.snapWallTo(keep)
		}
		return m, nil

	// Both of these rebuild visible, so the wall re-snaps for the same reason
	// refreshMsg does: an index does not survive a rebuild, and the wall has no
	// non-tileable selection to land on.
	case zoxideMsg:
		keep := m.currentTarget()
		m.zoxideItems = msg.items
		m.zoxideReady = true
		m = m.recombine().withFilter()
		if m.cursor >= len(m.visible) || !m.isSelectable(m.visible[m.cursor]) {
			m.cursor = m.firstSelectable(0)
		}
		if m.mode == modeWall {
			m = m.snapWallTo(keep)
		}
		return m, nil

	case remoteMsg:
		keep := m.currentTarget()
		m.remoteItems = msg.items
		m = m.recombine().withFilter()
		m = m.restoreCursor(keep)
		if m.mode == modeWall {
			m = m.snapWallTo(keep)
		}
		return m, nil

	case remoteAuthDoneMsg:
		if execErrorMessage, ok := remoteAuthStartFailure(msg.err); ok {
			m.statusMsg = execErrorMessage
		}
		return m, m.remoteCmd()

	case previewMsg:
		if msg.target == m.currentTarget() {
			sameTarget := msg.target == m.previewFor
			m.previewRaw = msg.content
			m.previewFor = msg.target
			if sameTarget && m.previewXOffset > 0 {
				m.applyPreviewXOffset()
			} else {
				m.previewXOffset = 0
				m.preview.SetContent(msg.content)
			}
			if !sameTarget {
				if msg.scrollTop {
					m.preview.SetYOffset(0)
				} else {
					m.preview.SetYOffset(m.preview.TotalLineCount())
				}
			}
		}
		return m, nil

	// Both mouse handlers hit-test by y against the list's rows, which the wall
	// doesn't have — a stray click there would resolve to an arbitrary row and,
	// when it matched the cursor, switch to it and quit.
	case tea.MouseWheelMsg:
		if m.mode != modeList {
			return m, nil
		}
		mouse := msg.Mouse()
		if m.inPreview(mouse.X, mouse.Y) {
			var cmd tea.Cmd
			m.preview, cmd = m.preview.Update(msg)
			return m, cmd
		}
		switch mouse.Button {
		case tea.MouseWheelUp:
			m = m.moveCursor(-1)
		case tea.MouseWheelDown:
			m = m.moveCursor(1)
		}
		return m, m.loadPreviewCmd()

	case tea.MouseClickMsg:
		if m.mode != modeList {
			return m, nil
		}
		mouse := msg.Mouse()
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		idx, ok := m.listIndexAt(mouse.X, mouse.Y)
		if !ok {
			return m, nil
		}
		if idx == m.cursor {
			return m.activateCurrent() // click the highlighted row to switch
		}
		m.statusMsg = ""
		m.cursor = idx
		return m, m.loadPreviewCmd()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.preview, cmd = m.preview.Update(msg)
	return m, cmd
}

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.statusMsg = "" // any keypress clears a stale create-error
	key := msg.String()
	// The wall's keymap runs first: letters navigate the grid there, so a shared
	// branch must never see a key the wall (or its filter prompt) owns.
	if m.mode == modeWall {
		// esc's meaning depends on focused/querying/query together, so it is
		// resolved once here rather than duplicated in the branches below.
		if key == "esc" {
			return m.applyWallEsc()
		}
		if m.querying {
			return m.handleWallQueryKey(key)
		}
		if m.focused {
			if wm, cmd, handled := m.handleFocusedKey(key); handled {
				return wm, cmd
			}
		}
		if wm, cmd, handled := m.handleWallKey(key); handled {
			return wm, cmd
		}
	}
	switch key {
	case "ctrl+c":
		return m, tea.Quit

	case "q", "esc":
		if m.query != "" {
			m.query = ""
			m = m.withFilter()
			m.cursor = m.firstSelectable(0)
			return m, m.loadPreviewCmd()
		}
		return m, tea.Quit

	case "ctrl+j", "down":
		m = m.moveCursor(1)
		return m, m.loadPreviewCmd()

	case "ctrl+k", "up":
		m = m.moveCursor(-1)
		return m, m.loadPreviewCmd()

	case "enter":
		return m.activateCurrent()

	case "ctrl+x":
		if m.emitPath != "" {
			// Inherited unchanged this would kill-session and zoxideForget
			// against the *remote* server and db (spec D8); renderHints drops
			// the ^x advertisement to match.
			return m, nil
		}
		item, ok := m.currentItem()
		if !ok {
			return m, nil
		}
		if item.createPath != "" {
			logEvent("picker", "event", "zoxide_forget", "path", item.createPath)
			if err := zoxideForget(item.createPath); err != nil {
				m.statusMsg = err.Error()
				return m, nil
			}
			return m, m.zoxideCmd()
		}
		if item.target != "" && item.remoteHost == "" {
			if strings.Contains(item.target, ":") {
				// A local kill-window is wrong for a mirror: the daemon
				// reconciles only toward the remote, so it would go on
				// servicing a registry entry whose localWin is gone (#393).
				if item.bridgePane != "" && item.bridgeSock != "" {
					logEvent("picker", "event", "kill_bridge_window", "target", item.target, "pane", item.bridgePane)
					if err := bridgeCtlKillWindow(m.tmuxOpts, item.bridgeSock, item.bridgePane); err != nil {
						m.statusMsg = err.Error()
						return m, nil
					}
				} else {
					logEvent("picker", "event", "kill_window", "target", item.target)
					exec.Command("tmux", "kill-window", "-t", item.target).Run() //nolint:errcheck
				}
			} else {
				logEvent("picker", "event", "kill_session", "target", item.target)
				// Must run before kill-session: it reads @bridge_sock off the
				// still-live session to find the daemon to signal — once the
				// session is gone, so is the option.
				if item.bridgeHost != "" {
					stopBridgeDaemon(item.target)
				}
				exec.Command("tmux", "kill-session", "-t", item.target).Run() //nolint:errcheck
			}
			return m, m.refreshDataCmd()
		}

	case "ctrl+o":
		item, ok := m.currentItem()
		host, live := remotePickHost(item, m.windowMode)
		if !ok || !live {
			return m, nil
		}
		bin := envOrMap("REMOTE_PICK_BIN", m.tmuxOpts, "@remote_pick_bin", "")
		if bin == "" {
			m.statusMsg = "remote picker not configured — reload tmux"
			return m, nil
		}
		if err := exec.Command("tmux", remotePickNewPaneArgs(bin, host)...).Run(); err != nil {
			m.statusMsg = err.Error()
			return m, nil
		}
		return m, tea.Quit

	case "ctrl+a":
		m = m.toggleAgentOnly()
		return m, m.loadPreviewCmd()

	case "ctrl+s":
		m = m.toggleScratchOnly()
		return m, m.loadPreviewCmd()

	case "ctrl+/", "ctrl+_":
		// In a wall-launched popup this is the return leg of the wall toggle, and
		// the new page needs its captures now rather than at the next tick.
		if m.wallLaunched {
			m.mode = modeWall
			m = m.snapWall()
			return m, m.captureWallCmd()
		}
		m.showPreview = !m.showPreview
		if m.ready {
			m.preview.SetWidth(m.previewWidth())
			m.preview.SetHeight(m.previewHeight())
		}
		return m, nil

	case "alt+j":
		m.preview.SetYOffset(m.preview.YOffset() + 3)

	case "alt+k":
		m.preview.SetYOffset(m.preview.YOffset() - 3)

	case "alt+l":
		m.previewXOffset += 8
		m.applyPreviewXOffset()

	case "alt+h":
		m.previewXOffset -= 8
		if m.previewXOffset < 0 {
			m.previewXOffset = 0
		}
		m.applyPreviewXOffset()

	case "ctrl+g":
		if !m.windowMode || m.mode == modeWall {
			return m, nil
		}
		m.stateGrouped = !m.stateGrouped
		m.cursor = m.firstSelectable(0)
		return m, m.refreshDataCmd()

	case "backspace":
		if len(m.query) > 0 {
			runes := []rune(m.query)
			m.query = string(runes[:len(runes)-1])
			m = m.withFilter()
			m.cursor = m.firstSelectable(0)
			return m, m.loadPreviewCmd()
		}

	default:
		if text, ok := printableKeyText(key); ok {
			m.query += text
			m = m.withFilter()
			m.cursor = m.firstSelectable(0)
			return m, m.loadPreviewCmd()
		}
	}
	return m, nil
}

// printableKeyText returns the literal character a key press types, and whether
// it types one at all.
//
// bubbletea v2 reports the space key by NAME: KeyPressMsg.String() falls
// through to Keystroke() for the one invisible printable character, so it
// arrives as "space", never " " (#689). Callers need the character, not the
// name — relayKeyArgs would otherwise send the word to a pane.
func printableKeyText(key string) (string, bool) {
	if key == "space" {
		return " ", true
	}
	if len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
		return key, true
	}
	return "", false
}

func (m tuiModel) toggleAgentOnly() tuiModel {
	m.agentOnly = !m.agentOnly
	if m.agentOnly {
		m.scratchOnly = false
	}
	m = m.withFilter()
	m.cursor = m.firstSelectable(0)
	return m
}

func (m tuiModel) toggleScratchOnly() tuiModel {
	m.scratchOnly = !m.scratchOnly
	if m.scratchOnly {
		m.agentOnly = false
	}
	m = m.withFilter()
	m.cursor = m.firstSelectable(0)
	return m
}

// --- Wall keys ---

// handleWallKey runs the wall's own bindings, returning handled=false for the
// keys the shared switch already gets right (enter, ^x, ^c, …). esc is not
// among them — applyWallEsc claims it before this is ever called.
func (m tuiModel) handleWallKey(key string) (tuiModel, tea.Cmd, bool) {
	cols, _ := wallGeometry(m.width, m.bodyHeight(), len(m.tileItems()))
	switch key {
	case "h", "left":
		wm, cmd := m.moveTile(-1)
		return wm, cmd, true
	case "l", "right":
		wm, cmd := m.moveTile(1)
		return wm, cmd, true
	case "k", "up", "ctrl+k":
		wm, cmd := m.moveTile(-max(cols, 1))
		return wm, cmd, true
	case "j", "down", "ctrl+j":
		wm, cmd := m.moveTile(max(cols, 1))
		return wm, cmd, true
	case "[", "pgup":
		wm, cmd := m.turnWallPage(-1)
		return wm, cmd, true
	case "]", "pgdown":
		wm, cmd := m.turnWallPage(1)
		return wm, cmd, true
	case "/":
		m.querying = true
		return m, nil, true
	case "tab":
		// Only a tile the relay can reach is worth focusing, and only when the
		// wall actually drew a grid to focus into (the too-small fallback draws
		// the list instead, per renderWallHints).
		gridCols, gridRows := wallGeometry(m.width, m.bodyHeight(), len(m.tileItems()))
		if item, ok := m.currentItem(); ok && relayable(item) && gridCols > 0 && gridRows > 0 {
			m.focused = true
		}
		return m, nil, true
	case "ctrl+/", "ctrl+_":
		// Back to list+preview inside the popup the wall already has; a tmux
		// popup can't be resized, so nothing is resized here. Focus is a wall
		// concept — leaving it drops the flag so a later ^/ back in starts
		// unfocused rather than silently relaying the next keystroke.
		m.mode = modeList
		m.focused = false
		return m, m.loadPreviewCmd(), true
	case "ctrl+a":
		m = m.toggleAgentOnly().snapWall()
		return m, m.captureWallCmd(), true
	case "ctrl+s":
		m = m.toggleScratchOnly().snapWall()
		return m, m.captureWallCmd(), true
	case "ctrl+x":
		if m.focused {
			// ctrl+x is the wall's kill binding, but also an ordinary chord to
			// type (readline cut, nano save) — must not reach the shared
			// switch's unconfirmed kill while a pane is being typed into.
			return m, nil, true
		}
		return m, nil, false
	case "q":
		if m.query == "" {
			return m, tea.Quit, true
		}
		m.query = ""
		m = m.withFilter().snapWall()
		return m, m.captureWallCmd(), true
	}
	// A half-modal wall where 4 letters move and the other 22 filter cannot
	// express a query like "tmux-og", so no printable falls through to the
	// shared query branch — / opens the prompt instead.
	_, typed := printableKeyText(key)
	return m, nil, typed || key == "backspace"
}

// handleWallQueryKey runs the wall's filter prompt. Every key belongs to the
// prompt until esc/enter closes it; only ^c still quits. esc itself is
// resolved by applyWallEsc before this is called — never reaches here.
func (m tuiModel) handleWallQueryKey(key string) (tea.Model, tea.Cmd) {
	switch {
	case key == "ctrl+c":
		return m, tea.Quit
	case key == "enter":
		m.querying = false
		return m, nil
	case key == "backspace":
		if m.query == "" {
			return m, nil
		}
		runes := []rune(m.query)
		m.query = string(runes[:len(runes)-1])
	default:
		text, ok := printableKeyText(key)
		if !ok {
			return m, nil
		}
		m.query += text
	}
	m = m.withFilter().snapWall()
	return m, m.captureWallCmd()
}

// handleFocusedKey relays an in-scope keystroke (see relayKeyArgs) to the
// focused tile's target, returning handled=false for anything outside the
// relay's scope so it falls through to handleWallKey's own bindings — that is
// how arrows keep moving the wall selection, and ctrl+a/ctrl+s keep toggling
// filters, even while a tile is focused.
func (m tuiModel) handleFocusedKey(key string) (tuiModel, tea.Cmd, bool) {
	item, ok := m.currentItem()
	if !ok || !relayable(item) {
		return m, nil, false
	}
	args, ok := relayKeyArgs(key)
	if !ok {
		return m, nil, false
	}
	target := item.target
	return m, func() tea.Msg {
		sendKeys(target, args, nil) //nolint:errcheck
		return nil
	}, true
}

// escAction is what esc does in the wall, chosen by which of its modal states
// is active.
type escAction int8

const (
	escQuit escAction = iota
	escClearQuery
	escCloseQuery
	escUnfocus
)

// wallEscAction is esc's precedence in the wall (#316): unfocus a focused tile
// before closing the filter prompt before clearing typed filter text before
// quitting. focused and querying can never both be true — a focused "/" is a
// relayed printable key, so it can never open the prompt, and the prompt's own
// switch has no case for tab, so it can never set focused — so exactly one
// rung is ever live; the two flags are still both taken so each rung has its
// own test rather than one inferred from the others.
func wallEscAction(focused, querying bool, query string) escAction {
	switch {
	case focused:
		return escUnfocus
	case querying:
		return escCloseQuery
	case query != "":
		return escClearQuery
	default:
		return escQuit
	}
}

// applyWallEsc runs wallEscAction's verdict. Escape is also one of the relay's
// in-scope keys (relayKeyArgs), but a focused esc always unfocuses rather than
// reaching the pane — a literal Escape can never reach the target through the
// wall (see relayKeyArgs for why that's accepted).
func (m tuiModel) applyWallEsc() (tea.Model, tea.Cmd) {
	switch wallEscAction(m.focused, m.querying, m.query) {
	case escUnfocus:
		m.focused = false
		return m, nil
	case escCloseQuery:
		m.querying = false
		return m, nil
	case escClearQuery:
		m.query = ""
		m = m.withFilter().snapWall()
		return m, m.captureWallCmd()
	default:
		return m, tea.Quit
	}
}

// --- Wall model ---

// relayable reports whether item is a real local pane — not a header, a
// zoxide suggestion, or a remote bridge row — the only shape capture-pane can
// read and send-keys can write. tileItems and the keystroke relay both stop at
// exactly this boundary, so both are defined off the one predicate rather than
// two copies of the same list of exclusions drifting apart.
func relayable(item listItem) bool {
	return item.target != "" && !item.isHeader && item.createPath == "" && !item.isRemoteRow && item.remoteHost == ""
}

// tileItems returns the indices into visible that are relayable.
func (m tuiModel) tileItems() []int {
	var out []int
	for i, item := range m.visible {
		if !relayable(item) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// wallPerPage is the tile count one page holds; 1 when the terminal is too small
// to tile, so paging arithmetic stays well-defined behind the list fallback.
func (m tuiModel) wallPerPage() int {
	cols, rows := wallGeometry(m.width, m.bodyHeight(), len(m.tileItems()))
	return max(cols*rows, 1)
}

func (m tuiModel) wallPageCount() int {
	n := len(m.tileItems())
	pp := m.wallPerPage()
	return max((n+pp-1)/pp, 1)
}

// tilePos is the position in tiles of the first row at or after cursor — where
// the selection snaps to when the filter moves the list underneath it.
func tilePos(tiles []int, cursor int) int {
	for i, idx := range tiles {
		if idx >= cursor {
			return i
		}
	}
	return len(tiles) - 1
}

// snapWall pins the cursor to a tileable row and the page to the one showing it.
func (m tuiModel) snapWall() tuiModel {
	return m.snapWallTo("")
}

// snapWallTo is snapWall for a rebuilt list: it re-finds keep, the target that
// was selected before the rebuild, because an index does not survive one. The
// window list is grouped by session activity, so a session gaining focus shifts
// every index after it and snapping by number lands on a different window.
// Falls back to the index when keep is gone (its pane closed) or empty.
func (m tuiModel) snapWallTo(keep string) tuiModel {
	tiles := m.tileItems()
	if len(tiles) == 0 {
		m.wallPage = 0
		return m
	}
	pos := tilePos(tiles, m.cursor)
	if keep != "" {
		for i, idx := range tiles {
			if m.visible[idx].target == keep {
				pos = i
				break
			}
		}
	}
	m.cursor = tiles[pos]
	m.wallPage = pos / m.wallPerPage()
	return m
}

// moveTile steps the selection by delta tiles in grid order. Crossing a page
// edge turns the page — that is what pagination is in the wall, there is no
// scrolling — and the new page needs its captures now, not in 500ms.
func (m tuiModel) moveTile(delta int) (tuiModel, tea.Cmd) {
	tiles := m.tileItems()
	if len(tiles) == 0 {
		return m, nil
	}
	pos := tilePos(tiles, m.cursor) + delta
	if pos < 0 || pos >= len(tiles) {
		return m, nil
	}
	m.cursor = tiles[pos]
	page := pos / m.wallPerPage()
	if page == m.wallPage {
		return m, nil
	}
	m.wallPage = page
	return m, m.captureWallCmd()
}

// turnWallPage turns a whole page, landing the cursor on its first tile.
func (m tuiModel) turnWallPage(delta int) (tuiModel, tea.Cmd) {
	tiles := m.tileItems()
	page := m.wallPage + delta
	first := page * m.wallPerPage()
	if page < 0 || first >= len(tiles) {
		return m, nil
	}
	m.wallPage = page
	m.cursor = tiles[first]
	return m, m.captureWallCmd()
}

func (m tuiModel) mergeWall(msg wallMsg) tuiModel {
	if m.wallContent == nil {
		m.wallContent = make(map[string]string, len(msg.content))
	}
	for target, content := range msg.content {
		m.wallContent[target] = content
	}
	if msg.bad != "" {
		if m.wallBad == nil {
			m.wallBad = map[string]bool{}
		}
		m.wallBad[msg.bad] = true
	}
	// Panes come and go while the wall is open; without a prune both caches grow
	// for the life of the picker and a reused target could show dead content.
	live := make(map[string]bool, len(m.visible))
	for _, i := range m.tileItems() {
		live[m.visible[i].target] = true
	}
	for target := range m.wallContent {
		if !live[target] {
			delete(m.wallContent, target)
		}
	}
	for target := range m.wallBad {
		if !live[target] {
			delete(m.wallBad, target)
		}
	}
	return m
}

// --- View ---

func (m tuiModel) View() tea.View {
	var content string
	switch {
	case !m.ready:
		content = "Loading..."

	// No bordered wrapper: its Width() would re-measure rows renderTile already
	// sized (see there), so the wall draws its own bottom rule instead.
	case m.mode == modeWall:
		content = strings.Join([]string{
			m.renderSearch(), m.renderWall(), m.renderSeparator(), m.renderWallHints(),
		}, "\n")

	default:
		body := m.renderList()
		if m.showPreview {
			body = m.renderPreview(body)
		}
		borderColor := m.thmColor("@thm_surface_1", "#45475a", "#9ca0b0")
		bordered := lipgloss.NewStyle().
			Width(m.width).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(borderColor).
			Render(body)
		content = lipgloss.JoinVertical(lipgloss.Left, m.renderSearch(), bordered, m.renderHints())
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// --- Layout ---

// bodyHeight is the total height available for list + preview (excludes search/hints/borders).
//
// The search bar is measured, not assumed: it borders only its bottom, so it is
// 2 rows, and 3 when a long query wraps. The other two rows are the body's
// bottom rule and the one-line hint bar.
func (m tuiModel) bodyHeight() int {
	h := m.height - lipgloss.Height(m.renderSearch()) - 2
	if h < 5 {
		return 5
	}
	return h
}

// The bounds keep both panes usable whatever the option says: a 2-row list or a
// 2-row preview is worse than no split at all.
const (
	listRatioDefault = 50
	listRatioMin     = 20
	listRatioMax     = 80
)

// listRatio is the percentage of the body the list gets, from
// @picker_list_ratio. Preview takes the rest, so a lower number is a taller
// preview.
func (m tuiModel) listRatio() int {
	raw := envOrMap("PICKER_LIST_RATIO", m.tmuxOpts, "@picker_list_ratio", "")
	r, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return listRatioDefault
	}
	return min(max(r, listRatioMin), listRatioMax)
}

func (m tuiModel) listHeight() int {
	bh := m.bodyHeight()
	if !m.showPreview {
		return bh
	}
	// Preview gets the rest of the body, minus 1 for the separator.
	return bh * m.listRatio() / 100
}

func (m tuiModel) innerWidth() int {
	return m.width // tmux popup provides the border
}

func (m tuiModel) listWidth() int {
	return m.innerWidth()
}

func (m tuiModel) previewWidth() int {
	return m.innerWidth()
}

func (m tuiModel) previewHeight() int {
	if !m.showPreview {
		return m.bodyHeight()
	}
	return m.bodyHeight() - m.listHeight() - 1 // -1 for separator
}

func (m tuiModel) scrollStart(h int) int {
	start := m.cursor - h/2
	if start < 0 {
		start = 0
	}
	if start+h > len(m.visible) {
		start = len(m.visible) - h
		if start < 0 {
			start = 0
		}
	}
	return start
}

// --- Navigation ---

func (m tuiModel) moveCursor(delta int) tuiModel {
	n := len(m.visible)
	if n == 0 {
		return m
	}
	c := m.cursor
	for {
		c += delta
		if c < 0 || c >= n {
			// Ran past the edge — keep the current cursor rather than
			// landing on a non-selectable title/header row.
			return m
		}
		if m.isSelectable(m.visible[c]) {
			break
		}
	}
	m.cursor = c
	return m
}

func (m tuiModel) isSelectable(item listItem) bool {
	if item.target == "" && item.remoteHost == "" {
		return false
	}
	// A host row pulled in only as tree context for a matching child is not
	// itself a match, so it can't hold the cursor.
	if item.remoteHost != "" && item.remoteSess == "" && item.remoteContextOnly {
		return false
	}
	// In window mode, session headers are not selectable
	return !item.isHeader || !m.windowMode
}

func (m tuiModel) firstSelectable(from int) int {
	for i := from; i < len(m.visible); i++ {
		if m.isSelectable(m.visible[i]) {
			return i
		}
	}
	return from
}

func (m tuiModel) currentTarget() string {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return ""
	}
	return m.visible[m.cursor].target
}

func (m tuiModel) currentItem() (listItem, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return listItem{}, false
	}
	return m.visible[m.cursor], true
}

// restoreCursor re-finds keep (the target selected before a rebuild) in the
// new visible list so a row growing or shrinking never moves the selection
// off it — index-based restore has regressed this list three times before
// (#173, #198, #234), most recently by aliasing onto whatever row now sits
// at a stale index.
func (m tuiModel) restoreCursor(keep string) tuiModel {
	if keep != "" {
		for i, item := range m.visible {
			if item.target == keep {
				m.cursor = i
				return m
			}
		}
		m.cursor = m.firstSelectable(0)
		return m
	}
	// No row to re-find, but the rebuild can still have moved a header under a
	// cursor resting on empty space: the Remote section arrives from an ssh probe
	// seconds after the query is typed, so a query matching only a remote session
	// selects nothing, stays at 0, and finds the Remote header there (#588).
	// refreshMsg and zoxideMsg clamp on selectability for the same reason.
	if m.cursor >= len(m.visible) || !m.isSelectable(m.visible[m.cursor]) {
		m.cursor = m.firstSelectable(0)
	}
	return m
}

// activateCurrent switches to (or creates) the highlighted target and quits.
func (m tuiModel) activateCurrent() (tea.Model, tea.Cmd) {
	item, ok := m.currentItem()
	if !ok || (item.target == "" && item.remoteHost == "") {
		return m, nil
	}
	if m.emitPath != "" {
		// Emit mode never switches or creates — the picker runs in an ssh
		// pty with no attached tmux client, so the only side effect it may
		// have is this one file write (spec D8).
		payload, ok := resolveEmitPick(item)
		if !ok {
			return m, nil
		}
		if err := writeEmitPayload(m.emitPath, payload); err != nil {
			m.statusMsg = err.Error()
			return m, tea.Quit
		}
		return m, tea.Quit
	}
	if item.remoteHost != "" {
		// A changed host key is a MITM signature as much as a reinstall, and
		// only a human comparing fingerprints out of band can clear it. Acting
		// here — even just offering to connect — would train that check away.
		if item.remoteInert {
			m.statusMsg = "host key changed for " + item.remoteHost + " — verify the fingerprint, then update known_hosts by hand"
			return m, nil
		}
		// A Tailscale ACL "check" re-arms on its own checkPeriod regardless of
		// keys or multiplexing — og-remote-auth's ssh-copy-id/ControlMaster
		// flow can't clear it, so Enter must not pretend it can (#486).
		if item.remoteTailscaleCheck {
			m.statusMsg = "tailscale check required for " + item.remoteHost + " — run: ssh " + item.remoteHost
			return m, nil
		}
		// ssh needs a terminal to ask its question and the picker is holding the
		// only one. ExecProcess releases the popup's pty for the duration, so
		// ssh prompts for itself and the secret never passes through this
		// process.
		if item.remoteNeedsAuth {
			authBin := envOrMap("REMOTE_AUTH_BIN", m.tmuxOpts, "@remote_auth_bin", "og-remote-auth")
			cmd := exec.Command(authBin, item.remoteHost)
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return remoteAuthDoneMsg{err: err} })
		}
		if err := openRemoteBridge(m.tmuxOpts, item.remoteHost, item.remoteSess, item.remoteRestore); err != nil {
			m.statusMsg = err.Error()
			return m, nil
		}
		return m, tea.Quit
	}
	if item.createPath != "" {
		if err := createAndSwitch(item.createName, item.createPath); err != nil {
			m.statusMsg = err.Error()
			return m, nil
		}
	} else {
		logEvent("picker", "event", "switch", "target", item.target)
		exec.Command("tmux", "switch-client", "-t", item.target).Run() //nolint:errcheck
	}
	return m, tea.Quit
}

// listRowTop is the first screen row of the list body — just below the search
// box, whose lipgloss bottom border sets its height. The list body has no top
// border, so its first row sits directly beneath.
func (m tuiModel) listRowTop() int {
	return lipgloss.Height(m.renderSearch())
}

// listIndexAt maps screen coords to the selectable visible index under them.
func (m tuiModel) listIndexAt(x, y int) (int, bool) {
	if !m.ready {
		return 0, false
	}
	h := m.listHeight()
	vy := y - m.listRowTop()
	if vy < 0 || vy >= h {
		return 0, false
	}
	idx := m.scrollStart(h) + vy
	if idx < 0 || idx >= len(m.visible) || !m.isSelectable(m.visible[idx]) {
		return 0, false
	}
	return idx, true
}

// inPreview reports whether screen coords fall in the preview pane, which always
// sits below the list (past the separator row).
func (m tuiModel) inPreview(x, y int) bool {
	if !m.showPreview || !m.ready {
		return false
	}
	return y >= m.listRowTop()+m.listHeight()+1 // past the separator row
}

// --- Filter ---

// itemVisible reports whether an item passes the current mode filters
// (scratch/agent). Headers are always visible (pruned separately).
// Zoxide suggestion rows are intentionally hidden by both modes: a dir
// has no agent activity and is never a scratch session. Remote bridge rows
// are exempt instead — they are the only way to reach a host from here, so
// neither mode may drop the section.
func (m tuiModel) itemVisible(item listItem) bool {
	if item.isRemoteRow {
		return true
	}
	if m.scratchOnly && !item.isScratch {
		return false
	}
	if !m.scratchOnly && item.isScratch {
		return false
	}
	if m.agentOnly && !item.hasActiveAgent {
		return false
	}
	return true
}

// scored pairs a listItem with its fuzzy-match score against the current
// query. File-scope (not local to withFilter) so sinkCurrentMatchBelowPeer
// can operate on it too.
type scored struct {
	item  listItem
	score int
}

// Match ranks: sessions always above remote + zoxide suggestions.
const (
	rankSession = iota
	rankRemote
	rankZoxide
)

// matchRank classifies a listItem into one of the three match ranks above.
// Hoisted out of withFilter's sort comparator so the post-pass that follows
// it can reuse the same classification.
func matchRank(it listItem) int {
	switch {
	case it.createPath != "":
		return rankZoxide
	case it.isRemoteRow:
		return rankRemote
	default:
		return rankSession
	}
}

// sinkCurrentMatchBelowPeer moves the currently attached session (at most one
// ever exists — a client attaches to exactly one session) to immediately after
// the LAST matched row it display-collides with on another host. A peer that
// never matched the query isn't in this slice, so there's nothing to sink
// below and this is a no-op.
//
// It must be a post-pass rather than a rule folded into the scored sort's
// comparator: a pairwise "current loses" rule is not transitive (three rows
// whose scores interleave across the collision produce a comparator cycle),
// and sort.SliceStable is unspecified on a non-transitive comparator.
func sinkCurrentMatchBelowPeer(matches []scored) {
	i := -1
	for idx, s := range matches {
		if s.item.current {
			i = idx
			break
		}
	}
	if i < 0 {
		return
	}
	cur := matches[i]
	lastPeer := -1
	for j, p := range matches {
		if j != i && sameDisplayDifferentHost(cur.item.session, cur.item.bridgeHost, p.item.session, p.item.bridgeHost) {
			lastPeer = j
		}
	}
	if lastPeer <= i {
		return
	}
	copy(matches[i:lastPeer], matches[i+1:lastPeer+1])
	matches[lastPeer] = cur
}

func (m tuiModel) withFilter() tuiModel {
	q := strings.ToLower(m.query)

	// No search query — filter by mode only
	if q == "" {
		var out []listItem
		for _, item := range m.allItems {
			if item.isHeader {
				out = append(out, item)
				continue
			}
			if !m.itemVisible(item) {
				continue
			}
			out = append(out, item)
		}
		m.visible = markRemoteTreeEnds(pruneOrphanHeaders(out))
		return m
	}

	// Score and filter matchable items
	var matches []scored
	for _, item := range m.allItems {
		if item.isHeader {
			continue
		}
		if !m.itemVisible(item) {
			continue
		}
		s := fuzzyScore(strings.ToLower(item.searchText), q)
		if s >= 0 {
			matches = append(matches, scored{item: item, score: s})
		}
	}

	// Sort by score descending; sessions always rank above remote + zoxide
	// suggestions. Stable preserves original order for ties.
	sort.SliceStable(matches, func(i, j int) bool {
		ri, rj := matchRank(matches[i].item), matchRank(matches[j].item)
		if ri != rj {
			return ri < rj
		}
		// Remote rows keep collection order so a host's sessions stay grouped
		// under it; ranking them by score breaks the tree.
		if ri == rankRemote {
			return false
		}
		return matches[i].score > matches[j].score
	})

	if !m.windowMode {
		sessionEnd := 0
		for sessionEnd < len(matches) && matchRank(matches[sessionEnd].item) == rankSession {
			sessionEnd++
		}
		sinkCurrentMatchBelowPeer(matches[:sessionEnd])
	}

	if m.windowMode {
		// Re-group under headers, ordered by best child score. groupKey is
		// the session name (session-grouped) or agent state (state-grouped,
		// #229) — whichever the current render built headers on.
		headerMap := make(map[string]listItem)
		for _, item := range m.allItems {
			if item.isHeader {
				headerMap[item.groupKey] = item
			}
		}
		seen := make(map[string]bool)
		var out []listItem
		for _, match := range matches {
			if !seen[match.item.groupKey] {
				seen[match.item.groupKey] = true
				if h, ok := headerMap[match.item.groupKey]; ok {
					out = append(out, h)
				}
			}
			out = append(out, match.item)
		}
		m.visible = out
	} else {
		// Re-insert section headers before the first row of each suggestion
		// block (sessions sort first, remotes next, zoxide last).
		var remoteHeader, sugHeader *listItem
		hostRows := make(map[string]listItem)
		for i := range m.allItems {
			if m.allItems[i].isRemoteHeader {
				remoteHeader = &m.allItems[i]
			}
			if m.allItems[i].isZoxideHeader {
				sugHeader = &m.allItems[i]
			}
			if it := m.allItems[i]; it.remoteHost != "" && it.remoteSess == "" {
				hostRows[it.remoteHost] = it
			}
		}
		seenHost := make(map[string]bool)
		var out []listItem
		for _, match := range matches {
			if remoteHeader != nil && match.item.isRemoteRow {
				out = append(out, *remoteHeader)
				remoteHeader = nil
			}
			// A matching session row pulls its host row in with it, so the
			// tree prefix never dangles under a host the query dropped.
			if h := match.item.remoteHost; h != "" && !seenHost[h] {
				seenHost[h] = true
				if row, ok := hostRows[h]; ok && match.item.remoteSess != "" {
					row.remoteContextOnly = true
					out = append(out, row)
				}
			}
			if sugHeader != nil && match.item.createPath != "" {
				out = append(out, *sugHeader)
				sugHeader = nil
			}
			out = append(out, match.item)
		}
		m.visible = markRemoteTreeEnds(out)
	}
	return m
}

// markRemoteTreeEnds gives the last visible session row under each host the
// closing tree glyph. Which row is last depends on the filter, not on the order
// the rows were collected in.
func markRemoteTreeEnds(items []listItem) []listItem {
	for i := range items {
		if items[i].displayEnd == "" {
			continue
		}
		next := i + 1
		sibling := next < len(items) &&
			items[next].remoteSess != "" &&
			items[next].remoteHost == items[i].remoteHost
		if !sibling {
			items[i].display = items[i].displayEnd
			items[i].plain = items[i].plainEnd
		}
	}
	return items
}

func pruneOrphanHeaders(items []listItem) []listItem {
	out := make([]listItem, 0, len(items))
	for i, item := range items {
		if !item.isHeader {
			out = append(out, item)
			continue
		}
		hasChild := i+1 < len(items) && !items[i+1].isHeader
		if hasChild {
			out = append(out, item)
		}
	}
	return out
}

// --- Commands ---

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// previewInterval is the preview's reload clock. Fast enough that a build or a
// Claude turn reads as moving; the fork it costs is skipped entirely when the
// preview is hidden, because loadPreviewCmd returns nil then.
const previewInterval = 400 * time.Millisecond

// The clock runs for the whole session rather than being started and stopped
// with the preview: restarting it on every ^/ would leave one live loop per
// toggle, all firing.
func previewTickCmd() tea.Cmd {
	return tea.Tick(previewInterval, func(time.Time) tea.Msg { return previewTickMsg{} })
}

// wallInterval is the wall's capture clock. Slower than the preview's 400ms
// because one batch redraws a page of panes rather than a single one; still fast
// enough that a build or a Claude turn reads as moving.
const wallInterval = 500 * time.Millisecond

// Same reasoning as previewTickCmd: one clock for the whole session, or every
// ^/ leaves another live loop behind.
func wallTickCmd() tea.Cmd {
	return tea.Tick(wallInterval, func(time.Time) tea.Msg { return wallTickMsg{} })
}

// captureWallCmd batches the current page's captures into one tmux call.
func (m tuiModel) captureWallCmd() tea.Cmd {
	if m.mode != modeWall {
		return nil
	}
	targets := m.wallPageTargets()
	if len(targets) == 0 {
		return nil
	}
	return func() tea.Msg {
		// captureViaSelf: never capture the picker's own popup pane over the
		// window/session it covers.
		content, err := captureViaSelf(targets, selfCaptureTarget, nil)
		msg := wallMsg{content: content}
		var cErr *captureErr
		if errors.As(err, &cErr) {
			msg.bad = cErr.Target
		}
		return msg
	}
}

// wallPageTargets is the page's capture set. Targets a previous batch died on
// are left out: one gone pane would otherwise abort every batch after it.
func (m tuiModel) wallPageTargets() []string {
	tiles := m.tileItems()
	pp := m.wallPerPage()
	var out []string
	for i := m.wallPage * pp; i < len(tiles) && i < m.wallPage*pp+pp; i++ {
		target := m.visible[tiles[i]].target
		if m.wallBad[target] {
			continue
		}
		out = append(out, target)
	}
	return out
}

func (m tuiModel) refreshDataCmd() tea.Cmd {
	wm := m.windowMode
	sg := m.stateGrouped
	opts := m.tmuxOpts
	theme := m.theme
	cur := m.currentSession
	lw := m.listWidth() // capture the value; the closure runs off-thread
	return func() tea.Msg {
		snap := collectPanesSnapshot()
		panes := collectAgentPanes(snap)
		var items []listItem
		if wm {
			items = buildWindowItems(opts, panes, theme, lw, sg)
		} else {
			items = buildSessionItems(opts, snap, panes, theme, true, cur)
		}
		// Always send — spinners need to animate even without structural changes.
		return refreshMsg{items: items}
	}
}

// zoxideCmd collects directory suggestions off the first-paint path (session
// mode only). The result merges in via zoxideMsg once stat-ing every zoxide
// dir completes, so the popup paints with sessions immediately.
func (m tuiModel) zoxideCmd() tea.Cmd {
	opts := m.tmuxOpts
	return func() tea.Msg {
		return zoxideMsg{items: collectZoxideItems(opts)}
	}
}

// remoteCmd collects remote host/session rows off the first-paint path
// (session mode only). ssh probes are bounded; the result merges via remoteMsg.
func (m tuiModel) remoteCmd() tea.Cmd {
	opts := m.tmuxOpts
	return func() tea.Msg {
		return remoteMsg{items: collectRemoteItems(opts, collectBridgeSessions(), nil, nil)}
	}
}

// recombine rebuilds allItems from the session base plus the async suggestion
// rows, so a 1s refresh (sessions only) doesn't drop loaded remote/zoxide entries.
func (m tuiModel) recombine() tuiModel {
	all := make([]listItem, 0, len(m.sessionItems)+len(m.remoteItems)+len(m.zoxideItems)+1)
	all = append(all, m.sessionItems...)
	all = append(all, m.remoteItems...)
	all = append(all, m.zoxideItems...)
	// A serverless remote still shows its zoxide dirs (spec D3), so the view is
	// only blank when both are empty — and that is indistinguishable from a
	// zoxide probe still in flight, hence zoxideReady. Deciding this at
	// item-build time instead would flash the row on every host's first paint.
	if m.emitPath != "" && m.zoxideReady && noSessionRows(m.sessionItems) && len(m.zoxideItems) == 0 {
		all = append(all, emitEmptyRow(m.tmuxOpts))
	}
	m.allItems = all
	return m
}

func (m tuiModel) loadPreviewCmd() tea.Cmd {
	// The wall draws no preview, so its clock must not pay for one on top of the
	// page it already captures.
	if !m.showPreview || m.mode == modeWall {
		return nil
	}
	item, ok := m.currentItem()
	if !ok || item.target == "" {
		// A header row or a query that matched nothing. Emitting an empty
		// preview clears the last target's content instead of leaving it on
		// screen next to a selection it doesn't belong to; the handler's
		// target gate accepts it because currentTarget() is "" here too.
		return func() tea.Msg { return previewMsg{target: "", scrollTop: true} }
	}
	t := item.target
	if cp := item.createPath; cp != "" {
		return func() tea.Msg {
			return previewMsg{content: listDir(cp), target: t, scrollTop: true}
		}
	}
	if item.remoteHost != "" {
		host, sess := item.remoteHost, item.remoteSess
		inert, needsAuth := item.remoteInert, item.remoteNeedsAuth
		tailscaleCheck, tailscaleURL := item.remoteTailscaleCheck, item.remoteTailscaleURL
		return func() tea.Msg {
			var msg string
			switch {
			case inert:
				msg = "remote bridge → " + host +
					"\n\nThe host key changed since it was last accepted. That is what a" +
					"\nreinstalled host looks like — and also what an interception looks" +
					"\nlike. Compare the fingerprint out of band, then fix known_hosts by" +
					"\nhand. Enter does nothing here."
			case tailscaleCheck:
				msg = "remote bridge → " + host +
					"\n\nA Tailscale ACL check is blocking this host, not ssh auth —" +
					"\nog-remote-auth's remedy can't clear it. Enter does nothing" +
					"\nhere.\n\nRun this yourself in a terminal:\n\n  ssh " + host
				if tailscaleURL != "" {
					msg += "\n\n(last-seen login URL, may be stale — the probe that" +
						"\ncaptured it already timed out):\n" + tailscaleURL
				}
			case needsAuth:
				msg = "remote bridge → " + host +
					"\n\nEnter runs og-remote-auth: ssh takes this popup and asks for" +
					"\nitself. It opens one shared connection, so the bridge and every" +
					"\nlater probe reuse it without asking again."
			default:
				msg = "remote bridge → " + host
				if sess != "" {
					msg += "/" + sess
				}
				msg += "\n\nEnter runs og-remote-open (outbound ssh)."
			}
			return previewMsg{content: msg, target: t, scrollTop: true}
		}
	}
	return func() tea.Msg {
		// selfCaptureTarget: never capture the picker's own popup pane over
		// the window/session it covers.
		out, err := exec.Command("tmux", "capture-pane", "-t", selfCaptureTarget(t), "-p", "-e").Output()
		if err != nil {
			return previewMsg{content: "(no preview available)", target: t}
		}
		// Same untrusted content as the wall's captures, through a different path.
		content := stripStringEscapes(strings.TrimRight(string(out), "\n "))
		// Reset background at end of each line to prevent ANSI color bleeding
		// into empty viewport padding cells (e.g. opencode's black input area).
		content = strings.ReplaceAll(content, "\n", "\033[49m\n") + "\033[49m"
		return previewMsg{content: content, target: t}
	}
}

// sameDisplayDifferentHost reports whether two sessions display under the
// same name but live on different hosts (bridgeHost=="" counts as local) —
// the collision that makes "current" ambiguous with a session the user is
// actually trying to reach.
func sameDisplayDifferentHost(nameA, hostA, nameB, hostB string) bool {
	return hostA != hostB && sessionDisplayName(nameA, hostA) == sessionDisplayName(nameB, hostB)
}

// sortSessionsForDisplay orders sessions by activity desc, then name asc, and
// nothing more. Sinking the current session below a same-display-name mirror
// (#551) is sinkCurrentMatchBelowPeer's job and the filtered list's alone: an
// unfiltered list shows every row with its Host column, and reordering it
// would move rows a user navigates by position.
func sortSessionsForDisplay(sessions []sessionData) {
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].activity != sessions[j].activity {
			return sessions[i].activity > sessions[j].activity
		}
		return sessions[i].name < sessions[j].name
	})
}

// --- Item builders ---

// resourcePlaceholder fills the CPU/Mem columns on the first paint, before the
// async resource pass (ps -A) returns. Single-byte so the len()-based column
// padding stays aligned (multibyte glyphs measure wide in bytes, narrow in cells).
const resourcePlaceholder = "-"

func buildSessionItems(tmuxOpts map[string]string, snap panesSnapshot, agentPanes []agentPaneInfo, theme string, withResources bool, currentSession string) []listItem {
	sessions := snap.sessions()
	agentMap := aggregateAgentBySession(agentPanes)
	mergeAgent(sessions, agentMap)

	for i := range sessions {
		sessions[i].current = sessions[i].name == currentSession
	}

	// Resource collection forks `ps -A`, so it stays off the first-paint path:
	// the initial render passes withResources=false and an immediate async
	// refresh fills real CPU/Mem a beat later.
	var resCh chan map[string]sessionResources
	if withResources {
		resCh = make(chan map[string]sessionResources, 1)
		go func() { resCh <- collectSessionResources(sessions) }()
	}
	// Merged before the rows are built: the walk's agent commands join a
	// session's proc list, and the icon column is built from that list.
	if withResources {
		mergeResources(sessions, <-resCh)
		mergeRemoteResources(sessions)
	}

	thmMauve := envOrMap("THM_MAUVE", tmuxOpts, "@thm_mauve", "#cba6f7")
	thmBlue := envOrMap("THM_BLUE", tmuxOpts, "@thm_blue", "#89b4fa")
	thmSubtext0 := envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8")
	iDir := envOrMap("PICKER_ICON_DIR", tmuxOpts, "@icon_dir", iconDir)
	iSess := envOrMap("PICKER_ICON_SESSION", tmuxOpts, "@icon_session", iconSession)
	iHost := envOrMap("PICKER_ICON_HOST", tmuxOpts, "@icon_host", iconHost)
	iProcs := envOrMap("PICKER_ICON_PROCS", tmuxOpts, "@icon_procs", iconProcs)
	iCPU := envOrMap("PICKER_ICON_CPU", tmuxOpts, "@icon_cpu", iconCPU)
	iMem := envOrMap("PICKER_ICON_MEM", tmuxOpts, "@icon_mem", iconMem)

	// Column labels are glyph + word. Each one sets a floor under its column's
	// width below: the columns are otherwise sized by their data alone, and a
	// label wider than its cell would push every later column right in the
	// header only, which is exactly the drift the alignment tests catch.
	lblSess := "Session"
	lblHost := iHost + " Host"
	lblProcs := iProcs + " Procs"
	lblCPU := iCPU + " CPU"
	lblMem := iMem + " Mem"
	lblPath := "Path"

	cMauve := ansiFg(thmMauve)
	cBlue := ansiFg(thmBlue)
	cDim := ansiFg(thmSubtext0)
	hostColor := hostColorFunc(tmuxOpts)
	rc := newResourceColors(tmuxOpts)
	reset := "\033[0m"
	dim := "\033[2m"

	sortSessionsForDisplay(sessions)

	type row struct {
		sess   *sessionData
		name   string // display name; a mirror drops the host its Host cell already carries
		icons  string
		iconDW int
	}
	rows := make([]row, len(sessions))
	maxName, maxIconDW := 0, 0
	for i := range sessions {
		s := &sessions[i]
		name := sessionDisplayName(s.name, s.bridgeHost)
		if len(name) > maxName {
			maxName = len(name)
		}
		icons, dw := buildProcIcons(s.procs, maxIconsPicker)
		icons, dw = appendAgentIcon(icons, dw, s.agent, theme, dim, reset)
		icons, dw = appendIssueIDs(icons, dw, s.agent.issues, cDim, reset)
		rows[i] = row{sess: s, name: name, icons: icons, iconDW: dw}
		if dw > maxIconDW {
			maxIconDW = dw
		}
	}
	maxName = max(maxName, visibleWidth(lblSess))
	iconCol := max(maxIconDW+1, max(5, visibleWidth(lblProcs)))
	for i := range rows {
		rows[i].icons = padToWidth(rows[i].icons, rows[i].iconDW, iconCol)
	}
	emptyIcons := strings.Repeat(" ", iconCol)

	// Host column: only remote-bridge mirrors carry @bridge_host, so an
	// all-local list spends no width on it at all.
	hostCol := 0
	for i := range sessions {
		hostCol = max(hostCol, len(sessions[i].bridgeHost))
	}
	if hostCol > 0 {
		hostCol = max(hostCol, visibleWidth(lblHost))
	}
	// color wraps the text only, so the plain variant (color "") stays escape-free.
	hostCell := func(host, color string) string {
		if hostCol == 0 {
			return ""
		}
		tail := strings.Repeat(" ", max(0, hostCol-visibleWidth(host))) + "  "
		if host == "" || color == "" {
			return host + tail
		}
		return color + host + reset + tail
	}

	// Pre-compute CPU and MEM strings separately so the "/" aligns
	cpuStrs := make([]string, len(rows))
	memStrs := make([]string, len(rows))
	maxCPU, maxMem := max(cpuColWidth(), visibleWidth(lblCPU)), visibleWidth(lblMem)
	for i, r := range rows {
		if withResources && !r.sess.resUnknown {
			cpuStrs[i] = formatCPU(r.sess.cpuPct)
			memStrs[i] = formatMem(r.sess.memMB)
		} else {
			cpuStrs[i] = resourcePlaceholder
			memStrs[i] = resourcePlaceholder
		}
		if len(cpuStrs[i]) > maxCPU {
			maxCPU = len(cpuStrs[i])
		}
		if len(memStrs[i]) > maxMem {
			maxMem = len(memStrs[i])
		}
	}

	// Build header row. Every label is a single glyph, so each cell is sized by
	// display width — a nerd glyph is one cell but four bytes, and len() here
	// would pad every column three cells short.
	//
	// The CPU and Mem labels mirror each other around the " / ": CPU padded on
	// the left, Mem on the right, so the pair reads as one CPU/Mem unit rather
	// than two labels adrift in their fields. Each field keeps the width the
	// rows give it, so Path stays in place.
	hdrCPUPad := strings.Repeat(" ", max(0, maxCPU-visibleWidth(lblCPU)))
	hdrMemPad := strings.Repeat(" ", max(0, maxMem-visibleWidth(lblMem)))
	hdrRes := hdrCPUPad + lblCPU + " / " + lblMem + hdrMemPad
	hdrName := lblSess + strings.Repeat(" ", max(0, maxName-visibleWidth(lblSess)))
	hdrDisplay := fmt.Sprintf("%s %s  %s%s  %s  %s %s",
		cDim+iSess+reset,
		cDim+hdrName+reset,
		hostCell(lblHost, cDim),
		cDim+padToWidth(lblProcs, visibleWidth(lblProcs), iconCol)+reset,
		cDim+hdrRes+reset,
		cDim+iDir+reset,
		cDim+lblPath+reset,
	)
	hdrPlain := fmt.Sprintf("%s %s  %s%s  %s  %s %s",
		iSess, hdrName,
		hostCell(lblHost, ""), padToWidth(lblProcs, visibleWidth(lblProcs), iconCol), hdrRes, iDir, lblPath,
	)

	home := os.Getenv("HOME")
	items := make([]listItem, 0, len(rows)+1)
	items = append(items, listItem{
		display:        hdrDisplay,
		plain:          hdrPlain,
		isColumnHeader: true,
	})
	for i, r := range rows {
		pad := strings.Repeat(" ", max(0, maxName-len(r.name)))
		cName := cMauve
		if r.sess.bridgeHost != "" {
			cName = hostColor(r.sess.bridgeHost)
		}
		icons := r.icons
		if icons == "" {
			icons = emptyIcons
		}
		shortPath := r.sess.path
		if home != "" && strings.HasPrefix(shortPath, home) {
			shortPath = "~" + shortPath[len(home):]
		}
		cpuPad := strings.Repeat(" ", max(0, maxCPU-len(cpuStrs[i])))
		memPad := strings.Repeat(" ", max(0, maxMem-len(memStrs[i])))
		display := fmt.Sprintf("%s %s%s  %s%s  %s%s %s %s%s  %s %s",
			cName+iSess+reset,
			cName+r.name+reset,
			pad,
			hostCell(r.sess.bridgeHost, cName),
			icons,
			cpuPad,
			rc.cpuColor(r.sess.cpuPct, r.sess.cores)+cpuStrs[i]+reset,
			cDim+"/"+reset,
			memPad,
			rc.memColor(r.sess.memMB)+memStrs[i]+reset,
			cBlue+iDir+reset,
			cDim+shortPath+reset,
		)
		resPlain := cpuPad + cpuStrs[i] + " / " + memPad + memStrs[i]
		plain := fmt.Sprintf("%s %s%s  %s%s  %s  %s %s",
			iSess, r.name, pad, hostCell(r.sess.bridgeHost, ""),
			stripANSI(icons), resPlain, iDir, shortPath,
		)
		// A mirror is searchable by its host, like the Remote rows, even when
		// its name doesn't carry the <host>- prefix.
		search := r.sess.name
		if h := r.sess.bridgeHost; h != "" && !strings.HasPrefix(search, h+"-") {
			search += " " + h
		}
		items = append(items, listItem{
			target:         r.sess.name,
			display:        display,
			plain:          plain,
			searchText:     search,
			session:        r.sess.name,
			bridgeHost:     r.sess.bridgeHost,
			current:        r.sess.current,
			hasActiveAgent: isActiveState(agentPriority(r.sess.agent)),
			isScratch:      strings.HasPrefix(r.sess.name, "scratch-"),
		})
	}
	return items
}

// collectZoxideItems builds the "New session" suggestion rows (header + zoxide
// dirs). Runs off the first-paint path so its zoxide query + per-dir stat walk
// never blocks the popup; the result merges in via zoxideMsg.
func collectZoxideItems(tmuxOpts map[string]string) []listItem {
	zoxExclude := parseExcludePatterns(envOrMap("PICKER_ZOXIDE_EXCLUDE", tmuxOpts, "@picker_zoxide_exclude", ""))
	sugs := collectZoxide(collectSessions(), zoxExclude)
	if len(sugs) == 0 {
		return nil
	}

	cBlue := ansiFg(envOrMap("THM_BLUE", tmuxOpts, "@thm_blue", "#89b4fa"))
	cDim := ansiFg(envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8"))
	iDir := envOrMap("PICKER_ICON_DIR", tmuxOpts, "@icon_dir", iconDir)
	reset := "\033[0m"
	home := os.Getenv("HOME")

	rule := "── New session " + strings.Repeat("─", 220)
	items := make([]listItem, 0, len(sugs)+1)
	items = append(items, listItem{
		display:        cDim + rule + reset,
		plain:          rule,
		isHeader:       true,
		isZoxideHeader: true,
		headerLabel:    "New session",
		headerIcon:     envOrMap("PICKER_ICON_DIR", tmuxOpts, "@icon_dir", iconDir),
	})
	for _, sg := range sugs {
		shortPath := sg.path
		if home != "" && strings.HasPrefix(shortPath, home) {
			shortPath = "~" + shortPath[len(home):]
		}
		display := fmt.Sprintf("%s %s  %s", cBlue+iDir+reset, sg.name, cDim+shortPath+reset)
		plain := fmt.Sprintf("%s %s  %s", iDir, sg.name, shortPath)
		items = append(items, listItem{
			target:     sg.path,
			createPath: sg.path,
			createName: sg.name,
			display:    display,
			plain:      plain,
			searchText: sg.name + " " + shortPath,
		})
	}
	return items
}

func buildWindowItems(tmuxOpts map[string]string, agentPanes []agentPaneInfo, theme string, width int, stateGrouped bool) []listItem {
	return renderWindowItems(collectWindows(), tmuxOpts, agentPanes, theme, width, stateGrouped)
}

const (
	defaultIdentityCap = 32
	minIdentityCap     = 12
	maxIdentityCap     = 48
	// layoutGaps: tree(2)+marker(1) glyph cells + 3 inter-field gaps + 1 gap
	// before the PR badge = 7.
	layoutGaps = 7
)

// identityCapFor sizes the inline-identity column from the terminal width, so
// wider terminals show longer ticket titles and narrow ones shorten them while
// the icon and PR columns stay pinned. width<=0 (size unknown) uses the default.
func identityCapFor(width, leadDW, iconDW, prDW int) int {
	if width <= 0 {
		return defaultIdentityCap
	}
	identCap := width - leadDW - iconDW - prDW - layoutGaps
	if identCap < minIdentityCap {
		return minIdentityCap
	}
	if identCap > maxIdentityCap {
		return maxIdentityCap
	}
	return identCap
}

// windowGroup is one header's worth of window rows, in render order. key is
// either an owning session name (session-grouped mode) or an agent priority
// state ("" = no agent, state-grouped mode).
type windowGroup struct {
	key     string
	windows []*windowData
}

// groupWindowsBySession buckets windows under their owning session, ordered
// by session activity (busiest first) then name — window mode's default
// grouping, unchanged from before #229.
func groupWindowsBySession(windows []windowData, sessActivity map[string]int64) []windowGroup {
	order := []string{}
	byName := map[string]*windowGroup{}
	for i := range windows {
		w := &windows[i]
		g, ok := byName[w.session]
		if !ok {
			g = &windowGroup{key: w.session}
			byName[w.session] = g
			order = append(order, w.session)
		}
		g.windows = append(g.windows, w)
	}
	groups := make([]windowGroup, len(order))
	for i, name := range order {
		groups[i] = *byName[name]
	}
	sort.Slice(groups, func(i, j int) bool {
		ai, aj := sessActivity[groups[i].key], sessActivity[groups[j].key]
		if ai != aj {
			return ai > aj
		}
		return groups[i].key < groups[j].key
	})
	for i := range groups {
		sort.Slice(groups[i].windows, func(a, b int) bool {
			return groups[i].windows[a].index < groups[i].windows[b].index
		})
	}
	return groups
}

// groupWindowsByState buckets windows by claude_priority_state (the same
// priority ordering the shell side uses for any agent, not only Claude — see
// scripts/lib-claude.sh), in agentStateOrder — the same priority the status
// bar and pane pollers use (#229). Windows with no agent state at all collapse
// into one trailing group (key ""), never one group per stateless window.
// Within a group, windows keep session-activity order so the busiest
// session's windows still lead — the same tiebreak groupWindowsBySession uses.
func groupWindowsByState(windows []windowData, sessActivity map[string]int64) []windowGroup {
	byKey := map[string]*windowGroup{}
	for i := range windows {
		w := &windows[i]
		key := agentPriority(w.agent)
		g, ok := byKey[key]
		if !ok {
			g = &windowGroup{key: key}
			byKey[key] = g
		}
		g.windows = append(g.windows, w)
	}
	order := append(append([]string{}, agentStateOrder...), "")
	groups := make([]windowGroup, 0, len(order))
	for _, key := range order {
		if g, ok := byKey[key]; ok {
			groups = append(groups, *g)
		}
	}
	for i := range groups {
		gw := groups[i].windows
		sort.Slice(gw, func(a, b int) bool {
			wa, wb := gw[a], gw[b]
			if sessActivity[wa.session] != sessActivity[wb.session] {
				return sessActivity[wa.session] > sessActivity[wb.session]
			}
			if wa.session != wb.session {
				return wa.session < wb.session
			}
			return wa.index < wb.index
		})
	}
	return groups
}

// renderWindowItems is the pure rendering half of buildWindowItems, split out so
// the enriched row layout can be unit-tested with synthetic windows. width is
// the list width in cells (0 = unknown → default identity cap).
func renderWindowItems(windows []windowData, tmuxOpts map[string]string, agentPanes []agentPaneInfo, theme string, width int, stateGrouped bool) []listItem {
	agentByWin := aggregateAgentByWindow(agentPanes)
	mergeAgentWindows(windows, agentByWin)

	thmMauve := envOrMap("THM_MAUVE", tmuxOpts, "@thm_mauve", "#cba6f7")
	thmGreen := envOrMap("THM_GREEN", tmuxOpts, "@thm_green", "#a6e3a1")
	thmRed := envOrMap("THM_RED", tmuxOpts, "@thm_red", "#f38ba8")
	thmPeach := envOrMap("THM_PEACH", tmuxOpts, "@thm_peach", "#fab387")
	thmSubtext0 := envOrMap("THM_SUBTEXT_0", tmuxOpts, "@thm_subtext_0", "#a6adc8")
	thmOverlay1 := envOrMap("THM_OVERLAY_1", tmuxOpts, "@thm_overlay_1", "#7f849c")
	thmOverlay0 := envOrMap("THM_OVERLAY_0", tmuxOpts, "@thm_overlay_0", "#6c7086")
	iSess := envOrMap("PICKER_ICON_SESSION", tmuxOpts, "@icon_session", iconSession)
	iBranch := envOrMap("PICKER_ICON_BRANCH", tmuxOpts, "@icon_branch", iconBranch)

	cMauve := ansiFg(thmMauve)
	cGreen := ansiFg(thmGreen)
	cDim := ansiFg(thmSubtext0)
	cFaint := ansiFg(thmOverlay1)
	reset := "\033[0m"
	dim := "\033[2m"
	prCols := prColors{success: cGreen, failure: ansiFg(thmRed), pending: ansiFg(thmPeach), merged: cMauve, closed: ansiFg(thmOverlay0), required: ansiFg(thmOverlay0), underline: "\033[4m", reset: reset}

	sessActivity := collectSessionActivity()
	var groups []windowGroup
	if stateGrouped {
		groups = groupWindowsByState(windows, sessActivity)
	} else {
		groups = groupWindowsBySession(windows, sessActivity)
	}

	// Pass A builds every fixed-width piece and the raw identity parts, and
	// tracks the column maxima (lead prefix, icons, PR). The identity cap is
	// then derived from the terminal width, and pass B truncates + aligns.
	// Keyed by "session:index" (not by group) so pass B can look a window's
	// pieces up the same way regardless of which grouping mode built groups.
	type rawIdentity struct {
		kind      int
		id, rest  string
		text      string
		leadGlyph string
	}
	type renderedWin struct {
		win         *windowData
		name        string
		icons       string
		iconDW      int
		leadPlain   string
		leadColored string
		leadDW      int
		ident       rawIdentity
		identSearch string
		prBadge     string
		prPlain     string
		crewName    string
	}
	winRows := make(map[string]renderedWin, len(windows))
	maxLeadDW, maxIconDW, maxPrDW, maxZoomDW := 0, 0, 0, 0
	for i := range windows {
		w := &windows[i]
		icons, dw := buildProcIcons(w.procs, maxIconsPicker)
		icons, dw = appendAgentIcon(icons, dw, w.agent, theme, dim, reset)
		icons, dw = appendIssueIDs(icons, dw, w.agent.issues, cDim, reset)

		name := truncateCells(w.name, 40)

		leadPlain := fmt.Sprintf("%d: ", w.index)
		leadColored := leadPlain
		if w.crewName != "" {
			leadPlain += w.crewName + " "
			crew := w.crewName
			if c := ansiFgTmux(w.crewColor); c != "" {
				crew = c + w.crewName + reset
			}
			leadColored += crew + " "
		}
		leadDW := iconCellWidth(leadPlain)

		var ri rawIdentity
		var idSearch string
		if w.labelID != "" {
			ri = rawIdentity{kind: 1, id: w.labelID, rest: w.labelRest}
			idSearch = w.labelID + w.labelRest
		} else if w.branch != "" && !branchEchoesName(w.branch, w.name) && w.branch != "main" && w.branch != "master" {
			ri = rawIdentity{kind: 2, text: w.branch, leadGlyph: ""}
			if iBranch != "" {
				ri.leadGlyph = iBranch + " "
			}
			idSearch = w.branch
		} else if w.bridgeName != "" {
			ri = rawIdentity{kind: 0, text: truncateCells(w.bridgeName, 40)}
			idSearch = w.bridgeName
		} else {
			ri = rawIdentity{kind: 0, text: name}
			idSearch = name
		}

		prBadge := colorPRBadge(w.prPlain, w.prState, w.prCheck, w.prMergeable, w.prReview, w.prAutoMerge, prCols)
		prPlain := strings.TrimSpace(w.prPlain)
		prDW := iconCellWidth(prPlain)

		winRows[fmt.Sprintf("%s:%d", w.session, w.index)] = renderedWin{
			win: w, name: name, icons: icons, iconDW: dw,
			leadPlain: leadPlain, leadColored: leadColored, leadDW: leadDW,
			ident: ri, identSearch: idSearch,
			prBadge: prBadge, prPlain: prPlain, crewName: w.crewName,
		}
		maxLeadDW = max(maxLeadDW, leadDW)
		maxIconDW = max(maxIconDW, dw)
		maxPrDW = max(maxPrDW, prDW)
		if w.zoomed {
			maxZoomDW = max(maxZoomDW, iconCellWidth(" 󰁌"))
		}
	}
	iconCol := max(maxIconDW+1, 3)
	identityCap := identityCapFor(width, maxLeadDW+maxZoomDW, iconCol, maxPrDW)
	labelCol := maxLeadDW + identityCap + maxZoomDW

	// truncID renders a rawIdentity to (colored, plain) within budget cells.
	truncID := func(ri rawIdentity, budget int) (string, string) {
		switch ri.kind {
		case 1: // issue: id accent + dim title
			id := ri.id
			idW := iconCellWidth(id)
			rest := ri.rest
			if idW >= budget {
				id = truncateCells(id, budget)
				rest = ""
				idW = iconCellWidth(id)
			} else if idW+iconCellWidth(rest) > budget {
				rest = truncateCells(rest, budget-idW)
			}
			plain := id + rest
			colored := cMauve + id + reset
			if rest != "" {
				colored += cDim + rest + reset
			}
			return colored, plain
		case 2: // branch: faint, optional glyph
			br := truncateCells(ri.text, max(budget-iconCellWidth(ri.leadGlyph), 1))
			plain := ri.leadGlyph + br
			return cFaint + plain + reset, plain
		default: // name: plain
			nm := truncateCells(ri.text, budget)
			return nm, nm
		}
	}

	var items []listItem
	for _, g := range groups {
		var headerDisplay, headerPlain, headerSession string
		var headerHasAgent bool
		if stateGrouped {
			headerDisplay, headerPlain = stateGroupHeader(g.key, theme, cFaint)
			headerHasAgent = isActiveState(g.key)
		} else {
			sessHasAgent := false
			for _, w := range g.windows {
				key := fmt.Sprintf("%s:%d", w.session, w.index)
				if cc, ok := agentByWin[key]; ok && isActiveState(agentPriority(*cc)) {
					sessHasAgent = true
					break
				}
			}
			headerDisplay = fmt.Sprintf("%s %s", cMauve+iSess+reset, cMauve+g.key+reset)
			headerPlain = fmt.Sprintf("%s %s", iSess, g.key)
			headerSession = g.key
			headerHasAgent = sessHasAgent
		}
		items = append(items, listItem{
			target:         g.key,
			display:        headerDisplay,
			plain:          headerPlain,
			searchText:     g.key,
			isHeader:       true,
			session:        headerSession,
			groupKey:       g.key,
			hasActiveAgent: headerHasAgent,
		})

		multiWin := len(g.windows) > 1
		for wi, w := range g.windows {
			r := winRows[fmt.Sprintf("%s:%d", w.session, w.index)]

			activeMarker := " "
			if w.active && multiWin {
				activeMarker = cGreen + "▸" + reset
			}

			icons := r.icons
			if icons == "" {
				icons = strings.Repeat(" ", iconCol)
			} else {
				icons = padToWidth(icons, r.iconDW, iconCol)
			}

			tree := "├─"
			if wi == len(g.windows)-1 {
				tree = "╰─"
			}

			identCap := identityCap
			prefixColored, prefixPlain := "", ""
			if stateGrouped {
				prefixColored, prefixPlain, identCap = foldSessionPrefix(w.session, cDim, reset, identityCap)
			}
			idColored, idPlain := truncID(r.ident, identCap)
			idColored = prefixColored + idColored
			idPlain = prefixPlain + idPlain

			zoom := ""
			if w.zoomed {
				zoom = " 󰁌"
			}
			lead := padToWidth(r.leadColored, r.leadDW, maxLeadDW)
			leadPlainPadded := padToWidth(r.leadPlain, r.leadDW, maxLeadDW)
			labelColored := lead + idColored + zoom
			labelPlain := leadPlainPadded + idPlain + zoom
			labelColored = padToWidth(labelColored, iconCellWidth(labelPlain), labelCol)
			labelPlain = padToWidth(labelPlain, iconCellWidth(labelPlain), labelCol)

			display := fmt.Sprintf("%s %s %s %s",
				cDim+tree+reset, activeMarker, labelColored, icons)
			plain := fmt.Sprintf("%s %s %s %s",
				tree, strings.TrimSpace(stripANSI(activeMarker)), labelPlain, stripANSI(icons))
			if r.prBadge != "" {
				display += " " + r.prBadge
				plain += " " + r.prPlain
			}
			display = strings.TrimRight(display, " ")
			plain = strings.TrimRight(plain, " ")

			search := w.session + " " + r.name
			if r.identSearch != "" {
				search += " " + r.identSearch
			}
			if r.prPlain != "" {
				search += " " + r.prPlain
			}
			if r.crewName != "" {
				search += " " + r.crewName
			}
			items = append(items, listItem{
				target:         fmt.Sprintf("%s:%d", w.session, w.index),
				display:        display,
				plain:          plain,
				searchText:     search,
				session:        w.session,
				groupKey:       g.key,
				bridgeHost:     w.bridgeHost,
				bridgePane:     w.bridgePane,
				bridgeSock:     w.bridgeSock,
				hasActiveAgent: isActiveState(agentPriority(w.agent)),
			})
		}
	}
	return items
}

// sessionFoldFrac caps how much of the identity budget a state-grouped row's
// session-name prefix may take: at most identityCap/sessionFoldFrac, so the
// row's own identity (branch/issue/name) keeps the majority of the column.
const sessionFoldFrac = 3

// foldSessionPrefix renders the owning session as a dim prefix for
// state-grouped rows (#229) — they have no session header to carry it. It
// reserves at most identityCap/sessionFoldFrac cells for the session name,
// carved OUT of identityCap (never added to it), and returns the cap left
// over for the row's own identity.
func foldSessionPrefix(session, cDim, reset string, identityCap int) (prefix, plain string, remainingCap int) {
	const sep = " / "
	sessCap := max(identityCap/sessionFoldFrac, 1)
	name := truncateCells(session, sessCap)
	plain = name + sep
	prefix = cDim + name + reset + sep
	remainingCap = identityCap - iconCellWidth(plain)
	if remainingCap < 1 {
		remainingCap = 1
	}
	return prefix, plain, remainingCap
}

// stateGroupHeader renders a state-grouped mode header for an agent priority
// state key ("" = the trailing no-agent group), using the same icon/color a
// window row in that state gets (claudeStateIcon, claudeColors — one state
// dot shared across agents, not a per-agent glyph) so the header reads as the
// same state at a glance. cNoAgent colors the trailing group, which has no
// entry in claudeColors.
func stateGroupHeader(key, theme, cNoAgent string) (display, plain string) {
	label := agentStateLabel[key]
	icon := claudeStateIcon(key)
	color := cNoAgent
	if hex, ok := claudeColors[theme][key]; ok {
		color = ansiFg(hex)
	}
	reset := "\033[0m"
	if icon == "" {
		return color + label + reset, label
	}
	return color + icon + " " + label + reset, icon + " " + label
}

// --- Utilities ---

// isActiveState reports whether an agent priority state is worth highlighting.
func isActiveState(state string) bool {
	return state != "" && state != "idle"
}

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip 'm'
		} else {
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// applyPreviewXOffset shifts preview content horizontally by previewXOffset visible cells,
// then truncates each line to the preview width to prevent wrapping changes.
func (m *tuiModel) applyPreviewXOffset() {
	if m.previewRaw == "" {
		return
	}
	if m.previewXOffset == 0 {
		m.preview.SetContent(m.previewRaw)
		return
	}
	pw := m.previewWidth()
	lines := strings.Split(m.previewRaw, "\n")
	shifted := make([]string, len(lines))
	for i, line := range lines {
		shifted[i] = truncateVisibleWidth(shiftLineLeft(line, m.previewXOffset), pw)
	}
	m.preview.SetContent(strings.Join(shifted, "\n"))
}

// shiftLineLeft drops the first n visible cells from a line, preserving ANSI escapes.
func shiftLineLeft(line string, n int) string {
	runes := []rune(line)
	var out strings.Builder
	skipped := 0
	i := 0
	// First pass: skip n visible cells, keeping ANSI escapes (preserves color state)
	for i < len(runes) && skipped < n {
		if runes[i] == '\033' && i+1 < len(runes) && runes[i+1] == '[' {
			// ANSI escape — emit it but don't count as visible
			j := i + 2
			for j < len(runes) && runes[j] != 'm' {
				j++
			}
			if j < len(runes) {
				j++ // skip 'm'
			}
			for _, r := range runes[i:j] {
				out.WriteRune(r)
			}
			i = j
		} else {
			skipped += runeCellWidth(runes[i])
			i++
		}
	}
	// Remainder
	for _, r := range runes[i:] {
		out.WriteRune(r)
	}
	return out.String()
}

// truncateVisibleWidth truncates a line to maxCells visible cells, preserving ANSI escapes.
func truncateVisibleWidth(line string, maxCells int) string {
	runes := []rune(line)
	var out strings.Builder
	cells := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\033' && i+1 < len(runes) && runes[i+1] == '[' {
			// ANSI escape — always emit, zero visible width
			j := i + 2
			for j < len(runes) && runes[j] != 'm' {
				j++
			}
			if j < len(runes) {
				j++
			}
			for _, r := range runes[i:j] {
				out.WriteRune(r)
			}
			i = j - 1
			continue
		}
		w := runeCellWidth(runes[i])
		if cells+w > maxCells {
			break
		}
		out.WriteRune(runes[i])
		cells += w
	}
	out.WriteString("\033[0m")
	return out.String()
}

// fitVisibleWidth truncates or pads a line to exactly targetCells visible cells,
// using runeCellWidth which handles nerd font PUA correctly (go-runewidth reports 0).
func fitVisibleWidth(line string, targetCells int) string {
	truncated := truncateVisibleWidth(line, targetCells)
	cells := visibleWidth(truncated)
	if cells < targetCells {
		return truncated + strings.Repeat(" ", targetCells-cells)
	}
	return truncated
}

// visibleWidth returns the display width of a string, skipping ANSI escapes.
func visibleWidth(s string) int {
	runes := []rune(s)
	cells := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\033' && i+1 < len(runes) && runes[i+1] == '[' {
			j := i + 2
			for j < len(runes) && runes[j] != 'm' {
				j++
			}
			if j < len(runes) {
				j++
			}
			i = j - 1
			continue
		}
		cells += runeCellWidth(runes[i])
	}
	return cells
}

// ---------------------------------------------------------------------------
// Fuzzy scoring (fzf-style)
//
// Two-pass alignment: forward scan finds the end of the first valid match,
// backward scan from that end finds a tighter start. Scoring uses fzf's
// constants: boundary/consecutive bonuses, gap penalties, first-char multiplier.
// ---------------------------------------------------------------------------

// Scoring constants (matching fzf).
const (
	fzfScoreMatch        = 16
	fzfScoreGapStart     = -3
	fzfScoreGapExtension = -1

	fzfBonusConsecutive         = 4  // -(scoreGapStart + scoreGapExtension)
	fzfBonusBoundary            = 8  // scoreMatch / 2
	fzfBonusBoundaryWhite       = 10 // boundary + 2
	fzfBonusBoundaryDelimiter   = 9  // boundary + 1
	fzfBonusNonWord             = 8
	fzfBonusCamelCase           = 7 // boundary + scoreGapExtension
	fzfBonusFirstCharMultiplier = 2
)

type charClass int8

const (
	charWhite charClass = iota
	charDelimiter
	charNonWord
	charLower
	charUpper
	charNumber
)

func classOf(c byte) charClass {
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		return charWhite
	case c == '/' || c == ',' || c == ';' || c == ':':
		return charDelimiter
	case c >= 'a' && c <= 'z':
		return charLower
	case c >= 'A' && c <= 'Z':
		return charUpper
	case c >= '0' && c <= '9':
		return charNumber
	default:
		return charNonWord // covers '-', '_', '.', etc.
	}
}

func charBonus(prev, curr charClass) int {
	if curr >= charLower { // letter or digit
		switch prev {
		case charWhite:
			return fzfBonusBoundaryWhite
		case charDelimiter:
			return fzfBonusBoundaryDelimiter
		case charNonWord:
			return fzfBonusBoundary
		}
	}
	if prev == charLower && curr == charUpper {
		return fzfBonusCamelCase
	}
	if prev != charNumber && curr == charNumber {
		return fzfBonusCamelCase
	}
	if curr <= charNonWord {
		return fzfBonusNonWord
	}
	return 0
}

// fuzzyScore returns a relevance score (>=0) if all characters in pattern
// appear in text in order, or -1 if there is no match.
func fuzzyScore(text, pattern string) int {
	if len(pattern) == 0 {
		return 0
	}

	// Forward pass: verify match exists, find end position.
	pi := 0
	endIdx := 0
	for i := 0; i < len(text) && pi < len(pattern); i++ {
		if text[i] == pattern[pi] {
			endIdx = i
			pi++
		}
	}
	if pi < len(pattern) {
		return -1
	}

	// Backward pass: from endIdx, find a tighter alignment.
	pos := make([]int, len(pattern))
	pi = len(pattern) - 1
	for i := endIdx; i >= 0 && pi >= 0; i-- {
		if text[i] == pattern[pi] {
			pos[pi] = i
			pi--
		}
	}

	// Score the alignment.
	score := 0
	prevBonus := 0
	consecutive := 0

	for k, idx := range pos {
		var prev charClass
		if idx == 0 {
			prev = charWhite // start of string acts as whitespace boundary
		} else {
			prev = classOf(text[idx-1])
		}
		b := charBonus(prev, classOf(text[idx]))

		score += fzfScoreMatch

		// Consecutive bonus propagation: carry forward the better of the
		// ongoing-run bonus and the fixed consecutive bonus.
		if k > 0 && pos[k]-pos[k-1] == 1 {
			consecutive++
			cb := max(fzfBonusConsecutive, prevBonus)
			if b >= fzfBonusBoundary {
				cb = max(cb, b)
			}
			b = cb
		} else if k > 0 {
			gap := pos[k] - pos[k-1] - 1
			score += fzfScoreGapStart + fzfScoreGapExtension*(gap-1)
			consecutive = 0
		}

		if k == 0 {
			score += b * fzfBonusFirstCharMultiplier
		} else {
			score += b
		}
		prevBonus = b
	}

	return score
}
