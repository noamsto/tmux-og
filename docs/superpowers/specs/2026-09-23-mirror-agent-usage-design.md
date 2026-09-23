# Mirror sessions show the remote host's agent usage (#743) — design

## Problem

The top-right usage segment (`picker/statusline/usage.go`) always renders
**this** host's `/tmp/og-agent-usage/<agent>.json` caches, gated by **this**
host's open agent panes (`openAgents()`, a local `list-panes -a`). In a bridge
mirror session every pane runs a renderer, so the segment shows local
subscriptions for a session whose agents run elsewhere — and the remote often
runs different keys/accounts (another OpenRouter key for pi, another Cursor
account). The bridge cannot read remote files, so nothing carries the remote's
figures today.

## Goal / acceptance

1. A client whose current session is a mirror sees the **remote host's**
   usage/spend/budget; a local session keeps the local set.
2. Only remote agents with an open remote pane render.
3. No carried string reaches a tmux format unsanitized (a `#(…)` payload is
   proven inert).
4. `nix build .#default`, `nix flake check`, `nix build .#lint` pass.
5. Docs updated: `bridge-shipped-state.md`, `enrichment.md`, the PATH table
   in `bridge-daemon.md`.

## Design

Four links: remote publish → remote subscription → daemon ship → local render.

### 1. Remote publish — `@og_agent_usage` (global user option)

`tmux-agent-usage --tick-run` already refreshes caches only for open agents and
deletes the cache of every agent not open. After its providers finish
(`wait`), it publishes the surviving cache files as one compact JSON object,
keyed by cache name, onto a **server-global** user option:

```
tmux set -g @og_agent_usage '{"claude":{"windows":[…],"monthly":…},"pi":{…,"spend":{…}}}'
```

- Built with `jq -c` from `$CACHE_DIR/{claude,codex,cursor,pi}.json`, each
  cache verbatim under its key — no schema change, the value *is* the caches.
- No cache files left → `tmux set -gu @og_agent_usage`. This fires only on a
  pass that ran (some agent open, none with a cache). When **every** remote
  agent closes, tick/tick-run exit at their `OPEN` gate before publishing, so
  the last value stays on the remote server; the daemon's live gate (§2) hides
  every closed agent, and on reopen the old figures show until the pass that
  the reopen triggers finishes — exactly the local path, whose cache files also
  survive an all-closed period.
- A `jq` failure (a torn/garbage cache) leaves the option as it was — same
  "failed refresh keeps the previous value" posture as every provider.
- Driven by the existing `@og-usage-tick` `-B` monitor hook, so it runs on a
  bridge-only remote too (#603). One extra `tmux` fork per tick-run (default
  every 120s, only while some agent is open).
- The local host publishes its own `@og_agent_usage` too; nothing local reads
  it. Harmless, and it keeps the script host-agnostic.

Global, not per-session: usage is host-wide (one account per host), and a
global option is visible from every session's format, which is what the
session-scoped subscription below evaluates in.

### 2. Remote subscription — `og_usage`, live open gate included

The daemon adds a fourth subscription beside `og_labels`/`og_agents`/`og_res`,
session-scoped (empty `what`), whose format carries **both** the host's open
agent panes and the published caches:

```
#{S:#{W:#{P:#{?#{m/r:(^|/)[.]?(claude|codex|cursor-agent|pi)(-wrapped)?$,#{pane_current_command}},#{pane_current_command} ,}}}}|#{@og_agent_usage}
```

The `S:`/`W:`/`P:` loop walks every pane on the remote server and emits the
`pane_current_command` of each agent pane (space-terminated), then `|`, then
the published JSON. tmux re-evaluates subscriptions once a second and reports
only on change, so:

- **The per-agent gate is live**, with the same key as the local gate
  (`pane_current_command`, never `@bridge_proc`): closing the last remote
  claude pane removes claude's block within ~1s, without waiting for the
  poller's next pass (≤ `refreshSeconds`, 120s default). This is what makes
  acceptance #2 hold as tightly as the local gate does, rather than lagging a
  refresh window.
- Traffic is a line only when the agent-pane set or the published JSON moves.
- Verified on tmux 3.7c with a scratch server and a control client
  (`%subscription-changed u $1 - - - : claude .pi-wrapped |{"x":1}`, then the
  loop part updating on `kill-window`, then the JSON part on `set -g`).
