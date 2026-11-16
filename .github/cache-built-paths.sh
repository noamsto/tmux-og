#!/usr/bin/env bash
# Print the store paths CI should push to the binary cache, given a
# nix-fast-build --result-file. `ultimate` marks outputs this machine built
# (not substituted); `deriver != null` drops eval-time source copies.
set -euo pipefail

drv_list=$(jq -r '.results[] | select(.type == "EVAL" and .success and .drvPath != null) | .drvPath' "$1")
[ -n "$drv_list" ] || exit 0

drvs=()
while IFS= read -r drv; do
	drvs+=("$drv")
done <<<"$drv_list"

path_list=$(nix-store --query --requisites --include-outputs "${drvs[@]}")

paths=()
while IFS= read -r p; do
	[[ $p == *.drv ]] && continue
	paths+=("$p")
done <<<"$path_list"

chunk=500
for ((i = 0; i < ${#paths[@]}; i += chunk)); do
	nix path-info --json --json-format 1 "${paths[@]:i:chunk}" |
		jq -r 'to_entries[] | select(.value.ultimate and .value.deriver != null) | .key'
done | sort -u
