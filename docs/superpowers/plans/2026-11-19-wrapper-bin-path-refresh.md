# Refresh the wrapper bin on the server PATH after a config reload (#1207)

## Mechanism

`wrapProgram --prefix PATH` is evaluated by the client that starts the server, so
the server's global `PATH` keeps the old `*-tmux-wrapper-bin/bin` store path after
a home-manager switch. A reload (`source-file`) rebinds keys to new store paths
but never touches the global `PATH`; panes created from a key bind or hook resolve
bare script names against the stale dir.

Measured on `next-3.9` (private socket): `set-environment -g` changes what
bind/hook-created panes get (a control client issuing `new-window` from inside the
server got the new global `PATH`); an external `tmux new-window` client's own env
wins, which is unchanged tmux behaviour and not what this fixes.

## Chosen approach (design twice)

- **A. Conf rewrites the global PATH on load** (chosen): config-time `$PATH`
  expands from the server's global environment; strip every
  `*-tmux-wrapper-bin/bin` segment with a nested `#{s|…||:}` format and prepend
  the current dir. Idempotent across reloads, one fork-free tmux-native step,
  same on Linux and macOS.
- B. A stable symlink path updated on activation: needs a mutable path outside
  the store, a home-manager activation step and a non-HM story; a fresh nix-build
  of the flake alone would not refresh anything. Rejected.

Format constraints found while prototyping: a `:` inside an `s|…|…|` pattern
splits the enclosing format modifier, so the patterns use
`[^-._+/a-zA-Z0-9]` for the separator; `$|` is literal (tmux only expands
`$` before a name character).

## Exact conf text (tmux `next-3.9`; `set-environment -F` exists since 3.2)

Emitted by `tmux.conf.tmpl` inside `{{if .Paths.WrapperBin}}…{{end}}` right after
the `BROWSER` line, and unconditionally (with `${wrapperBinDir}/bin`) at the same
spot in `tmux.conf.reference.nix`, byte-identical when rendered:

```
set -g @_og_path ":$PATH"
set-environment -gF PATH "<wrapper>/bin#{s|[^-._+/a-zA-Z0-9][-._+/a-zA-Z0-9]*-tmux-wrapper-bin/bin$||:#{s|[-._+/a-zA-Z0-9]*-tmux-wrapper-bin/bin[^-._+/a-zA-Z0-9]||:@_og_path}}"
set -gu @_og_path
```

Inner `s` removes every segment that has a following separator; outer removes a
last segment with its preceding separator. `$PATH` is expanded by the config
parser before format expansion; `-F` is required or the literal format is stored.
The leading `:` on the copy lets a lone wrapper segment match. The value is
substituted literally; only the patterns must avoid `:`, `,`, `}`, `#`. `generatedConf`
now follows `wrapperBinDir`'s hash; no cycle (no wrapper package refers to the conf).

## Steps

1. `generator/paths/paths.go`: optional `WrapperBin *string` (`wrapper_bin`), not
   in `optionalBinNames` or the `FromPrefix` probe map, so prefix mode leaves it
   nil (no `-tmux-wrapper-bin` segment to dedupe against; re-prepending would
   stack). `paths_test.go`: `optionalsOf` gains it (prefix mode asserts nil) and
   the present-fixture gains `wrapper_bin`.
2. `config/tmux.conf.nix`: `pathsToml` gains `wrapper_bin`; `wrapperBinDir` is
   passed to the reference import together with step 3 (its formals are closed).
3. `tmux.conf.tmpl` and `tmux.conf.reference.nix`: the block above.
4. `tests/wrapper-bin-path.bats` + flake check `wrapper-bin-path-tests`
   (`testsSrc ["wrapper-bin-path.bats"]`; vars `TMUX_BIN` = tmux-wrapped,
   `RAW_TMUX` = head of `wrapperBinPkgs`, `NEW_BIN`, `CONF` = generated conf; one
   import, the old dir is a fake PATH segment). Setup exports all five isolation
   vars. Cases: (a) raw server started with an old segment first, `source-file
   $CONF` twice, a pane spawned by a `-C attach` control client (a bare command
   client spawns with its own PATH) has exactly `NEW_BIN`, first, old gone;
   (b) old segments in the middle and at the end; (c) fresh start through
   tmux-wrapped has exactly one, first.
   Template layout: `{{if .Paths.WrapperBin}}` opens on the first comment line
   and `{{end}}` sits on the `# Timing` line (house style), so the block adds
   exactly one blank line on and none off.
5. `tmux-conf-extraction-assertions`' `--prefix` smoke: line-count equality
   subtracts the block (plus its blank line) from the reference, and asserts the
   prefix render has no `set-environment -gF PATH`.
6. `docs/agents/tmux-gotchas.md`: section with the mechanism, format traps, and
   that existing panes and bare-client spawns keep the old PATH.

## Acceptance

- [ ] bats test: pane created after reload has the new wrapper dir, no old one.
- [ ] fresh server start: exactly one `tmux-wrapper-bin/bin`, at the front.
- [ ] `nix build .#lint --accept-flake-config`, `nix flake check --no-build`,
      and the real checks (`wrapper-bin-path-tests`,
      `tmux-conf-extraction-assertions`, full `nix flake check`).
