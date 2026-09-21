#!/usr/bin/env bash
# Notification router (#164): `og-notify emit …`.
#
# One tmux read resolves the event's window and hands back everything else the
# emit needs — routing inputs, the session id the renderer targets, the server
# start_time the prune gates on, and the theme hexes the message line
# interpolates. Then: write the per-event file (which IS the history), render
# the active-window path, prune a dead server generation.
#
# ALWAYS exits 0, on every path. Producers sit in the Claude Code hook chain and
# in a background poller; neither may have its exit code or its host hook broken
# by a notification.
#
# The background-window path is history-only in P1 — the toast renderer is P2,
# and both candidate hosts were rejected (display-popup -N steals focus,
# new-pane -d is window-bound so a live toast dies on window switch).
set -uo pipefail

# shellcheck source=/dev/null
source @lib_log@
# shellcheck source=/dev/null
source @lib_notify@

# Dwell time in ms. display-time is never set in this repo, so an omitted -d
# inherits tmux's 750ms — too short to read. bell/activity get a shorter one:
# monitor-bell is on and bell-action is `any`, so a process spewing \a re-fires
# the hook, and a burst should clear in ~1.5s rather than pin the line for 4s.
MSG_MS=4000
MSG_MS_ALERT=1500

[[ ${1:-} == emit ]] || exit 0
shift

src="" level="" window="" pane="" title="" body=""
while (($#)); do
	case "$1" in
	--source | --level | --window | --pane | --title | --body)
		# A flag with no value would make `shift 2` a no-op and spin forever.
		(($# >= 2)) || exit 0
		case "$1" in
		--source) src="$2" ;;
		--level) level="$2" ;;
		--window) window="$2" ;;
		--pane) pane="$2" ;;
		--title) title="$2" ;;
		--body) body="$2" ;;
		esac
		shift 2
		;;
	*) exit 0 ;; # unknown flag: silent no-op, never a broken producer
	esac
done

notify_valid_source "$src" || exit 0
notify_valid_level "$level" || exit 0
[[ -n $title ]] || exit 0
# Exactly one of --window / --pane.
[[ -n $window && -n $pane ]] && exit 0
[[ -z $window && -z $pane ]] && exit 0
target="${window:-$pane}"

command -v tmux >/dev/null 2>&1 || exit 0

# THE single tmux read. session_name is last because it is the only free-form
# field: `read` gives the last name the remainder of the line, so a '|' in a
# session name cannot shift the parse. session_id ($N) rides along for free and
# is what the renderer targets — a numeric session *name* is ambiguous as a -t
# target. Safe here because the router passes it as its own argv; nothing
# re-expands it through sh -c.
info=$(tmux display-message -p -t "$target" \
	'#{window_id}|#{window_active}|#{session_attached}|#{session_id}|#{start_time}|#{pid}|#{@thm_red}|#{@thm_peach}|#{@thm_green}|#{@thm_subtext_0}|#{session_name}' \
	2>/dev/null) || exit 0
IFS='|' read -r win_id win_active sess_attached sess_id srv_start srv_pid \
	thm_red thm_peach thm_green thm_sub sess_name <<<"$info"
# Empty resolution (window gone, wrong server): nothing written, nothing shown.
[[ -n ${win_id:-} ]] || exit 0

notify_route "$win_active" "$sess_attached"
routed="$REPLY"

# --- event file: one per event, never an append spool ---
# The window field is normalized to the bare @N id whatever target shape came
# in, so the center and any future consumer see exactly one format.
notify_sanitize "$title"
s_title="$REPLY"
notify_sanitize "$body"
s_body="$REPLY"
notify_event_name
event="$NOTIFY_EVENTS_DIR/$REPLY"
printf -v now '%(%s)T' -1
mkdir -p "$NOTIFY_EVENTS_DIR" 2>/dev/null || exit 0
{
	printf 'ts=%s\n' "$now"
	printf 'server=%s\n' "$srv_pid"
	printf 'source=%s\n' "$src"
	printf 'level=%s\n' "$level"
	printf 'window=%s\n' "$win_id"
	printf 'session=%s\n' "${sess_name:-}"
	printf 'title=%s\n' "$s_title"
	[[ -n $s_body ]] && printf 'body=%s\n' "$s_body"
	printf 'routed=%s\n' "$routed"
} >"$event" 2>/dev/null || exit 0

# --- render: active window in an attached session only ---
if [[ $routed == message ]]; then
	case "$level" in
	error) col="$thm_red" ;;
	warn) col="$thm_peach" ;;
	info) col="$thm_green" ;;
	*) col="" ;;
	esac
	if [[ $src == bell || $src == activity ]]; then
		ms="$MSG_MS_ALERT"
	else
		ms="$MSG_MS"
	fi

	notify_icon "$src"
	glyph="$REPLY"
	notify_locator "${sess_name:-}" "$win_id"
	# A '#' in a value is a tmux format escape; double it. Same convention the
	# repo already applies to icon values that reach a format. '%' needs the same
	# treatment for a different reason: display-message runs its argument through
	# strftime, so an ordinary PR title like "cut build time by 50%d" renders the
	# day-of-month. Unlike the @pr_* status path (where tmux inserts an option's
	# value without rescanning it), the value here is spliced into the format
	# string itself, so both escapes are on us.
	loc="${REPLY//#/##}"
	loc="${loc//%/%%}"
	esc_title="${s_title//#/##}"
	esc_title="${esc_title//%/%%}"
	esc_body="${s_body//#/##}"
	esc_body="${esc_body//%/%%}"

	# Resolved hex inside #[fg=…] (the form tmux-apply-theme-colors.sh already
	# writes), not a nested #{@thm_*}. An EMPTY fetched colour means catppuccin
	# has not loaded yet — omit the #[fg=…] entirely rather than emit a malformed
	# #[fg=]; the line just renders uncoloured.
	msg="$glyph "
	if [[ -n ${thm_sub:-} ]]; then
		msg+="#[fg=$thm_sub]$loc#[default] "
	else
		msg+="$loc "
	fi
	if [[ -n $col ]]; then
		msg+="#[fg=$col]$esc_title#[default]"
	else
		msg+="$esc_title"
	fi
	[[ -n $esc_body ]] && msg+=" — $esc_body"

	# One display-message PER ATTACHED CLIENT, each with an explicit -c. A
	# client-less display-message from a detached process exits 0 and shows
	# NOTHING, so an unnamed client is a silent no-op bug. Target the session by
	# id, not name. No -N, so a keystroke dismisses the toast and still reaches
	# the pane/key table; -C so the pane's tty keeps redrawing while it's up.
	while IFS= read -r client; do
		[[ -n $client ]] || continue
		tmux display-message -c "$client" -d "$ms" -C "$msg" 2>/dev/null || true
	done < <(tmux list-clients -t "$sess_id" -F '#{client_name}' 2>/dev/null)
fi

# Prune last: it is a marker-gated no-op on all but the first emit of a server
# generation, and the just-written event is newer than start_time so it survives.
notify_prune "$srv_start" "$srv_pid"
exit 0
