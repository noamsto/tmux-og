# Auto-fold the status bar under a covering float (#1191)

## Mechanism

A new script `tmux-status-autofold <session> <window-id> <client-width>` decides the fold; hooks fork it only when a float-geometry signature changed.

- **State (session options):** `@og_status_folded` (existing; what reflow renders), `@og_status_autofolded` (new: `1` only while the auto-fold is what folded the bar), `@og_autofold_sig` (the signature the hook last forked on).
- **Script:** lists the window's panes (`pane_floating_flag|pane_modal_flag|left|top|width|height`, plus window size), skips modal panes (transient chrome, floats.md chrome rule) and `@bridge_pane` floats, and calls `float_covers_view` from `lib-float-cover.sh` (the one shared definition; no format-language copy).
  - covers and `@og_status_folded` != 1: set folded=1, autofolded=1, `tmux-reflow-windows --force` (instant, no stepping).
  - not covers and autofolded=1: unset autofolded, set folded=0, reflow `--force`.
  - otherwise nothing (a manual fold is never undone: autofolded is only ever set by the auto path).
- **Manual `prefix + Z`** also runs `set -u @og_status_autofolded`, so a manual fold or unfold owns the state and is never undone. A manual unfold while a float is open is respected until the next signature change (next float event).
- **Hooks** (`window-layout-changed[40]`, `session-window-changed[40]`, `pane-exited`-class events all raise layout-changed): an `if -F` gate forks only when `#{window_active}` and the signature `window_id:WxH:<floating pane geometries>` differs from `@og_autofold_sig` and (the window has a float or `@og_status_autofolded` is 1). It writes the new signature with `set -F` before the `run-shell -b`, so a burst coalesces. A hook event in a float-free window with nothing auto-folded forks nothing. The fold changes the window height, so `window-resized` runs the existing `tmux-float-refit` gate (percentage geometry re-fits into the reclaimed rows); one extra no-op fork may follow because the signature includes the new size.

## Steps

1. `scripts/tmux-status-autofold.sh` (sources `@lib_float_cover@`, `@reflow@` via `OG_AUTOFOLD_REFLOW` override like the fold script). shellcheck clean.
2. Wire: `config/tmux.conf.nix` (`scriptNames`, a `mkScriptFloatCover`-style builder that also substitutes `@reflow@`), `generator/paths/paths.go` (Scripts list), `config/tmux.conf.tmpl` (Z bind adds the `set -u`; two hooks), `flake.nix` (script source list, bats check `status-autofold-tests`, extend `status-fold-conf-assertions`).
3. `tests/status-autofold.bats` on a scratch server (real floats): covering float folds; closing / hiding (stash move) / switching to a float-free window unfolds; small float does not fold; manual fold survives float close; manual unfold respected until next float event; modal float ignored. Plus hook-gate tests: the gate string forks nothing for a no-state-change event; float-refit re-fits after fold (live check).
4. Docs: `status-bar.md`, `floats.md`, `scripts.md` row, `performance.md` fork counts.
5. Measure forks per event before/after on a scratch server (execve count behind a `tmux` shim), put in the PR body.

## Revision 1 (plan-critic findings)

- **Reference config:** `config/tmux.conf.reference.nix` (frozen copy diffed by `entryCheck`) gets the same Z-bind change and both hooks, with `${script.tmux-status-autofold}/bin/tmux-status-autofold` paths. Verified live: `window-layout-changed` fires in the source window on float create and on a hide (join-pane to the stash window), so no other trigger is needed.
- **Build wiring:** new `mkScriptAutofold` in `config/tmux.conf.nix` substituting `@lib_float_cover@` and `@reflow@`; name added to `scriptNames` (beside `tmux-status-fold`, ~305), the second list (~855), the dispatch (~680) and `generator/paths/paths.go`.
- **Gate (exact).** SIG = `#{window_id}:#{window_width}x#{window_height}:#{P:#{?#{&&:#{pane_floating_flag},#{!:#{pane_modal_flag}}},#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} ,}}` (non-modal floats only; stashed floats live in another window so never appear).
  ```
  set-hook -g window-layout-changed[40]  { if -F '#{&&:#{&&:#{window_active},#{!:#{@bridge_win}}},#{&&:#{!=:SIG,#{@og_autofold_sig}},#{||:#{!=:#{P:#{?#{&&:#{pane_floating_flag},#{!:#{pane_modal_flag}}},x,}},},#{==:#{@og_status_autofolded},1}}}}' { set -F @og_autofold_sig 'SIG' ; run-shell -b "<script> #{q:session_id} #{q:window_id} #{q:client_width}" } }
  set-hook -g session-window-changed[40] <same body>
  ```
  `@og_autofold_sig` and `@og_status_autofolded` are session options set via `set -F` in hook context (target = hook session) and, in the script, `-t '$N'` session id.
  The conf assertion greps the hook line count (2 per config) and the `tmux-status-autofold #{q:session_id}` shape; `tests/status-autofold.bats` installs the real gate lines extracted from `tmux.conf.tmpl` on a scratch server and asserts fork counts via a `run-shell` marker file.
- **Fork measurement:** a `tmux-status-autofold` stub that appends a line per invocation, driven by the extracted gate on a scratch server: events = float-free window layout change, tile split, window switch float-free↔float-free, covering float open/close/hide, small float open. Before = 0 forks (no hook); after = counts from the stub; reflow forks counted by a reflow stub.
