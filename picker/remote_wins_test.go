package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRemoteWindowsOutput(t *testing.T) {
	out := strings.Join([]string{
		"machine-id-1",
		"noams",
		"Welcome to fish, the friendly interactive shell",
		"S|$1|api",
		"S|$2|web",
		"S|$3|",
		"W|$1|@3|1|server",
		"W|$1|@4|2|logs|with|pipes",
		"W|$2|@7|1|vite",
		"W|$9|@8|1|orphan",
		"W|1|@3|1|x",
		"W|$1|3|1|x",
		"W|$1|@5|x|y",
		"",
	}, "\n")

	got := parseRemoteWindowsOutput(out)

	if got.Identity != (remoteIdentity{MachineID: "machine-id-1", User: "noams"}) {
		t.Errorf("identity = %+v", got.Identity)
	}
	if want := []string{"api", "web"}; !reflect.DeepEqual(got.Sessions, want) {
		t.Errorf("sessions = %v, want %v", got.Sessions, want)
	}
	want := []remoteWindow{
		{Session: "api", SessionID: "$1", ID: "@3", Index: 1, Name: "server"},
		{Session: "api", SessionID: "$1", ID: "@4", Index: 2, Name: "logs|with|pipes"},
		{Session: "web", SessionID: "$2", ID: "@7", Index: 1, Name: "vite"},
	}
	if !reflect.DeepEqual(got.Windows, want) {
		t.Errorf("windows = %+v, want %+v", got.Windows, want)
	}
}

// Names stay raw: the session name drives the exact =sess actions, and
// sanitizing happens only where a name is rendered (remoteDisplayName).
func TestParseRemoteWindowsOutputKeepsRawNames(t *testing.T) {
	sess := "  s\x1b[31m\x07 "
	win := " w\u202e\x1b[0m "
	out := "id\nuser\nS|$1|" + sess + "\nW|$1|@1|1|" + win + "\r\n"

	got := parseRemoteWindowsOutput(out)

	if len(got.Sessions) != 1 || got.Sessions[0] != sess {
		t.Fatalf("sessions = %q, want the raw %q", got.Sessions, sess)
	}
	if len(got.Windows) != 1 || got.Windows[0].Name != win || got.Windows[0].Session != sess {
		t.Fatalf("windows = %+v, want the raw name %q", got.Windows, win)
	}
}

func TestParseRemoteWindowsOutputNoServer(t *testing.T) {
	got := parseRemoteWindowsOutput("id\nuser\n")
	if len(got.Sessions) != 0 || len(got.Windows) != 0 {
		t.Fatalf("got %+v, want no sessions or windows", got)
	}
	if got.Identity.MachineID != "id" || got.Identity.User != "user" {
		t.Fatalf("identity = %+v", got.Identity)
	}
}

