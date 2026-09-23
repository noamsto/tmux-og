package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain points XDG_CACHE_HOME at a regular file, so the remote session
// cache can never be created: no test reads the real ~/.cache, and tests that
// probe hosts cannot leak rows into each other. Cache tests opt in with
// useRemoteCache.
func TestMain(m *testing.M) {
	f, err := os.CreateTemp("", "lz-nocache")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f.Close()
	os.Setenv("XDG_CACHE_HOME", f.Name())
	code := m.Run()
	os.Remove(f.Name())
	os.Exit(code)
}

func useRemoteCache(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return remoteSessionCacheDir()
}

func seedRemoteCache(t *testing.T, host string, savedAt time.Time, sessions ...string) {
	t.Helper()
	writeRemoteSessionCache(host, sessions, savedAt)
	if _, ok := readRemoteSessionCache(host); !ok {
		t.Fatalf("setup: cache for %s did not round-trip", host)
	}
}

func TestRemoteSessionCacheRoundTrip(t *testing.T) {
	dir := useRemoteCache(t)
	now := time.UnixMilli(1_700_000_000_123)
	writeRemoteSessionCache("lab", []string{"mono", "other"}, now)

	c, ok := readRemoteSessionCache("lab")
	if !ok {
		t.Fatal("cache did not read back")
	}
	if c.Host != "lab" || c.SavedAt != now.UnixMilli() || strings.Join(c.Sessions, ",") != "mono,other" {
		t.Fatalf("got %+v", c)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode = %v (%v), want 0700", info.Mode().Perm(), err)
	}
	info, err = os.Stat(filepath.Join(dir, "lab.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if _, ok := readRemoteSessionCache("dead"); ok {
		t.Error("a host never cached must not read")
	}
}

// Forgetting one session must drop exactly that row and preserve the
// snapshot's SavedAt, so the host's remaining rows keep their (cached …) age
// instead of jumping to fresh the moment a kill lands.
func TestForgetRemoteSessionCache(t *testing.T) {
	useRemoteCache(t)
	savedAt := time.UnixMilli(1_700_000_000_123)
	seedRemoteCache(t, "lab", savedAt, "mono", "gone", "other")

	forgetRemoteSessionCache("lab", "gone")
	c, ok := readRemoteSessionCache("lab")
	if !ok {
		t.Fatal("cache vanished after a forget")
	}
	if strings.Join(c.Sessions, ",") != "mono,other" {
		t.Errorf("sessions = %v, want mono,other", c.Sessions)
	}
	if c.SavedAt != savedAt.UnixMilli() {
		t.Errorf("SavedAt = %d, want the original %d (a forget must not refresh the age)", c.SavedAt, savedAt.UnixMilli())
	}

	// Forgetting an absent session leaves the list untouched.
	forgetRemoteSessionCache("lab", "absent")
	if c, _ = readRemoteSessionCache("lab"); strings.Join(c.Sessions, ",") != "mono,other" {
		t.Errorf("absent forget changed sessions: %v", c.Sessions)
	}

	// A host with no cache is a no-op.
	forgetRemoteSessionCache("dead", "x")
	if _, ok := readRemoteSessionCache("dead"); ok {
		t.Error("forget created a cache for a host that had none")
	}
}

func TestRemoteSessionCacheIgnoresUntrustedDir(t *testing.T) {
	dir := useRemoteCache(t)
	writeRemoteSessionCache("lab", []string{"mono"}, time.Now())
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, ok := readRemoteSessionCache("lab"); ok {
		t.Fatal("a group-writable cache dir must not be read")
	}
	writeRemoteSessionCache("lab", []string{"planted"}, time.Now())
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if c, _ := readRemoteSessionCache("lab"); strings.Join(c.Sessions, ",") != "mono" {
		t.Fatalf("an untrusted dir must not be written into, got %+v", c)
	}
}

// Two aliases can sanitise to one filename; the host field keeps one host's
// sessions from rendering under the other.
func TestRemoteSessionCacheRejectsHostMismatch(t *testing.T) {
	useRemoteCache(t)
	writeRemoteSessionCache("a:b", []string{"mono"}, time.Now())
	if _, ok := readRemoteSessionCache("a_b"); ok {
		t.Fatal("cache written for a:b must not read back as a_b")
	}
}

func TestRemoteCacheStaleBoundary(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	at := func(age time.Duration) remoteSessionCache {
		return remoteSessionCache{SavedAt: now.Add(-age).UnixMilli()}
	}
	if remoteCacheStale(at(remoteCacheStaleAfter), now) {
		t.Error("exactly the threshold must still be fresh")
	}
	if !remoteCacheStale(at(remoteCacheStaleAfter+time.Millisecond), now) {
		t.Error("past the threshold must be stale")
	}
}

func TestCollectRemoteItemsWritesCache(t *testing.T) {
	useRemoteCache(t)
	opts := map[string]string{"@remote_bridge_hosts": "lab serverless"}
	probe := func(host string) (remoteProbeResult, error) {
		if host == "serverless" {
			return remoteProbeResult{}, errRemoteNoServer
		}
		return probeWithSessions("mono", "other"), nil
	}
	seedRemoteCache(t, "serverless", time.Now(), "gone")

	collectRemoteItems(opts, parseBridgeSessions("lab-mono|lab|mono\n"), probe, noRestore)

	if c, _ := readRemoteSessionCache("lab"); strings.Join(c.Sessions, ",") != "mono,other" {
		t.Errorf("cache must hold the raw list, bridged sessions included; got %+v", c)
	}
	if c, ok := readRemoteSessionCache("serverless"); !ok || len(c.Sessions) != 0 {
		t.Errorf("a no-server answer must cache an empty list; got %+v ok=%v", c, ok)
	}
}

func TestCollectRemoteItemsUnreachableKeepsCachedRows(t *testing.T) {
	useRemoteCache(t)
	savedAt := time.Now().Add(-time.Minute)
	seedRemoteCache(t, "lab", savedAt, "mono", "other")
	opts := map[string]string{"@remote_bridge_hosts": "lab"}
	probe := func(string) (remoteProbeResult, error) { return remoteProbeResult{}, errors.New("unreachable") }

	items := collectRemoteItems(opts, nil, probe, noRestore)
	if len(items) != 4 {
		t.Fatalf("expected header + host + 2 cached rows, got %d: %+v", len(items), items)
	}
	if !strings.Contains(items[1].plain, "unreachable") {
		t.Errorf("host row must still say unreachable: %q", items[1].plain)
	}
	for i, sess := range []string{"mono", "other"} {
		row := items[i+2]
		if row.remoteSess != sess || row.target != "remote:lab:"+sess {
			t.Errorf("row %d: got %+v, want cached %s", i, row, sess)
		}
		// Only a minute old, yet stale: the probe failed, so nothing vouches for it.
		if !strings.Contains(row.plain, "(cached 1m ago)") {
			t.Errorf("row %d must be marked stale: %q", i, row.plain)
		}
	}
	if c, _ := readRemoteSessionCache("lab"); c.SavedAt != savedAt.UnixMilli() {
		t.Errorf("a failed probe must not rewrite the cache: %+v", c)
	}
}

func TestCollectRemoteItemsSpecialStatesIgnoreCache(t *testing.T) {
	for name, probeErr := range map[string]error{
		"needs auth":       errRemoteNeedsAuth,
		"host key changed": errRemoteHostKeyChanged,
		"tailscale check":  &tailscaleCheckErr{},
	} {
		t.Run(name, func(t *testing.T) {
			useRemoteCache(t)
			savedAt := time.Now()
			seedRemoteCache(t, "lab", savedAt, "mono")
			opts := map[string]string{"@remote_bridge_hosts": "lab"}
			probe := func(string) (remoteProbeResult, error) { return remoteProbeResult{}, probeErr }

			items := collectRemoteItems(opts, nil, probe, noRestore)
			if len(items) != 2 {
				t.Fatalf("expected header + host row only, got %+v", items)
			}
			if c, _ := readRemoteSessionCache("lab"); c.SavedAt != savedAt.UnixMilli() {
				t.Errorf("cache must be left alone: %+v", c)
			}
		})
	}
}

func TestPendingRemoteItemsPaintsCache(t *testing.T) {
	useRemoteCache(t)
	seedRemoteCache(t, "lab", time.Now(), "mono", "other")
	seedRemoteCache(t, "old", time.Now().Add(-10*time.Minute), "work")
	opts := map[string]string{"@remote_bridge_hosts": "lab old"}
	bridges := firstPaintBridges([]listItem{{target: "lab-mono", session: "lab-mono", bridgeHost: "lab"}})

	var plains []string
	for _, it := range pendingRemoteItems(opts, bridges) {
		plains = append(plains, it.plain)
	}
	want := []string{
		"lab  " + remotePendingNote,
		remoteTreeMid + " other",
		"old  " + remotePendingNote,
		remoteTreeMid + " work  (cached 10m ago)",
	}
	got := plains[1:]
	if len(got) != len(want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	for i := range want {
		if !strings.HasSuffix(got[i], want[i]) {
			t.Errorf("row %d = %q, want suffix %q", i, got[i], want[i])
		}
	}
}

func TestFirstPaintCachedRemoteSearchableByHost(t *testing.T) {
	useRemoteCache(t)
	seedRemoteCache(t, "halo", time.Now(), "agents")
	opts := map[string]string{"@remote_bridge_hosts": "halo lab"}
	m := newPickerModel(false, false, false, opts, "dark", []listItem{{target: "tmux-og", searchText: "tmux-og"}}, "")
	m.query = "halo"
	m = m.withFilter()

	var targets []string
	for _, it := range m.visible {
		if !it.isHeader {
			targets = append(targets, it.target)
		}
	}
	if strings.Join(targets, ",") != "remote:halo,remote:halo:agents" {
		t.Fatalf("query %q matched %v, want the halo host and its cached session", m.query, targets)
	}
}

// The probe lands under a typed query with the cursor on a cached row: a
// session vanishes and others appear above it, and neither the selection nor
// the query may move.
func TestRemoteMsgOverCachedRowsKeepsCursorAndQuery(t *testing.T) {
	useRemoteCache(t)
	seedRemoteCache(t, "lab", time.Now().Add(-time.Hour), "mono", "other")
	opts := map[string]string{"@remote_bridge_hosts": "lab"}
	m := newPickerModel(false, false, false, opts, "dark", []listItem{{target: "tmux-og", searchText: "tmux-og"}}, "")
	m.query = "lab"
	m = m.withFilter()
	m = m.restoreCursor("remote:lab:other")
	if m.currentTarget() != "remote:lab:other" {
		t.Fatalf("setup: cursor not on the cached row: %+v", m.visible)
	}
	before := m.cursor

	probe := func(string) (remoteProbeResult, error) { return probeWithSessions("a", "b", "other"), nil }
	next, _ := m.Update(remoteMsg{items: collectRemoteItems(opts, nil, probe, noRestore)})
	nm := next.(tuiModel)

	if nm.query != "lab" {
		t.Errorf("query reset to %q", nm.query)
	}
	if got := nm.currentTarget(); got != "remote:lab:other" {
		t.Fatalf("cursor moved to %q", got)
	}
	if nm.cursor == before {
		t.Errorf("setup: the merge should have shifted the row's index (still %d)", before)
	}
	for _, it := range nm.visible {
		if it.remoteSess == "mono" {
			t.Errorf("a session the probe no longer reports must drop: %+v", it)
		}
		if strings.Contains(it.plain, "cached") {
			t.Errorf("a fresh probe must replace cached rows: %q", it.plain)
		}
	}
}
