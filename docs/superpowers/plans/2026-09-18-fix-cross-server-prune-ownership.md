# Fix: config-load prune can delete a live, different server's agent state (#676)

## Problem

`config/tmux.conf.reference.nix:950` / `config/tmux.conf.tmpl:590`'s
`run-shell "tmux-update-icons #{qs:session_name}"` runs unconditionally at
config load — not client-gated, unlike the status-format[0] call site
(`reference.nix:639` / `tmpl:333`). That call reaches
`claude_prune_stale_state` (`scripts/lib-claude.sh`), whose only staleness
test is `file mtime < this booting server's start_time`. Under the shared
`CLAUDE_STATUS_DIR` (`/tmp/claude-status`, every wrapped-tmux server on the
machine), a second server's very first boot deletes a live, different
server's state the instant that server hasn't touched a file in the last few
seconds — with no ownership check at all. `CLAUDE.md` currently (and
incorrectly) claims this call site is client-gated in two places.

## Root cause

Timing-based staleness (`mtime < my start_time`) cannot distinguish "written
by a server that has since died" from "written moments ago by a different,
still-running server." Only per-file ownership can.

## Fix

Stamp each `panes/<id>` file with the PID of the tmux **server** that wrote
it (`#{pid}`, alongside the existing `session=` field), and teach
`claude_prune_stale_state` to protect any pane id whose recorded owner PID
is a still-alive, different process — across all 8 pane-keyed dirs it
sweeps. Justification for extending protection to the non-`panes/` dirs: an
id with a live foreign owner must never be reaped anywhere under it, full
stop; the cost is that this server's own same-id leftovers in
`names/`/`tasks/`/etc. can survive one extra generation when no
`panes/<id>` sibling exists to attribute them — not because a pane id is
provably unique to one server (it isn't; two servers can share the same
`%N`, as `tests/pane-shell-prompt.bats`'s own cross-server test proves).

## Steps

- [ ] **Step 1: `scripts/claude-status-update.sh` — stamp `server=<pid>` (conditionally)**
      When writing `panes/<id>` (~line 582-586), add a `server=$server_pid`
      line **only when `server_pid` is non-empty** — same
      append-only-if-set shape as the existing `unseen_line`/
      `transcript_line` variables, placed right after `session=$session_name`
      and before those. This is required, not stylistic:
      `tests/notify-producers.bats:135-147` pins the exact key list
      (`state`/`timestamp`/`session`/`unseen`) of a pane file written with an
      explicit `--session` against a fake tmux with no `#{pid}` arm (so
      `server_pid` resolves empty there) — an unconditional `server=` line
      breaks that assertion regardless of where it's fetched from. An empty
      `server_pid` already means "unprotected" under Step 2's rule, so
      omitting the line is exactly equivalent, not a behavior change.
      Derive `server_pid`:
      - If `session_name` is still empty at that point (the common case —
        the CC plugin's own hook calls never pass `--session`), replace the
        existing single-field `tmux display-message -p -t "$pane_id"
        '#{session_name}'` call (~line 478-480) with one call fetching
        **`'#{pid}|#{session_name}'`** — **pid first, session_name last**.
        A tmux session name may legally contain `|`; `IFS='|' read -r
        server_pid session_name` assigns bash `read`'s unsplit remainder
        (delimiters and all) to the LAST variable, so a `|` in the name
        lands inside `session_name` instead of truncating it and shifting
        `server_pid` into garbage. Same rule already documented one file
        over, in `tmux-update-icons.sh` (~line 170-172): "session_id is $N
        and cannot contain '|'; session_name can... parse names as the
        remainder after the first '|' so a pipe in the name cannot shift
        [it]." Getting the field order backwards here would write a
        truncated `session=` that breaks `claude_reap_pane`'s and
        `claude_clear_agent_state`'s existing ownership checks (acceptance
        criterion 3). Keep the existing `2>/dev/null || true` on this call.
      - If `session_name` was already given explicitly (`--session`), fetch
        `server_pid` with a second, **target-free** call: `tmux
        display-message -p '#{pid}' 2>/dev/null || true` (`#{pid}` is the
        server's own PID, independent of `-t`) — avoids depending on
        `$pane_id` being a resolvable target when the caller already knows
        the session. `2>/dev/null || true` matters here too: several bats
        suites exercise the raw script outside any tmux server, where this
        call would otherwise print "no server running" to stderr.
      - Either path: gate on `command -v tmux` (existing pattern); on
        failure/no tmux, `server_pid` stays empty → line omitted.
      Do not touch the `issue`/`task`/`name`/`clear`/`cleanup`/`mark-seen`
      early-exit branches — they never reach the panes/ write.

- [ ] **Step 2: `scripts/lib-claude.sh` — ownership-aware prune**
      Change `claude_prune_stale_state` to
      `claude_prune_stale_state SERVER_START [SERVER_PID]` — `SERVER_PID` is
      a new, **optional**, second positional arg. Omitting it must reproduce
      today's exact behavior byte-for-byte (existing 1-arg callers/tests stay
      untouched).
      When `SERVER_PID` is non-empty:
      - Before the deletion sweep, scan `CLAUDE_PANES_DIR` once. For each
        `panes/<id>` file, read its `server=` field (same
        `while IFS='=' read` pattern already used for `session=` elsewhere
        in this file). If it's numeric, not equal to `SERVER_PID`, and
        `kill -0 "$owner" 2>/dev/null` succeeds — mark that bare id
        **protected** in an associative array. Redirect stderr on the
        `kill -0` call: an EPERM (a live process owned by a different user)
        prints "Operation not permitted" that would otherwise leak onto the
        config-load `run-shell`'s stderr; EPERM also means no protection is
        granted there (falls through to mtime), though `/tmp/claude-status`
        permissions make that case mostly unreachable in practice.
      - In the existing per-dir deletion loop (all 8 dirs), skip any file
        whose bare id is in the protected set, before the mtime check —
        regardless of mtime.
      - A `panes/<id>` file with no `server=` field (legacy, or written by a
        pre-upgrade server that predates this field), or whose owner PID is
        dead, gets no protection and falls through to the existing
        mtime-only check — same for any id that has no `panes/<id>` sibling
        at all (e.g. a screen-only pane with no hook-based state, or a
        remote-bridge-mirrored pane — see Step "Go writer" below for why
        that case is covered instead of accepted as a gap).
      - Doc-comment note: `kill -0` proves *a* process exists, not that it
        is specifically a tmux server — after a reboot or long uptime, a
        reused PID could protect a dead generation's id "forever" (until
        that PID also dies), meaning a restored pane reusing that id could
        keep inheriting a stale name/task one generation longer than ideal.
        This fails **safe** (under-reap, never over-delete), which is the
        posture the fix needs; state this directly rather than implying
        ownership is exact.
      - Doc-comment note: the marker gate is per-machine, not per-server
        (`.server_start` is one file in the shared dir), so two servers
        alternating boots ping-pong it and each one's config-load prune can
        re-run on every source of this file rather than once per boot —
        pre-existing behavior, now also paying one `panes/*` scan + one
        `kill -0` per recorded owner per re-run. Cheap (builtin `kill`,
        small files) but worth one sentence so it isn't rediscovered later.
      Update the function's doc comment to describe the ownership check and
      correct the "prunes... only a previous tmux server's files" framing.

