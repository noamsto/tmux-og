# Open URLs on the controlling host from a mirrored session (#854)

## Problem

In a bridge mirror, `prefix + p` runs prdash **on the remote**
(`generator/render/keys.go` `prdashBind` → `bridgedFloatTool` → the daemon's
`"tool"` ctl verb, `picker/remotebridge/daemon/ctl.go`). Every prdash open —
`o` on a PR/issue (single and bulk) and `o` on a check — goes through its
`openURL`, which since prdash's half of this feature shipped runs `$BROWSER`
when set (one executable name or path) and otherwise `open`/`xdg-open`. On the
remote that launches a browser nobody is looking at, or fails headless. The
user is at the controller.

The approved cross-repo design is prdash's
`docs/superpowers/specs/2026-09-13-open-url-on-controller-design.md` (written
when this repo was "lazytmux"). This spec is that design's tmux-og half mapped
onto the current tree, with the decisions it left open settled.

## Goal

`o` in prdash inside a mirrored session opens the URL in the controller's
browser — single and bulk. Outside a mirror, behaviour is the platform opener.
Any other `$BROWSER`-aware tool on a tmux-og host (`gh … --web`) gets the same.

Non-goals: opening files or non-URL targets; a reverse socket/port forward;
any prdash change.

## Design

### 1. `og-open <url>` — `scripts/og-open.sh`

A small script, packaged like every other `scripts/*.sh` (`scriptNames` in
`config/tmux.conf.nix`, so it lands in `wrapperBinDir` and on every pane's
PATH on every tmux-og host, remotes included). Its body is written to POSIX sh
(no bash-isms) but ships through `writeShellScriptBin` with the repo's
`#!/usr/bin/env bash` header like its siblings, so shellcheck/shfmt lint it
unchanged. It is recorded in `ogInternal` (it has no `og` verb: `$BROWSER`
calls it by path).

1. **Validate** — exactly one argument; it must start with `http://` or
   `https://` (lowercase), have at least one byte after the scheme, contain no
   whitespace or control character (`[[:space:][:cntrl:]]`), be at most 4096
   bytes, and must not end in `;`. Anything else: message on stderr, exit 2.
   - Whitespace is the record separator below.
   - A trailing `;` is tmux's argv command separator: `tmux set-option -a @x
     ' n|https://a;'` stores the value **without** the `;` (measured, next-3.9)
     — the URL would arrive altered, so it is refused rather than mangled.
2. **Bridged?** — when `$TMUX` and `$TMUX_PANE` are both non-empty, one call:
   `tmux list-clients -t "$TMUX_PANE" -F '#{?#{&&:#{client_control_mode},#{==:#{client_name},#{@og_open_client}}},1,0}|#{n:@og_open_url}'`.
   `-t <pane>` resolves to the pane's session (measured), so this lists only
   that session's clients; per row, the first field is `1` only for the
   control-mode client whose name the daemon registered in the session option
   `@og_open_client` (§3, "Register"), and `#{n:@og_open_url}` is the log's
   current byte length. Bridged iff some row starts with `1|`. A failing call
   (server gone) is "not bridged".
   - Why a registered name and not "any control client": a control client that
     is **not** a live `og_open` subscriber — a controller running a daemon
     that predates this feature, a daemon whose subscribe failed, iTerm2's
     `-CC` — would otherwise make `og-open` append to a log nobody reads, and
     the URL would vanish with prdash reporting "Opened". With the name check
     such a session is "not bridged" and the URL opens on the remote as it did
     before this feature. A registered client that disconnects stops matching
     (measured: the name is compared against the *attached* clients).
3. **Bridged** — nonce `<epoch-seconds>-<pid>` (`date +%s`, `$$`), record
   `" <nonce>|<url>"`. If the length read in step 2 is over 4096,
   `tmux set-option -t "$TMUX_PANE" @og_open_url "<record>"` (replace);
   else `tmux set-option -a -t "$TMUX_PANE" @og_open_url "<record>"`
   (append, atomic inside the server). A pane target on `set-option` without
   `-w`/`-p` writes the pane's **session** option (measured). Exit with that
   call's status. The value is an append-only log, not a single slot, because
   tmux reports subscription changes on a ~1 s timer and a bulk open would
   otherwise overwrite itself inside one tick.
4. **Not bridged** — `unset BROWSER`, then `exec open "$url"` on Darwin
   (`uname -s`), else `exec xdg-open "$url"`. The unset stops `xdg-open`'s
   own `$BROWSER` fallback from calling `og-open` again forever.

### 2. Config — `BROWSER` for every server-started pane

