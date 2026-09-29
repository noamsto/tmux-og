package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clearDiscoveryVars unsets (not empties: git rejects an empty GIT_COMMON_DIR)
// every variable that moves git's repository discovery, for the test's span.
func clearDiscoveryVars(t *testing.T) {
	t.Helper()
	for _, name := range discoveryVars {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}

func TestHeadBranch(t *testing.T) {
	clearDiscoveryVars(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"

	writeFile(t, filepath.Join(root, "repo/.git/HEAD"), "ref: refs/heads/feat/874-fast\n")
	writeFile(t, filepath.Join(root, "repo/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "repo/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "repo/sub/deep/keep"), "")
	writeFile(t, filepath.Join(root, "detached/.git/HEAD"), sha+"\n")
	writeFile(t, filepath.Join(root, "detached/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "detached/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "remoteref/.git/HEAD"), "ref: refs/remotes/origin/main\n")
	writeFile(t, filepath.Join(root, "remoteref/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "remoteref/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "reftable/.git/HEAD"), "ref: refs/heads/.invalid\n")
	writeFile(t, filepath.Join(root, "reftable/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "reftable/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "garbage/.git/HEAD"), "not a head\n")
	writeFile(t, filepath.Join(root, "garbage/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "garbage/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "bare/HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(root, "plain/keep"), "")
	// Not repositories git would accept: no objects/refs, a symlinked HEAD.
	writeFile(t, filepath.Join(root, "hollow/.git/HEAD"), "ref: refs/heads/inner\n")
	writeFile(t, filepath.Join(root, "symhead/.git/objects/.keep"), "")
	writeFile(t, filepath.Join(root, "symhead/.git/refs/.keep"), "")
	writeFile(t, filepath.Join(root, "symhead/.git/real"), "ref: refs/heads/x\n")
	if err := os.Symlink("real", filepath.Join(root, "symhead/.git/HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "repo"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// A linked worktree: .git is a file naming the per-worktree gitdir.
	writeFile(t, filepath.Join(root, "main/.git/worktrees/wt/HEAD"), "ref: refs/heads/wt-branch\n")
	writeFile(t, filepath.Join(root, "main/.git/worktrees/wt/commondir"), "../..\n")
	writeFile(t, filepath.Join(root, "wt/.git"), "gitdir: "+filepath.Join(root, "main/.git/worktrees/wt")+"\n")
	writeFile(t, filepath.Join(root, "relwt/.git"), "gitdir: ../main/.git/worktrees/wt\n")

	tests := []struct {
		name   string
		dir    string
		want   string
		wantOK bool
	}{
		{"branch at the root", "repo", "feat/874-fast", true},
		{"branch from a subdirectory", "repo/sub/deep", "feat/874-fast", true},
		{"detached HEAD prints nothing", "detached", "", true},
		{"linked worktree, absolute gitdir", "wt", "wt-branch", true},
		{"linked worktree, relative gitdir", "relwt", "wt-branch", true},
		{"symbolic ref outside refs/heads is left to git", "remoteref", "", false},
		{"reftable placeholder is left to git", "reftable", "", false},
		{"unrecognized HEAD is left to git", "garbage", "", false},
		{"bare-shaped directory is left to git", "bare", "", false},
		{"no repository above prints nothing", "plain", "", true},
		{"gitdir without objects/refs is left to git", "hollow", "", false},
		{"symlinked HEAD is left to git", "symhead", "", false},
		{"symlinked dir is left to git", "link", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := headBranch(filepath.Join(root, tc.dir))
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("headBranch(%s) = %q, %v; want %q, %v", tc.dir, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	t.Run("relative dir is left to git", func(t *testing.T) {
		if _, ok := headBranch("repo"); ok {
			t.Error("ok = true for a relative dir")
		}
	})
	for _, name := range discoveryVars {
		t.Run(name+" set is left to git", func(t *testing.T) {
			t.Setenv(name, filepath.Join(root, "repo/.git"))
			if _, ok := headBranch(filepath.Join(root, "repo")); ok {
				t.Errorf("ok = true with %s set", name)
			}
		})
	}
}

// TestHeadBranchAgreesWithGit runs both answers over real repositories: the
// in-process read must print what `git branch --show-current` does whenever it
// claims to know.
func TestHeadBranchAgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	clearDiscoveryVars(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(repo, "checkout", "-q", "-b", "feat/x")
	git(repo, "worktree", "add", "-q", "-b", "other", filepath.Join(root, "wt"))
	git(repo, "worktree", "add", "-q", "--detach", filepath.Join(root, "det"))
	if err := os.MkdirAll(filepath.Join(repo, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{repo, filepath.Join(repo, "a/b"), filepath.Join(root, "wt"), filepath.Join(root, "det")} {
		got, ok := headBranch(dir)
		if !ok {
			t.Errorf("headBranch(%s) declined a plain repo", dir)
			continue
		}
		if want := currentBranchByGit(t, dir); got != want {
			t.Errorf("headBranch(%s) = %q, git says %q", dir, got, want)
		}
	}
}

func currentBranchByGit(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
