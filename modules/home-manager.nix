{
  config,
  lib,
  pkgs,
  tmux-pkg ? pkgs.tmux,
  tmux-remux-pkg ? null,
  carousel-toggle ? null,
  carousel-aeye ? null,
  carouselPluginSkills ? null,
  prdash ? null,
  ...
}: let
  cfg = config.programs.tmux-og;

  # Per-emulator defaults. terminfoPath uses an if-expression (not lib.optionalString)
  # because Nix evaluates function arguments strictly — the string interpolation would
  # force pkgs.<name> even when the condition is false.
  emulatorDefaults = {
    ghostty = {
      available = pkgs ? ghostty;
      term = "xterm-ghostty";
      termProgram = "ghostty";
      terminfoPath =
        if pkgs ? ghostty
        then "${pkgs.ghostty}/share/terminfo"
        else null;
    };
    kitty = {
      available = pkgs ? kitty;
      term = "xterm-kitty";
      termProgram = "kitty";
      terminfoPath =
        if pkgs ? kitty
        then "${pkgs.kitty}/share/terminfo"
        else null;
    };
  };

  # Resolved emulator config (null when emulator = null)
  emulatorCfg =
    if cfg.startupSession.terminal.emulator != null
    then emulatorDefaults.${cfg.startupSession.terminal.emulator} or null
    else null;

  # Effective env values: emulator preset wins over manual options
  effectiveTerm =
    if emulatorCfg != null
    then emulatorCfg.term
    else cfg.startupSession.terminal.term;

  effectiveTermProgram =
    if emulatorCfg != null
    then emulatorCfg.termProgram
    else cfg.startupSession.terminal.termProgram;

  effectiveTerminfoPath =
    if emulatorCfg != null
    then emulatorCfg.terminfoPath
    else cfg.startupSession.terminal.terminfoPath;

  # Session env the startup service imports. The display/seat half only exists
  # once a graphical session has set it, so a headless start leaves it out.
  startupImportVars =
    lib.optionals (!cfg.startupSession.headless) [
      "DISPLAY"
      "WAYLAND_DISPLAY"
      "XDG_SESSION_TYPE"
      "XDG_SESSION_DESKTOP"
      "XDG_CURRENT_DESKTOP"
    ]
    ++ ["COLORTERM" "TERM" "TERMINFO"];

  # tmux-remux binary path (null when persist is disabled or package missing).
  # Resolving here keeps the conditional logic out of the conf string itself.
  tmuxStateBin =
    if cfg.persist.enable && cfg.persist.package != null
    then "${cfg.persist.package}/bin/tmux-remux"
    else null;

  # Renders and sources the tmux-remux hook wiring at config load
  # (`tmux-remux triggers`, noamsto/tmux-remux#65) instead of hand-writing it.
  # `triggers` self-gates the tmux 3.8 monitor-hook periodic save (`set-hook -g
  # -B`) on the tmux version it's told about — pass it `#{version}`, the LIVE
  # server's own reported version, not a fresh `tmux -V` subprocess: a server
  # predating a nix switch keeps its old binary resident (#407), and a store
  # rebuild only changes the tmux -V a *new* process would report. Piped
  # straight through source-file, its `set-hook -g <event>` lines land on the
  # default index (0), which would clobber tmux-og's own index-0 hooks on the
  # same events (e.g. tmux-reflow-windows on window-unlinked) — rewrite them
  # onto [99], same as the hand-written snippet this replaces. The `-B` named
  # monitor hook has no event-name index to collide on, so it's left alone.
  # A live server still on pre-3.8 gets no periodic-save floor at all (only
  # structural/close hooks), so warn on the pane rather than leave it silent.
  tmuxRemuxWireScript =
    if tmuxStateBin == null
    then null
    else
      pkgs.writeShellScript "tmux-remux-wire" ''
        out=$("${tmuxStateBin}" triggers --bin="${tmuxStateBin}" \
          --auto-restore=${
          if cfg.persist.restoreMode == "auto"
          then "on"
          else "off"
        } --tmux-version="$1")
        printf '%s\n' "$out" \
          | sed -E 's/^(set-hook -g [A-Za-z][A-Za-z-]*)( )/\1[99]\2/' \
          | tmux source-file -
        if ! printf '%s\n' "$out" | grep -q '@remux-save'; then
          tmux display-message "tmux-remux: server is pre-3.8 ($1) — no periodic-save floor, only structural/close saves. Restart the server to pick up the tmux 3.8 monitor hook."
        fi
      '';

  tmuxConfig = import ../config/tmux.conf.nix {
    inherit pkgs lib;
    tmuxPkg = cfg.tmuxPackage;
    inherit carousel-toggle;
    inherit carousel-aeye;
    inherit prdash;
    extraProcessIcons = cfg.processIcons;
    zoxideExclude = lib.concatStringsSep "," cfg.picker.zoxideExclude;
    pickerListRatio = cfg.picker.listRatio;
    pickerLayout = cfg.picker.layout;
    remoteBridgeHosts = lib.concatStringsSep " " cfg.remote.hosts;
    remoteAuthPersistSeconds = cfg.remote.authPersistSeconds;
    inherit (cfg) prefix defaultShell focusFollowsMouse copyModeLineNumbers;
    # Pass the resolved TERM string so tmux.conf can derive terminal-features
    # without needing to re-encode emulator names. Null when no preset is active.
    terminalTerm =
      if emulatorCfg != null
      then emulatorCfg.term
      else null;
    inherit (cfg) sixelTerminals;
    extraConfText = cfg.extraConfig;
    persistWireScript = tmuxRemuxWireScript;
    enrichEnable = cfg.enrich.enable;
    enrichProviders = cfg.enrich.providers;
    enrichPrRefreshSeconds = cfg.enrich.prRefreshSeconds;
    enrichPrCheckRefreshSeconds = cfg.enrich.prCheckRefreshSeconds;
    enrichIcons = cfg.enrich.icons;
    splashEnable = cfg.splash.enable;
    splashTips = cfg.splash.tips;
    splashTimeout = cfg.splash.timeout;
    splashRemote = cfg.splash.remote;
    aiNamingEnable = cfg.aiNaming.enable;
    agentUsageEnable = cfg.agentUsage.enable;
    agentUsageRefreshSeconds = cfg.agentUsage.refreshSeconds;
    agentUsageMonthlyThreshold = cfg.agentUsage.monthlyThreshold;
    claudeStatusAssumeDeadAfter = cfg.claudeStatus.assumeDeadAfter;
    notifyEnable = cfg.notifications.enable;
    # Only stamp @remux_relaunch when tmux-remux is actually installed to read it.
    resumeClaudeEnable = cfg.persist.enable && cfg.persist.package != null && cfg.persist.resumeClaude;
    inherit resumeCarouselEnable;
  };

  inherit (pkgs.stdenv.hostPlatform) isLinux isDarwin;

  persistEnabled = cfg.persist.enable && cfg.persist.package != null;

  # Only provision the codex hook when tmux-remux is actually installed to
  # act on @remux_relaunch, mirroring resumeClaudeEnable above.
  resumeCodexEnable = cfg.persist.enable && cfg.persist.package != null && cfg.persist.resumeCodex;

  # Only provision the cursor resume hooks when tmux-remux is actually
  # installed to act on @remux_relaunch, mirroring resumeCodexEnable above.
  resumeCursorEnable = cfg.persist.enable && cfg.persist.package != null && cfg.persist.resumeCursor;

  # Only stamp the carousel pane's @remux_relaunch when tmux-remux is actually
  # installed to read it, mirroring resumeCursorEnable above.
  # carousel-aeye is part of the gate, not just persist: with the viewer package
  # unwired there is no store path to stamp, so @resume_carousel must stay off
  # rather than leave tmux-update-icons' own placeholder guard as the only thing
  # standing between a misconfigured consumer and a broken @remux_relaunch.
  resumeCarouselEnable = cfg.persist.enable && cfg.persist.package != null && cfg.persist.resumeCarousel && carousel-aeye != null;

  # Stable startup script shared by the Linux systemd service and the darwin
  # launchd agent. Resolves tmux from the user profile so the unit/plist never
  # embeds a nix store path (no churn on update).
  tmux-startup-script = pkgs.writeShellScript "tmux-startup" ''
    # Resolve tmux from user profile (avoids hardcoded store paths in the unit)
    # Try per-user profile (NixOS/home-manager/nix-darwin) then nix-profile (nix-env)
    for candidate in "/etc/profiles/per-user/$USER/bin/tmux" "$HOME/.nix-profile/bin/tmux"; do
      if [ -x "$candidate" ]; then
        TMUX_BIN="$candidate"
        break
      fi
    done
    if [ -z "''${TMUX_BIN:-}" ]; then
      echo "tmux not found in /etc/profiles/per-user/$USER/bin or ~/.nix-profile/bin" >&2
      exit 1
    fi

    SESSION=${lib.escapeShellArg cfg.startupSession.name}

    # Expand %h and leading ~ to $HOME — tmux does NOT expand format strings
    # (or shell ~) in the -c argument; it passes the path directly to chdir.
    DIRECTORY=${lib.escapeShellArg cfg.startupSession.directory}
    DIRECTORY="''${DIRECTORY//%h/$HOME}"
    DIRECTORY="''${DIRECTORY/#~/$HOME}"

    # Exact-match check (`=name`) — default is prefix match, which would
    # incorrectly skip creation if e.g. `foo-bar` existed when SESSION=foo.
    if "$TMUX_BIN" has-session -t "=$SESSION" 2>/dev/null; then
      echo "tmux session $SESSION already running, skipping"
      exit 0
    fi

    # Try to create the session. If creation fails but the session now
    # exists anyway, something else won the race to create it (e.g.
    # tmux-remux auto-restore on server start). Treat that as success.
    if "$TMUX_BIN" new -s "$SESSION" -c "$DIRECTORY" -d; then
      exit 0
    fi

    if "$TMUX_BIN" has-session -t "=$SESSION" 2>/dev/null; then
      echo "tmux session $SESSION exists (created by another source), continuing"
      exit 0
    fi

    exit 1
  '';
