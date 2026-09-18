# Plan — #679: `prefix + p/g/y` must focus the tool float it already has, not stack another

Tier: standard. Engine: pi (lead executes in-pane; no execute subagents). Base:
`433f5f2`. Revision 1 — a first draft's focus branch put a pane loop inside a
`run-shell` argument, which `tests/conf-shell-quoting.bats` rejects; see "Shape,
second attempt" below.

## Root cause (verified, not inferred)

`new-pane -A` is a **z-order** flag — tmux's own help: "creates a floating pane
which remains visible above a zoomed pane". There is no attach-if-exists. Every
`prefix + p`/`g`/`y` press therefore runs `new-pane` again and adds another
float at the same geometry. Reproduced on the pinned tmux:

```
tmux -L s new-pane -t w -x 90% -y 85% -X 5% -Y 8% -B heavy -A 'sleep 600'   # ×3
→ three floating panes at 106x23
```

Nothing in the repo dedupes. `mkFloat` builds `flags`/`flagsNoA` around `-A`,
`floatNewPaneGuard` version-guards on its presence, and
`remoteFloatShort`/`remoteFloatFull` hardcode it — all true, none of it reuse.

## Invariant and mechanism

**Invariant:** a window holds **at most one float per tool**, and `prefix + p|
g|y` reuses it. **Lookup key:** `@pane_label` — already stamped with the tool
name by every tool float bind (`... \; set -p @pane_label prdash`). No durable
new state.

**The predicate** is a tmux format pane loop, which turns "does this window
already hold the tool's float" into a truthy format with no fork. It is built
**once per file** — `bridgedFloatTool` in `keys.go` and in `reference.nix`, the
ctl builder in `ctl.go` — and the same string is interpolated into both the
condition and the register write, so those two cannot drift:

```
LOOP = #{P:#{?#{&&:#{==:#{@pane_label},TOOL},#{pane_floating_flag}},#{pane_id},}}
```

Verified on next-3.9, 3.7c and 3.6a: `#{P:...}` iterates the **target** window's
panes and expands its body per pane; it is empty when nothing matches, so
`if-shell -F "$LOOP"` is falsey with no `#{?:}` wrapper needed (`%2` is truthy,
`""` is falsey). `#{==:}` on an unset option is fine on all three.

## Shape, second attempt (the first one was blocked)

The first draft's focus branch was `run-shell "tmux select-pane -t '<LOOP>'"`.
That is a format nested inside a shell string, and
**`tests/conf-shell-quoting.bats` fails the build on it**: in that conf every
`#{...}` inside a shell-string argument must be `#{q:NAME}` or `#{qs:NAME}` of a
plain, unnested body, because run-shell/if-shell hand their argument to `sh -c`
after format expansion. The guard is right, and the same class applies to the
ctl leg even though that text is never scanned.

**Working shape (validated end to end — sourced, pressed twice, one float, the
second press focuses it):** pass the id to the shell through a *register
window option*, named per tool, so the shell string only ever contains
`#{q:@og_float_target_<tool>}`:

```
if-shell -F "$LOOP" {
    set -wF @og_float_target_TOOL "$LOOP"
    ; run-shell "tmux select-pane -t #{q:@og_float_target_TOOL}"
} { <the existing floatNewPaneGuard> }
```

- `set -wF` expands its value, so the register holds the pane id the loop found.
- The write and the read are two commands in **one command list**, and the
  branch is only entered when the loop just proved a match — so a value is never
  read without being rewritten first in the same list. That is the stale-id
  story: **durable in the option table** (an ordinary window option, so
  tmux-remux may persist it), but **never consulted across a press**. It is not
  a lookup key; it holds no state anything else reads.
- The name is derived from the tool, so the register can never be read for the
  wrong tool even if that invariant were ever broken.
- Cost, accepted and worth naming in the PR: on a mirror the register write is a
  *remote* window-option change, which costs one extra subscription notification
  and a no-op row compare in the label shipper. No round-trip, no local option
  written, no forced reflow.
