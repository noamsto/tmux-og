# Plan — #731 reveal re-seed for mirror panes holding kitty stores

Spec: `spec.md` beside this file (accepted). One refinement over the spec's D1/D4,
made while planning for a data race the spec missed: `mirrorWindow.remotePanes`
is mutated by reconcile on the main loop and read there without a lock, so the
watcher goroutine must **not** walk a window's panes. The watcher therefore
queues revealed **local window ids**; the main loop (which owns `mirrorWindow`)
maps them to panes. The per-sink `revealed` flag of D4 is dropped — only the
pump-written `hasImages atomic.Bool` remains on the sink.

All code in `picker/remotebridge/`. Go tests: `go test ./picker/remotebridge/...`
from the repo root is not how the repo is laid out — run `cd picker && go test
./remotebridge/...` (module root is `picker/`; confirm with `picker/go.mod`).

## Step 1 — graphics: retain cap + `Retained()` (implement: sonnet)

Files: `picker/remotebridge/graphics/proxy.go`, `proxy_test.go`.

1. `retainMaxIDs` 8 → 32; extend its doc with the reason (aeye carousel: preview
   + one id per visible filmstrip thumbnail; LRU would evict the preview first).
2. Add `func (p *Proxy) Retained() bool { return len(p.order) > 0 }` — pump-confined
   like `Replay`, doc says so.
