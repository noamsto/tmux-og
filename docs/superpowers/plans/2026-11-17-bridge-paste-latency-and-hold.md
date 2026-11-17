# Bridge image paste: latency and narrower hold (#1158)

## Measured (before) — method

Private `sshd` on loopback (fish login shell, as on the real host), one `ssh -M`
ControlMaster, real `xclip` against `Xvfb` for the clipboard, a scratch `tmux -L`
server for the local forks, and the real `bridgeProc` / `probeClipboardImage` /
`activeClient` / `paste` / `finish` code wired into a `pasteHandler` with
timestamp wrappers (uncommitted harness). Images: 20 KiB and 1 MiB random
bytes named `.png`. 6 runs each; `/tmp` on this box holds ~15.6k entries.

| phase (ms, median of 6) | 20 KiB | 1 MiB |
| --- | --- | --- |
| `@bridge_proc` gate (`list-panes` fork, on the pump) | 3.7 | 3.4 |
| clipboard TARGETS (`xclip`, on the pump) | 1.9 | 1.7 |
| client lookup (`list-clients` fork) | 3.5 | 2.8 |
| extract | 1.5 | 3.9 |
| ssh upload (one exec) | ~104 | ~99 |
| `finish`: send-keys + `image pasted` status fork, before held keys replay | 4.3 | 3.9 |
| total ctrl+v → path sent | ~120 | ~113 |