- [ ] **Step 3: `scripts/tmux-update-icons.sh` — thread SERVER_PID through**
      In `main()`, add a new positional `SERVER_PID` (after the existing
      `$1`..`$5`), falling back to `tmux display-message -p '#{pid}'` when
      not passed — same fallback shape as `$3`/`SERVER_START` already uses.
      Call `claude_prune_stale_state "$SERVER_START" "$SERVER_PID"`.

- [ ] **Step 4: pass the pid from the hot path — BOTH conf files**
      Append `'#{pid}'` as the new 6th positional argument to the
      status-format[0] call site, in **both**
      `config/tmux.conf.reference.nix:639` and `config/tmux.conf.tmpl:333`,
      after `'#{@catppuccin_flavor}'` — these two files must be edited
      together per `tmux.conf.reference.nix`'s own header ("THE TWO FILES
      MUST BE EDITED TOGETHER... or the check goes red on a difference that
      is not a bug"). `tmux-conf-extraction-assertions` (`flake.nix:870-877`)
      doesn't line-diff the two files directly — it renders the generator's
      output from `tmux.conf.tmpl` and `diff -u`s that rendered text against
      the frozen `referenceConf`, across a 12-entry option matrix — so the
      real requirement is that both edits *render* identically; a symmetric
      insertion in both satisfies that. `tmuxConf = generatedConf`
      (`config/tmux.conf.nix:978`): the **template** is what actually ships,
      so `tmpl:333` is the functional edit and `reference.nix:639` is what
      keeps the extraction check green. This avoids the per-tick poller
      paying the fallback fork.
      **Leave the config-load call site (`reference.nix:950` /
      `tmpl` equivalent) unchanged in both files** — it already relies on
      `main()`'s live-query fallback for `$3`, and the same fallback now
      covers the new positional for free; that's the site the bug lives in,
      and it gets the fix automatically through Step 3's fallback.

