# Plan: parked-cursor reshape hold (#1002, revision)

Spec: `docs/superpowers/specs/2026-10-19-bridge-reshape-hold-parked-cursor-design.md`
(revises `2026-10-18-bridge-reshape-hold-design.md`). Code under
`picker/remotebridge/`. The branch head before this plan is e150043.

Go gate per step (run from `picker/`):

- `go test -race ./remotebridge/daemon/ -count=1`. Six live-tmux tests also fail
  on origin/main on this machine and are known environmental failures:
  - `TestCarouselResolveScript*`
  - `TestEnrichRefreshVerb*`
  - `TestCtlSafeRealFormatsAgainstLiveTmux`
- `go vet`, `gofmt -l`, `golangci-lint run ./remotebridge/daemon/...`
- `nilaway -include-pkgs="$(go list -m)" ./remotebridge/...`

## Step 1. Hold bytes, read and barrier (`daemon/hold.go`, `hold_test.go`) — implement: sonnet

- **`holdBytes(rows, h)`** emits:
  - `ESC \ ESC[?1049h ESC[<h>;1H`;
  - then, for `i = h…1`, `ESC[<i>;1H ESC[m ESC[2K` + `rows[i-1]`;
  - then `ESC[1;1H`.
  - Drop the cursor arguments, and `cursor_x`/`cursor_y` from the header.
  - The comment explains the parked cursor (the `eat_empty` deletes from the bottom down to the cursor) and the bottom-up order (clamped CUPs on a late hold).
- **`holdHeaderFormat`** drops `#{alternate_on}`, `#{window_height}` and the cursor fields:
  `#{@bridge_pane}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{pane_dead}` (5 fields).
  - `holdRead` keeps only `local, holdable, w, h, rows`.
  - `holdable` = alternate-screen 1 and dead 0. Whether the pane is already held is the pump's call (Step 2), not the read's.
- **`holdPollArgs`** polls `#{alternate_on}|#{cursor_y}`.
  - `awaitAlt(cfg, locals, want)` is kept, and a hold waits for `want = "1|0"`.
  - `allAlt` is kept.
- **Constants** next to `holdWait`:
  - `holdFreezeMax = 1 * time.Second`, with a comment saying why it is longer than `holdWait`: it covers the reseed's WAN round trip, while `holdWait` covers only a local barrier.
  - It is a package var, so tests can shrink it.
- **Tests:**
  - exact bytes for h=3;
  - a late-hold table showing the bytes are bottom-up with a final `1;1H`;
  - the new header parse (5 fields, a dead pane, `alternate-screen` off, a 4-field header stops);
  - poll argv;
  - `allAlt` accepts `1|0` and rejects `1|5` and `0|0`.

## Step 2. Pump: filter-and-discard while held, release-as-reseed, deadline (`daemon/daemon.go`, `predictor.go`, `holdsink_test.go`) — implement: opus

Needs Step 1 (`holdFreezeMax`). In `outputSink.start`, alongside the pump-local `held`:

- **Published state:** `outputSink.heldScreen atomic.Bool`, set and cleared only by the pump wherever `held` changes. `func (s *outputSink) isHeld() bool` is nil-safe and is for tests and logging only. Nothing branches on it.
- **Deadline:** `holdTimer *time.Timer`, nil unless held. The pump's `select` gains `case <-holdC:`, where `holdC` is the timer's channel or nil.
- **`frameHold`:**
  - If `held` is already true, drop the frame: write nothing and do not restart the timer.
  - Otherwise as now (`onHold` unpaint + observe + write). Then `held = true`, publish it, and start `holdTimer(holdFreezeMax)`.
- **`wire.FrameOutput` while held:**
  - After `drainOutput`, run the batch through `kn.Feed`, then `gfx.Filter` + `hasImages.Store`.
  - Then drop it. Do not write it, do not `pred.observe` it, do not `pred.onOutput` it, do not `s.mouse.Feed` it. The tracker mirrors the bytes written, and the seed updates it.
  - `continue`.
  - Factor the shared filter prefix out of the not-held path, so both run the same chain.
