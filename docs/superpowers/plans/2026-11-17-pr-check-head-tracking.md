# PR check badge: follow the head commit, never trust an unregistered rollup (#1170)

## Root cause (from `picker/prenrich`, evidence in the issue)

- **(a) Stale failure.** The cheap identity poll (`identityBatchField`, every
  `prRefreshSeconds`) carries no head SHA. Only the heavy checks query
  (`checksBatchFields`) has `headRefOid`, and it runs on the settled
  `prCheckRefreshSeconds` clock unless the last applied state was `pending`.
  A cached `failure` for head `316b882c` therefore survives a push to
  `13606cca` until the next checks refresh (≤ 300s) and arms no fast poll.
- **(b) Premature success.** Right after open/push the rollup holds only entries
  that are already settled (`auto-merge` `SKIPPED`, lint `SUCCESS`); the build
  jobs are not registered yet. `collapseRollup` maps an all-settled set to
  `success`, which is not `pending`, so no fast re-poll is armed either.

## Rules chosen

1. **Head change is visible to the identity poll.** `prFields` gains
   `headRefOid,createdAt` (the `--force` suffix drops its own `headRefOid`, which
   would otherwise duplicate). Identity caches now carry the PR's live head.
2. **Mismatch = stale.** (Open PRs only.) If the identity cache head is non-empty and the checks
   cache head is empty (no checks cache, or the PR was absent from the last
   checks batch: a PR first seen late) or differs (open PR, `url|` caches included
   because `fetchURLPR` now stores `headRefOid` in its checks object): the pass refetches checks now (one
   `refreshRepoChecks`, which the repo group does at most once per pass) and
   `applyCache` reports `pending` (no progress) until the checks cache matches.
   No per-tick fetch for an unchanged head.
3. **Settling grace for a new head.** A per-branch stamp
   `<key>.head-seen` (`<oid>\n<first-seen epoch>`) records when the poller first
   saw the identity head. Stamp absent: first-seen = the PR's `createdAt`
   (a brand-new PR is in grace, an old PR after a cache wipe is not; no
   `createdAt` → 0 → no grace). Stamp holds another oid: first-seen = now (a push).
   Applies to open PRs only, and only when the identity head is non-empty (empty
   head: no grace, no stamp written; covers pre-upgrade caches, the batch-failure
   fallback and URL-mode JSON). Stamp written with `writeAtomic`.
   While `now - first-seen < headGraceSeconds` (120), a collapsed `success` or
   `none` is reported as `pending` (no progress, a failure still reads failure).
   120s is a floor on registration lag, not a sighting deadline: a PR first seen
   later than that is covered by the empty-checks-head stale rule above, which
   refetches and shows pending until the checks cache holds the head.
   Reasoning: head-registration lag is seconds, and a count-based rule cannot
   cover a new PR (no prior count). The pending marker then re-polls checks on
   the 30s clock until the grace ends, so the badge settles without waiting for
   a slow refresh.
4. **`SKIPPED` alone is not success.** `collapseRollup`: a rollup whose entries
   are all `SKIPPED` collapses to `pending` (no progress), never `success`/`none`.
   Why not `none`: every renderer (`enrichstate.Classify`, PRCOLOR in
   `tmux-reflow-windows.sh`/`tmux.conf.tmpl`, `colorPRBadge`, the enrich card)
   falls through to green for any check value other than failure/pending, so
   `none` would still show a success glyph. `pending` reads correctly in all of
   them with no renderer change. Trade-off, stated in the PR: a PR whose every
   check is genuinely skipped shows a pending pie until a real check appears; it
   polls at the bounded pending cadence (30s, 60s after 10 min, then
   `prCheckRefreshSeconds` after 30 min). Offered as a follow-up: a neutral
   render of "no verified checks" across the renderers.
5. The pending-marker fingerprint uses the identity head when present (falls
   back to the checks head), so a push re-arms the fast clock even when the
   checks refetch has not landed. Both fingerprint sites (`pass.go`
   `enrichRepoGroup`, `main.go` `runSingleTarget`) use the same helper.

## Budget

