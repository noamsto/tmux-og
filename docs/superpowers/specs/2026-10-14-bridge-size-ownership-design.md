# Bridge size ownership: the last-active controller sizes a shared mirror (#979)

## Problem

Two controllers (two machines, each running a bridge daemon) mirror the same
remote tmux session. Each daemon's control client asserts per-window caps
(`refresh-client -C @N:WxH`, `ConvergeCmd`) at its own local size. The remote
windows end up at the **smaller** controller's size and stay there, even while
the user works only on the larger one. The active machine then shows a small
window inside a big terminal.

Observed live on `halo` (2026-10-14): two control clients on session
`dispatcher`, `client-598182` (laptop, 138 cols, idle/asleep) and
`client-749583` (this Mac, 156x97). All three windows were 138x38. After
`detach-client -t client-598182`, the window the Mac had current (`@9`) went to
156x97 at once; the two non-current windows (`@11`, `@15`) stayed 138x38.

## Mechanism (tmux source at the pinned rev, plus a scratch-server repro)

`resize.c` `clients_calculate_size`:

1. Under `window-size latest` with more than one sizing client, the first loop
   takes only `w->latest`'s size (its per-window size if set, else its tty size).
2. The `skip:` loop then clamps the result by **every** non-ignored client's
   per-window size (`CLIENT_WINDOWSIZECHANGED` + `control_get_window_size`), with
   no regard to `latest`. So the window is `min(latest's size, every cap)`: the
   smallest controller always wins. Working hypothesis confirmed.
3. `ignore_client_size` drops a client from both loops (and from
   `clients_with_window`) when it has `CLIENT_IGNORESIZE` **and** some attached,
   unflagged client exists anywhere on the server. With every client flagged,
   all of them count again.

`cmd-refresh-client.c`: `-t` takes any target client (`CMD_CLIENT_TFLAG`), so a
control client can run `refresh-client -t <other> -f ignore-size` (and
`-f !ignore-size` on itself). `server_client_set_flags` does **not** recalculate
sizes; every `refresh-client -C …` form ends in `recalculate_sizes_now(1)`,
which recalculates and resizes every window on the server immediately.
`refresh-client -t <other> -C @N:` (no size) also exists and clears another
client's cap, but it does not remove that client's tty size, which still wins
the first loop whenever it is `w->latest`; `w->latest` moves only on a client's
own key input/resize, `select-window`, `new-window`, `break-pane` and spawn
(`server-client.c` `server_client_update_latest`, `cmd-select-window.c:146`),
never on another client's `send-keys`. So clearing caps alone is not enough;
`ignore-size` is the lever.

Second bug (non-current windows): `recalculate_size(w, now=0)` only sets
`WINDOW_RESIZE`; `server_client_check_window_resize` applies it only for a
window that is the **current** window of some attached session. A client going
away (`server_client_lost` → `recalculate_sizes()`, `now=0`) therefore resizes
only the current window. The daemon never selects remote windows, so visiting a
mirror locally does not apply the pending resize; it waits until any client
next sends a `refresh-client -C` (`now=1`), and `watchLocalClient` re-sends a
cap only when the local size changes. That is the `@11`/`@15` observation.

