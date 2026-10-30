# Keep per-machine env from leaking across attached clients (#1037)

## Design (two approaches, one chosen)

- **A. Per-client resolution only**: every consumer reads the invoking client (`#{I/e:VAR}`). Fixes the keybind (smart-nav) but not panes: a pane inherits the *session* environment, which has no per-client form, and the carousel reconcile hook / aeye run in the session env.
- **B. Pin from local clients only** (hook) plus **per-client** smart-nav. Chosen: B fixes spawned panes and hooks; the per-client read in smart-nav additionally stops an *ssh* client's Ctrl-hjkl from driving the desktop's kitty (a session-env value is the desktop's even when the laptop pressed the key).

Mechanism:
1. `update-environment` drops `DISPLAY`, `WINDOWID`, `XAUTHORITY`, `KITTY_LISTEN_ON`, `AEYE_HOST` (display/kitty-local; wrong when set from another machine). Kept: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`, `SSH_CONNECTION`, `SSH_ASKPASS`, `KRB5CCNAME` (a remote client's panes need its forwarded agent: standard tmux last-attach-wins, documented as the accepted trade-off), and the terminal set (`TERM*`, `COLORTERM`).
2. New `client-attached[30]` hook (placed after the bare `set-hook -gu client-attached` clear; `[30]` added to that clear block if uncovered): for a client whose own `SSH_CONNECTION` is empty, `set-environment -F` each of the five vars from `#{I/e:VAR}` when non-empty, into BOTH the session (`-t #{hook_session}`) and the global env (`-g`), so sessions created later (`new-session` no longer runs `environ_update` for these) inherit the local machine's values. Step 0 verifies `#{I/e:VAR}` resolves against the attaching client inside the hook; fallback `display-message -c #{hook_client}`. An ssh client never writes; a local client lacking a var leaves the previous value (was: removed).
3. `tmux-smart-nav` takes an optional 8th arg `kitty=<sock>` (bind text `kitty=#{q:#{?#{I/e:SSH_CONNECTION},,#{I/e:KITTY_LISTEN_ON}}}`: always exactly one token, since a bare `#{q:}` of an empty value is zero words). Arg present (even `kitty=`) is authoritative, so an ssh client gets no kitty hand-off; arg absent falls back to the env var (existing tests unchanged).

## File list
- `config/tmux.conf.tmpl` and `config/tmux.conf.reference.nix` (edited identically; `flake.nix` extraction check diffs them): update-environment list, new hook, smart-nav bind args.
- `picker/remotebridge/cmd/daemon/main.go` comment (~l.82) and `modules/home-manager.nix:288` example: re-read, fix wording if inaccurate.
- `scripts/tmux-smart-nav.sh`: 8th arg.
- `tests/multi-client-env.bats` (new): scratch server, two pty clients; registered in `flake.nix`.
- `tests/smart-nav.bats`: 8th-arg cases.
- `flake.nix`: smart-nav conf assertion (8 args), `update-environment-conf-assertions` (pinned vars absent), new test check.
- `docs/agents/tmux-gotchas.md`, `docs/agents/scripts.md`: multi-client env rule.

## Steps
- [ ] **Step 0: probe** on a scratch server (own `TMUX_TMPDIR`/`CLAUDE_STATUS_DIR`), two pty clients via `script -qfc 'tmux attach'` as `tests/client-theme.bats` does: confirm `#{I/e:VAR}` inside a `client-attached` hook reads the attaching client.
- [ ] **Step 1: failing test** `tests/multi-client-env.bats`: server with the repo conf; client L (`KITTY_LISTEN_ON=unix:/local DISPLAY=:0`, no SSH_CONNECTION) attaches, then client R (`unix:/remote`, `DISPLAY=localhost:10`, `SSH_CONNECTION` set, lacking another var) attaches. Assert the session env still has L's values, a freshly spawned pane's `printenv DISPLAY` prints `:0`, and R's attach doesn't unset them; a session created after L attached gets L's `DISPLAY` in its first pane; the pinned `AEYE_HOST` is visible to the session env (carousel consumer). Isolation: per-run `TMUX_TMPDIR` and `CLAUDE_STATUS_DIR`, `kill-server` on teardown. Run `bats tests/multi-client-env.bats`: fails on main.
- [ ] **Step 2: conf change** (`tmpl`): list + hook. Re-run step 1 test: passes.
- [ ] **Step 3: smart-nav test then script**: bats cases (arg given / empty / unset). Run `bats tests/smart-nav.bats`.
- [ ] **Step 4: bind args + flake assertions** (smart-nav `grep -Fq` pattern and count of 4 updated to the exact `kitty=` bind text; update-environment assertion extended: pinned vars absent, hook after the clear); mirror into `tmux.conf.reference.nix`; `nix build .#default` and the extraction check.
- [ ] **Step 5: docs**: multi-client env rule, incl. that re-adding these vars via `extraConfig` reintroduces the leak.
- [ ] **Step 6: gates**: `nix build .#lint`, `nix flake check`, `bats tests/tmux-next38-readiness.bats tests/conf-shell-quoting.bats`.

## Acceptance
- [ ] two-client bats red on main, green now (Step 1/2)
- [ ] single client unchanged: `tests/smart-nav.bats`, `tests/carousel-restore.bats` pass
- [ ] docs/agents/tmux-gotchas.md rule
- [ ] lint, flake check, default build pass; CI run id cited in PR
