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
  checks alone via the `--tick-run-pending` pass, so a running check suite
  doesn't sit stale for a whole `prCheckRefreshSeconds` window. The fast
  cadence is stepped and bounded: 30s for the first 10 minutes of a pending
  episode, 60s from 10 to 30 minutes, then `prCheckRefreshSeconds` until the
  checks settle. A required status stuck at `EXPECTED` or a deployment awaiting
  approval therefore stops costing the shared GraphQL bucket after 30 minutes;
  the gate keeps dispatching a gh-free pending pass every 30s during the 60s
  tier, so the extra cadence costs forks, not API calls. An **episode** is the
  set of pending heads, not the repo: the marker's content holds a first-seen
  epoch and a fingerprint of the sorted pending `branch|headRefOid` set (the
  checks query carries `headRefOid`), and a changed set — a new PR going
  pending, one settling, or a push moving a head — rewrites the epoch and drops
  `.last-pending-tick`, restarting the 30s tier. So a second PR is never stuck
  on an old repo clock, and a push that sends a pending PR back to pending
  re-arms the fast window at the next checks refresh. Worst case for one repo:
  ~40 check queries over the first 30 minutes, then 12/hour, when the pending
  set is stable; a churning set restarts the 30s tier each change, so sustained
  churn can approach ~120/hour plus the full-pass baseline. A marker whose repo
  has lost its windows is removed by the next pass, and `prefix + i` `r` on a
  pending PR arms the fast cadence immediately. Upgrade note: a one-line marker
  written by an older build has no fingerprint, so the first pass after upgrade
  restarts the window once.
