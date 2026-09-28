#!/usr/bin/env bats

load helper

setup() {
	export CLAUDE_STATUS_DIR="$BATS_TEST_TMPDIR/claude-status"
	unset TMUX TMUX_PANE
	export PROGRESS_LOG="$BATS_TEST_TMPDIR/progress.log"
	: >"$PROGRESS_LOG"

	local stub="$BATS_TEST_TMPDIR/lib-claude.sh"
	# Source the real lib first -- claude_status_dir_ensure et al. live there,
	# and claude-status-update.sh now calls it before writing any state -- then
	# override just the progress emitter.
	cat >"$stub" <<EOF
source "$PWD/scripts/lib-claude.sh"
claude_progress_emit() {
	printf '%s %s\n' "\$1" "\$2" >>"\$PROGRESS_LOG"
}
EOF
	CSU="$BATS_TEST_TMPDIR/claude-status-update.sh"
	sed -e "s|@lib_claude@|$stub|g" scripts/claude-status-update.sh >"$CSU"
}

@test "csu: processing emits processing" {
	bash "$CSU" processing --pane %7
	grep -qx '%7 processing' "$PROGRESS_LOG"
}

@test "csu: done emits clear" {
	bash "$CSU" "done" --pane %7
	grep -qx '%7 clear' "$PROGRESS_LOG"
}

@test "csu: clear verb emits clear" {
	bash "$CSU" processing --pane %7
	: >"$PROGRESS_LOG"
	bash "$CSU" clear --pane %7
	grep -qx '%7 clear' "$PROGRESS_LOG"
	[ ! -e "$CLAUDE_STATUS_DIR/panes/7" ]
}
