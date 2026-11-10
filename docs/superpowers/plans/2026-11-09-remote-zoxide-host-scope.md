# Remote zoxide dirs in the picker's host scope (#1099)

Goal: Tab-cycled single-host scope (`scopeHost`) in the **session** picker gains a
"── New session ──" section after that host's Remote block, filled with the host's zoxide
dirs. Enter creates the session on the host (`OG_REMOTE_NEW_DIR`) and opens it as a mirror via the
existing supervised attach. `scopeAll`, local scope, window mode and emit mode are unchanged.

## Design decisions

- **Fetch directly over ssh, not through `og-remote-picker`'s emit path.** That path runs the
  remote's whole interactive picker; we need one list. One bounded `ssh … sh -c '<script>'`
  (`remoteZoxideScript`) running `zoxide query -l | while IFS= read -r l; do [ -d "$l" ] && printf %s "$l" && echo; done | head -n 300` (both the `command -v` and the `zoxide query -l` get `2>/dev/null`; missing zoxide → empty stdout, exit 0 from `head`).
  The remote-side `-d` filter drops dead dirs and, because a newline inside a path splits it into
  fragments on the line-delimited wire, almost always drops the truncated fragments too (a fragment
  that happens to name a real dir is the residual limitation, documented in picker.md). `printf %s`
  not `echo "$l"` (dash's echo expands backslashes). Binary resolution mirrors `remoteTmuxBin`:
  `command -v zoxide || /etc/profiles/per-user/$(id -un)/bin/zoxide`; absent → empty stdout, exit 0
  (normal case, no section, no error row). Script contains no `'` or `\` (login shell may be fish),
  wrapped via `shellQuote`. ssh argv copies `sshProbe`'s `-o BatchMode=yes -o ConnectTimeout=2 -T host -- cmd`
  under a `remoteProbeTimeout` context (sshProbe itself is fixed to `remoteProbeResult`, so a small
  sibling helper is written; never a non-batch ssh, a prompt would land in the picker's pty).
- **Distinct row fields, never `createPath`/`remoteHost`.** New `listItem` fields `remoteDirHost`,
  `remoteDirPath` (raw, what the remote receives), `remoteDirName` (derived, sanitized). `target` is
  `"remotedir:<host>:<path>"` (unique key for `restoreCursor`). `isRemoteRow` stays false (so
  `remote.prefix` tree scaffolding in `withFilter` never wraps it), `remoteHost` stays empty (so
  every Remote-section consumer — preview, `^t`, `^x` kill, `^o` — ignores it). `isZoxideHeader` is
  reused for the divider.
- **Two stages, sanitize once.** Transport: `sshRemoteZoxide(host) ([]string, error)` splits stdout into
  entries via `splitRemoteZoxideOutput(string) []string`; the seam is
  `var fetchRemoteZoxideFn = sshRemoteZoxide` of type `func(host string) ([]string, error)` (error = ssh
  failure/timeout/non-zero exit). The Cmd returns `remoteZoxideMsg{host string; entries []string; err error}`
  (the handler, not the Cmd, runs `filterRemoteDirs` when `err == nil`). Argv helper: `remoteZoxideArgv(host) []string`
  (same flags as `sshProbe`). Filter: `filterRemoteDirs(entries []string) []remoteDir`
  (testable with an entry that really contains a newline): drop empty, non-absolute,
  >`maxEmitFieldLen`, any entry containing `\n`, `\r`, NUL (`validEmitField`) or `\` (og-remote-open's
  `shell_quotable` rejects it), keep at most 100 candidates. Name = `sanitizeStatusText(sessionNameFromPath(raw))`
  (derived from the raw path first, so the 200-rune cut never shortens it from the middle; empty → dropped);
  `sanitizeStatusText(raw)` is used only for display and `searchText`. `@picker_zoxide_exclude` patterns are
  matched against the raw remote path via `isExcluded`/`parseExcludePatterns` with no local tilde/HOME expansion
  (verified in Step 4; documented if `parseExcludePatterns` does any).
- **Handler.** On `msg.err != nil`: clear `fetching`, leave `done` unset (nothing cached). On success: store
  `filterRemoteDirs(entries)`, mark `done` (zero dirs still counts as done). So a host that needed auth is
  refetched on the next Tab entry after `og-remote-auth`. Display text and derived name come from `sanitizeStatusText(path)`
  (strips ESC sequences, C0/C1 controls, bidi); the name is `sanitizeStatusText(sessionNameFromPath(raw))` (see Handler bullet order)
  (empty → dropped). The raw path rides only as an env value to og-remote-open, which shell-quotes it.
  `remoteDir` is stored per host, so the 1s `recombine()` does no parsing. Remote paths are never `$HOME`→`~` shortened (the remote home differs).
- **Cache + async.** `tuiModel.remoteZoxide map[string]remoteZoxideEntry` (`dirs`, `fetching`),
  per host, popup lifetime. Only a **successful** fetch (ssh exit 0, even with zero dirs) is cached as done;
  a failed one clears `fetching` and caches nothing, so a host that needed auth (Tab can land on it while its
  row is still the `pendingRemoteItems` placeholder) is fetched again on the next Tab entry after
  `og-remote-auth` — one fetch per Tab entry, no loop. Tab handler, when the new scope is
  `scopeHost`, `!windowMode`, `emitPath==""`, entry neither done nor `fetching`, and the host's block row is
  — skip only when `block[0]` carries `remoteInert`, `remoteNeedsAuth`, `remoteTailscaleCheck` or
  `remoteUnreachable`; an absent or pending (unflagged) block fetches → mark `fetching` and return
  `remoteZoxideCmd(host)` alongside `loadPreviewCmd()`. Cmd runs `fetchRemoteZoxideFn(host)` (seam)
  bounded by `remoteProbeTimeout`, returns `remoteZoxideMsg{host, entries, err}`. The Tab handler returns `tea.Batch(m.loadPreviewCmd(), remoteZoxideCmd(host))` with `fetching` set on the returned `m`. New scopeHost tests set `m.scope = hostScope{kind: scopeHost, host: ...}`. Handler stores into the
  host's entry, `recombine().withFilter()`, `restoreCursor(keep)`, clamps to a selectable row
  (same as `remoteMsg`/`zoxideMsg`). A result for a non-current host just fills the cache.
  Nothing touches first paint or Tab latency.
- **Unreachable on the host row.** `remoteHostRowForState` sets `row.remoteUnreachable = true` for
  `remoteProbeUnreachable` (field is documented session-row only today; verify no host-row consumer
  breaks — `markable`/`isKillableRemoteSession` already exclude host rows by `remoteSess == ""`).
- **Section gating at build time** (`scopedItems`, `m.scope.kind == scopeHost && !windowMode &&
  emitPath == ""`): block exists and `block[0]` not inert/needsAuth/tailscaleCheck/unreachable (so a
  late probe turning the host inert hides the section). Filter: `isExcluded(rawPath, patterns)` with
  `@picker_zoxide_exclude`; drop dirs whose derived name equals a session on that host — union of
  block children (`seen`), `block[0].remoteHostSessionIDs` keys (live probe incl. bridged), and
  `m.mirrors` for the host; dedupe duplicate derived names (first rank wins); cap
  `maxZoxideSuggestions`. Path-based hiding is name-based only: the session probe returns names, not
  cwd (documented).
- **Activation.** `activateCurrent`: before the `createPath` branch, `remoteDirPath != ""` →
  `m.beginAttach(listItem{remoteHost: dirHost, remoteSess: dirName, remoteDirPath: path}, nil)`.
  `beginAttach` copies `first.remoteDirPath` into `attachSpec.newDir`; `buildAttachCmd` appends `OG_REMOTE_NEW_DIR=<path>` (never with `restore`).
  Requires `item.target != ""` guard to pass (it does). `createAndSwitch` becomes seam
  `createSession = createAndSwitch` so tests prove it is never called.
- **Inertness elsewhere.** `^x` on a remote dir row: explicit early `return m, nil` placed before the
  `item.target != "" && item.remoteHost == ""` fallthrough (a `remotedir:<host>:<path>` target contains
  `:` and would otherwise reach a local `tmux kill-window -t`); no `zoxideForget`, no kill, no `killConfirm`;
  hint suppressed for it. Preview: static text (host, path, "Enter creates the session on <host>") — no
  `listDir`. `markable` already false (no `isRemoteRow`); `relayable` (wall) gets
  `remoteDirPath == ""`; `matchRank` + the `withFilter` header re-insertion treat remote dir rows as
  `rankZoxide` / suggestion rows via helper `isSuggestionRow`.

## File list

- `picker/remote_zoxide.go` (new): script/body consts, `sshRemoteZoxide`, `splitRemoteZoxideOutput`,
  `filterRemoteDirs`, `remoteDir`, `fetchRemoteZoxideFn` seam, `remoteZoxideCmd`, `(m tuiModel) remoteZoxideRows`.
- `picker/remote_zoxide_test.go` (new): parse/sanitize, rows, fetch-message tests.
- `picker/tui.go`: rewrite the `scopedItems` doc comment ("Zoxide rows are local-scope only"), `listItem` fields
  (and the `remoteUnreachable` field comment: also set on an unreachable host row, window mode included), `tuiModel.remoteZoxide`, `remoteZoxideMsg` + handler, Tab handler
  trigger, `scopedItems` append, `activateCurrent` branch, `createSession` seam, `^x` guard, preview
  case, `relayable`, `matchRank`/`withFilter` header re-insertion helper, `renderHints` ^x label.
- `picker/attach.go`: `attachSpec.newDir`, `buildAttachCmd` env.
- `picker/remote.go`: `remoteUnreachable` on the unreachable host row.
- `picker/render_list.go`: kill/forget hint for remote dir rows (hidden).
- `picker/scope_test.go`: update `TestScopedItemsExcludesZoxideRows` (scopeAll still none, local unchanged).
- `docs/agents/picker.md`: rewrite the "No zoxide rows in host/all scope" sentence; document the section,
  fetch, sanitization, activation path.
- `docs/agents/scripts.md`: one clause on the `tmux-session-picker` row (host scope shows remote zoxide dirs) and
  on the `og-remote-open` row (`OG_REMOTE_NEW_DIR` is also set by the session picker's host scope).
- `scripts/og-remote-open.sh`: comment at ~728-732 names the session picker's host scope as a second caller (comment only).
- `docs/superpowers/plans/2026-11-09-remote-zoxide-host-scope.md`: this plan.

## Steps

- [ ] **Step 1: failing tests for the filter** — `picker/remote_zoxide_test.go`:
  `TestFilterRemoteDirsSanitizes` (ESC-sequence parent dir → sanitized name, raw path kept; an entry that
  really contains `\n`, a `\r`/NUL entry, a backslash entry, a relative path, empty: all dropped, and no
  fragment of the newline entry appears), `TestSplitRemoteZoxideOutput` (split yields entries; documents that
  a fragment is a separate entry), `TestRemoteZoxideScriptQuoting` (no `'`/`\` in script; `remoteZoxideArgv` has
  `BatchMode=yes`). Run `cd picker && go test -run 'FilterRemoteDirs|SplitRemoteZoxide|RemoteZoxideScript' ./...` → FAIL.
