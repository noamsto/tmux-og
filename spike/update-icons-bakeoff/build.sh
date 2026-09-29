#!/usr/bin/env bash
# Builds the compiled variants into .build/ (c-go, e-go-head, d-rust).
# Go comes from `nix develop`; Rust is pulled in for this one command only,
# from the flake's pinned nixpkgs, and is not part of any devShell.
#   ./build.sh
set -euo pipefail

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SPIKE_DIR/../.." && pwd)"
mkdir -p "$SPIKE_DIR/.build"

# go:embed cannot reach outside its module, so the one icon table is copied in.
cp "$SPIKE_DIR/icons.tsv" "$SPIKE_DIR/c-go/icons.tsv"
(
	cd "$SPIKE_DIR/c-go"
	go build -ldflags '-s -w' -o "$SPIKE_DIR/.build/c-go" .
	go build -tags head -ldflags '-s -w' -o "$SPIKE_DIR/.build/e-go-head" .
)

nix shell --inputs-from "$REPO_ROOT" nixpkgs#cargo nixpkgs#rustc -c \
	cargo build --release --locked --manifest-path "$SPIKE_DIR/d-rust/Cargo.toml" --target-dir "$SPIKE_DIR/.build/cargo-target"
cp "$SPIKE_DIR/.build/cargo-target/release/update-icons" "$SPIKE_DIR/.build/d-rust"
ls -l "$SPIKE_DIR/.build/c-go" "$SPIKE_DIR/.build/e-go-head" "$SPIKE_DIR/.build/d-rust"
