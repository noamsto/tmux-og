# Plan: bridge predictive local echo (#972)

Spec: `docs/superpowers/specs/2026-10-18-bridge-predictive-echo-design.md` (accepted). Every step implements the spec as written; where this plan names a constant, signature or byte sequence, it is the spec's decision made concrete. All Go paths are under `picker/remotebridge/`; `daemon/` means `picker/remotebridge/daemon/`.

Gate commands (run from `picker/`, inside the devshell):

- `go test -race ./remotebridge/...`
- `golangci-lint run ./remotebridge/...`
- `nilaway ./remotebridge/...`

Final gates (repo root): `nix build .#default`, `nix flake check`, `nix build .#lint`.

Constants (all in `daemon/predict.go`, unexported):

| name | value |
|---|---|
| `predictShowAbove` | 30ms |
| `predictHideBelow` | 20ms |
| `predictSampleMax` | 1s |
| `predictSampleWindow` | 8 |
| `predictIdle` | 30s |
| `predictOutputBudget` | 64 << 10 |
| `predictModelRetry` | 2s |
| `predictExpiryMin` / `predictExpiryMax` | 500ms / 3s (expiry = clamp(4 × estimate)) |
| `predictMaxPending` | 128 |

## Step 1: mode, shared latency estimate, remote-mode query (`implement: sonnet`)

Files: new `daemon/predict.go`, new `daemon/predict_test.go`.

- `type PredictMode int` with `PredictAuto` (zero value), `PredictOn`, `PredictOff`. `func ParsePredictMode(s string) (PredictMode, error)`: `""`/`"auto"` give Auto, `"on"` gives On, `"off"` gives Off, anything else is `fmt.Errorf("predict: %q is not auto, on or off", s)`.
- `type predictShared struct` holds `mode PredictMode`, a `sync.Mutex`, a ring of the last `predictSampleWindow` samples, its fill count, and `wanted bool` (the hysteresis state).
  - `newPredictShared(m PredictMode) *predictShared` returns nil for Off.
  - `(*predictShared).sample(d time.Duration)` discards `d <= 0 || d > predictSampleMax`, pushes into the ring and recomputes the median (the lower middle for an even count). It then sets `wanted = true` when median > `predictShowAbove` and `wanted = false` when median < `predictHideBelow`.
  - `(*predictShared).want() bool`: On always gives true; Auto gives `wanted`; a nil receiver gives false.
  - `(*predictShared).estimate() time.Duration` gives the median, or 0.
- `type predictModes struct { cursorVisible, insert, origin, wrap bool; top, bottom int; cx, cy int }`.
  - `func predictModesCmd(paneID string) string` is `display-message -p -t <id> -F '#{cursor_flag}|#{insert_flag}|#{origin_flag}|#{wrap_flag}|#{scroll_region_upper}|#{scroll_region_lower}'`, built like `cursorCmd` in `daemon/seed.go`.
  - `func parsePredictModes(l controlmode.Line, ok bool) (predictModes, bool)` splits `strings.TrimSpace(string(l.Data))` on `|`. It requires exactly 6 fields: the four flags each exactly `0` or `1`, and both regions as non-negative integers with `top <= bottom`. Otherwise it returns false, as it does for `!ok` and an `Error` kind. `cx`/`cy` are filled in later by the caller.
- Tests (table-driven):
  - `ParsePredictMode` for all valid values and a rejected `"yes"`.
  - `sample`: a >1s sample is discarded; 8 samples of 50ms turn `want` on; a single 900ms sample amid 2ms ones never turns it on (median); it drops below 20ms back to off; 25ms keeps the previous state (hysteresis). On mode is always wanted; a nil receiver is false.
  - `parsePredictModes`: valid, a missing field, `2` as a flag, top > bottom, a non-numeric field, an error line.
- Gate: `go test -race ./remotebridge/daemon/ -run 'Predict'`.

## Step 2: the lazy pane model (`implement: sonnet`)

Files: new `daemon/predictmodel.go`, new `daemon/predictmodel_test.go`.

