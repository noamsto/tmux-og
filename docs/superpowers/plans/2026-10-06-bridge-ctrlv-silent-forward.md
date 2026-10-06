# Image ctrl+v silently forwarded in a mirror pane (#938)

Design spec: `docs/superpowers/specs/2026-10-06-bridge-ctrlv-silent-forward-design.md`
(hypothesis evidence H1–H4, requirements R1–R7).

## Violated invariant

A ctrl+v outside a bracketed paste on an agent mirror pane with an image on
the local clipboard is intercepted, and **any** pass-through of it on a
pane that might be an agent explains itself. H1 breaks the first half:
`splitPasteDrops` matches a paste marker only within one payload. A marker
split across two `handle` calls leaves `inPaste` stuck, and every later
gesture on that pane is forwarded silently.

## Files

- `picker/remotebridge/daemon/paste.go`: matcher, handle, probe, bridgeProc
- `picker/remotebridge/daemon/paste_test.go`: regressions and new cases
- `picker/remotebridge/daemon/pumpinput_test.go`: one production-chain
  regression (pumpInput → handle)
- `picker/remotebridge/daemon/routedexec_test.go`: `TestPasterUsesPlainHooks`
  calls `h.procFor`, so it follows the signature change
- `docs/agents/bridge-graphics-paste.md`: "Bridge Image Paste"
- `docs/agents/bridge-daemon.md`: the image-paste requirements row
- `docs/superpowers/specs/2026-10-06-bridge-ctrlv-silent-forward-design.md`
  and this plan

## Steps

1. **Red first.** Add the H1 regressions before changing `paste.go`, and run
   them against the untouched code. Record the failing assertions.
   - `TestHandleMarkerSplitAcrossFrames` (paste_test.go), a table over both
     markers and every split offset 1..5:
     - End marker split: `handle("\x1b[200~text"+end[:k])`, then
       `handle(end[k:])`, then `handle("\x16")`. The last call must swallow
       the byte (return empty), and the fixture must see the path injection.
     - Begin marker split: `handle("x"+begin[:k])`, then
       `handle(begin[k:]+"a\x16b")`. The second call must return its input
       unchanged, keeping the 0x16 as content, with no paste. Then close
       with `handle(end)`, and `handle("\x16")` must swallow.
     - A carried partial, a mismatch, then a full marker:
       `"\x1b[200~t\x1b["`, `"\x1b[201~"`, `"\x16"` → swallowed. This is a
       matcher-restart guard, green on main.
   - `TestPumpInputPasteMarkerSplitByCarryFlush` (pumpinput_test.go) uses
     real `pumpInput` with a `newPasteFixture()` handler and
     `escCarryGrace = 20ms`. It writes frame `"\x1b[200~x\x1b[20"`, sleeps
     past the grace so the carry is flushed alone, then writes `"1~"`, then
     `"\x16"`. It asserts that no `send` carries the forwarded `16` byte and
     that the fixture sees the injection. Deterministic: frame 2 doesn't
     exist yet when the timer fires.
2. **R1: streaming marker matcher.** Add `matched int` to `pasteHandler`
   (the bytes of the awaited marker already matched at the end of the last
   payload). Rewrite `splitPasteDrops` as a per-byte loop. The awaited
   marker is `bracketedPasteEnd` if `inPaste`, else `bracketedPasteBegin`.
   - On `c == marker[matched]`, advance. When the marker completes, toggle
     `inPaste` and reset `matched`.
   - Else, if `c == 0x1b`, set `matched = 1`, else 0. ESC appears only at
     marker index 0, so this restart is exact.
   - A 0x16 outside the paste counts as a drop. Every other byte is kept.
     Bytes are never held back.
   Update the struct and function doc comments, dropping "the safe
   direction to fail in". Keep the package-level marker vars.
