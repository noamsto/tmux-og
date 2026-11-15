# Port `og-remote-open` to Go — plan

Spec: `docs/superpowers/specs/2026-11-15-og-remote-open-go-design.md` (section
letters A–F and "Process model" below refer to it). Issue #1107.

Oracle for every behaviour: the bash at commit `86a11d34`
(`git show 86a11d34:scripts/og-remote-open.sh`), the bats suites, and the
fixtures Step 1 captures from that bash. All `go` commands run from `picker/`
inside the devshell (`direnv`/`nix develop`). `$SCRATCH` below is any scratch
directory outside the repo.

## File list

Create:

- `picker/remoteopen/quote.go` — `shellQuote`, `bashQuote` (`printf %q`), `shellQuotable`, `validRemotePath`, `requireSessionName`, `posixDirname`.
- `picker/remoteopen/quote_test.go` — table tests, incl. `testdata/quote.tsv`.
- `picker/remoteopen/cksum.go` — POSIX `cksum` CRC, `intoSockPath`.
- `picker/remoteopen/cksum_test.go`.
- `picker/remoteopen/args.go` — `Opts`, `Env`, `UsageError`, `ParseArgs` (A1–A10), `Validate` (A11, A13–A18).
- `picker/remoteopen/args_test.go`.
- `picker/remoteopen/probe.go` — `buildProbeScript`, `parseProbe`.
- `picker/remoteopen/probe_test.go` — golden tests over `testdata/probe/`.
- `picker/remoteopen/testdata/quote.tsv`, `picker/remoteopen/testdata/probe/*.txt` — oracles captured from the bash (Step 1).
- `picker/remoteopen/runner.go` — external-command runner (inherit/capture/discard, trailing-newline strip, `set -e` exit), signal pending flag, `checkpoint`, `exit` path.
- `picker/remoteopen/runner_test.go` — plus the recording runner fake used by later tests.
- `picker/remoteopen/lock.go` — `intoLock` (D4).
- `picker/remoteopen/lock_test.go`.
- `picker/remoteopen/daemon.go` — `remoteDaemonAlive`, `probeDaemon`, `reapDaemon`, `launchDaemon`, daemon env builders (F2/F11).
- `picker/remoteopen/daemon_test.go` — helper-process children.
- `picker/remoteopen/local.go` — local tmux reads/writes: `resolveInto` (B), `initialMirrorArea` (E12), `termIdentity` + `splitTermIdentity` + `readSessionEnv` (F3), `pairLookup` + `splitPairLine` (F5), `retireMirror`, `collisionWalk` (F6).
- `picker/remoteopen/local_test.go` — incl. the three recorded-argv guards.
- `picker/remoteopen/remote.go` — ssh legs: probe (E2), `startRemoteServer` (E9), `firstRemoteSession`, `hasSession`, restore (E11), new-dir (E13), active window (E14).
- `picker/remoteopen/remote_test.go`.
- `picker/remoteopen/launcher.go` — `Paths`, `Main(paths, args) int`, the launcher state (`attached`, `placed`, `mirrorCreated`, `daemonStarted`, `lockHeld`), `onSignal`, the A→F flow, progress fd.
- `picker/remoteopen/launcher_test.go` — end-to-end flow over the recording runner.
- `picker/remoteopen/cmd/launcher/main.go` — internal flags `--self --ctl --daemon --renderer --reflow --loading --`, calls `remoteopen.Main`.

Modify:

