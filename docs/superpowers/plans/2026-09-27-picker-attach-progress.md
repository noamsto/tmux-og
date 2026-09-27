# Plan: picker attach progress, cancel and faults (#770)

Spec: `docs/superpowers/specs/2026-09-27-picker-attach-progress-design.md`
(D1–D8 below refer to it).

Orchestration consult: survey did not trip — two components (the Go picker
package, the bash launcher), no shared interface beyond an env-gated fd
protocol the launcher's other callers never set.

## File list

| file | purpose |
|---|---|
| `scripts/og-remote-open.sh` | D1 phase lines on `OG_REMOTE_OPEN_PROGRESS_FD`; fd closed before the daemon; D2 rollback trap, `attached=1` before both final `switch-client`s; `set -m` for the nohup fallback |
| `tests/remote-cold-start.bats` | bats for phase lines, fd not leaked to the daemon, rollback/no-rollback on TERM (fresh and dedup paths) |
| `picker/attach.go` (new) | D3/D4 runner (`attachRun`), phase table, budgets, `sanitizeStatusText` (D8), `attachSupervisor` |
| `picker/attach_linux.go` (new) | `attachSysProcAttr()` — `Setsid` + `Pdeathsig: SIGTERM` |
| `picker/attach_other.go` (new, `//go:build !linux`) | `attachSysProcAttr()` — `Setsid` only |
| `picker/attach_test.go` (new) | runner, sanitizer and supervisor tests with fake `#!/bin/sh` launchers |
| `picker/tui.go` | D5/D6 model wiring: `attach` state, messages, key/mouse gating, `beginAttach`, multi-open remainder, `runTUI` supervisor + SIGHUP |
| `picker/render_list.go` | D7 attach status line in `renderHints` |
| `picker/remote.go` | delete `openRemoteBridge` (replaced by the runner); `remoteOpenBin(opts)` helper shared by the runner and `launchRemoteBridgeDetached` |
| `picker/tui_attach_test.go` (new) | model-level tests (non-blocking Update, keys, outcomes, width, multi-open) |
| `picker/tui_test.go`, `picker/scope_test.go`, `picker/remote_test.go` | adapt the three tests that call `openMarkedRemoteWith` / `openRemoteBridge` |
| `docs/agents/picker.md` | new "Attach (remote session open)" section |
| `docs/agents/scripts.md` | `og-remote-open` row: progress fd + rollback trap; `tmux-session-picker` row: pointer to the attach section |
| `docs/superpowers/specs/…-design.md`, this plan | committed alongside |

## Interfaces (fixed — both Go steps and the script step code against these)

Script ↔ picker (D1): env `OG_REMOTE_OPEN_PROGRESS_FD=<digits>`; lines on that
fd, each exactly one of `connect`, `start-server`, `restore`, `create`,
`mirror`, newline-terminated. Nothing else is ever written there.

Go (`picker/attach.go`):

```go
type attachPhase string
const (
	phaseLaunch      attachPhase = "launch" // implicit, before the first line
	phaseConnect     attachPhase = "connect"
	phaseStartServer attachPhase = "start-server"
	phaseRestore     attachPhase = "restore"
	phaseCreate      attachPhase = "create"
	phaseMirror      attachPhase = "mirror"
)
var attachBudgets = map[attachPhase]time.Duration{launch:10s, connect:20s, start-server:45s, restore:90s, create:30s, mirror:15s}
var attachKillGrace = 5 * time.Second
func attachPhaseLabel(p attachPhase) string // starting|connecting|starting tmux server|restoring session|creating session|attaching

type attachOutcome int8 // attachOK, attachFailed, attachTimedOut, attachCancelled
type attachResult struct {
	outcome attachOutcome
	phase   attachPhase   // phase in effect when it ended
	budget  time.Duration // the expired budget, set for attachTimedOut
	msg     string        // attachFailed: sanitized launcher message (never empty)
}
type attachSpec struct {
	bin, host, sess string
	restore        bool
}
type attachRun struct { /* ctx context.Context; stop context.CancelFunc; spec; budgets; grace; progress chan attachPhase (cap 16, lossy send); done chan struct{}; res attachResult (valid after done closed) */ }
func newAttachRun(spec attachSpec) *attachRun        // allocation only; reads attachBudgets/attachKillGrace
func (r *attachRun) run()                             // blocking; forks, supervises, sets result, closes done. Idempotent-safe to call once.
func (r *attachRun) cancel()                          // non-blocking
func (r *attachRun) result() attachResult             // call after <-done
func buildAttachCmd(spec attachSpec) *exec.Cmd        // argv [host (sess)], env += OG_REMOTE_OPEN_PROGRESS_FD=3, SSH_ASKPASS_REQUIRE=never, OG_REMOTE_RESTORE=1 iff restore; Stdout nil; SysProcAttr attachSysProcAttr(); WaitDelay = attachKillGrace
func sanitizeStatusText(s string) string
type attachSupervisor struct{ /* mu; cur *attachRun */ }
func (s *attachSupervisor) track(r *attachRun)          // nil receiver = no-op
func (s *attachSupervisor) stop(wait time.Duration)      // nil receiver = no-op; cancel cur, wait for done up to wait
```

