#!/usr/bin/env bats
bats_require_minimum_version 1.5.0 # run !
# Tests tmux-issue-stamp.sh's --backfill/--backfill-run sweep (#599): a window
# stuck with an id but no title/url (a transient provider failure) gets
# re-stamped automatically, bounded by an attempt cap, without wiping a valid
# explicit-id or bridge-window stamp along the way.
#
# Deliberately NOT reusing tests/issue-stamp.bats's fake tmux: that fake
# hardcodes window_id to "@1" for every target and keys option state on the
# option name alone (ignoring -t), so every synthetic window there shares one
# namespace and one lock. A backfill sweep needs several distinct windows in
# one run, so this fake derives a per-target key from -t/$3 and namespaces
# window ids, option state, and locks by it. Targets used here are plain
# alnum tokens (w1, w2, ...) rather than realistic "$session:@window" shapes,
# since tmux-issue-stamp.sh only ever passes the target through opaquely as
# `-t "$target"` — it never parses it — and a plain token keeps the fake's key
# derivation (tr -c 'A-Za-z0-9_' '_') the identity function, so option files
# are named directly after the target with no separate key-mapping step.

setup() {
	export HOME="$BATS_TEST_TMPDIR" # keep git off any real user config
	STATE="$BATS_TEST_TMPDIR/state"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STATE" "$FAKEBIN"
	export FAKE_TMUX_STATE="$STATE"
	export OG_ENRICH_LOCK_DIR="$BATS_TEST_TMPDIR/lock"
	export OG_ENRICH_CACHE_DIR="$BATS_TEST_TMPDIR/cache"
	unset TMUX # stamps are suffixed per server when set; these tests expect none

	# Fake tmux: list-windows -a -F <fmt> cats a fixture file the test writes
	# ($STATE/windowlist) verbatim — it doesn't interpret the format string,
	# since every test controls both sides of that contract directly. Every
	# other call (display-message/show-options/set-option) uses the -t/$3
	# target directly as the option-state key (see file header on why targets
	# are kept alnum-only) so distinct targets get distinct window ids, option
	# namespaces, and lock dirs.
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		st="$FAKE_TMUX_STATE"
		case "$1" in
		list-windows)
			cat "$st/windowlist" 2>/dev/null
			;;
		display-message)
			case "$5" in
			*window_id*) printf '@%s' "$3" ;;
			*session_name*) printf 'sess' ;;
			esac
			;;
		show-options)
			[ -f "$st/opt_$3_$5" ] && cat "$st/opt_$3_$5"
			;;
		set-option)
			if [ "$4" = "-wu" ]; then
				rm -f "$st/opt_$3_$5"
				echo "unset $3 $5" >>"$st/setlog"
			else
				printf '%s' "$6" >"$st/opt_$3_$5"
				echo "$3 $5=$6" >>"$st/setlog"
			fi
			;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"

	# Fake providers: branch-pattern-matched canned responses, same style as
	# issue-stamp.bats. *now-eng-* branches resolve fully (simulating a
	# backfill that now succeeds); *stuck-eng-* branches resolve only the id,
	# never title/url (simulating a permanently-broken CLI, to prove the cap
	# works); explicit mode (3rd arg present) always resolves via github.
	cat >"$FAKEBIN/issue-stamp-linear" <<-'EOF'
		#!/bin/sh
		echo "linear $*" >>"$FAKE_TMUX_STATE/providerlog"
		if [ -n "$3" ]; then
			printf '\n\n\n'
		else
			case "$2" in
			*now-eng-*) printf 'NOW-ENG\nBackfilled Title\nhttps://linear.example/NOW-ENG\n' ;;
			*stuck-eng-*) printf 'STUCK-ENG\n\n\n' ;;
			*) printf '\n\n\n' ;;
			esac
		fi
	EOF
	chmod +x "$FAKEBIN/issue-stamp-linear"

	cat >"$FAKEBIN/issue-stamp-github" <<-'EOF'
		#!/bin/sh
		echo "github $*" >>"$FAKE_TMUX_STATE/providerlog"
		if [ -n "$3" ]; then
			case "$3" in
			999) printf '#%s\n\n\n' "$3" ;; # id resolves, title/url still stuck (partial explicit id)
			*) printf '#%s\nExplicit GH Title\nhttps://github.example/issues/%s\n' "$3" "$3" ;;
			esac
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
	# run_backfill_pass execs "${BASH_SOURCE[0]}" directly (both for the
	# per-window recursion and for --backfill's own `detach` re-invocation),
	# so this needs the exec bit — unlike issue-stamp.bats, which only ever
	# invokes its copy via `bash "$STAMP" ...` and never hits this path.
	chmod +x "$STAMP"
	# The source file's shebang is `#!/usr/bin/env bash`, which relies on
	# /usr/bin/env existing — true on a dev machine, false inside the Nix
	# build sandbox (no such path is mapped in). The real build never hits
	# this: pkgs.writeShellScriptBin bakes an absolute store-path shebang, so
	# a self-exec always resolves. Rewrite the shebang here to match that —
	# an absolute path to the bash actually running this test — rather than
	# working around a gap that's specific to this raw-script harness.
	sed -i "1s|^#!.*|#!$(command -v bash)|" "$STAMP"

	export PATH="$FAKEBIN:$PATH"
	export STAMP
}

