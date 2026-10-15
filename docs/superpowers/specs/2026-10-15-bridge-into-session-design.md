# Bridge `--into`: mirror a remote session's windows into an existing local session (#985)

## Goal

`og-remote-open <host> <remote-session> --into <local-session>` mirrors every
window of `<remote-session>` into an **existing** local session, next to its
local windows and next to other daemons' mirror windows. One local session can
then hold windows from several hosts. The first consumer is the dispatcher,
which shows remote workers' windows in its local session. Each mirror window is
self-describing: everything a consumer needs to identify and drive it is on
the window.

The dedicated-session mode (`og-remote-open <host> <sess> [win]`) keeps its
behaviour. Every change below is either mode-neutral or gated on the new
**placed** mode.

## Terms

- **Dedicated mode**: today's mode. The launcher creates `<host>-<sess>`, the
  daemon owns that session, and teardown kills it.
- **Placed mode**: the new mode. The daemon's windows live in a **host
  session** it did not create and never kills.
- **Own windows**: windows this daemon created. In placed mode the set is
  defined by one marker: the window-scoped `@bridge_sock` equals this daemon's
  socket path. The registry is the in-memory view of the same set. The marker
  also covers windows the registry does not (yet) hold: the rebuild
  placeholder, and a half-built window.

## Why not `link-window`

The dispatcher's design pass rejected linking windows out of a per-remote
mirror session, and the code agrees. Linking gives each window two owners. A
killed mirror session leaves a dead linked copy behind. The M0 spike rejected
link-window for option bleed (`2026-07-16-remote-pseudo-session-bridge-design.md`
:20-30). Creating the windows directly in the host session keeps one owner per
window and lets the dedicated path stay one code path with a different target.

## Option resolution (measured, pinned tmux 9566f6f)

`format_find` (`format.c:4627-4637`) looks an option up in this order:
server global, pane (whose parent chain is window and then global-window),
session, global-session. `format_defaults` fills a missing winlink with
`s->curw`. Consequences:

1. A keybind's `#{@bridge_sock}` runs in the pressed pane's context, so a
   window-scoped `@bridge_sock` wins over any session-scoped one. No keybind
   text changes.
2. `display-message -p -t <session>` and `list-sessions -F` evaluate in the
   session's **current window**. They see a window-scoped option, but only the
   current window's. Measured on a scratch server: `list-sessions -F
   '#{@bridge_host}'` printed `host|` and then `host|remoteA` after
   `select-window` onto a window carrying the option.
3. `show-options -t <sess>` without `-w` is session scope only, so it never
   sees a window option. Measured: it printed an empty value.
4. `list-panes -a -F` and `list-windows -F` evaluate per window. They see each
   window's own copy.
5. A session can be targeted as `$N` and a window as `$N:{end}`. Both resolve
   by id and survive `rename-session` (measured).

Point 2 is the hazard this design must close. Any session-level reader that
infers "this session is a mirror" from `@bridge_host`/`@bridge_session`/
`@bridge_sock` would take a shared session for a mirror whenever its current
window is a placed mirror window. `og-remote-open`'s pair lookup would then
`kill-session` the user's session.

## Window-scoped contract (both modes)

`stampMirrorWindow` stamps these on every mirror window, **before**
`@bridge_win`, so no gate can fire on a window that lacks a socket:

| option | value | modes |
|---|---|---|
| `@bridge_sock` | the daemon's socket path | both |
| `@bridge_host` | raw ssh host | both |
| `@bridge_session` | raw remote session name | both |
| `@bridge_placed` | `1` | placed only |
| `@bridge_win` | `1` (existing) | both |

Dedicated mode keeps its session-scoped `@bridge_sock`/`@bridge_host`/
`@bridge_session` as well. The window copies hold the same values, so every
session-level read in dedicated mode returns the same answer as before.

`@bridge_placed` is the marker a session-level reader uses to ignore a
window's per-mirror values: `#{?#{@bridge_placed},,#{@bridge_host}}`. In a
dedicated session no window carries it. In a shared session it is set on
exactly the windows whose values must not describe the session.

Crew stamps (contract item 3) extend the existing label shipper rather than
adding a new one. Today `windowLabelFormat` carries the remote `@crew_name`
and `@crew_color` into `@bridge_crew_name`/`@bridge_crew_color`
(`windowlabels.go:43`, `:92-93`). The format gains `#{@crew_branch}` and
`#{@crew_id}`, inserted **before** the last two fields because
`@window_label_rest_long` must stay last. Each is wrapped
`#{s/[|]/ /:…}` on the remote like every free-form field. They are stamped as
`@bridge_crew_branch`/`@bridge_crew_id` through `bridgeLabelOptions`, so the
diff, stamp, clear and park/reseed behaviour come for free. Sanitising follows
the identity policy (`cleanLabelValueExact`, drop whole):

