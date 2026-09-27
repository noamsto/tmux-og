# Typing lag on a busy session: the resize-hook storm (#793)

## Problem

On a busy session (halo: ~11 agent windows, mirrored to a laptop over the
remote bridge), typed text lags, worst on **attach** — both a fresh
`og-remote-open` and attaching a terminal to an already-mirrored session — and
when windows are added or removed.

Halo's live server has only control-mode clients (one `tmux -C attach` per
mirrored session, from the laptop's bridge daemon), so `#()` status jobs never
run there; every keystroke is a daemon `send-keys` on halo and an `%output`
back.

## Measurement (done before this spec)

A keystroke-echo probe (Go, control-mode client: `send-keys -l <marker>` into a
`cat` pane, time until the marker comes back in `%output`) was run on scratch
servers built from this branch's `nix build .#default`:

- **chain**: remote server `rem` (session `work`: a `cat` pane + 11 synthetic
  agent windows printing ~2 KB/50 ms with title churn) → the real bridge
  daemon (`--test-local`) → local server `loc` (session `mirror`) with a real
  status-drawing client attached from a third server. The probe types into the
  local mirror of the remote `cat` pane (end-to-end), and in parallel into the
  remote pane directly and into a plain local pane (to locate a stall).

Findings, ranked by measured contribution (preliminary: the "gated" figures
below used an early `@crew_grid`-only gate on plain windows; the PR's
before/after table is re-measured with the final stamp gates on the
halo-shaped load):

1. **Resize-hook fork storm (dominant, ~0.7–1.1 s stalls).** Every
   `window-resized` fires `run-shell -b tmux-float-refit` and
   `run-shell -b tmux-grid-refit`, and every `window-layout-changed` fires
   `run-shell -b tmux-grid-refit` — three forked bash scripts per window per
   event, each calling back into tmux, although both scripts no-op on a
   window that is not a crew grid / has no stamped float (i.e. almost every
   window). tmux-next's `refresh-client -C @N:WxH` recalculates **every**
   window **on the server**, every session, not just the mirrored one
   (verbose log: `recalculate_size: @1 is 200x49 … new size 200x49` →
   `resize_window`, even when unchanged), and the daemon sends one per
   mirrored window whenever the mirror's height changes. One height change is
   therefore (windows in the mirror) × (windows on the server) resize events
   plus as many layout events, three forks each (12 windows → 432 forks in
   one batch; halo today has 35 windows across 7 mirrored sessions). In a 20 s busy run the remote forked ~1 170 refit jobs vs
   ~130 of everything else, plus 784 `command-error`s from their option reads.
   The mirror height changes whenever the local status bar gains/loses a row
   (a window add/remove, a client of another size attaching).
   - Remote-direct probe with the daemon attached: max 646–691 ms → 2–4 ms
     with the refit hooks removed.
   - Re-attach to an existing mirror (second 180x45 client attach/detach ×3):
     p99 768–855 ms, max 0.94–1.13 s → with `if -F`-gated hooks p99
     16–18 ms, max 27–31 ms.
   - Remote window add/remove churn: max 721–765 ms → 59–61 ms gated.
   - Fresh attach: p95 14–16 ms, max 193–247 ms → p95 5–8 ms, max 83–110 ms.
2. **Daemon reconcile of a window add/remove (~40–65 ms, once per event)** —
   end-to-end only; both servers stay clean (remote max 14 ms, local plain
   pane max 19 ms). Inside the Go daemon; not fixed here (follow-up).
3. ~~Fresh-attach mirroring time (~7.5 s for 11 windows)~~ — a measurement
   artifact: the prototype read "all mirrored" only after its probe had
   finished, so it timed the probe. Timed in the background by the committed
   harness, the whole 12-window mirror fills in ~1.3 s. Not a finding.
4. **The Nix `tmux` wrapper costs 32.5 ms per invocation vs 1.3 ms raw** (66
   PATH prefix blocks in bash). Scripts run by the server resolve the raw
   binary first on PATH, and `og-remote-open` hands the daemon the server's
   PATH, so the hot paths never pay it; only external callers do (~12/s on
   halo, mostly a third-party image viewer) — client-side CPU, never the
   server thread. Follow-up, not a typing-lag cause.
