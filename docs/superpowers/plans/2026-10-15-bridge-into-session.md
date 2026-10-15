# Plan: bridge `--into` (#985)

Spec: `docs/superpowers/specs/2026-10-15-bridge-into-session-design.md`.
The component split, ordering and interfaces follow the task's decomposition.
Every step below maps to exactly one component (C1–C9), and the interface
names below are binding for every step. Daemon paths are under
`picker/remotebridge/daemon/`.

## Interfaces (binding)

- **Daemon flag/env**:
  - `--into <session-id>` / `OG_DAEMON_INTO`. The value must match
    `^\$[0-9]+$`, otherwise the daemon exits 1 before any dial.
  - `--local-sess` stays the host session **name**.
- **Config**:
  - `Placed bool`;
  - `LocalTarget string` (dedicated: empty, which means `LocalSess`; placed:
    `$N`);
  - `placeholder string`;
  - `opts *mirrorOpts`;
  - `func (c Config) target() string`, which returns `LocalTarget` when set,
    else `LocalSess`.
- **`placed.go`**:
  - `placedOption = "@bridge_placed"`;
  - `hostWindowsFormat = "#{window_id}|#{@bridge_placed}|#{@bridge_sock}"`;
  - `type hostWindow struct{ id, sock string; placed bool }`;
  - `parseHostWindows(out string) []hostWindow` (pure; `SplitN(line, "|", 3)`);
  - `hostWindows(cfg Config) ([]hostWindow, bool)`;
  - `ownWindows(cfg Config) ([]string, bool)`, the windows whose sock equals
    `cfg.SockPath`;
  - `hostCurrentIsOwn(cfg Config) bool`: dedicated returns true; placed reads
    `display-message -p -t $N '#{@bridge_sock}'` and compares it to
    `SockPath`.
- **Window stamp order** (both modes, before `@bridge_win`): `@bridge_sock`,
  `@bridge_host`, `@bridge_session`, then `@bridge_placed 1` in placed mode
  only, then `@bridge_win 1`. The rebuild placeholder gets `@bridge_sock` and
  `@bridge_placed` only.
- **`mirrorOpts`** (`mirroropts.go`):
  - `newMirrorOpts()`;
  - `set(cfg, opt, val)`, `unset(cfg, opt)`, `applyTo(cfg, localWin)`;
  - a nil receiver, or `!cfg.Placed`, keeps today's session write;
  - `nudgePathFor(cfg) string`: dedicated is `SockPath+resizeNudgeSuffix`,
    placed is `dir(SockPath)/og-nudge-<serverPID>-<N>.resize`.
- **Teardown**:
  - `type serverReplaced struct{ placeholder string }`, with
    `errors.Is(err, errServerReplaced)` still true for it;
  - `killOwnWindows(cfg Config)`;
  - `rebuildPlaceholder(cfg Config) (string, bool)`.
- **Shell**:
  - `og-remote-detach <session-name> [<window-id>]`;
  - the bind is `run-shell -b "<detach> #{qs:session_name} #{q:window_id}"`;
  - the session-reader guard is `#{?#{@bridge_placed},,#{@bridge_X}}`;
  - the session-menu gate is
    `#{&&:#{&&:#{@bridge_win},#{@bridge_pane}},#{!:#{@bridge_placed}}}`;
  - the placed socket is `${sock_dir}/og-into-<cksum>.sock`, with the lock
    directory `${sock}.lock/owner`.
- **Crew fields**:
  - `#{s/[|]/ /:@crew_branch}|#{s/[|]/ /:@crew_id}`, inserted before
    `#{@window_label_id}`;
  - `windowLabelFields = 24`.

## Files

