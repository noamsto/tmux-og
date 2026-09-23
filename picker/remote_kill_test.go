package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		// A non-255 exit is the remote tmux command's own failure — a session
		// that is already gone, not an unreachable host.
		{"exit 1 is a gone session", exitErr(1), "", "can't find session: mono", false, errRemoteSessionGone},
		{"exit 127 (tmux missing) still the command's own", exitErr(127), "", "", false, errRemoteSessionGone},
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

// TestKillRemoteSessionReachesScratchServer proves the real ssh command
// construction kills a session on a private scratch tmux server, never the
// live one. The fake ssh executes the command it is handed against a scratch
// server; a tmux shim on PATH forces the scratch TMUX_TMPDIR at the tmux
// invocation, so the command's literal /run/user/$(id -u) and /tmp legs
// (whose first value contains a space) need no string rewriting.
func TestKillRemoteSessionReachesScratchServer(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	scratch, err := os.MkdirTemp("", "lz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })

	// Unique names: if the shim ever fails to shadow PATH, the surviving real
	// session can never be a user's.
	victim := fmt.Sprintf("lz-victim-%d", os.Getpid())
	keep := fmt.Sprintf("lz-keep-%d", os.Getpid())
	const sock = "lzkill"
	env := append(os.Environ(), "TMUX_TMPDIR="+scratch)
	for _, name := range []string{victim, keep} {
		cmd := exec.Command(realTmux, "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", name)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("new-session %s: %v\n%s", name, err, out)
		}
	}
	t.Cleanup(func() {
		cmd := exec.Command(realTmux, "-L", sock, "kill-server")
		cmd.Env = env
		cmd.Run()
	})

	shimDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "shim-ran")
	shim := "#!/bin/sh\nprintf ran >>" + sentinel + "\nexec env TMUX_TMPDIR=" + scratch + " " + realTmux + " -L " + sock + " \"$@\"\n"
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
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", sshDir+":"+oldPath)

	if err := sshKillRemoteSession("lab", victim); err != nil {
		t.Fatalf("sshKillRemoteSession: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the tmux shim never ran — fake ssh did not shadow PATH: %v", err)
	}
	cmd := exec.Command(realTmux, "-L", sock, "list-sessions", "-F", "#{session_name}")
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	sessions := string(out)
	if strings.Contains(sessions, victim) {
		t.Errorf("victim %q survived the kill: %s", victim, sessions)
	}
	if !strings.Contains(sessions, keep) {
		t.Errorf("keep %q was killed too: %s", keep, sessions)
	}
}
