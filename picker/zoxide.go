package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxZoxideSuggestions caps the suggestions section below the session list.
// The TUI list scrolls (windows around the cursor), so this is just how deep
// into zoxide's ranking we offer, not a render limit.
const maxZoxideSuggestions = 30

var sessionNameReplacer = strings.NewReplacer(".", "_", ":", "_")

// suggestion is a zoxide directory offered for session creation.
type suggestion struct {
	path string // normalized absolute dir path
	name string // derived tmux session name
}

// sessionNameFromPath derives a tmux-safe session name from a directory path:
// basename with '.' and ':' replaced (tmux forbids them in session names).
func sessionNameFromPath(p string) string {
	base := filepath.Base(filepath.Clean(p))
	if base == "/" || base == "." {
		return ""
	}
	return sessionNameReplacer.Replace(base)
}

// normalizePath cleans and symlink-resolves a path for dedupe comparisons.
// Nonexistent paths keep the cleaned form.
func normalizePath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// collapseWorktree maps a git worktree path (or any subdir of one) back to its
// main repo root, so a worktree and its repo collapse to one zoxide suggestion.
//
// Works for any layout — nested "<repo>/.worktrees/<branch>", sibling
// "<repo>-worktrees/<branch>", or a shared external root
// "<root>/.worktrees/<org>/<repo>/<branch>" — by walking up to the nearest
// ".git". A linked worktree's ".git" is a FILE "gitdir: <main>/.git/worktrees/<name>",
// so its root is the segment before "/.git/worktrees/". A normal checkout's
// ".git" is a DIR — left uncollapsed, so a main-repo subdir is still offered
// as-is. Lstat-only per level (no forks); fine for the human-triggered picker.
//
// A string shortcut on "/.worktrees/" is tempting but wrong: it can't tell a
// repo-nested ".worktrees" from a shared root that merely contains one, so
// under a shared root it folds every worktree to the root's parent.
//
// Paths under no git dir are returned unchanged.
func collapseWorktree(p string) string {
	for dir := p; ; {
		gitpath := filepath.Join(dir, ".git")
		if fi, err := os.Lstat(gitpath); err == nil {
			if !fi.Mode().IsRegular() {
				return p // ".git" dir: normal checkout — don't collapse subdirs
			}
			if root := mainRootFromGitFile(gitpath, dir); root != "" {
				return root // linked worktree — collapse to its main repo root
			}
			return p // unparsable ".git" file: leave unchanged
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p // reached "/" with no ".git"
		}
		dir = parent
	}
}

// mainRootFromGitFile parses a linked worktree's ".git" file (gitFile, located
// in dir) and returns the main repo root, or "" when it is not a worktree
// pointer. The "gitdir:" path may be relative to dir.
func mainRootFromGitFile(gitFile, dir string) string {
	data, err := os.ReadFile(gitFile) //nolint:gosec // G304: path built from trusted local state, not user input
	if err != nil {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Clean(filepath.Join(dir, gitdir))
	}
	if i := strings.Index(gitdir, "/.git/worktrees/"); i != -1 {
		return gitdir[:i]
	}
	return ""
}

// zoxideSuggestions filters rank-ordered, normalized zoxide paths against
// existing sessions (by path and by derived name) and cuts to limit.
// Duplicate derived names among suggestions keep only the higher-ranked dir.
func zoxideSuggestions(paths []string, sessionPaths, sessionNames map[string]bool, limit int) []suggestion {
	out := make([]suggestion, 0, limit)
	seen := make(map[string]bool)
	for _, p := range paths {
		if sessionPaths[p] {
			continue
		}
		name := sessionNameFromPath(p)
		if name == "" || sessionNames[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, suggestion{path: p, name: name})
		if len(out) == limit {
			break
		}
	}
	return out
}

// sessionFilterMaps builds the path/name suppression sets from real sessions.
// Scratch sessions are hidden helpers; they must not suppress the suggestion
// for the dir they happen to live in.
//
// A bridge/mirror session's path is likewise not a real suppression signal:
// og-remote-open creates it with no "-c", so session_path is whatever cwd
// the launching client happened to have, not a workspace the user is "in" —
// its windows get their real (remote) cwds from the daemon separately. Its
// name still suppresses, though: mirror names are real tmux-namespace
// entries, so offering a zoxide dir whose derived name collides would make
// createAndSwitch's has-session check switch into the mirror instead of
// creating the local session.
func sessionFilterMaps(sessions []sessionData) (paths, names map[string]bool) {
	paths = make(map[string]bool, len(sessions))
	names = make(map[string]bool, len(sessions))
	for _, s := range sessions {
		if strings.HasPrefix(s.name, "scratch-") {
			continue
		}
		if s.bridgeHost == "" {
			paths[normalizePath(s.path)] = true
		}
		names[s.name] = true
	}
	return paths, names
}

