{
  pkgs,
  lib,
  # tmux package to wrap. Defaults to pkgs.tmux; pinned to a 3.6a nixpkgs by the
  # flake because tmux 3.7 no longer freezes background panes under a popup
  # (tmux/tmux#4920), which repaints/flickers every popup while a Claude pane is
  # redrawing behind it. See the flake input comment for the unpin condition.
  tmuxPkg ? pkgs.tmux,
  extraProcessIcons ? {},
  # TERM string of the outer terminal emulator (e.g. "xterm-ghostty", "xterm-kitty").
  # When set, adds a terminal-features line for RGB true-color + extended keys.
  # Null when no emulator preset is active (manual terminal config).
  terminalTerm ? null,
  # TERM strings of terminals that can paint sixel (e.g. "foot", "wezterm").
  # Each entry emits a terminal-features line enabling sixel for that TERM.
  sixelTerminals ? [],
  # Additional tmux config text appended verbatim at the end of the generated
  # tmux.conf. Used by the home-manager module to inject opt-in features
  # (e.g. tmux-remux hooks/keybindings) without polluting the base config.
  extraConfText ? "",
  # tmux-remux wire script, or null when persistence is off. Defaulted rather
  # than required: flake.nix imports this file directly in four places and none
  # of them knows about the module's persist option.
  persistWireScript ? null,
  # Issue/PR enrichment config (threaded from the home-manager module).
  enrichEnable ? true,
  enrichProviders ? ["linear" "github"],
  enrichPrRefreshSeconds ? 120,
  enrichPrCheckRefreshSeconds ? 300,
  enrichIcons ? {},
  # Notifications for background events (threaded from the home-manager module).
  # When false the hooks and the prefix+n bind are omitted and @notify@ is left
  # unsubstituted, which is how the producers see "disabled" — one mechanism.
  notifyEnable ? true,
  # Comma-separated glob/basename patterns the session picker drops from its
  # zoxide suggestions (e.g. "*/.ssh,/tmp/*"). Empty => suggest everything.
  zoxideExclude ? "",
  # Percentage of the picker body the list gets; the preview takes the rest.
  pickerListRatio ? 50,
  # What the pickers open with: "preview" (list + live preview) or "list".
  pickerLayout ? "preview",
  # Whitespace-separated ssh Host aliases the session picker probes for remote
  # tmux sessions (prefix + s remote section). Empty => no remote section.
  remoteBridgeHosts ? "",
  remoteAuthPersistSeconds ? 14400,
  # tmux prefix key (literal character). Default backtick.
  prefix ? "`",
  # Absolute path to the shell tmux spawns in new panes (default-shell).
  # Null => tmux uses $SHELL / the account shell.
  defaultShell ? null,
  # tmux focus-follows-mouse: whether moving the mouse into a pane selects it,
  # without clicking. Off by default, matching tmux's own default.
  focusFollowsMouse ? false,
  # tmux copy-mode-line-numbers mode (tmux 3.7+): off/default/absolute/relative/hybrid.
  copyModeLineNumbers ? "off",
  # agent-carousel toggle package (threaded from the flake input; used by the prefix+I keybind).
  carousel-toggle ? null,
  # agent-carousel viewer package itself (threaded from the flake input; the
  # toggle above deliberately does not re-export it, only tmux-claude-images —
  # see docs/superpowers/specs/2026-09-08-carousel-remux-resume-design.md fact
  # on why a store-path @carousel_aeye@ substitution is used instead of a bare
  # `aeye` on PATH). Used only by tmux-carousel-restore.
  carousel-aeye ? null,
  # prdash PR dashboard package (threaded from the flake input; used by the prefix+p popup).
  prdash ? null,
  # Welcome-buffer splash (threaded from the home-manager module).
  splashEnable ? true,
  splashTips ? [],
  splashTimeout ? 10,
  # Behavior when the attaching client came in over ssh: "full" (default,
  # unchanged animated splash), "static" (single already-resolved frame, no
  # redraw loop — picker-splash-bin --static), or "skip" (no splash for that
  # attach; a later local attach on the same server still gets it, since
  # @splash_shown is left unset in that case).
  splashRemote ? "full",
  # AI window naming (threaded from the home-manager module). When enabled, a
  # UserPromptSubmit hook nudges the pane's Claude to name fallback windows (no
  # tracked issue, on the default branch) for itself via `claude-status-update
  # name set`. Exposed to the plugin hook as the @ai_naming global.
  aiNamingEnable ? false,
  # Resume Claude sessions on tmux-remux restore (threaded from the module).
  # When on, tmux-update-icons stamps each Claude pane's @remux_relaunch override
  # so restore relaunches `claude --resume <uuid>` instead of a bare shell.
  # Exposed as the @resume_claude global, read by update-icons each tick.
  resumeClaudeEnable ? true,
  # Resume the agent-carousel image viewer on tmux-remux restore (threaded
  # from the module). When on, tmux-update-icons stamps the carousel pane's
  # @remux_relaunch override so restore relaunches tmux-carousel-restore
  # instead of a bare shell. Exposed as the @resume_carousel global, read by
  # update-icons each tick.
  resumeCarouselEnable ? false,
  # Coding-agent usage-limit stats on line 0 (threaded from the module).
  # Polled by tmux-agent-usage into /tmp/og-agent-usage/<agent>.json with
  # each CLI's own stored token; rendered by tmux-statusline while any agent
  # pane exists. The monthly window shows only at/above the threshold percent.
  agentUsageEnable ? true,
  agentUsageRefreshSeconds ? 120,
  agentUsageMonthlyThreshold ? 50,
  # Seconds a pane may go without looking like an agent before a stale agent
  # state on it is withdrawn instead of faded (0 = off). Baked into lib-claude as
  # CLAUDE_ASSUME_DEAD_AFTER, which also gates the presence sweep that feeds it.
  claudeStatusAssumeDeadAfter ? 0,
}: let
  # Process name → icon mapping (separate file for easy editing)
  # extraProcessIcons overrides defaults when keys collide
  processIcons = let
    raw = (import ./process-icons.nix) // extraProcessIcons;
    # VS16 (U+FE0F) emoji cause width miscalculation in lipgloss/go-runewidth
    # (charmbracelet/lipgloss#55, #562). Use text presentation (no VS16) instead.
    vs16Icons = lib.filterAttrs (_: v: lib.hasInfix "️" v) raw; # "️" = U+FE0F (invisible)
    vs16Names = builtins.attrNames vs16Icons;
  in
    if vs16Names != []
    then
      builtins.throw ''
        process-icons: VS16 emoji (U+FE0F) cause alignment bugs in the picker.
        Strip the trailing ️ from: ${builtins.concatStringsSep ", " vs16Names}
        See: charmbracelet/lipgloss#55
      ''
    else raw;
  fallbackIcon = "";
  maxIcons = "2";
  maxIconsPicker = "5";

  enrichProvidersStr = lib.concatStringsSep " " enrichProviders;
  # Nerd Font (Material Design) glyph defaults. Override per-icon with Nerd Font
  # glyphs via programs.tmux-og.enrich.icons (see CLAUDE.md). Keys: linear,
  # github, pending, success, failure, merged, closed, conflict, draft.
  enrichIconDefaults = {
    linear = "󰰍"; # nerd: nf-md-alpha-l-circle (U+F0C0D)
    github = "󰊤"; # nerd: nf-md-github (U+F02A4)
    pending = "󰦖"; # nerd: nf-md-progress-clock (U+F0996)
    success = "󰗠"; # nerd: nf-md-check-circle (U+F05E0)
    failure = "󰀨"; # nerd: nf-md-alert-circle (U+F0028)
    merged = "󰘭"; # nerd: nf-md-source-merge (U+F062D)
    closed = "󰅖"; # nerd: nf-md-close-circle-outline (U+F0156) — closed/superseded PR
    conflict = "󰀦"; # nerd: nf-md-alert (U+F0026) — swap for preferred conflict glyph
    # The only non-Material glyph in this set: MD has no draft-PR icon.
    draft = ""; # nerd: nf-cod-git_pull_request_draft (U+EBDB) — swap for preferred draft glyph
  };
  # Two dialects of one map. The tmux-format sites need '#' doubled; the shell
  # sites need the glyph the user typed. Only user overrides are doubled — a
  # default glyph carrying '#' would otherwise gain a second one here.
  enrichIconsDoubled = enrichIconDefaults // (builtins.mapAttrs (_: v: builtins.replaceStrings ["#"] ["##"] v) enrichIcons);
  enrichIconsRaw = enrichIconDefaults // enrichIcons;

  # Generate bash associative array entries from Nix attrset
  iconMapBash = lib.concatStringsSep "\n" (lib.mapAttrsToList (k: v: "  [${k}]=\"${v}\"") processIcons);

  # --- Shared libraries (sourced, not executed) ---
  mkLib = name: let
    raw = builtins.readFile ../scripts/${name}.sh;
    patched =
      builtins.replaceStrings
      ["@ICON_MAP@" "@FALLBACK_ICON@" "@assume_dead_after@" "@stat@"]
      [iconMapBash fallbackIcon (toString claudeStatusAssumeDeadAfter) "${pkgs.coreutils}/bin/stat"]
      raw;
  in
    pkgs.writeShellScript name patched;

  lib-icons = mkLib "lib-icons";
  lib-claude = mkLib "lib-claude";

  # lib-log's only placeholder: coreutils' stat (see scripts/lib-log.sh).
  lib-log = pkgs.writeShellScript "lib-log" (
    builtins.replaceStrings ["@stat@"] ["${pkgs.coreutils}/bin/stat"] (builtins.readFile ../scripts/lib-log.sh)
  );

  # lib-reflow has no build-time placeholders of its own either.
  lib-reflow = pkgs.writeShellScript "lib-reflow" (builtins.readFile ../scripts/lib-reflow.sh);

  # lib-notify has no build-time placeholders of its own; a plain writeShellScript.
  lib-notify = pkgs.writeShellScript "lib-notify" (builtins.readFile ../scripts/lib-notify.sh);

  # lib-enrich needs the provider-priority substitution rather than the icon map.
  lib-enrich = let
    raw = builtins.readFile ../scripts/lib-enrich.sh;
    patched =
      builtins.replaceStrings
      [
        "@providers@"
        "@enrich_icon_linear@"
        "@enrich_icon_github@"
        "@enrich_icon_pending@"
        "@enrich_icon_success@"
        "@enrich_icon_failure@"
        "@enrich_icon_merged@"
        "@enrich_icon_closed@"
        "@enrich_icon_conflict@"
        "@enrich_icon_draft@"
      ]
      [
        enrichProvidersStr
        enrichIconsRaw.linear
        enrichIconsRaw.github
        enrichIconsRaw.pending
        enrichIconsRaw.success
        enrichIconsRaw.failure
        enrichIconsRaw.merged
        enrichIconsRaw.closed
        enrichIconsRaw.conflict
        enrichIconsRaw.draft
      ]
      raw;
  in
    pkgs.writeShellScript "lib-enrich" patched;

  lib-remote = pkgs.writeShellScript "lib-remote" (builtins.readFile ../scripts/lib-remote.sh);

  # --- Helper scripts ---
  mkScript = name: pkgs.writeShellScriptBin name (builtins.readFile ../scripts/${name}.sh);

  mkScriptSplash = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings ["@tmux_splash@" "@splash_remote@"] [picker-splash-bin splashRemote]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # claude-status needs lib substitution but not self-reference
  mkScriptWithLibs = name: let
    raw = builtins.readFile ../scripts/${name}.sh;
    patched =
      builtins.replaceStrings
      ["@lib_icons@" "@lib_claude@"]
      ["${lib-icons}" "${lib-claude}"]
      raw;
  in
    pkgs.writeShellScriptBin name patched;

  # tmux-shell-prompt needs lib substitution plus the agent-manifest list the
  # sweep also carries, so the discriminator and the arm-sweep cannot diverge.
  # @reflow@ (#671) is the same forced-reflow seam mkScriptWithLog's scripts
  # get below — the event trigger's window-wide naming/crew reset forces a
  # reflow on a genuine transition, same as claude-status-update's window_stamp.
  mkScriptShellPrompt = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@lib_claude@" "@AGENT_COMMANDS@" "@reflow@"]
      ["${lib-claude}" agentCommands "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # Scripts that source only lib-log (gated event logging). Includes
  # claude-status-update, which is run RAW by tests/claude-issues.bats — its
  # source is guarded so the raw script defines no-op stubs.
  scriptsWithLog = ["claude-status-update" "og-log-event" "og-debug"];

  mkScriptWithLog = name: let
    raw = builtins.readFile ../scripts/${name}.sh;
    patched =
      builtins.replaceStrings
      ["@lib_log@" "@notify@" "@lib_claude@" "@reflow@"]
      ["${lib-log}" notifyBin "${lib-claude}" "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"]
      raw;
  in
    pkgs.writeShellScriptBin name patched;

  # Build claude-status first — other scripts reference it by full path
  claude-status-pkg = mkScriptWithLibs "claude-status";
  claude-status-bin = "${claude-status-pkg}/bin/claude-status";

  # Go binary for fast session picker generation (~4ms vs ~85ms in bash)
  picker-generate = import ../picker {
    inherit pkgs lib processIcons fallbackIcon;
    inherit maxIconsPicker;
    inherit splashTips splashTimeout prefix;
  };
  picker-generate-bin = "${picker-generate}/bin/tmux-picker-generate";
  picker-splash-bin = "${picker-generate}/bin/tmux-splash";
  picker-statusline-bin = "${picker-generate}/bin/tmux-statusline";
  picker-card-bin = "${picker-generate}/bin/tmux-enrich-card";
  picker-bridge-ctl-bin = "${picker-generate}/bin/og-remote-bridge-ctl";
  picker-bridge-daemon-bin = "${picker-generate}/bin/og-remote-bridge-daemon";
  picker-bridge-renderer-bin = "${picker-generate}/bin/og-remote-bridge-renderer";
  picker-session-res-bin = "${picker-generate}/bin/tmux-session-resources";

  picker-agent-detect-bin = "${picker-generate}/bin/agent-detect";

  # Commands the pipe-pane sweep in tmux-update-icons watches for, derived from
  # the same manifest dir agent-detect embeds via go:embed. The sweep filters on
  # this list before agent-detect runs, so a hand-maintained copy that missed a
  # manifest silently left it unscraped, with both sides looking correct alone.
  agentCommands = let
    dir = ../picker/agentdetect/manifest/manifests;
    manifests = lib.filter (lib.hasSuffix ".toml") (builtins.attrNames (builtins.readDir dir));
    commandsOf = f: (builtins.fromTOML (builtins.readFile (dir + "/${f}"))).match_commands;
  in
    lib.concatStringsSep " " (lib.unique (lib.concatMap commandsOf manifests));

  scriptNames = [
    "claude-status"
    "claude-status-update"
    "tmux-reflow-windows"
    "tmux-session-picker"
    "tmux-window-picker"
    "tmux-window-wall"
    "tmux-which-key"
    "tmux-update-icons"
    "tmux-branch-display"
    "tmux-dir-display"
    "tmux-window-nav"
    "tmux-kill-pane-guard"
    "tmux-reap-pane"
    "tmux-shell-prompt"
    "tmux-smart-nav"
    "tmux-reconcile-window"
    "tmux-float-refit"
    "tmux-default-size"
    "tmux-worktree-match"
    "tmux-apply-theme-colors"
    "tmux-client-theme"
    "tmux-scratchpad"
    "tmux-issue-stamp"
    "tmux-issue-stamp-linear"
    "tmux-issue-stamp-github"
    "tmux-pr-enrich"
    "tmux-splash-maybe"
    "og-log-event"
    "og-debug"
    "codex-relaunch-stamp"
    "pi-relaunch-stamp"
    "cursor-status-hook"
    "cursor-hooks-install"
    "cursor-relaunch-stamp"
    "cursor-relaunch-hooks-install"
    "og-remote-open"
    "og-remote-loading"
    "og-remote-picker"
    "og-remote-detach"
    "og-remote-auth"
    "og-remote-theme"
    "og-notify"
    "og-notify-center"
    "tmux-agent-usage"
    "tmux-agent-usage-claude"
    "tmux-agent-usage-codex"
    "tmux-agent-usage-cursor"
    "tmux-carousel-restore"
  ];

  # Scripts that need icon map + library + claude-status path substitution
  scriptsWithIcons = ["tmux-reflow-windows" "tmux-session-picker" "tmux-window-picker" "tmux-window-wall" "tmux-update-icons"];

  iconSubstFrom = ["@lib_icons@" "@lib_claude@" "@lib_enrich@" "claude-status " "@claude_status_bin@" "@ICON_MAP@" "@FALLBACK_ICON@" "@MAX_ICONS@" "@MAX_ICONS_PICKER@" "@picker_generate@" "@lib_log@" "@lib_reflow@"];
  iconSubstTo = ["${lib-icons}" "${lib-claude}" "${lib-enrich}" "${claude-status-bin} " claude-status-bin iconMapBash fallbackIcon maxIcons maxIconsPicker picker-generate-bin "${lib-log}" "${lib-reflow}"];

  mkScriptFull = name:
    pkgs.writeShellScriptBin name
    (builtins.replaceStrings iconSubstFrom iconSubstTo (builtins.readFile ../scripts/${name}.sh));

  # tmux-update-icons kicks a forced reflow on branch/task change. Pin that call
  # to the reflow store path via @reflow@ so a config reload alone repoints it; a
  # bare name resolves against the tmux server's frozen PATH and stays stale
  # until a full server restart. Built apart from mkScriptFull so reflow itself
  # (also icon-substituted) never references its own store path.
  mkScriptIcons = name:
    pkgs.writeShellScriptBin name
    (builtins.replaceStrings
      (iconSubstFrom ++ ["@reflow@" "@agent_detect_bin@" "@AGENT_COMMANDS@" "@issue_stamp@" "@carousel_restore@" "@reconcile@"])
      (iconSubstTo
        ++ [
          "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"
          picker-agent-detect-bin
          agentCommands
          (
            if enrichEnable
            then "${script.tmux-issue-stamp}/bin/tmux-issue-stamp"
            else ""
          )
          carouselRestoreBin
          "${script.tmux-reconcile-window}/bin/tmux-reconcile-window"
        ])
      (builtins.readFile ../scripts/${name}.sh));

  # Scripts that need enrich library + provider/icon/config substitution
  scriptsWithEnrich = ["tmux-issue-stamp" "tmux-issue-stamp-linear" "tmux-issue-stamp-github" "tmux-pr-enrich"];

  mkScriptEnrich = name: let
    raw = builtins.readFile ../scripts/${name}.sh;
    patched =
      builtins.replaceStrings
      [
        "@lib_enrich@"
        "@pr_refresh_seconds@"
        "@pr_check_refresh_seconds@"
        "@issue_stamp_linear@"
        "@issue_stamp_github@"
        "@pr_enrich@"
        "@reflow@"
        "@lib_log@"
        "@notify@"
      ]
      [
        "${lib-enrich}"
        (toString enrichPrRefreshSeconds)
        (toString enrichPrCheckRefreshSeconds)
        "${enrich-linear-bin}/bin/tmux-issue-stamp-linear"
        "${enrich-github-bin}/bin/tmux-issue-stamp-github"
        "${enrich-pr-bin}/bin/tmux-pr-enrich"
        "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"
        "${lib-log}"
        notifyBin
      ]
      raw;
  in
    pkgs.writeShellScriptBin name patched;

  enrich-linear-bin = mkScriptEnrich "tmux-issue-stamp-linear";
  enrich-github-bin = mkScriptEnrich "tmux-issue-stamp-github";
  enrich-pr-bin = mkScriptEnrich "tmux-pr-enrich";

  # Agent-usage providers are plain scripts; only the dispatcher needs
  # substitution (lib-log, the agent-command gate list, provider store paths —
  # the daemonized pass must not resolve providers against the tmux server's
  # frozen PATH).
  agent-usage-provider-bins =
    lib.genAttrs ["claude" "codex" "cursor"] (p: mkScript "tmux-agent-usage-${p}");

  mkScriptAgentUsage = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@lib_log@" "@refresh_seconds@" "@AGENT_COMMANDS@" "@usage_claude@" "@usage_codex@" "@usage_cursor@"]
      [
        "${lib-log}"
        (toString agentUsageRefreshSeconds)
        agentCommands
        "${agent-usage-provider-bins.claude}/bin/tmux-agent-usage-claude"
        "${agent-usage-provider-bins.codex}/bin/tmux-agent-usage-codex"
        "${agent-usage-provider-bins.cursor}/bin/tmux-agent-usage-cursor"
      ]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # Scripts that source lib-remote get its store path substituted, plus the
  # bridge binaries the launcher probes and spawns — pinned for the same reason
  # as @reflow@ above, which the launcher spells out.
  scriptsWithRemote = ["og-remote-open" "og-remote-auth" "og-remote-theme"];
  mkRemoteScript = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@lib_remote@" "@bridge_ctl@" "@bridge_daemon@" "@bridge_renderer@" "@reflow@" "@loading@"]
      [
        "${lib-remote}"
        picker-bridge-ctl-bin
        picker-bridge-daemon-bin
        picker-bridge-renderer-bin
        "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"
        "${script.og-remote-loading}/bin/og-remote-loading"
      ]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # tmux-client-theme's recovery replay runs catppuccin, tmux-apply-theme-colors
  # and og-remote-theme directly (the same three the config's own load order
  # runs), plus lib-log for the lock and log_event. catppuccin needs both its
  # own path and bash to run it with, exactly as pluginRunShells does.
  mkScriptClientTheme = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@lib_log@" "@catppuccin@" "@bash@" "@apply_theme_colors@" "@remote_theme@"]
      [
        "${lib-log}"
        "${catppuccin}/share/tmux-plugins/catppuccin/catppuccin.tmux"
        "${pkgs.bash}/bin/bash"
        "${script.tmux-apply-theme-colors}/bin/tmux-apply-theme-colors"
        "${script.og-remote-theme}/bin/og-remote-theme"
      ]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # The remote-side session picker's dual-role wrapper (#356). The remote copy
  # execs the picker and needs zoxide by store path — the ssh PATH carries neither,
  # and guessing at profile bin dirs would only degrade the miss instead of
  # removing it. @remote_open@ is pinned for the reason @reflow@ above spells out:
  # the local role is spawned by the tmux server, whose PATH is frozen until a
  # restart, so a bare name reaches a stale launcher — one that would silently
  # ignore OG_REMOTE_NEW_DIR.
  scriptsWithRemotePicker = ["og-remote-picker"];
  mkScriptRemotePicker = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@remote_open@" "@picker_generate@" "@zoxide@" "@coreutils@"]
      [
        "${script.og-remote-open}/bin/og-remote-open"
        picker-generate-bin
        "${pkgs.zoxide}"
        "${pkgs.coreutils}"
      ]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # The which-key popup: only needs the picker binary's store path, none of
  # scriptsWithIcons' icon-map/library placeholders — its own minimal builder
  # rather than folding into mkScriptFull for a script that uses one of its
  # six substitutions.
  scriptsWithPickerBin = ["tmux-which-key"];
  mkScriptPickerBin = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings ["@picker_generate@"] [picker-generate-bin]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # The notification router + history center. Both source lib-notify; the router
  # also sources lib-log (acquire_lock / file_mtime, reached via notify_prune).
  scriptsWithNotify = ["og-notify" "og-notify-center"];
  mkScriptNotify = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings ["@lib_notify@" "@lib_log@"] ["${lib-notify}" "${lib-log}"]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # The router's store path for the producers. With notifications off the
  # placeholder is left untouched: the producers' leading-'@' test then skips the
  # call, so disabling is one mechanism rather than two.
  notifyBin =
    if notifyEnable
    then "${script.og-notify}/bin/og-notify"
    else "@notify@";

  # tmux-update-icons's store path for the carousel-restore stamp. Same
  # leading-'@' idiom as notifyBin: with no viewer package wired in, the
  # placeholder is left untouched and tmux-update-icons' own guard skips the
  # stamp rather than pointing @remux_relaunch at a nonexistent command.
  carouselRestoreBin =
    if carousel-aeye != null
    then "${script.tmux-carousel-restore}/bin/tmux-carousel-restore"
    else "@carousel_restore@";

  # Its own builder, not mkScriptIcons: a script carrying @carousel_restore@
  # could substitute its own store path into itself (infinite recursion at eval,
  # the hazard @reflow@ avoids). @AGENT_COMMANDS@ matters as much as the viewer
  # path — host discovery matches a sibling pane's command against it, and left
  # unsubstituted it fails silently, degrading every restore to the
  # sole-non-self-pane fallback. Unwired, the viewer path stays a placeholder,
  # which the script's own `[[ -x ]]` treats as a miss.
  mkScriptCarouselRestore = name:
    pkgs.writeShellScriptBin name (
      builtins.replaceStrings
      ["@carousel_aeye@" "@AGENT_COMMANDS@"]
      [
        (
          if carousel-aeye != null
          then "${carousel-aeye}/bin/aeye"
          else "@carousel_aeye@"
        )
        agentCommands
      ]
      (builtins.readFile ../scripts/${name}.sh)
    );

  # The cwd-derived window reconciler. Always built (tagging drives navigation
  # even with enrich off); the @issue_stamp@ kick is empty when enrich is off.
  mkScriptReconcile = name: let
    raw = builtins.readFile ../scripts/${name}.sh;
    patched =
      builtins.replaceStrings
      ["@issue_stamp@" "@reflow@"]
      [
        (
          if enrichEnable
          then "${script.tmux-issue-stamp}/bin/tmux-issue-stamp"
          else ""
        )
        "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"
      ]
      raw;
  in
    pkgs.writeShellScriptBin name patched;

  # Individual script references for full store paths in config.
  # Reuse the pre-built enrich provider/poller derivations (also referenced by
  # the dispatcher's substitution) instead of rebuilding them via mkScriptEnrich.
  script = lib.genAttrs scriptNames (name:
    if name == "tmux-issue-stamp-linear"
    then enrich-linear-bin
    else if name == "tmux-issue-stamp-github"
    then enrich-github-bin
    else if name == "tmux-pr-enrich"
    then enrich-pr-bin
    else if name == "tmux-agent-usage"
    then mkScriptAgentUsage name
    else if lib.hasPrefix "tmux-agent-usage-" name
    then agent-usage-provider-bins.${lib.removePrefix "tmux-agent-usage-" name}
    else if builtins.elem name scriptsWithEnrich
    then mkScriptEnrich name
    else if name == "tmux-update-icons"
    then mkScriptIcons name
    else if builtins.elem name scriptsWithIcons
    then mkScriptFull name
    else if name == "claude-status"
    then claude-status-pkg
    else if name == "tmux-kill-pane-guard"
    then mkScriptWithLibs name
    else if name == "tmux-reap-pane"
    then mkScriptWithLibs name
    else if name == "tmux-shell-prompt"
    then mkScriptShellPrompt name
    else if name == "tmux-splash-maybe"
    then mkScriptSplash name
    else if name == "tmux-reconcile-window"
    then mkScriptReconcile name
    else if name == "tmux-carousel-restore"
    then mkScriptCarouselRestore name
    else if name == "tmux-client-theme"
    then mkScriptClientTheme name
    else if builtins.elem name scriptsWithLog
    then mkScriptWithLog name
    else if builtins.elem name scriptsWithRemote
    then mkRemoteScript name
    else if builtins.elem name scriptsWithRemotePicker
    then mkScriptRemotePicker name
    else if builtins.elem name scriptsWithPickerBin
    then mkScriptPickerBin name
    else if builtins.elem name scriptsWithNotify
    then mkScriptNotify name
    else mkScript name);

  scripts = lib.attrValues script;

  # --- og dispatcher (docs/superpowers/specs/2026-09-10-og-dispatcher-design.md) ---
  # Curated public API over a subset of `script`: verb tokens -> the script
  # name they target, plus the one-line summary `og <verb> --help` prints.
  # Keyed by script *name* (not derivation) so ogPartitionOk below can read
  # v.script directly; mkOg resolves names to "${script.<name>}/bin/<name>".
  ogVerbSpec = {
    "status" = {
      script = "claude-status";
      summary = "Aggregate claude/agent status for the status line";
    };
    "status update" = {
      script = "claude-status-update";
      summary = "Write claude/agent state, issue, task and name self-reports";
    };
    "remote open" = {
      script = "og-remote-open";
      summary = "Open a remote tmux session as local mirror windows";
    };
    "remote picker" = {
      script = "og-remote-picker";
      summary = "Browse a remote host's own tmux sessions";
    };
    "remote detach" = {
      script = "og-remote-detach";
      summary = "Detach the bridge for a mirrored session";
    };
    "remote auth" = {
      script = "og-remote-auth";
      summary = "Run one interactive ssh handshake for a bridge host";
    };
    "remote theme" = {
      script = "og-remote-theme";
      summary = "Fan a light/dark theme toggle out to mirrored hosts";
    };
    "pick session" = {
      script = "tmux-session-picker";
      summary = "Session picker (sessions, remote hosts, zoxide suggestions)";
    };
    "pick window" = {
      script = "tmux-window-picker";
      summary = "Window picker, grouped by session or claude priority state";
    };
    "pick wall" = {
      script = "tmux-window-wall";
      summary = "Tiled grid of live window previews";
    };
    "pick which-key" = {
      script = "tmux-which-key";
      summary = "Which-key popup: every bind, grouped and filterable";
    };
    "issue stamp" = {
      script = "tmux-issue-stamp";
      summary = "Detect and stamp the Linear/GitHub issue for a window's branch";
    };
    "issue linear" = {
      script = "tmux-issue-stamp-linear";
      summary = "Linear issue provider dispatched by og issue stamp";
    };
    "issue github" = {
      script = "tmux-issue-stamp-github";
      summary = "GitHub issue provider dispatched by og issue stamp";
    };
    "pr" = {
      script = "tmux-pr-enrich";
      summary = "PR enrichment poller (--tick background job, not interactive)";
    };
    "cursor hooks" = {
      script = "cursor-hooks-install";
      summary = "Install Cursor CLI status hooks";
    };
    "cursor relaunch" = {
      script = "cursor-relaunch-hooks-install";
      summary = "Install Cursor's relaunch-hooks managed config";
    };
    "cursor stamp" = {
      script = "cursor-relaunch-stamp";
      summary = "Stamp Cursor relaunch state";
    };
    "cursor status-hook" = {
      script = "cursor-status-hook";
      summary = "Cursor CLI status hook entry point";
    };
    "carousel restore" = {
      script = "tmux-carousel-restore";
      summary = "Rebind an aeye carousel viewer after a tmux-remux restore (not run directly)";
    };
    "codex stamp" = {
      script = "codex-relaunch-stamp";
      summary = "Stamp Codex relaunch state";
    };
    "pi stamp" = {
      script = "pi-relaunch-stamp";
      summary = "Stamp pi relaunch state (run by the hookyard Pi bridge, not interactive)";
    };
    "notify" = {
      script = "og-notify";
      summary = "Send a notification through the configured routing";
    };
    "notify center" = {
      script = "og-notify-center";
      summary = "Open the notification history";
    };
    "debug" = {
      script = "og-debug";
      summary = "Toggle or inspect event-logging debug mode";
    };
    # Neither is a scripts/ entry -- "init"/"doctor" are two more of the
    # generator's own binaries, the same target-shaped verb "generate" already
    # is -- so ogPartitionOk (which only ever looks at v.script) is untouched.
    "init" = {
      target = "${og-generate}/bin/og-init";
      summary = "Write a commented config.toml with detected defaults";
    };
    "doctor" = {
      target = "${og-doctor-wrapped}/bin/og-doctor";
      summary = "Diagnose what is missing on PATH, here and on each configured remote";
    };
    "generate" = {
      target = "${og-generate}/bin/og-generate";
      summary = "Render tmux.conf from a config and a path map";
    };
  };

  # Scripts reached only by store-path interpolation from this file, a parent
  # script, or a respawn-pane argv -- no human runs one standalone, so none
  # gets a verb. Recorded here, not just in the design doc, so ogPartitionOk
  # can check the partition.
  ogInternal = [
    "tmux-agent-usage"
    "tmux-agent-usage-claude"
    "tmux-agent-usage-codex"
    "tmux-agent-usage-cursor"
    "tmux-apply-theme-colors"
    "tmux-branch-display"
    "tmux-client-theme"
    "tmux-default-size"
    "tmux-dir-display"
    "tmux-float-refit"
    "tmux-kill-pane-guard"
    "tmux-reap-pane"
    "tmux-reconcile-window"
    "tmux-reflow-windows"
    "tmux-scratchpad"
    "tmux-shell-prompt"
    "tmux-smart-nav"
    "tmux-splash-maybe"
    "tmux-update-icons"
    "tmux-window-nav"
    "tmux-worktree-match"
    "og-log-event"
    "og-remote-loading"
  ];

  # Forced by the assert on the returned attrset below, never on `og` itself:
  # nothing in tmux-wrapped references og, so an assert there would never be
  # evaluated and a script landing in scriptNames undecided would stay silent.
  # target-shaped verbs are excluded: their target is not a scripts/ entry, so
  # they neither belong to the partition nor leave a script undecided.
  ogPartitioned =
    map (v: v.script) (builtins.filter (v: v ? script) (lib.attrValues ogVerbSpec))
    ++ ogInternal;
  ogPartitionOk =
    builtins.sort builtins.lessThan ogPartitioned
    == builtins.sort builtins.lessThan scriptNames;
  # Both directions, so a failure names the offending script(s).
  ogPartitionUndecided = lib.subtractLists ogPartitioned scriptNames;
  ogPartitionUnknown = lib.subtractLists scriptNames ogPartitioned;

  # mkOg is a function, not a fixed derivation, so a check can instantiate a
  # dispatcher over a table of its own pointing at stub targets -- the only
  # way to test argument passthrough without depending on a real script.
  # `verbs` takes already-resolved targets: { "<tokens>" = { target =
  # "<absolute exec path>"; summary = "..."; }; }.
  mkOg = verbs: let
    order = builtins.attrNames verbs; # Nix sorts attrset keys
    targetLine = v: "OG_TARGET[${lib.escapeShellArg v}]=${lib.escapeShellArg verbs.${v}.target}";
    summaryLine = v: "OG_SUMMARY[${lib.escapeShellArg v}]=${lib.escapeShellArg verbs.${v}.summary}";
    ogTable = pkgs.writeText "og-table.sh" ''
      ${lib.concatMapStringsSep "\n" targetLine order}
      ${lib.concatMapStringsSep "\n" summaryLine order}
      OG_ORDER=(${lib.concatMapStringsSep " " lib.escapeShellArg order})
    '';
  in
    pkgs.writeShellScriptBin "og"
    (builtins.replaceStrings ["@og_table@"] ["${ogTable}"] (builtins.readFile ../scripts/og.sh));

  og = mkOg (lib.mapAttrs (_: v: {
      target =
        if v ? script
        then "${script.${v.script}}/bin/${v.script}"
        else v.target;
      inherit (v) summary;
    })
    ogVerbSpec);

  # --- Generator (og generate) ---
  # What ships is the generator's output (tmuxConf = generatedConf); the frozen
  # reference is now only the extraction check's oracle, until step 3 deletes
  # it. The generator is imported here rather than taken as a function argument,
  # so the four direct importers in flake.nix keep working unchanged.
  og-generate = import ../generator {inherit pkgs lib;};

  # A real TOML encoder, never string concatenation: the values carry PUA nerd
  # glyphs, '#', '\' and — in extra_config — '"' and newlines, each a class a
  # hand-rolled escaper gets wrong exactly once. A null argument is an omitted
  # key, never "": default_shell and terminal_term distinguish the two.
  tomlFormat = pkgs.formats.toml {};

  configToml = tomlFormat.generate "config.toml" {
    # From the build's host platform, never the generator's own runtime.GOOS:
    # under cross-compilation it runs on the build platform, not this one.
    platform =
      if pkgs.stdenv.hostPlatform.isDarwin
      then "darwin"
      else "linux";
    tmux =
      {
        inherit prefix;
        focus_follows_mouse = focusFollowsMouse;
        copy_mode_line_numbers = copyModeLineNumbers;
        sixel_terminals = sixelTerminals;
        extra_config = extraConfText;
      }
      // lib.optionalAttrs (defaultShell != null) {default_shell = defaultShell;}
      // lib.optionalAttrs (terminalTerm != null) {terminal_term = terminalTerm;};
    picker = {
      zoxide_exclude = zoxideExclude;
      list_ratio = pickerListRatio;
      layout = pickerLayout;
    };
    remote = {
      hosts = remoteBridgeHosts;
      auth_persist_seconds = remoteAuthPersistSeconds;
    };
    enrich = {
      enable = enrichEnable;
      providers = enrichProviders;
      pr_refresh_seconds = enrichPrRefreshSeconds;
      pr_check_refresh_seconds = enrichPrCheckRefreshSeconds;
      # Overrides only. The nine defaults live twice — enrichIconDefaults here
      # and enrichIconDefaults in generator/render/keys.go — kept in step by
      # hand and by the extraction check alone, so edit both.
      icons = enrichIcons;
    };
    notifications.enable = notifyEnable;
    agent_usage = {
      enable = agentUsageEnable;
      refresh_seconds = agentUsageRefreshSeconds;
      monthly_threshold = agentUsageMonthlyThreshold;
    };
    claude_status.assume_dead_after = claudeStatusAssumeDeadAfter;
    splash = {
      enable = splashEnable;
      remote = splashRemote;
    };
    ai_naming.enable = aiNamingEnable;
    resume = {
      claude = resumeClaudeEnable;
      carousel = resumeCarouselEnable;
    };
    process_icons = processIcons;
  };

  # og doctor's own --config default (generator/config's DefaultPath, an
  # off-Nix $XDG_CONFIG_HOME/tmux-og/config.toml) is where a Homebrew/curl
  # install's `og init` writes -- not this store path. Left unwrapped, a Nix
  # install's `og doctor` would report on a file that doesn't exist. This
  # bakes the real configToml in, the same "baked default, flag.Parse takes
  # the last occurrence so an explicit override still wins" contract
  # generator/default.nix's own --template wrapProgram already relies on.
  # (nix run .#og -- doctor bakes the flake's own default-args configToml,
  # not a user's -- home.packages' install, which is what ships, always
  # carries the user's real options, so that path is unaffected.)
  og-doctor-wrapped = pkgs.writeShellScriptBin "og-doctor" ''
    exec ${og-generate}/bin/og-doctor --config ${configToml} "$@"
  '';

  # Same pin as the reference's own copy of this plugin. Identical inputs give
  # one store path, so a drift between the two shows up as an extraction-check
  # diff rather than silently.
  catppuccin = pkgs.tmuxPlugins.mkTmuxPlugin rec {
    pluginName = "catppuccin";
    version = "2.1.3";
    src = pkgs.fetchFromGitHub {
      owner = "catppuccin";
      repo = "tmux";
      rev = "v${version}";
      sha256 = "0v3vji240mykfxf573kpwjmnswil0f9j7srqlhq74ca9ar1h5k92";
    };
    meta = with lib; {
      description = "Catppuccin theme for Tmux";
      homepage = "https://github.com/catppuccin/tmux";
      license = licenses.mit;
      platforms = platforms.all;
    };
  };

  pathsToml = tomlFormat.generate "paths.toml" ({
      bash = "${pkgs.bash}/bin/bash";
      # Every script, not the subset the template names: the resolver requires
      # its own list and ignores the rest, and a second copy of that list here
      # would go stale against the template without anything noticing.
      scripts = lib.mapAttrs (name: drv: "${drv}/bin/${name}") script;
      bin = {
        tmux-splash = picker-splash-bin;
        tmux-statusline = picker-statusline-bin;
        tmux-enrich-card = picker-card-bin;
        og-remote-bridge-ctl = picker-bridge-ctl-bin;
        tmux-session-resources = picker-session-res-bin;
      };
      # The .tmux entry file, never the package root — the template joins no
      # path segments.
      plugins = {
        catppuccin = "${catppuccin}/share/tmux-plugins/catppuccin/catppuccin.tmux";
        better-mouse-mode = "${pkgs.tmuxPlugins.better-mouse-mode}/share/tmux-plugins/better-mouse-mode/scroll_copy_mode.tmux";
        vim-tmux-navigator = "${pkgs.tmuxPlugins.vim-tmux-navigator}/share/tmux-plugins/vim-tmux-navigator/vim-tmux-navigator.tmux";
        tmux-fzf = "${pkgs.tmuxPlugins.tmux-fzf}/share/tmux-plugins/tmux-fzf/main.tmux";
        fingers = "${pkgs.tmuxPlugins.fingers}/share/tmux-plugins/tmux-fingers/tmux-fingers.tmux";
      };
    }
    // lib.optionalAttrs (persistWireScript != null) {persist_wire_script = "${persistWireScript}";}
    // lib.optionalAttrs (carousel-toggle != null) {carousel_toggle = "${carousel-toggle}/bin/tmux-claude-images";}
    // lib.optionalAttrs (carousel-aeye != null) {carousel_aeye = "${carousel-aeye}/bin/aeye";}
    // lib.optionalAttrs (prdash != null) {prdash = "${prdash}/bin/prdash";});

  # $out is the rendered file itself, not the directory holding it: every
  # consumer of tmuxConf — tests/test-display.sh's wrapper scrape included —
  # requires a store path to a regular file named *-tmux.conf.
  generatedConf = pkgs.runCommand "tmux.conf" {} ''
    mkdir -p out
    ${og-generate}/bin/og-generate --config ${configToml} --paths ${pathsToml} \
      --template ${../config/tmux.conf.tmpl} --out out
    cp out/tmux.conf $out
  '';

  # The frozen reference is the extraction check's oracle only -- the generator
  # above is what ships. It survives until step 3; see that file's header for
  # the two-file rule it imposes until then.
  reference = import ./tmux.conf.reference.nix {
    inherit pkgs lib;
    inherit prefix copyModeLineNumbers focusFollowsMouse defaultShell;
    inherit terminalTerm sixelTerminals;
    inherit zoxideExclude pickerListRatio pickerLayout;
    inherit remoteBridgeHosts remoteAuthPersistSeconds;
    inherit splashEnable enrichEnable notifyEnable;
    inherit agentUsageEnable agentUsageMonthlyThreshold;
    inherit aiNamingEnable resumeClaudeEnable resumeCarouselEnable;
    inherit extraConfText persistWireScript;
    inherit carousel-toggle prdash;
    inherit processIcons script;
    inherit picker-bridge-ctl-bin picker-card-bin picker-splash-bin picker-statusline-bin;
    inherit picker-session-res-bin;
    inherit enrichIconsDoubled enrichIconsRaw;
  };
  referenceConf = reference.tmuxConf;

  # Config references ~/.config/tmux/tmux.conf (stable symlink managed by HM module)
  # so no self-referential substitution needed.
  # Bound to the derivation, never "${generatedConf}/tmux.conf": consumers scrape
  # a regular-file store path out of the wrapper (see generatedConf above), and a
  # directory-shaped path empties that scrape silently.
  tmuxConf = generatedConf;

  # --- Wrapped tmux binary ---
  tmux-wrapped = pkgs.symlinkJoin {
    name = "tmux-wrapped";
    paths = [tmuxPkg];
    nativeBuildInputs = [pkgs.makeWrapper];
    postBuild = ''
      wrapProgram $out/bin/tmux \
        --add-flags "-f ${tmuxConf}" \
        --prefix PATH : ${lib.makeBinPath ([tmuxPkg] ++ scripts ++ [picker-generate pkgs.bash pkgs.lazygit pkgs.yazi pkgs.btop pkgs.zoxide pkgs.jq pkgs.curl pkgs.util-linux pkgs.coreutils pkgs.xdg-utils pkgs.chafa pkgs.socat] ++ lib.optional (carousel-toggle != null) carousel-toggle ++ lib.optional (prdash != null) prdash)}
    '';
    meta.mainProgram = "tmux";
  };
in
  assert lib.assertMsg ogPartitionOk ''
    og dispatcher partition mismatch (see ogVerbSpec/ogInternal in config/tmux.conf.nix):
      in scriptNames but not in ogVerbSpec or ogInternal: ${lib.concatStringsSep ", " ogPartitionUndecided}
      in ogVerbSpec/ogInternal but not in scriptNames: ${lib.concatStringsSep ", " ogPartitionUnknown}
  ''; {inherit tmux-wrapped tmuxConf script og mkOg ogVerbSpec referenceConf configToml pathsToml generatedConf picker-session-res-bin;}
