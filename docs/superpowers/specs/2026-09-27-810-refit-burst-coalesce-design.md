# Spec — #810: coalesce redundant grid/float refit forks within one resize burst

## Problem

Since #807 (commit 01a8e0d) the three refit hooks fork only past an in-process
`if -F` gate: a stamped float whose pane-scoped `@float_refit_size` differs
from the window's `WxH`, or a crew grid whose decision signature differs from
`@grid_refit_layout` (the stamp `tmux-grid-refit` writes on its fast path).

On a *real* resize the bridge daemon's per-window `refresh-client -C` caps fire
a burst of `window-resized` / `window-layout-changed` events for every grid and
stamped float (tmux-next re-fires both hooks on every window per cap). Every
event that arrives before the first forked run has written its stamp still sees
the old stamp and forks its own run. Measured on #807's build with
`VLOG=1 tests/perf/keystroke-latency.sh reattach`: 429 refit jobs; re-attach
p99 116–164 ms, max 214–237 ms.

## Goal

Within one burst, stop forking a run per event: events that land between a
run's fork and its first tmux call skip, because that run will read the
window after them. Events between the run's read and its stamp write (the
lead read and three option reads on the grid's fast path; the per-pane stamp
on the float path) still fork, as today — the harness measures what is left.
Never let the coalescing suppress a refit that is actually needed.

## Design

### Pending marker, keyed on the gate's own signature

- **Grid** (`window-resized[10]` and `window-layout-changed`, both in
  `config/tmux.conf.tmpl` and its mirror `config/tmux.conf.reference.nix`):
  the gate gains a conjunct `#{!=:<SIG>,#{@grid_refit_pending}}`, where
  `<SIG>` is the existing signature (byte-identical to `grid_sig_fmt` in
  `scripts/tmux-grid-refit.sh`). The gated body becomes
  `set -wF @grid_refit_pending '<SIG>' ; run-shell -b "… tmux-grid-refit #{q:window_id}"`.
- **Float** (`window-resized` index 0): the gate gains
  `#{!=:#{window_width}x#{window_height},#{@float_refit_pending}}` and the body
  becomes `set -wF @float_refit_pending '#{window_width}x#{window_height}' ; run-shell -b "… tmux-float-refit #{q:window_id}"`.
- Both markers are **window-scoped** user options, written by the hook only,
  and cleared (unset) by the script only.
- The grid signature now appears four times in the conf (two gates, two marker
  writes) plus `grid_sig_fmt`. A drifted copy fails safe — a marker that never
  matches just stops coalescing, a gate/stamp mismatch forks every event — and
  either drift turns the new burst test (acceptance 1) or #807's storm tests
  red. The conf comment names that guard.

Keyed on the signature (not a boolean): a marker that is somehow left behind
blocks only events carrying that exact signature; any different state still
forks, and that run clears the marker.

### When the script clears it: in its first tmux call, before any decision read

The script does not clear the marker at exit. It clears it **in its first tmux
call, in the same client command list as that call's read, and every read a
decision depends on happens in or after that call**:

- `tmux-grid-refit`: the one-snapshot `display-message -p` moves to be the
  script's first tmux call and becomes
  `display-message -p … \; set-option -wu -t <win> @grid_refit_pending`. It
  also absorbs the two boolean reads that today precede it —
  `#{==:#{@crew_grid},1}` and `#{window_zoomed_flag}` (both `|`-free, so the
  field split is unaffected; the signature stays the last field). The lead
  `list-panes` read and the three `read_opt`s (`pct`, `min_cols`, `aspect`)
  stay as they are but now all run after the snapshot. Net: two fewer tmux
  calls per run.
- `tmux-float-refit`: its `list-panes -f '#{pane_floating_flag}' …` read is
  already its first tmux call; it becomes
  `list-panes … \; set-option -wu -t <win> @float_refit_pending`.

Commands in one client command list run back-to-back on the server thread, so
no hook gate is evaluated between the read and the clear. Unsetting an unset
user option is silent with status 0 (probed), so a direct dispatcher call
with no marker set is unaffected, and `display-message` runs first so its
output is captured regardless.

