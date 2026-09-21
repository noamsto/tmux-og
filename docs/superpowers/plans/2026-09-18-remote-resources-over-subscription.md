# Plan — ship remote CPU/mem over the subscription channel (#693)

Spec: `SPEC.md` (r3). Tier: deep. No orchestration consult: the change is wide
(Go picker, Go daemon, Go generator, Nix config + checks) but shallow — every
seam is already named by the spec and by `CLAUDE.md`, and the dependency order
is linear.

## Design in one paragraph

A new Go binary `tmux-session-resources`, armed by a fifth `-B` monitor hook
`@og-res-tick`, runs on **every** tmux-og host. While a control-mode client is
attached it walks the process tree once per pass and stamps each session's own
`@og_session_res` with `"<cpu> <mem> <cores> <tick>"`. The bridge daemon
subscribes to that session option on a session-scoped subscription
(`og_res::#{@og_session_res}`), deduplicates on the first three fields, and
writes the local mirror session's `@bridge_res` as `"<cpu> <mem> <cores>
<local-receive-epoch>"`. The picker reads `@bridge_res` out of the
`list-panes -a` snapshot it already takes, uses it when it is fresh, and ssh-
probes only the hosts that still have an uncovered mirror session.

## Shared contracts this change touches

Named up front because each is enforced by a check that fails loudly:

- **`config/tmux.conf.reference.nix` and the generator must stay byte-identical**
  (`tmux-conf-extraction-assertions`). Every tick-hook edit lands in
  `generator/render/status.go` **and** `config/tmux.conf.reference.nix`.
- **`tick-floor-conf-assertions`** pins each clear and setter literal, the
  clears-before-setters ordering, and the exact hook-command count (`n -eq 4`
  today). **`tick-floor-disabled-conf-assertions`** pins that every clear
  survives a disabled feature and that unconditional setters are still emitted.
- **The `-B` monitor traps** (`CLAUDE.md`): empty target `@name::<format>`, the
  guard body as a *string* not a brace block, and a `-u -B` clear paired with a
  `set -gu`, emitted before every setter.
- **`collectPanesSnapshot`'s format is pipe-delimited and has TWO parsers that
  each fail closed** on a wrong field count: `sessions()` (`picker/main.go:181`)
  and `paneMap()` (`picker/main.go:904`). Adding a field means bumping **both**
  in the same commit — `paneMap` is what maps a pane id to its session/window
  for the agent counts, so a missed bump empties every mirror's agent state
  silently. The new value must never contain `|`.
- **`pickerChecked.checkPhase` (`flake.nix:127-135`) is an explicit package
  list, not `go test ./...`.** A new package's tests do not run in
  `nix flake check` unless they are added there.
- **`aggregateResources` is "the only place the tree walk lives"** — the poller
  must reuse it, not copy it.

## Steps

### S1 — extract the process-tree walk into a shared package
`implement: sonnet`

New `picker/proctree/proctree.go`, `package proctree`:

- `var PSArgs = []string{"-Ao", "pid,ppid,pcpu,rss"}` — moved verbatim with its
  comment (`-A` not `-e`, no `--no-headers`).
- `type Totals struct{ CPUPct, MemMB float64 }`.
- `func Aggregate(rootPIDs map[string][]int, psOut string) map[string]Totals` —
  the body of today's `aggregateResources`, moved verbatim.

In `picker/main.go`:

- `psArgs` becomes `var psArgs = proctree.PSArgs`.
- `aggregateResources` becomes a thin wrapper converting `proctree.Totals` into
  the existing unexported `sessionResources{cpuPct, memMB}`. The wrapper exists
  because `sessionResources`' fields are unexported and every call site and test
  in `package main` reads them; a type alias is impossible.
- Delete the moved comment body from `main.go`; leave a one-line pointer.

Move `TestAggregateResources` / `TestAggregateResourcesEmpty` out of
`picker/remote_resources_test.go` into `picker/proctree/proctree_test.go`,
rewritten against the exported API.

