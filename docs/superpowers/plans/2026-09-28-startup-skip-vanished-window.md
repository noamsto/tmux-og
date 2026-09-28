# Startup skips a remote window that vanishes during setup (#837)

## Problem

Since #836, `readLayout` rejects a reply for a window other than the one it
asked about (a session-qualified dead `@N` falls back to the current window).
The rejection is a plain `fmt.Errorf`. `runMirror`'s startup loop
(`picker/remotebridge/daemon/daemon.go`, the `for i, rw := range remoteWins`
loop) treats every `setupWindow` error as fatal: `teardown()` and
`return tornDown{err}`. So one short-lived remote window that closes between the
startup `list-windows` and its `readLayout` fails the whole bridge open, and on
a reopen after a server replacement it throws away the whole rebuild.

**Violated invariant:** a remote window that no longer exists is not a reason to
abandon mirroring the windows that do. Startup must skip the vanished window and
mirror the rest, leaving no local window, registry entry or converger record
for it.

## Design

- `var errWindowGone = errors.New("window is gone")` in `daemon.go`, beside
  `readLayout`. `readLayout`'s window-id rejection wraps it with `%w`; the
  message text stays the same (`... answered window @X: window is gone`). This
  is the only #836 window-gone path: the `Error`-kind and empty-reply branches
  mean something else (a missing session, a lost round-trip).
- Extract the startup loop into `mirrorStartupWindows(cfg, remoteWins, send,
  router, waitHellos, cst, reg, cv, rt) (placeholder string, err error)`, called
  from `runMirror` with the same arguments the loop uses today. Behaviour for
  every error other than `errWindowGone` is unchanged: `runMirror` does
  `teardown()` and `return tornDown{err}`.
- **Placeholder rule.** The launcher's initial window (`firstMirrorWindow`) is
  read once before the loop. Each remote window takes the placeholder while it
  is still unclaimed, else a fresh `createMirrorWindow`. On `errWindowGone`:
  log to stderr, `reg.remove(rw.id)`, `cv.forget(rw.id)`,
  `cst.forgetWindow(rw.id)` (same drop sequence as `mirrorNewWindow`, though
  not strictly needed), and `kill-window`
  the local window **only if it is not the placeholder**; then `continue`. The
  placeholder therefore survives a vanished first window and is re-stamped and
  reused by the next remote window. A successful setup clears the placeholder.
  `readLayout` runs before `setupWindow` changes anything local (PlanWindow,
  respawn, `cst.setWindowPanes` all come after it), so there is nothing else to
  undo.
- `mirrorStartupWindows` returns the placeholder when no remote window claimed
  it (every window vanished).
