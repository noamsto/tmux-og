# Plan: route mirrored URL opens to the active controller (#1034)

Spec: `docs/superpowers/specs/2026-10-30-route-url-opens-active-controller-design.md`
(Design C: tag each record with the size owner `@og_bridge_owner` when that
client is registered in the new `@og_open_clients` list; daemons open untagged
records and records tagged with their own client name).

Orchestration consult: not tripped — two components (`og-open.sh`,
`openurl.go`) with a contract the spec already fixes byte for byte; an opus
plan suffices.

## Fixed contract (both producers and every test use exactly this)

- Record: tagged `" <epoch>-<pid>@<client>|<url>"`, untagged
  `" <epoch>-<pid>|<url>"` (unchanged). Tag charset `^[A-Za-z0-9_-]{1,64}$`;
  nonce `^[0-9]+-[0-9]+$`; split the token at the first `|`, then the nonce
  part at the first `@`.
- Registry: session option `@og_open_clients`, entries appended as
  `" <client_name>"`; consumers split on whitespace.
- `og-open`'s `list-clients -t "$TMUX_PANE" -F` format, three `|` fields
  (registered, target, log length):
  `#{?#{&&:#{client_control_mode},#{||:#{m:* #{client_name} *, #{@og_open_clients} },#{==:#{client_name},#{@og_open_client}}}},1,0}|#{?#{&&:#{client_control_mode},#{&&:#{m:* #{client_name} *, #{@og_open_clients} },#{m:*|#{client_name},#{@og_bridge_owner}}}},#{client_name},}|#{n:@og_open_url}`
- Register commands (daemon, after a successful seed and subscribe; `<t>` is
  `urlOpener.target()`, i.e. `" -t '<sess>'"` or empty):
  1. one round-trip batch of two reads:
     `display-message -p<t> <tmuxQuote(ctlSafe("#{client_name}|#{@og_open_clients}"))>`
     and `list-clients -F <tmuxQuote(ctlSafe("#{client_name}"))>` (server-wide,
     no `-t`);
  2. one round-trip batch of writes: per stale name (valid charset, not self,
     not in the list-clients reply)
     `set-option -F<t> @og_open_clients <tmuxQuote("#{s/ <name>( |$)/ /:@og_open_clients}")>`;
     then, unless self is already a list entry,
     `set-option -a -F<t> @og_open_clients <tmuxQuote(" #{client_name}")>`;
     then the existing `set-option -F<t> @og_open_client '#{client_name}'`.
  Any lost or `%error` reply in either batch, or a self name failing the
  charset → `o.fail("register")` (one notice, existing gate). The register
  step clears `o.self` first and sets it only from a valid read.
- Builders shared by `connect`, the scripted tests and the live test:
  `openRegisterReadCmds(target string) []string` and
  `openRegisterWriteCmds(target, self string, listed, attached []string) []string`.

## Steps

### 1. Daemon: tag-aware records and list registration — `implement: sonnet`

Files: `picker/remotebridge/daemon/openurl.go`,
`picker/remotebridge/daemon/openurl_test.go`,
`picker/remotebridge/daemon/sizeowner.go` (comment only).

- `openRecord` gains `tag`; `parseOpenRecords` parses `<nonce>[@<tag>]|<url>`
  per the contract; a token whose nonce or tag fails its regex is skipped.
- `urlOpener` gains `self string` (this control client's name, set by the
  register read); `connect`'s register step becomes the two batches above.
  Keep the doc comments' voice; update the `connect` comment.
- `handle`: after the fresh/seen bookkeeping, a record whose tag is non-empty
  and `!= o.self` is skipped before `validOpenURL` and the rate limit (no
  token, no cap slot, no suppression count).
- `sizeowner.go`: one comment line at `bridgeOwnerOption` that
  `scripts/og-open.sh` also reads the record (glob `*|<client>`), so its
  format is a cross-language contract.
- Tests (red first where they assert new behaviour):
  - `TestParseOpenRecords`: rows for tagged, malformed tag (charset, empty
    after `@`, over 64), `@` in the URL part of an untagged record (the cut is
    at the first `|`, so it stays the URL).
  - `handle`: tagged-for-self opens; tagged-for-other is skipped, marked seen
    and spends no token (assert bucket untouched via a following burst);
    untagged opens; `self == ""` skips every tagged record.
  - Register: update `connectCmds`/`TestURLOpenerConnectOrder`/
    `TestURLOpenerConnectFailures`/`TestURLOpenerUnknownPinUntargeted` to the
    new command sequence: stale name pruned, live name kept, self not
    re-appended when present, invalid list entries ignored (not pruned —
    `s/` with them would be unsafe), invalid self → register fails with no
    writes, write-batch error → register notice.
  - Live tmux (extend `TestOpenURLCommandsAgainstLiveTmux` or a sibling,
    same `startIsolatedTmux`/`OG_REQUIRE_TMUX` pattern): run the real
    builders on two control clients of one session: both names end up in
    `@og_open_clients` and `@og_open_client` names the later one; after one
    client exits, the other's register prunes it. Anchoring, with synthetic
    stale names since live ones are all `client-<pid>`: seed the list with
    `<live name minus its last digit>` and `<live name>9`, once in the middle
    and once at the end; both are pruned and the live name survives.
- Gate: `cd picker && go test -race ./remotebridge/daemon/ -run 'URLOpener|OpenURL|ParseOpen|OpenClaim'`, then
  `go vet ./remotebridge/...`.

