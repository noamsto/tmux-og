package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/tmux-og/picker/agentdetect/debounce"
	"github.com/noamsto/tmux-og/picker/agentdetect/drainbuf"
	"github.com/noamsto/tmux-og/picker/agentdetect/manifest"
	"github.com/noamsto/tmux-og/picker/agentdetect/screen"
	"github.com/noamsto/tmux-og/picker/agentdetect/statefile"
)

const (
	debounceWindow = 80 * time.Millisecond
	// Longest an animating pane can go unsampled. Agents that repaint faster
	// than debounceWindow never fall quiet, so this is their only sampling
	// path (#238); it also bounds how long any pane can show a stale state.
	sampleCeiling = 500 * time.Millisecond
	stateDir      = "/tmp/claude-status/screen"
	// Per-pane backlog cap. The reader always drains stdin into this buffer so
	// tmux never buffers the pipe-pane backlog in-server; if the emulator can't
	// keep up, oldest bytes are dropped and the emulator is resynced. 1 MiB
	// holds many full-screen repaints, so normal bursts never truncate.
	maxBufferedBytes = 1 << 20
	// Leak-reaper interval (#239): coarse because it forks `tmux capture-pane`.
	// Anything faster would fork per tick across every live watcher for no
	// benefit — a dead pane isn't time-sensitive the way animation is; tens
	// of seconds is fine for a backstop that only exists to bound the worst
	// case.
	livenessInterval = 30 * time.Second
	// Geometry poll: cheap enough (one display-message fork per watcher per
	// tick) and bounds how long a pane resize can desync the emulator — the
	// emulator never resizes itself, and a grown pane's repaint either panics
	// the parser (feedSafe re-seeds) or silently clamps the rows where the
	// idle signal lives (#251).
	geometryInterval = 5 * time.Second
	watcherRegDir    = "/tmp/claude-status/watchers"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	paneID := os.Args[1] // already sans '%'

	_, _, cmd, _ := paneInfo(paneID)
	ms, err := manifest.Load()
	if err != nil {
		return
	}
	m, ok := manifest.ForCommand(ms, cmd)
	if !ok {
		return // pane isn't running a known agent; nothing to watch
	}

	myPID := os.Getpid()
	server := serverPID(paneID)
	if !registerWatcher(watcherRegDir, paneID, myPID, server) {
		return
	}

	scr, curCols, curRows := seededScreen(paneID)
	deb := debounce.New(debounceWindow, sampleCeiling)
	w := statefile.New(stateDir, paneID).WithServer(server)
	emit(scr, m, w) // report what is already on screen, before any new output

	buf := drainbuf.New(maxBufferedBytes)
	go readStdin(buf)

	ticker := time.NewTicker(debounceWindow / 2)
	defer ticker.Stop()
	liveness := time.NewTicker(livenessInterval)
	defer liveness.Stop()
	geometry := time.NewTicker(geometryInterval)
	defer geometry.Stop()

	reseed := func() {
		scr.Close()
		scr, curCols, curRows = seededScreen(paneID)
	}

	for {
		select {
		case <-buf.Notify():
			data, truncated, closed := buf.Take()
			if truncated {
				// Dropped bytes broke VT continuity; re-seed so stale rows
				// can't linger. Seeding rather than blanking matters for an
				// agent that only ever repaints a few cells — a blank screen
				// would never be filled back in.
				reseed()
			}
			if len(data) > 0 {
				if !feedSafe(scr, data) {
					reseed()
				}
				deb.Mark(time.Now())
			}
			if closed {
				emitIfOwner(watcherRegDir, paneID, myPID, scr, m, w) // final snapshot on EOF
				return
			}
		case <-ticker.C:
			// A local file read, no fork, so this rides the existing hot
			// ticker rather than waiting on the coarse liveness probe below.
			// Neither removes the registry file (by now it's the new
			// watcher's, not ours — deleting it would make the new watcher
			// self-evict on its own next check) nor emits (see emitIfOwner).
			if !stillOwner(watcherRegDir, paneID, myPID) {
				return
			}
			if deb.Due(time.Now()) {
				emit(scr, m, w)
			}
		case <-geometry.C:
			if !stillOwner(watcherRegDir, paneID, myPID) {
				return
			}
			if c, r, _, ok := paneInfo(paneID); ok && (c != curCols || r != curRows) {
				reseed()
				emit(scr, m, w)
			}
		case <-liveness.C:
			// Backstop for the case EOF never arrives: dead pane, or the
			// tmux server gone outright.
			if !paneAlive(paneID) {
				emitIfOwner(watcherRegDir, paneID, myPID, scr, m, w)
				return
			}
		}
	}
}

