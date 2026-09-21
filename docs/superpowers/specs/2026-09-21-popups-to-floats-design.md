# Popups → floating panes after upstream `34cd5da4` (#725)

## Problem

Upstream tmux `34cd5da4` ("Remove popups and all the associated overlay
machinery", 2026-09-21) deletes popups. `display-popup` survives only as an
undocumented compatibility command that opens a **modal floating pane** in the
target window. The `popup-style`, `popup-border-style` and `popup-border-lines`
options are gone, as are `display-popup -N` and every `popup_*` format.

Our pin (`81794f30`) predates it. Bumping past it breaks config load today —
measured on a scratch server with the wrapped tmux built at `3a6c2e78`:

```
tmux.conf:6: invalid option: popup-border-lines
'…/catppuccin/catppuccin.tmux' returned 1        # catppuccin_tmux.conf sets popup-style / popup-border-style
```

`rg` over `config/ scripts/ picker/ generator/ modules/` finds no `display-popup
-N` and no `popup_*` format. `better-mouse-mode` and `vim-tmux-navigator` set no
popup option. There is no pinned which-key plugin any more (#629 replaced it
with `picker/whichkey.go`).

## Callers

| Caller | Launch |
|---|---|
| `prefix + s` / status-left click | `tmux-session-picker` → `display-popup -c <client> -E -w 90% -h 85%\|60% -b rounded -T " Sessions " -S fg=…` |
| `prefix + w` / `a` | `tmux-window-picker` → same shape |
| `prefix + W` | `tmux-window-wall` → `-w 100% -h 100% -b rounded -T " Wall "` |
| `prefix + ?` | `tmux-which-key` → same shape as the session picker |
| `prefix + S` | `tmux-scratchpad` → `-w 80% -h 80% -b rounded -T <hints>`, running a nested `tmux new-session -A` |
| attach hook | `tmux-splash-maybe` → `-E -B -w 100% -h 100% -t <session> -c <client>` |
| `prefix + C-Space` | bind → `display-popup -E -B -w 100% -h 100% tmux-splash --no-timeout` |
| `prefix + n` | bind → `display-popup -E -w 80% -h 60% og-notify-center` |

`og-remote-auth` is not a caller: it runs inside the picker's own pane via
`tea.ExecProcess`. The enrich card (`prefix + i`) and the tool floats already
use `new-pane`.

## Decision: keep `display-popup` (the compat command) as the launch primitive

Two designs were sketched.

**A — migrate every caller to native `new-pane -O -K`.** Each launcher would
have to re-derive what the compat command does internally
(`cmd-display-menu.c` at the pin):

- **Window choice.** The compat float lands in the `-t` window; `-c` only
  picks the client it is sized and positioned for. With no `-t`, tmux takes its
  best session: measured, with a newer session `zz` present,
  `run-shell -t s: "tmux display-popup -c $C -E …"` put the float in `zz`,
  where nobody could see it. With `-t "$C:"` (a client name is accepted as a
  session target, so this means the client's current window) it landed in the
  client's window. The script launchers therefore pass `-t "<client>:"` next to
  `-c`; the two config binds run in the key's own context and need neither.
  `new-pane` would need the same explicit target, plus the #346-class guard the
  compat command already applies (`tc->flags & CLIENT_CONTROL` → silent no-op).
  Without that guard a control-mode client would put a modal float in a window
  the bridge mirrors.
- **Title.** The compat command sets per-pane `pane-border-status top` +
  `pane-border-format '#{pane_title}'`. `new-pane -T` alone renders through our
  global `pane-border-format` (measured: `╭──━━ ● ━━──`, not the title).
- **Close-on-exit.** The compat command pins `remain-on-exit` per pane (`-E` →
  `0`). A native float in a mirror window inherits `remain-on-exit on` (#547),
  so every launcher would need the `@float_geom`/`remain-on-exit off` stamp.
- **Stacking.** A second `new-pane -O` in a window with a modal fails loudly
  (`window already has a modal pane`, rc 1). The compat command is a silent
  no-op, which is the behaviour `picker/whichkey.go`'s replay delay is built
  around.
- **Resident-server skew (#407).** A resident server predating `34cd5da4` runs
  a real popup for the same `display-popup` argv. Design A would need no gate
  there (`new-pane -O -K` predates it too), but it throws away a command that
  works identically across the skew.

Design A's only gains are documentation status, `-B rounded` (the compat
command maps `rounded` to `single`), and refit-on-grow via `@float_geom`.

**B — keep `display-popup`, fix what the removal actually broke.** It is chosen
because every popup semantic we depend on is reproduced by the compat command,
measured on the new pin:

| Property | Result on `3a6c2e78` |
|---|---|
| Opens a floating, modal, all-keys pane in the `-t "<client>:"` window | ✔ `pane_floating_flag=1 pane_modal_flag=1`, active |
| `-E` closes on exit; `-B` borderless | ✔ pane gone on exit; splash `pane-border-lines=none` |
| Closes cleanly in a `remain-on-exit on` window (mirror shape) | ✔ `remain-on-exit=off` on the float; no dead pane left |
| Focus + last-pane history restored on close | ✔ `%0` active again, `%4` still `last` |
| Second popup while one is open | ✔ silent no-op (rc 0, returns immediately, no pane) — which-key's rule still holds |
| Control-mode client | ✔ refused (existing `tmux-next38-readiness.bats` test) |
| Blocks the caller until close | ✔ same as a popup |
| which-key replay of a popup bind (`prefix + ?` → "notification history") | ✔ 5/5 with the existing `whichKeyReplayDelay` |
| Window options not polluted by the float | ✔ `@branch`/`@git_root` unchanged |
| `tmux-reap-pane` on the float's `pane-exited` | no-op: no state file exists for the float's pane id |
| `tmux-float-refit` | skips it: no `@float_geom`, same as a mouse-dragged float; upstream's clamp keeps it on screen |

What neither design avoids is that a modal float **is the window's active
pane** while it is open ("A modal pane is always the active pane", tmux(1)).
A popup never was. That is the real migration cost, and it is handled below
(§ Modal floats are chrome) regardless of A or B.

The risk accepted is that the compat command is undocumented. It is pinned, so a
later bump that drops it cannot land silently: the new live test below runs the
launchers against the pinned wrapper.

## What changes

1. **Pin.** `tmux-upstream` → `3a6c2e78` (portable `tmux/tmux` merge of
   `21b3da3b`, newest at time of writing; includes `166851bf`, `dda0e4d4`,
   `9be3a351`, `e361b8f8`). Upstream is still churning around floats, so this
   is "newest that builds and passes the gate", said so in the PR.
2. **Drop `patches/tmux-client-control-discard.patch`.** `3d5f946f` is in the
   new pin (`tmux.h`: `CLIENT_CONTROL_DISCARD 0x10000000000ULL`).
3. **Delete `set -g popup-border-lines rounded`** from `config/tmux.conf.tmpl`
   and `config/tmux.conf.reference.nix` (the extraction oracle — edited
   together). Every launcher that wants a border already passes `-b`; on a
   resident pre-removal server the one bind without `-b` (`prefix + n`) falls
   back to single lines until restart. Accepted.
4. **Catppuccin.** Both pinned copies (`config/tmux.conf.nix`,
   `config/tmux.conf.reference.nix`) get the same `postPatch` turning
   `set -gF popup-style` / `set -gF popup-border-style` into `set -gqF`. `-q`
   keeps the theme on a resident pre-removal server and is silent on the new
   one. Identical patches keep the two store paths identical, so the
   extraction check still diffs them as one.
5. **Pin the window.** The five script launchers add `-t "<client>:"` to their
   existing `-c <client>` (see Window choice). Without it the scratchpad
   float opened *inside* the freshly created scratch session, and the nested
   client then displayed a window containing itself.
5b. **Scratchpad nested attach.** A float is a real pane, so the nested client's
   tty now matches a server pane and tmux refuses it: measured inside the float,
   `sessions should be nested with care, unset $TMUX to force`, rc 1 — the
   scratchpad never opens. Inner mode runs
   `env -u TMUX tmux -S "${TMUX%%,*}" new-session -A -s "$SCRATCH"`: the socket
   comes from `$TMUX`'s first field, so the nested client still reaches the same
   server. It also works under a real popup on an older server.
6. **Modal floats are chrome** — see the next section.
7. **Comments/docs that describe popup mechanics** — `picker/whichkey.go`'s
   replay-delay rule (now "a window holds one modal pane; the compat
   `display-popup` no-ops while one is open"), the launcher comments, and
   `docs/agents/{floats,picker,splash,bridge-daemon,scripts}.md`.

## Modal floats are chrome

The rule: **a pane with `pane_modal_flag=1` is transient chrome, never window
content.** Consumers that enumerate panes skip it. Consumers that want "the
window's active pane" read the pane under it, which is the window's last pane
while a modal is open:

```
#{?window_modal_pane,#{pane_last},#{pane_active}}           # per-pane "effectively active"
#{?window_modal_pane,#{P:#{?pane_last,#{X},}},#{X}}         # field X of the pane under the modal
```

Enumerators skip with `list-panes -f '#{!:#{pane_modal_flag}}'`, which leaves
every positional `-F` format's field count untouched. Measured on the pin: in a one-pane window (`%9` + popup `%36`) the expression
marks `%9`; in a two-pane window (`%0` active before, `%4`, popup `%37`) it marks
`%0`. A resident server that predates modal panes expands the unknown
`window_modal_pane` to empty and falls back to `pane_active` — today's
behaviour, so no gate is needed. The rule covers the `prefix + s ^o` remote-picker
float too (also modal, also chrome); non-modal tool floats (lazygit, prdash,
yazi, the enrich card) stay window content as today.

Every consumer, walked:

| Consumer | Reads | With a modal float open | Action |
|---|---|---|---|
| `tmux-update-icons` process icons | every pane's command | the picker/splash/scratch client joins the window's icon list, forcing a reflow on open and again on close | skip modal rows |
| `tmux-update-icons` active pane | `pane_active` → `@window_task`, `@window_ai_name`, session `@active_pane_icon` | an agent window's label drops to its fallback while any picker is open (the task/name files are keyed by the float's id, which has none) | effective-active |
| `tmux-update-icons` `win_cwd` | first non-floating pane | already skips floats | none |
| `tmux-statusline` volatile fields | session's active pane: `pane_current_path`, `pane_current_command`, `@bridge_proc` | measured: a `/tmp` window's line 0 showed the worktree branch/dir and `tmux-picker-gen…` while the picker was open | read the pane under the modal |
| picker collectors (`collectPanesSnapshot` → sessions/paneMap, `collectWindows`) | every pane | the picker's own process is listed under the current session/window | skip modal rows |
| picker preview + wall (`capture-pane -t sess[:idx]`) | the target's active pane | measured: the wall tile for the current window showed the wall itself, not the window's marker text | the picker never captures itself: its own session and window targets resolve, once per picker, to the pane under its float (`display -p -t $TMUX_PANE` with the `pane_last` loop). A modal in some *other* window, such as another client's picker, is what that window really shows, so capturing it is faithful |
| agent scans (`agentdetect`, statusline `usage.go`) | agent processes | the picker is not an agent | none |
| `tmux-reflow-windows` | windows, not panes | none | none |
| `tmux-shell-prompt` | OSC 133 from a pane | no launched float runs a prompting shell | none |
| enrich card | window options; itself a non-modal `new-pane` float | none | none |
| `claude-status-update` `window_stamp` | `#{pane_active}` of the reporting agent pane | the stamp is skipped while a modal is open; on a bridge-only host (#589) nothing repairs it until the next self-report | effective-active |
| `tmux-worktree-match` | every pane's `pane_active` + `pane_current_path` | a `wt switch` run inside the scratchpad float contributes an active modal row with the server's start dir | skip modal rows |
| `tmux-issue-stamp` | window options | none | none |
| tmux-remux | pane snapshot | its snapshot skips every floating pane (`internal/snapshot/build.go`) | none |
| pane-border indicator (`tmux-apply-theme-colors`, `window_panes>1` + `pane_active`) | pane count | a one-pane window draws the inactive border segment under an open picker | accepted: cosmetic, transient |
| `tmux-splash-maybe` gate (1 window / 1 pane) | pane count | a modal opened by another client suppresses the splash for this attach; the next attach retries | accepted |

**Killing the picker's own window or session.** The picker now lives inside the
current window. Killing the current window from the window picker (or the
current session from the session picker) takes the picker with it, where the
popup used to survive and refresh its list. The action still completes, and the
end state is the one a picker that closes after acting leaves behind. Accepted
and documented, not worked around: switching away first would leave the float
behind in the dying window.

## Bridge

- **Local popup-float in a mirror window.** It is a user float to the daemon:
  `parseLocalPaneList` keeps floats out of the tiled set, `removeFloat` only
  kills floats in `w.localFloats`, and the compat command's per-pane
  `remain-on-exit 0` overrides the mirror's `on` (measured above). While open,
  its modal flag refuses the daemon's `focusLocalPane` `select-pane`, exactly
  as the `prefix + s ^o` float already does (documented in `floats.md`).
- **Remote host on the new tmux.** Its popups are now modal floats and appear
  in `%layout-change`. None can come from the bridge itself: the compat command
  refuses control clients, `tmux-splash-maybe` skips them, and no bridge verb
  runs a remote popup. A float appears only when a human tty client on the
  remote opens one, and mirroring it is faithful: it is a real pane in that
  window, and the v2 layout carries no modal marker to filter on. Not filtered;
  documented.

## Acceptance

- Generated conf sources with no error on the pinned wrapper — asserted by a new
  live bats file (`tests/popup-float.bats`, wired as a `nix flake check`
  derivation like `float-tool-focus-tests`), which also presses the real binds
  through an attached client: `prefix + n` opens a modal float in the client's
  window and closes back to the previous pane;
  a second `display-popup` while one is open adds no pane; `prefix + S` reaches
  the scratch session.
- No `popup-*` option, `display-popup -N`, or `popup_*` format that the new
  binary rejects.
- The chrome rule is pinned by tests: `tests/icons.bats` (a modal row is
  skipped and effective-active drives `@window_task`/`@active_pane_icon`), Go
  tests for the picker collectors and capture-target resolution, and a
  statusline test for the volatile-field format.
- Existing suites updated where they pin launcher argv (`picker-launcher.bats`,
  `splash.bats`, `notify-conf-assertions`).
- `nix build .#default`, `nix flake check`, `nix build .#lint` green.

## Out of scope

- Moving launchers to native `new-pane` (design A). Revisit if upstream drops
  the compat command.
- Filtering remote modal floats in the daemon.
- The stale `tmuxPackage` option description in `modules/home-manager.nix`
  (already wrong before this change: it names tmux 3.6a).
