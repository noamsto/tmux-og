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
2. **Mismatch = stale.** If the identity cache head and the checks cache head are
   both non-empty and differ (open PR): the pass refetches checks now (one
   `refreshRepoChecks`, which the repo group does at most once per pass) and
   `applyCache` reports `pending` (no progress) until the checks cache matches.
   No per-tick fetch for an unchanged head.
3. **Settling grace for a new head.** A per-branch stamp
   `<key>.head-seen` (`<oid>\n<first-seen epoch>`) records when the poller first
   saw the identity head. Stamp absent: first-seen = the PR's `createdAt`
   (a brand-new PR is in grace, an old PR after a cache wipe is not; no
   `createdAt` → 0 → no grace). Stamp holds another oid: first-seen = now (a push).
   While `now - first-seen < headGraceSeconds` (120), a collapsed `success` or
   `none` is reported as `pending` (no progress, a failure still reads failure).
   Reasoning: head-registration lag is seconds, and a count-based rule cannot
   cover a new PR (no prior count). The pending marker then re-polls checks on
   the 30s clock until the grace ends, so the badge settles without waiting for
   a slow refresh.
4. **`SKIPPED` alone is not success.** `collapseRollup`: a settled rollup whose
   entries are all `SKIPPED` collapses to `none`, not `success`.
5. The pending-marker fingerprint uses the identity head when present (falls
   back to the checks head), so a push re-arms the fast clock even when the
   checks refetch has not landed.

## Budget

- Mismatch refetch: +1 `gh pr list` (checks selection) per repo per pass only
  on the pass that first sees the new head. Worst case if the checks call keeps
  failing: +1 per identity pass (every `prRefreshSeconds`, ≥ 10s, default 120s)
  per repo with a mismatched branch; a successful fetch ends it.
- Grace: no extra gh calls beyond the existing pending clock (30s tier, ≤ 4
  polls in the 120s window); identity head costs nothing (field on an existing
  query).

## File list

- `picker/prenrich/fetch.go`: `prFields` adds `headRefOid,createdAt`; force suffix loses `headRefOid`.
- `picker/prenrich/rollup.go`: all-`SKIPPED` → `none`.
- `picker/prenrich/head.go` (new): `identityHead`, `headStale`, `headSettling` + stamp I/O.
- `picker/prenrich/apply.go`: `applyCache` applies stale/grace → `pending`.
- `picker/prenrich/pass.go`: `enrichRepoGroup` forces a checks refresh on mismatch; fingerprint uses identity head; `identityBatchField` follows `prFields`; `fetchURLPR` keeps working (view fields now include head).
- Tests: `rollup_test.go`, `apply_test.go`, `pass_test.go`, `fetch_test.go` (update pinned query strings), `head_test.go` (new).
- `docs/agents/enrichment.md`: head-tracking, grace, budget.

## Steps

- [ ] **Step 1: red tests** (`rollup_test.go`, `pass_test.go`, `apply_test.go`).
  (a) pass test: checks cache `failure` for head A, identity batch reports head B,
  checks batch reports B pending: assert one checks-batch call and
  `@pr_check_state pending`; sibling case head unchanged: zero checks-batch calls.
  (a2) checks refetch fails: `applyCache` writes `pending`, not `failure`.
  (b) `[SKIPPED, SUCCESS]` rollup on a head first seen < 120s ago (new PR via
  `createdAt`; push via stamp holding another oid) writes `pending`; the same at
  ≥ 120s writes `success`; old PR with no stamp writes `success` at once; `failure`
  inside grace stays `failure`. Rollup test: all-`SKIPPED` → `none`.
  Command: `cd picker && go test ./prenrich/ -run 'Head|Skipped|Stale|Grace'`;
  expect FAIL (assertion failures, not compile errors: add the new field names to
  fixtures only).
- [ ] **Step 2: rollup rule** (`rollup.go`): count settled non-skipped entries;
  none and not failed and not pending → `none`.
  `go test ./prenrich/ -run Skipped` → PASS.
- [ ] **Step 3: head tracking** (`fetch.go`, new `head.go`, `apply.go`, `pass.go`):
  implement rules 1, 2, 3, 5. Update pinned query strings in `fetch_test.go` and
  `pass_test.go` (`identBatchArgs`, `headFields`, `urlViewArgs`, `ghArgs`).
  `go test -race ./prenrich/...` → PASS, all of Step 1 green.
- [ ] **Step 4: counterfactual red evidence.** In a scratch copy
  (`mktemp -d` under the scratchpad, `git worktree`-free `cp -r picker`) restore
  the old `applyCache`/`enrichRepoGroup`/`collapseRollup` and run the Step 1
  tests; record the assertion failures for the PR.
- [ ] **Step 5: docs** (`enrichment.md`): head in identity query, mismatch rule,
  grace stamp, skipped rule, worst-case budget.
- [ ] **Step 6: gates**: `nix build .#lint --accept-flake-config`,
  `nix flake check --no-build`, `nix flake check`, `nix build .#default`.

## Consumer map (checks/identity cache edges)

| consumer | edge | disposition |
| --- | --- | --- |
| `applyCache` (pass, single-target, url) | reads identity head, stamp | changed, tests |
| `rollupFor` / `collapseRollup` | skipped rule | changed, tests |
| `armPendingMarker` fingerprint | identity head | changed, test |
| `fetchURLPR` (no checks head) | mismatch never fires (empty checks head); grace via `headRefOid` in view fields | compatible, test |
| `writeFetched` (`--force`, per-head) | `headRefOid` now in `prFields`; ident keeps it, checks cache gets it | changed, existing test updated |
| shell/picker readers of `/tmp/og-pr/*.json` | extra fields only | compatible (rg: none read identity JSON) |
| `@pr_check_state` readers (reflow, picker, bridge) | values unchanged set (`none|failure|pending|success`) | compatible |

## Acceptance

- PR body: causes, rules, budget (this file) → Step 6 write-up.
- Red/green tests → Steps 1, 3, 4.
- lint / flake check --no-build / flake check / build → Step 6.
- CI run URL on x86_64-linux and aarch64-darwin → after push.
