# CLAUDE.md

tmux-og is an opinionated tmux configuration delivered as a Nix flake: it wraps the tmux binary with a baked-in config. Catppuccin theme, multi-line status bar with auto-reflow, per-window Nerd Font process icons, live Claude Code status, Go bubbletea pickers, and a control-mode bridge that mirrors remote tmux sessions as local windows.

## Deep-dive docs — read before touching the area

This file holds only what every task needs. The measured evidence, invariants and issue history for each subsystem live in `docs/agents/`. **Read the matching doc before editing in its area** — most lines in them record a bug that already happened once.

| Working on… | Read |
|---|---|
| Any `scripts/*.sh` — what invokes it, which options/files it owns, its ownership guards | `docs/agents/scripts.md` (one long table row per script — `rg` the script name rather than reading the file whole) |
| `/tmp/claude-status/*` files, `read_pane_state`, agent-detect, interrupt/dead-agent detection, `@window_has_agent` naming reset, self-report files | `docs/agents/agent-state.md` |
| Status bar lines, `tmux-reflow-windows`, grid column widths, icon variables | `docs/agents/status-bar.md` |
| `picker/` TUI layout, header/pinned line, key input, which-key ranking | `docs/agents/picker.md` |
| `@issue_*`/`@pr_*`, `tmux-pr-enrich`, agent usage segment, `@window_cwd_seen` | `docs/agents/enrichment.md` |
| Anything a mirror window *displays* about the remote: `@bridge_*` labels, remote agent status, `@bridge_res` CPU/Mem, the enrich card in a mirror | `docs/agents/bridge-shipped-state.md` |
| `picker/remotebridge/daemon`: reconnect, session pinning, renderers, reseed, zoom, float mirroring, reply ordinals/barriers, `get-clipboard`; what a remote host needs on PATH | `docs/agents/bridge-daemon.md` |
| Kitty/sixel graphics across the bridge, `prefix + I` carousel, `ctrl+v` image paste | `docs/agents/bridge-graphics-paste.md` |
| Float binds, `@float_geom`, `@og_float_target_<tool>`, popup-floats / the modal-pane chrome rule (any new `list-panes` or `pane_active` consumer) | `docs/agents/floats.md` |
| Light/dark, `@catppuccin_flavor`, `theme-state.json`, client theme hooks | `docs/agents/theme.md` |
| tmux-remux persistence, `@remux_relaunch`, pi resume | `docs/agents/persist.md` |
| `tmux-splash` | `docs/agents/splash.md` |
| Why a tmux rule below exists (format delimiters, session targeting, scratch servers, `-B` monitor hooks) | `docs/agents/tmux-gotchas.md` |

When a change alters behaviour one of these docs describes, update that doc in the same PR. New subsystem detail goes there, never here.

## Build and Test

The local gate is three commands — none subsumes another:

```bash
nix build .#default   # the wrapped tmux (./result/bin/tmux)
nix flake check       # bats (tests/*.bats) + Go tests + conf assertions
nix build .#lint      # pre-commit hooks: alejandra, statix, deadnix, shellcheck, shfmt, typos, ...
```

