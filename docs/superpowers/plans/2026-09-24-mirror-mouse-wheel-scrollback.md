# Mirror mouse-wheel scrollback broken with a fullscreen TUI (#757) — implementation plan

## Diagnosis (already verified on a scratch tmux server)

Local tmux's `#{mouse_any_flag}`/`#{mouse_standard_flag}`/`#{mouse_button_flag}`/
`#{mouse_all_flag}`/`#{mouse_sgr_flag}`/`#{mouse_utf8_flag}` are derived purely
from DECSET/DECRST bytes (`\x1b[?1000h` etc.) that a pane's occupant writes to
its own pty — confirmed by `printf`-ing those sequences into a scratch pane and
reading the format back (`1000h`→standard+any, `1003h`→all [replaces
standard/button], `1006h`→sgr, `1005h`→utf8; in tmux, setting any one of
1000/1002/1003 clears the other two). The mirror pane's occupant is the
renderer (`render.Run`), whose stdout is exactly that pty: `render.Seed`
(`picker/remotebridge/render/snapshot.go`) already reasserts `alternate_on`
(`?1049h`) and `keypad_cursor_flag` (`?1h`) on every (re)seed, but never
touches mouse-tracking mode. `PaneSeeds`/`PaneSeed`
(`picker/remotebridge/daemon/seed.go`) is the single choke point for every
seed and reseed path (initial attach, `reattach`'s repair pass,
`reseedDropped`, `reseedReshaped`, zoom/layout reconcile, `reveal.go`,
`sessionpin.go`'s post-switch reseed) — confirmed via `rg PaneSeed(s)?\(`. So:
a remote pane that already has mouse tracking on *before* the mirror attaches
(the reported bug — pi already in its fullscreen, mouse-tracking TUI) never
gets that state painted into the local pty, local tmux thinks the mirror pane
has no mouse mode, and the wheel falls through to local
copy-mode/better-mouse-mode instead of being forwarded.

**Mouse mode is not like alt-screen here — it needs an explicit clear, not
just silence.** Alt-screen has no meaningful "off, but the local pane thinks
it's on" case worth correcting proactively. Mouse mode does: any reseed can
follow a state where the *previous* seed (or a stale live stream) left local
mouse tracking on — the remote's own `?1000l` can be lost while the sink is
paused (`docs/agents/bridge-daemon.md`: "a paused sink drops every frame") or
while pi exits mid-outage — and a plain "assert only what's true" seed would
leave the local pane's stale mouse tracking on forever, misrouting every later
wheel event to a shell that isn't listening for it. So the seed must always
assert the full off/on state: clear all three tracking-granularity DECSET
codes and both encoding extensions unconditionally, then set whichever ones
`MouseMode` reports true.

`capture-pane -e` cannot carry any of this — it's screen content, not terminal
mode — so the fix queries the remote pane's current mouse flags alongside
cursor/alt/appck, and reasserts (set or clear) the matching DECSET codes in
every seed.

Violated invariant: **a mirror pane's local terminal-mode state (alt screen,
app-cursor-keys, mouse tracking) matches the remote occupant's state exactly
after any seed** — not "eventually, via live output" and not "only when it's
on". This is a behavioral bug fix (`EVIDENCE_REVIEW.md`), not a shared-contract
change — the only producer/consumer is `PaneSeeds` → `render.Seed`, both
edited together; confirmed by grep that `render.Seed` has exactly two callers
(`daemon/seed.go:107`, `main.go:103`) and `parseCursor` has exactly one
(`seed.go:94`), so `PaneSeed`/`PaneSeeds`' own signatures — and every one of
*their* callers in `daemon.go`, `reconcile.go`, `reveal.go`, `sessionpin.go` —
need no change.

## Step 0 — red evidence (EVIDENCE_REVIEW.md)

