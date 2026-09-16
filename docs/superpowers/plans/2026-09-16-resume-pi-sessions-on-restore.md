# PLAN — #661: resume pi sessions on tmux-remux restore

Design intent: tmux-remux relaunches an agent pane on restore only when the pane carries a pane
option `@remux_relaunch` (exec'd verbatim via `/bin/sh -c`, later through the restored pane's
default-shell — fish included). tmux-og stamps it for claude (tmux-update-icons), codex
(`codex-relaunch-stamp` hook), cursor (`cursor-relaunch-stamp` hook) and the carousel — nothing
for **pi**. Every `pi` pane on the live server currently has an empty `@remux_relaunch`, so pi
sessions restore as bare shells.

## Verified pi facts (0.85.1, measured)

- Extensions load in-process; `process.argv` inside an extension is
  `["bun", "/$bunfs/root/pi", <user args...>]` — the first two entries are the bun runtime and
  the bundled-script path, so the replay starts at `process.argv.slice(2)` (matches what the
  user/launcher actually typed; on a machine with the nix-config `pi` wrapper the slice includes
  the wrapper-injected `-e hook-bridge.ts --skill … --prompt-template …`, which a restore replay
  correctly reproduces).
- `session_start` fires with `ctx.sessionManager.getSessionFile()` = the full session file path
  (already created for a new session at startup); `reason` is `"startup" | "reload" | "new" |
  "resume" | "fork"`. A bootstrap `session_start` with `getSessionFile() === undefined` fires
  first in some flows — the undefined-file check deliberately skips it.
- `--no-session` (ephemeral) ⇒ `getSessionFile()` is `undefined` — the same check covers it.
- `turn_end` fires per turn (one LLM response + tool calls) — the "every turn" re-stamp hook.
- `--session <path|id>` resumes by absolute session-file path or UUID (verified: resume by path
  re-attaches the exact session). The session file path is absolute and machine-local, so it
  survives restore regardless of cwd-slug or `PI_CODING_AGENT_DIR` changes — **the stamp appends
  `--session '<full session file path>'`**.
- Extensions are auto-discovered globally from `~/.pi/agent/extensions/*.ts` (docs/extensions.md:
  "Global (all projects)") — the install target; no settings.json mutation (pi owns that file).

## tmux-remux `@remux_relaunch` contract (read from source)

- `internal/restore/plan.go` `paneStartup`: a non-empty `@remux_relaunch` becomes `OverrideCmd`,
  emitted **verbatim** (no re-quoting) into the pane startup string
  `<relaunch>; exec <default-shell>` — the stamper must therefore single-quote every arg itself
  (POSIX/fish single-quote trick `'\''` is common to both).
- `internal/restore/shell.go` `DefaultShell`: the string runs through the user's default shell —
  fish included — so the stamp must be valid in fish: no leading `VAR=value`, no unquoted
  metacharacters (all neutralized by per-arg single quotes).
- The `|` constraint is not tmux-remux's but update-icons': the value is read back through a
  `|`-delimited `list-panes -F` (field 14 of 26, scripts/tmux-update-icons.sh:283), so a literal
  `|` (raw, inside quotes or not) shifts every field after it. Newline/tab are likewise mangled by
  tmux format output. → **refuse to stamp** when the assembled command contains `|`, tab, newline,
  or any control byte. This mirrors the codex/cursor stampers' reject-don't-sanitize posture.

## Steps

