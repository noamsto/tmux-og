# Bridge image paste progress (#1007)

A `ctrl+v` image paste into a mirror pane swallows the keystroke and uploads in
the background (up to 8 MiB over ssh, 5s bound) while later input is held
behind the pane's paste lock. Nothing on screen says it is happening. This adds
a progress indicator from the swallow until the path lands or a failure message
replaces it.

## Design choice

| Option | Verdict |
|---|---|
| (a) `display-message -c <client> -d <ttl>` refreshed on a ticker | **Chosen.** |
| (b) `@bridge_paste` window option rendered by the status line | Rejected. The status line redraws only on its 1s `#()` cadence (a spinner would stutter), a control-mode-only host draws none, and it is window-scoped, so every client of the session would flash. |
| (c) Overlay inside the renderer pane | Rejected. The renderer owns the pane's screen; an overlay needs repaint bookkeeping against remote output. |

Why (a) fits:

- `display-message -c` is per client, so only the pasting client shows it, and
  it takes no focus.
- Each refresh carries a finite delay of four ticks (1s). A daemon that dies,
  or a goroutine that never reports, leaves a message that expires by itself:
  it can never stay stuck. (`-d 0` would persist until a keypress, so it is
  not used.)
- No new option names, so the daemon-writes-only-`@bridge_*` rule is untouched.

## Behaviour

- The daemon cannot tell which client sent the byte. It targets the session's
  most recently active client (`#{client_activity}`), which is the one that
  just typed `ctrl+v`. The client is resolved once, in the paste goroutine,
  not on the input pump. `list-clients -F '#{client_activity}|#{client_name}'`
  (`|`-delimited, name last); the highest activity wins, the first listed on a
  tie. No client: progress is skipped silently, the paste proceeds.
- Every message the paste goroutine shows goes to that same client: frames,
  `image pasted`, and each failure (including the dropped-send notify in
  `sendChunks`). Inside `paste()` the handler's `notify` is replaced by a
  `status(client, msg, ttl)` call, so a failure cannot land on another client
  of the session. Pre-paste notifies in `handle` (gate, probe) are unchanged.
- Message text is escaped like `notifyLocal` (`#`->`##`, `%`->`%%`, after `--`);
  the escaper is shared, not duplicated.
- The ticker starts after the kept-prefix `sendChunks`, and a refused kept
  prefix aborts the paste (the pane input is already out of order), so no frame
  can overwrite that notify.
- Phases, in one `paste()` goroutine, driven by a ticker goroutine reading an
  atomic label:
  1. `reading clipboard image`
  2. `uploading image (1.2 MiB)`
  Each frame renders `<spinner> <label> · <elapsed>`; the first frame is drawn
  at once. Bytes sent are not reported: the upload is one `ssh` exec behind an
  injected function, with no cheap progress hook.
- The ticker is stopped (and joined) before any terminal message, so a stale
  frame never overwrites it.
- Terminal message: success shows `image pasted` for 1.5s; every failure keeps
  its existing `notify` (5s), which replaces the spinner. All paths, including
  an upload cancelled by timeout or a refused `send-keys` during a reconnect,
  stop the ticker (`defer`).
- Typed input held behind the paste is unchanged: the lock handoff in `handle`
  and the `paste` ordering are not touched. Showing a "queued" count is out of
  scope.
- Cost: one `display-message` fork per 250ms tick, only for the few seconds a
  paste runs, plus one `list-clients` fork per paste.

## Steps

1. `paste.go`: `pasteProgress` (ticker, label, idempotent `stop`), handler
   fields `clientFor`/`status`/`tick`, `activeClient(cfg)`, `sendChunks` returns
   whether it was fully sent, `paste` drives the progress. `paster()` wires them.
2. `paste_test.go`: slow fake upload, injected `tick` and a channel-recording
   `status` fake (no wall-clock sleeps); assert the ticker is joined before the
   terminal message (no frame after it), failures target the pasting client; assert start-before-complete, at least one
   update, cleared or replaced on success, failure, upload cancelled and refused
   inject; serialization test unchanged.
3. `paste_progress_test.go` (no new Go dependency, no pty library): an inner
   scratch server (`startIsolatedTmux`-style, own `TMUX_TMPDIR` and
   `CLAUDE_STATUS_DIR`) is attached by two clients, each running as a pane of an
   outer scratch server (`env -u TMUX tmux -L inner attach`), the pattern in
   `tests/client-theme.bats`. `send-keys` to one outer pane makes it the most
   recently active client. The real `activeClient`/`status` wiring runs against
   the inner socket with a fake slow upload; `capture-pane -p` on each outer pane
   shows the frame only on the active client's pane, a failure message on that
   same pane, and nothing after the TTL. Keeps the `OG_REQUIRE_TMUX`/LookPath
   skip guard.
4. `docs/agents/bridge-graphics-paste.md`: document the indicator and client
   targeting; `docs/agents/performance.md` fork note if it lists fork sources.
5. Gate: `nix build .#lint`, `nix flake check`, `nix build .#default`.
