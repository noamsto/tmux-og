# Bridge: Graphics and Image Paste

## Bridge Graphics

Kitty graphics crossing the remote bridge are localised by
`picker/remotebridge/graphics`, in each pane's output-sink pump. Active
whenever both the remote bridge (`programs.tmux-og.remote.hosts`) and the
agent-carousel toggle (the `aeye` flake input) are wired in — no separate
option.

- **What the remote emits splits into two independently-driven halves
  (#574).** The **relay capability** (the sixel drop gate plus
  `OG_RELAY_GRAPHICS`) is fully local and continuous — see below. The
  **advertised termname** — what aeye's `chooseRelayBackend` reads off the
  remote's own `client_termname` to pick kitty placements vs block art — only
  ever changes by dialling a whole new control client, and only on the
  `prefix + I` carousel gesture, never on a poll. Before #574 the daemon
  advertised whichever terminal *launched* the bridge, frozen for its life;
  viewing that same mirror from a different terminal got kitty placeholders
  (`U+10EEEE` tofu) or a dropped sixel it could actually paint.
- **A control client's `client_termname` is exactly its `TERM` at dial time,
  and tmux has no runtime setter for it** — measured on the pinned 3.7c:
  `refresh-client`'s own usage string carries no termname flag among
  `[-cDlLRSU] [-A pane:state] [-B name:what:format] ...`. So the only way to
  change what the remote sees is a new control client, dialled with a fresh
  `TERM` — there is no seam that patches the live one.
- **Replacement rides the carousel gesture rather than a poll because aeye
  picks its backend once per viewer *launch*, not continuously.** A poller
  that replaced the control client on every terminal switch would race
  `prefix + I`: a replacement still in flight when the keypress lands leaves
  the freshly-launched viewer reading the *stale* termname and painting tofu
  — the exact symptom this exists to fix. Tying the replacement to the
  gesture instead makes it deterministic (nothing happens until a viewer is
  about to launch) and bounds its cost to at most one dial per human
  keypress.
- **The multi-client rule is capability intersection, never last-writer-wins
  or `list-clients` order.** Every non-control client attached to the mirror
  session votes on two ANDs: `kitty` iff every one of them carries an
  `xterm-kitty`/`xterm-ghostty`-prefixed termname, `sixel` iff every one's own
  `#{I/f:sixel}` — tmux's own per-client capability interrogation — reads `1`.
  `client_termfeatures` is still read alongside it, only for the raw
  diagnostic — the kitty-prefix check reads `client_termname` alone; the sixel
  bool itself is never re-derived from matching tokens in `client_termfeatures`.
  Control-mode clients are excluded
  outright — their `client_termfeatures` (and `#{I/f:sixel}`) is always
  empty, so counting one would force sixel false and could hand it the
  termname pick for a "client" that paints nothing. The advertised termname
  is the lexicographically **smallest** termname among the clients that
  witness the AND'd kitty capability, so the identity is a pure function of
  who is attached rather than of attach order. No client attached at all
  keeps whatever was last advertised — a mirror nobody is looking at is not
  evidence to degrade it to block art.
- **The discovering `prefix + I` is nacked with a "press again" message,
  never queued across goroutines.** The ctl handler resolves, compares
  against what the live client actually advertises, and raises the
  replacement *before* `cst.submit` and *outside* `ctlState.mu` — it cannot
  block waiting on main-loop progress while holding that lock, because
  `repair()` → `reconcileWindows` → `cst.forgetWindow` takes the same mutex,
  and a blocked handler would deadlock against the very repair its own raise
  depends on. A differing termname therefore returns `"re-dialling for
  <term> — press again"` instead of running the carousel; the press after
  the swap lands submits normally, and same-terminal viewing is never nacked
  at all because the resolved termname never changed.
- **The ssh `ControlPath` is per-dial, owned by the transport `child` that
  dialled it**, reached everywhere through one accessor: "the path of the
  most recently started child that is still open." During a replacement's
  overlap that is the new, still-unverified master; once an aborted
  replacement's new child closes, the accessor falls back to the surviving
  old path; once a successful swap closes the old child, it reports the new
  one. Getting this wrong is not cosmetic — the graphics fetcher and the
  paste upload both read the path through this same accessor, and a consumer
  stuck on a dead child's path hands ssh a `-S` that no longer exists, which
  makes `ControlMaster=auto` *silently* stop multiplexing and re-authenticate
  per fetch with no tty, on exactly the path a failed replacement is supposed
  to leave untouched.
