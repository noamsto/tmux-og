# Bridge: Daemon Lifecycle and Mirror Invariants

Session pinning, reconnect, renderer/reseed/zoom/float rules, and what the remote host needs on PATH.

## Bridge Session Pinning

A control-mode client receives `%output` **only for the session it is currently
attached to**. A mirror pane's keystrokes reach the *remote* shell, where `$TMUX`
is set, so `sesh connect` — or any `switch-client` from a bridged shell — runs
`switch-client` with no `-c`, and tmux resolves "current client" to the daemon:
the only client the bridged session has. Every `%output` for the mirror's panes
stops from that instant, while input still lands and the remote command
completes, so the mirror reads as *frozen* rather than dead (#396).

`daemon/sessionpin.go` closes it:

- The mirrored session's id is read from the remote at startup, never learned
  from the first `%session-changed` — stream order is not evidence of which
  session the bridge is supposed to be on. A reply that isn't a `$N` id leaves
  pinning off rather than interpolating it into a command.
- A `%session-changed` naming any other id is an excursion: the daemon sends
  `switch-client -t '$N'` **with no `-c`** (a command sent over this stream
  resolves "current client" to the control client itself, which is exactly the
  one that was switched) and the stream resumes.
- **The reseed is not optional.** Output produced during the excursion is
  dropped by the server, not buffered, so the switch back alone leaves live
  panes showing a stale screen — the excursion typically swallows the very
  command that caused it and the prompt that followed. `capture-pane` restores
  the visible screen, not the scrollback.
- The session we were switched to is then handed to `og-remote-open <host>
  <sess>` (`Config.HandOff`, wired from `OG_DAEMON_REMOTE_OPEN`, which the
  launcher sets to `${BASH_SOURCE[0]}` so a hand-off re-enters the launcher at
  the daemon's own revision), which reuses a live bridge for that session or
  starts one and switches the local client to it. So `sesh connect` inside a
  mirror opens the target as a second mirror instead of no-opping.

Not done: rebuilding the mirror windows for the new session in place. That would
break the one-local-session ↔ one-remote-session invariant `@bridge_host`,
`@bridge_win` and `og-remote-detach` all assume.

## Bridge Reconnect

A control-connection drop no longer kills the mirror. `daemon.Run` is two
lifetimes, not one (#482): the ssh process, `ctlPump`, `stream`, round-trip and
async queue are rebuilt per attach behind a mutex-guarded `connHolder`
(`daemon/conn.go`), while the listener, pidfile, `@bridge_*` stamps, registry,
`ctlState`, resize watcher, renderer panes, their conns and the `Router`'s sinks
all survive. `send`, the round-trip and `waitHellos` are stable closures over
the holder, because `pumpInput` (started in `setupWindow` *and*
`applyPaneOps`), `watchResize` and the `acceptConns` ctl handler are each
started once and would otherwise hold a dead stream. An empty holder slot is a
normal state: sends fail closed through `stampAll`'s existing `ok == false`
path, which every caller already handles.

- **Two callers replace the control client, and only one of them is a
  reconnect.** `reattach` is involuntary: it runs off a connection drop,
  closes the dead one *before* it dials, sets `@bridge_state disconnected`
  for the outage, and — since #729 — parks rather than tearing the mirror
  down when it cannot re-dial (below); only a stop, a mismatched or malformed
  identity, or a `repair()` that empties the registry still runs
  `kill-session`. `replaceConn` (#574) is voluntary: it runs off the
  `prefix + I` carousel gesture wanting a fresher termname, dials, verifies
  and primes the new client *before* touching the old one, never sets the
  disconnected badge (the mirror is never actually down), and — unlike
  `reattach` — abandons the attempt with the old connection still live and
  published rather than risk the mirror over a nicety.
- **Only a bare EOF is a drop.** `%exit` is the remote deliberately ending the
  client and is terminal, as is an emptied registry and a raised stop.
  Measured: `detach-client` and `kill-server` both make the control client see
  `%exit`; only killing the transport process gives the bare EOF. That is why
  the offline reconnect tests SIGKILL the transport child rather than
  detaching it — a test built on `detach-client` asserts teardown and fails a
  correct daemon. The park tests (#729, `outage_start`) additionally make the
  outage itself by moving the SRC session's socket aside rather than killing
  its server (a dial then fails with ENOENT); moving it back restores the *same* server pid, so the identity
  check a re-dial runs still matches, and anything the test needs to write to
  SRC during the outage goes through `tmux -S <moved-path>` rather than the
  now-absent live path. An exhausted retry budget is no longer terminal on its
  own (#729): it parks instead (below), and only reaches teardown if the park
  wait itself answers false — `Shutdown` or the local mirror session gone.
- **The local mirror session is another ending** (#680). A session that is gone
  is none of the above — the registry's window ids are remote and all still
  there, and the control connection is healthy — so the orphan kept its control
  client, and with it the per-window size clamp it had asserted
  (`refresh-client -C`, released only when the client goes, #201), in force on
  the remote for days. `runConn`'s coarse tick now asks `has-session` about
  `cfg.LocalSess` (`localSessionGone`): only tmux's own exit status 1 counts —
  any other failure is a question that could not be asked, so a transient blip
  cannot tear a healthy mirror down — and two consecutive definite negatives are
  required, one affirmative answer clearing the count. The verdict is the
  existing `connEnd`, so `teardown` drops the control client and releases the
  clamp; on this path it also skips its own `kill-session`, since the name no
  longer belongs to this daemon. Renaming the mirror session counts as gone.
  `og-remote-open` already reaps a same-sock daemon whose session is gone when
  the host is reopened, so no launcher change accompanies this.
- **SIGTERM must be told apart from a link failure**, since it works by dropping
  the transport. `cmd/daemon/main.go` raises `Shutdown` *before* it touches the
  transport, and `reattach` consults it before scheduling any retry.
  `og-remote-detach` waits 2s before falling back to `kill-session` itself,
  so a reconnecting bridge that ignored this would be stranded.
- **Server identity is the correctness cliff.** Every attach reads
  `#{pid}|#{start_time}|#{session_id}` in one round-trip (folded into
  `newSessionPin`, so there is one authority for "which session are we on").
  `pid` + `session_id` are required and compared always; `start_time` is
  optional both ways, because tmux renders an unknown format as an empty field
  and the remote may predate it. A reply that arrives and mismatches or is
  malformed tears the mirror down — reconnecting into a rebooted server would
  mirror another machine's output into panes the user believes are their
  shells. A read that *EOFs* is a different thing: another drop, so it retries.
  The first attach records and never tears down; if it cannot, reconnect is
  disabled and the daemon stays single-shot.
- **Repair order is load-bearing and every error in it is silent**: reset the
  converger wholesale (it caches what *this* client told the remote, and
  `watchResize` records before it sends, so a resize during the outage left it
  believing a size the remote was never told) → re-send the client size and each
  window's cap → `resume()` every sink (a pane `%pause`d under the old client
  never gets its `%continue`, and a paused sink drops every frame forever, out
  of `reseedDropped`'s reach) → `reconcileWindows` → retire-or-`reconcileLayout`
  per survivor → one full reseed through the shared `reseedPanes` → re-subscribe
  both shippers (#566: subscriptions are per control client, so the fresh one
  carries none — and re-subscribing re-reports every window and pane, which is
  the label/agent-state half of the repair for free). `pause-after`
  deliberately stays where it is, re-armed by the main loop after the first
  `settle()`: a reattach *is* a setup pass, and arming it earlier re-opens the
  very window that leaves a pane paused with no `%continue`.
- **A local window that dies during the outage is retired by the repair pass,
  never mid-drop.** #487's retire-and-rebuild needs `reconcileWindows`, so it
  needs round-trips: a retire raised while disconnected would `closeWindow` and
  then fail to replace it, losing a window the remote still has. Nothing raises
  it there anyway — `reconcileLayout` runs only off the stream — so the verdict
  waits for the transport by construction, and the repair pass owns it. There it
  asks `localWindowGone` **outright**, where the live path asks only of a pass
  that already failed: an outage is the one stretch in which a local window can
  die with no `%layout-change` to discover it on, and a remote that never touches
  that window again would strand the entry for the life of the daemon.
- **The coarse main-loop tick is session-lifetime, not per attach.** It was cut
  for the label shipper, the one poller with no stream wake-up of its own; since
  #566 that shipper is subscribed and the tick clocks the maintenance sweep,
  `reseedDropped`/`reseedReshaped` instead — so `runConn`
  still selects on `loopTick` alongside the pump. Built once, before the attach
  loop: a ticker built per attach would leak one per reconnect, and teardown can
  only stop the handle it can see. The shippers' own rows survive a reattach
  untouched; what does not is the subscription, which is why repair re-sends it.
- **`@bridge_state`** is a session option the daemon alone writes:
  `disconnected` while a retry cycle is running — the initial drop or a wake —
  `parked` once that cycle's schedule is exhausted and the daemon is waiting on
  the user instead of dialing (#729), unset otherwise. Stamped before the
  first dial so the badge appears within a status tick, cleared only after the
  reseed — a stale screen the user knows is stale is a paused mirror; one they
  don't is a lie. `tmux-statusline` still renders `disconnected` in red beside
  `@bridge_host`; `parked` renders in the theme's overlay colour as "offline —
  press a key" so it reads as a waiting state rather than an error in
  progress, and a wake re-stamps `disconnected` for its own cycle before the
  next park (or a live connection) overwrites it.
- **Budget exhaustion parks the mirror instead of tearing it down** (#729).
  `reattach` is a loop of `attemptCycle`s on a shared dial/verify/repair body;
  on `cycleExhausted` it calls `park`, which stamps `@bridge_state parked`,
  dims every mirror window, and withdraws the remote's shipped agent state
  (`agents.clear()`, the same call `teardown` makes) so a `waiting`/`error`/
  `denied` icon does not stand in the global indicator forever — `clear`
  forgets what was written, so the repair's re-subscribe on un-park re-stamps
  every row for free. `@bridge_crew_*` and `@bridge_proc` are untouched by
  this: they are window/pane options `windowlabels.go`/`agentstatus.go`'s
  stamp path writes, not the agent-status files `clear` removes, so a parked
  mirror keeps showing its last-known role/proc labels frozen until the next
  reseed overwrites them. The dim is a per-window `window-style` and
  `window-active-style` (replacing the inherited global value, since a
  per-window value merges with nothing) painted in the theme's overlay-on-
  mantle colours, restored with `set-option -w -u` on both once `reattach`
  returns live again — unsetting an option a window never had (one the repair
  created or retired while parked) is harmless, so nothing needs filtering.
  `park` then blocks the main goroutine — no goroutine spawned — in one
  `select` on: a keypress (`pumpInput` calls `cfg.InputSeen`, a
  `parkWaker.poke` armed only while parked so the live hot path pays one
  atomic load per frame; the keystroke itself is dropped, not queued, since
  `hold.close()` already ran at reattach entry and `hold.send` fails closed on
  the empty slot same as any other outage), a focus edge (a 1s ticker rereads
  the resize-nudge file's mtime the existing session hooks already touch, and
  only the false→true transition on "is any local client's `client_session`
  this mirror" wakes it — a user already looking at it when it parks is not
  re-woken by reflow's own touches of that file), `cfg.Shutdown`, and the
  session-lifetime `loopTick`'s `sessionGoneTracker` observation (#680) —
  parked is unbounded and `runConn`'s own tick is not running, so the
  local-session-gone probe has to run here too. A wake restamps
  `disconnected` and retries on a short `WakeBackoff` (500ms/5s ceiling/30s
  budget/10 attempts) rather than the full retry schedule; exhausting that
  re-parks, uncapped, since each cycle is user-triggered. Only `Shutdown` or
  the session going away make `park` answer false and fall through to the
  same teardown exhaustion ran unconditionally before #729 — every other
  ending (mismatched/malformed identity, a `repair()` that empties the
  registry) is still reached from inside the next dial, never from parking
  itself. `cmd/daemon` exposes `--retry-max-elapsed`/`--wake-max-elapsed`
  (env `OG_DAEMON_RETRY_MAX_ELAPSED`/`OG_DAEMON_WAKE_MAX_ELAPSED`) purely so
  the bats suite can exhaust either budget in seconds instead of the
  production 10 minutes / 30s; those tests drive the outage itself by
  SIGKILLing the transport and moving SRC's socket aside (above).
- **The `ControlMaster` path is per-dial, not pid-derived-and-fixed** (#574),
  owned by the `child` that dialled it rather than captured in a closure: the
  graphics fetcher and the paste upload both read it through
  `transport.currentPath` — "the most recently started child that is still
  open" — at call time, so a replacement's fresh path reaches them with no
  capture to go stale. That retires the old hazard structurally rather than
  working around it: `ControlMaster=auto` meeting a stale socket *disables
  multiplexing* rather than replacing it, silently, but a fresh path per dial
  has nothing stale to meet. `ControlPersist=no` still unlinks a path only on
  a clean ssh exit, so `child.Close` unlinks its own child's path the instant
  it runs rather than waiting on that, and `cleanup` at process exit is the
  backstop for every path still tracked open — not one fixed path — for a
  child that never went through `Close` at all.

## What the Remote Host Needs on PATH

Each bridge feature that runs code on the *remote* names its own requirement, and
they are all satisfied the same way — a remote rebuilt from this revision, whose
`/etc/profiles/per-user/<user>/bin` is the only tmux-og PATH a non-interactive
`ssh` sees:

| Feature | Remote needs |
|---------|--------------|
| Bridge graphics (`prefix + I` across a mirror) | `tmux-claude-images`, `resvg` |
| Remote agent status | tmux-og's `claude-status-update` (`@claude_status`) for Claude, and `agent-detect` (`@agent_screen`, #635) for pi/codex/cursor — both stamp the pane options the daemon subscribes to |
| Remote window labels | tmux-og's own `tmux-reflow-windows` (what stamps `@window_label_*`) and, for a codename, whatever fan-out harness stamps `@crew_name`/`@crew_color` — plus its per-pane `@crew_role`/`@crew_state`/`@crew_role_color` for the role-grid borders. The one requirement with no capability probe: an older remote stamps nothing and the mirror silently falls back to the remote window name. |
| Remote session resources (the picker's CPU/Mem columns on a mirror row) | nothing new on PATH — the `@og-res-tick` monitor hook names `tmux-session-resources`' own store path; what it needs is a remote rebuilt from this revision, so the hook exists to arm it at all. No capability probe either: an older remote stamps nothing and the picker degrades silently to the ssh `ps` fallback (#693). |
| Remote agent usage (a mirror session's usage segment) | nothing new on PATH either — the `@og-usage-tick` monitor hook already names `tmux-agent-usage`'s store path; what it needs is a remote rebuilt from this revision, whose `--tick-run` also publishes `@og_agent_usage`. No capability probe: an older remote publishes nothing, the `og_usage` subscription reports an empty JSON half, and the mirror simply shows no usage segment — never local figures (`bridge-shipped-state.md`). |
| Cold start (`prefix + s` on a serverless host) | `tmux-startup.service` / the launchd agent, plus lingering |
| Remote-side picker (`prefix + s` `^o`) | `og-remote-picker` (`remote.exposePickOnPath`, default true) |
| Tool binds across a mirror (`prefix + p`/`g`/`y`) | whichever of `prdash`, `lazygit`, `yazi` you press — the bind sends a bare name, never this host's store path. A missing one opens a short-lived message pane instead of the tool. The remote leg opens a **float**, which the mirror renders as a local float, so the remote's tmux must know `new-pane -A` — a Z-ORDER flag (the float stays visible above a zoomed pane), not attach-if-exists. The reuse of an already-open float is the gate both legs build themselves (#679); a remote whose tmux-og predates it keeps stacking until it is rebuilt. |
| Theme fan-out (any light/dark toggle) | `theme-toggle` — it ships from the desktop profile, so a headless remote has none. The per-toggle apply stays silent (fire-and-forget, no exit status to see), but the daemon probes for it once per bridge connect and reports a miss via `display-message` (#545). |
| `[r]` in the enrich card across a mirror (`prefix + i`) | tmux-og's own `tmux-pr-enrich` — the `enrich-refresh` ctl verb runs its single-target `--force` mode on the remote, where the checkout is. Also `gh`, which that poller already needs. The verb resolves the remote window's `@branch` and `@worktree`/`@git_root` itself and exits silently unless **both** are set: an empty branch would fall through the poller's single-target guard into a whole-server pass, and an empty dir would run `gh` in the tmux server's cwd and write another repo's PR onto the remote window (#598). |
| Image paste into a mirror (`ctrl+v`) | nothing beyond POSIX `sh`/`mktemp`/`find` — the requirement is on the *local* host: `xclip` or `wl-paste` (without one the byte is forwarded, i.e. pre-#361 behaviour). |
| Popups on the remote (`display-popup` from a remote shell, older `fzf`, `fzf-tmux -p`, atuin, any popup bind of your own) | a remote rebuilt from this revision, for `patches/tmux-display-popup-control-client.patch` (#738). No capability probe: an unrebuilt remote opens nothing at all, silently — the same degradation shape as remote session resources. `new-pane`-based floats (fzf ≥ 0.74) are unaffected and work on any remote whose tmux has `new-pane`. |

`og-remote-picker` doubles as the picker's capability probe, so its absence
is the one requirement that reports itself: the asking side prints
`remote tmux-og too old — rebuild <host>` instead of degrading silently.


## Mirror invariants

- **A renderer's exit must not be structural.** `spawnRenderer` passes the sock path and the **remote** pane id as renderer arguments (`respawn-pane -k -t <local> -- <bin> <sock> <%N>`), so a bare `respawn-pane` (tmux's default Respawn binds: `prefix + <`, `prefix + >`, and both right-click pane menus) re-runs the same command and redials — pane environment does not survive that gesture, argv does. The daemon's main loop adopts that hello (`rebindRenderer`); `waitHellos` is not running, and the pane is live so heal would never see it. A genuine crash still exits, and with the host's `remain-on-exit off` the pane then closed, taking the window and, for a single-pane mirror, the whole mirror session with it (#547). `stampMirrorWindow` therefore still sets `remain-on-exit on` as a window option on every mirror window: a dying renderer leaves a corpse, never a lost session. The corpse is invisible to everything else — a dead pane is still a pane to `list-panes`, so the pane diff reads the local set as matching the remote's and reconcile correctly does nothing — so `healDeadRenderers` is what finds it, keyed on `#{pane_dead}` with `@bridge_pane` set (a dead float the daemon never created is the user's, not ours to reap, and a corpse's `pane_current_command` still reads `renderer`, so a command-name probe is blind to it). It repairs via `resetWindow`, not `retireMirror`: `closeWindow`'s `kill-window` on a live single-window mirror session destroys the session, which is the failure being repaired. Capped at `deadRendererStrikes` rebuilds per window (#657). The corpse is invisible to everything else *except* `select-layout`, which **counts a dead pane in the window's pane total** and refuses any layout that disagrees — `have 3 panes but need 2`, measured; floats, by contrast, are not counted, so the tiled-only `L.Raw` stays right for them. So while a corpse is present every reshape is refused, `applyLayout` returns `ok=false`, and the caller correctly suppresses the resize and the reseed — leaving the mirror on stale geometry while its live renderers go on painting the remote's current screen into it, which reads as a garbled window. Nothing recovers on the non-structural path: a pure reshape runs no `applyPaneOps`, so `errLocalPanesDesynced`'s count check never runs. `applyLayout`'s second return value closes that — a refusal it can attribute to a pane-count mismatch (read without disturbing `w.localPanes`, empty answer treated as no evidence) rebuilds through the same `resetWindow` the structural path uses (#672).

  `healDeadRenderers` is driven by the maintenance sweep (`windowSweeper.sweep`, gated by `windowSweepInterval`, run unconditionally every main-loop iteration) as a lower-cadence backstop, but the common case is event-driven (#657): `pumpInput` runs one goroutine per renderer connection and already returns the instant its `wire.ReadFrame` errors — exactly what a renderer's process exit does to its end of the unix socket, whether crash, clean exit, or the daemon's own deliberate close during a reconcile. That goroutine calls `cfg.RendererDied` (`death.wake`), which arms a `deathNudge` — a debounced `*time.Timer` shaped like `carouselProbe`, firing after `deathSweepDelay` (250ms, giving tmux's own `pane_dead` update time to land, since the socket EOF has no ordering guarantee against it). `runConn`'s `select` case then calls `windowSweeper.force()` (resets `lastPass` to zero) so the very next sweep pass actually runs instead of being silently floored by `windowSweepInterval` — a bare wake with no force can be swallowed by that floor, leaving the event path no faster than the sweep it's meant to shortcut. A renderer that never connects at all (no hello) has no `pumpInput` goroutine to EOF, so that death stays backstop-only. Deliberately **not** a `pane-died` tmux hook, despite one being the more obvious precedent (`registerResizeHook`'s touch/poll shape, #433): a session-scoped `set-hook -t <session> pane-died` was measured to entirely replace the session's view of the *global* `pane-died` array rather than add to it, which would silently shadow the global `pane-died` hook `tmux-reap-pane` already relies on (#647) and disable claude-status reaping inside every mirror session.

  Every successful heal is its own trap for the strike cap: `resetWindow` closes the superseded renderer conns, which is the same `died()` signal this feature wires up, so a heal always arms its own follow-up wake roughly `deathSweepDelay` later — by which time the just-rebuilt renderer is alive, and an unguarded "any healthy pass returns the budget" rule would read that as recovery rather than the rebuild's own echo. `deadRendererRecovery` (= `mainLoopTickInterval`, the sweep's own former cadence) gates the budget return: a healthy pass inside that window of the window's own last rebuild (`windowSweeper.lastRebuild`) does not clear its strikes. Without this, a renderer whose crash-to-crash lifetime sits between `deathSweepDelay` and `mainLoopTickInterval` — dies doing a little work, rather than instantly at spawn — would never accumulate strikes and get rebuilt forever, which is exactly the failure `deadRendererStrikes` exists to stop.
- **Every pane is re-seeded after a layout reshape, not just on a geometry-only change.** The renderer is a dumb painter with no back-buffer, so any pane whose dims moved must be repainted from `capture-pane` — and a pane that survived a close or a split has new dims for exactly the same reason one whose window merely resized does. Gating the re-seed on `!structural` left the survivor of a closed split showing the screen it was painted with at the old size (#417). The re-seed goes *after* `FitWindowCmd`/`select-layout`, never before: a seed sized for the new geometry painted into a pane still at the old size leaves the mirror blank (#233). `structural` still gates `focusLocalPane` — a pure resize must not yank local focus.
- **An output frame dropped to a full sink buffer is repaired by a re-seed**, not by the next `%output`. The drop stays deliberate — blocking the control-stream loop on one stalled renderer stalls every pane with it — but terminal output is positional, so a frame lost mid-repaint leaves those cells wrong until something overwrites them, which on an agent pane that just finished a turn can be the rest of the turn (#412). The sink counts drops; `reseedDropped` pushes `capture-pane` ground truth from the main loop (the only place a round-trip may run), and only once that pane has drained — re-seeding a congested pane is a whole extra screen on a queue already behind. A *paused* pane is exempt: its `%continue` already owes it a seed.
- **A command that runs further commands of its own takes a barrier.** The reply accounting (`stream.claim`) pairs the Nth client-flagged `%begin/%end` block with the Nth command written, but an `if-shell`'s branch runs as more commands of the *same* client and each guards a flagged block of its own — one command in, 1+N blocks out, N not even constant (a failing branch command aborts the rest of its list). The count then ran ahead for the rest of the connection: the `tool` verb's (#679) reconcile read the branch's empty block as its layout (`empty layout reply`), so the remote float never reached the mirror until a reattach reset the counters (#715). `stampAll` therefore writes a `display-message -p og-fanout-<n>` barrier behind **every** command, and `claim` gives no ordinal to the blocks between a command's own reply and its barrier's (recognised by body). The barrier was at first armed only for `if-shell`/`if`, keyed on the leading verb — but that is an enumeration, and the thing being enumerated is tmux's, not ours: measured on next-3.9, one control client, `run-shell -C` answers with **two** client-flagged blocks and matched no verb list this daemon had, while nothing anywhere compares `s.seen` against `s.sent`, so the next such verb would have reproduced #715 with no diagnostic naming the cause (#723). Arming it unconditionally is what removes the list: `claim`'s swallow window is not an N-block assumption — it consumes whatever arrives until the barrier — so an unforeseen fan-out is inert rather than silently poisoning the connection. The cost is one extra `display-message -p` per command, against a stream that carries `%output` in megabytes. A `run-shell -b` verb would need no barrier either way, since its script's own `tmux` calls answer a different client.
- **A batched round-trip must deliver each pane's result before reading the next pane's reply.** Control-mode replies come back in issue order, so `PaneSeeds` writes every command before reading any of them — one round-trip for a whole window instead of two per pane (#430). But `readReplyRouting` routes live `%output` into registered sinks *as it walks past reply blocks*, so reading all the replies and only then enqueueing the seeds would hand a pane its `FrameOutput` before the `FrameSeed` that predates it — a full repaint with stale content, the same defect class as #233/#412/#417. Hence the `replies` iterator rather than a returned slice: a caller cannot reach pane B's reply without an explicit `next()`, so the per-pane `onSeed` delivery is the shape of the code. Nothing inside that callback may issue a round-trip of its own — a nested one takes a later ordinal, and the reply reader discards the batch's remaining blocks hunting for it. A batch is bounded by one session's panes, orders below the transport buffer, so writing it without interleaved reads cannot wedge on backpressure.
- **Zoom crosses the bridge as a ctl verb, not a local `resize-pane -Z`.** A local zoom does grow the renderer pane, and it sticks — but it does not touch the remote pane, so the remote program keeps rendering at its old size and the rows it gained are dead space. Measured on a 150x40 two-pane mirror: local zoom gives `dst 150x39` against `src 150x19`; the `zoom` verb gives `150x39` on both. `prefix + z` therefore sends the verb, and the mirror learns the state from `#{window_layout}` and the zoom flag (the `Z` in `window_raw_flags`) carried in the `%layout-change` notification itself (`<window-id> <window_layout> <window_visible_layout> <window_raw_flags>`, `control-notify.c:79`), not a fresh `readLayout` round-trip: `reconcileLayoutFrom` gates the notification, and a line that reports nothing the mirror doesn't already reflect — a duplicate, an echo of the daemon's own verb, the trailing line of a push/pop-zoom bracket — returns with zero remote round-trips, while a geometry-only reshape (same panes, same floats, zoom flag off) is applied straight from the notification's layout string with none either — regardless of whether the window holds a mirrored float, since the tiled-only `select-layout` leaves every float where it is. A zoom transition and any pane-set or float change still fall through to `reconcileLayout`'s read-first entry, because `window_push_zoom`/`window_pop_zoom` bracket structural commands with transient unzoomed lines and a stale structural line applied verbatim would do renderer surgery on a pane the remote has already closed. The live dispatch path is left uncoalesced on purpose — the trailing re-read, not a coalesce pass, is what corrects a notification the remote has already moved past (#570). **tmux exposes zoom only as a toggle, so reconcile asserts the remote flag on the mirror with an idempotent `if -F` after `applyLayout` (which may unzoom via `select-layout`) and before FrameResize/reseed — never a bare toggle, and never a per-reconcile `display-message` of local state.** Zoom-on targets the tiled pane rendering `remoteActive` (skipped when that pane is a float, #517); unzoom targets the window. Last successfully asserted flag lives in `mirrorWindow.appliedZoom` for dedup against `readLayout`'s remote flag. The ctl `zoom` verb still owns the remote side. `#{window_layout}` stays the **unzoomed** geometry deliberately: `#{window_visible_layout}` reports a zoomed window as single-pane, and reconcile would read the hidden panes as closed and kill their renderers on every toggle. A zoom made by any other client on the remote follows too: tmux emits `%layout-change` for one, even though `#{window_layout}` itself is unchanged by it — which is why reconcile's trailing re-read compares the zoom flag alongside the layout string (#413).
- **A remote float is mirrored as a local float.** The daemon opts every control client into v2 (JSON) layouts by sending `refresh-client -f new-layouts` inside `readIdentity`, the round-trip that leads every attach path — first attach, `reattach`, `replaceConn`. Flags are per control client, so skipping it on any one of the three would silently lose float mirroring the moment that connection is replaced; without it the pinned remote's control client omits floats from `#{window_layout}`/`%layout-change` entirely. `ParseLayout`'s input is self-describing: a leading `{` parses as v2 JSON, where a float is an ordinary tree leaf carrying a `"z"` field at any depth, and anything else parses as v1, including the trailing `<WxH,X,Y,id...>` float section a next-3.8 remote predating the JSON format still sends; a current remote whose control client never got the flag reports tiled panes only, so its floats go unmirrored. `Layout.Raw` is always reconstructed as the v1 tiled-only string regardless of input format: on the pinned local server that is the one string `select-layout` accepts on a window holding floats, keeping every open float — the daemon's and the user's — exactly where it is; a v2 string would instead have to name every local pane cell for cell, floats included, or be rejected, and would also rewrite local focus, last-pane history and z-order — so the remote's JSON is never fed to the local `select-layout` verbatim. Two geometry spaces are in play and mixing them is the easy bug: a float's **layout cell is the inner box** and equals its usable pane size (so it feeds renderer dims unconverted), while `new-pane -x/-y/-X/-Y`, `resize-pane -x/-y`, `move-pane -X/-Y` and the `@float_geom` stamp all speak the **outer box** — inset 1 per side for every border style, 0 only for `-B none`. A float this daemon did not create (`prefix + b`/`k`/`i` are unguarded float binds) is never killed — it is not ours to reap. A rebuild (`resetWindow`) still drops and re-adds every mirrored float unconditionally, which is also what heals a dead float renderer. Degradation on a local mirror server that predates the pin and is still resident (#407): its `select-layout` still refuses a v1 tiled-only string while any float is open, so any reshape behind any float fails there and the mirror keeps its last-good screen until a remote layout change arrives with no local float open — closing a local float re-drives nothing. **Popups are floats now too (#725).** Until #738, `cmd_display_popup_exec` carried an overlay-era `CLIENT_CONTROL` bail (`cmd-display-menu.c` lines 416-417): `tc` is the *target* client, and for a shell running inside the session `cmd_find_best_client` picks it from the clients attached to that session — in a bridged session, only the daemon's control client — so a popup a **remote shell** asked for was silently refused (exit 0, no float, nothing logged) exactly like one the bridge itself would send, not just the latter. #738 drops the bail via `patches/tmux-display-popup-control-client.patch`, which also returns without setting `wait_item` for an item queued by a control client (its own command, or a command hook it fired): waiting there parks the very queue the bridge runs every command on, so a remote hook opening a popup off a daemon command would wedge the mirror. The cost is that `-E` does not order that queue — a following command in the same list runs at once and `after-display-popup` fires at open — while a command client (a shell's `tmux display-popup -E`) and the global queue still wait; fzf ≥ 0.74 dodged the bail by probing `list-commands new-pane` and preferring that over `display-popup`. A **modal** remote float still mirrors as an ordinary, non-modal local float; the daemon itself still never calls `display-popup` to open one. Since v1/v2 layout parsing already treats a float as an ordinary pane, no further special-casing is needed once the remote popup exists. A `display-popup` without `-E` leaves a dead modal float that `window_lost_pane` (`window.c:1156`/`1175`) never reaps, so `w->modal` stays set and every later popup in that window is a silent no-op. The mirror's own cancel key cannot clear it: the daemon delivers input as pane input (`pumpInput` → `send-keys -H`), while `PANE_CLOSEONCANCEL` (`server-client.c:1655-1661`) is a *client*-key rule a dead pane never sees. So `pumpInput`, on any frame that is exactly one lone Escape or `C-c`, also sends `if -F -t %N '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %N'` for that pane (#738). `-C`'s `server_kill_pane(w->modal)` branch runs *ahead of* the `CLIENT_CONTROL` bail, so it clears a wedged remote float on stock tmux too, not just patched; the guard is evaluated remotely and deliberately spares a live pane, whose cancel key belongs to the program inside it — a deliberate divergence from tmux's own `PANE_CLOSEONCANCEL`, which also kills a *live* no-`-E` popup on Escape/`C-c` for a normal client. This qualifies the "never killed" sentence above: that one is about **local** floats — this is the one daemon-initiated kill of a **remote** float, and only of a dead modal one a normal client's own cancel key would have closed. A local popup-float opened by a human inside a mirror window is, to the daemon, just another user float: `parseLocalPaneList` keeps it out of the tiled set and `removeFloat` never kills it, the same as any `prefix + b`/`k`/`i` float. Its modal flag also refuses `focusLocalPane`'s `select-pane` while it's open, same as the `prefix + s` `^o` remote-picker float.
- **A mirror window is addressed by tmux window ID**, never `<sess>:<index>`. `renumber-windows` is on, so closing one mirror window renumbers every window above it — an index captured at creation then silently addresses its neighbour, and every later rename/kill/layout command lands on the wrong window (#411). The daemon reads the id back at creation (`createMirrorWindow`, `-P -F '#{window_id}'`) and appends at `{end}`, leaving the re-indexing to tmux so a mirror never grows a gap a local session wouldn't have.
- **`@window_bridge_name`** — daemon-owned remote window name for a `@bridge_win` mirror window; read only by reflow's bridge branch (#196). The daemon stamps it *after* the `new-window` whose `after-new-window` hook already reflowed, and a remote rename changes no window count (so reflow's `count:width` cache would skip it) — hence the daemon forces its own `tmux-reflow-windows --force <local-sess>` after every path that stamps a name (`--reflow` / `OG_DAEMON_REFLOW`, resolved by the launcher). Without it a mirror window keeps a label built from the launcher's cwd.
- **`@bridge_host`** — session-scoped ssh host the mirror session's windows really live on, stamped by `og-remote-open`. `tmux-statusline` renders it after the session pill on `@bridge_win` windows, so a mirror is never read as local.
- **`@bridge_session_path`** — session-scoped remote `#{session_path}`, stamped once per bridge by the daemon. The mirror session's own `session_path` is the launcher's cwd (`og-remote-open` passes no `-c`), so the session picker's Path column reads this instead on a `@bridge_host` row, and renders nothing when it is unset.
- **`get-clipboard request`** (`set -s`, next to `set-clipboard on`, #694): tmux answers an app's OSC 52 *read* per `get-clipboard`, not `set-clipboard`. The default `buffer` replies synchronously from the server's own newest paste buffer, so on a mirrored host the query never crosses the bridge and the remote app pastes stale remote content. With `request`, a server whose only clients are control-mode stays silent, the query propagates through the bridge, and the local tmux asks the local terminal. Tradeoff: on a terminal with no OSC 52 read support an app's read now returns nothing instead of tmux's newest buffer — tmux has no fallback. Asserted in `float-conf-assertions`.
