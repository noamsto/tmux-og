# Bridge: predictive local echo in mirror panes (#972)

## Problem

A key typed into a mirror pane travels local tmux → renderer stdin → `FrameInput` → daemon `pumpInput` → remote `send-keys`; the character appears only when the remote's echo comes back as `%output` → `Router.Route` → the pane's `outputSink` pump → `FrameOutput` → renderer stdout → the mirror pane's pty. Every character costs a full round trip (network plus the remote app).

## Where a prediction can be drawn (feasibility)

The daemon sees every keystroke (`pumpInput`) and owns the only writer of the mirror pane's screen: the `outputSink` pump goroutine serialises every seed, output and resize frame to the renderer, which writes the bytes to its pty verbatim. So a prediction can be painted by the pump as extra bytes in the renderer's output stream. No renderer, wire-protocol or local-tmux change is needed. Painting is local; nothing extra goes to the remote.

The obstacle is **repair**. The local pane is a terminal the daemon cannot read back. To erase a wrong prediction, the daemon must know what the cells under it held and what cursor and pen the remote stream expects. That needs a model of the pane.

- `github.com/charmbracelet/x/vt` (already a dependency, used by `agentdetect/screen`) gives one: cursor, cells with styles, the alt screen, modes, and handler hooks.
- **It is too slow to run on every byte.** Measured in this worktree (Ryzen AI MAX+ 395): 2.5 MB/s on scrolling output, 26 MB/s without scrolling. A `cat` of a large log through a model-on-everything pump would be CPU-bound in the daemon.
- So the model is **lazy**. It exists only while someone is typing into that pane, prediction is wanted, and output stays light (below).

## Design

### Model lifecycle (per pane, pump goroutine only)

- **Born only from a model seed, which never reaches the renderer.** A key typed while prediction is wanted (rule below) and no model exists sets the sink's `modelWanted` flag. A new main-loop step, `seedPredictModels`, runs beside `reseedReshaped`, the only place a round trip may run. It applies `takeDirty`'s gates (not closed, not paused, drained) and issues, per flagged pane and in this order, `cursorCmd`, one more `display-message` (`predictModesCmd`) and `captureCmd`. Every command is written before any reply is read, the `PaneSeeds` pattern.
  - **The capture is each pane's last reply, and its `framePredictSeed` is enqueued the moment that reply is parsed**, before the next pane's replies are taken off the stream. `roundTrip`'s iterator routes `%output` into sinks while it walks past reply blocks. With any reply after the capture, output landing between them would reach the renderer before the model existed and never reach the model.
  - Reading the modes before the capture is the same skew `cursorCmd` already accepts.
  - A request is made at most once per 2s per pane, and the keys that triggered it are not predicted.
  - A test interleaves `%output` between the batch's replies and asserts the model ends up with it.
  - `predictModesCmd` reads `|`-delimited `#{cursor_flag}|#{insert_flag}|#{origin_flag}|#{wrap_flag}|#{scroll_region_upper}|#{scroll_region_lower}`. It is its own command, not fields appended to `cursorCmd`, because `cursorCmd`'s last field, `#{pane_private_modes}`, expands empty and `strings.Fields` drops it, which would shift anything after it.
  - The result is enqueued as an internal `framePredictSeed`: the `render.Seed` bytes plus the parsed modes. The pump builds a `vt` emulator at the pane's last `FrameResize` dims, feeds it the seed bytes, then applies the remote modes in the model only:
    - `?25l` if the cursor is hidden, `4h` if insert mode is on, `?7l` if autowrap is off;
    - DECSTBM `t;b r` followed by a CUP back to the cursor (DECSTBM homes the cursor);
    - `?6h` last if origin mode is on (DECOM homes the cursor; prediction is refused under DECOM anyway).
  - A missing or non-numeric field, unknown dims, or a capture error builds no model.
  - **Nothing is written to the renderer**, so the local pane's screen and history are untouched. A real `FrameSeed` writes `ESC[2J` on the main screen, and tmux's `scroll-on-clear` pushes the whole visible screen into the mirror's scrollback; routine prediction seeds must not do that.
  - The capture reply follows every `%output` routed before it, and nothing is routed into the sink between that reply and the enqueue (the `PaneSeeds` contract). So the model describes the pane at the same point of the pane's byte stream that the local pane has reached.
