package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/tmux-og/picker/probekit"
)

// Runner runs a probe's argv; a seam so tests never exec ssh.
type Runner interface {
	// Run runs argv to completion. A non-zero exit is an *exec.ExitError in err.
	Run(ctx context.Context, argv []string) (stdout, stderr string, err error)
}

// ExecRunner runs argv with exec.CommandContext.
type ExecRunner struct{}

// MaxOutput caps the stdout ExecRunner keeps.
const MaxOutput = 1 << 20

const (
	maxStderr = 64 << 10
	// waitDelay bounds how long Wait waits for a grandchild still holding stdout/stderr after the
	// process itself exits or is killed.
	waitDelay = 500 * time.Millisecond
)

// ErrInvalidHost refuses a host alias before any exec: one starting with `-` would be an ssh option.
var ErrInvalidHost = errors.New("invalid host alias")

// Run runs argv in its own session and SIGKILLs the whole process group on cancel, so a remote
// shell's child (a ProxyCommand, a git it started) cannot outlive the deadline holding the pipes.
func (ExecRunner) Run(ctx context.Context, argv []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv from ProbeArgv/HeadsArgv/SSHArgv, run without a shell
	stdout := &cappedBuffer{max: MaxOutput}
	stderr := &cappedBuffer{max: maxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// cappedBuffer keeps the first max bytes and discards the rest without failing the write, so the
// child runs to completion instead of dying on a broken pipe.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.buf.Len(); n > room {
		b.truncated = true
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}

// String drops a truncated buffer's partial last line: a row cut mid-field could still parse.
func (b *cappedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s = s[:strings.LastIndexByte(s, '\n')+1]
	}
	return s
}

// ProbeHost runs ProbeScript on host (a local sh for LocalHost, else ssh) under
// probekit.SnapshotProbeTimeout. Whatever stdout arrived is parsed even when the probe failed:
// ClassifyProbeErr reads it for a Tailscale check URL. Merge still ignores a probe with Err set.
func ProbeHost(ctx context.Context, r Runner, host string, paths []string) HostProbe {
	if !validHostAlias(host) {
		return HostProbe{Host: host, Err: fmt.Errorf("%w %q", ErrInvalidHost, host)}
	}
	ctx, cancel := context.WithTimeout(ctx, probekit.SnapshotProbeTimeout)
	defer cancel()
	stdout, stderr, err := r.Run(ctx, hostArgv(host, ProbeArgv(paths), ProbeRemoteCommand(paths)))
	hp := ParseProbe(host, stdout)
	if err != nil {
		hp.Err = probekit.ClassifyProbeErr(err, stdout, stderr, errors.Is(ctx.Err(), context.DeadlineExceeded))
	}
	return hp
}

// FetchHeads runs HeadsScript on host under probekit.HeadsProbeTimeout. A failed or partial run still
// returns the heads that answered; a missing one leaves its snapshot Stale.
func FetchHeads(ctx context.Context, r Runner, host string, reqs []HeadReq) map[string]string {
	if !validHostAlias(host) || len(headArgs(reqs)) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, probekit.HeadsProbeTimeout)
	defer cancel()
	stdout, _, _ := r.Run(ctx, hostArgv(host, HeadsArgv(reqs), HeadsRemoteCommand(reqs)))
	return ParseHeads(stdout)
}

func hostArgv(host string, local []string, remote string) []string {
	if host == LocalHost {
		return local
	}
	return append([]string{"ssh"}, probekit.SSHArgv(host, remote)...)
}

func validHostAlias(h string) bool {
	return h != "" && h[0] != '-' && allBytes(h, func(c byte) bool {
		return isAlnum(c) || strings.IndexByte("._@:+-", c) >= 0
	})
}
