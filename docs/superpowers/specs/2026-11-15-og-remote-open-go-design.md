# Port `og-remote-open` to Go — design

Issue #1107. `scripts/og-remote-open.sh` (~1,100 lines of bash) sits between the
Go picker (which builds its argv) and the Go bridge daemon (which it launches).
This spec moves its logic into a Go package in the `picker` module, keeps the
`og-remote-open` name and argv contract byte-for-byte, and deletes the shell
copy of the mirror session-name mapping (`mirror_name_part`), leaving
`mirrorname.Part` and `picker/mirrorname/testdata/vectors.tsv` as its only home.

## Goals

- G1. All launcher logic in Go: argv parsing and every refusal, the remote probe,
  cold start, restore, new-dir creation, the `--into` lock, daemon probe/reap,
  the mirror pair lookup and naming, daemon launch, signal rollback, the
  progress fd.
- G2. Same observable contract: argv, exit codes, every stderr message text,
  the progress-fd phase lines, the exact argv of every `tmux`/`ssh`/ctl/daemon
  invocation, the exact probe-script text sent to the remote, the daemon's
  environment, socket/lock/pid/phase/log file paths.
- G3. The seven named bats suites pass against the Go build; the only test
  edits are ones that read the deleted shell source or the deleted shell
  helpers (listed under "Test changes", each a test of implementation detail).
- G4. `mirror_name_part` deleted; `lib-remote.sh` keeps only what other scripts
  still source.
- G5. Go unit tests for argv parsing, refusals and the lock.

## Non-goals

- N1. Fish/`og` flag completion for `--into`/`--window`. The issue lists it as a
  motivation; `og`'s completion generator (`mkOg` in `config/tmux.conf.nix`)
  emits verbs only, and teaching it per-verb flags is a separate change. The Go
  parser added here is what such a change would hang off.
- N2. Moving the picker's argv construction onto shared Go types. The picker
  keeps building `[]string` argv as today; the launcher's argv contract is
  unchanged, so nothing on the picker side needs to move.
- N3. Behaviour changes. Every bash quirk below is preserved unless listed under
  "Deliberate deviations", each of which is unobservable to callers or tests.

## Shape

Two pieces, because the bats suites and the Nix build both feed the launcher
through `scripts/og-remote-open.sh` (they `sed` placeholders into it and run it
with `bash`), and a Go binary cannot carry store paths through `sed`:

1. **Go launcher** — package `picker/remoteopen` (all logic, unit-tested) and
   `picker/remoteopen/cmd/launcher` (a ~20-line `main`), built as a new
   `subPackages` entry in `picker/default.nix` and renamed in `postInstall` to
   **`og-remote-launcher`**, alongside `og-remote-bridge-{ctl,daemon,renderer}`.
   A distinct name keeps it from colliding with the `og-remote-open` script in
   the wrapper's merged bin dir (`wrapperBinDir`, whose precedence check is
   `wrapper-bin-merge-assertions`).
2. **Shim** — `scripts/og-remote-open.sh` shrinks to a bash shim that keeps the
   existing build-time placeholders and `exec`s the Go launcher:

   ```bash
   launcher="@remote_launcher@"
   [[ $launcher == @* ]] && launcher="${OG_REMOTE_LAUNCHER:-$(command -v og-remote-launcher)}"
   exec "$launcher" --self "${BASH_SOURCE[0]}" \
       --ctl "@bridge_ctl@" --daemon "@bridge_daemon@" --renderer "@bridge_renderer@" \
       --reflow "@reflow@" --loading "@loading@" -- "$@"
   ```

   `exec` keeps the pid, so the picker's group TERM, `Pdeathsig`, and the bats
   `kill -TERM $(cat $LAUNCHER_PID_FILE)` all reach the Go process.
   `--self` carries what `OG_DAEMON_REMOTE_OPEN="${BASH_SOURCE[0]}"` carried: the
   hand-off re-enters the shim at the daemon's own revision (#336).
   A flag value still starting with `@` means "unsubstituted": the Go side
   falls back to `exec.LookPath` of `og-remote-bridge-ctl`,
   `og-remote-bridge-daemon`, `og-remote-bridge-renderer`, `tmux-reflow-windows`,
   `og-remote-loading` (the last optional), exactly as the bash did.
   `OG_REMOTE_LAUNCHER` is read only while `@remote_launcher@` is unsubstituted
   (bats): the nohup-fallback case runs under a PATH of fakes and a few
   coreutils, where `command -v og-remote-launcher` cannot find the binary, so
   the flake checks export it. Production never consults it. When neither
   resolves (a local `bats` run with no built launcher), the shim prints
   `og-remote-open: og-remote-launcher not found (build picker/ or set
   OG_REMOTE_LAUNCHER)` and exits 1 instead of `exec`-ing an empty path.

   The internal flags (`--self` … `--`) are the shim↔launcher contract, not a
   user surface; everything after `--` is the public argv, parsed as below.

