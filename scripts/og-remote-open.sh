#!/usr/bin/env bash
# Create a local <host>-<sess> session (both halves sanitized) and launch the
# M2 multi-window bridge daemon detached: it enumerates every remote window and
# mirrors each into its own local window (live add/close/rename/active-changed).
# Resolves remote tmux path + TMUX_TMPDIR for the ssh control connection.
set -euo pipefail

# @lib_remote@ is substituted at Nix build time; in bats the lib is pre-sourced.
# shellcheck source=/dev/null
[[ -f "@lib_remote@" ]] && source "@lib_remote@"

# shell_quote single-quotes $1, escaping embedded single quotes — correct
# under any POSIX shell or fish for every character except a literal
# backslash (see shell_quotable() in lib-remote.sh for why). Callers must
# clear a value through shell_quotable() before it reaches here; $sess and
# OG_REMOTE_NEW_DIR are the two that do.
shell_quote() {
	local s="$1"
	s="${s//\'/\'\\\'\'}"
	printf "'%s'" "$s"
}

# Neither the detached mirror nor a session we create on the remote has a client
# yet, so tmux otherwise gives the first window `default-size` (80x24). Claude
# can start before the daemon's resize poll observes the real client, and some
# terminal UIs do not repaint after that first undersized PTY geometry. Seed
# both with the invoking client's content area.
initial_mirror_area() {
	local raw width height status status_rows
	local client_target=()
	if [[ -n ${TMUX_PANE:-} ]]; then
		client_target=(-t "$TMUX_PANE")
	fi
	raw="$(tmux display-message -p "${client_target[@]}" '#{client_width} #{client_height} #{status}' 2>/dev/null || true)"
	read -r width height status <<<"$raw"
	if [[ ! $width =~ ^[1-9][0-9]*$ || ! $height =~ ^[1-9][0-9]*$ ]]; then
		return 0
	fi

	if [[ $status == off ]]; then
		status_rows=0
	elif [[ $status == on ]]; then
		status_rows=1
	elif [[ $status =~ ^[0-9]+$ ]]; then
		status_rows=$status
	else
		status_rows=1
	fi

	height=$((height - status_rows))
	if ((height > 0)); then
		printf '%s %s\n' "$width" "$height"
	fi
}

# reap_daemon SIGTERMs pid, waits up to 2s, then SIGKILLs if it's still
# alive. Used whenever a live daemon has been proven stale so its socket +
# pidfile can be safely removed and a fresh one started.
reap_daemon() {
	local pid="$1"
	kill -TERM -- "$pid" 2>/dev/null || true
	for _ in {1..20}; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.1
	done
	kill -KILL -- "$pid" 2>/dev/null || true
}

# probe_daemon <sock> sets REPLY to `live` when the daemon behind sock's
# pidfile answers ping, `outdated` when it answers in an older ctl protocol,
# and empty when its pid is dead or its socket unreachable. A live pid alone
# is not enough: a config reload can leave a daemon that speaks an older ctl
# protocol behind. ctl bounds the probe at two seconds.
#
# A stale pidfile can be recycled by an unrelated process. Only the daemon's
# deterministic replies establish that the PID owns this socket, so only a
# non-empty REPLY may lead to signalling it. Matched on the suffix both
# old-protocol replies share: pinning the version digits stops reaping the
# daemon a later bump obsoletes.
probe_daemon() {
	local probe_error
	REPLY=""
	remote_daemon_alive "${1}.pid" || return 0
	if probe_error="$("$ctl" --sock "$1" ping _ 2>&1)"; then
		REPLY=live
	elif [[ $probe_error == *'— reopen the bridge'* ]]; then
		REPLY=outdated
	fi
}

# retire_mirror <session_id> removes a mirror whose name predates #783's
# sanitizing, with its daemon's files. Targeted by id: the raw name may not
# parse as a target.
retire_mirror() {
	local id="$1" old pid
	old="$(tmux display-message -p -t "$id" '#{@bridge_sock}' 2>/dev/null || true)"
	if [[ -n $old ]]; then
		probe_daemon "$old"
		if [[ -n $REPLY ]]; then
			pid="$(<"${old}.pid")"
			[[ $pid =~ ^[0-9]+$ ]] && reap_daemon "$pid"
		fi
	fi
	tmux kill-session -t "$id" 2>/dev/null || true
	if [[ -n $old ]]; then
		rm -f "$old" "${old}.pid" "${old}.phase"
	fi
}