- No `#{...}` inside a shell string but is a plain `#{q:}` name → the
  shell-quoting guard passes (verified by running the suite against the exact
  emitted bind text).

Verified shapes for both legs (each sourced twice against a private server, a
second press focusing the float and the count staying at 1):

- **local** (context = the pressed window):
  `if-shell -F "<LOOP>" { set -wF @og_float_target_TOOL "<LOOP>" ; run-shell "tmux select-pane -t #{q:@og_float_target_TOOL}" } { <GUARD> }`
  — nothing needs `-t`, and the branch is a brace block, so `GUARD` keeps the
  exact nesting/escaping it has today (string-form `if-shell` inside a brace
  block is already how it ships).
- **remote** (context = the *current* window, which is not the target): the
  branch must be a **string-form** branch (the daemon quotes with `tmuxQuote`)
  and every command in it needs an explicit target:
  `if-shell -t PANE -F "<LOOP>" 'set -wF -t PANE @og_float_target_TOOL "<LOOP>" ; run-shell -t PANE "tmux select-pane -t #{q:@og_float_target_TOOL}"' '<create>'`
  Measured: `if-shell -t` pins the *condition's* context but **not** the
  branch's (with the current window short-circuited to a window holding no
  float, a branch's bare `run-shell` expanded the loop to empty), `run-shell -t`
  does pin it, and `set -wF -t` expands the value in the target window.

## Semantics chosen: **focus-or-create** (never stack)

A press with the tool's float already open in the window focuses it; it never
creates a second and never destroys the first. Rationale:

- #679 asks for accumulation to stop; it does not ask for a new destructive
  gesture.
- `prefix + x` already closes a pane, and closing on a second `prefix + y` would
  kill a float the user may have been *looking at* from another pane — a
  surprise the issue never asked for.
- Focus is idempotent, so repeat presses are harmless rather than cumulative.

Stated in the PR body, because the AC allows either and demands the choice be
named.

## Scope

`prefix + p`, `g`, `y` — the three **bridged tool binds** (`bridgedFloatTool` /
the ctl `tool` verb), exactly what #679 and its AC name.

`prefix + b`/`k`/`i` (btop, k9s, enrich card) go through the same
`floatNewPaneGuard` and have the same latent stacking behavior, but they are not
bridged, not named in the issue, and not in the AC. They stay as they are; the
PR body records that as a deliberate out-of-scope follow-up.

## Known limitation (stated, not silently accepted)

A window that already holds **two or more** floats for the same tool — i.e. a
stack created *before* this fix — makes the loop expand to more than one pane id
(`%2%3`), so the register holds an ambiguous target and `select-pane` reports an
error instead of focusing. New stacks cannot form. The local leg is where this
is reachable (the local bind has always stamped `@pane_label`); the remote leg
is not, because remote floats created before this fix carry no label at all and
so never match. `#{s/.../}`-style "take the first match" was tried and rejected:
its behaviour differs between the pinned next-3.9 and 3.7c, and a silent
degradation on older servers is worse than a visible error. Closing the extras
with `prefix + x` clears it. The remote leg's pre-fix floats carry no label at
all, so there the first press after the update adds one more labelled float
and every press after that reuses it.

## Steps

- [ ] **Step 1 — the failing test first: remote leg (`picker/remotebridge/daemon/ctl_test.go`).**
  Add `TestToolVerbDoesNotStackFloats` next to `TestToolVerbBuildsRemoteFloatInRemoteCwd`,
  shaped on `TestCarouselResolveScriptManifestCheck` (that file's own live
  harness): `startIsolatedTmux(t, "PATH="+stubDir+":"+os.Getenv("PATH"))` with an
  executable `prdash` stub that **sleeps**, build the `tool` verb's command with
  `verbs["tool"].build`, write `cmds[0]` to a conf and `source-file` it **twice**.
  Between presses, `select-pane` back to the base pane. Assert, from
  `list-panes -F '#{pane_id}|#{pane_floating_flag}|#{@pane_label}|#{pane_active}'`:
  exactly **one** float carries `@pane_label prdash`, and after the second press
  that float is the active pane again. Behaviour only — no command-text
  assertions here, so the red run fails for the real reason.

