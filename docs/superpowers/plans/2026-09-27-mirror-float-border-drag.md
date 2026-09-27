# Mirror float border drag reaches the remote (#797): plan

Spec: `docs/superpowers/specs/2026-09-27-mirror-float-border-drag-design.md`
(accepted by the spec critic).

## File list

| file | purpose |
|---|---|
| `picker/remotebridge/daemon/floatgeom.go` | `clampInner`: the inner box of `outerFromCell`'s clamped outer box |
| `picker/remotebridge/daemon/floatgeom_test.go` | `clampInner` cases |
| `picker/remotebridge/daemon/ctl.go` | the `float-geom` verb and its remote-command builder |
| `picker/remotebridge/daemon/ctl_test.go` | translation, rejection and flag tests for `float-geom` |
| `picker/remotebridge/cmd/ctl/main.go` | `float-drag <local-pane>`: resolve the float via local `display-message`, send `float-geom` |
| `picker/remotebridge/cmd/ctl/main_test.go` | resolution and refusal tests with a stubbed tmux runner |
| `picker/whichkey.go`, `picker/whichkey_test.go` | skip the `og-bridge-drag` table in `parseListKeysRows` |
| `generator/render/stockdrags.txt` (new) | verbatim `list-keys` lines for the two stock drag binds |
| `generator/render/drags.go` (new) | `dragBinds(p)`: the `%if`-gated drag-start binds and the 160-line `og-bridge-drag` table |
| `generator/render/drags_test.go` (new) | generator tests for the block |
| `generator/render/menus.go` | `parseStockMenus` takes the file name for its panic message |
| `generator/render/render.go` | `DragBinds` field on `Data`, set in `Build` |
| `config/tmux.conf.tmpl` | `{{.DragBinds}}` slot, directly after `{{.MenuBinds}}`, with a comment |
| `config/tmux.conf.reference.nix` | a Nix twin of `dragBinds`, reading `stockdrags.txt`, spliced after `${menuBinds}` |
| `tests/menu-bind-integration.bats` | the stock tripwire also diffs `$STOCK_DRAGS` |
| `tests/float-drag-integration.bats` (new) | end to end: real daemon + generated config + pty client + SGR drags |
| `flake.nix` | `STOCK_DRAGS` on `menu-bind-integration-tests`; a new `float-drag-integration-tests` check |
| `docs/agents/floats.md`, `docs/agents/bridge-daemon.md` | behaviour docs |
| `docs/superpowers/specs/…-design.md`, this plan | committed alongside |

Nothing touches the reconcile path (`reconcile*.go`, `floatdiff.go`), so #808
is unaffected.

## Steps

- [ ] **Step 1: failing `clampInner` test** in `floatgeom_test.go`. Table:
  an in-window cell `{X:11,Y:6,W:38,H:10}` in `100x30` round-trips unchanged;
  a cell dragged off the left edge `{X:-5,Y:6,W:45,H:10}` becomes
  `{X:1,Y:6,W:45,H:10}`; a cell past the bottom-right edge, `{X:70,Y:25,W:38,H:10}`
  in `100x30`, becomes `{X:61,Y:19,W:38,H:10}`; a
  cell wider than the window, `W:120` in `100`, becomes `W:98, X:1`. The ID is
  carried through. Run `cd picker && go test ./remotebridge/daemon -run
  TestClampInner`; expect a compile failure (undefined), which is not red
  evidence. The red for the invariant is Step 10's.
- [ ] **Step 2: implement `clampInner(c controlmode.PaneCell, winW, winH int)
  controlmode.PaneCell`** in `floatgeom.go`: `ow, oh, ox, oy :=
  outerFromCell(c, winW, winH)`, then return the inner box `{ID: c.ID, X:
  ox+floatInset, Y: oy+floatInset, W: ow-2*floatInset, H: oh-2*floatInset}`.
  Add a doc comment on why the verb needs it (the reconcile places the local
  float with the same clamp). Step 1's test passes.
