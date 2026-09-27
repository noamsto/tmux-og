# Performance: keystroke latency and the tmux server thread

tmux runs every command, hook, format expansion and `run-shell` spawn on one
server thread. While that thread is busy, keystrokes to every pane wait. This
doc covers how the lag is measured, what it measured (#793), and the
regression guards.

## The probe and the harness

- **`picker/latencyprobe`** is a control-mode client. It writes
  `send-keys -t <pane> -l q<seq>z` and times how long that marker takes to
  come back in the pane's `%output`. The target pane runs `cat`. This is the
  bridge daemon's own path: it types through `send-keys` on a control client
  and reads echoes back as `%output`. It prints
  `label n= timeouts= p50= p95= p99= max=`, and with `-slow` it logs each
  slow sample's send time to stderr, so a stall can be matched against a
  `tmux -v` server log.
- **`tests/perf/keystroke-latency.sh [scenario…]`** builds a loopback bridge:
  1. **Remote** server `rem`, shaped like halo:
     - session `work`: a `cat` pane, 7 synthetic agent windows (1.5 KB of
       output plus an OSC title change every 50 ms), 3 three-pane
       `@crew_grid` windows and a window with a stamped float;
     - session `work2`: 4 more agent windows.
  2. **Bridge daemons**, one per remote session, run with `--test-local` and
     the raw tmux on their PATH, as `og-remote-open` gives them.
  3. **Local** server `loc`, holding the mirrors, with a real status-drawing
     client attached from a third server.

  The probe types into the local mirror of remote `%0`.

  Scenarios:

  | Scenario | What runs during the probe |
  | --- | --- |
  | `quiet` | No agent load |
  | `busy` | The full load, steady state |
  | `churn` | The full load, plus a remote window added and killed every 0.6 s |
  | `attach` | The probe starts as the first mirrored pane appears. Also prints the time until the whole mirror exists |
  | `reattach` | The full load, plus a second 180x45 client attaching to the existing mirror for 2 s and detaching, three times |

  Environment:

  | Variable | Effect |
  | --- | --- |
  | `TMUX_BIN` | Wrapped `tmux` under test (default `./result/bin/tmux`). Run once per build to compare |
  | `SAMPLES` | Probe sample count (default 400) |
  | `VLOG=1` | Starts `rem` under `tmux -v` and reports forked refit jobs after `busy` and `reattach` |

  The harness is timing-dependent and loads the machine, so it is not part
  of `nix flake check`. Run it by hand:

  ```bash
  nix build .#default
  TMUX_BIN=./result/bin/tmux tests/perf/keystroke-latency.sh
  ```

- To locate a stall, probe the local mirror pane, the remote pane directly
  and a plain local `cat` pane **at the same time**:
  - only the mirror slow → the daemon;
  - the remote pane slow too → the remote server thread;
  - the plain local pane slow → the local server thread.

  Then start the suspect server with `-v` and look at its log around the
  slow sample's timestamp. Count `job_run` lines (two per job) and
  `hooks_insert_event: hook=` lines to see what forked and which hooks
  fired.

## What dominated: the resize-hook fork storm (#793)

Every `window-resized` event ran `run-shell -b tmux-float-refit` and
`run-shell -b tmux-grid-refit`, and every `window-layout-changed` event ran
`tmux-grid-refit`. That is three forked bash scripts per window per event,
and each script calls back into tmux.

tmux-next's `refresh-client -C @N:WxH` recalculates every window on the
server, and fires both hooks even for windows whose size did not change. The
verbose log shows it: `recalculate_size: @1 is 200x49 … new size 200x49`,
then `resize_window`. The bridge daemon sends one per-window cap for each
mirrored window whenever the mirror's size changes.

So one size change costs (windows in the mirror) × (windows on the server)
events, times three forks. With 12 windows that is 432 forks. In a 20 s busy
run on the remote, the refit hooks forked ~1 170 jobs, against ~130 for
everything else combined.

The mirror's size changes on:
- attach;
- a terminal of a different size attaching to an existing mirror;
- any window add or remove that changes how many status rows the local bar
  needs.

These are the moments the lag was reported.

The fix gates the three hooks in `config/tmux.conf.tmpl` with `if -F`,
evaluated in-process with no fork:

- **Float gate.** Runs only when some floating pane carries `@float_geom`
  and its pane-scoped `@float_refit_size` differs from the window's current
  `WxH`.
- **Grid gate.** Runs only when `@crew_grid=1`, the window is not zoomed
  (zoom is user state, mirroring the script's own zoom exit), and the
  window's decision signature — every input the layout decision reads, not
  just geometry — differs from `@grid_refit_layout`.

Each script stamps the state it last verified. Crew grids needed their own
gate because they are the busy windows: every dispatched worker window is
one. `scripts.md` has the stamp rules. The signature is not
`#{window_layout}`, because tmux-next renders that per client.

**Why a stale stamp cannot hide a real change:**
- A change's hook item runs on the global queue, or right after the command
  that caused it. `server_loop` drains the global queue on every pass.
- A script needs at least one tmux round-trip between reading a state and
  writing its stamp.
- So every change is gated against the stamp as it stood before that
  change.

### Before / after

Measured with the harness: two rounds per build, 400 samples each.
"Before" is `main` 2fb6833, which carries #760's `window-layout-changed`
grid hook, so three forks per window per event. "After" is this change as shipped. Halo's live config predates
#760 and runs two. Ranges cover both rounds; times are ms.

| Scenario | Before p50 | Before p95 | Before p99 | Before max | After p50 | After p95 | After p99 | After max |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| quiet | 0.7 | 1.5–1.6 | 3.7–4.9 | 4.2–7.1 | 0.5–0.9 | 1.2–1.4 | 3.2–3.4 | 4.2–5.0 |
| busy | 0.4–0.5 | 5.8–6.0 | 12.4–12.7 | **871–978** | 0.4–0.5 | 5.5–5.9 | 10.5–12.5 | 167–207 |
| churn (window add/remove) | — | — | — | aborted | 0.7–1.0 | 12.9–14.3 | 40.5–41.5 | 68–170 |
| attach (fresh mirror) | 0.4 | 10.1–14.3 | 56–72 | 285–316 | 0.4 | 6.4–6.6 | 32.9–40.0 | 94–105 |
| reattach (terminal onto existing mirror) | 0.5 | 16.7–19.7 | **1243–1345** | **1448–1534** | 0.4 | 11.9–14.2 | 116–164 | 214–237 |

**Churn before the fix.** In both rounds the probe aborted after three
consecutive 5 s timeouts, having recorded stalls of 1.0–1.2 s. The daemon
lost the mirror pane under the storm and logged `%0: dropped 1 output
frame(s) (pane is gone)`, so echoes stopped arriving at all. After the fix
neither round aborted.

**Attach.** The whole 12-window mirror fills in 1.6 s before the fix and
1.0–1.1 s after.

**Forked refit jobs** (`VLOG=1`, one run, whole chain lifetime including
setup):

| Scenario | Before | After |
| --- | --- | --- |
| busy | 2 071 | 188 |
| reattach | 6 427 | 429 |

**What the "after" residual is.** A *real* resize, such as a client of
another size attaching, still refits every grid and stamped float, as it
should. The daemon's per-window caps then fire a burst of redundant events
for those windows. Events that arrive before the first forked run has
written its stamp see the old stamp and fork too. Coalescing that burst is a
follow-up. Plain windows fork nothing.

## What did not matter (measured)

| Suspect | Measured |
| --- | --- |
| Busy pane output and title churn alone | Remote-direct p99 0.6 ms |
| `-B` monitor ticks | 5 jobs per 5 s |
| `tmux-reflow-windows` | 2 batched tmux calls, ~7 ms, off the server thread |
| Local `#()` status jobs | Plain local pane p99 3–10 ms with a status client attached |
| `#()` jobs on a bridge-only host such as halo | Never run: control clients draw no status line |

`session-window-changed` runs reflow and `mark-seen` in the foreground. That
adds ~6 ms to a `select-window` on the same client. It was left alone: the
foreground run is deliberate ordering.

## Residuals (follow-ups)

- **Daemon window reconcile**: ~40–65 ms end-to-end, once per mirrored
  window add or remove. Both tmux servers stay clean during it (remote max
  14 ms, local plain pane max 19 ms), so the cost is inside the Go daemon:
  `%output` delivery waits behind reconcile work. Keystroke sends do not;
  they go straight to the stream.
- **The Nix `tmux` wrapper costs 32.5 ms per invocation**, against 1.3 ms
  for the raw binary. It is bash doing one PATH dedupe-and-prepend block per
  entry, 66 of them. Scripts the server runs resolve the raw binary first on
  PATH, and so does the bridge daemon, so hot paths never pay this. Only
  external callers do: shells in panes, Claude Code hooks, third-party
  tools. That is client-side CPU, never the server thread.
- **Upstream fan-out.** `refresh-client -C @N` recalculates every window on
  the server. With our forks gone this measured cheap, so it is documented
  here, not filed upstream.

## Regression guards

- `tests/grid-refit.bats` (`grid-refit-tests`, part of `nix flake check`)
  sources the production refit hook lines into a `tmux -v` server. It drives
  a same-size storm (per-window `refresh-client -C` from a control client)
  and counts forked refit jobs by script basename. The count must be zero
  for:
  - plain windows;
  - an already laid-out grid, including after a settle interval;
  - a stamped float.

  Real resizes, the stale-stamp float sequence and the #760 layout-change
  cases must still refit.
- A new `window-*` hook that forks per event multiplies under this fan-out
  the same way. Gate it in-process, or measure it with the harness.
