#!/usr/bin/env bats
# shellcheck disable=SC2030,SC2031,SC2016 # bats @test blocks run in subshells; export is intentional; '$(…)' in single quotes is the injection payload under test
# Cold-starting a serverless remote (#287). The launcher may only reach for
# tmux-startup.service when list-sessions came back empty, and must re-probe
# afterwards instead of assuming the unit produced the session it wanted —
# unit state is not server state, in either direction (#345).
#
# ssh and tmux are fakes on PATH; nothing here touches a real host or server.

setup() {
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	export SSH_LOG="$BATS_TEST_TMPDIR/ssh.log"
	export TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log"
	export CTL_LOG="$BATS_TEST_TMPDIR/ctl.log"
	# Presence of this file is the fake remote's "a tmux server is running".
	export REMOTE_SERVER="$BATS_TEST_TMPDIR/remote-server"
	export REMOTE_SESSION="workstation"
	export RESTORE_MARKER="$BATS_TEST_TMPDIR/restored"
	# Presence of this file is the fake remote's "session 'proj' exists".
	export NEWDIR_MARKER="$BATS_TEST_TMPDIR/created"
	: >"$SSH_LOG"
	: >"$TMUX_LOG"
	: >"$CTL_LOG"

	# Skips the launcher's `ssh host id -u` round-trip.
	export OG_REMOTE_TMPDIR="/run/user/1000"
	# Keeps the daemon socket + log inside the test tmpdir.
	export TMUX_TMPDIR="$BATS_TEST_TMPDIR"

	cat >"$FAKEBIN/ssh" <<-'EOF'
		#!/bin/sh
		# One marker line per invocation (independent of how many lines the
		# command itself spans) so a test can count actual ssh round-trips, not
		# just substring hits.
		printf '===SSH-CALL===\n' >>"$SSH_LOG"
		# The launcher ships multi-line scripts to an explicit `bash -s` on
		# stdin rather than to the remote login shell, so the command this
		# invocation really runs is argv plus whatever is piped in.
		cmd="$*"
		case "$cmd" in
		*"bash -s"*) cmd="$cmd
		$(cat)" ;;
		esac
		echo "$cmd" >>"$SSH_LOG"
		# The launcher's combined probe is recognized by its fixed leading no-op,
		# without actually interpreting the shell it received. Session/window
		# resolve here only when the caller didn't already name them (embedded as
		# *_lit literals).
		case "$cmd" in
		*": og-probe;"*)
			if [ -n "${FAKE_SSH_TERM:-}" ]; then
				kill -TERM "$(cat "$LAUNCHER_PID_FILE")" 2>/dev/null || true
				exit 0
			fi
			os="${FAKE_UNAME:-Linux}"
			uid=1000
			# Read the launcher's own resolution out of the probe script it
			# sent, rather than restating it here: a fake that duplicates the
			# rule under test agrees with the launcher however wrong it is (#531).
			if [ "$os" = Darwin ]; then
				tmpdir=$(printf '%s\n' "$cmd" | sed -n 's/.*Darwin) tmpdir="\([^"]*\)".*/\1/p')
			else
				tmpdir=$(printf '%s\n' "$cmd" | sed -n 's/.*\*) tmpdir="\([^"]*\)".*/\1/p')
			fi
			tmpdir=$(printf '%s\n' "$tmpdir" | sed "s/[\$]uid/$uid/")
			case "$cmd" in
			*"tmpdir_lit="*) tmpdir=$(printf '%s\n' "$cmd" | sed -n "s/.*tmpdir_lit='\([^']*\)'.*/\1/p") ;;
			esac
			sess=""
			case "$cmd" in
			*"sess_lit="*)
				sess=$(printf '%s\n' "$cmd" | sed -n "s/.*sess_lit='\([^']*\)'.*/\1/p")
				# The launcher's canonicalization step (sess_canon=…) only ships once
				# the #817 fix lands; a set FAKE_SESS_CANON simulates the remote's
				# prefix-matched #{session_name} coming back different from the literal.
				case "$cmd" in
				*"sess_canon="*) [ -n "${FAKE_SESS_CANON:-}" ] && sess="$FAKE_SESS_CANON" ;;
				esac
				;;
			*) [ -f "$REMOTE_SERVER" ] && sess="$REMOTE_SESSION" ;;
			esac
			win=""
			want=""
			case "$cmd" in
			# A caller-given index on a plain open: win is the session's ACTIVE
			# window, and want reports whether that index exists in it.
			*"want_lit="*)
				[ -n "$sess" ] && [ -z "${FAKE_NO_WINDOW:-}" ] && win=1
				[ -n "$win" ] && [ -z "${FAKE_WANT_ABSENT:-}" ] && want=1
				;;
			*"win_lit="*) win=$(printf '%s\n' "$cmd" | sed -n "s/.*win_lit='\([^']*\)'.*/\1/p") ;;
			*) [ -n "$sess" ] && [ -z "${FAKE_NO_WINDOW:-}" ] && win=1 ;;
			esac
			printf 'os=%s\nuid=%s\ntmux=%s\ntmpdir=%s\nsess=%s\nwin=%s\nwant=%s\n' "$os" "$uid" /usr/bin/tmux "$tmpdir" "$sess" "$win" "$want"
			exit 0
			;;
		esac
		case "$cmd" in
		*"command -v tmux-remux"*) echo /usr/bin/tmux-remux ;;
		*"tmux-remux restore"*)
			if [ -n "${FAKE_RESTORE_FAILS:-}" ]; then
				echo "restore: boom" >&2
				exit 1
			fi
			if [ -z "${RESTORE_TARGET_MISMATCH:-}" ]; then
				touch "$RESTORE_MARKER"
			fi
			;;
		*"has-session -t '=workstation'"*)
			[ -f "$REMOTE_SERVER" ] && exit 0
			exit 1
			;;
		*"has-session -t '=work'"*)
			[ -f "$RESTORE_MARKER" ] && exit 0
			exit 1
			;;
		*"new-session -d -s 'proj'"*)
			if [ -z "${NEWDIR_TARGET_MISMATCH:-}" ]; then
				touch "$NEWDIR_MARKER"
			fi
			;;
		*"has-session -t '=proj'"*)
			[ -f "$NEWDIR_MARKER" ] && exit 0
			exit 1
			;;
		*"systemctl --user restart"*)
			if [ -n "${FAKE_UNIT_MISSING:-}" ]; then
				echo "Failed to restart tmux-startup.service: Unit not found." >&2
				exit 1
			fi
			touch "$REMOTE_SERVER"
			;;
		# `start` against a dead server behind a RemainAfterExit=yes unit
		# systemd still calls `active`: exits 0, produces nothing (#345).
		*"systemctl --user start"*) ;;
		*"launchctl kickstart"*)
			if [ -n "${FAKE_AGENT_MISSING:-}" ]; then
				echo "Could not find service \"org.nix-community.home.tmux-startup\" in domain for gui" >&2
				exit 1
			fi
			touch "$REMOTE_SERVER"
			;;
		*list-sessions*) [ -f "$REMOTE_SERVER" ] && printf '%s\n' "${FAKE_LIST_SESSIONS_REPLY:-$REMOTE_SESSION}" ;;
		# A failed remote list-windows still exits 0 with empty stdout: the remote
		# command is a pipeline ending in awk, and carries none of our pipefail.
		*list-windows*) [ -n "${FAKE_NO_WINDOW:-}" ] || echo 1 ;;
		esac
		exit 0
	EOF

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$TMUX_LOG"
		# Simulates a TERM landing on the launcher mid-command: matched invocations
		# kill the launcher's own pid (from $LAUNCHER_PID_FILE) before returning, so
		# a test can pin the signal to an exact point in the script.
		if [ -n "${FAKE_TERM_ON:-}" ]; then
			case "$*" in
			"$FAKE_TERM_ON"*)
				kill -TERM "$(cat "$LAUNCHER_PID_FILE")" 2>/dev/null || true
				exit 0
				;;
			esac
		fi
		case "$*" in
		has-session*)
			if [ -n "${FAKE_LOCAL_SESSION:-}" ]; then
				case "$*" in
				*"=$FAKE_LOCAL_SESSION") exit 0 ;;
				*) exit 1 ;;
				esac
			fi
			if [ -n "${FAKE_DAEMON_SESSION:-}" ]; then
				case "$*" in
				*"=$FAKE_DAEMON_SESSION") exit 0 ;;
				*) exit 1 ;;
				esac
			fi
			[ -n "${FAKE_SESSION_GONE:-}" ] && exit 1
			exit 1
			;;
			display-message*)
			case "$*" in
			# Only a session-id target answers: the launcher's own cur_sess read
			# of #{session_name} targets $TMUX_PANE or nothing, never a $N id, and
			# must keep coming back empty.
			*'-t $'*'#{session_name}'*) [ -n "${FAKE_PAIR_NAME:-}" ] && printf '%s\n' "$FAKE_PAIR_NAME" ;;
			*'-t $'*'#{@bridge_sock}'*) [ -n "${FAKE_PAIR_SOCK:-}" ] && printf '%s\n' "$FAKE_PAIR_SOCK" ;;
			# A session's own pair defaults to what its FAKE_LIST_SESSIONS line
			# claims; set FAKE_PAIR_HOST/FAKE_PAIR_SESSION (even empty) to make
			# that line a forgery.
			*'-t $'*'#{@bridge_host}'*)
				if [ -n "${FAKE_PAIR_HOST+set}" ]; then
					printf '%s\n' "$FAKE_PAIR_HOST"
				else
					printf '%s\n' "${FAKE_LIST_SESSIONS:-}" | awk -F'|' -v id="$4" '$1 == id { print $2; exit }'
				fi
				;;
			*'-t $'*'#{@bridge_session}'*)
				if [ -n "${FAKE_PAIR_SESSION+set}" ]; then
					printf '%s\n' "$FAKE_PAIR_SESSION"
				else
					printf '%s\n' "${FAKE_LIST_SESSIONS:-}" | awk -v id="$4" 'index($0, id "|") == 1 { sub(/^[^|]*\|[^|]*\|/, ""); print; exit }'
				fi
				;;
			*"#{client_width} #{client_height} #{status}"*)
				[ -n "${FAKE_CLIENT_SIZE:-}" ] && printf '%s\n' "$FAKE_CLIENT_SIZE"
				;;
			esac
			;;
		show-options*)
			case "$*" in
			*"@bridge_host"*) [ -n "${FAKE_BRIDGE_HOST:-}" ] && printf '%s\n' "$FAKE_BRIDGE_HOST" ;;
			*"@bridge_session"*) [ -n "${FAKE_BRIDGE_SESSION:-}" ] && printf '%s\n' "$FAKE_BRIDGE_SESSION" ;;
			esac
			;;
		list-sessions*) [ -n "${FAKE_LIST_SESSIONS:-}" ] && printf '%s\n' "$FAKE_LIST_SESSIONS" ;;
		esac
		exit 0
	EOF

	# The launcher's PATH fallback for an unsubstituted placeholder. The shipped
	# script takes the pinned store paths instead — see the pinning case below.
	for stub in og-remote-bridge-renderer tmux-reflow-windows og-remote-bridge-daemon; do
		printf '#!/bin/sh\nexit 0\n' >"$FAKEBIN/$stub"
	done
	cat >"$FAKEBIN/og-remote-bridge-ctl" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$CTL_LOG"
		if [ -n "${FAKE_CTL_ERROR:-}" ]; then
			printf '%s\n' "$FAKE_CTL_ERROR" >&2
			exit 1
		fi
	EOF

	chmod +x "$FAKEBIN"/*
	export PATH="$FAKEBIN:$PATH"

	# Same @lib_remote@ substitution Nix does at build time.
	LAUNCHER="$BATS_TEST_TMPDIR/og-remote-open"
	# Both placeholders sit on one line (`[[ -f … ]] && source …`), so /g matters.
	sed "s|@lib_remote@|$PWD/scripts/lib-remote.sh|g" \
		scripts/og-remote-open.sh >"$LAUNCHER"
	export LAUNCHER
}

teardown() {
	if [[ -n ${DAEMON_PID:-} ]]; then
		kill "$DAEMON_PID" 2>/dev/null || true
	fi
}

# Runs the launcher backgrounded so a fake tmux/ssh invocation can signal it
# mid-run, then waits for it and sets $status like bats' own `run` does.
# Waits on `$!` rather than reading it back via $PPID: a fake invoked from a
# $(…) substitution has a subshell as its parent, not the launcher itself.
run_launcher_bg() {
	export LAUNCHER_PID_FILE="$BATS_TEST_TMPDIR/launcher.pid"
	bash "$LAUNCHER" "$@" &
	echo "$!" >"$LAUNCHER_PID_FILE"
	# `|| status=$?`, not a bare `wait`: bats runs under `set -e`, and a bare
	# non-zero exit here would abort the test before this assignment ran.
	status=0
	wait "$!" || status=$?
}

# Every case runs `bash "$LAUNCHER"` rather than executing it: the nix check
# sandbox has no /usr/bin/env, so the `#!/usr/bin/env bash` shebang cannot
# resolve there. writeShellScriptBin rewrites that shebang to a store path
# anyway, so an explicit interpreter is what the shipped script really gets.

@test "cold start: no server -> starts the unit, re-probes, bridges what it finds" {
	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q 'systemctl --user restart tmux-startup.service' "$SSH_LOG"
	# `start` would no-op against a unit systemd still calls active (#345).
	run grep -c 'systemctl --user start' "$SSH_LOG"
	[ "$status" -ne 0 ]

	# Two probes: the empty one that triggered the start, and the one after it.
	[ "$(grep -c list-sessions "$SSH_LOG")" -eq 2 ]

	# The session name came from the remote, never from the launcher.
	grep -q 'new-session -d -s tp-g6-workstation' "$TMUX_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "mirror session starts at the invoking client's content size" {
	export FAKE_CLIENT_SIZE='200 50 off'

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q 'new-session -d -s tp-g6-workstation -n workstation -x 200 -y 50' "$TMUX_LOG"
}

@test "the mirror session's first window runs the loading pane, not a shell" {
	printf '#!/bin/sh\nexit 0\n' >"$FAKEBIN/og-remote-loading"
	chmod +x "$FAKEBIN/og-remote-loading"
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q -- "new-session -d -s tp-g6-workstation -n workstation -- $FAKEBIN/og-remote-loading tp-g6 workstation $sock.phase" "$TMUX_LOG"
	# The caption the pane shows for the stretch before the daemon exists.
	grep -q 'connecting to tp-g6' "$sock.phase"
}

@test "no loading binary resolves: the initial window falls back to a shell" {
	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	run grep -c -- 'new-session .*--' "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "new dir: the remote session is created at the invoking client's content size" {
	# Without -x/-y the remote gives it default-size (80x24) and anything the
	# shell autostarts sees 80 columns until the daemon's converge lands.
	touch "$REMOTE_SERVER"
	export OG_REMOTE_NEW_DIR=/srv/proj
	export FAKE_CLIENT_SIZE='200 50 off'

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	grep -q "new-session -d -s 'proj' -c '/srv/proj' -x 200 -y 50" "$SSH_LOG"
}

@test "new dir: an unmeasurable client leaves the remote size to tmux" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_NEW_DIR=/srv/proj

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	grep -q "new-session -d -s 'proj' -c '/srv/proj'$" "$SSH_LOG"
}

@test "an ordinary local session survives a colliding mirror name" {
	export REMOTE_SESSION=config
	export FAKE_LOCAL_SESSION=nix-config

	run bash "$LAUNCHER" nix
	[ "$status" -eq 0 ]

	grep -q 'new-session -d -s nix-config-remote -n config' "$TMUX_LOG"
	run ! grep -q 'kill-session -t =nix-config$' "$TMUX_LOG"
}

@test "cold start: a host with no startup unit fails by name and bridges nothing" {
	export FAKE_UNIT_MISSING=1

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 1 ]
	[[ $output == *"no tmux-startup.service"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "cold start: a host that already has a session is never cold-started" {
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	run grep -c systemctl "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "live compatible daemon is reused only after its ping succeeds" {
	touch "$REMOTE_SERVER"
	export FAKE_DAEMON_SESSION=tp-g6-workstation FAKE_BRIDGE_HOST=tp-g6
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q -- "--sock $sock ping _" "$CTL_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	kill -0 "$DAEMON_PID"
}

@test "live daemon is reaped and the mirror recreated when its session is gone" {
	touch "$REMOTE_SERVER"
	export FAKE_SESSION_GONE=1
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q -- "--sock $sock ping _" "$CTL_LOG"
	run kill -0 "$DAEMON_PID"
	[ "$status" -ne 0 ]
	grep -q 'new-session -d -s tp-g6-workstation' "$TMUX_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "live incompatible daemon is terminated and replaced" {
	touch "$REMOTE_SERVER"
	export FAKE_CTL_ERROR='og-remote-bridge-ctl: ctl protocol version "2", this daemon speaks "1" — reopen the bridge'
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q -- "--sock $sock ping _" "$CTL_LOG"
	grep -q 'new-session -d -s tp-g6-workstation' "$TMUX_LOG"
	run kill -0 "$DAEMON_PID"
	[ "$status" -ne 0 ]
}

@test "substituted bridge binaries win over the ones on PATH" {
	touch "$REMOTE_SERVER"

	# Pinned copies stand in for the store paths Nix substitutes; setup()'s PATH
	# stubs stay in place as what a stale tmux server would reach instead.
	local pinned="$BATS_TEST_TMPDIR/pinned"
	mkdir -p "$pinned"
	export PINNED_CTL_LOG="$BATS_TEST_TMPDIR/pinned-ctl.log"
	export PINNED_DAEMON_ENV="$BATS_TEST_TMPDIR/pinned-daemon.env"
	cat >"$pinned/ctl" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$PINNED_CTL_LOG"
		exit 1
	EOF
	cat >"$pinned/daemon" <<-'EOF'
		#!/bin/sh
		printf '%s\n%s\n' "$OG_DAEMON_RENDERER" "$OG_DAEMON_REFLOW" >"$PINNED_DAEMON_ENV"
	EOF
	printf '#!/bin/sh\nexit 0\n' >"$pinned/renderer"
	printf '#!/bin/sh\nexit 0\n' >"$pinned/reflow"
	chmod +x "$pinned"/*

	# Same substitution Nix does at build time, for all five placeholders.
	local launcher="$BATS_TEST_TMPDIR/og-remote-open-pinned"
	sed -e "s|@lib_remote@|$PWD/scripts/lib-remote.sh|g" \
		-e "s|@bridge_ctl@|$pinned/ctl|g" \
		-e "s|@bridge_daemon@|$pinned/daemon|g" \
		-e "s|@bridge_renderer@|$pinned/renderer|g" \
		-e "s|@reflow@|$pinned/reflow|g" \
		scripts/og-remote-open.sh >"$launcher"

	# A live pid forces the probe; the pinned ctl fails it, so the launcher also
	# has to reach the recreate path with the pinned daemon.
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run bash "$launcher" tp-g6
	[ "$status" -eq 0 ]

	grep -q -- "--sock $sock ping _" "$PINNED_CTL_LOG"
	[ ! -s "$CTL_LOG" ] # the PATH ctl was never consulted

	# The renderer/reflow the spawned daemon was handed are pinned too — those
	# are what mirror panes respawn into.
	local waited=0
	while [[ ! -s $PINNED_DAEMON_ENV && $waited -lt 50 ]]; do
		sleep 0.1
		waited=$((waited + 1))
	done
	run cat "$PINNED_DAEMON_ENV"
	[ "${lines[0]}" = "$pinned/renderer" ]
	[ "${lines[1]}" = "$pinned/reflow" ]
}

@test "unreachable bridge socket never signals a recycled live pid" {
	touch "$REMOTE_SERVER"
	export FAKE_CTL_ERROR='og-remote-bridge-ctl: bridge daemon unreachable: connect: connection refused'
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -q 'new-session -d -s tp-g6-workstation' "$TMUX_LOG"
	kill -0 "$DAEMON_PID"
}

@test "cold start: an explicit session argument skips both the probe and the unit" {
	run bash "$LAUNCHER" tp-g6 scratch
	[ "$status" -eq 0 ]

	run grep -cE 'list-sessions|systemctl' "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q 'switch-client -t =tp-g6-scratch' "$TMUX_LOG"
}

@test "darwin cold start: kickstarts the launchd agent, re-probes, bridges" {
	export FAKE_UNAME=Darwin
	unset OG_REMOTE_TMPDIR

	run bash "$LAUNCHER" mbp
	[ "$status" -eq 0 ]

	grep -q 'launchctl kickstart gui/1000/org.nix-community.home.tmux-startup' "$SSH_LOG"
	# macOS socket dir's parent, never the Linux one and never the doubled
	# tmux-<uid> segment (#531).
	grep -q 'TMUX_TMPDIR=/tmp ' "$SSH_LOG"
	run grep -c 'TMUX_TMPDIR=/tmp/tmux-' "$SSH_LOG"
	[ "$status" -ne 0 ]
	run grep -c 'TMUX_TMPDIR=/run/user' "$SSH_LOG"
	[ "$status" -ne 0 ]
	# Two probes: the empty one that triggered the kickstart, and the one after.
	[ "$(grep -c list-sessions "$SSH_LOG")" -eq 2 ]

	grep -q 'new-session -d -s mbp-workstation' "$TMUX_LOG"
	grep -q 'switch-client -t =mbp-workstation' "$TMUX_LOG"
}

@test "darwin cold start: a missing launchd agent fails by name and bridges nothing" {
	export FAKE_UNAME=Darwin FAKE_AGENT_MISSING=1
	unset OG_REMOTE_TMPDIR

	run bash "$LAUNCHER" mbp
	[ "$status" -eq 1 ]
	[[ $output == *"no tmux-startup launchd agent"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "darwin host with a live server is never cold-started" {
	export FAKE_UNAME=Darwin
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" mbp
	[ "$status" -eq 0 ]

	run grep -cE 'launchctl|systemctl' "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q 'switch-client -t =mbp-workstation' "$TMUX_LOG"
}

@test "restore: requested session isn't live -> cold starts, restores, bridges" {
	export OG_REMOTE_RESTORE=1

	run bash "$LAUNCHER" tp-g6 work
	[ "$status" -eq 0 ]

	grep -q 'systemctl --user restart tmux-startup.service' "$SSH_LOG"
	grep -q "has-session -t '=work'" "$SSH_LOG"
	grep -q 'tmux-remux restore' "$SSH_LOG"
	# Guards against dropping the PATH= that lets tmux-remux find the bare
	# `tmux` binary it execs — the fake `command -v tmux` above resolves to
	# /usr/bin/tmux, so the restore command must carry /usr/bin on PATH.
	grep -q 'PATH=/usr/bin:.*tmux-remux restore' "$SSH_LOG"
	grep -q 'new-session -d -s tp-g6-work' "$TMUX_LOG"
	grep -q 'switch-client -t =tp-g6-work' "$TMUX_LOG"
}

@test "restore: server already running but session missing -> restores without a cold start" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_RESTORE=1

	run bash "$LAUNCHER" tp-g6 work
	[ "$status" -eq 0 ]

	run grep -c systemctl "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q "has-session -t '=work'" "$SSH_LOG"
	grep -q 'tmux-remux restore' "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-work' "$TMUX_LOG"
}

@test "restore: tmux-remux restore failing surfaces an error and bridges nothing" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_RESTORE=1
	export FAKE_RESTORE_FAILS=1

	run bash "$LAUNCHER" tp-g6 work
	[ "$status" -eq 1 ]
	[[ $output == *"restore failed"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "restore: session absent even after a successful restore fails loudly" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_RESTORE=1
	export RESTORE_TARGET_MISMATCH=1

	run bash "$LAUNCHER" tp-g6 work
	[ "$status" -eq 1 ]
	[[ $output == *"tmux-remux's restore filter may have skipped it"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "new dir: session absent and no server -> cold starts, then creates it" {
	export OG_REMOTE_NEW_DIR="/srv/my proj"

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	# Neither of the two -z $sess cold-start gates fires with a name in hand, so
	# without this branch's own gate the server would be an ssh session's.
	grep -q 'systemctl --user restart tmux-startup.service' "$SSH_LOG"
	grep -q "new-session -d -s 'proj' -c '/srv/my proj'" "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-proj' "$TMUX_LOG"
}

@test "new dir: server already running -> creates without a cold start" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_NEW_DIR=/srv/proj

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	run grep -c systemctl "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q "new-session -d -s 'proj' -c '/srv/proj'" "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-proj' "$TMUX_LOG"
}

@test "new dir: a session that already exists is bridged, not recreated" {
	touch "$REMOTE_SERVER" "$NEWDIR_MARKER"
	export OG_REMOTE_NEW_DIR=/srv/proj

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	run grep -c new-session "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q 'switch-client -t =tp-g6-proj' "$TMUX_LOG"
}

@test "new dir: session absent even after a successful create fails loudly" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_NEW_DIR=/srv/proj
	export NEWDIR_TARGET_MISMATCH=1

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 1 ]
	[[ $output == *"was not created"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "new dir: combined with a restore is rejected before any round trip" {
	export OG_REMOTE_NEW_DIR=/srv/proj
	export OG_REMOTE_RESTORE=1

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 1 ]
	[[ $output == *"mutually exclusive"* ]]

	[ ! -s "$SSH_LOG" ]
	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "a session with no active window fails instead of opening a blank mirror" {
	touch "$REMOTE_SERVER"
	export FAKE_NO_WINDOW=1

	run bash "$LAUNCHER" tp-g6 ghost
	[ "$status" -eq 1 ]
	[[ $output == *"ghost"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "restore: a live session attach with the flag unset is unaffected" {
	touch "$REMOTE_SERVER"
	# OG_REMOTE_RESTORE intentionally unset.

	run bash "$LAUNCHER" tp-g6 workstation
	[ "$status" -eq 0 ]

	# The combined probe now carries the exact-match has-session that resolves
	# the caller's own name (#817), so the plain attach's "unaffected" invariant
	# is the round-trip count: one ssh call, no restore machinery behind it.
	[ "$(grep -c '===SSH-CALL===' "$SSH_LOG")" -eq 1 ]
	run grep -c 'tmux-remux' "$SSH_LOG"
	[ "$status" -ne 0 ]
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "bad OG_REMOTE_TMPDIR is rejected before bridging" {
	export OG_REMOTE_TMPDIR="/run/user/1000 x"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 1 ]
	[[ $output == *"unusable remote tmpdir"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	# Rejected before the value ever rides into the combined probe — not merely
	# before the daemon launches.
	[ ! -s "$SSH_LOG" ]

	export OG_REMOTE_TMPDIR='/run/user/$(id -u)'

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 1 ]
	[[ $output == *"unusable remote tmpdir"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	[ ! -s "$SSH_LOG" ]
}

@test "OG_REMOTE_NEW_DIR containing a backslash is rejected before any round trip" {
	export OG_REMOTE_NEW_DIR='/srv/pro\ject'

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 1 ]
	[[ $output == *"backslash"* ]]

	[ ! -s "$SSH_LOG" ]
}

@test "a session name containing a backslash is rejected before list-windows ever runs" {
	# An explicit sess arg skips both cold-start gates regardless of
	# REMOTE_SERVER, so the only shell_quote("$sess") call this path reaches is
	# list-windows — assert it never gets built.
	run bash "$LAUNCHER" tp-g6 'wor\kstation'
	[ "$status" -eq 1 ]
	[[ $output == *"backslash"* ]]

	run grep -c list-windows "$SSH_LOG"
	[ "$status" -ne 0 ]
}

@test "a session name containing a backslash is rejected before the restore path's has-session ever runs" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_RESTORE=1

	run bash "$LAUNCHER" tp-g6 'wor\kstation'
	[ "$status" -eq 1 ]
	[[ $output == *"backslash"* ]]

	run grep -c has-session "$SSH_LOG"
	[ "$status" -ne 0 ]
}

@test "shell_quote is plain POSIX single-quoting: correct for quotes, backslash-bearing input is unchanged (rejected upstream, not doubled)" {
	# shellcheck disable=SC1090
	eval "$(sed -n '/^shell_quote()/,/^}/p' "$LAUNCHER")"

	local input quoted result

	# Single-quote correctness, round-tripped under sh — the case shell_quote
	# still has to get right in every dialect.
	input="it's/a path"
	quoted="$(shell_quote "$input")"
	result="$(sh -c "printf '%s' $quoted")"
	[ "$result" = "$input" ]

	# A literal backslash now passes through inert (no longer doubled):
	# backslash-bearing values are rejected at the launcher's entry before they
	# ever reach shell_quote (see the rejection tests below), so this guards
	# only against reintroducing backslash-doubling here directly.
	quoted="$(shell_quote 'a\b')"
	[ "$quoted" = "'a\\b'" ]

	# fish round-trips the same single-quote case identically — `nix flake
	# check`'s sandbox has no fish (see flake.nix's remote-tests
	# nativeBuildInputs), so this leg only strengthens local runs; it must never
	# skip the sh assertions above, which are what the CI gate actually relies
	# on.
	if command -v fish >/dev/null 2>&1; then
		quoted="$(shell_quote "$input")"
		result="$(fish -c "echo $quoted")"
		[ "$result" = "$input" ]
	fi
}

# The launcher makes at most one combined ssh probe (resolving whichever of
# session/window the caller didn't already name) before ever reaching the
# daemon. These assert that, for each combination of what the caller knows.

# ssh gives its command string to the remote user's LOGIN shell, which on these
# hosts is fish. fish rejects the probe's `var=value` lines outright, so putting
# the script in argv returns an empty probe behind a parse error on stderr — not
# an ssh failure, so the launcher only notices later, on the unusable values.
@test "the probe script rides stdin into bash, never the remote login shell" {
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	# Line 1 of the entry is argv (what the login shell parses), the rest is
	# stdin (what bash parses).
	local argv script
	argv="$(sed -n '2p' "$SSH_LOG")"
	script="$(sed -n '3,$p' "$SSH_LOG")"
	[[ $argv == *"-T tp-g6 bash -s" ]]
	[[ $script == ": og-probe;"* ]]

	# fish is absent from `nix flake check`'s sandbox (see flake.nix's
	# remote-tests nativeBuildInputs), so this leg only strengthens local runs;
	# the argv assertion above is what the CI gate relies on.
	if command -v fish >/dev/null 2>&1; then
		fish -n <<<"$argv"
	fi
}

@test "combined probe: neither session nor window given -> exactly one ssh call before the daemon" {
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	[ "$(grep -c '===SSH-CALL===' "$SSH_LOG")" -eq 1 ]
	grep -q ': og-probe;' "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "combined probe: session given, window not -> exactly one ssh call before the daemon" {
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" tp-g6 workstation
	[ "$status" -eq 0 ]

	[ "$(grep -c '===SSH-CALL===' "$SSH_LOG")" -eq 1 ]
	grep -q "sess_lit='workstation'" "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "combined probe: session and window both given -> exactly one ssh call before the daemon" {
	run bash "$LAUNCHER" tp-g6 workstation 3
	[ "$status" -eq 0 ]

	[ "$(grep -c '===SSH-CALL===' "$SSH_LOG")" -eq 1 ]
	grep -q "sess_lit='workstation'" "$SSH_LOG"
	grep -q "want_lit='3'" "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

# Waits for the backgrounded stub daemon to record the window it was handed.
wait_for_daemon_window() {
	local waited=0
	while [[ ! -s $DAEMON_WINDOW_LOG && $waited -lt 50 ]]; do
		sleep 0.1
		waited=$((waited + 1))
	done
}

install_window_logging_daemon() {
	export DAEMON_WINDOW_LOG="$BATS_TEST_TMPDIR/daemon-window.log"
	cat >"$FAKEBIN/og-remote-bridge-daemon" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "$OG_BRIDGE_WINDOW" >>"$DAEMON_WINDOW_LOG"
	EOF
}

@test "caller-given window: a gone session fails instead of opening a blank mirror" {
	export FAKE_NO_WINDOW=1

	run bash "$LAUNCHER" tp-g6 workstation 3
	[ "$status" -eq 1 ]
	[[ $output == *"session 'workstation' has no window"* ]]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "caller-given window: an index that exists in the session is the one the daemon gets" {
	install_window_logging_daemon

	run bash "$LAUNCHER" tp-g6 workstation 3
	[ "$status" -eq 0 ]

	wait_for_daemon_window
	[ "$(cat "$DAEMON_WINDOW_LOG")" = 3 ]
}

@test "caller-given window: an index absent from the session falls back to the active window" {
	install_window_logging_daemon
	export FAKE_WANT_ABSENT=1

	run bash "$LAUNCHER" tp-g6 workstation 3
	[ "$status" -eq 0 ]

	wait_for_daemon_window
	[ "$(cat "$DAEMON_WINDOW_LOG")" = 1 ]
}

@test "caller-given window: the probe resolves the session exactly, with no prefix fallback" {
	run bash "$LAUNCHER" tp-g6 workstation 3
	[ "$status" -eq 0 ]

	run ! grep -q sess_canon "$SSH_LOG"
}

@test "combined probe: OG_REMOTE_TMPDIR unset still resolves in one ssh call" {
	unset OG_REMOTE_TMPDIR
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	[ "$(grep -c '===SSH-CALL===' "$SSH_LOG")" -eq 1 ]
	grep -q 'switch-client -t =tp-g6-workstation' "$TMUX_LOG"
}

@test "a session name with spaces survives the combined probe end-to-end" {
	run bash "$LAUNCHER" tp-g6 'my session'
	[ "$status" -eq 0 ]

	# The whole name rides through shell_quote as one single-quoted literal,
	# never split on the embedded space.
	grep -q "sess_lit='my session'" "$SSH_LOG"
	grep -q 'switch-client -t =tp-g6-my_session' "$TMUX_LOG"
}

@test "a prefix-matched session name is canonicalized before the daemon sees it (#817)" {
	# The remote holds only 'api-main'; the daemon attaches EXACTLY, so a
	# prefix name here must become the real one or the first attach refuses.
	touch "$REMOTE_SERVER"
	export FAKE_SESS_CANON=api-main

	run bash "$LAUNCHER" h api
	[ "$status" -eq 0 ]

	grep -q "sess_lit='api'" "$SSH_LOG"
	grep -qxF 'set-option -t h-api-main @bridge_session api-main' "$TMUX_LOG"
	grep -qxF 'switch-client -t =h-api-main' "$TMUX_LOG"
}

@test "new dir: a prefix sibling never stands in for the session to create (#817)" {
	# The remote holds only 'proj-main'. A create's session does not exist yet
	# by design, so canonicalizing 'proj' by prefix match would skip the create
	# and mirror the sibling.
	touch "$REMOTE_SERVER"
	export OG_REMOTE_NEW_DIR=/srv/proj
	export FAKE_SESS_CANON=proj-main

	run bash "$LAUNCHER" tp-g6 proj
	[ "$status" -eq 0 ]

	grep -q "new-session -d -s 'proj' -c '/srv/proj'" "$SSH_LOG"
	grep -qxF 'switch-client -t =tp-g6-proj' "$TMUX_LOG"
	run ! grep -q 'proj-main' "$TMUX_LOG"
}

@test "restore: a prefix sibling never stands in for the session to restore (#817)" {
	# The remote has a live 'work-old'; canonicalizing 'work' onto it would
	# skip the restore and open the sibling.
	touch "$REMOTE_SERVER"
	export OG_REMOTE_RESTORE=1
	export FAKE_SESS_CANON=work-old

	run bash "$LAUNCHER" tp-g6 work
	[ "$status" -eq 0 ]

	grep -q "has-session -t '=work'" "$SSH_LOG"
	grep -q 'tmux-remux restore' "$SSH_LOG"
	grep -qxF 'switch-client -t =tp-g6-work' "$TMUX_LOG"
	run ! grep -q 'work-old' "$TMUX_LOG"
}

# --- #783: the local mirror name is sanitized; the raw pair is the identity ---

@test "a ##-substitution session name opens under a sanitized local name, raw in @bridge_session" {
	touch "$REMOTE_SERVER"
	export REMOTE_SESSION='x##(touch S)'

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -qxF 'new-session -d -s tp-g6-x___touch_S_ -n x___touch_S_' "$TMUX_LOG"
	grep -qxF 'set-option -t tp-g6-x___touch_S_ @bridge_session x##(touch S)' "$TMUX_LOG"
	grep -qxF 'switch-client -t =tp-g6-x___touch_S_' "$TMUX_LOG"

	local sess_line host_line
	sess_line="$(grep -n '^set-option .* @bridge_session ' "$TMUX_LOG" | head -1 | cut -d: -f1)"
	host_line="$(grep -n '^set-option .* @bridge_host ' "$TMUX_LOG" | head -1 | cut -d: -f1)"
	[ -n "$sess_line" ] && [ -n "$host_line" ]
	[ "$sess_line" -lt "$host_line" ]
}

@test "a quote break-out session name from the probe opens under a sanitized local name" {
	touch "$REMOTE_SERVER"
	export REMOTE_SESSION="x' '' ; run-shell 'touch S' ; display-menu -T 'y"
	local want=tp-g6-x_______run-shell__touch_S____display-menu_-T__y

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	grep -qxF "new-session -d -s $want -n ${want#tp-g6-}" "$TMUX_LOG"
	grep -qxF "set-option -t $want @bridge_session $REMOTE_SESSION" "$TMUX_LOG"
	grep -qxF "switch-client -t =$want" "$TMUX_LOG"
}

@test "a dotted user@host is sanitized in the local name, raw in @bridge_host" {
	touch "$REMOTE_SERVER"

	run bash "$LAUNCHER" user@tp.lan
	[ "$status" -eq 0 ]

	grep -qxF 'new-session -d -s user_tp_lan-workstation -n workstation' "$TMUX_LOG"
	grep -qxF 'set-option -t user_tp_lan-workstation @bridge_host user@tp.lan' "$TMUX_LOG"
	grep -qxF 'switch-client -t =user_tp_lan-workstation' "$TMUX_LOG"
}

@test "a legacy mirror is not adopted when sanitizing altered the remote name" {
	# a.b and a_b sanitize alike, and a legacy mirror (no @bridge_session)
	# cannot say which of the two it mirrors.
	export FAKE_LOCAL_SESSION=tp-g6-a_b FAKE_BRIDGE_HOST=tp-g6

	run bash "$LAUNCHER" tp-g6 a.b
	[ "$status" -eq 0 ]

	grep -qxF 'new-session -d -s tp-g6-a_b-remote -n a_b' "$TMUX_LOG"
	run grep -qxF 'kill-session -t =tp-g6-a_b' "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "a legacy mirror is still adopted when its remote name needed no sanitizing" {
	export FAKE_LOCAL_SESSION=tp-g6-a_b FAKE_BRIDGE_HOST=tp-g6

	run bash "$LAUNCHER" tp-g6 a_b
	[ "$status" -eq 0 ]

	grep -qxF 'new-session -d -s tp-g6-a_b -n a_b' "$TMUX_LOG"
}

@test "a mirror found by its raw pair keeps its own name, not the first free one" {
	export FAKE_LIST_SESSIONS='$7|tp-g6|a.b' FAKE_PAIR_NAME=tp-g6-a_b-remote-2

	run bash "$LAUNCHER" tp-g6 a.b
	[ "$status" -eq 0 ]

	grep -qxF 'new-session -d -s tp-g6-a_b-remote-2 -n a_b' "$TMUX_LOG"
	grep -qxF 'switch-client -t =tp-g6-a_b-remote-2' "$TMUX_LOG"
	run grep -qF 'kill-session -t $7' "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "a pre-#783 raw-named mirror is retired, its unproven pidfile pid left alone" {
	export FAKE_LIST_SESSIONS='$9|tp-g6|x##(y)' FAKE_PAIR_NAME='tp-g6-x#(y)'
	export FAKE_PAIR_SOCK="$BATS_TEST_TMPDIR/old.sock"
	export FAKE_CTL_ERROR='og-remote-bridge-ctl: bridge daemon unreachable: connect: connection refused'
	sleep 30 &
	DAEMON_PID=$!
	printf '%s\n' "$DAEMON_PID" >"$FAKE_PAIR_SOCK.pid"
	: >"$FAKE_PAIR_SOCK"

	run bash "$LAUNCHER" tp-g6 'x##(y)'
	[ "$status" -eq 0 ]

	grep -qxF -- "--sock $FAKE_PAIR_SOCK ping _" "$CTL_LOG"
	grep -qxF 'kill-session -t $9' "$TMUX_LOG"
	kill -0 "$DAEMON_PID"
	[ ! -e "$FAKE_PAIR_SOCK" ]
	[ ! -e "$FAKE_PAIR_SOCK.pid" ]
	grep -qxF 'new-session -d -s tp-g6-x___y_ -n x___y_' "$TMUX_LOG"
}

@test "a pre-#783 raw-named mirror is retired, its daemon reaped once ping proves ownership" {
	export FAKE_LIST_SESSIONS='$9|tp-g6|x##(y)' FAKE_PAIR_NAME='tp-g6-x#(y)'
	export FAKE_PAIR_SOCK="$BATS_TEST_TMPDIR/old.sock"
	sleep 30 &
	DAEMON_PID=$!
	printf '%s\n' "$DAEMON_PID" >"$FAKE_PAIR_SOCK.pid"

	run bash "$LAUNCHER" tp-g6 'x##(y)'
	[ "$status" -eq 0 ]

	grep -qxF -- "--sock $FAKE_PAIR_SOCK ping _" "$CTL_LOG"
	grep -qxF 'kill-session -t $9' "$TMUX_LOG"
	run kill -0 "$DAEMON_PID"
	[ "$status" -ne 0 ]
	[ ! -e "$FAKE_PAIR_SOCK.pid" ]
	grep -qxF 'new-session -d -s tp-g6-x___y_ -n x___y_' "$TMUX_LOG"
}

@test "a pre-#783 raw-named mirror is retired, its daemon reaped on an old-protocol reply" {
	export FAKE_LIST_SESSIONS='$9|tp-g6|x##(y)' FAKE_PAIR_NAME='tp-g6-x#(y)'
	export FAKE_PAIR_SOCK="$BATS_TEST_TMPDIR/old.sock"
	export FAKE_CTL_ERROR='og-remote-bridge-ctl: ctl protocol version "2", this daemon speaks "1" — reopen the bridge'
	sleep 30 &
	DAEMON_PID=$!
	printf '%s\n' "$DAEMON_PID" >"$FAKE_PAIR_SOCK.pid"

	run bash "$LAUNCHER" tp-g6 'x##(y)'
	[ "$status" -eq 0 ]

	grep -qxF -- "--sock $FAKE_PAIR_SOCK ping _" "$CTL_LOG"
	grep -qxF 'kill-session -t $9' "$TMUX_LOG"
	run kill -0 "$DAEMON_PID"
	[ "$status" -ne 0 ]
	[ ! -e "$FAKE_PAIR_SOCK.pid" ]
}

@test "a cold-started remote's multi-line session name is rejected before any local session exists" {
	export FAKE_LIST_SESSIONS_REPLY='evil
$0|tp-g6|main'

	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 1 ]
	[[ $output == *"session name contains a control character"* ]]

	run grep -cE 'new-session|kill-session|set-option' "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

@test "a caller-given session name containing a newline is rejected before any round trip" {
	run bash "$LAUNCHER" tp-g6 'wo
rk'
	[ "$status" -eq 1 ]
	[[ $output == *"session name contains a control character"* ]]

	[ ! -s "$SSH_LOG" ]
}

# A @bridge_session stored before control bytes were rejected can carry a
# newline, forging a list-sessions line that names another session's id.
@test "a forged pair line never reuses the unrelated session it names" {
	export FAKE_LIST_SESSIONS='$5|tp-g6|evil
$0|tp-g6|main' FAKE_PAIR_NAME=work FAKE_PAIR_HOST='' FAKE_PAIR_SESSION=''

	run bash "$LAUNCHER" tp-g6 main
	[ "$status" -eq 0 ]

	run grep -E 'kill-session -t (=work|\$0)$' "$TMUX_LOG"
	[ "$status" -ne 0 ]
	grep -qxF 'new-session -d -s tp-g6-main -n main' "$TMUX_LOG"
}

@test "a forged pair line never retires the unrelated session it names" {
	export FAKE_LIST_SESSIONS='$5|tp-g6|evil
$0|tp-g6|main' FAKE_PAIR_NAME='my.proj' FAKE_PAIR_HOST='' FAKE_PAIR_SESSION=''

	run bash "$LAUNCHER" tp-g6 main
	[ "$status" -eq 0 ]

	run grep -E 'kill-session -t (=my\.proj|\$0)$' "$TMUX_LOG"
	[ "$status" -ne 0 ]
	grep -qxF 'new-session -d -s tp-g6-main -n main' "$TMUX_LOG"
}

# --- #770: progress fd phase lines (opt-in, gated by OG_REMOTE_OPEN_PROGRESS_FD) ---

@test "progress: cold start emits connect, start-server, connect (late window lookup), mirror" {
	export OG_REMOTE_OPEN_PROGRESS_FD=3
	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ "$(cat "$BATS_TEST_TMPDIR/phases")" = "$(printf 'connect\nstart-server\nconnect\nmirror\n')" ]
}

@test "progress: warm attach emits connect, mirror" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_OPEN_PROGRESS_FD=3
	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ "$(cat "$BATS_TEST_TMPDIR/phases")" = "$(printf 'connect\nmirror\n')" ]
}

@test "progress: restore emits connect, restore, start-server, restore, mirror" {
	export OG_REMOTE_RESTORE=1
	export OG_REMOTE_OPEN_PROGRESS_FD=3
	run bash "$LAUNCHER" tp-g6 work 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ "$(cat "$BATS_TEST_TMPDIR/phases")" = "$(printf 'connect\nrestore\nstart-server\nrestore\nmirror\n')" ]
}

@test "progress: new dir emits connect, create, start-server, create, mirror" {
	export OG_REMOTE_NEW_DIR=/srv
	export OG_REMOTE_OPEN_PROGRESS_FD=3
	run bash "$LAUNCHER" tp-g6 proj 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ "$(cat "$BATS_TEST_TMPDIR/phases")" = "$(printf 'connect\ncreate\nstart-server\ncreate\nmirror\n')" ]
}

@test "progress: no active window emits connect twice and fails" {
	touch "$REMOTE_SERVER"
	export FAKE_NO_WINDOW=1
	export OG_REMOTE_OPEN_PROGRESS_FD=3
	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 1 ]

	[ "$(cat "$BATS_TEST_TMPDIR/phases")" = "$(printf 'connect\nconnect\n')" ]
}

@test "progress: with the env var unset, the fd stays untouched" {
	touch "$REMOTE_SERVER"
	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ ! -s "$BATS_TEST_TMPDIR/phases" ]
}

@test "progress: a non-numeric fd value is ignored, with no bad-fd noise on stderr" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_OPEN_PROGRESS_FD=abc
	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	[ ! -s "$BATS_TEST_TMPDIR/phases" ]
	[[ $output != *"Bad file descriptor"* ]]
}

@test "progress: an fd number with nothing open there is silently ignored" {
	touch "$REMOTE_SERVER"
	export OG_REMOTE_OPEN_PROGRESS_FD=9
	run bash "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]
	[ -z "$output" ]
}

@test "progress: the fd var is never passed on to the daemon" {
	touch "$REMOTE_SERVER"
	export DAEMON_ENV_LOG="$BATS_TEST_TMPDIR/daemon-env"
	cat >"$FAKEBIN/og-remote-bridge-daemon" <<-'EOF'
		#!/bin/sh
		printf '%s\n' "${OG_REMOTE_OPEN_PROGRESS_FD-unset}" >"$DAEMON_ENV_LOG"
	EOF
	chmod +x "$FAKEBIN/og-remote-bridge-daemon"
	export OG_REMOTE_OPEN_PROGRESS_FD=3

	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	local waited=0
	while [[ ! -s $DAEMON_ENV_LOG && $waited -lt 20 ]]; do
		sleep 0.1
		waited=$((waited + 1))
	done
	[ "$(cat "$DAEMON_ENV_LOG")" = unset ]
}

@test "progress: fd 3 is closed before the daemon launches" {
	touch "$REMOTE_SERVER"
	export DAEMON_FD3_LOG="$BATS_TEST_TMPDIR/daemon-fd3"
	cat >"$FAKEBIN/og-remote-bridge-daemon" <<-'EOF'
		#!/bin/sh
		if [ -e /dev/fd/3 ]; then
			echo open >"$DAEMON_FD3_LOG"
		else
			echo closed >"$DAEMON_FD3_LOG"
		fi
	EOF
	chmod +x "$FAKEBIN/og-remote-bridge-daemon"
	export OG_REMOTE_OPEN_PROGRESS_FD=3

	run bash "$LAUNCHER" tp-g6 3>"$BATS_TEST_TMPDIR/phases"
	[ "$status" -eq 0 ]

	local waited=0
	while [[ ! -s $DAEMON_FD3_LOG && $waited -lt 20 ]]; do
		sleep 0.1
		waited=$((waited + 1))
	done
	[ "$(cat "$DAEMON_FD3_LOG")" = closed ]
}

# --- #770: TERM/INT/HUP rollback (only undoes a mirror the script itself built,
# and only up to the commit point — the final switch-client of either path) ---

@test "signal: TERM after the mirror exists but before switch-client rolls it back" {
	touch "$REMOTE_SERVER"
	export FAKE_TERM_ON='set-option -t tp-g6-workstation @bridge_session'
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"

	run_launcher_bg tp-g6
	[ "$status" -eq 143 ]

	local new_line
	new_line="$(grep -n '^new-session' "$TMUX_LOG" | head -1 | cut -d: -f1)"
	[ -n "$new_line" ]
	sed -n "${new_line},\$p" "$TMUX_LOG" | grep -q 'kill-session -t =tp-g6-workstation'

	run grep -c switch-client "$TMUX_LOG"
	[ "$status" -ne 0 ]
	[ ! -e "$sock" ]
	[ ! -e "${sock}.pid" ]
	[ ! -e "${sock}.phase" ]
}

@test "signal: TERM at the final switch-client is a completed attach, not a rollback" {
	export FAKE_TERM_ON='switch-client'

	run_launcher_bg tp-g6
	[ "$status" -eq 0 ]

	local new_line
	new_line="$(grep -n '^new-session' "$TMUX_LOG" | head -1 | cut -d: -f1)"
	[ -n "$new_line" ]
	run bash -c "sed -n '${new_line},\$p' '$TMUX_LOG' | grep -c 'kill-session -t =tp-g6-workstation'"
	[ "$status" -ne 0 ]
}

@test "signal: TERM at switch-client on the dedup path leaves the reused daemon untouched" {
	touch "$REMOTE_SERVER"
	export FAKE_DAEMON_SESSION=tp-g6-workstation FAKE_BRIDGE_HOST=tp-g6
	export FAKE_TERM_ON='switch-client'
	sleep 30 &
	DAEMON_PID=$!
	local sock="$TMUX_TMPDIR/og-daemon-tp-g6-workstation.sock"
	printf '%s\n' "$DAEMON_PID" >"${sock}.pid"

	run_launcher_bg tp-g6
	[ "$status" -eq 0 ]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	run grep -c kill-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	kill -0 "$DAEMON_PID"
}

@test "signal: TERM during the initial probe unwinds with nothing to roll back" {
	export FAKE_SSH_TERM=1

	run_launcher_bg tp-g6
	[ "$status" -eq 143 ]

	run grep -c new-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
	run grep -c kill-session "$TMUX_LOG"
	[ "$status" -ne 0 ]
}

# --- #770: the nohup/set -m fallback, taken only when setsid can't be found ---

@test "nohup fallback: daemon lands in its own process group without setsid" {
	touch "$REMOTE_SERVER"

	local nosetsid="$BATS_TEST_TMPDIR/nosetsid"
	mkdir -p "$nosetsid"
	for tool in rm nohup dirname sleep cat sed touch cut tr printf mkdir ps; do
		local real
		real="$(command -v "$tool" 2>/dev/null)" || continue
		ln -s "$real" "$nosetsid/$tool"
	done
	local restricted_path="$FAKEBIN:$nosetsid"

	# Confirms the fallback below actually exercises the no-setsid branch,
	# rather than agreeing with a PATH that still has the real one on it.
	local setsid_probe
	setsid_probe="$(PATH="$restricted_path" command -v setsid || true)"
	[ -z "$setsid_probe" ]

	# Records its own pid and process-group id, then exits: leading its own
	# group is what a group TERM aimed at the launcher can no longer reach.
	export DAEMON_PGID_FILE="$BATS_TEST_TMPDIR/daemon-pgid"
	cat >"$FAKEBIN/og-remote-bridge-daemon" <<-'EOF'
		#!/bin/sh
		if [ -r /proc/$$/stat ]; then
			read -r _ _ _ _ pgid _ </proc/$$/stat
		else
			pgid=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ')
		fi
		printf '%s %s\n' "$$" "${pgid:-NOPGID}" >"$DAEMON_PGID_FILE"
	EOF
	chmod +x "$FAKEBIN/og-remote-bridge-daemon"

	PATH="$restricted_path" run "$BASH" "$LAUNCHER" tp-g6
	[ "$status" -eq 0 ]

	local waited=0
	while [[ ! -s $DAEMON_PGID_FILE && $waited -lt 20 ]]; do
		sleep 0.1
		waited=$((waited + 1))
	done
	[ -s "$DAEMON_PGID_FILE" ]

	local pid pgid
	read -r pid pgid <"$DAEMON_PGID_FILE"
	[ "$pid" = "$pgid" ]
}
