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
- **Stamp failures:** when a provider CLI runs but a matched id ends up with
  no title/url, `tmux-issue-stamp` stores the sanitized first line of the
  CLI's stderr in `@issue_stamp_error` (cleared on a successful stamp or when
  no provider matches). The enrich card's issue block permanently shows
  `no url — <reason>` whenever `@issue_url` is empty; pressing `o` on that
  window additionally flashes the same reason instead of silently no-op'ing.
  Local-only, never carried across the remote bridge (`@bridge_*`).
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

Per-agent rate-limit utilization (Claude/Codex/Cursor/pi) on the top-right of
status line 0. Enabled by default via `programs.tmux-og.agentUsage.enable`.

- **Cache files are the source of truth.** `tmux-agent-usage` (driven by the
  `@og-usage-tick` monitor hook, not status-format[0] — a control-mode
  client renders no status line, so a status-driven poller never runs on a
  host whose only clients are bridges, #603) drives provider scripts that
  normalize vendor API responses into `/tmp/og-agent-usage/<agent>.json`
  (`{windows:[{label,pct,reset_at?}], monthly:{label,pct,reset_at?},
  spend:{label,usd,period,limit_usd?}}`); `tmux-statusline` (Go) only reads them — it
  never curls. `spend` is optional: absent in an older cache or a provider
  that has none, it decodes as `nil` (`usageCache.Spend *usageSpend`,
  `picker/statusline/usage.go:20`) and the segment simply skips the `$`
  figure for that agent; when present it renders unconditionally, no
  threshold. `spend.limit_usd` is the provider's own known spending cap in
  USD — cursor's `GetHardLimit.hardLimit` (cents ÷ 100), pi's OpenRouter
  `/api/v1/key` `limit` (already USD) — and the key is omitted, not nulled,
  when no cap is set; present, the renderer shows `$<spend>/$<limit>`.
  No schema-version field was added for this — every cache field
  added since the format shipped has been an optional sibling (`monthly`,
  `reset_at`, `spend`, now `limit_usd`), so an old or new poller and an old or new
  renderer already interoperate by omission alone; a later remote-mirror
  follow-up should keep adding optional siblings rather than inventing a
  version scheme.
- **Auth is the CLIs' own.** Each provider extracts the token from the CLI's
  credential file and hits the same endpoint the CLI's own usage view uses.
  No configured API keys (pi is the one exception — see below); an
  expired/absent token just skips the refresh.
- **The gate is per-agent, not global.** Go's `openAgents()`
  (`picker/statusline/usage.go:68`) and bash's `OPEN` assoc array
  (`scripts/tmux-agent-usage.sh`'s `scan_open_agents`) each do their own
  `list-panes -a` scan and key the result by agent, not by "any agent
  anywhere": `usageSegment` drops an agent's block when `open[agent]` is
  false even if another agent's cache and pane both exist, and the poller
  forks each provider only when that provider's own command is in `OPEN`.
  The "gate before stamp" invariant carries over unchanged at this
  granularity: `scan_open_agents` still runs before `.last-tick` is touched,
  so a tick with no agent open spends nothing, and the first tick after an
  agent (re)appears isn't refused by a stamp a gate-less pass would have
  already written. New at per-agent granularity: `--tick-run` clears the
  cache file of every manifest command *not* currently in `OPEN` before
  forking providers. Without that, per-agent gating would reintroduce the
  exact "reappears showing the previous session's numbers" bug the
  gate-before-stamp ordering exists to prevent — just scoped to one agent:
  close cursor, leave claude running, and cursor's stale cache would sit
  untouched (nothing clears it) until cursor reopens and wins a refresh
  window, showing a dead session's spend in the meantime.
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
  `totalCostCents` is also written as `spend` (USD, billing cycle),
  unconditionally — no hard limit needed for the dollar figure to show; a
  set hard limit adds `spend.limit_usd`.
- **pi is OpenRouter-keyed, not pi's own token.** `tmux-agent-usage-pi.sh`
  reads `~/.pi/agent/auth.json`'s `.openrouter.key` and hits OpenRouter's
  `/api/v1/key` endpoint. pi's own key value supports a small syntax —
  `!cmd` (run a shell command), `$VAR`/`${VAR}` (env interpolation), `$$`/`$!`
  (escape a literal leading `$`/`!`), anything else literal — and the
  provider implements only the parts safe for a background status tick:
  `!cmd` is deliberately never executed (a tick must not run commands from a
  config file, so that case resolves to an empty token and falls through),
  `$$`/`$!` unescape correctly, and `$VAR`/`${VAR}` resolves only a
  whole-value variable name (`^[A-Za-z_][A-Za-z0-9_]*$`) via `${!var}` —
  a composite like `${A}_${B}` is left unresolved rather than risk expanding
  a malformed name. Known gap, documented in the script: pi also consults the
  credential's own `.openrouter.env` object before the process environment,
  and that object is not read here, so a key stored only there falls through
  to `$OPENROUTER_API_KEY` same as no key at all. With no resolvable token
  the script exits 0 and leaves the previous cache untouched, same as every
  other provider's failed-fetch case. OpenRouter has no short rate-limit
  windows (`windows` is always `[]`); `monthly` is computed from
  `limit_remaining` (`100 * (limit - limit_remaining) / limit`), not a naive
  `usage/limit`, so it stays correct regardless of what `limit_reset` says;
  `limit_reset` (`daily`/`weekly`/`monthly`/null) maps to the window's
  `label` (`day`/`wk`/`mo`/`cap` for a null reset — a lifetime cap). `spend`
  is `usage_monthly` (USD, current UTC calendar month), always written
  regardless of whether the key carries a cap. A nonzero `limit` is written
  as `spend.limit_usd` independently of `monthly` — a cap whose
  `limit_remaining` is null still shows its budget. `limit`/`limit_remaining`
  need no cents→USD conversion: they're the same "credits" unit as
  `usage_monthly`, and OpenRouter's docs (openrouter.ai/docs/faq) state
  credits are USD-denominated 1:1.
- **Mirror/remote panes never count toward the *local* gate.** Both
  `openAgents()` and `scan_open_agents` key strictly off
  `pane_current_command`/the manifest basenames and never look at
  `@bridge_proc`; #513 once let a bridge mirror's `@bridge_proc` open the
  (then-global) gate. At per-agent granularity that would be actively wrong,
  not just imprecise: it would show a remote agent's column sourced from a
  *local* cache file the local poller never refreshes for that agent, since
  the remote agent's usage lives on a different host entirely. This still
  holds unchanged — what changed is that a mirror session no longer needs the
  local gate at all: it renders through its own path (below) with the
  remote's own gate and caches.
- **A mirror session renders `@bridge_usage`, not the local set, at all.**
  The poller (`tmux-agent-usage`) additionally publishes the surviving cache
  files onto the global `@og_agent_usage` option, and the bridge daemon ships
  a sanitized, gate-applied copy onto the mirror session's `@bridge_usage`;
  `tmux-statusline` selects between the two sources by `@bridge_host`, never
  falling back to local caches/gate on a mirror. Full detail — the publish
  option, the live subscription gate, the sanitizer, lifecycle and known
  limits — is in `bridge-shipped-state.md`'s "Remote Agent Usage".


## Window cwd tracking

- **`@window_cwd_seen`** — window-scoped memo of the cwd `tmux-update-icons` last reconciled from: a shadow of a value no tmux hook reports, the same shape as `@crew_seen` shadowing `@crew_name`. tmux has no cwd-change hook, so a window created in repo A whose pane then `cd`s into repo B would otherwise keep A's `@worktree`/`@branch`/`@git_root` forever, and every downstream consumer — the status label, the window-picker row, the `prefix + i` card — describes the wrong repository. **The window's cwd is its first non-floating pane's path, not the active pane's**: keying on the active pane would make `prefix + o` between panes in two repos re-tag the window each way — options rewritten, `@pr_*` cleared, a forced `gh` query, two reflows — on a keystroke that changed nothing; floats are excluded because a focused float *is* the active pane, so a `prefix + b` shell float would otherwise become the window's identity. Consequence, stated plainly: a split window whose *other* pane moves is not re-stamped. The re-derive fires iff two conditions both hold. `under(@worktree)` (`tmux-worktree-match.sh`'s `under()`: equal, or `<base>/`-prefixed and NOT `<base>/.worktrees/`-prefixed, so a nested worktree isn't misread as inside its parent) is the fork-free one — a window whose stamps already describe its cwd, or a `cd` deeper into the same worktree, costs nothing and writes nothing, which is what keeps a 40-window tmux-remux restore from firing 40 reconciles. The memo bounds what containment cannot settle: after a move into a non-git directory the reconciler exits without writing, so `@worktree` still names the old repo and containment stays false forever — the memo is what makes that one fork rather than one per tick. The reconcile is targeted by the memoised pane's `%id`, never `<session>:<index>` — `renumber-window` is on, so an index captured before a backgrounded fork can slide onto a neighbour, and a pane-id target additionally pins `tmux-reconcile-window`'s own `display-message` to that exact pane. The memo is written by `tmux-update-icons`, not the reconciler — a reconciler that exits before writing (non-git cwd) would never memoise at all. `@bridge_win` mirror windows are skipped entirely: their labels are daemon-owned, and a local re-stamp is the two-writer race documented elsewhere in this file. Limit: `tmux-update-icons` runs from `status-format[0]`, which tmux evaluates only for a client drawing a status line, so a server whose only clients are control-mode never ticks and the re-derive never fires there — the same condition every existing 1s poller has (#596).
