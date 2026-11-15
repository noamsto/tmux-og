# Plan: reseed restores cursor visibility, shape/blink, colour, key mode (#1104)

Verified against tmux master source (format.c, input.c, input-keys.c, colour.c) and live next-3.9:
- `cursor_shape` ∈ block|underline|bar|default (no spaces). `cursor_blinking` ∈ 0|1.
- `cursor_colour`: `none` (unset, both ccolour and default -1), else `#rrggbb`, `colourN`, or a colour name (`default`, `terminal`, `red`...). `#{…}` value is never re-expanded.
- `pane_key_mode` ∈ `VT10x` | `Ext 1` | `Ext 2` — **contains a space**, so strings.Fields would split it. Use `k#{s/ //:pane_key_mode}` → `kVT10x|kExt1|kExt2` (empty-expanding old tmux leaves bare `k`, still one field).
- Key mode DOES matter: local tmux `input_key` encodes keys for the renderer pane by that pane's `s->mode & EXTENDED_KEY_MODES` (input-keys.c ~690); renderer forwards stdin verbatim, `pumpInput` send-keys -H to remote. A renderer in VT10x sends `\r` for Ctrl-Enter where a remote Ext-2 app expects `CSI 27;5;13~`. Restore via `CSI > 4 ; n m` (input.c INPUT_CSI_MODSET: m==2 → Ext2, m==1 → Ext1, m==0 → clears). Local `extended-keys` option off → no-op (fine). Kitty keyboard flags are not reported by `pane_key_mode`: out of scope, noted in docs.
- OSC 12 (`colour_parseX11`) sets per-screen ccolour; OSC 112 (no arg) resets to -1. DECSCUSR: 0 default, 1/2 block blink/steady, 3/4 underline, 5/6 bar.

## Design
Wire: add prefixed fields to `cursorCmd`, appended after `#{pane_private_modes}` so existing positional mouse/modes rules are untouched: ` s#{cursor_shape} b#{cursor_blinking} c#{cursor_colour} k#{s/ //:pane_key_mode}`. `parseCursor` plucks by first-byte prefix (w, s, b, c, k) from `fields[4:]` (extend the existing wrap loop to a switch), returning the extras as one `*render.Extra`; prefixed fields never enter `rest`. A positional field can't start with s/b/c/k/w (flags are 0/1, list is digits/commas).

Render: new `render.Extra{Style *CursorStyle; Colour *string; KeyMode *int}` and `CursorStyle{Shape string; Blink bool}`; `TerminalModes` gains `CursorVisible bool` (from `privateModeSet(list, 25)`, atomic with 2004/1004: same list). `Seed` gets one trailing param `extra *Extra` (nil = unknown, leave as-is); all existing call sites updated with `nil`.
Preamble (before 2J, after wrap/modes/mouse): DECSCUSR (only when Style non-nil; unknown shape string → skip; shape `default` → `CSI 0 SP q`, blink dropped), colour (sanitized daemon-side to `[A-Za-z0-9#]+`, else unknown/nil): `#rrggbb` → OSC 12 `\x1b]12;<c>\x1b\\`; `none` and any other value (colourN, names, default/terminal) → OSC 112 reset, since the local tmux's OSC 12 parses only #rrggbb/rgb:/X11 names; `CSI > 4 ; n m` for key mode (VT10x→0, Ext 1→1, Ext 2→2, other→nil).
Visibility: written LAST, after the final CUP (`?25h`/`?25l`), only when modes != nil. Nothing after Seed's tail (the predictor's `onSeed` feed, repaint) re-shows the cursor except predictor `reset()`/model bytes: verify predictor/hold do not emit `?25h` after a seed (predictmodel.go bytes() only emits `?25l` when hidden).

## Steps
1. render/snapshot.go: types, Seed param, preamble writers, trailing visibility. Tests in render/snapshot_test.go: each state set / cleared / nil-untouched (visible/hidden; every shape×blink→DECSCUSR n; default→0; colour set/none→112/nil; key mode 0/1/2/nil; visibility is the final bytes after CUP).
2. daemon/seed.go: cursorCmd, parseCursor (8th return `extra`, a `*render.Extra`), PaneSeeds passes it; predictseed.go and main.go call sites; update existing tests' parseCursor/Seed calls and the exact-string cursorCmd test; `TestPaneSeedCarriesWrapThroughSeed` now ends in CUP followed by `?25h` when the mode list carries 25. A predictor test feeds a seed with the new preamble to `newPredictModel` and checks the model is not poisoned and the trailing `?25l` hides its cursor. New parseCursor tests: full new reply; each of s/b/c/k fields missing; `cnone`; spaceless key field; colour with unsafe chars → nil; older-tmux reply with none of the new fields keeps extra nil.
3. daemon seed test (live-reply style, like seed_test.go mouse test): PaneSeed on a reply with hidden cursor+bar → bytes contain DECSCUSR 6 and `?25l`.
4. bats test in tests/remote-m2-integration.bats, copying the pre-attach pattern of the paste/focus test: remote pane runs `printf '\e[?25l\e[6 q'` BEFORE the daemon attaches (poll `$SRC` until `#{cursor_flag} #{cursor_shape}` = `0 bar`), then a `wait_mirror_cursor` helper (like `wait_mirror_paste_focus`) expects the mirror pane to read `0 bar`.
5. docs/agents/bridge-daemon.md: extend the #804 seed paragraph with the new fields, prefixed-field rule, key-mode rationale (local tmux encodes keys per renderer-pane mode; tmux.conf sets extended-keys on; kitty flags not covered), colour/shape rules, visibility-last rule.
6. Gates: `go test -race ./...` in picker, `nix build .#lint --accept-flake-config`, `nix flake check`, `nix build .#default`.

Not touched: hold.go, predictor behaviour.
