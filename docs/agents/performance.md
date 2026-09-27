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

- **Pending marker (#810).** Both gates also skip while the window's
  `@float_refit_pending` / `@grid_refit_pending` equals the live `WxH` /
  signature. The hook writes the marker right before it forks; the script
  clears it in its first tmux call (below).

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

**What the "after" residual was.** A *real* resize, such as a client of
another size attaching, still refits every grid and stamped float, as it
should. The daemon's per-window caps then fire a burst of redundant events
for those windows. Events that arrived before the first forked run had
written its stamp saw the old stamp and forked too. #810 coalesces that
burst (below). Plain windows fork nothing.

### Coalescing a resize burst (#810)

Each gated hook writes a window-scoped pending marker right before its
`run-shell -b`: `@float_refit_pending` = `WxH`, `@grid_refit_pending` = the
grid signature. The gate also skips while the live state equals the marker.
So the rest of the burst skips while the first run is on its way.

The script clears the marker **in its first tmux call**, in the same client
command list as its state read (`tmux-grid-refit`'s snapshot
`display-message`, which also carries the `@crew_grid` and zoom reads;
`tmux-float-refit`'s `list-panes`). Commands in one command list run
back-to-back on the server thread, so no gate is evaluated in between.

**Why a marker cannot hide a needed refit:**
- An event is skipped only while the marker is set, and it is set only
  between a hook's fork and that run's first tmux call.
- So every event the marker suppresses predates everything that run reads.
  The run acts on a state at least as new as the event.
- Events after the clear are gated by the stamp exactly as above.
- Early exits (zoomed, not a grid, no lead, lock loser) all come after the
  clear. A zoomed run leaves nothing behind, so the unzoom's own
  `window-layout-changed` forks normally.
- A lock loser still drops its work, as before #810. Coalescing removes the
  redundant burst forks that sometimes retried once the holder released, so
  a loser that read a new state is likelier to stay unrefit until the next
  event.

**Keyed on the state**, not a flag: a marker left behind — only possible
when the run is killed before its first tmux call, or `run-shell -b` fails
to fork — blocks events for that exact state only. Any other state forks,
and its run clears it.

What still forks: events that land after a run's read and before its stamp.
On the grid's fast path (#816) the lead lookup and the three `read_opt`s
(`@crew_grid_main_pct`, `@grid_refit_min_role_cols`, `@grid_refit_aspect`)
are folded into the one snapshot `display-message` — the lead's pane id via
a `#{P:#{?cond,#{pane_id} ,}}` iterator reusing the same lead-boolean the
list-panes filter used, the three option values via `#{s/[|]/ /:@opt}`
(`|`-safe, matching `tmux-update-icons.sh`'s same idiom; an unset option
still renders empty, so the existing default-fallback checks need no
change). So the grid path's only remaining gap is the stamp `set-option`
itself, which cannot be folded into the read it depends on. The float
path's per-pane stamp is unchanged.

A `set-option` of a user option fires neither `window-resized` nor
`window-layout-changed` on tmux-next (probed), so the marker writes and the
clear do not re-trigger the gates.

**Measured** (`VLOG=1`, `busy` + `reattach`, two rounds per build,
alternating, same session; "before" is `main` a9e2ceb with #807, "after" is
#810). The host was noisier than during #807's run, so compare the columns
with each other, not with #807's table. Times are ms.

| Scenario | Before p50 | Before p99 | Before max | After p50 | After p99 | After max |
| --- | --- | --- | --- | --- | --- | --- |
| busy | 1.8–2.6 | 13.9–19.9 | 23.8–311 | 1.9–3.6 | 17.1–39.0 | 241–259 |
| reattach | 7.7–7.9 | 370–388 | 478–484 | 4.3–6.0 | 237–255 | 292–510 |

| Forked refit jobs | Before | After |
| --- | --- | --- |
| busy | 25–234 | 24–29 |
| reattach | 600–666 | 56–65 |

Re-attach forks about 10× fewer refit jobs, and its p99 drops by about a
third. Max stays noisy in both builds: one "after" round hit 510 ms. The
busy forks vary widely before (one round hit a burst: 234 jobs) and stay
flat after; its latency spread is noise.

**Measured (#816)** — folding the grid path's lead lookup and three
`read_opt`s into the snapshot, narrowing the gap above. `VLOG=1
tests/perf/keystroke-latency.sh reattach`, two rounds per build, alternating
in one session, under a `systemd-run --user --scope -p CPUQuota=400%` cap
(other workers were on the shared host); "before" is `main` e78655a (#815),
"after" is this branch.

| Forked refit jobs | Before | After |
| --- | --- | --- |
| reattach | 68–70 | 61–66 |

A modest reduction, as expected for a race-window narrowing rather than a
closure: the grid path's only remaining gap is the final stamp
`set-option`, which cannot be folded into the read it depends on, so a
residual (now narrower) window remains alongside genuine resize-driven
refits.

## Daemon window reconcile (#808)

After #793, churn's remaining tail was the Go daemon. A mirrored window add
or remove stalled keystroke echo ~40–65 ms end to end while both tmux
servers stayed clean.

**Where it went.** Temporary tracing (not shipped) stamped each `%output`
line as the pump read it and again as `Router.Route` delivered it, and timed
every main-loop phase and local exec. On the baseline, waits for `%0` reached
95 ms: 21 samples over 10 ms and 14 over 15 ms in one churn run. Every long
one sits inside `addWindow`'s two local exec runs:
- `new-window` (23–51 ms), then five `set-option` and a `rename-window`;
- the shaping execs `resize-window`/`select-layout`, `list-panes`, the zoom
  `if`, `respawn-pane` and two `set-option`.

Round-trips and the hello wait never stalled: they route as they read. An
exec blocks the stream's only reader.

**Fix.** Window-set operations now run their execs through `routeWhile`,
which keeps reading the stream while the exec runs. Mechanism and scope are
in `bridge-daemon.md`.

**Before / after.** `churn`, 400 samples, run interleaved A/B under
`systemd-run --scope -p CPUQuota=400%` on a shared 32-core host. The
comparison is only valid within this table: the uncapped #793
figures above are a different setup. A is `main` a9e2ceb, B is this change.
Both use the same tmux. Times are ms; load is the 1-minute average at start.

| Pair | Load A / B | A p50 | A p95 | A p99 | A max | B p50 | B p95 | B p99 | B max |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 5.2 / 7.6 | 1.3 | 16.3 | 53.5 | 67.9 | 0.9 | 6.9 | **17.1** | **19.5** |
| 2 | 7.8 / 5.9 | 1.1 | 19.3 | 58.7 | 116.1 | 0.8 | 7.7 | **14.6** | **20.5** |
| 4 | 8.2 / 7.1 | 1.0 | 15.8 | 74.5 | 109.0 | aborted | | | |
| 5 | 6.9 / 8.4 | 0.9 | 16.8 | 58.9 | 157.5 | 1.0 | 10.7 | **15.2** | **23.2** |
| 6 | 7.5 / 9.5 | 0.9 | 15.6 | 67.4 | 175.9 | 0.8 | 9.8 | **15.6** | 211.3 |
| 7 | 7.6 / 7.6 | 0.8 | 13.7 | 49.9 | 69.8 | 0.9 | 13.1 | 57.0 | 226.4 |

Pair 3 aborted on both builds, and pair 4's B aborted. Every abort is the
`%0` rebuild listed under Residuals, which `main` shares.

B's p99 fell in 4 of the 5 pairs where both runs completed. Pair 7, where
B's p99 was higher, and B's two 200 ms maxes are not the daemon. Two
traced B runs had these probe results:
- p99 18.2 and 18.7 ms, max 29.5 and 40.3 ms;
- no `%0` wait over 15 ms at all (baseline: 14);
- a worst wait of 11.7 ms, every one of them over 10 ms inside a
  labels/agents/res flush's `set-option`.

The rest of the probe's tail is spent outside the daemon's routing, in the
tmux servers or the host.

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

- **Labels/agents/res flushes.** Their `set-option` execs still hold
  `%output`, up to ~12 ms per pass (traced below). They stay on the plain
  `Config` on purpose: they run after the pass's `settle`, and a
  notification a routed exec queued there would wait for the next stream
  line (`docs/agents/bridge-daemon.md`).
- **Churn aborts on a `%0` rebuild.** The first churned window add sometimes
  makes the daemon drop and rebuild the mirror of `%0` (`%0: dropped 1
  output frame(s) (pane is gone)` after a failed `%24` reseed). The mirror
  pane then has a new local id, so the probe times out and aborts. `main`
  does it too.
- **Fixed (#809): the Nix `tmux` wrapper's `--prefix PATH` now names one
  merged bin dir** (`wrapperBinDir` in `config/tmux.conf.nix`, a `symlinkJoin`
  of the same packages) instead of 66 separate packages. `make-wrapper.sh`'s
  `addValue` runs one PATH dedupe-and-prepend block per colon-separated
  segment of the `--prefix` value, so 66 packages cost 66 blocks of runtime
  bash; one merged dir costs one. Measured 32.5 ms → 2.8 ms per invocation
  (raw binary: 1.1 ms), on halo via
  `hyperfine -N 'result/bin/tmux -L x display -p x'`. `symlinkJoin`'s `paths`
  order preserves precedence (first path wins a name collision via `lndir`),
  so `tmuxPkg` stays first, same as the old `--prefix` order. Scripts the
  server runs resolve the raw binary first on PATH, and so does the bridge
  daemon, so hot paths never paid this either way — only external callers
  did: shells in panes, Claude Code hooks, third-party tools. That is
  client-side CPU, never the server thread.
  `checks.wrapper-bin-merge-assertions` (`nix flake check`) guards that every
  binary reachable on the old per-package PATH still resolves to the same
  target through the merged dir, and that `tmux` resolves to `tmuxPkg`'s
  pristine binary first.
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
- The #810 tests in the same file pin the pending marker: a burst on an
  unverified grid or a stale float forks exactly one job; a lock loser, a
  zoomed run never block a later refit, and a left-behind marker never blocks
  a different state (it still blocks its own exact state, above).
- A #827 test forces the apply branch's own peer-read race deterministically:
  a `tmux` wrapper placed on the server's `PATH` before it starts delays only
  a `set-option` call carrying `@grid_refit_sig`, which the pre-#827 apply
  path issued as a separate command. `make_grid 1` makes
  `select-layout` the apply's only layout-mutating command, so its hook
  forks a peer while that delayed write is still pending. Red on the
  pre-#827 separate-calls apply (the peer reads the stale sig,
  lands on the apply branch too, and — since only the fast path stamps
  `@grid_refit_layout` — leaves it unset with nothing left to trigger a
  later confirming run); green on the branch, where the wrapper's pattern
  never matches because the sig write is bundled into the same command list
  as `select-layout`, so no delay is ever injected.
- A new `window-*` hook that forks per event multiplies under this fan-out
  the same way. Gate it in-process, or measure it with the harness.
- **#808's `%output` routing during window-set execs**:
  - `tests/remote-m2-integration.bats`, "window add keeps live output flowing
    while the local new-window is slow": a `tmux` wrapper stalls the
    daemon's local `new-window` by 4s, and the mirror must still paint live
    output within 2s;
  - `routedexec_test.go` pins `routeWhile`'s ordering (park-and-stop, ordinal
    accounting, EOF).
