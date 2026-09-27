# Plan — bridge: remote respawn-pane/respawn-window verbs with re-seed (#784)

## Context / mechanism

A mirror window's menus currently hide the window menu's **Respawn** and offer
the pane menu's Respawn as a local **Reconnect** (`respawn-pane -k`, which
redials the renderer, #547). There is no menu path to restart the *remote*
program. This change adds two ctl verbs and routes the menus' Respawn items to
them on a mirror, keeping Reconnect as the local redial.

Faithfulness requires a re-seed: `respawn-pane -k` on the remote clears the
remote screen, but control mode carries no clear, so the mirror keeps stale
bytes until the new program repaints. A `wantReseed` intent, drained in
`settle()` beside `wantLayout`, asks the daemon to `capture-pane` the affected
window's panes and push a `render.Seed` (which begins `\x1b[2J\x1b[H`), reusing
`PaneSeeds` so the seed goes through the same path PR #802 extends with mouse
modes. `respawn-window -k` also destroys every remote pane but the first
(`spawn.c:129-147`); the layout reconcile handles the local pane-set removal,
and the re-seed repaints the survivor.

The re-seed is keyed by **remote window id**: `respawn-window`'s surviving pane
is the window's *first* pane, not necessarily the `@bridge_pane` the bind
carries, so the pane id is not known at command time. Re-seeding the window's
mirrored panes is correct for both verbs (one screen per pane) and needs no
guess.

## File list

- `picker/remotebridge/daemon/ctl.go` — `reseed` field on `verb`; `respawn-pane` and `respawn-window` entries; `wantReseed` map on `ctlState`; `ctlRequest.wantReseed`; register in `submit`, drain in `takeIntents`, drop in `forgetWindow`.
- `picker/remotebridge/wire/protocol.go` — `CtlProtocolVersion` `"6"` → `"7"` (verb table changed).
- `picker/remotebridge/daemon/daemon.go` — `settle()` drains the reseed intent after the layout loop; new `reseedWindow(reg, router, rt, remoteWin, reason)` helper beside `reseedDropped`.
- `picker/remotebridge/daemon/ctl_test.go` — verb table cases, intent coalesce/forget tests, no-side-effect test updated.
- `generator/render/menus.go` — window menu gains `Respawn R { ctl respawn-window }`; pane menu gains `Respawn R { ctl respawn-pane }` and keeps a local `Reconnect e { respawn-pane -k }` (stock key restored to Respawn; Reconnect moves off `R` because a menu cannot carry two items on one key).
- `config/tmux.conf.reference.nix` — `menuMirrors`/`mirrorWindowMenu`/`mirrorPaneMenu` updated byte-for-byte to match `menus.go` (the extraction check diffs `og generate` against it).
- `generator/render/menus_test.go` — add `respawn-pane`/`respawn-window` to the ctl-verb set; assert both items present on a mirror and the local `respawn-pane -k` still present (Reconnect).
- `tests/menu-bind-integration.bats` — mirror window menu shows `Respawn`; mirror pane menu shows `Respawn` and `Reconnect`; plain-window parity unchanged.
- `tests/remote-m2-integration.bats` — e2e: `ctl respawn-pane` restarts the REMOTE pane and the mirror drops the stale marker; `ctl respawn-window` collapses the remote window and re-seeds.
- `docs/agents/bridge-daemon.md` — classification tables (window + pane Respawn rows) and the #547 "Reconnect" sentence.
- `docs/superpowers/plans/2026-09-27-784-bridge-remote-respawn.md` — this plan, committed alongside the code per repo convention.

## Steps

