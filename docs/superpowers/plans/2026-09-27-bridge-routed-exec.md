# Plan: route %output while a window-set exec runs (#808)

Spec: `docs/superpowers/specs/2026-09-27-bridge-routed-exec-design.md`.

All Go commands run from `picker/` inside the devshell (`nix develop -c …`).
`$SP` below is this session's scratchpad:
`/tmp/claude-1000/-home-noams-Data-git--worktrees-git-tmux-og-feat-808-perf-bridge-daemon-reconcile-stalls-outp/6345d475-5abf-4012-8807-e2dd9e908b66/scratchpad`.

## File list

| File | Purpose |
| --- | --- |
| `picker/remotebridge/daemon/daemon.go` | `stream.awaitHigh` + one-slot `stream.parked`; `newRoundTrip` raises `awaitHigh`; `readReplyRouting` consults the slot; `Run` builds `flowCfg` and passes it to the window-set call sites; `runConn` clears the slot each pass. |
| `picker/remotebridge/daemon/conn.go` | `ctlConn.router` (the router the conn is bound to) and `ctlConn.routeWhile`. |
| `picker/remotebridge/daemon/routedexec.go` (new) | `routeWhile` itself and `Config.routing`. |
| `picker/remotebridge/daemon/paste.go` | `paster()` restores the plain hooks from a routing Config. |
| `picker/remotebridge/daemon/routedexec_test.go` (new) | Tests for the park slot, `routeWhile`, `Config.routing` and `paster`. |
| `tests/remote-m2-integration.bats` | Production-path regression. A daemon whose local `new-window` is slowed still paints live output of an existing mirror pane during the add. |
| other `picker/remotebridge/daemon/*_test.go` | Only if `-race` in Step 9 flags a test fake that now runs an exec on the helper goroutine: add a mutex to that fake. |
| `docs/agents/bridge-daemon.md` | New mirror-invariant bullet: window-set execs route output; the park rule; scope. |
| `docs/agents/performance.md` | Replace the "Daemon window reconcile" residual with the diagnosis, fix and A/B numbers. |
| `docs/superpowers/specs/2026-09-27-bridge-routed-exec-design.md`, this plan | Committed with the change (repo convention). |

## Steps

- [ ] **Step 0: remove the diagnostic tracing from the tree** —
  - `git apply -R --check $SP/trace808.patch`, then
    `git apply -R $SP/trace808.patch`;
  - `gtrash put picker/remotebridge/daemon/trace808.go`, which is untracked.
    A copy stays at `$SP/trace808.go`.

  Run: `git status --porcelain -- picker/` shows nothing, and
  `rg -n 808 picker/remotebridge/daemon` finds no trace hook. Expected: clean.

- [ ] **Step 1: failing tests for the park slot** — in `routedexec_test.go`:
  Every test builds slot state the way `routeWhile` will: stamp, **claim**
  the line with `claimSeq`, then park the claimed ordinal. Parking without
  claiming leaves `seen` behind, and every later claim is off by one.
  - `TestReadReplyRoutingReturnsParkedReply`: build `st := testStream()`,
    `seqs, _ := st.stampAll("a")`, `l := End{ClientCommandFlag, body "parked"}`,
    `seq := claimSeq(l, st)` (== `seqs[0]`), `st.park(seq, l)`.
    The reader holds only `%output %1 after` then EOF. Assert that
    `readReplyRouting(reader, router, &asyncQueue{}, st, seqs[0])` returns
    body `"parked"`. Assert that `%1`'s sink is still empty: nothing past the
    parked reply was read.
  - `TestReadReplyRoutingDropsStaleParkedReply`:
    - `seqs, _ := st.stampAll("a", "b")`: ordinals 1 and 3, barriers 2 and 4;
    - claim a's reply with `claimSeq` and park it at 1;
    - `readReplyRouting(..., want=seqs[1])` over a raw reader carrying
      barrier 2 (`%begin 1 2 1` / `og-fanout-1` / `%end 1 2 1`), then b's
      reply (body `B`) (`rawTestReader`, as in
      `TestReadReplyRoutingMatchesItsOwnCommand`).

    Assert it returns body `B` and that `st.parkedSeq() == 0`.

  Run: `go test ./remotebridge/daemon -run 'Parked' -count=1`. Expected: build
  failure (`st.park` undefined).

