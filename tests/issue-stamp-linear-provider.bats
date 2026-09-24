#!/usr/bin/env bats
# Tests scripts/tmux-issue-stamp-linear.sh directly (not the dispatcher-level
# fake used by tests/issue-stamp.bats): the branch-derived and explicit-key
# dispatch branches, and the sanitized 4th (error) output line.

setup() {
	export HOME="$BATS_TEST_TMPDIR"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"

	# Build a runnable lib-enrich (icon/@providers@ placeholders don't matter
	# here, but the file must still source cleanly), matching the pattern in
	# tests/issue-stamp.bats's setup().
	LIBENRICH="$BATS_TEST_TMPDIR/lib-enrich.sh"
	sed \
		-e 's/@providers@/linear github/g' \
		-e 's/@enrich_icon_linear@/L/g' \
		-e 's/@enrich_icon_github@/G/g' \
		-e 's/@enrich_icon_pending@/P/g' \
		-e 's/@enrich_icon_success@/S/g' \
		-e 's/@enrich_icon_failure@/F/g' \
		-e 's/@enrich_icon_merged@/M/g' \
		-e 's/@enrich_icon_closed@/X/g' \
		-e 's/@enrich_icon_conflict@/C/g' \
		scripts/lib-enrich.sh >"$LIBENRICH"

	PROVIDER="$BATS_TEST_TMPDIR/issue-stamp-linear.sh"
	sed \
		-e "s|@lib_enrich@|$LIBENRICH|" \
		scripts/tmux-issue-stamp-linear.sh >"$PROVIDER"

	WORKTREE="$BATS_TEST_TMPDIR/wt"
	mkdir -p "$WORKTREE"

	export PATH="$FAKEBIN:$PATH"
	export PROVIDER WORKTREE
}

# Fake `linear` CLI: `linear issue <title|url|id> [<key>]`. Fails all three
# subcommands when FAKE_LINEAR_FAIL is set; the failing `title` call writes a
# stderr line with a tab, a BEL byte, and ANSI color codes around the message.
# FAKE_LINEAR_SECRET=1 instead carries a labeled bearer token, an Authorization
# header, and a credentialed URL; FAKE_LINEAR_SECRET=2 carries a short (<20
# char) bearer token in the `Authorization: Bearer <token>` combined shape,
# which the label + value rules must not leave unmatched against each other.
write_fake_linear() {
	cat >"$FAKEBIN/linear" <<-'EOF'
		#!/bin/sh
		if [ -n "$FAKE_LINEAR_FAIL" ]; then
			if [ "$2" = "title" ]; then
				case "$FAKE_LINEAR_SECRET" in
				1) printf 'request failed: Authorization: token1234567890abcdef Bearer sk-abcdefghij0123456789 at https://user:hunter2password@api.linear.app/graphql\n' >&2 ;;
				2) printf 'request failed: Authorization: Bearer short1234tok status 401\n' >&2 ;;
				*) printf 'No API key \tconfigured\a\033[31m!!\033[0m\n' >&2 ;;
				esac
			fi
			exit 1
		fi
		case "$2" in
		title) printf 'Some Title\n' ;;
		url) printf 'https://linear.example/ENG-1957\n' ;;
		id) printf 'ENG-1957\n' ;;
		esac
	EOF
	chmod +x "$FAKEBIN/linear"
}

@test "branch-derived: failing linear CLI leaves title/url empty and emits sanitized error" {
	write_fake_linear
	FAKE_LINEAR_FAIL=1 run bash "$PROVIDER" "$WORKTREE" feat/eng-1957-x
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	[ "${lines[0]}" = "ENG-1957" ]
	[ "${lines[1]}" = "" ]
	[ "${lines[2]}" = "" ]
	[ "${lines[3]}" = "No API key configured!!" ]
}

@test "branch-derived: succeeding linear CLI emits no error" {
	write_fake_linear
	run bash "$PROVIDER" "$WORKTREE" feat/eng-1957-x
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	[ "${lines[0]}" = "ENG-1957" ]
	[ "${lines[1]}" = "Some Title" ]
	[ "${lines[2]}" = "https://linear.example/ENG-1957" ]
	[ "${lines[3]:-}" = "" ]
}

@test "explicit-key mode: failing linear CLI emits sanitized error" {
	write_fake_linear
	FAKE_LINEAR_FAIL=1 run bash "$PROVIDER" "$WORKTREE" unrelated-branch ENG-42
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	[ "${lines[0]}" = "ENG-42" ]
	[ "${lines[1]}" = "" ]
	[ "${lines[2]}" = "" ]
	[ "${lines[3]}" = "No API key configured!!" ]
}

@test "branch-derived: a bearer token, auth header, and URL credential in stderr are redacted" {
	write_fake_linear
	FAKE_LINEAR_FAIL=1 FAKE_LINEAR_SECRET=1 run bash "$PROVIDER" "$WORKTREE" feat/eng-1957-x
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	err="${lines[3]}"
	[ "$err" = "request failed: Authorization: [redacted] Bearer [redacted] at https://[redacted]@api.linear.app/graphql" ]
	case "$err" in
	*token1234567890abcdef* | *sk-abcdefghij0123456789* | *hunter2password*)
		echo "unredacted secret leaked into stamp error: $err" >&2
		return 1
		;;
	esac
}

@test "branch-derived: a short (<20 char) token in 'Authorization: Bearer <token>' is redacted" {
	write_fake_linear
	FAKE_LINEAR_FAIL=1 FAKE_LINEAR_SECRET=2 run bash "$PROVIDER" "$WORKTREE" feat/eng-1957-x
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	err="${lines[3]}"
	case "$err" in
	*short1234tok*)
		echo "unredacted short bearer token leaked into stamp error: $err" >&2
		return 1
		;;
	esac
}

@test "explicit-key mode: succeeding linear CLI emits no error" {
	write_fake_linear
	run bash "$PROVIDER" "$WORKTREE" unrelated-branch ENG-42
	[ "$status" -eq 0 ]
	mapfile -t lines <<<"$output"
	[ "${lines[0]}" = "ENG-42" ]
	[ "${lines[1]}" = "Some Title" ]
	[ "${lines[2]}" = "https://linear.example/ENG-1957" ]
	[ "${lines[3]:-}" = "" ]
}
