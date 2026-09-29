# Spike: cost of rewriting tmux-og's runtime binaries in Rust

**Status:** spike / estimate. No production code is ported here.
**Date:** 2026-09-29
**Issue:** #872

## TL;DR

A Rust rewrite of tmux-og's runtime is **not justified by performance**. The
per-second cost of the whole hot path is small (~52 ms/s, ≈5 % of one core on a
quiet machine), and most of it is language-independent: each `tmux` CLI call
costs ~6–8 ms and each `timeout git` call ~10 ms, regardless of the language
that forks them. The bash-vs-compiled delta is only ~2–4 ms per invocation.
The one place a language change *could* pay is a **resident or multicall binary**
that (a) removes the per-tick interpreter start and (b) collapses the several
`tmux` round trips per tick into one — and that win is available in the Go
toolchain the repo already ships, with none of Rust's packaging cost. The
already-Go `tmux-statusline` and `tmux-session-resources` sit at the compiled
floor; the remaining bash hot scripts are at most a few ms above it.

Recommendation: **do nothing for Rust; make at most a small, targeted
consolidation in Go if the 1 s path ever shows visible latency again.**

Every number below is tagged **[measured]** or **[estimated]**. Measured
numbers were taken on this machine on 2026-09-29 against a scratch tmux server;
the commands are in [Method](#method-reproducing-the-numbers).

---

## 1. Inventory

### 1.1 Shell scripts

`scripts/` holds **60 files / 9 900 lines** [measured, `wc -l scripts/*.sh`]:
**53 non-library scripts** — 52 of them the named packaged set `scriptNames` in
`config/tmux.conf.nix` (`og.sh` is the 53rd, a launcher) — plus **7 shared
libraries** (`lib-*.sh`, 2 274 lines) sourced into them at runtime. All are built with
`writeShellScript`/`writeShellScriptBin`; `scriptsWithIcons` additionally get
`@ICON_MAP@`, `@lib_claude@`, etc. substituted at build time.

Grouped by how they are driven:

| Driver | Scripts | Notes |
| --- | --- | --- |
| **`#()` every 1 s** (`status-format[0]`) | `tmux-update-icons` (847 LOC, bash); `tmux-statusline` (Go) | The status line has exactly two `#()` jobs. `claude-status`, `tmux-branch-display`, `tmux-dir-display` were the bash originals; they are now **ported into `tmux-statusline`** (Go) and are only still packaged, not driven. See §1.3. |
| **`-B` monitor hooks, every 5 s** (cluster-independent) | `tmux-update-icons` sweep (bash); `tmux-pr-enrich --tick` (634 LOC, bash); `tmux-issue-stamp --backfill` (241 LOC, bash); `tmux-agent-usage --tick` (158 LOC, bash); `tmux-session-resources --tick` (Go, 161 LOC) | Five hooks, one Go. |
| **tmux hooks** (window/pane/resize/theme) | `tmux-reflow-windows` (636 LOC, bash), `tmux-reap-pane`, `tmux-shell-prompt`, `tmux-client-theme`, `tmux-default-size`, `tmux-reconcile-window`, `tmux-float-refit`, `tmux-float-nudge`, `tmux-grid-refit`, `tmux-apply-theme-colors`, `tmux-splash-maybe` | `tmux-float-refit` / `tmux-grid-refit` are gated in-process by `if -F` and fork **only** on a real resize; the resize-storm fix (#793/#810) already removed the fork storm. |
| **Keybinds / one-shot** | pickers (Go), `tmux-scratchpad`, `tmux-smart-nav`, `tmux-window-nav`, `tmux-window-picker`, `tmux-session-picker`, `tmux-which-key`, `og-open`, `og-remote-*` | Human-paced; one fork per keystroke is invisible. |
| **Claude Code / worktrunk hooks** | `claude-status-update` (634 LOC), `tmux-worktree-match`, `tmux-issue-stamp`, `og-notify*` | Event-driven. |
| **Remote bridge launchers** | `og-remote-open` (708 LOC), `og-remote-picker` (306 LOC), `og-remote-auth`, `og-remote-detach`, `og-remote-theme`, `og-remote-loading` | Run once per attach / per user action. |

### 1.2 Go programs

Two modules under `picker/` and `generator/`, **≈33 000 non-test LOC and
≈43 600 test LOC** [measured, `find … -name '*.go'`]:

- `picker/` → **10 binaries** (`buildGoModule` `subPackages`): `tmux-picker-generate`,
  `tmux-splash`, `tmux-statusline`, `tmux-enrich-card`, `agent-detect`,
  `tmux-session-resources`, `og-remote-bridge`, `og-remote-bridge-daemon`,
  `og-remote-bridge-renderer`, `og-remote-bridge-ctl`.
- `generator/` → 3 binaries (`og-generate`, `og-init`, `og-doctor`).
- `picker/remotebridge/` alone is **17 063 non-test LOC** [measured] — the control-mode
  bridge daemon, the only **long-resident** component (`og-remote-bridge-daemon`).
- Binary sizes, stripped (`-s -w`) [measured]: daemon 4.17 MB, picker 5.13 MB,
  statusline 2.73 MB, session-resources 2.59 MB.
- Live daemon RSS [measured, `ps -o rss=` over three running daemons]: **13–19 MB**.

### 1.3 Already compiled: the hot-path port is half done

The repo has already moved its zero-fork-sensitive work to Go:

- `tmux-statusline` (Go) replaced the `claude-status` + `tmux-branch-display` +
  `tmux-dir-display` bash `#()` jobs.
- `tmux-session-resources` (Go) drives one of the five `-B` hooks.
- The resize-hook fork storm (#793) and its burst coalescing (#810) were fixed
  **in place** — gating with `if -F` and pending markers — not by rewriting in
  another language.

This matters for the estimate: the remaining bash hot surface is essentially
**`tmux-update-icons`** (1 s + 5 s sweep) and four `-B` pollers.

### 1.4 Per-invocation cost

Measured with `hyperfine --warmup 5` against a scratch server (see
[Method](#method-reproducing-the-numbers)); means, 60–450 runs each, on a
busy shared host, so the σ is wide. "extra forks" excludes the process itself.

| Command | mean ms | min–max ms | extra forks | tag |
| --- | ---: | ---: | ---: | --- |
| `true` (coreutils) | 0.66 | 0–12.8 | 0 | measured |
| minimal compiled stub (`go build` hello, `-s -w`, 1.4 MB) | 2.0 | 0.8–6.9 | 0 | measured |
| `bash -c 'exit 0'` | 3.5 | 1.2–16.2 | 0 | measured |
| `tmux-branch-display` (legacy bash 1 s job) | 3.0 | 1.5–9.7 | 0 | measured |
| `tmux-dir-display` (legacy bash 1 s job) | 3.7 | 1.8–21.2 | 0 | measured |
| `claude-status` (legacy bash 1 s job) | 5.3 | 3.4–15.7 | 0 | measured |
| **`tmux-statusline` (Go 1 s job)** | **7.1** | 5.3–13.1 | 1 | measured |
| **`tmux-update-icons` (bash 1 s job)** | **35.5** | 26.5–59.8 | 5 | measured |
| `tmux-update-icons` sweep (bash 5 s hook) | 14.5 | 12.0–20.3 | 2 | measured |
| `tmux-reflow-windows` (bash hook) | 10.5 | 7.9–24.9 | 1 | measured |
| `tmux-agent-usage --tick` (bash 5 s hook) | 9.1 | 7.0–13.3 | 1 | measured |
| `tmux-issue-stamp --backfill` (bash 5 s hook) | 8.7 | 6.1–14.6 | 2 | measured |
| `tmux-pr-enrich --tick` (bash 5 s hook, idle server) | 8.0 | 5.8–16.7 | 1 | measured |
| **`tmux-session-resources --tick` (Go 5 s hook)** | **6.8** | 4.6–14.5 | 1 | measured |

Primitive costs that dominate every row above:

| Primitive | mean ms | tag |
| --- | ---: | --- |
| one `tmux display-message -p …` | 7.9 | measured |
| one `tmux list-panes -a …` | 5.7 | measured |
| one `git -C <tree> branch --show-current` | 5.6 | measured |
| `timeout 2 git … branch --show-current` | 9.9 | measured |

**Where the 35.5 ms of `tmux-update-icons` goes** [measured by `strace -f`,
steady state after the branch cache is seeded]: 3 `tmux` calls (~17–24 ms) +
1 `timeout git` (~10 ms) + bash startup/parse (~3.5 ms) + script logic. The
`timeout` wrapper alone costs ~4 ms over the raw `git` call it guards.

### 1.5 Per-second budget

- 1 s path: 35.5 + 7.1 = **42.6 ms/s** [measured].
- 5 s path: 14.5 + 8.0 + 9.1 + 8.7 + 6.8 = **47.1 ms per 5 s ≈ 9.4 ms/s** [measured].
- **Total ≈ 52 ms/s ≈ 5.2 % of one core** [measured]. This excludes `tmux` hooks
  that fire only on events (reflow, float/grid refit, theme) and every
  human-paced keybind.

These jobs run under the tmux server thread (it forks the job and collects its
output), so under load the cost is what `docs/agents/performance.md` records as
keystroke latency — the reason this spike exists at all.

---

## 2. Candidates ranked by payoff

### 2.1 Where Rust would buy something — and how much

1. **`tmux-update-icons` (1 s) is the single biggest bash hot path** — 35.5 ms of
   the 42.6 ms 1 s budget [measured]. A Rust (or Go) rewrite could:
   - remove bash startup/parse: ~3.5 ms [measured against `bash -c exit 0`];
   - drop the `timeout` wrapper: ~4 ms [measured: git 5.6 vs `timeout git` 9.9];
   - collapse the 3 `tmux` CLI round trips into 1 batched
     `display-message`/command list: ~10–16 ms [estimated from the measured
     per-call cost].
   A port that keeps the same 3 tmux calls and the same git call saves only
   ~5–8 ms/s ≈ **0.5–0.8 % of a core** [estimated]. **The round-trip collapse,
   not the language, is where the win lives** — and that is available in Go.
2. **The four bash `-B` pollers** are 1.2–7.7 ms above the Go `session-resources`
   baseline (6.8 ms): pr-enrich +1.2, issue-stamp +1.9, agent-usage +2.3, the
   update-icons sweep +7.7 ms [measured] — the gap is interpreter start plus
   extra `tmux`/`stat`/`mkdir` forks, not script logic. Folding them into one
   multicall binary removes 4 interpreter starts: ~4 × 3.5 = ~14 ms per 5 s ≈
   **2.8 ms/s** [estimated]. A **resident**
   process could additionally batch their tmux reads, but that needs a lifecycle
   (start/stop per server, failure handling) the current `run-shell` hooks do
   not have.
3. **Resident bridge daemon memory.** Go daemons measure 13–19 MB RSS [measured];
   a Rust daemon of the same shape would likely land at ~3–6 MB
   [estimated from typical stripped-Rust daemon RSS], saving ~10 MB *per
   mirrored remote session*. On a laptop that is real but small, and it is the
   only place with a repeatable Rust win.

### 2.2 Where Rust buys nothing

- **One-shot keybind / launcher scripts** (`og-open`, `tmux-scratchpad`,
  `tmux-window-nav`, `og-remote-*`): one fork per human action; 3 ms of
  interpreter start is invisible.
- **Go TUI pickers** (`tmux-picker-generate`, splash, enrich card): already
  compiled; Rust would only re-do them.
- **`tmux-statusline` / `tmux-session-resources`**: already Go and already at
  the compiled floor (7.1 / 6.8 ms, of which ~6 ms is the one `tmux` round
  trip) [measured]. A Rust port saves on the order of the Go-runtime startup,
  ~1–2 ms [estimated].
- **Correctness.** The bugs on record (fork storms #793, coalescing races #810)
  were *design* bugs — fixed with in-process gating and pending markers, not by
  a language change. A rewrite re-derives a large body of accumulated fixes
  documented across `docs/agents/*.md` and buys no correctness.

### 2.3 The break-even: consolidate, don't translate

The break-even is **one process (or one multicall binary) doing the work of N
forked bash scripts and their tmux round trips** — not "bash → Rust". The
measured floor says so: a compiled stub starts in 2.0 ms vs bash's 3.5 ms
(≈1.5 ms saved), while a single `tmux` round trip costs 5.7–7.9 ms. **You save
roughly 4× more by deleting one tmux call than by deleting the interpreter.**

That consolidation is **strictly cheaper in Go**: the toolchain, `buildGoModule`,
the generated-table pipeline, the Go test suites, and the CI cross-build for
`aarch64-darwin` already exist. Rust would add a second toolchain and a second
packaging path to reach the same place.

---

## 3. Cost estimate per candidate group

All figures are **AI-assisted, in engineer-days [estimated]**, and include the
tests and Nix packaging a language change drags in. Ranges are wide because the
bash encodes years of accumulated edge-case fixes.

| Candidate | Scope | Effort | Why |
| --- | --- | --- | --- |
| **A. `tmux-update-icons` → Rust** | 847 LOC bash + its bats suites | **5–8 d** | The richest single script: per-window/pane batched reads, claude-state pruning (`claude_prune_stale_state`, server-owner liveness), the 60 s reaping backstop, `@window_has_agent`/naming reset, cwd-move reconcile. Plus Nix: `crane`/`naersk` or `rustPlatform` + `cargoHash`, and an `aarch64-darwin` cross/native build in the `macos-14` CI job. |
| **B. Four bash `-B` pollers → one Go multicall binary** | `tmux-pr-enrich` (634) + `tmux-issue-stamp` (241) + `tmux-agent-usage` (158) + the `update-icons` sweep | **4–6 d** | Same language the repo already builds. Keeps the provider scripts (`-linear`, `-github`, `-claude`, …). Main work is preserving the per-server stamp suffixes, locks, and backfill caps. |
| **C. Resident daemon replacing the `#()` + `-B` jobs** | A + B + a lifecycle | **10–15 d** | Real win (no per-tick exec, batched reads over one connection) but a new resident process per tmux server, with start/stop/failure semantics the hooks do not currently own. Independent of language. |
| **D. Rust multi-toolchain infra (shared by A–C)** | flake + devShell + CI + `nix flake check` | **1.5–3 d** | Add Rust to `devShells.default`, a `cargoHash`ed `buildRustPackage` (or crane), `cargo test` in `nix flake check`, and the `aarch64-darwin` build. |
| **E. Port all 52 scripts + 13 Go binaries to Rust** | ~10 k shell LOC + ~33 k Go LOC, all tests | **weeks–months** | No measured payoff; the Go binaries are already compiled. Not recommended. |

**Test-porting cost specifically.**

- bash → Rust/Go tests: the bats suites (`tests/*.bats`) drive the scripts as
  processes through a live tmux; a port must re-express them against the new
  entry point, and the byte-level tmux-format assertions stay.
- The **byte-identical tables** (`CLAUDE.md`) are the sharp edge. `ENRICH_PIE_GLYPHS`
  (shell) ↔ `enrichstate.PieSlices` (Go), the draft-badge rule in three places,
  the agent priority order (`claude_priority_state` ↔ `agentPriority`), and
  `mirror_name_part` ↔ `mirrorname.Part` are already duplicated across two
  languages. A **third** language makes each a three-way invariant, or forces
  the canonical table into one source with three codegen targets. That is
  ~2–4 d of infrastructure on its own [estimated] and an ongoing drift risk.
- Nix packaging: `nix flake check` currently runs bats + the Go test suites +
  conf assertions. A Rust crate adds `cargo test` and a second vendored
  dependency hash; the `flake check` wall time grows with the crate build
  [estimated].

---

## 4. Risks

- **Remote hosts must have the binaries.** The bridge ships *its own* store
  paths to the remote (`docs/agents/bridge-daemon.md` → "What the Remote Host
  Needs on PATH"). A Rust binary must be built for the remote's platform
  (`aarch64-darwin` mac clients, `x86_64-linux`); the repo already cross-builds
  Go for both, so Rust adds a second cross-build matrix. A binary that is not
  on the remote's PATH degrades features silently (documented above).
- **`@NAME@` store-path substitution pipeline.** `config/tmux.conf.nix` renders
  the config with `writeShellScriptBin`/`writeShellScript` and `@lib_*@`,
  `@ICON_MAP@`, `@agent_detect_bin@`, … placeholders. Rust has no equivalent:
  it needs a `build.rs` or Nix-generated `.rs`, mirroring the Go build's
  `icons_generated.go` / `tips_generated.go`. Any `@NAME@` pattern must stay out
  of every other context (`CLAUDE.md`).
- **Two toolchains.** Go + Rust in the devShell, CI, `nix flake check`, and the
  contributor docs; two dependency-vendoring systems; two ways a build can
  break. That is a permanent tax for a ~5 % -of-a-core saving.
- **Rewrite risk.** The bash encodes fixes recorded in `docs/agents/*.md`
  (ownership guards, `server=` liveness, lock losers, per-server stamp
  suffixes, `EFF_ACTIVE` modal-pane rule). A port must re-derive each one or
  silently regress it; there is no compiler help for that.

---

## 5. Recommendation

**Do nothing for Rust.** A Rust rewrite is not justified by the measured
performance headroom: the whole hot path is ≈52 ms/s ≈5 % of one core, and
~80 % of that is `tmux`/`git` round trips and `timeout` wrapping that a Rust
binary would still pay. The bash-vs-compiled delta is ~1.5 ms per invocation.

**If** the 1 s path ever shows visible latency again, take **candidate B in
Go**: fold the remaining bash hot scripts into one Go multicall binary and, more
importantly, **collapse their tmux round trips** (measured: one call = 5.7–7.9 ms,
so removing two calls/tick saves ~12–16 ms/s, an order of magnitude more than
the language change). Reach for a resident daemon (candidate C) only if the
per-tick exec itself proves to be the bottleneck, and do that in Go too.

Keep Rust off the table until there is a measured, language-attributable
bottleneck — which this spike did not find.

### Evidence behind the recommendation (one line each)

- `tmux-update-icons` 35.5 ms/s, of which bash startup ≈3.5 ms and `timeout`
  overhead ≈4 ms while tmux round trips ≈17–24 ms [measured].
- Compiled Go hot binaries already at 6.8–7.1 ms, dominated by their single
  `tmux` call [measured].
- Compiled stub starts in 2.0 ms vs bash 3.5 ms [measured].
- One `tmux` call = 5.7–7.9 ms [measured]; deleting a call beats deleting the
  interpreter.
- 13–19 MB RSS per Go bridge daemon — the only repeatable Rust win, and small
  [measured].

---

## Method: reproducing the numbers

Host: NixOS, `x86_64-linux`; tmux `next-3.8` wrapper from `nix build .#default`.
All scripts were the built store binaries from the wrapper (`-s -w` for Go).
Measurements ran on a shared, busy host, so treat σ as large; the ordering and
the round-trip dominance are stable, the absolute means are not.

Scratch server (never the live server — see
`docs/agents/tmux-gotchas.md`):

```bash
export TMUX_TMPDIR=/tmp/og872-bench/run CLAUDE_STATUS_DIR=$TMUX_TMPDIR/status
export OG_ENRICH_CACHE_DIR=$TMUX_TMPDIR/og-pr OG_AGENT_USAGE_DIR=$TMUX_TMPDIR/usage
rm -rf "$TMUX_TMPDIR"; mkdir -p "$TMUX_TMPDIR" "$CLAUDE_STATUS_DIR"
nix build .#default
./result/bin/tmux -L probe new-session -d -s probe -x 200 -y 50
./result/bin/tmux -L probe new-window -t probe; ./result/bin/tmux -L probe new-window -t probe
sock=$(./result/bin/tmux -L probe display-message -p '#{socket_path}')
pid=$(./result/bin/tmux -L probe display-message -p '#{pid}')
export TMUX="$sock,$pid,0"
```

Wall-time per invocation:

```bash
# hot path (1 s + 5 s)
hyperfine --warmup 5 --min-runs 60 \
  "tmux-update-icons probe '' '' '' latte $pid" \
  'env OG_TICK_SWEEP=1 tmux-update-icons' \
  'tmux-statusline --session probe' \
  'tmux-session-resources --tick' \
  'tmux-pr-enrich --tick' 'tmux-agent-usage --tick' 'tmux-issue-stamp --backfill' \
  'tmux-reflow-windows probe 200'

# compiled-startup baseline and the legacy bash 1 s jobs
hyperfine --warmup 5 --min-runs 60 \
  'true' 'bash -c "exit 0"' '/tmp/og872-bench/stub/stub' \
  "tmux-branch-display feat/x $PWD" "tmux-dir-display x $PWD $PWD" \
  'claude-status'

# the primitives the rows decompose into
hyperfine --warmup 5 --min-runs 60 \
  'tmux display-message -p "#{start_time}"' \
  'tmux list-panes -a -F "#{pane_id}|#{session_id}"' \
  "git -C $PWD branch --show-current" \
  "timeout 2 git -C $PWD branch --show-current"
```

Forks per invocation (count only successful execs; failed PATH probes inflate a
naive `grep`):

```bash
strace -f -e trace=execve -o /tmp/trace.txt tmux-update-icons probe '' '' '' latte "$pid"
grep -c 'execve(.* = 0' /tmp/trace.txt
```

Daemon RSS:

```bash
for p in $(pgrep -f 'og-remote-bridge-daemon$'); do ps -o rss= -p "$p"; done
```

LOC / sizes:

```bash
wc -l scripts/*.sh
find picker generator -name '*.go' -not -name '*_test.go' | xargs wc -l | tail -1
find picker generator -name '*_test.go' | xargs wc -l | tail -1
ls -la "$(dirname "$(command -v tmux-statusline)")"
```

The compiled stub is a 2-line `package main; func main(){}` built with
`go build -ldflags '-s -w'`; it is the native-startup baseline, and Rust startup
is expected to be equivalent [estimated].
