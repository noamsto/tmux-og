# Shard remote-m2-integration-tests (#1132)

Goal: replace the single 151-test `remote-m2-integration-tests` derivation (~8.5 min Linux, ~10 min macOS, the serial CI tail) with N balanced shard derivations plus an exactly-once guard. Re-partition only: no test, setup or teardown changes.

## Design (two options considered)

| | A. Split the .bats into several files + shared `load` helper | B. One file, per-shard generated copy where non-selected tests become dead functions |
|---|---|---|
| New test silently dropped? | Yes if it lands in a file no shard names | No: every `@test` not pinned lands in a computed shard |
| Churn | 8.5k-line file moved, helpers (defined between tests, e.g. `outage_start`) must be hoisted | None to the .bats file |
| Review | Huge diff | Small |

Choose **B**. In each shard derivation, `awk` rewrites the line `@test "name" {` of every test not assigned to the shard into `__unrun_<n>() {`: a valid bash function body that bats never registers, so helpers between tests, setup/teardown and the test bodies stay byte-identical. Assignment is computed in Nix from the test names read out of the source file (`builtins.readFile` + regex), so a new test needs no registration.

Balancing: `tests/remote-m2-weights.json` maps test name → measured milliseconds (x86_64-linux, from `bats --timing`). Nix runs greedy longest-processing-time assignment over those weights; a test missing from the table gets the median weight. Shard count N = 4 (`remoteM2ShardCount`).

## Steps

1. Baseline: build the existing check with `-L`, capture `bats --timing` for all 151 tests and the old wall time. Produce `tests/remote-m2-weights.json`.
2. flake.nix: factor `remoteM2Env` (nativeBuildInputs, DAEMON, RENDERER, CTL, STATUSLINE, DETACH, OG_OPEN, TMUX_OG_CONF, BATS_TEST_TIMEOUT) and `remoteM2Shard i` (one helper both the shards and the guard use). Names: `remote-m2-integration-tests-1..N`. Keep the 14m outer `timeout` and the relay_flake_diag output untouched (it lives in the .bats).
3. Guard `remote-m2-shard-coverage`: for each shard runs `bats --count` on its generated file; asserts the sum equals `grep -c '^@test'` of the source, and that the sorted union of the Nix-assigned names equals the sorted source names with no duplicates.
4. Prove the guard red: temporarily drop one test from the assignment, show failure, revert.
5. Docs/comments: update the flake.nix comment at `bridge-carousel-bind-assertions` and `docs/agents/` / CLAUDE.md mentions; the dated specs/plans are history and stay.
6. Measure each shard's wall time on x86_64-linux; PR body gets old vs new times and `## Removal candidates` (10-20 costliest, plus tests covered elsewhere with the covering test named; nothing deleted).
7. Gate: `nix build .#lint`, `nix flake check --no-build`, `nix flake check`, `nix build .#default`.

## Risks

- Shared `/tmp/og-m2-bats-$$` and tmux `-L` names are per-process/per-sandbox, so shards running concurrently do not collide (sandbox `/tmp` is private on Linux; macOS non-sandboxed builds share `/tmp` but `$$` differs and `-L m2src` lives under `$TMUX_TMPDIR`).
- The awk rewrite, the Nix name regex and the guard's grep must all use bats' own `^[[:blank:]]*@test` pattern, so an indented test cannot run in every shard.
