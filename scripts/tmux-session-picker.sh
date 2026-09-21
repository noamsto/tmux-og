#!/usr/bin/env bash
# Session picker — launches the bubbletea TUI in a tmux popup.
set -euo pipefail

CLIENT=""
CURRENT=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--client)
		CLIENT=${2:-}
		shift 2 || shift
		;;
	--current)
		CURRENT=${2:-}
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
POPUP_ENV=()
[[ -n $CURRENT ]] && POPUP_ENV=(-e "OG_PICKER_CURRENT_SESSION=$CURRENT")
tmux display-popup "${POPUP_CLIENT[@]}" "${POPUP_ENV[@]}" -E -w 90% -h "$HEIGHT" -b rounded -T " Sessions " \
	-S "fg=$BORDER_FG" "@picker_generate@ --tui"