- branch: cap 255, `^[A-Za-z0-9][A-Za-z0-9._/+-]*$`;
- id: cap 64, `^[A-Za-z0-9._-]+$`.

Neither admits `#`, `|`, whitespace or a leading `-`. A mirror window already
takes the codename colour the way a local crew window does: every render site
reads `#{?#{@bridge_win},#{@bridge_crew_color},#{@crew_color}}` per window
(`status.go:63-82`, `tmux-reflow-windows.sh:584-586`,
`tmux-apply-theme-colors.sh:125-126`). Placement in a non-mirror session
changes none of those, so no render change is needed. tmux-og stamps no remote
`@crew_branch`/`@crew_id` itself; the dispatcher does. An older dispatcher
stamps neither, and the mirror then carries neither, which is the same
degradation every label field already has.

## Telling a placed window from a local one (owner requirement)

**Programmatic test.** A window is a bridge mirror iff its **window-scoped**
`@bridge_host` is non-empty. The exact test is
`show-options -wqv -t @N @bridge_host`. `#{@bridge_host}` per window in
`list-windows -F` falls back to the session copy, so it is equivalent only
where no session copy exists:

- a placed host session never has one (launcher step 2);
- in a dedicated session every window reads the host, which is also correct,
  since every window there is the mirror's.

The test works in both modes. `@bridge_placed` then says which mode the mirror
runs in. No local window ever carries a window-scoped `@bridge_host`, and no
new marker is needed for this test.

`#{@bridge_win}` is not that test. The daemon sets it last, after
`setupWindow`'s renderer spawn, as the gate's "renderer is up" half. The rebuild
placeholder deliberately lacks it.

**Set before the window is first drawn.** `createMirrorWindow` issues
`new-window -d -P -F '#{window_id}' -a -t <T>:{end}` and, in the same `tmux`
invocation (`;`-joined), `set-option -w -t <T>:{end}` for `@bridge_sock`,
`@bridge_host`, `@bridge_session` and, in placed mode, `@bridge_placed`. One
invocation is one command list: the server runs it to the end before its next
redraw. `after-new-window` hooks are inserted after `new-window`, and one of
them runs synchronously, between `new-window` and the joined `set-option`s:
`after-new-window[30]` is a `set -pF @og_birth_size`
(`config/tmux.conf.tmpl:598`). No hook reads `@bridge_*` synchronously. The
`run-shell -b` hooks (reflow, `tmux-reconcile-window`) run after the list.

The local per-window scripts that opt out of mirrors
(`tmux-reconcile-window`, `tmux-issue-stamp`, `tmux-pr-enrich`) key on
`@bridge_win`, which is set only once the renderer is up. They can therefore
stamp `@issue_*`/`@pr_*` on a fresh placed window from its cwd before it opts
out. That residue is the one dedicated mirrors already carry. Every reader of
a `@bridge_win` window prefers `@bridge_*`, and the enrich card reads only
those (#598). Accepted: switching those opt-outs to `@bridge_host` would
change three scripts' local behaviour for no reader-visible gain. `{end}`
names the window `-a` just appended, since `-a` after the session's last
window makes it the new last one. `stampMirrorWindow` still re-stamps all
four afterwards (idempotent), because the claimed placeholders (dedicated
launcher window, placed rebuild placeholder) were not created by
`createMirrorWindow`. `rebuildPlaceholder` stamps the same four identity
options in its own create command, so the placeholder is marked from birth.

**Visible: host badge on the tab.** A placed window's tab shows
`<@icon_remote> <@bridge_host>` in `@thm_peach`, right after `index: ` and
before the crew badge. That is the glyph and colour `tmux-statusline` already
uses on line 0 for a mirror's host (`picker/statusline/main.go:236-237`).
Only `@bridge_placed` windows get it. A dedicated mirror session already names
its one host on line 0, so a badge on every tab there would be noise.

The badge follows the crew badge's established mechanism (`status-bar.md`,
`tmux-reflow-windows.sh:270-284`, `:623-630`):

- **Global `status-format[1]`** (`config/tmux.conf.tmpl:430` and its
  `tmux.conf.reference.nix` mirror) reads it live:
  `#{?#{@bridge_placed},#[fg=#{@thm_peach}#,bg=#{@thm_bg}]#{@icon_remote} #{@bridge_host} <active/inactive base style>,}`.
