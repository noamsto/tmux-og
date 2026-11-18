# tmux-crew-dash --side (#1185)

## Findings
- `scripts/tmux-crew-dash.sh` parses only `--detached` and `%N`; float path = list-panes scan for floating+label `crew-dash`, stash restore, `new-pane`.
- Bridge `tool` verb (`picker/remotebridge/daemon/ctl.go`) builds a float via `remoteToolFloat`; it cannot express a split or `--narrow`. Extending it needs new float-lookup/reuse logic and Go tests: not minimal.
- `tmux-carousel-guard.sh` keeps any tiled pane >= 20 cols; a restore could leave a 64-col side dash at 20-49. Revised: teach the guard `@pane_label crew-dash-side` (>= 50 cols in the snapshot) so a restore that crushes it under 50 rolls back; bats case + positive control in `tests/carousel-guard.bats` (done, check `carousel-guard-tests` green).
- `tmux-grid-refit.sh` re-tiles every tiled pane in `@crew_grid=1` windows; `tmux-dwindle` balances only when the pane count drops. Decision: `--side` in a `@crew_grid=1` window falls back to the float + notify (bats case); dwindle noted in docs.
- tmux-remux restores no pane options; panes with `@remux_relaunch` get that string exec'd.

## Steps
1. `scripts/tmux-crew-dash.sh`: parse `--side`. In `--side` mode, after the mirror branch:
   - Mirror window: call the bridge ctl exactly as today (remote float, same args), then, only when the ctl call succeeded, `notify` that the remote side panel is not supported and a float was opened. Decision recorded in docs + PR.
   - Local: `command -v crew` check first (notify). Then scan `list-panes -t $win` for a non-floating pane labelled `crew-dash-side` -> focus, exit. Else if `#{@crew_grid}` is 1 -> float fallback + notify. Else compute width `w = clamp(window_width*32/100, 50, 80)`; target width `tw = pane_width of target`; if `tw - w - 1 < 80` -> `notify "window too narrow for a side dash, opened the float"` and fall through to the float path. Else `split-window -h -l $w -t $pane -P -F '#{pane_id}' [-d] -c cwd 'exec crew dash [--narrow]'`; stamp `@pane_label crew-dash-side`, `@remux_relaunch` (re-stamps the label, then execs the dash). `--narrow` only when `crew dash --help </dev/null 2>&1 | grep -q -- --narrow` (once per call, only after the idempotency/narrow/grid checks). `@remux_relaunch` is exactly `tmux set -p @pane_label crew-dash-side; exec crew dash [--narrow]` (fish-compatible), asserted in bats.
   - Float path untouched; grid window falls back to float; remux relaunch string stamped; float and side coexist (float scan requires floating flag; side scan requires non-floating).
2. `tests/crew-dash-launch.bats` (additions only; the setup stub crew answers `dash --help` with exit 0 and prints help, listing `--narrow` unless `STUB_NO_NARROW` is set, else `exec sleep 3600`, so no existing test changes): side creates one tiled pane right of target, label, width in [50,80], command `exec crew dash --narrow` with a stub crew whose `--help` lists it, plain `exec crew dash` otherwise; second call focuses and creates nothing; `--detached` keeps focus on the target; narrow window falls back to float + display-message; mirror window `--side` goes through the ctl stub (log asserted) and notifies; grid window falls back to float; remux relaunch string stamped; float and side coexist; a float press does not touch the side pane.
3. Docs: `docs/agents/scripts.md` row, `docs/agents/floats.md` (side mode, guard rationale, remux relaunch), `docs/agents/bridge-daemon.md` (mirror --side falls back to remote float), script header comment.
4. Gates (also `carousel-guard-tests`): `nix build .#lint --accept-flake-config`; `nix flake check --no-build`; `nix build .#checks.x86_64-linux.crew-dash-launch-tests` (flake.nix copies only the touched bats + scripts already); carousel-guard check unaffected (no file change).

## Out of scope
Daemon/Go changes (fallback taken), `crew dash --narrow` itself.
