# Bridge daemon: end the run when the local mirror session is gone (#680)

## Problem

`Run`'s terminal endings were `%exit`, an emptied registry, a raised stop, and
an exhausted retry budget (`picker/remotebridge/daemon/daemon.go`). None of them
fires when the **local** mirror session disappears: the registry holds *remote*
window ids and they are all still there, and the control connection stays
healthy. Every local command fails, and each failure is individually designed to
be survivable — `localWindowSet` is positive-evidence-only precisely so a
transient read cannot tear down a healthy mirror — so a session that is
permanently gone reads like a blip forever.

Measured in the issue: 7 daemons for 5 live mirror sessions; the two orphans had
run ~4d 17h and 15h 37m, logging `can't find session` /
`layout-change resize-window: exit status 1` at ~1 MB/day between them. Worse
than noise: the orphan keeps its ssh transport and control-mode client attached
to the remote session, so the per-window size cap it asserted
(`refresh-client -C @N:WxH`) stays in force and tmux applies it as a hard clamp,
released only when the client goes (#201) — on `halo`, the role-grid window was
held at the dead mirror's 173x46 for 15 hours while the rest of the session sat
at the remote's natural 250x64.

## Invariant

A bridge daemon lives exactly as long as the local mirror session it serves. Its
terminal endings are the remote ending the client, the mirror being left with no
windows, a raised stop, an exhausted retry budget — and now the local
`cfg.LocalSess` being affirmatively gone, on which it drops the control client
so the remote clamp is released.

## Design

### The liveness question is `has-session`

`list-windows` (what `localWindowSet` uses) answers with a whole-session
listing; when the session is gone it fails, but so does a transient read, and
the positive-evidence rule deliberately treats the two as one. `has-session`
answers with tmux's own exit status, and that is the distinction:

| answer | meaning |
| --- | --- |
| exit 0 | the session exists → alive |
| exit status **1** (`*exec.ExitError`) | tmux answered: `can't find session` (or `no server running`, which means the same for a session that lived on that server) → **definitively gone** |
| anything else (tmux could not be exec'd, another status, killed by signal) | **could not ask** → not evidence |

Measured on the pinned tmux: `has-session -t gone` → `can't find session: gone`,
exit 1; with the server dead → `no server running on …`, exit 1.

`-t "=<name>"` is the exact match `og-remote-open.sh`'s own liveness check uses,
so a sibling session whose name has ours as a prefix cannot read as alive.

### Two consecutive definite negatives

`localSessionGone` filters a question that could not be asked; the double strike
is the second line of defence, so a single spurious negative cannot take a
healthy mirror down. One affirmative answer clears the count. The count is
session-lifetime state: a reconnect does not bring a gone session back.

### Where and what it ends

The coarse `loopTick` (5s) case in `runConn` — the loop's only idle wake-up, so
the check fires on a mirror with no stream traffic too. One extra local tmux
fork per 5s is negligible against the existing ~1/s maintenance sweep, and the
`mirrorPaneRows` listing on that same sweep already forks tmux, so a wedged
local tmux stalling the loop is a pre-existing exposure, not a new one (no
`LocalTmuxOut` deadline is added here).

The verdict is the existing `connEnd`: the attach loop breaks and `teardown()`
runs, whose `hold.close()` drops the control client and therefore releases the
remote clamp.

### Decisions taken

- **Renaming the mirror session now ends the bridge** (~10s). Ownership is keyed
  on the name the daemon created; after a `rename-session` every session-scoped
  listing already fails, so the mirror was already degraded (no heals, no resize
  convergence) — ending it is the honest outcome. `og-remote-detach` still
  reaches the daemon before it exits, since `@bridge_sock` is a session option
  and survives a rename.
- **Teardown leaves the session name alone on this path.** `kill-session -t
  cfg.LocalSess` is skipped when the run is ending because that session is gone:
  there is nothing to kill, and a reopen that recreated the name in the gap must
  not have the fresh session killed out from under it.
- **`og-remote-open` needs no change.** `scripts/og-remote-open.sh` already
  reaps a same-sock daemon whose session is gone when the host is reopened
  (`ping` succeeds → protocol-compatible; `has-session` fails → `reap_daemon`),
  so it covers the user-reopens-the-host case. This change covers the case it
  cannot: the host is never reopened. No new startup reap is added — it would
  widen the launcher's blast radius for a case already handled.

## Files

| file | change |
| --- | --- |
| `picker/remotebridge/daemon/localpanes.go` | `localSessionGone`, `sessionGoneStrikes`, `sessionGoneTracker.observe` |
| `picker/remotebridge/daemon/daemon.go` | tracker + vanished flag, the `loopTick` probe, the teardown guard, doc comments |
| `picker/remotebridge/daemon/localsession_test.go` | gone-vs-could-not-ask for the probe, and the two-strike rule |
| `tests/remote-m2-integration.bats` | kill the local mirror session, assert the real daemon exits and the control client goes |
| `CLAUDE.md` | the endings list in "Bridge Reconnect" |

## Steps

1. **Regression first, through the production entry point.** New bats test over
   the daemon's `--test-local` seam: mirror `m2src:rem` into `m2dst:host-sess`,
   gate on the renderer, assert the control client is attached (that is the
   clamp), `kill-session -t host-sess` on the local server only, then poll for
   up to 40s for the daemon to exit on its own, assert the exit status is 0, and
   assert no control-mode client remains on the remote. Run it against the
   unmodified daemon: **red** (alive after 43s, log repeating the issue's
   symptom).
2. **The probe and the strike rule.** `localSessionGone` (exit status 1 only)
   and `sessionGoneTracker`. Unit test in the same package; the
   `*exec.ExitError` is produced by re-executing the test binary (the
   `cmd/daemon` wedged-child pattern), so the test needs no tmux and nothing on
   PATH — `picker-go-tests` runs it in the sandbox.
3. **Wire it in.** Probe on the `loopTick` case, `connEnd` on the second
   consecutive definite negative, teardown skips `kill-session` on this path,
   comments updated.
4. **Re-run the bats test: green.** Then the scoped Go tests.
5. **Docs.** This plan, and CLAUDE.md's endings list.
6. **Gate.** `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Evidence

- `bats -f 'daemon exits when its local mirror session is gone'
  tests/remote-m2-integration.bats` with the pre-fix daemon: **fails**,
  `daemon still alive 43s after its local session died`, log repeating `no
  server running on …/m2dst` and `layout-change resize-window: exit status 1`.
- Same test with the fixed daemon: **ok**.
- `go test ./remotebridge/daemon/ -run 'LocalSessionGone|SessionGoneTracker'`:
  pass (exit 0 → alive; exit 1 → gone; exit 2 and an exec failure → alive;
  two strikes → gone, an affirmative answer clears the count).
- Consumer map: `og-remote-detach.sh` and the picker's `stopBridgeDaemon` both
  SIGTERM the daemon by `@bridge_sock`; neither depends on the daemon's exit
  reason, so both stay compatible (a SIGTERM run keeps its own teardown). The
  local mirror session's own consumers (`tmux-statusline`'s `@bridge_host`,
  `@bridge_state`) are gone with the session.

## Out of scope

- Any `og-remote-open` change (it already handles the reopen case).
- Reaping on a transient unreadable local answer — the could-not-ask rule
  forbids it.
- A deadline on `LocalTmuxOut`: a wedged local tmux already blocks `repair`
  and the resize watcher, so adding one to the daemon's tmux seam is a wider
  change than this issue.
