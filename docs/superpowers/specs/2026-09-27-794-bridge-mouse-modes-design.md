# Bridge: mouse input into mirrored panes and floats (#794)

> Folded into #757 and implemented there. The shipped representation is
> `render.MouseMode{Standard, Button, All, SGR, UTF8 bool}`, and `Seed` clears
> all five modes before setting the true ones rather than emitting a minimal
> set/reset; see `docs/superpowers/plans/2026-09-24-mirror-mouse-wheel-scrollback.md`.

## Problem

"Mouse isn't propagating into the mirrored aeye carousel, and probably other
utils." A click or wheel in a mirror pane whose remote program wants the mouse
never reaches that program; the wheel instead drops the local mirror pane into
copy mode.

## Diagnosis (measured)

Scratch servers (`-L m2src` remote, `-L m2dst` local with `mouse on`, a
pty-hosted client on `-L m2obs`), daemon `--test-local`, and a remote probe
that enables mouse modes (`printf '\e[?1000h\e[?1006h'`) then `cat`s raw stdin
to a log. SGR press, release and wheel-up are written into the local client's
terminal input over the probe pane's cell (3,2):

| probe pane | when it enabled mouse | local `mouse_any_flag` | probe received | local pane after |
|---|---|---|---|---|
| tiled | after the mirror was up | 1 | `\e[<0;4;3M \e[<0;4;3m \e[<64;4;3M` | normal |
| tiled | before the mirror attached | **0** | **nothing** | **copy mode** |
| float | after the mirror was up | 1 | `\e[<0;4;3M \e[<0;4;3m \e[<64;4;3M` | normal |
| float | before the mirror attached | **0** | **nothing** | **copy mode** |

More measured rows, same harness (`mouse_any_flag` on the local renderer pane):

| scenario | local mouse state |
|---|---|
| the real aeye viewer (`aeye open`, bubbletea v2: `?1002h ?1006h`) opened as a remote float while the mirror is up — no delay, and again with 80ms added each way on the control transport | mirrored (1002+1006), both runs |
| a mouse-mode float after the daemon's control transport is killed and re-dialled (warm reconnect) | mirrored |
| the same float after `respawn-pane -k` on its mirror pane (tmux's Respawn binds; `rebindRenderer` adopts the re-dial) | **lost** |
| the same float after its renderer process is SIGKILLed (corpse → `healDeadRenderers` → `resetWindow`, which re-adds every float) | **lost** |

So the forwarding path itself works for tiled panes and floats alike, with
correct pane-relative coordinates (`4;3` = pane cell (3,2), 1-based): local
tmux encodes the report relative to the renderer pane (`input_key_mouse` →
`cmd_mouse_at`), the renderer and remote pane have identical dims (a float's
layout cell is its inner box, `bridge-daemon.md`), the renderer forwards stdin
verbatim, and `send-keys -H` writes each byte as `KEYC_LITERAL` to the remote
pane. No coordinate translation or re-encoding is needed. A pane opened while
the mirror is attached also works: the router buffers its pre-registration
output and replays it ahead of the seed (`Router.Register`), so its startup
DECSET arrives.

What breaks is **mode state**. Local tmux forwards a mouse event to a pane only
when that pane's own screen has a mouse mode set (`input_key_mouse`:
`(s->mode & ALL_MOUSE_MODES) == 0` → drop), and the default and
better-mouse-mode `WheelUpPane` binds route on `#{mouse_any_flag}`. The
renderer pane learns mouse modes only from DECSET bytes in the live `%output`
stream. A seed is `capture-pane -e` plus `#{cursor_x} #{cursor_y}
#{alternate_on} #{keypad_cursor_flag}` — it re-asserts the alternate screen and
cursor keys, never the mouse modes. A seed never resets modes either, so a
renderer pane that saw the DECSET live keeps it across later reseeds. The loss
is on every path where a renderer pane gets a seed **without** having seen the
DECSET live:

- the program enabled the mouse before the mirror attached — for the carousel,
  an agent's `ensure-open` (`tmux-claude-images`, `-d` split) opens it on the
  remote with nobody watching, and the mirror attached later seeds it
  mouse-deaf; the same for any tool float or TUI already running at attach;
- a fresh local pane or reset screen that gets only a seed: `resetWindow`
  (heal, desync rebuild; drops and re-adds every mirrored float), a respawn
  adopted by `rebindRenderer` (tmux's `screen_reinit` resets `s->mode`);
- narrowly, a mode change made while that pane's output was being discarded
  (`%pause` window, frames dropped under sink backpressure, the gap between
  `Unregister` and `Register` during a rebuild) — the reseed that repairs the
  screen does not repair the mode.

The live-open path the user most likely pressed (`prefix + I` while viewing
the mirror) was green here even with added latency, so the precise sequence
behind the report is not pinned down; every path measured red is on the seed
side, and the carousel's normal life (auto-opened by the agent, outliving any
number of mirror attaches) puts it on the first bullet.

The converse also holds: a reseed never *clears* a mouse mode, so a remote
program that turned the mouse off while its output was lost (e.g. it exited
during an outage) leaves the mirror forwarding clicks as garbage input to
whatever runs next.

