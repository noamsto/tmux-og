# Spike: cost of rewriting tmux-og's runtime binaries in Rust

**Status:** spike / estimate, plus a measured bake-off of one hot path (§6).
No production code is ported here.
**Date:** 2026-09-29
**Issue:** #872

## TL;DR

A Rust rewrite of tmux-og's runtime is **not justified by performance**, but the
first version of this spike understated what a *compiled* rewrite of
`tmux-update-icons` buys. The per-second cost of the whole hot path is small
(~52 ms/s, ≈5 % of one core on a quiet machine), and `tmux-update-icons` is 2/3
of it. §6 prototypes that script's 1 s path four ways (plus a fifth variant) against one
fixture and measures them identically (load 4.5 on 16 cores, 250 runs each, byte-identical
output and tmux side effects) **[measured]**:

| variant | mean ms | process forks/run (execs) |
| --- | ---: | ---: |
| A. shipped bash | 37.2 | 15 (11) |
| B. bash, tmux calls collapsed, no `timeout` | 25.0 | 6 (6) |
| C. Go, same call pattern as B | 13.1 | 6 (6) |
| D. Rust, same call pattern as B | 10.8 | 5 (6) |
| E. Go, reads `.git/HEAD` instead of forking git | 5.6 | 2 (2) |

Both levers matter and they are about the same size: collapsing round trips and
dropping `timeout` (with some work B leaves out, §6.1) is A→B (−12 ms); leaving
bash is B→C (−12 ms). Go takes both in one step (**−24 ms per tick, −65 %**).
The Go→Rust step is **−2.4 ms** (≈0.24 % of a core; the two interleaved runs
before the last review round measured −1.4 to −1.5 ms) for a second toolchain —
not worth it. The earlier "language delta is only 2–4 ms" was an estimate; for a
script this size (847 lines, 14 panes of per-row parsing) the measured bash
step is ~12 ms, of which ~4 ms is startup and library sourcing.

Recommendation: **port the `tmux-update-icons` 1 s path to Go with collapsed
tmux calls and no `timeout` wrapper, in the toolchain the repo already ships.
Do not adopt Rust.** Reading `.git/HEAD` instead of forking git is a further
−7.6 ms and should ride along. See §5.

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

> **Superseded in part by §6.** The rows above and the "5 extra forks / 3 tmux
> calls" breakdown come from a 3-window scratch server with one git poll. On the
> 10-window fixture of §6 the shipped script forks 2 tmux (the batched write is
> skipped when nothing changed) and 4 `timeout`+`git` pairs, and takes 37–39 ms
> rather than 35.5 ms. Read §1.4/§1.5 as the small-server figures.

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
   - collapse the tmux CLI round trips (2 on an idle tick, §6.1) into 1 batched
     `display-message`/command list: ~10–16 ms [estimated from the measured
     per-call cost].
   The first draft of this spike estimated that a port keeping the same calls
   saves only ~5–8 ms/s and concluded that the round-trip collapse, not the
   language, is where the win lives. **§6 measured it and the estimate was low:**
   collapse + no `timeout` saves 12 ms and the language change a further 12 ms
   [measured]. Both are available in Go.
2. **The four bash `-B` pollers** are 1.2–7.7 ms above the Go `session-resources`
   baseline (6.8 ms): pr-enrich +1.2, issue-stamp +1.9, agent-usage +2.3, the
   update-icons sweep +7.7 ms [measured] — the gap is interpreter start plus
   extra `tmux`/`stat`/`mkdir` forks, not script logic (§6 found that for the
   larger `update-icons` script the bash step is ~12 ms, so this
   claim holds only for the small pollers). Folding them into one
   multicall binary removes 4 interpreter starts: ~4 × 3.5 = ~14 ms per 5 s ≈
   **2.8 ms/s** [estimated, a floor]. A **resident**
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
  compiled; Rust would only re-do them. If one were ever ported, the Rust side
  is rata&#x74;ui + crossterm, and since the Go pickers are Bubble Tea it is a
  rewrite of the view and input layer, not a translation (§3 candidate E).
- **`tmux-statusline` / `tmux-session-resources`**: already Go and already at
  the compiled floor (7.1 / 6.8 ms, of which ~6 ms is the one `tmux` round
  trip) [measured]. A Rust port saves on the order of the Go-runtime startup,
  ~1–2 ms [estimated].
