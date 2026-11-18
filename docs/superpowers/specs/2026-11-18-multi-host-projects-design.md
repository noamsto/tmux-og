# Multi-host projects: one local session per project, windows routed to the worktree's host (#1180)

## Goal

Today, working on one project from several machines means one tmux session per
host and remembering which host a branch lives on. This design gives each
project **one local tmux session** that holds the project's windows from every
host: local windows are native, remote windows are bridge mirrors. A new window
opens on the host that owns its worktree, and the pickers show the project
across hosts. The owner's words: "a seamless project on multiple hosts
workflow".

This document is a design. Nothing here changes production code. The phased
implementation plan is `docs/superpowers/plans/2026-11-18-multi-host-projects.md`.

## Non-goals (v1)

- Panes of two hosts in one window (see "Layout", question 6).
- Moving a running window between hosts.
- Cloning a project onto a host that has no checkout.
- Choosing the host for a new worktree automatically from load or capacity.
- Replacing dedicated mirrors (`og-remote-open <host> <sess>`) or filtered
  placed mirrors (`--into … --window`). Both keep working unchanged.

## What exists (the design builds on these)

| Piece | Where | What the design uses it for |
|---|---|---|
| Placed mirrors: `og-remote-open <host> <sess> --into <local-session>` | `bridge-daemon.md`, "Placed mirrors (`--into`, #985)" | The pickup mechanism. One daemon ↔ one remote session ↔ one local session; several daemons share the local one; own windows identified by window-scoped `@bridge_sock`. |
| Filtered mirrors: `--window @opt=val` / `@N` | "Filtered mirrors (`--window`, #989)" | Not used by the project path (see decision 3.2). Stays for the dispatcher. |
| Mirror identity `@bridge_host`, `@bridge_session`, `@bridge_placed`, `@bridge_pane`, `@bridge_dir`, `@bridge_crew_branch`, PR/issue stamps | `bridge-shipped-state.md` | A local window's host and branch are readable without a remote call. |
| Launcher dedup: same (host, remote session, local session, selector) reuses the live daemon | `og-remote-open` row in `scripts.md` | Idempotent on-demand pickup. |
| An emptied registry ends a placed daemon; a destroyed pinned session ends it (#934) | `bridge-daemon.md` | Teardown semantics for free. |
| Size ownership: daemons into one local session do not fight (#979) | "Size ownership across controllers (#979)" | Several host daemons in one session need no new rule. |
| `ctl new-window` / split verbs run on the remote against the mirrored window's own session | `picker/remotebridge/daemon/ctl.go` | `prefix + c` and splits inside a mirror window already create the window on that window's host. |
| `@remote_bridge_hosts`, `configuredHosts`, `picker/hostcolor`, the `remote-windows` cache and one-ssh-per-host probe | `picker.md`, "Remote windows in the window picker (#902)" | Host list, host colours, the probe/cache shape the new data follows. |
| worktrunk `post-switch` hook → `tmux-worktree-match` / `tmux-reconcile-window` / `tmux-issue-stamp` | `modules/home-manager.nix`, `scripts.md` | Local-only worktree navigation. Unchanged. |

## The central observation

Most of "route the window to the right host" is already true. A window created
**from inside a mirror window** (`prefix + c`, a split, `wt switch` typed in a
mirrored shell) is created by the *remote* tmux, in the remote session the
mirror shows, and a plain placed daemon mirrors it back into the local session
as soon as it exists. Routing logic is only needed at **entry points that have
no current window to inherit a host from**: opening a branch from a picker,
creating a new worktree, and attaching a host to a project. The design is
therefore a small routing layer plus a registry, not a change to the bridge.

## 1. Project identity and registry

**Identity.** A project is its `origin` remote, normalised to
`<forge-host>/<owner>/<repo>`: scheme, user, port, a trailing `.git` and a
trailing `/` stripped, and only the forge host lower-cased (owner and repo keep
their case, since not every forge folds it), so
`git@github.com:noamsto/tmux-og.git` and `https://github.com/noamsto/tmux-og`
are one key, `github.com/noamsto/tmux-og`. `git remote get-url` already applies
`url.<base>.insteadOf` rewrites; an ssh `Host` alias that git cannot see
(`git@gh-work:org/repo`) gives a different key on that host, which the
declarative layer fixes with `remotes = [ <extra keys> ]` aliases for one
project. Forks and same-named repos differ by owner. A repo without an `origin`
has no project key and is never routed (it keeps behaving as today).

**Slug.** The tmux session name for the project is `mirrorname.Part(<repo>)`
(`[A-Za-z0-9_-]`, so stock session menus parse it). On a collision between two
keys the slug becomes `<owner>-<repo>`. The raw key lives in the session option
`@og_project`, never in the name, for the reason `@bridge_session` is raw. The
slug names only the *local* hub; the remote leaf name is derived from the key
alone (3.1), so two controllers, or one controller after a local collision
clears, always agree on it.

**The key is validated where it is made.** `NormalizeKey` returns `ok=false`
for any key outside `[A-Za-z0-9._/@+:-]` (the key comes from a host's `git
remote get-url origin`, which a remote host controls), so a key never carries
`#`, quotes or control bytes into a session option, a cache file name, the
registry merge or a message.

**Registry.** Two layers, merged with the declarative layer winning:

1. *Declarative (optional)*: `programs.tmux-og.projects.<name> = { remote =
   "<key or url>"; hosts.<host>.path = "<checkout path>"; }`, rendered into a
   JSON file that the Go side reads (follow `picker/ogconfig`; the exact
   reader is verified in plan phase P1). It fixes the one thing discovery
   cannot find: a host where the project was never opened.
2. *Discovered*: one bounded ssh probe per host (`BatchMode`, the session
   probe's timeout and `classifyProbeErr`), plain `sh` + `git`, so it needs
   nothing from tmux-og on the remote. It collects candidate checkout roots
   from three sources: the remote tmux server's windows (`#{@git_root}`, which
   tmux-og already stamps), the host's zoxide directories (the #1099 script),
   and registry paths. Each candidate resolves to its main checkout by
   `git rev-parse --git-common-dir`, its key from `git remote get-url origin`,
   and its worktrees from `git worktree list --porcelain`. The result is a
   per-host snapshot: `{key, repoPath, worktrees:[{path, branch, head}],
   defaultBranch, drift}`.

**A host's checkout path** is the registry path if declared, else the
discovered main checkout. A host where neither finds the project is simply not
a host of that project. `og project open --host H` for such a host fails with
`no checkout of <key> on H — declare it in programs.tmux-og.projects or open the
repo there once`. Cloning is out of scope.

**Local project sessions (the hub).** The first `og project open` for a key
creates the local session `<slug>` and stamps the session option
`@og_project=<key>`. The session starts in the local checkout, or in `$HOME`
when this machine has none, with a plain shell window: a placed daemon needs an
existing session, and the window is the session's anchor, which the user may
close once mirror windows exist. Session options are read with session-scoped
`show-options` (no `-w`), the placed-mirror rule, so a window option can never
impersonate them.

## 2. Routing rule

**Confirmed, with one refinement.** A window runs on the host that owns its
worktree. Ownership is "the worktree path appears in that host's `git worktree
list`". A worktree is created on exactly one host, and a branch should be
checked out on one host only (question 4). The refinement is where the rule
applies: it is a *lookup at entry points*, because everywhere else the host is
inherited from the window the action starts in.

| Action in a project session | Current window | Behaviour |
|---|---|---|
| `prefix + c` | local | Native `new-window -c <cwd>` as today. |
| `prefix + c` | mirror | Existing ctl `new-window` on that mirror's remote session, cwd of the mirrored pane. The new remote window is mirrored back by the placed daemon. No new code. |
| `prefix + \|`, `prefix + -`, other splits | either | Unchanged: native split locally; ctl split on the remote (the window is all one host). |
| `wt switch <branch>` typed in a pane | either | Runs on the pane's host, under that host's own post-switch hook, so the outcome is the hook's: a new window (mirrored back when it lands in a remote session a daemon watches), an existing window selected (the local view does not move: a placed daemon never moves it), or the current window taken over. Agent sessions skip the hook (`$CLAUDECODE` and friends). Not changed by this design; see "Remote prerequisites". |
| Open an existing branch from a picker, or `og project open <branch>` | none (entry point) | Resolve `branch → host` from the cached snapshots. One host: ensure pickup (3.1), create or focus the window there. Two hosts: prefer the host of the current window if it has the branch, else ask. None: error. |
| New worktree (picker flow, `og project new <branch> [--host H]`) | none | The host is an explicit choice, default **local** (owner decision D3). Created on that host from a fresh `origin/<default>` (question 4). |
| Picker "open" on a worktree already open | none | Never open a second window. Identity is `(host, worktree path)`. Open in the project session (local, or a mirror: `@bridge_host` plus `@bridge_dir`/`@bridge_pane`, the placed-window match `placeFocus` uses): focus it. Open natively on the host outside the leaf: mirror that window (section 3.6). |

**Opening a window on a remote host** is ssh to the host (it rides the picker's
ControlMaster) running
`tmux new-window -P -F '#{window_id}|#{pane_id}' -t '=<leaf>:' -c
<worktree-path>` when the leaf exists. When it does not, the leaf's **first
window is the target window**: `new-session -d -P -F '#{window_id}|#{pane_id}'
-s <leaf> -c <worktree-path> \; set-option -t <leaf> @og_project_leaf <key>`
in one tmux invocation (the leaf name is never numeric, so `-t <leaf>` is
unambiguous), so no stray anchor window exists and no instant has the leaf
unstamped. Every remote tmux call resolves the binary and socket directory the
way the picker and launcher do (`remoteTmuxBin`, `TMUX_TMPDIR` pinned by
`remoteTmuxCmd`/`rm.env()`), never a bare `tmux`: a non-interactive ssh
`PATH` may lack it, or reach a different socket than the daemon's. There is **no `-n`**: `new-window -n` runs tmux
format expansion (`#()` jobs included) on the remote, and `#(` and `#{` are
legal in git ref names. The window is named by the existing reconcile path from
its cwd, as a local `wt switch` window is. The launcher then waits (bounded, a
few seconds) for the local mirror window whose `@bridge_host` and `@bridge_pane`
match the returned pane id, and `select-window`s it. A placed daemon never moves
the user's view, so the select is the launcher's job.

**Nothing free-form reaches a remote login shell (often fish).** The worktree
path must be absolute and match `[A-Za-z0-9._/@+:-]` (no space, quote,
backslash, `#`, `$`, backtick or control byte); a branch must pass `git
check-ref-format --branch` *and* `[A-Za-z0-9._/-]`. Anything else is refused
with a message in v1: a rare branch name is cheaper than a quoting rule that fish
and sh must both honour. Windows and sessions are addressed by id.

**Remote prerequisites.** The remote needs `git`, `wt` (worktrunk, with its
tmux-og post-switch hook, so a remote `wt switch` window is tagged and named),
a tmux new enough for `new-window -P -F` (any supported), and, for the
mirror itself, everything in `bridge-daemon.md` "What the Remote Host Needs on
PATH". The probe itself needs `sh` and `git`, plus the remote's `tmux
list-windows -a` for the windows of 3.6; with no tmux server, or windows
without `@git_root` stamps, it degrades silently to checkouts and worktrees
only. A missing `wt` is reported by
`og project new` as `wt not found on <host>` before anything is created.

**`wt switch` run locally for a branch that is open on another host** is not
intercepted: it is local by definition, and `tmux-worktree-match` cannot see
mirrors because the daemon writes `@bridge_*`, never `@worktree`. The result is
a second checkout of the same branch, which the same-branch detection (question
4) flags. An optional worktrunk `pre-switch` warning is a plan item, not a
requirement.

## 3. Remote pickup

### 3.1 Mechanism and topology

**Hub and spoke.** The local project session is a *hub*: it holds native
windows and mirrors. Every remote host contributes a *leaf*: a remote session
holding only native windows tmux-og created for the project. A leaf never holds
mirrors, so a mirror never mirrors a mirror, and the controller is the only
place the project's windows are united. This is why the pickup mirrors a
separate leaf and never the host's own hub: if host `H` also runs this feature,
`H`'s `<slug>` hub holds mirrors of the other hosts' windows, and mirroring it
would loop. Windows a user opens natively on `H` outside the leaf are covered
in 3.6.

Each `(project, host)` pair has one **remote project session**, created by
tmux-og on the host, holding that project's windows there. A placed, **unfiltered**
daemon mirrors it into the local project session:

```text
og-remote-open <host> <remote-session> --into <slug>
```

- **Remote session name** is the *leaf name*
  `og-<repo>-<8 hex of sha256(key)>` (`<repo>` through `mirrorname.Part`),
  derived from the key alone, so every controller computes the same name and two
  projects with the same repo name cannot share a leaf. It is stamped on the
  remote with the session option `@og_project_leaf=<key>`. (A hub carries
  `@og_project`, a leaf carries `@og_project_leaf`; the two never share a
  session.) The `og-` prefix means the session never collides with one the user
  already keeps on that host, and the stamp lets the probes and the launcher
  recognise it. **An existing session of that name is reused only if its
  `@og_project_leaf` equals the key**; otherwise `og project` refuses with a
  message (a hand-made or colliding session is never silently adopted).
- **Started on demand** by `og project open`/`new`, and by an explicit
  `og project attach <project> <host>`. Nothing starts at server boot in v1.
- **The remote session's windows are exactly the project's windows on that
  host.** A plain placed daemon already tracks `%window-add` for the whole
  remote session, so a window created by `prefix + c` or `wt switch` there
  needs no selector, stamp or reconcile to appear locally (decision 3.2).

### 3.2 Decision: unfiltered, one daemon per (project, host)

Filtered mirrors (`--window @opt=val`) exist for a remote session that holds
windows of several kinds. The project session holds one kind by construction,
so a filter adds only the double-mirror risk the filtered-mirror section
documents ("overlapping selectors, or a plain `--into` beside a filtered one …
mirror a window more than once"). The rule is therefore: **the project path
never starts a filtered daemon, and never starts a daemon beside a filtered one
on the same (host, remote session, local session).** The launcher gets one new
check enforcing it (plan phase P1): refuse `--into L` on `(host, S)` when a live
daemon for the same triple has the other selector kind.

### 3.3 Deduplication and the launcher guard

The launcher's own dedup is narrower than "one mirror per remote session".
Verified in `picker/remoteopen/launcher.go`: a dedicated open finds its mirror
by `pairLookup(host, sess)` among dedicated sessions, and a placed open keys its
socket by `(host, sess, serverPid, intoID, selector)` (`intoSockPath`). So a
dedicated open, or an `--into <other-session>` open, of the same remote session
is not refused today and would mirror it twice (two renderers and two size
claims on one remote window). Hiding project sessions from the picker (3.5)
only removes the picker's way in; `og-remote-open` itself, the dispatcher and
the #1106 Enter-places path are not covered by it.

So the invariant, **a remote leaf is mirrored once per local machine, into its
hub**, needs a guard in the launcher (plan phase P1), not a convention:

1. Before it dials, the launcher reads the target session's two stamps in one
   ssh round trip beside the `has-session` it already makes
   (`picker/remoteopen/remote.go`). `show-options` rejects the `=` exact-match
   prefix (`docs/agents/tmux-gotchas.md`, #474), so the read first resolves the
   id, `display-message -p -t '=<sess>:' '#{session_id}'`, requires a `$N`
   answer, then runs `show-options -qv -t '$N' @og_project_leaf` and
   `… @og_project`. A session-scoped read (no `-w`) never sees a window option,
   so a window option cannot forge a stamp. The read **fails open** only when
   the *session cannot be resolved or the call cannot run* (no `$N`, an error,
   a timeout, an old remote): the open proceeds as today. A resolved session
   whose options read empty is "no stamp", not an error. (The picker's own
   session rows, which use `list-sessions -F` and so evaluate in the session's
   current window, use the `#{?#{@bridge_placed},,#{@og_project…}}` guard idiom
   instead.)
2. A target session with `@og_project_leaf` is opened only by `og project` (an
   internal flag the verb passes); any other open of it is refused with
   `<host>/<sess> is a project leaf — use og project open`.
3. A target session with `@og_project` (a hub, which holds mirrors) is refused
   unless the open is an id-selector (`--window @N`) placement of one window
   (3.6). A plain open of a hub would mirror other hosts' mirrors. An
   option-selector open (the dispatcher's `@crew_branch=…`) is also refused
   there in v1, the conservative reading, since the launcher cannot prove the
   selector excludes mirror windows. **A dispatcher running remote workers in
   a hub session of a host that also runs `og project` is a known v1 limit**;
   workers in any non-hub session are unaffected, and so is every user who
   configures no project.
4. `og project` itself reuses a live daemon for the leaf, found by scanning
   local windows' `@bridge_host`/`@bridge_session` across all local sessions
   (the `collectBridgeMirrors` scan, placed-aware), so a second local session
   cannot start a second daemon either: it focuses the window in the hub. The
   scan-then-start runs under one per-(host, leaf) lock, not the launcher's
   per-`intoID` placed lock, so two local sessions cannot race past the scan.

The check lives in `picker/remoteopen`, tested by a unit test over the parsed
stamp read (including the `=`-prefix regression: an unresolvable id must not
read as "no stamp" silently in the test double) and a bats case with a fake
remote. A remote whose tmux
cannot report the stamps has none to report: a leaf is created by this
revision's `og project`, so its stamp exists by construction, and the
stamp-less case is only a hand-made session (best effort there).

### 3.4 Teardown

- The daemon ends when the remote session empties (existing behaviour). In
  project mode an empty host is a normal state, not an error: the host chip
  reads `idle`, and the next open recreates the remote session and the daemon.
- `og project detach <project> <host>` stops that host's daemon and kills only
  its own windows (the placed teardown rule), leaving the remote session and its
  processes running. It exists so a host can be hidden without losing its work.
- The local project session is never killed by teardown; it ends when its last
  window closes, like any session.

### 3.5 Relationship to the existing modes

| Mode | Status |
|---|---|
| Dedicated (`og-remote-open <host> <sess>`) | Coexists, unchanged. It is the way to browse a host's non-project sessions. The window picker's Remote section hides `@og_project_leaf` and `@og_project` sessions (the conditional-row trick that hides a host's own mirrors), which is cosmetic: the launcher guard (3.3) is what refuses a dedicated open of one. |
| Placed (`--into`, unfiltered) | The project path is a policy layer over it. No daemon change; the launcher gains the checks in 3.2 and 3.3. |
| Filtered (`--window`) | Coexists for the dispatcher's remote workers. Never combined with a project session (3.2). |
| Window picker "Enter places the window into the current session" (#1106) | Unchanged. In a project session it places a window from an arbitrary remote session, a filtered mirror; acceptable, it is the user's explicit pick. |

Nothing is deprecated.

### 3.6 Windows open natively on a host, outside the leaf

On a host where the user also works directly, worktrees are open in that host's
own sessions (its hub or any other), invisible to this controller's leaf. In a
same-project-on-many-hosts workflow this is the common case, so v1 handles it
rather than leaving it to chance:

- The host snapshot (5.1) records, per worktree, **every window of any session
  on that host** showing it (`@git_root`/`@worktree` and the session name), not
  only windows of the leaf, and skips windows carrying `@bridge_win` (mirrors,
  already hidden by the picker probe).
- The worktree picker marks such a worktree `open on <host> (<session>)`, never
  `closed`. Enter does **not** create a second window on it: it mirrors that
  window with the existing #1106 path
  (`og-remote-open <host> <session> --into <slug> --window @N`, an id selector).
  That daemon is filtered on a remote session other than the leaf, so it does
  not overlap the leaf's unfiltered daemon (3.2 is about one remote session).
  The id selector is the only form the guard (3.3) lets through on a hub. The
  `@N` comes from a cache that can be a minute or more old (and older while a
  host is unreachable), and window ids start over when the remote tmux server
  restarts, so a stale `@N` could name a different window, a mirror among them.
  The launcher therefore verifies, in the same ssh call that starts the
  mirror, that window `@N` has an empty `@bridge_win` and that its pane's
  `pane_current_path` equals the snapshot's worktree path, and refuses
  otherwise. With that check the mirror loop cannot start from the picker; a
  mirror window id supplied by hand on the CLI is a documented residual.
- `^n` "create anyway" on such a worktree is the same-branch refusal of
  section 4.

## 4. Out-of-sync mains

Per-host clones drift, so nothing in the design trusts a host's local default
branch.

**New worktrees branch from a fresh remote default, on the target host.**
`og project new <branch> --host H` runs on `H` (through ssh):

1. Resolve the default branch **inside the remote script**:
   `d=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|^origin/||')`
   (the command prints `origin/main`, so the prefix is stripped), falling back
   to `main`. The forge controls that value, so before any use it must pass
   `git check-ref-format --branch` and `[A-Za-z0-9._/-]`, else the creation is
   refused; it is only ever expanded as `"$d"` inside the script, never
   interpolated into a command line built locally.
2. `git fetch origin "$d"` with a timeout. A failed or timed-out fetch
   **refuses** the creation and says which host and why. (An `--offline`
   override that prints the stale `origin/<default>` short sha it will use is a
   post-v1 option.)
3. Create the worktree from that ref, never from the host's local `main`:
   `wt switch -c <branch> --base "origin/$d"` (the `--base` flag is
   verified in plan phase P0; the fallback `git worktree add -b <branch>
   <path> origin/<default>` is acceptable only if `wt` is absent, and then
   the worktree path template cannot be honoured, so v1 requires `wt`; D5).
4. Only then open the window (section 2).

**Drift is shown, not hidden.** Each host snapshot records, for the default
branch, how far the host's local branch is behind `origin/<default>` as of its
last fetch, and `git ls-remote origin "$d"` (the same validated value) gives the true remote head
(one cheap network call per host per refresh, bounded). Rendering: a per-host
chip `main ↓3` (behind), `main ↑1` (local commits not on origin), or `main ≠`
(diverged); `main ✓` is not shown. A host whose `ls-remote` failed shows its
last value dimmed.

**The same branch on two hosts** is detected from the union of snapshots:

- *Creation*: `og project new` and the picker flow **refuse** when the branch
  exists as a worktree on another host (`feat/x is checked out on lab`), with
  `--force` to override (D4).
- *Existing duplicates* (made by a local `wt switch`, or before this feature):
  flagged in the worktree picker as `⚠ also on <host>`, and as `diverged` when
  the two heads differ. Never auto-resolved.
- A branch that exists on `origin` but on no host is opened on the target host
  by `wt switch <branch>` (tracking the remote branch), which is just "open".

## 5. Pickers

### 5.1 Data and latency

No picker blocks on ssh for a keystroke. The shapes are the #902 window probe's:

- **Snapshot cache**, one file per host,
  `$XDG_CACHE_HOME/tmux-og/projects/<hostFileName>.json`, under the same trust
  rules as the `remote-windows` cache (`readOwnerOnlyJSON`/`writeOwnerOnlyJSON`,
  owner-only, `host` must match, write-to-temp + rename, `savedAtStale`
  dimming). Unreachable keeps the cache and dims it.
- **First paint is cache-only; the probe is async.** The popup builds from the
  cache and kicks one probe per configured host off the Update goroutine,
  replacing the rows when it lands.
- **A `-B` monitor hook** (`@og-project-tick`, per the repo rule that
  background side effects run from `-B` hooks, never from `status-format`)
  refreshes snapshots at a slow cadence (60 s) and after any `og project`
  mutation, so the cache is usually fresh before a picker opens. It probes only **hosts of known
  projects** (registry hosts plus hosts whose last snapshot found a checkout),
  never every configured host, and is a no-op when no
  `programs.tmux-og.projects` entry and no `@og_project` session exists, so a
  user who never uses the feature pays nothing. Its fork and ssh cost is
  measured against `performance.md` in plan phase P2 before it ships.
- *Not v1*: shipping the worktree list through the bridge as a remote session
  stamp (as `@bridge_res` does) so a live mirror needs no ssh at all. The probe
  is the only source in v1; the stamp is a later optimisation (plan P4).

### 5.2 Session picker (`prefix + s`)

One row per project: the local project session, with a host strip after the
name: coloured chips from `picker/hostcolor`, each `local`, `lab` and so on,
with the live/idle/unreachable state and the drift chip. The preview card
(#942) already lists windows with mirror substitution; it gains a host column
for placed windows. A project's remote project sessions are **not** separate
rows; the Remote section keeps listing only non-project sessions (3.5).

### 5.3 Worktree picker (new)

A new picker mode, `--tui --worktrees`, listing the project's worktrees across
hosts: `branch`, host tag (coloured), `open`/`closed` (a window exists),
issue/PR badge where the snapshot or an open mirror window provides it
(`@bridge_pr_*`, `@bridge_issue_*`; no per-row remote call), drift and the
duplicate warning. Keys: Enter opens or focuses (section 2); `^n` new worktree;
Tab cycles the host scope like the other pickers. The key binding is owner
decision D6. The project is the current session's `@og_project`, or a choice
list when launched outside a project session.

### 5.4 New-worktree flow

`^n` asks for the branch name, then a host list (default local; each row shows
its drift chip and `unreachable` hosts disabled), then runs `og project new`
with the supervised, cancellable runner idiom (`attachRun`), phases `fetch`,
`create`, `open`. The host question is asked every time; there is no sticky
default in v1.

## 6. Layout

**Mixed local+remote panes in one window: out of v1, on purpose.** The bridge
mirrors a remote *window*: the remote's `%layout-change` is authoritative, the
local window holds renderer panes that exist because remote panes exist, and
size ownership (#979), reseeds, zoom, floats and predictive echo are all
per-remote-window. A local pane inside that window would have no remote
counterpart, so the daemon's registry, layout reconciliation, `kill-pane` and
reseed paths would each need a "foreign pane" case. That is a bridge
redesign, not a routing feature, and nothing else in this design needs it.

**What "smart layout" means in v1: session-level, not pane-level.**

1. *Host-grouped tabs* (post-v1 phase, opt-in, D7): windows of a hub are
   ordered local first, then hosts in `configuredHosts` order, each group in
   creation order, via a debounced `move-window` pass on
   `window-linked`/`window-unlinked` for sessions with
   `@og_project_group_hosts`. Per-host tab colours (#1102) already mark the
   groups; ordering makes them contiguous. The pass must stay inside the hook
   fork budget (`performance.md`, checked in that phase) and is off by default
   because it overrides manual tab order.
2. *Existing per-window layout is untouched*: reflow's status-bar grid and the
   mirror's dwindle (#1039) keep working per window.
3. *Not v1*: grouping tabs by branch across hosts, or a side-by-side tab-pair
   view of the same branch on two hosts (a wall-picker feature at best).

## 7. Failure modes

| Failure | Behaviour | New work |
|---|---|---|
| Host unreachable at open | The probe state (`classifyProbeErr`) is the answer: `og project open/new --host H` fails fast with the same message the picker shows; nothing is created on any host. The host chip is dimmed with its cache age. | Message plumbing only. |
| Host drops while mirrored | Existing reconnect (`Bridge Reconnect`): the daemon retries, then the refused-attach path kills its own windows after notifying a client of the session. Other hosts' windows are untouched (placed teardown kills own windows only). The chip reads `unreachable`. | None. |
| Create succeeds on the remote but the mirror fails | Order is: ensure pickup, then create, then open. If the daemon cannot attach, the launcher reports it and leaves the worktree (it is valid on the host); a re-run opens it. No rollback of a created worktree. | Ordering in `og project`. |
| Remote project session killed | The #934 rule ends the daemon and its windows. Processes in those windows are gone with the session (the remote's own persistence, tmux-remux, may restore them). The next open recreates the session and daemon. | None. |
| Remote tmux server restarted | `errServerReplaced`: an unfiltered placed daemon re-opens and re-evaluates, as today. | None. |
| Registry/probe disagree (path moved, checkout deleted) | The snapshot is rebuilt per probe; a stale declarative path gives `no checkout of <key> on H` rather than a guess. | Message. |
| Project session has windows of a host with no daemon (daemon crashed) | Dead renderers are kept by `remain-on-exit`; the next `og project open`/`attach` for that host starts a daemon that reuses the `@bridge_sock` and `sweepStrayOwnWindows` cleans the strays (existing). | None. |
| URL forwarding (`og-open`) with N daemons in one local session | Records live on each remote session, and a record tagged with its size owner (#1034) opens on exactly one daemon, so one tab per URL. Untagged records from an old remote keep today's per-machine claimer (#994). Each host's daemon reads only its own remote session, so hosts do not interfere. | None. |
| Size ownership with several daemons into one session (#979) | Independent per remote session. Activity counts only rows showing an *own* window, so typing in a lab window claims lab's owner record and nobody else's. Daemons into one local session compute the same `LocalArea` and do not fight. | None. |
| Two local machines both running the project | Each has its own local project session and daemon to the same remote project session: two controllers on one remote session, the existing #979 case (the remote follows whoever typed last). | None. |
| Remote older than this revision (no stamp-hiding rows in its probe) | The leaf shows in the Remote section; a dedicated open of it is refused by the launcher guard (3.3), which reads the stamps with the session-scoped read of 3.3. | None. |

## Security and untrusted input

Everything a host returns (branch names, paths, remotes, session names) is
untrusted. It is sanitised before it renders (`sanitizeStatusText`,
`remoteDisplayName`), never reaches a tmux format string or a shell word
unquoted, and branch names are validated by the section 2 rule (`git
check-ref-format --branch` plus the `[A-Za-z0-9._/-]` charset; the project key
written into `@og_project_leaf` is restricted to the same charset plus `@+:`)
before use as an argv element. Remote commands address windows and sessions by
id (`@N`, `%N`, `$N`) and never interpolate a name into a remote login shell
(fish-safe, per the existing kill scripts). The probe script has no single
quote or backslash, as `remoteZoxideScript` does. Registry paths are
absolute-checked. No credential or secret is read or written by any step.

## Owner decisions

| # | Decision | Options | Recommendation |
|---|---|---|---|
| D1 | Project registry source | Declarative only; discovery only; both | **Both**: discovery by default, optional `programs.tmux-og.projects` for hosts discovery cannot reach. |
| D2 | Windows already open natively on a host outside the leaf | Mirror on demand (3.6); also move them into the leaf | **Mirror on demand in v1** (no duplicate window, nothing moved). Adopting them into the leaf (`og project adopt`, `move-window`) is a post-v1 opt-in, since moving a window is user-visible. |
| D3 | Host for a new worktree | Always ask; default local; by capacity | **Default local, always ask in the picker.** No capacity heuristic in v1: nothing here measures load honestly. |
| D4 | Same branch on two hosts | Refuse, warn, allow | **Refuse creation** (override `--force`), **warn** on existing duplicates. |
| D5 | `wt` on the remote | Require; fall back to `git worktree add` | **Require.** The fallback cannot honour the host's worktrunk path template, so a worktree would land somewhere the host's `wt` does not expect. |
| D6 | Keybinding for the worktree picker | A new `prefix + <key>`; a mode of `prefix + w`; CLI only | **Decide against the live which-key table at plan phase P2.** A new mode keeps `prefix + w` meaning unchanged. |
| D7 | Host-grouped tab order | Default on; default off | **Off by default**, opt-in per hub (`@og_project_group_hosts`), a post-v1 phase: the ordering pass overrides manual tab order, a surprise to ship unasked. |
| D8 | Local project session name | `<repo>`; `<owner>-<repo>` | **`<repo>`**, falling back to `<owner>-<repo>` on collision. |
| D9 | A pre-existing local session of the same repo | Leave and create a new hub; adopt it | **Create a new hub in v1** (never touch an existing session unasked); adoption after confirmation is post-v1. |

## v1 acceptance (testable statements; the plan carries the per-phase criteria)

- `og project open <branch>` for a branch whose worktree lives only on host `H`
  creates a window on `H` under the leaf, and a mirror window with
  `@bridge_host=H` exists in the local `<slug>` session within the bounded wait
  and is selected.
- Running it twice, or from a second local session, leaves exactly one daemon
  and one window for the leaf.
- A dedicated or other-session open of a leaf, and a plain open of a hub, is
  refused by the launcher with the messages in 3.3.
- `og project new <branch> --host H` creates the worktree from the freshly
  fetched `origin/<default>` on `H` and refuses when the fetch fails or the
  branch is checked out on another host.
- A branch or path outside the safe charsets (section 2) is refused before any
  remote command runs.
- The worktree picker lists worktrees from all hosts from cache on first paint,
  marks natively-open windows `open on <host>`, and shows drift and duplicate
  flags; with a host unreachable it shows that host's cached rows dimmed.
- No existing mirror, picker or hook behaviour changes for a user who configures
  no project.

## What this design deliberately does not change

- The bridge daemon, its ctl verbs, size ownership and teardown (the launcher
  guards of 3.2 and 3.3 aside).
- The worktrunk hooks and `tmux-worktree-match`: local-only, and mirrors are
  invisible to them by the `@bridge_*` rule.
- Dedicated and filtered mirror behaviour.