- **`wire.FrameSeed`:** stop the timer, set `held = false` and publish it. Then the existing seed path (predictor `onSeed`, `gfx.Replay`, write).
- **`frameRelease`:**
  - If `!held`, `continue`.
  - Otherwise raise `dropped` under `s.mu` and stay held. The reseed's seed releases; there is no `?1049l`.
- **Deadline fires while held:**
  - Write `?1049l` first, observed, plus `s.pred.reset()` bytes ahead of it if a predictor exists.
  - Raise `dropped` under `s.mu`. Set `held = false`, publish it, clear the timer.
- **`framePredictKeys` while held:** write `s.pred.reset()` output only, then `continue`.
- **`framePredictSeed` while held:** ignore it.
- **Channel closed while held:** no `?1049l`, since the pane is going away. Stop the timer.
- **Tests** (`TestSinkHold*`, with a shrunk `holdFreezeMax` except where noted):
  - Hold, then output `X`, then a seed. The peer reads the hold, then the FrameSeed, and no frame carries `X`.
  - The pump keeps state for discarded output:
    - a kitty store in discarded output is retained and replayed before the seed;
    - a `?1006h` in discarded output does NOT update `s.mouse`; the seed's modes do.
  - Hold, hold, seed: the peer reads exactly one hold output, then the seed.
  - Hold, seed, hold: the peer reads both holds.
  - A release while held writes nothing, and `takeDirty` reports a drop. A later seed releases the pane, and `isHeld` goes false.
  - Deadline:
    - With `holdFreezeMax` shrunk to 50 ms, a hold with no seed leads the peer to read `[?1049l` after the deadline, then output `Y`, sent after the deadline, as a normal FrameOutput. `takeDirty` reports a drop and `isHeld` is false.
    - The real constant: with `holdFreezeMax` at 1 s, the release comes no earlier than 0.9 s. Lower bound only.
  - `isHeld` is true after the hold frame is written, and false after a seed.
  - Predictor, built the way `TestPredictorHoldUnpaintsAndDropsModel` does it:
    - keys during a hold produce no paint bytes;
    - a `framePredictSeed` during a hold builds no model;
    - output during a hold is neither written nor observed;
    - after the seed, a key and a model seed predict again.
  - Existing cases are updated: a release with no seed no longer writes `?1049l`.

## Step 3. Reconcile wiring (`daemon/hold.go` holdReshaped, `reconcile.go`, `reconcilehold_test.go`) — implement: opus

- **`holdReshaped(cfg, w, L, remote, newRemote, structural, zoomed, remoteActive, router) []*outputSink`:**
  - Remove the Append, Swaps, `zoomed`, `w.appliedZoom` and window-height gates.
  - Do not skip candidates that are already held. Every holdable candidate is read and enqueued, and the pump drops a hold that arrives while it is still holding (Step 2). Local `alternate_on` is not read.
  - Restore the `zoomHides` candidate rule: in a zoomed window only `remoteActive` is a candidate, with target `L.W×L.H`.
  - `force := structural`. Keep the round-4 `force` comment: next-3.9 gives the freed space to the next cell, 3.6a/3.7c to the previous.
  - Barrier `awaitAlt(…, "1|0")`.
- **`reconcile.go`:**
  - Remove the loop-top release and `holdOK`.
  - `held = append(held, holdReshaped(...)...)`, so holds carry across passes. A pane that is still held is re-enqueued, and the pump drops the duplicate.
  - The deferred release at exit is kept.
  - Drop `heldPane` and `releaseHeld` if nothing else uses them.
- **Tests** (`TestReconcileHold*`):
  - An Append pass, a swap pass, a zoomed pass and a locally zoomed window now hold, and in a zoomed window only the zoomed pane is read.
  - The close order is read → poll → kill-pane.
  - A pass-2 candidate whose pump is still holding it is read and enqueued. The second hold writes nothing, and nothing writes `?1049l` between the passes.
  - A pane that reads as on the alternate screen (an app on its own alternate screen) is held.
  - The `force` test is kept.
  - A desync `resetWindow` inside a held pass makes no release before the rebuild.
  - The nil `LocalTmuxOut`, read-error, dead-pane and refused-select-layout cases are kept: the refused case now leaves the pane held, with a drop raised.

