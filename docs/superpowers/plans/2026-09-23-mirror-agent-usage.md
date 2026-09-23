# Mirror agent usage (#743) — implementation plan

Spec: `docs/superpowers/specs/2026-09-23-mirror-agent-usage-design.md`
(accepted). Four links: remote publish → subscription → daemon ship → render.

Contract names (fixed, used by every step):

- remote global user option: `@og_agent_usage` — compact JSON
  `{"<cache-key>": <cache verbatim>, …}`, cache keys `claude codex cursor pi`
- subscription name: `og_usage`, empty `what`, format constant
  `agentUsageFormat`:
  `#{S:#{W:#{P:#{?#{m/r:(^|/)[.]?(claude|codex|cursor-agent|pi)(-wrapped)?$,#{pane_current_command}},#{pane_current_command} ,}}}}|#{@og_agent_usage}`
- local session option: `@bridge_usage` — sanitized compact JSON,
  `map[string]cache` with only open, valid agents

## Step 1 — remote publish (`scripts/tmux-agent-usage.sh`)

- [ ] In `--tick-run`, after the provider `wait`, add `publish_usage`:
  collect existing `$CACHE_DIR/{claude,codex,cursor,pi}.json`; none →
  `tmux set -gu @og_agent_usage`; else
  `v=$(jq -cn 'reduce inputs as $c ({}; . + {(input_filename | split("/") | last | rtrimstr(".json")): $c})' "${files[@]}")`
  and on jq success `tmux set -g @og_agent_usage "$v"`; jq failure → no write.
  Update the header comment (one or two lines: publishes for the bridge).
- [ ] `tests/agent-usage-gate.bats`: fake `tmux` additionally appends
  `$*` of `set` calls to `$TMUX_SET_LOG`. Add `pkgs.jq` to
  `agent-usage-gate-tests` `nativeBuildInputs` in `flake.nix`. Cases:
  - open claude with a seeded `claude.json` and a seeded (closed) `codex.json`
    → log has exactly one `set -g @og_agent_usage {"claude":…}` whose JSON
    `jq -e '.claude.windows[0].pct == 42 and (has("codex")|not)'` holds;
  - open claude with no cache → log has `set -gu @og_agent_usage`;
  - no agent open → no `set` call at all.
- [ ] `shellcheck scripts/tmux-agent-usage.sh`; `bats tests/agent-usage-gate.bats`.

## Step 2 — daemon shipper (`picker/remotebridge/daemon`) (implement: escalated)

Security-sensitive (format-injection sanitizer).

- [ ] `subscriptions.go`: add `usageSubName = "og_usage"`; `subscribeFormats`
  returns a 4th bool from `sendSubscription(rt, usageSubName, "", agentUsageFormat)`;
  update the package comment's shipper count/wording minimally.
- [ ] New `agentusage.go`:
  - `agentUsageFormat` const (above; no quotes inside — the subscribe path
    single-quotes it).
  - `usageRawMaxLen = 4096`, `usageLabelRe = ^[A-Za-z0-9._-]{1,12}$`,
    `usageMaxWindows = 8`, bounds pct `[0,1000]`, usd/limit `[0,1e7]`,
    reset_at ≥ 0.
  - typed structs (window `label,pct,reset_at,omitempty`; spend `usd,
    limit_usd *float64 omitempty`; cache `windows, monthly *, spend *`), json
    tags matching `picker/statusline/usage.go`'s `usageCache`.
  - `usageOpenSet(part string) map[string]bool`: `strings.Fields`, `path.Base`,
    `^\.(.*)-wrapped$` unwrap, map `claude codex cursor-agent→cursor pi`.
  - `sanitizeUsage(v string, skew int64) string`: over-cap → ""; split at
    first `|` (none → ""); decode JSON part into `map[string]json.RawMessage`
    (error → ""); for each key in fixed order `claude codex cursor pi` that is
    in the open set, decode with a plain typed `json.Unmarshal` (unknown
    fields ignored — never `DisallowUnknownFields`, so a provider adding a field
    later does not drop the agent); validate (drop the
    agent on any failure); shift every nonzero `reset_at` by `+skew`; marshal
    the kept map (empty → ""). Belt-and-braces: if the marshaled output
    contains `|` or `#`, return "" (unreachable by construction; a test pins
    it).
  - `usageShipper{skew; pending; havePending; written string; known bool}`,
    `newUsageShipper(skew)`, `reskew`, `queue(v)` (pure, no session filter),
    `flush(cfg)`: no pending → return; `LocalSess == ""` → return; compute
    `out := sanitizeUsage`; `known && out == written` → return; then
    `out == ""` → `set-option -u -t <sess> @bridge_usage`, else
    `set-option -t <sess> @bridge_usage out`; set `written, known`.
    `reset()` clears written/known/pending; `clear(cfg)` → `clearBridgeUsage`.
