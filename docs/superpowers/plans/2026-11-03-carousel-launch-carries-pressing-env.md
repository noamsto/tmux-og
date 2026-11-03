# Carousel launch carries the pressing controller's relay env (#1071)

## Consumer map (why env on the launched process is not enough)

`daemon ctl carousel verb` → remote `run-shell -b` (session env) → `exec env … "$bin"`
= the aeye toggle (`tmux-claude-images`, aeye rev `beb98deb`) → `tmux new-pane` /
`split-window … -e AEYE_BRIDGED -e AEYE_CELL_PX "$cmd"` → the aeye viewer pane →
`os.Getenv("OG_RELAY_GRAPHICS")` / `AEYE_CELL_PX`.

The viewer is a *new pane*: its env is the remote **session** env plus the toggle's
explicit `-e` list, which carries `AEYE_BRIDGED` and non-empty `AEYE_CELL_PX` but
**never `OG_RELAY_GRAPHICS`**. So prefixing the toggle's `exec env` (the task's
suggested example) cannot reach the viewer's relay variable. The only producer that
every consumer sees is the session slot itself.

## Approach (design twice)

- **A (rejected): `NAME=value` words on the toggle's `exec env`.** Verified above to
  miss `OG_RELAY_GRAPHICS`; would also need a cross-repo aeye change to forward it.
- **A′ (rejected): aeye toggle forwards `OG_RELAY_GRAPHICS` via `-e`.** Cross-repo,
  needs an aeye release + flake-input bump, and still requires the remote to have it.
- **B (chosen): the press publishes the pressing daemon's own values into the
  session slot *in the same ordered control-mode batch*, immediately before the
  `run-shell`, and queues the owner claim.** Commands on one control connection run
  in order, so the launched toggle (and the viewer pane it opens) inherits B's values.
  - The slot write is **unguarded** on purpose: `relayEnv.guard` skips a write while
    the record names another owner, which is exactly the lag window being fixed. It
    does not change record ownership. The press carries the same activity evidence a
    claim acts on: each controller has its own daemon and its own `Viewing`, and
    `prefix + I` was typed on B's terminal.
  - The values go through the existing helpers `RelayEnvCmd` / `CellEnvCmd`
    (`tmuxQuote` on target and value), never raw interpolation. Relay is the closed set
    `graphics.Relay.String()` (`""`/`"sixel"`); the cell is written only if it passes
    `CellPx(w, h) == cell` (split on the first `x`, so `1x2x3` fails).
  - Unknown/invalid local cell → **no cell write** (inherit the session value, today's
    behaviour; never an empty export). The relay is always known (zero `Relay` = no
    capability = the value the slot publishes as `""`), so a non-sixel B always
    overwrites A's `sixel`.
  - The queued claim is `sizeOwner`'s existing claim-wanted flag (what `noteActivity`
    sets); the main loop's `claimIfStale` then moves the record, flags the other
    controllers and republishes (same values, guarded) on its next pass. It is
    largely redundant — the `prefix + I` keystroke advances `client_activity`, so
    `noteActivity` queues the same claim at the next reveal tick — but it costs one
    flag set per carousel press (a rare gesture, nothing per keystroke) and moves the
    record up to a tick sooner, so A's guarded publishes stop landing sooner.
