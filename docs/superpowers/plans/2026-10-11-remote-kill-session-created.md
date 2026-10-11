# Remote kill ids bound to `session_created` (#920)

## Problem

`^x` on a Remote-section row kills by the live probe's tmux id: `kill-session
-t '$N'` (`remoteKillSessionBody`) or `kill-window -t '$N:@M'`
(`remoteKillWindowBody`). Ids are unique only per server lifetime, and a remote
tmux server restart renumbers from `$0` — a tmux-remux relaunch while the popup
is open is the common case. If the restart lands between the probe and the `y`
confirm, the kill fires against whichever session or window now holds that id,
while the prompt named the old one. #919 (id-based session kill) and #912
(window kill) both deferred this: the probe line format is shared by two
producers and the parser, so the fix is wider than their scope.

Violated invariant: **a kill that the user confirmed against a named row must
not land on a different session, one that the probe did not identify.** The
residual was documented in `docs/agents/picker.md`'s session- and window-`^x`
bullets.

## Approach

Carry each session's `#{session_created}` (digits, seconds) from the probe to
the row, and have the remote kill script compare it against the live value
before it `exec`s.

- The `S|` line gains a fourth field: `S|#{session_id}|#{session_created}|#{session_name}`.
  The name stays last, so it may still hold `|`; the W line is unchanged and
  still joins its session by `session_id`. Both producers change together
  (`remoteListSessionsBody`, `remoteListWindowsBody`); they share one parser
  (`parseRemoteProbeOutput` → `parseRemoteWindowsOutput`).
- `remoteProbeResult.SessionCreated` maps session name → created (beside
  `SessionIDs`); the parser also keys it by session id while reading, so each
  `remoteWindow` gets its owning session's `SessionCreated`.
- Live session rows get `listItem.remoteSessionCreated`; window rows get it from
  `remoteWindow.SessionCreated`; a mirror row takes it from its host row's new
  `remoteHostSessionCreated` — the same probe as its `remoteHostSessionIDs`, so
  the pair cannot disagree. Neither the id nor the created is written to the
  session cache (a restart renumbers them both), and cached window rows stay
  kill-refused.
- `remoteKillScript` takes the expected created as `$3` and, on the server the
  probe would list, runs `display -p -t "$2" "#{session_created}"` and exits 1
  on a mismatch before `exec`ing. Exit 1 is the existing gone class
  (`errRemoteSessionGone`): the row is forgotten and the request was never
  acted on. A `display` failure (session gone) also exits 1. The target is `$N`
  for a session kill and `$N:@M` for a window kill; `display -t '$N:@M'`
  resolves the owning session's created. No name ever enters the command.
- `sshKillRemoteSessionCtx` / `sshKillRemoteWindowCtx` refuse a non-digits
  `created` with `errRemoteKillNoID` before spawning ssh, and `^x` refuses such
  a row at stage time.
- Tolerant parse: an `S|` line whose created field is not digits (an old remote
  tmux) still lists its session; the kill is refused rather than the row
  dropped.

Residual, documented: a restart that recreates the session within the same
second leaves `#{session_created}` unchanged (second resolution).

### Why the window guard is session-scoped

`@N` is not reused within one server lifetime, so the only way a window kill can
retarget is a renumbered `$N`; the session check covers it.

### Consumer map (probe/result/cache contract)

| edge | symbol / file | disposition |
| --- | --- | --- |
| producer (session probe) | `remoteListSessionsBody` (`picker/remote.go`) | changed: 4-field `S|` |
| producer (window probe) | `remoteListWindowsBody` (`picker/remote_wins.go`) | changed: 4-field `S|` |
| parser | `parseRemoteWindowsOutput` (via `parseRemoteProbeOutput`) | changed: `SplitN 4`, `SessionCreated` map, window created |
| persistence | `remoteSessionCache` | unchanged: names only, no id/created |
| persistence | `remoteWindowCache` | changed: `remoteWindow.SessionCreated` serialized; cached rows stay kill-refused |
| row builder | `collectRemoteItems` | changed: host row + session rows carry created |
| row builder | `collectRemoteWindowItems` / `remoteWindowRowItem` | changed: window rows carry created |
| mirror rows | `scopedItems` (`tui.go`) | changed: created from the host row |
| stage-time guard | `^x` handler (`tui.go`) | changed: refuses a row without digits created |
| kill run | `killRun.run` (`kill.go`) | changed: passes created into both ssh kills |
| absent | shell scripts parsing `S|` (`scripts/`, `modules/`, `config/`) | none (checked) |

