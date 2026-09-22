# Bridge park: keep the mirror after the reconnect budget (#729)

## Problem

`reattach` (`picker/remotebridge/daemon/conn.go`) retries a bare-EOF drop on
`DefaultBackoff` (10 minutes, 40 attempts). When `bo.Next` says the schedule is
over it returns `nil`, `Run`'s attach loop breaks, and `teardown` runs
`kill-session` on the local mirror session. A laptop asleep or offline for more
than ten minutes therefore loses every mirror and its window layout.

## Goal

Budget exhaustion on a network outage stops dialing but keeps the mirror:
a **parked** mirror. Leaving `parked` is user-triggered, one short background
attempt cycle at a time, and never blocks a pane or client.

## Non-goals

- Surviving a *local* tmux server restart (tmux-remux).
- Picker changes.
- Changing what counts as terminal today (below), or the repair order.

## States

`@bridge_state` (session option on `cfg.LocalSess`, daemon-only writer) gains a
value:

| value          | meaning                                           | dialing? |
|----------------|---------------------------------------------------|----------|
| unset          | live                                              | —        |
| `disconnected` | a retry cycle is running (initial or wake)        | yes      |
| `parked`       | budget exhausted, waiting for the user            | no       |

Transitions:

1. live → `disconnected`: bare-EOF drop (unchanged).
2. `disconnected` → live: dial + identity match + `repair()` true (unchanged:
   cleared only after the reseed).
3. `disconnected` → `parked`: the cycle's schedule is exhausted (`bo.Next`
   returns false). Replaces today's teardown on this one edge.
4. `parked` → `disconnected`: a wake (focus or keypress, below). Starts one
   fresh cycle on a **short wake schedule** (`WakeBackoff`: Base 500ms,
   Ceiling 5s, MaxElapsed 30s, MaxAttempts 10).
