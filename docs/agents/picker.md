# Picker (session / window / which-key TUI)

Layout and input invariants of the Go bubbletea pickers under `picker/`.

## Picker Chrome (header, sticky line, sections)

- **The session list's column labels are glyph + word** (`@icon_host`,
  `@icon_procs`, `@icon_cpu`, `@icon_mem`, added to the `icons` attrset like the
  rest). Every cell is sized with `visibleWidth`, never `len`: a nerd glyph is
  one cell and four bytes, so `len`-based padding puts each column three cells
  short. **Each label sets a floor under its column's width** — the columns are
  otherwise sized by their data alone (Host is as wide as the widest
  `@bridge_host`), and a label wider than its cell pushes every later column
  right in the header only. The CPU and Mem labels mirror each other around the
  ` / ` (CPU padded left, Mem padded right) so the pair reads as one unit;
  padding never changes a field's total width, or Path drifts. The alignment
  tests assert the ` / ` and the path glyph land on the same cell in the header
  and in a row, which is the regression net for both mistakes.
- **One pinned line** (`governingHeaderIdx` + `renderHeaderItem`) carries
  whichever header governs the rows beneath it — the column glyphs through the
  session list, the divider once the window starts inside Remote or New session,
  the group row in window mode. One line, not a column header stacked above a
  section header: this is a popup that rarely has twenty rows to give. At the top
  of the list the pinned row *is* `visible[start]`, so the body advances past it
  rather than drawing it twice; the rendered height is therefore constant across
  scroll, which a test asserts.
- **Section dividers are rebuilt at render width.** The collectors emit
  `"── Remote " + 220 dashes` and cannot do better — they do not know the popup
  width — so they now carry `headerLabel`/`headerIcon` instead and
  `renderHeaderItem` composes glyph + label + a rule that ends exactly at the
  edge. `isColumnHeader` marks the one header that is not a section, so the pin
  passes its own `display` through untouched.
- **`readTmuxOpts`** reads `tmux show -g -F '#{option_name} #{option_value}'`
  directly — the custom `-F` format bypasses `show -g`'s default quote-escaping
  template, so `#{option_value}` is already raw. An option set to the empty
  string now reads back as an empty string with no quote-stripping needed
  (previously `show -g` printed it as `''`, which is what `unquoteTmuxOptValue`
  used to unwrap; that helper is gone).

## Window list columns (#904)

- **Rows**: `tree marker identity icons [host] [cpu / mem] [pr] [path]`. The identity column
  (issue/branch/PR/crew) keeps its own width logic; the new columns are optional and
  `renderWindowItemsWith` drops them to protect it. **Drop order at a known width: Path, then
  CPU/Mem, then Host**, until the identity cap reaches 24 cells (a path costs at most 24 cells in
  that budget and the line clips the rest). An unknown width (`<= 0`, the first paint) keeps all.
  Host exists only when some window is a mirror.
- **Column-label row** is `items[0]` (`isColumnHeader`, no target, unselectable). In window mode
  `renderList` pins it on its own first line *and* keeps the group-header pin below it
  (`listLayout`), so the body is `h-2`; `listIndexAt` accounts for both. It passes `itemVisible`
  under `^a`/`^s` and is dropped under a query, like session mode. Session mode is untouched.
