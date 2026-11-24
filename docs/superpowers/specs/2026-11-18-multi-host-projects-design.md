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
| Picker "open" on a worktree already open | none | Never open a second window. Identity is `(host, worktree path)`. Open in the project session (local, or a mirror: `@bridge_host` plus the window-scoped `@bridge_dir` for the worktree path; `@bridge_pane` is pane-scoped and holds the remote pane id that `placeFocus` matches, not a path): focus it. Open natively on the host outside the leaf: mirror that window (section 3.6). |

**Opening a window on a remote host** is ssh to the host (it rides the picker's
ControlMaster) running
`tmux new-window -P -F '#{window_id}|#{pane_id}' -t '=<leaf>:' -c
<worktree-path>` when the leaf exists. When it does not, the leaf's **first
window is the target window**: `new-session -d -P -F '#{window_id}|#{pane_id}'
-s <leaf> -c <worktree-path> \; set-option -t '<leaf>:' @og_project_leaf <key>` in one tmux invocation (the trailing colon forces a session lookup: a bare `-t` string is matched against window names first), so no stray anchor window exists and no instant has the leaf
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
  The launcher therefore verifies, in its pre-dial stamp read (3.3 item 1), that window `@N` has an empty `@bridge_win` and that its `@git_root` (the field the probe keyed the window on, not `pane_current_path`, which follows a `cd` into a subdirectory and differs across symlinked prefixes) equals the snapshot's checkout root, and refuses otherwise. The small gap between that read and the daemon's own selector resolution is accepted: the id selector then re-evaluates on the remote, and a window that changed in between is a mirror only if a user raced it by hand. With that check the mirror loop cannot start from the picker; a
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
| Create succeeds on the remote but the mirror fails | Order for `new` is: create the worktree, then open (below); for any open: ensure hub; leaf absent → create the leaf with the worktree as its first window, then start pickup; leaf present → start pickup, then `new-window`; then wait for the mirror window and select it. If the daemon cannot attach, the launcher reports it and leaves the worktree (it is valid on the host); a re-run opens it. No rollback of a created worktree. | Ordering in `og project`. |
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

## P0 findings

Plan phase P0, run on `halo` (tmux `next-3.9`, `wt 0.81.0`, Tailscale 1.104.6).
Every tmux repro ran on a scratch server (`-L`, own `TMUX_TMPDIR`,
`CLAUDE_STATUS_DIR`, `XDG_DATA_HOME`). Remote steps used the tailnet peer
`argon` (aarch64 NixOS). It has no tmux and no `wt`, so the repros that need a
remote tmux server or `wt` ran locally; the `argon` facts below are PATH and
shell facts only. `ssh localhost` was refused (unknown host key; not accepted,
to avoid editing `known_hosts`).

1. **`wt switch -c <branch> --base <ref>`** creates the branch from `<ref>`,
   not from the local default branch. Proof: a throwaway repo whose local
   `main` was one commit ahead of `refs/remotes/origin/main`;
   `wt switch -c feat/x --base origin/main --no-hooks --no-cd` made `feat/x`
   equal to `origin/main` (`617a785`), not `main` (`7a7dce5`). The path follows
   the host's `worktree-path` template: the user's global template placed it
   under `~/git/.worktrees/<repo>/<branch>`, and
   `WORKTRUNK_CONFIG_PATH=<file>` holding
   `worktree-path = "{{ repo_path }}/../wt-{{ branch | sanitize }}"` placed it
   at `../wt-feat-y`. **Confirms D5**: `--base` accepts a remote ref. Caveat:
   with no template configured the default is `../<repo>.<branch>`, so
   `og project new` must read the created path from `wt` (or
   `git worktree list --porcelain`), never compute it.
2. **`wt` on a non-interactive ssh PATH.** On `argon`,
   `ssh argon -- sh -c 'command -v wt; command -v zoxide; command -v tmux'`
   prints nothing. Its `$PATH` is `/run/wrappers/bin:~/.nix-profile/bin:/nix/profile/bin:…:/run/current-system/sw/bin`
   and `/etc/profiles/per-user/` does not exist, so `argon` simply lacks the
   tools (the expected `wt not found on argon`). Its login shell is **fish**:
   a bare `ssh host -- 'cmd'` runs under fish, so every remote script keeps the
   `remoteZoxideScript` rule (no quote or backslash). On `halo`, `wt` lives at
   `/etc/profiles/per-user/<user>/bin/wt` and is also on the PATH. **The
   per-user fallback is needed**, as for zoxide: `command -v wt`, then
   `/etc/profiles/per-user/$(id -un)/bin/wt`. A standalone home-manager host's
   `~/.nix-profile/bin` is already on the PATH.