`set-environment -g BROWSER "<store path of og-open>"` in
`config/tmux.conf.tmpl` **and** the frozen `config/tmux.conf.reference.nix`
(the two must be edited together — the extraction check diffs them), next to
the `update-environment` block. The template reads the path as
`{{index .Paths.Scripts "og-open"}}`, the reference as
`${script.og-open}/bin/og-open`.

The absolute store path rather than the bare name `og-open`, because a
resident server may predate the rebuild (#407): a config reload after a
rebuild would set `BROWSER=og-open` on a server whose PATH (fixed at its start
by the wrapper) has no `og-open`, and every `$BROWSER` open in a new pane
would fail until the server restarted. The store path exists as soon as the
config naming it is loaded, and a pane holding it in its environment keeps it
a GC root. prdash treats `$BROWSER` as an executable name **or path**, and
`xdg-open` splits `$BROWSER` on `:`, which a store path never contains.

No version gate: `set-environment -g` predates every supported tmux.

Consequence, accepted: a user-level `BROWSER` exported into the tmux server is
overridden inside tmux panes. Unbridged, `og-open` falls back to the platform
opener (the desktop default browser), not the user's former `$BROWSER`.

**The bridged tool float keeps it.** The ctl `"tool"` verb spawns prdash
through the remote's default shell (fish), which rebuilds its environment from
the login profile — `toolResolveScript` (`daemon/ctl.go`) already restores
`PATH` from the tmux global environment for exactly that reason. A `BROWSER`
set in the remote's shell profile would override the tmux global one there
and silently disable the forward, so `toolResolveScript` restores `BROWSER`
the same way (guarded on a non-empty `BROWSER=` line; the body stays
single-quote-free and `#*`-trimmed like the `PATH` restore). An interactive
remote shell whose profile exports its own `BROWSER` keeps it — documented,
not overridden.

### 3. Daemon — the `og_open` subscription (controller side)

New file `picker/remotebridge/daemon/openurl.go`, following the
`og_res`/`og_usage` session-scoped pattern in `subscriptions.go`.

**Config seam.** `Config.OpenURL func(url string) error` — nil disables the
feature entirely (no seed read, no subscription), the convention of the other
injected funcs. Production (`cmd/daemon/main.go`) runs `open` on darwin,
`xdg-open` elsewhere, with `BROWSER` removed from the child's environment (the
controller's own tmux server now exports `BROWSER=og-open`, and the daemon
inherits it; without the removal a generic-mode `xdg-open` would bounce
through `og-open`, which on a controller that is itself mirrored elsewhere
would forward the URL a second hop). The child's stdout/stderr go to the
daemon's stderr (its log file), never a pipe. The wait is bounded: `xdg-open`
in generic mode, or a first browser launch, can block for the browser's whole
lifetime, and a crash hours later must not surface as a stale "could not
open". So only a start failure or a non-zero exit **within 5 s** is an error;
after 5 s the opener returns nil and a background goroutine keeps reaping the
child.

The controller opener reaches a browser only through the display/session
variables (`DISPLAY`, `WAYLAND_DISPLAY`, `DBUS_SESSION_BUS_ADDRESS`) the daemon
inherits from the local tmux server's environment — the same environment every
local pane gets. When they are missing the opener fails and the failure
surfaces through `notifyLocal`.

**`urlOpener`** — built once per mirror run beside `res`/`usage` (so its seen
set survives reconnects, which re-run `subscribe` via `repair`):