Status texts (D5), one format for every phase — `<label>` is the sanitized
`host/sess` (host alone when sess is empty), `<phase label>` from
`attachPhaseLabel`, `<n>` the budget in whole seconds:

- failed: `<label>: <msg> — enter to retry`
- timed out: `<label>: timed out <phase label> after <n>s — enter to retry`
  (e.g. `lab/mono: timed out connecting after 20s — enter to retry`)
- cancelled: `cancelled opening <label>`

Go (`picker/tui.go`) messages: `attachPhaseMsg{id int; phase attachPhase}`,
`attachDoneMsg{id int; result attachResult}`, `attachTickMsg{id int}`;
`waitAttachCmd(id int, r *attachRun) tea.Cmd` (select on progress / done),
`attachTickCmd(id int) tea.Cmd` (100ms). Model fields `attach *attachState`,
`attachSeq int`, `attachSup *attachSupervisor`. Package var
`launchDetached = launchRemoteBridgeDetached` (test seam).

## Steps

### Component A — launcher (`scripts/og-remote-open.sh`, bats)

- [ ] **Step 1: failing bats for phase lines.** `tests/remote-cold-start.bats`: add cases, each running `OG_REMOTE_OPEN_PROGRESS_FD=3 bash "$LAUNCHER" … 3>"$BATS_TEST_TMPDIR/phases"`:
  (a) cold start (`tp-g6`, no server) → phases file is exactly `connect\nstart-server\nconnect\nmirror\n` (the probe had no session, so the window is looked up by the late `list-windows`, labelled connect);
  (b) server running, `tp-g6` (touch `$REMOTE_SERVER`) → `connect\nmirror\n`;
  (c) restore (`OG_REMOTE_RESTORE=1 … tp-g6 work`, no server) → `connect\nrestore\nstart-server\nrestore\nmirror\n`;
  (d) new dir (`OG_REMOTE_NEW_DIR=/srv … tp-g6 proj`, no server) → `connect\ncreate\nstart-server\ncreate\nmirror\n`;
  (e) `FAKE_NO_WINDOW=1` with the server running → exit 1 and phases `connect\nconnect\n` (late list-windows is labelled connect);
  (f) same as (b) but **without** the env var → phases file empty, exit 0;
  (g) env set to `abc` → phases file empty, exit 0, and stderr has no `Bad file descriptor`;
  (g2) `OG_REMOTE_OPEN_PROGRESS_FD=9` with fd 9 not open, case (b) → exit 0, empty stderr;
  (g3) the variable is not passed on: the daemon stub also writes `${OG_REMOTE_OPEN_PROGRESS_FD-unset}` to `$BATS_TEST_TMPDIR/daemon-env`; case (b) with the env set → `unset`;
  (h) fd not leaked to the daemon: replace the `og-remote-bridge-daemon` stub with one that writes `open`/`closed` per `[ -e /dev/fd/3 ]` to `$BATS_TEST_TMPDIR/daemon-fd3`, run (b), poll ≤2s for the file, expect `closed`.
  Command: `bats tests/remote-cold-start.bats` inside `nix develop` → the new cases fail, the old ones pass.
