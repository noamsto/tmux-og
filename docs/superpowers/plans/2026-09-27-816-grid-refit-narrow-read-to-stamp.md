# Plan: fold the lead read and read_opts into the snapshot (#816)

## File list

- `scripts/tmux-grid-refit.sh` — fold the separate `list-panes` (lead lookup)
  and three `read_opt` calls (`@crew_grid_main_pct`,
  `@grid_refit_min_role_cols`, `@grid_refit_aspect`) into the existing
  snapshot `tmux display-message` command list, so they no longer run as
  separate tmux calls after the `@grid_refit_pending` marker is cleared.
- `docs/agents/performance.md` — update the "What still forks" paragraph in
  "Coalescing a resize burst (#810)" to reflect the narrowed gap, and add a
  short before/after refit-job-count note once measured.
- `tests/grid-refit.bats` — no new test (see rationale below); existing
  `grid-refit-tests` must stay green as the regression proof.

## Background / mechanism

`scripts/tmux-grid-refit.sh`'s fast path currently does, in order:

1. One `tmux display-message ... \; set-option -wu @grid_refit_pending`
   snapshot call (clears the pending marker as its first tmux call — #815's
   invariant: every event the marker suppressed predates everything this
   run reads).
2. A separate `tmux list-panes -f '@crew_role==lead && !floating' -F
   '#{pane_id}' | head -1` call to find the lead pane (line 80).
3. Three separate `read_opt` calls, each a `tmux show-options -w -v` (lines
   91, 94, 97), for `@crew_grid_main_pct`, `@grid_refit_min_role_cols`,
   `@grid_refit_aspect`.

Because the pending marker is already cleared after step 1, any
window-resized/window-layout-changed event landing during steps 2–3 is no
longer suppressed by the marker, and the stamp hasn't been written yet
either — so it forks a new run. Folding steps 2–3's reads into the same
command list as step 1 removes those as separate tmux round-trips between
the clear and the eventual stamp, narrowing (not eliminating — the final
`set-option` stamp is still necessarily a later call) the window in which a
competing event can fork.

### Format additions (tmux format-string only, no new tmux calls)

- **Lead pane id**: reuse the exact boolean already used as the `-f` filter
  at (current) line 80 — `#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}}`
  — as the condition of a `#{P:#{?cond,#{pane_id} ,}}` iterator, mirroring
  the existing `pane_ids` format's `#{P:#{?pane_floating_flag,,...}}` idiom
  one line above it. Parse the resulting space-separated string in bash with
  `read -r lead _ <<<"$lead_ids"` (extra tokens, if any misconfiguration ever
  tags two leads, silently discarded into `_` — same "first one wins"
  semantics as today's `head -1`, edge case not required to preserve exact
  tie-breaking order).
- **Option values**: `#{s/[|]/ /:@crew_grid_main_pct}`,
  `#{s/[|]/ /:@grid_refit_min_role_cols}`, `#{s/[|]/ /:@grid_refit_aspect}` —
  replacing `|` with a space, matching the *existing* repo convention for
  `|`-safe rendering of free-text option values in a `|`-delimited format
  string (`scripts/tmux-update-icons.sh:106,404` already use
  `#{s/[|]/ /:@opt}`, not `_`— follow that precedent over the issue body's
  illustrative `_` example, for consistency with the rest of the codebase).
  An unset option renders as an empty string via this format (same as
  `#{@grid_refit_sig}` already relies on at the existing field 6), so the
  existing fallback-to-default logic (regex check → default) is preserved
  with **no extra code**: `read_opt`'s explicit default-on-empty behavior is
  reproduced for free because the existing `[[ $pct =~ ^[0-9]+$ ]] || pct=60`
  style checks already treat an empty string as "use default".

### What does NOT change

- `grid_sig_fmt` (byte-identical-to-tmux.conf.tmpl invariant) — untouched.
- The `read_opt` function — kept, because it's still used for the **under-lock
  re-check** of `@grid_refit_sig` near the bottom of the script (a
  deliberately fresh, non-snapshot read of a value a peer may have written
  since the snapshot — not part of this gap).
- The `read_panes` function, lock acquisition, swap-pane/select-layout logic,
  signature computation, and all fallback defaults (60 / 30 / 2).
- Field order of the *existing* six snapshot fields
  (`is_grid|zoomed|w|h|pane_ids|stored_sig`) — new fields are **appended**
  before the final `grid_sig_fmt`-derived field (which must stay last, since
  it absorbs any literal `|` in pane role/geometry data via `read`'s
  last-variable behavior).

## Steps

- [ ] **Step 1: extend the snapshot format string and read**
  - File: `scripts/tmux-grid-refit.sh`
  - Change the `IFS='|' read -r is_grid zoomed w h pane_ids stored_sig geom <<<...`
    line (current line 71) to add four new fields —
    `lead_ids`, `pct_raw`, `min_cols_raw`, `aspect_raw` — inserted **between**
    `stored_sig` and the final `geom` (`$grid_sig_fmt`) field, both in the
    format string and the `read -r` variable list. Format additions, in
    order: `#{P:#{?#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}},#{pane_id} ,}}`,
    `#{s/[|]/ /:@crew_grid_main_pct}`, `#{s/[|]/ /:@grid_refit_min_role_cols}`,
    `#{s/[|]/ /:@grid_refit_aspect}`.
  - Proof: `shellcheck scripts/tmux-grid-refit.sh` passes (syntax only; no
    tmux to test against yet at this step).

