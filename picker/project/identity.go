// Package project gives tmux-og its notion of a project: one git repository identified by its
// normalized origin URL, possibly checked out on several hosts.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/noamsto/tmux-og/picker/mirrorname"
)

const (
	maxKeyLen    = 512
	maxPathLen   = 1024
	maxBranchLen = 255
)

var urlSchemes = map[string]bool{
	"ssh": true, "git": true, "http": true, "https": true, "git+ssh": true, "ssh+git": true,
}

// NormalizeKey reduces a git origin URL to "<forge-host>/<owner>/<repo>". ok is false when raw is
// not a remote URL or carries a byte that must never reach a session option, a cache file name or
// a message: the origin of a remote host is attacker-controlled.
func NormalizeKey(raw string) (key string, ok bool) {
	if raw == "" || len(raw) > maxKeyLen || !allBytes(raw, isRawKeyByte) {
		return "", false
	}
	host, path, ok := splitOrigin(raw)
	if !ok {
		return "", false
	}
	host = strings.ToLower(host)
	if host == "" || host[0] == '.' || host[0] == '-' || !allBytes(host, isHostByte) {
		return "", false
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimRight(path, "/")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	key = host + "/" + path
	if !allBytes(key, isKeyByte) {
		return "", false
	}
	return key, true
}

// splitOrigin returns the host (userinfo and port dropped) and the path of a URL, an scp-style
// address or a "host/path" key.
func splitOrigin(raw string) (host, path string, ok bool) {
	if scheme, rest, found := strings.Cut(raw, "://"); found {
		if !urlSchemes[strings.ToLower(scheme)] {
			return "", "", false
		}
		authority, path, found := strings.Cut(rest, "/")
		if !found {
			return "", "", false
		}
		authority = dropUserinfo(authority)
		host, port, hasPort := strings.Cut(authority, ":")
		if hasPort && (port == "" || !allBytes(port, isDigit)) {
			return "", "", false
		}
		return host, path, true
	}
	colon := strings.IndexByte(raw, ':')
	slash := strings.IndexByte(raw, '/')
	if colon >= 0 && (slash < 0 || colon < slash) {
		return dropUserinfo(raw[:colon]), raw[colon+1:], true
	}
	host, path, _ = strings.Cut(raw, "/")
	if !strings.Contains(host, ".") && !strings.Contains(strings.TrimRight(path, "/"), "/") {
		return "", "", false
	}
	return host, path, true
}

func dropUserinfo(s string) string {
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// RepoName is the last path segment of a key.
func RepoName(key string) string {
	_, repo := ownerRepo(key)
	return repo
}

// ownerRepo returns the segment before the last one, or the host when the key has a single
// segment, and the last segment.
func ownerRepo(key string) (owner, repo string) {
	i := strings.LastIndexByte(key, '/')
	if i < 0 {
		return key, key
	}
	repo = key[i+1:]
	owner = key[:i]
	if j := strings.LastIndexByte(owner, '/'); j >= 0 {
		owner = owner[j+1:]
	}
	return owner, repo
}

// LeafName is the remote tmux session that holds a project's windows. It derives from the key
// alone, so every controller agrees on it whatever its local slug collisions are.
func LeafName(key string) string {
	return "og-" + mirrorname.Part(RepoName(key)) + "-" + keyHash(key)[:8]
}

// Slug is the local hub session name for a key. taken reports a name already in use; when every
// candidate is taken the hashed form is returned anyway.
func Slug(key string, taken func(string) bool) string {
	owner, repo := ownerRepo(key)
	name := mirrorname.Part(repo)
	if !taken(name) {
		return name
	}
	name = mirrorname.Part(owner + "-" + repo)
	if !taken(name) {
		return name
	}
	return mirrorname.Part(owner + "-" + repo + "-" + keyHash(key)[:4])
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// SafePath reports whether p is an absolute path that is safe to hand to a remote login shell
// (often fish) unquoted.
func SafePath(p string) bool {
	if len(p) <= 1 || len(p) > maxPathLen || p[0] != '/' || !allBytes(p, isKeyByte) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// SafeBranch reports whether b passes `git check-ref-format --branch` and the narrower charset
// that fish and sh both leave unquoted.
func SafeBranch(b string) bool {
	if b == "" || len(b) > maxBranchLen || b == "HEAD" || !allBytes(b, isBranchByte) {
		return false
	}
	if b[0] == '-' || b[0] == '/' || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") {
		return false
	}
	for seg := range strings.SplitSeq(b, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

func allBytes(s string, ok func(byte) bool) bool {
	for i := range len(s) {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isHostByte(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || strings.IndexByte("._-", c) >= 0
}

// isKeyByte is the charset of a key and of a remote path.
func isKeyByte(c byte) bool {
	return isAlnum(c) || strings.IndexByte("._/@+:-", c) >= 0
}

// isRawKeyByte is isKeyByte plus the escapes a URL may carry; the final key rejects them.
func isRawKeyByte(c byte) bool {
	return isKeyByte(c) || c == '%' || c == '~'
}

func isBranchByte(c byte) bool {
	return isAlnum(c) || strings.IndexByte("._/-", c) >= 0
}
