# Bridge `--into --window`: mirror only selected remote windows (#989)

## Goal

`og-remote-open <host> <sess> --into <local-session>` (#985) mirrors every
window of a remote session into an existing local session. `--window
<selector>` narrows that to the windows the selector matches, so one remote
window (for example a single remote agent worker) can be pulled into an
arbitrary local session without the rest of its remote session.

```
og-remote-open <host> <sess> --into <local-session> --window <selector>
```

Dedicated mode and plain `--into` keep their behaviour: every change below is
gated on a selector being present, and an absent selector is the code path
that exists today.

## Terms

- **Filtered daemon**: a placed daemon started with a selector.
- **Selected window**: a remote window of the pinned session the selector
  matches right now.
- **Own windows / registry**: as in the `--into` spec
  (`2026-10-15-bridge-into-session-design.md`). A filtered daemon's registry
  only ever holds selected windows.

## 1. Selector grammar and matching

One selector per call, two forms:

| Form | Regex (launcher and daemon) | Matches |
|---|---|---|
| window id | `^@(0\|[1-9][0-9]*)$` | the remote window with that `#{window_id}` |
| option match | `^@[A-Za-z_][A-Za-z0-9_-]{0,63}=[A-Za-z0-9._/@+:-]{1,255}$` | every remote window whose `#{@<option>}` equals `<value>` exactly |

- The option form splits on the first `=`; the name charset excludes `=`, so
  the split is unambiguous. A name may not start with a digit, so `@5=x` is
  never confused with the id form.
- **One selector, not a list.** Several selectors into one local session are
  several daemons (point 4), which is what the use case needs and keeps the
  idempotency key a single canonical string. A repeated `--window` is refused.
- **Canonical form = the validated input.** The regexes admit exactly one
  spelling per selector (no leading zeros in an id, no whitespace, no quoting),
  so no normalisation step exists to get wrong.
- **Rename survival.** Neither form reads `#{window_name}`: an id is stable for
  the life of the remote server, and an option survives a rename.
- **Option resolution.** `#{@<option>}` in window context falls back to the
  session and global value, like every remote format the daemon already reads
  (`@crew_branch` in the label format). A window matches on its *resolved*
  value; a session- or global-level stamp of the same value would select every
  window. Accepted and documented: crew stamps are window options.

### One snapshot, evaluated remotely

The selection is a column of the window list the daemon already reads, never a
second read, so "which windows exist" and "which are selected" always describe
the same instant. A filtered daemon's `list-windows` (startup, `reconcileWindows`,
`addWindow`) uses

```
'#{window_index} #{window_id} #{window_active} <flag> #{window_name}'
```

instead of `windowListFormat`, with `<flag>` evaluated **on the remote**:

| Form | `<flag>` |
|---|---|
| window id `@N` | `#{==:#{window_id},@N}` |
| option `@opt=VAL` | `#{==:#{@opt},VAL}` |

- `#{==:…}` expands to exactly `1` or `0`, a fixed token before the free-form
  name, so the space-split parse stays positional. Measured on the pinned tmux
  (9566f6f, scratch server): a value holding every allowed punctuation
  (`feat/989-x:y@z+q.r_s`) compares equal, a different value gives `0`.
- The selector charset holds no `,`, `}`, `#`, quote or whitespace, so `VAL` is
  inert inside the format and inside the single-quoted command.
- **Nothing remote-derived is carried**: the reply holds the id (already
  validated by the existing parse) and a `0`/`1` the daemon wrote the question
  for. The remote option's value never crosses the bridge.

**Positive evidence only** (the `reconcileWindows` / `mirrorPaneRows` rule):

- A failed, errored or empty read changes nothing (unchanged rule).
- A row whose flag is not exactly `0` or `1` is malformed: that window is
  neither added nor closed.
- A window is mirrored only on a row showing `1`, and a registered window is
  closed as "stopped matching" only on a row positively showing `0`. A window
  missing from a non-empty reply is closed as today (it left the session).

### Learning of option changes

Control mode has no `%window-option-changed`. An option-selector daemon
subscribes (the mechanism the label and agent shippers already use,
`subscriptions.go`) to the same flag:

```
refresh-client -B 'og_select:@*:#{window_id}|#{==:#{@opt},VAL}'
```

(through `sendSubscription`, so `ctlSafe`-wrapped like every other format).

- tmux reports every window once on subscribe and then each window whose flag
  moves, including a window created after the subscription and stamped later,
  which is the `dispatch` shape (`new-window`, then `set -w @crew_branch`).
- **A notification only asks for a reconcile.** `dispatch` marks the filter
  dirty; it never adds or closes on a subscription row. The main loop runs
  `reconcileWindows` (with `flowCfg`, like the other window-set operations)
  once per pass while dirty, and that reconcile's snapshot decides. A burst of
  rows costs one reconcile.
- **Backstop:** a reconcile also runs on the `pollFloor` cadence: every 30s
  once subscribed, every `mainLoopTickInterval` (5s) if the remote refused the
  subscription.
- The subscription is per control client: `repair` re-subscribes it with the
  others, and its on-subscribe report triggers one reconcile.