- **Kept current from then on.** A live model is fed every seed and output byte the pump writes to the renderer. It therefore follows the *local pane*: frames lost to a full buffer or a `%pause` are missing from both alike. A real `FrameSeed` feeds an existing model, after dropping every prediction, but never builds one, because it carries none of the modes above.
- **Dropped** (model set to nil) when any of these happens. The next typed key requests a fresh model seed.
  - A `FrameResize` changes the pane's dims. Local tmux reflows the pane its own way, and every reshape is followed by a reseed anyway.
  - More than 64 KiB of output has been fed since the last key. Heavy output never runs through the emulator for more than one budget.
  - No key for 30s. Idle panes cost nothing.
  - The emulator panics on remote-controlled bytes. A recovered panic drops the model; a pre-1.0 parser must not take the daemon down.
  - The remote emits any REP (`CSI n b`). vt prints the char n times, n up to 65535, so a few remote bytes would cost the pump seconds. tmux (next-3.9, measured) caps it at the line end, prints nothing over a pending wrap, and prints nothing once any C0 control or CSI followed the char; the model tracks none of that, so every REP poisons it. A main-screen prompt essentially never emits REP.
- The emulator's reply pipe (DA, DSR, DECRQM answers) is drained by a goroutine that is stopped without `Emulator.Close`. `Close` races the reader on vt's unsynchronised `closed` flag, as `agentdetect/screen` documents. Stopping it means closing a stop channel and writing one byte to `InputPipe()`, which wakes the reader so it sees the channel.

### When a prediction is wanted (gating)

Option `@og_bridge_predict`: `auto` (default; unset reads as auto), `on`, `off`. `og-remote-open` reads it once at launch (`show-options -gqv`) and exports `OG_DAEMON_PREDICT`. The daemon takes `--predict` (env default, validated to the three values; anything else is a startup error).

- `off`: no predictor at all. Sinks are built exactly as today and pay no cost.
- `auto`: wanted only while the measured echo latency is high. Each pane samples latency with no model: the time from a key frame reaching the pump to the next output frame for that pane. A sample over 1s is discarded, since it is a key that never echoed (a password, `read -s`) timed against unrelated later output. The daemon keeps the last 8 samples across all panes (mutex-guarded), and the estimate is their **median**, so a few polluted samples in either direction do not move it. Wanted turns on above 30ms and off below 20ms (hysteresis), and starts off. On a LAN the model is never built.
- `on`: always wanted. Used by tests and by users who want it regardless of latency.

### Which keys

`pumpInput` hands each delivered chunk (after paste handling, before `send-keys`) to the sink as an internal key frame. The frame goes through the same channel as output, so a key is ordered before its own echo. The predictor classifies byte by byte:

- **Printable ASCII (0x20–0x7e)** is a character prediction at the predicted cursor. The cell is set to the char and the cursor moves one right.
- **Backspace (0x7f, 0x08)** is an erase prediction: the cell left of the predicted cursor becomes blank and the cursor moves one left. This applies only within the current run, i.e. the predicted cursor is right of where this epoch's first prediction began. It never erases the prompt.
- **Anything else** is a barrier: CR, LF, Tab, every other control byte, ESC and every escape sequence (arrows, mouse and focus reports, bracketed-paste markers), and non-ASCII bytes. The epoch is bumped, the rest of the chunk is not predicted, and no new prediction is made until every outstanding one has resolved.

The issue suggests gating on application cursor mode (DECCKM). That is deliberately not done: oh-my-zsh's `zle-line-init` runs `echoti smkx`, so DECCKM is on at a common shell prompt, and the gate would disable the feature there. Full-screen apps are excluded by the alt-screen and cursor-hidden gates plus confirmation instead.

### Adaptive display (epochs, after mosh)

Every prediction carries the epoch current when it was made. A prediction is **painted only if its epoch has been confirmed**, i.e. some prediction of that epoch or a later one has matched the remote's echo. The first key after any barrier, seed or misprediction is tentative: recorded, not painted. Once its echo confirms it, the rest of that epoch's run is painted immediately. Consequences:

- A password prompt never echoes, so it never confirms and nothing is shown. The prompt follows Enter, which is a barrier.
- In `less`, a TUI keybinding, or `read -s`, keys are not echoed, so nothing is shown.
- Typing at a shell prompt shows predictions from the second character of each run.

### Confirmation, misprediction, expiry

