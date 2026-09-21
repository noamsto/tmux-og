#!/usr/bin/env bash
# Notification event store + routing decision (#164). Sourced, not executed, by
# og-notify (the writer) and og-notify-center (the reader).
#
# This library does NOT source lib-log.sh. notify_prune needs acquire_lock and
# file_mtime, so a consumer that calls it must source lib-log.sh first — the
# router does; the center never prunes and needs nothing from lib-log.
# shellcheck disable=SC2034  # constants are used by the sourcing scripts

# Derived at source time so a test can relocate the whole store with one
# exported variable (same shape as CLAUDE_STATUS_DIR in lib-claude.sh).
OG_NOTIFY_DIR="${OG_NOTIFY_DIR:-/tmp/og-notify}"
NOTIFY_EVENTS_DIR="$OG_NOTIFY_DIR/events"
NOTIFY_MARKER="$OG_NOTIFY_DIR/.server_start"
NOTIFY_PRUNE_LOCK="$OG_NOTIFY_DIR/.prune.lock"

# Cap for title/body. They become a tmux message line and a popup row.
NOTIFY_VALUE_MAX=200

# Per-source glyphs. One definition, read by the router's message line and by
# the history center — the two are built in parallel and would otherwise drift.
# Icon overrides are P2. Codepoints verified with fontTools, not guessed.
NOTIFY_ICON_CLAUDE="󰚩"   # nerd: nf-md-robot (U+F06A9)
NOTIFY_ICON_PR="󰘭"       # nerd: nf-md-source-merge (U+F062D)
NOTIFY_ICON_BELL="󰂞"     # nerd: nf-md-bell-ring (U+F009E)
NOTIFY_ICON_ACTIVITY="󱅫" # nerd: nf-md-bell-badge (U+F116B)

# notify_route WINDOW_ACTIVE SESSION_ATTACHED -> REPLY=message|history
# Pure: no tmux, no forks, no stdout. `message` only when the event's window is
# the current one AND someone is attached to watch it; everything else — empty,
# non-numeric, missing — is history. The router is the only caller that feeds
# this live tmux values.
notify_route() {
	REPLY=history
	[[ ${1:-} == 1 ]] || return 0
	[[ ${2:-} =~ ^[0-9]+$ ]] || return 0
	# 10# so a hypothetical leading zero is decimal, not an octal error.
	((10#${2} > 0)) && REPLY=message
	return 0
}

# notify_icon SOURCE -> REPLY (empty for an unknown source)
notify_icon() {
	case "${1:-}" in
	claude) REPLY="$NOTIFY_ICON_CLAUDE" ;;
	pr) REPLY="$NOTIFY_ICON_PR" ;;
	bell) REPLY="$NOTIFY_ICON_BELL" ;;
	activity) REPLY="$NOTIFY_ICON_ACTIVITY" ;;
	*) REPLY="" ;;
	esac
}

# notify_locator SESSION WINDOW -> REPLY, e.g. "tmux-og:@7". The only locator
# format: the renderer and the center call this on the same two stored fields,
# so they agree by construction.
notify_locator() {
	REPLY="${1:-}:${2:-}"
}

# notify_ago SECONDS -> REPLY. Same compact vocabulary as claude_ago in
# lib-claude.sh and relAgo in the picker, so an age reads the same everywhere.
notify_ago() {
	local s="${1:-0}"
	((s < 0)) && s=0
	if ((s < 60)); then
		REPLY="${s}s"
	elif ((s < 3600)); then
		REPLY="$((s / 60))m"
	elif ((s < 86400)); then
		REPLY="$((s / 3600))h"
	else
		REPLY="$((s / 86400))d"
	fi
}

# notify_event_name -> REPLY="<epoch>-<ms>-<pid>". Sorts chronologically under a
# plain glob, so no consumer needs a stat to order events.
# EPOCHREALTIME's fraction is always 6 digits, so its first three ARE the
# zero-padded milliseconds — the same read log_event does in lib-log.sh.
# The [.,] class matters: a non-C LC_NUMERIC uses a comma radix.
notify_event_name() {
	local epoch us=${EPOCHREALTIME#*[.,]}
	printf -v epoch '%(%s)T' -1
	REPLY="$epoch-${us:0:3}-$$"
}

# notify_sanitize VALUE -> REPLY. Squeeze to one line, drop control chars, trim,
# cap. Deletes cntrl rather than keeping [:print:]: tr is byte-oriented, so a
# whitelist strips every non-ASCII byte and mangles UTF-8 (emoji, accents, RTL)
# — the same reasoning as the sanitizers in claude-status-update.sh.
notify_sanitize() {
	local clean
	clean=$(printf '%s' "${1:-}" | tr '\n\r\t' '   ' | tr -d '[:cntrl:]' | tr -s ' ')
	clean="${clean# }"
	clean="${clean:0:NOTIFY_VALUE_MAX}"
	clean="${clean% }"
	REPLY="$clean"
}

# notify_valid_source / notify_valid_level — constrained vocabularies, validated
# rather than sanitized. Status only, no REPLY.
notify_valid_source() {
	case "${1:-}" in
	claude | pr | bell | activity) return 0 ;;
	esac
	return 1
}

