# Measure tmux-update-icons, port only if it pays off (#1112)

Goal: put numbers on `scripts/tmux-update-icons.sh` (972 lines, the `#()` job in
`status-format[0]` once a second per status-drawing client, and the 5 s
`@og-sweep-tick` hook), prototype its hot inner part in Go, and decide from the
numbers. Follows the format of "Reflow full-pass cost (#1110)" in
`docs/agents/performance.md`.

## Decision rule (fixed before any Go number is taken)

Port iff the Go prototype clears **any** of:

- **T1, latency.** Keystroke-latency p95 with the icons job running improves by
  more than the harness's own run-to-run spread (measured first, from
  repeated shell-only runs).
- **T2, CPU.** Total CPU per tick (client process + tmux server thread) drops
  by more than 30 % at the realistic size (20 windows, ~33 panes).

and the port is feasible under the issue's acceptance: `tests/icons.bats` and
every `tests/update-icons-*.bats` pass **unchanged**. Those suites `sed`-
substitute `scripts/tmux-update-icons.sh` and run it, so they pin the shell
body; a port that keeps the shell body as the test seam is a second
implementation, not a port. If T1/T2 pass but this does not, record both facts
and do not port.

## File list

- `tests/perf/update-icons-cost.sh` (new): reproducible harness: scratch server
  (`-L`, private `TMUX_TMPDIR`/`CLAUDE_STATUS_DIR`/`XDG_DATA_HOME`), N windows x P
  panes with a share of agent windows, execs per tick (strace), wall, client
  CPU, server CPU, for the per-tick and sweep drivers.
- `tests/perf/keystroke-latency.sh` (modify): two env knobs, `LOCAL_WINS=N`
  (N plain windows on the local server) and `NO_ICONS=1` (disable the icons job
  and sweep hook), so the icons job's contribution is an A/B on one build.
- `docs/agents/performance.md` (modify): new section with the before/after
  tables, the threshold, and the decision.
- `docs/superpowers/plans/2026-11-15-update-icons-cost-measurement.md`: this
  plan.
- Go prototype: scratch only, not committed (it is measurement apparatus; a
  committed unused binary would be dead code if the answer is no).
  If the answer is "port", the port itself becomes the file list and this plan
  is re-entered through the replan path.

## Steps

- [ ] **Step 1: harness** - add `tests/perf/update-icons-cost.sh`. Prove:
  `nix develop -c shellcheck tests/perf/update-icons-cost.sh` clean and
  `tests/perf/update-icons-cost.sh 5 2 2` prints execs/wall/cpu rows.
- [ ] **Step 2: shell baseline** - run it for 5/20/60/120 windows (2 panes,
  20 % agent windows), per-tick and sweep drivers, 3 repetitions each; record
  mean and spread. Expect: 5 execs per tick (2 tmux, 1 git, 1 timeout, the
  script), wall growing with N.
- [ ] **Step 3: keystroke harness knobs** - add `LOCAL_WINS`/`NO_ICONS` to
  `tests/perf/keystroke-latency.sh`. Prove: shellcheck clean; `busy` scenario
  runs with each knob.
- [ ] **Step 4: latency A/B and noise floor** - `busy` scenario, `SAMPLES=400`,
  icons job on vs `NO_ICONS=1`, with `LOCAL_WINS` 0/20/60, repeated runs; the
  repeat spread is the T1 noise floor.
- [ ] **Step 5: Go prototype** - scratch binary: same two tmux reads (same
  `list-panes -a` format string), parse, per-window icon/display/padded
  compute from the same `ICON_MAP`, claude state from the pane files, the
  change gate, one `tmux source -` only on change, the same single `git` poll.
  Prove: its emitted `set` commands equal the shell's on the same scratch
  server (diff of `source -` input captured by a tmux shim), then measure
  with the Step 1 method (same sizes, same repetitions).
- [ ] **Step 6: decide** - apply the rule above to the Step 2/4/5 numbers.
- [ ] **Step 7: write-up** - `docs/agents/performance.md` section with the
  tables, the threshold, and the decision; the PR body repeats the table.
  Prove: `nix build .#lint --accept-flake-config`, `nix flake check`,
  `nix build .#default`.

## Acceptance

- [ ] Before/after table in the PR and in `docs/agents/performance.md`: Step 2,
  4, 5 output.
- [ ] If ported: `tests/icons.bats` and `tests/update-icons-*.bats` unchanged and
  green (`nix flake check`). If not ported: no change under `scripts/` or
  `picker/`, shown by `git diff --stat`.
- [ ] `nix build .#lint --accept-flake-config` passes.
- [ ] `nix flake check` passes.
- [ ] `nix build .#default` passes.
