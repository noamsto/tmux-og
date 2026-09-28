# Plan — per-user, owner-checked claude-status state dir (#850)

Spec: `docs/superpowers/specs/2026-09-28-claude-status-owner-dir-design.md` (accepted). Orchestration consult: survey did
not trip — one invariant (resolve + owner-check one dir), shell lib + four Go
call sites + fixtures; the decomposition is linear, no cross-cutting interface
beyond the dir contract itself.

Constants fixed across all steps (the interface every step shares):

- Default: shell `/tmp/claude-status-$UID`; Go `fmt.Sprintf("/tmp/claude-status-%d", os.Getuid())`.
- Override: `CLAUDE_STATUS_DIR` when non-empty.
- Trust = not a symlink, is a dir, owner == caller uid, `perm & 0o077 == 0`.
- Shell cached-verdict marker: `<root>/.owner-only` (empty file).
- Shell fail-closed root for derived vars: `/dev/null/claude-status`;
  flag `CLAUDE_STATUS_TRUSTED` = `1` or empty.
- Shared vector: `picker/claudestatus/testdata/default-dir.txt`, one line
  `/tmp/claude-status-{uid}`.

## Step 1 — Go package `picker/claudestatus` (implement: sonnet)

Files: `picker/claudestatus/claudestatus.go`, `claudestatus_test.go`,
`testdata/default-dir.txt`.

- `DefaultDir() string`, `Dir() string`, `Ensure(dir string) bool`
  (`os.MkdirAll(dir, 0o700)` → `ownerdir.OwnerOnly(dir)`), `Trusted(dir string) bool`
  (= `ownerdir.OwnerOnly`; exists so consumers import one package).
- Tests: `DefaultDir` equals the vector with `{uid}` → `os.Getuid()`; `Dir`
  honours a set override and falls back on empty; `Ensure` on a fresh nested
  path → true and `perm == 0o700`; `Ensure` on an existing 0755 dir → false and
  mode unchanged (no repair); symlink → a 0700 dir → false; regular file → false.
- Gate: `cd picker && go test ./claudestatus/ ./ownerdir/`.

## Step 2 — Go consumers (implement: sonnet; depends on 1)

- `picker/statusline/main.go:451-454`: `claudeDir := claudestatus.Dir()`; if
  `!claudestatus.Trusted(claudeDir)` set `claudeDir = ""` and make sure every
  reader of `claudeDir` (`sessionLiveIDs`, `claudeSegment`, `aggregateSession`
  in `claude.go`) yields no agent state for `""` (verify: `os.ReadDir("/panes")`
  would read the host's `/panes` — so rather than "" pass through, short-circuit:
  the segment renders empty and `sessionLiveIDs` still returns the live-pane map
  it needs; read the call sites and pick the minimal gate). Test: loose (0755)
  root with a planted `panes/<id>` → segment has no agent glyph; 0700 root → it
  does (existing tests already cover the positive path).
- `picker/main.go:802-806` `collectAgentPanes`: root := `claudestatus.Dir()`;
  `!Trusted` → `return nil`; else join `panes`/`screen`/`issues`. Fix the doc
  comments at `:689`, `:698` that name `/tmp/claude-status`.
- `picker/agentdetect/main.go`: drop the `stateDir`/`watcherRegDir` consts; in
  `main()`, `root := claudestatus.Dir()`; `if !claudestatus.Ensure(root) { return }`
  immediately before `registerWatcher` (same exit contract as today's
  unusable-FS path); pass `filepath.Join(root, "watchers")` / `"screen"` to the
  existing calls. Subdir `MkdirAll(..., 0o755)` inside the trusted root stays.
- `picker/remotebridge/daemon/agentstatus.go`: `newAgentShipper` uses
  `claudestatus.Dir()`; the write/remove path (`apply` at ~`:330-360` and the
  clear at ~`:420-426`) is gated once per batch by `claudestatus.Ensure(a.dir)`
  — untrusted → skip the whole batch (write nothing, remove nothing). Read
  the agentstatus tests: `t.TempDir()` is **0755** (`os.Mkdir(dir, 0777)&^umask`;
  see `ownerdir_test.go:50`, `statusline/main_test.go:221`), so every
  `&agentShipper{dir: t.TempDir()}` fixture in `agentstatus_test.go`
  (`:63,118,159,197,222,261,286,315`) must use a new `privateDir(t)` helper
  (t.TempDir + chmod 0700). Add one test on a plain (0755) `t.TempDir()`: loose
  root → no files written. Also chmod 0700 the dirs passed as
  `CLAUDE_STATUS_DIR=` to `startIsolatedTmux` in
  `remotebridge/daemon/{sessionres,floatgeom,deadkey,ctl,agentusage}_test.go`
  (keeps those servers on the production path, not fail-closed) and reword the
  `sessionres_test.go:248` comment naming `/tmp/claude-status`. Grep all of
  `picker/` for `CLAUDE_STATUS_DIR` / `claudeDir` fixtures and apply the same.
