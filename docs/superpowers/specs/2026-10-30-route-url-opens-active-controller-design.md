# Route mirrored URL opens to the active controller (#1034)

## Problem

Two machines mirroring one remote tmux session each run a bridge daemon whose
control client is attached to that session. Today:

1. **Both open.** `og-open` appends `" <epoch>-<pid>|<url>"` to the session
   option `@og_open_url`; every daemon subscribed to the session (`og_open`
   subscription, its own `seen` set) opens every new record. The per-machine
   claim directory (`newOpenClaimer`, O_EXCL marker under the local temp dir)
   dedups daemons on *one* machine only, so two machines each open a tab.
2. **Survivor goes unbridged.** Registration is a single slot: each daemon
   stores its own `#{client_name}` in `@og_open_client`, last connect wins.
   When the registrant disconnects while another controller stays attached,
   `og-open`'s check (`client_control_mode && client_name == @og_open_client`)
   matches no attached client and the URL opens on the remote.

Invariant to restore: **a URL `og-open` forwards is opened by exactly one
controller, the one whose user was last active on the session; and as long as
any current daemon with a live `og_open` subscription is attached, `og-open`
forwards.**

## Measurements (tmux next-3.9, the pinned `a94699c`, scratch servers)

- **A control client's `#{client_activity}` never advances from commands.**
  Two `-C` clients on one scratch session; client 1 sent `display-message -p`,
  then `send-keys -t s x`, client 2 sent `set-option`; `client_activity` of
  both stayed at the attach second throughout (8s run). So the task's first
  candidate — `og-open` picking the control client with the most recent
  `client_activity` — cannot work: the daemon's forwarded keys never show up
  there.
- **A user-option write marks every pane style-changed.** `options_push_changes`
  (`options.c`) sets `PANE_STYLECHANGED` on *every* pane of the server for any
  `@name`; `server_client_loop` then runs each pane mode's `style_changed`, and
  the next draw of each pane recomputes its style cache (`tty_style_changed`,
  a `format_create` per pane). Per keystroke that is a server-wide cost on the
  remote, on top of the one extra command (plus its `og-fanout` barrier) the
  stream already pays per command.
- `#{m:* #{client_name} *, #{@og_open_clients} }` (list membership),
  `#{m:*|#{client_name},#{@og_bridge_owner}}` (owner match against
  `<token>|<client>`), `set-option -F -t s @L '#{s/ client-1( |$)/ /:@L}'`
  (removes `client-1` but not `client-12`, also at the end of the list) and a
  control client's `set-option -a -F -t s @L ' #{client_name}'` (appends its own
  name) all behave as written.