- `session string` — the pinned remote session id (`pin.id`). The
  subscription is scoped to the control client's *current* session, so during
  a session-pin excursion (#396) it reports another session's log — whose
  nonces this daemon never saw, and which another bridge already opened.
  A notification naming any other `$N` is ignored; empty accepts all, the
  posture `resShipper` takes.
- `seen map[string]bool` — nonces already handled.
- `open func(string) error`, `notify func(string)`, and `launch func(func())`
  (production: `go f()`; tests run it synchronously).

**Connect (first attach and every `repair`), in order**, called from the
existing `subscribe` closure in `daemon.go` so it runs exactly where the other
subscriptions (re)install:

1. **Seed**: `show-options -qv -t '<pin.id>' @og_open_url` as a round-trip
   (`-t` omitted when the id is unknown). `show-options -v` returns the raw
   value — never `display-message -p`, whose output is strftime-expanded and
   would rewrite a `%`-escape inside a URL. Every nonce in it is added to
   `seen`. A missing reply or `%error` is a seed failure.
2. **Subscribe** — only after a successful seed:
   `refresh-client -B 'og_open::#{@og_open_url}'` via `sendSubscription`
   (empty `what` = the control client's session). `%error` = failure.
3. **Register** — only after a successful subscribe:
   `set-option -F -t '<pin.id>' @og_open_client '#{client_name}'` as a
   round-trip (`-t` omitted when the id is unknown). `-F` expands against the
   issuing command's client, which over the control stream is this control
   client (measured, next-3.9: it stores the daemon's own `client-<pid>`).
   Registering last means a remote `og-open` can only ever see a registered
   client that already carries the subscription. Each (re)connect re-registers
   under the new client's name; nothing unsets it on teardown, because a
   departed client's name no longer matches any attached client. With two
   controllers mirroring one session the last to register wins, and if it
   leaves first `og-open` falls back to the remote's own opener until the
   other reconnects — degraded to pre-feature behaviour, never a silent drop.
4. **Failure** (seed, subscribe or register) — `notifyLocal` once for this
   connection, naming the step: "og-open: URL opens on <host> will not reach
   this machine (<seed|subscribe|register> failed; subscriptions need
   tmux ≥ 3.2)". A failed seed never subscribes: without a
   seed, the subscribe-time report would replay every old record. A failed
   subscribe never registers. No polling fallback — opens are events, not
   state.

Seeding before subscribing is what makes both edges right: tmux re-reports the
current value right after a subscribe, and those nonces are already seen, so
neither the first attach nor a reconnect replays an old URL; a record appended
between the seed and the subscribe is not in `seen`, so the subscribe-time
report opens it.

**On `%subscription-changed og_open $N - - - : <value>`** (dispatched from the
main loop's `SubscriptionChanged` case like its siblings, handled
immediately — nothing to coalesce, and `handle` issues no round-trip):

1. Ignore if `session != ""` and `$N != session`.
2. Parse: `strings.Fields(value)`; each token is cut at its **first** `|`
   into nonce and URL. A token with no `|`, or a nonce not matching
   `^[0-9]+-[0-9]+$`, is malformed and skipped.
3. For each well-formed record whose nonce is not in `seen` and whose URL
   passes `validOpenURL`: `launch` an open. A launched open that returns an
   error sends `notifyLocal`: "og-open: could not open <url>: <err>".
4. `seen` = exactly the nonces of the well-formed records in this value
   (bounded by `og-open`'s own 4096-byte reset).

**`validOpenURL`** — the controller-side security boundary. The URL is
remote-derived and reaches a local process argv, so it is re-validated
daemon-side regardless of what `og-open` checked (CLAUDE.md: remote-derived
values are sanitized daemon-side):

- length 1–4096 bytes;
- begins with `http://` or `https://` exactly (so it can never begin with `-`
  and be read as an opener flag);
- no byte `<= 0x20` and no `0x7f` (no whitespace, no control character);
- `net/url.Parse` succeeds, `Scheme` is `http` or `https`, `Host` non-empty.

Non-ASCII bytes are allowed (IRIs); they cannot form a flag or a separator.

`notifyLocal` already escapes `#`/`%` and passes the message after `--`, so a
URL inside a notice cannot expand as a local format.

### Data flow

```
prdash (remote) --$BROWSER--> og-open (remote)
  --set-option -a @og_open_url (pane's session)--> remote tmux
  --%subscription-changed og_open $N--> bridge daemon (controller)
  --validOpenURL--> Config.OpenURL --> open/xdg-open (controller)
```

## Contract map (producer → consumers)

| edge | disposition |
|---|---|
| `og-open` writes ` <nonce>\|<url>` records to session option `@og_open_url` | new; bats unit |
| remote tmux reports `og_open` subscription | new; Go unit (`ParseLine` of a multi-record value) + bats integration |
| daemon `urlOpener.handle` parses/validates/dedupes | new; Go unit |
| `Config.OpenURL` production opener (`cmd/daemon/main.go`) | new; Go unit for argv/env builder |
| `BROWSER` in global env → prdash, `gh --web`, `xdg-open` | new; conf assertion via the reference diff; `xdg-open` recursion broken by `unset BROWSER` (bats) |
| `subscribeFormats` / the other four subscriptions | compatible — untouched; `og_open` installs beside them |
| session-pin excursion (#396) | compatible — foreign `$N` ignored (Go unit) |
| daemon registers `@og_open_client`; `og-open`'s bridged test reads it | new; bats unit (match / no match) + bats integration (a real daemon registers) |
| `toolResolveScript` restores `BROWSER` for the tool float | changed; Go unit on the script text + existing ctl tool bats still green |
| `@og_open_url` / `@og_open_client` readers other than the above | absent |
| `@bridge_*` mirror options | absent — nothing is stamped locally |

## Error handling

- prdash shows "Opened" once `$BROWSER` starts — handed off, not displayed.
- `og-open` rejects bad input locally (exit 2) → prdash's "Open failed".
- Controller failures (opener missing or non-zero, subscription unsupported)
  → `notifyLocal` on the client viewing the mirror (best-effort, as for every
  other caller).
- Latency ≤ ~1 s (tmux's subscription timer).
- Two controllers mirroring the same remote session each receive the
  notification and each opens — documented, not guarded.
- A control client that died without tmux noticing yet is still registered:
  `og-open` still appends; the reattach's seed marks that record seen and it
  never opens. Accepted (the design's reconnect loss).
- A stale `@og_open_client` (`client-<pid>`) could match a later control
  client that reuses the pid and is not a subscriber — the silent drop again,
  at negligible odds. Accepted.
- The cap reset is check-then-act: concurrent `og-open` calls that each see
  the log over 4096 bytes each replace it, and within one subscription tick
  only the last survives. Accepted (the design's "only loss"); a bulk open
  that straddles the cap can lose all but its last URL.

## Testing

**bats — `tests/og-open.bats`** (new, its own flake check), `tmux`,
`xdg-open`, `open` and `uname` stubbed on PATH:
- rejects: no arg, two args, `ftp://`, `file:///`, `javascript:`, bare
  `https://`, a URL with a space / tab / newline, a trailing `;`, >4096 bytes →
  exit 2, no tmux write, no opener run.
- the `list-clients` call carries the registered-name format verbatim.
- bridged (stub `list-clients` prints `1|10`): one `set-option -a -t <pane>
  @og_open_url " <n>|<url>"` with a `<digits>-<digits>` nonce.
- bridged over the cap (`1|5000`): `set-option` without `-a`.
- not bridged — `$TMUX` unset; `$TMUX_PANE` unset; `$TMUX` set but only
  `0|…` rows (a control client that is not the registered one); `list-clients`
  failing: runs `xdg-open <url>` with `BROWSER` unset in its environment;
  `uname` = `Darwin` runs `open`.

**Go — `picker/remotebridge/daemon/openurl_test.go`**:
- `parseOpenRecords`: several records, malformed token, bad nonce, `|` inside
  a URL (kept), empty value.
- `validOpenURL` table: http/https accept; `file:`, `javascript:`, `HTTP://`,
  `-https://…`, `https://` (no host), space/tab/`\x00`/`\x7f`, over-long →
  reject.
- only unseen nonces open; seen becomes exactly this value's nonces.
- connect: seed-then-subscribe order; nothing opens from the replayed
  subscribe-time value; a record appended between seed and subscribe opens
  once; a reconnect (second connect with the same opener) replays nothing.
- connect order is seed → subscribe → register; a failed seed does not
  subscribe, a failed subscribe does not register, a failed register
  notifies; each failure notifies once per connection; `OpenURL == nil`
  issues none of the three.
- a foreign session id is ignored.
- a subscription line carrying several records survives
  `controlmode.ParseLine` intact.
- opener error → notify.
- `toolResolveScript` carries the `BROWSER` restore and still contains no
  single quote.
- production opener (`cmd/daemon`): argv per GOOS, `BROWSER` stripped from
  the child env, a slow child returns nil after the bound.

**Integration — `tests/remote-m2-integration.bats`** (existing harness,
scratch `-L` servers, `--test-local`): `xdg-open`/`open` stubs on the
daemon's PATH append their argv to a file; the raw `scripts/og-open.sh` is
passed in as `OG_OPEN` (as `DETACH` is) and run in the SRC pane.
- `og-open https://example.invalid/x` → the stub records exactly that URL,
  once.
- three rapid calls in one command line → all three recorded.
- the SRC session carries `@og_open_client` naming the daemon's control
  client.
- `kill -9` the transport, wait for the reattach to complete, wait past two
  subscription ticks → nothing new recorded; one more `og-open` after the
  reattach → recorded exactly once.

**Gate**: `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Docs

- `docs/agents/bridge-shipped-state.md` — new "URL Opens (remote → controller)"
  section: the record format, seed → subscribe → register, the registered
  client check, seen-set, pin filter, validation boundary, the controller
  env assumption, accepted losses.
- `docs/agents/bridge-daemon.md` — "What the Remote Host Needs on PATH" row
  for `og-open`, and the reconnect section's re-seed note.
- `docs/agents/scripts.md` — an `og-open` row.