# Rollback state for on_signal (#770). The picker's group TERM only reaches
# ssh — the daemon is launched setsid, outside the group — so this script owns
# undoing its own mirror on a signal. mirror_created means there is local state
# (a socket dir, maybe a mirror session) worth tearing down; attached means the
# commit point (one of the two switch-clients) has already run, so a racing
# signal is reported as success — a complete mirror exists either way.
mirror_created="" attached="" daemon_started=""
on_signal() {
	[[ -n $attached ]] && exit 0
	if [[ -n $mirror_created ]]; then
		# daemon_started is set before the launch attempt: set after the `&`, a
		# signal in between would leave a launched daemon unreaped. $! is unset
		# until that launch because nothing earlier in the script backgrounds a
		# job — don't add one before it, or ${!:-} would reap the wrong pid.
		if [[ -n $daemon_started && -n ${!:-} ]]; then
			reap_daemon "$!"
		fi
		tmux kill-session -t "=$local_sess" 2>/dev/null || true
		rm -f "$sock" "${sock}.pid" "$phase_file"
	fi
	exit 143
}
trap on_signal TERM INT HUP

host="$1"
sess="${2:-}"
win="${3:-}"

# Opt-in progress channel for the picker: one phase name per line on this fd,
# never on stdout (which a remote command can forge).
# Never exported onward — the daemon's hand-off re-runs this script from its
# own environment, where fd 3 may be something else entirely.
progress_fd=""
if [[ ${OG_REMOTE_OPEN_PROGRESS_FD:-} =~ ^[0-9]+$ ]]; then
	progress_fd=$OG_REMOTE_OPEN_PROGRESS_FD
fi
unset OG_REMOTE_OPEN_PROGRESS_FD

# 2>/dev/null runs before the fd redirect so a closed or never-opened fd
# fails silently instead of printing "Bad file descriptor" on our own stderr.
phase() {
	[[ -n $progress_fd ]] || return 0
	# shellcheck disable=SC2261 # intentional: order matters, see the comment above
	printf '%s\n' "$1" 2>/dev/null >&"$progress_fd" || true
}

if [[ -n $win && ! $win =~ ^[0-9]+$ ]]; then
	echo "og-remote-open: window index must be numeric, got: $win" >&2
	exit 1
fi

# Both pre-create a session the caller named, by different means, so honouring
# them together would restore and then create over the result. Checked here
# rather than left to callers: this is a public entry point (the README hands it
# out), so the picker never setting both is not an invariant to rely on.
if [[ -n ${OG_REMOTE_NEW_DIR:-} && -n ${OG_REMOTE_RESTORE:-} ]]; then
	echo "og-remote-open: OG_REMOTE_NEW_DIR and OG_REMOTE_RESTORE are mutually exclusive" >&2
	exit 1
fi

if [[ -n ${OG_REMOTE_NEW_DIR:-} ]] && ! shell_quotable "$OG_REMOTE_NEW_DIR"; then
	echo "og-remote-open: OG_REMOTE_NEW_DIR contains a backslash, which no remote shell dialect can quote safely: $OG_REMOTE_NEW_DIR" >&2
	exit 1
fi

# require_session_name <sess> exits unless sess is safe to carry: shell_quote
# is unsafe for a backslash, and a control byte never appears in a real tmux
# session name (tmux rejects them), so one can only come from a hostile
# remote — whose newline in the raw @bridge_session would forge a line in the
# pair lookup below. Byte-wise under LC_ALL=C, like mirror_name_part; %q keeps
# the rejected bytes off the user's terminal.
require_session_name() {
	local LC_ALL=C quoted
	if [[ $1 == *[[:cntrl:]]* ]]; then
		printf -v quoted '%q' "$1"
		echo "og-remote-open: session name contains a control character, which no tmux session name can: $quoted — pass an explicit session name instead" >&2
		exit 1
	fi
	if ! shell_quotable "$1"; then
		echo "og-remote-open: session name contains a backslash, which no remote shell dialect can quote safely: $1 — pass an explicit session name instead" >&2
		exit 1
	fi
}

