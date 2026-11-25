# Spec: a local client grow never reaches the remote when the debounced reflow cannot nudge (#1266)

Base: main 8d63ce19. Plan: `docs/superpowers/plans/2026-11-25-bridge-grow-nudge.md`.

## Symptom

On the macOS CI runner, `float-drag-integration-tests` 20/21 (sometimes 22) fail intermittently. The local client shrinks (45x14), then grows (150x40). The remote control client stays at the shrink width, and the 20 s wait in `resize_client` times out with `remote window never reached 150x38 within 20s: remote now 45x12, local mirror now 45x12`.

## Evidence (verified)

1. Failing darwin runs on main after #1265, which added an unconditional `daemon: local client area WxH (<trigger>), client size asserted=<bool>` line to `watchLocalClient`:
   - 45568763804, test 20: `45x12 (nudge) asserted=true`, then `45x12 (nudge) asserted=false`, then nothing for the grow. Tests 21 and 22 show the same two-line shape.
   - 45572258862, test 21: the same shape.
   - In both runs `resize_diag` shows the debounce stamp `/tmp/og-reflow-debounce.host-sess` owned by `_nixbld2`, with an mtime about 10 minutes older than the test. This build's `client-resized` reflows therefore never wrote it.
2. Mechanism, from the code:
   - `scripts/tmux-reflow-windows.sh` `--debounce` (the `client-resized` hook at `config/tmux.conf.tmpl:533`, and `client-detached[10]`) writes `$EPOCHREALTIME` to the fixed path `/tmp/og-reflow-debounce.<session>`, sleeps 0.12 s, reads the file back, and exits unless it holds its own token.
   - When the file belongs to another user, the write fails and the read returns the other user's token, so every debounced reflow exits. It exits before the `touch -- "$NUDGE"` that is the bridge's resize nudge (#433).
   - Darwin nix builds share `/tmp` across `_nixbldN` build users. Several checks use session `host-sess`, so whichever build first creates the stamp locks every other build user out.
   - The mirror window is pinned to the remote's size, `window-size manual` via `FitWindowCmd` (`picker/remotebridge/daemon/mirror.go`). A client grow therefore fires no local `window-resized`, and the `window-resized[20]` nudge never fires either. No nudge at all is left for a grow.
   - `watchLocalClient` (`picker/remotebridge/daemon/daemon.go`) reads `LocalArea` only when the nudge mtime advances, or at the 30 s `resizeFallbackInterval`, which is longer than the test's 20 s wait.
   - The grow can still land by luck. The daemon's own refit after the shrink (`resize-window` → `window-resized[20]` → touch) leaves a nudge. When the next 1 s watcher tick that consumes it runs after the grow, `area()` already reads 150x38. Linux happens to win that race, which is why it never failed there.
3. Local deterministic repro, Linux, nix sandbox (`nix build .#checks.x86_64-linux.float-drag-integration-tests`, with temporary test edits that have since been reverted): test 20 with a read-only stale `/tmp/og-reflow-debounce.host-sess` and `sleep 2` between the shrink's `settle_agree` and the grow.
   - It fails with the exact macOS signature (`remote now 45x12`, the same two daemon lines).
   - With a writable stamp it passes and logs `150x38 (nudge) asserted=true`.
   - A reflow trace confirmed `debounce-bail` for every `client-resized` while the stamp was read-only.
   - The `size owner claimed twice` lines in the issue come from two different tests (20 and 21), each with its own daemon. They are unrelated.

## Violated invariants

- I1: a local client resize of a mirror session must reach the remote (`refresh-client -C`) within the watcher's normal latency (a few seconds), not just at the 30 s fallback.
- I2: an unrelated user, or another tmux server, must never be able to silence a session's debounced reflow (status-bar reflow and bridge nudge on `client-resized` / `client-detached`).

## Scope (approved by the dispatcher over the bus; `sizeowner.go` stays untouched)

### (1) Script: `scripts/tmux-reflow-windows.sh` debounce stamp

- Move the stamp from `/tmp/og-reflow-debounce.<session>` to `${TMPDIR:-/tmp}/og-reflow-debounce.<uid>.<socket>.<session>`, using the same `${TMPDIR:-/tmp}` base as the reflow lock.
  - `<uid>` is bash `$UID`.
  - `<socket>` is the server socket path, `${TMUX%,*,*}`, with `/` mapped to `_`. tmux sets `TMUX=<socket>,<pid>,<idx>` in every `run-shell` job's environment. The path is stable across server restarts, so no stamp leaks per restart. When `$TMUX` is unset, the field is empty.
  - `<session>` keeps the existing `/`→`_` mapping.
  - No fork: parameter expansion only.
- Fail open: if writing the token fails (the redirection's exit status), skip the debounce and continue with the reflow. A stamp the script cannot write must never make it exit. A redirection failure prints nothing (`2>/dev/null` placed before the `>` redirection, so it also covers bash's own open error).
- No other behaviour change. The nudge touch stays after the debounce, so only the last invocation of a burst touches it. The lock and cache are unchanged.

### (2) Daemon: client geometry from the existing reveal poll wakes the resize watcher

- `clientViewsFormat` (`reveal.go`) gains three trailing `|`-delimited fields: `#{client_width}|#{client_height}|#{status}`. Same `list-clients` call, no new fork.
  - `activityRows` must keep parsing: it splits to the new field count and still reads `client_flags` / `client_activity` by index.
  - `clientViews` keeps reading only the first four fields.
- `watchReveal` computes, per poll, a geometry signature over the non-control rows: the key `name|created` maps to `width|height|status`. When a successful poll's signature differs from the previous successful one, it marks a shared signal. It also marks on the first successful poll, so a grow that lands during startup is not silently baselined. `cv.need` makes that read a no-op send when nothing changed. A failed query keeps the previous baseline. This covers a resize, a status-line count change, and a client attach or detach.
- `watchLocalClient` takes the signal (a new parameter: a consume-once func, or a small mutex/atomic type with `mark`/`take`) and consumes it every tick.
  - Trigger order: `nudge` (mtime advanced), else `clients` (signal was marked), else `fallback`.
  - Every tick consumes the signal, even when the nudge also fired, so one event never causes a second area fork on the next tick.
  - A `clients` trigger runs the same path as a nudge: read `area()`, client size first, then per-window caps active-first, then the view re-resolve. It also resets `lastCheck`.
- Production wiring in `Run` (`daemon.go` near line 1470/1495): one signal instance shared by both goroutines. When `cfg.LocalSess == ""` the reveal watcher does not run, so nothing marks and behaviour is unchanged.
- Latency of a grow with no nudge: at most about 2 s (reveal tick, then watcher tick), against 30 s today.

### (3) Size-decision debug log behind an env var

- `--size-debug` flag on `og-remote-bridge-daemon` (`cmd/daemon/main.go`), defaulting from env `OG_BRIDGE_SIZE_DEBUG` (non-empty and not `0` means on). It sets `Config.SizeDebug`, and `Run` arms a package-level switch from it. Off by default: one atomic load per log site.
- Lines on stderr, prefixed `daemon: size: `:
  - (a) watcher: every tick that has a trigger, with the trigger, the nudge mtime (RFC3339Nano), and the area read. Each send it decides (client size, and each per-window cap) or skips as already asserted, and each failed send.
  - (b) reveal poll: a geometry change, old signature → new.
  - (c) wire trace in `stream`: every command written through `stampAll` that starts with `refresh-client -C ` or carries `ignore-size` (the `ClientSizeCmd`, `ConvergeCmd` and `recalcCmd` sends, and the owner's flag decisions `ignoreSizeCmd`). Logged with its ordinal when sent and, from `claimSeq`, with its reply kind (`%end`/`%error`) and trimmed body when claimed. Size-related traffic only, nothing else.
- The pending-ordinal map is only allocated or written when debug is on. Entries are deleted when claimed. A closed stream drops the map.
- `float-drag-integration-tests` runs its daemons with `OG_BRIDGE_SIZE_DEBUG=1`, and its failure dumps print enough of the daemon log to show the decisions (the existing `tail` widened). `resize_diag`'s stamp line points at the new stamp path.

### Tests

- Go, red→green, through `watchLocalClient`: the failing order. A nudge is consumed (45x12 asserted), the refit's nudge is consumed (45x12, nothing sent), then the client grows with no further nudge and the signal is marked. Expect `ClientSizeCmd(150,38)` plus the per-window caps.
  - Red evidence: the new test against a scratch copy where the watcher ignores the signal (old behaviour) fails on the assertion. A compile failure doesn't count as red.
  - Also: no area read when nothing marked and nothing nudged; a nudge and a mark in the same tick cause one area read, not two.
- Go, `watchReveal`: a size change (width, height or status) and an attach or detach mark the signal; an identical poll, the first poll and a failed query do not; `activityRows` still parses the extended rows (existing tests extended).
- bats, script-level, red→green (`stamp owned elsewhere or unwritable, reflow still nudges`): drive the real `tmux-reflow-windows --debounce` against a private config-less tmux server whose session carries `@bridge_nudge`, under two conditions. Expect the nudge file touched both times.
  - (i) A stale foreign stamp at the legacy path, which is unwritable because it is `chmod 444`. The test cannot create another user's file.
  - (ii) An unwritable stamp at the new path.
  Before the fix (i) fails. (ii) passes against the old script by construction, since the old script never touches the new path, so its counterfactual is the new path without fail-open. The cases live in `tests/reflow-fanout.bats`, the existing check that runs the real script.
- bats, end to end (`float-drag-integration.bats`): the deterministic repro as a regression case. Before the grow, an unwritable stale stamp sits at both the legacy path and the new path (socket path via `display-message -p '#{socket_path}'`), and the grow comes 2 s after the shrink settles. The remote must reach 150x38. Red before (shown above), green after (fixes 1 and 2 each close it independently).

### Docs

- `docs/agents/scripts.md`: `tmux-reflow-windows` row: stamp location and keying, fail-open.
- `docs/agents/bridge-daemon.md`: the reveal poll's geometry fields and the watcher's `clients` trigger, next to the #433 nudge text and `watchReveal`'s description; the `OG_BRIDGE_SIZE_DEBUG` / `--size-debug` switch and what it logs.
- `docs/agents/performance.md`: only if its debounce line names the path; otherwise untouched.
- Commit this spec and the plan under `docs/superpowers/specs/` and `docs/superpowers/plans/` (CLAUDE.md).

## Non-goals

- `sizeowner.go`.
- The reflow lock's `${TMPDIR:-/tmp}` cross-user exposure on a multi-user Linux host without TMPDIR (a deferred follow-up).
- Shortening `resizeFallbackInterval`.
- Lengthening any existing test wait, or adding retries.

## Acceptance

- The PR names the root cause, citing 45568763804 and 45572258862 and the local repro.
- A Go watcher test that is red before and green after.
- A script bats test that is red before and green after.
- `float-drag-integration-tests` green on 3 consecutive darwin runs of the final head.
- `cd picker && go test -race ./remotebridge/...`, `nix build .#default`, `nix flake check` and `nix build .#lint` all pass.