### 2. `og-open`: list/slot bridged check and owner tag — `implement: sonnet` (∥ step 1)

Files: `scripts/og-open.sh`, `tests/og-open.bats`.

- Replace the `list-clients` format with the contract's three-field one; read
  rows as `ok|target|n`: `ok=1` → bridged, `loglen=n`; non-empty `target` →
  `owner=$target`. Record is `" $(date +%s)-$$@$owner|$url"` when `owner` is
  set, else today's. Update the header comment. Validation, cap and reset are
  unchanged.
- `tests/og-open.bats`: stub rows move to three fields (`1||10` etc.); the
  exact-format assertion line follows the new format; new cases: tagged
  record when a row carries a target (regex `^set-option -a -t %3
  @og_open_url  [0-9]+-[0-9]+@client-7\|https://…$`), untagged when no row
  does, tagged on the replace-past-cap path too, bridged when only the
  second row is registered, not bridged when no row is.
- Gate: `bats tests/og-open.bats`; `shellcheck scripts/og-open.sh
  tests/og-open.bats`; `shfmt -d scripts/og-open.sh`.

### 3. Integration: two controllers, routing / survivor / untagged — `implement: sonnet` (after 1 and 2)

File: `tests/remote-m2-integration.bats` (near the #854 og-open case; reuse
the #979 helpers `two_controllers_up`, `type_until`, `size_daemon_up`,
`size_daemons_stop`).

- Each daemon is its own "machine": its own opener stub dir first on PATH and
  its own `TMPDIR` (separate claim dirs), passed as temp-env assignments on
  the `size_daemon_up` call (bash exports those to the function's children;
  measured). Keep the TMPDIRs short, under `$OG_TMUX_DIR` (the daemon puts
  `og-bridge-<pid>-<n>.sock` there; the unix-socket path limit is 108). Add a
  third "remote" stub dir first on PATH **before** `SRC` starts, so a
  remote-side open (the unbridged fallback on main) records into its own file
  instead of reaching a real browser; also prefix the remote command itself
  with a `PATH=<remote-stub>:$PATH` assignment in case the pane's shell
  resets PATH. `mkdir` each per-daemon TMPDIR first. `two_controllers_up` must therefore take
  the per-daemon env without changing its #979 callers (e.g. optional env
  variables it forwards, default empty).
- Case A — "og-open routes to the controller typed in last (#1034)": after
  `two_controllers_up`, `type_until obs-s "$small"` → `og-open` in a remote
  pane (`$SRC send-keys -t rem "bash '$OG_OPEN' https://example.invalid/s"
  Enter`) → wait for S's file, settle 2.5s, assert B's and the remote's files
  lack it. `type_until obs-b "$big"` → open `/b` → only B's file. Then an
  untagged record written directly
  (`$SRC set-option -a -t rem @og_open_url " 1-1|https://example.invalid/old"`)
  reaches both S's and B's files.
- Case B — "og-open still forwards after the registrant leaves (#1034)":
  `two_controllers_up` (B connects last, so B holds `@og_open_client`);
  assert that, then stop B's daemon (kill its pid, `wait`), wait until `SRC`
  lists one control client, run `og-open` → S's file gets it, the remote's
  file stays empty, and the raw `@og_open_url` shows that record untagged
  (the owner record still names B, so this pins the survivor path, not
  tagging).
- Each case dumps the three files and `@og_open_clients`/`@og_open_client`/
  `@og_bridge_owner` to fd 3 on failure, the way `size_failed` does.
- Red evidence: run both cases against `main`'s `og-open.sh` and daemon in an
  isolated scratch copy (`git worktree add` of `main` under the scratchpad,
  copy the new test file in), record the failing assertions; then green on
  the branch. Never revert the working tree.
- Gate: `bats -f '1034' tests/remote-m2-integration.bats` (DAEMON/RENDERER
  unset → the test builds them), then the #854 og-open case and the #979
  cases (`-f 'og-open|979'`) still pass.

### 4. Docs — `implement: sonnet` (after 1–3)

- `docs/agents/bridge-shipped-state.md` "URL Opens": record format with the
  optional `@<client>` tag; the registered-client check becomes list-or-slot
  with the target rule; connect step 3 rewritten (reads, prune, append, slot,
  `self`); `handle`'s tag filter and its place before the rate limit; the
  compatibility table; the accepted edges (claim lag, focus-in claims,
  suspended owner, pid reuse); per-machine claimer still dedups untagged
  records.
- `docs/agents/bridge-daemon.md`: the filtered-daemon "N tabs" sentence (only
  the owner daemon opens a tagged record now; untagged still N) and, in #979,
  that `og-open` reads the owner record.
- `docs/agents/scripts.md`: the `og-open` row's bridged check and the options
  it reads.
- Gate: `nix build .#lint` (typos, markdown hooks).

### 5. Full gate and delivery (lead)

`nix build .#default`, `nix flake check`, `nix build .#lint`, inside
`nix develop`. Commit spec, plan and code; code-review gate; push; PR; CI
green (`lint` + both `build` jobs), run id cited in the PR.

## Risks

- The live `#{m:...}`/`#{s/...}` behaviour is measured on next-3.9 only; on a
  3.2/3.3 remote a format it cannot evaluate renders empty: `ok` falls back to
  the slot, `target` empty → untagged (today's behaviour). The prune `s/`
  is older than 3.2.
- Changing `connect`'s replies count shifts `TestURLOpenerConnectFailures`'
  scripted reply indices — update, don't loosen.
