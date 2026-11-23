# Pane parent/child lifetime (#1222)

Goal: a pane marked `@pane_parent <pane_id>` closes when that parent pane exits/dies or when `tmux-pane-chain close-children <parent>` is called. Recursive, cycle-safe, `@bridge_pane` never closed.

## Design (chosen over alternatives)

- Option on the **child** (`@pane_parent`), not a list on the parent: no two-writer race, and a dead child leaves nothing to clean. Children are found by one `tmux list-panes -a` scan, which covers hidden floats in `scratch-og-float-stash` and other windows/sessions. Pane ids are per-server and the scan only queries the invoking server, so a foreign server's ids can never match.
- Alternative rejected: a `pane-exited` hook per parent. Needs per-parent hook lifecycle and does not cover a parent that never exits (dispatcher shell).
- remux: only `@remux_relaunch` is replayed on restore; other pane options (`@pane_parent`, `@pane_label`) are not persisted, and pane ids change. Decision: the chain is **dropped on restore** (document in `persist.md`); no remap.
- `tmux-crew-dash` stamps with a literal `set-option -p @pane_parent` in the existing option chains (no new substitution into that file); only on the two **create** paths, only with `--detached`. Restoring/focusing an existing dash keeps its stamp. A mirror-window dash runs on the remote host and is not stamped.

## Files

- `scripts/tmux-pane-chain.sh` (new): `adopt <child> <parent>`, `close-children <parent>`.
- `scripts/tmux-reap-pane.sh`: call `close-children` for the hook pane before the lib-claude reap.
- `scripts/tmux-crew-dash.sh`: stamp lines only.
- `config/tmux.conf.nix`: add `tmux-pane-chain` to `scriptNames`, `ogInternal` (plain `mkScript`); add `@pane_chain@` -> `${script.tmux-pane-chain}/bin/tmux-pane-chain` to `mkScriptWithLibs`'s `replaceStrings` (harmless to its other users, no cycle).
- `modules/home-manager.nix`: add `tmuxConfig.script.tmux-pane-chain` to `home.packages` beside `tmux-crew-dash`.
- `flake.nix`: new `pane-chain-tests` check (copies `tmux-pane-chain.sh`, `tmux-reap-pane.sh`, `tmux-float-group.sh` via `scriptsSrc`, `testsSrc ["pane-chain.bats"]`, `(mkTmux pkgs)`; the stash case uses the raw float-group script through a bin wrapper like `crew-dash-launch.bats`); `float-conf-assertions` gains `REAP`/`PANE_CHAIN` asserts (`! grep -qF '@pane_chain@' "$REAP"`, `grep -qF "$PANE_CHAIN" "$REAP"`).
- `tests/reap-pane-hook.bats`: add a production-hook case (wrapped server: stamp child, end parent's process, child gone).
- `tests/pane-chain.bats` (new), `tests/crew-dash-launch.bats` (extend).
- `docs/agents/scripts.md`, `floats.md`, `persist.md`.

## Steps

- [ ] **Step 1: failing tests** — `tests/pane-chain.bats` on a bare scratch tmux (harness copied from `float-group.bats`, `mkTmux` pinned build): (a) `close-children` kills a float child, a side (tiled) child, a stashed child (hidden via `tmux-float-group hide`), a grandchild; parent survives; (b) cycle a→b→a terminates, parent `a` survives; (c) child with `@bridge_pane` survives; (d) hook path: a `pane-exited` hook running `OG_PANE_CHAIN_BIN=<path> tmux-reap-pane.sh #{q:hook_pane}` inline (run-shell gets the server env, not the test's), end the parent's process (named "process exit", not kill-pane), poll up to ~2s for children to vanish (`run-shell -b` is async); (e) `adopt` refuses self/non-ids. Extend `crew-dash-launch.bats`: `--detached` float and `--side --detached` stamp `@pane_parent`; plain open does not. Command: `bats tests/pane-chain.bats tests/crew-dash-launch.bats` with `TMUX` from the pinned build — expect red (script missing / no stamp).
- [ ] **Step 2: `tmux-pane-chain.sh`** — one `list-panes -a -F '#{pane_id}|#{@pane_parent}|#{@bridge_pane}'` read (`|`-delimited per CLAUDE.md), compute descendants of the parent iteratively with a visited set seeded with the parent (cycle-safe, parent never killed), skip and do not traverse `@bridge_pane` panes, `kill-pane` deepest-first. Validates `%N` ids, fails closed. Command: `shellcheck scripts/tmux-pane-chain.sh`.
- [ ] **Step 3: reap hook** — `tmux-reap-pane.sh`: `PANE_CHAIN="${OG_PANE_CHAIN_BIN:-@pane_chain@}"; [[ -x $PANE_CHAIN ]] && "$PANE_CHAIN" close-children "${1:-}" || true` before the `@lib_claude@` guard so the raw script works under bats.
- [ ] **Step 4: crew-dash stamp** — two separate lines (a bash conditional cannot sit inside a `\;` chain; keeps the concurrent rebase trivial). Side path in `open_side()`, after the `@pane_label`/`@remux_relaunch` chain, before `return 0`: `((detached)) && tmux set-option -p -t "$sid" @pane_parent "$pane"` (the new pane is `$sid`; `$id` is unbound there under `set -u`). Float path, after the `@float_geom` chain, before `exit 0`: `((detached)) && tmux set-option -p -t "$id" @pane_parent "$pane"`.
- [ ] **Step 5: Nix wiring + placeholder assertions** — as in file list; `nix build .#default`; add the production-hook case to `reap-pane-hook.bats`.
- [ ] **Step 6: docs** — scripts.md new row + reap-pane/crew-dash row additions, floats.md (stamp rules, stash children; structural `kill-pane`/`kill-window` fire no pane hook, so only process exit/death and an explicit `close-children` close children; a restored dash is window-owned since `@remux_relaunch` does not re-stamp), persist.md (dropped on restore).
- [ ] **Step 7: gates** — `nix build .#default`, `nix flake check`, `nix build .#lint`.

## Acceptance

| item | evidence |
| --- | --- |
| test scenarios | `pane-chain-tests`: float/side/stashed/grandchild/cycle/bridge/shell; `crew-dash-launch-tests`: detached stamps, plain not stamped; `reap-pane-hook-tests`: production hook |
| placeholder substituted | `float-conf-assertions` greps the built `tmux-reap-pane` |
| docs updated | diff of scripts.md, floats.md, persist.md |
| `nix build .#default`, `nix flake check`, `nix build .#lint` | command exit 0 |
