# Plan: recover a parked mirror across a remote server restart (#817)

Spec: `docs/superpowers/specs/2026-09-27-bridge-server-restart-reopen-design.md`.

All Go paths below are under `picker/remotebridge/`. Go commands run from
`picker/`. The bats suite needs prebuilt binaries for speed:

```
B=$SCRATCH/bin; (cd picker && go build -o $B/daemon ./remotebridge/cmd/daemon && go build -o $B/renderer ./remotebridge/cmd/renderer && go build -o $B/ctl ./remotebridge/cmd/ctl && go build -o $B/statusline ./statusline)
DAEMON=$B/daemon RENDERER=$B/renderer CTL=$B/ctl STATUSLINE=$B/statusline bats -f '<filter>' tests/remote-m2-integration.bats
```

`$SCRATCH/bin-main` already holds the same four binaries built from
origin/main (e78655a) for the red runs.

## File list

- `daemon/backoff.go` — `RestoreBackoff`, `probeBackoff`.
- `daemon/daemon.go` — `Config.RestoreRetry`/`ParkProbe` (appended after
  `View`) and their schedule accessors; `Run` renamed `runMirror`; first dial
  publishes `View.setAdvertised`; `park` closure returns a `parkVerdict` and
  gains the probe ticker; attach loop reads `reattach`'s ending; teardown's
  last step switches on it; reopen notice goroutine started before the main
  loop; `runMirror` returns `errServerReplaced`.
- `daemon/conn.go` — `attachWatch` recorder in `newCtlConn`;
  `ctlConn.attachRefusal`; `cycleRefused`/`cycleReplaced`; `attemptCycle`'s
  `restoring` parameter; `reattach` returns `(conn, reattachEnd)` and runs the
  restore cycle and the three park verdicts.
- `daemon/park.go` — `parkVerdict`, `parkProbeInterval`.
- `daemon/reopen.go` (new) — `Run` loop, `errServerReplaced`,
  `resetMirrorSession`, `tombstoneMirror`/`tombstoneText`,
  `showReopenNotice`.
- `daemon/reattach_test.go` — adapt existing cases to the new signatures; new
  refusal/restore/probe/replaced cases.
- `daemon/reopen_test.go` (new) — `tombstoneText`, `resetMirrorSession`
  argv, `showReopenNotice` (fake LocalTmux).
- `daemon/park_test.go` — `RestoreBackoff` schedule pin.
- `cmd/daemon/main.go` — `--restore-max-elapsed`, `--park-probe-interval`,
  `--test-outage-file`.
- `cmd/daemon/main_test.go` — the outage-file dial branch.
- `tests/remote-m2-integration.bats` — outage helpers, `server_restart`
  helper, 4 new cases, 2 rewritten cases.
- `docs/agents/bridge-daemon.md` — "Bridge Reconnect" bullets per spec Docs.
- `docs/superpowers/specs/2026-09-27-bridge-server-restart-reopen-design.md`
  and `docs/superpowers/plans/2026-09-27-bridge-server-restart-reopen.md` —
  committed with the code.

## Consumer map

