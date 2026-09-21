# Popups → floating panes (#725) — implementation plan

Spec: `docs/superpowers/specs/2026-09-21-popups-to-floats-design.md`.

Two expressions recur below. They are named here once and used verbatim:

- **EFF_ACTIVE** — `#{?window_modal_pane,#{pane_last},#{pane_active}}`
  ("this pane is the window's active pane, looking past a modal float").
- **NOT_MODAL** — `#{!:#{pane_modal_flag}}` (a `list-panes -f` filter).
- **UNDER(X)** — `#{?window_modal_pane,#{P:#{?pane_last,X,}},X}` (field X of
  the pane under a modal float, else of the active pane).

Every step names its files. Only step 1 is applied in the worktree. Steps 2–4
were verified on a scratch server (config loads clean without the option,
nested attach works) but are **not yet applied**.

## 1. Pin + patch (done)

- `flake.nix`: `tmux-upstream.url` → `github:tmux/tmux/3a6c2e7877e8c017edb84c8d3ee41b98abee27d3`;
  delete the `patches = … tmux-client-control-discard.patch` line and its comment.
- `git rm patches/tmux-client-control-discard.patch`; `flake.lock` relocked
  (`nix flake lock --update-input tmux-upstream`).
- Keep `version = "next-3.9"` (verified: `tmux -V` → `tmux next-3.9`).

## 2. Removed option (to do)

- `config/tmux.conf.tmpl:6` and `config/tmux.conf.reference.nix:298`: delete
  `set -g popup-border-lines rounded`. The two files must stay in lockstep (the
  extraction check diffs generated vs reference).

## 3. Catppuccin (to do)

- `config/tmux.conf.nix` (`catppuccin = pkgs.tmuxPlugins.mkTmuxPlugin …`, ~l.900)
  and `config/tmux.conf.reference.nix` (~l.94): add the identical
  ```nix
  # popup-* options are gone upstream (tmux 34cd5da4); -q keeps them on a
  # resident older server and silent on the pinned one.
  postPatch = ''
    substituteInPlace catppuccin_tmux.conf \
      --replace-fail 'set -gF popup-style' 'set -gqF popup-style' \
      --replace-fail 'set -gF popup-border-style' 'set -gqF popup-border-style'
  '';
  ```
  Identical text in both → one store path, as their comment requires.

## 4. Scratchpad nested attach (to do)

- `scripts/tmux-scratchpad.sh` inner mode: replace
  `exec tmux new-session -A -s "$SCRATCH"` with
  `exec env -u TMUX tmux -S "${TMUX%%,*}" new-session -A -s "$SCRATCH"`, and
  rewrite the comment above it. The old comment is about popup PTYs. The new one
  says the launch is now a real pane, so tmux's nested-client check refuses the
  attach unless `TMUX` is unset, and the socket comes from `$TMUX`'s first field.
  The `tmux set -t "$SCRATCH"` lines above it still talk to the same server
  through `$TMUX`; leave them.

## 4b. Pin the launch window (added at execute time)

- Live test 4 found the compat float opening in the `-t`-default "best"
  session, not the client's window. `scripts/tmux-{session-picker,window-picker,window-wall,which-key,scratchpad}.sh`:
  `POPUP_CLIENT=(-c "$CLIENT" -t "$CLIENT:")`, with the comment updated;
  `tests/picker-launcher.bats` argv expectations updated to match.

## 5. `tmux-update-icons`: skip modal rows, EFF_ACTIVE

- `scripts/tmux-update-icons.sh:401` (`done < <(tmux list-panes -a -F '…')`):
  add `-f '#{!:#{pane_modal_flag}}'` and replace the 11th field
  `#{pane_active}` with EFF_ACTIVE. Field count unchanged (the `read -r` list at
  l.333 is untouched). Add a two-line comment above the loop's input stating the
  chrome rule (a modal float is the window's active pane while open; it is not
  window content). Point to `docs/agents/floats.md` instead of restating the
  details.

- New `tests/update-icons-modal.bats`, set up like
  `tests/update-icons-all-windows.bats` (sed-substituted script, private
  config-less server, bash copied to `claude`), but wired in `flake.nix` with
  `(mkTmux pkgs)` instead of `pkgs.tmux`: nixpkgs' tmux has no modal panes. The
  test drives a real server, so the `-f` filter runs in tmux itself; no stub
  has to imitate it. Setup: a window whose pane runs `claude` and has
  `$CLAUDE_TASKS_DIR/<id>` and `$CLAUDE_NAMES_DIR/<id>` files, plus a second
  tiled pane running a shell. Mark the window `@window_has_agent 1` so
  `naming_read` is set. With the claude pane active, open
  `new-pane -O -K -t <claude pane> -x 50% -y 50% '<bash copied to "picker-x">'`
  (a distinct `pane_current_command`). Then one `bash "$UPDATE_ICONS" <sess>` pass.
  First assert the float really is modal (`pane_modal_flag=1`,
  `window_modal_pane` = its id), or the test passes vacuously.
  Assert: `@window_task`/`@window_ai_name` equal the claude pane's files;
  `@window_icon_display` has no `picker-x` entry (compare against a pass with
  no float open); session `@active_pane_icon` is the claude icon.
