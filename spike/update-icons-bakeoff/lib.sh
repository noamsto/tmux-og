#!/usr/bin/env bash
# Shared helpers for the update-icons bake-off. Sourced, never executed.
#
# resolve_build builds the wrapped tmux once and exports:
#   OG_RAW_TMUX   the unwrapped tmux binary (the fixture runs its own server on it)
#   OG_WRAPPER_BIN  the wrapper's bin dir holding the real, Nix-substituted scripts
#   OG_REAL_SCRIPT  variant A: the shipped tmux-update-icons, untouched
#   OG_LIB_ICONS / OG_LIB_CLAUDE / OG_LIB_LOG  the store paths the shipped script sources
#   OG_AGENT_COMMANDS / OG_MAX_ICONS  the two build-time constants it embeds

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SPIKE_DIR/../.." && pwd)"

# The sed patterns match literal $ and ${ in the wrapper and script text.
# shellcheck disable=SC2016
resolve_build() {
	local out wrapper
	out="$(nix build "$REPO_ROOT#default" --no-link --print-out-paths 2>/dev/null)"
	wrapper="$out/bin/tmux"
	OG_RAW_TMUX="$(sed -n 's|^exec -a "\$0" "\(.*\)" *-f .*|\1|p' "$wrapper")"
	OG_WRAPPER_BIN="$(sed -n "s|^PATH='\(.*\)'\$PATH\$|\1|p" "$wrapper")"
	OG_REAL_SCRIPT="$OG_WRAPPER_BIN/tmux-update-icons"
	OG_LIB_ICONS="$(sed -n 's|^source \(/nix/store/[^ ]*-lib-icons\)$|\1|p' "$OG_REAL_SCRIPT")"
	OG_LIB_CLAUDE="$(sed -n 's|^source \(/nix/store/[^ ]*-lib-claude\)$|\1|p' "$OG_REAL_SCRIPT")"
	OG_LIB_LOG="$(sed -n 's|^source \(/nix/store/[^ ]*-lib-log\)$|\1|p' "$OG_REAL_SCRIPT")"
	OG_AGENT_COMMANDS="$(sed -n 's|^AGENT_COMMANDS="${AGENT_COMMANDS:-\(.*\)}"$|\1|p' "$OG_REAL_SCRIPT")"
	OG_MAX_ICONS="$(sed -n 's|^[[:space:]]*MAX_ICONS=\([0-9]*\)$|\1|p' "$OG_REAL_SCRIPT")"
	[[ -x $OG_RAW_TMUX && -x $OG_REAL_SCRIPT && -f $OG_LIB_ICONS && -f $OG_LIB_CLAUDE && -f $OG_LIB_LOG && -n $OG_AGENT_COMMANDS && -n $OG_MAX_ICONS ]] || {
		echo "resolve_build: could not locate the built wrapper pieces" >&2
		return 1
	}
	export OG_RAW_TMUX OG_WRAPPER_BIN OG_REAL_SCRIPT OG_LIB_ICONS OG_LIB_CLAUDE OG_LIB_LOG OG_AGENT_COMMANDS OG_MAX_ICONS
}

# Store bash, so every bash variant starts the same interpreter as the shipped
# script's own shebang and none pays an extra `env` exec.
resolve_bash() {
	OG_BASH="$(sed -n '1s|^#!||p' "$OG_REAL_SCRIPT")"
	[[ -x $OG_BASH ]] || OG_BASH="$(command -v bash)"
	export OG_BASH
}

load_report() {
	printf 'load: %s (cores: %s)\n' "$(cut -d' ' -f1-3 /proc/loadavg)" "$(nproc)"
}

# variant_cmd A|B|C|D|E -- sets VCMD to the command (sans arguments) of a variant.
# C/D/E are built by build.sh into $SPIKE_DIR/.build.
# shellcheck disable=SC2034  # VCMD is read by the sourcing scripts
variant_cmd() {
	case "$1" in
	A) VCMD=("$OG_REAL_SCRIPT") ;;
	B) VCMD=("$OG_BASH" "$SPIKE_DIR/b-bash/update-icons.sh") ;;
	C) VCMD=("$SPIKE_DIR/.build/c-go") ;;
	D) VCMD=("$SPIKE_DIR/.build/d-rust") ;;
	E) VCMD=("$SPIKE_DIR/.build/e-go-head") ;;
	*)
		echo "variant_cmd: unknown variant $1" >&2
		return 1
		;;
	esac
}
