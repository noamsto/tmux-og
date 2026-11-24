# og dispatcher (`picker/ogcli`)

`og <noun> [<verb>] [args...]` resolves to an absolute target and execs it with
the remaining argv untouched (#1111, replacing `scripts/og.sh`). The verb table
stays `ogVerbSpec` in `config/tmux.conf.nix`; the binary reads it at runtime.

- **Table file:** `mkOg` writes `share/og/table.json`, a JSON array in display
  order (`builtins.attrNames` order) of `{verb, target, summary, model?}`.
  Unknown JSON fields are rejected. The binary finds it at
  `filepath.EvalSymlinks(os.Executable())/../share/og/table.json`, which is why
  `bin/og` is a *copy* of `og-dispatch`, never a symlink (a link would resolve
  back into the `og-dispatch` derivation, which has no table).
- **`mkOg` stays a function over a table**, so the stub instances in the flake
  checks cost an `install`, not a Go build.
- **`model`:** optional name of a Go-backed verb, looked up in
  `picker/ogcli/models.go` (`remote-open` and `project` today). kong parses that
  verb's flags for completion and a hand-written `--help` text that states the
  target's real grammar. kong never validates the argv: anything but a sole
  `--help`/`-h` is exec'd raw. For `remote-open`, `remoteopen.ParseArgs` is the
  only validator (it is strictly positional: `--into` is argument 3, no
  `--into=value`, and dash-leading host/session names are legal) and exit codes
  are the launcher's; a test pins each declared flag as a literal token in an
  argv `ParseArgs` accepts. For `project`, `og-project-bin` itself is the
  validator. A model on a verb that also has subverbs, or an unknown model, fails
  table load. `project` is a help-only model: `og` intercepts a sole
  `--help`/`-h` and never runs the target, so without a model `og project
  --help` would print only the summary and the target path, not
  og-project-bin's usage (P0 finding 7). Its struct declares no flags, only one
  `arg:"" optional:"" passthrough:""` positional: a model replaces the verb's
  passthrough leaf, and without that positional completion would reject
  `og project status`. It is one verb with no `project x` entries, and
  `og project status --help` reaches the binary raw. The model's help is
  `project.Usage` (`picker/project/usage.go`), shared with the binary.
- **Hand resolver:** the longest-match resolver is a 1:1 port of `og.sh`
  because kong cannot express a node with both positional args and subcommands,
  which `status`/`notify` need (`status --flag` is the bare verb, `status x`
  its subverb). kong supplies the model for flags and completion only.
- **Separate derivation, leak guard:** `ogDispatchBin` is
  `goTools.overrideAttrs` building only `ogcli/cmd/og`, never part of
  `go-tools`: `go-tools` feeds `picker-generate` and `wrapperBinPkgs`, so a
  binary added there would land on the tmux server's PATH.
  `og-dispatch-binary-assertions` fails if `og-dispatch` is linked into
  `wrapperBinDir` or named in the generated conf (with a `find -lname` positive
  control). `picker/default.nix` changes only `vendorHash` for this.
- **Completions:** fish is a *static* script from `og completion fish` (nouns
  with summaries, "N subcommands" for subverb-only nouns, subverbs gated by
  `commandline -opc`, one `-l` line per flag of a Go-backed verb gated on the
  verb being typed, files past the verb). Static keeps every description, which
  a `COMP_LINE` hook would drop. bash/zsh are `jotaen/kong-completion`
  (`og completion bash|zsh -c`), dynamic and names only.
- **`completion` is hidden:** absent from `og help`, noun help and
  `ogVerbSpec`; `ogcli.Main` intercepts a literal first argument `completion`
  before `Resolve`, and table load refuses a verb whose first word is
  `completion`.
- **Contract pinned by `og-dispatch-assertions`:** help shape (`  %-22s %s`,
  blank line between nouns), `<verb> --help` last line is the target, the
  two-line `unknown command` on stderr with exit 2, raw passthrough
  (`--flag`, `--`, `""`). Edit that check only to change the contract.
- **Not Go-backed yet:** `init`/`doctor`/`generate` (separate Go module) and the
  bash verbs.
