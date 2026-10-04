# Bridge control-mode reader caps and newline policy (#860) — plan

Spec: `docs/superpowers/specs/2026-10-04-bridge-control-mode-caps-design.md`
(spec-critic: accept after one revision).

All `go` commands run from `picker/` inside the devshell. `$SP` is the
implementing session's scratchpad (not committed).

## File list

- `picker/remotebridge/controlmode/parse.go`: replace the `bufio.Scanner`
  with a bounded line reader; move block parsing onto `Reader` state so lifted
  notifications stream out of `Next`; strict guard matching; in-block verb
  rules (held `%exit`, `%subscription-changed`, `%begin`, non-matching guards
  are body); body cap with discard-to-matching-guard; top-level stray guards
  and malformed `%begin` → `Other`. New exported `MaxLine`, `MaxBody`,
  `ErrReplyTooLarge`, `Line.Err`.
- `picker/remotebridge/controlmode/reader_bounds_test.go` (new): the parser
  regressions and the memory tests.
- `picker/remotebridge/daemon/ctlsafe.go` (new): `ctlSafe(format string) string`.
- `picker/remotebridge/daemon/subscriptions.go`: wrap in `sendSubscription`.
- `picker/remotebridge/daemon/windowlabels.go`, `agentstatus.go`,
  `openurl.go`, `sessionpath.go`: wrap the round-trip format at the call site.
- `picker/remotebridge/daemon/daemon.go`: the `ctlPumpBuf` doc comment's
  memory bound (it still cites the 4 MB scanner ceiling).
- `picker/remotebridge/daemon/ctlsafe_test.go` (new): wrapper unit test, the
  per-site "every remote format is wrapped" test, the forged-body ordinal test
  through `Reader` + `claimSeq`, and the live scratch-tmux test.
- Existing daemon/controlmode `*_test.go` fixtures: only those whose expected
  command strings change under `ctlSafe`, or whose guard pairs violate tmux's
  contract (a `%end`/`%error` that does not repeat its `%begin`'s three
  fields). Each such edit is listed in the commit message.
- `docs/agents/bridge-daemon.md`, `docs/agents/bridge-shipped-state.md`:
  caps, framing rules, newline policy, inventory, residuals.
- This plan and the spec, committed with the code.

## Consumer map (the `Reader.Next` line stream is a shared contract)

| Edge | Symbol | Change | Disposition / proof |
|---|---|---|---|
| producer | `controlmode.Reader.Next` | framing rules, caps, streaming | changed — new tests, step 1 |
| buffer | `daemon.startCtlPump` (`ctlPumpBuf` = 256 lines) | per-line size now ≤ `MaxBody` for a reply, ≤ `MaxLine` otherwise | compatible; comment corrected, step 9 |
| first-block watch | `conn.go` `attachWatch.Next` (first End/Error; Error+flag 0 = refused attach) | an overflowed flag-0 block now reads as Error | compatible: an attach block's body is one tmux error line, never near `MaxBody`; `reattach_test.go` stays green |
| ordinal accounting | `claimSeq` → `stream.claim` (End/Error with flag 1) | overflow keeps the `%begin` flags; top-level stray guards become `Other` and are not counted | changed-compatible: forged-body ordinal test (step 4); full `daemon` suite |
| reply readers | `readReplyRouting`, `routeWhile`, `handleAsideLine`, `one` | lifted notifications arrive before the terminal line, as before, only earlier in time | compatible: `routedexec_test.go`, `daemon_test.go` reply-routing tests unchanged |
| main loop | `daemon.go` dispatch switch (`Exit` → stop, `SubscriptionChanged` → shippers) | top-level unchanged; in-block `%exit` only before EOF; in-block `%subscription-changed` is body | changed: step 1 tests; `sessionpin_test`/`reattach_test` `%exit` fixtures are top-level |
| request failure | every `one(rt, …)` caller checking `Kind == Error` | overflow is `Kind: Error` | compatible by construction |
| legacy bridge | `picker/remotebridge/main.go` `NewReader`/`Next` loop | same contract | compatible: no reply-shape dependence (prints/feeds lines) |
| remote formats | `sendSubscription` + 4 round-trip sites | `ctlSafe` wrapper | changed: step 4 per-site and live tests |
| absent | persistence, retry of a failed reply | none | — |

