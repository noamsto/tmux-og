# Local prdash for mirrored windows — design

Issue: #1236. Tier: deep.

## Problem

In a mirror window (`@bridge_win` + `@bridge_pane`), `prefix + p` sends the ctl
`tool prdash <@bridge_dir>` verb: the REMOTE opens a prdash float and the mirror
streams it back. prdash only reads GitHub, so the stream adds latency and
requires prdash on the remote. Only worktree actions (`enter`, `W`) touch the
checkout, and only those need to run remotely.

prdash (PR #215, merge `606b15f`, not yet in our `flake.lock`, which pins
`014241e`) now supports:

- `prdash --repo owner/name`: the named repo's board from any cwd. With `--repo`
  and a cwd that is not a checkout of it, every row's checkout is looked up at
  `$PRDASH_REPOS_ROOT/<owner>/<repo>`.
- `PRDASH_ACTION_FILE=<path>`: worktree switches append one line per action and
  prdash quits. A line is `<key>\t<json>`; `[...]` is an argv carrying a LOCAL
  path, `{...}` is a path-free entry written only with `--repo` and no local
  checkout of the row's repo:
  `{"repo":"o/n","kind":"pr|issue","target":"…","pr":7,"args":["switch",…],"key":"enter"}`.
  `args` is the `wt` argv minus `wt`/`-C dir`; `target` is its last element and
  is `pr:<N>` for a fork PR or a head ref outside `^[A-Za-z0-9._/@+-]+$`.
  Only worktree-switch actions produce entries (`X`, branch cleanup, does not).

## What is shipped today (checked)

The window label row (`picker/remotebridge/daemon/windowlabels.go`,
`windowLabelFormat`, 24 fields) ships `@bridge_dir` (remote
`@worktree || @git_root`), `@bridge_branch`, `@bridge_pr_url` and others. **No
repo identity (`owner/name`) is shipped**; `@bridge_pr_url` names it only when
the window has a PR. No remote tmux-og script stamps an origin/slug option.

## Requirements (from the task)

R1. In a mirror window, `prefix + p` opens a LOCAL prdash float with
`--repo <remote owner/name>` and `PRDASH_ACTION_FILE` set to a private temp file.
R2. When prdash exits, each hand-off entry is forwarded to the remote and run
there in the pressed pane's checkout (`wt switch <branch>` / `wt switch pr:N`),
so the remote window switches as today. Another repo's PR resolves its checkout
on the remote at `${PRDASH_REPOS_ROOT:-$HOME/git}/<owner>/<repo>`, origin must
match. Hand-off content is untrusted: validate shapes, pass as argv, never
interpolate into a shell string.
R3. Fallback when the remote repo cannot be resolved: keep today's remote prdash.
R4. Non-mirror windows unchanged.
R5. Docs: `bridge-daemon.md` tool-binds row, `bridge-shipped-state.md`,
`scripts.md`, README key text if any.

Out of scope: other floats, the enrich card, any cache daemon, `X` (it never
yields an entry).

## Design

### D1. Repo identity: a shipped `@bridge_repo` window option

Chosen over a ctl query verb. ctl is fire-and-forget by construction: the daemon
acks once it has queued the remote command and "never waits on the network"
(`cmd/ctl/main.go` header), the ack frame carries only an error string, and only
the main loop may round-trip to the remote. A query verb would need a new reply
frame, a handler→main-loop request channel, and a press that blocks on ssh
latency. A shipped option makes the bind decision a pure `if-shell -F` (no fork
on press, no wait), matches how every other remote fact reaches a mirror
(`@bridge_*`), and an older daemon simply never stamps it, which selects the
fallback.

Mechanism, needing only `sh`, `git` and `tmux` on the remote (no remote tmux-og
rebuild):

