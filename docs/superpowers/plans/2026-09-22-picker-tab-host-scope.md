# Picker: Tab cycles host scope, search all remote sessions (#733)

## Context

`picker/tui.go` is a bubbletea TUI (`tuiModel`). Today's session list
(`m.sessionItems`) is local sessions (incl. mirror sessions tagged
`bridgeHost`); `m.remoteItems` is a per-host "Remote" tree built by
`collectRemoteItems` (picker/remote.go), which **drops any remote session
already bridged locally** (`bridgeSessionPresent`). `recombine()`
(picker/tui.go:1765) concatenates `sessionItems + remoteItems + zoxideItems`
into `m.allItems`; `withFilter()` applies the query/mode filter into
`m.visible`.

`Tab` is currently unbound (picker.md already reserves it: "Tab is reserved
for an upcoming host-scope filter"). This plan wires it to cycle a **host
scope** that changes what `recombine()` puts in `allItems`, without touching
`withFilter()`, mark handling structure, or the async data-fetch cadence
(remote ssh probes still fire once at `Init()` / after
`remoteAuthDoneMsg` — **no new ssh calls**).

Key existing facts to build on:
- `parseRemoteHosts` + `dropCachedSelfAliases` give the configured host list
  in `@remote_bridge_hosts` order (picker/remote.go:102, :258) — reuse this
  exact list for both Tab's cycle order and all-scope's grouping order.
- `m.remoteItems` **is already** the "paint-from-cache then replace"
  (#631) data the task calls for: `pendingRemoteItems` fills it from the
  on-disk cache synchronously on first paint, and `collectRemoteItems`
  replaces it once the (one-shot, `Init()`-only) probe answers. Host/all
  scope's Remote-section rows are built by **reusing `m.remoteItems`
  verbatim** (see Design), not a second cache read — see the "Mirror
  lookup" section's revision note for why an earlier cache-rebuild design
  was rejected.
- `cachedRemoteSessionRows` (picker/remote.go:1038, used inside both
  `pendingRemoteItems` and `collectRemoteItems`) **skips** bridged sessions
  (`bridgeSessionPresent`) — this is *why* `m.remoteItems` alone doesn't list
  a host's bridged sessions, and why host/all scope has to append rows for
  them separately (from `m.mirrors`, see Design) rather than needing a
  cache-reading sibling of this function.
- `hostColorFunc` (picker/remote.go:877) already tints a host's row, its
  mirror sessions' names, and its Remote-section rows/children consistently
  — reuse verbatim, no new palette logic.
- `collectBridgeSessions()` (picker/remote.go:409) gives a bool set (is this
  host+sess bridged), not *which* local session mirrors it. We need the
  latter to resolve Enter on an already-mirrored remote row to a
  `switch-client`.
- Test idiom: `tuiModel{allItems: fixture}`, `m.withFilter()`,
  `m.handleKey(wallKey("..."))`, `findVisible(t, m, predicate)` (see
  picker/tui_test.go:1551-1573, remote_cache_test.go for cache-seeding
  helpers `useRemoteCache`/`seedRemoteCache`).

## Design

### Scope state

```go
type scopeKind int8

const (
    scopeLocal scopeKind = iota
    scopeHost
    scopeAll
)

type hostScope struct {
    kind scopeKind
    host string // set only when kind == scopeHost
}
```

Add `scope hostScope` to `tuiModel` (zero value = `scopeLocal` — today's
exact behavior when Tab is never pressed, satisfying the "no behaviour
change" acceptance bullet for free). Reset to `hostScope{}` wherever the
popup is freshly launched (`newPickerModel`) — it already zero-inits, so no
explicit reset code is needed there; just don't let anything persist it
across a relaunch.

### Host list + cycle order

Add `configuredHosts(tmuxOpts map[string]string) []string` in remote.go:
`dropCachedSelfAliases(parseRemoteHosts(envOrMap("REMOTE_BRIDGE_HOSTS",
tmuxOpts, "@remote_bridge_hosts", "")))` — the exact call
`pendingRemoteItems` (`remote.go:987`) already makes; factor it out and call
it from both places.

Cycle: `local -> hosts[0] -> hosts[1] -> ... -> hosts[n-1] -> all -> local`.
Zero configured hosts: Tab is a no-op (nothing to cycle to — stay local).

### Tab key handling (picker/tui.go, `handleKey`)

New `case "tab":` (not `printableKeyText` — bubbletea reports it as `"tab"`,
distinct from the space-name pitfall). A no-op when `m.windowMode` or
`m.emitPath != ""` (round-2 note): `m.remoteItems` is only populated for
the plain interactive session picker (`Init()`'s `!m.windowMode` guard,
`tui.go:312-320`) — window mode's rows carry no `bridgeHost`/remote data at
all, and emit mode explicitly skips the remote probe (spec D8). Scope
cycling has nothing to build from in either mode; say this in picker.md
rather than let Tab silently produce an empty/orphaned list there.
Otherwise: compute next scope from `configuredHosts(m.tmuxOpts)` + current
`m.scope`, set it, then rebuild: `m = m.recombine().withFilter()`,
`m.cursor = m.firstSelectable(0)`, return `m, m.loadPreviewCmd()`. Query
text is preserved across the Tab press (same as `^a`/`^s` mode toggles
don't clear it) — "filter typing searches within that scope" per the spec.

### Mirror lookup (new) — `[]bridgeMirror`, not an opaque key map

**Revision note (plan-critic round 1):** the original design read
`readRemoteSessionCache(host)` inside `recombine()` for every scoped host on
every tick, rebuilding that host's whole remote-session tree from the cache
and threading probe state through by hand. The critic found this fabricates
Enter-able/markable rows under a host the probe called needs-auth /
host-key-changed / tailscale-check (`collectRemoteItems` deliberately emits
**no** children for those states — `remote.go:1113-1122`,
`TestCollectRemoteItemsSpecialStatesIgnoreCache`), drops restorable
(`remoteRestore`) rows, and has no real mechanism to read a host row's
"unreachable" state (`listItem` has no such field on host rows). Replaced
with the design below, which reuses `m.remoteItems` verbatim instead of
rebuilding from cache, so every existing probe-state invariant carries over
for free.

Add to remote.go:

```go
// bridgeMirror is one local session already mirroring a remote host+session.
type bridgeMirror struct {
    host, sess, target string // target = the local mirror's session name
}

// parseBridgeMirrors is the pure parse of `tmux list-sessions -F
// "#{session_name}|#{@bridge_host}|#{@bridge_session}"` output into the
// mirrors it describes. A row with no @bridge_host isn't a mirror. A row
// with @bridge_session empty (legacy bridge, no option set) derives sess by
// stripping the "<host>-" prefix off the session name — same convention
// localBridgeSession/sessionDisplayName already assume; a name that doesn't
// carry the prefix is skipped (nothing to resolve).
func parseBridgeMirrors(raw string) []bridgeMirror

// collectBridgeMirrors runs the tmux call. Off-thread only — see below.
func collectBridgeMirrors() []bridgeMirror
```

One `tmux list-sessions -F "#{session_name}|#{@bridge_host}|#{@bridge_session}"`
call (same triplet `collectBridgeSessions`/`bridgeSessionNames` already read
independently — do not try to unify all three call sites in this change,
that's a separate cleanup).

This call must run **off the bubbletea update thread** (same rule as every
other tmux/ps fork here) — do not call it synchronously from `recombine()`
or `handleKey`. Fold it into `refreshDataCmd()`'s existing async closure
(picker/tui.go:1723), which already forks `collectPanesSnapshot`/
`collectAgentPanes` every 1s tick off-thread, but **only when `!wm`**
(`refreshDataCmd` already captures `wm := m.windowMode` for its own branch)
— window mode never reads `m.mirrors` (Tab is a no-op there, see the Tab
key handling section below), so gate the extra fork on the same flag rather
than paying for an unread result every tick in window mode too:

```go
type refreshMsg struct {
    items   []listItem
    mirrors []bridgeMirror
}
```

populate it in `refreshDataCmd`'s closure, and store `m.mirrors =
msg.mirrors` in the `case refreshMsg:` handler in `Update()` before
`recombine()`. `m.mirrors` is `nil` until the first tick answers —
`recombine()`'s host/all branch must tolerate that (a nil slice ranges as
empty, so this is naturally fine).

### listItem: one new field

```go
remoteMirrorTarget string // remote session row: local mirror session name already open for this host+session (host/all scope only, synthesized by scopedItems from m.mirrors) — Enter switches here instead of opening a duplicate
```

### recombine() scope branch (picker/tui.go:1765)

```go
func (m tuiModel) recombine() tuiModel {
    switch m.scope.kind {
    case scopeHost:
        m.allItems = m.scopedItems(func(h string) bool { return h == m.scope.host })
    case scopeAll:
        configured := hostSet(configuredHosts(m.tmuxOpts))
        m.allItems = m.scopedItems(func(h string) bool { return configured[h] })
    default:
        // existing body, unchanged
    }
    return m
}
```

`scopeAll`'s `hostMatches` is **membership in `configuredHosts(m.tmuxOpts)`**
(round-2 finding B2 — the Design and Step 2 previously gave two different
answers, `h != ""` vs. `configuredHosts`, which disagree whenever
`m.mirrors` contains a host outside `configuredHosts`, e.g. an ad-hoc bridge
or one `dropCachedSelfAliases` dropped). This is the same list Tab's cycle
uses, so "all hosts" in scope means exactly the hosts Tab can land on
individually. A mirror in `m.mirrors` whose host isn't in `configuredHosts`
is dropped in every scope (host scope already can't reach it either, since
`m.scope.host` only ever comes from that same list) — no mirror row is ever
emitted without its host's row present above it.

`scopedItems(hostMatches func(string) bool) []listItem`:
1. Local session rows: filter `m.sessionItems` to `hostMatches(item.bridgeHost)`
   — this drops plain local sessions and keeps only mirrors of a matching
   host. Reuses the rows exactly as built (Host-colored, killable, etc.) —
   **no new row type for these**, which is what makes "already-mirrored
   local sessions appearing in host scope" free.
2. Remote header + per-host trees: **reuse `m.remoteItems` verbatim as the
   base**, not a cache rebuild — this is the fix for round-1's blocking
   findings #1-#3. `m.remoteItems` is already `collectRemoteItems`'s output:
   one header row, then per matching host, its host row followed by
   whatever children that host's probe state earns it today (live unbridged
   sessions, or restorable rows, or cached-and-unreachable rows, or **no**
   children at all for needs-auth/host-key-changed/tailscale-check) — every
   existing invariant, including the inert-state gate, carries over
   unchanged because nothing about those rows is rebuilt.
   - **Assemble host-block by host-block, not by a single trailing append**
     (round-2 finding B1 — `markRemoteTreeEnds`, `tui.go:1624`, and
     `withFilter`'s query path are both adjacency/order-based; a flat
     "collect matching rows, then append every mirror row at the end" would
     interleave one host's mirror rows visually into a *different* host's
     block, breaking "grouped by host"). Iterate hosts in `configuredHosts`
     order (same list as Tab's cycle — see B2 below for why); for each host
     that `hostMatches`:
     1. Emit the single `isRemoteHeader` row, but only once, before the
        first host that has anything to show.
     2. Emit that host's row + children **exactly as they appear in
        `m.remoteItems`**, in their existing order (host rows are found by
        `remoteHost == host && remoteSess == ""`; children immediately
        follow until the next `remoteHost`-owning row). Track whether a
        host row was actually found (`hostRowFound`).
     3. Immediately after step 2, for this same host, **only when
        `hostRowFound`** (round-3 note: `configuredHosts` and
        `m.remoteItems` can disagree — `collectRemoteItems` drops a host
        live, after a self-host re-check, that `dropCachedSelfAliases`
        didn't catch on disk, `remote.go:1093-1100` — so a host from
        `configuredHosts` can have zero presence in `m.remoteItems`;
        skipping the mirror append then keeps the stated invariant "no
        mirror row is ever emitted without its host's row present above
        it" actually true instead of asserted), append its not-`seen`
        mirror rows from `m.mirrors` (`seen[sess]` = any child just
        emitted in step 2 with `remoteSess == sess`):
        `remoteSessionRowItem(host, bm.sess, "(mirrored)", hostColor(host), cDim, false)`
        with `.remoteMirrorTarget = bm.target` set. This runs **regardless
        of that host's current probe state** — including under an inert
        host: a mirror row's Enter is a plain local `switch-client`, never
        an ssh call or anything that "acts on" the remote host, so
        appending it does not violate "no row may make Enter act on a host
        the probe called inert". State this explicitly in code comments so
        a future reader doesn't "fix" it into matching the inert-children
        rule.
     4. Move to the next host — its block (row + children + its own
        mirrors) follows immediately, never interleaved with the previous
        host's.
   - Add an ordering assertion to the all-scope test (round-2 B1): every
     row with `remoteHost == A` is index-contiguous and precedes every row
     with `remoteHost == B`, for a fixture with both a live child and a
     mirror row on each host.
3. No zoxide rows in host/all scope (zoxide suggestions are a "create a
   session here" affordance with no host of their own — local-scope-only,
   simplest correct behavior; note this explicitly in picker.md).

Reuse `hostColor := hostColorFunc(m.tmuxOpts)` and `cDim` the same way
`pendingRemoteItems`/`collectRemoteItems` compute them.

### Marks resolve against an unscoped superset (round-1 finding #4)

`markedRemoteItems()` (`tui.go:1214`) currently resolves marks against
`m.allItems`, which host/all scope shrinks to one (or all-but-local) host's
rows — a mark set on host B in local scope would silently drop from the
batch if Enter fires while scoped to host A. Fix: change its source to a
concatenation of `m.sessionItems` and `m.remoteItems` (both always the full,
unscoped set regardless of current `m.scope`), never `m.allItems`. This is
safe with no further change: `markable` already requires
`item.remoteMirrorTarget == ""`, and the only rows that ever carry a
non-empty `remoteMirrorTarget` are the ones `scopedItems` synthesizes and
appends — they never exist in `m.remoteItems` — so no mirrored/appended row
can ever be marked in the first place, meaning `m.remoteItems` alone is
always a complete and correct superset for mark resolution. Add a
`markedRemoteItems` test asserting a mark set on a host that is not the
current scope still resolves.

### Enter / activateCurrent (picker/tui.go:1281) — test seam (round-1 finding #5)

`activateCurrent`'s plain-session branch execs `tmux switch-client` inline
today (`tui.go:1342`) with no seam a test can substitute — and the
acceptance criteria require asserting the mirror-switch call. Introduce one
package-level var, matching the existing `readLocalRemoteIdentity`/
`sshRemoteResources` seam pattern (`remote.go:135`, `remote_resources.go:120`):

```go
var switchClient = func(target string) error {
    return exec.Command("tmux", "switch-client", "-t", target).Run()
}
```

Route **both** the existing plain-target path and the new mirror path
through it, so the change is covered by whatever existing Enter tests
already exist for a plain session switch, plus the new mirror test:

```go
if item.remoteMirrorTarget != "" {
    logEvent("picker", "event", "switch_to_mirror", "target", item.remoteMirrorTarget)
    if err := switchClient(item.remoteMirrorTarget); err != nil {
        m.statusMsg = err.Error()
    }
    return m, tea.Quit
}
```

and replace the existing plain-target `exec.Command("tmux", "switch-client",
...)` call with `switchClient(item.target)`. Place the mirror branch before
the existing `if item.remoteHost != ""` branch.

**Preview text (round-2 note):** `loadPreviewCmd`'s remote branch
(`tui.go:1801-1837`) always renders "Enter runs og-remote-open (outbound
ssh)" for a `remoteHost != ""` row with no other flag set — wrong for a
mirror row, where Enter is a local `switch-client`. Add a case ahead of the
`default:` there: `item.remoteMirrorTarget != ""` → "remote bridge →
\<host\>/\<sess\>\n\nAlready mirrored locally — Enter switches to \<target\>."
`loadPreviewCmd` already copies every field it needs into locals *before*
building the returned closure (`host, sess := item.remoteHost,
item.remoteSess`, etc., `tui.go:1802-1804`) — do the same for
`item.remoteMirrorTarget` (e.g. `mirrorTarget := item.remoteMirrorTarget`),
not a bare `item.remoteMirrorTarget` reference inside the closure.

Marked-session batch-open (`activateCurrent`'s `markedRemoteItems()` branch,
which runs first) is unaffected — see `markable` below, mirrored rows are
excluded from marking so they never reach `openMarkedRemoteWith`. Note for
docs/tests: with ≥1 mark set, Enter on a mirrored row still opens the batch
(existing precedence, unchanged) rather than switching to that one mirror.

### markable (picker/tui.go:1200)

Add `&& item.remoteMirrorTarget == ""` to the existing condition — a
session that already has a live mirror has nothing to "open", so marking it
for batch-open makes no sense; Enter on it alone still switches.

### Inert-row invariant

`markable` already excludes `remoteInert`/`remoteNeedsAuth`/
`remoteTailscaleCheck`/`remoteUnreachable`. `activateCurrent`'s existing
inert checks (host-key-changed, tailscale-check) run on the **host row**.
Since `scopedItems` reuses `m.remoteItems`' rows for a host verbatim (not a
cache rebuild — see the mirror-lookup section above), every flag on every
row is exactly what `collectRemoteItems` already set, including "no
children at all" for needs-auth/host-key-changed/tailscale-check hosts —
this is a property that now genuinely requires no new logic, only a
regression test, because nothing rebuilds those rows. The one row type
`scopedItems` adds beyond what `m.remoteItems` already has — appended
mirror rows — is deliberately exempt from the inert gate (see above): its
Enter is a local `switch-client`, never anything that reaches the host.

### Header / footer

- **Search-row scope badge**: sibling of `withHostBadge`
  (picker/render_list.go:157), e.g. `withScopeBadge`, right-aligned same as
  the emit-mode host badge (don't stack both — emit mode and interactive
  scope cycling are mutually exclusive in practice, `emitHost` is only set
  in remote-pick mode where `@remote_bridge_hosts` scope cycling has no
  reason to be exercised, but keep the two independent/composable rather
  than asserting exclusivity in code). `scopeLocal`: nothing (today's exact
  render). `scopeHost`: host name tinted with `hostColorFunc(...)(host)`.
  `scopeAll`: a distinct label ("all hosts") in a neutral/highlight color
  (reuse the `@thm_peach`/highlight token `renderHints` already uses for
  toggled-on state, for visual consistency with `^a`/`^s`).
- **Footer hint**: add `hint("⇥", "scope")` in `renderHints`
  (picker/render_list.go:171), gated on `len(configuredHosts(m.tmuxOpts)) >
  0` (no hosts configured ⇒ nothing to cycle to ⇒ don't advertise it, same
  pattern as the existing `^g` group hint being gated on `m.windowMode`).
  When `m.scope.kind != scopeLocal`, highlight the hint like the other
  toggled-on hints (`agentLabel`/`scratchLabel`/`groupLabel` pattern).

## Steps

- [ ] **Step 1: scope types + listItem field (picker/tui.go) +
      `bridgeMirror` type (picker/remote.go)** Add `scopeKind`/`hostScope`,
      `tuiModel.scope`, `listItem.remoteMirrorTarget`, and the `bridgeMirror`
      struct itself (round-3 fix: `bridgeMirror` must exist before
      `tuiModel.mirrors []bridgeMirror` can compile — define the type here,
      leave `parseBridgeMirrors`/`collectBridgeMirrors` for Step 2). Add
      `tuiModel.mirrors []bridgeMirror` in this step too, now that the type
      exists. No behavior change yet — `go build ./picker/...` should pass
      with these as dead fields (nothing populates `mirrors` until Step 3).

- [ ] **Step 2: `configuredHosts` helper + `parseBridgeMirrors`/
      `collectBridgeMirrors` (picker/remote.go)** Factor `configuredHosts`
      out of `pendingRemoteItems`'s existing
      `dropCachedSelfAliases(parseRemoteHosts(...))` call (keep
      `pendingRemoteItems` calling the new helper, behavior unchanged). Note
      `pendingRemoteItems` applies `dropCachedSelfAliases`, but
      `collectRemoteItems` (the live probe path) calls bare
      `parseRemoteHosts` and instead drops self-hosts from the *result*
      after a live re-check (`remote.go:1093-1100`) — so a host flagged
      "self" from a stale on-disk marker but revalidated live can be in
      `m.remoteItems` yet absent from `configuredHosts`'s list. Use
      `configuredHosts` (the `dropCachedSelfAliases` version) for Tab's
      cycle order and `scopeAll`'s `hostMatches`, consistent with what
      `pendingRemoteItems` already does for the first-paint host list; this
      is an existing, pre-existing minor divergence in the codebase, not
      something this change needs to resolve — just don't make it worse by
      inventing a third variant. Add `bridgeMirror`, `parseBridgeMirrors`,
      `collectBridgeMirrors()` per the Design section, with a unit test
      mirroring `TestParseRemoteHosts`'s style: feed `parseBridgeMirrors`
      fixture `list-sessions -F` output covering (a) a pair-keyed bridge
      (`@bridge_session` set), (b) a legacy convention bridge (name =
      `host-sess`, no `@bridge_session`), (c) an ordinary local session (no
      `@bridge_host`, must be skipped), (d) a legacy-named session that
      doesn't carry the `host-` prefix (unresolvable, must be skipped).

- [ ] **Step 3: wire `mirrors` through `refreshDataCmd`/`refreshMsg`
      (picker/tui.go)** Extend `refreshMsg` with `mirrors []bridgeMirror`;
      populate it in `refreshDataCmd`'s closure via `collectBridgeMirrors()`;
      store `m.mirrors = msg.mirrors` in the `refreshMsg` case in
      `Update()`, before `m.recombine()`.

- [ ] **Step 4: `scopedItems` mirror-append + host-tree reuse
      (picker/tui.go, `recombine`)** Implement `scopedItems` per the
      revised Design section: slice `m.remoteItems` by `hostMatches` to get
      the header + per-host trees verbatim, then append synthesized mirror
      rows from `m.mirrors` for sessions not already present among each
      host's included children. Unit tests: a host with live unbridged
      sessions in `m.remoteItems` plus a mirror in `m.mirrors` gets both,
      no duplicate; a needs-auth/host-key-changed/tailscale-check host with
      **zero** children in `m.remoteItems` gets zero children in scope too
      (except an appended mirror row, if `m.mirrors` has one for it); a
      no-server host's restorable (`remoteRestore`) rows survive into scope
      unchanged; in `scopeAll` with two hosts each having a live child and a
      mirror row, every host-A row is index-contiguous and precedes every
      host-B row (round-2 finding B1 — assembly is host-block by
      host-block, never a trailing append of every mirror row). Add a small
      `hostSet(hosts []string) map[string]bool` helper (or inline the map
      build) for `scopeAll`'s `hostMatches` per the revised Design.

- [ ] **Step 5: Tab key handling (picker/tui.go `handleKey`)** Add the
      `case "tab":` branch per Design (no-op in window/emit mode; cycle
      order local → hosts → all → local using `configuredHosts`
      otherwise), in the **post-wall** `switch
      key` block (`tui.go:512` onward) — `tab` is already bound *inside*
      `handleWallKey` (`:758`, wall-focus toggle, tested at
      `wall_test.go:635,650,841`) and `handleKey` dispatches the wall
      keymap first (`:494-511`) whenever `m.mode == modeWall`, so the new
      case only ever fires in list mode and must not touch or duplicate the
      wall's binding. Run the existing wall tests to confirm nothing
      regresses. implement: escalated — this is the one place a subtle
      off-by-one (skipping a host, or not wrapping back to local from
      `all`) silently breaks the whole feature and is easy to get wrong
      under refactor.

- [ ] **Step 6: `recombine()` scope branch (picker/tui.go)** Wire the
      `switch m.scope.kind` branch per Design, calling Step 4's
      `scopedItems`. implement: escalated — this step touches the core
      list-assembly path shared by every render mode; a mistake here (e.g.
      dropping the zoxide-empty-row special case for local scope, or
      double-emitting the Remote header) regresses existing, well-tested
      local-scope behavior.

- [ ] **Step 7: Enter-resolves-to-mirror + `switchClient` seam + markable
      exclusion + marks-resolve-unscoped (picker/tui.go `activateCurrent`,
      `markable`, `markedRemoteItems`)** Per Design: the `switchClient` var
      (keep the `//nolint:errcheck` the replaced inline call carried,
      `tui.go:1342`, so `nix build .#lint` stays green), the mirror branch
      ahead of the `remoteHost != ""` branch, `&& item.remoteMirrorTarget ==
      ""` added to `markable`, and `markedRemoteItems` resolving against
      `m.sessionItems`+`m.remoteItems` instead of `m.allItems` (update its
      doc comment, `tui.go:1209-1213`, which currently says "against
      m.allItems").
      **Round-3 fix — this changes two existing tests, not just adds new
      ones:** `TestOpenMarkedRemoteWithLaunchesAllButFirst` and
      `TestActivateCurrentOpensAllMarkedIgnoringCursorRow`
      (`tui_test.go:1674`, `:1744`) build `tuiModel{allItems:
      remoteFixture()}` with `sessionItems`/`remoteItems` left nil, mark
      rows, then rely on `markedRemoteItems()` resolving them — after this
      change both resolve zero marks and go red. Re-seed both fixtures to
      also set `remoteItems` (e.g. `remoteItems: remoteFixture()[1:]` if
      index 0 of `remoteFixture()` is the local session row and the rest
      are the Remote header/host/session rows — check the actual fixture
      before assuming the split point) as part of this step, not
      discovered later from a red `nix flake check`.

- [ ] **Step 8: header/footer scope badge + hint
      (picker/render_list.go)** `withScopeBadge` on the search row,
      `⇥:scope` hint in `renderHints`, per Design.

- [ ] **Step 9: tests (picker/tui_test.go, picker/remote_test.go)**
      Acceptance-mapped:
      - Scope cycle order: local → host₁ → host₂ → … → all → local, for a
        2+-host fixture; zero-hosts Tab is a no-op.
      - Host-scope row set: mirrors of that host from `sessionItems` +
        that host's tree from `m.remoteItems` (unbridged sessions, exactly
        as today) + appended mirror rows for its bridged sessions (from
        `m.mirrors`) + no other host's rows + no zoxide rows.
      - All-hosts scope: every configured host's tree present, grouped
        (each host's rows keep `hostColorFunc`'s tint), no plain local
        session rows that aren't mirrors of a configured host.
      - Enter on an already-mirrored remote-section row switches to the
        mirror target: assert via the new `switchClient` seam (substitute
        it in the test, assert it was called with the mirror's local
        session name, restore the original after).
      - A session that is both a local mirror row (in `sessionItems`) and
        gets an appended Remote-section mirror row in host/all scope: both
        rows are expected to appear (the mirror row carries the
        `"(mirrored)"` note and `remoteMirrorTarget` set) — assert the
        Remote-section copy is present and distinguishable, not that one of
        them is suppressed.
      - `markedRemoteItems` resolves a mark set on a host that isn't the
        current scope (round-1 finding #4): mark a session while scoped to
        host A, cycle to host B, press Enter — assert the launcher for
        host A's session still fires (via `openMarkedRemoteWith`'s existing
        injected-func test pattern).
      - Filter query narrows within the active scope (host/all scope +
        query hides non-matching rows, same fuzzy-match path).
      - Inert host (each of needs-auth / host-key-changed / tailscale-check
        / unreachable) in host/all scope: Enter and `^t` both remain no-ops
        on every row under it, exactly as local scope's Remote section
        today (reuse/adapt `TestCtrlTNoOpOnInertRows`'s table).
      - Marks: mark a session in local scope, cycle through host/all/back
        to local, mark survives (keyed by `item.target`, unaffected by scope
        — this should require no new code, only a regression test); a
        mirrored row is never markable (`^t` no-op on it) in host/all scope.

- [ ] **Step 10: docs (docs/agents/picker.md, docs/agents/scripts.md)**
      picker.md: replace the "Tab is reserved for an upcoming host-scope
      filter" aside (in the `^t` bullet) with a real bullet describing the
      scope cycle, the row-set rule per scope, and the mirror-resolves-to-
      switch behavior — keep the existing `^t`-marks bullet's cross-scope
      note in sync. scripts.md: extend the `tmux-session-picker` row with
      the Tab/scope behavior, cross-referencing picker.md.

## Constraints carried from the task

- No new ssh calls anywhere in this change — every scope's remote data
  comes from `m.remoteItems` (already probed, reused verbatim) +
  `m.mirrors` (local `tmux list-sessions`, not ssh).
- `visibleWidth` for cell sizing, `printableKeyText` for typed keys —
  Tab is neither; it's a dedicated `case "tab":` key match, same shape as
  `ctrl+t`/`ctrl+g`.
- Marks (#730) must keep working unchanged across scope switches — this
  falls out of marks being keyed by `item.target` and scope-switching never
  touching `m.marked`.
- A later worker adds kill/forget keys on Remote rows — keep `handleKey`'s
  `case "tab":` and the mirror/markable changes textually tidy and close to
  their related existing branches, not scattered.