3. **R2/R5 gate.** Change `procFor` to `func(string) (string, error)`.
   `bridgeProc` returns an error for each of these: the `LocalTmuxOut`
   error (`mirror pane lookup failed: %w`), the timeout (`mirror pane
   lookup timed out after 2s`), the pane missing from the list, and an
   empty `@bridge_proc` (`… has no @bridge_proc yet`). In `handle`, an error
   forwards `payload` and runs `go h.notify(pasteForwardedMsg + err.Error())`,
   where `const pasteForwardedMsg = "tmux-og: ctrl+v forwarded to the
   remote: "`. Tests match the stable substring `ctrl+v forwarded` plus the
   reason. A non-agent proc still forwards silently.
4. **R2/R4/R5 probe.** Change the `probeClipboard` contract: `ok=false,
   err=nil` means a listing succeeded with no image (silent forward), and
   `err != nil` means we couldn't tell or the image isn't usable (forward +
   async notify). `handle`'s `err != nil` branch now forwards `payload`
   instead of swallowing it. In `probeClipboardImage`:
   - Collect per-tool listing failures with `listFailure(name, err,
     timedOut)`: the first trimmed `ExitError.Stderr` line, else `err`, else
     "timed out after 2s".
   - Remember whether any listing succeeded, and remember the first
     unsupported `image/…` target. The new `unsupportedImageTarget`
     validates it against `^image/[A-Za-z0-9.+-]+$` before it is used in a
     message.
   - Result precedence: a supported image (returned immediately), then an
     unsupported image (err), then any successful listing (silent), then
     the joined failures (err), then "no clipboard tool (xclip or wl-paste)
     on PATH" (err).
5. **Tests for R2/R3/R5.**
   - Rewrite `TestHandleClipboardProbeErrorNotifies` to assert forward plus
     notify.
   - Add `TestHandleGateLookupErrorNotifies`, which asserts forward plus
     notify carrying the reason.
   - `TestHandleForwardsWhenNoImage` and `TestHandleForwardsOnNonAgentPane`
     additionally assert no notify within a short wait.
   - Add one notify for a `"\x16\x16"` frame whose probe errors.
   - Add `TestProbeClipboardImage`, which uses fake `xclip`/`wl-paste`
     `#!/bin/sh` stubs and `t.Setenv("PATH", dir)`, a table covering: no
     tools; wl-paste png → ok, ext png; text only → silent; `image/tiff` →
     error naming it; wl-paste exits 1 with stderr "Nothing is copied" →
     error containing `wl-paste: Nothing is copied`; xclip fails and
     wl-paste lists text → silent; xclip text and wl-paste png → ok.
   - Add `TestBridgeProc` with a fake `LocalTmuxOut`: error, missing pane,
     empty proc, found proc.
   - Update every fixture `procFor` stub to the new signature. In
     `routedexec_test.go`'s `TestPasterUsesPlainHooks`, use `got, err :=
     h.procFor("%1")`, assert `err == nil` and `got == "claude"`, and keep
     the `plainCalls == 1` check.
6. **Docs (R7).** In `bridge-graphics-paste.md`:
   - The "Gated and conservative" bullet gains the notify-and-forward line.
   - A new bullet covers cross-frame marker matching: never holding bytes,
     and why (the carry flush).
   - "Failures are visible" now covers pass-through as well.
   - In `bridge-daemon.md`'s image-paste requirements row: without a tool
     the byte is forwarded **with a notify**.
   Commit the design spec next to this plan. The scratch H1 probe
   (`zz_probe_h1_test.go`) was already trashed; step 1's pumpInput
   regression replaces it.
7. **Gate.** Run `go test -race ./remotebridge/daemon/` (from `picker/`),
   `nix build .#default`, `nix flake check` and `nix build .#lint`.

## Validation

- The step 1 tests are red on the untouched `paste.go`, with the failing
  assertions recorded, and green after step 2.
- The step 5 tests are green. Every existing paste/pumpInput test stays
  green, apart from the one deliberately rewritten in step 5
  (`TestHandleClipboardProbeErrorNotifies`: swallow → forward).
- The full local gate passes.
