package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The sock is the last field because a socket path may itself hold '|': only
// the first two delimiters split a row.
func TestParseHostWindows(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []hostWindow
	}{
		{"a sock holding the delimiter stays whole", "@3|1|/tmp/a|b.sock\n", []hostWindow{{id: "@3", sock: "/tmp/a|b.sock", placed: true}}},
		{"an empty placed field is not placed", "@4||/tmp/s.sock\n", []hostWindow{{id: "@4", sock: "/tmp/s.sock"}}},
		{"a local window carries neither", "@5||\n", []hostWindow{{id: "@5"}}},
		{"a blank line is skipped", "@1|1|/s\n\n@2||\n", []hostWindow{{id: "@1", sock: "/s", placed: true}, {id: "@2"}}},
		{"a short row is skipped", "@1|1\n", nil},
		{"no output", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHostWindows(tc.out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseHostWindows(%q) = %+v, want %+v", tc.out, got, tc.want)
			}
		})
	}
}

// Own windows are the ones carrying this daemon's socket, placed or not: a
// local window and another daemon's mirror in the same host session are never
// ours.
func TestOwnWindows(t *testing.T) {
	var argv []string
	cfg := Config{
		LocalSess:   "work",
		LocalTarget: "$4",
		Placed:      true,
		SockPath:    "/tmp/og-into-1.sock",
		LocalTmuxOut: func(args ...string) (string, error) {
			argv = args
			return "@1||\n@2|1|/tmp/og-into-1.sock\n@3|1|/tmp/og-into-2.sock\n@4||/tmp/og-into-1.sock\n", nil
		},
	}
	got, ok := ownWindows(cfg)
	if !ok || !reflect.DeepEqual(got, []string{"@2", "@4"}) {
		t.Errorf("ownWindows = (%v, %v), want ([@2 @4], true)", got, ok)
	}
	if want := []string{"list-windows", "-t", "$4", "-F", hostWindowsFormat}; !reflect.DeepEqual(argv, want) {
		t.Errorf("ownWindows asked %v, want %v", argv, want)
	}

	cfg.LocalTmuxOut = func(...string) (string, error) { return "@2|1|/tmp/og-into-1.sock\n", errors.New("boom") }
	if got, ok := ownWindows(cfg); ok || got != nil {
		t.Errorf("ownWindows over a failed list-windows = (%v, %v), want (nil, false)", got, ok)
	}
	if got, ok := ownWindows(Config{LocalTarget: "$4"}); ok || got != nil {
		t.Errorf("ownWindows with no LocalTmuxOut = (%v, %v), want (nil, false)", got, ok)
	}
}

// Every local command naming the host session targets it by id in placed
// mode: the name is the user's to rename, and a bare name resolves by prefix.
func TestPlacedTargetsTheHostSessionByID(t *testing.T) {
	var asked [][]string
	cfg := Config{
		LocalSess:   "work",
		LocalTarget: "$4",
		Placed:      true,
		LocalTmux:   func(...string) error { return nil },
		LocalTmuxOut: func(args ...string) (string, error) {
			asked = append(asked, args)
			return "", nil
		},
	}
	localWindowSet(cfg)
	mirrorPaneRows(cfg)
	notifyLocal(cfg, "hi")
	_, _ = createMirrorWindow(cfg)
	want := [][]string{
		{"list-windows", "-t", "$4"},
		{"list-panes", "-s", "-t", "$4"},
		{"list-clients", "-t", "$4"},
		{"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", "$4:{end}"},
	}
	if len(asked) != len(want) {
		t.Fatalf("asked %q, want %d calls", asked, len(want))
	}
	for i, w := range want {
		if got := asked[i][:len(w)]; !reflect.DeepEqual(got, w) {
			t.Errorf("call %d = %q, want prefix %q", i, asked[i], w)
		}
	}
}

