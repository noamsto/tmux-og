# Enrichment: Issues, PRs, Agent Usage

## PR + Issue Enrichment

Per-worktree Linear/GitHub issue identity + PR check-state shown in the status
line. Enabled by default via `programs.tmux-og.enrich.enable`.

- **Window options are the source of truth.** `tmux-issue-stamp` (backgrounded
  from worktrunk `post-switch` hook) writes `@issue_*`; `tmux-pr-enrich`
  (driven by the `@og-pr-tick` monitor hook, not status-format[0] — a
  control-mode client renders no status line, so a status-driven poller never
  runs on a host whose only clients are bridges, #603) writes `@pr_*`. Display
  formats and keybinds only read them.
- **Providers** (`enrich.providers`, default `["linear" "github"]`) are tried in
  priority order; first non-empty issue id wins. Both CLIs are optional and
  degrade gracefully: `gh` (inherited from PATH) provides PR data and GitHub
  issue titles, `linear` provides Linear titles/URLs. Without a CLI, only the
  branch-regex-derived issue id is shown (no titles, no PR state).
- **Keybindings:** `prefix + i` enters the enrich table — `i` open issue URL,
  `p` open PR URL, `r` force-refresh the current window. In a **mirror** window
  the card reads the bridged `@bridge_*` state and `r` routes to the remote via
  the `enrich-refresh` ctl verb, since the local poller refuses a `@bridge_win`
  target outright (see "Remote Window Labels" in `bridge-shipped-state.md`, #598).
- **Refresh:** `prRefreshSeconds` (default 120, clamped 10-300) gates fast PR
  identity polling. `prCheckRefreshSeconds` (default 300, clamped 10-300)
  separately gates the more expensive CI-rollup query; `r` refreshes both for
  the current window immediately. PR state is cached at `/tmp/og-pr/`
  (60s TTL). A repo whose last applied rollup was still pending re-polls
  checks alone about every 30s (never slower than `prCheckRefreshSeconds`) via
  the `--tick-run-pending` pass, so a running check suite doesn't sit stale for
  a whole `prCheckRefreshSeconds` window. The fast cadence is capped at 10
  minutes per repo, counted from when the repo was first seen pending (the
  marker's content): a required status stuck at `EXPECTED` or a deployment
  awaiting approval would otherwise spend the shared GraphQL bucket 120 times
  an hour, so past the cap the repo falls back to `prCheckRefreshSeconds` until
  its checks settle. A marker whose repo has lost its windows is removed by the
  next pass, and `prefix + i` `r` on a pending PR arms the fast cadence
  immediately. Known gap: a push that sends a settled PR back to pending is
  noticed only by the slow check refresh, up to `prCheckRefreshSeconds` later,
  because the identity batch carries no head SHA — adding `headRefOid` to it is
  the follow-up.
- **Icons:** override the 9 glyphs (linear/github/pending/success/failure/
  merged/closed/conflict/draft) via `enrich.icons`; defaults are nerd-font
  glyphs. The `#` escape: Nix replaces `#` with `##` in icon values for tmux
  format safety.
- **Conflicts:** `@pr_mergeable` (lowercase `mergeable`/`conflicting`/`unknown`
  from gh) is written with the other `@pr_*` options; `conflicting` wins the
  badge glyph over check state and renders red.
- **Drafts:** `@pr_draft` (`1`/empty, from gh `isDraft`) is additive, not a
  state — the draft glyph *prepends* the check-state glyph and leaves the color
  precedence alone, because a draft PR still runs CI. Terminal states carry no
  marker (gh clears `isDraft` on merge; a closed PR already reads as dead). The
  rule lives three times: `build_window_label` (shell), `enrichstate.Draft`
  (Go, the enrich card), and the picker's `colorPRBadge`.
- **Review and auto-merge:** `@pr_review` (`approved`/`changes_requested`/
  `review_required`/empty, from gh `reviewDecision`) and `@pr_auto_merge`
  (`1`/empty, from gh `autoMergeRequest`) tint and decorate the badge's `#<n>`
  half independently of the glyph half's check-state color: green for
  approved, red for changes requested, dim overlay for review required,
  underlined on top of any of those when auto-merge is queued. Open PRs only;
  with no review decision the `#<n>` keeps the glyph half's tint.
- **Progress:** while a rollup is pending, `collapse_check_rollup` also sets
  `REPLY_PROGRESS` to `<finished>/<total>`; `pr_pie_glyph` turns that into an
  `nf-md-circle_slice_1`…`8` glyph at index `finished * 7 / total`, replacing
  the plain pending glyph so a check-state badge shows how far the rollup has
  gotten. The eight slices live twice — `ENRICH_PIE_GLYPHS` (shell) and
  `enrichstate.PieSlices` (Go) must stay byte-identical — and are not part of
  `enrich.icons`: they're not user-configurable.
- **Display test:** `./tests/test-display.sh` after `nix build .#default`
  (manual; not in `nix flake check`).

## Agent Usage Limits

Per-agent rate-limit utilization (Claude/Codex/Cursor) on the top-right of
status line 0. Enabled by default via `programs.tmux-og.agentUsage.enable`.

- **Cache files are the source of truth.** `tmux-agent-usage` (driven by the
  `@og-usage-tick` monitor hook, not status-format[0] — a control-mode
  client renders no status line, so a status-driven poller never runs on a
  host whose only clients are bridges, #603) drives provider scripts that
  normalize vendor API responses into `/tmp/og-agent-usage/<agent>.json`
  (`{windows:[{label,pct}], monthly:{label,pct}}`); `tmux-statusline` (Go)
  only reads them — it never curls.
- **Auth is the CLIs' own.** Each provider extracts the token from the CLI's
  credential file and hits the same endpoint the CLI's own usage view uses.
  No configured API keys; an expired/absent token just skips the refresh.
- **Both sides gate on "an agent is running".** The poller skips passes and
  the Go renderer hides the segment (live `list-panes -a` scan, basenames
  normalized for nix's `.foo-wrapped`) when no pane runs a manifest command.
  In the poller that gate precedes the `.last-tick` stamp, and the order is
  load-bearing: stamping first meant every agent-free tick spent a refresh
  cycle, so the first tick after an agent started was refused and the segment
  reappeared showing the previous session's numbers for one more window. The
  `#()` path stays cheap either way — the stamp's mtime check short-circuits
  first, so the scan forks at most once per `refreshSeconds`.
- **Monthly is threshold-gated** (`monthlyThreshold`, default 50): the monthly
  spend window renders only at/above that utilization; short windows (5h, 7d)
  are always on. Colors: <70 green, <90 peach, ≥90 red.
- **Reset countdowns**: providers pass each window's reset time through as
  `reset_at`; the renderer appends `↻<dur>` only to windows at ≥90% — the
  moment the reset starts to matter.
- **Cursor has no short windows.** Its DashboardService exposes only the
  monthly spend hard limit (`GetHardLimit`, cents) vs the billing cycle's
  aggregated usage-based cost (`GetAggregatedUsageEvents.totalCostCents`),
  plus `percentOfBurstUsed` (rendered as a `burst` window when nonzero).
  Fully pooled plans sit at 0% and stay hidden below the monthly threshold.


## Window cwd tracking

- **`@window_cwd_seen`** — window-scoped memo of the cwd `tmux-update-icons` last reconciled from: a shadow of a value no tmux hook reports, the same shape as `@crew_seen` shadowing `@crew_name`. tmux has no cwd-change hook, so a window created in repo A whose pane then `cd`s into repo B would otherwise keep A's `@worktree`/`@branch`/`@git_root` forever, and every downstream consumer — the status label, the window-picker row, the `prefix + i` card — describes the wrong repository. **The window's cwd is its first non-floating pane's path, not the active pane's**: keying on the active pane would make `prefix + o` between panes in two repos re-tag the window each way — options rewritten, `@pr_*` cleared, a forced `gh` query, two reflows — on a keystroke that changed nothing; floats are excluded because a focused float *is* the active pane, so a `prefix + b` shell float would otherwise become the window's identity. Consequence, stated plainly: a split window whose *other* pane moves is not re-stamped. The re-derive fires iff two conditions both hold. `under(@worktree)` (`tmux-worktree-match.sh`'s `under()`: equal, or `<base>/`-prefixed and NOT `<base>/.worktrees/`-prefixed, so a nested worktree isn't misread as inside its parent) is the fork-free one — a window whose stamps already describe its cwd, or a `cd` deeper into the same worktree, costs nothing and writes nothing, which is what keeps a 40-window tmux-remux restore from firing 40 reconciles. The memo bounds what containment cannot settle: after a move into a non-git directory the reconciler exits without writing, so `@worktree` still names the old repo and containment stays false forever — the memo is what makes that one fork rather than one per tick. The reconcile is targeted by the memoised pane's `%id`, never `<session>:<index>` — `renumber-window` is on, so an index captured before a backgrounded fork can slide onto a neighbour, and a pane-id target additionally pins `tmux-reconcile-window`'s own `display-message` to that exact pane. The memo is written by `tmux-update-icons`, not the reconciler — a reconciler that exits before writing (non-git cwd) would never memoise at all. `@bridge_win` mirror windows are skipped entirely: their labels are daemon-owned, and a local re-stamp is the two-writer race documented elsewhere in this file. Limit: `tmux-update-icons` runs from `status-format[0]`, which tmux evaluates only for a client drawing a status line, so a server whose only clients are control-mode never ticks and the re-derive never fires there — the same condition every existing 1s poller has (#596).