The alternative hypothesis — the converger takes its size from the local mirror
window (a fixed point) — is refuted: `LocalArea` (`cmd/daemon/main.go`
`localArea`) reads the local *client* (`clientArea`, else
`latestClientArea`); only the nothing-attached fallback reads the mirror's own
size, and that assert is a deliberate no-op (#532).

Scratch-server repro (`tmux -L probe`, own `TMUX_TMPDIR` and
`CLAUDE_STATUS_DIR`, two `tmux -C attach` clients over fifos; A caps 100x30,
B caps 150x40 and `select-window`s last so B is `w->latest`):

```
1) both capped, B selected @0 last (B is w->latest):
  @0 100x30 active=1
  @1 100x30 active=0
2) detach A (the observed live fix):
  @0 150x40 active=1
  @1 100x30 active=0
3) B re-sends an unchanged cap for @0 only (recalculate_sizes_now(1)):
  @0 150x40 active=1
  @1 150x40 active=0
```

and with the lever:

```
2) B flags A ignore-size, then re-asserts its own caps:
  @0 100x30  (flag alone: no recalculation)
  @0 150x40 / @1 150x40  (after B's refresh-client -C)
3) A (flagged, its user idle) re-asserts its caps on wake:
  @0 150x40 / @1 150x40  (ignored)
4) A's user returns: A unflags itself, flags B, re-asserts:
  @0 100x30 / @1 100x30
```

Also verified on the same server: a control client's
Also verified on the same server:

- a control client's `display-message -p '#{client_name}|#{@og_bridge_owner}'`
  answers its own name and the session option;
- a session-scoped subscription `refresh-client -B
  'og_self::#{client_name}|#{client_flags}|#{@og_bridge_owner}'` reports once on
  subscribe and again whenever **any** field moves, including this client's own
  `ignore-size` flag being set by another client:
  ```
  %subscription-changed og_self $0 - - - : client-90114|attached,focused,control-mode,UTF-8|
  %subscription-changed og_self $0 - - - : client-90114|attached,focused,control-mode,ignore-size,UTF-8|
  ```
- `%client-detached <name>` and `%client-session-changed <name> $N <session>`
  (the latter also on another client's attach) reach every other control client.

Third mechanism (a client moved onto a session it never mirrored, reported by
the dispatcher from `halo`): with `detach-on-destroy off`,
`server_destroy_session` moves every client of the destroyed session to another
one through `server_client_set_session`, which sets
`s->curw->window->latest = c` and recalculates. A suspended controller's
control client, moved this way, becomes `w->latest` of the new session's current
window, so its whole-client size (`ClientSizeCmd`, 138x38) wins that window's
first loop and the active controller's cap can only shrink it further. Its
daemon is asleep, so the #934 pin handling (`sessionPin.apply`) never runs. It
has no per-window caps on the new session's windows (its caps name the destroyed
session's window ids), so the current window is the one it pulls down; it keeps
`latest` until some other client takes it, which a bridge never does (bridges
type through `send-keys` and never `select-window` on the remote).

## Ownership rule

Per remote session, one bridge daemon is the **owner**. Its control client stays
unflagged; every other control client attached to that session carries
`ignore-size`, so only the owner's caps (plus any human tty client's size, as
today) size the windows.

- **Owner record.** Remote session option `@og_bridge_owner` on the pinned
  session (targeted by id, `$N`), value `<token>|<client_name>`. `token` is 16
  random hex chars minted once per daemon process, stable across that daemon's
  reconnects; `client_name` is its current control client, rewritten by every
  claim, and is what lets survivors notice the owner's client going away.
- **Self-report.** Every connection subscribes `og_self` (session-scoped) to
  `#{client_name}|#{client_flags}|#{@og_bridge_owner}`. Each report is the
  daemon's observed truth: its own client name, whether its client is flagged,
  and who owns.
- **Reconcile (level-triggered, on every `og_self` report):**
  - owner is this daemon's token and this client is flagged →
    `refresh-client -f !ignore-size` + recalculation trigger;
  - owner is another token and this client is unflagged →
    `refresh-client -f ignore-size` + recalculation trigger;
  - owner unset and this client flagged → `refresh-client -f !ignore-size` +
    recalculation trigger (the owner released the session, below);
  - otherwise nothing.

  Because it compares observed state, not a remembered "applied" bit, a wrong
  flag set by anyone (a lost race, a stale enforcement pass) heals at the next
  report (~1s). Reports are queued and only the **newest** is reconciled per
  main-loop pass: a daemon waking from suspend replays one report per change it
  slept through, and reconciling a stale one (owner unset, from before a newer
  claim) would unflag it for a beat with nobody at the keyboard.
- **Enforce** (owner only): one round-trip
  `list-clients -t $N -F '#{client_name}|#{client_control_mode}|#{client_flags}'`,
  then `refresh-client -t '<name>' -f ignore-size` for every **control-mode**
  client on the session that is not this daemon's own and not already flagged,
  then the recalculation trigger if anything was flagged. The record rides the
  same read batch, and a read naming another token sends nothing: the cached
  owner belief that gated the call can lag a foreign claim by one subscription
  tick. Human tty clients
  (`client_control_mode` 0) are never flagged, so #201's clamp story is
  unchanged. The same reply seeds the owner's **membership** set: the names of
  every other client on the pinned session, which the membership rows below need.
- **Release** (driven by the survivors, never by the departing owner): on
  `%client-detached <name>` where `<name>` is the recorded owner's client and
  the token is not ours, hold a release for 30s (`ownerReleaseGrace`, longer
  than a warm reattach), staying flagged meanwhile; a newer report or read
  showing a different record drops it. Releasing at once made every network
  blip on the active controller unflag the idle survivors and resize every
  window to their size and back. When due, re-read the record and the
  session's clients and send one atomic
  `if-shell -F -t '$N' '#{==:#{@og_bridge_owner},<token>|<name>}' 'set-option
  -u -t $N @og_bridge_owner ; refresh-client -t <c> -f !ignore-size ; …'`,
  unflagging every flagged control-mode client on the session but our own: a
  pre-fix daemon's or a `-CC` client's flag would otherwise outlive the owner.
  The explicit target matters: `if-shell` expands
  its format against its target, which otherwise defaults to the issuing
  client's current session, and the survivor may be a moved stray. Every
  awake survivor then sees owner unset while flagged and unflags itself (one
  asleep through it does the same on waking, if its newest report still says
  unset), so the session falls back to today's
  minimum-of-every-cap, which every survivor's terminal fits, instead of
  freezing at the departed owner's size (bigger than a smaller survivor's
  screen) — a freeze that happens whenever any other unflagged client exists
  on the server, such as the same controller's daemon owning another session.
  The departing owner cannot do this itself: its usual exit is SIGTERM, whose
  handler kills the transport before teardown runs (`daemon.go` teardown,
  `RelayEnvUnsetCmd` comment), and a crash or an ssh timeout runs nothing. The
  condition makes concurrent survivors harmless and leaves a newer claim alone:
  a `replaceConn` claim on the new connection rewrites the client name before
  the old client goes.
