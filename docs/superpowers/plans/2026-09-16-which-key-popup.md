# which-key popup built from live bind notes (#629)

## Context

`tmux-which-key` was removed (`ffc0633`) for spinning the CPU. The only
discovery path today is `prefix + ?` → `config/tmux.conf.tmpl:72`
(`bind-key ? list-keys -N -O key -F "..."`), which prints raw commands and
most binds carry no `-N` description.

Config generation is Go, not Nix: `config/tmux.conf.nix` only *wires up* the
build (imports, script/paths tomls) — the tmux.conf text itself ships from
`generator/` (the `og-generate` Go binary, run at
`config/tmux.conf.nix:886`), which executes `config/tmux.conf.tmpl` as a Go
template (`generator/render/render.go`) and calls into
`generator/render/keys.go` for the float-bind fragments interpolated into it
(`{{.LazygitBind}}`, `{{.BtopBind}}`, `{{.K9sBind}}`, `{{.PrdashBind}}`,
`{{.YaziBind}}`, `{{.EnrichCardBind}}`, `{{.CarouselBind}}`, built by
`floatBind`/`mkFloat`, `generator/render/keys.go:32,66`). What ships is
`tmuxConf = generatedConf` (`config/tmux.conf.nix:916`) — this is what
`float-conf-assertions` already scans and what step 2's new check scans too.

Separately, `config/tmux.conf.reference.nix` is a **frozen byte-identity
oracle** (its own header states the rule): `flake.nix`'s
`tmux-conf-extraction-assertions` (~line 551) diffs `generatedConf` against
`referenceConf` (`reference.tmuxConf`, built from this file) across a whole
config matrix, to prove the Go template extraction didn't change output.
Every bind line touched in `config/tmux.conf.tmpl` **and** every `floatBind`
call site touched in `generator/render/keys.go` has a byte-identical mirror
in `config/tmux.conf.reference.nix` (Nix string interpolation, e.g.
`${bridgeGate}` where the template has `{{.BridgeGate}}`) that must be edited
in lockstep, or `nix flake check` goes red on a "difference that is not a
bug." "Every bind has a description" therefore touches three files per bind
site: `config/tmux.conf.tmpl` (or `generator/render/keys.go` for float
binds), and `config/tmux.conf.reference.nix` always.

## Verification note carried from research (read before step 6)

Tested live on a scratch `tmux -L probe` server (own `TMUX_TMPDIR`, per
CLAUDE.md): `send-keys -K -t <pane> <key>` did **not** fire a root-table
bind with no real attached client — the bound command never ran. `-K`'s
key-table lookup appears to depend on genuine client/terminal state the
popup's own launch (`display-popup -c <client>`) may or may not supply.
Do not assume `send-keys -K` works from inside a popup without confirming it
against a *real* attached client first — step 6 must prototype this before
committing to a mechanism, using the resolve-and-replay fallback below if it
doesn't pan out.

