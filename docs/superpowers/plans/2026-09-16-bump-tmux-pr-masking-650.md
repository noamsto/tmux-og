# Fix silent PR-create failure in bump-tmux.yml (#650)

## Problem

`.github/workflows/bump-tmux.yml`'s "Commit and open PR" step ends in:

```
|| echo "PR already open for this branch; branch updated."
```

This swallows every `gh pr create` failure, not just "PR already exists" —
including `GitHub Actions is not permitted to create or approve pull
requests`. Read-only `gh api repos/noamsto/tmux-og/actions/permissions/workflow`
confirms `can_approve_pull_request_reviews: false` on this repo — the API field
backing the repo's "Allow GitHub Actions to create and approve pull requests"
setting (Settings → Actions → General → Workflow permissions). That is almost
certainly why the 2026-09-14 scheduled run pushed the branch but produced no
PR while reporting success.

## Root-cause decision

Per the task's two options, and since the read-only check settles it: enable
the repo setting (documented as a required human follow-up in the PR body),
not a PAT. Rationale: this is `noamsto/tmux-og`, a personal (non-org)
repository the user owns outright — flipping one checkbox in Settings →
Actions → General is strictly simpler and lower-risk than minting, storing,
and rotating a PAT secret for a once-a-week scheduled job. The PAT route is
noted as the alternative in the PR body, per the task's requirement, in case
the user prefers not to grant that repo-wide Actions permission.

## Scope check on sibling workflows

`grep -rn "|| echo" .github/workflows/` shows the pattern only in
`bump-tmux.yml` (twice: the changelog fallback at line 63, which is a
legitimate "log unavailable" fallback and NOT a failure-masking pattern to
touch, and the PR-create line at 104, which is the bug). `auto-assign.yml`,
`ci.yml`, and `dependabot-auto-merge.yml` have no such pattern. No sibling
fix is in scope.

## Steps

- [ ] **Step 1: Edit the "Commit and open PR" step in `.github/workflows/bump-tmux.yml`**
  - After `git push -f origin "$branch"`, add a check for an existing open PR
    on the branch: `gh pr list --head "$branch" --state open --json number
    --jq length` (or equivalent), and only run `gh pr create` when none exists.
  - Remove the trailing `|| echo "PR already open for this branch; branch
    updated."` catch-all from the `gh pr create` invocation entirely — a
    real failure (permissions refusal, network error, etc.) must fail the
    job now that "PR already exists" is handled explicitly above.
  - Keep everything else in the step (git config, commit, push, PR body
    construction) unchanged — this task is scoped to the masking bug and the
    stale-branch problem (step 2), not a rewrite of the step.

- [ ] **Step 2: Guarantee a fresh-main base, in the right place**
  - `actions/checkout@v7` with no `ref:` already checks out the default
    branch's tip at run time, so on an ordinary run the tree is already
    "main, right now" — the 15-commits-behind state observed in #650 is a
    *historical* artifact: a past run pushed the branch once, the PR was
    never created (the bug this task fixes), and `main` moved on while the
    orphaned branch sat still. Once PR creation stops silently failing, this
    self-heals on every subsequent run. Still, make it explicit and robust
    rather than relying on `checkout`'s default-ref behavior implicitly: add
    a new step **immediately after `actions/checkout@v7` and before "Get
    current and latest revs"** that does
    `git fetch origin main && git reset --hard origin/main`. This runs
    unconditionally (no `up_to_date` gate exists yet at that point), so the
    "current" pin is always grepped from the exact tree that gets committed
    and pushed later — current/latest comparison, the `sed` rewrite, and the
    final commit all operate on the same fresh-main tree, with no reordering
    of the later steps needed. Leave `git checkout -B "$branch"` in the
    "Commit and open PR" step exactly as it is today (line 82) — it now
    branches off a tree that was already reset onto `origin/main` at the top
    of the job, so it needs no change itself. Do not actually run or trigger
    the workflow to verify this — static review only, per the task's
    constraints.
  - **Guard the commit for a no-op case.** Even with the reset above,
    default-in-depth: `nix flake update tmux-upstream --accept-flake-config`
    could in principle leave `flake.lock` unchanged relative to what `sed`
    alone produced, or a concurrent run could race this one (see the
    concurrency note below) and land nothing to commit. Add, immediately
    before `git add flake.nix flake.lock`:
    ```
    if git diff --quiet -- flake.nix flake.lock; then
      echo "already at ${latest} on main; nothing to commit"
      exit 0
    fi
    ```
    This is a **success** exit (0), not a failure — "nothing changed" is not
    the bug #650 is about and must not turn the job red.
  - **Concurrency.** The workflow has no `concurrency:` group, so an
    overlapping `schedule` + manual `workflow_dispatch` can race: both read
    "no open PR" and both attempt `gh pr create`, and the loser now fails
    the job instead of being silently absorbed by the old `|| echo`. Add
    `concurrency: { group: bump-tmux, cancel-in-progress: false }` at the
    workflow level to close this instead of accepting it as a new failure
    mode.

