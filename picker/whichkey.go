package main

// which-key popup (#629): a standalone bubbletea model listing every tmux
// bind with its description, grouped by key table and fuzzy-filterable.
//
// Deliberately not folded into tuiModel (picker/tui.go): it shares no
// session/window/wall state with the session/window pickers, and a second
// small model keeps the blast radius of this fast-follow discovery UI small
// (CLAUDE.md "pull complexity downward"). It does reuse tui.go's
// package-level helpers — fuzzyScore, visibleWidth, fitVisibleWidth,
// truncateVisibleWidth, ansiBg, printableKey, readTmuxOpts, envOrMap — none
// of which are tuiModel methods, so no shared-state coupling is introduced.
//
// Enter resolves the picked bind and replays it against the invoking pane —
// see replayBind for the mechanism and why it is the one chosen.

import (
	"fmt"
	imgcolor "image/color"
	"os"
	"os/exec"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// whichKeyRow is one bind, parsed from `tmux list-keys -a`.
type whichKeyRow struct {
	table     string // key_table: "prefix", "root", "copy-mode-vi", ...
	keyString string // key_string alone — the token `list-keys -T <table> <key>` takes
	key       string // display form: the prefix chord for a prefix bind, "M-J" for a root one
	note      string // key_note, falling back to key_command when the bind has no -N
	described bool   // the note is a real -N description, not a key_command standing in
}

// whichKeyGroup is one key_table's rows, already sorted for display.
type whichKeyGroup struct {
	table string
	rows  []whichKeyRow
}

// whichKeyItem flattens groups into one list for cursor/scroll math: either a
// table header or a bind row.
type whichKeyItem struct {
	isHeader bool
	table    string // header row
	row      whichKeyRow
}

// whichKeyModel is the which-key popup's bubbletea model.
type whichKeyModel struct {
	groups      []whichKeyGroup // unfiltered, grouped and sorted — built once
	visible     []whichKeyItem  // groups + rows surviving the current query
	cursor      int
	keyColWidth int // fixed key column width, from the unfiltered rows — doesn't jitter while filtering
	tblColWidth int // ditto for the table column, which only the filtered (flat) list renders

	query string

	width, height int
	ready         bool

	theme    string
	tmuxOpts map[string]string

	// originPane is the tmux pane id (#{pane_id}) that invoked the popup, via
	// OG_PICKER_ORIGIN_PANE — set by scripts/tmux-which-key.sh (plan step 5).
	// replayBind targets it, so the bind runs where the keypress would have.
	originPane string

	// selected/hasPicked record the row chosen on enter. The model itself
	// never executes: RunWhichKey replays the pick once the popup has closed,
	// which is the whole point of deferring it (see whichKeyReplayDelay).
	selected  whichKeyRow
	hasPicked bool

	// rawView toggles (ctrl+r) a scrollable pane showing the pre-#629 `?`
	// bind's own output verbatim — grouped/sorted by key rather than table,
	// raw command shown when a bind has no note. rawLines is fetched lazily
	// on first toggle-on and cached; rawErr holds a fetch failure's message
	// so the pane can say so instead of rendering blank.
	rawView   bool
	rawLines  []string
	rawErr    string
	rawScroll int
}

// --- Entry point ---

// RunWhichKey launches the which-key popup.
func RunWhichKey() error {
	rows, err := listKeysRows()
	if err != nil {
		return err
	}
	opts := readTmuxOpts()
	theme := themeFromOpts(opts)
	originPane := os.Getenv("OG_PICKER_ORIGIN_PANE")

	m := newWhichKeyModel(rows, opts, theme, originPane)
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return err
	}
	done, ok := final.(whichKeyModel)
	if !ok {
		return nil
	}
	row, picked := done.Selected()
	if !picked {
		return nil
	}
	return replayBind(row, done.originPane)
}

