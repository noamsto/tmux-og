#!/usr/bin/env bash
# Link picker — launches the bubbletea link TUI in a tmux popup.
# See docs/agents/picker.md; client pinning rationale is in tmux-which-key.sh.
set -euo pipefail

CLIENT=""
ORIGIN=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--client)
		CLIENT=${2:-}
		shift 2 || shift
		;;
	--origin-pane)
		ORIGIN=${2:-}
		shift 2 || shift
		;;
	*)
		break
		;;
	esac
done

BORDER_FG=$(tmux display -p '#{@thm_overlay_1}' 2>/dev/null || true)
[[ -n $BORDER_FG ]] || BORDER_FG="#7f849c"
# Pin the client and its window; see tmux-which-key.sh.
POPUP_CLIENT=()
[[ -n $CLIENT ]] && POPUP_CLIENT=(-c "$CLIENT" -t "$CLIENT:")
POPUP_ENV=()
[[ -n $ORIGIN ]] && POPUP_ENV+=(-e "OG_PICKER_ORIGIN_PANE=$ORIGIN")
[[ -n $CLIENT ]] && POPUP_ENV+=(-e "OG_PICKER_CLIENT=$CLIENT")
tmux display-popup "${POPUP_CLIENT[@]}" "${POPUP_ENV[@]}" -E -w 90% -h 60% -b rounded -T " Links " \
	-S "fg=$BORDER_FG" "@picker_generate@ --links"
