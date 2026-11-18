# Predictor model: raw C1 / non-UTF-8 bytes (#1096)

## Problem

`predictModel.feed` hands remote bytes to x/vt. vt's parser table (`ansi/parser.Table`) executes
C1 controls in every state (0x84 IND, 0x8D RI scroll; 0x9B/0x9D/0x90 open CSI/OSC/DCS), ignores
0xA0-0xC1/0xF5-0xFF, and decodes a UTF-8 lead byte plus whatever follows with no validation.
Local tmux (probed, scratch server, `-f /dev/null`, next-3.9, 20x6) treats none of these as
controls. `feedTimed` cuts only after ASCII, so a 4096 x 0x84 run is one vt write (1.02 s at
400x100) and the model draws the wrong screen.

Probed tmux behaviour (each probe: `printf` in a pane, `capture-pane` + `#{cursor_x}`):

| input | tmux draws |
| --- | --- |
| ground: any stray byte 0x80-0xFF | one U+FFFD each |
| ground: truncated / overlong / surrogate / >U+10FFFF sequence (`c3 b`, `e0 80 80`, `ed a0 80`, `f4 90 80 80`) | one U+FFFD per bad sequence, ASCII after it is kept |
| ground: `c2 84` (U+0084) | printed, width 0 (cursor does not advance); `c2 a0` width 1 |
| ESC, CSI, DCS-entry state + byte >= 0x80 | ignored, state unchanged (`ESC [ 1 84 m` is still SGR) |
| OSC / DCS / APC / SOS / PM body, incl. 0x84 and 0x9C | consumed as body until BEL / `ESC \` |

vt differs from tmux in the ground stray-byte row, the truncation case of the invalid-sequence row (vt swallows the
ASCII after `c3`; overlong/surrogate/>U+10FFFF it draws as one U+FFFD like tmux), the ESC/CSI/DCS-entry rows and
`c2 84`, and in the body row only on 0x9C (vt ends OSC/DCS on it;
APC/SOS/PM bodies take only 0x20-0x7F as Put: C2-F4 enter its UTF-8 state and return to ground, other bytes >= 0x80 execute or fall to ground, so the string ends).

## Design (assessed against the issue's proposal)

Two approaches considered:

1. Teach `feedTimed` to cut inside runs of >= 0x80 bytes. Rejected: does not fix the wrong screen,
   and cuts inside UTF-8 break vt's grapheme flush parity (existing comment).
2. `vtGuard` in `predictModel.feed` (the issue's proposal). Chosen.

`vtGuard{st parser.State; need int; lo, hi byte}` lives beside `titleFilter` and runs on the
title-filtered bytes (the bytes vt actually sees), carrying state across `feed` calls
(seed and mode bytes go through `feed` too, so it starts at Ground like vt).

Per byte c:
- `st == Utf8` (inside a sequence): c must be in `[lo, hi]` (strict UTF-8: E0 -> A0..BF,
  ED -> 80..9F, F0 -> 90..BF, F4 -> 80..8F, else 80..BF) else poison. Counting down `need`;
  at 0 `st = Ground`. A bad continuation poisons, so vt's blind collection never runs.
- c < 0x80: `st, _ = parser.Table.Transition(st, c)` (the exact transition vt's `advance` takes).
- c >= 0x80, `st == Ground`: valid lead C2..F4 starts a sequence; `c2 80..9f` (U+0080..U+009F, C1 in
  disguise) poisons unconditionally (tmux gives it width 0 and appends; vt's grapheme path overwrites the cursor cell
  instead, so the two diverge); any other byte poisons.
- c >= 0x80, `st` in {OscString, DcsString}: body byte; 0x9C is dropped (tmux consumes it as body,
  vt would end the string); everything else passes (vt: Put, same as tmux).
- c >= 0x80 in any other state (ESC, CSI, DCS entry/param/intermediate, SOS/PM/APC): poison.

Poison = `pm.poisoned = true` before any byte of the slice reaches vt, so `feed` returns false and
the predictor drops the model (existing path, same as REP). A 4096 x 0x84 run is therefore a
scan of the first byte. A remote that really emits non-UTF-8 loses prediction for that pane
until the next seed; accepted (binary output is not a typing context).

The guard is O(1)/byte (table lookup); `streamGround` documents why a heavier parser is not
affordable on this path.

## File list

- `picker/remotebridge/daemon/predictmodel.go`: `vtGuard`, wire into `feed`.
- `picker/remotebridge/daemon/predictmodel_test.go`: `TestPredictModelNonUTF8MatchesTmux`, `TestVTGuardAgreesWithParser`.
- `picker/remotebridge/daemon/predictor_test.go`: bounded-time test + benchmark for the C1 run through `feedTimed`.
- `picker/remotebridge/daemon/predict.go`, `predictor.go`: delete the "Residual" paragraph / stale `feedTimed` sentence.
- `docs/agents/bridge-daemon.md`: bullet beside the REP bullet (and `rg` check that no doc still lists the residual).
- this plan.

## Steps

- [ ] **Step 1: red tests and baseline** (run from `picker/`). Add `TestPredictModelNonUTF8MatchesTmux`: table of
  `{name, in, keep bool, tmux rows, tmux cursor}` recorded from the probes above. A `keep` row
  must return true from `feed` (`t.Fatalf` otherwise) and draw the recorded rows and cursor;
  a non-keep row passes when `feed` is false or the model draws the recorded rows. keep rows: 0x84 in
  an OSC body, 0x84 in a DCS body, 0x9C inside an OSC body and inside a DCS body (rows `ab`),
  `ESC ] 0 ; Ĝ BEL` (c4 9c: a 0x9C continuation byte in an OSC body), valid mixed UTF-8 *without*
  a stray byte (`A é B C`), `c2 a0` (cursor 3). Non-keep rows: lone 0x84, 0x8D, 0x9C in ground,
  0x84/0x9C in ESC and CSI state, 0x84 in APC/SOS/PM body, 0x9B/0x9D/0x90 in ground,
  truncated/overlong/surrogate/>U+10FFFF UTF-8, `A é B 84 C`, and `c2 84`/`c2 9f` (poisoned unconditionally).
  Rows where vt already draws what tmux does (overlong `e0 80 80`, surrogate `ed a0 80`, `f4 90 80 80`)
  are expected green before the fix: the red expectation is "every row where vt's drawing or the keep contract differs
  fails", and the PR lists which rows were red. `AB CR c2 84` is one of the rows (vt overwrites the cell).
  Add `BenchmarkPredictFeedC1Run` and `TestPredictorFeedC1RunBounded`: 4096 x 0x84 through
  `predictor.onOutput` at 400x100, timed with `time.Now()` (the harness clock is fake), asserts < 100 ms (message prints the measured duration)
  and model dropped. Run `go test ./remotebridge/daemon -run 'NonUTF8|C1RunBounded' -bench C1Run`
  on the unchanged code: the refuse and bounded-time rows must fail on the draw/time
  assertions (not an idle drop, not a compile error), and the benchmark records the ~1 s "before".
  Commit-free evidence: paste the failing lines and the number into the PR `## Testing`.