`nix flake check` runs no formatter — a formatting failure reaches CI only by skipping `nix build .#lint` (#327). CI mirrors the split: a `lint` job, and a `build` job on `x86_64-linux` + `aarch64-darwin`.

Reload a running tmux with `prefix + r`. `./tests/test-display.sh` is a manual display test, outside `nix flake check`.

**Commit from inside `nix develop`/direnv.** The devShell `shellHook` generates `.pre-commit-config.yaml`; a fresh worktree without it fails with `No .pre-commit-config.yaml file was found`. `PRE_COMMIT_ALLOW_NO_CONFIG=1` skips `shfmt`/`shellcheck` entirely — load the devshell instead.

## Architecture

### Nix build pipeline

`flake.nix` imports `config/tmux.conf.nix`, the core of the build:

1. **Libraries**: `scripts/lib-*.sh` are built with `writeShellScript` and sourced at runtime via `source @lib_icons@`-style placeholders replaced with store paths.
2. **Scripts**: each `scripts/*.sh` becomes a store binary via `writeShellScriptBin`. `scriptsWithIcons` also get `@lib_icons@`, `@lib_claude@`, `@ICON_MAP@`, `@FALLBACK_ICON@`, `@MAX_ICONS@`, `@MAX_ICONS_PICKER@`, `@claude_status_bin@` substituted. Keep `@NAME@` patterns out of any other context.
3. **Plugins**: Catppuccin and which-key pinned with `mkTmuxPlugin`; the rest from nixpkgs `tmuxPlugins`.
4. **Config**: the full tmux.conf is a generated store file with interpolated store paths (`generator/` renders parts of it in Go).
5. **Wrapper**: `symlinkJoin` + `wrapProgram` produce a `tmux` that loads the config via `-f` with all scripts on PATH.

`modules/home-manager.nix` provides `programs.tmux-og` (`enable`, `worktrunk`, `skills`, `startupSession`, `persist`, `enrich`, `agentUsage`, `splash`, `remote`, …); its activation script reloads the config and reflows every session.

### Shared libraries

- **`lib-icons.sh`** — `ICON_MAP`, `build_proc_icons`, `measure_display_width`, `strip_tmux_colors`, `pad_to_width`. Icon mapping data lives in `config/process-icons.nix`.
- **`lib-claude.sh`** — `CLAUDE_PANES_DIR`, `read_pane_state` (staleness, interrupt, dead-agent), `claude_state_icon`, `setup_claude_colors`, `claude_priority_state`, the reap/prune/clear helpers.
- **`lib-enrich.sh`** — branch→issue parsing, `sanitize_title`, `collapse_check_rollup`, `pr_pie_glyph`, `split_pr_badge`. Pure logic, unit-tested in `tests/enrich.bats`.
- **`lib-reflow.sh`** — `reflow_fit_columns`, `reflow_clip_rests`.

Hot-path functions set `REPLY` instead of echoing, to avoid a subshell fork.

### Who runs what

Three drivers, and picking the wrong one is a recurring bug:

- **`#()` in `status-format[0]`, every 1s** — `tmux-update-icons`, `claude-status`, `tmux-branch-display`, `tmux-dir-display`. Runs only for a client that draws a status line.
- **`-B` monitor hooks, every 5s, client-independent** — `@og-sweep-tick` (`tmux-update-icons` sweep), `@og-pr-tick` (`tmux-pr-enrich`), `@og-backfill-tick` (`tmux-issue-stamp --backfill`), `@og-usage-tick` (`tmux-agent-usage`), `@og-res-tick` (`tmux-session-resources`).
- **tmux hooks / keybinds / external callers** — `tmux-reflow-windows` (window add/remove/resize), `tmux-reap-pane` (`pane-exited`/`pane-died`), `tmux-shell-prompt` (`pane-shell-prompt`, OSC 133), `tmux-float-refit` (`window-resized`), `tmux-grid-refit` (`window-resized[10]`), `tmux-client-theme` (`client-*-theme`), `tmux-splash-maybe` (`client-attached`), `claude-status-update` (Claude Code plugin hooks), `tmux-worktree-match` + `tmux-issue-stamp` (worktrunk `post-switch`), the pickers (`prefix + s`/`w`/`W`), the `og-remote-*` launchers.

### Claude Code plugin

The repo doubles as a CC plugin marketplace: `.claude-plugin/marketplace.json` → `claude-plugin/` (manifest, `hooks/hooks.json` driving the claude-status state machine, `skills/`). Hook commands route through `claude-plugin/scripts/status.sh`, a no-op when `claude-status-update` is off PATH. Install with `claude --plugin-dir "${inputs.tmux-og}/claude-plugin"` (Nix) or `claude plugin marketplace add noamsto/tmux-og` + `claude plugin install tmux-og@tmux-og`. `programs.tmux-og.skills.enable` symlinks the same `skills/` into `~/.claude/skills` — disable it when the plugin is installed.

## Key Conventions

- **Shell scripts are bash** (they run in tmux's environment); shfmt indents with tabs. Skip `compgen` — nixpkgs' non-interactive `bash` lacks it.
- **tmux `-F` formats are `|`-delimited.** tmux rewrites tabs and newlines to `_` for a non-UTF-8 querying client, collapsing the row into one field (#373). Enforced by `checks.<system>.tmux-format-delimiter-assertions` and `go test ./tmuxformat/...`.
- **Target a session by id (`-t '$N'`) in `set-option`** — a numeric name like `0` also resolves as a pane index. `show-options` rejects the `=name` exact-match prefix and `-q` hides the error as an empty value: read session options with a bare `-t "$name"` or `display-message -p`.
- **Address a window or pane by id (`@N`/`%N`), never `<sess>:<index>`** — `renumber-windows` is on, so an index captured earlier slides onto a neighbour.
- **Every hand-rolled tmux repro runs on its own server with its own state dir:**
  `TMUX_TMPDIR=/tmp/og-$$ CLAUDE_STATUS_DIR=/tmp/og-$$/status tmux -L probe new-session -d …`, then `kill-server` on that same socket. Inside a pane `$TMUX` is set, so a bare `tmux new-session` leaks a session onto the user's live server and a bare `kill-server` kills it. `CLAUDE_STATUS_DIR` defaults to a `/tmp/claude-status` shared by every server on the machine, and a scratch server prunes and reaps from it. Reading the live server is fine.
- **Drive a background side effect from a `-B` monitor hook, never from `status-format`.** A control-mode client (the remote bridge) renders no status line, so `#()` jobs never run on a bridge-only host (#603). Anything destructive to the shared `/tmp` dirs stays on the client-gated per-tick path. The three `-B` traps (empty target field, string body under a version guard, unconditional `-u -B` clear) are in `tmux-gotchas.md`.
- **Window options are the source of truth** for enrichment (`@issue_*`, `@pr_*`): only the stamp/enrich scripts write them; display formats, keybinds and pickers only read.
- **A `@bridge_win` mirror window is daemon-owned.** Local scripts skip it; the daemon writes `@bridge_*`, never the real option names (a same-name write is a two-writer race reflow wins). Every consumer naming a mirror pane's command prefers `@bridge_proc`.
- **Remote-derived values are sanitized daemon-side before any local format renders them** — `#(…)` in a carried value would execute here.
- **Every float bind stamps `@float_geom`** and pins `remain-on-exit off` (`float-conf-assertions` fails the build otherwise).
- **Duplicated tables must stay byte-identical**: `ENRICH_PIE_GLYPHS` (shell) ↔ `enrichstate.PieSlices` (Go); the draft-badge rule in `build_window_label`, `enrichstate.Draft` and the picker's `colorPRBadge`; agent priority order in `claude_priority_state` ↔ `agentPriority`.
- **Picker cells are sized with `visibleWidth`, never `len`**; a typed key is `printableKeyText`, never `len(key) == 1`.
- **A resident tmux server may predate the rebuilt binary** (#407) — version-gate on the live `#{version}`, never `tmux -V`.

## Plans and Specs

`docs/superpowers/plans/` and `docs/superpowers/specs/` are tracked directories, not scratch space. A substantial change commits its plan (`YYYY-MM-DD-slug.md`) — and its design spec (`YYYY-MM-DD-slug-design.md`) where one exists — alongside the code, in the same PR.

## Skill Usage

Most work here is a small, surgical edit to one script or `config/tmux.conf.nix` — match the surrounding file and stay in scope. Reach for planning/brainstorming/debugging skills only when the user asks for a plan or design, or when a bug genuinely resists a direct fix.