- Gate: `cd picker && go vet ./... && go test ./statusline/ ./agentdetect/... ./remotebridge/daemon/ .` (package-scoped).

## Step 3 — shell lib + writers (implement: opus — security + per-second hot path; independent of 1–2)

`scripts/lib-claude.sh`:

- Line 6: `CLAUDE_STATUS_DIR="${CLAUDE_STATUS_DIR:-/tmp/claude-status-$UID}"`.
- Move the `OG_STAT` block (currently ~`:180`) above the new functions.
- `claude_status_dir_trusted` (fork-free fast path + once-per-dir full check,
  per spec "Trust check"), `claude_status_dir_ensure` (`mkdir -p -m 700` when
  missing — no chmod of an existing dir — then trusted check), and
  `claude_status_dirs ROOT` which assigns every derived `CLAUDE_*_DIR` var
  (panes, screen, issues, tasks, names, interrupt, watchers, live) from ROOT.
- Source time: `if claude_status_dir_trusted; then CLAUDE_STATUS_TRUSTED=1;
  claude_status_dirs "$CLAUDE_STATUS_DIR"; else CLAUDE_STATUS_TRUSTED=;
  claude_status_dirs /dev/null/claude-status; fi`. `claude_status_dir_ensure`
  on success sets the flag and re-derives from the real root.
  Full check: `read -r uid mode < <("$OG_STAT" -c '%u %a' -- "$d")` is a
  process substitution (fork) — acceptable only on the miss path; compare
  `uid == $UID` and `(( 8#$mode & 8#077 == 0 ))`. Marker write `: >"$d/.owner-only" 2>/dev/null`.
- `claude_prune_stale_state`: call `claude_status_dir_ensure || return 0`
  before touching `.server_start` (first statement after the empty
  `server_start` guard); drop the later `mkdir -p "$CLAUDE_STATUS_DIR"`.
- Comments that say "bare /tmp path shared by every tmux server" (`:106`,
  `:150`, `:265`, `:358`, `:379`, and in `tmux-update-icons.sh`,
  `tmux-shell-prompt.sh`): still true per user (every server of this uid shares
  it) — reword "bare /tmp path shared by every tmux server on the machine" →
  "per-user dir shared by every tmux server of this uid" only where the wording
  is now false; keep edits minimal.

`scripts/claude-status-update.sh`:

- Move the `@lib_claude@` source block above the dir variables. Raw fallback
  branch: `source "${BASH_SOURCE[0]%/*}/lib-claude.sh"` then keep the
  `claude_progress_emit() { :; }` stub (preserves today's raw-bats behaviour).
- `STATE_DIR="$CLAUDE_STATUS_DIR"`; replace `mkdir -p "$PANES_DIR"` with
  `claude_status_dir_ensure || exit 0` followed by `mkdir -p "$PANES_DIR"`.
  Verify every subcommand path (`mark-seen`, `issue`, `task`, `name`, `enrich`,
  state writes) runs after that line; if any runs before it, move the ensure.

`scripts/tmux-update-icons.sh` `arm_agent_detect`:

- After the existing `arm`/`stamp` computation:
  `[[ -n $CLAUDE_STATUS_TRUSTED ]] || stamp=0` (live stamps need a trusted
  root) and refuse arming only for a root that is **present and untrusted**:
  `[[ -n $CLAUDE_STATUS_TRUSTED || ! ( -e $CLAUDE_STATUS_DIR || -L $CLAUDE_STATUS_DIR ) ]] || arm=0`
  (fork-free). A **missing** root keeps arming, so agentdetect's own `Ensure`
  creates it 0700 — the `-B` sweep path (`OG_TICK_SWEEP`) never runs
  prune/ensure and is the only driver on a bridge-only host (#603). No reap
  change (`claude_reap_dead_panes` already sees the sentinel dirs). The #692
  occupancy pass still runs.
- bats (Step 4 case 9): missing root + sweep caller still arms (pipe-pane
  invoked on a fake agent pane — reuse `agent-liveness.bats`' arming fakes);
  present 0755 root → does not arm.

Gates: `shellcheck scripts/lib-claude.sh scripts/claude-status-update.sh scripts/tmux-update-icons.sh`;
`shfmt -d` on the same; then Step 4/5 bats.

## Step 4 — new bats suite + flake wiring (implement: sonnet; depends on 3, and 1 for the vector file)

`tests/claude-status-dir.bats` (every case exports `CLAUDE_STATUS_DIR` under
`$BATS_TEST_TMPDIR` except the default-path case, which runs in a
`bash -c` subshell with it unset and only echoes the variable):

1. default path: `env -u CLAUDE_STATUS_DIR bash -c 'source scripts/lib-claude.sh; printf %s "$CLAUDE_STATUS_DIR"'`
   equals the vector line with `{uid}` → `$(id -u)`; vector read from
   `${CLAUDE_STATUS_DIR_VECTOR:-$BATS_TEST_DIRNAME/../picker/claudestatus/testdata/default-dir.txt}`.
2. `claude-status-update processing --pane %9 --session s` (raw
   `bash scripts/claude-status-update.sh`, `TMUX` unset — check the args the
   existing `claude-issues.bats` uses to reach the state write without tmux)
   with a pre-created **0755** root → status 0, no `panes/9`.
3. same with the root a **symlink** to a 0700 dir → status 0, no file in the target.
4. same with a **missing** root → created, `stat -c %a` = `700`, `panes/9` present.
5. lib readers: 0755 root with a planted `panes/9` → after sourcing,
   `CLAUDE_STATUS_TRUSTED` empty and `read_pane_state` on it returns non-zero /
   `CLAUDE_PANES_DIR` is not under the root.
6. steady state is fork-free: 0700 root with `.owner-only` present, `PATH` set
   to an empty dir, source lib-claude → trusted (no `stat` reachable).
7. marker written: 0700 root without marker → sourcing makes it trusted and
   creates `.owner-only`.
8. prune on an untrusted root writes no `.server_start`.
9. arming gate (see Step 3): missing root arms; present 0755 root does not.

Red-on-main evidence: in a scratch `git worktree add` of `origin/main` (never
this tree), copy the new bats file in and run it; record which cases fail
(2, 3, 4, 5, 6/7 expected red; 1 red since main's default is
`/tmp/claude-status`). Then green on the branch.

`flake.nix`: add a `claude-status-dir-tests` check (pattern of
`claude-issues-tests`, `nativeBuildInputs = [bats coreutils]`,
`CLAUDE_STATUS_DIR_VECTOR = ./picker/claudestatus/testdata/default-dir.txt`).
Check whether flake.nix has a "every tests/*.bats is registered" assertion and
satisfy it. Update the isolation-assertion comment (`:1532-1540`) that names
`/tmp/claude-status`.

## Step 5 — existing fixtures (implement: sonnet; depends on 3)

For each test file that sets `CLAUDE_STATUS_DIR` (38; 23 create it with
`mkdir`), make the root owner-only **before** anything sources lib-claude or
runs a script: right after the `export CLAUDE_STATUS_DIR=…` line add
`mkdir -p -m 700 "$CLAUDE_STATUS_DIR"` — `mkdir -p -m` applies the mode only
to the last component, which is the root. Where a file sources lib-claude and
*then* creates dirs through the derived vars (`$CLAUDE_PANES_DIR` etc.), the
new line already precedes the source, so the vars point at the real root.
Also `tests/perf/keystroke-latency.sh` (`mkdir -p "$CLAUDE_STATUS_DIR"` →
add `-m 700` path), `tests/reflow-race-e2e.sh` (root/status), and
`tests/test-display.sh` (mktemp -d is already 0700 — verify only). Files that
never pre-create the root and let `claude-status-update` create it need no
change. Go fixtures are handled in Step 2 (`t.TempDir()` is 0755).

Gate: run every bats suite that sets `CLAUDE_STATUS_DIR` locally
(`bats tests/<f>.bats`) — suites needing `TMUX_BIN` run as in the flake;
failing suites are fixed here. Then `nix flake check`.

## Step 6 — docs (implement: sonnet; after 3)

- `docs/agents/agent-state.md`: title + every `/tmp/claude-status/...` →
  `$CLAUDE_STATUS_DIR` (default `/tmp/claude-status-<uid>`); new bullet
  "Location and trust": default, override, owner-only check (shell fast path
  + `.owner-only` cached verdict + once-per-dir stat; Go per-use Lstat),
  fail-closed sentinel, upgrade/version-skew note, the shell-vs-Go divergence
  on a dir our own uid later loosened, aeye stays on its own default.
- `docs/agents/scripts.md`: rows for `claude-status-update` / lib-claude if
  they name the path or dir creation.
- `docs/agents/performance.md`: only if it enumerates per-tick forks of
  lib-claude sourcing — add "steady state: zero forks for the trust check".
- `CLAUDE.md:12` table cell and `:86` scratch-server sentence; `README.md:501`;
  `claude-plugin/README.md:89,110,121` (`cat /tmp/claude-status/panes/*` →
  `cat /tmp/claude-status-$(id -u)/panes/*`, or `${CLAUDE_STATUS_DIR:-…}`).
- Leave `CHANGELOG.md` and `docs/plans|superpowers` history untouched.
- Commit this plan as `docs/superpowers/plans/2026-09-28-claude-status-owner-dir.md`
  and the spec as `docs/superpowers/specs/2026-09-28-claude-status-owner-dir-design.md`
  (CLAUDE.md "Plans and Specs").

## Final gate (worker)

`nix build .#default`, `nix flake check`, `nix build .#lint`; then review gate.

## Risks

- A test file that sources lib-claude before exporting `CLAUDE_STATUS_DIR`
  would now resolve the real per-user dir; Step 5 must check ordering in each
  file, not only add the mkdir.
- `mkdir -p -m 700` on macOS BSD mkdir: same semantics (mode on the final
  component) — fine for tests; production uses the same form in
  `claude_status_dir_ensure`.
- `$UID` is bash-only; lib-claude is bash — fine.
