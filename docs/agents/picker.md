# Picker (session / window / which-key TUI)

Layout and input invariants of the Go bubbletea pickers under `picker/`.

## Picker Chrome (header, sticky line, sections)

- **The session list's column labels are glyph + word** (`@icon_host`,
  `@icon_procs`, `@icon_cpu`, `@icon_mem`, added to the `icons` attrset like the
  rest). Every cell is sized with `visibleWidth`, never `len`: a nerd glyph is
  one cell and four bytes, so `len`-based padding puts each column three cells
  short. **Each label sets a floor under its column's width** — the columns are
  otherwise sized by their data alone (Host is as wide as the widest
  `@bridge_host`), and a label wider than its cell pushes every later column
  right in the header only. The CPU and Mem labels mirror each other around the
  ` / ` (CPU padded left, Mem padded right) so the pair reads as one unit;
  padding never changes a field's total width, or Path drifts. The alignment
  tests assert the ` / ` and the path glyph land on the same cell in the header
  and in a row, which is the regression net for both mistakes.
- **One pinned line** (`governingHeaderIdx` + `renderHeaderItem`) carries
  whichever header governs the rows beneath it — the column glyphs through the
  session list, the divider once the window starts inside Remote or New session,
  the group row in window mode. One line, not a column header stacked above a
  section header: this is a popup that rarely has twenty rows to give. At the top
  of the list the pinned row *is* `visible[start]`, so the body advances past it
  rather than drawing it twice; the rendered height is therefore constant across
  scroll, which a test asserts.
- **Section dividers are rebuilt at render width.** The collectors emit
  `"── Remote " + 220 dashes` and cannot do better — they do not know the popup
  width — so they now carry `headerLabel`/`headerIcon` instead and
  `renderHeaderItem` composes glyph + label + a rule that ends exactly at the
  edge. `isColumnHeader` marks the one header that is not a section, so the pin
  passes its own `display` through untouched.
- **`readTmuxOpts`** reads `tmux show -g -F '#{option_name} #{option_value}'`
  directly — the custom `-F` format bypasses `show -g`'s default quote-escaping
  template, so `#{option_value}` is already raw. An option set to the empty
  string now reads back as an empty string with no quote-stripping needed
  (previously `show -g` printed it as `''`, which is what `unquoteTmuxOptValue`
  used to unwrap; that helper is gone).


## Input and filtering

- **A typed key is `printableKeyText`, never `len(key) == 1`** (`picker/tui.go`). bubbletea v2 reports the space key by its NAME — `KeyPressMsg.String()` falls through to `Keystroke()` for it, "the only invisible printable character" — so a length test silently dropped every space typed into a query and no picker filter could express `new window` (#689). The helper returns the literal character, which is what its three consumers need: the session/window picker and which-key append it to the query, and `relayKeyArgs` (`picker/capture.go`) sends it to a pane with `send-keys -l`, where widening the length test alone would have typed the word `space`.
- **The which-key popup's filtered list is flat and score-ranked; its unfiltered list is grouped.** Grouping and ranking cannot both hold, and a search wants its best hit on line 1 rather than wherever its key table happens to fall — so `rebuildVisible` (`picker/whichkey.go`) drops the `── prefix ──` headers under a query and moves the table into a per-row column. 298 of a stock 434-bind server carry no `-N`, so their "note" is the raw `key_command`, and a query subsequence-matching a `/nix/store` path the column never shows returned 20 rows for `float` of which 2 were the floating-pane binds. `describedRank` sorts every real description above every command, rather than dropping the command rows — a plugin bind stays reachable by its command text, just below. Table headers carry a plain-words gloss (`whichKeyTableGloss`) because `prefix`/`root`/`move` are tmux's jargon, not descriptions; the prefix key is read live from the `prefix` option, and an unknown table gets no gloss rather than a guessed one.
