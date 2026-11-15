# Port PR enrichment to Go (#1108) — plan

Spec: `docs/superpowers/specs/2026-11-15-pr-enrich-go-design.md` (accepted). Read it
first; this plan does not restate its behaviour list.

All Go commands run from `picker/`. Every Go `exec.Command`/file read with a
non-constant argument carries a per-line `//nolint:gosec // <why>` (the repo's
suppression rule); no path or linter exclusions.

## File list

| File | Purpose |
|---|---|
| `picker/enrichstate/enrichstate.go` | add `Icons` + `Glyph()` (badge glyph half); `Pie` digits-only |
| `picker/enrichstate/glyph_test.go` (new) | the badge-precedence/draft/pie cases moved from `enrich.bats` |
| `picker/enrichstate/enrichstate_test.go` | signed-progress Pie case |
| `picker/main.go` | `colorPRBadge` on `enrichstate.Classify`/`ReviewColor`/`AutoMerge` |
| `picker/enrich_test.go` | keep existing `colorPRBadge` cases green (add a conflicting+review row if absent) |
| `picker/prenrich/args.go` (new) | argv loop → `opts` (modes, targets, mock values, config flags, `cfgArgs` for re-exec) |
| `picker/prenrich/runner.go` (new) | `runner` interface (`output`, `run`, `detach`, `lookPath`) + `execRunner` (PATH exec, `Setsid` detach) |
| `picker/prenrich/cache.go` (new) | `sha1Key`, `srvSuffix`, `cacheDecision`, mkdir lock, atomic write, mtime helpers, marker read/arm |
| `picker/prenrich/rollup.go` (new) | `collapseRollup` |
| `picker/prenrich/sanitize.go` (new) | `sanitizeTitle` |
| `picker/prenrich/gh.go` (new) | `fetchPRCached`, `sameRepoPR`, `refreshRepoChecks`, `refreshURLPR`, `prURLWellformed` |
| `picker/prenrich/apply.go` (new) | `applyCache`, `writePROptions`, `notifyPRChange`, `logEvent` (lib-log exec under sentinel) |
| `picker/prenrich/pass.go` (new) | `runFullPass`, `enrichRepoGroup`, `glyphSync`, pending markers |
| `picker/prenrich/main.go` (new) | mode dispatch (mock, tick-run, tick-run-pending, url, single-target, tick gate); always exit 0 |
| `picker/prenrich/*_test.go` (new) | unit tests per file against a fake runner + `t.TempDir()` cache |
| `picker/prenrich/testdata/sanitize-title.tsv` (new) | shared sanitizer vectors (Go + bats) |
| `picker/prenrich/testdata/rollup-*.json` (moved) | the eight `tests/fixtures/rollup-*.json`, only `enrich.bats` uses them |
| `picker/default.nix` | `"prenrich"` in `subPackages`; `mv $out/bin/prenrich $out/bin/tmux-pr-enrich-go` |
| `scripts/tmux-pr-enrich.sh` | replaced by the exec shim (spec §shim) |
| `config/tmux.conf.nix` | `mkScriptEnrich`: `@pr_enrich_bin@`, `@enrich_icon_*@` (escapeShellArg); lib-enrich drops the 7 state/draft icon substitutions |
| `scripts/lib-enrich.sh` | remove pie/rollup/cache/sha1/state-icon code; `build_window_label` new signature |
| `scripts/tmux-reflow-windows.sh` | FMT reads `@pr_glyph`; reset + call updated |
| `scripts/tmux-reconcile-window.sh` | unset `@pr_glyph` with the other `@pr_*` |
| `tests/helper.bash` | `make_pr_enrich` substitutes `@pr_enrich_bin@` (+ lazy build) and icon placeholders |
| `tests/enrich.bats` | moved cases deleted; naming tests' args updated; composition + vectors tests added |
| `tests/reflow-fanout.bats` | `@pr_glyph S` fixture in the two `" S #12"` tests |
| `tests/reconcile.bats` | unset list gains `@pr_glyph` |
| `tests/test-display.sh` | `@pr_glyph` next to its `@pr_state` fixtures |
| `tests/fixtures/rollup-*.json` | moved to `picker/prenrich/testdata/` |
| `flake.nix` | `PR_ENRICH_BIN` on enrich-budget/url/fork + notify-producers checks; `SANITIZE_TITLE_VECTORS` on enrich-tests |
| `docs/agents/enrichment.md`, `docs/agents/scripts.md`, `CLAUDE.md` | ownership/docs updates |

