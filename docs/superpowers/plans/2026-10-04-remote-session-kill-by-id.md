# Remote session kill by id (#913)

## Problem

`^x` on a Remote-section session row runs
`remoteTmuxCmd("kill-session -t " + shellQuote("="+name))` over ssh. `shellQuote`
is POSIX single-quote escaping (`'` → `'\''`); the remote login shell on
tmux-og hosts is fish, where `\'` and `\\` are escapes inside single quotes. A
session named `x\';id;#` ends the quoted word early and the rest runs on the
remote host.

Violated invariant: **no remote-derived string reaches the remote login shell
on a kill command line.** #912 already holds it for windows by killing
`'$N:@M'`, both probe-validated ids.

## Approach

Kill by the session's `$N` id, never its name.

- The session probe switches from `list-sessions -F '#{session_name}'` to the
  window probe's own session-line format `list-sessions -F 'S|#{session_id}|#{session_name}'`.
  `parseRemoteProbeOutput` becomes a call to `parseRemoteWindowsOutput` (it
  already parses S lines strictly: `isTmuxID(f[1], '$')`, non-empty name; it
  returns no windows when there are no W lines). Both parsers now fill a new
  `remoteProbeResult.SessionIDs map[string]string` (name → `$N`; tmux session
  names are unique per server).
- Ids are **never persisted**. The session cache keeps its names-only format
  (`remoteSessionCache.Sessions []string`, unchanged). A cached `$N` would be
  dangerous: a remote server restart renumbers from `$0`, so a stale id can name
  a different session. So old-format and new-format caches are the same thing,
  and every cached row carries `remoteSessionID == ""`.
