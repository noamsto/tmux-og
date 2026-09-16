#!/usr/bin/env bash
# Create a local <host>-<sess> session and launch the M2 multi-window bridge
# daemon detached: it enumerates every remote window and mirrors each into its
# own local window (live add/close/rename/active-changed). Resolves remote
# tmux path + TMUX_TMPDIR for the ssh control connection.
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

host="$1"
sess="${2:-}"
win="${3:-}"

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

# The session-name quoting discipline (shell_quote is unsafe for a value
# containing a backslash) applies here too: check before a caller-given $sess
# rides into the probe below, not only after a remote-derived one comes back.
if [[ -n $sess ]] && ! shell_quotable "$sess"; then
	echo "og-remote-open: session name contains a backslash, which no remote shell dialect can quote safely: $sess — pass an explicit session name instead" >&2
	exit 1
fi

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
# the probe above, not by the caller — so it hasn't run the backslash check
# above yet.
if [[ -n $sess ]] && ! shell_quotable "$sess"; then
	echo "og-remote-open: session name contains a backslash, which no remote shell dialect can quote safely: $sess — pass an explicit session name instead" >&2
	exit 1
fi

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
	# A remote-derived name gets the same backslash check the caller-given path
	# already ran above — this is the only route it could have skipped it.
	if ! shell_quotable "$sess"; then
		echo "og-remote-open: session name contains a backslash, which no remote shell dialect can quote safely: $sess — pass an explicit session name instead" >&2
		exit 1
	fi
fi

# The picker's row came from a tmux-remux snapshot, not a live probe (#268):
# the named session may not exist on the remote yet. Only entered when the
# caller explicitly asked for a restore — a plain live-session attach (the
# common case) takes none of these extra round trips.
if [[ -n ${OG_REMOTE_RESTORE:-} && -n $sess ]]; then
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
		if [[ -z "$(first_remote_session)" ]]; then
			start_remote_server
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
	# shellcheck disable=SC2029 # intentional: expand client-side, resolved values ride in the remote command
	if ! ssh "$host" "env TMUX_TMPDIR=$remote_tmpdir $remote_tmux has-session -t $(shell_quote "=$sess")" 2>/dev/null; then
		# Both cold-start gates above are `[[ -z $sess ]]`, and we hold a name —
		# so without this the server would be whatever a transient ssh session
		# spawned, outside the startup unit that owns it everywhere else (#345).
		if [[ -z "$(first_remote_session)" ]]; then
			start_remote_server
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

base_local_sess="${host}-${sess}"
local_sess="$base_local_sess"

# Keep a real local session with the old deterministic name. New mirrors use a
# stable suffix only when that name is occupied by something other than this
# host/session bridge. @bridge_session makes the choice unambiguous even when
# either side contains a hyphen; the host-only fallback keeps old mirrors
# reusable while they are upgraded.
#
# show-options does not accept the "=" exact-match prefix has-session takes one
# line up: it answers "no such session", which -q turns into an empty string
# indistinguishable from an unset option — so these reads use the bare name
# (#474). has-session has already proved the exact name exists, and an exact
# match beats a prefix match, so the bare form cannot resolve to a sibling.
collision=0
while tmux has-session -t "=$local_sess" 2>/dev/null; do
	existing_bridge_host="$(tmux show-options -t "$local_sess" -qv @bridge_host 2>/dev/null || true)"
	existing_bridge_session="$(tmux show-options -t "$local_sess" -qv @bridge_session 2>/dev/null || true)"
	if [[ $existing_bridge_host == "$host" && $existing_bridge_session == "$sess" ]] ||
		[[ $existing_bridge_host == "$host" && -z $existing_bridge_session && $local_sess == "$base_local_sess" ]]; then
		break
	fi
	collision=$((collision + 1))
	if ((collision == 1)); then
		local_sess="${base_local_sess}-remote"
	else
		local_sess="${base_local_sess}-remote-${collision}"
	fi
done

sock_dir="${TMUX_TMPDIR:-${XDG_RUNTIME_DIR:-/tmp}}"
sock_name="${local_sess//[^A-Za-z0-9._-]/_}"
sock="${sock_dir}/og-daemon-${sock_name}.sock"
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