**Acceptance:** `cd picker && go test ./...` green; `go vet ./...` clean; no
behavioural change anywhere.

### S2 — the remote poller binary
`implement: opus`

New `picker/sessionres/main.go`, `package main`, built as
`tmux-session-resources`. One pass per invocation, no daemon, no state on disk.

Pure, table-tested helpers (each its own function so the test needs no tmux):

- `parseControlClients(out string) bool` — true iff any line of
  `tmux list-clients -F '#{client_control_mode}'` is `1`. A tmux error or empty
  output reads as **false**: no bridge is watching, so there is nothing to
  publish.
- `parsePanePIDs(out string) map[string][]int` — from
  `tmux list-panes -a -F '#{session_name}|#{pane_pid}'`. Splits at the **last**
  `|` (a session name may contain one), skips non-positive PIDs.
- `stampValue(t proctree.Totals, cores int, now int64) string` —
  `fmt.Sprintf("%.1f %.0f %d %d", t.CPUPct, t.MemMB, cores, now)`.
  Quantisation, not rendering: `formatCPU`/`formatMem` stay the sole owners of
  display precision (spec R1a). `%.1f` on CPU keeps `<1%` expressible; `%.0f` on
  MB is finer than `formatMem`'s worst case. Contains no `|` by construction.
- `setOptionArgv(rows map[string]string) []string` — one batched
  `set-option -t <sess> @og_session_res <val> ; …` argv. Sessions sorted by name
  so the argv is deterministic and testable.

`main`:

- Accept exactly `--tick` (what the hook passes); anything else is a usage
  error on stderr, exit 2.
- Gate: no control-mode client → exit 0 having done nothing. This is what keeps
  the poller off every host nobody is bridged to.
- `ps proctree.PSArgs` → `proctree.Aggregate` → `runtime.NumCPU()` for cores
  (this is what retires `getconf _NPROCESSORS_ONLN`).
- One batched `tmux` exec. A tmux or `ps` failure exits 0 quietly: this is a
  background poller on a 5s clock, and a noisy failure would land in a pane.
- Stamps **every** session on the server, not just bridged ones — the daemon's
  subscription resolves against whichever session its control client is attached
  to, and the poller has the whole table in hand either way.

**Acceptance:** unit tests for all four helpers, including: a session name
containing `|`; a zero row (`0.0 0 8 <tick>`) is produced, not skipped; the gate
returns false for empty/garbage `list-clients` output. `go test ./...` green.

### S3 — Nix + generator wiring for the poller
`implement: opus`

Edited **together**, in this order:

1. `picker/default.nix` — `"sessionres"` in `subPackages`; `mv $out/bin/sessionres
   $out/bin/tmux-session-resources` in `postInstall`.
2. `generator/paths/paths.go` — `"tmux-session-resources"` added to
   `RequiredBin` (keep the list sorted).
3. `generator/render/status.go` — `"@og-res-tick"` appended to `tickHookNames`,
   and an **unconditional** setter
   `setHook("@og-res-tick", p.Bin["tmux-session-resources"]+" --tick")` appended
   beside the sweep setter. Unconditional because the poller is the remote half
   of a feature whose local half is opted into on a *different* machine: a host
   with no `remote.hosts` of its own is exactly the host someone bridges *to*.
4. `config/tmux.conf.nix` — `picker-session-res-bin`; add to `pathsToml.bin`;
   pass to `reference`; export `picker-generate` from the final attrset so the
   flake checks can name the binary's store path.
5. `config/tmux.conf.reference.nix` — new `picker-session-res-bin` param,
   `"@og-res-tick"` in `hookNames`, the matching setter appended to `setters` in
   the **same position** as in `status.go`.
