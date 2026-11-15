#!/usr/bin/env bats
# Tests tmux-reconcile-window against a real git repo, with tmux + the issue
# stamp faked so we can assert which options get set and whether the stamp fires.

setup() {
	export HOME="$BATS_TEST_TMPDIR" # keep git off any real user config
	STATE="$BATS_TEST_TMPDIR/state"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$STATE" "$FAKEBIN"
	export FAKE_TMUX_STATE="$STATE"

	# Fake tmux: answers display-message/show-options from env+state, records
	# every set-option (both -w OPT VAL and the -wu OPT unset form) to
	# $STATE/setlog and mirrors into $STATE/opt_*. Arg shapes match reconcile's
	# exact calls:
	#   display-message -t T -p FMT      (we only need pane_current_path -> FAKE_CWD)
	#   show-options    -t T -wqv OPT    -> OPT is $5
	#   set-option      -t T -w OPT VAL  -> OPT is $5, VAL is $6
	#   set-option      -t T -wu OPT     -> OPT is $5 (unset)
	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		st="$FAKE_TMUX_STATE"
		case "$1" in
		display-message) printf '%s' "${FAKE_CWD:-}" ;;
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

	# Fake issue stamp: records its argv so we can assert it ran (and with what).
	cat >"$FAKEBIN/issue-stamp" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$FAKE_TMUX_STATE/stamplog"
	EOF
	chmod +x "$FAKEBIN/issue-stamp"

	# Fake reflow: records its argv so we can assert whether/how it fired.
	cat >"$FAKEBIN/reflow" <<-'EOF'
		#!/bin/sh
		echo "$*" >>"$FAKE_TMUX_STATE/reflowlog"
	EOF
	chmod +x "$FAKEBIN/reflow"

	# Build a runnable reconcile with the @issue_stamp@/@reflow@ placeholders resolved.
	RECONCILE="$BATS_TEST_TMPDIR/reconcile.sh"
	sed \
		-e "s|@issue_stamp@|$FAKEBIN/issue-stamp|" \
		-e "s|@reflow@|$FAKEBIN/reflow|" \
		scripts/tmux-reconcile-window.sh >"$RECONCILE"

	# A real git worktree to derive from, on a known branch.
	REPO="$BATS_TEST_TMPDIR/repo"
	mkdir -p "$REPO"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email t@t
	git -C "$REPO" config user.name t
	git -C "$REPO" config commit.gpgsign false
	git -C "$REPO" commit -q --allow-empty -m init
	git -C "$REPO" checkout -q -b feat/95-test
	TOP="$(git -C "$REPO" rev-parse --show-toplevel)"

	PLAIN="$BATS_TEST_TMPDIR/plain" # not a git repo
	mkdir -p "$PLAIN"

	export PATH="$FAKEBIN:$PATH"
}

# Poll for a file the disowned background stamp writes.
wait_for() {
	for _ in $(seq 1 40); do
		[[ -f $1 ]] && return 0
		sleep 0.05
	done
	return 1
}

@test "cwd mode: tags a worktree window and kicks the stamp" {
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@worktree")" = "$TOP" ]
	[ "$(cat "$STATE/opt_@git_root")" = "$TOP" ]
	[ "$(cat "$STATE/opt_@branch")" = "feat/95-test" ]
	wait_for "$STATE/stamplog"
	[ "$(cat "$STATE/stamplog")" = "@1 $TOP feat/95-test" ]
}

@test "non-worktree cwd: clean no-op, no tags, no stamp" {
	FAKE_CWD="$PLAIN" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/setlog" ]
	[ ! -f "$STATE/stamplog" ]
}

@test "idempotent: matching @worktree/@branch skips writes and stamp" {
	printf '%s' "$TOP" >"$STATE/opt_@worktree"
	printf '%s' "feat/95-test" >"$STATE/opt_@branch"
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/setlog" ]
	[ ! -f "$STATE/stamplog" ]
}