- Mismatch refetch: +1 `gh pr list` (checks selection) per repo per pass only
  on the pass that first sees the new head; a successful fetch ends it. Worst
  case if the checks call keeps failing: the mismatch reads `pending`, which arms
  the pending marker, so checks retry on the pending clock (≈40 queries in the
  first 30 min, then settled cadence, per the existing worst case) plus +1 per
  identity pass (`prRefreshSeconds`, default 120s) while the mismatch persists.
  Unchanged heads add no calls. A brand-new PR costs the same one checks query as
  a push (identity head set, checks head empty).
- Grace: no extra gh calls beyond the existing pending clock (30s tier, ≤ 4
  polls in the 120s window); identity head costs nothing (field on an existing
  query).

## File list

- `picker/prenrich/fetch.go`: `prFields` adds `headRefOid,createdAt`; force suffix loses `headRefOid`.
- `picker/prenrich/rollup.go`: all-`SKIPPED` → `pending`.
- `picker/prenrich/head.go` (new): `identityHead`, `headStale`, `headSettling` + stamp I/O.
- `picker/prenrich/apply.go`: `applyCache` applies stale/grace → `pending`.
- `tests/enrich-fork.bats`, `tests/enrich-budget.bats`: pinned gh argv strings (identity batch gains `headRefOid,createdAt`; forced per-head query ends `...autoMergeRequest,headRefOid,createdAt,statusCheckRollup,isCrossRepository`). Their batch fixtures carry no head/createdAt, so behaviour is unchanged (no mismatch, no grace). `rg -n autoMergeRequest tests/` before the gates to confirm no other pin.
- `docs/agents/scripts.md`: add `<key>.head-seen` to the `tmux-pr-enrich` owned files.
- `picker/prenrich/main.go`: `runSingleTarget` fingerprint uses the helper.
- `picker/prenrich/pass.go`: `enrichRepoGroup` forces a checks refresh on mismatch; fingerprint uses identity head; `identityBatchField` follows `prFields`; `fetchURLPR` stores `headRefOid` in its checks object (view fields now include it).
- Tests: `rollup_test.go`, `apply_test.go`, `pass_test.go`, `fetch_test.go` (update pinned query strings); `head_test.go` (new, written in Step 3 with the helpers).
- `docs/agents/enrichment.md`: head-tracking, grace, budget.

## Steps

- [ ] **Step 1: red tests** (`rollup_test.go`, `pass_test.go`, `apply_test.go`).
  (a) pass test: checks cache `failure` for head A, identity batch reports head B,
  checks batch reports B pending: assert one checks-batch call and
  `@pr_check_state pending`; (a3) brand-new PR seen late: identity head H with
  `createdAt` older than 120s and checks cache `[]`/absent: one checks-batch call
  and `@pr_glyph` is not `ic.Success` before the refetch lands; sibling case head unchanged: zero checks-batch calls.
  (a2) checks refetch fails: `applyCache` writes `pending`, not `failure`.
  (b) `[SKIPPED, SUCCESS]` rollup (and an all-`SKIPPED` rollup, at any age: `pending`, glyph is the pending icon not `ic.Success`) on a head first seen < 120s ago (new PR via
  `createdAt`; push via stamp holding another oid) writes `pending`; the same at
  ≥ 120s writes `success`; old PR with no stamp writes `success` at once; `failure`
  inside grace stays `failure`. Rollup test: all-`SKIPPED` → `pending`. Entry points: `runFullPass(false)` for (a), `runFullPass`/`runSingleTarget` for (b), asserting `@pr_check_state` and `@pr_glyph` via the fake runner. Record these red failures on the unmodified tree; Step 4 is a second confirmation.
  Command: `cd picker && go test ./prenrich/ -run 'Head|Skipped|Stale|Grace'`;
  expect FAIL (assertion failures, not compile errors: add the new field names to
  fixtures only).
- [ ] **Step 2: rollup rule** (`rollup.go`): count settled non-skipped entries;
  all entries `SKIPPED` (and not failed) → `pending`.
  `go test ./prenrich/ -run Skipped` → PASS.
