# Link picker that rejoins hard-wrapped URLs (#1068)

## Problem

Claude Code wraps its own output at the pane width and writes each row as a
hard line, so tmux never marks the break as a soft wrap. tmux-fingers matches
one captured line at a time (joining only tmux's own soft wraps via
`capture-pane -J`), so a URL or path longer than the pane reaches it as two or
more fragments, and only the first fragment can be picked. Upstream fingers will
not do multi-line matching (Morantron/tmux-fingers#163).

## Measurements (live server, tmux `next-3.9`)

| pane | renderer | width | what a long token looks like |
|---|---|---|---|
| `%385` | Claude Code | 62 | `  All four verdicts are in /tmp/claude-1000/-home-noams-git--w` / `  orktrees-git-…-false-posi` / `  tives-on-re/…/scratchpad/` / `  (verdict.json through verdict4.json).` |
| `%761` | Claude Code | 62 | `  /tmp/claude-1000/-home-noams-git--worktrees-git-tmux-og-feat` / `  -1035-keep-relay-graphics-and-cell-env-correct/beb63a10-bc61` |
| `%754` | pi | 91 | ` /home/noams/git/.worktrees/git/dispatcher/feat-1438-…-and-dispa ` / ` tch` |
| `%611` | Claude Code (Monitor event body) | 154 | `…is bounded b` / `y bytes…` (no indent) |

- **Claude Code breaks a token wider than the remaining row mid-token at exactly
  the pane width**, wherever the width falls (mid-word, before `-`, after `/`); it
  does not prefer `/`, `-` or `?`. Prose is word-wrapped at spaces (rows usually end
  short of the edge). Continuation rows of an assistant paragraph start with two
  spaces; event bodies with none. `capture-pane -p` and `-p -J` give identical rows.
- **pi keeps a one-cell margin on both sides** and pads rows with spaces: text
  ends at column `width-1`, continuation rows start with one space.
- **A token that ends exactly at the edge followed by a word-wrapped row is
  indistinguishable from a mid-token break by geometry alone**: `%385`'s
  `…/scratchpad/` ends at column 62 and the next row is `(verdict.json …`, which
  was a space break. The continuation-start rule (below) is what keeps that row out.
- **OSC 8 is partly available.** `capture-pane -e` keeps OSC 8 on this tmux
  (`terminal-features *:hyperlinks`). Claude Code emitted one for the PR link in its status line
  (`%384`/`%385`: `ESC]8;id=…;https://github.com/noamsto/hookyard/pull/267`), none
  for bare paths or for `%761`/`%788`. So hyperlinks are a supplementary source,
  never the primary one.
- **Bridge mirror panes** (`@bridge_win`): the renderer forwards live `%output`
  bytes, so the local tmux soft-wraps live output and `-J` rejoins it; seeded and
  reseeded rows are written as hard CRLF rows with no added indentation
  (`picker/remotebridge/daemon/seed.go` `captureCmd`, `render/snapshot.go`;
  `docs/agents/bridge-daemon.md`, "seeded rows are hard-CRLF"). A mirror after any
  reseed therefore shows the same zero-indent hard breaks as `%611`.

## Design

### Decisions

- **Applies to every pane**, not only agent panes. The rejoin fires only when a
  link token runs to the right edge and the next row continues it, so plain shells
  (whose wraps `-J` already joins) are unaffected, while pi panes and reseeded
  mirror panes, which hard-break the same way, benefit. No `@window_has_agent`
  dependency.
- **UI: a bubbletea list popup** (the which-key pattern: `--links` mode of
  `tmux-picker-generate`, a standalone model in the `main` package, launched by a
  script into a popup-float). Rejected: a fingers-style hint overlay. A rejoined
  link spans several rows, so hints would have to be drawn across a multi-row
  region of a copy of the pane, i.e. reimplementing fingers' overlay window; a list
  shows the whole rejoined text (the thing the user could not see in one piece),
  can include scrollback, and reuses `visibleWidth`, `printableKeyText`,
  `fuzzyScore`, `sanitizeStatusText` and the popup launcher conventions.
- **Bind: `prefix + O`** ("open"), free on the live server and in
  `config/tmux.conf.tmpl` (`o` is select-pane, `u`/`U` are remux, `f`/`F`/`J` are
  fingers). Local bind, no `BridgeGate` branch: in a mirror window the visible
  pane is the local renderer pane, so the local capture is the right source.

### Pure extraction (`picker/linkjoin`, new package, no tmux)

Input: the origin pane's width `W` and its capture (`capture-pane -p -J -e`,
history included). Output: an ordered, de-duplicated list of links
`{Text, Kind (url|path), Rows int /* rows spanned, 1 = not joined */}`, newest
(bottom-most) first.