- [ ] **Step 2: implement `remote_zoxide.go` split + filter + script + `sshRemoteZoxide` + seam.** Same command → PASS.
- [ ] **Step 3: failing tests for `scopedItems`** — in `scope_test.go`/`remote_zoxide_test.go`: scopeHost with
  fetched dirs yields divider + rows after host block (and after `(mirrored)` rows); scopeAll none; local
  unchanged; inert/unreachable host none; exclude pattern; name collision with live session / mirror hidden;
  window mode none. Update `TestScopedItemsExcludesZoxideRows`. → FAIL.
- [ ] **Step 4: add `listItem` fields, model map, `remoteZoxideRows`, `scopedItems` append,
  `remoteUnreachable` on host row.** → `go test -run 'Scoped|RemoteZoxide' ./...` PASS.
- [ ] **Step 5: failing tests for behaviour** — `TestEnterOnRemoteDirRowStartsAttachNotCreate`
  (`createSession` seam fails the test; `m.attach.run.spec` has host/sess/newDir; `buildAttachCmd` env has
  `OG_REMOTE_NEW_DIR=<path>`, no `OG_REMOTE_RESTORE`), `TestCtrlXOnRemoteDirRowDoesNotForget` (zoxideForget
  seam — introduce `zoxideForgetFn` var), `TestRemoteDirRowNoLocalPreview`, `TestLateRemoteZoxideForOtherHost`
  (scope host A, message for host B: items unchanged, cursor selectable; message for A while cursor off-row
  clamps), `TestTabFetchesOncePerHost` (Tab to host, cmd returned; second visit no cmd; inert host no cmd;
  window/emit mode no cmd; seam stub returning `(nil, errors.New(..))` → nothing cached, next Tab refetches, while `(nil, nil)` → cached as done, no refetch; missing host block fetches),
  `TestRemoteDirRowNotMarkableNotRelayable`. The `^x` test also asserts `killConfirm` empty and cmd nil; the
  escape-path test asserts on the row's `display`/`plain`/`searchText`, not only the filter output. → FAIL.
