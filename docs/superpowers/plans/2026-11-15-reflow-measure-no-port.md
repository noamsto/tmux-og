# Measure tmux-reflow-windows; port decision (#1110)

Goal: measure wall time and forks per reflow at realistic and large window counts, then decide whether porting the layout logic to Go pays off. Acceptance: measurements in the PR; lint, flake check and `nix build .#default` pass.

## Measurements already taken (scratch server, 200x50, `--force`, wrapped tmux, 8-run mean)

| windows | cache-hit pass | full pass (`--force`) | full pass, per-window `set -w` exec stubbed out | tmux execs per full pass |
| --- | --- | --- | --- | --- |
| 5 | 7 ms | 53 ms | 38 ms | 10 |
| 20 | 7 ms | 177 ms | 109 ms | 31 |
| 60 | 7 ms | 411 ms | 214 ms | 71 |
| 120 | 7 ms | 853 ms | 392 ms | ~130 |

Findings: the full pass costs ~7 ms per window, about half bash layout compute (~3.5 ms/window) and half one `tmux` exec per window (the per-window `set -w … ';' …` loop at `scripts/tmux-reflow-windows.sh:595`). `docs/agents/performance.md` says "2 batched tmux calls, ~7 ms": true only for the cache-hit path.

## Decision

Do not port. Reasons: (1) the per-window tmux exec is about half the cost (more at large counts) and is a batching shape a Go port would not fix by itself; batching can be done in bash; (2) full passes are mostly backgrounded or debounced, though `session-window-changed`, `after-new-session` and `client-session-changed` run in the foreground; (3) a ~1,100-line port that must keep four bats suites unchanged is large for a gain cheaper work mostly captures. Compute dominates at realistic counts (~3.5 ms/window), so a port stays a valid later step for testability and speed.

## Steps

1. Update the `tmux-reflow-windows` row in `docs/agents/performance.md`: correct the call count (cache-hit vs full pass), add the measurement table, method, and the port decision with reasons.
2. Add the follow-up note: batch the per-window sets into one argv exec (est. halves full-pass cost); revisit a Go port only if compute remains dominant afterwards.
3. Gates: `nix build .#default`, `nix flake check`, `nix build .#lint --accept-flake-config`. No code or test changes.
4. PR body: the measurement table and method (scratch server with private `TMUX_TMPDIR`/`-L probe`, a `tmux` shim for exec counts via `strace -f -e trace=execve`).
