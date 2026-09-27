#!/usr/bin/env bats
# shellcheck disable=SC2016 # '$(…)' in single quotes is the injection payload under test

setup() {
	# shellcheck source=/dev/null
	source "${BATS_TEST_DIRNAME}/../scripts/lib-remote.sh"
}

@test "remote_daemon_alive: true for a live pid, false for dead/missing pidfile" {
	local pidfile
	pidfile="$(mktemp)"

	# Live process: write our own shell's PID.
	echo $$ >"$pidfile"
	run remote_daemon_alive "$pidfile"
	[ "$status" -eq 0 ]

	# Dead process: use a pid that doesn't exist (max pid + 1 wraps, so use a
	# large fixed value instead of relying on /proc absent pids).
	echo 4194304 >"$pidfile"
	run remote_daemon_alive "$pidfile"
	[ "$status" -ne 0 ]

	# Missing pidfile entirely.
	rm -f "$pidfile"
	run remote_daemon_alive "$pidfile"
	[ "$status" -ne 0 ]
}

@test "valid_remote_path: accepts normal absolute paths" {
	run valid_remote_path "/run/user/1000"
	[ "$status" -eq 0 ]

	run valid_remote_path "/tmp/tmux-1000"
	[ "$status" -eq 0 ]
}

@test "valid_remote_path: rejects relative, whitespace, and metacharacters" {
	run valid_remote_path "bin/x"
	[ "$status" -ne 0 ]

	run valid_remote_path "/run/user/1000 x"
	[ "$status" -ne 0 ]

	run valid_remote_path '/run/user/$(id -u)'
	[ "$status" -ne 0 ]
}

@test "shell_quotable: rejects a literal backslash, accepts everything else" {
	run shell_quotable 'a\b'
	[ "$status" -ne 0 ]

	run shell_quotable "/srv/my project (v2)"
	[ "$status" -eq 0 ]

	run shell_quotable "workstation"
	[ "$status" -eq 0 ]

	run shell_quotable "it's a test"
	[ "$status" -eq 0 ]
}

# The vectors are shared with the Go mirrorname.Part test, so the two copies of
# the mapping cannot drift apart without one suite failing.
check_mirror_name_vectors() {
	local vectors="${MIRROR_NAME_VECTORS:-$BATS_TEST_DIRNAME/../picker/mirrorname/testdata/vectors.tsv}"
	local line in want rows=0
	while IFS= read -r line; do
		in="${line%%$'\t'*}"
		want="${line#*$'\t'}"
		mirror_name_part "$in"
		if [ "$REPLY" != "$want" ]; then
			printf 'LC_ALL=%s: %q -> %q, want %q\n' "$LC_ALL" "$in" "$REPLY" "$want"
			return 1
		fi
		rows=$((rows + 1))
	done <"$vectors"
	[ "$rows" -gt 0 ]
}

@test "mirror_name_part: maps every byte outside [A-Za-z0-9_-] to _, whatever the locale" {
	LC_ALL=C.UTF-8 check_mirror_name_vectors
	LC_ALL=C check_mirror_name_vectors
}

@test "the mirror dedup reads session options with a bare target, never '=' (#474)" {
	# show-options answers "no such session" for a "=" target, and -q hides it.
	run grep -n 'show-options -t "=' "${BATS_TEST_DIRNAME}/../scripts/og-remote-open.sh"
	[ "$status" -ne 0 ]
}

# read_session_env (#543): a fake `tmux show-environment` driven by
# $FAKE_ENV_OUT/$FAKE_ENV_STATUS, matching splash.bats' stub-on-PATH pattern —
# the three outcomes tmux itself produces (value set, removed marker "-NAME",
# unknown variable) are indistinguishable from a real tmux server's, so a real
# one buys nothing here.
setup_fake_tmux() {
	STUBDIR="$(mktemp -d)"
	cat >"$STUBDIR/tmux" <<-'EOF'
		#!/bin/sh
		case "$1" in
		show-environment)
			[ "${FAKE_ENV_STATUS:-0}" = 0 ] && printf '%s\n' "$FAKE_ENV_OUT"
			exit "${FAKE_ENV_STATUS:-0}"
			;;
		esac
	EOF
	chmod +x "$STUBDIR/tmux"
	PATH="$STUBDIR:$PATH"
}

@test "read_session_env: value set returns it in REPLY" {
	setup_fake_tmux
	FAKE_ENV_OUT="COLORTERM=truecolor" FAKE_ENV_STATUS=0 \
		run bash -c 'source "$1"; read_session_env sess COLORTERM && echo "REPLY=$REPLY"' _ "${BATS_TEST_DIRNAME}/../scripts/lib-remote.sh"
	[ "$status" -eq 0 ]
	[ "$output" = "REPLY=truecolor" ]
}

@test "read_session_env: removed marker (-NAME) fails, never returned as a value" {
	setup_fake_tmux
	FAKE_ENV_OUT="-COLORTERM" FAKE_ENV_STATUS=0 \
		run bash -c 'source "$1"; read_session_env sess COLORTERM; echo "status=$? REPLY=${REPLY:-unset}"' _ "${BATS_TEST_DIRNAME}/../scripts/lib-remote.sh"
	[ "$status" -eq 0 ]
	[ "$output" = "status=1 REPLY=unset" ]
}

@test "read_session_env: unknown variable (show-environment exits non-zero) fails" {
	setup_fake_tmux
	FAKE_ENV_STATUS=1 \
		run bash -c 'source "$1"; read_session_env sess COLORTERM; echo "status=$?"' _ "${BATS_TEST_DIRNAME}/../scripts/lib-remote.sh"
	[ "$status" -eq 0 ]
	[ "$output" = "status=1" ]
}

@test "read_session_env: empty session name fails without invoking tmux" {
	run bash -c 'source "$1"; read_session_env "" COLORTERM; echo "status=$?"' _ "${BATS_TEST_DIRNAME}/../scripts/lib-remote.sh"
	[ "$status" -eq 0 ]
	[ "$output" = "status=1" ]
}