- An id selector needs no subscription: an id never starts or stops matching a
  live window.

## 2. Filter site

The registry is what every downstream path iterates (reseed, heal, sweep,
layout reconcile, focus-follow, label/agent stamping, per-window caps, reveal
activity, teardown). So the filter sits at the only three places a remote
window *enters* the registry, plus the one place a still-registered window can
stop being selected:

| Path | Today | Filtered |
|---|---|---|
| startup (`runMirror` → `mirrorStartupWindows`) | every window of `list-windows` | only rows showing `1`; **none is a startup error** (`daemon: no window of <sess> matches <sel>`), returned before the listener exists, so the launcher's listen wait reports it with the log tail |
| `reconcileWindows` unknown window | `mirrorNewWindow` | only on a row showing `1` |
| `reconcileWindows` known window | re-assert name | on a row showing `0` → `closeWindow` (stopped matching); `1` → re-assert name; malformed → leave it |
| `addWindow` (`%window-add`) | mirror if in session | mirror only if its row shows `1` |
| `og_select` notification | — | marks a reconcile due; never adds or closes itself |

Every other path was checked and needs no change, because it keys on the
registry or on a registered remote id:

- `%layout-change`, `%window-renamed`, `%window-pane-changed`,
  `%session-window-changed` (`translateWindowNotification`): registry lookup,
  unregistered ids ignored.
- `%window-close` → `closeWindow`: no-op outside the registry.
- heal-lost sweep (`healLostWindows`), dead-renderer heal, `retireMirror`
  (rebuilds through `reconcileWindows`, so filtered), `resetWindow`,
  `reseedWindow`/`reseedDropped`/`reseedReshaped` (router sinks exist only for
  registered panes), `reveal` (placed rows count only own windows),
  `sweepStrayOwnWindows`.
- focus-follow: `reconcileWindows` selects `activeRemote` only when it is
  registered; startup selects nothing placed.
- label shipper `apply` and agent shipper `stamp`: drop rows whose window/pane
  is not registered or not own-mapped (`localPaneMap` filters on
  `@bridge_sock`).
- `watchLocalClient`: `ConvergeCmd` per `reg.remoteIDs()` only.
  `AggressiveResizeOffCmd` is sent from `setupWindow`, i.e. only for a window
  being mirrored.

**Emptied registry.** Today an empty registry ends the mirror (teardown). A
filtered daemon keeps that rule: when its last selected window closes or stops
matching, the daemon tears down and exits, exactly like a plain `--into` whose
remote session lost its last window. A filtered daemon never idles with no
windows (nothing local would show it, and `og-remote-detach` needs a window to
run from). So a stamp briefly unset and re-set (a crew resume re-stamping
`@crew_branch`) ends the daemon if a reconcile lands in between; listed under
the accepted limitations.

**Server replaced.** A window id names a window of one server; after
`errServerReplaced` a re-open would apply `@N` to an unrelated window of the new
server. An id-selector daemon therefore ends instead of re-opening
(`endReopen` with the existing reopen-failed text, which kills only its own
windows). An option selector re-opens normally and re-evaluates against the
new server; if nothing matches there, the re-opened run's startup error takes
the existing `runLoop` path for a failed rebuild (`endReopen(reopenFailedText)`
→ own windows killed, daemon exits).

## 3. Size ownership (main risk)

Two filtered daemons on one remote session are two control clients of it, so
#979/#980 multi-controller ownership applies unchanged: one session-level owner
record (`@og_bridge_owner` on the pinned session), every non-owner control
client flagged `ignore-size`.

**The rule:** a filtered daemon sends a per-window size command
(`ConvergeCmd` `refresh-client -C @N:WxH`, `AggressiveResizeOffCmd`) only for a
registered (selected) window, and counts local activity toward a claim only on
rows whose current window is one of its own (already the placed rule in
`watchReveal`). It takes part in the session-level record exactly like any
placed daemon: claim at process start, on counted activity and on reconnect;
enforce, release and reconcile unchanged.

What tmux allows and what is accepted (pinned `resize.c`):

- `ignore-size` is per **client**, not per window, so ownership cannot be split
  per window. A per-window cap is only a clamp (`clients_calculate_size`'s skip
  loop), and a control client's whole-client size (`ClientSizeCmd`) is its size
  for every window it has no cap on. A filtered daemon must keep sending the
  whole-client size: without it a control client that set only caps is not
  ignored (`CLIENT_WINDOWSIZECHANGED`) and contributes 80x0 to every other
  window (`tty.sy` 0). So the owner's client size still reaches remote windows
  it does not mirror whenever it is the latest or only non-ignored client on
  them — the same reach every controller has today, and the reason a dedicated
  daemon's attach has always sized the remote's unmirrored-by-it windows too.
  Documented, not changed.
