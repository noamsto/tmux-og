package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/noamsto/tmux-og/picker/probekit"
)

// Live check: real sshListRemoteSessions against a fish-login remote.
// Run: LIVE_REMOTE_PROBE=tp-g6 go test -count=1 -run TestSSHListRemoteSessionsLive .
func TestSSHListRemoteSessionsLive(t *testing.T) {
	host := os.Getenv("LIVE_REMOTE_PROBE")
	if host == "" {
		t.Skip("set LIVE_REMOTE_PROBE=<ssh-host>")
	}

	seed := exec.Command("ssh", probekit.SSHArgv(host,
		"env TMUX_TMPDIR=/run/user/$(id -u) $(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux) -f /dev/null new-session -d -s probe-verify")...)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed remote session: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("ssh", probekit.SSHArgv(host,
			"env TMUX_TMPDIR=/run/user/$(id -u) $(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux) kill-session -t probe-verify")...).Run()
	})

	result, err := sshListRemoteSessions(host)
	if err != nil {
		t.Fatalf("sshListRemoteSessions(%q): %v", host, err)
	}
	names := result.Sessions
	found := false
	for _, n := range names {
		if n == "probe-verify" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected probe-verify in %v", names)
	}
	t.Logf("sessions: %s", strings.Join(names, ", "))
}

// Live check: real sshListRestorableSessions against a host with tmux-remux
// installed and at least one snapshot on disk (no tmux server required).
// Run: LIVE_REMOTE_RESTORE=tp-g6 go test -count=1 -run TestSSHListRestorableSessionsLive .
func TestSSHListRestorableSessionsLive(t *testing.T) {
	host := os.Getenv("LIVE_REMOTE_RESTORE")
	if host == "" {
		t.Skip("set LIVE_REMOTE_RESTORE=<ssh-host> (must have tmux-remux + a snapshot, no live server)")
	}

	m, err := sshListRestorableSessions(host)
	if err != nil {
		t.Fatalf("sshListRestorableSessions(%q): %v", host, err)
	}
	t.Logf("host=%q saved_at=%d sessions=%v", m.Host, m.SavedAt, m.Sessions)
}