// A placed run writes no session-scoped identity: a session-level reader of
// the shared host session would take it for a mirror session, and the
// launcher's pair lookup would then kill the user's session. Dedicated mode
// keeps its session stamps. The script ends at EOF after the remote reads, so
// the run fails out of its post-listen setup once the stamps are behind it.
func TestRunMirrorSessionStampsAreDedicatedOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		placed bool
	}{
		{"dedicated", false},
		{"placed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// t.TempDir is too long a path for a unix socket on macOS.
			dir, err := os.MkdirTemp("", "ogp")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			var calls callLog
			cfg := Config{
				LocalSess:     "work",
				RemoteHost:    "h",
				RemoteSession: "api",
				SockPath:      filepath.Join(dir, "sock"),
				local:         testPin,
				View:          &Viewing{},
				LocalArea:     func() (int, int) { return 0, 0 },
				LocalTmuxOut:  func(...string) (string, error) { return ownedAnswer, nil },
				LocalTmux: func(args ...string) error {
					calls.add(args)
					return nil
				},
			}
			if tc.placed {
				cfg.Placed, cfg.LocalTarget = true, "$1"
			}
			script := identityMatch +
				"%begin 1 2 1\n@1 1 1 main\n%end 1 2 1\n" +
				"%begin 1 3 1\nyes\n%end 1 3 1\n" +
				"%begin 1 4 1\n/remote/api\n%end 1 4 1\n"
			conn := newEOFScriptConn(withBarriers(script))
			cfg.Dial = func() (io.ReadWriteCloser, error) { return conn, nil }

			if err := runMirror(cfg); err == nil {
				t.Fatal("runMirror over a script ending at EOF returned nil")
			}

			stamped := map[string]bool{}
			for _, c := range calls.snapshot() {
				if len(c) == 5 && c[0] == "set-option" && c[1] == "-t" && (c[2] == "work" || c[2] == "$1") {
					stamped[c[3]] = true
				}
			}
			for _, opt := range []string{"@bridge_sock", "@bridge_host", "@bridge_session"} {
				if stamped[opt] == tc.placed {
					t.Errorf("session-scoped %s written = %v, want %v", opt, stamped[opt], !tc.placed)
				}
			}
		})
	}
}

// A dedicated session is all ours, so it follows without a read. A placed one
// follows only while its current window carries our sock.
func TestHostCurrentIsOwn(t *testing.T) {
	const sock = "/tmp/og-into-1|x.sock"
	var argv []string
	for _, tc := range []struct {
		name   string
		placed bool
		reply  string
		err    error
		want   bool
	}{
		{"dedicated never reads", false, "", errors.New("must not ask"), true},
		{"placed on an own window", true, sock + "\n", nil, true},
		{"placed on another daemon's mirror", true, "/tmp/og-into-2.sock\n", nil, false},
		{"placed on a local window", true, "\n", nil, false},
		{"placed read fails", true, sock + "\n", errors.New("no server"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv = nil
			cfg := Config{
				LocalSess: "work",
				SockPath:  sock,
				LocalTmuxOut: func(args ...string) (string, error) {
					argv = args
					return tc.reply, tc.err
				},
			}
			if tc.placed {
				cfg.Placed, cfg.LocalTarget = true, "$4"
			}
			if got := hostCurrentIsOwn(cfg); got != tc.want {
				t.Errorf("hostCurrentIsOwn = %v, want %v", got, tc.want)
			}
			if !tc.placed {
				if argv != nil {
					t.Errorf("dedicated asked %q, want no read", argv)
				}
				return
			}
			if want := []string{"display-message", "-p", "-t", "$4", "#{@bridge_sock}"}; !reflect.DeepEqual(argv, want) {
				t.Errorf("asked %q, want %q", argv, want)
			}
		})
	}
}

// A selector matching no window of the remote session fails startup before the
// listener exists, so the launcher's listen wait reports it, and nothing is
// mirrored into the host session.
func TestRunMirrorFailsWhenNoWindowMatches(t *testing.T) {
	// t.TempDir is too long a path for a unix socket on macOS.
	dir, err := os.MkdirTemp("", "ogp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var calls callLog
	cfg := selectorCfg(t, Config{
		LocalSess:     "work",
		RemoteHost:    "h",
		RemoteSession: "api",
		SockPath:      filepath.Join(dir, "sock"),
		Placed:        true,
		LocalTarget:   "$1",
		local:         testPin,
		View:          &Viewing{},
		LocalArea:     func() (int, int) { return 0, 0 },
		LocalTmuxOut: func(args ...string) (string, error) {
			calls.add(args)
			return ownedAnswer, nil
		},
		LocalTmux: func(args ...string) error {
			calls.add(args)
			return nil
		},
	}, "@crew=x")
	script := identityMatch + "%begin 1 2 1\n1 @1 1 0 main\n2 @2 0 x other\n%end 1 2 1\n"
	conn := newEOFScriptConn(withBarriers(script))
	cfg.Dial = func() (io.ReadWriteCloser, error) { return conn, nil }

	err = runMirror(cfg)
	if want := "daemon: no window of api matches @crew=x"; err == nil || err.Error() != want {
		t.Fatalf("runMirror(...) = %v, want %q", err, want)
	}
	if _, err := os.Stat(cfg.SockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("sock stat = %v, want no listener", err)
	}
	if nw := findCall(calls.snapshot(), "new-window"); nw != nil {
		t.Errorf("a window was created: %v", nw)
	}
	if !conn.isClosed() {
		t.Error("the connection was left open")
	}
}