- [ ] Write `TestPaneSeedCarriesMouseModeThroughSeed` in `daemon/seed_test.go`
  FIRST, against HEAD, using only the existing `PaneSeed` (unchanged
  signature, so it compiles against HEAD) — with its **final** 9-field
  fixture, unchanged by any later step: cursor reply `"5 2 1 0 0 0 1 1 0"`
  (cx=5 cy=2 alt=1 appck=0 standard=0 button=0 all=1 sgr=1 utf8=0) plus a
  normal capture reply. Assert `\x1b[?1003h` and `\x1b[?1006h` each appear, as
  two separate `t.Errorf` checks (not one combined `&&`, so the failure
  message names which one is missing). Run
  `go test -run TestPaneSeedCarriesMouseModeThroughSeed ./remotebridge/daemon/`
  from `picker/` and record BOTH assertions failing at HEAD: HEAD's
  `parseCursor` requires exactly 4 fields, so this 9-field reply falls
  through the `len(fields) != 4` branch to `(0,0,false,false)` — the mouse
  bytes can never appear, for the same root cause as a real bug repro would
  hit (no mouse query exists yet), not an artificial fixture mismatch. Do
  **not** rewrite this test later — step 3 re-runs it verbatim and records it
  green once steps 1–2 ship, which is what makes this red/green pair honest
  evidence for the same claimed bug rather than two different tests.

## Step 1 — `render/snapshot.go`: carry mouse mode in the seed (set AND clear)

- [ ] Add:
  ```go
  // MouseMode is which mouse-tracking DECSET modes tmux reports set on a
  // pane. Standard/Button/All are mutually exclusive tracking granularity
  // (1000/1002/1003); SGR/UTF8 are independent encoding extensions
  // (1006/1005) layered on top.
  type MouseMode struct {
  	Standard, Button, All bool
  	SGR, UTF8              bool
  }
  ```
- [ ] Change `Seed`'s signature to
  `func Seed(captured []byte, cursorX, cursorY int, altScreen, appCursorKeys bool, mouse MouseMode) []byte`.
  After the existing `appCursorKeys` block (`\x1b[?1h`) and before `\x1b[2J\x1b[H`,
  **unconditionally** clear every mouse mode first, then set whichever ones
  are true — a mirror pane's *previous* mouse state (from an earlier seed or
  live stream) must never survive a seed that doesn't mention it:
  ```go
  b.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1005l")
  if mouse.All {
  	b.WriteString("\x1b[?1003h")
  } else if mouse.Button {
  	b.WriteString("\x1b[?1002h")
  } else if mouse.Standard {
  	b.WriteString("\x1b[?1000h")
  }
  if mouse.SGR {
  	b.WriteString("\x1b[?1006h")
  }
  if mouse.UTF8 {
  	b.WriteString("\x1b[?1005h")
  }
  ```