## Steps

- [ ] **Step 1: failing enrichstate glyph tests** — `picker/enrichstate/glyph_test.go`:
  table test of `Glyph(Icons{Pending:"P",Success:"S",Failure:"F",Merged:"M",Closed:"X",Conflict:"C",Draft:"D"}, state, check, mergeable, draft, progress)`
  with every `build_window_label` badge case from `tests/enrich.bats` lines 230–310
  and 578–590 (expected value = the `REPLY_PR` text between the leading space and
  ` #<n>`, e.g. `"D S"`, `"M"`, `"X"`, `"C"`, `"D C"`, pie `PieSlices[2]`, `"D "+PieSlices[4]`).
  In `enrichstate_test.go` add `Pie("+3/5") == ""` and `Pie("3/+5") == ""`.
  Run `go test ./enrichstate/` → fails to compile (`Glyph` undefined).
- [ ] **Step 2: implement `Glyph` + digits-only `Pie`** — `enrichstate.go`: `type Icons struct{ Pending, Success, Failure, Merged, Closed, Conflict, Draft string }`;
  `Glyph` maps `Classify`'s glyph role to the icon, pending → `Pie(progress)` else
  `Icons.Pending`, then prefixes `Icons.Draft + " "` when `Draft(state, draft)`.
  `Pie`: reject any non-digit in either half before `Atoi`. Update the PieSlices
  doc comment (no shell twin any more). `go test ./enrichstate/` → PASS.
- [ ] **Step 3: `colorPRBadge` on enrichstate** — `picker/main.go`: replace the
  inline colour switch with `enrichstate.Classify(state, check, mergeable)`'s colour
  role mapped to `prColors` fields (helper `prColor(role, c)`, `default` arm for
  `exhaustive`), the `#<n>` tint from `ReviewColor`, underline from `AutoMerge`.
  Before editing, run `go test -run 'PRBadge' .` and confirm the existing table
  passes; add a row for conflicting+approved if no row covers review on a
  conflicting PR. After: `go test -run 'PRBadge' .` → PASS, output unchanged.
- [ ] **Step 4: failing prenrich pure-helper tests** — new package `picker/prenrich`
  (package main, stub `main()`). Tests (all fail to compile first):
  `args_test.go` — modes and precedence incl. unknown args ignored, `--mock-pr-number`
  sets mock, config flags (`--refresh-seconds`, `--check-refresh-seconds`,
  `--reflow`, `--notify`, `--lib-log`, `--icon-{pending,success,failure,merged,closed,conflict,draft}`)
  land in opts and in `cfgArgs` (exactly the config pairs, never `--force`/mode/targets),
  a trailing flag with no value is ignored;
  `cache_test.go` — `sha1Key("a|b")` equals `printf 'a|b' | sha1sum`; `srvSuffix`
  (`/tmp/sock A,1,0` → `.srv_tmp_sock_A`, empty TMUX → ""); every `pr_cache_decision`
  case from `enrich.bats` (lines ~476–538) incl. the escaped-title case, plus a
  malformed-JSON content → TTL (the "TTL_TERMINAL defaults to TTL when omitted" case
  has no analogue once the TTLs are constants — named as dropped in the PR body); pending-clock clamps (check-refresh 20 → pending
  20/slow 20/fast 20);
  `rollup_test.go` — every `collapse_check_rollup` case incl. progress `1/3`,
  settled → no progress, malformed → `none`, reading the moved
  `testdata/rollup-*.json` (git mv them from `tests/fixtures/`);
  `sanitize_test.go` — reads `testdata/sanitize-title.tsv` (columns: name, input,
  expected; `\r \n \e \t \\` escapes) covering CR/LF/ESC strip, `'` `#` strip, `|`→space,
  300-char ASCII → 256, a 120-char title whole, and one multibyte over-length case;
  `gh_test.go` — `prURLWellformed` accepts `https://github.com/a/b/pull/12`, rejects
  trailing slash, `/pulls/`, non-digit, http, extra segment; `sameRepoPR` drops
  cross-repo, keeps first same-repo, strips `isCrossRepository`, bad JSON → `[]`.
  Run `go test ./prenrich/` → compile failure.
