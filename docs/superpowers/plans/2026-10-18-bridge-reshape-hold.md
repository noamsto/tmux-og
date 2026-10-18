# Plan: hold a mirror pane on the alternate screen across a local reshape (#1002)

> Superseded for the hold design by `2026-10-19-bridge-reshape-hold-parked-cursor.md`.

Spec: `docs/superpowers/specs/2026-10-18-bridge-reshape-hold-design.md`.
Code lives in `picker/remotebridge/` unless a path says otherwise.
Go gate per step: `cd picker && go test -race ./remotebridge/...` and `golangci-lint run ./remotebridge/...`.

## Steps

### Step 1. `render.Seed` resets the pen after `?1049l` (implement: haiku)

- `render/snapshot.go` `Seed`: write `\x1b[m` immediately after `\x1b[?1049l`.
  Comment: `?1049l` restores the pen saved at `?1049h` (a hold, or an earlier
  alt-screen seed), and the `2J` below erases with it.
- `render/snapshot_test.go`: update every expected byte string that contains
  `\x1b[?1049l` to carry the new `\x1b[m`. Add one case: the bytes after
  `\x1b[?1049l` begin with `\x1b[m`.
- Done: `go test ./remotebridge/render/...` green.

### Step 2. `daemon/hold.go`: pure hold helpers (implement: sonnet)

New file `daemon/hold.go`, unit tests in `daemon/hold_test.go`.

- `holdHeaderFormat = "#{@bridge_pane}|#{alternate_on}|#{alternate-screen}|#{pane_width}|#{pane_height}|#{cursor_x}|#{cursor_y}"`.
- `holdReadArgs(locals []string) []string`: for each local pane,
  `display-message -p -t <L> <holdHeaderFormat>`, `;`,
  `capture-pane -e -p -t <L>`, joined by `;` separators (the
  `windowlabels.go` argv style), with no trailing `;`.
- `type holdRead struct { local string; holdable bool; w, h, cx, cy int; rows []string }`
  plus `parseHoldRead(out string, locals, remoteIDs []string) []holdRead`.
  - Walk `out` split on `\n` in order. For each pane, the header must have
    exactly 7 `|` fields, and field 0 must equal its remote id.
  - Fields 3–6 are non-negative ints. Then take exactly `h` rows.
  - `holdable` is true iff field 1 is `0` and field 2 is `1`.
  - A malformed header or a short reply returns the panes parsed so far and
    stops: positional parsing cannot resync after a bad block.
- `holdBytes(rows []string, cx, cy int) []byte`: `\x1b\\`, `\x1b[?1049h`,
  then for row i `\x1b[<i+1>;1H` + row, then `\x1b[<cy+1>;<cx+1>H`.
- `holdPollArgs(locals []string) []string`: `display-message -p -t <L> '#{alternate_on}'`
  per pane, `;`-joined.
- Tests (table-driven where shape allows):
  - read argv;
  - parse of two panes;
  - id mismatch stops;
  - short capture stops;
  - `alternate_on 1` → `holdable=false`;
  - `alternate-screen 0` → `holdable=false`;
  - hold bytes exact for a 2-row case.

### Step 3. Sink and pump: `frameHold` / `frameRelease` (implement: sonnet)

`daemon/daemon.go` beside `framePredictKeys`, `daemon/predictor.go`, tests in
`daemon/holdsink_test.go`.

- Constants `frameHold wire.FrameType = 0xf2`, `frameRelease = 0xf3`, in the
  daemon-internal block. Its comment names them.
- `func (s *outputSink) hold(payload []byte) bool`: under `s.mu`. False when
  closed or paused. A non-blocking send that returns false on a full channel,
  without touching `dropped`.
- `func (s *outputSink) release()`: under `s.mu`. A no-op when closed. A
  non-blocking send of `frameRelease`. On a full channel, `dropped++`: the
  reseed's seed releases.
- `predictor.onHold() []byte`: `out := p.reset()`, then drop the model if one
  exists, then return `out`.
- Pump, `outputSink.start`, holding one goroutine-local `held bool`:
  - `frameHold`: if `s.pred != nil`, write `s.pred.onHold()` via
    `writeOutputIfAny` and `s.pred.observe(payload)`. Write the payload with
    `wire.WriteStream(conn, wire.FrameOutput, …)`. Set `held = true`, then
    `continue`.
  - `frameRelease`: if `!held`, `continue`. Otherwise set `held = false`,
    unpaint via `s.pred.reset()` if a predictor exists (written first), then
    write `\x1b[?1049l` as output (observed), then `s.mu.Lock(); s.dropped++;
    s.mu.Unlock()`, then `continue`.
  - `wire.FrameSeed`: set `held = false` in the existing case.