// isExcluded reports whether path matches any blacklist pattern. A pattern
// matches when it equals the path, is an ancestor dir of it (subtree exclude),
// or globs the full path or basename via filepath.Match. So "/tmp/*" drops
// /tmp children, ".ssh" drops any dir named .ssh, and "/home/x/Downloads"
// drops that dir and everything under it. Malformed globs are skipped.
func isExcluded(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, pat := range patterns {
		if pat == path || strings.HasPrefix(path, pat+"/") {
			return true
		}
		if ok, err := filepath.Match(pat, path); err == nil && ok {
			return true
		}
		if ok, err := filepath.Match(pat, base); err == nil && ok {
			return true
		}
	}
	return false
}

// parseExcludePatterns splits a comma-separated blacklist option into trimmed,
// non-empty patterns.
func parseExcludePatterns(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// canonicalZoxidePath maps a raw zoxide entry to the path the picker offers:
// symlink-resolved, worktrees folded into their main repo root. The second
// normalize matters because a worktree root recovered from a ".git" file is raw
// file content, not symlink-resolved like normalizePath/session paths.
func canonicalZoxidePath(line string) string {
	return normalizePath(collapseWorktree(normalizePath(line)))
}

// zoxideLinesFor returns the raw zoxide entries that canonicalize to path.
func zoxideLinesFor(lines []string, path string) []string {
	var out []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if canonicalZoxidePath(l) == path {
			out = append(out, l)
		}
	}
	return out
}

// zoxideForget drops a suggested dir from the zoxide database. It removes every
// raw entry canonicalizing to path, not path alone: one suggestion can stand for
// several entries (worktrees folded into their repo root, an unresolved symlink),
// and any survivor would just re-offer the row on the next refresh.
func zoxideForget(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "zoxide", "query", "-l").Output()
	if err != nil {
		return fmt.Errorf("zoxide query: %w", err)
	}
	raw := zoxideLinesFor(strings.Split(strings.TrimSpace(string(out)), "\n"), path)
	if len(raw) == 0 {
		return nil
	}
	if out, err := exec.Command("zoxide", append([]string{"remove"}, raw...)...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed binary, argv passed without a shell
		return fmt.Errorf("zoxide remove: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// collectZoxide returns ranked zoxide dirs not already covered by a session
// and not matching an exclude pattern. Missing zoxide binary, errors, or dead
// dirs degrade to no suggestions.
func collectZoxide(sessions []sessionData, exclude []string) []suggestion {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "zoxide", "query", "-l").Output()
	if err != nil {
		return nil
	}
	sessionPaths, sessionNames := sessionFilterMaps(sessions)
	var paths []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		p := canonicalZoxidePath(l)
		if isExcluded(p, exclude) {
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue
		}
		paths = append(paths, p)
	}
	return zoxideSuggestions(paths, sessionPaths, sessionNames, maxZoxideSuggestions)
}

const (
	minDefaultWidth  = 80
	minDefaultHeight = 24
)

// flooredClientSize applies the same floor tmux-default-size uses.
func flooredClientSize(width, height int) (int, int, bool) {
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	if width < minDefaultWidth {
		width = minDefaultWidth
	}
	if height < minDefaultHeight {
		height = minDefaultHeight
	}
	return width, height, true
}

func newSessionSizeArgs() []string {
	out, err := exec.Command("tmux", "display-message", "-p", "#{client_width}|#{client_height}").Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 2 {
		return nil
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil {
		return nil
	}
	w, h, ok := flooredClientSize(w, h)
	if !ok {
		return nil
	}
	return []string{"-x", strconv.Itoa(w), "-y", strconv.Itoa(h)}
}

// createAndSwitch creates a detached session at path (unless name already
// exists) and switches the attached client to it. zoxide add keeps the dir's
// rank fresh: the new session's shell never cd's, so zoxide never sees it.
func createAndSwitch(name, path string) error {
	if exec.Command("tmux", "has-session", "-t", "="+name).Run() != nil { //nolint:gosec // G204: fixed binary, argv passed without a shell
		args := []string{"new-session", "-d", "-s", name, "-c", path}
		args = append(args, newSessionSizeArgs()...)
		if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed binary, argv passed without a shell
			return fmt.Errorf("new-session: %s", strings.TrimSpace(string(out)))
		}
	}
	logEvent("picker", "event", "create", "target", name, "path", path)
	_ = exec.Command("tmux", "switch-client", "-t", "="+name).Run() //nolint:gosec // fixed binary, argv passed without a shell
	_ = exec.Command("zoxide", "add", path).Run()                   //nolint:gosec // fixed binary, argv passed without a shell
	return nil
}

// listDir renders a directory listing for the preview pane, preferring eza.
func listDir(path string) string {
	if eza, err := exec.LookPath("eza"); err == nil {
		if out, err := exec.Command(eza, "-la", "--color=always", "--group-directories-first", path).Output(); err == nil { //nolint:gosec // G204: fixed binary, argv passed without a shell
			return strings.TrimRight(string(out), "\n ")
		}
	}
	out, err := exec.Command("ls", "-la", path).Output() //nolint:gosec // G204: fixed binary, argv passed without a shell
	if err != nil {
		return "(no preview available)"
	}
	return strings.TrimRight(string(out), "\n ")
}
