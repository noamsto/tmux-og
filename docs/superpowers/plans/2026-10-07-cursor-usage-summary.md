# Cursor usage segment reads usage-summary (#944)

Switch `tmux-agent-usage-cursor` from DashboardService (on-demand spend only;
0% on team/enterprise plans) to the dashboard's `GET https://cursor.com/api/usage-summary`,
and add a policy-free `--print` mode sharing the same fetch/normalize function.

## Design

One function, `cursor_usage`, does token → account → fetch → normalize and
prints the **print object** (below) or returns 2/3/4/5. Both modes call it:

- `--print`: `out=$(cursor_usage) || exit $?; printf '%s\n' "$out"`. No cache
  dir, no gate, nothing written.
- tick (no arg): `OG_AGENT_USAGE_DIR` gate as today; `norm=$(cursor_usage) || exit 0`;
  a jq projection of the print object into the cache schema; atomic tmp+mv.
  Any failure exits 0 before the write, so the previous cache stays byte-identical.

Auth ported from refresh-budget's `probe_cursor` (minus its `cursor-agent`
presence check, which is the dispatcher's own gate): Linux
`${XDG_CONFIG_HOME:-$HOME/.config}/cursor/auth.json` first non-empty of
`.accessToken`/`.access_token`/`.token`; Darwin `timeout 10 security
find-generic-password -s cursor-access-token -a cursor-user -w`. Account from
`cli-config.json` (`$dir` on Linux, `~/.cursor/` on Darwin)
`.authInfo.authId`/`.authInfo.userId`, else JWT `sub`; part after last `|`.
Both must match `^[A-Za-z0-9._-]+$`. Request via `curl -sf --max-time 15 -K -`
with the Cookie header on stdin; every auth-touching call discards stderr.
The `CURSOR_AUTH` override is dropped (nothing sets it outside tests).

The dispatcher (`scripts/tmux-agent-usage.sh`) gates the cursor fork on
`-f $HOME/.config/cursor/auth.json`, which never exists on macOS (keychain) or
under a non-default `XDG_CONFIG_HOME`. The gate drops to the open-pane check
alone; the provider's own no-token path is a jq read (or one bounded
`security` call) and exits 0 silently.

### Print object (public contract, documented in enrichment.md)

```json
{"plan_type": "enterprise", "unlimited": false,
 "cycle": {"starts_at": 1790812800, "resets_at": 1793491200},
 "plan": {"used_pct": 38.67, "used_usd": 406.03, "limit_usd": 1050},
 "pools": {"auto_pct": null, "api_pct": null},
 "on_demand": [{"scope": "team", "enabled": true, "used_usd": 861.55, "limit_usd": 6000}]}
```

- `plan_type`: `membershipType` string or null. `unlimited`: `isUnlimited == true`.
- `cycle`: epoch seconds; both set (end > start) or both null.
- `plan.used_pct`: raw (unrounded) — overall used/limit*100 when
  `individualUsage.overall` is bounded (limit number > 0, used number), else
  max(auto, api) of `individualUsage.plan`, else null (only when unlimited).
  `used_usd`/`limit_usd`: overall used/limit ÷ 100 when numeric (`limit_usd`
  only when > 0), else null.
- `pools`: raw `autoPercentUsed`/`apiPercentUsed`, null when absent.
- `on_demand`: every `individualUsage.onDemand` (`scope:"individual"`) and
  `teamUsage.onDemand` (`scope:"team"`) object present, enabled or not;
  `enabled` boolean (`== true`), `used_usd` (null if non-numeric),
  `limit_usd` null when the limit is null/non-numeric (= uncapped); a
  numeric limit `<= 0` is passed through as-is and documented (probe_cursor's
  `bounded` treats it as neither capped-and-full nor uncapped, so consumers
  must test `limit_usd > 0` before comparing).
- Not object, or `used_pct` null and not unlimited → exit 5.
- Exit codes: 2 no token, 3 no account, 4 fetch failed, 5 unrecognised.

Dispatcher derivation: `credits_cover` = any enabled pool with `limit_usd`
null or `used_usd < limit_usd`; `limit_reached` = `max(used_pct, pools) >= 100`
or any enabled bounded pool at its limit. All inputs present.

### Cache projection

- `windows: []` (no burst window any more — usage-summary has none).
- `monthly`: `{label:"mo", pct: used_pct rounded to 1 decimal}` + `reset_at`
  when `cycle.resets_at` non-null; null when unlimited or pct null.
- `spend`: when `plan.used_usd` non-null → `{label:"mo", usd, period:"cycle"}`
  + `limit_usd` when non-null and not unlimited; else null (individual shape).

### Renderer decision

`usageSegment` already renders `spend` unconditionally and the monthly % only
at/above the threshold, so `$406/$1050` shows at 38.7% with no Go change.
Keep the % threshold-gated: below it the dollar pair already says how full the
plan is; at/above it the colour-graded % adds the warning. Cursor's render form
is unchanged, so `TestUsageSegmentCursorRenderUnchanged` stays as is; a new
test pins the below-threshold enterprise render.

## File list

- `scripts/tmux-agent-usage-cursor.sh` — rewrite: `cursor_usage` + `--print` + tick projection.
- `scripts/tmux-agent-usage.sh` — cursor gate: drop the Linux-only auth.json presence check.
- `tests/agent-usage-gate.bats` — open cursor-agent with no auth.json still forks the provider.
- `config/tmux.conf.nix` — `ogInternal` comment: the cursor provider's `--print` is called by other tools (partition unchanged).
- `tests/agent-usage-providers.bats` — curl stub learns `-K -` (Cookie from stdin, argv logged); cursor tests replaced; `OG_AGENT_USAGE_DIR unset` test updated to new fixtures.
- `picker/statusline/usage_test.go` — new `TestUsageSegmentCursorDollarsBelowThreshold`.
- `docs/agents/enrichment.md` — cursor bullets rewritten; `--print` contract section; `limit_usd` source sentence.
- `docs/agents/scripts.md` — cursor part of the provider row.
- `docs/superpowers/plans/2026-10-07-cursor-usage-summary.md` — this plan.