- Test names start `TestSinkHold`. Tests use `net.Pipe` + `newOutputSink`, read frames from the peer, and poll
  `takeDirty` with a deadline. Cases:
  - Hold, then a seed, then a release: the peer reads output(hold bytes),
    then seed, and nothing else. `takeDirty` stays 0.
  - Hold, then a release: the peer reads output(hold), then output
    `\x1b[?1049l`. `takeDirty` reaches 1.
  - A release with no hold: nothing written.
  - `hold` on a full channel (no pump, cap-1 channel pre-filled) returns
    false and leaves `dropped` 0.
  - `hold` on a paused sink returns false.
  - `release` on a full channel raises `dropped`.
  - A predictor with a model: `onHold` returns the `reset` bytes and leaves
    `p.model == nil` (in-package test, built the way `predictor_test.go`
    builds one).

### Step 4. Wire the hold into `reconcileSnapshot` (implement: opus)

`daemon/hold.go` (orchestration) and `daemon/reconcile.go`, tests in
`daemon/reconcilehold_test.go`.

- `var holdWait = 200 * time.Millisecond` and `holdPoll = 2 * time.Millisecond`
  (package vars so tests can shrink them).
- `holdReshaped(cfg Config, w *mirrorWindow, L controlmode.Layout, remote, newRemote []string, structural, zoomed bool, remoteActive string, router *Router) []*outputSink`:
  1. Candidates, in `newRemote` order: ids in `remote` too, with
     `router.sink(id)` non-nil and not paused, and a local pane from
     `localPaneFor(w, remote, id)`. In a zoomed window
     (`zoomed && indexOf(newRemote, remoteActive) >= 0`), keep only
     `remoteActive`. Target dims are `L.Panes[indexOf(newRemote,id)]`, or
     `L.W×L.H` for the zoomed pane.
  2. No candidates, or `cfg.LocalTmuxOut == nil` (many reconcile test fakes
     leave it unset, as `localpanes.go`/`ownership.go` already guard): return
     nil with no exec.
  3. `cfg.LocalTmuxOut(holdReadArgs(locals)...)`. On an error, return nil.
     `parseHoldRead`.
  4. `force := structural || (zoomed && L.Raw != w.layout)`. The second term
     covers a zoomed geometry-only pass, whose `select-layout` unzooms and
     whose `assertMirrorZoom` re-zooms, so the zoomed pane is shrunk and
     regrown although its final dims equal the target. For each parsed pane
     with `holdable` set and (`force` or (w,h) differs from its target): `sink.hold(holdBytes(rows, cx, cy))`. Keep the sinks
     whose `hold` returned true, with their local ids.
  5. Barrier, its own function `awaitAlt(cfg Config, locals []string, want string) bool`
     so tests drive it directly: until `holdWait` elapses, call
     `cfg.LocalTmuxOut(holdPollArgs(heldLocals)...)` and stop once the reply
     holds exactly `len(heldLocals)` lines, all `want` (`1` after a hold, `0` after a release), sleeping `holdPoll`
     between tries. An exec error stops polling at once: the read is not
     coming back within the window. `awaitAlt` returns
     false on a timeout or error; `holdReshaped` then logs
     `daemon: hold %s: not confirmed in %v` once and continues.
  6. Return the held sinks.
- `reconcileSnapshot`: after the same-shape early-return block, declare
  `var held []*outputSink` and
  `defer func() { for _, s := range held { s.release() } }()`. In the pass
  loop, after `structural` is computed and before the `switch`:
  `held = append(held, holdReshaped(cfg, w, L, remote, newRemote, structural, zoomed, remoteActive, router)...)`.
  Comment why it sits there: tmux reflows a main-screen pane the instant the
  reshape below lands (spec: Confirmed mechanism).
