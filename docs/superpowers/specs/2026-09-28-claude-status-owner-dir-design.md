# Spec — per-user, owner-checked claude-status state dir (#850)

## Problem

`CLAUDE_STATUS_DIR` defaults to the fixed machine-wide `/tmp/claude-status`
(`scripts/lib-claude.sh:6`, `scripts/claude-status-update.sh:11`, and four Go
sites). Nothing checks who owns an existing directory: shell creates it with a
plain `mkdir -p` (`lib-claude.sh:230`, `:624`, `claude-status-update.sh:20`),
Go with `MkdirAll(..., 0o755)` (`agentdetect/main.go:267`,
`agentdetect/statefile/statefile.go:68`, `daemon/agentstatus.go:443`). On a
shared host another account can create `/tmp/claude-status` first (or make it a
symlink / group-writable) and then control — and read — every pane-state, task
(prompt text), name, issue and watcher file tmux-og reads and writes.

Same class as #841 (`picker/ownerdir.OwnerOnly`, statusline cache).

## Goals (from the task's "Must hold")

G1. One per-user default, derived identically in shell and Go with no
    configuration; `CLAUDE_STATUS_DIR` (non-empty) overrides it everywhere —
    including the two Go sites that today hard-code the path and ignore the
    override (`picker/agentdetect/main.go:27,45`, `picker/main.go:804`).
G2. Before trusting an existing dir: Lstat (not a symlink, is a dir), owner ==
    caller uid, `mode & 0o077 == 0`. Applies to the resolved dir whether it came
    from the default or the override (same as #841's `readLastGood`, which checks
    `OG_STATUSLINE_CACHE_DIR` too). Fail closed: no state read, none written, no
    non-zero exit / stderr from hot-path scripts or hooks. A missing dir is
    created `0700`.
G3. No new forks per tick on the per-second `#()` path in the normal (trusted)
    state.
G4. Upgrade: state under the old `/tmp/claude-status` is simply abandoned; the
    new dir starts empty; agents re-report on their next hook. No crash.