## Steps

- [ ] **Step 1: failing bats tests** — `tests/agent-usage-providers.bats`.
  Stub curl: when argv has `-K -`, read stdin, take the `Cookie:` header
  value, require it equal `WorkosCursorSessionToken=$EXPECT_ACCOUNT::$EXPECT_TOKEN`;
  append argv to `$BATS_TEST_TMPDIR/curl-argv.log`. Pi's `-H` path unchanged.
  `cursor_setup`: auth.json `{"accessToken":"cursor-test-not-a-token"}`,
  cli-config.json `{"authInfo":{"authId":"auth0|user_synth01"}}`,
  `EXPECT_ACCOUNT=user_synth01`; a fake `uname` printing `Linux` (overridden to
  `Darwin` in the macOS test) so a macOS host still takes the Linux fixture path. Fixture helpers write `usage-summary.json`
  (synthetic; team shape = the task's numbers with fake ids). Tests:
  1. team → cache `.monthly == {"label":"mo","pct":38.7,"reset_at":1793491200}`,
     `.spend == {"label":"mo","usd":406.03,"period":"cycle","limit_usd":1050}`, `.windows == []`.
  2. individual (`plan.autoPercentUsed 20, apiPercentUsed 61.5`, no overall) → `.monthly.pct == 61.5`, `.spend == null`.
  3. unlimited (`isUnlimited:true`, overall `{used:5000, limit:null}`) → `.monthly == null`, `.spend == {"label":"mo","usd":50,"period":"cycle"}`.
  4. pre-seeded cache + missing fixture (fetch fails) → `cmp` byte-identical; same for `{}` response and for no account (exit-3 path).
  5. `--print` team/individual/unlimited → exact `jq -cS` object; `OG_AGENT_USAGE_DIR` unset, cache dir empty after.
  6. `--print` exit codes: no auth.json → 2; token `not-a-jwt` and no cli-config → 3; missing fixture → 4; `{"foo":1}` → 5 — each with empty stdout.
  7. account from JWT `sub` when cli-config absent (token = synthetic `header.<b64url {"sub":"auth0|user_jwt9"}>.sig`).
  8. Darwin path: fake `uname` → `Darwin`, fake `security` prints token, cli-config at `~/.cursor/`.
  9. token absent from stdout, stderr, cache file and curl argv log (team, `--print` and tick).
  10. update `OG_AGENT_USAGE_DIR unset` test to the new fixture.
  Run: `bats tests/agent-usage-providers.bats` → new cursor tests fail, pi tests pass.
- [ ] **Step 2: rewrite the provider** — `scripts/tmux-agent-usage-cursor.sh` per Design,
  header comment included (no DashboardService/burst/totalCostCents left).
  Run: `bats tests/agent-usage-providers.bats` → all pass;
  `shellcheck scripts/tmux-agent-usage-cursor.sh tests/agent-usage-providers.bats`; `shfmt -d` clean.
- [ ] **Step 2b: dispatcher gate** — failing test first in `tests/agent-usage-gate.bats`:
  `FAKE_PANES='cursor-agent'`, no `~/.config/cursor/auth.json` → `USAGE_LOG` is `cursor`.
  Then `scripts/tmux-agent-usage.sh`: `[[ -v OPEN[cursor-agent] ]] && (`.
  Run: `bats tests/agent-usage-gate.bats` → pass. `config/tmux.conf.nix` ogInternal comment tweak.
- [ ] **Step 3: Go render test** — `picker/statusline/usage_test.go`: cursor cache
  `Monthly{mo, 38.7, reset_at}` + `Spend{mo, 406.03, cycle, LimitUSD 1050}`,
  threshold 50 → `"#[fg=#9a8]CU #[fg=#9a8]$406/$1050  "`.
  Run: `cd picker && go test ./statusline/ -run Cursor` → pass (no production change needed).
- [ ] **Step 4: docs** — `enrichment.md`: replace the "Cursor has no short windows"
  bullet with the usage-summary description + renderer decision; fix the
  `limit_usd` source sentence; add a "`tmux-agent-usage-cursor --print`"
  contract subsection (fields, units, nulls, exit codes, no cache write,
  public interface; the limit<=0 note). `enrichment.md` "Auth is the CLIs' own" bullet: cursor's
  keychain/XDG token sources and the open-only gate. `scripts.md`: the provider row's cursor
  token source and the "Cursor chains four DashboardService calls" sentence; the dispatcher row's gate if it names auth.json.
- [ ] **Step 5: gates** — `nix build .#default`, `nix flake check`, `nix build .#lint` → pass.
- [ ] **Step 6: manual** — `bash scripts/tmux-agent-usage-cursor.sh --print | jq '{unlimited, cycle, plan, pools, on_demand: [.on_demand[] | {scope, enabled, used_usd, limit_usd}]}'` against the real account; report numeric fields only.

## Acceptance

- bats shapes/failed fetch/`--print`/exit codes/token leak → Step 1+2 bats run (in `nix flake check` `agent-usage-provider-tests`).
- Go renderer test → Step 3 `go test`.
- Manual real-account `--print` → Step 6 output (numbers only in PR).
- `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 5.
