#!/usr/bin/env bash
# Times a from-scratch Nix build of the minimal Go and Rust derivations of the
# prototype, against the flake's pinned nixpkgs. Run each twice: the first run
# may also substitute the toolchain; the second, with the toolchain in the
# store and a fresh salt, is the derivation's own build cost.
#
#   nix-build-times.sh
set -euo pipefail

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SPIKE_DIR/../.." && pwd)"

build() {
	local name="$1" file="$2" start end
	# SALT changes the derivation hash, so every call is an uncached build.
	start="$EPOCHREALTIME"
	nix build --impure --no-link --print-out-paths --expr "
		let
			flake = builtins.getFlake \"$REPO_ROOT\";
			pkgs = flake.inputs.nixpkgs.legacyPackages.\${builtins.currentSystem};
		in
		(pkgs.callPackage $file {}).overrideAttrs (_: { SALT = \"$RANDOM$RANDOM\"; })
	" >/dev/null
	end="$EPOCHREALTIME"
	printf '%s: %.1f s\n' "$name" "$(awk -v s="$start" -v e="$end" 'BEGIN { print e - s }')"
}

for round in 1 2; do
	echo "round $round"
	build go "$SPIKE_DIR/c-go/default.nix"
	build rust "$SPIKE_DIR/d-rust/default.nix"
done