// whichKeyListFormat is the -F format listKeysRows reads.
//
// Pipe-delimited per this repo's tmux -F convention (CLAUDE.md "Key
// Conventions" — tab/newline-delimited formats silently collapse fields), which
// puts two fields at risk of carrying a literal "|" of their own: the note (or,
// for an undescribed bind, the command standing in for it) and key_string,
// since `prefix |` is itself a bind. The note is therefore stripped of pipes on
// the tmux side — the `#{s/[|]/ /:…}` precedent CLAUDE.md records for the
// bridge's free-form fields, bracket expression included — and key_string is
// kept last so its own "|" lands inside the trailing field rather than shifting
// the row.
//
// key_command is deliberately NOT carried: replayBind re-reads it per-bind from
// `list-keys -T <table> <key>`, which is authoritative and immune to the same
// shift (an undescribed bind whose command holds a pipe — `prefix Y`'s
// `… | wl-copy` — would otherwise arrive truncated and be replayed truncated).
const whichKeyListFormat = "#{key_table}|#{key_prefix}|#{?key_note,1,}|#{s/[|]/ /:#{?key_note,#{key_note},#{key_command}}}|#{key_string}"

// listKeysRows shells out to tmux for every bind's table, key and note.
//
// -N is deliberately omitted: tmux's list-keys -N filters to keys that carry
// a note, but measured live it narrows -a's table coverage to just prefix and
// root, dropping copy-mode-vi (and any other table) entirely even once every
// bind in it has a -N description. whichKeyListFormat already derives the
// note (falling back to key_command) itself, so -N buys nothing here.
func listKeysRows() ([]whichKeyRow, error) {
	out, err := exec.Command("tmux", "list-keys", "-a", "-F", whichKeyListFormat).Output()
	if err != nil {
		return nil, err
	}
	return parseListKeysRows(string(out)), nil
}

// parseListKeysRows turns whichKeyListFormat output into rows. Split from the
// exec so the parsing is testable without a live server.
func parseListKeysRows(out string) []whichKeyRow {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	var rows []whichKeyRow
	for _, line := range strings.Split(trimmed, "\n") {
		// SplitN(5): key_string is the trailing field and may itself be "|".
		parts := strings.SplitN(line, "|", 5)
		if len(parts) != 5 || parts[4] == "" {
			continue
		}
		// #{key_prefix} renders the prefix key for every table, not just the
		// prefix one (measured: a root-table F12 reports "`"), so a root bind
		// would otherwise display — and be named in an error — as "`F12".
		display := parts[4]
		if parts[0] == "prefix" {
			display = parts[1] + parts[4]
		}
		rows = append(rows, whichKeyRow{
			table:     parts[0],
			keyString: parts[4],
			key:       display,
			note:      parts[3],
			described: parts[2] == "1",
		})
	}
	return rows
}

// --- Raw view (ctrl+r toggle) ---

// whichKeyRawFormat is the pre-#629 `?` bind's own -F format, kept verbatim
// (config/tmux.conf.tmpl history) so the in-popup toggle reproduces exactly
// what pressing prefix+? used to print — grouped/sorted by key via `-O key`,
// not by table, raw key_command shown when a bind carries no note.
const whichKeyRawFormat = "#{key_table}: #{key_prefix}#{key_string} #{?key_note,#{key_note},#{key_command}}"

