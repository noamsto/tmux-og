# Per-user, owner-checked agent-usage cache dir (#851)

The agent-usage cache defaulted to a machine-wide `/tmp/og-agent-usage` that
the shell providers created with `mkdir -p` and the Go statusline read with no
ownership check, so another local account could create it first and feed the
statusline its own figures. Invariant: **usage data is only read from, or
written to, a directory that is a real directory (not a symlink), owned by the
caller's uid, with no group/other permission bits** — and both sides agree on
where that directory is.

## Consumer map

| Edge | Symbol / file | Disposition |
|---|---|---|
| Producer (dispatcher) | `scripts/tmux-agent-usage.sh` `CACHE_DIR`, tick/tick-run `mkdir -p`, `.last-tick`, cache `rm -f`, locks, `publish_usage` | **changed** — resolves the per-user default, creates `0700`, `owner_only_dir` gate before any read/write, exports the checked path to providers |
| Producers (providers) | `scripts/tmux-agent-usage-{claude,codex,cursor,pi}.sh` `CACHE_DIR` + `mkdir -p` | **changed** — no own default or `mkdir`; write only into the `OG_AGENT_USAGE_DIR` the dispatcher exported, exit 0 if unset |
| Shared shell helper | `scripts/lib-log.sh` (stat home, sourced by the dispatcher) | **changed** — new `owner_only_dir DIR [FILE]`, one `stat` fork, FILE's mtime in `REPLY` |
| Consumer (Go) | `picker/statusline/usage.go` `usageCacheDir`, `loadUsageCaches`; `main.go:476` | **changed** — `usageCacheDir()` per-user resolver honouring `OG_AGENT_USAGE_DIR`; `loadUsageCaches` returns empty unless `ownerdir.OwnerOnly(dir)` |
| Strict check | `picker/ownerdir.OwnerOnly` | **compatible** — reused as is |
| Bridge daemon | `@og_agent_usage` option → `@bridge_usage` (`bridge-shipped-state.md`) | **compatible** — reads the tmux option `publish_usage` writes, never the dir; `publish_usage` now only runs past the owner gate |
| Mirror render | `usageFor` bridge branch (`parseBridgeUsage`) | **compatible** — never touches the dir |
| Nix wiring | `config/tmux.conf.nix` `mkScriptAgentUsage` (substitutes `@lib_log@`), providers are plain `mkScript` | **compatible** — no new placeholder; comment at `:93` updated |
| Isolation assertion | `flake.nix` `wrapped-tmux-suite-isolation-assertions` | **compatible** — still requires `export OG_AGENT_USAGE_DIR=` |
| Tests pre-creating the dir | `tests/agent-usage-gate.bats`, `tests/remote-m2-integration.bats`, `tests/reflow-mirror-attach.bats`, `docs/media/demo.sh` | **changed** — `mkdir -m 700` so a seeded cache is still trusted |
| Tests only isolating | the other `export OG_AGENT_USAGE_DIR=` bats files | **compatible** — dir is created 0700 by the poller itself |
| Docs | `docs/agents/{enrichment,bridge-shipped-state,scripts}.md` | **changed** — name the per-user path |

## Design

