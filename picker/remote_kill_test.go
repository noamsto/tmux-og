package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyKillErr(t *testing.T) {
	exitErr := func(code int) error {
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}

	cases := []struct {
		name     string
		err      error
		stdout   string
		stderr   string
		timedOut bool
		want     error
	}{
		// A non-255 exit is the remote tmux command's own failure. Exit 1 is a
		// session already gone; a command that could not run (126/127) or any
		// other uncertain non-zero code keeps the row.
		{"exit 1 is a gone session", exitErr(1), "", "can't find session: mono", false, errRemoteSessionGone},
		{"exit 1, no server running, is gone too", exitErr(1), "", "no server running on /tmp/tmux-1000/default", false, errRemoteSessionGone},
		{"exit 127 (tmux missing)", exitErr(127), "", "", false, errRemoteKillUnrunnable},
		{"exit 126 (tmux not executable)", exitErr(126), "", "", false, errRemoteKillUnrunnable},
		{"exit 2 (uncertain tmux failure) keeps the row", exitErr(2), "", "", false, errRemoteKillUnrunnable},
		{"timeout beats the exit status", exitErr(1), "", "", true, errRemoteUnreachable},
		{"ssh binary missing", errors.New(`exec: "ssh": not found`), "", "", false, errRemoteUnreachable},

		// 255 keeps the probe classifier's host-level states.
		{"bare 255", exitErr(255), "", "", false, errRemoteUnreachable},
		{"unknown host key", exitErr(255), "", "Host key verification failed.", false, errRemoteNeedsAuth},
		{"password refused", exitErr(255), "", "noams@mbp: Permission denied (publickey,password).", false, errRemoteNeedsAuth},
		{"refused", exitErr(255), "", "ssh: connect to host lab port 22: Connection refused", false, errRemoteUnreachable},
		{
			"host key changed",
			exitErr(255), "", "@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\nHost key verification failed.", false,
			errRemoteHostKeyChanged,
		},
		{
			"tailscale check banner",
			exitErr(255), "Tailscale SSH requires an additional check.\n", "", true,
			errRemoteTailscaleCheck,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyKillErr(tc.err, tc.stdout, tc.stderr, tc.timedOut)
			if !errors.Is(got, tc.want) {
				t.Errorf("classifyKillErr(%v, %q, %q, %v) = %v, want %v", tc.err, tc.stdout, tc.stderr, tc.timedOut, got, tc.want)
			}
		})
	}
}

// A remote-controlled session name must never escape shellQuote: the remote
// login shell sees one single-quoted literal, so no second command can ride in.
func TestRemoteKillSessionBodyQuotesHostileName(t *testing.T) {
	hostile := `x'; rm -rf / #`
	body := remoteKillSessionBody(hostile)
	// shellQuote turns `x'; rm -rf / #` into `'x'\''; rm -rf / #'`, so the whole
	// hostile name is one quoted word.
	want := `'=` + `x'\''` + `; rm -rf / #'`
	if !strings.Contains(body, want) {
		t.Errorf("body = %q, want it to contain the quoted literal %q", body, want)
	}
}

// remoteKillSessionBody stays fish-safe like the probe: the remote login shell
// may be fish, which rejects a bare `var=value` script assignment (the
// `env TMUX_TMPDIR=...` command prefix is fine).
func TestRemoteKillSessionBodyFishSafe(t *testing.T) {
	body := remoteKillSessionBody("mono")
	if !strings.Contains(body, "env TMUX_TMPDIR=") {
		t.Fatalf("body = %q, want TMUX_TMPDIR set via env(1)", body)
	}
	if !strings.Contains(body, "kill-session -t '=mono'") {
		t.Fatalf("body = %q, want an exact-match, quoted kill-session", body)
	}
	if strings.Contains(body, "td=") || strings.Contains(body, "; t=") {
		t.Fatalf("body = %q must not use shell assignments (fish-incompatible)", body)
	}
}

// scratchTmux is a private tmux server (own TMUX_TMPDIR and -L socket) with a
// fake ssh on PATH that runs the command it is handed against it, never the
// live server. A tmux shim on PATH forces the scratch TMUX_TMPDIR at the tmux
// invocation, so the command's literal /run/user/$(id -u) and /tmp legs (whose
// first value contains a space) need no string rewriting.
type scratchTmux struct {
	t        *testing.T
	realTmux string
	sock     string
	env      []string
	sentinel string
}

func newScratchTmux(t *testing.T, sock string) *scratchTmux {
	t.Helper()
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	scratch, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	s := &scratchTmux{t: t, realTmux: realTmux, sock: sock, env: append(os.Environ(), "TMUX_TMPDIR="+scratch)}
	t.Cleanup(func() { _, _ = s.tmux("kill-server") })

	shimDir := t.TempDir()
	s.sentinel = filepath.Join(t.TempDir(), "shim-ran")
	shim := "#!/bin/sh\nprintf ran >>" + s.sentinel + "\nexec env TMUX_TMPDIR=" + scratch + " " + realTmux + " -L " + sock + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	sshDir := t.TempDir()
	fakeSSH := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -o) shift 2 ;;\n" +
		"    -T) shift ;;\n" +
		"    --) shift; break ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"export PATH=\"" + shimDir + ":$PATH\"\n" +
		"exec bash -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+":"+os.Getenv("PATH"))
	return s
}

