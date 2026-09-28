#!/usr/bin/env bats

load helper

CSU="scripts/claude-status-update.sh"

setup() {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	# shellcheck disable=SC2174  # only the root itself must be owner-only
	mkdir -p -m 700 "$CLAUDE_STATUS_DIR"
	mkdir -p "$CLAUDE_STATUS_DIR/panes"
	FAKEBIN="$BATS_TEST_TMPDIR/bin"
	mkdir -p "$FAKEBIN"
	unset TMUX TMUX_PANE

	cat >"$FAKEBIN/tmux" <<-'EOF'
		#!/bin/sh
		case "$*" in
		*list-panes\ -t\ s1:0*)
			printf '%s\n' '%10' '%11'
			;;
		*list-panes\ -t\ s1:1*)
			printf '%s\n' '%20'
			;;
		esac
		exit 0
	EOF
	chmod +x "$FAKEBIN/tmux"
	export PATH="$FAKEBIN:$PATH"
}

write_pane() {
	local id="$1"
	cat >"$CLAUDE_STATUS_DIR/panes/$id" <<EOF
state=processing
timestamp=1
session=s1
unseen=1
EOF
}

pane_has_unseen() {
	grep -q '^unseen=1$' "$CLAUDE_STATUS_DIR/panes/$1"
}

@test "mark-seen: clears unseen on panes in target window" {
	write_pane 10
	write_pane 11
	run bash "$CSU" mark-seen --session s1 --window 0
	[ "$status" -eq 0 ]
	[ ! -e "$CLAUDE_STATUS_DIR/panes/10" ] || ! pane_has_unseen 10
	[ ! -e "$CLAUDE_STATUS_DIR/panes/11" ] || ! pane_has_unseen 11
}

@test "mark-seen: pane outside target window keeps unseen" {
	write_pane 10
	write_pane 11
	write_pane 20
	run bash "$CSU" mark-seen --session s1 --window 0
	[ "$status" -eq 0 ]
	[ ! -e "$CLAUDE_STATUS_DIR/panes/10" ] || ! pane_has_unseen 10
	[ ! -e "$CLAUDE_STATUS_DIR/panes/11" ] || ! pane_has_unseen 11
	pane_has_unseen 20
}

@test "mark-seen: unknown flag exits non-zero with error" {
	run bash "$CSU" mark-seen --session s1 --window 0 --bogus x
	[ "$status" -eq 1 ]
	[[ $output == *"Unknown option '--bogus'"* ]]
}