@test "explicit mode: tags from args, ignores cwd" {
	# cwd points at a non-git dir to prove it isn't consulted in explicit mode.
	FAKE_CWD="$PLAIN" run bash "$RECONCILE" @1 "/some/worktree" "feat/95-explicit"
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@worktree")" = "/some/worktree" ]
	[ "$(cat "$STATE/opt_@git_root")" = "/some/worktree" ]
	[ "$(cat "$STATE/opt_@branch")" = "feat/95-explicit" ]
	wait_for "$STATE/stamplog"
	[ "$(cat "$STATE/stamplog")" = "@1 /some/worktree feat/95-explicit" ]
}

@test "re-tag in cwd mode with --cwd-move fires the reflow fake exactly once with --force" {
	printf '%s' "$TOP" >"$STATE/opt_@worktree"
	printf '%s' "old-branch" >"$STATE/opt_@branch"
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1 --cwd-move
	[ "$status" -eq 0 ]
	wait_for "$STATE/reflowlog"
	[ "$(wc -l <"$STATE/reflowlog")" -eq 1 ]
	grep -q -- '--force' "$STATE/reflowlog"
}

@test "creation seed tags from empty but does not fire the reflow" {
	# No prior @worktree — the plain creation-hook call shape — proves the
	# loosened gate didn't bring back the duplicate reflow on window creation.
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@worktree")" = "$TOP" ]
	[ ! -f "$STATE/reflowlog" ]
}

@test "a window untagged since creation, reconciled via --cwd-move once it sits in a repo, fires the reflow fake exactly once" {
	# No prior @worktree at all (the #605 case: window was created in a non-git
	# cwd, so the creation seed exited early and never tagged it).
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1 --cwd-move
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@worktree")" = "$TOP" ]
	wait_for "$STATE/reflowlog"
	[ "$(wc -l <"$STATE/reflowlog")" -eq 1 ]
	grep -q -- '--force' "$STATE/reflowlog"
}

@test "idempotent early exit does not fire the reflow" {
	printf '%s' "$TOP" >"$STATE/opt_@worktree"
	printf '%s' "feat/95-test" >"$STATE/opt_@branch"
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/reflowlog" ]
}

@test "explicit mode does not fire the reflow even on a re-tag" {
	printf '%s' "/old/worktree" >"$STATE/opt_@worktree"
	printf '%s' "old-branch" >"$STATE/opt_@branch"
	FAKE_CWD="$PLAIN" run bash "$RECONCILE" @1 "/some/worktree" "feat/95-explicit"
	[ "$status" -eq 0 ]
	[ ! -f "$STATE/reflowlog" ]
}

@test "re-tag onto a detached HEAD unsets @branch" {
	git -C "$REPO" checkout -q --detach
	printf '%s' "$TOP" >"$STATE/opt_@worktree"
	printf '%s' "old-branch" >"$STATE/opt_@branch"
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	grep -q 'unset @branch' "$STATE/setlog"
	[ ! -f "$STATE/opt_@branch" ]
}

@test "re-tag that changes @worktree unsets all twelve @pr_* options" {
	printf '%s' "/old/worktree" >"$STATE/opt_@worktree"
	printf '%s' "old-branch" >"$STATE/opt_@branch"
	for opt in @pr_number @pr_title @pr_state @pr_check_state @pr_url @pr_mergeable @pr_draft @pr_branch @pr_review @pr_auto_merge @pr_check_progress @pr_glyph; do
		printf 'x' >"$STATE/opt_$opt"
	done
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	for opt in @pr_number @pr_title @pr_state @pr_check_state @pr_url @pr_mergeable @pr_draft @pr_branch @pr_review @pr_auto_merge @pr_check_progress @pr_glyph; do
		[ ! -f "$STATE/opt_$opt" ]
	done
}

@test "re-tag that does not change @worktree leaves @pr_* alone" {
	printf '%s' "$TOP" >"$STATE/opt_@worktree"
	printf '%s' "old-branch" >"$STATE/opt_@branch"
	printf 'keep' >"$STATE/opt_@pr_number"
	FAKE_CWD="$REPO" run bash "$RECONCILE" @1
	[ "$status" -eq 0 ]
	[ "$(cat "$STATE/opt_@pr_number")" = "keep" ]
}
