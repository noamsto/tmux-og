# Statusline: refuse a cache dir the caller does not own (#842)

Stacked on #841. `writeLastGood` does `os.MkdirAll(dir, 0o755)` under a shared
temp base and trusts whatever is already there, so another account can
pre-create `og-statusline-<uid>` (or make it world-writable / a symlink) and
feed line 0 its own frame. Violated invariant: the statusline reads and writes
its last-good frame only in a directory owned by the caller with no group/other
permission bits, and never through a symlinked dir.

## Design

The check already exists as `ownerOnlyDir` in `picker/remote.go` (package
`main`, not importable from `picker/statusline`). Move it to a new tiny package
`picker/ownerdir` (same Go module, no new deps, no nix change — `buildGoModule`
compiles every imported package) and call it from both places, rather than
copying it a second time. `remote.go` changes only by swapping its two call
sites and deleting the old function.

```go
// Package ownerdir
func OwnerOnly(dir string) bool            // Lstat; dir, not symlink; ownedPrivate(info, os.Getuid())
func ownedPrivate(info fs.FileInfo, uid int) bool // Stat_t.Uid == uid && Perm()&0o077 == 0
```

The pure `ownedPrivate` exists so the foreign-owner case (unreachable without
root in tests) has a unit test with a fake `FileInfo`.

Statusline: `readLastGood` returns a miss when `!ownerdir.OwnerOnly(dir)`;
`writeLastGood` does `MkdirAll(dir, 0o700)` then bails on `!OwnerOnly(dir)`,
and writes the file `0o600`. Failure is silent ("no cache"), never an error.

## Files

- `picker/ownerdir/ownerdir.go` — new: `OwnerOnly`, `ownedPrivate`.
- `picker/ownerdir/ownerdir_test.go` — new: unit tests incl. foreign uid.
- `picker/remote.go` — use `ownerdir.OwnerOnly`, delete `ownerOnlyDir`.
- `picker/statusline/main.go` — gate `readLastGood`/`writeLastGood`.
- `picker/statusline/main_test.go` — regression tests through the read/write path.
- `flake.nix` — add `go test ./ownerdir/...` to `pickerChecked.checkPhase` (it runs a fixed package list).
- `tests/remote-m2-integration.bats` — pre-create the statusline cache dir `mkdir -m 700 -p` so the test still exercises a trusted cache.
- `docs/agents/bridge-shipped-state.md` or `enrichment.md` — only if a line there describes the cache dir's trust (checked in step 6).

## Steps

- [ ] **Step 1: failing statusline tests** — `picker/statusline/main_test.go`: add
  - `TestLastGoodRefusesForeignModeDir`: `dir` = `t.TempDir()/c`, `Mkdir 0o700` then `Chmod 0o777`; plant `dir/work` with content `evil`; assert `readLastGood(dir,"work")` misses; call `writeLastGood(dir,"work","mine")`; assert `dir/work` still reads `evil` and no other entry (no `.tmp.*`) exists.
  - `TestLastGoodRefusesSymlinkedDir`: real dir `t.TempDir()/real` (0o700) holding `work`=`evil`; `link` → `real`; assert `readLastGood(link,…)` misses and `writeLastGood(link,…,"mine")` leaves `real/work` == `evil`.
  - `TestLastGoodCreatesPrivateDir`: `dir` = `t.TempDir()/new` (absent); `writeLastGood`; assert `Lstat(dir).Mode().Perm() == 0o700` and round-trip reads `mine`.
  Run `cd picker && go test ./statusline/ -run 'LastGood'` → expect the three new tests FAIL (read hits `evil`, perm 0o755), existing ones pass. Record output (red).
- [ ] **Step 2: failing ownerdir tests** — `picker/ownerdir/ownerdir_test.go` with a fake `fs.FileInfo` whose `Sys()` returns `*syscall.Stat_t{Uid: …}`: owner match + 0o700 → true; foreign uid + 0o700 → false; own uid + 0o770 / 0o707 → false; `Sys()` not a `*Stat_t` → false. Plus `OwnerOnly`: `t.TempDir()` → true; after `Chmod 0o755` → false; missing path → false; a regular file → false; a symlink to an owner-only dir → false. `go test ./ownerdir/` fails to compile (red for missing package; not bug evidence).
- [ ] **Step 3: implement `picker/ownerdir/ownerdir.go`** — body lifted from `ownerOnlyDir` (keep its doc comment's why). `go test ./ownerdir/` → PASS.
- [ ] **Step 3b: run ownerdir tests in the flake** — `flake.nix` `pickerChecked.checkPhase`: insert `go test ./ownerdir/...` after `go test ./statusline/...`. Proven in Step 7 (`nix flake check` log / `nix log` shows the ownerdir package `ok`).
- [ ] **Step 4: switch `picker/remote.go`** — replace both `ownerOnlyDir(dir)` calls with `ownerdir.OwnerOnly(dir)`, delete `ownerOnlyDir`, fix imports (`syscall` still used elsewhere in the file). `cd picker && go build ./... && go test . -run 'Remote|Cache'` → PASS.
- [ ] **Step 5: gate statusline** — in `main.go`, `readLastGood`: `if !ownerdir.OwnerOnly(dir) { return "", false }`; `writeLastGood`: `if os.MkdirAll(dir, 0o700) != nil || !ownerdir.OwnerOnly(dir) { return }` and `os.WriteFile(tmp, …, 0o600)`. Extend the `statuslineCacheDir` doc comment by one clause (dir must be caller-owned and private). `go test ./statusline/` → all PASS (green).
- [ ] **Step 5b: bats cache dir** — `tests/remote-m2-integration.bats` line ~2624: `mkdir -p "$statusline_cache_dir"` → `mkdir -m 700 -p "$statusline_cache_dir"` (a 0o755 dir would now be refused silently). Proven in Step 7.
- [ ] **Step 6: docs** — `rg -n 'og-statusline|OG_STATUSLINE_CACHE_DIR' docs/`; if a doc describes the cache dir, add one sentence on the owner-only refusal; otherwise no doc edit.
- [ ] **Step 7: full gate** — `nix build .#default`, `nix flake check` (includes `tests/remote-m2-integration.bats`, whose cache dir is now pre-created 0o700, and the new `./ownerdir/...` tests), `nix build .#lint` → all succeed.
- [ ] **Step 8: follow-up issues** — map writers/readers of `/tmp/claude-status` (`CLAUDE_STATUS_DIR`) and `og-agent-usage` (`OG_AGENT_USAGE_DIR`) with `rg -n 'claude-status|CLAUDE_STATUS_DIR|CLAUDE_PANES_DIR'` and `rg -n 'og-agent-usage|OG_AGENT_USAGE_DIR|usageCacheDir'`; file one GitHub issue per dir (after `gh issue list --search` dedupe) listing consumers. Mention in the claude-status issue (or a third one) that `remoteSelfCacheDirTrusted` in `picker/remote.go` is a looser Stat/0o002 variant that could adopt `ownerdir.OwnerOnly`. No code change to them.

## Acceptance

- Foreign-mode (0o777) dir and symlinked dir refused, nothing read or written → Step 1 tests, red before Step 5 / green after (red evidence recorded from Step 1 run on this branch's pre-fix code, which equals #841's behavior).
- Fresh dir created 0o700 and used → `TestLastGoodCreatesPrivateDir`.
- Foreign-owner comparison → `ownedPrivate` foreign-uid unit test (Step 2).
- `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 7.
