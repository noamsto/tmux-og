# Picker: progress, cancel and fault handling for a slow remote attach (#770)

## Problem

Enter on a Remote-section session row (`prefix + s`) calls `openRemoteBridge`
(`picker/remote.go`) **synchronously from `activateCurrent`, i.e. inside
bubbletea's `Update`**. It `exec`s `og-remote-open` and `cmd.Run()`s it to
completion. For the whole run:

- no frame is painted (Update has not returned) — the popup looks frozen;
- no key is read — Esc/C-c do nothing, the only exit is killing the popup;
- there is no bound on the run: `og-remote-open`'s ssh calls carry no
  `ConnectTimeout`, so a host that blackholes packets holds the picker for the
  kernel's TCP connect timeout (~2 min), and a remote `tmux-remux restore` can
  take as long as it likes;
- a failure's text is `lastNonEmptyLine(stderr)` rendered raw into the hint
  line — that line is often remote-derived (ssh banner/errors, remote stderr),
  and ESC/C0/C1 bytes in it reach the popup's terminal unfiltered.

The multi-open path (`openMarkedRemoteWith`) has the same synchronous call for
the first marked session.

### Where the time goes (og-remote-open, in order)

| phase | what runs | can be slow / hang because |
|---|---|---|
| connect | `ssh -T host bash -s` combined probe (os, tmux path, session, active window) | ssh connect (DNS, TCP, Tailscale, key exchange); no ConnectTimeout |
| start-server | `ssh host systemctl --user restart tmux-startup.service` (or launchctl) + `ssh … list-sessions` re-probe | cold start of the remote server; only when the host has no session |
| restore | `ssh … has-session`, possibly cold start, `tmux-remux restore`, `has-session` | only for a snapshot (`remoteRestore`) row; restore replays a whole session |
| create | `ssh … has-session`, possibly cold start, `new-session`, `has-session` | only for `OG_REMOTE_NEW_DIR` (not reachable from `prefix + s` today, but the script is shared) |
| connect (window) | `ssh … list-windows` when the probe did not resolve a window | one more ssh round trip |
| mirror | local only: ctl ping of an existing daemon (≤2s), `reap_daemon` (≤2s), `kill-session`, `new-session` (loading pane), `set-option`s, detached daemon launch, `switch-client` | bounded by construction |

After `switch-client` the picker's job is over: the client lands on the mirror
session whose first window runs `og-remote-loading`, which already shows the
daemon's own phases (dial, first window) from `${sock}.phase`. This spec
covers the stretch *before* that hand-off — the one that currently freezes the
picker.

## Goals

1. **Never block Update on the attach.** The launcher runs off the Update
   goroutine; Update only starts/cancels it and consumes messages.
2. **Progress.** While an attach is in flight the hint line becomes an attach
   status line: spinner, `host/sess`, current phase label, elapsed seconds, and
   the cancel hint — e.g. `⠹ opening lab/main · connecting · 3s   esc:cancel`.
3. **Cancel.** Esc or C-c during an in-flight attach cancels it and returns to
   the list with no residue: no surviving launcher/ssh processes, and no
   half-built local mirror (session, daemon, socket/pid/phase files).
4. **Faults.** Each phase has a timeout. A timeout or a launcher failure
   returns control to the list with a clear, **sanitized** one-line error and a
   retry path (Enter on the still-selected row — or the still-set marks —
   retries).

## Non-goals

- The daemon's post-switch phases (dial, window mirroring) — already covered
  by `og-remote-loading` in the mirror session.
- `og-remote-auth` (interactive, `tea.ExecProcess` already hands it the pty)
  and `^o`/`og-remote-picker` (runs in its own float).
- Other synchronous calls on Update that are not the attach — notably
  `killRemoteSessions` (ssh, bounded 3s per host). Filed as a follow-up issue,
  not fixed here.
- Rolling back remote-side effects of a cancelled phase (a remote server
  cold-started, a session `tmux-remux` began restoring). Those are the remote
  host's own state, created by its own tools; the invariant is "nothing half
  built **locally**".
- #754 (refreshMsg vs forgotten mirrors) — queued separately.

## Design

### D1. Phase reporting: a dedicated fd, gated by env

`og-remote-open` learns an opt-in progress channel:
`OG_REMOTE_OPEN_PROGRESS_FD=<n>` (digits only; anything else is ignored). When
set, the script writes one line per phase transition to that fd:
`connect`, `start-server`, `restore`, `create`, `mirror`. Absent the variable,
behaviour and output are byte-identical to today, so the CLI, `og-remote-picker`
and the daemon hand-off (`OG_DAEMON_REMOTE_OPEN`) are unaffected.

