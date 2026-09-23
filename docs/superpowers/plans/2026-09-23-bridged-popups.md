# Bridged popups: let a bridged session open a popup, and let the mirror clear it (#738)

Spec: `docs/superpowers/specs/2026-09-23-bridged-popups-design.md`. Dispatcher approved the full scope:
carry the tmux patch, regression-test through the real bridge, correct the docs,
and say plainly in the PR what the original `halo-toddl` report actually was.

Worktree: `/home/noams/Data/git/.worktrees/noamsto/tmux-og/feat-738-ctrl-r-history-float-never-mirrors-in-ha`

## Contract and consumer map (EVIDENCE_REVIEW.md)

The contract being changed is "a remote float reaches the mirror". The tmux
patch does not change its *shape* — it changes which remote events can produce
one. Before the patch no remote `display-popup` in a bridged session could
produce anything; after it, a **modal** float can reach the mirror in the
common case for the first time. Every stage, with its disposition:

| Stage | Symbol / file | Disposition |
| --- | --- | --- |
| Producer | `cmd_display_popup_exec` → `layout_floating_pane` → `spawn_pane(SPAWN_FLOATING\|SPAWN_MODAL\|SPAWN_FLOATOVERZOOM)`, remote tmux | **changed** — the `CLIENT_CONTROL` bail is removed, so the producer fires where it previously returned |
| Producer, zoom bracket | `window_push_zoom(w, 0, 1)` / `window_pop_zoom(w)` around the create | compatible — `reconcileLayoutFrom` already falls back to a read on a zoom transition, and push/pop brackets are covered by the existing `reconcilezoom_test.go`; not re-tested here (the popup path adds no new zoom shape) |
| Notification | remote `%layout-change` (v2 JSON, `z` leaf) | compatible — a modal float is an ordinary `z` leaf; `layoutnotice.go`'s `layoutShaped` already accepts v2 |
| Parse | `controlmode.ParseLayout` → `Layout.Floats`, `Layout.Raw` (floats pruned) | compatible — measured in §2.5(b): remote `modal=1` float parsed and mirrored |
| Diff | `noFloatWork` / `mirrorableFloats` / `planFloatOps` | compatible — keyed on remote pane id and cell, blind to modality |
| Apply | `reconcileFloats` → `floatCreateArgv` (`new-pane -B heavy -A`), `spawnRenderer`, `waitHellos`, `seedRenderer`, `pumpInput` | **changed** — task 3: every `pumpInput` also sends a remote-guarded `display-popup -C -t %N` on a lone Escape/C-c |
| Geometry stamp | `stampFloatGeom` → `@float_geom` → `scripts/tmux-float-refit.sh` | compatible — the mirrored float is created by `new-pane`, so it carries `@float_geom` exactly as today (`tests/float-refit.bats` still owns it) |
| Local tiled set | `parseLocalPaneList` (floats excluded), `sortedFloatIDs` (rebuild) | compatible — the local mirror float is **non-modal** (`modal=0` measured), so the modal-pane chrome rule in `docs/agents/floats.md` does not engage locally |
| Focus | `focusLocalPane`, `assertMirrorZoom` (#517 skips zoom-on when remote active is a float) | compatible — remote-active-is-a-float is the existing path; task 6's tests keep it honest |
| Teardown | `removeFloat`, `retireOrRestoreFloats`, `w.floatsDropped` | compatible — measured: an `-E` popup closing removes the float on both sides |
| Teardown, no `-E` | `remain-on-exit=1` → dead remote float → `w->modal` never cleared (`window.c:1156/1175`) | **broken, fixed here** — task 3 |
| Local consumers of `pane_floating_flag` | picker (`render_list.go`, `sorted_tiled_dims` in tests), `windowlabels` | compatible — the mirrored float is indistinguishable from today's `new-pane` float |

Absent stages: no persistence, no batch/retry path, no validator — a float is
recomputed from the remote's layout on every pass.

## Tasks

Execute in order. Tasks 1-2 must land before 6-8 can go green.

### 1. Carry the tmux patch

**Files:** new `patches/tmux-display-popup-control-client.patch`, `flake.nix`.

The patch deletes exactly these two lines from `cmd_display_popup_exec` in
`cmd-display-menu.c` (lines 416-417 at pinned rev `3a6c2e78`):

```c
	if (tc->flags & CLIENT_CONTROL)
		return (CMD_RETURN_NORMAL);
```

The patch file opens with a prose rationale (why the bail is obsolete since
upstream `34cd5da4` made popups modal floating panes; that
`status_line_size`/`status_at_line` short-circuit on `CLIENT_CONTROL`, that
`tty_window_offset` reads cached zeroes, that `cmdq_get_event(item)->m.valid`
is false for a command client, and that `tc` is otherwise only `sc.tc`) and
ends with "Submitted upstream; drop this file once it lands." — matching the
posture of the patch `5447f1e` carried.

In `flake.nix`, inside `mkTmux`'s `overrideAttrs`, directly after
`src = inputs.tmux-upstream;`:

```nix
        # display-popup still carries the overlay-era CLIENT_CONTROL bail, so a
        # popup a remote shell opens inside a bridged session resolves to the
        # daemon's control client and silently opens nothing (#738). Drop once
        # upstream takes it.
        patches = (old.patches or []) ++ [./patches/tmux-display-popup-control-client.patch];
```

`git add patches/` immediately — `nix build` only sees tracked files in a git
worktree, and an untracked patch fails evaluation with "path does not exist".

**Verify:** `nix build .#default` succeeds; `./result/bin/tmux -V` prints
`tmux next-3.9`.

### 2. Prove the patch red→green at the tmux layer

**Files:** none (evidence only).

Run the scratch-server repro against the tmux the tree shipped *before* task 1
and against `./result/bin/tmux` after it. Record both outputs for the PR's
`## Evidence` section. The repro is a throwaway scratch-server script; it is not committed (task 6
commits the bridge-level equivalent, which is the production path).

Expected: unpatched `floats=0 … FAIL`, patched `floats=1 … PASS`, and
`server_sessions=1` in both — the #346 property still holds.

### 3. Let the mirror clear a wedged remote modal float

**File:** `picker/remotebridge/daemon/daemon.go` only (`pumpInput`). No call site
changes, no signature change.

Without this the patch is net-negative (spec §2.6): a `display-popup` without
`-E` sets `remain-on-exit=1`, the pane dies but is never removed, `w->modal`
stays set and every later popup in that remote window is a silent no-op — and
the mirror cannot dismiss it, because `PANE_CLOSEONCANCEL`
(`server-client.c:1655-1661`) is tmux's *client* key rule, while `pumpInput`
writes pane input (`send-keys -H -t %N`), which a dead pane ignores.

`display-popup -C` is the server-side clear, and its branch runs **ahead of**
the `CLIENT_CONTROL` bail, so it works for a control client patched or not.

Design: the clear targets the **pane**, `display-popup -C -t %N`. `-t` is
`CMD_FIND_PANE`, so the window is resolved from the pane's *current* window at
the moment the command runs — no window id is captured, so nothing can be stale
after a remote `break-pane`/`join-pane`, and there is no session name to quote
(pane ids are `%N`, already interpolated bare by `SendKeysArgs`). The guard is
evaluated on the remote against the same pane:

```
if -F -t %N '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %N'
```

`pane_modal_flag` is `wp == wp->window->modal` (`format.c`), so the guard is
true only when **this** pane is its window's modal pane and is dead — exactly
the wedge. Measured on a scratch server with a control client attached: the
command sent for a tiled pane `%0` leaves a dead modal `%1` untouched; sent for
`%1` it removes it; a later popup then opens.

Because the guard is remote, the hook needs no knowledge of whether a pane is a
float, so it goes on **every** pump — so a float pump started from any path — including
`rebindRenderer` (`daemon.go:2096`), which adopts a reconnect hello for a float
as readily as a tiled pane — carries it; there is no classification site to get
wrong. Cost: one extra fire-and-forget command (plus its fanout barrier, `daemon.go:517-534`) per *lone* Escape or C-c
keystroke in any mirrored pane; a live pane (every tiled pane, every non-modal
float such as fzf's, a live modal popup whose program owns its cancel key) fails
the guard and nothing happens.

Cancel keys are tmux's own: `\033` and `C-c` (`server-client.c:1658`). `q` is
not a tmux cancel key and the spec's §4.7 is corrected accordingly. Only a
payload that is exactly one of those bytes counts, so an escape sequence's
leading `0x1b` (arrows, function keys) never pays for it.

In `pumpInput`, after the `SendKeysArgs` loop:

```go
		if isCancelKey(payload) {
			send(modalClearCmd(remotePane))
		}
```

and beside it:

```go
// isCancelKey reports a frame that is one lone cancel key — the two keys
// tmux's own PANE_CLOSEONCANCEL rule answers (server-client.c). A leading
// 0x1b inside an escape sequence is not one.
func isCancelKey(b []byte) bool {
	return len(b) == 1 && (b[0] == 0x1b || b[0] == 0x03)
}

// modalClearCmd dismisses pane when it is its window's modal float and has
// died. A popup opened without -E stays behind dead with remain-on-exit set,
// and w->modal is only cleared when the pane is removed, so until then every
// later display-popup in that window is a silent no-op. tmux clears it on a
// client's cancel key, but this daemon writes pane input, which a dead pane
// ignores; display-popup -C is the server-side clear and runs ahead of the
// command's CLIENT_CONTROL bail. The guard is evaluated remotely, so a live
// pane keeps its cancel key.
func modalClearCmd(pane string) string {
	return fmt.Sprintf("if -F -t %s '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %s'", pane, pane)
}
```

The paste handler rewrites `payload` before the `SendKeysArgs` loop; test the
*rewritten* payload, which is what was actually sent.

**Verify:** `go build ./...` and `go test -race ./remotebridge/...` from `picker/`.

### 4. Unit-test the cancel hook

**File:** `picker/remotebridge/daemon/pumpinput_test.go` (existing — append).

Negative cases need a terminator, not a timeout: after each frame write a
sentinel `z` frame and assert the next `send` is its `send-keys`.

- `TestModalClearCmd` — `modalClearCmd("%7")` is exactly
  `if -F -t %7 '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %7'`.
- `TestPumpInputSendsModalClearOnLoneCancel` — table-driven over a `net.Pipe`,
  collecting `send` calls: lone `0x1b` → `send-keys -H -t %7 1b` then the clear;
  lone `0x03` → likewise; `0x1b 0x5b 0x41` (Up) → only the `send-keys`; `a` →
  only the `send-keys`. Same harness shape as the file's existing
  `TestPumpInputDoesNotCallDiedOnACleanFrameRead`.

**Verify:** `go test ./remotebridge/daemon/ -run 'ModalClear|PumpInput'` from
`picker/`.

### 5. Re-point the readiness test at the new behaviour

**File:** `tests/tmux-next38-readiness.bats`, the test
`"display-popup is refused for an attached control client"` (line ~198).

It pins the old behaviour (`[ ! -f "$marker" ]`) and fails after task 1.
Rewrite as `"display-popup for an attached control client opens a modal float"`,
keeping the coproc control-client harness and the sentinel round-trip. `-E` with a command that exits at once closes the
pane before any poll can see it, so the popup command writes the marker **and
then stays alive**:

```bash
printf 'display-popup -E "printf popup-ok > %q; sleep 30"\n' "$marker" >&"${CTL[1]}"
```

Poll (bounded, like the existing sentinel loop) for the marker file, then read
`t list-panes -t s -F '#{pane_modal_flag}'` while the popup is still open and
assert a `1`; then assert `t list-sessions` succeeds (the #346 property —
the server survives). Tear down with `display-popup -C` over the control stream
before `detach-client`. Rewrite the comment: `af3e4d2` made a control client's
popup a silent no-op while popups were client overlays; `34cd5da4` made them
modal floating panes, so the bail is dead weight and #738 drops it via
`patches/tmux-display-popup-control-client.patch`.

**Verify:** `nix build .#checks.x86_64-linux.tmux-next38-readiness-tests`.

### 6. Bridge-level regressions

Notes for every case: commands issued *inside* the mirrored pane call bare
`tmux` — the pane's `$TMUX` already names the m2src server, and the check's
`nativeBuildInputs` puts `mkTmux` first on PATH. Once a popup is open it is the
remote's active pane, so every later `$SRC send-keys` must target the base
pane by id (`$base`, read with `display-message -p -t rem '#{pane_id}'` before
the popup), never `-t rem`, or the keys land in the popup.


**File:** `tests/remote-m2-integration.bats`, appended after the existing
`"a remote float is mirrored as a local float"` case (around line 3787) so the
float cases stay together. The check derivation already builds with
`(mkTmux pkgs)`, so both scratch servers run the patched tmux.

Three cases, each using the file's existing `bridge_up` helper and its
`$SRC`/`$DST` idiom, each killing `$daemon_pid` before asserting:

6a. `"a remote display-popup opens a float the mirror gains (#738)"` — one
remote pane; issue the popup **from inside the mirrored pane**, which is the
production entry point and the thing that was broken:

```bash
$SRC send-keys -t "$base" "tmux display-popup -E 'sleep 90'" Enter
```

Poll until `$SRC list-panes -t rem -f '#{pane_floating_flag}' -F
'#{pane_width}x#{pane_height}'` and the matching `$DST` read agree and are
non-empty, then assert both non-empty and equal, and that the remote float
reports `#{pane_modal_flag}` = 1 while the mirror's reports 0 (the mirror
renders a modal remote float as an ordinary local float — the documented
design, and the thing a future regression would break silently).

This is the red case: against unpatched tmux the remote float never exists, so
`src_float` is empty and the test fails on its first assertion, not on setup.

6b. `"a dead remote popup is dismissable from the mirror (#738)"` — the wedge.
Issue `display-popup 'echo popup-done'` (no `-E`) from inside the mirrored
pane, wait for the mirror to gain a float and the remote pane to read
`#{pane_dead}` = 1, then send a lone Escape **into the mirror float**
(`$DST send-keys -t "$mirror_float" Escape`) and assert that both the remote
and the mirror lose their float. Then issue a second
`display-popup -E 'sleep 90'` and assert a float appears again — that is the
`w->modal` wedge, and it is the half a test of the dismissal alone would miss.

6c. `"a float a remote shell opens from inside its own pane mirrors (#738)"` —
the `fzf --tmux` shape, which nothing covers today: the existing case creates
the float with an **external** `new-pane -d`. Send, from inside the mirrored
pane, the argv fzf 0.74 actually uses (captured with a PATH shim on the real
host):

```bash
$SRC send-keys -t rem "tmux -L m2src if -F -t $active '#{window_zoomed_flag}' 'resize-pane -Z -t $active' \; new-pane -P -F '#{pane_id}' -t $active -x 94 -y 16 -X 13 -Y 7 sleep 90" Enter
```

against a **three-pane** remote window (`active=$($SRC display-message -p -t rem '#{pane_id}')` read after the splits; bare `tmux` inside the pane), since a float arriving in a window with
a tiled split is where the pane-diff mapping could go wrong and the one-pane
case cannot see it. Assert the tiled dims are unchanged (`sorted_tiled_dims`)
and the float dims match.

**Verify:** `nix build .#checks.x86_64-linux.remote-m2-integration-tests`, and
the red run in task 8.

Every bounded wait in 6a/6b is followed by an assertion of what it waited for,
so a red lands on a named assertion, not on a later step.

### 7. Docs

**Files:** `docs/agents/bridge-daemon.md` (7a-7c) and `docs/agents/floats.md`
(7d).

7a. In the `## Mirror invariants` bullet for floats, replace the two sentences
beginning "A remote's `display-popup` opens a modal floating pane…" and "The
bridge itself can never originate one…" with the corrected text: the bail is
`cmd_display_popup_exec`'s, `tc` is the *target* client resolved by
`cmd_find_best_client` from the clients attached to the session, so a popup a
**remote shell** asks for was refused exactly as one the bridge sends was —
exit 0, no float, nothing logged; #738 drops the bail via
`patches/tmux-display-popup-control-client.patch`; fzf ≥ 0.74 dodged it by
probing `list-commands new-pane`.

7b. Add, in the same bullet, the `display-popup -C` rule: a `display-popup`
without `-E` leaves a dead modal float that `window_lost_pane` never reaps, so
`w->modal` stays set and every later popup in that window is a silent no-op.
The mirror's cancel key cannot clear it, because the daemon writes pane input
while `PANE_CLOSEONCANCEL` is a client-key rule — so a lone Escape or C-c
forwarded to any mirrored pane also sends
`if -F -t %N '#{&&:#{pane_dead},#{pane_modal_flag}}' 'display-popup -C -t %N'`.
Note that `-C` is honoured for a control client with or without the patch,
because its branch runs ahead of the bail, and that the guard deliberately
spares a live pane, whose cancel key belongs to the program in it. Also qualify
the existing sentence "A float this daemon did not create is never killed" in
the same bullet: it is about **local** floats; this clear is the one
daemon-initiated kill of a **remote** float, and only of a dead modal one the
user's own cancel key would have closed on a normal client.

7c. Add a row to the `## What the Remote Host Needs on PATH` table:

| Feature | Remote needs |
|---------|--------------|
| Popups on the remote (`display-popup` from a remote shell, older `fzf`, `fzf-tmux -p`, atuin, any popup bind of your own) | a remote rebuilt from this revision, for `patches/tmux-display-popup-control-client.patch` (#738). No capability probe: an unrebuilt remote opens nothing at all, silently — the same degradation shape as remote session resources. `new-pane`-based floats (fzf ≥ 0.74) are unaffected and work on any remote whose tmux has `new-pane`. |

7d. `docs/agents/floats.md`: in the one-modal-per-window sentence (line ~14),
add that a **dead** modal pane (a no-`-E` popup whose command exited) still
counts, so the window's popups stay wedged until the pane is dismissed or
`display-popup -C` runs. In the `remain-on-exit` sentence (lines ~55-57),
replace "or its pane lingers dead inside a mirror window" with the new
behaviour: without `-E` the pane lingers dead; on a normal client Escape/C-c
closes it, and inside a mirror window the daemon's `display-popup -C` clear
does the same (#738).

### 8. Evidence run, red and green — one red per changed contract

**Files:** none. Scratch copies only (`git worktree add` under the scratchpad,
or `git stash`-free `git archive` into a temp dir); never touch this tree.

Two contracts change, so two reds, each isolating one change:

- **Red A — the tmux patch.** `HEAD` of `main` plus this branch's
  `tests/remote-m2-integration.bats` (the file, with the three task-6 cases
  appended) and nothing else. Run
  `nix build .#checks.x86_64-linux.remote-m2-integration-tests`.
  Expected: 6a fails on its `[ -n "$src_float" ]` assertion (no remote float
  exists at all); 6b fails at its first wait (no float). 6c passes — it is
  coverage for a path that already works, and is stated as such.
- **Red B — the daemon mitigation.** This branch with task 3 (and task 4)
  reverted — i.e. tmux patch present, `pumpInput` unchanged — plus the task-6
  tests. Expected: 6a passes, **6b fails on the dismissal assertion** (remote
  float still present, `dead=1`, after Escape into the mirror). That is what
  proves the mitigation is load-bearing rather than asserted.
- **Green.** The full branch: all three pass.

A setup or build failure is not red evidence; if a red run fails anywhere other
than the named assertion, fix the harness and rerun. Record, for each run: the
revision/patch set, the command, and the observed failing assertion, in
`REVIEW_NOTES.md` at the worktree root and in the PR's `## Evidence`.

### 8b. Commit the plan and spec

Copy this file to `docs/superpowers/plans/2026-09-23-bridged-popups.md` and
`spec.md` to `docs/superpowers/specs/2026-09-23-bridged-popups-design.md`
(CLAUDE.md "Plans and Specs").

### 9. Local gate

`nix build .#default`, `nix flake check`, `nix build .#lint` — all three; none
subsumes another. `nix flake check` runs no formatter, so `.#lint` is not
optional.

### 10. Follow-up issue

File one issue: any remote pane that dies with `remain-on-exit` set is
undismissable from the mirror, because the daemon delivers input as pane input
rather than through a client key path. Task 3 fixes only the modal-float case
via `display-popup -C`. Link the PR. Also mention in the PR body, not as an
issue, that nothing compares the control stream's `s.seen` against `s.sent`
(the task doc's second lead) — it stayed out of scope because it is not this
bug, but it is still the reason a desync would be undiagnosable.

## PR body requirements

- `Closes #738`.
- `## Evidence` — task 8's red/green, plus the tmux-layer red/green from task 2.
- `## Review notes` — the finding ledger.
- A plainly-worded section on what the original report was: `halo-toddl`'s panes
  are one `bash` home window plus nine `claude` panes, and `FZF_DEFAULT_OPTS`
  carries `--tmux` only as a **fish** universal variable, so `ctrl+r` opens no
  float in any of them — while `halo-nix-config`'s single pane is fish. That
  accounts for the every-window form of the report. It does **not** account for
  the original new-window form: halo's remote `default-shell` is fish, and an
  end-to-end run over real ssh (a scratch remote session on halo, the real
  daemon, `ctrl+r` pressed into the mirror pane) mirrors the float correctly at
  `106x21`. So #738's literal symptom is unreproduced; what this PR fixes is the
  popup hole found while investigating it, which produces the same symptom for
  every popup-based tool. Say that, and leave the issue's disposition to the
  user.
- `## Follow-ups` — the task 10 issue, appended after `gh pr create`.