- [ ] `conn.go`: `clearBridgeUsage(cfg)` beside `clearBridgeRes`; `reattach`
  calls it right after `clearBridgeRes(cfg)`. `daemon.go` also calls it at
  startup next to `clearBridgeState(cfg)` (~line 786): mirror sessions are
  reused (#474), and a prior daemon killed while connected would otherwise
  leave its value until this one's first report.
- [ ] `daemon.go`: declare `usage *usageShipper` beside `res`; teardown
  `usage.clear(cfg)` (nil-guarded); construct `usage = newUsageShipper(skew)`
  beside `res`; `subscribe` closure ignores the 4th result; dispatch routes
  `subscriptionValue(l, usageSubName)` → `usage.queue(v)`; main loop
  `usage.flush(cfg)` after `res.flush(cfg)`; repair `usage.reskew(skew)` and
  `usage.reset()` beside `res`'s.
- [ ] Tests:
  - `subscriptions_test.go`: update `subscribeFormats` call sites for 4
    results; issued count 4 with the 4th containing `usageSubName+"::"`; add
    `agentUsageFormat` to the no-quotes loop.
  - new `agentusage_test.go`: table tests for `sanitizeUsage` — open gate
    filters (codex cache, no codex pane → absent); `.claude-wrapped`,
    `/nix/store/…/pi`, `cursor-agent` normalise; `#(touch /tmp/x)`,
    `#{pane_id}`, `#[fg=red]`, `a|b`, 13-char and empty label each drop only
    their agent; 9 windows drop; pct 1001 / usd -1 / reset_at -5 drop;
    unknown agent key dropped; an unknown field leaves the agent kept and is
    absent from the output; spend label/period not
    carried; over-cap and no-`|` and bad JSON → ""; skew shifts reset_at,
    leaves 0 alone; output never contains `|` or `#` (fuzz-ish loop over the
    payload cases). `usageShipper.flush` sequence test (fake `LocalTmux`
    recording argv): first report writes; identical re-report writes nothing;
    empty JSON → `-u`; first-ever empty report still issues `-u` (known=false);
    `reset` → identical report re-writes; no pending → nothing.
  - live test `TestAgentUsageSubscriptionReportsOpenAgents` modelled on
    `TestSessionResSubscriptionIsSessionScoped`: isolated tmux, a second
    session whose pane runs a copy of bash named `claude`
    (`claude -c 'sleep 600; :'`), subscribe with
    `subscribeCmd(usageSubName, "", agentUsageFormat)`, `set -g
    @og_agent_usage '{"claude":{"windows":[{"label":"5h","pct":7}]}}'`, wait
    for a session-scoped line; `sanitizeUsage(value, 0)` decodes to a claude
    entry with pct 7.
- [ ] `cd picker && go test ./remotebridge/daemon/ -run 'Usage|Subscri' -race`.

## Step 3 — statusline selection (`picker/statusline`)

- [ ] `main.go`: append `"#{@bridge_usage}"` to `volatileFields`; `args`
  gains `bridgeUsage`; `fetchVolatile` sets `a.bridgeUsage = f[22]`.
- [ ] `usage.go`: add `parseBridgeUsage(v string) map[string]usageCache`
  (empty/malformed → nil) and
  `usageFor(a args, localDir string, localOpen func() map[string]bool, now int64) string`:
  `a.bridgeHost != ""` → caches from `parseBridgeUsage(a.bridgeUsage)`, open =
  every key, `usageSegment`; local sessions keep today's logic (cheap cache read
  first, `localOpen()` only when caches exist). `main()` replaces its inline
  block with `usageFor(a, usageDir, openAgents, now)`, and runs it only when
  `ok` (spec §4 cold-start rule).
- [ ] Tests (`main_test.go`, `usage_test.go`): `volatileFields` length 23,
  index 22 = `#{@bridge_usage}`; `usageFor` — mirror with `@bridge_usage`
  renders its figures (`77%·5h`, `$12/$50`) and never calls `localOpen`
  (stub fails the test if called) even with a local cache dir holding a
  different figure; mirror with empty `@bridge_usage` renders "" with local
  caches present; mirror with malformed value renders ""; local session with
  a cache and `localOpen` true renders the local figure.
- [ ] `cd picker && go test ./statusline/`.

## Step 4 — integration (`tests/remote-m2-integration.bats`, `flake.nix`)

Depends on steps 2 and 3.

- [ ] `flake.nix` `remote-m2-integration-tests`: add
  `STATUSLINE = "${pickerChecked}/bin/tmux-statusline";`. Bats `setup()`:
  `go build -o … ./statusline` fallback when `STATUSLINE` unset, like
  DAEMON/RENDERER/CTL.
