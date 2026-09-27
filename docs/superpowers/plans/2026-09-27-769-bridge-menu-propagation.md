# Plan: bridge menu propagation (#769)

Spec: `docs/superpowers/specs/2026-09-27-769-bridge-menu-propagation-design.md`
(accepted). Read its Classification, Expansion layers and Old resident servers
sections before any step — this plan does not repeat the rationale.

Notation used below:

- `GATE` = `#{&&:#{@bridge_win},#{@bridge_pane}}` (`bridgeGate`, keys.go).
- `CTL` = `bridgeCtl(p)` (keys.go); `E1(s)` doubles every `#` in `s` (one
  display-menu build layer); `E2(s)` = `E1(E1(s))` (`run-shell -C` + build).
- `ITEM(verb, args…)` = `run-shell "E1(CTL) <verb> ##{q:@bridge_pane}<args>"`.

## File list

| File | Purpose |
|---|---|
| `generator/render/stockmenus.txt` (new) | The pinned tmux's own `list-keys -T <table> <key>` line for each of the ten menu bindings, verbatim, one per line, in the order of the table in Step 1 |
| `generator/render/menus.go` (new) | `go:embed` of the stock file, `stockMenuVersion`, escaping helpers, mirror item lists, `menuBinds(p)` |
| `generator/render/menus_test.go` (new) | Unit tests listed in Steps 2 and 4 |
| `generator/render/render.go` | `MenuBinds string` field in `Data`, set in `Build` |
| `config/tmux.conf.tmpl` | Comment + `{{.MenuBinds}}` line after `set -g detach-on-destroy off` |
| `config/tmux.conf.reference.nix` | The same block, built from the same stock file and literal mirror text (two-file rule) |
| `tests/conf-shell-quoting.bats` | `run-shell -C` argument recursed as a tmux command, plus fixtures |
| `tests/menu-bind-integration.bats` (new) | Behavioural + stock-parity + version-gate tests |
| `flake.nix` | `menu-bind-integration-tests` check |
| `docs/agents/bridge-daemon.md` | "Menus in a mirror window" section with the classification tables; #547 Respawn sentence |
| `README.md` | Bridge paragraph: menus act on the remote too |
| `docs/superpowers/specs/…-design.md`, this plan | Committed with the code (CLAUDE.md) |

Nothing under `picker/` changes: every propagating item uses an existing verb.

## Steps