- [ ] **Step 5: Go writer — `picker/remotebridge/daemon/agentstatus.go`**
      `agentShipper.stamp` (~line 292) also writes `panes/<id>` — for a
      *local* mirror pane, from the remote-bridge daemon, on the box running
      the bridge — with `state=/timestamp=/session=` and **no** `server=`.
      Because it deliberately skips rewriting unchanged rows (documented in
      CLAUDE.md's "Remote Agent Status"), these files routinely sit for
      minutes with an old mtime. Left unfixed, every bridged/mirrored pane's
      state would fall into the "no `server=`" bucket and a second server's
      boot could still delete a live server's *mirrored* agent state —
      acceptance criterion 1 ("can never delete... a currently-live,
      different tmux server") would not actually hold in this repo. Fix it:
      - Add a `localPID string` + `localPIDResolved bool` pair to
        `agentShipper`, and a method `func (a *agentShipper) localServerPID(cfg
        Config) string` that resolves once (via `cfg.LocalTmuxOut("display-message",
        "-p", "#{pid}")` — confirmed signature `func(args ...string) (string,
        error)`, `daemon.go:40` — trimmed) and caches on the receiver —
        mirrors the shipper's existing "resolve once, reuse" shape (e.g.
        `skew`). Guard `cfg.LocalTmuxOut == nil` (existing test fakes such as
        `mirrorCfg` in `agentstatus_test.go` don't set it) → returns `""`.
      - In `stamp`, after building the existing `state=/timestamp=/session=`
        body (line 292), append `server=<pid>\n` only when
        `localServerPID(cfg)` is non-empty — same conditional-emission
        rule as Step 1, and for the same reason:
        `agentstatus_test.go:131`'s `TestAgentShipperApply` asserts the
        exact body string, and `mirrorCfg` (used by most tests in that
        file) never sets `LocalTmuxOut`, so `localServerPID` returns `""`
        there and those tests are unaffected by construction.
      - Add one new test exercising the positive path: a `Config` whose
        `LocalTmuxOut` returns a fixed fake pid, asserting the written body
        includes `server=<that pid>\n`.

- [ ] **Step 6: `CLAUDE.md` — correct the invariant (both sites)**
      The false claim is **not** in "Remote Agent Status" — it's at two
      other spots:
      - `CLAUDE.md:49` (Script Roles table, `tmux-update-icons` row):
        "...only from the per-tick, **client-gated** status-format/
        config-load call sites, never from the monitor hook..." — the
        config-load call site is *not* client-gated (never was); correct
        this to say the config-load site is unconditional and always ran on
        every boot, and that safety now comes from the per-file `server=`
        PID-liveness ownership check (Step 2), not from client-gating.
      - `CLAUDE.md:1094` ("Every hand-rolled tmux repro..."): "...its
        per-session tick (status-format / config-load, **whenever a client
        attaches**) prunes and reaps..." — same false gating claim for the
        config-load half; correct similarly (the status-format half's
        client-gating is real and unaffected; only the config-load framing
        is wrong).
      Keep `lib-claude.sh:87-95`'s own doc-comment fix from Step 2 in sync
      with whatever wording lands here.

- [ ] **Step 7: regression tests**
      - `tests/prune-stale-state.bats` (unit-level, `claude_prune_stale_state`
        directly, no real tmux needed):
        - a stale `panes/<id>` with `server=$$` (bats' own PID — provably
          alive) survives when the `SERVER_PID` passed is something else;
        - a stale `panes/<id>` with `server=<dead pid, e.g. 2147483647>` is
          still reaped (mtime-based, as before);
        - protection on `panes/<id>` extends to a sibling `names/<id>` (or
          similar) file under the same id;
        - a legacy `panes/<id>` with no `server=` field keeps the old
          mtime-only behavior;
        - all existing 1-arg `claude_prune_stale_state "$SERVER_START"`
          calls in this file are unchanged (proves the 2nd arg is additive).
      - `tests/pane-shell-prompt.bats` (the actual issue repro, through the
        real production entry point): this file already has two-real-server
        infrastructure (`t`/`tb`, `SHIM_DIR` PATH-routing, `TMUX_BIN`,
        `SCRATCH_SOCK`-keyed teardown) built for its own "cross-server
        guard" test (#675), which deliberately seeds alpha's file **after**
        beta boots to dodge this exact race — do not touch that test or its
        ordering (#675's own isolation fix). Add one **new** `@test` in the
        same file, reusing the harness:
        1. `SCRATCH_SOCK="$BATS_TEST_TMPDIR/scratch.sock"` must be set
           before the first `tb` call, matching the existing test's own
           pattern (teardown keys on it);
        2. boot alpha (`t new-session -d -s alpha ...`), get its pane id
           `a_id` (`%N` form, per `bare_id`);
        3. seed alpha's `panes/<id>` via the real writer, routed to alpha's
           own socket: `CLAUDE_STATUS_DIR="$CLAUDE_STATUS_DIR"
           PATH="$SHIM_DIR:$PATH" bash scripts/claude-status-update.sh
           processing --pane "$a_id"` (the `%N` form, not bare — `-t
           "$pane_id"` target resolution is the fragile part per the file's
           own `seed_shell_state_via_writer` comment; `CLAUDE_STATUS_DIR`
           must be threaded through explicitly, matching that same
           helper), no `--session`, so it round-trips through alpha's real
           socket (the `$SHIM_DIR/tmux` wrapper execs `$TMUX_BIN -L
           "$SOCKET"`) and picks up alpha's real server PID;
        4. backdate that file's mtime ~30s (matching the issue's repro:
           `touch -d "@$(($(date +%s) - 30))"`);
        5. **positive control against the marker-gate race**: also seed an
           unowned, backdated decoy file at an id that cannot collide with
           a real pane (e.g. `names/99999` — alpha and beta both open
           `%0`), backdated the **same** ~30s as alpha's file. This must be
           deleted by *any* prune pass, fixed or not, so a green outcome
           cannot come from beta's config-load prune silently no-op'ing
           (its marker-gate short-circuits if `#{start_time}` happens to
           collide with alpha's, e.g. both boot in the same wall-clock
           second — genuinely possible in a fast test run: `[[ -r $marker
           && $(<"$marker") == "$server_start" ]] && return 0` in
           `lib-claude.sh`). Assert the decoy IS gone and alpha's
           `panes/<id>` file (with its original content) survives — the
           pairing proves the sweep actually ran *and* the ownership guard
           actually held, not merely that nothing happened. `sleep 1`
           before booting beta, or capture+compare `#{start_time}` between
           alpha and beta, to additionally reduce (not just detect) the
           same-second collision;
        6. boot beta (`tb new-session -d -s beta ...`), which synchronously
           runs beta's config-load prune;
        7. assert alpha's `panes/<id>` file and its content still exist,
           and the decoy is gone.
        This is the issue's exact deterministic repro becoming a regression
        test — red on current `main` (or a change that skips Steps 1-5), and
        provably exercised (not vacuous) both before and after the fix.

- [ ] **Step 8: evidence + gate**
      Record in the PR `## Evidence` section: the violated invariant (a
      config-load prune must never delete a currently-live different
      server's state), the new `pane-shell-prompt.bats` test name, that it
      fails against pre-fix code and passes after (with the decoy-file
      positive control proving the sweep ran both times), and the residual
      transitional-exposure note (files written by a server that predates
      this upgrade carry no `server=` and stay mtime-prunable until that
      server's agents write again — same shape as any format migration).
      Run `nix build .#default`, `nix flake check`, `nix build .#lint`
      before push.

## Non-goals / explicitly out of scope

- Extending ownership to `screen/`, `tasks/`, `names/`, `issues/`,
  `interrupt/`, `watchers/`, `live/` entries that have **no** corresponding
  `panes/<id>` file at all (a screen-only pi/codex/cursor pane with no hook
  state, not the remote-bridge case — that's covered by Step 5). These
  still rely on mtime-only pruning. Noted as a residual, narrower gap in the
  PR body rather than fixed here, to keep the diff scoped to the issue's
  actual repro and acceptance criteria.
- Any change to `tests/pane-shell-prompt.bats`'s existing "cross-server
  guard" test or its seed-after-boot ordering (#675's own scoped fix).
