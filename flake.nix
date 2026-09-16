{
  description = "Opinionated tmux configuration with Claude Code integration";

  # Public binary cache so installs pull the from-source tmux (mkTmux) and the Go
  # binaries instead of compiling. Populated by CI (cachix-action) for
  # x86_64-linux + aarch64-darwin.
  nixConfig = {
    extra-substituters = ["https://lazytmux.cachix.org"];
    extra-trusted-public-keys = ["lazytmux.cachix.org-1:8P28D3LZAKqPlkEGKzRRU9gon3rgBv4u8/4VWRn6TCg="];
  };

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # The wrapped tmux is pinned to plain upstream at a fixed rev to pick up the
    # next-3.8 work (floating panes, scene renderer, menus-in-scene,
    # display-panes-as-a-mode). Pin the exact rev (not a moving branch) so builds
    # stay reproducible. The prior fork (noamsto/tmux fix/popup-overlay-flicker)
    # is dropped: its overlay-clipping fix is already upstream (e242da16), and its
    # two #5336 popup-flicker fixes are now resolved — the overlay redraw fix
    # landed as tmux/tmux#5398, the other was rejected upstream.
    # Bump: repoint rev, then `nix flake lock --update-input tmux-upstream`.
    tmux-upstream = {
      url = "github:tmux/tmux/e880cf63e0a9fe095d7c5d313761520fb1a8653c";
      flake = false;
    };
    flake-parts.url = "github:hercules-ci/flake-parts";
    git-hooks-nix.url = "github:cachix/git-hooks.nix";
    git-hooks-nix.inputs.nixpkgs.follows = "nixpkgs";
    tmux-remux = {
      url = "github:noamsto/tmux-remux";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    aeye = {
      url = "github:noamsto/aeye";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    prdash = {
      url = "github:noamsto/prdash";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs @ {flake-parts, ...}: let
    # tmux pinned to upstream at a fixed rev (see tmux-upstream input).
    # autoreconfHook and bison are already in nixpkgs tmux's nativeBuildInputs, so
    # overriding src to a raw git checkout (no pre-generated configure) just
    # works. The version must be a substring of `tmux -V` output ("tmux
    # next-3.9") for the versionCheckHook to pass — upstream cut release_3.8 in
    # 9b3268a2 (2026-09-09) and master moved on, so a bump across a branch point
    # fails the build here until this string follows it.
    # --disable-asan: ASan's runtime deadlocks during init on macOS 26
    # (llvm/llvm-project#200447), hanging every tmux call before main(). Upstream
    # now defaults ASan off on Darwin, so this is belt-and-suspenders — it keeps
    # the flag correct regardless of upstream's default. No-op on Linux.
    # --enable-jemalloc (darwin only): with ASan off, configure now *requires* an
    # explicit --enable/--disable-jemalloc on macOS, because its calloc(3) can
    # fail to zero allocations for complex codepoints (emoji/nerd-font glyphs we
    # render). jemalloc avoids that, so opt in and add the lib to buildInputs.
    mkTmux = pkgs:
      pkgs.tmux.overrideAttrs (old: {
        version = "next-3.9";
        src = inputs.tmux-upstream;
        configureFlags =
          old.configureFlags
          ++ ["--disable-asan"]
          ++ pkgs.lib.optionals pkgs.stdenv.isDarwin ["--enable-jemalloc"];
        buildInputs = old.buildInputs ++ pkgs.lib.optionals pkgs.stdenv.isDarwin [pkgs.jemalloc];
      });
  in
    flake-parts.lib.mkFlake {inherit inputs;} {
      imports = [inputs.git-hooks-nix.flakeModule];

      systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];

      perSystem = {
        config,
        pkgs,
        lib,
        ...
      }: let
        tmuxConfig = import ./config/tmux.conf.nix {
          inherit pkgs lib;
          tmuxPkg = mkTmux pkgs;
          carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
          carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
          prdash = inputs.prdash.packages.${pkgs.system}.prdash;
        };

        # buildGoModule's checkPhase only runs `go test ./<pkg>` per subPackage
        # (non-recursive), so the default `picker` derivation never exercises the
        # nested packages under agentdetect/ (debounce/manifest/screen/statefile)
        # or remotebridge/ (daemon/wire/controlmode/render) — where the mirror
        # engine, the pane diff, the ctl verb table and the focus state machine
        # live. Both run here in one derivation: as two, all nine subPackages got
        # compiled twice over the same source, the most expensive thing in a CI
        # run. -race on remotebridge because the mirror engine touches mirror
        # state from a second goroutine (M2.3).
        # Reused (not just for its checkPhase) by the remote bridge integration
        # checks below, which need its binaries prebuilt and offline.
        pickerChecked =
          (import ./picker {
            inherit pkgs lib;
            processIcons = import ./config/process-icons.nix;
            fallbackIcon = "";
            maxIconsPicker = "5";
          }).overrideAttrs (old: {
            doCheck = true;
            # tmux: TestReflowRunShellArgsSurvivesFormatInjection (#368) drives a
            # real run-shell call, same private config-less pattern as
            # reflow-fanout-tests. Kept here rather than picker/default.nix so it's
            # scoped to this check alone — the package itself is reused for
            # prebuilt binaries by the remote bridge integration checks, which
            # don't need a test-only dependency.
            #
            # mkTmux, not pkgs.tmux, matching every other live-tmux check here:
            # these tests assert version-sensitive command grammar, so the binary
            # under them has to be the one that ships. 3.7c is not merely older,
            # it is actively misleading — its `list-commands new-pane` advertises
            # -A and -B while its parser rejects both, so a capability probe
            # passes and the command still fails.
            nativeBuildInputs = (old.nativeBuildInputs or []) ++ [(mkTmux pkgs)];
            # Tells TestReflowRunShellArgsSurvivesFormatInjection (#368) to fail
            # rather than skip if tmux is somehow still missing, so pruning the
            # nativeBuildInputs entry above breaks loudly instead of silently
            # dropping the regression check.
            OG_REQUIRE_TMUX = "1";
            checkPhase = ''
              runHook preCheck
              export GOFLAGS=''${GOFLAGS//-trimpath/}
              go test ./tmuxformat/...
              go test ./enrichstate/...
              go test ./agentdetect/...
              go test ./statusline/...
              go test -race ./remotebridge/...
              runHook postCheck
            '';
          });
      in {
        # Not a check: the hook closure (python + every nix/shell linter, ~1200
        # store paths) is the largest fetch in the repo, and as a check it was
        # paid on both CI matrix legs. It moves to `packages.lint`, built once.
        # The devShell shellHook below still installs the hooks locally.
        pre-commit.check.enable = false;

        pre-commit.settings.hooks = {
          # Nix
          statix.enable = true;
          deadnix.enable = true;
          alejandra.enable = true;

          # Shell
          shellcheck.enable = true;
          shfmt.enable = true;
          macos-portability = {
            enable = true;
            name = "macos-portability";
            description = "Reject Linux-only binaries that break on nix-darwin";
            entry = "bash ${./tests/check-portability.sh}";
            files = "^scripts/.*\\.sh$";
          };

          # General
          typos.enable = true;
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace.enable = true;
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages =
            config.pre-commit.settings.enabledPackages
            ++ [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
              pkgs.bats
              pkgs.jq
            ];
        };

        checks = {
          enrich-tests =
            pkgs.runCommand "enrich-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.jq pkgs.coreutils];
              # truncate_ellipsis appends a multibyte "…"; bash's ${#REPLY}
              # only counts it as one char under a UTF-8 locale.
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/enrich.bats
              touch $out
            '';

          enrich-budget-tests =
            pkgs.runCommand "enrich-budget-tests" {
              # git: the pass groups windows by `git rev-parse --git-common-dir`,
              # so the fixture needs a real repo.
              nativeBuildInputs = [pkgs.bats pkgs.jq pkgs.coreutils pkgs.git];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/enrich-budget.bats
              touch $out
            '';

          agent-usage-gate-tests =
            pkgs.runCommand "agent-usage-gate-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-usage-gate.bats
              touch $out
            '';

          reflow-tests =
            pkgs.runCommand "reflow-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/reflow.bats
              touch $out
            '';

          icons-tests =
            pkgs.runCommand "icons-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.jq pkgs.coreutils];
              # measure_display_width classifies multibyte codepoints; bash's
              # per-char indexing only works under a UTF-8 locale.
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/icons.bats
              touch $out
            '';

          claude-issues-tests =
            pkgs.runCommand "claude-issues-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/claude-issues.bats
              touch $out
            '';

          claude-progress-tests =
            pkgs.runCommand "claude-progress-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/claude-progress.bats
              touch $out
            '';

          mark-seen-tests =
            pkgs.runCommand "mark-seen-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/mark-seen.bats
              touch $out
            '';

          codex-relaunch-stamp-tests =
            pkgs.runCommand "codex-relaunch-stamp-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/codex-relaunch-stamp.bats
              touch $out
            '';

          codex-status-hooks-tests =
            pkgs.runCommand "codex-status-hooks-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gawk pkgs.gnused];
            } ''
              cp -r ${./tests} tests
              mkdir modules
              cp ${./modules/home-manager.nix} modules/home-manager.nix
              bats tests/codex-status-hooks.bats
              touch $out
            '';

          startup-session-tests =
            pkgs.runCommand "startup-session-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused];
            } ''
              cp -r ${./tests} tests
              mkdir modules
              cp ${./modules/home-manager.nix} modules/home-manager.nix
              bats tests/startup-session.bats
              touch $out
            '';

          picker-launcher-tests =
            pkgs.runCommand "picker-launcher-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.bash];
            } ''
              cp -r ${./tests} tests
              cp -r ${./scripts} scripts
              bats tests/picker-launcher.bats
              touch $out
            '';

          cursor-status-hooks-tests =
            pkgs.runCommand "cursor-status-hooks-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.jq];
            } ''
              cp -r ${./tests} tests
              cp -r ${./scripts} scripts
              mkdir modules
              cp ${./modules/home-manager.nix} modules/home-manager.nix
              bats tests/cursor-status-hooks.bats
              touch $out
            '';

          cursor-relaunch-stamp-tests =
            pkgs.runCommand "cursor-relaunch-stamp-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/cursor-relaunch-stamp.bats
              touch $out
            '';

          cursor-relaunch-hooks-install-tests =
            pkgs.runCommand "cursor-relaunch-hooks-install-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.jq];
            } ''
              cp -r ${./tests} tests
              cp -r ${./scripts} scripts
              mkdir modules
              cp ${./modules/home-manager.nix} modules/home-manager.nix
              bats tests/cursor-relaunch-hooks-install.bats
              touch $out
            '';

          prune-stale-state-tests =
            pkgs.runCommand "prune-stale-state-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/prune-stale-state.bats
              touch $out
            '';

          # Deliberately no LANG/LC_ALL. Nothing in these suites asserts a
          # character count or a display width — the multibyte values are only
          # ever compared as bytes — and C.UTF-8 does not exist on darwin, where
          # bash's setlocale warning lands on stderr. bats folds stderr into
          # $output, so pinning a locale here buys nothing and breaks the
          # center's line-count assertions on macOS.
          notify-router-tests =
            pkgs.runCommand "notify-router-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/notify-router.bats
              touch $out
            '';

          notify-producers-tests =
            pkgs.runCommand "notify-producers-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/notify-producers.bats
              touch $out
            '';

          notify-center-tests =
            pkgs.runCommand "notify-center-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/notify-center.bats
              touch $out
            '';

          # The bell/activity WIRING, asserted on the generated conf rather than
          # by inspection: hooks present at the free [20] index, both commands
          # pinned to a store path (a bare name resolves against the tmux
          # server's frozen PATH and would make prefix+r an incomplete deploy),
          # both passing #{q:window_id} (never #{session_id} — run-shell's sh -c
          # re-expands a leading $), matching -gu clears for reload idempotence,
          # and the n bind running a store-path center.
          #
          # Two things here are the ONLY automated coverage of their failure mode
          # and must not be softened:
          #   * hook ORDER — a bare `set-hook -gu alert-bell` clears every index,
          #     so a setter above it is erased on every load. Presence greps pass
          #     either way; only the line-number comparison catches it.
          #   * @notify@ SUBSTITUTION — both bats suites override the seam via
          #     OG_NOTIFY_BIN, so a placeholder-name drift would build clean,
          #     test green, and never notify in production. The store paths are
          #     bound straight out of tmuxConfig, so nothing is globbed or guessed.
          notify-conf-assertions =
            pkgs.runCommand "notify-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
              CSU = "${tmuxConfig.script.claude-status-update}/bin/claude-status-update";
              PRE = "${tmuxConfig.script.tmux-pr-enrich}/bin/tmux-pr-enrich";
            } ''
              grep -q 'set-hook -g alert-bell\[20\]' "$CONF"
              grep -q 'set-hook -g alert-activity\[20\]' "$CONF"
              grep -q 'set-hook -gu alert-bell' "$CONF"
              grep -q 'set-hook -gu alert-activity' "$CONF"
              grep -E 'alert-bell\[20\].*/nix/store/[^ ]*/bin/og-notify .*--window #\{q:window_id\}' "$CONF"
              grep -E 'alert-activity\[20\].*/nix/store/[^ ]*/bin/og-notify .*--window #\{q:window_id\}' "$CONF"
              grep -E 'bind-key n display-popup -E .*/nix/store/[^ ]*/bin/og-notify-center' "$CONF"

              # ORDER, not just presence: the clear must precede the setter, or
              # every config load (fresh server AND prefix+r) erases the hook.
              bell_clear=$(grep -n 'set-hook -gu alert-bell' "$CONF" | head -1 | cut -d: -f1)
              bell_set=$(grep -n 'alert-bell\[20\]' "$CONF" | head -1 | cut -d: -f1)
              [ "$bell_clear" -lt "$bell_set" ]
              act_clear=$(grep -n 'set-hook -gu alert-activity' "$CONF" | head -1 | cut -d: -f1)
              act_set=$(grep -n 'alert-activity\[20\]' "$CONF" | head -1 | cut -d: -f1)
              [ "$act_clear" -lt "$act_set" ]

              # The producer seam actually resolved to the router's store path.
              grep -qE '/nix/store/[^ ]*/bin/og-notify' "$CSU"
              grep -qE '/nix/store/[^ ]*/bin/og-notify' "$PRE"
              ! grep -q '@notify@' "$CSU"
              ! grep -q '@notify@' "$PRE"

              # monitor-activity stays off by design (a working Claude pane would
              # be a continuous activity event), and monitor-bell is already on.
              ! grep -q 'set -g monitor-activity on' "$CONF"
              ! grep -q 'set -g monitor-bell' "$CONF"
              touch $out
            '';

          update-environment-conf-assertions =
            pkgs.runCommand "update-environment-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
            } ''
              grep -q 'set -gu update-environment' "$CONF"
              clear=$(grep -n 'set -gu update-environment' "$CONF" | head -1 | cut -d: -f1)
              first_append=$(grep -n 'set -ga update-environment' "$CONF" | head -1 | cut -d: -f1)
              [ "$clear" -lt "$first_append" ]
              touch $out
            '';

          # The float-refit WIRING (#371). tmux bakes a float's percentage
          # geometry into cells at creation and never revisits it, so every
          # float bind must hand its percentages to @float_geom for the
          # window-resized hook to reassert. A bind that forgets the stamp
          # builds clean and looks right until the client resizes — nothing
          # else catches it, which is why it is asserted on the generated conf.
          float-conf-assertions =
            pkgs.runCommand "float-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.gnused pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
            } ''
              # Join backslash-continued lines: the enrich card's bind spans
              # several, with its stamp on the last one.
              sed -e :a -e '/\\$/N; s/\\\n//; ta' "$CONF" >joined

              [ "$(grep -cE '^bind(-key)? .*new-pane' joined)" -ge 1 ]
              if grep -E '^bind(-key)? .*new-pane' joined | grep -v '@float_geom'; then
                echo "float bind above has no @float_geom stamp — tmux-float-refit cannot refit it" >&2
                exit 1
              fi

              # And the remain-on-exit pin (#587), asserted for the same reason:
              # a bind that forgets it looks right until it is pressed inside a
              # mirror window, whose own remain-on-exit the pane inherits.
              if grep -E '^bind(-key)? .*new-pane' joined | grep -v 'remain-on-exit off'; then
                echo "float bind above does not pin remain-on-exit off — its pane will linger dead inside a mirror window" >&2
                exit 1
              fi

              grep -E 'set-hook -g window-resized .*/nix/store/[^ ]*/bin/tmux-float-refit #\{q:window_id\}' "$CONF"

              # ORDER, as for the alert hooks above: the clear must precede the
              # setter, or every config load erases the hook it just set.
              clear=$(grep -n 'set-hook -gu window-resized' "$CONF" | head -1 | cut -d: -f1)
              setter=$(grep -n 'set-hook -g window-resized ' "$CONF" | head -1 | cut -d: -f1)
              [ "$clear" -lt "$setter" ]
              touch $out
            '';

          # nix build .#default cannot verify this: it imports tmux.conf.nix
          # with no terminal options at all, so terminalConfig is already
          # empty there and a default-build grep would be green whether or
          # not the sixel line is ever emitted. Import with sixelTerminals
          # set instead, the way float-conf-assertions imports tmux.conf.nix
          # above.
          sixel-conf-assertions = let
            mkSixelConf = args:
              (import ./config/tmux.conf.nix ({
                  inherit pkgs lib;
                  tmuxPkg = mkTmux pkgs;
                  carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
                  carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
                  prdash = inputs.prdash.packages.${pkgs.system}.prdash;
                }
                // args))
              .tmuxConf;
          in
            pkgs.runCommand "sixel-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep];
              # No preset active, so terminalTerm is null. This is the case the
              # option exists for -- kitty and ghostty are the only presets and
              # neither speaks sixel -- so it must emit on its own.
              ALONE = mkSixelConf {sixelTerminals = ["foot"];};
              # Two entries beside a preset: pins the per-entry expansion (a
              # regression that only shows at N>1) and the join with the
              # RGB:extkeys line the preset emits.
              BOTH = mkSixelConf {
                terminalTerm = "xterm-ghostty";
                sixelTerminals = ["foot" "wezterm"];
              };
            } ''
              grep -F "set -as terminal-features 'foot*:sixel'" "$ALONE"
              grep -F "set -as terminal-features 'xterm-ghostty*:RGB:extkeys'" "$BOTH"
              grep -F "set -as terminal-features 'foot*:sixel'" "$BOTH"
              grep -F "set -as terminal-features 'wezterm*:sixel'" "$BOTH"
              touch $out
            '';

          # The extraction gate (§ The extraction check in
          # docs/superpowers/specs/2026-09-10-og-generate-extract-config-generation-design.md):
          # render the tmux.conf both ways from the same arguments and diff.
          # The matrix is defined here rather than inherited from the flake's own
          # four direct imports, which cover only defaults, sixelTerminals and
          # enrich+ agentUsage off -- never the off branch of splashEnable,
          # notifyEnable or carousel-toggle/prdash, five of the de-indent hazard
          # sites.
          #
          # The template is complete, so this is a whole-file diff: every byte
          # of the candidate comes from Go. The positive control is what shows
          # the diff bites at all -- a stray byte in the template must turn
          # every entry red.
          tmux-conf-extraction-assertions = let
            # A real store path, so entry 10's persist grep asserts the shape
            # the module produces rather than a placeholder string.
            persistStub = pkgs.writeShellScript "persist-wire-stub" ''
              exit 0
            '';

            mkEntry = {
              name,
              args ? {},
              extra ? "",
            }: let
              cfg = import ./config/tmux.conf.nix ({
                  inherit pkgs lib;
                  tmuxPkg = mkTmux pkgs;
                  carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
                  carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
                  prdash = inputs.prdash.packages.${pkgs.system}.prdash;
                }
                // args);
            in {
              inherit name extra;
              inherit (cfg) generatedConf referenceConf configToml;
            };

            # Every flag both ways: entry 12 exists because aiNamingFlag and
            # resumeCarouselFlag are hand-transcribed bool->string maps whose
            # arguments both default to false, so entries 1 and 11 render the
            # same string for each and an inverted map would be invisible.
            matrix = map mkEntry [
              {name = "01-defaults";}
              {
                name = "02-splash-off";
                args.splashEnable = false;
              }
              {
                name = "03-notify-off";
                args.notifyEnable = false;
              }
              {
                name = "04-enrich-off";
                args.enrichEnable = false;
              }
              {
                name = "05-agent-usage-off";
                args.agentUsageEnable = false;
              }
              {
                name = "06-no-optional-tools";
                args = {
                  carousel-toggle = null;
                  carousel-aeye = null;
                  prdash = null;
                };
              }
              {
                name = "07-terminals";
                args = {
                  sixelTerminals = ["foot" "wezterm"];
                  terminalTerm = "xterm-ghostty";
                };
              }
              {
                name = "08a-splash-remote-static";
                args.splashRemote = "static";
              }
              {
                name = "08b-splash-remote-skip";
                args.splashRemote = "skip";
              }
              {
                name = "09-values";
                args = {
                  # Raw dialect (I8): this entry imports tmux.conf.nix directly,
                  # so it never passes through the module's '#' doubling.
                  enrichIcons = {
                    linear = "L#";
                    github = "G#";
                  };
                  defaultShell = "/run/current-system/sw/bin/fish";
                  copyModeLineNumbers = "hybrid";
                  focusFollowsMouse = true;
                };
                # I8's only cover. No check imports the module, so the two
                # dialects are asserted here, per site: `L#` is a substring of
                # `L##`, so each grep carries the closing quote and is scoped to
                # its own line rather than the whole file.
                extra = ''
                  if ! grep -F 'set -g status-format[0]' candidate \
                    | grep -Fq -- "--icon-linear 'L##' --icon-github 'G##'"; then
                    echo "entry 9: status-format[0] must carry the doubled dialect (--icon-linear 'L##')" >&2
                    exit 1
                  fi
                  if ! grep -F -- '--icon-linear' candidate \
                    | grep -Fv 'set -g status-format[0]' \
                    | grep -Fq -- "--icon-linear 'L#' --icon-github 'G#'"; then
                    echo "entry 9: the enrich card must carry the raw dialect (--icon-linear 'L#')" >&2
                    exit 1
                  fi
                '';
              }
              {
                name = "10-persist";
                args = {
                  extraConfText = ''
                    # matrix entry 10
                    set -g @matrix "quoted \"value\" and a # hash"
                  '';
                  persistWireScript = persistStub;
                };
                # I7's deliberate third copy of the persist block's bytes. The
                # reference and the generator are otherwise compared only to
                # each other, so a transcription error shared by both passes
                # the diff; only a literal written here catches it.
                extra = ''
                  marker='# === tmux-remux (Phase 2a, opt-in via programs.tmux-og.persist) ==='
                  # Exactly one, not merely at least one: a doubled block makes
                  # $n multi-line and the arithmetic below dies with a generic
                  # error instead of naming the regression it just found.
                  hits=$(grep -c -Fx "$marker" candidate) || true
                  if [ "$hits" != 1 ]; then
                    echo "entry 10: persist block comment line appears $hits times, want 1" >&2
                    exit 1
                  fi
                  n=$(grep -n -Fx "$marker" candidate | cut -d: -f1)
                  # The block's other two literal lines, in position: a blank
                  # line above and the run-shell below.
                  if [ "$n" -lt 2 ]; then
                    echo "entry 10: persist block is on line 1, so nothing precedes it" >&2
                    exit 1
                  fi
                  if [ -n "$(sed -n "$((n - 1))p" candidate)" ]; then
                    echo "entry 10: persist block is not preceded by a blank line" >&2
                    exit 1
                  fi
                  if ! sed -n "$((n + 1))p" candidate | grep -Fxq 'run-shell "${persistStub} #{q:version}"'; then
                    echo "entry 10: persist block run-shell line does not match" >&2
                    exit 1
                  fi
                '';
              }
              {
                name = "11-all-off";
                args = {
                  splashEnable = false;
                  notifyEnable = false;
                  enrichEnable = false;
                  agentUsageEnable = false;
                  aiNamingEnable = false;
                  resumeClaudeEnable = false;
                  resumeCarouselEnable = false;
                  focusFollowsMouse = false;
                  carousel-toggle = null;
                  carousel-aeye = null;
                  prdash = null;
                };
              }
              {
                name = "12-all-on";
                args = {
                  splashEnable = true;
                  notifyEnable = true;
                  enrichEnable = true;
                  agentUsageEnable = true;
                  aiNamingEnable = true;
                  resumeClaudeEnable = true;
                  resumeCarouselEnable = true;
                  focusFollowsMouse = true;
                };
              }
              # Entry 12's hole, one level down: the leaf values no other entry
              # moves off its default. The gate's whole proof is "render both
              # ways and diff", so a field pinned at its default renders the
              # same bytes whether its plumbing is right or entirely dead, and a
              # wrong hard-coded Go default is invisible.
              #
              # Five of the thirteen are carried for the Nix->TOML key spelling
              # only and reach no tmux.conf byte: enrichProviders, the two
              # enrich refresh seconds, agentUsageRefreshSeconds and
              # claudeStatusAssumeDeadAfter are baked into scripts on the Nix
              # side, so no render diff can witness them.
              {
                name = "13-leaf-values";
                args = {
                  enrichProviders = ["github" "linear"];
                  enrichPrRefreshSeconds = 45;
                  enrichPrCheckRefreshSeconds = 90;
                  zoxideExclude = "*/.ssh,/tmp/*";
                  pickerListRatio = 35;
                  pickerLayout = "list";
                  remoteBridgeHosts = "halo mbp";
                  remoteAuthPersistSeconds = 3600;
                  prefix = "a";
                  agentUsageRefreshSeconds = 60;
                  agentUsageMonthlyThreshold = 75;
                  claudeStatusAssumeDeadAfter = 30;
                  # Only the three agent keys reach tmux.conf, via the usage
                  # segment's icons; the rest of the map is script-side.
                  extraProcessIcons.claude = "C";
                };
                # The entry asserts nothing if its values happen to render the
                # defaults' bytes, which a later change to any of those defaults
                # would quietly make true.
                extra = ''
                  if cmp -s ${(mkEntry {name = "01-defaults";}).generatedConf} candidate; then
                    echo "entry 13: renders the defaults' bytes, so it varies nothing" >&2
                    exit 1
                  fi
                '';
              }
            ];

            # --prefix resolves the optional tools by existence under DIR/bin,
            # and DIR does not exist in the smoke, so every optional renders off.
            # Entry 6 is the one that also has them off, so it is the only entry
            # line-for-line comparable with that render. Selected from the built
            # matrix: re-running mkEntry on a bare name rebuilds the DEFAULTS
            # entry under that label, optionals and all.
            smokeEntry =
              lib.findFirst (e: e.name == "06-no-optional-tools")
              (throw "extraction smoke: matrix has no 06-no-optional-tools entry")
              matrix;

            # Shared by every store-path assertion. `[ -s ]` first, because an
            # empty file passes a negative grep vacuously; and grep's exit 2 (an
            # unreadable path, say) is a real error, not "no match" -- reading it
            # as clean is how a broken check reports success.
            storePathHelper = ''
              no_store_path() {
                label=$1
                file=$2
                if [ ! -s "$file" ]; then
                  echo "$label: $file is empty" >&2
                  exit 1
                fi
                rc=0
                grep -F /nix/store "$file" >&2 || rc=$?
                case $rc in
                  0)
                    echo "$label: carries a store path" >&2
                    exit 1
                    ;;
                  1) ;;
                  *)
                    echo "$label: grep failed on $file (exit $rc)" >&2
                    exit 1
                    ;;
                esac
              }
            '';

            entryCheck = e: ''
              echo "=== ${e.name}"
              cat ${e.generatedConf} >candidate
              if ! diff -u ${e.referenceConf} candidate >delta; then
                echo "${e.name}: generated tmux.conf differs from the frozen reference" >&2
                head -200 delta >&2
                exit 1
              fi

              # Scoped to the matrix's own config.toml files on purpose: this is
              # NOT an invariant of config.toml in general, because extra_config
              # copies cfg.extraConfig verbatim and a Nix user may legitimately
              # interpolate a store path into it.
              no_store_path "${e.name} config.toml" ${e.configToml}

              # The one property tests/verify-extraction.sh structurally cannot
              # see: every consumer of tmuxConf, tests/test-display.sh's wrapper
              # scrape included, needs a regular file named *-tmux.conf.
              conf=${e.generatedConf}
              [ -f "$conf" ]
              case "$conf" in
                *-tmux.conf) ;;
                *)
                  echo "${e.name}: $conf is not a *-tmux.conf store path" >&2
                  exit 1
                  ;;
              esac

              ${e.extra}
            '';
          in
            pkgs.runCommand "tmux-conf-extraction-assertions" {
              nativeBuildInputs = [pkgs.diffutils pkgs.gnugrep pkgs.gnused pkgs.coreutils];
              # Same derivation packages.og-generate builds; identical inputs, one
              # store path, so this costs nothing extra.
              OG_GENERATE = "${pkgs.callPackage ./generator {}}/bin/og-generate";
              OG_INIT = "${pkgs.callPackage ./generator {}}/bin/og-init";
              SMOKE_CONFIG = smokeEntry.configToml;
              SMOKE_REFERENCE = smokeEntry.referenceConf;
              TEMPLATE = ./config/tmux.conf.tmpl;
            } (storePathHelper
              + lib.concatMapStrings entryCheck matrix
              + ''
                # --prefix is the resolver's second mode; the smoke proves it
                # renders and that nothing store-shaped leaks into the output.
                # It is the one render path with no reference diff behind it, so
                # the assertions have to be positive: an empty file passes both
                # `-f` and a negative grep, which made a render-nothing
                # regression read as a clean pass.
                echo "=== --prefix smoke"
                mkdir -p prefixout
                "$OG_GENERATE" --config "$SMOKE_CONFIG" --prefix /opt/tmux-og \
                  --template "$TEMPLATE" --out prefixout
                if ! grep -q '^set -g ' prefixout/tmux.conf; then
                  echo "--prefix render carries no 'set -g' line" >&2
                  exit 1
                fi
                if ! grep -Fq /opt/tmux-og/bin/ prefixout/tmux.conf; then
                  echo "--prefix render carries no path under the given prefix" >&2
                  exit 1
                fi
                # Presence is not completeness: the two greps above sit at lines
                # 2 and 32 of a ~550-line render, so a truncation past those
                # still satisfies them. The prefix render differs from its
                # reference only in path text, and a path holds no newline, so
                # the line counts must agree exactly.
                want=$(wc -l <"$SMOKE_REFERENCE")
                got=$(wc -l <prefixout/tmux.conf)
                if [ "$got" != "$want" ]; then
                  echo "--prefix render is $got lines, reference is $want" >&2
                  exit 1
                fi

                no_store_path "--prefix render" prefixout/tmux.conf

                # og init's own output, fed straight into og generate --prefix:
                # the one place the pair is proven to work end to end, since a
                # Go-level test can't reach the real template (generator/'s
                # buildGoModule src is scoped to generator/ alone, so
                # config/tmux.conf.tmpl sits outside its sandbox).
                echo "=== og init -> og generate --prefix"
                "$OG_INIT" --out init.toml
                mkdir -p initout
                "$OG_GENERATE" --config init.toml --prefix /opt/tmux-og \
                  --template "$TEMPLATE" --out initout
                if ! grep -q '^set -g ' initout/tmux.conf; then
                  echo "og init render carries no 'set -g' line" >&2
                  exit 1
                fi
                if ! grep -Fq /opt/tmux-og/bin/ initout/tmux.conf; then
                  echo "og init render carries no path under the given prefix" >&2
                  exit 1
                fi
                no_store_path "og init render" initout/tmux.conf

                touch $out
              '');

          # The og dispatcher's contract
          # (docs/superpowers/specs/2026-09-10-og-dispatcher-design.md). Never
          # parses the generated table file -- every target assertion goes
          # through `og <verb> --help`, whose last line is the absolute
          # target path alone (scripts/og.sh's pinned help shape). Argument
          # passthrough is checked by re-instantiating mkOg over a stub
          # table.
          og-dispatch-assertions = let
            stub = pkgs.writeShellScriptBin "stub" ''
              echo "$#"
              printf '[%s]\n' "$@"
            '';
            ogStub = tmuxConfig.mkOg {
              "t echo" = {
                target = "${stub}/bin/stub";
                summary = "stub";
              };
              # A noun with both a bare verb and a subverb, mirroring status/
              # notify's shape -- proves the flag-vs-subverb distinction
              # without depending on a real script's own argument grammar.
              "s" = {
                target = "${stub}/bin/stub";
                summary = "stub bare";
              };
              "s x" = {
                target = "${stub}/bin/stub";
                summary = "stub sub";
              };
            };
          in
            pkgs.runCommand "og-dispatch-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.coreutils];
              OG = tmuxConfig.og;
              OG_STUB = ogStub;
              CONF = tmuxConfig.tmuxConf;
              TMUX_WRAPPED = tmuxConfig.tmux-wrapped;
              REMOTE_PICKER = "${tmuxConfig.script.og-remote-picker}/bin/og-remote-picker";
              # Derived from ogVerbSpec itself (not retyped here) so a verb
              # added there is asserted on automatically instead of silently
              # skipping coverage.
              VERBS = lib.concatStringsSep "\n" (builtins.attrNames tmuxConfig.ogVerbSpec);
            } ''
              OG_BIN="$OG/bin/og"
              OG_STUB_BIN="$OG_STUB/bin/og"

              verbs="$VERBS"

              # 1: bare `og` and `og help` both list every verb, exit 0.
              bare_out=$("$OG_BIN")
              help_out=$("$OG_BIN" help)
              while read -r v; do
                grep -qE "^  $v( |\$)" <<<"$bare_out"
                grep -qE "^  $v( |\$)" <<<"$help_out"
              done <<<"$verbs"

              # `og remote` prints exactly that noun's 5 verbs, exit 0.
              remote_out=$("$OG_BIN" remote)
              for v in "remote open" "remote picker" "remote detach" "remote auth" "remote theme"; do
                grep -qE "^  $v( |\$)" <<<"$remote_out"
              done
              remote_count=$(grep -cE '^  ' <<<"$remote_out")
              [ "$remote_count" -eq 5 ]

              # 2: every target printed by --help is executable -- except a
              # bare noun that also owns subverbs (status, notify), where
              # "--help" means noun help (see check 12) rather than this
              # verb's own summary/target.
              while read -r v; do
                case "$v" in
                  *' '*) ;;
                  *)
                    if grep -qE "^$v " <<<"$verbs"; then
                      continue
                    fi
                    ;;
                esac
                # shellcheck disable=SC2086
                target=$("$OG_BIN" $v --help | tail -1)
                [ -x "$target" ]
              done <<<"$verbs"

              # 3: two-token verbs resolve to the two-token target, not the
              # one-token noun with the second token as a stray argument.
              su_target=$("$OG_BIN" status update --help | tail -1)
              case "$su_target" in
                */bin/claude-status-update) ;;
                *)
                  echo "og status update resolved to $su_target" >&2
                  exit 1
                  ;;
              esac
              nc_target=$("$OG_BIN" notify center --help | tail -1)
              case "$nc_target" in
                */bin/og-notify-center) ;;
                *)
                  echo "og notify center resolved to $nc_target" >&2
                  exit 1
                  ;;
              esac

              # 4: `og remote picker` execs the same derivation
              # exposePickOnPath puts on PATH.
              rp_target=$("$OG_BIN" remote picker --help | tail -1)
              [ "$rp_target" = "$REMOTE_PICKER" ]

              # 5: `og remote open --help` exits 0, prints the target,
              # never execs it (no ssh in the sandbox to attempt).
              ro_target=$("$OG_BIN" remote open --help | tail -1)
              [ -x "$ro_target" ]

              # 6: an unknown token exits 2 with empty stdout.
              rc=0
              bogus_out=$("$OG_BIN" bogus 2>/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              [ -z "$bogus_out" ]

              # 7: arguments after the verb reach the target verbatim --
              # a flag, a `--`, and an empty argument all survive.
              stub_out=$("$OG_STUB_BIN" t echo foo --flag -- bar "")
              stub_count=$(head -n1 <<<"$stub_out")
              [ "$stub_count" = "5" ]
              grep -qxF '[foo]' <<<"$stub_out"
              grep -qxF '[--flag]' <<<"$stub_out"
              grep -qxF '[--]' <<<"$stub_out"
              grep -qxF '[bar]' <<<"$stub_out"
              grep -qxF '[]' <<<"$stub_out"

              # 8: og "" (bug: an empty first token used to hit bash's
              # "bad array subscript") exits 2 with empty stdout.
              rc=0
              empty_out=$("$OG_BIN" "" 2>/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              [ -z "$empty_out" ]

              # 9: og remote --help (bug: any noun-help spelling other than
              # bare `og remote` used to hit unknown_command) exits 0 and
              # lists the same 5 remote verbs.
              rc=0
              remote_help_out=$("$OG_BIN" remote --help) || rc=$?
              [ "$rc" -eq 0 ]
              for v in "remote open" "remote picker" "remote detach" "remote auth" "remote theme"; do
                grep -qE "^  $v( |\$)" <<<"$remote_help_out"
              done
              remote_help_count=$(grep -cE '^  ' <<<"$remote_help_out")
              [ "$remote_help_count" -eq 5 ]

              # 12: a noun that owns both a bare one-token verb and subverbs
              # (status, notify) must not let the bare match swallow a second
              # token -- that bypassed unknown_command/print_noun_help below
              # for exactly those two nouns.
              rc=0
              status_bogus_out=$("$OG_BIN" status frobnicate 2>&1 >/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              grep -qF "unknown command: status frobnicate" <<<"$status_bogus_out"

              status_help_out=$("$OG_BIN" status --help)
              grep -qE "^  status update( |\$)" <<<"$status_help_out"

              # notify goes through the identical code path as status; cover
              # it too rather than asserting on one representative noun.
              rc=0
              notify_bogus_out=$("$OG_BIN" notify frobnicate 2>&1 >/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              grep -qF "unknown command: notify frobnicate" <<<"$notify_bogus_out"

              notify_help_out=$("$OG_BIN" notify --help)
              grep -qE "^  notify center( |\$)" <<<"$notify_help_out"

              # a flag after the bare verb still passes through -- only a
              # bare word is ambiguous with a subverb attempt. Exercised on
              # the stub table (see ogStub's "s"/"s x" pair above) rather
              # than the real status/notify scripts, whose own argument
              # grammar this derivation has no business asserting on.
              stub_flag_out=$("$OG_STUB_BIN" s --flag foo)
              stub_flag_count=$(head -n1 <<<"$stub_flag_out")
              [ "$stub_flag_count" = "2" ]
              grep -qxF '[--flag]' <<<"$stub_flag_out"
              grep -qxF '[foo]' <<<"$stub_flag_out"

              rc=0
              stub_bogus_out=$("$OG_STUB_BIN" s frobnicate 2>&1 >/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              grep -qF "unknown command: s frobnicate" <<<"$stub_bogus_out"

              # the existing `remote` noun (subverbs only, no bare verb) is
              # unchanged by the guard above.
              rc=0
              remote_bogus_out=$("$OG_BIN" remote frobnicate 2>&1 >/dev/null) || rc=$?
              [ "$rc" -eq 2 ]
              grep -qF "unknown command: remote frobnicate" <<<"$remote_bogus_out"

              # 10: `og` never appears in the generated tmux config.
              if grep -qF "$OG" "$CONF"; then
                echo "og store path leaked into the generated tmux config" >&2
                exit 1
              fi

              # 11: `og` also never reaches the tmux server's own PATH --
              # only scripts partitioned into ogVerbSpec/ogInternal do.
              # wrapProgram moves the pristine binary to bin/.tmux-wrapped and
              # writes the PATH-bearing wrapper script to bin/tmux -- grepping
              # the pristine binary would never see the PATH text at all.
              if grep -qF "$OG" "$TMUX_WRAPPED/bin/tmux"; then
                echo "og store path leaked into the tmux wrapper's PATH" >&2
                exit 1
              fi

              touch $out
            '';

          default-size-conf-assertions =
            pkgs.runCommand "default-size-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
            } ''
              grep -q 'set-hook -g client-attached\[20\]' "$CONF"
              grep -q 'set-hook -g client-resized\[20\]' "$CONF"
              grep -q 'set-hook -gu client-attached\[20\]' "$CONF"
              grep -q 'set-hook -gu client-resized\[20\]' "$CONF"
              grep -E 'client-attached\[20\].*/nix/store/[^ ]*/bin/tmux-default-size' "$CONF"
              grep -E 'client-resized\[20\].*/nix/store/[^ ]*/bin/tmux-default-size' "$CONF"

              attach_clear=$(grep -n 'set-hook -gu client-attached\[20\]' "$CONF" | head -1 | cut -d: -f1)
              attach_set=$(grep -n 'set-hook -g client-attached\[20\]' "$CONF" | head -1 | cut -d: -f1)
              [ "$attach_clear" -lt "$attach_set" ]
              resize_clear=$(grep -n 'set-hook -gu client-resized\[20\]' "$CONF" | head -1 | cut -d: -f1)
              resize_set=$(grep -n 'set-hook -g client-resized\[20\]' "$CONF" | head -1 | cut -d: -f1)
              [ "$resize_clear" -lt "$resize_set" ]
              touch $out
            '';

          # Build-time guard for the #603 tick floor's wiring in
          # config/tmux.conf.nix: four `-B` session monitors driving the
          # former status-format[0] poller jobs. Modelled on
          # default-size-conf-assertions above. The store-path setter strings
          # are built from tmuxConfig.script so a future binary rename can't
          # silently desync the check from the config.
          tick-floor-conf-assertions = let
            # \\\" (not \") -- the emitted conf wraps each run-shell command in
            # BACKSLASH-escaped quotes (it's itself quoted by the outer
            # if-shell body string), so the literal substring to match is
            # backslash-quote, not a bare quote.
            prSetter = "set-hook -g -B '@og-pr-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-pr-enrich}/bin/tmux-pr-enrich --tick\\\"'";
            backfillSetter = "set-hook -g -B '@og-backfill-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-issue-stamp}/bin/tmux-issue-stamp --backfill\\\"'";
            usageSetter = "set-hook -g -B '@og-usage-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-agent-usage}/bin/tmux-agent-usage --tick\\\"'";
            sweepSetter = "set-hook -g -B '@og-sweep-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"OG_TICK_SWEEP=1 ${tmuxConfig.script.tmux-update-icons}/bin/tmux-update-icons\\\"'";
          in
            pkgs.runCommand "tick-floor-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.gawk pkgs.gnused pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
              TICK_OPT = "set -g @og_tick '%s'";
              PR_CLEAR_B = "set-hook -g -u -B '@og-pr-tick'";
              PR_CLEAR_OPT = "set -gu '@og-pr-tick'";
              BACKFILL_CLEAR_B = "set-hook -g -u -B '@og-backfill-tick'";
              BACKFILL_CLEAR_OPT = "set -gu '@og-backfill-tick'";
              USAGE_CLEAR_B = "set-hook -g -u -B '@og-usage-tick'";
              USAGE_CLEAR_OPT = "set -gu '@og-usage-tick'";
              SWEEP_CLEAR_B = "set-hook -g -u -B '@og-sweep-tick'";
              SWEEP_CLEAR_OPT = "set -gu '@og-sweep-tick'";
              PR_SETTER = prSetter;
              BACKFILL_SETTER = backfillSetter;
              USAGE_SETTER = usageSetter;
              SWEEP_SETTER = sweepSetter;
              # The guard's condition-close / string-branch-open join, which
              # only exists when the hooks sit inside if-shell's STRING form
              # ("..." "...") rather than a brace block -- tmux parses every
              # branch of a { } block at source time, so -B would be rejected
              # even on the untaken branch of a pre-3.8 server.
              GUARD_JOIN = "grep -q -- -B\" \"set-hook -g -u -B '@og-pr-tick'";
            } ''
              grep -qF "$TICK_OPT" "$CONF"

              for v in PR_CLEAR_B PR_CLEAR_OPT BACKFILL_CLEAR_B BACKFILL_CLEAR_OPT \
                       USAGE_CLEAR_B USAGE_CLEAR_OPT SWEEP_CLEAR_B SWEEP_CLEAR_OPT \
                       PR_SETTER BACKFILL_SETTER USAGE_SETTER SWEEP_SETTER GUARD_JOIN; do
                pat="''${!v}"
                grep -qF "$pat" "$CONF" || {
                  echo "missing from conf ($v): $pat" >&2
                  exit 1
                }
              done

              # Highest-value line in this check (upstream 557967c3): the
              # empty-target monitor spec ('@name::') is the only spelling
              # that survives a flake.lock bump past that commit, and
              # ':session:' must never appear anywhere in the emitted conf.
              # Written as an explicit if/exit, not a bare `! grep ...`: bash's
              # errexit never fires on a command whose status is inverted by
              # `!`, so that form would silently never catch a regression.
              if grep -qF ':session:' "$CONF"; then
                echo "a -B monitor spec uses ':session:' -- only the empty target ('::') survives upstream 557967c3" >&2
                exit 1
              fi

              # status-format[0] no longer smuggles the poller jobs through.
              fmt0="$(grep -F 'set -g status-format[0]' "$CONF")"
              case "$fmt0" in
                *--tick*|*--backfill*)
                  echo "status-format[0] still carries a --tick/--backfill job" >&2
                  exit 1
                  ;;
              esac

              # Every clear precedes every setter in the emitted text -- the
              # ordering is what makes a disable (enrich.enable = false, etc.)
              # actually take effect rather than leave a stale monitor firing
              # at a store path GC will remove.
              line="$(grep -F 'if-shell "tmux list-commands set-hook' "$CONF")"
              [ -n "$line" ] || { echo "tick-floor guard line not found" >&2; exit 1; }

              clear_max=-1
              for v in "$PR_CLEAR_B" "$PR_CLEAR_OPT" "$BACKFILL_CLEAR_B" "$BACKFILL_CLEAR_OPT" \
                       "$USAGE_CLEAR_B" "$USAGE_CLEAR_OPT" "$SWEEP_CLEAR_B" "$SWEEP_CLEAR_OPT"; do
                prefix="''${line%%"$v"*}"
                [ "$prefix" != "$line" ] || { echo "clear not found in guard line: $v" >&2; exit 1; }
                [ "''${#prefix}" -gt "$clear_max" ] && clear_max="''${#prefix}"
              done
              setter_min=-1
              for v in "$PR_SETTER" "$BACKFILL_SETTER" "$USAGE_SETTER" "$SWEEP_SETTER"; do
                prefix="''${line%%"$v"*}"
                [ "$prefix" != "$line" ] || { echo "setter not found in guard line: $v" >&2; exit 1; }
                if [ "$setter_min" -eq -1 ] || [ "''${#prefix}" -lt "$setter_min" ]; then
                  setter_min="''${#prefix}"
                fi
              done
              [ "$clear_max" -lt "$setter_min" ] || {
                echo "a clear (offset $clear_max) does not precede every setter (offset $setter_min)" >&2
                exit 1
              }

              # A hook COMMAND (not the monitor spec, which is legitimately a
              # bare #{T:...} format evaluated by hooks_monitor_add itself)
              # must carry NO #{ at all, not merely a #{q: form. -B monitor
              # hooks format-expand their action string and then re-lex the
              # result with tmux's own command parser -- which strips the
              # very backslashes #{q:} inserts -- before run-shell expands a
              # second time; #{q:start_time} was inert only because a start
              # time is pure digits, the kind of idiom someone copies next
              # for a format that isn't. All four commands are pure store paths
              # plus literal flags, so the invariant is enforceable as written.
              cmds="$(echo "$line" | awk -F'\\\\"' '{for(i=2;i<NF;i+=2) print $i}')"
              [ -n "$cmds" ] || { echo "no hook commands extracted from guard line" >&2; exit 1; }
              n=0
              while IFS= read -r cmd; do
                n=$((n + 1))
                case "$cmd" in
                  *'#{'*)
                    echo "bare #{ in hook command: $cmd" >&2
                    exit 1
                    ;;
                esac
              done <<<"$cmds"
              # Still 4, not 8, with sixteen clear literals above: n counts the
              # \"-wrapped run-shell payloads (awk's odd fields), and a clear
              # carries no command payload at all.
              [ "$n" -eq 4 ] || { echo "expected 4 hook commands, got $n" >&2; exit 1; }

              touch $out
            '';

          # nix build .#default cannot verify the disabled path: enrichEnable
          # and agentUsageEnable both default true there, so a regression that
          # made the clears conditional on a feature flag (leaving a stale
          # monitor pointed at a store path GC will remove) would pass every
          # existing check. Modelled on sixel-conf-assertions above: import
          # tmux.conf.nix directly with the flags off, the way
          # nix build .#default never does.
          tick-floor-disabled-conf-assertions = let
            disabledTmuxConfig = import ./config/tmux.conf.nix {
              inherit pkgs lib;
              tmuxPkg = mkTmux pkgs;
              carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
              carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
              prdash = inputs.prdash.packages.${pkgs.system}.prdash;
              enrichEnable = false;
              agentUsageEnable = false;
            };
            disabledConf = disabledTmuxConfig.tmuxConf;
            # The pr/backfill/usage setters are built off the DEFAULT
            # tmuxConfig.script -- they must be absent regardless of exact
            # store path, since the whole setHook line is omitted when its
            # flag is off. The sweep setter is NOT optional, so it must be
            # built off disabledTmuxConfig.script instead: tmux-update-icons's
            # OWN @issue_stamp@ substitution differs with enrichEnable off,
            # which changes its store path independently of this setter list.
            prSetter = "set-hook -g -B '@og-pr-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-pr-enrich}/bin/tmux-pr-enrich --tick\\\"'";
            backfillSetter = "set-hook -g -B '@og-backfill-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-issue-stamp}/bin/tmux-issue-stamp --backfill\\\"'";
            usageSetter = "set-hook -g -B '@og-usage-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"${tmuxConfig.script.tmux-agent-usage}/bin/tmux-agent-usage --tick\\\"'";
            sweepSetter = "set-hook -g -B '@og-sweep-tick::#{e|/|:#{T:@og_tick},5}' 'run-shell -b \\\"OG_TICK_SWEEP=1 ${disabledTmuxConfig.script.tmux-update-icons}/bin/tmux-update-icons\\\"'";
          in
            pkgs.runCommand "tick-floor-disabled-conf-assertions" {
              nativeBuildInputs = [pkgs.gnugrep];
              CONF = disabledConf;
              PR_CLEAR_B = "set-hook -g -u -B '@og-pr-tick'";
              PR_CLEAR_OPT = "set -gu '@og-pr-tick'";
              BACKFILL_CLEAR_B = "set-hook -g -u -B '@og-backfill-tick'";
              BACKFILL_CLEAR_OPT = "set -gu '@og-backfill-tick'";
              USAGE_CLEAR_B = "set-hook -g -u -B '@og-usage-tick'";
              USAGE_CLEAR_OPT = "set -gu '@og-usage-tick'";
              SWEEP_CLEAR_B = "set-hook -g -u -B '@og-sweep-tick'";
              SWEEP_CLEAR_OPT = "set -gu '@og-sweep-tick'";
              PR_SETTER = prSetter;
              BACKFILL_SETTER = backfillSetter;
              USAGE_SETTER = usageSetter;
              SWEEP_SETTER = sweepSetter;
            } ''
              # All eight clears survive a disabled feature -- four names times
              # the -B and option forms. They're keyed off the fixed hookNames
              # list, never the enable flags, which is what makes disabling a
              # feature actually drop its stale monitor on reload instead of
              # leaving argv's previous generation armed.
              for v in PR_CLEAR_B PR_CLEAR_OPT BACKFILL_CLEAR_B BACKFILL_CLEAR_OPT \
                       USAGE_CLEAR_B USAGE_CLEAR_OPT SWEEP_CLEAR_B SWEEP_CLEAR_OPT; do
                pat="''${!v}"
                grep -qF "$pat" "$CONF" || {
                  echo "disabled conf is missing a clear ($v): $pat" >&2
                  exit 1
                }
              done

              grep -qF "$SWEEP_SETTER" "$CONF" || {
                echo "disabled conf is missing the unconditional sweep setter" >&2
                exit 1
              }

              for v in PR_SETTER BACKFILL_SETTER USAGE_SETTER; do
                pat="''${!v}"
                if grep -qF "$pat" "$CONF"; then
                  echo "disabled conf still emits a gated setter ($v): $pat" >&2
                  exit 1
                fi
              done

              touch $out
            '';

          # The #603 tick floor's poller and sweep hooks fire inside any server
          # started from tmuxConfig.tmux-wrapped and reach functions that delete
          # files under CLAUDE_STATUS_DIR, OG_ENRICH_CACHE_DIR,
          # OG_AGENT_USAGE_DIR and OG_ENRICH_LOCK_DIR — whose defaults are the
          # developer's real /tmp trees. A bats suite that sets TMUX_BIN loads
          # that config, so it must export all four in its own setup() or a
          # future one added without isolation is destructive the moment it runs
          # locally, while nix flake check stays green (the sandbox's
          # /tmp/claude-status is empty).
          wrapped-tmux-suite-isolation-assertions =
            pkgs.runCommand "wrapped-tmux-suite-isolation-assertions" {
              nativeBuildInputs = [pkgs.gnugrep];
            } ''
              fail=0
              for f in ${./tests}/*.bats; do
                grep -q 'TMUX_BIN' "$f" || continue
                for var in CLAUDE_STATUS_DIR OG_ENRICH_CACHE_DIR OG_AGENT_USAGE_DIR OG_ENRICH_LOCK_DIR; do
                  grep -q "export $var=" "$f" || {
                    echo "$(basename "$f") references TMUX_BIN but does not export $var" >&2
                    fail=1
                  }
                done
              done
              [ "$fail" -eq 0 ]
              touch $out
            '';

          # A control byte is invisible in review and only misbehaves for clients
          # without a UTF-8 locale, so the delimiter rule needs a build-time gate
          # rather than vigilance (#373). Shell sources: scripts/, config/, modules/.
          # picker/ is gated by go test ./tmuxformat/... inside picker-go-tests.
          tmux-format-delimiter-assertions =
            pkgs.runCommand "tmux-format-delimiter-assertions" {
              nativeBuildInputs = [pkgs.bash pkgs.coreutils pkgs.findutils];
            } ''
              # `bash "$scan"`, not "$scan": the scanner's shebang is
              # /usr/bin/env bash and the Linux build sandbox has no /usr/bin.
              scan=${./tests/check-tmux-format-delimiters.sh}
              bash "$scan" ${./scripts}
              bash "$scan" ${./config}
              bash "$scan" ${./modules}

              # Prove the scanner can actually fail, and that EACH rule pulls its
              # weight: a single non-zero exit would let an inverted rule ship.
              if report=$(bash "$scan" ${./tests/fixtures/tmux-format-delimiters} 2>&1); then
                echo "scanner accepted the deliberately-broken fixtures" >&2
                exit 1
              fi
              for f in literal-tab escaped-tab var-indirect newline-escape us-escape hex-tab-escape; do
                case "$report" in
                  *"$f"*) ;;
                  *) echo "scanner missed fixture $f:" >&2; echo "$report" >&2; exit 1 ;;
                esac
              done
              touch $out
            '';

          notify-bell-integration-tests =
            pkgs.runCommand "notify-bell-integration-tests" {
              # mkTmux, not pkgs.tmux: the hook behavior this pins must be the
              # tmux that actually ships.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils (mkTmux pkgs)];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              export HOME=$TMPDIR
              bats tests/notify-bell-integration.bats
              touch $out
            '';

          log-tests =
            pkgs.runCommand "log-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.util-linux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/log.bats
              touch $out
            '';

          splash-tests =
            pkgs.runCommand "splash-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/splash.bats
              touch $out
            '';

          # Regression guard for the command-injection class of issue #355,
          # which has now been introduced three times. Asserted on the EMITTED
          # conf, never the Nix source — that layer plus tmux's own quoting make
          # the source unreliable to eyeball. A presence grep for today's
          # #{q:...} would not hold: this has to reject the NEXT bare one, which
          # is why it is a scanner rather than a grep. Rule and scope live in
          # tests/conf-shell-quoting.bats.
          conf-shell-quoting-tests =
            pkgs.runCommand "conf-shell-quoting-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
              CONF = tmuxConfig.tmuxConf;
            } ''
              cp -r ${./tests} tests
              bats tests/conf-shell-quoting.bats
              touch $out
            '';

          interrupt-tests =
            pkgs.runCommand "interrupt-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/interrupt.bats
              touch $out
            '';

          naming-seed-tests =
            pkgs.runCommand "naming-seed-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.jq pkgs.coreutils];
            } ''
              cp -r ${./claude-plugin} claude-plugin
              cp -r ${./tests} tests
              bats tests/naming-seed.bats
              touch $out
            '';

          reconcile-tests =
            pkgs.runCommand "reconcile-tests" {
              # git: the test derives tags from a real repo it builds in $HOME.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.git];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/reconcile.bats
              touch $out
            '';

          issue-stamp-tests =
            pkgs.runCommand "issue-stamp-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/issue-stamp.bats
              touch $out
            '';

          issue-backfill-tests =
            pkgs.runCommand "issue-backfill-tests" {
              # git: one test builds a real repo in $HOME to exercise the
              # live-branch-vs-stale-argument override (like reconcile-tests).
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.git];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/issue-backfill.bats
              touch $out
            '';

          enrich-command-tests =
            pkgs.runCommand "enrich-command-tests" {
              # git: the test derives worktree/branch from a real repo it builds
              # in $HOME (like reconcile-tests).
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.git];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/enrich-command.bats
              touch $out
            '';

          update-icons-enrich-trigger-tests =
            pkgs.runCommand "update-icons-enrich-trigger-tests" {
              # tmux: drives a private, config-less server (like reflow-fanout-tests);
              # git: builds a real repo in $HOME to exercise a real branch transition.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.git pkgs.tmux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/update-icons-enrich-trigger.bats
              touch $out
            '';

          update-icons-cwd-move-tests =
            pkgs.runCommand "update-icons-cwd-move-tests" {
              # tmux: drives a private, config-less server (like reflow-fanout-tests);
              # git: builds two real repos to exercise a genuine cwd move.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.git pkgs.tmux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/update-icons-cwd-move.bats
              touch $out
            '';

          update-icons-resume-guard-tests =
            pkgs.runCommand "update-icons-resume-guard-tests" {
              # tmux: drives a private, config-less server (like reflow-fanout-tests);
              # git: builds a real repo in $HOME to exercise a real branch transition.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.git pkgs.tmux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/update-icons-resume-guard.bats
              touch $out
            '';

          update-icons-all-windows-tests =
            pkgs.runCommand "update-icons-all-windows-tests" {
              # tmux: drives a private, config-less server (like reflow-fanout-tests);
              # git: builds a real repo so unseeded @branch seeding has a cwd.
              # bash: copied to a binary named `claude` so pane_current_command is literal.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.git pkgs.tmux pkgs.bash];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/update-icons-all-windows.bats
              touch $out
            '';

          carousel-restore-tests =
            pkgs.runCommand "carousel-restore-tests" {
              # tmux: drives a private, config-less server (like
              # update-icons-resume-guard-tests above). No gnused: both
              # @carousel_aeye@ and @AGENT_COMMANDS@ have documented
              # env-var test seams (AEYE_BIN / AGENT_COMMANDS), so the raw
              # script runs unsubstituted.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.tmux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/carousel-restore.bats
              touch $out
            '';

          # tmux-carousel-restore recomputes aeye's manifest key itself, so a
          # change on aeye's side breaks the carousel silently — it opens, finds
          # nothing, and reads as an unrelated bug. Pinned BEHAVIOURALLY: ask
          # aeye's own shipped launcher what key it derives (`--resolve` is that
          # script's documented seam and needs no tmux server, it parses $TMUX as
          # a string) and compare against the shape this repo hardcodes. A
          # substring grep for `="$srv-` stays green through a key EXTENSION
          # (`$srv-$pane-$winid`), which is the drift that matters. Reads
          # ${inputs.aeye}, never a local checkout, which would pass here and
          # still miss the drift.
          carousel-key-formula-pin =
            pkgs.runCommand "carousel-key-formula-pin" {
              nativeBuildInputs = [pkgs.gnugrep pkgs.coreutils inputs.aeye.packages.${pkgs.system}.toggle];
            } ''
              # Two distinct pairs, so a reordering ("<pane>-<srv>") cannot pass
              # by coincidence on a single sample.
              for pair in "12345 %7 12345-7" "999 %0 999-0"; do
                set -- $pair
                got="$(TMUX="/tmp/sock,$1,0" TMUX_PANE="$2" tmux-claude-images --resolve | cut -f2)"
                if [ "$got" != "$3" ]; then
                  echo "aeye's manifest key formula changed: TMUX_PANE=$2 srv=$1 now derives '$got', expected '$3'." >&2
                  echo "scripts/tmux-carousel-restore.sh computes <srv>-<pane sans %> and must be updated to match." >&2
                  exit 1
                fi
              done

              # And pin our side, so a change here without one there is equally
              # loud. Anchored on the assignment, not a bare substring.
              grep -qF 'key="$srv-''${HOST#%}"' ${./scripts/tmux-carousel-restore.sh}
              touch $out
            '';

          worktree-match-tests =
            pkgs.runCommand "worktree-match-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gawk pkgs.gnugrep];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/worktree-match.bats
              touch $out
            '';

          worktree-match-integration-tests =
            pkgs.runCommand "worktree-match-integration-tests" {
              # mkTmux, not pkgs.tmux: assert against the tmux that actually ships.
              # No version divergence is known here (both report window options on
              # pane rows); the derivation is already built for the m2 check anyway.
              # Deliberately no LANG/LC_ALL: the sandbox's stripped locale is the
              # hostile case. tmux rewrites non-printable bytes to "_" without
              # UTF-8, which is why the -F format is "|"-delimited rather than
              # tab-delimited — pinning a UTF-8 locale here would hide a
              # regression back to a tab everywhere except the one test that
              # overrides the locale itself.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gawk (mkTmux pkgs)];
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR
              bats tests/worktree-match-integration.bats
              touch $out
            '';

          agent-detect-arm-tests =
            pkgs.runCommand "agent-detect-arm-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-detect-arm.bats
              touch $out
            '';

          agent-liveness-tests =
            pkgs.runCommand "agent-liveness-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-liveness.bats
              touch $out
            '';

          agent-detect-merge-tests =
            pkgs.runCommand "agent-detect-merge-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-detect-merge.bats
              touch $out
            '';

          agent-bg-badge-tests =
            pkgs.runCommand "agent-bg-badge-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-bg-badge.bats
              touch $out
            '';

          agent-detect-enum-tests =
            pkgs.runCommand "agent-detect-enum-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/agent-detect-enum.bats
              touch $out
            '';

          picker-go-tests = pickerChecked;

          reflow-fanout-tests =
            pkgs.runCommand "reflow-fanout-tests" {
              # tmux: the test drives a private, config-less tmux server so the
              # scripts' bare `tmux` calls hit it, never the dev's own server.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.tmux];
              # reflow measures display width, which needs a UTF-8 locale.
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/reflow-fanout.bats
              touch $out
            '';

          default-size-tests =
            pkgs.runCommand "default-size-tests" {
              nativeBuildInputs = [pkgs.bats pkgs.coreutils];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/default-size.bats
              touch $out
            '';

          tmux-next38-readiness-tests =
            pkgs.runCommand "tmux-next38-readiness-tests" {
              # mkTmux via the wrapped default package: exercise the same
              # generated config, plugin store paths, and PATH wrapper users run.
              nativeBuildInputs = [pkgs.bash pkgs.bats pkgs.coreutils pkgs.gawk pkgs.gnugrep pkgs.gnused];
              TMUX_BIN = "${tmuxConfig.tmux-wrapped}/bin/tmux";
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR/home
              mkdir -p "$HOME"
              bats tests/tmux-next38-readiness.bats
              touch $out
            '';

          # Live proof for #603: the monitor-hook floor actually fires on the
          # wrapped server's own clock, with zero clients and with only a
          # control-mode client, which tick-floor-conf-assertions (a text
          # check) cannot demonstrate by itself.
          tick-floor-tests =
            pkgs.runCommand "tick-floor-tests" {
              # mkTmux (not pkgs.tmux) for the same reason as every other
              # live-tmux check here, AND because case 5 runs a second,
              # hand-written-config server directly on the bare `tmux` this
              # provides (deliberately NOT the wrapped TMUX_BIN, to pin plain
              # tmux's own status-line behaviour rather than anything this
              # repo's config does). gnugrep is NOT optional the way it is in
              # tmux-next38-readiness-tests: the -B guard in
              # config/tmux.conf.nix shells out to
              # `tmux list-commands set-hook | grep -- -B`, so a PATH with no
              # grep makes that guard fail closed and the suite would pass or
              # fail for the wrong reason. util-linux supplies `script`, which
              # case 5 uses to give a real attach a pty.
              nativeBuildInputs = [pkgs.bash pkgs.bats pkgs.coreutils pkgs.gnugrep pkgs.util-linux (mkTmux pkgs)];
              TMUX_BIN = "${tmuxConfig.tmux-wrapped}/bin/tmux";
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR/home
              mkdir -p "$HOME"
              bats tests/tick-floor.bats
              touch $out
            '';

          remote-tests =
            pkgs.runCommand "remote-tests" {
              # bash: the cold-start cases run the launcher through an explicit
              # interpreter (no /usr/bin/env in the sandbox). util-linux
              # provides `script`, which remote-auth.bats uses to give the
              # accept-path cases a real pty (same pattern as
              # remote-bridge-integration-tests below).
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.gnugrep pkgs.bash pkgs.util-linux];
            } ''
              cp -r ${./scripts} scripts
              cp -r ${./tests} tests
              bats tests/remote.bats
              bats tests/remote-cold-start.bats
              bats tests/remote-picker.bats
              bats tests/remote-auth.bats
              bats tests/remote-theme.bats
              touch $out
            '';

          remote-bridge-integration-tests =
            pkgs.runCommand "remote-bridge-integration-tests" {
              # tmux: same private, config-less server pattern as the other
              # integration tests. The bridge binary is prebuilt via the
              # vendored buildGoModule (pickerChecked) so this check never
              # invokes `go build` — a non-FOD sandbox has no network.
              # util-linux provides `script`, which the real-tty case uses to
              # give the bridge a pty (so refresh-client + real cursor fire).
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.gnugrep pkgs.tmux pkgs.util-linux];
              BRIDGE = "${pickerChecked}/bin/og-remote-bridge";
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR
              bats tests/remote-bridge-integration.bats
              touch $out
            '';

          remote-m2-integration-tests =
            pkgs.runCommand "remote-m2-integration-tests" {
              # Same private, config-less two-server pattern: an isolated
              # "remote" tmux -L server plus an isolated "local" tmux -L
              # server, wired together by the M2.1 daemon's --test-local seam
              # (no ssh). Both binaries are prebuilt via the vendored
              # buildGoModule (pickerChecked) so this check never invokes
              # `go build` — a non-FOD sandbox has no network.
              #
              # Uses the pinned next-3.8 tmux (mkTmux), not pkgs.tmux (3.7b):
              # the M2 bridge is a next-3.8 effort and its control-mode
              # notification behavior (e.g. %window-close on kill-window) differs
              # from 3.7b, so the mirror is exercised against the version it —
              # and production, local + remote — actually runs.
              # procps supplies ps, which the #482 reconnect cases use to find
              # the daemon's own transport child (not the daemon_pid itself)
              # and SIGKILL it for a bare-EOF drop. On darwin this resolves to
              # unixtools' shim — ps/sysctl/top/watch, no pgrep — which is why
              # transport_child reads the process table rather than pgrepping.
              nativeBuildInputs = [pkgs.bats pkgs.coreutils pkgs.gnused pkgs.gnugrep pkgs.procps (mkTmux pkgs)];
              DAEMON = "${pickerChecked}/bin/og-remote-bridge-daemon";
              RENDERER = "${pickerChecked}/bin/og-remote-bridge-renderer";
              # M2.3 structural input: the tests drive ctl straight at the
              # daemon's socket, since these vanilla -L servers carry no
              # tmux-og keybindings for a gate to intercept.
              CTL = "${pickerChecked}/bin/og-remote-bridge-ctl";
              # Only tests/ is copied in, so a ../scripts path would not resolve.
              # The raw source file is what ships: this script carries no
              # build-time placeholder substitution.
              DETACH = ./scripts/og-remote-detach.sh;
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR
              bats tests/remote-m2-integration.bats
              touch $out
            '';

          # A keypress, not the conf text: `prefix + ,` inside a mirror window is
          # driven for real (#367) — the wrapped tmux with the emitted config, a
          # second -L server supplying the attached client a keybind needs, and a
          # socat stub on @bridge_sock recording the delivered ctl frame. socat is
          # already in the wrapper's PATH closure but is named here explicitly
          # because the test, not the wrapper, invokes it. CTL is the real binary,
          # which the harness self-test drives to prove the stub's ack satisfies it.
          #
          # TMUX_BIN is built with enrich/agent-usage OFF (same knobs as
          # tick-floor-disabled-conf-assertions above), not tmuxConfig.tmux-wrapped:
          # since #603 the pr/backfill/usage monitor hooks fire on the server's own
          # 5s clock with zero clients, so leaving them on had this test's server
          # spawning three extra `run-shell -b` jobs every 5s for the run's whole
          # duration, on top of the one keybind under test -- enough background
          # contention on aarch64-darwin CI to push wait_for_frame's 10s poll past
          # its deadline. The sweep hook stays on: it's unconditional by design and
          # a single cheap list-panes -a, not a gh/curl-touching poller.
          rename-bind-integration-tests = let
            renameBindTmuxConfig = import ./config/tmux.conf.nix {
              inherit pkgs lib;
              tmuxPkg = mkTmux pkgs;
              carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
              carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
              prdash = inputs.prdash.packages.${pkgs.system}.prdash;
              enrichEnable = false;
              agentUsageEnable = false;
            };
          in
            pkgs.runCommand "rename-bind-integration-tests" {
              nativeBuildInputs = [pkgs.bash pkgs.bats pkgs.coreutils pkgs.diffutils pkgs.gnugrep pkgs.socat];
              TMUX_BIN = "${renameBindTmuxConfig.tmux-wrapped}/bin/tmux";
              CTL = "${pickerChecked}/bin/og-remote-bridge-ctl";
              # A window name fixture is UTF-8, and so is the status line it is
              # read back from.
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR/home
              mkdir -p "$HOME"
              # argv[0] of every ctl frame, read from the one source of truth so a
              # protocol bump doesn't read as a wire-shape regression.
              protocol_go=${./picker/remotebridge/wire/protocol.go}
              CTL_PROTOCOL_VERSION=$(sed -n 's/^const CtlProtocolVersion = "\(.*\)"$/\1/p' "$protocol_go")
              [ -n "$CTL_PROTOCOL_VERSION" ] || { echo "no CtlProtocolVersion in $protocol_go" >&2; exit 1; }
              export CTL_PROTOCOL_VERSION
              bats tests/rename-bind-integration.bats
              touch $out
            '';

          # `qs:` silently degrades to a raw expansion on tmux 3.7, so the
          # shell-word mechanism is exercised only through the pinned wrapper.
          conf-shell-quoting-integration-tests =
            pkgs.runCommand "conf-shell-quoting-integration-tests" {
              nativeBuildInputs = [pkgs.bash pkgs.bats pkgs.coreutils];
              TMUX_BIN = "${tmuxConfig.tmux-wrapped}/bin/tmux";
              LANG = "C.UTF-8";
              LC_ALL = "C.UTF-8";
            } ''
              cp -r ${./tests} tests
              export HOME=$TMPDIR/home
              mkdir -p "$HOME"
              bats tests/conf-shell-quoting-integration.bats
              touch $out
            '';

          # The gate itself, not a keypress: remote-m2-integration-tests above
          # drives vanilla -L servers with no tmux-og keybindings for a gate to
          # intercept (comment on that check), so it can only exercise the
          # `carousel` ctl verb directly. Proving both bind I branches exist in
          # the REAL generated config is what actually covers carouselBind.
          bridge-carousel-bind-assertions =
            pkgs.runCommand "bridge-carousel-bind-assertions" {
              nativeBuildInputs = [pkgs.gnugrep];
              CONF = tmuxConfig.tmuxConf;
            } ''
              grep -qE 'bind I if-shell -F .*@bridge_win' "$CONF"
              grep -qE 'bind I if-shell -F .*--display-error.*client_name' "$CONF"
              touch $out
            '';

          # The cwd-bound tool binds must keep BOTH branches: the bridged one
          # (the remote's cwd, via the ctl tool verb) and the local float body
          # the brace block replaced — a `\;` chain flattened into a brace list
          # is exactly the edit that silently drops the trailing commands.
          bridge-tool-bind-assertions =
            pkgs.runCommand "bridge-tool-bind-assertions" {
              nativeBuildInputs = [pkgs.gnugrep];
              CONF = tmuxConfig.tmuxConf;
            } ''
              for k in p g y; do
                grep -qE "bind-key $k if-shell -F .*@bridge_win" "$CONF"
                grep -qE "bind-key $k if-shell -F .*bridge-ctl .*tool #\{q:@bridge_pane\} [a-z]+ #\{qs:@bridge_dir\}" "$CONF"
              done
              grep -qE "new-pane -c '#\{pane_current_path\}'.*prdash" "$CONF"
              for label in prdash lazygit yazi; do
                grep -qE "set -p @pane_label $label" "$CONF"
              done
              touch $out
            '';
        };

        packages = {
          default = tmuxConfig.tmux-wrapped;
          # Runs every pre-commit hook over the tree (see pre-commit.check above).
          lint = config.pre-commit.settings.run;
          # Stable store path for the Codex managed-hook config (tmux-og#140
          # Task 3) to point its `command` at, independent of the tmux wrapper.
          codex-relaunch-stamp = tmuxConfig.script.codex-relaunch-stamp;
          # The og dispatcher (docs/superpowers/specs/2026-09-10-og-dispatcher-design.md).
          # .#default is tmux-wrapped, which by design contains no og.
          inherit (tmuxConfig) og;
          # Renders tmux.conf from a serialized config + resolved paths. Its own
          # Go module, so nothing under picker/ moves.
          og-generate = pkgs.callPackage ./generator {};
        };

        # `nix run .#demo` re-renders the README GIFs (docs/media/tapes/*.tape).
        # vhs 0.11.0, not the pinned 0.12.0: 0.12.0 prints "Creating <out>.gif"
        # and exits without writing any file, the same tape renders on 0.11.0.
        apps.demo = let
          vhs = pkgs.vhs.overrideAttrs (old: rec {
            version = "0.11.0";
            src = pkgs.fetchFromGitHub {
              owner = "charmbracelet";
              repo = "vhs";
              rev = "v${version}";
              hash = "sha256-VOiI+ddiax04QtCcDr6ze53kd/HHGbfQE3j/32iq4Ro=";
            };
            vendorHash = "sha256-cgKLYUATtn4hMdIOXZe9JWYNUOrX3S6BDfvS+rIWDfM=";
            ldflags = map (f:
              if lib.hasPrefix "-X=main.Version=" f
              then "-X=main.Version=${version}"
              else f)
            old.ldflags;
          });
        in {
          type = "app";
          meta.description = "Render the README GIFs against a throwaway tmux-og server";
          program = lib.getExe (pkgs.writeShellApplication {
            name = "og-demo";
            # No inherited PATH: a `gh` or `linear` on it would let the enrich
            # pollers overwrite the seeded @issue_*/@pr_* options.
            inheritPath = false;
            # bashInteractive: vhs resolves its `Set Shell bash` on PATH. The
            # text tools: the server inherits this PATH, and the config's
            # if-shell version probes pipe through grep.
            runtimeInputs = [vhs pkgs.bashInteractive pkgs.git pkgs.coreutils pkgs.gnugrep pkgs.gnused pkgs.gawk pkgs.findutils pkgs.procps pkgs.ncurses pkgs.zoxide];
            runtimeEnv = {
              OG_DEMO_TMUX = lib.getExe tmuxConfig.tmux-wrapped;
              OG_DEMO_TMUX_RAW = lib.getExe (mkTmux pkgs);
              OG_DEMO_SHELL = lib.getExe pkgs.bashInteractive;
              FONTCONFIG_FILE = pkgs.makeFontsConf {fontDirectories = [pkgs.nerd-fonts.jetbrains-mono];};
            };
            text = builtins.readFile ./docs/media/demo.sh;
          });
        };
      };

      flake = {
        homeManagerModules.default = {pkgs, ...} @ args:
          import ./modules/home-manager.nix (args
            // {
              tmux-pkg = mkTmux pkgs;
              tmux-remux-pkg = inputs.tmux-remux.packages.${pkgs.system}.default;
              carousel-toggle = inputs.aeye.packages.${pkgs.system}.toggle;
              carousel-aeye = inputs.aeye.packages.${pkgs.system}.default;
              carouselPluginSkills = "${inputs.aeye}/adapters/claude-code/plugin/skills";
              prdash = inputs.prdash.packages.${pkgs.system}.prdash;
            });
      };
    };
}
