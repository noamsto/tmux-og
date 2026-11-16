# CI: push every locally built derivation to lazytmux from main (#1137)

## Problem

The build job pushes only `packages.<sys>.{default,go-tools,tmux-remux}` closures from
main. Check-side derivations are never cached, so every PR run rebuilds them even when
their inputs are unchanged. The worst one is `pickerChecked` (`checks.<sys>.picker-go-tests`,
`flake.nix:116`): the picker build plus `go test -race ./...`, which every remote-bridge
integration check needs for its binaries. On darwin it finished 11.5 min into a 24 min run
for a test-only PR (CI run 43829973680).

## Decisions

1. **Which paths are pushed.** Every derivation output this run *built* on this system:
   the build closure (`nix-store --query --requisites --include-outputs`) of every
   attribute nix-fast-build evaluated, filtered to paths that are valid locally,
   are derivation outputs (`deriver` set, so eval-time source copies such as
   `${./tests}` are excluded) and are `ultimate` (built here, not substituted:
   `nix path-info --json` marks only locally built paths `ultimate`; verified locally,
   a built path reports `ultimate: true, signatures: []`, a substituted one
   `ultimate: false` with the cache's signature). This covers, without a hand-kept list:
   - `pickerChecked` (both as the `picker-go-tests` check output and as a build input of the remote-bridge checks);
   - the other check outputs (`touch $out` stamps, tiny);
   - intermediates the package runtime closures leave out: the generator build, the
     plain `picker` build used by `enrich-budget-tests`, the overridden `nilaway`
     (built from source for `goLintGate`), Go vendor (`*-go-modules`) fixed-output derivations,
     `mkTmux`/wrapped tmux when a check rebuilt them.
   The existing push of the three package closures stays as is: their runtime closures
   include substituted paths (aeye, prdash) that lazytmux serves to installers, and
   `ultimate` would drop those.
   Rejected alternative: nix-fast-build's own `--cachix-cache … --push-build-closure`.
   It needs the token inside the build step and an upload failure there fails the build,
   breaking both the main-only/continue-on-error constraints. A hand-maintained list of
   attributes (exposing `pickerChecked`, generator, … as packages) rots as checks are added.
2. **Push check outputs too: yes.** `pickerChecked` *is* a check (`picker-go-tests`), so it
   cannot be cached without caching a check output. The others are a few bytes each, and
   `--skip-cached` then skips every check whose inputs a PR does not touch (Go, generator,
   lint gates on a test-only change). Cost: a flaky test that passed once on main is not re-run
   by a PR with identical inputs. That is the same contract a local `nix flake check` already
   applies to the local store. Any change to its inputs (its source, its test file, nixpkgs)
   re-runs it, and `nix build --rebuild` re-runs it by hand. Accepted; stated in the ci.yml comment.
3. **Only after a green build, main only, token only there.** The push step keeps
   `if: github.event_name == 'push' && github.ref == 'refs/heads/main'` and the default
   `success()` gate, `continue-on-error: true`, and is the only step that gets `CACHIX_AUTH_TOKEN`.
   Both cachix-action steps stay read-only (no authToken).
4. **Input list source.** nix-fast-build writes `--result-file` (JSON
   `{"results":[{type, attr, success, drvPath, cacheStatus, …}]}`). The successful `EVAL`
   results carry every attribute's `drvPath`, including the ones skipped as cached;
   their outputs are not valid on the runner, so the validity filter drops them and nothing
   is re-downloaded.

## Files

- `.github/cache-built-paths.sh` (new): reads the nix-fast-build result file and prints the
  store paths to push (decision 1), one per line. A script, not inline YAML, so the local
  simulation runs the exact selection CI runs, and shellcheck/shfmt lint it.
- `.github/workflows/ci.yml`: add `--result-file "$RUNNER_TEMP/nix-fast-build.json"` to the
  nix-fast-build step; rename the push step, and feed `{ package closures; cache-built-paths.sh; }`
  into one `cachix push lazytmux`; rewrite the comments (the `--skip-cached` comment and
  "Check results and lint closures stay out of the cache").
- `docs/superpowers/plans/2026-11-16-ci-cache-built-derivations.md`: this plan.

No doc under `docs/agents/` describes what CI caches (`rg -i 'cachix|skip-cached' docs/agents` is empty).
The README "Binary cache" section is about consuming the cache, and stays accurate.

## Steps

- [ ] **Step 1: write `.github/cache-built-paths.sh`**. bash, `set -euo pipefail`, one
  argument (the result file). Pipeline:
  `jq -r '.results[] | select(.type == "EVAL" and .success and .drvPath != null) | .drvPath'`
  → collect into a bash array without `mapfile` (macOS runner bash may be 3.2): use a
  `while IFS= read -r` loop; empty array → exit 0 with no output;
  → `nix-store --query --requisites --include-outputs "${drvs[@]}"` → `grep -v '\.drv$'`
  → only valid paths (`nix-store --check-validity --print-invalid` is not needed if
  `--include-outputs` only emits valid outputs: Step 2 verifies this; otherwise filter with
  `nix path-info` on each batch, skipping invalid ones)
  → `nix path-info --json --json-format 1 <paths>` (CI installs the latest Determinate Nix, which is at least the local 3.24.2 that has this flag) → `jq -r 'to_entries[] | select(.value.ultimate and .value.deriver != null) | .key'`.
  Pass paths as arguments in chunks of 500 via a loop, not `xargs` (BSD/GNU `-r` differences).
  Proof: `shellcheck .github/cache-built-paths.sh` clean; `shfmt -d` clean.
- [ ] **Step 2: verify the script against a real result file.** Using the result file
  from a local main build (`nix-fast-build … --result-file main-result.json`, the CI `--select`
  for x86_64-linux), run `bash .github/cache-built-paths.sh main-result.json > push.txt`.
  Expected: non-empty; contains the `picker-go-tests` output path
  (`nix eval --raw .#checks.x86_64-linux.picker-go-tests.outPath`), contains a `*-go-modules`
  path and the nilaway override; contains no `.drv` and no `-source` eval copy of `./tests`;
  `nix path-info --json --json-format 1 $(cat push.txt) | jq '[.[] | select(.ultimate|not)] | length'` is 0.
  Also confirm `nix-store -qR --include-outputs` emits no invalid path (`nix-store --check-validity` over the list exits 0).
- [ ] **Step 3: edit `.github/workflows/ci.yml`**. nix-fast-build step: add
  `--result-file "$RUNNER_TEMP/nix-fast-build.json"` (the `run: >-` folded scalar runs under
  bash on both runners, so `$RUNNER_TEMP` expands). Push step: name
  "Push built derivations to lazytmux", same `if`, `continue-on-error`, `env`, `shell: bash`;
  `run: |` body
  `{ nix build <3 packages> --no-link --print-out-paths; bash .github/cache-built-paths.sh "$RUNNER_TEMP/nix-fast-build.json"; } | cachix push lazytmux`.
  Keep the pipefail comment (GitHub's `shell: bash` is `bash -eo pipefail`, so either
  producer failing fails the step; continue-on-error keeps the job green). Rewrite the
  `--skip-cached` comment ("Only packages from main are pushed") and the push comment to state
  decisions 1–3 briefly, including the flaky-test trade-off. Proof:
  `nix run --inputs-from . nixpkgs#actionlint -- .github/workflows/ci.yml` clean (if actionlint
  is unavailable, `yq` parse of the file succeeds), and `nix build .#lint --accept-flake-config`.
- [ ] **Step 4: local simulation (acceptance 1).** All under `$S` (scratchpad).
  - Signing: a file cache filled from the dev store holds unsigned, locally built paths that
    a `require-sigs` store refuses. `nix key generate-secret --key-name og-sim-1 > $S/sim.sec`,
    `nix key convert-secret-to-public < $S/sim.sec > $S/sim.pub`; `nix copy` signs as it
    uploads via the store URL's `secret-key=$S/sim.sec` parameter (the daemon store is left alone).
  - Two caches standing for lazytmux after a main push: `before-cache` = the 3 package
    closures only (today's push); `after-cache` = those closures plus `push.txt` (this change).
    `nix copy --to "file://$S/<cache>?compression=zstd&secret-key=$S/sim.sec" <paths>`.
  - Store isolation: nix-fast-build's `--store` still evaluates and checks cache status
    against the daemon store, where every output is already `local`. So point every nix
    child process at a fresh chroot store through the environment instead:
    a NIX_CONFIG with real newlines (bash heredoc into a variable), holding
    `store = $S/simstore-<n>`, `substituters = https://cache.nixos.org file://$S/<cache>`,
    `trusted-public-keys = cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY= <sim.pub>`,
    `extra-sandbox-paths =` (the host's binfmt sandbox path need not exist in the chroot store)
    (lazytmux is left out of both runs, so its current contents do not blur the comparison;
    a fresh simstore per run).
  - Test-only commit: a scratch commit touching one comment in a `tests/*.bats` file
    (never pushed; reset after).
  - Run the CI's exact command line, plus `--result-file`, under each environment:
    `nix run --inputs-from . nixpkgs#nix-fast-build -- --no-nom --skip-cached --eval-workers 4 --flake .# --select '<CI select for x86_64-linux>' --result-file $S/<run>.json`
    (resolve the nix-fast-build path from the dev store first and invoke the binary directly, so `nix run` itself does not go through the chroot store).
  - Count from the fresh simstore, not the log (nix-fast-build never prints per-drv
    `building` lines): derivations built =
    `nix path-info --store $S/simstore-<n> --all --json --json-format 1 | jq '[to_entries[] | select(.value.ultimate and .value.deriver != null)] | length'`.
    Also run `.github/cache-built-paths.sh $S/<run>.json` under the before run's NIX_CONFIG, which
    tests the script against a run that genuinely built things.
    pickerChecked: `nix path-info --store $S/simstore-<n> --json --json-format 1 <picker-go-tests outPath> | jq '.[].ultimate'`:
    true (built) in the before run, false (substituted) or absent (skipped as cached) in the after run;
    the after run's result file shows `picker-go-tests` with `cacheStatus: "cached"` and no BUILD entry.
  - If the chroot store cannot build on this machine (sandbox/userns failure), fall back to
    `nix build --dry-run` counts in the same environments and say so in the PR body.
  Reset the scratch commit afterwards (`git reset --hard HEAD~1`, only the scratch commit).
- [ ] **Step 5: gates.** `nix build .#default`, `nix flake check --no-build`, `nix flake check`,
  `nix build .#lint --accept-flake-config`; all exit 0.

## Acceptance

- [ ] Local simulation with before/after built counts → Step 4 output, quoted in the PR body.
- [ ] PR body lists exactly what is pushed and why, and that PR jobs never get the token → decisions 1–3, copied into the PR body.
- [ ] `nix build .#lint --accept-flake-config` → Step 5.
- [ ] `nix flake check --no-build` → Step 5.
- [ ] `nix flake check` → Step 5.
- [ ] `nix build .#default` → Step 5.
- [ ] CI lint + build jobs green on x86_64-linux and aarch64-darwin → the PR's checks (`gh pr checks`). The push step itself only runs after merge to main; its first main run is checked via the step log (`cachix push` path count), a follow-up observation, not a PR gate.