1. **Split escapes from text.** Collect OSC 8 hrefs per logical line; strip every
   escape sequence to get the plain line (`ansi.Strip`, `charmbracelet/x/ansi`,
   already a dependency). An href is kept only if the whole href is a URL by the
   rule below (scheme allow-list, no whitespace or control bytes).
2. **Tail width.** For each plain line, trim trailing spaces; `tail` = the cell
   width of its last physical row: `w = visibleWidth(line)`, `tail = w % W`, and
   `tail = W` when `w > 0 && w % W == 0` (a `-J`-joined soft-wrapped line is longer
   than `W`). Known imprecision: tmux leaves a row's last cell blank when a wide
   glyph does not fit, so a soft-wrapped line holding such a row computes a tail
   one cell short per row; the one-cell margin below absorbs a single such row,
   and links are ASCII, so this is accepted rather than worked around.
3. **Rejoin.** The *edge token* of line `i` is its trailing run of
   non-whitespace characters. Line `i` continues into line `i+1` when all hold:
   - `tail(i) >= W - 1` (the margin: Claude Code fills to `W`, pi to `W-1`);
   - the edge token is a *link head*: (a) it contains a URL or path match whose
     greedy (pre-trim) end is the token's end — a link runs to the edge; or (b)
     some suffix of it is a non-empty prefix of an allowed scheme, the full
     scheme included (`h`…`https://`, `g`…`git@`/`git://`, `s`…`ssh://`,
     `f`…`ftp://`/`file:///`), so a break inside or right after the scheme
     (`see htt` / `  ps://…`, `(https://` / `  github.com/…`, `git@` /
     `  github.com:…`) still joins; for (b) the crossing match below must be a
     *URL* match starting inside that suffix (`logs` / `  files/x` has the
     scheme-prefix suffix `s`, but only a path crosses, so it is not joined); or
     (c) a *bare segment*
     `[.\w~$@-]+` (no `/`, e.g. `picker` or `~`) when line `i+1` starts with `/`
     (a relative path broken just before a slash);
   - line `i+1` is not blank (never join across a blank row);
   - line `i+1`, after stripping its leading spaces, starts with a continuation
     character `[A-Za-z0-9/_.~%?#&=+@$:,;!-]` — not `(`, `[`, `<`, a quote, a
     backtick or `*` — and is not a list item (`^(\d+[.)]|[-*+•])\s`). `:,;!` are
     safe starters: a word-wrap breaks at a space, and those characters attach to
     the preceding word, so they never begin a word-wrapped row;
   - the edge token followed by line `i+1` has a URL or path match that
     **crosses the join point** (starts before it, ends after it) — a head and a
     continuation that do not form one link are not joined. The match is taken
     over edge token + continuation only (no pattern spans whitespace, so none
     can start earlier), keeping each join's work proportional to the token;
   - the edge token is at most 8192 bytes. Each join rescans the token, so an
     unspaced blob of edge-filling rows would otherwise make rejoining quadratic
     (2000 rows at width 200 took ~55 s); past the cap the next row starts a new
     virtual line.
   Joining appends line `i+1` minus its leading spaces to line `i`; chains repeat
   (`i+1` may continue into `i+2`), so a 3-row URL becomes one virtual line. The
   continuation row's remaining text stays in the virtual line, so later links on
   it are still found once.
