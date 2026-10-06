# Floating Panes

- **Popups are modal floats (#725).** Upstream `34cd5da4` removes popups;
  `display-popup` survives only as an undocumented compat command that opens a
  **modal floating pane** in the target window instead — `new-pane -O` under
  the hood, always `-K` (all-keys, so the prefix key itself reaches the float
  — the compat command has no `-N`/`-K` flag of its own), `remain-on-exit`
  pinned from `-E`, and `pane-border-status top` +
  `pane-border-format '#{pane_title}'` for the title. The target window comes
  from `-t`, which defaults to tmux's "best" session, not the `-c` client's —
  launchers pass `-t "<client>:"` alongside `-c` so the float lands in the
  client's own window (measured: without it, a newer session stole the float
  invisibly). A window holds at most
  one modal pane; a second `display-popup` while one is open is a silent
  no-op (rc 0, no pane) — `picker/whichkey.go`'s `whichKeyReplayDelay` is
  built around that. A **dead** modal pane (a no-`-E` popup whose command
  already exited) still counts: `w->modal` is cleared only when the pane is
  removed, so every later `display-popup` in that window stays a no-op until
  the dead pane is dismissed or `display-popup -C` runs (#738).
  **The chrome rule:** a pane with `pane_modal_flag=1` is transient chrome,
  never window content.
  ```
  EFF_ACTIVE  #{?window_modal_pane,#{pane_last},#{pane_active}}        # per-pane "effectively active"
  NOT_MODAL   #{!:#{pane_modal_flag}}                                  # a list-panes -f filter
  UNDER(X)    #{?window_modal_pane,#{P:#{?pane_last,X,}},X}            # field X of the pane under the modal
  ```
  A resident server that predates modal panes expands the unknown
  `window_modal_pane` to empty and falls back to `pane_active`/no filter —
  today's behaviour, so no gate is needed. Consumers, each pinned by its own
  test rather than one shared pin: `tmux-update-icons` (`NOT_MODAL` on its
  `list-panes -a` loop, `EFF_ACTIVE` for
  `@window_task`/`@window_ai_name`/session `@active_pane_icon`) →
  `tests/update-icons-modal.bats`; the picker's collectors (`NOT_MODAL`, one
  `notModalFilter` const) → `TestCollectorArgvCarriesNotModalFilter`
  (`picker/main_test.go`); `tmux-statusline`'s volatile fields (`UNDER(X)` for
  `pane_current_path`/`pane_current_command`/`@bridge_proc`) →
  `TestUnderModal`/`TestVolatileFieldsModalSwap`
  (`picker/statusline/main_test.go`); the picker's self-capture (below) →
  `TestSelfTargets`/`TestCaptureViaSelf` (`picker/capture_test.go`); the raw
  `EFF_ACTIVE`/`NOT_MODAL`/`UNDER(X)` expressions on the pinned binary →
  `tests/popup-float.bats` test 5 ("chrome rule at the tmux layer").
  `tmux-worktree-match` (`NOT_MODAL` + `EFF_ACTIVE`) and
  `claude-status-update`'s `window_stamp` (`EFF_ACTIVE`) carry the same
  expressions verbatim but are unpinned. `tmux-grid-refit` is float-aware too,
  but via `#{!:#{pane_floating_flag}}` — **all** floats, not the modal
  `NOT_MODAL` — because `window-layout-changed` fires on any float open/close
  and a non-modal float would otherwise churn its `@grid_refit_sig` (#760). The
  two filters are deliberately different, not drift: the chrome rule is about
  transient *modal* panes, this one is about the tiled set `select-layout`
  operates on. The picker additionally never
  captures its own float, and only when the picker's own window holds a
  modal: `selfCaptureTarget` maps its own session/window targets to the pane
  under its float before any preview or wall `capture-pane`, so the tile for
  the picker's own window shows the window, not the picker — a picker run in
  a plain (non-float) pane gets no redirect, by the same `window_modal_pane`
  gate. Accepted leftovers, not worked around: a one-pane window still draws the inactive
  pane-border segment while a modal is open (cosmetic); the status-format
  segment for the launching command *is* the float while it's open; killing
  the picker's own window/session from inside the picker takes the picker
  with it, the same end state a picker that closes after acting leaves; the
  session picker's launcher (and window picker's launcher) treat that death as a
  clean exit — the destroyed float makes `display-popup` exit 129 (SIGHUP), and
  the launcher exits 0 for it only while the popup's host window is actually
  gone, so a genuine picker failure still surfaces (#884, #887). A
  popup-float carries no `@float_geom`, so `tmux-float-refit` skips it (same
  as a mouse-dragged float; upstream's own clamp keeps it on screen across a
  resize) — `float-conf-assertions` matches `^bind(-key)? .*new-pane` only, so
  a `display-popup` bind is exempt from the `@float_geom` stamp it enforces.
  The popup-float's `remain-on-exit` is off only because every caller passes
  `-E`; the compat command sets it to on (1) without `-E` — keep `-E` on
  every `display-popup` call: without it the pane lingers dead, and on a
  normal client Escape/`C-c` closes it (`PANE_CLOSEONCANCEL`), while inside a
  mirror window the bridge daemon's own `display-popup -C` clear on a lone
  Escape/`C-c` does the same (#738).
- **`resize-pane -y` on a float can add a row.** Under `pane-border-status top` it adds one when the pane's `pane_top` is 1; under `bottom`, when the pane's bottom edge sits one above the window's last row (`cmd-resize-pane.c`; bordered and `-B none` floats alike; `top-floating`/`bottom-floating` count as off). `new-pane -y` is exact. The bridge daemon compensates with an `if-shell -F` branch that asks for one row less on those rows (`floatResizeCmd`, `bridge-daemon.md`), because `resize-pane` does not format-expand its `-y`.
- **`@og_bridge_drag`** is a session-scoped local pane id. It is written only by the mirror branch of the stock drag binds (`MouseDrag1Border` for any mirror pane, `M-MouseDrag1Border`/`M-MouseDrag1Pane` for a mirror float only; `generator/render/stockdrags.txt`, `generator/render/drags.go`) in the same command list that enters the one-shot `og-bridge-drag` key table, and read only by that table's drag-end binds. They hand it to `og-remote-bridge-ctl drag`, which looks the pane up by id — the drag end's own format target is wherever the button was released — and routes a float's drag as `float-geom` (#797) and a tiled divider drag as `tile-layout` (#823; `bridge-daemon.md`). Only a daemon-owned pane (`@bridge_pane`) takes that branch: a user float in a mirror window and every non-mirror pane keep tmux's stock drag. The which-key picker skips the `og-bridge-drag` table.
- **`@float_refit_size`** — pane-scoped `WxH`, written by `tmux-float-refit` just before it refits that float, and by `tmux-float-nudge` after a hand resize marks the float current for the window; the `window-resized` hook gate skips a float already refit at the window's current size (#793, `performance.md`), and `tmux-float-refit` itself skips such a float per pane — so a hand resize on one float survives a fork another stale float triggered. Never set it at window or global level: every new float would inherit a matching stamp and miss its first refit. Its window-scoped sibling `@float_refit_pending` (`WxH`) is written only by that hook, right before it forks, and cleared only by `tmux-float-refit`'s first tmux call; it coalesces a resize burst into one run (#810).
- **`@og_float_target_<tool>`** — window-scoped pane id, written by a bridged tool bind's focus branch and read by the `run-shell` in the very same command list, so it can never be stale and is not a second lookup key (#679). It exists because a pane loop nested inside a shell string is the injection shape `tests/conf-shell-quoting.bats` rejects, and `#{q:<option>}` is the one legal way to hand an id to a shell. `prefix + p`/`g`/`y` **reuse** the window's float for that tool — found by a `#{P:...}` loop over `@pane_label` — instead of stacking another at the same geometry: **`new-pane -A` is a z-order flag** (the float stays visible above a zoomed pane), not attach-if-exists, which is why every press used to add a pane. The remote leg (`ctl.go`'s `tool` verb) builds the same gate, now stamps `@pane_label` on the remote float (which it previously did not), and carries an explicit `-t` on **every** command in its branch — `if-shell -t` pins the *condition's* context but not the branch's, and the remote's current window is not the window the press came from. The local leg needs none: the pressed window is the current one, so a branch `-t` there would only be noise. A window that already held floats for a tool before this change is a one-time edge, and the two legs differ: the local bind has always stamped `@pane_label`, so a pre-fix local stack makes the loop match more than one pane and the press reports a bad target instead of focusing (close the extras with `prefix + x`), while remote floats created before this change carry no label at all — so there the first press after the update adds one more labelled float and every press after that reuses it. New stacks cannot form on either leg. `prefix + i` is not bridged — the enrich card reads only local window options — so it is the one float bind that can still stack.
- **`@float_geom`** — pane-scoped `<width> <height> <xoff> <yoff>`, the four values the float was created with (percentages, or cells for the fixed-size enrich card). Written only at creation, by the same `mkFloat` attrset (`generator/render/keys.go`) that emits the `new-pane` flags, so the two cannot drift; `picker/remotepick.go` carries its own copy for the `prefix + s` `^o` float, which is also the one float that carries `-O -K -C` (modal, all-keys, close-on-click-outside) natively in place of the retired `@pane_keys_raw` (#648) — `-K` alone doesn't block a pane-switch attempt away from the float, only `-O` does; `-C`'s click-outside-close is the mouse-based escape `-K`'s prefix capture would otherwise remove for a stuck float (see `og-remote-picker`'s documented hang, #486). While that float is open, the bridge daemon's own `focusLocalPane`-issued `select-pane` for its window (see `focusLocalPane` in `picker/remotebridge/daemon/reconcile.go` for the actual mechanics) is refused by `-O` like any other pane-switch attempt — logged (daemon stderr) and otherwise silent, and it resolves on the next remote focus change to a different pane or the next local focus gesture, not instantly. `-C` also means a stray click outside the float now closes it mid-pick, where it used to survive; `set -g mouse on` (unconditional in this config) is what makes that escape reachable at all. Every float bind must stamp `@float_geom` — `float-conf-assertions` (in `nix flake check`) fails the build otherwise, since an unstamped bind looks correct until the client resizes. The same stamp pins `remain-on-exit off` on the pane, asserted by the same check: a mirror window carries `remain-on-exit on` so a dying renderer leaves a corpse rather than taking the session (#547), pane options inherit from the window's, and nothing reaps a float's corpse — `healDeadRenderers` keys on `@bridge_pane`, and a float the daemon never created is the user's (#587).
- **A float resize (keyboard or mouse) rewrites `@float_geom` (#864).** `tmux-float-nudge` (`<pane-id> <L|R|U|D> [step]`, or `<pane-id> stamp`) is the M-arrow bind's float branch and the `og-float-drag` table's drag-end handler. Upstream `resize-pane` on a float only ever **grows** it — `-L`/`-U` pick which edge moves, they do not invert the size change, and nothing clamps, so holding M-Left walks the float off the window — so the nudge grows by `step` toward the pressed direction while `step` cells of room remain on that side, else shrinks from that edge by `step`, else no-ops. It then rewrites `@float_geom` preserving each field's own unit: a `%` field becomes `(inner+2)*100/window` for a size or `(inner-1)*100/window` for an offset (the refit's measured `-2`/`+1` border arithmetic and tmux's integer `args_string_percentage`), a cell field stays cells (`inner+2` / `inner-1`), and a negative offset — a float dragged off screen, which `strtonum`'s `0..1000` rejects as a `%` — falls back to cells. The `stamp` write marks `@float_refit_size` for the current window, so the user's size survives a later `tmux-float-refit` exactly. A mouse drag fires no hook while it runs, so the drag-start binds enter a one-shot `og-float-drag` table (stashing the pane id in `@og_float_drag`) whose `MouseDragEnd1<loc>` rows hand it to the nudge; the mirror `og-bridge-drag` table and the tiled M-arrow `resize-pane` are untouched (the tiled path forks nothing). An unstamped float is never resized or stamped — it stays the user's, upstream's own clamp keeps it on screen, and the refit skips it.
- **`Ctrl-h/j/k/l` uses float-aware geometry while a window has floats.** `tmux-smart-nav` receives the origin pane and window plus a `#{P:...}` float-presence probe. With no floats it keeps the inexpensive native `select-pane`/kitty-edge path. Otherwise it reads one `list-panes` geometry snapshot, ignores modal panes, considers both tiled and floating candidates by center direction, prefers perpendicular overlap (then the nearest perpendicular candidate), ranks by the zero-clamped near-edge gap, and breaks an exact tie toward a float from a tiled origin or a tile from a float origin. This lets a tiled pane enter an overlapping float and a float leave through the appropriate tiled pane; `pane_at_*` alone reports `0` for floats and cannot provide that behavior.