- [ ] **Step 1: capture the stock text.** Create `generator/render/stockmenus.txt`
  from the raw pinned binary (never the wrapper, which adds `-f <conf>`), in
  `bash -c` (the interactive shell is fish):
  ```
  R=$(nix build --no-link --print-out-paths --impure --expr '(builtins.getFlake (toString ./.)).packages.x86_64-linux.default')/bin/.tmux-wrapped
  # (or the store path of mkTmux pkgs' bin/tmux; both are the pinned next-3.9)
  T=/tmp/og-sm$$; mkdir -p $T
  TMUX_TMPDIR=$T $R -f /dev/null -L sm new-session -d
  for k in "prefix <" "prefix >" "root MouseDown3Pane" "root M-MouseDown3Pane" "root MouseDown3Status" "root M-MouseDown3Status" "root MouseDown3StatusLeft" "root M-MouseDown3StatusLeft" "root MouseDown3Empty" "root M-MouseDown3Empty"; do
    set -- $k; TMUX_TMPDIR=$T $R -f /dev/null -L sm list-keys -T "$1" "$2"
  done > generator/render/stockmenus.txt
  TMUX_TMPDIR=$T $R -L sm kill-server; rm -rf $T
  ```
  If the wrapper path differs, use `readlink -f` on `./result/bin/tmux`'s
  `exec` target. Expected: 10 lines, each starting `bind-key`, containing no tab,
  no `$`, no `\`. Proof: `wc -l generator/render/stockmenus.txt` → 10;
  `grep -c '^bind-key' …` → 10; `grep -cP '[\t$\\\\]' …` → 0.

- [ ] **Step 2: failing unit tests for the helpers and the stock branch.**
  `generator/render/menus_test.go`:
  - `TestEscapeLayers`: `E1("a #{b} ##{c}") == "a ##{b} ####{c}"`, `E2` quadruples.
  - `TestQuoteRoundTrip`: `tmuxDoubleQuote(s)` wraps in `"` and escapes exactly
    `\`, `"`, `$` (tmux `args_escape`'s set for a double-quoted word); an
    `unquote` test helper reverses it for every stockmenus line.
  - `TestStockMenusParse`: 10 entries; each `table`/`key` is one of the ten; the
    command part is the line with the `bind-key -T <table> <key>` prefix and its
    column padding removed (`strings.Fields` on the first four fields, then the
    remainder after the key token).
  - `TestMenuBindsGatedOnStockVersion`: `menuBinds(keysPaths())` first line is
    `%if "#{==:#{version},next-3.9}"`, last line `%endif`, exactly 10 lines
    between, each starting `bind-key `.
  - `TestMenuStockBranchIsVerbatim`: for every bind line, the final argument is a
    double-quoted string whose unquoted value equals that binding's stock command.
  - `TestPrefixMenusKeepStockNotes`: the `<` line carries
    `-N 'Display window menu' -T prefix <`, the `>` line `-N 'Display pane menu' -T prefix >`.
  Run: `cd generator && go test ./render/ -run 'Escape|Quote|StockMenus|MenuBinds|MenuStock|PrefixMenus'`
  → fails (undefined symbols).

- [ ] **Step 3: implement `menus.go`** (implement: escalated — the escaping is the
  injection boundary for remote-derived values).
  - `//go:embed stockmenus.txt`, parsed once into `[]stockMenu{table, key, cmd}`
    (panic on a malformed line is fine: it is build input).
  - `const stockMenuVersion = "next-3.9"`.
  - `esc1`, `esc2` (`strings.ReplaceAll(s, "#", "##")` once / twice),
    `tmuxDoubleQuote`.
  - Mirror texts, **exactly** (P = prefix variant, M = mouse variant):
    - window menu: `display-menu -T "#[align=centre]#{window_index}:#{window_name}"`
      then P: `-x W -y W` / M: `-t = -x W -y W`, then
      `"#{?#{>:#{session_windows},1},,-}Swap Left" l { swap-window -t :-1 } "#{?#{>:#{session_windows},1},,-}Swap Right" r { swap-window -t :+1 } '' Kill X { ITEM(kill-window) } Rename n { command-prompt -I'##{@window_bridge_name}' { run-shell "E1(CTL) rename ##{q:@bridge_pane} ##{qs:1}" %1 } } '' "New Window" w { ITEM(new-window) }`
    - pane menu: `display-menu -T "#[align=centre]#{pane_index} (#{pane_id})"`
      then P: `-x P -y P` / M: `-t = -x M -y M`, then the stock items from
      `"#{?#{m/r:(copy|view)-mode,#{pane_mode}},Go To Top,}" <` through
      `… h { copy-mode -q ; set-buffer "#{q:mouse_hyperlink}" } ''` copied
      verbatim from the `prefix >` stock line (hold them as one Go constant and
      assert in a test that it is a substring of that stock command), then
      `"#{?#{!:#{pane_floating_flag}},Horizontal Split,}" h { ITEM(split-h) } "#{?#{!:#{pane_floating_flag}},Vertical Split,}" v { ITEM(split-v) } '' "#{?#{&&:#{!:#{pane_floating_flag}},#{>:#{window_panes},1}},Swap Up,}" u { ITEM(swap, " U") } "#{?#{&&:#{!:#{pane_floating_flag}},#{>:#{window_panes},1}},Swap Down,}" d { ITEM(swap, " D") } '' Kill X { ITEM(kill-pane) } Reconnect R { respawn-pane -k } "#{?#{>:#{window_panes},1},,-}#{?window_zoomed_flag,Unzoom,Zoom}" z { ITEM(zoom) }`
      (for swap the direction follows the pane id: `ITEM` renders
      `… swap ##{q:@bridge_pane} U`).
    - `MouseDown3Pane` mirror: `if-shell -F -t = "#{||:#{mouse_any_flag},#{&&:#{pane_in_mode},#{?#{m/r:(copy|view)-mode,#{pane_mode}},0,1}}}" { select-pane -t = ; send-keys -M } { <pane menu M> }`.
      `M-MouseDown3Pane` mirror: `<pane menu M>`.
    - session menu (both StatusLeft keys): `run-shell -C "display-menu -t= -xM -yW -T '#[align=centre]#{session_name}'  #{S/t:#{?#{&&:#{<:#{loop_index},6},#{!:#{session_active}}},'Switch To #[underscore]#{session_name}' '' {switch-client -t=#{session_id}#} ,}} '' 'Renumber' 'N' {move-window -r} 'Detach' 'd' {run-shell -b '<og-remote-detach> ####{qs:session_name}'} '' 'New Session' 's' {new-session} 'New Window' 'w' {run-shell 'E2(CTL) new-window ####{q:@bridge_pane}'}"` —
      `<og-remote-detach>` is `p.Scripts["og-remote-detach"]`, as the `d` bind uses.
    - empty menu (both Empty keys): `display-menu -T "#[align=centre]#{window_index}:#{window_name}" -t = -x M -y M "New Window" w { ITEM(new-window) }`.
  - Bind lines, in stockmenus order:
    `bind-key -N 'Display window menu' -T prefix < if-shell -F 'GATE' { <window P> } <quoted stock>`,
    `bind-key -N 'Display pane menu' -T prefix > if-shell -F 'GATE' { <pane P> } <quoted stock>`,
    and for each root key `bind-key -T root <key> if-shell -F -t = 'GATE' { <mirror> } <quoted stock>`.
  - `menuBinds(p)` = `%if "#{==:#{version},` + stockMenuVersion + `}"` + `\n` +
    the ten lines joined by `\n` + `\n%endif` (no trailing newline).
  Run the Step 2 command → passes.

