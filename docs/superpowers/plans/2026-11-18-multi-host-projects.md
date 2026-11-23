# Multi-host projects: implementation plan (#1180)

Design: `docs/superpowers/specs/2026-11-18-multi-host-projects-design.md`. Read
it first; this plan cites its sections as "spec N".

**Goal:** one local tmux session per project (the hub) holding the project's
windows from every host, each new window opened on the host that owns its
worktree, and pickers that show the project across hosts.

**Architecture:** a policy layer over the existing placed (`--into`) mirrors:
a Go package for project identity and per-host snapshots, an `og project` verb
that routes at entry points, a launcher guard that makes "one mirror per remote
leaf" real, then picker surfaces over cached snapshots. The bridge daemon does
not change.

**Repo conventions this plan follows** (from `CLAUDE.md`): shell scripts are
bash and run `shellcheck`/`shfmt`; a bats check copies only the files it reads
(`testsSrc [...]`/`scriptsSrc [...]`); every tmux repro runs on its own server
(`TMUX_TMPDIR=/tmp/og-$$ CLAUDE_STATUS_DIR=… XDG_DATA_HOME=… tmux -L probe`);
background side effects run from `-B` monitor hooks; a change that alters
behaviour a `docs/agents/*.md` file describes updates that doc in the same PR;
new subsystem detail goes in `docs/agents/`, never `CLAUDE.md`; commit from
inside `nix develop`.

## The gate, every phase

```bash
nix build .#default   # the wrapped tmux
nix flake check       # bats + Go tests (-race) + golangci-lint + nilaway + conf assertions
nix build .#lint      # alejandra, statix, deadnix, shellcheck, shfmt, typos, markdown
```

None subsumes another. A Go-only edit may iterate with
`cd picker && go test -race ./project/... ./remoteopen/...` first, but the three
commands above are the acceptance gate. A phase is done when all three pass and
its own criteria hold.

## Phase map

| Phase | Ships | v1 | Depends on |
|---|---|---|---|
| P0 | Verification spike (facts the spec marks "verify") | yes | none |
| P1a-0 | Behaviour-preserving extraction of the picker's probe/cache/sanitize helpers into a shared package | yes | P0 |
| P1a | Project identity, registry, host snapshots, `og project status` (read-only) | yes | P1a-0 |
| P1b | Routing: hub session, leaf pickup, `og project open/attach/detach`, launcher guard | yes | P1a |
| P1c | `og project new`: fresh-origin worktree, same-branch refusal | yes | P1b |
| P2a | Snapshot cache, `@og-project-tick`, session-picker host strip and drift | yes | P1a |
| P2b | Worktree picker and new-worktree flow | yes | P1b, P1c, P2a |
| P3 | Host-grouped tab ordering (opt-in) | no | P1b |
| P4 | `adopt`, `--offline`, hub adoption (D9), bridge-shipped snapshot | no | P2b |

P1a, P1b, P1c, P2a and P2b are each independently shippable and each leaves the
product working: after P1a the user can read their project map, after P1b they
can route opens by CLI, after P1c they can create worktrees, after P2 the
pickers show it. P3 and P4 are optional follow-ups the owner may drop.

## P0: Verification spike (no production code)

The spec names facts it cannot settle from the docs. Settle them on a scratch
server and a real second host (or `ssh localhost`) before P1a, and record each
answer as a short "P0 findings" list at the end of the spec (docs-only change).

- [ ] **Step 1:** `wt switch --help` and a throwaway repo: confirm
  `wt switch -c <branch> --base <ref>` creates from `<ref>` and that the
  worktree path follows the host's `worktree-path` template.
