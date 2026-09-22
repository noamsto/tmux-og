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


## Launch

- **The picker itself launches in a popup-float** (`display-popup`'s compat
  modal-pane form, #725 — see `floats.md`'s "Popups are modal floats"): its
  own window/session targets are never captured by the preview or the wall,
  `selfCaptureTarget` (`picker/capture.go`) resolving them to the pane under
  the float instead so a tile for the current window shows the window's real
  content, not the picker looking at itself. Killing the picker's *own*
  window (from the window picker) or session (from the session picker) takes
  the picker down with it — the popup used to survive and refresh; a
  popup-float cannot, since it lives inside the window it just killed.
  Accepted, not worked around: the action still completes, and the end state
  is the one any picker that closes after acting leaves behind. Launchers
  pass `-t "<client>:"` alongside `-c "$CLIENT"` — the float lands in the
  `-t` window, and unpinned that defaults to tmux's "best" session rather
  than the client's own.

## Input and filtering

- **A typed key is `printableKeyText`, never `len(key) == 1`** (`picker/tui.go`). bubbletea v2 reports the space key by its NAME — `KeyPressMsg.String()` falls through to `Keystroke()` for it, "the only invisible printable character" — so a length test silently dropped every space typed into a query and no picker filter could express `new window` (#689). The helper returns the literal character, which is what its three consumers need: the session/window picker and which-key append it to the query, and `relayKeyArgs` (`picker/capture.go`) sends it to a pane with `send-keys -l`, where widening the length test alone would have typed the word `space`.
- **`^t` marks a Remote-section session row for multi-open (#730), never Tab** — Tab drives host-scope cycling instead (below), and a printable key belongs to the query (`printableKeyText`), not a mark. `markable` (`picker/tui.go`) gates it to a resolvable session row: host rows, local sessions, zoxide rows, any row Enter itself can't act on (needs-auth, host-key-changed, tailscale-check, `remoteUnreachable` — a cached row of a host the probe just confirmed down), and any row already carrying a live mirror (`remoteMirrorTarget != ""`, see below) are all no-ops. A mark is keyed by `item.target` ("remote:\<host\>:\<sess\>"), the same key `restoreCursor` matches a cached row against its live replacement by (#631), so marking a session before its host's probe answers survives the cached→live swap, and survives typing a query that hides the row entirely — marks are a selection independent of the filter, not a property of what's on screen. **Marks resolve against the always-unscoped `m.sessionItems`+`m.remoteItems`, never `m.allItems`**: `markedRemoteItems` (`tui.go`) — since host/all scope shrinks `m.allItems` to one (or all-but-local) host's rows, resolving against it would silently drop a mark set on a host that isn't the current scope the moment Tab cycles away; a mirrored row can never appear in this set because `markable` excludes it and `m.remoteItems` never contains the synthesized mirror rows in the first place (those exist only in `scopedItems`' output). `renderList` draws a marked row with a tinted `✓` in the gutter column (sized like every other column glyph, never `len`); `renderHints` turns `enter`'s label into `open <n>` and adds a `^t:mark` hint (highlighted, with the count) once any row is marked. Enter with ≥1 mark ignores the cursor row entirely and opens every marked session instead: `openMarkedRemoteWith` (`markedRemoteItems`'s list-order result, testable via injected `open`/`launch` functions) runs the first through `openRemoteBridge` unchanged — one foreground `og-remote-open`, the same cost a single Enter always paid, and the session the client switches to — and every other marked session through `launchRemoteBridgeDetached`, fired and not waited on (`Setsid: true`, matching how `og-remote-open.sh` itself detaches the bridge daemon it starts) so N marks cost one serialized ssh dial instead of N. Esc clears marks first, before its usual clear-query-then-quit chain, when any are set.
- **Tab cycles a host scope that filters what `recombine()` puts in `allItems`** (#733): `local → host₁ → host₂ → … → hostN → all hosts → local`, in `configuredHosts` (`picker/remote.go`) order — the same `@remote_bridge_hosts` list, self-aliases dropped, that Tab's cycle and `scopeAll`'s membership both use. `nextScope` (`tui.go`) computes the next `hostScope{kind, host}`; `tuiModel.scope` resets to its zero value (`scopeLocal`) on every fresh popup launch, so a relaunch never inherits a prior scope. `handleKey`'s `case "tab":` no-ops when `m.windowMode` or `m.emitPath != ""` (neither carries remote data to scope) or when `configuredHosts` is empty (nothing to cycle to). Host scope's row set (`scopedItems`): that host's already-mirrored local sessions filtered out of `m.sessionItems`, plus that host's Remote-section block reused **verbatim** from `m.remoteItems` (`remoteHostBlock` walks it by adjacency — row + immediately-following children — never rebuilt from cache) so every existing inert-state invariant (no children for needs-auth/host-key-changed/tailscale-check hosts) carries over unchanged, plus appended `"(mirrored)"` rows for that host's sessions already mirrored locally (from `m.mirrors`, populated off-thread each tick by `collectBridgeMirrors`/`parseBridgeMirrors` in `refreshDataCmd`, gated on `!windowMode`) — a mirrored row's Enter goes through the `switchClient` seam to the mirror's local session instead of opening a duplicate, regardless of that host's probe state (a plain `switch-client` never touches the remote). All-hosts scope assembles the same per-host blocks across every `configuredHosts` entry, host-block by host-block rather than a flat merge, so `hostColorFunc` tints stay visually grouped and no host's mirror rows interleave into another host's block. No zoxide rows in host/all scope — a "create a session here" suggestion has no host to scope to. The search row shows a scope badge (`withScopeBadge`, `render_list.go`: host name tinted via `hostColorFunc`, or "all hosts" in the peach highlight; nothing for local scope), and the footer's `⇥:scope` hint (highlighted once `m.scope.kind != scopeLocal`) is gated on `len(configuredHosts(m.tmuxOpts)) > 0`.
- **The which-key popup's filtered list is flat and score-ranked; its unfiltered list is grouped.** Grouping and ranking cannot both hold, and a search wants its best hit on line 1 rather than wherever its key table happens to fall — so `rebuildVisible` (`picker/whichkey.go`) drops the `── prefix ──` headers under a query and moves the table into a per-row column. 298 of a stock 434-bind server carry no `-N`, so their "note" is the raw `key_command`, and a query subsequence-matching a `/nix/store` path the column never shows returned 20 rows for `float` of which 2 were the floating-pane binds. `describedRank` sorts every real description above every command, rather than dropping the command rows — a plugin bind stays reachable by its command text, just below. Table headers carry a plain-words gloss (`whichKeyTableGloss`) because `prefix`/`root`/`move` are tmux's jargon, not descriptions; the prefix key is read live from the `prefix` option, and an unknown table gets no gloss rather than a guessed one.
