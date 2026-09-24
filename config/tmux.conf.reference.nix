# Frozen reference copy of the tmux.conf text, lifted verbatim out of
# config/tmux.conf.nix so that file no longer carries the config it generates.
# It is the oracle the extraction check diffs `og generate`'s output against, so
# THE TWO FILES MUST BE EDITED TOGETHER: a tmux.conf change lands here and in
# config/tmux.conf.tmpl, or the check goes red on a difference that is not a bug.
#
# An earlier version of this header said the file would be deleted once the
# tmux-og rename landed. It was not: this is the only byte-identity oracle the
# render has, and removing the check is expressly not wanted. The file survives,
# and the two-file edit-together rule above survives with it.
#
# `sections` is the ordered cut the check's hybrid render and the template lift
# work through; its four boundaries are the ones the freeze was verified at.
{
  pkgs,
  lib,
  # Module options this text interpolates directly.
  prefix,
  copyModeLineNumbers,
  focusFollowsMouse,
  defaultShell,
  terminalTerm,
  sixelTerminals,
  zoxideExclude,
  pickerListRatio,
  pickerLayout,
  remoteBridgeHosts,
  remoteAuthPersistSeconds,
  splashEnable,
  enrichEnable,
  notifyEnable,
  agentUsageEnable,
  agentUsageMonthlyThreshold,
  aiNamingEnable,
  resumeClaudeEnable,
  resumeCarouselEnable,
  extraConfText,
  # Optional packages, null when not wired in.
  carousel-toggle,
  prdash,
  # Built by config/tmux.conf.nix and passed in rather than rebuilt here.
  processIcons,
  script,
  picker-bridge-ctl-bin,
  picker-card-bin,
  picker-splash-bin,
  picker-statusline-bin,
  picker-session-res-bin,
  # The two enrich icon dialects (I8). Derived by the caller, which also feeds
  # the raw set to the lib-enrich substitution that does not move.
  enrichIconsDoubled,
  enrichIconsRaw,
  # tmux-remux wire script, or null when persistence is off. The block it emits
  # reaches the module through extraConfText until the caller edit lands.
  persistWireScript,
}: let
  # --- Nerd font icons (edit these if they don't render in your terminal) ---
  icons = {
    session = "";
    branch = "";
    dir = "";
    remote = "";
    # Session-picker column headers. Verified present in the pinned nerd font:
    # md-server, md-apps, md-chip, md-memory.
    host = "";
    procs = "󰀻";
    cpu = "";
    mem = "";
    window-last = "󰖰";
    window-current = "󰖯";
    window-zoom = "󰁌";
    window-mark = "󰃀";
    window-silent = "󰂛";
    window-activity = "󱅫";
    window-bell = "󰂞";
  };

  aiNamingFlag =
    if aiNamingEnable
    then "1"
    else "0";

  resumeClaudeFlag =
    if resumeClaudeEnable
    then "on"
    else "off";

  resumeCarouselFlag =
    if resumeCarouselEnable
    then "on"
    else "off";

  # --- Custom plugins (pinned versions) ---
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
    # popup-* options are gone upstream (tmux 34cd5da4); -q keeps them on a
    # resident older server and silent on the pinned one.
    postPatch = ''
      substituteInPlace catppuccin_tmux.conf \
        --replace-fail 'set -gF popup-style' 'set -gqF popup-style' \
        --replace-fail 'set -gF popup-border-style' 'set -gqF popup-border-style'
    '';
  };

  # === Remote bridge: structural input gate (M2.3) ===
  # Inside a mirror window a structural gesture must act on the REMOTE, not on
  # the local mirror. bridgeGate is the format that says "this is a live mirror
  # pane": @bridge_win (window) and @bridge_pane (pane) are stamped by the bridge
  # daemon, @bridge_sock (session) is where it listens. Requiring both the window
  # tag and the pane carrier means a pane the daemon does not own falls through to
  # the normal local action instead of failing at the daemon.
  #
  # `if-shell -F` expands a format and picks a branch without forking, so a
  # non-bridge window pays one format expansion per gated keypress and spawns no
  # process. Every gated bind keeps its existing local behavior verbatim in the
  # else branch — a regression in normal keybind behavior would defeat the whole
  # point of the bridge (zero blast radius on the human's live session).
  bridgeGate = "#{&&:#{@bridge_win},#{@bridge_pane}}";
  # @bridge_sock is always in --sock= form, never word-initial; bridgeGate guarantees it is non-empty.
  bridgeCtl = "${picker-bridge-ctl-bin} --display-error=#{q:client_name} --sock=#{q:@bridge_sock}";

  # A mirror window's own @crew_*/@pr_* describe the launcher's repo; the daemon
  # ships the remote window's under @bridge_*. These are read live at render
  # time, so the choice has to be a format conditional. Built per option name,
  # not per site — each appears more than once in the window format. The commas
  # inside are deliberately NOT '#,'-escaped: format_expand resolves the
  # conditional before format_draw parses '#[…]', and its argument splitter
  # tracks '#{'/'}' nesting, so they are already protected.
  bridgeOpt = name: "#{?#{@bridge_win},#{@bridge_${name}},#{@${name}}}";

  inherit (pkgs) tmuxPlugins;

  # terminal-features line for the outer terminal, derived from its TERM string.
  # Pattern uses a wildcard suffix to match version variants (e.g. "xterm-ghostty*").
  terminalConfig =
    lib.optionalString (terminalTerm != null)
    "set -as terminal-features '${terminalTerm}*:RGB:extkeys'\n    "
    + lib.concatMapStrings
    (term: "set -as terminal-features '${term}*:sixel'\n    ")
    sixelTerminals;

  # default-shell line, emitted only when a shell path is configured.
  defaultShellConfig =
    lib.optionalString (defaultShell != null)
    "set -g default-shell ${defaultShell}\n    ";

  # prefix+I bind for the agent-carousel image gallery, emitted only when the
  # toggle package is wired in (carousel-toggle != null). Inside a mirror window
  # the carousel must open on the REMOTE (its manifest and its images live
  # there) via the carousel ctl verb; the local branch is the existing bind,
  # verbatim — tmux's run-shell does not export TMUX_PANE, so the toggle can't
  # key the manifest to the pressing pane and reports "no images yet for this
  # pane". Inject it via #{pane_id}, which tmux format-expands in the command
  # string before exec.
  carouselBind =
    lib.optionalString (carousel-toggle != null)
    "bind -N 'Toggle image carousel' I if-shell -F '${bridgeGate}' { run-shell \"${bridgeCtl} carousel #{q:@bridge_pane}\" } { run-shell 'TMUX_PANE=#{q:pane_id} ${carousel-toggle}/bin/tmux-claude-images' }";

  # Float geometry, declared once per shape. tmux resolves the percentages into
  # absolute cells at creation and never revisits them (layout_resize skips
  # floating cells), so a float outlives the client size it was made for —
  # @float_geom carries the percentages forward for tmux-float-refit to reassert
  # on window-resized. Both come from one source here so they cannot drift.
  # -A is a Z-ORDER flag — it keeps a float visible above a zoomed pane — and
  # not attach-if-exists: new-pane has no such mode, so every press creates a
  # pane and the reuse of an already-open float is explicit, in floatReuse
  # (#679). String-form if-shell defers parsing so an older server never sees
  # the unknown flag at source (#407).
  #
  # remain-on-exit is pinned off on the pane because a mirror window sets it on
  # (#547) and pane options inherit from the window's, so a float inside a
  # mirror outlived its command as a dead pane nothing reaps — healDeadRenderers
  # only touches panes carrying @bridge_pane (#587).
  mkFloat = w: h: x: y: let
    base = "-x ${w} -y ${h} -X ${x} -Y ${y} -B heavy";
  in {
    flags = "${base} -A";
    flagsNoA = base;
    stamp = "set -p @float_geom '${w} ${h} ${x} ${y}' \\; set -p remain-on-exit off";
  };
  # String-form if-shell — brace blocks parse every branch at source time.
  floatNewPaneGuard = float: prefix: suffix: let
    esc = builtins.replaceStrings ["\""] ["\\\""];
    mk = flags: esc "new-pane ${prefix}${flags} ${suffix} \\; ${float.stamp}";
  in "if-shell \"tmux list-commands new-pane | grep -q -- -A\" \"${mk float.flags}\" \"${mk float.flagsNoA}\"";
  floatBind = key: note: float: prefix: suffix: "bind-key -N '${note}' ${key} ${floatNewPaneGuard float prefix suffix}";

  # floatRegister/floatLookup/floatReuse mirror generator/render/keys.go byte for
  # byte — this file is the extraction oracle, so the two are edited together.
  #
  # floatRegister is the window option a bridged tool press hands the float's
  # pane id to its own focus branch through. Not a second lookup key: the value
  # is written by the branch that reads it, in the same command list, and that
  # branch is only reached once floatLookup has matched. It exists because a pane
  # loop nested inside a run-shell argument is a shell-injection shape
  # tests/conf-shell-quoting.bats rejects — `#{q:<option>}` is the one legal way
  # to hand an id to a shell.
  floatRegister = tool: "@og_float_target_${tool}";
  floatLookup = tool: "#{P:#{?#{&&:#{==:#{@pane_label},${tool}},#{pane_floating_flag}},#{pane_id},}}";
  floatReuse = tool: guard: "if-shell -F \"${floatLookup tool}\" { set -wF ${floatRegister tool} \"${floatLookup tool}\" ; run-shell \"tmux select-pane -t #{q:${floatRegister tool}}\" } { ${guard} }";
  floatFull = mkFloat "90%" "90%" "5%" "5%";
  floatShort = mkFloat "90%" "85%" "5%" "8%";
  # The enrich card sizes to its contents, not to the client; only its offsets
  # are percentages, and those are what walk off a shrinking window.
  floatCard = mkFloat "64" "18" "20%" "15%";

  # Inside a mirror window #{pane_current_path} expands on the renderer pane —
  # the daemon's cwd, not the remote worktree on screen — so the bridged branch
  # hands the tool to the ctl `tool` verb.
  #
  # @bridge_dir rides along because the remote cannot resolve the cwd either: a
  # -c format expands against the client's current pane, not the -t target, so
  # the remote leg would open the tool in whichever window the remote is on
  # (#643). It is #{qs:}, not #{q:} — the value is a path, and run-shell hands it
  # to a shell that would otherwise split it on a space. An unset option quotes
  # as an empty argument, which the verb reads as "no cwd".
  bridgedFloatTool = key: note: tool: float: prefix: suffix: "bind-key -N '${note}' ${key} if-shell -F '${bridgeGate}' { run-shell \"${bridgeCtl} tool #{q:@bridge_pane} ${tool} #{qs:@bridge_dir}\" } { ${floatReuse tool (floatNewPaneGuard float prefix suffix)} }";

  # prdash PR dashboard (prefix+p), scoped to the pane's repo. `enter` opens a git
  # worktree: prdash execs `wt switch` itself as it exits, so the tmux window
  # switches when the pane closes.
  #
  # -X/-Y are required: without them tmux cascades each new float down-right,
  # and the cascade counter survives kill-pane. @pane_label → mauve border title.
  prdashBind =
    lib.optionalString (prdash != null)
    (bridgedFloatTool "p" "Open PR dashboard" "prdash" floatShort "-c '#{pane_current_path}' "
      "${prdash}/bin/prdash \\; set -p @pane_label prdash");

  # In kitty-pane mode (AEYE_HOST=kitty) the carousel is a kitty split that doesn't
  # know about tmux focus, so reconcile it whenever the on-screen window changes —
  # stashing carousels for off-screen panes and restoring the visible one. Indexed
  # ([60]) to coexist with the reflow (index 0) and splash ([50]) hooks on the same
  # events. -b so focus changes never block; --reconcile self-gates to kitty mode,
  # so it's a fast no-op for tmux-split users.
  carouselHooks = lib.optionalString (carousel-toggle != null) ''
    set-hook -g client-session-changed[60] 'run-shell -b "${carousel-toggle}/bin/tmux-claude-images --reconcile"'
    set-hook -g session-window-changed[60] 'run-shell -b "${carousel-toggle}/bin/tmux-claude-images --reconcile"'
    set-hook -g client-attached[60]        'run-shell -b "${carousel-toggle}/bin/tmux-claude-images --reconcile"'
  '';

  # --- Plugin config options (set before run-shell) ---
  pluginConfigs = ''
    # catppuccin theme
    # Detect theme from state file on first load (theme-toggle sets flavor before re-source)
    # The x-prefix keeps @catppuccin_flavor non-word-initial and safe when empty.
    if-shell '[ x#{q:@catppuccin_flavor} = x ]' \
      'if-shell "grep -q light \"$HOME/.local/state/theme-state.json\" 2>/dev/null" \
        "set -g @catppuccin_flavor latte" \
        "set -g @catppuccin_flavor mocha"'
    set -g @catppuccin_status_background 'none'
    set -g @catppuccin_window_status_style 'none'
    set -g @catppuccin_window_flags 'icon'
    set -g @catppuccin_window_flags_icon_last " ${icons.window-last}"
    set -g @catppuccin_window_flags_icon_current " ${icons.window-current}"
    set -g @catppuccin_window_flags_icon_zoom " ${icons.window-zoom}"
    set -g @catppuccin_window_flags_icon_mark " ${icons.window-mark}"
    set -g @catppuccin_window_flags_icon_silent " ${icons.window-silent}"
    set -g @catppuccin_window_flags_icon_activity " ${icons.window-activity}"
    set -g @catppuccin_window_flags_icon_bell " ${icons.window-bell}"
    set -g @catppuccin_pane_status_enabled 'off'
    set -g @catppuccin_pane_border_status 'off'

  '';

  # --- Plugin run-shell loading ---
  # Order matters: theme first, then others
  pluginRunShells = ''
    run-shell "${pkgs.bash}/bin/bash ${catppuccin}/share/tmux-plugins/catppuccin/catppuccin.tmux"
    run-shell ${tmuxPlugins.better-mouse-mode}/share/tmux-plugins/better-mouse-mode/scroll_copy_mode.tmux
    run-shell ${tmuxPlugins.vim-tmux-navigator}/share/tmux-plugins/vim-tmux-navigator/vim-tmux-navigator.tmux
    run-shell ${tmuxPlugins.tmux-fzf}/share/tmux-plugins/tmux-fzf/main.tmux
  '';

  # Persist (tmux-remux) tmux.conf snippet. Empty string when disabled.
  persistConf =
    if persistWireScript == null
    then ""
    else ''

      # === tmux-remux (Phase 2a, opt-in via programs.tmux-og.persist) ===
      run-shell "${persistWireScript} #{q:version}"
    '';

  # --- Generated tmux.conf, in the four sections the freeze was cut at ---
  baseText = ''
    # === Base Settings ===
    set -g default-terminal "tmux-256color"
    ${defaultShellConfig}set -g history-limit 1500000
    set -g base-index 1
    setw -g pane-base-index 1

    # === Plugin Configs (must be set before run-shell) ===
    ${pluginConfigs}

    # === Plugin Loading ===
    ${pluginRunShells}

    # === Main Config ===
    set -g mouse on
    set -g focus-follows-mouse ${
      if focusFollowsMouse
      then "on"
      else "off"
    }
    set -g window-size latest
    set -g aggressive-resize on
    set-option -g renumber-window on
    set -g focus-events on
    # Deliberately on, not all: `all` lets a program in any pane of an attached
    # session write escape sequences straight to the terminal while invisible,
    # and a mirror pane replays a remote host's bytes verbatim — global `all`
    # would widen a remote's reach from "while you are looking at that window"
    # to "whenever any client is attached". Mirror panes, which need `all` to
    # keep their kitty image stores, get it per pane from the daemon instead
    # (markRendererPane, #464).
    set -g allow-passthrough on
    set -g visual-activity off

    # update-environment: clear stale entries first so source-file is idempotent.
    # Without this, repeated config reloads duplicate TERM, KITTY_LISTEN_ON, etc.
    set -gu update-environment
    # Preserve terminal environment variables
    set -ga update-environment TERM
    set -ga update-environment TERM_PROGRAM
    set -ga update-environment COLORTERM
    set -ga update-environment TERMINFO
    set -ga update-environment TERMINFO_DIRS
    # kitty remote-control socket, so `kitty @` (carousel reconcile) reaches the
    # enclosing kitty from panes attached after the server started outside it.
    set -ga update-environment KITTY_LISTEN_ON
    # kitty-pane carousel opt-in. The reconcile hook runs in the session env, not
    # the user's interactive shell, so thread AEYE_HOST in on attach — otherwise
    # the carousel never follows tmux focus and the launcher degrades to a split.
    set -ga update-environment AEYE_HOST

    # Timing
    set -s escape-time 0
    set -g repeat-time 300
    set -g initial-repeat-time 600

    # Extended keyboard + clipboard
    set -g extended-keys on
    set -g extended-keys-format csi-u
    ${terminalConfig}set -as terminal-features '*:hyperlinks'
    set -as terminal-features 'xterm-kitty*:progressbar'
    set -s set-clipboard on
    # Default `buffer` answers an app's OSC 52 read from this server's own newest
    # paste buffer; on a mirrored host that is stale remote content and the query
    # never crosses the bridge.
    set -s get-clipboard request
    set -s copy-command '${
      if pkgs.stdenv.hostPlatform.isDarwin
      then "pbcopy"
      else "wl-copy"
    }'

    # Prefix (configurable; default backtick)
    unbind C-b
    set-option -g prefix ${prefix}
    bind -N 'Send the prefix key to the pane' ${prefix} send-prefix

  '';

  keysText = ''
    # Config reload
    bind -N 'Reload tmux config' r source-file ~/.config/tmux/tmux.conf \; display "Config reloaded!"

    # which-key popup (#629): every bind, grouped by key table and note,
    # fuzzy-filterable; Enter replays the pick against this pane. ctrl+r inside
    # the popup toggles the old sorted-by-key raw listing (see whichKeyRawFormat,
    # picker/whichkey.go) in place.
    bind-key -N 'Show keybindings' ? run-shell '${script.tmux-which-key}/bin/tmux-which-key #{?client_name,--client #{q:client_name},} --origin-pane #{q:pane_id}'

    # Vi copy mode
    setw -g mode-keys vi
    set -g status-keys vi
    unbind-key -T copy-mode-vi v
    bind-key -N 'Begin selection' -T copy-mode-vi v send -X begin-selection
    bind-key -N 'Toggle rectangle selection' -T copy-mode-vi C-v send -X rectangle-toggle
    bind-key -N 'Copy selection' -T copy-mode-vi 'y' send -X copy-pipe

    # Copy mode styling (tmux 3.6+)
    set -g copy-mode-position-style "bg=#{@thm_surface_0},fg=#{@thm_mauve}"
    set -g copy-mode-selection-style "bg=#{@thm_mauve},fg=#{@thm_bg}"
    # Line numbers in copy mode (tmux 3.7+). Configurable — matter of taste.
    set -g copy-mode-line-numbers ${copyModeLineNumbers}

    bind -N 'Clear screen' -n M-l send-keys C-l

    # Shift+Enter: process-aware newline for pi / Claude Code / Amp / OpenCode.
    # pi binds alt+enter to follow-up queueing, so it wants the raw CSI-u sequence
    # its own newline binding reads; every other CLI gets its own newline key.
    bind -N 'Insert newline in agent CLI' -n S-Enter if-shell "ps -o comm= -t #{q:pane_tty} | grep -qE '^pi$'" { send-keys -H 1b 5b 31 33 3b 32 75 } { if-shell "ps -o comm= -t #{q:pane_tty} | grep -qE '^(amp|bun|opencode)$'" { send-keys \\ Enter } { send-keys M-Enter } }

    # Pane splitting (| and _)
    unbind %
    bind -N 'Split pane horizontally' | if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} split-h #{q:@bridge_pane}" } { split-window -h -c "#{pane_current_path}" }
    unbind '"'
    bind -N 'Split pane vertically' _ if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} split-v #{q:@bridge_pane}" } { split-window -v -c "#{pane_current_path}" }
    bind -N 'Create new window' c if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} new-window #{q:@bridge_pane}" } { if-shell -F '#{m:scratch-*,#{session_name}}' 'display-message "scratchpad: new windows disabled"' 'new-window -c "#{pane_current_path}"' }
    # Zoom, like every other structural gesture, happens on the remote in a
    # mirror: zooming the local renderer pane grows it without growing the
    # remote pane, so the remote program keeps rendering at its old size and
    # the rows gained are dead space.
    bind -N 'Toggle pane zoom' z if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} zoom #{q:@bridge_pane}" } { resize-pane -Z }
    # client_name is tty-derived (never ~-initial); conditionals/--display-error= make empty values harmless.
    # #{q:} and NOT \"...\", as at client-attached[50]. Bare #{q:client_name}
    # would be zero words when no client exists, sliding the session name into
    # --client's slot; #{?...} emits the flag and its value together or neither,
    # which is what this script's `$1 == --client` parsing needs (no = form).
    bind -N 'Open scratchpad session' S run-shell '${script.tmux-scratchpad}/bin/tmux-scratchpad #{?client_name,--client #{q:client_name},} #{qs:session_name}'
    ${carouselBind}

    # Yank pane's current working directory to system clipboard
    bind -N 'Copy pane path to clipboard' Y run-shell 'tmux display-message -p #{qs:pane_current_path} | wl-copy'

    # Resize panes. In a mirror window the resize lands on the remote pane and the
    # mirror re-fits from the remote's new layout; -r still repeats.
    bind -N 'Resize pane up' -r -T prefix M-Up    if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} resize #{q:@bridge_pane} U 5" } { resize-pane -U 5 }
    bind -N 'Resize pane down' -r -T prefix M-Down  if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} resize #{q:@bridge_pane} D 5" } { resize-pane -D 5 }
    bind -N 'Resize pane left' -r -T prefix M-Left  if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} resize #{q:@bridge_pane} L 5" } { resize-pane -L 5 }
    bind -N 'Resize pane right' -r -T prefix M-Right if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} resize #{q:@bridge_pane} R 5" } { resize-pane -R 5 }

    # === Remote bridge: keys this config did NOT previously bind ===
    # Gating them means the config now owns them, so each else-branch reproduces
    # next-3.8's default verbatim, -N note included (the note feeds which-key).
    # The rename prompt seeds from @window_bridge_name, not #W: on a mirror window
    # #W is the label reflow derived, while the option holds the remote's own name.
    # That seed is remote-derived, so the prompt result is untrusted and reaches
    # the shell as a run-shell ARGUMENT (%1) referenced #{qs:1}, never as text
    # spliced into the command string, where run-shell would format-expand it and
    # a #(...) in the name would run before sh saw the command at all.
    #   - #{qs:1}, not #{q:1}: format_quote_shell omits ~, { and } from its escape
    #     set and never wraps, so the value lands as a bare shell word — measured,
    #     a remote window named ~/src was delivered as the LOCAL $HOME-expanded
    #     path, and x{a,b} split into two argv words. #{qs:1} POSIX-single-quotes.
    #   - %1, not '%%': tmux's template substitution leaves %N unescaped, so
    #     #{qs:} is the only quoting layer; '%%' would double-escape.
    #   - The { ... } block form is what makes an unescaped %1 safe: the block is
    #     never re-lexed, whereas a string command argument would re-parse it.
    #   - #{qs:1} must stay in a BARE word position: it emits its own surrounding
    #     single quotes, so wrapping it ('#{qs:1}') would have the modifier's
    #     opening quote close the outer one and defeat the escaping. The
    #     conf-shell-quoting scanner tracks no sub-quoting context and cannot
    #     catch that.
    bind-key -N 'Rename current window' , if-shell -F '${bridgeGate}' { command-prompt -I'#{@window_bridge_name}' { run-shell "${bridgeCtl} rename #{q:@bridge_pane} #{qs:1}" %1 } } { command-prompt -I'#W' { rename-window -- '%%' ; set-window-option @window_manual_name 1 } }
    bind-key -N 'Swap the active pane with the pane above' '{' if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} swap #{q:@bridge_pane} U" } { swap-pane -U }
    bind-key -N 'Swap the active pane with the pane below' '}' if-shell -F '${bridgeGate}' { run-shell "${bridgeCtl} swap #{q:@bridge_pane} D" } { swap-pane -D }

    # Alt-shift window navigation: H/L step within a row, J/K move row-to-row in
    # the reflowed multi-line window grid (no-op when there is no row that way).
    bind -N 'Previous window' -n M-H previous-window
    bind -N 'Next window' -n M-L next-window
    bind -N 'Move down a row in the window grid' -n M-J run-shell '${script.tmux-window-nav}/bin/tmux-window-nav down #{qs:session_name} #{q:window_index} #{q:@window_per}'
    bind -N 'Move up a row in the window grid' -n M-K run-shell '${script.tmux-window-nav}/bin/tmux-window-nav up #{qs:session_name} #{q:window_index} #{q:@window_per}'

    # Session/window pickers (wrappers pre-compute agent status), plus the
    # tiled wall (W) — the same window list rendered as live preview tiles.
    bind -N 'Open session picker' s run-shell '${script.tmux-session-picker}/bin/tmux-session-picker #{?client_name,--client #{q:client_name},} --current #{qs:session_name}'
    bind -N 'Open window picker' w run-shell '${script.tmux-window-picker}/bin/tmux-window-picker #{?client_name,--client #{q:client_name},}'
    bind -N 'Open window picker (agent view)' a run-shell '${script.tmux-window-picker}/bin/tmux-window-picker #{?client_name,--client #{q:client_name},} --agent'
    bind -N 'Open window wall' W run-shell '${script.tmux-window-wall}/bin/tmux-window-wall #{?client_name,--client #{q:client_name},}'
    # Click session name in status bar (the #[range=left] marker in the Go
    # statusline) to open the session picker.
    bind -N 'Open session picker' -T root MouseDown1StatusLeft run-shell '${script.tmux-session-picker}/bin/tmux-session-picker #{?client_name,--client #{q:client_name},} --current #{qs:session_name}'

    ${lib.optionalString splashEnable ''
      # Summon the welcome splash on demand (bypasses the once-per-session gate;
      # no auto-timeout — dismiss with any key).
      bind -N 'Show welcome splash' C-Space display-popup -E -B -w 100% -h 100% '${picker-splash-bin} --no-timeout'
    ''}

    ${lib.optionalString enrichEnable ''
      # === Issue/PR enrichment ===
      # prefix + i opens the enrich card in a floating pane, window-scoped like
      # yazi/prdash below (it reads the *current window's* @issue_*/@pr_* — or
      # their @bridge_* copies in a mirror — so window scope is correct). Icons
      # use the RAW set: the card's stdout is not re-parsed as a tmux format, so
      # ##-escaped glyphs must not be passed.
      # Stays a plain floatBind — never bridgeGate'd, never bridgedFloatTool —
      # because the card has nothing to run on the remote: it only reads local
      # window options (which, after the bridge label shipper, already carry the
      # remote's truth). Launching it on the remote would put [o]/[p]'s
      # xdg-open on a headless machine, breaking URL opening outright. It still
      # gets a ctl handle (--bridge-ctl-bin/--bridge-sock/--bridge-pane, all
      # empty on a non-mirror window) so [r] refresh can reach the remote poller
      # without moving the launch there.
      ${floatBind "i" "Show issue/PR enrichment card" floatCard "" ''        "${picker-card-bin} \
                --target '#{session_id}:#{window_id}' \
                --pr-enrich-bin '${script.tmux-pr-enrich}/bin/tmux-pr-enrich' \
                --bridge-ctl-bin '${picker-bridge-ctl-bin}' \
                --bridge-sock '#{@bridge_sock}' --bridge-pane '#{@bridge_pane}' \
                --issue-stamp-bin '${script.tmux-issue-stamp}/bin/tmux-issue-stamp' \
                --thm-fg '#{@thm_fg}' --thm-mauve '#{@thm_mauve}' \
                --thm-red '#{@thm_red}' --thm-green '#{@thm_green}' --thm-peach '#{@thm_peach}' \
                --thm-blue '#{@thm_blue}' --thm-overlay0 '#{@thm_overlay_0}' \
                --thm-subtext0 '#{@thm_subtext_0}' \
                --icon-linear '${enrichIconsRaw.linear}' --icon-github '${enrichIconsRaw.github}' \
                --icon-pending '${enrichIconsRaw.pending}' --icon-success '${enrichIconsRaw.success}' \
                --icon-failure '${enrichIconsRaw.failure}' --icon-merged '${enrichIconsRaw.merged}' \
                --icon-closed '${enrichIconsRaw.closed}' --icon-conflict '${enrichIconsRaw.conflict}' \
                --icon-draft '${enrichIconsRaw.draft}'" \; set -p @pane_label enrich''}
    ''}

    ${lib.optionalString notifyEnable ''
      # prefix + n: notification history. This deliberately shadows tmux's
      # built-in next-window — the config already provides M-L / M-H (no prefix)
      # for next/previous window and M-J / M-K for row-to-row movement, so
      # next-window on the prefix table is dead weight. With notifications off,
      # n reverts to next-window.
      bind-key -N 'Show notification history' n display-popup -E -w 80% -h 60% '${script.og-notify-center}/bin/og-notify-center'
    ''}

    # Floating panes (window-scoped; see the yazi comment below for why floats
    # over popups — full escape-sequence passthrough, and a pane is mirrorable
    # across the remote bridge in principle where a popup can never be).
    ${bridgedFloatTool "g" "Open lazygit" "lazygit" floatFull "-c '#{pane_current_path}' " "lazygit \\; set -p @pane_label lazygit"}
    ${floatBind "b" "Open btop" floatFull "" "btop \\; set -p @pane_label btop"}
    # PATH only, unlike the binds above: falling back to a pkgs.k9s store path
    # dragged k9s + kubectl into every closure — 237 MB, its largest single
    # item — for a bind only k8s users press. Add pkgs.k9s to popupTools.
    ${floatBind "k" "Open k9s" floatFull "" ''"command -v k9s >/dev/null 2>&1 && exec k9s || { echo 'k9s not found in PATH — add pkgs.k9s to programs.tmux-og.popupTools'; read -r; }" \; set -p @pane_label k9s''}
    ${prdashBind}
    bind-key -N 'Toggle debug overlay' D run-shell '${script.og-debug}/bin/og-debug toggle'
    # yazi in a tmux 3.7 floating pane: unlike display-popup, floating panes have
    # full escape-sequence passthrough, so yazi's image preview / terminal
    # detection work. Scoped to the launching window (no window-line entry).
    ${bridgedFloatTool "y" "Open yazi file manager" "yazi" floatShort "-c '#{pane_current_path}' " "yazi \\; set -p @pane_label yazi"}

    # New session prompt
    bind -N 'Create new session' N command-prompt -p "New session name:" "new-session -s '%%'"

    # An idle shell and an idle Claude pane kill instantly; a Claude pane
    # mid-work (processing/compacting/waiting/denied) or anything else running
    # (vim, a build, a REPL) prompts first, so a reflexive prefix+x can't
    # silently take down a working pane. The guard reads the pane's claude-status
    # state and normalizes the nix makeWrapper decoration before matching shells.
    # In a mirror window the guard would be reading the wrong process — a mirror
    # pane runs the renderer, not the remote workload — so a bridge kill always
    # confirms, naming the remote pane, then kills it on the remote.
    bind-key -N 'Kill pane' x if-shell -F '${bridgeGate}' { confirm-before -p "kill remote pane #{@bridge_pane}#{?@bridge_proc, (#{@bridge_proc}),} on #{@bridge_host}? (y/n)" { run-shell "${bridgeCtl} kill-pane #{q:@bridge_pane}" } } { if-shell '${script.tmux-kill-pane-guard}/bin/tmux-kill-pane-guard #{q:pane_id} #{qs:pane_current_command}' kill-pane 'confirm-before -p "kill-pane #P (#{pane_current_command})? (y/n)" kill-pane' }
    bind-key -N 'Kill window' & if-shell -F '${bridgeGate}' { confirm-before -p "kill remote window #{@window_bridge_name}? (y/n)" { run-shell "${bridgeCtl} kill-window #{q:@bridge_pane}" } } { confirm-before -p "kill-window #W? (y/n)" kill-window }
    # detach-client is not lost in a mirror, just deferred: the detach kills the
    # mirror session, and detach-on-destroy off (below) lands the client on
    # another local session, where d is this bind's other branch.
    #
    # -b is load-bearing, not just responsiveness: the script waits for the
    # daemon's teardown to kill the mirror session, and that teardown issues its
    # kill-session through the same command queue a foreground run-shell holds.
    bind-key -N 'Detach client' d if-shell -F '${bridgeGate}' { run-shell -b "${script.og-remote-detach}/bin/og-remote-detach #{qs:session_name}" } { detach-client }
    set -g detach-on-destroy off

    # Vim-tmux navigation (respects zoom)
    # tmux substitutes $is_vim into each bind at parse time, so this string IS an
    # if-shell shell argument and #{pane_tty} is expanded before sh -c — quoted
    # like every other such site. The conf-shell-quoting guard cannot see this one:
    # it scans the emitted text, where the binds below read only "$is_vim".
    is_vim="ps -o state= -o comm= -t #{q:pane_tty} | grep -iqE '^[^TXZ ]+ +(\\S+\\/)?g?(view|l?n?vim?x?|fzf)(diff)?$'"
    bind-key -N 'Navigate left' -n C-h if-shell "$is_vim" "send-keys C-h" "run-shell 'tmux-smart-nav L left #{q:window_zoomed_flag} #{q:pane_at_left}'"
    bind-key -N 'Navigate down' -n C-j if-shell "$is_vim" "send-keys C-j" "run-shell 'tmux-smart-nav D down #{q:window_zoomed_flag} #{q:pane_at_bottom}'"
    bind-key -N 'Navigate up' -n C-k if-shell "$is_vim" "send-keys C-k" "run-shell 'tmux-smart-nav U up #{q:window_zoomed_flag} #{q:pane_at_top}'"
    bind-key -N 'Navigate right' -n C-l if-shell "$is_vim" "send-keys C-l" "run-shell 'tmux-smart-nav R right #{q:window_zoomed_flag} #{q:pane_at_right}'"

  '';

  statusText = ''
    # Window titles
    set-option -g set-titles on
    # Strip the nix makeWrapper decoration (.foo-wrapped → foo). On macOS,
    # pane_current_command resolves to the real on-disk binary, which
    # makeWrapper names `.foo-wrapped`; argv[0] can't change that (the kernel
    # reports the resolved file via proc_pidpath). Rewrite at the display layer.
    # Backslashes are doubled: tmux's config-file lexer unescapes "..." once
    # (\\1 -> \1) before the format engine sees the regex backreference.
    set-option -g set-titles-string "#S / #{s|^\\.(.*)-wrapped$|\\1|:pane_current_command}"

    # === Status Bar (Catppuccin + multi-line) ===
    set -g allow-rename off
    set -g status-position top
    set -g status-interval 1
    set -g status-style "bg=#{@thm_bg}"

    # Multi-line status bar (tmux 3.4+). The splits default past any real window
    # index so a session tmux-reflow-windows has not stamped yet still resolves
    # every row's "index <= split" test.
    set -g status 2
    set -g @window_split 999
    set -g @window_split2 999
    set -g @window_split3 999

    # Read by the CC plugin's UserPromptSubmit hook to gate the window-naming
    # nudge (programs.tmux-og.aiNaming.enable).
    set -g @ai_naming "${aiNamingFlag}"
    set -g @resume_claude "${resumeClaudeFlag}"
    set -g @resume_carousel "${resumeCarouselFlag}"

    # Icon variables
    set -g @icon_session "${icons.session}"
    set -g @icon_branch "${icons.branch}"
    set -g @icon_dir "${icons.dir}"
    set -g @icon_remote "${icons.remote}"
    set -g @icon_host "${icons.host}"
    set -g @icon_procs "${icons.procs}"
    set -g @icon_cpu "${icons.cpu}"
    set -g @icon_mem "${icons.mem}"
    set -g @picker_zoxide_exclude "${zoxideExclude}"
    set -g @picker_list_ratio "${toString pickerListRatio}"
    set -g @picker_layout "${pickerLayout}"
    set -g @remote_bridge_hosts "${remoteBridgeHosts}"
    # An option, not a PATH lookup: the picker's ^o spawns this from inside a
    # popup on the tmux server, whose PATH is frozen until a server restart, so a
    # brand-new script would resolve to nothing until then. An option repoints on
    # a config reload alone (#336).
    set -g @remote_pick_bin "${script.og-remote-picker}/bin/og-remote-picker"
    # Same, and these two are never on PATH at any point: of the remote scripts
    # only og-remote-picker reaches home.packages (remote.exposePickOnPath).
    set -g @remote_open_bin "${script.og-remote-open}/bin/og-remote-open"
    # The window picker's ^x on a mirror row sends the same ctl kill-window verb
    # prefix+& does, so it needs the binary by store path for the same reason.
    set -g @bridge_ctl_bin "${picker-bridge-ctl-bin}"
    set -g @remote_auth_bin "${script.og-remote-auth}/bin/og-remote-auth"
    set -g @remote_auth_persist "${toString remoteAuthPersistSeconds}"

    # Same reasoning, for outside consumers: an external tool that stamps
    # @crew_* on a window has to kick a reflow for the badge to render, and
    # reflow is never on PATH. The option is the only handle it can reach.
    set -g @reflow_bin "${script.tmux-reflow-windows}/bin/tmux-reflow-windows"
    ${lib.optionalString (carousel-toggle != null) ''
      # Same frozen-PATH reasoning, for the bridge's carousel ctl verb: it
      # resolves the toggle on the remote via `command -v`, which reads the
      # server environment frozen at its start — a nix switch + reload leaves
      # it launching the old generation's script until a restart (#554).
      set -g @carousel_bin "${carousel-toggle}/bin/tmux-claude-images"
    ''}

    # Line 0: Session / Branch / Dir / Claude status (left) | usage + pane (right)
    # PR badge lives on the window list only — not duplicated here.
    # The statusline #() takes only stable args (session, theme, icons); it
    # self-fetches the volatile fields (pane cmd, prefix, @issue_* …) via
    # one display-message. Passing those as args would change the expanded
    # command string every tick and reset tmux's #() output cache → a blank
    # frame that shows as a blink under load. Do not add volatile #{...} here.
    # A second, distinct hazard: a #() whose job writes no complete line leaves
    # tmux's fj->out NULL, and once that job has run >1s tmux paints the literal
    # `<'cmd' not ready>` — the whole store path — in its place (format.c:446).
    # The ticker below exists only for its side effect and prints
    # nothing, so it must lead with `echo;` to land an (empty) line
    # immediately. Left untreated this does not merely blink: refresh-client -S
    # (which claude-status-update and reflow call constantly) job_free()s the
    # in-flight job without ever reaching the completion callback that would
    # publish an empty output, so under CPU load the placeholder pins instead of
    # passing.
    # ticker per client attach — whenever that first tick exceeds 1s, i.e. under
    # CPU load.
    set -g status-format[0] "#(echo; ${script.tmux-update-icons}/bin/tmux-update-icons #{qs:session_name} '#{@resume_claude}' '#{start_time}' '#{@resume_carousel}' '#{@catppuccin_flavor}' '#{pid}')#(${picker-statusline-bin} --session #{qs:session_name} --thm-bg '#{@thm_bg}' --thm-red '#{@thm_red}' --thm-mauve '#{@thm_mauve}' --thm-blue '#{@thm_blue}' --thm-text '#{@thm_fg}' --thm-subtext0 '#{@thm_subtext_0}' --thm-overlay1 '#{@thm_overlay_1}' --thm-peach '#{@thm_peach}' --thm-green '#{@thm_green}' --flavor '#{@catppuccin_flavor}' --icon-session '#{@icon_session}' --icon-branch '#{@icon_branch}' --icon-dir '#{@icon_dir}' --icon-remote '#{@icon_remote}' --icon-linear '${enrichIconsDoubled.linear}' --icon-github '${enrichIconsDoubled.github}'${lib.optionalString agentUsageEnable " --icon-usage-claude '${processIcons.claude or "🧠"}' --icon-usage-codex '${processIcons.codex or "🤖"}' --icon-usage-cursor '${processIcons."cursor-agent" or "🧊"}' --icon-usage-pi '${processIcons.pi or "🥧"}' --agent-usage-monthly-threshold '${toString agentUsageMonthlyThreshold}'"})"
    # Lines 1-3: Window list (dynamically generated by tmux-reflow-windows hook)
    # A window tagged by an external fan-out orchestrator (@crew_name codename +
    # @crew_color) shows the codename as a badge after "index: ", tinted by that
    # color; the index and label keep the default mauve/subtext0 so the label stays
    # legible in both themes. The PR segment keeps its own check-state color.
    # tmux-reflow-windows mirrors this for the multi-line variants, carving the
    # badge from the tagged window's label so untagged windows stay gapless (see
    # its @window_crew_disp).
    # Tab anatomy: idx + bold identity prefix (@window_label_id) + remainder
    # (@window_label_disp, reflow's display copy — clipped when the row only fits
    # with its widest labels shaved, so @window_label_rest_* can stay the full
    # identity the pickers read) + icons, then the PR segment last, split into a
    # glyph half and a #<n> half.
    # The glyph is colored by check state, in order: merged=mauve, closed=overlay0,
    # failing/conflicting=red, pending=peach, success/open=green (closed wins over
    # a stale check). The #<n> half (open PRs only) is retinted by review decision
    # (approved=green, changes_requested=red, review_required=overlay0, no decision
    # keeps the glyph's tint) and underlined when auto-merge is queued. Rendered
    # last so its color only runs into the separator, which sets its own color.
    # tmux-reflow-windows mirrors this layout for the multi-line variants with
    # column-padded segments.
    set -g status-format[1] "#[align=left,bg=#{@thm_bg}]#[fg=#{@thm_overlay_1}] ╰─ #{W:#[range=window|#{window_index}]#[nobold]#{?window_active,#[fg=#{@thm_mauve}#,bg=#{@thm_bg}#,bold],#[fg=#{@thm_subtext_0}#,bg=#{@thm_bg}]}#{window_index}: #{?#{&&:#{?#{@bridge_win},1,#{@window_has_agent}},${bridgeOpt "crew_name"}},#{?${bridgeOpt "crew_color"},#[fg=${bridgeOpt "crew_color"}#,bg=#{@thm_bg}],}${bridgeOpt "crew_name"} #{?window_active,#[fg=#{@thm_mauve}#,bg=#{@thm_bg}#,bold],#[fg=#{@thm_subtext_0}#,bg=#{@thm_bg}]},}#[bold]#{@window_label_id}#{?window_active,,#[nobold]}#{@window_label_disp}#{?window_active,#[fg=#{@thm_fg}#,bg=#{@thm_bg}#,nobold],} #{@window_icon_display}#{?window_zoomed_flag, 󰁌,}#{?#{&&:${bridgeOpt "pr_number"},#{!=:${bridgeOpt "pr_number"},none}},#{?#{==:${bridgeOpt "pr_state"},merged},#[fg=#{@thm_mauve}],#{?#{==:${bridgeOpt "pr_state"},closed},#[fg=#{@thm_overlay_0}],#{?#{||:#{==:${bridgeOpt "pr_check_state"},failure},#{==:${bridgeOpt "pr_mergeable"},conflicting}},#[fg=#{@thm_red}],#{?#{==:${bridgeOpt "pr_check_state"},pending},#[fg=#{@thm_peach}],#[fg=#{@thm_green}]}}}},}#{@window_pr_glyph}#{?#{==:${bridgeOpt "pr_state"},open},#{?#{==:${bridgeOpt "pr_review"},approved},#[fg=#{@thm_green}],#{?#{==:${bridgeOpt "pr_review"},changes_requested},#[fg=#{@thm_red}],#{?#{==:${bridgeOpt "pr_review"},review_required},#[fg=#{@thm_overlay_0}],}}}#{?${bridgeOpt "pr_auto_merge"},#[underscore],},}#{@window_pr_num}#[nounderscore]#{?#{@window_claude_ago}, #[fg=#{@thm_overlay_1}]#{@window_claude_ago},}#[bg=#{@thm_bg}]#[norange]#{?next_window_index, #[fg=#{@thm_subtext_0}#,nobold]│ ,}}"
    set -g status-format[2] ""
    set -g status-format[3] ""
    set -g status-format[4] ""

    # Monitor-hook floor for the status-tick side effects (#603): a `-B` session
    # monitor's timer is owned by the monitor set, not a client
    # (monitor.c:monitor_timer), so these fire on a server with zero clients --
    # which is exactly the state of a control-only bridge host (its only client
    # is a control-mode daemon, which draws no status line), where the
    # status-format[0] `#()` jobs below never ran at all.
    set -g @og_tick '%s'
    ${
      let
        esc = builtins.replaceStrings ["\""] ["\\\""];
        # The target field is deliberately EMPTY ('<name>::<format>'), never
        # ':session:' -- upstream 557967c3 turned an unrecognised non-empty
        # target into a hard parse failure, and '::' is the one spelling both
        # the flake's pinned tmux and a post-557967c3 bump accept.
        # Divisor 5 matches arm_agent_detect's own every-5th-tick cadence, so the
        # floor cannot make a new agent pane wait longer to be armed than today.
        tick = name: "${name}::#{e|/|:#{T:@og_tick},5}";
        # The CLEAR list, not the set list, and its order is load-bearing. Mirror
        # of tickHookNames in generator/render/status.go, which carries the
        # ordering constraint.
        hookNames = [
          "@og-pr-tick"
          "@og-backfill-tick"
          "@og-usage-tick"
          "@og-sweep-tick"
          "@og-res-tick"
        ];
        # `-g` leaves the monitor's session NULL (cmd-set-option.c), which keeps
        # the hook alive for the server's whole life instead of dying with the
        # session that loaded this config.
        #
        # Cleared unconditionally, ABOVE the conditional setters: hooks_monitor_add
        # keys on the name so a reload replaces a hook, but disabling a feature
        # does not re-set one -- without this, `enrich.enable = false` + reload
        # would leave the previous generation's monitor firing `run-shell -b` at
        # a store path GC will remove, every 5s, for the life of the server.
        # `-u -B` only drops the subscription; the `@name` option itself (the
        # command string, store path included) survives in `show-options -gv`
        # until a plain `set -gu` clears it too -- nothing fires without the
        # subscription, so this second clear is for auditability, not liveness.
        clears = lib.concatMap (n: ["set-hook -g -u -B '${n}'" "set -gu '${n}'"]) hookNames;
        setHook = name: cmd: "set-hook -g -B '${tick name}' 'run-shell -b \"${cmd}\"'";
        setters =
          (lib.optional enrichEnable (setHook "@og-pr-tick" "${script.tmux-pr-enrich}/bin/tmux-pr-enrich --tick"))
          ++ (lib.optional enrichEnable (setHook "@og-backfill-tick" "${script.tmux-issue-stamp}/bin/tmux-issue-stamp --backfill"))
          ++ (lib.optional agentUsageEnable (setHook "@og-usage-tick" "${script.tmux-agent-usage}/bin/tmux-agent-usage --tick"))
          # Unconditional, and arming only -- tmux-update-icons skips the reap
          # and the prune for this caller; its own comments carry why.
          #
          # `VAR=1 cmd` works because run-shell hands its whole argument to
          # `sh -c`. The env var, not a `--sweep` argv flag, is what the script
          # dispatches on: $1 is a session name at every other callsite, and a
          # session literally named "--sweep" would misroute itself forever.
          # That also leaves this command free of any tmux format, which
          # tick-floor-conf-assertions enforces for every setter here.
          ++ [(setHook "@og-sweep-tick" "OG_TICK_SWEEP=1 ${script.tmux-update-icons}/bin/tmux-update-icons")]
          # Unconditional too, and deliberately flagless: the session-resource
          # stamp is the REMOTE half of a feature whose local half (the
          # picker's CPU/Mem columns) is opted into on a different machine, so
          # a host with no remote.hosts of its own is exactly the host someone
          # bridges to. Its command is a bare store path plus a flag, so it
          # keeps the no-tmux-format invariant the sweep comment above states.
          ++ [(setHook "@og-res-tick" "${picker-session-res-bin} --tick")];
        body = esc (lib.concatStringsSep " \\; " (clears ++ setters));
      in ''
        # -B needs tmux 3.8+, probed against the LIVE server (#407) since a
        # server predating a nix switch keeps its old binary resident. String
        # form, not a brace block: tmux parses every branch of `{ }` at source
        # time, so -B is rejected even on the untaken branch. The hook command
        # string is format-expanded before it is parsed (hooks_parse via
        # hooks_monitor_hook_cb), which is safe here only because Nix store
        # paths never contain '#'.
        if-shell "tmux list-commands set-hook | grep -q -- -B" "${body}" "display-message 'tmux-og: tmux predates 3.8 -B session monitors -- PR/backfill/usage polling and the agent sweep only run while a real client has this session attached, and remote session resources are not stamped at all'"
      ''
    }

  '';

  hooksText = ''
    # Reflow hooks: clear stale hooks first so source-file is idempotent.
    # Without this, hooks from previous configs or manual testing persist across reloads.
    set-hook -gu after-new-window
    set-hook -gu session-window-changed
    set-hook -gu client-resized
    set-hook -gu client-resized[20]
    set-hook -gu after-new-session
    set-hook -gu client-session-changed
    set-hook -gu client-attached
    set-hook -gu client-attached[20]
    set-hook -gu window-resized
    set-hook -gu window-layout-changed

    # Also clear hooks from older config versions that may linger
    set-hook -gu window-linked
    set-hook -gu window-unlinked
    set-hook -gu after-resize-pane
    set-hook -gu after-kill-pane
    set-hook -gu pane-exited
    set-hook -gu pane-died
    set-hook -gu pane-shell-prompt
    set-hook -gu 'client-light-theme[40]'
    set-hook -gu 'client-dark-theme[40]'
    set-hook -gu pane-focus-in
    set-hook -gu after-select-pane

    # Notification producers (#164): bare -gu clears every index, so prefix+r
    # stays idempotent — and a rebuild with notifications OFF cannot leave a hook
    # pointing at a dead store path. The alert-*[20] setters MUST sit below this
    # block; above it they would be cleared on every load.
    set-hook -gu alert-bell
    set-hook -gu alert-activity

    # Hooks to reflow windows across status lines.
    # after-new-window / window-unlinked are the burst-prone ones (dispatcher
    # fan-out, mass close): run them backgrounded (-b) so the reflow lock's
    # brief retry-wait (see tmux-reflow-windows, issue #150) stays off the
    # server's command queue instead of wedging it. session-window-changed
    # fires on every switch but is a win_count:WIDTH cache hit that exits before
    # the lock, so it stays synchronous.
    set-hook -g after-new-window        'run-shell -b "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'
    set-hook -g window-unlinked         'run-shell -b "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'
    set-hook -g session-window-changed  'run-shell "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'
    # client-resized fires on every step of a terminal drag; each distinct width
    # is a cache miss → full O(N) recompute. Background it (-b, off the server's
    # command queue) with --debounce so a drag coalesces to one reflow at the
    # final width — see the debounce block in tmux-reflow-windows.
    set-hook -g client-resized          'run-shell -b "${script.tmux-reflow-windows}/bin/tmux-reflow-windows --debounce #{qs:session_name} #{q:client_width}"'
    set-hook -g after-new-session       'run-shell "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'
    set-hook -g client-session-changed  'run-shell "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'
    # A cold attach to an existing session (tmux attach, terminal reconnect, a
    # 2nd client at a different width) fires client-attached but not
    # client-session-changed, so the window-list grid would stay sized for the
    # last reflow's width until a resize/switch nudged it (issue #188). Indexed
    # [10] so it coexists with the splash/carousel client-attached[50]/[60]
    # hooks; the bare `set-hook -gu client-attached` above clears it on reload.
    set-hook -g client-attached[10]     'run-shell -b "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"'

    # Birth-only knob for detached sessions under window-size latest (#494).
    # Indexed [20] beside reflow [10] and splash [50]; -gu [20] in the block
    # above clears on reload (bare -gu on the hook name clears every index too).
    set-hook -g client-attached[20]     'run-shell -b "${script.tmux-default-size}/bin/tmux-default-size"'
    set-hook -g client-resized[20]      'run-shell -b "${script.tmux-default-size}/bin/tmux-default-size"'

    # Refit floating panes to the new window size (#371). window-resized, not
    # client-resized: it carries the window that actually changed, and it also
    # covers the sizes that change without a client resize (attaching a second,
    # smaller client; switching a window between clients of different sizes).
    # Undebounced, unlike the reflow above — the work is two no-op-when-unchanged
    # tmux commands per stamped float, and tracking a live terminal drag is the
    # point.
    set-hook -g window-resized          'run-shell -b "${script.tmux-float-refit}/bin/tmux-float-refit #{q:window_id}"'

    # Responsive dispatcher grid layout (#749). Indexed [10] beside the
    # tmux-float-refit setter at index 0, so a single resize fires both; the bare
    # `set-hook -gu window-resized` above clears every index on reload. The
    # dispatcher calls tmux-grid-refit itself after adding or removing a role
    # pane. Read-only on the @crew_* hints it consumes.
    set-hook -g window-resized[10]      'run-shell -b "${script.tmux-grid-refit}/bin/tmux-grid-refit #{q:window_id}"'

    # A pane split/kill/move changes the layout without resizing the window, so
    # window-resized never fires and the lead keeps whatever cells tmux
    # redistributed to it (#760) — the aeye carousel toggle is exactly such a
    # split-then-kill. window-layout-changed fires on every layout mutation and
    # tmux-grid-refit re-normalises the grid; it stays a no-op on anything else,
    # and the script excludes floating panes (which also fire this hook).
    set-hook -g window-layout-changed   'run-shell -b "${script.tmux-grid-refit}/bin/tmux-grid-refit #{q:window_id}"'

    # Tag every newly-created window as a worktree window from its cwd — at
    # creation, regardless of creator or CLAUDECODE (issue #95). new-session
    # doesn't fire after-new-window for its first window, so both are needed; -b
    # keeps the git probe off the creation path and avoids the foreground
    # run-shell re-entrancy that cascades the hook. Indexed so they coexist with
    # the index-0 reflow hooks; the bare `set-hook -gu` above clears them on reload.
    # Target by #{window_id} alone (globally unique): #{session_id} is "$N", and
    # run-shell's sh -c would re-expand the leading $ (e.g. $0 -> "sh").
    set-hook -g after-new-window[10]  'run-shell -b "${script.tmux-reconcile-window}/bin/tmux-reconcile-window #{q:window_id}"'
    set-hook -g after-new-session[10] 'run-shell -b "${script.tmux-reconcile-window}/bin/tmux-reconcile-window #{q:window_id}"'

    # Post-#199 (issue #100): after-split-window / pane-focus-in reconcile hooks
    # were considered here and deliberately NOT added. Since #199
    # (tmux-worktree-match.sh), every `wt switch` already treats a stale
    # @worktree tag as untrusted: it unsets a tag no pane corroborates and
    # retags whichever window the switch actually lands on. The residual gap —
    # a window's worktree changes via a raw split/focus/cd and is never
    # `wt switch`ed into again — is real but narrow (wt switch is this repo's
    # primary navigation path), while pane-focus-in fires on every pane focus
    # change, the hottest per-interaction path in this config, and
    # tmux-reconcile-window forks ~7 subprocesses even in its idempotent
    # branch. Not worth it. That residual gap is closed since #596, in
    # tmux-update-icons' batched read instead of a hook: it already walks every
    # pane each tick, so noticing the move costs a format field rather than a
    # fork, and it fires only when the cwd leaves the window's @worktree.

    # Mirror local pane focus onto the remote inside a bridge window.
    # after-select-pane is the reliable seam: it fires whether or not a client is
    # attached, and tmux skips it for a select-pane that was already a no-op
    # (pane-focus-in needs terminal focus reporting and fires erratically).
    # Backgrounded so a socket round-trip never sits on the server's command queue
    # — the daemon drops a request that arrives out of order. Indexed [20] so it
    # coexists with any future consumer; the bare `set-hook -gu` above clears it.
    set-hook -g after-select-pane[20] "if-shell -F '${bridgeGate}' { run-shell -b \"${bridgeCtl} focus #{q:@bridge_pane}\" }"

    # Measured on the pinned next-3.9 binary: process exit/signal fires pane-exited
    # (remain-on-exit off) or pane-died (remain-on-exit on, corpse stays).
    # Structural kills (kill-pane/kill-window/kill-session/respawn-pane -k) fire no
    # pane hook, which is why the sweep survives as backstop. Index 0 is tmux-og's
    # primary-hook convention (remux sits at [99]). run-shell -b keeps the fork off
    # the server's command queue.
    set-hook -g pane-exited 'run-shell -b "${script.tmux-reap-pane}/bin/tmux-reap-pane #{q:hook_pane}"'
    set-hook -g pane-died   'run-shell -b "${script.tmux-reap-pane}/bin/tmux-reap-pane #{q:hook_pane}"'

    # Dead-agent detection from the shell's own prompt mark (#646): OSC 133;A fires
    # pane-shell-prompt whenever the shell redraws a prompt, so the handler clears
    # the exited agent's state. pane_current_command is the foreground pgrp leader's
    # argv[0], so a nested prompt inside a still-running agent reads as the agent
    # and the handler skips it. pane-command-finished/started are deliberately
    # unwired: the prompt event is the conservative "back at a prompt" signal.
    # Index 0 (tmux-og's convention; remux owns [99]); run-shell -b keeps the fork
    # off the command queue. pane_current_command and session_name are wrap-required
    # formats so they take bare #{qs:} (never #{q:}, which loses a leading ~/word);
    # #{q:hook_pane} is a %N id. session_name (never session_id — its $N re-expands
    # in run-shell) carries the ownership guard. window_id and @window_has_agent
    # (#671) are the event trigger's window-wide naming/crew reset args — the
    # latter is the hook-fire-time value, read for free in this same
    # hook-context substitution, no extra fork to fetch it. @bridge_win (#741) is
    # passed the same way so the handler can skip a daemon-owned mirror pane
    # before it clears pane state, again with no fork on the every-prompt path.
    # Both booleans go through #{?…,1,0}: an unset user option makes #{q:…}
    # expand to *nothing* (not an empty word), so a bare @window_has_agent would
    # vanish and shift @bridge_win into its slot.
    set-hook -g pane-shell-prompt 'run-shell -b "${script.tmux-shell-prompt}/bin/tmux-shell-prompt #{q:hook_pane} #{qs:pane_current_command} #{qs:session_name} #{q:window_id} #{?#{@window_has_agent},1,0} #{?#{@bridge_win},1,0}"'

    # A scratchpad dies with its parent session ([99] is tmux-remux's capture-event)
    set-hook -g session-closed[98] 'run-shell -b "tmux kill-session -t =scratch-#{qs:hook_session_name} 2>/dev/null || true"'

    # Clear unseen claude status flags when user focuses a window
    set-hook -g session-window-changed[99] 'run-shell "${script.claude-status-update}/bin/claude-status-update mark-seen --session #{qs:session_name} --window #{q:window_index}"'
    set-hook -g client-session-changed[99] 'run-shell "${script.claude-status-update}/bin/claude-status-update mark-seen --session #{qs:session_name} --window #{q:window_index}"'

    # Pane borders
    setw -g pane-border-status top
    setw -g pane-border-format "━━━━━"
    # A mirror's borders take their colour from the bridged crew options (#640):
    # the pane's own role colour, else the window's agent colour, which is how
    # the remote colours the same borders.
    setw -g pane-active-border-style "bg=#{@thm_bg},fg=#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{@thm_mauve}}}"
    setw -g pane-border-style "bg=#{@thm_bg},fg=#{?@bridge_crew_role_color,#{@bridge_crew_role_color},#{?@bridge_crew_color,#{@bridge_crew_color},#{@thm_overlay_1}}}"
    setw -g pane-border-lines heavy

    # Pane background: dim inactive
    set -g window-style "fg=#{@thm_fg},bg=#{@thm_mantle}"
    set -g window-active-style "fg=#{@thm_fg},bg=#{@thm_bg}"

    # Pane scrollbars (tmux 3.8+): auto-hide overlays the scrollbar on scroll or
    # hover and lets it disappear after pane-scrollbars-timeout (500ms default,
    # fine as-is) instead of narrowing the pane while it's visible (the old modal
    # behaviour).
    set -g pane-scrollbars auto-hide
    set -g pane-scrollbars-style "fg=#{@thm_mauve},bg=#{@thm_surface_0},width=1"
    set -g pane-scrollbars-position right

    # Prompt cursor styling (tmux 3.6+)
    set -g prompt-cursor-style "blinking-bar"
    set -g prompt-cursor-colour "colour183"

    # Window naming: multi-pane process icons + branch or dir name
    set -wg automatic-rename on
    # window_name keeps the plain PR number (no color codes) so choose-tree
    # pickers still show which window owns which PR. Same segment order as the
    # status tabs: name, icons, PR last.
    set -g automatic-rename-format "#{?#{@window_label_short},#{@window_label_short},#{b:pane_current_path}} #{@window_icon_display}#{@window_pr_plain}"

    # tmux-fingers (smart copy) — hint colors set dynamically by tmux-apply-theme-colors
    set -g @fingers-pattern-0 "[A-Z]{2,}-[0-9]+"
    set -g @fingers-pattern-1 "[a-z][a-z_]*_[0-9a-hjkmnp-tv-z]{26}"
    set -g @fingers-pattern-2 "sha256-[A-Za-z0-9+/]{43}="
    set -g @fingers-pattern-3 "sha256:[0-9a-z]{52}"
    set -g @fingers-pattern-4 "[0-9A-HJKMNP-TV-Z]{26}"
    set -g @fingers-pattern-5 "([0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}"
    set -g @fingers-pattern-6 "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
    set -g @fingers-pattern-7 "arn:[a-z0-9-]+:[a-z0-9-]+:[a-z0-9-]*:[0-9]*:[a-zA-Z0-9_/.:-]+"
    set -g @fingers-pattern-8 "eyJ[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{10,}"
    set -g @fingers-pattern-9 "([0-9a-fA-F]{2}[:-]){5}[0-9a-fA-F]{2}"
    # Only scan built-ins we actually use. Dropped: digit (too noisy — matches
    # any 4+ digit run), git-status, git-status-branch, diff (niche).
    set -g @fingers-enabled-builtin-patterns "url,path,ip,uuid,sha,hex,kubernetes"

    # fingers opens its hint overlay as a window and closes it again, so every
    # invocation left a close in tmux-remux's undo list that nobody made and
    # nobody can want back. Matched literally, not as a glob: "[fingers]" as a
    # glob is a character class matching one of f,i,n,g,e,r,s.
    set -g @remux_ignore_windows "[fingers]"

    # Apply theme-dependent colors (must run after catppuccin loads, and
    # before tmux-fingers loads: load-config snapshots @fingers-*-style into
    # its own config.json, so the styles must be set first or fingers renders
    # the previous theme's colors until the next toggle).
    run-shell "${script.tmux-apply-theme-colors}/bin/tmux-apply-theme-colors"
    # A toggle re-sources this config, which is the only signal a mirror gets
    # that the theme moved; the script itself is a no-op unless the flavor
    # actually changed, so `prefix + r` costs nothing.
    run-shell -b "${script.og-remote-theme}/bin/og-remote-theme"

    # Follow the terminal's own reported theme (#663). tmux 3.6+ fires these on
    # every report, not only on a change (measured), so the handler carries its
    # own no-op guard and lock; the hook only has to stamp the want synchronously,
    # on the server's command queue, so concurrent reports land in report order.
    # The literal per hook, never #{client_theme}, so no format value reaches a
    # shell. `set -g @og_follow_client_theme off` disables this at runtime.
    # Control-mode clients (the remote bridge) have no tty and never fire.
    # The { } command list is load-bearing, not style: a hook value chained with
    # a bare `\;` (measured) is re-split by tmux's own parser into too many
    # arguments for set-hook, and the second command is silently never set.
    set-hook -g 'client-light-theme[40]' { set -g @og_client_theme_want light ; set -gF @og_client_theme_client "#{hook_client}" ; run-shell -b "${script.tmux-client-theme}/bin/tmux-client-theme #{q:hook_client}" }
    set-hook -g 'client-dark-theme[40]' { set -g @og_client_theme_want dark ; set -gF @og_client_theme_client "#{hook_client}" ; run-shell -b "${script.tmux-client-theme}/bin/tmux-client-theme #{q:hook_client}" }

    run-shell ${tmuxPlugins.fingers}/share/tmux-plugins/tmux-fingers/tmux-fingers.tmux

    # Synchronous init on config load so icons + window bar are ready before the user sees it
    run-shell "${script.tmux-update-icons}/bin/tmux-update-icons #{qs:session_name}"
    run-shell "${script.tmux-reflow-windows}/bin/tmux-reflow-windows #{qs:session_name} #{q:client_width}"

    ${lib.optionalString splashEnable ''
      # Welcome buffer: indexed ([50]) so it coexists with the reflow hooks'
      # index-0 bindings on the same events (a bare set-hook would clobber them).
      # _name (not #{hook_session}) sidesteps the $0 re-expansion hazard at :960-961.
      # hook_client is tty-derived (never ~-initial); tmux-splash-maybe defaults its final positional when empty.
      # #{q:} and NOT \"...\": the whole string is format-expanded before sh -c sees
      # it, so a quote in the name breaks out and executes — and a bridged session's
      # name comes from the remote host (og-remote-open builds it from the
      # remote's list). || true stops the gate's fail-closed exit from pushing the
      # hook's pane into view-mode.
      set-hook -g client-attached[50]        'run-shell -b "${script.tmux-splash-maybe}/bin/tmux-splash-maybe #{qs:hook_session_name} #{q:hook_client} || true"'
      set-hook -g client-session-changed[50] 'run-shell -b "${script.tmux-splash-maybe}/bin/tmux-splash-maybe #{qs:hook_session_name} #{q:hook_client} || true"'
    ''}

    ${lib.optionalString notifyEnable ''
      # === Notification producers (#164) ===
      # Placed here, below the set-hook -gu clear block: a bare -gu clears every
      # index, so these setters must come after it or each config load erases
      # them. Index [20] is free (this file uses [10], [50], [60], [98], [99]),
      # so nothing is clobbered; -b keeps the router off the server's command
      # queue. The command is the STORE PATH, never a bare name: a bare name
      # resolves against the tmux server's frozen PATH and would stay stale until
      # a full restart, making prefix+r an incomplete deploy.
      # #{window_id} and NOT #{session_id}: session_id is "$N" and run-shell's
      # sh -c re-expands a leading $. Titles are single words on purpose — a
      # quoted title would need three layers of escaping (Nix, tmux's config
      # lexer, sh -c) for no gain.
      # monitor-bell already defaults to on and this config never changes it, so
      # the bell producer works the moment the hook is wired. monitor-activity
      # stays at its default off — turning it on would make every unfocused
      # window with output (i.e. every working Claude pane) an event — so the
      # activity hook ships wired but dormant.
      set-hook -g alert-bell[20]     'run-shell -b "${script.og-notify}/bin/og-notify emit --source bell --level warn --window #{q:window_id} --title bell"'
      set-hook -g alert-activity[20] 'run-shell -b "${script.og-notify}/bin/og-notify emit --source activity --level info --window #{q:window_id} --title activity"'
    ''}

    ${carouselHooks}

    ${persistConf}${extraConfText}
  '';

  sections = [
    {
      name = "base";
      text = baseText;
    }
    {
      name = "keys";
      text = keysText;
    }
    {
      name = "status";
      text = statusText;
    }
    {
      name = "hooks";
      text = hooksText;
    }
  ];
in {
  inherit sections;

  tmuxConf = pkgs.writeText "tmux.conf" (lib.concatMapStrings (s: s.text) sections);
}