3. **Stamp read and leaf creation.** In `picker/remoteopen/remote.go`,
   `firstRemoteSession` runs `list-sessions -F '#{session_name}' | head -1` and
   `hasSession` runs `has-session -t =<sess>`, both as
   `rm.ssh(rm.host, rm.env()+…)`. `projectStamps` slots in beside `hasSession`
   with the same shape. Verified on a scratch `next-3.9`:
   - `display-message -p -t '=leaf:' '#{session_id}'` prints `$1`.
   - `show-options -qv -t '$1' @og_project_leaf` prints the stamp when set, and
     an empty value with exit 0 when unset.
   - `show-options -v -t =leaf …` fails (`no such session: =leaf`), confirming #474.
   - `new-session -d -P -F '#{window_id}|#{pane_id}' -s leaf -c /tmp \; set-option -t 'leaf:' @og_project_leaf KEY1`
     prints `@1|%1` (the first window), and the stamp reads back.
   - **Confirms 3.3 item 1** (the `$N` requirement), and shows why it matters: for a **missing** session,
     `display-message -p -t '=nope:' '#{session_id}'` prints an **empty line
     with exit 0**, not an error. The launcher must require a `$N` answer and
     treat anything else as "cannot resolve", never trust the exit status.
   - On `next-3.9`, `show-options -qv -t '=leaf:'` (trailing colon) also
     resolves. Not relied on: older tmux differs and #474 stands, so the `$N`
     route stays.
4. **Mirror identity names** (read from the daemon code, not a live placed
   daemon: `picker/remotebridge/daemon/windowlabels.go:45,110`,
   `daemon.go:1131-1150,2681`, `picker/tui.go:1890-1906`). `@bridge_dir` is
   **window-scoped** and holds the remote's `@worktree`, falling back to
   `@git_root`: the remote worktree path, and the only mirror stamp usable as
   the `(host, worktree path)` identity. `@bridge_pane` is **pane-scoped** on
   the renderer pane and holds the remote **pane id** (`%N`).
   `@bridge_session_path` is **session-scoped on a dedicated mirror** (on a placed mirror
   `mirrorOpts.writeOwn` stamps it window-scoped on every window the daemon
   owns, `mirroropts.go:10-33`) and holds the remote session's
   `#{session_path}`, read once at bridge start: the session's creation dir,
   not a window's worktree. `placeFocus` keys on `(local session id, host,
   remote pane id, remote session name)` (`findPlacedWindow`), **not on a
   path**. **Spec change, applied** (section 2, the "Picker open on a worktree
   already open" row): path identity comes from `@bridge_dir`; `@bridge_pane`
   is the launcher's wait key for a window it just created.
5. **`git remote get-url`.** With
   `url."https://github.com/".insteadOf = git@github.com:` it prints
   `https://github.com/noamsto/tmux-og.git` (rewritten). With origin
   `git@gh-work:org/repo.git` and an ssh `Host gh-work` alias
   (`HostName github.com`) it prints the alias unchanged, while `ssh -G gh-work`
   resolves `hostname github.com`. **Confirms section 1**: the
   `remotes = [ … ]` alias layer is required.
6. **`picker/ogconfig/ogconfig.go`** is a fixed-field `Config` struct decoded
   from `$TMUX_OG_CONFIG` (process icons, splash tips); `Load` returns
   `ok=false` when the env is unset and callers keep built-in defaults.
   **Decision: a sibling reader** in `picker/project/registry.go` with its own
   env var and file (what P1a plans), not an extension of `Config`: the
   registry belongs to one feature, and growing `Config` would couple the
   picker and splash binaries to its shape.
7. **`og project` and the model.** In `picker/ogcli/models.go` a model is a
   kong struct plus a hand-written `help`, and `table.go:74` fails table load
   when a model verb also has subverbs. So `project` is declared as **one
   verb**, no `project x` entries (matches P1a). **Contradicts the plan:**
   `runVerb` (`run.go:74`) intercepts a **sole** `--help`/`-h` and never execs
   the target, so with no model `og project --help` prints only
   `og project — <summary>` and the target path, **not** the binary's usage.
   Every other argument is passed through raw (`og project status --help`
   reaches the binary). **Proposed change:** P1a adds a `project` model: the
   `ogVerbSpec` entry needs `model = "project"` (nothing else references a
   `models.go` entry), and the model struct keeps the passthrough positional
   (`Args []string` tagged `arg:"" optional:"" passthrough:""`), because
   `leafType` (`ogcli/model.go:77`) replaces `passthroughVerb` with it and
   completion would otherwise reject `og project status`. Or accept
   summary-only help. Recommended: the model, with a test that
   `og project --help` prints its help.