- [ ] **Step 2 — run it red.** `cd picker && go test ./remotebridge/daemon/ -run TestToolVerbDoesNotStackFloats -count=1`
  on the unmodified `ctl.go` → two floats → FAIL. Record the output for `## Evidence`.

- [ ] **Step 3 — remote leg fix (`ctl.go`).** In the `tool` verb's `build`, wrap
  the existing `new-pane` in the reuse gate, using the exact remote shape above
  (`tmuxQuote` every argument; `-t <pane>` on `if-shell`, on `set -wF` and on
  `run-shell`).
  - **The branch separates its two commands with a bare `;`, never `\;`.** The
    wire parses with `cmd_parse_from_string`, the same parser as a config file,
    where `\;` is a *literal* semicolon argument (measured: `\;` leaves the pane
    uncreated, `;` creates it; and the real control-mode path was exercised with
    a `-C` client too). Put that reason in a comment — it is not guessable.
  - The create branch gains `; set -p @pane_label <tool>` (today the remote
    float carries no label at all, so it could not be found on the next press).
    `tool` is already the whitelisted `remoteTools` key, so it needs no quoting.
  - Leave the `@float_geom` policy alone: still never stamped on the remote pane.
  - Build the loop text once and interpolate it into the condition, the register
    write and the option name, so the three cannot drift.

- [ ] **Step 4 — local leg fix (`generator/render/keys.go`).** Wrap
  `floatNewPaneGuard`'s output inside `bridgedFloatTool`'s local branch with the
  same gate, using the `tool` parameter as the label (it is already the same
  string the `suffix` stamps) and brace-block branches — no `-t` on this leg.
  Keep the guard's string-form `if-shell` so `-A` still parses lazily (#407).

- [ ] **Step 5 — correct the `-A` documentation where it reads as dedupe.**
  `ctl.go`'s `remoteFloatShort`/`remoteFloatFull` comment ("always carrying -A:
  …") and `keys.go`'s float-shape block: state that `-A` is z-order (the float
  stays above a zoomed pane) and that the reuse is explicit, in the bind, via
  `@pane_label`. Keep it to the sentences that are now wrong or silent.

- [ ] **Step 6 — Go unit parity**: update the exact-shape assertions in
  `ctl_test.go` (`TestToolVerbBuildsRemoteFloatInRemoteCwd`,
  `TestToolVerbUsesSuppliedCwd`) to the new command shape, and add a case to
  `generator/render/keys_test.go` asserting the local bind carries the loop for
  the label the same bind stamps with `set -p @pane_label <tool>` — the two
  places the label appears, cross-checked rather than eyeballed.

- [ ] **Step 7 — mirror the generator byte-for-byte in `config/tmux.conf.reference.nix`.**
  `bridgedFloatTool` there must emit the identical text (the extraction check
  diffs `generatedConf` against `referenceConf`). This is the oracle, not
  scratch space: the two files are edited together.