- [ ] **Step 4: failing-then-passing mirror-branch invariants** in `menus_test.go`:
  First add `"og-remote-detach": "/store/og-remote-detach"` to `keysPaths()`'s
  `Scripts` in `keys_test.go` — without it the session menu renders an empty
  detach path unnoticed.
  - `TestMirrorBranchesRunNoLocalStructuralCommand`: take each bind line's mirror
    branch (the text between the gate and the quoted stock). A `run-shell -C "…"`
    argument is a tmux command, not a shell string: replace it by its unquoted
    content and keep checking (this is where the whole session menu lives).
    Then delete every remaining non-`-C` `run-shell "…"` / `run-shell '…'` /
    `run-shell -b '…'` argument, and assert none of `kill-window`, `kill-pane`,
    `split-window`, `new-window`, `new-pane`, `rename-window`, `rename-session`,
    `swap-pane`, `respawn-window`, `select-pane -m`, `break-pane`, `join-pane`,
    `move-pane`, `detach-client`, `resize-pane` appears as a word, and that every
    `swap-window` is followed by `-t :-1` or `-t :+1`. A self-check subtest feeds
    it a branch containing `run-shell -C "display-menu X x {rename-session}"` and
    expects a failure, so the -C recursion cannot silently regress.
  - `TestMirrorCtlItemsUseTheKeybindEntryPoint`: every non-`-C` `run-shell` in a
    mirror branch of the window, pane and empty menus starts `run-shell "` +
    esc1(bridgeCtl(p)) + ` `; the verb that follows is in
    `{kill-window, kill-pane, rename, split-h, split-v, swap, zoom, new-window}`
    (the verbs the keybinds already send; Step 8 test 9 cross-checks them against
    `ctl.go`'s table).
  - `TestSessionMirrorMenuEscapesTwice`: both StatusLeft mirror branches contain
    exactly `{run-shell -b '/store/og-remote-detach ####{qs:session_name}'}` and
    `{run-shell '` + esc2(bridgeCtl(p)) + ` new-window ####{q:@bridge_pane}'}`,
    and contain neither `'Rename'` nor `rename-session` nor `detach-client`.
  - `TestEmptyMirrorMenuHasNoNewPane`: both Empty mirror branches contain
    `"New Window" w` and not `New Pane` / `new-pane` / `join-pane`.
  - `TestMirrorRenameMatchesTheKeybindAfterBuildExpansion`:
    `strings.ReplaceAll(<rename item command>, "##", "#")` equals
    `command-prompt -I'#{@window_bridge_name}' { run-shell "` + bridgeCtl(p) +
    ` rename #{q:@bridge_pane} #{qs:1}" %1 }` — the `,` keybind's mirror branch
    (`config/tmux.conf.tmpl:150`), held as a literal in the test because the
    generator's Nix build sees only `generator/`; the comment names that line.
  - `TestKeyboardAndMouseMenusShareItems`: prefix and mouse window/pane mirror
    menus are equal after removing `-t = ` and normalising `-x/-y`.
  Run: `cd generator && go test ./render/` → passes.

- [ ] **Step 5: wire the template.** `render.go`: `MenuBinds string` in `Data`
  (next to `CarouselHooks`), `MenuBinds: menuBinds(p)` in `Build`.
  `config/tmux.conf.tmpl`, directly after `set -g detach-on-destroy off`:
  ```
  # tmux's own default menus, re-bound so that on a mirror window every
  # structural item goes through the bridge's ctl verbs and acts on the remote
  # (#769; classification in docs/agents/bridge-daemon.md). The non-mirror branch
  # is tmux's stock command verbatim, as a string so a same-version server that
  # lacks one of its commands fails that menu, never the config load; the %if
  # keeps any other server version on its own stock menus (#407).
  {{.MenuBinds}}
  ```
  Run: `cd generator && go test ./...` → passes; `nix build .#default` → builds;
  then on a scratch server (`TMUX_TMPDIR=/tmp/og-$$`, `-L probe`) from the built
  wrapper: `list-keys -T prefix '<'` contains `-T prefix < if-shell -F` and `list-keys -N -T prefix` shows `Display window menu` / `Display pane menu`,
  and `show-messages` has no parse error from the config.

- [ ] **Step 6: the reference oracle.** In `config/tmux.conf.reference.nix` add
  a `menuBinds` let-binding, built in Nix from
  `builtins.readFile ../generator/render/stockmenus.txt` (split on `\n`, drop the
  empty last element, strip each line's `bind-key -T <table> <key>` prefix and
  padding with `builtins.match`), a Nix `quote` (replace `\`, `"`, `$` then wrap
  in `"`), `esc1`/`esc2` via `builtins.replaceStrings ["#"] ["##"]`, and the
  mirror texts of Step 3 written as Nix strings (`"…"` strings, where `${` needs
  `\${`, never `''…''` strings, whose `''` terminator collides with the menus'
  empty-label `''` tokens). Interpolate `${menuBinds}` in `keysText` after
  `set -g detach-on-destroy off`, preceded by the same comment lines, indented
  like its neighbours. Run: `nix flake check` extraction check (or
  `nix build .#checks.x86_64-linux.<extraction check name>` — find it with
  `grep -n "differs from the frozen reference" flake.nix`) → passes.

- [ ] **Step 7: failing scanner fixture, then the scanner fix.**
  `tests/conf-shell-quoting.bats`: add to the "known-good" fixture test a line
  `bind M run-shell -C "display-menu -T '#{session_name}' Foo f {new-window}"`
  (must NOT be flagged: `-C`'s argument is a tmux command), and to the bad
  fixture `bind N run-shell -C "display-menu Foo f {run-shell '/bin/x #{session_name}'}"`
  (MUST be flagged: the nested run-shell's shell string). Run
  `bats tests/conf-shell-quoting.bats` → the good line fails. Fix `walk_tokens`'
  `run-shell)` branch: note whether the flags include `-C`; if so, recurse into
  the argument as a command line — `tmux_tokenize "$arg"; walk_tokens "$lineno"
  "${TOKENS[@]}"`, the same two calls the `*)` branch makes for a nested command
  string — instead of `scan_shell_string`. (`TOKENS` is global, so capture the
  argument before re-tokenizing, and restore nothing: the caller's `_toks` is a
  local copy.)
  Re-run → passes, including the check over the emitted conf (the test's
  integration half; find how it gets the conf with `grep -n TMUX_CONF tests/conf-shell-quoting*.bats flake.nix`).
  Run that emitted-conf half against the Step 5 build before moving on: the
  stock session menu's `#{S/t:…'Switch To…' '' {switch-client -t=#{session_id}#} ,}}`
  now goes through `tmux_tokenize` with quotes and braces inside a format, and a
  false positive there must be fixed in the tokenizer, not by exempting the line.

- [ ] **Step 8: the behavioural bats file** `tests/menu-bind-integration.bats`,
  copying the harness of `tests/rename-bind-integration.bats` (setup/teardown,
  `inner`/`outer`, `start_recorder`, `attach_client`, `send`, `press_prefix_key`,
  `screen`, `wait_for_frame`, `clear_frames`, `sole_payload`, sentinel poll) —
  copy, do not source, matching that file. Add an argv writer generalised to any
  verb: `write_argv file verb args…` → `<ver>\0<verb>\0%42[\0arg…]`, and
  `click btn col row` = `outer send-keys -t "$OPANE" -H $(printf '\e[<%d;%d;%dM' … | od -An -tx1)`
  (raw bytes: `-l` would re-encode ESC under the client's extended keys).
  Menus are opened, then an item is chosen by its key (`send X`). Tests:
  1. `prefix <` then each of `X`, `w` → frames `kill-window %42`, `new-window %42`;
     `n` → prompt `(run-shell) <name>` → Enter → `rename %42 <name>`.
  2. `prefix >` then each of `h`, `v`, `X`, `z` → `split-h`, `split-v`,
     `kill-pane`, `zoom`; with a second local pane in the mirror window
     (`split-window` directly on the inner server, stamping `@bridge_pane` on
     it too) `u`/`d` → `swap %42 U` / `swap %42 D`. After every case: local
     `list-windows`/`list-panes` counts unchanged.
  3. Mouse: right-click in the pane area (`click 2 20 20`) → pane menu → `X` →
     `kill-pane %42`. Right-click on the current window's tab (locate the row and
     column from `screen`, as the status is 2 lines at the top here) → `X` →
     `kill-window %42`.
  4. Hostile rename: `@window_bridge_name` = `a'b;c#d}e f#(touch <sentinel>)`;
     `prefix <`, `n`, Enter → the wire argv carries it byte-exact and the sentinel
     is never created (bounded poll, as the rename test).
  4b. Session menu: right-click the session name on status row 1 (the
     `#[range=left]` pill; locate its column from `screen`) → the menu shows
     `Detach`, `New Window`, `Renumber` and no `Rename`; `w` → frame
     `new-window %42` and the local window count is unchanged. Reopen it, `d` →
     `og-remote-detach` runs: with no daemon pid file beside `@bridge_sock` it
     falls back to `kill-session`, so `inner has-session -t =s` turns false
     within 5s and no ctl frame was recorded (a plain `detach-client` would leave
     the session alive). Keep a second inner session (`keep`) alive for this test
     and assert it survives: with `s` the only session, `kill-session` would end
     the server and a crash would read as a pass.
  5. Hidden/relabelled: after `prefix <` the screen contains `Kill` and
     `Rename` and not `Respawn`, `Mark`, `Swap Marked`, `New After`; after
     `prefix >` it contains `Reconnect` and not `Respawn`, `Mark`, `Float`.
  6. Non-mirror parity: clear `@bridge_win` on a second, plain window; `prefix <`
     → `X` → that local window is gone and no frame was recorded; `prefix >`
     shows `Respawn` and `Mark`.
  7. Stock tripwire: for each stockmenus line's table/key, the raw pinned server
     (`$TMUX_RAW -f /dev/null -L <own socket>`) prints exactly that line
     (`diff`); and its `#{version}` equals the version in the conf's `%if` line.
  8. Version gate: extract the `%if … %endif` block from `$CONF`; source it into
     a raw `-f /dev/null` server → `list-keys -T prefix '<'` contains `if-shell -F`;
     the same block with the version literal changed to `next-0.0` sourced into
     another raw server → all ten `list-keys` lines equal stockmenus.txt.
  9. Verb cross-check: every verb the conf's menu block sends appears as a
     `"<verb>": {` key in `$CTL_GO`.
  Run locally: `TMUX_BIN=… TMUX_RAW=… CTL=… CTL_PROTOCOL_VERSION=… CONF=… STOCK_MENUS=generator/render/stockmenus.txt CTL_GO=picker/remotebridge/daemon/ctl.go bats tests/menu-bind-integration.bats` → all pass.
  Red check: temporarily render the stock command in the mirror branch too
  (scratch copy of the conf, not the tree) and confirm tests 1–5 fail.

- [ ] **Step 9: the flake check.** In `flake.nix`, add
  `menu-bind-integration-tests` beside `rename-bind-integration-tests`, same
  reduced conf (`enrichEnable = false; agentUsageEnable = false;`) and
  `nativeBuildInputs`, plus env `TMUX_RAW = "${mkTmux pkgs}/bin/tmux"`,
  `CONF = "${<that conf>.tmuxConf}"`, `STOCK_MENUS = ./generator/render/stockmenus.txt`,
  `CTL_GO = ./picker/remotebridge/daemon/ctl.go`, and the same
  `CTL_PROTOCOL_VERSION` extraction. Run
  `nix build .#checks.x86_64-linux.menu-bind-integration-tests -L` → passes.

- [ ] **Step 10: docs.** `docs/agents/bridge-daemon.md`: a "Menus in a mirror
  window (#769)" section after "Mirror invariants" holding the spec's inventory
  and four classification tables, the expansion-layer rule, the `%if` +
  string-branch rule, and the "right-click on a non-current tab opens nothing on
  next-3.9" measurement; in the #547 bullet replace "tmux's default Respawn binds:
  `prefix + <`, `prefix + >`, and both right-click pane menus" with the pane
  menus' Reconnect item (the window menu no longer offers Respawn on a mirror).
  `README.md` bridge paragraph: "structural keybinds and tmux's right-click /
  `prefix <`/`>` menus inside a mirror window act on the remote." Run
  `nix build .#lint` → passes (typos, markdown hooks).

- [ ] **Step 11: full gate.** `nix build .#default && nix flake check && nix build .#lint` → all pass.

## Acceptance

| Task acceptance item | Settled by |
|---|---|
| Menu Kill window / kill pane close the REMOTE window/pane and the mirror follows; menu rename renames the remote window | Step 8 tests 1–4 (ctl frames at the socket boundary) + the daemon's existing `TestParseCtlVerbTranslation` for those verbs + the end-to-end `--test-local` scratch repro the worker records in the PR body (or states it could not be driven) |
| Non-mirror menus behave exactly as before (conf assertion in `nix flake check`) | Step 2/3 `TestMenuStockBranchIsVerbatim`, Step 8 tests 6–8 in the `menu-bind-integration-tests` check |
| Classification table committed in `docs/agents/bridge-daemon.md` | Step 10 |
| `nix build .#default`, `nix flake check`, `nix build .#lint` pass | Step 11 |
