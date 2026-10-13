{
  pkgs,
  lib,
}: let
  src = pkgs.runCommand "picker-src" {} ''
    cp -r ${lib.cleanSource ./.} $out
    chmod -R u+w $out
    rm -f $out/default.nix
  '';
in
  pkgs.buildGoModule {
    pname = "tmux-og-go-tools";
    version = "0.1.0";
    inherit src;
    # checks.picker-go-tests runs the suite (with -race and a real tmux);
    # repeating it here would rerun timing-sensitive tests on every cache miss.
    doCheck = false;
    vendorHash = "sha256-Z72w5oQsXFFqvkK1ndOTtu7AonXtxAM55JhzgJV5Usc=";
    subPackages = ["." "splash" "statusline" "enrichcard" "agentdetect" "sessionres" "remotebridge" "remotebridge/cmd/daemon" "remotebridge/cmd/renderer" "remotebridge/cmd/ctl"];
    # -s -w strip debug info for a smaller binary + faster startup. psBin pins
    # tmux-session-resources' ps: it runs from a monitor hook, whose PATH is the
    # server's own — a headless tmux-startup.service sets none. Darwin takes
    # Apple's /bin/ps: the store's adv_cmds ps refuses the rss keyword ("requires
    # entitlement") and exits 1 with the column dropped.
    ldflags = [
      "-s"
      "-w"
      "-X main.psBin=${
        if pkgs.stdenv.hostPlatform.isDarwin
        then "/bin/ps"
        else "${pkgs.ps}/bin/ps"
      }"
    ];
    # Binary name matches pname (Go module produces "picker" by default)
    postInstall = ''
      mv $out/bin/picker $out/bin/tmux-picker-generate
      mv $out/bin/splash $out/bin/tmux-splash
      mv $out/bin/statusline $out/bin/tmux-statusline
      mv $out/bin/enrichcard $out/bin/tmux-enrich-card
      mv $out/bin/agentdetect $out/bin/agent-detect
      mv $out/bin/sessionres $out/bin/tmux-session-resources
      mv $out/bin/remotebridge $out/bin/og-remote-bridge
      mv $out/bin/daemon $out/bin/og-remote-bridge-daemon
      mv $out/bin/renderer $out/bin/og-remote-bridge-renderer
      mv $out/bin/ctl $out/bin/og-remote-bridge-ctl
    '';
  }