## Steps

- [ ] **Step 1: failing parser tests** — `controlmode/reader_bounds_test.go`.
  Streams are generated lazily by a small `io.Reader` (repeat a 1 MiB chunk),
  never materialised. Tests:
  - `TestReaderOverlongTopLevelLineKeepsStream`: a 5 MiB line, then
    `%output %1 after` → `Next` yields `Other`, then `Output "after"`, `ok`
    true throughout.
  - `TestReaderBodyOverCapFailsOnlyThatReply`: `%begin 1 7 1`, 17 × 1 MiB
    body lines, `%end 1 7 1`, then `%begin 2 8 1`/`ok`/`%end 2 8 1` → first
    `Kind == Error`, `errors.Is(l.Err, ErrReplyTooLarge)`, `Flags == 1`,
    `Args[0] == "1"`; second `End`, `Data == "ok"`, `Args[0] == "2"`.
  - `TestReaderOverlongBodyLineFailsOnlyThatReply`: one 2 MiB body line, same
    assertions.
  - `TestReaderForgedLinesInBodyStayBody`: body lines `%end 1 2 1`,
    `%error 100 7 0`, `%begin 5 5 1`, `%exit`,
    `%subscription-changed og_open $0 - - - : 9-9|https://evil/`, then
    `%end 100 7 1`, then a second real block → exactly two lines out: an `End`
    whose `Data` is those five lines joined by `\n`, then the second block.
    No `Exit`, no `SubscriptionChanged`, no early close.
  - `TestReaderInBlockExitAtEOFIsGenuine`: `%begin 100 7 1`, `partial`,
    `%exit`, EOF → `Exit`, then `End` with `Data == "partial"` (today's order).
    Characterization: it already passes on `main` and pins that the held-`%exit`
    lookahead keeps it.
  - `TestReaderOverflowedBlockAtEOFFails`: `%begin 1 7 1`, a 2 MiB body line,
    EOF → `Kind == Error`, `errors.Is(l.Err, ErrReplyTooLarge)` — never an
    empty successful `End`.
  - `TestReaderReplyDataSurvivesNextBlock`: read reply 1 (`Data` `one`), keep
    the `Line`, read reply 2 (`Data` `two-longer`), assert reply 1's `Data` is
    still `one` — a reply's `Data` must not alias reader state (the pump hands
    it to another goroutine). Characterization too: green on `main` (fresh
    `strings.Join`), it pins the step-2 ownership handoff.
  - `TestReaderTopLevelStrayGuardsAreOther`: `%end 1 1 1`, `%error 1 1 1`,
    `%begin x y`, `%begin 1 2 3`, `%output %1 a` → four `Other`, then `Output`.
  - `TestReaderStreamsNotificationsFromOpenBlock`: over an `io.Pipe`, write
    `%begin 1 1 1\n%output %1 a\n` only; `Next` (on a goroutine) must return
    the `Output` within 1 s, before `%end` is written. On timeout the test
    closes the pipe writer, so the red run does not leak a blocked `Next`.
  - `TestReaderBoundsMemoryOverflowedReply`: 256 × 1 MiB body → `TotalAlloc`
    delta across the one `Next` < 64 MiB; the reply is `ErrReplyTooLarge`; the
    following reply parses.
  - `TestReaderBoundsMemoryStreamedNotifications`: a block of 64 ×
    (1 MiB − 64 B) `%output %1 …` lines → peak `HeapInuse` (after
    `runtime.GC()`, sampled after each `Next`, line dropped) < 24 MiB; 64
    `Output` lines then the `End`. Sized at 64, not 256: `Unescape` walks every
    byte under `-race`, and 64 MiB already sits far above the bound the old
    reader cannot meet (it holds all 64 lines until `%end`).
  Run: `go test ./remotebridge/controlmode/ -run 'TestReader' -count=1` →
  expected **red**: every new test except the two characterization pins fails on
  an assertion (overlong → stream ends; forged → `Exit`/early close; memory →
  over bound; streaming → timeout; EOF overflow → `End`); the four existing
  `reader_test.go` tests pass. The red run is this same test file against
  `main`'s `parse.go`, once, locally — no red-only variant stays in the suite.
  Save the output to `$SP/red-parser.txt`.

