package main

import (
	"os/exec"
	"strings"
	"syscall"
)

// runner is the seam between the poller and every external command, so tests
// can script gh/tmux without PATH shims.
type runner interface {
	// output returns stdout with trailing newlines trimmed, like bash $(...);
	// cmd.Dir is dir when non-empty.
	output(dir, name string, args ...string) (string, error)
	run(name string, args ...string) error
	// detach starts the command fully detached and never waits on it.
	detach(name string, args ...string)
	lookPath(name string) bool
}

type execRunner struct{}

func (execRunner) output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

func (execRunner) run(name string, args ...string) error {
	return exec.Command(name, args...).Run() //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
}

func (execRunner) detach(name string, args ...string) {
	cmd := exec.Command(name, args...) //nolint:gosec // argv is fixed or config-derived and exec'd directly, no shell
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() != nil {
		return
	}
	_ = cmd.Process.Release()
}

func (execRunner) lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
