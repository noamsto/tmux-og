package main

import "testing"

// zoxide suggestions are local-scope-only: they have no host to be scoped to.
func TestScopedItemsExcludesZoxideRows(t *testing.T) {
	m := scopeModel()
	m.zoxideItems = []listItem{
		{target: "/git/alpha", createPath: "/git/alpha", createName: "alpha", searchText: "alpha"},
	}
	got := m.scopedItems(func(h string) bool { return h == "alpha" })
	for _, it := range got {
		if it.createPath != "" {
			t.Fatalf("zoxide row leaked into host scope: %+v", it)
		}
	}
}

// Enter on an already-mirrored Remote-section row switches to the mirror's
// local session, via the switchClient seam, instead of opening a duplicate.
func TestActivateCurrentSwitchesToMirrorTarget(t *testing.T) {
	orig := switchClient
	var got string
	switchClient = func(target string) error {
		got = target
		return nil
	}
	defer func() { switchClient = orig }()

	m := scopeModel()
	m.scope = hostScope{kind: scopeHost, host: "alpha"}
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteMirrorTarget != "" })

	_, cmd := m.activateCurrent()
	if cmd == nil {
		t.Fatal("expected tea.Quit")
	}
	if got != "alpha-build" {
		t.Fatalf("switchClient called with %q, want %q", got, "alpha-build")
	}
}

// A query narrows within the active scope, same fuzzy-match path as local
// scope: a host's rows that don't match are hidden, matching ones stay.
func TestFilterNarrowsWithinScope(t *testing.T) {
	m := scopeModel()
	m.scope = hostScope{kind: scopeHost, host: "alpha"}
	m.query = "live"
	m = m.recombine().withFilter()

	for _, it := range m.visible {
		if it.remoteSess != "" && it.remoteSess != "live" {
			t.Fatalf("query %q left non-matching session row visible: %+v", m.query, it)
		}
	}
	if findVisible(t, m, func(it listItem) bool { return it.remoteSess == "live" }) < 0 {
		t.Fatal("matching session row was filtered out")
	}
}

// An inert host (needs-auth / host-key-changed / tailscale-check) carries the
// same invariant into scope that local scope already enforces: no session
// rows under it (collectRemoteItems never emits any), and ^t / Enter remain
// no-ops on the host row itself — exactly TestCtrlTNoOpOnInertRows's table,
// driven through host scope this time.
func TestInertHostRowsNoOpInHostScope(t *testing.T) {
	cases := []struct {
		name      string
		flag      func(*listItem)
		enterNoOp bool // false only for needs-auth, which runs og-remote-auth
	}{
		{"needs auth", func(it *listItem) { it.remoteNeedsAuth = true }, false},
		{"host key changed", func(it *listItem) { it.remoteInert = true }, true},
		{"tailscale check", func(it *listItem) { it.remoteTailscaleCheck = true }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := scopeOpts()
			hostRow := remoteHostRowItem(opts, "beta", "")
			c.flag(&hostRow)
			m := tuiModel{
				tmuxOpts:    opts,
				remoteItems: []listItem{remoteHeaderItem(opts), hostRow},
				scope:       hostScope{kind: scopeHost, host: "beta"},
			}
			m = m.recombine().withFilter()
			for _, it := range m.visible {
				if it.remoteHost == "beta" && it.remoteSess != "" {
					t.Fatalf("inert host has a session row in scope: %+v", it)
				}
			}
			m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteHost == "beta" })

			next, _ := m.handleKey(wallKey("ctrl+t"))
			nm := next.(tuiModel)
			if len(nm.marked) != 0 {
				t.Errorf("^t marked an inert host row: %v", nm.marked)
			}

			next, cmd := nm.handleKey(wallKey("enter"))
			em := next.(tuiModel)
			if !c.enterNoOp {
				if cmd == nil {
					t.Errorf("expected an og-remote-auth exec cmd")
				}
				return
			}
			if cmd != nil {
				t.Errorf("Enter on inert host row returned %v, want no-op", cmd)
			}
			if em.statusMsg == "" {
				t.Error("Enter on inert host row should set a status message, not silently do nothing")
			}
		})
	}
}

