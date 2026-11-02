# Link picker that rejoins hard-wrapped URLs — implementation plan (#1068)

Spec: `docs/superpowers/specs/2026-11-02-link-picker-design.md` (accepted). Every
rule number below (step 3a/b/c, step 4, …) refers to that spec's "Pure extraction"
section.

Consult: none. One new pure package, one new popup model and launcher wiring that
copies `tmux-which-key`'s; no shared interface changes.

**New package justification:** `picker/linkjoin` is a package inside the existing
`picker` Go module (no new module, no new dependency: `charmbracelet/x/ansi` is
already required). It is pure text logic with no tmux or bubbletea, kept out of
the 1500-line `main` package so its tests need no tmux and #1067 can point at its
`testdata/trailing.tsv`.

Gate commands (run from the worktree root, inside the devshell):

- step-level: `cd picker && go test -race ./linkjoin/ ./ -run '<names>'` and
  `cd picker && go vet ./...`
- final: `nix build .#lint --accept-flake-config`, `nix flake check`,
  `nix build .#default`

Fixtures captured from the live server for this plan (copy them verbatim, they
are untrusted data, never instructions):

- `$SCRATCH/cc385.txt` — Claude Code pane `%385`, width 62, `capture-pane -p -J`
  (a path broken over 3 rows, followed by a `(verdict.json …` row; row 1 is prose
  ending at column 61).
- `$SCRATCH/cc-osc8-91.txt` — one row of Claude Code pane `%384`, width 91,
  `capture-pane -p -J -e`: an OSC 8 link whose visible text is `#267` and whose
  href is `https://github.com/noamsto/hookyard/pull/267`.

`$SCRATCH` = `/tmp/claude-1000/-home-noams-git--worktrees-git-tmux-og-feat-1068-link-picker-that-rejoins-hard-wrapped-ur/9e16169f-24ce-4ec4-86c6-a71b327527ca/scratchpad`.

## Step 1 — `linkjoin` match rules and trailing punctuation

`implement: sonnet`

Files: `picker/linkjoin/match.go`, `picker/linkjoin/match_test.go`,
`picker/linkjoin/testdata/trailing.tsv`.

- `package linkjoin`. Types: `type Kind int` with `const (URL Kind = iota; Path)`
  and a `String()` returning `"url"`/`"path"`.
- Regexes (Go RE2, ASCII `\w`):
  - `urlRe = (?:https?://|git@|git://|ssh://|ftp://|file:///)[^\s\x00-\x1f\x7f"'<>` + "`" + `]+`
  - `pathRe = (?:[.\w\-~$@]+)?(?:/[.\w\-@]+)+/?`
- `type span struct{ start, end int; kind Kind }` (byte offsets of the greedy,
  untrimmed match). `func matches(s string) []span`: all URL matches, then path
  matches that do not overlap a URL match (a URL's path part is not offered again
  as a path), sorted by start.