func TestRemoteDisplayName(t *testing.T) {
	raw := "a\x1b[31mb\x07c\u202ed" + strings.Repeat("x", 100)
	got := remoteDisplayName(raw)
	if want := truncateCells(sanitizeStatusText(raw), 40); got != want {
		t.Fatalf("remoteDisplayName = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\x1b\x07\u202e") {
		t.Fatalf("remoteDisplayName left a control in %q", got)
	}
}

// The remote login shell may be fish, which rejects a bare `var=value`.
func TestRemoteListWindowsCmdFishSafe(t *testing.T) {
	cmd := remoteListWindowsCmd
	if !strings.HasPrefix(cmd, remoteIdentityPreamble+"; ") {
		t.Fatalf("cmd = %q, want the identity preamble first", cmd)
	}
	if got := strings.Count(cmd, "env TMUX_TMPDIR="); got != 2 {
		t.Fatalf("cmd = %q, want both remoteTmuxCmd legs (got %d)", cmd, got)
	}
	if !strings.Contains(cmd, `list-sessions -F 'S|#{session_id}|#{session_name}' \; list-windows -a -F 'W|#{session_id}|#{window_id}|#{window_index}|#{window_name}'`) {
		t.Fatalf("cmd = %q, want list-sessions \\; list-windows -a", cmd)
	}
	if strings.Contains(cmd, "td=") || strings.Contains(cmd, "; t=") {
		t.Fatalf("cmd = %q must not use shell assignments (fish-incompatible)", cmd)
	}
}

func sampleRemoteWindows() []remoteWindow {
	return []remoteWindow{
		{Session: "api", SessionID: "$1", ID: "@3", Index: 1, Name: "server"},
		{Session: "api", SessionID: "$1", ID: "@4", Index: 2, Name: "logs"},
		{Session: "web", SessionID: "$2", ID: "@7", Index: 1, Name: "vite"},
	}
}

func TestRemoteWindowCacheRoundTrip(t *testing.T) {
	useRemoteCache(t)
	now := time.UnixMilli(1_700_000_000_123)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), now)

	c, ok := readRemoteWindowCache("lab")
	if !ok {
		t.Fatal("cache did not read back")
	}
	if c.Host != "lab" || c.SavedAt != now.UnixMilli() || !reflect.DeepEqual(c.Windows, sampleRemoteWindows()) {
		t.Fatalf("got %+v", c)
	}
	dir := remoteWindowCacheDir()
	if filepath.Base(dir) != "remote-windows" || filepath.Dir(dir) != filepath.Dir(remoteSessionCacheDir()) {
		t.Errorf("window cache dir = %q, want a remote-windows sibling of %q", dir, remoteSessionCacheDir())
	}
	info, err := os.Stat(filepath.Join(dir, "lab.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode = %v, want 0600", info.Mode().Perm())
	}
	if _, ok := readRemoteWindowCache("dead"); ok {
		t.Error("a host never cached must not read")
	}
	if _, ok := readRemoteSessionCache("lab"); ok {
		t.Error("the window cache must not feed the session cache")
	}
}

func TestRemoteWindowCacheRejectsHostMismatch(t *testing.T) {
	useRemoteCache(t)
	writeRemoteWindowCache("a:b", sampleRemoteWindows(), time.Now())
	if _, ok := readRemoteWindowCache("a/b"); ok {
		t.Fatal("a cache written for another host must not read")
	}
	if _, ok := readRemoteWindowCache("a:b"); !ok {
		t.Fatal("the owning host must still read its cache")
	}
}

func TestRemoteWindowCacheIgnoresUntrustedDir(t *testing.T) {
	useRemoteCache(t)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), time.Now())
	dir := remoteWindowCacheDir()
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, ok := readRemoteWindowCache("lab"); ok {
		t.Fatal("a group-writable cache dir must not be read")
	}
	writeRemoteWindowCache("lab", nil, time.Now())
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if c, _ := readRemoteWindowCache("lab"); len(c.Windows) != 3 {
		t.Fatalf("an untrusted dir must not be written into, got %+v", c)
	}
}

func TestForgetRemoteWindowCache(t *testing.T) {
	useRemoteCache(t)
	savedAt := time.UnixMilli(1_700_000_000_123)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), savedAt)

	forgetRemoteWindowCache("lab", "@4")
	c, ok := readRemoteWindowCache("lab")
	if !ok {
		t.Fatal("cache vanished after a forget")
	}
	if len(c.Windows) != 2 || c.Windows[0].ID != "@3" || c.Windows[1].ID != "@7" {
		t.Errorf("windows = %+v, want @3 and @7", c.Windows)
	}
	if c.SavedAt != savedAt.UnixMilli() {
		t.Errorf("SavedAt = %d, want the original %d", c.SavedAt, savedAt.UnixMilli())
	}

	forgetRemoteWindowCache("dead", "@1")
	if _, ok := readRemoteWindowCache("dead"); ok {
		t.Error("forget created a cache for a host that had none")
	}
}

func TestForgetRemoteSessionWindowsCache(t *testing.T) {
	useRemoteCache(t)
	savedAt := time.UnixMilli(1_700_000_000_123)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), savedAt)

	forgetRemoteSessionWindowsCache("lab", "api")
	c, ok := readRemoteWindowCache("lab")
	if !ok {
		t.Fatal("cache vanished after a forget")
	}
	if len(c.Windows) != 1 || c.Windows[0].Session != "web" {
		t.Errorf("windows = %+v, want only web's", c.Windows)
	}
	if c.SavedAt != savedAt.UnixMilli() {
		t.Errorf("SavedAt = %d, want the original %d", c.SavedAt, savedAt.UnixMilli())
	}
}

