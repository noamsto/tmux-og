# picker: run ^x remote kill off the Update path (#781)

## File list

- `picker/remote.go` — add `sshKillRemoteSessionCtx(ctx, host, sess)` (ctx-parameterized core, with `cmd.WaitDelay` so a grandchild holding stderr/stdout can't stall past cancel/timeout — mirrors `attach.go`'s `cmd.WaitDelay = grace`), refactor `sshKillRemoteSession` to call it with `context.Background()`.
- `picker/kill.go` (new) — `killRun`/`killSpec`/`killResult`/`killItemResult`, mirroring `attach.go`'s runner shape but simpler (no phases, no subprocess pipe): forks nothing itself, just drives `sshKillRemoteSessionCtx` per target off the Update goroutine, with cancel via context.
- `picker/tui.go` — model fields (`killRun *killRunState`, `killSeq int`), messages (`killProgressMsg`, `killDoneMsg`, `killTickMsg`), `beginKill`, `finishKill`, `handleKillRunKey`, wire into `Update`/`handleKey`/mouse-guard, replace the synchronous `killRemoteSessions` call from `handleKillConfirm`. `finishKill` sanitizes every hint-line message with `sanitizeStatusText`; `beginKill` sanitizes the per-target label once, the way `beginAttach` does.
- `picker/render_list.go` — `renderKillStatus` (mirrors `renderAttachStatus`), wire into `renderHints`.
- `picker/kill_test.go` (new) — mirrors `tui_attach_test.go`'s `TestAttachUpdateNeverLaunchesSynchronously` pattern for kill: never-synchronous, progress, timeout, cancel, mocked-ssh outcomes. Reuses `fakeKillSSH`/`killRemoteRow` (`tui_test.go`) and `runBatchAsync`/`awaitAttachMsg`/`waitForFile` (`tui_attach_test.go`) — no redeclaration.
- `picker/tui_test.go` — port the seven existing synchronous `^x`/`y` kill-flow tests (`TestRemoteKillConfirmYesKillsAndForgets`, `TestRemoteKillGoneForgetsRow`, `TestRemoteKillUnreachableKeepsRow`, `TestCtrlXWithMarksConfirmsOnce`, `TestRemoteKillUnrunnableKeepsRow`, `TestRemoteKillTearsDownMirror`, `TestKillForgetSurvivesLateRemoteMsg`) to the async flow: drive the `y` keypress's `tea.Cmd` with `runBatchAsync`, wait for `killDoneMsg`, feed it through `Update`, then keep the existing assertions unchanged. This is the only behavior-preserving test change; `fakeKillSSH`/`killRemoteRow` (already there, `tui_test.go:1794`/`1808`) are reused as-is.
- `docs/agents/picker.md` — extend the Attach section (or add a Kill subsection) documenting the async kill runner.

## Design