- [ ] **Step 5: implement args/runner/cache/rollup/sanitize/sameRepoPR** — files per
  the file list; `execRunner.output(dir, name, args...)` sets `cmd.Dir` when non-empty
  and returns stdout with trailing newlines trimmed; `detach` = `Start` with
  `SysProcAttr{Setsid:true}`, nil stdio, `Process.Release()`. Lock: `os.Mkdir(<key>.lock)`;
  on `EEXIST` a dir older than `OG_LOCK_STALE_SECONDS` (default 60) is removed and the
  mkdir retried once; a non-dir is removed and retried; release = `os.Remove`.
  Atomic write = `<path>.tmp.<pid>` + rename. `go test ./prenrich/` → PASS.
- [ ] **Step 6: failing fetch/apply tests** — fake runner records every argv (and
  `cmd.Dir`) and answers `gh`/`tmux`/`git` from per-test maps. Tests:
  `fetchPRCached` open+all: open hit → one call; open empty → second `--state all`
  call; argv exactly `pr list --head <b> --state open --limit 100 --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,isCrossRepository`;
  force adds `,statusCheckRollup,headRefOid` before `,isCrossRepository` and splits
  identity/checks caches (`[{statusCheckRollup, headRefOid}]`, rollup null → `[]`);
  gh failure leaves an existing cache byte-identical; gh absent → path returned, no
  call; held fresh lock → no call; fresh cache → no call; fork-only answer caches `[]`.
  `applyCache`: missing file → no tmux call, applied ""; `[]` → writes `none` + empty
  fields + empty glyph; a full PR → `set-option -t T -w` for `@pr_number @pr_title
  @pr_state @pr_check_state @pr_url @pr_mergeable @pr_draft @pr_review
  @pr_auto_merge @pr_check_progress @pr_glyph @pr_branch` in that order, one
  invocation each, lowercased enums, `isDraft`/`autoMergeRequest` → `1`/"",
  sanitized title; checks cache preferred over an inline legacy rollup; malformed
  identity JSON → no write. `writePROptions`: prev display-message format ends
  `|#{@pr_check_progress}|#{@pr_glyph}`; unchanged prev → no reflow detach; changed →
  detach `<reflow> <session> --force` with session from `display-message -t T -p
  '#{session_name}'`; notify cases: merged flip (wins over check flip), check flip to
  failure/success from non-empty prior, empty prior silent, pending/closed silent,
  `--notify @notify@` → never; argv `emit --source pr --level <l> --window T --title
  <h> --body '#<n> <title>'`. `logEvent`: no sentinel → no exec; sentinel +
  `--lib-log /x` → `bash -c 'source "$1"; shift; log_event "$@"' _ /x enrich event pr …`;
  `--lib-log @lib_log@` → no exec. Run → fails.
- [ ] **Step 7: implement gh.go fetch + apply.go** — to the tests; glyph via
  `enrichstate.Glyph(opts.icons, …)`. `go test ./prenrich/` → PASS.
