# Dismissing a dead remote pane from its mirror (#748)

## 1. Problem

The bridge daemon delivers every mirror keystroke as pane input:
`pumpInput` → `controlmode.SendKeysArgs` → `send-keys -H -t %N`. tmux's rules
that dismiss a dead pane run only on the *client* key path, so no key pressed in
a mirror reaches them. #746 (#738) patched one case — a dead modal popup opened
without `-E` — with a remote-guarded `display-popup -C` after a lone Escape or
`C-c`. A dead tiled pane and a dead non-modal float are still unreachable.

## 2. tmux's own local semantics at the pinned rev

Pinned tmux (`flake.nix` input `tmux-upstream`, `3a6c2e78`), `server-client.c`.
A normal client's key on the active pane of its current window goes through
two dead-pane rules, both before the key is delivered:

1. **Dead-key rule** — `server_client_handle_dead_key` (line 1278), called from
   the fast path (line 1653) and again at `forward_key` (line 1556). It fires
   when **all** hold:
   - the pane has `PANE_EXITED`;
   - the key is neither a mouse key (`KEYC_IS_MOUSE`) nor a paste key
     (`KEYC_IS_PASTE`);
   - the pane's `remain-on-exit` is `key` (3) or `failed-key` (4).

   It then sets `remain-on-exit off` and calls `server_destroy_pane`, which
   removes the pane. The key is consumed.
2. **Cancel rule** — the modal pane with `PANE_CLOSEONCANCEL` is killed on
   Escape or `C-c`, dead or live (lines 1655-1661). #746 already mirrors the
   dead half of this.

With `remain-on-exit on` (1) or `failed` (2), **the dead-key rule does not
fire.** The one exception is a dead no-`-E` popup, which has `on`
(`cmd-display-menu.c:523`) and is closed by its cancel key (rule 2). The key falls through to `window_pane_key`, which returns at
`wp->fd == -1` (`window.c:2048`). The pane stays until an explicit command
removes it: `kill-pane`, for which tmux-og's `prefix + x` confirms first, or
`respawn-pane`.

Which panes get `key`/`failed-key` at the pinned rev:

- `split-window`/`new-pane` with `-k` or `-m` (`cmd-split-window.c:257-263`).
  This covers tiled panes and floats (`-O`/`new-pane`).
- `display-popup -k` → `key` (3); `display-popup -EE -k` → `failed-key` (4)
  (`cmd-display-menu.c:512-521`).
- Any pane where the user sets `remain-on-exit key|failed-key`.

## 3. What the mirror already does

- **`prefix + x` on a mirror pane already dismisses the remote pane.** The bind
  at `config/tmux.conf.tmpl:228` is gated on `#{&&:#{@bridge_win},#{@bridge_pane}}`.
  It confirms, then runs the ctl `kill-pane` verb (`ctl.go`, `kill-pane -t %N` on
  the remote). The daemon stamps `@bridge_pane` on every renderer pane, tiled or
  float (`daemon.go:1986`). The renderer pane stays live locally even when the
  remote pane is dead, so the gate passes. `kill-pane` on the remote works on a
  dead pane like on a live one. This is exactly the local gesture for a dead
  `remain-on-exit on` pane, and the pane's death does not affect it.
- The one daemon-side dismissal is #746's modal clear.

So the part the issue names that is actually broken, measured against tmux, is
the **dead-key rule**: a dead `key`/`failed-key` pane, tiled or float, that any
key would close locally ignores every key in the mirror.

## 4. Options

- **(a) Map a key to a remote-guarded `kill-pane` — chosen.** On a key frame,
  `pumpInput` first sends
  `if -F -t %N '<dead-key predicate>' 'kill-pane -t %N'`, then the frame's
  `send-keys`. The predicate is evaluated **on the remote**, against the pane's
  current state, so it is authoritative and never reads a local cache:
  `#{&&:#{pane_dead},#{||:#{==:#{remain-on-exit},key},#{==:#{remain-on-exit},failed-key}}}`.
  `#{remain-on-exit}` resolves through the pane's options with inheritance
  (`format.c:4621-4631`, `options_get`). Checked on a scratch server running
  the pinned tmux:
  - dead tiled `-k` pane: killed;
  - dead float `new-pane -k`: killed;
  - dead `failed-key` pane: killed;
  - live pane: survives;
  - dead `on` tiled pane and dead `on` float: both survive.

  A remote whose tmux predates `key` reports `on`/`off`/`failed`, so the guard
  is false there and the pane is never killed.