- [ ] **Step 2: implement the park slot** — in `daemon.go`:
  - add `awaitHigh uint64` and `parked *parkedReply`
    (`type parkedReply struct{ seq uint64; l controlmode.Line }`) to `stream`;
  - add these methods, all under `s.mu`:
    - `awaitUpTo(seq)`, which raises `awaitHigh` monotonically;
    - `awaited(seq) bool`, true when `seq != 0 && seq <= awaitHigh`;
    - `park(seq, l)`;
    - `takeParked() (parkedReply, bool)`, which returns and clears the slot;
    - `parkedSeq() uint64`, 0 when empty;
    - `dropParked()`;
  - in `newRoundTrip`, after a successful `stampAll`, call
    `st.awaitUpTo(seqs[len(seqs)-1])` when `len(seqs) > 0`;
  - in `readReplyRouting`, before the loop:
    `if p, ok := st.takeParked(); ok && p.seq == want { return p.l, true }`.
    Otherwise the slot is dropped: it held an earlier ordinal, which the old
    walk would have dropped.

  Doc comments carry the spec's why: the slot exists for `routeWhile`, and
  the reply order in the stream bounds it to ordinals `<= want`.
  Run: `go test ./remotebridge/daemon -run 'Parked|ReadReplyRouting|RoundTrip|WaitHellos' -count=1`.
  Expected: PASS.

- [ ] **Step 3: failing tests for routeWhile** — in `routedexec_test.go`.
  Each test drives `routeWhile(lines, router, async, st, fn)` with a buffered
  `chan controlmode.Line` and an `fn` that blocks until the test releases it:
  - `TestRouteWhileRoutesOutputDuringExec`: `fn` waits on a `signalSink`
    registered for `%0`. Its `got` channel fires when `%output %0 echo`
    (pre-loaded) is routed. Assert `routeWhile` returns within 2s. The old
    blocking exec never routes, so this deadlocks → times out.
  - `TestRouteWhileQueuesNotifications`: pre-load `%window-add @9`, then
    `%output %0 x`. `fn` waits for the `%0` sink. Afterwards
    `async.take()` holds exactly the `%window-add`.
  - `TestRouteWhileDropsUnawaitedReplies`:
    - `st.send("send-keys …")`, a fire-and-forget command at ordinal 1,
      barrier 2;
    - pre-load its reply `End`, the barrier's `End` (body `og-fanout-1`),
      then `%output %0 x`;
    - `fn` waits for the `%0` sink;
    - then `seqs, _ := st.stampAll("next")` and assert
      `st.claim(nil) == seqs[0]`. Ordinals stay exact.
  - `TestRouteWhileParksAwaitedReplyAndStops`:
    - `rt := newRoundTrip(pumpReaderOver(lines), router, async, st)`, where
      `pumpReaderOver` wraps the channel as a `lineReader` via a
      `*ctlPump{lines: lines}`;
    - `next := rt("capture-pane -p -t %1")`, which makes ordinal 1 awaited;
    - pre-load 1's reply `End` (body `seed`), then `%output %1 after`;
    - `fn` returns after a short `time.Sleep(50ms)`: routing must stop, not
      wait;
    - after `routeWhile`, assert the `%1` sink is empty and
      `st.parkedSeq() == 1`;
    - then `next()` returns body `seed`, and only a following
      `readReplyRouting` or aside read routes `after`. Check the latter by
      reading one line through `nextLine` and routing it.
  - `TestRouteWhileSkipsReadingWhileParked`: `rt("a")` (awaited 1). Claim
    and park its reply. Pre-load `%output %0 x`. `fn` returns after 50ms.
    Assert the `%0` sink is empty and `len(lines) == 1`.
  - `TestRouteWhileParksAtBarrierMidBatch`:
    - `next := rt("a", "b")`: ordinals 1 and 3, barriers 2 and 4,
      `awaitHigh` 3;
    - call `next()` once over a pre-loaded 1's reply, before `routeWhile`;
    - pre-load barrier 2's `End` (body `og-fanout-1`), 3's reply (body `B`),
      barrier 4;
    - `routeWhile` parks at barrier 2, which is `<= awaitHigh`;
    - the second `next()` still returns body `B`.
  - `TestRouteWhileEOFMidExec`: close `lines` before the call. `fn` returns
    `nil` after 20ms. `routeWhile` returns. A following
    `readReplyRouting(pumpReaderOver(lines), …)` returns `ok == false`.

  Run: `go test ./remotebridge/daemon -run 'RouteWhile' -count=1`. Expected:
  build failure (`routeWhile` undefined).