- [ ] **Step 3: failing `float-geom` ctl tests** in `ctl_test.go`:
  - A `TestParseCtlVerbTranslation` row: argv `float-geom %3 11 6 38 10 100 30`
    gives exactly one command, the nested `if-shell` of Step 4 with bordered
    branch `move-pane -t %3 -X 10 -Y 5 ; resize-pane -t %3 -x 40 -y 12` and
    none branch `move-pane -t %3 -X 11 -Y 6 ; resize-pane -t %3 -x 38 -y 10`,
    with `layout: "@1"`, `windows` false and `reseed` empty.
  - A row for the negative-offset case (`-5 6 45 10 100 30`) asserting the
    clamped inner box `1,6 45x10` reaches both branches.
  - `TestParseCtlRejects` rows: wrong arity (5 args), a non-integer (`1x`), `w`
    = 0, `winW` = 2 (below the 3 a bordered box needs), `x` = 10000, and an
    injection attempt `11;kill-server`.
  - A flags test: `invalidate` empty (not `moves`), `needsView` false, and
    `probePane` empty.

  Run `go test ./remotebridge/daemon -run 'TestParseCtl'`; expect failures
  with "unknown verb".
- [ ] **Step 4: implement the verb** in `ctl.go`.
  - `"float-geom": {args: 6, layout: true, build: …}`. Parse each arg with
    `strconv.Atoi` and check its bounds: `x`, `y` in `-9999..9999`; `w`, `h`
    in `1..9999`; `winW`, `winH` in `3..9999`. The error is `float-geom: bad
    <name> %q`.
  - Clamp with `clampInner`, then build the command with `floatGeomCommand(pane
    string, c controlmode.PaneCell) string`:
    `if-shell -t %R -F '#{pane_floating_flag}' <inner>`, where `<inner>` is
    `tmuxQuote("if-shell -t %R -F '#{==:#{pane-border-lines},none}' " +
    tmuxQuote(<none branch>) + " " + tmuxQuote(<bordered branch>))`.
    The none branch is `move-pane -t %R -X x -Y y ; resize-pane -t %R -x w -y h`.
    The bordered branch is the same with `x-1 y-1` and `w+2 h+2`. This is the
    string-branch style the `tool` verb uses (`ctl.go:470`).
  - A comment block on the verb carries the why from spec §3: the inner box
    as the unit, move before resize, the tiled guard, and no `moves`.
  - Then run `go test ./remotebridge/daemon/...`; all pass. Also run `go vet
    ./remotebridge/...`.
- [ ] **Step 5: live check of the built remote command.** Run it against a
  scratch `tmux -L` server with a heavy float and a `-B none` float. For each,
  `tmux <cmd>` must land the float on the requested inner box, and a tiled
  target must be untouched. Scratch servers only (`TMUX_TMPDIR=/tmp/og-$$`),
  killed on their own socket. This is evidence only; nothing is committed.
- [ ] **Step 5a: failing ctl `float-drag` tests** in
  `picker/remotebridge/cmd/ctl/main_test.go`. Make the tmux runner seam
  capture output as well: a `runTmuxOut` var beside `runTmux`. With it
  stubbed, `resolveFloatDrag("%7")` runs `display-message -p -t %7 <fmt>`
  (fmt asserted verbatim) and turns the newline-terminated reply `%3|1|11|6|38|10|100|30\n`
  (as `display-message -p` prints it; trimmed before splitting) into
  `["float-geom","%3","11","6","38","10","100","30"]`. Refusals: arg `7` or
  `%7;x`, which never reach tmux; a tmux error; empty `@bridge_pane`
  (`|1|…`); `pane_floating_flag` 0; `@bridge_pane` `%3;x`; a reply with the
  wrong field count. Run `cd picker && go test ./remotebridge/cmd/ctl/`;
  expect "undefined".
- [ ] **Step 5b: implement** in `main.go`. Beside the `focus` special case:
  `if args[0] == "float-drag" && len(args) == 2 { args, err =
  resolveFloatDrag(args[1]) }`, and on error take the existing
  `showError`/`fail` path. `resolveFloatDrag` validates with
  `regexp.MustCompile("^%[0-9]+$")` and splits the reply on `|` into
  exactly 8 fields after `strings.TrimSpace`. The usage line and the package
  comment gain the `float-drag <local-pane-id>` form. A comment says why ctl resolves the float: tmux targets
  are not format-expanded, and the release target is not the float. Step 5a
  passes, and `go vet ./remotebridge/...` is clean.