3. Docs: the `Proxy` type doc ("Replay immediately after each FrameSeed") and the
   `Replay` doc become "Replay immediately *before* each FrameSeed" with the
   one-line reason (a placeholder resolves when painted, so the store must land
   ahead of the seed's repaint).
4. Test `TestProxyRetainedTracksStores`: new Proxy → false; after a localised
   `a=T,i=5,t=f` store → true; after `a=d,d=I,i=5` → false; after a store then
   `a=d,d=A` → false.

## Step 2 — pump: replay first, publish `hasImages` (implement: sonnet)

Files: `picker/remotebridge/daemon/daemon.go` (outputSink struct + `start` pump +
`enqueueSeedWithReplay` doc only), `daemon_test.go`, `dropreseed_test.go`,
`reconcilereseed_test.go`.

1. `outputSink` gains `hasImages atomic.Bool` with a doc line: written only by the
   pump, read by the main loop's reveal pass.
2. In `start`: after `buf = gfx.Filter(buf)` add `s.hasImages.Store(gfx.Retained())`.
   For `FrameSeed` with `gfx != nil`: write `gfx.Replay()` as `FrameOutput`
   **before** writing the seed (skip when empty), then the seed; remove the
   after-seed replay block.
3. `enqueueSeedWithReplay` doc: stores are written immediately before the seed.
4. Tests: rename helper `seedThenReplayFrames` → `replayThenSeedFrames(t, peer)
   (replay, seed wire.Frame)` reading replay first; update its three callers
   (`daemon_test.go:464`, `dropreseed_test.go:136`, `reconcilereseed_test.go:142`)
   and rename `TestPauseContinueReplaysRetainedKittyStoreAfterSeed` →
   `…BeforeSeed`, its doc to match. These go red on the old pump order.

## Step 3 — `daemon/reveal.go` (implement: sonnet)

New file + `reveal_test.go`.

```go
// revealQueue: mutex-guarded set of local window ids a local client started
// displaying. Watcher goroutine adds; main loop takes.
type revealQueue struct{ mu sync.Mutex; wins map[string]struct{} }
func (q *revealQueue) add(win string)
func (q *revealQueue) take() []string // sorted, clears

// clientViews parses list-clients output lines
// "<control_mode>|<name>|<created>|<window_id>", skipping control-mode (1),
// blank and malformed lines, returning the set of "<name>|<created>|<window_id>".
func clientViews(out string) map[string]string // key -> window_id

// revealedWindows returns the window ids of views in cur absent from prev, sorted, deduped.
func revealedWindows(prev, cur map[string]string) []string

// clientViewsCmd returns the list-clients argv for the local session.
func clientViewsArgs(sess string) []string
// {"list-clients", "-t", sess, "-F", "#{client_control_mode}|#{client_name}|#{client_created}|#{window_id}"}

// watchReveal: same shape as watchLocalClient. On each tick whose nudge mtime
// advanced: views := query(); on a query error skip (keep prev). For each
// revealed window that isMirror(win) reports true, q.add(win); if any were
// added, wake() once. prev = views. Returns on stop.
func watchReveal(nudged func() (time.Time, bool), query func() (string, error),
    isMirror func(localWin string) bool, q *revealQueue, wake func() bool,
    stop <-chan struct{}, tick <-chan time.Time)

// wakeCmd: output-less, side-effect-free, so its %begin/%end only wakes the loop.
func wakeCmd(remoteSession string) string // "has-session -t " + tmuxQuote(sess)

// reseedRevealed runs on the main loop beside reseedReshaped: for each window id
// from q.take(), find mw in reg.all() with mw.localWin == id; for each of
// mw.allRemotePanes() whose sink exists, is revealable (not closed/paused) and
// hasImages.Load(), collect; then PaneSeeds + enqueueSeedWithReplay, logging
// errors like reseedReshaped does ("re-seed after reveal").
func reseedRevealed(reg *registry, router *Router, rt roundTrip, q *revealQueue)
```

`isMirror` in production: iterate `reg.all()` comparing `localWin` (immutable
after `reg.add`, and `reg.all()` takes the registry lock — safe off-loop, the
same access `activeFirst` already makes from `watchLocalClient`).

Add `func (s *outputSink) revealable() bool` (under `mu`: `!s.closed && !s.paused`)
next to `takeDirty` — put it in `reveal.go`, not daemon.go.

Tests in `reveal_test.go` (all red before: symbols absent / behavior missing):
- `TestClientViewsSkipsControlAndMalformed`.
- `TestRevealedWindows`: none→one view reveals W; unchanged set reveals nothing;
  same name, new `client_created` reveals again; window change `@1→@2` reveals @2
  only; view leaving reveals nothing.
- `TestWatchRevealQueuesMirrorWindowsAndWakes`: fake nudge advancing, fake
  query sequence (empty, then a real client on @7, then same), `isMirror`
  true for @7 → queue gets @7 once, wake called once; a control-mode-only
  query and a non-mirror window add nothing and don't wake; no nudge advance
  → query not called.
- `TestReseedRevealedReplaysBeforeSeed` — **the regression test for the user
  symptom**: registry with one mirror window (localWin @7, remote pane %1),
  router sink for %1 over `net.Pipe` with a `graphics.New(&stubLocalizer…)`
  proxy; route `testKittyStore("9")` as output and read that FrameOutput off
  the peer (so the pump has run and `hasImages` is true); queue @7;
  `reseedRevealed` with a test round-trip whose capture reply is `FRESH-CAPTURE`
  (copy the `%begin/%end` fixture shape used by
  `TestPauseContinueReplaysRetainedKittyStoreBeforeSeed`); assert the peer next
  reads `FrameOutput` containing `kittyLocalisedMarker`, then `FrameSeed`
  containing `FRESH-CAPTURE`. Second case: a sink with no retained store → no
  capture issued, nothing written (reader must not be consumed; assert with a
  short read deadline).

## Step 4 — wiring in `daemon.go` (implement: sonnet; minimal lines)

1. `resizeHookEvents` += `"session-window-changed"`; extend its doc: fires on a
   window switch inside the mirror session, which the reveal watcher needs.
2. In `Run`, declare `reveals := &revealQueue{}` next to `stopWatch`/nudge setup
   (before the main loop and before the watcher start).
3. Beside the `watchLocalClient` goroutine start, start a second goroutine
   with its own ticker (`time.NewTicker(resizePollInterval)`, stopped on exit)
   calling `watchReveal(nudged, func() (string, error) { return
   cfg.LocalTmuxOut(clientViewsArgs(cfg.LocalSess)...) }, isMirror, reveals,
   func() bool { return sendCtl(wakeCmd(cfg.RemoteSession)) }, stopWatch, t.C)`.
   Skip starting it when `cfg.LocalSess == ""` (no hook, nothing to watch).
   Confirm `cfg.LocalTmuxOut`'s signature before use.
4. Main loop: `reseedRevealed(reg, router, rt, reveals)` directly after
   `reseedReshaped(router, rt)`.
No other daemon.go changes (reconnect/backoff untouched — #729).

## Step 5 — docs (implement: sonnet)

`docs/agents/bridge-graphics-paste.md`: new bullet after "A sixel does not
survive a bridge reseed" — **"A local attach, switch-client or window switch
re-seeds the revealed panes that hold kitty stores (#731)"** covering: tmux drops
passthrough for undisplayed panes (the measurement); aeye's own visibility
recovery runs on the remote where the control client never changes; the reveal
watcher (list-clients views keyed with `client_created`, nudge incl.
`session-window-changed`), main-loop re-seed gated on retained stores; replay now
precedes the seed and why; retain cap 32; residual limits (B→A→B inside one poll,
relay-only/`--test-local` retains nothing, pruned cache file). Also fix any
existing sentence there that says replay follows the seed.

## Step 6 — gate (worker)

`cd picker && go test ./remotebridge/...` (race: `go test -race ./remotebridge/daemon/`),
then `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Ordering

1 → 2 → 3 → 4 → 5 (3 depends on 1's `Retained`, 2's `hasImages`; 4 on 3).
Steps 1+2 are one subagent; 3+4 one subagent; 5 one subagent after.

## Acceptance

- [ ] `TestReseedRevealedReplaysBeforeSeed` red before, green after.
- [ ] replay-first order pinned by the renamed pause/continue, drop and reconcile tests.
- [ ] Watcher tests cover control-mode skip, re-attach via `client_created`, non-mirror windows.
- [ ] `go test -race` clean for the daemon package.
- [ ] Doc updated; three nix gates green.