- [ ] **Step 2: replace the separate lead lookup with the folded field**
  - File: `scripts/tmux-grid-refit.sh`
  - Delete the `lead=$(tmux list-panes -t "$target" -f '#{&&:#{==:#{@crew_role},lead},#{!:#{pane_floating_flag}}}' -F '#{pane_id}' 2>/dev/null | head -1)`
    line (current line 80) and its preceding comment about the lead lookup
    that referenced a tmux call; replace with `read -r lead _ <<<"$lead_ids"`.
    Keep the existing `[[ -n $lead ]] || exit 0` guard immediately after,
    unchanged, and keep it positioned exactly where it is today (after the
    zoom check, before the width/height regex check) — do not reorder exits.
  - Proof: (manual read) the resulting exit-path order matches the
    pre-change order: is_grid → zoomed → lead → w/h regex → pane count.

- [ ] **Step 3: replace the three `read_opt` calls with the folded fields**
  - File: `scripts/tmux-grid-refit.sh`
  - Replace `pct=$(read_opt @crew_grid_main_pct 60)` with `pct=$pct_raw`;
    `min_cols=$(read_opt @grid_refit_min_role_cols 30)` with
    `min_cols=$min_cols_raw`; `aspect=$(read_opt @grid_refit_aspect 2)` with
    `aspect=$aspect_raw`. Leave the three subsequent validation/default lines
    (`[[ $pct =~ ^[0-9]+$ ]] && ((pct >= 1 && pct <= 99)) || pct=60`, etc.)
    completely unchanged — they already fall back to the default on an empty
    string, which is exactly what an unset option renders as in the folded
    format.
  - Proof: (manual read) no behavior change to the default-substitution
    logic; only the *source* of the raw value changed from a `tmux
    show-options` call to a pre-fetched format field.

- [ ] **Step 4: run the existing test suite**
  - Command: `nix build .#checks.x86_64-linux.grid-refit-tests -L` (or
    `nix flake check`, which runs it as part of the full check set).
  - Expected: all existing `grid-refit-tests` (24+ tests per #815's PR body)
    stay green — this is the regression proof for the fold, since it changes
    only *how* six pre-existing values are obtained, not their values or the
    decision logic that consumes them.
  - If red: read the failure — a wrong nested `#{?...}`/`#{&&:...}` brace
    match is the most likely cause (verify against the tested precedent at
    the existing `pane_ids` field and the hook gate in
    `config/tmux.conf.tmpl`, which already uses the identical lead-boolean
    expression as an `-f` filter and inside a similar nested ternary,
    respectively).

- [ ] **Step 5: update the design doc**
  - File: `docs/agents/performance.md`
  - Rewrite the "What still forks" paragraph (currently: "What still forks:
    events that land after a run's read and before its stamp (the lead read
    and three `read_opt`s on the grid's fast path, the per-pane stamp on the
    float path). Folding those reads into the snapshot would narrow it
    further; their values are free text, so that needs `|`-safe rendering.")
    to say the grid side is now folded (#816): the lead pane id and the
    three option reads are part of the one snapshot `display-message`, so the
    remaining grid-side gap is only the final stamp `set-option` call itself
    (irreducible — the stamp must follow the decision). The float path's
    per-pane stamp is unchanged (out of scope for #816 per the issue body).
  - Proof: re-read the paragraph; it should not claim something the code no
    longer does (no dangling reference to a since-removed `read_opt` call
    for pct/min_cols/aspect).

- [ ] **Step 6: before/after re-measurement**
  - Command: `VLOG=1 tests/perf/keystroke-latency.sh reattach`, run twice
    per build (this branch vs. `main` e78655a, alternating in one session),
    under a `systemd-run --user --scope -p CPUQuota=400%` cap (4-core cap
    per the dispatcher note), announcing start/end on the crew bus before
    and after each round (Rule 8's shared-host courtesy — other workers are
    running).
  - Expected: refit job counts at or below the current 56–65 reattach
    baseline from #815's own table in `docs/agents/performance.md` (a
    narrower gap can only reduce or hold steady the count of jobs forked
    purely from this race — it does not change genuine resize-driven
    refits). Record the actual before/after numbers in the PR body and in
    `docs/agents/performance.md`.
  - If the after-count is not clearly lower: report the raw numbers anyway
    (the doc already frames this as a probabilistic race-window narrowing,
    not a guaranteed-zero fix) — do not fabricate a bigger effect than
    measured.

- [ ] **Step 7: full local gate**
  - Commands: `nix build .#default`, `nix flake check`, `nix build .#lint`.
  - Expected: all three pass (per task Acceptance).

## Test-first note

There is no new deterministic test added for the "narrowed gap" itself: it
is a race-window-width change (probabilistic under real concurrent tmux
events), not a change in any function's return value or any test-observable
invariant beyond what the existing `grid-refit-tests` already assert (lead
placement, layout choice, pct/min_cols/aspect defaults and overrides, sig
caching, lock behavior, pending-marker semantics). A parallel investigation
is checking whether a deterministic per-tmux-invocation-count test (as
opposed to the existing per-forked-job-count `refit_jobs()` helper) is
feasible against the real server this suite already spins up; if it reports
a viable technique before execute reaches step 4, add one assertion of the
form "the script's fast path makes N tmux calls, not N+4" using that
technique, gated on the same real per-test tmux server the suite already
uses (no new test infra). Otherwise the existing suite passing green, plus
the explicit before/after job-count re-measurement in step 6, is the
acceptance evidence per the task's own wording ("plus a test covering the
narrowed gap **if it's testable deterministically**").

## Acceptance mapping

- "Existing `grid-refit-tests` pass, plus a test covering the narrowed gap
  if it's testable deterministically." → Step 4 (existing suite green) +
  the Test-first note above (new test added only if research confirms
  feasibility; otherwise explicitly justified as not deterministically
  testable).
- "Before/after refit job counts in the PR." → Step 6.
- "`nix build .#default`, `nix flake check`, `nix build .#lint` pass." →
  Step 7.