# The caption the loading pane renders. Written here for the stretch before
# the daemon exists, by the daemon after that, removed by its teardown.
phase_file="${sock}.phase"

# Dedup: a live pid alone is not enough. A config reload can leave a daemon
# that speaks an older ctl protocol behind, so prove its compatibility before
# reusing the mirror. ctl bounds the probe at two seconds.
if remote_daemon_alive "${sock}.pid"; then
	if probe_error="$("$ctl" --sock "$sock" ping _ 2>&1)"; then
		if tmux has-session -t "=$local_sess" 2>/dev/null; then
			tmux switch-client -t "=$local_sess"
			exit 0
		fi
		# The daemon is alive and speaks the protocol, but the mirror session it
		# was serving is gone — killed from the picker, by hand, or a crash.
		# switch-client above would otherwise fail against a session
		# that no longer exists, so reap the orphan daemon and fall through to
		# recreate.
		daemon_pid="$(<"${sock}.pid")"
		[[ $daemon_pid =~ ^[0-9]+$ ]] && reap_daemon "$daemon_pid"
	# A stale pidfile can be recycled by an unrelated process. Only the daemon's
	# deterministic old-protocol replies establish that the PID owns this socket;
	# an unreachable socket goes straight to cleanup/recreate without signalling.
	# Matched on the suffix both replies share: pinning the version digits stops
	# reaping the daemon a later bump obsoletes.
	elif [[ $probe_error == *'— reopen the bridge'* ]]; then
		daemon_pid="$(<"${sock}.pid")"
		[[ $daemon_pid =~ ^[0-9]+$ ]] && reap_daemon "$daemon_pid"
	fi
fi
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
new_session_args=(new-session -d -s "$local_sess" -n "$sess")
if [[ -n $initial_width ]]; then
	new_session_args+=(-x "$initial_width" -y "$initial_height")
fi
if [[ -n $loading ]]; then
	new_session_args+=(-- "$loading" "$host" "$sess" "$phase_file")
fi
tmux "${new_session_args[@]}"

# Read by tmux-statusline to name the machine on line 0. Session-scoped, so it
# survives the daemon replacing every window under it.
tmux set-option -t "$local_sess" @bridge_host "$host"
tmux set-option -t "$local_sess" @bridge_session "$sess"

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
# Empty (no client, or a control-mode client — client_termfeatures is always
# empty for one) is fine: the remote then falls back to block art, which
# renders anywhere. A bare `display-message` with no -t falls back to the
# server's most-recently-used session, not necessarily this process's own
# pane — wrong on any server with more than one attached client — so target
# $TMUX_PANE explicitly, like initial_mirror_area() above already does.
term_target=()
[[ -n ${TMUX_PANE:-} ]] && term_target=(-t "$TMUX_PANE")
# Guarded like the cur_sess read below: now that this targets a specific pane
# it can fail on a stale $TMUX_PANE, and an empty identity is a valid answer
# (block art everywhere) rather than a reason to abort the launch.
term_raw="$(tmux display-message -p "${term_target[@]}" '#{client_termname}|#{client_termfeatures}' 2>/dev/null || true)"
term="${term_raw%%|*}"
termfeatures="${term_raw#*|}"
export OG_BRIDGE_TERM="$term"
export OG_BRIDGE_TERMFEATURES="$termfeatures"

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

# Launch the daemon DETACHED, outside the panes it manages (I4): it is not the
# window's command — it respawns the local panes into renderers. setsid is
# Linux-only (not on macOS base), so fall back to plain backgrounding + disown
# where it's unavailable; either way the daemon is fully detached from this shell.
if command -v setsid >/dev/null 2>&1; then     # portable-ok: guard, verified fallback below
	setsid "$daemon" >/dev/null 2>"${sock}.log" & # portable-ok: guarded above; else branch is the verified macOS fallback
else
	nohup "$daemon" >/dev/null 2>"${sock}.log" &
	disown 2>/dev/null || true
fi

tmux switch-client -t "=$local_sess"
