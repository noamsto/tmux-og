package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// currentBranch is `git -C dir branch --show-current`. It reads HEAD directly
// and forks git only for a repo it cannot read that way: forking one git per
// window put the window picker's first paint behind them all.
func currentBranch(dir string) string {
	if branch, ok := headBranch(dir); ok {
		return branch
	}
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// discoveryVars are the variables that change where git looks for a repository.
var discoveryVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM"}

// headBranch resolves dir's checked-out branch the way git's repository
// discovery would, and reports ok=false whenever it is not certain git would
// answer the same: a discovery variable set, a bare-repo-shaped directory on
// the way up, a repo owned by someone else (git refuses it as unsafe), a
// symbolic ref outside refs/heads, and HEAD content it does not recognize
// (reftable's placeholder included). No repo found is a certain answer: git
// prints nothing.
func headBranch(dir string) (string, bool) {
	for _, name := range discoveryVars {
		if os.Getenv(name) != "" {
			return "", false
		}
	}
	if !filepath.IsAbs(dir) {
		return "", false
	}
	// git chdirs and discovers from the physical path, so a symlinked dir
	// would walk different parents than the lexical walk below.
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != filepath.Clean(dir) {
		return "", false
	}
	var startDev any
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		st, err := os.Stat(d)
		if err != nil {
			return "", false
		}
		// Discovery does not cross a filesystem boundary.
		dev := devOf(st)
		if startDev == nil {
			startDev = dev
		} else if dev != startDev {
			return "", true
		}
		dotGit := filepath.Join(d, ".git")
		fi, err := os.Lstat(dotGit)
		if err == nil {
			// git's ownership check covers the work tree as well as the gitdir.
			if !ownedByMe(st) {
				return "", false
			}
			gitDir, ok := gitDirOf(d, dotGit, fi)
			if !ok {
				return "", false
			}
			return readHead(gitDir)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		// git also accepts a bare repository as the directory itself.
		if _, err := os.Lstat(filepath.Join(d, "HEAD")); !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		if parent := filepath.Dir(d); parent == d {
			return "", true
		}
	}
}

// gitDirOf is the directory holding HEAD for the repo whose .git entry is
// dotGit: the entry itself, or the target of a worktree's "gitdir:" file.
func gitDirOf(workTree, dotGit string, fi os.FileInfo) (string, bool) {
	if fi.IsDir() {
		return dotGit, true
	}
	if !fi.Mode().IsRegular() {
		return "", false
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok || target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(workTree, target)
	}
	return filepath.Clean(target), true
}

func readHead(gitDir string) (string, bool) {
	st, err := os.Stat(gitDir)
	if err != nil {
		return "", false
	}
	if !ownedByMe(st) {
		return "", false
	}
	// A gitdir git would skip (no objects/refs) or a legacy symlinked HEAD
	// (still a symbolic ref to git) is git's to answer.
	// A linked worktree's gitdir holds a commondir pointer instead of them.
	names := []string{"objects", "refs"}
	if _, err := os.Stat(filepath.Join(gitDir, "commondir")); err == nil {
		names = []string{"commondir"}
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(gitDir, name)); err != nil {
			return "", false
		}
	}
	if fi, err := os.Lstat(filepath.Join(gitDir, "HEAD")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	b, err := readSmallFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	head := string(bytes.TrimSpace(b))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || name == "" || name == ".invalid" {
			return "", false
		}
		return name, true
	}
	if isObjectID(head) {
		return "", true // detached: --show-current prints nothing
	}
	return "", false
}

func ownedByMe(fi os.FileInfo) bool {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid()
}

// readSmallFile reads a regular file of at most 4 KiB: a HEAD is one line, and
// a FIFO or huge file in its place must not stall the first paint.
func readSmallFile(path string) ([]byte, error) {
	// Stat before Open: opening a FIFO for reading blocks.
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return nil, errors.New("not a small regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4096))
}

func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func devOf(fi os.FileInfo) any {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return sys.Dev
	}
	return nil
}