# The session-name discipline applies here too: check before a caller-given
# $sess rides into the probe below, not only after a remote-derived one comes
# back.
require_session_name "$sess"

# Validated here, before it ever rides into probe_script below — not after the
# probe has already shipped it to the remote. valid_remote_path's charset also
# rejects a backslash, so this doubles as this value's shell_quotable check.
if [[ -n ${OG_REMOTE_TMPDIR:-} ]] && ! valid_remote_path "$OG_REMOTE_TMPDIR"; then
	echo "og-remote-open: unusable remote tmpdir: $OG_REMOTE_TMPDIR" >&2
	exit 1
fi

# Prints the host's most-recent session name, or nothing when the remote has no
# tmux server: list-sessions fails into `head`, so the remote pipeline still
# exits 0 with empty output. Used both inside the combined probe below and to
# re-check after a cold start — that round-trip stays separate, since the
# server didn't exist yet when the probe ran.
first_remote_session() {
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux list-sessions -F '#{session_name}' | head -1"
}

# One round-trip for everything the launcher needs before it can act: remote
# OS (tmpdir default + cold-start service manager), the tmux binary path, and
# — for whichever of session/window the caller didn't already name — the live
# session and its active window. Marker line first so a test double can
# recognize this call without parsing full shell semantics. Every
# single-quoted probe_script segment below is intentional: it's the
# remote-evaluated half of the command and must not expand locally.
# shellcheck disable=SC2016
probe_script=': og-probe;
os=$(uname -s)
uid=$(id -u)
tmux_bin=$(command -v tmux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux)'

if [[ -n ${OG_REMOTE_TMPDIR:-} ]]; then
	probe_script+="
tmpdir_lit=$(shell_quote "$OG_REMOTE_TMPDIR")
tmpdir=\"\$tmpdir_lit\""
else
	# tmux appends tmux-<uid> to $TMUX_TMPDIR itself, so both arms name the
	# PARENT of the socket dir, not the socket dir: /run/user/<uid> resolves to
	# /run/user/<uid>/tmux-<uid>/default, /tmp to /tmp/tmux-<uid>/default. macOS
	# has no $XDG_RUNTIME_DIR and its launchd startup agent sets no TMUX_TMPDIR,
	# so tmux's own default is the one to match there (#531).
	# shellcheck disable=SC2016
	probe_script+='
case "$os" in
	Darwin) tmpdir="/tmp" ;;
	*) tmpdir="/run/user/$uid" ;;
esac'
fi

if [[ -n $sess ]]; then
	probe_script+="
sess_lit=$(shell_quote "$sess")
sess=\"\$sess_lit\""
else
	# shellcheck disable=SC2016
	probe_script+='
sess=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-sessions -F '"'"'#{session_name}'"'"' | head -1)'
fi

if [[ -n $win ]]; then
	probe_script+="
win_lit=$(shell_quote "$win")
win=\"\$win_lit\""
else
	# shellcheck disable=SC2016
	probe_script+='
win=""
if [ -n "$sess" ]; then
	win=$(env TMUX_TMPDIR="$tmpdir" "$tmux_bin" list-windows -t "$sess" -F '"'"'#{window_index} #{window_active}'"'"' | awk '"'"'$2==1{print $1; exit}'"'"')
fi'
fi

# shellcheck disable=SC2016
probe_script+='
printf '"'"'os=%s\nuid=%s\ntmux=%s\ntmpdir=%s\nsess=%s\nwin=%s\n'"'"' "$os" "$uid" "$tmux_bin" "$tmpdir" "$sess" "$win"'

# ssh hands its command to the remote user's LOGIN shell, which here is fish:
# it rejects the `var=value` lines above outright, so the probe comes back empty
# behind a fish parse error on stderr. Feed the script to an explicit bash on
# stdin instead, the same way og-remote-picker already does. A fish login
# greeting can still land on stdout, which the key=value parse below ignores.
phase connect
probe_out="$(ssh -T "$host" bash -s <<<"$probe_script")"

