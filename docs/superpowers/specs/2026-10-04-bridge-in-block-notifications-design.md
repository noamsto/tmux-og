# Bridge control-mode reader: stop lifting forged notifications out of reply bodies (#899)

## Problem

`controlmode.Reader` lifts any reply-body line that parses as a known
notification verb out of the block and returns it as a notification (#276).
Since #860 it keeps guards, `%begin`, `%subscription-changed` and an `%exit`
not followed by end-of-stream as body; every other verb is still acted on:
`%output`, `%extended-output`, `%window-close`, `%window-add`,
`%window-renamed`, `%layout-change`, `%session-changed`,
`%session-window-changed`, `%window-pane-changed`, `%pause`, `%continue`.

A program running in a remote pane controls the body of the
`capture-pane -e -p` reply the daemon reads on every seed and reseed
(`daemon/seed.go`). It needs no socket access. A row it prints that parses as
one of those verbs is acted on by the daemon (`daemon.go` `dispatch`), and is
missing from the seeded snapshot. A lifted `%output %N` is `Unescape`d and
routed into another mirrored pane's renderer, so one pane can spoof another
pane's screen. A lifted `%window-close @N` retires a mirror window. A lifted
`%continue %N` from pane N's own screen reseeds N, and that reseed's capture
lifts it again: the reseed loops for as long as the row stays on screen.

## Evidence: which notifications tmux can write inside a block

`%begin` and its `%end`/`%error` are written from one synchronous
`cmdq_fire_command` call (`cmd-queue.c`, `cmdq_guard` before and after
`entry->exec`), so a line can sit between them only if something writes it
synchronously while that command runs. The source paths per tmux version:

- **`%output` / `%extended-output`**: written only by `control_write_output`.
  Its one caller is `window_pane_read_callback` (`window.c`), a libevent read
  callback that cannot run inside `cmdq_fire_command`. The data is queued as
  an output block in `cs->all_blocks` and flushed from the write callback
  (`control_write_pending`). Guard and body lines are queued as one contiguous
  run of line blocks, which `control_flush_all_blocks` writes in one loop. So
  an `%output` line can be reordered ahead of a block, but never inside one.
  The path is the same in 3.2a (`window.c:1019` in 3.3a), and in the pinned
  next-3.9 (`window.c:1513` in 29bf7fe). The event rewrite (d29aa121) does not
  route output through events.
- **Notify-path notifications** (`%window-*`, `%layout-change`,
  `%session-*`, `%sessions-changed`, `%unlinked-window-*`, `%client-*`,
  `%pane-mode-changed`, `%paste-buffer-*`): on every release through 3.7c,
  `notify_add` (`notify.c`) does `cmdq_append(NULL, notify_callback)`. That puts
  the notification on the **global** queue, which `server_loop` runs on its next
  pass (`cmdq_next(NULL)`), after the client's command item has finished and
  written its `%end`. Upstream commit d29aa121 (2026-07-10, merged to portable
  master at 25e2e1d6 on 2026-07-13) replaced the notify system with
  synchronous events. That moved these lines **inside** the running command's
  block. tmux issue 5458 (George Nachman) says so: "Before that, the notify
  system deferred delivery until the running command had finished, so
  notifications came out after `%end`." Commit 6db5175e, merged at 9228f97d
  on 2026-08-03, fixed it: `control_notify_write` defers every notification
  while `guard_depth > 0` and flushes it after the outermost `%end`
  (`control.c`). `configure.ac` reports `next-3.8` from 89a59d4d (2026-07-03)
  until 9b3268a2 (2026-09-09) bumps it to `next-3.9`. So **every build that
  writes these inside a block reports `next-3.8`**. Not every `next-3.8` build
  does: those built after 2026-08-03 have the fix. 3.8-rc3, and so release 3.8,
  contains the fix.
- **`%pause` / `%continue`**: `refresh-client -A '%N:pause'` and
  `'%N:continue'` call `control_pause_pane` / `control_continue_pane`
  synchronously (`cmd-refresh-client.c:158-160` in 3.7c). Up to the fix those
  called `control_write`, so the line sits inside the `refresh-client` block.
  After 6db5175e they go through `control_notify_write` and are deferred. The
  `%pause` that pause-after writes (`control_check_age`) comes from the
  read/write callbacks, never from inside a block. The daemon sends
  `refresh-client -A '%N:continue'` (`handlePause`) and never `:pause`.
- **`%subscription-changed`**: written from a timer (`control_check_subs_timer`).
  It is already body (#860).
- **`%exit`**: printed by the exiting client as its last line. The held-`%exit`
  rule from #860 is unchanged.

Live measurements back the source reading. The probe is one `tmux -C` client
per version on a private `-L` server under its own `TMUX_TMPDIR`. Its pane
printed `%window-close @0`, `%output %9 forged`, `%session-changed $9 evil`,
`%continue %0` and `%window-add @7`. The client then sent `new-window -d`,
`rename-window`, `split-window -d`, `capture-pane -p`,
`refresh-client -A '%0:pause'`, `refresh-client -A '%0:continue'`,
`new-session -d`, `switch-client` and `kill-window`. "in" means the line sat
between the command's `%begin` and `%end`.

| verb (genuine) | 3.2a | 3.7c | next-3.8 @29bf7fe (2026-07-24) | next-3.9 @8a9122d (pin) |
|---|---|---|---|---|
| `%output` / `%extended-output` | after | after | after | after |
| `%window-add` (new-window) | after | after | **in** | after |
| `%window-renamed` (rename-window) | after | after | **in** | after |
| `%layout-change` (split-window) | after | after | **in** | after |
| `%session-changed` (attach, switch-client) | after | after | **in** (attach: in the flags-0 attach block) | after |
| `%unlinked-window-add`, `%sessions-changed` (new-session) | after | after | **in** | after |
| `%unlinked-window-close` (kill-window) | after | after | **in** | after |
| `%pause` (`refresh-client -A :pause`) | **in** | **in** | **in** | after |
| `%continue` (`refresh-client -A :continue`) | **in** | **in** | **in** | after |
| forged rows in a `capture-pane -p` body | in (body) | in (body) | in (body) | in (body) |

The 29bf7fe column is what #276 measured: that was tmux-og's pinned tmux
(`fad3985`, 2026-07-24 → `3e527b0`, 2026-08-05), and the issue's wire capture
shows `%window-add @12` inside the `new-window` reply. #276 lifted
notifications because of that build window. On every released version, only
`%pause`/`%continue` from `refresh-client -A` are written inside a block.

## Design

1. **`%output` / `%extended-output` inside a block are always body**, whatever
   the remote's version. No tmux version writes a genuine one there, so this
   closes the cross-pane spoof unconditionally, before anything is known about
   the remote. **`%pause` / `%continue` inside a block are body on every
   version too.** A genuine in-block one comes only from this client's own
   `refresh-client -A`: the daemon never sends `:pause` and reads its
   `:continue` from the reply (rule 4). A lifted forged `%pause %N` would loop:
   pause, reseed, capture, lift again.
2. **In-block lifting becomes a per-connection switch on the Reader**,
   `Reader.SetLiftInBlock(bool)`. It is safe to call from another goroutine,
   because the pump goroutine calls `Next` while the main loop sets the switch.
   It defaults to `false`. Off, every in-block line that is not the matching
   closing guard is body, except the held `%exit` (#860, unchanged). On, every
   verb is lifted except `%output`/`%extended-output`/`%pause`/`%continue`,
   which rule 1 keeps body.
   The default is off because a block read before the identity read can carry
   pane-derived text: a flags-0 remote hook block may run
   `display-message -p '#{pane_title}'`.
3. **The remote's version decides the switch.**
   `controlmode.LiftsInBlock(version string) bool` returns `true` for
   `next-3.8` and for any version string it cannot classify, such as an
   OpenBSD base build (`openbsd-7.9`). It returns `false` for a release
   (`X.Y`, optional letter, optional `-rc` or `-rcN`, any X.Y; tmux has
   shipped bare `3.3-rc`, `3.7-rc` and `3.8-rc` in `configure.ac`), and for
   `next-X.Y` with X.Y ≠ 3.8. Older `next-` builds predate d29aa121; newer ones contain
   the fix. The only 3.8 pre-release is `3.8-rc3` (tag 331611b6, released
   2026-09-09), and it contains 6db5175e. Released ≤ 3.7c returns `false` although `%continue` is in-block
   there: rule 4 removes the daemon's dependence on that line.
4. **The daemon no longer needs `%continue` lifted.** `handlePause`
   round-trips `refresh-client -A '%N:continue'` instead of firing it and
   forgetting. Once the reply arrives, tmux has resumed the pane, so
   `handlePause` runs the reseed-then-resume itself (`handleContinue`). A
   Any reply runs that reseed-then-resume, an `%error` included (for example,
   the pane closed after its `%pause`). `handleContinue` already tolerates a
   failed capture, and resuming a sink whose pane is gone is harmless. A
   `%continue` notification, top-level or lifted, is then a no-op in
   `dispatch`. tmux writes `%continue` only in answer to
   `refresh-client -A :continue`, and the daemon's only sender is
   `handlePause`, so that no-op drops nothing. A connection that closes
   before the reply skips the reseed. `repair` on the next connection resumes
   every sink and reseeds it, as it does today for a `%continue` lost to a
   drop.
5. **The identity read carries the version.** It is the first round-trip of
   every attach and replacement dial. Its format becomes
   `#{pid}|#{start_time}|#{session_id}|#{version}`. `#{version}` is the
   server's own `getversion()`, available since 2.4. `parseIdentity` treats
   the fourth field as optional, so a body without it still parses.
   `remoteIdentity.matches` ignores the version: a tmux upgrade restarts the
   server, so the pid already differs. Once the identity read succeeds (both
   `reattach`/`runConn` and `tryReplace` call sites, and `newSessionPin`),
   the connection calls `SetLiftInBlock(LiftsInBlock(id.version))` on its
   own Reader. That reaches the Reader through `ctlConn`, which keeps a
   reference to it. The reattach and replacement paths use one `ctlConn`
   helper that reads the identity and applies the switch, so a path cannot
   read one without the other. The first attach applies it from
   `newSessionPin`'s recorded identity. A first-attach identity read that
   fails leaves pinning off (#482) and lifting off.

   Before the identity read, the switch is `false`, so every block read before
   it is body. That includes the attach block, which holds `%session-changed`
   on 29bf7fe, `refresh-client -f new-layouts`, the identity read itself, and
   any remote hook block (flags 0), whose text can be pane-derived. A
   pre-identity block on a remote that needs lifting is therefore not lifted
   either. The first capture is sent after the switch is set, so its reply is read
   after it too: the pump reads a reply only after its command was written.

## Behaviour per remote

| remote reports | in-block lifting | forged rows in capture | `%continue` |
|---|---|---|---|
| 3.2–3.7c (bare `-rc` included), 3.8+, next-3.9+ | off | body: kept in snapshot, nothing acted on | body; the daemon reads it from the `refresh-client -A` reply |
| next-3.8, unclassifiable | on (all verbs but `%output`/`%extended-output`/`%pause`/`%continue`) | `%output`, `%pause`, `%continue` body; other verbs still lifted (residual) | body; the daemon reads it from the reply |

## Residual (documented in `bridge-daemon.md`, residual 2)

On a `next-3.8` remote, or one whose version string `LiftsInBlock` cannot
classify, a pane row that parses as a notification verb other than `%output`/
`%extended-output`/`%pause`/`%continue` is still lifted. Examples are a forged
`%window-close`, `%layout-change` and `%session-changed`. The residual also
covers OpenBSD-base remotes, whose version string is unclassifiable. Telling a pre-fix `next-3.8` build from a post-fix
one would take a behaviour probe, not a version string. The cross-pane spoof
and the `%pause`/`%continue` reseed loop are closed on every version, and a
failed identity read leaves lifting off.

## Out of scope

- Residuals (1) and (3) of #860: a socket holder's top-level forgery, and a
  guessed `%end <t> <n> 1`.
- Failures of the over-cap reply in the label/agent polls.

## Tests

- **Reader, default:** a fresh Reader with no `SetLiftInBlock` call keeps the
  same forged block as body (default off). A failed identity read leaves it so.
- **Reader, lifting off:** a `%begin … 1` block holding forged `%output`,
  `%extended-output`, `%window-close`, `%session-changed`, `%layout-change`,
  `%window-add`, `%pause` and `%continue` rows yields one `End` and no other
  line. Its `Data` holds every row verbatim, in order.
- **Reader, lifting on:** the same block lifts every verb except
  `%output`/`%extended-output`/`%pause`/`%continue`, which stay in `Data`. The #276 pin replays the
  29bf7fe wire transcript recorded above. `%window-add @1` inside the
  `new-window` reply and `%session-changed $0 s` inside the flags-0 attach block
  are lifted, ahead of their block's terminal line, and the bodies are empty.
- **`%pause`/`%continue` body:** with lifting on and off, in-block `%pause %N`
  and `%continue %N` rows stay in `Data` and yield no notification.
- **Reader, switch mid-stream:** a `SetLiftInBlock(false)` between two blocks
  changes only the later block.
- **`LiftsInBlock` table:** `3.2a`, `3.3a`, `3.7c`, `3.8`, `3.8-rc3`, `3.8a`,
  `3.3-rc`, `3.7-rc`, `3.8-rc`, `3.10`, `next-3.7`, `next-3.9`, `next-3.10` → false; `next-3.8`, `openbsd-7.9`,
  `master`, `` → true.
- **Identity:** a four-field body parses the version; a three-field body still
  parses, with an empty version; `matches` ignores the version.
- **Daemon:** `handlePause` pauses the sink, round-trips the continue, then
  captures, enqueues the seed and resumes. Over a closed connection it captures
  nothing and leaves the sink paused. `dispatch` ignores `Continue`.
- **Live, pinned next-3.9** (`remote-m2-integration.bats`): a remote pane prints
  `%window-close @<its window>`, `%output %<sibling pane> FORGED` and
  `%session-changed $9 evil` before the daemon attaches, so the initial seed's
  capture carries them. The test asserts:
  - the mirror window survives;
  - the sibling's mirror never shows `FORGED`;
  - the forging pane's mirror shows the three rows literally.

  Before the fix, the same test fails: the mirror window is retired, or the
  sibling's mirror shows `FORGED`. The test asserts both outcomes
  independently. The sibling's sink may not be registered yet when the first
  capture is read, so the window-survives assertion is the primary red signal.
- **Live, second test** (same file): the later-seeded pane prints
  `%output <earlier pane> FORGED`. The first test's sibling leg cannot go red,
  because of the pending-output replay and the seed's clear. Here the forging
  pane is seeded after its target is registered, so the assertion that the
  earlier pane's mirror never shows `FORGED` can fail.
- **Daemon `%error` reply:** `handlePause` on an `%error` reply still reseeds
  and resumes.

## Deliverables beyond code

- `docs/agents/bridge-daemon.md`: correct the #860 paragraph's "3.3a–3.8 emit
  them inside the block" sentence, and rewrite residual (2).
- The PR body carries the per-verb, per-version table above with its sources:
  tmux commits d29aa121, 6db5175e/9228f97d, 89a59d4d, 9b3268a2, tmux issue
  5458, and the `probe.sh` measurements.