- **All windows vanished.** `runMirror` carries on to the existing
  `reconcileWindows` + `if reg.empty() { teardown(); return nil }`:
  - The remote has no windows left → reconcile mirrors nothing (its early
    return on an empty or failed `list-windows` is harmless, reg is already
    empty) → the existing empty-registry teardown.
  - Reconcile finds windows that appeared since → it mirrors them through
    `mirrorNewWindow` (fresh local windows), and `runMirror` then kills the
    unclaimed placeholder, which is no longer the session's last window, and
    reflows (reconcile's deferred reflow already ran). This kill runs only when
    `reg` is non-empty, so it can never empty the session.

## Consumer map for the wrapped error

| consumer | disposition | reference |
| --- | --- | --- |
| `setupWindow` → startup loop | **changed**: skip on `errWindowGone` | new tests |
| `setupWindow` → `addWindow` | compatible: any error drops the half-built mirror | `daemon.go` addWindow |
| `setupWindow` → `mirrorNewWindow` (reconcileWindows, healLostWindows) | compatible: any error drops the half-built mirror | `reconcilewindows.go` |
| `setupWindow` → `resetWindow` | compatible: generic error path, unchanged | `reconcile.go` resetWindow |
| `readLayout` → `reconcileLayout` | compatible: logs, applies nothing | `reconcile.go:28` |
| `readLayout` → `reconcileSnapshot` re-read | compatible: `err != nil` ⇒ converged | `reconcile.go:304` |
| `readLayout` tests | compatible: `readlayout_test.go` checks `err == nil`/non-nil only | existing tests |

## File list

- `picker/remotebridge/daemon/daemon.go` — `errWindowGone`, `readLayout` wrap,
  extract `mirrorStartupWindows` (all skip/placeholder logic), and one
  `dropUnclaimedPlaceholder` call in `runMirror` right after the
  `reg.empty()` teardown check.
- `picker/remotebridge/daemon/startupwindows_test.go` — new regression tests.
- `picker/remotebridge/daemon/readlayout_test.go` — assert the wrong-window
  rejection `errors.Is(err, errWindowGone)`.
- `docs/agents/bridge-daemon.md` — replace the exact line "A startup
  `setupWindow` failure fails the open…" (~:354) with the skip/placeholder
  behaviour, noting an unclaimed placeholder keeps the last vanished window's
  stamp until it is killed or the session torn down.
- `docs/superpowers/plans/2026-09-28-startup-skip-vanished-window.md` — this plan.

## Steps

- [ ] **Step 1: extract the loop, no behaviour change** — in `daemon.go` move
  the startup loop into `mirrorStartupWindows` (placeholder read once up front,
  but still fatal on every error), call it from `runMirror`. Declare
  `var errWindowGone` here too (the `readLayout` wrap waits for Step 3), so the
  Step 2 tests compile against it, and a no-op
  `dropUnclaimedPlaceholder(cfg Config, reg *registry, placeholder string)`
  stub. Proves:
  `cd picker/remotebridge && go test ./daemon/...` green.
- [ ] **Step 2: failing tests** — `startupwindows_test.go`, driving
  `mirrorStartupWindows` with the real `setupWindow`, `setupWindowRT(script)`,
  `noHellos` (nil map, nil error ⇒ no seed round-trips), `LocalArea` 80x24,
  a `LocalTmuxOut` fake answering `list-windows` → `@100`, `new-window` →
  `@101`, `@102`…, `list-panes` → `%lN 0`, and a recording `LocalTmux`. Each
  window's script is a ConvergeCmd block then a readLayout block
  (`@N <1-pane layout> %N 0`; the vanished one answers a different `@M`).
  Cases:
  1. `TestStartupSkipsAWindowThatVanishes` — `@1 @2 @3`, `@2` answers `@1`:
     err nil; reg holds `@1`→`@100`, `@3`→`@102`, no `@2`; `cv.last` has no
     `@2`; `kill-window -t @101` issued; placeholder returned `""`.
  2. `TestStartupVanishedFirstWindowKeepsThePlaceholder` — `@1 @2`, `@1`
     answers `@2`: reg holds only `@2`→`@100`; no `kill-window`; no
     `new-window`; placeholder `""`.
  3. `TestStartupEveryWindowVanished` — `@1 @2`, both answer `@9`: err nil;
     reg empty; `cv.last` has neither `@1` nor `@2`; no `new-window` (`@2`
     re-takes the unclaimed placeholder); no `kill-window` at all; placeholder
     `"@100"`.
  4. `TestStartupOtherSetupErrorsStayFatal` — `@1 @2`, script ends after `@1`'s
     converge (connection closed): err non-nil and not `errWindowGone`.
  5. `TestStartupDropsUnclaimedPlaceholder` — the post-reconcile helper
     `dropUnclaimedPlaceholder(cfg, reg, placeholder)` issues
     `kill-window -t @100` only when the placeholder is non-empty **and** reg
     is non-empty, then calls `cfg.reflow()`; nothing for `""` or an empty reg.
  `readlayout_test.go`: the wrong-window case asserts
  `errors.Is(err, errWindowGone)`.
  Proves: `go test ./daemon/ -run 'TestStartup|TestReadLayout'` compiles;
  cases 1–3, case 5 and the readLayout `errors.Is` assertion fail on
  assertions (red), case 4 passes.
- [ ] **Step 3: implement** — `errWindowGone` + `%w` wrap in `readLayout`;
  skip/placeholder handling in `mirrorStartupWindows`; add
  `dropUnclaimedPlaceholder` and call it in `runMirror` after `reconcileWindows`
  and the `reg.empty()` teardown check.
  Proves: the Step 2 command green.
- [ ] **Step 4: counterfactual red** — in a scratch copy of the tree, make
  `mirrorStartupWindows` fatal on every error again (the `main` behaviour) and
  run the Step 2 command; record the assertion failures.
- [ ] **Step 5: docs** — update the #826 bullet in `docs/agents/bridge-daemon.md`.
- [ ] **Step 6: gate** — `go vet ./...` and `go test ./...` in
  `picker/remotebridge`; then `nix build .#default`, `nix flake check`,
  `nix build .#lint`.

## Acceptance

- [ ] Startup-path test, one of several windows vanishes → Step 2 case 1,
  red via Step 4, green after Step 3.
- [ ] First-window and all-windows-vanished cases → Step 2 cases 2 and 3.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 6.
- [ ] `docs/agents/bridge-daemon.md` updated → Step 5.