Before each output frame is fed to the model, the model's cursor and the cell under every outstanding prediction are snapshotted. After the feed, the outstanding predictions are judged against the model, using cursor position as the "has the remote processed it yet" signal. A char is *standing* in a run when no later prediction in the run touches its cell; an erase is never standing.

- **Shortest proven prefix first.** The predictor settles the shortest prefix of the outstanding predictions that the frame provably completed. Three things must hold. The model cursor is where the prefix's last prediction leaves it. Every standing char of the prefix is in its cell. And the frame changed something: the cursor moved, or a standing char's cell differs from the snapshot. Keys that net to nothing, such as a char erased again, leave the screen exactly as it was before them, so only the shortest matching prefix is proven; a longer one would settle keys whose echo may still be in flight.
- **An erase settles only inside a matching prefix.** Erased cells are not compared, because zsh-autosuggestions refills an erased cell with its grey suggestion. A prefix confirms its epoch only if it leaves a typed char standing, so a silent prompt (a password, `read -s`) never confirms, even when its keys net to a blank.
- **Head-by-head fallback.** With no prefix matching, the head prediction is checked alone, repeatedly. A char at (x, y) is confirmed when the cursor is on row y past x and the cell holds the char. A cursor at or left of x, on another row, or a head that is an erase leaves it pending, and everything after it: the fallback never judges an erase. This tolerates fish/zsh redraws, where mid-redraw the cursor sits at column 0 or on a row of a multi-line prompt (p10k, `reset-prompt`). A head char whose cell holds something else but which a later outstanding key overwrites also stays pending.
- **Wrong** only on proof: some prefix ending at the model cursor, in a frame that changed something, has a standing char whose cell holds something else, and no later outstanding key overwrites that cell; or the head-by-head check finds such a cell. Every outstanding prediction is dropped and the epoch is bumped, so the next run is tentative again. Repair does not depend on the verdict: unpaint always restores from the model.
- **A barrier's keys are dropped without a verdict.** When no prefix settles and the oldest outstanding prediction's epoch is older than the current one (a barrier such as Enter followed it), the predictions are dropped without touching the epoch. Their verdict could no longer confirm anything, and a cursor the barrier moved off their row would leave them painted there until expiry.
- **Expired:** the oldest prediction is older than `clamp(4×estimate, 500ms, 3s)`, driven by a timer in the pump's select. If the outstanding predictions already draw the model (the cursor is where the last leaves it and every standing char is in its cell), they are keys that net to nothing echoed in one payload, indistinguishable from keys not yet echoed. They are unpainted and dropped quietly, with no epoch bump, provided some output arrived since the last key; with none, a no-echo prompt looks the same, so the epoch is bumped. Otherwise every outstanding prediction is unpainted and dropped and the epoch is bumped.

### Painting and repair (exact, model-based)

While predictions are painted, the local pane shows *model + overlay*. Painting uses DECSC/DECRC (ESC 7 / ESC 8), which in tmux save and restore cursor position, pen (attrs, colours, link), charset and origin mode. Verified on a scratch tmux next-3.9 server: a red pen survives a dim paint between ESC 7 and ESC 8, and `CSI s` shares ESC 7's slot.

- **Paint:** ESC 7 (the slot holds the real cursor and pen), then for each painted prediction a CUP to its cell, `ESC[0;2m` (dim) and the char (a space for an erase). Finally a CUP to the predicted cursor. The local cursor then sits after the predicted text.
- **Unpaint**, before anything else is written to the renderer (output, a seed, the gfx replay), and on wrong/expiry/reset: ESC 8 ESC 7 (real cursor and pen back, slot unchanged). For every painted cell, a CUP, the model cell's style (`uv.Style.String()`) and its content. Then ESC 8. The model has not changed since the paint, because unpaint runs before the next feed, so this restores the cells exactly.
- **Only at a sequence boundary.** A paint is written only when the renderer's byte stream so far ends in ground state: not inside an escape sequence, an OSC/DCS/APC string, or a split UTF-8 rune. `keyneg` does not hold those back, so a frame can end inside one. A small tracker observes every byte written to the renderer. Unpaint only follows a paint, so it inherits the guarantee. The unpainted cell's style is preceded by an SGR reset, because `uv.Style.String()` carries none.
- **Output frame:** `unpaint + payload + paint(remaining)`, written as one `FrameOutput`. Confirmed predictions vanish because the remote's own bytes now occupy their cells. Wrong ones are restored from the model.
- **Seed:** unpaint if painted, drop every prediction, bump the epoch, then the gfx replay and the seed as today.
- **Resize changing dims:** drop every prediction and the model. If painted, emit only ESC 8 (cursor and pen back). The cell coordinates are stale after a local reflow, and the reseed that follows every reshape repaints the cells.
- **`%pause`:** a key arriving while the sink is paused resets the predictor (unpaint, drop) and predicts nothing. Output written while paused is dropped, so no echo could confirm a prediction, and the `%continue` seed resyncs.
- **Key frames have their own non-blocking enqueue.** A key frame that finds the channel full is dropped and sets only an atomic `keysLost` flag; the predictor resets at its next event. It never touches `dropped`, which would cost a full `reseedDropped`.