- `#{L:...}` (loop over clients) exists only from tmux 3.4 (`CHANGES`, "3.3a TO
  3.4"); the bridge supports remotes from 3.2 (subscriptions), so the design
  does not use it.

## Design A — `og-open` picks by activity (task's candidate): rejected

Dead on measurement: control-client activity never moves.

## Design B — daemon stamps "last input from me" per forwarded keystroke: rejected

`pumpInput` would write `set-option -t <sess> @og_open_last <self>` ahead of
each forwarded chunk, giving exact key→open ordering on the control stream.
Rejected for two reasons:

- **Wrong signal.** `docs/agents/bridge-daemon.md` (#979, "Activity is the
  local `#{client_activity}`") already established that renderer input is not
  user activity: it also carries the local tmux's automatic answers to remote
  terminal queries (DA, DSR), which flow from an *idle* controller whenever the
  remote app queries. An idle controller would steal the routing.
- **Per-key cost.** One user-option write per keystroke marks every remote pane
  style-changed (above) and adds two commands (stamp + barrier) per key. A
  belief-based debounce ("only when I'm not the last stamper") is not exact:
  the other controller's stamp reaches this daemon a subscription tick late.

## Design C (chosen) — route by the #979 size-owner record

The bridge already elects "the controller whose user was last active on this
session": the size owner, `@og_bridge_owner = <token>|<client_name>` on the
pinned session (`sizeowner.go`). It is claimed at process start, on counted
local activity (focused local client, `client_activity` above the high-water
mark and within 3s — DA/DSR answers excluded by construction), and on
reconnect; survivors release it 30s after the owner's client detaches. Using
the same record for URL routing costs nothing per key and gives one consistent
notion of "the active controller": the remote's windows and its URL opens
follow the same machine.

### Components

**1. Registry: `@og_open_clients` (session option, new).** A space-separated
list of the control-client names of current daemons with a live `og_open`
subscription. Written only by daemons, read by `og-open`.

- Register (the existing third connect step, still after a successful seed and
  subscribe) becomes one round-trip read plus one batch:
  - read: `display-message -p -t <sess> <ctlSafe('#{client_name}|#{@og_open_clients}')>`
    and server-wide `list-clients -F <ctlSafe('#{client_name}')>`;
  - send: for each name in the list that is valid (`^[A-Za-z0-9_-]{1,64}$`),
    not this client and not attached anywhere on the server, one atomic
    `set-option -F -t <sess> @og_open_clients '#{s/ <name>( |$)/ /:@og_open_clients}'`;
    then `set-option -a -F -t <sess> @og_open_clients ' #{client_name}'`
    (skipped when the list already holds this client); then the existing
    `set-option -F -t <sess> @og_open_client '#{client_name}'`.
  - Each write is a single remote command evaluated inside the server, so two
    daemons registering at once cannot lose each other's entry (no
    read-modify-write of the whole list). A prune only ever removes a name the
    reader saw *not attached*; a live name is never removed. The list stays
    bounded by live registrants plus names that left since the last register.
  - The register read also yields this daemon's own `client_name`
    (`urlOpener.self`), which routing needs. A name failing the charset reads
    as unknown (`self` empty, so no tagged record ever matches it) and the
    register fails through the existing notice ("register failed").
- **`@og_open_client` stays written** (single slot, same command as today) so
  an older `og-open` on the remote keeps today's bridged check.
- A disconnected registrant drops out without any write: `og-open` only
  considers *attached* clients (`list-clients -t "$TMUX_PANE"`), so a stale
  name in the list or in the slot never matches. That is what fixes failure 2:
  the surviving daemon is still in the list.

**2. `og-open`: bridged check and target.** One `list-clients -t "$TMUX_PANE"`
fork, as today, with a row per attached client:

```
#{?#{&&:#{client_control_mode},#{||:#{m:* #{client_name} *, #{@og_open_clients} },#{==:#{client_name},#{@og_open_client}}}},1,0}|#{?#{&&:#{client_control_mode},#{&&:#{m:* #{client_name} *, #{@og_open_clients} },#{m:*|#{client_name},#{@og_bridge_owner}}}},#{client_name},}|#{n:@og_open_url}
```

- bridged = some row's first field is `1` (an attached control client is in
  the list **or** in the old slot).
- target = the second field of a row, when non-empty: the attached control
  client that is both registered in the list and named as size owner. At most
  one client can match the owner record.
- Record: target found → `" <epoch>-<pid>@<target>|<url>"`; else the old
  untagged `" <epoch>-<pid>|<url>"`. Validation, the 4096-byte cap and the
  replace-past-4096 rule are unchanged.

**3. Daemon `handle`: tag-aware records.** `parseOpenRecords` accepts a token
`<nonce>[@<tag>]|<url>`: nonce `^[0-9]+-[0-9]+$`, tag `^[A-Za-z0-9_-]{1,64}$`
(remote-derived, validated daemon-side; anything else skips the token, as a
malformed record is skipped today). `seen`, the claim key and the replay rules
use the nonce only. A fresh valid record opens when it is untagged (today's
behaviour) or its tag equals `urlOpener.self`; a record tagged for another
client is marked seen and skipped *before* the rate limit (it costs no token,
counts toward neither the per-report cap nor a suppression notice).
`validOpenURL`, the bounded format, the token bucket, the per-report cap, the
notice gate and the per-machine claimer are unchanged.

### Compatibility (every version pairing)

| remote `og-open` | daemons attached | record | who opens |
| --- | --- | --- | --- |
| old | any | untagged | every subscribed daemon, as today (new daemons keep writing the old slot, so old `og-open` still sees the session as bridged) |
| new | only old daemons (none in the list) | untagged (no registered owner) | every old daemon, as today |
| new | new owner + old other | tagged → new owner | new owner only; old daemons skip a tagged token (their nonce regex rejects `<nonce>@<tag>`) — the target always is a registered current daemon, so nothing is dropped |
| new | old owner + new other | untagged (owner not in the list) | everyone, as today |
| new | owner gone (released or not yet), survivors registered | untagged | every registered survivor; one survivor → exactly one |
| new | owner's `og_open` connect failed (never registered) | untagged | the registered others |

"Today's behaviour" for an older daemon is kept in the sense the task asks:
an untagged record opens on it exactly as before, and no URL is ever routed
to a client that cannot read it — a tagged record names only a client that
registered in `@og_open_clients`, which only a current daemon does, after its
own seed and subscribe succeeded.

### Known limitation (documented, accepted)