5. Not contributors (measured): busy pane output and title churn alone
   (remote-direct p99 0.6 ms), the `-B` monitor ticks (5 jobs / 5 s),
   `tmux-reflow-windows` (2 batched tmux calls, ~7 ms, off the server thread),
   the local `#()` status jobs (plain local pane p99 ≤ 3–10 ms).
   `session-window-changed`'s foreground reflow + `mark-seen` add ~6 ms to a
   `select-window` on the same client — noted, left alone (foreground is
   deliberate ordering).

The "before" numbers above use this branch's parent (`main`, which has #760's
`window-layout-changed` grid hook: 3 forks per window per event). Halo's live
server still runs a pre-#760 config (2 forks per event), so its storm is two
thirds of the "before" column's, not smaller in kind.

Crew grids matter: on halo 10 of 35 windows carry `@crew_grid=1` (every
dispatched worker window is a 3-pane grid), and `tmux-grid-refit` on an
unchanged grid still does ~8 tmux round-trips before its `@grid_refit_sig`
early exit. A gate on `@crew_grid` alone would leave those windows forking on
every storm event, so the design below also skips *redundant* events on grid
and float windows.

## Goals

- G1. A `window-resized` / `window-layout-changed` event forks a refit job
  only when the script could change something: never for a window that is
  neither a crew grid (`@crew_grid` = 1) nor holds a floating pane with a
  `@float_geom` stamp, and for those windows never for an event that leaves
  them in the state the script last verified. A real resize, split, kill or
  swap still refits exactly as today.
- G2. The probe is committed and re-runnable, with the scenarios above
  (quiet, busy, churn, attach, re-attach), printing p50/p95/p99/max. Its
  remote load is shaped like halo: about a third of the agent windows are
  3-pane `@crew_grid=1` windows (a `@crew_role=lead` pane), one window holds
  a stamped float, and a second session on the same remote server is
  mirrored too, so cross-session recalculation is exercised. The residual
  after the fix is reported, not only the improvement.
- G3. A `nix flake check` regression guard that fails if a resize storm on
  plain windows forks refit jobs again.
- G4. The method, numbers and ranking are recorded in
  `docs/agents/performance.md`, linked from CLAUDE.md.

## Non-goals

- No change to status-bar or reflow output, to the daemon, or to the wrapper.
  Items 2 and 4 become GitHub follow-up issues with their numbers.
- No upstream tmux patch. The (mirror windows × server windows)
  `refresh-client -C @N` recalculation is upstream behaviour; with our forks
  gone it measured cheap (≤ 4 ms on the plain-window chain; re-measured on
  the halo-shaped chain), so it is documented in `performance.md`, not
  filed — unless the halo-shaped residual shows it dominating.

## Design

Gate each refit hook in `config/tmux.conf.tmpl` with an `if-shell -F` on a
format — evaluated in-process, no fork — so the `run-shell -b` only runs when
the script could act. Each gated `set-hook` stays on one line (the
`flake.nix` wiring greps and `grid-refit.bats`'s `arm_grid_hooks` match single
lines):

```
set-hook -g window-resized        { if -F '<float gate>' { run-shell -b "<float-refit> #{q:window_id}" } }
set-hook -g window-resized[10]    { if -F '<grid gate>'  { run-shell -b "<grid-refit> #{q:window_id}" } }
set-hook -g window-layout-changed { if -F '<grid gate>'  { run-shell -b "<grid-refit> #{q:window_id}" } }
```

- **Grid gate**
  `#{&&:#{==:#{@crew_grid},1},#{!=:<geometry sig>,#{@grid_refit_layout}}}`
  where `<geometry sig>` is
  `#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}`
  — the window size plus every tiled pane's id and geometry, in `list-panes`
  order. Not `#{window_layout}`: on tmux-next that renders per client (JSON
  for a CLI / `run-shell` client, the legacy string for a control client),
  so a stamp written by the script would never match a gate evaluated for
  the bridge's control client. The signature prints identically for both
  (verified). The first half of the gate is the script's own first early
  exit. The second half skips an event whose window is in a geometry the
  script last *verified* correct: `tmux-grid-refit`, on its fast-path
  `@grid_refit_sig` match exit (the "already laid out" verdict), stamps the
  window option `@grid_refit_layout` with the signature it read. So the stamp
  describes exactly the state the verdict was computed on, the script reads
  the size, the tiled pane ids, the signature and the stored
  `@grid_refit_sig` in **one** `display-message` — replacing today's
  separate `read_panes` and size reads before the sig check (the under-lock
  re-check keeps `read_panes` and does **not** stamp: a match there means a
  peer applied after our read, so our snapshot was never verified). The
  signature format is duplicated between the hook gate and the script and
  must stay byte-identical; the bats guard fails if they drift. The other
  sig inputs (`@crew_role`, `@crew_grid_main_pct`,
  `@grid_refit_min_role_cols`, `@grid_refit_aspect`) are never hook
  triggers — the dispatcher calls the binary directly after changing them,
  which the gate does not touch. After an apply the script does not stamp:
  its own `select-layout` fires `window-layout-changed`, that event forks
  once, and that run's sig match stamps.