- Residual edges (documented, not fixed): an already-running aeye keeps launch-time
  env; a B with an unknown cell size still inherits the previous owner's cell until
  the claim lands (the task's "unknown → inherit" rule); a late A publish (A's view
  change, A's `repair()`, or an A activity claim queued just before the switch) can
  rewrite the slot between B's write and the toggle's launch — rare, never periodic.

## File list

- `picker/remotebridge/daemon/relayenv.go` — add `pressEnvCmds(target string, view *Viewing) []string`: unguarded relay write + cell write (only when `CellPx`-valid), built on `relayEnv{target}` / `CellEnvCmd`; nil view → nil.
- `picker/remotebridge/daemon/ctl.go` — `verb` gains `publishesEnv bool` (set on `carousel`); `ctlState` gains plain fields `view *Viewing`, `relayTarget string`, `claimNow func()` (written once before `go acceptConns`, read-only afterwards, so no lock); `parseCtl` prepends `pressEnvCmds(c.relayTarget, c.view)` to `cmds` for such verbs and sets `ctlRequest.queueClaim`; `handleCtl` is the single `claimNow` call site: after a successful `submit`, next to `probe.arm`, `if req.queueClaim && cst.claimNow != nil { cst.claimNow() }`. No change to `carouselResolveScript` or the verb's `build`.
- `picker/remotebridge/daemon/sizeowner.go` — `(*sizeOwner).requestClaim()`: sets `claimWanted` and wakes the loop (the tail `noteActivity` already runs after `mark.advance`, extracted so both share it).
- `picker/remotebridge/daemon/daemon.go` — immediately after `cst := newCtlState()` (`runMirror`, :1185, **before `go acceptConns`** at :1227): `cst.view = cfg.View`; `cst.relayTarget = env.target` (the `relayEnv` built at :1060 is already `pin.id` when `ownerOn`, else `cfg.RemoteSession`; `pin` is fixed for the run, so no per-connection write); `if ownerOn { cst.claimNow = cfg.own.requestClaim }`. Nothing is written from `runConn`.
- `picker/remotebridge/daemon/relayenv_test.go` (existing relay env unit tests) — unit tests for `pressEnvCmds`.
- `picker/remotebridge/daemon/ctl_test.go` — `parseCtl`/`handleCtl` tests: carousel batch order and quoting, no-view batch unchanged, claim queued once and only for the carousel verb.
- `picker/remotebridge/daemon/relayenv_live_test.go` — two-controller live test.
- `docs/agents/bridge-graphics-paste.md` — replace "The carousel launch needs no change…" and the "Edge, accepted…" sentence.
- `docs/superpowers/specs/2026-11-02-relay-env-follows-owner-design.md` — consumer-map row `consumer: carousel` (~line 146) and the "Carousel opened right after switching controllers" edge (~183).
- `docs/superpowers/plans/2026-11-03-carousel-launch-carries-pressing-env.md` — this file.

## Steps

- [ ] **Step 1: failing live test.** In `relayenv_live_test.go` add `TestCarouselPressCarriesPressingControllerEnv`. Setup: `requireLiveTmux(t)` first (the test builds its server with `startIsolatedTmux` directly, so it does not get the skip from `startLiveEnvSlot`); a stub dir with an executable `tmux-claude-images` that (`--resolve`) prints `MODE\tKEY\t<manifest>` for a non-empty manifest file, and otherwise mimics aeye's toggle: `tmux split-window -d -t "$TMUX_PANE" -e AEYE_BRIDGED="$AEYE_BRIDGED" [-e AEYE_CELL_PX="$AEYE_CELL_PX" when non-empty] "$viewer"` where `$viewer` is a script appending `"$OG_RELAY_GRAPHICS|$AEYE_CELL_PX"` to a log file (so the test observes the env of the pane the toggle opens; the stub, like aeye, never forwards `OG_RELAY_GRAPHICS`). Pane id from `tmux display-message -p -t w '#{pane_id}'`, as `TestCarouselResolveScriptManifestCheck` does. `tmux := startIsolatedTmux(t, "PATH="+stub+":"+os.Getenv("PATH"), "CLAUDE_STATUS_DIR="+privateDir(t))`; `slot := liveEnvSlot{t: t, tmux: tmux}`; controllers A (`sixel=true`, `15x28`) and B (`sixel=false`, `9x18`) via `startLiveController(t, tmux, slot.sessionID(), …)`; `a.claim()`; `slot.assertValues("after A claims", "sixel", "15x28")`; B does **not** claim. Press through the production entry point: `cst := newCtlStateWith("@1", pane)`, `cst.view = b.ctl.view`, `cst.relayTarget = sessID`, `cst.claimNow = func(){}`; `rep, _, _ := replacerFixture("foot", true)` resolving to the advertised terminal (as `handlerFixture` does, `ctl_test.go:2112`) so `raise()` returns none; `err := handleCtl(cst, rep, newCarouselProbe(), []string{wire.CtlProtocolVersion, "carousel", pane}, "w", nil, b.ctl.send)`; `b.sync()`; poll the log up to 3s; assert it contains `|9x18` (empty relay, B's cell) and not `sixel`. Command: `cd picker && go test -race -run TestCarouselPressCarriesPressingControllerEnv ./remotebridge/daemon/`. Expected after Step 3: PASS (it first compiles after Step 3 adds the `ctlState` fields).
- [ ] **Step 2: unit tests (written first, fail to compile until Step 3).** `TestPressEnvCmds` (table over the returned slice): sixel+`15x28` → `[set-environment -t 'S' OG_RELAY_GRAPHICS 'sixel', set-environment -t 'S' AEYE_CELL_PX '15x28']`; non-sixel+`9x18` → relay `''`, cell `'9x18'`; cell `""`, `0x28`, `15x`, `513x1`, `1x2x3`, `15x28;x`, `$(id)x1`, `a'b` → relay command only, no `AEYE_CELL_PX`; nil view → nil; target containing `'`, a space and `#(touch x)` → emitted target equals `tmuxQuote(target)` (single quotes `'\''`-escaped). `TestParseCtlCarouselPublishesPressEnvFirst`: with a view set, the leading commands are the env writes and the last command is exactly the no-view `run-shell` command; with `cst.view == nil` the batch equals the no-view command; a non-carousel verb's batch is unchanged. `TestHandleCtlCarouselQueuesClaim`: `claimNow` is called exactly once after a successful carousel send, and not for a non-carousel verb (e.g. `layout` with a valid preset name such as `tiled`, since it takes one argument), the viewer-mismatch nack, or a failed send. Command: `cd picker && go test -race -run 'TestPressEnvCmds|TestParseCtlCarousel|TestHandleCtlCarouselQueuesClaim' ./remotebridge/daemon/`. Expected after Step 3: PASS.
- [ ] **Step 3: implement.** Per the file list. Comment the why at the call site (unguarded because the guard is what lags; before the `run-shell` because the toggle's `-e` list drops `OG_RELAY_GRAPHICS`). Commands: `cd picker && go build ./... && go test -race ./remotebridge/daemon/...`. Expected: everything passes, including Steps 1 and 2.
- [ ] **Step 4: docs.** Update `docs/agents/bridge-graphics-paste.md` and the #1035 spec as listed, including the residual edges. Command: `rg -n 'Edge, accepted|needs no change' docs/agents/bridge-graphics-paste.md` → no match.
- [ ] **Step 5: red-on-main proof, in an isolated copy.** Commit the implementation, tests and docs (a normal conventional commit), then `git worktree add <scratchpad>/redproof HEAD` (a detached copy that carries the new test, fields and `pressEnvCmds`). In the copy only: make `pressEnvCmds` return nil, run the Step 1 command, and record the command, that one-line patch and the failing log line (expected `sixel|15x28`, A's values). `git worktree remove` the copy. Never revert the live worktree. Expected: FAIL in the copy, PASS on the real branch.
- [ ] **Step 6: gates.** `cd picker && go test -race ./remotebridge/daemon/...`; then repo root `nix build .#lint --accept-flake-config`, `nix flake check`, `nix build .#default`. Expected: all succeed.

## Acceptance

- [ ] Two-controller live test: B's values (not A's) seen by the pane the toggle opens, before B's claim lands; red on main, green on branch → Steps 1 and 5.
- [ ] Metacharacter values can't reach the `set-environment`/`run-shell` string unquoted → `TestPressEnvCmds` rows (target and cell), Step 2.
- [ ] `go test -race ./remotebridge/daemon/...` from `picker/` → Steps 3, 6.
- [ ] `nix build .#lint --accept-flake-config`, `nix flake check`, `nix build .#default` → Step 6; CI green after PR.
- [ ] Unknown/invalid cell → no cell write, never an empty export → `TestPressEnvCmds` rows.
- [ ] Cheap claim: one flag set per press, none per key → Step 3 (`requestClaim`), asserted by `TestHandleCtlCarouselQueuesClaim`.
- [ ] Docs + accepted-edge note updated → Step 4.
