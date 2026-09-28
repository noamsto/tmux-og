# Plan: open URLs on the controlling host via og-open (#854)

Spec: `docs/superpowers/specs/2026-09-28-og-open-controller-url-design.md`
(accepted). Every step below implements a section of it; where a step says
"per spec §N" the spec's text is the exact behaviour.

## File list

| File | Purpose |
|---|---|
| `scripts/og-open.sh` (new) | The opener: validate, bridged test, append/replace the record, or unset BROWSER + exec the platform opener (spec §1). |
| `tests/og-open.bats` (new) | Unit tests for `og-open` with `tmux`/`xdg-open`/`open`/`uname` stubbed on PATH. |
| `config/tmux.conf.nix` | Add `og-open` to `scriptNames` and `ogInternal`. |
| `config/tmux.conf.tmpl` | `set-environment -g BROWSER "<og-open store path>"` (spec §2). |
| `config/tmux.conf.reference.nix` | The same line, byte-identical in the rendered output (frozen reference). |
| `generator/paths/paths.go` | Add `og-open` to `RequiredScripts` (the template's script sites; `--prefix`/`og init` renders fill `Scripts` only from it). |
| `flake.nix` | New `og-open-tests` check; `OG_OPEN` input on `remote-m2-integration-tests`; a positive `BROWSER` grep in the `--prefix` smoke. |
| `picker/remotebridge/daemon/openurl.go` (new) | `parseOpenRecords`, `validOpenURL`, `urlOpener` (connect/handle), `BrowserOpener`/`runBounded` (spec §3). |
| `picker/remotebridge/daemon/openurl_test.go` (new) | Go unit tests for all of the above. |
| `picker/remotebridge/daemon/subscriptions.go` | `openSubName = "og_open"` and `openURLFormat` constant beside the others. |
| `picker/remotebridge/daemon/daemon.go` | `Config.OpenURL` field; build the opener beside `res`/`usage`; call `connect` from the `subscribe` closure; route `og_open` notifications in `dispatch`. |
| `picker/remotebridge/daemon/ctl.go` | `toolResolveScript` restores `BROWSER` from the tmux global env (spec §2). |
| `picker/remotebridge/daemon/ctl_test.go` | `TestToolBrowserRestore` (run for real under `/bin/sh` against a stub, like `TestToolPathRestore`). |
| `picker/remotebridge/cmd/daemon/main.go` | `OpenURL: daemon.BrowserOpener(runtime.GOOS)`. |
| `tests/remote-m2-integration.bats` | One integration test: single, bulk of 3, no replay across a drop+reattach, one more after. |
| `docs/agents/bridge-shipped-state.md` | New "URL Opens (remote → controller)" section. |
| `docs/agents/bridge-daemon.md` | PATH-table row for `og-open`; reconnect note that repair re-seeds and re-registers. |
| `docs/agents/scripts.md` | `og-open` row. |
| `CLAUDE.md` | Deep-dive table row for `bridge-shipped-state.md` mentions URL opens. |
| `docs/superpowers/specs/2026-09-28-og-open-controller-url-design.md`, this plan | Committed with the code (CLAUDE.md "Plans and Specs"). |

Nothing else. Explicitly untouched (owned by other workers): `scripts/lib-claude.sh`,
`claude-status-update.sh`, agent-usage scripts, `picker/statusline/usage.go`,
`picker/remote.go`, `picker/remotebridge/controlmode/layout.go`.

## Steps

All commands run from the worktree root inside the devshell (direnv). Go
commands run from `picker/`.

### A. og-open script

- [ ] **Step 1: write the failing bats suite** — `tests/og-open.bats`.
  `setup` builds `$BATS_TEST_TMPDIR/bin` with stubs and prepends it to PATH:
  - `tmux` stub: appends `"$*"` as one line to `$BATS_TEST_TMPDIR/tmux.log`;
    for a `list-clients` call prints `$TMUX_STUB_CLIENTS` (default empty) and
    exits `${TMUX_STUB_RC:-0}`; for `set-option` exits 0.
  - `xdg-open` and `open` stubs: append `"<name> $* BROWSER=${BROWSER-unset}"`
    to `$BATS_TEST_TMPDIR/opened`.
  - `uname` stub: prints `${UNAME_STUB:-Linux}`.
  `SCRIPT="${OG_OPEN:-$BATS_TEST_DIRNAME/../scripts/og-open.sh}"`, run as
  `run bash "$SCRIPT" …`.
  Cases (each asserts status, `tmux.log`, `opened`):
  1. rejects with exit 2 and neither log touched: no arg; two args;
     `ftp://x`; `file:///etc/passwd`; `javascript:alert(1)`; `https://`;
     `http://`; `HTTPS://x`; `https://a b`; `$'https://a\tb'`;
     `$'https://a\nb'`; `https://a;`; a 4097-byte `https://` URL; a URL
     of 4000 ASCII bytes plus 50 `é` (100 bytes, 50 characters — over the
     byte cap, under it in characters), run with `LC_ALL=C.UTF-8`.
  2. accepts a 4096-byte URL (boundary) on the unbridged path.
  3. bridged: `TMUX=/tmp/s,1,0 TMUX_PANE=%3 TMUX_STUB_CLIENTS=$'0|10\n1|10'`
     → exit 0; `tmux.log` line 1 is exactly
     `list-clients -t %3 -F #{?#{&&:#{client_control_mode},#{==:#{client_name},#{@og_open_client}}},1,0}|#{n:@og_open_url}`;
     line 2 matches `^set-option -a -t %3 @og_open_url  [0-9]+-[0-9]+\|https://example.invalid/x$`
     (two spaces: the separator between `@og_open_url` and the record's own
     leading space); `opened` absent.
  4. bridged over the cap: `TMUX_STUB_CLIENTS='1|4097'` → line 2 is
     `set-option -t %3 @og_open_url  <nonce>|<url>` (no `-a`). `1|4096` still
     appends.
  5. not bridged, each → `opened` is `xdg-open https://example.invalid/x BROWSER=unset`
     and `tmux.log` has no `set-option`: `TMUX` unset (with `BROWSER=og-open`
     exported, proving the unset); `TMUX` set, `TMUX_PANE` unset; only `0|10`
     rows; `TMUX_STUB_RC=1`.
  6. `UNAME_STUB=Darwin`, not bridged → `open https://example.invalid/x BROWSER=unset`.
  Proof: `bats tests/og-open.bats` → every case fails (script missing).

- [ ] **Step 2: implement `scripts/og-open.sh`** per spec §1: `#!/usr/bin/env bash`
  header, POSIX body (`case` globs, `${#url}`, no arrays/`[[`), a short
  header comment stating the contract. Validation order: arg count, scheme
  `case "$url" in http://?* | https://?*)`, then `*[[:space:][:cntrl:]]*`
  and `*\;` rejects, then the byte length: `${#url}` counts characters under a
  UTF-8 locale, so bytes are counted with `printf %s "$url" | wc -c` (never
  an exported `LC_ALL=C`, which the exec'd opener — and a browser it starts —
  would inherit) and `> 4096` rejects. Bridged test: capture the
  `list-clients` output (stderr to /dev/null) into a variable, then scan it
  with `while IFS='|' read -r ok len; do …; done <<EOF` (a heredoc, never a
  pipe — a piped `while` runs in a subshell and loses `ok`/`len`); bridged
  iff a row has `ok = 1`, keeping that row's `len`. Record `" $(date +%s)-$$|$url"`; `set-option` with `-a`
  unless `len -gt 4096`; `exec tmux …` so the exit status is tmux's.
  Unbridged: `unset BROWSER`; `[ "$(uname -s)" = Darwin ] && exec open "$url"`;
  `exec xdg-open "$url"`.
  Proof: `bats tests/og-open.bats` all green; `shellcheck scripts/og-open.sh`
  clean; `shfmt -d scripts/og-open.sh tests/og-open.bats` empty.

- [ ] **Step 3: package it** — `config/tmux.conf.nix`: add `"og-open"` to
  `scriptNames` (after `"og-remote-theme"`) and to `ogInternal` (a
  `$BROWSER` target, no verb). `flake.nix`: add `og-open-tests` beside
  `reflow-tests` — `runCommand` with `nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.bash]`,
  `cp -r ${./scripts} scripts; cp -r ${./tests} tests; bats tests/og-open.bats`.
  Proof: `nix build .#checks.x86_64-linux.og-open-tests` succeeds;
  `nix build .#default` succeeds and the wrapped tmux resolves it on its own
  PATH: `TMUX_TMPDIR=$(mktemp -d) CLAUDE_STATUS_DIR=$(mktemp -d) result/bin/tmux -L ogprobe new-session -d 'command -v og-open >"$TMUX_TMPDIR/where"; sleep 1'`,
  then `grep -q '/bin/og-open$' "$TMUX_TMPDIR/where"`, then `kill-server` on
  that socket.

### B. Config

- [ ] **Step 4: BROWSER in the global env** — in `config/tmux.conf.tmpl`,
  directly after the `set -ga update-environment AEYE_HOST` line, add a
  blank-line-separated block:
  ```
  # $BROWSER-aware tools (prdash, gh --web) in every server-started pane go
  # through og-open, which forwards to the controlling host when this session
  # is bridged and is the platform opener otherwise. A store path, not the
  # bare name: a resident server's PATH predates a rebuild (#407).
  set-environment -g BROWSER "{{index .Paths.Scripts "og-open"}}"
  ```
  and the byte-identical rendered text in `config/tmux.conf.reference.nix`
  with `"${script.og-open}/bin/og-open"`. Add `"og-open"` to
  `generator/paths/paths.go` `RequiredScripts`, sorted (after
  `"og-notify-center"`): `FromPrefix` fills `Scripts` only from that list, and
  a template `index` on a missing key renders `""` without error, so without
  it every `--prefix`/`og init` render would ship `BROWSER ""`. In `flake.nix`'s
  `--prefix` smoke, after the `/opt/tmux-og/bin/` grep, add a positive
  `grep -Fq 'set-environment -g BROWSER "/opt/tmux-og/bin/og-open"' prefixout/tmux.conf`
  failing with a named message.
  Proof: `go test ./paths/... ./...` in `generator/` green;
  `nix build .#checks.x86_64-linux.tmux-conf-extraction-assertions`
  succeeds (reference diff clean across the matrix, prefix smoke finds the
  line);
  `grep -c 'set-environment -g BROWSER "/nix/store/.*/bin/og-open"' <generated conf>`
  = 1 where the generated conf is `nix build .#default` then the `-f` path
  scraped from `result/bin/tmux`.

### C. Daemon (Go)

- [ ] **Step 5: failing Go tests** — `picker/remotebridge/daemon/openurl_test.go`,
  written against the API below (it will not compile until Step 6):
  - `TestParseOpenRecords` — `" 1-2|https://a/x 1-3|https://b/y"` → two;
    `"garbage 1-4|https://c"` → one; `"x-1|https://a"`, `"1-|https://a"`,
    `"|https://a"`, `"12|https://a"` → malformed; `"1-5|https://a/b|c"` → URL
    `https://a/b|c`; `""` → none.
  - `TestValidOpenURL` table — accept `http://a`, `https://github.com/o/r/pull/1#issuecomment-2`,
    `https://x/%20y`, `https://例え.jp/`; reject `file:///etc/passwd`,
    `javascript:alert(1)`, `HTTPS://a`, `-https://a`, `https://`, `http:///x`,
    `https://a b`, `"https://a\tb"`, `"https://a\x00"`, `"https://a\x7f"`,
    a 4097-byte URL, `""`.
  - `TestURLOpenerOpensOnlyUnseen` — handle `$1` value `{a,b}` opens a,b; then
    `{a,b,c}` opens only c; then `{c}` (cap reset) opens nothing; then `{c,d}` opens d.
  - `TestURLOpenerSkipsInvalid` — a `file:` record and a malformed token in
    the same value as a good one: only the good one opens.
  - `TestURLOpenerIgnoresForeignSession` — `session: "$1"`, handle `"$2"` with an
    unseen record → nothing opens and `seen` unchanged; `session: ""` accepts any.
  - `TestURLOpenerConnectOrder` — a scripted `roundTrip` recording commands:
    seed replies `" 1-1|https://old"`; subscribe replies End; register replies End.
    Assert issued == `show-options -qv -t '$1' @og_open_url`,
    `refresh-client -B 'og_open::#{@og_open_url}'`,
    `set-option -F -t '$1' @og_open_client '#{client_name}'` in that order;
    then handle the subscribe-time report `" 1-1|https://old 1-2|https://new"` →
    opens only `https://new` (appended between seed and subscribe) and never
    `https://old`.
  - `TestURLOpenerReconnectReplaysNothing` — after the above, connect again
    (new rt) whose seed returns `" 1-1|https://old 1-2|https://new"`, then
    handle the same value → nothing opens.
  - `TestURLOpenerConnectFailures` — seed `%error` → one command issued, one
    notify naming `seed`; subscribe `%error` → two commands, one notify naming
    `subscribe`; register `%error` → three commands, one notify naming
    `register`; seed with no reply (rt returns false) → treated as seed failure.
  - `TestURLOpenerUnknownPinUntargeted` — `session: ""` issues
    `show-options -qv @og_open_url` and `set-option -F @og_open_client '#{client_name}'`.
  - `TestURLOpenerNilOpenURLIsOff` — `newURLOpener(Config{}, …)` returns nil;
    a nil `*urlOpener`'s `connect`/`handle` are no-ops (no command issued).
  - `TestURLOpenerOpenErrorNotifies` — `open` returns an error → one notify
    containing the URL.
  - `TestOpenSubscriptionSurvivesParseLine` —
    `controlmode.ParseLine("%subscription-changed og_open $1 - - - :  1-1|https://a 1-2|https://b|c")`
    → `subscriptionValue(l, openSubName)` yields a value `parseOpenRecords`
    turns into both records with `l.Args[1] == "$1"`.
  - `TestBrowserOpenerArgv` — `browserOpenerName("darwin") == "open"`,
    `("linux") == "xdg-open"`; `envWithout([]string{"A=1","BROWSER=x","BROWSERX=2"}, "BROWSER")`
    == `{"A=1","BROWSERX=2"}`.
  - `TestRunBounded` — `runBounded(exec.Command("sh","-c","exit 3"), time.Second)`
    returns a non-nil error; `exec.Command("sh","-c","sleep 5")` with a 100ms
    bound returns nil in < 1s; a missing binary returns an error; and a child
    whose env is checked: `sh -c 'test -z "${BROWSER+x}"'` exits 0 with
    `BROWSER=x` set in the test's own env (`t.Setenv`).
  Tests inject `launch: func(f func()) { f() }` and capture `open`/`notify`
  calls in slices, so nothing is asynchronous in the unit tests.
  Proof: `go test ./remotebridge/daemon/ -run 'OpenRecords|ValidOpenURL|URLOpener|OpenSubscription|BrowserOpener|RunBounded'`
  fails to compile (undefined symbols).

- [ ] **Step 6: implement `openurl.go` + the subscription constant**
  (implement: escalated — the controller-side security boundary). Per spec §3:
  - `subscriptions.go`: `openSubName = "og_open"` in the const block; in
    `openurl.go` `const openURLFormat = "#{@og_open_url}"` and
    `openClientOpt = "@og_open_client"`; add `openURLFormat` to the
    no-quotes loop in `TestSubscribeCmdIsOneQuotedToken`.
  - `type openRecord struct{ nonce, url string }`;
    `parseOpenRecords(v string) []openRecord` (`strings.Fields`, `strings.Cut`
    on `|`, nonce `^[0-9]+-[0-9]+$` via a package regexp).
  - `validOpenURL(u string) bool` — the four spec rules, in cheap-first order.
  - `type urlOpener struct { host, session string; seen map[string]bool; open func(string) error; notify func(string); launch func(func()) }`.
  - `newURLOpener(cfg Config, session string) *urlOpener` — nil when
    `cfg.OpenURL == nil`; `notify` = `func(m string){ notifyLocal(cfg, m) }`
    with `cfg` restored from `cfg.plain` when set (paster's rule);
    `launch` = `func(f func()){ go f() }`.
  - `(o *urlOpener) connect(rt roundTrip)` — nil-safe; seed via `one(rt, …)`
    (`-t tmuxQuote(session)` only when non-empty); on success add each parsed
    nonce to `seen`; then `sendSubscription(rt, openSubName, "", openURLFormat)`;
    then the register round-trip; the first failure calls `o.fail(step)` which
    launches one notify and stops.
  - `(o *urlOpener) handle(sess, v string)` — nil-safe; foreign-session guard;
    open each unseen valid record via `launch`, reporting an open error through
    `notify`; replace `seen` with this value's nonces.
  - `BrowserOpener(goos string) func(string) error`, `browserOpenerName`,
    `envWithout`, `runBounded(cmd *exec.Cmd, bound time.Duration) error` (env
    = `envWithout(os.Environ(), "BROWSER")`, stdout/stderr = `os.Stderr`,
    Start, wait on a buffered channel, return nil after `bound`), and
    `const openerWaitBound = 5 * time.Second`.
  Proof: the Step 5 command passes; `go vet ./remotebridge/...` clean.

- [ ] **Step 7: wire it into Run** — `daemon.go`:
  - `Config` gains `OpenURL func(url string) error` with a doc comment
    (nil = off, like the other injected funcs; production = `BrowserOpener`).
  - declare `opener *urlOpener` in the `labels/res/usage` var block; build it
    with `opener = newURLOpener(cfg, pin.id)` right after `usage = newUsageShipper(skew)`.
  - `subscribe := func() { labels.subscribed, agents.subscribed, _, _ = subscribeFormats(rt); opener.connect(rt) }`
    — so first attach and every `repair` re-seed, re-subscribe, re-register.
  - `dispatch`'s `SubscriptionChanged` case: `if v, ok := subscriptionValue(l, openSubName); ok && len(l.Args) > 1 { opener.handle(l.Args[1], v) }`.
  - `cmd/daemon/main.go`: `OpenURL: daemon.BrowserOpener(runtime.GOOS),` in
    the `daemon.Config` literal (import `runtime`).
  Proof: `go build ./...` and `go test ./remotebridge/...` green (the full
  daemon package, so existing reconnect/subscription tests still pass with
  the extra round-trips; any test whose scripted `roundTrip` counts replies
  and now breaks is fixed by giving it an `OpenURL == nil` Config — the
  feature is then off and issues nothing).

- [ ] **Step 8: live-tmux proof of the three remote commands** — in
  `openurl_test.go`, `TestOpenURLCommandsAgainstLiveTmux`, modelled on
  `TestSessionResSubscriptionIsSessionScoped` (skip without tmux unless
  `OG_REQUIRE_TMUX`; `startIsolatedTmux` with its own `CLAUDE_STATUS_DIR`):
  set `@og_open_url` to `" 1-1|https://old"` on session `w`, attach a `-C`
  client, write the three `connect` commands for session id `$0`, then
  `set-option -a` a `" 1-2|https://new"` record from outside; read lines until
  a `%subscription-changed og_open` whose value holds both records (5s
  deadline), and assert `show-options -v -t w @og_open_client` equals the
  control client's `list-clients -F '#{client_name}'` name.
  Proof: `go test ./remotebridge/daemon/ -run LiveTmux -v` passes.

- [ ] **Step 9: failing then passing BROWSER restore in the tool float** —
  `ctl_test.go`: `TestToolBrowserRestore` takes the prefix before
  `command -v` exactly as `TestToolPathRestore` does and runs it under
  `/bin/sh -c "$prefix"'printf %s "${BROWSER-unset}"'` with a `tmux` stub that
  answers by `$3`: `BROWSER` → cases `echo BROWSER=/nix/store/x/bin/og-open`
  (→ that path), `exit 1` unset (→ the inherited `BROWSER=profile` kept),
  `echo BROWSER=` (→ kept); `PATH` → `echo PATH=/opt/a`. Run it (red), then
  in `ctl.go` insert after the PATH restore:
  `b=$(tmux show-environment -g BROWSER 2>/dev/null); case $b in BROWSER=?*) BROWSER=${b#*=}; export BROWSER;; esac; `
  and extend the doc comment with one sentence on why (fish's profile may set
  its own `BROWSER`, which would disable the forward). Existing
  `TestToolPathRestore` stub answers every call with its PATH line, which the
  `BROWSER=?*` pattern ignores — unchanged.
  Proof: `go test ./remotebridge/daemon/ -run 'ToolBrowserRestore|ToolPathRestore|ToolResolveScript|ToolVerb'` green;
  the zero-single-quote and format-expansion assertions still pass.

### D. Integration

- [ ] **Step 10: integration test** — `tests/remote-m2-integration.bats`,
  new `@test "og-open in a remote pane opens on the controller once, in bulk, and never again after a reattach"`
  placed after `"a reconnect re-stamps the remote agent usage the reattach dropped"`:
  - `OG_OPEN="${OG_OPEN:-$BATS_TEST_DIRNAME/../scripts/og-open.sh}"`;
    `$BATS_TEST_TMPDIR/openbin/{xdg-open,open}` stubs appending `$1` to
    `$BATS_TEST_TMPDIR/opened`; `export PATH="$BATS_TEST_TMPDIR/openbin:$PATH"`
    **before** `bridge_up` so the daemon inherits it.
  - `$SRC new-session -d -s rem -x 100 -y 30`; `$DST new-session -d -s host-sess -x 100 -y 30`; `bridge_up 1 ogo`.
  - helper `registered_client` = `$SRC show-options -qv -t rem @og_open_client`;
    poll (≤ 8s) until it equals the one control client from
    `$SRC list-clients -t rem -F '#{?client_control_mode,#{client_name},}' | grep .`.
  - `$SRC send-keys -t rem "bash '$OG_OPEN' https://example.invalid/x" Enter`;
    poll (≤ 8s) for 1 line; `sleep 2.5`; assert the file is exactly
    `https://example.invalid/x`.
  - `$SRC send-keys -t rem "for i in 1 2 3; do bash '$OG_OPEN' https://example.invalid/b\$i; done" Enter`;
    poll for 4 lines; assert `sort` of lines 2–4 is `b1 b2 b3`.
  - drop: `old=$(transport_child)`; `kill -9 "$old"`;
    `wait_bridge_disconnected ogo …`; poll for a new `transport_child` ≠ old;
    `wait_bridge_state "" ogo …`; poll until `registered_client` equals the new
    control client's name (proves re-register); `sleep 2.5`; assert still 4 lines.
  - `og-open https://example.invalid/after` once more; poll for 5 lines;
    `sleep 2.5`; assert 5 lines, last `https://example.invalid/after`.
  - kill/wait the daemon before the assertions that follow the last poll, as
    the neighbouring tests do.
  `flake.nix` `remote-m2-integration-tests`: add `OG_OPEN = ./scripts/og-open.sh;`
  beside `DETACH` (same "raw source ships" comment applies).
  Proof: `bats tests/remote-m2-integration.bats -f 'og-open'` green locally;
  `nix build .#checks.x86_64-linux.remote-m2-integration-tests` green.

### E. Docs

- [ ] **Step 11: docs** —
  - `docs/agents/bridge-shipped-state.md`: new `## URL Opens (remote → controller)`
    after `## Remote Agent Usage`, covering (spec §1–§3): the record format
    and 4096 reset, the registered-client check and why, seed → subscribe →
    register and why that order, seen = this value's nonces, the pin filter,
    `validOpenURL` as the security boundary, the bounded opener wait and the
    `BROWSER` strip, the controller env assumption, the tool-float `BROWSER`
    restore, and the accepted losses (reconnect, cap race, pid reuse, two
    controllers).
  - `docs/agents/bridge-daemon.md`: a "What the Remote Host Needs on PATH"
    row — `og-open` via the wrapper plus `BROWSER` from the remote's own
    config (remote rebuilt + config reloaded); an older remote simply has no
    `og-open` and opens remotely. One sentence in "Bridge Reconnect" that
    `repair`'s `subscribe` re-seeds `og_open` and re-registers `@og_open_client`.
  - `docs/agents/scripts.md`: an `og-open` row (invocation: `$BROWSER`;
    purpose; owns `@og_open_url` writes).
  - `CLAUDE.md`: the `bridge-shipped-state.md` table row adds "URL opens
    forwarded to the controller (`og-open`, `@og_open_url`)".
  Proof: `nix build .#lint` (typos, markdown hooks) green.

### F. Gate

- [ ] **Step 12: full local gate** — `nix build .#default`,
  `nix flake check`, `nix build .#lint`, each green.

## Contract map

Carried verbatim from the spec's "Contract map" table; the integration test
(Step 10) covers the producer→remote tmux→daemon→opener chain end to end, the
Go unit tests cover each daemon edge, the bats unit covers the producer.

## Acceptance

- [ ] Unit — `og-open` validation + bridged/unbridged branch (bats):
  `nix build .#checks.x86_64-linux.og-open-tests` (Steps 1–3).
- [ ] Unit — daemon record parsing, seen dedupe across a reconnect (no replay;
  appended-between-seed-and-subscribe opens), scheme/flag rejection (Go):
  `go test ./remotebridge/daemon/ -run 'OpenRecords|ValidOpenURL|URLOpener|OpenSubscription|LiveTmux'`
  (Steps 5–8).
- [ ] Integration — single URL exactly once, bulk of 3 all open, nothing
  reopens after drop+reattach: `bats tests/remote-m2-integration.bats -f og-open`
  and `nix build .#checks.x86_64-linux.remote-m2-integration-tests` (Step 10).
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` all pass
  (Step 12).
