#!/usr/bin/env bash
# Event logging for debugging tmux/claude/reflow/enrich/picker behavior.
# Sourced, not executed. Off unless the sentinel exists; the gate is a
# fork-free [[ -f ]] test, so hot paths pay nothing when debug is off.
# See docs/superpowers/specs/2026-06-09-event-logging-design.md
# shellcheck disable=SC2034  # exported names are used by sourcing scripts

OG_LOG_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/og"
OG_LOG_FILE="$OG_LOG_DIR/events.log"
# Sentinel lives in /tmp: dies on reboot, survives config reload (tmux sources
# the conf on every prefix+r, so a conf-load clear would disarm debug mid-bug).
OG_DEBUG_SENTINEL="${OG_DEBUG_SENTINEL:-/tmp/og-debug.on}"

# log_enabled: true when debug is armed. Fork-free builtin test — the hot-path gate.
log_enabled() { [[ -f $OG_DEBUG_SENTINEL ]]; }

# file_size / file_mtime FILE -> bytes / mtime-epoch on stdout (0 if absent).
# Home is lib-log because every stat-using script already sources it.
#
# GNU `stat -c` by absolute path (Nix-substituted): a bare `stat` is whichever
# of GNU or BSD comes first on PATH, and their flags collide (`-f` is format on
# BSD, filesystem mode on GNU). Unsubstituted under bats, PATH's `stat` is used;
# the checks make that GNU.
OG_STAT="@stat@"
if [[ $OG_STAT == @* ]]; then
	OG_STAT=stat
fi
file_size() { "$OG_STAT" -c %s "$1" 2>/dev/null || echo 0; }
file_mtime() { "$OG_STAT" -c %Y "$1" 2>/dev/null || echo 0; }

# owner_only_dir DIR [FILE] -> REPLY=FILE's mtime (0 if absent). True iff DIR
# is a real directory (not a symlink — GNU stat's default is lstat, so a
# symlink reports type "symbolic link"), owned by $UID, with no group/other
# permission bits. FILE, when given, must live inside DIR. Shell twin of
# picker/ownerdir.OwnerOnly.
owner_only_dir() {
	local out uid mode type mtime
	# A missing FILE makes stat exit nonzero while still printing DIR's line;
	# parse $out regardless. `|| :` on both this and the read group below keeps
	# that expected nonzero from tripping a caller's `set -e`.
	out=$("$OG_STAT" -c '%u %a %Y %F' -- "$@" 2>/dev/null) || :
	{
		read -r uid mode _ type
		read -r _ _ mtime _
	} <<<"$out" || :
	REPLY=${mtime:-0}
	[[ $uid == "$UID" && $type == directory ]] || return 1
	(((8#$mode & 8#077) == 0))
}

# acquire_lock DIR — non-blocking lock via atomic mkdir; `flock` is Linux-only
# (absent on macOS), so it can't be the primitive. Call INSIDE the subshell
# whose exit should release the lock: a successful acquire arms an EXIT trap
# that rmdir's it, mirroring flock's release-on-fd-close. Returns 1 when a live
# holder owns it. A crashed holder can't fire its trap, so a dir older than the
# stale window is stolen; a leftover plain file (e.g. from the old `9>"$lock"`
# redirect) is cleared too.
OG_LOCK_STALE_SECONDS="${OG_LOCK_STALE_SECONDS:-60}"
acquire_lock() {
	local dir="$1"
	mkdir "$dir" 2>/dev/null && {
		# shellcheck disable=SC2064  # bake $dir now; the local is gone at EXIT
		trap "rmdir \"$dir\" 2>/dev/null" EXIT
		return 0
	}
	if [[ -d $dir ]]; then
		local age
		age=$(($(date +%s) - $(file_mtime "$dir")))
		((age < OG_LOCK_STALE_SECONDS)) && return 1
		rmdir "$dir" 2>/dev/null
	else
		rm -f "$dir" 2>/dev/null
	fi
	mkdir "$dir" 2>/dev/null && {
		# shellcheck disable=SC2064  # bake $dir now; the local is gone at EXIT
		trap "rmdir \"$dir\" 2>/dev/null" EXIT
		return 0
	}
	return 1
}

# detach CMD [ARGS...] — run CMD fully backgrounded and disconnected from the
# caller's stdio, surviving the caller's exit. Portable stand-in for `setsid …
# &` (setsid is Linux-only): the subshell backgrounds the job and returns at
# once so tmux's #() reaps immediately, the grandchild reparents to init, and
# nohup detaches it from SIGHUP. Redirecting fds releases tmux's status pipe.
detach() {
	(nohup "$@" >/dev/null 2>&1 &)
}

# _json_escape STR -> REPLY  (JSON-safe inner string, no surrounding quotes).
# Backslash first, then quote/tab/cr; newlines stripped; remaining C0 controls stripped.
_json_escape() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\t'/\\t}
	s=${s//$'\r'/\\r}
	s=${s//$'\n'/}
	s=${s//[$'\x01'-$'\x08'$'\x0b'$'\x0c'$'\x0e'-$'\x1f']/}
	REPLY=$s
}

# _log_rotate: lock-guarded size rotation, keeps events.log.1. Cap is read live
# from OG_LOG_MAX_BYTES (default 5 MiB) so tests can shrink it.
_log_rotate() {
	[[ -f $OG_LOG_FILE ]] || return 0
	local cap="${OG_LOG_MAX_BYTES:-5242880}"
	local size
	size=$(file_size "$OG_LOG_FILE")
	((size < cap)) && return 0
	(
		acquire_lock "$OG_LOG_DIR/.rotate.lock" || exit 0
		local s
		s=$(file_size "$OG_LOG_FILE")
		((s >= cap)) && mv -f "$OG_LOG_FILE" "$OG_LOG_FILE.1"
	)
}

# log_event CATEGORY [KEY VALUE]...  No-op unless debug armed. One JSON line.
log_event() {
	log_enabled || return 0
	local cat=$1
	shift
	mkdir -p "$OG_LOG_DIR"
	# Millisecond ISO-8601, fork-free: bash strftime + EPOCHREALTIME. Avoids
	# date's GNU-only %N (BSD date has no sub-second). [.,] tolerates a comma
	# radix under a non-C LC_NUMERIC.
	local ts us=${EPOCHREALTIME#*[.,]}
	printf -v ts '%(%FT%T)T.%s' -1 "${us:0:3}"
	_json_escape "$cat"
	local line="{\"ts\":\"$ts\",\"cat\":\"$REPLY\""
	local k v ek
	while (($# >= 2)); do
		k=$1
		v=$2
		shift 2
		_json_escape "$k"
		ek=$REPLY
		_json_escape "$v"
		line+=",\"$ek\":\"$REPLY\""
	done
	line+="}"
	_log_rotate
	printf '%s\n' "$line" >>"$OG_LOG_FILE"
}
