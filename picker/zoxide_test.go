package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseExcludePatterns(t *testing.T) {
	got := parseExcludePatterns("  */.ssh , /tmp/* ,, ")
	want := []string{"*/.ssh", "/tmp/*"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pattern %d: got %q want %q", i, got[i], want[i])
		}
	}
	if parseExcludePatterns("") != nil {
		t.Errorf("empty string should yield nil")
	}
}

func TestIsExcluded(t *testing.T) {
	pats := []string{".ssh", "/tmp/*", "/home/noams/Downloads"}
	cases := []struct {
		path string
		want bool
	}{
		{"/home/noams/.ssh", true},                 // basename
		{"/tmp/zr-rank-test", true},                // glob child
		{"/home/noams/Downloads", true},            // exact dir
		{"/home/noams/Downloads/teamviewer", true}, // subtree
		{"/home/noams/Data/git/tmux-og", false},
		{"/home/noams/.config", false},
	}
	for _, c := range cases {
		if got := isExcluded(c.path, pats); got != c.want {
			t.Errorf("isExcluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if isExcluded("/anything", nil) {
		t.Errorf("nil patterns should exclude nothing")
	}
}

func TestIsExcludedDescendants(t *testing.T) {
	pats := []string{"/tmp/*", "/home/*/scratch", ".ssh"}
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/claude-1000/a/scratchpad/x", true}, // deep under a glob child
		{"/tmp/a/b/c", true},
		{"/home/u/scratch/proj/deep", true}, // glob ancestor mid-path
		{"/home/u/other/proj", false},
		{"/tmpx/a/b", false},           // sibling prefix is not an ancestor
		{"/home/u/.ssh/keys/x", false}, // basename pattern is not applied to ancestors
		{"/home/u/.ssh", true},
	}
	for _, c := range cases {
		if got := isExcluded(c.path, pats); got != c.want {
			t.Errorf("isExcluded(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if !isExcluded("/a/b", []string{"/*"}) {
		t.Errorf("/* should exclude /a/b via its ancestor /a")
	}
}

func TestSessionNameFromPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/n/Data/git/tmux-og", "tmux-og"},
		{"/home/n/proj/foo.bar", "foo_bar"},    // tmux forbids '.' in names
		{"/home/n/proj/a:b", "a_b"},            // tmux forbids ':' in names
		{"/home/n/proj/trailing/", "trailing"}, // trailing slash trimmed
		{"/", ""},                              // root has no usable basename
	}
	for _, c := range cases {
		if got := sessionNameFromPath(c.in); got != c.want {
			t.Errorf("sessionNameFromPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := normalizePath(link); got != normalizePath(real) {
		t.Errorf("normalizePath(%q) = %q, want %q", link, got, normalizePath(real))
	}
	// Nonexistent path: falls back to Clean, no error
	if got := normalizePath("/no/such/dir/../dir2/"); got != "/no/such/dir2" {
		t.Errorf("normalizePath nonexistent = %q, want /no/such/dir2", got)
	}
}

func TestZoxideSuggestions(t *testing.T) {
	paths := []string{
		"/home/n/git/covered", // dropped: session path match
		"/home/n/git/tmux-og", // dropped: derived name collides with session "tmux-og"
		"/home/n/git/alpha",
		"/home/n/work/alpha", // dropped: name collides with earlier suggestion "alpha"
		"/home/n/git/beta",
		"/home/n/git/gamma",
	}
	sessionPaths := map[string]bool{"/home/n/git/covered": true}
	sessionNames := map[string]bool{"tmux-og": true}

	got := zoxideSuggestions(paths, sessionPaths, sessionNames, 15)
	want := []suggestion{
		{path: "/home/n/git/alpha", name: "alpha"},
		{path: "/home/n/git/beta", name: "beta"},
		{path: "/home/n/git/gamma", name: "gamma"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d suggestions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suggestion[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestZoxideSuggestionsTopN(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		p := filepath.Join(dir, n)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, normalizePath(p))
	}
	// nil maps are safe to read in Go — also anchors that contract
	got := zoxideSuggestions(paths, nil, nil, 3)
	if len(got) != 3 {
		t.Fatalf("limit not applied: got %d, want 3", len(got))
	}
	// Rank order preserved
	if got[0].name != "a" || got[2].name != "c" {
		t.Errorf("rank order broken: %v", got)
	}
}

func TestSessionFilterMapsSkipsScratch(t *testing.T) {
	dir := t.TempDir()
	sessions := []sessionData{
		{name: "real", path: dir},
		{name: "scratch-hidden", path: dir},
	}
	paths, names := sessionFilterMaps(sessions)
	if !paths[normalizePath(dir)] || !names["real"] {
		t.Errorf("real session not recorded: paths=%v names=%v", paths, names)
	}
	if names["scratch-hidden"] {
		t.Errorf("scratch session leaked into name filter: %v", names)
	}
	// A dir hosting only a scratch session must still be suggested.
	paths, names = sessionFilterMaps(sessions[1:])
	got := zoxideSuggestions([]string{normalizePath(dir)}, paths, names, 15)
	if len(got) != 1 {
		t.Errorf("scratch-only dir suppressed from suggestions: %v", got)
	}
}

func TestSessionFilterMapsSkipsBridgePath(t *testing.T) {
	dir := t.TempDir()
	sessions := []sessionData{
		{name: "real", path: dir},
		{name: "otherhost-tmux-og", path: dir, bridgeHost: "otherhost"},
	}
	paths, names := sessionFilterMaps(sessions)
	if !paths[normalizePath(dir)] || !names["real"] {
		t.Errorf("real session not recorded: paths=%v names=%v", paths, names)
	}
	// The mirror's own name is a real tmux-namespace entry and must still
	// suppress, even though its path (launcher-cwd artifact) must not.
	if !names["otherhost-tmux-og"] {
		t.Errorf("bridge session name dropped from name filter: %v", names)
	}

	// A dir hosting only a mirror session (no other real session at that
	// path) must still be suggested (#461).
	paths, names = sessionFilterMaps(sessions[1:])
	if paths[normalizePath(dir)] {
		t.Errorf("bridge session's launcher cwd leaked into path filter: %v", paths)
	}
	got := zoxideSuggestions([]string{normalizePath(dir)}, paths, names, 15)
	if len(got) != 1 {
		t.Errorf("mirror-only dir suppressed from suggestions: %v", got)
	}
}

func TestCollapseWorktree(t *testing.T) {
	// Nested layout "<repo>/.worktrees/<branch>": the worktree's ".git" file
	// points into <repo>/.git, so it folds to <repo> via the same walk-up used
	// for every layout — there is no "/.worktrees/" string shortcut.
	repo := t.TempDir()
	wt := mkWorktree(t, repo, filepath.Join(repo, ".worktrees", "feat-x"), "feat-x")
	if got := collapseWorktree(wt); got != repo {
		t.Errorf("collapseWorktree(%q) = %q, want %q", wt, got, repo)
	}

	// Passthrough: with no ".git" to walk up to, the path is returned unchanged.
	// The shared-root case is the anti-regression — the old string shortcut
	// folded it to "/home/n/Data/git".
	for _, in := range []string{
		"/home/n/git/tmux-og",
		"/home/n/notes/.worktrees-backup/x",
		"/home/n/Data/git/.worktrees/org/repo/feat-x",
	} {
		if got := collapseWorktree(in); got != in {
			t.Errorf("collapseWorktree(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestCollapseWorktreeNonNested(t *testing.T) {
	// A linked worktree placed OUTSIDE <repo>/.worktrees/ (e.g. a central or
	// sibling layout). Its ".git" is a file "gitdir: <main>/.git/worktrees/<n>",
	// so collapse must recover <main> even with no "/.worktrees/" in the path.
	main := t.TempDir()
	wt := t.TempDir()
	gitfile := "gitdir: " + main + "/.git/worktrees/feat-x\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(gitfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := collapseWorktree(wt); got != main {
		t.Errorf("collapseWorktree(%q) = %q, want %q (main repo root)", wt, got, main)
	}

	// A subdir of that worktree (e.g. a monorepo's apps/mobile) collapses too,
	// via the walk-up to the worktree's .git file. This is the case that would
	// regress under the sibling layout without the walk-up.
	sub := filepath.Join(wt, "apps", "mobile")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := collapseWorktree(sub); got != main {
		t.Errorf("collapseWorktree(%q) = %q, want %q (main repo root)", sub, got, main)
	}

	// A plain directory (no .git anywhere above) is returned unchanged.
	plain := t.TempDir()
	if got := collapseWorktree(plain); got != plain {
		t.Errorf("collapseWorktree(%q) = %q, want unchanged", plain, got)
	}

	// A normal repo (.git is a directory) and its subdirs are NOT collapsed —
	// only worktrees fold into their root; main-checkout dirs stay as-is.
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := collapseWorktree(repo); got != repo {
		t.Errorf("collapseWorktree(%q) = %q, want unchanged", repo, got)
	}
	repoSub := filepath.Join(repo, "src")
	if err := os.Mkdir(repoSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := collapseWorktree(repoSub); got != repoSub {
		t.Errorf("collapseWorktree(%q) = %q, want unchanged (main-repo subdir)", repoSub, got)
	}
}

func TestCollapseThenSuggest(t *testing.T) {
	// Mirror collectZoxide: collapse every worktree path, then suggest. delta
	// has two worktrees (fold to one suggestion); epsilon's folds to a path with
	// a live session (suppressed). Worktrees live under a shared ".worktrees"
	// root to exercise the layout the old string shortcut mishandled.
	base := t.TempDir()
	root := filepath.Join(base, ".worktrees")
	delta := filepath.Join(base, "delta")
	epsilon := filepath.Join(base, "epsilon")
	for _, d := range []string{delta, epsilon} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	raw := []string{
		mkWorktree(t, delta, filepath.Join(root, "delta", "feat-a"), "feat-a"),
		mkWorktree(t, delta, filepath.Join(root, "delta", "feat-b"), "feat-b"),
		mkWorktree(t, epsilon, filepath.Join(root, "epsilon", "wip"), "wip"),
	}
	var paths []string
	for _, p := range raw {
		paths = append(paths, collapseWorktree(p))
	}
	sessionPaths := map[string]bool{epsilon: true}

	// nil sessionNames is a safe read in Go; this case has no name collisions to suppress
	got := zoxideSuggestions(paths, sessionPaths, nil, 15)
	want := []suggestion{{path: delta, name: "delta"}}
	if len(got) != len(want) {
		t.Fatalf("got %d suggestions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suggestion[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestZoxideLinesFor(t *testing.T) {
	// One suggestion row can stand for several raw zoxide entries: the repo
	// itself, its worktrees (folded to the repo root), and a symlink to it.
	// Forgetting the row must remove all of them, or the survivors re-offer it.
	repo := t.TempDir()
	wt := mkWorktree(t, repo, filepath.Join(repo, ".worktrees", "feat-x"), "feat-x")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()

	lines := []string{repo, wt, link, other, ""}
	got := zoxideLinesFor(lines, normalizePath(repo))
	want := []string{repo, wt, link}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// mkWorktree writes a linked-worktree ".git" file at path pointing back into
// mainRoot's ".git/worktrees/<name>", mirroring what `git worktree add` creates.
func mkWorktree(t *testing.T, mainRoot, path, name string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gitfile := "gitdir: " + mainRoot + "/.git/worktrees/" + name + "\n"
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte(gitfile), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollapseWorktreeCentralRoot(t *testing.T) {
	// worktrunk's shared external root: <root>/.worktrees/<org>/<repo>/<branch>.
	// The path contains "/.worktrees/" but the segment before it is the shared
	// root, NOT the repo — so a "/.worktrees/" string shortcut folds every
	// worktree under the root to the root's parent. Collapse must walk up to
	// the worktree's ".git" file instead.
	main := t.TempDir()
	wt := mkWorktree(t, main,
		filepath.Join(t.TempDir(), ".worktrees", "org", "repo", "feat-x"), "feat-x")
	if got := collapseWorktree(wt); got != main {
		t.Errorf("collapseWorktree(%q) = %q, want %q (main repo root)", wt, got, main)
	}

	// A subdir of that worktree collapses too, via the walk-up.
	sub := filepath.Join(wt, "apps", "mobile")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := collapseWorktree(sub); got != main {
		t.Errorf("collapseWorktree(%q) = %q, want %q", sub, got, main)
	}
}

func TestZoxideSuggestionsSymlinkDedupe(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// collectZoxide normalizes both sides; mirror that contract here.
	sessionPaths := map[string]bool{normalizePath(real): true}
	got := zoxideSuggestions([]string{normalizePath(link)}, sessionPaths, nil, 15)
	if len(got) != 0 {
		t.Errorf("symlinked dir not deduped against session at target: %v", got)
	}
}

func TestFlooredClientSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		w, h  int
		ok    bool
		wantW int
		wantH int
	}{
		{200, 55, true, 200, 55},
		{40, 20, true, 80, 24},
		{0, 24, false, 0, 0},
		{80, 0, false, 0, 0},
	}
	for _, tc := range tests {
		gotW, gotH, ok := flooredClientSize(tc.w, tc.h)
		if ok != tc.ok || gotW != tc.wantW || gotH != tc.wantH {
			t.Errorf("flooredClientSize(%d,%d) = (%d,%d,%v), want (%d,%d,%v)",
				tc.w, tc.h, gotW, gotH, ok, tc.wantW, tc.wantH, tc.ok)
		}
	}
}

func TestNewSessionSizeArgs(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "tmux")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"200|55\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got := newSessionSizeArgs()
	want := []string{"-x", "200", "-y", "55"}
	if len(got) != len(want) {
		t.Fatalf("newSessionSizeArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("newSessionSizeArgs()[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}
