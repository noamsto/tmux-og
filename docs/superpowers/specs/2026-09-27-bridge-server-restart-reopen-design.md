# Bridge: recover a parked mirror across a remote server restart (#817)

## Problem

A mirror parked by a long outage (#729) never recovers once the remote tmux
server has been restarted during that outage, even when the host is reachable
again.

### Reproduction (origin/main, `--test-local` harness)

Mirror `rem`, make the outage (`outage_start`: SIGKILL the transport, move
SRC's socket aside), wait for `parked`, `kill-server` the old SRC through the
moved socket, start a fresh SRC server on the original path, then press keys
into the mirror for 10s:

| New server has | Result on origin/main |
|---|---|
| another session only (`other`) | **parked forever**: every wake dials, logs `identity read for rem: connection closed before reply`, exhausts the wake cycle, re-parks (3 wake/park rounds in 10s, daemon alive, `@bridge_state` cycling) |
| no session at all | **parked forever**, same log shape |
| a same-named `rem` | first wake logs `now hosts rem on a different tmux server …; tearing the mirror down`, daemon exits, local session killed |

- **Hypothesis 1 — confirmed.** Measured what a refused attach looks like on
  the wire (`tmux -C attach-session -t rem`, tmux next-3.9): with no such
  session, or no server, tmux answers with one unflagged reply block and ends
  the client:

  ```
  %begin 1790528595 1400 0
  can't find session: rem        (or: no sessions)
  %error 1790528595 1400 0
  %exit
  ```

  then EOF, exit 1. The flags field `0` marks it as the reply to a command the
  control client did not send — the attach itself. `handleAsideLine` drops
  every reply block nobody waits for and `readIdentity` then sees EOF, so the
  cycle classifies a **definite answer from a reachable remote** as "another
  drop, retry". Worse, when tmux has already exited before `readIdentity`
  writes, `stampAll` fails on EPIPE and the lines are never read at all. The
  mismatch/teardown path never runs because no identity reply ever arrives.
- **Hypothesis 2 — refuted as a cause of the stuck park.** A same-named
  session on a new server answers the identity read with a new pid, and the
  wake path reaches the mismatch teardown (existing bats case "a keypress that
  wakes a parked mirror into a different tmux server tears the mirror down"
  passes). It ends the mirror instead of recovering it, which is the behaviour
  this change replaces.
- **Hypothesis 3 — key delivery refuted as a cause.** Keys pressed into a
  parked, dimmed mirror reach `pumpInput` and wake it (`waking on input`
  logged per press round above, and the existing wake bats cases). Whether a
  parked mirror should also re-probe on its own is decided below (D4).

## Goal

A parked or reconnecting mirror whose remote server was replaced recovers:

- same-named session on the new server → the old mirror is torn down and a
  **fresh mirror** is built onto the new server, in the same local session,
  with a visible "fresh server" notice;
- no such session → wait a bounded window for it (tmux-remux restore), then
  end cleanly with a message — never park forever.

## Invariants (from the task, restated as design constraints)

1. A server whose identity differs from the pinned one is never attached to
   the old mirror's router, registry or renderer panes. Re-open builds from
   scratch; the identity check is not relaxed.
2. Mismatched or malformed identity still tears the old mirror down. Only the
   step *after* teardown changes for a mismatch (re-open); malformed keeps its
   current ending.
3. Re-open targets the same remote session name only, and nothing here ever
   creates a remote session.
4. SIGTERM/`og-remote-detach`, `%exit`, and local-session-gone (#680) keep
   their meaning; none of them re-opens.
5. Existing park/wake/reconnect behaviour for an outage that clears onto the
   same server is unchanged.

## Design

### D1. Classify a refused attach (fixes hypothesis 1)

A fresh control connection's **first reply block** is the reply to the
`attach-session` the transport ran. If that block is an unflagged `%error`,
the remote tmux answered and refused: the pinned session does not exist on
whatever server now owns that socket. That is a *verdict*, not a drop.

- `newCtlConn` wraps the pump the unverified round-tripper reads in a small
  recorder (`attachWatch`) that notes the first reply block it sees and keeps
  its body when it is a flag-0 `Error`. `bind` goes on using the pump
  directly, so the recorder only ever sees pre-verification traffic.
- When `readIdentity` fails with the retry shape on a connection the deadline
  did **not** close, `attemptCycle` drains the rest of that connection through
  the same recorder, bounded by a fresh identity-deadline timer (the pipe has
  usually hit EOF already, so this returns at once), and asks whether the
  attach was refused. This covers both the EOF path (lines already read by
  `readIdentity`) and the EPIPE path (lines never read).
- Only the first block counts. A hook error or anything later on a connection
  that attached successfully is not a refusal. A misclassification is still
  benign: the restore cycle below keeps dialling and accepts a matching
  identity, so the only way a false refusal ends a mirror is by recurring for
  the whole restore window.
- ssh failures, a missing `tmux` binary, and a silent far end produce no
  stdout block and stay "unreachable — retry" exactly as today. That — no
  control block at all, then EOF — is the production network-loss shape.
- A reachable host whose tmux server is not running *is* refused: the
  attach's `CMD_STARTSERVER` starts a server that answers `no sessions`
  (measured). That is correct — the pinned server is gone either way.

#### The offline harness has to change its outage model

The park bats cases make the outage by moving SRC's socket aside. That is not
"unreachable" to tmux: `tmux -L m2src -C attach-session -t rem` finds no
socket, starts a fresh server on the vacated path and answers `no sessions`
(measured on a scratch socket). Under D1 every existing park case would read
as a refusal. The harness therefore models an outage the way production sees
one — a dial with no control output:

- `cmd/daemon` gains a test-only `--test-outage-file` (env
  `OG_DAEMON_TEST_OUTAGE_FILE`), honoured only under `--test-local`: while
  that file exists, the test-local dial runs `false` instead of
  `tmux -C attach-session` (stdout EOF, no block, exit 1).
- `outage_start` touches the file in addition to what it does today;
  `outage_end` removes it before moving the socket back. The socket move stays
  so every existing `tmux -S "$src_sock.away"` write keeps working. No
  existing assertion changes; setup exports the variable for every case.
- A server restart in the new cases is `tmux -S "$src_sock.away" kill-server`,
  drop the moved path, start the fresh SRC on the original path, then remove
  the outage file.
- The existing park cases (exhaust → park, wake → reconnect, wake into a
  still-down outage → re-park) are then the regression pinning that a dial
  with no control block still parks.

### D2. Restore window for a refused attach

`attemptCycle` gains a fourth and fifth result:

| result | meaning | `reattach` does |
|---|---|---|
| `cycleConnected` | identity matched, repair kept the mirror | return live (unchanged) |
| `cycleTerminal` | stop, malformed identity, emptied registry | end (unchanged) |
| `cycleExhausted` | unreachable for the whole schedule | park (unchanged) |
| `cycleRefused` *(new)* | the attach was refused | start/finish the restore window |
| `cycleReplaced` *(new)* | identity mismatch | end with "re-open" |

- The first refusal in any cycle (initial drop, wake, probe) ends that cycle
  with `cycleRefused`. `reattach` stamps `disconnected` and runs **one restore
  cycle** on `RestoreBackoff` (Base 1s, Ceiling 5s, MaxElapsed 60s,
  MaxAttempts 30 as the type's backstop). Inside it a refusal is retried
  rather than returned.
- The restore cycle ends: matched → live (the old server came back, e.g. the
  refusal was transient); mismatch → re-open (the session was restored onto
  a new server — the tmux-remux case); stop/malformed → as today; exhausted
  with the **last** attempt refused → end with the "session gone" message;
  exhausted with the last attempt unreachable (the host went away again
  mid-window) → park as normal.
- Why 60s and not "end at once": with tmux-remux `restoreMode = auto` a
  restarted server restores its sessions moments after it starts — started
  by `tmux-startup.service` at boot, or by our own attach's
  `CMD_STARTSERVER` — and a host is reachable over ssh before that restore has
  finished. With `restoreMode = off` (the default, `persist.md`) nothing
  brings the session back and the window only delays the ending by a minute.
  Why bounded at all: the task forbids parking forever, and a session that is
  not back within a minute is not coming back on its own; the picker reopens
  it in one gesture when it is.

### D3. Re-open onto a replaced server (daemon-owned)

`reattach` now returns `(conn, ending)`, where the ending distinguishes a
plain teardown, "session gone" and "server replaced". `Run`'s attach loop
records the ending; teardown then diverges only at its last step:

- **plain** (every existing ending): `kill-session`, as today.
- **replaced**: everything teardown already does to the old mirror (sinks
  unregistered, renderer conns closed, agent/label/res/usage state withdrawn,
  listener/socket/pidfile removed, resize hook unregistered, watchers stopped
  via `stopWatch`, phase cleared), then — instead of `kill-session` — reset
  the local session to one pristine window (`resetMirrorSession`): create a
  placeholder window (`new-window -d … -- sleep 2147483647`: no shell, nothing
  on screen) and `kill-window` every other window by id. The old mirror
  windows, their panes, floats, dim styles and window options go with them,
  so no old pane content or option can reach the new mirror. A viewing client
  stays on the session and lands on the placeholder.
- **gone**: the same reset, but the one window left is a **tombstone** (D5)
  instead of a placeholder, and the daemon exits.

`Run` becomes a thin loop over the renamed body (`runMirror`): an
`errServerReplaced` return rebuilds the mirror by calling `runMirror` again
with `reopened` set on its Config copy — the same code path a first open
takes: fresh router, registry, converger, shippers, listener, renderers,
connection, and a first-attach identity record. The old connection that
answered the mismatch was never bound (it round-tripped against an empty
router) and is closed. `firstMirrorWindow` finds the placeholder as the
session's only window, and `setupWindow` respawns it into the new first
renderer, exactly as it respawns the launcher's loading pane on a first open.
The re-open therefore costs a second dial — deliberately: reusing the
connection that answered the mismatch would mean handing a verified-foreign
stream to a half-built mirror, which is the splice the invariant forbids.

What crosses the boundary between the two runs, and why each is safe:

| shared | why it is fine |
|---|---|
| `cfg.View` | describes the local viewer, not the remote. The first dial of every run now publishes `View.setAdvertised(term)` for the term it dialled (the reattach rule); before this, only `Seed` wrote it, which is right for a first open but would leave a reopened run's raise guard comparing against the old connection's term. |
| `cfg.Shutdown`, `cmd/daemon`'s transport tracker | a SIGTERM during or between runs still closes `Shutdown` first and kills whatever child is current; `Run` checks `Shutdown` before starting another run. |
| `cfg.NewGraphics`, `PasteUpload`, `HandOff` | stateless builders; the ControlPath is read through `tr.currentPath` per call. |
| the socket path | the old listener is closed and the path removed before the new run listens. Renderers dial once at startup and never re-dial (`cmd/renderer`), and every old renderer pane is killed with its window before the new listener exists, so no stale renderer can hello into the new registry (whose remote pane ids restart at `%0` and would otherwise match by id in `rebindRenderer`). |

Every goroutine of the old run ends with its teardown: `acceptConns` on the
listener close, `watchLocalClient`/`watchReveal` on `stopWatch`, each
`pumpInput` and sink pump on its conn/sink close, each `ctlPump` on its
transport's EOF. The old run's `cfg` copy (with its `SendCtl`,
`RendererDied`, `InputSeen` stamps) is not reused; `runMirror` stamps its own.

A reopened `runMirror` that fails before the mirror stands (dial, identity
timeout, `list-windows` failure, `has no windows`) leaves only the
placeholder; `Run` turns it into a tombstone (D5) — a clean end rather than an
orphaned blank session. A stop raised between the two runs `kill-session`s
instead, which is what `og-remote-detach` expects.

`replaceConn` (the voluntary carousel swap) is unchanged: it still abandons
on a mismatch and leaves the verdict to the drop that follows.

Why daemon-owned rather than `og-remote-open`: the launcher kills and
recreates the local session (bouncing the viewing client off it), would
cold-start or create on the remote in paths this must never take
(`start_remote_server`, `OG_REMOTE_NEW_DIR`), and has no `--test-local` seam.
The in-process rebuild keeps the client on its session and reuses exactly the
first-open code path.

### D4. The notices outlive the moment

Both user-facing outcomes typically happen with nobody looking — the probe
(D6) makes the background the common path — so neither may rely on a client
being attached at that instant.

- **Fresh server (re-open).** A reopened `runMirror`, once its mirror stands,
  logs the notice and starts one notice goroutine bound to `stopWatch`: if a
  client is viewing the mirror now it shows the notice at once; otherwise it
  waits for the first viewing edge using the park's own `focusEdge`
  (resize-nudge mtime stat per second, `localViewing` only on an advance),
  plus a direct `localViewing` check every 15s as the backstop for an attach
  that touches no nudge hook, and shows it then. Shown once via `notifyLocal` (5s `display-message` on the
  viewing client), then the goroutine ends. Its lifetime is bounded by the
  mirror's: teardown closes `stopWatch`. Text:
  `<host>: tmux server restarted — now mirroring a fresh <sess>`.
- **Session gone.** Covered by the tombstone (D5), which is persistent by
  construction.

### D5. Tombstone: what a gone mirror leaves behind

`resetMirrorSession` with a notice pane instead of the placeholder:

- the pane runs `sh -c 'printf "%s\n\n%s\n" "$1" "press Enter to close"; read -r _' sh <text>`
  — the text is an argv element, never interpolated into the script, and is
  built from `RemoteHost`/`RemoteSession` with every control byte stripped
  (these are remote-derived and reach a local pty). tmux's refusal body is
  logged, never printed to the pane. Text: `<host>: session <sess> no longer
  exists (the tmux server was restarted or the session was closed) — this
  mirror is closed`.
- the window gets `remain-on-exit off`, so Enter closes it and with it the
  session;
- `@bridge_sock` and `@bridge_state` are unset (no daemon answers, nothing is
  dialling); `@bridge_host`/`@bridge_session` stay, so `og-remote-open`'s
  pair lookup recognises the session as this mirror and replaces it when the
  user reopens (its `kill-session` of a stale mirror), instead of walking to a
  `-remote` suffix;
- a client viewing the mirror lands on the tombstone at once; a user who comes
  back hours later finds the session in the picker and the explanation inside
  it. The daemon has already exited (socket/pidfile gone).

### D6. Parked mirrors re-probe on their own

Decision: **yes**, slowly. The owner expects recovery once the host is
reachable, and a user already looking at a parked mirror gets no focus edge.
`park`'s select gains a probe ticker (`parkProbeInterval`, 2 minutes).
`park` now returns one of `parkStop` / `parkWoken` / `parkProbe`:

- `parkProbe` runs one cycle on a single-attempt schedule (`MaxAttempts 1`,
  no delay) and does **not** restamp `disconnected` — the badge stays
  `offline — press a key` unless the probe connects, is refused (→ restore
  window, badge `disconnected`) or finds a new server (→ re-open). An
  exhausted probe re-parks; a re-park straight after a probe skips the log
  line and the re-dim (the windows are still dimmed, the badge still
  `parked`).
- Cost: one dial per 2 minutes per parked mirror. Offline, ssh fails
  immediately; a black-holed host costs at most one ssh process for the 30s
  identity deadline. Suspend does not tick. Keys and focus still wake at once
  with the full wake cycle.

### Test seams

`cmd/daemon` (all test-only, like the existing `--retry-max-elapsed`):
`--restore-max-elapsed` (`OG_DAEMON_RESTORE_MAX_ELAPSED`),
`--park-probe-interval` (`OG_DAEMON_PARK_PROBE_INTERVAL`),
`--test-outage-file` (`OG_DAEMON_TEST_OUTAGE_FILE`, `--test-local` only).
`Config` gains `RestoreRetry *Backoff` and `ParkProbe time.Duration`
(0 = default).

## Tests

Bats (`tests/remote-m2-integration.bats`, production binary, offline harness;
server restart as in D1's harness note):

1. **H1 regression — refused, never restored.** Parked, server restarted with
   only `other`, wake → the daemon logs the refusal, exits within the (shrunk)
   restore window, socket/pidfile gone, and `host-sess` is a one-window
   tombstone whose pane shows the gone text; Enter closes the session. Red on
   origin/main: parks forever.
2. **Re-open.** Parked with a marker on the old SRC pane; server restarted
   with a same-named `rem` carrying a different marker; wake → live on the
   new server: new marker painted, `@bridge_state` unset, windows not dimmed,
   daemon running on a new transport, the old marker nowhere in any mirror
   pane. A pty client (`m2obs`) attached to the mirror only *after* the
   re-open shows the notice text on its screen (the persist-until-seen half).
   Red on origin/main: teardown.
3. **Refused, then restored** — as 1, but `rem` is created on the new server
   inside the restore window → re-open (the tmux-remux case).
4. **Probe** — parked, outage ended onto the same server, no key pressed,
   shrunk probe interval → reconnects on its own and undims. Red on
   origin/main.
5. Existing: "a control-connection drop into a different tmux server tears
   the mirror down" and "a keypress that wakes … different tmux server tears
   the mirror down" assert the old ending; they are rewritten to assert the
   re-open (2 absorbs the latter). Every other park/wake/reconnect case keeps
   its assertions; only the outage helpers change (D1).

Go (`reattach_test.go`, scripted connections):

- a refused attach (first block flag-0 `%error`, then EOF) returns
  `cycleRefused`, both when the identity read consumed those lines and when
  its write failed first; a flag-0 `%end` followed by a later flag-0 `%error`
  is not a refusal; no block + EOF stays `cycleExhausted`;
- restore cycle: refusals retried; match → live; mismatch → replaced;
  exhausted with the last attempt refused → gone; exhausted unreachable →
  park;
- mismatch ending is `replaced`, malformed stays plain, and the mismatched
  connection's output never reaches a registered sink (existing
  trust-boundary test, adapted);
- probe: `parkProbe` runs exactly one attempt and does not stamp
  `disconnected`;
- tombstone text: control bytes stripped from host/session.

## Docs

`docs/agents/bridge-daemon.md` "Bridge Reconnect": the reattach/replaceConn
bullet (endings), "Server identity is the correctness cliff" (refusal is a
verdict; mismatch re-opens; never spliced), the park bullet (probe, restore
window, `park` verdicts), the `@bridge_state` bullet (probe keeps
`parked`), and the outage model the park tests use (the socket move alone is
a refusal, not an outage). One line on the tombstone/placeholder windows:
they carry no `@bridge_win`, so local scripts treat them as ordinary windows,
and a tmux-remux save can resurrect a tombstone after a local restart — which
`og-remote-open` already discards as a ghost on the next open.

## Out of scope

- Surviving a *local* tmux server restart.
- Statusline/picker changes (no new `@bridge_state` value).
- `replaceConn`'s mismatch handling.