Inside the one ssh exec (shell loop, same master): a no-op `sh -c true` costs
~78 ms (the remote login shell's startup, fish here); the store script without
the janitor `find` ~85 ms; with it ~105 ms. So per paste: **one** ssh exec
(not several), ~75 ms of it remote login-shell startup the daemon cannot
shorten without a resident remote helper, ~20 ms the inline janitor sweep
(scales with the remote `/tmp` entry count), ~5 ms mktemp/cat/transfer.
Ruled out: several execs (there is one); sequential tool listing (the first
tool that answers returns, ~2 ms); ticker forks contending (upload 92-114 ms
with a 5 ms ticker forking `display-message` vs 95-121 ms standalone).

## Decisions

**Latency.** (1) Janitor sweep leaves the reply path: it runs after the
file is written and the path printed, detached from the ssh channel's fds.
(2) `list-clients` lookup runs concurrently with `extract`. (3) The `image
pasted` status fork leaves the pump goroutine, so held keys replay without
waiting for a tmux fork. Not done: a resident remote upload helper that
would remove the ~75 ms login-shell cost — new remote process lifecycle
(reconnect, orphan cleanup); filed as a follow-up. Rejected: shipping the
bytes through the control-mode stream (head-of-line blocks live pane
output; tmux arguments are not NUL-safe).

**Hold.** Two phases.
- *Reading* (swallow until the bytes are extracted, a few ms): today's rule —
  plain text free, the first non-text byte and everything after held. Keeps
  the insertion point where the user pressed ctrl+v.
- *Uploading* (path already inserted): the daemon picks the remote path itself
  (`/tmp/og-paste-<32 hex from crypto/rand>/img.<ext>`) so the path is sent
  right after extraction. Only submit-capable input and anything unknown is
  held: CR/LF, `ESC CR`, SS3 `M`, CSI-u / modifyOtherKeys Enter, paste markers,
  lone ESC, ctrl bytes outside a small editing allowlist (^A ^B ^E ^F ^K ^U ^W,
  BS, DEL, Tab), all mouse reports (a click can confirm an agent dialog), incomplete escapes, and a further ctrl+v.
  Printable/UTF-8 text, cursor and edit sequences (CSI A–F/H/Z, `n~` except
  27/200/201, focus I/O, SS3 A–D/F/H) and `ESC b/f/d/DEL` pass.
  Once anything is held, everything after it is held, in order.
- Safety: (a) no splice — the path is inserted at the pump goroutine's point
  in the input stream, before any pass-through key, so later cursor/edit keys
  act on a buffer that already holds it, like a local paste. Counterexample
  for the alternative "keep inserting after the upload but let arrows
  through": type `ab`, ctrl+v, Left, upload lands → path splices between `a`
  and `b`. (b) no submit before the file exists — every submit-capable or
  unclassified byte waits for the final result, which is posted only after
  the upload returns. (c) failure — held keys are released with a visible
  message; the inserted path is erased with backspaces when it went out whole and nothing
  passed through since insertion, else left and the message says so.
  Depends on the agent resolving the path at submit (spec: verified live).

## Handoff state machine (pump goroutine owns all of it)

Fields: `phase` (idle | reading | uploading), `inserted` (the path+space send
succeeded whole), `passed` (a frame passed through since insertion).
`pasteResult` gains `ready bool`, `path`, `cancel`, `msg` (terminal message the
pump shows) and `ok`. `done` buffers 2.

- swallow: `phase=reading`, `inserted=passed=false`; today's strict hold.
- paste goroutine: pre-extract failures (bad ext, extract error, too large)
  say their message themselves and post one final `{ok:false}` with no `ready`
  (today's behaviour, tests unchanged). After a good extract it picks the path,
  posts `{ready, path, cancel}`, uploads, `prog.stop()`s, then posts the final
  `{ok, msg}`; it never paints a post-extract terminal message itself.
- `settle(pane, r, deliver)` (replaces `finish`+`replay` at both `pumpInput` call
  sites): `ready` → take `h.held`, clear it, `inserted = sendChunks(path+" ")`
  with a silent say (the ~60-byte path is one send-keys chunk, so an insert is
  whole or absent); `phase=uploading`; a refused send sets `h.insertRefused` and
  calls `cancel()` (the upload is pointless); replay the taken frames through
  the uploading classifier. Final → `phase=idle`, take `h.held`, then in this
  order, all synchronous on the pump except the display: (1) failure and
  `inserted && !passed` → send `0x7f` × (len(path)+1) via `sendChunks`
  (synchronous, so it precedes any replayed Enter on the same writer);
  (2) display, via `go`: `insertRefused` → "input to the mirror pane was
  dropped (bridge reconnecting?)" whatever the upload result; else success and
  `inserted` → `pasteDoneMsg` (keeps the `client != ""` guard); else failure →
  `msg` (+ " (path left in the prompt)" when `inserted && passed`); (3) replay
  the held frames through the idle `handle`. The paste goroutine maps no error
  to "dropped": `context.Canceled` from the upload keeps its own text.
- `replay` compares `h.pumping` before/after each `deliver` against its value at
  entry, so it stops only when a frame *started* a paste, or (uploading) when
  something was held; `h.held` non-empty ⇒ hold the remaining frames in order.
- `handle` while uploading: `holdAll` ⇒ everything held; else `submitHold`
  splits, pass-through sets `passed`, the rest is held. Passed frames are never
  scanned by `splitPasteDrops`; that is sound because paste markers, lone ESC,
  incomplete escapes and ^V are held and every passed sequence ends on a byte
  that leaves `matched` at 0 (comment + test).
- close while uploading: `pumpInput` loops `settle(<-results())` until
  `!pumping` (a pending ready or the final); the return can wait up to
  `pasteTimeout`. (comment at the existing call site)

## File list

- `picker/remotebridge/daemon/paste.go` — path chosen by the daemon, two-phase hold, ready/final results, `settle`, concurrent client lookup (the first progress frame waits for the lookup, extract does not), async done-status; security comments updated (no longer mktemp).
- `picker/remotebridge/daemon/pastekeys.go` (new) — `submitHold(payload)` classifier: index of the first byte that must wait.
- `picker/remotebridge/daemon/pastekeys_test.go` (new) — table test for the classifier.
- `picker/remotebridge/daemon/daemon.go` — `Config.PasteUpload` signature (`path`, no returned path); `pumpInput` uses `settle` and drains until final on close.
- `picker/remotebridge/cmd/daemon/main.go` — store script reads the path from the first stdin line (not argv, which other accounts can see in `ps`), `mkdir` instead of `mktemp -d`, detached sweep after the reply; closure, `pasteUploadArgs` (no ext arg), comments at the script.
- `picker/remotebridge/cmd/daemon/main_test.go` — argv test, script-shape test, and a run of the script against a scratch dir (sweep removes an old dir, file lands, stdout closes before the sweep ends).
- `picker/remotebridge/daemon/paste_test.go`, `paste_progress_test.go`, `routedexec_test.go` — fixture wiring for the new upload signature and ready/final results; new hold and single-exec cases; the tests listed in Step 3 changed.
- `picker/remotebridge/daemon/pumpinput_test.go` — the production-path pumpInput tests (see the addendum).
- `picker/remotebridge/daemon/zz_bench_test.go` — the uncommitted measurement harness: gets `//go:build pastebench` now, is ported in Step 4, run with `-tags pastebench`, never staged, removed with `gtrash put` after Step 6.
- `docs/agents/bridge-graphics-paste.md` — hold, path choice, janitor, measured numbers.
- `docs/agents/bridge-daemon.md` — only if it names the paste upload (check with `rg`).

## Steps

- [ ] **Step 0: harness out of the default build.** Prepend `//go:build pastebench` to `zz_bench_test.go`. `go vet ./remotebridge/daemon` still builds.
- [ ] **Step 1: failing classifier test.** `pastekeys_test.go` table: CR, LF, `ESC CR`, `ESC[13u`, `ESC[13;2u`, `ESC[109;5u` (ctrl+m), every CSI-u, every `ESC[27;…~`, `ESC OM`, `ESC[200~`/`201~`, lone ESC, incomplete `ESC[1`, `ESC` + printable other than b/f/d, all mouse (SGR and X10), ^V, ^C held; ^A ^B ^E ^F ^K ^U ^W BS DEL Tab, arrows, `ESC[3~`, `ESC[1;5C`, `ESC[I`/`O`, `ESC b/f/d/DEL`, SS3 A–D/F/H, UTF-8, text-then-CR splits at the CR pass/hold as specified. Run: `cd picker && go test ./remotebridge/daemon -run TestSubmitHold` → fails to build.
- [ ] **Step 2: classifier.** `pastekeys.go`: `submitHold(payload []byte) int` (len(payload) when nothing must wait). Same run → green.
- [ ] **Step 3: tests first.** Helper contract: the fixture's `deliver` records the frames it is given; `awaitOutcome` plays the pump (calls `settle` on each result, ready then final) and returns the first observable (send-keys or notify); `f.held` is what the *final* released; `finishPaste` drains to the final. Existing tests that change (each named in the PR body with why):
  - `TestPasteProgressStartsBeforeUploadFinishesAndClearsOnSuccess`: path send-keys arrives before the upload is released; `image pasted` only after final.
  - `TestPasteFailuresNotifyAndSendNothing`: pre-extract cases unchanged; "upload error" → path sent, `0x7f`×(len+1), then the failure message (driven through `finishPaste`); "bad reply path" removed, replaced by a case asserting the daemon-chosen path matches `pastePathRe` with a whitelisted ext.
  - `TestHeldInputIsReleasedOnFailure`, `TestHeldFramesKeepTheirBoundaries` (calls `finish` today), `TestBytesTypedBeforeCtrlVAreNeverHeld`, `TestTypingDuringUploadIsNotHeld`, `TestHeldCtrlVIsReinterceptedOnRelease` (settle's replay now does the re-interception through `deliver`): re-pointed at ready/final, reading `f.held` after the final.
  - `TestSubmissionAndCursorKeysAreHeldBehindThePath`: rewritten — Enter held with the bytes after it, arrows/backspace/tab pass during upload, a cursor key typed during *reading* is held until ready.
  - `TestPasteWithoutClientFallsBackToNotify`, `TestPasteFailureFallsBackToNotifyWhenClientDetached`, `TestPasteProgressShowsOnThePastingClientOnly`, `TestHandleNotifiesOnRefusedSend`: drive the final through `finishPaste` because the upload-failure and dropped messages now come from `settle`.
  - `TestPasteProgressReplacedOnFailureCancelAndRefusedSend`: refused insert → upload cancelled, "dropped" is the last status, no spinner frame after it, never `image pasted`; the plain-cancel case still shows "context canceled".
  Any other existing test the new contract breaks is fixed the same way and added to the PR-body list. New: ready send precedes upload return; failure after pass-through keeps the path and says so; the erase precedes a replayed Enter; arrow passes, a held bracketed paste replays at final, a later ^V is still intercepted; `holdAll` holds in the uploading phase; close-while-reading drains ready then final. Run `go test ./remotebridge/daemon -run 'Paste|Hold|Held|Typing|Submission'` → red against the current code.
- [ ] **Step 4: handler change** (`paste.go`, `daemon.go`) per the state machine, plus the `PasteUpload` closure signature in `cmd/daemon/main.go` (path in, error out; script body unchanged until Step 5) so `./remotebridge/...` compiles; port `zz_bench_test.go` (ready send-keys and final timed as separate phases). Same run plus `go test -race ./remotebridge/...` → green. `implement: opus` (concurrency on the pump/paste handoff).
- [ ] **Step 5: store script + upload closure** (`main.go`) and tests (`main_test.go`). Script: `umask 077; IFS= read -r p || exit 1; case $p in *..*) exit 1;; /tmp/og-paste-*/img.*) ;; *) exit 1;; esac; d=${p%/*}; mkdir "$d" || exit 1; cat > "$p" || { rm -rf "$d"; exit 1; }; printf "%s" "$p"`, then the sweep as a detached `( find … ) </dev/null >/dev/null 2>&1 &`. Closure feeds `path+"\n"` then the bytes on stdin. Tests: argv shape (no path or ext in argv, and the script has no single quote or backslash, which fish treats specially); script text has `mkdir`, no `mktemp`, the sweep after the `printf`; a real `sh` run with `/tmp` rewritten to `t.TempDir()` in the script text (no touching the real `/tmp`) prints the path, writes the bytes, refuses an existing dir, a path outside the pattern and one containing `..`, and — with a `sleep` prepended to the sweep in the test copy — `Output()` returns well before the sweep ends while a pre-aged sibling (`touch -d`) is removed afterwards. `go test ./remotebridge/cmd/daemon` → green.
- [ ] **Step 6: re-measure** with the uncommitted harness (same method, store script and handler as shipped) → "after" table for both sizes.
- [ ] **Step 7: docs** — update `bridge-graphics-paste.md` (hold, path choice, janitor, measured ~75 ms login-shell cost and the resident-helper follow-up, remove text saying the path lands only after the file exists); `rg -n 'mktemp|paste' docs/agents/bridge-daemon.md`.
- [ ] **Step 8: gates** — `gtrash put` the harness, commit (harness never staged), `nix build .#lint --accept-flake-config`, `nix flake check --no-build`, `nix flake check`, `nix build .#default`; golangci-lint/nilaway run through `nix flake check`.

## Evidence (shared contract: `PasteUpload` and the store script)

Producer `paste()` → `Config.PasteUpload` (daemon.go) → closure in `cmd/daemon/main.go` → `pasteUploadArgs`/`remoteStoreScript` → remote `sh`. Consumers of the returned path: `finish`/`sendChunks` only (changed: path now sent at `ready`); `pastePathRe` validation (kept, now applied to the locally chosen path); `routedexec_test.go:393` (fixture, updated); `paster()` wiring (unchanged). No other reader of `/tmp/og-paste-*` (`rg og-paste`). Behavioral change regression: new tests in Step 3 run red against the current code (assert failures, once the fixture compiles against the old signature in a scratch copy) and green after.

## Addendum after the revision cap (critic round 3, verified against the tree)

- `pumpinput_test.go` has two old-signature upload fixtures (`TestPumpInputPasteKeepsPaneResponsive`, `TestPumpInputConnCloseMidUploadStillInjects`). Port both to `func(ctx, path string, data []byte) error`. Rewrite the first deterministically: wait for the ready path send-keys before writing "hi"; assert "hi" is sent while the upload is blocked; assert Enter is held until `close(release)`; failure variant A (text passed): message contains "path left in the prompt", no `0x7f`, then the held Enter; variant B (nothing passed): the `0x7f` erase reaches `pumpInput`'s writer before the held Enter. The close test still sees path then Enter, plus a "close while reading drains ready then final" case. These are the production-path regressions for the ready→final ordering.
- `settle` skips the failure display when `msg` is empty (pre-extract failures already showed theirs). `newPasteFixture` buffers `done` at 2. Step 6 runs after Step 5 (the closure passes the path in at Step 4 but the script only honours it from Step 5). Script test uses `os.Chtimes` and a Go-side delay rather than `touch -d`/`sleep` where it can.

## Acceptance

- [ ] PR body per-phase timings before/after, both sizes, method, dominant cause, hold decision with safety argument → Step 6 + the Decisions above.
- [ ] Go tests pin hold behaviour and single-exec upload → Steps 1–5; existing cases green except the rewritten wider-hold case and fixture wiring.
- [ ] `nix build .#lint --accept-flake-config`, `nix flake check --no-build`, `nix flake check`, `nix build .#default` → Step 8.
- [ ] CI build job on x86_64-linux and aarch64-darwin → cite run URL after push (darwin capacity cancel → report blocked).
