# Plan: `og-remote-open --into --window <selector>` (#989)

Spec: `docs/superpowers/specs/2026-10-16-bridge-into-window-selector-design.md`
(accepted). Stacked on #990's branch: build on its `--into` lock/wait section
of `scripts/og-remote-open.sh`, never rewrite it.

Every step below keeps the unfiltered paths byte-for-byte: a nil filter (no
selector) takes today's code, so the existing `--into` and dedicated suites
(AC4) are the regression guard for each step.

## Consumer map (shared contract: launcher → daemon env/flag, `sock_key`)

| Edge | Producer → consumer | Disposition |
|---|---|---|
| `OG_DAEMON_SELECT` / `--select` | `og-remote-open.sh` placed launch → `cmd/daemon/main.go` | changed (new); dedicated launch unsets it, daemon refuses it without `--into` |
| `sock_key` (`og-into-<cksum>.sock`) | `og-remote-open.sh` → its own lock / `probe_daemon` / daemon `--sock` | changed only with a selector (5th field); plain `--into` key byte-identical (bats pins both) |
| `@bridge_sock` window stamp | daemon → `og-remote-detach`, ctl `resolveDrag`, `localPaneMap`, `ownWindows`, reveal | compatible: one sock per daemon, unchanged stamp |
| remote `list-windows` reply | remote tmux → `parseWindowList` (unfiltered) / new `parseSelectedWindowList` (filtered) | changed for filtered only; `windowListFormat` unchanged |
| `og_select` subscription | remote tmux → `dispatch` | new; marks a reconcile due only |
| `og-into-*.sock` in tests (`park_test`, `placed*_test`, ctl `main_test`) | fixtures | compatible: path strings only |
| `install_into_fakes` (`tests/remote-cold-start.bats`) | fake daemon logs | the `LAUNCH_LOG` line format stays unchanged; the selector goes to a separate log |

## Steps

### Step 1 — selector model and parse (`implement: sonnet`)

New file `picker/remotebridge/daemon/selector.go`:

- `type WindowSelector struct` (unexported fields: `raw`, `id`, `opt`, `value`),
  `ParseWindowSelector(s string) (WindowSelector, error)` with the spec's two
  regexes (`^@(0|[1-9][0-9]*)$`; `^@[A-Za-z_][A-Za-z0-9_-]{0,63}=[A-Za-z0-9._/@+:-]{1,255}$`,
  split on the first `=`), `String()` (canonical = raw), `IsZero()`.
- `flag()` → `#{==:#{window_id},@N}` or `#{==:#{@opt},VAL}`.
- `listFormat()` → `'#{window_index} #{window_id} #{window_active} <flag> #{window_name}'`
  (single-quoted the way `windowListFormat` is; the charset holds no `'`).
- `subscribes()` → true for the option form only.
- `selState` (`selUnknown`, `selNo`, `selYes`) and a `sel selState` field on
  `remoteWindow` (zero value `selUnknown`; unfiltered parsing sets `selYes` on
  every row so the shared code can test one field).
- `parseSelectedWindowList(body string) []remoteWindow`: cuts index, id,
  active, flag, then name (name optional, as `parseWindowList`). Flag `1` →
  `selYes`, `0` → `selNo`, anything else → `selUnknown` **but the row is
  kept** (its id stays live). A row with fewer than four fields is dropped, as
  `parseWindowList` drops a malformed one.
- `parseWindowList` sets `sel: selYes` on each row (no other change).
- `type windowFilter struct { sel WindowSelector; dirty, subscribed bool; last time.Time }`,
  `newWindowFilter(sel) *windowFilter` (nil for a zero selector). Nil-safe
  methods:
  - `listCmd(remoteSession string) string`: nil → today's
    `list-windows -t <q> -F windowListFormat`; else the filtered format.
  - `parse(body string) []remoteWindow`: nil → `parseWindowList`, else
    `parseSelectedWindowList`.
  - `observe(l controlmode.Line)`: calls `note()` when `l` is an `og_select`
    `%subscription-changed` line (via `subscriptionValue`); anything else is
    ignored. `dispatch` calls it, so the routing is unit-testable without the
    `runMirror` closure.
  - `note()`: sets `dirty` (pure, no I/O).
  - `due(now time.Time) bool`: nil → false; `dirty` or
    `now.Sub(last) >= pollFloor(subscribed, mainLoopTickInterval, windowLabelBackstopInterval)`.
    An id selector (`!subscribes()`) is never due.
  - `reconciled(now)`: clears `dirty`, sets `last`.
  - `newWindowFilter` seeds `last = time.Now()`: the startup reconcile that
    follows within the same setup resets it again, so the first backstop is a
    full floor away, not immediate.
