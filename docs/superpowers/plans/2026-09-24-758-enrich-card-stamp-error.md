# Plan: enrich card — show why a stamp has no URL (#758)

## Problem

Pressing `o` in the enrich card on a window whose `@issue_id` is set but
`@issue_url` is empty does nothing. Root cause: `tmux-issue-stamp-linear.sh`
discards the `linear` CLI's stderr (`2>/dev/null`), so a CLI failure (e.g. "No
API key configured") produces the same empty output as "no such issue" — the
stamp script and the card can't distinguish "nothing to show" from "the CLI
broke". `openCmd`'s `xdg-open` failure is silently dropped too.

## Fix, file by file

### 1. `scripts/lib-enrich.sh` — new sanitizer

Add `sanitize_stamp_error RAW` near `sanitize_title` (after line 88): strips
ANSI CSI sequences, then all remaining control chars (not just CR/LF/ESC —
tabs, BEL, etc.), then clamps to 120 chars. Sets `REPLY`.

```bash
# sanitize_stamp_error RAW
# Strips ANSI CSI sequences and control chars from a captured stderr line,
# then clamps to 120 chars. Sets REPLY to the cleaned text.
sanitize_stamp_error() {
	local clean
	clean="$(printf '%s' "$1" | sed -E $'s/\x1b\\[[0-9;]*[a-zA-Z]//g')"
	clean="${clean//[[:cntrl:]]/}"
	REPLY="${clean:0:120}"
}
```

Never strip `'`/`#` here (this text rides `tmux set-option` argv, not a
single-quoted `#()` format string like titles do, and is read back via
`show-options`, one option per line — only control chars/newlines matter).
Leave embedded `"`/`\` as-is (a known display quirk in `unquote`, not this
sanitizer's job to fix — out of scope).

### 2. `scripts/tmux-issue-stamp-linear.sh` — capture + emit error

Contract changes from 3 output lines (id/title/url) to 4
(id/title/url/error), on **both** dispatch branches. `error` is the sanitized
first line of stderr from the `title` CLI call — chosen because both branches
already run it, and a systemic CLI failure (auth, network) produces the same
message on every subcommand, so one representative call is enough. Set
**only** when `linear` is on PATH, was invoked, and both `title` and `url`
ended up empty (a real CLI failure, not "no key found in branch" — that path
already returns 3 empty lines before any CLI call and is untouched here).

Concrete diff shape, **explicit-key branch** (currently lines ~24-27):

```bash
raw_err=""
if [[ -n $explicit_key ]]; then
	id="$explicit_key"
	if command -v linear >/dev/null 2>&1; then
		errf="$(mktemp)"
		title="$(timeout 15 linear issue title "$id" 2>"$errf")" || title=""
		raw_err="$(head -n1 "$errf" 2>/dev/null)"
		rm -f "$errf"
		url="$(timeout 15 linear issue url "$id" 2>/dev/null)" || url=""
	fi
else
```

**Branch-derived branch** (currently lines ~36-56): change the `title`
subshell's redirect from `2>/dev/null` to `2>"$tmpd/title.err"`, and read it
**before** the existing `rm -rf "$tmpd"` (line ~54):

```bash
	if command -v linear >/dev/null 2>&1 && [[ -d $worktree ]]; then
		tmpd="$(mktemp -d)"
		(cd "$worktree" && timeout 15 linear issue title 2>"$tmpd/title.err") >"$tmpd/title" &
		(cd "$worktree" && timeout 15 linear issue url 2>/dev/null) >"$tmpd/url" &
		(cd "$worktree" && timeout 15 linear issue id 2>/dev/null) >"$tmpd/id" &
		wait
		title="$(<"$tmpd/title")"
		url="$(<"$tmpd/url")"
		cli_id="$(<"$tmpd/id")"
		raw_err="$(head -n1 "$tmpd/title.err" 2>/dev/null)"
		rm -rf "$tmpd"
		[[ -n $cli_id ]] && id="$cli_id"
	fi
fi
```

After both branches converge (both declare `raw_err`, default `""` — add
`raw_err=""` once before the `if [[ -n $explicit_key ]]` line so it's always
defined), one shared block before the existing title-sanitize step:

```bash
err=""
if [[ -z $title && -z $url && -n $raw_err ]]; then
	sanitize_stamp_error "$raw_err"
	err="$REPLY"
