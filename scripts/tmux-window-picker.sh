#!/usr/bin/env bash
# Window picker — launches the bubbletea TUI in a tmux popup.
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

ARGS="--tui --windows"
TITLE=" Windows "
if [[ ${1:-} == "--agent" ]]; then
	ARGS="$ARGS --agent"
	TITLE=" Agent Windows "
fi

# Both options in one round-trip: the popup's open latency is dominated by
# forks queued behind the (single-threaded) tmux server, and these run before
# anything paints. Both are only ever `set -g`, so resolving them through the
# option chain rather than `show -gv` picks the same values. The client's window
# id rides along: it is needed after the popup to tell "the popup's host window
# was destroyed" from "the picker failed" (#887), and a separate read before
# first paint would only add a fork to that queue. It is only kept when
# `--client` pinned the popup's window — without it the popup lands in whatever
# window tmux resolves for `-t`, which the pre-paint read cannot name.
HOST_ARGS=()
[[ -n $CLIENT ]] && HOST_ARGS=(-t "$CLIENT")
OPTS=$(tmux display -p "${HOST_ARGS[@]}" '#{@thm_overlay_1}|#{@picker_layout}|#{window_id}' 2>/dev/null || true)
BORDER_FG=${OPTS%%|*}
[[ -n $BORDER_FG ]] || BORDER_FG="#7f849c"
REST=${OPTS#*|}
# List-only wants a shorter popup so a full-height list isn't mostly blank;
# a popup can't be resized after creation, so the height is chosen here.
HEIGHT=85%
[[ ${REST%%|*} == list ]] && HEIGHT=60%
HOST_WINDOW=""
[[ -n $CLIENT ]] && HOST_WINDOW=${REST#*|}
# A failed `display` returns fewer fields, so `${REST#*|}` is REST itself and no
# window was reported: leave the guard off rather than trust a bogus id.
[[ $HOST_WINDOW == "$REST" ]] && HOST_WINDOW=""
# Pin the client: unpinned, tmux re-resolves to the session's most-recently-active
# client, which on a bridged host can be the tty-less control client (#346,
# reported upstream as tmux/tmux#5551 — drop the pin once that ships). Also pin
# -t "$CLIENT:": display-popup opens a float in the -t window, and -c only
# picks the client, not the window — unpinned, tmux resolves -t to its "best"
# session rather than the client's (measured: landed in an unrelated newer session).
POPUP_CLIENT=()
[[ -n $CLIENT ]] && POPUP_CLIENT=(-c "$CLIENT" -t "$CLIENT:")
POPUP_ENV=()
[[ -n $CURRENT ]] && POPUP_ENV=(-e "OG_PICKER_CURRENT_WINDOW=$CURRENT")
set +e
tmux display-popup "${POPUP_CLIENT[@]}" "${POPUP_ENV[@]}" -E -w 90% -h "$HEIGHT" -b rounded -T "$TITLE" \
	-S "fg=$BORDER_FG" "@picker_generate@ $ARGS"
rc=$?
set -e

# display-popup exits 129 (killed by SIGHUP) when the float is destroyed from
# under it — the picker killing the window it was opened from, or the window
# closing. That is a completed kill, not a picker failure, and printing
# `returned 129` on the binding is the bug (#887). Suppress it only when the
# host window is really gone (a `list-windows` that fails because the server
# itself went away counts as gone): a non-zero exit with the window still there
# is a genuine picker failure and must still reach run-shell. TOCTOU: a real
# failure that happens to coincide with the window vanishing is swallowed too;
# telling them apart would need a marker the picker writes, more machinery than
# the bug is worth.
if [[ -n $HOST_WINDOW ]] && ((rc != 0)) &&
	! tmux list-windows -a -F '#{window_id}' 2>/dev/null | grep -Fqx "$HOST_WINDOW"; then
	exit 0
fi
exit "$rc"