8. **Tailscale as an optional host source** (measured on `halo`, 5 peers).
   - **Call.** `tailscale status --json` talks to the local daemon only (no
     ssh, no network): 13 ms and ~3 KB for 5 peers. With `tailscaled` absent
     it fails **after 2.0 s** with exit 1
     (`failed to connect to local tailscaled`). `BackendState` is `Running`
     here; any other value (`NeedsLogin`, `Stopped`) means "no data". The call
     runs async under a short timeout (about 2 s) off the Update goroutine,
     like every probe in 5.1, and any failure means "no tailscale source",
     never a user-facing error. Never from `#()`/`status-format`.
   - **Fields.** Per peer: `DNSName` (`argon.<tailnet>.ts.net.`; its first
     label is the MagicDNS name ssh resolves, so **use this** as the host),
     `HostName` (the machine's own hostname, which can differ: `mbp-m4-pro`
     against MagicDNS `mbp`, and an iPhone reports `localhost`), `Online`,
     `OS` (`linux`, `macOS`, `iOS`), `Tags` (null here; tagged nodes list
     `tag:…`), `ShareeNode` (shared-in nodes; false here) and `ExitNode`. The
     candidate filter is `OS` in {`linux`, `macOS`} (this excludes `iOS`,
     `android`; `windows` only if configured) and `ShareeNode` false. `Self`
     is not in `Peer`.
   - **`Online` is not "ssh works".** `a check-mode peer` is `Online: true`, yet
     `ssh -o BatchMode=yes -o ConnectTimeout=5 a check-mode peer true` printed
     `# Tailscale SSH requires an additional check.` with a login URL and hung
     until `timeout 12` killed it (exit 124): `check` mode ignores
     `ConnectTimeout`. This is the #486 failure
     (`docs/superpowers/plans/2026-09-03-picker-tailscale-check.md`,
     `remoteProbeTailscaleCheck`). So `Online=false` can only **skip** a host
     (do not dial; dim the cached row); `Online=true` never proves
     reachability, and the existing probe timeout and `classifyProbeErr` stay
     the authority.
   - **Recommendation (proposed spec change; optional, opt-in).** A tailscale
     host source, off unless enabled, that (a) lists peers missing from
     `@remote_bridge_hosts` as suggestions the user adds by hand, never
     auto-added, and (b) supplies an offline signal. It slots into **P1a** (a
     `picker/project/tailscale.go` reader returning `[]Peer{Name, Online, OS}`
     with the timeout above, a pure parser tested on a captured JSON fixture)
     and **P2a** (the tick and the host strip skip and dim `Online=false`
     hosts; a host absent from the tailnet is unaffected). The opt-in is a
     named switch, `programs.tmux-og.projects.tailscale.enable` (default
     `false`), and with it off no `tailscale` process is ever executed.
     Entries of `@remote_bridge_hosts` are ssh aliases, so peers are matched to
     them by `ssh -G <alias>` `hostname` (first label), not by alias string;
     an alias that does not resolve to a peer is simply not subject to the
     offline signal, and a peer reachable only under an alias may be suggested
     again. tmux-og is public:
     the configured list is always the fallback and the only source when
     Tailscale is unavailable, and no code path requires Tailscale.

## Host views (post-v1)