- **Two honest limits, not bugs.** An already-open carousel viewer never
  re-picks its backend — aeye chooses once at process launch, so the
  guarantee is "the *next* `prefix + I` paints correctly," never "an open one
  repaints." And a real client attached **directly** to the remote session
  (not through this bridge) defeats the whole mechanism: aeye's reader
  requires every client of that session to be control-mode, so one real
  client there makes it fall back to an untargeted `display-message` this
  daemon does not control. Detecting that state would cost a remote
  round-trip per carousel press to change nothing steerable, so it is a
  documented limitation, not a guarded path.
- **Placements need nothing.** Kitty unicode placeholders are ordinary grid
  text, so they cross the bridge (and survive `capture-pane` reseeds) on the
  normal text path. Only the store's `t=f`/`t=t` payload — a path on the
  remote filesystem — is host-bound.
- **`allow-passthrough all` is asserted on both ends.** The global stays `on`,
  and `on` releases a DCS passthrough only for a pane a client can see — which a
  *control* client never is. `markRendererPane` stamps the local mirror pane
  (#464); `PassthroughAllCmd`, sent from `setupWindow` beside
  `AggressiveResizeOffCmd`, stamps the remote window (#529).
  **The option does not gate whether a store reaches `%output`.** Measured on a
  scratch server with a control client attached: a pane emitting an `\ePtmux;`-wrapped
  kitty APC produces a byte-for-byte identical `%output` line under
  `allow-passthrough off` and `all`. `%output` is built from the raw pty buffer
  (`control_append_data` → `window_pane_get_new_data`) before `input.c` parses
  anything, and the option is consulted only in `input_dcs_dispatch`, which
  decides what reaches the *screen* and its tty clients. So the remote stamp
  cannot be what lets a store cross the bridge, and the real mechanism is
  unidentified — most likely sender-side, the image tool declining to emit at
  all when its pane's passthrough is off. Worth settling before anyone relies on
  it: a regression here will not look like the option being unset.
  The remote half is `-w`, not `-p`: pane options inherit from the window's, so
  one command covers panes the remote splits later — which the carousel always
  is.
- **The proxy's kitty path** scans kitty APCs out of the stream (bare and
  `\ePtmux;`-wrapped), rewrites `t=f`/`t=t` payloads to a locally-fetched copy,
  drops `t=s` and any fetch it cannot satisfy (a stale path renders the
  *wrong* image; a blank one self-heals), and re-wraps exactly once for the
  local tmux. `t=t` ("transmit, then delete") is downgraded to `t=f` on
  localisation — the payload now names our local cache copy, and honouring
  the sender's delete-after-read would have the local terminal unlink what
  the fetcher just wrote.
- **A complete bare sixel is relayed byte-for-byte, never re-wrapped in a
  passthrough.** tmux itself parses sixel — `tty_cmd_sixelimage` clamps it to
  the pane and positions it at the pane's cursor — while the passthrough sink
  `tty_cmd_rawstring` does neither, so a wrapped sixel would land wherever
  tmux last left the real cursor, unclipped, and be destroyed by the next
  redraw. Relaying means forwarding unchanged; a `\ePtmux;`-wrapped sixel
  keeps today's drop for exactly that reason.
- **The gate is the AND of every non-control client's own `sixel`
  terminal-feature currently attached to the mirror session** — not, since
  #574, a single sample of whichever client launched the bridge.
  `#{I/f:sixel}` is interrogated continuously off `list-clients` (the
  daemon's `watchLocalClient` watcher, nudged by `client-session-changed` and
  `client-detached` session hooks) rather than once via a launch-time
  `display-message`, and every `Proxy.Filter` plus the `OG_RELAY_GRAPHICS`
  publish site load the *same* `graphics.RelaySource` cell, so the local drop
  and the published value can never disagree. Still deliberately tmux's own
  render condition underneath: anything narrower relays images tmux only
  draws as a `SIXEL IMAGE (WxH)` placeholder (#319's symptom). tmux enables
  `sixel` for no terminal by default, so `programs.tmux-og.sixelTerminals`
  (a list of TERM strings, each getting a `*` suffix) is what emits `set -as
  terminal-features '<term>*:sixel'`.
- **The capability is published to the remote** as `OG_RELAY_GRAPHICS`
  (`sixel` or empty; its only reader is the `aeye` input's carousel) in the bridged remote **session**'s environment via
  control-mode `set-environment` — the same cell that gates the local drop,
  now **re-resolved continuously as the viewing client set changes** (#574)
  rather than computed once at launch, so the remote can never emit what
  we'd drop nor withhold what we'd relay. A capability change re-publishes
  immediately with no re-dial and no attach; `repair()` also re-sends it
  unconditionally on every reconnect, since the outage window is the one
  stretch in which a capability change had no live connection to publish on.
  Unset on teardown, but the dominant teardown path is
  SIGTERM, where the transport is already gone and the unset does not land —
  a stale value is then corrected only by the next bridge's unconditional
  write, and a direct attach to that remote session in the gap can read it.
- **OSC 1337's inline-image verb (`\x1b]1337;File=`) is recognised and
  dropped, whole or partial — never relayed.** Structurally impossible: tmux
  has no inline-image handling, so the only route out is a passthrough, and
  `tty_cmd_rawstring` positions and clips nothing, the same failure mode as a
  wrapped sixel. No capability is lost — foot, WezTerm and iTerm2 all speak
  sixel, so the sixel path already covers the named set. Only the
  inline-image verb is scoped in: the rest of the OSC 1337 namespace
  (`CurrentDir=`, `SetUserVar=`, `RemoteHost=`), which a remote shell emits
  routinely, still forwards verbatim.
- **A truncated sequence is dropped unconditionally, regardless of relay
  policy** — policy governs complete sequences only, the #319 invariant.
  Overflow now enters a discard-to-next-ESC state rather than emitting the
  tail as text: a sixel body contains no ESC, so the discard ends exactly at
  the corrupt image and cannot latch onto later output.
- **A sixel does not survive a bridge reseed.** `capture-pane` returns text,
  so a reseed loses the image as it loses any non-text cell content; the
  kitty path's `retain`/`Replay` doesn't apply here — a kitty placement is
  position-independent, while a sixel is painted at the cursor the sender
  left, so replaying one after a reseed would paint it in the wrong place.
  The image returns on the viewer's next repaint.
- **Sixel over the bridge is megabytes per repaint**, not the short path
  kitty's `t=f` sends — a real cost, not parity with kitty. A sink frame
  dropped under that burst truncates the sequence, which the
  discard-to-next-ESC and `reseedDropped` paths repair.
- The once-per-pane "client has no sixel terminal-feature" diagnostic lands
  on `${sock}.log`, the daemon's stderr file the launcher redirects to — named
  here because a user cannot be expected to know it exists.
- **Fetches** ride the daemon's own ssh `ControlMaster` socket, so they never
  share the control stream with live terminal output. Cache key is
  `(path, mtime, size)` — mtime matters, since viewers rewrite scratch frames
  at a stable path while panning. Each fetch is bounded by a 2s
  `context.WithTimeout` reaching all the way to `exec.CommandContext`, so a
  hung socket or a stalled remote `stat`/`cat` kills the ssh process and drops
  that store instead of freezing the pane's whole stream.
- **Coalescing** drops a store a later one in the same batch supersedes — a
  batch is whatever one `Scanner.Feed` call returns, which in the daemon's
  wiring is one pump-drain's worth (every queued `FrameOutput` handed to the
  proxy in one call). Never coalesced across an `a=d` delete for the same id.
- **`prefix + I`** is gated on `bridgeGate`: inside a mirror window it runs the
  toggle on the remote (ctl verb `carousel`), so the carousel is a remote
  split mirrored back like any other structural change.
- **A press that opens nothing still reports itself, as a local status
  message** (#593). Nothing the remote can say reaches the user: its only
  client is the daemon's control client, which renders no status line, so a
  `display-message` there evaporates — which is why the empty-manifest and
  missing-binary paths used to open a 90%x90% float on the remote and mirror
  it home to say one sentence. The remote script now stamps its outcome
  (`ok` / `noimages` / `nobin`) on the ctl pane as `@og_carousel` and
  `daemon/carouselprobe.go` reads it back: a session-lifetime seam like
  `viewReplacer` (its two sides are closures over `Run`'s locals), armed by
  the ctl handler *after* the submit, with a timer `runConn` selects on —
  a press with no images changes nothing the mirror can see, so its own reply
  block is the last thing that would wake the loop. The read happens on the
  main loop, the only place a round-trip may run. Verdicts are a **closed
  set**, since the option is remote-derived; an unrecognised one says nothing
  but still stops the probe. An **empty read is not an answer** — the stamp
  rides a `run-shell -b`, so it lands a few forks after the press — hence
  bounded retries (250ms x 8), past which the press is treated as launched.
  `ok` is stamped before the `exec` so the common case stops the probe
  instead of burning that budget.
- Remote host needs `tmux-claude-images` and `resvg` on PATH.

## Bridge Image Paste

`ctrl+v` with an image on the *local* clipboard works inside a mirror pane
(#361), even though the agent reading it runs on the remote host. The
mechanism is in `picker/remotebridge/daemon/paste.go`; the design spec
(`docs/superpowers/specs/2026-09-03-clipboard-image-paste-mirror-design.md`)
carries the measured evidence.

- **A paste is one byte.** `ctrl+v` delivers `0x16` on stdin and the agent
  then reads the *system clipboard itself* — so in a mirror the byte reaches
  the remote and the remote clipboard (headless: absent) is read. The daemon
  intercepts the byte in `pumpInput`, the only component that is both local
  (can read the local clipboard) and holding the ssh `ControlMaster` (can
  ship the bytes).
- **Gated and conservative, not a security boundary.** Interception fires
  only when the remote pane's `@bridge_proc` is in the agent set (`claude`
  and `pi` are verified; codex has no clipboard read at all) AND the local
  clipboard actually holds an image. Everything else — shells' quoted-insert,
  text clipboards, empty clipboards — forwards the byte unchanged. A `0x16`
  inside a bracketed paste is content, not the gesture, and is kept. The
  process-name gate is a usability heuristic like every other `@bridge_*`
  field: it's daemon-sanitized but remote-derived, and a hostile remote that
  stamped it falsely would already need code execution in the foreground of
  the pane the user is mirroring — a stronger foothold than the exfil buys.
- **Only the TARGETS probe runs on the input pump.** It has to — it decides
  forward-vs-swallow — and it is bounded (`clipTimeout`, one fork per tool).
  Everything after a swallow (extracting the bytes, capping at 8 MiB,
  uploading over the daemon's ControlMaster with a 5s timeout, injecting the
  path) runs async in a goroutine, so a slow transfer or a wedged clipboard
  owner never freezes the pane.
- **The remote half is a path.** The image rides the daemon's ControlMaster to
  a fresh, per-paste `mktemp -d` directory (0700 by construction, unpredictable
  name — nothing to pre-create) under `/tmp/og-paste-*` on the remote,
  landing at a plain-named file inside it, and the daemon hex-`send-keys` the
  path plus a trailing space into the pane. Claude Code resolves an existing
  image path in the prompt into an attachment at submit (verified live); pi
  — whose own `ctrl+v` also just writes the clipboard image to a temp file
  and inserts that path — reads an injected path back with its `read` tool,
  which sends the image as an attachment (verified live). Either way the
  agent receives the image exactly as if the paste were local. The trailing
  space keeps the user's next keystrokes from merging into the path token,
  which would silently break that resolution.
- **One pane's paste is serialized against its own later input.** The
  handler's lock is acquired for every frame and, when a frame triggers a
  paste, handed off to the paste goroutine rather than released — so nothing
  typed on that pane afterwards (including Enter) can reach the remote ahead
  of the kept prefix and the path injection. Otherwise a prompt could submit
  imageless with no notification if the user hit Enter inside the upload
  window.
- **Cleanup is a janitor, not a delete.** The path is read at *submit*, which
  can be minutes after the paste, so the store script sweeps sibling
  directories older than 60 minutes on each paste instead of deleting on
  inject.
- **Failures are visible.** After the byte is swallowed, any failure (BMP —
  which the agent's path regex excludes — oversize, timeout, unwritable
  remote dir, malformed reply, a dropped send while the bridge reconnects) is
  a local `display-message`, never a silent no-op and never a frozen pane.
  Disabled entirely when the daemon has no ssh transport (`--test-local`),
  where `Config.PasteUpload` is nil.

