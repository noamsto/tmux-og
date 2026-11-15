# og shell completions generated from the verb table (#1105)

Goal: `og <TAB>` / `og <noun> <TAB>` complete in fish, with summaries, generated inside `mkOg` from the same `verbs` attrset that renders `og-table.sh`.

## Steps

1. `config/tmux.conf.nix` `mkOg`: add `fishQuote = s: "'" + replaceStrings ["\\" "'"] ["\\\\" "\\'"] s + "'"` (fish single quotes honour only `\\` and `\'`; `escapeShellArg` is NOT fish-safe). Derive `nouns` (first token of each `order` entry, unique, in order) and `subverbs noun` (entries `"noun X"` -> token `X`, summary). Noun description = its bare-verb summary, else `"<N> subcommands"` (singular `1 subcommand`).
2. Render `og.fish` via `pkgs.writeText`:
   - `complete -c og -n '__fish_use_subcommand' -f -a <noun> -d <desc>` per noun, plus `help` (`-d 'List every verb'`).
   - per subverb: `complete -c og -f -n 'set -l t (commandline -opc); test (count $t) -eq 2; and test "$t[2]" = <noun>' -a <verb> -d <summary>`.
   - Nothing else is registered, so past the verb fish falls back to files.
3. Make `og` a `symlinkJoin { name = "og"; paths = [ogBin completions]; }` where `completions` is a `runCommand` placing `og.fish` at `share/fish/vendor_completions.d/og.fish`. `bin/og` is the unchanged `writeShellScriptBin` output (og.sh uses no `$0`), so store-path consumers (`${og}/bin/og`, home-manager `home.packages`, `packages.og`) keep working; home-manager links `share/` into the profile.
4. `flake.nix`: new check `og-completion-assertions`: `mkOg` over a stub table (noun with bare verb + subverbs, subverb-only noun, a summary with `'`, `(`, `--`, `\`), `nativeBuildInputs = [pkgs.fish]`, runs `fish --no-config -c 'source <og.fish>; complete -C "og "'` and `complete -C "og noun "` and asserts expected names + descriptions, that `og noun x ` yields no subverbs, and a negative proof that a verb is in completions by iterating the table's verbs (derived from the stub attrset, not retyped). Also run it against the real `tmuxConfig.og` verbs from `ogVerbSpec` (every noun/subverb must appear).
5. Docs: README `og` section (fish completions, auto-loaded from the profile); `docs/agents/scripts.md` where `og` is described (completions generated in `mkOg`, quoting rule, subverb gating).
6. Gate: `nix build .#default`, `nix flake check`, `nix build .#lint --accept-flake-config`; show `complete -C "og "` against the built package in the PR.

## Out of scope
bash/zsh (not cheap: needs a different data layout), per-script flag completion.

## Acceptance
- completions come from the table (check fails if table verb missing from output)
- quotes/parens/`--` summaries escaped correctly
- lint, flake check, default build pass

## Revision 1 (plan-critic)

- Step 3: the join's `bin/og` is a symlink to the inner `writeShellScriptBin`, so checks 10/11 of `og-dispatch-assertions` (which grep for `$OG`) would go vacuous. `mkOg` exposes `passthru.bin = ogBin`; those checks also grep `$OG_INNER = tmuxConfig.og.bin`.
- Subverb condition is built by `fishQuote` over the whole condition string (nested quoting verified in the check, including a noun-with-quote-free stub).
- Use `commandline -x` if fish 4.9.3 deprecates `-o`.
- Check asserts that no subverb names come back for `og <noun> <verb> `, and that every key in the stub table and `ogVerbSpec` shows up (noun in `og `, subverb in `og <noun> `).
- Demo is against `.#og` (`.#default` contains no og): `source result/share/fish/vendor_completions.d/og.fish`.
- README notes NixOS per-user profiles need `programs.fish.enable` to link `share/fish/vendor_completions.d`.