remote_os="" remote_uid="" remote_tmux="" remote_tmpdir="" probe_sess="" probe_win=""
while IFS= read -r probe_line; do
	case "$probe_line" in
	os=*) remote_os="${probe_line#os=}" ;;
	uid=*) remote_uid="${probe_line#uid=}" ;;
	tmux=*) remote_tmux="${probe_line#tmux=}" ;;
	tmpdir=*) remote_tmpdir="${probe_line#tmpdir=}" ;;
	sess=*) probe_sess="${probe_line#sess=}" ;;
	win=*) probe_win="${probe_line#win=}" ;;
	esac
done <<<"$probe_out"
[[ -z $sess ]] && sess="$probe_sess"
[[ -z $win ]] && win="$probe_win"

# A session already live on the remote (the common case) is named here, by
# the probe above, not by the caller — so it hasn't run the check above yet.
require_session_name "$sess"

if ! valid_remote_path "$remote_tmpdir"; then
	echo "og-remote-open: unusable remote tmpdir: $remote_tmpdir" >&2
	exit 1
fi

# Starts the host's OWN startup session — the remote's tmux-startup unit
# carries its configured session name and directory, so nothing is invented
# here. Starting it blind is safe: the systemd unit is Type=forking with
# RemainAfterExit, the launchd agent is RunAtLoad, and both scripts
# exact-match `has-session` before creating anything. Callers must always
# re-probe with first_remote_session afterwards rather than trust this
# returning cleanly — unit state is not server state: a live server can sit
# behind an `inactive` unit (#287), and a dead one behind an `active` unit
# (#345). Exits the whole script on failure: a cold start is a fatal
# precondition for every caller.
start_remote_server() {
	phase start-server
	if [[ $remote_os == Darwin ]]; then
		# The launchd agent mirrors tmux-startup.service on macOS; kickstart
		# runs a RunAtLoad agent on demand.
		start_cmd=(launchctl kickstart "gui/$remote_uid/org.nix-community.home.tmux-startup")
		start_desc="tmux-startup launchd agent"
	else
		# `restart`, not `start`: RemainAfterExit keeps the unit `active` after
		# the tmux server it forked has exited, so `start` no-ops on exactly the
		# host this function exists for (#345).
		start_cmd=(systemctl --user restart tmux-startup.service)
		start_desc="tmux-startup.service"
	fi
	if ! ssh "$host" -- "${start_cmd[@]}"; then
		echo "og-remote-open: $host has no tmux server, and no $start_desc to start one" >&2
		exit 1
	fi
}

if [[ -z $sess ]]; then
	start_remote_server
	sess="$(first_remote_session)"
	if [[ -z $sess ]]; then
		echo "og-remote-open: started $start_desc on $host but no session appeared" >&2
		exit 1
	fi
	# A remote-derived name gets the same check the caller-given path already
	# ran above — this is the only route it could have skipped it. `$(…)` keeps
	# the inner newlines of a multi-line reply.
	require_session_name "$sess"
fi

