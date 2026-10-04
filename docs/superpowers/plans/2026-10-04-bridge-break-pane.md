# Route break-pane to the remote in a mirror window (#922)

## Problem

In a `@bridge_win` mirror window, `prefix + !` (tmux's stock `break-pane`) only
changes the local mirror: the local renderer pane is broken out into a plain
local window the bridge does not own, and the remote pane stays where it was.
The local view now disagrees with the remote.

The violated invariant is the one `docs/agents/bridge-daemon.md` records for the
#769 menus: **an action that changes a remote object goes through the ctl entry
point when a faithful verb exists; with no faithful verb it is hidden on a
mirror**. #769 rewrote the menus and missed the keybinds. Two more keys carry the
same hole and were checked here:

- `prefix + @` and `MouseDown1Control7` both run
  `if-shell -F "#{pane_floating_flag}" { join-pane } { break-pane -W }` — the
  float/tile toggle. On a mirror pane they float or tile the local renderer pane
  and leave the remote untouched.
- `MouseDown1Control7` is unreachable in this config: it fires only on a
  `range=control|7` in `pane-border-format`, and this config's format
  (`setw -g pane-border-format "━━━━━"`) carries no control range.

Measured on the live server of the pinned next-3.9 (`tmux list-keys`):

```
bind-key -T prefix ! break-pane
bind-key -T prefix @ if-shell -F "#{pane_floating_flag}" { join-pane } { break-pane -W }
bind-key -T root MouseDown1Control7 if-shell -F "#{pane_floating_flag}" { join-pane } { break-pane -W }
```

## Approach

**A new ctl verb `break-pane`** (`picker/remotebridge/daemon/ctl.go`), mirroring
`kill-pane`'s intent flags:

```go
"break-pane": {layout: true, windows: true, moves: true, build: func(pane, _, _ string, _ []string) ([]string, error) {
    return []string{fmt.Sprintf("break-pane -s %s", pane)}, nil
}},
```

- `layout` — the source window loses the pane and reshapes.
- `windows` — a new remote window appears, and `reconcileWindows`' `added &&
  activeRemote` branch creates *and selects* the new mirror window.
- `moves` — the source window's active pane and the session's active window
  change, so the daemon's active-pane belief is invalidated.

The target is `-s`, never `-t`: `break-pane`'s `-t` names the **destination
window**, so `-t %N` would resolve the pane's own window as the destination (and
fail with `index in use`). `-s %N` names the source pane, and the new window
lands in that pane's own session. Verified on the pinned next-3.9 over a control
connection: `break-pane -s %0` adds the window at the end, makes it active
(`session_select`, `spawn.c`) and emits `%layout-change @0`, `%window-add @1`,
`%session-window-changed $0 @1`.

**`prefix !` is bridge-gated** in `config/tmux.conf.tmpl`:

```
bind-key -N 'Break pane to a new window' ! if-shell -F '<gate>' { run-shell "<ctl> break-pane #{q:@bridge_pane}" } { break-pane }
```

`break-pane` exists in every tmux, so this bind needs no version gate.

**`prefix @` is a no-op on a mirror**, in the same `%if next-3.9` block style as
the menus and drags:

```
%if "#{==:#{version},next-3.9}"
bind-key -N 'Toggle pane between floating and tiled' @ if-shell -F '<gate>' { display-message "float/tile: not mirrored - do it on the remote" } { if-shell -F '#{pane_floating_flag}' { join-pane } { break-pane -W } }
%endif
```

There is no faithful ctl verb for float/tile: a float is mirrored *from* the
remote, and `float-geom`/`tile-layout` only reshape an already-floating or
already-tiled remote object — neither creates a float nor tiles one. The version
gate is required because the stock branch carries `break-pane -W`, a next-only
flag a block tmux builds at source time would make an older resident server
reject (#407). The block is placed **after** the menu and drag blocks because
`tests/menu-bind-integration.bats` selects those two by ordinal.

`MouseDown1Control7` gets no bind: it cannot fire in this config, so a bind for
it would be dead config; the docs record why.

### Consumer map (ctl verb + guarded binds)

| edge | symbol / file | disposition |
| --- | --- | --- |
| request parser | `ctlState.parseCtl` (`ctl.go`) | compatible — reuses the pane lookup (`pane %s is not mirrored by this bridge`) and arity checks |
| verb producer | `verbs["break-pane"]` (`ctl.go`) | **new** |
| reply/intent accounting | `ctlState.submit`/`takeIntents`, `stampAll` barrier | compatible — same `layout`/`windows`/`moves` intents `kill-pane` uses, so the `og-fanout` barrier covers it unchanged |
| window reconcile | `reconcileWindows` | compatible — `windows: true` sets `wantWindows`; `added && activeRemote` selects the new mirror window |
| source-window reconcile | `reconcileLayout`/`applyPaneOps` | compatible — removes the moved pane from the source window (verified: the remote's `%layout-change` precedes `%window-add`, and the moved pane keeps painting through the new sink) |
| protocol version | `wire.CtlProtocolVersion` | **bumped 7 → 8** — the constant's own doc requires a bump for any verb-table change, so a new ctl against an old resident daemon says `reopen the bridge` |
| conf binds | `config/tmux.conf.tmpl` | **changed** (`!` guarded, `@` gated no-op) |
| byte-identity oracle | `config/tmux.conf.reference.nix` | **changed**, same text |
| conf assertion | `flake.nix` `break-pane-conf-assertions` | **new** |
| readiness assertions | `tests/tmux-next38-readiness.bats` | **changed** (else branches + notes) |
| mirror-routing integration | `tests/menu-bind-integration.bats` | **changed** (`!` keypress, which-key replay, `@` no-op, plain-window stock) |
| ctl → daemon → mirror integration | `tests/remote-m2-integration.bats` | **changed** (new `break-pane` case) |
| docs | `docs/agents/bridge-daemon.md` (#769 §) | **changed** |

## Steps

1. `picker/remotebridge/wire/protocol.go`: `CtlProtocolVersion` `"7"` → `"8"`.
2. `picker/remotebridge/daemon/ctl.go`: add the `break-pane` verb.
3. `picker/remotebridge/daemon/ctl_test.go`: a translation case
   (`break-pane -s %3`, windows + layout), a dedicated
   `TestParseCtlBreakPaneInvalidatesActiveBelief` (invalidate `@1`, no reseed),
   and rejection rows (unmirrored pane, extra argument).
4. `config/tmux.conf.tmpl` + `config/tmux.conf.reference.nix`: the guarded `!`
   bind and the `%if`-gated `@` block (byte-identical in both).
5. `flake.nix`: `break-pane-conf-assertions`.
6. `tests/menu-bind-integration.bats`: the four routing tests.
7. `tests/remote-m2-integration.bats`: the ctl `break-pane` end-to-end.
8. `tests/tmux-next38-readiness.bats`: else-branch and note assertions.
9. `docs/agents/bridge-daemon.md`: the keybind subsection and the
   `MouseDown1Control7` out-of-scope note.

## Acceptance

1. **Daemon/ctl Go test** — `cd picker && go test ./remotebridge/...` (PASS;
   `break-pane -s %3` with the right intents; `%99` → `not mirrored`).
2. **End-to-end bats test** — `tests/remote-m2-integration.bats`, "ctl break-pane
   moves the REMOTE pane to a new window and the mirror follows": the remote ends
   with the pane in a new window, the mirror gains a matching window, the moved
   pane still **paints** (a router-sink clobber would freeze it), and no
   non-mirror local window is stranded. Fails on current `main` (unknown verb).
3. **Conf assertion** — `break-pane-conf-assertions` (`!` and `@` guarded; `@`'s
   mirror branch a `display-message` no-op with no ctl call), plus the readiness
   else-branch assertions.
4. **Full local gate** — `nix build .#default`, `nix flake check`,
   `nix build .#lint`.