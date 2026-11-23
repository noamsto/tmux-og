# tmux upstream review (pin a94699c3 → master 6a32033b)

Reviewed 2026-11-23. Nothing was posted upstream; every issue, PR and comment mentioned below was only read.

## Summary and recommended pin

- **Range:** 162 commits after the pin (72 on the first-parent line). The version string is still `next-3.9`.
- **Recommended pin:** `b243b8ec2e5facf787d8a7d6dbe04f68f7bd5f35` (short `b243b8ec`), the 2026-11-23 merge of the OpenBSD tree, rather than master HEAD. It carries every functional change in the range; master HEAD `6a32033b` adds only `bench/` and a `.gitignore` line on top. A throwaway build of it compiled with our patch and configure flags and reports `tmux next-3.9`; no test suite was run against it.
- **Do not bump blindly.** Four commits break something tmux-og relies on (verdict `breaks us`), and each needs a small tmux-og change first: `0a3b2a1a` (float hide/show restore), `541bc85e` (float `resize-pane -y` compensation), `d13ee046` (stock pane menu tripwire) and `e644e74b` (5 ms output batching, a keystroke-latency risk).
- **Fallback pin:** `f4146fd6` (2026-11-21 merge) holds the same three layout and menu breaks but predates the output batching, the backoff grace, `disable-backoff` and `client_pending` (`9bf987ea`, which the latency probe is to use), the synchronized-update rewrite and the OSC 7501 forwarding.
- **Verdict counts** (35 commits with a verdict row): `breaks us` 4, `adopt` 5, `drop workaround` 1, `ignore` 25. Two rows also touch a workaround: `541bc85e` lets `W19` go, and `0a3b2a1a` makes the `W33` comment stale.
- **Watch rows from the 2026-09-24 survey:** `d202aa7a` and `4624bc21` already landed inside the current pin; the hide-pane PR (`b8cadef8`, tmux#5402) is still open. Details under Prior watch rows.
- **Pane lifetime:** upstream has no parent/child pane lifetime and nobody asked for one. Recommendation: do not post; tmux-og has no measured case.

## Method and range

- **Which pin is the wrapped binary.** The root flake input `tmux-upstream` (`flake.nix:23`) resolves in `flake.lock` to node `tmux-upstream_2` at `a94699c3108fe1467446d62b2b0532bffd8bd4ad`. `mkTmux` builds it through `src = inputs.tmux-upstream` (`flake.nix:66-69`). The lock node literally named `tmux-upstream` at `d5afb67a81d8a30379e0d4186ec4b968244393bf` is tmux-remux's transitive input and does not affect the wrapped binary.
- **Range.** `git log a94699c3..origin/master` in a clone under the session scratch area: 162 commits, 72 on `--first-parent`. Master was `6a32033b` (2026-11-23) when fetched.
- **Method.** Each commit was read from `git show --stat` and, for anything touching floats, layout, formats, menus, control mode, hooks or output timing, from its diff. Each was then matched against tmux-og with `rg` over `scripts/`, `picker/`, `config/`, `generator/`, `docs/agents/`, `tests/`, `patches/`, `flake.nix` and `modules/`. A commit with a tmux-og consumer gets one row below. A commit with none is counted as skipped and listed in the appendix. A merge commit that only rolls up commits already on a row is skipped too, because the row of the commit it merges carries the verdict.
- **Portable commits.** Every first-parent commit in this range has `configure.ac` (checked with `git cat-file -e <sha>:configure.ac`, and with `gh api repos/tmux/tmux/contents/configure.ac?ref=<sha>` for `b243b8ec`, `6a32033b`, `71afaaff` and `f4146fd6`). The plain OpenBSD-tree commits are the second parents of those merges and have no `configure.ac` of their own; they are not pinnable. The Nov 10-20 fuzzer-branch merges (`90a89024`, `b2a01282`) are portable and only add `fuzz/` targets guarded by the fuzzing `check_PROGRAMS` list.
- **Caveats.** The blobless clone and `gh` were the only sources. The mailing lists were not searched in depth (see Pane lifetime upstream).

## Commits by area

Verdicts: `adopt` (bump and use), `drop workaround`, `breaks us` (must handle on bump), `ignore` (checked against the tmux-og path, no action).

### Floating panes

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `0a3b2a1a` | `layout_cell_get_neighbour` prefers the preceding tiled sibling, so a closed, floated or hidden cell gives its space to the previous cell, not the next | `scripts/tmux-float-group.sh:173-190` | breaks us |
| `541bc85e` | `resize-pane -y` adds the pane-border-status row only for tiled panes without separate borders; floats are exempt | `picker/remotebridge/daemon/floatgeom.go:73-96` | breaks us |
| `377dcdfc` | `join-pane -f` inserts the full-size pane at the list head or tail so pane indexes stay ordered | `scripts/tmux-float-group.sh:190` | ignore |
| `b09b7526` | scrollbar minimum size now comes from the pane style (part of the separate-borders work) | `tests/tmux-next38-readiness.bats:621` | ignore |
| `ea086e49` | layout cell spread code reorganised for separate borders (refactor, no behaviour change when joined) | `picker/remotebridge/daemon/floatgeom.go:9-60` | ignore |
| `302c5bb3` | `layout_apply_borders` and related helpers added for separate borders | `picker/remotebridge/daemon/floatgeom.go:9-60` | ignore |

### Scene renderer

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `13f9aa0d` | new window option `pane-border-type` (`joined`, `separate`, `separate-active`); `pane_at_*` and `pane_unzoomed_*` change only when it is not `joined` (default `joined`) | `config/tmux.conf.tmpl:342` | ignore |
| `f3e9a8a5` | repairs `window_pane_find_up/down` gap handling that the previous merge got wrong; a pin must include it (merge `71afaaff` or later) | `scripts/tmux-smart-nav.sh:131` | ignore |
| `84b41334` | `dim=` is honoured in `pane-border-style` and `pane-active-border-style` | `scripts/tmux-apply-theme-colors.sh:163` | ignore |
| `55ce5fb0` | `window_visible_ranges` checks the pane's rows before the border-style lookup (restructure, same result) | `scripts/lib-float-cover.sh` | ignore |

### Hooks

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `4e007547` | `cmdq_insert_hook` sets the hook payload target only when the current find-state is valid | `config/tmux.conf.tmpl:730` | ignore |

### Formats

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `434ab3a2` | minimal OSC 7501 program status: panes store state, name and message; new formats `pane_program_state`, `pane_program_name`, `pane_program_message` | `docs/agents/agent-state.md` | adopt |
| `9e5702fb` | forwards the active pane's OSC 7501 to the outer terminal; adds a `programstatus` token to `client_termfeatures` | `docs/agents/bridge-graphics-paste.md:39` | ignore |
| `d296a6ea` | `#{S:}`, `#{W:}`, `#{P:}`, `#{L:}` loops and `sort_get_*` stop using static lists, which fixes a use-after-free with nested loops | `docs/agents/bridge-shipped-state.md:166` | adopt |

### Popups and menus

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `d13ee046` | the stock pane menu drops its copy-mode `Edit` item (key `e`) | `generator/render/menus.go:89` | breaks us |
| `219c6414` | `list-keys -N` text of prefix `(` and `)` reworded; no binding change, and the readiness key-note case checks `,` `{` `}` `!` `@` and other `-N` notes, not `(`/`)` | `tests/tmux-next38-readiness.bats:296` | ignore |

### Perf

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `e644e74b` | pane output is flushed at once when no batch timer is pending, then batched behind a 5 ms timer (`screen_write_start_batch`) for writes that follow; alternate screen with sync seen is exempt | `tests/perf/keystroke-latency.sh` | breaks us |
| `a796765e` | synchronized-update bitmaps replaced by the normal collection path with a `REDRAW` item (large `screen-write.c` rewrite) | `docs/agents/performance.md` | ignore |
| `60457319` | 10 ms grace before a slow client enters output backoff | `docs/agents/performance.md` | ignore |
| `9bf987ea` | server option `disable-backoff` and format `client_pending` for repeatable perf runs | `tests/perf/keystroke-latency.sh` | adopt |
| `8f6107a1` | fast path for runs of ASCII on input | `docs/agents/performance.md` | ignore |
| `b016814c` | `screen-write` tracks the written line range and loops only over it | `docs/agents/performance.md` | ignore |

### Fixes

| sha | change | tmux-og path | verdict |
|---|---|---|---|
| `ad77e4a7` | the built-in `kitty` terminal features now include `progressbar` | `config/tmux.conf.tmpl:73` | drop workaround |
| `e6211b41` | control-mode clients start with `tty.fg`/`tty.bg` = -1, so colours are initialised before attach; the remote tmux can then answer OSC 10/11 queries from the daemon control client's tty colours (the reply path described at `picker/remotebridge/keyneg/strip.go:6`) | `picker/remotebridge/keyneg/strip.go:6` | adopt |
| `608f3078` | `control.c` discards a pane's blocks when ignoring the pane, so offsets cannot point outside the data | `picker/remotebridge/daemon/daemon.go:2463` | adopt |
| `5fdad439` | control clients can no longer be suspended | `picker/remotebridge/main.go:49` | ignore |
| `b555de74` | `%N`/`@N` arguments of `refresh-client -A/-r` and the monitor `-B` target reject trailing characters | `generator/render/status.go:140-147` | ignore |
| `fa1c0e5b` | the pane gets focus-out when its client's terminal goes away | `picker/remotebridge/daemon/focus.go:22-45` | ignore |
| `8e3bb660` | a bare `@` or `{current}` is accepted as the window part of a target | `docs/agents/tmux-gotchas.md` | ignore |
| `71fd946a` | `send-keys -K` clears `KEYC_SENT` early so the key reaches the pane | `picker/whichkey.go:241-262` | ignore |
| `76183990` | a client's stdin can be given to `source -` once per process (client-side flag) | `scripts/tmux-update-icons.sh:686` | ignore |
| `50893828` | `capture-pane -e` reopens a hyperlink on continued lines | `picker/tui.go:4318` | ignore |
| `a04f41ab` | `screen_resize` frees inline images | `docs/agents/bridge-graphics-paste.md` | ignore |
| `853977c7` | UTF-8 width table fixes and missing combined characters | `scripts/lib-icons.sh:25-60` | ignore |
| `fb035009` | more missing emoji widths | `scripts/lib-icons.sh:25-60` | ignore |

## Prior watch rows

Re-checked from the 2026-09-24 survey (`docs/superpowers/specs/2026-09-24-tmux-upstream-survey.md`).

- **`b8cadef8`, hide-pane and show-pane, tmux#5402: still open.** `gh pr view 5402` reports state `OPEN`, draft, head still `b8cadef8`, last updated 2026-10-17, not merged. No commit in the range adds `hide-pane`, `show-pane` or any `hidden` pane state (`git log --grep` over the range finds none; there is no `cmd-hide-pane.c`). The active vehicle looks like the non-draft stack tmux#5668 (zoomed and floating panes) → tmux#5669 (hidden panes) → tmux#5856 (new `pane-floated`, `pane-tiled`, `pane-hidden`, `pane-unhidden` hooks), all open, last updated 2026-11-13, so #5402 is effectively superseded by #5669 as the likely merge path, but nothing is on master. Verdict: keep watching; the stash-window workaround in `scripts/tmux-float-group.sh` stays (see `W23` below). The partial piece that did land is `0a3b2a1a`, which breaks that same script.
- **`d202aa7a`, `remain-on-exit failed-key` and `new-pane -D`: landed before the pin.** It is on master (2026-09-10) and already an ancestor of `a94699c3`. tmux-og already reads `failed-key` in `picker/remotebridge/daemon/daemon.go:3899-3903`; nothing in the range touches it. Verdict: ignore, already adopted.
- **`4624bc21`, zoomed v2 layout dumps emit `z` for every float (tmux#5624): landed before the pin.** On master (2026-09-22) and an ancestor of `a94699c3`; tmux#5624 is closed as completed. `W26` and `W61` (`picker/remotebridge/daemon/reconcile.go:78-90`) still stand, because no format reports which tile holds the zoom; nothing in the range changes that. Verdict: ignore.
- **Other upstream references in the workaround inventory.** tmux#5135 (floating panes discussion) is closed, completed 2026-10-08. tmux#5582 (`layout_clamp_floating_panes`) is closed and in the pin. tmux#5390 (`new-layouts`) is closed. tmux#5336 and tmux#5398 are closed and in the pin. tmux#5551 (popup on a control client) is closed, but its fix only made popups a no-op for control clients, so `patches/tmux-display-popup-control-client.patch` is still needed (it applies cleanly at `b243b8ec`). tmux#4594 (`show-options -t =name`) is closed with no fix commit in the range, so the `=name` workaround stays (`W42`). tmux#5433 (per-pane borders) closed on 2026-11-17, the day `13f9aa0d` landed.

## Workarounds that can drop

Ids refer to the inventory of tmux-og workarounds gathered for this review.

- **`W19`, float `resize-pane -y` row compensation.** Path `picker/remotebridge/daemon/floatgeom.go:73-96` (`floatResizeTrigger`, `floatResizeCmd`); doc `docs/agents/floats.md:75`. Upstream `541bc85e` (in merge `440ed9b7`) makes floats exempt from the border-status bump. Drop condition: every tmux the daemon drives, local and remote, is at or after `440ed9b7`. Until then the compensation must be version-gated at each site on the rev of the server that runs it (remote version for `floatGeomCommand`, local for `floatResizeArgv`), because at the new rev it over-compensates by one row (see Breaks us on bump).
- **Progressbar feature, `config/tmux.conf.tmpl:73` and `config/tmux.conf.reference.nix:658`.** Upstream `ad77e4a7` adds `progressbar` to the default features of kitty. Drop condition: kitty answers XTVERSION (the built-in list is chosen by that name), so keep the override while a `TERM=xterm-kitty*` client that does not answer must still get the feature. A resident pre-bump server (#407) that reloads a config without the line loses `progressbar` for every kitty client.
- **`W33`, "kill-pane gives the space to the next cell" comment and forced re-hold.** Path `picker/remotebridge/daemon/hold.go:213-232`; doc `docs/agents/bridge-daemon.md:614`. Upstream `0a3b2a1a` makes next-3.9 give a closed pane's space to the previous cell, like 3.6a and 3.7c. The comment becomes false at the new rev. The forced re-hold (`force := structural`) should stay until every remote is at that rev, because a remote on an older build still gives the space to the next cell.
- **`W02`, stale `tmuxPackage` default text.** Path `modules/home-manager.nix:251-261`. Not caused by this range: it still names a 3.6a pin and tmux#5336, both long resolved (see the survey and `docs/upstream-tmux.md`). Comment-only cleanup.
- **Not droppable yet:** `W01` (popup patch: bail still present, no upstream PR found, see Bump risk list), `W23` (hide-pane not landed), `W42` (tmux#4594), `W26` and `W61` (zoom unreadable), `W10` (modal clear: no change).

## Breaks us on bump

- **`0a3b2a1a`, float hide and show restore.** `tile_target` and `restore` in `scripts/tmux-float-group.sh:173-190` rely on the old rule that a closing cell hands its space to its next sibling: they join the float back with `join-pane -b` before the target tile, then float it out again to get the space back. With the new rule that space goes to the preceding sibling. `join-pane -b` (default `-v`) inserts the new cell into the target's existing parent only when the tile sits in a top/bottom stack, so the layout changes after a hide and show round trip only for a target tile that is not first in such a stack; in a left/right row `-b` creates a new `[new, TILE]` node and the space still returns to the tile, so nothing changes. Join after the target tile (no `-b`) on the new rev, version-gated for older resident servers. `tests/float-group.bats:431-446` uses the first tile as the target (no preceding sibling, falls back to the following one), so it may not catch the break; add a case with a later tile of a top/bottom stack active.
- **`541bc85e`, float resize compensation.** `floatResizeCmd` asks for one row less when `floatResizeTrigger` is true (`picker/remotebridge/daemon/floatgeom.go:73-96`). It has two call sites that run on different servers: `floatGeomCommand` (`picker/remotebridge/daemon/ctl.go:410-414`, the `float-geom` verb's `build` at `ctl.go:754`) builds a command that runs on the remote tmux, and `floatResizeArgv` (`picker/remotebridge/daemon/reconcile.go:991`, `ctl.go:765`, the `buildLocal` path) runs on the local one. At the new rev floats no longer get the extra row, so a mirror float resized under `pane-border-status top` or `bottom` ends one row short. Gate each site on the rev of the server that runs it: the remote version for `floatGeomCommand`, the local one for `floatResizeArgv`. A single local gate would size a float one row wrong in a mixed fleet that straddles `440ed9b7` (merge of `541bc85e`).
- **`d13ee046`, stock pane menu.** Three entries of `generator/render/stockmenus.txt` change (see tripwire results). `paneMenuLocalItems` in `generator/render/menus.go:89` still emits `Edit`, and `menu-bind-integration-tests` (`flake.nix` ~3411-3452, `tests/menu-bind-integration.bats:500-515`) fails until the snapshot is regenerated. The mirror menu's `Reconnect e` collision note (`generator/render/menus.go:100-104`) becomes stale.
- **`e644e74b`, 5 ms output batching.** `screen_write_stop` flushes pending output at once when no batch timer is pending and arms the 5 ms timer only for writes that follow inside the window (`b243b8ec:screen-write.c`, `screen_write_start_batch`), so an isolated echo is not delayed. The risk is an echo that lands while the pane is already streaming (an agent printing) or that is split over several writes: it waits up to one 5 ms tick, which adds to keystroke echo latency. `docs/agents/performance.md` and `tests/perf/keystroke-latency.sh` hold the baseline; re-run before pinning, and include a case that measures echo while the pane prints streaming output, not only an idle shell. If the numbers regress, pin `f4146fd6` instead (it lacks `9bf987ea`, so the probe would run without `disable-backoff` and `client_pending`).

## Recommended pin

- **Rev:** `b243b8ec2e5facf787d8a7d6dbe04f68f7bd5f35` (merge, 2026-11-23, `configure.ac` present, `AC_INIT([tmux], next-3.9)`). The fallback `f4146fd6031f3c7c7bd32b4cefa9d6204a12655d` is also a merge with `configure.ac`.
- **Why portable:** it is a first-parent merge commit; `git cat-file -e b243b8ec:configure.ac` succeeds and `gh api repos/tmux/tmux/contents/configure.ac?ref=b243b8ec` returns the file (29273 bytes). The OpenBSD-tree commits it merges are not pinnable alone.
- **Why this one:** it is the last merge point; master HEAD `6a32033b` differs only by `bench/` (52 files) and `.gitignore` (53 changed files). It includes `71afaaff` (so the `pane-border-type` repair is in: never pin at `161c4e3c`) and the `b2a01282` fuzzer merge.
- **Build:** tested as a throwaway `nix build` outside the worktree flake, using the lock's nixpkgs rev, `src` set to a `git archive` of `b243b8ec`, our popup patch and `--disable-asan`, libpng and zlib. It compiled and printed `tmux next-3.9`. The repo test suites (`nix flake check`) were not run against it.
- **Conditions:** do not bump before the four `breaks us` rows are handled; the first three are small, the fourth needs a latency measurement.

## Bump risk list

- **Stock menu tripwire (6a).** `git diff a94699c3..b243b8ec -- key-bindings.c` changes exactly the `Edit` item (`d13ee046`) and two `list-keys -N` note strings (`219c6414`). In `generator/render/stockmenus.txt` the entries that change are `prefix >`, `root MouseDown3Pane` and `root M-MouseDown3Pane` (each loses `"#{?#{==:#{pane_mode},copy-mode},Edit,}" e ...`). `prefix <`, the status, status-left and empty-area menus, and every line of `generator/render/stockdrags.txt` are unchanged. `stockMenuVersion = "next-3.9"` (`generator/render/menus.go:24`) still matches, so the `%if` gate does not hide the mismatch: regenerate `stockmenus.txt`, then drop `Edit` from the two hand-maintained copies of `paneMenuLocalItems`: `generator/render/menus.go:89` and `config/tmux.conf.reference.nix:194-198`. `menuStockLines` (`config/tmux.conf.reference.nix:147`) only reads `stockmenus.txt` and follows it. `TestPaneMenuLocalItemsAreStock` (`generator/render/menus_test.go:466`) also fails after the snapshot is regenerated until `Edit` is dropped.
- **Popup patch (6b).** `git apply --check patches/tmux-display-popup-control-client.patch` against a `git archive` of `b243b8ec` succeeds (the patch touches only `cmd-display-menu.c`, unchanged in the range; `popup.c` does not exist at the pin or at `b243b8ec`; `--reverse` fails, so the bail is not upstream). The patch text says "Submitted upstream", but an author search of tmux/tmux issues and PRs finds no change proposing to remove the bail (only the closed #5551 report), so it will not drop by itself.
- **Live mkTmux checks (6c).** `tests/tmux-next38-readiness.bats` popup cases (lines 175, 200, 229) are unaffected (the popup patch's file, `cmd-display-menu.c`, is untouched). The key-note case (line 296) checks the notes and bindings of `,`, `{`, `}`, `!`, `@` and the `-N` entries; it says nothing about `(` or `)`, so the `219c6414` rewording does not affect it. The `I/f:sixel` cases (677-690) still hold; `9e5702fb` only adds a feature token. Geometry-sensitive suites to re-run: `tests/float-group.bats`, `tests/float-refit.bats`, `tests/float-nudge.bats`, `tests/float-drag-integration.bats`, `tests/dwindle.bats`, `tests/grid-refit.bats` (neighbour rule and `resize-pane -y`), `tests/pane-border-format.bats` (`pane_at_*`, unchanged while `pane-border-type` is `joined`), and `tests/menu-bind-integration.bats` (stock tripwire).
- **Version string (6d).** `git show b243b8ec:configure.ac` still has `AC_INIT([tmux], next-3.9)`, equal to `version = "next-3.9"` (`flake.nix:68`) and `stockMenuVersion`; no release branch point in the range, so `versionCheckHook` still passes and no string needs to change.
- **NixOS packaging.** `Makefile.am` gains four fuzz targets under the fuzzing-only `check_PROGRAMS` list and `image-grid.c`/`tmux-image.h` change; neither affects `pkgs.tmux.overrideAttrs` (the throwaway build used the existing `autoreconfHook`). A resident tmux server older than the rebuilt binary must keep the `#{version}` gates (#407): all the gating advised above is for that case.
- **Performance.** `e644e74b`, `60457319` and `a796765e` all landed 2026-11-22, one day before the recommended rev. They change output latency, backoff and synchronized-update redraw; expect follow-up fixes upstream, so check `git log b243b8ec..origin/master -- screen-write.c tty.c input.c` again before the bump.
- **`pane-border-type`.** Off by default; if a user sets it to `separate`, `pane_at_*` (smart-nav, `config/tmux.conf.tmpl:342-345`) and the 2/+1 float inset arithmetic (`picker/remotebridge/daemon/floatgeom.go:9-60`) change. tmux-og never sets it.
- **Stricter ids.** `b555de74` rejects `%12x` style trailing characters; tmux-og passes clean ids (`picker/remotebridge/daemon/daemon.go:2463`, empty `-B` target in `generator/render/status.go:140-147`), so no change.

## Pane lifetime upstream

Searched 2026-11-23 against master `6a32033b`, GitHub issues and PRs, and a web search. Nothing was posted.

### Findings

- **No such feature.** `git grep` over the clone for `parent`, `owner`, `creator`, `spawner` and `PANE_(CHILD|PARENT|OWNED|STICKY|DEPEND)` finds no pane-to-pane relation, not even on the 23 remote branches (`floating_panes`, `zoomed-floating-panes`, `hidden-panes`, `new-pane-hooks`, `layouts`, `pane-redistribution`, `never-above-zoom`, ...). `layout_cell->parent` is layout structure, not a pane relation. `struct window_pane` has only list links.
- **Nearest mechanism:** `new-pane -W` blocks the queuing command until the pane exits; `-O` is modal (one per window); `choose-tree -k`, `-D` and `-C` are self-closing. None ties a pane's lifetime to another pane. `wait-for -E -F <filter> <event>` (events landed in `d29aa121`, 2026-07-10, and are in the current pin) blocks one waiter until a matching event fires, and `wait-for -E -F '#{==:#{pane},%N}' pane-exited` (or `pane-died`) therefore works today per pane for process exit; tmux-og already uses the same form for `pane-resized` (`picker/agentdetect/geometry.go:33`, `docs/agents/tmux-gotchas.md:24`). The gap is structural kills: no `pane-closed` event exists in master (`git grep pane-closed b243b8ec` finds nothing), and `kill-pane`, `kill-window` and `respawn-pane -k` fire neither `pane-exited` nor `pane-died`; only `pane-created` fires from `spawn.c`.
- [tmux#5384](https://github.com/tmux/tmux/issues/5384) (closed, locked): junegunn (fzf) wanted only the parent pane to wait for a floating pane. nicm: "If you run this from a key binding then there is no 'parent pane'", and pointed to `new-pane -W`, `select-pane -d`, `TMUX_PANE`, and `wait-for -E pane-closed` with `-F` on the `events` branch. His comment is dated 2026-07-13, while the events work was still landing (`events.c` arrived in `d29aa121` on 2026-07-10), so "build the `events` branch" described the state of the day; `events.c` and `wait-for -E` are in the pin now, but `pane-closed` was never added.
- [tmux#5135](https://github.com/tmux/tmux/issues/5135) (closed): the floating panes discussion; no hit for parent, owner or child lifetime.
- [tmux#5219](https://github.com/tmux/tmux/issues/5219) (open, "Layouts rework"): maintainer wishlist for floats (always-on-top, sticky position, keep-bottom, preferred size); no ownership item.
- [tmux#5406](https://github.com/tmux/tmux/issues/5406) (open, 3.9 and 4.0 roadmap): zoomed and floating panes, hidden panes, layouts; no ownership item.
- [tmux#5856](https://github.com/tmux/tmux/pull/5856), [#5668](https://github.com/tmux/tmux/pull/5668), [#5669](https://github.com/tmux/tmux/pull/5669), [#5402](https://github.com/tmux/tmux/pull/5402) (open): hooks and hidden panes; hooks only, no close propagation.
- [tmux#5535](https://github.com/tmux/tmux/issues/5535) (closed): linking one terminal into several panes; nicm wanted to avoid primary and secondary roles. Weak evidence of the same stance.
- Context only: [#5083](https://github.com/tmux/tmux/issues/5083) (persistent popups), [#5443](https://github.com/tmux/tmux/issues/5443) (popups deprecated in favour of floats), [#5258](https://github.com/tmux/tmux/issues/5258) (floats and zoom).
- `server_destroy_pane` fires `pane-died` or `pane-exited` and then removes the pane; structural kills (`kill-pane`, `kill-window`, `respawn-pane -k`) fire neither (measured and noted in `config/tmux.conf.tmpl:724-733`).

### Searches that returned nothing

- `gh search issues` and `gh search prs --repo tmux/tmux` for "parent child pane", "pane lifetime" (PRs), "close children", "owner pane", "kill-pane children" (PRs), "pane dependency" (only systemd pane-scope PRs), "popup tied to pane", "kill floating when parent exits", "floating pane belongs to pane" and similar phrasings: zero relevant rows.
- A local scan of the 1577 issues and PRs updated since 2025-01-01 (title and body) for parent, owner, child, close-with, tied, bound or attached next to float, popup, new-pane or modal: no hit. Comments of 18 float-related threads: only #5384 above.
- `git log origin/master --all-match -i --grep` for the same terms: no commit introduces a pane relation.
- No `TODO` file exists in the clone.
- **Mailing list coverage is thin.** Only web-search snippets were reachable: older popup threads on tmux-users and nothing on pane close propagation. tmux-dev was not searched in depth and no archive search was available. GitHub issues serve as the discussion venue (#5135, #5406).

### Recommendation

Do not post an upstream issue now. nicm answered the nearest request with existing building blocks (`new-pane -W`, and `wait-for -E` with a pane filter, which is in master) and "there is no parent pane", and 3.9 is a layout, zoom and hide release in which new pane state meets resistance. tmux-og has no measured case: no code writes a `@float_owner` option (`rg '@float_owner'` finds nothing), and the lifetime links that exist are different in kind. `reap_orphans` (`scripts/tmux-float-group.sh:86`) kills a stashed float whose home window is gone; the bridge daemon owns mirror floats itself (`picker/remotebridge/daemon/reconcile.go`); the crew dash floats are toggled by `@pane_label`, not tied to a pane. The structural-kill gap is already covered by the sweep backstop noted in `config/tmux.conf.tmpl:724-733`. If a real case turns up, the cheaper and more general ask is a `pane-closed` event that also fires for structural kills; the owner flag below is the heavier version.

The draft's use case is hypothetical: its wording ("We keep a pane option `@float_owner` and a `pane-exited` hook...") describes a design tmux-og does not have. Say so plainly: no measured tmux-og case exists. Replace the Use case section with a real, measured example, or do not post.

### Draft issue (NOT POSTED)

**Title:** `new-pane: option to close a floating pane when the pane it was opened from is destroyed`

**Body:**

> ### Use case
>
> A tool that opens a floating pane on behalf of a pane (a diff viewer, a PR dashboard, or a helper agent view for the shell or an AI agent
> running in that pane) has no way to say that the float belongs to that pane. When the owning pane is destroyed (process exit, `kill-pane`,
> or its window going away), the float stays behind on whatever window is left, still holding a process and screen space. We keep a pane option
> `@float_owner` and a `pane-exited` hook that kills matching floats. It misses `kill-pane`, `kill-window` and `respawn-pane -k`, which fire no pane
> hook, so we also sweep from a monitor hook. [Replace with a measured example from tmux-og before posting; as of this draft there is no such
> case.]
>
> This is different from `new-pane -W` (the *command* blocks, the float is still not tied to the pane) and from `-O` (modal, one per window,
> blocks the other panes).
>
> ### Proposed minimal API
>
> `new-pane -o`: the new pane is owned by the target pane (`-t`, or the active pane). When the owner is destroyed, for any reason, the owned
> panes are destroyed with it (no `remain-on-exit` corpse). One level only; an owned pane may not be an owner. Flag letter is bikesheddable
> (`-o` is unused in `new-pane`). Plus a format `#{pane_owner}` (the owner's pane id, empty if none) so scripts can see the link, and so
> `list-panes -f` can filter on it.
>
> Out of scope: any "primary/secondary" link between tiled panes (see #5535).
>
> ### Alternatives considered
>
> - `pane-exited` hook plus a user option: misses structural kills, and the hook runs outside the destroying code path, so there is a window in
>   which the orphan is visible.
> - `new-pane -W` with `-O`: gives modal blocking, not lifetime. It does not help for a non-modal float, which is the point of floats over popups.
> - `wait-for -E -F '#{==:#{pane},%N}' pane-exited` (events are in master): needs a long-lived waiter process per float and misses structural
>   kills (`kill-pane`, `kill-window`, `respawn-pane -k`), because no `pane-closed` event fires for them.
>   A `pane-closed` event that also fires for structural kills would meet most of this need and be more general. Happy to take that instead.
> - Zellij's `--blocking` / close-with-parent behaviour: relevant prior art (raised in #5384), and the same limit (parent is only defined for
>   callers that run inside a pane; none exists for a key binding or `run-shell`). Hence "target pane or active pane" above, and a no-op when there is
>   no target.
>
> ### Open questions
>
> - What should happen on `swap-pane`, `join-pane` and `break-pane` of the owner? (Suggest the link follows the pane id.)
> - Should owned panes be hidden or stashed (#5402) with the owner?
> - Should a zoomed or marked pane change anything? (Suggest no.)
>
> *Disclosure line, if posted with AI help, in the same style as our earlier #5219 comment.*

## Candidate follow-up issues

One line per recommended tmux-og change, as `title — verdict — doc path`. None is filed.

- Restore floats after hide and show without `join-pane -b` on tmux at or after `0a3b2a1a` (version-gated) — breaks us — `docs/agents/floats.md`
- Version-gate the float `resize-pane -y` row compensation and drop it once all tmux is at or after `541bc85e` — breaks us — `docs/agents/floats.md`
- Regenerate `stockmenus.txt` and drop `Edit` from `paneMenuLocalItems` for `d13ee046` — breaks us — `docs/agents/bridge-daemon.md`
- Re-measure keystroke latency (echo during streaming output, not only idle) against the 5 ms output batch of `e644e74b`, and fall back to pin `f4146fd6` (no `9bf987ea`) if it regresses — breaks us — `docs/agents/performance.md`
- Bump the `tmux-upstream` pin to `b243b8ec` once the four items above are done — adopt — `docs/superpowers/specs/2026-11-23-tmux-upstream-review.md`
- Drop the `xterm-kitty*:progressbar` terminal-features line (`ad77e4a7`) once every kitty client answers XTVERSION and no resident pre-bump server (#407) reloads the config — drop workaround — `config/tmux.conf.tmpl`
- Evaluate OSC 7501 `pane_program_state` (`434ab3a2`) as an input to the agent-state machine — adopt — `docs/agents/agent-state.md`
- Use `disable-backoff` and `client_pending` (`9bf987ea`) in the keystroke-latency probe — adopt — `docs/agents/performance.md`
- Fix the stale `hold.go` kill-pane comment and the `tmuxPackage` default text (`0a3b2a1a`, `W02`) — drop workaround — `docs/agents/bridge-daemon.md`
- Correct or back the "Submitted upstream" claim in the popup control-client patch (no upstream PR found) — ignore — `docs/agents/bridge-daemon.md`

## Appendix: commits skipped (no tmux-og relevance)

127 of the 162 commits get no verdict row; the other 35 have one in Commits by area (35 + 127 = 162, no commit appears twice).

### Merge points whose relevance is recorded on the commit they merge (22)

- `399d53b1` Merge of openbsd: control-client tty colour init (e6211b41), choose-tree -i, focus-out on client lost, usage strings.
- `f8426e7d` Merge of openbsd: progressbar for Kitty, localtime cache, ( ) key notes, prompt cursor colour.
- `7d4fc58a` Merge of openbsd 0a3b2a1a (neighbour preference when tiling/floating).
- `d84b5829` Merge of openbsd: OSC 7501 program status (434ab3a2), pane-border dim, setaf cache, strict number parsing (refresh-client/monitor).
- `05109d97` Merge of openbsd 8e3bb660 ({current}/@ window target).
- `d32dd280` Merge of openbsd: layout spread refactor, scrollbar min size, hyperlink length, vi prompt cursor, display-panes -P doc.
- `31c28422` Merge of openbsd: separate-borders helpers and bold-bright palette fix.
- `75fac4a7` Merge of openbsd 4cce25ed (window-tree pane info).
- `161c4e3c` Merge of openbsd 13f9aa0d (pane-border-type separate borders).
- `71afaaff` Merge of openbsd f3e9a8a5 (pane-border-type follow-up; pin point that fixes find_up/down gap).
- `49ef4397` Merge of openbsd d13ee046 (stock pane menu loses Edit item).
- `440ed9b7` Merge of openbsd 541bc85e (resize-pane status accounting).
- `f4146fd6` Merge of openbsd: control-client suspend, control_discard_pane on ignored pane, nested format-loop UAF, send-keys -K, strftime, hyperlink, width ...
- `cce5f10a` Merge of openbsd: ASCII input fast path and join-pane -f ordering.
- `b272a8dc` Merge of openbsd: extended-cell runs, underscore compare, window_visible_ranges early row check.
- `83400d63` Merge of openbsd: written-line range tracking, invisible output discard.
- `71658679` Merge of openbsd: NULL-client checks in input start and capture-pane, emoji widths.
- `b999817c` Merge of openbsd: pane prompt position, month buffer, copy-crop fix, range walk.
- `4d609a0b` Merge of openbsd a796765e (sync update rewrite).
- `17c01675` Merge of openbsd: disable-backoff option, client_pending format, backoff grace, reflow-wrapped fix.
- `f8afa001` Merge of openbsd: input batching delay (e644e74b), clear function merge.
- `b243b8ec` Merge of openbsd: OSC 7501 forwarding (9e5702fb), hook payload guard, extended cell growth, capture-pane -P fix.

### No tmux-og consumer found (105)

- `ad1000c9` Regress tests for EL/ED clearing of tabs and wide chars (5819).
- `4a317618` screen_write: EL/ED no longer leave half a tab or wide character at the cut edge.
- `a73616d7` Merge of openbsd 4a317618 (EL/ED tab/wide-char clear fix).
- `58e1b557` compat freezero uses explicit_bzero.
- `bdc082d2` Fix usage strings of list-*/choose-tree/load-buffer commands and tmux.1 synopses.
- `054edaf3` key-string: bigger buffer so User1000 key name fits.
- `3a91622f` choose-tree/choose-client gain -i (start with info view instead of preview); tmux.1 fix.
- `6140e213` format_expand caches localtime_r result within the same second.
- `e2cb80a4` Prompt cursor colour set correctly (server-client/prompt).
- `e4abf957` Regress test layout-neighbour.
- `7073d607` OSS-Fuzz copy-mode fuzzer target.
- `eef26569` OSS-Fuzz imsg frame fuzzer target.
- `a7ae820b` Regress tests for OSC 7501.
- `c36f6b23` Regress tests for pane border dim and floating-pane border clients.
- `c05e7105` Merge PR 5874 (copy-mode fuzzer).
- `836405c1` tty-term caches setaf/setab capability expansions.
- `870cf5ff` input_print avoids redundant clearing; skip empty lists when flushing.
- `18153a5f` Regress test for targets (5841).
- `0f7ef1de` Regress test list-commands (5840).
- `fd30d37e` Regress test prompt cursor.
- `0889fc10` Regress tests for synchronized output, hyperlinks, image tabs.
- `2fc89e32` Regress tests for long hyperlink URIs (5836).
- `41cd12b0` Synchronized updates redraw only modified cells, not whole lines (hyperlinks/screen-write/tty-draw).
- `6fa1a889` Merge of openbsd 41cd12b0 (sync-update per-cell redraw).
- `83e0e020` screen-write: preserve cell attributes when clearing tabs.
- `8c2290c4` tmux.1 documents display-panes -P.
- `ec8b341a` Bump max hyperlink length (style.c/hyperlinks.c).
- `72d47cf6` Vi-mode command-prompt cursor constrained to content.
- `d56d7209` Regress palette test.
- `ed4611a4` tty: look up bright colours for bold before dim (5883).
- `4cce25ed` choose-tree pane info shows Program (OSC 7501), Command Line/Start/End and running flag.
- `5f02cf2c` Regress tests for copy-mode eol and prompt repeat counts.
- `caf8144215e1ef77d42931c5fb69114a36698135` copy-mode prompt motions take repeat counts (5890).
- `88e0a2ec` copy-mode remembered column fix with vi keys / after EOL (5887).
- `1d3c2e78` Merge of openbsd copy-mode fixes (88e0a2ec and caf8144215e1ef77d42931c5fb69114a36698135).
- `cdb9701c` Regress tests for pane-border-type, display-panes, options layout notify.
- `747d23f0` window-switch (modes): do not dim selected rows (5885).
- `ac7e727d` Merge of openbsd 747d23f0.
- `a1b5ffe6` Regress test floating-pane-resize.
- `4b6f35b6` Regress test macOS fix.
- `e1890173` wchar_t swapped to int (input-keys, key-string, utf8) for 16-bit wchar_t platforms (5803).
- `6d678cf3` Merge of openbsd e1890173.
- `3da62517` Portable parts of 5803 (compat utf8proc, utf8.c).
- `1247a421` Regress test fix floating-pane-resize.
- `7d7da0b3` Regress test copy-mode prompt scroll.
- `c87dd0f1` copy-mode: do not scroll view for prompt/mark jumps to a visible line (5891).
- `c8def167` Merge of openbsd c87dd0f1.
- `780c1970` Regress test fix tty-palette-bright.
- `f4174bcf` OSS-Fuzz modes tree fuzzer target.
- `3724f105` OSS-Fuzz session commands fuzzer target.
- `90a89024` Merge master into imsg fuzzer branch (brings the Nov 10-20 openbsd merges onto the fuzzer branch).
- `b2a01282` Merge PR 5875 (imsg frame fuzzer); aggregate carries all prior merged changes into the first-parent line.
- `faac4f18` Regress tests from Martin Trojer (control-client-suspend, send-keys -K, format nested loop, source-file stdin, destroy-unattached).
- `0d07ff24` Regress test join-pane -f.
- `38a4f370` tty-features: remove stray %; from builtin Setulc (5895).
- `43462428` format_find: handle strftime failure (empty result).
- `9e596989` server_check_unattached uses RB_FOREACH_SAFE (session destroyed while walking).
- `ef46f7e0` grid.c: clear cells directly instead of zeroing first.
- `16146d9e` tty-draw: collect runs of ASCII cells when drawing a grid line.
- `f6cefa4b` Merge of openbsd: grid clear and ASCII run drawing.
- `e9777d83` grid: compare underscore colour when deciding cells look the same.
- `206e9fdd` tty-draw/utf8: collect runs of extended cells.
- `f22bffe9` grid: do not encode a character into an extended cell if it is immediately replaced.
- `3032aa3e` Merge of openbsd f22bffe9.
- `f6d0c2a4` screen-write: discard invisible collected output immediately.
- `69ca9d50` Images bit and tests (tty-draw.c plus regress for sync/raw input).
- `fd88829c` input.c: char buffer instead of evbuffer for since_ground.
- `d75014eb` Merge of openbsd fd88829c.
- `dfa842aa` Regress test fix.
- `8b55a022` Fuzzer harness fix (imsg).
- `58a83027` Fuzzer harness fix (session cmds).
- `7bc2715e` Regress test pane prompt with status at top.
- `b776b65b` window_pane_start_input returns early for NULL client (e.g. from hooks/run-shell).
- `8d6a31de` capture-pane checks for NULL client.
- `19fe2d1a` Regress test fast copy crop on floating pane.
- `a93718c6` format_pretty_time month-name buffer enlarged.
- `df21e4de` Pane prompt cursor/mouse position use status-position option instead of status_at_line (fix with status off).
- `0c2b5796` screen-write: process collected items only over the modified line range.
- `d82f3007` screen-write copy: do not stop at first invisible cell, crop wide chars correctly (5915).
- `78a19c87` Regress tests for Unicode and direct CUP/RGB.
- `61296180` tty: expand CUP and RGB colour sequences directly (avoid tiparm_s).
- `bf2d0f78` log_debug becomes a macro skipped when logging is off.
- `fc4c3d90` Merge of openbsd: direct CUP/RGB expansion, log_debug macro.
- `338f1904` Regress test reflow at edge.
- `15f62e99` tty-draw: collect runs of blanks.
- `aa31ba37` utf8-combined: early reject of non-combining characters.
- `47a3bbd4` Merge of openbsd: blank-run collection, combining early reject.
- `a2118c14` CI regress workflow: no debug for test run.
- `d20e4c3d` vasprintf -> xvasprintf (environ.c, window-copy.c).
- `43d1445d` grid reflow keeps line wrapped if next character does not fit (5910).
- `539318c4` tty-draw/tty: merge near-identical clear functions.
- `754935c4` screen-write: no clipping clears on insert (clipped again at flush).
- `b67f272e` More regress tests (erase wrap, malformed, pending, sync state).
- `16033ec3` grid: reset wrapped flag when clearing beyond end of line.
- `094ab647` Merge of openbsd 16033ec3.
- `f79956ea` tty-term: small cache for RGB capability expansions.
- `866b2a71` input.c: parse numbers more efficiently.
- `913c1893` input.c: parse parameters in one pass.
- `ee446103` input.c: skip screen_write_collect_end until the ground state.
- `dbeb9ae0` Merge of openbsd: RGB cache, faster CSI parameter parsing.
- `65aa9d6e` Fix screen-write bits with images plus tests (portable image-grid.c/screen-write.c changes).
- `ecdb0e38` capture-pane -P with signed char.
- `6767a51d` grid: grow extended cell array geometrically (5853).
- `b83b591c` LLM-generated benchmark suite (bench/) incl. floating.sh/many-panes.sh throughput tests.
- `6a32033b` .gitignore ignores logs.