- `func TrimTrailing(s string) string` (exported: the #1067 rule, used by the
  picker tests too): loop — strip a final `.,;:!?`; strip a final `)`/`]`/`}`/`>`
  /`'`/`"`/`` ` `` only when the count of that closer in `s` exceeds the count of
  its opener (`(`/`[`/`{`/`<`; for the quote/backtick closers: an odd count);
  stop when nothing changed.
- `func Openable(s string) bool`: prefix `http://`, `https://` or `file:///`.
- `trailing.tsv`: `input<TAB>expected` (the first link `Extract` returns for the
  input as a single line at width 200; `-` = no link), header comment naming #1067.
  Rows (at least): `see https://x.io/a<P>` → `https://x.io/a` for each `<P>` in
  `. , ; : ! ? ) ] >` + backtick + `'` + `"`; `(https://x.io/a)` →
  `https://x.io/a`; `` `https://x.io/a` `` → `https://x.io/a`;
  `https://x.io/a?b=1&c=2#frag.` → `https://x.io/a?b=1&c=2#frag`;
  `https://en.wikipedia.org/wiki/Foo_(bar)` kept whole; `file:///tmp/x.txt.` →
  `file:///tmp/x.txt`; `edit foo/bar.go.` → `foo/bar.go`; `./a/b.go:12:3` →
  `./a/b.go`; `~/x/y` → `~/x/y`; `a/b.tar.gz` → `a/b.tar.gz`.
- Tests: `TestTrimTrailing` (table), `TestMatchesURLShadowsPath`
  (`https://x.io/a/b` yields one URL span, no path span), `TestOpenable`.
  `TestTrailingVectors` is added in step 2 (it needs `Extract`).

Verify: `cd picker && go test -race ./linkjoin/` passes; `go vet ./linkjoin/`.

## Step 2 — `linkjoin.Extract`: rejoin, ambiguity, OSC 8

`implement: opus` (the core heuristic; every spec rule lands here)

Files: `picker/linkjoin/extract.go`, `picker/linkjoin/extract_test.go`,
`picker/linkjoin/testdata/*.txt` (fixtures below). Depends on step 1.

API:

```go
type Link struct {
	Text string
	Kind Kind
	Rows int // physical rows the link's virtual line spans; 1 = not rejoined
}

// Extract returns the URLs and paths in capture (a `capture-pane -p -J [-e]`
// dump of a pane width cells wide), newest (bottom-most) first.
func Extract(capture string, width int) []Link
```

Algorithm (spec steps 1–5, in this order):

1. Split `capture` on `\n` (drop one trailing empty element). Per line: collect
   OSC 8 hrefs — scan for `ESC ] 8 ;` params `;` URI terminated by `BEL` or
   `ESC \`; keep a URI only when `urlRe` matches it whole and `TrimTrailing`
   leaves it unchanged — then `plain = ansi.Strip(line)`
   (`github.com/charmbracelet/x/ansi`), then trim trailing spaces.
2. `tail(line)`: `w := ansi.StringWidth(plain)`; `w == 0` → 0; `w % width == 0`
   → `width`; else `w % width`. (`ansi.StringWidth` instead of `main.visibleWidth`:
   different package; both count a narrow ASCII URL identically.) Comment the
   wide-glyph imprecision in one line.
3. Build virtual lines top to bottom. A virtual line holds `text`, `rows`
   (count), the byte offsets of each join point in `text`, an `ambiguous` flag
   and the original physical lines. To decide whether the current virtual line
   continues into physical line `j`:
   - `tail` of the *last physical row* appended (not of the virtual text) `>= width-1`;
   - `next := strings.TrimLeft(plain[j], " ")` non-empty; `next[0]` in
     `[A-Za-z0-9/_.~%?#&=+@$:,;!-]`; `next` not matching `^(\d+[.)]|[-*+•])\s`;
   - the *edge token* = the trailing non-space run of the virtual text; it is a
     link head per spec 3a (a `matches` span whose end == len(text)), 3b (for some
     suffix of the token that is a non-empty prefix of one of `https://`,
     `http://`, `git@`, `git://`, `ssh://`, `ftp://`, `file:///` — note `http://`
     is listed so `http:/` counts), or 3c (token matches `^[.\w~$@-]+$` and
     `next[0] == '/'`);
   - joining `cand := text + next` has a crossing match: a `matches(cand)` span
     with `start < len(text) && end > len(text)`; for a 3b-only head (not 3a), the
     crossing span must be `kind == URL` and start inside that suffix
     (`start >= len(text)-len(suffix)`).
   - On join: ambiguous (spec step 4) when the edge token ends in `.,;:!?`, or
     `next`'s first token matches `^[A-Za-z]+$` and is followed by a space or
     the end of `next`, or the head was 3c-only.
4. Per virtual line, links in reading order: for each `matches` span,
   `TrimTrailing(text[start:end])`; drop empty; `Rows` = the number of physical
   rows the *span* covers (count join points inside `(start, end)` + 1). For an
   ambiguous virtual line, append the links of each physical row on its own
   (Rows 1) after the joined ones. Then that line's OSC 8 hrefs (Kind URL, Rows 1).
5. Order virtual lines bottom-most first; within a virtual line keep reading
   order (so the list is newest-first by line); de-duplicate by `Text` keeping
   the first occurrence. Exception for ambiguity: a joined link must precede its
   own head/continuation readings, which the per-line order already gives.

Fixtures (`testdata/`, first line `# width=<W>` then the capture verbatim;
a helper `loadFixture(t, name) (capture string, width int)`):

- `claude-code-path-62.txt` — copy of `$SCRATCH/cc385.txt` with the header
  `# width=62` (real Claude Code capture).
- `claude-code-osc8-91.txt` — copy of `$SCRATCH/cc-osc8-91.txt`, `# width=91`
  (real, escapes kept byte-for-byte: write it with `cp`, not an editor).
- `cc-url-2rows-60.txt`, `cc-url-3rows-40.txt` — synthetic in the measured Claude
  Code shape: `● ` / two-space continuation indent, the URL broken mid-token at
  exactly the width, the last row continuing with ` and more prose.`
- `pi-path-30.txt` — pi shape: one leading space, text ending at column
  `width-1`, one trailing pad space, continuation with one leading space.
- `mirror-seed-30.txt` — zero-indent hard break of a URL.

Tests (`extract_test.go`), each asserting the exact `[]Link` (Text, Kind, Rows):

- `TestExtractRealClaudeCodePath`: the fixture yields, in order,
  `/tmp/claude-1000/-home-noams-git--worktrees-git-hookyard-feat-264-priors-write-guard-false-positives-on-re/f9266e70-b330-463b-b41f-50721c09f72e/scratchpad/`
  (Rows 3), `declare/printf`, `BASH_CMDS/PATH` (row 1 ends `the` at column 61:
  no join).
- `TestExtractRealOSC8`: contains `https://github.com/noamsto/hookyard/pull/267`
  (Kind URL), whose text is not visible in the plain row.
- `TestExtractClaudeCodeURL2Rows`, `…3Rows`, `TestExtractPiMargin`,
  `TestExtractMirrorSeed`: whole URL/path, Rows 2/3/2/2, trailing `.` excluded.
- `TestExtractNoJoin` (table, inline captures): prose reaching the edge; a blank
  row between; the edge token followed by `(…`, by `- item`, by `1. item`;
  `data` / `files/x`; `logs` / `files/x`; `x.go` / `:12` (no crossing match;
  path stays `x.go`).
- `TestExtractSchemeBreaks` (table): `see htt` / `  ps://x.io/a b`; `https://` /
  `  x.io/a b`; `git@` / `  github.com:o/r.git b`; `(htt` / `  ps://x.io/a) b`;
  `https://h` / `  :8080/a b` — each rejoined into one URL, Rows 2.
- `TestExtractAmbiguous` (table): `…/a.` + `  Then x` (edge punctuation),
  `…/scratchpad` + `  and then` (plain word), `saved to` + `  /tmp/x` (3c) — each
  yields the joined link first, then the separate readings
  (`to/tmp/x`, `/tmp/x`).
- `TestExtractUnambiguousOffersOnlyJoined`: a URL continuing with
  `ared-state.md and more` yields the joined URL and not its head.
- `TestExtractSoftWrappedLongLine`: a single `-J` line of `2*W` cells ending in a
  URL fragment, next row continuing it → joined (tail == W).
- `TestExtractDedupNewestFirst`: the same URL on rows 1 and 5 → one entry, from
  row 5's position (first in the list).
- `TestTrailingVectors`: reads `testdata/trailing.tsv` and checks
  `Extract(input, 200)` first link (or none) per row.

Verify: `cd picker && go test -race ./linkjoin/` passes; `go vet ./linkjoin/`.
Before the code: run the new tests once against a stub `Extract` returning nil
and confirm they fail (red), then implement.

## Step 3 — the `--links` popup model

`implement: sonnet` (UI; follows `docs/agents/picker.md`)

Files: `picker/links.go`, `picker/links_test.go`, `picker/main.go` (one branch).
Depends on step 2.

- `main.go`: `else if flags["--links"] { err = RunLinks() }` next to
  `--which-key`.
- `links.go`, modelled on `picker/whichkey.go` (a standalone model; reuse the
  package helpers `visibleWidth`, `truncateVisibleWidth`, `fitVisibleWidth`,
  `printableKeyText`, `fuzzyScore`, `sanitizeStatusText`, `readTmuxOpts`,
  `themeFromOpts`, and `whichKeyModel.color`'s option-with-fallback pattern —
  copy it as a `linksModel` method rather than coupling to `whichKeyModel`):
  - `RunLinks()`: `origin := os.Getenv("OG_PICKER_ORIGIN_PANE")`,
    `client := os.Getenv("OG_PICKER_CLIENT")`; empty origin → error
    `"links: OG_PICKER_ORIGIN_PANE unset"`. One tmux call:
    `linksCaptureArgv(origin)` = `display-message -p -t <o> '#{pane_width}' ;
    capture-pane -p -J -e -t <o> -S -2000` (argv with a literal `;` element,
    as `collectStartup` chains). First output line = width (`strconv.Atoi`;
    failure → error), the rest = capture. `linkjoin.Extract(capture, width)`.
    Run the model with `tea.NewProgram(m, programOptions()...)`. After `Run` returns, perform the picked action (never inside
    `Update`): open → `exec.Command("og-open", text)` with
    `cmd.Env = append(os.Environ(), "TMUX_PANE="+origin)` and
    `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}` (the popup pty closes
    when the picker exits; a cold-started browser must not get its SIGHUP), still
    `Run()` so the error is reported; copy →
    `exec.Command("tmux", "set-buffer", "-w", "-t", client, "--", text)` (omit
    `-t client` when client is empty). Non-nil error → `tmux display-message`
    (`-c client` when set) `"links: <open|copy> failed: <sanitized stderr or
    err>"` and return nil (the popup has closed; the message is the report).
  - Model fields: `links []linkjoin.Link`, `visible []int` (indices into links),
    `cursor`, `query`, `width`, `height`, `tmuxOpts`, `theme`, `action` (`none` /
    `open` / `copy`), `picked linkjoin.Link`.
  - Keys: printable (`printableKeyText`) appends to `query`; `backspace` drops
    the last rune; `up`/`ctrl+p`, `down`/`ctrl+n` move; `enter` sets
    `action = open` if `linkjoin.Openable(text)` else `copy`, then `tea.Quit`;
    `ctrl+y` sets `copy`, quits; `esc`/`ctrl+c` quits with `none`.
  - Filter: empty query → all, Extract's order; else `fuzzyScore(text, query)
    >= 0`, sorted by score descending, stable.
  - View: title row, then rows `" " + tag + " " + link + marker`: `tag` =
    `fitVisibleWidth(kind.String(), 4)` in the overlay colour; link =
    `sanitizeStatusText(text)` truncated with `truncateVisibleWidth` so the row
    fits `width`; `marker` = `" +N"` (N = Rows) in the overlay colour when
    `Rows > 1`. Selected row on the surface background (as which-key's
    `renderRow`). Bottom: search row (`/ <query>`) and a hint line
    `enter:<open|copy> · ^y:copy · esc:close` where `<open|copy>` reflects the
    selected row. Scroll keeps the cursor visible. No links → one dim line
    `no links in this pane`; `enter`/`ctrl+y` then do nothing.
  - All widths via `visibleWidth`/`fitVisibleWidth`, never `len`.
- Tests (`links_test.go`, no tmux):
  - `TestLinksTypedSpaceReachesQuery` (`space` key → query `" "`, via
    `printableKeyText`).
  - `TestLinksEnterOpensURLCopiesPath` (URL row → `open`; path row and `git@`
    row → `copy`); `TestLinksCtrlYCopies`.
  - `TestLinksRowsFitWidth`: a link with a wide glyph (`https://x.io/日本`) and a
    long link; every rendered row's `visibleWidth` ≤ width.
  - `TestLinksJoinedMarker` (`+3` on a Rows-3 link, none on Rows 1).
  - `TestLinksFilterRanks` and `TestLinksEmptyState`.
  - `TestLinksCaptureArgv` pins `linksCaptureArgv("%5")` (the `-J -e`, `-S
    -2000` and the `;` element).
  - `TestParseLinksOutput` (width line + capture split; bad width → error) —
    factor the parse into `parseLinksOutput(out string) (width int, capture string,
    err error)`.

Verify: `cd picker && go test -race . -run 'Links|ParseLinksOutput'` and
`go vet ./...`.

## Step 4 — launcher, bind and Nix wiring

`implement: sonnet`

Files: `scripts/tmux-link-picker.sh` (new), `config/tmux.conf.nix`,
`config/tmux.conf.tmpl`, `config/tmux.conf.reference.nix`,
`generator/paths/paths.go`, `tests/tmux-next38-readiness.bats`. Independent of
steps 1–3 (∥), but its build check needs step 3's `--links`.

- `scripts/tmux-link-picker.sh`: copy `scripts/tmux-which-key.sh`'s structure
  (same `--client`/`--origin-pane` parsing, the one `display -p` options read,
  `POPUP_CLIENT` pinning with its comment shortened to one line pointing at
  `tmux-which-key.sh`). Env: `-e OG_PICKER_ORIGIN_PANE=$ORIGIN`, and
  `-e OG_PICKER_CLIENT=$CLIENT` when set. `display-popup … -E -w 90% -h 60% -b
  rounded -T " Links " -S "fg=$BORDER_FG" "@picker_generate@ --links"`. Header
  comment: two lines (what it launches; see `docs/agents/picker.md`).
- `config/tmux.conf.nix`: add `"tmux-link-picker"` to `scriptNames` (after
  `"tmux-which-key"`), to `scriptsWithPickerBin`, and an `og` catalog entry
  `"pick links" = { script = "tmux-link-picker"; summary = "Link picker: URLs and
  paths in this pane, hard wraps rejoined"; };` after `"pick which-key"`.
- `generator/paths/paths.go`: `"tmux-link-picker"` in `RequiredScripts`
  (alphabetical: between `"tmux-kill-pane-guard"` and `"tmux-pr-enrich"`).
- `config/tmux.conf.tmpl`, right after the which-key bind (line ~82):

  ```
  # Link picker (#1068): URLs and paths in this pane, with Claude Code's hard
  # line breaks rejoined (fingers sees only one row at a time). Enter opens a
  # URL via og-open, ^y copies. See docs/agents/picker.md.
  bind-key -N 'Pick a link to open or copy' O run-shell '{{index .Paths.Scripts "tmux-link-picker"}} #{?client_name,--client #{q:client_name},} --origin-pane #{q:pane_id}'
  ```

  and the same lines in `config/tmux.conf.reference.nix` after its which-key
  bind, with `${script.tmux-link-picker}/bin/tmux-link-picker` (four-space
  indent, as its neighbours).
- `tests/tmux-next38-readiness.bats`: after the which-key assertions, assert
  `list-keys -T prefix O` contains `run-shell`, `tmux-link-picker` and
  `--origin-pane`, and the store binary is executable (the same `store_path`
  pattern).
- `shellcheck scripts/tmux-link-picker.sh` and `shfmt -d` clean.

Verify: `nix build .#default` succeeds; `nix flake check` (bats + the extraction
oracle diff + `og` partition check) passes; `./result/bin/tmux -L lp -f
/dev/null …` is not needed — the bats test covers the bind.

## Step 5 — docs

`implement: sonnet`

Files: `docs/agents/picker.md`, `docs/agents/scripts.md`, `README.md`. Depends on
steps 2–4 (documents what landed).

- `picker.md`: new `## Link picker (#1068)` section before `## Attach`: launch
  (`prefix + O`, `tmux-link-picker`, popup-float, `--links`), the measurements
  table rows from the spec (Claude Code mid-token at exactly `W` with a two-space
  indent; pi one-cell margin; mirror reseeds hard-CRLF, zero indent; OSC 8 partial),
  the rejoin rule summary (tail ≥ `W-1`, link head 3a/3b/3c, continuation class,
  crossing match, ambiguity offers both readings, never across a blank row), the
  trailing-punctuation rule shared with #1067 via `linkjoin/testdata/trailing.tsv`,
  actions (enter: open http(s)/file:/// via `og-open` with `TMUX_PANE`, else copy;
  `^y` copy via `set-buffer -w`), every pane (no agent gating), known gaps (a
  break before `(`/`'`/`*` inside a URL is not joined; wide-glyph tail imprecision;
  history captured at an earlier pane width computes wrong tails after a resize).
  Name the struct field `Rows`, as the code does.
- `scripts.md`: a `tmux-link-picker` row (`prefix + O` with `--client`/
  `--origin-pane` → popup running `tmux-picker-generate --links`).
- `README.md` key table: `| <kbd>O</kbd> | Link picker — URLs and paths in the pane, hard-wrapped ones rejoined |`
  after the `?` row.

Verify: `nix build .#lint --accept-flake-config` (typos, markdown hooks).

## Step 6 — gate (lead)

The three final gate commands. Then an end-to-end smoke on a scratch server:
`TMUX_TMPDIR=$SCRATCH/og CLAUDE_STATUS_DIR=$SCRATCH/og/status ./result/bin/tmux -L lp
new-session -d -x 62 -y 20 'cat <claude-code-path-62 fixture minus header>; sleep 600'`;
`list-keys -T prefix O` shows the bind. The popup needs an attached client, so
drive the picker binary directly (its store path is the `@picker_generate@`
substitution inside the built `tmux-link-picker` script; it is not on `PATH` under
that name) in a second window of that server through
`OG_PICKER_ORIGIN_PANE=<pane id>`, `capture-pane` its popup-less TUI frame and confirm
the 3-row path is listed whole with `+3`; `ctrl+y` and check `show-buffer` holds the
path. Separately, open a URL with no browser running and confirm the browser
survives the picker's exit (Setsid). Every call carries `-L lp`; finish with `kill-server` on `-L lp`.
