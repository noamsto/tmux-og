# Bridge park — implementation plan (#729)

Spec: `docs/superpowers/specs/2026-09-22-bridge-park-design.md`.
All daemon paths are under `picker/remotebridge/daemon/`.

Orchestration consult: survey did not trip — one package plus three small
satellites (cmd flag, statusline badge, bats), no shared interface outside the
daemon package changes except `pumpInput`'s parameter list (package-private).

## Task 1 — park primitives (implement: sonnet)

Files: `backoff.go`, `daemon.go` (Config only), new `park.go`, new `park_test.go`.

1. `backoff.go`: add `WakeBackoff(now func() time.Time) Backoff` — Base 500ms,
   Ceiling 5s, MaxAttempts 10, MaxElapsed 30s, Jitter rand.Float64 — with a doc
   comment saying it is the one short cycle a wake from `parked` buys.
2. `daemon.go` Config: add `WakeRetry *Backoff` next to `Retry` (nil takes
   WakeBackoff) and `InputSeen func()` next to `RendererDied` (stamped once per
   Run, like RendererDied). Add `func (c Config) wakeSchedule() Backoff` beside
   `retrySchedule`.
3. `park.go`:
   - `const bridgeStateParked = "parked"` (move nothing; `bridgeStateDisconnected`
     stays in conn.go).
   - `parkWaker{armed atomic.Bool; ch chan struct{}}`, `newParkWaker()` (ch cap 1),
     `poke()` (no-op unless armed; non-blocking send), `arm()` (drain ch, then
     set armed), `disarm()`, `C() <-chan struct{}`.
   - `focusEdge{nudged func() (time.Time, bool); viewing func() bool; last time.Time; was bool}`
     with `reset()` (records current mtime and current viewing) and `poll() bool`
     (false unless mtime advanced past `last`; then re-reads viewing and returns
     `viewing && !was`, updating `was`).
   - `localViewing(cfg Config) bool`: `cfg.LocalTmuxOut("list-clients", "-F",
     "#{client_session}")`, true iff any line equals `cfg.LocalSess`; false on
     error or nil LocalTmuxOut / empty LocalSess.
   - `const parkDimStyle = "fg=#{@thm_overlay_0},bg=#{@thm_mantle}"`,
     `var parkDimOptions = [...]string{"window-style", "window-active-style"}`,
     `dimMirror(cfg, reg)` / `undimMirror(cfg, reg)`: per `reg.all()` window,
     `cfg.LocalTmux("set-option", "-w", "-t", mw.localWin, opt, parkDimStyle)` /
     `cfg.LocalTmux("set-option", "-w", "-u", "-t", mw.localWin, opt)`. No-op when
     `cfg.LocalSess == ""` (matching setBridgeState).
   - `parkFocusInterval = time.Second`.
4. `park_test.go`: poke while disarmed does not deliver; poke while armed
   delivers exactly one (second poke non-blocking, coalesced); arm drains a stale
   poke; focusEdge: no wake when viewing at reset, wake on false→true after an
   mtime advance, no wake without an mtime advance, no second wake while still
   viewing; dim/undim issue the expected argv via a recording LocalTmux.

Validation: `cd picker && go test ./remotebridge/daemon/ -run 'Park|Focus|Dim|WakeBackoff'`.

## Task 2 — reattach cycles + parked wait in Run (implement: opus)

Files: `conn.go`, `daemon.go`, `reattach_test.go`, `pumpinput_test.go` (call sites only).

1. `conn.go`: split `reattach`'s `for attempt` loop into
   `attemptCycle(cfg, router, hold, want, repair, bo Backoff) (*ctlConn, cycleResult)`
   with `cycleResult` ∈ {`cycleConnected`, `cycleTerminal`, `cycleExhausted`}.
   Body byte-for-byte the current loop except: `!ok` from `bo.Next` →
   `cycleExhausted` (keep the "giving up" stderr line, reworded "parking" is
   done by the caller); every other `return nil` → `cycleTerminal`; the success
   path keeps `clearBridgeState(cfg)` after `repair()` and returns
   `cycleConnected`. `start := bo.Now()` is per cycle.
