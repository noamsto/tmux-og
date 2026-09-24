#!/usr/bin/env bats
# Tests tmux-issue-stamp.sh's dispatch (branch-derived vs explicit-id mode) and
# its per-window lock (#137 conflict safety), with tmux + the providers faked.

setup() {
	export HOME="$BATS_TEST_TMPDIR" # keep git off any real user config
	STATE="$BATS_TEST_TMPDIR/state"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STATE" "$FAKEBIN"
	export FAKE_TMUX_STATE="$STATE"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/lock"

	# Fake tmux: answers display-message (#{window_id}/#{session_name}) and
	# show-options from state; records every set-option (both -w OPT VAL and
	# the -wu OPT unset form) to $STATE/setlog + mirrors into $STATE/opt_*.
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		st="$FAKE_TMUX_STATE"
		case "$1" in
		display-message)
			case "$5" in
			*window_id*) printf '@1' ;;
			*session_name*) printf 'sess' ;;
			esac
			;;
		show-options)
			[ -f "$st/opt_$5" ] && cat "$st/opt_$5"
			;;
		set-option)
			if [ "$4" = "-wu" ]; then
				rm -f "$st/opt_$5"
				echo "unset $5" >>"$st/setlog"
			else
				printf '%s' "$6" >"$st/opt_$5"
				echo "$5=$6" >>"$st/setlog"
			fi
			;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"

	# Fake providers: record argv, emit canned id/title/url. Branch-derived path
	# (no 3rd arg) only matches a "feat/eng-1957-x"-style branch; explicit mode
	# (3rd arg present) always resolves, mirroring the real scripts' contract.
	cat >"$FAKEBIN/issue-stamp-linear" <<-'EOF'
		#!/bin/sh
		echo "linear $*" >>"$FAKE_TMUX_STATE/providerlog"
		if [ -n "$3" ]; then
			printf '%s\nExplicit Title\nhttps://linear.example/%s\n' "$3" "$3"
		else
			case "$2" in
			*eng-1957*) printf 'ENG-1957\nBranch Title\nhttps://linear.example/ENG-1957\n' ;;
			*eng-9001*) printf 'ENG-9001\n\n\nsome error\n' ;;
			*) printf '\n\n\n' ;;
			esac
		fi
	EOF
	chmod +x "$FAKEBIN/issue-stamp-linear"

	cat >"$FAKEBIN/issue-stamp-github" <<-'EOF'
		#!/bin/sh
		echo "github $*" >>"$FAKE_TMUX_STATE/providerlog"
		if [ -n "$3" ]; then
			printf '#%s\nExplicit GH Title\nhttps://github.example/issues/%s\n' "$3" "$3"
		else
			printf '\n\n\n'
		fi
	EOF
	chmod +x "$FAKEBIN/issue-stamp-github"

	cat >"$FAKEBIN/reflow" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$FAKE_TMUX_STATE/reflowlog"
	EOF
	chmod +x "$FAKEBIN/reflow"

	cat >"$FAKEBIN/pr-enrich" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$FAKE_TMUX_STATE/prlog"
	EOF
	chmod +x "$FAKEBIN/pr-enrich"

	# Build a runnable stamp script: lib-enrich (with @providers@ + icon
	# placeholders stubbed, like tests/helper.bash's setup_lib_enrich) and the
	# provider/reflow/pr-enrich placeholders pointed at the fakes above.
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

	STAMP="$BATS_TEST_TMPDIR/issue-stamp.sh"
	sed \
		-e "s|@lib_enrich@|$LIBENRICH|" \
		-e "s|@lib_log@|$PWD/scripts/lib-log.sh|" \
		-e "s|@issue_stamp_linear@|$FAKEBIN/issue-stamp-linear|" \
		-e "s|@issue_stamp_github@|$FAKEBIN/issue-stamp-github|" \
		-e "s|@reflow@|$FAKEBIN/reflow|" \
		-e "s|@pr_enrich@|$FAKEBIN/pr-enrich|" \
		scripts/tmux-issue-stamp.sh >"$STAMP"

	export PATH="$FAKEBIN:$PATH"
	export STAMP
}

# Poll for a file the disowned background calls (reflow/pr-enrich) write.
wait_for() {
	for _ in $(seq 1 40); do
		[[ -f $1 ]] && return 0
		sleep 0.05
	done
	return 1
}

@test "branch-derived: matching branch stamps linear, kicks reflow + PR fetch" {
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@issue_provider")" = "linear" ]
	[ "$(cat "$STATE/opt_@issue_id")" = "ENG-1957" ]
	[ "$(cat "$STATE/opt_@issue_title")" = "Branch Title" ]
	wait_for "$STATE/reflowlog"
	wait_for "$STATE/prlog"
}

