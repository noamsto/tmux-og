# Plan: dismiss a dead remote pane from its mirror (#748)

Spec: `docs/superpowers/specs/2026-09-27-748-bridge-dead-pane-dismiss-design.md`.

The mechanism: `pumpInput` sends a remote-guarded
`if -F -t %N '<dead-key predicate>' 'kill-pane -t %N'` **before** the `send-keys`
of every frame that counts as a key (not a mouse report, a focus report, or a
bracketed paste). The predicate is
`#{&&:#{pane_dead},#{||:#{==:#{remain-on-exit},key},#{==:#{remain-on-exit},failed-key}}}`.
#746's modal clear stays as it is, after `send-keys`.

## Invariant and regression

Violated invariant: a key a local client would use to dismiss a dead pane
(tmux `server_client_handle_dead_key`: `PANE_EXITED`, `remain-on-exit`
`key`/`failed-key`, a non-mouse, non-paste key) must dismiss the same remote pane
from its mirror, and must never kill a live pane.

The production entry point that owns it is `pumpInput`. Red/green proof:

- the live-tmux Go test (Step 5) runs against the tree **without** the
  `pumpInput` change and fails, because the dead `key` panes survive;
- the bats test (Step 7) fails the same way against the old tree.

Both are recorded in `REVIEW_NOTES.md`. For the Go test, the old `pumpInput`
body is restored in a scratch copy under `$TMPDIR`, never in the worktree. The
existing sequence test (Step 3) is red simply because the new guard line is
missing.

## Consumer map (the `pumpInput` send contract)

| Edge | Symbol / file | Disposition |
|---|---|---|
| Producer | renderer `FrameInput` frames (`picker/remotebridge/renderer`) | unchanged |
| Transform | `pasteHandler.handle` (`paste.go:149`) | unchanged; the classifier reads its output |
| Transport | `send` → `connHolder.send` → `stream.stampAll` (barrier per command, `daemon.go:~520`) | compatible: `if -F` is already sent this way by the #746 clear; its fan-out is swallowed by the barrier (#715/#723) |
| Callers of `pumpInput` | `daemon.go:1543,1555,2111`, `reconcile.go:649,929` | unchanged signature |
| Remote | `if -F`/`kill-pane` on the remote tmux | an older remote lacking `key` evaluates the guard false (spec §4a) |
| Mirror reaction | a pane the remote removed → `%layout-change` → reconcile drops its renderer (the same path as ctl `kill-pane`) | compatible, covered by the Step 7 assertion on the DST pane count |
| Error path | `send-keys` into a removed `%N` → `%error`, claimed and discarded | compatible, same as the existing #746 clear |

## File list

- `picker/remotebridge/daemon/daemon.go` — add `isDismissKey` and
  `deadKeyCmd`; call the guard in `pumpInput` before `send-keys`.
- `picker/remotebridge/daemon/pumpinput_test.go` — unit tests for the two
  helpers; update the send-order table test.
- `picker/remotebridge/daemon/deadkey_test.go` (new) — live-tmux test driving
  `pumpInput` into a real control client.
- `tests/remote-m2-integration.bats` — end-to-end bridge tests for a dead
  tiled pane, a dead float, and a live pane plus a dead `on` pane.
- `docs/agents/bridge-daemon.md` — the new rule and its divergences.
- `docs/superpowers/specs/2026-09-27-748-bridge-dead-pane-dismiss-design.md`,
  `docs/superpowers/plans/2026-09-27-748-bridge-dead-pane-dismiss.md` —
  committed with the change (CLAUDE.md).

## Steps