2. `reattach(cfg, router, hold, want, repair, park func() bool) *ctlConn`:
   `hold.close()`, `setBridgeState(disconnected)`, `clearBridgeRes` as now; then
   `bo := cfg.retrySchedule()`; loop: run a cycle; connected → return conn;
   terminal → nil; exhausted → if `park == nil || !park()` return nil, else
   `setBridgeState(cfg, bridgeStateDisconnected)`, `bo = cfg.wakeSchedule()`,
   continue. Doc comment on reattach updated (exhaustion parks).
3. `daemon.go` Run:
   - Next to `death := newDeathNudge()`: `waker := newParkWaker()` and
     `cfg.InputSeen = waker.poke` (before any pumpInput can start).
   - `pumpInput(conn, remotePane, send, paste, died, seen func())`: call
     `seen()` (nil-guarded) for every `FrameInput` frame **before** the
     forwarding loop. Update **all five** production call sites
     (`daemon.go` ×3, `reconcile.go` ×2 — split and float panes — confirm with
     `rg -n 'pumpInput\(' --glob '!*_test.go'`) to pass `cfg.InputSeen`, and
     every test call site (`pumpinput_test.go`, others found by `rg -n
     'pumpInput\('`) to pass nil.
   - After `loopTick` is created, build `dimmed := false` and
     `park := func() bool { ... }`:
     `setBridgeState(cfg, bridgeStateParked)`; `dimMirror(cfg, reg)`;
     `dimmed = true`; `agents.clear()`;
     `fmt.Fprintf(os.Stderr, "daemon: %s unreachable; parked until the mirror is focused or typed into\n", cfg.RemoteHost)`;
     `focus := focusEdge{nudged: nudged, viewing: func() bool { return localViewing(cfg) }}`;
     `focus.reset()`; `waker.arm()`; `defer waker.disarm()`;
     `ft := time.NewTicker(parkFocusInterval)`; `defer ft.Stop()`;
     loop `select`: `<-waker.C()` → log "waking on input", return true;
     `<-cfg.Shutdown` → return false; `<-ft.C` → if `focus.poll()` log "waking on
     focus", return true; `<-loopTick.C` → if
     `sessionGone.observe(localSessionGone(cfg))` { `localSessionVanished = true`;
     return false }.
     (`nudged` is the existing closure; it is declared before the watcher
     starts, which precedes the attach loop — move its declaration up only if
     it is not already in scope at that point.)
   - Attach loop `connDrop` arm: `reattach(..., park)`; on non-nil:
     `replacer.cancel()`, and `if dimmed { undimMirror(cfg, reg); dimmed = false }`.
     A wake cycle that fails re-enters park, which re-dims (idempotent).
4. `reattach_test.go` (existing `reattachCfg`, `scriptConn` harness):
   - exhaustion with `park == nil` returns nil (existing behaviour, keep/adjust
     existing tests to the new signature);
   - exhaustion calls park exactly once and, when park returns false, reattach
     returns nil without further dials;
   - park returning true starts a second cycle bounded by `cfg.WakeRetry`
     (count dials: Retry.MaxAttempts dials, then WakeRetry.MaxAttempts dials,
     then park again);
   - wake cycle that connects returns the connection (identityMatch script);
   - identity mismatch during a wake cycle returns nil (terminal, park not
     called again).
   Use a stub `repair` returning true and `LocalSess: ""` so stamps no-op.

Validation: `cd picker && go test ./remotebridge/daemon/... && go vet ./remotebridge/...`.

## Task 3 — test knobs in cmd/daemon (implement: sonnet; after Task 1)

File: `picker/remotebridge/cmd/daemon/main.go`.

Add `--retry-max-elapsed` (env `OG_DAEMON_RETRY_MAX_ELAPSED`) and
`--wake-max-elapsed` (env `OG_DAEMON_WAKE_MAX_ELAPSED`) `flag.Duration`s,
default 0 = production schedule. When non-zero, set
`cfg.Retry = &b` with `b := daemon.DefaultBackoff(time.Now); b.MaxElapsed = v`
(likewise `cfg.WakeRetry` from `daemon.WakeBackoff`). Help text marks them as
test knobs. Follow the file's existing env-default helper style (write an
`envDurationDefault` only if none exists).

Validation: `cd picker && go build ./remotebridge/cmd/daemon && go vet ./remotebridge/cmd/...`.

## Task 4 — parked badge (implement: sonnet; independent)

Files: `picker/statusline/main.go`, `picker/statusline/main_test.go`.