- `scripts/og-remote-open.sh` — becomes the shim (spec "Shape" 2).
- `scripts/lib-remote.sh` — delete `remote_daemon_alive`, `valid_remote_path`, `shell_quotable`, `mirror_name_part`, `read_session_env`.
- `picker/default.nix` — `subPackages` += `"remoteopen/cmd/launcher"`; `postInstall` `mv $out/bin/launcher $out/bin/og-remote-launcher`.
- `config/tmux.conf.nix` — `mkRemoteScript` replacement `@remote_launcher@` → `${picker-generate}/bin/og-remote-launcher`; comment above `scriptsWithRemote`.
- `flake.nix` — `remote-tests`: add `OG_REMOTE_LAUNCHER = "${pickerChecked}/bin/og-remote-launcher";`, drop `MIRROR_NAME_VECTORS` and its comment; `mirror-session-name-integration-tests`: add the same `OG_REMOTE_LAUNCHER`, reword the comment calling the launcher a script.
- `tests/remote.bats` — delete the helper cases and the three source-grep cases (spec "Test changes"); delete the file and its `bats` line in `flake.nix` if nothing remains.
- `tests/mirror-session-name-integration.bats` — drop the `source lib-remote.sh`; replace `mirror_name_part` calls with literal expected names.
- `picker/mirrorname/mirrorname.go` — drop the `mirror_name_part` twin clause.
- `picker/remote.go`, `picker/remote_zoxide.go`, `picker/remotebridge/graphics/fetch.go` — comments naming deleted shell helpers.
- `CLAUDE.md`, `docs/agents/scripts.md`, `docs/agents/bridge-daemon.md`, `docs/agents/picker.md` — point at the Go package; drop the mirror-name pair from "Duplicated tables".

## Steps

Dependency order: 1 → {2, 3} → 4 → 5 → {6, 7} → {8, 10} → 9 → 11 → 12 → 13 → 14 → 15 → 16. Inside a brace, steps are parallel-safe (disjoint files, no symbol of one used by the other): 5 needs `Opts`/`Env` from 4; 9's `retireMirror` needs 8's `probeDaemon`/`readPidFile`/`reapDaemon`; 10 needs only the runner (6) and quote helpers (2).

Phase lines: `phase()` lives with the progress fd in `launcher.go` (Step 11). Steps 9 and 10 take it as a `phase func(string)` parameter (or a field on the struct their functions hang off) so `startRemoteServer` can emit `start-server`, restore/new-dir emit `restore`/`create` and E14 emits `connect`; their tests pass a recorder.

Flakes see only git-tracked files: from Step 1 on, every new file is `git add`-ed as it is created (Steps 1–11 commit their files at the end of the step: `git add picker/remoteopen && git commit -m 'wip(remoteopen): step N'`; the series is squashed or kept as reviewable commits at ship time). The spec and plan are committed before Step 1.

