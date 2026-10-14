# Plan: bridge size ownership (#979)

Spec: `docs/superpowers/specs/2026-10-14-bridge-size-ownership-design.md`.
Every step below implements a named part of it; read its Ownership rule,
Activity signal and Transitions sections before any step.

Gate commands (run from the repo root unless noted):

- Go, per step: `cd picker && go test -race ./remotebridge/... && go vet ./remotebridge/...`
- Lint, before review: `nix build .#lint`
- Full gate: `nix flake check` (bats + Go + golangci-lint + nilaway), `nix build .#default`
- Targeted bats while iterating: `cd picker && go build -o /tmp/og-979/daemon ./remotebridge/cmd/daemon && go build -o /tmp/og-979/renderer ./remotebridge/cmd/renderer`, then
  `DAEMON=/tmp/og-979/daemon RENDERER=/tmp/og-979/renderer bats -f '<name>' tests/remote-m2-integration.bats`

Rules for every step: match surrounding style (comment density, naming, `REPLY`
idioms do not apply to Go); comments only for a non-obvious why; no speculative
guards; never read `.env*`, credentials or keys, never print a secret.

## Step 1 — control-mode notifications (`implement: haiku`)

Files: `picker/remotebridge/controlmode/parse.go`, its `_test.go`.

- Add `ClientDetached` and `ClientSessionChanged` to the `Kind` enum (append
  after `SubscriptionChanged`, so existing values keep their numbers).
- `%client-detached <name>` → `Line{Kind: ClientDetached, Args: []string{name}}`.
- `%client-session-changed <name> $N <session name…>` → `Line{Kind:
  ClientSessionChanged, Args: []string{name, "$N"}, Data: <session name>}`; the
  session name may hold spaces (same shape as `%session-changed`: cut twice).
- Table-test both, including a session name with spaces and a truncated line
  (`%client-session-changed name` with no id → `Args` has what was there; do not
  panic).
- Every `switch l.Kind` in `picker/remotebridge` that lists all kinds
  exhaustively must still compile (`daemon.go` dispatch lists `Other, Begin,
  End, Error`; add the two new kinds to that no-op case for now — Step 4.6 gives
  them bodies). `grep -rn 'case controlmode.Other' picker/` to find them.

Done when: `go test ./remotebridge/controlmode/...` passes.

## Step 2 — pure ownership module (`implement: sonnet`)

