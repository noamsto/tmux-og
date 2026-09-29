package render

import (
	"regexp"
	"strings"
	"testing"
)

var menuKeys = []string{
	"prefix <", "prefix >",
	"root MouseDown3Pane", "root M-MouseDown3Pane",
	"root MouseDown3Status", "root M-MouseDown3Status",
	"root MouseDown3StatusLeft", "root M-MouseDown3StatusLeft",
	"root MouseDown3Empty", "root M-MouseDown3Empty",
}

// unquote reverses tmuxDoubleQuote.
func unquote(t *testing.T, q string) string {
	t.Helper()
	if len(q) < 2 || q[0] != '"' || q[len(q)-1] != '"' {
		t.Fatalf("not a double-quoted word: %q", q)
	}
	var b strings.Builder
	body := q[1 : len(q)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case '\\':
			i++
			if i == len(body) || !strings.ContainsRune(`\"$`, rune(body[i])) {
				t.Fatalf("bad escape in %q", q)
			}
			c = body[i]
		case '"', '$':
			t.Fatalf("unescaped %q in %q", c, q)
		}
		b.WriteByte(c)
	}
	return b.String()
}

// trailingQuoted splits a line ending in a double-quoted word into the text
// before that word and the word itself: the opening quote is the last '"' not
// escaped by an odd run of backslashes.
func trailingQuoted(t *testing.T, line string) (head, word string) {
	t.Helper()
	if !strings.HasSuffix(line, `"`) {
		t.Fatalf("line does not end in a quoted word: %q", line)
	}
	for i := len(line) - 2; i >= 0; i-- {
		if line[i] != '"' {
			continue
		}
		n := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			n++
		}
		if n%2 == 0 {
			return line[:i], line[i:]
		}
	}
	t.Fatalf("no opening quote in %q", line)
	return "", ""
}

func menuBindLines(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(menuBinds(keysPaths()), "\n")
	if len(lines) != len(menuKeys)+2 {
		t.Fatalf("menuBinds has %d lines, want %d", len(lines), len(menuKeys)+2)
	}
	return lines[1 : len(lines)-1]
}

func stockFor(t *testing.T, id string) stockMenu {
	t.Helper()
	for _, s := range stockMenus {
		if s.table+" "+s.key == id {
			return s
		}
	}
	t.Fatalf("no stock menu %q", id)
	return stockMenu{}
}