- [ ] **Step 1: `scripts/pi-relaunch-stamp.sh` (new — the testable transform + stamp)**
  - `#!/usr/bin/env bash`, `set -euo pipefail`, shfmt tabs. Interface:
    `pi-relaunch-stamp <session-file> <pi-argv...>` (the TS glue passes the file plus pi's real
    args as argv, so spaces/quotes/`$` round-trip without any encoding).
  - Guards first, copying cursor-relaunch-stamp.sh: `[[ -n ${TMUX_PANE:-} ]] || exit 0` and
    `command -v tmux >/dev/null 2>&1 || exit 0`. Empty/absent `$1` (ephemeral, defensive) → exit 0
    without stamping.
  - **Arg replay rules** over `"$2@"`:
    - Value-taking options (keep flag + next token; `--opt=value` keeps the whole token): the long
      forms `--provider --model --api-key --system-prompt --append-system-prompt --mode --session
      --session-id --fork --session-dir --name --models --tools --exclude-tools --thinking
      --extension --skill --prompt-template --theme --use-theme --export --tui-mode` and the short
      forms `-n -t -xt -e`. `--list-models` is treated as a flag (its search arg is optional and
      positional — dropped like any positional).
    - Session flags dropped entirely (flag + its value): `--session --session-id --fork
      --continue -c --resume -r` (plus `--no-session`, which cannot be in effect when the file
      exists and must not contradict the appended `--session`). `--api-key <key>` is dropped
      likewise: the credential would be persisted in the pane option / tmux-remux's state.db
      and re-exposed in the restored pane's argv — a keyed launch restores through the
      provider's env var instead (review finding #2).
    - Any other `-`-leading token is a kept flag. A `--` separator: drop it and everything after
      (all positional by definition).
    - A token that is neither (a positional message / `@file`) is dropped: it is the launch
      prompt, which must not be re-sent on restore.
    - A value-taking option consumes its next token **even when it starts with `-`** (e.g.
      `--name -weird`): the parser must not re-classify an option's value as a flag.
    - Version caveat (resumeCursor parity): a future pi bump adding a new value-taking flag
      degrades to "flag kept, its value dropped as positional" — a possibly-broken relaunch,
      never an exploitable one (the `|`/control-byte reject still holds).
  - Header note: a launch carrying `--no-extensions` (disables auto-discovery of the extension
    itself) or `--no-session` (ephemeral) never stamps — both degrade to the pre-#661 bare-shell
    restore, the documented failure mode.
  - Assemble `cmd="pi <each kept token single-quoted> --session '<session-file>'"` using the
    `'\''` trick (`shellQuoteSingle`, same as tmux-remux's own helper).
  - Reject: if the raw (pre-quoted) command string contains `|` → exit 0, or matches the POSIX
    `[:cntrl:]` class (C0 + DEL — tab, newline, CR, VT, FF included, which fall inside
    `[:space:]` on their own so the rule must be cntrl-not-space; review finding #1) → exit 0,
    no stamp (degrade to bare-shell restore, never a broken or exploitable relaunch).
  - Change-gate: `cur=$(tmux show-option -pqv -t "$TMUX_PANE" @remux_relaunch 2>/dev/null) ||
    cur=""`; exit 0 when `$cmd == "$cur"`; else
    `tmux set-option -p -t "$TMUX_PANE" @remux_relaunch "$cmd"` (no `-q`, matching update-icons'
    loud-write posture). Suppress nothing else — best-effort per the cursor precedent. Because of
    the read-back gate, the bats fake below must be **stateful** (serves `show` reads from a store
    the `set` writes fill) — a log-only fake would answer nothing, the second invocation would
    re-stamp, and the change-gate test (and the gate with it) would fail.

- [ ] **Step 2: `plugins/pi-relaunch-stamp.ts` (new — thin pi-extension glue)**
  - `import type {ExtensionAPI} from "@earendil-works/pi-coding-agent"` (type-only — erased at
    runtime, so the auto-discovered single .ts needs no node_modules; hook-bridge.ts precedent).
  - `const stamp = (ctx) => { const file = ctx.sessionManager.getSessionFile(); if (!file) return;
    execFile("pi-relaunch-stamp", [file, ...process.argv.slice(2)], () => {}); }` — errors
    swallowed (pane may vanish / tmux absent), `TMUX_PANE`/tmux checks live in the bash side so
    there is exactly one copy of the guard logic.
  - Wire `pi.on("session_start", …)` and `pi.on("turn_end", …)` to `stamp`.
  - Header comment: which events, the argv[0..1] coupling (measured on 0.85.1), why ephemeral
    (`--no-session`: `getSessionFile()` is `undefined`) and `--no-extensions` (the extension
    never loads) degrade to bare-shell restore, and that the relaunch replays the original
    flags so restore reproduces the launch (extension loads, model, thinking, no-approve all
    intact) minus the positional prompt.

- [ ] **Step 3: Nix packaging — `config/tmux.conf.nix`**
  - Add `"pi-relaunch-stamp"` to `scriptNames` (list at :276, beside the codex/cursor stamps at
    :303-307) — routed by the default `mkScript` branch (no placeholders), like the other
    stampers.
  - Add `"pi stamp"` to `ogVerbSpec` (beside `"codex stamp"` :646 / `"cursor stamp"` :637) with
    summary `"Stamp pi relaunch state (run by the pi extension, not interactive)"` — keeps the
    `ogPartitionOk` assert (:715) green, mirroring the codex/cursor entries.
  - No generator/`paths.go` change: the script is not referenced by the tmux.conf template.

- [ ] **Step 4: home-manager module — `modules/home-manager.nix`**
  - `persist.resumePi` option (bool, default `false`) after `resumeCursor` (~:485), description
    following the resumeCursor prose: when on, home-manager installs the pi extension into
    `~/.pi/agent/extensions/` (pi's global auto-discovery dir) and adds `pi-relaunch-stamp` to
    PATH; the extension stamps each pi pane's `@remux_relaunch` on session start and every turn
    with `pi <original flags> --session <file>`, so tmux-remux relaunches the resumed pi session
    instead of a bare shell. Notes: an extension only loads into pi processes started after the
    install (no settings hook exists to retrofit a running one — the pi counterpart of codex's
    one-time trust / cursor's missing re-fire event); a launch carrying `--no-extensions` or
    `--no-session` never stamps (bare-shell restore); and a user overriding `PI_CODING_AGENT_DIR`
    away from `~/.pi/agent` must install the extension into their own config dir's
    `extensions/` (the option installs to the default dir).
  - `resumePiEnable = cfg.persist.enable && cfg.persist.package != null && cfg.persist.resumePi`
    (:165-block, mirroring `resumeCursorEnable`).
  - `packages`: `++ lib.optionals resumePiEnable [tmuxConfig.script.pi-relaunch-stamp]` (beside
    the cursor resume line :1063).
  - `file`: `lib.optionalAttrs resumePiEnable { ".pi/agent/extensions/pi-relaunch-stamp.ts" = {
    source = ../plugins/pi-relaunch-stamp.ts; }; }` (the opencode-status.ts precedent :1092-1093,
    with a comment noting this is the whole global mechanism — no settings.json edit).

- [ ] **Step 5: `tmux-update-icons` guard — `tests/update-icons-resume-guard.bats`**
  - New @test next to the cursor case (~:232): write a `screen/<id>` state file (agent-detect
    scrapes pi like any known agent CLI, so a pi pane is a screen-only pane with no transcript),
    pre-stamp `@remux_relaunch` with a realistic pi relaunch (`pi --name reef --no-approve
    --session '/home/u/.pi/agent/sessions/--slug--/2026-09-16T00-00-00-000Z_01a0aa66-….jsonl'`),
    run update-icons, assert the stamp survives. This is the pi-specific instantiation of the
    existing "screen-only pane does not clobber another agent's relaunch stamp" guard — the guard
    logic itself needs no change (empty desired refuses to clobber), the test pins it for pi.

- [ ] **Step 6: transformation tests — `tests/pi-relaunch-stamp.bats` (new) + `flake.nix`**
  - Follow `cursor-relaunch-stamp.bats` (`load helper`, `export TMUX_PANE="%7"`). The fake tmux
    is **stateful** (the plan-critic's high finding): it parses
    `set-option -p -t <pane> @remux_relaunch <value>` lines into a per-pane store and answers
    `show-option -pqv -t <pane> @remux_relaunch` by echoing the stored value (failing quietly
    when unset) — the spy idiom update-icons-resume-guard.bats uses for its "no write is issued
    when the stamp already matches" test. Every argv line is also logged to `$TMUX_LOG`, so
    no-write assertions check the log while the gate works against the store.
  - Dispatcher-shaped argv: `--name reef --model opencode/deepseek-v4-flash --thinking high
    --no-approve` + a trailing launch prompt → stamped command ends `--session '<file>'` and the
    prompt words are absent.
  - Positional removal: prompt words never appear in the log; `@file` args dropped too.
  - Session-flag removal: an argv containing `--session old.jsonl --continue --resume -c -r
    --session-id X --fork Y` stamps exactly one `--session '<file>'` (the appended one) and none
    of the old flags/values survive.
  - Value-taking fidelity: `--name`/`--model`/`--append-system-prompt` values with spaces and
    single quotes survive single-quoted — assert the log contains the exact quoted rendering
    (e.g. `'--append-system-prompt' '/tmp/a b'\''c'`).
  - `|`-guard: an argv element containing `|` → no stamp at all.
  - Empty session-file arg (`--no-session` equivalent) → no stamp.
  - Change-gate: run twice with identical argv+file — the second run writes nothing (assert the
    log has exactly one `set-option` line; the store-backed `show` suppresses the re-stamp); a
    changed session-file path writes again.
  - `TMUX_PANE` unset → no call; tmux absent → no call.
  - Register `pi-relaunch-stamp-tests` in `flake.nix` `checks` (copy the
    `cursor-relaunch-stamp-tests` derivation pattern, nativeBuildInputs `[pkgs.bats
    pkgs.coreutils]`).

- [ ] **Step 7: CLAUDE.md Persist section**
  - Add a bullet to `### Persist (tmux-remux)` (after the restoreMode bullet, ~:392): the pi
    extension mechanism (`persist.resumePi`, `~/.pi/agent/extensions/`, session_start + turn_end
    stamping, `--session <file>` replay, per-turn re-stamp so repeated restore cycles survive as
    long as one message is sent per cycle — the cursor caveat, stated).

- [ ] **Step 8: manual demonstration + PR wiring**
  - Scratch server per CLAUDE.md (own `TMUX_TMPDIR`, `CLAUDE_STATUS_DIR` scratch too): install
    the extension + stamper to the scratch pi config dir (`PI_CODING_AGENT_DIR`),
    `pi -e <stamp.ts> --name demo --model opencode/deepseek-v4-flash --thinking high --no-approve
    "resume me" --print` inside a pane, assert `tmux show -pv @remux_relaunch` holds the expected
    replay; run a `--no-session` variant and assert it stays unset; also assert the value never
    contains `|`. Paste the stamped value in the PR body.
  - Commit this plan doc under `docs/superpowers/plans/`.

## Acceptance

- `nix build .#default`, `nix flake check`, `nix build .#lint` all pass (three separate commands).
- Tests: the update-icons guard pi case; the bats transform suite (positional removal, session
  flags, quoting with spaces/quotes, `--no-session`, `|` guard, change-gate).
- Manual demo paste + CLAUDE.md Persist bullet + plan doc committed.

## Out of scope

- The dispatcher repo's `dispatch resume`; changes to tmux-remux itself.
- A store-path-absolute `pi` in the stamp: `pi` resolves through PATH at restore (the profile
  tmux-og installs into on that machine), same as `claude`/`codex`/`cursor-agent` do.