Non-goals: `og-agent-usage` (#851), `og-remote-self` (#852), `og-notify`,
deleting the old `/tmp/claude-status` (external tools still use it — see
consumer map), protecting against the same uid or root.

## Design

### Default path

`/tmp/claude-status-<uid>` — literal `/tmp`, numeric **real** uid:
shell `/tmp/claude-status-$UID` (bash builtin var, fork-free), Go
`fmt.Sprintf("/tmp/claude-status-%d", os.Getuid())`.

Considered and rejected: `$XDG_RUNTIME_DIR/claude-status` (the #841 statusline
choice). That cache has a single reader/writer binary, so env divergence is
harmless there. Here ~10 independent processes must agree — tmux `#()` jobs,
`-B` hooks, Claude/Codex/Cursor hook commands in panes, the bridge daemon,
agent-detect watchers, the picker popup, the home-manager activation reload —
and `XDG_RUNTIME_DIR` / `TMPDIR` are exactly the variables that differ between
an ssh login, a systemd/launchd-spawned process and a GUI session (macOS has no
`XDG_RUNTIME_DIR`; `os.TempDir()` there is a per-session `$TMPDIR`). A split
would silently show no status. The cost of a fixed `/tmp` name is that another
account can pre-create it: that is a denial of service, and G2 turns it into
"no status shown", never into trusted foreign state.

### Trust check

Go — new package `picker/claudestatus` (module `github.com/noamsto/tmux-og/picker`):

- `DefaultDir() string` — the default above.
- `Dir() string` — `CLAUDE_STATUS_DIR` if non-empty, else `DefaultDir()`.
- `Ensure(dir string) bool` — `os.MkdirAll(dir, 0o700)` then
  `ownerdir.OwnerOnly(dir)`. (MkdirAll on an existing dir changes nothing, so a
  foreign/loose dir is never "repaired", only refused.)
- Readers call `ownerdir.OwnerOnly(dir)` (one Lstat syscall, no fork) on each
  use; writers call `Ensure`.

Shell — in `scripts/lib-claude.sh`, evaluated once at source time (= once per
process):

- `CLAUDE_STATUS_DIR="${CLAUDE_STATUS_DIR:-/tmp/claude-status-$UID}"`.
- `claude_status_dir_trusted DIR` — fork-free fast path:
  `[[ -d $d && ! -L $d && -O $d && -e $d/.owner-only ]]`. The `.owner-only`
  marker is a cached positive verdict: it is written only after a full check
  (below) passed. It cannot be forged by another account: a dir owned by us
  (`-O`) that passed `mode&077==0` when the marker was written can only have its
  mode or contents changed by our uid or root; a dir owned by anyone else fails
  `-O` before the marker is looked at; a symlink fails `-L`.
  When the fast path misses but the dir exists, is not a symlink and `-O`
  holds, run the full check once (`$OG_STAT -c '%u %a'` — GNU stat, already
  substituted as `@stat@` for lib-claude) and on pass write the marker; this
  forks once per dir lifetime, not per tick. A foreign-owned or symlinked dir
  is refused without any fork. (Own-uid dir with a loose mode — only reachable
  by our own uid's deliberate chmod or a foreign-to-tmux-og creator — re-runs
  the stat each process and stays refused; documented as a degenerate,
  self-inflicted state.)
- `claude_status_dir_ensure` — for writers: `mkdir -p -m 700 "$dir"` when
  missing, then the full check + marker. Returns non-zero when untrusted.
- Fail closed: when the source-time verdict is false, every derived variable
  (`CLAUDE_PANES_DIR`, `CLAUDE_SCREEN_DIR`, … `CLAUDE_LIVE_DIR`) is pointed under
  a root that can never exist or be created (`/dev/null/claude-status` — any
  path component under a character device is ENOTDIR), and a flag
  `CLAUDE_STATUS_TRUSTED` is empty. Readers and deleters then find nothing
  without per-function guards (`[[ -f ]]`-guarded globs are empty; `rm -f` on
  ENOTDIR is silent). The few shell **writers** check the flag / call
  `claude_status_dir_ensure` explicitly and return quietly:
  - `claude-status-update` (top of script, replaces `mkdir -p "$PANES_DIR"`):
    ensure, else `exit 0` — a hook must never error.
  - `claude_prune_stale_state` (server start): ensure first; on failure return 0
    without pruning or writing `.server_start`.
  - `tmux-update-icons` live stamps (`mkdir -p "$CLAUDE_LIVE_DIR"`, `printf >`):
    skipped unless trusted.
  - `read_pane_state` interrupt-verdict stamp: skipped unless trusted (it only
    runs on a trusted path anyway, since the pane file was read from the root).
  A writer that ensures successfully re-derives the subdir variables so the
  rest of the same process sees the real root.
- `claude-status-update.sh` duplicates the default derivation today (its raw
  bats run does not source lib-claude). Keep one derivation in shell: move the
  default + check into lib-claude and have claude-status-update use them; for
  the raw-bats fallback path (`@lib_claude@` unsubstituted), source
  `scripts/lib-claude.sh` relative to the script, or pin both copies with the
  shared vector test (plan decides; the vector test pins every copy either way).

### Consumer map (dispositions)

| Consumer | Role | Disposition |
|---|---|---|
| `scripts/lib-claude.sh` (vars, prune, reap, clear, read_pane_state, reap_dead_panes, interrupt stamp) | R/W/delete | **changed**: default, source-time check, ensure in prune, gated interrupt write |
| `scripts/claude-status-update.sh` (CC/Codex/Cursor hooks; `mark-seen` tmux hooks) | writer | **changed**: ensure-or-exit-0; drop own default |
| `scripts/cursor-status-hook.sh` | execs claude-status-update | compatible (no path) |
| `scripts/tmux-update-icons.sh` | reader + live/ writer + reap | **changed**: live stamp gated on trust; reads via lib vars |
| `scripts/claude-status.sh`, `tmux-kill-pane-guard.sh`, `tmux-reap-pane.sh`, `tmux-shell-prompt.sh` | readers/deleters via lib vars | compatible (inherit lib gate); covered by existing suites |
| `scripts/lib-notify.sh` | own `OG_NOTIFY_DIR` | out of scope (comment reference only) |
| `scripts/tmux-carousel-restore.sh` | no status-dir access (comment ref) | compatible |
| `picker/statusline` (`main.go:451`) | reader (per-second `#()`) | **changed**: `claudestatus.Dir()` + `OwnerOnly` → empty segment when untrusted |
| `picker/main.go:804` (pickers) | reader, **hard-coded path** | **changed**: `claudestatus.Dir()` + `OwnerOnly` |
| `picker/agentdetect` (`main.go:27,45`, `statefile`) | writer, **hard-coded path** | **changed**: root from `claudestatus.Dir()`; `Ensure(root)` before `registerWatcher` (existing "unusable FS → exit" path); subdir MkdirAll stays |
| `picker/remotebridge/daemon/agentstatus.go` | writer/deleter | **changed**: `claudestatus.Dir()`; `Ensure(root)` gates each write batch (no root creation at 0755 via `MkdirAll(filepath.Dir(path))`) |
| `generator/paths/paths.go`, `generator/doctor/checks.go` | binary names only | compatible |
| `modules/home-manager.nix` | no path reference (verified: no `CLAUDE_STATUS` / `/tmp/claude-status`) | compatible |
| `claude-plugin/scripts/status.sh` | execs claude-status-update | compatible |
| `flake.nix` isolation assertion + comment (`:1532-1548`) | test guard | compatible; comment text updated |
| `tests/*.bats` (38 files set `CLAUDE_STATUS_DIR`) | fixtures | **changed where they pre-create the root**: create it `0700` (a shared helper) — otherwise the new check (correctly) refuses their 0755 fixture |
| external `aeye` (carousel/diagrams, `${AEYE_DIR:-${CLAUDE_STATUS_DIR:-/tmp/claude-status}}/images`) | independent writer/reader of `images/` | compatible-by-separation: tmux-og does **not** export `CLAUDE_STATUS_DIR`, so aeye keeps its own default and never creates (possibly loosely) the new root. `claude-status-update`'s `rm images/<pane>.jsonl` becomes a no-op unless the user sets a shared override. Follow-up issue: aeye adopts the per-user owner-checked default. |
| docs: `docs/agents/agent-state.md`, `scripts.md`, `performance.md` (if it lists forks), `CLAUDE.md` scratch-server line, `README.md:501`, `claude-plugin/README.md` troubleshooting `cat /tmp/claude-status/panes/*` | reference | **changed** |

Why not export `CLAUDE_STATUS_DIR` from the tmux wrapper so external tools
follow automatically: an external tool creating the root first with `mkdir -p`
under a 022/002 umask makes it 0755/0775, which G2 must refuse — status would
silently die. Keeping external tools off the new path avoids that race.

## Acceptance tests

- Go `picker/claudestatus`: `Dir` override/default; `Ensure` creates 0700 on a
  fresh path; refuses a 0755 own dir, a symlink to a 0700 dir, a regular file.
  (Foreign uid is covered by `ownerdir.ownedPrivate`'s fake-FileInfo test.)
- Go consumers: statusline segment empty and daemon writes nothing for a loose
  root; picker collect returns nil.
- bats (`tests/claude-status-dir.bats`, new): `claude-status-update processing`
  with a pre-created 0755 root → exit 0, no pane file; symlinked root → same;
  missing root → created 0700 with the pane file; lib-claude readers
  (`read_pane_state`) see nothing under a loose root even with a planted pane
  file; fork-free steady state: marker present → trusted with no `stat` on PATH.
  Red on main (writes into the loose/symlinked root), green with the fix.
- Shared default vector `picker/claudestatus/testdata/default-dir.txt` (template
  `/tmp/claude-status-{uid}`), asserted by the Go test and by bats against
  `lib-claude.sh` (unset `CLAUDE_STATUS_DIR`) — mirrors the
  `mirrorname/testdata/vectors.tsv` pattern.
- `nix build .#default`, `nix flake check`, `nix build .#lint` green.

## Revisions after spec-critic (accept, non-blocking findings folded in)

- `tmux-update-icons`: agent-detect **arming** (pipe-pane) is also gated on
  `CLAUDE_STATUS_TRUSTED` (fork-free), not just the live stamp — otherwise an
  untrusted root turns into a pipe-pane + exec respawn loop every 5th tick per
  agent pane (agentdetect exits before `registerWatcher`). The #692 occupancy
  (option-only) pass keeps running.
- Documented divergence: once `.owner-only` exists, shell keeps trusting a dir
  our own uid later loosened, while Go's per-use `OwnerOnly` refuses it.
  Self-inflicted, not reachable by another uid.
- Documented upgrade skew: an already-running bridge daemon and existing
  agent-detect watchers keep writing the old path until they reconnect /
  are re-armed; panes whose PATH puts an old wrapper bin first run the old
  hook until restarted.
- `read_pane_state`'s interrupt stamp needs no extra gate: it is only reached
  after a pane file was read from a trusted root.
- A doctor check for an untrusted root is out of scope (`generator` is a
  separate Go module that cannot import `picker/claudestatus`); filed as a
  follow-up.