The owner claim lags the first counted input on a controller by at most one
reveal poll (1s) plus a claim round-trip. A URL opened by the very first input
on a controller just switched to (within that window) is routed to the
previous owner. The same lag already applies to window sizing (#979); the
user's typical sequence (navigate, then open) claims before the open. Closing
it exactly needs a per-key signal, which is Design B.

Further accepted edges, documented in `bridge-shipped-state.md`:

- **Focus claims too.** `focus-events` is on and a focus-in stamps the local
  client's `client_activity` with `CLIENT_FOCUSED`, so switching terminal
  focus to a controller claims within one reveal poll, before any key — which
  shrinks the window above in practice.
- **A suspended owner keeps the URLs.** An owner machine asleep with its ssh
  still attached stays the owner until another controller's counted activity
  claims (a claim needs nothing from the old owner). An open that no input on
  the awake controller preceded (a background job) is routed to the sleeping
  one, where today both machines opened it. Any input on the awake controller
  first claims and fixes it.
- **Remote pid reuse.** Names are `client-<pid>`; a stale list entry survives
  until the next register's prune, so a later non-subscriber control client
  (iTerm2 `-CC`, a pre-#854 daemon) reusing that pid on the same session reads
  as registered — the same class as today's stale single slot, widened to the
  names since the last register.

## Shared-contract map

| edge | symbol / file | disposition |
| --- | --- | --- |
| producer: record append/replace | `scripts/og-open.sh` | **changed**: tagged record when a registered owner is attached; untagged otherwise |
| producer: bridged check | `scripts/og-open.sh` list-clients format | **changed**: list or slot |
| transport: bounded log | `openURLFormat`, `ctlSafe` (`openurl.go`, `ctlsafe.go`) | compatible: the tag adds ≤ 65 bytes per record inside the existing 12288-byte bound; `og-open`'s 4096-byte URL cap is unchanged; `ctlsafe_test.go` pins the format |
| consumer: seed | `urlOpener.connect` → `boundedOpenRecords` | **changed** via `parseOpenRecords` (tag-aware; seen keyed by nonce) |
| consumer: report | `urlOpener.handle` | **changed**: tag filter |
| consumer: older daemons | released `parseOpenRecords` | compatible: skip tagged tokens, open untagged ones |
| register: slot | `@og_open_client`, `connect` step 3 | compatible: still written |
| register: list | `@og_open_clients` (new) | **new**: written by `connect`, read by `og-open` |
| owner record | `@og_bridge_owner`, `sizeowner.go` | compatible, read-only new consumer (`og-open`, glob `*\|<client>`); its format regex is unchanged. Now a cross-language contract: a comment at `bridgeOwnerOption` names `og-open` as a reader, and the integration routing test runs real daemons' claims through the real `og-open`, so a format change fails it |
| per-machine claimer | `newOpenClaimer` | compatible: still dedups daemons on one machine for untagged records |
| tests | `openurl_test.go` (`connectCmds`, `TestURLOpenerConnect*`, live-tmux `TestOpenURLCommandsAgainstLiveTmux`), `tests/og-open.bats`, `tests/remote-m2-integration.bats` (#854 case) | **changed/extended** |
| docs | `docs/agents/bridge-shipped-state.md` "URL Opens", `bridge-daemon.md` (filtered-daemon note on N tabs; #979 owner record gains a reader), `scripts.md` (`og-open` row: bridged check, options it reads) | **changed** |
| absent | retry/batch/operator paths for records | absent: `og-open` is fire-once, the daemon has no retry |

## Acceptance tests

- **Routing (red on main):** `tests/remote-m2-integration.bats`, two daemons
  on two local servers (the #979 `two_controllers_up` pattern) mirroring one
  `SRC` session, each with its own opener stub on PATH **and its own `TMPDIR`**
  (separate claim dirs: two machines). Type into controller S until S owns
  (`type_until`), run `og-open` in a remote pane: only S's stub records the
  URL. Type into B until B owns, run `og-open` again: only B's stub. On main
  both stubs record both URLs.
- **Survivor (red on main):** after the registrant disconnects (kill the
  daemon that registered last), `og-open` in the remote pane still reaches the
  survivor's stub (on main it opens on the remote).
- **Untagged record:** a record written directly in the old format
  (`set-option -a @og_open_url ' <n>-<n>|<url>'`) opens on both controllers;
  plus Go unit tests for `parseOpenRecords`/`handle` (tagged-for-me,
  tagged-for-other no token spent, malformed tag skipped, untagged).
- `tests/og-open.bats`: tagged record when the owner row names a target,
  untagged otherwise, bridged via list or slot, not bridged via neither.
- Go: the register step's commands (prune, append, slot) and their
  failure/notice; a live-tmux case that two control clients registering leave
  both names in the list and a detached name is pruned on the next register.
