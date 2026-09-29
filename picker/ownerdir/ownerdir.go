// Package ownerdir checks that a cache directory is owned and private to the
// caller, so a dir another account planted (or made world/group-writable, or
// pointed at via a symlink) can't be trusted.
package ownerdir

import (
	"io/fs"
	"os"
	"syscall"
)

// OwnerOnly rejects a cache dir another user could have planted or can write
// into: the rows it holds become trusted input.
func OwnerOnly(dir string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return ownedPrivate(info, os.Getuid())
}

// ownedPrivate is split from OwnerOnly so a foreign owner is testable without root.
func ownedPrivate(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(uid) { //nolint:gosec // callers pass os.Getuid, which is non-negative
		return false
	}
	return info.Mode().Perm()&0o077 == 0
}
