# Plan: mirrored float local size diverges from remote after a local client resize (#1241)

## Measured mechanism (scratch servers, real daemon via tests/float-drag-integration.bats harness)
- Remote float made with `new-pane -x 40 -y 12 -X 10 -Y 5` (no @float_geom on the remote, like the
  carousel float), pane-border-status top on both servers. Local client resized 100x32 -> 45x14 -> 150x40.
- After the grow: local float `6,1 38x11`, remote float `6,1 38x10`, permanently (re-read 2s+ later).
- Cause: `reconcileFloats` stamps `@float_geom` (outer box, absolute cells) on every local mirrored float
  (`stampFloatGeom`, reconcile.go). On every local `window-resized` the conf's hook gate sees a stamped float
  whose `@float_refit_size` != window size and forks `scripts/tmux-float-refit.sh`, which replays the stamp
  with a bare `resize-pane -x W -y H` + `move-pane`. That is a second, non-authoritative writer of a
  daemon-owned box: (1) a float whose inner top is row 1 under pane-border-status top gets +1 row from
  `resize-pane -y` (docs/agents/floats.md "resize-pane -y on a float can add a row") — the daemon's own
  writer compensates via `floatResizeCmd`, refit does not; (2) it replays a stale stamp mid-resize (timeline: local
  float briefly 58x20 in an 18-row window) and reconcile only re-moves a float when the REMOTE cell changes
  (`planFloatOps` diffs last-applied remote cell, never the local box), so a refit write landing after the
  reconcile sticks.
- Local and remote windows are already kept equal-sized (`FitWindowCmd`), and every remote reshape of a float
  arrives as `%layout-change` -> `reconcileFloats` Move (resize+move+FrameResize+reseed), so the stamp buys nothing:
  verified by experiment — daemon with the stamp removed is green on the new test and on a remote that
  itself runs a percentage tmux-float-refit (second test).
- The kitty images are `U=1` virtual placements: placeholder cells are grid text, position independent. Smearing
  follows from the grid disagreeing with the app's layout, i.e. the geometry mismatch above, not from a
  separate placement-tracking defect. No separate graphics fix is planned.

## File list
- tests/float-drag-integration.bats — add `resize_client` / `settle_agree` helpers + two tests (red on main, then green).
- picker/remotebridge/daemon/reconcile.go — delete `stampFloatGeom` and its two call sites (Add, Move); fix comments.
- picker/remotebridge/daemon/floatgeom.go — delete `floatGeomStamp` (unused after the above).
- picker/remotebridge/daemon/floatgeom_test.go — delete `TestFloatGeomStamp`.
- picker/remotebridge/daemon/reconcilefloats_test.go — replace the two stamp assertions with "no `@float_geom` write" assertions.
- docs/agents/bridge-daemon.md, docs/agents/floats.md — record measured mechanism and the rule: the daemon never stamps @float_geom on a mirrored float; local refit/nudge skip it.
- (no change to scripts/tmux-float-refit.sh, config, flake.nix: the existing integration check already runs the bats file.)

## Steps
- [ ] **Step 1: red test.** In tests/float-drag-integration.bats add `resize_client W H` (resize observer window, wait until the
  remote window reaches W x H-2) and `settle_agree` (poll up to 4s then hold 0.5s and recheck local==remote float geom), and test
  "a local client resize leaves a float on the top row the remote's size": `mirror_up -x 40 -y 12 -X 10 -Y 5`; resize 45 14; settle; resize 150 40; settle.
  Run on unfixed main: `TMUX_BIN=<nix build .#default>/bin/tmux TMUX_RAW=<pinned tmux> DAEMON=… RENDERER=… CTL=… bats tests/float-drag-integration.bats -f 'local client resize'`
  Expected: FAIL `local=6,1 38x11 remote=6,1 38x10`.
- [ ] **Step 2: guard test.** Second test "a local client resize follows a remote float the remote refits itself": remote float `-x 60% -y 70% -X 20% -Y 15%`
  plus `$SRC set-hook -g window-resized "resize-pane -t $RF -x 60% -y 70% ; move-pane -t $RF -X 20% -Y 15%"` (emulates the remote's own tmux-float-refit);
  resize through 60x20, 150x40, 45x14, 100x32 asserting settle_agree each. Expected: passes before and after (it pins that dropping the stamp does not strand a remote-refit float).
- [ ] **Step 3: unit tests first.** reconcilefloats_test.go: in the Add and Move tests assert `f.find("set-option","-p","-t","%l9","@float_geom") == nil`.
  `cd picker && go test ./remotebridge/daemon -run 'TestReconcileFloats' ` Expected: FAIL (still stamping).
- [ ] **Step 4: fix.** reconcile.go: remove `stampFloatGeom` and calls; add a comment at the Add site: the box is the remote's, written only by reconcile/ctl verbs, and no `@float_geom` so
  `tmux-float-refit`/`tmux-float-nudge` leave it alone. floatgeom.go: remove `floatGeomStamp`; floatgeom_test.go: remove `TestFloatGeomStamp`; update floatgeom.go/ctl.go/reconcile.go comments that mention the stamp.
  `cd picker && go build ./... && go vet ./remotebridge/... && go test -race ./remotebridge/...` Expected: pass.
- [ ] **Step 5: green integration.** Rebuild daemon, rerun both new tests plus the whole `tests/float-drag-integration.bats`. Expected: all pass.
- [ ] **Step 6: red proof on origin/main.** In an isolated scratch copy (git worktree of origin/main or the daemon binary built from it) run the new bats tests against the old daemon; paste the failing assertion line.
- [ ] **Step 7: docs.** bridge-daemon.md "A remote float is mirrored as a local float": replace the `@float_geom` stamp mention; add the measured mechanism (1-row case, stale replay, reconcile never diffs local box).
  floats.md: note `@float_geom` is never set on a mirrored float, so `tmux-float-refit`/`tmux-float-nudge` skip it by their unstamped rule.
- [ ] **Step 8: gates.** `nix build .#default`, `nix flake check`, `nix build .#lint` (inside devshell); then commit, review gate, push, PR.

## Evidence / consumer map (shared contract: `@float_geom` on a mirrored float)
- Producer: reconcile.go Add/Move — REMOVED.
- Readers: scripts/tmux-float-refit.sh (skips unstamped — compatible, and is the fix); config hook gate `window-resized` in config/tmux.conf.tmpl (`&&#{@float_geom}` — unstamped floats never fork refit: compatible);
  scripts/tmux-float-nudge.sh (unstamped -> no-op: behavior change only for a local-only keyboard nudge of a mirror float, which never reached the remote anyway); scripts/tmux-float-group.sh (reads stamp; mirror floats are not grouped).
- Remote leg never stamped (ctl.go comment, tests) — unchanged.
- Residual by design: floats a pre-fix daemon already stamped keep the stamp until their mirror window is rebuilt.

## Acceptance
- [ ] Regression test red on origin/main, green with fix — Steps 1,5,6.
- [ ] Graphics half: no separate fix; reasoning above (U=1 placeholders), reviewer to assess; manual check in PR: open carousel in a mirrored window, shrink/grow the terminal, footer stays unwrapped.
- [ ] `cd picker && go test -race ./remotebridge/...` — Step 4.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` — Step 8.
- [ ] CI green on PR head (run id cited in PR).
