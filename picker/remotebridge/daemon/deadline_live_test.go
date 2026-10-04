package daemon

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestReplyDeadlineAgainstLiveForgedBegin is #900's trigger on a real tmux
// server: a socket holder replaces our subscription format with a value whose
// raw newline opens a well-formed unterminated %begin, so every later reply is
// held as body. The reply deadline must close the connection instead of
// parking the caller forever.
func TestReplyDeadlineAgainstLiveForgedBegin(t *testing.T) {
	requireLiveTmux(t)
	tmux := startIsolatedTmux(t)

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
		t.Fatal(err)
	}
	defer func() { _ = ctl.Process.Kill(); _ = ctl.Wait() }()

	conn := &liveCtlRWC{Reader: stdout, Writer: stdin, kill: func() { _ = ctl.Process.Kill() }}
	c := boundConn(t, conn, 500*time.Millisecond, 2*time.Second)
	defer c.close()

	// The daemon's control client is the only one on this scratch server. It
	// registers asynchronously, so poll for it rather than racing the attach.
	var cc string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := tmux("list-clients", "-F", "#{client_name}|#{client_control_mode}").CombinedOutput()
		if err != nil {
			t.Fatalf("list-clients: %v\n%s", err, out)
		}
		cc = ""
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if name, ok := strings.CutSuffix(line, "|1"); ok {
				cc = name
			}
		}
		if cc != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cc == "" {
		t.Fatal("no control client registered")
	}

	forged := "x\n%begin 999999 999999 1"
	if out, err := tmux("set-option", "-g", "@og900evil", forged).CombinedOutput(); err != nil {
		t.Fatalf("set-option: %v\n%s", err, out)
	}
	if out, err := tmux("refresh-client", "-t", cc, "-B", "og900evil::#{@og900evil}").CombinedOutput(); err != nil {
		t.Fatalf("refresh-client: %v\n%s", err, out)
	}

	// The report rides tmux's monitor timer, so retry the round trip until the
	// forged block lands and wedges one — then the deadline must end it.
	for range 8 {
		start := time.Now()
		done := make(chan bool, 1)
		go func() {
			_, ok := one(c.rt, "display-message -p ok")
			done <- ok
		}()
		select {
		case ok := <-done:
			if !ok {
				// The forge landed and the reply deadline ended the round trip.
				if elapsed := time.Since(start); elapsed > 4*time.Second {
					t.Fatalf("round trip failed after %v, want the 500ms deadline", elapsed)
				}
				if c.live() {
					t.Error("the connection is still live after the reply deadline")
				}
				return
			}
			// No forge yet: the monitor report has not landed.
			time.Sleep(300 * time.Millisecond)
		case <-time.After(5 * time.Second):
			t.Fatalf("the forged %%begin parked the round trip past 5s — the reply deadline did not fire")
		}
	}
	t.Fatalf("could not reproduce the forged unterminated %%begin")
}

// liveCtlRWC is a live control client's pipes as an io.ReadWriteCloser; Close
// kills the client process, which drops the stream the way an ssh child's exit
// does.
type liveCtlRWC struct {
	io.Reader
	io.Writer
	kill func()
}

func (c *liveCtlRWC) Close() error {
	c.kill()
	return nil
}