in {
  imports = [
    (lib.mkRenamedOptionModule
      ["programs" "tmux-og" "claudeIntegration" "enable"]
      ["programs" "tmux-og" "agentIntegration" "enable"])
    # zoxideExclude moved from the session-only sessionPicker.* namespace to
    # picker.* alongside layout/listRatio, which apply to both pickers (#286).
    (lib.mkRenamedOptionModule
      ["programs" "tmux-og" "sessionPicker" "zoxideExclude"]
      ["programs" "tmux-og" "picker" "zoxideExclude"])
    (lib.mkRemovedOptionModule
      ["programs" "tmux-og" "remote" "enable"]
      "The arch-C reverse-socket promotion was retired (#167). Use programs.tmux-og.remote.hosts for the control-mode bridge picker.")
    (lib.mkRemovedOptionModule
      ["programs" "tmux-og" "remote" "trustedHosts"]
      "The arch-C RemoteForward block was retired (#167). Use programs.tmux-og.remote.hosts for the control-mode bridge picker.")
    (lib.mkRemovedOptionModule
      ["programs" "tmux-og" "persist" "saveInterval"]
      "The periodic remux-save timer unit was dropped in favour of tmux-remux's own tmux 3.8 monitor hook (#344), which fires on the session's own clock rather than a configurable cadence.")
  ];

  options.programs.tmux-og = {
    enable = lib.mkEnableOption "tmux-og - opinionated tmux configuration";

    tmuxPackage = lib.mkOption {
      type = lib.types.package;
      default = tmux-pkg;
      defaultText = lib.literalExpression "inputs.nixpkgs-tmux36.legacyPackages.\${system}.tmux";
      description = ''
        The tmux package tmux-og wraps and installs. Defaults to the flake's
        pinned tmux 3.6a: tmux 3.7 no longer freezes background panes under a
        popup (tmux/tmux#4920), so a popup flickers whenever a full-screen TUI
        redraws behind it. Override with pkgs.tmux to track unstable once
        tmux/tmux#5336 is resolved, or point at a custom build.
      '';
    };

    processIcons = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = {};
      example = lib.literalExpression ''{ "my-app" = "⚡"; }'';
      description = "Extra process name → icon mappings. Overrides built-in defaults on collision.";
    };

    sixelTerminals = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
      example = ["foot" "wezterm"];
      description = ''
        TERM strings of outer terminal emulators that can paint sixel (e.g.
        foot, WezTerm, iTerm2). Each entry gets a `*` suffix appended and emits
        a terminal-features line enabling sixel for that TERM — write
        `[ "foot" ]`, not `[ "foot*" ]`, or the pattern doubles to `foot**`.
        An entry containing a single quote breaks the generated line, the same
        exposure `extraConfig` already has.
      '';
    };

    extraConfig = lib.mkOption {
      type = lib.types.lines;
      default = "";
      example = lib.literalExpression "''set -ga update-environment KITTY_LISTEN_ON''";
      description = ''
        Extra verbatim lines appended to the generated tmux.conf after all
        built-in settings (including persist hooks). Use for one-off tmux
        options that tmux-og does not expose as structured options.
      '';
    };

    prefix = lib.mkOption {
      type = lib.types.str;
      default = "`";
      example = "§";
      description = ''
        tmux prefix key (literal character). Defaults to backtick. On macOS ISO
        keyboards the otherwise-unused § key (left of 1) is a convenient prefix.
      '';
    };

    defaultShell = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/run/current-system/sw/bin/fish";
      description = ''
        Absolute path to the shell tmux spawns in new panes (tmux
        default-shell). When null, tmux uses $SHELL / the account shell. Set
        this when the login shell isn't reliably propagated to the tmux server
        — e.g. launchd-started servers on macOS capture a stale $SHELL, so
        panes open in /bin/zsh even after the account shell is changed.
      '';
    };

    focusFollowsMouse = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        tmux focus-follows-mouse: whether moving the mouse into a pane
        selects it, without clicking. Off by default, matching tmux's own
        default — some terminals/multiplexer stacks interact awkwardly with
        it.
      '';
    };

    copyModeLineNumbers = lib.mkOption {
      type = lib.types.enum ["off" "default" "absolute" "relative" "hybrid"];
      default = "off";
      description = ''
        tmux copy-mode-line-numbers mode (tmux 3.7+): line-number display in
        copy mode. "off" preserves tmux-og's previous behavior.
      '';
    };

    worktrunk = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Whether to install worktrunk and configure tmux integration hooks";
      };

      worktreePath = lib.mkOption {
        type = lib.types.str;
        default = "{{ repo_path }}-worktrees/{{ branch | sanitize }}";
        description = ''
          worktrunk `worktree-path` template for new worktrees. The default is a
          sibling dir next to the repo (see the config.toml comment for why not
          nested). Override to relocate worktrees, e.g. a single external root
          keyed by parent dir + repo so same-named repos across orgs don't collide:
          `''${config.home.homeDirectory}/.worktrees/{{ repo_path | dirname | basename }}/{{ repo_path | basename }}/{{ branch | sanitize }}`.
          Only affects newly-created worktrees; existing ones keep their path.
        '';
      };
    };

    remote = {
      hosts = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [];
        example = ["tp-g6" "lab"];
        description = ''
          ssh Host aliases the session picker (`prefix + s`) probes for remote
          tmux sessions. Enter on a row runs `og-remote-open` (control-mode
          bridge). Empty list hides the remote section.
        '';
      };

      exposePickOnPath = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Expose `og-remote-picker` on PATH via home.packages, so this host
          can serve `prefix + s` → `^o` (the asking host opens *this* host's own
          session picker in a floating pane).

          Deliberately not gated on `remote.hosts`: that list names the hosts a
          machine reaches *out* to, while this script is needed on the machine
          being reached — which typically sets no `remote.hosts` at all. The
          asking side probes for it over a non-interactive ssh, where only the
          per-user profile is on PATH, and treats its absence as "remote tmux-og
          too old".

          Set false on a host that should never be a bridge target.
        '';
      };

      authPersistSeconds = lib.mkOption {
        type = lib.types.ints.between 60 86400;
        default = 14400;
        description = ''
          How long an ssh ControlMaster tmux-og creates survives idle, in
          seconds (clamped 60–86400, default 4h). Passed straight to ssh's
          `ControlPersist`. Two things build one: the picker's auth handshake,
          and the remote-side picker's own probe leg (so its three legs share a
          connection on a host that never needs the handshake). This is an
          *idle* timer covering only the picker's probe and launcher calls,
          which ride this master:
          the remote-bridge daemon opens its own ssh connection with its own
          `ControlPath` and never rides this one, so a live bridge does not
          extend this value — the timer measures time since the last probe or
          launcher call to that host, regardless of whether a mirror is open.
        '';
      };
    };

    persist = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Whether to enable tmux-remux persistence (snapshots, undo, auto-restore).
          Replaces tmux-resurrect/tmux-continuum.
        '';
      };

      restoreMode = lib.mkOption {
        type = lib.types.enum ["auto" "interactive" "off"];
        default = "off";
        description = ''
          Behavior on tmux server start. "off" disables auto-restore (manual
          `prefix + R` still works). "auto" applies the smart filter and restores.
          "interactive" prompts via picker (not yet implemented — falls back to "off").
        '';
      };

      resumeClaude = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Resume Claude Code sessions when a window is restored. When on,
          tmux-update-icons stamps each Claude pane's @remux_relaunch override with
          its session id, so tmux-remux relaunches `claude --resume <uuid>` on
          restore instead of a bare shell. Restore is manual-by-default
          (restoreMode = "off"), so this only fires on an explicit restore.
        '';
      };

      resumeCodex = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Resume Codex sessions when a window is restored. When on, home-manager
          activation idempotently ensures a `[[hooks.SessionStart]]` block exists
          in `~/.codex/config.toml`, pointing at the `codex-relaunch-stamp`
          binary (matcher `"startup|resume"`); the hook stamps each Codex pane's
          `@remux_relaunch` with its resumable session id, so tmux-remux relaunches
          `codex resume <uuid>` on restore instead of a bare shell.

          Defaults to false, unlike resumeClaude: this mutates an EXTERNAL
          tool's config file (not just internal tmux state), and codex requires
          a one-time manual trust step per machine before the hook is allowed to
          run headlessly — run `codex`, open `/hooks`, and choose "Trust all".
          There is no way to pre-seed that trust declaratively (the trust hash is
          an undocumented, versioned, content-based digest), so this is opt-in
          until you've done that step. Restore is manual-by-default
          (restoreMode = "off"), so this only fires on an explicit restore.
        '';
      };

      resumeCursor = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Resume Cursor Agent CLI sessions when a window is restored. When on,
          home-manager activation idempotently upserts a `sessionStart` AND
          `beforeSubmitPrompt` hook entry into `~/.cursor/hooks.json`
          (marker-guarded, like `cursorStatus`'s status hooks — aeye/user entries
          are left alone) pointing at the `cursor-relaunch-stamp` binary; the hook
          reads the chat id (`conversation_id`, falling back to `session_id`) out
          of the hook's own JSON payload and stamps the pane's `@remux_relaunch`
          with `cursor-agent --resume <chatId>`, so tmux-remux relaunches the
          actual chat (not a bare shell) on restore.

          `sessionStart` alone would only fire once per new conversation — Cursor
          never re-fires it on `--resume` — so the hook is also wired on
          `beforeSubmitPrompt` to re-stamp on every subsequent turn, including
          turns sent after a restore. This means resume survives repeated restore
          cycles as long as at least one message is sent per cycle; a restored
          pane that receives zero further turns before the next save reverts to a
          plain shell on the restore after that. Unlike `resumeClaude`'s
          continuous poll or `resumeCodex`'s `startup|resume` hook matcher,
          Cursor's CLI gives no re-fire-on-resume event to hang this off.

          Defaults to false, like `persist.resumeCodex`: this is a brand-new
          capture path across an externally-versioned CLI, not the long-proven
          Claude transcript path. If a future Cursor CLI version renames or drops
          the id field, the hook stamps nothing and the pane restores as a plain
          shell rather than a broken resume command. Restore is manual-by-default
          (restoreMode = "off"), so this only fires on an explicit restore.
        '';
      };

      resumeCarousel = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Resume the agent-carousel image viewer when a window is restored.
          When on, tmux-update-icons stamps the carousel pane's
          `@remux_relaunch` override with a fixed store-path relaunch that
          execs the aeye viewer at that pane's restored key, so tmux-remux
          relaunches the carousel instead of a bare shell. The pane becomes
          the viewer at the new key rather than the one it had before the
          restore — aeye's own `session-backfill` hook is what supplies the
          images there, from the resumed Claude session's transcript. A
          window that restores with two agent panes is ambiguous (which
          agent's images does the carousel show), so host discovery resolves
          to the lowest `pane_index` among them.

          Defaults to false, like `persist.resumeCodex` and `resumeCursor`:
          the two-agent-panes case picks a sibling by convention rather than
          certainty, and a viewer relaunched into the wrong pane's images is a
          more visible mistake than a plain shell. Restore is manual-by-default
          (restoreMode = "off"), so this only fires on an explicit restore.
        '';
      };

      package = lib.mkOption {
        type = lib.types.nullOr lib.types.package;
        default = tmux-remux-pkg;
        defaultText = lib.literalExpression "inputs.tmux-remux.packages.\${system}.default";
        description = ''
          The tmux-remux package to use. Defaults to the flake input. Set to a
          different derivation to override (e.g. a local checkout for dev).
        '';
      };
    };

    enrich = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          PR + issue-tracker window enrichment: stamps tmux windows with the
          Linear/GitHub issue id and PR check-state for their worktree's branch,
          and adds `prefix + i` keybinds to open the issue/PR or force refresh.
        '';
      };

      providers = lib.mkOption {
        type = lib.types.listOf (lib.types.enum ["linear" "github"]);
        default = ["linear" "github"];
        description = "Issue-tracker providers, tried in priority order. First match wins.";
      };

      prRefreshSeconds = lib.mkOption {
        type = lib.types.ints.between 10 300;
        default = 120;
        description = ''
          Background PR enrichment cadence in seconds (clamped 10–300). Each pass
          spends GitHub API budget per repo, so short cadences across many
          worktree windows can exhaust the hourly quota on their own.
        '';
      };

      prCheckRefreshSeconds = lib.mkOption {
        type = lib.types.ints.between 10 300;
        default = 300;
        description = ''
          CI check-state refresh cadence in seconds (clamped 10–300). PR
          identity still refreshes at `prRefreshSeconds`; this slower query
          keeps routine status-line polling within GitHub's API budget. A
          repo with pending checks re-polls every 30 seconds (never slower
          than this value) until they settle.
        '';
      };

      icons = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = {};
        example = lib.literalExpression ''{ linear = "<glyph>"; github = "<glyph>"; }'';
        description = ''
          Override enrichment icon glyphs (keys: linear, github, pending,
          success, failure, merged, closed, conflict, draft). Unset keys fall
          back to nerd-font defaults. Values must not contain '#' (tmux format
          escape).
        '';
      };
    };

    notifications = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Notifications for asynchronous events: a Claude pane transitioning to
          waiting/error/denied, a PR merging or its CI flipping, and window
          bells. The event's own window being the current one gets a styled
          message line; anything else lands in the history buffer on
          `prefix + n` (which shadows tmux's built-in next-window — this config
          already binds M-L / M-H for that).
        '';
      };
    };

    agentUsage = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Coding-agent usage-limit stats (Claude/Codex/Cursor) on the top-right
          of status line 0, shown while any coding agent is running. Data comes
          from the providers' own usage endpoints using each CLI's stored
          credentials — no extra API keys. Agents without meaningful caps (e.g.
          Cursor on an enterprise tier) stay hidden.
        '';
      };

      refreshSeconds = lib.mkOption {
        type = lib.types.ints.between 10 300;
        default = 120;
        description = ''
          Usage-endpoint polling cadence in seconds (clamped 10–300). Each pass
          spends provider API budget, and the numbers move slowly, so short
          cadences buy little.
        '';
      };

      monthlyThreshold = lib.mkOption {
        type = lib.types.ints.between 0 100;
        default = 50;
        description = ''
          Utilization percent at which the monthly spend window joins the
          always-on short-window stats. Below it the monthly segment stays
          hidden — it's the "only when close to the cap" window.
        '';
      };
    };

    claudeStatus = {
      assumeDeadAfter = lib.mkOption {
        type = lib.types.ints.unsigned;
        default = 0;
        description = ''
          Seconds a pane may go without running a coding agent before a stale
          agent state on it is withdrawn instead of just fading. Closes the
          "agent exited, pane went back to a shell" gap: no hook fires on that
          transition, so the last state written otherwise lives until the tmux
          server restarts.

          Reaches the shell consumers of `read_pane_state` only: the per-window
          and active-pane agent icons, `claude-status`, and the kill-pane guard.
          The session pill on status line 0 and the `prefix + s`/`prefix + w`
          pickers are rendered by the Go binaries, which read the state files
          directly and still show the withdrawn state.

          0 (the default) disables it — no presence stamps are written and the
          check never runs. Values below 15 are clamped to 15, three sweeps of
          headroom over the 5s presence cadence.

          Only `processing`/`compacting`/`done` are ever withdrawn, and only
          once already past their own staleness threshold (5 minutes for
          `processing`). `waiting`/`error`/`denied` block a human and are never
          touched.
        '';
      };
    };

    splash = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Whether to show the animated welcome-buffer splash on fresh sessions.";
      };
      timeout = lib.mkOption {
        type = lib.types.ints.between 1 120;
        default = 10;
        description = "Seconds before the splash auto-dismisses (also dismissed by any key).";
      };
      tips = lib.mkOption {
        type = lib.types.listOf (lib.types.submodule {
          options = {
            key = lib.mkOption {
              type = lib.types.str;
              description = "Key hint; the token `prefix` is replaced with the real prefix.";
            };
            label = lib.mkOption {
              type = lib.types.str;
              description = "What the key does.";
            };
          };
        });
        default = [
          {
            key = "prefix + s";
            label = "Sessions";
          }
          {
            key = "prefix + w";
            label = "Windows";
          }
          {
            key = "prefix + W";
            label = "Window wall";
          }
          {
            key = "prefix + a";
            label = "Claude windows";
          }
          {
            key = "prefix + i";
            label = "Issues / PRs";
          }
          {
            key = "prefix + g";
            label = "LazyGit";
          }
          {
            key = "prefix + b";
            label = "btop";
          }
          {
            key = "prefix + R";
            label = "Restore snapshot";
          }
          {
            key = "prefix + u";
            label = "Undo close";
          }
        ];
        description = "Keybind cheatsheet shown in the welcome buffer. Empty = mascot only.";
      };
      remote = lib.mkOption {
        type = lib.types.enum ["skip" "static" "full"];
        default = "full";
        description = ''
          Behavior for the passive welcome-buffer splash when the attaching
          tmux client came in over ssh: "full" shows the normal animated
          splash (default, unchanged behavior); "static" shows a single
          already-resolved frame with no periodic redraw (cheaper over a
          slow link); "skip" shows nothing for that attach (a later local
          attach on the same tmux server still gets the splash it never
          got). Detected via `#{I/e:SSH_CONNECTION}` interrogated against the
          attaching client directly, not the session's environment table and
          not this process's own env. Does not affect the on-demand
          `prefix + C-Space` splash, which always shows the full animated
          version.
        '';
      };
    };

    aiNaming = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Whether fallback windows — no tracked issue, on the default branch —
          get a descriptive title from the pane's own Claude. When enabled, the
          Claude Code plugin's UserPromptSubmit hook nudges Claude (which has
          full conversation context) to name such a window once via
          `claude-status-update name set`, and again if the focus clearly
          shifts. No separate API call — the running session does it. Requires
          the tmux-og Claude Code plugin (or `skills.enable`) to be installed.
        '';
      };
    };

    skills = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Whether to install Claude Code skills into ~/.claude/skills (tmux-og skills and agent-carousel skills). Disable when the tmux-og Claude Code plugin is installed (marketplace or --plugin-dir) — the plugin ships the same skills.";
      };
    };

    opencode = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Whether to install OpenCode status plugin into ~/.config/opencode/plugin";
      };
    };

    codexStatus = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Drive the tmux status line from Codex's native hooks (processing /
          waiting / done / compacting / idle) instead of the two states the
          screen-scraper (agent-detect) can infer. Not full parity with Claude:
          error, denied and interrupted have no codex hook event to hang off, so
          they stay unreachable this way (#158). When on, home-manager activation
          idempotently appends `[[hooks.*]]` blocks to `~/.codex/config.toml`
          (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse,
          PermissionRequest, Stop, PreCompact, PostCompact) that call
          `claude-status-update <state>`, keyed off the pane's $TMUX_PANE — no
          hook payload is parsed. Requires agentIntegration.enable (asserted):
          the hooks reference
          claude-status-update by its rebuild-stable profile path so codex's hook
          trust survives tmux-og bumps.

          Defaults to false, like `persist.resumeCodex` and for the same reasons:
          it mutates an EXTERNAL tool's config file, and codex requires a
          one-time manual trust step per machine before the hooks run — run
          `codex`, open `/hooks`, and choose "Trust all".
        '';
      };
    };

    cursorStatus = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Drive the tmux status line from Cursor Agent CLI hooks
          (sessionStart / beforeSubmitPrompt / preToolUse / postToolUse /
          postToolUseFailure / preCompact / stop / subagentStart) via a thin
          `cursor-status-hook` wrapper around `claude-status-update`, keyed off
          `$TMUX_PANE`. Home-manager activation upserts entries into
          `~/.cursor/hooks.json` every switch (strips prior
          `/bin/cursor-status-hook` commands, leaves other entries alone —
          including aeye's carousel hooks). Requires agentIntegration.enable
          (asserted): the wrapper and `claude-status-update` must sit side by
          side on the rebuild-stable profile path.

          Defaults to false: mutates an EXTERNAL tool's config file. agent-detect
          remains the backfill (and the source of `waiting` — Cursor has no clean
          permission-prompt hook equivalent).
        '';
      };
    };

    agentIntegration = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Expose claude-status-update and claude-status on PATH via
          home.packages. Needed when agent hooks (Claude Code, Codex, OpenCode,
          Cursor) call them by bare name in a shell that doesn't inherit the tmux
          wrapper's PATH — e.g. a fish login shell, or a direnv-loaded devshell.
        '';
      };
    };

    popupTools = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [pkgs.sesh pkgs.lazygit pkgs.yazi pkgs.btop];
      defaultText = lib.literalExpression "[pkgs.sesh pkgs.lazygit pkgs.yazi pkgs.btop]";
      description = ''
        Tools installed via home.packages so popup keybindings
        (prefix+g → lazygit, prefix+b → btop, prefix+y → yazi) resolve in
        shells that don't inherit the tmux wrapper's PATH prepends — e.g.
        fish login shells opened by display-popup, or direnv-loaded
        devshells. sesh has no binding anymore but stays
        for external `sesh connect` CLI workflows.

        Set to [] to opt out entirely, or drop individual entries if you
        install those tools elsewhere (home-manager errors if two
        different derivations install the same file).

        prefix+k → k9s is deliberately not in the default: k9s pulls kubectl
        along for 237 MB. Add pkgs.k9s here (or install it any other way) to
        make that bind resolve.
      '';
    };

    carouselDiagramTools = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [carousel-aeye carousel-toggle pkgs.resvg];
      defaultText = lib.literalMD "the aeye binary + `tmux-claude-images` + `pkgs.resvg` when the agent-carousel flake input is wired in";
      description = ''
        Renderers installed via home.packages so the agent-carousel diagram
        hook (a PostToolUse hook) can turn the `.d2` files an agent writes into
        PNG images for the carousel. The hook calls `aeye render-diagram`, which
        embeds the d2 compiler in-process and shells out to `resvg`, so it needs
        `aeye` and `resvg` on PATH (the same PATH-reach reason as popupTools).
        It then auto-opens the carousel with a bare `tmux-claude-images
        --ensure-open`, which needs the profile copy: a pane's shell may
        rebuild PATH from the login profile and drop the tmux wrapper's.
        Only installed when the agent-carousel flake input is wired in
        (carousel-toggle != null).

        Set to [] to opt out of diagram rendering (the hook then no-ops
        silently), or drop an entry if you install it elsewhere.
      '';
    };

    picker = {
      layout = lib.mkOption {
        type = lib.types.enum ["preview" "list"];
        default = "preview";
        example = "list";
        description = ''
          What `prefix + s` and `prefix + w` open with. `preview` shows the
          live pane-content preview below the list (the historical default);
          `list` opens list-only in a shorter popup, so a full-height list is
          not mostly blank. `^/` still toggles the preview for the current
          invocation — this only sets the starting state.
        '';
      };

      listRatio = lib.mkOption {
        type = lib.types.ints.between 20 80;
        default = 50;
        example = 30;
        description = ''
          Percentage of the picker body the list gets; the pane-content
          preview takes the rest. Lower means a taller preview — 30 gives
          the preview about two thirds. Ignored when `layout = "list"`.
          Clamped to 20-80 by the picker as well, so neither pane can collapse.
        '';
      };

      zoxideExclude = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [".ssh" "/tmp/*"];
        example = [".ssh" "/tmp/*" "*/node_modules"];
        description = ''
          Patterns the session picker drops from its zoxide directory
          suggestions. A pattern matches when it equals the path, is an
          ancestor dir of it (subtree), or globs the full path or its
          basename — so ".ssh" hides any dir named .ssh, "/tmp/*" hides
          /tmp children, and "/home/you/Downloads" hides that subtree.
          Set to [] to suggest every zoxide dir.
        '';
      };
    };

    startupSession = {
      enable = lib.mkEnableOption "systemd service to start a tmux session on login";

      headless = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Start the session without waiting for a graphical login: bind the
          service to `default.target` instead of `graphical-session.target` and
          skip the DISPLAY/WAYLAND_DISPLAY import, which has nothing to read.

          Enable this on a host you reach over ssh — with the default graphical
          gating, a machine sitting at its login screen never starts a tmux
          server, so there is nothing for the remote bridge to attach to. See
          `linger`, which is on by default and makes that server survive logout.
        '';
      };

      linger = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = ''
          Enable systemd lingering for the user so the startup tmux server
          survives logout. Without it, systemd stops the user manager — and the
          tmux server with it — when the last login session ends; a session
          cold-started by the remote bridge (over ssh) then dies the moment the
          bridge disconnects.

          When on (the default) and `enable` is set, home-manager activation
          runs `loginctl enable-linger` for the user on Linux. It is idempotent,
          needs no privilege (systemd's polkit `set-self-linger` allows a user
          to linger itself), and a failure only warns — it never aborts the
          switch. Activation never *disables* lingering: turning this off later
          leaves an already-lingering user alone rather than guessing the user
          did not want it for some other reason.

          Set to false to manage lingering yourself — e.g. declaratively with
          NixOS `users.users.<name>.linger = true`, which is revertible. With
          `enable` on and this off, activation warns that the server will not
          survive logout.
        '';
      };

      name = lib.mkOption {
        type = lib.types.str;
        default = "main";
        description = "Name of the tmux session to create on login";
      };

      directory = lib.mkOption {
        type = lib.types.str;
        default = "~";
        description = ''
          Starting directory for the session. Leading `~` and any `%h` are
          expanded to `$HOME` by the startup script before being passed to
          `tmux new -c` (tmux itself does not expand these in `-c`).
        '';
      };

      terminal = {
        emulator = lib.mkOption {
          type = lib.types.nullOr (lib.types.enum ["ghostty" "kitty"]);
          default = null;
          description = ''
            Terminal emulator preset. When set, auto-configures TERM,
            TERM_PROGRAM, TERMINFO, and tmux terminal-features/overrides
            for the chosen emulator. The emulator package must be available
            in pkgs. Set to null to configure terminal options manually.
          '';
          example = "ghostty";
        };

        term = lib.mkOption {
          type = lib.types.str;
          default = "xterm-256color";
          description = "TERM value for the tmux session (ignored when emulator is set)";
        };

        colorterm = lib.mkOption {
          type = lib.types.str;
          default = "truecolor";
          description = "COLORTERM value";
        };

        termProgram = lib.mkOption {
          type = lib.types.str;
          default = "";
          description = "TERM_PROGRAM value (ignored when emulator is set)";
        };

        terminfoPath = lib.mkOption {
          type = lib.types.nullOr lib.types.str;
          default = null;
          description = "Path to terminfo directory (ignored when emulator is set)";
        };
      };
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions =
        lib.optional (
          cfg.startupSession.terminal.emulator
          != null
          && !emulatorCfg.available
        ) {
          assertion = false;
          message = ''
            programs.tmux-og.startupSession.terminal.emulator = "${cfg.startupSession.terminal.emulator}"
            but pkgs.${cfg.startupSession.terminal.emulator} is not available.
            Add it to your packages or set terminal.emulator = null and configure manually.
          '';
        }
        ++ lib.optional (cfg.codexStatus.enable && !cfg.agentIntegration.enable) {
          assertion = false;
          message = ''
            programs.tmux-og.codexStatus.enable requires agentIntegration.enable:
            the codex hooks call claude-status-update by its rebuild-stable profile
            path, which agentIntegration installs. Without it the binary isn't on
            the profile and codex would re-prompt for hook trust on every bump.
          '';
        }
        ++ lib.optional (cfg.cursorStatus.enable && !cfg.agentIntegration.enable) {
          assertion = false;
          message = ''
            programs.tmux-og.cursorStatus.enable requires agentIntegration.enable:
            the Cursor hooks call cursor-status-hook (sibling of claude-status-update)
            on the rebuild-stable profile path, which agentIntegration installs.
          '';
        };

      home = {
        packages =
          [tmuxConfig.tmux-wrapped tmuxConfig.og]
          ++ lib.optionals cfg.worktrunk.enable [pkgs.worktrunk]
          ++ lib.optionals (cfg.persist.enable && cfg.persist.package != null) [cfg.persist.package]
          ++ lib.optionals cfg.agentIntegration.enable [
            tmuxConfig.script.claude-status-update
            tmuxConfig.script.claude-status
          ]
          ++ lib.optionals cfg.cursorStatus.enable [
            tmuxConfig.script.cursor-status-hook
            tmuxConfig.script.cursor-hooks-install
          ]
          ++ lib.optionals resumeCodexEnable [tmuxConfig.script.codex-relaunch-stamp]
          ++ lib.optionals resumeCursorEnable [tmuxConfig.script.cursor-relaunch-stamp tmuxConfig.script.cursor-relaunch-hooks-install]
          ++ lib.optionals resumeCarouselEnable [tmuxConfig.script.tmux-carousel-restore]
          ++ lib.optionals cfg.enrich.enable [
            tmuxConfig.script.tmux-issue-stamp
            tmuxConfig.script.tmux-issue-stamp-linear
            tmuxConfig.script.tmux-issue-stamp-github
            tmuxConfig.script.tmux-pr-enrich
          ]
          ++ lib.optionals cfg.remote.exposePickOnPath [
            tmuxConfig.script.og-remote-picker
          ]
          ++ lib.optionals (carousel-toggle != null) cfg.carouselDiagramTools
          ++ cfg.popupTools;

        file =
          lib.optionalAttrs cfg.skills.enable (
            lib.mapAttrs' (name: _: {
              name = ".claude/skills/${name}";
              value.source = ../claude-plugin/skills/${name};
            }) (builtins.readDir ../claude-plugin/skills)
          )
          # agent-carousel ships its skills via the flake input; omitted when the
          # module is imported standalone (no agent-carousel input → null).
          // lib.optionalAttrs (cfg.skills.enable && carouselPluginSkills != null) (
            lib.mapAttrs' (name: _: {
              name = ".claude/skills/${name}";
              value.source = "${carouselPluginSkills}/${name}";
            }) (builtins.readDir carouselPluginSkills)
          )
          // lib.optionalAttrs cfg.opencode.enable {
            ".config/opencode/plugin/opencode-status.ts".source = ../plugins/opencode-status.ts;
          };

        # Reload tmux config + reflow all sessions after profile switch.
        # The config embeds full /nix/store paths, so reloading makes the running
        # server use new script versions without restart. Reflow regenerates
        # status-format lines for all sessions with the new reflow script.
        # Run after restoreTheme (which sources the config and sets theme vars).
        # We only need to: ensure config is loaded, then reflow all sessions.
        activation = {
          # Lingering keeps the startup server alive past logout (see the
          # startupSession.linger option). enable-linger is idempotent and needs
          # no privilege for self-linger; a failure only warns. Never disables —
          # see the option doc for why the asymmetry is deliberate.
          startupLinger = lib.mkIf (isLinux && cfg.startupSession.enable) (
            lib.hm.dag.entryAfter ["writeBoundary"] (
              if cfg.startupSession.linger
              then ''
                if ! ${lib.getExe' pkgs.systemd "loginctl"} show-user "$USER" -p Linger --value 2>/dev/null | grep -qx yes; then
                  ${lib.getExe' pkgs.systemd "loginctl"} enable-linger "$USER" \
                    || echo "tmux-og: could not enable lingering for $USER; the startup tmux server will not survive logout" >&2
                fi
              ''
              else ''
                if ! ${lib.getExe' pkgs.systemd "loginctl"} show-user "$USER" -p Linger --value 2>/dev/null | grep -qx yes; then
                  echo "tmux-og: startupSession.enable is on but startupSession.linger is off and $USER is not lingering — the tmux server will die at logout. Enable it declaratively with 'users.users.$USER.linger = true' or set programs.tmux-og.startupSession.linger = true." >&2
                fi
              ''
            )
          );

          reloadTmux = lib.hm.dag.entryAfter ["writeBoundary" "restoreTheme"] ''
            TMUX=${pkgs.tmux}/bin/tmux
            REFLOW=${tmuxConfig.script.tmux-reflow-windows}/bin/tmux-reflow-windows
            if $TMUX info &>/dev/null 2>&1; then
              # Source config (restoreTheme may have already done this, but it's
              # idempotent and handles the case where restoreTheme doesn't exist)
              if SOURCE_ERR=$($TMUX source-file ${tmuxConfig.tmuxConf} 2>&1); then
                # Wait for async run-shell plugin commands to finish
                sleep 1
                # Reflow ALL sessions
                WIDTH=$($TMUX list-clients -F '#{client_width}' 2>/dev/null | head -1)
                WIDTH=''${WIDTH:-200}
                while read -r sess; do
                  [ -n "$sess" ] && "$REFLOW" "$sess" "$WIDTH" || true
                done < <($TMUX list-sessions -F '#{session_name}' 2>/dev/null)
              else
                echo "tmux-og: tmux rejected the new config during home-manager switch:" >&2
                echo "$SOURCE_ERR" >&2
                echo "tmux-og: restart the tmux server to pick up this generation." >&2
              fi
            fi
          '';

          # Ensure the codex SessionStart hook block exists in the user's
          # ~/.codex/config.toml. Codex only auto-loads that one file globally
          # (confirmed via `codex --help`'s -c flag doc and the #140 spike notes:
          # no drop-in dir, no CODEX_HOME layered profile without `-p`); a real
          # config.toml can carry substantial hand-edited content (model,
          # mcp_servers, per-project trust), so home.file would clobber it.
          # Existing tmux-og blocks are updated in place so the command stays on
          # the rebuild-stable profile path after a Nix package update. Trust for
          # the hook itself still requires a one-time manual `/hooks` -> "Trust
          # all" per machine (see the resumeCodex option doc).
          provisionCodexResumeHook = lib.mkIf resumeCodexEnable (
            lib.hm.dag.entryAfter ["writeBoundary"] (
              let
                resumeBinary = "${config.home.profileDirectory}/bin/codex-relaunch-stamp";
              in ''
                CONFIG="$HOME/.codex/config.toml"
                MARKER='# tmux-og-managed: codex resume-on-restore SessionStart hook'
                # Pre-rename spelling, matched so an existing block is found and
                # reused as the sed anchor rather than duplicated. Never rewritten
                # to the new spelling: this is the user's file. Drop this once no
                # machine carries a `lazytmux-managed:` block.
                LEGACY_MARKER='# lazytmux-managed: codex resume-on-restore SessionStart hook'
                mkdir -p "$(dirname "$CONFIG")"
                touch "$CONFIG"
                FOUND_MARKER=""
                if grep -qF "$MARKER" "$CONFIG"; then
                  FOUND_MARKER="$MARKER"
                elif grep -qF "$LEGACY_MARKER" "$CONFIG"; then
                  FOUND_MARKER="$LEGACY_MARKER"
                fi
                if [ -n "$FOUND_MARKER" ]; then
                  run sed -i "/^$FOUND_MARKER$/,/^timeout = 30$/ s#^command = .*codex-relaunch-stamp.*#command = \"${resumeBinary}\"#" "$CONFIG"
                else
                  {
                    echo ""
                    echo "$MARKER"
                    echo '[[hooks.SessionStart]]'
                    echo 'matcher = "startup|resume"'
                    echo ""
                    echo '[[hooks.SessionStart.hooks]]'
                    echo 'type = "command"'
                    echo 'command = "${resumeBinary}"'
                    echo 'timeout = 30'
                  } >> "$CONFIG"
                fi
              ''
            )
          );

          # Idempotently append codex status-line hook blocks to config.toml.
          # Mirrors provisionCodexResumeHook (marker-guarded append, same one-time
          # /hooks trust caveat — see that block and the codexStatus.enable doc).
          # One [[hooks.<event>]] entry per state; codex resolves the pane from
          # $TMUX_PANE so the commands carry no payload. NOT a copy of hooks.json:
          # codex ships 10 hook events and Claude Code's Notification/StopFailure/
          # PermissionDenied/SessionEnd are not among them, so `waiting` comes from
          # PermissionRequest here and error/denied/interrupted have no hook source
          # at all (#158 tracks reaching those by other means). An event name codex
          # doesn't know is accepted silently by config parsing and simply never
          # fires, so every entry below is checked against codex's own embedded
          # per-event schemas; SessionStart, UserPromptSubmit, PreToolUse,
          # PermissionRequest and Stop were additionally watched firing on a live
          # pane.
          #
          # Every command redirects stdout: codex parses hook stdout, and anything
          # non-JSON marks the hook failed with a visible TUI error ("hook returned
          # invalid stop hook JSON output"), while UserPromptSubmit stdout is
          # injected into the model's context. claude-status-update is silent on the
          # state path today; the redirect keeps a future stray echo from surfacing
          # in every codex turn.
          provisionCodexStatusHooks = lib.mkIf cfg.codexStatus.enable (
            lib.hm.dag.entryAfter ["writeBoundary"] (
              let
                # Stable profile path, NOT a /nix/store path: codex records hook
                # trust as a content hash over the config, so a store path that
                # changes every tmux-og rebuild would force a fresh `/hooks` trust
                # each bump. The profile path is rebuild-stable. Requires
                # agentIntegration.enable (asserted below) to put the binary there.
                csu = "${config.home.profileDirectory}/bin/claude-status-update";
                hookBlock = ''
                  # tmux-og-managed: codex status-line hooks
                  [[hooks.SessionStart]]
                  matcher = "startup|resume"

                  [[hooks.SessionStart.hooks]]
                  type = "command"
                  command = "${csu} cleanup >/dev/null"
                  timeout = 30

                  [[hooks.SessionStart.hooks]]
                  type = "command"
                  command = "${csu} idle >/dev/null"
                  timeout = 30

                  [[hooks.UserPromptSubmit]]

                  [[hooks.UserPromptSubmit.hooks]]
                  type = "command"
                  command = "${csu} processing --force >/dev/null"
                  timeout = 30

                  [[hooks.PreToolUse]]

                  [[hooks.PreToolUse.hooks]]
                  type = "command"
                  command = "${csu} processing >/dev/null"
                  timeout = 30

                  [[hooks.PostToolUse]]

                  [[hooks.PostToolUse.hooks]]
                  type = "command"
                  command = "${csu} processing >/dev/null"
                  timeout = 30

                  [[hooks.PermissionRequest]]

                  [[hooks.PermissionRequest.hooks]]
                  type = "command"
                  command = "${csu} waiting >/dev/null"
                  timeout = 30

                  [[hooks.Stop]]

                  [[hooks.Stop.hooks]]
                  type = "command"
                  command = "${csu} done >/dev/null"
                  timeout = 30

                  [[hooks.PreCompact]]

                  [[hooks.PreCompact.hooks]]
                  type = "command"
                  command = "${csu} compacting >/dev/null"
                  timeout = 30

                  [[hooks.PostCompact]]

                  [[hooks.PostCompact.hooks]]
                  type = "command"
                  command = "${csu} processing >/dev/null"
                  timeout = 30
                '';
              in ''
                CONFIG="$HOME/.codex/config.toml"
                MARKER='# tmux-og-managed: codex status-line hooks'
                # Pre-rename spelling, matched so an existing block counts as
                # present and is not appended a second time (duplicate hooks fire
                # twice, and the content change re-prompts /hooks trust). Never
                # rewritten to the new spelling, for the same reason the stale
                # Notification block below is left alone. Drop this once no machine
                # carries a `lazytmux-managed:` block.
                LEGACY_MARKER='# lazytmux-managed: codex status-line hooks'
                mkdir -p "$(dirname "$CONFIG")"
                touch "$CONFIG"
                FOUND_MARKER=""
                if grep -qF "$MARKER" "$CONFIG"; then
                  FOUND_MARKER="$MARKER"
                elif grep -qF "$LEGACY_MARKER" "$CONFIG"; then
                  FOUND_MARKER="$LEGACY_MARKER"
                fi
                if [ -z "$FOUND_MARKER" ]; then
                  printf '\n%s' ${lib.escapeShellArg hookBlock} >>"$CONFIG"
                elif grep -qF '[[hooks.Notification]]' "$CONFIG"; then
                  # Append-once by design (codex hashes the config for hook trust,
                  # so rewriting it would re-prompt), so a block written before the
                  # Notification→PermissionRequest fix can't be corrected in place.
                  # Removing it stays the user's call — tmux-og does not own this file.
                  echo "tmux-og: ~/.codex/config.toml has a stale tmux-og hook block ([[hooks.Notification]] is not a codex event, so 'waiting' never fires)." >&2
                  echo "tmux-og: delete the block under '$FOUND_MARKER' and re-run home-manager switch to get the corrected one." >&2
                elif ! grep -qF ${lib.escapeShellArg csu} "$CONFIG"; then
                  echo "tmux-og: ~/.codex/config.toml status hooks don't reference ${csu}." >&2
                  echo "tmux-og: delete the block under '$FOUND_MARKER' and re-run home-manager switch (then re-trust /hooks), or sed the command paths in place." >&2
                fi
              ''
            )
          );

          # Upsert Cursor CLI status hooks into ~/.cursor/hooks.json every switch
          # (strip prior /bin/cursor-status-hook entries; leave aeye/user alone).
          provisionCursorStatusHooks = lib.mkIf cfg.cursorStatus.enable (
            lib.hm.dag.entryAfter ["writeBoundary"] ''
              run env PATH="${lib.makeBinPath [pkgs.jq pkgs.coreutils]}:$PATH" \
                ${tmuxConfig.script.cursor-hooks-install}/bin/cursor-hooks-install \
                ${config.home.profileDirectory}/bin/cursor-status-hook
            ''
          );

          # Upsert Cursor CLI resume-on-restore hooks into ~/.cursor/hooks.json every
          # switch (strip prior /bin/cursor-relaunch-stamp entries; leave
          # aeye/cursor-status-hook/user entries alone). No agentIntegration
          # assertion needed (unlike cursorStatus/codexStatus) — cursor-relaunch-stamp
          # calls tmux directly, no claude-status-update dependency.
          provisionCursorResumeHook = lib.mkIf resumeCursorEnable (
            lib.hm.dag.entryAfter ["writeBoundary"] ''
              run env PATH="${lib.makeBinPath [pkgs.jq pkgs.coreutils]}:$PATH" \
                ${tmuxConfig.script.cursor-relaunch-hooks-install}/bin/cursor-relaunch-hooks-install \
                ${config.home.profileDirectory}/bin/cursor-relaunch-stamp
            ''
          );
        };
      };

      xdg.configFile = {
        "worktrunk/config.toml" = lib.mkIf cfg.worktrunk.enable {
          text = ''
            # Default (worktreePath option): sibling dir, NOT nested under the
            # repo — a worktree inside the repo working tree sits under watchman's
            # already-watched root, so a fresh npm install floods fsevents and
            # watchman drops events, breaking Metro module resolution in RN/Expo
            # worktrees (see issue #41). A sibling becomes its own watch root with
            # a clean crawl. Any override should stay non-nested for the same
            # reason. Only affects newly-created worktrees; existing keep their path.
            worktree-path = "${cfg.worktrunk.worktreePath}"

            # The tmux post-switch hook owns navigation (select-window or switch-client),
            # so skip cd'ing the parent shell — otherwise it ends up pwd'd at the
            # worktree behind a different session's window. Hooks still fire normally.
            [switch]
            cd = false

            [post-switch]
            tmux = """
            [ -z "$TMUX" ] && exit 0
            # Agents drive their own windows (dispatch / in-session wt). Skip
            # so we don't stack a bare sibling (Claude: $CLAUDECODE; Cursor: $CURSOR_AGENT).
            [ -n "$CLAUDECODE" ] && exit 0
            [ -n "$CURSOR_AGENT" ] && exit 0
            # display-message resolves against the attached client's ACTIVE
            # window unless pinned to the invoking pane — and wt's pane often
            # isn't the active one at hook time (long checkout, multi-client,
            # focus moved). $TMUX_PANE is set for every pane once $TMUX is.
            CUR_SESSION=$(tmux display-message -t "$TMUX_PANE" -p '#{session_name}')
            CUR_WIN=$(tmux display-message -t "$TMUX_PANE" -p '#{window_index}')
            # Primary + fallback in one query: the matcher ranks windows by the
            # @worktree tag *corroborated by a pane's cwd*, so a tag that outlived
            # the cd that earned it can no longer win (#199); it also unsets any
            # tag it proves false. Output is "<session>\t<window>\t<window_id>",
            # empty when nothing matches.
            MATCH=$(${tmuxConfig.script.tmux-worktree-match}/bin/tmux-worktree-match "{{ worktree_path }}" "$CUR_SESSION" "$CUR_WIN")
            if [ -n "$MATCH" ]; then
              SESS=$(printf '%s' "$MATCH" | cut -f1)
              WIN=$(printf '%s' "$MATCH" | cut -f2)
              if [ "$SESS" = "$CUR_SESSION" ]; then
                tmux select-window -t "$SESS:$WIN"
              else
                tmux switch-client -t "$SESS:$WIN"
              fi
              # Tagging (incl. auto-tag of matched-by-path windows, so the next
              # call hits the primary signal) is done by the reconcile tail below.
              # The matcher's #{window_id} sidesteps numeric-session ambiguity.
              STAMP_TARGET=$(printf '%s' "$MATCH" | cut -f3)
            else
              # Take over the current window when it's a single pane whose
              # worktree is already shown by another window in THIS session —
              # repurpose the redundant window instead of stacking up a new
              # one. Same-session only: another session showing the same path
              # doesn't make this session's window redundant.
              CUR_PANES=$(tmux display-message -t "$TMUX_PANE" -p '#{window_panes}')
              CUR_WT=$(tmux display-message -t "$TMUX_PANE" -p '#{@worktree}')
              CUR_PATH=$(tmux display-message -t "$TMUX_PANE" -p '#{pane_current_path}')
              CUR_CMD=$(tmux display-message -t "$TMUX_PANE" -p '#{pane_current_command}')
              DUP=""
              # send-keys below types into the active pane, so only take over
              # when wt itself is what's running there (or a bare shell) —
              # never when wt was invoked via a popup/binding over e.g. nvim.
              case "$CUR_CMD" in
                wt | fish | bash | zsh | sh)
                  if [ "$CUR_PANES" = "1" ]; then
                    DUP=$(tmux list-windows -t "$CUR_SESSION" -F '#{window_index}|#{@worktree}|#{pane_current_path}' \
                      | awk -F'|' -v cw="$CUR_WIN" -v cwt="$CUR_WT" -v cp="$CUR_PATH" '
                          NF != 3 { next }
                          $1 == cw { next }
                          (cwt != "" && $2 == cwt) || $3 == cp { print "dup"; exit }')
                  fi
                  ;;
              esac
              if [ -n "$DUP" ]; then
                CUR_TARGET=$(tmux display-message -t "$TMUX_PANE" -p '#{session_id}:#{window_index}')
                # Queued in the pty; the shell reads it once wt exits. Tagging is
                # done by the reconcile tail below (explicit mode, so it doesn't
                # depend on this still-pending cd landing).
                tmux send-keys -t "$CUR_TARGET" "cd '{{ worktree_path }}'" Enter
                STAMP_TARGET="$CUR_TARGET"
              else
                # A bare `new-window` is enough: its after-new-window hook fires
                # tmux-reconcile-window, which tags the new window from its cwd
                # ({{ worktree_path }}, via -c). No STAMP_TARGET — the reconcile
                # tail is only for the reused-window branches above.
                tmux new-window -a -t "$CUR_SESSION" -c "{{ worktree_path }}"
              fi
            fi
            # Single source of truth for tagging the reused window: sets
            # @worktree/@branch/@git_root and (with enrich) kicks the issue stamp.
            # Unconditional — navigation needs the tags even with enrich off;
            # foreground so the tag is set before any rapid re-switch.
            if [ -n "''${STAMP_TARGET:-}" ]; then
              ${tmuxConfig.script.tmux-reconcile-window}/bin/tmux-reconcile-window "$STAMP_TARGET" "{{ worktree_path }}" "{{ branch | sanitize }}" >/dev/null 2>&1
            fi
            """
            zoxide = """
            command -v zoxide >/dev/null 2>&1 && zoxide add "{{ worktree_path }}"
            """
            devshell = """
            # Materialize the flake devShell once in a freshly-created worktree so
            # its shellHook side effects exist before the first commit — notably the
            # git-hooks.nix .pre-commit-config.yaml symlink, which is gitignored and
            # absent until a devshell loads. Without this, commits from a non-direnv
            # shell (agents, scripts) fail with "No .pre-commit-config.yaml file".
            # No agent-env guard: the agent case is the one this most helps.
            wt="{{ worktree_path }}"
            [ -f "$wt/flake.nix" ] || exit 0
            [ -e "$wt/.pre-commit-config.yaml" ] && exit 0
            command -v nix >/dev/null 2>&1 || exit 0
            hook=$(git -C "$wt" rev-parse --git-path hooks/pre-commit 2>/dev/null) || exit 0
            [ -f "$hook" ] && grep -q pre-commit "$hook" 2>/dev/null || exit 0
            ( cd "$wt" && nix develop --quiet --command true ) >/dev/null 2>&1 || true
            """

            [post-remove]
            tmux = """
            [ -z "$TMUX" ] && exit 0
            [ -n "$CLAUDECODE" ] && exit 0
            [ -n "$CURSOR_AGENT" ] && exit 0
            SESSION=$(tmux display-message -t "$TMUX_PANE" -p '#{session_name}')
            WIN=$(tmux list-windows -t "$SESSION" -F '#{window_index}|#{@worktree}|#{pane_current_path}' \
              | awk -F'|' 'NF != 3 { next } $2 == "{{ worktree_path }}" || $3 == "{{ worktree_path }}" { print $1; exit }')
            [ -n "$WIN" ] && tmux kill-window -t "$SESSION:$WIN" 2>/dev/null || true
            """
          '';
        };

        # Stable config path so theme-toggle and prefix+r can source it.
        # The actual config is in /nix/store; this symlink always points to the latest.
        "tmux/tmux.conf".source = tmuxConfig.tmuxConf;
      };
    }
    # Never restart on switch — Type=forking with no KillMode override means
    # stopping the unit kills the tmux server's whole cgroup, taking every
    # session with it. Byte-stability of the unit can't be relied on to prevent
    # that: TERMINFO embeds the terminal's store path, which churns on unrelated
    # nixpkgs rebuilds.
    (lib.mkIf isLinux {
      systemd.user = {
        services = {
          tmux-startup = lib.mkIf cfg.startupSession.enable {
            Unit =
              {
                Description = "Start tmux server on login";
                X-SwitchMethod = "keep-old";
              }
              // lib.optionalAttrs (!cfg.startupSession.headless) {
                After = ["graphical-session.target"];
                Wants = ["graphical-session.target"];
              };

            Service = {
              Type = "forking";
              ExecStartPre = "${lib.getExe' pkgs.systemd "systemctl"} --user import-environment ${lib.concatStringsSep " " startupImportVars}";
              ExecStart = "${tmux-startup-script}";
              RemainAfterExit = true;
              TimeoutStopSec = "5s";
              Environment =
                [
                  "COLORTERM=${cfg.startupSession.terminal.colorterm}"
                  "TERM=${effectiveTerm}"
                  "TMUX_TMPDIR=%t"
                ]
                ++ lib.optionals (effectiveTermProgram != "") [
                  "TERM_PROGRAM=${effectiveTermProgram}"
                ]
                ++ lib.optionals (effectiveTerminfoPath != null) [
                  "TERMINFO=${effectiveTerminfoPath}"
                ];
            };

            Install = {
              WantedBy = [
                (
                  if cfg.startupSession.headless
                  then "default.target"
                  else "graphical-session.target"
                )
              ];
            };
          };

          # Weekly GC sweeps orphaned scrollback files (panes whose snapshot row
          # was already pruned). Cheap to run; safe to skip on missed firings.
          # Periodic snapshot saves now come from tmux-remux's own tmux 3.8
          # monitor hook (see tmuxRemuxWireScript above), not a timer unit.
          tmux-og-remux-gc = lib.mkIf persistEnabled {
            Unit.Description = "tmux-remux garbage collection";
            Service = {
              Type = "oneshot";
              ExecStart = "${cfg.persist.package}/bin/tmux-remux gc";
            };
          };
        };

        timers = {
          tmux-og-remux-gc = lib.mkIf persistEnabled {
            Unit.Description = "tmux-remux GC (orphan scrollback files)";
            Timer = {
              OnCalendar = "weekly";
              Unit = "tmux-og-remux-gc.service";
            };
            Install.WantedBy = ["timers.target"];
          };
        };
      };
    })
    # Darwin: launchd agents mirroring the systemd units. tmux uses its default
    # /tmp/tmux-$UID socket here (no $XDG_RUNTIME_DIR), so we omit the
    # TMUX_TMPDIR / %t handling the Linux units need.
    (lib.mkIf isDarwin {
      launchd.agents =
        lib.optionalAttrs cfg.startupSession.enable {
          tmux-startup = {
            enable = true;
            config = {
              ProgramArguments = ["${tmux-startup-script}"];
              RunAtLoad = true;
              EnvironmentVariables =
                {
                  COLORTERM = cfg.startupSession.terminal.colorterm;
                  TERM = effectiveTerm;
                }
                // lib.optionalAttrs (effectiveTermProgram != "") {
                  TERM_PROGRAM = effectiveTermProgram;
                }
                // lib.optionalAttrs (effectiveTerminfoPath != null) {
                  TERMINFO = effectiveTerminfoPath;
                };
              StandardOutPath = "/tmp/og-startup.log";
              StandardErrorPath = "/tmp/og-startup.log";
            };
          };
        }
        // lib.optionalAttrs persistEnabled {
          # Weekly GC of orphaned scrollback files. The periodic save agent is
          # gone (#344) — see the systemd block above for why.
          tmux-og-remux-gc = {
            enable = true;
            config = {
              ProgramArguments = ["${cfg.persist.package}/bin/tmux-remux" "gc"];
              StartCalendarInterval = [{Weekday = 0;}];
            };
          };
        };
    })
  ]);
}
