package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKVGet mirrors scripts/og-remote-picker.sh's kv_get: the value is
// everything after the first '=' on the last matching line. There's no Go
// binding to the shell parser, so round-trip tests reimplement its contract
// here rather than skip it.
func testKVGet(text, want string) (string, bool) {
	val, found := "", false
	for _, line := range strings.Split(text, "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 {
			continue
		}
		if line[:i] != want {
			continue
		}
		val, found = line[i+1:], true
	}
	return val, found
}

func TestEmitPayloadEncodeSession(t *testing.T) {
	p := emitPayload{kind: "session", name: "tmux-og"}
	out, err := p.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if kind, _ := testKVGet(out, "kind"); kind != "session" {
		t.Errorf("kind = %q, want session", kind)
	}
	if name, _ := testKVGet(out, "name"); name != "tmux-og" {
		t.Errorf("name = %q, want tmux-og", name)
	}
	if _, ok := testKVGet(out, "path"); ok {
		t.Errorf("session payload carried a path field: %q", out)
	}
}

// The earlier positional format ("dir <path> <name>" split at the last
// space) broke on exactly this case: /home/x/My Docs -> My Docs. key=value
// must round-trip it because the value is everything after the first '='.
func TestEmitPayloadEncodeDirRoundTripsSpaces(t *testing.T) {
	p := emitPayload{kind: "dir", path: "/home/x/My Docs", name: "My Docs"}
	out, err := p.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if kind, _ := testKVGet(out, "kind"); kind != "dir" {
		t.Errorf("kind = %q, want dir", kind)
	}
	if path, _ := testKVGet(out, "path"); path != "/home/x/My Docs" {
		t.Errorf("path = %q, want '/home/x/My Docs'", path)
	}
	if name, _ := testKVGet(out, "name"); name != "My Docs" {
		t.Errorf("name = %q, want 'My Docs'", name)
	}
}

func TestEmitPayloadEncodeRejectsEmptyRequired(t *testing.T) {
	cases := []emitPayload{
		{kind: "session", name: ""},
		{kind: "", name: "x"},
		{kind: "dir", path: "", name: "x"},
		{kind: "dir", path: "/x", name: ""},
	}
	for _, p := range cases {
		if _, err := p.encode(); err == nil {
			t.Errorf("encode(%+v) = nil error, want rejection", p)
		}
	}
}

func TestEmitPayloadEncodeRejectsNUL(t *testing.T) {
	p := emitPayload{kind: "session", name: "bad\x00name"}
	if _, err := p.encode(); err == nil {
		t.Error("encode with embedded NUL should be rejected")
	}
}

// A newline in a value would forge a second key=value line. kv_get takes the
// last match, so an emitted `name` of "x\nkind=dir" would reach the wrapper as
// a *dir* pick — the one input that can change a pick's kind after the fact.
func TestEmitPayloadEncodeRejectsNewlines(t *testing.T) {
	for _, name := range []string{"x\nkind=dir", "x\rkind=dir", "trailing\n"} {
		p := emitPayload{kind: "session", name: name}
		if _, err := p.encode(); err == nil {
			t.Errorf("encode(name=%q) was accepted; a line break must be rejected", name)
		}
	}
}

func TestEmitPayloadEncodeRejectsOverlong(t *testing.T) {
	p := emitPayload{kind: "session", name: strings.Repeat("x", maxEmitFieldLen+1)}
	if _, err := p.encode(); err == nil {
		t.Error("encode with an overlong field should be rejected")
	}
}

// resolveEmitPick must be side-effect-free: it is a plain field mapping with
// no exec.Command call anywhere in its body, unlike the ordinary
// activateCurrent path (switch-client / createAndSwitch) this mode replaces.
func TestResolveEmitPickSessionRow(t *testing.T) {
	item := listItem{target: "tmux-og"}
	p, ok := resolveEmitPick(item)
	if !ok {
		t.Fatal("resolveEmitPick(session row) = false, want true")
	}
	if p.kind != "session" || p.name != "tmux-og" {
		t.Errorf("got %+v, want kind=session name=tmux-og", p)
	}
}

func TestResolveEmitPickZoxideRow(t *testing.T) {
	item := listItem{target: "/home/x/proj", createPath: "/home/x/proj", createName: "proj"}
	p, ok := resolveEmitPick(item)
	if !ok {
		t.Fatal("resolveEmitPick(zoxide row) = false, want true")
	}
	if p.kind != "dir" || p.path != "/home/x/proj" || p.name != "proj" {
		t.Errorf("got %+v, want kind=dir path=/home/x/proj name=proj", p)
	}
}

func TestResolveEmitPickUnselectableRow(t *testing.T) {
	// A header row: no target, no createPath — nothing to emit.
	if _, ok := resolveEmitPick(listItem{isHeader: true}); ok {
		t.Error("resolveEmitPick(header row) = true, want false")
	}
}

func TestWriteEmitPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "emit")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if err := writeEmitPayload(path, emitPayload{kind: "session", name: "tmux-og"}); err != nil {
		t.Fatalf("writeEmitPayload: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name, _ := testKVGet(string(data), "name"); name != "tmux-og" {
		t.Errorf("name = %q, want tmux-og", name)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestWriteEmitPayloadRejectsInvalidPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "emit")
	if err := writeEmitPayload(path, emitPayload{kind: "session", name: ""}); err == nil {
		t.Fatal("writeEmitPayload with an invalid payload should be rejected")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("writeEmitPayload must not create the file when the payload is invalid")
	}
}

func TestNoSessionRows(t *testing.T) {
	// buildSessionItems never omits its header row, so "no sessions" means
	// exactly one header row, not an empty slice.
	headerOnly := []listItem{{isHeader: true}}
	if !noSessionRows(headerOnly) {
		t.Error("noSessionRows(header only) = false, want true")
	}
	withSession := []listItem{{isHeader: true}, {target: "tmux-og"}}
	if noSessionRows(withSession) {
		t.Error("noSessionRows(with a session) = true, want false")
	}
	if !noSessionRows(nil) {
		t.Error("noSessionRows(nil) = false, want true")
	}
}

func TestRemotePickHost(t *testing.T) {
	cases := []struct {
		name       string
		item       listItem
		windowMode bool
		wantHost   string
		wantOK     bool
	}{
		{"session remote row", listItem{remoteHost: "tp-g6"}, false, "tp-g6", true},
		{"session remote row in window mode", listItem{remoteHost: "tp-g6"}, true, "tp-g6", true},
		{"mirror window row", listItem{target: "g6-main:1", bridgeHost: "tp-g6"}, true, "tp-g6", true},
		{"mirror row ignored in session mode", listItem{bridgeHost: "tp-g6"}, false, "", false},
		{"local window row", listItem{target: "tmux-og:1"}, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, ok := remotePickHost(tc.item, tc.windowMode)
			if ok != tc.wantOK || host != tc.wantHost {
				t.Errorf("remotePickHost() = %q, %v, want %q, %v", host, ok, tc.wantHost, tc.wantOK)
			}
		})
	}
}

func TestRemotePickNewPaneArgsHostWithoutQuotes(t *testing.T) {
	bin := "/nix/store/xxx-og-remote-pick/bin/og-remote-pick"
	host := "tp-g6"
	args := remotePickNewPaneArgs(bin, host)
	cmdStr := bin + " " + shellQuote(host)
	want := []string{
		"new-pane",
		"-x", "90%", "-y", "85%", "-X", "5%", "-Y", "8%", "-B", "heavy", "-A", "-O", "-K", "-C",
		cmdStr,
		";",
		"set", "-p", "@pane_label", "remote " + host,
		";",
		"set", "-p", "@float_geom", "90% 85% 5% 8%",
		";",
		"set", "-p", "remain-on-exit", "off", // inherited on inside a mirror (#587)
	}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// Semicolons must be their own argv token, or tmux would merge one into a
	// neighbouring word (e.g. "off ;") and silently misparse the command
	// chain. Located by the command string's value rather than a hardcoded
	// index, so the next flag added to remotePickNewPaneArgs can't silently
	// turn this into a no-op.
	cmdIdx := -1
	for i, a := range args {
		if a == cmdStr {
			cmdIdx = i
			break
		}
	}
	if cmdIdx == -1 {
		t.Fatalf("command string %q not found in args: %#v", cmdStr, args)
	}
	if args[cmdIdx+1] != ";" {
		t.Errorf("expected %q immediately after the command string, got %q at index %d: %#v", ";", args[cmdIdx+1], cmdIdx+1, args)
	}
}

// A host with an embedded quote must not break out of the shell-command
// string tmux hands to the pane's own shell.
func TestRemotePickNewPaneArgsHostWithEmbeddedQuote(t *testing.T) {
	args := remotePickNewPaneArgs("/bin/og-remote-pick", "o'brien")
	cmdStr := args[15] // after -x/-y/-X/-Y/-B heavy/-A/-O/-K/-C (#648)
	want := `/bin/og-remote-pick 'o'\''brien'`
	if cmdStr != want {
		t.Errorf("command string = %q, want %q", cmdStr, want)
	}
}

func TestEmitEmptyRowIsUnselectable(t *testing.T) {
	row := emitEmptyRow(map[string]string{})
	if row.target != "" || row.remoteHost != "" {
		t.Errorf("emitEmptyRow carries a target/remoteHost: %+v", row)
	}
	if row.isHeader {
		t.Error("emitEmptyRow must not be a header row, or pruneOrphanHeaders drops it when nothing follows")
	}
	if !strings.Contains(row.plain, "no sessions") {
		t.Errorf("plain = %q, want it to mention no sessions", row.plain)
	}
}
