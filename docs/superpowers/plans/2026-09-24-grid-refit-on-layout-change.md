# Plan: `tmux-grid-refit` must run on every layout change (issue #760)

## Root cause (measured, not hypothesised)

The pinned tmux server (`next-3.9`, the one the flake builds and every check runs
against) changed cell redistribution on `kill-pane`: a pane's freed cells go to a
neighbouring layout node rather than collapsing back to the pre-split sibling.
`tmux-grid-refit` is wired only to `window-resized[10]` (plus the dispatcher's
direct call after it adds/removes a *role* pane). The aeye carousel toggle is a
plain `tmux split-window` / `tmux kill-pane` on the host (lead) pane — neither
fires `window-resized`, so no refit runs and the lead never returns to
`@crew_grid_main_pct`.

Measured on a scratch next-3.9 server, 200x50, `@crew_grid=1`, lead + 2 roles:

```
before refit-applied : lead=119
after split -h lead  : lead=59  carousel=59
after kill carousel  : lead=59  roles grew 80 -> 140   # bug
```

Same sequence on tmux 3.7c restores to 119, which is why the bug only shows on
the pinned server.

`window-layout-changed` fires on every layout mutation we care about — verified on
next-3.9: `split-window`, `kill-pane`, a pane process exiting, `move-pane`
(aeye's `s` axis toggle), and float `new-pane`/close. It carries
`#{q:window_id}` correctly in all cases.

## Decision: option (b) — refit on layout change

Option (a) ("exclude the carousel from the grid and give it its own slice") is
not expressible: `select-layout main-vertical` operates on the window's whole
tiled set, and there is no per-pane opt-out. Reproducing a custom layout with
`resize-pane` arithmetic is fragile and would fight the existing refit. So: add a
`window-layout-changed` hook that calls `tmux-grid-refit`, exactly as
`window-resized[10]` already does. While the carousel is open it joins the role
column and the lead keeps its share; closing it refits back to
`@crew_grid_main_pct`. This is idempotent (the existing `@grid_refit_sig` cache +
`mkdir` lock) and a no-op on a non-grid, a zoomed window, a lead-less window, or a
single pane.

Because `window-layout-changed` also fires on float open/close, `tmux-grid-refit`
must stop counting floating/modal panes in its tiled set (the modal-pane chrome
rule, `docs/agents/floats.md`). Without the filter, `np` churns and
`select-layout` is re-applied on every float operation in a grid. Filter with
`#{!:#{pane_floating_flag}}` on both the pane enumeration and the lead lookup.

## Consumer / contract map (grid-hint contract, shared with dispatcher)

- **Producer** (dispatcher): window `@crew_grid`, `@crew_grid_main_pct`, pane
  `@crew_role`. Unchanged.
- **tmux-og consumers of those hints**: `tmux-grid-refit` only. Unchanged reads.
- **Invokers of `tmux-grid-refit`**: dispatcher direct call (unchanged),
  `window-resized[10]` (unchanged), **new** `window-layout-changed` (covers
  split / kill-pane / pane-exit / move-pane).
- **Pane set**: was "all panes"; now "all non-floating panes". `select-layout`
  already ignores floats, so this only removes spurious sig churn.

## Files

- `config/tmux.conf.tmpl` — add the `window-layout-changed` hook + its `-gu`
  clear line; update the grid comment.
- `config/tmux.conf.reference.nix` — byte-identical mirror (extraction check
  diffs the two).
- `scripts/tmux-grid-refit.sh` — float filter in `read_panes` and the lead lookup.
- `flake.nix` — `grid-conf-assertions` asserts the new hook + clear-before-set;
  `grid-refit-tests` receives the rendered conf and the built script path.
- `tests/grid-refit.bats` — red-first regression, sourcing the production hook
  wiring.
- `docs/agents/scripts.md` — `tmux-grid-refit` row.
- `docs/agents/floats.md` — add `tmux-grid-refit` to the float-aware consumers.
- `docs/superpowers/plans/2026-09-24-grid-refit-on-layout-change.md` — this plan.

## Steps

- [ ] **Step 1 — write the red regression first.** In `tests/grid-refit.bats`, add
  a helper and test:
  - `CONF="${TMUX_OG_CONF:-}"`; the new test `skip`s when `TMUX_OG_CONF` is unset
    (so a bare local `bats tests/grid-refit.bats` still runs the existing cases).
  - The test extracts the *production* wiring with
    `grep tmux-grid-refit "$TMUX_OG_CONF"` and `tmux source-file` it onto the
    scratch server — this is what makes it red before the config change and green
    after, exercising the real hook lines rather than a hand-rolled copy.
  - `make_grid 1`, apply the layout, record `before`; `split-window -h -t "$LEAD"`,
    poll up to ~3s until the split settles, assert the lead is still `>= 115`
    (carousel joined the role column, lead kept ~60%); `kill-pane` the carousel,
    poll, assert the lead width is back to `before` exactly.
  - Verify locally against the pinned binary: `PATH="$(dirname "$GRID_BIN"):$PATH"`
    or the built `result/bin/tmux` on PATH, since stock 3.7c restores on its own
    and would hide the regression. Expected: **fails** on the current tree
    (`after kill: lead=59`), passes once Step 3 lands.
  - [ ] **Step 2 — add the production hook.**
  - `config/tmux.conf.tmpl`: in the clear block beside
    `set-hook -gu window-resized`, add `set-hook -gu window-layout-changed`.
    Right after the `window-resized[10]` grid line add:
    `set-hook -g window-layout-changed 'run-shell -b "{{index .Paths.Scripts "tmux-grid-refit"}} #{q:window_id}"'`.
    Extend the comment: refit must run on any layout mutation (split/kill/move),
    because the carousel toggle splits/kills a pane without resizing; floats are
    excluded inside the script.
  - `config/tmux.conf.reference.nix`: the same two lines in Nix string form
    (`"${script.tmux-grid-refit}/bin/tmux-grid-refit #{q:window_id}"`), same
    column alignment as the `window-resized[10]` line, byte-identical modulo
    the render.
- [ ] **Step 3 — float filter in the script.** `scripts/tmux-grid-refit.sh`:
  - `read_panes()` → `tmux list-panes -t "$target" -f '#{!:#{pane_floating_flag}}' -F '#{pane_id}'`.
  - the `lead=` lookup gains the same `-f '#{!:#{pane_floating_flag}}'`.
  - update the header comment to name the new hook and the float exclusion.
- [ ] **Step 4 — flake wiring + assertions.**
  - `grid-conf-assertions`: add a grep for
    `set-hook -g window-layout-changed[[:space:]]+.*/bin/tmux-grid-refit #\{q:window_id\}`,
    and assert its `set-hook -gu window-layout-changed` clear sits above the setter
    (same shape as the existing window-resized ordering check).
  - `grid-refit-tests`: add `GRID_BIN = "${tmuxConfig.script.tmux-grid-refit}/bin/tmux-grid-refit";`
    and `TMUX_OG_CONF = "${tmuxConfig.tmuxConf}";` to the derivation env.
- [ ] **Step 5 — float invisibility test.** In `tests/grid-refit.bats`, add a case
  (guarded by a float-capable tmux, mirroring the existing coexistence test) that
  sources the production hooks, makes a grid, creates a floating pane, and asserts
  `@grid_refit_sig` and the lead width are unchanged by the float's creation and
  removal.
- [ ] **Step 6 — docs.** `docs/agents/scripts.md`: in the `tmux-grid-refit` row,
  name `window-layout-changed` as a trigger and the `pane_floating_flag` filter.
  `docs/agents/floats.md`: list `tmux-grid-refit` among the float-aware consumers.
- [ ] **Step 7 — commit this plan** to
  `docs/superpowers/plans/2026-09-24-grid-refit-on-layout-change.md`.

## Verification

- `bats tests/grid-refit.bats` with the pinned next-3.9 tmux on PATH — new test
  red before Step 2/3, green after.
- `nix build .#default`
- `nix flake check`
- `nix build .#lint`

## Risks / notes

- `window-layout-changed` fires several times per mutation (measured 3x on
  `move-pane`). The script's fast sig+lead pre-check returns after two cheap
  reads; no state-changing command runs when unchanged. Instrumented count over
  open/close/move/float was bounded and converged (no recursion storm).
- Refit's own `swap-pane`/`select-layout` re-trigger the hook; the lock plus the
  under-lock re-check make the recursive call exit on the matching sig.
- Zoom is user state: the script's `window_zoomed_flag` guard already no-ops; the
  existing zoom test stays valid because the hook only invokes the same script.
## Plan-critic review (accept, 2026-09-24)

Independent plan-critic: **accept**, no blocking findings. Non-blocking notes
applied during execution:

- The lead lookup must **combine** filters, not replace: it stays
  `-f '#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}}'` (a lone float
  filter would promote the first tiled pane and demote the real lead).
- No dead `GRID_BIN` env: the test sources the rendered conf's store-path hook
  lines, and the derivation's PATH already carries the pinned tmux.
- Dropped the move-pane coverage claim: a move that preserves pane count and the
  lead-first order leaves `@grid_refit_sig` unchanged, so it is not
  re-normalised; only the pane-count-changing open/close path is guaranteed and
  is what the acceptance and the bats regression exercise.
- Step order: write the test, wire `TMUX_OG_CONF` into the derivation, capture
  red, then land the hook + filter.
- `floats.md` records explicitly that `tmux-grid-refit` filters **all** floats
  with `pane_floating_flag` (not the modal `NOT_MODAL`), so the divergence reads
  as deliberate, not drift.