- **Disjoint daemons in one host session do not fight.** Both compute the same
  `LocalArea` (it reads the host session's clients, then the latest client on
  the server), so the owner's client size, which sizes the non-owner's windows
  while the non-owner is flagged, equals what the non-owner's caps would have
  asserted. A claim moves only on process start, counted activity in the
  claimer's own windows, or a reconnect, so two idle daemons never trade the
  record. Each switch of the user's typing between them costs one claim and its
  recalculation, the existing #979 cost per controller switch; the
  recalculation's same-size `%layout-change` lines are deduplicated by
  `reconcileLayoutFrom` with no round-trip.
- Daemons with **different** local areas (two host sessions on different
  terminals) behave like any two #979 controllers: the remote follows the one
  the user last typed in. Accepted and documented.

Proof: a Go unit test that a filtered reconcile registers, caps and
`aggressive-resize`s only selected windows and that a cap pass never names an
unselected one; and a bats case with two selectors from one remote session into
one local session, an observer client attached at a size different from the
remote's start size. Observable: both remote windows' `#{window_width}x#{window_height}`
reach the observer's content area within the resize budget, then 20 samples
100ms apart (2s) of both windows all equal that area (no flip-flop), and each
mirror window's size equals its remote window's.

This is a deviation from the task's literal "never sizes or claims ownership
over windows it does not mirror": the per-window commands honour it, the
session-level claim cannot. The PR body calls it out.

## 4. Idempotency key

`sock_key` gains the selector as a fifth NUL-separated field **only when one is
given**:

```
printf '%s\0%s\0%s\0%s\0%s' "$host" "$sess" "$server_pid" "$into_id" "$selector" | cksum
```

A plain `--into` key stays byte-identical to today's, so an existing daemon is
still found by its repeat call after an upgrade. A repeat call with the same
(host, remote session, local session, selector) finds the live daemon and exits
0 before any ssh (the existing `probe_daemon` path under the lock). Different
selectors into one local session hash to different sockets and run as separate
daemons. Selectors are not deduplicated against each other: two selectors that
match the same window give two mirrors of it (both sized from the same area, so
they do not fight). The same holds for a plain `--into` and a filtered one on
the same (host, remote session, local session): different keys, so the
selected windows are mirrored twice.

## 5. Bandwidth

Control mode streams `%output` for every pane of the attached session, so a
filtered daemon still receives output from unselected panes (the router drops
it: no sink), plus their `%layout-change` lines and label/agent subscription
rows (dropped by registry lookup). Accepted and documented. A future reduction
is `refresh-client -A '%N:off'` for unselected panes, which needs pane-set
tracking for windows the daemon otherwise ignores; out of scope.

## 6. Teardown

Unchanged mechanism, already per daemon: placed teardown and `og-remote-detach`
kill only windows whose window-scoped `@bridge_sock` is this daemon's, and each
selector has its own socket. Detaching from one filtered mirror window ends
only that daemon; daemon exit removes only its windows.

## 7. Launcher surface

- `--window <selector>` is accepted only as `og-remote-open <host> <sess>
  --into <local> --window <selector>` (arguments 5 and 6, nothing after): the
  `--into` refusal of more than 4 arguments relaxes to exactly 4, or exactly 6
  with `$5 == --window`. `--window` anywhere without `--into` (dedicated mode)
  is refused: `og-remote-open: --window needs --into`, checked before the
  dedicated window-argument regex, which would otherwise report it as a bad
  window index. Any other placement or a repeated
  `--window` is refused with the usage line. All refusals exit 1 before any
  ssh, like the other `--into` refusals.
- The selector is validated against the regexes in point 1 in the launcher,
  before it reaches the lock path, the ssh probe or the environment; a refused
  selector is echoed with `%q`.
- The launcher passes it as `OG_DAEMON_SELECT` (daemon flag `--select`; the
  daemon's existing `--window` is the dedicated mode's initial window index).
  The dedicated launch unsets `OG_DAEMON_SELECT`, so a value in the caller's
  environment cannot filter a dedicated mirror. The daemon re-validates with
  the same regexes and refuses `--select` without `--into`.
- The probe is unchanged: the daemon decides "nothing matches" against the
  session it attached, and the launcher's listen wait already reports a daemon
  that exits before listening with its log tail.

## Accepted limitations (documented in `bridge-daemon.md`)

- A window created from a filtered mirror (`prefix + c` → ctl `new-window`,
  `prefix + !` → `break-pane`) is created on the remote but appears locally only
  if it matches the selector.
- Bandwidth as in point 5.
- Option matching uses the resolved value (point 1).
- Size ownership is session-level (point 3).
- A selected window whose stamp is unset and re-set across a reconcile loses its
  mirror, and if it was the last one the daemon exits (point 2).
- Overlapping selectors, or a plain `--into` beside a filtered one, mirror a
  window more than once (point 4).

## Out of scope

- Dedicated mode filtering.
- More than one selector per daemon; boolean selector expressions.
- `refresh-client -A %N:off` output suppression.

## Docs

`docs/agents/bridge-daemon.md` "Placed mirrors": a "Filtered mirrors
(`--window`)" sub-section (grammar, subscription, filter sites, emptied
registry, server replaced, limitations) and a shared-session sizing bullet in
"Size ownership across controllers". `docs/agents/scripts.md`'s `og-remote-open`
row: the `--window` argument, its refusals and the fifth key field.