notify_valid_level() {
	case "${1:-}" in
	info | warn | error) return 0 ;;
	esac
	return 1
}

# notify_pid_is_tmux PID
# Succeeds when PID looks like a live tmux server. `kill -0` alone is satisfied
# by any process that reused a dead server's pid, so the process name is
# checked too (`ps -o comm= -p`, portable to macOS; the nix wrapper may show as
# .tmux-wrapped). Fails safe toward "yes": no ps on PATH, or an empty answer,
# cannot tell, so the caller protects. Deliberate copy of claude_pid_is_tmux in
# lib-claude.sh rather than a shared helper: factoring it into lib-log.sh would
# edit lib-claude.sh, owned by another worker while this landed.
notify_pid_is_tmux() {
	kill -0 "$1" 2>/dev/null || return 1
	command -v ps &>/dev/null || return 0
	local comm
	comm=$(ps -o comm= -p "$1" 2>/dev/null)
	[[ -z $comm ]] && return 0
	[[ $comm == *tmux* ]]
}

# notify_prune SERVER_START [SERVER_PID]
# Drops events written by a previous tmux server. Window and pane ids restart on
# server (re)start, so an event naming @7 from a dead server points at an
# unrelated window — actively misleading, not merely stale. A marker holding the
# current start_time gates the directory scan to once per server generation, so
# the emit path never globs the events dir outside that gate.
#
# mtime alone cannot tell "written by a server that has since died" from
# "written moments ago by a different, still-running server" — under the shared
# OG_NOTIFY_DIR, a second server's very first boot could otherwise delete a live
# different server's events. When SERVER_PID (this booting server's own #{pid})
# is passed, events/<name> files are scanned for their server= field (stamped
# by og-notify.sh); a file whose recorded owner PID is numeric, not SERVER_PID,
# and still a live tmux process (notify_pid_is_tmux, evaluated once per distinct
# owner per pass) is protected from deletion regardless of mtime. A legacy file
# with no server= field falls to mtime alone. With SERVER_PID the marker gate is
# per-server (.server_start.<pid>, content = start_time), so two live servers
# each sweep once per boot instead of ping-ponging one shared marker; the shared
# .server_start is still written after every sweep, and is the only gate when
# SERVER_PID is empty. A sweep also removes .server_start.<pid> markers whose
# pid is no longer alive.
#
# Requires acquire_lock + file_mtime from lib-log.sh. Failing to acquire the
# lock is not an error: another emit is already pruning, so skip and continue.
notify_prune() {
	local server_start="${1:-}" server_pid="${2:-}"
	[[ -z $server_start ]] && return 0
	local marker="$NOTIFY_MARKER"
	local gate="$marker"
	[[ -n $server_pid ]] && gate="$marker.$server_pid"
	[[ -r $gate && $(<"$gate") == "$server_start" ]] && return 0
	# The lock is a mkdir inside this dir, so the dir must exist first or every
	# acquire fails and the prune never runs.
	mkdir -p "$OG_NOTIFY_DIR" 2>/dev/null || return 0
	(
		# Called inside the subshell whose exit releases it: acquire_lock arms an
		# EXIT trap that rmdir's the lock.
		acquire_lock "$NOTIFY_PRUNE_LOCK" || exit 0
		local -A owner_live=()
		local f mt owner key val
		for f in "$NOTIFY_EVENTS_DIR"/*; do
			[[ -f $f ]] || continue
			if [[ -n $server_pid ]]; then
				owner=""
				while IFS='=' read -r key val || [[ -n $key ]]; do
					[[ $key == server ]] && {
						owner="$val"
						break
					}
				done <"$f"
				# Owner liveness is memoized: one ps per distinct pid per pass.
				if [[ $owner =~ ^[0-9]+$ && $owner != "$server_pid" ]]; then
					if [[ -z ${owner_live[$owner]+x} ]]; then
						if notify_pid_is_tmux "$owner"; then
							owner_live["$owner"]=1
						else
							owner_live["$owner"]=0
						fi
					fi
					[[ ${owner_live[$owner]} == 1 ]] && continue
				fi
			fi
			mt=$(file_mtime "$f")
			((mt < server_start)) && rm -f "$f"
		done
		if [[ -n $server_pid ]]; then
			printf '%s\n' "$server_start" >"$gate"
			local m mpid
			for m in "$marker".*; do
				[[ -f $m ]] || continue
				mpid="${m##*.server_start.}"
				[[ $mpid == "$server_pid" ]] && continue
				kill -0 "$mpid" 2>/dev/null || rm -f "$m"
			done
		fi
		printf '%s\n' "$server_start" >"$marker"
	)
	return 0
}
