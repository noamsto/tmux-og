# Spec — ship remote CPU/mem over the subscription channel (#693)

Revision 3. r2 answered the churn finding by deleting liveness outright; the
critic was right that this left a dead poller and an orphaned mirror frozen
forever. r3 restores liveness **without** restoring churn: the tick rides the
published row, and the *daemon* deduplicates on the display-grade fields, so a
report carrying only a new tick costs no local write. Precedence stays
per-session (r2).

## Problem

The session picker's CPU/Mem columns on a **mirror** row must measure the
*remote* session, because the mirror's local panes run bridge renderers, not the
work. Today `picker/remote_resources.go` gets that by fetching the remote's
whole `ps` table over a per-host ssh round-trip and aggregating it locally.

Three pieces of incidental complexity exist only because a **shell on the
remote** is assembling and a **local process** is parsing that payload:

1. `remoteResourcesSeparator = "PSTABLE"` must begin with a letter, because the
   remote's login shell is whatever the user set and fish reads `echo --` as
   end-of-options — which once left every mirror at `0% / 0M`.
2. `getconf _NPROCESSORS_ONLN` rather than `nproc`, because `nproc` is
   coreutils-only and absent on macOS.
3. Hundreds of `ps` rows cross the wire per host per 10s so that the tree walk
   can happen locally.