- [ ] **Step 3: Trailing-line cleanup and `--assignee` caveat**
  - Removing the `|| echo "PR already open for this branch; branch
    updated."` line also leaves a dangling trailing `\` continuation on the
    prior line (`--assignee "@me" \`). Bash tolerates a continuation running
    into EOF, so this would not error, but clean it up so the diff isn't
    confusing — make the last line of the `gh pr create` invocation the
    final flag, no trailing backslash.
  - Note in the plan (and later in this worker's own PR body, not the
    workflow's generated PR body) that `--assignee "@me"` resolves the
    viewer through the token identity; under the default `GITHUB_TOKEN`
    (a bot/app-installation identity, not a real user) `@me` can itself
    fail. This is a pre-existing line, out of scope to change here, but call
    it out explicitly as a possible *second* reason the job could still fail
    red after the Settings toggle is flipped, so it isn't mistaken for a
    regression from this change.

- [ ] **Step 4: PR body note (this worker's PR, not the workflow's)**
  - No PR body template content needs to change inside the workflow itself
    for the setting decision — that requirement is about *this task's own
    PR* (the one this worker opens against `main`), not the workflow's
    generated PR body. Confirm in this worker's PR description: paste the
    `gh api repos/noamsto/tmux-og/actions/permissions/workflow` output
    verbatim (read-only, no secrets) rather than merely asserting the
    result, name the exact Settings path, note the PR is blocked on that
    human action, note the PAT alternative, and state plainly that until the
    human flips the setting, every Monday's scheduled run will now fail red
    by design (intended per #650 — no more silent, invisible failure) rather
    than let that come as a surprise.
  - Also confirm the diff going into this worker's PR is exactly
    `.github/workflows/bump-tmux.yml` plus this plan document under
    `docs/superpowers/plans/` (per this repo's CLAUDE.md, plan docs are
    committed alongside the code) — `WORKER_TASK.md` at the repo root is
    worker scaffolding, untracked, and must not be committed.

## Verification (static only — never run/trigger the workflow)

Corrected from the first draft: this repo's `nix build .#lint` pre-commit
hook set (`flake.nix` ~145-166) is statix/deadnix/alejandra (Nix),
shellcheck/shfmt (scoped to `scripts/*.sh` only — a `.yml` file's embedded
`run:` block is invisible to it), typos, check-merge-conflicts,
trim-trailing-whitespace. It does **not** lint YAML or the shell inside a
workflow's `run:` blocks, and there is no `actionlint` in the devShell — say
so plainly rather than claiming coverage that isn't there. Since this whole
change is about shell error-propagation (`set -e`, `||`, exit codes), verify
with checks that can actually fail:

- `nix run nixpkgs#actionlint -- .github/workflows/bump-tmux.yml` — confirm
  it resolves (it's not in the devShell) and paste its output.
- Extract the "Commit and open PR" step's `run:` body to a scratch file and
  run `bash -n <file>` (syntax) and `shellcheck -s bash <file>` (the tool
  that actually catches `||`/`set -e`/continuation mistakes) — paste both
  outputs.
- `nix build .#lint` anyway, for the Nix/typo/whitespace hooks it does
  cover — paste its output, but don't claim it validated the shell logic.
- A read-only `gh pr list --head chore/bump-tmux-upstream --state open
  --base main --json number,baseRefName` against the real repo, to confirm
  the new check's actual output shape before relying on it in the step
  (non-mutating; allowed under the task's constraints).
- Re-read the finished step and state explicitly in the PR body which
  failure modes now fail the job (a genuine `gh pr create` error, e.g. the
  permissions refusal) and which are still tolerated (an existing open PR
  targeting `main`, detected explicitly and skipped before the create call
  — not swallowed after the fact; and the new no-op "nothing to commit"
  case from Step 2, which exits 0 rather than failing).

## Out of scope

- Do not push to `chore/bump-tmux-upstream`.
- Do not run, dispatch, or trigger `bump-tmux.yml`.
- Do not merge to `main`.
- Do not touch `auto-assign.yml`, `ci.yml`, or `dependabot-auto-merge.yml`
  (no matching masking pattern found).
- Do not create the PAT secret or flip the repo setting from this worktree —
  both are human follow-ups called out in the PR body.
