# Plan: machine-relative bound for TestExtractUnspacedBlobBounded (#1141)

Measured (32-core host, unloaded): n=2000 rows x width 200: 0.09s no-race, 1.39s race.
Scaling n=500..8000 is linear in both modes (race: 0.37/0.68/1.35/2.74/5.45s) -> no superlinear bug; 13.7s on darwin CI is slowness, not algorithmic.

Steps
1. picker/linkjoin/extract_test.go: replace the wall-clock `blobBudget` with a ratio test. Time Extract on n and 4n rows (best of 3 each, to damp scheduler noise); assert t(4n)/t(n) <= 10 (linear ~4, quadratic ~16). Keep the links-exist and `maxEdgeToken+width` assertions on the 4n run. Remove `blobBudget` var.
2. Delete picker/linkjoin/race_test.go (only held the budget widening).
3. Prove: temporarily make rejoin quadratic in extract.go (or stand-in), show test fails, revert.
4. Stress: `go test -race -count=50 -run TestExtractUnspacedBlobBounded ./linkjoin/` under stress-ng/taskset load >= cores.
5. Gates: nix build .#lint, nix flake check --no-build, nix flake check, nix build .#default.
Files: only the two above. Sibling wall-clock tests (remotebridge/graphics/proxy_test.go:332 `d > time.Second`, tui_attach_test.go:100) don't share the helper -> follow-ups.

## Revision 1 (critic)
- n=150 vs 4n=600 (reviewer: cheaper, stays above the 8192-byte cap row count) (width 200); trials interleaved n,4n x3, min of each; Fatalf prints both durations and ratio.
- Step 0: record loaded timings (stress-ng --cpu nproc / taskset -c 0-2), race and no-race, for the PR body.
- Step 3: inject the original bug shape (rescan the growing joined line per join), save failing output as evidence.
- Final: PR body with timings, diagnosis (linear), assertion, injected-quadratic output, load method; cite CI run URLs.
- Follow-ups: kill_test.go:39, attach_test.go:214, tui_attach_test.go:100, remotebridge/graphics/proxy_test.go:332.