| file | purpose | comp |
|---|---|---|
| `picker/remotebridge/daemon/daemon.go` | Config fields, `target()`, window create/stamp/placeholder, session-stamp gate (C1); `registerResizeNudge`/`unregister`/nudge path, `opts` init, session-path stamp (C2); `localActiveWindow`, `resolveView`, reveal wiring, select gates (C3); teardown ending + `serverReplaced` return (C4) | C1–C4 |
| `picker/remotebridge/daemon/placed.go` (new) | own-window helpers | C1, C3 |
| `picker/remotebridge/daemon/placed_test.go` (new) | helper units | C1, C3 |
| `picker/remotebridge/daemon/ownership.go` | placed pin by id | C1 |
| `picker/remotebridge/daemon/reopen.go` | `Run` placed pin fatal (C1); `runLoop`/`endReopen` placed (C4) | C1, C4 |
| `picker/remotebridge/daemon/localpanes.go` | `localWindowSet`/`localSessionGone` target | C1 |
| `picker/remotebridge/daemon/reconcilewindows.go` | `mirrorPaneRows` target (C1); select gate (C3) | C1, C3 |
| `picker/remotebridge/daemon/paste.go` | `notifyLocal` target (C1); `bridgeProc` own-sock filter (C3) | C1, C3 |
| `picker/remotebridge/daemon/mirroropts.go` (new) + `_test.go` | placed values | C2 |
| `picker/remotebridge/daemon/conn.go` | state/res/usage writers via opts | C2 |
| `picker/remotebridge/daemon/agentusage.go`, `sessionres.go` | flush via opts / placed skip | C2 |
| `picker/remotebridge/daemon/reveal.go`, `park.go`, `sizeowner.go` (struct only) | activity filter, placed viewing | C3 |
| `picker/remotebridge/cmd/daemon/main.go` | flag + Config literal (C1); `localArea`/`clientArea`/`sessionWinSize`/`localPaneMap`/reflow/seedView target + own-sock (C3) | C1, C3 |
| `picker/remotebridge/daemon/placedteardown.go` (new) + `_test.go` | `killOwnWindows`, `rebuildPlaceholder` | C4 |
| `picker/remotebridge/daemon/windowlabels.go` + `_test.go` | crew branch/id | C5 |
| `scripts/og-remote-open.sh` | `--into` + pair-lookup guard | C6 |
| `tests/remote-cold-start.bats`, `tests/mirror-session-name-integration.bats` | launcher cases | C6 |
| `scripts/og-remote-detach.sh`, `config/tmux.conf.tmpl`, `config/tmux.conf.reference.nix`, `generator/render/menus.go`, `generator/render/keys.go` + tests | detach + binds | C7a |
| `picker/main.go`, `picker/remote.go`, `picker/remote_resources.go`, `picker/remotebridge/cmd/ctl/main.go`, `scripts/og-remote-theme.sh` + tests | outside readers | C7b |
| `tests/remote-m2-integration.bats` | AC2/AC3 placed suite | C8 |
| `docs/agents/bridge-daemon.md`, `docs/agents/bridge-shipped-state.md`, `docs/agents/scripts.md` | AC5 | C9 |
| this plan + the spec | AC1 | — |

Deterministic commands used below:

- `GO`: `cd picker && go test -race ./remotebridge/... ./...` scoped as
  stated per step;
- `LINTGO`: `cd picker && golangci-lint run ./...` (the whole module, since
  scoped runs can lie);
- `SC`: `shellcheck <file>`;
- `BATS <file>`: `bats <file>` from the devShell. The m2 suite builds the
  daemon/renderer/ctl/statusline itself when `DAEMON` etc. are unset.

## Steps

### C1 — daemon placed core

- [ ] **Step 1: placed helpers, test first.** Create `placed_test.go` with
  table tests for `parseHostWindows`:
  - a row whose sock holds `|` stays whole;
  - an empty `placed` field → `false`;
  - a blank line is skipped.

  Also add `ownWindows` over a fake `LocalTmuxOut` returning three rows: two
  with our sock (one placed, one not) and one with another sock → our two ids.
  `ok=false` on an exec error. Then create `placed.go` to the interface.
  Proof: `cd picker && go test ./remotebridge/daemon -run 'HostWindows|OwnWindows'` → PASS (first FAIL before `placed.go`).
- [ ] **Step 2: Config fields and `target()`; mechanical target swap.** Add
  `Placed`, `LocalTarget` and `placeholder` plus `target()` to `daemon.go`.
  `opts` is not added here: C2 adds it in Step 8, together with
  `mirroropts.go`. Replace `-t cfg.LocalSess` with `-t cfg.target()`
  in:
  - `createMirrorWindow` (`cfg.target()+":{end}"`) and `firstMirrorWindow`;
  - `localWindowSet` (`localpanes.go:86`);
  - `mirrorPaneRows` (`reconcilewindows.go:149`);
  - `notifyLocal` (`paste.go:570`).

  `localSessionGone` placed: `has-session -t <LocalTarget>` (no `=`);
  dedicated keeps `"="+LocalSess`. Proof: `cd picker && go test -race ./remotebridge/daemon` → PASS unchanged (dedicated argv byte-identical: `target()` returns `LocalSess`).
- [ ] **Step 3: placed pin, test first.** In `ownership_test.go`, add a case:
  with `Placed` and `LocalTarget "$4"`, a `list-sessions` reply holding
  `123|$4|renamed` pins `{123,$4}`, while the name match is not used. A missing
  row → no pin. In `pinLocalSession`, match `f[1]==cfg.LocalTarget` when
  `cfg.Placed`. In `Run`, a placed config with no pin returns
  `fmt.Errorf("daemon: host session %s not found", cfg.LocalTarget)` before
  `runLoop`. Add a case to `daemon_test.go` (`reopen_test.go` belongs to C4):
  `Run` with a placed config and a failing pin returns an error, and its fake
  `Dial` is never called.
  Proof: `go test ./remotebridge/daemon -run 'Pin|Run'` → PASS.