# The picker's row came from a tmux-remux snapshot, not a live probe (#268):
# the named session may not exist on the remote yet. Only entered when the
# caller explicitly asked for a restore — a plain live-session attach (the
# common case) takes none of these extra round trips.
if [[ -n ${OG_REMOTE_RESTORE:-} && -n $sess ]]; then
	phase restore
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
		if [[ -z "$(first_remote_session)" ]]; then
			start_remote_server
			phase restore
		fi
		remote_remux="$(ssh "$host" 'command -v tmux-remux 2>/dev/null || echo /etc/profiles/per-user/$(id -un)/bin/tmux-remux')"
		# Bypasses the remote's own restoreMode=off gate (config/tmux.conf.nix's
		# `restore --auto`) on purpose: the user directly asked for this
		# session, not merely for the server to start.
		# tmux-remux shells out to the bare `tmux` binary name (it doesn't know
		# the store path we just resolved), so it needs that directory on its
		# PATH — the same non-interactive-ssh-PATH problem $remote_tmux above
		# already had to work around.
		# Same login-shell problem as the probe: fish expands the unquoted $PATH
		# into one argument per element, leaving `env` a PATH of one directory.
		if ! ssh -T "$host" bash -s <<<"env TMUX_TMPDIR=$remote_tmpdir PATH=$(dirname "$remote_tmux"):\$PATH $remote_remux restore"; then
			echo "og-remote-open: tmux-remux restore failed on $host" >&2
			exit 1
		fi
		# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
		if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
			# tmux-remux restore can exit 0 having restored nothing: its own
			# smart filter (idle-shells-only sessions, or sessions/snapshots
			# past its age ceiling) runs regardless of what the picker listed
			# (see the design doc's "Restore filter mismatch" section) — name
			# that as the likely cause instead of a bare "not found".
			echo "og-remote-open: session '$sess' was not restored on $host — tmux-remux's restore filter may have skipped it (idle shells / stale age)" >&2
			exit 1
		fi
	fi
fi

# Read once, here: the remote creation below and the local mirror further down
# seed their first window from the same measurement.
initial_area="$(initial_mirror_area)"
initial_width=""
initial_height=""
if [[ $initial_area =~ ^([1-9][0-9]*)[[:space:]]+([1-9][0-9]*)$ ]]; then
	initial_width="${BASH_REMATCH[1]}"
	initial_height="${BASH_REMATCH[2]}"
fi

# The picker's row was a remote zoxide directory, not a session (#356): the name
# is derived, so nothing by it exists yet. Creation lives here rather than in the
# remote-side picker so there is one creator resolving one socket dir, and so the
# session is made moments before the daemon attaches instead of having to survive
# the whole interactive pick.
if [[ -n ${OG_REMOTE_NEW_DIR:-} && -n $sess ]]; then
	phase create
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
		# Both cold-start gates above are `[[ -z $sess ]]`, and we hold a name —
		# so without this the server would be whatever a transient ssh session
		# spawned, outside the startup unit that owns it everywhere else (#345).
		if [[ -z "$(first_remote_session)" ]]; then
			start_remote_server
			phase create
		fi
		remote_size=""
		if [[ -n $initial_width ]]; then
			remote_size=" -x $initial_width -y $initial_height"
		fi
		# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
		if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux new-session -d -s $(shell_quote "$sess") -c $(shell_quote "$OG_REMOTE_NEW_DIR")$remote_size"; then
			echo "og-remote-open: could not create session '$sess' in '$OG_REMOTE_NEW_DIR' on $host" >&2
			exit 1
		fi
		# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
		if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
			echo "og-remote-open: session '$sess' was not created on $host" >&2
			exit 1
		fi
	fi
fi

if [[ -z $win ]]; then
	phase connect
	# base-index is non-zero under tmux-og (windows start at 1), so target the
	# session's active window rather than assuming index 0.
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	win="$(ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux list-windows -t $(shell_quote "$sess") -F '#{window_index} #{window_active}' | awk '\$2==1{print \$1; exit}'")"
	# A live session always has an active window, and this pipeline can't fail:
	# a failed list-windows still exits 0 into awk with the remote shell carrying
	# none of our pipefail. So empty means the session isn't there, and bridging
	# on would launch the daemon at a blank window index.
	if [[ -z $win ]]; then
		echo "og-remote-open: session '$sess' has no window on $host — it is gone or was never there" >&2
		exit 1
	fi
fi

phase mirror
# Store paths, substituted at build time. This script runs from the tmux server,
# whose PATH is frozen until a server restart, while the keybinds that reach the
# daemon repoint on a config reload alone — a bare name straddles the two, so
# the daemon can end up older than the ctl talking to it (#336).
# Unsubstituted placeholders keep their leading '@' and fall back to PATH (bats).
ctl="@bridge_ctl@"
daemon="@bridge_daemon@"
renderer="@bridge_renderer@"
reflow="@reflow@"
loading="@loading@"
[[ $ctl == @* ]] && ctl="$(command -v og-remote-bridge-ctl)"
[[ $daemon == @* ]] && daemon="$(command -v og-remote-bridge-daemon)"
[[ $renderer == @* ]] && renderer="$(command -v og-remote-bridge-renderer)"
[[ $reflow == @* ]] && reflow="$(command -v tmux-reflow-windows)"
[[ $loading == @* ]] && loading="$(command -v og-remote-loading || true)"

