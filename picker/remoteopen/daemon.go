package remoteopen

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// readPidFile parses a daemon pidfile: digits only (after trailing newlines),
// greater than zero.
func readPidFile(path string) (int, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the launcher-derived daemon pidfile
	if err != nil {
		return 0, false
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func remoteDaemonAlive(pidfile string) bool {
	pid, ok := readPidFile(pidfile)
	return ok && processAlive(pid)
}

// probeDaemon asks a live daemon to ping: "live", "outdated" (it predates the
// ctl protocol) or "" (no daemon, or an unrecognised failure).
func probeDaemon(r runner, ctl, sock string) string {
	if !remoteDaemonAlive(sock + ".pid") {
		return ""
	}
	out, status, _ := r.Run(cmdSpec{Name: ctl, Args: []string{"--sock", sock, "ping", "_"}, Stderr: stderrMerge})
	if status == 0 {
		return "live"
	}
	if strings.Contains(out, "— reopen the bridge") {
		return "outdated"
	}
	return ""
}

// reapDaemon TERMs pid, waits up to 2s for it to die, then KILLs it.
func reapDaemon(pid int, sleep func(time.Duration)) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for range 20 {
		if !processAlive(pid) {
			return
		}
		sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

type daemonProc struct {
	Pid  int
	done chan struct{}
}

func (d *daemonProc) exited() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// launchDaemon starts path in its own session with stderr in logPath. A
// goroutine reaps the child so a dead daemon fails kill -0 instead of
// lingering as a zombie.
func launchDaemon(path string, environ []string, logPath string) (*daemonProc, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // G302/G304: caller-chosen log path, world-readable like the bash `2>`
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	defer logf.Close()        //nolint:errcheck // parent's copy of the log fd
	cmd := exec.Command(path) //nolint:gosec // G204: path is the resolved bridge daemon binary
	cmd.Env = environ
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	d := &daemonProc{Pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()
	return d, nil
}

type daemonVals struct {
	Host, Session, Window, Tmux, Tmpdir, LocalSess, Into, Sock, Renderer, Reflow, Select, Predict, RemoteOpen, Term, TermFeatures, Sixel, CellW, CellH, Colorterm, TermProgram string
}

// setEnv sets key: the first KEY= entry is replaced in place, further ones
// dropped; an absent key is appended.
func setEnv(base []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	done := false
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
			continue
		}
		if !done {
			out = append(out, prefix+val)
			done = true
		}
	}
	if !done {
		out = append(out, prefix+val)
	}
	return out
}

func unsetEnv(base []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base))
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func commonEnv(base []string, v daemonVals) []string {
	for _, kv := range [][2]string{
		{"OG_BRIDGE_HOST", v.Host},
		{"OG_BRIDGE_SESSION", v.Session},
		{"OG_BRIDGE_WINDOW", v.Window},
		{"OG_BRIDGE_TMUX", v.Tmux},
		{"OG_BRIDGE_TMPDIR", v.Tmpdir},
		{"OG_DAEMON_LOCAL_SESS", v.LocalSess},
		{"OG_DAEMON_SOCK", v.Sock},
		{"OG_DAEMON_RENDERER", v.Renderer},
		{"OG_DAEMON_REFLOW", v.Reflow},
		{"OG_DAEMON_PREDICT", v.Predict},
		{"OG_BRIDGE_TERM", v.Term},
		{"OG_BRIDGE_TERMFEATURES", v.TermFeatures},
		{"OG_BRIDGE_SIXEL", v.Sixel},
		{"OG_BRIDGE_CELL_W", v.CellW},
		{"OG_BRIDGE_CELL_H", v.CellH},
		{"OG_BRIDGE_COLORTERM", v.Colorterm},
		{"OG_BRIDGE_TERM_PROGRAM", v.TermProgram},
	} {
		base = setEnv(base, kv[0], kv[1])
	}
	return base
}

// placedEnv is the daemon environment for a placed (--into) launch.
func placedEnv(base []string, v daemonVals) []string {
	out := commonEnv(base, v)
	out = setEnv(out, "OG_DAEMON_INTO", v.Into)
	out = setEnv(out, "OG_DAEMON_SELECT", v.Select)
	return unsetEnv(out, "OG_DAEMON_REMOTE_OPEN")
}

// dedicatedEnv is the daemon environment for a dedicated-session launch; an
// inherited OG_DAEMON_INTO is left as is.
func dedicatedEnv(base []string, v daemonVals) []string {
	out := commonEnv(base, v)
	out = unsetEnv(out, "OG_DAEMON_SELECT")
	return setEnv(out, "OG_DAEMON_REMOTE_OPEN", v.RemoteOpen)
}