- Test names start `TestReconcileHold`. Tests, with fakes in the style of `reshapereseed_test.go` (`scriptedRT`,
  `net.Pipe` sinks, a recording `LocalTmux`/`LocalTmuxOut`):
  - **Close.** Two panes → one. `LocalTmuxOut` answers the hold read with a
    valid two-block-shaped reply for the survivor and `1` to the poll. Assert
    the order of the local calls: hold read, then poll, then `kill-pane`.
    The survivor's peer reads the hold output frame before its resize and
    seed. No release bytes follow, since the seed released it.
  - **Refused `select-layout`.** `LocalTmux` errors on `select-layout`, and
    the pane-count read reports no desync. The peer reads the hold, then the
    `\x1b[?1049l` release, and `takeDirty` reports a drop.
  - **Zoomed.** The window is zoomed with `remoteActive` `%3`: only `%3`'s
    local pane is in the hold read.
  - **Geometry-only, unchanged pane.** A pane whose read dims equal its
    target is not held: no hold frame and no poll.
  - **Read error.** `LocalTmuxOut` fails on the read: no hold, no poll, and
    the pass is unchanged (`kill-pane` still runs).
  - **Nil `LocalTmuxOut`.** A close with only `LocalTmux` set completes, with
    no hold and no panic.
  - **Barrier.** A short poll reply (one line for two held panes) does not
    pass. A poll error stops after one call. Both call `awaitAlt`
    directly with a counting fake.
  - **Zoomed geometry-only.** The window is zoomed with `L.Raw != w.layout`
    and the zoomed pane's read dims already equal `L.W×L.H`: it is held.
  - Then run the whole `./remotebridge/daemon` suite. Any existing test whose
    `LocalTmuxOut` fake is call-order sensitive gets its fake taught to
    answer the hold read (a header that fails the id check is enough). Never
    weaken an assertion.

### Step 5. Bats regression (AC2) (implement: sonnet)

`tests/remote-m2-integration.bats`, right after
`daemon reconciles a mid-list remote kill (non-last pane)`.

- Name: `a remote pane close never shows the survivor a frame outside its pre-close and settled screens (#1002)`.
- Slow wrapper as in
  `a geometry-only layout change still holds live output until the local reshape lands`:
  sleep 1.5 on an `m2dst` + `select-layout` call while `$BATS_TEST_TMPDIR/slow.on`
  exists.
- The remote survivor reads a FIFO:
  `$SRC new-session -d -s rem -x 120 -y 30 "exec 3<>'$fifo'; cat <&3"`.
  Then `$SRC split-window -h -t rem 'sleep 600'`.
- The local pane carries a banner the remote never had:
  `$DST new-session -d -s host-sess -x 120 -y 30 'i=0; while [ $i -lt 40 ]; do i=$((i+1)); echo LOCALJUNK$i; done; sleep 600'`.
- `PATH="$BATS_TEST_TMPDIR/slowbin:$PATH" bridge_up 2 hold`.
- Live output after the mirror is up: 12 lines of 87 characters
  (`printf 'wrap%02d-%080d\r\n'`) into the FIFO. Wait until the survivor's
  mirror pane shows `wrap12`.
- Address the survivor by id, never by index (CLAUDE.md): its remote id from
  `$SRC list-panes`, its local `%N` from `$DST list-panes -F '#{pane_id} #{@bridge_pane}'`.
- `pre` = the survivor's local capture, trailing spaces trimmed.
- `touch slow.on`, then `$SRC kill-pane` on the right pane. Sample the
  survivor every 0.05 s for 3 s, each sample to its own file: first line
  `#{pane_width}` (`display-message -p`), then `capture-pane -p`. `rm slow.on`.
- Wait for settle: survivor width 120 and two equal captures 0.3 s apart.
  `settled` = that capture.
- Oracle: every non-blank row of every sample (trailing spaces trimmed) is a
  row of `pre` or `settled`. On a miss, print the offending sample with `>&3`
  before failing. Also assert at least one sample was taken during the stall
  (the sampler names a sample `stall-<n>` when `slow.on` exists at the
  moment it is taken; assert at least one `stall-*` sample whose width line
  reads 120, i.e. taken after the local `kill-pane` and before the stalled
  `select-layout` returned), so the
  oracle cannot pass vacuously.
- Red/green, run by the lead:
  - `DAEMON`/`RENDERER` built from origin/main (`git worktree add` a scratch
    copy at `$base` in the scratchpad) must fail on `LOCALJUNK`.
  - Built from this branch, it must pass.
  - Record both outputs in `## Testing`.

### Step 6. Docs (AC4) (implement: sonnet)

`docs/agents/bridge-daemon.md`:

- New bullet in "Mirror invariants" after the #951 bullet: **A pane the daemon
  is about to reshape locally is held on the alternate screen until its seed
  (#1002).** One paragraph:
  - the mechanism: reflow with local-only history and wrap flags;
  - the hold, read, barrier and release (pump-owned, reseed on release);
  - what is held: structural or dims change, main screen, `alternate-screen`
    on, the zoom rule;
  - why sync (2026) does not help: resize and damage redraws bypass it;
  - the residuals;
  - pinned by the new bats case and the `TestReconcileHold*` /
    `TestSinkHold*` tests.
- Predictive echo, "THE invariant" bullet: add `hold = onHold bytes first;
  release = reset bytes first` to the list of pump write paths.
- "Every pane is re-seeded after a layout reshape" bullet: one clause noting
  that the seed now also releases the hold.

### Step 7. Gates (lead)

- `cd picker && go test -race ./...`
- `nix build .#lint`
- `nix flake check`
- `nix build .#default`
- Locally run `bats tests/remote-m2-integration.bats -f '#1002'` plus the
  close/split/zoom/swap/layout cases, using the prebuilt `DAEMON`/`RENDERER`/`CTL`.

## Order

Step 1 ∥ Step 2 ∥ Step 3, then Step 4 (needs 2 and 3). Step 5 is written in
parallel with Step 4 and its red half runs against origin/main at once. Then
Step 6, then Step 7.

## Acceptance mapping

- AC1: spec and plan committed, with mechanism evidence in the spec.
- AC2: Step 5, red on origin/main, green here.
- AC3: Step 7 runs the remote suites through `nix flake check`.
- AC4: Step 6.
- AC5: Step 7 locally, CI on the PR.

## Round 3 (dispatcher-approved scope)

Alternate-screen shrinks lose rows (`screen_resize_y` deletes from the top when
the cursor is low and a regrow adds blank rows at the bottom), so the hold is
gated to passes where no held pane shrinks and then regrows before its seed. Spec: "Where".

- `holdReshaped` returns nil (no read, no hold) for a pass with `ops.Append`, a
  pass with `ops.Swaps`, a zoomed pass (`zoomed`) and a locally zoomed window
  (`w.appliedZoom`).
- `holdReadArgs`/`parseHoldRead` gain a ninth header field, `#{window_height}`
  (after `#{pane_dead}`). The read holds nothing when the window is taller than
  `L.H`, because `FitWindowCmd`'s `resize-window` would shrink heights first.
- `force` is `len(ops.Remove) > 0`: a close-only pass holds every candidate.
  The `zoomHides` candidate rule and the zoomed-geometry-only force term are
  gone, since zoomed passes never hold.
- Tests: update the Step 2 and Step 4 Go cases (nine-field parse; no read on
  Append, Swaps, zoomed, locally zoomed; window taller than target holds
  nothing; the Zoomed and Zoomed geometry-only cases now assert no hold). New
  bats case `an unzoomed split of the top of a stacked pair takes no hold and
  keeps the bottom pane's rows in place (#1002)`: the wrapper stalls after a
  local `swap-pane`, the oracle samples `#{pane_width} #{window_panes}
  #{alternate_on}`, no sample shows `alternate_on` 1 and rows stay in place.
- Docs: `docs/agents/bridge-daemon.md` #1002 bullet carries the gating rule,
  the nine-field header, the new pin and the residual that non-holding passes
  keep the pre-hold reflow. The spec is updated to match.
- Follow-up, not in this PR: park the hold's cursor at row 1 (rows written
  bottom-up, the barrier checking `cursor_y` 0) and have the pump defer a held
  pane's `%output` until its seed, which would re-enable holds on every pass.

## Round 4 (dispatcher-approved)

- Rule wording: the hold is taken only on passes where no held pane shrinks and
  then regrows before its seed. A single net shrink looks the same on both
  screens and matches what the remote did; a shrink-then-regrow does not (the
  alternate screen has no `GRID_HISTORY`, so the shrink deletes rows from the
  top when the cursor is low and the regrow adds blanks at the bottom, while
  the main screen round-trips the rows through history). This replaces the
  "no held pane's height can drop" phrasing.
- `force` (a close-only pass holds every candidate) can hold a survivor whose
  final size matches its target: the local `kill-pane` can give the freed space
  to a different neighbour than the remote did (next-3.9 gives it to the next
  cell, stock 3.6a and 3.7c to the previous one). Pinned by
  `TestReconcileHoldForceHoldsAnUnchangedSurvivor`.
- Cross-pass release: at the top of each later pass of `reconcileSnapshot` the
  daemon releases every still-held sink and awaits `#{alternate_on}` 0
  (`awaitAlt`, bounded by `holdWait`) before reshaping. A timeout skips holds
  for that pass (origin/main behaviour). The deferred release covers a refused
  `select-layout`, a seed error and a dropped seed only at function exit, not
  across passes. Residual: a released pane whose seed put it on the remote's
  alternate screen keeps `alternate_on` 1, so that pass waits `holdWait` and
  holds nothing.
- `awaitAlt(cfg, locals, want)` replaces `awaitHeld`.
- Tests: `TestReconcileHoldForceHoldsAnUnchangedSurvivor`,
  `TestReconcileHoldReleasesBeforeALaterPass`,
  `TestReconcileHoldReleaseTimeoutSkipsHolds`, and the bats case `a hold
  carried into a later reconcile pass is released before that pass reshapes
  (#1002)`.
- Gate: `nix flake check` must pass (with `nix build .#lint` and
  `nix build .#default`) before the PR.
