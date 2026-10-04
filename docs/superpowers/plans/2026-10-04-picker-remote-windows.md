# Remote windows in the window picker (#902) — plan

Spec: `docs/superpowers/specs/2026-10-04-picker-remote-windows-design.md` (D1–D7).

Gate commands (used by every step; run from the worktree root inside the devshell):

- `G-go`: `cd picker && go vet ./... && go test -race ./...`
- `G-lint`: `cd picker && golangci-lint run ./...`
- `G-bats`: `bats tests/remote-cold-start.bats`
- `G-sh`: `shellcheck scripts/og-remote-open.sh tests/perf/picker-open-latency.sh && shfmt -d scripts/og-remote-open.sh tests/perf/picker-open-latency.sh`

## File list

| File | Purpose |
| --- | --- |
| `scripts/og-remote-open.sh` | D4 launcher fix: a caller-given window index is validated by the probe (gone session → exit 1; absent index → active window; exact session match only) |
| `tests/remote-cold-start.bats` | fake-ssh probe learns the new `want_lit`/`want=` fields; three new cases |
| `picker/remote_wins.go` (new) | window probe command + parser, ssh probe, window cache (read/write/forget), row builders (pending, cached, collected), remote kill-window body + ssh call |
| `picker/remote_wins_test.go` (new) | tests for everything in `remote_wins.go` |
| `picker/remote.go` | `remoteProbeResult.Windows`; shared `probeStateOf(err)`, shared host-row builder for a resolved probe state, shared `sshRun` helper for probe/kill ssh; stale `sessionDisplayName` comment fixed |
| `picker/tui.go` | `listItem` fields; window-mode first paint / Init / `remoteCmd` / auth re-probe; `withFilter` window-mode remote handling; preview card; attach with window index; kill staging/dispatch/forget for window rows; session kill also forgets window cache; window-mode Tab scope |
| `picker/kill.go` | `killRun.run` dispatches window rows to the window kill |
| `picker/attach.go` | `attachSpec.window` → third launcher argv |
| `picker/render_list.go` | kill prompt wording for window rows; `^x` hint label on remote rows |
| `picker/tui_test.go`, `picker/tui_attach_test.go`, `picker/scope_test.go`, `picker/attach_test.go` | model-level tests (window-mode rows, Enter, kill, scope) |
| `tests/perf/picker-open-latency.sh` | opt-in `REMOTE_HOSTS=<n>` fixture knob |
| `docs/agents/picker.md`, `docs/agents/scripts.md`, `docs/agents/performance.md` | behaviour, launcher contract, latency results |
| `docs/superpowers/specs/2026-10-04-picker-remote-windows-design.md`, this plan | committed with the change |

## Steps

### Launcher (independent of the Go steps; may run in parallel with Step 3+)