func TestEscapeLayers(t *testing.T) {
	if got := esc1("a #{b} ##{c}"); got != "a ##{b} ####{c}" {
		t.Fatalf("esc1 = %q", got)
	}
	if got := esc2("a #{b} ##{c}"); got != "a ####{b} ########{c}" {
		t.Fatalf("esc2 = %q", got)
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	if got := tmuxDoubleQuote(`a\b"c$d'e#{f}`); got != `"a\\b\"c\$d'e#{f}"` {
		t.Fatalf("tmuxDoubleQuote = %q", got)
	}
	for _, s := range stockMenus {
		if got := unquote(t, tmuxDoubleQuote(s.cmd)); got != s.cmd {
			t.Fatalf("%s %s: round trip =\n%q\nwant\n%q", s.table, s.key, got, s.cmd)
		}
	}
}

func TestStockMenusParse(t *testing.T) {
	if len(stockMenus) != len(menuKeys) {
		t.Fatalf("%d stock menus, want %d", len(stockMenus), len(menuKeys))
	}
	lines := strings.Split(strings.TrimSuffix(stockMenusText, "\n"), "\n")
	for i, s := range stockMenus {
		if id := s.table + " " + s.key; id != menuKeys[i] {
			t.Fatalf("stock menu %d is %q, want %q", i, id, menuKeys[i])
		}
		f := strings.Fields(lines[i])
		if f[0] != "bind-key" || f[1] != "-T" || f[2] != s.table || f[3] != s.key {
			t.Fatalf("line %d prefix %q", i, f[:4])
		}
		rest := lines[i][strings.Index(lines[i], " "+s.key+" ")+len(s.key)+2:]
		if want := strings.TrimLeft(rest, " "); s.cmd != want {
			t.Fatalf("%s: cmd =\n%q\nwant\n%q", menuKeys[i], s.cmd, want)
		}
		if strings.HasPrefix(s.cmd, " ") || s.cmd == "" {
			t.Fatalf("%s: cmd keeps padding or is empty: %q", menuKeys[i], s.cmd)
		}
	}
}

func TestMenuBindsGatedOnStockVersion(t *testing.T) {
	lines := strings.Split(menuBinds(keysPaths()), "\n")
	if lines[0] != `%if "#{==:#{version},next-3.9}"` {
		t.Fatalf("first line = %q", lines[0])
	}
	if lines[len(lines)-1] != "%endif" {
		t.Fatalf("last line = %q", lines[len(lines)-1])
	}
	if len(lines) != 12 {
		t.Fatalf("%d lines, want 10 binds inside %%if/%%endif", len(lines))
	}
	for _, l := range lines[1:11] {
		if !strings.HasPrefix(l, "bind-key ") {
			t.Fatalf("not a bind line: %q", l)
		}
	}
}

func TestMenuStockBranchIsVerbatim(t *testing.T) {
	for i, line := range menuBindLines(t) {
		head, word := trailingQuoted(t, line)
		if !strings.HasSuffix(head, "} ") {
			t.Fatalf("%s: stock string does not follow the mirror block: %q", menuKeys[i], head)
		}
		if got, want := unquote(t, word), stockFor(t, menuKeys[i]).cmd; got != want {
			t.Fatalf("%s: stock branch =\n%q\nwant\n%q", menuKeys[i], got, want)
		}
	}
}

func TestPrefixMenusKeepStockNotes(t *testing.T) {
	lines := menuBindLines(t)
	for i, want := range []string{
		"bind-key -N 'Display window menu' -T prefix < ",
		"bind-key -N 'Display pane menu' -T prefix > ",
	} {
		if !strings.HasPrefix(lines[i], want) {
			t.Fatalf("line %d = %q, want prefix %q", i, lines[i][:min(80, len(lines[i]))], want)
		}
	}
}

// TestEveryMenuBindHasNote pins every menu bind's -N note, including the eight
// root mouse binds: the which-key popup and the bind-note-assertions flake
// check both read -N, so a mirror bind without one is silent everywhere.
func TestEveryMenuBindHasNote(t *testing.T) {
	notes := map[string]string{
		"prefix <":                    "Display window menu",
		"prefix >":                    "Display pane menu",
		"root MouseDown3Pane":         "Display pane menu",
		"root M-MouseDown3Pane":       "Display pane menu",
		"root MouseDown3Status":       "Display window menu",
		"root M-MouseDown3Status":     "Display window menu",
		"root MouseDown3StatusLeft":   "Display session menu",
		"root M-MouseDown3StatusLeft": "Display session menu",
		"root MouseDown3Empty":        "Display new pane/window menu",
		"root M-MouseDown3Empty":      "Display new pane/window menu",
	}
	lines := menuBindLines(t)
	for i, key := range menuKeys {
		want := "bind-key -N '" + notes[key] + "' -T " + key + " "
		if !strings.HasPrefix(lines[i], want) {
			t.Fatalf("%s: line = %q, want prefix %q", key, lines[i][:min(80, len(lines[i]))], want)
		}
	}
}

// mirrorBranches maps each menu key to its bind line's mirror block: the text
// between the gate and the quoted stock branch, braces included.
func mirrorBranches(t *testing.T) map[string]string {
	t.Helper()
	gate := "'" + bridgeGate + "' "
	out := map[string]string{}
	for i, line := range menuBindLines(t) {
		head, _ := trailingQuoted(t, line)
		g := strings.Index(head, gate)
		if g < 0 {
			t.Fatalf("%s: no gate: %q", menuKeys[i], head)
		}
		out[menuKeys[i]] = strings.TrimSuffix(head[g+len(gate):], " ")
	}
	return out
}

// readQuoted reads the tmux quoted word starting at s[i]: a single-quoted word
// has no escapes, a double-quoted one the tmuxDoubleQuote set.
func readQuoted(t *testing.T, s string, i int) (value string, end int) {
	t.Helper()
	q := s[i]
	if q == '\'' {
		e := strings.IndexByte(s[i+1:], '\'')
		if e < 0 {
			t.Fatalf("unterminated single quote: %q", s[i:])
		}
		return s[i+1 : i+1+e], i + e + 2
	}
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
			b.WriteByte(s[j])
		case '"':
			return b.String(), j + 1
		default:
			b.WriteByte(s[j])
		}
	}
	t.Fatalf("unterminated double quote: %q", s[i:])
	return "", 0
}

// stripRunShellArgs inlines every run-shell -C argument, which is a tmux
// command, and deletes every other run-shell argument, which is a shell string.
func stripRunShellArgs(t *testing.T, s string) string {
	t.Helper()
	for from := 0; ; {
		i := strings.Index(s[from:], "run-shell")
		if i < 0 {
			return s
		}
		j := from + i + len("run-shell")
		inline := false
		for {
			for j < len(s) && s[j] == ' ' {
				j++
			}
			if j+1 >= len(s) || s[j] != '-' {
				break
			}
			e := strings.IndexByte(s[j:], ' ')
			if e < 0 {
				t.Fatalf("run-shell flags run to the end: %q", s[j:])
			}
			inline = inline || strings.Contains(s[j+1:j+e], "C")
			j += e
		}
		if j >= len(s) || (s[j] != '"' && s[j] != '\'') {
			t.Fatalf("run-shell without a quoted argument: %q", s[from+i:])
		}
		val, end := readQuoted(t, s, j)
		if inline {
			s = s[:j] + val + s[end:]
		} else {
			s = s[:j] + s[end:]
		}
		from = j
	}
}

