# Spec — #731 bridge: aeye carousel not repainting on attach to a mirror session

## Problem (user report)

"mirrored sessions aeye carousel is not reconciling when attaching, user must
move the carousel pane so it will show / reconcile the content"

## Root cause (evidence)

1. **tmux drops a passthrough store written by a pane no client is displaying.**
   Deterministic scratch-server loop (`TMUX_TMPDIR=/tmp/og-$$ tmux -L probe`,
   `allow-passthrough all`, one real client under `script` attached to session
   `other`): a `\ePtmux;` kitty store (`a=t,i=7`) written to the pty of a pane in
   session `mirror` never appears in the client's tty stream after the client is
   `switch-client`ed onto `mirror`; a store written *after* the switch (`i=8`) does.
   3/3 runs identical. Placeholder cells (`U+10EEEE`) are grid text, so tmux
   redraws them on attach — the image they reference was simply never delivered.

2. **aeye recovers from this locally, but its recovery cannot see a mirror.**
   aeye (pinned `d64c1a6`, `gallery.go`) polls `tmuxPaneVisible()` every 1.5s —
   `display-message -t $TMUX_PANE '#{window_active} #{session_attached} …'` —
   and on the hidden→visible edge forces a re-store + full repaint (also on
   `tea.FocusMsg`). In a mirror, aeye runs on the *remote* host, where the only
   client is the daemon's control client: it is always attached, and local
   window selection is never mirrored to the remote (the daemon only mirrors
   remote→local `select-window`, `translate.go`/`reconcilewindows.go`). So a local
   client attaching to / switching onto the mirror session, or selecting the
   carousel's mirror window, produces **no** edge on the remote: aeye never
   re-stores.

3. **The daemon holds the stores but only replays them after a re-seed.** Every
   localised kitty store passes through the pane's `graphics.Proxy`, which
   retains the newest store per image id (`retain`, cap 8) and replays them
   only right after a `FrameSeed` (`outputSink.start` pump). Re-seeds happen on
   `%continue`, dropped frames, reshapes, session-pin return and reconnect —
   never on a local attach or window switch.

4. **Why moving the pane fixes it:** a local relayout resizes the remote window
   (converge) → aeye gets SIGWINCH → re-renders and re-stores while the local
   pane is now visible, and the reshape re-seed replays the retained stores.

## Goal

When a local, non-control client starts displaying a mirror window (attach,
switch-client onto the mirror session, select-window within it), every pane of
that window that has retained kitty stores receives them again **and** its
placeholder cells are repainted after them — without any remote-side
cooperation (the fix must work for any kitty-placeholder app, not just aeye).

## Design

### D1. Reveal detection — a local watcher (new file `daemon/reveal.go`)

- One local query per nudge: `list-clients -t <LocalSess> -F
  '#{client_control_mode}|#{client_name}|#{client_created}|#{window_id}'`.
  Control-mode clients are skipped (they render nothing). The rest yield a set
  of *views* `(client_name, client_created, window_id)`. `client_created` is in
  the key so a detach + re-attach from the same tty that both land inside one
  poll interval still reads as a new view.
- A view present now but absent from the previous observation is a **reveal**
  of its `window_id`. The previous set starts empty (so the first observation
  reveals everything visible — harmless: at startup no sink has retained
  stores yet).
- A revealed local window maps to its mirror via the registry
  (`mirrorWindow.localWin`); every pane of it (`allRemotePanes()`, floats
  included) whose sink **currently holds retained stores** is marked
  `revealed`. Panes with no retained graphics are never marked, so an ordinary
  window switch costs nothing beyond the one local fork.
- After marking at least one pane, the watcher sends one output-less,
  side-effect-free remote command (`has-session -t <remote session>`) through
  the existing mutex-guarded `send` so its `%begin/%end` wakes the main loop —
  the same fire-and-forget pattern `watchLocalClient` uses. A failed send
  (stream closed) leaves the marks in place; reconnect's own full re-seed
  already replays, and the next pass consumes the mark with one redundant
  re-seed.
- Driven by the **same** nudge file and poll tick as `watchLocalClient` (its own
  `lastNudge`, own goroutine, started beside it, stopped by the same
  `stopWatch`). No 30s fallback is needed: a missed nudge only delays until the
  next one, and there is nothing to converge.
- `session-window-changed` is added to `resizeHookEvents` so a window switch in
  the mirror session touches the nudge. (`client-session-changed` already
  covers attach and switch-client — measured redundant with `client-attached`,
  per the existing doc.) Side effect: `watchLocalClient` also runs its `area()`
  fork on a window switch; its `cv.need`/Relay dedupe keep sends at zero.

### D2. Consuming the mark — main loop