- Only rows built from a live probe get `row.remoteSessionID = result.SessionIDs[name]`
  (`collectRemoteItems`), mirroring #912's `remoteLive` precedent for window rows.
  Synthesized `(mirrored)` rows (`scopedItems`, `tui.go`) take their id from the
  host row's new `remoteHostSessionIDs map[string]string`, set from the same live
  probe (bridged sessions included — the probe's raw list is pre-suppression).
- `remoteKillSessionBody(sessionID)` → `remoteTmuxCmd("kill-session -t " + shellQuote(sessionID))`.
  `sshKillRemoteSessionCtx(ctx, host, sessionID)` refuses (new `errRemoteKillNoID`,
  no ssh spawned) unless `isTmuxID(sessionID, '$')`. `sshKillRemoteWindowCtx`
  gets the same guard on both ids (defence in depth; its ids come from the
  owner-only cache or probe).
- `kill.go` passes `item.remoteSessionID`. In `tui.go`'s `^x` handler a
  session row with no id stages nothing and sets the status line to
  `<host/sess>: listed from cache — wait for the probe, then ^x` (the window
  precedent's text). If any marked killable row (or, with no marks, the cursor row) has no id, the
  whole `^x` is refused with that message — nothing is silently dropped and the
  cursor row is never substituted. The kill never falls back to the name.

### Consumer map (probe/result/cache contract)

| edge | symbol / file | disposition |
| --- | --- | --- |
| producer | `remoteListSessionsBody` (`picker/remote.go`) | changed: S-line format |
| parser | `parseRemoteProbeOutput` → `parseRemoteWindowsOutput` | changed: strict S lines; fills `SessionIDs` |
| window producer/parser | `remoteListWindowsBody`, `parseRemoteWindowsOutput` | compatible: format unchanged; additionally fills `SessionIDs` |
| live probe | `sshListRemoteSessions` | compatible (passes parser) |
| live test | `remote_live_test.go` (skipped w/o host) | compatible: asserts on `Sessions` |
| row builder | `collectRemoteItems` / `remoteSessionsForHost` | changed: live rows carry id; host row carries id map |
| cache write/read/forget | `writeRemoteSessionCache`, `readRemoteSessionCache`, `forgetRemoteSessionCache` | compatible: unchanged names-only format |
| cached rows | `pendingRemoteItems`, `cachedRemoteSessionRows` | compatible: no id → kill refused |
| mirror rows | `scopedItems` (`tui.go`) | changed: id from host row map |
| restore rows | `remoteRestore` rows | compatible: never killable (`isKillableRemoteSession`) |
| kill run | `killRun.run` (`kill.go`) | changed: passes id |
| kill confirm prompt / log | `render_list.go:408`, `tui.go` logEvent | compatible: display name only, render-side |
| forget-after-kill | `finishKill` (`tui.go`) keyed by `remoteSess` | compatible: local bookkeeping by name, no shell |
| attach/open | `launchDetached` → og-remote-open | compatible: name gated by `require_session_name`/`shell_quotable` |

### shellQuote audit (`rg -n shellQuote picker/`)

| site | value | outcome |
| --- | --- | --- |
| `picker/remote.go` `remoteKillSessionBody` | remote session name | **fixed**: now `$N` id, validated |
| `picker/remote_wins.go` `remoteKillWindowBody` | `$N:@M` | ids only; **added** `isTmuxID` guard in `sshKillRemoteWindowCtx` |
| `picker/remotepick.go` `remotePickNewPaneArgs` | host alias from local `@remote_bridge_hosts` | not remote-derived (local config); unchanged |
| `picker/remotebridge/main.go` attach-session `-t` | `OG_BRIDGE_SESSION` from og-remote-open | gated upstream by `require_session_name` (`shell_quotable`); unchanged |
| `picker/remotebridge/cmd/daemon/main.go` `sshControlArgs` TERM/COLORTERM/TERM_PROGRAM | local env | local values; unchanged |
| same, attach-session `-t "="+session` | session from og-remote-open | gated upstream; unchanged |
| same, `pasteUploadArgs` | constant script; ext gated by `pasteExtRe` | unchanged |
| same, `reflowRunShellArgs` | local tmux `run-shell`, not ssh | unchanged |

## File list

- `picker/remote.go` — probe format, parser delegation, `SessionIDs`, kill body/ctx by id + guard, `errRemoteKillNoID`, live rows/host row carry ids.
- `picker/remote_wins.go` — `parseRemoteWindowsOutput` fills `SessionIDs`; window kill guard.
- `picker/tui.go` — `remoteHostSessionIDs` field; mirror rows get id; `^x` refuses when any target session row lacks an id.
- `picker/kill.go` — pass `remoteSessionID`.
- `picker/remote_kill_argv_test.go` — `TestKillRunSessionRowCommand` alone (compiles at base for the red run).
- `picker/remote_kill_test.go`, `picker/remote_test.go`, `picker/remote_cache_test.go`, `picker/tui_test.go`, `picker/kill_test.go` — tests below; update existing callers and fixtures (`killRemoteRow`, `remoteFixture` get ids).
- `docs/agents/picker.md` — note id-based session kill + cached rows refuse.
- `docs/superpowers/plans/2026-10-04-remote-session-kill-by-id.md` — this plan.

## Steps

- [ ] **Step 1: failing tests for the kill command** (`picker/remote_kill_test.go`). Replace `TestRemoteKillSessionBodyQuotesHostileName`/`FishSafe` with
  `TestRemoteKillSessionBodyTargetsIDOnly`: `remoteKillSessionBody("$3")` contains `kill-session -t '$3'` and `env TMUX_TMPDIR=`; and
  `TestSSHKillRemoteSessionRefusesNonID`: with a fake `ssh` on PATH that touches a marker, `sshKillRemoteSessionCtx(ctx, "lab", s)` for
  `s` in {`x\';id;#`, `a\b`, `=mono`, `""`, `$`, `$3x`} returns `errRemoteKillNoID` and the marker is absent. Plus, in its own file `picker/remote_kill_argv_test.go` using only `newKillRun`/`run`, `listItem.remoteSessionID` and `remoteKillSessionBody(string)` (no `errRemoteKillNoID`),
  `TestKillRunSessionRowCommand`: a fake ssh records its last argv to a file; a `killRun` over a row `{remoteHost:"lab", remoteSess:"x\\';id;#", remoteSessionID:"$7"}` records a command whose final arg equals `remoteKillSessionBody("$7")`, logs it with `t.Logf`, and contains neither `x\` nor `id;#`; the same row with `remoteSessionID:""` spawns no ssh. Update `TestKillRemoteSessionReachesScratchServer` and the cancel test to pass ids (`sessionID(victim)` via `display-message`, `"$0"`).
  Run: `cd picker && go test -run 'RemoteKill|KillRun|KillRemoteSession|SSHKill' ./...` → fails to compile / fails.
- [ ] **Step 2: kill by id** (`picker/remote.go`, `picker/remote_wins.go`, `picker/kill.go`). Add `errRemoteKillNoID = errors.New("remote kill has no probe-validated id")`; `remoteKillSessionBody(sessionID)`; `sshKillRemoteSessionCtx(ctx, host, sessionID)` returns `errRemoteKillNoID` before spawning unless `isTmuxID(sessionID,'$')`; same in `sshKillRemoteWindowCtx` for both ids; `sshKillRemoteSession` wrapper follows. `kill.go` passes `item.remoteSessionID`. Check `finishKill`'s error classification treats `errRemoteKillNoID` as a kept-row failure (it is not `errRemoteSessionGone`).
  In the same step, keep existing kill tests on live rows(`picker/tui_test.go`, `picker/kill_test.go`). `killRemoteRow(host, sess)` (tui_test.go) sets `remoteSessionID: "$0"`; `remoteFixture` gives `mono` `"$1"` and `other` `"$2"`; any other hand-built killable session row in kill_test.go/tui_test.go that expects ssh to run gets an id. Tests that must stay green: kill_test.go `TestKillUpdateNeverLaunchesSynchronously`, `TestKillProgressAdvancesThroughTargets`, `TestKillCancelStopsRemainingTargets`, `TestKillTimeoutSurfacesSanitizedError`, `TestKillOutcomesMixedSuccessAndFailure`; tui_test.go `TestCtrlXOnRemoteRowStagesConfirmation`, `TestRemoteKillConfirmYesKillsAndForgets`, `TestRemoteKillGoneForgetsRow`, `TestRemoteKillUnreachableKeepsRow`, `TestCtrlXWithMarksConfirmsOnce`, `TestMarkedRestoreRowNotStagedForKill`.
  Run: `cd picker && go test -run 'RemoteKill|KillRun|KillRemoteSession|SSHKill|Kill|CtrlX|MarkedRestore' ./...` → pass.
- [ ] **Step 3: failing probe-parse tests** (`picker/remote_test.go`). `TestParseRemoteProbeOutput` input `"abc123\nnoams\nS|$0|mono\nS|$4|x\\';id;#\n"` → `Sessions == [mono, x\';id;#]`, `SessionIDs == {mono:$0, x\';id;#:$4}`. A legacy names-only line (`mono`), a greeting, and `S|bad|n` produce no session. Probe command test asserts `remoteListSessionsCmd` contains `list-sessions -F 'S|#{session_id}|#{session_name}'`. `parseRemoteWindowsOutput` test asserts `SessionIDs` too.
  Run: `cd picker && go test -run 'ParseRemote|ProbeCmd|RemoteWindowsOutput' ./...` → fail.
- [ ] **Step 4: probe emits and parses ids** (`picker/remote.go`, `picker/remote_wins.go`). Change `remoteListSessionsBody`; `parseRemoteProbeOutput` returns `parseRemoteWindowsOutput(stdout)`; S branch fills `res.SessionIDs`. Update comments.
  Run: step 3 command → pass.
- [ ] **Step 5: failing row/cache tests** (`picker/remote_test.go`, `picker/remote_cache_test.go`). (a) `collectRemoteItems` with a fake probe returning `Sessions:[mono]`, `SessionIDs:{mono:$2}` → the mono row has `remoteSessionID == "$2"` and the host row's `remoteHostSessionIDs["mono"] == "$2"`. (b) Old-format cache: write `{"host":"lab","saved_at":<now>,"sessions":["mono"]}` by hand under a temp `XDG_CACHE_HOME`, `pendingRemoteItems` yields a mono row with `remoteSessionID == ""`; a garbage cache file (`{"sessions":7}`) yields no rows and no panic.
  Run: `cd picker && go test -run 'CollectRemoteItems|RemoteCache|PendingRemote' ./...` → fail.
- [ ] **Step 6: live rows carry ids** (`picker/remote.go`, `picker/tui.go`). Add `remoteHostSessionIDs map[string]string` to `listItem` (comment: host row only; the live probe's name→`$N`, bridged sessions included). In `collectRemoteItems`, keep `result.SessionIDs` in `hostResult`; set it on the host row and set `remoteSessionID` on each live session row. In `scopedItems`, set mirror rows' `remoteSessionID = block[0].remoteHostSessionIDs[bm.sess]`.
  Run: step 5 command → pass.
- [ ] **Step 7: failing UI refusal test** (`picker/tui_test.go`). Model with a cached session row (`row.remoteSessionID = ""` set explicitly) under cursor, `^x` → `killConfirm` empty, `statusMsg` contains `wait for the probe`. A live row with id → `killConfirm` staged. Marked rows where any marked killable row lacks an id → nothing staged (the cursor row is not substituted) and the same status message. A mirror row in host scope whose host row map has the id → staged with that id.
  Run: `cd picker && go test -run 'Kill' ./...` → fail.
- [ ] **Step 8: refuse id-less rows in the UI** (`picker/tui.go`). In the `^x` `isKillableRemoteSession` branch: if any killable marked row, or (with no marks) the cursor row, has `remoteSessionID == ""`, set `<label>: listed from cache — wait for the probe, then ^x` and stage nothing. `killableMarkedRemoteItems` is unchanged (the refusal happens before staging, so an id-less mark never silently drops and never falls back to the cursor row). Update the `remoteSessionID` field comment (tui.go:64): session rows (live, mirror) carry it too.
  Run: step 7 command → pass.
- [ ] **Step 9: docs** — `docs/agents/picker.md`: session `^x` kills by `$N` from the live probe; cached rows refuse until the probe answers; ids are never cached (server restart renumbers).
- [ ] **Step 10: gate** — `cd picker && go test -race ./...`; `nix flake check`; `nix build .#lint`.

## Regression proof

Put `TestKillRunSessionRowCommand` in its own file (`picker/remote_kill_argv_test.go`); it compiles against HEAD because `listItem.remoteSessionID` exists and `remoteKillSessionBody(string)` keeps its signature. Copy only that file into a scratch `git worktree` at the base commit, run `go test -run TestKillRunSessionRowCommand ./...` there, and record the red failure: the recorded ssh argv contains `x\';id;#`. Then run it green on the branch. Record both in the PR `## Testing`.

## Acceptance

- Hostile names (`x\';id;#`, plain `\`) → kill command contains no part of the name, or is refused: Steps 1–2 tests (`TestKillRunSessionRowCommand`, `TestSSHKillRemoteSessionRefusesNonID`) with the argv logged.
- Probe parses `session_id`; old-format cache loads with id missing, kill refused, no crash, no name fallback: Steps 3–8 tests.
- shellQuote audit listed in PR body: the audit table above.
- `nix flake check` and `nix build .#lint` pass: Step 10.