// listKeysRaw shells out for the raw, sorted-by-key listing. Split from the
// toggle handler so it stays testable/mockable in shape, matching
// listKeysRows/parseListKeysRows above.
//
// -N is omitted for the same reason listKeysRows omits it: it narrows table
// coverage to prefix/root only, which would make the toggle's "full raw
// listing" silently drop copy-mode-vi and every other table. whichKeyRawFormat
// already falls back to key_command when a bind has no note.
func listKeysRaw() ([]string, error) {
	out, err := exec.Command("tmux", "list-keys", "-O", "key", "-F", whichKeyRawFormat).Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// --- Replay ---

// whichKeyReplayDelay defers the replay past the popup's own teardown.
//
// Measured on a scratch server with a real client: a `run-shell -b -C` issued
// from inside the popup with no delay fired 0/5 times for a bind whose command
// is a display-popup — tmux silently no-ops a popup raised while another is
// open, which is most of this repo's binds. At 0.05s it was 5/5, and tmux
// honours a fractional delay (a -d 0.5 fired at 0.532s). 0.1s is that floor
// with margin, and stays under the threshold of notice.
const whichKeyReplayDelay = "0.1"

// replayBind runs the picked bind's own command against the pane that invoked
// the popup.
//
// Mechanism, chosen over `send-keys -K` after prototyping both against a real
// pty client on an isolated scratch server (the plan's research note had only
// tested -K with no client at all, where it does nothing):
//
//   - `send-keys -K` DOES fire a bind once a real client is attached — but it
//     delivers the key to a *client*, so the bind runs against that client's
//     current pane and nothing else. `send-keys -K -t %1 -c <client> <key>`
//     with the client sitting on %0 fired against %0, measured. A popup knows
//     the pane it was invoked from, not that the client is still on it, and it
//     would additionally have to reconstruct the prefix chord and mutate the
//     client's key table for anything outside the root table.
//   - `run-shell -C '<command>'` takes the command as one string, so tmux's own
//     lexer re-parses it — braces, nested if-shell, quoting and all, which no
//     hand-rolled argv split would survive — and `-t` DOES propagate into it
//     (`run-shell -t %1 -C 'display-message -p "#{pane_id}"'` printed %1, where
//     the same command under `if-shell -t %1` printed %0). That makes the
//     origin pane an explicit argument rather than a hope about client focus.
//
// Bridge gating needs no special case either way: the `if-shell -F
// '#{@bridge_win}' {…} {…}` routing is part of the command text, and the
// targeted replay picks the branch the origin pane's own options select —
// driven through the real popup against a brace-form bind, which took the
// bridge branch for an origin pane whose window carried @bridge_win and the
// local branch for one that did not.
func replayBind(row whichKeyRow, originPane string) error {
	if originPane == "" {
		// Normal launches go through scripts/tmux-which-key.sh, which exports
		// this; a bare `tmux-picker-generate --which-key` does not.
		return fmt.Errorf("which-key: OG_PICKER_ORIGIN_PANE unset, nothing to run %q against", row.key)
	}

	line, err := resolveBindLine(row.table, row.keyString)
	if err != nil {
		return whichKeyFail(originPane, fmt.Sprintf("which-key: cannot resolve %s: %v", row.key, err))
	}
	command, err := stripBindKeyPrefix(line)
	if err != nil {
		return whichKeyFail(originPane, fmt.Sprintf("which-key: cannot parse bind for %s: %v", row.key, err))
	}

	out, err := exec.Command("tmux", "run-shell", "-b",
		"-d", whichKeyReplayDelay, "-t", originPane, "-C", command).CombinedOutput()
	if err != nil {
		return whichKeyFail(originPane, fmt.Sprintf("which-key: %s failed: %s", row.key, strings.TrimSpace(string(out))))
	}
	return nil
}

// resolveBindLine reads one bind back as the `bind-key …` line tmux would
// echo for it.
func resolveBindLine(table, key string) (string, error) {
	// tmux's argv parser reads a lone ";" as a command separator, so that one
	// key has to arrive escaped — unescaped it silently lists every bind
	// instead of the one asked for (measured).
	if key == ";" {
		key = `\;`
	}
	out, err := exec.Command("tmux", "list-keys", "-T", table, key).Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("no such bind: %s %s", table, key)
	}
	// One bind, one line — but fail closed rather than replay a stray extra.
	if strings.Contains(line, "\n") {
		return "", fmt.Errorf("ambiguous bind: %s %s", table, key)
	}
	return line, nil
}