- [ ] **Step 1: failing unit tests for the helpers** — `pumpinput_test.go`.
  Add `TestDeadKeyCmd`. It asserts that `deadKeyCmd("%7")` equals
  `` `if -F -t %7 '#{&&:#{pane_dead},#{||:#{==:#{remain-on-exit},key},#{==:#{remain-on-exit},failed-key}}}' 'kill-pane -t %7'` ``.

  Add `TestIsDismissKey`, a table:

  | Input | Want |
  |---|---|
  | `"a"`, `"\r"`, `{0x1b}`, `{0x03}`, `"\x1b[A"`, `"\x1bx"` (M-x), `"é"`, `"hello"` | true |
  | `"\x1b[<0;5;5M"`, `"\x1b[<0;5;5m"`, `"\x1b[M !!"`, `"\x1b[I"`, `"\x1b[O"`, `"\x1b[200~hi\x1b[201~"`, `"x\x1b[200~y"` | false |

  Proof: `cd picker && go test ./remotebridge/daemon -run 'TestDeadKeyCmd|TestIsDismissKey'`
  fails to compile (undefined symbols).

- [ ] **Step 2: implement the helpers** — `daemon.go`, next to
  `isCancelKey`/`modalClearCmd`.

  `isDismissKey(b []byte) bool` returns false when:
  - `bytes.HasPrefix(b, "\x1b[<")` or `bytes.HasPrefix(b, "\x1b[M")`;
  - `b` equals `"\x1b[I"` or `"\x1b[O"`;
  - `bytes.Contains(b, "\x1b[200~")`.

  It returns true otherwise. Callers never pass it an empty slice.

  `deadKeyCmd(pane string) string` returns the Step 1 string via `fmt.Sprintf`.
  Each helper gets a doc comment that states the tmux rule it mirrors: the
  mouse/paste exclusions and the focus-report reason for the first, and
  `server_client_handle_dead_key` with `key`/`failed-key` only for the second.
  Keep them as short as the neighbours' comments.

  Proof: the Step 1 command passes.

- [ ] **Step 3: failing send-order test** — `pumpinput_test.go`,
  `TestPumpInputSendsModalClearOnLoneCancel`. Rename it
  `TestPumpInputSendOrder`. Every key frame now sends `deadKeyCmd("%7")` first,
  and so does the sentinel `z`, whose guard lands in `got` just before the
  sentinel's own `send-keys`. The loop still stops on
  `send-keys -H -t %7 7a`, so every `want` ends with `deadKeyCmd("%7")`.
  New `want`s:
  - lone escape: `[guard, "send-keys -H -t %7 1b", modalClear, guard]`
  - lone ctrl-c: `[guard, "send-keys -H -t %7 03", modalClear, guard]`
  - up arrow: `[guard, "send-keys -H -t %7 1b 5b 41", guard]`
  - `a`: `[guard, "send-keys -H -t %7 61", guard]`
  - new case, SGR mouse click `"\x1b[<0;5;5M"`: `[send-keys hex…, guard]`,
    so the mouse frame gets no guard of its own
  - new case, focus-in `"\x1b[I"`: `[send-keys 1b 5b 49, guard]`
  - new case, bracketed paste `"\x1b[200~hi\x1b[201~"`: `[send-keys hex…, guard]`

  Build the expected strings from `deadKeyCmd("%7")`/`modalClearCmd("%7")`, not
  from literals, so Step 1 alone owns the literal. Keep the channel buffer ≥ 8.

  Proof: `go test ./remotebridge/daemon -run TestPumpInputSendOrder` fails,
  because no guard is sent yet.

- [ ] **Step 4: wire the guard into `pumpInput`** — `daemon.go:2509-2518`.
  After `paste.handle`, and before the `SendKeysArgs` loop, add
  `if len(payload) > 0 && isDismissKey(payload) { send(deadKeyCmd(remotePane)) }`.
  Leave the `isCancelKey` clear after the loop unchanged. Add a one-line comment
  saying the guard goes first because tmux checks deadness before it delivers
  the key.

  Proof: `go test ./remotebridge/daemon -run 'TestPumpInput|TestDeadKeyCmd|TestIsDismissKey|TestModalClearCmd'`
  passes.

