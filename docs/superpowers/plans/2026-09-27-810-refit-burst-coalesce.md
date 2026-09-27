# Plan — #810: coalesce refit forks within one resize burst

Spec: `docs/superpowers/specs/2026-09-27-810-refit-burst-coalesce-design.md`. Mechanism: the
gated hook writes a signature-keyed pending marker right before its
`run-shell -b`; the gate also skips when the live signature equals the marker;
the script clears the marker in its first tmux call, in the same client
command list as its state read.

Consult: survey did not trip (one subsystem: two hook families, two scripts,
one bats file, docs). No `DECOMPOSITION.md`.

## Files

| File | Change |
| --- | --- |
| `scripts/tmux-grid-refit.sh` | Snapshot becomes the first tmux call, absorbs `@crew_grid` + zoom, clears `@grid_refit_pending` in the same command list; lead read moves after it. |
| `scripts/tmux-float-refit.sh` | Its `list-panes` read also clears `@float_refit_pending` in the same command list. |
| `config/tmux.conf.tmpl` | Three refit hook lines: marker conjunct in the gate, `set -wF <marker>` before `run-shell -b`; comment update. |
| `config/tmux.conf.reference.nix` | Byte-mirror of the three hook lines + comment (extraction-check oracle; `${script.…}` in place of the template path). |
| `tests/grid-refit.bats` | Five new tests (below); `teardown` removes a test-created lock dir. |
| `docs/agents/performance.md` | Gate description gains the marker; "after" residual paragraph rewritten; new before/after rows from the re-measure. |
| `docs/agents/scripts.md` | `tmux-float-refit` / `tmux-grid-refit` rows name the marker they clear. |
| `docs/agents/floats.md` | `@float_refit_size` line names `@float_refit_pending`. |
| `docs/superpowers/specs/2026-09-27-810-refit-burst-coalesce-design.md`, `docs/superpowers/plans/2026-09-27-810-refit-burst-coalesce.md` | This spec and plan, committed per CLAUDE.md. |

## Steps

### 1. `scripts/tmux-grid-refit.sh` (execute: sonnet)

Replace lines 60–82 (the `@crew_grid` read, zoom read, lead read, `read_panes`
def, snapshot, `w`/`h` check, `ids`/`np`) with, in this order:

```bash
read_panes() { tmux list-panes -t "$target" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id}' 2>/dev/null; }

# One snapshot, so the stamp below describes exactly the state the verdict
# used. The signature is the last field: it carries free-text option values,
# and as the final `read` variable it absorbs any '|' in them.
#
# The hooks set @grid_refit_pending before forking this run, so the rest of a
# resize burst skips while it is on its way. Clearing it in this command list
# means every event it suppressed predates everything this run reads, so this
# must stay the first tmux call.
IFS='|' read -r is_grid zoomed w h pane_ids stored_sig geom <<<"$(tmux display-message -p -t "$target" "#{==:#{@crew_grid},1}|#{window_zoomed_flag}|#{window_width}|#{window_height}|#{P:#{?pane_floating_flag,,#{pane_index}:#{pane_id} }}|#{@grid_refit_sig}|$grid_sig_fmt" \; set-option -wu -t "$target" @grid_refit_pending 2>/dev/null)"
[[ $is_grid == 1 ]] || exit 0

# Zoom is user state: a zoomed grid is left exactly as the user left it.
[[ $zoomed == 1 ]] && exit 0

# The lead pane is the main pane. No lead -> not a grid we can lay out.
# Floats are excluded everywhere (#760): window-layout-changed fires on a float
# open/close, and a float is not part of the tiled set select-layout lays out.
lead=$(tmux list-panes -t "$target" -f '#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}}' -F '#{pane_id}' 2>/dev/null | head -1)
[[ -n $lead ]] || exit 0

[[ $w =~ ^[0-9]+$ && $h =~ ^[0-9]+$ ]] || exit 0
# #{P:} is not layout order on tmux-next (a swap-pane leaves it unchanged);
# pane_index is.
mapfile -t ids < <(tr ' ' '\n' <<<"$pane_ids" | grep . | sort -t: -k1,1n | cut -d: -f2)
np=${#ids[@]}
((np > 1)) || exit 0
```

