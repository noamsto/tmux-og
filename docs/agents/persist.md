# Persist (tmux-remux)

The [tmux-remux](https://github.com/noamsto/tmux-remux) Go binary is the persistence
layer (replaces tmux-resurrect/tmux-continuum). Enabled by default via
`programs.tmux-og.persist.enable`; set to `false` to opt out.

- The hook wiring is rendered at tmux config-load time by `tmux-remux triggers`
  (noamsto/tmux-remux#65) rather than hand-written: `tmuxRemuxWireScript`
  (`modules/home-manager.nix`) runs it with `--tmux-version` set to `#{version}`
  — the LIVE server's own reported version, not a fresh `tmux -V` subprocess,
  since a server predating a nix switch keeps its old binary resident (#407) —
  reindexes its bare `set-hook -g <event>` lines onto `[99]` (a sed pass, to
  avoid colliding with tmux-og's own index-0 hooks on the same events, e.g.
  tmux-reflow-windows on `window-unlinked`), and sources the result.
- tmux hooks fire `tmux-remux save` on structural change and
  `tmux-remux capture-event` on close, on any tmux version.
- On tmux 3.8+, `triggers` also emits a `set-hook -g -B` monitor hook
  (`@remux-save:session:...`) that fires `tmux-remux save --reason=timer` on
  its own clock — this replaced the `tmux-og-remux-save` systemd
  timer/service (and macOS launchd agent) removed in #344. A **live** server
  that's still pre-3.8 (#407) gets no periodic-save floor at all — only the
  structural/close hooks above — and the wire script warns on the pane via
  `tmux display-message` in that case rather than leaving it silently off;
  restarting the server onto 3.8+ resolves it. Weekly GC (`tmux-og-remux-gc`,
  kept on both platforms) drops orphan scrollback files.
- Keybindings: `prefix + u` (undo pop), `prefix + U` (close-event picker),
  `prefix + R` (snapshot picker), `prefix + Ctrl-s` (immediate save).
- Storage: `$XDG_DATA_HOME/tmux-remux/state.db` + scrollbacks dir.
- `restoreMode` defaults to `"off"` (manual `prefix + R` only). Set to `"auto"`
  to apply the smart filter on tmux server start.
- Pi sessions resume like the other agents via `persist.resumePi` (default false):
  the flake Home Manager module symlinks `pi-hookyard-plugin` under
  `~/.local/share/pi/extensions/tmux-og` and idempotently adds its bridge to
  `~/.pi/agent/settings.json`. Restart pi after a switch; its extension set is
  loaded only at process start. Its hookyard-built bridge
  routes `session_start` and `turn_end` envelopes to `pi-relaunch-stamp`, which
  stamps the pane's `@remux_relaunch` with `pi <original flags> --session
  <file>` — every flag replayed with per-arg single quotes, the positional
  launch prompt and session-selection flags (`--session`/`--continue`/
  `--resume`/`--fork`/`--session-id`/`--no-session`) dropped. Hookyard removes
  secret-looking flags before the handler receives argv. It stamps on every turn, so
  repeated restore cycles survive as long as one message is sent per cycle
  (the cursor caveat), and never stamps an ephemeral session (`--no-session`
  ⇒ the bridge supplies no session file) or a value containing `|` or any control
  byte (C0 + DEL — the tmux format reader mangles them).
- **`pi` on PATH is commonly a wrapper that injects its own flags ahead of the
  caller's** (this machine's own `home/ai/pi/default.nix` loads a hook-bridge
  extension that way, via `/nix/store` paths) — `process.argv` inside pi
  carries the wrapper's injected flags too, and replaying them verbatim on
  restore would run the wrapper's own injections a SECOND time (double
  hook-bridge load) on top of persisting a store path that goes stale once
  garbage-collected. A wrapper that exports `PI_USER_ARGC=$#` right before its
  `exec` (the count of trailing argv entries that are the caller's own, since
  the wrapper appends `"$@"` last) lets `pi-relaunch-stamp.sh` replay only
  that trailing slice — the wrapper-injected prefix is dropped, and the
  CURRENT wrapper re-injects its own current store paths on restore. Unset (no
  wrapper, or an older one predating this contract) or malformed
  (non-integer, or larger than the available args) falls back to replaying
  everything, same as before this contract existed.
- **A pi pane launched by this repo's own dispatcher (crew/dispatch) has the
  same staleness on its own worker flags** (e.g. `--append-system-prompt
  <store path>/WORKER_PROTOCOL.md`, and any dispatch-supplied `-e` files).
  `pi-relaunch-stamp.sh` detects a dispatcher-launched pane via
  `CREW_WORKER_ID` in its environment (set by dispatch's own launch command,
  inherited straight through to the stamper): a worker lead (`worker:…`) gets
  `dispatch resume` stamped instead of a raw pi replay — that command
  re-resolves engine/model/effort and current protocol paths from the
  worktree's `WORKER_TASK.md` rather than replaying stale argv. A role-grid
  pane sharing the worktree (`role:…`) stamps nothing and restores as a bare
  shell: there is no "resume as role" verb, and role panes must not each
  launch a second lead — a role pane's own resume onto the crew bus is out of
  scope for this mechanism.
- Restored windows already get `@worktree`/`@branch`/`@issue_*` from the
  ordinary `after-new-window`/`after-new-session` creation hooks — tmux-remux's
  restore/undo/pick all create windows via `new-window -c`/`new-session -c`
  with the historical cwd, so the hook's cwd read never races an async `cd`
  (#100). This depends on tmux-remux continuing to pass `-c` at creation; if a
  future tmux-remux bump stops doing that, this would need revisiting.