**Why this cannot hide a needed refit.** An event at time *t* is skipped only if
the marker equals its signature. The marker's last write happened at some
*t_w ≤ t*, by a hook that forked run *R* at *t_w*; no clear has run since
*t_w* (it would have unset the marker). So *R* has not made its first tmux
call yet, and every read *R* bases a decision on — `@crew_grid`, zoom, the
snapshot, the lead, the options — observes the window after *t*. Whatever the
window looks like then, *R* acts on it (including deciding it is zoomed or not
a grid, which is then true of a state after *t*: a later unzoom fires its own
`window-layout-changed`, which finds the marker clear). Every event after
*R*'s first call is gated by the stamp exactly as in #807, whose argument
("every change is gated against the stamp as it stood before that change") is
unchanged. No `EXIT` trap is needed: there is no exit path before the clear
other than the empty-target check, and the hook always passes a target.

Moving the lead read after the snapshot is also the safe direction for the
stamp: a lead re-tag between the two reads leaves the stamped signature (old
roles) different from the live one, so the next event re-verifies — where
today a re-tag between the lead read and the snapshot could stamp a state the
verdict never saw.

**Exits after the clear** (not a grid, zoomed, no lead, `np ≤ 1`, bad
`w`/`h`, fast path, lock loser, apply) need nothing. A lock loser still drops
its work as today (out of scope); coalescing removes the redundant burst forks
that today sometimes happened to retry after the holder released, so a loser
whose read saw a genuinely new state is somewhat more likely to stay
unrecovered when the burst ends before its read. That is the pre-existing
loser bug, not a new suppression: the marker never blocks an event after the
loser's read.

**Residual stuck-marker cases** (accepted, documented): the forked run is
SIGKILLed before its read, or `run-shell -b` fails to fork. The marker then
blocks only events with that exact signature, until the window reaches any
other state (whose run clears it). This is the same class of failure as the
existing grid lock's 60 s stale window, and strictly narrower.

### Measured assumptions this relies on (probed on the pinned tmux-next-3.9)

- A `set-option` / `set-option -u` of a user option does **not** fire
  `window-resized` or `window-layout-changed` (probe: hooks that increment a
  counter via `set -gF` did not re-fire; with a control client attached,
  counts unchanged across `set -w`, `set -wu`, `set -p`). So the marker writes
  and the clear do not themselves re-trigger the gates.
- `set -w` with no `-t` inside a window hook targets the hook's window (probe:
  `set -wF @self '#{window_id}'` in `window-resized` stamped each window with
  its own id).

## Out of scope

- The `acquire_lock` loser dropping its work (pre-existing; unchanged by this).
- Any script refactor beyond the reorder/fold above.
- Folding the lead read and the three `read_opt`s into the snapshot too (would
  shrink the read→stamp gap further; the option values are free text, so it
  needs `|`-safe rendering) — a follow-up if the re-measure shows a residual.
- `#809` (wrapper PATH) — same nix file region; this change stays in the hook
  definitions and the two scripts.

## Acceptance

- Bats (`tests/grid-refit.bats`, production hook lines via `arm_refit_hooks`,
  job counts from the `tmux -v` log):
  1. A burst of identical events against an unverified grid state forks
     exactly one `tmux-grid-refit` job (red on #807's build: one per event).
  2. Same for a stamped float with a stale `@float_refit_size`: exactly one
     `tmux-float-refit` job.
  3. A lock loser (live lock dir pre-created) leaves `@grid_refit_pending`
     unset; once the lock is gone, the next event at the *same* signature
     refits.
  4. A stale `@grid_refit_pending` left behind (set by hand) does not block
     the next *differing* signature (change `@crew_grid_main_pct`, storm →
     lead share applied).
  5. Zoom: a run that finds the window zoomed clears the marker, and unzoom
     then refits (marker pre-set to the post-unzoom signature; red if the
     zoom exit leaves it).
  6. A direct call (no marker set) still lays the grid out — covered by the
     existing direct-call tests.
  7. All existing grid-refit / float-refit tests still pass.
- Re-measure `VLOG=1 tests/perf/keystroke-latency.sh reattach` (and `busy`):
  report refit job count and p99/max vs #807's (429 jobs; p99 116–164 ms, max
  214–237 ms) in the PR; update `docs/agents/performance.md` (gate description,
  the "after" residual paragraph, before/after numbers) and `docs/agents/scripts.md`
  / `floats.md` where they name the stamps.
- `nix build .#default`, `nix flake check`, `nix build .#lint` pass.