- [ ] **Step 4: stamping and placeholder, test first.** In
  `startupwindows_test.go`/`setupwindow_test.go` (recording fake
  `LocalTmux`), add:
  - (a) dedicated: `stampMirrorWindow` issues `@bridge_sock`, `@bridge_host`,
    `@bridge_session` (`-w -t <win>`) before `@bridge_win`, and no
    `@bridge_placed`;
  - (b) placed: the same plus `@bridge_placed 1` before `@bridge_win`;
  - (c) placed `mirrorStartupWindows` with `cfg.placeholder==""` never runs
    `list-windows` for a placeholder and creates every window with
    `new-window … -t $4:{end}`;
  - (d) placed with `cfg.placeholder="@9"`: the first remote window claims
    `@9`.

  Implement:
  - `stampMirrorWindow` stamps the identity options in the interface order.
    One `LocalTmux` call joined with `;` is allowed, but keep each option a
    separate argv element.
  - `mirrorStartupWindows`: `placeholder = cfg.placeholder` when
    `cfg.Placed`, else `firstMirrorWindow`.
  - `dropUnclaimedPlaceholder` is unchanged (it acts on the returned
    placeholder).

  Proof: `go test ./remotebridge/daemon -run 'Stamp|Startup|Setup'` → PASS.
- [ ] **Step 4b: atomic create+identity, test first** (owner requirement).
  `startupwindows_test.go`: `createMirrorWindow` makes exactly **one**
  `LocalTmuxOut` call. Its argv is:

  ```
  new-window -d -P -F #{window_id} -a -t <T>:{end} ;
  set-option -w -t <T>:{end} @bridge_sock <S> ;
  set-option -w -t <T>:{end} @bridge_host <H> ;
  set-option -w -t <T>:{end} @bridge_session <R>
  ```

  Placed mode adds `; set-option -w -t <T>:{end} @bridge_placed 1`. The `;`
  is its own argv element. The returned id is still parsed from stdout.
  Implement this in `createMirrorWindow`. Proof: `go test ./remotebridge/daemon -run 'CreateMirror|Startup'` → PASS.
- [ ] **Step 5: session stamps gated; HandOff unaffected.** In `runMirror`,
  wrap the `@bridge_sock/_host/_session` session `set-option` block
  (`daemon.go:1144-1150`) in `if !cfg.Placed`. Add a test asserting that a
  placed run issues no `set-option -t <target> @bridge_sock` (session form).
  Proof: `go test ./remotebridge/daemon` → PASS.
- [ ] **Step 6: daemon flag.** In `cmd/daemon/main.go`, add
  `into := flag.String("into", os.Getenv("OG_DAEMON_INTO"), "…")`. Non-empty
  and not matching `^\$[0-9]+$` → exit 1 with a message. Set
  `Placed: *into != ""` and `LocalTarget: *into` in the Config literal. Under
  `--test-local`, pass it through too. With `--into` set, `--host` must match
  `^[A-Za-z0-9][A-Za-z0-9._@-]*$` (the placed-host rule, owner requirement),
  else exit 1. Add `main_test.go` cases for both validators (extract
  `validInto(string) bool` and `validPlacedHost(string) bool`). Proof:
  `go test ./remotebridge/cmd/daemon` → PASS.

### C5 — crew stamps (parallel with C1)

- [ ] **Step 7: test first.** In `windowlabels_test.go`, extend the row
  fixture to 24 fields with `@crew_branch`/`@crew_id` at positions 20/21:
  - valid `feat/985-x` / `1791968080-89470` → carried;
  - each of `a#b`, `x|y` (already spaced by the remote, so `x y`), `-x`, a
    256-rune branch and a 65-rune id → `""`.

  Assert that `bridgeLabelOptions` holds `@bridge_crew_branch` and
  `@bridge_crew_id`. Then edit:
  - `windowLabelFormat`: insert the two fields before `#{@window_label_id}`;
  - `windowLabelFields = 24`;
  - `labelRow` gains `crewBranch` and `crewID`;
  - parse with `matching(cleanLabelValueExact(at(20),255), crewBranchRe)` and
    `matching(cleanLabelValueExact(at(21),64), crewIDRe)`;
  - label id/rest move to 22/23;
  - two new `bridgeLabelOptions` entries;
  - `crewBranchRe = ^[A-Za-z0-9][A-Za-z0-9._/+-]*$`,
    `crewIDRe = ^[A-Za-z0-9._-]+$`.

  Proof: `go test ./remotebridge/daemon -run 'Label|Subscri|CtlSafe'` → PASS.

### C2 — placed values and nudge (after C1)