- [ ] **Step 5: live-tmux regression through `pumpInput`** — new
  `deadkey_test.go`, `TestPumpInputDismissesDeadKeyPaneLiveTmux`.

  Skip idiom: when `exec.LookPath("tmux")` fails, `t.Fatal` under
  `OG_REQUIRE_TMUX`, else `t.Skip`, same as `sessionres_test.go:240-247`. Start
  the server with `tmux := startIsolatedTmux(t, "CLAUDE_STATUS_DIR="+t.TempDir())`.
  It must run the **pinned** tmux, which `flake.nix:130` puts on PATH for
  `pickerChecked`. nixpkgs tmux lacks `new-pane`/`key`.

  Setup, all in session `w`:
  1. `set-option -w -t w remain-on-exit on` before creating any child pane.
  2. **Live:** `split-window -d -P -F '#{pane_id}' -t w cat`. `-d` keeps the
     base pane active.
  3. **Dead tiled `key`:** `split-window -d -k -P -F '#{pane_id}' -t w true`.
  4. **Dead float `key`:** `new-pane -d -k -P -F '#{pane_id}' -t w -x 20 -y 5 -X 2 -Y 2 true`.
  5. **Dead `on`:** `split-window -d -P -F '#{pane_id}' -t w true`. It inherits
     the window's `on`.
  6. Poll `display-message -p -t <id> '#{pane_dead}'` until it reads `1` for
     all three dead panes. Timeout 5s, else `t.Fatal`.

  Start a control client `tmux -C attach-session -t w`, as in
  `sessionres_test.go:254-275`. Drain its stdout in a goroutine. `send` writes
  `line+"\n"` to its stdin under a mutex.

  For each of the four panes, create a `net.Pipe` and start
  `go pumpInput(conn, id, send, nil, nil, nil)`. Write one `wire.FrameInput`
  frame `"x"` to each peer, with a 5s deadline.

  Assertions, each polled for up to 5s:
  - `list-panes -s -t w -F '#{pane_id}'` loses both dead `key` panes;
  - the live pane's `capture-pane -p -t <live>` contains `x`, echoed by the tty;
  - after the other conditions settle, `list-panes` still has the live pane and
    the dead `on` pane.

  The four `pumpInput` goroutines have no ordering between them, so the
  survivor assertion needs its own barrier. After the first `"x"` frame, write a
  **second** frame to the live pipe and to the `on` pipe. On a `net.Pipe`, that
  write blocks until the pump returns to `ReadFrame`, and the pump only does
  that after `send` has written the first frame's guard. Then assert the
  survivors, after the kills have been observed.

  Proof: `go test ./remotebridge/daemon -run TestPumpInputDismissesDeadKeyPaneLiveTmux -v`
  passes. Red proof: in a scratch copy of `picker/` under `$TMPDIR` with the
  Step 4 line removed, the same command fails on the `key` panes still present.
  Record both commands and the failing assertion in `REVIEW_NOTES.md`.

- [ ] **Step 6: full daemon package under `-race`** — no file change.

  Proof: `cd picker && go test -race ./remotebridge/...` passes. `go vet ./remotebridge/...`
  is clean.