// seedBytes converts capture-pane's bare-LF row separators to CR+LF. A raw LF
// moves the cursor down without returning to column 0, so feeding capture-pane
// output unmodified drifts every line right of the last and garbles the seed
// (#251).
func seedBytes(out []byte) []byte {
	return bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
}

// seededScreen returns an emulator primed with the pane's current contents,
// plus the geometry it was created with. pipe-pane only delivers bytes written
// after it is armed, so an emulator that starts blank shows whatever the agent
// happens to repaint next. Agents that redraw a whole frame converge within a
// frame or two; codex animates a few cells at a time, so the line we match on
// ("esc to interrupt") is never rewritten and a blank-start emulator never
// sees it at all (#238). Re-reads geometry so a re-seed also re-syncs the
// emulator size with the pane (#251).
func seededScreen(paneID string) (screen.Screen, int, int) {
	cols, rows, _, _ := paneInfo(paneID)
	scr := screen.New(cols, rows)
	if out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-t", "%"+paneID).Output(); err == nil {
		scr.Feed(seedBytes(out))
	}
	return scr, cols, rows
}

// feedSafe isolates vt emulator panics: an agent addressing rows outside the
// emulator's geometry — the pane grew after the watcher started, and the
// emulator never resizes — panics inside ultraviolet (#251). Dying would
// freeze the pane's last state until the sweep re-arms; recovering and
// re-seeding self-corrects both the parser state and the geometry.
func feedSafe(scr screen.Screen, data []byte) (ok bool) {
	defer func() { ok = recover() == nil }()
	scr.Feed(data)
	return true
}

func emit(scr screen.Screen, m manifest.Manifest, w *statefile.Writer) {
	state, flags, _ := manifest.Match(m, scr.Text(), scr.Title(), scr.AltScreen())
	_, _ = w.Update(state, flags, time.Now())
}

// emitIfOwner skips emit() once superseded by a re-arm — the new watcher
// already reported current state at its own startup, and statefile.Writer's
// temp file isn't pid-namespaced, so a stale write here could race and
// clobber the new watcher's write.
func emitIfOwner(dir, paneID string, pid int, scr screen.Screen, m manifest.Manifest, w *statefile.Writer) {
	if stillOwner(dir, paneID, pid) {
		emit(scr, m, w)
	}
}

func readStdin(buf *drainbuf.Buffer) {
	r := bufio.NewReader(os.Stdin)
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			buf.Append(b[:n]) // Append copies; reusing b is safe
		}
		if err != nil {
			buf.Close()
			return
		}
	}
}

func paneInfo(paneID string) (cols, rows int, cmd string, ok bool) {
	cols, rows = 80, 24
	out, err := exec.Command("tmux", "display", "-p", "-t", "%"+paneID,
		"#{pane_width} #{pane_height} #{pane_current_command}").Output()
	if err != nil {
		return
	}
	c, r, cmd, ok := parsePaneInfo(string(out))
	if !ok {
		return // keep 80×24 defaults; callers must check ok before acting
	}
	return c, r, cmd, true
}

