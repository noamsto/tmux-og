# Bridge: reseed after a transient remote reshape (#951)

Spec (accepted by spec-critic): artifacts `spec.md`. Root cause: a remote reshape that
goes out and comes back before the mirror reads it (read-first escalation finds
ground truth == applied, or coalescing drops the leading notice) makes
every reconcile path a no-op, so nothing reseeds the panes whose `%output` was
painted at the transient size.

Design (a boolean mark on the window, no signature churn). Design it twice:
(A) thread a `transient bool` through `reconcileLayout`/`reconcileSnapshot` (12+ test
call sites change, and the coalesce path has no way to pass it); (B) a
`mirrorWindow.reshapeOwed` mark set where evidence appears and consumed at
`reconcileSnapshot` entry. B chosen.

## Steps

### 1. Mark + consume + reseed (impl: sonnet; runs AFTER step 2's red run)

Files: `picker/remotebridge/daemon/windows.go`, `reconcile.go`, `daemon.go`, new `transientreseed.go`.

1. `windows.go` `mirrorWindow`: add `reshapeOwed bool` with a doc comment: remote panes
   were resized and restored (or the notifications for the intermediate shapes were
   dropped) since the mirror last reseeded them; the next reconcile pass owes a
   reseed even when it finds nothing to apply.
2. New file `picker/remotebridge/daemon/transientreseed.go` holds the new code, so the
   `reconcile.go` hunks stay small (another open PR, #954, edits `reconcile.go` and
   its tests): `reseedOwed(w, router, rt, remoteActive, zoomed)` (the
   `reseedIDs`/`sinks`/`PaneSeeds`/`markReshaped` loop of the shaped branch, over
   `w.remotePanes`, restricted to `remoteActive` under the `zoomHides` rule),
   `coalesceLayoutBursts` and `dispatchLayoutChange` (step 5). The shaped branch is
   left untouched (a deliberate ~15-line duplication to keep the rebase trivial).
3. `reconcileLayoutFrom` checks, by name (not number): pane-set check and zoom-flag
   check set `w.reshapeOwed = true` immediately before escalating to `reconcileLayout`
   (the float check does not mark). The raw-equal check (`L.Raw == w.layout`) returns
   false only when `!w.reshapeOwed`; otherwise it calls `reconcileLayout`.
4. `reconcileSnapshot`: first statement `owed := w.reshapeOwed; w.reshapeOwed = false`.
   In the early-return block (`L.Raw == w.layout && w.appliedZoom == zoomed`), both
   sub-branches (float work, no float work): after the existing float handling and
   before `return false`, `if owed { ... }` reseed via `reseedReshapedPanes` over
   `w.remotePanes`, restricted to `[]string{remoteActive}` when `zoomed &&
   indexOf(w.remotePanes, remoteActive) >= 0` (same rule as `zoomHides`). Restructure
   the block so there is one exit (float work first, then the owed reseed).