4. **Ambiguous joins offer both readings.** Geometry cannot tell a mid-token
   break from a link that ended exactly at the edge followed by a word-wrapped
   row. Ambiguity is decided per join. A join is *ambiguous* when any holds:
   - the edge token ends in trailing punctuation (`.,;:!?`) — a sentence that
     ended at the edge looks like a mid-token break after a dot;
   - the continuation's first whitespace-delimited token, with trailing
     `.,;:!?` trimmed, is a word, number, contraction or hyphenated word
     (`^[A-Za-z0-9'-]+$`: `…/pull/12` / `  here.`, `  3 files`, `  it's`,
     `  follow-up`) — except when that token is the whole continuation row and
     the row itself reaches the edge: a one-token row running to the edge is a
     mid-token break, not a wrapped word;
   - the head is a bare segment only (rule 3c: `saved to` / `  /tmp/x` reads as
     both `to/tmp/x` and `/tmp/x`);
   - the continuation starts with `/` and the head qualified by rule 3a: a
     complete link at the edge followed by a `/` row reads both as one link and
     as two (`…/tmux-og` / `  /blob/main/…`).
   The links of the fully joined virtual line come first. When any join is
   ambiguous, the virtual line is then read again split at its ambiguous joins
   only: each segment between them keeps its certain joins (and counts `Rows`
   within the segment), so a URL rejoined across a certain break is offered whole
   even when a word row follows it. Results are de-duplicated. An unambiguous
   join (a URL head continuing with `ared-state.md and more`) offers only the
   joined link, so a correctly rejoined URL is not shadowed by its truncated head.
5. **Match** each virtual line with the URL and path rules; trim trailing
   punctuation; drop empty results; de-duplicate by text (keep the newest);
   add OSC 8 hrefs (also de-duplicated against the text matches).

**Match rules** — an RE2 port of #1067's `@fingers-pattern-url`/`path` (its
replacements for fingers' url and path built-ins), so both pick the same text:

- URL: #1067's PCRE
  `` (?:https?://|git@|git://|ssh://|ftp://|file:///)(?:[^\s()"'`]|\([^\s()"'`]*\))+(?<![.,;:!?>\]}]) ``,
  with control bytes (`\x00-\x1f`, `\x7f`) also excluded from the body — a safety
  addition, so no escape byte from a capture reaches the opener. Balanced `(…)`
  is admitted by the pattern (`…/wiki/Foo_(bar)` stays whole); an unbalanced `(`
  or `)` ends the match (`(https://x.io/a)` gives `https://x.io/a`,
  `https://x.io/a(b` gives `https://x.io/a`).
- Path: #1067's pattern, fingers' built-in `(([.\w\-~\$@]+)?(/[.\w\-@]+)+/?)`
  plus `(?<![.])` (ASCII `\w`, so `path:line:col` stops before `:`).
- **Trailing punctuation**: RE2 has no lookbehind, so the lookbehinds become
  `TrimTrailing`, which repeatedly strips a final `.,;:!?>]}` — what PCRE
  backtracking off the lookbehind amounts to. No bracket or quote balancing: a
  stray `]` is stripped even when its `[` is inside (`https://x.io/a[1]` gives
  `https://x.io/a[1`, as fingers does). The path pattern holds only `.` of that
  set. The input/expected table `picker/linkjoin/testdata/trailing.tsv` mirrors
  `tests/fingers-patterns.bats` of #1067 by hand; the two are kept in step.

### Picker (`picker/links.go`, `main` package)

- `tmux-picker-generate --links`: reads `OG_PICKER_ORIGIN_PANE` and
  `OG_PICKER_CLIENT`; one tmux invocation (`display-message -p -t <pane>
  '#{pane_width}' \; capture-pane -p -J -e -t <pane> -S -<history>`; history 2000
  lines), then `linkjoin.Extract`.
- Rows: a dim kind tag (`url`/`path`), the link text (`sanitizeStatusText`, then
  `truncateVisibleWidth` to the row), and a dim `+N` marker when the link was
  rejoined from `N` rows. Widths via `visibleWidth`, never `len`. Typed keys extend
  the query via `printableKeyText`; the list is `fuzzyScore`-ranked under a query,
  newest-first otherwise. Empty state: "no links in this pane".
- Keys: `enter` opens an *openable* link (`http://`, `https://`, `file:///`) and
  copies anything else (paths, `git@`, `git://`, `ssh://`, `ftp://`); `ctrl+y`
  copies any link; up/down, `ctrl+p`/`ctrl+n` move; `esc`/`ctrl+c` close. The hint
  line says which action `enter` takes for the selected row.
- **Open** runs `og-open <url>` (argv, no shell) with `TMUX_PANE=<origin pane>`
  after the popup closes: off a bridge `og-open` is the platform opener
  (`xdg-open`/`open`, what fingers' `:open:` runs); on a remote session that a
  controller mirrors it forwards the URL to the controller (`@og_open_url`,
  `bridge-shipped-state.md`), which accepts only http(s) — so a `file:///` open
  there fails with `og-open`'s own error, shown via `display-message`. Paths are
  never opened: they are relative to the origin pane (and in a mirror, to a
  remote host). A failed open or copy is reported with `tmux display-message -c
  <client>`, never silently dropped.
- **Copy** runs `tmux set-buffer -w -t <client> -- <text>`: a paste buffer plus
  OSC 52 to the invoking client (`set-clipboard on`), which reaches the terminal's
  clipboard locally and over ssh alike.
- Launcher `scripts/tmux-link-picker.sh` (the which-key launcher's shape:
  `--client`/`--origin-pane`, pinned `-c`/`-t "$CLIENT:"`, `display-popup -E`,
  title ` Links `), registered like `tmux-which-key` (`scriptsWithPickerBin`,
  `generator/paths`, the `og` catalog entry `pick links`).

### Tests

- `picker/linkjoin` table tests over fixtures in `picker/linkjoin/testdata/`:
  URL broken across 2 and 3 rows with Claude Code's two-space indent (rejoined
  whole); the same with pi's one-cell margin; a zero-indent mirror-seed break;
  prose that reaches the edge (not joined); a path broken mid-segment (rejoined);
  a token at the edge followed by `(…` and by a list item (not joined); a blank row
  between (not joined); trailing punctuation excluded; a break inside the scheme
  (`htt` / `ps://…`, rejoined), right after it (`https://` / `github.com/…`,
  `git@` / `github.com:…`, rejoined) and after an opener (`(htt` / `ps://…`,
  rejoined); a URL continuing with `:8080/…` (rejoined); each ambiguous-join
  trigger offering both readings, joined first (`saved to` / `/tmp/x` gives
  `to/tmp/x` then `/tmp/x`); ambiguity per join (a URL rejoined across a certain
  break, then a word row, offers the URL whole as its fallback); an unambiguous
  join offering only the joined link; an unspaced blob of 2000 edge-filling rows
  at width 200 extracted within a wall-clock budget (the 8192-byte token cap);
  a prose word at the edge followed by a row not starting with `/` (`data` /
  `files/x` and `logs` / `files/x`, not joined); OSC 8 href collected and de-duplicated;
  soft-wrapped `-J` line longer than `W`.
- **Real fixture:** `%385`'s capture (Claude Code, width 62, path across 3 rows
  followed by the `(verdict.json …` row) committed under `testdata/`.
- `trailing.tsv` table covering #1067's acceptance rows.
- Picker model tests: rows sized with `visibleWidth` (wide glyph in a link),
  typed space reaches the query, enter/ctrl+y produce the right action, empty state.
- bats: `prefix O` renders a `run-shell` of `tmux-link-picker` with
  `--origin-pane`, next to the which-key assertion.

### Docs

`docs/agents/picker.md` gains a "Link picker (#1068)" section (heuristic, margin,
measurements, actions, mirror behaviour); `docs/agents/scripts.md` a
`tmux-link-picker` row; README's key table a `prefix + O` row.

## Out of scope

- Changing fingers or its patterns (#1067 owns that).
- Rejoining arbitrary tokens (hashes, ids) — URLs and paths only.
- Opening paths in an editor.
