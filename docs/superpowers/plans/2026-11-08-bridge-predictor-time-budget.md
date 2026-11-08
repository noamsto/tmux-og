# Bridge predictor: time-bounded model feed, no false epoch bump on expiry (#1092)

Files: `picker/remotebridge/daemon/predictor.go`, `predict.go`, `predictor_test.go`,
`predictor_resolve_test.go`, `docs/agents/bridge-daemon.md`. Nothing else.

## Reproduction (both confirmed on main before any edit)

- Defect 1: `newHarness(.., w, h)`, confirm `a`, key `b`, one `onOutput` of 60 KiB of `\n`
  (below the 64 KiB byte budget), real clock: 1.0 s at 80x24, 4.1 s at 200x50, 13.9 s at 400x100;
  the model survives each time.
- Defect 2: `confirmedHarness`, keys `ab\x7f`, output `ab\b \b`, +400 ms, key `c`, +150 ms,
  `expire()`: epoch 1 -> 2. Cause: `onKeys` zeroes `fedSinceKey`, so the quiet-drop branch
  (`fedSinceKey == 0 || !drawsModel()`) takes `reset()`; and `drawsModel` over the whole
  set is false anyway because `c` is not echoed.

## Design

Defect 1 (`onOutput`):
- Replace the byte budget by a time budget. `predictOutputBudget` is removed; add
  `predictFeedBudget = 20ms` and `predictFeedSlice` (bytes per vt write).
- `fedSinceKey int` becomes `fedCost time.Duration` (reset in `onKeys` where `fedSinceKey` was).
- `predictFeedSlice = 256` bytes. From the step-0 measurement (~226 us/byte of `\n` at 400x100) one
  slice costs ~58 ms worst case there, so the stall bound is budget + one slice (~80 ms at
  400x100, vs 13.9 s for 60 KiB); at 80x24 (~17 us/byte) it is ~4 ms. `time.Now` per 256 B is noise.
- Slices end just after a byte < 0x80 at or past `predictFeedSlice` (none: feed the rest). vt's
  grapheme buffer is flushed at the end of every `Write` (x/vt `Emulator.Write`), so a cut inside
  a multi-rune cluster would change its width and drift the model from local tmux; vt's buffer is
  always empty after an ASCII byte, so ASCII-boundary slicing is equivalent to one `Write`.
- `onOutput` feeds `b` in those slices through `m.feed`; before each slice, if
  `fedCost > predictFeedBudget` the model is dropped (`reset` + `dropModel`, payload still
  written to the renderer unfed, as today). Each slice's cost is `p.now().Sub(start)`,
  using the injectable `p.now`, so tests drive it with a ticking fake clock. Idle and
  failed-feed drops unchanged. No model -> early return, so a non-predicting pane pays nothing.

Defect 2 (`onExpire`):
- Add `fedTotal int` (monotonic bytes fed, never reset on key) and `fed int` on `prediction`
  (the `fedTotal` when the key was typed), set at BOTH `onKeys` append sites (backspace and char;
  a backspace left at 0 would let a char+BS no-echo prompt pass the evidence guard). Output "since the key" is `fedTotal > pred.fed`;
  this replaces the `fedSinceKey == 0` guard and keeps its meaning for a char+BS no-echo prompt.
- On head expiry: find the longest prefix `preds[:k+1]` with `fedTotal > preds[k].fed`, model cursor
  == `preds[k].curX/curY` and every standing char of the prefix (not overwritten within the
  prefix) equal to its model cell (`drawsPrefix(k)`, `drawsModel` becomes `drawsPrefix(len-1)`).
  Found: `unpaint`, drop that prefix, `p.preds` keeps the tail, `blocked=false` only if none left,
  repaint the tail (`unpaint+paint`), `arm()`; no `reset()`, no epoch bump. None found:
  `reset()` exactly as today. A real misprediction (no prefix drawn) still resets and bumps.

## Steps

1. Tests first, red on old code (scratch-verify by stashing the production edit in a copy):
   - `TestPredictorFeedTimeBudget` (predictor_test.go): small pane (20x5). `h.p.now` is a ticking
     fake (returns `h.clock`, then advances it a fixed 5 ms; deterministic, no real durations).
     (a) `\n` flood, ticking clock: model dropped, fake time consumed is a few slices' worth (far
     below a full feed), `requireDrained`, `settled`; (b) more than 64 KiB of `\x1b[H` with a
     FROZEN clock keeps the model (red on old code: the byte budget dropped it); (c) fedCost
     accumulates over several small payloads after one key and is reset by the next key (a key
     between payloads keeps the model; dropping the reset in `onKeys` fails it). Replaces the old
     byte "budget" subtest (same requests/rate-limit assertions).
   - Slice-boundary test: a payload with a multi-rune emoji (U+1F468 U+200D U+1F469) straddling
     `predictFeedSlice`, via `onOutput`: model cursor and cells equal one `m.feed` of the whole
     payload; red with naive fixed slicing.
   - `predictor_resolve_test.go`: `TestPredictorExpireKeepsEpochAfterNetZeroTail` (the repro,
     epoch stays 1, `c` still outstanding and painted faint, local == ref apart from the overlay;
     variant A: `c` echoed -> confirms, `settled`; variant B: no echo, past c.at+expiry,
     `expire()` resets, epoch +1: pins the re-arm to the tail's new head);
     `TestPredictorExpireMispredictionResets` (tail never drawn, unrelated output arrived, epoch
     unchanged right after that output, expire -> epoch +1, nothing painted, `settled`).
   - `TestPredictorQuietExpiryNeedsOutput` stays unchanged and green.
   - Red checks on old code use a temp WIP commit / file copy of predictor.go, never bare `git stash`.
2. Implement predictor.go / predict.go changes.
3. Measure after (same scratch measurement, real clock) at 80x24, 200x50, 400x100: record
   before/after; delete `zz_measure_test.go` (checklist item before commit).
4. Update `docs/agents/bridge-daemon.md` lines 511 (drop-point bullet: time budget, ASCII-boundary
   slices, wall-clock caveat: a GC pause counts, worst case a re-requested model) and 521 (Expired
   bullet: longest-prefix quiet drop, `fedTotal`). Line 528's 64 KiB (REP) stays; the 2026-10-18
   plan/spec are history.
5. Gates: `go test -race ./picker/remotebridge/daemon/...`, `golangci-lint`/`nilaway` via
   `nix flake check`, `nix build .#default`, `nix build .#lint`.

## Acceptance map

Defect 1 test + before/after numbers; defect 2 repro test; misprediction test; docs; three nix gates.
