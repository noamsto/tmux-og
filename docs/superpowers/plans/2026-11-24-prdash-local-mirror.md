# Local prdash for mirrored windows — plan

Spec: `docs/superpowers/specs/2026-11-24-prdash-local-mirror-design.md` (accepted
by spec-critic, findings folded in). Issue #1236. Consult: none (the spec fixes
the decomposition: daemon label probe, ctl verb, launcher script, bind, docs).

Gate commands (whole plan): `nix build .#default`, `nix flake check`,
`nix build .#lint --accept-flake-config`. Per-step fast checks are listed per
step. All commits run inside the devshell (direnv) so pre-commit hooks load.

Ordering: S1 ∥ S2 → S3 ∥ S4 → S5 → S6. S2 and S3 both touch
`picker/remotebridge/daemon`, so they never run concurrently.

## Contracts fixed up front (shared by steps)

- Remote window option `@og_bridge_origin` = `"<host-and-path> <dir>"`, e.g.
  `github.com/o/n.git /home/u/git/o/n` or `github.com:o/n /home/u/x` (scheme and
  userinfo stripped remote-side).
- Local window option `@bridge_repo` = validated `owner/name`, unset when unknown.
- Local window option `@og_prdash_origin` = the pressed pane's `@bridge_pane`
  (`%N`), stamped by the bind with `set -wF` right before the float opens.
- ctl verb: `handoff <remote-pane> <dir> <line> [<line>...]`, 1..32 lines, each a
  raw prdash hand-off line `<key>\t<json>`; `CtlProtocolVersion` "11".
- Script: `tmux-prdash-mirror <prdash-bin> <ctl-bin>` (store paths as argv).
- Owner/name validator (Go and bash, identical): owner
  `^[A-Za-z0-9]([A-Za-z0-9-]{0,38})$`, name `^[A-Za-z0-9._-]+$`, name not `.`
  or `..`.

## S1 — bump prdash to PR #215 (implement: haiku)

Files: `flake.lock` only.

1. `nix flake update prdash` (expect rev `606b15f24d3d3460f86d12b19cc78676eaa6b7d1`
   or a later main commit).
2. Verify: `nix build .#default` succeeds; the flake exposes no `.#prdash`, so
   build the locked input directly (`nix build --no-link --print-out-paths
   github:noamsto/prdash/<locked-rev>`, or read the prdash store path from the
   built conf) and check `<prdash> --repo bad` exits non-zero with
   `invalid --repo` (no TTY needed).