# seed_window TARGET ID TITLE_SET URL_SET BRANCH EXPLICIT_ID WORKTREE TRIES
# Pre-populates the fake tmux option state for one synthetic window, matching
# what a prior tmux-issue-stamp.sh write would have left behind. TITLE_SET and
# URL_SET are "1" or "" (this only ever tests presence, not the sweep's own
# text handling — that's covered by issue-stamp.bats).
seed_window() {
	local t="$1" id="$2" title_set="$3" url_set="$4" branch="$5" explicit="$6" wt="$7" tries="$8"
	[ -n "$id" ] && printf '%s' "$id" >"$STATE/opt_${t}_@issue_id"
	[ -n "$title_set" ] && printf 'Existing Title' >"$STATE/opt_${t}_@issue_title"
	[ -n "$url_set" ] && printf 'https://existing.example' >"$STATE/opt_${t}_@issue_url"
	[ -n "$branch" ] && printf '%s' "$branch" >"$STATE/opt_${t}_@issue_branch"
	[ -n "$explicit" ] && printf '%s' "$explicit" >"$STATE/opt_${t}_@issue_explicit_id"
	[ -n "$wt" ] && printf '%s' "$wt" >"$STATE/opt_${t}_@worktree"
	[ -n "$tries" ] && printf '%s' "$tries" >"$STATE/opt_${t}_@issue_backfill_tries"
}

# windowlist_line TARGET ID TITLE_SET URL_SET BRANCH EXPLICIT_ID WORKTREE BRIDGE TRIES
# Emits one -F-formatted fixture line matching the exact field order
# run_backfill_pass parses:
# tgt|id|has_title|has_url|branch|explicit_id|worktree|git_root|bridge_win|tries
windowlist_line() {
	printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
		"$1" "$2" "$3" "$4" "$5" "$6" "$7" "$7" "${8:-}" "${9:-}"
}

@test "backfill: a partial window that now resolves gets re-stamped and tries cleared" {
	mkdir -p "$BATS_TEST_TMPDIR/repo1"
	seed_window w1 NOW-ENG "" "" now-eng-99-x "" "$BATS_TEST_TMPDIR/repo1" 0
	windowlist_line w1 NOW-ENG "" "" now-eng-99-x "" "$BATS_TEST_TMPDIR/repo1" "" 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	grep -q 'linear .*now-eng-99-x' "$STATE/providerlog"
	[ "$(cat "$STATE/opt_w1_@issue_title")" = "Backfilled Title" ]
	[ "$(cat "$STATE/opt_w1_@issue_url")" = "https://linear.example/NOW-ENG" ]
	[ ! -f "$STATE/opt_w1_@issue_backfill_tries" ]
}