// whichKeyFail flashes a failure on the invoking pane and returns it. A popup's
// stderr dies with the popup, so display-message is the only channel the user
// actually sees; main.go still prints the returned error for a CLI run.
func whichKeyFail(originPane, msg string) error {
	_ = exec.Command("tmux", "display-message", "-d", "5000", "-t", originPane, msg).Run()
	return fmt.Errorf("%s", msg)
}

// bindKeyValueFlags / bindKeyBareFlags are the flags `list-keys` echoes ahead
// of the key token. It normalizes -n into `-T root` and prints -r and -T
// always; -N is listed for the tmux versions that echo the note too.
var (
	bindKeyValueFlags = map[string]bool{"-N": true, "-T": true}
	bindKeyBareFlags  = map[string]bool{"-r": true, "-n": true}
)

// stripBindKeyPrefix isolates the bare command from a `bind-key …` echo line,
// dropping the verb, every flag ahead of the key (with its value, for -N/-T)
// and the key token itself. The command is returned as the untouched remainder
// of the line, never re-joined from tokens — tmux's quoting is what `run-shell
// -C` parses back, and rebuilding it would lose exactly the nesting that makes
// a bridge-gated bind work.
func stripBindKeyPrefix(line string) (string, error) {
	tok, pos, ok := nextBindToken(line, 0)
	if !ok || (tok != "bind-key" && tok != "bind") {
		return "", fmt.Errorf("not a bind-key line: %q", line)
	}
	for {
		tok, next, ok := nextBindToken(line, pos)
		if !ok {
			return "", fmt.Errorf("no command in %q", line)
		}
		switch {
		case bindKeyValueFlags[tok]:
			if _, after, ok := nextBindToken(line, next); ok {
				pos = after
				continue
			}
			return "", fmt.Errorf("flag %s has no value in %q", tok, line)
		case bindKeyBareFlags[tok]:
			pos = next
		default:
			// tok is the key; everything past it is the command.
			command := strings.TrimSpace(line[next:])
			if command == "" {
				return "", fmt.Errorf("no command in %q", line)
			}
			return command, nil
		}
	}
}

// nextBindToken reads the next whitespace-separated token starting at or after
// from, honouring the backslash escapes and quoting `list-keys` emits (a key
// prints as \" or \;, a note as '…' with spaces inside). It returns the token
// unescaped, the offset just past it, and whether one was found.
func nextBindToken(line string, from int) (string, int, bool) {
	i := from
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return "", i, false
	}
	var b strings.Builder
	var quote byte
	for ; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			return b.String(), i, true
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), i, true
}

// whichKeyTableRank orders "prefix" and "root" ahead of every other key
// table (copy-mode-vi, etc.), which then sort alphabetically — those two
// carry this repo's own binds and are what most people are looking for.
func whichKeyTableRank(table string) int {
	switch table {
	case "prefix":
		return 0
	case "root":
		return 1
	default:
		return 2
	}
}

// groupWhichKeyRows buckets rows by table, orders the tables per
// whichKeyTableRank (alphabetical within a rank), and sorts each table's rows
// by key — the same fallback ordering tmux's own `-O key` offers.
func groupWhichKeyRows(rows []whichKeyRow) []whichKeyGroup {
	var order []string
	byTable := map[string][]whichKeyRow{}
	for _, r := range rows {
		if _, ok := byTable[r.table]; !ok {
			order = append(order, r.table)
		}
		byTable[r.table] = append(byTable[r.table], r)
	}

	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := whichKeyTableRank(order[i]), whichKeyTableRank(order[j])
		if ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})

	groups := make([]whichKeyGroup, 0, len(order))
	for _, table := range order {
		tableRows := byTable[table]
		sort.SliceStable(tableRows, func(i, j int) bool { return tableRows[i].key < tableRows[j].key })
		groups = append(groups, whichKeyGroup{table: table, rows: tableRows})
	}
	return groups
}

// whichKeyKeyColMax bounds the key column so one pathological long chord
// (e.g. a multi-key sequence) can't shove every note off-screen.
const whichKeyKeyColMax = 24