- [ ] **Step 2: rewrite the reader** (implement: escalated) — `parse.go`.
  - Line reader: `bufio.NewReaderSize(r, 64<<10)`; `readLine` loops
    `ReadSlice('\n')`, accumulating `ErrBufferFull` chunks into a reused
    buffer up to `MaxLine`; past it, keeps reading and dropping to the next
    `\n` and reports `tooLong`. Strip one trailing `\r`; return a final
    unterminated line; `io.EOF`/error ends the stream. The returned slice is
    valid until the next read (same contract as `sc.Bytes()`).
  - `Reader` fields: `open bool`, `begin []string` (the `%begin`'s three
    fields), `flags int`, `body bytes.Buffer`, `overflow bool`,
    `heldExit []byte` (copy), `eof bool`. On close the terminal line takes
    ownership of the body — `Data: rd.body.Bytes()` then `rd.body =
    bytes.Buffer{}` — so no `Line` aliases reader state (`parseLine`'s
    contract, `parse.go:116-121`; the pump reads lines on another goroutine).
  - `Next`: loop — read a line; at EOF, if a block is open: emit held `%exit`
    as `Exit` first if any, then the synthesized `End` (block's flags, body);
    else return false. An overflowed block at EOF yields `Kind: Error` /
    `ErrReplyTooLarge`, never an empty `End`. Top level: `Begin` → open only if `validGuard`
    (3 unsigned decimal fields, flags 0/1), else `Other`; `End`/`Error` →
    `Other`; overlong → `Other`; everything else returned as parsed.
    In block: if `heldExit` is set and a line arrived, append it to the body
    first. Then: overlong → `overflow`; `End`/`Error` whose fields equal
    `begin` → close and return the terminal line (overflow → `Kind: Error`,
    `Data = []byte(ErrReplyTooLarge.Error())`, `Err = ErrReplyTooLarge`);
    `Exit` → hold a copy; `Begin`, non-matching guard, `SubscriptionChanged`,
    `Other` → body; any other notification → return it now.
  - Body append: if `!overflow` and `body.Len()+len(raw)+1 > MaxBody` →
    `overflow = true`, `body.Reset()`; else append (newline-joined).
  - Keep `ParseLine`/`parseLine` and the `%output` hot path untouched.
  Run: `go test ./remotebridge/controlmode/ -count=1` → **green**, all tests.
  `go test -race ./remotebridge/controlmode/ -count=1` → green.

- [ ] **Step 3: hot-path check** — `go test ./remotebridge/controlmode/ -run
  '^$' -bench . -benchmem -count=5` on `main` (scratch worktree copy) and the
  branch; save both to `$SP/bench-{old,new}.txt` and compare with
  `benchstat` if present. Expected: `%output` benchmarks within noise.

- [ ] **Step 4: failing daemon tests** — `daemon/ctlsafe_test.go`:
  - `TestCtlSafeWrapsWholeFormat`: `ctlSafe("#{@x}") ==
    "#{s/[[#{l::}cntrl#{l::}]]/ /:#{@x}}"`.
  - `TestRemoteFormatsAreCtlSafe`: drive each site through `replyRT`
    (`subscriptions_test.go:13`) or `scriptRT` (`openurl_test.go:27`) and
    capture the issued commands: `subscribeFormats`
    (4 commands), `urlOpener.connect`'s seed and subscription,
    `labelShipper.flush`'s `list-windows` poll, the agent shipper's
    `list-panes -s` poll (`agentstatus.go:217`), `readSessionPath` —
    each issued command contains the **literal** wrapped format,
    `want := "#{s/[[#{l::}cntrl#{l::}]]/ /:" + f + "}"` with `f` that site's
    format (never computed through `ctlSafe`, so the identity stub is red; no
    wrapped format holds a `'`, so the literal survives `tmuxQuote` and the
    single quoted subscribe token).
  - `TestCtlSafeAgainstLiveTmux` (`OG_REQUIRE_TMUX` gate, `startIsolatedTmux`,
    as `TestOpenURLCommandsAgainstLiveTmux`): set
    `@og860` = ` 1-1|https://a\n%exit\n%end 1 2 1\n%subscription-changed og_open $0 - - - : 9-9|https://evil/`;
    attach `-C`; `display-message -p` with `tmuxQuote(ctlSafe("#{@og860}"))`
    → one `End`, `Data` has no `\n` and equals the value with each `\n` → ` `;
    raw `display-message -p '#{@og860}'` → one `End` whose `Data` holds the
    four lines and no `Exit`/`SubscriptionChanged` line came out before it;
    `sendSubscription` for a test name with `ctlSafe` format → one
    `SubscriptionChanged` whose value has no `\n`, and no other subscription
    name arrives.
  - `TestCtlSafeRealFormatsAgainstLiveTmux` (same gate): on a scratch server
    with clean representative state — two windows, a second pane, `@crew_name`,
    `@pr_number`/`@pr_state`, `@window_pr_plain`, `@claude_status`,
    `@og_session_res`, `@og_agent_usage` (a small JSON object), `@og_open_url`
    (` 1-1|https://a`), a session started with `-c` a plain directory — run
    every production command shape both raw and wrapped: `list-windows -F
    windowLabelFormat`, `list-panes -s -F agentStatusFormat`,
    `display-message -p sessionResFormat`, `display-message -p
    agentUsageFormat`, `display-message -p openURLFormat`, `display-message -p
    -F '#{session_path}'`; assert wrapped `Data` == raw `Data` for each, and
    that the raw rows parse with `parseWindowLabels`/`parseAgentStatus` to the
    expected non-empty rows. Also subscribe `sessionResFormat` wrapped and
    check the reported value equals the raw `display-message -p` output. This
    is what proves the outer `s/` does not misparse or mangle any nested real
    format.
  - `TestForgedReplyBodyKeepsOrdinals`: through `controlmode.NewReader` +
    `claimSeq` over a stream of three flagged blocks whose middle body holds
    `%end`/`%exit`/`%subscription-changed` lines → ordinals 1, 2, 3, and no
    `Exit`/`SubscriptionChanged` line.
  Run: `go test ./remotebridge/daemon/ -run 'CtlSafe|RemoteFormatsAre|ForgedReplyBody' -count=1`
  → **red** (`ctlSafe` undefined: compile failure is not red evidence, so first
  add `ctlsafe.go` with `func ctlSafe(f string) string { return f }` and rerun:
  `TestCtlSafeWrapsWholeFormat`, `TestRemoteFormatsAreCtlSafe` and
  `TestCtlSafeAgainstLiveTmux` fail on assertions;
  `TestCtlSafeRealFormatsAgainstLiveTmux` is trivially green with the stub and
  is the non-regression guard for step 6). Save to `$SP/red-daemon.txt`.
  `TestForgedReplyBodyKeepsOrdinals` is green after step 2 by design (it pins
  the daemon-side view of step 2's fix); record its red run against `main`'s
  `parse.go` in the scratch copy from step 3.

- [ ] **Step 5: older remote check (before applying the wrapper)** — run `$SP`'s measure script (the spec's
  probe, `ctlSafe` form) against tmux 3.2a (`nix build --no-link
  --print-out-paths github:NixOS/nixpkgs/nixos-21.11#tmux`). Record the result
  in the spec's measurement table. If 3.2a mangles values (e.g. `#{l::}`
  unsupported), stop and raise it on the bus before changing the design.