- **CPU/Mem is async, like the session list**: `initialModel` never merges, so the first paint
  shows `-`; `refreshDataCmd` calls `mergeWindowResources` (one `ps` walk from every pane pid, the raw output cached 5 s and
  re-aggregated against the *current* windows each refresh, so a renumber never reads a stale
  `sess:idx`; both the chained and fallback branches; `Init` kicks the first refresh at once). The same walk gives a
  shell-hosted agent its icon (`agentCmds` appended to the window's procs).
- **Header labels are glyph+word, falling back to glyph-only** for Procs/CPU/Mem when the word
  floors would shrink the identity column at a known width.
- **Mirror windows (`@bridge_win`, not merely an inherited `@bridge_host`) never get local figures or tree agents**: their local pane pids measure the
  renderer and `@bridge_res` is session-scoped, so every mirror window renders `-` and
  `@bridge_proc` alone names its command. Their path is `@bridge_session_path`.
- **Session-grouped headers** show the session display name (`sessionDisplayName`; target and
  `groupKey` stay raw), the aggregated agent icon and `N win`; state-grouped headers are
  unchanged. Host is part of row and header search text.

## Launch

- **The picker itself launches in a popup-float** (`display-popup`'s compat
  modal-pane form, #725 — see `floats.md`'s "Popups are modal floats"): its
  own window/session targets are never captured by the preview or the wall,
  `selfCaptureTarget` (`picker/capture.go`) resolving them to the pane under
  the float instead so a tile for the current window shows the window's real
  content, not the picker looking at itself. Killing the picker's *own*
  window (from the window picker) or session (from the session picker) takes
  the picker down with it — the popup used to survive and refresh; a
  popup-float cannot, since it lives inside the window it just killed.
  Accepted, not worked around: the action still completes, and the end state
  is the one any picker that closes after acting leaves behind. The session
  picker's launcher makes that a *clean* end rather than an error: the
  destroyed float makes `display-popup` exit 129 (killed by SIGHUP), and
  `tmux-session-picker.sh` (or `tmux-window-picker.sh` for the window picker)
  exits 0 for it — but only when the popup's host window is really gone, so a
  genuine picker failure still reaches `run-shell`
  and the binding paints no `returned 129` (#884). Launchers
  pass `-t "<client>:"` alongside `-c "$CLIENT"` — the float lands in the
  `-t` window, and unpinned that defaults to tmux's "best" session rather
  than the client's own.

## First paint and key latency

What the popup does before its first list frame and on every key, and why.
Measurements are in `performance.md` ("Picker open latency").

- **One tmux invocation reads everything the first frame needs** (`collectStartup`,
  `picker/startup.go`): `show -g`, the pane snapshot and, in window mode, the window
  rows and session activity, joined with `\;` and split on a line equal to
  `@@og-picker-sep@@` (a whole-line match, so an option value carrying the text is
  not a separator). A failed list falls back to `readTmuxOpts` +
  `collectPanesSnapshot`. Add a first-paint read to this list rather than as another
  call; every call is a fork plus a queue behind the server thread.
- **`currentBranch` never forks git for a repo it can read** (`picker/gitbranch.go`):
  it reads `HEAD` and declines to git for anything it cannot answer identically. See
  `performance.md` for the decline list. It backs both the first paint and the window
  picker's 1 s refresh.
- **`programOptions` (`picker/tui.go`) sets `tea.WithFPS(120)` and
  `tea.WithColorProfile(lipgloss.Writer.Profile)`.** The renderer only flushes on its
  ticker, so the first frame waits up to a period, and `Run` would otherwise fork a
  second `tmux info` before starting it. Do not drop the profile: without it the
  picker forks `tmux info` twice before painting.
- **The window picker's 1 s refresh chains its reads too** (`collectTmux(true, false)`:
  pane snapshot, window rows, session activity in one call), and the `ctrl+g` regroup
  runs the same `refreshDataCmd`.
- **`View()` composes the list frame itself** (`picker/render_frame.go`). Lipgloss'
  bordered `Style.Render` plus two `JoinVertical`s re-measure every line with grapheme
  segmentation, ~75% of a 2 ms frame; `composeFrame` measures each line once, pads it
  to the popup width and reuses the bottom rule `renderSearch` already draws in the
  same colour. It declines to `composeFrameLipgloss` (the original, kept as the test
  oracle) for a line wider than the frame or holding a control character other than ESC
  (tab, CR, VT, NUL, DEL, C1, U+2028/9), which lipgloss would wrap, expand or drop. `TestComposeFrameMatchesLipgloss` holds the two
  byte-identical across sizes, previews, queries, host badges and wide glyphs. Change
  the frame's layout in `composeFrameLipgloss` first and make the fast path match.
- **Preview captures are throttled, not debounced** (`previewThrottle`, `tui.go`). The
  first request after a quiet spell forks its `capture-pane` at once, so one cursor move
  previews as fast as before. Requests inside `previewMinGap` (40 ms) wait for it and
  only the newest still runs, so a held `j`/`k` (~30 Hz) forks at most one capture per
  gap, and the row it stops on is always previewed. The cmd for a superseded request
  returns nil.
- **Async data stays async.** Zoxide, remote hosts, previews and the full resource
  refresh still arrive through `Init` after the first frame; nothing new is loaded
  ahead of it.
- **`OG_PICKER_TRACE=<file>`** (`picker/trace.go`) appends `<event> <µs>` per startup
  milestone, timed from `OG_PICKER_T0` (unix ns) or process start, and exits 30 ms
  after the first paint. Unset it is inert. `--dump-first-frame [--windows]`
  (hidden) prints the first list frame as plain text at `OG_PICKER_DUMP_SIZE`
  (`WxH`, default 120x40) and runs no `tea.Cmd`; it is what `tests/perf/gotorque-picker.sh`
  and `TestDumpFirstFrameChainsTmuxReads` (which pins one tmux call and no git fork)
  drive.

## Input and filtering

- **A typed key is `printableKeyText`, never `len(key) == 1`** (`picker/tui.go`). bubbletea v2 reports the space key by its NAME — `KeyPressMsg.String()` falls through to `Keystroke()` for it, "the only invisible printable character" — so a length test silently dropped every space typed into a query and no picker filter could express `new window` (#689). The helper returns the literal character, which is what its three consumers need: the session/window picker and which-key append it to the query, and `relayKeyArgs` (`picker/capture.go`) sends it to a pane with `send-keys -l`, where widening the length test alone would have typed the word `space`.
- **`^t` marks a Remote-section session row for multi-open (#730), never Tab** — Tab drives host-scope cycling instead (below), and a printable key belongs to the query (`printableKeyText`), not a mark. `markable` (`picker/tui.go`) gates it to a resolvable session row: host rows, local sessions, zoxide rows, any row Enter itself can't act on (needs-auth, host-key-changed, tailscale-check, `remoteUnreachable` — a cached row of a host the probe just confirmed down), and any row already carrying a live mirror (`remoteMirrorTarget != ""`, see below) are all no-ops. A mark is keyed by `item.target` ("remote:\<host\>:\<sess\>"), the same key `restoreCursor` matches a cached row against its live replacement by (#631), so marking a session before its host's probe answers survives the cached→live swap, and survives typing a query that hides the row entirely — marks are a selection independent of the filter, not a property of what's on screen. **Marks resolve against the always-unscoped `m.sessionItems`+`m.remoteItems`, never `m.allItems`**: `markedRemoteItems` (`tui.go`) — since host/all scope shrinks `m.allItems` to one (or all-but-local) host's rows, resolving against it would silently drop a mark set on a host that isn't the current scope the moment Tab cycles away; a mirrored row can never appear in this set because `markable` excludes it and `m.remoteItems` never contains the synthesized mirror rows in the first place (those exist only in `scopedItems`' output). `renderList` draws a marked row with a tinted `✓` in the gutter column (sized like every other column glyph, never `len`); `renderHints` turns `enter`'s label into `open <n>` and adds a `^t:mark` hint (highlighted, with the count) once any row is marked. Enter with ≥1 mark ignores the cursor row entirely and opens every marked session instead: `openMarkedRemote` (`markedRemoteItems`'s list-order result) runs the first as the picker's supervised attach (see "Attach" below) — the session the client switches to — and, **only once that attach succeeds**, fires every other marked session through `launchDetached` (`launchRemoteBridgeDetached`, `Setsid: true`, not waited on), so they dial in parallel with each other and N marks never serialize N dials. They used to be fired before the first (#730); that made Esc cancel one of N and a retry double-launch the rest (#770). A cancelled or failed first attach launches none of them and keeps the marks, so Enter retries all. Esc clears marks first, before its usual clear-query-then-quit chain, when any are set.
- **`^x` on a Remote-section session row confirms inline, then kills on the remote (#736)** — local `^x` is unchanged (unconfirmed; only the tmux-native `prefix+x`/`prefix+&` binds confirm). The first `^x` on a remote row stages it — or, when `^t` marks exist, every killable marked remote row, skipping a marked snapshot-`remoteRestore` row (it has no live remote session) — and renders `kill <host>/<sess> on the remote?  (y/N)` (multi: `kill <n> remote sessions?  (y/N)`) instead of the hint row. The prompt is sized with `visibleWidth` (`fitVisibleWidth`) and reservations the `(y/N)` cells so the answer never truncates. Only `y`/`Y` — read through `printableKeyText`, never `len(key)==1` — acts; any other key cancels, so the destructive default is NO (Esc clears the prompt, not the query). On `y` it runs `ssh <host> … kill-session -t '=<sess>'` (`classifyKillErr`: a 255 keeps the unreachable/auth/host-key/tailscale states, exit 1 is a gone session, any other exit is an unrunnable remote tmux that keeps the row and says `could not run tmux`), tears down an existing mirror through `stopBridgeDaemon` — the same owner the manual mirror close uses, the daemon's own teardown doing the local `kill-session` — and forgets the row at once: `forgetRemoteSessionCache` rewrites the host's cache preserving `SavedAt` (remaining rows keep their age) and `forgetRemoteRows` drops the rows, mirrors, and marks in memory, so a stale listing cannot revive it. A gone-already session still forgets its row (with a hint); an unreachable host keeps the row and says why.
- **Tab cycles a host scope that filters what `recombine()` puts in `allItems`** (#733): `local → host₁ → host₂ → … → hostN → all hosts → local`, in `configuredHosts` (`picker/remote.go`) order — the same `@remote_bridge_hosts` list, self-aliases dropped, that Tab's cycle and `scopeAll`'s membership both use. `nextScope` (`tui.go`) computes the next `hostScope{kind, host}`; `tuiModel.scope` resets to its zero value (`scopeLocal`) on every fresh popup launch, so a relaunch never inherits a prior scope. `handleKey`'s `case "tab":` no-ops when `m.windowMode` or `m.emitPath != ""` (neither carries remote data to scope) or when `configuredHosts` is empty (nothing to cycle to). Host scope's row set (`scopedItems`): that host's already-mirrored local sessions filtered out of `m.sessionItems`, plus that host's Remote-section block reused **verbatim** from `m.remoteItems` (`remoteHostBlock` walks it by adjacency — row + immediately-following children — never rebuilt from cache) so every existing inert-state invariant (no children for needs-auth/host-key-changed/tailscale-check hosts) carries over unchanged, plus appended `"(mirrored)"` rows for that host's sessions already mirrored locally (from `m.mirrors`, populated off-thread each tick by `collectBridgeMirrors`/`parseBridgeMirrors` in `refreshDataCmd`, gated on `!windowMode`) — a mirrored row's Enter goes through the `switchClient` seam to the mirror's local session instead of opening a duplicate, regardless of that host's probe state (a plain `switch-client` never touches the remote). All-hosts scope assembles the same per-host blocks across every `configuredHosts` entry, host-block by host-block rather than a flat merge, so `hostColorFunc` tints stay visually grouped and no host's mirror rows interleave into another host's block. No zoxide rows in host/all scope — a "create a session here" suggestion has no host to scope to. The search row shows a scope badge (`withScopeBadge`, `render_list.go`: host name tinted via `hostColorFunc`, or "all hosts" in the peach highlight; nothing for local scope), and the footer's `⇥:scope` hint (highlighted once `m.scope.kind != scopeLocal`) is gated on `len(configuredHosts(m.tmuxOpts)) > 0`.
- **The which-key popup's filtered list is flat and score-ranked; its unfiltered list is grouped.** Grouping and ranking cannot both hold, and a search wants its best hit on line 1 rather than wherever its key table happens to fall — so `rebuildVisible` (`picker/whichkey.go`) drops the `── prefix ──` headers under a query and moves the table into a per-row column. 298 of a stock 434-bind server carry no `-N`, so their "note" is the raw `key_command`, and a query subsequence-matching a `/nix/store` path the column never shows returned 20 rows for `float` of which 2 were the floating-pane binds. `describedRank` sorts every real description above every command, rather than dropping the command rows — a plugin bind stays reachable by its command text, just below. Table headers carry a plain-words gloss (`whichKeyTableGloss`) because `prefix`/`root`/`move` are tmux's jargon, not descriptions; the prefix key is read live from the `prefix` option, and an unknown table gets no gloss rather than a guessed one.

## Attach (remote session open)

Enter on a Remote-section session row (or on marks) opens the session through `og-remote-open`. That launcher used to run synchronously inside `Update` (`openRemoteBridge`): no frame painted, no key read, no bound — a blackholed host held a frozen popup for the kernel's TCP connect timeout (#770). Now:

- **The launcher never runs on the Update goroutine.** `beginAttach` (`picker/tui.go`) only allocates an `attachRun` (`picker/attach.go`) and returns Cmds: one runs `attachRun.run()` (the fork happens there), one waits for the next progress/done event, one drives a 100ms spinner tick. Every attach message carries an attempt id; a message from a superseded attempt is dropped. `tui_attach_test.go` pins this: an Update on Enter returns immediately and the launcher has not started until the returned Cmd runs.
- **Progress comes over a dedicated fd, never stdout.** The runner passes a pipe as fd 3 and `OG_REMOTE_OPEN_PROGRESS_FD=3`; the launcher writes one phase name per line (`connect`, `start-server`, `restore`, `create`, `mirror`) and unsets the variable so the daemon's hand-off re-run never inherits it. stdout is not usable: `systemctl` and `tmux-remux restore` inherit it, so a remote could forge a phase line — and stdout now goes to `/dev/null`, because live frames would otherwise be painted over by raw remote bytes (the old code sent it to the popup pty). Unknown lines are ignored. The launcher closes the fd before starting the daemon, which would otherwise hold it for its whole life.
- **The status line replaces the hint line** while an attach is in flight (same row, so `bodyHeight` is unchanged): spinner, `opening <host>/<sess> · <phase label> · <n>s` (`attachPhaseLabel`: connecting, starting tmux server, restoring session, creating session, attaching, starting), where `<n>s` is seconds elapsed in the current phase, resetting on each phase line. The whole left-hand text is clipped from the right (`fitVisibleWidth`), so elapsed time and phase truncate before host/session does; `esc:cancel` has its own cells reserved separately. The picker's progress ends at `attaching`: after `switch-client` the client sits on the mirror's `og-remote-loading` pane, which shows the daemon's own phases.
- **Per-phase timeouts live in the runner, not the script**: launch (before the first line) 10s, connect 20s, start-server 45s, restore 90s, create 30s, mirror 15s (`attachBudgets`). Each phase line restarts the timer; expiry cancels exactly like Esc and reports `<host>/<sess>: timed out <phase label> after <n>s — enter to retry`.
- **Cancel (Esc or C-c) returns to the list with nothing half-built locally.** The launcher runs `Setsid` (own process group, and no controlling tty, so an ssh that would prompt fails fast instead of painting over the popup; `SSH_ASKPASS_REQUIRE=never` keeps it off a GUI askpass). Cancel sends SIGTERM to the whole group — every in-flight ssh dies with it — then SIGKILL after `attachKillGrace` (5s). Once the launcher exits, the phase-pipe drain is bounded by a short `attachPostKillDrain` (250ms), not a second `attachKillGrace`, so a descendant that survived the group kill still holding the pipe cannot delay the done message; a drain that follows a normal exit keeps the `attachKillGrace` bound. The daemon is outside the group (`setsid`, or `set -m` in the nohup fallback), so the launcher's own `TERM/INT/HUP` trap rolls back its mirror phase: reaps the daemon it started, kills the local mirror session, removes `sock`/`.pid`/`.phase`. Remote-side effects of a cancelled phase (a cold-started server, a half-run `tmux-remux restore`) are the remote's own state and are not undone. While cancelling, a second C-c ends the TUI, and the popup closes once `runTUI`'s bounded wait for the rollback (`attachKillGrace`+1s) returns.
- **The commit point is the final `switch-client`** (the fresh mirror's, or the dedup path's reuse of a live daemon): the launcher sets `attached=1` just before it, and a trap that sees it exits 0 with no rollback; the runner treats exit 0 as success even after a cancel. Accepted edge: a group TERM that lands while that `tmux switch-client` runs can kill the tmux client before the switch lands — the picker reports success and quits, the client stays put, and a complete mirror exists. Not a bug.
- **Every picker exit cancels an in-flight attach**, since `Setsid` removed the incidental protection of sharing the popup pty's session: `runTUI` owns an `attachSupervisor` the model registers each run with, and after `p.Run` returns — any quit, SIGTERM/SIGINT, or a panic bubbletea recovered (which returns a nil model) — it cancels and waits for the run before any error return. SIGHUP (popup pane killed) is caught and turned into `p.Kill()`. Uncovered: a SIGKILLed picker. On Linux the launcher carries `Pdeathsig: SIGTERM` (the runner pins its OS thread from fork to Wait, golang/go#27505), so its trap rolls back — eventually, since the signal reaches bash only and bash runs a trap after the foreground ssh returns; on darwin the launcher runs to completion unsupervised.
- **Faults are sanitized before they render.** The failure text is the launcher's last stderr line, which is often remote-derived; `sanitizeStatusText` drops ESC sequences (CSI, OSC to BEL/ST), C0/C1 controls (`0x9b` is a one-byte CSI) and bidi overrides, collapses whitespace and caps at 200 runes. The `host/sess` label goes through it too. It is separate from the daemon's `stripWindowName`, which targets a tmux-format sink (`|`, `#[…]`) rather than a terminal.
- **Retry is Enter.** A failure or timeout leaves the cursor and marks where they were, and the status line says `— enter to retry`. While an attach is in flight every other key and mouse event is ignored, so a second concurrent attach cannot start.

`^x` remote kill (`picker/kill.go`) reuses this same off-Update runner idiom: a `killRun` forked via a `tea.Cmd`, reporting back through `progress`/`done` channels, cancellable through its `context.CancelFunc` — same shape as `attachRun` above. It has no phases, though: each kill attempt is one bounded ssh round trip (`sshKillRemoteSessionCtx`), not a multi-stage subprocess with its own progress pipe, so there is nothing to report but which target is next.

