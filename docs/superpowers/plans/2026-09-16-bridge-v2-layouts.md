# Bridge reconcile on tmux v2 layouts — plan

Spec: `docs/superpowers/specs/2026-09-16-bridge-v2-layouts-design.md` (read it
first; D-numbers below refer to it). Issue #645.

Scope: `picker/remotebridge/controlmode/**`, `picker/remotebridge/daemon/**`
(layout/reconcile/attach only), `tests/remote-m2-integration.bats`,
`CLAUDE.md`. Do **not** touch `graphics/relay.go` or `watchLocalClient`
(parallel worker #649), nor `healDeadRenderers` (#657).

Pinned tmux for probes/fixtures (raw binary, no config):
`/nix/store/8qkjbhdqwc8nx39fcrjcjc9l8x58dp9w-tmux-wrapped/bin/.tmux-wrapped`.
Always `TMUX_TMPDIR=/tmp/og645-<x> … -L <name> -f /dev/null`, `kill-server`
when done, never touch the live server.

Order: Task 1 ∥ Task 2 ∥ Task 3 → Task 4 → Task 5 → Task 6 → gates.

---

## Task 1 — `ParseLayout` understands v2 JSON (D2)

Files: `picker/remotebridge/controlmode/layout.go`, `layout_test.go`.

- [ ] **Step 1.1: capture fixtures.** On a scratch server (100x30), build each
  window below and record, for each, the CLI `display -p '#{window_layout}'`
  (v2 JSON) **and** the control-client v1 dump without the flag
  (`printf 'display -p -t a "#{window_layout}"\n' | tmux … -C attach -t a`),
  plus `list-panes -F '#{pane_id} #{pane_floating_flag} #{pane_width}x#{pane_height} #{pane_left},#{pane_top}'`.
  Windows:
  a. 2 tiled panes `h`, no floats;
  b. nested: `h{p, v{p,p}}` + one float created *after* the split so it sits as
     a direct root child;
  c. a float nested inside a split whose pruning leaves one child — produce it
     by creating the float, then `select-layout` with a JSON (built from the
     captured dump) that places the float leaf inside the inner `v`
     alongside one tiled pane; if tmux will not emit that placement, hand-build
     the JSON (tmux's own parser accepts floats at any depth) and state so in a
     test comment;
  d. one tiled pane + two floats (root becomes a node; pruned → leaf root);
  e. zoomed version of b (`resize-pane -Z`) — JSON is the saved tree;
  f. a `-B none` float.
  Paste the literal strings into table-driven tests; no runtime tmux in unit
  tests.
- [ ] **Step 1.2: tests first.** For every fixture assert: `Panes` (ids + cells,
  depth-first tiled order), `Floats` (ids + inner-box cells, matching
  list-panes), `W,H`, and **`Raw` == the captured control-client v1 dump
  byte-for-byte** (checksum included). Error cases: `{"V":1,…}`, `V` missing,
  unknown `t`, pane with `c`, node with one child, node with zero children,
  missing `w/h/x/y/I`, `I` not `%`+digits, only floats (no tiled pane), JSON
  trailing garbage, and a depth bomb (nesting 1001 deep → error, no panic).
  Existing v1 tests (incl. trailing `<…>` section) stay green unchanged.
  If `Raw` and the captured dump disagree, tmux's compat output is the
  authority (M5: local select-layout accepts it): fix `pruneFloats` or add a
  v2-only fix-up until they match; never loosen the assertion.
- [ ] **Step 1.3: implement.** In `ParseLayout`, after trimming, if the input
  starts with `{` call a new `parseLayoutV2(s)`; else the existing v1 path
  unchanged. `parseLayoutV2`:
  - `encoding/json` into `struct{ V *int; L json.RawMessage }` (check `V != nil && *V == 2`), then decode cells recursively from `json.RawMessage` into a
    `jsonCell{T string; W,H,X,Y *int; C []json.RawMessage; I string; Z *int}`
    so depth is checked *before* descending (depth > 1000 → error);
  - build the existing `*node` tree (`kind` `{` for `h`, `[` for `v`; leaf
    `id` = `I` after validating `%`+digits); collect float leaves (`Z != nil`)
    into `Floats` in tree order and their ids into the set;
  - `pruneFloats` + `writeCell` + `layoutChecksum` → `Raw` (always
    re-serialized, even with no floats: the input is JSON, not select-layout-safe v1);
  - `collectLeaves` → `Panes`; error if empty.
  Update the `Layout` field comments and the `ParseLayout` doc: two input
  formats; `Raw` is always v1 tiled-only; why v1 (spec Consequence 2/3, one
  short paragraph — local select-layout keeps floats for a v1 string, a v2 one
  must name every local pane). Keep comment density like the file.
- [ ] **Step 1.4:** `cd picker && go test ./remotebridge/controlmode/...`
  green; `go vet ./remotebridge/controlmode/...`.

## Task 2 — `%layout-change` notice accepts v2 fields (D1b)

Files: `picker/remotebridge/daemon/layoutnotice.go`, `layoutnotice_test.go`.

- [ ] **Step 2.1: capture** a real `%layout-change` line with the flag set
  (control client: `refresh-client -f new-layouts`, then from another CLI
  `split-window`), once plain and once with the window zoomed (flags `*Z`).
- [ ] **Step 2.2: tests first**: both lines parse ok with `layout` = field 1
  verbatim and correct `zoomed`; a JSON field not starting with `{"V":` is
  rejected; existing v1 cases unchanged.
- [ ] **Step 2.3: implement**: `layoutShaped` returns true for
  `strings.HasPrefix(s, "{\"V\":")` or the existing v1 shape. Update its doc
  line.

## Task 3 — opt every control client into new layouts (D1)

Files: `picker/remotebridge/daemon/sessionpin.go` and the tests whose
scripted replies model the identity read.

Design: the identity read leads every attach (Run via `newSessionPin`,
`reattach`, `replaceConn`) and runs on the unbound connection before any layout
read, so it is the one place that covers all three. Measured: a
`cmd1 ; cmd2` line still yields two reply blocks, so this is two commands in
one batch.

- [ ] **Step 3.1: implement.** In `readIdentity`, issue
  `rt("refresh-client -f new-layouts", <identity cmd>)`; claim the first reply
  and ignore its content (an `%error` from an odd remote must not fail the
  attach — a remote that cannot take the flag simply reports v1); if the first
  claim finds no reply (connection closed), return the same retryable
  `identityReadErr` shape the identity read uses. Then parse the second reply
  exactly as today. Doc comment: why here (leads every attach, flags are per
  control client, M2 float loss without it).
- [ ] **Step 3.2: fix scripts.** Every scripted reply that models the identity
  read gains a preceding empty block for the flag. `identityMatch` in
  `reattach_test.go` and the literal ones in `sessionpin_test.go`,
  `replaceconn_test.go`, `reattach_test.go`, plus any in `daemon_test.go`/other
  `Run` tests (find by running `go test ./remotebridge/daemon/...` and by
  `rg '\|\$[0-9]+\\n%end'`). Reply-block numbering in scripts is by position
  (ordinal count), so an empty `%begin 1 0 1\n%end 1 0 1\n` block is enough.
  Add one assertion (sessionpin_test) that the sent stream contains
  `refresh-client -f new-layouts` **before** the identity command, and one that
  an `%error` reply to the flag still yields the parsed identity.
- [ ] **Step 3.3:** `go test ./remotebridge/daemon/...` green.

## Task 4 — delete the workarounds (D5) (implement: escalated)

Files: `picker/remotebridge/daemon/reconcile.go`, `windows.go`,
`reconcilewindows.go` and their tests. Tagged escalated: wide deletion across
the reconcile state machine with invariants documented only in comments.

- [ ] **Step 4.1: `applyLayout`.** Remove the `localCellsMatch` branch, the
  float-drop block, and every `shapeFailedFor` read/write; on select-layout
  error log every time and return false. Keep `FitWindowCmd` first,
  `L.Raw == w.layout` short-circuit, `appliedZoom=false`, `w.layout = L.Raw`.
  Rewrite its doc comment: fit then shape; a v1 tiled-only string keeps local
  floats (the user's and ours) in place on the pinned local server; ok=false
  only on select-layout failure (still gates the broadcast); note D4 (pre-pin
  resident local server fails behind any float, keeps last-good screen).
- [ ] **Step 4.2:** delete `localCellsMatch`. Delete `sortedFloatIDs` only if
  it has no remaining caller (resetWindow/dropMirroredPanes still use it — keep).
- [ ] **Step 4.3: `reconcileLayoutFrom`.** `if n.zoomed || len(w.localFloats) > 0`
  → `if n.zoomed`; fix the comment above it (no drop/re-add any more).
- [ ] **Step 4.4: `reconcileSnapshot`.** Keep `w.floatsDropped = false` at the
  loop head but reword its comment (the only remaining raiser is a rebuild's
  `dropMirroredPanes`). Fix the "applyLayout may drop a float" references in
  the loop-exit and post-loop comments.
- [ ] **Step 4.5: `retireOrRestoreFloats` / `dropMirroredPanes` comments.**
  Behavior unchanged. Remove the "applyLayout kills them" route; the rebuild's
  float teardown now stands on its own reason: rebuild from scratch, which is
  also what heals a dead float renderer via `healDeadRenderers`.
- [ ] **Step 4.6: `windows.go`.** Delete `shapeFailedFor`; reword
  `floatsDropped` doc (raised only by a rebuild's drop; tells a failing exit it
  owes a re-add).
- [ ] **Step 4.7: `reconcilewindows.go`.** Delete `retryFailedShapes`,
  `localWindowHasFloat` (verify no other caller with `rg`), and the call in
  `windowSweeper.sweep`. Leave `healLostWindows`/`healDeadRenderers` untouched.
- [ ] **Step 4.8: tests.** Delete `retryshape_test.go`. In
  `reconcilelayout_test.go`, `reconcilefloatteardown_test.go`,
  `reconcilenotice_test.go`, `reconcileordering_test.go` remove or rewrite
  cases that assert the drop, the cells short-circuit, or `shapeFailedFor`
  (read each first; convert a case to the new behavior where it still says
  something true — e.g. "reshape behind a mirrored float issues select-layout
  and kills no float"). Add:
  - a scripted test: a geometry-only `%layout-change` (same panes, same floats)
    on a window holding a mirrored float applies from the notice with **zero**
    remote round-trips, issues the local select-layout, and kills no float;
  - a test that a select-layout failure behind a mirrored float kills no float
    and returns ok=false (broadcast skipped).
  Fake local tmux in these tests: follow the existing reconcile test fakes.
- [ ] **Step 4.9:** `cd picker && go build ./... && go vet ./remotebridge/... && go test ./remotebridge/...` green; `rg -n "shapeFailedFor|localCellsMatch|retryFailedShapes|localWindowHasFloat" picker` empty.

## Task 5 — integration coverage (`tests/remote-m2-integration.bats`)

Uses the pinned tmux on both servers (as in flake). Follow the existing test
shape (wait for renderer, poll loops, capture before kill).

- [ ] **Step 5.1: "a remote float is mirrored as a local float".** SRC 100x30
  one pane; start daemon; wait renderer; `$SRC new-pane -d -t rem -x 40 -y 10
  -X 5 -Y 3`; poll DST `list-panes -F '#{pane_floating_flag} #{pane_width}x#{pane_height}'`
  until a floating pane exists whose dims equal SRC's float's dims. Assert.
  (Fails on main post-#652: no float reported over the control client.)
- [ ] **Step 5.2: "a reshape behind a local user float applies (#535)".** SRC
  two panes `-h`; daemon; wait both renderers; open a **local user float** on
  DST in the mirror window (`$DST new-pane -d -t host-sess:1 -x 20 -y 5`);
  `$SRC resize-pane -t rem.1 -x 30`; poll until `sorted_dims` of DST's
  *non-floating* panes equals SRC's; assert, and assert the user float still
  exists. Use a `-F '#{?pane_floating_flag,,#{pane_width}x#{pane_height}}'`
  variant (skip empty lines) for the tiled-only comparison.
- [ ] **Step 5.3:** run locally:
  `nix build .#checks.x86_64-linux.remote-m2-integration-tests -L` (or bats
  directly with DAEMON/RENDERER/CTL built from `picker`). Green, including the
  pre-existing "no-op %layout-change costs the remote nothing" (validates
  Task 2). `shellcheck`/`shfmt` clean via `nix build .#lint`.

## Task 6 — docs

- [ ] **Step 6.1: CLAUDE.md** Key Conventions float bullet ("A remote float is
  mirrored as a local float…"): rewrite to the new mechanism — daemon opts in
  with `refresh-client -f new-layouts` on every attach (inside the identity
  read); `ParseLayout` takes v2 JSON (floats = `"z"` leaves) or v1 (+ trailing
  `<…>` for older next-3.8 remotes); `Raw` is always v1 tiled-only because the
  pinned local `select-layout` keeps floats for a v1 string while a v2 string
  must name every local pane, floats included (so verbatim JSON breaks on a
  user float and moves focus); the two geometry spaces paragraph stays; delete
  the short-circuit / drop-and-re-add / `retryFailedShapes` /
  "pending upstream" text; add the D4 degradation in one sentence. Also drop
  `retryFailedShapes` mentions elsewhere in CLAUDE.md (`rg`), and note in the
  "Bridge Reconnect" repair order nothing if unaffected.
- [ ] **Step 6.2:** remove the tracked `WORKER_TASK.md`? No — it is untracked;
  leave it out of commits.

## Gates

- `nix build .#default`, `nix flake check` (includes
  `remote-m2-integration-tests`, `picker-go-tests`), `nix build .#lint`.
- Commit from inside the devshell (pre-commit), conventional message
  `feat(bridge): adopt tmux v2 JSON layouts in reconcile (#645)`; spec + plan
  committed with the code.

## Acceptance

- [ ] `%layout-change` and `readLayout` carry JSON after attach, reattach and
  replacement; floats mirrored again on the pin.
- [ ] `ParseLayout` parses v2 (nesting, floats with z at any depth, zoom) and v1
  (+ trailing floats); `Raw` equals tmux's v1 compat dump.
- [ ] #570 zero-round-trip notice paths still hit (existing bats + new unit test).
- [ ] Reshape behind a user float applies (bats 5.2).
- [ ] Workarounds deleted; rebuild float teardown kept; CLAUDE.md updated.