- `Config` gains `Select WindowSelector` (exported, set by cmd) and
  `filter *windowFilter` (unexported, built in `runMirror` beside `cfg.opts`).

Unit tests `selector_test.go` (AC3):
- parse table: accepted `@0`, `@12`, `@crew_branch=feat/989-x`,
  `@a=b:c@d+e.f_g-h`; refused `@`, `@01`, `@1x`, `@=x`, `@a=`, `@1a=x`,
  `@a b=x`, `@a=b c`, `@a=$(x)`, `@a=#{pid}`, `@a=b;c`, `@a=b'c`, `@a=b,c`,
  `@a=b}c`, `@a=b|c`, `a=b`, a 256-rune value, a 65-rune name, `@a=b\nc`.
- `flag`/`listFormat` exact strings for both forms.
- `parseSelectedWindowList`: `1`/`0`/malformed flag (`x`, empty) rows; a
  malformed row keeps its id with `selUnknown`; a name with spaces stays whole;
  a 3-field row dropped.
- `due`: nil filter never due; dirty → due; unsubscribed backstop due at 5s not
  before; subscribed at 30s not before; id selector never due; `reconciled`
  clears.

### Step 2 — filter sites in the daemon (`implement: opus`)

All edits in `picker/remotebridge/daemon/`:

1. `runMirror` (`daemon.go`): set `cfg.filter = newWindowFilter(cfg.Select)`
   right after `cfg.opts = newMirrorOpts()`. The startup `list-windows` uses
   `cfg.filter.listCmd(cfg.RemoteSession)` / `cfg.filter.parse`. Keep the
   empty-reply error. Then, when `cfg.filter != nil`, keep only `selYes` rows;
   none left → `hold.close()` and
   `return fmt.Errorf("daemon: no window of %s matches %s", cfg.RemoteSession, cfg.Select)`
   (before the listener; same shape as the "has no windows" return).
   `localWinForRemoteIndex` keeps the full list (placed selects nothing anyway).
2. `reconcileWindows` (`reconcilewindows.go`): list via `listCmd`/`parse`.
   `live[rw.id] = true` for **every** parsed row (selUnknown included). Unknown
   window: `mirrorNewWindow` only when `rw.sel == selYes`. Known window:
   `selNo` → `closeWindow` (log `daemon: %s stopped matching %s`);
   `selYes` → `applyMirrorName`; `selUnknown` → nothing. After a successful
   read with a filter, `cfg.filter.reconciled(time.Now())`. The empty-reply
   guard is unchanged.
3. `addWindow` (`daemon.go`): list via `listCmd`/`parse`; mirror only when the
   row for `remoteID` is in the reply **and** `sel == selYes`.
4. Subscription: `subscriptions.go` gains `selectSubName = "og_select"`.
   `subscribe()` in `runMirror`: when `cfg.filter != nil && cfg.filter.sel.subscribes()`,
   `cfg.filter.subscribed = sendSubscription(rt, selectSubName, "@*", "#{window_id}|"+cfg.filter.sel.flag())`.
   `dispatch`'s `SubscriptionChanged` case: `cfg.filter.observe(l)` (nil-safe).
5. Main loop (`runConn`, after `labels.flush`): `if cfg.filter.due(now) { cfg.filter.reconciled(now); reconcileWindows(flowCfg, …); if reg.empty() { return connEnd } }`.
   `reconciled` runs **before** the reconcile, so a failed or empty read waits
   for the next floor instead of retrying on every loop pass (a notification
   arriving meanwhile re-arms it). `reconcileWindows` also calls `reconciled`
   after a successful read, so a reconcile run for any other reason resets the
   backstop too.
6. `runLoop` (`reopen.go`): on `errServerReplaced`, **after** the existing
   `stopped(cfg.Shutdown)` check (a shutdown racing the replacement stays the
   silent `endReopen(cfg, "")`), when `cfg.Select` is an id selector (`!cfg.Select.IsZero() && !cfg.Select.subscribes()`), log
   `daemon: %s: %s names a window of the replaced server; not re-opening` and
   `endReopen(cfg, reopenFailedText(cfg.RemoteHost, cfg.RemoteSession))`, then
   `return nil`. (Placed only, since a selector requires `--into`.)

Unit tests (AC3), extending the existing harnesses (`reconcilewindows_test.go`,
`windowsweep_test.go`/`size_test.go`, `reopen_test.go`, `startupwindows_test.go`):
- filtered `reconcileWindows` with a scripted reply (`@1` flag 1, `@2` flag 0,
  `@3` flag `x`): registers only `@1`; the sent commands carry
  `AggressiveResizeOffCmd`/`ConvergeCmd` for `@1` only and never name `@2`/`@3`;
  `@3` is not closed when registered beforehand (malformed = leave it); a
  registered `@2` is closed (stopped matching); a registered window missing
  from a non-empty reply is closed; an errored/empty reply closes nothing.