- `type predictModel struct` holds `e *vt.Emulator`, `w, h int`, the flags `hidden`, `insert`, `origin`, `slotUsed` (plain bools, pump-goroutine only), and `drained chan struct{}`. The drain goroutine closes `drained` when it exits. Tests wait on it.
- `func newPredictModel(w, h int, seed []byte, m predictModes) *predictModel` does, in order:
  1. `vt.NewEmulator(w, h)`, then `SetScrollbackSize(0)`.
  2. Register `e.RegisterEscHandler('7', …)` and `e.RegisterCsiHandler('s', …)`. Each sets `slotUsed = true` and returns false, so vt's defaults still run.
  3. `e.SetCallbacks(vt.Callbacks{CursorVisibility: …, EnableMode: …, DisableMode: …})` tracking `hidden` and `ansi.ModeInsertReplace` / `ansi.ModeOrigin`.
  4. Start the drain goroutine: loop `e.Read(buf)` and exit on the first error, closing `drained`.
  5. `feed(seed)`.
  6. Feed the mode bytes: `"\x1b[?25l"` if `!cursorVisible`; `"\x1b[4h"` if `insert`; `"\x1b[?7l"` if `!wrap`; when `top != 0 || bottom != h-1`, `fmt.Sprintf("\x1b[%d;%dr\x1b[%d;%dH", top+1, bottom+1, cy+1, cx+1)`; `"\x1b[?6h"` if `origin`.

  It returns nil (after `close()`) if any feed fails.
- `(*predictModel).feed(p []byte) (ok bool)` wraps `e.Write` in a `defer recover()` that returns false. A pre-1.0 parser fed remote-controlled bytes must not crash the daemon; say so in the comment.
- `(*predictModel).close()` closes the reply pipe's **writer**: `if c, ok := e.InputPipe().(io.Closer); ok { _ = c.Close() }`. `InputPipe()` is vt's `*io.PipeWriter`. The drain goroutine's `Read` then returns EOF and it exits; later vt reply writes fail with `ErrClosedPipe`, which vt ignores.
  - It never calls `e.Close()`: vt's `closed` flag is unsynchronised against `Read`, the race `agentdetect/screen/screen.go` documents.
  - It never writes a wake byte either. A write to an `io.Pipe` blocks until read, and a reader that has already exited would wedge the pump forever.
  - Idempotent; a nil model is a no-op.
- Accessors: `cursor() (x, y int)` from `e.CursorPosition()`, `alt() bool` from `e.IsAltScreen()`, `cell(x, y int) *uv.Cell` from `e.CellAt`.
- Tests:
  - A seed with text plus a CUP builds a model whose cursor and cells match.
  - `cursorVisible=false` gives `hidden`; `insert`/`origin` likewise.
  - A scroll region is applied and the cursor is restored after DECSTBM.
  - ESC 7 or `CSI s` fed later sets `slotUsed`, and vt still saves (ESC 8 then restores the position).
  - `?25h` after a hidden seed clears `hidden` through the callback.
  - `close()` returns, and `drained` closes within 1s, under `-race`. This holds both when the reader is parked in `Read` and right after a DSR reply was drained.
  - A DSR query (`"\x1b[6n"`) fed to the model does not block (drain works).
- Gate: `go test -race ./remotebridge/daemon/ -run 'PredictModel'`.

## Step 3: the predictor core (`implement: opus`; this is the spec's heart)

Files: `daemon/predict.go` (append), new `daemon/predictor_test.go`.

`type predictor struct`, pump-goroutine-only state:

- `shared *predictShared`, `model *predictModel`, `w, h int` (last resize; 0 = unknown).
- `preds []prediction`, where `type prediction struct { x, y int; ch byte /* 0 = erase */; curX, curY int; epoch int; at time.Time }`.
- `painted []struct{ x, y int }` (unique cells currently painted) and `paintedAny bool`.
- `epoch int`; `confirmed int`, initialised to −1 (the highest confirmed epoch); `blocked bool`.
- `runEpoch, runX, runY int`: where the current epoch's first prediction began. `runEpoch` is −1 until this epoch has one.
- `ground streamGround`, the tracker below, describing every byte written to the renderer so far.
- `keyAt, lastKey, lastRequest time.Time`; `fedSinceKey int`.
- `timer *time.Timer`.
- Injected funcs: `now func() time.Time`, `requestModel func()`, `paused func() bool`, `keysLost func() bool` (swap-and-clear).

`func newPredictor(shared *predictShared, requestModel func(), paused func() bool, keysLost func() bool) *predictor` uses `now: time.Now`; tests override `now`.

**Ground-state tracker.** `type streamGround struct { st uint8; utf8Left uint8 }` with `observe(b []byte)` and `atGround() bool`. It is a hand-rolled byte loop: the bare `x/ansi` parser measured 227 MB/s here, and this runs on every byte while the predictor exists.