- [ ] **Step 8: failing pass tests** — fake runner + temp cache, fixed `now`:
  grouping by git common dir with `@git_root` retry; empty branch and `@bridge_win=1`
  skipped; gone checkout + well-formed url → url group; 31 unique branches → 30
  processed and no stale-marker sweep; stale marker of this srv removed, another
  srv's marker kept; identity batch argv exact; batch hit writes cache, batch-ok miss
  → `--state all` lookup, batch failure → open+all lookup; checks batch only when
  `.last-check-tick` is stale or the marker is due; pending → marker written
  (`<epoch>\n<sha1 of sorted unique "br|oid" lines joined by \n>\n`) and
  `.last-pending-tick` removed; same fingerprint → content kept; no pending → marker
  removed; pending-only pass runs only due repos and pushes `.last-pending-tick`
  `CHECK_REFRESH` ahead when no marker is inside `PENDING_MAX`; url refresh argv
  `pr view <url> --json number,title,url,state,mergeable,isDraft,reviewDecision,autoMergeRequest,statusCheckRollup`,
  failure → log + cache untouched. **Glyph sync**: list-windows format is the six
  existing fields (with `#{s/[|]/ /:@pr_url}`) + `@pr_number|@pr_state|@pr_check_state|@pr_mergeable|@pr_draft|@pr_check_progress|@pr_glyph`;
  6-field rows parse with empty trailing fields; capped, branchless and
  gone-without-url windows with stale glyph get exactly one `@pr_glyph` write each;
  an up-to-date window and a mirror get none; two fixed windows in one session →
  one reflow detach; before each sync write the window's seven PR options are
  re-read with one `display-message` and the glyph recomputed from that read (a
  concurrent writer's newer values win); no-PR (`""`/`none`) → wants empty glyph;
  the sync runs in both `--tick-run` and `--tick-run-pending`. Run → fails.
- [ ] **Step 9: implement pass.go** (implement: escalated) — goroutine per repo group
  and per url group, `sync.WaitGroup` join before the pending-tick push-ahead;
  marker/tick stamps exactly as the spec's full-pass/repo-group bullets; no shared
  mutable state across goroutines except via the filesystem (per-group results
  returned, not written to package vars). `go test -race ./prenrich/` → PASS.
- [ ] **Step 10: failing mode tests** — `run(opts)` against fake runner: mock with
  empty `--target` → no tmux call; mock goes through `writePROptions` (prev snapshot,
  reflow detach on change, notify) writing values + glyph, no gh, no mkdir; url mode: malformed url → nothing; mirror refusal;
  forces refetch; tags with `@branch` from `show-options -t T -wqv @branch`;
  single-target: mirror refusal; `--dir` unresolvable → window `@pr_url` path, bad
  url → nothing; resolvable → open+all fetch, apply, pending arms marker with
  `sha1("<branch>|<oid>")`; tick gate: `.last-tick` stale → touch + detach
  `<self> <cfgArgs…> --tick-run`; fresh + no marker → nothing; fresh + marker +
  `.last-pending-tick` fresh → nothing; else touch it + detach `--tick-run-pending`;
  `--force` bypasses the gate but is not forwarded. Run → fails.
- [ ] **Step 11: implement main.go dispatch** — `main()` = `run(parseArgs(os.Args[1:]), execRunner{}, os.Getenv)`,
  self path from `os.Executable()`, always `os.Exit(0)`. `go test -race ./prenrich/ && go vet ./prenrich/` → PASS.
- [ ] **Step 12: Nix packaging + shim** — `picker/default.nix`: add `"prenrich"` to
  `subPackages`, `mv $out/bin/prenrich $out/bin/tmux-pr-enrich-go` (vendorHash
  unchanged: stdlib only). `scripts/tmux-pr-enrich.sh` → shim:
  `exec @pr_enrich_bin@ --refresh-seconds @pr_refresh_seconds@ --check-refresh-seconds @pr_check_refresh_seconds@ --reflow @reflow@ --notify "${OG_NOTIFY_BIN:-@notify@}" --lib-log @lib_log@ --icon-pending @enrich_icon_pending@ … --icon-draft @enrich_icon_draft@ "$@"`
  with a header comment naming the Go package and the placeholder contract.
  `config/tmux.conf.nix` `mkScriptEnrich`: add `@pr_enrich_bin@` →
  `"${goTools}/bin/tmux-pr-enrich-go"` and the seven `@enrich_icon_*@` →
  `lib.escapeShellArg enrichIconsRaw.<k>` (the `lib-enrich` derivation keeps its
  icon substitutions until Step 14 removes their consumers). Verify: `nix build .#default` → success, then
  `p=$(nix-store -qR ./result | grep -- '-tmux-pr-enrich$'); cat "$p/bin/tmux-pr-enrich"`
  → an absolute `/nix/store/…/bin/tmux-pr-enrich-go`, numeric refresh values, a store
  `tmux-reflow-windows`, a store `lib-log`, and seven quoted icons; no `@…@` left
  except inside `${OG_NOTIFY_BIN:-…}` when notifications are off.
- [ ] **Step 13: test harness** — `tests/helper.bash` `make_pr_enrich`: resolve
  `bin="${PR_ENRICH_BIN:-}"`; empty → `go build -C "$PWD/picker" -o "$BATS_RUN_TMPDIR/tmux-pr-enrich-go" ./prenrich`
  once (skip the build when that file exists); substitute `@pr_enrich_bin@` and the
  seven icon placeholders (`P S F M X C D`). `flake.nix`: add
  `PR_ENRICH_BIN = "${(import ./picker {inherit pkgs lib;})}/bin/tmux-pr-enrich-go";`
  to `enrich-budget-tests`, `enrich-url-tests`, `enrich-fork-tests`,
  `notify-producers-tests`. Then `bats tests/enrich-budget.bats tests/enrich-url.bats
  tests/enrich-fork.bats tests/notify-producers.bats tests/enrich-command.bats
  tests/update-icons-enrich-trigger.bats` (devshell) → all pass with **no edits to
  those .bats files**. Any failure is a port bug: fix the Go, never the test.
- [ ] **Step 14: shell consumers** — `scripts/lib-enrich.sh`: delete
  `ENRICH_PIE_GLYPHS`, `pr_pie_glyph`, `collapse_check_rollup`, `pr_cache_decision`,
  `branch_sha1`, `ENRICH_ICON_{PENDING,SUCCESS,FAILURE,MERGED,CLOSED,CONFLICT,DRAFT}`;
  `build_window_label MODE PROVIDER ISSUE_ID ISSUE_TITLE PR_NUMBER PR_GLYPH BRANCH PANE_PATH [TASK] [AI_NAME]`,
  `REPLY_PR=" ${glyph:+$glyph }#${num}"` under the existing empty/`none` guard; header
  comment updated (glyph composed by the poller). `scripts/tmux-reflow-windows.sh`:
  FMT replaces `#{@pr_state}|#{@pr_check_state}|#{@pr_mergeable}|#{@pr_draft}|#{@pr_check_progress}`
  with `#{@pr_glyph}` and the matching `read` variables; the branch-mismatch reset
  clears the glyph var; both `build_window_label` calls use the new args.
  `scripts/tmux-reconcile-window.sh`: add `@pr_glyph` to its unset list.
  `config/tmux.conf.nix` `lib-enrich`: drop the pending…draft substitutions (same
  commit as the `lib-enrich.sh` change, so no build renders a raw placeholder).
  `rg -n 'prstate|prcheck|prmerge|prdraft|prprog|ENRICH_PIE|pr_pie_glyph|collapse_check_rollup|pr_cache_decision|branch_sha1' scripts/`
  → no hits. `shellcheck scripts/lib-enrich.sh scripts/tmux-reflow-windows.sh scripts/tmux-reconcile-window.sh scripts/tmux-pr-enrich.sh` → clean.
- [ ] **Step 15: shell tests** — `tests/enrich.bats`: delete the `branch_sha1`,
  `collapse_check_rollup`, `pr_cache_decision`, `pr_pie_glyph` tests and the badge
  cases moved in Step 1; rewrite remaining `build_window_label` calls to the new
  signature keeping every assertion; add `build_window_label: PR segment is " <glyph> #<n>"`
  (`… 9 "D S" br /x` → `" D S #9"`), the empty-glyph form (`" #9"`), and a vectors test
  reading `${SANITIZE_TITLE_VECTORS:-$BATS_TEST_DIRNAME/../picker/prenrich/testdata/sanitize-title.tsv}`
  through `sanitize_title` (multibyte row only when the locale is UTF-8).
  `flake.nix` enrich-tests: `SANITIZE_TITLE_VECTORS = ./picker/prenrich/testdata/sanitize-title.tsv;`.
  `tests/reflow-fanout.bats`: `tmux set -wq -t S:0 @pr_glyph S` in the two tests
  asserting `" S #12"` from `@pr_state`/`@pr_check_state`. `tests/reconcile.bats`:
  add `@pr_glyph` to both loops and the test name's count. `tests/test-display.sh`:
  set `@pr_glyph` beside each `@pr_state` fixture. Run `bats tests/enrich.bats
  tests/reflow-fanout.bats tests/reconcile.bats` → pass.
- [ ] **Step 16: docs** — `docs/agents/enrichment.md`: the Drafts and Progress
  bullets say the glyph is composed once in `enrichstate.Glyph` by the Go poller
  (`picker/prenrich`, binary `tmux-pr-enrich-go` behind the `tmux-pr-enrich` shim)
  and written as `@pr_glyph`; the glyph-sync freshness rule and the icon-change
  delay; drop the "lives three times"/"live twice" text. `docs/agents/scripts.md`:
  `tmux-pr-enrich` row → shim exec'ing the Go poller, options it owns gain
  `@pr_glyph`; `tmux-reflow-windows`/`tmux-reconcile-window` rows if they list
  `@pr_*` reads. `CLAUDE.md`: remove the pie-glyph and draft-badge items from
  "Duplicated tables"; add `sanitize_title` ↔ `prenrich.sanitizeTitle`, pinned by
  `picker/prenrich/testdata/sanitize-title.tsv`; update the `lib-enrich.sh`
  shared-libraries line (no `collapse_check_rollup`/`pr_pie_glyph`); the
  "Who runs what" `@og-pr-tick` entry stays `tmux-pr-enrich`.
  Commit this plan and the spec with the code (`docs/superpowers/plans/2026-11-15-pr-enrich-go.md`,
  `docs/superpowers/specs/2026-11-15-pr-enrich-go-design.md`).
- [ ] **Step 17: full gate** — `nix build .#default`, `nix flake check`,
  `nix build .#lint --accept-flake-config` → all succeed.

## Acceptance

- [ ] Six named bats suites pass unchanged — Step 13 (local run) + `nix flake check`
  (`enrich-budget-tests`, `enrich-url-tests`, `enrich-fork-tests`,
  `enrich-command-tests`, `update-icons-enrich-trigger-tests`, `enrich-tests`);
  `git diff main -- tests/enrich-budget.bats tests/enrich-command.bats tests/enrich-fork.bats tests/enrich-url.bats tests/update-icons-enrich-trigger.bats`
  is empty; `tests/enrich.bats`' diff is the moved-to-Go cases + signature update,
  justified in the PR body (spec §Tests).
- [ ] CLAUDE.md duplicated-table entries for pie glyphs and draft badge removed —
  Step 16; `rg -n 'ENRICH_PIE_GLYPHS|draft-badge rule' CLAUDE.md scripts picker` → no hits.
- [ ] `nix build .#lint --accept-flake-config` passes — Step 17.
- [ ] `nix flake check` passes — Step 17.
- [ ] `nix build .#default` passes — Steps 12, 17.
