# tmux-og — Claude Code plugin

The Claude Code side of [tmux-og](https://github.com/noamsto/tmux-og): lifecycle
hooks that drive the tmux status bar, plus three skills. This README is written
for an **agent setting the plugin up on its own** — the steps are copy-pasteable
and each command is non-interactive.

> Human installing tmux-og's tmux itself (Nix / home-manager)? See the
> [top-level README](../README.md). This file only covers the Claude Code plugin.

## What this plugin is

| Component | What it does |
|-----------|--------------|
| **Hooks** (`hooks/hooks.json`) | A state machine over the CC lifecycle (`SessionStart`, `PreToolUse`, `PostToolUse`, `Stop`, `Notification`, `PreCompact`, …). Each event routes through `scripts/status.sh <state>`, which writes the pane's Claude state (`processing`/`waiting`/`done`/`idle`/…) so the tmux status bar reflects it live. |
| **Skills** (`skills/*/SKILL.md`) | `tmux-og:issue-tracking`, `tmux-og:tmux-interactive` — see [Skills](#skills). |

**Safe to install anywhere.** `status.sh` `exit 0` silently when the
`claude-status-update` binary isn't on `PATH` (i.e. you're not in a tmux-og tmux
pane). The plugin never errors on a hook; the status-bar effects simply appear
once Claude is running inside tmux-og's wrapped tmux.

## Prerequisites

- Claude Code with plugin support: `claude --version`.
- For the **status-bar effects to be visible**: Claude must be running inside a
  pane of tmux-og's wrapped tmux (the `claude-status-update` binary on `PATH`).
  Installing the plugin without that is harmless — hooks no-op.
- The skills assume a tmux session; `tmux-interactive` needs a tmux pane to act
  on.

## Install

Pick the path that matches the environment.

### A. Marketplace (most setups)

```bash
claude plugin marketplace add noamsto/tmux-og
claude plugin install tmux-og@tmux-og
```

`tmux-og@tmux-og` is `<plugin>@<marketplace>` — both are named `tmux-og`
(`.claude-plugin/marketplace.json`).

### B. Local plugin dir (development, or pinned via Nix)

Point Claude at the plugin directory directly — no marketplace, no install step.
The Nix flake exposes the plugin at a read-only store path, which pins the plugin
and the tmux scripts to one revision:

```nix
# in your claude wrapper
claude --plugin-dir "${inputs.tmux-og}/claude-plugin"
```

Or against a checkout:

```bash
claude --plugin-dir /path/to/tmux-og/claude-plugin
```

### C. Skills only (plugin already wired another way)

If tmux-og's home-manager module manages the hooks and you only want the skills,
`programs.tmux-og.skills.enable` symlinks `claude-plugin/skills/` into
`~/.claude/skills`. Disable it when the full plugin is installed — otherwise the
skills load twice.

## Verify

```bash
claude plugin list                 # tmux-og present + enabled?
claude plugin list --enabled       # enabled only
```

In an interactive session the slash-command equivalents are `/plugin list` and
`/plugin` (the manager UI); `/reload-plugins` picks up hook/skill changes without
restarting.

What to expect:

- **Skills loaded** — `tmux-og:issue-tracking`,
  `tmux-og:tmux-interactive` appear in the skill list immediately.
- **Hooks active** — fire on the next lifecycle event. To confirm they reach the
  status writer (only meaningful inside a tmux-og tmux pane):

  ```bash
  command -v claude-status-update && cat "${CLAUDE_STATUS_DIR:-/tmp/claude-status-$(id -u)}"/panes/* 2>/dev/null
  ```

  A `state=…` line for the current pane means the hook chain works end to end.

## Skills

| Skill | Use it when |
|-------|-------------|
| `tmux-og:issue-tracking` | Working a Linear/GitHub issue or PR whose branch is **not** the current tmux window's branch — orchestrating from `main`, spawning agents into worktrees, driving PRs. Stamps issue ids into the status bar. |
| `tmux-og:tmux-interactive` | Driving an interactive CLI (Python REPL, gdb, psql, node, lldb) that needs keystroke-level control, output scraping, or waiting on prompts inside a tmux pane. |

Skills auto-invoke from their descriptions; no manual step beyond having the
plugin installed.

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| Skills don't appear | Run `/reload-plugins`; confirm `claude plugin list` shows tmux-og enabled. Check each `skills/*/SKILL.md` has valid frontmatter (`name`, `description`). |
| Status bar shows nothing | Expected unless Claude runs inside tmux-og's wrapped tmux. Check `command -v claude-status-update` — if absent, the hooks are no-opping by design. Install/run tmux-og's tmux (see [top README](../README.md)). |
| Hooks seem dead even in tmux | `cat "${CLAUDE_STATUS_DIR:-/tmp/claude-status-$(id -u)}"/panes/*` after a tool call — empty means the writer isn't on `PATH`. The tmux server may predate the tmux-og deploy; restart it so panes inherit the new `PATH`. |
| Skills loaded twice | The plugin and `programs.tmux-og.skills.enable` are both active. Pick one (see [Install §C](#c-skills-only-plugin-already-wired-another-way)). |

## Quick reference

```bash
# Install + verify, start to finish
claude plugin marketplace add noamsto/tmux-og
claude plugin install tmux-og@tmux-og
claude plugin list --enabled
# (inside a tmux-og tmux pane, after one tool call:)
cat "${CLAUDE_STATUS_DIR:-/tmp/claude-status-$(id -u)}"/panes/*
```