Files: new `picker/remotebridge/daemon/sizeowner.go`, new
`picker/remotebridge/daemon/sizeowner_test.go`, `picker/remotebridge/daemon/size.go`
(converger accessor only). `ownership.go` already exists and belongs to the
local-session pin (#817: `type ownership int`, `owned`/`notOwned`); do not
touch it or reuse its names.

Contents (names are binding; Step 4 wires them):

- `const bridgeOwnerOption = "@og_bridge_owner"`, `const ownerSubName = "og_self"`,
  `const ownerSelfFormat = "#{client_name}|#{client_flags}|#{@og_bridge_owner}"`.
- `newOwnerToken() string`: 8 bytes from `crypto/rand`, lower-case hex (16 chars).
- `ownerValueRe = regexp.MustCompile(`^([0-9a-f]{16})\|([A-Za-z0-9_./-]{1,64})$`)`;
  `parseOwnerValue(v string) (token, client string)`: trims space; no match →
  `"", ""`.
- `type selfReport struct { client string; flagged bool; ownerToken, ownerClient string }`;
  `parseSelfReport(v string) (selfReport, bool)`: `strings.SplitN(v, "|", 3)`;
  fewer than 2 fields or an empty client → false. `flagged` = the
  comma-separated flags field contains exactly `ignore-size`. Third field (may
  be missing or empty) goes through `parseOwnerValue`.
- Command builders (all fire-and-forget strings):
  - `ignoreSizeCmd(client string, on bool) string`: `refresh-client -f ignore-size`
    / `refresh-client -f !ignore-size` for `client == ""` (self); with a client,
    `refresh-client -t <tmuxQuote(client)> -f ignore-size`.
  - `ownerClaimCmd(sessID, token, client string) string`:
    `set-option -t <tmuxQuote(sessID)> @og_bridge_owner <tmuxQuote(token+"|"+client)>`.
  - `ownerReleaseCmd(sessID, token, client string) string`:
    `if-shell -F -t <tmuxQuote(sessID)> <tmuxQuote("#{==:#{@og_bridge_owner},"+token+"|"+client+"}")> <tmuxQuote("set-option -u -t "+tmuxQuote(sessID)+" @og_bridge_owner")>`.
    Only called with a `client` that passed `parseOwnerValue` (charset has no
    `#`, `,`, `}` or quote).
  - `ownerReadCmd(sessID string) string`:
    `display-message -p -t <tmuxQuote(sessID)> <tmuxQuote(ctlSafe("#{client_name}|#{@og_bridge_owner}"))>`.
  - `sessionClientsCmd(sessID string) string`:
    `list-clients -t <tmuxQuote(sessID)> -F <tmuxQuote(ctlSafe("#{client_name}|#{client_control_mode}|#{client_flags}"))>`.
- `type sessionClient struct { name string; control, flagged bool }`;
  `parseSessionClients(body string) []sessionClient` (skip blank/malformed rows).
- `enforceTargets(clients []sessionClient, self string) []string`: names of
  control-mode clients that are not `self` and not flagged, in input order.
- `reconcileCmd(r selfReport, token string) (cmd string, ok bool)`: the three
  reconcile rows of the spec — owner ours & flagged → `ignoreSizeCmd("", false)`;
  owner another token & unflagged → `ignoreSizeCmd("", true)`; owner unset &
  flagged → `ignoreSizeCmd("", false)`; else `"", false`.
- `foreignOwner(ownerToken, myToken string) bool`: true iff the owner token is
  non-empty and not mine (used by `primeClient` and `repair`).
- Activity: `type activityRow struct { focused bool; stamp int64 }` (defined
  here; Step 3 builds rows from the reveal poll). `type activityMark struct { seeded bool; high int64 }` with
  `advance(rows []activityRow, now int64) bool`: on the first call record the
  max stamp, set seeded, return false. Later: a row counts iff `focused` and
  `stamp > high` and `stamp >= now-3`; return whether any counted; always raise
  `high` to the max stamp seen (counted or not).
- `type sizeOwner struct` holding, under one `sync.Mutex`: `token`; `self`
  (current connection's client name); `owner` (last owner token/client seen,
  from a report, a read or our own claim); `ownedBefore bool` (set by a claim,
  cleared when a report names another token — what the new-connection row's
  "owned before the drop" reads); `pendingReport *selfReport` (newest queued
  only); `claimWanted bool`; `members map[string]bool`; `mark activityMark`; and
  a `wake chan struct{}` (buffered 1, non-blocking send). Methods, each a few
  lines: `isOwner() bool` (owner token == mine); `queueReport(v string)`
  (parse; keep newest); `takeReport() (selfReport, bool)`; `noteActivity(rows,
  now)` (advance && !owned → claimWanted=true, signal wake); `takeClaim() bool`;
  `noteClaim(self string)` (owner=mine+self, ownedBefore=true);
  `setMembers(names []string)`, `dropMember(name) bool` (reports whether it was
  present), `addMember(name)`; `setSelf(name)`.
- `size.go`: `func (c *converger) asserted(key string) (w, h int, ok bool)` and
  `recalcCmd(cv *converger) (string, bool)` returning `ClientSizeCmd` at the
  recorded `clientSizeKey` size.

Tests (table-driven, `sizeowner_test.go`): owner value parse (good, bad token
length, uppercase hex, bad charset `#`/`,`/space, empty); self report parse
(flags with and without `ignore-size`, a flag like `ignore-size-x` must not
count, owner missing/empty/garbage); every builder's exact string, including a
client name with `/`; `enforceTargets` skips self, tty clients and already
flagged; `reconcileCmd` all rows **including** the spec's unit-only case
"owner ours while flagged → unflag"; `activityMark`: first call seeds, a
focused fresh stamp advances, an unfocused fresh stamp does not (focus-out),
a focused stale stamp (`now-10`) does not (moved-in client), equal-to-high does
not; `sizeOwner` queue keeps the newest report; `noteActivity` while owned does
not want a claim; `dropMember` membership semantics.

Done when: `go test -race ./remotebridge/daemon/ -run 'SizeOwner|OwnerValue|ActivityMark|SelfReport|Enforce|Reconcile|Recalc'` passes (name the new tests to match).

## Step 3 — activity rows from the reveal poll (`implement: sonnet`)

Files: `picker/remotebridge/daemon/reveal.go`, `reveal_test.go`.

- `clientViewsFormat` gains `|#{client_flags}|#{client_activity}` at the end
  (5 `|` separators total). Keep `#{window_id}` the 4th field.
- `clientViews` splits with `SplitN(line, "|", 6)`, requires at least 4 fields,
  and keys/values exactly as today (window id = field 4 only).
- New `activityRows(out string) []activityRow`: same row skipping as
  `clientViews` (control-mode, blank, malformed, unparsable stamp); `focused`
  from the flags field containing exactly `focused`.
- `watchReveal` takes one more parameter `activity func(rows []activityRow, now int64)`;
  a nil `activity` is skipped (tests, and the `daemon.go` caller until Step 4
  passes a real one — pass `nil` there in this step). Each tick it
  takes `now := time.Now().Unix()` **before** calling `query()`, then calls
  `activity(activityRows(out), now)` after the reveal handling.
- Update the existing reveal tests' fixture rows to the new format, and add
  `activityRows` cases (focused/unfocused, control row skipped, bad stamp).

Done when: `go test -race ./remotebridge/daemon/ -run 'Reveal|ClientViews|Activity'` passes.

## Step 4 — wire ownership into the daemon (`implement: opus`, high-risk)

Files: `picker/remotebridge/daemon/daemon.go`, `conn.go`, `subscriptions.go`,
`reopen.go` (only `Run`). Depends on Steps 1–3.

1. **Lifetime.** `Config` gains unexported `own *sizeOwner`. `Run` sets
   `cfg.own = newSizeOwner(newOwnerToken())` before `runLoop`, so the token and
   activity mark survive re-opens. `runMirror` treats `cfg.own == nil` (tests
   that call it directly) or `pin.id == ""` as "ownership off": every hook below
   is skipped. Put that in one helper (`ownerOn := cfg.own != nil && pin.id != ""`).
2. **Helpers on the main loop** (closures in `runMirror`, beside `repair`; all
   round-trips use `rt`, sends use `sendCtl`; log one stderr line per claim,
   self-flag and release, `daemon: size owner: …`, for bats diagnosis):
   - `enforce()`: `one(rt, …)` cannot carry two commands; use `rt` with
     `ownerReadCmd` + `sessionClientsCmd` and read both replies (the
     `primeClient` batch pattern), `setSelf`, `setMembers` (every other client
     name), send `ignoreSizeCmd(name, true)` for each `enforceTargets` name,
     then `recalcCmd` if any was sent and it is ok. Returns the read owner value
     too (for the re-open rule).
   - `claim()`, in exactly this order: the read batch (`ownerReadCmd` +
     `sessionClientsCmd`, giving self, owner and the session's clients) →
     `ownerClaimCmd(pin.id, token, self)` → `ignoreSizeCmd("", false)` → one
     `ignoreSizeCmd(name, true)` per `enforceTargets` name → one `recalcCmd`
     (if recorded). `noteClaim(self)`, `setMembers`. Factor the read batch out
     (`readOwnerState()`) so `enforce`, `claim`, the re-open rule and `repair`
     share it.
   - `applyReport()`: `takeReport`; if a report, `setSelf(r.client)`, update
     `owner`/`ownedBefore`, and if `reconcileCmd` gives a command send it plus
     `recalcCmd`.
3. **First attach.** In `runMirror`, right after `hold.set(c)` and before the
   `ClientSizeCmd` send at the "first thing the remote might act on" block:
   if `ownerOn` and `!cfg.reopened` → `claim()`. If `cfg.reopened` → read the
   owner (via `enforce`'s batch without flagging — split the read from the
   flagging so it can be reused): owner token another → `ignoreSizeCmd("", true)`;
   unset and no other control-mode client on the session → `claim()`; else
   nothing. Ordering note from the spec: no recalc is needed here when nothing
   is recorded; the size sends that follow recalculate.
4. **Subscription.** Leave `subscribeFormats` and its 4-value return alone
   (`subscriptions_test.go` and `ctlsafe_test.go` pin it). In the `subscribe`
   closure in `runMirror`, add `if ownerOn { sendSubscription(rt, ownerSubName,
   "", ownerSelfFormat) }` (it applies `ctlSafe` itself). `subscribe()` already
   runs at first attach and at the end of `repair`, so nothing else is needed. In `dispatch`'s `SubscriptionChanged` case,
   `if ownerOn`, `subscriptionValue(l, ownerSubName)` → `cfg.own.queueReport(v)`.
5. **Main-loop pass.** In `runConn`, at the top of each pass after `settle`:
   `applyReport()`, then `if cfg.own.takeClaim() { claim() }` (both only when
   `ownerOn`). Add a `select` case on a local `var ownWake <-chan struct{}`,
   set to `cfg.own.wake` only when `ownerOn` — never `cfg.own.wake` directly,
   which would dereference a nil `cfg.own` in the tests that call `runMirror`
   without `Run`. A nil channel case never fires.
6. **Notifications** in `dispatch` (all only when `ownerOn`):
   - `ClientDetached`: `name := l.Args[0]`; if `name` equals the last known
     owner client and the owner token is not ours → send `ownerReleaseCmd`; if
     `isOwner()` and `dropMember(name)` → send `recalcCmd`.
   - `ClientSessionChanged`: `name, sid := l.Args[0], l.Args[1]`; skip our own
     name. `sid == pin.id`: `addMember(name)`; if `isOwner()` → `enforce()`.
     Otherwise: if `isOwner()` and `dropMember(name)` → send `recalcCmd`.
7. **New connection (repair).** At the top of `repair`, before `cv.reset()`'s
   size sends: read the owner on the new connection (`enforce`'s read). Owner
   token mine, or unset and `ownedBefore` → `claim()`. Another token →
   `ignoreSizeCmd("", true)` **before** the client-size send. Unset and not
   owned before → nothing.
8. **primeClient** (`conn.go`): the new connection asserts sizes before
   `repair` runs, and `replaceConn` closes the old connection
   (`hold.close()`) **before** `repair`, so whatever the new client must be
   told has to ride the prime batch. Prepend one round-trip on `c.rt`:
   `ownerReadCmd(sessID)` (it returns the new client's name and the owner).
   Owner token mine → prepend `ownerClaimCmd(sessID, token, <new client
   name>)` to the existing `cmds` batch, so the record names the new client
   before the old one detaches and survivors' release condition no longer
   matches (otherwise every viewer-switch replacement with a second
   controller present would release and snap the size). `foreignOwner` →
   prepend `ignoreSizeCmd("", true)`. `primeClient` needs the session id and
   token: pass them in (`primeClient(cfg, c, reg, ownerSess, ownerToken
   string)`; empty `ownerSess` → skip, matching ownership off). Keep its
   reply-count discipline: every command in the batch, and the read, has its
   reply claimed. No `noteClaim` here: `repair`'s own claim (4.7) runs right
   after, re-reads our token, records the claim and re-flags strays.
9. **Activity.** The `watchReveal` call passes
   `func(rows []activityRow, now int64) { cfg.own.noteActivity(rows, now) }`
   when `ownerOn`, else `nil` (replacing Step 3's placeholder `nil`).
   `noteActivity` only flips `claimWanted` and signals `wake`; the claim itself
   runs on the main loop (round-trips are main-loop-only).

Tests (`daemon_test.go` or a new `ownership_wiring_test.go`, using the existing
fake-stream harness the `watchLocalClient`/`repair` tests use — read
`TestWatchLocalClientConvergesActiveWindowFirst` and the `replaceconn_test.go`
fakes first): `primeClient` prepends `-f ignore-size` when the read names
another token, prepends the claim with the **new** client's name when it names
ours, and neither when unset; reply counting still matches (a following
round-trip on the same fake connection gets its own reply). Wiring beyond that
is covered by Step 5's bats.

Done when: `go test -race ./remotebridge/...` passes and `go vet` is clean.

## Step 5 — bats regressions (`implement: opus`)

File: `tests/remote-m2-integration.bats` only. Depends on Step 4.

Shared helpers (new, near `bridge_up`):

- `teardown` also kills `m2dst2` and `m2obs` (`m2obs` is already there).
- `DST2="tmux -L m2dst2 -f $DST_CONF"` set in setup.
- `obs_attach <obs-session> <w> <h> <tmux-cmd…>`: `$OBS new-session -d -s <obs-session> -x <w> -y <h> "<cmd> attach -t host-sess"`
  (or `attach -t rem` for a human on SRC) and wait until the target server
  lists one more client; this is the pty-hosted real client from the #201 test.
- `type_into <obs-session>`: `$OBS send-keys -t <obs-session> ' '` (one key is
  activity; the space reaches the remote shell, harmless).
- `src_sizes`: `$SRC list-windows -t rem -F '#{window_width}x#{window_height}'`.
- `wait_src_all <want>`: poll `src_sizes` until every line equals `<want>`
  within `RESIZE_CONVERGE_BUDGET_SECS`; on timeout print sizes, both daemon logs
  (`tail -40`) to fd 3 and fail.
- `hold_src_all <want> <secs>`: assert every line equals `<want>` at every
  0.2s sample for `<secs>` seconds.
- Each daemon runs with its own `--sock`, `--dst-socket` and log, as
  `bridge_up` does; derive the expected window size from what SRC shows once a
  single bridge has converged (status line rows differ between configs — never
  hard-code the height; read `#{window_width}x#{window_height}` after each
  single-controller convergence and reuse it).

Cases:

1. `@test "the last-active controller sizes a shared mirror (#979)"`: SRC `rem`
   with two windows (`new-window`; window 1 current). DST host-sess 100x30 with
   an OBS client at 100x30 (small controller S); DST2 host-sess 150x45 with an
   OBS client at 150x45 (big controller B). Start S, wait for convergence, note
   `small`. Start B, note what SRC settles at (on `origin/main` this is `small`;
   with the fix B's first-attach claim makes it `big`). Then `type_into` S →
   `wait_src_all small`; `type_into` B → `wait_src_all big` (**red on
   origin/main: stays small**), both windows.
2. `@test "an idle controller does not reclaim when it wakes (#979)"`: same
   setup, end with B owning (`type_into` B, `wait_src_all big`). `kill -STOP` S's
   daemon, `type_into` B, `kill -CONT` S → `hold_src_all big 5`. Force S to
   reconnect: `kill -9` S's transport child (`transport_child` reads the global
   `daemon_pid`; add a `transport_child_of <pid>` variant and leave the
   original as a wrapper) → wait for S's log to show the reconnect (or 5s) →
   `hold_src_all big 5`. `type_into` S → `wait_src_all small`.
3. `@test "a sleeping controller's client moved onto the session does not size it (#979)"`:
   `$SRC set -g detach-on-destroy off`. SRC sessions `rem` (B mirrors it) and
   `rem-a` (S mirrors it: `--session rem-a`). Both up and converged (B's `rem`
   at `big`). `kill -STOP` S's daemon; `$SRC kill-session -t rem-a`; assert S's
   control client now lists on `rem` (`$SRC list-clients -t rem` count grows);
   `wait_src_all big` then `hold_src_all big 3` (**red on origin/main: the
   current window drops to S's size**). `kill -CONT` S at the end so teardown is
   clean.
4. `@test "a non-current mirrored window converges after a client detaches (#979)"`:
   one bridge only. SRC `rem` two windows; a human OBS client on SRC at 100x30
   (`$SRC attach -t rem`); DST host-sess 150x45, daemon up. Wait until SRC
   windows settle (`small` = the human's clamp). Detach the human only:
   `$OBS kill-server` (never `detach-client -s rem`, which detaches the bridge
   too and would make the red run fail for the wrong reason); assert the
   bridge's control client is still listed on `rem` afterwards →
   `wait_src_all big` for **both** windows (**red on origin/main: the
   non-current window stays small**).

Every case: kill both daemons and wait for them in a cleanup path that also
runs on failure (each existing test's `kill`/`wait` pattern; S may be stopped,
so `kill -CONT` before `kill`). Run `shellcheck` on the file.

Done when: the four cases pass with the branch daemon, and each red-marked
assertion fails with an `origin/main` daemon (Step 7 records that).

## Step 6 — docs (`implement: sonnet`)

Files: `docs/agents/bridge-daemon.md`, `picker/remotebridge/daemon/size.go`
comments.

- New `## Size ownership across controllers (#979)` section in
  `bridge-daemon.md`, after "Bridge Reconnect" and before "What the Remote Host
  Needs on PATH": the tmux mechanism in three sentences (min of caps;
  `ignore-size` + recalculation; deferred resize only for current windows), the
  ownership rule (owner record, `og_self` reconcile, enforce, claim, release),
  the activity signal and its two guards, the transitions table condensed, and
  the edge cases (all-flagged freeze, iTerm2 `-CC` flagged, mixed versions,
  flagged `w->latest` with a human). Line-wrapped like the surrounding bullets.
- `size.go`: `ConvergeCmd`'s doc gains one sentence: with several control
  clients the caps combine as a minimum, which size ownership (`sizeowner.go`)
  resolves by flagging every non-owner client `ignore-size`.

## Step 7 — evidence (lead, not delegated)

- AC1: re-run the scratch repro (`repro.sh old` / `fix`) and paste the trimmed
  output into the PR's `## Testing`.
- Red/green: build daemon+renderer from `origin/main` in a scratch worktree
  (`git worktree add <scratch>/main origin/main`), run the four new cases with
  `DAEMON=<main daemon> RENDERER=<main renderer>` against the branch's test
  file, record each red assertion; then green with the branch build. Remove the
  scratch worktree after.
- AC3 report: in the red run of case 4, also `$DST select-window` the
  non-current mirror and record that the remote window stays small (visiting
  does not heal on `origin/main`).
- Full gate: `nix build .#lint`, `nix flake check`, `nix build .#default`.
- AC6: the spec and this plan are committed with the code.

## Ordering

Steps 1 ∥ 2 → 3 → 4 → 5 ∥ 6 → 7 (Step 3 uses Step 2's `activityRow`).
