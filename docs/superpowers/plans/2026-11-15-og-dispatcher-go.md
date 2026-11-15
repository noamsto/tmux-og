# og dispatcher in Go with kong (#1111)

Rewrite `scripts/og.sh` as a Go binary built on kong. The verb table stays
`ogVerbSpec` in `config/tmux.conf.nix`; `mkOg` serialises it to JSON that the
binary reads at runtime. Bash-backed verbs still exec their script with the
argv untouched. `remote open` is the first Go-backed verb: kong parses its real
flags for `--help`, validation and completion, and the target is still exec'd
with the original argv.

## Design (decisions)

- **Runtime JSON table + reflect-built kong model + hand resolver.** `mkOg`
  writes `share/og/table.json`; the binary finds it next to itself
  (`filepath.EvalSymlinks(os.Executable())` → `../share/og/table.json`). `mkOg`
  stays a function over a table, so the stub instances in the flake checks cost
  an `install` each, not a Go build (rejected alternative: generate Go source
  from Nix and compile per table). The longest-match resolver is ported 1:1
  from `og.sh`, because kong cannot express a node that has both positional
  args and subcommands, which `status`/`notify` need. kong supplies the model
  (help text, flags, completion) and parses a Go-backed verb's remainder.
- **The dispatcher is a separate derivation, never part of `go-tools`.**
  `go-tools` → `picker-generate` → `wrapperBinPkgs`, so any binary added there
  lands on the tmux server's PATH, and check 11 of `og-dispatch-assertions`
  (which greps only `og`'s own path) would not notice. `ogDispatchBin =
  goTools.overrideAttrs (_: { subPackages = ["ogcli/cmd/og"]; postInstall =
  "mv $out/bin/og $out/bin/og-dispatch"; })` in `config/tmux.conf.nix`
  (`goTools` is a real `buildGoModule` in every importer: `flake.nix:3392`,
  `modules/home-manager.nix:12`; it shares `vendorHash`, and `picker/default.nix`
  does not list the new package). A new check asserts `og-dispatch`'s path is
  in neither `wrapperBinDir` nor the generated conf.
- **`bin/og` is the real binary, not a wrapper**, so the table lookup and
  bash/zsh completion (which embeds `os.Executable()`) are self-contained.
  `ogBin = runCommand "og"` installs a *copy* of `og-dispatch` (a symlink would
  resolve back to the dispatcher derivation), then `share/og/table.json`, then
  generates `share/fish/vendor_completions.d/og.fish` by running
  `$out/bin/og completion fish` (after the table is installed). `passthru.bin`
  is the derivation itself (`lib.extendDerivation`), so checks 10/11 keep
  grepping the real path unchanged.
- **Completions:** fish is a *static* script emitted by `og completion fish`
  from the kong model, same shape as today's Nix-generated file (nouns with
  summaries, "N subcommands" for subverb-only nouns, subverbs gated by
  `commandline -opc`, files past the verb) plus one `complete -c og -n <verb
  cond> -l <flag> -d <help>` line per flag of a Go-backed verb. Keeping
  descriptions and keeping `og-completion-assertions` close to unchanged is the
  point: a dynamic `COMP_LINE` hook (kongplete / kong-completion, names only)
  would drop the summaries #1105 shipped. bash/zsh use
  `jotaen/kong-completion` (`og completion bash|zsh -c`, dynamic via
  `COMP_LINE`). kongplete itself is kong v0.8-era; kong-completion tracks kong
  v1.
- **`completion` is hidden**: not in `og help`/noun help, not in
  `ogVerbSpec`; `og help` output stays byte-for-byte what `og.sh` printed. The
  resolver routes a literal first token `completion` (a Go test asserts no table
  verb or noun may be named `completion`).

## Consumer map (shared contract: the `og` CLI and table)

| Edge | Disposition |
|---|---|
| `mkOg verbs` callers: `og`, `og-completion-assertions` stub, `og-dispatch-assertions` stub | compatible: same `{target, summary}` input; `model` optional |
| `ogVerbSpec` → `og` (`script`/`target` resolution) | compatible; `model = "remote-open"` added to `remote open` only |
| `og.bin`/`OG_INNER` leak checks 10/11 | compatible: `bin` is the same drv; guards still grep its path |
| `go-tools` → `picker-generate` → `wrapperBinPkgs` → tmux PATH; `packages.go-tools` | unchanged: dispatcher is a separate drv; new leak check covers `og-dispatch` |
| `og <verb> --help` last line = target (checks 2–5, 9, 12) | preserved: summary line, kong help (Go-backed only), target last |
| bare `og`, `og help`, `og <noun>`, `og <noun> --help|-h|help` lists | preserved byte-for-byte in shape (`  %-22s %s`, blank line between nouns) |
| unknown command: stderr 2 lines, exit 2, empty stdout (checks 6, 8, 12) | preserved |
| flag-vs-subverb rule for `status`/`notify` (check 12, stub `s`/`s x`) | ported verbatim in `resolve` + table-driven Go test |
| exec of target with `remaining` untouched (check 7: `--flag`, `--`, `""`) | preserved: raw remainder exec'd |
| `og remote open` argv: kong stricter on leading-dash session names / unknown flags, laxer on flag position | forward raw argv so `remoteopen.ParseArgs` still decides; exit 2 (kong) vs 1 (launcher) for the kong-rejected cases; no runtime caller of `og remote open` (docs, check 5 only) |
| `modules/home-manager.nix:1151` (`home.packages`), `flake.nix` `packages.og` | compatible: one derivation |
| fish completions `share/fish/vendor_completions.d/og.fish` | same shape + flag lines; Go-emitted instead of Nix-emitted |
| `README.md` completion paragraph (61-65) | updated (flags, bash/zsh) |
| `docs/agents/scripts.md` `og` row | updated |
| `scripts/og.sh` | deleted (not in `scriptNames`/`ogInternal`; read only by `mkOg`) |
| `hookyard.json` / bats | no references to `og.sh` (verified by `rg`) |

The full map also goes in the PR's Agent ledger block (`REVIEW_NOTES.md` before
the PR exists, never committed).

## Files

- `picker/ogcli/table.go` — `Table`/`Verb` types, JSON load, `model` registry lookup.
- `picker/ogcli/resolve.go` — port of the `og.sh` resolver and help/unknown text.
- `picker/ogcli/model.go` — builds the kong parser (nouns/verbs via `reflect.StructOf`, Go-backed leaves from the registry).
- `picker/ogcli/models.go` — registry: `"remote-open"` → flag struct (`Host`, `Sess`, `Win` positionals; `--into`, `--window`).
- `picker/ogcli/fish.go` — static fish emitter over the kong model; `completion.go` — hidden `completion` command (fish own, bash/zsh delegated).
- `picker/ogcli/run.go` — `Main(tablePath, args, env) int`.
- `picker/ogcli/*_test.go` — resolver table test, help-shape test, model test, exec/completion tests via `TestMain` re-exec.
- `picker/ogcli/cmd/og/main.go` — binary entry; table path from the symlink-resolved executable.
- `picker/go.mod`, `picker/go.sum` — kong, kong-completion (pin what `go get` resolves).
- `picker/default.nix` — new `vendorHash` only (the dispatcher is not in `subPackages`).
- `config/tmux.conf.nix` — `ogDispatchBin`, `ogVerbSpec."remote open".model`, `mkOg` rewrite.
- `flake.nix` — `og-completion-assertions` extended (flag lines), new `og-dispatch-binary-assertions`; `og-dispatch-assertions` untouched.
- `scripts/og.sh` — delete.
- `README.md`, `docs/agents/scripts.md`, `docs/agents/og-dispatcher.md` (new, short), `CLAUDE.md` table row.

## Steps

- [ ] **Step 1: spike the kong model.** Files: `picker/ogcli/model_test.go`, `picker/go.mod`, `picker/go.sum`. `cd picker && go get github.com/alecthomas/kong@v1.17.0 github.com/jotaen/kong-completion@latest`. Failing test `TestModelShape`: a parser from a stub table (`q`, `q sub`, `g a`, `g b`, a bare-verb-plus-subverb noun `s`/`s x`, one Go-backed leaf with `--into`) asserts the node tree (`q`→`sub`, `s`→`x` with no positional on `s`, passthrough positional on bash leaves, `--into`/`--window` on the Go leaf). Command: `cd picker && go test ./ogcli -run TestModelShape` (red: package missing, then assertions). If kong rejects `reflect.StructOf`, fall back to per-noun `kong.DynamicCommand` and re-record the choice here; later steps are independent of it.
- [ ] **Step 2: table + model.** Files: `table.go`, `model.go`, `models.go`. `Load(path)`; registry (unknown model → error); `buildParser(table)`. Command: `go test ./ogcli -run TestModelShape` → green.
- [ ] **Step 3: failing resolver tests.** File: `resolve_test.go`. Table test encoding every `og.sh` branch: bare/`help`/`--help`/`-h`; two-token match; one-token match for the `status`/`notify` shape (`s`, `s --flag foo`, `s frobnicate` → unknown, `s --help` and `s help` → noun help); `og ""`; unknown; subverb-only noun (`remote`) bare, `--help`, `help`, `frobnicate`; `<verb> --help` as sole remainder; a table naming a verb `completion` is refused. Command: `go test ./ogcli -run TestResolve` (red).
- [ ] **Step 4: resolver + help.** File: `resolve.go`. Port `og.sh` exactly (same conditions and order, `%-22s`, blank line between nouns, two-line `unknown command` on stderr). Command: `go test ./ogcli -run TestResolve` → green.
- [ ] **Step 5: failing run + completion tests.** Files: `run_test.go`, `fish_test.go`. Process-replacing paths (exec, `kongcompletion.Register` which may `os.Exit`) run through a `TestMain` re-exec helper, never in-process. (a) exec passthrough with a stub script: `--flag -- bar ""` arrives verbatim (mirror of check 7); (b) `<verb> --help` last line is the target for a bash-backed and the Go-backed verb; (c) Go-backed verb: unknown flag → exit 2, no exec; `host sess --into x` → target exec'd with the raw argv; (d) `completion fish` output over a stub table: noun lines with escaped summaries (quote, paren, `--`, backslash), `N subcommands` description, gated subverb lines, `help` line, flag lines for the Go verb only, nothing past the verb; (e) `completion bash -c` names the binary path; `COMP_LINE="og remote open --"` lists `--into`/`--window`; (f) `og help` contains no `completion` line. Command: `go test ./ogcli -run 'TestRun|TestFish'` (red).
- [ ] **Step 6: run + fish + main.** Files: `run.go`, `fish.go`, `completion.go`, `cmd/og/main.go`. `Main`: `kongcompletion.Register(parser)` first (COMP_LINE), then resolve → help → kong-parse remainder for Go-backed verbs → `syscall.Exec(target, [target]+remainder, os.Environ())`; help via kong with custom writer/exit so the target is the last line. Command: `go test ./ogcli` → green.
- [ ] **Step 7: lint locally.** Command: `cd picker && golangci-lint run ./ogcli/... && go vet ./ogcli/... && go test -race ./ogcli/...`. Fix at the source; per-line `//nolint:gosec // G204: ...` on the exec like `remoteopen/runner.go:86`. Run `nilaway` as the devShell provides.
- [ ] **Step 8: Nix wiring.** Files: `picker/default.nix` (`vendorHash = lib.fakeHash`, build, paste the printed hash), `config/tmux.conf.nix` (`model` on `remote open`; `ogDispatchBin`; table JSON from `builtins.toJSON` of the ordered verb list with `model` where set; `ogBin = runCommand` per Design, `lib.extendDerivation` for `passthru.bin`). Give `ogDispatchBin` its own `pname = "og-dispatch"` and add it to the attrset `tmux.conf.nix` returns (the `inherit` list at the end) so the flake check can reach it. `git rm scripts/og.sh`. Commands: `nix build .#og --accept-flake-config -o result-og`; `./result-og/bin/og help | head`; `./result-og/bin/og remote open --help`; `nix build .#go-tools` and `ls result/bin | rg og-dispatch` → absent.
- [ ] **Step 9: checks.** File: `flake.nix`. `og-completion-assertions`: keep every existing assertion (stub and real) and add: real `og remote open --` lists `--into` and `--window`; stub Go-backed verb's flags only appear after it. The flag lines are gated on the `commandline -opc` tokens being `og remote open`; assert they do not appear at `og remote ` or past the verb. New `og-dispatch-binary-assertions`: `wrapperBinDir` is a symlinkJoin, so test link targets (`find -lname '*og-dispatch*'` over `wrapperBinDir`, plus `ls` for the name) and grep `tmuxConf` for the store path; show the `find` can match by a positive control against a link known to exist. Command: `nix build .#checks.x86_64-linux.og-completion-assertions .#checks.x86_64-linux.og-dispatch-binary-assertions .#checks.x86_64-linux.og-dispatch-assertions --accept-flake-config`.
- [ ] **Step 10: docs.** Files: `README.md` (completion paragraph), `docs/agents/scripts.md` (replace the `og` row), `docs/agents/og-dispatcher.md` (table JSON, `model` registry, why a hand resolver, separate derivation + leak guard, static fish vs `COMP_LINE`, hidden `completion`), `CLAUDE.md` table row. Command: `rg -n 'og\.sh|symlinkJoin' docs/agents/scripts.md README.md` → no stale mention.
- [ ] **Step 11: full gate.** `nix build .#default --accept-flake-config`; `nix flake check --accept-flake-config`; `nix build .#lint --accept-flake-config`; `fish --no-config -c 'source ./result-og/share/fish/vendor_completions.d/og.fish; complete -C "og remote open --"'` with `PATH` including `result-og/bin`.

## Risks and decisions for the reviewer

- **kong vs `remoteopen.ParseArgs`:** see the consumer-map row; kong only rejects unknown flags and a missing host, the launcher still validates the rest.
- **Go-emitted fish duplicates the old Nix emitter's rules** (quoting, "N subcommands"); the existing assertions pin them, so the port cannot drift silently.
- **Out of scope (follow-up):** kong models for `init`/`doctor`/`generate` (separate Go module, own `flag` sets) and the bash verbs.

## Acceptance

- [ ] Existing og flake checks pass unchanged (`og-dispatch-assertions` untouched, `og-completion-assertions` only extended) → `nix flake check --accept-flake-config`.
- [ ] `fish --no-config -c 'complete -C "og remote open --"'` (vendored file sourced) lists `--into` and `--window` → Step 11.
- [ ] `nix build .#lint --accept-flake-config` → Step 11.
- [ ] `nix flake check --accept-flake-config` → Step 11.
- [ ] `nix build .#default --accept-flake-config` → Step 11.