- **(b) Route the key through a client-key path (`send-keys -K`) — rejected.**
  `-K` calls `server_client_handle_key` on a client (`cmd-send-keys.c:75-87`).
  Both of tmux's rules then apply to `s->curw->window->active`, the active pane
  of the control client's *current remote window*, not the `%N` the user typed
  into. The daemon mirrors every window of the session, so a key typed into a
  mirror of a non-current window would test, and could destroy, a different
  pane. It also runs every forwarded key through the remote's key tables. Root
  bindings on the remote (tmux-og binds `M-arrows` there) would fire instead of
  reaching the pane, after the local server already processed the key once.
- **(c) Reflect deadness into the mirror — rejected.** Mirroring deadness would
  mean making the local renderer pane dead, or keying local binds on a copied
  flag. Either way the destructive decision would rest on a local copy that can
  lag the remote, which the task forbids. It also needs a deadness notification
  path the control protocol does not give us: there is no `%pane-died`. A
  renderer pane that is dead on purpose collides with `healDeadRenderers`, which
  rebuilds exactly those (#547/#657).

## 5. Design

### 5.1 Which frames count as a key

tmux excludes mouse keys and the paste *markers* from the dead-key rule.
`KEYC_IS_PASTE` (`tmux.h:228-231`) matches only `KEYC_PASTE_START`/`END`. The
fast path (line 1653) runs before the bracket-paste diversion (line 1369), so a
terminal paste, bracketed or not, dismisses on its first content key. A frame is
the raw bytes local tmux wrote to the renderer's pty, so the daemon classifies
bytes. It scans the **whole** frame, skips every non-key report below wherever it
sits, and treats the frame as a dismissal key iff any byte is left:

- **mouse reports** — SGR `ESC [ < … M|m` (1006), and `ESC [ M` plus three
  parameters (X10, or UTF-8 1005, where each parameter is one or two bytes,
  `input-keys.c:756-772`). tmux's URXVT (1015) encoding is not listed, because
  local tmux never emits it to a pane. A mirror of a pane whose program died
  with mouse mode on keeps that mode, since the remote leaves the screen modes
  set, so local tmux still forwards clicks there. A click that selects the pane
  must not kill it.
- **focus reports** — `ESC [ I` / `ESC [ O`. This one is a bridge-only choice.
  Locally a dead pane never receives a pane focus report: `window.c:685` checks
  `PANE_EXITED`, and the writes are at `window.c:703-709`. So a focus report
  that reaches the daemon is local tmux's own pane-focus notification, not a
  key.
- **bracketed-paste markers** — `ESC [ 200 ~` / `ESC [ 201 ~`. The pasted body
  counts, as it does locally.

The scan has to cover the whole frame. A click that focuses a mirror pane
flushes the focus-in and the mouse press in one write, so one frame reads
`ESC [ I ESC [ < … M`. A check on the frame's first bytes only would take that
click for a key. (Found in review.)

Everything else counts, the same as tmux counts it: printable text, control
characters, escape-prefixed keys (arrows, function keys, `M-x`) and UTF-8.

**`paste-buffer` diverges (accepted).** Locally, `paste-buffer` (`prefix + ]`)
writes to the pane directly and never dismisses. In the mirror its bytes reach
the daemon like any typed frame, so it dismisses a dead `key` pane. At the byte
level the daemon cannot tell it from a terminal paste, which does dismiss
locally. The paste could not have reached the dead pane either way.

**Keys local tmux consumes never reach the daemon.** Locally the dead-key rule
runs in the fast path (line 1653), *before* key-table lookup, so the prefix key
and root-bound keys (tmux-og's `M-arrows`) also dismiss a dead `key` pane. In a
mirror the local renderer pane is live, so local tmux handles those keys itself
and no frame is produced. `prefix + x` still dismisses through the ctl verb.

**A multi-key frame is consumed whole.** Locally the dismissal consumes only
the first key, and later keys go to whichever pane is active next. Here the
whole frame's `send-keys` targets the removed `%N` and is dropped.

All three divergences are recorded in the docs. None of them can reach a live
pane, because the guard is evaluated on the remote.

The classification runs on the payload after `pasteHandler.handle`, the bytes
actually delivered. When that is empty (an image paste in flight), nothing is
sent, guard included.

### 5.2 Ordering: guard before delivery

tmux checks deadness **before** delivering the key. The daemon sends the guard
**before** the frame's `send-keys`, so the order matches. That order also
closes a race that guard-after would open. Take a `-k` pane running a shell: the
user types `exit` and presses Enter. The Enter frame's `send-keys` exits the
shell. A guard sent after it can land once the shell has died, and would kill
the pane on the very keystroke that killed its process. The user would never
see the dead pane the `-k` flag exists to show. Locally that cannot happen,
because the Enter is checked while the pane is still live. With guard-before,
the guard for a keystroke is evaluated before that keystroke is delivered.

When the guard does kill the pane, the frame's `send-keys` then targets a
missing pane and draws an `%error` reply. Fire-and-forget replies are claimed
and discarded. This already happens whenever someone types into a pane at the
moment it closes, and after every #746 clear.

### 5.3 Interaction with #746's modal clear

The #746 clear stays exactly as it is: after `send-keys`, on a lone Escape or
`C-c`, guarded by `pane_dead && pane_modal_flag`. tmux's fast path runs the
dead-key rule, then the cancel kill, then delivery. The daemon sends the
dead-key guard, then `send-keys`, then the modal clear. The two orders are
equivalent only because the clear is gated on `pane_dead`, and input to a dead
pane is dropped. The two predicates only overlap on a dead modal
`-k` popup. The dead-key guard removes that pane first, and the modal clear's
`if -F -t %N` then errors harmlessly.

### 5.4 Cost

The guard costs one more `if -F` plus its `og-fanout` barrier per key frame
(`stampAll` barriers every command). That is two small commands per human
keystroke, fire-and-forget and pipelined, so no round-trip is added to the
typing path. The docs already weigh one barrier per command as negligible
against a `%output` stream measured in megabytes. Frames holding only mouse
reports, focus reports or paste markers pay nothing.

### 5.5 What does not change

- `remain-on-exit on`/`failed` dead panes, tiled or float: no plain key
  dismisses them, as locally. `prefix + x` does, through the existing ctl
  `kill-pane` verb. A regression test pins that path for a dead pane.
- A live pane: its key frames get only a guard that evaluates false on the
  remote, so the pane receives the key unchanged. Nothing is killed and no
  popup is cleared.
- The local renderer corpse rule (`remain-on-exit on` on every mirror window,
  #547) and `healDeadRenderers`. These concern a dead **local renderer**, which
  is a different object from a dead **remote** pane whose renderer is alive.
- Menus (#782): this design adds no menu item. If #782 routes a "Kill pane"
  menu entry to the ctl verb, that entry dismisses a dead pane too, with no
  further work.

## 6. Acceptance mapping

| Acceptance item | How it is met |
|---|---|
| Dead tiled pane dismissable with the local key(s) | `key`/`failed-key`: any key, via the new guard. `on`: `prefix + x`, via the existing ctl verb. Matches §2. |
| Dead non-modal float, same | The same guard. The predicate does not care whether the pane floats. Scratch check in §4(a). |
| Live pane gets the key as input | Guard false on the remote; `send-keys` unchanged. |
| Go tests: dead-tiled, dead-float, live | A pure test for the command string and the frame classifier. A `pumpInput` send-order test. A live-tmux test that pipes `pumpInput`'s output into a real control client on a scratch server and asserts the dead-tiled and dead-float `key` panes are removed while a live pane and a dead `on` pane stay. |
| Scratch-server repro in the PR body | Bridge integration test (`tests/remote-m2-integration.bats`): a dead `-k` tiled pane and a dead `-k` float, each dismissed by a key sent into the mirror; a live pane and a dead `on` pane survive a key. The PR body records the manual `tmux -L probe` repro. |
| `docs/agents/bridge-daemon.md` | New rule next to the #746 paragraph. |
