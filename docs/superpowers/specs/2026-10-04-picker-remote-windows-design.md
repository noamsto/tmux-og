# Remote windows in the window picker (#902) — design

## Problem

The window picker (`prefix + w`, `picker --windows`) lists only local windows.
The session picker has a Remote section: host rows, cached-then-probed remote
sessions, attach through `og-remote-open` with spinner/cancel/timeouts, auth for
a host that needs a prompt, a y/N-confirmed remote kill, and a Tab host-scope
cycle. None of this runs in window mode — every remote path is gated on
`!m.windowMode` — so a remote host's individual windows cannot be browsed or
opened from the window picker. Windows of a remote session that is *already*
mirrored do show up, as ordinary local mirror windows; windows of an
unmirrored remote session are invisible.

## Goal

Window-mode parity with the session picker's remote reach: list and search
every configured host's windows, open one with the session picker's attach
behaviour, kill one with a y/N confirm, and scope by host with Tab — without
slowing the window picker's first paint.

## Decisions

### D1. Row shape: host row, then one flat row per remote window

The Remote section in window mode is the session picker's section with window
rows in place of session rows:

```
── Remote ─────────────────────────
 devbox
├─ api  1: server
├─ api  2: logs
╰─ web  1: vite
 laptop  (unreachable — open default)
```

- The **host row** is the session picker's host row, unchanged: same notes
  (`(unreachable — open default)`, `(auth needed — Enter to connect)`,
  `(host key changed — verify manually)`, `(tailscale check — run: ssh …)`,
  `(no server — Enter starts one)`, `(all open)`), the same flags
  (`remoteNeedsAuth`, `remoteInert`, `remoteTailscaleCheck`) and the same Enter.
- Each **window row** is a tree child of its host: tree glyph in the host tint,
  the remote session name dimmed, then `<index>: <name>`. Its `searchText` is
  `<host>/<sess>:<index> <host> <sess> <name>` (sanitized names, D7), so a
  query matches by host, session, window name, or the `host/sess:index` path. Flat, not a
  host → session → window tree: one level of tree keeps `markRemoteTreeEnds`,
  the filter's host-context pull-in and `remoteHostBlock` working unchanged
  (they key on `remoteHost`/`remoteSess`, which a window row also sets), and the
  session name in every row keeps a filtered list readable without its parent.
- Local window rows keep their own grouping (by session, or by agent state with
  `ctrl+g`); the Remote section always follows them, exactly as in session mode.
  A remote row never joins a local group — not even the state-grouped "no
  agent" group, whose key is `""` like an ungrouped remote row's.
- Windows of a remote session already mirrored locally are **not** listed as
  remote rows: the mirror's windows are already in the list as local rows with
  `bridgeHost` set, and Enter there is a local `switch-client`. This is the
  window-mode form of `remoteSessionsForHost`'s bridged-session dedup.

### D2. Data: a window probe and a window cache, separate from the session ones

- **Probe.** One ssh round trip per host, same bounds as the session probe
  (`remoteProbeTimeout` 3 s, `BatchMode=yes`, `ConnectTimeout=2`), same
  identity preamble (self-alias detection), same error classification
  (`classifyProbeErr`). Its tmux body is one chained command:

  ```
  list-sessions -F 'S|#{session_id}|#{session_name}' \; list-windows -a -F 'W|#{session_id}|#{window_id}|#{window_index}|#{window_name}'
  ```

  run through `remoteTmuxCmd` (TMUX_TMPDIR legs, fish-safe; `\;` was checked
  to parse as a literal `;` argument in both fish and bash). Two free-text
  fields (session name, window name) cannot share one `|`-delimited line, so
  each sits **last** on its own line kind and the window line joins its
  session by `session_id`. A parser keeps only lines matching
  `S|$<n>|<name>` and `W|$<n>|@<n>|<n>|<name>` and drops a `W` line whose
  session id has no `S` line. No `S` lines at all is "no server", like an
  empty session list today.
- **Why not extend `--remote-pick` to emit windows.** That path runs the picker
  on the remote inside an ssh pty (`^o`); it is a second interactive picker,
  not a listing. The in-list probe gives every host's windows in one popup,
  searchable together, which is the reach the session picker has. `--remote-pick
  --windows` stays rejected and `^o` is unchanged (non-goal).