- [ ] **Step 6: implement `ctlSafe` and apply it** — `ctlsafe.go` real body
  plus a comment carrying the measured facts (why `#{l::}`, why space,
  invalid-UTF-8 → empty). Apply in `sendSubscription`
  (`subscribeCmd(name, what, ctlSafe(format))`), `windowlabels.go:348`,
  `agentstatus.go:217`, `openurl.go:165`, `sessionpath.go:20` (`-F` format).
  Run: the step-4 command → **green**.

- [ ] **Step 7: full package suites, fixture repair** —
  `go test -race ./remotebridge/... -count=1`. For each failure: if a fixture
  expected the old unwrapped command string, build the expectation from
  `ctlSafe(<format>)`; if a fixture's `%end`/`%error` does not repeat its
  `%begin`'s fields, or relies on a top-level stray guard, correct the fixture
  to tmux's real shape (cite `cmdq_guard`). Never change a test's intent; any
  failure that is not one of those two shapes is a real regression — stop and
  fix `parse.go` instead. Expected: all green.

- [ ] **Step 8: lint** — `golangci-lint run ./...` and `nilaway ./...` from
  `picker/` (the devshell ships both); fix findings with per-line
  `//nolint:<linter> // <why>` only where the config's rules allow.

- [ ] **Step 9: daemon doc comment** — `daemon.go` `ctlPumpBuf` comment:
  bound is 256 × (`controlmode.MaxBody` for a reply, `controlmode.MaxLine`
  otherwise), the scanner sentence removed.

