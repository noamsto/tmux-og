# prdash hand-off: land the client on the worktree's window (#1261)

## Diagnosis (measured, bridge integration harness)

Enter on a PR row in a mirror runs `tmux-prdash-mirror.sh` → `ctl handoff` → a
remote float runs `wt switch`, whose post-switch hook does one of:

| hook branch | remote effect | daemon sees | client lands on the worktree window? |
|---|---|---|---|
| matched window, same session | `select-window` | `%session-window-changed` (window already mirrored) | yes |
| no match | `new-window -a` (selects it) | `%session-window-changed @N` **then** `%window-add @N` | **no** |
| matched window, other session | `switch-client` | `%session-changed` → pin-back + `HandOff` | session handed to `og-remote-open` (launcher owns the switch) |

Root cause: tmux emits the select before the link notification for a
`new-window` without `-d`. `translateWindowNotification` finds no registry
entry for `@N` yet and drops the follow; `addWindow` mirrors the window but never
selects it, and `reconcileWindows`' follow-on-add is not on this path. prdash
and the script are not at fault: with a hand-off line present the float runs.

## Steps

1. **Daemon follow** (`picker/remotebridge/daemon/daemon.go`, dispatch loop):
   remember the window a `%session-window-changed` named while unmirrored
   (`selectedBeforeAdd`), only when its session arg is the pinned `pin.id`
   (the event is broadcast for every remote session); after the `%window-add`
   for that id mirrors it, select it locally under the same `hostCurrentIsOwn`
   gate.
2. **Integration tests** (`tests/remote-m2-integration.bats`) and **flake**
   (`flake.nix`): a rig driving the real launcher script, real `ctl` and real
   daemon against two real tmux servers with a local client attached, and a
   remote `wt` stub doing each hook branch. The remote-m2 shards copy only
   `tests/`, so `remoteM2Env` gains `pkgs.git` in `nativeBuildInputs` (the rig builds a checkout and the remote float probes it with `git`) and `PRDASH_MIRROR = ./scripts/tmux-prdash-mirror.sh;`
   (raw source ships, no placeholders) and the rig runs `bash "$PRDASH_MIRROR"`
   (falling back to `$BATS_TEST_DIRNAME/../scripts/...` for a local run).
   - new-window (red on main): client lands on the new window's mirror.
   - select-window: client lands on the selected window's mirror (guards the
     path that already worked).
   - switch-client: asserts the session reaches the launcher (`og-remote-open
     <host> other`) and the mirror pins back. The client's final window there
     is the launcher's and the remote `switch-client -t SESS:WIN`'s job
     (WIN is the target session's current window when the second mirror
     starts); not assertable without a real launcher/remote.
   - launcher failure: the `og-remote-open` stub exits 1 -> the local client's
     message log (`show-messages -t <client>`) holds the notice.
3. **No silent no-op.** Notices are `display-message -d 5000 -c <client>`.
   - `picker/remotebridge/daemon/handoffnotice.go`:
     `notifyClients(out, run, target, msg, own)` lists `list-clients -t target
     -F '#{client_control_mode}|#{client_name}|#{window_id}'`, skips control
     clients, and for a placed mirror (`own != nil`) only those whose window
     passes `own`. The text carries no remote-derived data.
   - `handoffWatch` (own mutex, held briefly, never across a send; the ctl
     goroutine arms, the main loop takes; injected clock `now`): armed by
     `handleCtl` after `cst.submit` succeeds, via a verb-table field
     `armsHandoff` (set on `handoff`) copied into `ctlRequest` like
     `needsView`/`probe`. `take()` is true once within 60s and clears;
     `disarm()` clears. Ordinary remote navigation stays quiet outside it.
   - While armed, a notice is raised when: the `%window-add` for
     `selectedBeforeAdd` leaves the window unmirrored (excluded by a `--select`
     filter, or `addWindow` failed); `sessionPin.apply` pins back with
     `handOff == nil` (placed mirror: the session switch opens nothing). A
     successful follow (select-window applied, or the pending follow applied)
     disarms. The "follow skipped, `hostCurrentIsOwn` false" case stays
     intentionally silent: the user has moved off the mirror, and every client
     of a placed session sees that same window.
   - `Config.HandOff` / `sessionPin.handOff` become `func(string) error`;
     `cmd/daemon/main.go` returns `cmd.Run()`'s error (still logging it) and
     `sessionPin.apply` raises the notice (not armed-gated) when the hand-off
     fails. Existing `handOff` literals in `sessionpin_test.go` updated.
   - `selectedBeforeAdd` is only recorded when `pin.id == "" || l.Args[0] ==
     pin.id` (empty id = pinning off, as `apply` treats it).
   - `scripts/tmux-prdash-mirror.sh`: a `display-message` when prdash exits
     non-zero (a plain quit with `q` exits 0 and stays quiet; lines written
     before a non-zero exit are NOT forwarded) or when the `ctl handoff` call
     exits non-zero (ctl exits 0 after its own `--display-error` banner, so
     this fires only when that banner could not be shown).
   - Tests: Go tests for `notifyClients` (dedicated vs placed filtering, control
     clients skipped), `handoffWatch` (arm/take/disarm/expiry with the injected
     clock, plus a concurrent arm/take test under `-race`), the
     `sessionPin.apply` failing-`handOff` and nil-`handOff`-while-armed
     notices, and the `armsHandoff` table field; `prdash-mirror.bats` cases for
     the prdash and ctl failures; the bats launcher-failure case (lands with
     the Go notice code, red until then).
   - Accepted limits: a hand-off whose `wt` selects an *existing* window the
     filter excludes raises no `%window-add`; a remote without the worktrunk
     post-switch hook makes `wt` succeed with no window or session event.
     Both stay silent.
4. **Docs**: `docs/agents/bridge-daemon.md` (follow ordering; a dedicated
   mirror now follows any remote `new-window` without `-d`, not only a
   hand-off; the notices), `docs/agents/scripts.md` (script notices).

## Verification

`nix build .#default`, full `nix flake check` plus the affected checks
(`remote-m2-shard-coverage`, `remote-m2-integration-tests-{1..4}`, `prdash-mirror-tests`, `picker-go-tests`,
`picker-go-lint`), `nix build .#lint`. Red evidence: the new-window bats case
against main's `daemon.go` (hunk reverted in a scratch copy, daemon rebuilt):
client stays on `origin`, `wait_client_view` fails; green with the fix.

## Out of scope / unverifiable here

The `switch-client` branch's last hop (`og-remote-open` switching the local
client) is stubbed in the harness; confirming it needs a real remote host. The
harness runs a `wt` stub, not the post-switch hook, so hook-only paths are not
exercised: the DUP take-over branch (`send-keys cd` into the window holding the
float) and a `CLAUDECODE` in the remote server's global env making the hook
exit early. prdash is untouched.
