# Plan: #1260 remote New-session list folds worktrees, hides deep /tmp

Files: picker/zoxide.go (isExcluded), picker/remote_zoxide.go (remoteZoxideScript), picker/zoxide_test.go + new picker/remote_zoxide_test.go (tests), docs/agents/picker.md.

Mechanism: remoteZoxideRows never folds worktrees (collapseWorktree reads local FS) and isExcluded's `/tmp/*` only matches direct children.

1. Tests first (red on main): isExcluded descendant cases (`/tmp/*` hides `/tmp/a/b/c`, `/home/*/x` etc., basename/exact still work, non-matching sibling kept); remoteZoxideRows with entries [worktree path, repo, /tmp/claude-1000/a/scratchpad/x]  — note the Go side can't fold, so row-level test asserts the deep /tmp is hidden; fold is covered by step 2 test.
2. isExcluded: also test each glob against every ancestor of path (full-path match only; basename match stays on the path itself).
3. remoteZoxideScript: per entry, `[ -d ]`; walk up to nearest `.git`; `.git` file -> read first line with `read` (no forks), strip `gitdir: `, relative resolved against dir, if contains `/.git/worktrees/` fold to the prefix; `.git` dir -> unchanged; re-check `[ -d ]` on folded; dedupe with awk preserving order; then head -n 300. POSIX sh, no single quote or backslash (body rides in shellQuote single quotes), no per-entry fork.
4. Test running remoteZoxideScript under `sh -c` with PATH-stub `zoxide` printing a repo, its linked worktree (real `git worktree add`), a plain dir, a nested subdir of the worktree, a missing dir; assert folded deduped ordered output. Skip if git absent. Add to the bats/go check wiring only if nix needs it (Go tests already in picker-go-tests; git in sandbox must be available — verify, else build the worktree .git file by hand).
5. docs/agents/picker.md: describe remote fold + descendant exclude.
Gate: go test -race, nix build .#default, nix flake check, nix build .#lint.

## Revision 1 (critic: revise)
- Script test: add pkgs.git + OG_REQUIRE_GIT="1" to pickerChecked (flake.nix); test t.Fatal when set and git absent, else Skip. Real `git worktree add`; no hand-built fallback.
- Dedupe stays in the script with no quote/backslash: `D=$; ... awk "!s[${D}0]++"` ($0 built from a variable, so sh does not expand it). Walk-up via ${p%/*}, stop at empty string; no dirname. Relative gitdir: join with dir; `..` segments left (documented; only worktree.useRelativePaths repos).
- Tests go into the existing picker/remote_zoxide_test.go / zoxide_test.go. Fold evidence = script test (red on main); remoteZoxideRows test = deep /tmp hidden + name-dedupe of an already-folded entry. Add isExcluded test: basename pattern is not applied to ancestors.
- Update isExcluded and remoteZoxideScript comments. Move this plan to docs/superpowers/plans/2026-11-24-remote-zoxide-worktree-fold.md. Gate adds CI run id.
