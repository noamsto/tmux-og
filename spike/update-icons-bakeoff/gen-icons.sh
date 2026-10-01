#!/usr/bin/env bash
# Regenerates icons.tsv from the built lib-icons, the one icon table C and D
# embed (go:embed / include_str!) as the shipped bash embeds ICON_MAP.
#   gen-icons.sh > icons.tsv
set -euo pipefail

# shellcheck source=/dev/null  # sibling helper
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
resolve_build
# shellcheck source=/dev/null  # store path resolved at runtime
source "$OG_LIB_ICONS"

printf '#max\t%s\n#agents\t%s\n' "$OG_MAX_ICONS" "$OG_AGENT_COMMANDS"
# The shipped fallback is empty (an unknown process gets no icon); the embedded
# table has no fallback row, and this stops the regeneration if that changes.
[[ -z $FALLBACK_ICON ]] || {
	echo "gen-icons: FALLBACK_ICON is now non-empty; C/D need a fallback row" >&2
	exit 1
}
# An empty entry means "no icon", which a missing row already yields, and a
# row ending in a tab would not survive the trailing-whitespace hook.
for k in "${!ICON_MAP[@]}"; do
	[[ -n ${ICON_MAP[$k]} ]] || continue
	printf '%s\t%s\n' "$k" "${ICON_MAP[$k]}"
done | LC_ALL=C sort