- [ ] **Step 1: capture the bash oracles** — files `picker/remoteopen/testdata/probe/*.txt`, `picker/remoteopen/testdata/quote.tsv`.
  Generator lives in `$SCRATCH`, not the repo. Run everything from `picker/`. Setup: `root=$(git rev-parse --show-toplevel)`; `mkdir -p "$SCRATCH/t" "$SCRATCH/bin" remoteopen/testdata/probe`; `git show 86a11d34:scripts/og-remote-open.sh | sed "s|@lib_remote@|$root/scripts/lib-remote.sh|g" > $SCRATCH/old.sh` (absolute lib path; lib-remote at HEAD still has every helper at this point — check `grep -c '^mirror_name_part' $root/scripts/lib-remote.sh` → `1`). Fakes in `$SCRATCH/bin`, prefixed to PATH: `ssh` — if its argv contains `bash -s`, `cat > "$OUT"` (raw stdin bytes, trailing newline included) and exit 0 printing nothing; `tmux` — `list-sessions -F '#{session_id}|#{session_name}'` prints `$3|dispatch`, `display-message … #{pid}` prints `4242`, everything else prints nothing and exits 0; `og-remote-bridge-ctl` exits 1. Run with `TMUX_TMPDIR=$SCRATCH/t` (exists, so the placed lock's `mkdir` succeeds at once), each case `OUT=$PWD/remoteopen/testdata/probe/<name>.txt bash $SCRATCH/old.sh <args>` (the script exits 1 at "unusable remote tmpdir" after the probe; expected). Between the two placed cases `rm -rf "$SCRATCH"/t/og-into-*` so no leftover lock or pidfile is probed. Cases and names:
  `default` (`h`), `tmpdir` (`OG_REMOTE_TMPDIR=/run/user/1000`, `h`), `sess` (`h work`), `sess-quote` (`h "it's"`), `want-index` (`h work 2`), `want-id` (`h work @5`), `restore` (`OG_REMOTE_RESTORE=1`, `h work`), `restore-win` (`OG_REMOTE_RESTORE=1`, `h work 3`), `newdir` (`OG_REMOTE_NEW_DIR=/srv/p`, `h proj`), `nosess-win` (`h "" 2`), `placed` (`h work --into dispatch`), `placed-tmpdir` (`OG_REMOTE_TMPDIR=/run/user/1000`, `h work --into dispatch`).
  `quote.tsv`: one row per input, `<input as $'…' C-escaped>\t<bash printf %q output>`, generated under `LC_ALL=C` by `printf '%q'`, inputs: empty string; each printable ASCII byte 0x20–0x7e alone; `a<c>b` for each printable ASCII `<c>`; `~a`, `a~`, `#a`, `a#`, `=a`, `a=b`; `\t`, `\n`, `\x01`, `\x7f`, `a\nb`, `it's`, `$(touch x)`, `#{pid}`, `@a=b c`, `\xc3\xa9`. (Row encoding is the implementer's choice as long as the test decodes it; document it in a comment at the top of `quote_test.go`.)
  Proof (from `picker/`): `ls remoteopen/testdata/probe | wc -l` → `12`; `grep -c ': og-probe;' remoteopen/testdata/probe/*.txt` → every file `1`; `wc -l < remoteopen/testdata/quote.tsv` ≥ 200; `git add remoteopen/testdata && git commit` succeeds.

- [ ] **Step 2: quoting and validation helpers (test-first)** — files `quote.go`, `quote_test.go`.
  Test first: `TestBashQuote` over `quote.tsv`; `TestShellQuote` (`it's` → `'it'\''s'`, empty → `''`); `TestShellQuotable` (backslash rejected; `/srv/my project (v2)`, `it's` accepted); `TestValidRemotePath` (accepts `/run/user/1000`, `/tmp/tmux-1000`; rejects `bin/x`, `/run/user/1000 x`, `/run/user/$(id -u)`, empty); `TestRequireSessionName` (newline, `\x01`, `\x7f` → the A16 message with `bashQuote` of the name; `a\b` → A17 message; `work`, empty → nil); `TestPosixDirname` (`/usr/bin/tmux`→`/usr/bin`, `/usr/bin/`→`/usr`, `tmux`→`.`, `/`→`/`, `//`→`/`, `/tmux`→`/`). `go test ./remoteopen/` → FAIL (undefined). Then implement; `requireSessionName` returns `*UsageError` (declare the type here: `type UsageError struct{ Msg string }`, `Error()` returns `og-remote-open: ` + Msg).
  Proof: `go test -race ./remoteopen/` → `ok`.

- [ ] **Step 3: POSIX cksum and the --into socket path (test-first)** — files `cksum.go`, `cksum_test.go`.
  Test first: `TestCksum` vectors generated now with coreutils (`printf '' | cksum` → `4294967295 0`, `printf 'abc' | cksum`, `printf 'h\0s\0' | cksum`, a 300-byte input) — record CRC and length; `TestIntoSockPath`: `intoSockPath(base, host, sess, serverPid, intoID, selector)` equals `printf '%s/og-into-%s.sock' base "$(printf '%s\0%s\0%s\0%s' tp-g6 workstation 4242 '$3' | cksum | cut -d' ' -f1)"` (value computed by shell during authoring and pasted), and the five-field variant with `@crew_branch=B`. `intoBaseDir(env)` = `TMUX_TMPDIR` non-empty → it, else `XDG_RUNTIME_DIR` non-empty, else `/tmp`, tested with empty-string values. Red, then implement.
  Proof: `go test -race ./remoteopen/ -run 'Cksum|IntoSock|BaseDir'` → `ok`.

- [ ] **Step 4: argv parsing and refusals (test-first)** — files `args.go`, `args_test.go`.
  Test first, one table per function with exact `err.Error()` strings from spec A (prefix `og-remote-open: `):
  `ParseArgs` — every A1–A10 row, each with a passing neighbour; overlaps that pin order: `h --into` (A2), `h s 1 --window x` (A3), `h s --window @1 --into x` (A4 usage), `h s 1 --into x` (A5), `h s --into x y` (A6), `h s --into x --window` (`$#==5` → A6), `h s --into x --nope @1` (A6), env `OG_REMOTE_RESTORE=1` + `h s --into x` (A7), `h "" --into x` (A8, not A2), `'bad|host' s --into x` and `-x s --into x` (A9), each A10 bad selector from `tests/remote-cold-start.bats` line ~2045 plus `''` (message uses `bashQuote`). Accepted shapes: `h` (sess ""), `h s`, `h s 3`, `h s @5` (WantMode true), `h s 3 junk` (junk ignored, Win `3`), `h s --into x`, `h s --into x --window @3`, `h s --into x --window @crew_branch=B`; placed sets `Win=""`.
  `Validate` — A11 (`h s abc`), A13 (`h "" @5`; env restore + `h s @5`), A14, A15, A16/A17 on `Sess`, A18; `WantMode` false under either env var; placed opts skip A11/A13.
  Red, then implement exactly the bash conditions (spec A; `$#` counts are over the public argv).
  Proof: `go test -race ./remoteopen/ -run 'ParseArgs|Validate'` → `ok`.

- [ ] **Step 5: probe script builder and parser (golden)** — files `probe.go`, `probe_test.go`.
  Test first: for each Step 1 fixture, build the matching `Opts`/`Env` and assert `buildProbeScript(o)+"\n"` equals the fixture bytes (byte diff on failure). `TestParseProbe`: lines `os=Linux`, `uid=1000`, `tmux=/usr/bin/tmux`, `tmpdir=/run/user/1000`, `sess=a=b`, `win=1`, `want=`, a fish greeting line, a duplicate `sess=` (last wins), `sess=x` with trailing `\r` kept verbatim. Red, then port lines 381-474 segment by segment, using `shellQuote`.
  Proof: `go test -race ./remoteopen/ -run 'Probe'` → `ok`.

- [ ] **Step 6: runner, signals, exit path (test-first)** (implement: opus) — files `runner.go`, `runner_test.go`.
  `type runner interface { Run(c cmdSpec) (stdout string, status int, err error) }` with `cmdSpec{Name string; Args []string; Stdin string /* "" = inherit */; StdinData *string; Capture bool; DiscardStderr bool; CombinedOutput bool}` (exact field set is the implementer's; it must express: inherit stdin/stdout/stderr, feed a string to stdin, capture stdout with all trailing `\n` stripped, discard stderr, capture stdout+stderr combined). `execRunner` resolves `Name` via PATH at call time; a PATH miss returns status 127 and a `not found` error. `checkpoint()` is called by the launcher before and after every `Run` and every poll sleep (the runner takes a `before/after func()` hook pair so the launcher wires it). `signals.go`-style code in `runner.go`: `watchSignals()` = `signal.Notify(ch, TERM, INT, HUP)` + goroutine storing into an `atomic.Bool`/`atomic.Int32`; `pending()` reads it.
  Exit path: `type exiter struct{ release func(); osExit func(int) }`; `exit(code)` runs `release` then `osExit`. Tests inject `osExit` that panics with a sentinel.
  Also `recordingRunner` (test-only, `runner_fake_test.go` in the same package) recording each `cmdSpec` and answering from a `func(cmdSpec) (string, int)` script — used by Steps 9–11.
  Tests first: capture strips `"a\n\n"` → `"a"` but keeps `"a\nb"`; non-zero status reported; discarded stderr does not reach the test's stderr (run `sh -c 'echo x >&2; exit 3'` with stderr redirected to a pipe the test reads); stdin string delivered (`cat`); a self-sent `syscall.Kill(os.Getpid(), SIGTERM)` followed by a 200 ms poll of `pending()` observes the signal; exit runs `release` before `osExit`.
  Proof: `go test -race ./remoteopen/ -run 'Runner|Signal|Exit'` → `ok`.

- [ ] **Step 7: the --into lock (test-first)** (implement: opus) — files `lock.go`, `lock_test.go`.
  `intoLock{path string; pid int; ticks int; tick time.Duration; sleep func(time.Duration); now func() time.Time; alive func(int) bool; rename func(from, to string) bool; onTimeout func() bool /* probeDaemon==live → true */}`; `acquire() (acquired bool, err error)` implements spec D4 steps 1–7 verbatim (owner write `"<pid>\n"`; failed owner write returns an error the launcher turns into exit 1; timeout returns `acquired=false` after `onTimeout`; the launcher prints the D4.2 message). `release()` removes the dir only while `lockHeld` and `owner` reads our pid. `ident()` returns `pid:<owner>` / `ino:<inode>` (`syscall.Stat_t.Ino`) / `""`. Production `rename` = exec PATH `mv <from> <to>` with stderr discarded, true on exit 0; production `alive` = `syscall.Kill(pid,0)==nil`.
  Tests first (temp dirs, fake sleep that advances a fake clock, no real sleeping): free path acquired, owner file `"<pid>\n"`; dead-owner lock (`alive` false) broken and re-acquired, no `*.stale.*` left; owner-less lock broken only on the 50th tick, not the 49th; ident change mid-wait resets the count; live owner held past `ticks` → `onTimeout` consulted, `acquired=false`; `onTimeout` true → treated as success by the caller (return value distinguishes); `.break` older than 5 s removed (set mtime with `os.Chtimes`), a fresh one left; rename hook that recreates the lock in between (simulating the takeover) → the re-checked ident differs, so the new lock is not moved; `release` with a foreign owner leaves the dir.
  Proof: `go test -race ./remoteopen/ -run Lock` → `ok`.

- [ ] **Step 8: daemon liveness, probe, reap, launch** — files `daemon.go`, `daemon_test.go`.
  `readPidFile(path) (int, bool)`: trim trailing `\n`, `^[0-9]+$`, > 0 (spec deviation 2). `remoteDaemonAlive(pidfile)`; `probeDaemon(r runner, ctl, sock) string` → `live`/`outdated`/`""` (CombinedOutput; `— reopen the bridge` substring); `reapDaemon(pid, sleep)` TERM, 20 × 100 ms `kill -0`, KILL. `launchDaemon(path, env []string, logPath string) (*daemonProc, error)`: `SysProcAttr{Setsid: true}`, stdin/stdout `/dev/null`, stderr `O_CREATE|O_TRUNC|O_WRONLY 0644` on `logPath`; a goroutine `Wait`s and closes `done`. `dedicatedEnv(base []string, v envVals) []string` and `placedEnv(...)` per spec F11/F2 (set/override keys, remove `OG_DAEMON_SELECT` resp. `OG_DAEMON_REMOTE_OPEN`, keep `OG_DAEMON_INTO` untouched in dedicated).
  Tests: helper-process pattern (`TestHelperProcess` gated on an env var) for a child that sleeps, one that ignores TERM (reap escalates to KILL, `kill -0` then fails), one that prints its pgid (equals its pid → Setsid); pidfile parsing table (`"123\n"`, `""`, `"-1"`, `"0"`, `"12a"`); probeDaemon with a fake ctl script written to a temp dir (exit 0 → live; exit 1 printing `… — reopen the bridge` → outdated; exit 1 other → `""`; missing/dead pidfile → `""` without running ctl); env builders: placed removes `OG_DAEMON_REMOTE_OPEN`, sets `OG_DAEMON_SELECT=` when empty; dedicated removes `OG_DAEMON_SELECT`, sets `OG_DAEMON_REMOTE_OPEN`, keeps an inherited `OG_DAEMON_INTO`.
  Proof: `go test -race ./remoteopen/ -run 'Daemon|PidFile|Env'` → `ok`.

- [ ] **Step 9: local tmux helpers + recorded-argv guards** — files `local.go`, `local_test.go`.
  Port spec B1–B2, E12, F3, F5, `retireMirror`, F6 over the `runner` interface (each tmux call's argv, stderr handling and error-to-empty rule exactly as the spec rows). Pure splitters: `splitPairLine(line) (id, host, sess string)`, `splitTermIdentity(raw) [5]string` (spec F3 IFS rule), `parseMirrorArea(raw) (w, h int, ok bool)`.
  Tests (recordingRunner): the three guards — collision-walk `show-options` argv is `show-options -t <name> -qv @bridge_host` (never `=`); no recorded call is a `display-message` naming `@bridge_`; pair lookup `-F` arg equals the spec string; `splitPairLine("$1|h|x|")` → sess `x|`; `splitTermIdentity` cases `a|b|c|d|z|` → `z`, `a|b|c|d||` → `""`, `a|b|c|d|z|w|` → `z|w|`, `a|b|c|d|z||` → `z||`; fewer fields → empties; `parseMirrorArea` for `200 50 on` → (200,49), `200 50 off` → (200,50), `200 50 2` → (200,48), `200 1 on` → not ok, `0 50 on` → not ok, `200 50 weird` → (200,49); collision walk: base free → base; base held by other host → `base-remote`, then `base-remote-2`; legacy adoption only when `base == host-sess` and session empty; `readSessionEnv` for `NAME=v`, `-NAME`, non-zero status, empty session (no call recorded); `resolveInto` exact match with a `|` in the name.
  Proof: `go test -race ./remoteopen/ -run 'Local|Pair|Term|MirrorArea|Collision|SessionEnv|ResolveInto|Guard'` → `ok`.

- [ ] **Step 10: remote ssh legs** — files `remote.go`, `remote_test.go`.
  Port spec E2, E9, E11, E13, E14 and `firstRemoteSession`/`hasSession` over `runner`, with the exact argv strings from the bash (`env TMUX_TMPDIR=<t> <tmux> has-session -t '<=sess>'` with `shellQuote("="+sess)`, etc.). Placed probe argv: `[timeout <p>] ssh -T -o ConnectTimeout=<p> -o ServerAliveInterval=5 -o ServerAliveCountMax=3 <host> bash -s`, `timeout` included only when `exec.LookPath("timeout")` succeeds (inject the lookup for tests). Each function returns `(value, status)` or a `*UsageError`; the launcher decides exit codes.
  Tests (recordingRunner): exact argv for each leg, incl. restore's stdin `env TMUX_TMPDIR=/run/user/1000 PATH=/usr/bin:$PATH /usr/bin/tmux-remux restore\n`, new-dir `-x 200 -y 49` suffix only with an area, Darwin vs Linux start command and its error message, placed probe with/without `timeout`.
  Proof: `go test -race ./remoteopen/ -run Remote` → `ok`.

- [ ] **Step 11: launcher flow, signals and main** (implement: opus) — files `launcher.go`, `launcher_test.go`, `cmd/launcher/main.go`.
  `Main(p Paths, args []string) int` in exact bash order: progress-fd adoption (F_GETFD, CLOEXEC, `os.Unsetenv("OG_REMOTE_OPEN_PROGRESS_FD")`) happens first in code but its **observable** position (A12 after it) is preserved; then `watchSignals`; `ParseArgs`; placed → B1, B2; A12 predict read; `Validate`; placed → D1–D5 (lock with `onTimeout`), E1–E14, F1–F12. Every `[set -e]` row exits with the failing command's status; a bridge-binary PATH miss prints the spec deviation 4 message and exits 1. Wire the runner hooks to `checkpoint()`; add the explicit checkpoints before each `attached = true` and right after daemon `Start`. `onSignal` per spec. `Paths{Self, Ctl, Daemon, Renderer, Reflow, Loading string}` with the `@`-prefix PATH fallback (`og-remote-bridge-ctl`, `og-remote-bridge-daemon`, `og-remote-bridge-renderer`, `tmux-reflow-windows`, optional `og-remote-loading`) resolved where the bash resolved them (placed ctl at D3; all at F1). Progress fd closed right before each daemon launch. `cmd/launcher/main.go`: parse the six internal flags with `flag` up to `--`, then `os.Exit(remoteopen.Main(paths, rest))`; missing `--` → usage to stderr, exit 2.
  Tests (recordingRunner + temp dirs, daemon launch stubbed through an injectable `launch` func): dedicated happy path records `new-session -d -s h-work -n work -- <loading> h work <phase>` then `set-option … @bridge_session` before `@bridge_host`, then `switch-client -t =h-work`; dedup path (live daemon + session) records `switch-client` and no `new-session`; placed happy path records no `new-session|kill-session|switch-client|set-option`; `onSignal` with `attached` → 0; dedicated `mirrorCreated` → records `kill-session -t =h-work`, removes sock files, 143; placed with started daemon → reaps it, lock released, 143.
  Proof: `go test -race ./remoteopen/...` → `ok`; `go build -o $SCRATCH/og-remote-launcher ./remoteopen/cmd/launcher` → exit 0.

- [ ] **Step 12: shim and Nix wiring** — files `scripts/og-remote-open.sh`, `picker/default.nix`, `config/tmux.conf.nix`, `flake.nix`.
  Shim exactly as spec "Shape" 2 plus the not-found error (`[[ -n $launcher ]] || { echo "og-remote-open: og-remote-launcher not found (build picker/ or set OG_REMOTE_LAUNCHER)" >&2; exit 1; }` before the `exec`); header comment: logic lives in `picker/remoteopen`, placeholders are substituted by `mkRemoteScript`. Nix edits per the file list.
  Proof: `shellcheck scripts/og-remote-open.sh` and `shfmt -d scripts/og-remote-open.sh` → no output; `nix build .#default` → success; `nix-store -qR ./result | rg -x '.*-og-remote-open'` names the shim derivation `$D`, and `rg -o '/nix/store/[^"]*/bin/og-remote-launcher' $D/bin/og-remote-open` → one store path that exists (`test -x`).

- [ ] **Step 13: drive the launcher bats suite green** — files: only `picker/remoteopen/*.go` (no test edits).
  `go build -o $SCRATCH/og-remote-launcher ./remoteopen/cmd/launcher`, then from the repo root `OG_REMOTE_LAUNCHER=$SCRATCH/og-remote-launcher bats tests/remote-cold-start.bats` and `bats tests/remote-picker.bats tests/picker-launcher.bats`. Each failure is a divergence from the bash: compare against `git show 86a11d34:scripts/og-remote-open.sh` and fix the Go (add a unit test pinning the divergence when it is not already covered).
  Proof: both commands → `ok` for every case, `0 failures`.

- [ ] **Step 14: delete the shell helpers and their tests** — files `scripts/lib-remote.sh`, `tests/remote.bats`, `tests/mirror-session-name-integration.bats`, `flake.nix` (only if `remote.bats` is deleted, or to drop `MIRROR_NAME_VECTORS` if Step 12 left it).
  Delete the five helpers; `rg -n 'remote_daemon_alive|valid_remote_path|shell_quotable|mirror_name_part|read_session_env' scripts/ tests/` must list only `scripts/og-remote-picker.sh`'s own `valid_remote_path` copy. `remote.bats`: delete the cases named in spec "Test changes" (if none remain, delete the file and its `bats` line). Mirror-name suite: drop the `source`, replace each `mirror_name_part X; v=$REPLY` with the literal mapped value (map every byte outside `[A-Za-z0-9_-]` to `_`, checked against `vectors.tsv` where a row exists).
  Proof: `bats tests/remote.bats` (if kept) → `0 failures`; `bats tests/remote-auth.bats tests/remote-theme.bats` → `0 failures`; `nix build .#checks.x86_64-linux.mirror-session-name-integration-tests` → success.

- [ ] **Step 15: docs and comments** — files `CLAUDE.md`, `docs/agents/scripts.md`, `docs/agents/bridge-daemon.md`, `docs/agents/picker.md`, `picker/mirrorname/mirrorname.go`, `picker/remote.go`, `picker/remote_zoxide.go`, `picker/remotebridge/graphics/fetch.go`.
  `rg -n 'mirror_name_part|lib-remote|shell_quotable|remote_daemon_alive|read_session_env|og-remote-open\.sh' CLAUDE.md docs/agents picker` and update each hit: behaviour text unchanged, mechanism now "`og-remote-open` (shim) → `og-remote-launcher` (`picker/remoteopen`)". Then `rg -n 'nohup|set -m|setsid|herestring|attached=1|EXIT trap|BASH_SOURCE' docs/agents/scripts.md docs/agents/picker.md docs/agents/bridge-daemon.md` and rewrite each hit that describes the launcher's bash mechanics (the nohup/`set -m` fallback, the herestring-vs-`$!` reasoning, the `attached=1` trap, the EXIT-trap lock release) for the Go launcher: `setsid(2)` on every platform, checkpoint-time signal rollback, the exit path's lock release; hits about other scripts stay. CLAUDE.md "Duplicated tables": drop the mirror-name pair; the sanitisation bullet keeps naming `og-remote-open`. `scripts.md` row: add one sentence on the shim + Go launcher.
  Proof: the `rg` above → only intentional mentions remain (`og-remote-picker.sh`'s `valid_remote_path`; `scripts/og-remote-open.sh` as the shim).

- [ ] **Step 16: full gates** — no new files.
  Proof: `nix build .#default` → success; `nix flake check` → success; `nix build .#lint --accept-flake-config` → success. Any failure → fix at the source (no suppressions except a per-line `//nolint:gosec // <why>` on `exec.Command` with a variable path, matching existing usage).

## Acceptance

- [ ] Seven bats suites pass against the Go build → `nix flake check` (`remote-tests` runs `remote-cold-start.bats`, `remote-picker.bats`; plus `remote-m2-integration-tests`, `remote-bridge-integration-tests`, `mirror-session-name-integration-tests`, `picker-launcher-tests`). Test edits: only `remote.bats` and `mirror-session-name-integration.bats`. Every `remote.bats` case tests a deleted shell helper or greps the deleted bash, so the file is expected to go; the PR body's "Test changes" section maps each deleted case to its Go replacement (`remote_daemon_alive` → `daemon_test` pidfile/alive cases; `valid_remote_path`, `shell_quotable` → `quote_test`; `mirror_name_part` vectors → `mirrorname` Go test; `read_session_env` → `local_test`; the three source greps → `local_test` recorded-argv guards).
- [ ] Go unit tests for argv parsing, refusals and the lock → `go test -race ./remoteopen/...` (Steps 4, 7), also inside `picker-go-tests`.
- [ ] `nix build .#lint --accept-flake-config` → Step 16.
- [ ] `nix flake check` → Step 16.
- [ ] `nix build .#default` → Step 16.