- **Correctness.** The bugs on record (fork storms #793, coalescing races #810)
  were *design* bugs — fixed with in-process gating and pending markers, not by
  a language change. A rewrite re-derives a large body of accumulated fixes
  documented across `docs/agents/*.md` and buys no correctness.

### 2.3 The break-even: consolidate *and* compile, in Go

The floor measurements say a compiled stub starts in 2.0 ms vs bash's 3.5 ms
(≈1.5 ms saved) while one `tmux` round trip costs 2.6–7.9 ms depending on host
load [measured]. Taken alone that reads as "deleting a call beats deleting the
interpreter". The bake-off (§6) shows the interpreter's *startup* is the small
part: what a compiled binary really removes is bash **interpreting the script**
— sourcing three libraries and looping over every pane row — which is ~12 ms for
`tmux-update-icons` [measured, B→C], as much as the round-trip collapse. The
break-even is therefore a compiled binary that also collapses its tmux round
trips, and that is **strictly cheaper in Go**: the toolchain, `buildGoModule`,
the generated-table pipeline, the Go test suites and the CI cross-build for
`aarch64-darwin` already exist. Rust reaches the same place ~1.5–2.4 ms/tick faster
[measured] with a second toolchain and packaging path.

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
| **E. Port all 52 scripts + 13 Go binaries to Rust** | ~10 k shell LOC + ~33 k Go LOC, all tests | **weeks–months** | No measured payoff; the Go binaries are already compiled. The TUIs (pickers, splash, enrich card) would be rebuilt on the Rust TUI stack named in §2.2 rather than translated from Bubble Tea, which is most of the range. Not recommended. |

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

**Rust: no. Go: yes, for `tmux-update-icons`.**

1. **Do not rewrite in Rust.** Against the same call pattern, Rust beats Go by
   1.5–2.4 ms per tick (10.8 vs 13.1 ms mean in the latest run, ≈0.24 % of a
   core; the gap is noisy) and shrinks the
   binary from 2.0 to 0.5 MB [measured]. In exchange the repo gets a second
   toolchain, a second dependency-hash system and a three-way copy of every
   byte-identical table (§3, §4), plus `aarch64-darwin` CI. Nothing measured
   justifies that. The 13–19 MB bridge-daemon RSS remains the only place a Rust
   win is plausible, and it is unmeasured here.
2. **Port the 1 s path of `tmux-update-icons` to Go, with the collapsed call
   pattern of variant B and no `timeout` wrapper.** Measured: 37.2 → 13.1 ms per
   tick (−24.1 ms/s ≈ −2.4 % of a core, −65 %), 15 → 6 process forks, and — the reason
   this matters — the keystroke-latency probe's p95/p99 under the status job
   drops from 2.0–2.4 / 4.0–4.6 ms (A) to 0.3–0.4 / 0.7–1.2 ms (C, D), i.e. to
   the no-job floor (§6.4). The port carries the script's 800 lines of edge-case
   fixes (§4 "Rewrite risk"), so budget candidate A's 5–8 engineer-days (§3; less in Go, which needs none
   of candidate D's toolchain work) and keep the bats suites as the acceptance suite.
3. **Fold the `.git/HEAD` read into that port** (variant E): a further −7.6 ms
   (13.1 → 5.6 ms) and 6 → 2 forks. The fork count is driven by the script's own
   polling policy: it re-polls git every tick for the invoking session's active
   window and for **every window whose cached `@branch` is empty** — non-git
   directories and detached HEADs — so the 10-window fixture forks git 4× per
   tick. A cheaper policy (cache "not a repo" until the cwd changes) helps the
   bash too and is worth its own issue.
4. **Interim, if the port waits:** the bash-only change in variant B
   (one tmux read, `read -t` guard instead of `timeout`) saves 12 ms per tick on
   its own and is ~30 lines. It is not a substitute for the port: bash still
   spends ~12 ms more than a compiled binary (about 4 ms of it startup and
   library sourcing, the rest interpreting the rows).

Reach for a resident daemon (candidate C) only if the per-tick exec proves to
be the bottleneck after the port; do that in Go too.

### Evidence behind the recommendation (one line each)

- Bake-off, same fixture, ≥200 runs, load 4.5/16: A 37.2, B 25.0, C 13.1,
  D 10.8, E 5.6 ms mean; process forks 15/6/6/5/2 [measured, §6.2].
- Outputs and tmux side effects of A–E are byte-identical on the fixture
  [measured, §6.3].
- Keystroke-echo p95/p99: A 2.0–2.4/4.0–4.6 ms, B 0.9–1.6/3.8–5.0, C/D/E
  0.3–0.4/0.7–1.2, no-job floor 0.4–1.4/1.0–2.8 [measured, §6.4, two runs].
- Go 8.5–10.3 s and Rust 7.0–7.3 s to build the minimal derivation with the
  toolchain already in the store [measured, §6.5] — build time does not decide it.
- Compiled stub starts in 2.0 ms vs bash 3.5 ms [measured, §1.4].
- 13–19 MB RSS per Go bridge daemon — the only repeatable Rust win, and small
  [measured].

---

## 6. Bake-off: bash vs collapsed bash vs Go vs Rust

§1–§5 reasoned from primitive costs. This section prototypes the 1 s path of
`tmux-update-icons` — the per-window name/icon build the status line consumes —
four ways and measures them identically. Code: `spike/update-icons-bakeoff/`.
It is evidence, not product: nothing there is wired into `flake.nix`, the tmux
config or `nix flake check`, and `scripts/tmux-update-icons.sh` is untouched.

### 6.1 What was compared

| | Variant | Source |
| --- | --- | --- |
| A | the shipped script, unmodified | the Nix-built `tmux-update-icons` from `nix build .#default` |
| B | bash, same libs and per-window naming logic (not A plus only these changes, see below); the `list-sessions` and `list-panes -a` reads become **one** tmux call; `timeout 2 git` becomes a `read -t 2` guard on a process substitution (no `timeout` exec) | `b-bash/update-icons.sh` |
| C | Go, same call pattern as B | `c-go/` |
| D | Rust (std only), same call pattern as B | `d-rust/` |
| E | C with `git branch --show-current` replaced by reading `.git/HEAD` (`-tags head`) | `c-go/branch_head.go` |

B omits work A still does in the fixture (per-pane maps only the excluded paths
use, the claude colour/ago computation, the task/name file probes, the
arming-sweep call and one process substitution), so A→B bundles the two
intended changes with that omitted work; B→C and B→D share it and isolate the
language. B, C and D make the same forks (one tmux read, one guarded git per polled
window, one tmux write only when something changed), so the language is the
only variable between them. E is reported separately because it changes what is
forked, not the language.

**Fixture** (`fixture.sh`): a scratch server on the raw tmux binary with
`-f /dev/null`, its own `TMUX_TMPDIR`, socket (`-L probe`) and
`CLAUDE_STATUS_DIR` — never the live server. 3 sessions, 10 windows, 14 panes:
git, worktree, detached-HEAD and non-git cwds; four agent windows (`claude`,
`codex`, `.claude-wrapped`); three multi-pane windows; stale task/AI-name/ago
options, a window with `automatic-rename` off, a manually named one, a crew
name and a stale session tint, so every write path of the loop fires once.
Every git window has its `@branch` seeded (the branch cache) and the prune
marker is in place, so a run is the 1 s steady state.

**Excluded in every variant** (each exits 3 rather than run a path the fixture
does not exercise): the 5 s arming sweep (pinned off with `CLAUDE_NOW`, a test
seam the script already has), carousel/remux stamping, the cwd-move reconcile,
the reflow kick, agent-state file parsing (the state dirs are empty), and the
branch-transition path. The shipped script's background helpers are pointed at
`@none` through its env seams so a run does exactly the naming work. Because
the port excludes the state-file reads, A's real cost on a busy machine is
higher than measured here, which makes the A→C gap a lower bound.

**Steady-state fork pattern.** Shipped A forks **2** tmux processes, not 3 —
the batched `tmux source -` write is skipped when nothing changed — plus
`timeout`+`git` for the invoking session's active window and for **every window
whose cached `@branch` is empty** (here: one non-git dir, one detached HEAD,
one more non-git dir → 4 polls). The §1.4 breakdown assumed one git call.

### 6.2 Results [measured]

`hyperfine -N --warmup 5`, 250 runs per variant in 5 interleaved rounds so host
load drifts across variants alike; steady state (a priming run stamps every
option first). "Execs" are successful `execve`s of one steady-state run under
`strace -f`, counting the process itself; "forks" are `clone`/`fork`/`vfork`
calls that create a process (not a thread), which also sees bash subshells that
fork without exec and the child Go's `os/exec` spawns once to probe pidfd
support. Host: 16 cores, x86_64-linux.

**Run 4 — load average 4.57 → 4.52 (1 min), after the review fixes, the
reported run:**

| var | mean ms | median ms | p95 ms | min ms | execs | forks | vs A |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| A shipped bash | 37.22 | 36.27 | 44.67 | 31.49 | 11 (tmux×2, timeout×4, git×4) | 15 | — |
| B collapsed bash | 24.96 | 24.57 | 28.85 | 21.05 | 6 (bash, tmux×1, git×4) | 6 | −33 % |
| C Go | 13.13 | 12.42 | 15.69 | 10.16 | 6 (tmux×1, git×4) | 6 | −65 % |
| D Rust | 10.75 | 10.52 | 12.83 | 8.23 | 6 (tmux×1, git×4) | 5 | −71 % |
| E Go, `.git/HEAD` | 5.56 | 5.53 | 6.61 | 4.46 | 2 (tmux×1) | 2 | −85 % |

**Earlier interleaved runs (before the review round added two directory
checks to B, C and D; forks then counted as execs only):**
run 3, load 6.96 → 6.24: A 39.21 / B 26.97 / C 12.60 / D 11.15 / E 5.60 ms mean
(medians 38.19 / 26.40 / 12.45 / 10.90 / 5.47); run 2, load 8.66 → 8.83 (above
the ~8 limit): A 43.31 / B 29.41 / C 14.84 / D 13.29 / E 6.61. The ordering
A > B > C > D > E held in all three, and the Go–Rust gap was 1.4–2.4 ms mean.
One still earlier non-interleaved pass at load 7.5 → 8.5 had D (18.0) slower
than C (14.3) with a 38 ms D p95, so the Go–Rust difference is within noise
of a loaded shared host; the A > B > C and E ordering was stable in every pass.
Loads of 30–40 earlier in the day inflated every number and were discarded.

Reading the deltas (run 4):

- **A→B −12.3 ms** — one tmux call and four `timeout` execs gone, plus the work
  B leaves out (§6.1), which is not separable here. Primitives on
  the fixture [measured]: `tmux list-sessions` 2.6 ms, `git branch
  --show-current` 1.7 ms, `timeout 2 git …` 3.3 ms, so `timeout` costs 1.6 ms a
  poll × 4.
- **B→C −11.8 ms** — leaving bash (run 3: −14.4 ms). Starting bash and sourcing
  the three libs is 4.1 ms [measured]; most of the rest is bash interpreting
  14 pane rows × 29 fields and the per-window loops.
- **C→D −2.4 ms mean, −1.9 ms median** (runs 2–3: −1.4 to −1.5 ms) — Rust vs Go,
  same forks, same logic: process start and parsing. Go also forks one extra
  process per run to probe pidfd support (6 forks vs 5).
- **C→E −7.6 ms** — four git forks (1.7 ms each + fork/wait) replaced by four
  file reads.

### 6.3 Equivalence check [measured]

`equiv.sh` builds a fresh fixture per variant, runs the variant once, and
snapshots every window and session option (`show-options -w` / `show-options`
per id); it then runs the variant a second time and requires no change.
Each variant must exit 0, print nothing (the script prints nothing), match A's
option snapshot byte for byte, and leave the snapshot unchanged on the second run.

```
$ spike/update-icons-bakeoff/equiv.sh
A PASS (options set: 66 lines, stdout bytes: 0)
B PASS (options set: 66 lines, stdout bytes: 0)
C PASS (options set: 66 lines, stdout bytes: 0)
D PASS (options set: 66 lines, stdout bytes: 0)
E PASS (options set: 66 lines, stdout bytes: 0)
```

The 66 lines include the writes each variant makes on its first run:
`@window_icon_padded` on all 10 windows and `@window_icon_display` on those
whose display is non-empty, `@window_has_agent` on the four agent windows, `automatic-rename on`,
`@crew_seen`, `@active_pane_icon` per session, and the clears of the stale
task / AI-name / ago / session-tint options.

### 6.4 Keystroke latency under the real harness [measured]

`tests/perf/keystroke-latency.sh` builds a remote-bridge chain, which is more
than this question needs, so `latency.sh` reuses its measurement and drops the
bridge: the scratch fixture server, a real status-drawing client attached from
a second server, `status-interval 1` and `status-format[0]` set to
`#(echo; <variant> …)` — the live invocation — and `picker/latencyprobe` typing
into a `cat` pane over a control-mode client (2 500 samples at 10 ms per
variant, ≈ 25 status-job bursts each). `none` is a status line with no job.

| variant | p50 ms | p95 ms | p99 ms | max ms |
| --- | ---: | ---: | ---: | ---: |
| none (run 1 / run 2) | 0.2 / 0.2 | 0.4 / 1.4 | 1.0 / 2.8 | 4.4 / 6.7 |
| A shipped bash | 0.3 / 0.3 | 2.0 / 2.4 | 4.0 / 4.6 | 17.9 / 19.9 |
| B collapsed bash | 0.2 / 0.2 | 1.6 / 0.9 | 5.0 / 3.8 | 15.1 / 10.8 |
| C Go | 0.2 / 0.2 | 0.3 / 0.3 | 0.7 / 0.9 | 3.7 / 6.4 |
| D Rust | 0.2 / 0.2 | 0.3 / 0.4 | 0.8 / 1.2 | 2.9 / 3.2 |
| E Go, `.git/HEAD` | 0.2 / 0.2 | 0.3 / 0.3 | 0.8 / 1.0 | 2.0 / 4.8 |

Load 5.5 → 6.0 (run 1) and 5.5 → 7.0 (run 2), measured before the review round's
directory-check additions. A pushes the tail to 4.0–4.6 ms p99 and 18–20 ms max in both runs; B's p99 (3.8–5.0)
and max (10.8–15.1) are elevated in both runs but its p95 (1.6 and 0.9) overlaps
the no-job floor, which is itself noisy at p95 (0.4 and 1.4 ms across runs) —
so p95 does not cleanly separate A or B from `none`, only p99/max do. C, D and E
sit at the floor and are not distinguishable from each other or from `none`. A
shorter 600-sample pass at 30 ms was too sparse to separate anything (most
samples miss the once-a-second burst) and is not reported. Two runs on a
10-window fixture: read this as "the shipped bash shows a p99/max tail and the
compiled variants do not", not as precise percentiles; absolute values on a
40-window server will be larger.

### 6.5 Build cost [measured unless marked]

| | C Go | D Rust |
| --- | ---: | ---: |
| stripped binary | 2 031 778 B (2.0 MB; includes the icon table) | 537 424 B (0.5 MB) |
| minimal Nix derivation, cold (`buildGoModule` / `buildRustPackage`) | 8.5–10.3 s | 7.0–7.3 s |
| cargo/go build outside Nix | — | 2.1 s (`cargo build --release`) |

Times are `nix-build-times.sh`, two rounds, toolchain already in the store, load
11–12; each build has a fresh salt so nothing is cached. Neither number is a
reason to choose: the real cost is the toolchain around it. What Rust would add
to the flake [estimated]:

- `cargo`, `rustc` (and `clippy`, `rustfmt`, `rust-analyzer`) in
  `devShells.default`, or a pinned toolchain via `fenix`/`rust-overlay` — the
  spike pulled them in with `nix shell` for one command and added nothing
  permanent;
- a `rustPlatform.buildRustPackage` (or `crane`) package wired into
  `config/tmux.conf.nix`'s script pipeline. This prototype has no crates, so
  `cargoLock.lockFile` needs no hash; any real dependency adds a `cargoHash` or
  `outputHashes`, and a way to inject the `@NAME@` store paths (a `build.rs` or
  generated `.rs`, as `icons_generated.go` does for Go);
- `cargo test` as a `nix flake check` derivation, `rustfmt`/`clippy` hooks in
  `pre-commit.settings.hooks`, and a committed `Cargo.lock`;
- the `aarch64-darwin` CI leg builds it natively, and the remote bridge needs a
  second cross-build matrix (§4).

### 6.6 What this does and does not show

- It shows the language and the round-trip pattern each contribute about
  half of the bash→Go gain for this script, that Rust adds ~1.5–2.4 ms over Go,
  and that the compiled variants remove the bash tail from keystroke latency.
- It does **not** cover the excluded paths above. A port must also carry the
  agent-state reads, the sweep and the reconcile hooks; those add work to every
  variant, and to bash more than to a compiled binary.
- One machine, one fixture, four benchmark passes and two latency passes. The
  A > B > C and E ordering was stable in every pass; the Go–Rust gap was not
  (one pass reversed it); the absolute values are not portable.
- The prototypes are deliberately not shippable: they exit 3 on branch
  transitions, agent-state files and untrusted or unpruned state dirs.

### 6.7 Reproducing

From `nix develop` (Go, jq, shellcheck are in the devShell; the rest are
one-off):

```bash
cd spike/update-icons-bakeoff
nix shell nixpkgs#hyperfine nixpkgs#strace -c true   # or have them on PATH
./gen-icons.sh > icons.tsv        # only when the icon table changes
./build.sh                        # go build (C, E) + cargo build via `nix shell` (D)
./equiv.sh                        # 6.3: A B C D E must all PASS
./bench.sh 250                    # 6.2: mean/median/p95/min + forks (load before/after)
INTERVAL=10ms ./latency.sh 2500   # 6.4
./nix-build-times.sh              # 6.5 build times
```

`bench.sh` prints `load:` before and after; rerun when the 1-minute load is
above ~8 on this 16-core host. Every script builds its own scratch server under
`/tmp/og-bakeoff-*` and removes it on exit.

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