- `reseedRevealed(router, rt)` runs beside `reseedDropped`/`reseedReshaped`
  (the only place a round-trip may run). It collects sinks whose
  `takeRevealed()` is true — gated like `takeDirty` (skips closed/paused
  sinks; a paused pane's `%continue` re-seed already replays) — and
  re-seeds them through `PaneSeeds` + `enqueueSeedWithReplay`.

### D3. Replay **before** the seed (ordering change, all re-seeds)

- The pump currently writes `FrameSeed` then the replay. A kitty placeholder
  resolves its image when the cell is *painted*; a store that lands after the
  paint stays invisible until something repaints (aeye's own measured note,
  `gallery.go repaintCmd`). For a terminal that lacks the image (exactly the
  reveal case), seed→replay therefore leaves the boxes blank. The pump will
  write the retained stores **first**, then the seed (whose `\e[2J` + capture
  repaints every placeholder cell to the now-visible client).
- Kitty stores are position-independent, and the outer terminal is in tmux's
  alternate screen regardless of the pane's own `?1049h`, so moving the store
  ahead of the seed changes nothing for the existing replay callers
  (`%continue`, drop, reshape, session-pin, reconnect) except making them also
  correct when the terminal lacks the image.

### D4. Sink state

- `outputSink` gains `revealed bool` (under `mu`) with `markRevealed()` /
  `takeRevealed()`, and `hasImages atomic.Bool`, stored by the pump after each
  `gfx.Filter` (and cleared on bulk delete / evict to empty) from a new
  pump-confined `Proxy.Retained() bool`. The watcher reads only the atomic, so
  `retain` stays pump-confined.

### D5. Retain cap

- `retainMaxIDs` rises from 8 to 32. aeye's carousel uses one id for the
  preview plus one per visible filmstrip thumbnail (`stripCols =
  (paneW+gutter)/(stripThumbW+2+gutter)`, thumbnail 18 cells) — a ≥~170-col
  carousel already needs >8 ids, and LRU eviction drops the *oldest* store,
  which is the preview. Each retained entry is a localised `t=f` store (a local
  path, a few hundred bytes), so 32 is negligible memory.

## Out of scope

- The reconnect loop / backoff / parked state (#729's territory in
  `daemon.go`). daemon.go edits are limited to: the `outputSink` fields, the
  pump's replay order, `resizeHookEvents`, starting the new goroutine next to
  `watchLocalClient`, and one `reseedRevealed` call beside `reseedReshaped`.
- Sixel: still not replayed (position-dependent, documented).
- Local zoom toggles: already mirrored to the remote as a layout change →
  reshape re-seed → replay (now replay-first).
- A retained store whose local cache file was pruned stays blank (same as
  today's replay).

## Acceptance

- Root cause above stated in the PR.
- Regression tests (Go, `picker/remotebridge/daemon`), each failing before the fix:
  1. Reveal detection: feeding the watcher a `list-clients` sequence (none →
     one real client on window W) marks exactly W's panes that hold retained
     stores; a control-mode client, an unchanged view set, and panes with no
     retained stores mark nothing; a same-tty re-attach (new
     `client_created`) marks again.
  2. Main-loop consumption: a revealed sink holding a retained store gets
     `FrameOutput(replayed store)` **then** `FrameSeed(fresh capture)` on its
     renderer conn.
  3. Existing `TestPauseContinueReplaysRetainedKittyStoreAfterSeed` updated to
     the replay-first order.
- `docs/agents/bridge-graphics-paste.md` documents the reveal re-seed and the
  new order; `resizeHookEvents` doc updated.
- Gate: `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Spec-critic notes folded in

- **Residual (documented, not closed):** a B→A→B window/session flip inside one
  poll interval, or two nudges in one coarse-mtime bucket, produces no view-set
  edge, so stores dropped during the excursion stay blank until the next reveal
  or re-seed. Stated in the PR and the doc.
- **Relay-only mode** (`--test-local` / empty ctl socket) retains nothing, so
  the reveal re-seed is a no-op there; manual verification must use an ssh bridge.
- **Lifetime:** the reveal watcher starts once per `Run`, beside
  `watchLocalClient`, so its view set survives reconnects; its first observation
  seeds the set (at startup no sink holds stores, so nothing is lost).
- **Doc debts from D3:** `enqueueSeedWithReplay` doc, `graphics.Proxy` type doc
  ("Replay immediately after each FrameSeed") and `Replay` doc, the pump's
  ordering comments.

## Review amendment

Code review found that a session-scoped `session-window-changed` hook, indexed
or not, shadows every global `session-window-changed` hook (reflow, `mark-seen`,
the carousel's `--reconcile`) inside the mirror session (measured on a scratch
server, same trap as `pane-died` in #647). `session-window-changed` is therefore
**not** added to `resizeHookEvents`, and `watchReveal` is not nudge-gated: it
runs one local `list-clients` every poll tick (1s). That also sees a client
leaving the session and returning to the same window as a reveal.
