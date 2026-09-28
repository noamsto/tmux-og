// Package claudestatus resolves and trusts the claude-status state dir: a
// fixed path under /tmp another account on the same machine could plant
// ahead of us, so every consumer must agree on where it lives and refuse to
// read or write it unless we own it and nobody else can.
package claudestatus

import (
	"fmt"
	"os"

	"github.com/noamsto/tmux-og/picker/ownerdir"
)

// DefaultDir is per-user so two accounts on the same machine never share
// the bare /tmp path a symlink or pre-created dir could hijack.
func DefaultDir() string {
	return fmt.Sprintf("/tmp/claude-status-%d", os.Getuid())
}

// Dir honours CLAUDE_STATUS_DIR when set, matching the shell lib's override.
func Dir() string {
	if dir := os.Getenv("CLAUDE_STATUS_DIR"); dir != "" {
		return dir
	}
	return DefaultDir()
}

// Ensure creates dir 0700 if it doesn't exist yet, then reports whether it's
// trusted. It never chmods an existing dir — a loose dir stays loose rather
// than have us silently repair (and thus trust) something another account
// could have planted.
func Ensure(dir string) bool {
	_ = os.MkdirAll(dir, 0o700)
	return ownerdir.OwnerOnly(dir)
}

// Trusted reports whether dir is safe to read or write: not a symlink, is a
// dir, owned by us, and unreadable/unwritable by anyone else. It exists so
// consumers depend on one package for both resolution and the trust check.
func Trusted(dir string) bool {
	return ownerdir.OwnerOnly(dir)
}
