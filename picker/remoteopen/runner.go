package remoteopen

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
)

type stderrMode int

const (
	stderrInherit stderrMode = iota
	// stderrDiscard is the bash `2>/dev/null`.
	stderrDiscard
	// stderrMerge is `$(… 2>&1)`: stderr joins the captured stdout.
	stderrMerge
)

// cmdSpec is one external command. The zero value of each mode inherits the
// launcher's stream, as a bare bash command does.
type cmdSpec struct {
	// Name is resolved through PATH when the command runs, never earlier.
	Name string
	Args []string
	// Stdin, when non-nil, is fed to the child instead of os.Stdin.
	Stdin *string
	// Capture returns stdout like bash `$(…)`: every trailing "\n" stripped.
	Capture bool
	Stderr  stderrMode
}

func (c cmdSpec) captures() bool { return c.Capture || c.Stderr == stderrMerge }

// runner runs external commands. A non-zero exit is a status, not an error;
// err reports a command that never ran (status 127 when Name is not on PATH).
// A child killed by a signal reports 128+signo, as bash's $? does.
type runner interface {
	Run(c cmdSpec) (stdout string, status int, err error)
}

var _ runner = execRunner{}

// execRunner runs real processes. before and after (either may be nil) run
// around every command on the caller's goroutine; the launcher's signal
// checkpoint hangs off them.
type execRunner struct {
	before, after func()
}

func (r execRunner) Run(c cmdSpec) (string, int, error) {
	if r.before != nil {
		r.before()
	}
	out, status, err := r.run(c)
	if r.after != nil {
		r.after()
	}
	return out, status, err
}

func (r execRunner) run(c cmdSpec) (string, int, error) {
	path, err := exec.LookPath(c.Name)
	if err != nil {
		return "", 127, fmt.Errorf("%s: %w", c.Name, err)
	}
	cmd := exec.Command(path, c.Args...) //nolint:gosec // G204: argv passed without a shell; the launcher names every program
	cmd.Args[0] = c.Name
	cmd.Stdin = os.Stdin
	if c.Stdin != nil {
		cmd.Stdin = strings.NewReader(*c.Stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = os.Stdout
	if c.captures() {
		cmd.Stdout = &buf
	}
	switch c.Stderr {
	case stderrInherit:
		cmd.Stderr = os.Stderr
	case stderrDiscard:
		cmd.Stderr = nil
	case stderrMerge:
		cmd.Stderr = cmd.Stdout
	}
	if err := cmd.Start(); err != nil {
		return "", 126, fmt.Errorf("%s: %w", c.Name, err)
	}
	err = cmd.Wait()
	out := strings.TrimRight(buf.String(), "\n")
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return out, cmd.ProcessState.ExitCode(), fmt.Errorf("%s: %w", c.Name, err)
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return out, 128 + int(ws.Signal()), nil
	}
	return out, cmd.ProcessState.ExitCode(), nil
}

// sigWatch records the latest TERM, INT or HUP so the launcher can act on it
// between commands, where bash would run its trap.
type sigWatch struct {
	last atomic.Pointer[os.Signal]
}

func watchSignals() *sigWatch {
	w := &sigWatch{}
	ch := make(chan os.Signal, 3)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range ch {
			w.last.Store(&s)
		}
	}()
	return w
}

func (w *sigWatch) pending() (os.Signal, bool) {
	p := w.last.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}

// exiter is the launcher's single way out: release (the --into lock, once
// held) always runs before the process ends, since os.Exit skips defers.
// Production osExit is os.Exit.
type exiter struct {
	release func()
	osExit  func(int)
}

func (e *exiter) exit(code int) {
	if e.release != nil {
		e.release()
	}
	e.osExit(code)
}
