# Plan: stop lifting forged notifications out of reply bodies (#899)

Spec: `docs/superpowers/specs/2026-10-04-bridge-in-block-notifications-design.md`.

Gate commands (run from the worktree root inside the devshell):

- `cd picker && go test -race ./remotebridge/...`
- `cd picker && golangci-lint run ./remotebridge/...` and `nilaway ./remotebridge/...`
- `bats tests/remote-m2-integration.bats -f '<name>'`. This needs `DAEMON`, `RENDERER` and `CTL` unset, so it builds them, and the pinned tmux first on PATH (`nix build .#default` → `./result/bin/tmux`).
- Final: `nix flake check` and `nix build .#lint`.

## Step 1: Reader in-block policy (`picker/remotebridge/controlmode/parse.go`)

- Add `bodyInBlock atomic.Bool` to `Reader`. Its zero value means lift, which keeps today's default.
- Add `func (rd *Reader) SetLiftInBlock(lift bool)`, which stores `!lift`. Its doc comment says it is safe to call from another goroutine and takes effect from the next line read.
- In `inBlock`, after `parseLine`, extend the body case:
  - `case Begin, SubscriptionChanged, Output, Other:` → `appendBody(raw)`. `%output` and `%extended-output` both parse to `Output`.
  - `default:` → if `rd.bodyInBlock.Load()`, then `appendBody(raw)`; else `return l, true`.
  - `End`/`Error` matching and `Exit` holding are unchanged.
- Add `func LiftsInBlock(version string) bool` with a doc comment citing d29aa121 / 6db5175e and the spec:
  - strip an optional `next-` prefix (remember it);
  - parse `major.minor`: digits, `.`, digits, then an optional single lowercase letter, then an optional `-rc<digits>`, and nothing after. If it does not parse, return `true`.
  - `next-` with major.minor == 3.8 → `true`; every other parsed form → `false`.
