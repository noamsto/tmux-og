#!/usr/bin/env bash
# which-key popup — launches the bubbletea TUI in a tmux popup.
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

# Both options in one round-trip: the popup's open latency is dominated by
# forks queued behind the (single-threaded) tmux server, and these run before
# anything paints. Both are only ever `set -g`, so resolving them through the
# option chain rather than `show -gv` picks the same values.
OPTS=$(tmux display -p '#{@thm_overlay_1}|#{@picker_layout}' 2>/dev/null || true)
BORDER_FG=${OPTS%%|*}
[[ -n $BORDER_FG ]] || BORDER_FG="#7f849c"
# List-only wants a shorter popup so a full-height list isn't mostly blank;
# a popup can't be resized after creation, so the height is chosen here.
HEIGHT=85%
[[ ${OPTS#*|} == list ]] && HEIGHT=60%
# Pin the client: unpinned, tmux re-resolves to the session's most-recently-active
# client, which on a bridged host can be the tty-less control client (#346,
# reported upstream as tmux/tmux#5551 — drop the pin once that ships). Also pin
# -t "$CLIENT:": the compat display-popup opens a float IN A WINDOW, and -c only
# picks the client, not the window — unpinned, tmux resolves -t to its "best"
# session rather than the client's (measured: landed in an unrelated newer session).
POPUP_CLIENT=()
[[ -n $CLIENT ]] && POPUP_CLIENT=(-c "$CLIENT" -t "$CLIENT:")
# OG_PICKER_ORIGIN_PANE is read by RunWhichKey (picker/whichkey.go) and
# replayed against after the popup closes — see replayBind for why.
POPUP_ENV=()
[[ -n $ORIGIN ]] && POPUP_ENV=(-e "OG_PICKER_ORIGIN_PANE=$ORIGIN")
tmux display-popup "${POPUP_CLIENT[@]}" "${POPUP_ENV[@]}" -E -w 90% -h "$HEIGHT" -b rounded -T " Keys " \
	-S "fg=$BORDER_FG" "@picker_generate@ --which-key"