- States: ground, esc, csi, string (OSC/DCS/APC/PM/SOS), stringEsc, and UTF-8 continuation (`utf8Left > 0`).
- Transitions:
  - From ground: ESC → esc; a lead byte `0xC2–0xDF`/`0xE0–0xEF`/`0xF0–0xF4` → 1/2/3 continuations left.
  - From esc: `[` → csi; `]`, `P`, `_`, `^`, `X` → string; `0x20–0x2f` stays in esc; anything else → ground.
  - From csi: `0x40–0x7e` → ground. From esc or csi, ESC → esc (tmux aborts the sequence and starts a new one).
  - From string: BEL ends only an OSC (track which string kind); ESC → stringEsc.
  - From stringEsc: `\` → ground; any other byte re-enters esc processing with that byte.
  - From any state, CAN `0x18` or SUB `0x1a` → ground.
  - During UTF-8: a continuation `0x80–0xBF` decrements; any other byte resets `utf8Left` to 0 and is processed from ground.
- Every error leans "not ground", so the tracker only ever withholds a paint, never permits one mid-sequence.

**Every byte written to the renderer is observed.** `onOutput` observes its payload; `paint`/`unpaint` output always ends at ground, so it is not observed. The pump calls `s.pred.observe(b)` for the gfx replay and the seed payload. **`paint()` returns nil unless `ground.atGround()`.** A payload that ends inside an OSC 8/133 or a split UTF-8 rune therefore gets no paint after it, and nothing is ever spliced into a remote sequence. Unpaint only runs while something is painted, i.e. only directly after a paint taken at ground, so it inherits the guarantee.

Methods. Each returns the bytes to write to the renderer as `FrameOutput`, nil for none:

- `unpaint() []byte`: if `!paintedAny`, nil. Otherwise `"\x1b8\x1b7"`, then for each painted cell:
  - a CUP;
  - `cell.Style.String()`, which for a zero style is already `ESC[m`. For a non-zero style, prepend `"\x1b[0m"`: `uv.Style.String()` then carries no leading reset and would layer onto the pen ESC 8 just restored. Every unpainted cell therefore starts from a reset pen;
  - `cell.Content` (`" "` if empty).

  Then `"\x1b8"`. It clears `painted`/`paintedAny`. It runs before the model is fed anything new; that is the invariant that makes it exact.
- `paint() []byte`: if `!paintable()` or no prediction has `epoch <= confirmed`, nil. Otherwise `"\x1b7"`, then for each displayed prediction (a prefix of `preds`) a CUP, `"\x1b[0;2m"` and `ch` (`' '` for erase). Then a CUP to the last displayed prediction's `curX, curY`. It records `painted`.
- `paintable() bool`: `model != nil && !paused() && shared.want() && !model.alt() && !model.hidden && !model.insert && !model.origin && !model.slotUsed`.
- `reset() []byte`: `out := unpaint()`; `preds = nil`; `epoch++`; `blocked = false`; stop the timer; return `out`.
- `dropModel()`: `model.close()`; `model = nil`.
- `onKeys(b []byte) []byte`:
  - If `keysLost()` or `paused()`, return `reset()`.
  - `t := now()`; `lastKey = t`; `fedSinceKey = 0`; if `keyAt.IsZero()` then `keyAt = t`.
  - If `!shared.want()`: if a model exists, drop the model after `reset()`; return the reset bytes.
  - If `model == nil`: if `w > 0 && t.Sub(lastRequest) >= predictModelRetry`, call `requestModel()` and set `lastRequest = t`; return nil.
  - If `!paintable()`, return `reset()`.
  - Then per byte:
    - Printable `0x20–0x7e`, unless `blocked`: the base is the model cursor when `preds` is empty, else the last prediction's `curX, curY`.
      - Refuse when `curX > w-2`, or `len(preds) >= predictMaxPending`, or `model.cell(x, y)` is nil or `Width != 1`. A refusal sets `blocked = true` and the byte is not predicted.
      - Otherwise append `{x, y, ch, curX: x+1, curY: y, epoch, at: t}`. If `runEpoch != epoch`, set `runEpoch, runX, runY = epoch, x, y`.
    - `0x7f`/`0x08`, unless `blocked`: with the same base, refuse unless `runEpoch == epoch && y == runY && x-1 >= runX`. That allows erasing this epoch's typed run, including chars already confirmed, and never the prompt or a previous line. A refusal sets `blocked`. Otherwise append the erase `{x-1, y, 0, x-1, y, epoch, t}`.
    - Any other byte: `epoch++`, `blocked = true`, stop processing the chunk.
  - Return `unpaint() + paint()` and arm the timer (below).
- `onOutput(p []byte) []byte`:
  - If `!keyAt.IsZero()`: `shared.sample(now()-keyAt)`; zero `keyAt`.
  - `ground.observe(p)`. This runs always, model or not, and before the early return, so the tracker never misses a byte.
  - If `model == nil`, return `p`.
  - `pre := unpaint()`; `fedSinceKey += len(p)`.
  - If `fedSinceKey > predictOutputBudget || now()-lastKey > predictIdle || !model.feed(p)`: set `preds = nil`, `dropModel()`, and return `pre + p`.
  - Resolve the predictions in order with the spec's rules.
    - Character: confirmed iff cursor row == y && cursor x > x && cell content == string(ch). Wrong iff cursor row == y && cursor x > x && content differs. Else pending: stop.
    - Erase: confirmed iff row == y && cursor x <= x. Else pending: stop.
    - Confirm pops the prediction and sets `confirmed = max(confirmed, epoch)`. Wrong sets `preds = nil` and `epoch++`.
  - If `preds` is empty, `blocked = false`. If `!paintable()`, `preds = nil`. Re-arm the timer.
  - Return `pre + p + paint()`.
- `onSeed(seed []byte) []byte`: `out := reset()`; if a model exists and `!model.feed(seed)`, `dropModel()`; return `out`.
- `onResize(w, h int) []byte`: if the dims are unchanged, nil. Otherwise set them, `out := "\x1b8"` if `paintedAny` (else nil), clear `painted`, `preds = nil`, `epoch++`, `dropModel()`, and return `out`.
- `onModelSeed(seed []byte, m predictModes)`: ignored when `model != nil || w == 0`; else `model = newPredictModel(w, h, seed, m)`.
- `observe(b []byte)`: `ground.observe(b)`; the pump calls it for the bytes it writes on its own (the gfx replay, the seed payload).
- `shutdown() []byte`: `out := unpaint()`; `preds = nil`; stop the timer; `dropModel()`; return `out`. The pump calls it when the sink's channel closes.
- `timerC() <-chan time.Time`: nil when there is no timer or the receiver is nil.
- `onExpire() []byte`: if the oldest prediction is older than `expiry()`, return `reset()`; otherwise re-arm and return nil.
- Arming: when `preds` is non-empty, `timer` is reset to fire at `preds[0].at + expiry()`, where `expiry()` is `clamp(4×shared.estimate(), predictExpiryMin, predictExpiryMax)`. When `preds` is empty, the timer is stopped.

Test harness in `predictor_test.go`:

- `local`: a `vt` emulator fed every byte the test writes to "the renderer": the seed, then every `onKeys`/`onOutput`/`onSeed`/`onResize`/`onExpire` return value, in order.
- `ref`: an emulator fed only the remote stream (the seed plus the raw `p` passed to `onOutput`).
- `screenEq(t, local, ref)` compares every `CellAt` (Content, Width, `Style.Equal`) and `CursorPosition`.
- A fake clock is used. The shared estimate is On, except where Auto is under test.
- The model is built via `onResize(80, 24)` and then `onModelSeed(render.Seed(…), modes)`.

Tests:

- **AC1:** at a `$ ` prompt (cursor 2,0), keys `l`, echo `l` (confirms epoch 0), then keys `s`. Before the `s` echo, `local` cell (3,0) is `s` with `AttrFaint` set and the local cursor is at 4,0. After `onOutput("s")`, `screenEq(local, ref)` holds, and cell (3,0) has no `AttrFaint`.
- **Tentative:** the first key after the seed paints nothing (`onKeys` returns nil) until its echo confirms.
- **AC2:** a seed with `\x1b[?1049h` (alt) then keys gives `onKeys` nil, and `local` is unchanged. Likewise a seed with `cursorVisible=false`.
- **AC3, mispredictions.** Each ends with `screenEq(local, ref)` and no `AttrFaint` cell anywhere:
  - The echo is a different char.
  - No echo, then `onExpire` after the expiry.
  - A redraw: `\r\x1b[K$ other`.
  - Backspace refused: the remote prints a bell only.
  - fish-style redraw with grey autosuggestion text right of the cursor: confirmed, and no residue.
  - Typing over existing non-blank cells (prediction mid-line), then a remote ICH-style insert echo `\x1b[1@x`.
- **AC3, drops:**
  - `onSeed` while painted unpaints. `preds` is empty afterwards, and `local` equals `ref` after the seed bytes are also fed to both.
  - `onResize` with new dims while painted emits exactly `"\x1b8"`, drops the model, and the next key requests a model.
- The ESC 7 the app sends (sticky slot) stops painting.
- **A split sequence gets no paint inside it.** With a confirmed epoch and a pending prediction:
  - `onOutput("\x1b]8;;http://x")`, then `onOutput("\x1b\\")`;
  - likewise `onOutput("\xc3")`, then `onOutput("\xa9")`.

  The first call's return carries no `\x1b7`; the second's does; `screenEq` holds at the end. A `streamGround` table test covers each transition, including BEL ending an OSC but not an APC, CAN/SUB, and ESC-non-backslash inside a string.
- **Unpaint resets the pen:** the real pen is bold red (`\x1b[1;31m` in the stream before the prompt cursor), and the cells under the predictions are grey autosuggestion text (`\x1b[90m`) plus one blank default cell. After a misprediction and unpaint, `screenEq(local, ref)` holds, including `Style`.
- Erasing a confirmed char of the current run is predicted. Backspace as the first key of a new epoch is refused.
- `shutdown()` while painted returns the unpaint bytes and closes the model (its `drained` closes).
- The right edge: keys reaching `w-2` set `blocked` and paint nothing beyond it.
- The output budget and idle (fake clock) drop the model; the next key requests a model, and `requestModel` is rate-limited to once per 2s.
- `keysLost` resets.
- Gate: `go test -race ./remotebridge/daemon/ -run 'Predictor'`.

## Step 4: wire the predictor into the sink, router and main loop (`implement: opus`)

Files: `daemon/daemon.go`, `daemon/router.go`, `daemon/reconcile.go`, new `daemon/predictseed.go`, new `daemon/predictsink_test.go`.

- `sinkFrame` gets `modes *predictModes`. Two daemon-internal frame types sit next to `sinkFrame`: `framePredictKeys wire.FrameType = 0xf0` and `framePredictSeed wire.FrameType = 0xf1`. A comment says they never reach the wire and the pump consumes them.
- `outputSink` gets `pred *predictor` (nil means prediction off and every path is today's), `modelWanted atomic.Bool` and `keysLost atomic.Bool`.
- `newOutputSink(conn, gfx)` becomes `newPredictingSink(conn, gfx, nil)`. `newPredictingSink(conn net.Conn, gfx *graphics.Proxy, shared *predictShared) *outputSink` builds `pred = newPredictor(shared, func(){ s.modelWanted.Store(true) }, s.isPaused, func() bool { return s.keysLost.Swap(false) })` when `shared != nil`, then calls `start`. `isPaused` locks `s.mu`. The pump never holds `s.mu`, so this cannot deadlock.
- `(*outputSink).keys(p []byte)` is nil-safe (`s == nil || s.pred == nil` returns). Under `s.mu`, if not closed, it does a non-blocking send of `{framePredictKeys, copy(p)}`. When the channel is full it calls `s.keysLost.Store(true)` and never touches `dropped`.
- `(*outputSink).predictSeed(seed []byte, m predictModes)` makes the same non-blocking send of `{framePredictSeed, seed, &m}`. A full channel drops it silently: no model, and a later key re-requests.
- `(*outputSink).takeModelWanted() bool` applies `takeDirty`'s gates without spending the flag (closed, paused, `len(s.ch) > 0`), then `modelWanted.CompareAndSwap(true, false)`.
- Pump (`start`). Replace the blocking `v, ok := <-s.ch` with a `select` on `s.ch` and `s.pred.timerC()`. A timer fire writes `s.pred.onExpire()`, if non-empty, as `FrameOutput` (`wire.WriteStream`) and continues. Then:
  - `framePredictKeys`: write `s.pred.onKeys(payload)` if non-empty; continue. It is never fed to `s.mouse`.
  - `framePredictSeed`: `s.pred.onModelSeed(payload, *modes)`; continue. **Nothing is written.**
  - `FrameOutput`, after kn/gfx and the existing empty-payload `continue`: `s.mouse.Feed(payload)` on the remote bytes as today, then `payload = s.pred.onOutput(payload)` when `pred != nil`, then the existing write.
  - `FrameSeed`: when `pred != nil`, write `s.pred.onSeed(payload)` first, if non-empty, before the gfx replay; the rest is unchanged.
  - `FrameResize`: when `pred != nil`, decode it. If it decodes, write `s.pred.onResize(w, h)` first, if non-empty; then the resize frame as today.
  - In the `FrameSeed` path, after writing `onSeed`'s bytes, call `s.pred.observe(replay)` for a non-empty gfx replay and `s.pred.observe(payload)` for the seed itself, in write order.
  - Restructure so the existing `s.mouse.Feed` call for Output/Seed still sees exactly the remote bytes.
  - **Close path.** In the channel-closed branch, when `pred != nil`, prepend `s.pred.shutdown()` to the kn/gfx tail before the final write. If the tail is empty, write the shutdown bytes alone when non-empty.
    - The unpaint must precede the tail: what we painted sits after the last byte written, while the tail is bytes held back and not yet written.
    - The model must close, or its drain goroutine and its vt emulator leak for the daemon's life. vt allocates a 4 MiB parser buffer per emulator.
    - `reconcile.go:754` registers a fresh, unseeded sink on the same conn, so leftover paint would otherwise persist into it.
- `Router` gets `predict *predictShared`, nil from `NewRouter`, so tests and ctlConn's unverified router stay off.
  - `wireRenderer` (`daemon.go` ~2803) and the `reconcile.go` ~754 re-register use `newPredictingSink(conn, gfx, router.predict)`.
  - `func (r *Router) predictModelPanes() []string` is a copy of `reshapedPanes` over `takeModelWanted`.
- `daemon/predictseed.go`: `func seedPredictModels(router *Router, rt roundTrip)` collects the panes and their sinks as `reseedReshaped` does. Per pane it issues `cursorCmd(id), predictModesCmd(id), captureCmd(id)`, all of them via one `rt(cmds...)`. Per pane, in order:
  1. Read the three replies.
  2. `parseCapture`; on `isErr`, write a stderr line `daemon: predict model seed for %s: …` and continue.
  3. `parsePredictModes`; on false, continue silently, since the remote cannot report the modes.
  4. `parseCursor`; set `m.cx, m.cy`.
  5. Call `sinks[i].predictSeed(render.Seed(replaceLF(captured), cx, cy, alt, appck, mouse, tmodes), m)` **immediately**, before the next pane's `next()`.

  The doc comment cites the spec's ordering paragraph.
- Main loop (`daemon.go` ~1687): call `seedPredictModels(router, rt)` right after `reseedReshaped(router, rt)`.
- `runMirror`: after `router := NewRouter()`, set `router.predict = newPredictShared(cfg.Predict)` (step 6 adds `Config.Predict`). **Sequencing: step 4 adds the `Config.Predict PredictMode` field itself** (zero value = Auto), with a doc comment. Step 6 only wires the flag.
- Tests (`predictsink_test.go`, `net.Pipe` harness as in `sinksplit_test.go`, reading frames back; feed the payloads into a `vt` "local" emulator):
  - **AC1 end-to-end through the real pump:**
    1. Shared On; `enqueue(FrameResize 80x24)`; `predictSeed(seed, modes)`; `keys("l")`; `Write("l")` (confirms); `keys("s")`.
    2. The renderer receives a `FrameOutput` that paints a faint `s`.
    3. `Write("s")` makes local equal the reference.
  - **The model seed writes nothing:** after `predictSeed` alone, a short read deadline sees no frame.
  - **Output after a pane's capture reply reaches that pane's model:** two predicting sinks A (`%1`) and B (`%2`) on one router, both flagged `modelWanted` and sized. A scripted `roundTrip` (`scriptedRTRouter`, `reconcileordering_test.go`) answers A's cursor, modes and capture (capture text `$ `), then routes `%output %1 ab`, then answers B's three replies.
    - Drive A's pump: `keys("c")`, then `Write("c")`, then `keys("d")`.
    - Assert the faint `d` is painted at column 5: `$ ` fills columns 0–1, the interleaved `ab` columns 2–3 and the echoed `c` column 4. It would land at column 3 if `predictSeed` were enqueued after the loop, since the model would then be built after `ab` had already passed it.
  - **Close while painted:** a predicting sink with a painted prediction is `Close()`d and `Wait()`ed. The last frame read starts with `"\x1b8\x1b7"`, and the model's `drained` channel is closed (test hook: read `s.pred` after `Wait`, which happens-after the pump).
  - **A full channel on `keys`** sets `keysLost` and leaves `dropped == 0`.
  - **`takeModelWanted`** honours the paused/drained gates.
  - **`seedPredictModels`** sends the three commands in the order cursor, modes, capture (`orderedLog`).
- Gate: `go test -race ./remotebridge/daemon/`.

## Step 5: keys from `pumpInput` (`implement: sonnet`)

Files: `daemon/daemon.go`, `daemon/reconcile.go`, `daemon/pumpinput_test.go`.

- Rename `sinkMouseResolver` to `sinkResolver(router *Router, paneID string) func() *outputSink` (it returns `router.sink(paneID)`) and delete `sinkMouseMode`. Its only caller is the resolver, verified by grep.
- `pumpInput`'s last parameter becomes `sink func() *outputSink`. In `deliver`:
  1. Resolve `s` once.
  2. Take `m = &s.mouse` when `s != nil`, for `isDismissKey`.
  3. Call `s.keys(payload)` after `paste.handle` and the empty check, **before** the `SendKeysArgs` loop.
- Update the 5 call sites: `reconcile.go:668`, `reconcile.go:951`, `daemon.go:2114`, `daemon.go:2128`, `daemon.go:2755`.
- Tests:
  - Adapt `pumpinput_test.go:608`: wrap the tracker in `&outputSink{}` whose `mouse` is fed the same bytes.
  - Adapt `pumpinput_test.go:654`: `sinkResolver`.
  - New: a sink with a predictor (shared On), whose channel the test drains by hand, receives a `framePredictKeys` frame carrying the typed bytes before `send` is called.
- Gate: `go test -race ./remotebridge/daemon/ -run 'PumpInput|DismissKey|Predict'`.

## Step 6: daemon flag (`implement: sonnet`)

Files: `cmd/daemon/main.go`, `cmd/daemon/main_test.go`.

- Add the flag `predict := flag.String("predict", envDefault("OG_DAEMON_PREDICT", "auto"), "predictive local echo in mirror panes: auto (only while echo latency is high), on, or off")`.
- After `flag.Parse`, call `daemon.ParsePredictMode(*predict)`. On error, print `daemon: <err>` to stderr and exit 2, the way the file's other startup validation errors exit (match the nearest one, e.g. `--select`'s).
- Set `Predict: mode` in the `daemon.Config` literal.
- Test: following the file's existing pattern for flag/env helpers, cover that `OG_DAEMON_PREDICT=off` parses to Off and an invalid value is rejected. If `main()` is not unit-testable, `ParsePredictMode`'s step 1 test covers parsing; then add only a test of the default flag value via whatever seam `main_test.go` already uses, or none.
- Gate: `go test -race ./remotebridge/cmd/daemon/`.

## Step 7: the launcher passes the option (`implement: haiku`)

Files: `scripts/og-remote-open.sh`.

- In both export blocks (the placed block ~line 795 and the dedicated block ~line 991), assign then export:
  ```
  predict="$(tmux show-options -gqv @og_bridge_predict 2>/dev/null || true)"
  export OG_DAEMON_PREDICT="$predict"
  ```
  A combined `export X="$(…)"` trips shellcheck SC2155. An empty value means auto. Reuse one `predict` assignment if both blocks share a scope, and match the surrounding variable naming.
- Gate: `shellcheck scripts/og-remote-open.sh`; `bats tests/remote.bats` if it runs offline (it exercises the launcher with fakes).

## Step 8: docs (`implement: sonnet`)

Files: `docs/agents/bridge-daemon.md`, `docs/agents/scripts.md`.

- `bridge-daemon.md`: a new `## Predictive local echo (#972)` section before `## What the Remote Host Needs on PATH`. It covers:
  - the option and env;
  - the lazy model and its lifetime (model seed via `seedPredictModels`, never written to the renderer, and why: `ESC[2J` with scroll-on-clear);
  - the paint protocol and the invariant that every pump write is preceded by `unpaint`;
  - the paint preconditions;
  - epochs and confirmation;
  - the drop points (real seed, dims change, budget, idle, pause, `keysLost`).

  Match the doc's bullet density and link the spec.
- `scripts.md`: in the `og-remote-open` row, append that it exports `OG_DAEMON_PREDICT` from `@og_bridge_predict`.
- Gate: `nix build .#lint` (typos and markdown hooks).

## Order and parallelism

Steps 1 → 2 → 3 → 4 → 5 run in order (each consumes the previous step's symbols). Steps 6, 7 and 8 depend only on step 4's `Config.Predict` and the spec, and run in parallel after step 4, alongside step 5.

## Acceptance evidence

| AC | evidence |
|---|---|
| AC1 | `TestPredictor…AC1` (step 3) and the through-the-pump test (step 4) |
| AC2 | step 3's alt-screen and hidden-cursor tests |
| AC3 | step 3's misprediction and drop tests, step 4's seed test |
| AC4 | `nix build .#default` |
| AC5 | `nix flake check` |
| AC6 | `nix build .#lint` |

The vendorHash is unchanged: `charmbracelet/x/vt`, `ultraviolet` and `x/ansi` are already in `picker/go.mod`. If `go mod tidy` promotes `ultraviolet` from indirect to direct (the `uv.Cell` import), `nix build .#default` verifies the vendor tree still matches. If it does not, update `picker/default.nix`'s `vendorHash` from the build's reported hash.