3. If other lock nodes move (prdash's own inputs follow ours), keep only what
   `nix flake update prdash` produced; do not update other inputs.

Done when: build green, `flake.lock` diff limited to the prdash node(s).

## S2 — daemon: origin probe and `@bridge_repo` (implement: sonnet)

Files: `picker/remotebridge/daemon/windowlabels.go`, new
`picker/remotebridge/daemon/originprobe.go` (+ `originprobe_test.go`),
`windowlabels_test.go` and any test fixture built from `windowLabelFields`.

1. **Red first**: in `originprobe_test.go` write table tests for
   `githubSlug(origin string) string`: `github.com/o/n` → `o/n`;
   `github.com:o/n.git` → `o/n`; `GitHub.com/O/N/` → `O/N`;
   `user:tok@github.com/o/n` → `o/n` (defensive strip);
   `notgithub.com.example/o/n`, `github.com.evil/o/n`, `gitlab.com/o/n`,
   `github.com/o`, `github.com/o/n/extra`, `github.com/-o/n`, `github.com/o/..`,
   `github.com/o/n#x`, `` → all `""`. Then implement it.
2. Read `internal/gh/repo.go` `parseGitHubRemote` in prdash at `606b15f`
   (`gh api repos/noamsto/prdash/contents/internal/gh/repo.go?ref=606b15f…`, or
   the S1 store path) and confirm every github.com shape it accepts is accepted
   here, with an EXACT host match instead of its substring match.
3. `originProbeCmd(win string) (string, bool)`: false unless `win` matches
   `^@[0-9]+$`. Returns
   `run-shell -b -t <win> <tmuxQuote("exec /bin/sh -c "+tmuxQuote(script))>`
   where script (zero single quotes; `#` only in the `${p#*=}` PATH-restore
   idiom and `${u#*://}` / `${u#*@}`; never `##`) is:
   PATH restore as `toolResolveScript`; `d=$(tmux show-options -wqv -t <win> @worktree)`;
   fallback `@git_root`; empty → `exit 0`;
   `u=$(git -C "$d" remote get-url origin 2>/dev/null)`; `u=${u#*://}`;
   `case $u in *@*) u=${u#*@};; esac`;
   `tmux set-option -w -t <win> @og_bridge_origin "$u $d"`; `exit 0`.
   Test: the command string shape, the window-id rejection, no single quote in
   the script, and — run the script body itself under `/bin/sh` with stub
   `tmux` and `git` on `PATH` in a `t.TempDir()` (stubs record argv to files)
   — that an https origin with credentials stamps `github.com/o/n.git <dir>`
   and an scp origin stamps `github.com:o/n <dir>`, and a dir-less window
   stamps nothing. The test runs no real tmux.
4. `windowLabelFormat`: insert `#{s/[|]/ /:@og_bridge_origin}|` immediately
   before `#{@window_label_id}`; `windowLabelFields` 24 → 25; shift the parser's
   `at(22)`/`at(23)` to 23/24. `labelRow` gains `repo string` and
   `originDir string` (comparable). Parse: `cleanLabelValueExact(at(22),
   urlMaxRunes+1+dirMaxRunes)`, split at the first space → origin, stamped dir;
   `originDir = stamped dir` (only if it matches `dirRe`); `repo =
   githubSlug(origin)` only when `originDir != "" && originDir == dir`.
5. `bridgeLabelOptions` gains `{"@bridge_repo", r.repo}` (place it right after
   `@bridge_dir`).
6. Probe trigger in `labelShipper`: field `probed map[string]string` (remote
   window id → dir last probed; init in `newLabelShipper`). In `flush`, for every
   row applied this pass (queued and backstop poll alike), call a helper
   `s.probeOrigin(rt, rows)`: for rows with `dir != ""`, `originDir != dir`, and
   `probed[id] != dir`, issue every `rt(cmd)` first and only then await the
   returned waiters (`rt` returns a waiter, `seed.go`), so N probes on a first
   connect cost one round-trip, not N; record `probed[id] = dir`. Forget ids no longer in the registry where `written` is
   pruned. Tests (with the package's existing fake round-trip helpers — find
   them in `windowlabels_test.go` / `seed_test.go`): first row with a dir sends
   exactly one probe; the same row again sends none; a dir change sends one;
   a row whose stamp matches its dir sends none and stamps `@bridge_repo`; a
   stale stamp (other dir) leaves `@bridge_repo` unset; garbage origin → unset.
7. Update existing label tests/fixtures for the new field index (the fixture is
   built from `windowLabelFields`; extend any literal rows).

Fast check: `cd picker && go test -race ./remotebridge/...` and
`golangci-lint run ./remotebridge/...` (devshell). Commit:
`feat(bridge): ship the remote window's GitHub repo as @bridge_repo`.

## S3 — daemon: `handoff` ctl verb (implement: opus — untrusted input crossing to a remote shell)

Files: `picker/remotebridge/wire/protocol.go` (`CtlProtocolVersion` "10" → "11"
and its comment unchanged in substance), `picker/remotebridge/daemon/ctl.go`
(verb entry), new `picker/remotebridge/daemon/handoff.go` (+ `handoff_test.go`),
and any test that hard-codes `"10"`.

1. **Red first**: `handoff_test.go` table tests for
   `parseHandoff(lines []string) ([]handoffEntry, error)` where
   `handoffEntry{repo, mode, target string; pr int}`:
   - valid: `enter\t{"repo":"o/n","kind":"pr","target":"feat/x","pr":7,"args":["switch","feat/x"],"key":"enter"}`;
     fork `pr:7` (`args ["switch","pr:7"]`, target `pr:7`, pr 7);
     issue `{"repo":"o/n","kind":"issue","target":"feat/9-x","args":["switch","-c","feat/9-x"],"key":"enter"}`
     → mode `create`, pr 0; bulk of 3 `W` lines; unusual in-charset branch
     `feat/a+b@c.d-1`.
   - rejected (error names the 1-based entry index; args errors quote the args):
     not JSON; `[...]` argv form; key mismatch; key `en ter`; repo `o`,
     `o/n/x`, `-o/n`, `o/..`; kind `foo`; pr kind with pr 0; issue with pr 3;
     args `["switch"]`, `["switch","-x","claude","feat/x"]`,
     `["remove","feat/x"]`, `["switch","-c","x"]` on a pr kind; target ≠ last
     arg; hostile targets `$(touch pwned)`, `a;b`, `a'b`, `a b`, `#{x}`,
     `-rf`, `a..b`, `a//b`, `a/`, `a.lock`, `a.`, 256-byte name; `pr:7` with
     pr 8; line over 4096 bytes; 0 lines; 33 lines.
   Implement `parseHandoff` per spec D4 (`json.Unmarshal` into a struct with
   `repo, kind, target, pr, args, key`; validator regexes compiled once).
2. `handoffCommand(pane, win, cwd string, es []handoffEntry) string`: like
   `floatToolCommand`'s create branch, without the reuse gate:
   `new-pane -t <pane> -c <cwd> <remoteFloat> <tmuxQuote("exec /bin/sh -c "+tmuxQuote(handoffScript)+" sh "+args)> ; set -p -t <win> @pane_label prdash-handoff`,
   args = for each entry `'<repo>' '<mode>' '<target>' '<pr>'` (single quotes,
   safe because validated values hold no quote/backslash). `cwd` is already
   tmux-quoted (caller uses `remoteToolCwd(dir)` → `tmuxQuote(dir)`, else
   `'#{pane_current_path}'`, exactly like the `tool` verb).
   `handoffScript` is a constant POSIX body (zero single quotes, zero
   backslashes — fish, the remote default shell, treats a backslash inside a
   single-quoted string as an escape — `#` only in `${p#*=}`; never `$#` or
   `##`; a test asserts no `'` and no `\` in it): PATH/BROWSER restore copied from
   `toolResolveScript`; `command -v wt` else message + `sleep 5`;
   `root=${PRDASH_REPOS_ROOT:-$HOME/git}`; `fail=0`; loop
   `while [ -n "$1" ]; do repo=$1 mode=$2 target=$3 pr=$4; shift 4; …; done`;
   per entry: `want=$(printf %s "$repo" | tr A-Z a-z)`; for candidate in `.` and
   `"$root/$repo"`: `o=$(git -C "$c" remote get-url origin 2>/dev/null | tr A-Z a-z)`,
   `o=${o%/}`, `o=${o%.git}`,
   `case $o in *[@/]github.com[:/]"$want") dir=$c; break;; esac`; none →
   `echo "tmux-og: no checkout of $repo here or at $root/$repo"`, `fail=1`,
   continue; `create` → `wt -C "$dir" switch -c "$target" || fail=1`; else if
   `pr` ≠ 0 and target not `pr:*` and neither
   `git -C "$dir" rev-parse --verify --quiet "refs/heads/$target"` nor
   `…"refs/remotes/origin/$target"` succeeds → `target=pr:$pr`; then
   `wt -C "$dir" switch "$target" || fail=1`. After the loop
   `[ "$fail" = 0 ] || sleep 5`.
3. Verb entry `"handoff": {args: 2, optArgs: 31, layout: true, moves: true, build: …}`
   with a comment block in the style of the `tool` verb explaining: why a float
   (wt may prompt; `TMUX_PANE` makes worktrunk's post-switch hook switch the
   pressed window as prdash's own `wt switch` does), why no reuse gate, why the
   label differs, why the data is positional, and the protocol bump.
4. Tests:
   - `parseCtl` on `[ "11", "handoff", "%3", "/d", line ]` returns cmds with the
     expected shape, `wantLayout`, `invalidate`; an invalid line returns the
     error and NO cmds; version `"10"` returns the `reopen the bridge` error.
   - **Script execution test** (no tmux): extract the script and positional args
     by running `/bin/sh -c handoffScript sh <args…>` directly with `PATH`
     pointing at a temp dir of stubs: `wt` records its argv (one line per arg)
     to a file; `git` emulates `remote get-url origin` per `-C` dir from a map
     file and `rev-parse --verify --quiet` from a branch list; `tmux` prints
     nothing (PATH restore must tolerate it). `HOME`/`PRDASH_REPOS_ROOT` point
     into the temp dir; cwd is a temp "checkout". Cases: same-repo PR → `wt -C . switch feat/x`;
     unfetched branch → `wt -C . switch pr:7`; issue → `switch -c`; other repo
     under the root → `wt -C <root>/o2/n2 switch …`; origin mismatch → no `wt`
     call, message on stdout; `feat/a+b@c.d-1` reaches `wt` byte-identical;
     bulk runs `wt` once per entry in order. Make the `sleep 5` skippable in the
     test via a stub `sleep` on PATH.
   - A builder-level test feeding `handoffCommand` an entry whose target is
     `$(touch pwned)` (bypassing the validator) and asserting the built string
     carries it only inside single quotes in the positional section, never in
     the script body — documents defence in depth.
5. Grep all tests/fixtures for the literal protocol `"10"` and update.

Fast check: `cd picker && go test -race ./remotebridge/...`, golangci-lint,
`nilaway` per CLAUDE.md (devshell). Commit:
`feat(bridge): forward prdash worktree hand-offs to the remote (ctl v11)`.

## S4 — launcher script `tmux-prdash-mirror` (implement: sonnet)

Files: new `scripts/tmux-prdash-mirror.sh`; `config/tmux.conf.nix`
(`scriptNames` ~line 301 AND `ogInternal` ~line 829 — `ogPartitionOk` fails
if it is missing from the latter; follow how `tmux-crew-dash` is listed); `generator/paths/paths.go` script list; new
`tests/prdash-mirror.bats` + its `checks` entry in `flake.nix` (copy only the
files it reads: `testsSrc ["prdash-mirror.bats" "helper.bash"]`,
`scriptsSrc ["tmux-prdash-mirror.sh"]`; the closest precedent is
`tests/crew-dash-launch.bats` and its check in `flake.nix` ~line 2966).

1. **Red first**: write the bats file. Each test starts a scratch tmux server
   per CLAUDE.md (`TMUX_TMPDIR`, `CLAUDE_STATUS_DIR`, `XDG_DATA_HOME` under a
   fresh `mktemp -d`, `-L` socket, `kill-server` on the same socket in
   teardown, delete only the dir it created). Stubs in a temp bin dir: `prdash`
   records `$@`, `$PWD`, `$PRDASH_ACTION_FILE`, `$PRDASH_REPOS_ROOT` and whether
   the repos root is an empty dir, then appends lines from a fixture to the
   action file; `ctl` records each argv element on its own line. Run the script
   inside a pane of the scratch server (so `$TMUX_PANE` and `display-message -t`
   resolve) via `run-shell`/`new-window` and wait for a sentinel file; or set
   `TMUX`/`TMUX_PANE` explicitly against the scratch socket — choose whichever
   the existing bats helpers already support. Cases:
   - window `@bridge_repo o/n`, `@og_prdash_origin %5`, `@bridge_dir /r/d`,
     session `@bridge_sock /s/sock`: prdash gets `--repo o/n`, cwd ≠ any
     checkout, `PRDASH_ACTION_FILE` inside a private dir, empty repos root;
     with 2 fixture lines ctl gets exactly
     `--display-error=<client> --sock=/s/sock handoff %5 /r/d <line1> <line2>`
     (one call); the temp dir is gone afterwards.
   - no fixture lines → ctl not called.
   - a line containing `$(touch pwned)` is passed to ctl byte-identical and no
     `pwned` file appears (bash passes it as argv; the daemon rejects it).
   - fallback: `@bridge_repo` unset or `o` (invalid) with a valid pane → ctl gets
     `… tool %5 prdash /r/d`, prdash not run.
   - invalid `@og_prdash_origin` → neither stub runs; exit 0.
   - 33 fixture lines → ctl not called, a `display-message` names the 32-entry
     bridge cap (nothing partial is forwarded: a bulk `W` that large is refused
     whole, and the cap is documented in `scripts.md`).
2. Implement the script per spec D3: `set -uo pipefail`; one
   `display-message -p -t "$TMUX_PANE"` read
   `#{@og_prdash_origin}|#{@bridge_repo}|#{@bridge_sock}|#{client_name}|#{@bridge_dir}`
   split with `IFS='|' read -r pane repo sock client dir` (dir last, keeps any
   `|`); regex checks in bash `[[ =~ ]]` with the shared validator; fallback
   `exec "$ctl" --display-error="$client" --sock="$sock" tool "$pane" prdash "$dir"`;
   `work=$(mktemp -d)`; `trap 'rm -rf -- "$work"' EXIT`; `mkdir "$work/root"`;
   `cd "$work"`; run prdash (not exec) with the two env vars; `mapfile -t` the
   action file if non-empty; more than 32 lines → `tmux display-message -d 5000`
   naming the cap and exit; else one ctl call. Header comment: why it reads tmux
   itself (`new-pane` shell-command and `-e` are not format-expanded, measured
   next-3.9), why the empty repos root (forces path-free entries), why one call.
   `shellcheck` and `shfmt` (tabs) clean.
3. Register the binary in both nix lists and `generator/paths/paths.go`; run
   `cd generator && go test ./...` (paths tests enumerate the list).

Fast check: `nix build .#checks.x86_64-linux.<new-check-name>` and
`shellcheck scripts/tmux-prdash-mirror.sh`. Commit:
`feat(prdash): launcher that runs prdash locally for a mirror and forwards hand-offs`.

## S5 — the bind (implement: sonnet)

Files: `config/tmux.conf.reference.nix` (`prdashBind`), `generator/render/keys.go`
(`prdashBind`), `generator/render/keys_test.go`; the generator's paths/render
plumbing only if the script path is not already reachable as
`p.Scripts["tmux-prdash-mirror"]`.

1. **Red first**: extend `keys_test.go`: with `p.Prdash` set,
   `prdashBind(p)` (a) starts with
   `bind-key -N 'Open PR dashboard' p if-shell -F '#{&&:#{&&:#{@bridge_win},#{@bridge_pane}},#{@bridge_repo}}' { set -wF @og_prdash_origin "#{@bridge_pane}" ; `,
   (b) contains the floatReuse/floatNewPaneGuard local branch running
   `<scripts>/tmux-prdash-mirror <prdash-bin> <ctl-bin> \; set -p @pane_label prdash`
   with the `floatFull` stamp, and (c) its else block is byte-identical to the
   old `bridgedFloatTool(p, "p", …, "prdash", …)` string (compute the old string
   in the test so R4 is pinned). `bridgedFloatTool` returns a whole
   `bind-key -N … <key> <body>` line, so split it: a new
   `bridgedFloatToolBody(p, tool, f, prefix, suffix)` returns the
   `if-shell -F '<bridgeGate>' {…} {…}` body, `bridgedFloatTool` becomes
   `bind-key -N '<note>' <key> ` + that body (byte-identical output for the
   other tools), and the test pins prdash's else block to
   `bridgedFloatToolBody(p, "prdash", floatFull, …)` with the old prefix/suffix.
   Mirror the same split in the reference nix. Keep
   `TestBridgedFloatToolsReuseTheirOwnLabel` passing (adapt only its prdash row
   if needed).
2. Implement in Go: a `prdashMirrorBind`-style composition — keep
   `bridgedFloatTool` untouched for the other tools; build the outer `if-shell`
   in `prdashBind`. The ctl-bin literal is `p.Bin["og-remote-bridge-ctl"]` (no
   flags: the script adds `--display-error`/`--sock`).
3. Mirror it byte-for-byte in `config/tmux.conf.reference.nix` with a comment
   block above `prdashBind` (why local, the gate, why the stash option, the
   fallback) — the reference diff check in `flake.nix` fails on any drift.
4. `nix build .#default` then confirm the rendered conf contains the new bind:
   `rg -n "@og_prdash_origin" result/…` (find the conf path via the wrapper).
   Check `tests/conf-shell-quoting.bats` and `float-conf-assertions` pass
   (`nix flake check` or their individual checks).

Fast check: `cd generator && go test ./...`; the reference-diff check and
float/quoting checks via `nix build .#checks.x86_64-linux.<name>`. Commit:
`feat(prdash): open prdash locally in a mirror window with the remote's repo`.

## S6 — docs and diagrams (implement: sonnet)

Files: `docs/agents/bridge-daemon.md` (tool-binds row: `prefix + p` in a mirror
with `@bridge_repo` now opens LOCAL prdash; fallback; new `handoff` verb row
or sentence: validation, float, positional data, `wt` post-switch; protocol
`"10"` → `"11"` and what an old daemon shows), `docs/agents/bridge-shipped-state.md`
(`@bridge_repo` / `@og_bridge_origin`: the probe, freshness by dir, window-level
scope, staleness, credentials stripped remote-side), `docs/agents/scripts.md`
(new `tmux-prdash-mirror` row), README key table text for `prefix + p` if it
describes the mirror behaviour (rg `prdash` in README.md), and the CLAUDE.md
table only if a new deep-dive doc is created (none planned). Keep the plan and
spec files committed.

Fast check: `nix build .#lint --accept-flake-config` (typos, markdown hooks).
Commit: `docs: local prdash in mirror windows`.

## Final gate (lead)

`nix build .#default && nix flake check && nix build .#lint --accept-flake-config`.
Manual check in a real mirror only if one is reachable from this host; else the
PR states it was not run.