Recommended default (verify in step 6, don't skip): resolve the bind's full
command text with `tmux list-keys -T <table> <key>` (this returns the exact
`bind-key -T <table> <key> <command...>` line, note included), strip the
`bind-key ...` prefix to get the bare command, then execute it with
`tmux -t <origin-pane-id> <command>` — run **after** the popup closes, so the
command isn't executed inside the popup's own transient pane/client context,
and targeted at the pane that invoked the popup so pane-scoped formats
(`#{@bridge_pane}`, `#{pane_current_path}`, etc.) resolve the same way they
would for a real keypress. This is why the picker must capture and thread the
invoking pane id (`#{pane_id}`) through from the wrapper script, the same way
`tmux-window-picker.sh` already threads `--client`.

## Mirror-window reasoning (confirm in step 6, "Done when" requires it)

Per CLAUDE.md's "Bridge Session Pinning"/"Remote Window Labels" sections, a
mirror window's keybinds are **local** binds — the popup, `list-keys`, and
the replay all run on the local tmux server regardless of whether the window
mirrors a remote session. Bridge-gated binds already carry their own
`if-shell -F '{{.BridgeGate}}'` routing baked into the command text, so
replaying that text unchanged (as in the mechanism above) reproduces the
exact bridge-vs-local branch a real keypress would take — no special-casing
needed in the picker itself. Step 6 must exercise this against a real
mirrored window, not just assert it.

## Steps

- [ ] **Step 1: describe every bind**
  - In `config/tmux.conf.tmpl`, add `-N '<short description>'` to every
    `bind`/`bind-key` line (prefix table, root `-n` table, `copy-mode-vi`
    table). Touch **only** bind lines — add the flag and, where the line has
    no explicit `-T`/`-n` marker already, leave the rest of the line
    untouched (no reformatting/reordering) so this rebases cleanly against
    the parallel #649 branch also editing this file. `config/tmux.conf.tmpl`
    already documents (around line 124) that the bridge else-branches
    reproduce next-3.8's defaults "`-N` note included (the note feeds
    which-key)" — those notes already exist and must stay byte-identical to
    upstream's wording, not be rewritten, or `config/tmux.conf.reference.nix`
    churns for no reason.
  - In `generator/render/keys.go`, add a `note string` parameter to
    `floatBind` (`:66`) and `bridgedFloatTool` if it independently emits a
    `bind-key` line, and thread a short note through each call site
    (`lazygitBind`, `yaziBind`, `btopBind`, `k9sBind`, `prdashBind`,
    `carouselBind`, `enrichCardBind` — same file). Keep notes short and
    consistent with the style already used at `config/tmux.conf.tmpl:146-148`
    (`'Rename current window'`, `'Swap the active pane with the pane above'`).
    Update `generator/render/keys_test.go` for the new signature/expected
    output.
  - **Mirror every one of the above edits in `config/tmux.conf.reference.nix`**
    (Nix string interpolation, not Go template — same bind text, `${...}`
    instead of `{{...}}`) — this file is a frozen byte-identity oracle
    (`tmux-conf-extraction-assertions` in `flake.nix`, ~line 551) and drifts
    independently of `config/tmux.conf.tmpl`/`generator/render/keys.go`; every
    bind line changed in either of those needs the identical `-N` addition
    here too, or the extraction check fails on an intentional change it reads
    as a bug. The file's own header states this rule explicitly.
  - **Adding `-N` shifts the key token right** (tmux requires `-N '<note>'`
    *before* the key argument — repo precedent:
    `bind-key -N 'Rename current window' , …` at `:146-148`). Three existing
    `nix flake check` derivations in `flake.nix` grep the rendered config
    anchored on `bind[-key] <key>` immediately adjacent, and will stop
    matching once a note is inserted:
    - `flake.nix:424` (`notify-conf-assertions`) — greps
      `bind-key n display-popup -E .*og-notify-center` (source
      `config/tmux.conf.tmpl:194`).
    - `flake.nix:1940-1941` (`bridge-carousel-bind-assertions`) — greps
      `bind I if-shell -F .*@bridge_win` / `...--display-error.*client_name`
      (source `generator/render/keys.go:92`, `carouselBind`).
    - `flake.nix:1955-1956` (`bridge-tool-bind-assertions`, `for k in p g y`)
      — greps `bind-key $k if-shell -F .*@bridge_win` /
      `...bridge-ctl .*tool #{q:@bridge_pane} ...` (source
      `generator/render/keys.go:81`, `bridgedFloatTool`).
    Widen each grep to tolerate an optional note group, e.g.
    `bind-key( -N '[^']*')? n display-popup ...` — don't loosen to a bare
    `.*`, since each check's value is pinning the key→command pairing, not
    just "a bind exists". Edit these three sites in `flake.nix` in the same
    commit as the `-N` additions. After the pass, sweep for any other check
    or bats test grepping a bind by key position:
    `rg -n "bind(-key)? [A-Za-z?|_&{}-]" flake.nix tests/` and widen any hit
    the same way (`tests/conf-shell-quoting.bats` matches on
    `run-shell`/`if-shell` command text, not key position, so it should be
    unaffected — confirm with
    `nix build .#checks.<system>.conf-shell-quoting-assertions` early rather
    than waiting for step 8).
  - Verify: `rg -n "^bind" config/tmux.conf.tmpl | rg -v -- "-N '"` returns
    nothing; `git diff config/tmux.conf.tmpl` shows no line outside a
    `bind`/`bind-key` changed; `go test ./generator/...`; and
    `nix build .#checks.<system>.tmux-conf-extraction-assertions` (or run as
    part of `nix flake check` in step 8) to confirm the reference mirror is
    exact.

