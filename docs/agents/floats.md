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
  expressions verbatim but are unpinned. The picker additionally never
  captures its own float, and only when the picker's own window holds a
  modal: `selfCaptureTarget` maps its own session/window targets to the pane
  under its float before any preview or wall `capture-pane`, so the tile for
  the picker's own window shows the window, not the picker — a picker run in
  a plain (non-float) pane gets no redirect, by the same `window_modal_pane`
  gate. Accepted leftovers, not worked around: a one-pane window still draws the inactive
  pane-border segment while a modal is open (cosmetic); the status-format
  segment for the launching command *is* the float while it's open; killing
  the picker's own window/session from inside the picker takes the picker
  with it, the same end state a picker that closes after acting leaves. A
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
- **`@og_float_target_<tool>`** — window-scoped pane id, written by a bridged tool bind's focus branch and read by the `run-shell` in the very same command list, so it can never be stale and is not a second lookup key (#679). It exists because a pane loop nested inside a shell string is the injection shape `tests/conf-shell-quoting.bats` rejects, and `#{q:<option>}` is the one legal way to hand an id to a shell. `prefix + p`/`g`/`y` **reuse** the window's float for that tool — found by a `#{P:...}` loop over `@pane_label` — instead of stacking another at the same geometry: **`new-pane -A` is a z-order flag** (the float stays visible above a zoomed pane), not attach-if-exists, which is why every press used to add a pane. The remote leg (`ctl.go`'s `tool` verb) builds the same gate, now stamps `@pane_label` on the remote float (which it previously did not), and carries an explicit `-t` on **every** command in its branch — `if-shell -t` pins the *condition's* context but not the branch's, and the remote's current window is not the window the press came from. The local leg needs none: the pressed window is the current one, so a branch `-t` there would only be noise. A window that already held floats for a tool before this change is a one-time edge, and the two legs differ: the local bind has always stamped `@pane_label`, so a pre-fix local stack makes the loop match more than one pane and the press reports a bad target instead of focusing (close the extras with `prefix + x`), while remote floats created before this change carry no label at all — so there the first press after the update adds one more labelled float and every press after that reuses it. New stacks cannot form on either leg. `prefix + b`/`k`/`i` are not bridged and still stack.
- **`@float_geom`** — pane-scoped `<width> <height> <xoff> <yoff>`, the four values the float was created with (percentages, or cells for the fixed-size enrich card). Written only at creation, by the same `mkFloat` attrset (`generator/render/keys.go`) that emits the `new-pane` flags, so the two cannot drift; `picker/remotepick.go` carries its own copy for the `prefix + s` `^o` float, which is also the one float that carries `-O -K -C` (modal, all-keys, close-on-click-outside) natively in place of the retired `@pane_keys_raw` (#648) — `-K` alone doesn't block a pane-switch attempt away from the float, only `-O` does; `-C`'s click-outside-close is the mouse-based escape `-K`'s prefix capture would otherwise remove for a stuck float (see `og-remote-picker`'s documented hang, #486). While that float is open, the bridge daemon's own `focusLocalPane`-issued `select-pane` for its window (see `focusLocalPane` in `picker/remotebridge/daemon/reconcile.go` for the actual mechanics) is refused by `-O` like any other pane-switch attempt — logged (daemon stderr) and otherwise silent, and it resolves on the next remote focus change to a different pane or the next local focus gesture, not instantly. `-C` also means a stray click outside the float now closes it mid-pick, where it used to survive; `set -g mouse on` (unconditional in this config) is what makes that escape reachable at all. Every float bind must stamp `@float_geom` — `float-conf-assertions` (in `nix flake check`) fails the build otherwise, since an unstamped bind looks correct until the client resizes. The same stamp pins `remain-on-exit off` on the pane, asserted by the same check: a mirror window carries `remain-on-exit on` so a dying renderer leaves a corpse rather than taking the session (#547), pane options inherit from the window's, and nothing reaps a float's corpse — `healDeadRenderers` keys on `@bridge_pane`, and a float the daemon never created is the user's (#587).