@test "backfill: a window at the tries cap is never re-invoked" {
	mkdir -p "$BATS_TEST_TMPDIR/repo2"
	seed_window w2 STUCK-ENG "" "" stuck-eng-2-x "" "$BATS_TEST_TMPDIR/repo2" 5
	windowlist_line w2 STUCK-ENG "" "" stuck-eng-2-x "" "$BATS_TEST_TMPDIR/repo2" "" 5 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/providerlog" ]
	[ ! -f "$STATE/opt_w2_@issue_title" ]
}

@test "backfill: a bridge window is never swept even if partial" {
	mkdir -p "$BATS_TEST_TMPDIR/repo3"
	seed_window w3 SOME-ID "" "" some-branch "" "$BATS_TEST_TMPDIR/repo3" 0
	windowlist_line w3 SOME-ID "" "" some-branch "" "$BATS_TEST_TMPDIR/repo3" 1 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/providerlog" ]
}

@test "backfill: a window with both title and url already set is never swept" {
	mkdir -p "$BATS_TEST_TMPDIR/repo4"
	seed_window w4 SOME-ID 1 1 some-branch "" "$BATS_TEST_TMPDIR/repo4" 0
	windowlist_line w4 SOME-ID 1 1 some-branch "" "$BATS_TEST_TMPDIR/repo4" "" 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/providerlog" ]
}

@test "backfill: an explicit-id window is re-invoked in explicit mode and keeps its id" {
	mkdir -p "$BATS_TEST_TMPDIR/repo5"
	seed_window w5 GH-7 "" "" main GH-7 "$BATS_TEST_TMPDIR/repo5" 0
	windowlist_line w5 GH-7 "" "" main GH-7 "$BATS_TEST_TMPDIR/repo5" "" 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	# GH-7 parses as github (parse_explicit_issue_id: [Gg][Hh]-<digits>), so
	# the fake github provider is invoked with the explicit id ("7") as arg3,
	# never linear, and branch-derived resolution ("main" — no issue) is
	# never attempted.
	grep -q '^github .* 7$' "$STATE/providerlog"
	run ! grep -q '^linear ' "$STATE/providerlog"
	[ "$(cat "$STATE/opt_w5_@issue_id")" = "#7" ]
	[ "$(cat "$STATE/opt_w5_@issue_title")" = "Explicit GH Title" ]
	run ! grep -q 'unset w5 @issue_id' "$STATE/setlog"
}

@test "backfill: a window whose worktree no longer exists is skipped, stamp unchanged" {
	seed_window w6 SOME-ID "" "" some-branch "" "$BATS_TEST_TMPDIR/gone6" 0
	windowlist_line w6 SOME-ID "" "" some-branch "" "$BATS_TEST_TMPDIR/gone6" "" 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill-run
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/providerlog" ]
	[ "$(cat "$STATE/opt_w6_@issue_id")" = "SOME-ID" ]
}

@test "backfill tick gate: a fresh --backfill call detaches a run; a second call within the TTL is a no-op" {
	mkdir -p "$BATS_TEST_TMPDIR/repo7"
	seed_window w7 NOW-ENG "" "" now-eng-9-x "" "$BATS_TEST_TMPDIR/repo7" 0
	windowlist_line w7 NOW-ENG "" "" now-eng-9-x "" "$BATS_TEST_TMPDIR/repo7" "" 0 >"$STATE/windowlist"

	run bash "$STAMP" --backfill
	[ "$status" -eq 0 ]
	[ -f "$OG_ENRICH_CACHE_DIR/.last-backfill-tick" ]

	for _ in $(seq 1 40); do
		[ -f "$STATE/opt_w7_@issue_title" ] && break
		sleep 0.05
	done
	[ "$(cat "$STATE/opt_w7_@issue_title")" = "Backfilled Title" ]

	rm -f "$STATE/providerlog"
	run bash "$STAMP" --backfill
	[ "$status" -eq 0 ]
	sleep 0.2
	[ ! -f "$STATE/providerlog" ]
}

