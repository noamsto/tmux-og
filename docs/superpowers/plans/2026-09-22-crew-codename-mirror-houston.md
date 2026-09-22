# Crew Codename Missing on Houston Mirror — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decide whether houston's missing crew codename/colour on a mirror is a bug in this repo's shipper/render path or houston running a tmux-og that predates `@bridge_crew_*`, then either fix it with a production-path regression or ship a no-code finding PR with the exact version-check command.

**Architecture:** Houston is the viewer (DST). The daemon that stamps `@bridge_crew_name`/`@bridge_crew_color` and the formats that draw them all run on the viewer, not on the source where local decoration already works. Reproduce with `tests/remote-m2-integration.bats` (two local tmux servers, no ssh). Rule out an old viewer first from evidence this repo can produce; only then hunt the "permanently bare mirror" lead (`registry.generation` / skipped `--force` reflow). Do not invent a fix if the production path is already green.

**Tech Stack:** bash bats (`tests/remote-m2-integration.bats`), Go daemon (`picker/remotebridge/daemon/windowlabels.go`), `scripts/tmux-reflow-windows.sh`, generated `status-format[1]`.

## Global Constraints

- `ssh houston` is impossible from this machine; never plan around it.
- Do not touch float mirroring or the layout parser (issue #738 / `feat/738-…`).
- Daemon writes `@bridge_*` only, never `@crew_name`/`@crew_color`.
- Out of scope: `tmux-apply-theme-colors.sh` pane-border `@bridge_crew_role*` (pane chrome, not window-list decoration).
- Local gate is `nix build .#default`, `nix flake check`, `nix build .#lint` — all three.
- Houston version-check command must be fish (user shell).
- No speculative code change if the M2 production path already ships and the three render sites already read `@bridge_crew_*`.

## Consumer map (producer/consumer contract)

Factual map for `EVIDENCE_REVIEW.md`. Mark each edge **compatible** unless a later task changes it.

| Stage | Symbol / file | Disposition |
| --- | --- | --- |
| Producer | `labelShipper.apply` in `picker/remotebridge/daemon/windowlabels.go` writes `@bridge_crew_name` / `@bridge_crew_color` | diagnose; change only if Task 3 finds a red production-path case |
| Transform | `cleanLabelValue` + `crewColorRe` in the same file | compatible unless sanitizer drops a real colour |
| Persistence | window options on the DST mirror | compatible |
| Set-change backstop | `registry.generation` in `picker/remotebridge/daemon/windows.go`; `due()` in `subscriptions.go`; `labelShipper.flush` | diagnose against the "permanently bare mirror" note |
| Redraw | `Config.reflow()` then `tmux-reflow-windows --force` (`picker/remotebridge/daemon/daemon.go`) | diagnose; M2 `bridge_up` does **not** pass `--reflow` unless the test does |
| Consumer 1 | `config/tmux.conf.tmpl` `status-format[1]` via `bridgeOpts()` in `generator/render/status.go` (`#{?#{@bridge_win},#{@bridge_crew_name},#{@crew_name}}`) | compatible; flake check `crew-badge-conf-assertions` pins the `#671` bridge bypass |
| Consumer 2 | `scripts/tmux-reflow-windows.sh` reads `@bridge_crew_name` in the `@bridge_win==1` arm, stamps `@window_crew_disp`; colour via `bopt[crew_color]` | compatible unless Task 3 shows `@window_crew_disp` empty while `@bridge_crew_name` is set |
| Consumer 3 | `picker/main.go` substitutes fields 20/21 when `@bridge_win` | compatible |
| Consumer 4 | `picker/statusline/main.go` `bridgeWin=="1"` arm draws `bridgeCrewName` and returns (does not apply `#671` local-agent gate) | compatible |
| Validator | `tests/remote-m2-integration.bats` cases at ~1835 (stamp), ~1932 (subscription), ~2811 (reconnect); `picker/remotebridge/daemon/windowlabels_test.go` | extend only if a new failing production path is found |
| Absent | houston SSH probe | absent by constraint |

## Houston version-check

Pass/fail is the "bare mirror is a viewer-build check" paragraph under Remote Window Labels in `docs/agents/bridge-shipped-state.md`. Run that snippet on houston. Do not copy the command or the criteria here.

---

### Task 1: Prove the current tree ships `@crew_name` → `@bridge_crew_name`

**Files:**
- Test (run only): `tests/remote-m2-integration.bats` (filter the three existing production-path cases)
- Read-only: `docs/agents/bridge-shipped-state.md` "Remote Window Labels"; `picker/remotebridge/daemon/windowlabels.go`

**Interfaces:**
- Consumes: two-server M2 harness (`bridge_up`, SRC/DST `tmux -L`)
- Produces: pass/fail of the existing stamp + subscription + reconnect cases; log paths if red

- [ ] **Step 1: Run the three existing M2 label cases against this tree**

From the worktree root, with the repo's wrapped tmux on PATH if `nix develop` is loaded:

```bash
nix develop -c bats tests/remote-m2-integration.bats \
  -f 'daemon ships the remote.s window labels onto the mirror windows|a remote label change reaches the mirror on a subscription, not a poll|window labels keep tracking the remote across a reconnect'
```

Expected: all three PASS. These are the production entry point (`labelShipper` over `--test-local`, same `windowLabelFormat` and `@bridge_crew_*` writes as a live daemon).

If a case is red, that **is** a code bug in this tree: skip Task 2's "not a bug" branch and go to Task 3 with that case as the failing oracle. Record the exact assertion and output in the PR `## Evidence`.

- [ ] **Step 2: Confirm the three render sites already read `@bridge_*` (no code yet)**

```bash
rg -n 'bridge_crew_name|BridgeOpt "crew_name"|bridgeCrewName' \
  config/tmux.conf.tmpl \
  generator/render/status.go \
  scripts/tmux-reflow-windows.sh \
  picker/main.go \
  picker/statusline/main.go
```

Expected hits (compatible, do not "fix" them in this task):

- `config/tmux.conf.tmpl`: `{{index .BridgeOpt "crew_name"}}` / `"crew_color"`
- `generator/render/status.go`: `"crew_name"` / `"crew_color"` in `bridgeOptNames`
- `scripts/tmux-reflow-windows.sh`: `#{@bridge_crew_name}` in `FMT`; `crew="$bcrew"` in the `@bridge_win` arm; `bopt[crew_color]`
- `picker/main.go`: field 20/21 substitution when `bridgeWin`
- `picker/statusline/main.go`: `if a.bridgeCrewName != ""` inside `bridgeWin == "1"`

- [ ] **Step 3: Commit nothing**

This task is evidence only.

---

### Task 2: Probe the two named failure modes the existing cases do not cover

**Files:**
- Read-only unless a probe is red: `picker/remotebridge/daemon/windowlabels.go`, `picker/remotebridge/daemon/subscriptions.go`, `picker/remotebridge/daemon/windows.go`, `scripts/tmux-reflow-windows.sh`, `tests/remote-m2-integration.bats`
- Do not edit floats / layout parser.

**Interfaces:**
- Consumes: Task 1 green (or the red case if Task 1 failed)
- Produces: a written verdict — **viewer-build** vs **this-repo bug** — plus, if the latter, a failing bats case name to implement in Task 3

The M2 DST conf is a stub (`set -g status on`, no `status-format[1]` from tmux-og). Option stamps can be green while houston still looks bare if (a) houston's baked conf lacks the consumer, or (b) this tree stamps options but never forces reflow / misses generation. Probe (b) here. Probe (a) is the version-check command, not a local run.

- [ ] **Step 1: Check whether production wires Reflow to `tmux-reflow-windows --force`**

Read `picker/remotebridge/daemon/daemon.go` `Config.reflow` (nil = off). M2 `bridge_up` only passes `--reflow` when a test asks. Grep the viewer launcher:

```bash
rg -n 'reflow|tmux-reflow-windows' scripts/og-remote-open.sh picker/remotebridge
```

Expected: `scripts/og-remote-open.sh` exports `OG_DAEMON_REFLOW` (the binary path). `--force` is applied in `picker/remotebridge/cmd/daemon/main.go` `reflowRunShellArgs`, not in the launcher string. Wiring is present if both exist. A missing `--force` substring in `og-remote-open.sh` alone is **not** Task 3A. Task 3A only if `OG_DAEMON_REFLOW` is unset in the launcher **or** `reflowRunShellArgs` no longer passes `--force`.

- [ ] **Step 2: Unchanged-value mirror-set change (generation / retireMirror)**

The lead in `docs/agents/bridge-shipped-state.md` and `due()` in `subscriptions.go` is a **mirror-set** change while the **remote value stays unchanged**, so `%subscription-changed` never fires and only `registry.generation` re-reads inside the 8s budget (`windowLabelBackstopInterval` is 30s).

Do **not** probe with `new-window` then `set-option`. `tmux new-window` cannot take a window option, so that sequence is a value change — the same push Task 1 already proves (`nova` → `orbit` at `tests/remote-m2-integration.bats:1932`) — and it greens even if generation re-read is broken. Unit coverage of the gap is `TestLabelShipperFlushReReadsWhenTheMirrorSetMoves` and `TestLabelShipperRestampsRebuiltMirror`; this step is the production-path analogue.

Throwaway bats body (do not commit a green-only speculative test here). One procedure; the other is forbidden.

Before starting the daemon:

1. `$SRC new-session -d -s rem` then `$SRC new-window -t 'rem:{end}' -a` so there are **two** SRC windows. `$SRC set -w -t rem:1 @crew_name nova` only on the window you will retire. Leave the other SRC window untagged. `$DST new-session -d -s host-sess`.
2. Start the daemon the way `:2745` does (do **not** pass `bridge_up`'s first argument as a window count). `bridge_up`'s `$1` is the pane count on `host-sess:1` only (`tests/remote-m2-integration.bats:731-746`); `bridge_up 2` with two single-pane windows never reaches two panes in window 1 and fails the 12s gate. Gate on session-wide `@bridge_pane` the way `:2751` does (`list-panes -s`), and do not continue until **both** DST mirror window ids exist.
3. Record the DST window id whose `@bridge_crew_name` is `nova`.
4. With the daemon **still connected**, `$DST kill-window` that nova mirror **by id**. Do **not** kill the transport. Do **not** copy the `:2739` outage/reattach sequence: that rebuild re-subscribes and re-reports every remote window (`labelShipper.queue`/`apply`), which stamps `@bridge_crew_name` with no `due()` generation check and stays green when `registry.generation` re-read is broken. Emptying the registry (killing the last window) tears `host-sess` down and is a false red.
5. Within 8s (covers `deathSweepDelay` 250ms plus one `mainLoopTickInterval` 5s on the live path `healLostWindows` → `retireMirror` → next `labels.flush`), find the new DST window id that is **not** the killed id and **not** the surviving other mirror. Assert its `@bridge_crew_name` is `nova`. **Do not** touch SRC `@crew_name`.

A red result is the Task 3A oracle (implicated path: `labelShipper.flush` / `due` / `registry.generation`, not a render site). A green **outage-rebuild** is **not** `"generation path green"`. Only a green live-kill two-window probe unlocks Task 3B.

- [ ] **Step 3: Verdict**

Write the verdict into the PR notes (do not invent code):

- **this-repo bug** if: Task 1 red, or `OG_DAEMON_REFLOW` / `reflowRunShellArgs --force` wiring missing, or the rebuild-without-SRC-edit probe fails inside the 8s budget.
- **houston build** if: Task 1 green, that reflow wiring present, rebuild-without-SRC-edit green, and all four consumers already read `@bridge_crew_*`.

- [ ] **Step 4: Commit nothing unless Task 3A starts**

---

### Task 3A: If this-repo bug — red production-path test, then minimal fix

**Files:**
- Modify: `tests/remote-m2-integration.bats` (extend; do not invent a parallel harness)
- Modify only the producer/consumer file the probe implicated (`windowlabels.go` / `subscriptions.go` / `windows.go` / `scripts/tmux-reflow-windows.sh` / launcher). Never both a speculative shipper change **and** a render-site change without a failing test pointing at that site.
- Modify: `docs/agents/bridge-shipped-state.md` only if behaviour changes.

**Interfaces:**
- Consumes: Task 2 verdict = this-repo bug, named failing assertion
- Produces: bats case red on old behaviour, green on the fix

- [ ] **Step 1: Write the failing bats case in `tests/remote-m2-integration.bats`**

Follow the existing "daemon ships the remote's window labels" shape: `$SRC`/`$DST` isolated servers, `bridge_up`, assert `$DST show-options -w -t host-sess:N -qv @bridge_crew_name`. Do not assert against a helper that reimplements `parseWindowLabels`. Include `@crew_color` if colour was part of the fail.

- [ ] **Step 2: Run it and confirm it fails for the claimed reason**

```bash
nix develop -c bats tests/remote-m2-integration.bats -f '<exact new test name>'
```

Expected: FAIL on the new assertion (not a compile/setup failure). Record command + snippet in `## Evidence`.

- [ ] **Step 3: Minimal fix in the implicated file only**

Keep `@bridge_*` names. Do not write `@crew_name` on the mirror. Do not touch float code.

- [ ] **Step 4: Re-run the new case and the three Task 1 cases**

Expected: PASS.

- [ ] **Step 5: Update `docs/agents/bridge-shipped-state.md` Remote Window Labels** if the mechanism changed (e.g. generation, reflow force). If the fix is a missed wiring, document the invariant that was violated.

- [ ] **Step 6: Commit**

```bash
git add tests/remote-m2-integration.bats docs/agents/bridge-shipped-state.md
git add picker/remotebridge/daemon/windowlabels.go   # only files the fix actually touched
git commit -m "$(cat <<'EOF'
fix(bridge): ship crew codename onto mirrors that were staying bare

Closes #739
EOF
)"
```

Adjust the message to the actual root cause.

---

### Task 3B: If houston-build — no code fix; document the finding

**Files:**
- Create none that change runtime behaviour.
- Optional one-paragraph addition to `docs/agents/bridge-shipped-state.md` under Remote Window Labels **only if** it records a diagnosis procedure operators need (version-check). That is documentation of existing behaviour, not a speculative shipper change. Skip the doc edit if the PR body already carries the command and the file would only repeat it.

**Interfaces:**
- Consumes: Task 2 verdict = houston build
- Produces: PR with `## Evidence`, the fish version-check command, consumer map marked compatible, no daemon/format/picker edits

- [ ] **Step 1: Do not change shipper, formats, picker, or reflow**

Acceptance forbids a speculative fix.

- [ ] **Step 2: Append Task 1 command output and the Task 2 verdict under `## Evidence` in this plan file, then commit**

A commit of an already-tracked unmodified plan file is empty. The no-code path needs that evidence section as the non-empty diff (and the PR body's source). Optionally add one diagnosis paragraph to `docs/agents/bridge-shipped-state.md`.

```bash
git add docs/superpowers/plans/2026-09-22-crew-codename-mirror-houston.md
git commit -m "$(cat <<'EOF'
docs: record that houston's bare crew badge is a viewer-build check

The M2 production path already ships @crew_name as @bridge_crew_name.
Closes #739
EOF
)"
```

---

### Task 4: Fast deterministic gate

**Files:** none new

- [ ] **Step 1: Scoped tests**

```bash
nix develop -c bats tests/remote-m2-integration.bats \
  -f 'daemon ships the remote.s window labels onto the mirror windows|a remote label change reaches the mirror on a subscription, not a poll|window labels keep tracking the remote across a reconnect'
(cd picker && go test ./remotebridge/daemon/ -count=1 -run 'Label|WindowLabel|Subscription|FlushReReadsWhenTheMirrorSetMoves|RestampsRebuiltMirror')
```

If Task 3A changed `tmux-reflow-windows.sh`:

```bash
nix develop -c bats tests/reflow-fanout.bats tests/update-icons-all-windows.bats
```

- [ ] **Step 2: Full local gate (acceptance)**

```bash
nix build .#default
nix flake check
nix build .#lint
```

Expected: all succeed.

- [ ] **Step 3: Commit any gate fixes as separate commits, not by amending a pushed commit**

---

## Self-review

1. **Spec coverage:** symptom + houston-unreachable constraint + M2 harness + definite fault side + no speculative fix + docs if behaviour changes + three nix commands — Tasks 1–4.
2. **Placeholders:** none; both 3A and 3B are fully specified.
3. **Types:** `@bridge_crew_name` / `@bridge_crew_color` names are consistent across tasks.
4. **Approach-sanity:** the cheaper explanation (old houston) is Task 1+2 first; code changes only on a red production path.

## Evidence

- bats (nix develop -c bats tests/remote-m2-integration.bats -f 'daemon ships the remote.s window labels onto the mirror windows|a remote label change reaches the mirror on a subscription, not a poll|window labels keep tracking the remote across a reconnect'): all 3 PASS, exit 0.
- Consumers already read @bridge_crew_*: config/tmux.conf.tmpl:360 BridgeOpt crew_name/crew_color; generator/render/status.go:65-66 bridgeOptNames; scripts/tmux-reflow-windows.sh FMT @bridge_crew_name, crew=$bcrew, bopt[crew_color]; picker/main.go fields 20/21 when bridgeWin; picker/statusline/main.go bridgeWin arm draws bridgeCrewName.
- OG_DAEMON_REFLOW exported in scripts/og-remote-open.sh:470; reflowRunShellArgs in picker/remotebridge/cmd/daemon/main.go:918-919 passes --force.
- Live two-window kill probe: DST @0 had nova, killed @0 with daemon connected, SRC @crew_name untouched; at 0.341s replacement @2 had @bridge_crew_name=nova @bridge_win=1. Not a 12s missed-nudge. Generation path green.
- Verdict: houston build (viewer tmux-og predates or differs), not this-repo shipper bug.
