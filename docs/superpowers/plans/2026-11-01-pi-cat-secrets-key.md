# pi usage: read a `!cat <file>` OpenRouter key without exec (#1061)

## Problem

`scripts/tmux-agent-usage-pi.sh` maps every `!…` `.openrouter.key` to an empty
token (`'!'*) token='' ;;`), because a background tick must never run commands
from a config file. A secrets-manager setup stores the key as
`!cat '/run/secrets/openrouter_api_key'`, so the provider finds no token, exits
0 and never writes `pi.json`; pi drops out of `@og_agent_usage`.

Violated invariant: a pi key that names a secrets file resolves to that file's
contents. Kept invariant: nothing from the config is ever executed.

## Mechanism

A parser, not an exec. A new `case` arm `'!cat'[[:blank:]]*)` placed before
`'!'*)` matches the raw key against bash regexes (held in variables, `[[ =~ ]]`)
for exactly one argument after `!cat` + blanks (spaces/tabs only, never a
newline), followed only by optional trailing blanks:

- bare: an absolute path (`/` first) of characters that are none of:
  whitespace, control chars, quotes `'` `"`, `$`, backtick, `\`, `;` `&` `|`
  `<` `>` `(` `)` `{` `}` `*` `?` `[` `]` `~` `#` `!`;
