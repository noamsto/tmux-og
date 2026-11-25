# Plan: a local client grow never reaches the remote when the debounced reflow cannot nudge (#1266)

Spec: `docs/superpowers/specs/2026-11-25-bridge-grow-nudge-design.md`. spec-critic accepted it with low findings, which are folded in below as F1–F6.
Base: main 8d63ce19.
Orchestration consult: not run. The blast radius is three contained seams (one script, two daemon watchers plus the stream, bats and docs), and no shared interface changes beyond package-internal signatures.

## Spec-critic findings folded in

- F1: `watchReveal` also marks on the first successful poll. `cv.need` makes the resulting area read a no-op send when nothing changed.
- F2: the stamp key uses the server socket path, not the pid. It is the part of `$TMUX` before the last two comma fields: `sock=${TMUX%,*,*}`, mapped `/`→`_`. The socket path is stable across server restarts, so there is no per-restart file leak (F3), and it still separates servers and users (the tmux socket dir is per-uid). `$UID` is kept in the name anyway, because TMUX_TMPDIR can be shared. When `TMUX` is unset (a direct invocation), the socket part is empty.
- F3: no stamp deletion. A bailing or surviving invocation removing the stamp would race the next burst. With F2 the stamp count is bounded by (uid, socket, session).
- F4: the PR text names the shrink-side nudge: a leftover nudge from the daemon's own earlier refit (the attach-time `resize-window` → `window-resized[20]`), consumed on the next tick. This is the same lucky-leftover mechanism.
- F5: the bats red/green cases assert that the stamp is really unwritable (`[[ ! -w $stamp ]]`), and skip with a reason when it is writable (running as root).
- F6: when debug is off, every log site costs a single `atomic.Bool` load. Tests toggle the switch only through its atomic setter, and restore it with `t.Cleanup`.

## Steps

Dependencies: S1 ∥ S2 → S3 (after S2) → S4 (after S1, S2 and S3) → S5 (lead).

### S1: debounce stamp, per user and server, fail-open, plus a script regression (implement: execute)

Files: `scripts/tmux-reflow-windows.sh`, `tests/reflow-fanout.bats`.

1. In the `if ((DEBOUNCE))` block, replace the stamp path with
   `"${TMPDIR:-/tmp}/og-reflow-debounce.${UID}.${sock//\//_}.${SESSION//\//_}"`, where `sock` is `${TMUX%,*,*}` when `TMUX` is set and empty otherwise. Parameter expansion only, no fork.
2. Write the token with `2>/dev/null` placed before the `>` redirection, and branch on the write's status:
   - If the write fails, skip the debounce entirely and fall through to the reflow (fail-open).
   - If it succeeds, keep the existing sleep 0.12, read-back and token compare.
3. Update the block's comment: why the per-user/server key exists (#1266), and why a write failure must fail open.
4. In `tests/reflow-fanout.bats` (the existing real-script reflow harness: private config-less server, `TMPDIR=$TDIR`), add a commented section "debounce stamp (#1266)" with these `@test` cases. Each one sets `@bridge_nudge` on session S to a file under `$TDIR`. It runs the reflow through `tmux run-shell "$REFLOW --debounce S 200"` (synchronous, so `TMUX` is set exactly as it is in the hook), then asserts that the nudge file exists, created by the reflow's touch:
   - (a) A stale foreign stamp at the LEGACY path `/tmp/og-reflow-debounce.S` (content `stale`, mode 444; F5 unwritable check or skip). The reflow still nudges.
     - Red before: the old script reads `stale` and exits.
     - The test removes the file in teardown only if it created it. Use a session name unique to the test, e.g. `S` plus `$$`, if the legacy path could clash. Otherwise `S` is fine inside the nix sandbox.
   - (b) An unwritable stamp at the NEW path: `$TDIR/og-reflow-debounce.$UID.<socket_path with / → _>.S`, where `socket_path` comes from `tmux display-message -p '#{socket_path}'`. Same assertions. This is the fail-open proof.
   - (c) A writable path still debounces. Two overlapping `--debounce` runs, the first stamped and the second stamping during the first's sleep: only one nudge-touching pass. Assert it in a cheap way, e.g. the first run exits before touching. Keep the case only if it is deterministic. Otherwise drop it and say why in the step report.
5. Red evidence: run (a) against the unmodified script (a scratch copy of the old script; never stash live work) and record the failing assertion. (a) is the red-before proof. (b) passes against the old script by construction, since the old script never touches the new path. Its counterfactual is therefore the new path WITHOUT fail-open: a scratch copy that keeps the read-back after a failed write. Record that failure. Then run everything green with the fix.
   - Command: `nix build .#checks.x86_64-linux.reflow-fanout-tests -L`.
   - The check's `scriptsSrc` already copies `tmux-reflow-windows.sh` (coreScripts), so no flake change is needed. Verify that.

### S2: client geometry from the reveal poll wakes the resize watcher (implement: execute)