func probeWithWindows(id remoteIdentity, wins ...remoteWindow) remoteProbeResult {
	res := remoteProbeResult{Identity: id, Windows: wins}
	seen := map[string]bool{}
	for _, w := range wins {
		if !seen[w.Session] {
			seen[w.Session] = true
			res.Sessions = append(res.Sessions, w.Session)
		}
	}
	return res
}

func winProbe(res remoteProbeResult, err error) func(string) (remoteProbeResult, error) {
	return func(string) (remoteProbeResult, error) { return res, err }
}

func TestRemoteWindowRowItem(t *testing.T) {
	w := remoteWindow{Session: "api", SessionID: "$1", ID: "@3", Index: 2, Name: "server"}
	row := remoteWindowRowItem("lab", w, "", "<H>", "<D>", false)

	if want := "<H>" + remoteTreeMid + "\033[0m <D>api\033[0m 2: server"; row.display != want {
		t.Errorf("display = %q, want %q", row.display, want)
	}
	if want := "<H>" + remoteTreeEnd + "\033[0m <D>api\033[0m 2: server"; row.displayEnd != want {
		t.Errorf("displayEnd = %q, want %q", row.displayEnd, want)
	}
	if want := remoteTreeMid + " api 2: server"; row.plain != want {
		t.Errorf("plain = %q, want %q", row.plain, want)
	}
	if want := remoteTreeEnd + " api 2: server"; row.plainEnd != want {
		t.Errorf("plainEnd = %q, want %q", row.plainEnd, want)
	}
	if !row.isRemoteRow || row.target != "remote:lab:api:@3" || row.remoteHost != "lab" || row.remoteSess != "api" {
		t.Errorf("identity fields wrong: %+v", row)
	}
	if row.remoteWindowID != "@3" || row.remoteWindowIndex != 2 || row.remoteWindowName != "server" || row.remoteLive {
		t.Errorf("window fields wrong: %+v", row)
	}
	if want := "lab/api:2 lab api server"; row.searchText != want {
		t.Errorf("searchText = %q, want %q", row.searchText, want)
	}
}

func TestRemoteWindowRowItemNoteAndDim(t *testing.T) {
	w := remoteWindow{Session: "api", ID: "@3", Index: 1, Name: "server"}

	noted := remoteWindowRowItem("lab", w, "(cached 10m ago)", "", "<D>", false)
	if want := remoteTreeMid + " api 1: server  (cached 10m ago)"; noted.plain != want {
		t.Errorf("plain = %q, want %q", noted.plain, want)
	}
	if want := "\033[0m <D>api\033[0m 1: server<D>  (cached 10m ago)\033[0m"; !strings.HasSuffix(noted.display, want) {
		t.Errorf("display = %q, want suffix %q", noted.display, want)
	}

	dimmed := remoteWindowRowItem("lab", w, "(cached 10m ago)", "", "<D>", true)
	if want := "\033[0m <D>" + "api 1: server  (cached 10m ago)\033[0m"; !strings.HasSuffix(dimmed.display, want) {
		t.Errorf("display = %q, want suffix %q", dimmed.display, want)
	}
}

func TestRemoteWindowRowItemSanitizesNames(t *testing.T) {
	w := remoteWindow{
		Session: "s\x1b[31mess\x07",
		ID:      "@1", Index: 1,
		Name: "n\u202eame\x1b]0;x\x07" + strings.Repeat("y", 100),
	}
	row := remoteWindowRowItem("lab", w, "", "", "", false)

	for _, s := range []string{row.display, row.displayEnd} {
		if strings.ContainsRune(strings.ReplaceAll(s, "\033[0m", ""), 0x1b) || strings.Contains(s, "\u202e") || strings.Contains(s, "\x07") {
			t.Errorf("display %q carries a remote control sequence", s)
		}
	}
	for _, s := range []string{row.plain, row.plainEnd, row.searchText, row.remoteWindowName} {
		if strings.ContainsAny(s, "\x1b\x07\u202e") {
			t.Errorf("%q carries a remote control sequence", s)
		}
	}
	if got := visibleWidth(row.remoteWindowName); got > remoteDisplayNameCells {
		t.Errorf("window name is %d cells, want <= %d", got, remoteDisplayNameCells)
	}
	if row.remoteSess != w.Session || row.target != "remote:lab:"+w.Session+":@1" {
		t.Errorf("raw session must stay in remoteSess/target: %+v", row)
	}
}