- [ ] **Step 1: red bats cases for a caller-given window index** (`tests/remote-cold-start.bats`).
  Teach the fake ssh's `: og-probe;` branch the new protocol: when the probe script carries
  `want_lit='<n>'`, print `win=` as the session's *active* window (`1`, or empty when
  `FAKE_NO_WINDOW` is set) and `want=1`, or `want=` (empty) when `FAKE_WANT_ABSENT` is set;
  keep the existing `win_lit` handling untouched for now so old tests still pass. Rewrite
  `combined probe: session and window both given` to grep `want_lit='3'` instead of
  `win_lit='3'`. Add three tests, each running `bash "$LAUNCHER" tp-g6 workstation 3`:
  (a) `FAKE_NO_WINDOW=1` → status 1, output names the session, no `new-session` in
  `$TMUX_LOG`; (b) index present → status 0 and the daemon env/argv carries window `3`
  (the launcher backgrounds the daemon, so have the stub daemon append
  `$OG_BRIDGE_WINDOW` to a log in the test tmpdir and poll that log for a bounded time, the
  way the suite's existing daemon tests wait on the stub — never a bare immediate read);
  (c) `FAKE_WANT_ABSENT=1` → status 0 and the daemon gets window `1` (the active one).
  Also (d): with an index, the probe script contains no `sess_canon=` step
  (`! grep -q sess_canon "$SSH_LOG"`).
  Proof: `G-bats` → the new cases fail (red) against the unchanged launcher; report which.
- [ ] **Step 2: launcher fix** (`scripts/og-remote-open.sh`).
  Save `caller_win="$win"` right after argv parsing. In the probe script builder:
  replace the `if [[ -n $win ]]; then … win_lit …` branch so that when an index was given
  on a plain open (neither `OG_REMOTE_RESTORE` nor `OG_REMOTE_NEW_DIR`) it emits
  `want_lit=$(shell_quote "$win")` / `want="$want_lit"`, skips the `sess_canon` prefix
  fallback (exact `has-session -t "=$sess"` only — leave `sess` literal when it fails), and
  — like the no-index branch — always computes `win` as the active window, but against
  the exact target `-t "=$sess"`; then computes `want_ok` = `1` iff
  `list-windows -t "=$sess" -F '#{window_index}'` has a line equal to `$want`
  (`awk -v w="$want" '$0==w{f=1} END{if(f)print 1}'`), and adds `want=%s` to the final
  printf. Restore/new-dir opens with an index keep today's `win_lit` path verbatim.
  Parse `want=` into `probe_want`. After `[[ -z $win ]] && win="$probe_win"`, replace it with:
  if `caller_win` is set on a plain open → if `probe_win` is empty, print
  `og-remote-open: session '$sess' has no window on $host — it is gone or was never there`
  to stderr and `exit 1`; else `win=$caller_win` when `probe_want == 1`, otherwise
  `win=$probe_win`. Otherwise keep the old line. Keep the script bash-only on the remote
  side (it is fed to `bash -s`).
  Proof: `G-bats` all green (old + new); `G-sh` clean.

### Go data layer

- [ ] **Step 3: red tests for the window probe parser and cache** (`picker/remote_wins_test.go`).
  - `TestParseRemoteWindowsOutput`: identity lines 1–2; `S|$1|api`, `S|$2|web`,
    `W|$1|@3|1|server`, `W|$1|@4|2|logs|with|pipes` (name keeps its `|`), `W|$2|@7|1|vite`;
    junk lines dropped: `W|$9|@8|1|orphan` (no S line), `W|1|@3|1|x` (bad session id),
    `W|$1|3|1|x` (bad window id), `W|$1|@5|x|y` (bad index), a fish greeting line,
    `S|$3|` (empty name). Expect `Sessions = [api web]`, windows in input order with
    `Session` filled from the S map, `Index` ints.
  - `TestParseRemoteWindowsOutputSanitizesNames`: a window name and a session name holding
    `\x1b[31m`, `\x07`, `‮` render-safe via the row builder (Step 5) — here assert
    the parser keeps the **raw** session and window names (no `TrimSpace`, no
    sanitizing — the raw session name drives the exact `=sess` actions), and that
    `remoteDisplayName(s)` returns `truncateCells(sanitizeStatusText(s), 40)`.
  - `TestRemoteListWindowsCmdFishSafe`: the command has no `var=value` assignment, uses
    `\;` between `list-sessions` and `list-windows -a`, carries the identity preamble,
    and uses both `remoteTmuxCmd` legs (mirror `TestRemoteKillSessionBodyFishSafe`).
  - Cache round trip with `useRemoteCache(t)`: write → read; host mismatch rejected;
    a group/world-writable dir rejected; `forgetRemoteWindowCache(host, "@4")` keeps
    `SavedAt`; `forgetRemoteSessionWindowsCache(host, "api")` drops every `api` window.
  Proof: `G-go` fails to compile (red) on the missing symbols.
- [ ] **Step 4: implement the data layer** (`picker/remote_wins.go`, `picker/remote.go`).
  - `type remoteWindow struct { Session, SessionID, ID string; Index int; Name string }`
    with json tags; `remoteProbeResult` gains `Windows []remoteWindow`.
  - `remoteListWindowsBody = remoteTmuxCmd("list-sessions -F 'S|#{session_id}|#{session_name}' \\; list-windows -a -F 'W|#{session_id}|#{window_id}|#{window_index}|#{window_name}'")`,
    `remoteListWindowsCmd = remoteIdentityPreamble + "; " + remoteListWindowsBody`.
  - `parseRemoteWindowsOutput(stdout) remoteProbeResult` per Step 3 (validate `$<digits>`,
    `@<digits>`, `<digits>`; `SplitN(line, "|", 3)` for S, `SplitN(line, "|", 5)` for W;
    identity lines are trimmed as today, S/W names are kept raw — only a trailing `\r`
    from the ssh pipe is dropped).
  - `remoteDisplayName(s string) string` = `truncateCells(sanitizeStatusText(s), 40)`, the
    one place a remote session/window name is made render-safe (D7).
  - Factor the ssh invocation in `sshListRemoteSessions` into
    `sshProbe(host, remoteCmd string, parse func(string) remoteProbeResult)`;
    `sshListRemoteSessions` and the new `sshListRemoteWindows` both call it (session
    behaviour byte-identical).
  - Window cache: `remoteWindowCacheDir()` = sibling `remote-windows` of
    `remoteSessionCacheDir()`; `remoteWindowCache{Host, SavedAt, Windows}`;
    `writeRemoteWindowCache`, `readRemoteWindowCache`, `forgetRemoteWindowCache(host, id)`,
    `forgetRemoteSessionWindowsCache(host, sess)` with the session cache's exact trust
    checks (factor a shared owner-only JSON read/write helper used by both caches rather
    than copying the Lstat/uid/rename code).
  Proof: `G-go` green, including every pre-existing `remote_cache_test.go` case.

- [ ] **Step 5: red tests for row building** (`picker/remote_wins_test.go`).
  With injected probes (no ssh), mirror `collectRemoteItems`' existing table tests:
  - `collectRemoteWindowItems` for each probe state: OK (host row with no note, window
    rows in probe order with `remoteLive`, `remoteHost`, `remoteSess` (raw),
    `remoteWindowID`, `remoteWindowIndex`, target `remote:<host>:<sess>:@<id>`,
    `searchText == "<host>/<sess>:<idx> <host> <sess> <name>"`); OK with every session
    bridged → `(all open)` and no window rows; a bridged session's windows excluded,
    another session's kept; NoServer → `(no server — Enter starts one)`, no rows, cache
    written empty; Unreachable → host note `(unreachable — open default)` + cached rows
    dimmed, `remoteUnreachable`, not `remoteLive`; NeedsAuth / HostKeyChanged /
    TailscaleCheck → host row only with the session picker's flags and notes; self-alias
    → host dropped; no hosts → nil. Probe OK writes the window cache.
  - `pendingRemoteWindowItems`: header + host rows with `…` + cached rows (not live,
    stale ones dimmed with `(cached <age>)`), bridged sessions skipped via
    `firstPaintBridges`; nil without configured hosts.
  - Display sanitization: an ESC/bidi-laden window and session name produce a `display`
    and `plain` with no ESC byte other than the builder's own colour codes and no U+202E;
    window name truncated to 40 cells.
  - Regression: `collectRemoteItems` (session mode) output for each state is unchanged —
    the existing tests cover it; run them.
  Proof: `G-go` red on missing symbols.
- [ ] **Step 6: implement row building** (`picker/remote_wins.go`, `picker/remote.go`, `picker/tui.go` listItem only).
  - `listItem` gains `remoteWindowID string`, `remoteWindowIndex int`,
    `remoteWindowName string` (already `remoteDisplayName`d), `remoteLive bool` (comments in
    the existing style). `remoteWindowRowItem` sets all four (`remoteLive` false; the
    collector sets it on probe-OK rows).
  - Factor from `collectRemoteItems` a `probeStateOf(err) remoteProbeState` (the switch in
    `remoteSessionsForHost`) and `remoteHostRowForState(opts, host, state, tailscaleURL string, anyRows bool) listItem`
    (the note switch + flag switch). `collectRemoteItems` calls them; its output must not
    change.
  - `remoteWindowRowItem(host string, w remoteWindow, note, cHost, cDim string, dim bool) listItem`:
    display `cHost+├─+reset + " " + cDim+remoteDisplayName(sess)+reset + " " + "<idx>: " + remoteDisplayName(name)`,
    `displayEnd`/`plainEnd` with `╰─` (so `markRemoteTreeEnds` works), dim variant like
    `remoteSessionRowItem`. `searchText` uses the display names.
  - `cachedRemoteWindowRows`, `pendingRemoteWindowItems`, `collectRemoteWindowItems`
    (per-host goroutines, self-alias handling, cache policy exactly as
    `collectRemoteItems`, no restore probe).
  - Fix the `sessionDisplayName` doc comment (`remote.go` ~1051–1056): window rows do
    carry `bridgeHost`; the reason the session is never renamed is that the bare name is
    the tmux target and is read outside the picker.
  Proof: `G-go` green; `G-lint` clean.

### Wiring into the window picker

- [ ] **Step 7: red model tests for listing and search** (`picker/tui_test.go`).
  - First paint: with `useRemoteCache(t)` + a seeded window cache for `lab` and
    `@remote_bridge_hosts=lab`, `newPickerModel(true, …, windowItems, "")` has the Remote
    header, the `lab …` host row and the cached window rows after the local groups, and
    `m.cursor` is on the first local window.
  - `Init` in window mode includes the remote probe Cmd (assert via a seam: make
    `remoteCmd` read the collector through a package var `collectRemoteWindowItemsFn`
    the test swaps, run the batch, see `remoteMsg`).
  - `remoteMsg` in window mode replaces the cached rows and keeps the cursor on a row
    whose target is unchanged (cached and live share `remote:<host>:<sess>:@<id>`).
  - Query in window mode, session-grouped and state-grouped (`stateGrouped=true`, with a
    local window in the `""` "no agent" group): typing the remote window's name shows it
    under the Remote header with its host row as context (`remoteContextOnly`), never
    under a local group header; typing a local name still shows only local groups;
    `markRemoteTreeEnds` gives the last remote window `╰─`. In state-grouped mode a query
    matching a **local** no-agent window renders it under the local `""` group header,
    never under the Remote header (the remote header also has `groupKey ""`).
  - Preview card text for a window row contains `remote bridge → lab/api:2 logs` and
    `og-remote-open`.
  - `refreshMsg` in window mode leaves `remoteItems` untouched.
  Proof: `G-go` red.
- [ ] **Step 8: implement listing** (`picker/tui.go`).
  - `newPickerModel`: `m.remoteItems = pendingRemoteWindowItems(...)` when `windowMode`
    (emit mode can't be window mode; keep the `emitPath == ""` guard).
  - `Init`: in window mode also append `m.remoteCmd()`.
  - `remoteCmd`: window mode calls `collectRemoteWindowItemsFn(opts, collectBridgeSessions(), nil)`.
    `remoteAuthDoneMsg` already calls `remoteCmd`, so the re-probe follows.
  - `withFilter`: extract the session branch's remote-header + host-context insertion
    into a helper `appendRemoteMatch(out, match, …)` used by both branches; in the window
    branch route `rankRemote` matches through it instead of the `groupKey` grouping, build
    the window branch's `headerMap` from local headers only (skip `isRemoteHeader` — its
    `groupKey` is `""`, the state-grouped "no agent" key, and would replace that header),
    and run `markRemoteTreeEnds` on the window-mode result. Session-mode output must not
    change (existing tests).
  - `loadPreviewCmd` default card: append `:<idx> <remoteWindowName>` when
    `remoteWindowID != ""`.
  Proof: `G-go` green (all existing tests too); `G-lint` clean.

- [ ] **Step 9: red tests for Enter on a remote window** (`picker/tui_attach_test.go`, `picker/attach_test.go`).
  - `buildAttachCmd` with `window: 3` → argv `[host sess 3]`; without → `[host sess]`
    (session path unchanged).
  - A window-mode model on a remote window row: Enter returns without launching (same
    assertion style as `TestAttachUpdateNeverLaunchesSynchronously`), and the fake
    launcher (the suite's bin) records argv `lab api 2`; `m.attach.label == "lab/api:2"`.
  - Enter on a window-mode host row with `remoteNeedsAuth` returns an `ExecProcess` cmd
    (as session mode); on `remoteInert` / `remoteTailscaleCheck` sets the same status text.
  - A failed attach on a window row leaves `— enter to retry` in `statusMsg`.
  Proof: `G-go` red.
- [ ] **Step 10: implement Enter** (`picker/attach.go`, `picker/tui.go`).
  `attachSpec.window int` (0 = none; tmux-og windows start at 1, and the launcher
  already treats empty as "active"); `buildAttachCmd` appends `strconv.Itoa(window)`
  when > 0. `beginAttach` passes `first.remoteWindowIndex` when `first.remoteWindowID != ""`.
  `remoteRowLabel` appends `:<idx>` for window rows. `markable` returns false when
  `remoteWindowID != ""`.
  Proof: `G-go` green.

- [ ] **Step 11: red tests for remote window kill** (`picker/remote_wins_test.go`, `picker/tui_test.go`).
  - `remoteKillWindowBody("my sess", "@4")` contains `kill-window -t '=my sess:@4'`
    (shellQuoted), a hostile session name `a'; rm -rf ~; '` is quoted inert, fish-safe
    like `TestRemoteKillSessionBodyFishSafe`.
  - Scratch-server test like `TestKillRemoteSessionReachesScratchServer`: sessions `a`
    (window `@0`) and `b` (windows `@1`, `@2`); killing `=b:@2` removes only `@2`;
    killing `=a:@2` (id not in that session) returns `errRemoteSessionGone`-class and
    kills nothing.
  - Model: `^x` on a live window row stages `killConfirm` and the hint line reads
    `kill lab/api:2 logs on the remote?  (y/N)`; any key but `y` cancels; on a cached
    (not `remoteLive`) row `^x` stages nothing and sets a status mentioning the cache;
    `y` with `fakeKillSSH(t, 0)` drops the row, the window cache entry and records the
    forgotten key; with `fakeKillSSH(t, 1)` → `already gone`, row dropped. Killing the
    only window row of `api` also drops `api` from the session cache; killing one of two
    does not. A late `remoteMsg` carrying the killed window does not revive it **and leaves the
    session cache's `api` entry intact** while other `api` windows are live (the
    forgotten-row filter must not forget the whole session).
  - Session-picker regression: killing session `mono` also removes `mono`'s windows from
    `lab`'s window cache (seed both caches).
  - `^t` on a window row marks nothing.
  Proof: `G-go` red.
- [ ] **Step 12: implement remote window kill** (`picker/remote_wins.go`, `picker/kill.go`, `picker/tui.go`, `picker/render_list.go`).
  - `remoteKillWindowBody`, `sshKillRemoteWindowCtx(ctx, host, sess, id)` sharing the
    `sshKillRemoteSessionCtx` runner (factor `sshKillCtx(ctx, host, body)`; session path
    unchanged), same `classifyKillErr`.
  - `killRun.run`: `remoteWindowID != ""` → window kill.
  - `ctrl+x`: a window row (`remoteWindowID != ""`) stages `[]listItem{item}` only when
    `remoteLive`; else `statusMsg = "<label>: listed from cache — wait for the probe, then ^x"`.
    Window rows are never added from marks (`killableMarkedRemoteItems` unchanged — window
    rows are unmarkable). `isKillableRemoteSession` excludes window rows.
  - Forgotten key: `forgottenKey(it listItem)` = host\0sess, plus \0id for window rows;
    used by `forgetRemoteRows`, `filterForgottenRemoteRows`, `filterForgottenMirrors`
    (mirrors keep the session key). `filterForgottenRemoteRows` branches on
    `remoteWindowID`: a dropped window row calls `forgetRemoteWindowCache(host, id)` (never
    `forgetRemoteSessionCache`); a dropped session row keeps today's
    `forgetRemoteSessionCache` plus `forgetRemoteSessionWindowsCache`. `forgetRemoteRows` for a window row:
    `forgetRemoteWindowCache`, drop the row, and when no other row of that host+session
    with `remoteWindowID != ""` remains in `m.remoteItems`, `forgetRemoteSessionCache`.
    For a session row additionally `forgetRemoteSessionWindowsCache(host, sess)`.
    Only session rows tear down mirrors.
  - `finishKill` messages (cancelled, already gone, failed) and `remoteKillFailure` take
    the row label (`remoteRowLabel`, so `lab/api:2`) instead of `host + "/" + sess`; the
    session-row text stays byte-identical.
  - `render_list.go`: prompt `kill <remoteRowLabel> <sanitized name> on the remote?` for a
    window row (sessions unchanged); multi-row wording unchanged; `^x` hint label
    `kill remote` on any remote row with `remoteSess != ""`.
  Proof: `G-go` green; `G-lint` clean.

- [ ] **Step 13: red tests for window-mode Tab scope** (`picker/scope_test.go`).
  Replace `TestTabNoOpInWindowAndEmitMode`'s window half: in window mode with hosts
  `lab devbox`, Tab → `scopeHost lab`: `allItems` holds only local window rows with
  `bridgeHost == "lab"` (and their group headers), then the Remote header and `lab`'s
  block verbatim — no `devbox` rows, no non-mirror local windows, no synthesized
  `(mirrored)` rows; Tab again → `devbox`; again → all hosts (both blocks, host by host);
  again → local. Emit mode stays a no-op. With a query, scoped results still group local
  windows under headers and remote rows under the Remote header. (The `⇥:scope` hint
  already renders in window mode — `render_list.go` has no window gate — so no hint test.)
  Proof: `G-go` red.
- [ ] **Step 14: implement window-mode scope** (`picker/tui.go`).
  `case "tab"`: drop the `m.windowMode` early return (keep emit-mode's). `scopedItems`:
  in window mode keep local rows whose `bridgeHost` matches plus every local header row
  (orphans are pruned by `withFilter`), append host blocks via `remoteHostBlock`, and
  skip the `m.mirrors` loop. Keep `collectBridgeMirrors` session-mode only. Update the
  `tab` case comment.
  Proof: `G-go` green; `G-lint` clean.

### Latency, docs, gate

- [ ] **Step 15: perf knob** (`tests/perf/picker-open-latency.sh`).
  Export `XDG_CACHE_HOME="$W/cache"` in the env block (the picker prefers an absolute
  `XDG_CACHE_HOME`, so `HOME` alone does not isolate the cache). `REMOTE_HOSTS=<n>`
  (default 0): in `build_fixture`, when > 0, set `@remote_bridge_hosts` on the scratch
  server to `og-perf-h1 … og-perf-hn` and write
  `$XDG_CACHE_HOME/tmux-og/remote-windows/og-perf-h<i>.json` (`mkdir -p -m 700`) holding 3 sessions × 4 windows with `saved_at` = now in ms,
  in the Step 4 JSON shape. Document it in the header `Env:` block.
  Proof: `G-sh` clean; `REMOTE_HOSTS=2 OPENS=3 tests/perf/picker-open-latency.sh window` runs,
  and one `--dump-first-frame --windows` against the fixture env (`OG_PICKER_DUMP_SIZE=140x40`)
  shows the seeded remote window rows — proof the first paint really read the caches.
- [ ] **Step 16: measure** (no file change yet). Build `nix build .#default`. Build a base picker
  from `origin/main` (`git worktree add <scratch> origin/main` + `go build`, passed as
  `PICKER_BIN`). Run, alternating base and new, two rounds each:
  `OPENS=40 tests/perf/picker-open-latency.sh window` and
  `REMOTE_HOSTS=2 OPENS=40 tests/perf/picker-open-latency.sh window` (base ignores the
  hosts' window caches — that is the comparison), recording `uptime` per run. Also
  `tests/perf/picker-open-latency.sh forks` for new with `REMOTE_HOSTS=2` to show no
  ssh exec before `paint`.
  Proof: the medians/p95 table; window first-frame median with hosts within noise of base.
- [ ] **Step 17: docs.**
  - `docs/agents/picker.md`: window-mode Remote section (rows, cache, first paint,
    Enter with index, dedup residual, `^x` live-only window kill and last-window session
    cache forget, Tab scope in window mode, no marks, sanitization); fix the "Tab …
    no-ops when `m.windowMode`" sentence.
  - `docs/agents/scripts.md`: `og-remote-open` row documents the third argument contract
    (D4); the `tmux-window-picker` row (or `prefix + w` mention) gains the Remote section.
  - `docs/agents/performance.md`: a short "Remote windows in the window picker (#902)"
    subsection under "Picker open latency" with Step 16's table.
  Proof: `nix build .#lint` green (typos, markdown hooks).
- [ ] **Step 18: full gate.** `nix build .#default && nix flake check && nix build .#lint`.
  Proof: all three exit 0.

## Acceptance → evidence

| Acceptance item | Evidence |
| --- | --- |
| Remote windows listed and searchable; Enter opens/attaches with spinner/cancel/auth | Steps 7–10 tests (`go test -race ./...`), Steps 1–2 bats for the launcher's gone/absent index |
| Remote kill y/N confirmed, default No | Step 11 tests |
| First paint not slowed; latency before/after | Steps 15–16 table in `performance.md` |
| Go tests cover parsing, row building, attach/kill dispatch | Steps 3, 5, 7, 9, 11, 13 |
| `picker.md` updated; `nix flake check`, `nix build .#lint` pass | Steps 17–18 |
