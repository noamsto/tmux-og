package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// attachPhase is one og-remote-open phase line on OG_REMOTE_OPEN_PROGRESS_FD.
type attachPhase string

const (
	phaseLaunch      attachPhase = "launch" // implicit, before the first line
	phaseConnect     attachPhase = "connect"
	phaseStartServer attachPhase = "start-server"
	phaseRestore     attachPhase = "restore"
	phaseCreate      attachPhase = "create"
	phaseMirror      attachPhase = "mirror"
)

// Vars, not consts, so tests can shrink them; newAttachRun snapshots both.
var (
	attachBudgets = map[attachPhase]time.Duration{
		phaseLaunch:      10 * time.Second,
		phaseConnect:     20 * time.Second,
		phaseStartServer: 45 * time.Second,
		phaseRestore:     90 * time.Second,
		phaseCreate:      30 * time.Second,
		phaseMirror:      15 * time.Second,
	}
	// Longer than the launcher's own TERM trap needs (2s) to roll back.
	attachKillGrace = 5 * time.Second
)

func attachPhaseLabel(p attachPhase) string {
	switch p {
	case phaseConnect:
		return "connecting"
	case phaseStartServer:
		return "starting tmux server"
	case phaseRestore:
		return "restoring session"
	case phaseCreate:
		return "creating session"
	case phaseMirror:
		return "attaching"
	}
	return "starting"
}

// parseAttachPhase accepts only the five names the launcher emits; launch is
// implicit and never a line.
func parseAttachPhase(line string) (attachPhase, bool) {
	switch p := attachPhase(line); p {
	case phaseConnect, phaseStartServer, phaseRestore, phaseCreate, phaseMirror:
		return p, true
	}
	return "", false
}

type attachOutcome int8

const (
	attachOK attachOutcome = iota
	attachFailed
	attachTimedOut
	attachCancelled
)

type attachResult struct {
	outcome attachOutcome
	phase   attachPhase   // phase in effect when it ended
	budget  time.Duration // the expired budget, set for attachTimedOut
	msg     string        // attachFailed: sanitized launcher message (never empty)
}

type attachSpec struct {
	bin, host, sess string
	restore         bool
}

// attachRun supervises one og-remote-open: phase progress, per-phase budgets,
// and cancel as TERM then KILL to its whole process group.
type attachRun struct {
	ctx      context.Context
	stop     context.CancelFunc
	spec     attachSpec
	budgets  map[attachPhase]time.Duration
	grace    time.Duration
	progress chan attachPhase // lossy: a slow reader misses phases, never blocks the run
	done     chan struct{}
	res      attachResult // valid once done is closed
	started  atomic.Bool
}

// newAttachRun only allocates: the fork happens in run, off the Update path.
func newAttachRun(spec attachSpec) *attachRun {
	ctx, stop := context.WithCancel(context.Background())
	return &attachRun{
		ctx:      ctx,
		stop:     stop,
		spec:     spec,
		budgets:  maps.Clone(attachBudgets),
		grace:    attachKillGrace,
		progress: make(chan attachPhase, 16),
		done:     make(chan struct{}),
	}
}

func (r *attachRun) cancel() { r.stop() }

func (r *attachRun) result() attachResult { return r.res }

// budget falls back to the launch budget so a phase missing from the table is
// still bounded.
func (r *attachRun) budget(p attachPhase) time.Duration {
	if d, ok := r.budgets[p]; ok {
		return d
	}
	return r.budgets[phaseLaunch]
}

func (r *attachRun) emit(p attachPhase) {
	select {
	case r.progress <- p:
	default:
	}
}

func (r *attachRun) finish(res attachResult) {
	r.res = res
	close(r.done)
}

func buildAttachCmd(spec attachSpec) *exec.Cmd {
	args := []string{spec.host}
	if spec.sess != "" {
		args = append(args, spec.sess)
	}
	cmd := exec.Command(spec.bin, args...)
	// With no controlling tty (Setsid), ssh must fail a surprise auth prompt
	// fast rather than pop a GUI askpass.
	cmd.Env = append(os.Environ(), "OG_REMOTE_OPEN_PROGRESS_FD=3", "SSH_ASKPASS_REQUIRE=never")
	if spec.restore {
		cmd.Env = append(cmd.Env, "OG_REMOTE_RESTORE=1")
	}
	cmd.SysProcAttr = attachSysProcAttr()
	// A grandchild holding stderr must not stall Wait past a clean exit.
	cmd.WaitDelay = attachKillGrace
	return cmd
}

