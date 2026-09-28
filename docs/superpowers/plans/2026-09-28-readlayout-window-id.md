# Plan — #826: reject a readLayout reply for a window other than the target

## Task (from WORKER_TASK.md)

When a remote window dies while the bridge daemon builds its mirror, `readLayout`
(`picker/remotebridge/daemon/daemon.go:2175`) sends
`display-message -p -t 'sess':@N -F '#{window_layout} #{pane_id} #{window_zoomed_flag}'`.
`display-message`'s target is CMD_FIND_CANFAIL: from the daemon's control client a dead
session-qualified `@N` resolves to the session's current window. `setupWindow` then mirrors
@0's layout, `wireRenderer` registers a second sink for `%0` (replacing the real one), and the
dead window's `%window-close` → `closeWindow` → `Router.Unregister("%0")` cuts %0's feed.
Fix: `readLayout` also reads `#{window_id}` and errors when it is not the targeted window.
Audit the other CANFAIL-target commands sent over the control connection.

Acceptance: (AC1) regression test through the production path, red on main / green with fix;
(AC2) scratch-server check of the control-client fallback; (AC3) `nix build .#default`,
`nix flake check`, `nix build .#lint` pass; (AC4) `docs/agents/bridge-daemon.md` documents the
fallback rule.

## Scratch-server facts already measured (tmux next-3.9, control client attached to `work`, windows @0 current, @1)

| command from control client | reply |
|---|---|
| `display-message -p -t 'work':@99 -F '#{window_id} #{pane_id}'` | `@0 %0` (FALLBACK) |
| `display-message -p -t @99 -F …` (bare id) | empty |
| `display-message -p -t %99 -F …` | empty |
| `display-message -p -t 'nosuch' -F '#{session_name}'` | empty |
| `set-option -w -t 'work':@99 @probe yes` | succeeds on @0 (FALLBACK) |
| `set-option -w -t @99 …` (bare, what size.go/passthrough.go send) | `%error no such window` |
| `set-option -p -t %99 …` | `%error no such pane` |
| `if-shell -t %99 -F '#{pane_id}' A B` | runs A (condition evaluated in fallback context) |
| `show-options -pqv -t %99 opt` | empty |
| `refresh-client -C @99:50x20` | no-op, @0 unchanged |
| `capture-pane -e -p -t %99` | `%error can't find pane` |
| `set-environment -t 'nosuch' …` | `%error no such session` |
| `run-shell -t @99 'echo #{window_id}'` / `-t %99` (bare) | `@0` / `%0` (FALLBACK, added after review) |
| `if-shell -t @99 -F '#{window_id}' A B` (bare) | runs A (FALLBACK, added after review) |
| `display-message -p -t 'nosess' '#{pid}|#{start_time}|#{session_id}'` | `<pid>|<start>|` — server-scoped fields still answer |