- ownership rule: with a registry built by the filtered reconcile,
  one `watchLocalClient` size pass sends `ConvergeCmd` only for selected ids,
  plus the whole-client `ClientSizeCmd` (spec point 3).
- filtered `addWindow`: a `0` row is not mirrored, a `1` row is.
- startup: filtered `runMirror`-level helper (or `mirrorStartupWindows` caller)
  returns the "no window … matches" error when no row shows `1`. If the
  existing harness cannot drive `runMirror`'s list step, extract the
  selection into a small pure helper (`selectStartup(wins, filter)`) and test
  that.
- `runLoop` with an injected `run` returning `serverReplaced`: id selector →
  `run` called once, no re-open; option selector → re-opened (`run` called
  twice).
- `observe`: an `og_select` `%subscription-changed` line marks the filter
  dirty, an `og_labels` line and an `%output` line do not, a nil filter is a
  no-op (`note` → `due` timing is covered in Step 1).

### Step 3 — daemon command flag (`implement: haiku`)

`picker/remotebridge/cmd/daemon/main.go`: `sel := flag.String("select",
os.Getenv("OG_DAEMON_SELECT"), "…")`. After the `--into` checks: non-empty
`*sel` with empty `*into` → `fatal(fmt.Errorf("--select needs --into"))`;
`daemon.ParseWindowSelector(*sel)` error → `fatal`. Pass `Select: parsed` in
`daemon.Config`. A test beside the existing `validInto` test only if that file
already tests flag validation helpers; otherwise the bats daemon case in Step 6
covers the refusal.

### Step 4 — launcher (`implement: sonnet`)

`scripts/og-remote-open.sh`, inside the existing `--into` parse block and
nowhere in #990's lock/wait section:

- A pre-check ahead of the existing `--into` detection scans `$3..$#` for
  `--window` and `--into`. If `--window` is present:
  - no `--into` anywhere → `og-remote-open: --window needs --into`, exit 1.
    This runs before the dedicated `win` regex check (line ~255), which would
    otherwise report it as a bad window index.
  - `--into` present but neither `$3` nor `$4` is `--into` (so `--window`
    comes first, e.g. `h s --window @1 --into x`) → the usage line
    `og-remote-open: usage: og-remote-open <host> <sess> --into <local-session> [--window <selector>]`,
    exit 1. (`$4 == --into` keeps flowing into the existing block, whose
    "--into takes no window argument" message the existing test expects.)
- Inside the existing block, the `$win != --into || $# -gt 4` refusal becomes:
  `$win != --into` → the existing "--into takes no window argument"; otherwise
  allowed shapes are exactly 4 args, or exactly 6 with `$5 == --window`;
  anything else (a 5th arg that is not `--window`, a 7th arg, a repeated
  `--window`) → the usage line.
- `selector="${6:-}"`, validated with the two regexes (bash `=~` against a
  variable holding the pattern); refusal
  `og-remote-open: --window must be @<id> or @<option>=<value> ([A-Za-z0-9._/@+:-]), got: <%q>`.
- `sock_key`: `if [[ -n $selector ]]` the 5-field printf, else the existing
  4-field one unchanged.
- The lock-timeout message names the selector when set.
- Placed launch: `export OG_DAEMON_SELECT="$selector"`. Dedicated launch:
  `unset OG_DAEMON_SELECT` beside the existing `OG_DAEMON_*` exports.
- `shellcheck` + `shfmt` clean (the lint gate).

### Step 5 — launcher bats (`implement: sonnet`)

`tests/remote-cold-start.bats`, in the `into:` block. `install_into_fakes`
exports and truncates `SELECT_LOG` the way it does `LAUNCH_LOG` (the fake's
heredoc is quoted, so it only sees exported variables), and the fake daemon
also appends `select=${OG_DAEMON_SELECT-<unset>}` to `$SELECT_LOG`, leaving
`LAUNCH_LOG`'s line format unchanged. Plus an `into_sock_sel` helper (5-field
key):

- `--window @crew_branch=feat/x` launches one daemon with
  `OG_DAEMON_SELECT=@crew_branch=feat/x` on the 5-field socket.
- a repeat call with the same selector launches nothing (AC2 repeat no-op).
- two different selectors into one local session launch two daemons on two
  sockets, and neither equals the plain `--into` socket; a plain `--into`
  still uses the 4-field key (`into_sock`).
