# Plan: repo tag on worker windows from another repo (#1165)

Mechanism: mirror tabs get `#{?#{@bridge_placed},<peach>@icon_remote @bridge_host ,}` right after `window_index: ` in status-format[1] (tmpl, mirrored byte-for-byte in config/tmux.conf.reference.nix). Add a sibling segment for `@crew_repo`.

Steps
1. generator/render/status.go: `crewRepoTag` const (single place for the format), exposed as `Data.CrewRepoTag`. Gate: `@crew_repo` set, `!@bridge_placed`, `@crew_repo != #{b:session_path}`; body: peach `@crew_repo ` then the active/inactive style restore the mirror badge uses.
2. config/tmux.conf.tmpl: splice `{{.CrewRepoTag}}` after the `@bridge_placed` segment; reference.nix gets the same literal.
3. generator/default.nix: `nativeCheckInputs = [pkgs.tmux]` so the gates run the tmux-driven test.
4. status_test.go: `TestCrewRepoTagEvaluates` (scratch tmux, own -L socket and TMUX_TMPDIR: unset, equal, different, placed mirror) and `TestCrewRepoTagInStatusFormat`.
5. docs/agents/status-bar.md note.
Out of scope (follow-up): multi-line reflow variants (tmux-reflow-windows.sh win_host column) and compact/active-only mode.
Gates: nix build .#default, nix flake check, nix build .#lint.