- **Float gate**
  `#{P:#{?#{&&:#{&&:#{pane_floating_flag},#{@float_geom}},#{!=:#{window_width}x#{window_height},#{@float_refit_size}}},1,}}`
  — non-empty iff some floating pane carries a `@float_geom` stamp (the only
  panes the script touches; it also skips a stamp without four fields, so
  the loop is a superset) **and** was last refit at a size other than the
  window's current one. The stamp is **per pane**: `tmux-float-refit`, for
  each float it is about to refit, first runs
  `set-option -pF -t <float> @float_refit_size '#{window_width}x#{window_height}'`,
  then its existing `resize-pane`/`move-pane`. A newly created float has no
  stamp, so its first resize always forks, whichever of the three float
  creators made it (the binds, the picker, the bridge daemon's
  `floatgeom.go`) — none of them changes. Stamping before the refit means a
  resize landing mid-refit differs from the stamp and forks its own run; the
  cost is that a refit whose `resize-pane`/`move-pane` fails is not retried
  until the next size change (today any later same-size event retries) —
  accepted: the failure mode is a vanished pane. On a tmux without floats
  `pane_floating_flag` is empty → gate false → the script would have found no
  float anyway.
- **Why a stale stamp cannot hide a real change.** A change's hook item
  runs on the global queue (or right after the causing command), and
  `server_loop` drains the global queue on every pass. A script needs at least
  one tmux round-trip between reading a state and writing its stamp, so the
  hook item for any change after that read runs before the stamp lands and is
  gated against the old stamp. The only way a gate sees `stamp == current`
  for a changed window is a return to a state already verified, which needs
  no refit. `@float_refit_size` and `@grid_refit_layout` are only ever written
  by their scripts, per pane / per window — never at window/global
  (resp. global) level, where every new float or window would inherit a
  matching stamp.

Alternatives considered: (a) debounce the hooks (one refit per burst) —
still forks per event and adds latency to a real grid refit; (b) have the
daemon send fewer size commands — per-window caps are required (#478) and the
recalculation fan-out is tmux's (the daemon could send the caps only when the
size actually changes, which it already does via `converger`); (c) move the
check into a single dispatcher script — still a fork per event. The format
gates remove the fork outright for plain windows and for redundant events on
grid/float windows, with no behaviour change for a real change.

## Verification

- Existing `grid-refit.bats` (sources the production hook lines),
  `float-refit.bats`, `tmux-next38-readiness.bats` (every registered hook is
  stored) and the `float-conf-assertions` / grid wiring greps in `flake.nix`
  stay green — the greps must still find the store paths on those lines.
- New guard (in `grid-refit.bats`, which already runs with the pinned tmux
  and `TMUX_OG_CONF`): source the production `window-resized` /
  `window-layout-changed` lines into a `-v` server started in a temp dir,
  and count `job_run` lines by script basename (not store path):
  - plain windows resized via per-window `refresh-client -C` from a control
    client → zero refit jobs;
  - an already-laid-out grid window hit by a same-size storm → zero
    grid-refit jobs after its stamp;
  - a grid window's real resize and a split still refit (layout correct,
    existing #760 assertions);
  - a window with a stamped float still runs float-refit on a real resize
    (float regrows) — this also proves `#{P:}` iterates floating panes on
    the pinned tmux; a same-size event forks none;
  - the stale-stamp sequence: a float refit at 200x50, closed, window shrunk,
    a new stamped float opened, window grown back to 200x50 → the new float
    is refit (regrows);
  - settle: after each storm, a further quiet interval forks no refit job
    (a stamp write is itself a `set-option`, which runs
    `recalculate_sizes`; the gate must cut that off after one pass).
- Before/after table from the probe run against `main`'s build and this
  branch's build.
