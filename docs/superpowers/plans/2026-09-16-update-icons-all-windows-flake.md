# update-icons-all-windows test 2 flake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `tests/update-icons-all-windows.bats` print `tmux-update-icons` stdout/stderr when the script exits non-zero, and either fix a demonstrated root cause of the CI-only test-2 flake or add a documented bounded retry.

**Architecture:** Keep the suite's structure. First stop swallowing the failing invocation (the defect that is true regardless of flake). Then inspect `scripts/tmux-update-icons.sh` last-command / unguarded tmux paths and the suite's tmux/`-u`/locale setup against the two named suspects. Change the production script only if a failure can be shown to exit non-zero; otherwise add a small retry around the test invocation with a comment that names what was ruled out. No `sleep`.

**Tech Stack:** bats 1.5, bash, tmux (private `TMUX_TMPDIR`), `nix flake check` / `nix build .#default` / `nix build .#lint`.

## Global Constraints

- Scope: `tests/update-icons-all-windows.bats` and, only if the root cause is there, `scripts/tmux-update-icons.sh`. Do not edit unrelated scripts or other bats files except to match an existing helper idiom copied into this file.
- Do not create tmux sessions on the live server; this suite already uses `TMUX_TMPDIR` + `unset TMUX` + scratch `CLAUDE_STATUS_DIR`.
- Do not pin `LANG`/`LC_ALL=C.UTF-8` on the flake check derivation: C.UTF-8 is missing on darwin and `worktree-match-integration-tests` deliberately leaves the sandbox locale hostile for #373.
- Do not add a bare `sleep`. A retry must be a bounded re-invoke of the same command.
- Neighbours that already use `bash "$UPDATE_ICONS" … >/dev/null 2>&1 || true` (`tests/update-icons-cwd-move.bats`, `tests/update-icons-enrich-trigger.bats`, `tests/reflow-fanout.bats`) are out of scope: they ignore failure by design.
- `Closes #653`.

---

## File map

| File | Role |
| --- | --- |
| `tests/update-icons-all-windows.bats` | Two tests; lines 91 and 130 both discard both streams. Test 2 is the `a\|b` session discriminator (#580). |
| `scripts/tmux-update-icons.sh` | Invoked as `bash "$UPDATE_ICONS" A`. No `set -e`. Keys windows by `session_id:index`. Batches sets via `tmux source -`. |
| `flake.nix` `update-icons-all-windows-tests` | `pkgs.runCommand` running this bats file; no `LANG`/`LC_ALL`; no `tmux -u`. Do not change unless investigation proves a locale/`-u` fix that is still darwin-safe. |

## Evidence / consumer map (EVIDENCE_REVIEW)

- **Invariant:** one `tmux-update-icons` pass on session A must exit 0 and stamp `@window_icon_padded` on every `list-windows -a` row, including a session whose name contains `\|`.
- **Production entry:** the bats file is the regression; `nix flake check` → `update-icons-all-windows-tests` is CI. No other consumer parses this test's discarded streams.
- **Red/green of the flake itself:** issue #653 could not reproduce locally (0/8 idle, 0/24 concurrent). Do not treat local green as proof the test is sound. The diagnostic change is verified by a deliberate non-zero child (or by reading that `run` + `echo "$output"` is on both invocations). A script-level root cause needs a quoted failing command; a retry needs the ruled-out list in the comment and PR `## Evidence`.

## Suspects (from the issue; resolve in Step 2)

1. **Transient tmux / last-command status.** The script has no `set -e`, so a mid-body `tmux` failure does not abort. Exit status is the last command of `main`. Unguarded `tmux set` (no `-q`, by design near `@remux_relaunch`) and `printf … | tmux source -` can still be non-zero. `lib-icons.sh` uses `((count++)) || true` because a zero arithmetic result is status 1; `tmux-update-icons.sh` has a bare `((icon_dw += 2))`. `CLAUDE_NOW` is pinned to a multiple of 5, so `arm_agent_detect` does not early-return on the `% 5` throttle. Test 2 creates `a|b` immediately before the invoke.
2. **#373 `\|`-delimiter / non-UTF-8 client.** No `tmux -u` anywhere in this repo. This suite unsets `TMUX` (so `CLIENT_UTF8` is not implied by a client env) and the nix check sandbox is the hostile locale. **However** `\|` is printable ASCII (0x7C); tmux's non-UTF-8 rewrite targets non-printables (tab/newline → `_`). The script already reads `#{session_id}\|#{session_name}` and keys by `$N:index`, which is the #580 fix. Pinning UTF-8 here would hide a tab-delimiter regression and break darwin. Treat `-u`/locale as a fix only if a format in *this* read path can be shown to mangle under `LC_ALL=C`.

---

## Step 1: Keep update-icons output on failure

- [ ] In `tests/update-icons-all-windows.bats`, replace **both** discarded invocations (test 1 line 91 and test 2 line 130):

```bash
run bash "$UPDATE_ICONS" A
[ "$status" -eq 0 ] || { echo "update-icons exited $status: $output"; false; }
```

Match surrounding style (tabs, `{ echo …; false; }` like the padded-length assertions). Do not restructure the suite. `run` is bats 1.5 (`bats_require_minimum_version 1.5.0` is already at the top).

## Step 2: Investigate the two suspects against the live script

- [ ] Confirm whether `main`'s last commands (`tmux source -`, the `sess_need_reflow` loop / `disown`, empty-`if` status) can yield non-zero when test 2's extra session exists and `@branch` seeding runs `git` + unguarded `tmux set-option -t "$target"`.
- [ ] Confirm the bats/`flake.nix` tmux calls do not pass `-u`; confirm `list-panes -a -F` in `tmux-update-icons.sh` is `|`-delimited (already). Optionally reproduce a `LC_ALL=C` invoke of the same format on a `|`-named session; if fields stay intact, **rule out** #373 as this flake's cause (record that in the PR).
- [ ] If a demonstrated mechanism exists (quoted command, observed non-zero, why test 2 / linux CI is special), fix it in `scripts/tmux-update-icons.sh` only as needed (e.g. `|| true` on a known-zero arithmetic, `tmux -u` on this script's tmux client invocations if #373 is actually in play, or a failed `tmux` that should not fail the 1s poller). Do not add `set -e`. Do not broaden error-swallowing on the no-`-q` stamp paths without evidence those are the CI exit.
- [ ] If neither suspect yields a demonstrated root cause: add a **bounded retry** (at most 2 extra attempts) around the test-2 `run bash "$UPDATE_ICONS" A` only, still printing `$output` on the final failure. Comment must name: (a) last-command/`set -e` findings, (b) #373 ruled out or not, (c) no sleep. Do not retry test 1 unless the same invoke is shared via a helper.

## Step 3: Gate

- [ ] From the worktree, with direnv/`nix develop` if needed: `nix build .#default`, then `nix flake check` (this bats file), then `nix build .#lint`.
- [ ] `shellcheck` on any edited `.sh` / the bats file if the project hook covers it (lint job includes shellcheck/shfmt).

## Step 4: Commit

- [ ] One commit, conventional message, body pointing at #653 (diagnostics + cause or justified retry). Do not commit `WORKER_TASK.md`.