// markedRemoteItems resolves against the unscoped m.sessionItems+m.remoteItems
// superset, not m.allItems — a mark set while scoped to one host must still
// resolve after cycling away to another scope entirely.
func TestMarkedRemoteItemsResolvesAcrossScopeSwitch(t *testing.T) {
	m := scopeModel()
	m.scope = hostScope{kind: scopeHost, host: "alpha"}
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteHost == "alpha" && it.remoteSess == "live" })

	next, _ := m.handleKey(wallKey("ctrl+t"))
	m = next.(tuiModel)
	if !m.marked["remote:alpha:live"] {
		t.Fatalf("setup: mark did not take: %v", m.marked)
	}

	m.scope = hostScope{kind: scopeHost, host: "beta"}
	m = m.recombine().withFilter()

	marked := m.markedRemoteItems()
	if len(marked) != 1 || marked[0].target != "remote:alpha:live" {
		t.Fatalf("markedRemoteItems() = %+v, want the alpha mark to survive the scope switch", marked)
	}

	var opened struct{ host, sess string }
	fakeOpen := func(_ map[string]string, host, sess string, _ bool) error {
		opened.host, opened.sess = host, sess
		return nil
	}
	_, cmd := m.openMarkedRemoteWith(marked, fakeOpen, func(map[string]string, string, string, bool) {})
	if cmd == nil {
		t.Fatal("expected tea.Quit")
	}
	if opened.host != "alpha" || opened.sess != "live" {
		t.Fatalf("launcher fired for %+v, want alpha/live", opened)
	}
}

// Marks are keyed by item.target, independent of m.scope: a mark set in local
// scope must survive cycling through host and all scope and back.
func TestMarksSurviveScopeCycle(t *testing.T) {
	m := scopeModel()
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteHost == "alpha" && it.remoteSess == "live" })

	next, _ := m.handleKey(wallKey("ctrl+t"))
	m = next.(tuiModel)
	if !m.marked["remote:alpha:live"] {
		t.Fatalf("setup: mark did not take: %v", m.marked)
	}

	for _, want := range []hostScope{
		{kind: scopeHost, host: "alpha"},
		{kind: scopeHost, host: "beta"},
		{kind: scopeAll},
		{},
	} {
		next, _ := m.handleKey(wallKey("tab"))
		m = next.(tuiModel)
		if m.scope != want {
			t.Fatalf("scope = %+v, want %+v", m.scope, want)
		}
		if !m.marked["remote:alpha:live"] {
			t.Fatalf("mark lost after cycling to scope %+v: %v", m.scope, m.marked)
		}
	}
}

// A synthesized mirror row is never markable, in host or all scope.
func TestMirroredRowNeverMarkableInScope(t *testing.T) {
	m := scopeModel()
	m.scope = hostScope{kind: scopeHost, host: "alpha"}
	m = m.recombine().withFilter()
	m.cursor = findVisible(t, m, func(it listItem) bool { return it.remoteMirrorTarget != "" })

	next, _ := m.handleKey(wallKey("ctrl+t"))
	nm := next.(tuiModel)
	if len(nm.marked) != 0 {
		t.Errorf("^t marked a mirror row: %v", nm.marked)
	}
}

// Tab is a no-op in window mode and emit mode — neither has remote data to
// build a scope from (round-3 note).
func TestTabNoOpInWindowAndEmitMode(t *testing.T) {
	cases := []struct {
		name string
		m    tuiModel
	}{
		{"window mode", tuiModel{tmuxOpts: scopeOpts(), windowMode: true}},
		{"emit mode", tuiModel{tmuxOpts: scopeOpts(), emitPath: "/tmp/emit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, _ := c.m.handleKey(wallKey("tab"))
			nm := next.(tuiModel)
			if nm.scope != (hostScope{}) {
				t.Fatalf("scope = %+v, want local", nm.scope)
			}
		})
	}
}

func scopeOpts() map[string]string {
	return map[string]string{"@remote_bridge_hosts": "alpha beta"}
}