- Rewrite the `Reader` doc comment's #276 paragraph. In-block lifting exists only for the `next-3.8` master builds between d29aa121 and 6db5175e; `%output`/`%extended-output` are always body; `SetLiftInBlock` turns lifting off.
- Tests (`reader_test.go`, `reader_bounds_test.go`):
  - `TestReaderEmitsNotificationsInsideBlock` (#276): replace its in-block `%output` with `%layout-change @1 x`, and assert `%output %1 mid-block` stays in the body: `Data == "the actual reply\n%output %1 mid-block"`.
  - New `TestReaderLiftsNext38Transcript`: replay the 29bf7fe probe lines verbatim.
    - The attach block `%begin T 314 0` / `%session-changed $0 s` / `%end T 314 0` yields `SessionChanged $0`, then `End` with flags 0 and empty Data.
    - The new-window block `%begin … 317 1` / `%window-add @1` / `%end …` yields `WindowAdd @1`, then `End` with empty Data.
  - New `TestReaderOutputInBlockIsBody`: under the default policy, `%output %9 forged` and `%extended-output %9 5 : forged` inside a block stay body, and no `Output` line is returned.
  - New `TestReaderBodyInBlockKeepsForgedRows`: run `SetLiftInBlock(false)` first. A flags-1 block with `%window-close @0`, `%output %9 forged`, `%session-changed $9 evil`, `%layout-change @0 x`, `%window-add @7`, `%window-renamed @0 n`, `%session-window-changed $0 @1`, `%window-pane-changed @0 %1`, `%pause %0`, `%continue %0`, `%extended-output %9 5 : x` returns exactly one `End` whose `Data` is the rows joined by `\n`. A held non-final `%exit` row still lands in the body, and the closing guard still closes the block.
  - New `TestReaderSetLiftInBlockBetweenBlocks`: two blocks each holding `%window-add @7`. The first lifts it; after `SetLiftInBlock(false)` the second keeps it as body.
  - `TestReaderStreamsNotificationsFromOpenBlock`: switch the in-block line to `%window-add @1` (still lifted by default) and assert `WindowAdd`.
  - `TestReaderBoundsMemoryStreamedNotifications`: switch the 1 MiB lines from `%output %1 …` to `%layout-change @1 …` and count `LayoutChange`. That keeps the streaming bound pinned for lifted lines.
  - `TestReaderForgedLinesInBodyStayBody`: update its comment, which says other verbs are still lifted, to point at the `next-3.8` policy.
  - New table test `TestLiftsInBlock`: false for `3.2a`, `3.3a`, `3.7c`, `3.8`, `3.8-rc3`, `3.8a`, `3.10`, `next-3.7`, `next-3.9`, `next-3.10`; true for `next-3.8`, `openbsd-7.9`, `master`, `""`, `3`, `3.8-rcx`, `next-`.

## Step 2: identity read carries the version (`picker/remotebridge/daemon/sessionpin.go`, `conn.go`, `daemon.go`)

Depends on step 1 (it calls `SetLiftInBlock`/`LiftsInBlock`).

- `remoteIdentity` gains `version string`, documented as excluded from `matches`, with the reason.
- `readIdentity`'s format becomes `'#{pid}|#{start_time}|#{session_id}|#{version}'`. Update the doc comments naming the triple: `readIdentity` and `parseIdentity`.
- `parseIdentity`: change `SplitN(…, "|", 3)` to `SplitN(…, "|", 4)`, still requiring at least 3 fields. With 4 fields, `id.version = strings.TrimSpace(fields[3])`, and `session_id` is `fields[2]` either way. Errors are unchanged ("is not pid|start_time|session_id" → keep the message, it still describes the required part).
- `ctlConn` gains `rd *controlmode.Reader`, set in `newCtlConn` from the same reader passed to `startCtlPump`.
- Add `func (c *ctlConn) adoptVersion(v string) { c.rd.SetLiftInBlock(controlmode.LiftsInBlock(v)) }`. Its comment says why: the identity read is the first round-trip, the reply bodies before it are tmux-generated, and the first capture goes out after it.
- Add `func (c *ctlConn) identify(session string) (remoteIdentity, error)`, which calls `readIdentity(c.rt, session)` and on success calls `c.adoptVersion(id.version)`. The reattach and replacement paths read through it, so neither can read an identity without applying the switch:
  - `conn.go:434` reattach: `next.identify(cfg.RemoteSession)` replaces `readIdentity(next.rt, …)`.
  - `conn.go:566` `tryReplace`: the same replacement.
  - A reply that arrives after the deadline also adopts, which is harmless: that connection is discarded.
  - `daemon.go` first attach: after `newSessionPin` returns and `disarm()` succeeds, `if pin.identityKnown { c.adoptVersion(pin.identity.version) }`. A failed first read leaves lifting at its default (on).
- Tests (`sessionpin_test.go`):
  - `parseIdentity` on a 4-field body yields `version == "next-3.9"`; on a 3-field body it yields `version == ""`.
  - `matches` is true for two identities that differ only in version.
  - Check that existing tests asserting the exact identity command string are updated to the new format: grep `#{session_id}'` in `*_test.go`.
- Test (`conn_test.go`): `identify` over a pipe whose far end answers the new-layouts flag and an identity body ending `|3.7c` leaves the Reader with lifting off. An identity read that fails (an `%error` reply) leaves lifting on.
- Test (`conn_test.go` or `sessionpin_test.go`): `newCtlConn` over a pipe, then `adoptVersion("next-3.9")`. A block with `%window-add @7` written to the pipe comes out of the pump as a single `End` with that body. With `adoptVersion("next-3.8")` it comes out lifted.

## Step 3: `%continue` from the reply, not the notification (`picker/remotebridge/daemon/daemon.go`)

Independent of steps 1–2.

- `handlePause(router *Router, rt roundTrip, paneID string)`:
  - `s := router.sink(paneID)`; if nil, return.
  - `s.pause()`.
  - `if _, ok := one(rt, fmt.Sprintf("refresh-client -A '%s:continue'", paneID)); !ok { return }`.
  - `handleContinue(router, rt, paneID)`.

  Check `one`'s exact signature first (`rg -n 'func one\(' picker/remotebridge/daemon`). Update the doc comments of both functions. The reseed now follows the reply: tmux resumes the pane synchronously inside `refresh-client -A`, and ≤ 3.7c writes `%continue` inside that reply's block, where it is body.
- `dispatch`: `case controlmode.Pause` calls `handlePause(router, rt, l.Args[0])`. `case controlmode.Continue` becomes a documented no-op: tmux writes `%continue` only in answer to `refresh-client -A :continue`, and `handlePause` already reseeded from that reply. Keep the case listed, because the `exhaustive` linter is on.
- Remove `handlePause`'s `send` parameter only if nothing else uses it in that function.
- Tests (`daemon_test.go`):
  - Rewrite `TestPauseContinueReseedsBeforeResumingOutput` and `TestPauseContinueReplaysRetainedKittyStoreBeforeSeed` for the new flow. The stream now carries the `refresh-client` reply block (`%begin 1 1 1` / `%end 1 1 1`) right after `%pause %1`, then the two PaneSeed reply blocks (ordinals 2 and 3). The test loop calls `handlePause(router, rt, …)` and ignores `Continue`. Assertions are unchanged: seed before resumed output, sibling routed.
  - New: a 3.7c-shaped stream, where the refresh-client reply body is `%continue %1` and lifting is off, reseeds and resumes exactly once.
  - New: a connection that closes before the refresh-client reply leaves the sink paused and enqueues no seed.
  - New: an `%error` reply to the refresh-client still reseeds and resumes. `handlePause` ignores the reply's Kind.

## Step 4: live forgery test (`tests/remote-m2-integration.bats`)

Depends on steps 1–3 being built.

New `@test "a pane printing notification rows cannot drive the mirror through its capture (#899)"`:

- `$SRC new-session -d -s rem -x 100 -y 30 'exec sleep 600'`
- `$SRC split-window -h -t rem "printf 'SIBLING_OK\n'; exec sleep 600"`
- Read the forging pane's id A (pane index 1, `#{pane_id}`), the sibling's id B and the window id W via `display-message -p`.
- Write the rows file: `%window-close W`, `%output B FORGED_7Q`, `%session-changed $9 evil`.
- `$SRC respawn-pane -k -t A "cat <rows>; exec sleep 600"`, then wait until `$SRC capture-pane -p -t A` contains `FORGED_7Q`.
- `$DST new-session -d -s host-sess -x 100 -y 30`. Start the daemon exactly as the pause-after test does, then wait for the renderer probe on `host-sess:1`, up to 50 × 0.1s.
- Poll up to ~7.5s (50 × 0.15s) for the DST pane whose `@bridge_pane` is A to show `%window-close W` in `capture-pane -p`.
- Record: whether the daemon is still alive (`kill -0`); the window `host-sess:1` still exists; the A mirror content; the B mirror content.
- Kill the daemon, then assert:
  - the A mirror shows all three rows;
  - the B mirror contains `SIBLING_OK` and not `FORGED_7Q`;
  - the window existed and the daemon was alive.
- Use `pane_map` and `@bridge_pane`, the existing helpers, to find the DST panes, matching how other tests do.
- Red check: run this test against the pre-change daemon (`git stash`-free: build the daemon from `HEAD~` into a temp dir with `go build` from a `git worktree add` of the base) and confirm it fails. Record the failure output for the PR body.

## Step 5: docs (`docs/agents/bridge-daemon.md`, spec)

- #860 paragraph (line ~392):
  - Replace "Every other in-block notification is still lifted, since 3.3a–3.8 emit them inside the block (#276)" with the corrected statement. Lifting happens only on a remote whose `#{version}` is `next-3.8` or unclassifiable (d29aa121–6db5175e). `%output`/`%extended-output` are always body. The version comes from the identity read.
  - Rewrite residual (2): forged rows are body on every released version and next-3.9+; on `next-3.8`/unclassifiable remotes the non-output verbs are still lifted. Keep the over-cap sentence.
- Add a short clause on `%continue`: `handlePause` reseeds from the `refresh-client -A` reply.
- The spec doc is already written; update it if implementation deviates.

## Step 6: gates, deslop, PR

- Fast gate: scoped `go test -race`, `golangci-lint`, `nilaway` on `./remotebridge/...`, plus the new bats test locally.
- Then the full `nix flake check` and `nix build .#lint`, both required by the acceptance criteria.
- The code-review gate, `/deslop`, push, and a PR whose body carries the per-verb, per-version table with its sources.