6. `generator/render/status_test.go` — **both** `tickPaths()` (gains a
   `Bin: map[string]string{"tmux-session-resources": "/store/sr/bin/tmux-session-resources"}`
   field; it currently returns `Scripts` only, so `p.Bin[...]` would render an
   empty path and the test would assert the bug) **and** the expected strings in
   `TestTickHookIfShellJoinAndEscaping` / `TestTickHookIfShellClearsSurviveFeaturesOff`.
7. `flake.nix` `tick-floor-conf-assertions` — `RES_CLEAR_B`, `RES_CLEAR_OPT`,
   `RES_SETTER`; added to both `for` loops and to the ordering loops; `n -eq 4`
   → `n -eq 5`.
8. `flake.nix` `tick-floor-disabled-conf-assertions` — `RES_CLEAR_B` /
   `RES_CLEAR_OPT` in the clears loop, and `RES_SETTER` asserted **present**
   (it is unconditional, like the sweep setter). Built off the **default**
   `tmuxConfig`, not `disabledTmuxConfig`: unlike `tmux-update-icons`, the
   poller binary comes from `picker-generate`, whose inputs are the process
   icons and splash tips — neither `enrichEnable` nor `agentUsageEnable` moves
   its store path, so the two configs name the same binary.
9. `tests/tick-floor.bats:84` and `tests/tmux-next38-readiness.bats:463` — both
   carry a **hardcoded four-name loop** over the monitor names. Both become
   five. Missing these is a silently-passing suite, not a failure.
10. `flake.nix` `pickerChecked.checkPhase` — add `go test ./proctree/...` and
    `go test ./sessionres/...`, or S1's and S2's tests never run under
    `nix flake check`. **Also attempt `go test .`**: S5's tests live in the
    root package, which that list does not currently cover, so without it the
    PR's headline regression is not gated. **Measured green** by the plan-critic
    (`go test -count=1 .` in `picker/`, tmux next-3.9, `TMUX`/`TMUX_PANE` unset,
    scratch `CLAUDE_STATUS_DIR`: ok, 0.584s), so the line goes in. Scope note for
    the PR body: this brings ~16 root test files into `nix flake check` for the
    first time, so a pre-existing failure among them would surface as this PR's
    CI going red for reasons unrelated to #693.

**Acceptance:** `nix build .#default`; `nix flake check` green — specifically
`tmux-conf-extraction-assertions` (proves reference and generator agree),
`tick-floor-conf-assertions`, `tick-floor-disabled-conf-assertions`. Confirm by
inspection of the built conf that the res hook uses the empty `::` target and
carries no `#{` in its command.

### S4 — daemon: subscribe, dedupe, stamp
`implement: opus`

`picker/remotebridge/daemon/subscriptions.go`:

- `resSubName = "og_res"`.
- `subscribeFormats` gains a third result. The session-scoped `what` is the
  **empty string**, which is what `monitor_parse` reads as `MONITOR_SESSION` —
  the same empty-target spelling the `-B` hooks use, and for the same upstream
  reason. The returned bool is recorded but drives nothing: this shipper has no
  poll mode (spec R2).

New `picker/remotebridge/daemon/sessionres.go`:

- `const sessionResFormat = "#{@og_session_res}"`.
- **A malformed subscription spec is not reportable.**
  `cmd_refresh_client_update_subscription` (`cmd-refresh-client.c`) parses the
  `-B` argument and, on failure, silently *removes* the subscription and returns
  — no `%error`. (It is a separate code path from `set-hook -B`'s
  `monitor_parse`, which happens to accept the same empty-target spelling; do
  not cite one as evidence for the other.) So
  `sendSubscription`'s `Kind != Error` check cannot catch a typo in the spec,
  and this shipper (unlike its two siblings) has no poll backstop to mask one.
  The spec string is therefore a constant with its own unit test, which is the
  only guard that exists.
- `const sessionResRefresh = 30 * time.Second` — the floor at which an unchanged
  row is re-stamped so a quiet-but-live mirror does not age out (spec R3a).
  Comfortably above `mainLoopTickInterval` (5s), so the loop always has a chance
  to honour it.