- [ ] **Step 2: flake check enforcing descriptions**
  - Add a new check in `flake.nix` beside `float-conf-assertions` (same
    `checks.<system>` attrset, same `CONF = tmuxConfig.tmuxConf` input —
    scan the **rendered** config, i.e. `generatedConf`, so both
    `.tmpl`-literal and `generator/render/keys.go`-generated binds are
    covered), e.g. `bind-note-assertions`: joins backslash-continued lines
    the same way `float-conf-assertions` does, then fails if any
    `^bind(-key)? ` line lacks `-N '...'` or `-N "..."` anywhere among its
    flags (order-independent — tmux/callers don't guarantee flag order).
  - `extraConfText` (`config/tmux.conf.nix`'s `extraConfText` argument) is
    copied verbatim into the rendered conf, so a downstream consumer's
    unrelated `extraConfig` bind (home-manager module callers, e.g.
    tmux-remux's hook wiring) could trip this check on content this repo
    doesn't own. Either scope the grep to end before the extra-config
    section, or add a comment on the check documenting the limitation,
    matching the comment style the neighbouring checks already use for their
    own scope caveats. The default build's `extraConfText` is empty, so this
    doesn't block `nix flake check` passing either way — just document the
    choice.
  - `config/tmux.conf.tmpl` has several `{{if ...}}`-gated bind blocks
    (splash `C-Space`, notify `n`, enrich `i`, resume-carousel) that only
    render into `generatedConf` when their option is on — a check built from
    just the default `tmuxConfig.tmuxConf` would miss an undescribed bind
    hiding in an off-by-default branch. Either run the new check across the
    same config matrix `tmux-conf-extraction-assertions` already builds
    (~`flake.nix:551`), or note this scope limitation in the check's own
    comment (matching the `extraConfText` comment above).
  - Verify: `nix build .#checks.<system>.bind-note-assertions` (or run it as
    part of `nix flake check`) — introduce a deliberate unnoted bind locally,
    confirm it fails, then remove the deliberate break.

- [ ] **Step 3: which-key Go TUI mode (new, self-contained model)**
  - Add a new file `picker/whichkey.go` with its own small bubbletea model
    (do not thread new fields through the existing `tuiModel` in `tui.go`,
    which already carries session/window/wall-specific state — a
    self-contained model keeps blast radius small and matches "pull
    complexity downward"). It:
    - Shells out to `tmux list-keys -N -a -F "#{key_table}|#{key_prefix}#{key_string}|#{?key_note,#{key_note},#{key_command}}|#{key_command}"` (pipe-delimited per this repo's tmux `-F` convention — see CLAUDE.md's "Key Conventions" on why formats must never use tabs/newlines) and parses rows into `{table, key, note, command}`.
    - Groups rows by `key_table`, sorted with `prefix`/`root` first then
      alphabetical (match the existing `-O key` sort tmux itself offers as a
      fallback ordering within a group).
    - Reuses `fuzzyScore` (`picker/tui.go:2660`) for filtering over
      `key + " " + note`, and `visibleWidth` (`picker/tui.go:2564`) for all
      column sizing — no `len()`-based padding (CLAUDE.md "Picker Chrome").
    - Invoke the `charm-tui` skill before writing render code — follow its
      border/overflow/resize rules.
  - `picker/main.go`: add a `--which-key` flag, route to the new model's
    entry point alongside the existing `--windows`/`--wall`/`--remote-pick`
    routing (call site `picker/main.go:121`, which dispatches into
    `runTUI` — defined at `picker/tui.go:251`, not in `main.go`). Update the
    usage comment at the top of the file.
  - `main.go`'s flag parsing (`main.go:116-120`) is a bare
    `map[string]bool` — it has no mechanism for a *valued* flag. Step 4 needs
    to thread the originating pane id into the which-key process; don't
    invent new flag-value syntax for this — follow the repo's existing
    precedent of an env var set by the wrapper script
    (`OG_PICKER_CURRENT_SESSION`, `OG_PICKER_EMIT` are the pattern) instead,
    e.g. `OG_PICKER_ORIGIN_PANE`, set by `scripts/tmux-which-key.sh` (step 5)
    before invoking the binary.
  - Verify: `go build ./picker/...` from the repo's Go module root; manual
    smoke test with `go run ./picker --which-key` inside a real tmux session.

- [ ] **Step 4: Enter executes the bind against the calling client** (implement: opus)
  - High-risk: a wrong mechanism silently breaks discoverability for every
    bind, and could re-run a command against the wrong pane/client or double
    up bridge routing.
  - Implement the resolve-and-replay mechanism from the "Verification note"
    above, but **first** prototype both candidates (`send-keys -K` against a
    real attached client, and resolve+replay via `list-keys -T` + `tmux -t`)
    on a scratch `TMUX_TMPDIR`-isolated server with a real pty client
    attached (e.g. via `tmux -L probe attach` under `script`/a test harness,
    not just `new-session -d`), and pick whichever actually reproduces a
    keypress's effect, including bridge-gated commands. Record the finding
    inline as a short comment at the call site (why this mechanism, not the
    other).
  - `list-keys` normalizes flag order in its echo and includes `-N
    '<note>'`, `-T <table>`, and `-r` (when set) ahead of the key token, not
    just the literal word `bind-key` — the strip-the-prefix logic that
    isolates the bare command must drop all of those flags/values, not just
    `bind-key`. Write this as a small parsing helper with its own test case
    (folds into step 7's test file) rather than an ad hoc string trim.
  - The popup must close *before* the resolved command runs (so the command
    isn't executing while its own popup pane still exists and could be
    targeted by `#{q:@bridge_pane}`-style pane-relative formats meant for the
    original pane) — thread the originating pane id in via
    `OG_PICKER_ORIGIN_PANE` (set by `scripts/tmux-which-key.sh`, step 5),
    the same env-var pattern `tmux-window-picker.sh`'s sibling scripts already
    use for `OG_PICKER_CURRENT_SESSION`/`OG_PICKER_EMIT`.
  - Verify: bind a throwaway test key with a visible side effect (e.g. a
    `display-message`), open the which-key popup, select it, confirm the
    side effect fires exactly as pressing the real key would. Then test one
    real bridge-gated bind (e.g. `z` zoom, or `|` split) against an actual
    mirror window connected to a real or scratch remote host, confirming it
    routes through `{{.BridgeGate}}` correctly (mirror-window "Done when"
    criterion).

- [ ] **Step 5: trigger + raw-list toggle**
  - Change `config/tmux.conf.tmpl:72` (and its mirror in
    `config/tmux.conf.reference.nix`, per step 1's rule) from the raw
    `list-keys -N -O key -F "..."` bind to launch the popup, following the
    exact pattern `tmux-session-picker`/`tmux-window-picker` binds use
    (`config/tmux.conf.tmpl:159-161`): add a new wrapper script
    `scripts/tmux-which-key.sh` modeled on `scripts/tmux-window-picker.sh`
    (same `--client`/`-c` pinning, same `@thm_overlay_1`/`@picker_layout`
    popup sizing lookup; also export `OG_PICKER_ORIGIN_PANE` from
    `#{pane_id}` per step 4). Add `tmux-which-key` to `scriptNames`
    (`config/tmux.conf.nix:276`) — no `@ICON_MAP@`/`@lib_*@` placeholders
    needed, so it does **not** go in `scriptsWithIcons`. `scriptNames` alone
    is not sufficient: `ogPartitionOk` (`config/tmux.conf.nix:718`) asserts
    that `scriptNames` and `ogVerbSpec` (`v.script` entries) + `ogInternal`
    are the same set, so every new script name must also land in exactly one
    of those two, or `nix build .#default` fails Nix evaluation with "og
    dispatcher partition mismatch". Follow the sibling pickers' precedent
    (`tmux-session-picker`/`tmux-window-picker` are `ogVerbSpec` entries —
    `"pick session"`/`"pick window"` around `config/tmux.conf.nix:601-607`,
    not `ogInternal`, since a human can invoke them directly via `og` for
    debugging): add a `"pick which-key"` (or similar) entry to `ogVerbSpec`
    with `script = "tmux-which-key"` and a one-line `summary`, alongside
    those two. Give the `?` bind a note too (e.g. `-N 'Show keybindings'`).
  - Register the new script in `generator/paths/paths.go`'s
    `RequiredScripts` (~line 33) — this is a separate list from
    `config/tmux.conf.nix`'s script wiring, consumed by the `--prefix`/
    `og-init` render path (`paths.New`, `Validate`). Without this entry the
    `--prefix` render path silently ships an empty path for
    `{{index .Paths.Scripts "tmux-which-key"}}` and the extraction check's
    `--prefix` smoke test (which only asserts non-emptiness generically,
    not per-script correctness) won't catch it.
  - Inside the which-key popup, add a keybind (e.g. `?` or `R`) that toggles
    to the raw `list-keys -N` output in place — do not add a second tmux
    bind for it.
  - **Update `tests/tmux-next38-readiness.bats:517-528`** (`"focus-follows-
    mouse, copy-mode-line-numbers, and the ? list-keys rebind are wired"`) —
    it currently asserts `list-keys -T prefix '?'` contains the raw
    `list-keys -N ... -F "..." -O key` command text, which this step
    replaces. Change the assertion to check the new bind
    (`run-shell '<...>/bin/tmux-which-key ...'` or equivalent) instead; if
    the raw-list format string still exists (now driving the in-popup
    toggle), assert it there rather than dropping the coverage.
  - Verify: `prefix + ?` opens the new popup in a live tmux session; the
    toggle key shows the raw list; `q`/`esc` closes as the other pickers do;
    `nix flake check` (step 8) picks up the updated bats case.

- [ ] **Step 6: mirror-window verification**
  - Manually confirm (can't be scripted in `nix flake check`): open the
    which-key popup from inside a `@bridge_win` mirror window (use the
    existing `og-remote-open`/bridge test setup, or the repo's scratch-server
    pattern with a bridged session), select a bridge-gated bind (`z`, `|`,
    `_`, `x`, `&`, `,`), confirm it executes against the *remote* pane via
    `{{.BridgeCtl}}` exactly as pressing the real key would, not against the
    local renderer pane.

- [ ] **Step 7: Go tests for list-keys parsing/grouping/filtering**
  - New `picker/whichkey_test.go`: table-driven tests parsing sample
    `list-keys -N -a -F ...` output (multiple tables, some rows with empty
    `key_note`, at least one bridge-gated multi-line/`if-shell`-wrapped
    command) into the row struct; a grouping test asserting table order and
    within-group ordering; a filtering test asserting `fuzzyScore` matches
    across key+note. Follow existing test conventions in
    `picker/tui_test.go`/`picker/render_list_test.go` (table-driven,
    `t.Run` subtests).
  - Verify: `go test ./picker/...` and confirm `picker-go-tests` (referenced
    from `nix flake check`) picks the new file up with no extra wiring
    (it globs `_test.go` files already).

- [ ] **Step 8: full verification pass**
  - `nix build .#default`
  - `nix flake check` (includes `bind-note-assertions`, `picker-go-tests`,
    `float-conf-assertions`, `tmux-conf-extraction-assertions`,
    `notify-conf-assertions`, `bridge-carousel-bind-assertions`,
    `bridge-tool-bind-assertions`, and the rest — step 1's grep-widening
    sub-bullet must already be applied or three of these go red on the `-N`
    additions).
  - `nix build .#lint`
  - All three must pass as separate commands (per CLAUDE.md's "Build and
    Test" — none subsumes another).

- [ ] **Step 9: commit the plan doc**
  - This file, `docs/superpowers/plans/2026-09-16-which-key-popup.md`, is
    committed alongside the code in the same PR (already true once step 8's
    commits land, since it was written into the worktree at the start).

- [ ] **Step 10: rebase before opening the PR**
  - A parallel worker (#649) is also editing `config/tmux.conf.tmpl`
    (`pane-border-format`, `pane-scrollbars`) and `picker`'s
    `unquoteTmuxOptValue`. Rebase this branch onto `main` immediately before
    opening the PR so the two diffs merge cleanly — this branch's `.tmpl`
    diff is scoped to bind lines only (step 1), which should rebase without
    conflicts if #649 hasn't also touched those exact lines. Note the
    conflict surface is larger than just `.tmpl`: since
    `config/tmux.conf.reference.nix` mirrors `.tmpl` byte-for-byte (step 1),
    if #649's `.tmpl` changes touch the reference file too, expect to
    resolve both halves of that pairing during the rebase, not just the
    template.

## Done when (from WORKER_TASK.md)

- [ ] Every custom bind has a description, and the flake check enforces it.
- [ ] `prefix + ?` opens the popup, and Enter on a row does what pressing
      that bind does.
- [ ] The popup works inside a mirror window.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` all pass
      (three separate commands).
- [ ] Go tests for list-keys parsing/grouping/filtering exist and pass.
- [ ] This plan doc is committed under `docs/superpowers/plans/`.

## Open questions for the critic

1. Step 1's coverage of `generator/render/keys.go` float-bind call sites (and
   the mirrored `config/tmux.conf.reference.nix` edits this now requires)
   goes beyond the literal wording of WORKER_TASK.md's "every bind in
   config/tmux.conf.tmpl" — scoped in because "every bind has a description"
   only holds true end-to-end if the float binds are covered too, and the
   flake check naturally scans the fully rendered config. Flag if this is
   considered scope creep.
2. Step 4's mechanism is not settled by research alone — the scratch-server
   test showed `send-keys -K` not firing without a real attached client, but
   that could be a scratch-server artifact rather than a fundamental
   limitation. The step is tagged `implement: opus` and includes an explicit
   prototype-both-and-pick instruction rather than prescribing one.

## Revision 1 (addressed plan-critic blocking findings)

Verified against the actual repo and corrected: `config/tmux.conf.nix` only
wires up the Nix build (imports `generator/`, builds script/paths tomls); the
real bind-generating Go code is `generator/render/keys.go`
(`floatBind`/`mkFloat`), and `config/tmux.conf.reference.nix` is a separate,
frozen byte-identity oracle that must be edited in lockstep with both
`config/tmux.conf.tmpl` and `generator/render/keys.go` for every bind change
(steps 1, 4, 5, 10 updated accordingly). Also added: the bats test update
(`tests/tmux-next38-readiness.bats`, step 5), the `generator/paths/paths.go`
`RequiredScripts` registration (step 5), the `extraConfText` scoping note
(step 2), the `OG_PICKER_ORIGIN_PANE` env-var threading in place of a new
flag syntax (steps 3-4), and the `list-keys` flag-stripping precision note
(step 4).

## Revision 2 (addressed remaining plan-critic blocking finding)

Round-1 findings were confirmed fully resolved. Round 2's one blocking
finding: adding `tmux-which-key` to `scriptNames` alone is insufficient —
`ogPartitionOk` (`config/tmux.conf.nix:718`) asserts `scriptNames` equals
`ogVerbSpec`'s script set plus `ogInternal`, so the new script also needs an
entry in one of those two or `nix build .#default` fails Nix evaluation.
Step 5 now adds it to `ogVerbSpec` (following the `tmux-session-picker`/
`tmux-window-picker` precedent — a human-invocable `og` verb, not
`ogInternal`). Also fixed: step 2's verify command now uses the correct
`checks.<system>.<name>` attr path, the config-matrix scope limitation for
`bind-note-assertions` against `{{if}}`-gated binds, the existing
bridge-else-branch `-N` note precedent (step 1), and a cosmetic line-number
correction in step 5.

## Escalated (revision cap reached)

A third critic pass (beyond the 2-revision cap) found one more genuine
blocking gap: adding `-N` shifts the key token in three bind lines that
`flake.nix`'s `notify-conf-assertions` (`:424`),
`bridge-carousel-bind-assertions` (`:1940-1941`), and
`bridge-tool-bind-assertions` (`:1955-1956`) grep anchored on key-adjacency —
all three would go red once the `-N` pass lands. Per the revision cap this
was **not** sent back for a 4th critic round; the exact, fully-specified fix
(widen each grep to `bind[-key]( -N '[^']*')? <key> ...`, applied in the same
commit as the `-N` change) has instead been folded directly into step 1 and
flagged again in step 8's verify list above. This section exists so the fix
is visible as critic-unreviewed if anything about it turns out wrong during
execute — the implementer should re-check the three grep sites against
`flake.nix` at execute time rather than trusting the line numbers blindly,
since they may have shifted.