- **Claim** (become owner): `set-option -t $N @og_bridge_owner '<token>|<self>'`,
  `refresh-client -f !ignore-size`, then enforce. Enforcement is what makes the
  takeover unilateral: the previous owner may be asleep, lid closed, ssh not
  yet timed out, and does nothing.
- **Recalculation trigger.** `server_client_set_flags` does not recalculate, so
  every flag change is followed by `ClientSizeCmd` at the client size the
  converger last recorded. Its `recalculate_sizes_now(1)` resizes every window
  on the server at once, current or not. When nothing is recorded yet (a
  first-attach claim runs before `mirrorStartupWindows` asserts anything), no
  trigger is sent: the client-size and per-window asserts that follow on the
  same connection are themselves `refresh-client -C` and recalculate. Every
  path that changes a flag ahead of its own size asserts (claim at first
  attach, the new-connection self-flag) is ordered before them for exactly this
  reason.

Claim and enforce do round-trips, so they run on the main-loop goroutine. The
reveal watcher only marks a claim as wanted and wakes the loop (its existing
`wakeCmd` path).

### Activity signal

The owner is the controller the user was last active on. A daemon counts its
user active when **`#{client_activity}` advances on a focused, non-control
local client attached to its mirror session**, or when the process first starts.

- `client_activity` is the local tmux's per-client last-input time, updated on
  every key, mouse and focus event and on resume from suspend (`server-client.c`
  key callback `:1338-1342`, `MSG_WAKEUP` `:2618`). `MSG_RESIZE` does not touch
  it, so a terminal resize alone never claims. It moves only through the real
  terminal in front of the user. A local client switching
  into the mirror session shows up carrying the stamp of the keypress that
  switched it, so "switching into the mirror" counts too.
- **Focused only.** The key callback stamps `activity_time` before it branches
  on the key (`server-client.c:1338-1342`), so a focus-*out* (the terminal
  losing focus, a screen lock on an idle controller) also advances it. A row
  counts only when `#{client_flags}` also carries `focused` at poll time;
  `CLIENT_FOCUSED` is cleared by a focus-out and stays set on a terminal that
  never reports focus.
- **Advance** means a counted row's stamp exceeds the high-water mark of every
  stamp seen on earlier polls **and** lies within two poll ticks of the poll's
  own clock (`stamp >= now - 3s`, whole seconds, `now` taken before the
`list-clients` fork; the stamp is the same local machine's clock and prints
truncated to the second, so 3s rather than two 1s ticks keeps a one-shot
switch-in from missing the window).
  The first poll only seeds the mark. A client that switches in carries the
  stamp of the keypress that just switched it, so it advances. The recency
  bound is for a client moved in with nobody at it: tmux-og sets
  `detach-on-destroy off` locally (`config/tmux.conf.tmpl:280`), and
  `server_client_set_session` sets `CLIENT_FOCUSED` unconditionally
  (`server-client.c:343`), so a client landing in the mirror because an
  automated job killed its session passes the focus gate with a stamp newer
  than the mark. Its stamp is its last input, older than two ticks unless the
  user was typing on this controller just then.
- Renderer input (bytes forwarded to `send-keys`) is rejected as the signal: it
  also carries the local tmux's automatic answers to remote terminal queries
  (DA, DSR, …), which flow with nobody at the keyboard and would let an idle
  controller reclaim.
- No extra fork: `watchReveal` already runs `list-clients -t <mirror session>`
  every tick; its format gains trailing `#{client_flags}` and
  `#{client_activity}` fields.
