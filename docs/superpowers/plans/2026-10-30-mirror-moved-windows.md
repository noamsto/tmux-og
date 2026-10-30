# Mirror picks up windows moved into the mirrored session (#1025)

## Evidence (scratch server, tmux next-3.9, control client attached to `toddl`)

`tmux move-window -d -s @2 -t toddl:` from session `disp` yields, in order:

```
%window-add @2
%window-close @2      <- spurious: @2 is still in toddl
```

`window-unlinked` fires for the source session; `control_notify_window_unlinked`
sends `%window-close` (not `%unlinked-window-close`) because the attached
client's session now contains the window. `dispatch` handles `%window-add` with
`addWindow` (mirror created), then `%window-close` with `closeWindow`
(mirror torn down) — so a moved/linked window is mirrored and immediately lost.
Same for `link-window` followed by an unlink from the source.

Root cause: `%window-close` is treated as proof the window left the mirrored
session. It is not; only the remote's window list is.

## Fix

New seam in `reconcilewindows.go`: `closeWindowIfGone(cfg, router, cst, reg, cv, rt, remoteID) bool`
(returns whether it tore the mirror down). For a registered id it runs
`list-windows` (`cfg.filter.listCmd`/`parse`, as `addWindow` does). Window still
listed with `selYes` → ignore the notification, return false. Anything else
(absent, empty body, listed `selNo`/`selUnknown`, list error) → `closeWindow`,
return true (prior behaviour; `selUnknown` is no evidence for *keeping* either).
`dispatch`'s `WindowClose` case calls it with `flowCfg` and returns
`reg.empty()` only when it closed.

## Files

- `picker/remotebridge/daemon/reconcilewindows.go` - `closeWindowIfGone` (`closeWindow`/`addWindow` stay in daemon.go).
- `picker/remotebridge/daemon/daemon.go` - `dispatch` WindowClose case calls it.
- `picker/remotebridge/daemon/windowmove_test.go` - new tests calling `closeWindowIfGone` directly.
- `docs/agents/bridge-daemon.md` - spurious `%window-close` on move/link; amend the "Filter sites" bullet (`%window-close` now confirms), the #934 close-paths note and the `flowCfg` scope list.

## Steps

- [ ] **Step 1: seam stub + failing test.** Add `closeWindowIfGone` as a bare `closeWindow` wrapper (current behaviour), and `windowmove_test.go` with: (a) registry holds `@2`, scripted `rt` lists `@2` selected: expect `@2` still registered, no `kill-window` in `cfg.LocalTmux` calls; (b) list omits `@2`: torn down; (c) list errors: torn down. `cd picker && go test ./remotebridge/daemon -run CloseWindowIfGone` -> (a) fails by assertion.
- [ ] **Step 1b: dispatch-level test** `TestWindowMoveDispatch...` driving the real `runMirror` loop (see existing `runMirror`-level tests in daemon_test.go/conn_test.go) with scripted `%window-add @2` then `%window-close @2` and `rt` still listing `@2`: mirror survives; reverting the dispatch call to `closeWindow` must turn it red. If runMirror cannot be driven in a unit test, cover it with a `tests/remote-m2-integration.bats` case using `move-window -d -s <id> -t <sess>:` on the live remote.
- [ ] **Step 2: implement** the confirm in the seam; wire `dispatch`. Same command -> passes.
- [ ] **Step 3: existing suites** `cd picker && go test -race ./...`. `TestLoopStopsOnWindowClose` no longer exists; real-close regression is `tests/remote-m2-integration.bats` (live window kill cases, ~lines 341/388), which must stay green via `nix flake check`; includes the last-window-dies case with `detach-on-destroy off` (#934 cases, ~line 7474), where the extra `list-windows` errors once the session is gone and falls through to `closeWindow`.
- [ ] **Step 4: move-out verification.** On a scratch server (`TMUX_TMPDIR=/tmp/og-$$ ... tmux -L probe`), attach a `-C` client to `toddl`, `move-window` a toddl window to another session, record the notification. If it is `%unlinked-window-close` (parses `Other`; `controlmode/parse_test.go:47/:73` pin that), the mirror is stale: handle it by a daemon-side `reconcileWindows` trigger rather than reparsing, add test `TestUnlinkedWindowCloseReconciles`. If the mirror already converges, state so in the PR.
- [ ] **Step 5: docs** as listed.
- [ ] **Step 6: gates** `nix build .#lint`, `nix flake check`, `nix build .#default` in the devshell.

## Acceptance

- [ ] Go test fails on main, passes after: `go test ./remotebridge/daemon -run 'CloseWindowIfGone|WindowMoveDispatch'` (same pattern in Steps 1-2; red run = assertion failure, never "no tests to run").
- [ ] Existing bridge suites: `cd picker && go test -race ./...`.
- [ ] `nix build .#lint`, `nix flake check`, `nix build .#default`.
- [ ] CI green, run id cited in PR.