5. `disconnected` (wake cycle) → `parked`: that short schedule is exhausted.
6. Any state → teardown, exactly as today, on: `Shutdown` (SIGTERM /
   `og-remote-detach`), identity mismatch or malformed identity reply,
   `repair()` false (emptied registry), `%exit` (never reaches reattach). New
   while parked: the local mirror session gone (#680) — the parked wait runs
   the same `sessionGoneTracker` observation on the session-lifetime
   `loopTick`, since parked is unbounded and runConn's tick is not running.

A wake cycle that exhausts its schedule re-parks; nothing bounds the number of
wake cycles, because each one is user-triggered.

## Parked wait

`reattach` becomes a loop of attempt cycles. The shared per-attempt body (dial,
post-dial stop check, identity read, identity compare, bind, repair) is unchanged
and yields one of: connected, terminal, retry. On cycle exhaustion, `reattach`
enters `park`, which:

- stamps `@bridge_state parked` and dims every mirror window (below);
- arms the waker (drains any stale poke first);
- blocks in one `select` on: the wake channel, `cfg.Shutdown`, the
  session-lifetime `loopTick` (local-session-gone check), and a park-local
  focus ticker (1s, `defer Stop()` — one per park, never leaked);
- on wake: disarms, stamps `disconnected`, returns to the cycle loop with
  `WakeBackoff`;
- on Shutdown / session gone: returns terminal. Session-gone sets the flag
  that makes `teardown` skip `kill-session` (as #680 does).

Park entry also withdraws the shipped remote agent state (`agents.clear()`, the
same call teardown makes): `waiting`/`error`/`denied` never fade on their own,
so an unbounded park would otherwise show a remote agent as needing input in the
global indicator indefinitely. `clear` forgets the shipper's `written` set, so
the repair's re-subscribe re-stamps every row on un-park with no extra reset.
The label/res shippers are left alone: `res` is already dropped at reattach
entry, and labels are static text that the re-subscribe refreshes.

After `reattach` returns a connection (initial or wake cycle), the attach loop
calls `replacer.cancel()` — a carousel/terminal-switch raise that landed while
parked or reconnecting is moot, since the dial that just succeeded already read
`View.Desired`; without it runConn would return `connReplace` immediately and
pay a redundant dial and repair.

The backoff measures elapsed time on Go's monotonic clock, which on Linux does
not advance during suspend: a sleeping laptop does not spend the budget, only
awake-and-offline time does. That is fine for parking (it only delays the park)
and is left as is.

No goroutine is spawned for park; the main goroutine blocks in the select, so a
parked daemon costs one stat per second (focus probe, below) and nothing else.
The control connection is already closed (`hold.close()` at reattach entry), so
the remote's per-window size clamp is released with it.

## Wake triggers

### Keypress (in-process)

`pumpInput` gains a poke: every `FrameInput` calls `cfg.InputSeen()` (a
`parkWaker.poke`), which is a non-blocking send on a 1-buffered channel and a
no-op unless the waker is armed (atomic flag). Armed only while parked, so the
live hot path pays one atomic load per frame.

The keystroke itself is **dropped**: while parked or mid-cycle the connHolder
slot is empty, so `hold.send` fails closed as it already does during an outage.
Nothing is queued or replayed. Keys typed after a wake cycle has published the
new connection reach the live remote as ordinary input — same as today's
reattach.

### Focus (existing session hooks)

The daemon already registers session-scoped hooks (`client-session-changed`,
`client-resized`, `window-resized`, `client-detached`) that touch
`<sock>.resize`. The parked wait reads that file's mtime on the focus ticker
(no fork while unchanged); on an advance it asks the local tmux whether any
client's `client_session` is `cfg.LocalSess`. A wake fires on the **edge** from
"no client viewing" to "a client viewing"; the viewing state is sampled at park
entry so a user already looking at the mirror when it parks is not re-woken by
reflow-driven `window-resized` touches (they press a key instead). Chosen over a
new ctl verb because it needs no new hook, no ctl-binary path inside the daemon,
and no fork in the hook.

This also covers re-opening a parked mirror from the picker or
`og-remote-open`: its `ping` is answered by `acceptConns` off the main
goroutine (so it works while parked and the launcher reuses the bridge), and
its `switch-client` onto the session is exactly the focus edge above.

## Dim

tmux-og sets both options globally (`config/tmux.conf.tmpl`):
`window-style "fg=#{@thm_fg},bg=#{@thm_mantle}"` (inactive panes) and
`window-active-style "fg=#{@thm_fg},bg=#{@thm_bg}"`. A per-window value
*replaces* the inherited one rather than merging, so the dim stamps a complete,
themed style on both options of every mirror window in the registry:

    set-option -w -t @N window-style        'fg=#{@thm_overlay_0},bg=#{@thm_mantle}'
    set-option -w -t @N window-active-style 'fg=#{@thm_overlay_0},bg=#{@thm_mantle}'

i.e. every pane of a parked mirror looks like an inactive pane, with its
default-foreground text (most shell output) in the theme's overlay colour.
Restored with `set-option -w -u` on both after a successful repair, over every
window then in the registry — unsetting falls back to the global theme value
exactly, and unsetting an unset option is harmless, so windows created or
retired by the repair need nothing special. Teardown kills the session, so it
needs no un-dim.

- The values are formats resolved against the theme's own `@thm_*`, so the dim
  follows the flavour; tmux caches a pane's resolved style until the option
  changes, so a flavour switch *while parked* keeps the old colours until
  un-park — cosmetic, and cleared by the `-u`.
- These are real tmux options on daemon-owned mirror windows, not remote-shipped
  values, so the "daemon writes `@bridge_*` only" rule (about carried remote
  state racing local writers) does not apply; the daemon already stamps
  `pane-base-index` on the same windows.

## Badge

`picker/statusline` distinguishes the values: `disconnected` keeps today's red
glyph; `parked` renders `<glyph> offline — press a key` in the theme's
overlay/peach colour so it reads as a waiting state, not an error in progress.

## Test seam

`cmd/daemon` gains `--retry-max-elapsed` (env `OG_DAEMON_RETRY_MAX_ELAPSED`)
and `--wake-max-elapsed`, test-only knobs so the bats suite can exhaust a budget
in seconds. Go tests inject `Config.Retry` / `Config.WakeRetry` as they already
do for `Retry`.

## Docs

`docs/agents/bridge-daemon.md` "Bridge Reconnect": the teardown-on-exhaustion
wording in the reattach/replaceConn bullet and the "Only a bare EOF is a drop"
bullet, the `@bridge_state` bullet (new value, badge), and a new bullet for the
parked state (wake triggers, dim, agent-state withdrawal, what stays terminal,
the socket-move test outage). `docs/agents/bridge-shipped-state.md` if it names
`@bridge_state` values.

## Acceptance tests

Offline bats (`tests/remote-m2-integration.bats`); the outage is made by
SIGKILLing the transport child **and** moving the SRC server's socket aside
(measured: dials then fail with ENOENT; moving it back restores the same server
pid, so identity matches):

1. Budget exhaustion → `@bridge_state parked`, session + windows + panes
   survive, `window-style` stamped on every mirror window.
2. Socket restored, `send-keys` into the mirror pane → cycle → state unset,
   `window-style` unset, content written on SRC during the outage painted.
3. Socket still away, key → `disconnected` observed, then back to `parked`;
   while the wake cycle is running the daemon stays responsive — a `ctl ping`
   against its socket succeeds, and the mirror pane is not dead
   (`#{pane_dead}` 0) — and a second key after re-park starts another cycle
   (the waker re-arms).
4. SIGTERM while parked → daemon exits promptly (<2s), socket/pidfile gone,
   session killed.
5. Parked, SRC server killed and restarted, key → identity mismatch → teardown.

Go unit tests: reattach cycle exhaustion parks rather than returning nil;
waker poke no-op when disarmed; stop while parked returns terminal; wake →
second cycle uses WakeRetry; focus edge logic (no wake when viewing at entry,
wake on false→true).
