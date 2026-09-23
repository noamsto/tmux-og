# Bridged popups design (#738): a remote popup never reaches the mirror

## 1. What was reported

`ctrl+r` inside the `halo-toddl` bridge mirror produces **nothing at all** — no
flash, no border, no empty pane, no message pane. The same key in the
`halo-nix-config` mirror draws a history float. The user confirmed it fails in
**every** window of `halo-toddl`, i.e. session-wide, not window-scoped.

The task doc offered two leads: a lost per-control-client `new-layouts` flag on
a reconnect path, and a reply-ordinal desync (#715/#723).

## 2. What the evidence actually says

All of this is read-only observation of the live machine plus scratch-server
repros; nothing in the user's live sessions was mutated.

### 2.1 Lead 1 is refuted; lead 2 is displaced, not disproved

Lead 1 (the `new-layouts` flag lost on a reconnect path):

- `list-clients` on `halo` reports, right now, for **both** sessions:
  `attached,focused,control-mode,new-layouts,pause-after=1,UTF-8`. The flag is
  in force on the broken session's control client.
- `halo-toddl`'s daemon (pid 123990, started 21:49:11) still owns its
  **original** ssh child (pid 124049, started 21:49:12,
  `ControlPath=…-1.sock`). No re-dial has ever happened, so no reconnect path —
  `reattach`, `replaceConn`, or the park/wake cycle added by da43385 — has run
  at all. A flag cannot have been lost on a path that never executed.

Lead 2 (reply-ordinal desync, #715/#723) cannot be refuted from the daemon log:
the whole point of the task doc's note is that such a
desync is **silent** — "nothing anywhere compares `s.seen` against `s.sent`" —
so an empty log is consistent with both a healthy and a desynced stream. It is
displaced instead by positive evidence that the round-trip path is working on
that very daemon:

- `@bridge_res` on `$4` (halo-toddl) advances every ~5 s (`…1790107085` →
  `…1790107090` over an 8 s window). That stamp is written from a reply the
  daemon reads off the same ordinal-counted stream; a desynced stream would not
  keep producing correctly-parsed, monotonically fresh values.
- §2.5's end-to-end repro drives dozens of round-trips (layout reads, float
  surgery, reseeds) over a live stream with no desync.

That is enough to stop chasing lead 2, and not enough to claim it can never
happen — which is why §4 keeps the `s.seen`/`s.sent` check out of scope rather
than declaring it unnecessary.

### 2.2 Float mirroring is healthy for the non-modal (`new-pane`) shape

Reproduced with the real daemon over its `--test-local` seam (two scratch tmux
servers), for the exact command shape `fzf --tmux` uses — `new-pane` issued
**from inside the mirrored pane**, without `-d`, so the float takes remote
focus:

| case | remote float | mirror float |
| --- | --- | --- |
| 1-pane mirror window | `38x8` | `38x8` |
| 3-pane mirror window | `92x14` | `92x14` |

Both mirrored, renderer-backed, daemon log clean. The existing regression
(`tests/remote-m2-integration.bats`, "a remote float is mirrored as a local
float") only covers an **external** `new-pane -d`; the shell-initiated shape is
uncovered but works.

### 2.3 The two sessions differ in what their panes run, not in bridge state

Remote inventory (`list-panes -a` on halo):

- `toddl` — one `bash` pane (the home window) and nine `claude` panes.
- `nix-config` — one `fish` pane.

`FZF_DEFAULT_OPTS` on halo carries `--tmux='80%,60%'`, but it is a **fish
universal variable and is not exported**, so only fish panes get it. Proven on
a scratch server on halo:

- fish + `ctrl+r` → fzf runs `new-pane -P -F '#{pane_id}' -t %0 … sh -c …`
  (captured with a PATH shim; fzf 0.74.4 probes `list-commands new-pane` and
  prefers it over `display-popup`) → a non-modal float.
- bash + `ctrl+r` → readline `(reverse-i-search)` printed **inline**; no float,
  no fzf binding (`~/.bashrc` has none).
- `claude` panes consume `ctrl+r` themselves; it is not a shell widget there.

So none of `halo-toddl`'s **current** panes is one where `ctrl+r` opens a
float. Note the limit of that claim: halo's remote `default-shell` is
`/run/current-system/sw/bin/fish` with an empty `default-command`, so a *new*
window in `toddl` gets fish and would get the float — the bash panes are only
the pre-existing dispatcher-made home windows. The original report said "new
window", the follow-up widened it to every window; §2.5 shows the new-window
case works.

### 2.4 The defect that does exist: a bridged session can open no popup at all

`cmd_display_popup_exec` (upstream `cmd-display-menu.c` at the pinned rev
`3a6c2e78`, function from line 389) still carries, at **lines 416-417**:

```c
if (tc->flags & CLIENT_CONTROL)
    return (CMD_RETURN_NORMAL);
```

`tc` is `cmdq_get_target_client(item)`. A `tmux display-popup` run by a shell
**inside** a session resolves that through `cmd_find_best_client`, which only
considers clients attached to that session. In a bridged session the bridge
daemon's control client is the **only** client, so every such popup is silently
refused: exit status 0, no float, no error, nothing logged anywhere.

Measured on both hosts, tmux `next-3.9`, a session whose only client is a
`tmux -C attach` (script: `repro-738-display-popup.sh`):

```
stock next-3.9 -> floats=0 server_sessions=1   FAIL: the popup was swallowed
```

With **no** client attached the same command errors `no current client`
(status 1) — the silent-success shape is specific to the bridged case.

This contradicts `docs/agents/bridge-daemon.md` head-on, which claims under
#725 that "A remote's `display-popup` opens a modal floating pane on its own
tmux and crosses the bridge exactly like any other float — no special-casing",
and frames the guard as merely meaning "the bridge itself can never originate
one". The guard is not confined to what the daemon originates.

The bail predates the popups-are-floats migration (upstream `34cd5da4`,
tmux-og #725). When a popup was a client-side overlay, one asked for by a
client with no screen took the server down (#346), and `af3e4d2` softened that
crash into a silent no-op. A popup is now an ordinary modal **floating pane in
the target window** — a server-side object with no client overlay — and every
client-dependent input in the placement helper already answers safely for a
control client:

- `status_line_size()` and `status_at_line()` both short-circuit on
  `CLIENT_CONTROL` (`status.c`), so the entire status-line placement block is
  skipped and `sr` stays `NULL`;
- `tty_window_offset()` reads cached zeroes off a tty that was never started —
  which is exactly the window-relative placement a mirrored float wants;
- `cmdq_get_event(item)->m.valid` is false for a command issued by a
  command client rather than a key press, so every mouse-relative format is
  skipped too;
- `tc` is otherwise only `sc.tc` (cwd/environment resolution for `spawn_pane`),
  which a control client answers normally.

Everything else in the function is `w->`/`wp->`-relative.

Blast radius: fzf ≥ 0.74 dodges this by preferring `new-pane`, but `new-pane`
is brand new and undocumented. Older fzf, `fzf-tmux -p`, atuin, lazygit popup
integrations, and any `display-popup` in a user's own remote bindings or
scripts open nothing at all inside a bridged session, with no diagnostic.

### 2.5 The reported gesture works end to end, and so does a modal float

Two further repros, both against the production entry point.

**(a) The user's literal gesture, over real ssh.** A scratch tmux server on
halo (`TMUX_TMPDIR=/tmp/og-738-remote`, never their live server), a fish pane
at 138x36, the real daemon + renderer, and `ctrl+r` pressed **into the local
mirror pane** so the whole path runs (local pane → renderer → daemon → ssh →
remote fish → fzf → `new-pane` float → `%layout-change` → daemon → local
float):

```
input+output path: OK
REMOTE float: 106x21      MIRROR float: 106x21
mirror float screen: the fzf history list, painting live
daemon log: empty
```

So the ctrl+r history float **does** mirror correctly, including into a fresh
fish pane at toddl's geometry.

**(b) A modal float crosses the bridge.** §2.2 is a non-modal float, which does
not cover the thing the fix produces. Measured with a patched tmux on both scratch servers, a
`display-popup -E 'sleep 90'` issued from inside the mirrored pane:

```
REMOTE: %1 sleep floating=1 modal=1 67x16
MIRROR: %1 renderer floating=1 modal=0 67x16
```

— mirrored as a plain local float, which is the documented design (a modal
float's flag is the remote's business; the mirror renders it as an ordinary
float). Closing it (`C-c` into the float) removes it on **both** sides with no
dead pane left behind. Against stock tmux the same script produces no float at
all on either side: that is the red/green pair.

### 2.6 A no-`-E` popup wedges the window's popups, and the mirror cannot clear it

`display-popup` without `-E` sets `remain-on-exit` to 1 and marks the pane
`PANE_CLOSEONCANCEL`. Two measured consequences.

**(a) The mirror cannot dismiss the dead float.** With the patch:

```
REMOTE: %1 echo floating=1 modal=1 dead=1
MIRROR: %1 renderer floating=1 dead=0   (screen still shows "popup-done")
Escape sent into the mirror float -> both sides UNCHANGED
```

The cause is general, not popup-specific: `PANE_CLOSEONCANCEL` is honoured by
tmux's *client* key path, while the daemon delivers input as pane input
(`pumpInput` → `controlmode.SendKeysArgs` → `send-keys -H -t %N`), which a dead
pane ignores. **Any** remote pane that dies with `remain-on-exit` set is
undismissable from the mirror today.

**(b) It wedges every later popup in that window.** `w->modal` is cleared only
in `window_lost_pane` (`window.c:1156`/`1175`), which a pane held alive by
`remain-on-exit` never reaches, and `cmd_display_popup_exec`'s third guard is
`if (w->modal != NULL) return (CMD_RETURN_NORMAL)`. Measured on the patched
build, all three commands issued from the base pane:

```
popup #1 (no -E, `echo one`)  -> %1 floating=1 modal=1 dead=1
popup #2 (-E, sleep 90)       -> nothing; pane set unchanged; exit 0
display-popup -C              -> %1 gone
popup #3 (-E, sleep 90)       -> %2 floating=1 modal=1 dead=0
```

So without a mitigation the patch trades "no popup ever opens" for "the first
popup opens, dies, and then no popup ever opens again **and** a stale float sits
in the mirror".

**The mitigation is one command, and it already works today.** The
`args_has(args, 'C')` block in `cmd_display_popup_exec` is handled **before** the
`CLIENT_CONTROL` bail, so `display-popup -C` — which calls
`server_kill_pane(w->modal)` — is already honoured for a control client, on
stock tmux as well as patched. The daemon can therefore clear a wedged remote
modal float over its existing control stream with no further tmux change. §4.7
makes wiring that in-scope.

`fzf --tmux` is immune to all of this because it explicitly stamps
`remain-on-exit off` on its own float.

## 3. Invariant this change restores

> A `display-popup` issued on the remote host inside a bridged session opens a
> modal floating pane on the remote, exactly as it does for a session with a
> normal client, and the bridge mirrors it as a local float.

That is the invariant `docs/agents/bridge-daemon.md` already documents and that
does not hold.

## 4. Scope

### In scope

1. Carry a tmux patch dropping the obsolete `CLIENT_CONTROL` bail in
   `cmd_display_popup_exec`, wired through `mkTmux`'s `patches` — the mechanism
   the repo already used in `5447f1e` (`patches/` + `patches = (old.patches or
   []) ++ [...]`), with the patch file carrying its own rationale and a
   "drop once upstream takes it" note.
2. **Name the remote-host requirement.** The defect is on the **remote**
   tmux server: `cmd_display_popup_exec` runs where the session lives, so
   patching `mkTmux` changes nothing on `halo` until `halo` is rebuilt from
   this revision. The local mirror server's tmux is irrelevant to it. Add a row
   to `docs/agents/bridge-daemon.md`'s **What the Remote Host Needs on PATH**
   table — the section that already exists precisely to record "this feature
   needs a remote rebuilt from this revision" — saying that a remote popup
   opens nothing at all until the remote's tmux carries this patch, and that
   there is no capability probe, so an unrebuilt remote degrades silently.
3. A regression that drives the **production entry point**: a `display-popup`
   issued from inside a mirrored remote pane, through the real daemon, ending
   in a local floating pane in the mirror window. Red against unpatched tmux
   (no float on the remote at all, so none in the mirror), green against the
   fix. It runs on `mkTmux` for both scratch servers, which is already what
   every live-tmux check in `flake.nix` uses.
4. Update `tests/tmux-next38-readiness.bats`'s "display-popup is refused for an
   attached control client" case, which pins the *old* behaviour and will
   otherwise fail. It becomes the assertion that a control client's
   `display-popup` opens a modal float in that client's current window **and
   the server survives** — the #346 property that actually mattered.
5. Correct the #725 paragraph in `docs/agents/bridge-daemon.md`.
   `docs/agents/floats.md` describes the compat command but never claims it
   works over the bridge, so it needs no change.
6. Add the missing coverage for the shell-initiated `new-pane` float — the
   shape `fzf --tmux` actually uses, and the one this issue is literally about.
   Nothing tests it today; §2.2 and §2.5(a) show it works, and a test keeps it
   working.
7. **Ship the §2.6 mitigation with the patch, not after it.** The wedge in
   §2.6(b) makes the patch net-negative on its own, so the daemon must be able
   to clear a wedged remote modal float: when one of tmux's own cancel keys
   (`Escape`, `C-c` — `server-client.c`'s `PANE_CLOSEONCANCEL` rule) is
   forwarded for a mirrored pane that is its window's dead modal pane, send
   `display-popup -C` for it over the control stream, as
   `if -F -t %N '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %N'`
   sent alongside the keystroke, gated to a lone cancel key so no ordinary
   keypress pays for it. Covered by a regression
   that a no-`-E` remote popup can be dismissed from the mirror and that a
   later popup in the same window still opens.
8. **File the general limitation as a follow-up.** §2.6(a)'s root cause —
   any dead remote pane with `remain-on-exit` is undismissable from the mirror,
   because the daemon delivers input as pane input rather than through a client
   key path — is broader than popups and is not fixed here. One issue, linked
   from the PR.

9. The patch also skips `wait_item` when the item's queuing client is a control
   client (found in review): past the dropped bail, `CMD_RETURN_WAIT` would park
   that control client's whole queue on the popup's lifetime, wedging the bridge
   when a remote hook opens a popup off a daemon command. `-E` therefore does not
   order a control client's queue; a command client still waits.

### Out of scope

- The `s.seen`/`s.sent` ordinal-desync diagnostic the task doc asks for
  conditionally ("If the cause is a lost per-connection flag or a desynced
  counter"). Neither is the cause (§2.1), so building it here would be
  unrelated bridge work, which the task doc lists as out of scope. It
  is a real gap and stays worth doing — flag it in the PR so it is not lost.
- The general repair for §2.6(a) — any dead remote pane with
  `remain-on-exit` being undismissable from the mirror — is filed, not built
  (§4.8). Only the popup-specific `display-popup -C` path is built here.
- Anything about the float bind vocabulary, reflow, or the status bar.
- Changing what `ctrl+r` is bound to on the remote host — not this repo's.

## 5. Acceptance criteria

- [ ] Root cause named and demonstrated: the `CLIENT_CONTROL` bail, shown
      silently swallowing a popup in a bridged session, and shown fixed.
- [ ] A remote `display-popup` inside a bridged session opens a **modal** float
      on the remote and the mirror gains a matching local float; closing it
      with `-E` leaves nothing behind on either side.
- [ ] Regression test red against the old behaviour (evidence recorded:
      commands, revisions, observed failure), green against the fix.
- [ ] `docs/agents/bridge-daemon.md`'s remote-host table names the
      requirement that the **remote** be rebuilt from this revision, and says
      there is no probe, so an unrebuilt remote degrades silently.
- [ ] A no-`-E` remote popup can be dismissed from the mirror, and a
      later `display-popup` in that same remote window still opens — i.e. the
      §2.6(b) wedge is not shippable state.
- [ ] The general dead-pane limitation (§2.6(a)) is filed as a
      follow-up issue and linked from the PR.
- [ ] `tests/tmux-next38-readiness.bats` reflects the new behaviour and still
      pins that the server survives.
- [ ] The #725 paragraph in `docs/agents/bridge-daemon.md` is corrected.
- [ ] The shell-initiated `new-pane` float path has a regression.
- [ ] PR splits the report honestly rather than claiming it is solved:
      §2.3 accounts for the *every-window* form (all nine other toddl panes run
      `claude`; the home window is `bash` with no fzf binding), the *new-window*
      form is **unexplained** — halo's remote `default-shell` is fish, so a new
      toddl window would get the float, and §2.5(a) shows a fresh session on
      that path working — and #738's literal symptom therefore stays
      unreproduced. The PR must say so and leave the issue's disposition to the
      user rather than asserting it is answered.
- [ ] Local gate green: `nix build .#default`, `nix flake check`,
      `nix build .#lint`.

## 6. Risks

- **The fix is inert until the remote is rebuilt.** Nothing in this PR
  changes `halo`'s behaviour on merge; the user must rebuild the remote. There
  is no capability probe for it (the same posture as remote session resources
  and remote window labels, both of which degrade silently and say so in the
  table). Mitigated only by documenting it — a probe would mean a round-trip
  per connect for a feature the daemon never invokes itself.
- **Without §4.7 the patch is net-negative.** A no-`-E` popup opens once,
  dies, wedges every later popup in that remote window (`w->modal` never
  cleared) and leaves a stale float in the mirror — strictly worse than today's
  "nothing ever opens". This is why the `display-popup -C` path ships with the
  patch instead of trailing it.
- **The patch changes behaviour for a popup the daemon itself sends.** Nothing
  in the daemon calls `display-popup`, so no code path changes; the readiness
  test is the only thing pinning it and is updated deliberately. The #346
  property (the server surviving) is measured in both repros and holds.
- **Upstream divergence.** The repo deliberately dropped its tmux fork once its
  fixes landed upstream. This patch is two deleted lines, carries its rationale,
  and is marked "drop once upstream takes it", matching `5447f1e`'s posture.
- **This does not reproduce the user's literal `ctrl+r`.** §2.5(a) shows that
  path working over real ssh. Stated openly rather than hidden; the user may
  still want the environment answer instead of, or as well as, this fix, which
  is why the PR must carry §2.3 and §2.5.
