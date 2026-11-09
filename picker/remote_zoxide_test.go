package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestFilterRemoteDirsSanitizes(t *testing.T) {
	got := filterRemoteDirs([]string{
		"/srv/\x1b[31mred\x1b[0m/proj",
		"/srv/line\nbreak",
		"/srv/cr\rdir",
		"/srv/nul\x00dir",
		`/srv/back\slash`,
		"relative/dir",
		"",
		"/",
		"/srv/ok",
	})
	var names []string
	for _, d := range got {
		names = append(names, d.name)
	}
	if want := []string{"proj", "ok"}; fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if len(got) == 0 {
		t.Fatal("no dirs survived the filter")
	}
	if got[0].path != "/srv/\x1b[31mred\x1b[0m/proj" {
		t.Fatalf("raw path must be kept for the remote, got %q", got[0].path)
	}
	if got[0].shown != "/srv/red/proj" {
		t.Fatalf("shown = %q, want sanitized", got[0].shown)
	}
	for _, d := range got {
		if strings.ContainsAny(d.name+d.shown, "\x1b\n\r\x00") || strings.Contains(d.name+d.shown, "break") {
			t.Fatalf("control byte or newline fragment leaked: %+v", d)
		}
	}
}

func TestFilterRemoteDirsBounds(t *testing.T) {
	long := "/" + strings.Repeat("a", maxEmitFieldLen)
	entries := []string{long}
	for i := range 150 {
		entries = append(entries, fmt.Sprintf("/d/dir%d", i))
	}
	got := filterRemoteDirs(entries)
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100 (over-length dropped, rest capped)", len(got))
	}
	if got[0].name != "dir0" {
		t.Fatalf("first = %q, want rank order kept", got[0].name)
	}
}

