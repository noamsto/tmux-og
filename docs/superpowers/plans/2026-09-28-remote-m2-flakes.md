# Plan: #801 — remote-m2 bats flakes (revision 1)

## Findings that ground this plan

- #785's failing CI job (`gh run view 36314628607 --attempt 1 --log-failed`) printed nix's
  "Last 25 log lines" = `ok 49` … `ok 73`; the suite had 73 cases at that commit
  (`git show 1a35ff8:tests/remote-m2-integration.bats | rg -c '^@test'` → 73). All 73
  ran; the `not ok` was in cases 1–48, above the tail — "after case 61" is a misread.
  The linux sandbox log is not recoverable, so that exact case is unknown.
- Harness (committed to the artifacts dir, shellchecked):
  `<A>/harness/run1.sh` — one full or filtered suite run: copies a tests dir to a fresh
  `/tmp/og801.XXXXXX`, exports the check's env (`DAEMON/RENDERER/CTL/STATUSLINE` from
  `$GO_TOOLS/bin`, default = main's check go-tools
  `/nix/store/q446rjg08f9rcr3d8mjpjjwlrc5sfng3-tmux-og-go-tools-0.1.0`, `DETACH`, the
  check's pinned `tmux-next-3.9` first on PATH, `HOME/TMPDIR/CLAUDE_STATUS_DIR` inside the
  run dir, `TMUX`/`TMUX_PANE` unset), runs `bats [args] tests/remote-m2-integration.bats`,
  appends `rc=N`, trashes the run dir. The store paths come from
  `nix derivation show $(nix eval --raw .#checks.x86_64-linux.remote-m2-integration-tests.drvPath)`
  (`.env.DAEMON` etc.), realised with `nix-store -r`.
  `<A>/harness/batch.sh <tests> <tag> <par> <rounds>` — full suite, `par` parallel × rounds.
  `<A>/harness/cbatch.sh <tests> <tag> <par> <rounds> <filter>` — same, `bats -f <filter>`.
  Load = `systemd-run --user --scope --unit=og801-<tag> -p CPUQuota=300% <batch> …`
  (12 suites sharing 3 CPUs ≈ 0.25 CPU each; 8 suites at 400% never failed).
  `<A>` = `/home/noams/git/tmux-og/.git/crew/artifacts/feat/801-fix-tests-find-and-fix-the-remote-m2-bat`.
- Main baseline (HEAD 520af12, tests copied verbatim to scratch, `batch.sh … 12 3`
  → 36 runs): so far 24 done, **2 failures, two distinct flakes**, TAP saved to
  `<A>/evidence/`:
  1. `hmain-2-5.tap`: `not ok 67 a SIGTERM while parked exits cleanly and tears down the mirror`
     — `wait_bridge_state(spk): want=parked last=disconnected`; daemon log 5×
     `identity read for rem: connection closed before reply`; never parked in 15s.
     Darwin CI shows this signature on another park case (run 36319654425, `not ok 55`,
     `wait_bridge_state parked mpk`).
  2. `hmain-3-12.tap`: `not ok 21 the relay capability follows whichever client is attached to the mirror session`
     — line 1351 `$OBS new-session -d -s obsB …` failed: `server exited unexpectedly`.
- Root cause 1 (daemon): `attemptCycle` (conn.go:392) calls `bo.Next(attempt, start)`,
  which checks `elapsed >= MaxElapsed` then returns the full jittered `delay(attempt)`
  (Base·2^(attempt-1), cap Ceiling); the loop `Wait`s it. So MaxElapsed bounds when the
  last sleep *starts*: with `OG_DAEMON_RETRY_MAX_ELAPSED=2s` / `OG_DAEMON_WAKE_MAX_ELAPSED=2s`
  (Base 500ms, Ceilings 30s / 5s untouched), fast-failing low-jitter dials put attempt
  5–6 just under 2s and its sleep can be 8–16s, past `PARK_WAIT_BUDGET_SECS=15`.
  Violated invariant: backoff.go's doc "MaxElapsed is the bound that governs"; the
  bats comment (3258-3261) "bounded near 2s of dials plus scheduling". Affects the
  retry, wake-then-reexhaust, and restore paths alike.