// whichKeyTableColWidth sizes the table column the filtered list renders.
// Unbounded: a key table name is short and is the whole reason a flat list
// stays readable once the "── prefix ──" headers are gone.
func whichKeyTableColWidth(groups []whichKeyGroup) int {
	w := 0
	for _, g := range groups {
		if gw := visibleWidth(g.table); gw > w {
			w = gw
		}
	}
	return w
}

func whichKeyColWidth(groups []whichKeyGroup) int {
	w := 0
	for _, g := range groups {
		for _, r := range g.rows {
			if rw := visibleWidth(r.key); rw > w {
				w = rw
			}
		}
	}
	if w > whichKeyKeyColMax {
		w = whichKeyKeyColMax
	}
	return w
}

func newWhichKeyModel(rows []whichKeyRow, opts map[string]string, theme, originPane string) whichKeyModel {
	groups := groupWhichKeyRows(rows)
	m := whichKeyModel{
		groups:      groups,
		keyColWidth: whichKeyColWidth(groups),
		tblColWidth: whichKeyTableColWidth(groups),
		theme:       theme,
		tmuxOpts:    opts,
		originPane:  originPane,
	}
	m = m.rebuildVisible()
	m.cursor = m.firstSelectable(0)
	return m
}

// Selected returns the row picked on enter, and whether anything was picked
// (false after quit via q/esc/ctrl+c).
func (m whichKeyModel) Selected() (whichKeyRow, bool) {
	return m.selected, m.hasPicked
}

// --- Filtering ---

// whichKeySearchText is what a query is matched against: the displayed key
// chord plus the displayed note. Both are what the row shows, so a hit is
// always attributable — except in the note of an undescribed bind, whose
// key_command is far wider than the column and is why describedRank exists.
func whichKeySearchText(r whichKeyRow) string {
	return strings.ToLower(r.key + " " + r.note)
}

// describedRank sorts a bind carrying a real -N description ahead of one whose
// note is a raw key_command, whatever either scored.
//
// 298 of a stock 434-bind server have no -N, so their "note" is a command —
// often a /nix/store path or a nested format string, none of which fits the
// column. Scoring alone can't separate them: a subsequence buried in a store
// path scores like any other, so "float" returned 20 rows of which 2 were the
// floating-pane binds (#689). Ranking rather than dropping keeps a plugin bind
// reachable by the text of its command, just below every real description.
func describedRank(r whichKeyRow) int {
	if r.described {
		return 0
	}
	return 1
}

// rebuildVisible re-derives visible from groups + query.
//
// The two shapes are deliberate. With no query the list is the grouped
// reference — every bind under its key table, in table order. With a query it
// is a flat, score-ranked list: grouping and ranking cannot both hold, and a
// search wants its best hit on line 1, not wherever its table happens to fall.
// The table each row belongs to is not lost, it moves into a column
// (renderRow's showTable).
func (m whichKeyModel) rebuildVisible() whichKeyModel {
	q := strings.ToLower(strings.TrimSpace(m.query))

	if q == "" {
		var visible []whichKeyItem
		for _, g := range m.groups {
			if len(g.rows) == 0 {
				continue
			}
			visible = append(visible, whichKeyItem{isHeader: true, table: g.table})
			for _, r := range g.rows {
				visible = append(visible, whichKeyItem{row: r})
			}
		}
		m.visible = visible
		return m
	}

	type scoredRow struct {
		row   whichKeyRow
		score int
	}
	var matches []scoredRow
	for _, g := range m.groups {
		for _, r := range g.rows {
			if score := fuzzyScore(whichKeySearchText(r), q); score >= 0 {
				matches = append(matches, scoredRow{row: r, score: score})
			}
		}
	}

	// Stable keeps groupWhichKeyRows' table-then-key order for ties.
	sort.SliceStable(matches, func(i, j int) bool {
		ri, rj := describedRank(matches[i].row), describedRank(matches[j].row)
		if ri != rj {
			return ri < rj
		}
		return matches[i].score > matches[j].score
	})

	visible := make([]whichKeyItem, 0, len(matches))
	for _, match := range matches {
		visible = append(visible, whichKeyItem{row: match.row})
	}
	m.visible = visible
	return m
}

