# Plan: mirror window bar stale after attach (#820)

Spec: `docs/superpowers/specs/2026-09-27-mirror-bar-stale-attach-820-design.md`.

## File list

| File | Purpose |
|---|---|
| `picker/remotebridge/daemon/daemon.go` | `registerResizeHook`/`unregisterResizeHook` → `registerResizeNudge`/`unregisterResizeNudge` (publish `@bridge_nudge`, clear the four pre-#820 session hooks); delete `resizeHookEvents`; comments and call sites. |
| `picker/remotebridge/daemon/park.go` | `focusEdge` comment: the wake rides the config's `client-session-changed` reflow; a switch away is not nudged. |
| `scripts/tmux-reflow-windows.sh` | Read `#{@bridge_nudge}` in the existing fast-path `display-message` and touch it after the empty-width bail, before the cache check. |
| `config/tmux.conf.tmpl` | Gated `window-resized[20]` and `client-detached` nudge hooks; `set-hook -gu client-detached` in the reload clear block. |
| `config/tmux.conf.reference.nix` | Byte-for-byte reference of the rendered conf (`tmux-conf-extraction-assertions`): mirror the two hooks, the clear line and their comments. |
| `tests/reflow-mirror-attach.bats` | New regression through the real hook path: switch-into and attach-into a mirror, each compared against a forced pass. |
| `flake.nix` | New `reflow-mirror-attach-tests` check (built wrapper + picker binaries, plus conf greps for the two hooks and the clear). |
| `tests/remote-m2-integration.bats` | `write_dst_conf` helper carrying the four gated touch hooks (vanilla server, no reflow script); the `show-hooks` resize gate → a non-empty `@bridge_nudge` gate. |
| `docs/agents/status-bar.md` | “Bridge resize nudge” section: reflow touches `@bridge_nudge` after the width bail, before the cache check. |
| `docs/agents/bridge-daemon.md` | `@bridge_nudge` bullet: what it is and why it is an option, not a session hook (#647/#820). |
| `docs/agents/bridge-graphics-paste.md` | The `watchLocalClient` nudge clause: session hooks → the config's hooks. |
| `docs/superpowers/specs/2026-09-27-mirror-bar-stale-attach-820-design.md`, this plan | Committed alongside, per `CLAUDE.md`. |

Nothing outside this list changes.

## Steps

- [x] **Step 1: failing regression test** — `tests/reflow-mirror-attach.bats`.
  `bridge_up` (4-window remote, mirror born detached, daemon `--test-local`
  with `--reflow`, splash hooks cleared, blank `status-format[0]` + `status
  off`) and `equals_forced_pass` (capture → clear `@reflow_key` → forced pass →
  poll for the re-stamp → capture → diff). Test A switches a client onto the
  mirror and polls for a reflow; Test B attaches directly.
  Red evidence pre-fix: `bats tests/reflow-mirror-attach.bats` → Test A “no
  reflow after switching into the mirror — the reported bug”, Test B green.
- [x] **Step 2: config carriers** — `config/tmux.conf.tmpl` +
  `config/tmux.conf.reference.nix` + `scripts/tmux-reflow-windows.sh` (spec,
  Fix). Proof: `nix build .#default`,
  `nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions`
  (the reference diff), `bash -n scripts/tmux-reflow-windows.sh`.
- [x] **Step 3: daemon publishes the option** — `daemon.go`, `park.go` (spec,
  Fix). Proof: `go build ./...`, `go vet ./remotebridge/...`, then the Step 1
  suite green.
- [x] **Step 4: m2 suite** — `write_dst_conf <remain-on-exit>` at the three
  write sites and the `@bridge_nudge` gate. Proof:
  `bats -f resize tests/remote-m2-integration.bats` (5/5),
  `-f 'respawned mirror pane'`, `-f 'killing a renderer process'`.
- [x] **Step 5: flake check** — `reflow-mirror-attach-tests` with the conf
  greps. Proof: `nix build
  .#checks.x86_64-linux.reflow-mirror-attach-tests -L` (2/2 + greps).
- [x] **Step 6: docs** — status-bar, bridge-daemon, bridge-graphics-paste.
- [x] **Step 7: committed plan + design spec.**
- [ ] **Step 8: red/green recorded on a scratch `origin/main` tree** (the bats
  file copied in, that tree's daemon/renderer/conf) → Test A fails; the branch
  tree passes.
- [ ] **Step 9: gate** — `nix build .#default`, `nix flake check`,
  `nix build .#lint`.

## Validation commands

```sh
nix build .#default
nix build .#lint
nix build .#checks.x86_64-linux.reflow-mirror-attach-tests -L
nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions -L
nix flake check -L
# local fast loop for the new bats file (branch wrapper + freshly built daemon)
go build -o /tmp/og820/daemon ./picker/remotebridge/cmd/daemon
go build -o /tmp/og820/renderer ./picker/remotebridge/cmd/renderer
TMUX_BIN="$PWD/result/bin/tmux" DAEMON=/tmp/og820/daemon RENDERER=/tmp/og820/renderer \
  bats tests/reflow-mirror-attach.bats
```