# The local name reaches stock `run-shell -C` menus, which re-parse it as
# commands, and every target parser, which splits on `.`/`:` — so only inert
# bytes survive (#783). Identity lives in the raw @bridge_host/@bridge_session.
mirror_name_part "$host"
host_part=$REPLY
mirror_name_part "$sess"
sess_part=$REPLY
base_local_sess="${host_part}-${sess_part}"

# The mirror of this raw pair, wherever an earlier open put it. Captured, then
# fed through a herestring: a `< <(…)` would set $!, which on_signal reaps.
# Split by hand, not `IFS='|' read`: read drops a lone trailing `|`, so a
# session named `x|` would come back as `x`. Session last, so a `|` inside it
# stays in the final field.
local_sess=""
pairs="$(tmux list-sessions -F '#{session_id}|#{@bridge_host}|#{@bridge_session}' 2>/dev/null || true)"
while IFS= read -r pair_line; do
	pair_id="${pair_line%%|*}"
	pair_rest="${pair_line#*|}"
	pair_host="${pair_rest%%|*}"
	pair_sess="${pair_rest#*|}"
	[[ $pair_host == "$host" && $pair_sess == "$sess" ]] || continue
	# A @bridge_session stored before control bytes were rejected can hold a
	# newline, forging a line above whose id names an unrelated session: act
	# only once that session's own options claim this pair.
	pair_own_host="$(tmux display-message -p -t "$pair_id" '#{@bridge_host}' 2>/dev/null || true)"
	pair_own_sess="$(tmux display-message -p -t "$pair_id" '#{@bridge_session}' 2>/dev/null || true)"
	[[ $pair_own_host == "$host" && $pair_own_sess == "$sess" ]] || continue
	pair_name="$(tmux display-message -p -t "$pair_id" '#{session_name}' 2>/dev/null || true)"
	mirror_name_part "$pair_name"
	if [[ $REPLY != "$pair_name" ]]; then
		retire_mirror "$pair_id"
	elif [[ -z $local_sess ]]; then
		local_sess="$pair_name"
	fi
done <<<"$pairs"

# No mirror of this pair yet: take the base name, or a stable suffix when it
# is occupied by something other than this host/session bridge. The pair
# clause covers a mirror created between the lookup above and here; a legacy
# mirror (host only, no @bridge_session) is adopted only when sanitizing left
# the name unchanged, since otherwise it cannot say which remote session it
# mirrors (a.b and a_b share a base name).
#
# show-options does not accept the "=" exact-match prefix has-session takes one
# line up: it answers "no such session", which -q turns into an empty string
# indistinguishable from an unset option — so these reads use the bare name
# (#474). has-session has already proved the exact name exists, and an exact
# match beats a prefix match, so the bare form cannot resolve to a sibling.
if [[ -z $local_sess ]]; then
	local_sess="$base_local_sess"
	collision=0
	while tmux has-session -t "=$local_sess" 2>/dev/null; do
		existing_bridge_host="$(tmux show-options -t "$local_sess" -qv @bridge_host 2>/dev/null || true)"
		existing_bridge_session="$(tmux show-options -t "$local_sess" -qv @bridge_session 2>/dev/null || true)"
		if [[ $existing_bridge_host == "$host" && $existing_bridge_session == "$sess" ]] ||
			[[ $existing_bridge_host == "$host" && -z $existing_bridge_session && $local_sess == "$base_local_sess" && $base_local_sess == "${host}-${sess}" ]]; then
			break
		fi
		collision=$((collision + 1))
		if ((collision == 1)); then
			local_sess="${base_local_sess}-remote"
		else
			local_sess="${base_local_sess}-remote-${collision}"
		fi
	done
fi