Pinned: field order `is_grid zoomed w h pane_ids stored_sig geom` — the two
booleans first, `geom` last. Everything below (pct/min_cols/aspect `read_opt`s,
verdict, fast-path stamp, lock, apply) is unchanged. `read_opt` and
`acquire_lock` definitions stay above (they make no tmux call at definition).
Run `shellcheck` + `shfmt -d` on the file.

### 2. `scripts/tmux-float-refit.sh` (execute: sonnet)

The loop's input becomes:

```bash
done < <(tmux list-panes -t "$target" -f '#{pane_floating_flag}' \
	-F '#{pane_id}|#{@float_geom}' \; set-option -wu -t "$target" @float_refit_pending 2>/dev/null)
```

and add, above the `while`, a comment in the file's register: the hook sets
`@float_refit_pending` before forking this run, so the rest of a resize burst
skips; clearing it in the same command list as the read (the first tmux call)
means any event it suppressed predates the read. `shellcheck` + `shfmt -d`.

### 3. Hook lines — `config/tmux.conf.tmpl` and `config/tmux.conf.reference.nix` (execute: sonnet)

Let `SIG` = the existing grid signature, byte-identical to `grid_sig_fmt`:
`#{window_width}x#{window_height}:#{@crew_grid_main_pct}:#{@grid_refit_min_role_cols}:#{@grid_refit_aspect}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height}.#{@crew_role} }}`.
Let `WH` = `#{window_width}x#{window_height}`.

- `window-resized` (float, index 0):
  `set-hook -g window-resized          { if -F '#{&&:#{!=:WH,#{@float_refit_pending}},#{P:#{?#{&&:#{&&:#{pane_floating_flag},#{@float_geom}},#{!=:WH,#{@float_refit_size}}},1,}}}' { set -wF @float_refit_pending 'WH' ; run-shell -b "<float path> #{q:window_id}" } }`
- `window-resized[10]` and `window-layout-changed` (grid, identical bodies):
  `{ if -F '#{&&:#{&&:#{==:#{@crew_grid},1},#{!:#{window_zoomed_flag}}},#{&&:#{!=:SIG,#{@grid_refit_layout}},#{!=:SIG,#{@grid_refit_pending}}}}' { set -wF @grid_refit_pending 'SIG' ; run-shell -b "<grid path> #{q:window_id}" } }`

Each stays one line (the flake's wiring greps and `arm_refit_hooks` read
single lines). Comment block above `window-resized` (tmpl + reference): add
two sentences — a gated hook marks the window's signature pending before it
forks (`@float_refit_pending`, `@grid_refit_pending`) and skips while it
matches, so a burst forks one run until that run's first tmux call clears it;
keyed on the signature so a left-behind marker never blocks a different
state. The `window-resized[10]` comment gains: the signature appears in both
gates and both marker writes; a drifted copy fails safe and turns
grid-refit.bats red. Reference file: same text inside the nix string (no `${`
introduced).

### 4. `tests/grid-refit.bats` (execute: sonnet)

Helpers: `wait_stamp` is NOT added (keep the inline poll loops the file uses).
`teardown` gains `[[ -n ${LOCKDIR:-} ]] && rmdir "$LOCKDIR" 2>/dev/null || true` before
`kill-server`. One job = two `job_run` lines (probed), so "exactly one job" is
asserted as `delta -gt 0 && delta -le 2`.

1. **"a burst of events on an unverified grid forks one refit (#810)"** —
   `arm_refit_hooks || skip`; `start_logged_server`; three `new-window -d`;
   `make_grid 1`; `arm_refit_hooks`; `bash "$GRID" "$WIN"`; poll
   `@grid_refit_layout` non-empty; `settle`; `set-option -w @grid_refit_layout
   stale`; `settle`; `g0`; `storm`; `settle`; assert one job; assert the stamp is no
   longer `stale`.
2. **"a burst of events on a stale stamped float forks one refit (#810)"** —
   like #807's stamped-float test plus three windows; after the float regrew,
   `settle`, `set-option -p -t "$float" @float_refit_size stale`, `settle`, `f0`,
   `storm`, `settle`, assert one `tmux-float-refit` job and the stamp is
   `200x50`.
