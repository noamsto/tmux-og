package remoteopen

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/tmux-og/picker/mirrorname"
)

// Paths are the shim's build-time store paths. A value still starting with
// '@' is an unsubstituted placeholder and falls back to PATH.
type Paths struct{ Self, Ctl, Daemon, Renderer, Reflow, Loading string }

// exitPanic carries an exit code from exiter.osExit up to main's recover, so
// every exit unwinds through one place without os.Exit skipping the caller.
type exitPanic struct{ code int }

const pollTick = 100 * time.Millisecond

type launcher struct {
	r        runner
	getenv   func(string) string
	environ  func() []string
	lookPath func(string) (string, error)
	launch   func(path string, env []string, logPath string) (*daemonProc, error)
	// sleep is the raw wait; pollSleep wraps it in signal checkpoints.
	sleep    func(time.Duration)
	reap     func(pid int)
	pid      int
	stderr   io.Writer
	ex       *exiter
	sig      *sigWatch
	progress *os.File
	paths    Paths

	placed, attached, mirrorCreated, daemonStarted bool
	// inSignal stops onSignal's own commands from re-entering it.
	inSignal bool
	// settled: the failure path already waited for an in-flight signal.
	settled bool
	// settle bounds the wait for an in-flight signal (signalSettle in production).
	settle    time.Duration
	proc      *daemonProc
	sock      string
	phaseFile string
	localSess string
}

// Main runs og-remote-open over the public argv and returns its exit code.
func Main(p Paths, args []string) int {
	// First, before anything opens an fd that the number could alias.
	progress := adoptProgressFD()
	l := &launcher{
		getenv:   os.Getenv,
		environ:  os.Environ,
		lookPath: exec.LookPath,
		launch:   launchDaemon,
		sleep:    time.Sleep,
		pid:      os.Getpid(),
		stderr:   os.Stderr,
		progress: progress,
		paths:    p,
		settle:   signalSettle,
	}
	l.sig = watchSignals()
	l.r = execRunner{before: l.checkpoint, after: l.afterCommand}
	l.reap = func(pid int) { reapDaemon(pid, l.pollSleep) }
	return l.main(args)
}

// adoptProgressFD takes OG_REMOTE_OPEN_PROGRESS_FD only when something is
// open at that number, and never passes the variable or the fd on.
func adoptProgressFD() *os.File {
	v := os.Getenv("OG_REMOTE_OPEN_PROGRESS_FD")
	_ = os.Unsetenv("OG_REMOTE_OPEN_PROGRESS_FD")
	if !isDigits(v) {
		return nil
	}
	fd, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0); errno != 0 {
		return nil
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "progress")
}

func (l *launcher) main(args []string) (code int) {
	l.ex = &exiter{osExit: func(c int) { panic(exitPanic{c}) }}
	defer func() {
		r := recover()
		if e, ok := r.(exitPanic); ok {
			code = e.code
			return
		}
		if r != nil {
			panic(r)
		}
	}()
	l.run(args)
	return 0
}

// exit ends the run. A failure first settles any in-flight signal, which
// exits through onSignal instead.
func (l *launcher) exit(code int) {
	if code != 0 {
		l.settleSignal()
	}
	l.ex.exit(code)
}

// signalSettle bounds how long a failure waits for a signal os/signal is
// still forwarding.
const signalSettle = 100 * time.Millisecond

// settleSignal runs, once, before a failure is reported: a signal a child
// sent just before exiting may not have reached the watch by the command's
// after-checkpoint, and bash would have run its trap there, before this
// failure. Not from onSignal, and not once attached.
func (l *launcher) settleSignal() {
	if l.inSignal || l.attached || l.settled {
		return
	}
	l.settled = true
	l.sig.waitPending(l.settle)
	l.checkpoint()
}

// fail prints msg and exits 1.
func (l *launcher) fail(msg string) {
	l.settleSignal()
	_, _ = fmt.Fprintln(l.stderr, msg)
	l.exit(1)
}