- [ ] **Step 3: head tracking** (`fetch.go`, new `head.go`, `apply.go`, `pass.go`):
  implement rules 1, 2, 3, 5. Update pinned query strings in `fetch_test.go` and
  `pass_test.go` (`identBatchArgs`, `headFields`, `urlViewArgs`, `ghArgs`) and the
  two bats files listed above.
  `go test -race ./prenrich/...` → PASS, all of Step 1 green.
- [ ] **Step 4: counterfactual red evidence.** In a scratch copy
  (`mktemp -d` under the scratchpad, `git worktree`-free `cp -r picker`) restore
  the old `applyCache`/`enrichRepoGroup`/`collapseRollup` and run the Step 1
  tests; record the assertion failures for the PR.
- [ ] **Step 5: docs** (`enrichment.md`, `scripts.md`; note `.head-seen` stamps are never
  pruned like the existing caches, the restart/upgrade no-grace edge, the URL-mode wait): head in identity query, mismatch rule,
  grace stamp, skipped rule, worst-case budget.
- [ ] **Step 6: gates**: `nix build .#lint --accept-flake-config`,
  `nix flake check --no-build`, `nix flake check`, `nix build .#default`.

## Consumer map (checks/identity cache edges)

| consumer | edge | disposition |
| --- | --- | --- |
| `applyCache` (pass, single-target, url) | reads identity head, stamp | changed, tests |
| `rollupFor` / `collapseRollup` | skipped rule | changed, tests |
| `armPendingMarker` fingerprint | identity head | changed, test |
| `fetchURLPR` | stores `headRefOid` in its checks object so the empty-checks-head rule does not fire permanently; grace via the same field | changed, test |
| `writeFetched` (`--force`, per-head) | `headRefOid` now in `prFields`; ident keeps it, checks cache gets it | changed, existing test updated |
| shell/picker readers of `/tmp/og-pr/*.json` | extra fields only | compatible (rg: none read identity JSON) |
| `@pr_check_state` readers (reflow PRCOLOR, `enrichstate.Classify`, `colorPRBadge`, enrich card, bridge `@bridge_pr_check_state`) | value set unchanged; all-`SKIPPED` now emits `pending`, which every reader already renders | compatible |
| URL mode / `runURL` / single-target URL fallback | ignore `applyCache`'s return and are skipped by the pending pass, so a grace-pending badge there waits for the next full pass after its 60s TTL | compatible, documented |

## Acceptance

- PR body: causes, rules, budget (this file) → Step 6 write-up.
- Red/green tests → Steps 1, 3, 4.
- lint / flake check --no-build / flake check / build → Step 6.
- CI run URL on x86_64-linux and aarch64-darwin → after push.

## Post-cap amendment (third critic pass, unresolved at the revision cap)

The third pass returned `revise` with one blocking finding: an open PR outside
the top-100 batch has a non-empty identity head (via the `--head ... --state all`
fallback) and a permanently empty checks head, so Rule 2 would refetch every
identity pass and pin the badge to pending. Resolution, adopted without a further
critic pass and carried to the PR body under `## Escalated`:

- One predicate, `headStale(cache)`, used both to force the refetch and to show
  `pending`. The *empty*-checks-head case is stale only while the checks cache
  predates our observation of the head: checks file absent, or its mtime is
  strictly older than the `.head-seen` stamp's mtime (the stamp is rewritten only
  when the head changes). The *differing non-empty* case is always stale. After
  one refetch that still lacks the PR, the checks file is newer than the stamp, so the rule stops firing: at most one
  refetch per head, and the PR keeps today's no-rollup rendering.
- Tests: pass test with identity batch missing the branch, `--head <br> --state all`
  returning an OPEN PR with `headRefOid`, a checks batch without it, two
  consecutive full passes with a fresh `.last-check-tick` (`p.setTick`): assert no
  checks-batch call on the second pass.
- Test-setup notes from the critic: Step 1 (a)/(a3) set `.last-check-tick` to now
  so `refreshChecks` is false (else the old tree also refetches and (a) cannot go
  red); keep the old query-string prefixes in Step 1 and change them in Step 3;
  (a3) is made observable by a failing checks batch (old tree: `none`, success
  glyph); the batch-failure fallback uses `prFields`, so it does carry a head.