- The mark is per daemon process, not per connection, so a reconnect is not
  activity.
- Process start (`og-remote-open`, from the picker or a remote `switch-client`
  hand-off) is always a user action on this controller, so the process's first
  `runMirror` claims. A re-open is not: `runLoop` re-enters `runMirror` with
  `cfg.reopened` set after the remote server was replaced
  (`errServerReplaced`), with nobody at the keyboard. A re-opened mirror takes
  the new-connection row instead (on a fresh server the option is unset, so it
  does nothing).
- Counted activity always makes the main loop re-read the record, owner or not,
  and claim unless this daemon already owns it with its client unflagged. The
  believed owner (last `og_self` report, or an own claim) can lag another
  daemon's claim by up to one subscription tick, and a keystroke swallowed by
  that belief is lost, its stamp already consumed. A timing window instead would
  let a controller steal ownership back after a fast machine switch.

### Transitions

| Event | Daemon does |
| --- | --- |
| First `runMirror` of the process (`!cfg.reopened`) | **claim**, before `mirrorStartupWindows`; subscribe `og_self` with the other subscriptions |
| Re-opened `runMirror` (`cfg.reopened`) | the new-connection row, except that with the option unset it claims when no other control-mode client is on the session (a lone daemon after a server replacement keeps owning, so the membership rows keep covering it) |
| Activity, not owner | **claim** |
| Activity, owner | re-read; **claim** only if the record is not ours or our client is flagged, else nothing |
| New connection (reattach after a drop, `replaceConn`'s `primeClient`) | one round-trip read of `#{@og_bridge_owner}` **before any size assert** on it. Our token → claim (rewrites the client name). Another token → `refresh-client -f ignore-size` first, then the usual asserts. Unset → claim if this daemon owned before the drop (the survivors released it when the old client went; the user's own mirror was down meanwhile, so nothing they see changed), else nothing. In `repair` this read runs before `cv.reset()`: reattach publishes the connection first, and after the reset a `watchLocalClient` tick during the read would assert sizes on the still-unflagged client. Then re-subscribe `og_self` (repair already re-subscribes everything) |
| `og_self` report | reconcile (above) |
| `%client-session-changed <name> $N`, `$N` our pinned session, name not ours, we own | enforce |
| `%client-detached <name>`, `<name>` the recorded owner's client, not ours | hold the release; send it 30s later unless the record changed meanwhile (the owner reclaimed under its new client name) |
| `%client-detached <name>`, we own, `<name>` in membership | drop it; recalculation trigger |
| `%client-session-changed <name> $M`, we own, `$M` not ours, `<name>` in membership | drop it; recalculation trigger |

The two membership rows close the non-current-window stall for a release tmux
recalculates with `now=0`: a client on our session detaching, or moving off it.
That includes a human client on the remote, a pre-existing gap in the
single-controller case (the lone daemon owns from its first attach).
`%client-session-changed` names only the **new** session
(`control-notify.c:204-226`), so leaving is recognised by membership, not by
the notification. Only the owner reacts, and only for a client that was on its
own session, so a `refresh-client -C` — which under `now=1` resizes every
window on the server whether or not its size changed (`resize.c:388-394`) — is
not spent by every daemon on every attach, switch or detach anywhere on the
server. The owner's own size is the one that counts, so its trigger is the one
that matters.

### Walkthroughs

- **Takeover while the other controller sleeps.** Laptop L owns; user moves to
  Mac M and types → M claims: writes the token, flags L's client, recalculates
  → every window, current or not, goes to M's size.
- **L wakes, old connection alive.** L's backlog carries an `og_self` report
  `…|ignore-size…|<M's token>`: flagged, foreign owner → nothing to do. L's caps,
  re-sent by the 30s fallback or a resize, are ignored. No snap-back.
- **L wakes and reconnects.** The new client starts unflagged; the
  new-connection read finds M's token → `-f ignore-size` before `primeClient`
  or `repair` asserts a size. No snap-back.
- **M's network blips.** M's client detaches; L holds the release and stays
  flagged. M reattaches within 30s and reclaims under its new client name, so
  L drops the release (and the conditional one would be a no-op anyway). No
  resize.
- **User returns to L.** L's local client activity advances → L claims and
  enforces (flags M) → size follows the user back.
- **A sleeping controller's client is moved onto the session** (its own session
  destroyed under `detach-on-destroy off`). M owns the session; the move raises
  `%client-session-changed <L's client> $N` → M enforces → L's client is flagged
  and the windows recalculate at M's size. A pre-fix daemon's stray is handled
  the same way, since enforcement keys on control mode, not on a registry.