- [ ] **Step 8: `mirrorOpts`, test first.** Create `mirroropts_test.go`:
  - dedicated `set` → exactly `set-option -t host-sess @bridge_state x`
    (today's argv);
  - placed `set` → a `list-windows -t $4 -F <hostWindowsFormat>` read, then
    `set-option -w -t @2 @bridge_state x` for each own window only;
  - `unset` → the `-u` form;
  - `applyTo(@7)` stamps every recorded placed value, and is a no-op in
    dedicated mode;
  - a nil receiver behaves like dedicated mode.

  Implement `mirroropts.go` (`sync.Mutex` + map) and `nudgePathFor`, and add
  the `opts *mirrorOpts` field to `Config` (C2 owns both). Proof:
  `go test ./remotebridge/daemon -run MirrorOpts` → PASS.
- [ ] **Step 9: route the writers.**
  - `setBridgeState`/`clearBridgeState`/`clearBridgeUsage` (`conn.go`) call
    `cfg.opts.set/unset` (the `LocalSess==""` guard is kept).
  - `usageShipper.flush` writes through `cfg.opts.set`.
  - `clearBridgeRes` and `resShipper.flush` return early when `cfg.Placed`.
  - The `@bridge_session_path` stamp goes through `cfg.opts.set`.
  - `runMirror` sets `cfg.opts = newMirrorOpts()` before the first writer
    (before `clearBridgeState(cfg)`), and `stampMirrorWindow` ends with
    `cfg.opts.applyTo(cfg, localWin)`.

  Extend `agentusage_test.go`/`sessionres_test.go` with placed cases (usage
  window-scoped, res none). The existing dedicated assertions must pass
  unedited. Proof: `go test -race ./remotebridge/daemon` → PASS.
- [ ] **Step 10: shared nudge.**
  - `nudgePath := nudgePathFor(cfg)`; skip `os.Remove(nudgePath)` when
    `cfg.Placed`.
  - `registerResizeNudge` placed: no `set-hook -u`, and
    `set-option -t <target> @bridge_nudge <path>`.
  - `unregisterResizeNudge` placed: `hostWindows`; unset only if no window has
    `placed && sock != cfg.SockPath`; on a read error, leave it set.
  - Teardown's `_ = os.Remove(nudgePath)` is skipped when `cfg.Placed`.

  Unit tests:
  - placed register issues no `set-hook`;
  - unregister leaves the option when another placed sock is present, and
    unsets it when only ours or none is.

  Proof: `go test ./remotebridge/daemon -run Nudge` → PASS.

### C3 — view, size and activity follow the host session (after C1)

- [ ] **Step 11: activity filter, test first.** `reveal_test.go`: in placed
  mode, `activityRows` passed to `activity` include only rows whose window is
  a mirror. Add a `window` field to `activityRow`, and filter in
  `watchReveal`'s call site when the caller supplies a placed flag. Wire it:
  the `runMirror` reveal block passes `cfg.Placed`, and the `clientViewsArgs`
  target is `cfg.target()`. Proof: `go test ./remotebridge/daemon -run 'Reveal|Activity|SizeOwner'` → PASS.
- [ ] **Step 12: placed viewing, test first.** `park_test.go`:
  `placedViewing("…sockA\n\n…", sockA)` → true; only other socks or empty
  rows → false. `localViewing`: when `cfg.Placed`, run
  `list-clients -t <target> -F '#{@bridge_sock}'` and call
  `placedViewing(out, cfg.SockPath)`; dedicated is unchanged. Proof:
  `go test ./remotebridge/daemon -run 'Viewing|Focus|Park'` → PASS.
- [ ] **Step 13: select gates.**
  - Append `hostCurrentIsOwn` to `placed.go` (unit test with a fake reply).
  - The three `select-window` sites:
    - (1) `reconcilewindows.go:72`, the `added && activeRemote != ""` block:
      gated on `hostCurrentIsOwn(cfg)`;
    - (2) the `daemon.go` dispatch case for `SessionWindowChanged`
      (`daemon.go:~1421`): gated on `hostCurrentIsOwn(cfg)`;
    - (3) the startup `localWinForRemoteIndex` select (`daemon.go:~1309`):
      skipped entirely when `cfg.Placed`.
  - `localActiveWindow` and `resolveView` use `cfg.target()`.

  Tests:
  - `reconcilewindows_test.go`: a placed add with a reply naming another sock
    issues no `select-window`, and one naming our sock does;
  - dedicated is unchanged.

  Proof: `go test -race ./remotebridge/daemon` → PASS.
- [ ] **Step 14: `bridgeProc` own-sock.** `paste.go`: the format becomes
  `#{@bridge_pane}|#{@bridge_proc}|#{@bridge_sock}`, and only rows with our
  sock count, in both modes. Every mirror window now carries the window sock
  (C1), and a dedicated session's placeholder row has no `@bridge_pane`.
  Extend `paste_test.go` with a colliding `%3` from another sock that is
  ignored. Proof: `go test ./remotebridge/daemon -run 'Paste|BridgeProc'` → PASS.
- [ ] **Step 15: `cmd/daemon` main.go.**
  - The closures (`area`, `reflow`, `panes`, `seedView`) are built from flags
    **before** the Config literal (`main.go:351-407`). So compute
    `target := *localSess` and `if *into != "" { target = *into }` in `main()`
    first, and build every closure from `target` (`cfg.LocalTarget` does not
    exist yet there).
  - `localArea(localTmuxArgv, target, ownSock)` → `clientArea`/`readClients`
    target; `seedView` resolves `ResolveLocalViewIdentity(…, target)`.
  - `sessionWinSize(…, target, ownSock)`: an empty `ownSock` (dedicated)
    keeps today's `display-message`. Placed passes `*sock`: run `list-windows
    -t <target> -F '#{window_width} #{window_height}|#{@bridge_sock}'` (sock
    last, split on the first `|`, since the sock may hold `|`) and take the
    first row with our sock.
  - `localPaneMap(…, target, ownSock)`: the format becomes
    `#{@bridge_pane}|#{pane_id}|#{@bridge_sock}` (`SplitN` 3, sock last), and
    only rows with our sock count (both modes pass `*sock`).
  - Reflow in placed mode: `display-message -p -t <target> '#{session_name}'`
    per call, then `reflowRunShellArgs(bin, name)`.

  `main_test.go` covers each parser and the colliding-pane filter. Proof:
  `go test ./remotebridge/cmd/daemon` → PASS.

### C4 — placed teardown (after C1, C2)

- [ ] **Step 16: helpers, test first.** `placedteardown_test.go` (fake tmux):
  - `killOwnWindows`: pin check → owned → `hostWindows`, then `kill-window`
    for each own window only; not owned → no `kill-window`;
  - `rebuildPlaceholder`: issues `new-window -d -P -F '#{window_id}' -a -t
    $4:{end} -- sleep 2147483647` and, in the same `LocalTmuxOut` call
    (`;`-joined, targets `$4:{end}`), stamps `@bridge_sock`, `@bridge_host`,
    `@bridge_session` and `@bridge_placed` (no `@bridge_win`). It then kills
    every other own window and returns the id.

  Implement `placedteardown.go`. Proof: `go test ./remotebridge/daemon -run 'KillOwn|RebuildPlaceholder'` → PASS.
- [ ] **Step 17: teardown, `runLoop`, `endReopen`.** In the teardown
  closure's ending switch, when `cfg.Placed`:
  - `endReplaced` → `ph, _ := rebuildPlaceholder(cfg)`, stored for the return;
  - `endGone` → `notifyLocal(cfg, tombstoneText(...))` **first** (while a
    client is still on the session), then `killOwnWindows`;
  - default → `killOwnWindows`.

  `localSessionVanished` still returns early. `runMirror` returns
  `serverReplaced{placeholder: ph}`, whose `Is(errServerReplaced)` is true,
  where it returns `errServerReplaced` today. `runLoop` uses `errors.As` to set
  `cfg.placeholder`. `endReopen` placed: `notifyLocal` first when the
  tombstone text is non-empty, then `killOwnWindows`; never `kill-session`/
  `tombstoneMirror`.

  Tests (`reopen_test.go`):
  - a placed `runLoop` with a fake run returning `serverReplaced{"@9"}` passes
    `placeholder "@9"` to the next run;
  - placed `endReopen` issues no `kill-session`;
  - dedicated cases are unchanged.

  Proof: `go test -race ./remotebridge/daemon` → PASS.

### C6 — launcher (parallel with C1)

- [ ] **Step 18: test first (fake ssh/daemon harness).** Add cases to
  `tests/remote-cold-start.bats` (it already stubs ssh + daemon + tmux).
  These cases need their own per-test fakes, because today's fake daemon is
  `exit 0` and the fake ctl answers every ping:
  - **daemon fake**: appends one line per launch (with its
    `OG_DAEMON_INTO`/`OG_DAEMON_REMOTE_OPEN` env) to a launch log, sleeps
    `$FAKE_LISTEN_DELAY` (default 0.5s), writes `$$` to
    `${OG_DAEMON_SOCK}.pid`, touches `${OG_DAEMON_SOCK}.live`, then sleeps
    600 (case (g): exits 1 at once instead);
  - **ctl fake**: `ping` succeeds only while `<sock>.live` exists and the
    pidfile's pid is alive;
  - every case reaps the fake daemons it started (Rule 8: `trap` in the test
    plus kill of the pids in the launch log in `teardown`).

  Cases:
  - (a) `--into nosuch` → exit 1, `no local session`, and the ssh log is
    empty;
  - (b) host session carrying session `@bridge_host` → exit 1;
  - (c) `--into` + window arg, + `OG_REMOTE_NEW_DIR`, + `OG_REMOTE_RESTORE`
    → exit 1 each;
  - (d) first call: the fake daemon writes its pidfile and answers `ping`
    after 0.5s → exit 0, the daemon env has `OG_DAEMON_INTO=$N` and no
    `OG_DAEMON_REMOTE_OPEN`, and there is no `new-session`/`kill-session`/
    `switch-client` in the tmux log;
  - (e) second call while live → exit 0, no second daemon launch;
  - (f) two concurrent calls → exactly one daemon launched, both exit 0;
  - (g) the fake daemon exits immediately → exit 1;
  - (h) a stale lock dir with a dead `owner` pid is taken over;
  - (i) `--into` with host `bad|host` or `-x` → exit 1 before any ssh (the
    placed-host rule `^[A-Za-z0-9][A-Za-z0-9._@-]*$`).

  Proof: `bats tests/remote-cold-start.bats` → the new cases FAIL before
  Step 19.
- [ ] **Step 19: implement `--into`** in `scripts/og-remote-open.sh` per the
  spec's Launcher steps 1–9: argument parse (`$3 == --into`), the early
  exact-name `$N` resolve, the dedicated-host refusal (session-scoped
  `show-options`), the probe, the cksum key, the `mkdir` lock with `mv`
  stale-takeover and an `EXIT` trap, probe/reap, launch with the placed env,
  the wait-for-live, and an `on_signal` placed branch. The dedicated branch
  stays textually unchanged apart from Step 20. Proof:
  `bats tests/remote-cold-start.bats` → PASS; `shellcheck scripts/og-remote-open.sh` → clean.
- [ ] **Step 20: pair-lookup guard, test first.**
  `tests/mirror-session-name-integration.bats` (real inner server, fake
  daemon): create a session `work` holding a window stamped `-w @bridge_host
  h`, `@bridge_session s` and `@bridge_placed 1`, and make it current. Then
  `og-remote-open h s` must neither switch to nor kill `work`, and must create
  `h-s`. Also add the AC2 scratch-server idempotence case here:
  - pre-create a local session `dispatch`;
  - `og-remote-open h s --into dispatch` with a fake daemon that writes its
    pidfile and a fake ctl whose ping passes while the pid lives (the
    file's existing fakes extended the way Step 18 describes) → exit 0;
  - a second identical call → exit 0, the launch log holds one launch, and
    `dispatch` keeps its window count and id.

  In the launcher:
  - the `list-sessions -F` fields become
    `#{?#{@bridge_placed},,#{@bridge_host}}` and
    `#{?#{@bridge_placed},,#{@bridge_session}}`;
  - the verify uses `show-options -qv -t "$pair_id" @bridge_host` and
    `@bridge_session`;
  - `retire_mirror` reads `show-options -qv -t "$id" @bridge_sock`.

  Proof: `bats tests/mirror-session-name-integration.bats` → PASS (`TMUX_BIN`
  from `nix build .#default`).

### C7a — detach and binds (parallel)

- [ ] **Step 21: generator, test first.**
  - `keys_test.go`/`menus_test.go`: the `d` bind expects
    `#{qs:session_name} #{q:window_id}`.
  - Both StatusLeft entries expect the placed-excluding gate. Add a
    `bridgeGateNotPlaced` constant in `keys.go`. In `menus.go`, give the
    `menuMirror` entry struct an optional `gate` field: `menuBinds` uses
    `m.gate`, else `bridgeGate`. Set it only on the two StatusLeft entries.
  - Mirror both changes:
    - the `d` bind in `config/tmux.conf.tmpl:279` and
      `config/tmux.conf.reference.nix:876`;
    - in `config/tmux.conf.reference.nix`, a `gate` attr on the two
      `menuMirrors` StatusLeft entries, with `menuBindLine` (`:300-314`)
      emitting `'${m.gate or bridgeGate}'`.
    - The `:238` Detach item text is unchanged.

  Proof: `cd generator && go test ./...` → PASS; `nix build .#checks.$(nix eval --impure --raw --expr builtins.currentSystem).tmux-conf-extraction-assertions` → PASS.
- [ ] **Step 22: `og-remote-detach` placed path.** Optional `$2` window.
  - Read `#{@bridge_placed}|#{@bridge_sock}` at `-t "${2:-$sess:}"`.
  - Placed: SIGTERM the pidfile pid; poll `list-windows -a -F '#{@bridge_sock}'`
    for 2s until no line equals `$sock`; then `kill-window` every
    `list-windows -a -F '#{window_id}|#{@bridge_sock}'` row with our sock.
  - Never `kill-session`. Dedicated is unchanged.

  Proof: `shellcheck scripts/og-remote-detach.sh`; the behaviour is covered in
  Step 27.

### C7b — outside readers (parallel)

- [ ] **Step 23: picker snapshot + `collectBridge*`, test first.**
  - `main_test.go`: a snapshot row whose field 12 is `1` contributes no
    session-level `bridgeHost`/`bridgeRes`/`bridgeSessionPath`, while a
    later non-placed row of the same session still sets them (a dedicated
    session never has the flag).
  - `remote_test.go`: the `parseBridgeMirrors`/`collectBridgeSessions` format
    constant carries the guard.
  - Implement: `panesSnapshotArgv` appends `#{@bridge_placed}` as the last
    field, and `sessions()` skips flagged rows for those three fields.
  - Bump **both** field-count gates from 11 to 12: `sessions()` and
    `paneMap()` (`picker/main.go:1052`), since a stale count drops every pane.
    Add a `paneMap` test whose fixture rows have 12 fields.
  - Picker window-row test (owner requirement): in a mixed session, the
    placed window row (`bridgeHost "lab"`) renders the host cell
    (`tui.go:3613`) and the local row does not. Add it to the nearest
    existing window-row render test.
  - Add `bridgeSessionFormat` in `remote.go`, used by both collectors and by
    `bridgeSessionNames`.

  Proof: `cd picker && go test ./ -run 'Snapshot|Bridge|Session'` → PASS.
- [ ] **Step 24: `stopBridgeDaemon` every socket, test first.**
  `remote_kill_test.go`: two distinct socks across the session's panes → both
  are signalled once. Implement it with
  `list-panes -s -t <sess> -F '#{@bridge_sock}'`, deduped. Proof:
  `go test ./ -run 'Kill|StopBridge'` → PASS.
- [ ] **Step 25: ctl drag sock, test first.** `cmd/ctl/main_test.go`:
  `resolveDrag` returns the stashed pane's sock (the last field of
  `dragFormat`), and `main` dials it when it is non-empty. Proof:
  `go test ./remotebridge/cmd/ctl` → PASS.
- [ ] **Step 26: `og-remote-theme` per socket.** Replace the
  `list-sessions` loop with
  `list-panes -a -F '#{@bridge_sock}|#{@bridge_pane}'`: the first non-empty
  pane per distinct sock, one verb each. Split each row on the **last** `|`
  (`${row%|*}` / `${row##*|}`), since a sock path may hold `|` while a pane id
  never does. Extend `tests/remote-theme.bats`:
  two socks in one session → two verbs. Proof:
  `bats tests/remote-theme.bats`; `shellcheck scripts/og-remote-theme.sh`.

### C10 — host badge on placed tabs (owner requirement; parallel with C1)

This component is not in the decomposition: the dispatcher added the
requirement after the decomposition was written. It touches only the
status-bar render path, which no other component owns:
`scripts/tmux-reflow-windows.sh`, `config/tmux.conf.tmpl` `status-format[1]`,
`config/tmux.conf.reference.nix`'s mirror of it, `docs/agents/status-bar.md`,
`tests/reflow-fanout.bats` (where the crew-badge `@window_crew_disp` cases
live), and one new `flake.nix` check (`host-badge-conf-assertions`). Risk is **medium**: the reflow fit math is shared by every
session, and the grid clips on overflow. Read `docs/agents/status-bar.md`
first.

- [ ] **Step 26b: test first.** In the reflow bats suite (`tests/reflow-fanout.bats`, beside the crew-badge cases), set up a session with window A
  (local) and window B (`-w @bridge_win 1`, `@bridge_placed 1`,
  `@bridge_host lab`, `@bridge_label_id x`) and run
  `tmux-reflow-windows --force <sess> <width>`. Then:
  - (a) in grid mode, B's `@window_host_disp` is `<icon_remote> lab ` (the
    `@icon_remote` global), and A's is empty;
  - (b) B's charged floor includes it: at a width where A+B fit without the
    badge but not with it, the row goes to the next rung. Mirror how the crew
    badge case asserts this;
  - (c) a dedicated mirror window (`@bridge_win` 1, no `@bridge_placed`,
    `@bridge_host lab`) gets no badge;
  - (d) `@bridge_host` holding `|` on a non-placed window does not shift the
    row.

  Proof: the new cases FAIL before 26c.
