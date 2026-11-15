package remoteopen

import (
	"fmt"
	"strconv"
)

// exitStatus is a `set -e` exit: the launcher ends with the failing remote
// command's status and prints nothing more.
type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return "exit status " + strconv.Itoa(e.code) }

// remote is the ssh side of the launcher: every leg is one ssh argv with the
// remote command as a single element, expanded client-side like the bash.
// phase receives the progress phase names; lookPath resolves timeout(1).
type remote struct {
	r                      runner
	host, tmux, tmpdir, os string
	uid                    string
	phase                  func(string)
	lookPath               func(string) (string, error)
}

func (rm *remote) env() string { return "env TMUX_TMPDIR=" + rm.tmpdir + " " + rm.tmux }

func (rm *remote) ssh(args ...string) cmdSpec {
	return cmdSpec{Name: "ssh", Args: args}
}

// probe feeds script to a remote bash. Placed probes are bounded by ssh
// options and, when timeout(1) exists, by timeout itself. Stderr is inherited.
func (rm *remote) probe(script string, placed bool, probeTimeout int) (string, int, error) {
	stdin := script + "\n"
	c := cmdSpec{Name: "ssh", Args: []string{"-T", rm.host, "bash", "-s"}, Stdin: &stdin, Capture: true}
	if placed {
		p := strconv.Itoa(probeTimeout)
		c.Args = []string{"-T", "-o", "ConnectTimeout=" + p, "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3", rm.host, "bash", "-s"}
		if _, err := rm.lookPath("timeout"); err == nil {
			c.Args = append([]string{p, "ssh"}, c.Args...)
			c.Name = "timeout"
		}
	}
	return rm.r.Run(c)
}

// firstRemoteSession prints the host's most recent session name, empty when
// the remote has no server (the pipeline still exits 0 through head).
func (rm *remote) firstRemoteSession() (string, int) {
	c := rm.ssh(rm.host, rm.env()+" list-sessions -F '#{session_name}' | head -1")
	c.Capture = true
	out, status, _ := rm.r.Run(c)
	return out, status
}

func (rm *remote) hasSession(sess string) bool {
	c := rm.ssh(rm.host, rm.env()+" has-session -t "+shellQuote("="+sess))
	c.Stderr = stderrDiscard
	_, status, err := rm.r.Run(c)
	return err == nil && status == 0
}

// startRemoteServer starts the host's own startup unit. desc names it for
// the caller's "no session appeared" message.
func (rm *remote) startRemoteServer() (string, error) {
	rm.phase("start-server")
	args := []string{rm.host, "--", "systemctl", "--user", "restart", "tmux-startup.service"}
	desc := "tmux-startup.service"
	if rm.os == "Darwin" {
		args = []string{rm.host, "--", "launchctl", "kickstart", "gui/" + rm.uid + "/org.nix-community.home.tmux-startup"}
		desc = "tmux-startup launchd agent"
	}
	if _, status, err := rm.r.Run(rm.ssh(args...)); err != nil || status != 0 {
		return desc, &UsageError{Msg: fmt.Sprintf("%s has no tmux server, and no %s to start one", rm.host, desc)}
	}
	return desc, nil
}

// coldStart is E10: start the server, then resolve the session it created.
func (rm *remote) coldStart() (string, error) {
	desc, err := rm.startRemoteServer()
	if err != nil {
		return "", err
	}
	sess, status := rm.firstRemoteSession()
	if status != 0 {
		return "", &exitStatus{status}
	}
	if sess == "" {
		return "", &UsageError{Msg: fmt.Sprintf("started %s on %s but no session appeared", desc, rm.host)}
	}
	return sess, nil
}

// startIfNoServer re-runs the phase after a cold start, as the bash does.
func (rm *remote) startIfNoServer(phase string) error {
	if first, _ := rm.firstRemoteSession(); first != "" {
		return nil
	}
	if _, err := rm.startRemoteServer(); err != nil {
		return err
	}
	rm.phase(phase)
	return nil
}

// restore is E11: bring sess back through tmux-remux when it is not live.
func (rm *remote) restore(sess string) error {
	rm.phase("restore")
	if rm.hasSession(sess) {
		return nil
	}
	if err := rm.startIfNoServer("restore"); err != nil {
		return err
	}
	c := rm.ssh(rm.host, `command -v tmux-remux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux-remux`)
	c.Capture = true
	remux, status, _ := rm.r.Run(c)
	if status != 0 {
		return &exitStatus{status}
	}
	stdin := "env TMUX_TMPDIR=" + rm.tmpdir + " PATH=" + posixDirname(rm.tmux) + ":$PATH " + remux + " restore\n"
	_, status, err := rm.r.Run(cmdSpec{Name: "ssh", Args: []string{"-T", rm.host, "bash", "-s"}, Stdin: &stdin})
	if err != nil || status != 0 {
		return &UsageError{Msg: "tmux-remux restore failed on " + rm.host}
	}
	if !rm.hasSession(sess) {
		return &UsageError{Msg: fmt.Sprintf("session '%s' was not restored on %s — tmux-remux's restore filter may have skipped it (idle shells / stale age)", sess, rm.host)}
	}
	return nil
}

// createSession is E13: make sess in dir unless it is live. w > 0 adds the
// -x/-y size of the local mirror area.
func (rm *remote) createSession(sess, dir string, w, h int) error {
	rm.phase("create")
	if rm.hasSession(sess) {
		return nil
	}
	if err := rm.startIfNoServer("create"); err != nil {
		return err
	}
	size := ""
	if w > 0 {
		size = fmt.Sprintf(" -x %d -y %d", w, h)
	}
	cmd := rm.env() + " new-session -d -s " + shellQuote(sess) + " -c " + shellQuote(dir) + size
	if _, status, err := rm.r.Run(rm.ssh(rm.host, cmd)); err != nil || status != 0 {
		return &UsageError{Msg: fmt.Sprintf("could not create session '%s' in '%s' on %s", sess, dir, rm.host)}
	}
	if !rm.hasSession(sess) {
		return &UsageError{Msg: fmt.Sprintf("session '%s' was not created on %s", sess, rm.host)}
	}
	return nil
}

// activeWindow is E14 including its phase("connect"): the active window index
// of sess, empty when the session is gone. A non-zero status is a `set -e` exit.
func (rm *remote) activeWindow(sess string) (string, int) {
	rm.phase("connect")
	cmd := rm.env() + " list-windows -t " + shellQuote(sess) + " -F '#{window_index} #{window_active}' | awk '$2==1{print $1; exit}'"
	c := rm.ssh(rm.host, cmd)
	c.Capture = true
	out, status, _ := rm.r.Run(c)
	return out, status
}
