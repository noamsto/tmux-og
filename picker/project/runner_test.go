package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunnerCapturesBothStreams(t *testing.T) {
	stdout, stderr, err := ExecRunner{}.Run(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; exit 3"})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	if stdout != "out\n" || stderr != "err\n" {
		t.Errorf("stdout %q stderr %q", stdout, stderr)
	}
}

func TestExecRunnerCapsStdoutAtWholeLines(t *testing.T) {
	if _, err := exec.LookPath("yes"); err != nil {
		t.Skip("yes not on PATH")
	}
	stdout, _, err := ExecRunner{}.Run(context.Background(), []string{"sh", "-c", "yes 0123456789 | head -n 200000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) > MaxOutput || len(stdout) < MaxOutput-11 {
		t.Errorf("len(stdout) = %d, want just under %d", len(stdout), MaxOutput)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(stdout, "\n"), "\n") {
		if line != "0123456789" {
			t.Fatalf("partial line %q survived the cap", line)
		}
	}
}

func TestExecRunnerKillsProcessGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The backgrounded sleep inherits stdout; only the group kill ends it, WaitDelay alone does not.
	stdout, _, err := ExecRunner{}.Run(ctx, []string{"sh", "-c", "sleep 30 & echo $!; wait"})
	if err == nil {
		t.Fatal("err = nil, want the cancel's error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v after a 200ms deadline", elapsed)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil || pid <= 1 {
		t.Fatalf("background pid %q: %v", stdout, err)
	}
	for deadline := time.Now().Add(2 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background sleep %d outlived the cancel", pid)
		}
	}
}

// alive reports a process that exists and is not a zombie: an orphan's reaper (a sandbox's pid 1)
// may be slow to collect it.
func alive(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	_, rest, ok := strings.Cut(string(stat), ") ")
	return !ok || !strings.HasPrefix(rest, "Z")
}