- [ ] **Step 26c: implement.**
  - In `tmux-reflow-windows.sh`, `FMT` gains
    `#{?#{@bridge_placed},#{@bridge_host},}` in the token-safe region (before
    `#{window_name}`), with the matching `read -r` variable.
  - Build `win_host` and `win_host_dw` like `win_crew`, and charge
    `win_host_dw` wherever `win_crew_dw` is charged (floor, `total_long`,
    `total_short`).
  - Drop it alongside the crew badge when floors overflow
    (`win_host_disp`).
  - Stamp `@window_host_disp`, and render
    `HOST="#[fg=#{@thm_peach}#,bg=#{@thm_bg}]#{@window_host_disp}${BASE}"`
    in `ENTRY` before `${CREW}`, only when some window has a badge (the
    `crew_colw` pattern).
  - In `config/tmux.conf.tmpl:430` `status-format[1]`, insert after
    `#{window_index}: ` and before the crew conditional:
    `#{?#{@bridge_placed},#[fg=#{@thm_peach}#,bg=#{@thm_bg}]#{@icon_remote} #{@bridge_host} #{?window_active,#[fg=#{@thm_mauve}#,bg=#{@thm_bg}#,bold],#[fg=#{@thm_subtext_0}#,bg=#{@thm_bg}]},}`.
  - Mirror it byte-for-byte in `config/tmux.conf.reference.nix`.
  - Update `docs/agents/status-bar.md` (the badge, its charge and drop
    order).

    Also add a `host-badge-conf-assertions` flake check modelled on `crew-badge-conf-assertions` (`flake.nix:777`). It greps `tmuxConfig.tmuxConf` for the `#{?#{@bridge_placed},#[fg=#{@thm_peach}` fragment.

  Proof: `bats tests/reflow-fanout.bats` → PASS; `shellcheck scripts/tmux-reflow-windows.sh`; `nix build .#checks.$(nix eval --impure --raw --expr builtins.currentSystem).host-badge-conf-assertions` → PASS.
