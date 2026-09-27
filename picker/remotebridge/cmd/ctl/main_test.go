package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

func TestRunReportsUnreachableDaemon(t *testing.T) {
	err := run(filepath.Join(t.TempDir(), "missing.sock"), []string{"split-h", "%1"})
	if err == nil || !strings.Contains(err.Error(), "bridge daemon unreachable") {
		t.Fatalf("run() error = %v, want unreachable daemon", err)
	}
}

func TestRunReportsDaemonErrorAck(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_, err = wire.ReadFrame(conn)
		}
		if err == nil {
			err = wire.WriteFrame(conn, wire.FrameCtlAck, []byte("carousel: pane %99 is not mirrored by this bridge"))
		}
		done <- err
	}()

	err = run(sock, []string{"split-h", "%1"})
	if err == nil || err.Error() != "carousel: pane %99 is not mirrored by this bridge" {
		t.Fatalf("run() error = %v, want daemon ack", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestShowErrorPassesExactCtlErrorToTmux(t *testing.T) {
	original := runTmux
	t.Cleanup(func() { runTmux = original })
	var got []string
	runTmux = func(args ...string) error {
		got = args
		return nil
	}

	err := errors.New("carousel: pane %99 is not mirrored by this bridge")
	if err := showError("/dev/pts/1", err); err != nil {
		t.Fatal(err)
	}
	want := []string{"display-message", "-d", "5000", "-t", "/dev/pts/1", errorPrefix + err.Error()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tmux argv = %#v, want %#v", got, want)
	}
}

func TestResolveFloatDrag(t *testing.T) {
	wantFormat := "#{@bridge_pane}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{window_width}|#{window_height}"

	stub := func(t *testing.T, reply string, err error) *[]string {
		t.Helper()
		original := runTmuxOut
		t.Cleanup(func() { runTmuxOut = original })
		var got []string
		runTmuxOut = func(args ...string) (string, error) {
			got = args
			return reply, err
		}
		return &got
	}

	t.Run("resolves a floating bridge pane", func(t *testing.T) {
		got := stub(t, "%3|1|11|6|38|10|100|30\n", nil)
		args, err := resolveFloatDrag("%7")
		if err != nil {
			t.Fatalf("resolveFloatDrag() error = %v", err)
		}
		want := []string{"display-message", "-p", "-t", "%7", wantFormat}
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("tmux argv = %#v, want %#v", *got, want)
		}
		wantArgs := []string{"float-geom", "%3", "%7", "11", "6", "38", "10", "100", "30"}
		if !reflect.DeepEqual(args, wantArgs) {
			t.Fatalf("args = %#v, want %#v", args, wantArgs)
		}
	})

	t.Run("refuses a non-pane-id argument without calling tmux", func(t *testing.T) {
		called := false
		original := runTmuxOut
		t.Cleanup(func() { runTmuxOut = original })
		runTmuxOut = func(args ...string) (string, error) {
			called = true
			return "", nil
		}
		if _, err := resolveFloatDrag("7"); err == nil {
			t.Fatal("resolveFloatDrag(\"7\") error = nil, want refusal")
		}
		if called {
			t.Fatal("runTmuxOut called for a non-pane-id argument")
		}
	})

	t.Run("refuses an argument with shell metacharacters without calling tmux", func(t *testing.T) {
		called := false
		original := runTmuxOut
		t.Cleanup(func() { runTmuxOut = original })
		runTmuxOut = func(args ...string) (string, error) {
			called = true
			return "", nil
		}
		if _, err := resolveFloatDrag("%7;x"); err == nil {
			t.Fatal("resolveFloatDrag(\"%7;x\") error = nil, want refusal")
		}
		if called {
			t.Fatal("runTmuxOut called for %7;x")
		}
	})

	t.Run("refuses a tmux error", func(t *testing.T) {
		stub(t, "", errors.New("can't find pane %7"))
		if _, err := resolveFloatDrag("%7"); err == nil {
			t.Fatal("resolveFloatDrag() error = nil, want tmux error surfaced")
		}
	})

	t.Run("refuses an empty @bridge_pane", func(t *testing.T) {
		stub(t, "|1|11|6|38|10|100|30\n", nil)
		if _, err := resolveFloatDrag("%7"); err == nil {
			t.Fatal("resolveFloatDrag() error = nil, want refusal for empty @bridge_pane")
		}
	})

	t.Run("refuses a non-floating pane", func(t *testing.T) {
		stub(t, "%3|0|11|6|38|10|100|30\n", nil)
		if _, err := resolveFloatDrag("%7"); err == nil {
			t.Fatal("resolveFloatDrag() error = nil, want refusal for pane_floating_flag=0")
		}
	})

	t.Run("refuses a malformed @bridge_pane", func(t *testing.T) {
		stub(t, "%3;x|1|11|6|38|10|100|30\n", nil)
		if _, err := resolveFloatDrag("%7"); err == nil {
			t.Fatal("resolveFloatDrag() error = nil, want refusal for malformed @bridge_pane")
		}
	})

	t.Run("refuses a reply with the wrong field count", func(t *testing.T) {
		stub(t, "%3|1|11|6|38|10|100\n", nil)
		if _, err := resolveFloatDrag("%7"); err == nil {
			t.Fatal("resolveFloatDrag() error = nil, want refusal for wrong field count")
		}
	})
}

func TestShowErrorWithTmuxServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not available")
	}

	tmpdir, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpdir) })
	const socket = "s"
	env := append(os.Environ(), "TMUX_TMPDIR="+tmpdir)

	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "ctl-error", "sleep 30")
	start.Env = env
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command("tmux", "-L", socket, "kill-server")
		stop.Env = env
		_ = stop.Run()
	})

	original := runTmux
	t.Cleanup(func() { runTmux = original })
	runTmux = func(args ...string) error {
		cmd := exec.Command("tmux", append([]string{"-L", socket}, args...)...)
		cmd.Env = env
		return cmd.Run()
	}

	want := "bridge daemon unreachable: connection refused"
	if err := showError("ctl-error:0.0", errors.New(want)); err != nil {
		t.Fatalf("show error through tmux: %v", err)
	}
	show := exec.Command("tmux", "-L", socket, "show-messages", "-t", "ctl-error:0.0")
	show.Env = env
	out, err := show.CombinedOutput()
	if err != nil {
		t.Fatalf("show tmux messages: %v: %s", err, out)
	}
	if !strings.Contains(string(out), want) {
		t.Fatalf("tmux messages = %q, want %q", out, want)
	}
}
