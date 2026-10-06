# Spec: image ctrl+v in a mirror pane silently falls through to the remote (#938)

## Problem

Pressing ctrl+v with an image on the local clipboard inside a remote-mirror
pane sometimes doesn't attach the image. The remote agent (Claude Code or pi)
says it found no image on its clipboard, and no `tmux-og: …` message appears.
Every failure that happens *after* the daemon swallows the byte already
notifies, so the cause must be one of the **silent forward** branches of
`pasteHandler.handle` (`picker/remotebridge/daemon/paste.go`):

1. `drops == 0`: no 0x16 seen outside a bracketed paste.
2. `!pasteAgentProcs[procFor(...)]`: the gate lookup missed.
3. probe `!ok`: no image target found.

## Evidence (gathered before fixing)

| # | hypothesis | verdict | evidence |
|---|---|---|---|
| H1 | bracketed-paste state sticks after a split `ESC[201~` | **confirmed** | `pumpInput` carries an incomplete trailing escape (`ESC[20`) to the next frame, but it also starts a 50 ms `escCarryGrace` timer *before* `deliver(complete)`. When `deliver` takes longer than the grace (its `send` blocks on ssh stdin backpressure during a large paste), the timer and the next frame are both ready, and Go's `select` picks one at random. When it picks the timer, the carry `ESC[20` reaches `handle` alone and `1~` arrives in the next frame. `splitPasteDrops` matches markers only inside one payload, so `inPaste` stays true until the next bracketed text paste on that pane (whose `ESC[201~` matches the stuck end-scan), and every ctrl+v on that pane until then is forwarded silently. A scratch probe ran real `pumpInput` with the real `pasteHandler`, a 4096-byte paste frame ending in `ESC[20`, a 60 ms first `send`, and grace set to 20 ms. The later 0x16 was forwarded silently in **20 of 40 trials**. The control run (no backpressure) gave **0 of 40**. A gap of more than the grace between the two pty reads (scheduler stall under load) reaches the same state deterministically. The state is per pane and per renderer connection, which matches "sometimes, no clear pattern". |
| H2 | ctrl+v arrives as `ESC[118;5u` (extended keys) | **refuted** | On a scratch tmux 3.7c server with `extended-keys on` and `csi-u`, a raw 0x16 written through a real attached client pty reaches the pane as `\x16` for no request, for kitty push `CSI >1u` and `CSI >31u` (tmux ignores both: the pane stays `VT10x`), and for `CSI >4;1m`. Only `CSI >4;2m` produces `\x1b[118;5u`, and `keyneg` strips every `CSI > … m` on the output path (#338). Every live mirror pane on the user's server reads `pane_key_mode = VT10x`. |
| H3 | gate lookup misses | **refuted for the steady state; transient failure unverifiable** | Live `@bridge_proc` is `claude`, `fish` or `bash` on every mirror pane, and pi's `pane_current_command` measures `pi` (scratch tmux). The shipper stamps every remote pane on first sight. A lookup error, the 2 s timeout, a missing row, or a not-yet-stamped pane would all forward silently today. None of them can be observed after the fact. |
| H4 | clipboard probe misses | **unverifiable** | From the tmux server's environment, `wl-paste -l` lists types in about 2 ms and xclip is absent. The daemon's own environment (for example a stale `WAYLAND_DISPLAY`) can't be inspected without reading `/proc/*/environ`, which is out of bounds. An image MIME type outside the supported list, or a listing that errors, forwards silently today. |

## Requirements

R1. **H1 fix.** The bracketed-paste scan matches `ESC[200~` / `ESC[201~`
however the input stream is split across `handle` calls, including at every
byte boundary inside a marker. A partial-marker match is carried across frames
in `pasteHandler` state, and the guard stays in place. No bytes are held back
or delayed: kept bytes are returned in the same call, exactly as before. Only
the matcher state crosses the frame boundary. On any single payload the
result is byte-for-byte what it is today.

R2. **Observable pass-through.** When the gesture (a 0x16 outside a paste) is
forwarded for a reason other than "this is a non-agent pane" or "the local
clipboard holds text", the user gets **one** `notify` per gesture. That covers
a frame carrying several drops too: still one message.
- Gate lookup failure. The local `list-panes` errored or timed out, the pane's
  row is missing, or its `@bridge_proc` is still unset. In every one of these
  cases we can't tell an agent pane from a shell, so the forward explains
  itself.
- Probe failure on an agent pane: no clipboard tool on PATH, or every
  available tool's listing failed (error or timeout). The message carries the
  tool's own reason: the first trimmed line of `exec.ExitError.Stderr`, else
  the error, else "timed out" on a deadline (e.g. `wl-paste: Nothing is
  copied`). An empty clipboard therefore notifies too: wl-paste reports it as
  a failed listing, which can't be told apart from a broken one without
  parsing its wording, and the message is accurate either way.
- Unsupported image on an agent pane. A listing offered an `image/*` type,
  but none in the supported set (png/jpeg/jpg/webp/gif/bmp). The message names
  the type.

R3. **Unchanged silent paths.** A non-agent `@bridge_proc` (shell
quoted-insert) forwards silently. So does an agent pane whose clipboard
listing succeeded with no `image/*` type at all, which means text: the agent's
own text-paste or "no image" answer is accurate there. A frame with no 0x16
outside a paste forwards silently, as it always has.

R4. **Forward, not swallow, on these new notify paths.** The byte still
reaches the remote. This matches the design spec's "a host with neither tool
cannot know … so the byte is forwarded", and it keeps a remote-side clipboard
paste working. The notification is what removes the confusion. Swallowing
stays reserved for "we found an image and failed to deliver it".

R5. **No new freeze.** A notify issued on a forward path must not block the
input pump. The gate-failure case can be caused by a wedged local tmux, and
`notifyLocal` forks that same tmux, so these notifies run asynchronously.

R6. **Tests.** Each confirmed cause gets a regression through `handle` that is
red on `main` and green on the branch. For H1 that is a split marker at every
interior offset of both markers. The red run is required for the confirmed
cause. The new notify paths, each R3 silent path, and `probeClipboardImage`'s
classification are covered as well.

R7. **Docs.** `docs/agents/bridge-graphics-paste.md` → "Bridge Image Paste"
describes the cross-frame marker matching and the new notify-and-forward line.
The plan is committed under `docs/superpowers/plans/`.

## Non-goals

- Fixing `pumpInput`'s carry-vs-timer `select` race itself. It also splits
  mouse reports and other escapes under backpressure, which is a separate
  invariant and is reported as an untracked follow-up for the dispatcher (no issue filed without approval). R1 makes the paste scan independent of
  how frames are split, so this bug no longer depends on that race.
- Recognising an extended-key ctrl+v (H2 refuted).
- Re-reading `WAYLAND_DISPLAY` from the tmux server (H4 unverifiable; R2 now
  surfaces the tool's error instead).
- Text-clipboard shipping (an existing non-goal).
- A transiently stale `@bridge_proc` (an agent launched less than a shipper
  pass ago, still stamped `fish`) keeps forwarding silently. It is a valid
  non-agent value, indistinguishable from a real shell, and the next shipper
  pass corrects it.

## Acceptance

- The H1 regression through `handle` fails on `main` and passes on the branch.
- `nix build .#default`, `nix flake check` and `nix build .#lint` pass.
- The PR body reports H1–H4 with the evidence above.