sock_dir="${TMUX_TMPDIR:-${XDG_RUNTIME_DIR:-/tmp}}"
sock="${sock_dir}/og-daemon-${local_sess}.sock"

# The caption the loading pane renders. Written here for the stretch before
# the daemon exists, by the daemon after that, removed by its teardown.
phase_file="${sock}.phase"

# Dedup: reuse the mirror only behind a daemon proven compatible. Any other
# proven daemon is reaped; an unreachable one goes straight to cleanup/recreate
# without signalling.
probe_daemon "$sock"
if [[ $REPLY == live ]] && tmux has-session -t "=$local_sess" 2>/dev/null; then
	attached=1
	tmux switch-client -t "=$local_sess"
	exit 0
fi
# A live daemon that got here speaks the protocol, but the mirror session it
# was serving is gone — killed from the picker, by hand, or a crash.
# switch-client above would otherwise fail against a session that no longer
# exists, so reap the orphan daemon (or the outdated one) and fall through to
# recreate.
if [[ -n $REPLY ]]; then
	daemon_pid="$(<"${sock}.pid")"
	[[ $daemon_pid =~ ^[0-9]+$ ]] && reap_daemon "$daemon_pid"
fi
mirror_created=1
# Stale cleanup: a prior daemon was killed (SIGTERM/SIGKILL) without running
# teardown, leaving socket + pidfile behind. Remove both so the new daemon can
# bind cleanly; the session below is also replaced.
rm -f "$sock" "${sock}.pid" "$phase_file"

# The <host>-<sess> session is an ephemeral mirror (the remote is the source of
# truth). Discard a pre-existing bridge — a stale bridge from a prior run, or a
# ghost resurrected by tmux-remux on restore — so it can't collide with
# new-session ("duplicate session"); =-prefix is exact-match (numeric names).
tmux kill-session -t "=$local_sess" 2>/dev/null || true

# Create the local session with a single initial window; the daemon reuses it
# for the first remote window and creates the rest.
#
# It runs the loading pane rather than a shell: switch-client below lands here
# seconds before the daemon has painted the first mirror. The daemon's
# respawn-pane for that window is what replaces it, so there is nothing extra
# to reap; a build with no loading binary on PATH falls back to the shell.
printf 'connecting to %s\n' "$host" >"$phase_file"
new_session_args=(new-session -d -s "$local_sess" -n "$sess_part")
if [[ -n $initial_width ]]; then
	new_session_args+=(-x "$initial_width" -y "$initial_height")
fi
if [[ -n $loading ]]; then
	new_session_args+=(-- "$loading" "$host" "$sess" "$phase_file")
fi
tmux "${new_session_args[@]}"

# Read by tmux-statusline to name the machine on line 0. Session-scoped, so it
# survives the daemon replacing every window under it. @bridge_session goes
# first: a concurrent walk or picker probe must never see host set, session
# empty, and take this mirror for a legacy one.
tmux set-option -t "$local_sess" @bridge_session "$sess"
tmux set-option -t "$local_sess" @bridge_host "$host"

# Pass the (remote-derived, untrusted) params through the environment instead
# of interpolating them into a shell/command string tmux/ssh would re-parse,
# so a crafted remote session name can't break out into local shell execution.
export OG_BRIDGE_HOST="$host"
export OG_BRIDGE_SESSION="$sess"
export OG_BRIDGE_WINDOW="$win"
export OG_BRIDGE_TMUX="$remote_tmux"
export OG_BRIDGE_TMPDIR="$remote_tmpdir"
export OG_DAEMON_LOCAL_SESS="$local_sess"
export OG_DAEMON_SOCK="$sock"
export OG_DAEMON_RENDERER="$renderer"
export OG_DAEMON_REFLOW="$reflow"
# This very script, so a hand-off (a remote switch-client the daemon pinned back)
# re-enters the launcher at the revision the daemon itself came from, never
# whatever a later home-manager switch left on PATH (#336).
export OG_DAEMON_REMOTE_OPEN="${BASH_SOURCE[0]}"

