# Tiled mirror border drag reaches the remote (#823) — plan

Spec: `docs/superpowers/specs/2026-09-28-mirror-tiled-border-drag-design.md`.

## Files

| file | purpose |
|---|---|
| `picker/remotebridge/controlmode/layout.go` | extract one both-format parse helper; add `TiledLayout(s, id)` |
| `picker/remotebridge/controlmode/layout_test.go` | `TiledLayout` tests; `ParseLayout` pinned unchanged |
| `picker/remotebridge/daemon/ctl.go` | `tile-layout` verb + `tileLayoutCommand`; `ctlRequest.sentLayout`; `wantLayout map[string]string`; `submit`/`takeIntents`/`forgetWindow` |
| `picker/remotebridge/daemon/ctl_test.go` | verb, command text, rejection, intent coalescing; the one existing `c.wantLayout["@1"]` read adapts to the string map |
| `picker/remotebridge/daemon/windows.go` | `(*mirrorWindow).noteLocalLayout(sent)` — clear `layout` only when stale |
| `picker/remotebridge/daemon/windows_test.go` | `noteLocalLayout` table test |
| `picker/remotebridge/daemon/daemon.go` | `settle`: `for remoteID, sent := range layouts` → `mw.noteLocalLayout(sent)` before `reconcileLayout` |
| `picker/remotebridge/cmd/ctl/main.go` | `float-drag` → `drag`; `resolveDrag` float branch (unchanged request) + tiled branch (`TiledLayout` → `tile-layout`) |
| `picker/remotebridge/cmd/ctl/main_test.go` | `TestResolveFloatDrag` → `TestResolveDrag`, tiled cases |
| `generator/render/drags.go` | per-stock-bind gate (`resize-pane -M` → `bridgeGate`, else float gate); body `drag`; notes |
| `generator/render/drags_test.go` | gate per bind, body `drag` |
| `config/tmux.conf.reference.nix` | byte-identical twin of the drags.go output and the conf comment |
| `config/tmux.conf.tmpl` | the `{{.DragBinds}}` comment covers tiled panes |
| `picker/whichkey_test.go` | fixture row says `drag` (cosmetic, keeps it real) |
| `tests/menu-bind-integration.bats` | drag-block verb test expects `drag`, and `tile-layout` in ctl.go |
| `tests/float-drag-integration.bats` | tiled drag cases in the #797 harness; `sgr` reads the status line from the session, not `$LF` |
| `flake.nix` | `float-drag-integration-tests` env gains `CTL = "${pickerChecked}/bin/og-remote-bridge-ctl"` (test 6 drives the verb directly; the ctl is otherwise only on the tmux server's wrapper PATH) |
| `docs/agents/bridge-daemon.md`, `docs/agents/floats.md`, `README.md` | behaviour docs |
| `docs/superpowers/specs/…-design.md`, this plan | committed with the change |

## Steps

All Go commands run from `picker/` (or `generator/`) inside the devshell.

- [ ] **Step 1: failing `TiledLayout` tests** — `picker/remotebridge/controlmode/layout_test.go`, new `TestTiledLayout` (table) and `TestTiledLayoutErrors`:
  - v2 two tiled panes + a float leaf *between* them (`{"t":"h",…,"c":[p %0, p %2 z:0, p %1]}`), map `%0→%10, %1→%11`: `Raw == "<csum>,100x30,0,0{50x30,0,0,10,49x30,51,0,11}"`, `Panes` ids `%10 %11`, `W,H == 100,30`, `Floats == nil`.
  - v1 with a trailing float section (`…{…,…}<20x8,5,3,2>`), same mapping: float pruned, ids mapped.
  - 3+ pane nested: v2 `h[ p%0, v[ p%1, p%2 ] ]` and v1 `100x30,0,0{50x30,0,0,0,49x30,51,0[49x15,51,0,1,49x14,51,16,2]}` with map `%0→%5 %1→%6 %2→%7`: Raw body `100x30,0,0{50x30,0,0,5,49x30,51,0[49x15,51,0,6,49x14,51,16,7]}`, Panes `%5 %6 %7` in tree order.
  - checksum validity: `ParseLayout(out.Raw)` succeeds and its `Raw == out.Raw` and its `Panes` equal `out.Panes`.
  - hostile checksum prefix: v1 input `x'; kill-server,100x30,0,0,1` with identity map → `Raw` is `<fresh csum>,100x30,0,0,1`, never containing `kill`.
  - byte equality §5 relies on: for the v2 fixtures, `TiledLayout(s, identity).Raw == ParseLayout(s).Raw`.
  - errors: a tiled leaf the map does not know; a map value `%x`, `""`, `5`; malformed input (`garbage`, v2 `{"V":2}` without `L`); a layout whose only leaves are floats.
  - Run `cd picker && go test ./remotebridge/controlmode/` → **fails to compile** (`undefined: TiledLayout`). Record it.
- [ ] **Step 2: implement** — `layout.go`: `parseTree(s string) (root *node, floats []PaneCell, err error)` holding today's both-format parse (v2 decode/trailing/version/`buildV2Node`; v1 checksum cut/`cell`/`floatSection`/trailing check). `ParseLayout` = `parseTree` + today's prune/Raw/collect logic, keeping `Raw = s` exactly for a v1 input with no floats. `TiledLayout(s string, id func(string) (string, bool)) (Layout, error)`: `parseTree`, `pruneFloats` (error "no tiled panes" on nil), walk leaves replacing `n.id` with `id(n.id)` — error unless `ok && validPaneID(mapped)` — then `writeCell` + `layoutChecksum` into `Raw`, `collectLeaves` into `Panes`, `W/H` from the unpruned root. Doc comment says `Raw` is always rebuilt, never `s`. Run `go test ./remotebridge/controlmode/` → PASS (all old + new); `go test -run xxx -bench . -benchtime 1x ./remotebridge/controlmode/` compiles and runs.
- [ ] **Step 3: failing daemon tests** — `ctl_test.go`:
  - `wantTileLayout(pane, order []string, w, h int, raw string) string` built from literals (independent of production), pinning `if-shell -t <pane> -F '<cond with #{P/i:…}>' 'select-layout -t <pane> '\''<raw>'\'''`.
  - `parseCtl([v, "tile-layout", "%3", <v1 raw with ids %3 %4>])` on `newCtlStateWith("@1", "%3", "%4")`: `cmds == [wantTileLayout("%3", ["%3","%4"], 100, 30, rebuiltRaw)]`, `wantLayout == "@1"`, `sentLayout == rebuiltRaw`, `invalidate == ""`.
  - a hostile checksum prefix in the arg never reaches `cmds` (rebuilt).
  - rejects: no layout arg, two args, `garbage`, a leaf id the parser yields as `%` (`100x30,0,0,`).
  - intents: submit `tile-layout` (S) then `layout tiled` for `@1` → `takeIntents` layouts `{"@1": S}`; `layout` then `tile-layout` → S; two `tile-layout`s → the second S; `layout` alone → `{"@1": ""}`; `forgetWindow` drops it.
  - `windows_test.go` `TestNoteLocalLayout`: `(layout, sent) → layout'`: `("A","")→"A"`, `("A","A")→"A"`, `("A","B")→""`, `("","B")→""`.
  - Run `go test ./remotebridge/daemon/ -run 'TileLayout|Intent|NoteLocalLayout|ForgetWindow'` → compile failure. Record it.
- [ ] **Step 4: implement daemon** — `ctl.go`: `ctlRequest.sentLayout string` (doc: the tiled layout a local drag already shows, for `noteLocalLayout`); a new `verb.localLayout bool` (doc: the verb's one argument is a tiled layout the local window already shows); `parseCtl`, after `build` accepted the args, runs `L, err := tiledArg(args[0])` for such a verb, returns `err` like any other parse error, and sets `req.sentLayout = L.Raw`, where `tiledArg(s) (controlmode.Layout, error)` is the one package func both `build` and `parseCtl` call (`controlmode.TiledLayout` with an identity map; validation lives in `TiledLayout`). `tileLayoutCommand(pane string, L controlmode.Layout) string` per spec §4 (`P/i`, size, zoom guards; `tmuxQuote` nested). Verb entry `"tile-layout": {args: 1, layout: true, build: …}` with a comment block like `float-geom`'s. `wantLayout map[string]string`; `submit`: `if req.sentLayout != "" { c.wantLayout[w] = req.sentLayout } else if _, ok := c.wantLayout[w]; !ok { c.wantLayout[w] = "" }`; `takeIntents` returns `layouts map[string]string`; `newCtlState` map type. `windows.go`: `noteLocalLayout`. `daemon.go` `settle`: iterate the map, `mw.noteLocalLayout(sent)` before `reconcileLayout`. Adapt the existing `sawIntent = c.wantLayout["@1"]` read (`_, sawIntent = …`). Run `go test ./remotebridge/daemon/` → PASS; `go vet ./remotebridge/...` clean.
- [ ] **Step 5: failing ctl tests** — `cmd/ctl/main_test.go`: rename `TestResolveFloatDrag` → `TestResolveDrag`; the fake `runTmuxOut` now answers the combined format (8 geometry fields, then `|<pairs>|<layout>`). Cases: float → `["float-geom","%3","%9",…]` exactly as before; tiled with a v2 layout holding a user float (no `@bridge_pane`) and two mirror panes → `["tile-layout", "<remote of stashed>", "<raw with remote ids>"]`; a tiled leaf whose `@bridge_pane` is empty → error naming the pane; stashed tiled pane with bad `@bridge_pane` → error; bad local pane id → no tmux call. Run `go test ./remotebridge/cmd/ctl/` → FAIL (undefined `resolveDrag`).
- [ ] **Step 6: implement ctl** — `main.go`: `dragFormat = floatDragFormat's fields + "|#{P:#{pane_id}=#{@bridge_pane} }|#{window_layout}"`; `resolveDrag` splits with `SplitN(…, 10)`; floating → the old `float-geom` argv; else pairs → map (values must match `panePattern`), `controlmode.TiledLayout(layout, lookup)` → `["tile-layout", bridgePane, L.Raw]`. `main()` handles `drag` (was `float-drag`); usage string and package doc updated. Run `go test ./remotebridge/cmd/ctl/` → PASS.
- [ ] **Step 7: failing generator tests** — `drags_test.go`: `TestDragStartBinds` expects, per stock line, gate `bridgeGate` when `s.cmd == "resize-pane -M"` and `#{&&:bridgeGate,#{pane_floating_flag}}` otherwise; `TestDragEndTableComplete` body `… drag #{q:@og_bridge_drag}`; add an assertion that exactly one stock bind takes the wide gate. Run `cd generator && go test ./render/` → FAIL.
- [ ] **Step 8: implement generator + twin** — `drags.go`: per-bind gate with the `cmd-join-pane.c` reason in a comment; body `drag`; notes `dragEndNote = "Route a mirror border drag to the remote"`. `config/tmux.conf.reference.nix`: `dragStartBindLine` picks `bridgeGate` when `s.cmd == "resize-pane -M"`, body `drag`, same note; the `{{.DragBinds}}` comment in `config/tmux.conf.tmpl` and its reference twin both mention tiled panes (#823). `picker/whichkey_test.go` fixture `drag`. `tests/menu-bind-integration.bats`: the drag-block test expects `drag`, greps `'"drag"'` in `$CTL_MAIN_GO` and `'"tile-layout": {'` + `'"float-geom": {'` in `$CTL_GO`. Run `cd generator && go test ./...` → PASS; `cd picker && go test ./ -run WhichKey` (package `picker`) → PASS.
- [ ] **Step 9: integration tests** — `tests/float-drag-integration.bats` (header gains the tiled case): `sgr` reads status from `-t host-sess`. Add `mirror_tiled_up [remote setup cmds…]` (remote `rem` 100x30, a `-h` split, optional extra shaping, no float; sets `LL`/`LR` local left/right of the first divider and `RL`/`RR` remote). Helpers `tiled_geoms <server>` (all tiled panes of the window, `list-panes -F '#{pane_floating_flag}|#{pane_left},#{pane_top} #{pane_width}x#{pane_height}'`, floats dropped, sorted) and `wait_tiled_agree <before>`. Tests:
  1. issue table: drag divider 10 cols left → local == remote tiled geoms, differs from before, remote left width == before − 10.
  2. with a mirror float open (`mirror_up`): divider drag syncs, float geometry unchanged on both sides.
  3. 3-pane nested (`split -h`, then `split -v` on the right pane): drag the right column's horizontal divider 3 rows → agree, changed.
  4. rotated remote (3 `-h` panes + `rotate-window`): drag → agree, changed.
  5. local non-mirror window (`$DST new-window`, `split-window -h`, select it): drag divider → local left pane width changed by the drag.
  6. guard + snap-back: on a two-pane mirror, `$DST select-layout -t <local win>` a skewed layout (simulated drag), then run `"$CTL" --sock "$SOCK" tile-layout <RL> <that skewed layout with the two remote ids swapped>` (`CTL` is required in `setup` like `DAEMON`); first wait (≤2s) for the local tiled geoms to snap back to the remote's, then assert the remote geoms still equal their pre-test value — both are ordered after the command, so the remote check cannot pass before the command ran.
  Add `CTL` to the `float-drag-integration-tests` env in `flake.nix`.
  Red run: create a scratch worktree at `origin/main` (`git worktree add /tmp/og-823-red origin/main`), copy the new bats file and the `flake.nix` `CTL` line in, `nix build .#checks.x86_64-linux.float-drag-integration-tests` there → FAIL on 1–4 and 6 (5 passes on main; 6 fails on main because `tile-layout` is an unknown verb). Record the failing assertion lines; `git worktree remove` it.
- [ ] **Step 10: docs** — confirm the spec and this plan are at `docs/superpowers/specs/2026-09-28-mirror-tiled-border-drag-design.md` / `docs/superpowers/plans/2026-09-28-mirror-tiled-border-drag.md` (they are authored there; the crew artifacts are copies) and staged with the change; `bridge-daemon.md`: the #797 bullet's last-but-one sentences become the tiled design (gate, ctl `drag`, `tile-layout`, guards incl. why `P/i`, `noteLocalLayout`); the menu-classification table row mentions tiled drag; `floats.md` `@og_bridge_drag` bullet (written by the mirror branch of `MouseDrag1Border` for any mirror pane, M-binds float-only; ctl `drag`); `README.md` lines 360–361.
- [ ] **Step 11: gates** — `nix build .#default` → ok; `nix build .#checks.x86_64-linux.float-drag-integration-tests .#checks.x86_64-linux.menu-bind-integration-tests` → ok (green run of Step 9's tests); `nix flake check` → ok; `nix build .#lint` → ok.

## Acceptance

| task acceptance | settled by |
|---|---|
| integration test reproducing the table, red on main / green with fix | Step 9 red run (recorded) + Step 11 green `float-drag-integration-tests` |
| unit tests for local→remote-order layout string incl. float + 3+ pane nested | Step 1/2 `TestTiledLayout` (+ Step 5/6 ctl tiled branch) |
| `nix build .#default`, `nix flake check`, `nix build .#lint` pass | Step 11 |
| docs updated | Step 10 |
| must-hold: floats untouched | integration test 2 + `TiledLayout` float fixtures |
| must-hold: converge without loop/bounce | spec §5; `TestNoteLocalLayout`; integration test 6 |
| must-hold: non-mirror drags unchanged | integration test 5; drags_test else-branch unchanged |