Why an fd and not stdout: stdout already carries remote output (`systemctl`,
`tmux-remux restore` inherit it), so a remote could forge a phase line. The
fd is written only by the script itself; ssh closes inherited fds above 2 at
startup. The picker passes a pipe as `ExtraFiles[0]` (fd 3).

The fd must not outlive the launcher: the detached daemon would otherwise hold
the pipe's write end open for its whole life. The script closes it
(`exec {fd}>&-`) right before launching the daemon; the picker never relies on
EOF for completion anyway (process exit is the completion signal), so a leaked
fd would cost only a goroutine, not correctness.

The picker ignores a line that is not one of the five names (forward
compatibility; no free text crosses this channel).

`connect` is emitted again before the late `list-windows` round trip
(`og-remote-open.sh`, the `[[ -z $win ]]` block after restore/create), so that
ssh call carries the connect label and budget rather than the previous phase's.

**Launcher stdout never reaches the pty.** Today `openRemoteBridge` sets
`cmd.Stdout = os.Stderr`, i.e. the popup's pty — harmless only because nothing
repaints while Update is blocked. With live frames during the attach, remote
output (`systemctl`, `tmux-remux restore`) would paint raw bytes over the
frame. The runner discards stdout (`/dev/null`); stderr is captured into a
buffer as today, and nothing of the launcher's is ever written to the
picker's own stdout/stderr.

### D2. Cancellation: own process group, TERM then KILL, script-side rollback

The picker starts the launcher with `Setsid: true` (new session ⇒ own process
group, and **no controlling terminal**: an ssh that would otherwise open
`/dev/tty` to prompt — painting over the popup and waiting on keys bubbletea
is consuming — fails fast instead, and the error surfaces as a fault). The
runner also sets `SSH_ASKPASS_REQUIRE=never` in the launcher's environment
only, so with no tty ssh does not fall back to a GUI askpass when `DISPLAY` /
`WAYLAND_DISPLAY` is set. (Hosts that need interactive auth are already routed
to `og-remote-auth` by the probe's needs-auth classification; this only turns
a surprise prompt into a fast failure.)

Cancel = `SIGTERM` to the whole group (`kill(-pgid)`), then `SIGKILL` to the
group after a grace period (5s) if the launcher has not exited. Every ssh the
script has in flight is in that group and dies with it.

The daemon is launched `setsid` and so is **outside** the group. The script
therefore owns rolling back its own `mirror` phase: a `trap … TERM INT HUP`
installed at the top of the script which, **only if the local mirror session
was created and `switch-client` has not yet run**, terminates the daemon it
launched (`reap_daemon "$!"`-style TERM→wait→KILL, bounded 2s), kills the local
mirror session (`kill-session -t "=$local_sess"`), and removes
`$sock $sock.pid $sock.phase`. Before the mirror phase there is no local
state to undo; the trap just exits 143. The 5s grace exceeds the trap's
2s worst case.

The commit point is the final `switch-client` — of either path: the
dedup path that reuses a live daemon, or the fresh mirror. The script sets
`attached=1` immediately **before** each of them; a trap that sees it exits 0
without rolling back. A cancel racing that last local command is therefore
reported as success: at worst the client was not switched but a complete,
healthy mirror exists — never a half-built one. The picker treats exit 0 as
success even if it had asked for a cancel, and the runner never signals the
group after `Wait` has returned. (A group TERM landing while that final
`tmux switch-client` runs can kill the tmux client before the switch lands:
the picker reports success and quits, the client stays put, a complete mirror
exists. `docs/agents/picker.md` records this so it is not mistaken for a bug.)

**The daemon must be outside the group on every platform.** Linux uses
`setsid`. The macOS fallback (`nohup … &`) would leave the daemon in the
launcher's group, where a group TERM racing the commit point kills a healthy
mirror; the fallback therefore starts it under `set -m` (bash job control puts
the background job in its own process group) and restores `set +m` after.

**Every picker exit cancels an in-flight attach.** Setsid removes the
incidental protection the old code had (the launcher sharing the popup pty's
session and dying with it), so the picker takes it on explicitly:

- `tea.Quit` from any key path, SIGTERM/SIGINT (bubbletea turns both into a
  quit), and a panic bubbletea recovers inside `Run` (which returns a nil
  model): `runTUI` owns an `attachSupervisor` the model points to and the
  model registers each run with, so the cleanup never depends on the final
  model. After `p.Run` returns — **before** any error return — it cancels the
  tracked run and waits (bounded by the grace +1s) for it to finish.
- SIGHUP (popup pane killed, pty gone): bubbletea does not handle it, so
  `runTUI` registers it and calls `p.Kill()`, which returns from `Run` into the
  same cleanup.
- An uncatchable death (SIGKILL): on Linux the launcher is started with
  `Pdeathsig: SIGTERM`, so its own trap rolls back — **eventually**: the
  signal reaches bash only (not its group), and bash defers a trap until the
  foreground ssh returns. The runner goroutine holds `runtime.LockOSThread`
  from fork until `Wait` returns, since Go ties Pdeathsig to the forking OS
  thread (golang/go#27505). Elsewhere (darwin) the launcher runs to completion
  unsupervised — no worse than a CLI `og-remote-open`. Both are documented as
  the uncovered exits.

### D3. Per-phase timeouts, enforced in Go

The runner (D4) restarts a timer on every phase line. Budgets:

| phase | budget |
|---|---|
| (launch — before the first line) | 10s |
| connect | 20s |
| start-server | 45s |
| restore | 90s |
| create | 30s |
| mirror | 15s |

Expiry cancels exactly like D2 and ends the attempt with a timeout error naming
the phase and the budget: `timed out connecting to lab after 20s`. The budgets
are table-driven so tests can shrink them. Enforcing in Go keeps the script
free of timeout plumbing and covers every caller-visible hang, including one
inside a single ssh call.

### D4. The attach runner (off the Update goroutine)

A small unit in a new `picker/attach.go`:

```go
type attachPhase string // "launch", "connect", "start-server", "restore", "create", "mirror"
type attachEvent struct { // one of: phase change, or done
	phase attachPhase
	done  bool
	err   error         // nil on success
	kind  attachOutcome // ok | failed | timedOut | cancelled
}
func runAttach(ctx context.Context, spec attachSpec, budgets map[attachPhase]time.Duration, events chan<- attachEvent)
```

`runAttach` builds the command (same bin resolution and args as
`openRemoteBridge`, plus `OG_REMOTE_OPEN_PROGRESS_FD=3`, `OG_REMOTE_RESTORE=1`
when restoring), starts it, reads phase lines, applies D3, honours `ctx`
cancellation per D2, waits for exit, and sends exactly one `done` event last.
Stderr is captured as today; the failure text is
`sanitizeStatusText(lastNonEmptyLine(stderr))`, falling back to the exit error.

Bubbletea wiring: Enter creates `ctx, cancel`, a buffered events channel and an
attempt id, stores an `attachState` on the model, and returns
`tea.Batch(startAttachCmd, waitAttachEventCmd, attachTickCmd)`. The start Cmd
runs `runAttach` in its goroutine (never in Update). `waitAttachEventCmd`
blocks on the channel in a Cmd goroutine and returns an `attachEventMsg{id,…}`;
Update re-issues it after each non-final event. Every message carries the
attempt id; a message from a superseded attempt is dropped.

Starting the launcher (fork/exec) also happens in the Cmd goroutine, so Update
performs no process or file I/O for an attach.

### D5. Model and key handling during an attach

`tuiModel.attach *attachState` (nil = idle) holds id, label (`host/sess`,
sanitized), phase, attempt start, phase start, spinner frame, `cancel`,
`cancelling`, the events channel, and the multi-open remainder (D6).

While `attach != nil`:
- `esc` / `ctrl+c`: call `cancel()`, set `cancelling`, status line reads
  `cancelling lab/main…`. A second `ctrl+c` while `cancelling` quits the popup
  (the launcher is in its own session and finishes its rollback on its own).
- Every other key, and mouse clicks, are ignored — in particular Enter can
  never start a second concurrent attach. Data refresh messages keep flowing
  so the list underneath stays live.
- `attachTickMsg` (every 100ms, only re-armed while an attach is in flight)
  advances the spinner; elapsed time is recomputed from the stored start in
  View, so the tick carries no state.

On the final event:
- **ok** → launch the multi-open remainder (D6), clear marks, `tea.Quit`
  (today's behaviour).
- **cancelled** → `attach = nil`, `statusMsg = "cancelled opening lab/main"`.
- **timedOut / failed** → `attach = nil`,
  `statusMsg = "<error> — enter to retry"`. Marks and cursor are untouched, so
  Enter re-runs the same attempt: that is the retry path.

`statusMsg` is cleared by the next keypress, as today.

### D6. Multi-open (`^t` marks)

Today the N−1 extra marked sessions are fired detached *before* the first,
foreground one. That ordering makes cancel and retry incoherent (Esc would
cancel one of N; a retry after a failure would double-launch the others). New
order: the first marked session is the in-flight attach; the rest are fired
with `launchRemoteBridgeDetached` **only when the first succeeds**, right
before the popup quits. They still run in parallel with each other (one ssh
dial each, concurrently), so the cost rule "N marks don't serialize N dials"
still holds; the only change is they start after the first instead of
alongside it. Cancel or failure launches none of them and keeps the marks, so
Enter retries all of them.

### D7. Rendering

The attach status line replaces the hint line (same slot, one row, so
`bodyHeight` is unchanged). Built from styled segments, clipped with
`fitVisibleWidth(…, m.width)` so it never wraps or bleeds; the `esc:cancel`
suffix gets its cells reserved first, the label is what truncates (same
approach as the kill prompt). Spinner frames are braille dots (one cell each).
Phase labels: launch→`starting`, connect→`connecting`,
start-server→`starting tmux server`, restore→`restoring session`,
create→`creating session`, mirror→`attaching`.

### D8. Sanitizing remote-derived text

`sanitizeStatusText(s)`: drop ESC-introduced sequences (CSI, OSC — through BEL
or ST — and two-byte ESC sequences), drop every remaining rune for which
`unicode.IsControl` holds or that is a bidi override/isolate (U+202A–U+202E,
U+2066–U+2069), collapse whitespace runs, trim, and cap at 200 runes.
Applied to: the launcher's error line, and the host/session label shown in the
attach status line (the session name is remote-derived via the probe).

It does not reuse the daemon's `stripWindowName` (`remotebridge/daemon`): that
one targets a *tmux format* sink (drops `|` and `#[…]`, keeps C1 and bidi
controls, which tmux renders inertly), lives in another package the picker's
main package does not import, and this one targets a *terminal* sink, where C1
(`0x9b` is a one-byte CSI), OSC and bidi controls are the threats.

## Acceptance mapping

- Go tests for the runner state machine with a fake launcher script (`#!/bin/sh`
  in a temp dir, pointed at by `@remote_open_bin`): phase events in order;
  cancel mid-phase kills the whole process group (a background child the fake
  started in its group is gone) and reports `cancelled`; a phase that outlives
  its (shrunk) budget reports `timedOut` naming the phase; a failing launcher's
  sanitized last stderr line comes back.
- Update never blocks: Enter on a remote row with a fake launcher that sleeps
  30s returns from `Update` within a short bound, with `attach != nil` and a
  non-nil Cmd; Esc then cancels, and the drained events end in `cancelled`.
- Model tests: keys ignored during attach, second attach impossible, retry via
  Enter after a failure, marks kept on failure/cancel and remainder launched
  only on success, stale-attempt messages dropped, status line width ==
  `m.width` at narrow and wide sizes.
- bats (og-remote-open): phase lines appear on the progress fd in order for a
  cold start and a restore; nothing is written without the env var; a
  TERM during the mirror phase (fake `switch-client` that blocks) kills the
  local mirror session, the launched daemon, and removes sock/pid/phase files;
  a TERM after `attached=1` (a fake `switch-client` that TERMs its parent)
  exits 0 with no rollback — on the fresh-mirror path and on the dedup
  (live-daemon reuse) path; the late `list-windows` round trip is preceded by
  a `connect` line.
- Runner: launcher stdout is discarded (a fake writing ESC bytes to stdout
  never reaches the returned error or any writer of the picker's), and the
  env carries `OG_REMOTE_OPEN_PROGRESS_FD=3` and `SSH_ASKPASS_REQUIRE=never`.
- `runTUI` exit cleanup: a helper that takes the final model is unit-tested —
  an in-flight attach is cancelled and awaited.
- `docs/agents/picker.md` gets an "Attach" section; `docs/agents/scripts.md`
  row for `og-remote-open` notes the progress fd and rollback trap.
- Manual repro in the PR body: `@remote_open_bin` pointed at a wrapper that
  sleeps between phases (and a variant that never exits) showing progress,
  Esc cancel, and a timeout.
