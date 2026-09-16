# Adopt native modal floats, retire `@pane_keys_raw` and `@pane_label` (#648)

## Task

Issue #648 asks to adopt tmux 3.8's native `new-pane -O -K -T` (modal / all-keys
/ title) flags in place of two hand-rolled options: `@pane_keys_raw` (keeps
C-hjkl/M-l for a float's own keymap) and `@pane_label` (drives the mauve
titled-border branch of `pane-border-format`). It also asks to fix a
pre-existing comma-parsing bug in `pane-border-format` while restructuring it.

A comment on the issue claims this already "merged in main." It did not —
`@pane_keys_raw` was still present in `config/tmux.conf.tmpl` and
`config/tmux.conf.reference.nix`, and no PR referenced #648. This plan started
from main as of #649/#665 and #629/#664 (every bind carries `-N`, enforced by
`bind-note-assertions`).

## Verification performed BEFORE writing code (mandatory per the issue)

All of the following was measured directly against the **pinned** tmux binary
(`tmux/tmux@e880cf63e0a9fe095d7c5d313761520fb1a8653c`, reports itself as
`next-3.9`), extracted unwrapped from the cached `nix build .#default` output
(`-f /dev/null`, private `-L`/`TMUX_TMPDIR` per CLAUDE.md's scratch-server
rule) — never against the ambient PATH tmux.

Two capture techniques were needed:

- **Format-string questions** ("what does this ternary evaluate to for pane
  X") — `tmux display-message -p -t <pane> -F "<format>"`. The technique the
  prior `2026-09-16-adopt-tmux-3-8-features.md` plan (Step 1.0) used to find
  the original comma bug, reused here.
- **Rendering questions** ("does tmux draw a border-status line for this pane
  at all", "does a keystroke reach a bind or the pane") — control-mode (`-C`
  attach) and `send-keys` are both **wrong tools** here: control mode never
  sends border/status chrome over `%output` (measured: a `pane-border-status
  top` test showed zero border bytes in a `-C` capture), and `send-keys`
  injects directly into a pane, bypassing the server's key-table dispatch
  entirely (measured: a root-table `bind -n C-l` never fired via `send-keys`
  on an ordinary pane, where it unquestionably should). What actually works:
  a real pty (`os.forkpty` in Python, since this sandbox has no interactive
  tty) reading the composited terminal frame or delivering a real keystroke.

### Finding 1 — `pane-border-status top-floating` cannot coexist with the
existing tiled-pane decorations

Verified on a 2-tiled-pane window, both with a distinctive `pane-border-format`
marker, captured via the real-pty technique:

| `pane-border-status` | tiled panes get a border-status line? | a float in the same window? |
|---|---|---|
| `top` (current value) | **yes**, both panes | yes |
| `top-floating` | **no — none at all** | yes, only the float |

`top-floating` is not "top, but floats also get a title" — it is structurally
exclusive: switching to it deletes the border-status line (and therefore
`@bridge_crew_role`/`@bridge_crew_name`/the multi-pane `●` marker) from every
ordinary tiled and mirror-window pane. **Declined**: `pane-border-status`
stays `top`.

### Finding 2 — neither `-T`'s `pane_title` nor the native
`#{pane_floating_flag}` is a safe replacement signal for `@pane_label`

1. **Every pane already has a non-empty `pane_title`** by default (a plain
   shell pane showed `title=noams@halo: ~/path...`, the shell's own OSC-2
   title). `pane_title` is truthy for every pane unconditionally, so
   `#{?pane_title,...}` cannot distinguish "one of our labeled utility
   floats" from an ordinary shell pane, the way empty/unset `@pane_label`
   cleanly does.
2. **The label drifts under at least one real tool.** Floating `yazi` with
   `-T test-yazi` reset its own title to `Yazi: <dirname>` within ~1s (its own
   OSC-2 write); `lazygit`/`btop` held their `-T` value for the ~2s window
   tested, but nothing guarantees they always will.
3. **`#{pane_floating_flag}`** (native, structural, verified correct as a
   float/tiled gate in a `pane-border-format` ternary) is closer, but it is
   true for **every** floating pane, including a bridge-daemon-reconciled
   mirror float (`picker/remotebridge/daemon/floatgeom.go`'s `new-pane`,
   which stamps neither `@pane_label` nor `@bridge_crew_*` on creation — those
   `@bridge_*` options land later from the label/agent-state shippers).
   Swapping the ternary's outer gate to `pane_floating_flag` would make a
   *plain* mirrored float (no crew role yet stamped) take the "labeled float"
   branch instead of falling through to the multi-pane `●` marker it gets
   today — the exact mirror-window regression class the issue's #535 caveat
   warns about.

**Declined**: `@pane_label` is kept, unchanged, as the signal driving
`pane-border-format`'s labeled-float branch. `-T` is not adopted for the
general tool floats (btop/lazygit/yazi/prdash/k9s/enrich card) — no
verified-safe consumer once both of the above are ruled out.

### Finding 3 — the comma bug affects **two** branches, not one

The referenced plan (`2026-09-16-adopt-tmux-3-8-features.md`, Step 1.0) found
the *default* branch (`#[bg=X,fg=Y]━━━━━`) silently empty, because
`format_choose`/`format_skip1` track ternary-branch nesting for `#{...}` but
not `#[...]`, so the bare comma inside `#[bg=X,fg=Y]` is misread as a
ternary-argument separator.

Re-running that same probe against the **second** occurrence of the identical
pattern — the `@pane_label` branch's `pane_active == false` arm — showed it is
**also** silently empty today. Not called out in the prior plan (scoped to
the default branch alone); new information this PR surfaces.

**Fix**: split every `#[bg=X,fg=Y]` into two single-attribute blocks,
`#[bg=X]#[fg=Y]`. Verified this resolves both occurrences without changing
any other branch's rendering.

### Finding 4 — `-O`/`-K` on the `^o` remote-picker float work as documented
and are additive; `-C` is a necessary escape hatch, not an optional extra

Verified with real pty-delivered keystrokes and SGR mouse escape sequences:

- **`-K` alone**: a root-table bind does not fire when the key is sent to a
  `-K` pane. Complete native replacement for `@pane_keys_raw` + the M-l
  `if-shell` conditional + `tmux-smart-nav`'s trailing arg, since
  `@pane_keys_raw` is stamped in exactly one place
  (`picker/remotepick.go`'s `^o` float).
- **`-K` alone does not block `select-pane`**: a programmatic `select-pane -t
  <other>` (standing in for a mouse click, or another client's action)
  succeeds in moving focus away from a `-K`-only float.
- **`-O` (added alongside `-K`) blocks it**: the identical `select-pane`
  attempt is refused, focus stays on the modal float — genuinely additive
  over `-K`, not redundant.
- **`-K` swallows the entire prefix sequence**, not just the four
  `C-hjkl`/`M-l` keys `@pane_keys_raw` ever intercepted. CLAUDE.md already
  documents a real, pre-existing hang (#486): `^o` on a host-key-changed or
  Tailscale-ACL-check row dials this float's own *unbounded* interactive ssh
  leg (`scripts/og-remote-picker.sh`'s `local_pick`: only legs 1 and 3 are
  `timeout 8`-wrapped). **Today**, a hung `^o` float can still be killed via
  `prefix + x` (kill-pane), since `@pane_keys_raw` never touched the prefix
  table. **With bare `-K`**, that keyboard escape is gone.
- **`-C` (close on click outside) restores an escape, independent of `-K`.**
  Verified on the pinned binary (`set -g mouse on`, a 2-tiled-pane window plus
  an `-O -K -C` float): a mouse click outside the float's bounding box closes
  it even with `-K` active — `-C`'s click-outside-close operates on mouse
  coordinates against pane geometry, structurally, not through the key tables
  `-K` bypasses.
- **`-O` enforces "one modal pane per window"**: verified a second `-O` float
  in the same window is refused outright (`tmux` error: "window already has a
  modal pane", exit 1) — `picker/tui.go`'s `^o` handler already surfaces any
  `.Run()` error via `m.statusMsg`, so this reads as a graceful in-popup
  message, not a crash or silent no-op.

**Decision, per bind:**

- `^o` remote-picker float (`picker/remotepick.go`): **`-O -K -C`**. The one
  float `@pane_keys_raw` ever applied to; needs every key including prefix
  for its own remote keymap; blocking accidental pane-switch-away is the
  right UX for something modal-shaped by design; `-C` is the required
  mouse-based escape for the documented #486 hang once `-K` removes the
  keyboard one.
- `prefix + b/g/y/p/i` utility floats (btop, lazygit, yazi, prdash, enrich
  card) and `k9s`: **no change**. Meant to be tabbed away from while left
  running in the background; `@pane_keys_raw` was never stamped on any of
  them, so nothing needs `-K`, and `-O` would be a real regression (no more
  `prefix + o` away from a running lazygit float) with no problem it solves.
  `-T` is not added either (Finding 2).
- No version guard needed for `-O -K -C` on the `^o` float: like the existing
  unconditional `-A` in the same argv, this float only ever runs via
  `exec.Command` from the local, pinned wrapped tmux server — no older-server
  case to fall back to, unlike the *bridged tool* binds (out of scope here,
  unchanged).

### Interaction traced: `-O` vs. the bridge daemon's own `focusLocalPane`

`^o` is explicitly usable from *inside* a mirror window (#535). While a
remote session is mirrored, the daemon's reconcile loop calls `focusLocalPane`
(`picker/remotebridge/daemon/reconcile.go:732-743`) → `select-pane -t <local>`
on that window every time the *remote's* active pane changes. With an `-O`
float open there, that call is refused like any other pane-switch attempt.

Traced rather than treated as hypothetical: `focusLocalPane` calls
`cst.noteLocalFocus(w.remoteID, remoteActive)` (`focus.go:112-119`) **before**
issuing `select-pane`, and only logs a refusal to stderr — `noteLocalFocus` is
a plain field write (`f.localActive`, `f.remoteActivePane`), not an entry in
the `f.commanded` echo-matching FIFO used elsewhere in that file. So a refused
`select-pane` strands nothing in that FIFO. Net effect: the daemon's *belief*
about local focus can diverge from actual focus while the modal float is
open. It resolves on the next remote focus change to a **different** pane
(`applyRemoteFocus`'s `if pane == f.localActive { return "", false }` would
otherwise suppress the correction if the remote's focus returns to the same
pane it already "moved" to), or the next local focus gesture (a local click
fires `after-select-pane` → `planFocusLocked`, which drags the *remote* to
the local pane instead) — not instantly, but bounded and self-correcting.

**Decision: keep `-O`.** The failure mode is a narrow, self-healing focus
display lag while the user has this window's modal picker open, versus
losing `-O`'s real benefit. Documented in `CLAUDE.md`'s `@float_geom` bullet
and `picker/remotepick.go`'s doc comment.

## Scope

**In scope:**
1. Fix the `pane-border-format` comma bug (both occurrences) in
   `scripts/tmux-apply-theme-colors.sh`.
2. Add `-O -K -C` to the `^o` remote-picker float's `new-pane` call
   (`picker/remotepick.go`), delete its `@pane_keys_raw` stamp.
3. Delete `@pane_keys_raw` everywhere else: the `M-l` conditional and the four
   `C-hjkl` binds' trailing arg (`config/tmux.conf.tmpl` +
   `config/tmux.conf.reference.nix`, edited together), and
   `tmux-smart-nav.sh`'s `raw` parameter/branch.
4. New regression test (`tests/pane-border-format.bats`) for the border-format
   fix and the `^o` float's new flags, registered in `flake.nix`; updated
   Go tests for the new argv shape.
5. `CLAUDE.md`: remove the `@pane_keys_raw` bullet, update the `@float_geom`
   bullet (also fixes a pre-existing stale `mkFloat` location — it lives in
   `generator/render/keys.go`, not `config/tmux.conf.nix`, since the
   `og-generate` extraction), update the `tmux-apply-theme-colors` Script
   Roles row.
6. This plan doc.

**Explicitly declined (with the evidence above):**
- `-T` / `pane-border-status top-floating` anywhere.
- `@pane_label` retirement.
- `-O`/`-K`/`-C` on any float besides `^o`.

**Out of scope (verified unaffected):**
- `generator/render/keys.go`'s `mkFloat`/`floatBind`/`bridgedFloatTool` and
  `picker/remotebridge/daemon/ctl.go`'s `remoteToolFloat`/`remoteFloatShort`/
  `remoteFloatFull` — untouched, since no tool-float flags change.
- `flake.nix`'s `float-conf-assertions` — scans the generated tmux.conf text;
  `remotepick.go`'s float is Go-invoked `tmux`, never in that text.

## Process note

This plan went through two rounds of adversarial `plan-critic` review before
execution. Round 1 returned `revise` with 2 blocking findings: a test-index
break in `picker/remotepick_test.go` (`TestRemotePickNewPaneArgsHostWithEmbeddedQuote`
reads `args[12]` positionally, which the new flags shift), and the
`focusLocalPane`/`-O` interaction above, not originally considered. Both are
reflected in the final design and code as written. The `-C` addition was a
direct result of round-1 review surfacing the `-K`-swallows-prefix escape-hatch
problem for the documented #486 hang. Round 2 accepted with only non-blocking
notes (a wrong argv-index worked example, a vacuous test-assertion phrasing,
imprecise "immediately"/"silent" wording, and a suggestion to strengthen the
new bats test with a real float-creation + modal-enforcement check) — all
incorporated during implementation and code review.

The code-review gate (go-reviewer, shell-reviewer, a general Nix reviewer, an
`agent-docs-reviewer` for CLAUDE.md, and a general reviewer for the
`config/tmux.conf.tmpl` file the automated roster doesn't glob-match) approved
four of five; `agent-docs-reviewer` returned **Block** on one HIGH finding — a
fabricated cross-reference in the `@float_geom` CLAUDE.md bullet pointing at
two sections that don't discuss focus-tracking and are physically above the
bullet, not "below" as written — plus a MEDIUM (a `select-pane` refusal is
logged to daemon stderr, not purely "silent"). Both fixed and verified before
push.

## Acceptance

- `@pane_keys_raw` is gone from code, tests, and CLAUDE.md.
- `@pane_label` is explicitly kept, with the PR body explaining why (Finding
  2 above).
- `tests/pane-border-format.bats` covers all 6 border-rendering cases
  (labeled float active/inactive, bridged crew role, bridged crew name,
  multi-pane marker, true default) plus one verifying the `^o` float's
  `-O -K -C` flags are accepted and `-O`'s modal enforcement actually fires;
  the border-format cases were confirmed to fail against the pre-fix string
  (a negative control baked into the test) and pass against the fixed one.
- `nix build .#default`, `nix flake check`, `nix build .#lint` all green.
- This plan doc committed under `docs/superpowers/plans/`.
