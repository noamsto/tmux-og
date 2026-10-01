//go:build head

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// currentBranch is variant E: read .git/HEAD instead of forking git. It walks
// up from dir like git's discovery, follows a worktree's `gitdir:` file, and
// reports only a symbolic refs/heads/ HEAD (a detached HEAD is "", as
// `git branch --show-current` prints).
func currentBranch(dir string) string {
	for {
		dotgit := filepath.Join(dir, ".git")
		fi, err := os.Lstat(dotgit)
		if err == nil {
			gitdir := dotgit
			if !fi.IsDir() {
				b, err := os.ReadFile(dotgit)
				if err != nil {
					return ""
				}
				target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
				if !ok {
					return ""
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				gitdir = target
			}
			head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
			if err != nil {
				return ""
			}
			ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
			if !ok {
				return ""
			}
			return ref
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
