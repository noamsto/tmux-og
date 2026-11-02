# Plan: relay graphics and cell env follow the size owner (#1035)

Spec: `docs/superpowers/specs/2026-11-02-relay-env-follows-owner-design.md`.
The slot holds the size owner's values. Every write is a remote-side
conditional `if-shell -F`. A claim and a survivor's release republish. The
teardown unsets only when no other control client is attached.

Orchestration consult: not tripped. One Go package (`picker/remotebridge/daemon`),
three files of production code, and a contract the spec already fixes. An opus
plan suffices.

Out of bounds: `daemon/predictor.go`, `daemon/predict.go` (a parallel worker
owns them). No new session option and no remote script change.

## Fixed contract (every step and test uses exactly this)

- Owned condition, `relayOwnedCond(token)`:
  `#{?#{@og_bridge_owner},#{m:<token>|*,#{@og_bridge_owner}},1}`. This is true
  when the record is unset or names `token`. Build it from `bridgeOwnerOption`,
  never a second literal.
- `relayEnv{target, token string}` (in `relayenv.go`), with one builder per write:
  - `guard(cmd)`: returns `cmd` unchanged when `token == ""`. Otherwise it returns
    `"if-shell -F -t " + tmuxQuote(target) + " " + tmuxQuote(relayOwnedCond(token)) + " " + tmuxQuote(cmd)`.
  - `relay(v) = guard(RelayEnvCmd(target, v))`,
    `cell(c) = guard(CellEnvSyncCmd(target, c))`.
  - `publish(view *Viewing) []string = {relay(view.Relay.Load().String()), cell(view.Cell())}`.
  - Guarded iff size ownership is on. `ownerCtl.relayEnv()` returns
    `relayEnv{target: o.sessID, token: o.own.token}`. That is the only
    guarded construction, so the token and target have one source. Run uses
    `owner.relayEnv()` when `ownerOn`, else `relayEnv{target: cfg.RemoteSession}`.
    In that unguarded case every string is byte-identical to today's.
- Teardown write, `relayEnvTeardownCmd(sessID string, attached int)`:
  `"if-shell -F -t " + tmuxQuote(sessID) + " " + tmuxQuote("#{==:#{session_attached}," + strconv.Itoa(attached) + "}") + " " + tmuxQuote(RelayEnvUnsetCmd(sessID) + " ; " + CellEnvUnsetCmd(sessID))`.
- Release write: `ownerReleaseCmd(sessID, token, client string, then ...string)`.
  The branch becomes `unset` followed by `" ; " + cmd` for each `then`. With no
  `then` it is byte-identical to today's. The release passes
  `RelayEnvCmd(o.sessID, relay), CellEnvSyncCmd(o.sessID, cell)`. These are
  unguarded: the branch runs only while the record still names the departed
  owner, and its own unset just cleared it.
- Multiple commands inside one `if-shell` branch are joined with `" ; "`. This
  was verified against a scratch tmux: both branch commands ran, and a false
  condition ran neither. The per-command `og-fanout` barrier in
  `stream.stampAll` absorbs the branch's extra reply blocks.

## Steps

### 1. Builders — `implement: sonnet`

Files: `picker/remotebridge/daemon/relayenv.go`,
`picker/remotebridge/daemon/relayenv_test.go`.