// parsePaneInfo is the pure half of paneInfo: ok is true only when width and
// height both parse from a non-empty reply. tmux display-message is
// CANFAIL-tolerant (exits 0 against a dead pane), so callers must never act
// on a failed read — never re-seed to the 80×24 default on a transient miss
// (#251).
func parsePaneInfo(out string) (cols, rows int, cmd string, ok bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 2 {
		return 0, 0, "", false
	}
	c, errC := strconv.Atoi(f[0])
	r, errR := strconv.Atoi(f[1])
	if errC != nil || errR != nil {
		return 0, 0, "", false
	}
	if len(f) >= 3 {
		cmd = f[2]
	}
	return c, r, cmd, true
}

var serverPIDRe = regexp.MustCompile(`^[0-9]+$`)

// serverPID returns the pid of the tmux server owning paneID, or "" when it
// cannot be resolved to a number. It is the ownership stamp that keeps another
// server's boot-time prune off this pane's state files.
func serverPID(paneID string) string {
	out, err := exec.Command("tmux", "display", "-p", "-t", "%"+paneID, "#{pid}").Output()
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(out))
	if !serverPIDRe.MatchString(pid) {
		return ""
	}
	return pid
}

// registerWatcher atomically claims paneID for pid, replacing whatever a
// previous watcher wrote. The file is the pid on line 1, plus a server=<pid>
// line when server is known. Returns false only if the filesystem itself is
// unusable (can't mkdir/write/rename) — in that case the caller can't
// guarantee it's the sole watcher for this pane, so it should not become a
// long-lived process that might duplicate one (#239).
func registerWatcher(dir, paneID string, pid int, server string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	final := filepath.Join(dir, paneID)
	tmp := final + "." + strconv.Itoa(pid) + ".tmp"
	content := strconv.Itoa(pid)
	if server != "" {
		content += "\nserver=" + server + "\n"
	}
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false
	}
	return os.Rename(tmp, final) == nil
}

// stillOwner reports whether pid is still the registered watcher for paneID.
// A re-arm overwrites the registry with the new watcher's pid; once that
// happens the old process reads a mismatch here and exits, which is what
// closes the re-arm leak (#239) without any tmux-side changes.
func stillOwner(dir, paneID string, pid int) bool {
	content, err := os.ReadFile(filepath.Join(dir, paneID))
	if err != nil {
		return false
	}
	return ownerMatches(string(content), pid)
}

// ownerMatches is the pure comparison stillOwner delegates to, split out so
// the decision is testable without touching the filesystem.
func ownerMatches(registered string, pid int) bool {
	first, _, _ := strings.Cut(registered, "\n")
	return strings.TrimSpace(first) == strconv.Itoa(pid)
}

// paneAlive reports whether the pane still exists, by asking tmux directly
// rather than trusting any cached state. Deliberately not `tmux display-message
// -t <pane>`: display-message's target lookup is declared CMD_FIND_CANFAIL in
// tmux's own source (cmd-display-message.c), so it tolerates a missing target
// and exits 0 even against a dead pane — verified empirically against tmux
// 3.7b, the exact binary this repo wraps. capture-pane's target flag is not
// CANFAIL (cmd-capture-pane.c), so it correctly errors "can't find pane" and
// exits nonzero on a dead one; it's also the pattern seededScreen already uses
// elsewhere in this file, so no new probing style is introduced.
func paneAlive(paneID string) bool {
	err := exec.Command("tmux", "capture-pane", "-p", "-t", "%"+paneID).Run()
	return aliveFromProbe(err)
}

// aliveFromProbe is the pure decision behind the leak-reaper backstop: a nil
// error means the pane answered, anything else — pane gone, session gone, or
// tmux server unreachable entirely — means exit. An unreachable tmux must
// never be read as "assume alive" (#239): a dead server can't tell us
// otherwise, so silence has to mean gone.
func aliveFromProbe(err error) bool { return err == nil }