5. `daemon.go`, burst plumbing without a shared map: `dispatch` (a closure defined in
   `Run`'s scope, before `settle(c)`; `c` is NOT in scope) becomes
   `dispatch := func(l controlmode.Line, burst bool) (done bool)`.
   - New `coalesceLayoutBursts(lines) (out []controlmode.Line, burst map[int]bool)`:
     same collapse as today; `burst[i]` (index into `out`) is true when a dropped
     same-window notice differs from the survivor in tiled shape
     (`ParseLayout(...).Raw`, floats excluded; the raw layout string if `ParseLayout`
     fails; `Args` if `parseLayoutNotice` fails) or zoom flag. The
     `coalesceLayoutChanges` wrapper is removed and its test calls
     `coalesceLayoutBursts`.
   - `asyncQueue.drain(dispatch func(controlmode.Line, bool) bool)` runs the
     coalesce, then calls `dispatch(l, burst[i])` per surviving line (a burst flag is
     only ever true for a `LayoutChange` survivor).
   - Call sites to update: `c.async.drain(dispatch)` (settle, daemon.go ~1463),
     the direct pump call `dispatch(l)` (~1549) -> `dispatch(l, false)`, and any drain
     tests (`grep -n 'drain(' *_test.go`).
   - Extract the `LayoutChange` case body of `dispatch` into
     `dispatchLayoutChange(cfg, mw, l, burst, send, router, waitHellos, cst, cv, rt) (retire bool)`:
     `if burst { mw.reshapeOwed = true }` then `reconcileLayoutFrom(...)`. `dispatch`
     calls it and `retireMirror`s on true (behaviour otherwise identical); tests call
     the same function.

Acceptance: `go build ./...` clean; existing tests in `picker/remotebridge/daemon`
that count writes/seeds still pass or are adjusted only where a read-first no-op now
correctly seeds (list each adjusted test in the PR).

### 2. Go tests FIRST, red, then step 1 (impl: sonnet)

Files: new `picker/remotebridge/daemon/transientreseed_test.go`, plus edits to
existing tests only where step 1 changes an assertion. Reuse `noticeLine`,
`scriptedRTRouterW`, `orderedLog`, `NewRouter`/sink helpers from
`reconcilenotice_test.go`, `reconcileordering_test.go`, `reshapereseed_test.go`.

- T1: notice zoom flag on, read returns unzoomed == applied (out-and-back): one
  `capture-pane` per tiled pane, FrameSeed reaches each sink, panes marked reshaped.
- T2: notice with a pane set that read shows already back to the applied set: same.
- T3: mirror zoomed (`appliedZoom` true), notice unzoomed transient, read zoomed:
  only the `remoteActive` pane is seeded.
- T4: coalesce path: `coalesceLayoutBursts` over `[B, A]` for window `@1` flags the
  survivor; then `dispatchLayoutChange(..., burst=true)` for applied state A with a
  read returning A: seeds sent. Survivor-only `[A]` (duplicate, burst=false): no
  round-trip, no capture (negative). `drain` end-to-end with a recording dispatch
  asserts the burst flag reaches the survivor and only it.
- T5 negatives: duplicate notice with no mark stays at zero writes; float-only
  notice does not seed tiled panes; burst `[B, C]` where the survivor applies a real
  shape records the expected seed count (shape reseed only, because the mark is
  consumed at entry).
- Order: write T1, T2, T3, T5 first (they use only `reconcileLayoutFrom` and existing
  helpers, so they compile on the current tree) and run them red (record output);
  T4 is red by compile failure (new functions), so also keep a behavioural variant
  of T4 that compiles today: drive `reconcileLayoutFrom` with the surviving notice
  and a read equal to applied but pre-set `w.reshapeOwed`-free, asserting the
  expected seed count of 0 now and N after. Then implement step 1 and run all green.

### 3. Bats regression + docs (impl: sonnet)

- `tests/remote-m2-integration.bats`: new case "a remote zoom out-and-back while the
  daemon is stalled leaves the mirror identical" modelled on the existing mirror
  fixtures: 3-pane remote window whose panes run a program that draws once and, on
  SIGWINCH, stamps row `h-2` (script generated in `$BATS_TEST_TMPDIR`);
  `kill -STOP "$daemon_pid"`; remote `resize-pane -Z -t <top-right pane>`, sleep, again;
  `kill -CONT`; poll until every renderer pane's `capture-pane -p` equals its remote
  pane's (RESIZE_CONVERGE_BUDGET_SECS style deadline) or fail printing both. Red run: before step 1, build the daemon from this tree (it is the
  pre-fix code) with `go build -o $SCRATCH/daemon-prefix ./remotebridge/cmd/daemon`
  from `picker/` and run the case with `DAEMON=$SCRATCH/daemon-prefix` (the bats
  `setup` honours a preset `DAEMON`); green run uses the post-fix build. Record both.
- `docs/agents/bridge-daemon.md` (Mirror invariants, next to the "Every pane is
  re-seeded after a layout reshape" bullet): new bullet stating the invariant: a
  reshape the mirror did not see end-to-end (read-first notice whose ground truth
  equals applied; coalesced burst dropping a differing leading notice) still owes a
  reseed, via `reshapeOwed`; scope (all tiled panes; the zoomed pane only when
  zoomed); the accepted costs; that the mechanism is the one reproduced (daemon
  stalled), the owner's exact trigger unknown. Also add the bats case name.

## Gates

`go test -race ./...` in `picker` (and the daemon package alone first), then the
spec's three commands: `nix build .#default`, `nix flake check`, `nix build .#lint`.
Commit from inside the devshell (direnv) so pre-commit config exists.
