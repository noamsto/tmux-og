# #820 — mirror window bar stale after attach — design

## Symptom

Entering a remote-bridge mirror (a client switching onto it from the picker, or
a fresh attach) left the status bar's window list wrong — wrapped for the wrong
width, labels/icons stale or empty — until an unrelated interaction (a window
switch, a resize) happened to run a reflow.

## Root cause (measured)

The bridge daemon registered **session-scoped** hooks on the mirror session:

```go
var resizeHookEvents = [...]string{
    "client-resized", "window-resized", "client-session-changed", "client-detached"}
// registerResizeHook: set-hook -t <local-sess> <event> 'run-shell -b "touch -- <sock>.resize"'
```

In tmux, a session's own hook array **replaces** the session's view of the
global one (`hooks_insert` consults `fs.s->options`; `options_get` only falls
back to the parent when the session has no entry of its own). So inside a
mirror session the conf's **global reflow hooks never ran** for those four
events:

- `client-session-changed` → switching a client into a mirror never reflowed.
  The launcher path (`og-remote-open` … `tmux switch-client`) and the picker's
  open path are both this event.
- `client-resized` → a terminal resize while on a mirror never reflowed.
- `window-resized` → the global float/grid refit hooks were shadowed too
  (no-ops on a mirror, same class of bug).
- `client-detached` → only the bridge's own hook was there; no harm.

`client-attached` was **not** in the bridge's set, so a cold attach still
reflowed — which is why the bug reads as attach-only-in-some-paths.

The hazard was already known and documented for `pane-died`
(`docs/agents/bridge-daemon.md`, #647): “a session-scoped `set-hook -t
<session>` entirely replaced the session's view of the *global* array”. The
resize nudge was the remaining instance.

### Evidence

Scratch repro: two `-L` servers (a "remote" with four windows and the local
mirror), `og-remote-open`'s mirror session born detached, the daemon's
`--test-local` seam, the real conf.

- With no client ever attached, no layout was stamped — `@reflow_key` empty
  (`#{client_width}` empty, #235's empty-width bail).
- A client attached to a *different* session and switched onto the mirror:
  `@reflow_key` stayed empty, every `@window_label_short` empty, and the global
  `status-format[1]` rendered `1: ` with nothing.
- A probe hook fired on `client-session-changed` for a switch *off* the mirror
  but never for a switch *onto* it; a verbose tmux server log showed the
  switch-in dispatch running exactly one hook —
  `hooks_insert_one: hook client-session-changed is: run-shell -b "touch -- …resize"`,
  the bridge's, never the conf's reflow.
- `client-attached` did reflow, and its result equalled a forced pass — the
  attach case is the pin, the switch case is the bug.

## Fix

**The bridge publishes its nudge path as a session option; the config's own
hooks carry the touch.** No session-scoped hooks remain.

- `picker/remotebridge/daemon/daemon.go`: `registerResizeHook` →
  `registerResizeNudge`, a single `set-option -t <sess> @bridge_nudge <path>`
  (teardown unsets it). The four legacy hook events are unset once here, so a
  mirror session that outlived a binary swap cannot keep shadowing.
- `scripts/tmux-reflow-windows.sh`: the existing fast-path `display-message`
  gains a fourth field `#{@bridge_nudge}` (no extra fork) and touches it after
  the empty-width bail, before the cache check — so `client-resized` (the
  surviving `--debounce` pass) and `client-session-changed` nudge with their
  reflow, a cache hit included.
- `config/tmux.conf.tmpl` (+ the frozen `config/tmux.conf.reference.nix`):
  `window-resized[20]` (free beside the refit gates at [0]/[10], already
  cleared by the bare `set-hook -gu window-resized`) and a `client-detached`
  hook, both `if -F '#{@bridge_nudge}'`-gated. `set-hook -gu client-detached`
  joins the reload clear block. A non-mirror session evaluates the gate
  in-process and forks nothing.

### Rejected alternatives

- **Append the touch to the global hook arrays (`set-hook -ag`)**: keeps the
  daemon self-sufficient, but any config reload (`set-hook -gu`) silently drops
  it for the daemon's whole life — a #433 regression after `prefix + r` or a
  home-manager activation — and removal needs a `show-hooks` read-back.
- **Copy the global hook commands into the session array at registration**:
  freezes the config's commands (store paths) into session state; stale after
  a reload.
- **Hook an event the config does not use**: no free event covers a session
  switch without a resize, so the watcher's viewing-identity re-resolve would
  go stale; `pane-resized` is pane-scoped and misses switches and detaches.
- **Revert to the 1s fork poll**: #433.

## Contract map (`@bridge_nudge`)

| edge | disposition | reference |
| --- | --- | --- |
| producer: daemon `Run` start / teardown | `set-option -t` / `-u` | `registerResizeNudge` / `unregisterResizeNudge` |
| consumer: reflow snapshot | touch when non-empty, after the width check | `tmux-reflow-windows.sh` |
| consumer: `window-resized[20]` hook | gated touch | conf |
| consumer: `client-detached` hook | gated touch | conf |
| absent (non-mirror session) | no touch, no fork | reflow reads empty; gates false |
| local server without the conf | 30s `resizeFallbackInterval` (unchanged) | only the `--test-local` suites, which mirror the hooks |

Edge cases: a `|` in the path is safe (last field of a `|`-delimited read);
`touch -- "$NUDGE"` is a quoted argv, and `#{q:}` quotes the conf side; paths
with spaces survive both.

## Switch-away stays un-nudged (non-regression)

`client-session-changed` dispatches once, with the **new** session as its
context (measured on the pinned next-3.9). Switching a client *off* a mirror
never nudged its daemon — neither before this change (the hook on the old
session is not the one consulted) nor after (the global hook runs with the
switched-to session, where `@bridge_nudge` is unset). The daemon re-learns its
area on the 30s fallback or the next event.

## Tests

- `tests/reflow-mirror-attach.bats`: the real conf on a scratch server, a
  bridge mirror via `--test-local`, and the assertion that the reflow-derived
  state right after the client enters equals the state a forced pass produces
  (`@reflow_key` cleared as a marker before the forced pass). Test A is the
  switch (red before the fix; the guard is a bounded poll for `@reflow_key`),
  Test B the fresh attach (green before and after — the pin). Blank
  `status-format[0]` + `status off` on the dst server keep the per-tick `#()`
  jobs (`tmux-update-icons`, a second reflow source) from masking the hook
  path; the state is asserted as options, not drawn output.
- `tests/remote-m2-integration.bats`: the vanilla `DST_CONF` gets all four
  gated touch hooks (there is no reflow script to carry two of them there), and
  the `show-hooks` resize gate becomes a non-empty `@bridge_nudge` gate.
- `flake.nix`: `reflow-mirror-attach-tests` runs the bats file with the built
  wrapper/binaries and asserts the generated conf carries the two hooks and the
  clear.