A design spike (#1244), docs only: it changes no shipped code. A project's hub
session gains three things: a **host bar** in its status line (one chip per
host, the active one marked, click or a prefix key to switch), a **filtered
view** (the hub shows only the marked hosts' windows), and an **omni window**
(one tab whose shell always runs on the active host, in the worktree of the
same branch there). The spike checks the one risky assumption: that a placed
mirror window survives being taken out of the hub and put back.

### Mechanism

- **Views are window links.** Each host's mirror windows stay alive. The hub
  shows a window only while it is linked into the hub; a hidden window sits in a
  **holding session** (`og-hold-<slug>`, stamped `@og_project_hold=<key>`).
  `next-window`, `prefix + n/p`, window numbers and the pickers skip hidden
  windows natively, which a status-only filter cannot do.
- **Omni window.** One shell window per host (the local shell; a placed mirror
  of a remote window at the project's checkout on that host). Only the active
  host's slot is linked into the hub; switching host replaces it in place, then
  moves the new slot to the worktree of the same branch (below).
- **Host bar.** `@og_active_host` and `@og_view_hosts` (the marked set) are
  hub session options; `@og_hostbar` is the rendered chip string.

### Spike method

Scratch servers only. Each case ran on a fresh pair: a "remote" tmux
(`-L m2src`) and a "local" one (`-L m2dst`), each with its own `TMUX_TMPDIR`,
`CLAUDE_STATUS_DIR` and `XDG_DATA_HOME`, joined by the daemon's `--test-local`
seam, exactly the harness of `tests/remote-m2-integration.bats` (no ssh: the
findings concern the daemon, which the launcher only starts). Host `halo`,
tmux `next-3.9`, daemon at `51b29973`. The local server had the harness's
`DST_CONF` (`base-index 1`, `renumber-windows on`, `remain-on-exit on`, the
nudge hooks). The probe scripts are not in the repo. The daemon was started
the way `bridge_up_placed` does it:

```bash
daemon --test-local --src-socket m2src --dst-socket m2dst --session rem --window 1 \
  --local-sess host --into '$N' --host lab --renderer <renderer> --sock <sock>
```

Three variants were measured. In every one the remote session `rem` has two
windows and the subject is the mirror of `rem:1`:

| Variant | Daemon placed into | Hide | Show |
|---|---|---|---|
| **A** unlink | the hub (`host`) | `tmux -L m2dst move-window -d -s @2 -t hold:` | `move-window -d -s @2 -t host:` |
| **B** view link | the holding session (`hold`) | `unlink-window -t host:@2` (the window also stays in `hold`) | `link-window -d -s @2 -t host:` |
| **C** unlink, stamp-scoped ownership | the hub | as A | as A |

C is A with a scratch-patched daemon (a copy of `picker/` outside the repo; the
patch is three queries, see "What the daemon asks"). B and C were measured to
see whether "a window linked into two sessions" or a small daemon change is the
safer route.

**States.** *Unlinked*: hide, run the event at once, observe for 4 s or more
while echoing on the remote every 1.2 s (the daemon's maintenance sweep runs
off control-stream lines). *Relinked*: hide, wait 0.3 s, show, then run the
event. B also has *linked* (never hidden).

**Events** (all on the scratch servers):

- reshape: `tmux -L m2src split-window -d -t rem:1`
- reconnect: `kill -9` the daemon's `attach-session` child (the transport)
- reseed: blank the mirror pane (`send-keys -R -t <pane>`), then force an
  excursion, `tmux -L m2src switch-client -c <control client> -t other`; the
  pin-back reseeds, and the pane must show the remote marker again
- remote window close: `kill-window -t rem:1`
- remote window add: `new-window -d -t rem:`
- client resize: a pty client on the hub (`tmux -L m2obs new-session … 'tmux -L m2dst attach -t host'`),
  then `tmux -L m2obs resize-window -t obs -x 140 -y 50`
- teardown: `kill -TERM <daemon>`; and `kill-session -t rem` (the #934 path)
- refused attach: `OG_DAEMON_RETRY_MAX_ELAPSED=2s OG_DAEMON_WAKE_MAX_ELAPSED=2s OG_DAEMON_RESTORE_MAX_ELAPSED=6s`,
  the transport killed, the remote socket moved aside, a remote server without
  `rem` started, then `send-keys` into the mirror pane until the log says
  `refused the attach`

### Findings: variant A (what the spike asked)

"Own windows" means windows in any session (`list-windows -a`) whose
`@bridge_sock` equals the daemon's socket.

| Case | Unlinked (A) | Relinked after 0.3 s (A) | Deciding code |
|---|---|---|---|
| Hide and show, nothing else (`move-window` out and back) | The daemon **kills the window and builds a new one in the hub**: `@2` was gone and `@4` appended at the hub's end after 1.0 s and 4.1 s (two idle runs), at the first control-stream line after the 1 s floor; relinking `@2` then fails `can't find window: @2`. Mirror content keeps painting in the replacement. | Window kept, no duplicate, paints. | `windowSweeper.sweep` (`reconcilewindows.go:309`, floor `windowSweepInterval` `:274`, called per stream line, `daemon.go:1722`) reads `mirrorPaneRows` (`:158`, `list-panes -s -t <hub>`); a window the hub lacks is "lost", `healLostWindows` (`:123`) → `retireMirror` (`daemon.go:2267`) → `closeWindow` (`:2240`, `kill-window -t @2`, which removes the window from every session) → `reconcileWindows` → `createMirrorWindow` (`:293`, `new-window -a -t <hub>:{end}`). The same one-session view is `localWindowSet` (`localpanes.go:82`). |
| Remote reshape (`split-window`) | Replaced as above; the rebuilt window shows both panes (2 panes, built from the remote's current layout). | Layout followed: 2 panes, window kept. | `%layout-change` routing is by registry entry, which the retire removed. |
| Reconnect (transport killed) | Transport redialled (new `attach-session` child); the window was replaced as in row 1. | Redialled; window kept, paints. | `reattach` (`conn.go:390`); the retire as above. |
| Reseed (blank pane, excursion) | Not reachable: the window was replaced first (its replacement is seeded fresh). | Marker back in the pane (2 occurrences; 0 right after the blank). | `reseedWindow` (`daemon.go:2498`) and the pin-back act on registry entries, not sessions. |
| Remote window close | Window removed everywhere, **no leak**: the registry holds `@2`, and `kill-window -t @2` finds it in the holding session. | Removed. | `closeWindow` (`daemon.go:2240`). |
| Remote window add | New window lands in the **hub**, not the holding session: the filtered view would show a hidden host's new window (`hub=[@3,@4,@5]`). | Same (`hub=[@2,@3,@4]`). | `createMirrorWindow` (`daemon.go:293`) always targets `cfg.target()`. |
| Client resize (120x39 → 140x49 client) | The window had been replaced (as in row 1); the remote followed to 140x47. | Mirror and remote both followed within 1 s. | `localArea` (`cmd/daemon/main.go:593`) reads the hub's clients; the hub's `client-resized` hook touches the hub's `@bridge_nudge`. |
| Teardown, SIGTERM and `kill-session -t rem` | Hub windows killed; **`@2` survives in the holding session** as a dead-renderer corpse (`remain-on-exit`), an orphan nothing will kill. | All killed. | `killOwnWindows` (`placedteardown.go:22`) lists the hub only through `ownWindows` (`placed.go:66`, `hostWindows` `:52`, `list-windows -t <hub>`). |
| Refused attach | The daemon parks (`lab unreachable; parked until the mirror is focused or typed into`) and, with its window replaced and nothing viewable, never reaches `refused the attach`. | Woke on input, `refused the attach` ×6, `closeOwnWindows` killed the hub windows, daemon ended. | `localViewing` (`park.go:131`) counts only a hub client viewing an own window; a hidden window cannot be viewed. `closeOwnWindows` (`placedteardown.go:39`). |

**Reading.** With the shipped daemon a hidden placed mirror is unsafe in A:
the daemon asks "is my window still alive?" and "which windows are mine?" of one
session, so the next sweep (1 to 4 s) kills the hidden window and recreates it
in the hub, and a teardown leaks it. A fast relink (under about a second) is a
race, not a mechanism.

### Findings: variants B and C

| Case | B: daemon in the holding session, view link | C: A with stamp-scoped ownership |
|---|---|---|
| Hide and show | Linked, unlinked and relinked all keep `@2` in `hold`; no replacement, no duplicate. | Hidden 4 s+ and relinked: no replacement; `@2` kept in `hold`, then back in the hub. |
| Reshape | `hold`'s `@2` grew to 2 panes while unlinked from the hub, and after relink. | Same, hidden and relinked. |
| Reconnect | Transport redialled; windows and link kept; paints. | Same. |
| Reseed | Marker restored in linked, unlinked and relinked. | Marker restored while hidden and relinked. |
| Remote window close | `@2` gone from `hold` and the hub (`kill-window` removes every link): no stale view link. | `@2` gone from `hold`. |
| Remote window add | Lands in `hold` (hidden). A `window-linked` hook fires there with `@bridge_host` and `@bridge_sock` set but `@bridge_win` still empty, so a hook can link a marked host's window into the hub by `@bridge_host`. | Lands in the hub (visible). A `window-linked` hook moved it to `hold` (`run-shell -C "move-window -d -s #{hook_window} -t hold:"` behind an `if -F` on `@bridge_host` and the hub's session name): `hold=[@2,@4]` while hidden, `[@4]` after relink. |
| Client resize | **No follow for 27 s**: the window stayed 100x30 and the remote 100x30 until the 30 s fallback poll; the measurement at 3 s still read 100x30. | Followed in 1 s while hidden and while shown (140x47). |
| Teardown, SIGTERM and `kill-session -t rem` | All windows killed (the daemon's own session is `hold`). | All killed, hidden or not. |
| Refused attach | Woke on input; `refused the attach`; all windows gone. | Same; hidden window included. |

Why B fails resize: `readClients` and `localArea` read the clients of the
daemon's own session (`cmd/daemon/main.go:707`, `-t <target>`), and the daemon
stamps `@bridge_nudge` on that session (`registerResizeNudge`, `daemon.go:588`).
A hub client's `client-resized` hook evaluates `#{@bridge_nudge}` in the hub, so
it is unset and nothing nudges. Five more queries share the assumption (read in
code, not measured): `ResolveLocalViewIdentity` (`viewident.go:147`),
`clientViewsArgs` (`reveal.go:56`), `localViewing` (`park.go:131`),
`paste.go:803` and `pasteprogress.go:138`.

Also in B: a window moved **out of the daemon's session** is the same loss as
in A. Measured: with the daemon in `hold`, `move-window -s @4 -t host:` made it
kill `@4` and recreate `@5` in `hold`. In B a window may only be linked, never
moved.

### What the daemon asks (the symbols that decide the outcome)

Placed ownership is by `@bridge_sock` (`ownWindows`, `placed.go:66`), but every
*liveness and listing* question is scoped to the hub (`cfg.target()`):
`hostWindows` (`placed.go:52`), `localWindowSet` (`localpanes.go:82`),
`mirrorPaneRows` (`reconcilewindows.go:158`), `createMirrorWindow`
(`daemon.go:293`), and, for panes, `localPaneMap` (`cmd/daemon/main.go:631`) and
`paste.go:764`. The session pin itself (`pinLocalSession`, `ownership.go:24`;
`ownsLocalSession`, `reopen.go:119`) is by session id and is unaffected.

The scratch patch behind variant C is the first three of those, each changed
from "windows of the hub" to "windows (`-a`) whose `@bridge_sock` is mine":
`list-windows -a -F '#{window_id}|#{@bridge_sock}'` in `localWindowSet`,
`list-panes -a -F …|#{@bridge_sock}` in `mirrorPaneRows`, and `list-windows -a`
in `hostWindows`. Because the patch changed `hostWindows` itself, every caller
moved with it: `ownWindows` (so `killOwnWindows`, `sweepStrayOwnWindows`,
`rebuildPlaceholder`) and `unregisterPlacedNudge` (`mirroropts.go:116`). Client
queries stay on the hub, which is where the clients are. It passed 22 runs (11
events × hidden and relinked; the table above groups SIGTERM and
`kill-session`). Not measured and not patched: `localPaneMap` and
`paste.go:764` (a hidden window's agent status is not shipped to local options
until it is shown again, and a paste into a hidden window cannot happen). For
P5 `hostWindows` splits in two: an `-a` own-window listing for the callers
above, and the hub-scoped listing kept for `unregisterPlacedNudge`, since a
stale `@bridge_nudge` is the safe error. The daemon change is therefore **three
measured queries (`hostWindows` split, `localWindowSet`, `mirrorPaneRows`) plus
two pane maps (`localPaneMap`, `paste.go:764`) that P5 changes unmeasured**.

### Recommended mechanism

**C: the hub stays the daemon's target; hide is `move-window` into a holding
session; ownership becomes stamp-scoped (three measured queries, two pane maps).** B needs the
daemon to learn about clients of other sessions in six places and a nudge on a
second session; A is unsafe. C changes only "which windows are mine", which the
daemon already decides by stamp. The v1 claim "the bridge daemon does not
change" is untouched: this is post-v1, and the change is P5's first step.

- **Hide a host**: one tmux invocation,
  `move-window -d -s @a -t <holding id>: ; move-window -d -s @b -t …`,
  for every window of that host (read from a `list-windows -a` scan:
  `@bridge_host` equal to the host, `@og_omni` empty). The omni slot is
  excluded: it is linked in both the hub and the holding session, and
  `move-window -s @N -t <holding>:` on such a window resolved the holding
  session's own link and left the hub link in place (rc 0, nothing hidden), so
  an omni slot is only ever swapped, never hidden. The local chip's windows are
  the hub windows with no `@bridge_host` and no `@og_omni`. **Show** is the
  same with `-t <hub id>:` over the windows in the holding session.
  `move-window` with no index takes the lowest free index, filling gaps; D7's
  ordering pass restores order when it is on.
- **A new window of a hidden host** lands in the hub (measured) and a
  `window-linked` hook moves it out. The hook is gated by a zero-fork `if -F` on
  the hub's `@og_project` stamp, the window's `@bridge_host`, and the host
  **not being in** `@og_view_hosts` (the gate pattern of `performance.md`). The
  last condition matters: show is itself a `move-window` into the hub, which
  fires `window-linked`, and a gate without it moves the window straight back
  out (the C prototype's hook lacked it: after the relink `@4` was still
  moved to `hold`).
- **Holding session** is created on the first hide, never at project open
  (nothing starts eagerly), stamped `@og_project_hold`, hidden from the session
  and window pickers with the `@og_project` conditional-row idiom, and excluded
  from persistence (`persist.md`; verify in P5).

### Fallback if unlink proves unsafe

If the daemon change is refused, or P5's own tests find C unsafe on some
host/tmux combination: a **status-only filter**. Hidden windows stay in the hub
and carry a window option `@og_view_hidden` (stamped by the same
`window-linked` hook); the grid and tab formats skip them. This needs no daemon
change but leaks: `prefix + n/p/l`, `select-window -t :N`, `last-window`,
`M-1…M-7`, the window picker and wall, and window numbers all still see hidden
windows. A shipped fallback must at least rebind `n`/`p` to skip hidden
windows and mark them in the pickers. It is the fallback, not the plan.

### Host bar and status-line clicks

Measured on the wrapped build (`nix build .#default`, tmux-og's real config,
`next-3.9`) on a scratch server (`-L clickp`, a pty client from a second
server `-L clickobs`, SGR mouse reports written with
`tmux -L clickobs send-keys -t obs -l $'\e[<0;<col>;<row>M'` then `m`, the
technique of `tests/remote-m2-integration.bats`' `mouse_at`). The config was not
modified; the prototype was `source-file`'d and set onto the scratch server:

```tmux
set -g @og_hostbar "#[range=user|h:local] local #[norange]#[range=user|h:lab] lab #[norange]#[range=user|h:argon] argon #[norange]"
set -g status-format[0] "#{E:@og_hostbar}<the shipped format>"
bind -T root MouseDown1Status if -F '#{m:h:*,#{mouse_status_range}}' \
  "run-shell 'echo host=#{q:mouse_status_range} >> clicks'" 'switch-client -t ='
```

The clicks were made with these `h:<name>` keys (a 26-character name was a
fourth chip). The `ogh:<index>` key form recommended below is the same
mechanism with a prefix that cannot collide with `window`, `left` or
`session`; it was exercised for the option-only update row, not for clicks.

| Check | Result |
|---|---|
| A user range in the status format plus `MouseDown1Status` reading `#{mouse_status_range}` | Works. A click on a chip delivers `mouse_status_range` = the user name (`h:lab`; `host=h:argon` through the dispatch bind), `mouse_status_line` = the line (0), `mouse_x`. |
| User range names | Truncated to 15 bytes: `h:abcdefghijklmnopqrstuvwxyz` arrived as `h:abcdefghijklm`. Use short keys (`ogh:<index>`), never the host name. |
| Other ranges | A tab click reports `window`; the else branch `switch-client -t =` (the stock `MouseDown1Status` command, `list-keys -T root`) still selected window 3. The session name's `range=left` still fired `MouseDown1StatusLeft`. A chip click fires only `MouseDown1Status`. |
| Replacing the stock bind | The first test bind, with no else branch, broke tab clicks (window stayed 1). The dispatch bind must keep `switch-client -t =` as its fallthrough. |
| Composition with the reflowed grid | With 15 windows (`status` 4, the multi-line grid) reflow holds **session-level copies of `status-format[0..4]`**. A global `status-format[0]` edit after that did **not** reach the session: line 0 had no chips until a structural reflow (a new window) re-copied it. A session option change (`@og_active_host` lab → local, the chip text flipping between `lab` and `[lab]`) updated line 0 with no reflow and no format edit. |
| Where the chips can live | Line 0 only. Lines 1 to 4 belong to reflow's grid and are overwritten on each full pass. |
| Fold | Not measured. The folded line is its own session-level `status-format[0]` (`status-bar.md`), so it needs `#{E:@og_hostbar}` too or the chips vanish while folded. |
| Right-click on a chip | Not measured (the stock `MouseDown3Status` window menu on a user range). Checked in P5. |

So: P5 puts `#{E:@og_hostbar}` into the shipped global `status-format[0]` (and
the folded line) **once**, so reflow's copies carry the indirection; after that
every toggle is a session-option write, no reflow and no format edit. A chip
click handler is `og-project-bin view <index>`; `prefix + <key>` is the same
verb with `cycle`. Chip text is stamped by the P5 binary from the snapshot
cache (live/idle/unreachable, drift); host names pass the existing
`^[A-Za-z0-9][A-Za-z0-9._@-]*$` check, so `#{E:}` never expands remote text.

### Omni window

Slots are plain windows stamped `@og_omni=<host>` in the holding session; the
hub holds exactly one at a time. Measured on a scratch server (`-f /dev/null`,
`renumber-windows on`), with slots `A`, `B`, `C` in `hold` and a hub of five:

- `link-window -d -s @B -t hub:3` on an occupied index fails (`index in use: 3`,
  rc 1). `link-window -b` inserts and shifts later windows.
- **`link-window -d -k -s @B -t hub:3` replaces the occupant in one command**:
  the occupant (`slotLocal`) stayed alive in `hold` because it is linked there;
  later windows did not move. This is the switch. `-k` is safe only because
  every slot is also linked in `hold`: `move-window -k` over an ordinary hub
  window (linked nowhere else) **destroyed it** (`tab2` was killed).
- `unlink-window` leaves a gap (hub indices 2, 4) with `renumber-windows on`;
  compaction happens on `kill-window`. A slot's index is therefore stable only
  until some window dies, so the slot is found by `@og_omni`, not by a fixed
  index.

A remote slot is a placed mirror, so under C it is an ordinary own window of
the hub's daemon and the omni switch is a `link-window -k` of two windows the
daemon owns by stamp; the same stamp-scoped ownership covers it. (The omni
switch itself was measured with plain windows; mirror windows were measured for
hide and show above.)

**Branch-follow rule.** On a switch to host H, with branch B the hub's current
omni branch (`@og_omni_branch`, from the active slot's worktree), using H's
cached snapshot (P1a/P2a):

| Case | Behaviour |
|---|---|
| B has a worktree on H | Link the slot, then, only when its pane is at a shell prompt (`pane_current_command` is a shell and no foreground job; never type into a running program), send `cd -- <path>` and Enter. The path is the snapshot's, validated against the spec 2 charset, so no quoting is needed in fish or sh. The slot's own `@og_omni_branch` updates when its `@bridge_dir` follows. |
| B has no worktree on H | Link the slot unmoved (it keeps its last cwd, or the checkout root for a fresh slot), set the status message `no <B> on <H>`, never create a worktree (a v1 non-goal). `og project new <B> --host H` is the way to make one. |
| H unreachable (snapshot `unreachable`, or no snapshot) | Do not switch: the chip is dimmed with its cache age, a key press prints `<H> unreachable (cached Ns ago)`, and the active host stays. A host that drops while its slot is shown shows the daemon's existing reconnect state. |

A fresh remote slot is created on first switch with `og project open`'s leaf
path (`new-window -P -F … -c <worktree>` on the leaf), so a slot's first window
already starts in the right directory.

### What P1b must expose

1. **Keep the leaf daemon `--into <hub id>`.** C depends on it; B would not.
   P1b's plan already has it.
2. **`ensureHostVisible(host)` as a seam in `og project open`.** After pickup
   the wait-and-`select-window` step must make the host visible first, or the
   select races the hide hook. A no-op until P5.
3. **The local scan stays all-sessions** (`localscan.go`, `list-panes -a`), so
   a hidden window in the holding session is still found for the "reuse a live
   daemon" step. Add to P1b step 5a (plan): a `localscan.go` unit test over a
   fake `list-panes -a` reply that holds a stamped mirror pane in a non-hub
   session. (Not a bats case with a real daemon: the shipped daemon rebuilds a
   moved window within 1 to 4 s, so such a test could pass on the replacement.)
4. **From P1a/P2a:** per-host state (`live`/`idle`/`unreachable`, cache age)
   and `Snapshot.Worktrees[].Branch/Path`, as an exported function, not a
   second probe; the host-name validation, shared with the chips.
5. **Hub id pair**: `@og_hold_id` (the holding session's `$N`), written when the
   holding session is created.

### Fork budget per toggle

`strace -f -e trace=execve` on a scratch server of the wrapped build with a pty
client attached (`-L forkp`, the hub and a holding session, 8 spare windows;
the idle baseline was 116 to 143 execs per 3 s from the status pollers, so only
hook-driven scripts are counted):

| Operation (3 runs) | `tmux-reflow-windows` forks |
|---|---|
| `link-window` ×1, and ×3 in one invocation | 0, 0, 0 |
| `unlink-window` ×1 | 1, 0, 1 |
| `unlink-window` ×3 in one invocation | 3, 1, 3 |
| `move-window` hold → hub / hub → hold | 2 / 1 in every run |
| `link-window -k` omni swap (occupant also linked in `hold`) | 1 in every run |

The `window-unlinked` hook is `run-shell -b … tmux-reflow-windows`, with no
`--debounce`, so the bound is one reflow per unlinked window (the runs that
show fewer did not spawn the job; the cause was not chased). A full reflow pass is about N+11 tmux execs on a multi-line grid,
53 to 73 ms at 5 windows (`performance.md`, #1110). `link-window` fires no
reflow, so the hub's `@reflow_key` stays at the old window count (measured
`2:200:49` after the third window was linked; the tab itself rendered, the grid
stayed stale). Budget: a host toggle with k windows is **one** `move-window`
invocation plus **one** forced reflow (`tmux-reflow-windows --force <hub>`), not
k passes. To get that, `window-unlinked` must skip its reflow while a batch flag
(`@og_view_batch`, set by the toggle on the hub) is set, checked in a zero-fork
`if -F`, and must skip it for sessions stamped `@og_project_hold` (a toggle
shows windows by unlinking them from the holding session, which nobody views:
`move-window` hold → hub measured 2 forks, hub → hold 1). `--debounce` is not
enough: it still forks one bash per event (`scripts/tmux-reflow-windows.sh`,
token write plus a short sleep) and coalesces passes, not forks. The toggle then
clears the flag and runs one `tmux-reflow-windows --force <hub>`. P5 measures
the result against this table.

### Interaction with D7

A filtered view does **not** replace host-grouped tab ordering: the view picks
*which* windows are shown, D7 picks *in what order*, and they compose (D7's
pass runs on `window-linked`/`window-unlinked`, which a toggle fires; its
`move-window` operates on the linked windows only, so hidden ones are not
reordered). A relinked window takes the lowest free index, so with D7 off a host
comes back at a gap or the end; that is the reason to turn D7 on together with a
non-default view. D7 stays off by default and independent.

### Owner decisions (host views)

| # | Decision | Options | Recommendation |
|---|---|---|---|
| D10 | Daemon change to make hide safe | Stamp-scoped ownership (C, three measured queries plus two pane maps); view-link into a holding daemon (B, about six queries plus a second nudge); status-only filter (fallback) | **C.** Measured safe 22/22; the smallest change; B fails resize (27 s). Keep the status filter as the fallback only. |
| D11 | Prefix key to cycle the active host | `V`, `e`, `u` | **`V`** ("views"). Free in the shipped prefix table: `list-keys -T prefix` on the wrapped build leaves `e j K P Q R u U v V X` and `" % + ^` unbound (`h`/`H` are taken). Also register it in the which-key table at P5 (D6 rule). |
| D12 | Host slots and omni slots: eager or on demand | Eager at project open; on demand | **On demand.** The holding session at the first hide, an omni slot at the first switch to its host. Eager means a remote shell and a mirror per host at project open, against "nothing starts at server boot". |
| D13 | Is `local` a chip | Yes; no | **Yes**, the first chip: it is the omni slot of the local shell and the filter for local windows (the hub windows with no `@bridge_host` and no `@og_omni`). |
| D14 | Default filter | All hosts marked; active host only | **All hosts marked.** A project opens as v1 shows it; the filter is something the user turns on. The active host drives only the omni window. |
