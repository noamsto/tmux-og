package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

const placedTestSock = "/tmp/og-into-1|x.sock"

// placedTeardownConfig is a placed Config over recording fakes: the pin
// answers owned unless lost, and the host session holds a local window (@1),
// two of ours (@2, @4), another daemon's mirror (@3) and, once rebuilt, our
// placeholder (@9).
func placedTeardownConfig(log *callLog, lost bool) Config {
	return Config{
		LocalSess:     "work",
		LocalTarget:   "$1",
		Placed:        true,
		RemoteHost:    "h",
		RemoteSession: "api",
		SockPath:      placedTestSock,
		local:         testPin,
		LocalTmuxOut: func(args ...string) (string, error) {
			log.add(args)
			switch args[0] {
			case "display-message":
				if lost {
					return "100|\n", nil
				}
				return ownedAnswer, nil
			case "list-windows":
				return "@1||\n@2|1|" + placedTestSock + "\n@3|1|/tmp/og-into-2.sock\n@4|1|" + placedTestSock + "\n@9|1|" + placedTestSock + "\n", nil
			case "new-window":
				return "@9\n", nil
			}
			return "", nil
		},
		LocalTmux: func(args ...string) error {
			log.add(args)
			return nil
		},
	}
}

func killedWindows(calls [][]string) []string {
	var ids []string
	for _, c := range calls {
		if c[0] == "kill-window" {
			ids = append(ids, c[2])
		}
	}
	return ids
}

func TestKillOwnWindows(t *testing.T) {
	var log callLog
	killOwnWindows(placedTeardownConfig(&log, false))
	if got, want := killedWindows(log.snapshot()), []string{"@2", "@4", "@9"}; !slices.Equal(got, want) {
		t.Errorf("killed %v, want only our own %v", got, want)
	}
	if kill := findCall(log.snapshot(), "kill-session"); kill != nil {
		t.Errorf("killOwnWindows ran %v", kill)
	}
}

// A lost pin may mean a restarted local server whose window ids name
// unrelated windows: nothing is listed and nothing killed.
func TestKillOwnWindowsWithALostPinKillsNothing(t *testing.T) {
	var log callLog
	killOwnWindows(placedTeardownConfig(&log, true))
	for _, c := range log.snapshot() {
		if c[0] != "display-message" {
			t.Errorf("killOwnWindows over a lost pin ran %v", c)
		}
	}
}

// The placeholder is made and stamped in one call, before any kill, so a host
// session holding only our windows never empties mid-rebuild. It carries our
// identity but no @bridge_win, and survives the kill of every other own window.
func TestRebuildPlaceholder(t *testing.T) {
	var log callLog
	id, ok := rebuildPlaceholder(placedTeardownConfig(&log, false))
	if !ok || id != "@9" {
		t.Fatalf("rebuildPlaceholder = (%q, %v), want (@9, true)", id, ok)
	}
	calls := log.snapshot()
	const end = "$1:{end}"
	want := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-a", "-t", end, "--", "sleep", "2147483647",
		";", "set-option", "-w", "-t", end, "@bridge_sock", placedTestSock,
		";", "set-option", "-w", "-t", end, "@bridge_host", "h",
		";", "set-option", "-w", "-t", end, "@bridge_session", "api",
		";", "set-option", "-w", "-t", end, placedOption, "1"}
	nw := slices.IndexFunc(calls, func(c []string) bool { return c[0] == "new-window" })
	if nw < 0 || !reflect.DeepEqual(calls[nw], want) {
		t.Fatalf("new-window call = %q, want %q", findCall(calls, "new-window"), want)
	}
	if slices.ContainsFunc(calls[:nw], func(c []string) bool { return c[0] == "kill-window" }) {
		t.Errorf("a window was killed before the placeholder stood: %v", calls)
	}
	if got, want := killedWindows(calls), []string{"@2", "@4"}; !slices.Equal(got, want) {
		t.Errorf("killed %v, want the other own windows %v", got, want)
	}
}

func TestRebuildPlaceholderWithALostPinMakesNothing(t *testing.T) {
	var log callLog
	if id, ok := rebuildPlaceholder(placedTeardownConfig(&log, true)); ok || id != "" {
		t.Errorf("rebuildPlaceholder over a lost pin = (%q, %v), want (\"\", false)", id, ok)
	}
	for _, c := range log.snapshot() {
		if c[0] != "display-message" {
			t.Errorf("rebuildPlaceholder over a lost pin ran %v", c)
		}
	}
}

// A placed run that ends tears down its own windows and never the host
// session. The script ends at EOF after the remote reads, so the first
// window's setup fails and the run takes the plain teardown ending.
func TestPlacedRunMirrorTeardownKillsOnlyOwnWindows(t *testing.T) {
	// t.TempDir is too long a path for a unix socket on macOS.
	dir, err := os.MkdirTemp("", "ogp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var log callLog
	cfg := placedTeardownConfig(&log, false)
	cfg.SockPath = filepath.Join(dir, "sock")
	cfg.View = &Viewing{}
	cfg.LocalArea = func() (int, int) { return 0, 0 }
	listing := "@1||\n@7|1|" + cfg.SockPath + "\n@3|1|/tmp/og-into-2.sock\n"
	cfg.LocalTmuxOut = func(args ...string) (string, error) {
		log.add(args)
		switch args[0] {
		case "display-message":
			return ownedAnswer, nil
		case "list-windows":
			return listing, nil
		case "new-window":
			return "@7\n", nil
		}
		return "", nil
	}
	script := identityMatch +
		"%begin 1 2 1\n@1 1 1 main\n%end 1 2 1\n" +
		"%begin 1 3 1\nyes\n%end 1 3 1\n" +
		"%begin 1 4 1\n/remote/api\n%end 1 4 1\n"
	conn := newEOFScriptConn(withBarriers(script))
	cfg.Dial = func() (io.ReadWriteCloser, error) { return conn, nil }

	if err := runMirror(cfg); !errors.As(err, new(tornDown)) {
		t.Fatalf("runMirror over a script ending at EOF = %v, want a torn-down error", err)
	}
	calls := log.snapshot()
	if kill := findCall(calls, "kill-session"); kill != nil {
		t.Errorf("placed teardown ran %v", kill)
	}
	if got, want := killedWindows(calls), []string{"@7"}; !slices.Equal(got, want) {
		t.Errorf("placed teardown killed %v, want only our own %v", got, want)
	}
}
