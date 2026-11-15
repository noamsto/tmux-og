package main

import (
	"crypto/sha1" //nolint:gosec // cache-file naming only, must match the shell sha1sum keys
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

func sha1Key(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // cache-file naming only, must match the shell sha1sum keys
	return hex.EncodeToString(sum[:])
}

// srvSuffix scopes stamp files to one tmux server (#705): the socket path from
// $TMUX, sanitized for use in a filename. Empty outside tmux.
func srvSuffix(tmuxEnv string) string {
	if tmuxEnv == "" {
		return ""
	}
	sock, _, _ := strings.Cut(tmuxEnv, ",")
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, ".srv"+sock)
}

// cacheDecision returns "fetch" or "serve" for a cached gh answer. A merged or
// closed PR is terminal and lives under ttlTerminal; a cached "no PR" under ttlNone.
func cacheDecision(force, exists bool, content string, age, ttl, ttlNone, ttlTerminal int64) string {
	if force || !exists {
		return "fetch"
	}
	switch {
	case content == "[]":
		ttl = ttlNone
	case terminalPR(content):
		ttl = ttlTerminal
	}
	if age < ttl {
		return "serve"
	}
	return "fetch"
}

func terminalPR(content string) bool {
	var prs []struct{ State string }
	if json.Unmarshal([]byte(content), &prs) != nil || len(prs) == 0 {
		return false
	}
	return prs[0].State == "MERGED" || prs[0].State == "CLOSED"
}

// pendingClocks derives the pending-check poll clocks, each capped by the
// settled cadence: the fast clock, the slow clock a long episode steps up to,
// and the episode age at which that step happens.
func pendingClocks(checkRefresh int64) (check, slow, fast int64) {
	return min(30, checkRefresh), min(60, checkRefresh), min(600, 1800, checkRefresh)
}

// fileMtime is the mtime in epoch seconds, 0 when the path is absent.
func fileMtime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().Unix()
}

func touchAt(path string, t time.Time) {
	if os.Chtimes(path, t, t) == nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // path is a cache file under the poller's own dir
	if err != nil {
		return
	}
	_ = f.Close()
	_ = os.Chtimes(path, t, t)
}

// writeAtomic renames a pid-suffixed temp over path so readers never see a
// partial file.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // cache files are not secret
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// acquireLock is a mkdir lock (flock is absent on macOS). A holder that crashed
// leaves its dir behind, so one older than OG_LOCK_STALE_SECONDS is stolen; a
// leftover plain file is cleared too.
func acquireLock(path string) (release func(), ok bool) {
	release = func() { _ = os.Remove(path) }
	if os.Mkdir(path, 0o755) == nil { //nolint:gosec // lock dir under the poller's own cache dir
		return release, true
	}
	fi, err := os.Stat(path)
	if err == nil && fi.IsDir() {
		stale := int64(60)
		if v, err := strconv.ParseInt(os.Getenv("OG_LOCK_STALE_SECONDS"), 10, 64); err == nil {
			stale = v
		}
		if time.Now().Unix()-fi.ModTime().Unix() < stale {
			return nil, false
		}
	}
	_ = os.Remove(path)
	if os.Mkdir(path, 0o755) == nil { //nolint:gosec // lock dir under the poller's own cache dir
		return release, true
	}
	return nil, false
}