- The existing `update-icons-*` suites keep running on `pkgs.tmux`, which lacks
  `pane_modal_flag`/`window_modal_pane`. That makes them the degradation
  proof: on a server without modal panes, the filter expands to `1` and
  EFF_ACTIVE to `pane_active`, so they must still pass unchanged.

## 6. `claude-status-update` `window_stamp`

- `scripts/claude-status-update.sh:90`: `'#{?pane_active,1,}|…'` →
  `'#{?#{?window_modal_pane,#{pane_last},#{pane_active}},1,}|…'`. Extend the
  "Active pane only" comment by one clause: "looking past a modal float".

## 7. `tmux-worktree-match`

- `scripts/tmux-worktree-match.sh:37`: add `-f '#{!:#{pane_modal_flag}}'` and
  replace `#{pane_active}` with EFF_ACTIVE. The awk consumer is unchanged.

## 8. `tmux-statusline` volatile fields

- `picker/statusline/main.go` `volatileFields`: replace `"#{pane_current_path}"`,
  `"#{pane_current_command}"` and `"#{@bridge_proc}"` with UNDER(X) for each
  (build them with a small helper `underModal(x string) string` so the three
  entries stay readable). Order and count unchanged. Update the comment on
  `fetchVolatile` ("to the session's active pane") to name the modal case.
- Test in `picker/statusline/main_test.go`: assert `underModal("#{pane_id}")`
  equals the literal UNDER expression, and that `volatileFields` still has 22
  entries with the three swapped fields at indices 6, 9, 20.

## 9. Picker collectors skip modal rows

- `picker/main.go:163` (`collectPanesSnapshot`) and `picker/main.go:435`
  (`collectWindows`): add `"-f", "#{!:#{pane_modal_flag}}"` to the
  `list-panes -a` argv, through one package const
  `notModalFilter = "#{!:#{pane_modal_flag}}"`. No field-count change. Extend
  each doc comment by one sentence explaining why. Hoist each collector's argv
  into a package-level func (`panesSnapshotArgv()`, `windowsArgv()`) that the
  `exec.Command` call spreads, so `picker/main_test.go` can assert that both
  carry `-f notModalFilter`. Live test 5 (§13) pins what the filter does on the
  pinned tmux.

## 10. Picker never captures itself

- `picker/capture.go`: add
  ```go
  // selfTargets maps the picker's own session ("sess") and window ("sess:idx")
  // targets to the pane under its float …
  func selfTargets(pane string, run captureRunner) map[string]string
  ```
  One call: `display-message -p -t <pane> '#{window_index}|#{P:#{?pane_last,#{pane_id},}}|#{session_name}'`
  (session_name last, since it may contain `|`). Returns nil when `pane` is
  empty, the call fails, or the loop result is not a `%N` id. The last case
  covers a pre-float server or a picker not running in a float. Cached behind
  a `sync.Once` wrapper, `selfCaptureTarget(t string) string`, which reads
  `os.Getenv("TMUX_PANE")` and returns the mapped id or `t` unchanged.
- `picker/tui.go:1736` (preview): capture `selfCaptureTarget(t)` instead of
  `t`; `previewMsg.target` stays `t`.
- `picker/tui.go` `captureWallCmd` (~l.1594): map each target through
  `selfCaptureTarget` before `captureTargets`. Then re-key the result map and
  `captureErr.Target` back to the item targets, so `wallContent`/`wallBad`
  stay keyed by item target.
- Tests in `picker/capture_test.go` with the existing fake runner: `selfTargets`
  parses `2|%9|my|sess` → `{"my|sess": "%9", "my|sess:2": "%9"}`; empty pane →
  nil; a non-`%` loop result → nil; runner error → nil. Plus a test for the
  wall re-keying helper (factor it as `captureViaSelf(targets, resolve, run)` so
  it is testable without the `sync.Once`).

## 11. Comments that describe popup mechanics

- `picker/whichkey.go:213-224`: the replay-delay rationale. Rewrite the
  mechanism sentence: `display-popup` now opens a modal float, a window holds
  one modal pane, and the compat command silently no-ops while one is open
  (re-measured 5/5 on `3a6c2e78`). The delay itself is unchanged.
- `scripts/tmux-scratchpad.sh` header ("in a popup") and
  `scripts/og-notify-center.sh:3`: wording only where it is now wrong.

