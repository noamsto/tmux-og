package daemon

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/tmux-og/picker/remotebridge/wire"
)

// TestPumpInputDismissesDeadKeyPaneLiveTmux drives pumpInput's guard against a
// real control client: a dead tiled pane and a dead float, both
// remain-on-exit key, must be killed by an ordinary key typed into their
// mirror; a live pane and a dead remain-on-exit-on pane must not be.
func TestPumpInputDismissesDeadKeyPaneLiveTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		// OG_REQUIRE_TMUX is set by pickerChecked's checkPhase in flake.nix,
		// which also puts tmux in nativeBuildInputs — so a missing tmux there
		// means that input was pruned, not that this is a dev machine.
		if os.Getenv("OG_REQUIRE_TMUX") != "" {
			t.Fatal("tmux is required (OG_REQUIRE_TMUX set) but not on PATH — check pickerChecked's nativeBuildInputs in flake.nix")
		}
		t.Skip("tmux is not available")
	}
	tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())

	if out, err := tmux("set-option", "-w", "-t", "w", "remain-on-exit", "on").CombinedOutput(); err != nil {
		t.Fatalf("set remain-on-exit: %v\n%s", err, out)
	}

	live := newPane(t, tmux, "split-window", "-d", "-k", "-P", "-F", "#{pane_id}", "-t", "w", "cat")
	deadKeyTiled := newPane(t, tmux, "split-window", "-d", "-k", "-P", "-F", "#{pane_id}", "-t", "w", "true")
	deadKeyFloat := newPane(t, tmux, "new-pane", "-d", "-k", "-P", "-F", "#{pane_id}", "-t", "w",
		"-x", "20", "-y", "5", "-X", "2", "-Y", "2", "true")
	deadOn := newPane(t, tmux, "split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "w", "true")

	for _, id := range []string{deadKeyTiled, deadKeyFloat, deadOn} {
		waitPaneDead(t, tmux, id)
	}

	ctl := tmux("-C", "attach-session", "-t", "w")
	stdin, err := ctl.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := ctl.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.Start(); err != nil {
		t.Fatalf("control client: %v", err)
	}
	t.Cleanup(func() {
		ctl.Process.Kill()
		ctl.Wait()
	})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
		}
	}()

	var mu sync.Mutex
	send := func(cmd string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(stdin, cmd)
	}

	peers := make(map[string]net.Conn, 4)
	for _, id := range []string{live, deadKeyTiled, deadKeyFloat, deadOn} {
		conn, peer := net.Pipe()
		t.Cleanup(func() {
			conn.Close()
			peer.Close()
		})
		peers[id] = peer
		go pumpInput(conn, id, send, nil, nil, nil)
	}

	for _, id := range []string{live, deadKeyTiled, deadKeyFloat, deadOn} {
		peer := peers[id]
		peer.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
			t.Fatalf("write input to %s: %v", id, err)
		}
	}

	// Barrier: on a net.Pipe this write returns only once pumpInput has sent
	// the first frame's guard and send-keys and looped back to ReadFrame, so
	// the survivors' first-frame commands are already queued on the control
	// connection before the assertions below run.
	for _, id := range []string{live, deadOn} {
		peer := peers[id]
		peer.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := wire.WriteFrame(peer, wire.FrameInput, []byte("x")); err != nil {
			t.Fatalf("write second input to %s: %v", id, err)
		}
	}

	// Prove every guard/send-keys queued above has already run: a control
	// client executes commands in the order it receives them, so once the
	// server answers this wait-for, sent last on the same connection, every
	// earlier command is done.
	send("wait-for -S og748")
	waitCmd := tmux("wait-for", "og748")
	if err := waitCmd.Start(); err != nil {
		t.Fatalf("wait-for og748: start: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- waitCmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("wait-for og748: %v", err)
		}
	case <-time.After(5 * time.Second):
		waitCmd.Process.Kill()
		t.Fatal("wait-for og748 timed out — a queued command never reached the server")
	}

	deadline := time.Now().Add(5 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		ids = listPaneIDs(t, tmux)
		if !slices.Contains(ids, deadKeyTiled) && !slices.Contains(ids, deadKeyFloat) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if slices.Contains(ids, deadKeyTiled) {
		t.Errorf("dead tiled key pane %s survived: %q", deadKeyTiled, ids)
	}
	if slices.Contains(ids, deadKeyFloat) {
		t.Errorf("dead float key pane %s survived: %q", deadKeyFloat, ids)
	}

	deadline = time.Now().Add(5 * time.Second)
	var capture string
	for time.Now().Before(deadline) {
		out, err := tmux("capture-pane", "-p", "-t", live).Output()
		if err == nil && strings.Contains(string(out), "x") {
			capture = string(out)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(capture, "x") {
		t.Errorf("live pane capture = %q, want it to contain the echoed key", capture)
	}

	ids = listPaneIDs(t, tmux)
	if !slices.Contains(ids, live) {
		t.Errorf("live pane %s missing after key input: %q", live, ids)
	}
	if !slices.Contains(ids, deadOn) {
		t.Errorf("dead remain-on-exit-on pane %s missing after key input: %q", deadOn, ids)
	}
}

func newPane(t *testing.T, tmux func(args ...string) *exec.Cmd, args ...string) string {
	t.Helper()
	out, err := tmux(args...).Output()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func waitPaneDead(t *testing.T, tmux func(args ...string) *exec.Cmd, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := tmux("display-message", "-p", "-t", id, "#{pane_dead}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s never reported pane_dead=1", id)
}

func listPaneIDs(t *testing.T, tmux func(args ...string) *exec.Cmd) []string {
	t.Helper()
	out, err := tmux("list-panes", "-s", "-t", "w", "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("list-panes: %v", err)
	}
	return strings.Fields(string(out))
}