func TestSplitRemoteZoxideOutput(t *testing.T) {
	got := splitRemoteZoxideOutput("/a/b\n/c d\n\n/e\n")
	if want := []string{"/a/b", "/c d", "/e"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A newline inside a path splits it on the wire; the fragments are separate
	// entries here and filterRemoteDirs drops the non-absolute one.
	frag := splitRemoteZoxideOutput("/srv/a\nb\n")
	if len(frag) != 2 || frag[1] != "b" {
		t.Fatalf("fragment not a separate entry: %q", frag)
	}
	if got := splitRemoteZoxideOutput(""); len(got) != 0 {
		t.Fatalf("empty output -> %q, want none", got)
	}
}

func TestRemoteZoxideScriptQuoting(t *testing.T) {
	if strings.ContainsAny(remoteZoxideScript, `'\`) {
		t.Fatalf("script must hold no quote or backslash (login shell may be fish): %s", remoteZoxideScript)
	}
	argv := remoteZoxideArgv("lab")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=2", "-T lab -- "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing %q", joined, want)
		}
	}
	if last := argv[len(argv)-1]; !strings.HasPrefix(last, "sh -c '") || !strings.HasSuffix(last, "'") {
		t.Fatalf("body = %q, want sh -c '<script>'", last)
	}
}

func TestRemoteZoxideCmdUsesSeam(t *testing.T) {
	orig := fetchRemoteZoxideFn
	defer func() { fetchRemoteZoxideFn = orig }()
	boom := errors.New("ssh failed")
	fetchRemoteZoxideFn = func(host string) ([]string, error) {
		if host == "lab" {
			return []string{"/a"}, nil
		}
		return nil, boom
	}
	ok, isMsg := remoteZoxideCmd("lab")().(remoteZoxideMsg)
	if !isMsg || ok.host != "lab" || ok.err != nil || len(ok.entries) != 1 {
		t.Fatalf("ok msg = %+v", ok)
	}
	bad := remoteZoxideCmd("down")().(remoteZoxideMsg)
	if bad.host != "down" || !errors.Is(bad.err, boom) {
		t.Fatalf("err msg = %+v", bad)
	}
}

// zoxideHostModel is scopeModel scoped to alpha with fetched dirs.
func zoxideHostModel(entries ...string) tuiModel {
	m := scopeModel()
	m.scope = hostScope{kind: scopeHost, host: "alpha"}
	m.remoteZoxide = map[string]remoteZoxideEntry{
		"alpha": {dirs: filterRemoteDirs(entries), done: true},
	}
	return m
}

func scopedTargets(m tuiModel) []string {
	var out []string
	for _, it := range m.scopedItems(func(h string) bool { return h == m.scope.host }) {
		out = append(out, it.target)
	}
	return out
}

func remoteDirNames(rows []listItem) []string {
	var names []string
	for _, r := range rows {
		if r.remoteDirPath != "" {
			names = append(names, r.remoteDirName)
		}
	}
	return names
}

func TestScopedItemsHostScopeRemoteZoxide(t *testing.T) {
	m := zoxideHostModel("/home/u/proj", "/srv/api")
	got := m.scopedItems(func(h string) bool { return h == "alpha" })
	want := []string{
		"alpha-build", "", "remote:alpha", "remote:alpha:live", "remote:alpha:build",
		"", "remotedir:alpha:/home/u/proj", "remotedir:alpha:/srv/api",
	}
	if fmt.Sprint(scopedTargets(m)) != fmt.Sprint(want) {
		t.Fatalf("rows = %q, want %q", scopedTargets(m), want)
	}
	if len(got) < 7 {
		t.Fatalf("rows = %d, want at least 7", len(got))
	}
	div := got[5]
	if !div.isZoxideHeader || !div.isHeader || div.headerLabel != "New session" {
		t.Fatalf("divider = %+v", div)
	}
	if !strings.HasPrefix(div.plain, "── New session ") {
		t.Fatalf("divider plain = %q", div.plain)
	}
	row := got[6]
	if row.remoteDirHost != "alpha" || row.remoteDirPath != "/home/u/proj" || row.remoteDirName != "proj" {
		t.Fatalf("row fields = %+v", row)
	}
	if row.isRemoteRow || row.remoteHost != "" || row.createPath != "" {
		t.Fatalf("row must not look like a Remote-section or local-create row: %+v", row)
	}
}

func TestScopedItemsRemoteZoxideOnlyInHostScope(t *testing.T) {
	m := zoxideHostModel("/home/u/proj")
	m.scope = hostScope{kind: scopeAll}
	for _, tgt := range scopedTargets(m) {
		if strings.HasPrefix(tgt, "remotedir:") {
			t.Fatalf("scopeAll leaked a remote dir row: %q", tgt)
		}
	}
	m.scope = hostScope{}
	m = m.recombine()
	for _, it := range m.allItems {
		if it.remoteDirPath != "" {
			t.Fatalf("local scope leaked a remote dir row: %+v", it)
		}
	}
}

func TestRemoteZoxideRowsGatedByHostState(t *testing.T) {
	cases := map[string]func(*listItem){
		"inert":     func(r *listItem) { r.remoteInert = true },
		"needsAuth": func(r *listItem) { r.remoteNeedsAuth = true },
		"tailscale": func(r *listItem) { r.remoteTailscaleCheck = true },
		"down":      func(r *listItem) { r.remoteUnreachable = true },
	}
	for name, flag := range cases {
		t.Run(name, func(t *testing.T) {
			m := zoxideHostModel("/home/u/proj")
			flag(&m.remoteItems[1])
			if rows := m.remoteZoxideRows("alpha"); len(rows) != 0 {
				t.Fatalf("rows = %v, want none", rows)
			}
		})
	}
	t.Run("no block", func(t *testing.T) {
		m := zoxideHostModel("/home/u/proj")
		m.remoteItems = nil
		if rows := m.remoteZoxideRows("alpha"); len(rows) != 0 {
			t.Fatalf("rows = %v, want none", rows)
		}
	})
	t.Run("not fetched", func(t *testing.T) {
		m := zoxideHostModel()
		m.remoteZoxide = nil
		if rows := m.remoteZoxideRows("alpha"); len(rows) != 0 {
			t.Fatalf("rows = %v, want none", rows)
		}
	})
	t.Run("no dirs", func(t *testing.T) {
		if rows := zoxideHostModel().remoteZoxideRows("alpha"); len(rows) != 0 {
			t.Fatalf("rows = %v, want no divider for zero dirs", rows)
		}
	})
}

func TestRemoteZoxideRowsExclude(t *testing.T) {
	m := zoxideHostModel("/home/u/proj", "/tmp/scratch", "/home/u/.ssh")
	m.tmuxOpts["@picker_zoxide_exclude"] = "/tmp/*,.ssh"
	if names := remoteDirNames(m.remoteZoxideRows("alpha")); fmt.Sprint(names) != "[proj]" {
		t.Fatalf("names = %v, want [proj]", names)
	}
}

func TestRemoteZoxideRowsHideNameCollisions(t *testing.T) {
	m := zoxideHostModel("/x/live", "/x/build", "/x/bridged", "/x/fresh", "/y/fresh", "/x/other")
	if len(m.remoteItems) < 2 {
		t.Fatal("fixture has no host row")
	}
	m.remoteItems[1].remoteHostSessionIDs = map[string]string{"bridged": "$7"}
	rows := m.remoteZoxideRows("alpha")
	// live: block child; build: mirror; bridged: live probe id; fresh: deduped, first wins.
	if names := remoteDirNames(rows); fmt.Sprint(names) != "[fresh other]" {
		t.Fatalf("names = %v, want [fresh other]", names)
	}
	if len(rows) < 2 || rows[1].remoteDirPath != "/x/fresh" {
		t.Fatalf("dedupe kept %q, want first rank", rows[1].remoteDirPath)
	}
}

func TestRemoteZoxideRowsCapped(t *testing.T) {
	var entries []string
	for i := range maxZoxideSuggestions + 20 {
		entries = append(entries, fmt.Sprintf("/d/dir%d", i))
	}
	rows := zoxideHostModel(entries...).remoteZoxideRows("alpha")
	if len(rows) != maxZoxideSuggestions+1 {
		t.Fatalf("rows = %d, want divider + %d", len(rows), maxZoxideSuggestions)
	}
}

func TestScopedItemsRemoteZoxideNotInWindowOrEmitMode(t *testing.T) {
	m := zoxideHostModel("/home/u/proj")
	m.windowMode = true
	for _, tgt := range scopedTargets(m) {
		if strings.HasPrefix(tgt, "remotedir:") {
			t.Fatalf("window mode leaked a remote dir row: %q", tgt)
		}
	}
	m = zoxideHostModel("/home/u/proj")
	m.emitPath = "/tmp/emit"
	for _, tgt := range scopedTargets(m) {
		if strings.HasPrefix(tgt, "remotedir:") {
			t.Fatalf("emit mode leaked a remote dir row: %q", tgt)
		}
	}
}

func TestRemoteZoxideRowsRenderSanitized(t *testing.T) {
	m := zoxideHostModel("/srv/\x1b]0;pwn\x07\x1b[31mred\x1b[0m/we\u202eird\u009b2J")
	rows := m.remoteZoxideRows("alpha")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want divider + 1", len(rows))
	}
	r := rows[1]
	if r.remoteDirPath == "" || !strings.Contains(r.remoteDirPath, "\x1b") {
		t.Fatalf("raw path must ride unmodified for the remote: %q", r.remoteDirPath)
	}
	for field, s := range map[string]string{"display": r.display, "plain": r.plain, "searchText": r.searchText} {
		for _, bad := range []string{"pwn", "\x07", "\u009b", "\u202e", "\x1b[31m"} {
			if strings.Contains(s, bad) {
				t.Fatalf("%s leaks %q: %q", field, bad, s)
			}
		}
		if field != "display" && strings.Contains(s, "\x1b") {
			t.Fatalf("%s holds an escape: %q", field, s)
		}
	}
	if r.remoteDirName != "weird2J" {
		t.Fatalf("name = %q, want sanitized basename", r.remoteDirName)
	}
	if !strings.Contains(r.plain, "/srv/red/weird2J") {
		t.Fatalf("plain = %q", r.plain)
	}
}

func TestRemoteHostRowFlagsUnreachable(t *testing.T) {
	row := remoteHostRowForState(scopeOpts(), "alpha", remoteProbeUnreachable, "", false)
	if !row.remoteUnreachable {
		t.Fatalf("unreachable host row must carry remoteUnreachable: %+v", row)
	}
	if row := remoteHostRowForState(scopeOpts(), "alpha", remoteProbeOK, "", true); row.remoteUnreachable {
		t.Fatalf("live host row flagged unreachable: %+v", row)
	}
}

// zoxideRowModel is zoxideHostModel built into visible, cursor on the dir row
// whose derived name is name, pointed at a launcher that is never forked.
func zoxideRowModel(t *testing.T, name string, entries ...string) tuiModel {
	t.Helper()
	m := zoxideHostModel(entries...)
	m.tmuxOpts["@remote_open_bin"] = "/unused/og-remote-open"
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteDirName == name })
	return m
}

func TestEnterOnRemoteDirRowStartsAttachNotCreate(t *testing.T) {
	origCreate := createSession
	defer func() { createSession = origCreate }()
	createSession = func(name, path string) error {
		t.Fatalf("createSession(%q, %q) must never run for a remote dir row", name, path)
		return nil
	}
	origSwitch := switchClient
	defer func() { switchClient = origSwitch }()
	switchClient = func(target string) error {
		t.Fatalf("switchClient(%q) must never run for a remote dir row", target)
		return nil
	}

	m := zoxideRowModel(t, "proj", "/home/u/proj", "/srv/api")
	next, cmd := m.activateCurrent()
	nm := next.(tuiModel)
	if cmd == nil || nm.attach == nil {
		t.Fatalf("Enter did not start an attach: cmd=%v attach=%+v", cmd, nm.attach)
	}
	spec := nm.attach.run.spec
	if spec.host != "alpha" || spec.sess != "proj" || spec.newDir != "/home/u/proj" || spec.restore || spec.window != "" {
		t.Fatalf("spec = %+v", spec)
	}
	env := buildAttachCmd(spec, attachKillGrace).Env
	if !slices.Contains(env, "OG_REMOTE_NEW_DIR=/home/u/proj") {
		t.Fatalf("env lacks OG_REMOTE_NEW_DIR: %v", env)
	}
	if slices.Contains(env, "OG_REMOTE_RESTORE=1") {
		t.Fatal("a new-dir attach must not restore")
	}
}

func TestBuildAttachCmdNewDir(t *testing.T) {
	env := buildAttachCmd(attachSpec{bin: "/x/og-remote-open", host: "lab", sess: "p", newDir: "/a b/c"}, attachKillGrace).Env
	if !slices.Contains(env, "OG_REMOTE_NEW_DIR=/a b/c") {
		t.Fatalf("env lacks the new dir: %v", env)
	}
}

func TestCtrlXOnRemoteDirRowDoesNotForget(t *testing.T) {
	orig := zoxideForgetFn
	defer func() { zoxideForgetFn = orig }()
	zoxideForgetFn = func(path string) error {
		t.Fatalf("zoxideForgetFn(%q) must not run for a remote dir row", path)
		return nil
	}
	m := zoxideRowModel(t, "proj", "/home/u/proj")
	next, cmd := m.handleKey(wallKey("ctrl+x"))
	if cmd != nil {
		t.Fatalf("cmd = %v, want nil", cmd)
	}
	if nm := next.(tuiModel); len(nm.killConfirm) != 0 || nm.killRun != nil || nm.statusMsg != "" {
		t.Fatalf("^x staged something: confirm=%v run=%v status=%q", nm.killConfirm, nm.killRun, nm.statusMsg)
	}
}

func TestRemoteDirRowHidesKillHint(t *testing.T) {
	m := zoxideRowModel(t, "proj", "/home/u/proj")
	m.width = 160
	if hints := stripANSI(m.renderHints()); strings.Contains(hints, "^x") {
		t.Fatalf("hints advertise ^x on a remote dir row: %q", hints)
	}
}

func TestRemoteDirRowNoLocalPreview(t *testing.T) {
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "marker-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := zoxideHostModel(local)
	m.tmuxOpts["@remote_open_bin"] = "/unused/og-remote-open"
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteDirPath == local })
	m.showPreview = true
	cmd := m.loadPreviewCmd()
	if cmd == nil {
		t.Fatal("no preview cmd")
	}
	msg, ok := cmd().(previewMsg)
	if !ok {
		t.Fatalf("msg = %T", cmd())
	}
	if strings.Contains(msg.content, "marker-file") {
		t.Fatalf("preview read the local filesystem: %q", msg.content)
	}
	for _, want := range []string{"alpha", local, "Enter creates the session on alpha and opens it as a mirror"} {
		if !strings.Contains(msg.content, want) {
			t.Fatalf("preview %q lacks %q", msg.content, want)
		}
	}
	if msg.target != "remotedir:alpha:"+local {
		t.Fatalf("target = %q", msg.target)
	}
}

func TestRemoteDirRowPreviewSanitizesPath(t *testing.T) {
	m := zoxideRowModel(t, "red", "/srv/\x1b[31mred")
	m.showPreview = true
	msg := m.loadPreviewCmd()().(previewMsg)
	if strings.Contains(msg.content, "\x1b") {
		t.Fatalf("preview leaks an escape: %q", msg.content)
	}
}

func TestRemoteDirRowNotMarkableNotRelayable(t *testing.T) {
	m := zoxideRowModel(t, "proj", "/home/u/proj")
	row, _ := m.currentItem()
	if m.markable(row) {
		t.Fatal("remote dir row is markable")
	}
	if relayable(row) {
		t.Fatal("remote dir row is relayable (wall)")
	}
	if !m.isSelectable(row) {
		t.Fatal("remote dir row must be selectable")
	}
}

func TestMatchRankRemoteDirIsZoxide(t *testing.T) {
	if got := matchRank(listItem{remoteDirPath: "/x"}); got != rankZoxide {
		t.Fatalf("rank = %d, want rankZoxide", got)
	}
}

func TestLateRemoteZoxideForOtherHost(t *testing.T) {
	m := zoxideRowModel(t, "proj", "/home/u/proj", "/srv/api")
	m.remoteZoxide["beta"] = remoteZoxideEntry{fetching: true}
	before := visiblePlain(m)
	keep := m.currentTarget()

	next, cmd := m.Update(remoteZoxideMsg{host: "beta", entries: []string{"/b/one", "/b/two"}})
	nm := next.(tuiModel)
	if cmd != nil {
		t.Fatalf("cmd = %v", cmd)
	}
	if got := visiblePlain(nm); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("visible changed for another host's result:\n got %q\nwant %q", got, before)
	}
	if nm.currentTarget() != keep {
		t.Fatalf("cursor moved to %q, want %q", nm.currentTarget(), keep)
	}
	e := nm.remoteZoxide["beta"]
	if !e.done || e.fetching || len(e.dirs) != 2 {
		t.Fatalf("beta cache = %+v", e)
	}
}

func TestLateRemoteZoxideForCurrentHost(t *testing.T) {
	m := zoxideHostModel()
	m.remoteZoxide["alpha"] = remoteZoxideEntry{fetching: true}
	m = m.recombine().withFilter()
	m.cursor = len(m.visible) + 4 // stale, e.g. rows shrank under it

	next, _ := m.Update(remoteZoxideMsg{host: "alpha", entries: []string{"/home/u/proj"}})
	nm := next.(tuiModel)
	if nm.cursor < 0 || nm.cursor >= len(nm.visible) || !nm.isSelectable(nm.visible[nm.cursor]) {
		t.Fatalf("cursor %d not on a selectable row of %d", nm.cursor, len(nm.visible))
	}
	if !hasDirRow(nm, "proj") {
		t.Fatalf("dir rows missing after the result: %q", visiblePlain(nm))
	}
	// A selected row survives the rebuild.
	nm.cursor = findVisible(t, nm, func(it listItem) bool { return it.remoteDirName == "proj" })
	next, _ = nm.Update(remoteZoxideMsg{host: "alpha", entries: []string{"/other/new", "/home/u/proj"}})
	if got := next.(tuiModel).currentTarget(); got != "remotedir:alpha:/home/u/proj" {
		t.Fatalf("cursor followed to %q", got)
	}
}

func TestRemoteZoxideErrorNotCached(t *testing.T) {
	m := zoxideHostModel()
	m.remoteZoxide["alpha"] = remoteZoxideEntry{fetching: true}
	next, _ := m.Update(remoteZoxideMsg{host: "alpha", err: errors.New("ssh failed")})
	e := next.(tuiModel).remoteZoxide["alpha"]
	if e.fetching || e.done || len(e.dirs) != 0 {
		t.Fatalf("entry after error = %+v", e)
	}
}

func hasDirRow(m tuiModel, name string) bool {
	for _, it := range m.visible {
		if it.remoteDirName == name {
			return true
		}
	}
	return false
}

func visiblePlain(m tuiModel) []string {
	var out []string
	for _, it := range m.visible {
		out = append(out, it.target+"|"+it.plain)
	}
	return out
}

// fetchedHosts runs cmd and returns the hosts of every remoteZoxideMsg it (or
// its batch) produced, through the fetchRemoteZoxideFn stub.
func fetchedHosts(cmd tea.Cmd) []string {
	var hosts []string
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			for _, sub := range msg {
				run(sub)
			}
		case remoteZoxideMsg:
			hosts = append(hosts, msg.host)
		}
	}
	run(cmd)
	return hosts
}

func TestTabFetchesOncePerHost(t *testing.T) {
	orig := fetchRemoteZoxideFn
	defer func() { fetchRemoteZoxideFn = orig }()
	var result func(host string) ([]string, error)
	fetchRemoteZoxideFn = func(host string) ([]string, error) { return result(host) }
	result = func(string) ([]string, error) { return nil, nil }

	tab := func(m tuiModel) (tuiModel, []string) {
		next, cmd := m.handleKey(wallKey("tab"))
		return next.(tuiModel), fetchedHosts(cmd)
	}
	deliver := func(m tuiModel, host string) tuiModel {
		msg := remoteZoxideCmd(host)()
		next, _ := m.Update(msg)
		return next.(tuiModel)
	}

	t.Run("once per host", func(t *testing.T) {
		m := scopeModel().recombine().withFilter()
		m, got := tab(m) // local -> alpha
		if fmt.Sprint(got) != "[alpha]" || !m.remoteZoxide["alpha"].fetching {
			t.Fatalf("first Tab: fetched %v, entry %+v", got, m.remoteZoxide["alpha"])
		}
		m = deliver(m, "alpha")
		if e := m.remoteZoxide["alpha"]; !e.done || e.fetching {
			t.Fatalf("(nil, nil) must cache as done: %+v", e)
		}
		for _, hop := range []string{"beta", "all", "local"} {
			var fetched []string
			m, fetched = tab(m)
			if hop == "beta" {
				// scopeModel's beta row carries no inert flag: it fetches.
				if fmt.Sprint(fetched) != "[beta]" {
					t.Fatalf("Tab to beta fetched %v", fetched)
				}
				m = deliver(m, "beta")
				continue
			}
			if len(fetched) != 0 {
				t.Fatalf("Tab to %s fetched %v", hop, fetched)
			}
		}
		if _, got = tab(m); len(got) != 0 { // local -> alpha again
			t.Fatalf("revisit refetched %v", got)
		}
	})

	t.Run("in flight is not refetched", func(t *testing.T) {
		m := scopeModel().recombine().withFilter()
		m, _ = tab(m)
		m.scope = hostScope{kind: scopeAll}
		m, got := tab(m) // all -> local
		if len(got) != 0 {
			t.Fatalf("fetched %v", got)
		}
		m, got = tab(m) // local -> alpha, still fetching
		if len(got) != 0 {
			t.Fatalf("refetched while in flight: %v", got)
		}
		_ = m
	})

	for name, flag := range map[string]func(*listItem){
		"inert":     func(r *listItem) { r.remoteInert = true },
		"needsAuth": func(r *listItem) { r.remoteNeedsAuth = true },
		"tailscale": func(r *listItem) { r.remoteTailscaleCheck = true },
		"down":      func(r *listItem) { r.remoteUnreachable = true },
	} {
		t.Run("skips "+name, func(t *testing.T) {
			m := scopeModel()
			flag(&m.remoteItems[1])
			m = m.recombine().withFilter()
			m, got := tab(m)
			if len(got) != 0 || m.remoteZoxide["alpha"].fetching {
				t.Fatalf("fetched %v for a %s host", got, name)
			}
		})
	}

	t.Run("absent block fetches", func(t *testing.T) {
		m := scopeModel()
		m.remoteItems = nil
		m = m.recombine().withFilter()
		if _, got := tab(m); fmt.Sprint(got) != "[alpha]" {
			t.Fatalf("fetched %v, want [alpha]", got)
		}
	})

	t.Run("window mode and emit mode do not fetch", func(t *testing.T) {
		m := scopeModel()
		m.windowMode = true
		m = m.recombine().withFilter()
		if _, got := tab(m); len(got) != 0 {
			t.Fatalf("window mode fetched %v", got)
		}
		m = scopeModel()
		m.emitPath = "/tmp/emit"
		if _, got := tab(m); len(got) != 0 {
			t.Fatalf("emit mode fetched %v", got)
		}
	})

	t.Run("error is not cached", func(t *testing.T) {
		result = func(string) ([]string, error) { return nil, errors.New("ssh failed") }
		defer func() { result = func(string) ([]string, error) { return nil, nil } }()
		m := scopeModel().recombine().withFilter()
		m, got := tab(m)
		if fmt.Sprint(got) != "[alpha]" {
			t.Fatalf("fetched %v", got)
		}
		m = deliver(m, "alpha")
		if e := m.remoteZoxide["alpha"]; e.done || e.fetching {
			t.Fatalf("error cached: %+v", e)
		}
		for range 3 { // beta, all, local
			m, _ = tab(m)
		}
		if _, got = tab(m); fmt.Sprint(got) != "[alpha]" {
			t.Fatalf("next Tab fetched %v, want a refetch", got)
		}
	})
}

func TestFilterKeepsRemoteDirHeader(t *testing.T) {
	m := zoxideHostModel("/srv/live-tools", "/srv/other")
	m = m.recombine().withFilter()
	m.query = "live"
	m = m.withFilter()

	kinds := rowKinds(m)
	var sessIdx, divIdx, dirIdx = -1, -1, -1
	for i, it := range m.visible {
		switch {
		case it.target == "remote:alpha:live":
			sessIdx = i
		case it.isZoxideHeader:
			divIdx = i
		case it.remoteDirPath != "":
			if dirIdx < 0 {
				dirIdx = i
			}
			if it.remoteDirName != "live-tools" {
				t.Fatalf("non-matching dir row visible: %+v", it)
			}
		}
	}
	if sessIdx < 0 || divIdx < 0 || dirIdx < 0 {
		t.Fatalf("rows = %v", kinds)
	}
	if sessIdx >= divIdx || divIdx+1 != dirIdx {
		t.Fatalf("want Remote block, then divider directly before the first dir row: %v", kinds)
	}
}
