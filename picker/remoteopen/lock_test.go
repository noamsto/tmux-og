package remoteopen

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const lockTestPid = 4242

type fakeLock struct {
	*intoLock
	sleeps    int
	renames   int
	timeouts  int
	timeoutOK bool
}

// newFakeLock builds a lock whose sleep advances a fake clock and whose
// liveness reports only the pids in live as running.
func newFakeLock(path string, ticks int, live ...int) *fakeLock {
	f := &fakeLock{}
	clock := time.Unix(1_700_000_000, 0)
	f.intoLock = &intoLock{
		path:  path,
		pid:   lockTestPid,
		ticks: ticks,
		tick:  100 * time.Millisecond,
		sleep: func(d time.Duration) {
			f.sleeps++
			clock = clock.Add(d)
		},
		now:   func() time.Time { return clock },
		alive: func(pid int) bool { return slices.Contains(live, pid) },
		rename: func(from, to string) bool {
			f.renames++
			return os.Rename(from, to) == nil
		},
		onTimeout: func() bool {
			f.timeouts++
			return f.timeoutOK
		},
	}
	return f
}

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "og-into-1.sock.lock")
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeOwner(t *testing.T, lock, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readOwner(t *testing.T, lock string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(lock, "owner"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoLeftovers(t *testing.T, lock string) {
	t.Helper()
	m, err := filepath.Glob(lock + ".stale.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("stale dirs left behind: %v", m)
	}
	if _, err := os.Lstat(lock + ".break"); !os.IsNotExist(err) {
		t.Fatalf("breaker mutex left behind: %v", err)
	}
}

func mustAcquire(t *testing.T, l *fakeLock) {
	t.Helper()
	res, err := l.acquire()
	if err != nil || res != lockAcquired {
		t.Fatalf("acquire = %v, %v; want lockAcquired", res, err)
	}
}

func TestLockAcquireFree(t *testing.T) {
	path := lockPath(t)
	l := newFakeLock(path, 10)
	mustAcquire(t, l)
	if !l.held {
		t.Fatal("held not set")
	}
	if got := readOwner(t, path); got != "4242\n" {
		t.Fatalf("owner = %q, want %q", got, "4242\n")
	}
	if l.sleeps != 0 {
		t.Fatalf("slept %d times on a free path", l.sleeps)
	}
}

func TestLockDeadOwnerBroken(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	l := newFakeLock(path, 10)
	mustAcquire(t, l)
	if got := readOwner(t, path); got != "4242\n" {
		t.Fatalf("owner = %q after takeover", got)
	}
	if l.renames != 1 {
		t.Fatalf("renames = %d, want 1", l.renames)
	}
	// Breaking continues straight into the next mkdir, without sleeping.
	if l.sleeps != 0 {
		t.Fatalf("slept %d times breaking a dead owner's lock", l.sleeps)
	}
	assertNoLeftovers(t, path)
}

func TestLockJunkOwnerIsStale(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "junk\n")
	l := newFakeLock(path, 10)
	mustAcquire(t, l)
	assertNoLeftovers(t, path)
}

func TestLockOwnerlessBrokenOnFiftiethTick(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)

	// 49 stale ticks: still waiting, nothing renamed, timeout consulted.
	l := newFakeLock(path, 49)
	res, err := l.acquire()
	if err != nil || res != lockTimedOut {
		t.Fatalf("acquire = %v, %v; want lockTimedOut", res, err)
	}
	if l.renames != 0 || l.sleeps != 49 {
		t.Fatalf("renames=%d sleeps=%d; want 0, 49", l.renames, l.sleeps)
	}

	// The 50th stale tick breaks it.
	l = newFakeLock(path, 50)
	mustAcquire(t, l)
	if l.renames != 1 || l.sleeps != 49 {
		t.Fatalf("renames=%d sleeps=%d; want 1, 49", l.renames, l.sleeps)
	}
	if l.timeouts != 0 {
		t.Fatal("onTimeout consulted though the lock was broken")
	}
	assertNoLeftovers(t, path)
}

func TestLockIdentChangeResetsStaleCount(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	l := newFakeLock(path, 200)
	sleep := l.sleep
	l.sleep = func(d time.Duration) {
		sleep(d)
		// After 30 owner-less ticks the lock is replaced by a new owner-less
		// one with a different inode: its count starts over.
		if l.sleeps == 30 {
			swapped := path + ".new"
			mustMkdir(t, swapped)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(swapped, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustAcquire(t, l)
	// 30 ticks on the first lock, then 49 sleeps before the 50th tick on the
	// second breaks it.
	if l.sleeps != 30+49 {
		t.Fatalf("sleeps = %d, want %d", l.sleeps, 30+49)
	}
	if l.renames != 1 {
		t.Fatalf("renames = %d, want 1", l.renames)
	}
}

func TestLockLiveOwnerTimesOut(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutOK bool
		want      lockResult
	}{
		{"no live daemon", false, lockTimedOut},
		{"live daemon", true, lockTimedOutLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := lockPath(t)
			mustMkdir(t, path)
			writeOwner(t, path, "777\n")
			l := newFakeLock(path, 5, 777)
			l.timeoutOK = tc.timeoutOK
			res, err := l.acquire()
			if err != nil || res != tc.want {
				t.Fatalf("acquire = %v, %v; want %v", res, err, tc.want)
			}
			if l.timeouts != 1 || l.sleeps != 5 || l.renames != 0 {
				t.Fatalf("timeouts=%d sleeps=%d renames=%d; want 1, 5, 0", l.timeouts, l.sleeps, l.renames)
			}
			if l.held {
				t.Fatal("held set on timeout")
			}
			if got := readOwner(t, path); got != "777\n" {
				t.Fatalf("foreign owner rewritten: %q", got)
			}
		})
	}
}

func TestLockOldBreakMutexRemoved(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	mustMkdir(t, path+".break")
	l := newFakeLock(path, 10)
	old := l.now().Add(-6 * time.Second)
	if err := os.Chtimes(path+".break", old, old); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, l)
	// Tick 0 removes the abandoned mutex and sleeps; tick 1 breaks the lock.
	if l.sleeps != 1 || l.renames != 1 {
		t.Fatalf("sleeps=%d renames=%d; want 1, 1", l.sleeps, l.renames)
	}
	assertNoLeftovers(t, path)
}

func TestLockFreshBreakMutexKept(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	mustMkdir(t, path+".break")
	l := newFakeLock(path, 10)
	// The fake clock advances 1s over 10 ticks, so the mutex never passes 5s.
	fresh := l.now().Add(-4 * time.Second)
	if err := os.Chtimes(path+".break", fresh, fresh); err != nil {
		t.Fatal(err)
	}
	res, err := l.acquire()
	if err != nil || res != lockTimedOut {
		t.Fatalf("acquire = %v, %v; want lockTimedOut", res, err)
	}
	if l.renames != 0 {
		t.Fatalf("renames = %d while another waiter holds the mutex", l.renames)
	}
	if _, err := os.Lstat(path + ".break"); err != nil {
		t.Fatalf("fresh mutex removed: %v", err)
	}
}

// Waiter A holds the breaker mutex: waiter B, judging the same lock stale,
// cannot take the mutex and so never renames.
func TestLockBreakMutexExcludesSecondBreaker(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	var b *fakeLock
	a := newFakeLock(path, 10)
	a.rename = func(from, to string) bool {
		a.renames++
		res, err := b.acquire()
		if err != nil || res != lockTimedOut {
			t.Errorf("B acquire = %v, %v; want lockTimedOut", res, err)
		}
		return os.Rename(from, to) == nil
	}
	b = newFakeLock(path, 3)
	mustAcquire(t, a)
	if a.renames != 1 || b.renames != 0 {
		t.Fatalf("renames A=%d B=%d; want 1, 0", a.renames, b.renames)
	}
	if got := readOwner(t, path); got != "4242\n" {
		t.Fatalf("owner = %q", got)
	}
	assertNoLeftovers(t, path)
}

// The lock is taken over between A's stale verdict and its re-check under
// the mutex: the identity differs, so A leaves the successor's lock alone.
func TestLockTakeoverBeforeRecheckSkipsRename(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	a := newFakeLock(path, 3, 999)
	alive := a.alive
	swapped := false
	a.alive = func(pid int) bool {
		live := alive(pid)
		if !live && !swapped {
			swapped = true
			writeOwner(t, path, "999\n")
		}
		return live
	}
	res, err := a.acquire()
	if err != nil || res != lockTimedOut {
		t.Fatalf("acquire = %v, %v; want lockTimedOut", res, err)
	}
	if a.renames != 0 {
		t.Fatalf("renames = %d; the successor's lock was moved", a.renames)
	}
	if got := readOwner(t, path); got != "999\n" {
		t.Fatalf("owner = %q, want the successor's", got)
	}
	assertNoLeftovers(t, path)
}

// A failed rename leaves the lock in place and still drops the mutex.
func TestLockFailedRenameKeepsLock(t *testing.T) {
	path := lockPath(t)
	mustMkdir(t, path)
	writeOwner(t, path, "4194304\n")
	l := newFakeLock(path, 2)
	l.rename = func(string, string) bool {
		l.renames++
		return false
	}
	res, err := l.acquire()
	if err != nil || res != lockTimedOut {
		t.Fatalf("acquire = %v, %v; want lockTimedOut", res, err)
	}
	if l.renames != 2 {
		t.Fatalf("renames = %d, want 2", l.renames)
	}
	if got := readOwner(t, path); got != "4194304\n" {
		t.Fatalf("owner = %q", got)
	}
	assertNoLeftovers(t, path)
}

func TestLockRelease(t *testing.T) {
	t.Run("own lock removed", func(t *testing.T) {
		path := lockPath(t)
		l := newFakeLock(path, 10)
		mustAcquire(t, l)
		l.release()
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("lock still present: %v", err)
		}
	})
	t.Run("foreign owner left", func(t *testing.T) {
		path := lockPath(t)
		l := newFakeLock(path, 10)
		mustAcquire(t, l)
		writeOwner(t, path, "999\n")
		l.release()
		if got := readOwner(t, path); got != "999\n" {
			t.Fatalf("owner = %q", got)
		}
	})
	t.Run("not held left", func(t *testing.T) {
		path := lockPath(t)
		mustMkdir(t, path)
		writeOwner(t, path, "4242\n")
		l := newFakeLock(path, 10)
		l.release()
		if got := readOwner(t, path); got != "4242\n" {
			t.Fatalf("owner = %q", got)
		}
	})
}

func TestLockProductionRenameUsesPathMv(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	script := "#!/bin/sh\n: >" + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "mv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	l := newIntoLock(lockPath(t), lockTestPid, 10, func() bool { return false })
	if l.rename("a", "b") {
		t.Fatal("rename reported success for a failing mv")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("PATH mv not run: %v", err)
	}
}
