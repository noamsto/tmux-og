# Forward the dwindle toggle to the remote (#1040)

Mechanism: `prefix + A` in a mirror runs a new ctl verb `dwindle-toggle <pane>`. The
daemon sends the remote a clear + `if-shell` that flips the remote window's
`@dwindle` (or refuses on `@crew_grid=1`) and stamps a whitelisted verdict
(`dwindle-on|dwindle-off|dwindle-grid`) on the ctl pane in `@og_carousel`. The existing
`carouselProbe` (verb flag `probe: true`) reads it back and `notifyLocal`s the text,
because a remote `display-message` has no status line to land on. Nothing is ever
written to the local mirror window. `CtlProtocolVersion` bumps 8 -> 9 (required by
that constant's comment), so an old daemon answers a new ctl with the readable
`ctl protocol version "9", this daemon speaks "8" - reopen the bridge`, which ctl
shows via `--display-error`.

## File list

- `picker/remotebridge/daemon/ctl.go` — verb table entry `dwindle-toggle` + command builder.
- `picker/remotebridge/daemon/carouselprobe.go` — three verdict constants and their messages in `carouselVerdictMessage`.
- `picker/remotebridge/daemon/ctl_test.go` — parse (pins exact command text and `probePane`), arity, injection cases; a `parseCtl` row with argv[0]="8" asserting `this daemon speaks "9"`.
- `picker/remotebridge/daemon/carouselprobe_test.go` — drive `poll` with `dwindle-on|off|grid`; assert the notify text and the clear command.
- `picker/remotebridge/wire/protocol.go` — `CtlProtocolVersion` "8" -> "9".
- `config/tmux.conf.tmpl` and `config/tmux.conf.reference.nix` (frozen byte-identity oracle, must be edited together) — the mirror branch of the `A` bind only, same single line in each.
- `tests/remote-m2-integration.bats` — ctl `dwindle-toggle` flips the remote and refuses on a grid window (appended test).
- `README.md`, `docs/agents/bridge-daemon.md`, `docs/agents/bridge-shipped-state.md` — document the verb and the verdict.

## Steps

- [ ] **Step 1: failing parse tests** in `ctl_test.go`: `dwindle-toggle %3` parses to two cmds (clear, if-shell) targeting `%3`, no `moves`/`layout`, `probePane == "%3"`; extra arg rejected; unknown pane rejected; injection pane id (`%3; kill-server`) rejected by pane lookup; the built commands contain no raw user text. Run `cd picker && go test ./remotebridge/daemon/ -run Ctl` — expect FAIL (unknown verb).
- [ ] **Step 2: failing message test**: `carouselVerdictMessage("dwindle-on", host)` -> `dwindle auto-balance: on (remote window)`, `-off` likewise, `-grid` -> `dwindle: grid window - tmux-grid-refit owns this layout (remote window)`. Run the same command — FAIL.
- [ ] **Step 3: implement** in `ctl.go` (verb `{probe: true, build: ...}`) and `carouselprobe.go`, with the stamp `set -pF -t <pane> @og_carousel 'dwindle-#{@dwindle}'` after the flip, the flip expression `#{?#{==:#{@dwindle},on},off,on}`, `-t <pane>` on every command in the branch, `;` not `\;`. Bump the protocol version. Run `cd picker && go test -race ./remotebridge/... ` — expect PASS.
- [ ] **Step 4: bind**: replace the mirror branch with `run-shell "{{.BridgeCtl}} dwindle-toggle #{q:@bridge_pane}"` in the template and, with `${bridgeCtl}`, in `tmux.conf.reference.nix`. Verify with `nix flake check` (extraction assertions) and `nix build .#default`.
- [ ] **Step 5: bats** test (placed in the M2.3 ctl section, not EOF; no verdict-message assertion, DST has no client — that is covered in Go by Step 2's probe test) via the ctl binary against the daemon socket on scratch servers (`$SRC`/`$DST` with their own sockets, `TMUX` unset): press flips `@dwindle` on the remote window to `on`/`off`; with `@crew_grid=1` it stays unchanged, asserted after an ordering barrier (a following press on a non-grid pane has flipped); the local mirror window never gains `@dwindle`. Run via `nix flake check` (or the bats runner for that file).
- [ ] **Step 6: docs**: README keys line, the two agent docs.
- [ ] **Step 7: gate**: `nix build .#lint`, `nix flake check`, `nix build .#default`.

## Acceptance

- [ ] `cd picker && go test -race ./remotebridge/daemon/ -run Ctl` — Step 1-3.
- [ ] Mirror flips / grid refusal / old-daemon message — Step 5 bats plus the version-mismatch error path (a new `parseCtl` row for argv[0]="8").
- [ ] `nix build .#lint`, `nix flake check`, `nix build .#default` — Step 7.
- [ ] CI green, run id cited in the PR.