fi
```

Change the final `printf` to 4 lines: `printf '%s\n%s\n%s\n%s\n' "$id" "$title" "$url" "$err"`.
Update the header comment: "four lines on stdout — id, title, url, error".

### 3. `scripts/tmux-issue-stamp-github.sh` — same shape, for parity

`gh issue view` is the single CLI call. Redirect its stderr to a mktemp file
instead of `/dev/null`, read the first line into `raw_err`, remove the temp
file, and — if `title`/`url` both end up empty after a non-empty `num` — run
the same `sanitize_stamp_error` block as above. Emit as a 4th line. Update the
header comment to match (id/title/url/error).

### 4. `scripts/tmux-issue-stamp.sh` — new `@issue_stamp_error` option

- `run_provider()`: no signature change — it already runs the provider script
  and the caller does `mapfile -t out < <(run_provider ...)`; read
  `out[3]:-` as `err` alongside `id`/`title`/`url` in both the explicit-id
  dispatch block (~line 138-146) and the branch-derived loop (~line 148-157).
- No-id branch (`if [[ -z $id ]]`, ~line 163-173, the block that does
  `-wu @issue_provider`/`@issue_id`/etc.): add
  `tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null` next to
  the other `-wu` clears — a branch with no matched provider has no stamp
  error to show.
- Id-found branch: the existing backfill-tries block (~line 203-213) already
  branches on `if [[ -n $title && -n $url ]]`. Reuse that exact condition —
  do not add a second, independent `if`:
  ```bash
  if [[ -n $title && -n $url ]]; then
  	tmux set-option -t "$target" -wu @issue_backfill_tries 2>/dev/null
  	tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null
  else
  	if [[ -n $err ]]; then
  		tmux set-option -t "$target" -w @issue_stamp_error "$err"
  	else
  		tmux set-option -t "$target" -wu @issue_stamp_error 2>/dev/null
  	fi
  	if [[ $old_branch == "$branch" && $old_explicit_id == "$explicit_id" ]]; then
  		tries="$(tmux show-options -t "$target" -wqv @issue_backfill_tries 2>/dev/null)"
  		[[ $tries =~ ^[0-9]+$ ]] || tries=0
  	else
  		tries=0
  	fi
  	tmux set-option -t "$target" -w @issue_backfill_tries "$((tries + 1))"
  fi
  ```
  (i.e. insert the `@issue_stamp_error` set/clear into each existing arm,
  don't wrap the whole thing in a new outer conditional.)

### 5. `picker/enrichcard/options.go` — parse the option

- `winState`: add `issueStampError string` field next to `issueExplicitID`.
- `parseWindowOptions`: add `case "@issue_stamp_error": o.local.issueStampError = val`.
  Local-only (see bridge decision below) — no `@bridge_issue_stamp_error`
  case, no `resolve()` change.

### 6. `picker/enrichcard/model.go` — render + flash + xdg-open failure

- Add a helper (used by both `issueBlock` and `handleKey`):
  ```go
  func (w winState) noURLReason() string {
  	if w.issueStampError != "" {
  		return w.issueStampError
  	}
  	return "stamp failed"
  }
  ```
- `issueBlock()`: when `w.issueID != ""` and `w.issueURL == ""`, append a
  second line below the existing id/title head reading
  `"no url — " + w.noURLReason()`, truncated with the same
  `truncate(_, m.titleWidth())` the title line already uses (the reason can be
  up to 120 chars — must not overflow the card border). Style with
  `c.overlay0` (matches the "no issue"/"no PR" quiet-placeholder tone used
  elsewhere in this file).
- `handleKey()`, `"o"` case: currently only acts `if m.win.issueURL != ""`.
  Add an `else if` for `m.win.issueID != "" && m.win.issueURL == ""`: set
  Add `m.flash = "no url — " + m.win.noURLReason()`. `footer()` does **not**
  truncate `m.flash` (it renders it as-is), so wrap this the same way
  `issueBlock` wraps the title: `truncate(m.win.noURLReason(), m.titleWidth())`
  appended after `"no url — "` (or truncate the whole composed string — either
  keeps the footer from widening the card). Set
  `m.flashUntil = time.Now().Add(flashErrorDuration)`,
  return `m, nil` (no `openCmd` — there is no URL to open).
- `openCmd(url string) tea.Cmd`: change to report success/failure. Add
  `type openDoneMsg struct{ errText string }` beside the other msg types
  (`tickMsg`/`refreshDoneMsg`/`bridgeRefreshDoneMsg`):
  ```go
  func openCmd(url string) tea.Cmd {
  	return func() tea.Msg {
  		if err := exec.Command("xdg-open", url).Start(); err != nil {
  			return openDoneMsg{errText: err.Error()}
  		}
  		return openDoneMsg{}
  	}
  }
  ```
- `Update()`: add a case for `openDoneMsg`, following the existing
  `m.flashUntil = time.Now().Add(...)` style (not `+=`):
  ```go
  case openDoneMsg:
  	if msg.errText != "" {
  		m.flash = "open failed: " + msg.errText
  		m.flashUntil = time.Now().Add(flashErrorDuration)
  	}
  	return m, nil
  ```
  `handleKey`'s `"o"`/`"p"` success arms keep setting `m.flash = "opened ↗"`
  optimistically as today; a non-empty `openDoneMsg.errText` overwrites it
  shortly after. (Footer flashes render in `c.green` regardless of error vs.
  confirm — pre-existing shared styling with the bridge-refresh flash; not
  changed here.)

### 7. `picker/enrichcard/bridge.go` — bridge decision (explicit, stated in PR)

**Decision: `@issue_stamp_error` stays local-only, does not cross the
bridge.** Rationale for the PR body: `windowLabelFormat`/`labelRow`/
`bridgeLabelOptions` in `picker/remotebridge/daemon/windowlabels.go` are a
fixed-width, carefully-audited carry format (`windowLabelFields = 22`,
sanitization applied per-field on the remote). A mirror window's local
`@issue_*` are already known-stale residue from the launcher's cwd (see
`resolve()`'s comment) and are never shown — only bridge fields are. The
failure this bug fixes is a **local** CLI misconfiguration (no API key) on
whichever host runs the stamp; that host's own enrich card (opened directly
on that host, not through a mirror) already shows the local error via
`resolve()`'s `!o.mirror` branch. Extending the bridge protocol (new field,
new struct member, new test fixture row, `windowLabelFields` bump) to carry a
rare diagnostic string is not proportionate — no plan step touches
`windowlabels.go`. A mirror window whose remote issue has no URL will
therefore show `"no url — stamp failed"` (the generic fallback, since
`@bridge_issue_stamp_error` never exists) — consistent with this decision;
state it in the PR body.

### 8. `docs/agents/enrichment.md`

In the "PR + Issue Enrichment" bullet list (after the "Window options are the
source of truth" bullet, ~line 13), add a bullet:
> **Stamp failures:** when a provider CLI runs but a matched id ends up with
> no title/url, `tmux-issue-stamp` stores the sanitized first line of the
> CLI's stderr in `@issue_stamp_error` (cleared on a successful stamp or when
> no provider matches). The enrich card's `o` row reads `no url — <reason>`
> and flashes it instead of silently no-op'ing; local-only, never carried
> across the remote bridge (`@bridge_*`).

### 9. `docs/agents/scripts.md`

Update the `tmux-issue-stamp-linear` / `-github` row (line 25) — the
documented output contract changes from `id\ntitle\nurl` to
`id\ntitle\nurl\nerror`:
> Provider impls: branch regex (+ `linear`/`gh` CLI) → `id\ntitle\nurl\nerror`
> (error is the sanitized first stderr line from a CLI call that ran but left
> title/url empty). First provider with a non-empty id wins.

Update the `tmux-issue-stamp` row (line 24) to mention it also writes
`@issue_stamp_error` (cleared on a full stamp or a no-id clear):
> ...writes `@issue_provider`/`@issue_id`/`@issue_title`/`@issue_url`/
> `@issue_stamp_error`, then kicks an immediate PR fetch. ...

### 10. `flake.nix`

The new provider-level bats file must be registered or `nix flake check`
never runs it. Add it to the existing `issue-stamp-tests` `runCommand`
(~line 1606-1614, `nativeBuildInputs = [pkgs.bats pkgs.coreutils]` already
covers `mktemp`/`timeout`/`sed`):
```nix
issue-stamp-tests =
  pkgs.runCommand "issue-stamp-tests" {
    nativeBuildInputs = [pkgs.bats pkgs.coreutils];
  } ''
    cp -r ${./scripts} scripts
    cp -r ${./tests} tests
    bats tests/issue-stamp.bats
    bats tests/issue-stamp-linear-provider.bats
    touch $out
  '';