- [ ] **Step 2: emit phases.** In `og-remote-open.sh`: after arg parsing, `progress_fd=""; [[ ${OG_REMOTE_OPEN_PROGRESS_FD:-} =~ ^[0-9]+$ ]] && progress_fd=$OG_REMOTE_OPEN_PROGRESS_FD; unset OG_REMOTE_OPEN_PROGRESS_FD` (never exported onward: the daemon's hand-off re-runs this script from its own environment, where fd 3 may be something else entirely), and `phase() { [[ -n $progress_fd ]] || return 0; printf '%s\n' "$1" 2>/dev/null >&"$progress_fd" || true; }` (2>/dev/null **before** the fd redirection so a closed fd prints nothing). Emit: `phase connect` before the probe; `phase start-server` first line of `start_remote_server`; `phase restore` at the top of the restore block and again right after its `start_remote_server`; `phase create` likewise in the new-dir block; `phase connect` at the top of the `[[ -z $win ]]` list-windows block; `phase mirror` before `base_local_sess=`. Before the daemon launch: `if [[ -n $progress_fd ]]; then exec {progress_fd}>&-; progress_fd=""; fi`. Command: `bats tests/remote-cold-start.bats` → all green; `shellcheck scripts/og-remote-open.sh` clean.
- [ ] **Step 3: failing bats for the rollback trap.** A helper `run_launcher_bg` runs `bash "$LAUNCHER" "$@" &`, writes `$!` to `$LAUNCHER_PID_FILE`, and `wait`s it into `$status` (not `$PPID`: a fake invoked from a `$(…)` substitution has a subshell as its parent). Extend the fake `tmux` with `FAKE_TERM_ON=<prefix>`: when `$*` starts with it, `kill -TERM "$(cat "$LAUNCHER_PID_FILE")"` before exiting 0; the fake `ssh` with `FAKE_SSH_TERM=1` does the same on the `: og-probe;` call. Cases:
  (a) `FAKE_TERM_ON='set-option -t tp-g6-workstation @bridge_session'`, server running → exit 143; `$TMUX_LOG` has `kill-session -t =tp-g6-workstation` **after** `new-session`; no `switch-client`; `$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock{,.pid,.phase}` absent;
  (b) `FAKE_TERM_ON='switch-client'` on the fresh path → exit 0; no `kill-session` line after `new-session`;
  (c) dedup path (copy the setup of "live compatible daemon is reused only after its ping succeeds") with `FAKE_TERM_ON='switch-client'` → exit 0, no `kill-session` of the mirror, the fake daemon pid still alive;
  (d) TERM before the mirror phase: `FAKE_SSH_TERM=1` → exit 143 and **no** `kill-session`/`new-session` in `$TMUX_LOG` (nothing local to undo).
  Command: `bats tests/remote-cold-start.bats` → new cases fail.
- [ ] **Step 4: implement the trap.** After `reap_daemon` is defined: `mirror_created="" attached="" daemon_started=""` and `on_signal()` — `[[ -n $attached ]] && exit 0`; if `mirror_created`: `if [[ -n $daemon_started && -n $! ]]; then reap_daemon "$!"; fi` (guard `$!` against being empty under `set -u`: use `${!:-}`), `tmux kill-session -t "=$local_sess" 2>/dev/null || true`, `rm -f "$sock" "${sock}.pid" "$phase_file"`; then `exit 143`. `trap on_signal TERM INT HUP`. Set `mirror_created=1` right after the dedup `if remote_daemon_alive …; fi` block (before the stale `rm -f`); `attached=1` immediately before the dedup `tmux switch-client` and before the final one; `daemon_started=1` immediately before the daemon launch `if`. Nohup fallback: `set -m` before and `set +m` after the `nohup … &` line (comment: own process group, so a group TERM aimed at the launcher never reaches the daemon). Command: `bats tests/remote-cold-start.bats tests/remote.bats` → green; `shellcheck scripts/og-remote-open.sh`; `shfmt -d scripts/og-remote-open.sh` → no diff.

### Component B — runner (`picker/attach*.go`)

- [ ] **Step 5: sanitizer, test first.** `picker/attach_test.go` `TestSanitizeStatusText` table: CSI (`"\x1b[31mred\x1b[0m"`→`red`), OSC-BEL (`"a\x1b]0;t\x07b"`→`ab`), OSC-ST (`"a\x1b]8;;u\x1b\\b"`→`ab`), 2-byte ESC (`"a\x1bcb"`→`ab`), C1 CSI (`"a\u009b31mb"`→`a31mb`), bidi (`"a‮b⁦c"`→`abc`), newline/tab collapse (`"a\n\t b"`→`a b`), trim, cap (300×`x` → 200 runes), plain unicode kept (`"héllo ✓"`). Run `cd picker && go test -run TestSanitizeStatusText ./` → fails (undefined). Implement in `attach.go`; rerun → pass.
- [ ] **Step 6: runner tests.** In `attach_test.go`, helper `fakeLauncher(t, body string) string` writes `#!/bin/sh\n`+body to a temp file (0755); helper `drain(r) ([]attachPhase, attachResult)` runs `go r.run()`, collects progress until `done` (fail after 10s), then empties whatever is still buffered in `progress`. Shrink `attachKillGrace` (500ms) and budgets via `t.Cleanup` restore. Cases:
  (a) phases in order: `printf 'connect\n' >&3; printf 'mirror\n' >&3` → `[connect mirror]`, `attachOK`;
  (b) unknown line ignored: `printf 'bogus\nconnect\n' >&3` → `[connect]`;
  (c) cancel kills the whole group: body `sh -c 'sleep 1; echo x > MARK' & printf 'connect\n' >&3; wait` (MARK a temp path); after the first progress event call `r.cancel()`; result `attachCancelled`, `phase connect`; sleep 1.5s; MARK absent;
  (d) TERM ignored escalates to KILL: `trap '' TERM; printf 'connect\n' >&3; while :; do sleep 0.1; done` → cancel → done within grace+2s, `attachCancelled`;
  (e) timeout names phase: budgets `connect=200ms`; `printf 'connect\n' >&3; sleep 30` → `attachTimedOut`, `phase connect`, `budget 200ms`, done within 3s;
  (f) launch budget: budget `launch=200ms`, `sleep 30` with no line → `attachTimedOut`, `phase launch`;
  (g) failure sanitized: `printf '\033[2Jout\n'; printf 'noise\nog-remote-open: \033]0;x\007bad \033[31mthing\n' >&2; exit 1` → `attachFailed`, `msg == "og-remote-open: bad thing"`;
  (h) failure with empty stderr → `msg` is the exit error text (`exit status 3` for `exit 3`);
  (i) exit 0 after cancel is success: `trap 'exit 0' TERM; printf 'mirror\n' >&3; sleep 30 & wait` → cancel → `attachOK`;
  (j) cancel before run: `r.cancel()` then `r.run()` with a body that touches MARK → `attachCancelled`, MARK absent;
  (k) start failure: `bin` = nonexistent path → `attachFailed`, msg non-empty;
  (l) `buildAttachCmd`: argv `[bin host sess]` (and `[bin host]` for empty sess), env contains `OG_REMOTE_OPEN_PROGRESS_FD=3` and `SSH_ASKPASS_REQUIRE=never`, `OG_REMOTE_RESTORE=1` only when restore, `cmd.Stdout == nil`, `cmd.SysProcAttr.Setsid`.
  Run `cd picker && go test -run 'TestAttach|TestBuildAttachCmd' ./` → fails.
- [ ] **Step 7: implement the runner** (implement: escalated — process-group signalling, timer/cancel select loop, Pdeathsig thread pinning). `attach.go`: `run()` checks `ctx.Err()` first (cancelled before start → result without forking); `os.Pipe()`; `buildAttachCmd` + `ExtraFiles=[pw]`, `Stderr=&buf`; `runtime.LockOSThread()` / deferred unlock around start→Wait; close `pw` after Start; reader goroutine (`bufio.Scanner`, known names only) → internal chan; `Wait` goroutine → chan; select loop over lines (reset the per-phase timer, lossy send to `progress`), timer (→ timedOut, `kill(-pid, SIGTERM)`, arm grace), `ctx.Done()` (→ cancelled, same), grace (→ `kill(-pid, SIGKILL)`), wait result → first drain the reader to EOF (bounded by `grace`), forwarding each remaining line to `progress` in order (a fast launcher's last lines are otherwise lost to the select race), then close `pr`, then classify: `err == nil`, or `errors.Is(err, exec.ErrWaitDelay)` with `cmd.ProcessState.Success()` (a grandchild held stderr past a clean exit), ⇒ OK regardless of flags; timedOut ⇒ TimedOut; cancelled ⇒ Cancelled; else Failed with `sanitizeStatusText(lastNonEmptyLine(buf))`, falling back to `sanitizeStatusText(err.Error())`. Signal at most once per kind; never after Wait returned. `attach_linux.go` / `attach_other.go` per the file list. `remoteOpenBin(opts)` in `remote.go` (`envOrMap("REMOTE_OPEN_BIN", opts, "@remote_open_bin", "og-remote-open")`), used by `launchRemoteBridgeDetached` too. Command: `cd picker && go test -race -run 'TestAttach|TestBuildAttachCmd|TestSanitize' ./` → pass; `go vet ./`.
- [ ] **Step 8: supervisor.** Test first `TestAttachSupervisorStopCancelsAndWaits`: track a run on `sh -c 'sleep 1; echo x > MARK' & printf 'connect\n' >&3; wait`, start it, wait for first progress, `stop(3s)` returns within 3s with the run's `done` closed and result `attachCancelled`; MARK absent after 1.5s. `TestAttachSupervisorNil`: nil receiver `track`/`stop` don't panic; `stop` with no tracked run returns immediately. Implement; `go test -race -run TestAttachSupervisor ./` → pass.

### Component C — model wiring (`picker/tui.go`, `picker/render_list.go`)

- [ ] **Step 9: model tests, test first** in new `picker/tui_attach_test.go` (use `remoteFixture`, `findVisible`, `wallKey`):
  (a) **Update never launches synchronously**: fake launcher `touch STARTED; printf 'connect\n' >&3; sleep 30` via `@remote_open_bin`; cursor on `remote:lab:mono`; `t0 := time.Now(); next, cmd := m.Update(wallKey("enter"))`; assert elapsed < 250ms, `nm.attach != nil`, `cmd != nil`, and after `time.Sleep(200ms)` STARTED does **not** exist (no Cmd was run); then run the batch's Cmds in goroutines (helper `runBatchAsync`) → STARTED appears, an `attachPhaseMsg{phase: connect}` arrives; `Update(esc)` → `cancelling`; the `attachDoneMsg` arrives with `attachCancelled`; after `Update(doneMsg)`: `attach == nil`, `statusMsg` contains `cancelled`.
  (b) keys ignored during attach: with `attach` set (run from `newAttachRun`, never started), each of `enter`, `j`, `ctrl+x`, `tab`, `ctrl+t`, `down` returns a nil Cmd and leaves `attach.id`, `query`, `cursor`, `marked` unchanged;
  (c) `esc` → `attach.cancelling == true` and the run's ctx is cancelled; `ctrl+c` while cancelling → `tea.Quit`; `ctrl+c` while not cancelling → cancelling (not quit);
  (d) mouse click / wheel ignored while `attach != nil`;
  (e) outcomes: `attachDoneMsg` OK → `tea.Quit`, marks cleared, `launchDetached` seam called once per `rest` item in order; Failed → `statusMsg == "lab/mono: <msg> — enter to retry"`, marks kept, `launchDetached` not called, `attach == nil`; TimedOut → statusMsg contains `timed out connecting` and `20s` and `enter to retry`; Cancelled → `cancelled opening lab/mono`;
  (f) retry: after a Failed outcome, `Update(enter)` starts a new attach with `id` greater than the previous;
  (g) stale ids: `attachPhaseMsg`/`attachDoneMsg`/`attachTickMsg` with a non-current id → model unchanged and nil Cmd; `attachTickMsg` while idle → nil Cmd;
  (h) render: with `attach` built through `beginAttach` (never set by hand), `visibleWidth(m.renderHints()) == m.width` for widths 12, 40, 80, 160, and at 80 the line contains `connecting`, `lab/mono`, `esc:cancel`; a remote row whose `remoteSess` carries `\x1b[2J` renders without ESC;
  (i) multi-open: `m.openMarkedRemote(marked)` → `attach` for the first (host/sess), `rest` = the others, `launchDetached` not called yet.
  Adapt: `TestOpenMarkedRemoteWithLaunchesAllButFirst` → drive `openMarkedRemote` + OK `attachDoneMsg` (assert launch order and quit); `TestActivateCurrentOpensAllMarkedIgnoringCursorRow` → assert `attach` targets `lab/mono` with `rest` = `[lab/other]` (no process); `scope_test.go` `TestMarkedRemoteItemsResolvesAcrossScopeSwitch` → assert `attach` targets `alpha/live`; `remote_test.go` `TestOpenRemoteBridgeUsesConfiguredBin` → deleted (covered by 6(l) plus a `remoteOpenBin` assertion there). Command: `cd picker && go test ./` → new/adapted tests fail to compile or fail.
- [ ] **Step 10: implement the wiring.** `tui.go`: messages and Cmds from Interfaces; `attachState{id int; run *attachRun; host, sess, label string; phase attachPhase; started, phaseAt time.Time; frame int; cancelling bool; rest []listItem}`; `beginAttach(first listItem, rest []listItem)` (spec `attachSpec{bin: remoteOpenBin(m.tmuxOpts), host, sess, restore: first.remoteRestore}`; label = `sanitizeStatusText(host + "/" + sess)`, host alone when sess empty; `m.attachSup.track(run)`; returns `tea.Batch(func() tea.Msg { run.run(); return nil }, waitAttachCmd, attachTickCmd)`); `activateCurrent` remote branch calls it instead of `openRemoteBridge`; `openMarkedRemote(marked)` = `beginAttach(marked[0], marked[1:])`; delete `openMarkedRemoteWith` and `openRemoteBridge`. `Update`: the three attach messages (copy-on-write the state), `MouseWheelMsg`/`MouseClickMsg` return early while `attach != nil`; `handleKey` first line after reading `key`: `if m.attach != nil { return m.handleAttachKey(key) }` (before the wall/kill branches). Status texts exactly as in Interfaces. `runTUI`: `sup := &attachSupervisor{}`, `m.attachSup = sup`; SIGHUP → `p.Kill()` goroutine (`signal.Notify`/`signal.Stop`); after `p.Run`, `sup.stop(attachKillGrace + time.Second)` **before** the `err != nil` return. `render_list.go`: `renderHints` returns `m.renderAttachStatus()` when `m.attach != nil` — spinner `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, `opening <label> · <phase label> · <n>s` (or `cancelling <label>…`), suffix `esc:cancel` (or `^c:quit` while cancelling) styled like other hints with its cells reserved; head clipped with `fitVisibleWidth`; when the width cannot hold the suffix, clip the whole line to `m.width`. Command: `cd picker && go test -race ./` → all pass; `go vet ./`.

### Component D — docs and gates

- [ ] **Step 11: docs.** `docs/agents/picker.md`: new `## Attach (remote session open)` section — async runner (never on Update), the fd protocol and why not stdout, phase table + budgets, cancel semantics (process group, script trap, commit point incl. the racing-switch edge case), exit-path coverage (post-Run supervisor, SIGHUP, Linux Pdeathsig, darwin SIGKILL gap), multi-open ordering change (#730 rest launched only on success), sanitization, retry. Update the `^t` bullet's "the first via the usual foreground og-remote-open, the rest launched detached" sentence to the new order. `docs/agents/scripts.md`: `og-remote-open` row — `OG_REMOTE_OPEN_PROGRESS_FD` and the rollback trap; `tmux-session-picker` row — the "Enter with any marked opens all of them — the first via the usual foreground og-remote-open…" clause updated likewise. Command: `rg -n "foreground .og-remote-open" docs/agents` → no stale wording left.
- [ ] **Step 12: full gate.** `nix build .#default`, `nix flake check`, `nix build .#lint` → all succeed.
- [ ] **Step 13: follow-up issue.** At PR time (Deferred findings order): `gh issue list --search "killRemoteSessions synchronous" --state open`; if none, `gh issue create --assignee @me` for "picker: ^x remote kill runs ssh synchronously on the Update path" (evidence `picker/tui.go` `killRemoteSessions`, up to 3s per host; deferred as out of #770's attach scope; link the PR and #770).
- [ ] **Step 14: manual repro (for the PR body).** A wrapper `slow-open` that emits phases with sleeps (`printf connect >&$OG_REMOTE_OPEN_PROGRESS_FD; sleep 4; …`) and a hanging variant, set via `tmux set -g @remote_open_bin …` on a scratch server (`TMUX_TMPDIR=/tmp/og-$$ … tmux -L probe`), run the built picker in that server, capture-pane snapshots showing: the progress line advancing, Esc → `cancelled opening …`, hang → `timed out connecting … — enter to retry`. Paste the trimmed capture lines into the PR's `## Testing`.

## Acceptance

- [ ] Go tests for the attach state machine: phases (6a/6b), cancel mid-phase leaves no residue (6c/6d, 8; bats 3a), per-phase timeout surfaces an error and returns control (6e/6f, 9e) — `cd picker && go test -race ./` green.
- [ ] Update never performs the blocking attach synchronously — 9a green.
- [ ] Manual repro note in the PR body — Step 14 capture.
- [ ] `docs/agents/picker.md` updated — Step 11.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` pass — Step 12.
