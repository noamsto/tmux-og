# remote-m2: replace fixed sleeps with polls or proven windows (#1134)

Speed-only change to `tests/remote-m2-integration.bats`. No test is deleted, skipped or weakened.

## Design choice for negative waits (two approaches)

- **A. Timing override** (test-only knob shrinking `activityRecency`, reveal tick, reply deadline). Cheap to write, but it changes the window under test, needs a Go change and a doc entry, and must never go below the window it guards.
- **B. Observable proving the window has passed.** Wait on a signal the daemon cannot produce before the guarded window ends: a later record processed strictly after the one under test (reports are ordered per daemon), a pane marker proving a script finished, or a stamp advancing past the guarded timestamp.

**Pick B wherever an observable exists** (#1034 negative waits, #979 idle precondition): daemon code untouched, assertion exactly as strong. A only for a window with no observable. `--reply-timeout` / `--seed-timeout` flags already exist (`Config.ReplyTimeout`); no new knob is planned. If one proves necessary it is a test-only flag named here first, read once at startup, unset = today's constant, documented in `docs/agents/bridge-daemon.md`.

## File list

- `tests/remote-m2-integration.bats` — replace/shorten fixed sleeps; add shared helper `poll_until <deadline_s> <cmd...>` and `og1034_sync_*`.
- `tests/remote-m2-weights.json` — re-balance shard weights (#1133 mechanism).
- `docs/superpowers/plans/2026-11-16-remote-m2-fixed-sleeps.md` — this plan.

## Steps

- [ ] **Step 1: baseline.** Run `nix build .#checks.x86_64-linux.remote-m2-integration-tests-{1..4}` capturing `bats --timing` from `nix log`; record per-shard wall and per-test times of the five named tests to `<artifacts>/baseline.txt`.
- [ ] **Step 2: classify** every sleep >= 0.5s and every loop sleep setting a test floor (`rg -n 'sleep ' tests/remote-m2-integration.bats`): positive / negative / injected-stall / fixture. Injected stalls inside tmux stubs (1484, 1545, 1693, 1730) are the condition under test: keep, shorten only with >= 3x sampler margin, else record why. Proves: table complete, each row has a disposition.
- [ ] **Step 3: #979 `sleep 5` (964).** The guarded stamp is the local `obs-s` client on `$DST` (`watchReveal` queries `LocalTmuxOut(clientViewsArgs(host-sess))`; `activityMark.advance` counts `focused && stamp > high && stamp >= now-3`), not S's control client. Replace with a poll on `$DST list-clients -t host-sess -F '#{client_control_mode}|#{client_activity}'` (non-control rows only, S's row) until `date +%s - stamp >= 6` (expected saving ~0.5-1s; the tighter alternative, proof that one reveal tick ran after the last stamp, is optional) (recency 3 + reveal tick 1 + truncation 1, +1 margin; truncation means >=5 guarantees only ~4.01s). `hold_src_all` negative windows unchanged.
- [ ] **Step 4a: #1034 tests (4360, 4368, 4390 in the routing test; 4419 in "og-open still forwards after the registrant leaves").** Per site: wait for the pane's og-open to finish (`; echo OGDONE_n` added deliberately in the shared `og1034_open` helper for every caller, poll pane), then append a unique untagged sentinel record to `@og_open_url` (nonce `^[0-9]+-[0-9]+$`, unique per site, never `1-1` which 4387 uses; URL `https://example.invalid/sync-N`) and `og1034_wait_opened` it on the daemons that are alive (s and b; at 4419 B is killed, so s only), then `og1034_not_opened`. At 4390 (`og1034_not_opened r old` after a direct `set-option`) r can only record via og-open's unbridged fallback, so that guard is trivially true: drop the sleep without a sentinel and say so in the table. Ordering holds because `daemon.go:1588-1592` calls `opener.handle` inline in the control-read loop; the open itself is launched in a goroutine (`openurl.go` launch), so a false pass window of milliseconds exists but a false failure cannot: say so in the PR table.
- [ ] **Step 4b: single-daemon test "og-open in a remote pane ... never again after a reattach" (4476, 4516, 4526).** It asserts by line position/count on `$opened` (`bulk=sed -n 2,4p`, `lines_after_reattach -eq 4`, `final_lines -eq 5`), so sentinels must not enter those reads. Use sentinel URL `https://example.invalid/sync-N` with a unique nonce, wait for it in `$opened`, and read every count/`sed`/`tail` through `grep -v /sync-` of `$opened`. Guarded behaviours: 4476 no duplicate open of `x`; 4516 no replay after the reattach seed/subscribe; 4526 no duplicate `after`. The sentinel is delivered as in 4a (`$SRC set-option -a -t rem @og_open_url " <nonce>|https://example.invalid/sync-N"`). The filter also covers the three `wc -l` poll loops (4472, 4482, 4522), not only the captured values. Assertion lines unchanged.
- [ ] **Step 5a: diagnose the 32.8s test.** From the Step 1 baseline log, read how many retry-loop iterations (`i`) the wedge needed in 7558 and 7617 and where the time goes (iterations x 1s beat vs 2s deadline vs recovery); record in the plan notes.
- [ ] **Step 5b: reply-deadline tests (7558, 7617).** 7558 is a convergent retry loop (settle beat, then `split-window`, then check log): shorten the beat from 1s to 0.3s, correctness-neutral since the loop retries until the log shows the deadline. Keep `--reply-timeout 2s`. 7617: the first beat becomes 0.3s. The second beat must keep its meaning: right after `split-window` the remote has n+1 panes and the mirror n until a live daemon mirrors the split, so `src_n != dst_n` on the first sample proves nothing. Invert the poll: each iteration polls every 0.1s for up to the old 1s until `src_n == dst_n`; equality means the daemon is live, so start the next iteration early (the saving). Set `parked=yes` only if the inequality persists when the cap runs out, never on the first unequal sample; `rename-session` ordering unchanged. Step 5a confirms from the baseline log that the wedge converges within the iteration budget (repeated `split-window -d` on a 100x30 pane starts failing after ~4-5 halvings, errors swallowed by `|| true`); if the iteration count at the 0.3s beat could exceed that, reset panes between iterations or keep the 1s beat for 7558. 7580 belongs here only (not Step 6).
- [ ] **Step 6: remaining waits** (456, 1225, 3642, 5341, 6869, 8277, 8282): convert positives to bounded `poll_until` using existing helpers. 5341 (`sleep 2`, pane pause before the drop): use a daemon log/pause observable if one exists, else keep and record why. 1566 stays a bounded negative sample before the 3s stall. For stub stalls 1484/1545/1693/1730, name the sampler interval each is measured against in the table (keep unless >= 3x margin).
- [ ] **Step 7: re-time and re-balance** `tests/remote-m2-weights.json` if shard spread moves materially.
- [ ] **Step 8: sensitivity.** Edit the daemon in place (still a git tree for the flake), break the daemon condition for #979 (let idle controller reclaim) and #1034 (drop tag routing); run the shard check, record the red assertion, `git checkout` the file back.
- [ ] **Step 9: stability and gates.** `nix build --rebuild` each shard 3x; `nix build .#lint --accept-flake-config`; `nix flake check --no-build`; `nix flake check`; `nix build .#default`.

## Acceptance

- [ ] PR sleep table — Steps 2–6 (4a/4b/5b).
- [ ] Before/after shard and five-test times — Steps 1, 7.
- [ ] 3 consecutive passes per shard — Step 9.
- [ ] #979/#1034 red-when-broken — Step 8.
- [ ] lint, flake check --no-build, flake check, build .#default — Step 9.
- [ ] CI build job both platforms — `gh pr checks` after push.