- [ ] `main.go:103` (the legacy `og-remote-bridge` binary — see step 4 for why
  it's otherwise out of scope): update the same call site in *this* step, not
  later: `render.Seed(captured, s.cx, s.cy, s.alt, s.appck, render.MouseMode{})`.
- [ ] `daemon/seed.go:107` also calls `render.Seed` with the old 5-arg shape,
  so `picker/remotebridge/daemon` won't build again until step 2 threads a
  real `mouse` value through. Pass `render.MouseMode{}` there too, in *this*
  step, purely to keep the tree compiling at every step boundary; step 2
  replaces it with the parsed `mouse` value. After this step,
  `go test ./remotebridge/render/... ./remotebridge/daemon/...` both build
  (the daemon package's behavior is unchanged until step 2 — every existing
  daemon test still passes against the old 4-field wire format).
- [ ] `render/snapshot_test.go` (`package render`, internal test — bare
  `MouseMode{}`, not `render.MouseMode{}`): add `MouseMode{}` as the 6th arg to the
  3 existing `Seed(...)` calls (their assertions are unaffected, but note the
  seed now also contains the unconditional `l` resets). Add:
  - `TestSeedMouseTrackingModes` — table over `{All}`→contains `?1003h`, not
    `?1002h`/`?1000h`-as-a-*set*; `{Button}`→`?1002h` only;
    `{Standard}`→`?1000h` only — confirming the mutual-exclusivity precedence.
  - `TestSeedMouseEncodingExtensions` — `{SGR:true}`→contains `?1006h`;
    `{UTF8:true}`→contains `?1005h`; independent of tracking granularity.
  - `TestSeedNoMouseTrackingClearsMouseModes` — zero-value `MouseMode{}`: seed
    contains `?1000l`, `?1002l`, `?1003l`, `?1006l`, `?1005l` (the clears) and
    none of `?1000h`/`?1002h`/`?1003h`/`?1006h`/`?1005h` (no sets) — this is
    the regression guard for "no mouse tracking → local default", and also
    the guard against the stale-mouse-mode failure mode the diagnosis names.

## Step 2 — `daemon/seed.go`: query and parse the remote's mouse flags

- [ ] Extend `cursorCmd`'s format string with 5 more fields, in this order:
  `#{cursor_x} #{cursor_y} #{alternate_on} #{keypad_cursor_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_all_flag} #{mouse_sgr_flag} #{mouse_utf8_flag}`.
  (All five format variables exist in any tmux this bridge supports, so this
  is not expected to matter in practice; noted only because `parseCursor`'s
  `len(fields) != 9` guard means an unknown variable — which tmux expands to
  empty — would silently zero out cursor/alt/appck too, not just mouse.)
- [ ] `parseCursor` returns a `render.MouseMode` alongside the existing
  values: `func parseCursor(l controlmode.Line, ok bool) (cx, cy int, alt, appCursorKeys bool, mouse render.MouseMode)`.
  On the two existing degrade paths (`!ok || Error`, and `len(fields) != 9`
  — was `!= 4`), return `render.MouseMode{}` (all clears, no sets — the safe
  default) alongside the existing zero values. On success,
  `mouse = render.MouseMode{Standard: fields[4]=="1", Button: fields[5]=="1", All: fields[6]=="1", SGR: fields[7]=="1", UTF8: fields[8]=="1"}`.
- [ ] `PaneSeeds`: `cx, cy, alt, appck, mouse := parseCursor(curLine, curOK)`;
  pass `mouse` through to `render.Seed(replaceLF(captured), cx, cy, alt, appck, mouse)`.
- [ ] Update the two doc comments on `cursorCmd`/`parseCursor` that quote the
  old 4-field format string.

## Step 3 — `daemon` test fixtures and the regression tests

- [ ] Every scripted cursor-reply body in `picker/remotebridge/daemon/*_test.go`
  is one of exactly 3 literals: `"0 0 0 0"`, `"1 1 0 0"`, `"5 2 0 0"` (53
  occurrences via `rg -n '"[0-9]+ [0-9]+ [01] [01]"' picker/remotebridge/daemon/*_test.go`,
  **plus** `reconcilelayout_test.go:72`, which builds the same `"0 0 0 0"`
  body through a `fmt.Sprintf` the regex doesn't match literally — check that
  call site by name, not just the regex). Mechanically extend each to 9
  fields with 5 trailing zeros: `"0 0 0 0 0 0 0 0 0"`, `"1 1 0 0 0 0 0 0 0"`,
  `"5 2 0 0 0 0 0 0 0"` — none of these tests assert anything about mouse
  mode, only that the wire format still has the fields `parseCursor` expects.
- [ ] Re-run `TestPaneSeedCarriesMouseModeThroughSeed` (step 0) UNCHANGED —
  same fixture, same assertions. It's now green: `parseCursor` reads all 9
  fields, `mouse.All`/`mouse.SGR` come back true, and `render.Seed` emits
  `\x1b[?1003h`/`\x1b[?1006h`. This is the red/green pair `EVIDENCE_REVIEW.md`
  and acceptance bullet 1 ask for — same test, old behavior red, fixed
  behavior green.
- [ ] Add `TestPaneSeedNoMouseTrackingClearsMouseModes` — reply
  `"0 0 0 0 0 0 0 0 0"` (today's only real-world shape until this ships) →
  seed contains the `?1000l`/`?1002l`/`?1003l`/`?1006l`/`?1005l` clears and
  none of the `h` sets — the daemon-level twin of step 1's render-level test,
  proving the clear survives the full `PaneSeeds` → `render.Seed` path.

## Step 4 — legacy `og-remote-bridge` scope note

`picker/remotebridge/main.go`'s call site was already fixed in step 1 (to keep
it compiling). No further change: this M1 standalone binary isn't in the
production mirror path (`scripts/og-remote-open.sh:390-392` starts only the
`ctl`/`daemon`/`renderer` trio; the bare `og-remote-bridge` binary is named
only at `flake.nix:2068`, for `tests/remote-bridge-integration.bats`) — it
gets a correct, inert `render.MouseMode{}` and nothing else. No test-fixture
edits needed there (`main.go`'s own `readCursor`/format string is untouched).

## Step 5 — docs

- [ ] `docs/agents/bridge-daemon.md`, "Mirror invariants" section: add one
  terse bullet next to the existing reseed invariants, e.g.: "**A seed sets
  and clears the remote pane's mouse-tracking DECSET state** (`?1000h`/
  `?1002h`/`?1003h` tracking granularity, `?1006h`/`?1005h` encoding, each
  unconditionally cleared before whichever are true are set), alongside
  alt-screen and app-cursor-keys — without the clear half, a mirror pane's
  stale local mouse tracking (from a paused sink dropping the remote's own
  `?1000l`, or the occupant exiting mid-outage) would survive every later
  seed and keep misrouting the wheel to a shell that isn't listening; without
  the set half, a mirror attached (or reseeded) after the remote occupant
  already turned mouse tracking on leaves local tmux believing the pane has
  none, so the wheel falls through to local copy-mode instead of forwarding
  to the app (#757)."

## Step 6 — manual verification on a scratch server (acceptance's 2nd bullet)

Per CLAUDE.md's scratch-server convention and the M2.1 `--test-local` seam
(`tests/remote-m2-integration.bats`'s own pattern — no ssh, two private `tmux
-L` servers), *after* steps 0–3 are green. Two things must be shown, not one:
the mirror pane's *flags* end up right (step 2's fix), and a wheel byte
*actually reaches the remote pane* once they do (the daemon's send-path,
triage candidate (2), which nothing before this step exercises).

- [ ] Build `daemon`/`renderer` via `go build` from `picker/`. Use a private
  `TMUX_TMPDIR`/`CLAUDE_STATUS_DIR` per this repo's CLAUDE.md scratch-server
  convention, and minimal `-f` configs for both `tmux -L` servers (base-index
  1, per `tests/remote-m2-integration.bats`'s own `SRC_CONF`/`DST_CONF`) — a
  bare `tmux -L` with no `-f` loads this repo's full wrapped config and hooks,
  which is not what's under test here.
- [ ] `SRC`: private `tmux -L` server, a session/window whose pane already has
  `printf '\x1b[?1049h\x1b[?1000h\x1b[?1006h'` run in it, then a foreground
  `cat` — its tty stays in canonical mode with echo on, so bytes it receives
  as pane input are echoed straight back into the visible screen and show up
  in `capture-pane` — simulating pi already fullscreen with mouse tracking on
  when the mirror opens (the exact reported scenario).
- [ ] `DST`: private `tmux -L` server, run
  `daemon --test-local --src-socket <SRC socket name> --dst-socket <DST socket name> --session <SRC session> --window <SRC window> --local-sess <DST session> --renderer "$RENDERER" --sock <ctl sock path>`
  (flags per `picker/remotebridge/cmd/daemon/main.go` and
  `tests/remote-m2-integration.bats`'s own setup) mirroring that window; wait
  for the renderer pane; record its pane id.
- [ ] Assert `DST`'s mirror pane's
  `#{alternate_on} #{mouse_any_flag} #{mouse_sgr_flag}` reads `1 1 1` —
  matching `SRC` — proving the fix closes the seed-time gap through the real
  pipeline (not just the Go unit tests' synthetic stream).
- [ ] With the flags confirmed, prove the wheel byte's own round trip: run
  `tmux -L DST send-keys -t <mirror pane id> -H 1b 5b 3c 36 34 3b 35 3b 35 4d 0a`
  (a literal SGR wheel-up, `\x1b[<64;5;5M`, plus a trailing newline so `cat`'s
  canonical-mode line buffering flushes it — exactly what tmux's own
  `better-mouse-mode`/core mouse dispatch would write to a `PANE_MOUSE`-
  flagged pane's stdin once `mouse_any_flag` is 1, per
  `tmux -L DST list-keys -T root WheelUpPane`, worth recording too). Then
  `tmux -L SRC capture-pane -p -t <src pane>` and confirm the substring
  `[<64;5;5M` appears — this is the tty's own echo of the raw bytes `cat`
  received (`capture-pane` doesn't render a bare ESC itself; the echoed `^[`
  or the terminal's rendering of it is the actual evidence, not a decoded
  ESC glyph) — proving `pumpInput` → `send-keys -H` → the remote pane
  received the SGR mouse sequence intact, not just typed text. For an
  unambiguous byte-level dump, run `stty -echo` first (tty echo is a
  line-discipline setting independent of the reader program, so `od` alone
  doesn't disable it) then `od -An -c` instead of `cat` in the SRC pane; GNU
  `od` buffers in fixed blocks, so also send `C-d` after the wheel bytes to
  flush it, and match its output loosely (`033`, `[`, `<`, `6`, `4` as
  separate column-padded tokens, not the single-spaced literal
  `033 [ < 6 4 ; 5 ; 5 M`). Record every command and its output in the PR's
  `## Testing` section.
- [ ] Negative case: repeat with no `printf` (no mouse tracking) — assert
  `DST`'s mirror pane reads `#{mouse_any_flag}` = `0`, unchanged from today,
  so local copy-mode/better-mouse-mode still governs the wheel exactly as
  before.
- [ ] Not in scope: tmux's own decision of *when* to write an SGR sequence to
  a pane's stdin on a real physical wheel event (that's core tmux mouse
  dispatch, unmodified by this fix, and already exercised by every local pane
  in this config) — the `send-keys -H` step above stands in for that decision
  deliberately, isolating what this fix actually changes: the flag that makes
  tmux take that branch, and the daemon's forwarding of whatever tmux decides
  to send. `tests/remote-m2-integration.bats` has no precedent for
  content-level (as opposed to structural/dims) seed assertions either
  (alt-screen/app-cursor reassertion, shipped earlier, has zero bats
  coverage, only Go-level) — this manual check is proportionate to that
  precedent while still closing the gap the first plan draft's critic flagged
  (asserting flags alone doesn't prove a wheel byte reaches the app).

## Step 7 — gates and commit

- [ ] `go test -race ./remotebridge/...` (from `picker/`, matches
  `flake.nix`'s own invocation).
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint`.
- [ ] Commit this plan file (`docs/superpowers/plans/2026-09-24-mirror-mouse-wheel-scrollback.md`)
  alongside the code, per this repo's CLAUDE.md convention.

## Addendum — #794 folded in (2026-09-27)

#794 (mouse into mirrored floats) measured that forwarding already works for
floats and tiled panes alike once the mode is seeded, so it was folded into
this change. Two amendments came with it
(`docs/superpowers/specs/2026-09-27-794-bridge-mouse-modes-design.md`):

- **Unknown is not off.** `Seed` takes `*MouseMode`; nil emits no mouse bytes.
  `parseCursor` returns nil unless all nine fields are present, and still
  parses the cursor from any reply with at least four, so a transient
  `display-message` error cannot turn a working mirror mouse-deaf and an old
  remote keeps its cursor placement. `main.go` passes nil. This replaces the
  degrade-to-`MouseMode{}` rule in steps 1–2.
- **More paths, pinned by bats.** Besides pre-attach, `respawn-pane -k`
  (`rebindRenderer`) and a renderer kill (heal → `resetWindow`, which re-adds
  every float) also seed a pane with no live DECSET behind it. All go through
  `PaneSeeds`. `tests/remote-m2-integration.bats` drives a tiled probe and a
  float probe through each path with a real client's click, wheel and drag, in
  SGR and X10, plus the `%pause` stale-mode case — replacing step 6's manual
  check with a test that runs in `nix flake check`.