This is also the likely cause of #757 (mouse-wheel scrollback in a mirror
running a fullscreen TUI): with `mouse_any_flag` wrongly 0, better-mouse-mode's
`WheelUpPane` falls through to scroll emulation/copy mode instead of
`send-keys -M`.

## Design

A seed asserts the remote pane's full mouse-reporting state, both on and off.

- **Read** the five mode flags tmux exposes, all read from `wp->base.mode`
  (true even while the remote pane is in copy mode): `mouse_standard_flag`
  (1000), `mouse_button_flag` (1002), `mouse_all_flag` (1003),
  `mouse_utf8_flag` (1005), `mouse_sgr_flag` (1006). They ride the existing
  per-pane `display-message` of `PaneSeeds` (`cursorCmd`), appended after the
  four existing fields, so no new round-trip and the seed-ordering contract
  (reply i acted on before reply i+1 is read) is untouched.
- **Represent** them as `render.MouseMode{Tracking int; UTF8, SGR bool}` —
  `Tracking` is 0, 1000, 1002 or 1003. tmux keeps at most one tracking bit
  (setting any of 1000/1002/1003 clears `ALL_MOUSE_MODES` first); if a reply
  somehow reports more than one, the highest wins.
- **Emit** from `render.Seed`, alongside the existing alt-screen/cursor-key
  modes: `\e[?1000l` when `Tracking == 0` (tmux's reset for any of
  1000–1003 clears all tracking modes), else `\e[?<Tracking>h` (which itself
  clears the other tracking modes); then `\e[?1005h|l` and `\e[?1006h|l`.
  Order-independent of the clear/home and content.
- **Unknown is not off.** `Seed` takes the mouse state as `*MouseMode`; nil
  emits no mouse bytes and leaves the local pane's modes as they are. The
  daemon passes nil when the `display-message` reply is an error or missing,
  or has anything other than exactly nine fields (a remote whose tmux lacks one
  of the formats expands it empty, so the count drops). A transient error must
  not turn a working mirror mouse-deaf. The first four fields are parsed
  whenever there are at least four (today: exactly four), so an old remote
  keeps its cursor placement.
- The M1 single-pane `og-remote-bridge` (`remotebridge/main.go`) passes nil:
  its behaviour is unchanged. It is not the mirror path this issue is about.

Nothing else changes: no input-path re-encoding, no coordinate translation, no
change to `pumpInput`, the dead-key classifier, or any bind. Local
click-to-focus (`MouseDown1Pane` = `select-pane -t=; send -M`), float `-C`
click-outside-close, and copy-mode wheel behaviour on a pane whose remote
program does **not** want the mouse all keep their local semantics; a pane
whose remote program does want the mouse now forwards the wheel exactly as a
local pane running that program would.

### Alternative considered

Put every renderer pane permanently in any-event + SGR mode and have the
daemon re-encode/filter each report for the remote's current mode, tracking
that mode by parsing DECSET out of `%output`. Rejected: it duplicates tmux's
encoder (`input_key_get_mouse`: motion/release filtering, SGR-only-if-the-outer-
terminal-sent-SGR, UTF-8/legacy clamping), needs a stateful parser that is
itself lost across `%pause`, and would forward events locally meant for copy
mode. Mirroring the mode lets local tmux do exactly what it does for a local
program.

### #789 / #790

Forwarding stays byte-verbatim, so it needs no mouse tokenizer: a report split
across two renderer reads (#790) still reaches the remote program intact, since
both halves are written to the same remote pty in order. #789/#790 remain
classifier-only issues in `isDismissKey`; this change does not resolve or
worsen them (a pane that wanted the mouse already produced reports whenever its
DECSET arrived live).

## Acceptance

1. e2e (bats, scratch servers, a pty-hosted client writing real mouse input):
   a remote **tiled** pane and a remote **float** whose program enabled mouse
   reporting before the mirror attached receive a click (press+release) and a
   wheel event at the right pane-relative cell, in SGR (1006) and legacy X10
   (1000) encodings; red before, green after.
2. e2e: a mouse-mode float whose mirror pane is respawned (`respawn-pane -k`)
   still receives a click afterwards; red before, green after.
3. e2e: button-event drag (1002) reaches the program; a pane whose program
   never enabled the mouse still enters local copy mode on wheel (no
   regression).
4. Go unit tests: `MouseMode` sequences for every tracking value and extension;
   `Seed` with nil emits no mouse bytes; `parseCursor` for the 9-field reply,
   the legacy 4-field reply (cursor kept, mouse unknown), and an error reply.
5. Existing bridge tests and #788's dead-key tests stay green.
6. `docs/agents/bridge-daemon.md` documents the mouse path.
7. `nix build .#default`, `nix flake check`, `nix build .#lint` pass.

## Out of scope

Bracketed paste (2004) and focus reporting (1004) have the same seed gap; they
are filed as a follow-up rather than folded in. `alternate_on`/app-cursor keys
are asserted only when on (never cleared) — pre-existing, untouched.
