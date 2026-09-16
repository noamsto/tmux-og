# Bridge reconcile on tmux v2 (JSON) layouts — design

Issue #645. Pin `e880cf6` (#652) carries tmux/tmux#5390: `bf43fdc0` (JSON
layout format with floats), `d9692f7e` (`refresh-client -f new-layouts`),
`01d1d658` (JSON parser), `b7c50300` (v1 nesting limit).

## Measured on the pinned binary (`tmux next-3.9`, scratch `-L` server)

| # | Probe | Result |
|---|-------|--------|
| M1 | `#{window_layout}` from a **CLI** client (`display -p`) | **v2 JSON**, floats included as leaves with `"z"` |
| M2 | same from a **control** client, no flag | v1, **tiled only** — floats omitted entirely (compat copy collapses single-child splits, root offsets 0) |
| M3 | same from a control client after `refresh-client -f new-layouts` | v2 JSON with floats |
| M4 | `%layout-change` after the flag | both layout fields are v2 JSON (no spaces inside, so the space-split of the notice still works) |
| M5 | `select-layout` of a **v1 tiled-only** string on a window holding floats (daemon-style *and* an extra user float, zoomed or not) | **accepted**: tiled panes reshaped, every float kept at its geometry, window unzoomed |
| M6 | `select-layout` of **v2 JSON** whose cells match the local pane set incl. floats | accepted; also rewrites float geometry, **active pane**, last-pane stack and z-order |
| M7 | `select-layout` of v2 JSON with fewer cells than local panes (e.g. a local float the JSON does not name) | **rejected**: `have 3 panes but need 2` |
| M8 | float cell geometry in JSON | inner box, identical to the old trailing `<…>` section (`new-pane -x40 -y10 -X5 -Y3` → `38x8` at `6,4`; `-B none` → no inset) |
| M9 | zoomed window | `#{window_layout}` JSON is the saved (unzoomed) tree incl. floats |
| M10 | `refresh-client -f <unknown>` | silently ignored by `server_client_set_flags` (source) — safe to send to an older remote |

Pane→cell assignment in a v2 `select-layout` is by the JSON `"i"` index
against the local window's pane list order, floats included
(`layout_assign_from_ctx`), and the pane count checked includes floats
(`window_count_panes(w, with_floating = version > 1)`).

## Consequences

1. **The pin bump already broke two things silently.** Over a control client
   without the flag (M2) the remote now reports no floats at all, so no remote
   float is mirrored. And `localCellsMatch` reads the local window with a CLI
   client (M1), gets JSON, fails `ParseLayout`, and always misses.