- **Checkout gone:** a window whose `@worktree` and `@git_root` both fail to
  resolve a repo (a merged PR's worktree was removed, the window stayed open)
  but which carries a well-formed `@pr_url` is refreshed by that url —
  `gh pr view <url>`, no cwd — and written through the same
  `apply_cache_to_target` path. Cache key is derived from `url|<url>` so it
  can't collide with a `repo|branch` key; TTLs are the usual ones (merged/closed
  `TTL_TERMINAL`, open `TTL`). These windows count against the same 30-entry cap
  and are skipped in the checks-only pending pass. No `@pr_url` (or a url that
  isn't `https://github.com/<owner>/<repo>/pull/<N>`) still means skipped, and
  `@bridge_win` mirrors stay skipped. Without this the badge stayed `open`
  forever (#921).
- **Fork PRs are ignored.** `gh pr list --head <b>` matches the head branch
  *name* across forks, so a fork PR whose branch is also called `main` used to
  shadow the repo's own PR — and a window merely sitting on the default branch
  with no PR of its own showed a long-merged fork's state forever (#926). Every
  `gh pr list` that maps a PR to a branch — the per-head lookup, the identity
  batch and the check-rollup batch — now asks for `isCrossRepository` and drops
  cross-repo PRs before picking by `headRefName`, with the per-head lookup's
  `--limit` raised past the fork PRs that could share the name. The field is
  stripped before the answer is cached, so the cached JSON shape is unchanged.
  A window already stamped with a fork PR clears to no PR on the next refresh.
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
  normalize vendor API responses into
  `$XDG_RUNTIME_DIR/og-agent-usage-<uid>/<agent>.json` (else `$TMPDIR` or
  `/tmp`; `OG_AGENT_USAGE_DIR` overrides)
  (`{windows:[{label,pct,reset_at?}], monthly:{label,pct,reset_at?},
  spend:{label,usd,period,limit_usd?,remaining_usd?,remaining_label?},
  balance:{usd_remaining}?}`);
  `tmux-statusline` (Go) only reads them — it
  never curls. The dispatcher owner-checks the cache dir (real directory, not
  a symlink, owned by the caller's uid, no group/other permission bits;
  `owner_only_dir` in `lib-log.sh`, Go twin `ownerdir.OwnerOnly`) before any
  read/write, creating it `0700` and never chmod'ing an existing one — a
  dir another account got to first is refused, not trusted, and the segment
  simply shows nothing for that refresh. `spend` is optional: absent in an older cache or a provider
  that has none, it decodes as `nil` (`usageCache.Spend *usageSpend`,
  `picker/statusline/usage.go:20`) and the segment simply skips the `$`
  figure for that agent; when present it renders unconditionally, no
  threshold. `spend.limit_usd` is the provider's own known spending cap in
  USD — cursor's usage-summary `individualUsage.overall.limit` (cents ÷ 100), pi's OpenRouter
  `/api/v1/key` `limit` (already USD) — and the key is omitted, not nulled,
  when no cap is set; present, the renderer shows `$<spend>/$<limit>`.
  `spend.label` renders as a trailing suffix (e.g. `$1.20 mo`) for every
  agent except cursor — see the pi bullet below for why cursor is pinned
  unchanged. `balance` is a separate top-level optional sibling
  (`usageCache.Balance *usageBalance`) carrying a provider's remaining
  prepaid account balance, independent of `spend`/`limit_usd` (a per-key
  spending cap and an account's overall remaining balance answer different
  questions); when present it renders as its own `$<amt> acct left` clause
  — distinct from `spend.remaining_usd`'s `$<amt> left` clause (see the pi
  bullet below), so the two are never confused — joined onto the spend
  clause with ` · ` (or standing alone if a provider ever reports a balance
  with no spend). No schema-version field was added for this — every cache
  field added since the format shipped has been an optional sibling
  (`monthly`, `reset_at`, `spend`, `limit_usd`, now `balance`), so an old
  or new poller and an old or new renderer already interoperate by
  omission alone; a later remote-mirror follow-up should keep adding
  optional siblings rather than inventing a version scheme.
- **Auth is the CLIs' own.** Each provider extracts the token from the CLI's
  credential file and hits the same endpoint the CLI's own usage view uses.
  No configured API keys (pi is the one exception — see below); an
  expired/absent token just skips the refresh. Cursor's token is Linux
  `${XDG_CONFIG_HOME:-~/.config}/cursor/auth.json` or the macOS keychain item
  `cursor-access-token`; the account comes from `cli-config.json` (else the
  token's JWT `sub`), and the call authenticates with
  `Cookie: WorkosCursorSessionToken=<account>::<token>` passed through
  `curl -K -` on stdin so it never reaches argv. The dispatcher forks cursor
  on an open `cursor-agent` pane alone — no `auth.json` presence test, since
  the token may be in the keychain or under `$XDG_CONFIG_HOME`, and the
  provider exits 0 silently when it finds none. Do not restore a file gate.
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
- **Cursor reads the dashboard's usage-summary.** `GET
  https://cursor.com/api/usage-summary` is the dashboard's own endpoint; the
  legacy DashboardService figure counted on-demand usage only and read 0% on
  team/enterprise plans (#944). Monthly pct = `individualUsage.overall`
  used/limit (team shape), else max(`autoPercentUsed`, `apiPercentUsed`) of
  `individualUsage.plan`; `reset_at` = billing-cycle end. `spend` = overall
  used/limit in USD, team shape only — the individual shape carries no
  dollars, so no `spend`. `isUnlimited` → no `monthly`, and `spend` without
  `limit_usd`. There is no burst window anymore. Renderer decision: `spend`
  renders unconditionally, so `$406/$1050` shows below the monthly threshold;
  the `%` stays threshold-gated because below it the dollar pair already
  conveys fullness and at/above it the colour-graded `%` adds the warning.
  Cursor's `$used/$limit` form is unchanged, so
  `TestUsageSegmentCursorRenderUnchanged` stands and
  `TestUsageSegmentCursorDollarsBelowThreshold` pins the enterprise case.
- **`tmux-agent-usage-cursor --print` — public contract.** One-shot: needs no
  `OG_AGENT_USAGE_DIR`, writes no cache, applies no agent-open gate. Prints
  one JSON object plus newline and exits 0; on failure stdout is empty and
  the exit code is 2 no token, 3 no account, 4 fetch failed, 5 unrecognised
  response. Fields:
  - `plan_type` — `membershipType` (string|null); `unlimited` — bool.
  - `cycle.starts_at` / `cycle.resets_at` — epoch seconds; both set or both
    null.
  - `plan.used_pct` — raw unrounded percent, null only when `unlimited`;
    `plan.used_usd` / `plan.limit_usd` — USD, null on the individual shape;
    `limit_usd` is null unless > 0.
  - `pools.auto_pct` / `pools.api_pct` — raw percents, null when absent.
  - `on_demand[]` — `{scope: "individual"|"team", enabled, used_usd (USD|null),
    limit_usd (USD; null = uncapped)}`, every pool present, enabled or not. A
    numeric `limit_usd` <= 0 is passed through, so test `limit_usd > 0`
    before comparing.

  A consumer derives `credits_cover` = any enabled pool with `limit_usd` null
  or `used_usd < limit_usd`; `limit_reached` = max of `plan.used_pct` and the
  pools >= 100, or an enabled pool with `limit_usd > 0` and
  `used_usd >= limit_usd`. The script is internal to the tmux wrapper (in
  `ogInternal`, no `og` verb): on PATH inside tmux panes via the wrapper's bin
  dir, outside tmux use its store path. It is a public interface — changes
  are additive only.
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
  credits are USD-denominated 1:1. When the key carries a cap and
  `limit_remaining` is non-null, the same `/api/v1/key` call also writes
  `spend.remaining_usd` (`limit_remaining` verbatim, same credits unit, no
  conversion) and `spend.remaining_label` (reusing the `limit_reset`→
  day/wk/mo/cap mapping `monthly.label` already uses); the renderer shows
  this as its own `$<amt> left` clause (dropping `/<label>` when the label
  is `cap`, since a lifetime cap needs no reset-period annotation), joined
  onto the spend clause with ` · ` (e.g. `$5.00/$20 mo · $18 left`).

  The "remaining credit" figure follows a 3-tier precedence, mutually
  exclusive by construction (pi's script writes at most one of
  `spend.remaining_usd` / `balance`, never both): (1) the key has a cap and
  `limit_remaining` is known → `spend.remaining_usd`/`remaining_label` as
  above; (2) no cap, but a **management** key is configured → the account's
  remaining prepaid balance via `GET /api/v1/credits`
  (`total_credits - total_usage`), written as a top-level `balance.usd_remaining`
  sibling of `spend` and rendered as its own distinct `$<amt> acct left`
  clause, so it's never confused with the per-key figure; (3) neither →
  `spend` renders alone, no "left" clause at all. `/api/v1/credits` requires
  a **management** key — OpenRouter's API returns 403 ("Only management keys
  can perform this operation") for pi's ordinary inference key, so pi's own
  `auth.json` token is never used for this call. The management key is
  resolved from `$OG_OPENROUTER_MGMT_KEY_FILE` (default
  `$XDG_CONFIG_HOME/tmux-og/openrouter-mgmt-key` — the same path
  `programs.tmux-og.agentUsage.openrouterManagementKeyFile` symlinks to via
  `mkOutOfStoreSymlink`, never copying the secret into the Nix store) or,
  failing that, the `$OPENROUTER_MANAGEMENT_KEY` env var; it is never logged
  or printed. A missing/unreadable file, unset env, a refused `/credits`
  call, or a malformed/non-object response all degrade silently to tier 3 —
  `balance` is simply omitted, and `spend`/`monthly`/`windows` (from
  `/api/v1/key`) are unaffected either way, same "failed fetch leaves the
  previous figure alone" invariant as every other failure mode here.
  The segment also appends `spend.label` (e.g. `mo`) as a trailing suffix
  — `$1.20 mo` rather than a bare `$1.20` — for every agent **except
  cursor**, whose pre-existing render (`$12/$7.50`) is pinned
  byte-identical by `TestUsageSegmentCursorRenderUnchanged`; this is a
  deliberate per-agent exception to keep that render unchanged, not a
  data-driven distinction — the label text itself still comes from the
  cache's own `Label` field, never a hardcoded string.
  No staleness cue was added for either figure: the cache carries no
  fetch-time field today (locally or across the bridge), and adding one
  would need a new `fetched_at` field, clock-skew handling in the daemon,
  and more segment width — for a figure that already refreshes every
  `@og-usage-tick` poll and, on a failed fetch, simply leaves the previous
  cached figure in place rather than showing something wrong.
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