Nix wiring (`config/tmux.conf.nix`): `og-remote-open` leaves `scriptsWithRemote`'s
`@lib_remote@` dependency behind and gains `@remote_launcher@` →
`${picker-generate}/bin/og-remote-launcher` (added to `mkRemoteScript`'s
replacement list; `og-remote-auth`/`og-remote-theme` carry no such placeholder,
so it is a no-op for them). The flake checks that run the real launcher
(`remote-tests`, `mirror-session-name-integration-tests`) set
`OG_REMOTE_LAUNCHER = "${pickerChecked}/bin/og-remote-launcher"`. `pickerChecked`
must therefore build the new subpackage (it is the same `buildGoModule`).

## Process model (bash semantics the Go must reproduce)

- **External commands.** One runner helper. Children inherit the launcher's
  stdin, stdout and stderr unless the bash redirected them; `2>/dev/null`
  discards stderr; `$(…)` captures stdout and strips *all* trailing newlines.
  Commands are resolved through `PATH` at call time (`tmux`, `ssh`, `timeout`,
  ctl fallback), never cached across PATH changes (there are none, but tests
  swap PATH per case).
- **`set -e`.** A failing command that bash would exit on exits the launcher
  with that command's status (inventory below marks them `[set -e]`). A failing
  command guarded with `|| true`, inside `if`/`[[ ]]`/`&&`-left, is ignored.
- **Signals.** `TERM`, `INT`, `HUP` are trapped via `signal.Notify` at the top
  of `main`. Bash runs a trap only between commands, after the foreground child
  returns; the Go equivalent: a goroutine drains the Notify channel into an
  atomic "pending" flag, and a `checkpoint()` runs `onSignal` on the main
  goroutine when the flag is set. `checkpoint()` runs before starting and after
  finishing every external command and every poll sleep, explicitly right
  before each `attached = true` (F2, F8, F12), and right after the daemon
  `Start` returns. Rollback therefore never races an in-flight child (the
  property bash's wait-then-trap gave: a `kill-session` can never land before
  the `new-session` it undoes).
  Residual race, same in kind as bash's: `os/signal` forwards through a runtime
  goroutine, so a signal a child sent just before exiting can reach the flag
  one checkpoint late. The step that then runs is the next launcher step, and
  the explicit checkpoint before `attached` keeps it from crossing a commit
  point unchecked. The bats signal cases tolerate this:
  `FAKE_TERM_ON='set-option -t … @bridge_session'` has a dozen checkpoints
  before `switch-client`; `FAKE_TERM_ON=switch-client` exits 0 either way
  (attached is set before it); `FAKE_SSH_TERM` is followed by the probe parse
  and several commands. `onSignal`:
  - `attached` set → exit 0.
  - placed: if `daemonStarted` and a daemon pid exists → `reapDaemon(pid)`,
    `rm -f sock sock.pid sock.phase`; exit 143 (lock released by the exit path).
  - dedicated, `mirrorCreated` set: reap the started daemon if any;
    `tmux kill-session -t =<local_sess> 2>/dev/null`; `rm -f sock sock.pid
    phase_file`; exit 143.
  - otherwise exit 143. (All three signals exit 143, as the bash does.)
- **Exit path.** Every exit goes through one `exit(code)` that first releases
  the `--into` lock if this process holds it and `lock/owner` still reads our
  pid (the bash `trap release_lock EXIT`). Nothing uses `defer` across
  `os.Exit`.
