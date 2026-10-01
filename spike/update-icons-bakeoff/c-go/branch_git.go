//go:build !head

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// currentBranch forks `git branch --show-current`, killed after 2 s: the same
// fork count and guard as variants B and D, with no `timeout` exec.
func currentBranch(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}
