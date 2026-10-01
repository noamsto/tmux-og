# Cost probe only, not wired into flake.nix: what a minimal buildRustPackage of
# variant D costs to build. See ../nix-build-times.sh.
{
  lib,
  rustPlatform,
}:
rustPlatform.buildRustPackage {
  pname = "update-icons-rust";
  version = "0";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [./Cargo.toml ./Cargo.lock ./src ../icons.tsv];
  };
  sourceRoot = "source/d-rust";
  cargoLock.lockFile = ./Cargo.lock;
}