- **`tmux-reflow-windows`** adds `#{?#{@bridge_placed},#{@bridge_host},}` to
  its `list-windows` format, in the token-safe region. The value is empty on
  every non-placed window, so a dedicated host holding `|` cannot shift the
  row. Reflow builds `win_host[idx]="<icon> <host> "`, measures it, and
  charges it per window in the single-line fit test and the grid floor,
  exactly like `win_crew`. When floors overflow it is dropped together with the
  agent badge, as decoration ahead of the issue id. It stamps the grid copy as
  `@window_host_disp` and renders it in `ENTRY` before `CREW`.
  `@icon_remote` is read once per reflow pass.
- **Placed host validation.** A placed host must match
  `^[A-Za-z0-9][A-Za-z0-9._@-]*$`: the launcher refuses others before any ssh,
  and the daemon refuses `--into` with such a `--host` at startup. The value
  reaches a local format and a `|`-split row. Dedicated mode is unchanged: it
  never renders the window copy.

**Window picker.** Window rows are read per pane (`windowsFormat` field 28,
`#{@bridge_host}`) and already render a host cell
(`picker/tui.go:3613-3614`), so a placed window's row shows its host with no
code change. A Go test pins this for a mixed session: a local row and a placed
row in one session; only the placed row has the host cell.

## Placed-mode daemon (`--into`)

`cmd/daemon` gains `--into <session-id>` (env `OG_DAEMON_INTO`), the host
session's `$N`. `--local-sess` stays the session **name**; status-file
attribution (`newAgentShipper`) and log lines use it. A new
`Config.LocalTarget` is the `-t` target for every local tmux command that
names the session:

- dedicated mode: `LocalSess`, so the argv is byte-identical to today;
- placed mode: the `$N` id.

Startup pins the host session by id: `pinLocalSession` matches the `--into`
row's `session_id` instead of its name. A failed pin in placed mode is fatal:
the daemon exits non-zero before it dials anything.

### Site dispositions

Each row says what moves to window scope, what follows the host session, and
what is unchanged. "Host session" means `LocalTarget`. In placed mode that is
the session every own window lives in, pinned by id.