- [ ] **Step 8 — local leg behavioural regression (`tests/float-tool-focus.bats` + a flake check).**
  Model on `tests/rename-bind-integration.bats` and its
  `rename-bind-integration-tests` derivation — including its reason: a key
  binding fires only for an **attached client**, so the harness is a second
  server whose pane runs `tmux attach`. Details that matter:
  - config from `./config/tmux.conf.nix` with `mkTmux`, `prdash` wired in, and
    `enrichEnable = false; agentUsageEnable = false;` (the rename check's own noise cut);
  - a stub `yazi` **that sleeps** earlier on `PATH` than the real one (the yazi
    bind runs the bare name) — an immediately-exiting stub is closed by the
    `remain-on-exit off` the bind stamps, so the assertion would see zero
    labelled panes on *both* trees and the red run would prove nothing;
  - `SHELL` pinned to bash so the pane's command resolves;
  - `@splash_shown 1`, plus the `CLAUDE_STATUS_DIR` / `OG_*` scratch dirs every
    live-tmux bats suite here sets (`/tmp/claude-status` is machine-global);
  - assert `bridgeGate` is off in the scratch server, press `prefix + y` twice,
    then assert exactly one pane carries `@pane_label yazi` and it is a float.

- [ ] **Step 9 — run Step 8 red too.** Build the base revision's wrapper
  (`git archive <base> | tar -x -C <scratch>` + `nix build path:<scratch>#default`)
  and run the same bats file with `TMUX_BIN` pointed at it → two floats → FAIL.
  No live-work revert; the counterfactual lives in a scratch tree.

- [ ] **Step 10 — static coverage.** Extend `bridge-tool-bind-assertions` in
  `flake.nix` to require the reuse gate on each of `p g y` (the loop beside the
  ctl tool branch), so a later edit that drops it fails the build rather than a
  user's window filling up.

- [ ] **Step 11 - `CLAUDE.md`.** Add the invariant where the float conventions
  live (the `@float_geom` bullet in Key Conventions): a tool float bind reuses
  its existing float by `@pane_label` through the `@og_float_target_<tool>` register,
  `new-pane -A` is a z-order flag and never attach-if-exists, and the
  pre-existing-stack limitation above. Re-read the "What the Remote Host Needs on
  PATH" row that names `new-pane -A` and correct it if it now reads wrong.

- [ ] **Step 12 — commit the plan** as `docs/superpowers/plans/2026-09-18-tool-float-focus-or-create-679.md`
  (this document, with the critic-driven revision folded in), per the repo's
  "a substantive change commits its plan" rule.

- [ ] **Step 13 — the local gates**, all of them, and the scoped ones first while
  iterating: `nix build .#default`, `nix flake check` (bats + Go, incl.
  `picker-go-tests`, `float-conf-assertions`, `bridge-tool-bind-assertions`,
  `bind-note-assertions`, `conf-shell-quoting-tests`, and
  `tmux-conf-extraction-assertions`), `nix build .#lint`
  (alejandra/statix/deadnix/shellcheck/shfmt/typos — the reference-nix and bats
  edits are exactly what it catches).

## Evidence (per EVIDENCE_REVIEW)

Violated invariant: "a window holds at most one float per tool"; the producer
that owns it is the bind (local) and the ctl `tool` verb (remote).

- Red/green, remote: Step 2 (base `ctl.go`) vs Step 3 — same test, same command.
- Red/green, local: Step 9 (base wrapper) vs Step 4/7 — same bats file.
- Consumer map for `@pane_label`: producers today are the generator's float binds
  and `picker/remotepick.go`'s `remote <host>`; the consumer is
  `scripts/tmux-apply-theme-colors.sh`'s `pane-border-format`, which *renders the
  value* as the pane border title (not merely a presence test). This change adds
  a **producer** — the remote float now stamps it — and a **consumer**, the
  predicate. Locally nothing changes: the same binds stamp the same values. The
  remote gain is that its own border title now reads the tool name too; it does
  **not** cross the bridge, since no `@pane_label` reference exists anywhere under
  `picker/remotebridge/`, so a local mirror float still gets no label.
- `@og_float_target_<tool>` is new and has exactly one writer (`set -wF`) and one
  reader (the focus branch's `run-shell`), both in the same command list;
  nothing else in the tree reads it.
- The remote's `set -p @pane_label` is deliberately *not* `@float_geom`: that
  option stays unstamped remotely so the remote's `tmux-float-refit` cannot fight
  the mirror for geometry authority (the reason already documented there).

## Risks / review-risk notes

- Cross-version: a new local config against a **not-yet-rebuilt** remote keeps
  stacking (the ctl verb is the remote's code). Called out in the PR body.
- `;`-vs-`\;` on the wire is the subtlest part of this change; it is commented at
  the site and exercised by the live Go test.
- The transient register is the shape most likely to draw a review question
  ("is this a second lookup key?"); the answer — written once and read once in the
  same command list, entered only when the `@pane_label` loop already matched —
  belongs in the code comment, not only here.
- Nesting a second string-form `if-shell` inside a brace block is unusual for
  this file; the extraction check is what keeps Go and Nix in step.