// check ends the run on err: a *UsageError prints and exits 1, an
// *exitStatus exits with its code.
func (l *launcher) check(err error) {
	if err == nil {
		return
	}
	if status, ok := errors.AsType[*exitStatus](err); ok {
		l.exit(status.code)
	}
	if usageErr, ok := errors.AsType[*UsageError](err); ok {
		l.fail(usageErr.Error())
	}
	l.fail("og-remote-open: " + err.Error())
}

// phase writes one progress line; a closed or never-opened fd is silent.
func (l *launcher) phase(name string) {
	if l.progress != nil {
		_, _ = l.progress.WriteString(name + "\n")
	}
}

func (l *launcher) closeProgress() {
	if l.progress != nil {
		_ = l.progress.Close()
		l.progress = nil
	}
}

// checkpoint runs onSignal when a signal arrived: the point between commands
// where bash would run its trap.
func (l *launcher) checkpoint() {
	if l.inSignal {
		return
	}
	if _, ok := l.sig.pending(); ok {
		l.onSignal()
	}
}

// afterCommand is the checkpoint after a command. A child killed by a signal
// was most likely hit by the same group signal as this process, which
// os/signal may still be forwarding; bash would run its trap before the next
// step, and that step can be destructive with no command in between (a dead
// ctl ping reads as "no daemon", and the live daemon's files are removed).
func (l *launcher) afterCommand(status int) {
	if status > 128 && !l.inSignal && !l.attached {
		l.sig.waitPending(l.settle)
	}
	l.checkpoint()
}

func (l *launcher) pollSleep(d time.Duration) {
	l.checkpoint()
	l.sleep(d)
	l.checkpoint()
}

// onSignal undoes what this call built, up to the commit point: once
// attached, a complete mirror exists and the signal is reported as success.
func (l *launcher) onSignal() {
	l.inSignal = true
	if l.attached {
		l.exit(0)
	}
	if l.placed {
		// The host session is the user's: only the daemon this call launched
		// and its files are undone; the exit path releases the lock.
		if l.daemonStarted && l.proc != nil {
			l.reapOwnDaemon()
			removeFiles(l.sock, l.sock+".pid", l.sock+".phase")
		}
		l.exit(143)
	}
	if l.mirrorCreated {
		if l.daemonStarted && l.proc != nil {
			l.reapOwnDaemon()
		}
		_, _, _ = l.r.Run(cmdSpec{Name: "tmux", Args: []string{"kill-session", "-t", "=" + l.localSess}, Stderr: stderrDiscard})
		removeFiles(l.sock, l.sock+".pid", l.phaseFile)
	}
	l.exit(143)
}

// reapOwnDaemon reaps the daemon this call launched, unless its Wait already
// reaped it and the pid may belong to another process by now.
func (l *launcher) reapOwnDaemon() {
	if !l.proc.exited() {
		l.reap(l.proc.Pid)
	}
}