| site | dedicated | placed |
|---|---|---|
| `createMirrorWindow` (`daemon.go:258`) `new-window -a -t <sess>:{end}` | unchanged | `-t $N:{end}` |
| `firstMirrorWindow` / startup placeholder (`daemon.go:270`, `:879`) | reuse the launcher's first window | **no placeholder from the session**. Startup uses `cfg.placeholder` when set (only a rebuild sets it), otherwise every window is created. The user's first window is never touched. |
| `dropUnclaimedPlaceholder` (`daemon.go:923`) | unchanged | kills `cfg.placeholder` if unclaimed |
| `stampMirrorWindow` (`daemon.go:310`) | plus window `@bridge_sock`/`_host`/`_session` before `@bridge_win` | same, plus `@bridge_placed`. Also applies the current placed-mode window values (below). |
| `@bridge_sock`/`_host`/`_session` session stamps (`daemon.go:1144`) | unchanged | not written |
| `@bridge_session_path` (`daemon.go:1062`) | unchanged | window-scoped (placed values) |
| `@bridge_state` (`conn.go:820`) | unchanged | window-scoped (placed values) |
| `@bridge_usage` (`agentusage.go:253`, `conn.go:841`) | unchanged | window-scoped (placed values) |
| `@bridge_res` (`sessionres.go:118,149`, `conn.go:834`) | unchanged | **not stamped**. The session picker's CPU/Mem is a per-session figure, and the host session is local. Accepted limit. |
| `@bridge_nudge` + legacy hook unset (`daemon.go:533-550`) | unchanged | one shared per-host-session nudge (below). No `set-hook -u`: those would delete the user's own session hooks. |
| `localArea` → `clientArea` (`cmd/daemon/main.go:512-587`) | unchanged | `list-clients -t $N` (follows the host session) |
| `sessionWinSize` fallback (`main.go:652`) | unchanged | first **own** window's size (own-sock filter over `list-windows -t $N`), not the session's current window, which may be another daemon's |
| `localPaneMap` (`main.go:550`) | own-sock filter (harmless, identical result) | own-sock filter. Remote `%N` ids collide across daemons. |
| `paste.go:535` `bridgeProc` | own-sock filter | own-sock filter |
| `mirrorPaneRows`, `localWindowSet` (`reconcilewindows.go:149`, `localpanes.go:86`) | unchanged | `-t $N`. Keyed on registry window ids, so other windows are ignored. |
| `Reflow` (`main.go:354`, `:955`) | unchanged | `--force <current name of $N>`, resolved per call. Reflow's lock and debounce files are keyed by the name string, and the hook-driven reflows pass the name. |
| `localActiveWindow` (`daemon.go:477`) | unchanged | `-t $N`. It only orders work. |
| `ResolveLocalViewIdentity` (`daemon.go:1099`, `main.go:407`) | unchanged | `-t $N` |
| `watchReveal` / `clientViewsArgs` (`reveal.go:53`) | unchanged | `-t $N`. Reveals are already filtered by `isMirror`. |
| `watchReveal` activity rows → `sizeOwner.noteActivity` (`reveal.go:88-109`) | unchanged | **only rows whose `window_id` is an own window**. Otherwise typing in a local window, or in another daemon's mirror, would claim remote size ownership for every daemon in the session (#979). |
| `localViewing` / park focus edge / reopen notice (`park.go:128`) | unchanged | viewing = some client's current window is an own window. With a session-level test, a user on a local window would read as viewing and the false→true focus edge would never fire. |
| `select-window` on a remote add / `%session-window-changed` / startup `RemoteWindow` (`reconcilewindows.go:72`, `daemon.go:1309`, `:1421`) | unchanged | follow the remote's active window **only when the host session's current window is already an own window**. Startup selects nothing. The user's view is never yanked off a local window. |
| `localSessionGone` (`localpanes.go:117`) | unchanged | `has-session -t $N` (id), same exit-status-1 rule and two strikes. Measured on next-3.9: a live `$0` exits 0, a never-existing `$9` and a killed `$0` both exit 1 (`can't find session`). That is unlike `display-message -t $N`, which exits 0 with empty output (`ownership.go:52-56`). Renaming the host session no longer ends the mirror. |
| `notifyLocal` (`paste.go:570`) and its callers: paste notices, `showReopenNotice` (`reopen.go:226`), the placed tombstone message | unchanged | `list-clients -t $N` (any client of the host session) |
| `showReopenNotice` viewing test | unchanged | the placed `localViewing` (row below) |
| `localWindowGone` (`localpanes.go:71`) via `localWindowSet` | unchanged | `-t $N` |
| `pinLocalSession` / `ownsLocalSession` (`ownership.go:22`, `reopen.go:99`) | unchanged (by name) | pinned by the `--into` id; a failed pin is fatal at startup |
| `setPhase` / `clearPhase` (`phase.go:28`) | unchanged | unchanged. The file is keyed on the socket and nothing reads it (no loading pane). |
| `cfg.LocalSess == ""` guards (`park.go:129-171`, `sessionres.go:110`, `agentusage.go:241`, `conn.go:821-842`, `daemon.go:478,534,546,1144,1277,1347`) | unchanged | unchanged. Placed mode always has a name. |
| `HandOff` (`sessionpin.go`) | unchanged | off. The launcher does not set `OG_DAEMON_REMOTE_OPEN`, so a remote `switch-client` excursion is pinned back and reseeded but never opens a new session or switches the user's client. |
| `newAgentShipper(cfg.LocalSess)` (`daemon.go:1373`) | unchanged | unchanged. It records where the pane lives (the host session's name). |
| `dimMirror` / `undimMirror` (`park.go:156`) | unchanged | unchanged (registry-scoped) |
| `closeWindow`, `retireMirror`, `resetWindow` | unchanged | unchanged (window-id-scoped) |
| teardown (`daemon.go:1180-1290`) | unchanged | see Teardown |
| rebuild / tombstone / `endReopen` (`reopen.go`) | unchanged | see Teardown |
| size ownership (`sizeowner.go`) | unchanged | unchanged. It targets the **remote** pinned session. Only its activity input is filtered (row above). |
| converger, `FitWindowCmd`, `ConvergeCmd` (`size.go`, `mirror.go:17`) | unchanged | unchanged (remote ids and local window ids) |

A window the user moves out of the host session with `move-window` reads as
gone to `localWindowSet` (`list-windows -t $N`). The existing #487 retire path
then kills it in its new session and rebuilds it in the host session. That is
the same contract dedicated mode has: the daemon's windows live in one
session. Tracking a moved window's own session would mean a per-window session
read on every tick, for a gesture no consumer needs. This is the one place the
dispatcher's "follow the session each window actually lives in" is read as
"the one host session the windows are created in and kept in". With a single
host session the two are the same thing everywhere else.

### Placed values (state, usage, session path)

Every per-mirror value that is session-scoped in dedicated mode is written in
placed mode through one small type, `mirrorOpts`, set on `Config` by
`runMirror`:

- `set(opt, val)` / `unset(opt)`:
  - dedicated: today's `set-option [-u] -t <LocalSess> opt`;
  - placed: records the value, then writes it window-scoped to every own
    window in one local `tmux` invocation. The own windows come from one
    `list-windows -t $N -F '#{window_id}|#{@bridge_sock}'` filtered to its
    socket.
- `stampMirrorWindow` applies every recorded value to a new window, so a
  window created while `@bridge_state` is `parked` carries it too.

`setBridgeState`/`clearBridgeState`/`clearBridgeUsage`, the usage flush and
the session-path stamp route through it. `@bridge_res` writes are skipped in
placed mode.

`@bridge_state`'s readers are the statusline and the picker's window rows.
Both resolve through the current window or per pane, so a window-scoped value
reaches them unchanged. `@bridge_usage` and `@bridge_host` are read together
by the statusline (`usage.go:161`): a placed window shows its remote's usage,
and a local window in the same session shows local usage. That is correct per
window.

### Resize nudge (placed)

The nudge has to be a session option. `tmux-reflow-windows` reads it through
`display-message -t "$SESSION"`, which resolves the current window, and the
gated `client-detached` hook resolves the client session's current window. A
window-scoped nudge would therefore reach only the daemon whose window is
current. Placed mode uses one deterministic file per host session:

- path: `<dir of SockPath>/og-nudge-<server pid>-<N>.resize`;
- every placed daemon in that session writes the same value
  (`set-option -t $N @bridge_nudge <path>`, idempotent) and stats the same
  file, with its own mtime baseline;
- the path is not removed at startup, since another daemon may be watching it;
- teardown unsets `@bridge_nudge` only when no other window in `$N` carries
  `@bridge_placed`. A race between two exiting daemons leaves either the
  option set (one cheap `touch` per hook) or a surviving daemon on its 30s
  `resizeFallbackInterval`. Both are benign.

### Teardown (placed)

Placed mode never runs `kill-session`, `resetMirrorSession` or
`tombstoneMirror` on the host session. Every destructive step is gated on the
pin (`cfg.local.check == owned`, server pid + session id). A local server
restart reuses window ids, so an unguarded `kill-window @5` could hit an
unrelated window.

- **Exit** (stop, `%exit`, emptied registry, destroyed pinned remote session,
  refused-attach tombstone ending): kill every own window, i.e. every window in
  `$N` whose window-scoped `@bridge_sock` is ours. That covers the registry,
  a placeholder and a half-built window. Then run the nudge unset rule. A
  tombstone ending also sends one `notifyLocal` message with
  `tombstoneText`/`reopenFailedText`. No tombstone window is left: the
  contract says all of the daemon's windows close when it exits.
- **Rebuild** (`errServerReplaced`): create one `sleep 2147483647` window in
  `$N` stamped with our `@bridge_sock` and `@bridge_placed` (no
  `@bridge_win`), kill every other own window, and carry the new window's id
  into the next run as `cfg.placeholder`. Creating it first means a host
  session made only of our windows never empties mid-rebuild. The next run's
  startup claims it exactly as dedicated mode claims the launcher's window.
  Until it is claimed it has no `@bridge_win`, so `after-new-window` and the
  local pollers treat it as a local window and may stamp `@issue_*`/`@pr_*`
  from its cwd. That is the same residue dedicated mode's launcher window
  already carries. Once `stampMirrorWindow` sets `@bridge_win`, every reader
  prefers `@bridge_*`, and the enrich card reads only those (#598). Accepted:
  the placeholder lives for one rebuild's dial.
- **Host session gone** (`localSessionVanished`): skip every kill. The windows
  went with it.
- **A host session made only of our windows.** The user may have closed every
  local window. tmux then destroys the session when the exit path kills our
  last window. This is accepted. The daemon still never runs `kill-session`,
  and a session holding nothing but this daemon's windows has nothing of the
  user's left to keep. A placeholder left behind would break the contract that
  all of the daemon's windows close when it exits. The rebuild path is
  different: it keeps the session alive with its placeholder, because a new
  mirror follows at once. Pinned by a bats case.

Detach and the picker's kill also act through the socket marker (below), so
"kill exactly the daemon's own windows" has one definition shared by the
daemon and its outside callers.

## Launcher (`og-remote-open … --into <local-session>`)

Argument shape: `og-remote-open <host> <remote-session> --into <local-session>`.
`$3 == --into` takes `$4` as the local session. The following exit non-zero
with a message:

- `--into` together with a window argument, `OG_REMOTE_NEW_DIR` or
  `OG_REMOTE_RESTORE`;
- an empty remote session;
- an empty local session name.

Placed flow, in order:

1. **Local session must exist.** Resolve by exact name from `list-sessions -F
   '#{session_id}|#{session_name}'`, splitting on the first `|` so a name
   holding `|` stays whole. If there is no row, exit 1 (`og-remote-open: no
   local session '<name>'`). This runs before any ssh.
2. **Refuse a dedicated mirror session as host.** If session-scoped
   `show-options -qv -t $N @bridge_host` or `@bridge_sock` is set, exit 1. That
   session's own daemon would kill it on teardown.
3. **Remote probe**, as today: the `sess` canonicalisation and the `win`
   lookup. The cold-start, restore and new-dir branches are not reachable
   (rejected above). A remote session that does not exist has no active
   window, which is today's "session '…' has no window on <host>" exit 1.
4. **Socket key.** `sock="$sock_dir/og-into-<k>.sock"`, where `<k>` is
   `cksum` (POSIX) over host, canonical session, local server pid and `$N`,
   NUL-separated. The id and pid make the key survive a rename of the host
   session and tell apart a recreated namesake. The fixed short prefix keeps
   the path under the 104-byte `AF_UNIX` limit.
5. **Serialise per key.** The daemon listens and writes its pidfile only
   after its ssh dial and first remote reads (`daemon.go:1074-1093`). Until
   then a probe reads it as absent, so two concurrent calls for one key would
   both launch. The launcher therefore takes a per-key lock, `mkdir
   "${sock}.lock"`, before probing. `flock` is absent on macOS; `mkdir` is
   atomic on every platform. The lock directory holds the holder's pid in
   `owner`. A waiter polls every 0.1s for at most 60s, which is above the
   daemon's 30s identity deadline plus its startup reads. A lock is stale when
   its `owner` pid is dead, or when `owner` is still empty after 5s (the
   window between `mkdir` and the `owner` write).
   - The waiter takes over a stale lock by `mv`-ing it to a name unique to
     itself (`${sock}.lock.stale.$$`). That rename is atomic, so exactly one
     of several waiters wins it. The winner removes the moved directory and
     retries `mkdir`.
   - A loser's `mv` fails and it keeps waiting, so it can never remove a
     lock that another waiter has just re-acquired.
   - `on_signal` and every exit path of the holder remove the lock (an `EXIT`
     trap).
6. **Idempotence** (under the lock). `probe_daemon "$sock"` `live` → release,
   exit 0. No `switch-client`, no new window, nothing else touched.
   `outdated` or a recycled pid → reap it and fall through. Stale files → `rm`.
7. **Launch** the daemon detached, exactly as today, with:
   - `OG_DAEMON_LOCAL_SESS=<name>`;
   - `OG_DAEMON_INTO=$N`;
   - `OG_DAEMON_SOCK=$sock`;
   - `OG_BRIDGE_WINDOW=$win`, which the daemon ignores in placed mode for
     selection;
   - no `OG_DAEMON_REMOTE_OPEN`.
8. **Wait for the daemon** (still under the lock): poll every 0.1s, for at
   most the same 60s, until `probe_daemon "$sock"` answers `live` or the
   daemon pid is gone.
   - live → release, exit 0;
   - the daemon exited → release, print the last lines of `${sock}.log`,
     exit 1;
   - timeout → reap the daemon (`reap_daemon`: its own SIGTERM teardown
     removes whatever windows it built), remove its files, release, exit 1
     with a message. Leaving it running would let a retry launch a second
     daemon on the same key before the first one listens.
9. **No** `new-session`, `kill-session`, session stamps, loading pane or
   `switch-client`. On a signal, `on_signal` reaps the daemon (whose teardown
   kills its own windows), removes the socket files and releases the lock. It
   never kills a session.

So exit 0 means a daemon for this key is listening. Its windows may still be
mid-setup, as in dedicated mode. Waiting makes `--into` synchronous, which
dedicated mode is not. Dedicated mode hands the user a loading pane to watch,
while placed mode has no pane of its own until the first window exists. The
caller (the dispatcher) gets a real failure code instead of a daemon that died
unseen, and a second call can never race the first.

Dedicated-mode guards against the point-2 hazard (shared sessions met by
dedicated-mode code paths):

- The pair lookup (`og-remote-open.sh:546-566`) reads
  `#{?#{@bridge_placed},,#{@bridge_host}}`/`…@bridge_session…` in
  `list-sessions -F`, and verifies with **session-scoped** `show-options -qv
  -t <id>` instead of `display-message`. A shared session never matches a
  pair, so it is never adopted, retired or `kill-session`ed.
- `retire_mirror` reads `@bridge_sock` with session-scoped `show-options`.
- The collision walk already uses `show-options` (session scope): unchanged.

## Outside callers

| caller | today | change |
|---|---|---|
| `og-remote-detach` (`prefix + d`) | session's `@bridge_sock` → SIGTERM → wait for the session to vanish → `kill-session` fallback | gains an optional second argument, the window id. The bind passes `#{qs:session_name} #{q:window_id}` (`config/tmux.conf.tmpl:279`, `tmux.conf.reference.nix:876`). It is expanded at the keypress, so the `run-shell -b` script cannot race a window switch. The script reads `#{@bridge_placed}|#{@bridge_sock}` at `-t <window>` (no window given: `-t "$sess:"`, as today). Placed: SIGTERM the pid, wait (2s) until no window server-wide carries that window-scoped `@bridge_sock`, then fall back to `kill-window` on each window that still does. Never `kill-session`. Dedicated: unchanged. |
| status-left session menu (`MouseDown3StatusLeft`, `menus.go:118`, reference `:238`) | `bridgeGate` → mirror session menu (Rename hidden, Detach → `og-remote-detach`, New Window → ctl) | gate becomes `#{&&:<bridgeGate>,#{!:#{@bridge_placed}}}` for these two bindings only. A shared session's pill is the user's own session, so with a placed window current it opens tmux's stock session menu, Rename included: the session is pinned by id, so a rename is safe. Detaching a placed mirror stays on `prefix + d` and the window/pane menus. The mirror session menu's Detach then only ever runs in dedicated mode, so its one-argument call is unchanged. |
| picker `stopBridgeDaemon` (`remote.go:1410`) before a picker session kill | one `display-message -t <sess> @bridge_sock` | SIGTERM every distinct `@bridge_sock` over `list-panes -s -t <sess>`. In dedicated mode that is the one daemon. In a shared session it stops every placed daemon whose windows die with the session. |
| picker session snapshot (`picker/main.go:176-247`), first pane's host/res/path per session | a placed window could label a shared session as a mirror | session-level `bridgeHost`/`bridgeRes`/`bridgeSessionPath` read through the `@bridge_placed` guard. Window rows keep the raw per-pane values, so placed windows show their host there. |
| `collectBridgeSessions` / `collectBridgeMirrors` (#902) (`remote.go:460-510`), `bridgeSessionNames` (`remote_resources.go:329`) | `list-sessions -F` (current window) | guarded with `@bridge_placed`. A shared session is never a mirror target. A remote row whose session also has a placed mirror stays a plain remote row, and opening it creates a dedicated mirror (two controllers on one remote session are supported, #979). |
| `og-remote-theme` | `list-sessions -F '#{@bridge_sock}'` + the first `@bridge_pane` of the session | `list-panes -a -F '#{@bridge_sock}|#{@bridge_pane}'`, one verb per distinct socket, sending the first pane that carries a `@bridge_pane`. Every placed daemon gets the theme. |
| ctl `drag` (`cmd/ctl/main.go:117`) | `--sock` comes from the release pane's context | `resolveDrag` also reads the stashed pane's `#{@bridge_sock}` and dials that when it is non-empty. A drag released over another daemon's window in a shared session would otherwise hit the wrong daemon with a colliding `%N`. |
| keybinds / `bridgeGate` / `bridgeCtl` / carousel `prefix + I` / enrich card `--bridge-sock` | `#{@bridge_sock}` in pane context | unchanged (window copy wins) |
| statusline (`picker/statusline/main.go:52-61`) | `display-message -t <session>` | unchanged. It renders the current window's host/state/usage, which is what is on screen. |
| tmux-remux snapshot (external) | skips sessions carrying `@bridge_host` via `list-sessions -F` | out of scope: a separate repo. With a placed window current, a shared session reads as bridged and is skipped. Otherwise placed windows are saved as ordinary windows. Reported as a follow-up for tmux-remux (key per window on `@bridge_win`). |

## Docs

- `docs/agents/bridge-daemon.md`:
  - rewrite the "Not done … one-local-session ↔ one-remote-session invariant"
    paragraph (:55-57) as the new invariant: one daemon ↔ one remote session
    ↔ one host session; placed vs dedicated; what is window-scoped; the
    `@bridge_placed` session-reader rule;
  - update the `@bridge_host` / `@bridge_nudge` / `@bridge_session_path` /
    `@bridge_state` bullets and the teardown/rebuild/tombstone text for placed
    mode.
- `docs/agents/bridge-shipped-state.md`: window-scoped values in placed mode
  (`@bridge_usage`/`@bridge_state`/`@bridge_session_path`, `@bridge_res`
  absent), "Stored per mirror session" qualified, crew stamps
  (`@bridge_crew_branch`/`@bridge_crew_id`).
- `docs/agents/scripts.md` rows for `og-remote-open`, `og-remote-detach`,
  `og-remote-theme` and `tmux-reflow-windows` (the host badge).
- `docs/agents/status-bar.md`: the host badge, which is charged like the
  agent badge and dropped with it.

## Tests

- **Daemon, placed** (`tests/remote-m2-integration.bats`, `--test-local
  --into <$N>`): DST host session `host` pre-created with one local window
  running a marker command. Covers:
  - remote windows appear as new windows in `host`, and the local window keeps
    its id, command and options;
  - a remote `new-window` appears;
  - a remote `kill-window` removes its mirror;
  - a gated key from a placed window: a real attached client presses a DST
    bind whose body is the production `bridgeGate`/`bridgeCtl` shape, and a
    remote window plus its mirror are created;
  - the statusline host label: `STATUSLINE` run against `host` with a placed
    window current prints the remote host;
  - a client attached to `host` at W×H → the remote window and the mirror at
    the client's content area;
  - SIGTERM → own windows gone, `host` and its local window intact;
  - two placed daemons (two SRC sessions) in one `host` → each window's
    `@bridge_sock` reaches its own daemon (a ctl verb through each lands on the
    right remote), and each daemon's exit removes only its own windows;
  - a `host` holding only placed windows: daemon exit ends the session, and no
    `kill-session` is issued (accepted behaviour, pinned).
- **Launcher** (`tests/mirror-session-name-integration.bats`'s
  real-launcher/fake-daemon harness, or `tests/remote.bats`):
  - missing local session → exit 1 with no ssh;
  - a dedicated mirror as host → exit 1;
  - a second `--into` with a live daemon → exit 0, no second launch, no
    `switch-client`;
  - two concurrent `--into` calls for one key → exactly one daemon launched,
    both exit 0. The fake daemon listens only after a delay;
  - the fake daemon exits at once → the launcher exits 1;
  - `--into` + window / `NEW_DIR` / `RESTORE` → exit 1;
  - the pair lookup ignores a shared session whose current window is placed.
- **Distinguishability**:
  - m2 placed suite: every placed window has a non-empty window-scoped
    `@bridge_host` and the local window has none;
  - a Go unit: `createMirrorWindow` issues `new-window` and the four identity
    `set-option`s in **one** `LocalTmuxOut` call;
  - reflow bats (`tests/reflow.bats` style, fake/scratch server): a session
    with one local and one placed window → the placed window's
    `@window_host_disp` carries `<icon> <host>` and the local one's is empty,
    and the fit math charges it;
  - a conf assertion: `status-format[1]` carries the `@bridge_placed` host
    fragment;
  - a picker Go test: in a mixed session, only the placed window row renders
    the host cell.
- **Go units**:
  - `windowlabels` row parse for the two new crew fields: valid, `#`/`|`/
    leading `-`/over-cap dropped;
  - `parseBridgeMirrors` / picker snapshot guard;
  - activity filter;
  - the `localViewing` placed predicate;
  - the `mirrorOpts` placed write.
- **Regression**: every existing remote suite passes unchanged, since
  dedicated-mode argv is byte-identical except the added window-scoped stamps.
  Existing tests that read `show-options -t host-sess @bridge_*` keep reading
  the session copies dedicated mode still writes.

## Risks

- **Fork count per window creation.** Up to four more `set-option` per mirror
  window (sock, host, session, placed) plus the placed values. Window creation
  is rare and already ~5 forks; this does not touch any per-keystroke or
  per-tick path. These can be one `tmux` invocation joined with `;`, as the
  label stamp already does.
- **The pair-lookup guard** is the one dedicated-mode behaviour change
  outside the daemon. Its regression test is the "shared session is never
  adopted" case.
- **tmux-remux skip** (above) is the one known gap left to another repo.
- **A dedicated window moved into another session.** Its window-scoped
  `@bridge_host`/`@bridge_session` come without `@bridge_placed`, so until the
  daemon's #487 retire path reclaims it (one sweep), a session-level reader
  sees that session as a mirror whenever the moved window is current. The one
  destructive reader is already closed: the pair lookup verifies with
  session-scoped `show-options`, which a window copy cannot satisfy, so
  `og-remote-open` never adopts or kills that session. The picker's session
  row may show the host label for that one sweep. Accepted as transient.
