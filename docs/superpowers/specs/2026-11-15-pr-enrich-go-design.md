# Port PR enrichment to Go (#1108) — design

## Problem

`scripts/tmux-pr-enrich.sh` (795 lines) plus the parts of `scripts/lib-enrich.sh`
it uses parse `gh` JSON with `jq`, compute check rollups and cache decisions in
bash, and — through `build_window_label` — compose the PR badge glyph. The same
badge rules also live in Go (`picker/enrichstate`, used by the enrich card) and
the picker's `colorPRBadge`, so CLAUDE.md carries two "must stay byte-identical"
entries maintained by hand:

- the pie glyph table: `ENRICH_PIE_GLYPHS` (shell) ↔ `enrichstate.PieSlices` (Go);
- the draft-badge rule: `build_window_label` (shell), `enrichstate.Draft` (Go),
  and the picker's `colorPRBadge` (which re-implements `Classify`'s colour
  precedence plus the review/auto-merge tint inline).

Goal: the poller in Go, reusing `enrichstate`; each of those tables authored in
exactly one place; the poller's driver, option contract, gh/fork budget and
failure modes unchanged.

## Decision: where the badge glyph is computed

The badge glyph half (draft marker + state glyph or pie slice) has two consumers:
`tmux-reflow-windows` (status bar and picker, via `@window_pr_plain`) and the
enrich card (Go). Three ways to get one source were considered:

| Option | Single source? | Cost |
|---|---|---|
| A. Generate the shell pie array from a shared data file (Nix substitution) | Pie: yes. Draft/precedence rule: **no** — it is logic, still written in bash and Go | Smallest diff; fails the acceptance item for the draft entry |
| B. Reflow asks Go for every label | Yes | Rewrites the reflow hot path; a fork per reflow |
| **C. The Go poller writes the composed glyph as a window option `@pr_glyph`; reflow only concatenates** | **Yes** — `enrichstate` owns precedence, draft rule and pie; shell holds none of them | `build_window_label`'s PR arguments shrink to number + glyph; one new option |

**Chosen: C.** It follows the existing contract — window options are the source
of truth and only the stamp/enrich writers set `@pr_*` — and the poller already
knows every input (state, rollup, mergeable, draft, progress). The user icon set
(`enrich.icons`, raw dialect, the same strings lib-enrich receives today) is
passed to the Go binary as flags, as the enrich card already does.