- **Why a separate probe, not one probe for both modes.** The session picker's
  probe, its cache file and their consumers stay byte-for-byte unchanged, so
  this change cannot regress the session picker's Remote section. The cost is a
  second probe kind; the session picker never runs it.
- **Cache.** `$XDG_CACHE_HOME/tmux-og/remote-windows/<hostFileName>.json`, a
  sibling of the session cache dir, with the session cache's trust rules
  (owner-only dir, regular file owned by the uid, host field must match,
  write-to-temp + rename). It holds `{Host, SavedAt, Windows: [{Session,
  SessionID, ID, Index, Name}]}`. Staleness is `remoteCacheStaleAfter`, the
  session cache's threshold. Written on every probe that answers (`OK` and
  `NoServer`, the latter empty), kept on `Unreachable`, never shown for the
  auth / host-key / tailscale states — the session cache's exact policy.

### D3. First paint is cache-only; the probe is async (cached-then-probed)

`newPickerModel` builds, in window mode too, the Remote section from
`@remote_bridge_hosts` and the window cache alone — no ssh, no tmux fork —
with the pending `…` note on each host row (`pendingRemoteItems`' window
twin). Mirrored sessions are excluded from the cached rows by the same
`firstPaintBridges(items)` session mode uses: window rows carry the owning
`session` and `bridgeHost`, so the legacy-key index it builds is the same. The
async probe uses `collectBridgeSessions()` (the authoritative pair keys), as
session mode's `remoteCmd` does. `Init` starts the window probe as a `tea.Cmd` alongside the existing
ones; `remoteMsg` replaces the section wholesale, re-finding the cursor by
target as it does today. The 1 s refresh never re-probes and never touches the
remote rows. Emit mode cannot be window mode, so it is unaffected.

Measured with `tests/perf/picker-open-latency.sh window` before and after, on
the stock fixture and on the fixture with two configured hosts whose window
caches are seeded (the hosts need not resolve: the probe is off the
first-paint path, and the cache read is what the first paint pays for). The
script gains an opt-in `REMOTE_HOSTS=<n>` knob that sets `@remote_bridge_hosts`
on the scratch server to `n` unresolvable host aliases and seeds a window cache
for each under the fixture's `XDG_CACHE_HOME` (the fixture already isolates
`HOME`). The results go in `performance.md`.

### D4. Enter on a remote window opens the session focused on that window

Enter on a window row runs the session picker's supervised attach
(`beginAttach`: off-Update fork, phase pipe, spinner, per-phase timeouts, Esc /
C-c cancel with rollback, `— enter to retry` on failure) with one addition: the
attach spec carries the window **index**, passed as `og-remote-open`'s existing
third argument. The launcher forwards it as `OG_BRIDGE_WINDOW`, and the daemon
already selects the mirror window for that remote index at startup
(`localWinForRemoteIndex`). Status/hint labels read `<host>/<sess>:<index>`.

**Launcher fix: a caller-given index must not skip the gone-session guard.**
Today `og-remote-open host sess win` with a non-empty `win` skips the probe's
active-window lookup and the `[[ -z $win ]]` block — the launcher's only "session
has no window — it is gone" exit — so a stale row (a cached first-paint row, or a
session killed after the probe) would launch a daemon and switch the client into
a loading mirror of nothing, exit 0, and the picker would report success. The
launcher changes so that, on a plain live-session open (neither
`OG_REMOTE_RESTORE` nor `OG_REMOTE_NEW_DIR`), the probe always resolves the
session's active window and also reports whether the caller's index exists in
that session:

- active window empty → the session is gone: exit 1 with the existing
  `session '<sess>' has no window on <host> — it is gone or was never there`
  message (the picker shows it with `— enter to retry`, as for a session row);
- caller's index present → use it;
- caller's index absent (the window closed, or `renumber-windows` slid it) →
  fall back to the session's active window rather than fail: the session is
  there, and the index was only a focus hint.