```
(No new derivation needed — same inputs suffice.)

## Tests

### `tests/issue-stamp-linear-provider.bats` (new)

Direct bats test of `scripts/tmux-issue-stamp-linear.sh` itself (not the
dispatcher-level fake used by `tests/issue-stamp.bats`), following the same
sed-substitution build pattern as `tests/issue-stamp.bats`'s setup (stub
`@lib_enrich@` against a copy of `scripts/lib-enrich.sh` with the icon/
`@providers@` placeholders replaced — those placeholders don't matter for
this script, but `lib-enrich.sh` must still be a runnable file, i.e. every
`@..@` token substituted or the `source` fails). Put a fake `linear` binary on
PATH; the branch-derived path requires `[[ -d $worktree ]]` (script line ~45),
so pass a real directory (e.g. `$BATS_TEST_TMPDIR/wt`, `mkdir -p` it) as arg 1
or the fake `linear` is never invoked.

- **Failing fake `linear`** (branch-derived: `linear issue title` exits 1 and
  writes to stderr a line containing a tab, a BEL byte, and an ANSI color
  sequence around the message, e.g.
  `printf 'No API key \tconfigured\a\033[31m!!\033[0m\n' >&2; exit 1` — the
  other two subcommands (`url`, `id`) can just exit 1 with no output) → id
  resolves from branch regex (e.g. branch `feat/eng-1957-x`), title/url
  empty, 4th line equals the sanitized message: no tab/BEL/ANSI codes, clamped
  to 120 chars if needed.
- **Succeeding fake `linear`** (all three subcommands print a value, exit 0)
  → 4th line is empty (no error emitted).
- **Explicit-key mode** (3rd arg passed): same failing/succeeding split for
  the `title`/`url` two-call path.

Also extend `tests/issue-stamp.bats`'s existing dispatcher-level fixture: make
the fake `issue-stamp-linear` provider (in that file's `setup()`) emit a 4th
line when its 2nd arg matches a new trigger branch (e.g. `*eng-9001*`) —
`printf 'ENG-9001\n\n\nsome error\n'` (non-empty id, **empty** title and url;
`tmux-issue-stamp.sh` only stores `err` in the `else` arm of
`[[ -n $title && -n $url ]]`, so a non-empty title/url here would make the
assertion fail) — and add a test asserting
`tmux-issue-stamp.sh` stores it into `$STATE/opt_@issue_stamp_error`; then run
the existing successful-stamp test path (branch `eng-1957`) afterward on the
same window and assert `grep -q 'unset @issue_stamp_error' "$STATE/setlog"`
(mirroring the existing `@issue_id` unset assertion pattern at line 139).

### `picker/enrichcard/model_test.go`

Read the file's existing test structure first (helper builders for `model`/
`winState`, assertion style) before writing — it wasn't read in planning.
Add cases matching that style:
- `issueBlock()` (or the full `card()` render, whichever existing tests
  target) with `issueID` set, `issueURL` empty, `issueStampError` set to some
  text → rendered output contains `"no url — <that text>"`.
- Same with `issueStampError` empty → contains `"no url — stamp failed"`.
- `TestCardFullIssueAndPR` (or equivalently-named existing full-fixture test
  at model_test.go:42) currently builds a window with `issueID` set and no
  `issueURL` — add an `issueURL` to that fixture so it keeps describing a
  *complete* stamp and doesn't start incidentally exercising the new no-url
  row.
- `handleKey("o")` on a model whose `win.issueID != "" && win.issueURL == ""`
  → resulting model's `flash` is `"no url — <reason>"`, returned cmd does not
  invoke `xdg-open` (assert cmd is `nil`, or that calling it does not error —
  match whatever the existing `"o"`/`"p"` tests already assert for the
  success path).
- `Update` on `openDoneMsg{errText: "no such file"}` → resulting model's
  `flash` is `"open failed: no such file"`. This tests the `Update` message
  handling directly, without exercising the real `exec.Command`/`xdg-open`
  (matching how `bridgeRefreshDoneMsg` is presumably already tested — check
  the existing pattern for that message type in this file first).

## Acceptance criteria checklist

- [ ] bats: failing fake `linear` → `@issue_stamp_error` set to sanitized
      first stderr line (new provider-level test + dispatcher-level test)
- [ ] bats: succeeding stamp clears `@issue_stamp_error`
- [ ] Go test: card renders `no url — …` row for an id-without-url window
- [ ] Go test: `o` flashes the same reason on an id-without-url window
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` green
      (includes registering the new bats file in `flake.nix`)
- [ ] `docs/agents/enrichment.md` and `docs/agents/scripts.md` updated
- [ ] PR body states the bridge decision (local-only) explicitly

## Out of scope

- Carrying `@issue_stamp_error` across the remote bridge (decided against,
  §7).
- Any change to the `gh`/`linear` CLI invocation retry/backoff behavior.
- Changing `sanitize_title`'s stripping rules.
- Fixing `unquote`'s handling of embedded `"`/`\` in option values (a
  pre-existing display quirk, not introduced by this change).