func removeFiles(paths ...string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// resolve maps a shim path to a binary: an '@' placeholder falls back to
// name on PATH. A required miss exits 1; an optional one is "".
func (l *launcher) resolve(v, name string, required bool) string {
	if !strings.HasPrefix(v, "@") {
		return v
	}
	if p, err := l.lookPath(name); err == nil {
		return p
	}
	if required {
		l.fail("og-remote-open: " + name + " not found on PATH")
	}
	return ""
}

// readDaemonPid reads a proven daemon's pidfile: an unreadable file exits 1,
// non-digit content is no pid.
func (l *launcher) readDaemonPid(path string) (int, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the launcher-derived daemon pidfile
	if err != nil {
		l.fail("og-remote-open: " + err.Error())
	}
	return parsePid(b)
}

func (l *launcher) reapFromPidFile(path string) {
	if pid, ok := l.readDaemonPid(path); ok {
		l.reap(pid)
	}
}

// knob reads an integer test knob; unset, empty or non-integer is def.
func (l *launcher) knob(name string, def int) int {
	if n, err := strconv.Atoi(l.getenv(name)); err == nil {
		return n
	}
	return def
}

// startDaemon launches the bridge daemon detached. daemonStarted goes first,
// so a signal right after the launch reaps it.
func (l *launcher) startDaemon(path string, env []string) {
	l.daemonStarted = true
	proc, err := l.launch(path, env, l.sock+".log")
	if err != nil {
		l.fail("og-remote-open: " + err.Error())
	}
	l.proc = proc
	l.checkpoint()
}

// tmuxStatus runs tmux with inherited streams and returns its status.
func (l *launcher) tmuxStatus(args ...string) int {
	_, status, _ := l.r.Run(cmdSpec{Name: "tmux", Args: args})
	return status
}

// mustTmux runs tmux and exits with its status on failure.
func (l *launcher) mustTmux(args ...string) {
	if status := l.tmuxStatus(args...); status != 0 {
		l.exit(status)
	}
}

// commit marks the attach and switches the client to the mirror, exiting
// with switch-client's status.
func (l *launcher) commit() {
	l.checkpoint()
	l.attached = true
	l.exit(l.tmuxStatus("switch-client", "-t", "="+l.localSess))
}

func noWindowMsg(sess, host string) string {
	return fmt.Sprintf("og-remote-open: session '%s' has no window on %s — it is gone or was never there", sess, host)
}

// run is the launch sequence. It always ends through exit.
func (l *launcher) run(args []string) {
	o, err := ParseArgs(args, Env{
		NewDir:  l.getenv("OG_REMOTE_NEW_DIR"),
		Restore: l.getenv("OG_REMOTE_RESTORE"),
		Tmpdir:  l.getenv("OG_REMOTE_TMPDIR"),
	})
	l.check(err)
	lt := &localTmux{r: l.r, pane: l.getenv("TMUX_PANE"), sleep: l.pollSleep, reap: l.reap, readPid: l.readDaemonPid}

	var intoID string
	if o.Placed {
		l.placed = true
		intoID, err = lt.resolveInto(o.IntoName)
		l.check(err)
	}
	l.check(validateWin(o))
	// The daemon refuses a bad --predict only after the mirror session exists
	// and the client sits on the loading pane, so reject it here.
	predict := lt.query("show-options", "-gqv", "@og_bridge_predict")
	switch predict {
	case "", "auto", "on", "off":
	default:
		l.fail("og-remote-open: @og_bridge_predict must be auto, on or off, got: " + bashQuote(predict))
	}
	l.check(Validate(o))

	probeTimeout, listenTimeout := 0, 0
	if o.Placed {
		probeTimeout, listenTimeout = l.placedLock(o, intoID)
	}

	sess, win, pr := l.probeAndPrepare(o, probeTimeout)
	sess, win, area := l.prepareRemote(o, lt, pr, sess, win)

	l.phase("mirror")
	ctl := l.resolve(l.paths.Ctl, "og-remote-bridge-ctl", true)
	daemon := l.resolve(l.paths.Daemon, "og-remote-bridge-daemon", true)
	renderer := l.resolve(l.paths.Renderer, "og-remote-bridge-renderer", true)
	reflow := l.resolve(l.paths.Reflow, "tmux-reflow-windows", true)
	loading := l.resolve(l.paths.Loading, "og-remote-loading", false)
	lt.ctl = ctl

	vals := daemonVals{
		Host: o.Host, Session: sess, Window: win, Tmux: pr.Tmux, Tmpdir: pr.Tmpdir,
		Renderer: renderer, Reflow: reflow, Predict: predict,
	}
	if o.Placed {
		vals.LocalSess, vals.Into, vals.Sock, vals.Select = o.IntoName, intoID, l.sock, o.Selector
		l.launchPlaced(o, lt, ctl, daemon, listenTimeout, vals)
	}
	l.launchDedicated(o, lt, ctl, daemon, loading, sess, area, vals)
}

type mirrorArea struct {
	w, h int
	ok   bool
}

// placedLock computes the socket key, the per-key lock (held to the end)
// and the live-daemon check, all before any ssh.
func (l *launcher) placedLock(o Opts, intoID string) (probeTimeout, listenTimeout int) {
	serverPid, status, _ := l.r.Run(cmdSpec{Name: "tmux", Args: []string{"display-message", "-p", "-t", intoID, "#{pid}"}, Capture: true})
	if status != 0 {
		l.exit(status)
	}
	l.sock = intoSockPath(intoBaseDir(l.getenv), o.Host, o.Sess, serverPid, intoID, o.Selector)
	probeTimeout = l.knob("OG_INTO_PROBE_TIMEOUT", 15)
	listenTimeout = l.knob("OG_INTO_LISTEN_TIMEOUT", 60)
	ctl := l.resolve(l.paths.Ctl, "og-remote-bridge-ctl", true)

	lock := newIntoLock(l.sock+".lock", l.pid, (probeTimeout+listenTimeout+15)*10, func() bool {
		return probeDaemon(l.r, ctl, l.sock) == "live"
	})
	lock.sleep = l.pollSleep
	l.ex.release = lock.release
	res, err := lock.acquire()
	if err != nil {
		l.fail("og-remote-open: " + err.Error())
	}
	switch res {
	case lockAcquired:
	case lockTimedOutLive:
		l.exit(0)
	case lockTimedOut:
		msg := fmt.Sprintf("og-remote-open: timed out waiting for another --into of %s:%s into '%s'", o.Host, o.Sess, o.IntoName)
		if o.Selector != "" {
			msg += " for --window " + o.Selector
		}
		l.fail(msg)
	}

	switch probeDaemon(l.r, ctl, l.sock) {
	case "live":
		l.exit(0)
	case "":
	default:
		l.reapFromPidFile(l.sock + ".pid")
	}
	removeFiles(l.sock, l.sock+".pid", l.sock+".phase")
	return probeTimeout, listenTimeout
}

// probeAndPrepare runs the one-round-trip probe and checks what it found.
func (l *launcher) probeAndPrepare(o Opts, probeTimeout int) (sess, win string, pr probeResult) {
	l.phase("connect")
	rm := &remote{r: l.r, host: o.Host, phase: l.phase, lookPath: l.lookPath}
	out, status, _ := rm.probe(buildProbeScript(o), o.Placed, probeTimeout)
	if status != 0 {
		l.exit(status)
	}
	pr = parseProbe(out)
	sess, win = o.Sess, o.Win
	// A placed open keeps the caller's name, which its socket key was built
	// from.
	if pr.Sess != "" && !o.Placed {
		sess = pr.Sess
	}
	if win == "" {
		win = pr.Win
	}
	l.check(requireSessionName(sess))
	if !validRemotePath(pr.Tmpdir) {
		l.fail("og-remote-open: unusable remote tmpdir: " + pr.Tmpdir)
	}
	if o.WantMode {
		if pr.Win == "" {
			l.fail(noWindowMsg(sess, o.Host))
		}
		win = pr.Want
		if win == "" {
			win = pr.Win
		}
	}
	if o.Placed && pr.Win == "" {
		l.fail(fmt.Sprintf("og-remote-open: no session named exactly '%s' on %s", sess, o.Host))
	}
	return sess, win, pr
}

// prepareRemote does the cold start, restore, the invoking client's area
// (read once, for both the remote creation and the local mirror), new-dir
// creation and the late active-window lookup.
func (l *launcher) prepareRemote(o Opts, lt *localTmux, pr probeResult, sess, win string) (string, string, mirrorArea) {
	rm := &remote{r: l.r, host: o.Host, tmux: pr.Tmux, tmpdir: pr.Tmpdir, os: pr.OS, uid: pr.UID, phase: l.phase, lookPath: l.lookPath}
	if sess == "" {
		var err error
		sess, err = rm.coldStart()
		l.check(err)
		l.check(requireSessionName(sess))
	}
	if o.Restore != "" && sess != "" {
		l.check(rm.restore(sess))
	}
	var area mirrorArea
	area.w, area.h, area.ok = lt.initialMirrorArea()
	if o.NewDir != "" && sess != "" {
		w, h := 0, 0
		if area.ok {
			w, h = area.w, area.h
		}
		l.check(rm.createSession(sess, o.NewDir, w, h))
	}
	if win == "" {
		var status int
		win, status = rm.activeWindow(sess)
		if status != 0 {
			l.exit(status)
		}
		if win == "" {
			l.fail(noWindowMsg(sess, o.Host))
		}
	}
	return sess, win, area
}

// clientVals fills the invoking client's terminal identity and its session's
// COLORTERM/TERM_PROGRAM.
func clientVals(lt *localTmux, v *daemonVals) {
	ti := lt.termIdentity()
	v.Term, v.TermFeatures, v.Sixel, v.CellW, v.CellH = ti[0], ti[1], ti[2], ti[3], ti[4]
	cur := lt.curSession()
	v.Colorterm, _ = lt.readSessionEnv(cur, "COLORTERM")
	v.TermProgram, _ = lt.readSessionEnv(cur, "TERM_PROGRAM")
}

// launchPlaced launches into the caller's session under the held lock
// and wait for the daemon to listen.
func (l *launcher) launchPlaced(o Opts, lt *localTmux, ctl, daemon string, listenTimeout int, vals daemonVals) {
	clientVals(lt, &vals)
	env := placedEnv(l.environ(), vals)
	l.closeProgress()
	l.startDaemon(daemon, env)

	for range listenTimeout * 10 {
		if probeDaemon(l.r, ctl, l.sock) == "live" {
			l.checkpoint()
			l.attached = true
			l.exit(0)
		}
		if l.proc.exited() {
			l.settleSignal()
			_, _ = fmt.Fprintf(l.stderr, "og-remote-open: the bridge daemon for %s:%s exited before listening:\n", o.Host, o.Sess)
			_, _ = io.WriteString(l.stderr, tailLines(l.sock+".log", 20))
			l.exit(1)
		}
		l.pollSleep(pollTick)
	}
	l.settleSignal()
	_, _ = fmt.Fprintf(l.stderr, "og-remote-open: the bridge daemon for %s:%s was not listening after %ds\n", o.Host, o.Sess, listenTimeout)
	l.reapOwnDaemon()
	removeFiles(l.sock, l.sock+".pid", l.sock+".phase")
	l.exit(1)
}

// tailLines is `tail -n n path`: the last n lines, a missing final newline
// kept missing.
func tailLines(path string, n int) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the daemon's own log
	if err != nil {
		return ""
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// launchDedicated finds or names the mirror session, reuses it behind
// a live daemon, else recreates it, launches its daemon and switches to it.
func (l *launcher) launchDedicated(o Opts, lt *localTmux, ctl, daemon, loading, sess string, area mirrorArea, vals daemonVals) {
	host := o.Host
	sessPart := mirrorname.Part(sess)
	base := mirrorname.Part(host) + "-" + sessPart
	local := lt.pairLookup(host, sess)
	if local == "" {
		local = lt.collisionWalk(base, host, sess)
	}
	l.localSess = local
	l.sock = intoBaseDir(l.getenv) + "/og-daemon-" + local + ".sock"
	l.phaseFile = l.sock + ".phase"

	reply := probeDaemon(l.r, ctl, l.sock)
	if reply == "live" && lt.ok("has-session", "-t", "="+local) {
		l.commit()
	}
	if reply != "" {
		l.reapFromPidFile(l.sock + ".pid")
	}
	l.mirrorCreated = true
	removeFiles(l.sock, l.sock+".pid", l.phaseFile)
	lt.ok("kill-session", "-t", "="+local)

	if err := os.WriteFile(l.phaseFile, []byte("connecting to "+host+"\n"), 0o644); err != nil { //nolint:gosec // G306: the loading pane reads it
		l.fail("og-remote-open: " + err.Error())
	}
	args := []string{"new-session", "-d", "-s", local, "-n", sessPart}
	if area.ok {
		args = append(args, "-x", strconv.Itoa(area.w), "-y", strconv.Itoa(area.h))
	}
	if loading != "" {
		args = append(args, "--", loading, host, sess, l.phaseFile)
	}
	l.mustTmux(args...)
	// Session first: a concurrent reader must never see host set and session
	// empty, and take this mirror for a legacy one.
	l.mustTmux("set-option", "-t", local, "@bridge_session", sess)
	l.mustTmux("set-option", "-t", local, "@bridge_host", host)

	vals.LocalSess, vals.Sock, vals.RemoteOpen = local, l.sock, l.paths.Self
	clientVals(lt, &vals)
	env := dedicatedEnv(l.environ(), vals)
	l.closeProgress()
	l.startDaemon(daemon, env)
	l.commit()
}