- [ ] **Step 2:** `wt` on `PATH` for a non-interactive `ssh host -- sh -c 'command -v wt'`
  (the per-user profile rule in `bridge-daemon.md`, "What the Remote Host Needs
  on PATH"). Record whether `/etc/profiles/per-user/<user>/bin/wt` fallback is
  needed, as `remoteZoxideScript` does for zoxide.
- [ ] **Step 3:** read `picker/remoteopen/remote.go` and `launcher.go`: the
  launcher's remote calls are `list-sessions -F '#{session_name}' | head -1`
  and `has-session -t =<sess>` (`rm.ssh`, `rm.env()`). Record where the stamp read slots in beside `hasSession`. `show-options` rejects the `=` prefix (`tmux-gotchas.md`, #474), so confirm on a scratch server of the pinned tmux that `display-message -p -t '=<sess>:' '#{session_id}'` answers a `$N` and that `show-options -qv -t '$N' @og_project_leaf` prints an empty value (not an error) for an unset option and the stamp for a set one. Also confirm `new-session -d -P -F '#{window_id}|#{pane_id}' -s <leaf> -c <path> \; set-option -t '<leaf>:' @og_project_leaf <key>` prints the ids of the first window.
- [ ] **Step 4:** on a scratch server with a placed mirror, print
  `@bridge_dir`, `@bridge_pane`, `@bridge_session_path` for a mirror window and
  confirm which names the remote worktree path and which the remote pane id
  (the spec's identity key `(host, worktree path)` and `placeFocus` match).
- [ ] **Step 5:** `git remote get-url origin` with a `url.<base>.insteadOf`
  rewrite and with an ssh `Host` alias: confirm the first rewrites, the second
  does not (spec 1).
- [ ] **Step 6:** read `picker/ogconfig/ogconfig.go` and decide whether the
  declarative registry reuses it or adds a sibling reader.
- [ ] **Step 7:** read `picker/ogcli/models.go` and `model.go`: a model is a
  kong struct for flags plus a hand-written help text, and a model on a verb
  that also has subverbs fails table load. Confirm `og project <subcommand>`
  is therefore declared as one verb (`project`, no `project x` entries) and
  decide whether v1 ships it without a model (the plan assumes no model:
  `og project --help` reaches the target raw, which prints its own usage).
  **P0 result:** that assumption is wrong (a sole `--help` never reaches the
  target; see the spec's P0 findings, item 7), so P1a adds a help-only
  `project` model.
- **Acceptance:** each step has a recorded answer; any answer contradicting the
  spec is raised to the lead as a spec change before P1a starts. Gate:
  `nix build .#lint` (docs only).

## P1a-0: Extract the shared probe helpers (behaviour-preserving)

The helpers the project code needs are `package main` functions in `picker/`
and cannot be imported from another package (`remoteopen/sanitize.go` already
twins one for that reason): `hostFileName` (`remote.go`), `readOwnerOnlyJSON`,
`writeOwnerOnlyJSON`, `savedAtStale`, `classifyProbeErr` (`remote.go`),
`sanitizeStatusText` (`attach.go`), `remoteDisplayName` (`remote_wins.go`).

**Files:**

- Create `picker/probekit/{hostfile,ownerjson,stale,classify,sanitize}.go` with exported forms (`HostFileName`, `ReadOwnerOnlyJSON`, `WriteOwnerOnlyJSON`, `SavedAtStale`, `ClassifyProbeErr`, `SanitizeStatusText`, `DisplayName`) and the tests that currently cover them, moved with them.
- Modify `picker/remote.go`, `picker/attach.go`, `picker/remote_wins.go` and their callers to use `probekit`; delete the moved bodies (no thin wrappers left behind).
- Modify `picker/default.nix`: nothing (a library package, no binary).
- `remoteopen/sanitize.go` stays its own twin; if its table is not already pinned against the picker's by a shared fixture, pin both to `picker/prenrich/testdata/sanitize-title.tsv`-style shared testdata (the repo's byte-identical-tables rule).

- [ ] **Step 1:** move each function with its tests one at a time; after each move run `cd picker && go test -race ./...` (must stay green).
- [ ] **Step 2:** repoint the callers, delete the originals.
- **Acceptance:** no behaviour change: `cd picker && go test -race ./...` is green before and after with the same test count (moved, not dropped); `git diff --stat` shows deletions in `picker/*.go` matching additions in `picker/probekit/`; gate: the three commands.
- **Why a separate phase:** it is a pure refactor a reviewer can verify by "tests unchanged", and P1a, P1b and P2a all depend on it compiling.

## P1a: Identity, registry, host snapshots, `og project status`

**Files:**

- Create `picker/project/identity.go` (+ `_test.go`): `NormalizeKey(url string) (string, bool)` (returns `ok=false` for a key outside `[A-Za-z0-9._/@+:-]`), `Slug(key string, taken func(string) bool) string`, `LeafName(key string) string` (`og-<repo>-<8 hex of sha256(key)>`, derived from the key alone) (uses `picker/mirrorname`).
- Create `picker/project/registry.go` (+ test): declarative JSON load, merge with discovered, `remotes` aliases.
- Create `picker/project/probe.go` (+ test): the remote probe script (no single quote or backslash), the parser, and the `Snapshot` type (`Key`, `RepoPath`, `DefaultBranch`, `Worktrees[{Path, Branch, Head, Windows[{Session, WindowID}]}]`, `Drift`, `Err`).
- Create `picker/project/testdata/` fixtures: urls table, probe output with hostile names.
- Create `picker/project/cmd/project/main.go` (subcommands `status` now; `open`, `attach`, `detach`, `new`, `refresh` in later phases, parsed by the binary itself).
- Modify `picker/default.nix`: add `"project/cmd/project"` to the explicit `subPackages` list and `mv $out/bin/project $out/bin/og-project-bin` in `postInstall` (the `mv $out/bin/launcher $out/bin/og-remote-launcher` pattern); without both there is no binary.
- Create `scripts/og-project.sh`, a shim substituting the store path and exec'ing the pinned `@project_bin@` (the Go binary `og-project-bin`; the shim must not share its name, or `wrapperBinDir`, which holds both `scripts` and `picker-generate`, would let the shim find itself) with argv unchanged. Add a `mkProjectScript` beside `mkRemoteScript` in `config/tmux.conf.nix` substituting `@project_bin@` and the pinned `@remote_open@` path (a server's PATH is frozen, so `og-project-bin` execs `og-remote-open` by that path, passed as `--remote-open`, never by name), `shellcheck`/`shfmt` clean.
- Modify `config/tmux.conf.nix`: **one** `ogVerbSpec` entry, `"project" = { script = "og-project"; summary = "Open and route a project's windows across hosts"; model = "project"; }`, **one `project` model in `picker/ogcli/models.go` (P0 finding 7: the struct keeps the passthrough positional `Args []string` and carries the help text; the entry is `model = "project"`; a test asserts `og project --help` prints it)** and no `project x` sub-verb entries (a model on a verb with subverbs fails table load, `og-dispatcher.md`); add a `scriptsWithProject = ["og-project"]` branch to the script-builder dispatch chain (beside `scriptsWithRemote`) that calls `mkProjectScript`, and keep `og-project` out of the plain and `scriptsWithRemote` lists (those builders substitute other tokens and would leave a literal `@project_bin@`). `og project status` then reaches the binary with `status` as its first argument.
- Update the `og` table tests (`picker/ogcli` table-load test and the flake's dispatch-check) for the new verb, per `og-dispatcher.md`.
- Create `picker/project/tailscale.go` (+ test, fixture `testdata/tailscale-status.json`): optional, opt-in host source from `tailscale status --json` (P0 finding 8): `[]Peer{Name, Online, OS}` from `DNSName`'s first label, `OS` in linux/macOS, `ShareeNode` false; async, ~2 s timeout; any failure (daemon absent takes 2 s, `BackendState` other than `Running`) yields no source. Off unless `programs.tmux-og.projects.tailscale.enable` (default false); disabled or failing means no `tailscale` exec and no source. Peers are matched to configured aliases by `ssh -G` hostname. Test first: parser on the fixture, and the disabled and failing paths. Acceptance: with the source disabled or `tailscaled` absent, no `tailscale` exec occurs (disabled) and the picker behaves as today. Suggests missing hosts, never auto-adds; the configured list stays the fallback.
- Modify `modules/home-manager.nix`: `programs.tmux-og.projects.<name> = { remote; remotes; hosts.<host>.path; }`, rendered to the JSON the reader loads.
- Create `docs/agents/projects.md`; add its row to the `CLAUDE.md` doc table and a row for `og-project` in `docs/agents/scripts.md`, and a line in `docs/agents/og-dispatcher.md`.

- [ ] **Step 1 (test first):** `identity_test.go` table: ssh/https/scp forms, `.git`, trailing slash, port, mixed-case host, owner case kept, insteadOf-already-applied input, no-origin → `ok=false`, and hostile urls (`#(…)`, quotes, newline, control bytes) → `ok=false`. `LeafName` test: same key → same name on every call and process; two keys with the same repo name → different names; output only `[A-Za-z0-9_-]`. Run `cd picker && go test ./project/ -run TestNormalizeKey`, expect FAIL, implement `NormalizeKey`, expect PASS.
- [ ] **Step 2:** `Slug` test (collision → `<owner>-<repo>`; output only `[A-Za-z0-9_-]`). Implement.
- [ ] **Step 3:** probe parser test over fixtures: well-formed rows, a row with `|`/ESC/newline in a branch, a host with no checkout, a worktree with windows in two sessions, `@bridge_win` windows skipped (spec 3.6). Parser keeps only well-formed rows (the `parseRemoteWindowsOutput` rule). Implement script and parser. The script is `sh` + `git` + optional `tmux list-windows -a`, degrading silently without a server, and gathers (a) **candidate roots** from three sources, each with its own fixture: the remote tmux server's `#{@git_root}`, the host's zoxide directories (the `remoteZoxideScript` approach, `command -v` then the per-user profile path), and registry paths; each resolved with `git rev-parse --git-common-dir` and keyed by `git remote get-url origin`; (b) **drift**: the host's local default branch against `origin/<default>` as of its last fetch (`git rev-list --left-right --count`), and the true remote head from `git ls-remote origin <default>` under a hard timeout, an `ls-remote` failure leaving the field marked `stale` rather than zero. Tests cover drift `↓n`, `↑n`, diverged, up to date, and `ls-remote` failure.
- [ ] **Step 4:** registry merge test (declarative path wins; alias keys fold into one project; discovered-only host appears). Implement.
- [ ] **Step 5:** `og project status [<project>]` prints, per host: reachable, checkout path, worktree count, drift chip, using timeout constants `probekit` owns (lifted from the picker's `remoteProbeTimeout` family in P1a-0) and `probekit.ClassifyProbeErr`. Output goes through `probekit.SanitizeStatusText`. Test with a fake runner (`runner_fake_test.go` pattern in `remoteopen`).
- [ ] **Step 6:** wire Nix (derivation, shim, `ogVerbSpec`, option) and docs. Add a bats test `tests/og-project.bats` with a fake `ssh` earlier on `PATH` returning fixture probe output; add it to `flake.nix` with `testsSrc ["og-project.bats" …]`/`scriptsSrc [...]` (only the files it reads).
- **Acceptance:**
  - `og project status` against a fake two-host fixture prints both hosts, one project key, and flags a duplicate branch.
  - Hostile branch/path/remote strings never reach the output unsanitised (test).
  - A user with no `programs.tmux-og.projects` and no `@og_project` session sees no behaviour change (no new hook, no new probe).
  - Gate: the three commands above; `go test -race ./project/...` green; `golangci-lint`/`nilaway` clean via `nix flake check`.
- **Risk:** the probe is the one remote-facing surface. Keep its script free of quotes/backslashes (fish login shells), validate every echoed path against the spec 2 charset, and cap output size.

## P1b: Routing — hub, leaf pickup, `open`/`attach`/`detach`, launcher guard

**Files:**

- Create `picker/project/route.go` (+ test): `ResolveHost(snapshots, branch, currentHost) (host, ambiguous, err)` per spec 2's table.
- Create `picker/project/open.go` (+ test): ensure hub (`new-session -d`, `@og_project`), ensure leaf (absent: `new-session -d -P -F '#{window_id}|#{pane_id}' -s <leaf> -c <path> \; set-option -t '<leaf>:' @og_project_leaf <key>` in one tmux invocation, whose first window is the target window, so no stray anchor window and no unstamped instant; present with `@og_project_leaf` equal to the key: `new-window`; present with any other stamp: refuse; all remote tmux calls go through `remoteTmuxBin`/`remoteTmuxCmd` resolution with `TMUX_TMPDIR` pinned, never bare `tmux`), ensure pickup by exec'ing `og-remote-open <host> <leaf> --into <slug>` with the internal project flag (a subprocess, not an import: `picker/remoteopen` stays a separate binary), remote `new-window -P -F '#{window_id}|#{pane_id}' -t '=<leaf>:' -c <path>` (no `-n`), bounded wait for the mirror window by `@bridge_host` + `@bridge_pane`, `select-window`.
- Create `picker/project/localscan.go` (+ test): the project package's own local scan, `list-panes -a -F '#{window_id}|#{@bridge_host}|#{@bridge_session}|#{@bridge_placed}|#{@bridge_pane}'` (`list-panes`, not `list-windows`: the daemon sets `@bridge_pane` pane-scoped, so `list-windows` would see only the active pane's value, which is why `placedWindowFormat` uses `list-panes -s`) (pipe-delimited per the repo rule; `collectBridgeMirrors` is `package main` and not importable), used for the reuse-a-live-daemon step.
- Create `picker/project/lock.go`: the per-(host, leaf) lock around scan-then-start (spec 3.3 item 4), modelled on `picker/remoteopen/lock.go`.
- Create `picker/remoteopen/leafguard.go` (+ test): the guard of spec 3.2/3.3 (leaf only via `og project`, hub only via an `@N` selector, refuse the opposite selector kind on one triple).
- Modify `picker/remoteopen/remote.go` and `launcher.go`: add a `projectStamps(sess)` read beside `hasSession` (one ssh call: `display-message -p -t '=<sess>:' '#{session_id}'`, require `$N`, then session-scoped `show-options -qv -t '$N'` for the two stamps; fail-open only when the session cannot be resolved or the call cannot run, per spec 3.3 item 1 and P0 step 3; a unit test pins that an `=`-prefixed `show-options` is never used). For an `@N` id-selector open on a hub it also checks, in the same call, that window `@N` has an empty `@bridge_win` and that its `@git_root` equals the checkout root passed by the picker (spec 3.6; not `pane_current_path`), refusing otherwise; call `leafGuard` before any dial. Internal flag for `og project` only, added in `args.go` with its test.
- Modify `picker/remote.go` (`remoteSessionRowsFormat`) and `picker/remote_wins.go` (`remoteWindowRowsFormat`): hide `@og_project_leaf`/`@og_project` sessions from the Remote section with the same conditional-row trick as `@bridge_host` (cosmetic).
- Modify `docs/agents/projects.md`, `docs/agents/bridge-daemon.md` (a short "Project leaves and hubs" pointer under "Placed mirrors"), `docs/agents/scripts.md` (`og-remote-open` row), `docs/agents/picker.md` ("Remote windows" hiding rule).

- [ ] **Step 1 (test first):** `leafguard_test.go` over `projectStamps` output: leaf + non-project caller → refuse with the spec message; hub + plain → refuse; hub + `@N` → allow; hub + option selector → refuse; leaf + `og project` caller → allow; no stamps → allow (unchanged). Expect FAIL, implement, PASS.
- [ ] **Step 2:** `route_test.go`: one host has the branch; two hosts (prefer current host, else ambiguous); none (error); worktree natively open outside the leaf (route to "mirror window `@N`", never create).
- [ ] **Step 3:** `open.go` against a fake runner: unreachable host fails fast with nothing created; leaf created once with its first window as the target (no stray window); a leaf of the same name stamped with a different key is refused; wait times out → worktree left, error names the step; unsafe path/branch refused before any runner call.
- [ ] **Step 4:** bats `tests/og-project-open.bats` (wired into `flake.nix` like P1a's: `testsSrc ["og-project-open.bats" …]`, `scriptsSrc [...]` for only the files it reads, the built picker binaries on `PATH`) on a scratch local server pair (one server as "local", a second `-L` server reached through a fake `ssh` wrapper as "remote"): `og project open` twice leaves one daemon and one window; a dedicated `og-remote-open` of the leaf is refused; a second local session cannot start a second daemon. Every tmux call uses its own `TMUX_TMPDIR`/`-L`; teardown kills only those servers.
- [ ] **Step 5:** `detach` kills only the daemon's own windows; `attach` starts the pickup with no window creation.
- [ ] **Step 6:** docs updates listed above.
- **Acceptance:**
  - The spec's first four v1 acceptance bullets pass in bats.
  - A user with no project configured: `og-remote-open` behaviour is byte-identical in the existing remote bats checks (they still pass unmodified).
  - Gate: the three commands; the guard has both a Go unit test and a bats case.
- **Risks:** the guard must fail open when the remote cannot report the stamps, and its extra ssh round trip must not add to the picker's attach budget (`attachBudgets` mirror phase) without a matching test; the hidden-rows change in `remoteSessionRowsFormat`/`remoteWindowRowsFormat` must leave a plain or older remote unchanged (an unset option expands empty). The mirror-window wait is bounded (a few seconds) so a slow host does not hang the verb.

## P1c: `og project new`

**Files:**

- Create `picker/project/newwt.go` (+ test): the remote script (`d=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||')` inside the script, falling back to `main`, validated with `git check-ref-format --branch` and `[A-Za-z0-9._/-]`, then `git fetch origin "$d"` with timeout and `wt switch -c <branch> --base "origin/$d"`), default-branch resolution, same-branch refusal from the union of snapshots, `--force`.
- Modify `picker/project/cmd/project/main.go`: the `new` verb and flags (`--host`, `--force`).
- Modify `docs/agents/projects.md`.

- [ ] **Step 1 (test first):** `newwt_test.go`: a hostile default branch (`$(…)`, `;`, backtick, `{`) from a fixture `symbolic-ref` is refused before any fetch; the `origin/` prefix is stripped (`origin/main` → fetch `main`, base `origin/main`, not `origin/origin/main`); fetch failure refuses and names host and reason; branch present on another host refuses, `--force` proceeds; unsafe branch refused before any runner call; `wt` missing reports `wt not found on <host>` and creates nothing.
- [ ] **Step 2:** implement; creation order is create worktree → open (spec 7: a created worktree is never rolled back), and the open order is the one in spec 7 (hub; leaf absent → `new-session` with the worktree as its first window, then pickup; leaf present → pickup, then `new-window`; then wait and select; the trailing `new-window` is skipped when the leaf was just created).
- [ ] **Step 3:** bats case (wired like P1b's; `git` and a stub `wt` that records its argv, or the real `wt` on `PATH` if the devshell has it, so the test never needs a network) with a real throwaway repo and a bare "origin": the new worktree's merge-base with `origin/main` equals origin's tip even though the host's local `main` is stale.
- **Acceptance:** spec v1 acceptance bullets 4 and 5 pass; gate: the three commands.
- **Risk:** `wt`'s `--base` semantics come from P0 step 1. If it cannot honour `origin/<default>`, stop and raise it (spec D5), do not fall back silently to `git worktree add`.

## P2a: Snapshot cache, tick, session-picker host strip

**Files:**

- Create `picker/project/cache.go` (+ test): `$XDG_CACHE_HOME/tmux-og/projects/<HostFileName>.json` through `probekit.ReadOwnerOnlyJSON`/`WriteOwnerOnlyJSON`, `SavedAtStale` dimming.
- Modify `picker/project/cmd/project/main.go`: the `refresh [--host H] [--tick]` subcommand, which probes the hosts of known projects and writes their cache files. `--tick` is the no-op-when-unconfigured entry the hook runs.
- Modify `config/tmux.conf.nix` and the `-B` monitor hook set: `@og-project-tick` (60 s) running `og-project refresh --tick`, probing only hosts of known projects; no-op with no project configured. Follow the `-B` traps in `docs/agents/tmux-gotchas.md` (empty target field, string body under a version guard, unconditional clear).
- Modify `picker/remote.go`/`picker/tui.go` session mode: one row per project (hub session) with the host strip from `picker/hostcolor`, state (`live`/`idle`/`unreachable`) and the drift chip; the preview card (`picker/session_card.go`) gains a host column for placed windows.
- In the tick and the host strip, skip probing and dim a host the tailscale source reports `Online=false` (P0 finding 8); `Online=true` never counts as reachable (Tailscale SSH `check` mode, #486), so the probe timeout and `probekit.ClassifyProbeErr` stay authoritative. A host absent from the tailnet is unaffected.
- Modify `docs/agents/picker.md`, `docs/agents/performance.md` (the tick's measured fork/ssh budget), `docs/agents/projects.md`.

- [ ] **Step 1 (test first):** cache trust-rule tests mirroring `remote_cache_test.go` (wrong owner, symlink, host mismatch, stale dimming, unreachable keeps rows).
- [ ] **Step 2:** first paint is cache-only; add the tick and measure forks per tick with the performance harness described in `performance.md`; record the number in that doc.
- [ ] **Step 3:** session-picker rendering with `visibleWidth` (never `len`) cell sizing; golden-frame tests like `render_list_test.go`.
- **Ordering note:** P2a depends only on P1a, but the hub sessions its project rows describe are created in P1b; shipped alone it renders nothing new until a hub exists. That is intended (each phase leaves the product working).
- **Acceptance:** with a populated cache the session picker renders a project row with host chips and drift without any ssh (test with the runner asserting no call); a project-less user sees no change and no tick work; the tick's measured cost is recorded and within the budget `performance.md` sets. Gate: the three commands.

## P2b: Worktree picker and new-worktree flow

**Files:**

- Create `picker/worktrees.go` (+ test): the `--worktrees` mode rows (branch, host tag, `open`/`open on <host> (<session>)`/`closed`, issue/PR badges from open mirror windows' `@bridge_pr_*`/`@bridge_issue_*`, drift, `⚠ also on <host>`).
- Modify `picker/tui.go`: Enter focuses or opens (spec 2, 3.6), Tab host scope, `^n` new-worktree flow (branch, host list, supervised cancellable runner idiom of `attachRun`, phases `fetch`/`create`/`open`).
- Modify `config/tmux.conf.nix`: the keybinding, chosen per spec D6 against the live which-key table; add it to the which-key description table; `float-conf-assertions` untouched unless the picker opens as a float.
- Modify `docs/agents/picker.md`, `docs/agents/scripts.md`, `docs/agents/projects.md`.

- [ ] **Step 1 (test first):** row-building tests: closed, open in hub, open natively on a host (Enter plans an `@N` id-selector placement, never a create), duplicate branch flag, unreachable host dimmed from cache.
- [ ] **Step 2:** Enter paths test with a fake runner (no ssh on Update; the `@N` placement passes the snapshot worktree path so the launcher can verify it, and a stale `@N` whose path differs or that is a `@bridge_win` mirror is refused); `tui_attach_test.go`-style test that Enter returns immediately.
- [ ] **Step 3:** `^n` flow test: host list default local, unreachable disabled, drift chip shown, `esc` cancels with nothing half-built.
- [ ] **Step 4:** bind the key per D6 and verify with `list-keys` on a scratch server.
- **Acceptance:** spec v1 acceptance bullets 6 and 7; remote strings are sanitized before render (`probekit.DisplayName`); gate: the three commands.

## P3 (post-v1, opt-in): Host-grouped tab ordering

`tmux-project-order` script driven by `window-linked`/`window-unlinked` for
sessions with `@og_project_group_hosts`, debounced, ordering local first then
`configuredHosts` order. **Acceptance:** off by default; no fork added to any
hook for sessions without the option (checked against `performance.md`'s
`if -F` gating rule); a manual `move-window` is honoured until the next window
add. Gate: the three commands.

## P4 (post-v1, optional): adopt, offline, adoption, shipped snapshots

`og project adopt` (`move-window` of natively-open windows into the leaf),
`--offline` for `new`, adopting an existing local session as a hub (D9), and a
remote-stamped snapshot shipped like `@bridge_res` so a live mirror needs no
ssh. Each is its own PR with its own criteria; none is committed to by this
plan.

## Decisions this plan cannot make

The owner decisions D1–D9 in the spec carry recommendations; this plan builds
the recommendations. D6 (the keybinding) is the only one the plan defers to a
live check (P2b step 4).