- [ ] **Step 6: implement activation branch, `attachSpec.newDir`, `createSession`/`zoxideForgetFn` seams,
  `^x` guard + hint, preview case, `relayable`, `matchRank`/header helper, Tab trigger, `remoteZoxideMsg`
  handler.** → `cd picker && go test -race ./...` PASS.
- [ ] **Step 7: filter test** — `TestFilterKeepsRemoteDirHeader` (type a query in host scope: divider re-inserted
  before first matching remote dir row, ranked after the Remote block). Fix `withFilter` if red.
- [ ] **Step 8: docs** — `picker.md` (replace the host-scope bullet sentence; add fetch/gating/sanitization/
  activation bullet, note name-based hide, no remote forget), `scripts.md` clause. `rg -n 'No zoxide rows' docs/agents`
  → empty.
- [ ] **Step 9: gates** — `nix build .#default`, `nix flake check`, `nix build .#lint --accept-flake-config`.

## Acceptance map

| Item | Settled by |
| --- | --- |
| scopedItems divider/rows; scopeAll none; local unchanged | Step 3 tests |
| Enter → remote-open seam with path+name, never `createAndSwitch` | Step 5 `TestEnterOnRemoteDirRow…` |
| `^x` never `zoxideForget` | Step 5 `TestCtrlXOnRemoteDirRow…` |
| escape/control sanitized; newline dropped | Step 1 test |
| late result for other host leaves list/cursor valid | Step 5 `TestLateRemoteZoxide…` |
| picker.md updated | Step 8 grep |
| nix build / flake check / lint | Step 9 |