- [ ] New test `daemon ships the remote host's agent usage, gated and
  sanitized, into the mirror's statusline`:
  - agent stand-ins: copy `$(readlink -f "$(command -v bash)")` to
    `$BATS_TEST_TMPDIR/bin/{claude,pi}` (the copied name is what
    `pane_current_command` reports; proven on 3.7c).
  - SRC: `rem` session as usual; a separate `agents` session with a `claude`
    window and a `pi` window, each `<bin> -c 'sleep 600; :'` (host-wide scope:
    not the mirrored session). `set -g @og_agent_usage` with claude
    `{"windows":[{"label":"5h","pct":77}],"spend":{"label":"mo","usd":12.34,"period":"month","limit_usd":50}}`,
    pi with a `"#(touch $BATS_TEST_TMPDIR/pwned)"` window label, codex valid but
    no codex pane.
  - DST: a local `lo` session running the `claude` stand-in (so on main the
    local gate is open); `OG_AGENT_USAGE_DIR` with a local
    `claude.json` = `{"windows":[{"label":"5h","pct":11}]}`.
  - `bridge_up 1 usage --host lab` — the daemon stamps `@bridge_host` from
    `--host` (empty under `--test-local` without it, which would send the
    selector down the local path; precedent `bridge_up 1 pin --host lab`);
    assert DST `show -t host-sess -v @bridge_host` is `lab`; poll DST `show -t host-sess -v @bridge_usage` until
    nonempty; assert it `jq -e 'keys == ["claude"]'` (needs `pkgs.jq` in that
    check's `nativeBuildInputs`), no `#(`.
  - remove `/tmp/og-statusline/host-sess` first (the statusline's last-good
    cache is fixed there and shared across runs); run statusline against DST: `TMUX="$TMUX_TMPDIR/tmux-$(id -u)/m2dst,0,0"
    OG_AGENT_USAGE_DIR=… CLAUDE_STATUS_DIR=… "$STATUSLINE" --session host-sess
    --agent-usage-monthly-threshold 50 --icon-usage-claude C --icon-usage-pi P
    --icon-usage-codex X --icon-usage-cursor U` + theme flags; last line
    contains `77%·5h` and `$12/$50`, not `11%·5h`, not `#(`; and
    `[ ! -e pwned ]`.
  - kill SRC's claude window; poll until DST `@bridge_usage` is unset.
- [ ] Red evidence: in a scratch `git worktree add` of `main` (never the live
  tree), copy only the new test + flake/setup edits, run the test against
  main's daemon/statusline built by the bats fallback: must fail on the
  assertion (no `@bridge_usage` / renders `11%·5h`), not on setup. Record
  command and failure for the PR `## Evidence`. Then green on this branch.
- [ ] `bats tests/remote-m2-integration.bats -f 'agent usage'` locally.

## Step 5 — docs

- [ ] `docs/agents/bridge-shipped-state.md`: new `## Remote Agent Usage`
  section — publish option, subscription + live gate, sanitizer policy
  (fails closed, re-typed, label alphabet, `|`/`#` exclusion), skew on
  `reset_at`, per-session storage and why, lifecycle, known limits (no
  staleness bound; not-rebuilt remote → no segment; orphaned mirror).
- [ ] `docs/agents/enrichment.md` "Agent Usage Limits": the poller publishes
  `@og_agent_usage`; a mirror session renders `@bridge_usage` instead of the
  local set and never local figures; point at bridge-shipped-state. Adjust the
  "Mirror/remote panes never count toward the gate" bullet so it stays true
  (local gate unchanged; mirrors now use the remote's own gate).
- [ ] `docs/agents/bridge-daemon.md` PATH table: `Remote agent usage` row —
  nothing new on PATH, a remote rebuilt from this revision (its
  `tmux-agent-usage` publishes); no capability probe, silent degrade.
- [ ] `docs/agents/scripts.md` `tmux-agent-usage` row: add that it owns
  `@og_agent_usage` (rg the row).

## Step 6 — gates

- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` (inside
  the devshell).

## Consumer map

As in the spec's table; additions found during planning:

- `tmux-remux` is an external flake input (not in `scripts/`), and
  `og-remote-open` already deals with mirror-session ghosts it resurrects.
  Whether remux restores session user options is outside this repo; it does
  not matter here, because a new daemon's first report always re-stamps or
  clears `@bridge_usage` (`known=false`), so a resurrected value cannot survive
  an attach. Compatible.
- `agent-usage-gate-tests` and `remote-m2-integration-tests` gain `jq`.

## Order

1 ∥ 2 ∥ 3 → 4 → 5 → 6.