- `'…'` or `"…"`: one matching pair around an absolute path with no quote of
  either kind, no `$`, backtick, `\` or control char inside (stricter than
  shell single quotes on purpose: one rule for both forms, nothing that could
  expand under either).

On a match (`BASH_REMATCH`), require `[[ -f $path && -r $path ]]` (follows
symlinks — sops-nix's `/run/secrets/x` is one), read it with bash only:
`token=$(<"$path")`, then strip trailing whitespace
(`token=${token%"${token##*[![:space:]]}"}`), which covers `$(cat …)`'s
newline stripping plus a stray trailing CR/space. No match, unreadable,
non-regular or empty → `token=''`, so the existing fall-through to
`.openrouter.access` then `OPENROUTER_API_KEY` runs unchanged. The token is
never echoed; it only reaches curl's `-H Authorization` as today. Every other
`!…` still hits `'!'*) token=''`.

Callers: `tmux-agent-usage.sh`'s gate (`-f auth.json || OPENROUTER_API_KEY`)
already passes for this setup (auth.json exists); no change. Bridge/statusline
consumers read `pi.json`/`@og_agent_usage` whose shape is unchanged.

## Files

- `scripts/tmux-agent-usage-pi.sh` — new `!cat` arm; header + `!cmd` comment
  say the `!cat <path>` form is read, not executed.
- `tests/agent-usage-providers.bats` — positive/negative `!cat` cases; header
  comment mentions the `!cat` form.
- `docs/agents/enrichment.md` — "pi is OpenRouter-keyed" bullet: `!cat` form
  read, general `!cmd` still unsupported.
- `docs/agents/scripts.md` — `-pi` row: key sources mention the `!cat` form.
- `docs/superpowers/plans/2026-11-01-pi-cat-secrets-key.md` — this plan.

## Steps

- [ ] **Step 1: failing positive tests** — `tests/agent-usage-providers.bats`,
  after the existing `!command` test. Add a helper `pi_auth_key() { jq -n
  --arg k "$1" '{openrouter:{key:$k}}' >"$HOME/.pi/agent/auth.json"; }`
  (mkdir first) so keys with quotes/`$` need no hand escaping. Tests:
  1. `pi key: !cat reads a secrets file (bare, '…', "…") without running it`
     — key file `$BATS_TEST_TMPDIR/secrets/openrouter_api_key` holding
     `sk-or-test-cat-not-a-key\n` (trailing newline, written with printf
     '%s\n'). `EXPECT_TOKEN=sk-or-test-cat-not-a-key`, no OPENROUTER_API_KEY.
     Loop over the three forms (`!cat $f`, `!cat '$f'`, `!cat "$f"`): remove
     pi.json and curl-argv.log, `run_pi`, assert `[ -e "$(pi_cache)" ]` and
     `grep -qxF -- "-fsS --max-time 10 -H Authorization: Bearer
     sk-or-test-cat-not-a-key https://openrouter.ai/api/v1/key"
     curl-argv.log` (exact argv line ⇒ no trailing newline in the token; a
     newline would split the line).
  2. `pi key: !cat follows a symlinked secrets file` — sops-nix shape:
     `/…/secrets/link -> /…/secrets.d/1/key`, key form bare; pi.json written.
  Command: `nix develop -c bats tests/agent-usage-providers.bats -f '!cat'`
  → both FAIL on the current script (no pi.json). Record the failure line.
- [ ] **Step 2: negative tests** — same file. One test,
  `pi key: other !cat shapes are never run and resolve no token`, table-driven
  over: `!cat $f; touch $marker`, `!cat $(touch $marker)`, ``!cat `touch $marker` ``,
  `!cat $f $f`, `!cat -A $f`, `!cat relative/path`, `!cat ~/x`,
  `!cat $dir/*`, `!sh -c 'touch $marker'`, `!cat '$f` (unbalanced),
  `!cat $missing`, `!cat $dir` (a directory), `!cat $empty` (empty file),
  `!cat $ws` (file of only whitespace/newlines). For each: (a) with
  `OPENROUTER_API_KEY=sk-or-test-fallback-not-a-key` and
  `EXPECT_TOKEN=` same → pi.json exists; (b) with both unset → pi.json absent;
  always `[ ! -e "$marker" ]`. On failure print the case (`echo "case: $k"`
  before asserts so bats shows it). Command as above → passes already on the
  current script (guards, not red evidence).
- [ ] **Step 3: implement** — `scripts/tmux-agent-usage-pi.sh`: regex vars
  just above the `case`, new `'!cat'[[:blank:]]*)` arm before `'!'*)` per
  Mechanism; edit lines 4-5 header ("…`.openrouter.key` (a `!cat <file>` key
  is read from that file)…") and the lines 45-50 paragraph (`!cmd` runs a
  command — never executed here, except the `!cat <absolute path>` shape,
  which is parsed and the file read with bash). Commands:
  `nix develop -c bats tests/agent-usage-providers.bats` → all pass;
  `shellcheck scripts/tmux-agent-usage-pi.sh` clean;
  `shfmt -d scripts/tmux-agent-usage-pi.sh tests/agent-usage-providers.bats`
  empty.
- [ ] **Step 4: docs** — `docs/agents/enrichment.md` bullet: replace the
  "`!cmd` is deliberately never executed…" clause with: the `!cat <absolute
  path>` shape (bare, or one `'…'`/`"…"` pair with no expansion) is parsed and
  the file read with bash (regular readable file, trailing whitespace
  stripped, empty → falls through); every other `!cmd` is never executed and
  falls through. `docs/agents/scripts.md` row `-pi`: "`.openrouter.key` (a
  `!cat <abs path>` key read from the file, never run)". Test-file header
  lines 8-10 likewise. Command: `rg -n '!cat' docs/agents scripts tests` shows
  each.
- [ ] **Step 5: gate** — `nix build .#default`, `nix flake check`,
  `nix build .#lint`, all succeed.

## Acceptance

- [ ] three `!cat` forms send `Bearer <fake key>` (argv log) and write
  pi.json → Step 1 test 1.
- [ ] trailing newline stripped → Step 1 test 1 exact argv line.
- [ ] negative cases resolve nothing, execute nothing, fall through to
  `OPENROUTER_API_KEY` / write nothing → Step 2 test.
- [ ] existing provider tests (`$VAR`, `$$`, `$!`, `.access`) pass → Step 3
  full bats run.
- [ ] enrichment.md documents `!cat` + unsupported `!cmd` → Step 4.
- [ ] `nix build .#default`, `nix flake check`, `nix build .#lint` → Step 5.

## Regression evidence

Red: Step 1 tests against the pre-fix script (pi.json absent). Green: same
tests after Step 3. Both recorded in the PR `## Testing`.

## Critic notes folded in

- Every test that expects pi.json seeds `pi_key_fixture '{"usage_monthly":1}'`
  (the fake curl exits 22 without `$FIXTURES/key.json`); keep it uncapped and
  `OG_OPENROUTER_MGMT_KEY_FILE` unset so curl runs once and the argv line is
  unique.
- Step 2: before each (a)/(b) run, remove pi.json and curl-argv.log; for (b)
  unset `OPENROUTER_API_KEY` and `EXPECT_TOKEN`.
- Regex: `]` must come first inside the negated bracket (`[^]…]`); `\` is
  literal inside brackets.
- Step 1 test 1 also asserts the fake key never appears in stdout, stderr
  (`run --separate-stderr`) or pi.json.