- [ ] **Step 2: differential test.** `TestVTGuardAgreesWithParser`: seeded generator over an alphabet
  {ASCII printable, ESC, `[ ] P _ X ^ \ k`, BEL, CAN, SUB, LF, digits, `;`, `0x80-0x9F`, `0xA0 0xA9 0xBE 0xBF 0xC0 0xC1 0xC2 0xDF 0xE0 0xE1 0xED 0xEF 0xF0 0xF1 0xF3 0xF4 0xF5 0xFF`}, random split points.
  Two checks per stream. (a) Independent oracle for the verdict (written from the tmux table, not from the
  guard): poison iff, in ground, a byte >= 0x80 is not part of a strictly valid UTF-8 sequence, or a
  byte >= 0x80 appears in any state other than Ground, UTF-8 continuation, OscString or DcsString;
  the oracle drives its own real `ansi.Parser` and reads `State()`, so it does not share the guard's tracking.
  Spelled out: it feeds the parser every byte except a 0x9C seen while `State()` is OscString or DcsString (the tmux
  rule: that byte is body); it stops at the first poison byte (state after it is meaningless); a strictly valid
  UTF-8 prefix cut off by end of stream is not poison (test: `utf8.FullRune(rest)` false => truncated prefix; else `DecodeRune`, poison on RuneError size 1 or U+0080..U+009F; compare the boolean verdict only, not the position); `c2 80..9f` is poison. The oracle's input is the title-filtered stream
  (the guard runs after `titleFilter`); check (a) runs the guard alone on that same stream. This fails an over-poisoning guard. (b) For non-poisoned streams: feed the guard's filtered bytes to the real
  parser one byte at a time (Handler records Print runes and Execute bytes) asserting
  `guard.st == parser.State()` after every byte outside UTF-8 sequences, no `Execute` byte >= 0x80,
  no `Print` of `utf8.RuneError` unless the input held U+FFFD, and chunk invariance (any split gives the
  same verdict and output). Also feed the same streams to `predictModel.feed` and to a model fed the
  filtered bytes directly: same cells (the second model is built with `newPredictModel` and written via `pm.e.Write` so the REP handler and the reply-pipe drain exist; a bare `vt.NewEmulator` blocks on `ESC [ 6 n`), using the same chunking on both sides (vt flushes graphemes per Write) and `pm.close()` on each. The
  parser-level checks run on 20k fixed-seed streams; the two-emulator comparison runs on a 300-stream subset at a small pane
  size (cost under `-race`).