var localStructuralCommands = []string{
	"kill-window", "kill-pane", "split-window", "new-window", "new-pane",
	"rename-window", "rename-session", "swap-pane", "respawn-window",
	"select-pane -m", "break-pane", "join-pane", "move-pane", "detach-client",
	"resize-pane",
}

var swapWindowRe = regexp.MustCompile(`(?:^|[^\w-])swap-window( -t :[-+]1)?(?:$|[^\w-])`)

func localStructuralViolations(t *testing.T, branch string) []string {
	t.Helper()
	s := stripRunShellArgs(t, branch)
	var bad []string
	for _, w := range localStructuralCommands {
		if regexp.MustCompile(`(?:^|[^\w-])` + regexp.QuoteMeta(w) + `(?:$|[^\w-])`).MatchString(s) {
			bad = append(bad, w)
		}
	}
	for _, m := range swapWindowRe.FindAllStringSubmatch(s, -1) {
		if m[1] == "" {
			bad = append(bad, "swap-window without a relative target")
		}
	}
	return bad
}

func TestMirrorBranchesRunNoLocalStructuralCommand(t *testing.T) {
	t.Run("run-shell -C is recursed into", func(t *testing.T) {
		if bad := localStructuralViolations(t, `{ run-shell -C "display-menu X x {rename-session}" }`); len(bad) == 0 {
			t.Fatal("rename-session inside run-shell -C went unnoticed")
		}
	})
	for key, branch := range mirrorBranches(t) {
		if bad := localStructuralViolations(t, branch); len(bad) > 0 {
			t.Errorf("%s: mirror branch runs %q locally: %q", key, bad, branch)
		}
	}
}

// buildEscaped reports whether every '#' in s is doubled, so display-menu's
// build expansion leaves no format resolved.
func buildEscaped(s string) bool {
	return !strings.Contains(strings.ReplaceAll(s, "##", ""), "#")
}

func TestMirrorCtlItemsUseTheKeybindEntryPoint(t *testing.T) {
	p := keysPaths()
	entry := `run-shell "` + esc1(bridgeCtl(p)) + " "
	verbs := map[string]bool{
		"kill-window": true, "kill-pane": true, "rename": true, "split-h": true,
		"split-v": true, "swap": true, "zoom": true, "new-window": true,
		"respawn-pane": true, "respawn-window": true,
	}
	branches := mirrorBranches(t)
	for _, key := range []string{
		"prefix <", "prefix >", "root MouseDown3Pane", "root M-MouseDown3Pane",
		"root MouseDown3Status", "root M-MouseDown3Status",
		"root MouseDown3Empty", "root M-MouseDown3Empty",
	} {
		b := branches[key]
		idx := regexp.MustCompile(`run-shell`).FindAllStringIndex(b, -1)
		if len(idx) == 0 {
			t.Fatalf("%s: no ctl item: %q", key, b)
		}
		for _, loc := range idx {
			rest := b[loc[0]:]
			if !strings.HasPrefix(rest, entry) {
				t.Fatalf("%s: run-shell is not the ctl entry point: %q", key, rest)
			}
			verb, _, _ := strings.Cut(rest[len(entry):], " ")
			if !verbs[verb] {
				t.Fatalf("%s: verb %q is not one the keybinds send", key, verb)
			}
			if arg, _ := readQuoted(t, rest, len("run-shell ")); !buildEscaped(arg) {
				t.Fatalf("%s: a format in %q would expand at menu build", key, arg)
			}
		}
	}
}

// The pane menu keeps both gestures: Respawn runs the remote verb, Reconnect
// (key e) the local renderer redial, since one menu cannot carry two items on R.
func TestMirrorRespawnItemsRouteToCtl(t *testing.T) {
	p := keysPaths()
	branches := mirrorBranches(t)

	window := branches["prefix <"]
	wantWindow := `Respawn R { ` + esc1(ctlRun(p, "respawn-window", "")) + ` }`
	if !strings.Contains(window, wantWindow) {
		t.Errorf("window menu missing %q in %q", wantWindow, window)
	}

	pane := branches["prefix >"]
	wantPane := `Respawn R { ` + esc1(ctlRun(p, "respawn-pane", "")) + ` }`
	if !strings.Contains(pane, wantPane) {
		t.Errorf("pane menu missing %q in %q", wantPane, pane)
	}
	if !strings.Contains(pane, `Reconnect e { respawn-pane -k }`) {
		t.Errorf("pane menu must keep the local Reconnect redial: %q", pane)
	}
}