In the `a.bridgeState != ""` arm: `parked` renders
`"#[fg=" + a.thmOverlay1 + "]" + bridgeParkedGlyph + " offline — press a key  "`;
any other non-empty value keeps today's red disconnected glyph. Add
`const bridgeParkedGlyph = "󰤮"` with `// nerd: nf-md-wifi_off` (if the literal
glyph mangles, follow the Nerd Font rule: variable + nerd comment). Add
`TestRenderLineBridgeStateParked` mirroring the disconnected test.

Validation: `cd picker && go test ./statusline/`.

## Task 5 — offline bats (implement: sonnet; after Tasks 2–3)

File: `tests/remote-m2-integration.bats`, beside the #482 drop tests.

Outage helpers: `src_sock="$TMUX_TMPDIR/tmux-$(id -u)/m2src"`;
`outage_start() { kill -9 "$(transport_child)"; mv "$src_sock" "$src_sock.away"; }`
and `outage_end() { mv "$src_sock.away" "$src_sock"; }`. Writes to SRC while
the socket is aside go through `tmux -S "$src_sock.away" …` ($SRC resolves the
moved path and would hit ENOENT). `teardown()` gains, before its `$SRC
kill-server`: `[ -S "$src_sock.away" ] && tmux -S "$src_sock.away" kill-server 2>/dev/null || true`
(define `src_sock` in `setup()` so teardown sees it), so a case that ends
parked never leaks the SRC server. Start the daemon with
`OG_DAEMON_RETRY_MAX_ELAPSED=2s OG_DAEMON_WAKE_MAX_ELAPSED=2s` (check how
`bridge_up` launches the daemon and pass env through it). Helper
`wait_bridge_state <value> <tag>` polling `show-options -v -q @bridge_state`
(empty value = unset).

Cases:
1. parks: outage → `parked` within ~10s; `has-session` ok; window count and
   pane count unchanged; `show-options -w -v -t <win> window-style` equals the
   dim style on every mirror window.
2. keypress reconnects: park, `tmux -S "$src_sock.away" send-keys -t rem 'echo PARK_WAKE_7Q' Enter`,
   `outage_end`, `$DST send-keys -t host-sess:1 x` → state unset; window-style
   no longer set per-window (`show-options -w -q -v` empty); mirror contains
   PARK_WAKE_7Q; a new transport child exists.
3. keypress while still down: park, `$DST send-keys` → observe `disconnected`,
   `"$CTL" --sock "$sock" ping _` succeeds during the cycle, pane_dead 0, then
   `parked` again; a second key → `disconnected` again (re-armed).
4. SIGTERM while parked: park, `kill -TERM $daemon_pid`, wait ≤2s for exit;
   socket and pidfile gone; `has-session` fails.
5. identity mismatch on wake: park, `rm "$src_sock.away"`-style is wrong — do
   `outage_end`, `$SRC kill-server`, `$SRC new-session -d -s rem -x 100 -y 30`
   (fresh server, same name), key → log contains "different tmux server";
   daemon exits; session gone.

Validation: `bats tests/remote-m2-integration.bats -f 'park'` (from the devshell)
then the full file.

## Task 6 — docs (implement: sonnet; after Task 2)

`docs/agents/bridge-daemon.md` "Bridge Reconnect": rewrite the reattach
"tears the whole mirror down if it cannot re-dial" clause and the "exhausted
retry budget" item in "Only a bare EOF is a drop" to say exhaustion parks;
extend the `@bridge_state` bullet with `parked` and its badge; add a **parked**
bullet (wake by keypress via `InputSeen`/focus via the resize-nudge mtime +
viewing edge, keystrokes dropped, dim via per-window `window-style` restored with
`-u`, `agents.clear()` on park, `@bridge_crew_*`/`@bridge_proc` stay frozen,
what stays terminal, session-gone probe while parked, test outage = SIGKILL +
socket move). Check `docs/agents/bridge-shipped-state.md` for `@bridge_state`
value lists and update if it enumerates them.

## Commit

The spec and this plan are committed with the code
(`docs/superpowers/specs/2026-09-22-bridge-park-design.md`,
`docs/superpowers/plans/2026-09-22-bridge-park.md`).

## Gate

`nix build .#default`, `nix flake check`, `nix build .#lint`.