func windowRowTargets(items []listItem) []string {
	var out []string
	for _, it := range items {
		if it.remoteWindowID != "" {
			out = append(out, it.target)
		}
	}
	return out
}

func TestCollectRemoteWindowItems(t *testing.T) {
	useRemoteCache(t)
	opts := map[string]string{"@remote_bridge_hosts": "lab"}
	res := probeWithWindows(remoteIdentity{}, sampleRemoteWindows()...)

	items := collectRemoteWindowItems(opts, nil, winProbe(res, nil))

	if len(items) != 5 {
		t.Fatalf("want header + host + 3 windows, got %d: %+v", len(items), items)
	}
	if !items[0].isRemoteHeader {
		t.Errorf("first row should be the remote header: %+v", items[0])
	}
	host := items[1]
	if host.remoteHost != "lab" || host.remoteSess != "" || host.plain != iconSession+" lab" {
		t.Errorf("host row = %+v, want a bare lab row with no note", host)
	}
	want := []string{"remote:lab:api:@3", "remote:lab:api:@4", "remote:lab:web:@7"}
	if got := windowRowTargets(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("window targets = %v, want %v", got, want)
	}
	first := items[2]
	if first.remoteHost != "lab" || first.remoteSess != "api" || first.remoteWindowID != "@3" || first.remoteWindowIndex != 1 || !first.remoteLive {
		t.Errorf("first window row = %+v", first)
	}
	if first.searchText != "lab/api:1 lab api server" {
		t.Errorf("searchText = %q", first.searchText)
	}
	for _, it := range items[2:] {
		if !it.remoteLive || it.remoteUnreachable {
			t.Errorf("probe-OK row must be live and reachable: %+v", it)
		}
	}
	c, ok := readRemoteWindowCache("lab")
	if !ok || !reflect.DeepEqual(c.Windows, sampleRemoteWindows()) {
		t.Errorf("probe OK must write the window cache, got %+v ok=%v", c, ok)
	}
}

func TestCollectRemoteWindowItemsBridgedSessions(t *testing.T) {
	useRemoteCache(t)
	opts := map[string]string{"@remote_bridge_hosts": "lab"}
	res := probeWithWindows(remoteIdentity{}, sampleRemoteWindows()...)

	items := collectRemoteWindowItems(opts, parseBridgeSessions("lab-api|lab|api\n"), winProbe(res, nil))
	if len(items) == 0 {
		t.Fatal("no items collected")
	}
	if got, want := windowRowTargets(items), []string{"remote:lab:web:@7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("window targets = %v, want only web's %v", got, want)
	}
	if strings.Contains(items[1].plain, "all open") {
		t.Errorf("host row = %q, want no (all open) while web is unbridged", items[1].plain)
	}
	c, _ := readRemoteWindowCache("lab")
	if len(c.Windows) != 3 {
		t.Errorf("the cache keeps the raw list before bridge suppression, got %+v", c.Windows)
	}

	both := parseBridgeSessions("lab-api|lab|api\nlab-web|lab|web\n")
	items = collectRemoteWindowItems(opts, both, winProbe(res, nil))
	if len(items) != 2 {
		t.Fatalf("every session bridged: want header + host row, got %d: %+v", len(items), items)
	}
	if !strings.Contains(items[1].plain, "(all open)") {
		t.Errorf("host row = %q, want (all open)", items[1].plain)
	}
}

func TestCollectRemoteWindowItemsNoServer(t *testing.T) {
	useRemoteCache(t)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), time.Now())
	opts := map[string]string{"@remote_bridge_hosts": "lab"}

	items := collectRemoteWindowItems(opts, nil, winProbe(remoteProbeResult{}, errRemoteNoServer))
	if len(items) != 2 {
		t.Fatalf("want header + host row, got %d: %+v", len(items), items)
	}
	if !strings.Contains(items[1].plain, "(no server — Enter starts one)") || items[1].target == "" {
		t.Errorf("host row = %+v, want the selectable no-server row", items[1])
	}
	if c, ok := readRemoteWindowCache("lab"); !ok || len(c.Windows) != 0 {
		t.Errorf("no server must overwrite the cache with an empty one, got %+v ok=%v", c, ok)
	}

	items = collectRemoteWindowItems(opts, nil, winProbe(remoteProbeResult{}, nil))
	if len(items) != 2 || !strings.Contains(items[1].plain, "no server") {
		t.Errorf("an empty probe is no server, got %+v", items)
	}
}

