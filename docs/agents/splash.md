# Welcome Buffer (splash)

`tmux-splash` (a bubbletea binary, second main package in the `picker/` Go
module) renders a sleeping-cat mascot as an animated braille frame deck —
breathing + drifting sleep `z`s baked into the source — with a dissolve-in
intro (braille static settling into the art) and a plasma-field shimmer (summed
sines drive both gradient color and per-cell brightness), plus a keybind
cheatsheet — shown once per tmux server via `display-popup`, the compat
command that opens a modal floating pane rather than an actual popup (#725,
`floats.md`). Enabled by default through `programs.tmux-og.splash.enable`.

- **Art:** `assets/frames.txt` is the loop (one braille frame per form-feed
  `\f`, uniform height) and `cat-small.txt` is a single static frame for small
  viewports — both embedded via `//go:embed`. Regenerated from
  `assets/cat-source.mp4` (a seamless sleeping-cat clip) by `assets/catgen.py`
  (ffmpeg negate + hard threshold → chafa braille `--dither none`; U+2800 blanks
  folded to spaces). The deck plays ~real-time (`deckStep` ticks/frame); the
  renderer recolors every glyph, so the source is shape-only.
- **Trigger:** indexed `client-attached[50]` / `client-session-changed[50]`
  hooks fire `tmux-splash-maybe`, passing `#{hook_session_name}` (the id form
  `#{hook_session}` gets re-expanded by `run-shell`'s own shell) and
  `#{hook_client}`, which gates on the global `@splash_shown` (once per
  server) + 1-window/1-pane + `pane_current_command` being a shell, and now
  also skips control-mode clients (the remote bridge's `-CC` attach) without
  setting `@splash_shown`; the popup itself is pinned to the attaching client
  with `-c`.
- **Remote (ssh) attach:** `programs.tmux-og.splash.remote` (`full` default,
  `static`, or `skip`) controls what `tmux-splash-maybe` does when the client
  that attached to the session came in over ssh. Detected via
  `#{I/e:SSH_CONNECTION}`, tmux's per-client environment interrogation
  (`format.c`'s `I` modifier reading `ft->c->environ` for the *attaching*
  client named by `-c`), which is strictly more correct than the old
  session-table read since it can no longer be confused by whichever client
  most recently attached to the session. `static` launches
  `tmux-splash --static` (forces the existing single small-frame fallback,
  with no dissolve-in and no periodic redraw — the bandwidth-light path);
  `skip` opens nothing for that attach and leaves `@splash_shown` unset, so a
  later local attach on the same tmux server still gets the splash. Doesn't
  affect the on-demand `prefix + C-Space` bind, which always shows the full
  splash.
- **On demand:** `prefix + C-Space` pops the splash directly (bypasses the gate),
  launched with `--no-timeout` so it dismisses on keypress only.
- **Cheatsheet:** `splash.tips` (list of `{ key; label; }`) is codegen'd into the
  binary at build time (`picker/splash/tips_generated.go`, like the picker's
  `icons_generated.go`); the `prefix` token is substituted at render time.
- **Dismiss:** any key or `splash.timeout` seconds (default 10).

