# Cost probe only, not wired into flake.nix: what a minimal buildGoModule of
# variant C costs to build. See ../nix-build-times.sh.
{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "update-icons-go";
  version = "0";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [./main.go ./branch_git.go ./branch_head.go ./go.mod ../icons.tsv];
  };
  sourceRoot = "source/c-go";
  # go:embed cannot reach outside its module.
  preBuild = "cp ../icons.tsv icons.tsv";
  vendorHash = null;
  ldflags = ["-s" "-w"];
}