- Why not the poller's `OPEN` set alone: the poller only scans past its
  refresh window, so a closed remote agent would keep rendering for up to
  120s; the local segment hides it within a second.
- Session-pin excursions (#396) are irrelevant here: every session reports the
  same host-wide value, so unlike `og_res` no report is filtered by session id.
- A remote that is not rebuilt reports `<agents>|` (empty JSON): nothing to
  ship. A remote whose tmux lacks subscriptions already degrades every
  subscription; this one has no poll fallback, like `og_res`.

### 3. Daemon ship — `usageShipper` → local `@bridge_usage` (session option)

New `picker/remotebridge/daemon/agentusage.go`, shaped like `resShipper`
(queue from dispatch, pure; flush from the main loop, no round-trip):

1. Split the report at the **first** `|`: open part, JSON part.
2. **Open set**: tokens of the open part, each normalised exactly as the
   renderer's `openAgents()` does (basename, `^\.(.*)-wrapped$` unwrap, then
   the `claude|codex|cursor-agent→cursor|pi` map); anything else ignored.
3. **Caps**: the raw value is capped (4 KiB) before parsing; over the cap the
   report is treated as malformed.
4. **Sanitize by re-typing**: decode the JSON into `map[string]json.RawMessage`
   and each *known* agent key (`claude codex cursor pi`) that is also in the
   open set into a typed cache struct. An agent entry is dropped whole (the
   identity-field policy: no partial reading) when any of:
   - a window/monthly `label` fails `^[A-Za-z0-9._-]{1,12}$` (excludes `#`,
     `|`, spaces, braces — a `#(…)`/`#{…}`/`#[…]` payload cannot survive);
   - more than 8 windows;
   - `pct` outside `[0, 1000]`, `usd`/`limit_usd` outside `[0, 1e7]`, or a
     negative `reset_at`.
   Unknown keys and unknown fields are dropped by the typed decode;
   `spend.label`/`spend.period` are not rendered and are not carried.
5. **Clock skew**: every nonzero `reset_at` is shifted by the shipper's
   measured skew (`localNow − remoteNow`, the value `resShipper` already gets),
   so the renderer's `↻<dur>` countdown is on the local clock.
6. Re-marshal the surviving map with `encoding/json` (compact; keys sorted).
   The output alphabet is then JSON punctuation, digits, the fixed field
   names, the validated labels and agent keys — no `|` (the statusline reads
   it out of a `|`-delimited `display-message` row that fails closed on a
   wrong field count), no `#`.
7. **Write**: nonempty → `set-option -t <local-sess> @bridge_usage <json>`;
   empty map (no open agent with data, empty/malformed JSON, not-rebuilt
   remote) → `set-option -u`. Malformed fails **closed** (unset), unlike
   `og_res`'s keep-previous: a figure the daemon cannot vouch for must not
   stand in for the remote's.
8. **Unchanged-row suppression**: skip the write when the output equals the
   last one written; the first report after start/reset always applies (so a
   stale value from a previous daemon on the same session is replaced or
   cleared).

Lifecycle, mirroring `@bridge_res`:

- `reattach` clears `@bridge_usage` at the moment it stamps
  `@bridge_state disconnected` (figures over a dead link describe nothing
  live); `repair` calls `usage.reset()` before re-subscribing, so the
  re-report re-stamps.
- `teardown` clears it (near-vacuous — the session is killed — but it is the
  path when the kill fails).
- `reskew` on repair, like `res`/`agents`.

Stored per mirror **session**, not per host: each daemon owns exactly one
mirror session, so a session option needs no cross-daemon coordination —
two mirror sessions of one host each carry the same host-wide value, and one
daemon's teardown cannot delete what another still serves (a host-keyed file
would need refcounting for exactly that). It also dies with the session.

### 4. Local render — selection in `tmux-statusline`

- `fetchVolatile` gains `#{@bridge_usage}` as one more `|`-field (resolved
  from the session; no extra fork).
- Usage is computed only when `fetchVolatile` succeeded: on a failed fetch
  with no last-good frame (cold start) the bridge fields are empty and a
  mirror would otherwise fall into the local path for that frame.