3. **"a lock loser leaves no pending marker behind (#810)"** — logged server,
   `make_grid 1`, arm, `bash "$GRID"`, poll stamp, `settle`. `LOCKDIR` =
   `${TMPDIR:-/tmp}/og-grid-refit.lock.<#{pid}>.${WIN//[^A-Za-z0-9]/_}` (the
   script's own formula; the job's `$TMUX` second field is the server pid);
   `mkdir "$LOCKDIR"`. `set-option -w @crew_grid_main_pct 40`; `g0`; `storm`;
   `settle`; assert ≥1 job ran, `@grid_refit_pending` is empty, and the lead
   width is still > 85 (the loser applied nothing). `rmdir "$LOCKDIR"`;
   `LOCKDIR=`; `storm`; poll lead width ≤ 85.
4. **"a pending marker left behind does not block a different signature (#810)"** —
   logged server not needed; default server, `make_grid 1`, arm, `bash
   "$GRID"`, poll stamp. `set-option -w @grid_refit_pending "<stamp value>"`
   (as a run killed before its read leaves it); `set-option -w
   @crew_grid_main_pct 40`; `storm`; poll lead width ≤ 85.
5. **"a run that finds the grid zoomed clears the marker, and unzoom refits (#810)"** —
   default server, `make_grid 1`, arm, `bash "$GRID"`, poll stamp.
   `set-option -w @crew_grid_main_pct 40` (a set-option fires no refit hook —
   probed); `fmt` = `sed -n "s/^grid_sig_fmt='\(.*\)'\$/\1/p" "$GRID"`;
   `post=$(tmux display-message -p -t "$WIN" "$fmt")`; `resize-pane -Z -t
   "$LEAD"`; `settle`; `set-option -w @grid_refit_pending "$post"`; `bash "$GRID"
   "$WIN"`; assert marker empty; `resize-pane -Z -t "$LEAD"`; poll
   `main-pane-width` = `40%`, lead first, lead width ≤ 85.

### 5. Gate (lead, not delegated)

- `shellcheck` + `shfmt -d` on the two scripts; `bats` locally against a
  conf built from this tree: `TMUX_OG_CONF=$(nix build --no-link --print-out-paths .#…conf)`
  if exposed, else `nix build .#checks.x86_64-linux.grid-refit-tests`
  (+ `float-refit-tests`).
- Red evidence: a scratch worktree at `origin/main` with the new
  `tests/grid-refit.bats` copied in; `nix build .#checks.x86_64-linux.grid-refit-tests`
  there must fail on tests 1–2 (each forks > 1 job). Sensitivity for 3/5: scratch copy of the script
  without the `\; set-option -wu` clear (and a conf pointing at it) → 3's
  second storm and 5's marker assertion fail. Test 4: a boolean marker gate
  in a scratch conf → blocks → red.
- `nix build .#default`, `nix flake check`, `nix build .#lint`.

### 6. Re-measure + docs (lead)

`nix build .#default`; `VLOG=1 TMUX_BIN=./result/bin/tmux tests/perf/keystroke-latency.sh busy reattach`
twice; record refit jobs and p50/p95/p99/max. Update `performance.md`: the
gate bullets (marker), "What the 'after' residual is" → the new residual and
what is left (read→stamp gap), a #810 row set in the tables. `scripts.md` +
`floats.md` lines. Commit spec + plan into `docs/superpowers/`.

## Acceptance mapping

| Acceptance (task doc) | Evidence |
| --- | --- |
| Burst of identical events forks once | tests 1, 2 (red on main) |
| Left-behind pending (lock loser / aborted) does not block the next differing signature | tests 3, 4 |
| Zoom in/out still refits | test 5 + #807's zoom test |
| Re-measure + perf doc | step 6 |
| build / flake check / lint | step 5 |

## PR notes

- `#{==:#{@crew_grid},1}` in the snapshot resolves with option inheritance,
  where `read_opt` read the window option only; the gate already used the
  format, so the script now agrees with it.