- [ ] **Step 4: implement routeWhile** (implement: escalated) — new
  `routedexec.go`:

  ```go
  func routeWhile(lines <-chan controlmode.Line, router *Router, async *asyncQueue, st *stream, fn func()) {
  	done := make(chan struct{})
  	go func() { defer close(done); fn() }()
  	if st.parkedSeq() != 0 {
  		lines = nil
  	}
  	for {
  		select {
  		case <-done:
  			return
  		case l, ok := <-lines:
  			if !ok {
  				lines = nil
  				continue
  			}
  			seq := claimSeq(l, st)
  			if st.awaited(seq) {
  				st.park(seq, l)
  				lines = nil
  				continue
  			}
  			handleAsideLine(l, router, async)
  		}
  	}
  }
  ```

  A nil channel disables its `select` arm, which is how "stop reading" is
  spelled. A closed channel stays closed for the next reader, so its EOF path
  is unchanged. The doc comment carries the spec's contract:
  - main-loop goroutine only;
  - `waitHellos` semantics;
  - park-and-stop, and why;
  - no reads while the slot is occupied.

  Run: `go test ./remotebridge/daemon -run 'RouteWhile|Parked' -race -count=1`.
  Expected: PASS.

- [ ] **Step 5: failing tests for Config.routing and paster** — in
  `routedexec_test.go`:
  - `TestRoutingWrapsEveryExecHook`: a Config with counting `LocalTmux`,
    `LocalTmuxOut`, `LocalArea`, `Reflow` and `LocalPanes`, and `run` that
    counts calls and invokes `fn`. Call each hook on `cfg.routing(run)` and
    assert:
    - `run` saw 5 calls;
    - each hook's own counter incremented;
    - return values pass through (`LocalTmuxOut` → `"out", nil`,
      `LocalArea` → `80, 24`, `LocalPanes` → map);
    - nil hooks stay nil (a Config with `Reflow == nil` yields
      `routing(run).Reflow == nil`).
  - `TestPasterUsesPlainHooks`: a routing Config whose `run` calls
    `t.Error`, with `PasteUpload` set. `h := cfg.paster()`. Call
    `h.procFor("%1")`, which reaches `bridgeProc` → `LocalTmuxOut`. Assert
    `run` was never called, and the plain `LocalTmuxOut` counter is 1.

  Run: `go test ./remotebridge/daemon -run 'Routing|PasterUsesPlain' -count=1`.
  Expected: build failure (`cfg.routing` undefined).

- [ ] **Step 6: implement Config.routing and paster restore** — in
  `routedexec.go`:
  - add the unexported field `plain *Config` to `Config` (defined in
    `daemon.go`, with a comment);
  - add `func (c Config) routing(run func(func())) Config`:
    - copy `c`, then set `r.plain = &c` (the original hooks);
    - wrap each non-nil hook of `LocalTmux`, `LocalTmuxOut`, `LocalArea`,
      `Reflow` and `LocalPanes` in a closure that captures the result inside
      `run(func(){ … })`.
  - In `paste.go` `paster()`, first line: `if c.plain != nil { c = *c.plain }`.

  Run: `go test ./remotebridge/daemon -run 'Routing|PasterUsesPlain|Paste' -race -count=1`.
  Expected: PASS.