### Paint preconditions

All of these must hold, or the predictor resets (unpaint + drop):

- The model exists and the pane is not paused.
- The main screen is active (`IsAltScreen` false). Without this, nvim, fzf and htop would be predicted into.
- The cursor is visible. Ink TUIs such as Claude Code hide it. The model seed sets it from `#{cursor_flag}`, and vt's `CursorVisibility` callback tracks it after that.
- Insert mode (IRM) is off, or a paint would shift cells. Origin mode (DECOM) is off, or CUP would be scroll-region relative. Both are seeded from `#{insert_flag}`/`#{origin_flag}` and tracked through vt's `EnableMode`/`DisableMode` callbacks.
- **The app has not used the save-cursor slot** (ESC 7 or `CSI s` seen by this model, via registered handlers that return false so vt's defaults still run). This is sticky for the model's lifetime. A paint would clobber the app's saved cursor. The cost is that prediction never shows for prompts that use sc/rc.
  - Restoring cursor and pen explicitly instead of through DECSC/DECRC was considered and rejected. vt@2cc9a8fe1146 does not export the pen (the `Emulator` exposes only `CursorPosition`; `Screen.Cursor()` belongs to the unexported active screen). Mirroring it alongside vt means re-implementing SGR, DECSC/DECRC, `?1048`/`?1049`, DECSTR and RIS pen semantics. A wrong pen there would restyle the remote's real output persistently, while the slot clobber below is a rare, local-only cursor misplacement.
- Each predicted cell is a width-1 model cell, not part of a wide character. Every predicted cursor stays at x ≤ width−2, so a paint never reaches the last column or leaves tmux in pending-wrap state, which DECRC would not restore. Reaching it blocks further predictions until the outstanding ones resolve.

### Accepted limitations

- A hyperlink (OSC 8) on a cell under a prediction is not restored by unpaint; the cell keeps its text and style.
- tmux has no format for the save-cursor slot, so an app that saved its cursor *before* the model seed and restores it after a paint lands at the daemon's saved cursor locally, until its next repaint.
- Charset designations (`ESC ( 0`) are not seeded: a predicted ASCII char under DEC line drawing renders as a line glyph until repaired.
- The first key of every run is not predicted.
- A model built on the alt screen stays until it is dropped (budget or idle), never predicting.
- The option is read at launch: changing `@og_bridge_predict` affects the next `og-remote-open`.
- A Home Manager option for it is not added. The tmux option is the switch, set from `extraConfig`.

## Acceptance mapping

- **AC1:** a predictor test feeds a "local pane" reference `vt` emulator with every byte the sink writes. A predicted char sits dimmed at the cursor before the echo; after the echo the cell holds the remote's style (not dim) and the cursor is where the remote put it.
- **AC2:** the same harness with the model seed on the alt screen draws no prediction bytes. So does a main-screen seed whose remote reports the cursor hidden (`#{cursor_flag}` 0).
- A model seed writes nothing to the renderer, so the local pane's history does not grow.
- **AC3:** mispredictions (no echo then expiry; a different char; a redraw elsewhere; backspace refused by the shell) end with the local emulator equal, cell for cell plus the cursor, to a reference fed only the remote stream. A seed or dims-changing resize drops outstanding predictions.
- **AC4–6:** `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Docs

`docs/agents/bridge-daemon.md` gets a "Predictive local echo (#972)" section with the lifecycle, gates, paint protocol and the invariant that every pump write is preceded by unpaint. `docs/agents/scripts.md`'s `og-remote-open` row gets the new option.