### C8 — placed integration (after C1–C4, C7a)

- [ ] **Step 27: `tests/remote-m2-integration.bats` placed block.** Add a
  `bridge_up_into <n> <src-sess> [--host h]` helper beside `bridge_up`. It
  launches `--test-local … --session <src-sess> --local-sess host --into $N
  --sock …` (`$N` from `$DST display-message -p -t host: '#{session_id}'`)
  against a DST session `host` that was pre-created with one local
  window (`@1`, running `sleep 600`, carrying `-w @marker keep`). Cases:
  1. remote windows appear in `host`, `@1` survives with its command and
     `@marker`, and each mirror window has `@bridge_win`/`_host`/`_session`/
     `_sock`/`_placed` window-scoped. The programmatic test holds:
     `show-options -wqv -t <win> @bridge_host` is non-empty on every placed
     window and empty on `@1`;
  2. a remote `new-window` → a new placed mirror window;
  3. a remote `kill-window` → its mirror is gone;
  4. a gated key: DST conf binds `F12` to
     `if -F '#{&&:#{@bridge_win},#{@bridge_pane}}' { run-shell "$CTL --sock=#{q:@bridge_sock} new-window #{q:@bridge_pane}" }`.
     A real client is attached to `host`: `obs_attach obs-k 120 40 "$DST"
     host` hosts it in OBS session `obs-k`, with a placed window current
     (`$DST select-window -t <placed id>`). `$OBS send-keys -t obs-k F12`
     reaches the client, so the key is the DST client's key → the SRC
     window count +1 and a new mirror;
  5. statusline: run `$STATUSLINE --session host` with a placed window
     current → the output contains the host label (`--host lab`);
  6. resize: `obs_attach` a 120x40 client to `host` → the SRC window and the
     mirror pane reach the client content area (minus the status row);
  7. SIGTERM → every placed window gone, while `host` and `@1` remain;
  8. two daemons (SRC sessions `ra`/`rb`) into `host`: each window's
     `@bridge_sock` differs, a ctl `new-window` through each sock grows only
     its own SRC session, and killing daemon A leaves B's windows and `@1`;
  9. a host made only of placed windows (`@1` killed after the mirror is
     up): SIGTERM → `host` is gone (tmux ended it with its last window). That
     no `kill-session` is issued is pinned at unit level by Step 17's
     placed-teardown argv assertions, so this case asserts only the observable
     ending and that a second, unrelated DST session survives;
  10. placed detach: `$DETACH host <placed-window-id>` → that daemon's windows
      are gone, `host` and `@1` remain, and the daemon pid has exited.

  Proof: `bats tests/remote-m2-integration.bats -f 'into'` → PASS; the full
  file → PASS (AC4: no existing `@test` body edited).