What the change alters that anything outside the reattach path reads: the
error classification of a dial (refused vs unreachable), `reattach`'s
endings, the daemon's lifetime (in-process rebuild; exit on gone), the
pidfile/socket across a rebuild, and two new local-session shapes — the
transient **placeholder** (one `sleep` window, no `@bridge_win`, session
options intact, pidfile present, socket absent for the rebuild's ~1s) and
the **tombstone** (one `sh` notice window, no `@bridge_win`, `@bridge_sock`/
`@bridge_state` unset, `@bridge_host`/`@bridge_session` kept, daemon gone).

| consumer | reads | disposition |
|---|---|---|
| `readIdentity` callers: `newSessionPin` (first attach), `attemptCycle`, `replaceConn` | identity errors | **changed** only in `attemptCycle`, which alone consults `attachRefusal`; first attach and `replaceConn` ignore it (Step 3 cases 1–4 pin the classifier; existing replaceconn tests stay green). |
| `newCtlConn`/`dialConn` callers (first dial, reattach, `replaceConn`) | unverified round-trip | **compatible**: `attachWatch` only records; `bind` still uses the pump (all existing reattach/replaceconn tests). |
| `reattach` callers | `*ctlConn` | **changed**: only `runMirror`'s attach loop (Step 4/5) and tests. |
| `Run` callers | exported `Run(cfg) error` | **compatible**: same signature; `cmd/daemon` only. A reopened run's early failure now returns its error after tombstoning — `fatal` logs it, exit 1; the launcher ignores the detached daemon's status. |
| `og-remote-open.sh` pair lookup + `probe_daemon` | `@bridge_host`/`@bridge_session`, pidfile, ctl ping | **compatible**: tombstone → pair found, pid dead/absent → `rm` + `kill-session` + recreate (spec-critic verified). Placeholder → pid alive but no socket → `REPLY` empty → launcher recreates; a narrow race (user reopening in the ~1s rebuild) with the same shape as two concurrent first opens, not new. |
| `og-remote-detach.sh` | `@bridge_sock`, pidfile | placeholder: **changed-safe** — pidfile kept (Step 5), so it SIGTERMs the daemon; `Run` sees `Shutdown` and `kill-session`s. Tombstone: `@bridge_sock` unset → prints "not a bridged session", leaves it; the tombstone closes on Enter. Documented. |
| picker `stopBridgeDaemon` (`picker/remote.go`) | `@bridge_sock` + pidfile | same as detach; tombstone → no-op then the picker's own `kill-session`. |
| picker `parseBridgeMirrors`/`bridgeSessionPresent`, Tab scope, Host column (`picker/remote.go`, `tui.go`, `main.go`) | `@bridge_host`/`@bridge_session` | **compatible, documented**: a tombstone reads as the open mirror of its pair, so the Remote section hides that pair's row and Enter switches to the tombstone, which explains itself; one Enter closes it and the row returns. |
| picker resources (`picker/remote_resources.go`) | mirror pairs, `@bridge_res` | **compatible**: `@bridge_res` cleared by teardown; the ssh `ps` fallback finds no session and shows nothing. |
| `picker/statusline` | `@bridge_win`, `@bridge_host`, `@bridge_state` | **compatible**: no new `@bridge_state` value; placeholder/tombstone windows lack `@bridge_win`, so line 0 renders them as local windows. The restore window shows the existing red `disconnected`; a probe leaves `parked`. |
| config binds (`BridgeGate`: `@bridge_win` && `@bridge_sock`), `generator/render/keys.go` | window + session options | **compatible**: placeholder/tombstone fail the gate → local binds, as for the launcher's loading window. |
| `og-remote-theme.sh` | `@bridge_sock` sessions | **compatible**: tombstone skipped (unset); placeholder → ctl to a missing socket fails silently, as for any unreachable daemon. |
| local scripts skipping mirror windows (`tmux-update-icons`, `tmux-reflow-windows`, `tmux-worktree-match`, `tmux-issue-stamp`, `tmux-pr-enrich`, `tmux-shell-prompt`, `tmux-reconcile-window`) | `@bridge_win` | **compatible, cosmetic**: placeholder/tombstone are ordinary windows (a `sleep`/`sh` icon), like the loading window. |
| tmux-remux (local persistence, `persist.md`) | local sessions | **compatible**: a saved tombstone can be resurrected after a local restart; `og-remote-open` already discards such ghosts. Doc line in Step 9. |
| `@bridge_state` / dim / `agents.clear()` on the park path | park closure | **changed**: a probe keeps `parked` and a post-probe re-park is quiet; the next full park after any other path is loud (Step 8 case 4 pins it). Keys pressed while a probe's single dial is in flight are dropped (waker disarmed), up to the 30s identity deadline on a black-holed host — acceptable, documented in Step 9. |

## Steps

- [ ] **Step 1: harness outage model (test-first on the unchanged daemon).**
  `cmd/daemon/main.go`: add `testOutage := flag.String("test-outage-file",
  os.Getenv("OG_DAEMON_TEST_OUTAGE_FILE"), "test-local: while this file
  exists, a dial yields no control output (an unreachable remote)")`. In the
  `*testLocal` `newCtlCmd` closure, before building the tmux command: if
  `*testOutage != ""` and `os.Stat(*testOutage)` succeeds, return
  `exec.Command("false"), ""` (env via `localCtlCmdEnv(view)` like the other
  branch). Pull the decision into `func testLocalDialArgv(outage, src,
  session string) []string` returning either `["false"]` or the tmux argv, so
  `main_test.go` can pin both branches (`TestTestLocalDialArgvHonoursOutageFile`:
  temp file present → `false`; absent → `tmux -L src -C attach-session -t
  sess`). Bats: in `setup()` add
  `export OG_DAEMON_TEST_OUTAGE_FILE="$BATS_TEST_TMPDIR/outage"`;
  `outage_start` gets `: >"$OG_DAEMON_TEST_OUTAGE_FILE"` as its **first**
  line (before the `kill -9`), `outage_end` gets `rm -f
  "$OG_DAEMON_TEST_OUTAGE_FILE"` as its **last** line (after the `mv` back),
  so no dial can land in a socket-aside window without the marker.
  Proof: `cd picker && go test ./remotebridge/cmd/daemon/` green; rebuild the
  daemon and run `bats -f 'park|parked|outage'` → every existing park case
  green with the old reattach logic.

- [ ] **Step 2: schedules and Config fields.** `backoff.go`:
  `RestoreBackoff(now)` = Base 1s, Ceiling 5s, MaxAttempts 30, MaxElapsed
  60s, `rand.Float64` jitter, with a doc comment carrying the spec's D2
  rationale (restoreMode auto vs off); `probeBackoff(now)` = MaxAttempts 1,
  Base 0. `daemon.go` Config, appended after `View`: `RestoreRetry *Backoff`
  (nil = RestoreBackoff) and `ParkProbe time.Duration` (0 =
  `parkProbeInterval`), plus accessors `restoreSchedule()`,
  `probeSchedule()` (always `probeBackoff(time.Now)`), `parkProbeEvery()`
  beside `wakeSchedule`. `park.go`: `const parkProbeInterval = 2 *
  time.Minute`; `type parkVerdict int` with `parkStop`, `parkWoken`,
  `parkProbe`. Test first in `park_test.go`:
  `TestRestoreBackoffMatchesTheDesignedSchedule` (mirrors the existing wake
  test) and `TestProbeBackoffIsOneImmediateAttempt` (`Next(1)` → `0, true`;
  `Next(2)` → `false`). Proof: `go test ./remotebridge/daemon/ -run
  'Backoff'` red before, green after.

- [ ] **Step 3: failing Go tests for refusal/restore/probe/replaced.**
  `reattach_test.go`: add `eofScriptConn` (like `scriptConn` but Read returns
  `io.EOF` once the script is spent) and `epipeConn` (Write returns
  `syscall.EPIPE`, Read replays its script then EOF). Constant
  `refusedAttach = "%begin 1 1 0\ncan't find session: A\n%error 1 1 0\n%exit\n"`
  (raw, never `withBarriers`: it contains no flagged block). Change every
  existing `reattach(...)` call to the two-value form and every `park` fake to
  return `parkVerdict` (`true` → `parkWoken`, `false` → `parkStop`). New tests:
  1. `TestAttemptCycleRefusedAttachIsAVerdict` — dial returns
     `eofScriptConn(refusedAttach)`: `attemptCycle(…, restoring=false)` →
     `cycleRefused` after exactly 1 dial.
  2. `TestAttemptCycleRefusalSeenAfterAFailedWrite` — same with `epipeConn`.
  3. `TestAttemptCycleLaterUnflaggedErrorIsNotARefusal` — script
     `"%begin 1 1 0\n%end 1 1 0\n%begin 1 2 0\nhook failed\n%error 1 2 0\n"`
     then EOF, Retry of 2 attempts → `cycleExhausted`.
  4. `TestAttemptCycleNoControlOutputIsUnreachable` — `eofScriptConn("")` →
     `cycleExhausted` (the production network-loss shape).
  5. `TestReattachRestoreWindowEndsGoneWhenStillRefused` — every dial
     refused, RestoreRetry of 3 attempts → `(nil, endGone)`, dials = 1 + 3,
     park never called.
  6. `TestReattachRestoreWindowReopensOnANewServer` — dial 1 refused, dial 2
     answers a mismatched identity → `(nil, endReplaced)`.
  7. `TestReattachRestoreWindowReconnectsToTheSameServer` — dial 1 refused,
     dial 2 `identityMatch` → live connection, published in `hold`.
  8. `TestReattachRestoreWindowParksWhenTheHostVanishes` — dial 1 refused,
     then dial errors for the whole restore window → park called once.
  9. `TestReattachProbeRunsOneAttemptAndKeepsTheBadge` — Retry exhausts,
     park returns `parkProbe` then `parkStop`; a `LocalTmux` recorder on a
     Config with `LocalSess` set shows no `@bridge_state disconnected` stamp
     between the two parks, and dials = Retry + 1.
  10. Adapt `TestReattachDropsOutputFromAnUnverifiedConnection` and
     `TestReattachWakeCycleIdentityMismatchIsTerminal` to assert
     `endReplaced`; add `TestReattachMalformedIdentityIsAPlainTeardown`
     (reply `garbage` → `endTeardown`).
  Proof: `go test ./remotebridge/daemon/ -run 'Reattach|AttemptCycle'` fails
  to compile / fails (expected red).

- [ ] **Step 4: refusal recorder and cycle results** (implement: escalated).
  `conn.go`: `type attachWatch struct { r lineReader; seen bool; refusal
  string; refused bool }` whose `Next` records the first `End`/`Error` line
  it sees (`seen`), and when that first one is `Error` with `Flags !=
  controlmode.ClientCommandFlag` keeps `strings.TrimSpace(string(l.Data))`.
  `ctlConn` gains `watch *attachWatch`; `newCtlConn` builds it over `c.pump`
  and passes it (not the pump) to the unverified `newRoundTrip`; `bind`
  unchanged. `func (c *ctlConn) attachRefusal(d time.Duration) (string,
  bool)`: `disarm := armIdentityDeadline(c, d)`, drain `c.watch.Next()` until
  `!ok`, `disarm()`, return the record. `const refusalDrainTimeout = 5 *
  time.Second`, capped at `cfg.identityTimeout()`. `attemptCycle` gains
  `restoring bool` and a `refused` flag: in the retry-shaped identity
  failure branch, when `live`, call `next.attachRefusal(...)` before
  `next.close()`; on a refusal log `daemon: %s refused the attach to %s
  (%s)` and set `refused = true`, returning `cycleRefused` unless
  `restoring`; every other failed attempt sets `refused = false`. At
  exhaustion return `cycleRefused` if `refused`, else `cycleExhausted`. The
  mismatch branch returns the new `cycleReplaced` (log line unchanged but
  ending `; re-opening onto it`). `reattach` returns `(*ctlConn,
  reattachEnd)` (`endTeardown`, `endReplaced`, `endGone`) with the loop from
  the spec (D2/D6): `cycleRefused` → `endGone` if already restoring, else
  stamp `disconnected`, `bo = cfg.restoreSchedule()`, `restoring = true`;
  `cycleExhausted` → `restoring = false`, park; `parkWoken` → stamp
  `disconnected`, wake schedule; `parkProbe` → probe schedule, no stamp;
  `parkStop` or nil park → `endTeardown`. Update the doc comments of
  `reattach`, `cycleResult`, `attemptCycle`. Keep the `newCtlConn` change to
  the one line that passes `c.watch` (plus the field init) — #808 edits the
  routing code around it. Also adapt the one production call site in
  `daemon.go` just enough to compile: `park` returns `parkVerdict` (existing
  arms mapped: waker/focus → `parkWoken`, Shutdown/session-gone →
  `parkStop`) and `if c, _ = reattach(...); c == nil { break attach }`; Step
  5 then fills in the endings. Proof: `cd picker && go build ./... && go
  test ./remotebridge/daemon/` green, including every Step 3 test.

- [ ] **Step 5: Run split, park verdicts, teardown endings, reopen**
  (implement: escalated). `daemon.go`: rename `func Run(cfg Config) error`
  → `func runMirror(cfg Config) error` (move the "Run mirrors every window…"
  doc to `reopen.go`'s `Run`, keep a one-line doc on `runMirror`).
  Immediately before the first `dialConn(cfg)` take `term :=
  cfg.View.Desired()`; after `c.bind(router)` call
  `cfg.View.setAdvertised(term)`. `park` closure: signature `func()
  parkVerdict` (already, from Step 4); add `afterProbe bool` in `runMirror`'s
  scope beside `dimmed`. At park entry consume it once — `quiet :=
  afterProbe; afterProbe = false` — and skip the `dimMirror` + "parked
  until…" log only when `quiet` (stamp and `agents.clear()` stay
  unconditional). This way any path out of a probe (reconnect, re-open,
  restore) leaves the next park a full one. Add `probe :=
  time.NewTicker(cfg.parkProbeEvery())` with `defer probe.Stop()`; the probe
  arm logs `daemon: %s: probing` and returns `parkProbe` after setting
  `afterProbe = true`. Attach loop: `var ending
  reattachEnd` declared beside `localSessionVanished`; `c, ending =
  reattach(...)`; nil → `break attach`. Teardown's final `kill-session`
  block becomes: `localSessionVanished` → nothing (unchanged); `ending ==
  endReplaced` → `resetMirrorSession(cfg, "sleep", "2147483647")` — and on
  this ending teardown also skips `os.Remove(pidFile)`, so the pidfile names
  the (same) live daemon through the rebuild: `og-remote-detach` and the
  picker's `stopBridgeDaemon` then SIGTERM it instead of falling back to a
  bare `kill-session` under a half-built mirror (the new run rewrites the
  same pid); `ending ==
  endGone` → `tombstoneMirror(cfg)`; else `kill-session` (unchanged). After
  the final `teardown()`: `if ending == endReplaced { return
  errServerReplaced }`. Before `attach:` (after `loopTick`), `if cfg.reopened
  { go showReopenNotice(cfg, nudged, stopWatch) }`. Config gets unexported
  `reopened bool` beside the new fields.
  `reopen.go`: `var errServerReplaced = errors.New(...)`; `Run(cfg)` loops
  `runMirror`; on `errServerReplaced` → if `stopped(cfg.Shutdown)`
  `kill-session` and return nil, else log `daemon: %s: re-opening %s on the
  new tmux server`, set `cfg.reopened = true`, continue; any other return
  when `cfg.reopened && err != nil` → if stopped, `kill-session`, else
  `tombstoneMirror(cfg)`; in both cases also `os.Remove(cfg.SockPath + ".pid")`
  (the replaced teardown kept it for the rebuild); return err. The stop
  branch after `errServerReplaced` removes it too. `resetMirrorSession(cfg, argv...)
  (string, bool)`: `list-windows -t LocalSess -F '#{window_id}'` (old ids),
  `new-window -d -P -F '#{window_id}' -a -t LocalSess:{end} -- argv…`,
  `parseWindowID`, then `kill-window -t` each old id; returns the new id.
  `tombstoneMirror(cfg)`: `resetMirrorSession(cfg, "sh", "-c",
  tombstoneScript, "sh", tombstoneText(cfg.RemoteHost, cfg.RemoteSession))`,
  `set-option -w -t <id> remain-on-exit off`, `set-option -u -t LocalSess
  @bridge_sock` and `@bridge_state`, log the text. `tombstoneText` drops
  every `unicode.IsControl` rune from host and session.
  `showReopenNotice(cfg, nudged, stop)`: text `<host>: tmux server restarted
  — now mirroring a fresh <sess>` (host/session through the same
  control-byte strip); logs it; if `localViewing(cfg)` → `notifyLocal` and
  return; else loop on a 1s ticker (`focusEdge{nudged, viewing}` after
  `reset()`) plus a 15s ticker (direct `localViewing`), returning on `stop`
  or after one successful `notifyLocal`. Proof: `cd picker && go build
  ./... && go vet ./remotebridge/... && go test ./remotebridge/...` green.

- [ ] **Step 6: reopen unit tests.** `reopen_test.go`, fake `LocalTmux`/
  `LocalTmuxOut` recording argv (reuse `argvKey` from `park_test.go`):
  `TestTombstoneTextStripsControlBytes` (`"h\x1b]0;x\x07"`, `"s\x9bq"` →
  no rune with `unicode.IsControl`); `TestResetMirrorSessionKillsEveryOldWindow`
  (list-windows answers `@1\n@4\n`, new-window answers `@9` → one
  `new-window … -- sleep 2147483647`, then `kill-window -t @1` and `-t @4`,
  never `@9`, returns `@9`); `TestTombstoneMirrorUnsetsTheDaemonOptions`
  (`remain-on-exit off` on the new window, `-u @bridge_sock`, `-u
  @bridge_state`); `TestShowReopenNoticeWaitsForAViewer` (LocalTmuxOut
  answers no client first, then the mirror session on the 2nd call; a
  `nudged` func whose mtime advances; exactly one `display-message` lands;
  closing `stop` before any viewer lands none). Proof: `go test
  ./remotebridge/daemon/ -run 'Tombstone|ResetMirror|ReopenNotice'` green.

- [ ] **Step 7: daemon flags for the new schedules.** `cmd/daemon/main.go`:
  `--restore-max-elapsed` (`envDurationDefault("OG_DAEMON_RESTORE_MAX_ELAPSED",
  0)`) → `b := daemon.RestoreBackoff(time.Now); b.MaxElapsed = …;
  cfg.RestoreRetry = &b`; `--park-probe-interval`
  (`envDurationDefault("OG_DAEMON_PARK_PROBE_INTERVAL", 0)`) →
  `cfg.ParkProbe`. Proof: `go build ./remotebridge/cmd/daemon`; `daemon
  --help 2>&1 | grep -c 'restore-max-elapsed\|park-probe-interval'` → 2.

- [ ] **Step 8: bats cases (red on main, green on branch).**
  `tests/remote-m2-integration.bats`, after the park cases. Helper
  `server_restart [session…]`: `tmux -S "$src_sock.away" kill-server`, `rm
  -f "$src_sock.away"`, for each argument `$SRC new-session -d -s "$arg" -x
  100 -y 30`, then `rm -f "$OG_DAEMON_TEST_OUTAGE_FILE"`. Each case exports
  `OG_DAEMON_RETRY_MAX_ELAPSED=2s OG_DAEMON_WAKE_MAX_ELAPSED=2s` and, where
  used, `OG_DAEMON_RESTORE_MAX_ELAPSED=3s`.
  1. "a parked mirror whose remote server restarted without its session ends
     in a tombstone" — `bridge_up`, `outage_start`, parked,
     `server_restart other`, press keys (at most `PARK_WAIT_BUDGET_SECS*10`
     iterations of 0.1s) until the log shows `refused the attach`, recording
     `refused=yes|no`, then wait for the daemon (bounded 15s loop on `kill
     -0`, recording `exited=yes|no`, `kill -9` fallback); assert
     `refused = yes`, `exited = yes`,
     `! -e $sock`, `! -e $sock.pid`, `host-sess` has one
     window, `capture-pane` shows `no longer exists`, `@bridge_sock` unset;
     `send-keys Enter` → `has-session -t =host-sess` fails within 5s.
  2. "a parked mirror whose remote server restarted re-opens onto the
     same-named session" — before the outage `$SRC send-keys -t rem 'echo
     OLDSRV_4K1' Enter` and wait for it in the mirror; `outage_start`, parked,
     `server_restart rem`, `$SRC send-keys -t rem 'echo NEWSRV_8P3' Enter`,
     press keys (same bound, recording `reopened=yes|no`) until the log shows
     `re-opening`; then poll (≤`PARK_WAIT_BUDGET_SECS`) until
     `mirror_contains 1 NEWSRV_8P3` and `@bridge_state` empty; assert daemon
     alive, `transport_child` non-empty, `window-style`/`window-active-style`
     unset on every window, `@bridge_win` = 1 on window 1, no mirror pane's
     `capture-pane -p` contains `OLDSRV_4K1`. Then attach the pty client
     (`$OBS new-session -d -s obs -x 100 -y 30 "$DST attach -t host-sess"`)
     and poll `$OBS capture-pane -p -t obs` for `tmux server restarted`
     every 0.5s for up to 25s (the 15s backstop plus the 5s display), with
     the log's notice line asserted first. Assert `reopened = yes` before any
     of the live-mirror checks, so the red run fails on it rather than
     hanging.
  3. "a refused mirror re-opens when the session is restored inside the
     window" — as 1 with `OG_DAEMON_RESTORE_MAX_ELAPSED=10s`; once the log
     shows `refused the attach` (same bounded loop, asserted), `$SRC
     new-session -d -s rem -x 100 -y 30`;
     assert the log shows `re-opening`, `@bridge_state` ends empty, daemon
     alive, `mirror_contains` a marker sent to the new `rem`.
  4. "a parked mirror re-probes on its own once the outage clears" —
     `OG_DAEMON_PARK_PROBE_INTERVAL=1s`; `outage_start`, parked, send a marker
     through `tmux -S "$src_sock.away"`, `outage_end`, **no key**; poll ≤15s
     for `@bridge_state` empty; assert undimmed and marker painted. Then a
     second `outage_start` → parked again → every window dimmed and the log
     has a second `unreachable; parked` line: pins that a probe that
     reconnected does not leave the next park quiet.
  Rewrite "a control-connection drop into a different tmux server tears the
  mirror down" → "…re-opens the mirror onto the new server": same setup,
  then assert the `different tmux server` log line, the daemon still alive,
  and the mirror painting a marker sent to the new server. Delete "a
  keypress that wakes a parked mirror into a different tmux server tears the
  mirror down" (case 2 covers it through the wake path).
  Proof (red): `DAEMON=$SCRATCH/bin-main/daemon … bats -f 'tombstone|re-opens|re-probes'`
  → cases 1–4 and the rewrite fail; record each failing assertion line.
  Proof (green): same with the branch build → all pass; then the whole file
  `bats tests/remote-m2-integration.bats` green.

- [ ] **Step 9: docs.** `docs/agents/bridge-daemon.md` "Bridge Reconnect":
  the reattach/replaceConn bullet (endings: plain / replaced → re-open /
  gone → tombstone); "Only a bare EOF is a drop" (the socket move alone
  reads as a refusal — tmux's `CMD_STARTSERVER` answers `no sessions` — so
  the park cases also set `OG_DAEMON_TEST_OUTAGE_FILE`); "Server identity is
  the correctness cliff" (a refused attach is a verdict; mismatch now
  re-opens in-process onto a fresh mirror, never spliced; the second dial is
  deliberate); the `@bridge_state` bullet (a probe keeps `parked`); the park
  bullet (`park` verdicts, the 2-minute probe, the restore window, the new
  test knobs); one line on placeholder/tombstone windows carrying no
  `@bridge_win` and tmux-remux ghosts; one line that keys pressed while a
  probe's single dial is in flight are dropped (the waker is disarmed). Proof: `nix build .#lint` (typos,
  markdown hooks) green.

- [ ] **Step 10: full gate.** `nix build .#default`, `nix flake check`,
  `nix build .#lint` — all green.

## Acceptance

- [ ] Regression per confirmed hypothesis (H1) through the production
  reconnect path, red on origin/main, green on branch → Step 8 case 1 (+ Go
  tests 1–2 in Step 3); red evidence recorded from the `bin-main` run.
- [ ] Re-open test: restart + same-named session → live on the new server,
  no parked/disconnected badge, notice shown, no old pane content → Step 8
  case 2 (+ case 3 for the restore path).
- [ ] Restart + no same-named session → clean messaged end, never an
  indefinite park → Step 8 case 1.
- [ ] `docs/agents/bridge-daemon.md` "Bridge Reconnect" updated → Step 9.
- [ ] Local gate: `nix build .#default`, `nix flake check`, `nix build
  .#lint` → Step 10.
