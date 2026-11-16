# Crew dash float launcher and float groups (#1139)

## Goal

1. `prefix + H` opens `crew dash` in a labelled floating pane, or focuses it when it is already open in the window. A dispatcher can call the same launcher after a dispatch.
2. Floating panes tagged `@float_group <name>` hide and show together with `prefix + G`, geometry preserved.

## Verified facts (scratch server, tmux next-3.9 = the flake pin a94699c3)

- `join-pane -s <float> -t <window>` tiles the float into that window; the pane keeps its pane options (`@float_geom`, `@pane_label`, `@float_group`).
- `break-pane -W -s <pane> -x W -y H -X X -Y Y` floats a tiled pane **in its current window** (`-t` does not move it). Float into another window = `join-pane` first, then `break-pane -W`.
- `break-pane -W` on a float errors "pane is already floating".
- `move-pane` of a float into another window also tiles it.
- The pin has no `hide-pane`/`show-pane`/minimise (`list-commands`); `prefix _` here is a split, not minimise. tmux/tmux#5402 is unmerged.
- `scratch-*` sessions are the repo's hidden-helper convention: reflow, status-fold and the pickers already skip them.
- Windows carry `@crew_id` (dispatcher-stamped); `crew dash` takes no tab flag, so it opens on its default (Overview) tab.

## Design (two options considered)

- **A. Stash window in the same session.** Shows up in the status bar and every window picker. Rejected.
- **B. Stash session `scratch-float-stash`** holding hidden panes as tiles. Invisible to existing skip logic. Chosen. Per-pane state lives in pane options so no side file: `@float_group_home` (origin window id), `@float_group_cells` (`W H X Y` live cells at hide time).
- Native `hide-pane`: not available in any tmux we can run, so no code path is written for it (unverifiable behaviour). The stash path is the only implementation; the PR body says so.
- Hide/show only act on **local** floats: a pane carrying `@bridge_pane` is daemon-owned and is skipped. A mirror window's launcher press still opens the dash locally (the bus is local) but passes no `-c`, because `#{pane_current_path}` there is the renderer's cwd.

## Steps

`run-shell` does not set `TMUX_PANE` and a bare `display -p` resolves tmux's "best" client, so **both scripts take the pressed pane id as an explicit argument**; `$TMUX_PANE` is only the fallback for a caller already inside a pane (the dispatcher).

1. `scripts/tmux-float-group.sh <hide|show|toggle> <group> [pane-id]` and `tag <group> [pane-id]`. The window is the pane's window. Refuses with `display-message` when `list-commands break-pane` lacks `-W`.
   - Stash = session `scratch-og-float-stash` (a name a user session is unlikely to have: `session-closed[98]` kills `scratch-<closed session>`, and `tmux-scratchpad` uses `scratch-<session>`). **One stash window per hidden pane**, so a pile of hidden floats never hits "pane too small". Each stash window is made with a bootstrap pane (`new-session -d` / `new-window -d`), the float is `join-pane -d`-ed in, the bootstrap pane is killed.
   - hide: every non-bridge (`@bridge_pane` empty) floating pane in the window with `@float_group == group`: stamp `@float_group_home` (window id) and `@float_group_cells` (`W H X Y` live cells), then move it. A `join-pane` failure leaves that float visible and reports via `display-message`; the others still move. Focus falls to a tiled pane.
   - show: every pane in the stash session with that group and `@float_group_home == window`: `join-pane -d` into the home window, `break-pane -W` using `@float_geom` (else saved cells), re-pin `remain-on-exit off`, clear home/cells, focus the last restored.
   - Orphans: at the start of hide/show, kill stashed panes whose `@float_group_home` window no longer exists (their window is gone, nothing could ever restore them).
   - toggle: visible tagged floats in the window → hide, else show.
2. `scripts/tmux-crew-dash.sh [--detached] [pane-id]`. Reuse the float labelled `crew-dash` in the pane's window (focus unless `--detached`); stashed for this window → `tmux-float-group show crew <pane>` then focus; else `new-pane` `90% 90% 5% 5%` (`-A` only when `list-commands new-pane` lists it; `-c <pane_current_path>` unless the window carries `@bridge_win`, where it is the renderer's cwd), running `exec crew dash`, stamping `@float_geom`, `remain-on-exit off`, `@pane_label crew-dash`, `@float_group crew`. No `CREW_ID` is passed: nothing shows `crew dash` reads it (its bus comes from the cwd's repo). `crew` missing → `display-message`, exit 0. It calls the sibling by name (`tmux-float-group`), resolved through the wrapper's PATH.
3. Wiring (the same two lines in both files, two-file rule):
   - tmpl: `bind -N 'Open crew dash' H run-shell '{{index .Paths.Scripts "tmux-crew-dash"}} #{q:pane_id}'` and `bind -N 'Toggle floating pane group' G run-shell '{{index .Paths.Scripts "tmux-float-group"}} toggle crew #{q:pane_id}'`
   - reference.nix: the same with `${script.tmux-crew-dash}/bin/tmux-crew-dash` and `${script.tmux-float-group}/bin/tmux-float-group`.
   - `config/tmux.conf.nix` script build list and `ogInternal` (bind-driven; the dispatcher reaches the launcher through the server PATH, so no `og` verb), `generator/paths/paths.go` `RequiredScripts`, and `flake.nix` check derivations next to `float-nudge-tests`.
4. Tests (real tmux on a private socket per CLAUDE.md; `crew` stubbed on PATH; a shim dir on PATH execs `scripts/tmux-float-group.sh` for the launcher's sibling call):
   - `tests/float-group.bats`: hide→show keeps float flag and `pane_width/height/left/top`; untagged and `@bridge_pane` floats untouched; invoked with `TMUX_PANE` unset and a pane id from another window acts on that pane's window; second window's floats unaffected; orphaned stash panes reaped; refusal via a tmux shim that hides `-W`.
   - `tests/crew-dash-launch.bats`: first press creates exactly one labelled float with the stamps; second press focuses (pane count unchanged); press after hide restores instead of duplicating; `--detached` keeps focus on the caller; a `@bridge_win` window gets no `-c`.
   - Existing: `conf-shell-quoting.bats` and the extraction check cover the two bind lines.
5. Docs: `docs/agents/floats.md` (launcher, groups, stash session, the `@float_geom` "written only at creation" sentence reworded since the launcher is a second writer, native `hide-pane` not present in the pin), `docs/agents/scripts.md` rows for both scripts. The README has no float key table, so no README edit.

## Out of scope

Dispatcher-side one-line call (separate repo, noted in the PR); tagging the enrich card/carousel by default; native `hide-pane`.

## Acceptance

- [ ] `prefix + H` opens, pressing again focuses (no duplicate).
- [ ] `prefix + G` hides and restores all tagged floats with geometry.
- [ ] Runs on the pin; refuses clearly where `break-pane -W` is absent.