1. **Probe.** When the label shipper (main loop, owns `rt`) sees a row whose
   `dir` is non-empty and not covered by a fresh origin stamp (below), and it has
   not already probed that `(remote window id, dir)` pair this daemon lifetime,
   it sends one
   `run-shell -b -t <@N> 'exec /bin/sh -c <script>'` to the remote. The script
   reads the window's own `@worktree`/`@git_root` (the same `worktree||git_root`
   the label row resolves), runs `git -C "$d" remote get-url origin` (prdash's
   own call, so `insteadOf` rewrites apply), strips the scheme (`${u#*://}`)
   and any userinfo (`case $u in *@*) u=${u#*@};; esac`) so a credential in an
   https origin never leaves git, and stamps ONE remote window option in one
   `set-option`: `@og_bridge_origin "<host-and-path> <dir>"` (atomic: the
   origin and the dir it was read for land together). The window id embedded in the script is validated
   `^@[0-9]+$`; no remote-derived value is embedded. Body follows the existing
   remote-script rules: zero single quotes, `#`-free except the `${p#*=}` PATH
   restore idiom (run-shell format-expands).
2. **Ship.** `windowLabelFormat` gains `#{s/[|]/ /:@og_bridge_origin}` placed
   before `@window_label_id` (the free-form `@window_label_rest_long` stays
   last); `windowLabelFields` 24 → 25. The parser cleans it with
   `cleanLabelValueExact` (cap `urlMaxRunes + 1 + dirMaxRunes`), splits at the
   first space into origin and stamped dir, and derives
   `repo = githubSlug(origin)` only when stamped dir == the row's `dir`; otherwise
   `repo = ""` and the stamp is stale (triggers a re-probe for the new dir).
3. **Stamp.** `bridgeLabelOptions` gains `@bridge_repo` ← `repo`. Empty unsets.
4. **`githubSlug(origin)`** (Go, unit-tested) takes the stripped
   `host[:/]owner/name[.git][/]` value and requires the host to be exactly
   `github.com` (case-insensitive) — the same URL shapes prdash's
   `parseGitHubRemote` accepts (scp `github.com:o/n`, ssh/https
   `github.com/o/n`, optional `.git` and trailing `/`), but NOT its loose
   substring host match (`notgithub.com.example` must not parse). It also
   strips a leading `user[:pass]@` defensively, so a stamp from an older probe
   form still parses and validates owner
   `^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`, name `^[A-Za-z0-9._-]+$`, name not
   `.`/`..` — prdash's `--repo` validator, so a shipped value is always one
   prdash accepts. Anything else → `""`. The result is `#`-free, quote-free and
   space-free, so it is safe in a tmux format.

Staleness accepted: an origin URL changed under an unchanged dir stays stale
until the dir changes (stamp persists on the remote window). Scope accepted:
the identity is window-level (`@worktree || @git_root`), exactly like the
`tool` verb's `@bridge_dir` cwd, so a split window whose panes sit in different
repos opens the window's repo board; a remote whose windows carry neither
option (no tmux-og there) ships no dir, so no repo, so the fallback. Both
documented.

### D2. The bind (`prdashBind`, reference nix + generator, byte-identical)

```
bind-key -N 'Open PR dashboard' p
  if-shell -F '#{&&:<bridgeGate>,#{@bridge_repo}}'
    { set -wF @og_prdash_origin "#{@bridge_pane}" ;
      <floatReuse "prdash" (floatNewPaneGuard floatFull "" "<mirror-script> <prdash-bin> <ctl-bin> \; set -p @pane_label prdash")> }
    { <today's bridgedFloatTool body, unchanged> }
```

- R4: a non-mirror window fails `bridgeGate`, so it takes the inner else branch
  exactly as today (same bytes).
- R3: a mirror without `@bridge_repo` takes the inner bridged branch: today's
  `tool prdash` verb. Chosen over local prdash without `--repo`: in a mirror the
  local cwd is the daemon's, so a repo-less local board would show the wrong
  repo (or the all-repos board), while the remote float is today's working
  behaviour.
- `new-pane`'s shell-command and its `-e` values are NOT format-expanded
  (measured on next-3.9: `-e "R=#{@bridge_repo}"` arrives literally), so the
  pressed remote pane id is stashed with `set -wF` into the local mirror window
  option `@og_prdash_origin` (the `floatRegister` precedent: a value handed from
  a bind to what it launches through a window option). The script reads it and
  the other window/session options itself.
