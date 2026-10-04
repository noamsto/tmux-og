package main

import (
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