- **Default path** (both sides): `$XDG_RUNTIME_DIR/og-agent-usage-<uid>` when
  `XDG_RUNTIME_DIR` is absolute, else `${TMPDIR:-/tmp}/og-agent-usage-<uid>`
  (Go: `os.TempDir()`), trailing `/` of the base dropped so the shell matches
  `filepath.Join`. `OG_AGENT_USAGE_DIR` overrides it on both sides. Same shape
  as `statuslineCacheDir` (#841); Go factors the resolver into one
  `perUserDir(env, name)` shared by both caches.
- **Shell owner check**: `owner_only_dir DIR [FILE]` runs one
  `"$OG_STAT" -c '%u %a %Y %F' -- DIR [FILE]` (GNU stat's default is lstat, so
  a symlink reports `symbolic link`); true when uid = `$UID`, type
  `directory`, `mode & 077 == 0`; sets `REPLY` to FILE's mtime (0 if absent).
- **Fork budget on the `-B` tick**: the fresh-stamp fast path already forks one
  `stat` (`file_mtime .last-tick`); it becomes the single `owner_only_dir` stat
  over dir + stamp — same count. The no-stamp path forks nothing new before the
  `list-panes` gate. The create-and-check (`mkdir -p -m 700` + `owner_only_dir`)
  runs only on the daemonize path, once per refresh window.
- **Fail closed**: an untrusted dir makes tick/tick-run `exit 0` (no poll, no
  publish, no rm) and the Go reader render no usage segment.

## Files

- `scripts/lib-log.sh` — add `owner_only_dir`.
- `scripts/tmux-agent-usage.sh` — resolver, gate, `0700` create, export.
- `scripts/tmux-agent-usage-{claude,codex,cursor,pi}.sh` — use the exported dir.
- `picker/statusline/usage.go`, `picker/statusline/main.go` — resolver + gate.
- `picker/statusline/usage_test.go`, `picker/statusline/main_test.go` — Go tests.
- `tests/agent-usage-gate.bats`, `tests/agent-usage-providers.bats` — bats tests.
- `tests/remote-m2-integration.bats`, `tests/reflow-mirror-attach.bats`, `docs/media/demo.sh` — `mkdir -m 700`.
- `config/tmux.conf.nix` (comment), `docs/agents/{enrichment,bridge-shipped-state,scripts}.md`.

## Steps

- [ ] **Step 1: failing bats tests** (`tests/agent-usage-gate.bats`): (a) with
  `OG_AGENT_USAGE_DIR` unset, `XDG_RUNTIME_DIR=$T/xdg`, an open claude pane,
  `--tick` creates `$T/xdg/og-agent-usage-$UID` mode `700`; with
  `XDG_RUNTIME_DIR` unset and `TMPDIR=$T/tmp/` it creates
  `$T/tmp/og-agent-usage-$UID`; (b) a pre-existing `0755` dir with a seeded
  `claude.json`: `--tick-run` runs no provider, publishes nothing, leaves the file;
  (c) a symlink to a `0700` dir: same refusal; (d) `--tick` with a fresh stamp in
  a `0755` dir does not short-circuit into trusting it (exits 0, no provider, no
  stamp rewrite); (e) existing seeded tests switch to `mkdir -m 700`. Providers
  test (`tests/agent-usage-providers.bats`): `mkdir -m 700` the cache in setup;
  a provider with `OG_AGENT_USAGE_DIR` unset writes nothing under `/tmp`.
  Run `bats tests/agent-usage-gate.bats tests/agent-usage-providers.bats` — red.
- [ ] **Step 2: failing Go tests** (`picker/statusline`): `usageCacheDir()`
  override, XDG default, TMPDIR fallback (incl. trailing slash) all end in
  `og-agent-usage-<uid>`; `loadUsageCaches` returns empty for a `0755` dir and
  for a symlink to a `0700` dir, reads a `0700` dir. Existing
  `loadUsageCaches` tests chmod their dir `0700`. `go test ./statusline/...`
  (in `picker/`) — red.
- [ ] **Step 3: shell impl** — `owner_only_dir` in `lib-log.sh`; dispatcher
  resolver/gate/export per Design; providers read `OG_AGENT_USAGE_DIR` (exit 0
  if unset), drop `mkdir -p`. `bats` from Step 1 — green; `shellcheck` clean.
- [ ] **Step 4: Go impl** — `perUserDir`, `usageCacheDir()`,
  `statuslineCacheDir()` via it, `ownerdir.OwnerOnly` in `loadUsageCaches`,
  `main.go` calls `usageCacheDir()`. `go test ./...` in `picker/` — green.
- [ ] **Step 5: callers & docs** — `mkdir -m 700` in remote-m2 / reflow-mirror /
  demo.sh; doc + comment updates.
- [ ] **Step 6: gate** — `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Acceptance

- Tests red on main, green with the fix → Step 1/2 red runs recorded, Step 3/4 green.
- Three nix commands pass → Step 6.
- `docs/agents/enrichment.md` path updated → Step 5.
