# Plan — #852 picker: ownerdir.OwnerOnly for the og-remote-self cache

## Context / invariant

Violated invariant: a cached "alias is self" verdict (which hides a host from the
picker's first paint) must only be trusted when it lives in a directory owned by
and private to the caller, reached without following symlinks, and the marker
file itself is a regular, caller-owned file not reached via a symlink.

Today (`picker/remote.go`):
- `var remoteSelfCacheDir = "/tmp/og-remote-self"` — fixed machine-wide path.
- `remoteSelfCacheDirTrusted` uses `os.Stat` (follows symlinks) and rejects only `&0o002` (group-writable passes).
- `isCachedRemoteSelfAlias` `os.Stat`s the marker (follows a symlinked marker).
- `markCachedRemoteSelfAlias` writes without checking the dir is trusted.

Consumer map (`rg -n 'og-remote-self|remoteSelfCacheDir'`): the only producer
(`markCachedRemoteSelfAlias`), consumers (`isCachedRemoteSelfAlias` via
`dropCachedSelfAliases`/`pendingRemoteItems`, and `collectRemoteItems`
revalidation) and the clear path (`clearCachedRemoteSelfAlias`) all live in
`picker/remote.go`. No script, Nix file, or remote-side writer references the
path; docs/superpowers plans only mention it historically. So changing the
default path needs no second writer to follow.

Per-user default: reuse the same base as the existing remote session cache —
`$XDG_CACHE_HOME/tmux-og/remote-self` (falling back to `~/.cache`), a sibling of
`remoteSessionCacheDir()` (`…/tmux-og/remote`). This is per-user by construction
and not squattable by another account the way a `/tmp/<name>-<uid>` path is. The
package `TestMain` already points `XDG_CACHE_HOME` at a regular file, so tests
that don't opt in can no longer leak into a real `/tmp/og-remote-self` (they do today).

## Files

- `picker/remote.go` — replace the var with a `remoteSelfCacheDir()` func; switch trust to `ownerdir.OwnerOnly`; Lstat the marker; guard the writer.
- `picker/remote_test.go` — migrate the 7 tests that override the var to `useRemoteCache(t)`; replace the world-writable test with the new red tests.
- `docs/agents/*.md` — update only if one mentions `/tmp/og-remote-self` (check with rg; none expected outside docs/superpowers).

## Steps

- [ ] **Step 1: write failing tests** (`picker/remote_test.go`). Replace `TestIsCachedRemoteSelfAliasRejectsUntrustedMarker` with table-free focused tests, each starting with `useRemoteCache(t)`:
  - `TestIsCachedRemoteSelfAliasRejectsGroupWritableDir`: `markCachedRemoteSelfAlias("localhost")`, assert cached (sanity), `os.Chmod(remoteSelfCacheDir(), 0o770)`, assert `!isCachedRemoteSelfAlias("localhost")`.
  - `TestIsCachedRemoteSelfAliasRejectsSymlinkedDir`: create a real 0700 dir `t.TempDir()/real` containing marker file `localhost` (0600), `os.MkdirAll(filepath.Dir(remoteSelfCacheDir()), 0o700)`, `os.Symlink(real, remoteSelfCacheDir())`, assert not cached.
  - `TestIsCachedRemoteSelfAliasDoesNotFollowSymlinkedMarker`: mark some other host `other` so the dir exists 0700; create a regular 0600 file elsewhere; `os.Symlink(that, remoteSelfCachePath("localhost"))`; assert not cached.
  - `TestMarkCachedRemoteSelfAliasCreatesPrivateDir`: fresh (non-existent) dir; `markCachedRemoteSelfAlias("localhost")`; assert `Lstat(remoteSelfCacheDir()).Mode().Perm() == 0o700` and `isCachedRemoteSelfAlias("localhost")`.
  - Also `TestMarkCachedRemoteSelfAliasSkipsUntrustedDir`: pre-create the dir 0o770, mark, assert no marker file was written (Lstat of the path is ENOENT).
  Migrate the other 6 tests from `remoteSelfCacheDir = cacheDir` + Cleanup to `useRemoteCache(t)`.
  Command: `cd picker && go test -run 'RemoteSelf|SelfAlias|SelfHost|SelfOnNoServer|SameMachineDifferentUser' .` — expected: compile failure until step 2 (API change), so red evidence is taken in step 3.
- [ ] **Step 2: implement** (`picker/remote.go`):
  - Delete `var remoteSelfCacheDir`; add `func remoteSelfCacheDir() string` returning `filepath.Join(filepath.Dir(remoteSessionCacheDir()), "remote-self")` when `remoteSessionCacheDir() != ""`, else `""` — or extract a small `tmuxOgCacheDir()` shared by both; pick whichever reads cleaner, one base only.
  - `remoteSelfCachePath(host)` joins onto it.
  - `markCachedRemoteSelfAlias`: `dir := remoteSelfCacheDir(); if dir == "" || os.MkdirAll(dir, 0o700) != nil || !ownerdir.OwnerOnly(dir) { return }`, then write 0600 (mirrors `writeRemoteSessionCache`).
  - `clearCachedRemoteSelfAlias`: return early on `""` dir.
  - `isCachedRemoteSelfAlias`: `dir == "" || !ownerdir.OwnerOnly(dir)` → false; `os.Lstat` the marker; regular + own uid (mirrors `readRemoteSessionCache`).
  - Delete `remoteSelfCacheDirTrusted`. Drop the `syscall` import only if unused.
  - Command: same go test as step 1 → all pass.
- [ ] **Step 3: red/green evidence.** In a scratch copy (`cp -r picker $SCRATCH/picker-red`), restore only the old production checks in `remote.go` (os.Stat + `&0o002` dir check, os.Stat marker, unguarded writer) while keeping the new `remoteSelfCacheDir()` func so the new tests compile. Run the new tests there: expect group-writable, symlinked-dir, symlinked-marker, and skips-untrusted to fail with their assertion messages. Record commands and failures. Fresh-0700 test is expected green on both (it pins the "is used" half).
- [ ] **Step 4: docs.** `rg -n 'og-remote-self' docs/agents CLAUDE.md` — update any hit to the new path; if none, no change.
- [ ] **Step 5: gate.** `cd picker && go vet ./... && go test ./...`, then `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Acceptance

- Group-writable dir refused → `TestIsCachedRemoteSelfAliasRejectsGroupWritableDir` (red in step 3, green after).
- Symlinked dir refused → `TestIsCachedRemoteSelfAliasRejectsSymlinkedDir` (red/green).
- Symlinked cache file not followed → `TestIsCachedRemoteSelfAliasDoesNotFollowSymlinkedMarker` (red/green).
- Fresh dir created 0700 is used → `TestMarkCachedRemoteSelfAliasCreatesPrivateDir` (green).
- `nix build .#default`, `nix flake check`, `nix build .#lint` pass → step 5.