- Root cause 2 (harness): case 21 kills `obsA`, the ONLY session on the `m2obs` server,
  so the server exits (tmux default `exit-empty on`) and the immediate
  `new-session -s obsB` on the same socket reaches the dying server (#832's class). The
  test's comment assumes kill-session keeps the server — false with one session.
- Rejected hypothesis: `wait_bridge_disconnected` catching a transient stamp — 108
  targeted runs of its 5 callers (`cbatch`-style filter) under the same load, 0 failures.
  Not changed.

## Consumer map (root cause 1: Backoff.Next's returned delay)

- Producer: `Backoff.Next` / `delay` (backoff.go). CHANGED.
- Sole consumer: `attemptCycle` (conn.go:392) → `Wait(d, cfg.Shutdown)`. Compatible:
  shorter final wait; next iteration's `Next` reports exhausted.
- Schedules: `retrySchedule` (DefaultBackoff), `wakeSchedule` (WakeBackoff),
  `restoreSchedule` (RestoreBackoff) — each now ends ≈ MaxElapsed + one dial instead of
  up to MaxElapsed + Ceiling. `probeSchedule` (MaxElapsed 0) — unaffected.
- `cmd/daemon` `--*-max-elapsed` / `OG_DAEMON_*_MAX_ELAPSED` — compatible, honoured tightly.
- Bats restore-window cases (4006 kill-session after refusal; 4164 recreate `rem` in
  the 15s window; 3926/4028) lose the overshoot slack — covered by the full-suite load run.
- Go fakes: reattach_test.go:204/385/610, park_test.go, backoff_test.go — gate runs them.

## File list

- `picker/remotebridge/daemon/backoff.go` — clamp `Next`'s delay to the remaining
  MaxElapsed; update doc comment.
- `picker/remotebridge/daemon/backoff_test.go` — regression test.
- `tests/remote-m2-integration.bats` — case 21: `$OBS set -g exit-empty off` before
  `kill-session -t obsA`, and correct its comment; park budget comment (≈3258) to say
  the cycle ends at MaxElapsed plus one dial.
- `docs/agents/bridge-daemon.md` — one sentence: the final retry wait is clamped to the
  schedule's MaxElapsed.
- `docs/superpowers/plans/2026-09-28-remote-m2-flakes.md` — this plan (repo convention).

## Steps

- [ ] **Step 1: failing regression test** — `backoff_test.go`:
  `TestBackoffNextNeverWaitsPastMaxElapsed`: `Backoff{Base: 500ms, Ceiling: 30s,
  MaxAttempts: 40, MaxElapsed: 2s, Now: fake, Jitter: noJitter}`; now = start+1.9s;
  `d, ok := b.Next(6, start)` → want `ok && d <= 100ms` (unclamped 16s); and now = start,
  `Next(1)` → exactly 500ms (early attempts untouched).
  Proof: `cd picker && go test ./remotebridge/daemon/ -run TestBackoffNextNeverWaitsPastMaxElapsed`
  → FAIL reporting 16s. Save output to `<A>/evidence/red.txt`.
- [ ] **Step 2: clamp** — `Backoff.Next`: read `elapsed := b.Now().Sub(start)` once; after
  the bound checks `d := b.delay(attempt)`; `if b.MaxElapsed > 0 && d > b.MaxElapsed-elapsed { d = b.MaxElapsed - elapsed }`.
  Doc: the delay never carries a wait past MaxElapsed, so a cycle ends at MaxElapsed
  plus one attempt. Proof: Step 1 command → PASS;
  `cd picker && go test ./remotebridge/...` → ok.
- [ ] **Step 3: case 21 harness fix** — in the bats case, insert
  `$OBS set -g exit-empty off` immediately before `$OBS kill-session -t obsA`; rewrite
  the comment above to state obsA is the server's only session, so the server would
  exit with it and the next new-session would reach the dying server; exit-empty off
  keeps it up (teardown's kill-server still stops it). Update the park-budget comment.
  Proof: `bats -f 'the relay capability follows' tests/remote-m2-integration.bats` via
  `run1.sh` → ok.
- [ ] **Step 4: doc** — docs/agents/bridge-daemon.md ≈ line 295, one sentence; commit
  the plan copy under docs/superpowers/plans/.
- [ ] **Step 5: fast gate** — `cd picker && go vet ./remotebridge/... && go test ./remotebridge/...`;
  `nix build .#default`; `nix build .#lint`; `nix flake check`.
- [ ] **Step 6: load proof** — get the fixed go-tools path via the `nix derivation show`
  command above, `nix-store -r` it; copy the worktree's `tests/` to scratch `fix-tests`;
  `GO_TOOLS=<fixed> systemd-run --user --scope --unit=og801-fix -p CPUQuota=300% <A>/harness/batch.sh fix-tests fix 12 3`
  → 36/36 `rc=0`. Baseline: `<A>/harness/batch.sh main-tests hmain 12 3` (same shape,
  main tests + main go-tools) → ≥1 failure (already 2). Plus targeted: case 21 via
  `cbatch.sh … 12 10 'the relay capability follows'` on main vs fix, and park cases via
  `cbatch.sh … 'parked'` on main vs fix, reported as counts.
  Cleanup: `pgrep -af 'og801|og-m2-bats'` empty; `gtrash put /tmp/og801.*`.

## Acceptance mapping

- Flaking cases named + trimmed output → `<A>/evidence/*.tap` excerpts in PR `## Testing`.
- N≥30 consecutive under load with fix, ≥1 failure on main in a comparable run →
  Step 6 (36 fix runs vs 36 main runs, same harness/shape).
- `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 5.
- Load processes cleaned → Step 6 cleanup.
- Disclosure: #785's exact case unrecoverable (truncated log); the PR says the two
  fixed flakes are the ones reproduced locally, one matching darwin CI's signature.

## Amendment (execute): third flake from the targeted park baseline

- `cbatch.sh main-tests pmain 12 10 parked` on main: `not ok 4 a parked mirror whose remote
  server restarted without its session ends in a tombstone` — `[[ $tomb_text == *"no longer
  exists"* ]]` failed. `tombstoneMirror` (reopen.go) respawns the mirror into a fresh
  `sh -c printf …; read` pane, so the text paints asynchronously after the daemon exits;
  the test captured it once. Fix: poll `capture-pane` for the text, bounded (5s), in the
  same case. Only site; the other tombstone check (≈4133) already polls.

## Amendment (execute): fourth flake — found by the fix load run

- Fix batch (36 full runs): 35 ok, 1 `not ok 93 daemon rebuilds a mirror holding a pane the
  remote layout does not name` (`[ "$dst_panes" -eq 2 ]`). Targeted on main vs fix
  (`cbatch … 'pane the remote layout does not name'`, two scopes at once): 14/120 vs 13/120
  failures — pre-existing, independent of the backoff change.
- Instrumented (pre-kill dump of panes + daemon log) shows two harness races:
  A. The test's first loop waits, unasserted, only for 2 local panes, then `split-window`s the
     mirror before the daemon finished wiring: the daemon logs
     `mirror for @0: 3 local panes for 2 remote` and exits, taking the session (DST server gone).
  B. After the remote reshape, the loop breaks on a transient 2-pane count mid-rebuild and
     samples dims before the reshape lands (`dst=[200x49]` while the final layout is 60/139).
- Fix (test only): start the daemon with `bridge_up 2 d9` (stamps + painted marker, same
  daemon flags) instead of the hand-rolled launch; after the reshape poll until the pane count
  is 2 AND `sorted_tiled_dims` agree, bounded by `RESIZE_CONVERGE_BUDGET_SECS`.