- [ ] **Step 7: bind routeWhile to the connection** — in `conn.go`:
  - add `router *Router` to `ctlConn`. `newCtlConn` sets it to the same
    empty `NewRouter()` it hands `newRoundTrip`, and `bind` sets it to
    `router`;
  - add `func (c *ctlConn) routeWhile(fn func()) { routeWhile(c.pump.lines, c.router, c.async, c.st, fn) }`.

  Run: `go test ./remotebridge/daemon -count=1 -run 'Conn|Reattach|Replace'`.
  Expected: PASS.

- [ ] **Step 7b: failing production-path test** — in
  `tests/remote-m2-integration.bats` add
  `@test "window add keeps live output flowing while the local new-window is slow"`:
  - a `slowbin/tmux` wrapper under `$BATS_TEST_TMPDIR`: it sleeps 4s when
    its argv holds both `m2dst` and `new-window`, then
    `exec "$real" "$@"`, with `real="$(command -v tmux)"` baked in when
    written;
  - `$SRC new-session -d -s rem -x 100 -y 30` and the matching `$DST`
    session, then `PATH="$BATS_TEST_TMPDIR/slowbin:$PATH" bridge_up 1 slowadd`.
    bash exports a prefix assignment to a function's children, so the
    daemon resolves the wrapper;
  - `$SRC new-window -d -t rem`, `sleep 0.3`, then
    `$SRC send-keys -t rem:1 'echo LIVEADD_7K2' Enter`;
  - poll `$DST capture-pane -p -t host-sess:1` every 0.1s for up to 2.0s
    for `LIVEADD_7K2`;
  - then wait for 2 local windows (up to 8s), so the add still completes;
  - `chmod +x` the wrapper; `kill "$daemon_pid"` and `wait` at the end, as the
    other cases do;
  - assert both `[ "$painted" = yes ]` and `[ "$n" -eq 2 ]`.

  On the current code the marker lands only after the 4s exec, so the test
  is red.
  Run: `nix develop -c bats -f 'slow' tests/remote-m2-integration.bats`.
  Expected: FAIL (`painted=no`).

- [ ] **Step 8: wire flowCfg in Run** (implement: escalated) — in
  `daemon.go` `Run`:
  - build `flowCfg` right after `cfg.InputSeen = waker.poke`, the last
    per-Run stamp:

    ```go
    flowCfg := cfg.routing(func(fn func()) {
    	if c := hold.get(); c != nil {
    		c.routeWhile(fn)
    		return
    	}
    	fn()
    })
    ```

  - pass `flowCfg` instead of `cfg` at exactly these call sites:
    - startup `reconcileWindows`;
    - dispatch `WindowRenamed` (`applyMirrorName` and `.reflow()`);
    - dispatch `WindowAdd` (`addWindow`);
    - dispatch `WindowClose` (`closeWindow`);
    - `settle`'s `reconcileWindows`;
    - `repair`'s `reconcileWindows`.
  - At the top of `runConn`'s loop body, before `settle(c)`, add
    `c.st.dropParked()`, with a comment: no operation is in flight here, so
    a parked reply is abandoned.
  - Verify with `rg -n 'flowCfg' picker/remotebridge/daemon/daemon.go` that
    the code lines are exactly:
    - the declaration;
    - `reconcileWindows(flowCfg, …)` ×3 (startup, settle, repair);
    - `applyMirrorName(flowCfg, …)` and `flowCfg.reflow()` (WindowRenamed);
    - `addWindow(flowCfg, …)`;
    - `closeWindow(flowCfg, …)`.

    Comment lines naming it do not count.
  - Verify `rg -n 'go .*\bflowCfg\b|flowCfg\.paster'` finds nothing: no
    goroutine captures it.

  Run:
  - `go test ./remotebridge/daemon -race -count=1`. Expected: PASS;
  - `nix develop -c bats tests/remote-m2-integration.bats`. Expected: PASS,
    including Step 7b's test, now green.

  If `-race` flags a test fake that now runs on the helper goroutine, give
  that fake a mutex. That is in scope.

- [ ] **Step 9: fast deterministic gate** — from the repo root:
  - `nix develop -c bash -c 'cd picker && go vet ./remotebridge/... && go test -race ./remotebridge/...'`;
  - `nix build .#lint`.

  Expected: green. Fix and re-run until green.