- Add `relayOwnedCond`, `relayEnv` (with `guard`, `relay`, `cell` and `publish`)
  and `relayEnvTeardownCmd` per the contract. Doc comments follow the file's
  voice and say why: the record is the arbiter (#979), and the condition is
  evaluated atomically remote-side.
- Keep `RelayEnvCmd`, `RelayEnvUnsetCmd`, `CellEnvCmd`, `CellEnvUnsetCmd` and
  `CellEnvSyncCmd` unchanged. The unguarded path and the builders above use them.
- Unit tests (table, exact strings): unguarded `relay`/`cell` equal today's
  builders. Guarded ones wrap with the exact condition. `publish` reads the
  `Viewing`. `relayEnvTeardownCmd("$3", 2)` gives the exact string.
- Gate: `cd picker && go test ./remotebridge/daemon/ -run 'RelayEnv|CellEnv|CellPx'`.

### 2. Owner: claim and release publish, env teardown — `implement: sonnet` (after 1)

Files: `picker/remotebridge/daemon/sizeowner.go`,
`picker/remotebridge/daemon/sizeowner_test.go`,
`picker/remotebridge/daemon/sizeowner_wiring_test.go`.

- `ownerCtl` gains `view *Viewing` and the method `relayEnv()` (contract).
- `claimWith`: after `ownerClaimCmd(...)`, append `o.relayEnv().publish(o.view)...`.
  It stays in the same `send` batch, ahead of the flag commands and `withRecalc`.
- `ownerReleaseCmd` gains `then ...string` per the contract. `releaseDue` passes
  the releaser's values (`o.view`).
- New `func (o ownerCtl) envTeardown()`. It does its own one-batch read, not
  `o.read()`, which also rewrites the owner state:
  `display-message -p -t '$N' <ctlSafe("#{client_name}|#{session_attached}")>`
  and `sessionClientsCmd(o.sessID)`. A lost or `%error` reply, or a malformed
  first reply, sends nothing. If any client row other than self has `control`
  set, it sends nothing. Otherwise it runs
  `o.send(relayEnvTeardownCmd(o.sessID, attached))`, with `attached` the
  `#{session_attached}` it read. That is tmux's own count, which skips
  suspended and exiting clients, so a suspended tty does not make the guard miss.
  The doc comment says why: never clear values another attached controller
  depends on (#1035), the count closes the read-to-write race, and a tty client
  does not block. The read is bounded by the connection's reply deadline, and a
  closed connection fails it at once.
- `testOwnerCtl` sets `view` (a `Viewing` with a relay and cell set). Update
  every expected claim and release batch in the wiring tests to the new commands,
  building them from the builders, never literal copies.
- New unit tests:
  - The claim batch carries the guarded publish right after the record write.
  - The release command carries the releaser's values.
  - `envTeardown` sends nothing when the read lists another control client.
  - `envTeardown` sends `relayEnvTeardownCmd("$1", n)` when only tty clients or
    no others are attached.
  - `envTeardown` sends nothing on a failed read (an `%error` reply).
  - `envTeardown`'s guard count is the read's `session_attached`, not the row
    count. For example, 3 rows with `session_attached` 2 gives `…,2}`.
- Gate: `cd picker && go test ./remotebridge/daemon/ -run 'Owner|Release|Claim|EnvTeardown'`.

### 3. Daemon wiring — `implement: sonnet` (after 2)

Files: `picker/remotebridge/daemon/daemon.go`,
`picker/remotebridge/daemon/daemon_test.go`.

- Run: pass `view: cfg.View` into `ownerCtl{...}`. Right after it, set
  `env := relayEnv{target: cfg.RemoteSession}` and, if `ownerOn`,
  `env = owner.relayEnv()`.
- `conn.go` `primeClient` also writes `ownerClaimCmd`, on the replacement
  connection. Leave it without a publish. It only renames our own record, it
  is not an ownership move, and its per-command reply count would break on an
  `if-shell` branch's extra block. `repair`'s `reconnected()` claim publishes.
- Startup publish: `sendCtl(env.publish(cfg.View)...)`. Repair publish: the same
  call. Rewrite both comments: still sent on every attach, now guarded by
  the owner record, so a non-owner's write does not land.
- `watchLocalClient`: replace the `remoteSession string` parameter with
  `env relayEnv`. Send `env.relay(id.Relay.String())` and `env.cell(id.Cell)`.
  The undo-on-failed-send logic is unchanged. A guarded write the remote skips
  still counts as sent: the view keeps the value, and the next claim publishes
  it. Update the doc comment. Update the call site to pass `env`.
- Teardown closure: replace `sendCtl(RelayEnvUnsetCmd(...), CellEnvUnsetCmd(...))`
  with `if ownerOn { owner.envTeardown() } else { sendCtl(<today's two unsets>) }`.
  Rewrite the comment above it. Keep the stale-sixel reason and the SIGTERM
  caveat. Add: never cleared while another controller is attached (#1035).
  Survivors replace a departed owner's values at release.
  The closure runs on Run's goroutine, both at the two early returns before the
  main loop and after the loop has returned. So the round-trip inside
  `envTeardown` has no concurrent reader. `owner` (daemon.go ~1057) is defined
  before the closure.
- `daemon_test.go`: the `watchLocalClient` helpers pass
  `relayEnv{target: "sess"}`, so the existing expectations stay byte-identical.
  Add one test with a guarded `relayEnv`: a capability change sends exactly
  `env.relay("sixel")`.
- Gate: `cd picker && go build ./... && go test -race ./remotebridge/daemon/...`.

### 4. Live tmux tests — `implement: opus` (harness design over a real control client; after 3)

File: `picker/remotebridge/daemon/relayenv_live_test.go` (new).

- Skip and require like `TestOpenURLRegisterAgainstLiveTmux`: skip without
  tmux, and fail under `OG_REQUIRE_TMUX`. Use
  `startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+privateDir(t))` and a session `w`.
- Each controller is a production `ctlConn` over a live `tmux -C attach-session -t w`
  process. Wrap its stdin/stdout as an `io.ReadWriteCloser`, then call
  `newCtlConn`, `bind(NewRouter())` and give it a real reply timeout (e.g. 5s).
  Wrap it in an `ownerCtl{own: newSizeOwner(tok), sessID: <$id>, rt: c.rt,
  send: c.st.send, cv: newConverger(), view: <Viewing with its relay/cell>}`. Kill each process and close each conn
  in `t.Cleanup`. Read env values with the isolated `tmux show-environment -t w
  <VAR>` (out-of-band, not through the control clients). After a fire-and-forget
  `send`, synchronise with one cheap round-trip on the same conn before
  asserting.
- `TestRelayEnvTeardownKeepsOtherControllersValues` (acceptance 1): A is sixel
  `15x28`, B is no relay `9x18`. A `claim()`, then B `claim()`, which leaves the
  slot as B's. A `envTeardown()`: assert `OG_RELAY_GRAPHICS=` and
  `AEYE_CELL_PX=9x18` (B's) remain. Stop A's process and wait until
  `list-clients` shows one client. B `envTeardown()`: assert both are unset
  (`-OG_RELAY_GRAPHICS` / `-AEYE_CELL_PX`, or absent).
- `TestRelayEnvFollowsOwnerMixedCapability` (acceptance 2): A `claim()`, so the
  slot is `sixel` and `15x28`. B sends `b.relayEnv().publish(b.view)` (non-owner) and
  the slot is unchanged. B `claim()`: the slot is relay empty and `9x18`. A sends
  `a.relayEnv().publish(a.view)` and the slot is unchanged (still B's).
- `TestRelayEnvReleaseCarriesSurvivorValues` (the SIGTERM path): A `claim()`.
  Kill A's process, with no teardown, and wait for one client. B runs
  `b.own.holdRelease(<A's token>, <A's client name from A's read>, past)` then
  `b.releaseDue(now)`. Assert the record is unset and the slot is B's values.
- Red evidence (record in the step report, not committed): in a scratch copy of
  the package (`cp -r` to the scratchpad), replace `envTeardown`'s body with
  main's `o.send(RelayEnvUnsetCmd(o.sessID), CellEnvUnsetCmd(o.sessID))`. Run
  `go test -run TestRelayEnvTeardownKeepsOtherControllersValues` there and
  capture the failing assertion. Run the same test in the worktree and capture
  the pass.
- Gate: `cd picker && go test -race -run 'RelayEnv' -count=3 ./remotebridge/daemon/`.

### 5. Docs — `implement: sonnet` (after 3; ∥ 4)

Files: `docs/agents/bridge-graphics-paste.md`, `docs/agents/bridge-daemon.md`,
`docs/agents/bridge-shipped-state.md` (only if it describes the publish/unset;
`rg -n 'OG_RELAY|AEYE_CELL|set-environment'` first).

- `bridge-graphics-paste.md`, the "capability is published" and "local cell
  size" bullets: the slot follows the size owner, writes are owner-guarded
  `if-shell`, a claim and a survivor's release republish, and teardown unsets
  only with no other control client attached. Describe the mixed-capability
  outcome and the carousel-after-switch edge in one sentence each, and link the
  spec.
- `bridge-daemon.md` "Size ownership across controllers": one bullet saying the
  claim and release also carry the env publish (#1035). Mention the
  `ownerReleaseCmd` branch extension in the release bullet. Add the mixed-version
  edge: an older daemon still writes unguarded and unsets on clean exit.
- Gate: `nix build .#lint --accept-flake-config` (typos/markdown hooks).

### 6. Full gate (lead)

From the worktree root: `nix build .#default`, `nix flake check`,
`nix build .#lint --accept-flake-config`. Then rebase on `origin/main` before
`pr_open`, and rerun `cd picker && go build ./... && go test -race ./remotebridge/daemon/...`.
A textually clean merge in this package has broken the build before.

## Acceptance mapping

| acceptance | covered by |
| --- | --- |
| two control clients, first teardown keeps second's values; red on main, green on branch | step 4 `TestRelayEnvTeardownKeepsOtherControllersValues` + scratch red |
| mixed-capability test under the chosen policy | step 4 `TestRelayEnvFollowsOwnerMixedCapability` |
| survivor republishes when ownership moves | step 2 unit + step 4 release test |
| `go test -race ./remotebridge/daemon/...` | steps 3, 6 |
| lint, flake check, build | step 6 |
| docs updated | step 5 |