// scopeModel has one plain local session, one mirror of alpha, alpha's remote
// tree with a live child, and beta's tree with no children (needs-auth).
func scopeModel() tuiModel {
	opts := scopeOpts()
	return tuiModel{
		tmuxOpts: opts,
		sessionItems: []listItem{
			{target: "work", plain: "work"},
			{target: "alpha-build", plain: "alpha-build", bridgeHost: "alpha"},
		},
		remoteItems: []listItem{
			remoteHeaderItem(opts),
			remoteHostRowItem(opts, "alpha", ""),
			remoteSessionRowItem("alpha", "live", "", "", "", false),
			remoteHostRowItem(opts, "beta", "(auth needed — Enter to connect)"),
		},
		mirrors: []bridgeMirror{
			{host: "alpha", sess: "build", target: "alpha-build"},
			{host: "beta", sess: "api", target: "beta-api"},
		},
	}
}

func TestScopedItemsHostScope(t *testing.T) {
	m := scopeModel()
	got := m.scopedItems(func(h string) bool { return h == "alpha" })

	var targets []string
	for _, it := range got {
		targets = append(targets, it.target)
	}
	want := []string{"alpha-build", "", "remote:alpha", "remote:alpha:live", "remote:alpha:build"}
	if len(targets) != len(want) {
		t.Fatalf("rows = %v, want %v", targets, want)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("rows = %v, want %v", targets, want)
		}
	}
	if !got[1].isRemoteHeader {
		t.Fatalf("row 1 is not the Remote header: %+v", got[1])
	}
	if got[4].remoteMirrorTarget != "alpha-build" {
		t.Fatalf("mirror row target = %q, want alpha-build", got[4].remoteMirrorTarget)
	}
}

// A mirror row belongs to its own host's block, never appended after every
// other host's rows.
func TestScopedItemsAllScopeGroupsByHost(t *testing.T) {
	m := scopeModel()
	configured := hostSet(configuredHosts(m.tmuxOpts))
	got := m.scopedItems(func(h string) bool { return configured[h] })

	lastAlpha, firstBeta := -1, -1
	for i, it := range got {
		switch it.remoteHost {
		case "alpha":
			lastAlpha = i
		case "beta":
			if firstBeta < 0 {
				firstBeta = i
			}
		}
	}
	if lastAlpha < 0 || firstBeta < 0 || lastAlpha > firstBeta {
		t.Fatalf("alpha rows must precede beta rows: lastAlpha=%d firstBeta=%d", lastAlpha, firstBeta)
	}
	// beta's probe left it childless; only its mirror row may follow.
	if got[firstBeta+1].target != "remote:beta:api" {
		t.Fatalf("beta block = %+v", got[firstBeta:])
	}
	for _, it := range got {
		if it.target == "work" {
			t.Fatal("plain local session leaked into all-hosts scope")
		}
	}
}

func TestNextScopeCycle(t *testing.T) {
	hosts := []string{"alpha", "beta"}
	want := []hostScope{
		{kind: scopeHost, host: "alpha"},
		{kind: scopeHost, host: "beta"},
		{kind: scopeAll},
		{},
	}
	cur := hostScope{}
	for i, w := range want {
		cur = nextScope(cur, hosts)
		if cur != w {
			t.Fatalf("step %d: scope = %+v, want %+v", i, cur, w)
		}
	}
}

func TestTabCyclesScopeAndRebuilds(t *testing.T) {
	m := scopeModel()
	next, _ := m.handleKey(wallKey("tab"))
	got := next.(tuiModel)
	if got.scope != (hostScope{kind: scopeHost, host: "alpha"}) {
		t.Fatalf("scope = %+v", got.scope)
	}
	for _, it := range got.allItems {
		if it.target == "work" {
			t.Fatal("plain local session survived the scope switch")
		}
	}
}

func TestTabIsNoOpWithoutConfiguredHosts(t *testing.T) {
	m := scopeModel()
	m.tmuxOpts = map[string]string{}
	next, _ := m.handleKey(wallKey("tab"))
	if got := next.(tuiModel); got.scope != (hostScope{}) {
		t.Fatalf("scope = %+v, want local", got.scope)
	}
}
