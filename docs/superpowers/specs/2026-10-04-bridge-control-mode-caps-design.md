# Bridge control-mode reader: byte caps and newline forgery (#860)

## Problem

tmux writes option values raw into control-mode output, both inside a reply
body (`display-message -p`, `list-* -F`, `show-options -v`) and after the
` : ` of a `%subscription-changed` line. A remote-set value holding `\n`
therefore becomes several stream lines on the controller, and
`controlmode.Reader` acts on any of them that parse as a known verb:

- **Line forgery.** Inside a reply block, any `%end`/`%error` closes the block
  (`readBlock` does not compare it with the `%begin`), and any known verb is
  lifted out as a notification. A forged `%end` desyncs reply framing, a forged
  `%exit` ends the mirror, a forged `%subscription-changed` feeds a shipper a
  value it never subscribed to. The same applies to *pane content*: the seed
  reads `capture-pane -e -p`, and a pane row that starts with `%exit` or
  `%end …` is read the same way. That needs no socket access at all, only a
  program printing to a remote pane.
- **Unbounded buffering.** `readBlock` accumulates a body with no byte cap
  (256 × 1 MiB measured `heapInuse=508MiB` from one `Next()`), and one line over
  the scanner's 4 MiB cap ends the stream (`bufio.ErrTooLong` → bare EOF →
  drop → reattach → the same value again, in a loop).

The attacker model is the remote tmux socket holder, who can already
`detach-client`; the goal is robustness of the remote-derived-values boundary
(CLAUDE.md), plus the content case above, which has no such precondition.

## Measured (scratch servers, `-f /dev/null`, private `TMUX_TMPDIR`)