With an index given, the probe also skips its unique-prefix canonicalisation
of the session name (exact `has-session -t =<sess>` only): a window row names
an exact session, and resolving a gone session to a prefix-sharing sibling
would put the index on the wrong session's windows.

Restore and new-dir opens keep their current handling of a caller index (the
picker never passes one there). `tests/remote-cold-start.bats` (the launcher's ssh-double
suite, which drives the probe script) gains the gone-session-with-index, index-present and index-absent cases,
and `docs/agents/scripts.md`'s `og-remote-open` row documents the third
argument's contract.

- **Existing mirror.** Rows for mirrored sessions are not built (D1), so the
  common case never reaches the launcher. A mirror opened *after* the probe
  takes the launcher's dedup path: it switches to the live mirror session and
  ignores the window index. Accepted residual (noted in `picker.md`): the client
  lands on the right session, on its current window. Mapping a remote index to
  a local mirror window would need the daemon's registry, which the launcher
  does not have.
- Host rows keep their session-picker Enter in window mode: open the default
  session, run `og-remote-auth` for a needs-auth host (then re-probe — the
  window probe in window mode), refuse for host-key-changed and tailscale-check
  hosts with the same status text.
- Remote window rows are **not** `^t`-markable (non-goal: multi-open is a
  session-picker feature; opening N windows means N mirrors).

### D5. `^x` on a remote window kills that window on the remote, y/N confirmed

Window mode's local `^x` kills a window, so its remote `^x` kills a window too.
It reuses the session picker's whole remote-kill flow: stage, the inline
`kill <host>/<sess>:<index> <name> on the remote?  (y/N)` prompt (only `y`/`Y`
acts, default No, sized with `visibleWidth`), the off-Update `killRun` with
cancel, the result classification and the forget-at-once on success / gone.

- **Remote command:** `kill-window -t '=<sess>:@<id>'` through `remoteTmuxCmd`,
  the target single-quoted with `shellQuote`. `=` makes the session part an
  exact name match; `@<id>` resolves only when that window is linked into that
  session — on next-3.9 `kill-window -t '=a:@2'` for an `@2` in session `b`
  fails `can't find window: @2`, exit 1 — so a window that moved, or an id the
  server never had, is "already gone" (exit 1 → `errRemoteSessionGone`'s
  window-worded twin), never a different window killed.
- **Live rows only.** Window ids are unique only within one server lifetime; a
  cached row may carry ids from a server that has since restarted, where `@3`
  can name an unrelated window of a same-named, restored session. So `^x`
  stages a kill only for a row the probe returned in this popup (a new
  `remoteLive` flag). On a cached row it stages nothing and the hint says the
  row is from the cache and to wait for the probe. The session picker's
  session-kill rule (names, cached rows allowed) is unchanged.
- **Forget.** A killed or already-gone window is dropped from the in-memory
  rows and from the window cache (preserving `SavedAt`), and recorded in the
  popup's forgotten set so a late `remoteMsg` cannot revive it. The forgotten
  key gains the window id for window rows; session-row keys are unchanged.
- **Last window.** Killing a session's last window kills the session on the
  remote. When a window kill succeeds (or finds it gone) and no other window
  row of that host+session remains in `m.remoteItems` (the unscoped,
  unfiltered remote rows — never the filtered view), the session is also dropped from the host's
  *session* cache (`forgetRemoteSessionCache`), so the session picker's next
  first paint does not show a dead cached session.
- **Session-picker kill keeps the window cache honest.** Killing a remote
  session from the session picker also drops that session's windows from the
  host's window cache, so the next window-picker first paint does not resurrect
  them from cache.

### D6. Tab cycles the host scope in window mode too

`local → host₁ → … → hostN → all hosts → local`, the same `nextScope` and
`configuredHosts` order. Window-mode scoping is simpler than session mode's
because mirror windows are already local rows:

- **local scope:** today's window list plus the Remote section (the whole
  section, as in session-mode local scope).
- **host scope:** local window rows with `bridgeHost == host` (the mirror
  windows of that host) under their group headers, then that host's Remote
  block verbatim (`remoteHostBlock`). No synthesized `(mirrored)` rows — the
  mirror windows themselves are the mirrored entries.
- **all hosts:** the same, for every configured host, host block by host block.

