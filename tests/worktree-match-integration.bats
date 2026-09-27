#!/usr/bin/env bats
# Real tmux server. Pins the two assumptions the fake-tmux suite cannot: window
# options (@worktree, @bridge_win) are readable from a `list-panes -a` format and
# are reported on EVERY pane row of the window, and $'\t' in a format arrives as
# a real tab. Both failures would be silent — the matcher would degrade to
# path-only (R1) or stop excluding bridge windows (R9).
#
# TMUX_TMPDIR is a short, per-run /tmp dir: `tmux -L` resolves to
# "$TMUX_TMPDIR/tmux-<uid>/<name>" and a long bats tmpdir path blows the unix
# socket 108-char limit. -f /dev/null keeps the user's config out of it. The
# socket name is per-run too, so this never adopts (or races the teardown of) a
# server left by another run — `kill-server` is asynchronous, and reusing a
# fixed name made this test flaky in the nix sandbox.

setup() {
	command -v tmux >/dev/null || skip "tmux not on PATH"
	OG_TMUX_DIR="/tmp/og-wtm-$$-${BATS_TEST_NUMBER}"
	export TMUX_TMPDIR="$OG_TMUX_DIR"
	rm -rf "$OG_TMUX_DIR"
	mkdir -p "$TMUX_TMPDIR"
	unset TMUX
	SOCK="wtm-$$-${BATS_TEST_NUMBER}"
	TM=(tmux -L "$SOCK" -f /dev/null)

	# Assert the fixture built, so a half-set-up server surfaces here instead of
	# as an unreadable parse failure downstream.
	"${TM[@]}" new-session -d -s s1 -x 80 -y 24
	"${TM[@]}" split-window -t s1:
	"${TM[@]}" set-option -t s1: -w @worktree /tmp/wt-a
	"${TM[@]}" set-option -t s1: -w @bridge_win 1
}

teardown() {
	[ -n "${SOCK:-}" ] && tmux -L "$SOCK" kill-server 2>/dev/null
	rm -rf "${OG_TMUX_DIR:-}"
	return 0
}

FMT='#{session_name}|#{window_index}|#{window_id}|#{@worktree}|#{@bridge_win}|#{pane_active}|#{pane_current_path}'

# Shared assertion: $1 is the raw `list-panes -a` output.
assert_rows() {
	# Dump the raw bytes on failure so a future flake is diagnosable.
	printf '%s\n' "$1" | cat -A >&2
	printf '%s\n' "$1" | awk -F'|' '
		NF != 7 { print "row " NR " has " NF " fields, want 7"; bad = 1; next }
		$3 == "" { print "row " NR ": empty window_id"; bad = 1 }
		$4 != "/tmp/wt-a" { print "row " NR ": @worktree=[" $4 "], want /tmp/wt-a"; bad = 1 }
		$5 != "1" { print "row " NR ": @bridge_win=[" $5 "], want 1"; bad = 1 }
		$6 !~ /^[01]$/ { print "row " NR ": pane_active=[" $6 "]"; bad = 1 }
		END {
			if (NR != 2) { print "got " NR " rows, want 2 (one per pane)"; bad = 1 }
			exit bad
		}
	'
}

@test "window options are reported on every pane row of list-panes -a" {
	run "${TM[@]}" list-panes -a -F "$FMT"
	[ "$status" -eq 0 ]
	assert_rows "$output"
}

@test "the field delimiter survives a non-UTF-8 locale" {
	# tmux rewrites non-printable bytes in command output to "_" when the locale
	# is not UTF-8, so a tab-delimited format silently collapses to one field
	# under LC_ALL=C and the matcher would match nothing at all — every
	# `wt switch` would spawn a new window. "|" is printable and survives.
	run env LC_ALL=C LANG=C "${TM[@]}" list-panes -a -F "$FMT"
	[ "$status" -eq 0 ]
	assert_rows "$output"

	# Pin the underlying tmux behavior this guards against, so the reason the
	# delimiter is "|" stays visible if anyone is tempted to "tidy" it to a tab:
	# under this locale a tab does NOT come back as a tab.
	run env LC_ALL=C LANG=C "${TM[@]}" list-panes -a -F $'a\tb'
	[ "$status" -eq 0 ]
	[[ $output != *$'\t'* ]]
}