func TestCollectRemoteWindowItemsUnreachableKeepsCache(t *testing.T) {
	useRemoteCache(t)
	writeRemoteWindowCache("lab", sampleRemoteWindows(), time.Now())
	opts := map[string]string{"@remote_bridge_hosts": "lab"}

	items := collectRemoteWindowItems(opts, parseBridgeSessions("lab-web|lab|web\n"), winProbe(remoteProbeResult{}, errors.New("ssh down")))
	if len(items) < 2 {
		t.Fatalf("items = %d, want a header and a host row", len(items))
	}
	if !strings.Contains(items[1].plain, "(unreachable — open default)") {
		t.Errorf("host row = %q, want the unreachable note", items[1].plain)
	}
	if got, want := windowRowTargets(items), []string{"remote:lab:api:@3", "remote:lab:api:@4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cached targets = %v, want %v (web is bridged)", got, want)
	}
	for _, it := range items[2:] {
		if !it.remoteUnreachable || it.remoteLive {
			t.Errorf("cached row of a down host: want remoteUnreachable and not live: %+v", it)
		}
		if !strings.Contains(it.plain, "(cached ") {
			t.Errorf("unreachable rows are always stale and say so: %q", it.plain)
		}
	}
	if c, ok := readRemoteWindowCache("lab"); !ok || len(c.Windows) != 3 {
		t.Errorf("an unreachable probe must not touch the cache, got %+v ok=%v", c, ok)
	}
}

func TestCollectRemoteWindowItemsInteractiveStates(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		note  string
		check func(listItem) bool
	}{
		{"needs auth", fmt.Errorf("%w: x", errRemoteNeedsAuth), "(auth needed — Enter to connect)", func(r listItem) bool { return r.remoteNeedsAuth }},
		{"host key", fmt.Errorf("%w: x", errRemoteHostKeyChanged), "(host key changed — verify manually)", func(r listItem) bool { return r.remoteInert }},
		{"tailscale", &tailscaleCheckErr{url: "https://login.tailscale.com/a/xyz"}, "(tailscale check — run: ssh lab)", func(r listItem) bool {
			return r.remoteTailscaleCheck && r.remoteTailscaleURL == "https://login.tailscale.com/a/xyz"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useRemoteCache(t)
			writeRemoteWindowCache("lab", sampleRemoteWindows(), time.Now())
			opts := map[string]string{"@remote_bridge_hosts": "lab"}

			items := collectRemoteWindowItems(opts, nil, winProbe(remoteProbeResult{}, tc.err))
			if len(items) != 2 {
				t.Fatalf("want header + host row only (no cached rows), got %d: %+v", len(items), items)
			}
			if !strings.Contains(items[1].plain, tc.note) || !tc.check(items[1]) {
				t.Errorf("host row = %+v, want note %q and its flag", items[1], tc.note)
			}
		})
	}
}

func TestCollectRemoteWindowItemsDropsSelfHost(t *testing.T) {
	const selfID = "test-machine-id-self-windows"
	local := remoteIdentity{MachineID: selfID, User: "noams"}
	readLocalRemoteIdentity = func() remoteIdentity { return local }
	t.Cleanup(func() {
		readLocalRemoteIdentity = func() remoteIdentity {
			return remoteIdentity{MachineID: readLocalMachineID(), User: localUsername()}
		}
	})
	useRemoteCache(t)

	opts := map[string]string{"@remote_bridge_hosts": "localhost lab"}
	probe := func(host string) (remoteProbeResult, error) {
		if host == "localhost" {
			return probeWithWindows(local, remoteWindow{Session: "me", ID: "@1", Index: 1, Name: "x"}), nil
		}
		return probeWithWindows(remoteIdentity{MachineID: "other", User: "noams"}, remoteWindow{Session: "work", ID: "@2", Index: 1, Name: "y"}), nil
	}

	items := collectRemoteWindowItems(opts, nil, probe)
	if len(items) != 3 {
		t.Fatalf("want header + lab + work window, got %d: %+v", len(items), items)
	}
	for _, it := range items {
		if it.remoteHost == "localhost" {
			t.Fatalf("self alias must be dropped: %+v", it)
		}
	}
	if !isCachedRemoteSelfAlias("localhost") {
		t.Error("self alias should be cached after probe")
	}
}