So among the value-reading commands the fallback that retargets a live object is a **session-qualified window target**; `if-shell`/`run-shell` fall back even for a bare dead id, so their branches must name objects explicitly
(`'sess':@N`). `remoteWinTarget` (daemon.go:2155) builds exactly that form, and its only
consumer is `readLayout` (callers: daemon.go:1668 setupWindow, reconcile.go:30
reconcileLayout, reconcile.go:308 reconcileSnapshot's trailing re-read).

## Design

- New format: `#{window_id} #{window_layout} #{pane_id} #{window_zoomed_flag}` — id first, so a
  3-field legacy reply (layout first) can never parse as valid, and "no zoom flag" stays
  expressible as a 3-field reply.
- Signature: `readLayout(rt roundTrip, cfg Config, remoteID string)`; it builds the target with
  `remoteWinTarget(cfg, remoteID)` itself, so the id it checks is by construction the id it asked
  for (callers can no longer pass a target/id pair that disagree). The now-dead local `target`
  variables in reconcileLayout / reconcileSnapshot go.
- Reply validation (strict; contract: "the reply describes the window asked for"): fields < 2
  → existing "empty layout reply" style error; `fields[0] != remoteID` → error
  `daemon: layout read for <target> answered window <got>: window is gone`. No leniency for a
  missing id — the real server always emits it for a resolved window, and an empty reply (bare
  target / plain client) already errors.
- Caller dispositions (no new handling code needed; each already treats a readLayout error as
  "window unreadable" and none has touched another window's state by then):
  - `setupWindow`: returns the error before PlanWindow / setWindowPanes / spawnRenderer /
    wireRenderer; `mw.remotePanes` stays empty. `addWindow` then `reg.remove` + `cv.forget` +
    local `kill-window` — the later `%window-close` for the dead id finds nothing registered
    (closeWindow is a no-op), so `%0`'s sink is never unregistered. Rebuild callers of
    setupWindow (resetWindow et al.) already handle a readLayout `%error` — the same path
    (setupwindow_test.go:144/193/225 pin it).
  - `reconcileLayout`: logs and returns `false` — nothing applied.
  - reconcileSnapshot trailing re-read: `err != nil` → `converged = true; break` and the post-loop
    `reconcileFloats` uses the previous `L`, which was this window's own read. Nothing of
    another window is applied.

## File list

- `picker/remotebridge/daemon/daemon.go` — readLayout format, signature, id check; setupWindow call.
- `picker/remotebridge/daemon/reconcile.go` — two call sites; drop unused `target` vars.
- `picker/remotebridge/daemon/readlayout_test.go` (new) — regression tests (AC1).
- `picker/remotebridge/daemon/*_test.go` fixtures that script a readLayout reply (≈13 files:
  setupwindow, seedfailure, reconcilezoom, reconcilewindows, reconcilereseed, reconcileordering,
  reconcilenotice, reconcilelayout, reconcilefloatteardown, reconcilededup, reconcile_lostwindow,
  ctl, deadrendererheal …) — prefix each scripted layout reply with the window id the test's
  mirror targets; update `readLayoutFmt` const (reconcilenotice_test.go:357); update
  `TestReadLayoutCarriesTheZoomFlag` to the new signature/shape.
- `docs/agents/bridge-daemon.md` — a "Mirror invariants" bullet: session-qualified window
  targets fall back from a control client; readLayout verifies `#{window_id}`; the audit table
  of which targets are safe and why (AC4).

## Steps

- [ ] **Step 1: write the failing regression tests** — new `readlayout_test.go`:
  (a) `TestReadLayoutRejectsAnotherWindowsReply`: scripted rt replies `@0 <layout> %0 0` to a read
  for `@5`; assert `err != nil`. On main this test is written against the new signature, so for
  the red run it is adapted in a scratch copy (see Step 5).
  (b) `TestAddWindowGoneMidAddLeavesOtherMirrorsFeed`: through `addWindow` with a real `Router`
  where `%0` is already registered to a sentinel sink (the live mirror of @0); the scripted rt
  answers list-windows with `@5` present, then the readLayout with @0's id/layout/%0 (the
  fallback). Assert: `router` still routes `%0` to the original sink (not replaced), `reg` has no
  `@5`, no `respawn-pane` for a renderer of `%0` issued, and a subsequent
  `closeWindow(..., "@5")` leaves `%0` registered to the original sink. Reuse existing helpers
  (`scriptedRT`, `withBarriers`, fake LocalTmux/registry helpers from setupwindow_test.go /
  seedfailure_test.go — read them first and follow their pattern).
  Command: `cd picker && go test ./remotebridge/daemon/ -run 'TestReadLayoutRejects|TestAddWindowGoneMidAdd' -count=1` → fails (before Step 2).
- [ ] **Step 2: implement the id check** — daemon.go readLayout per Design; setupWindow call
  `readLayout(rt, cfg, mw.remoteID)`; reconcile.go:30 and :308 likewise; delete the `target`
  locals that become unused.
  Command: `cd picker && go build ./... && go vet ./remotebridge/daemon/` → clean.
- [ ] **Step 3: migrate fixtures** — every scripted readLayout reply gains its window id prefix
  (the id the test's mirrorWindow `remoteID` is); `readLayoutFmt` → new format; zoom-flag test to
  new signature and `@1 bd67,… %7 1` shaped replies (keep the "no flag" 3-field case: `@1 bd67,… %7`).
  Command: `cd picker && go test ./remotebridge/... -count=1` → all green, including Step 1 tests.
- [ ] **Step 4: docs** — bridge-daemon.md bullet under "## Mirror invariants" (fallback rule +
  safe-target audit summary).
- [ ] **Step 5: red/green evidence** — in a scratch copy (`git worktree add` of main into the
  scratchpad, never touching this tree), copy the Step 1 test file, adapt only the readLayout call
  to main's `(rt, target)` signature and the scripted reply to main's 3-field shape (the fallback
  reply `<@0 layout> %0 0`), run it → red with the assertion failure (sink for %0 replaced /
  Unregistered, error nil). Record command + failing assertion. Green: Step 3 command on the branch.
- [ ] **Step 6: full gate** — `nix build .#default`, `nix flake check`, `nix build .#lint` → all pass.

## Acceptance mapping

- AC1 → Step 1 tests + Step 5 red (scratch main) / green (branch) records.
- AC2 → the probe table above (re-run and pasted trimmed into PR `## Testing`).
- AC3 → Step 6.
- AC4 → Step 4.
