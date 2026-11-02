# Relay graphics and cell env follow the size owner (#1035)

## Problem

Two controllers mirroring one remote session share one session-environment slot
for `OG_RELAY_GRAPHICS` and `AEYE_CELL_PX` (`daemon/relayenv.go`). Every bridge
daemon writes its own values unconditionally (Run startup, `repair()`,
`watchLocalClient` on change) and unsets both unconditionally on teardown
(`sendCtl(RelayEnvUnsetCmd, CellEnvUnsetCmd)` in Run's teardown closure). So:

1. Each daemon overwrites the other's values; with a sixel laptop and a
   non-sixel desktop the slot flips with whichever wrote last, so the remote
   sends graphics one controller drops or withholds them from one that paints.
2. A daemon that exits cleanly unsets both while the other is still attached.
   The survivor republishes only on its own capability/cell change or a
   reconnect, so until then a remote `aeye` loses sixel relay and uses a
   default cell size.

## Invariants

- **I1 (teardown):** a daemon's teardown never removes the variables while
  another client is still attached to the session.
- **I2 (one writer):** while the size-owner record `@og_bridge_owner` names a
  daemon, only that daemon's values are written to the slot; a non-owner's
  write never lands.
- **I3 (ownership move):** when ownership moves (a claim, or a survivor
  releasing a departed owner's record), the slot gets the new holder's values
  without waiting for a capability change or reconnect.

## Decision: follow the size owner

The slot holds the values of the controller named by `@og_bridge_owner`, the
controller whose local user was last active (#979).

Why not a merge across controllers:

- It is the precedent: window size (#979) and URL routing (#1034) both follow
  the owner, so the remote behaves as if the active controller were the only one.
- The remote cannot compute a merge. The env value is a single string.
  Intersecting capabilities and picking a cell size would need a per-controller
  registry and a remote-side reduction that tmux formats cannot loop over. A
  daemon could only reduce from a read, then race other daemons doing the same.
- A cell size has no right merged value. Two terminals with different cell sizes
  need different crops, and any pick is wrong for one of them. Following the
  owner makes it right for the screen the user is looking at.
- An intersection takes sixel away from the sixel controller even while the
  user works on it. Following the owner keeps it.

**What a remote `aeye` sees in the mixed-capability case** (sixel laptop L,
non-sixel desktop D). `aeye` reads both variables from its process environment,
inherited from the session env when its `run-shell` or pane is spawned:

- User last active on L (L owns): `OG_RELAY_GRAPHICS=sixel`, L's cell size. A
  carousel launched now relays sixel. L paints it. D's own `Proxy.Filter` drops
  the sixel (its local `RelaySource` says no), so D shows no image there. That
  is the same drop D does today for any sixel it cannot paint, and the user is
  not looking at D.
- User moves to D and types (D claims): the slot becomes `OG_RELAY_GRAPHICS=`
  (empty) and D's cell size (or unset when D's size is unknown). A carousel
  launched afterwards takes the kitty/carousel path.
- An `aeye` already running keeps the values it was launched with. The session
  env reaches only processes spawned after the write, as it does today.

## Mechanism

All writes are remote-side conditional `if-shell -F -t <pinned $id>`, the
`ownerReleaseCmd` pattern. The condition is evaluated atomically by the remote
tmux when the command runs, so no read-then-write race between daemons exists.

1. **Guarded publish (I2).** Every publish becomes
   `if-shell -F -t '$N' '<owned>' '<set-environment …>'` with
   `<owned>` = `#{?#{@og_bridge_owner},#{m:<token>|*,#{@og_bridge_owner}},1}`,
   which is true when the record is unset or names this daemon's token. It
   applies at all four existing publish sites (Run startup, `repair()`,
   `watchLocalClient` relay change, `watchLocalClient` cell change). The
   conditional form also orders racing claims. With A: claim, publish and B:
   claim, publish interleaved as claim A, claim B, publish B, publish A, A's
   publish sees B's record and does not land. Built from `#{?}` and `#{m:}`
   only, both much older than the control-mode features a bridge already needs.
   No `#{||:}` is used. The inner `set-environment` commands target the same
   pinned `$N` as the `if-shell`, never the session name, so the write lands on
   the session the guard checked.
2. **Claim publishes (I3).** `ownerCtl.claimWith` sends the guarded publish of
   this daemon's current values in the same batch as its record write. Every
   claim (attach, activity `claimIfStale`, reconnect `reconnected`/`reopened`)
   goes through it, so the slot moves with ownership. The guard still applies:
   a claim that lost a race to a later one does not overwrite.
3. **Release publishes (I3).** When the owner departs without a clean
   teardown (its SIGTERM kills the transport first, or it crashed), survivors
   hold the release for `ownerReleaseGrace` (30s, unchanged) and then send
   `ownerReleaseCmd`. Its `if-shell` branch, which runs only while the record
   still names the departed owner, now also sets the releaser's own values. Only
   the survivor whose release actually cleared the record writes, because later
   survivors' conditions are false. During the grace the slot keeps the departed
   owner's values, as the windows keep its size.
4. **Teardown (I1).** The unconditional unset becomes a read, then a
   conditional write. The departing daemon reads the session's clients
   (`list-clients -t $N` with `client_control_mode`, plus its own name and
   `#{session_attached}` in the same batch). If another control client is
   attached, it sends nothing. Otherwise it sends
   `if-shell -F -t '$N' '#{==:#{session_attached},<n>}' '<unset both>'`, where
   `<n>` is the `session_attached` it just read (tmux's own count, which skips
   suspended or exiting clients that `list-clients` still lists). The count closes the read-to-write
   race: a controller attaching in between changes it, and the unset does not
   land. A human tty client does not block the unset. A later or concurrent
   direct attach from a non-sixel terminal still does not inherit a stale
   `sixel`, which is why the unset exists. A departing owner with other
   controllers attached leaves its values, and step 3 replaces them after the
   grace. A failed read (dead transport) sends nothing, because the unset could
   not have landed anyway. `session_attached` alone cannot express this rule,
   because tmux formats cannot filter it by `client_control_mode`.

   On main the teardown unset lands only on Run's early-return teardowns, where
   the connection is still alive. The dominant exit is SIGTERM, whose handler
   kills the transport before teardown runs (see the comment at the teardown
   unset). So I1 fixes the clean-exit clobber, and step 3 carries the real-world
   relief for a SIGTERM'd or crashed owner. The tests cover both.
5. **Unpinned fallback.** When the identity read failed (`pin.id == ""`),
   size ownership is already off (`ownerOn` false) and no `$id` exists to target
   `if-shell -t` safely: `if-shell -t` takes a target pane, and a numeric session
   name like `0` resolves as a pane or window index. The daemon then keeps
   today's unguarded publishes and unconditional teardown unset. Guarded iff
   `ownerOn`.

The local drop is untouched: each daemon's `Proxy.Filter` keeps reading its own
`RelaySource`, so a controller never paints what its terminal cannot.

## Producer/consumer map

| edge | symbol / file | disposition |
| --- | --- | --- |
| producer: startup publish | `Run`, `daemon.go` (after the ownership check) | changed: guarded |
| producer: reattach publish | `repair()`, `daemon.go` | changed: guarded |
| producer: change publish | `watchLocalClient`, `daemon.go` | changed: guarded, takes the env builder instead of a session name |
| producer: claim | `ownerCtl.claimWith`, `sizeowner.go` | changed: also publishes (new) |
| owner rename on replace | `primeClient`, `conn.go` (`ownerClaimCmd` on the replacement connection) | unchanged, no publish: it only renames our own record, it is not an ownership move, and its per-command reply count would break on an `if-shell` branch block; `repair`'s claim publishes |
| producer: release | `ownerCtl.releaseDue` / `ownerReleaseCmd`, `sizeowner.go` | changed: branch also publishes the releaser's values (new) |
| producer: teardown unset | Run teardown closure, `daemon.go` → `ownerCtl` | changed: read clients, unset only with no other control client, guarded by the read count |
| transport | `stream.stampAll` per-command `og-fanout` barrier | compatible: absorbs the `if-shell` branch's extra reply blocks, as it already does for `ownerReleaseCmd` |
| persistence | remote session env table; survives reconnect (same server) | compatible; replaced server → `reopened()` claim publishes |
| consumer: carousel | `ctl.go` `carousel` verb, remote `run-shell` inherits session env | compatible: grammar unchanged |
| consumer: `aeye` (noamsto/aeye `cellsize.go`, relay reader) | process env | compatible: grammar unchanged, values now the owner's |
| consumer: `og-open.sh` | reads `@og_bridge_owner` (glob `*\|<client>`) | compatible: record format and writers unchanged |
| consumer: local drop | `Proxy.Filter` ← `graphics.RelaySource` | unchanged |
| validators | `CellPx`, `graphics.Relay.String()` | unchanged; remote side has none (absent) |

## Compatibility (mixed versions)

- **Older daemon (pre-#1035) on the other controller.** It still writes
  unguarded and unsets unconditionally on clean teardown. Its writes win
  whenever it makes them, and its clean exit still clears this daemon's values.
  That is the #1035 behaviour for that pair until it upgrades, never worse than
  today. This daemon's teardown no longer clears the older daemon's values, and
  this daemon's guarded writes are ordinary `set-environment` to the older
  daemon.
- **Older daemon owning the record** (post-#979, pre-#1035). This daemon, as a
  non-owner, does not write. The slot holds whatever the owner last wrote
  unguarded. Correct under the policy.
- **Pre-#979 daemon** (never claims). Behaves like the first bullet. It is
  flagged and never named by the record.
- **Remote tmux.** Needs `if-shell -F`, `#{?}`, `#{m:}` and `session_attached`,
  all older than the `refresh-client -f` the bridge already requires. No new
  session option is introduced, so no remote script changes.
- **`aeye`.** Unchanged contract: `OG_RELAY_GRAPHICS` grammar and
  `AEYE_CELL_PX` grammar are the same; only who writes them changes.

## Edges, accepted

- **Grace window.** For up to `ownerReleaseGrace` after the owner leaves, the
  slot keeps the departed owner's values (as the windows keep its size), or
  until a survivor's user types and claims.
- **Every survivor asleep, or the release done by an older daemon.** The slot
  keeps the departed owner's values until the next claim.
- **An iTerm2 `-CC` client attached directly to the remote session.** It is a
  control client, so it blocks the teardown unset just as a bridge does. Its
  panes then inherit the departed bridge's values. tmux cannot tell a bridge's
  control client from another (size-ownership edge (2), #979).
- **Carousel opened right after switching controllers.** `prefix + I` launches
  through the pressing controller's daemon at once, while its activity claim
  lands on the next reveal tick (≤ 1s). A carousel opened inside that window
  launches with the previous owner's values. Fixing it means the `carousel`
  verb carrying its own values into the `run-shell` environment, a separate
  change.
- **Clean exit of the owner with survivors attached.** Values persist until
  the release (step 3). The owner does not release its own record on clean
  exit, so size ownership behaviour is unchanged.

## Tests

- **Live tmux, two control clients (acceptance 1, I1).** One isolated scratch
  session (`startIsolatedTmux`, `startOpenCtl` pattern) hosts two control
  clients, each driven through a real `ownerCtl`. B claims and publishes. A
  runs the production teardown, and B's `OG_RELAY_GRAPHICS`/`AEYE_CELL_PX`
  remain. Then B, left alone, runs teardown, and both are unset (the
  single-controller behaviour is preserved). The new teardown entry point does
  not exist on main. Red is shown in a scratch copy by restoring main's
  unconditional unset pair inside that entry point. The PR's `## Testing`
  records the command and the failing assertion.
- **Live tmux, departed owner (I3, the SIGTERM path).** A claims and
  publishes. A's control client is killed, so its transport drops and no
  teardown runs. B's release of A's record lands B's values.
- **Live tmux, mixed capability (acceptance 2, I2/I3).** A claims and
  publishes `sixel` and `15x28`. B's guarded publish (`""`, unknown cell) does
  not land. B claims, and the slot becomes B's (relay empty, cell unset). A's
  guarded publish does not land. A release by B of A's record carries B's values.
  The claim, publish and release commands are the production builders.
- **Unit.** `claimWith` and `releaseDue` batches carry the publish. The
  teardown sends nothing while another control client is attached, and sends
  the count-guarded unset otherwise.
  `watchLocalClient` sends the guarded form when guarded and today's form when
  unpinned. Builder strings for the guard and teardown.
