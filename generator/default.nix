{
  pkgs,
  lib,
  ...
}:
pkgs.buildGoModule {
  pname = "og-generate";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = "sha256-pbA/AlBz3cQYRTMnQ/qBPcinYOKokrBLNhkbRTq54gE=";
  ldflags = ["-s" "-w"];
  nativeBuildInputs = [pkgs.makeWrapper];
  # Go names each main package's binary after its directory (the root package
  # takes the module directory's own name) -- this derivation now builds three
  # that way: og-generate (the root package), og-init, og-doctor. pname and
  # `nix run .#og-generate` still resolve fine via meta.mainProgram either way.
  #
  # --template stays required in the binary and gets its I6 default from the
  # wrapper rather than a patched source default: the flag's required-ness is
  # asserted by a test buildGoModule runs, and flag.Parse takes the last
  # occurrence, so a caller's own --template still wins over the added one.
  postInstall = ''
    mv $out/bin/generator $out/bin/og-generate
    wrapProgram $out/bin/og-generate \
      --add-flags "--template ${../config/tmux.conf.tmpl}"
    mv $out/bin/init $out/bin/og-init
    mv $out/bin/doctor $out/bin/og-doctor
  '';
  meta.mainProgram = "og-generate";
}