- [ ] **Step 5c: which-key skips the table.** A failing
  `picker/whichkey_test.go` case: a `parseListKeysRows` input with an
  `og-bridge-drag|…|MouseDragEnd1Pane` row next to a prefix row yields only
  the prefix row. Then filter in `parseListKeysRows` (`if parts[0] ==
  bridgeDragTable { continue }`, with a const and a comment naming the
  generator's table). The raw view (`ctrl+r`, `listKeysRaw`) stays the
  verbatim `list-keys` dump, since that is its stated contract; note that in
  the const's comment. Run `cd picker && go test . -run ParseListKeys`.
- [ ] **Step 6: `stockdrags.txt` + failing generator tests.** Write
  `generator/render/stockdrags.txt` from `list-keys` on the pinned binary
  (`-f /dev/null`):
  `bind-key  -T root MouseDrag1Border resize-pane -M`
  `bind-key  -T root M-MouseDrag1Border move-pane -M`.
  `drags_test.go` checks:
  - `TestDragBindsGatedOnStockVersion`: the first line is the same `%if`
    `menuBinds` uses (`stockMenuVersion`) and the last is `%endif`.
  - `TestDragStartBinds`: exactly two `-T root` binds, keyed from
    `stockdrags.txt`. Each carries `if-shell -F -t = '<bridgeGate &&
    pane_floating_flag>'`, and its mirror branch is `set -F @og_bridge_drag
    '#{pane_id}' ; <stock cmd> ; switch-client -T og-bridge-drag`. The else
    branch is the stock command verbatim, double-quoted with
    `tmuxDoubleQuote`.
  - `TestDragEndTableComplete`: exactly 160 `-T og-bridge-drag` binds; the
    key set equals 20 locations × the 8 prefixes `"" M- C- S- C-M- M-S- C-S-
    C-M-S-` (tmux prints `C-` then `M-` then `S-`, `key-string.c:347-351`;
    Step 7 confirms it with `list-keys`); every body is exactly `run-shell "`
    + `bridgeCtl(p)` + ` float-drag #{q:@og_bridge_drag}"`.

  Run `cd generator && go test ./render/`; expect "undefined: dragBinds".
- [ ] **Step 7: implement `dragBinds`** in `drags.go`.
  - `//go:embed stockdrags.txt`, parsed with the existing `parseStockMenus`,
    which is shape-only. A malformed line panics.
  - Build the block in the order: `%if`, the two start binds, the 160 end
    binds, `%endif`. `parseStockMenus` gains a `name` argument so a malformed
    `stockdrags.txt` panics naming that file.
  - Add `DragBinds string` to `Data` (comment like `MenuBinds`), and set
    `DragBinds: dragBinds(p)` in `Build`.
  - In `config/tmux.conf.tmpl`, add a comment plus `{{.DragBinds}}` directly
    after `{{.MenuBinds}}`.
  - Then run `go test ./render/` (passes), `nix build .#default`, and
    sourcing the built conf's drag block into a raw scratch server:
    the second `%if`…`%endif` range of the built conf.
    `list-keys -T og-bridge-drag` must count 160 and name-match the test's
    key set exactly. If tmux prints the modifiers in another order, fix the
    Go list to tmux's order, since list-keys is the ground truth.
    Also run `nix build .#checks.x86_64-linux.conf-shell-quoting-tests`; it
    passes.
- [ ] **Step 8: Nix twin** in `config/tmux.conf.reference.nix`: a `dragStock`
  parse of `stockdrags.txt` (same regex as `menuStock`), then `dragBinds`,
  built from the same location and modifier lists in the same order, spliced
  on the line after `${menuBinds}`, in the same position as the tmpl slot.
  The tmpl comment from Step 7 is copied byte for byte into the twin's text.
  Run `nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions`; it
  passes, which proves byte identity.
- [ ] **Step 9: tripwire covers `stockdrags.txt`.** In
  `tests/menu-bind-integration.bats` setup, add `STOCK_DRAGS="${STOCK_DRAGS:?…}"`.
  The stock-tripwire test loops over `"$STOCK_MENUS" "$STOCK_DRAGS"`: the
  same `list-keys` diff for each file. The version-gate test also checks,
  after the version flip, that each `stockdrags.txt` line is what
  `list-keys` returns. In `flake.nix`, set `STOCK_DRAGS =
  ./generator/render/stockdrags.txt;` on `menu-bind-integration-tests`. Run
  `nix build .#checks.x86_64-linux.menu-bind-integration-tests`; it passes.
  The verb cross-check test ("every verb the menu block sends …") extracts
  with a re-triggering `sed` range, so it would also read the drag block and
  fail on `float-drag`, which is ctl-side. Scope its extraction to the
  **first** `%if`…`%endif` block: `sed -n '/^%if "#{==:#{version},/,/^%endif$/{p;/^%endif$/q}'`.
  Add a sibling test that reads the **second** block and asserts that every
  `--sock=#{q:@bridge_sock} <verb>` there is `float-drag`, that
  `"float-drag"` appears in `$CTL_MAIN_GO` (`picker/remotebridge/cmd/ctl/main.go`,
  a new env var wired in `flake.nix`), and that `"float-geom": {` appears in
  `$CTL_GO`.
- [ ] **Step 10: end-to-end bats**, `tests/float-drag-integration.bats`,
  plus a flake check `float-drag-integration-tests`.
  - The check builds the wrapped tmux with enrich/agent-usage off (the
    `renameBindTmuxConfig` pattern) as `TMUX_BIN`, plus `TMUX_RAW` (raw
    `mkTmux`), `DAEMON`, `RENDERER` and `CONF`. Its `PATH` puts the wrapped
    tmux first, so the daemon's `--test-local` local commands and the
    conf-embedded ctl run as in production. `nativeBuildInputs` are bats,
    coreutils, gnugrep, gnused, gawk, procps and jq.
  - Harness: SRC is `$TMUX_RAW -L fdsrc -f <minimal src.conf>` (the M2
    `SRC_CONF`), and DST is `$TMUX_BIN -L fddst`, the real conf. Set
    `@splash_shown 1`, and use private `CLAUDE_STATUS_DIR`/`OG_*` dirs as in
    `menu-bind-integration.bats`. The daemon runs `--test-local
    --src-socket fdsrc --dst-socket fddst`. The pty host is `$TMUX_RAW -L
    fdobs` attaching a client to the mirror session. The remote float is
    `new-pane -x 40 -y 12 -X 10 -Y 5 "sleep 300"` and the second remote pane
    comes from `split-window -h`. SGR input goes through
    `send-keys -t <obs> -l $'\e[<…'`.
  - Helpers: `geom <server> <pane>` prints `left,top WxH`, and
    `wait_agree <remote pane> <local pane>` polls every 0.1s for at most 2s
    until the two agree and also differ from the pre-drag geometry.
  - Four `@test`s, one per spec acceptance 1a to 1d. In 1d, before the
    release, `$DST select-pane -t <the tiled mirror pane>` moves the local
    active pane off the float. Each starts from a
    fresh mirror, finds the float's local pane via `@bridge_pane`, reads its
    border cells from the local geometry (never hard-coded), then drives
    press, drag, release.
  - `teardown` kills all three servers on their own sockets and the daemon.
  - **Red evidence:** run the file once against a DST with
    `unbind -T root MouseDrag1Border ; bind -T root MouseDrag1Border
    resize-pane -M` (and the same for `M-`) sourced after start, which puts
    back the pre-fix stock binds. Record the failing assertion, then run it
    green without the override. The run command is `nix build
    .#checks.x86_64-linux.float-drag-integration-tests`, and it passes. The
    red run uses a local `bats` with the same env vars built from `nix
    build` outputs, with its output recorded in `REVIEW_NOTES.md`.
- [ ] **Step 11: docs.**
  - In `bridge-daemon.md`, add a "Mirror invariants" bullet ("A mirror
    float's border drag crosses as a ctl verb"): the mechanism, the key-table
    trick, the inner-box unit, and the residual. Update the pane-menu row
    "Move, Move & Resize…": still hidden, but the mouse drag now routes via
    `float-geom`.
  - In `floats.md`, add a bullet for `@og_bridge_drag`, ctl `float-drag`
    and the `og-bridge-drag` table (who writes and reads them, and why the
    table), plus a note that tiled mirror border drags are still local-only
    (#follow-up).
- [ ] **Step 12: file the tiled follow-up issue** (`tracker: github`,
  standing approval). First `gh issue list --search "tiled mirror border
  drag"`. The issue gets the measured `40/59 vs 50/49` table, the
  `select-layout` direction, and links to #797 and the PR.
- [ ] **Step 13: gates.** Run the fast deterministic gate: `cd picker && go
  vet ./... && go test . ./remotebridge/...`, `cd generator && go test ./...`,
  then `nix build .#default`, `nix build .#lint` and `nix flake check`.

## Acceptance mapping

| spec acceptance | evidence |
|---|---|
| 1 e2e agree within 2s (a–d), red→green | `float-drag-integration-tests` green; red run recorded (Step 10) |
| 2 verb + ctl unit tests | `go test ./remotebridge/daemon -run 'TestParseCtl|TestClampInner'`, `go test ./remotebridge/cmd/ctl/` |
| 3 generator test + scanner | `go test ./render/ -run 'TestDrag'`; `conf-shell-quoting-tests` |
| 4 docs | `git diff --stat docs/agents` |
| 5 builds/checks | `nix build .#default`, `nix flake check`, `nix build .#lint` |