- [ ] **Step 10: measurement** — build the after daemon, then run A/B churn,
  interleaved (A, B, A, B), at CPUQuota=400%:
  - announce start and end on the bus;
  - A = `main`: `git worktree add $SP/base origin/main` (detached). B =
    this worktree;
  - harness: `$SP/klat.sh`, the scratch copy of
    `tests/perf/keystroke-latency.sh` that also copies `daemon.log` to
    `$KEEP_LOG`. Make one copy per tree, with `REPO_ROOT=` rewritten to
    that tree;
  - both use `TMUX_BIN=$PWD/result/bin/tmux` from this worktree: the same
    tmux, so only the daemon differs;
  - each run:
    `TMUX_BIN=… timeout 300 systemd-run --user --scope -q -p CPUQuota=400% nix develop -c <klat copy> churn`;
  - record `uptime` before each run, and p50/p95/p99/max for the churn
    probe;
  - then apply `$SP/trace808.patch` and copy `$SP/trace808.go` into B only.
    The patch was cut against pre-change `daemon.go`; if it does not apply,
    re-add the same four hooks by hand: `mark808` in the pump, `routed808`
    in `Route`, `wrapTmux808` in `Run`, and phase timers in `runConn`.
  - Run one traced churn with `OG_TRACE808=1`, and list every `%0` outwait
    over 15 ms with the phase that covered it.
  - Remove the tracing again: `rg -n '808\(|trace808' picker/` must find nothing.

- [ ] **Step 11: docs** —
  - `docs/agents/bridge-daemon.md`, "Mirror invariants": add one bullet.
    It states that window-set operations run their local execs through
    `routeWhile`, which routes `%output` meanwhile with `waitHellos`'
    semantics. It covers the park-and-stop rule, the scope (window-set ops
    only; why pane-shaping paths stay blocking; why the flushes stay out),
    and goroutine confinement (`flowCfg` never reaches a goroutine;
    `paster()` restores plain hooks).
  - Also fix the `ctlPumpBuf` comment in `daemon.go`, which lists
    "LocalTmux execs" as buffered-only stretches: window-set execs now
    route.
  - `docs/agents/performance.md`:
    - replace the "Daemon window reconcile" residual bullet with a
      "Daemon window reconcile (#808)" section: the measured diagnosis, the
      fix, the A/B table (cap, host load, p50/p95/p99/max per run) and the
      traced outwait result;
    - add the new tests to "Regression guards".

  Run: `nix build .#lint` (typos, markdown hooks). Expected: green.

- [ ] **Step 12: full gate** — `nix build .#default`, `nix flake check` and
  `nix build .#lint`, all green.

## Acceptance

- [ ] Before/after churn p99 and max for the mirror probe, in the PR → Step
  10's A/B table (the after churn p99 is below the before's in every
  interleaved pair), plus the traced outwait list. No `%0` wait over 15 ms
  falls inside a window-set exec; any residual is attributed to its phase.
- [ ] Tests covering the ordering/concurrency change → Steps 1, 3 and 5,
  run under `-race` in Steps 4, 6, 8 and 9. Step 7b's bats case is the
  production-path red/green.
- [ ] `docs/agents/bridge-daemon.md` and `performance.md` updated → Step 11.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` pass →
  Step 12.

## Consumer map (park slot)

| Reader | Consults the slot | Why that is enough |
| --- | --- | --- |
| `readReplyRouting` | Yes: returns it on `want`, drops an earlier ordinal | The only reader that waits for a specific ordinal. |
| `routeWhile` | Yes: reads nothing while it is occupied | Keeps the stream position of the parked reply. |
| `waitHellos` | No | It already drops every reply. A slot is only occupied here if a batch spans a hello wait, which today would hang instead. No such call exists. |
| `runConn` select | No, but it clears the slot at the pass top | No round-trip is on the stack at the pass top, so a parked reply is abandoned. It is untested in isolation: `runConn` is a `Run` closure. `readReplyRouting`'s stale-drop (Step 1) is the tested backstop. |