Files: `picker/remotebridge/daemon/reveal.go`, `picker/remotebridge/daemon/daemon.go`, and their tests (`reveal_test.go`, `daemon_test.go`, `size_test.go`, plus any other `watchLocalClient(` / `watchReveal(` / `activityRows` caller, found with `rg`).

1. `clientViewsFormat`: append `|#{client_width}|#{client_height}|#{status}` (9 fields).
   - `activityRows`: split to 9 and require 9 fields; fields 4 and 5 are unchanged.
   - `clientViews` is unchanged: it splits to at most 6 and reads the first 4. Update the format doc comment.
2. A new small type, e.g. `geomSignal`, with `mark()` and `take() bool` (mutex or `atomic.Bool` Swap). Plus a pure helper `clientGeoms(out string) map[string]string` that maps a non-control row's `name|created` to `w|h|status` and skips malformed rows. Put both in `reveal.go`.
3. `watchReveal` gains a `geom *geomSignal` parameter (nil-safe: nil means never mark).
   - After a successful query, compute `cur := clientGeoms(out)` and mark when this is the first successful poll (F1) or `!maps.Equal(prevGeom, cur)`.
   - A failed query keeps `prevGeom` and does not mark.
4. `watchLocalClient` gains a parameter `clientsMoved func() bool`, the consume-once take. On every tick, call `nudged()` first, then call `clientsMoved()` unconditionally (consume it), then decide the trigger:
   - The trigger is `nudge` if the mtime advanced.
   - Else `clients` if it was marked.
   - Else `fallback` if 30s have elapsed.
   - The rest of the body is unchanged, including `lastCheck = now`.
   - Update the doc comment: the reveal poll is a second, hook-independent trigger, citing #1266.
5. `Run` wiring: one `geom := &geomSignal{}` shared between the `watchReveal` call (`daemon.go` ~1495) and `watchLocalClient` (~1474), passed as `geom.take`. When the reveal goroutine does not start, nothing marks.
6. Tests:
   - (a) The NEW `TestWatchLocalClientGrowWithoutNudge` in `daemon_test.go`, in the style of `TestWatchLocalClientReconvergesOnChange` (channels and barrier ticks, no sleeps). The failing order:
     - tick 1: nudge t1, area 45x12 → client size and caps sent.
     - tick 2: nudge t2 (the refit), area 45x12 → nothing.
     - tick 3: same mtime, clientsMoved true, area 150x38 → `ClientSizeCmd(150,38)` plus each window's cap.
     - tick 4: same mtime, clientsMoved false → area NOT read. Count the area calls, or select on sizeCh with a timeout, so an unexpected read fails fast instead of hanging.
     - Also one tick with both a nudge and a mark → exactly one area read.
   - (b) Red evidence: in a scratch copy of the package (never the worktree), make `watchLocalClient` ignore `clientsMoved` and run (a). Record the assertion failure (missing `refresh-client -C 150x38`). A compile failure is not red.
   - (c) First migrate the existing 6-field fixtures in `reveal_test.go` (`TestActivityRows`, `TestWatchRevealReportsActivity`, `TestWatchRevealPlacedActivityOnlyFromMirrors`, and the `step()` rows) to 9 fields. This is mechanical and expected once `activityRows` requires 9. Then add `watchReveal` tests. A first successful poll marks. An identical poll does not. A width, height or status change marks. A client attach or detach marks. A failed query neither marks nor resets the baseline. Control-mode rows are ignored. `activityRows` parses 9-field rows and rejects 6-field ones.
   - (d) Update every existing caller to the new signatures (pass `func() bool { return false }` / nil).
7. `cd picker && go test -race ./remotebridge/...` and `golangci-lint run ./remotebridge/...` (devShell) green.

### S3: size-decision debug log behind `OG_BRIDGE_SIZE_DEBUG` (implement: execute)

Files: `picker/remotebridge/cmd/daemon/main.go`, `picker/remotebridge/daemon/daemon.go` (Config, Run, stream, claimSeq, watchLocalClient), `picker/remotebridge/daemon/reveal.go`, a new `picker/remotebridge/daemon/sizedebug.go` with a `sizedebug_test.go`. sizeowner.go is NOT edited.

1. `sizedebug.go`:
   - A package switch `var sizeDebug atomic.Bool`.
   - `func sizeDebugf(format string, args ...any)`, which returns immediately when the switch is off and otherwise writes `daemon: size: <msg>\n` to stderr through a package `io.Writer` var that tests can swap under a mutex.
   - `func isSizeTraced(cmd string) bool`, true for the prefix `refresh-client -C ` or any command containing `ignore-size`.
2. `Config.SizeDebug bool`. `Run` stores it into the switch first thing, and never clears it on return.
   - main.go: `--size-debug` bool flag, defaulting from env `OG_BRIDGE_SIZE_DEBUG` (on when non-empty and not `0`), passed into Config.