- A new `usageInputs`-style selector decides the caches and gate:
  - session has `@bridge_host` (the session-scoped mirror marker
    `og-remote-open` stamps) → caches = `@bridge_usage` decoded as
    `map[string]usageCache`; gate = every key present (the daemon already
    applied the remote's live open gate). Local caches and the local
    `list-panes` gate are **not** consulted — a mirror never shows local
    figures, even when `@bridge_usage` is absent (not-rebuilt remote,
    disconnected, nothing open) — it shows no usage segment.
  - otherwise → unchanged local path (`loadUsageCaches` + `openAgents`).
- `usageSegment` itself is unchanged: same order, monthly threshold, colors,
  reset suffix and `$spend[/$limit]`.
- The renderer does not re-sanitize (single owner of the rule is the daemon,
  per the repo convention), but a malformed `@bridge_usage` decodes to nothing.

## Consumer map (new carried value)

| Stage | Symbol / file | Disposition |
|---|---|---|
| Producer | `tmux-agent-usage --tick-run` → `@og_agent_usage` (remote, global) | new; bats: publish from open caches, unset when none |
| Cache files | providers → `/tmp/og-agent-usage/*.json` | compatible, unchanged schema |
| Transport | remote tmux subscription `og_usage` (`subscribeFormats`) | new; live integration test (malformed specs are silently dropped, so the spec string needs a live test) |
| Parse/sanitize | `usageShipper.queue/flush` (`agentusage.go`) | new; Go unit tests: gate, `#(…)`, caps, malformed, skew, dedupe, reset |
| Persistence | local session option `@bridge_usage` | new; cleared in `reattach`, `teardown`; reset in `repair` |
| Consumer | `picker/statusline` `fetchVolatile` + usage selection | changed; Go tests for selection; integration test through the binary |
| Other readers of session `@bridge_*` | picker's `list-panes -a` row (`picker/remote_resources.go`, `picker/main.go`) | compatible — reads named options only; `@bridge_usage` not added there |
| Local gate | `openAgents()`, `scan_open_agents` | compatible — local path unchanged; mirror path never consults them |
| tmux-remux | persist saves/restores | to verify in plan: mirror sessions/`@bridge_*` not persisted |

## Tests / evidence

- **Go (daemon)**: parse/sanitize table tests; open-gate filtering; `#(touch …)`
  label drops the agent; `|` never appears in output; over-cap and malformed
  → unset; skew shifts `reset_at`; unchanged report → no write; `reset` →
  re-write; subscription spec string constant.
- **Go (statusline)**: selector — mirror session with `@bridge_usage` renders
  remote figures and ignores local caches; mirror without it renders nothing;
  local session renders local.
- **bats (`tests/agent-usage-gate.bats`)**: tick-run publishes
  `@og_agent_usage` holding only open agents' caches; unsets when none survive.
- **bats integration (`tests/remote-m2-integration.bats`)**: SRC (remote)
  carries `@og_agent_usage` for claude, pi and codex; live panes whose
  commands are `claude` and `pi`, none for codex; pi's entry carries a `#(…)`
  label. The daemon stamps DST `@bridge_usage` with claude only (codex gated
  out by the open set, pi dropped by the sanitizer despite being open); `tmux-statusline` run against DST's mirror
  session renders the remote `$` figure (and never `#(`), while a local cache
  with a different figure is ignored; closing the remote claude pane removes
  it. Red on `main` (no `@bridge_usage`; statusline renders the local
  figure), green with the change.

## Out of scope

`pumpInput`/dead-pane dismissal (#748); `picker/statusline/claude.go` (#745);
provider scripts and cache schema; the session picker.

## Known limits (documented)

- A remote not rebuilt publishes nothing → a mirror shows no usage segment
  (no capability probe; same shape as remote session resources).
- **No staleness bound** on the published value, unlike `og_res`'s tick +
  30s cutoff: a remote poller that stops while agents stay open (hook gone, or
  a resident server predating the rebuild, #407) keeps serving its last
  figures on every subscribe. Accepted at a 120s refresh cadence for figures
  that move slowly — the same staleness the local cache files have when the
  local poller stops.
- An orphaned mirror (daemon killed without teardown) keeps its last
  `@bridge_usage` until the session dies.
