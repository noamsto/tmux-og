# Status bar: mirrored-host list after session indicators, active-window host top right (#1164)

## Design (two approaches considered)

- A. Format-only top-right host in tmux.conf: the colour of a dedicated mirror's host is not available as a window option (only placed mirrors carry `@window_host_color`), so it cannot colour every case.
- B (chosen). Render in `picker/statusline` (Go). The renderer already fetches `@bridge_host` and `@remote_bridge_hosts` for the active window in its one per-tick `display-message`; add `#{host_short}` to that same call. No new fork. Colour comes from `hostcolor`, shared with the tabs.

## Steps

1. `scripts/tmux-reflow-windows.sh`: marker lead-in becomes a dim `⇄` (not `@icon_remote`, which reads "is on"). Comment updated. `FOLD_FMT`: move `#{?#{@session_host_marker}, …}` from after `#{session_name}` to the end of the folded line, after the agent windows.
2. `picker/statusline/main.go`:
   - add `#{host_short}` to `volatileFields`, parse into `a.localHost`; bump indexes.
   - `sessionSegment`: drop the marker after the session name. A dedicated-mirror `@bridge_win` window keeps its per-host badge next to the name (the whole session is that host's), unless the marker covers it.
   - `renderLine`: write the marker after the claude and dispatcher segments (also for mirror windows, whose `sessionSegment` returns early).
   - `renderLine`: right block becomes `host  usage  paneSlot`; the host goes first so a width change never slides the `paneSlot` (#260). The local host is dim (`thmOverlay1`); a mirror window's `@bridge_host` takes `hostHex`. Clipped to 16 runes.
3. Tests:
   - Go: update `TestSessionSegmentHostMarker`; add marker-position tests (after indicators, two hosts, none); add top-right host tests (local, placed mirror, dedicated mirror).
   - bats (`reflow-host-colour.bats`): lead-in is `⇄`; fold line carries the marker after the agent windows; two hosts each coloured.
   - `remote-m2-integration.bats` "into" marker test: new position.
4. `docs/agents/status-bar.md`: marker position and form, top-right host segment, dedicated-mirror decision. Check `scripts.md` for stale text.
5. Gate: `go test ./...` in `picker/`, `nix build .#default`, `nix flake check`, `nix build .#lint`. Before/after capture on a scratch server (own `TMUX_TMPDIR`, `CLAUDE_STATUS_DIR`, `XDG_DATA_HOME`) goes in the PR body.

## Revision 1 (plan-critic findings, verified)

- Step 2: empty `localHost` emits nothing, so fixtures without it keep their wants; the `renderLine` tests with an exact tail (`TestRenderLineFull`, `Dispatchers`, `UntrustedDir`, `BridgeWinSuppressesDir`, `BridgeHost`, `BridgeStateDisconnected`, `BridgeStateParked`, `UsageAdjacentToPaneSlot`) are re-run and updated only where a host is now set. Host values go through `slotSafe` before the right block. The `⇄` gets a why-comment.
- Step 2: the dedicated-badge gate stays `hostMarker == ""` (documented: a dedicated session that also holds placed mirrors from another host loses the name-side badge but keeps the top-right host).
- Step 3: `remote-m2-integration.bats` "into" test: marker asserted after the agent/dispatcher segment and left of `#[align=right]`, exactly once there. Right block asserted to contain `lab` for the placed window and `$($DST display-message -p '#{host_short}')` for the local window. A dedicated-mirror case is added. The folded line gets its own leading separator; `reflow-host-colour.bats` lines ~125-129, 191 and 194-204 update for `⇄` and the fold position.
- Step 4: also `docs/agents/bridge-daemon.md:655`. status-bar.md notes that the host follows window switches on the next statusline tick (same as the branch/dir segments), no new fork, and that a dedicated session shows its host twice.

## Out of scope

Per-tab host badge, bridge daemon, `tmux-crew-dash.sh`, window naming.