// --- Bubbletea interface ---

func (m whichKeyModel) Init() tea.Cmd {
	return nil
}

func (m whichKeyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m whichKeyModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		return m, tea.Quit
	}

	if key == "ctrl+r" {
		if !m.rawView && m.rawLines == nil && m.rawErr == "" {
			lines, err := listKeysRaw()
			if err != nil {
				m.rawErr = err.Error()
			} else {
				m.rawLines = lines
			}
		}
		m.rawView = !m.rawView
		m.rawScroll = 0
		return m, nil
	}

	if m.rawView {
		switch key {
		case "q", "esc":
			m.rawView = false
			return m, nil
		case "ctrl+j", "down":
			return m.scrollRaw(1), nil
		case "ctrl+k", "up":
			return m.scrollRaw(-1), nil
		}
		return m, nil
	}

	switch key {
	case "q", "esc":
		if m.query != "" {
			m.query = ""
			m = m.rebuildVisible()
			m.cursor = m.firstSelectable(0)
			return m, nil
		}
		return m, tea.Quit

	case "ctrl+j", "down":
		m = m.moveCursor(1)
		return m, nil

	case "ctrl+k", "up":
		m = m.moveCursor(-1)
		return m, nil

	case "enter":
		if row, ok := m.currentRow(); ok {
			m.selected = row
			m.hasPicked = true
		}
		return m, tea.Quit

	case "backspace":
		if len(m.query) > 0 {
			runes := []rune(m.query)
			m.query = string(runes[:len(runes)-1])
			m = m.rebuildVisible()
			m.cursor = m.firstSelectable(0)
		}
		return m, nil

	default:
		if text, ok := printableKeyText(key); ok {
			m.query += text
			m = m.rebuildVisible()
			m.cursor = m.firstSelectable(0)
		}
		return m, nil
	}
}

// --- Navigation ---

func (m whichKeyModel) isSelectable(item whichKeyItem) bool {
	return !item.isHeader
}

func (m whichKeyModel) firstSelectable(from int) int {
	for i := from; i < len(m.visible); i++ {
		if m.isSelectable(m.visible[i]) {
			return i
		}
	}
	return from
}

func (m whichKeyModel) moveCursor(delta int) whichKeyModel {
	n := len(m.visible)
	if n == 0 {
		return m
	}
	c := m.cursor
	for {
		c += delta
		if c < 0 || c >= n {
			// Ran past the edge — keep the current cursor rather than landing
			// on a non-selectable header row.
			return m
		}
		if m.isSelectable(m.visible[c]) {
			break
		}
	}
	m.cursor = c
	return m
}

func (m whichKeyModel) currentRow() (whichKeyRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return whichKeyRow{}, false
	}
	item := m.visible[m.cursor]
	if item.isHeader {
		return whichKeyRow{}, false
	}
	return item.row, true
}

// scrollRaw moves the raw pane's scroll offset by delta, clamped so the last
// page never scrolls past its own end.
func (m whichKeyModel) scrollRaw(delta int) whichKeyModel {
	h := m.bodyHeight()
	maxScroll := len(m.rawLines) - h
	if maxScroll < 0 {
		maxScroll = 0
	}
	s := m.rawScroll + delta
	if s < 0 {
		s = 0
	}
	if s > maxScroll {
		s = maxScroll
	}
	m.rawScroll = s
	return m
}

