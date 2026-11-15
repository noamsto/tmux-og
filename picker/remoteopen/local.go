package remoteopen

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/mirrorname"
)

// localTmux is the launcher's view of the local tmux server.
type localTmux struct {
	r runner
	// pane is $TMUX_PANE: when set, client-scoped display-message calls
	// target it, since a bare one falls back to the most recently used
	// session.
	pane string
	// ctl is og-remote-bridge-ctl, used to probe a daemon before reaping it.
	ctl   string
	sleep func(time.Duration)
	// reap stops a daemon by pid; nil means reapDaemon.
	reap func(pid int)
	// readPid reads a proven daemon's pidfile; nil means readPidFile.
	readPid func(path string) (int, bool)
}

// query runs tmux and returns its stdout, treating a failure as empty.
func (l *localTmux) query(args ...string) string {
	out, status, err := l.r.Run(cmdSpec{Name: "tmux", Args: args, Capture: true, Stderr: stderrDiscard})
	if err != nil || status != 0 {
		return ""
	}
	return out
}

func (l *localTmux) ok(args ...string) bool {
	_, status, err := l.r.Run(cmdSpec{Name: "tmux", Args: args, Stderr: stderrDiscard})
	return err == nil && status == 0
}

func (l *localTmux) display(format string) string {
	args := []string{"display-message", "-p"}
	if l.pane != "" {
		args = append(args, "-t", l.pane)
	}
	return l.query(append(args, format)...)
}

func (l *localTmux) option(target, name string) string {
	return l.query("show-options", "-qv", "-t", target, name)
}

// resolveInto maps an --into name to a local, non-mirror session id.
func (l *localTmux) resolveInto(name string) (string, error) {
	var id string
	rows := l.query("list-sessions", "-F", "#{session_id}|#{session_name}")
	for row := range strings.SplitSeq(rows, "\n") {
		rowID, rest, found := strings.Cut(row, "|")
		if !found {
			rest = row
		}
		if rest == name {
			id = rowID
			break
		}
	}
	if id == "" {
		return "", &UsageError{Msg: "no local session '" + name + "'"}
	}
	if l.option(id, "@bridge_host") != "" || l.option(id, "@bridge_sock") != "" {
		return "", &UsageError{Msg: "'" + name + "' is a bridge mirror session; --into needs a local one"}
	}
	return id, nil
}

// initialMirrorArea is the invoking client's content area, for seeding a
// detached mirror or a created remote session.
func (l *localTmux) initialMirrorArea() (w, h int, ok bool) {
	return parseMirrorArea(l.display("#{client_width} #{client_height} #{status}"))
}

func parseMirrorArea(raw string) (w, h int, ok bool) {
	f := strings.Fields(raw)
	if len(f) < 2 || !positiveInt(f[0]) || !positiveInt(f[1]) {
		return 0, 0, false
	}
	w, _ = strconv.Atoi(f[0])
	h, _ = strconv.Atoi(f[1])
	status := strings.Join(f[2:], " ")
	rows := 1
	switch status {
	case "off":
		rows = 0
	case "on":
	default:
		if n, err := strconv.Atoi(status); err == nil && isDigits(status) {
			rows = n
		}
	}
	h -= rows
	if h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func positiveInt(s string) bool {
	if !isDigits(s) || s[0] == '0' {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

// termIdentity is the invoking client's termname, termfeatures, sixel flag and
// cell width and height; an empty field is a valid answer.
func (l *localTmux) termIdentity() [5]string {
	return splitTermIdentity(l.display("#{client_termname}|#{client_termfeatures}|#{I/f:sixel}|#{client_cell_width}|#{client_cell_height}"))
}

// splitTermIdentity reads the first line like `IFS='|' read -r` into five
// fields: the last takes the remainder, minus a lone trailing `|`.
func splitTermIdentity(raw string) [5]string {
	line, _, _ := strings.Cut(raw, "\n")
	var out [5]string
	parts := strings.SplitN(line, "|", 5)
	copy(out[:], parts)
	if len(parts) == 5 {
		if rest, trimmed := strings.CutSuffix(parts[4], "|"); trimmed && !strings.Contains(rest, "|") {
			out[4] = rest
		}
	}
	return out
}

func (l *localTmux) curSession() string { return l.display("#{session_name}") }

// readSessionEnv reads NAME from sess's environment; a removed marker
// (`-NAME`) or an unset name is a miss.
func (l *localTmux) readSessionEnv(sess, name string) (string, bool) {
	if sess == "" {
		return "", false
	}
	v := l.query("show-environment", "-t", sess, name)
	if rest, found := strings.CutPrefix(v, name+"="); found {
		return rest, true
	}
	return "", false
}

// splitPairLine splits a pair-lookup row by hand: the session is everything
// after the second `|`, so a trailing `|` in it survives.
func splitPairLine(line string) (id, host, sess string) {
	id, rest, _ := strings.Cut(line, "|")
	host, sess, _ = strings.Cut(rest, "|")
	return id, host, sess
}

// pairLookup finds the mirror of the raw (host, sess) pair, retiring any
// whose name predates name sanitizing.
func (l *localTmux) pairLookup(host, sess string) string {
	var local string
	rows := l.query("list-sessions", "-F", "#{session_id}|#{?#{@bridge_placed},,#{@bridge_host}}|#{?#{@bridge_placed},,#{@bridge_session}}")
	for line := range strings.SplitSeq(rows, "\n") {
		if line == "" {
			continue
		}
		id, h, s := splitPairLine(line)
		if h != host || s != sess {
			continue
		}
		// A stored session holding a newline can forge a row naming an
		// unrelated session: act only once its own options claim the pair.
		if l.option(id, "@bridge_host") != host || l.option(id, "@bridge_session") != sess {
			continue
		}
		name := l.query("display-message", "-p", "-t", id, "#{session_name}")
		if mirrorname.Part(name) != name {
			l.retireMirror(id)
		} else if local == "" {
			local = name
		}
	}
	return local
}

// retireMirror removes a mirror whose name predates sanitizing, with its
// daemon's files. Targeted by id: the raw name may not parse as a target.
func (l *localTmux) retireMirror(id string) {
	old := l.option(id, "@bridge_sock")
	if old != "" && probeDaemon(l.r, l.ctl, old) != "" {
		read := l.readPid
		if read == nil {
			read = readPidFile
		}
		if pid, ok := read(old + ".pid"); ok {
			l.reapPid(pid)
		}
	}
	l.ok("kill-session", "-t", id)
	if old != "" {
		for _, p := range []string{old, old + ".pid", old + ".phase"} {
			_ = os.Remove(p)
		}
	}
}

func (l *localTmux) reapPid(pid int) {
	if l.reap != nil {
		l.reap(pid)
		return
	}
	reapDaemon(pid, l.sleep)
}

// collisionWalk picks the local session name for (host, sess): base, or a
// stable -remote[-n] suffix while base is held by something else.
func (l *localTmux) collisionWalk(base, host, sess string) string {
	name := base
	for n := 0; l.ok("has-session", "-t", "="+name); {
		// show-options rejects the "=" prefix has-session takes: -q turns
		// that into an empty value (#474). The bare name is safe, since
		// has-session proved it exists and an exact match beats a prefix.
		h := l.query("show-options", "-t", name, "-qv", "@bridge_host")
		s := l.query("show-options", "-t", name, "-qv", "@bridge_session")
		if h == host && (s == sess || (s == "" && name == base && base == host+"-"+sess)) {
			break
		}
		n++
		if n == 1 {
			name = base + "-remote"
		} else {
			name = base + "-remote-" + strconv.Itoa(n)
		}
	}
	return name
}