// run forks the launcher and blocks until it has exited and its result is
// set. Only the first call does anything.
func (r *attachRun) run() {
	if !r.started.CompareAndSwap(false, true) {
		return
	}
	if r.ctx.Err() != nil {
		r.finish(attachResult{outcome: attachCancelled, phase: phaseLaunch})
		return
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		r.finish(attachResult{outcome: attachFailed, phase: phaseLaunch, msg: sanitizeStatusText(err.Error())})
		return
	}
	defer pr.Close()
	cmd := buildAttachCmd(r.spec)
	cmd.ExtraFiles = []*os.File{pw}
	// Launcher stderr is the failure text, never painted: the picker owns the
	// screen.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Pdeathsig fires when the forking OS thread exits, not the process
	// (golang/go#27505): keep this goroutine on its thread until Wait returns.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err = cmd.Start()
	pw.Close()
	if err != nil {
		r.finish(attachResult{outcome: attachFailed, phase: phaseLaunch, msg: sanitizeStatusText(err.Error())})
		return
	}
	pgid := cmd.Process.Pid // Setsid: the launcher leads its own group

	lines := make(chan attachPhase)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if p, ok := parseAttachPhase(sc.Text()); ok {
				lines <- p
			}
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	cur := phaseLaunch
	phaseTimer := time.NewTimer(r.budget(cur))
	defer phaseTimer.Stop()
	var (
		timedOut, cancelled bool
		budget              time.Duration
		graceC              <-chan time.Time
	)
	ctxDone := r.ctx.Done()
	terminate := func() {
		phaseTimer.Stop()
		ctxDone = nil
		syscall.Kill(-pgid, syscall.SIGTERM) //nolint:errcheck
		graceC = time.After(r.grace)
	}
	for {
		select {
		case p, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			r.emit(p)
			if timedOut || cancelled {
				continue
			}
			cur = p
			phaseTimer.Reset(r.budget(p))
		case <-phaseTimer.C:
			timedOut, budget = true, r.budget(cur)
			terminate()
		case <-ctxDone:
			cancelled = true
			terminate()
		case <-graceC:
			graceC = nil
			syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck
		case err := <-waited:
			r.drainLines(lines, pr)
			r.finish(classifyAttach(err, cmd, cur, timedOut, cancelled, budget, stderr.String()))
			return
		}
	}
}

// drainLines forwards the phase lines still in the pipe after the launcher
// exited: a fast launcher's last lines otherwise lose the race to Wait. A
// descendant still holding the write end bounds this by the grace, not EOF.
func (r *attachRun) drainLines(lines <-chan attachPhase, pr *os.File) {
	if lines == nil {
		return
	}
	deadline := time.After(r.grace)
	for {
		select {
		case p, ok := <-lines:
			if !ok {
				return
			}
			r.emit(p)
		case <-deadline:
			deadline = nil
			pr.Close()
		}
	}
}

func classifyAttach(err error, cmd *exec.Cmd, phase attachPhase, timedOut, cancelled bool, budget time.Duration, stderr string) attachResult {
	// Exit 0 wins over a cancel or timeout: past its commit point the launcher
	// finishes a complete mirror instead of rolling it back.
	if err == nil || (errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success()) {
		return attachResult{outcome: attachOK, phase: phase}
	}
	if timedOut {
		return attachResult{outcome: attachTimedOut, phase: phase, budget: budget}
	}
	if cancelled {
		return attachResult{outcome: attachCancelled, phase: phase}
	}
	msg := sanitizeStatusText(lastNonEmptyLine(stderr))
	if msg == "" {
		msg = sanitizeStatusText(err.Error())
	}
	return attachResult{outcome: attachFailed, phase: phase, msg: msg}
}

// attachSupervisor lets runTUI cancel the in-flight attach on every exit path
// without depending on the final model.
type attachSupervisor struct {
	mu  sync.Mutex
	cur *attachRun
}

func (s *attachSupervisor) track(r *attachRun) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cur = r
	s.mu.Unlock()
}

// stop cancels the tracked run and waits up to wait for it to finish. A run
// that never started cannot fork once cancelled, so there is nothing to wait
// for.
func (s *attachSupervisor) stop(wait time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	r := s.cur
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	if !r.started.Load() {
		return
	}
	select {
	case <-r.done:
	case <-time.After(wait):
	}
}

const statusTextMaxRunes = 200

// sanitizeStatusText makes remote-derived text safe for a terminal sink: the
// daemon's stripWindowName targets a tmux format instead and keeps C1 and bidi
// controls, which are exactly the threats here (0x9b is a one-byte CSI).
func sanitizeStatusText(s string) string {
	var b strings.Builder
	runes := 0
	space := false
	for i := 0; i < len(s) && runes < statusTextMaxRunes; {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == 0x1b:
			i = skipEscape(s, i)
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || isBidiControl(r):
			continue
		}
		if space {
			b.WriteByte(' ')
			runes++
			space = false
		}
		if runes < statusTextMaxRunes {
			b.WriteRune(r)
			runes++
		}
	}
	return strings.TrimSpace(b.String())
}

// skipEscape returns the index just past the escape sequence whose ESC ended
// at i: a CSI through its final byte, a string sequence (OSC, DCS, SOS, PM,
// APC) through BEL or ST, or else the one character after ESC.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		for i++; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return i
	case ']', 'P', 'X', '^', '_':
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return i
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return i + size
}

func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}
