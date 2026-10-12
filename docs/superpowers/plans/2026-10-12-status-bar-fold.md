# Status bar fold (`prefix + Z`)

Closes #970. Plan of record, as revised after plan-critic review.

## Files

- `scripts/tmux-status-fold.sh` (new): animator for the fold/unfold; token file (last press wins, re-checked before every write) + `acquire_lock` serialization; skips steps in a bridge mirror.
- `scripts/tmux-reflow-windows.sh`: reads `@og_status_folded` / `@og_status_cap`; stamps `@reflow_lines`; applies `status` 1 (`on`) or `min(lines, cap)`; sets the folded `status-format[0]`.
- `config/tmux.conf.tmpl`, `config/tmux.conf.reference.nix` (edited identically): `bind Z`; the update-icons `#()` moves to `@og_icons_job`, expanded in `status-format[0]` via `#{E:}`.
- `config/tmux.conf.nix`, `generator/paths/paths.go`: register the script (`scriptNames`, `scriptsWithLog`, `ogInternal`, `RequiredScripts`).
- `flake.nix`, `tests/status-fold.bats`: conf assertion + bats check on a scratch server.
- Docs: `docs/agents/status-bar.md`, `docs/agents/scripts.md`, `README.md`, `CLAUDE.md`.

## Decisions

- The icons job must stay alive while folded: it only runs inside a drawn `status-format[0]`, and the 5s sweep covers occupancy, not icons or the session tint.
- Reflow computes everything while folded so labels and `@reflow_key` stay fresh; toggles call it with `--force`.
- Bridge mirror: jump, never step (each step is a remote reshape).

## Acceptance

- AC1: `status-fold-conf-assertions`.
- AC2/AC3: `status-fold-tests` (fold, reflow while folded, unfold, rapid toggles, mirror jump).
- AC4: `nix build .#default`, `nix flake check`, `nix build .#lint`.