- **Progress fd.** `OG_REMOTE_OPEN_PROGRESS_FD` matching `^[0-9]+$` is taken,
  then the variable is unset from the process environment (children never see
  it). The fd is adopted only if `fcntl(fd, F_GETFD)` succeeds at startup,
  before the launcher opens any fd of its own (signal setup and the first child
  come after), so a number with nothing open there is treated as unset and can
  never alias a pipe or `/dev/null` the launcher opens later (the bats case
  "an fd number with nothing open there"). An adopted fd is wrapped with
  `os.NewFile` and marked close-on-exec at startup
  (bash children inherited it incidentally; Go's fork would otherwise leak an
  inherited non-CLOEXEC fd into the setsid'd daemon). `phase(name)` writes
  `name\n`, ignoring every error (closed/never-opened fd). The fd is closed
  right before the daemon launch, as the bash `exec {fd}>&-` did.
- **Daemon launch.** `exec.Cmd` with `SysProcAttr{Setsid: true}` on every
  platform (replaces both `setsid` and the `set -m; nohup` fallback: either way
  the daemon leads its own session and process group), stdin `/dev/null`,
  stdout `/dev/null`, stderr `O_CREAT|O_TRUNC` `<sock>.log`. `daemonStarted` is
  set before `Start` (bash set it before the `&`). A goroutine `Wait`s the
  child so a daemon that dies is reaped and stops answering `kill -0` — bash
  reaped its background job on SIGCHLD; a Go zombie would keep answering
  `kill -0`, breaking the placed "exited before listening" check. The placed
  listen loop reads that goroutine's done channel for "exited".
- **Liveness.** `kill -0` = `syscall.Kill(pid, 0) == nil` (EPERM counts as
  dead, as bash's failing `kill -0` did). Pid files are read, trailing
  newlines stripped, and must match `^[0-9]+$` with value > 0 before any
  signal (see Deliberate deviations).

## Branch inventory (bash → Go)

Line numbers are `scripts/og-remote-open.sh` at 86a11d34. Each row names the Go
function it lands in (package `remoteopen`). Messages are byte-identical,
including `printf %q` renderings, which need a bash-compatible `%q` quoter
(`bashQuote`: bash's `printf %q` rules — `''` for empty, backslash-escaping of
shell metacharacters for printable input, `$'…'` with `\n`, `\t`, `\E`, octal
`\NNN` escapes once any control byte is present).

### A. argv and refusals — `ParseArgs(args []string, env Env) (Opts, error)`

Two pure functions, because the refusal order is observable and tmux reads
sit between them (B1, B2 and A12 all run before A11/A13–A18: a placed `sess`
holding a backslash with no such local session must report `no local session`
first):

- `ParseArgs(args []string, env Env) (Opts, error)` covers A1–A10.
- `Validate(o Opts, env Env) error` covers A11 and A13–A18; `main` calls it
  after B1, B2, the progress-fd setup and A12.

`env` is the subset of the environment they read (`OG_REMOTE_NEW_DIR`,
`OG_REMOTE_RESTORE`, `OG_REMOTE_TMPDIR`). Each returns a typed `*UsageError`
carrying the exact message; `main` prints it and exits 1. The first matching
refusal wins, in table order:

| # | bash | condition | message (after `og-remote-open: `) |
|---|------|-----------|------|
| A1 | 184 | `$1` unset | bash `set -u` aborts: `$1: unbound variable`, exit 1. Go: `usage: og-remote-open <host> [<sess> [<win>]]` — see Deliberate deviations |
| A2 | 194 | `sess == --into` | `--into needs both a remote session and a local session name` |
| A3 | 200-208 | any arg ≥3 is `--window`, none is `--into` | `--window needs --into` |
| A4 | 209 | `--window` present, `win != --into` and `$4 != --into` | the usage line `usage: og-remote-open <host> <sess> --into <local-session> [--window <selector>]` |
| A5 | 214 | `$4 == --into` (and `$3 != --into`) | `--into takes no window argument` |
| A6 | 218 | placed, `$# > 4` and not (`$# == 6` and `$5 == --window`) | the usage line |
| A7 | 225 | placed with `OG_REMOTE_NEW_DIR` or `OG_REMOTE_RESTORE` non-empty | `--into cannot be combined with OG_REMOTE_NEW_DIR or OG_REMOTE_RESTORE` |
| A8 | 229 | placed, `sess` or `into_name` empty | `--into needs both a remote session and a local session name` |
| A9 | 234 | placed, host not `^[A-Za-z0-9][A-Za-z0-9._@-]*$` | `--into needs a host matching [A-Za-z0-9][A-Za-z0-9._@-]*, got: <host>` |
| A10 | 238-245 | placed, `$# == 6`, selector matches neither `^@(0\|[1-9][0-9]*)$` nor `^@[A-Za-z_][A-Za-z0-9_-]{0,63}=[A-Za-z0-9._/@+:-]{1,255}$` | `--window must be @<id> or @<option>=<value>, got: <%q selector>` |
| A11 | 286 | non-placed `win` non-empty and not `^@?[0-9]+$` | `window must be a numeric index or an @id, got: <win>` |
| A13 | 312 | `win` starts `@` and not want_mode | `a window @id needs a live session to resolve against (not with OG_REMOTE_RESTORE, OG_REMOTE_NEW_DIR, or no session): <win>` |
| A14 | 321 | both env vars non-empty | `OG_REMOTE_NEW_DIR and OG_REMOTE_RESTORE are mutually exclusive` |
| A15 | 326 | `OG_REMOTE_NEW_DIR` holds `\` | `OG_REMOTE_NEW_DIR contains a backslash, which no remote shell dialect can quote safely: <dir>` |
| A16 | 353 | `requireSessionName(sess)`: any byte < 0x20 or 0x7f (bash `[[:cntrl:]]` under `LC_ALL=C`) | `session name contains a control character, which no tmux session name can: <%q> — pass an explicit session name instead` |
| A17 | 353 | `requireSessionName`: contains `\` | `session name contains a backslash, which no remote shell dialect can quote safely: <sess> — pass an explicit session name instead` |
| A18 | 358 | `OG_REMOTE_TMPDIR` set and fails `validRemotePath` (`^/[A-Za-z0-9._/@+:-]*$`) | `unusable remote tmpdir: <dir>` |

`Opts` carries: `Host`, `Sess`, `Win`, `Placed bool`, `IntoName`, `Selector`,
`WantMode bool` (win non-empty ∧ sess non-empty ∧ neither env var), `NewDir`,
`Restore`, `Tmpdir`. Extra positional args beyond `$3` on a non-placed open are
ignored, as in bash (`og-remote-open h s 1 junk` opens window 1).

A12 (`@og_bridge_predict`, line 293) is *not* pure: it reads
`tmux show-options -gqv @og_bridge_predict 2>/dev/null` (errors → empty) after
the placed local-session checks (B1–B2) and the progress-fd setup, and refuses
anything but `''|auto|on|off` with `@og_bridge_predict must be auto, on or off,
got: <%q value>`. Order relative to argv checks: after A11, before A13 — the
bash order is A1–A10, B1, B2, progress fd, A11, A12, A13–A18. The Go keeps it.

### B. placed local-session checks — `resolveInto(name) (id string, err)`

| # | bash | step |
|---|------|------|
| B1 | 248-257 | `tmux list-sessions -F '#{session_id}\|#{session_name}' 2>/dev/null` (errors → empty); first row whose text after the first `\|` equals `into_name` exactly gives the id. None → `no local session '<name>'`, exit 1 |
| B2 | 260-265 | `tmux show-options -qv -t <id> @bridge_host` and `@bridge_sock` (errors → empty); either non-empty → `'<name>' is a bridge mirror session; --into needs a local one`, exit 1 |

### C. probe script — `buildProbeScript(o Opts) string`

Byte-identical to the bash-built string (the bats fake `ssh` parses it with
`sed`, and the "real probe" cases run it under a real tmux). Built from the
same segments in the same order (lines 381-474); `shellQuote` is the bash
`shell_quote` (`'` + s with `'` → `'\''` + `'`). Segment choice:

- C1 tmpdir: `OG_REMOTE_TMPDIR` set → `tmpdir_lit=<q>` / `tmpdir="$tmpdir_lit"`;
  else the `case "$os" in Darwin) tmpdir="/tmp" ;; *) tmpdir="/run/user/$uid" ;; esac` block.
- C2 sess: non-empty → `sess_lit=<q>` / `sess="$sess_lit"`, plus the
  exact-then-prefix canonicalisation block only when not new-dir, not restore,
  not want_mode, not placed; empty → `sess=$(… list-sessions … | head -1)`.
- C3 win: want_mode → `want_lit=<q>` + the active/want resolution block;
  placed → the exact active-window line; caller window → `win_lit=<q>` /
  `win="$win_lit"`; else the active-window block.
- C4 the trailing `printf` of the seven `key=value` lines.

A golden test pins the Go output for each of the C1×C2×C3 combinations
against fixtures captured from the bash at 86a11d34 (captured once while the
bash still exists, committed under `picker/remoteopen/testdata/probe/`).

### D. placed lock + live check — `acquireLock`, `probeDaemon`, `reapDaemon` (lines 479-566)

- D1 key: `server_pid = tmux display-message -p -t <into_id> '#{pid}'` `[set -e]`.
  `sock_key` = POSIX `cksum` CRC of `host\0sess\0server_pid\0into_id`
  (`\0selector` appended when a selector is set). Go implements POSIX `cksum`
  (CRC-32/CKSUM: poly 0x04C11DB7, MSB-first, length bytes appended, final
  complement) in `cksum.go`, unit-tested against vectors from coreutils
  `cksum`. `sock = <base>/og-into-<crc>.sock`, `<base>` =
  `TMUX_TMPDIR` if non-empty, else `XDG_RUNTIME_DIR` if non-empty, else `/tmp`.
  `lock = sock + ".lock"`.
- D2 knobs: `OG_INTO_PROBE_TIMEOUT` (default 15), `OG_INTO_LISTEN_TIMEOUT`
  (default 60); `listenTicks = listen*10`; `lockTicks = (probe+listen+15)*10`;
  tick = 100 ms.
- D3 ctl resolution (placed only, here; dedicated resolves at F1).
- D4 lock loop, per tick:
  1. `mkdir lock` succeeds → `lockHeld`, write `<pid>\n` to `lock/owner`
     (`[set -e]`: a failed write exits 1; the exit path then leaves the lock,
     whose owner is not ours), done.
  2. `tick >= lockTicks` → `probeDaemon(sock)`; `live` → exit 0; else
     `timed out waiting for another --into of <host>:<sess> into '<name>'`
     + ` for --window <sel>` when a selector is set; exit 1.
  3. `ident = lockIdent()`: `pid:<owner>` when `lock/owner` reads non-empty,
     else `ino:<inode>` of `lock`, else empty (gone). On change from the
     remembered ident: remember it, reset `staleTicks`. `staleTicks++`.
  4. stale = (`pid:N` and `kill -0 N` fails) or (`ino:X`, X non-empty, and
     `staleTicks >= 50`).
  5. stale and `mkdir lock.break` succeeds → re-read ident; still equal →
     `mv lock lock.stale.<pid> 2>/dev/null`, **run as the PATH `mv`**, and
     only on mv success `rm -rf` it (`os.RemoveAll`); `rmdir lock.break`;
     forget the ident; next tick without sleeping.
  6. stale and the mutex is held by someone else → `lock.break` older than 5 s
     (mtime) → `rmdir lock.break`.
  7. sleep one tick.
  The lock is a type (`intoLock{path, pid, clock, sleep, alive, rename, ticks}`)
  so unit tests drive it with a fake clock/sleep, fake liveness and a rename
  hook. Production's `rename` execs PATH `mv`, not `os.Rename`: the bats case
  "two waiters on a dead owner's lock break it once" stalls the first rename
  through a fake `mv` on PATH to prove the breaker mutex, and asserts the fake
  ran. Keeping `mv` keeps that suite unchanged and the race covered end to
  end; the unit test covers the same race through the hook.
- D5 under the lock: `probeDaemon(sock)` `live` → exit 0. Non-empty (outdated)
  → read `sock.pid` `[set -e]`, digits → `reapDaemon`. Then
  `rm -f sock sock.pid sock.phase`.
- `probeDaemon(sock)`: `""` unless `remoteDaemonAlive(sock.pid)` (file exists,
  content non-empty, `kill -0` ok); then `ctl --sock <sock> ping _` with
  stdout+stderr combined: success → `live`; output contains
  `— reopen the bridge` → `outdated`; else `""`.
- `reapDaemon(pid)`: `kill -TERM pid` (errors ignored), up to 20 × 100 ms
  `kill -0` polls, then `kill -KILL`.

### E. probe, cold start, restore, new-dir (lines 568-776)

| # | bash | step |
|---|------|------|
| E1 | 573 | `phase connect` |
| E2 | 574-588 | placed: `ssh -T -o ConnectTimeout=<p> -o ServerAliveInterval=5 -o ServerAliveCountMax=3 <host> bash -s`, wrapped as `timeout <p> ssh …` when `timeout` is on PATH; else `ssh -T <host> bash -s`. Stdin = probe script + `\n` (herestring). Stdout captured `[set -e]` (ssh's, or timeout's 124, status). stderr inherited |
| E3 | 590-601 | parse stdout lines `os= uid= tmux= tmpdir= sess= win= want=` (last occurrence wins; other lines ignored) |
| E4 | 606 | non-placed only: a non-empty probe sess replaces `sess` (placed keeps the caller's name, which its socket key was built from) |
| E4b | 607 | every mode: an empty `win` takes the probe win (a placed open's `OG_BRIDGE_WINDOW` is the probe's active window) |
| E5 | 611 | `requireSessionName(sess)` (A16/A17) |
| E6 | 613 | `validRemotePath(remote_tmpdir)` else `unusable remote tmpdir: <v>` |
| E7 | 618-627 | want_mode: empty probe win → `session '<sess>' has no window on <host> — it is gone or was never there`; else `win = want` or, empty, probe win |
| E8 | 631 | placed and empty probe win → `no session named exactly '<sess>' on <host>` |
| E9 | 646-664 | `startRemoteServer`: `phase start-server`; Darwin → `ssh <host> -- launchctl kickstart gui/<uid>/org.nix-community.home.tmux-startup` (desc `tmux-startup launchd agent`); else `ssh <host> -- systemctl --user restart tmux-startup.service` (desc `tmux-startup.service`). Failure → `<host> has no tmux server, and no <desc> to start one`, exit 1 |
| E10 | 666-677 | empty sess: E9, then `sess = firstRemoteSession()` `[set -e]`; empty → `started <desc> on <host> but no session appeared`; then E5 |
| E11 | 683-716 | restore (`OG_REMOTE_RESTORE` ∧ sess): `phase restore`; `hasSession` (ssh `env TMUX_TMPDIR=<t> <tmux> has-session -t '<=sess quoted>'`, stderr discarded); absent → firstRemoteSession empty (errors ignored) → E9 + `phase restore`; `remux = ssh <host> 'command -v tmux-remux 2>/dev/null \|\| echo /etc/profiles/per-user/$(id -un)/bin/tmux-remux'` `[set -e]`; `ssh -T <host> bash -s` with stdin `env TMUX_TMPDIR=<t> PATH=<dirname tmux>:$PATH <remux> restore\n`, failure → `tmux-remux restore failed on <host>`; hasSession again, absent → `session '<sess>' was not restored on <host> — tmux-remux's restore filter may have skipped it (idle shells / stale age)` |
| E12 | 720-726 | `initialMirrorArea()`: `tmux display-message -p [-t $TMUX_PANE] '#{client_width} #{client_height} #{status}' 2>/dev/null` (errors → empty), whitespace-split; width/height `^[1-9][0-9]*$` else none; status `off`→0, `on`→1, digits→n, else 1; height-status > 0 → (w, h-status) |
| E13 | 734-760 | new-dir (`OG_REMOTE_NEW_DIR` ∧ sess): `phase create`; hasSession; absent → firstRemoteSession empty → E9 + `phase create`; `ssh <host> "env TMUX_TMPDIR=<t> <tmux> new-session -d -s <q sess> -c <q dir>[ -x W -y H]"` failure → `could not create session '<sess>' in '<dir>' on <host>`; hasSession absent → `session '<sess>' was not created on <host>` |
| E14 | 762-776 | empty win: `phase connect`; `win = ssh <host> "env TMUX_TMPDIR=<t> <tmux> list-windows -t <q sess> -F '#{window_index} #{window_active}' \| awk '\$2==1{print \$1; exit}'"` `[set -e]`; empty → the E7 "has no window" message |
| — | 370 | `firstRemoteSession()` = `ssh <host> "env TMUX_TMPDIR=<t> <tmux> list-sessions -F '#{session_name}' \| head -1"` |

`dirname` is a POSIX `dirname` (trailing slashes stripped, no slash → `.`,
all slashes → `/`), not `filepath.Dir`.

### F. launch (lines 778-866 placed, 868-1096 dedicated)

- F1 `phase mirror`; resolve ctl/daemon/renderer/reflow (each `[set -e]` on a
  PATH miss: exit 1) and loading (optional, empty on miss).
- F2 placed (lock still held): daemon env = launcher env plus
  `OG_BRIDGE_HOST/SESSION/WINDOW/TMUX/TMPDIR`, `OG_DAEMON_LOCAL_SESS=<into_name>`,
  `OG_DAEMON_INTO=<into_id>`, `OG_DAEMON_SOCK`, `OG_DAEMON_RENDERER`,
  `OG_DAEMON_REFLOW`, `OG_DAEMON_SELECT=<selector>` (set even when empty),
  `OG_DAEMON_PREDICT`, `OG_BRIDGE_TERM/TERMFEATURES/SIXEL/CELL_W/CELL_H`,
  `OG_BRIDGE_COLORTERM/TERM_PROGRAM`; `OG_DAEMON_REMOTE_OPEN` removed.
  Close the progress fd; launch (Process model); listen loop `listenTicks`
  × 100 ms: `probeDaemon == live` → `attached`, exit 0; daemon exited →
  `the bridge daemon for <host>:<sess> exited before listening:` then the last
  20 lines of `sock.log` on stderr, exit 1. Loop exhausted →
  `the bridge daemon for <host>:<sess> was not listening after <N>s`, reap,
  `rm -f sock sock.pid sock.phase`, exit 1.
- F3 terminal identity (both modes): `tmux display-message -p [-t $TMUX_PANE]
  '#{client_termname}|#{client_termfeatures}|#{I/f:sixel}|#{client_cell_width}|#{client_cell_height}' 2>/dev/null`
  (errors → empty), first line, split on `|` into 5 (missing → empty). The 5th
  field takes the remainder `R` verbatim, except that when `R` ends in `|` and
  `R` without that last byte holds no `|`, the trailing `|` is dropped: the
  `IFS='|' read` rule (`z|` → `z`, `|` → empty, `z|w|` and `z||` unchanged).
  `cur_sess = tmux display-message -p [-t $TMUX_PANE] '#{session_name}' 2>/dev/null`.
  `readSessionEnv(cur_sess, NAME)`: cur_sess empty → miss; `tmux
  show-environment -t <cur_sess> NAME 2>/dev/null` non-zero → miss; output
  (trailing newlines stripped) starting `NAME=` → the rest; else miss.
  COLORTERM and TERM_PROGRAM, miss → empty.
- F4 dedicated name: `host_part = mirrorname.Part(host)`, `sess_part =
  mirrorname.Part(sess)`, `base = host_part + "-" + sess_part`.
- F5 pair lookup: `tmux list-sessions -F
  '#{session_id}|#{?#{@bridge_placed},,#{@bridge_host}}|#{?#{@bridge_placed},,#{@bridge_session}}' 2>/dev/null`
  (errors → empty). Per line: id = before first `|`; host = next field;
  session = everything after the second `|` (so `x|` stays `x|`). Skip unless
  (host, session) equals (host, sess). Verify with session-scoped
  `show-options -qv -t <id> @bridge_host` / `@bridge_session` (errors →
  empty) equal to the pair, else skip. `pair_name = display-message -p -t <id>
  '#{session_name}'` (errors → empty). `Part(pair_name) != pair_name` →
  `retireMirror(id)`; else the first such name becomes `local_sess`.
- `retireMirror(id)`: `old = show-options -qv -t <id> @bridge_sock`; non-empty
  → `probeDaemon(old)` non-empty → read `old.pid` `[set -e]`, digits → reap;
  `tmux kill-session -t <id> 2>/dev/null`; non-empty old → `rm -f old
  old.pid old.phase`.
- F6 collision walk (no local_sess yet): `local_sess = base`, `n = 0`; while
  `tmux has-session -t =<local_sess> 2>/dev/null` succeeds: read `show-options
  -t <local_sess> -qv @bridge_host` / `@bridge_session` (bare target, never
  `=`, #474; errors → empty); stop if (host, session) match, or host matches,
  session empty, `local_sess == base` and `base == host + "-" + sess`
  (legacy adoption); else `n++`, `local_sess = base-remote` (n = 1) or
  `base-remote-<n>`.
- F7 `sock = <base dir>/og-daemon-<local_sess>.sock`; `phase_file = sock.phase`.
- F8 `probeDaemon(sock)` `live` and `tmux has-session -t =<local_sess>` →
  `attached`, `tmux switch-client -t =<local_sess>`, exit with its status.
  Non-empty → read `sock.pid` `[set -e]`, digits → reap.
- F9 `mirrorCreated`; `rm -f sock sock.pid phase_file`; `tmux kill-session -t
  =<local_sess> 2>/dev/null`.
- F10 write `connecting to <host>\n` to `phase_file` `[set -e]`; `tmux
  new-session -d -s <local_sess> -n <sess_part> [-x W -y H] [-- <loading>
  <host> <sess> <phase_file>]` `[set -e]`; `tmux set-option -t <local_sess>
  @bridge_session <sess>` then `@bridge_host <host>` `[set -e]` (session
  first: a concurrent reader must never see host set and session empty).
- F11 daemon env = launcher env plus the F2 set with
  `OG_DAEMON_LOCAL_SESS=<local_sess>`, no `OG_DAEMON_INTO` change (inherited
  value kept, as bash never touched it), `OG_DAEMON_SELECT` removed,
  `OG_DAEMON_REMOTE_OPEN=<--self>`. Close the progress fd; launch.
- F12 `attached`; `tmux switch-client -t =<local_sess>`; exit with its status.

Order inside F2/F11 matches the bash: env and identity reads before the fd
close, the fd close before the launch.

## Deliberate deviations (unobservable to callers and tests)

1. Missing `$1`: bash dies on `set -u` with `line 184: $1: unbound variable`;
   Go prints a usage line and exits 1. Same status; the bash text named a
   script line that no longer exists.
2. A pidfile whose content is not all digits or is `0` reads as dead before
   any `kill`. Bash would `kill -0 -1` (succeeds: every process) on a `-1`
   pidfile and go on to ping. Only a forged pidfile reaches this.
3. The progress fd is close-on-exec for every child, not only the daemon
   (bash let `ssh`/`tmux` inherit it; nothing reads it there, and a persisted
   ssh ControlMaster holding it would delay the picker's EOF).
4. A PATH miss for a bridge binary exits 1 with
   `og-remote-open: <name> not found on PATH` instead of bash's silent `set -e`
   exit 1.
5. Daemon detach always uses `setsid(2)`; the `setsid(1)` / `set -m nohup`
   split disappears. Both bash branches produced a daemon leading its own
   process group, which is what the nohup-fallback test checks.
6. `bashQuote` renders `printf %q` as bash does under `LC_ALL=C` for every
   message (A10, A12, A16). Bash used the caller's locale for A10/A12, so a
   non-ASCII byte in an invalid selector or predict value printed literally
   under UTF-8; Go prints `$'\303\251'`. Only already-refused, non-ASCII input
   reaches this; the bats inputs are ASCII.

## Deletions and file changes

- `scripts/og-remote-open.sh` → the shim (above), with a comment saying where
  the logic lives.
- `scripts/lib-remote.sh`: delete `remote_daemon_alive`, `valid_remote_path`,
  `shell_quotable`, `mirror_name_part`, `read_session_env` (only
  `og-remote-open` sourced them; `og-remote-picker` has its own
  `valid_remote_path`). `remote_auth_identity` stays for `og-remote-auth`.
  Check `og-remote-theme` sources nothing deleted.
- `picker/mirrorname/mirrorname.go`: drop the "byte-identical to
  mirror_name_part" clause; vectors stay the oracle.
- `CLAUDE.md`: drop the mirror-name pair from "Duplicated tables"; the
  sanitisation bullet keeps naming `og-remote-open`.
- `docs/agents/scripts.md` (`og-remote-open` row), `docs/agents/bridge-daemon.md`
  (lines naming `lib-remote.sh`/`mirror_name_part`/bash mechanics),
  `docs/agents/picker.md` (`shell_quotable` reference): point at the Go
  package; behaviour text unchanged.
- `picker/remote.go:1358` comment naming `remote_daemon_alive`: repoint at
  `remoteopen`.
- `picker/remote_zoxide.go:84` and `picker/remotebridge/graphics/fetch.go:178`
  comments naming `shell_quotable`: repoint at `remoteopen`'s equivalent.
- `flake.nix` `remote-tests`: drop `MIRROR_NAME_VECTORS` and its comment (no
  shell half is left to loop the vectors) and add `OG_REMOTE_LAUNCHER`; update
  the `mirror-session-name-integration-tests` comment that calls the launcher
  a script.

## Test changes (each reads deleted shell, not behaviour)

- `tests/remote.bats`: delete the `remote_daemon_alive`, `valid_remote_path`,
  `shell_quotable`, `mirror_name_part` vector and `read_session_env` cases
  (helpers deleted; Go unit tests cover each, and `mirrorname`'s Go test
  already loops the vectors). Delete the three source-grep cases too (#474
  bare `show-options` target, no `display-message` read of `@bridge_*`, the
  placed-aware pair-lookup format): they grep bash syntax in a file that no
  longer holds the logic, and `remote-tests` copies only `scripts/` and
  `tests/`, so `picker/` is not there to grep. Each moves to a Go unit test
  asserting the argv a recording runner captured (below). If the file is left
  with no cases, delete it and its `bats tests/remote.bats` line.
- `tests/mirror-session-name-integration.bats`: it sources `lib-remote.sh` only
  to compute expected names with `mirror_name_part`; replace those calls with
  the literal expected names (derived from `vectors.tsv`'s rule) and drop the
  `source`.
- No other suite changes. `remote-cold-start.bats` and the integration suite
  keep running `bash "$LAUNCHER"` on the `sed`-ed shim.

## Go unit tests (`picker/remoteopen/*_test.go`)

- `ParseArgs`/`Validate`: table over A1–A18 (each refusal and its exact message, the
  ordering between overlapping refusals, the accepted shapes: dedicated with
  and without win, `@id` win in want_mode, placed with/without selector).
- `bashQuote` against `printf %q` vectors captured from bash.
- `buildProbeScript` golden fixtures (C).
- `cksum` vectors; `sockKey` against the bats `into_sock` formula.
- Lock (D4): acquire on a free path; a dead-owner lock broken; an owner-less
  lock broken only after 50 ticks; an ident change resets the stale count; a
  live owner waited out to the timeout; a `.break` older than 5 s removed and
  a fresh one left alone; release only while `owner` reads our pid.
- `probeDaemon`/`reapDaemon` with a fake ctl and a real child process.
- Recorded-argv guards (replacing the three deleted source greps), through a
  runner fake that records each argv: the F6 collision walk's `show-options`
  uses a bare `-t <name>`, never `-t =<name>` (#474); every `@bridge_*` read is
  a `show-options`, never a `display-message`; the F5 pair lookup's `-F`
  argument is exactly
  `#{session_id}|#{?#{@bridge_placed},,#{@bridge_host}}|#{?#{@bridge_placed},,#{@bridge_session}}`.
- `splitPairLine`, `initialMirrorArea` parsing, `splitTermIdentity`,
  `readSessionEnv` parsing, `posixDirname`, `validRemotePath`,
  `requireSessionName`.

## Acceptance mapping

| Acceptance item | Evidence |
|---|---|
| seven bats suites pass against the Go build | `nix flake check` (`remote-tests`, `remote-m2-integration-tests`, `remote-bridge-integration-tests`, `mirror-session-name-integration-tests`, `picker-launcher-tests`) — remote.bats/remote-picker.bats run inside `remote-tests` |
| Go unit tests for argv, refusals, lock | `go test -race ./remoteopen/...` inside `picker-go-tests` |
| lint | `nix build .#lint --accept-flake-config` |
| flake check | `nix flake check` |
| build | `nix build .#default` |