- refused before any ssh (no `ssh` in the fake's log, exit 1), message names
  the selector: `'@a=$(touch x)'`, `'@a=#{pid}'`, `'@a=b c'`, `'@a=b;c'`,
  `"@a=b'c"`, `'@01'`, `'@=x'`, `'@a='`, `'x=y'` (AC2 metacharacters).
- `--window X` without `--into` → "needs --into"; `--window` before `--into`,
  a 7th argument, and a repeated `--window` → usage line.
- a dedicated open with `OG_DAEMON_SELECT` in the caller's environment launches
  its daemon with it unset (dedicated fake path: assert from whatever the
  dedicated launch already logs; if it logs no env, add the select line to
  `$SELECT_LOG` there too).

### Step 6 — integration bats (`implement: sonnet`)

`tests/remote-m2-integration.bats`, after the `into:` cases. A helper
`bridge_up_select <tag> <src-sess> <selector> <want-panes> <paint-target>` —
`bridge_up_into` with `--select <selector>` and the pane count/paint target
taken from the arguments instead of the whole session (or a parameterised
`bridge_up_into`, if that keeps the existing callers unchanged). Cases
(prefix `into-select:`):

1. `@crew_branch=X` mirrors only the matching window (remote: 3 windows, one
   stamped X, one stamped Y, one unstamped); exactly one placed window, its
   panes mirror the X window (`sorted_dims` equal); `local_win_intact`.
2. a later `new-window` that is stamped X **after** creation (`new-window`,
   then `set -w @crew_branch X`) gets a mirror; a later window stamped Y does
   not (assert count stays 2 for 2s after the Y window's stamp, while a
   subsequent X window still arrives as the barrier).
3. two selectors (`@crew_branch=A`, `@crew_branch=B`) from one remote session
   into `host`: two daemons, two sockets, one window each. Observer attached at
   120x40 (content area via `list-clients` as in "a placed mirror resizes to
   the host session's client"); both remote windows reach that area within
   `RESIZE_CONVERGE_BUDGET_SECS`; then 20 samples 100ms apart of both remote
   windows' `#{window_width}x#{window_height}` all equal it; each mirror's size
   equals its remote's.
4. killing the selected remote window (of two selected) removes only its
   mirror; unsetting the option on the other (stopped matching) removes the
   last mirror and the daemon exits (`wait_daemon_exit`); `local_win_intact`.
5. SIGTERM on a filtered daemon removes only its windows; a second filtered
   daemon's window survives.
6. `@N` selector: mirrors exactly that window; a remote rename keeps it.
7. a selector matching nothing: the daemon exits non-zero, its log holds
   `no window of rem matches`, and `host` is untouched.
8. `--select` without `--into` refuses at startup (exit non-zero, message).

Run locally: `bats tests/remote-m2-integration.bats -f 'into'` (DAEMON/RENDERER
fall back to `go build`), and the same for `tests/remote-cold-start.bats -f into`.

### Step 7 — docs (`implement: sonnet`)

- `docs/agents/bridge-daemon.md` "Placed mirrors": a `#### Filtered mirrors
  (--window, #989)` block — grammar, the one-snapshot remote-evaluated flag,
  positive-evidence rules, `og_select` subscription and backstop, the filter
  sites and the "every other path is registry-keyed" list, emptied registry,
  server replaced (id vs option), bandwidth, limitations — including that the
  attach-time ownership claim and client size precede the startup selection
  read, so a selector that matches nothing (a typo) still claims
  `@og_bridge_owner` before the daemon exits, flagging other controllers until
  a survivor's release (`ownerReleaseGrace`, 30s); the same as today's "has no
  windows" exit, accepted rather than reordering the attach for one mode. And that a selection
  notification landing just before a failed read waits for the next backstop
  (30s subscribed), since the main loop marks the pass reconciled before it
  reads. "Size ownership
  across controllers": one bullet for filtered/shared-session sizing (the
  spec's point 3, including the session-level-claim deviation).
- `docs/agents/scripts.md` `og-remote-open` row: the `--window` argument, its
  refusals and ordering, `OG_DAEMON_SELECT`, the fifth key field only with a
  selector.

### Step 8 — commit the plan (AC1)

This file, committed with the code.

## Gates (run by the lead after execute)

- `cd picker && go test -race ./remotebridge/... && golangci-lint run ./remotebridge/...`
- `nilaway` as the flake runs it (via `nix flake check`'s `picker-go-lint`).
- `bats tests/remote-cold-start.bats -f into`, `bats tests/remote-m2-integration.bats -f 'into'`
- `nix build .#lint`, `nix flake check`, `nix build .#default` (AC6).

## Acceptance mapping

| AC | Where |
|---|---|
| AC1 | spec (committed), this plan (Step 8) |
| AC2 | Step 6 cases 1–5 (+6, 7); repeat no-op and metacharacters in Step 5 |
| AC3 | Step 1 and Step 2 unit tests (parse, match, ownership/cap rule) |
| AC4 | existing `into:` and dedicated suites, unchanged, run in the gates |
| AC5 | Step 7 |
| AC6 | gates; CI on the PR |