`@pr_glyph` = `<draft-icon> <state-glyph>` or `<state-glyph>`, where state-glyph is
merged > closed > conflict > failure > pending (pie slice when progress is usable,
else the pending icon) > success — byte-identical to today's `build_window_label`
output between `" "` and `" #<n>"`. Written on every `write_pr_options` alongside
the other `@pr_*`, empty for the "no PR" write. `build_window_label` then builds
`REPLY_PR=" ${glyph} #${number}"` (or `" #${number}"` when the glyph is empty —
only possible between upgrading and the first full pass's glyph sync), keeping
today's guard that an empty or `none` number yields no segment. The reflow's
branch-mismatch reset clears the glyph with the other PR fields.

**Freshness invariant.** Every non-mirror window carrying a `@pr_number` other
than `none` has `@pr_glyph` equal to the glyph `enrichstate` composes from its own
`@pr_state`/`@pr_check_state`/`@pr_mergeable`/`@pr_draft`/`@pr_check_progress`
and the current icon flags. `write_pr_options` keeps it for every window a pass
applies; the windows a pass deliberately skips (empty `@branch`, past the 30-entry
cap, no cache file yet, gone checkout without a well-formed `@pr_url`) still hold
leftover `@pr_*` that the reflow renders. So every `--tick-run` and
`--tick-run-pending` pass starts with a gh-free **glyph sync**: the pass's single
`list-windows` format is widened with `@pr_number`, `@pr_state`,
`@pr_check_state`, `@pr_mergeable`, `@pr_draft`, `@pr_check_progress` and
`@pr_glyph` (appended after the six existing fields, parsed tolerantly: missing
trailing fields read empty, so the budget fakes' 6-field rows still parse), and
before the branch/bridge/cap filters each non-`@bridge_win` window whose
recomputed glyph differs from its `@pr_glyph` first has its seven PR options
re-read with one `display-message` and the glyph recomputed from that read (so a
concurrent writer's newer values win); only if that still differs does it get one
`set-option` and its session one forced reflow. A window with no PR (`@pr_number` empty or `none`)
wants an empty glyph. An `enrich.icons` change therefore lands on every window at
the next full pass (≤ `prRefreshSeconds`, since a reload does not reset
`.last-tick`) rather than at the activation reflow as today — an accepted delay
for a rare config change.

`colorPRBadge` is rewritten on top of `enrichstate.Classify` (colour role),
`ReviewColor` and `AutoMerge`, mapping roles to its `prColors`; output unchanged.

End state of the two CLAUDE.md entries: removed. `PieSlices`, `Draft` and the
glyph precedence exist only in `enrichstate`; shell has no pie table, no draft
rule and no state-glyph precedence.

## Architecture

### New binary: `tmux-pr-enrich-go` (`picker/prenrich`, package main)

Added to `picker/default.nix` `subPackages` and renamed in `postInstall`, like the
other picker-set binaries. Stdlib + `enrichstate` only (small, fast start — it runs
from a 5s monitor hook). Logic split into testable functions over a `runner`
interface (exec `tmux`/`gh`/`git` via `PATH`, so the bats fakes keep working) and
the cache dir.

### `scripts/tmux-pr-enrich.sh` becomes a shim

```bash
exec @pr_enrich_bin@ --refresh-seconds @pr_refresh_seconds@ \
  --check-refresh-seconds @pr_check_refresh_seconds@ --reflow @reflow@ \
  --notify "${OG_NOTIFY_BIN:-@notify@}" --icon-pending @enrich_icon_pending@ … "$@"
```

`mkScriptEnrich` keeps producing `bin/tmux-pr-enrich`, so every caller is untouched:
the `@og-pr-tick` hook string (asserted in `flake.nix`), `tmux-issue-stamp`'s
`@pr_enrich@`, the bridge daemon's `enrich-refresh` (`tmux-pr-enrich` by name on
the remote), the `og pr` verb, `notify-conf-assertions`. Icon values are
substituted shell-quoted (`lib.escapeShellArg`). A `--notify` value still starting
with `@` means notifications off — the existing single "off" mechanism.

Per-tick cost: today one bash process sourcing lib-enrich + lib-log; after, one
bash `exec` (no sourcing) into one Go process whose gate is in-process (stat +
compare). No extra fork.

### Behaviour ported verbatim

Every rule below is the bash behaviour; the Go port keeps it exactly.

- **Modes and precedence**: hand-rolled argv loop (unknown args ignored, never a
  flag-parse error; always exit 0): mock → `--tick-run` → `--tick-run-pending` →
  `--target`+`--url` → `--target`+`--branch` → tick gate.
- **Tick gate**: `.last-tick<srv>` vs `--refresh-seconds`; inside the window, a
  `*.checks-pending<srv>` glob and `.last-pending-tick<srv>` vs
  `PENDING_CHECK_SECONDS` decide a `--tick-run-pending` dispatch; else touch
  `.last-tick` before dispatching `--tick-run`. Dispatch = re-exec of
  `os.Executable()` with the same config flags, `Setsid`, stdio to `/dev/null`,
  not waited on (the `nohup … &` detach). `<srv>` = `.srv${TMUX%%,*}` with
  `[^A-Za-z0-9._-]` → `_`, empty outside tmux (#705).
- **Cadence constants**: TTL 60, TTL_NONE 15, TTL_TERMINAL 3600,
  PENDING_CHECK 30 / SLOW 60 / FAST 600 / MAX 1800 / DUE_SLACK 5, with the same
  clamps against `--check-refresh-seconds`.
- **Cache**: `$OG_ENRICH_CACHE_DIR` (default `/tmp/og-pr`); keys are
  `sha1hex("<git-common-dir>|<branch>")` and `sha1hex("url|<url>")`; files
  `<key>.json` (identity, one-element array or `[]`), `<key>.checks.json`
  (`[{statusCheckRollup, headRefOid}]`), `<key>.lock` (mkdir lock, stale after
  `OG_LOCK_STALE_SECONDS`, default 60, released when the fetch returns),
  `<repo-key>.checks-pending<srv>` (line 1 first-seen epoch, line 2 fingerprint),
  `.last-tick`, `.last-check-tick`, `.last-pending-tick` (+`<srv>`). Writes are
  tmp-file + rename. JSON is re-encoded by Go (key order may differ from jq's);
  no reader depends on byte layout once the terminal-state test parses JSON (see
  below). `isCrossRepository` is stripped before caching.
- **Cache decision** (`pr_cache_decision`): force or missing → fetch; `[]` → TTL_NONE;
  `[0].state` MERGED/CLOSED → TTL_TERMINAL; else TTL. The bash substring match is
  replaced by a parse of `[0].state` — identical on every well-formed cache, and
  immune to an escaped `"state":"MERGED"` inside a title by construction.
- **gh calls**: identical argv, field lists and order, so the budget tests'
  substring fakes match: per-head `pr list --head <b> --state open|all --limit 100
  --json <fields>,isCrossRepository` (force adds `statusCheckRollup,headRefOid`);
  identity batch `pr list --state open --limit 100 --json number,title,url,state,
  mergeable,isDraft,reviewDecision,autoMergeRequest,headRefName,isCrossRepository`;
  checks batch `--json headRefName,statusCheckRollup,headRefOid,isCrossRepository`;
  url mode `pr view <url> --json number,title,url,state,mergeable,isDraft,
  reviewDecision,autoMergeRequest,statusCheckRollup`. gh runs with `cmd.Dir` = the
  repo checkout; gh absent → serve cache; non-zero exit → cache untouched.
- **Full pass**: one `tmux list-windows -a -F '#{session_id}:#{window_id}|#{@worktree}
  |#{@git_root}|#{@branch}|#{@bridge_win}|#{s/[|]/ /:@pr_url}'`, rows with ≥5 fields (the `s/[|]/ /` keeps a `|` in the url from shifting
  the glyph-sync fields that follow it); skip
  empty branch and `@bridge_win=1`; repo = `git -C <@worktree or @git_root>
  rev-parse --path-format=absolute --git-common-dir`, retrying `@git_root`; gone
  checkout + well-formed `@pr_url` → url group; 30-entry cap shared by both, a
  truncated pass removes no stale markers; stale-marker sweep of this server's
  markers only; per-repo due/fast computation; each repo group and url group runs
  concurrently (goroutines, joined before the pending-tick push-ahead).
- **Repo group**: identity batch → per-branch cache write, else (batch ok)
  `--state all` terminal lookup, else (batch failed) the open+all lookup; checks
  batch when due; apply to every window of the branch; pending fingerprint =
  `sha1hex(sort -u of "<branch>|<headRefOid>\n" lines, trailing newline stripped
  as $() does)`; arm or clear the marker.
- **Apply / write**: rollup collapse (`collapse_check_rollup`: none / failure set /
  pending set incl. unfinished CheckRun / success; progress `<finished>/<total>`
  only when pending, malformed → none); title through `sanitize_title`; one
  `tmux set-option -t T -w <opt> <val>` exec per option, same order, plus
  `@pr_glyph`; prev snapshot via one `display-message` whose format gains a
  trailing `|#{@pr_glyph}` (fields 2–3 still state/check, so the notify parser and
  `FAKE_PREV` fixtures keep working); changed → detached `<reflow> <session>
  --force`; notify only on a flip to merged (wins) or a check flip to
  failure/success from a non-empty prior, via detached `<notify> emit --source pr
  --level … --window T --title … --body "#<n> <title>"`.
- **Single-target / url modes**: `@bridge_win` refusal, `--dir` repo resolution,
  gone checkout → window's `@pr_url`, url mode forces a refetch and tags with the
  window's `@branch`; pending result arms the marker (`<branch>|<headRefOid>` fp).
- **Mock mode**: writes the given values (sanitized title) plus the composed
  `@pr_glyph`; no gh.
- **Logging**: no Go twin of `lib-log.sh`. The shim passes `--lib-log @lib_log@`;
  Go tests the sentinel itself (`$OG_DEBUG_SENTINEL`, default `/tmp/og-debug.on`,
  a stat — zero cost when debug is off) and only when it exists runs
  `bash -c 'source "$1"; shift; log_event "$@"' _ <lib-log> enrich event pr …`,
  so format, rotation and the sentinel stay owned by `lib-log.sh`. Same two
  events (`enrich pr`, `enrich pr_url_fail`) and key/value pairs.
- **Malformed cache files** (only reachable by hand-editing; writes are
  tmp+rename): the cache decision falls back to TTL as bash's substring test
  does; apply skips the write and keeps the last-known options instead of bash's
  write of empty fields — the same "never wipe on a bad read" rule the missing-cache
  path already follows.

### Title sanitizer: new duplication, made mechanical

`sanitize_title` stays in shell (`tmux-issue-stamp-{github,linear}` use it for
issue titles) and gains a Go twin for PR titles. Rather than a third hand-kept
entry, both are pinned by one vectors file `picker/prenrich/testdata/sanitize-title.tsv`
(input → expected, escape-encoded), read by a Go test and by a new bats test —
the `mirrorname` vectors pattern. Go truncates at 256 runes. Bash counts
characters only under a UTF-8 locale (the bats check sets `C.UTF-8`); under a C
locale `${clean:0:256}` counts bytes. So the vectors' over-length cases are
ASCII, plus one multibyte case asserted under the UTF-8 locale only; Go's
rune-safe cut is the intended behaviour for PR titles.

### Shell side after the port

`lib-enrich.sh` loses `ENRICH_PIE_GLYPHS`, `pr_pie_glyph`, `collapse_check_rollup`,
`pr_cache_decision`, `branch_sha1`, the state/draft icon constants and the badge
precedence. It keeps the issue-identity helpers (`branch_to_*`,
`parse_explicit_issue_id`, `provider_priority_list`, `sanitize_title`,
`sanitize_stamp_error`, `truncate_ellipsis`), `ENRICH_CACHE_DIR`/`ENRICH_SRV`/
`ENRICH_STAMP_LOCK_DIR` (used by `tmux-issue-stamp`), `ENRICH_ICON_LINEAR/GITHUB`,
`split_pr_badge`, and `build_window_label` with the new signature
`MODE PROVIDER ISSUE_ID ISSUE_TITLE PR_NUMBER PR_GLYPH BRANCH PANE_PATH [TASK] [AI_NAME]`.
`tmux-reflow-windows` reads `#{@pr_glyph}` instead of state/check/mergeable/
draft/progress in its FMT. `tmux-reconcile-window` unsets `@pr_glyph` with the
other `@pr_*`.

## Tests

- **Unchanged and passing**: `enrich-budget.bats`, `enrich-command.bats`,
  `enrich-fork.bats`, `enrich-url.bats`, `update-icons-enrich-trigger.bats`,
  `notify-producers.bats` — they drive the shim through `make_pr_enrich` with
  stub `gh`/`tmux`/`git` on `PATH`.
- **Harness change (implementation detail)**: `tests/helper.bash`
  `make_pr_enrich` also substitutes `@pr_enrich_bin@` with `$PR_ENRICH_BIN` and
  the seven state/draft `@enrich_icon_*@` with the single-letter test icons `setup_lib_enrich`
  uses (it already substitutes `@lib_log@`)
  (the flake checks set it to the built binary, like `PICKER_GENERATE`); unset,
  it builds `picker/prenrich` once per bats run into `$BATS_RUN_TMPDIR`. The
  affected check derivations gain the env var.
- **`enrich.bats` changes (justified as implementation detail)**: the tests of
  functions whose implementation moved to Go — `branch_sha1`,
  `collapse_check_rollup`, `pr_cache_decision`, `pr_pie_glyph`, and the
  `build_window_label` badge-precedence cases (glyph choice, draft, pie) — move
  to Go table tests with the same inputs and expected outputs (`enrichstate`
  for the glyph; `prenrich` for rollup/cache/key). The remaining
  `build_window_label` naming tests keep every assertion; only their argument
  lists change to the new signature, plus two cases pinning
  `REPLY_PR=" <glyph> #<n>"` composition and the empty-glyph form.
  `enrichstate.Pie` is tightened to digits-only (`strconv.Atoi` accepts `+3/5`,
  the bash regex did not), so the moved pie cases — plus a signed input — pass
  unchanged in meaning. Keeping the
  old tests unchanged would require keeping the shell copies the task removes.
- **`reflow-fanout.bats` fixture change**: the two badge tests that set
  `@pr_state open`/`@pr_check_state success` and expect `" S #12"` also set
  `@pr_glyph S` — the reflow's input is now the option the poller writes. No
  assertion changes. `reconcile.bats`'s unset list gains `@pr_glyph`. The manual
  `tests/test-display.sh` sets `@pr_glyph` next to its `@pr_state` fixtures.
- **Glyph sync tests (Go, against a fake runner)**: a capped window, a branchless
  window and a gone-checkout window without url, each holding `@pr_*` with a
  stale/empty `@pr_glyph`, get the recomputed glyph; an up-to-date window gets no
  write; a mirror gets none.
- **New Go tests**: argv/mode parsing, tick gate decisions, cache decision,
  rollup collapse, pending fingerprint, full-pass grouping/cap, glyph composition
  (every precedence row, draft, pie), sanitizer vectors, `colorPRBadge` unchanged
  output.

## Non-goals

- `tmux-issue-stamp*` stay bash.
- The status-bar's tmux-format colour conditionals (generator) are untouched.
- No change to the bridge daemon: mirrors keep shipping the remote's
  `@window_pr_plain` as `@bridge_pr_plain`.

## Upgrade

A window with `@pr_number` but no `@pr_glyph` renders `" #<n>"` until the next
full pass's glyph sync writes it (≤ `prRefreshSeconds`), skipped windows
included. Caches written by the
bash poller are read as-is (same file names, same JSON fields).

## Docs

`docs/agents/enrichment.md` (glyph ownership, `@pr_glyph`, the Go binary),
`docs/agents/scripts.md` (tmux-pr-enrich row: shim → Go), CLAUDE.md (drop the two
duplicated-table entries; add the sanitize-title vectors pin; shared-libraries
line for lib-enrich).