@test "branch-derived: no match clears a stale stamp" {
	printf 'linear' >"$STATE/opt_@issue_provider"
	printf 'ENG-1' >"$STATE/opt_@issue_id"
	run bash "$STAMP" sess:1 /repo unrelated-branch
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/opt_@issue_provider" ]
	[ ! -f "$STATE/opt_@issue_id" ]
	grep -q 'unset @issue_id' "$STATE/setlog"
}

@test "branch-derived: no match still kicks a PR fetch when worktree is non-empty" {
	run bash "$STAMP" sess:1 /repo unrelated-branch
	[ "$status" -eq 0 ]
	wait_for "$STATE/prlog"
	grep -q -- '--force' "$STATE/prlog"
}

@test "branch-derived: no match skips the PR fetch when worktree is empty" {
	run bash "$STAMP" sess:1 "" unrelated-branch
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/prlog" ]
}

@test "explicit-id mode: GH-<number> resolves via github, skipping branch derivation" {
	run bash "$STAMP" sess:1 /repo unrelated-branch GH-42
	[ "$status" -eq 0 ]
	grep -q '^github .* 42$' "$STATE/providerlog"
	[ "$(cat "$STATE/opt_@issue_provider")" = "github" ]
	[ "$(cat "$STATE/opt_@issue_id")" = "#42" ]
}

@test "explicit-id mode: bare key resolves via linear with the id passed directly" {
	run bash "$STAMP" sess:1 /repo unrelated-branch eng-9
	[ "$status" -eq 0 ]
	grep -q '^linear .* ENG-9$' "$STATE/providerlog"
	[ "$(cat "$STATE/opt_@issue_provider")" = "linear" ]
	[ "$(cat "$STATE/opt_@issue_id")" = "ENG-9" ]
	[ "$(cat "$STATE/opt_@issue_title")" = "Explicit Title" ]
}

@test "explicit-id mode: a malformed id clears the stamp instead of falling back to the branch" {
	printf 'linear' >"$STATE/opt_@issue_provider"
	printf 'ENG-1' >"$STATE/opt_@issue_id"
	# Branch would resolve via derivation (matches *eng-1957*) if the malformed
	# id were allowed to silently fall through — it must not.
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x "GH-"
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/opt_@issue_provider" ]
	[ ! -f "$STATE/opt_@issue_id" ]
	# The malformed id never reached a provider script at all.
	[ ! -f "$STATE/providerlog" ]
}

@test "bridge window: never stamped" {
	printf '1' >"$STATE/opt_@bridge_win"
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/setlog" ]
	[ ! -f "$STATE/providerlog" ]
}

@test "lock: a held lock makes a concurrent fire a clean no-op" {
	mkdir -p "$OG_ENRICH_LOCK_DIR"
	mkdir "$OG_ENRICH_LOCK_DIR/1.lock"
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/setlog" ]
	[ ! -f "$STATE/providerlog" ]
}

@test "lock: a held lock is logged, not a silent drop, when debug is armed" {
	export XDG_STATE_HOME="$BATS_TEST_TMPDIR/xdg-state"
	export OG_DEBUG_SENTINEL="$BATS_TEST_TMPDIR/debug.on"
	: >"$OG_DEBUG_SENTINEL"
	mkdir -p "$OG_ENRICH_LOCK_DIR"
	mkdir "$OG_ENRICH_LOCK_DIR/1.lock"
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	grep -q 'stamp_skip_locked' "$XDG_STATE_HOME/og/events.log"
}

@test "lock: released after a run, so a later fire proceeds normally" {
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ ! -d "$OG_ENRICH_LOCK_DIR/1.lock" ]
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@issue_id")" = "ENG-1957" ]
}

@test "repeated fires are idempotent: re-stamping the same branch yields the same options" {
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	first="$(cat "$STATE/opt_@issue_id")"
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	second="$(cat "$STATE/opt_@issue_id")"
	[ "$first" = "$second" ]
	[ "$second" = "ENG-1957" ]
}

@test "branch-derived: a partial stamp with a provider error stores @issue_stamp_error" {
	run bash "$STAMP" sess:1 /repo feat/eng-9001-x
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@issue_id")" = "ENG-9001" ]
	[ "$(cat "$STATE/opt_@issue_stamp_error")" = "some error" ]
}

@test "branch-derived: a later successful stamp clears @issue_stamp_error" {
	run bash "$STAMP" sess:1 /repo feat/eng-9001-x
	[ "$(cat "$STATE/opt_@issue_stamp_error")" = "some error" ]
	run bash "$STAMP" sess:1 /repo feat/eng-1957-x
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/opt_@issue_stamp_error" ]
	grep -q 'unset @issue_stamp_error' "$STATE/setlog"
}