2. **Feeding the remote's JSON verbatim to the local `select-layout` is wrong.**
   It needs the local pane set, floats included, to match the JSON cell for
   cell (M6/M7), which never holds while the user has a float of their own open
   over the mirror (#535), while a float add/remove is still in flight, or above
   `maxMirroredFloats`. When it does hold it assigns by the *remote's* pane
   index against the *local* pane list order, and moves local focus, last-pane
   history and z-order — focus-follow is deliberately structural-only today.
   Rewriting indexes and stripping `a`/`l` to make it fit is a second
   serializer that buys nothing over (3).
3. **The v1 tiled-only string is the float-tolerant `select-layout`** on the
   pinned local server (M5). That is the fix the workarounds were waiting for:
   they existed because the *local* `select-layout` refused a window holding
   floats, not because of anything the remote sent.

## Decisions

### D1 — Opt in on every attach

Send `refresh-client -f new-layouts` on each new control connection before
anything reads a layout: first attach in `Run`, `reattach`, and `replaceConn`.
Flags are per control client, so a reattach or replacement that skipped it
would silently lose float mirroring again. Sent on the unbound, not yet
verified connection as a claimed round-trip (the same posture as
`primeClient`: nothing drains an unbound pump, and an unclaimed reply desyncs
the ordinal count). Harmless on an older remote (M10).

### D1b — `%layout-change` notice shape

`parseLayoutNotice` (`daemon/layoutnotice.go`) accepts a layout field only
when it looks like a v1 dump (4 hex + `,`). After D1 every notice carries JSON
(M4), so as written every notice would fall back to a read and #570's
zero-round-trip paths would vanish without an error. `layoutShaped` must also
accept a v2 field (prefix `{"V":`), with a notice-parse test using a real
captured v2 `%layout-change` line, zoomed and not.

### D2 — `ParseLayout` sniffs the format

Input starting with `{` parses as v2 JSON; otherwise as v1 exactly as today,
trailing `<…>` float section included. No version probe, no gate: the string
is self-describing, which is what "not probe-able" in the issue resolves to.

For v2:
- decode `{"V":2,"L":cell}`; reject `V != 2`, unknown `t`, a pane with `c`,
  a node with fewer than two children, a tree with no tiled pane;
- a leaf with `"z"` is a float → `Layout.Floats` (inner box, unconverted);
- floats are pruned out of the tree and single-child splits collapsed, exactly
  as the v1 float path does today (reuse `pruneFloats`);
- `Layout.Panes` stays the depth-first tiled leaf order;
- `Layout.Raw` is the **v1** serialization of the pruned tiled tree with a
  recomputed checksum (reuse `writeCell` + `layoutChecksum`). `Raw` remains
  the string fed to local `select-layout` and compared against
  `mirrorWindow.layout`; its meaning does not change;
- `I` gives the pane id (`%N`), validated as `%` + digits;
- nesting depth is bounded (1000, matching tmux's v1 limit) so a hostile or
  corrupt remote cannot drive unbounded recursion in the walk.

No fixed dump buffer exists on the pin (`layout_string` grows), and v2 is
~61 bytes/pane (≈3.4x v1); the reader's 4 MiB line cap puts the ceiling far
beyond any real window, so no special handling.

`W,H` = root cell size. `a`, `l`, `i`, `z` values are not surfaced: nothing
consumes them (z-order mirroring is out of scope).

### D3 — Old remotes

- **Remote predating `bf43fdc0` but with floats (older next-3.8):** sends v1
  with the trailing `<…>` section. Still parsed; floats still mirrored. The
  trailing-section branch therefore stays — it is live for these remotes, not
  dead code.
- **Remote 3.7 and older:** no floats, plain v1. Unchanged.
- **New remote where the flag somehow did not land:** v1 tiled-only, floats not
  mirrored. Degrade (nothing to mirror is reported); no workaround.

None of these need the local workaround path, because the workaround was about
the local server (Consequence 3).

### D4 — Local server requirement

The local mirror server must be at the pin (it ships with tmux-og). A server
predating it that is still resident after a nix switch (#407) keeps the old
`select-layout`, which refuses a v1 tiled-only string while any float is open.
What that leaves, path by path:

- **Reshape behind a float** (mirrored or the user's): `applyLayout` returns
  ok=false, the broadcast is skipped, the mirror keeps its last-good screen.
  Today the drop would have rescued a mirrored-float-only window, and
  `localCellsMatch` (still v1 on such a server) short-circuited the common case
  behind a user float; now any layout change behind any float fails there,
  including one the window fit alone would have satisfied. The next remote layout change retries; nothing retries on a local float
  closing (retryFailedShapes is gone).
- **Rebuild** (`resetWindow` → `setupWindow`): mirrored floats are still
  dropped first (D5), so only a *user* float blocks `PlanWindow`'s
  `select-layout` — identical to today's behavior on that path.
- **Split/kill via applyPaneOps**: same as reshape.

No pane is lost and no session is torn down on any of these; the degradation is
a stale shape until the next remote change. Accepted and stated in the PR and
CLAUDE.md; restarting the server resolves it. No local version gate.

### D5 — Delete the workarounds

- `localCellsMatch` and its call (already broken by M1).
- `applyLayout`'s drop-mirrored-floats step (`floatsDropped` itself stays, see the rebuild bullet).
- `retryFailedShapes`, `localWindowHasFloat` and `mirrorWindow.shapeFailedFor`
  (and the sweep's call). A `select-layout` failure is logged each time; with
  floats no longer a cause it is not expected to repeat.
- `reconcileLayoutFrom`'s `len(w.localFloats) > 0` fall-through: a
  geometry-only reshape behind a mirrored float now applies straight from the
  notification (floats unchanged is already gated by `noFloatWork`), keeping
  #570's zero-round-trip path for that case too.
- **Rebuilds still drop mirrored floats.** `dropMirroredPanes` keeps killing
  them and raising `floatsDropped`, and `retireOrRestoreFloats` keeps its
  re-add. The original reason (setupWindow's `select-layout` failing with them
  present) is gone, but `healDeadRenderers` repairs a dead *float* renderer by
  calling `resetWindow`, and it is the rebuild's float teardown + re-add that
  heals it. Keeping floats across a rebuild would need a live/dead carve-out
  for no user-visible gain, so the rebuild path is left as it is and only its
  comments are corrected. `floatsDropped` is now raised only there; applyLayout
  no longer reads or writes it.

`reconcileFloats` (the add/move/remove diff), `mirrorableFloats`,
`floatCellsEqual`, `noFloatWork` and the two geometry spaces are unchanged:
creating, moving and killing mirrored floats is still done with
`new-pane`/`resize-pane`/`move-pane`/`kill-pane` in the outer box.

### D6 — Invariants kept

- zoom asserted via idempotent `if -F` after `applyLayout`, before dims/reseed;
- `#{window_layout}` is the unzoomed geometry (M9 confirms for JSON);
- every pane reseeded after a reshape, after `FitWindowCmd`/`select-layout`
  (#233/#417);
- `reconcileLayoutFrom`'s zero-round-trip paths (#570);
- a float the daemon did not create is never killed (#535) — now also never an
  obstacle;
- window addressed by id (#411).

## Out of scope

Mirroring float z-order; tmux-remux's own layout save/restore, which reads
`#{window_layout}` from a CLI client and now gets JSON on the pin (follow-up
check, not this repo's code); using the JSON `"a"` field to skip the active-pane
read; `healDeadRenderers` (#657); client capability detection (#649).

## Verification

- `controlmode` unit tests: v2 nesting (h inside v inside h), floats with `z`
  pruned and collapsed (single tiled pane + floats → leaf root), zoomed dump,
  `Raw` equals tmux's own v1 compat dump for the same window (captured strings
  from M2/M3 on one scratch window), malformed inputs (bad `V`, pane with `c`,
  single-child node, missing fields, bad id, depth bomb), v1 + trailing float
  section unchanged.
- `layoutnotice` tests with captured v2 `%layout-change` lines (zoomed flag
  `*Z`, and not).
- daemon unit tests updated/removed for the deleted paths; a scripted test that
  a geometry-only notice behind a mirrored float applies with no round-trip.
- `remote-m2-integration.bats`: a remote float is mirrored as a local float (the
  pin-bump regression), and a remote reshape behind a *local user float* (the
  #535 case) reshapes the mirror (pane dims match the remote) rather than
  freezing.
- `nix flake check`, `nix build .#default`, `nix build .#lint`.
- CLAUDE.md: rewrite the float bullet in Key Conventions and drop the
  `retryFailedShapes` references.