- [ ] **Step 7: bridge integration tests** — `tests/remote-m2-integration.bats`,
  after the #738 popup tests. Each test follows the shape of the #738 dismiss
  test: `bridge_up`, locate the mirror pane, re-press in a loop of 60 × 0.15s,
  `kill "$daemon_pid"`, then assert.

  To find the mirror of remote pane `$r`:
  `$DST list-panes -s -t host-sess -F '#{pane_id} #{@bridge_pane}' | awk -v r="$r" '$2==r {print $1}'`.
  Poll it, because the renderer is stamped asynchronously.

  1. `@test "a dead remote key-pane is dismissed by a key in its mirror (#748)"`:
     - `$SRC new-session -d -s rem -x 100 -y 30`;
       `dead="$($SRC split-window -d -k -P -F '#{pane_id}' -t rem true)"`;
       wait for `pane_dead` = 1; `$DST new-session …`; `bridge_up 2 deadkey1`.
     - Loop `$DST send-keys -t "$mirror" q` until `$SRC list-panes -t rem`
       lacks `$dead` and the DST window has 1 pane.
     - Assert both, and that the base pane survives.
  2. `@test "a dead remote key-float is dismissed by a key in its mirror (#748)"`:
     - `new-session`, then `bridge_up 1 deadkey2` **first**. `bridge_up`
       counts `@bridge_pane` across floats too, so a float that already exists
       would make it wait for 2. The existing float tests create their float
       after `bridge_up` for the same reason.
     - Then `dead="$($SRC new-pane -d -k -P -F '#{pane_id}' -t rem -x 30 -y 8 -X 5 -Y 5 true)"`.
     - Poll until the mirror has a float (`-f '#{pane_floating_flag}'`) and
       `$SRC` reads `pane_dead` = 1 for `$dead`.
     - Loop `send-keys q` into it until neither side has a float.
     - Assert that, and that the base pane survives.
  3. `@test "a key leaves a live pane and a dead remain-on-exit-on pane alone (#748)"`:
     - `$SRC set -w -t rem remain-on-exit on`;
       `on="$($SRC split-window -d -P -F '#{pane_id}' -t rem true)"`; wait dead;
       `bridge_up 2 deadkey3`.
     - Re-press `x` into both mirrors for 20 rounds × 0.15s.
     - Assert the live base pane's `capture-pane` shows `x`, and
       `$SRC list-panes` still lists `$on`.
     - After `bridge_up`, also create a dead `on` float:
       `onf="$($SRC new-pane -d -P -F '#{pane_id}' -t rem -x 30 -y 8 -X 5 -Y 5 true)"`.
       It inherits the window's `on`. Poll for its mirror float and
       `pane_dead` = 1, re-press `x` into it for the same 20 rounds, and assert
       it survives.
     - Then run `"$CTL" --sock "$sock" kill-pane "$on"` and
       `"$CTL" --sock "$sock" kill-pane "$onf"` (what `prefix + x` runs). Poll
       until both sides have only the base pane. That pins the `on` gesture for
       a tiled pane and a float.

  Proof, in the devshell: `bats tests/remote-m2-integration.bats -f '#748'` passes 3/3.
  Red proof: with the Step 4 line removed (a scratch copy of `picker/` under
  `$TMPDIR`, pointing `DAEMON` at a binary built from it), tests 1 and 2 fail.
  Record this in `REVIEW_NOTES.md`.

- [ ] **Step 8: docs** — `docs/agents/bridge-daemon.md`. Add one paragraph right
  after the #738 modal-clear sentences in the float bullet, or as its own bullet
  directly after it. It covers:
  - the rule;
  - its tmux source, `server_client_handle_dead_key`, `server-client.c:1278`,
    fast path at `:1653`;
  - `key`/`failed-key` only, and `on` via `prefix + x`/ctl `kill-pane`;
  - guard-before ordering and the `exit`+Enter race it closes;
  - the mouse/focus/bracketed-paste exclusions;
  - the three recorded divergences (unbracketed and split paste, prefix and
    root-bound keys, a multi-key frame dropped whole);
  - that this is a second daemon-initiated remote kill, like #746's, and only of
    a pane a normal client's own key would have closed.

  Keep the doc's sentence-dense style.

  Proof: `rg -n '#748' docs/agents/bridge-daemon.md` hits.

- [ ] **Step 9: full gate.**
  - `nix build .#default`
  - `nix build .#lint` (typos, shellcheck/shfmt on the bats file)
  - `nix flake check`, which runs the Go tests with the pinned tmux and the bats
    integration

  Proof: all three exit 0.

## Acceptance

| Task acceptance item | Settled by |
|---|---|
| Dead tiled `remain-on-exit on` pane is dismissable with the local key(s) | `on`: the Step 7 test 3 ctl `kill-pane` leg, which is `prefix + x`'s action. `key`: Step 7 test 1 and the Step 5 tiled pane. Spec §2 shows no plain key dismisses `on` locally. |
| Same for a dead non-modal float | `key`: Step 7 test 2 and the Step 5 float pane. `on`: the Step 7 test 3 float leg. |
| Live pane gets the key as ordinary input | Step 5 capture-pane assertion and Step 7 test 3 |
| Go tests for dead-tiled, dead-float and live | Steps 1, 3, 5 |
| Scratch-server repro in the PR body | the manual `TMUX_TMPDIR=/tmp/og-$$ … tmux -L probe` transcript from spec §4a, re-run at the end, plus the bats run |
| `docs/agents/bridge-daemon.md` updated | Step 8 |