## Step 4. Bats (`tests/remote-m2-integration.bats`) — implement: sonnet

- **Split-swap case:** becomes "an unzoomed split of the top of a stacked pair keeps the bottom pane's rows in place (#1002)".
  - Drop the "no `alternate_on` 1" assertion and assert the positional oracle.
  - Stall prefix `120 3 `.
- **Cross-pass case:** assert pass 1 held the pane. That is the shell re-review's fix: some `post-*` sample has header `120 1 1`, checked before the drop loop.
  - Its comment says the wrapper's `sleep 0.3` opens that window.
- **New cases** with the existing helpers:
  - "a remote rotate-window keeps a held pane's rows in place (#1002)". Three stacked panes of different heights, with the survivor's cursor low. `$SRC rotate-window -t rem`. The wrapper stalls after the local `swap-pane`.
  - "a remote zoom of a pane in a changed layout keeps its rows in place (#1002)". A zoom-on transition where `select-layout` first shrinks the pane. Build it with a remote `resize-pane` plus `resize-pane -Z` in one remote command list, so the mirror sees a new layout string and the zoom flag together. The wrapper stalls after `select-layout`.
- **Red/green:**
  - Each new case must fail on `$S/headbin` (801f494, the first hold design) or `$S/r3bin`/`$S/r4bin` where that version holds on the path, and pass with the revision.
  - Record which builds go red. If a case cannot go red on any earlier build (because no earlier build held on that path), say so; it is then a coverage case for the revision.
- The late hold on an already-shrunk pane is not deterministic from bats (it needs the barrier to miss). It is pinned by the Step 1 bytes test and the Step 5 probe.

## Step 5. Acceptance probes and freeze measurement (lead, scratchpad)

- **Probe matrix.** Re-run the 11-path matrix (`assess/cases.sh`, `widen.sh`, `zwiden.sh`, `wide.sh`) with a mode that emits exactly the new `holdBytes`. The fault paths:
  - a carried hold (`crosspass/probe3.sh` adapted);
  - a back-to-back reconcile with an unawaited release;
  - split/select-layout on a held pane 0 before `respawn-pane -k`;
  - a late hold on a shrunk pane (`r5probe/clamp.sh` adapted).

  Every case must show 0 positional violations. Record the commands and the decisive lines.
- **Freeze.** A control-mode sampler at ~2–3 ms on the survivor's `#{alternate_on}` over 20 closes, idle and under capped CPU load (Deliberate load rules). Report hold-to-seed duration p50 and max. Also confirm no hold outlives `holdFreezeMax`.

## Step 6. Docs — implement: sonnet

- **`docs/agents/bridge-daemon.md`:**
  - Rewrite the #1002 bullet to the revision: the parked cursor and why, bottom-up rows, the barrier `1|0`, deferral, prediction off while held, release-as-reseed, `holdFreezeMax`, holds on every pass, the residuals, the pins.
  - In the predictive-echo section, add the held-pane rules: keys reset only, model seed ignored, output while held filtered and discarded, never written or observed. Add the held pump paths (discard, release-as-reseed, deadline `?1049l`) to the THE-invariant list.
- **The 2026-10-18 spec and plan:** one line at the top of each pointing to the revision.

## Step 7. Gates and delivery (lead)

- Code-review gate on the revision (a fresh 2-round budget; a recurrence in the family after 2 rounds blocks).
- `nix build .#lint`, `nix flake check` (load-flaky checks rerun individually, any that also fail on origin/main named), `nix build .#default`.
- Push and open the PR, with option A's latencies in `## Testing`.

## Order

- Step 1, then Step 2 (needs `holdFreezeMax`), then Step 3 (needs 1 and 2).
- Step 4 (written, red half) and Step 6 run in parallel with Steps 1–3.
- Then the green bats run, Step 5, Step 7.