- The float is a normal local float: `floatNewPaneGuard` stamps `@float_geom`
  (float-conf-assertions), `floatReuse` focuses an already-open local prdash
  float. The daemon tolerates local panes in a mirror window
  (`localpanes.go`: "a mirror window can acquire a pane this daemon never
  created"); the enrich card is the precedent.

### D3. `scripts/tmux-prdash-mirror.sh` (new, bash)

Argv: `<prdash-bin> <ctl-bin>` (store paths, literal in the bind). Steps:

1. One `tmux display-message -p -t "$TMUX_PANE"` read of
   `#{@og_prdash_origin}|#{@bridge_repo}|#{@bridge_sock}|#{client_name}|#{@bridge_dir}`
   (`|`-delimited per CLAUDE.md; `@bridge_dir` last because a path may hold
   `|`). `@bridge_sock` is the raw socket path (`daemon.go` stamps
   `cfg.SockPath`), so the script passes `--sock="$sock"` itself.
2. Validate pane `^%[0-9]+$` and repo `owner/name` (same regexes). Invalid repo
   with a valid pane → fallback: `exec <ctl> --sock… tool <pane> prdash <dir>`
   (today's behaviour). Invalid pane → `tmux display-message` and exit.
3. `work=$(mktemp -d)`, trap removes it by that name; `mkdir "$work/root"`;
   `cd "$work"` (not a checkout, so prdash's launch repo is remote-only); run
   `PRDASH_ACTION_FILE="$work/handoff" PRDASH_REPOS_ROOT="$work/root" <prdash> --repo "$repo"`.
   The empty repos root guarantees no local checkout is found, so prdash writes
   only path-free `{...}` entries.
4. No or empty hand-off file → exit 0. Else read its lines into an array and run
   `<ctl> --display-error=<client> <sock> handoff <pane> <dir> <line>...` as argv
   (one ctl call for the whole batch, so a bulk `W` runs sequentially in one
   remote float). ctl failures are already surfaced by `--display-error`.

### D4. ctl verb `handoff` (daemon, `ctl.go`)

`handoff <pane> <dir> <line> [<line>...]`: `args: 2, optArgs: 31` (≤ 32
entries), `layout: true, moves: true` (opens a remote float, like `tool`).
`CtlProtocolVersion` "10" → "11" (verb table change; an older resident daemon
answers `ctl protocol version "11", this daemon speaks "10" — reopen the bridge`,
shown by ctl's `--display-error`, for both the hand-off and the fallback `tool`
press).

**Validation** (`parseHandoff`, pure Go; any failure rejects the whole batch
with an error naming the entry index; nothing is sent):

- line ≤ 4096 bytes; split at the first `\t` into key and JSON; key
  `^[A-Za-z0-9_-]{1,16}$`.
- JSON must start with `{` (an `[` argv line is rejected: it carries a local
  path that means nothing remotely); `json.Unmarshal` into the entry struct.
- `key` field == line key; `repo` passes the owner/name validator; `kind` ∈
  {`pr`,`issue`}; `pr` in 1..999999999 for `kind: pr`, 0 for `issue`.
- `args` is exactly `["switch", T]`, or `["switch","-c",T]` for `kind: issue`
  only; `T == target`.
- `target` is `pr:<digits>` equal to `pr:<pr>` (`kind: pr` only), or a branch
  matching prdash's `safeHeadRef` set `^[A-Za-z0-9._/@+-]+$`, no leading `-`,
  ≤ 255 bytes, no `..`, no `//`, not ending in `/`, `.` or `.lock`. Hostile
  names (`$(…)`, `;`, quotes, `#`, spaces, leading `-`) are rejected — the
  validated charset holds nothing special to tmux, fish or sh. This is stricter
  than the task's "hostile branch passed inert" wording (prdash already sends a
  non-`safeHeadRef` head as `pr:N`); the PR says so, and the passed-inert proof
  is an in-charset but unusual branch (`feat/a+b@c.d-1`) reaching the stub `wt`
  byte-identical through the tmux, fish-safe quoting and sh layers.
- Known limit: prdash actions are user-configurable; a custom switch argv with
  extra flags is written as a path-free entry but fails the exact `args` shape.
  The rejection message names the entry index AND the unexpected args.

**Execution**: one remote float, built like `floatToolCommand`'s create branch
but with no reuse gate (a transient, labelled `prdash-handoff`, so it can never
be mistaken for a `prdash` float):
`new-pane -t <pane> -c <cwd> <remoteFloat> '<exec /bin/sh -c <script> sh <repo> <mode> <target> <pr> ...>' ; set -p -t <win> @pane_label prdash-handoff`.
`cwd` is `remoteToolCwd(dir)` when valid, else `'#{pane_current_path}'` (the
origin check below keeps a wrong cwd from being used). Each entry contributes
four positional parameters (mode `switch|create`, pr `0` for issues), each
validated value wrapped in single quotes — safe in both the remote's
default-shell (fish) and sh because the charset has no quote or backslash. The
script body is a constant (no entry data in it), so data only ever reaches sh as
`$1..$N`: it restores PATH/BROWSER like `toolResolveScript`, checks
`command -v wt`, then per entry resolves the checkout — the float's cwd (`.`)
if its origin is that repo, else `${PRDASH_REPOS_ROOT:-$HOME/git}/<repo>` if
its origin is — comparing a lowercased, `.git`/`/`-stripped origin against
`*[@/]github.com[:/]<repo-lowercased>`. No checkout → prints a message naming
the paths tried. PR entry whose branch target is neither `refs/heads/T` nor
`refs/remotes/origin/T` in that checkout → `pr:<N>` (prdash's own standalone
rule, applied on the far side). Runs `wt -C "$dir" switch [-c] "$target"`
sequentially; on any failure, `sleep 5` so the message is readable. The float
runs in the pressed pane's window with `TMUX_PANE` set, so `wt`'s worktrunk
post-switch hook switches the remote window exactly as prdash's own `wt switch`
does today.

### D5. Flake input

Bump `prdash` in `flake.lock` to `606b15f` (or later main) so `--repo` and
path-free entries exist locally. Remote prdash is no longer needed for R1.

## Contract map (shared-contract change)

| Edge | Change | Proof |
|---|---|---|
| remote window → `@og_bridge_origin` (daemon-sent probe script) | new | Go test of the probe command string; script run under `/bin/sh` with stub `git`/`tmux` |
| `windowLabelFormat` / `parseWindowLabels` / `windowLabelFields` | changed (+1 field, before label_id) | existing parser tests + fixture update; new field tests (fresh/stale/garbage) |
| `labelRow` → `bridgeLabelOptions` → local `@bridge_repo` | new option | apply test |
| `labelShipper.flush` probe trigger | new | test: once per (win,dir), re-probe on dir change, none when fresh |
| other label consumers (enrich card, reflow, pickers reading `@bridge_*`) | compatible (new option only, field order of existing fields unchanged after index 21; label_id/rest shift by one index in the parser only) | full daemon test suite |
| `prdashBind` reference ↔ generator | changed, byte-identical | reference diff check + keys_test |
| bind → `tmux-prdash-mirror` | new | bats with stub prdash/ctl on a scratch server |
| script → ctl `handoff` | new verb | ctl parse tests |
| `CtlProtocolVersion` 10 → 11 | changed | existing version tests; every ctl caller is the same build |
| ctl `tool` (fallback) | unchanged | existing tests |
| docs | updated | — |

## Acceptance mapping

- Local launch with the remote repo: bats test of `tmux-prdash-mirror` (stub
  prdash records argv/env/cwd) + generator/reference bind test. Manual check in
  a real mirror only if one is available at execute time; otherwise stated.
- Forwarding: Go tests of `parseHandoff` (valid `enter` PR, issue `-c`, fork
  `pr:N`, bulk), hostile branch rejected, malformed entries rejected (bad JSON,
  argv form, key mismatch, bad repo, args not a switch, target ≠ last arg, too
  many entries), and a test that runs the built remote script under `/bin/sh`
  with stub `wt`/`git` proving the exact argv `wt` receives and the checkout it
  picks (same-repo cwd, other repo under repos root, origin mismatch → message).
- Fallback: bats (no `@bridge_repo` → script/bind takes `tool`), Go test that
  an old protocol version yields the reopen message.
- Docs updated; `nix build .#lint` and `nix flake check` green.