- `sessionResRe` — `^[0-9]+(\.[0-9]+)? [0-9]+(\.[0-9]+)? [0-9]+ [0-9]+$`. Matches
  whole or nothing, which is the identity-field policy: a truncated number is a
  different number. It excludes `|` by construction, which is what protects the
  picker's pipe-delimited snapshot parse.
- `type resShipper struct { pending string; havePending bool; figures string;
  lastWrite time.Time }`.
  - `queue(v string)` — pure, records the raw value and sets `havePending`.
    Called from `dispatch`.
  - `flush(cfg Config)` — main loop only, no round-trips. **First line is
    `if !s.havePending { return }`**, and `havePending` is cleared on the way
    out. Without it the loop's own 5s tick would re-enter `flush` with a stale
    `pending`, and the refresh floor would turn into an unconditional re-stamp
    of whatever arrived last — including re-stamping a value the empty-value
    branch had just unset. With a pending value:
    - pending empty string → unset `@bridge_res`, clear `figures`;
    - pending non-empty and non-matching → ignore (drop whole), leave the last
      good stamp standing;
    - figures unchanged and `time.Since(lastWrite) < sessionResRefresh` → no
      write (this is what makes R1a's per-pass tick free);
    - otherwise write `set-option -t <LocalSess> @bridge_res
      "<f1> <f2> <f3> <time.Now().Unix()>"`.
  - `reset()` — clears `figures`/`lastWrite` **and** `pending`/`havePending`, so
    the next report writes even if identical and a value queued just before the
    outage cannot be applied after it as though it were fresh. Called by
    `repair`.
  - `clear(cfg)` — unsets the option; called from `teardown`, placed beside
    `labels.clear(cfg, reg)` (`daemon.go:771`) and **above** the
    `reg.all()`/`kill-session` block, which is where the existing comment
    ("Before the kill-session below, which is what makes the -u land") already
    puts an option unset that must land.
  - `subscribed bool` — set from `subscribeFormats`' third result. Recorded for
    symmetry and for a future backstop; nothing reads it today, and
    `daemon.go:895`'s assignment becomes three-way.
- `clearBridgeRes(cfg Config)` package-level, beside `clearBridgeState`.

`picker/remotebridge/daemon/conn.go` — in `reattach`, immediately after
`setBridgeState(cfg, bridgeStateDisconnected)`, call `clearBridgeRes(cfg)`. The
stamp describes a mirror that is no longer live, and dropping it at the source is
what implements spec R3a's third condition without the picker having to read a
second option.

`picker/remotebridge/daemon/daemon.go`:

- construct `res := newResShipper()` beside `agents`/`labels`; declare it with
  them so `teardown` can `res.clear(cfg)`;
- `dispatch`'s `SubscriptionChanged` case gains
  `if v, ok := subscriptionValue(l, resSubName); ok { res.queue(v) }`;
- the main loop calls `res.flush(cfg)` beside `agents.flush` / `labels.flush`;
- `repair` calls `res.reset()` before its `subscribe()`, so the re-subscription's
  re-report re-stamps a value the outage dropped.

**Acceptance:** table tests over `flush` driving a fake `LocalTmux`: a first
report writes; an identical report inside the floor writes nothing; the same
report past the floor writes again with a newer epoch; a malformed value writes
nothing and leaves the previous stamp; an empty value unsets; `reset()` makes an
identical report write; `flush` with nothing pending writes nothing.

For the subscription itself, a **live** assertion rather than string equality on
a constant — the string is the one thing a unit test cannot prove correct, and
the repo already pays for the capability (`pickerChecked` carries `mkTmux` in
`nativeBuildInputs` with `OG_REQUIRE_TMUX=1`). The test starts a scratch tmux on
its own `TMUX_TMPDIR` **and** its own `CLAUDE_STATUS_DIR`, attaches a control
client, subscribes with the real constant, sets `@og_session_res`, and asserts a
`%subscription-changed og_res $N - - - : <value>` line comes back — the
session-scope shape, which is exactly what a wrong `what` field would break
silently.

### S5 — picker: prefer the stamp, demote the ssh path
`implement: opus`

`picker/main.go`:

- `collectPanesSnapshot`'s format gains `#{@bridge_res}` as the **trailing**
  field; `sessions()`' `len(parts) != 10` becomes `!= 11`. Trailing because the
  fields before it are positional and a mid-format insert shifts every later one.
- `sessionData` gains `bridgeRes string`; `sessInfo` carries it the way
  `bridgeHost` already is (first pane of the session wins — it is session-scoped,
  so every pane reports the same value).

`picker/remote_resources.go`:

- `const bridgeResStaleAfter = 90 * time.Second` — three of the daemon's 30s
  refreshes, so one missed refresh never flaps a healthy mirror.
- `func parseBridgeRes(v string, now int64) (sessionResources, float64, bool)` —
  four space-separated fields; rejects a wrong count, a bad number, and an epoch
  older than `bridgeResStaleAfter` (or in the future beyond a small skew
  allowance, which would mean a clock jump rather than a fresh stamp).
- `mergeRemoteResources` is restructured into three passes, **per session**:
  1. cover every mirror session whose `bridgeRes` parses fresh — set `cpuPct`,
     `memMB`, `cores`, leave `resUnknown` false;
  2. collect the hosts that still have at least one **uncovered** mirror session;
     return early when that set is empty — this is the steady state, and it does
     no ssh work and no `bridgeSessionNames()` fork;
  3. for the remaining sessions only, the existing ssh path fills them or marks
     `resUnknown`. A covered session is never touched by this pass.
- The file header comment gains a paragraph stating the ssh path is now the
  legacy fallback for un-rebuilt remotes, with the sunset condition.

**Deviation from spec R3a, recorded deliberately:** the spec listed "not
`@bridge_state disconnected`" as a third condition the *reader* checks. It is
implemented one level up instead — S4's `clearBridgeRes` drops the stamp at the
moment the daemon marks the mirror disconnected, so the reader needs only
conditions 1 and 2. Same guarantee, one fewer snapshot field, and it keeps both
options owned by the one writer that already owns `@bridge_state`. `CLAUDE.md`
records it this way.

Also in `picker/main.go`: `paneMap()`'s `len(parts) != 10` becomes `!= 11`. It
parses the same rows and is what maps a pane to its session/window for the agent
counts; leaving it at 10 drops every pane from that map and silently empties the
agent column.

**Acceptance:** tests proving (a) a covered session takes the stamp and
`sshRemoteResources` is **not** called; (b) two mirrors of one host where only
one is covered — the host is probed, the uncovered one is filled, the covered
one keeps its stamped values; (c) a stamp past `bridgeResStaleAfter` is
uncovered; (d) a session with no stamp and no ssh answer keeps `resUnknown`;
(e) a zero row (`0.0 0 8 <now>`) is covered, not treated as absent; (f) both
snapshot parsers accept an 11-field row and still fail closed on a 10-field one
— `paneMap` included, asserted through `paneMap()` directly rather than only
through `sessions()`.

### S6 — documentation
`implement: sonnet`

- `CLAUDE.md` → **Remote Session Resources**: rewrite. The aggregator now runs on
  the remote; the subscription is the transport; `@og_session_res` and
  `@bridge_res` are named with their owners; the three retired workarounds are
  recorded as retired-from-the-primary-path; the freshness rule and its two
  self-heals are stated; the ssh path is described as the legacy fallback with
  its sunset condition ("deletable once every host in `@remote_bridge_hosts`
  reports a stamp").
- `CLAUDE.md` → **What the Remote Host Needs on PATH**: a new row — remote
  session CPU/Mem needs the remote's own rebuilt tmux-og (the `@og-res-tick`
  monitor and `tmux-session-resources`), and its absence degrades silently to the
  ssh fallback, the same shape as the remote-window-labels row.
- `CLAUDE.md` → the `-B` monitor bullet under **Key Conventions**: the hook list
  becomes five.
- `docs/superpowers/plans/2026-09-18-remote-resources-over-subscription.md` —
  this plan, committed with the code.

**Acceptance:** `nix build .#lint` green (typos, trailing whitespace).

## Validation (run in this order, all three are required)

```
nix build .#default
nix flake check
nix build .#lint
```

Plus, scoped during development: `cd picker && go test ./... && go vet ./...`
and `cd generator && go test ./... && go vet ./...`.

## Evidence for the PR

The behavioural claim is "a covered mirror renders from the subscription and
costs no ssh". The regression that pins it is S5's test (a): it fails on today's
code (which always calls `sshRemoteResources` for a bridged host) and passes on
the change. Recorded in the PR's `## Evidence` section with the command and
output.

## Risks and how each is caught

| Risk | Caught by |
| --- | --- |
| Session subscription spelling wrong (`:session:` instead of `::`) | `tick-floor-conf-assertions` for the hooks; S4's `subscribeFormats` test for the subscription |
| reference.nix and generator drift | `tmux-conf-extraction-assertions` |
| Snapshot field-count bump missed in the parse | S5 test (f) — every session silently vanishes from the picker otherwise |
| `@bridge_res` carrying a `|` | `sessionResRe` rejects it; S4 malformed-value test |
| Poller taxing an unbridged host | S2's gate test |
| A quiet-but-live mirror ageing out | S4's past-the-floor rewrite test + S5 (c) |
| `paneMap` left at 10 fields | S5 test (f) — the agent column empties silently otherwise |
| New packages' tests never running in CI | S3 item 10; confirmed by `nix flake check` failing if a deliberately broken test is introduced |
| A bats suite's hardcoded hook list passing vacuously | S3 item 9 |

## Amendment — rebase onto #695

While this branch was in flight, main landed #695 (`3ff508b`), which taught the
same tree walk to report the agent commands found anywhere in a session's
process tree (`psArgs` gained a trailing `comm`), and taught
`mergeRemoteResources`' ssh leg to merge them into a mirror's Procs column.
Taken as-is, S5's covered path would have dropped that for every stamped
mirror — a regression against main, not against the plan. Two changes close it,
both inside this plan's existing seams:

- **S1:** `proctree` absorbs #695 whole — `PSArgs` carries `comm`, `Totals`
  gains `AgentCmds`, and the manifest-derived command set and `.foo-wrapped`
  normalisation move with the walk, so the walk stays the one place this lives.
  `aggregateResources` passes `AgentCmds` through.
- **S2/S4/S5:** the stamp gains a fifth field, the agent commands comma-joined
  or `-`: `"<cpu> <mem> <cores> <tick> <agents>"` on the remote,
  `"<cpu> <mem> <cores> <epoch> <agents>"` locally. `sessionResRe` accepts only
  lowercase manifest-shaped names (no `#`, no `|`), the daemon dedupes on every
  field but the tick — so an agent appearing in the tree writes at once rather
  than waiting out the floor — and the covered path calls `mergeAgentCmds` the
  way the ssh leg does. No version skew is introduced: nothing but this revision
  emits the stamp, and a four-field row is rejected whole on both sides.

**S3 item 10, resolved at the gate:** only `go test ./proctree/...` joins
`pickerChecked.checkPhase`. The flake-check log shows the default
`tmux-og-go-tools` derivation already runs `go test` per subPackage — root `.`
and `sessionres` included — so naming those two there as well would only
recompile them a second time, the cost that check's own comment warns about.
`proctree` is a library, not a subPackage, which is why it alone needs naming.