## File list

- `picker/remote.go` — probe format, `SessionCreated`, kill script guard + third
  arg, `remoteKillBody`/`remoteKillSessionBody`/`sshKillRemoteSession`/`sshKillRemoteSessionCtx`
  take and validate `created`, `collectRemoteItems` threads it.
- `picker/remote_wins.go` — window probe format, parser, `remoteWindow.SessionCreated`,
  window kill signatures + validation, `remoteWindowRowItem`.
- `picker/tui.go` — `remoteSessionCreated` / `remoteHostSessionCreated` fields,
  mirror row wiring, stage-time refusal.
- `picker/kill.go` — pass `item.remoteSessionCreated`.
- `picker/remote_kill_test.go` — the guard tests, `killGuardShim`, a
  `scratchTmux.sessionCreated` helper, updated call sites.
- `picker/remote_kill_argv_test.go` — arity + item created.
- `picker/remote_test.go`, `picker/remote_wins_test.go`, `picker/tui_test.go` —
  parser/format/fixture updates.
- `docs/agents/picker.md` — session and window `^x` residuals.
- `docs/superpowers/plans/2026-10-11-remote-kill-session-created.md` — this plan.

## Steps

- [ ] **Step 1: failing guard tests** (`picker/remote_kill_test.go`).
  `killGuardShim` runs the real kill body against a `tmux` shim (list-sessions
  answers, display prints a chosen created, kill-* logged). `TestKillSessionRefusesRenumberedID`
  (different created / empty / display fails → `errRemoteSessionGone`, no
  `kill-session` in the log) and `TestKillSessionKillsWhenCreatedMatches`; the
  `kill-window` twins. `scratchTmux.sessionCreated` + real values at the
  scratch-server call sites.
  Run: `cd picker && go test -run 'Kill.*Renumbered|Kill.*Matches|Kill.*BodyTargets' ./...` → red.
- [ ] **Step 2: implement the guard** (`remote.go`, `remote_wins.go`, `kill.go`).
  Single-line script with the `display` compare and `$3`; created validation;
  every call site updated. Fish-safe (no `'`/`\` in the script, no `=` outside).
  Run: step 1 → green.
- [ ] **Step 3: failing parse/format tests** (`remote_test.go`,
  `remote_wins_test.go`, `tui_test.go` fixtures). 4-field `S|` parse +
  `SessionCreated`; probe-format string assertions; `sampleRemoteWindows` and
  `killTestWindows` carry created; `killRemoteRow`/`remoteFixture` carry it.
  Run: `cd picker && go test -run 'ParseRemote|CmdFishSafe|RemoteWindowCacheRoundTrip' ./...` → red.
- [ ] **Step 4: implement probe/parse/row wiring** (`remote.go`,
  `remote_wins.go`, `tui.go`). Run: step 3 + `go test ./...` → green.
- [ ] **Step 5: docs** — `docs/agents/picker.md` residuals name the guard and
  the same-second residual.
- [ ] **Step 6: gate** — `cd picker && go build ./... && go vet ./... && go test -race ./...`;
  `nix build .#default`; `nix flake check`; `nix build .#lint`.

## Regression proof

`TestKillSessionRefusesRenumberedID` / `TestKillWindowRefusesRenumberedID` are
the regression: drop only the guard from `remoteKillScript` (keeping the new
arity) and they fail with `kill = <nil>, want the gone class` — the kill lands
on the renumbered id. Restore it and they pass, alongside
`TestKillSessionKillsWhenCreatedMatches` / `TestKillWindowKillsWhenCreatedMatches`,
which pin that a matching created still kills. Recorded in the PR `## Testing`.

## Acceptance

- A session kill and a window kill refuse (gone class, no kill exec) when the
  target id's live `session_created` differs from the probed row, and still kill
  when it matches; `cd picker && go test -race ./...` passes.
- The `S|` format change lands in both probes and the shared parser;
  `docs/agents/picker.md` residual bullets updated.
- `nix build .#default`, `nix flake check`, `nix build .#lint` pass.
