# Crew dispatcher host badge (#1020)

1. `scripts/tmux-reflow-windows.sh`: add `#{s/[|]/ /:@crew_dispatcher_host}` after `@crew_name` in `FMT` and `chost` to the `read`; blank it in the bridge arm; suffix ` ⇄<host>` to `crew` before the width measure so `win_crew_dw`/`crew_colw` count it.
2. `scripts/tmux-update-icons.sh`: the `@crew_name` list-panes field becomes `<name>[ <host>]`, compared to `@crew_seen` — a host change re-fires the forced reflow.
3. Tests in `tests/reflow-fanout.bats` (AC1–AC3); doc in `docs/agents/agent-state.md`.
4. Gate: `nix build .#checks.x86_64-linux.reflow-fanout-tests`, `nix build .#lint`, `nix flake check --no-build`, shellcheck.