@test "branch change: a window that exhausted tries on the old branch gets a fresh attempt on a new branch" {
	mkdir -p "$BATS_TEST_TMPDIR/repo8"
	printf 'stuck-eng-old-x' >"$STATE/opt_w8_@issue_branch"
	printf '5' >"$STATE/opt_w8_@issue_backfill_tries"

	run bash "$STAMP" w8 "$BATS_TEST_TMPDIR/repo8" stuck-eng-new-x
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_w8_@issue_backfill_tries")" = "1" ]
}

@test "same branch still failing: tries increments rather than resets" {
	mkdir -p "$BATS_TEST_TMPDIR/repo9"
	printf 'stuck-eng-same-x' >"$STATE/opt_w9_@issue_branch"
	printf '2' >"$STATE/opt_w9_@issue_backfill_tries"

	run bash "$STAMP" w9 "$BATS_TEST_TMPDIR/repo9" stuck-eng-same-x
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_w9_@issue_backfill_tries")" = "3" ]
}

@test "stale branch argument: a real worktree's live branch wins over a stale argument" {
	# Needs a REAL git repo — every other test's "worktree" is a plain mkdir,
	# which the live-branch read silently no-ops against (git fails, so the
	# passed argument wins), so this is the one case that exercises the
	# override.
	repo="$BATS_TEST_TMPDIR/repo11"
	mkdir -p "$repo"
	git -C "$repo" init -q
	git -C "$repo" config user.email test@example.com
	git -C "$repo" config user.name test
	git -C "$repo" config commit.gpgsign false
	git -C "$repo" commit -q --allow-empty -m init
	git -C "$repo" checkout -q -b now-eng-live-x

	# Pass a DIFFERENT (stale) branch as the argument — as if the sweep's
	# scan observed the window before this switch happened.
	run bash "$STAMP" w11 "$repo" now-eng-stale-x
	[ "$status" -eq 0 ]
	# The live branch (now-eng-live-x) must win, not the stale argument.
	[ "$(cat "$STATE/opt_w11_@branch")" = "now-eng-live-x" ]
	[ "$(cat "$STATE/opt_w11_@issue_branch")" = "now-eng-live-x" ]
	grep -q 'linear .*now-eng-live-x' "$STATE/providerlog"
}

@test "same branch, new explicit id: tries resets rather than inheriting the old id's exhausted count" {
	mkdir -p "$BATS_TEST_TMPDIR/repo10"
	printf 'main' >"$STATE/opt_w10_@issue_branch"
	printf 'GH-1' >"$STATE/opt_w10_@issue_explicit_id"
	printf '5' >"$STATE/opt_w10_@issue_backfill_tries"

	# GH-999 is the fake's "id resolves, title/url still stuck" case — a
	# genuinely partial result, so this exercises the tries-reset branch
	# itself rather than the complete-stamp branch (which would clear the
	# counter regardless of whether old/new ids matched).
	run bash "$STAMP" w10 "$BATS_TEST_TMPDIR/repo10" main GH-999
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_w10_@issue_backfill_tries")" = "1" ]
}

@test "backfill tick gate: each tmux server keeps its own stamp (#705)" {
	TMUX=/tmp/sockA,1,0 run bash "$STAMP" --backfill
	[ "$status" -eq 0 ]
	TMUX=/tmp/sockB,1,0 run bash "$STAMP" --backfill
	[ "$status" -eq 0 ]
	[ -f "$OG_ENRICH_CACHE_DIR/.last-backfill-tick.srv_tmp_sockA" ]
	[ -f "$OG_ENRICH_CACHE_DIR/.last-backfill-tick.srv_tmp_sockB" ]
}