- [ ] **Step 1: failing ctl verb-table tests.** In `ctl_test.go`, extend the table case struct with a `reseed string` field plus assertion, and add:
  - `respawn-pane %3` → cmds `["respawn-pane -k -t %3"]`, `reseed "@1"`, `layout ""`, `windows false`, and no focus invalidation (the table struct already has no `invalidate` field; assert `invalidate == ""` only if the existing tests expose it — otherwise leave it unasserted).
  - `respawn-window %3` → cmds `["respawn-window -k -t @1"]`, `reseed "@1"`, `layout "@1"`, `windows false`, `invalidate "@1"` (the table already asserts the other verbs' invalidation via dedicated tests; add a small `TestParseCtlRespawnInvalidatesActiveBelief` for `respawn-window` only, since `respawn-pane` must not invalidate).
  Extend `TestTakeIntentsCoalescesAndDrains` to submit each verb and assert the reseed set drains (and coalesces for the same window); extend `TestForgetWindowDropsState` to assert no reseed survives `forgetWindow`. Extend the no-side-effect check in `TestHandleCtlNacksAndRaisesOnAStaleViewer` (ctl_test.go:1404, **not** the ping test) to include `wantReseed`. Command: `go test ./picker/remotebridge/daemon/ -run 'TestParseCtl|TestTakeIntents|TestForgetWindow|TestHandleCtlNacks' -count=1` → currently FAIL (unknown verb / field missing).

- [ ] **Step 2: implement the ctl side, the drain and the helper (one compilable step).** In `ctl.go`: add `reseed bool` to `verb`; add the two `verb` entries — `"respawn-pane": {reseed: true, build: …["respawn-pane -k -t %s"]}` and `"respawn-window": {windows: false, layout: true, moves: true, reseed: true, build: …["respawn-window -k -t %s"]}`; add `wantReseed map[string]bool` to `ctlState` (init in `newCtlState`); add `wantReseed string` to `ctlRequest`; set `req.wantReseed = win` when `v.reseed`; register `c.wantReseed[req.wantReseed] = true` in `submit` inside the mutex; delete it in `forgetWindow`; extend `takeIntents` to `(windows bool, layouts, reseeds []string)`. **Also update its sole production caller in the same step** so the package compiles and `reseeds` is used: `daemon.go:1104` `wantWindows, layouts, reseeds := cst.takeIntents()`, and after the `layouts` loop in `settle()` call `reseedWindow(reg, router, rt, remoteWin, "after respawn")` for each reseed window (skipping a window the reconcile retired). Add `reseedWindow` beside `reseedDropped`, modeled on `reseedPanes`/`reseedDropped`: `reg.byRemoteID(remoteWin)` → `mw.allRemotePanes()` → sinks → `PaneSeeds` → `enqueueSeedWithReplay`, logging per-pane errors. Run Step 1's command → PASS.

- [ ] **Step 3: verify the daemon package.** Command: `go test ./picker/remotebridge/daemon/ -count=1` → PASS. Also `go build ./...`.

- [ ] **Step 4: bump the protocol version.** `wire/protocol.go` `CtlProtocolVersion = "7"`. Command: `go test ./picker/remotebridge/... -count=1` → PASS (tests read the constant; `flake.nix` derives the wire version from it).

- [ ] **Step 5: failing menu tests.** In `generator/render/menus_test.go`, add `"respawn-pane": true, "respawn-window": true` to `TestMirrorCtlItemsUseTheKeybindEntryPoint`'s `verbs` map; add a case asserting the `prefix >` mirror branch contains `Respawn R` routed through `ctlRun(p, "respawn-pane", "")` and a local `Reconnect` still containing `respawn-pane -k`, and the `prefix <` mirror branch contains `Respawn R` routed through ctlRun `respawn-window`. Command: `go test ./generator/... -run 'TestMirror|TestSession' -count=1` → currently FAIL.

- [ ] **Step 6: implement the menu routing.** In `menus.go`, update `mirrorWindowMenu` (add `Respawn R { esc1(ctlRun(p,"respawn-window","")) }` before/after Kill) and `mirrorPaneMenu` (add the remote `Respawn R` ctl item; change the local item to `Reconnect e { respawn-pane -k }`). Keep every kept item's stock visibility condition. Run Step 5's command → PASS.

- [ ] **Step 7: mirror the menu change into the Nix reference.** In `config/tmux.conf.reference.nix`, update `mirrorWindowMenu` and `mirrorPaneMenu` identically. Verify with the extraction check: `nix build .#checks.$(nix eval --raw --impure --expr builtins.currentSystem).tmux-conf-extraction-assertions` (or `nix flake check`), or the local `og generate` diff if the check's invocation is impractical — record the exact command actually run. → PASS.

- [ ] **Step 8: failing e2e tests.** In `tests/remote-m2-integration.bats`, add beside the existing respawn test:
  - `ctl respawn-pane restarts the REMOTE pane and the mirror drops the stale screen`: bridge one pane; send `printf 'OLD_<tag>\n'`, assert `mirror_contains 1 OLD_<tag>`; then `$CTL --sock "$sock" respawn-pane "$(remote_pane_of 0)"`; send `printf 'NEW_<tag>\n'` on the SRC; poll until `mirror_contains 1 NEW_<tag>` and assert the mirror no longer shows `OLD_<tag>` (the re-seed's `\x1b[2J` clears it).
  - `ctl respawn-window respawns the REMOTE window and re-seeds the mirror`: bridge a 2-pane remote window (split-h) with a marker in each; `$CTL --sock "$sock" respawn-window "$pane"`; assert the remote window has 1 pane and the mirror window has 1 renderer; send a new marker; assert it appears and the old markers are gone.
  Command: `nix build .#checks.$(nix eval --raw --impure --expr builtins.currentSystem).remote-m2-integration-tests` (the real e2e harness) → currently FAIL on `unknown verb respawn-pane`/`respawn-window`.

- [ ] **Step 9: run the e2e.** After Steps 2–4 and 6, run Step 8's command → PASS.

- [ ] **Step 10: update the bats menu-content test.** In `tests/menu-bind-integration.bats`'s "hidden and relabelled items" test, invert the window-menu `screen_lacks 'Respawn'` into a check that it is present, and for the pane menu assert both `Respawn` and `Reconnect` are present. Run `nix build .#checks.$(...).menu-bind-integration-tests` → PASS.

- [ ] **Step 11: docs.** Update `docs/agents/bridge-daemon.md`: window-menu Respawn row → ctl `respawn-window`; pane-menu Respawn row → ctl `respawn-pane`, with a new row/note for `Reconnect e` as the local redial; update the #547 "A renderer's exit must not be structural" sentence that says the window menu no longer offers Respawn; and soften the #769 paragraph's "no new verb was needed" (it is scoped to #769; #784 adds `respawn-pane`/`respawn-window`). Command: `nix build .#lint` (typos/markdown) → PASS.

- [ ] **Step 12: full gate.** `nix build .#default`, `nix flake check`, `nix build .#lint` all PASS.

## Acceptance mapping

- Go tests: both verbs build the right remote command and register the re-seed intent; `respawn-window` registers the layout reconcile too → Step 1–2 tests.
- e2e like #796's `remote-m2-integration.bats` → Step 8–9.
- Menus route Respawn to the new verbs; Reconnect stays the local redial; classification table updated → Step 5–7, 10, 11.
- `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 12.

## Risks / notes

- **Key conflict.** The pane menu cannot carry two `R` items. Chosen: stock `Respawn R` (remote) + `Reconnect e` (local redial). This is the one user-visible judgement call; it is documented under the PR's `## Assumptions` and in the classification table.
- **Re-seed timing (ordering argument, not a plan change).** `submit` registers the intent and calls `send` while holding `c.mu`; `takeIntents` takes the same mutex, so no drain can observe the intent before the respawn command has been flushed to the control stream. The reseed's `PaneSeeds` capture is written after that flush on the same ordered control client, so tmux executes it after the respawn. A reply-ordinal barrier or output re-arm is therefore unnecessary; the capture reflects the post-respawn screen even if the loop drains the intent promptly. (Refuted against `submit`/`stampAll`/`takeIntents` during plan review.)
- **`respawn-window` survivor.** The surviving pane is `TAILQ_FIRST`, so re-seeding is window-scoped, not pane-scoped.
- **No bespoke capture.** Re-seed uses `PaneSeeds`, satisfying the #802 mouse-mode requirement.

