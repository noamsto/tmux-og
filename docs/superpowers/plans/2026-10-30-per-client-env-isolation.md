# Keep per-machine env from leaking across attached clients (#1037)

## Design (two approaches, one chosen)

- **A. Per-client resolution only**: every consumer reads the invoking client (`#{I/e:VAR}`). Fixes the keybind (smart-nav) but not panes: a pane inherits the *session* environment, which has no per-client form.
- **B. Record local values, restore after an ssh attach** (hook) plus **per-client** smart-nav. Chosen: B fixes spawned panes and hooks while leaving `update-environment` untouched; the per-client read in smart-nav additionally stops an *ssh* client's Ctrl-hjkl from driving the desktop's kitty.

Mechanism:
1. `update-environment` is unchanged (single-client and `new-session` behaviour byte-identical to main).
2. One hook body on `client-attached[30]` and `after-new-session[40]` (`after-new-session[30]` is the birth-size hook; `client-attached` does not fire for `new-session`). A client with an empty `SSH_CONNECTION` (local) records `DISPLAY WAYLAND_DISPLAY WINDOWID XAUTHORITY KITTY_LISTEN_ON AEYE_HOST` into **per-session** `@og_local_<VAR>` (`set -F`, no `-g`) and stamps `@og_local_seen 1`; an ssh client's attach has already overwritten the session env, so the hook restores per var: non-empty record -> `set-environment -F VAR`; empty record but `@og_local_seen` -> `set-environment -r VAR` (the local client lacked it, as `update-environment` would mark); never seen -> leave the ssh client's value. No `-t` on `set-environment` (targets the hook's session; `-t #{hook_session}` is a silent no-op). `client-attached` is asynchronous and a bare `#{I/e:}` there resolves the session's best client, so it stashes `#{hook_client}` in global `@og_attach_client` and reads the vars through `#{L:...}` filtered on that client; after-new-session is synchronous and reads the creating client directly, gated on `#{==:#{client_session},#{session_name}}` so `new-session -d` does nothing. Both are cleared by the bare `set-hook -gu` of the hook names.
3. `tmux-smart-nav` takes an optional 8th arg `client=<name>` (bind text `client=#{?client_name,#{q:client_name},}`: always one token). The script resolves that client's `KITTY_LISTEN_ON` (`display-message -c`, no `SSH_CONNECTION` gate); arg present (even `client=`) is authoritative; an unreachable socket fails `kitty @` and falls back to `select-pane`; absent falls back to the env var. `#{q:#{...}}` does not shell-quote, hence the resolution in the script.

Accepted trade-off: an ssh client's panes see the last local client's `DISPLAY`/kitty values; `SSH_*` stay last-attach-wins.

## Superseded decisions
- **Pin design (abandoned):** dropping the six vars from `update-environment` and pinning them from local clients in `client-attached[30]`. Reviewers blocked it: `client-attached` does not fire for `new-session`, so a local `tmux new` lost the vars on its first pane and later sessions inherited another terminal's globals; a local client lacking a var no longer cleared the old value (single-client behaviour changed); it was fragile on older resident servers; its flake check needed `script`, which darwin lacks, and a `! grep` assertion that cannot fail under `set -e`.
- **Bare `#{I/e:}` in `client-attached` (abandoned):** measured to read the wrong client when two clients attach quickly; replaced by the `#{hook_client}` + `#{L:}` form.

- **Global records with empty-means-unseen (abandoned):** an empty `@og_local_VAR` meant both "no local client yet" and "local client lacks VAR", so a local client without kitty followed by an ssh client left the ssh client's `KITTY_LISTEN_ON`; replaced by the `@og_local_seen` stamp. Global records also let a restore write another terminal's values into an unrelated session; records are now per session.
- **Ungated after-new-session body (abandoned):** `new-session -d` wiped records or, with an ssh client attached elsewhere, restored; gated on the creator being attached to the new session.
- **`SSH_CONNECTION` gate in smart-nav (abandoned):** blanked the socket for every ssh client, breaking `kitten ssh` remote-control forwarding; the client's own socket is now used.

## File list
- `config/tmux.conf.tmpl` and `config/tmux.conf.reference.nix` (edited identically; `flake.nix` extraction check diffs them): new hooks, smart-nav bind args.
- `scripts/tmux-smart-nav.sh`: 8th arg.
- `tests/multi-client-env.bats` (new): scratch server, two pty clients; registered in `flake.nix`.
- `tests/smart-nav.bats`: 8th-arg cases.
- `flake.nix`: smart-nav conf assertion (8 args), `update-environment-conf-assertions` (hooks record and restore all six vars, no `-t`), new test check.
- `docs/agents/tmux-gotchas.md`, `docs/agents/scripts.md`: multi-client env rule.

## Steps
- [x] **Step 0: probe** on a scratch server (own `TMUX_TMPDIR`/`CLAUDE_STATUS_DIR`), pty clients via `script -qfc`: `#{I/e:VAR}` reads the attaching client in `after-new-session` (and a `new-session` pty client); in `client-attached` it needs the `#{hook_client}` + `#{L:}` form.
- [x] **Step 1: failing test** `tests/multi-client-env.bats`: local client L then ssh client R attach; assert the session env is restored to L's values and a spawned pane sees `:0`; ssh-only attach keeps the ssh values; a pty `new-session` then an ssh attach restores that session's values; a single local client lacking `KITTY_LISTEN_ON` still marks it removed; smart-nav per client. Skips without util-linux `script`. Red against main's conf.
- [x] **Step 2: conf change** (`tmpl` + `reference.nix`): the two hooks. Step 1 passes.
- [x] **Step 3: smart-nav** tests then script (8th arg).
- [x] **Step 4: flake assertions**: smart-nav bind text/count; `update-environment-conf-assertions` (list unchanged, hooks on both events record and restore every var, no `-t`, after the clear; negated checks use `if grep`, never `! grep`).
- [x] **Step 5: docs**: `tmux-gotchas.md` rule and trade-off, `scripts.md` smart-nav row.
- [ ] **Step 6: gates**: `nix build .#lint`, `nix flake check`.

## Acceptance
- [ ] two-client bats red on main, green now (Step 1/2)
- [ ] single client unchanged: `tests/smart-nav.bats`, `tests/carousel-restore.bats` pass; a local client lacking `KITTY_LISTEN_ON` still marks it removed
- [ ] docs/agents/tmux-gotchas.md rule
- [ ] lint, flake check, default build pass; CI run id cited in PR
