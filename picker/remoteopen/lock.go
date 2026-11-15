package remoteopen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// lockResult is how acquire ended: the caller exits 0 on lockTimedOutLive
// and prints the timeout message on lockTimedOut.
type lockResult int

const (
	lockAcquired lockResult = iota
	// lockTimedOutLive: the wait timed out but onTimeout found a live daemon.
	lockTimedOutLive
	lockTimedOut
)

// staleOwnerlessTicks is how long (5s at 100ms ticks) a lock may stay
// without an owner file before a waiter judges it stale.
const staleOwnerlessTicks = 50

// intoLock is the --into per-key lock. The daemon listens only after its ssh
// dial and first remote reads, so two calls for one key would both probe no
// daemon and both launch: mkdir serialises them (macOS has no flock). A lock
// is stale when its owner pid is dead, or owner is still unwritten after 5s.
//
// Only the waiter holding path+".break" may break a stale lock, and only
// while the lock still has the identity it was judged stale by. Without that
// mutex a waiter could judge the lock stale, another waiter take it over and
// re-acquire it, and the first one's rename then move that fresh lock. A
// breaker killed inside its section leaves the mutex behind, so one older
// than 5s is removed.
type intoLock struct {
	path  string
	pid   int
	ticks int
	tick  time.Duration
	sleep func(time.Duration)
	now   func() time.Time
	alive func(pid int) bool
	// rename is the PATH mv in production: a bats case stalls it through a
	// fake mv to prove the breaker mutex.
	rename    func(from, to string) bool
	onTimeout func() bool
	held      bool
}

func newIntoLock(path string, pid, ticks int, onTimeout func() bool) *intoLock {
	return &intoLock{
		path:      path,
		pid:       pid,
		ticks:     ticks,
		tick:      100 * time.Millisecond,
		sleep:     time.Sleep,
		now:       time.Now,
		alive:     func(pid int) bool { return syscall.Kill(pid, 0) == nil },
		rename:    mvRename,
		onTimeout: onTimeout,
	}
}

func mvRename(from, to string) bool {
	cmd := exec.Command("mv", from, to) //nolint:gosec // fixed program; from/to are the launcher's own lock paths
	cmd.Stdout = os.Stdout
	return cmd.Run() == nil
}

// acquire waits for the lock. A failed owner write returns an error with
// held set: the lock is ours by mkdir but its owner does not name us, so
// release leaves it for a waiter to break as stale.
func (l *intoLock) acquire() (lockResult, error) {
	breakPath := l.path + ".break"
	staleIdent, staleTicks := "", 0
	for tick := 0; ; tick++ {
		if os.Mkdir(l.path, 0o700) == nil {
			l.held = true
			owner := strconv.Itoa(l.pid) + "\n"
			if err := os.WriteFile(filepath.Join(l.path, "owner"), []byte(owner), 0o600); err != nil {
				return lockTimedOut, err
			}
			return lockAcquired, nil
		}
		if tick >= l.ticks {
			if l.onTimeout() {
				return lockTimedOutLive, nil
			}
			return lockTimedOut, nil
		}
		if ident := l.ident(); ident != staleIdent {
			staleIdent, staleTicks = ident, 0
		}
		staleTicks++
		stale := false
		switch {
		case strings.HasPrefix(staleIdent, "pid:"):
			pid, err := strconv.Atoi(strings.TrimPrefix(staleIdent, "pid:"))
			stale = err != nil || !l.alive(pid)
		case strings.HasPrefix(staleIdent, "ino:") && staleIdent != "ino:":
			stale = staleTicks >= staleOwnerlessTicks
		}
		if stale && os.Mkdir(breakPath, 0o700) == nil {
			stalePath := l.path + ".stale." + strconv.Itoa(l.pid)
			if l.ident() == staleIdent && l.rename(l.path, stalePath) {
				_ = os.RemoveAll(stalePath)
			}
			_ = syscall.Rmdir(breakPath)
			staleIdent = ""
			continue
		}
		if stale {
			if age, ok := l.dirAge(breakPath); ok && age > 5 {
				_ = syscall.Rmdir(breakPath)
			}
		}
		l.sleep(l.tick)
	}
}

// release removes the lock only while held and its owner still names us: a
// holder whose owner write failed can have its lock broken as stale, and what
// sits at the path afterwards is another caller's.
func (l *intoLock) release() {
	if l.held && l.owner() == strconv.Itoa(l.pid) {
		_ = os.RemoveAll(l.path)
	}
}

// owner reads path/owner as `$(cat owner)` would: trailing newlines
// stripped, empty when unreadable.
func (l *intoLock) owner() string {
	b, err := os.ReadFile(filepath.Join(l.path, "owner"))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// ident names which lock sits at path: `pid:<owner>`, or `ino:<inode>` while
// owner is unwritten, so a stale verdict reached on one lock is never applied
// to a successor. Empty once the lock is gone.
func (l *intoLock) ident() string {
	if owner := l.owner(); owner != "" {
		return "pid:" + owner
	}
	fi, err := os.Lstat(l.path)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return "ino:" + strconv.FormatUint(st.Ino, 10)
}

// dirAge is path's age in whole seconds, from second-resolution clocks.
func (l *intoLock) dirAge(path string) (int64, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	return l.now().Unix() - fi.ModTime().Unix(), true
}