func TestCollectRemoteWindowItemsNoHosts(t *testing.T) {
	probe := func(string) (remoteProbeResult, error) {
		t.Fatal("probe must not run")
		return remoteProbeResult{}, nil
	}
	if items := collectRemoteWindowItems(map[string]string{}, nil, probe); items != nil {
		t.Fatalf("no hosts: want nil, got %+v", items)
	}
}

func TestPendingRemoteWindowItems(t *testing.T) {
	useRemoteCache(t)
	now := time.Now()
	writeRemoteWindowCache("lab", sampleRemoteWindows(), now)
	writeRemoteWindowCache("old", sampleRemoteWindows()[:1], now.Add(-10*time.Minute))
	opts := map[string]string{"@remote_bridge_hosts": "lab old fresh"}

	items := pendingRemoteWindowItems(opts, parseBridgeSessions("lab-web|lab|web\n"))
	if len(items) == 0 {
		t.Fatal("no pending items")
	}
	if !items[0].isRemoteHeader {
		t.Fatalf("first row should be the remote header: %+v", items[0])
	}
	var hosts []string
	for _, it := range items {
		if it.remoteHost != "" && it.remoteSess == "" {
			hosts = append(hosts, it.remoteHost)
			if !strings.HasSuffix(it.plain, remotePendingNote) {
				t.Errorf("host row %q lacks the pending note", it.plain)
			}
		}
	}
	if !reflect.DeepEqual(hosts, []string{"lab", "old", "fresh"}) {
		t.Fatalf("host rows = %v", hosts)
	}
	want := []string{"remote:lab:api:@3", "remote:lab:api:@4", "remote:old:api:@3"}
	if got := windowRowTargets(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached targets = %v, want %v (web is bridged)", got, want)
	}
	for _, it := range items {
		if it.remoteWindowID == "" {
			continue
		}
		if it.remoteLive || it.remoteUnreachable {
			t.Errorf("a first-paint cached row is neither live nor confirmed down: %+v", it)
		}
		stale := it.remoteHost == "old"
		if stale != strings.Contains(it.plain, "(cached 10m ago)") {
			t.Errorf("row %q: stale = %v mismatch", it.plain, stale)
		}
		if stale && !strings.HasSuffix(it.display, "api 1: server  (cached 10m ago)\033[0m") {
			t.Errorf("stale row %q should be dimmed: %q", it.plain, it.display)
		}
	}
}

func TestPendingRemoteWindowItemsNoHosts(t *testing.T) {
	if items := pendingRemoteWindowItems(map[string]string{}, nil); items != nil {
		t.Fatalf("no hosts: want nil, got %+v", items)
	}
}

// searchText carries the full names: the display truncation must not make the
// tail of a long name unsearchable.
func TestRemoteWindowRowItemSearchTextKeepsFullNames(t *testing.T) {
	name := strings.Repeat("alpha ", 10) + "zebrafinal"
	w := remoteWindow{Session: "api", SessionID: "$1", ID: "@3", Index: 1, Name: name}
	row := remoteWindowRowItem("lab", w, "", "", "", false)
	if !strings.Contains(row.searchText, "zebrafinal") {
		t.Errorf("searchText = %q, want the last word of the long name", row.searchText)
	}
	if visibleWidth(row.remoteWindowName) > remoteDisplayNameCells {
		t.Errorf("remoteWindowName must stay truncated, got %q", row.remoteWindowName)
	}
}

func TestRemoteWindowRowItemCarriesSessionID(t *testing.T) {
	w := remoteWindow{Session: "api", SessionID: "$9", ID: "@3", Index: 1, Name: "x"}
	if got := remoteWindowRowItem("lab", w, "", "", "", false).remoteSessionID; got != "$9" {
		t.Errorf("remoteSessionID = %q, want $9", got)
	}
}