func TestSessionMirrorMenuEscapesTwice(t *testing.T) {
	p := keysPaths()
	branches := mirrorBranches(t)
	for _, key := range []string{"root MouseDown3StatusLeft", "root M-MouseDown3StatusLeft"} {
		b := branches[key]
		for _, want := range []string{
			`{run-shell -b '/store/og-remote-detach ####{qs:session_name}'}`,
			`{run-shell '` + esc2(bridgeCtl(p)) + ` new-window ####{q:@bridge_pane}'}`,
		} {
			if !strings.Contains(b, want) {
				t.Fatalf("%s: missing %q in %q", key, want, b)
			}
		}
		for _, bad := range []string{"'Rename'", "rename-session", "detach-client"} {
			if strings.Contains(b, bad) {
				t.Fatalf("%s: contains %q: %q", key, bad, b)
			}
		}
	}
}

func TestEmptyMirrorMenuHasNoNewPane(t *testing.T) {
	branches := mirrorBranches(t)
	for _, key := range []string{"root MouseDown3Empty", "root M-MouseDown3Empty"} {
		b := branches[key]
		if !strings.Contains(b, `"New Window" w`) {
			t.Fatalf("%s: no New Window item: %q", key, b)
		}
		for _, bad := range []string{"New Pane", "new-pane", "join-pane"} {
			if strings.Contains(b, bad) {
				t.Fatalf("%s: contains %q: %q", key, bad, b)
			}
		}
	}
}

// The want literal is the `,` keybind's mirror branch in config/tmux.conf.tmpl
// (line 150), copied rather than read: the generator's Nix build sees only
// generator/.
func TestMirrorRenameMatchesTheKeybindAfterBuildExpansion(t *testing.T) {
	p := keysPaths()
	want := `command-prompt -I'#{@window_bridge_name}' { run-shell "` + bridgeCtl(p) +
		` rename #{q:@bridge_pane} #{qs:1}" %1 }`
	branches := mirrorBranches(t)
	for _, key := range []string{"prefix <", "root MouseDown3Status", "root M-MouseDown3Status"} {
		b := branches[key]
		start := strings.Index(b, "command-prompt")
		end := strings.Index(b[max(start, 0):], "%1 }")
		if start < 0 || end < 0 {
			t.Fatalf("%s: no rename item: %q", key, b)
		}
		item := b[start : start+end+len("%1 }")]
		if !buildEscaped(item) {
			t.Fatalf("%s: a format in %q would expand at menu build", key, item)
		}
		if got := strings.ReplaceAll(item, "##", "#"); got != want {
			t.Fatalf("%s: rename after build expansion =\n%q\nwant\n%q", key, got, want)
		}
	}
}

func TestKeyboardAndMouseMenusShareItems(t *testing.T) {
	b := mirrorBranches(t)
	pos := regexp.MustCompile(`-x [A-Z] -y [A-Z]`)
	norm := func(s string) string { return pos.ReplaceAllString(s, "-x _ -y _") }
	for _, key := range []string{"root MouseDown3Status", "root M-MouseDown3Status"} {
		if got := strings.Replace(b[key], "-t = ", "", 1); got != b["prefix <"] {
			t.Fatalf("%s window menu differs from prefix <:\n%q\n%q", key, got, b["prefix <"])
		}
	}
	mouse := b["root M-MouseDown3Pane"]
	if got := norm(strings.Replace(mouse, "-t = ", "", 1)); got != norm(b["prefix >"]) {
		t.Fatalf("mouse pane menu differs from prefix >:\n%q\n%q", got, norm(b["prefix >"]))
	}
	stock := stockFor(t, "root MouseDown3Pane").cmd
	guard := stock[:strings.Index(stock, "send-keys -M } ")+len("send-keys -M } ")]
	if want := "{ " + guard + mouse + " }"; b["root MouseDown3Pane"] != want {
		t.Fatalf("MouseDown3Pane mirror =\n%q\nwant\n%q", b["root MouseDown3Pane"], want)
	}
}

func TestPaneMenuLocalItemsAreStock(t *testing.T) {
	if !strings.Contains(stockFor(t, "prefix >").cmd, paneMenuLocalItems) {
		t.Fatal("paneMenuLocalItems is not a substring of the stock prefix > menu")
	}
}

func TestMouseMenusGateOnTheEventTarget(t *testing.T) {
	for i, line := range menuBindLines(t) {
		want := "-T " + menuKeys[i] + " if-shell -F '" + bridgeGate + "' "
		if strings.HasPrefix(menuKeys[i], "root ") {
			want = "-T " + menuKeys[i] + " if-shell -F -t = '" + bridgeGate + "' "
		}
		if !strings.Contains(line, want) {
			t.Fatalf("%s: gate is not %q: %q", menuKeys[i], want, line[:min(160, len(line))])
		}
	}
}