The search row shows the scope badge, and the footer shows `⇥:scope` in window
mode whenever hosts are configured (highlighted off local scope). The
`collectBridgeMirrors` fork stays session-mode only.

### D7. Remote strings are sanitized before they render

Window and session names come from the remote. Window names in particular are
settable by any program the remote runs. Before a remote string reaches a row's
`display`/`plain`/`searchText`, the cache, or a status/prompt line, it goes
through `sanitizeStatusText` (ESC sequences, C0/C1, bidi overrides dropped,
whitespace collapsed, 200-rune cap), then the 40-cell `truncateCells` window
rows already use. The **raw** session name is kept separately for the actions:
`og-remote-open`'s argv (no shell; the launcher re-validates it) and the
`shellQuote`d kill target — sanitizing it would act on a different session.
Nothing remote reaches a tmux format string, so `stripWindowName` does not
apply.

## Non-goals

- `--remote-pick --windows` and `^o` (unchanged).
- Restorable (tmux-remux snapshot) rows in window mode: a no-server host shows
  only its host row; Enter on it cold-starts as in session mode.
- `^t` multi-open of remote windows.
- Proc icons, agent state, PR/issue columns, CPU/Mem on remote window rows
  (later column-parity tasks).
- Remote preview capture: the preview pane shows the session picker's text
  card for the row (`remote bridge → host/sess:index name`, `Enter runs
  og-remote-open`).
- Any change to the session picker's probe, cache, rows or Enter.

## Affected code and consumers

| Area | Change | Session-picker effect |
| --- | --- | --- |
| `picker/remote_windows.go` (new) | window probe cmd + parse, cache read/write/forget, pending/collected row builders, kill body | none |
| `picker/remote.go` | host-row note/flag switch shared by both collectors; stale "window mode has no host" comment fixed | identical rows (existing tests) |
| `listItem` | `remoteWindowID`, `remoteWindowIndex`, `remoteLive`; window rows' target `remote:<host>:<sess>:@<id>` | session rows unchanged |
| `isKillableRemoteSession`, `markable`, `remoteRowLabel`, forgotten key, `forgetRemoteRows`, `filterForgottenRemoteRows` | branch on `remoteWindowID` | unchanged for rows without it |
| `kill.go` `killRun.run` | dispatch window rows to the window kill | unchanged |
| `attach.go` `attachSpec`/`buildAttachCmd` | optional window index → 3rd argv | argv unchanged when empty |
| `tui.go` `newPickerModel`, `Init`, `remoteCmd`, `remoteAuthDoneMsg` | window-mode pending rows and probe | unchanged |
| `tui.go` `withFilter` | window-mode query path appends remote matches after local groups (shared helper with session mode) | identical output (existing tests) |
| `tui.go` `recombine`/`scopedItems`, `handleKey` tab, `render_list.go` hints | window-mode scope | unchanged |
| `tui.go` `loadPreviewCmd` | window-row text card | unchanged |
| `scripts/og-remote-open.sh` | caller-given window index validated by the probe (D4) | none: session opens pass no index |
| `tests/remote-cold-start.bats` | launcher index cases | — |
| `tui.go` `forgetRemoteRows` (session kill) | also drops the session's windows from the window cache | same rows, one extra cache rewrite |
| `tests/perf/picker-open-latency.sh` | opt-in `REMOTE_HOSTS` fixture knob | — |
| `docs/agents/picker.md`, `docs/agents/performance.md`, `docs/agents/scripts.md` | behaviour, latency, launcher contract | — |

## Acceptance (from the task)

- Window picker lists and searches remote hosts' windows; Enter opens the
  session focused on the window with the session picker's spinner, cancel and
  auth behaviour.
- Remote kill from the window picker is y/N confirmed, default No.
- First paint not slowed: cache-only first paint, async probe; latency before
  and after via `tests/perf/picker-open-latency.sh`.
- Go tests: probe-output parsing, row building (pending, collected, every host
  state, dedup of mirrored sessions, sanitization), attach argv with the window
  index, kill dispatch and the remote kill body, live-only kill, scope.
- `docs/agents/picker.md` updated; `nix flake check` and `nix build .#lint` pass.
