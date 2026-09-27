# Plan: bridge layout propagation (#787)

Notation: `GATE` = `#{&&:#{@bridge_win},#{@bridge_pane}}` (`bridgeGate`, keys.go).
`CTL` = `bridgeCtl(p)` (keys.go). `verb` = `og-remote-bridge-ctl`.

## Problem

tmux's stock layout bindings ran `select-layout` / `rotate-window` on the local
mirror only. That reshapes the renderer panes while the remote panes keep their
sizes, the programs in them render at the old ones, and the next remote
`%layout-change` reverts the local shape — the same failure mode `zoom` already
crosses the bridge to avoid. Every other structural gesture is `bridgeGate`'d
through `og-remote-bridge-ctl`; the layout keys were not.

## Stock bindings (measured on the pinned `next-3.9`)

| key | stock command |
|---|---|
| `M-1` | `select-layout even-horizontal` |
| `M-2` | `select-layout even-vertical` |
| `M-3` | `select-layout main-horizontal` |
| `M-4` | `select-layout main-vertical` |
| `M-5` | `select-layout tiled` |
| `M-6` | `select-layout main-horizontal-mirrored` |
| `M-7` | `select-layout main-vertical-mirrored` |
| `Space` | `next-layout` |
| `E` | `select-layout -E` |
| `C-o` | `rotate-window` |
| `M-o` | `rotate-window -D` |

Measured: `select-layout`, `next-layout`, `previous-layout`, `select-layout -E`
preserve the active pane id; `rotate-window` does not (`%2` → `%0`). Each
accepts a pane id (`%N`) as its `-t` target.

## File list

| File | Purpose |
|---|---|
| `picker/remotebridge/daemon/ctl.go` | `layout` (arg allow-list) and `rotate` (optional `U`/`D`) verbs, `layoutCommand` type + `layoutCommands`/`rotateDirs` maps |
| `picker/remotebridge/daemon/ctl_test.go` | Translation cases for each allowed arg, rejection cases, and `TestParseCtlRotateInvalidatesActiveBelief` |
| `config/tmux.conf.tmpl` | The 11 keys bound behind `{{.BridgeGate}}`, stock else-branch |
| `config/tmux.conf.reference.nix` | The same 11 binds (extraction oracle — two-file rule) |
| `flake.nix` | `layout-conf-assertions` (gated + stock-parity) |
| `tests/remote-m2-integration.bats` | `ctl layout reshapes the REMOTE window` M2.3 e2e |
| `docs/agents/bridge-daemon.md` | Layout bullet + verb documentation |
| this plan | Committed with the code (CLAUDE.md) |
| `REVIEW_NOTES.md` (untracked) | Finding ledger |

`tmux-grid-refit` is deliberately unchanged: its first guard exits unless
`@crew_grid=1`, which a `@bridge_win` mirror never has.

## Steps

- [x] **Step 1 (test-first): failing Go verb tests.**
      `cd picker && go test ./remotebridge/daemon/ -run 'TestParseCtl'` — red
      (`unknown verb "layout"`).
- [x] **Step 2: implement the verbs.** `layoutCommands` maps the wire arg to a
      `layoutCommand{verb,arg}` (pane spliced at build time, never free-form
      text); `layout` is `args:1, layout:true`; `rotate` is
      `optArgs:1, layout:true, moves:true`. Same command — green.
- [x] **Step 3 (test-first): conf assertion.** `layout-conf-assertions` greps
      each gated bind and its stock else-branch — red before Step 4.
- [x] **Step 4: bindings.** Template + reference, byte-identical modulo
      interpolation. `layout-conf-assertions` and
      `tmux-conf-extraction-assertions` green.
- [x] **Step 5 (test-first): e2e repro.** `bats -f "ctl layout reshapes"
      tests/remote-m2-integration.bats` — green.
- [x] **Step 6: document.** `docs/agents/bridge-daemon.md`.
- [ ] **Step 7: full gate.** `nix build .#default`, `nix flake check`,
      `nix build .#lint`.

## Acceptance checklist

- [x] Go tests: each allowed arg builds the right remote command; a bad arg is
      rejected.
- [x] Conf assertion: each layout binding is gated and its else-branch is the
      stock command verbatim.
- [x] E2E: `layout … even-vertical` on a 3-pane mirror changes the REMOTE
      `#{window_layout}` and the mirror follows (output in the PR body).
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` pass.

## Risks

- `rotate` carries `moves: true` because of the measured active-pane change; a
  missed invalidation would leave the focus echo guard stale.
- No non-constant `fmt.Sprintf`: the allow-list stores verb + arg, not a format
  string, so `go vet`'s printf check stays quiet.