3. Log sites (all through `sizeDebugf`):
   - (a) `watchLocalClient`, on a tick with a trigger: the trigger, the nudge mtime (`RFC3339Nano`, or `none`) and the area. Per send: `client size WxH sent|skipped (asserted)|send failed`, and `cap @N WxH sent|skipped|send failed`.
   - (b) `watchReveal`, on a mark: `clients changed: <old sorted sig> -> <new sorted sig>` (bounded: names, created and sizes only).
   - (c) `stream`: a `traced map[uint64]string` field, written only when `sizeDebug` is on.
     - In `stampAll`, for each traced cmd, record `seq → cmd` and log `sent #seq <cmd>`.
     - In `claimSeq`, after `st.claim` returns `seq != 0`, a method `st.traceReply(seq, l)` takes the entry under `st.mu`. If present, it deletes it and logs `reply #seq to <cmd>: %end|%error <first 200 bytes of body, trimmed>`.
     - `close()` drops the map.
4. Tests in `sizedebug_test.go`:
   - Off: `stampAll` and `claimSeq` leave `traced` nil and write nothing.
   - On: a traced send then its End reply logs both lines and empties the map. An Error reply logs `%error`. An untraced command (`display-message`) is never recorded.
   - Restore the switch and writer with `t.Cleanup`. The tests that toggle the global must not run in parallel.
5. `go test -race ./remotebridge/...` and golangci-lint green.

### S4: float-drag harness, debug on, and the end-to-end regression (implement: execute)

Files: `tests/float-drag-integration.bats` only. `flake.nix` only if an env var must be declared, which it should not need, since the bats sets it inline.

1. Both daemon launches (`mirror_up`, `mirror_tiled_up`) run with `OG_BRIDGE_SIZE_DEBUG=1` in their environment.
2. The daemon-log dumps in `resize_client` and `settle_agree` widen from `tail -20` to `tail -80`, so the size lines show.
3. `resize_diag`'s stamp line lists the new stamp path for `host-sess` on DST: `${TMPDIR:-/tmp}/og-reflow-debounce.$UID.<DST socket_path with / → _>.host-sess`, with the socket path read through `$DST display-message -p '#{socket_path}'`. It keeps printing the legacy path too.
4. A new `@test`: "a client grow reaches the remote when the debounced reflow cannot nudge (#1266)".
   - Run `mirror_up` with the test-20 float args.
   - Plant unwritable stale stamps at both the legacy `/tmp/og-reflow-debounce.host-sess` and the new path (F5: assert `! -w`, or skip). Remove the legacy one in teardown only if this test planted it.
   - `resize_client 45 14`, `settle_agree`, then `sleep 2`. The sleep is a deliberate ordering delay that lets the refit's leftover nudge be consumed before the grow. It is not a wait for a condition. A comment says so and cites the repro.
   - `resize_client 150 40`, `settle_agree`.
   - Red before (both fixes absent): the exact macOS signature, as the local repro showed. Green after.
5. Run `nix build .#checks.x86_64-linux.float-drag-integration-tests -L`: all cases green. Show red evidence for the new case with the S1 and S2 behaviour reverted in a scratch tree, or cite the spec's recorded repro if a scratch revert is impractical, saying why.

### S5: docs, spec and plan commit, gate (lead, no subagent)

1. `docs/agents/scripts.md`, the `tmux-reflow-windows` row: the debounce stamp location and key (`${TMPDIR:-/tmp}`, uid, socket, session), fail-open on an unwritable stamp, and #1266.
2. `docs/agents/bridge-daemon.md`:
   - The reveal poll's geometry fields and the watcher's `clients` trigger, next to the #433 nudge, `watchReveal` and `localArea` text.
   - The `OG_BRIDGE_SIZE_DEBUG` / `--size-debug` switch and what it logs.
3. Commit the spec as `docs/superpowers/specs/2026-11-25-bridge-grow-nudge-design.md` and the plan as `docs/superpowers/plans/2026-11-25-bridge-grow-nudge.md`.
4. Gate: `nix build .#default`, `nix flake check`, `nix build .#lint`, and `cd picker && go test -race ./remotebridge/...`.
5. After the push, rerun the darwin build job on the final head (`gh run rerun <id>`) until `float-drag-integration-tests` is green on 3 consecutive darwin runs of that head. Cite the run ids and attempts in the PR.

## Risks

- `activityRows`' strict field count is the one consumer of the extended format. S2 covers it with tests.
- `watchLocalClient` signature churn touches several test call sites. This is mechanical.
- The stream trace runs in the hot write path. When debug is off it costs one atomic load per batch command (`isSizeTraced` is only evaluated when the switch is on).
- `float-drag` adds about 3 s of wall time for the new case.

## Acceptance (from the task, with dispatcher amendments)

- The PR names the root cause, citing 45568763804 and 45572258862 and the local repro.
- The Go watcher test is red before (scratch counterfactual) and green after.
- The script bats test is red before and green after.
- `float-drag-integration-tests` is green on 3 consecutive darwin runs of the final head.
- `go test -race ./remotebridge/...`, `nix build .#default`, `nix flake check` and `nix build .#lint` all pass.