`killRemoteSessions` (`picker/tui.go:1708`) currently loops over targets and calls `sshKillRemoteSession` synchronously inside `Update`, each call bounded by `remoteProbeTimeout` (3s) — with N marked rows this can freeze the popup for up to 3s×N (#781, same class of bug #770 fixed for attach).

Reuse the attach runner's idiom (`picker/attach.go`): a struct forked off the Update goroutine via a `tea.Cmd`, communicating back through a `progress`/`done` channel pair, with a `context.CancelFunc` for cancel. Unlike attach, kill has no subprocess-with-progress-pipe (each `sshKillRemoteSession` call is already a bounded, complete round trip), so the runner is a plain loop:

```go
type killItemResult struct {
    item      listItem
    err       error // nil = success; classified error otherwise (same states killRemoteSessions handled)
    cancelled bool  // true when the run loop observed context.Canceled for this attempt; err is unspecified/ignored
}

type killRunOutcome int8
const (
    killRunDone killRunOutcome = iota
    killRunCancelled
)

type killResult struct {
    outcome killRunOutcome
    results []killItemResult // completed attempts, in target order, up to the cancel point
}

type killRun struct {
    ctx      context.Context
    stop     context.CancelFunc
    targets  []listItem
    progress chan killProgress // lossy, buffered len(targets): one per target about to start
    done     chan struct{}
    res      killResult
    started  atomic.Bool
}

type killProgress struct {
    index, total int
    item         listItem
}
```

`run()` iterates targets; before each, checks `ctx.Err()` (stop early → `killRunCancelled`), emits `killProgress`, calls `sshKillRemoteSessionCtx(ctx, item.remoteHost, item.remoteSess)`, appends `killItemResult` — including one appended for the in-flight target that was itself interrupted by the cancel, before the loop's next `ctx.Err()` check breaks out to `killRunCancelled`. `sshKillRemoteSessionCtx` derives its own `context.WithTimeout(ctx, remoteProbeTimeout)` child so a cancel propagates into the in-flight ssh (`exec.CommandContext` sends the kill signal) instead of only gating the *next* target. It also sets `cmd.WaitDelay` (value fixed once in Step 1) — without it, a grandchild still holding stdout/stderr (a real ssh `ProxyCommand`, or a test shim's `sleep`) stalls `cmd.Run()` past both the context deadline and a user cancel, exactly the bug `attach.go`'s `cmd.WaitDelay = grace` (`attach.go:161`) already fixed for attach.

`waitKillCmd` must drain any already-ready `killProgress` non-blockingly before returning a `done` result (mirror: `select` with a `default` case checked first, or just prefer `progress` when both are ready) — otherwise, when the last target's ssh already finished by the time the loop re-arms, `select`'s random choice between a still-buffered final progress message and `done` can silently drop that last progress message, which would flake a test asserting the full index/total sequence.

A target cancelled mid-flight (`ctx.Err() != nil` inside `sshKillRemoteSessionCtx`) is reported as cancelled, not "unreachable": `classifyKillErr`'s existing `timedOut` bool conflates "deadline exceeded" and "context cancelled" (both make `ctx.Err() != nil`), so `sshKillRemoteSessionCtx` passes `errors.Is(ctx.Err(), context.DeadlineExceeded)` as the `timedOut` argument instead of the current `ctx.Err() != nil`. A killed ssh process exits non-zero (typically -1/killed), which `classifyKillErr` would otherwise map to `errRemoteKillUnrunnable` ("could not run tmux") — wrong for a cancel. So `killItemResult` gets a third field, `cancelled bool`; the run loop sets it by checking `errors.Is(r.ctx.Err(), context.Canceled)` right after `sshKillRemoteSessionCtx` returns a non-nil error, independent of what `classifyKillErr` would have said. `finishKill` checks `cancelled` before `remoteKillFailure` and writes "cancelled killing <label>" instead. The row is always kept on cancel, since whether the remote kill actually landed is unknown.

Also mirror `classifyAttach`'s exit-0-wins rule (`attach.go:280-297`): with `WaitDelay` set, an ssh that exits 0 while a lingering grandchild (a real `ProxyCommand`, or a test shim's background process) still holds stdout/stderr makes `cmd.Run()` return `exec.ErrWaitDelay` instead of nil. `sshKillRemoteSessionCtx` must treat `err == nil || (errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success())` as success, or a successful kill would be misreported as unreachable and the row wrongly kept.

Model wiring, symmetric to `attachState`/`beginAttach`/`finishAttach`/`handleAttachKey`:

- `tuiModel.killRun *killRunState` (nil = idle); `killRunState{id int, run *killRun, targets []listItem, index int, total int, cancelling bool, frame int}`.
- `beginKill(targets []listItem) tea.Cmd` — mints an id, builds `killRun`, sets `m.killRun`, returns `tea.Batch(fork, waitKillCmd, killTickCmd)` — mirrors `beginAttach`.
- `waitKillCmd(id, r)` — mirrors `waitAttachCmd`: blocks on `r.progress`/`r.done`, returns `killProgressMsg{id, killProgress}` or `killDoneMsg{id, result}`.
- `handleKillConfirm`'s `y` path does `cmd := m.beginKill(targets); return m, cmd` instead of `return m.killRemoteSessions(targets)` (matching `openMarkedRemote`'s pattern at `tui.go:1580`, not an inline `m, m.beginKill(targets)` — Go leaves evaluation order unspecified when a pointer-receiver call mutates `m` next to reading it in the same return).
- `Update` cases for `killProgressMsg`/`killDoneMsg`/`killTickMsg`, guarded by `m.killRun == nil || msg.id != m.killRun.id` exactly like the attach cases.
- `finishKill(res killResult) (tuiModel, tea.Cmd)` — replays the exact bucketing `killRemoteSessions` did (success/gone → forget + optional "was already gone" message; classified failure → `remoteKillFailure` message), running every message through `sanitizeStatusText` before joining into `m.statusMsg` (the existing synchronous path never sanitized because `remoteHost`/`remoteSess` on a real listed row can't carry control chars, but the async hint line is a new render surface and the repo rule is "sanitized before any local format renders" — apply it here too rather than argue the risk is zero). Calls `m.forgetRemoteRows(forget)`, clears `m.killRun`. On `killRunCancelled`, forgets whatever completed before the cancel point; a target caught mid-flight by the cancel (`errors.Is(ctx.Err(), context.Canceled)`, distinct from a timeout) is kept and reported as "cancelled", not "unreachable".
- `handleKillRunKey(key)` — mirrors `handleAttachKey`: only esc/ctrl+c act; esc sets `cancelling` and calls `run.cancel()`; a second ctrl+c while cancelling quits.
- `handleKey` gains a `m.killRun != nil` branch immediately after the existing `m.attach != nil` branch, both ahead of `len(m.killConfirm) > 0` (kill-run and attach are mutually exclusive: `handleKillConfirm`'s confirm step is unreachable while an attach or kill-run is active, since `handleKey` routes to those first).
- Mouse guards (`tea.MouseWheelMsg`/`tea.MouseClickMsg`) extend their `m.attach != nil` check to `m.attach != nil || m.killRun != nil`.
- `renderHints` gains `if m.killRun != nil { return m.renderKillStatus(dim, key) }` ahead of the `killConfirm` branch; `renderKillStatus` mirrors `renderAttachStatus`'s reserved-suffix layout, showing "killing " + the sanitized per-target label (set once in `beginKill`, the way `beginAttach` sanitizes `label` at `tui.go:1594`) + " (i/N)" with the same spinner, and "esc:cancel" / "cancelling…, ^c:quit" hints.

No change to `forgetRemoteRows`, the `refreshMsg`/`forgotten` filtering, or `remoteKillSessionBody`/`classifyKillErr` (owned by #753, #785 territory per dispatcher notes).

`killRemoteSessions` itself is deleted (superseded); `sshKillRemoteSession(host, sess)` keeps its existing signature and behavior so the #753 tests (`TestKillRemoteSessionReachesScratchServer` et al.) stay green unchanged.

## Steps

- [ ] **Step 1: ctx-parameterized kill call**
  `picker/remote.go`: add `sshKillRemoteSessionCtx(ctx context.Context, host, sess string) error` — same body as current `sshKillRemoteSession`, but takes `ctx` as the parent for `context.WithTimeout`, sets `cmd.WaitDelay = killWaitDelay` (a package `var killWaitDelay = 500 * time.Millisecond`, shrinkable in a test the way `attachBudgets` is — this is the only WaitDelay value the plan specifies, replacing any other figure), treats `err == nil || (errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState.Success())` as success (mirrors `classifyAttach`'s exit-0-wins rule, `attach.go:283`), and passes `errors.Is(ctx.Err(), context.DeadlineExceeded)` (not the current bare `ctx.Err() != nil`) as `classifyKillErr`'s `timedOut` argument, so a user cancel (`context.Canceled`) doesn't get misclassified as a timeout. Change `sshKillRemoteSession` to `return sshKillRemoteSessionCtx(context.Background(), host, sess)`.
  Prove: `nix build .#default -L 2>&1 | tail -30` compiles; `cd picker && go test ./... -run TestKillRemoteSessionReachesScratchServer -v` still passes (signature unchanged).

- [ ] **Step 2: kill runner (new file, no model wiring yet)**
  `picker/kill.go`: add `killItemResult`, `killRunOutcome`/`killRunDone`/`killRunCancelled`, `killResult`, `killProgress`, `killRun` (fields per design), `newKillRun(targets []listItem) *killRun`, `(*killRun) cancel()`, `(*killRun) result() killResult`, `(*killRun) emit(killProgress)`, `(*killRun) finish(killResult)`, `(*killRun) run()`.
  Prove: `cd picker && go build ./...` compiles (new file unused is fine — Go doesn't error on unused unexported funcs, only unused locals/imports).

- [ ] **Step 3: failing test for never-synchronous kill**
  `picker/kill_test.go`: write `TestKillUpdateNeverLaunchesSynchronously` mirroring `tui_attach_test.go`'s `TestAttachUpdateNeverLaunchesSynchronously` — stage `killConfirm` via `tuiModel{killConfirm: []listItem{killRemoteRow(...)}}`, drive the `y` keypress through `m.Update(tea.KeyPressMsg{Code: 'y'})` (not `handleKillConfirm` directly, so the test also covers `handleKey`'s routing), using a PATH-shadowed `ssh` shim (extend `fakeKillSSH` from `tui_test.go` to accept a script body, since its current form always exits immediately with a code — add a sibling `fakeKillSSHScript(t, script string) string` rather than redeclaring the helper) that `exec`s `sleep 30` with no shell wrapper holding the pipes. Assert the returned `Update` takes <250ms and the shim has not run yet, then drive the returned `tea.Cmd` with `runBatchAsync` and observe it run via `waitForFile`. This test will fail to compile/pass until Step 4 wires the model — write it now, confirm it fails for the right reason (`m.killRun` doesn't exist yet / `beginKill` undefined).
  Prove: `cd picker && go test ./... -run TestKillUpdateNeverLaunchesSynchronously -v` fails (compile error naming the missing symbols).

- [ ] **Step 4: wire the runner into tuiModel**
  `picker/tui.go`: add `killRunState` struct and `killRun *killRunState` + `killSeq int` fields to `tuiModel`; add `killProgressMsg{id int; progress killProgress}`, `killDoneMsg{id int; result killResult}`, `killTickMsg{id int}`; add `waitKillCmd(id int, r *killRun) tea.Cmd` (mirrors `waitAttachCmd`); add `killTickCmd(id int) tea.Cmd` (mirrors `attachTickCmd`, reuse `attachTickInterval`); add `beginKill(targets []listItem) tea.Cmd` (sanitizes the first target's label via `sanitizeStatusText`, the way `beginAttach` does at `tui.go:1594`); add `finishKill(res killResult) (tuiModel, tea.Cmd)` porting the bucketing logic out of `killRemoteSessions`, sanitizing every joined message; add `handleKillRunKey(key string) (tea.Model, tea.Cmd)` mirroring `handleAttachKey`. Wire `Update` cases for the three new msg types (guarded by id match, mirroring the attach cases at `tui.go:545-568`). In `handleKey`, add `if m.killRun != nil { return m.handleKillRunKey(key) }` immediately after the existing `if m.attach != nil` branch. In `handleKillConfirm`, replace `return m.killRemoteSessions(targets)` with `cmd := m.beginKill(targets); return m, cmd`. Extend the two mouse-guard conditions (`tea.MouseWheelMsg`, `tea.MouseClickMsg`) to also skip when `m.killRun != nil`. Delete `killRemoteSessions` (its logic now lives in `finishKill`).
  Prove: `cd picker && go build ./...` compiles; `go test ./... -run TestKillUpdateNeverLaunchesSynchronously -v` passes.

- [ ] **Step 4b: a `driveKill` test helper, then port the seven synchronous kill-flow tests to the async flow**
  `picker/tui_test.go`: `waitKillCmd` (like `waitAttachCmd`) yields exactly one message per invocation — `runBatchAsync` says so directly (`tui_attach_test.go:24-29`) — and `run()` emits a `killProgressMsg` before every target, so a single `runBatchAsync`+`awaitAttachMsg`+`Update` round trip is not enough even for a one-target kill (the first message racing back is as likely to be progress as done), and `TestCtrlXWithMarksConfirmsOnce` has two targets needing two round trips. Add a shared helper:
  ```go
  // driveKill runs cmd (and every Cmd Update returns) until a killDoneMsg for
  // this run lands, re-arming waitKillCmd after each killProgressMsg/killTickMsg
  // the way bubbletea's own runtime would — mirrors the by-hand loop
  // TestAttachUpdateNeverLaunchesSynchronously already does for attach.
  func driveKill(t *testing.T, m tea.Model, cmd tea.Cmd) tuiModel {
      t.Helper()
      for {
          ch := runBatchAsync(cmd)
          msg := awaitAttachMsg(t, ch, 5*time.Second, func(msg tea.Msg) bool {
              switch msg.(type) {
              case killProgressMsg, killDoneMsg:
                  return true
              default:
                  return false
              }
          })
          m, cmd = m.Update(msg)
          if _, ok := msg.(killDoneMsg); ok {
              return m.(tuiModel)
          }
      }
  }
  ```
  Then change `TestRemoteKillConfirmYesKillsAndForgets`, `TestRemoteKillGoneForgetsRow`, `TestRemoteKillUnreachableKeepsRow`, `TestCtrlXWithMarksConfirmsOnce`, `TestRemoteKillUnrunnableKeepsRow`, `TestRemoteKillTearsDownMirror`, `TestKillForgetSurvivesLateRemoteMsg` (each currently `next, _ := m.handleKey(tea.KeyPressMsg{Code: 'y'})` and asserts on `next` immediately) to `next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y'}); mm := driveKill(t, next, cmd)`, then keep every existing assertion (sentinel ran, `remoteItems`/cache/`statusMsg`/`marked`/`stopBridgeDaemonFn` state) unchanged against `mm`, since `finishKill` reproduces `killRemoteSessions`'s exact bucketing. `TestCtrlXOnRemoteRowStagesConfirmation` and `TestRemoteKillConfirmDefaultIsNo` need no change — they never reach `y`. This is the only test-behavior change in the PR; no assertion's meaning changes, only how the result is obtained.
  Prove: `cd picker && go test ./... -run 'TestRemoteKillConfirmYesKillsAndForgets|TestRemoteKillGoneForgetsRow|TestRemoteKillUnreachableKeepsRow|TestCtrlXWithMarksConfirmsOnce|TestRemoteKillUnrunnableKeepsRow|TestRemoteKillTearsDownMirror|TestKillForgetSurvivesLateRemoteMsg' -v` — all seven pass.

- [ ] **Step 5: render the kill-in-flight hint line**
  `picker/render_list.go`: add `renderKillStatus(dim, key lipgloss.Style) string` mirroring `renderAttachStatus` (spinner + "killing " + sanitized label + " (i/N)" + reserved esc:cancel / ^c:quit suffix). Wire into `renderHints`: `if m.killRun != nil { return m.renderKillStatus(dim, key) }` ahead of the existing `if len(m.killConfirm) > 0` branch.
  Prove: `cd picker && go build ./...` compiles.

- [ ] **Step 6: progress, timeout, cancel, and outcome tests**
  `picker/kill_test.go`: add `TestKillProgressAdvancesThroughTargets` (multi-target run using `driveKill`-style manual re-arming — assert the `killProgressMsg` sequence's index/total before the final `killDoneMsg`), `TestKillCancelStopsRemainingTargets` (esc mid-run, using a `sleep`-exec'd shim per target with no shell wrapper holding the pipes so `WaitDelay`/cancel actually cuts it short quickly; assert `killRunCancelled` outcome, that a target after the cancel point never ran via a sentinel file per target — reuse `waitForFile`/`assertNoFile` from `attach_test.go` — and that the cancelled in-flight target's `killItemResult.cancelled` is true and its `finishKill` message reads "cancelled", not "unreachable"), `TestKillTimeoutSurfacesSanitizedError` (a fake ssh that hangs past `remoteProbeTimeout`, using a control-character-laden session name; assert the per-target result carries `errRemoteUnreachable`, that the resulting `statusMsg` after `finishKill` matches the sanitized `remoteKillFailure` text, and that neither the raw control chars nor the raw name appear in `statusMsg` or `renderHints()`'s output), `TestKillOutcomesMixedSuccessAndFailure` (mirrors old `killRemoteSessions` behavior: one target succeeds, one is "already gone", one fails — assert `forgetRemoteRows` was applied to the first two and `statusMsg` joins the right messages), `TestKillKeysAndMouseIgnoredDuringKillRun` (mirrors `TestAttachKeysIgnoredDuringAttach`/`TestAttachMouseIgnoredDuringAttach` for `killRun`), `TestKillStaleMessagesIgnored` (a `killDoneMsg`/`killProgressMsg` with a stale id is a no-op, mirrors `TestAttachStaleMessagesIgnored`). Use `assertNoFile`/`waitForFile` (`attach_test.go`), `runBatchAsync`/`awaitAttachMsg` (`tui_attach_test.go`), and `driveKill` (Step 4b, `tui_test.go`) directly — package-private helpers in the same `package main`, no redeclaration — plus `fakeKillSSH`/`killRemoteRow`/`fakeKillSSHScript` (`tui_test.go`) for the ssh shim on `PATH`.
  Prove: `cd picker && go test ./... -run TestKill -v` — all six new tests pass.

- [ ] **Step 7: full picker suite + existing kill-flow regression**
  Prove: `cd picker && go test ./...` — all pass, including `TestClassifyKillErr`, `TestRemoteKillSessionBodyQuotesHostileName`, `TestRemoteKillSessionBodyFishSafe`, `TestKillRemoteSessionReachesScratchServer` (#753), the seven ported tests from Step 4b, `TestCtrlXOnRemoteRowStagesConfirmation`/`TestRemoteKillConfirmDefaultIsNo` (unchanged), and the full `tui_attach_test.go` suite (#770) untouched and green.

- [ ] **Step 8: doc update**
  `docs/agents/picker.md`: extend the Attach section (added by #780) with a short note that `^x` remote kill uses the same off-Update runner idiom (link the two flows so a future reader finds both). Keep it to what's non-obvious: the runner shape is shared, kill has no phases because each attempt is one bounded round trip, and it cross-references the design section above conceptually (no need to duplicate prose — one or two sentences).
  Prove: `rg -n "off-Update|killRun" docs/agents/picker.md` shows the new note.

- [ ] **Step 9: full gate**
  Prove: `nix build .#default -L 2>&1 | tail -40` (build), `nix flake check -L 2>&1 | tail -60` (bats + Go tests + conf assertions), `nix build .#lint -L 2>&1 | tail -60` (pre-commit hooks) — all green.

## Acceptance

- "A test proving `^x` on a remote row never runs ssh synchronously in Update" → `TestKillUpdateNeverLaunchesSynchronously` (Step 3/4).
- "Progress/timeout/cancel behaviour for the kill, with tests using mocked commands; failure surfaces a sanitized error on the hint row" → `TestKillProgressAdvancesThroughTargets`, `TestKillCancelStopsRemainingTargets`, `TestKillTimeoutSurfacesSanitizedError` (Step 6).
- "Existing #753 kill-flow tests stay green" → Step 7: `TestClassifyKillErr`, `TestRemoteKillSessionBodyQuotesHostileName`, `TestRemoteKillSessionBodyFishSafe`, `TestKillRemoteSessionReachesScratchServer` pass unmodified; the seven behavioral kill-flow tests in `tui_test.go` pass with their assertions unchanged, ported to the async flow (Step 4b).
- "`nix flake check`, `nix build .#lint` pass" → Step 9.