- [ ] **Step 3: implement `vtGuard`** (test-first: steps 1-2 are red/compile-red until now) and wire into
  `feed`: `pm.fbuf = pm.title.filter(...)`, then `fb, bad := pm.guard.filter(...)`; `if bad { pm.poisoned = true; return false }`
  (never assign `pm.poisoned = bad`: that would clear an earlier REP poison; compact in place to drop 0x9C;
  on poison skip `Write`). Expect steps 1-2 green.
  Command: `cd picker && go test -race ./remotebridge/daemon/...`.
- [ ] **Step 4: docs and comments.** Remove the "Residual" paragraph in `predict.go` and the
  "Raw C1 bytes are not ASCII ..." sentence in `feedTimed`; replace with a pointer to the guard. Rewrite the residual sentence in the "Drop points" bullet (`bridge-daemon.md` ~line 511) and list guard poison as a drop point; add the
  `bridge-daemon.md` bullet next to the REP one (states the table above in two lines, names
  `TestPredictModelNonUTF8MatchesTmux`/`TestVTGuardAgreesWithParser`; notes that a seed holding a poisoning byte yields
  no model, retried every `predictModelRetry`). Negative check: `rg -n 'Residual: a run of up to|Residual, not closed' docs picker` returns nothing
  (`predictor_test.go:~713`'s "fed whole" is about over-cap valid UTF-8, unchanged: leave it).
- [ ] **Step 5: after-measurement.** Rerun the benchmark on the fixed tree; PR body states
  before/after.
- [ ] **Step 6: gates.** `nix build .#default`, `nix flake check`, `nix build .#lint` (from the devshell).

## Acceptance

- [ ] `TestPredictModelNonUTF8MatchesTmux` table: Step 1/3 (`go test -run NonUTF8`).
- [ ] Differential test against the ansi parser: Step 2 (`go test -run VTGuardAgrees`).
- [ ] 4096 x 0x84 no longer ~1 s, measured before/after: Step 1 baseline, Step 5.
- [ ] Docs bullet beside the REP note: Step 4 (`rg -n 'vtGuard' docs/agents/bridge-daemon.md`).
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint`: Step 6.

## Evidence / consumer map

Producer: remote bytes -> `outputSink` pump -> `predictor.onOutput` -> `feedTimed` -> `predictModel.feed`
(also seed + modes in `newPredictModel`, `seedPredictModels`, and `predictor.onSeed` at predictor.go ~480, which feeds every FrameSeed into an existing model and drops it on false: compatible, same drop path, and guard state carries across feeds like vt's). Consumers of `feed`'s result: `onOutput`
(drops model on false, compatible: the same path REP uses), `newPredictModel` (returns nil on false:
a seed containing non-UTF-8 now yields no model; compatible, the predictor retries per
`predictModelRetry`). `streamGround` (paint boundary) is independent of the model and unchanged.
No other consumer of `predictModel.feed` (verify with `rg -n '\.feed\(' picker`).