Versions: next-3.9 (the flake's pin, rev 8a9122d), 3.7c (nixpkgs), 3.3a
(nixos-23.05). 3.2a (nixos-21.11, the oldest release with subscriptions)
matched 3.3a on every wrapper probe: `#{s/[[#{l::}cntrl#{l::}]]/ /:…}` works
for a `display -p` value, a whole `-F` row and a subscription format, and `\n`
and `[[#:cntrl#:]]` do not match.

| Probe | next-3.9 | 3.7c | 3.3a |
|---|---|---|---|
| `display -p '#{@nl}'`, `@nl` = `a\n%exit\n%end 1 2 1\nb` | 4 raw body lines | same | same |
| `refresh-client -B 'sub::#{@nl}'` | `%subscription-changed … : a` then 3 top-level lines | same | same |
| `#{s/\n/ /:@nl}` | `\n` is the letter `n` (`%e d 1 2 1`) — no newline match | same | same |
| `#{s/[[:cntrl:]]/ /:@nl}` | modifier parse breaks at the first `:` | same | same |
| `#{s/[[#:cntrl#:]]/ /:@nl}` | **works** | no match, value raw | no match, value raw |
| `#{s/[[#{l::}cntrl#{l::}]]/ /:@nl}` | **works**: `a %exit %end 1 2 1 b` | **works** | **works** |
| same, wrapping a whole `-F` row (`#{window_id}\|#{@nl}`) | one row, `@0\|a %exit …` | works | works |
| same, as a subscription format | one line | one line | one line |
| `\r`, `\t`, `ESC` under the same wrapper | each → one space | same | same |
| value with invalid UTF-8 + `\n`, UTF-8 locale | expansion is **empty** (regexec fails) | — | empty |
| `window_name` / `session_name` set with `\n` | rejected (`invalid window name`) | — | vis-escaped `w\nx` |
| `pane_title` set with `\n` | not applied | — | not applied |
| `session_path` from a `-c` dir holding `\n` | **raw newline** in reply | — | **raw newline** |
| `#(printf 'j1\nj2')` | last line only (`j2`) | — | same |

tmux source (pinned rev):

- `cmdq_fire_command` writes `%begin`, runs the command, then writes
  `%end`/`%error`, all synchronously, with the same `time`, `number` and `flags`
  (`cmd-queue.c:595,653-655`, `cmdq_guard`). `number` is a server-global
  counter (`cmdq_next`). So a real `%end` always repeats its `%begin`'s three
  fields exactly, and blocks on one client's stream never nest.
- `%subscription-changed` is written only from `monitor_timer` (an
  `evtimer`, `monitor.c`); on older releases from `control_check_subs_timer`,
  also a timer. A timer cannot fire between a `%begin` and its `%end`, so a
  `%subscription-changed` line inside a block is never genuine.
- `%exit` is printed by the tmux *client process* as it exits
  (`client.c:419-421`), so it is always the last line of the stream.
- The pin also defers every notification out of open blocks
  (`control_notify_write`, `guard_depth`); 3.3a–3.8 do not (#276), so
  in-block notifications must still be lifted for older remotes.

## Design

Two layers, because neither closes the class alone: remote-side escaping
cannot cover a socket holder who replaces a subscription format
(`refresh-client -t <ours> -B …`), or a remote whose regex engine behaves
differently, and the reader cannot tell a forged top-level line from a real one
after the fact.

### 1. Reader framing (`picker/remotebridge/controlmode`)

**Bounded line reading.** Replace `bufio.Scanner` with a `bufio.Reader` loop
(`ReadSlice('\n')`) that keeps `ScanLines`' semantics (strip one trailing
`\r`; a final unterminated line is still a line) and a per-line cap
`MaxLine = 1 MiB`. A longer line is consumed to its newline without being
kept, and never ends the stream:

- top level → returned as `Kind: Other` (dropped by every consumer);
- inside a block → the block is marked overflowed (below).

**Notifications stream out of an open block.** Today `readBlock` collects
every lifted in-block notification into a slice that `Next` hands out only
once the block closes, so those lines are buffered without bound (a body of
1 MiB `%output` lines is never "body" and never capped). The block becomes
state on the `Reader` instead: `Next` returns each lifted notification the
moment it is read and the terminal line when the block closes. The order a
consumer sees is unchanged — the notifications already preceded the terminal
line — only the timing moves earlier, and at most one line plus the capped body
is held at any time.

**Body cap.** `MaxBody = 16 MiB` of retained body bytes per block. Once a
block exceeds it (or holds an overlong line) it stops retaining body and keeps
reading — still lifting in-block notifications as today — until its *matching*
guard line, then yields its terminal line as `Kind: Error`, `Flags` = the
`%begin`'s flags (so `claimSeq` counts it exactly once), `Args = [time]` as
today, `Data` = the error text, and a new `Line.Err = ErrReplyTooLarge`. Every
caller already treats `Kind == Error` as a failed command, so the one request
fails and the next reply parses normally; nothing drops the connection. The
body is built in one `bytes.Buffer`, not a `[]string` plus `strings.Join` copy.

**Strict guard matching.** A `%end`/`%error` closes the open block only when
its three fields equal the `%begin`'s; any other guard line inside a block is
body. A top-level `%end`/`%error` (no block open — tmux never writes one) is
`Other`. A top-level `%begin` that is not exactly three unsigned decimal fields
with flags `0` or `1` is `Other`, so a malformed line cannot open a block that
nothing can close.

**In-block verbs that are never genuine.** Inside a block, `%begin` (already),
`%subscription-changed`, and a non-matching guard are body. `%exit` is held:
if the very next read is end-of-stream it was genuine (a server that died
mid-command) and is emitted as today (`Exit`, then the synthesized `End`);
otherwise it was body. The lookahead costs nothing: the reader is already
blocked waiting for the block's end. It relies on the daemon's transport
running `tmux -C` (not `-CC`, which prints a trailing `ESC \` after `%exit`):
the client exits right after printing `%exit`, so end-of-stream follows it. Every other in-block notification is
lifted unchanged (#276).

Top-level `%exit` and `%subscription-changed` stay as they are — see
Residuals.

### 2. Remote-side newline policy (`picker/remotebridge/daemon`)

A helper `ctlSafe(format string) string` returns
`#{s/[[#{l::}cntrl#{l::}]]/ /:` + format + `}`: every control byte of the
row's expansion, newline included, becomes one space. Applied to the whole
format, so every field — including ones added later — is covered and tmux's own
row separators (outside the expansion) are untouched. Space, not deletion:
`og_open`'s records are whitespace-split (`strings.FieldsSeq`), JSON treats it
as whitespace, and every other consumer already strips control bytes, so no
consumer's parse changes. Byte length is preserved for single-byte control
characters, so `openURLFormat`'s `#{n:}` bound still holds (the bound is
evaluated inside the wrapper).

Invalid UTF-8 in the wrapped expansion measured two ways: empty on one probe,
the 0xff byte kept with control bytes turned to spaces on another. Either way no
raw newline reaches the stream. An empty expansion reads however each consumer
reads an empty row: a label row is skipped (previous stamp persists), an empty
`og_res`/`og_usage` value unsets `@bridge_res`/`@bridge_usage`, and a skipped
agent row reaps that pane's agent status. The blast radius is the wrapped
expansion: one window's label row, one pane's agent row — but
`agentUsageFormat` is a single `#{S:#{W:#{P:…}}}` row, so one pane command or
`@og_agent_usage` holding invalid UTF-8 can blank the whole usage
segment. Accepted: that is the "remote publishes nothing" state the segment
already handles, and per-field wrapping would leave a later field unwrapped by
omission.

Applied at:

| Site | Command | Remote-settable fields | Disposition |
|---|---|---|---|
| `subscriptions.go` `sendSubscription` | `refresh-client -B` for `og_labels`, `og_agents`, `og_res`, `og_usage`, `og_open` | user options, `pane_current_command` | **wrap** (one choke point for all five) |
| `windowlabels.go:348` | `list-windows -F windowLabelFormat` | user options | **wrap** |
| `agentstatus.go:217` | `list-panes -s -F agentStatusFormat` | user options, `pane_current_command` | **wrap** |
| `openurl.go:165` | `display-message -p openURLFormat` (seed) | `@og_open_url` | **wrap** |
| `sessionpath.go:20` | `display-message -p '#{session_path}'` | `session_path` (raw `\n` measured) | **wrap** |
| `windows.go` `windowListFormat` (3 sites) | `list-windows -F` | `window_name` | none: tmux rejects or vis-escapes a newline name (measured); wrapping a structural row would make an invalid-UTF-8 name empty the row and drop the window |
| `carouselprobe.go:172` | `show-options -pqv @carousel_verdict` | the verdict | none: `show-options` takes no format; reader layer bounds it, and a multi-line value fails the exact-match verdict compare |
| `ctl.go:680` `themeProbeCmd` | `display-message -p '#(…)'` | job stdout | none: tmux keeps only the job's last line (measured) |
| `daemon.go:2257`, `sessionpin.go:72`, `seed.go:46`, `agentstatus.go:476`, `main.go:187,198` | ids, geometry, flags, time | none | none |
| `seed.go:50`, `main.go:199` | `capture-pane -e -p` | pane content | none possible (content); a row cannot hold `\n`; the reader layer makes only a non-matching guard, `%begin`, `%exit` (unless EOF follows) and `%subscription-changed` inert in a body — other notification verbs are lifted (#276), residual |
| `ctl.go` `run-shell -b` script bodies | remote `tmux show-options` into shell variables | — | none: output never reaches the stream |

Commands the daemon runs on the **local** server (`LocalTmuxOut`) are out of
scope: that server is trusted and not on the control stream.

### 3. Cap sizes

Largest genuine payloads, by producer:

- `%output` lines: tmux writes at most `CONTROL_BUFFER_HIGH` (8 KiB) of pane
  data per write, octal-escaped ≤ 4×, so ≤ ~32 KiB per line.
- `%layout-change` / v2 JSON layout: ~200 B per pane; 100 panes ≈ 20 KiB.
- Subscription values: labels/agent rows are dropped daemon-side over 128 B per
  field; `og_usage` JSON is capped at 4 KiB plus ~16 B per pane;
  `og_open` ≤ 12288 B by its own bound.
- Capture rows (`capture-pane -e`): ≤ cols × ~50 B with dense truecolor SGR;
  a 1000-column pane ≈ 50 KiB.
- Whole bodies: a capture is rows × row size — 500×150 cells at 50 B ≈ 3.75 MB,
  an extreme 1000×300 ≈ 15 MB; `list-panes -s` with `agentStatusFormat` is
  ~1–2 KiB per pane, so 500 panes ≈ 1 MiB.

`MaxLine = 1 MiB` is ≥ 20× the largest genuine line and bounds a single
subscription line's memory to 1 MiB (was 4 MiB). `MaxBody = 16 MiB` holds the
extreme capture with margin and bounds one reply's memory to ~16 MiB plus one
line (was unbounded).

### Residuals (documented, not closed)

- **A socket holder who replaces a subscription format** (`refresh-client -t
  <ours> -B`) can still forge top-level lines: a `%exit` (ends the mirror — the
  same reach as `detach-client`), a `%subscription-changed` (the same reach as
  setting the option), or a complete `%begin`/`%end` pair (misattributes one
  reply; the `og-fanout-N` barrier re-syncs the count at the next barrier).
  A *well-formed unterminated* forged `%begin` stalls every later reply and
  subscription line as body until the connection drops: pane output keeps
  flowing (notifications stream out of the open block) and memory stays
  bounded by `MaxBody` plus one line. Resolving it
  would mean letting a nested pair close an outer block, which would let a
  program printing to a pane drop a genuine reply and desync the stream for
  good; the content case has the weaker attacker, so it wins. A round-trip
  deadline would bound the stall; that is a separate daemon change.
- **Other notifications inside capture bodies** (`%output`, `%window-close`,
  `%layout-change`, `%session-changed`, …) are still lifted, on every remote
  version: 3.3a–3.8 emit genuine notifications inside blocks (#276) and the
  reader does not know the remote's version. Pane content alone can forge
  them. Pre-existing; filed as a follow-up.
- **Guessed guards.** A `%begin`'s `time` is the current second and `number` a
  server-global counter, so a program printing into a remote pane could spray
  guessed `%end <t> <n> 1` rows hoping one lands inside a capture of its own
  pane while that capture's block is open. A hit needs the exact counter value
  (no format exposes it) and closes that block early: its remaining rows become
  top-level lines — a row that parses as a verb, `%exit` included, is then acted
  on — while the reply count is unaffected, because the real `%end` is then a
  top-level guard, which is dropped.
- **A remote whose libc regex differs** (e.g. macOS) is unmeasured; the reader
  bounds memory and keeps a newline inside a reply body from opening or closing
  a block, but cannot hold a raw newline in a top-level `%subscription-changed`
  value, so a regex that does not match still forges top-level lines.

## Non-goals

No change to reply ordinals/barriers (`stream.claim`), no round-trip
deadlines, no remote version gating, no change to the local-server commands.

## Acceptance mapping

- Parser tests (`controlmode`): body over `MaxBody` → that block is
  `Error`/`ErrReplyTooLarge`, the next block parses with its own fields; a
  5 MiB line (top level and in-block) → stream continues; bodies holding
  `%exit`, a non-matching `%end`, `%subscription-changed` → no `Exit`, no early
  close, no `SubscriptionChanged`; genuine in-block `%exit` before EOF still
  emitted.
- Daemon test: the same forged body through `Reader` + `claimSeq` keeps
  ordinals exact.
- Memory test: a 256 × 1 MiB reply streamed through `Next()` allocates a
  bounded amount (`TotalAlloc` delta well under 64 MiB) and fails only that
  request. For a block of 256 × ~1 MiB in-block `%output` lines, which `Next()`
  returns one at a time, cumulative allocation is ~256 MiB by design (each line's
  `Data` is fresh), so that variant asserts peak live heap instead: `HeapInuse`
  after a GC, sampled across the `Next()` calls while each line is dropped.
- Live scratch-tmux Go test (`OG_REQUIRE_TMUX`): an option holding
  `\n%exit\n%end …\n%subscription-changed …` read through `ctlSafe` in both a
  `display-message -p` and a subscription arrives as one line; read raw, the
  reader keeps the forged lines as body.
- `nix flake check`, `nix build .#lint`.

## Docs

`docs/agents/bridge-daemon.md`: reader caps, strict guard matching, the
in-block verb rules, the residuals. `docs/agents/bridge-shipped-state.md`:
the `ctlSafe` newline policy and inventory, replacing the "tracked in a
separate issue" sentence under og-open.