Meanwhile the bridge already has a push channel for exactly this shape of state:
`refresh-client -B` subscriptions (`agentstatus.go`, `windowlabels.go`, #566).
Remote agent state and remote window labels already ride it. Remote resources do
not.

## Goal

Move the aggregation to where the process tree is, ship ~3 numbers instead of a
table, and delete the ssh round-trip from the picker's steady state — without
regressing a mirror whose remote has not been rebuilt yet.

## Requirements

### R1 — a remote poller computes the answer

A poller running **on the remote** computes, per tmux session on that server, the
same two quantities the local leg produces (`cpuPct`, a raw per-core `ps` sum;
`memMB`, summed RSS) plus that host's core count, and stamps them on the session
as a tmux **session option**.

- It must reuse the *existing* process-tree walk, not a second implementation.
  `aggregateResources` is documented as "the only place the tree walk lives" and
  that must stay true.
- It must not need `getconf`/`nproc`: the poller is a program, so the core count
  is available directly from the runtime.
- It must run on a host whose only clients are control-mode bridges — i.e. it
  must not hang off `status-format[0]` (#603). The `-B` monitor-hook floor is the
  established mechanism, and the three traps `CLAUDE.md` already records for it
  are binding here:
  - a session monitor's target field must be **empty** (`@name::<format>`), never
    the word `session` (upstream `557967c3` made a non-empty unknown target a
    hard parse failure);
  - the version guard's body must be passed as a **string**, not a brace block,
    or the pinned parser rejects `-B` on the untaken branch;
  - the new hook needs an unconditional `set-hook -g -u -B` **clear** paired with
    a `set -gu`, emitted before every setter, or a rebuild that disables it
    leaves the old monitor firing at a garbage-collected store path.
- It must not tax a host nobody is bridged to: a pass runs only while the server
  has a control-mode client attached.

### R1a — the published row is display-grade, plus a liveness tick

The remote publishes the two figures **quantised to the precision the picker
actually renders**, followed by the remote's own epoch-second tick.

- The quantisation is what keeps the *expensive* half quiet: a subscription
  reports on every string change, sampled by tmux's own 1s monitor timer, and
  each report that moves the display costs the daemon a local `tmux set-option`
  fork. Quantising means an idle mirror never moves that.
- The tick is what makes the poller's own liveness observable at all. It is
  deliberately cheap to carry and deliberately **not** deduplicated by tmux: it
  changes every pass, so a notification arrives every pass, and the *absence* of
  notifications is the only evidence that the remote stopped publishing. A remote
  tmux option persists after the process that wrote it dies, so no amount of
  re-**reading** the option can distinguish a live poller from a dead one — which
  is why a daemon-side backstop read alone does not solve this, and why the tick
  travels in the row.
- The cost this trades for is one short control-stream line per poller pass per
  bridged session (order tens of bytes, against the `%output` frames the same
  stream already carries continuously). R2 makes it cost no local work.
- "Quantised" means **published at a coarse fixed precision**, not pre-rendered.
  The poller must not reimplement `formatCPU`/`formatMem`: those own the display
  and R5 keeps them untouched, so a second copy of `<1%` / `M` vs `G` across the
  boundary would be two things to keep in step. The poller publishes numbers; the
  picker formats them, exactly as it does for a local session.
- A legitimately **zero** row — an idle remote session at 0 CPU, 0 MB, with a real
  core count — is a valid published row and must read as fresh and covered.
  Zero and absent are different states and the encoding must keep them so.

### R2 — the daemon lands the value on the local mirror session

The bridge daemon subscribes to the remote stamp on the established channel and
writes it onto the **local mirror session** as a daemon-owned `@bridge_*`
option, the way `@bridge_session_path`, `@bridge_host`, `@bridge_state` and the
`@bridge_crew_*` trio already work.

- The daemon never writes the remote's own option name locally.
- **The stamp must not contain `|`.** The picker reads session-scoped `@bridge_*`
  values out of a pipe-delimited `list-panes -a` format whose parse fails
  *closed* on a wrong field count, so a pipe in this value would not corrupt one
  column — it would silently drop the whole session from the picker. The
  validation rejects it outright rather than substituting, which is the identity
  -field policy this value already falls under.
- The carried value is validated before it is stamped (numeric fields only); an
  unusable or over-long value is **dropped whole** rather than truncated,
  matching `cleanLabelValueExact`'s identity-field policy — a truncated number is
  a different number.
- **The daemon deduplicates on the display-grade fields, not on the whole row.**
  A report whose figures are unchanged and whose only difference is R1a's tick
  performs no local write. This is what keeps R1a's per-pass notification from
  becoming per-pass churn.
- **The local stamp carries the daemon's own receive time**, recorded on the
  local clock at the moment it writes — never the remote's tick, which would need
  the clock-skew machinery and would be measuring the wrong thing. So that a
  quiet-but-live mirror does not age out, the daemon re-writes the stamp when its
  recorded time falls further behind than a refresh floor, even with the figures
  unchanged. That floor is a purely local `set-option`, an order of magnitude
  below the poller's own cadence in frequency.
- An **empty** carried value (a remote with no poller, or one whose poller
  stopped publishing) must **unset** the local option, not leave the last value
  standing.
- The subscription must be (re-)installed on **every** attach, first and
  reconnect alike, like the other two. Re-subscribing re-reports the current
  value — including an **empty** initial value, which is precisely what lets a
  remote with no poller be recognised as such rather than merely as silent, and
  so is what makes R4's fallback trigger at all.
- The daemon does **not** add a polling backstop for this value. If the remote
  refuses the subscription outright (a tmux predating 3.2), the option simply
  stays unset and R4's fallback covers that host — one mechanism serving both
  skew cases. A backstop read would buy nothing the tick does not already give
  (see R1a) and would cost a remote round-trip per floor.

#### Why an option and not in-memory

The issue offered "the established subscription shape, **or** in memory". Options
win and the spec records why: the picker is a short-lived process spawned per
popup and already reads session-scoped `@bridge_*` values out of the **single**
`list-panes -a` fork it makes before first paint. An in-memory value would have
to be dialled out of the daemon over its ctl socket, per host, synchronously, on
the 1s item rebuild — a new round-trip on the picker's scarcest path, and a new
failure mode when no daemon answers.

### R3 — the picker reads the stamp instead of ssh

The picker prefers the landed stamp for a mirror row and does **no** ssh work for
a session that carries a fresh one.

- **Precedence is per session, not per host.** A session with a fresh stamp keeps
  it unconditionally; an ssh answer fills only the sessions that do not have one.
  A host is ssh-probed only when at least one of its mirror sessions is
  uncovered, and a covered session's values are never overwritten by that probe's
  result.
- `sessionData.cores` keeps its current semantics: the core count of the machine
  the processes run on, `0` meaning this one. The poller supplies it.

### R3a — freshness criterion

A landed stamp counts as **fresh** iff all three hold, read from the same
`list-panes -a` snapshot the picker already takes:

1. the mirror session carries a parseable resource stamp, **and**
2. the receive time inside that stamp is not older than a staleness threshold,
   measured against the reader's own clock — the stamp was written locally by the
   daemon on that same clock, so no skew correction is involved, **and**
3. that session is **not** marked `@bridge_state disconnected`.

Each condition covers a distinct death, and none is redundant:

- **(2) covers a poller that stopped while the bridge stayed healthy.** No
  notifications arrive, so the daemon stops refreshing, so the stamp ages out and
  the session falls back to ssh. This is the case r2 accepted and should not
  have.
- **(2) also covers a daemon that died without teardown** — SIGKILL, a crash, a
  lost host — leaving an orphaned mirror session with nobody to unset anything.
  Named here explicitly because r2 did not name it at all: the stamp ages out on
  the reader's own clock, which needs no cooperation from the corpse.
- **(3) covers a known outage immediately**, rather than waiting out (2). The
  daemon sets `disconnected` for the whole of a control-connection outage and
  unsets it once repair completes, so a mirror whose bridge is down is uncovered
  at once and falls back to ssh — which is what the current code does
  unconditionally anyway, so this is never a regression.

The threshold must be a small multiple of the daemon's refresh floor (R2), so a
single missed refresh never flaps a healthy mirror into the fallback.

### R4 — version skew is an explicit, documented decision

An un-rebuilt remote has no poller and therefore stamps nothing. Those mirrors
must still render real figures, not `0%` and not a permanent `-`.

- The fallback is the existing ssh-`ps` path, kept intact but **demoted**: it
  runs only for hosts with at least one uncovered mirror session, and fills only
  those sessions.
- The decision is written down in `CLAUDE.md` alongside the rest of the bridge
  contract, including the condition under which the ssh path may be deleted.
- `CLAUDE.md`'s **"What the Remote Host Needs on PATH"** table gains a row for
  this feature, stating what the remote must have and that its absence degrades
  silently to the fallback — the same shape as the remote-window-labels row,
  which is already called out as the requirement with no capability probe.

### R5 — no regression in the columns

- A session covered by neither path still renders `-`, never the renderer's own
  local figures (`resUnknown`).
- Column widths, `cpuColor`'s core scaling and `formatCPU`/`formatMem` are
  untouched.

## The acceptance criteria's apparent tension

The task asks both that "the three shell-parsing workarounds disappear" and that
"the existing ssh-`ps` path survives as fallback". The version-skew criterion is
written as a genuine either/or, so these are not a contradiction to adjudicate so
much as a choice to record: this spec keeps the fallback, so the workarounds
disappear from the **primary** path and survive only inside the explicitly
legacy one, whose sunset condition is written down (R4).

Deleting the ssh path now is rejected: it strands every un-rebuilt host at `-`,
which R4 forbids outright.

## Out of scope

- The control stream and the wider agent-channel work.
- Remote agent state (#566) and remote window labels, which already ride this
  channel.
- The local (non-mirror) resource path: `collectSessionResources` is unchanged.
- Per-window or per-pane resource figures. Session granularity only, as today.

## Acceptance evidence

- A Go test proves the picker takes the subscription value and performs **no**
  ssh fetch for a covered session; that an uncovered session on the same host is
  filled from the ssh answer while the covered one is not overwritten; that a
  `@bridge_state disconnected` session is treated as uncovered; and that a stamp
  older than the staleness threshold is treated as uncovered.
- A Go test proves the daemon stamps the local mirror session from a
  `%subscription-changed` session-scoped line, performs **no** local write for a
  report whose figures are unchanged, does write once its refresh floor has
  elapsed, rejects a malformed value whole, and unsets on an empty one.
- A Go test proves the poller derives the same numbers as the local leg from a
  fixed `ps` table, that it publishes display-grade values plus a tick, and that
  it no-ops with no control-mode client attached.
- `nix build .#default`, `nix flake check`, `nix build .#lint` all green.

## Gate note

The spec-critic's second round returned `revise` with the two findings this
revision answers (a dead poller and an orphaned mirror both frozen forever).
The protocol's 2-assignment cap was reached at that round, so r3 was not
re-gated; its changes are recorded above and carried into the plan, which is
gated separately.