### C9 — docs (after C2–C4)

- [ ] **Step 28: docs.**
  - `docs/agents/bridge-daemon.md`: replace :55-57 with the new invariant
    (one daemon ↔ one remote session ↔ one host session; dedicated vs placed;
    the `@bridge_placed` reader rule). Update the `@bridge_state`,
    `@bridge_nudge`, `@bridge_host` and `@bridge_session_path` bullets and the
    teardown/rebuild/tombstone/local-session-gone text for placed mode.
  - `docs/agents/bridge-shipped-state.md`: placed window-scoped values (usage,
    state, session path), `@bridge_res` absent in placed mode, the "Stored per
    mirror session" qualifier, and `@bridge_crew_branch`/`@bridge_crew_id`.
  - `docs/agents/scripts.md`: rows for `og-remote-open`, `og-remote-detach`
    and `og-remote-theme`.

  Proof: `nix build .#lint` (typos, markdown hooks) → PASS.

### Gate

- [ ] **Step 29: full local gate (AC6).**
  - `nix build .#lint`, `nix flake check` and `nix build .#default`, all
    green.
  - The affected bats suites are already green per step; `nix flake check`
    reruns them in the sandbox.

## Acceptance mapping

- [ ] AC1: this plan + the spec committed → `git ls-files docs/superpowers/specs/2026-10-15-bridge-into-session-design.md docs/superpowers/plans/2026-10-15-bridge-into-session.md`. The spec's site table covers every task-listed site.
- [ ] AC2: Step 27 cases 1–7, plus Step 18 (e) for the second-call no-op → `bats tests/remote-m2-integration.bats -f into`, `bats tests/remote-cold-start.bats`.
- [ ] AC3: Step 27 case 8.
- [ ] AC4: existing suites unedited and green → `nix flake check` (remote-m2-integration-tests, remote-tests, remote-bridge-integration-tests).
- [ ] AC5: Step 28 diff.
- [ ] AC6: Step 29 commands; CI on the PR (lint, build x86_64-linux + aarch64-darwin).
- [ ] AC-owner (dispatcher-added, distinguishable placed windows): Step 4b (atomic stamp unit), Step 27 case 1 (window-scoped `@bridge_host` present on placed windows and absent on local ones), Step 26b/c (tab badge), and Step 23 (picker row).
