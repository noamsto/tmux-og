#!/usr/bin/env bats

load helper

setup() {
	export XDG_STATE_HOME="$BATS_TEST_TMPDIR/state"
	export OG_DEBUG_SENTINEL="$BATS_TEST_TMPDIR/debug.on"
	setup_lib_log
}

@test "log_event is a no-op when the sentinel is absent" {
	log_event claude event transition from idle to processing
	[ ! -f "$OG_LOG_FILE" ]
}

@test "log_event writes a JSON line when armed" {
	: >"$OG_DEBUG_SENTINEL"
	log_event claude event transition from idle to processing
	run cat "$OG_LOG_FILE"
	[[ $output == *'"cat":"claude"'* ]]
	[[ $output == *'"event":"transition"'* ]]
	[[ $output == *'"from":"idle"'* ]]
	[[ $output == *'"to":"processing"'* ]]
}

@test "all values are quoted (numeric-looking session names stay strings)" {
	: >"$OG_DEBUG_SENTINEL"
	log_event claude sess 10 win 2
	run cat "$OG_LOG_FILE"
	[[ $output == *'"sess":"10"'* ]]
	[[ $output == *'"win":"2"'* ]]
}

@test "_json_escape handles backslash, quote, tab, newline, control chars" {
	_json_escape $'a"b\\c\td\ne\x01f'
	[ "$REPLY" = 'a\"b\\c\tdef' ]
}

@test "rotation moves the log to .1 at the cap" {
	: >"$OG_DEBUG_SENTINEL"
	export OG_LOG_MAX_BYTES=200
	for i in $(seq 1 20); do log_event t k "value-$i-padding-padding-padding-padding"; done
	[ -f "$OG_LOG_FILE.1" ]
}

@test "acquire_lock creates the lock dir" {
	# Called directly (not via `run`): the EXIT trap fires at test-end, so the
	# dir is still present for the assertion.
	local lock="$BATS_TEST_TMPDIR/x.lock"
	acquire_lock "$lock"
	[ -d "$lock" ]
}

@test "a fresh lock dir (live holder) blocks acquire" {
	local lock="$BATS_TEST_TMPDIR/held.lock"
	mkdir "$lock"
	run acquire_lock "$lock"
	[ "$status" -eq 1 ]
}

@test "acquire_lock steals a stale lock dir" {
	local lock="$BATS_TEST_TMPDIR/stale.lock"
	mkdir "$lock"
	export OG_LOCK_STALE_SECONDS=0
	run acquire_lock "$lock"
	[ "$status" -eq 0 ]
}

@test "acquire_lock clears a leftover plain file (old flock-redirect artifact)" {
	local lock="$BATS_TEST_TMPDIR/leftover.lock"
	: >"$lock"
	acquire_lock "$lock"
	[ -d "$lock" ]
}

@test "owner_only_dir accepts a caller-owned 0700 dir, REPLY 0 with no FILE" {
	local dir="$BATS_TEST_TMPDIR/owned"
	mkdir -m 700 "$dir"
	run owner_only_dir "$dir"
	[ "$status" -eq 0 ]
}

@test "owner_only_dir refuses a group/other-readable dir" {
	local dir="$BATS_TEST_TMPDIR/open"
	mkdir -m 755 "$dir"
	run owner_only_dir "$dir"
	[ "$status" -eq 1 ]
}

@test "owner_only_dir refuses a symlink standing in for the dir" {
	local real="$BATS_TEST_TMPDIR/real" link="$BATS_TEST_TMPDIR/link"
	mkdir -m 700 "$real"
	ln -s "$real" "$link"
	run owner_only_dir "$link"
	[ "$status" -eq 1 ]
}

@test "owner_only_dir sets REPLY to FILE's mtime when FILE exists" {
	local dir="$BATS_TEST_TMPDIR/stamped"
	mkdir -m 700 "$dir"
	touch "$dir/f"
	local want
	want=$(stat -c %Y "$dir/f")
	owner_only_dir "$dir" "$dir/f"
	[ "$REPLY" = "$want" ]
}

@test "owner_only_dir sets REPLY to 0 when FILE is absent, still trusts DIR" {
	local dir="$BATS_TEST_TMPDIR/absent-file"
	mkdir -m 700 "$dir"
	run owner_only_dir "$dir" "$dir/missing"
	[ "$status" -eq 0 ]
	owner_only_dir "$dir" "$dir/missing"
	[ "$REPLY" = 0 ]
}

@test "owner_only_dir never relies on stat's localized %F type string" {
	local real_stat stub
	real_stat=$(command -v stat)
	stub="$BATS_TEST_TMPDIR/stat-no-percentF"
	cat >"$stub" <<-EOF
		#!$BASH
		# Test double for a localized-coreutils stat: refuses to run with any
		# %F in its format args, standing in for gettext turning it into
		# "Verzeichnis" instead of "directory".
		for a in "\$@"; do
			case "\$a" in
			*%F*) exit 1 ;;
			esac
		done
		exec "$real_stat" "\$@"
	EOF
	chmod +x "$stub"
	# shellcheck disable=SC2034  # read by owner_only_dir, sourced from lib-log.sh
	OG_STAT="$stub"

	local dir="$BATS_TEST_TMPDIR/no-percentF"
	mkdir -m 700 "$dir"
	run owner_only_dir "$dir"
	[ "$status" -eq 0 ]
}

@test "owner_only_dir refuses a caller-owned 0600 file passed as DIR" {
	local file="$BATS_TEST_TMPDIR/plain-file"
	touch "$file"
	chmod 600 "$file"
	run owner_only_dir "$file"
	[ "$status" -eq 1 ]
}