# The remote viewer picks its graphics backend from #{client_termname} and
# #{client_termfeatures}, which are whatever the daemon's ssh advertises — so
# hand it the identity of the terminal that will actually paint the pixels.
# The third field, #{I/f:sixel}, is tmux's own per-client sixel-capability
# interrogation (R6) — the daemon seeds its Relay from it directly rather than
# re-deriving a capability from client_termfeatures in Go. Empty (no client,
# or a control-mode client — client_termfeatures and #{I/f:sixel} are both
# empty for one) is fine: the remote then falls back to block art, which
# renders anywhere. A bare `display-message` with no -t falls back to the
# server's most-recently-used session, not necessarily this process's own
# pane — wrong on any server with more than one attached client — so target
# $TMUX_PANE explicitly, like initial_mirror_area() above already does.
term_target=()
[[ -n ${TMUX_PANE:-} ]] && term_target=(-t "$TMUX_PANE")
# Guarded like the cur_sess read below: now that this targets a specific pane
# it can fail on a stale $TMUX_PANE, and an empty identity is a valid answer
# (block art everywhere) rather than a reason to abort the launch. An explicit
# IFS split, not a two-field suffix-strip: a suffix-strip would fold the third
# field into termfeatures' tail once a field was merely appended.
term_raw="$(tmux display-message -p "${term_target[@]}" '#{client_termname}|#{client_termfeatures}|#{I/f:sixel}' 2>/dev/null || true)"
IFS='|' read -r term termfeatures sixel_flag <<<"$term_raw"
export OG_BRIDGE_TERM="$term"
export OG_BRIDGE_TERMFEATURES="$termfeatures"
export OG_BRIDGE_SIXEL="$sixel_flag"

# COLORTERM/TERM_PROGRAM (#543) ride into the INVOKING client's session via
# update-environment on attach (config/tmux.conf.nix), the same channel
# #{client_termname} reflects the outer terminal through above. Read them off
# that session, not $local_sess — the mirror session just created a few lines
# up has no attaching client yet, so it never receives an update-environment
# pass and its table is just a copy of the server's own environment.
#
# A bare `display-message` with no -t falls back to the server's
# most-recently-used session, not necessarily this process's own pane — wrong
# on any server with more than one attached client. Target $TMUX_PANE
# explicitly, like initial_mirror_area() above already does.
cur_target=()
[[ -n ${TMUX_PANE:-} ]] && cur_target=(-t "$TMUX_PANE")
cur_sess="$(tmux display-message -p "${cur_target[@]}" '#{session_name}' 2>/dev/null || true)"

# read_session_env is defined in lib-remote.sh (sourced above), REPLY-based.
# `|| true` guards each call under set -e: a miss (no session, unset name, or
# the update-environment removed marker) is an expected outcome here, not a
# script-ending error.
colorterm="" term_program=""
read_session_env "$cur_sess" COLORTERM && colorterm="$REPLY" || true
read_session_env "$cur_sess" TERM_PROGRAM && term_program="$REPLY" || true
export OG_BRIDGE_COLORTERM="$colorterm"
export OG_BRIDGE_TERM_PROGRAM="$term_program"

# The fd must not outlive the launcher: the detached daemon would otherwise
# hold the pipe's write end open for its whole life.
if [[ -n $progress_fd ]]; then
	exec {progress_fd}>&-
	progress_fd=""
fi

# Launch the daemon DETACHED, outside the panes it manages (I4): it is not the
# window's command — it respawns the local panes into renderers. setsid is
# Linux-only (not on macOS base), so fall back to plain backgrounding + disown
# where it's unavailable; either way the daemon is fully detached from this shell.
daemon_started=1
if command -v setsid >/dev/null 2>&1; then     # portable-ok: guard, verified fallback below
	setsid "$daemon" >/dev/null 2>"${sock}.log" & # portable-ok: guarded above; else branch is the verified macOS fallback
else
	# The daemon must land outside this script's process group on every
	# platform: bash job control (set -m) puts a backgrounded job in its own
	# group, so a group TERM racing the commit point below never reaches it.
	set -m
	nohup "$daemon" >/dev/null 2>"${sock}.log" &
	set +m
	disown 2>/dev/null || true
fi

attached=1
tmux switch-client -t "=$local_sess"