func (m whichKeyModel) scrollStart(h int) int {
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

// --- Rendering ---

// color reads a Catppuccin color from tmux options, falling back by theme —
// the whichKeyModel-scoped twin of tuiModel.thmColor (picker/tui.go), kept
// separate so this file touches no tuiModel state.
func (m whichKeyModel) color(tmuxOpt, darkFallback, lightFallback string) imgcolor.Color {
	return lipgloss.Color(m.colorHex(tmuxOpt, darkFallback, lightFallback))
}

func (m whichKeyModel) colorHex(tmuxOpt, darkFallback, lightFallback string) string {
	if v, ok := m.tmuxOpts[tmuxOpt]; ok && v != "" {
		return v
	}
	if m.theme == "light" {
		return lightFallback
	}
	return darkFallback
}

func (m whichKeyModel) View() tea.View {
	var content string
	switch {
	case !m.ready:
		content = "Loading..."
	case m.rawView:
		body := m.renderRawBody()
		bordered := lipgloss.NewStyle().
			Width(m.width).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(m.color("@thm_surface_1", "#45475a", "#9ca0b0")).
			Render(body)
		content = lipgloss.JoinVertical(lipgloss.Left, m.renderSearch(), bordered, m.renderHints())
	default:
		body := m.renderBody()
		bordered := lipgloss.NewStyle().
			Width(m.width).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(m.color("@thm_surface_1", "#45475a", "#9ca0b0")).
			Render(body)
		content = lipgloss.JoinVertical(lipgloss.Left, m.renderSearch(), bordered, m.renderHints())
	}

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// bodyHeight excludes the search bar and the one-line hint bar; -2 covers the
// hint line plus the body's own bottom border rule (mirrors tuiModel.bodyHeight).
func (m whichKeyModel) bodyHeight() int {
	h := m.height - lipgloss.Height(m.renderSearch()) - 2
	if h < 5 {
		return 5
	}
	return h
}

func (m whichKeyModel) renderBody() string {
	h := m.bodyHeight()
	w := m.width
	start := m.scrollStart(h)

	lines := make([]string, 0, h)
	for i := start; i < start+h && i < len(m.visible); i++ {
		item := m.visible[i]
		if item.isHeader {
			lines = append(lines, fitVisibleWidth(m.renderTableHeader(item.table, w), w))
			continue
		}
		lines = append(lines, m.renderRow(item.row, w, i == m.cursor, m.query != ""))
	}
	empty := strings.Repeat(" ", w)
	for len(lines) < h {
		lines = append(lines, empty)
	}
	return strings.Join(lines, "\n")
}

// renderRawBody draws the ctrl+r raw-listing pane: plain text, scrolled by
// rawScroll, truncated (never wrapped) to the popup width like every other
// row in this model.
func (m whichKeyModel) renderRawBody() string {
	h := m.bodyHeight()
	w := m.width

	if m.rawErr != "" {
		dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
		lines := []string{fitVisibleWidth(dim.Render("tmux list-keys failed: "+m.rawErr), w)}
		for len(lines) < h {
			lines = append(lines, strings.Repeat(" ", w))
		}
		return strings.Join(lines, "\n")
	}

	lines := make([]string, 0, h)
	for i := m.rawScroll; i < m.rawScroll+h && i < len(m.rawLines); i++ {
		lines = append(lines, fitVisibleWidth(m.rawLines[i], w))
	}
	empty := strings.Repeat(" ", w)
	for len(lines) < h {
		lines = append(lines, empty)
	}
	return strings.Join(lines, "\n")
}

// whichKeyTableGloss says in plain words when a table's binds are live. A key
// table name is tmux's own jargon — "prefix", "root", "move" describe nothing
// to a reader (#689) — and the prefix one has to name the actual prefix key,
// which is a setting, so it is passed in rather than baked in.
//
// Unknown tables (a plugin's own) get no gloss rather than a guessed one.
func whichKeyTableGloss(table, prefixKey string) string {
	switch table {
	case "prefix":
		if prefixKey == "" {
			return "after the prefix key"
		}
		return "after " + prefixKey
	case "root":
		return "no prefix"
	case "copy-mode", "copy-mode-vi":
		return "in copy mode"
	case "move":
		return "in pane move mode"
	case "fingers":
		return "while tmux-fingers is open"
	default:
		return ""
	}
}

// renderTableHeader draws a table's section divider at the real popup width —
// mirrors renderHeaderItem's shape (picker/render_list.go).
func (m whichKeyModel) renderTableHeader(table string, w int) string {
	accent := lipgloss.NewStyle().Foreground(m.color("@thm_lavender", "#b4befe", "#7287fd"))
	rule := lipgloss.NewStyle().Foreground(m.color("@thm_surface_1", "#45475a", "#9ca0b0"))

	head := rule.Render("── ") + accent.Render(table) + " "
	if gloss := whichKeyTableGloss(table, m.tmuxOpts["prefix"]); gloss != "" {
		head += rule.Render("— "+gloss) + " "
	}
	fill := w - visibleWidth(head)
	if fill < 1 {
		return head
	}
	return head + rule.Render(strings.Repeat("─", fill))
}

// renderRow draws one bind row: a fixed-width key column, then the note, then —
// only in the filtered flat list, where the section headers are gone —
// the table the bind belongs to.
//
// Selected rows get a background — built as one plain string styled once
// (never a pre-styled span re-rendered inside another background), and the
// key's own foreground reset is patched to re-assert the background after it
// (same trick as renderList's selResetKeepBg).
func (m whichKeyModel) renderRow(r whichKeyRow, w int, selected, showTable bool) string {
	keyStyle := lipgloss.NewStyle().Foreground(m.color("@thm_lavender", "#b4befe", "#7287fd"))
	dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
	table := lipgloss.NewStyle().Foreground(m.color("@thm_overlay_0", "#6c7086", "#9ca0b0"))

	tableW := 0
	if showTable {
		tableW = m.tblColWidth + 2
	}
	key := fitVisibleWidth(r.key, m.keyColWidth)
	noteW := w - m.keyColWidth - 4 - tableW
	if noteW < 0 {
		noteW = 0
	}
	line := keyStyle.Render(key) + "  "
	if showTable {
		line += dim.Render(fitVisibleWidth(r.note, noteW)) + "  " + table.Render(r.table)
	} else {
		line += dim.Render(truncateVisibleWidth(r.note, noteW))
	}

	if !selected {
		return fitVisibleWidth("  "+line, w)
	}

	selBgHex := m.colorHex("@thm_surface_2", "#45475a", "#acb0be")
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color(selBgHex))
	patched := strings.ReplaceAll(line, "\033[0m", "\033[39m"+ansiBg(selBgHex))
	return selStyle.Render(fitVisibleWidth("▶ "+patched, w))
}

func (m whichKeyModel) renderSearch() string {
	blue := lipgloss.NewStyle().Foreground(m.color("@thm_blue", "#89b4fa", "#1e66f5"))
	dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))

	icon := blue.Render("  ")
	var queryStr string
	if m.query != "" {
		queryStr = m.query + "█"
	} else {
		queryStr = dim.Render("filter by key or description...") + " "
	}

	return lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(m.color("@thm_surface_1", "#45475a", "#9ca0b0")).
		Render(icon + queryStr)
}

func (m whichKeyModel) renderHints() string {
	dim := lipgloss.NewStyle().Foreground(m.color("@thm_surface_2", "#585b70", "#9ca0b0"))
	key := lipgloss.NewStyle().Foreground(m.color("@thm_lavender", "#b4befe", "#7287fd"))

	hint := func(k, desc string) string {
		return key.Render(k) + dim.Render(":"+desc)
	}

	var parts []string
	if m.rawView {
		parts = []string{
			hint("^jk/↑↓", "scroll"),
			hint("^r", "grouped view"),
			hint("q/esc", "back"),
		}
	} else {
		parts = []string{
			hint("^jk/↑↓", "nav"),
			hint("enter", "run"),
			hint("^r", "raw list"),
			hint("q", "quit"),
		}
	}
	return fitVisibleWidth("  "+strings.Join(parts, "  "), m.width)
}