## 12. Docs

- `docs/agents/floats.md`: new bullet **Popups are modal floats**: the compat
  `display-popup` (what it sets per pane: modal, all-keys, `remain-on-exit`,
  border/title). Then the chrome rule with EFF_ACTIVE / NOT_MODAL / UNDER, the
  consumers it is applied in (5–10), the accepted leftovers (the one-pane
  border segment, the status command segment being the float while open,
  killing the picker's own window/session closes the picker), and that
  `tmux-float-refit` skips popup-floats (no `@float_geom`).
- `docs/agents/picker.md`: a **Launch** bullet: popup-floats, the
  never-capture-self rule, and the kill-own-window acceptance.
- `docs/agents/splash.md`: "via `display-popup`" → the compat modal float.
- `docs/agents/bridge-daemon.md`: extend the "A remote float is mirrored as a
  local float" bullet. Remote popups are now floats and mirror like any float;
  the bridge cannot originate one (control clients are refused). A local
  popup-float in a mirror window is a user float to the daemon.
- `docs/agents/scripts.md`: the `tmux-update-icons`, `tmux-worktree-match`,
  `tmux-scratchpad` and `claude-status-update` rows: one clause each for the
  modal rule / nested-attach fix. `rg` the script name to find the row.

## 13. Live regression test

- New `tests/popup-float.bats`, the attached-client harness copied from
  `tests/float-tool-focus.bats` (outer server pane attaches to the inner
  server; isolated `HOME`, `CLAUDE_STATUS_DIR`, etc.; `@splash_shown 1` set
  before attach). Tests:
  1. **config sources clean**: the inner server is already running the wrapped
     config; `inner source-file "$CONF"` exits 0 with empty stderr. `$CONF`
     comes from the wrapper (`grep -o -- "-f [^ ']*"` on `$TMUX_BIN`, as the
     probe did). This fails on today's `main` against the new pin.
  2. **`prefix + n` opens a modal float in the client's window**: create a
     second window and select it, press `prefix n`, then assert one pane with
     `pane_floating_flag=1 pane_modal_flag=1 remain-on-exit=off` in *that*
     window. Escape closes it (og-notify-center exits on Escape): no float
     remains, and the previously active pane is active again.
  3. **a second popup while one is open adds no pane**: with the float from 2
     open, `inner display-popup -c <client> -E 'sleep 30'` returns 0, and the
     window still holds exactly one float.
  4. **`prefix + S` reaches the scratch session**: after the press, a second
     client appears with `client_session=scratch-s` within 5s. Detach it with
     `prefix d` (routed to the nested client by the float's all-keys capture)
     and assert the float is gone.
  5. **chrome rule at the tmux layer**: with a popup open (`display-popup -c
     <client> -E 'sleep 30'`), assert `list-panes -f NOT_MODAL` omits the float
     and EFF_ACTIVE marks the pane that was active before. This pins the
     expressions the scripts and Go code rely on against the pinned binary.
- `flake.nix`: `checks.popup-float-tests`, a copy of `float-tool-focus-tests`
  (same `import ./config/tmux.conf.nix` shape, `enrichEnable = false`,
  `agentUsageEnable = false`; `notifyEnable` and `splashEnable` keep their
  `true` defaults (`config/tmux.conf.nix:34,67`), since `prefix + n` needs notify on).

## 13b. Commit the design docs

- `git add docs/superpowers/specs/2026-09-21-popups-to-floats-design.md
  docs/superpowers/plans/2026-09-21-popups-to-floats.md` in the same PR. The
  crew artifact copies are refreshed from these after each revision.

## 14. Gate

Fast gate: `go test ./...` in `picker/` (statusline, capture), `shellcheck` +
`shfmt -d` on the four scripts, `bats tests/popup-float.bats` against the built
wrapper, then the full local gate in order: `nix build .#default`,
`nix flake check`, `nix build .#lint`. Also re-run the existing suites that pin
launcher argv (`picker-launcher.bats`, `splash.bats`) and
`tmux-next38-readiness.bats`. The launcher argv is unchanged in this design, so
those should pass untouched. The same holds for `notify-conf-assertions`: the
`prefix + n` bind is still `display-popup -E …og-notify-center`.
Steps 6 and 7 get no dedicated test. They use EFF_ACTIVE / NOT_MODAL verbatim,
and live test 5 and `update-icons-modal.bats` pin both expressions on the pinned
binary.

## Ordering

1 done. 2–4 first: 13 cannot pass without them. 5, 6, 7 are independent shell
edits. 8, 9, 10 are independent Go edits (10 is the largest). 11 depends on
nothing. 12 comes after 5–11 so the docs describe what landed. 13 can be
written in parallel with 5–11. 14 runs last.