// tmux runs the real tmux against the scratch server.
func (s *scratchTmux) tmux(args ...string) (string, error) {
	cmd := exec.Command(s.realTmux, append([]string{"-L", s.sock, "-f", "/dev/null"}, args...)...)
	cmd.Env = s.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *scratchTmux) mustTmux(args ...string) {
	s.t.Helper()
	if out, err := s.tmux(args...); err != nil {
		s.t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
}

func (s *scratchTmux) requireShimRan() {
	s.t.Helper()
	if _, err := os.Stat(s.sentinel); err != nil {
		s.t.Fatalf("the tmux shim never ran — fake ssh did not shadow PATH: %v", err)
	}
}

// TestKillRemoteSessionReachesScratchServer proves the real ssh command
// construction kills a session on a private scratch tmux server, never the
// live one.
func TestKillRemoteSessionReachesScratchServer(t *testing.T) {
	// Unique names: if the shim ever fails to shadow PATH, the surviving real
	// session can never be a user's.
	victim := fmt.Sprintf("lz-victim-%d", os.Getpid())
	keep := fmt.Sprintf("lz-keep-%d", os.Getpid())
	s := newScratchTmux(t, "lzkill")
	s.mustTmux("new-session", "-d", "-s", victim)
	s.mustTmux("new-session", "-d", "-s", keep)

	if err := sshKillRemoteSession("lab", victim); err != nil {
		t.Fatalf("sshKillRemoteSession: %v", err)
	}
	s.requireShimRan()
	sessions, _ := s.tmux("list-sessions", "-F", "#{session_name}")
	if strings.Contains(sessions, victim) {
		t.Errorf("victim %q survived the kill: %s", victim, sessions)
	}
	if !strings.Contains(sessions, keep) {
		t.Errorf("keep %q was killed too: %s", keep, sessions)
	}
}

func TestRemoteKillWindowBodyTargetsIDsOnly(t *testing.T) {
	body := remoteKillWindowBody("$3", "@4")
	if !strings.Contains(body, "env TMUX_TMPDIR=") || !strings.Contains(body, "kill-window -t '$3:@4'") {
		t.Fatalf("body = %q, want a quoted id-only kill-window under env(1)", body)
	}
	if strings.Contains(body, "td=") || strings.Contains(body, "; t=") {
		t.Fatalf("body = %q must not use shell assignments (fish-incompatible)", body)
	}
}

// TestKillRemoteWindowReachesScratchServer proves kill-window -t '$<sid>:@id'
// kills exactly that window, and that an id not linked into the named session
// is the gone class and kills nothing.
func TestKillRemoteWindowReachesScratchServer(t *testing.T) {
	pid := os.Getpid()
	a, b := fmt.Sprintf("lz-a-%d", pid), fmt.Sprintf("lz-b-%d", pid)
	s := newScratchTmux(t, "lzkillwin")
	s.mustTmux("new-session", "-d", "-s", a)
	s.mustTmux("new-session", "-d", "-s", b)
	s.mustTmux("new-window", "-d", "-t", "="+b+":")
	listWindows := func() string {
		out, _ := s.tmux("list-windows", "-a", "-F", "#{session_name}|#{window_id}")
		return out
	}
	sessionID := func(name string) string {
		out, _ := s.tmux("display-message", "-p", "-t", "="+name+":", "#{session_id}")
		return strings.TrimSpace(out)
	}
	aID, bID := sessionID(a), sessionID(b)
	before := listWindows()
	if want := b + "|@2"; !strings.Contains(before, want) {
		t.Fatalf("setup: windows = %q, want %q", before, want)
	}

	err := sshKillRemoteWindowCtx(context.Background(), "lab", aID, "@2")
	if !errors.Is(err, errRemoteSessionGone) {
		t.Fatalf("kill of @2 under %s = %v, want the gone class", a, err)
	}
	s.requireShimRan()
	if got := listWindows(); got != before {
		t.Errorf("a wrong-session kill changed the windows: before %q after %q", before, got)
	}

	if err := sshKillRemoteWindowCtx(context.Background(), "lab", bID, "@2"); err != nil {
		t.Fatalf("kill of @2 under %s: %v", b, err)
	}
	after := listWindows()
	if strings.Contains(after, b+"|@2") {
		t.Errorf("@2 survived: %q", after)
	}
	if !strings.Contains(after, a+"|@0") || !strings.Contains(after, b+"|@1") {
		t.Errorf("a sibling window was killed too: %q", after)
	}
}

// TestSSHKillRemoteSessionCtxCancelReapsProcessGroup proves a cancel doesn't
// just kill ssh itself: without Setsid + a process-group kill, a shell child
// that hasn't been exec'd into (a real ssh ProxyCommand, or here a fake ssh
// that forks a subcommand rather than exec'ing it) survives as an orphan,
// still holding the stdout/stderr pipes cmd.Wait needs to see EOF on (#781).
func TestSSHKillRemoteSessionCtxCancelReapsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	shimDir := t.TempDir()
	marker := filepath.Join(dir, "started")
	// The grandchild is a uniquely-pathed script (rather than a bare "sleep")
	// so pgrep can identify it precisely on a shared machine — it must not
	// exec into sleep itself, or its argv (and pgrep match) would become
	// indistinguishable "sleep 5" the moment it starts.
	child := filepath.Join(shimDir, "slow-grandchild")
	if err := os.WriteFile(child, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT `exec`-ing the grandchild: a plain invocation forks a
	// child of this shell rather than replacing it, so a kill of the shell
	// alone leaves it running — the shape a real ssh ProxyCommand child takes.
	shim := "#!/bin/sh\ntouch " + marker + "\n" + child + "\n"
	if err := os.WriteFile(filepath.Join(shimDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", shimDir+":"+oldPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sshKillRemoteSessionCtx(ctx, "lab", "mono") }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shim never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sshKillRemoteSessionCtx did not return after cancel")
	}

	// The orphaned grandchild survives well past killWaitDelay if it isn't
	// reaped as part of ssh's process group.
	time.Sleep(300 * time.Millisecond)
	out, err := exec.Command("pgrep", "-f", child).CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("orphaned process survived cancel: %s", out)
	}
}