- **A new bridge attaches to the session.** M (owner) sees the join and flags
  it; the newcomer's first-attach claim then unflags itself and flags M. If M's
  flag lands after that claim, the newcomer's next `og_self` report
  (owner = itself, flagged) heals it.
- **Single controller.** First attach claims, enforcement finds nothing to
  flag. Converger, `#201` clamp, `#478` aggressive-resize opt-out, `#482` reset
  on reattach and the `@bridge_nudge` path (#647, #820) are untouched.

### Edge cases

- **All bridges flagged** (owner tore down): `ignore_client_size` counts flagged
  clients again only when no unflagged attached client exists on the **whole
  server**. With another unflagged client elsewhere (a human, or a bridge owning
  another session) the flagged clients are ignored and the window keeps its
  size. The next activity on any controller claims. Accepted.
- **Simultaneous claims**: last `set-option` wins; reconcile makes the loser
  flag itself and the winner unflag itself. Converges within one subscription
  tick.
- **Other control-mode clients** on a mirrored session (an iTerm2 `-CC`
  attach) are flagged by enforcement like any non-owner bridge. tmux exposes no
  way to tell a bridge's control client from another one; accepted and
  documented.
- **Mixed versions**: both controllers need the fix. A pre-fix daemon's client
  is flagged by enforcement like any other control client, and it never claims,
  so it is size-ignored while a fixed owner holds the record (under the old code
  it at least clamped everyone to the minimum); the release clears its flag.
  Documented in `bridge-daemon.md`.
- **A flagged client that is still `w->latest`** (a moved-in stray, or a former
  owner that created the window): `clients_with_window` no longer counts it,
  but with an owner and a human tty both on the window `n > 1` and the first
  loop takes only `w->latest`, which is skipped as ignored, so the window comes
  from the owner's caps alone and the human's own size stops clamping it.
  Resolves as soon as the human types (their key input takes `latest`); a
  bridge cannot re-take `latest` on a non-current window without selecting it.
  Accepted.
- **Remote-derived values**: the owner value is accepted only as
  `^[0-9a-f]{16}\|[A-Za-z0-9_./-]{1,64}$` (anything else reads as unset);
  client names reach `refresh-client -t` and the release condition only
  single-quoted (`tmuxQuote`), and the validated charset holds none of `#`,
  `,`, `}` or a quote, so a name cannot break out of the `#{==:…}` comparison;
  the subscription format goes through `ctlSafe` like every other.

## Testing

- **AC1** — the repro above, recorded in the PR's `## Testing`.
- **AC2** — bats cases in `tests/remote-m2-integration.bats` driving two real
  daemons (`--test-local`) against one SRC session from two DST servers, each
  with a real attached client hosted in a pty server (the `m2obs` pattern) at
  different sizes, typing driven by `send-keys` into those pty panes:
  1. both up, SRC at the small size; type into the big controller's client →
     SRC grows to its size. Red on `origin/main`: it never grows.
  2. `SIGSTOP` the small daemon, type into the big one, `SIGCONT` the small one
     → SRC stays big for several seconds; force the small daemon to reconnect
     (the suite's outage lever) → still big; type into the small controller's
     client → SRC follows to the small size.
  3. the dispatcher's moved-client case: the small daemon mirrors a second SRC
     session A; `SIGSTOP` it; `kill-session` A on SRC (`detach-on-destroy off`)
     → its client lands on the big daemon's session → that session's windows
     stay at (or return to) the big size.
- **AC3** — with one bridge only: a two-window remote, a smaller human client
  (pty) attached, then detached → the current window grows (tmux does that) and
  the non-current window grows too only with the `%client-detached`
  recalculation (red on `origin/main`). Also records whether visiting the
  mirror window locally heals it on `origin/main` (by the source analysis it
  does not).
- Go unit tests for the pure parts: owner token validation, the reconcile and
  enforce decisions and their command batches, the new-connection read
  decision, activity parsing from the extended `list-clients` row, and the
  two new control-mode notifications in `controlmode`. The pty-hosted bats
  clients send no focus events, so two cases are unit-only: a row whose stamp
  advanced without `focused` does not claim, and an `og_self` report naming
  this daemon as owner while `client_flags` carries `ignore-size` unflags it.
  Also unit-only: a focused row with a stale stamp (moved in) does not claim;
  membership adds and drops, the release decision and its command's quoting.

## Non-goals

- Changing `window-size`, `aggressive-resize` or `detach-on-destroy` on the
  remote.
- A registry telling bridge control clients from other control clients.
- Sizing across different remote sessions; ownership is per session.
