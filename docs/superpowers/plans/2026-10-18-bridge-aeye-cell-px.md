# Publish the local cell size as AEYE_CELL_PX to bridged sessions (#1001)

A remote program behind the bridge that asks for the cell size (CSI 16 t) is answered by the
remote tmux, which has no pixel size for a control-mode client. aeye (noamsto/aeye #356) reads
`AEYE_CELL_PX=<w>x<h>` first. The daemon publishes the LOCAL client's cell size into the bridged
remote **session** environment the same way it publishes `OG_RELAY_GRAPHICS`.

## Design

Two shapes were weighed:

- **A. Dedicated cell field on `Viewing` + new field in `viewClientFormat`** (chosen). The size is
  resolved by the same `list-clients` query as the capability, carried on `ViewIdentity`, stored on
  `Viewing`, published by the same sites. `watchLocalClient` already re-resolves on attach/resize
  nudges, so no new plumbing.
- B. A separate `display-message` query per publish. Rejected: a second fork per tick and a second
  notion of "the client" that can disagree with the capability's.

Decisions:

- Value `"<w>x<h>"` built by `CellPx(w, h string) string`: both positive decimal integers, each
  `<= 512` (aeye's grammar), else `""`. Empty/0/garbage/oversize all collapse to `""` — never a guess.
- Several non-control clients attached: publish only when every one reports the same valid size;
  any disagreement or invalid report → `""` (unset). Order-independent, like the capability AND.
- `""` means *unset the variable*, not "set to empty" (aeye treats empty as unset anyway, but the
  task says publish nothing). `CellEnvSyncCmd(session, cell)` returns the set command or the unset
  command accordingly; the unconditional-at-startup/`repair()` sites use it so a stale value from a
  prior bridge is cleared (same reasoning as the relay's unconditional write).
- Seed: `Viewing.cell` seeded from the resolver when a client is attached, else from launcher flags
  `-cell-w/-cell-h` (`OG_BRIDGE_CELL_W/H`, sampled from the invoking client alongside
  `OG_BRIDGE_SIXEL`). Also the injection point for the bats seed/zero cases, where the default
  `bridge_up` harness has no attached local client. A nested pty-host client (the OBS pattern the
  relay test uses) does report a real cell size (measured 16x32 on next-3.9), so the shipped
  resolver path gets its own un-injected integration case.
- R3 preserved: an empty resolution (no client attached) changes nothing.
- Carousel launch (`carouselResolveScript`): verified on a scratch tmux (next-3.9) that both
  `run-shell -b -t <session>` and `split-window` children inherit `set-environment -t <session>`
  values, so `ctl.go` is **not** changed.
- Teardown: `CellEnvUnsetCmd` next to `RelayEnvUnsetCmd` (best-effort, same caveat).

## File list

- `picker/remotebridge/daemon/relayenv.go` — `CellEnvVar` + contract doc, `CellPx`, `CellEnvCmd`, `CellEnvUnsetCmd`, `CellEnvSyncCmd`.
- `picker/remotebridge/daemon/relayenv_test.go` — unit tests for the above (AC1).
- `picker/remotebridge/daemon/viewident.go` — format gains `|#{client_cell_width}|#{client_cell_height}` (6 fields); `ViewIdentity.Cell`; agreement logic; `Viewing.cell` accessors.
- `picker/remotebridge/daemon/viewident_test.go` — update 4-field rows to 6, add cell agreement/disagreement/invalid cases, `Viewing` cell round trip.
- `picker/remotebridge/daemon/daemon.go` — publish at Run startup, `repair()`, `watchLocalClient` (with undo on failed send), unset at teardown.
- `picker/remotebridge/daemon/daemon_test.go` — watcher tests: publish on change, skip unchanged, empty→unset, empty resolution preserves.
- `picker/remotebridge/cmd/daemon/main.go` — `-cell-w/-cell-h` flags; `seedView` takes the cell.
- `picker/remotebridge/cmd/daemon/main_test.go` — update `seedView` callers, add cell seed cases.
- `scripts/og-remote-open.sh` — both `display-message` sites gain the two cell fields, export `OG_BRIDGE_CELL_W/H`.
- `tests/remote-m2-integration.bats` — AC2 tests.
- `docs/agents/bridge-graphics-paste.md` — document publication.

## Steps

- [ ] **Step 1: failing unit tests for the env commands** — `relayenv_test.go`: `TestCellPx` (table: `"15","28"→"15x28"`, `"512","512"`, `"513","28"→""`, `"0","0"→""`, `"","" →""`, `"15",""`, `"-1","8"`, `"1.5","8"`, `"abc","8"`, `"015","28"→""` and `"+5","8"→""` — accept only canonical positive decimals: digits only, no leading zero, 1..512), `TestCellEnvCmd` (`set-environment -t 'mysession' AEYE_CELL_PX '15x28'`, quoted session `my'session`), `TestCellEnvUnsetCmd` (`set-environment -u -t 'mysession' AEYE_CELL_PX`), `TestCellEnvSyncCmd` (non-empty → set, `""` → unset). Run `cd picker && go test ./remotebridge/daemon -run 'Cell'` → FAIL (undefined).
- [ ] **Step 2: implement** in `relayenv.go`; same test command → PASS.
- [ ] **Step 3: failing resolver tests** in `viewident_test.go`: convert all rows to 6 fields (`...|0|0`), add cases: one client `0|xterm-kitty|sixel|1|15|28` → Cell `15x28`; two agreeing clients → `15x28`; two disagreeing → `""`; one valid + one empty → `""`; control client with sizes ignored; `0|foot||0||` → `""`; 4-field row skipped; `TestViewClientFormatDelimiter` expects 6 fields; `Viewing` cell round trip + concurrent access; the inline fixture in `TestResolveLocalViewIdentity` (`"0|xterm-kitty|bpaste,sixel|1\n"`) also becomes 6 fields. The cell is an `atomic.Pointer[string]` read via `loadTermCell`, so `&Viewing{}` (used by many tests) reads `""`. `go test ./remotebridge/daemon -run 'ViewIdent|Viewing|ViewClientFormat'` → FAIL.
- [ ] **Step 4: implement** in `viewident.go`; command above → PASS.
- [ ] **Step 5: failing watcher tests** in `daemon_test.go` using `runWatchLocalClientView`'s pattern (extend the helper with the initial cell): changed cell → exactly `[CellEnvSyncCmd("sess","15x28")]` and stored; unchanged → none; resolve `Cell:""` after stored `15x28` → `[CellEnvUnsetCmd("sess")]`; `ok=false` → none and cell kept; failed send restores the previous cell. FAIL.
- [ ] **Step 6: implement** the publish in `watchLocalClient` (compare + undo like Relay), at Run startup beside the relay write (comment: unconditional, `""` clears; one variadic `sendCtl(RelayEnvCmd(...), CellEnvSyncCmd(...))` so the pair stays one batch), in `repair()` likewise, and `CellEnvUnsetCmd` in teardown beside `RelayEnvUnsetCmd`. Then `go test ./remotebridge/...` → PASS (fix any `sent`-list assertions the new startup send shifts, only by adding the expected command).
- [ ] **Step 7: seed + flags** — `main.go` flags `-cell-w`/`-cell-h` default from `OG_BRIDGE_CELL_W/H`; `seedView(resolve, term, termfeatures, sixel, cell)`; resolved identity wins, flags are the fallback (R3 startup instant). Update `main_test.go` callers + three new cases (resolver beats flags; flags stand when unresolved; invalid flags → `""`). `go test ./remotebridge/cmd/daemon` → PASS.
- [ ] **Step 8: launcher** — `scripts/og-remote-open.sh` at both `display-message` sites append `|#{client_cell_width}|#{client_cell_height}` and extend the explicit `IFS='|' read -r term termfeatures sixel_flag cell_w cell_h`; export `OG_BRIDGE_CELL_W`/`_H`. `shellcheck scripts/og-remote-open.sh` clean.
- [ ] **Step 9: bats (AC2)** in `tests/remote-m2-integration.bats` after the relay tests; each polls with a bounded loop like its neighbours and asserts through `show-environment -t rem` (not the SRC server's global env):
  - (a) **shipped path, un-injected**: modelled on "the relay capability follows whichever client is attached" — `bridge_up 1 cellpx` with no cell flags, attach an OBS pty-host client to `host-sess`, read the expected value from the client itself (`$DST list-clients -t host-sess -F '#{client_cell_width}x#{client_cell_height}'`, no hard-coded 16x32), poll `$SRC show-environment -t rem AEYE_CELL_PX` until `AEYE_CELL_PX=<that>`, then `$SRC split-window -d -t rem "sh -c 'echo \$AEYE_CELL_PX > $f'"` and assert the pane saw the same value. The attach nudge drives `watchLocalClient`.
  - (b) **injected seed**: `bridge_up 1 cellseed --cell-w 15 --cell-h 28` (the default `bridge_up` harness has no attached local client, so the value is injected via the launcher flags) → `show-environment` = `AEYE_CELL_PX=15x28`, split pane sees `15x28`.
  - (c) **zero publishes nothing and clears a stale value**: pre-seed `$SRC set-environment -t rem AEYE_CELL_PX 9x9`, `bridge_up 1 cellzero --cell-w 0 --cell-h 0` → `show-environment -t rem AEYE_CELL_PX` reports it unset, split pane sees empty (the pre-seed gives the unset teeth).
  Run: `bats -f '<name>' tests/remote-m2-integration.bats` from the devShell (pinned next-3.9 on PATH) → PASS. `nix flake check --no-build` does not run bats; CI's `remote-m2-integration-tests` does.
- [ ] **Step 10: docs** — extend the `OG_RELAY_GRAPHICS` bullet in `docs/agents/bridge-graphics-paste.md` with the `AEYE_CELL_PX` publication, agreement rule and the carousel-inherits finding.
- [ ] **Step 11: gates** — `nix build .#lint --accept-flake-config`, `nix flake check --no-build`, affected `go test`/bats (scoped), golangci-lint via the flake check's targets as discovered.

## Acceptance

- AC1 → Steps 1–2 (`TestCellPx`, `TestCellEnv*`; empty/zero publishes nothing = `CellPx` zero cases + `CellEnvSyncCmd("")` unset + watcher empty case).
- AC2 → Step 9 bats: case (a), an attached nested client with its own measured cell size and nothing injected, is the primary evidence; (b) and (c) cover the injected seed and the zero/stale paths.
- AC3 → `nix build .#lint --accept-flake-config`.
- AC4 → `nix flake check --no-build`.
- AC5 → CI `build` jobs on the PR head; run URL in the ledger.