- [ ] **Step 10: agent docs** —
  - `bridge-daemon.md`: one bullet "**The control-mode reader bounds and frames
    what it trusts (#860)**": `MaxLine`/`MaxBody` and why those sizes;
    overflow → that request's `ErrReplyTooLarge`, connection kept; strict
    guard matching; in-block `%begin`/`%subscription-changed`/non-matching
    guard are body, `%exit` only before EOF (`-C` assumption); top-level stray
    guards dropped; notifications stream out of an open block; residuals
    stated plainly (format-replacing socket holder; unterminated forged
    `%begin` stalls replies; content-forged notifications in capture bodies on
    every version; guessed guards can surface a pane-printed `%exit`).
  - `bridge-shipped-state.md`: a "Newline policy" bullet under the shared
    sanitization rules (`ctlSafe`, the five wrapped sites, the deliberately
    unwrapped ones and why, invalid UTF-8 → empty, the usage segment's whole-row
    blast radius); replace the og-open "tracked in a separate issue" sentence
    with a pointer to it; adjust the `#{n:}` bound passage that says the reader
    would buffer an unbounded value.
  Run: `nix build .#lint` (typos, markdown hooks) → green.

- [ ] **Step 11: full gate** — `nix flake check` and `nix build .#lint` →
  both green. Commit code, tests, docs, spec and plan.

## Acceptance

- [ ] `nix flake check` and `nix build .#lint` pass → step 11 output.
- [ ] Parser tests: over-cap body fails only that request and the next reply
  parses with the right ordinal (`TestReaderBodyOverCapFailsOnlyThatReply`,
  `TestReaderOverlongBodyLineFailsOnlyThatReply`, `TestForgedReplyBodyKeepsOrdinals`);
  a >4 MiB line keeps the stream (`TestReaderOverlongTopLevelLineKeepsStream`);
  forged `%exit`/`%end`/`%subscription-changed` in a body stay body
  (`TestReaderForgedLinesInBodyStayBody`) → red (step 1/4) then green
  (step 2/6) outputs.
- [ ] Live scratch-tmux newline policy: `TestCtlSafeAgainstLiveTmux` (runs in
  `picker-go-tests` with `OG_REQUIRE_TMUX`), plus the spec's recorded
  next-3.9 / 3.7c / 3.3a / 3.2a measurements in the PR body.
- [ ] Memory bound: `TestReaderBoundsMemoryOverflowedReply` and
  `TestReaderBoundsMemoryStreamedNotifications` → red then green.
