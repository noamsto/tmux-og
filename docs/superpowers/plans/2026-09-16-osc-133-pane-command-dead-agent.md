# PLAN — #646: drive dead-agent detection from `pane-shell-prompt` (OSC 133)

Plan of record for `feat/646-use-osc-133-pane-command-hooks-for-dead`. Derives from
`docs/superpowers/specs/2026-09-16-osc-133-pane-command-dead-agent-design.md` (spec-critic:
`accept`, notes folded in). Scope is one coherent change: clear an exited agent's state from the
shell's own prompt mark, keep the floor as backstop. No `picker/remotebridge/**` changes, no float
or `pane-border-format` touches (#648), no `pane-died`/`pane-exited` duplication (#647).

## Verified facts the plan leans on (from the spec, re-measured)

- `pane-shell-prompt` is a pane-scoped hook (WINDOW|PANE), wired with `set-hook -g` like
  `pane-exited` (#647) — verified: `-g` fires on a detached server.
- In the hook's `run-shell`, usable formats are `#{hook_pane}` (`%N`), `#{pane_current_command}`,
  `#{session_name}`, `#{window_id}`. **Never** `#{session_id}` (the `$N` re-expands to `sh` in
  `run-shell`) nor any `#{hook_session_*}` / `#{hook_window_id}` (empty: `events_fire_pane`
  payloads only pane/window).
- `#{pane_current_command}` = the foreground process-group leader's argv[0] basename. At a real
  prompt it is the shell (`fish`/`bash`); at a nested prompt inside a still-running agent it is
  the agent. `#{q:session_name}` preserves spaces as one argv (verified).
- Fish 4.9.3 emits `133;A`/`B`/`C`/`D;<status>` by default; bash without shell-integration emits
  nothing. SIGINT on the foreground agent still ends in a prompt redraw (`A`) — kill-safe.

## Consumer map (shared-contract edges, per EVIDENCE_REVIEW.md)

Producer: `claude_clear_agent_state` (new) deletes `panes/<id>`, `screen/<id>`,
`interrupt/<id>` and empties pane options `@claude_status`/`@agent_screen`. Consumers of those:

| Edge | Reads | Disposition |
| --- | --- | --- |
| `read_pane_state` (lib-claude) | `panes/<id>` + `screen/<id>` | compatible: file gone → returns 1 (no state). `interrupt/<id>` unread once `panes/<id>` is gone. |
| `claude_pane_ids` (lib-claude) | globs `panes/*`,`screen/*` | compatible: id drops out. |
| `picker/statusline`, pickers (Go) | `panes/*`,`screen/*` directly | fixed: this is the consumer-asymmetry gap the clear closes; they stop rendering. |
| bridge shipper `agentstatus.go` | `@claude_status`/`@agent_screen` options | compatible: empty carried state → `removeFiles`/`removeScreenFile` on the mirror (verified; no change). |
| `agent-detect` `statefile.Writer` | writes `screen/<id>` + `@agent_screen` | compatible: its own `Clear` already empties `@agent_screen`; an exit-to-prompt runs it anyway. |
| floor (`live/`, `.sweep`, `read_pane_state` veto) | `live/<id>` presence | compatible: clear is immediate and file-level; floor stays for non-OSC-133 shells/old remotes (Q4). |
| `claude_progress_emit` | pane tty | compatible: cleared via the same emit. |

Changed files carry the full map in the PR `## Evidence`; the deterministic gate includes the
changed shell script + the new bats check only (no Go consumer files change).

## Steps

### Step 1 — `scripts/lib-claude.sh`: add `claude_clear_agent_state PANE_ID SESSION`

Insert after `claude_reap_pane` (same provenance comment family). Signature `(pane_id, session)`.

1. Normalize `%N`: strip a leading `%`, then require `^%[0-9]+$` after re-prefixing, exactly the
   `claude_reap_pane` posture (fail closed on junk).
2. **Short-circuit:** if none of `$CLAUDE_PANES_DIR/$id`, `$CLAUDE_SCREEN_DIR/$id`,
   `$CLAUDE_INTERRUPT_DIR/$id` exist → `return 0` (an agent-free prompt costs one `[[ -f ]]`,
   skipping the two `tmux set` forks and the tty write on the near-universal case). Residual,
   accepted and self-healing: a *stale `@claude_status`/`@agent_screen` option with no backing
   file* survives the short-circuit — but every writer/clearer sets the option atomically with
   the file (`bridge_stamp` after the `cat`; `claude-status-update clear` clears both; pane death
   drops the pane and its options), so the state is unreachable except by a partial failure
   (`rm` ok, `tmux set` failed), which the next write or clear re-syncs.
3. **Ownership guard (panes file):** if `panes/<id>` exists, read its `session=` field (same
   `while IFS='=' read` loop); if both the field and SESSION are non-empty and they differ →
   `return 0` (foreign server's colliding id, or a rename-session → fail closed to the floor). If
   `panes/<id>` is absent or its `session=` is empty, proceed unguarded (screen-only pane, same
   #647 residual).
4. `claude_progress_emit "$id" clear`; `rm -f` exactly `panes/<id>`, `screen/<id>`,
   `interrupt/<id>`; `tmux set -pq -t "%${id}" @claude_status "" \; set -pq -t "%${id}"
   @agent_screen ""` wrapped `2>/dev/null || true` (best-effort option clear for the bridge).

No touch to `tasks/`,`issues/`,`names/`,`watchers/`,`live/` (spec Q5). No touch to the floor
(`claude_agent_gone`, `claude_live_epoch`, `read_pane_state`).

Verification: `bash -n scripts/lib-claude.sh`; `setup_lib_claude` + a direct
`claude_clear_agent_state` call in a bats-style tmp dir later (Step 5's unit assertions), and the
full path via Step 5's scratch server.

### Step 2 — `scripts/tmux-shell-prompt.sh` (new)

Thin entry point, mirroring `tmux-reap-pane.sh`: `set -euo pipefail`; guarded source
`[[ -f "@lib_claude@" ]] && source "@lib_claude@" || exit 0`. Then:

```bash
AGENT_COMMANDS="${AGENT_COMMANDS:-@AGENT_COMMANDS@}"
normalize_wrapped_cmd() {          # same 3 lines as the sweep, but set -e safe
	REPLY="$1"
	[[ $REPLY == .*-wrapped ]] && REPLY="${REPLY#.}" && REPLY="${REPLY%-wrapped}"
	return 0
}
pcc=""
normalize_wrapped_cmd "${2:-}"
pcc="$REPLY"
[[ -n $pcc ]] || exit 0            # empty pcc fails closed
case " $AGENT_COMMANDS " in
*" $pcc "*) exit 0 ;;              # nested prompt inside a live agent
*) : ;;
esac
claude_clear_agent_state "${1:-}" "${3:-}"
```

Args from the hook (Step 3): `$1 = #{q:hook_pane}`, `$2 = #{q:pane_current_command}`,
`$3 = #{q:session_name}`.

Verification: `bash -n scripts/tmux-shell-prompt.sh`; `shellcheck`.

### Step 3 — `config/tmux.conf.tmpl` + `config/tmux.conf.reference.nix` (edit together)

Three coordinated edits (the check diffs the two against each other):

1. **Clear block** (tmpl ~line 395, reference ~line 755): add `set-hook -gu pane-shell-prompt`
   beside the `pane-exited`/`pane-died` clears, so `prefix + r` stays idempotent.
2. **Setter, index 0**, beside the `pane-exited`/`pane-died` setters:
   - tmpl: `set-hook -g pane-shell-prompt 'run-shell -b "{{index .Paths.Scripts
     "tmux-shell-prompt"}} #{q:hook_pane} #{qs:pane_current_command} #{qs:session_name}"'`
   - reference: `set-hook -g pane-shell-prompt 'run-shell -b
     "${script.tmux-shell-prompt}/bin/tmux-shell-prompt #{q:hook_pane} #{qs:pane_current_command}
     #{qs:session_name}"'`
3. Comment above it: this is the #646 OSC-133 dead-agent clear; index 0, `run-shell -b`;
   `pane-current-command`/`session_name` are wrap-required formats (per
   `tests/conf-shell-quoting.bats` `WRAP_REQUIRED_FORMATS`) so they take bare `#{qs:}` — never
   `#{q:}`, which loses a leading `~`/word; `#{q:hook_pane}` is a `%N` id. `pane-command-finished`/
   `pane-command-started` deliberately unwired.

Verification: `nix build .#default` renders; the extraction check (part of `nix flake check`)
passes only if tmpl and reference agree byte-for-byte on the rendered text.

### Step 4 — wire the script name in `config/tmux.conf.nix` and `generator/`

1. `scriptNames` (config/tmux.conf.nix ~:274): add `"tmux-shell-prompt"` next to
   `"tmux-reap-pane"` (the list is group-ordered, not sort-asserted).
2. New builder beside `mkScriptWithLibs`:
   ```nix
   mkScriptShellPrompt = name:
     pkgs.writeShellScriptBin name (
       builtins.replaceStrings
       ["@lib_claude@" "@AGENT_COMMANDS@"]
       ["${lib-claude}" agentCommands]
       (builtins.readFile ../scripts/${name}.sh)
     );
   ```
3. Route in the `script = lib.genAttrs scriptNames (name: if …)` map:
   `else if name == "tmux-shell-prompt" then mkScriptShellPrompt name` (before the `else
   mkScript name` fallback; place next to the `tmux-reap-pane` → `mkScriptWithLibs` branch).
4. **`ogInternal`** (config/tmux.conf.nix ~:704): add `"tmux-shell-prompt"` (sorted, between
   `"tmux-scratchpad"` and `"tmux-smart-nav"`). The forced `ogPartitionOk` assert
   (`sort(ogVerbSpec scripts ++ ogInternal) == sort(scriptNames)`, ~:949) fails the whole
   evaluation — `nix build .#default` included — if the name lands in `scriptNames` only.
5. **`generator/paths/paths.go` `RequiredScripts` (~:33):** add `"tmux-shell-prompt"` (sorted,
   between `"tmux-session-picker"` and `"tmux-splash-maybe"`). The Step 3 template key
   `{{index .Paths.Scripts "tmux-shell-prompt"}}` is rendered with `missingkey=error`, and
   `FromPrefix` builds `Paths.Scripts` from `RequiredScripts` alone — without the entry the
   extraction check's `--prefix smoke`/`og init → og generate --prefix` legs fail
   (`nix flake check`). This is the #647-plan step the earlier #646 draft dropped.

`agentCommands` already exists (line ~269) and equals `claude codex cursor-agent pi` — the same
list `tmux-update-icons.sh`/`tmux-agent-usage.sh` receive, so the discriminator and the sweep
cannot diverge.

Verification: `nix build .#default` (the `ogPartitionOk` assert is forced here);
`nix flake check` (the extraction/`--prefix` checks exercise `RequiredScripts`); and the wrapped
store path for `tmux-shell-prompt` must not contain the literal `@AGENT_COMMANDS@`.

### Step 5 — `tests/pane-shell-prompt.bats` (new) + flake check

Model on `tests/reap-pane-hook.bats`: `TMUX_BIN` + scratch socket + scratch `CLAUDE_STATUS_DIR`
+ seeded `panes/`,`screen/`,`interrupt/` and the two pane options. Pane shell is `bash` (the
mark is `printf`ed; bash builtin `printf` is fork-free so `pane_current_command` stays `bash`).
**Seed `panes/<id>` with `session=<that pane's session>`** (mirror `seed_pane_state`), else the
Step 1.3 ownership guard (`session=` mismatch vs firing session) skips and a clearing case reads
as a false fail; `screen/`/`interrupt/` seeds are content-free (`x`), exactly like
`reap-pane-hook.bats`.

Cases:

1. **agent exits to prompt → cleared.** `t new-session -d -s one -- bash`; seed id; set pane
   options `@claude_status`/`@agent_screen`; `t send-keys 'printf "\033]133;A\033\\"' Enter`;
   `wait_for 5` that `panes/`,`screen/`,`interrupt/` for that id are gone and both options are
   empty. (The `A` fires `pane-shell-prompt` with `pane_current_command=bash` — a non-manifest
   shell, so the discriminator clears.)
2. **nested prompt inside a live agent → not cleared.** seed id; run a foreground process whose
   argv[0] is a manifest command and that emits an `A` while alive:
   `t send-keys 'exec -a pi bash -c '\''printf "\033]133;A\033\\"; sleep 3'\''' Enter`; after ~1s
   assert `panes/<id>` still present and `@claude_status` still set (pane_current_command=`pi` ∈
   manifest → skip). Cleanup waits out the `sleep 3`.
3. **no OSC 133 → floor unchanged.** seed id; `t send-keys 'echo hi' Enter` (bash emits no mark);
   assert the three files remain after ~1.5s (the hook never fires; the floor is the only
   withdrawal path, already pinned by `tests/agent-liveness.bats` which stays green).
4. **cross-server ownership guard.** two wrapped servers on different `-L` sockets sharing
   `CLAUDE_STATUS_DIR`; seed `panes/0` with `session=alpha` from server A; on server B create a
   pane (`%0`, session `beta`) and emit `A`; assert A's `panes/0` survives (mismatch → skip).

Wire in `flake.nix` a new `osc-133-dead-agent-tests = pkgs.runCommand …` mirroring
`reap-pane-hook-tests` (lines ~1810): `nativeBuildInputs = [pkgs.bash pkgs.bats pkgs.coreutils
pkgs.gnugrep (mkTmux pkgs)]; TMUX_BIN = "${tmuxConfig.tmux-wrapped}/bin/tmux"; LANG/LC_ALL
C.UTF-8`, `cp -r ${./tests} … ; bats tests/pane-shell-prompt.bats`.

Verification: `bats tests/pane-shell-prompt.bats` locally (with the built wrapped `TMUX_BIN`), and
via the check in `nix flake check`.

### Step 6 — `CLAUDE.md` "Dead-agent floor" update

Extend the existing bullet with: the `pane-shell-prompt` (OSC 133) clear (`claude_clear_agent_state`
+ `tmux-shell-prompt`), the `panes/screen/interrupt` + `@claude_status`/`@agent_screen` clear set,
the event/floor precedence (event clears immediately; floor remains for non-OSC-133 shells and old
remotes), the `session=` ownership guard and its grouped-session/rename-session residuals, and the
inherited non-manifest-argv[0] gap. Keep it a few sentences, matching surrounding style. Also add
a `tmux-shell-prompt` row to the Script Roles table at the top (the #647 `tmux-reap-pane`
precedent): invocation `pane-shell-prompt` window hook with `#{q:hook_pane}
#{q:pane_current_command} #{q:session_name}`; purpose — clears an exited agent's state from the
OSC 133 prompt mark.

### Step 7 — gate

Three separate commands, in order: `nix build .#default`; `nix flake check` (bats + the new
extraction agreement + `osc-133-dead-agent-tests`); `nix build .#lint`.

## Out of scope / invariants (carried from the spec)

- No `pane-command-started`/`pane-command-finished` wiring.
- No `picker/remotebridge/**` change (Q2 rides the untouched `agentstatus.go` shipper).
- No float binds, no `pane-border-format`.
- Hook path never sweeps a directory (one pane id → three unlinks + two option writes).
- `claude_reap_pane`, `claude_reap_dead_panes`, `claude_prune_stale_state`, the floor and
  `read_pane_state` are untouched byte-for-byte.

## Ordering / risks

- Steps 1–2 (script + lib) are independent of 3–4 (config) but the hook can't fire until both
  land; do 1→2→3→4, then 5 (tests need the wired config), then 6, then 7.
- The two-file rule (Step 3) is the likeliest CI failure: the reference uses `${script.NAME}` while
  the tmpl uses `{{index .Paths.Scripts "NAME"}}` — keep them textually in sync.
- `exec -a pi bash -c …` relies on bash's `exec -a` (argv[0] override); if a sandbox bash lacks
  it, fall back to a tiny compiled helper named `pi` (gcc is in the check env) — a mechanical
  swap in Step 5, not a design change.
